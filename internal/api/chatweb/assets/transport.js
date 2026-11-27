// Корень управляющих маршрутов на том же сервере, который отдал страницу.
export const apiRoot = "/api/v1";

// Минимальная пауза перед новым запросом снимка или открытием нового EventSource, в миллисекундах.
const retryDelay = 1000;

// Имена кадров, которые сервер отправляет отдельными именованными SSE-событиями.
const eventKinds = ["chat.message", "chat.source_status", "chat.gap", "chat.cleared", "resync_required", "chat.session_closed"];

// Проверяет поддержку чтения исходной числовой лексемы и её записи без кавычек и округления.
export function supportsExactJSON() {
  return typeof JSON.rawJSON === "function" && JSON.parse("0", (_key, _value, context) => {
    // Проверяет третье значение reviver на исходной лексеме простого JSON-числа.
    return context?.source;
  }) === "0";
}

// Сохраняет целочисленные JSON-токены сервера в BigInt. Строки, в том числе идентификаторы и
// курсоры, остаются неизменными. Сервер выдаёт целые в десятичной записи без экспоненты.
export function parseExactJSON(text) {
  return JSON.parse(text, (_key, value, context) => {
    // Для числового значения читает исходную лексему до потери точности в Number.
    if (typeof value !== "number") return value;
    if (context?.source === undefined) throw new Error("Браузер не поддерживает точные числа JSON.");
    return /^-?\d+$/.test(context.source) ? BigInt(context.source) : value;
  });
}

// Передаёт целое BigInt как десятичный токен JSON для маршрутов, требующих число.
export function stringifyExactJSON(value) {
  return JSON.stringify(value, (_key, field) => {
    // Записывает BigInt без промежуточного Number. Остальные типы кодирует штатный JSON.
    return typeof field === "bigint" ? JSON.rawJSON(field.toString()) : field;
  });
}

// ChatHTTPError хранит только статус HTTP и машинный код для перевода в текст интерфейса.
export class ChatHTTPError extends Error {
  // Статус ответа сервера, например 409 при несовпадении ревизии.
  status;
  // Код из Problem Details, если сервер предоставил его.
  code;

  // Сохраняет статус и код. Подробности ответа сервера в сообщение исключения не попадают.
  constructor(status, code) {
    super("Ошибка запроса к серверу чатов.");
    this.status = status;
    this.code = code;
  }
}

// Делает один запрос на заданный локальный путь. Возвращает статус, декодированное тело и Location,
// а для 204 возвращает null вместо тела. Метод POST не следует за перенаправлениями.
export async function requestJSON(path, { method = "GET", body, signal } = {}) {
  // response содержит ответ одного запроса. Реквизиты передаются только через body.
  const response = await fetch(path, {
    method, signal, cache: "no-store", redirect: "error",
    headers: body === undefined ? undefined : { "Content-Type": "application/json" },
    body: body === undefined ? undefined : stringifyExactJSON(body),
  });
  // text содержит фактическое тело ответа, которое нужно разобрать до преобразования чисел.
  const text = response.status === 204 ? "" : await response.text();
  if (!response.ok) {
    // code получает только машинный идентификатор ошибки при наличии JSON-тела.
    let code = "";
    try {
      code = parseExactJSON(text).code;
    } catch {
      // Сервер или промежуточный узел может вернуть ошибку без тела JSON.
    }
    throw new ChatHTTPError(response.status, code);
  }
  return { data: text ? parseExactJSON(text) : null, location: response.headers.get("Location"), status: response.status };
}

// Читает все страницы локального списка. map сохраняет только поля, которые нужны интерфейсу.
// Прекращает чтение при отмене запроса или при зацикленном серверном курсоре.
export async function listItems(path, signal, map) {
  // items накапливает результаты, seen хранит уже прочитанные позиции страниц.
  const items = [], seen = new Set();
  // cursor остаётся строкой из ответа до построения следующего запроса.
  let cursor = "";
  do {
    // query добавляет необязательную позицию к первому или следующему запросу списка.
    const query = cursor ? `?${new URLSearchParams({ cursor })}` : "";
    // data содержит очередную страницу публичной конфигурации.
    const { data } = await requestJSON(path + query, { signal });
    items.push(...(map ? data.items.map(map) : data.items));
    cursor = data.next_cursor;
    if (cursor && seen.has(cursor)) throw new Error("Сервер повторил позицию страницы списка.");
    seen.add(cursor);
  } while (cursor);
  return items;
}

// ChatStream ведёт одну цепочку snapshot и SSE. Каждый callback проверяет, что его запрос или
// EventSource всё ещё обслуживает ленту. Переподключение начинает после последнего принятого кадра.
export class ChatStream {
  // Путь к согласованному снимку и событиям одного потока без query-параметров.
  #path;
  // Обработчики снимка, события, завершения Session и состояния соединения в представлении страницы.
  #view;
  // Непрозрачная позиция последнего применённого снимка или события.
  #cursor = "";
  // Контроллер единственного незавершённого чтения согласованного снимка.
  #request = null;
  // Текущий EventSource. Закрытый или заменённый экземпляр не меняет состояние ленты.
  #source = null;
  // Таймер следующего чтения снимка либо повторного открытия EventSource.
  #timer;
  // Признак завершения работы при уходе со страницы.
  #stopped = false;

  // Сохраняет адрес потока и обработчики представления. Для начала чтения вызывает start.
  constructor(streamID, view) {
    this.#path = `${apiRoot}/chat/streams/${encodeURIComponent(streamID)}`;
    this.#view = view;
  }

  // Читает первый согласованный снимок для нового экземпляра ленты.
  start() {
    if (!this.#stopped && !this.#request && !this.#source) void this.#snapshot();
  }

  // Отменяет активный HTTP-запрос, EventSource и таймер. После вызова экземпляр больше не открывается.
  stop() {
    this.#stopped = true;
    clearTimeout(this.#timer);
    this.#request?.abort();
    this.#request = null;
    this.#disconnect();
  }

  // Заменяет ожидающее действие и сообщает представлению о восстановлении до следующего запроса.
  #retry(action) {
    if (this.#stopped) return;
    clearTimeout(this.#timer);
    this.#view.connection?.("retrying");
    this.#timer = setTimeout(action, retryDelay);
  }

  // Читает новый снимок, атомарно применяет его в представлении и открывает SSE с его курсора.
  // При 404 лента остаётся закрытой до обновления страницы. Другие ошибки запускают новый запрос.
  async #snapshot() {
    if (this.#stopped) return;
    // request различает этот запрос и любой следующий запрос после отмены.
    const request = new AbortController();
    this.#request = request;
    try {
      // data содержит согласованный серверный снимок вместе с позицией начала SSE.
      const { data } = await requestJSON(`${this.#path}/snapshot`, { signal: request.signal });
      if (this.#request !== request) return;
      if (typeof data.cursor !== "string" || !data.cursor) throw new Error("Снимок не содержит курсора.");
      this.#view.snapshot(data);
      this.#cursor = data.cursor;
      this.#open();
    } catch (error) {
      // error содержит отказ чтения, разбора или применения именно этого снимка.
      if (this.#request !== request) return;
      if (error instanceof ChatHTTPError && error.status === 404) {
        this.#view.connection?.("missing");
        return;
      }
      this.#retry(() => {
        // После паузы повторяет только чтение, с новым контроллером запроса.
        void this.#snapshot();
      });
    } finally {
      if (this.#request === request) this.#request = null;
    }
  }

  // Закрывает текущий EventSource до создания нового, отключая автоматический повтор браузера.
  #disconnect() {
    this.#source?.close();
    this.#source = null;
  }

  // Открывает новое соединение без встроенного Last-Event-ID. При сетевой ошибке повторяет
  // открытие после паузы по текущему применённому курсору, не перечитывая снимок.
  #open() {
    if (this.#stopped) return;
    // source автоматически закрывается при ошибке и остаётся ссылкой для проверки обработчиков.
    const source = new EventSource(`${this.#path}/events?${new URLSearchParams({ cursor: this.#cursor })}`);
    this.#source = source;
    source.onopen = () => {
      // Запоздалое открытие заменённого источника не меняет отображаемый статус.
      if (this.#source === source) this.#view.connection?.("open");
    };
    source.onerror = () => {
      // Закрывает этот источник прежде, чем запланировать ручное переподключение.
      if (this.#source !== source) return;
      this.#disconnect();
      this.#retry(() => {
        // Открывает новый EventSource с последним принятым курсором.
        this.#open();
      });
    };
    // kind указывает имя очередного варианта события, зарегистрированного на этом экземпляре.
    for (const kind of eventKinds) {
      source.addEventListener(kind, (event) => {
        // Передаёт кадр вместе с экземпляром, на котором он получен.
        this.#receive(source, kind, event);
      });
    }
  }

  // Проверяет согласованность имени, JSON и id SSE. Требование нового снимка обрывает старую
  // подписку. Для обычного события cursor меняется только после применения в представлении.
  #receive(source, kind, event) {
    if (this.#source !== source) return;
    try {
      // payload содержит данные текущего кадра. Его непрозрачная позиция сверяется с id SSE.
      const payload = parseExactJSON(event.data);
      if (payload.kind !== kind || !event.lastEventId || payload.cursor !== event.lastEventId) {
        throw new Error("Несогласованный кадр чата.");
      }
      if (kind === "resync_required" || kind === "chat.session_closed") {
        this.#disconnect();
        this.#view.reset?.(kind, payload);
        this.#retry(() => {
          // Читает новую согласованную историю после смены эпохи или потери replay.
          void this.#snapshot();
        });
      } else if (event.lastEventId !== this.#cursor) {
        this.#view.event(payload);
        this.#cursor = event.lastEventId;
      }
    } catch {
      this.#disconnect();
      this.#retry(() => {
        // После ошибки кадра получает корректную границу через новый снимок.
        void this.#snapshot();
      });
    }
  }
}
