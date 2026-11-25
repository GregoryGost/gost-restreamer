// Отображаемые имена провайдеров используются лентой, настройками и статусами.
export const providers = Object.freeze({ youtube: "YouTube", twitch: "Twitch", vk_video_live: "VK Видео Live" });

// providerIcons задаёт пути встроенных SVG для youtube, twitch и vk_video_live.
const providerIcons = Object.freeze({
  youtube: "/chat/assets/provider-youtube.svg",
  twitch: "/chat/assets/provider-twitch.svg",
  vk_video_live: "/chat/assets/provider-vk-video-live.svg",
});

// Возвращает локализованную подпись закрытого каталога или общую подпись для нового значения.
export function providerLabel(provider) {
  return Object.hasOwn(providers, provider) ? providers[provider] : "Площадка";
}

// Возвращает встроенный значок площадки или текст при отсутствии значка.
// Для тестового provider значок выбирает testProvider, а подпись сообщает о тестовом происхождении.
function providerMark(provider, testProvider) {
  // platform определяет значок, а label подписывает площадку для наведения и экранного диктора.
  const platform = provider === "test" ? testProvider : provider;
  const label = providerLabel(platform);
  if (!Object.hasOwn(providerIcons, platform)) return element("span", "provider", provider === "test" ? "Тест" : label);
  // icon показывает локальное изображение рядом с именем автора.
  const icon = element("img", "provider-icon");
  icon.alt = provider === "test" ? `Тестовое сообщение · ${label}` : label;
  icon.title = icon.alt;
  icon.src = providerIcons[platform];
  return icon;
}

// Возвращает абсолютный HTTPS-адрес ссылки или изображения. URL с реквизитами не отображается.
export function httpsURL(value) {
  if (typeof value !== "string" || !value) return null;
  try {
    // url разбирает абсолютный адрес без базового URL страницы.
    const url = new URL(value);
    return url.protocol === "https:" && url.hostname && !url.username && !url.password ? url.href : null;
  } catch {
    return null;
  }
}

// Возвращает узел заданного типа и записывает текст без интерпретации разметки.
export function element(tag, className, value) {
  // node получает только доверенные tag и className из вызова модуля.
  const node = document.createElement(tag);
  if (className) node.className = className;
  if (value !== undefined) node.textContent = value;
  return node;
}

// Возвращает безопасное изображение с текстовым эквивалентом либо null при недопустимом адресе.
function publicImage(url, alt, className) {
  // src задаёт проверенный исходный HTTPS-адрес, image представляет изображение фиксированного размера.
  const src = httpsURL(url);
  if (!src) return null;
  const image = element("img", className);
  image.alt = alt ?? "";
  image.referrerPolicy = "no-referrer";
  image.src = src;
  return image;
}

// Оборачивает фрагмент чата в ссылку только для проверенного адреса. Ссылки открываются отдельно
// от ленты, а Overlay получает тот же визуальный фрагмент без интерактивной ссылки.
function publicLink(url, content, interactive) {
  // href содержит разрешённый адрес, link создаётся только на интерактивной странице.
  const href = interactive ? httpsURL(url) : null;
  if (!href) return content;
  const link = element("a", "message-link");
  link.href = href;
  link.target = "_blank";
  link.rel = "noopener noreferrer";
  link.append(content);
  return link;
}

// Собирает строку из message со значком площадки, автором и безопасными фрагментами текста.
// interactive включает ссылки в Dashboard. В Overlay те же фрагменты отображаются без ссылок.
// Цвет имени проходит проверку RGB. Эмодзи и бейджи с недопустимым адресом отображаются текстом.
function messageRow(message, interactive) {
  // row содержит сообщение, header группирует визуальные атрибуты автора, avatar остаётся необязательным.
  const row = element("li", "message");
  const header = element("div", "message-header");
  const avatar = publicImage(message.author.avatar_url, "", "avatar");
  if (avatar) header.append(avatar);
  header.append(providerMark(message.provider, message.test_provider));
  // badge обозначает очередную метку автора. content содержит её изображение либо читаемое имя.
  for (const badge of message.badges ?? []) {
    const content = publicImage(badge.image_url, badge.name, "badge") ?? element("span", "badge-label", badge.name);
    header.append(publicLink(badge.url, content, interactive));
  }
  // author получает видимое имя и допустимый цвет. body хранит фрагменты сообщения исходного порядка.
  const author = element("span", "author", message.author.display_name);
  if (/^#[0-9a-f]{6}$/i.test(message.author.color ?? "")) author.style.setProperty("--author-color", message.author.color);
  header.append(author);
  const body = element("p", "message-text");
  if (message.parts?.length) {
    // part описывает очередной исходный фрагмент. content содержит эмодзи либо его текстовый эквивалент.
    for (const part of message.parts) {
      const content = (part.kind === "emote" && publicImage(part.image_url, part.text, "emote")) || document.createTextNode(part.text);
      body.append(publicLink(part.url, content, interactive));
    }
  } else {
    body.textContent = message.text;
  }
  row.append(header, body);
  return row;
}

// Возвращает stream_id из URL, когда он указан ровно один раз. Значение остаётся строкой.
export function streamIDFromPage() {
  // values включает все параметры stream_id, чтобы одинаково обнаруживать пустой и спорный адрес.
  const values = new URLSearchParams(location.search).getAll("stream_id");
  return values.length === 1 ? values[0] : "";
}

// FeedView удерживает ограниченный хвост сообщений и соответствующие ему DOM-строки. В режиме
// просмотра истории восстанавливает первое доступное видимое сообщение с прежним смещением.
export class FeedView {
  // Список сообщений страницы с серверным пределом в data-message-limit.
  #list;
  // Контейнер прокрутки. Overlay использует визуальное выравнивание CSS без него.
  #scroll;
  // Серверный предел количества сообщений в удерживаемом окне.
  #limit;
  // Строки окна в порядке получения. Каждая связывает сообщение и его DOM-элемент.
  #entries = [];
  // Идентификатор источника для фильтра Dashboard. Пустое значение выбирает все источники.
  #filter = "";
  // Признак прокрутки в конец после обновления списка.
  #following = true;
  // Число подходящих сообщений, полученных после остановки автопрокрутки.
  #unread = 0;

  // Связывает список и необязательный контейнер. hardLimit ограничивает число сообщений даже
  // при неверном значении в разметке. При корректной настройке применяется меньший предел сервера.
  constructor(list, hardLimit, scroll = null) {
    // limit содержит числовое значение из атрибута, установленного сервером.
    const limit = Number(list.dataset.messageLimit);
    this.#list = list;
    this.#scroll = scroll;
    this.#limit = Number.isInteger(limit) && limit > 0 ? Math.min(limit, hardLimit) : hardLimit;
  }

  // Заменяет хвост согласованным снимком. При той же Session сохраняет строку и считает новые
  // подходящие сообщения по seq. При смене Session окно и счётчик начинают с новой истории.
  replace(messages, sameSession) {
    // anchor содержит прежние точки восстановления, lastSeq задаёт последний известный порядок.
    const anchor = sameSession ? this.#anchor() : [];
    const lastSeq = this.#entries.at(-1)?.message.seq;
    if (!sameSession) this.#unread = 0;
    // tail ограничивает вновь создаваемые DOM-строки до применения фильтра.
    const tail = messages.slice(-this.#limit);
    this.#entries = tail.map((message) => {
      // Создаёт строку из снимка и учитывает только видимые сообщения после старого хвоста.
      if (!this.#following && sameSession && lastSeq !== undefined && message.seq > lastSeq && this.#matches(message)) this.#unread++;
      return this.#entry(message);
    });
    this.#list.replaceChildren(...this.#entries.map(({ row }) => {
      // Возвращает подготовленную DOM-строку в порядке снимка.
      return row;
    }));
    this.#restore(anchor);
  }

  // Добавляет новое сообщение в конец и освобождает место в начале перед вставкой. По Dashboard
  // учитывает только подходящие активному фильтру сообщения и сохраняет прежнюю прокрутку.
  append(message) {
    // anchor содержит видимые до изменения строки, entry связывает новое сообщение с его DOM-строкой.
    const anchor = this.#anchor();
    const entry = this.#entry(message);
    if (this.#entries.length === this.#limit) this.#entries.shift().row.remove();
    this.#entries.push(entry);
    this.#list.append(entry.row);
    if (!this.#following && !entry.row.hidden) this.#unread++;
    this.#restore(anchor);
  }

  // Меняет видимый набор, сохраняя ограниченный общий хвост и текущий режим автопрокрутки.
  filter(sourceID) {
    // anchor хранит прежние видимые позиции для строк, доступных при новом фильтре.
    const anchor = this.#anchor();
    this.#filter = sourceID;
    this.#unread = 0;
    // entry связывает сообщение и строку, видимость которой зависит от текущего фильтра.
    for (const entry of this.#entries) entry.row.hidden = !this.#matches(entry.message);
    this.#restore(anchor);
  }

  // Включает или останавливает автоматический переход к концу. При возобновлении очищает счётчик.
  follow(enabled) {
    this.#following = enabled;
    if (enabled) {
      this.#unread = 0;
      this.#restore([]);
    }
  }

  // Сообщает, осталась ли прокрутка у нижней границы ленты с учётом округления координат браузером.
  atEnd() {
    if (!this.#scroll) return true;
    // scrollHeight, scrollTop и clientHeight относятся к текущему контейнеру ленты.
    const { scrollHeight, scrollTop, clientHeight } = this.#scroll;
    return scrollHeight - scrollTop - clientHeight <= 2;
  }

  // Возвращает число новых сообщений для счетчика до перехода в конец или смены фильтра.
  get unread() {
    return this.#unread;
  }

  // Проверяет, есть ли видимая строка при текущем фильтре для отображения пустого состояния.
  get empty() {
    return !this.#entries.some(({ row }) => {
      // Проверяет состояние строки, уже вычисленное при вставке или фильтрации.
      return !row.hidden;
    });
  }

  // Создаёт строку с начальным состоянием видимости для указанного сообщения.
  #entry(message) {
    // row содержит готовую строку. Ссылки интерактивны только в прокручиваемом Dashboard.
    const row = messageRow(message, this.#scroll !== null);
    row.hidden = !this.#matches(message);
    return { message, row };
  }

  // Сверяет источник сообщения с текущим фильтром Dashboard.
  #matches(message) {
    return !this.#filter || message.chat_source_id === this.#filter;
  }

  // Фиксирует строку, видимую у верхней кромки, и следующие строки. Если первая будет вытеснена,
  // следующая доступная сохранит своё прежнее вертикальное смещение относительно контейнера.
  #anchor() {
    if (!this.#scroll || this.#following) return [];
    // top определяет верх контейнера, points сохраняет позиции строк от первой видимой до конца.
    const top = this.#scroll.getBoundingClientRect().top;
    const points = [];
    // message задаёт устойчивый seq, row предоставляет текущие экранные координаты видимой строки.
    for (const { message, row } of this.#entries) {
      if (row.hidden) continue;
      // rect даёт верх и низ этой строки относительно области просмотра.
      const rect = row.getBoundingClientRect();
      if (rect.bottom > top) points.push({ seq: message.seq, offset: rect.top - top });
    }
    return points;
  }

  // Прокручивает в конец либо возвращает первое сохранившееся сообщение на прежнюю позицию.
  // Если после вытеснения или фильтра не выжило ни одной строки, показывает начало нового окна.
  #restore(points) {
    if (!this.#scroll) return;
    if (this.#following) {
      this.#scroll.scrollTop = this.#scroll.scrollHeight;
      return;
    }
    // offsets содержит положение каждой кандидатной строки по точному seq перед изменением.
    const offsets = new Map(points.map(({ seq, offset }) => {
      // Преобразует сохранённую точку в пару ключа и смещения для поиска по seq.
      return [seq, offset];
    }));
    // message обозначает очередную удержанную запись, row хранит её новую позицию в ленте.
    for (const { message, row } of this.#entries) {
      if (row.hidden || !offsets.has(message.seq)) continue;
      this.#scroll.scrollTop += row.getBoundingClientRect().top - this.#scroll.getBoundingClientRect().top - offsets.get(message.seq);
      return;
    }
    this.#scroll.scrollTop = 0;
  }
}
