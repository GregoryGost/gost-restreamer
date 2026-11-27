import { byID, errorMessage, notice, returnFocusOnClose } from "./controls.js";
import { httpsURL, providerLabel } from "./feed.js";
import { apiRoot, ChatHTTPError, requestJSON } from "./transport.js";

// Путь используется для общего запуска подключения и проверки Location из ответа 202.
const accountPath = `${apiRoot}/chat/accounts`;

// Одни и те же поля формы показываются и отправляются для соответствующего провайдера.
const credentials = Object.freeze({ youtube: ["api_key"], twitch: ["client_id"], vk_video_live: ["client_id", "client_secret"] });

// Подписи переводят проверенные причины сервера в действие для владельца аккаунта.
const failureMessages = Object.freeze({
  credentials_required: "Площадка отклонила реквизиты. Проверьте их и повторите подключение.",
  network: "Не удалось связаться с площадкой. Повторите подключение после восстановления связи.",
  quota: "Площадка ограничила запросы. Повторите подключение позже.",
  limit: "Достигнут предел запросов сервера. Повторите подключение позже.",
  configuration: "Не удалось завершить подключение. Проверьте настройки и повторите попытку.",
});

// retryableFailures перечисляет подтверждённые отказы, после которых можно повторить ввод.
// Результат с неизвестным исходом сохранения обрабатывается до проверки этого списка.
const retryableFailures = new Set(["credentials_required", "network", "quota", "limit", "configuration"]);

// pollDelay отделяет чтения состояния. Интервалы запросов к площадке определяет сервер.
const pollDelay = 2000;

// Удерживает одну цепочку подключения в памяти страницы. Только отправка формы запускает POST.
// Для каждого известного Location использует чтение и отмену одной и той же серверной попытки.
export class AuthorizationView {
  // Диалог общей формы подключения и состояния текущей операции.
  #dialog = byID("authorization-dialog");
  // Форма принимает реквизиты и очищает их поля после построения тела запроса.
  #form = byID("authorization-form");
  // Единственный элемент состояния и безопасных ошибок подключения.
  #status = byID("authorization-status");
  // Признак незавершённого запроса запуска или отмены в текущем диалоге.
  #busy = false;
  // Публичная запись аккаунта при замене. Для создания значение пустое.
  #target = null;
  // Объект известной попытки остаётся тем же для GET и DELETE по текущему Location.
  #attempt = null;
  // Таймер чтения состояния одной попытки, очищаемый при отмене и завершении.
  #timer;
  // Функция обновления списка аккаунтов после подтверждённого сохранения.
  #onSaved;

  // Связывает поля, запуск, закрытие формы и отмену попытки. Во время обработки текущей попытки
  // закрытие блокируется, чтобы код Twitch и кнопка отмены оставались доступны.
  constructor(onSaved) {
    this.#onSaved = onSaved;
    this.#form.elements.provider.addEventListener("change", () => {
      // При смене провайдера очищает скрытые реквизиты и показывает нужные поля формы.
      this.#showFields();
    });
    this.#form.addEventListener("submit", (event) => {
      // Отправляет один POST из формы после нативной проверки полей.
      event.preventDefault();
      void this.#start();
    });
    byID("close-authorization").addEventListener("click", () => {
      // Закрывает форму, когда попытка завершилась или ещё не запускалась.
      this.#dialog.close();
    });
    byID("cancel-authorization").addEventListener("click", () => {
      // Отменяет только текущую известную попытку по сохранённому Location.
      void this.#cancel();
    });
    this.#dialog.addEventListener("cancel", (event) => {
      // Не позволяет Escape скрыть интерфейс незавершённого подключения.
      if (this.#busy || this.#attempt) event.preventDefault();
    });
    this.#dialog.addEventListener("close", () => {
      // Убирает реквизиты, если закрывается незаполненная либо завершённая форма.
      if (!this.#dialog.open) this.#form.reset();
    });
  }

  // Открывает создание или замену выбранного аккаунта и запоминает trigger для возврата фокуса.
  // fallback используется, если запись, из которой открыли диалог, исчезла во время подключения.
  open(account, trigger, fallback) {
    if (this.#dialog.open) return;
    this.#target = account;
    this.#form.reset();
    this.#form.hidden = false;
    byID("authorization-title").textContent = account ? "Повторное подключение" : "Подключение аккаунта";
    if (account) this.#form.elements.provider.value = account.provider;
    this.#form.elements.provider.disabled = Boolean(account);
    this.#form.elements.name.closest("label").hidden = Boolean(account);
    this.#showFields();
    this.#showDevice(null);
    notice(this.#status, "");
    returnFocusOnClose(this.#dialog, trigger, fallback);
    this.#dialog.showModal();
  }

  // Показывает ввод реквизитов текущей площадки и очищает остальные поля.
  #showFields() {
    // active содержит допустимые поля формы после выбора платформы.
    const active = credentials[this.#form.elements.provider.value];
    // name обозначает реквизит из фиксированного набора, field находится в той же форме.
    for (const name of ["api_key", "client_id", "client_secret"]) {
      const field = this.#form.elements[name];
      field.required = active.includes(name);
      field.closest("label").hidden = !field.required;
      if (!field.required) field.value = "";
    }
  }

  // Показывает публичный код и проверенную HTTPS-ссылку Twitch до окончания текущего ожидания.
  #showDevice(data) {
    // href содержит только проверенную ссылку подтверждения Twitch, link — элемент её отображения.
    const href = data?.provider === "twitch" && data.user_code ? httpsURL(data.verification_uri) : null;
    const link = byID("device-link");
    byID("device").hidden = !href;
    if (!href) {
      link.removeAttribute("href");
      byID("device-code").textContent = "";
      byID("device-expiry").textContent = "";
      return;
    }
    link.href = href;
    byID("device-code").textContent = data.user_code;
    byID("device-expiry").textContent = `Действует до ${new Date(data.expires_at).toLocaleTimeString("ru-RU")}`;
  }

  // Запускает одну попытку и убирает форму со страницы после считывания тела. Если 202 потерян
  // или не удалось проверить Location, показывает неизвестный результат без повторного POST.
  async #start() {
    if (this.#busy || this.#attempt || this.#form.hidden) return;
    // provider определяет набор входных реквизитов. body принадлежит единственному POST.
    const provider = this.#form.elements.provider.value;
    const body = { provider };
    if (this.#target) {
      body.account_id = this.#target.id;
      body.expected_revision = this.#target.revision;
    } else {
      body.name = this.#form.elements.name.value;
    }
    // name указывает реквизит этого провайдера. Его значение передаётся без изменений.
    for (const name of credentials[provider]) {
      body[name] = this.#form.elements[name].value;
      this.#form.elements[name].value = "";
    }
    // close блокирует выход до ответа, active указывает, потребуется ли заменить скрываемый фокус.
    const close = byID("close-authorization");
    const active = document.activeElement;
    this.#busy = true;
    notice(this.#status, "Запускаем подключение…");
    if (this.#form.contains(active) || active === close) this.#status.focus({ preventScroll: true });
    this.#form.hidden = true;
    close.disabled = true;
    try {
      // status подтверждает принятие запроса; data содержит открытые поля, location адресует точный ID.
      const { data, location, status } = await requestJSON(`${accountPath}/connect`, { method: "POST", body });
      if (status !== 202 || typeof data?.attempt_id !== "string" || !data.attempt_id) throw new Error("Нет подтверждения запуска подключения.");
      // path строит единственный адрес, который клиент вправе использовать для этой попытки.
      const path = `${accountPath}/authorizations/${encodeURIComponent(data.attempt_id)}`;
      if (location !== path || data.provider !== provider) throw new Error("Некорректный адрес подключения.");
      this.#busy = false;
      this.#attempt = { path, id: data.attempt_id, provider, cancelable: false, expiresAt: Date.parse(data.expires_at) };
      this.#apply(this.#attempt, data);
    } catch (error) {
      // error различает проверку входных данных, другие отказы HTTP и неизвестный исход запуска.
      if (error instanceof ChatHTTPError && error.status < 500 && error.code !== "credential_commit_unknown") {
        this.#finish(errorMessage(error), error.status === 422);
      } else {
        this.#finish("Результат запуска неизвестен. Обновите настройки и проверьте аккаунты перед новой попыткой.");
      }
    }
  }

  // Применяет ответ только для действующей попытки. Для неопределённого COMMIT скрывает Device Code,
  // прекращает чтение статуса и отображает, что результат сохранения неизвестен.
  #apply(attempt, data) {
    if (this.#attempt !== attempt) return;
    if (data?.attempt_id !== attempt.id || data.provider !== attempt.provider) {
      this.#finish("Не удалось подтвердить состояние этого подключения.");
      return;
    }
    if (data.code === "credential_commit_unknown") {
      this.#finish("Результат сохранения неизвестен. Список аккаунтов может измениться, но не подтверждает исход этой попытки.");
      return;
    }
    if (data.state === "PENDING") {
      // active, link и cancel позволяют сохранить фокус при обновлении окна подтверждения.
      const active = document.activeElement;
      const link = byID("device-link");
      const cancel = byID("cancel-authorization");
      this.#showDevice(data);
      attempt.expiresAt = Date.parse(data.expires_at);
      attempt.cancelable = data.cancelable === true;
      cancel.hidden = !attempt.cancelable;
      cancel.disabled = false;
      notice(this.#status, data.user_code ? "Подтвердите подключение на Twitch." : `Ожидаем подтверждение от ${providerLabel(data.provider)}…`);
      if (active === this.#status || (active === link && !link.hasAttribute("href")) || (active === cancel && cancel.hidden)) {
        (link.hasAttribute("href") ? link : cancel.hidden ? this.#status : cancel).focus({ preventScroll: true });
      }
      this.#schedule(attempt);
      return;
    }
    switch (data.state) {
      case "SUCCEEDED":
        this.#finish("Аккаунт подключён.");
        this.#onSaved();
        break;
      case "FAILED":
        this.#finish(Object.hasOwn(failureMessages, data.code) ? failureMessages[data.code] : "Не удалось подключить аккаунт.",
          retryableFailures.has(data.code));
        break;
      case "CANCELLED": this.#finish("Подключение отменено."); break;
      case "EXPIRED": this.#finish("Время подключения истекло."); break;
      default: this.#finish("Сервер вернул неизвестное состояние подключения.");
    }
  }

  // Откладывает GET текущей попытки. Для каждого GET сохраняет запрос на самой попытке, чтобы
  // отмена или более новый запрос могли исключить запоздалый результат.
  #schedule(attempt) {
    clearTimeout(this.#timer);
    this.#timer = setTimeout(() => {
      // Запрашивает только публичное состояние уже известной операции.
      void this.#poll(attempt);
    }, pollDelay);
  }

  // Читает состояние попытки. При временном отказе сохраняет известный Location и читает ещё раз.
  // При 404 попытка больше не наблюдаема и её исход не подтверждается интерфейсом.
  async #poll(attempt) {
    if (this.#attempt !== attempt || this.#busy) return;
    // request различает этот GET и последующие действия с тем же Location.
    const request = new AbortController();
    attempt.request = request;
    try {
      // data содержит очередную публичную проекцию авторизации.
      const { data } = await requestJSON(attempt.path, { signal: request.signal });
      if (attempt.request === request) this.#apply(attempt, data);
    } catch (error) {
      // error отличает утраченную RAM-попытку от временной недоступности её чтения.
      if (this.#attempt !== attempt || attempt.request !== request) return;
      if (error instanceof ChatHTTPError && error.status === 404) {
        this.#finish("Состояние попытки больше недоступно. Исход подключения неизвестен.");
      } else if (Date.now() >= attempt.expiresAt) {
        this.#finish("Не удалось проверить состояние в срок. Исход подключения неизвестен.");
      } else {
        notice(this.#status, "Связь потеряна. Проверяем состояние подключения…");
        if (document.activeElement === byID("device-link")) this.#status.focus({ preventScroll: true });
        this.#showDevice(null);
        this.#schedule(attempt);
      }
    }
  }

  // Отменяет попытку по её Location. При конфликте или потере ответа читает текущее состояние,
  // поскольку сохранение могло уже начаться. Запрос запуска при этом никогда не повторяется.
  async #cancel() {
    // attempt владеет текущей операцией и контроллером чтения, которое требуется прервать.
    const attempt = this.#attempt;
    if (!attempt?.cancelable || this.#busy) return;
    this.#busy = true;
    clearTimeout(this.#timer);
    attempt.request?.abort();
    attempt.request = null;
    // cancel останется недоступна до подтверждения отмены либо нового чтения текущей попытки.
    const cancel = byID("cancel-authorization");
    notice(this.#status, "Отменяем подключение…");
    if (document.activeElement === cancel) this.#status.focus({ preventScroll: true });
    cancel.disabled = true;
    try {
      await requestJSON(attempt.path, { method: "DELETE" });
      if (this.#attempt === attempt) this.#finish("Подключение отменено.");
    } catch (error) {
      // error сообщает о специальном исходе COMMIT либо об отказе самого запроса отмены.
      if (this.#attempt !== attempt) return;
      if (error instanceof ChatHTTPError && error.code === "credential_commit_unknown") {
        this.#finish("Результат сохранения неизвестен. Сервер не может подтвердить отмену.");
        return;
      }
      this.#busy = false;
      notice(this.#status, "Проверяем результат отмены…");
      void this.#poll(attempt);
    }
  }

  // Завершает наблюдение и удаляет временные параметры Twitch. retry разрешает новый ручной ввод
  // при подтверждённом отказе. Площадка и имя остаются в форме, реквизиты уже очищены.
  #finish(text, retry = false) {
    // active, link и cancel определяют, исчезает ли или меняет назначение текущий фокус.
    const active = document.activeElement;
    const link = byID("device-link");
    const cancel = byID("cancel-authorization");
    // close закрывает завершённую попытку, а credential позволяет сразу исправить реквизиты.
    const close = byID("close-authorization");
    const credential = retry ? this.#form.elements[credentials[this.#form.elements.provider.value][0]] : null;
    clearTimeout(this.#timer);
    this.#attempt?.request?.abort();
    this.#attempt = null;
    this.#busy = false;
    this.#showDevice(null);
    cancel.hidden = true;
    close.disabled = false;
    this.#form.hidden = !retry;
    notice(this.#status, text);
    if (active === this.#status || active === link || active === cancel) {
      (credential ?? close).focus({ preventScroll: true });
    }
  }
}
