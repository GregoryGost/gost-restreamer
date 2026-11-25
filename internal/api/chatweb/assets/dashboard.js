import { applyChatAppearance, editChatAppearance } from "./appearance.js";
import { BroadcastView } from "./broadcast.js";
import { actionButton, byID, errorMessage, fillSelect, notice, replaceEntryRows, returnFocusOnClose } from "./controls.js";
import { element, FeedView, providerLabel, streamIDFromPage } from "./feed.js";
import { SettingsView, sourceLabel } from "./settings.js";
import { apiRoot, ChatHTTPError, ChatStream, requestJSON, supportsExactJSON } from "./transport.js";

// Названия закрытого каталога оперативных состояний. Цвет применяется только как дополнительный знак.
const stateLabels = Object.freeze({
  DISABLED: "Отключён", WAITING_OUTPUT: "Ожидает публикации", DISCOVERING: "Поиск эфира",
  WAITING_CHAT: "Ожидает чат", CONNECTING: "Подключение", CONNECTED: "Подключён",
  RETRYING: "Повторное подключение", AUTH_REQUIRED: "Требуется авторизация", FAILED: "Ошибка", STOPPED: "Остановлен",
});

// Дополнительные подписи безопасных причин, которые помогают понять препятствие или следующее действие.
const reasonLabels = Object.freeze({
  no_broadcast: "Текущий эфир не найден", selection_required: "Найдено несколько эфиров",
  chat_unavailable: "Чат недоступен", chat_ended: "Чат завершён", chat_disabled: "Чат отключён на площадке",
  network: "Площадка временно недоступна", quota: "Достигнут лимит площадки",
  configuration: "Проверьте настройки источника", limit: "Достигнут лимит сервера",
});

// Читаемые состояния транспорта отделены от состояний платформ и собственно сообщений.
const connectionLabels = Object.freeze({
  open: "Сервер на связи", retrying: "Восстанавливаем связь…", missing: "Трансляция не найдена",
});

// unreadAnnouncementDelay задаёт окно объединения объявлений в миллисекундах.
const unreadAnnouncementDelay = 2000;

// Запускает панель для streamID из URL. Связывает оперативную ленту и настройки через публичные
// строковые идентификаторы. История, статусы и предупреждения принадлежат текущему запуску.
function showDashboard() {
  // streamID выбирает потоки HTTP/SSE, connection содержит их статус для пользователя.
  const streamID = streamIDFromPage();
  const connection = byID("connection");
  if (!streamID || !supportsExactJSON() || typeof EventSource !== "function") {
    connection.textContent = !streamID ? "Укажите stream_id в адресе страницы." : "Для чтения чата нужен браузер с поддержкой точных чисел JSON и SSE.";
    notice(byID("empty-feed"), "Лента недоступна.");
    notice(byID("no-status"), "Источники недоступны.");
    notice(byID("no-sources"), "Источники недоступны.");
    notice(byID("no-accounts"), "Аккаунты недоступны.");
    return;
  }
  // viewport прокручивает ленту, follow задаёт режим, more возвращает зрителя к её концу,
  // unreadStatus отдельно сообщает экранному диктору количество новых сообщений.
  const viewport = byID("feed");
  const follow = byID("follow");
  const more = byID("new-messages");
  const unreadStatus = byID("new-messages-status");
  // unreadAnnouncement хранит таймер ближайшего объявления либо undefined между объявлениями.
  let unreadAnnouncement;
  // filter выбирает источник, feed удерживает ограниченные DOM-строки, broadcasts ведёт выбор эфира.
  const filter = byID("filter");
  const feed = new FeedView(byID("messages"), 200, viewport);
  const broadcasts = new BroadcastView();
  // interval содержит желаемый темп. start и stop управляют генератором, clear очищает текущую ленту.
  const interval = byID("test-feed-interval");
  const start = byID("test-feed-start");
  const stop = byID("test-feed-stop");
  const clear = byID("chat-clear");
  // commandStatus показывает результат запроса. commandPending блокирует одновременную отправку из формы.
  const commandStatus = byID("test-feed-status");
  let commandPending = false;
  // sessionID содержит точный действующий запуск, snapshotSources — настройки из текущего снимка.
  let sessionID = "";
  let snapshotSources = [];
  // statuses хранит последнюю оперативную проекцию каждого источника из снимка и событий.
  const statuses = new Map();
  // ready отмечает первый применённый снимок для текста пустой ленты.
  let ready = false;
  // settings читает независимую сохранённую конфигурацию, доступную также до запуска эфира.
  const settings = new SettingsView(streamID, () => {
    // Обновляет подписи источников и действия статусов после актуального чтения настроек.
    showSources();
  });

  // Перечисляет источники текущего запуска и доступную конфигурацию. Действия адресуют выбор
  // эфира, замену полномочий или переход к настройкам именно того источника, у которого возник сбой.
  function showSources() {
    // sources связывает настройки с оперативными статусами по публичному идентификатору.
    const sources = new Map();
    // source обозначает сохранённую конфигурацию. После отдельной загрузки её список отражает
    // удаления через форму, даже если снимок всё ещё содержит прежний источник.
    for (const source of settings.loaded ? settings.sources : snapshotSources) sources.set(source.id, source);
    // configuredSourceIDs включает только записи, для которых доступны строки настроек.
    const configuredSourceIDs = new Set(settings.loaded ? sources.keys() : []);
    // source из согласованного снимка может остаться активным у Session после переноса назначения.
    for (const source of snapshotSources) {
      // state показывает, что источник ещё участвует в текущем запуске.
      const state = statuses.get(source.id)?.state;
      if (sessionID && state && state !== "STOPPED" && state !== "WAITING_OUTPUT" && state !== "DISABLED" && !sources.has(source.id)) {
        sources.set(source.id, source);
      }
    }
    // currentFilter сохраняет активный вариант на время замены списка.
    const currentFilter = filter.value;
    fillSelect(filter, Array.from(sources.values(), (source) => {
      // Строит вариант по ID источника, даже когда несколько источников имеют одинаковое имя.
      return [source.id, sourceLabel(source)];
    }), currentFilter, "Все источники");
    if (filter.value !== currentFilter) feed.filter(filter.value);
    // rows содержит строки статусов, statusTitle принимает фокус при исчезновении записи.
    const rows = [];
    const statusTitle = byID("status-title");
    // id адресует конфигурационный источник, даже если SSE-статус пришёл после предыдущего снимка.
    for (const id of new Set([...sources.keys(), ...statuses.keys()])) {
      // source содержит подпись и связи, status — текущее состояние, row и stateText — его разметку.
      const source = sources.get(id);
      const status = statuses.get(id);
      if (settings.loaded && !source && status?.state === "STOPPED") continue;
      const row = element("li", "entry");
      const stateText = element("p", "status-text", stateLabels[status?.state] ?? (source?.enabled === false ? "Отключён" : "Ожидает публикации"));
      row.dataset.entryId = id;
      row.tabIndex = -1;
      if (status) stateText.dataset.state = status.state;
      row.append(element("strong", "entry-title", source ? sourceLabel(source) : id), stateText);
      if (source) row.append(element("p", "entry-meta", providerLabel(source.provider)));
      if (Object.hasOwn(reasonLabels, status?.reason)) row.append(element("p", "entry-meta", reasonLabels[status.reason]));
      if (status?.dropped > 0n) row.append(element("p", "entry-meta", `Пропущено сообщений: ${status.dropped}`));
      // account нужен только для кнопки восстановления чтения этого источника.
      const account = status?.state === "AUTH_REQUIRED" && source ? settings.account(source.account_id) : null;
      if (account) row.append(actionButton("Подключить заново", (event) => {
        // Открывает замену точного аккаунта. event указывает кнопку для возврата фокуса.
        settings.reconnect(account, event.currentTarget, statusTitle);
      }, "secondary", "reconnect"));
      if (status?.reason === "selection_required" && sessionID) row.append(actionButton("Выбрать эфир", (event) => {
        // Получает варианты для источника и Session, действующей в момент нажатия.
        returnFocusOnClose(byID("broadcast-dialog"), event.currentTarget, statusTitle);
        void broadcasts.open(id);
      }, "secondary", "broadcast"));
      if (status?.state === "FAILED" && configuredSourceIDs.has(id)) row.append(actionButton("Перейти к настройкам источника", () => {
        // Раскрывает настройки и переводит фокус на редактор сохранённого источника.
        settings.focusSource(id);
      }, "secondary", "settings"));
      rows.push(row);
    }
    replaceEntryRows(byID("statuses"), rows, statusTitle);
    notice(byID("no-status"), rows.length ? "" : "У потока пока нет источников чата.");
    showFeedState();
  }

  // Обновляет кнопку сразу, а счётчик для экранного диктора объявляет накопленным итогом.
  // При обнулении удаляет прежнее объявление и отменяет ещё не сработавший таймер.
  function showFeedState() {
    // count учитывает текущий фильтр, unreadText содержит подпись кнопки и краткое объявление.
    const count = feed.unread;
    const unreadText = `Новые сообщения: ${count}`;
    if (count === 0 && document.activeElement === more) viewport.focus({ preventScroll: true });
    more.hidden = count === 0;
    if (more.textContent !== unreadText) more.textContent = unreadText;
    if (count === 0) {
      clearTimeout(unreadAnnouncement);
      unreadAnnouncement = undefined;
      if (unreadStatus.textContent) unreadStatus.textContent = "";
    } else if (unreadStatus.textContent !== unreadText && unreadAnnouncement === undefined) {
      unreadAnnouncement = setTimeout(() => {
        // Читает текущий итог, объединяя сообщения, пришедшие после запуска таймера.
        unreadAnnouncement = undefined;
        unreadStatus.textContent = `Новые сообщения: ${feed.unread}`;
      }, unreadAnnouncementDelay);
    }
    notice(byID("empty-feed"), !feed.empty || (ready && !sessionID) ? "" : !ready ? "Загрузка сообщений…"
      : filter.value ? "От этого источника пока нет сообщений." : "Сообщений пока нет.");
  }

  // Синхронизирует состояние нативного переключателя с режимом ленты.
  function setFollowing(enabled) {
    follow.checked = enabled;
    feed.follow(enabled);
    showFeedState();
  }

  // Разрешает тестовые команды до эфира и очистку загруженной ленты. Во время запроса
  // команды остаются в порядке Tab. Уход фокуса требуется только при смене доступности.
  function syncFeedCommands() {
    // active определяет, теряет ли текущий контрол доступность после обновления ленты.
    const active = document.activeElement;
    clear.disabled = !ready;
    interval.disabled = start.disabled = stop.disabled = clear.disabled || Boolean(sessionID);
    interval.readOnly = commandPending;
    // button последовательно объявляет ту же блокировку для всех вариантов команды.
    for (const button of [start, stop, clear]) {
      if (commandPending && !button.disabled) button.setAttribute("aria-disabled", "true");
      else button.removeAttribute("aria-disabled");
    }
    if ((active === start || active === stop || active === interval || active === clear) && active.disabled) {
      (clear.disabled ? viewport : clear).focus({ preventScroll: true });
    }
  }

  // Отправляет одну команду выбранному потоку. action задаёт её путь, body содержит интервал при запуске.
  // Историю обновляет SSE, поэтому обе страницы видят одну серверную границу очистки.
  async function runFeedCommand(action, body) {
    if (commandPending || (action === "clear" ? clear.disabled : start.disabled)) return;
    commandPending = true;
    syncFeedCommands();
    notice(commandStatus, "");
    try {
      // data содержит фактический интервал запуска либо null у команды без тела ответа.
      const { data } = await requestJSON(`${apiRoot}/chat/streams/${encodeURIComponent(streamID)}/${action}`, { method: "POST", body });
      if (action === "clear") notice(commandStatus, "Чат очищен.");
      else if (!sessionID) {
        notice(commandStatus, action === "test/start" ? `Интервал генерации: ${data.interval_ms} мс.` : "Генерация остановлена.");
      }
    } catch (error) {
      // error содержит безопасный статус отказа. Конфликт запуска означает, что эфир уже начался.
      notice(commandStatus, action === "test/start" && error instanceof ChatHTTPError && error.status === 409
        ? "Тестовые сообщения доступны до начала эфира." : errorMessage(error));
    } finally {
      commandPending = false;
      syncFeedCommands();
    }
  }

  // stream применяет снимок и события к точному состоянию страницы через проверенный SSE-клиент.
  let stream;

  // Начинает новую цепочку чтения при первом показе и после восстановления документа из кэша.
  function startFeed() {
    stream?.stop();
    stream = new ChatStream(streamID, {
      // Обновляет историю, конфигурацию и статусы по одной серверной границе. При том же запуске
      // удерживает видимую строку и считает полученные во время разрыва сообщения.
      snapshot(data) {
        // nextSession остаётся исходной строкой. sameSession определяет сохранение видимой позиции.
        const nextSession = data.session?.session_id ?? "";
        const sameSession = ready && Boolean(nextSession) && nextSession === sessionID;
        if (!sameSession) notice(byID("gap"), "");
        if (sessionID !== nextSession) notice(commandStatus, "");
        sessionID = nextSession;
        broadcasts.setSession(sessionID);
        snapshotSources = data.sources;
        statuses.clear();
        // status связывает состояние конкретного источника с границей этого снимка.
        for (const status of data.statuses) statuses.set(status.source_id, status);
        ready = true;
        syncFeedCommands();
        feed.replace(data.messages, sameSession);
        showSources();
      },
      // Применяет события выбранной ленты. Разрывы площадки выводит отдельно.
      event(event) {
        if ((event.session_id ?? "") !== sessionID) throw new Error("Событие относится к другой трансляции.");
        if (event.kind === "chat.message") {
          feed.append(event.message);
          showFeedState();
        } else if (event.kind === "chat.cleared") {
          feed.replace([], false);
          showFeedState();
        } else if (event.kind === "chat.source_status") {
          statuses.set(event.status.source_id, event.status);
          showSources();
        } else {
          notice(byID("gap"), event.lost > 0n ? `Часть сообщений пропущена: ${event.lost}.` : "Часть сообщений площадки могла быть пропущена.");
        }
      },
      // Убирает закончившуюся ленту и выбор эфира. При resync ждёт согласованную историю.
      reset(kind) {
        if (kind === "chat.session_closed") {
          sessionID = "";
          snapshotSources = [];
          statuses.clear();
          broadcasts.setSession(sessionID);
          feed.replace([], false);
          notice(byID("gap"), "");
          syncFeedCommands();
          showSources();
        } else if (sessionID) {
          notice(byID("gap"), "Восстанавливаем ленту. Часть сообщений могла быть пропущена.");
        } else {
          feed.replace([], false);
          showFeedState();
        }
      },
      // Показывает состояние доставки. При неизвестном потоке убирает сообщение о загрузке.
      connection(state) {
        connection.dataset.state = state;
        connection.textContent = connectionLabels[state];
        if (state === "missing") {
          ready = false;
          syncFeedCommands();
          notice(byID("empty-feed"), "Лента недоступна.");
        }
      },
    });
    stream.start();
  }

  viewport.addEventListener("scroll", () => {
    // Ручная прокрутка вверх останавливает прилипание к концу ленты до явного возобновления.
    if (follow.checked && !feed.atEnd()) setFollowing(false);
  });
  follow.addEventListener("change", () => {
    // Применяет явное переключение автопрокрутки владельцем страницы.
    setFollowing(follow.checked);
  });
  more.addEventListener("click", () => {
    // Переводит фокус в ленту, потому что кнопка исчезает вместе со счётчиком.
    viewport.focus({ preventScroll: true });
    setFollowing(true);
  });
  filter.addEventListener("change", () => {
    // Ограничивает видимую часть уже удержанного окна без изменения курсора.
    feed.filter(filter.value);
    showFeedState();
  });
  byID("test-feed-form").addEventListener("submit", (event) => {
    // event отменяет нативную отправку формы. Сервер принимает целое число миллисекунд.
    event.preventDefault();
    if (!start.disabled) void runFeedCommand("test/start", { interval_ms: interval.valueAsNumber });
  });
  stop.addEventListener("click", () => {
    // Просит сервер завершить генератор и оставить его историю.
    void runFeedCommand("test/stop");
  });
  clear.addEventListener("click", () => {
    // Сервер очищает всю ленту потока независимо от фильтра Dashboard.
    void runFeedCommand("clear");
  });
  window.addEventListener("pagehide", () => {
    // Завершает запрос снимка, EventSource и ожидающее объявление при уходе со страницы.
    stream?.stop();
    clearTimeout(unreadAnnouncement);
    unreadAnnouncement = undefined;
  });
  window.addEventListener("pageshow", (event) => {
    // Открывает новое соединение после возврата страницы из кэша браузера.
    if (event.persisted) startFeed();
  });
  editChatAppearance(byID("appearance-form"), streamID, (appearance) => {
    // Сохраняет видимую историю, когда новый размер шрифта меняет высоту уже показанных строк.
    feed.preserveScroll(() => applyChatAppearance(viewport, appearance));
  });
  startFeed();
  void settings.reload();
}

showDashboard();
