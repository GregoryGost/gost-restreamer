import { AuthorizationView } from "./authorization.js";
import { actionButton, bindClose, byID, errorMessage, fillSelect, findEntry, notice, replaceEntryRows, returnFocusOnClose } from "./controls.js";
import { element, providerLabel } from "./feed.js";
import { apiRoot, listItems, requestJSON } from "./transport.js";

// sourcePath адресует чтение и изменение отдельной настройки источника.
const sourcePath = `${apiRoot}/chat-sources`;
// accountPath адресует конфигурационные метаданные и управление сохранённым аккаунтом.
const accountPath = `${apiRoot}/chat/accounts`;

// Подсказки к постоянному идентификатору канала и правила заполнения формы зависят от провайдера.
const channelHints = Object.freeze({
  youtube: ["Channel ID YouTube (начинается с UC)", true],
  twitch: ["ID канала Twitch (пусто для своего канала)", false],
  vk_video_live: ["Короткое имя канала VK Видео Live", true],
});

// Возвращает подпись аккаунта, различая записи одной площадки без введённого названия.
function accountLabel(account) {
  return `${account.name || account.id} · ${providerLabel(account.provider)}`;
}

// Возвращает название источника для настроек, фильтра и оперативного статуса.
export function sourceLabel(source) {
  return source.name || source.channel_hint || providerLabel(source.provider);
}

// Возвращает путь источников одного назначения, доступный также до запуска Session.
function destinationSources(id) {
  return `${apiRoot}/destinations/${encodeURIComponent(id)}/chat-sources`;
}

// SettingsView загружает настройки потока и управляет сохранением по ревизии. Редакторы хранят
// выбранные записи до завершения запроса, даже если другой клиент меняет конфигурацию.
export class SettingsView {
  // Идентификатор потока страницы для чтения списка его назначений.
  #streamID;
  // Кнопки и списки настроек обновляются только после актуального чтения целого набора.
  #status = byID("settings-status");
  // Контроллер текущей загрузки настроек. Следующая загрузка отменяет предыдущую.
  #request;
  // Пути записей, для которых ещё выполняется удаление, включая обновление строк списка.
  #removing = new Set();
  // Назначения хранят только id и отображаемое имя из публичного списка медиаконфигурации.
  #destinations = [];
  // Сохранённая проекция публичных аккаунтов для текущих действий и формы источника.
  #accounts = [];
  // Сохранённые источники всех назначений потока, включая отсутствие активной Session.
  #sources = [];
  // Список становится конфигурационным источником данных после первой успешной загрузки.
  #loaded = false;
  // Диалог создания и редактирования источника.
  #sourceDialog = byID("source-dialog");
  // Форма источника читает текущие значения назначения, аккаунта, канала и признака включения.
  #sourceForm = byID("source-form");
  // Исходная запись источника с CAS-ревизией или null в режиме создания.
  #sourceTarget = null;
  // Диалог и форма редактирования отображаемого имени аккаунта.
  #renameDialog = byID("rename-dialog");
  #renameForm = byID("rename-form");
  // Исходная запись аккаунта с CAS-ревизией на момент открытия формы.
  #renameTarget;
  // Общий диалог запуска, наблюдения и отмены подключения на любой площадке.
  #authorization;
  // Вызывается после актуального чтения для обновления фильтров и действий оперативных статусов.
  #onRead;

  // Связывает формы, кнопки и callback загрузки. Запросы начинаются при явном вызове reload.
  constructor(streamID, onRead) {
    this.#streamID = streamID;
    this.#onRead = onRead;
    this.#authorization = new AuthorizationView(() => {
      // После подтверждённого подключения перечитывает метаданные для выбора аккаунта.
      void this.reload();
    });
    bindClose(this.#sourceDialog);
    bindClose(this.#renameDialog);
    byID("add-source").addEventListener("click", (event) => {
      // Открывает пустой редактор и запоминает кнопку добавления из event.
      this.#editSource(null, event.currentTarget);
    });
    byID("add-account").addEventListener("click", (event) => {
      // Открывает общую форму для создания аккаунта с возвратом к кнопке из event.
      this.#authorization.open(null, event.currentTarget, byID("settings-title"));
    });
    byID("refresh-settings").addEventListener("click", () => {
      // Читает текущую конфигурацию, если обновление ещё не запущено.
      if (!this.#request) void this.reload();
    });
    this.#sourceForm.elements.account.addEventListener("change", () => {
      // Уточняет назначение поля канала при выборе аккаунта другой площадки.
      this.#setChannelHint();
    });
    this.#sourceForm.addEventListener("submit", (event) => {
      // Отправляет форму источника один раз без перехода к документу REST.
      event.preventDefault();
      void this.#saveSource();
    });
    this.#renameForm.addEventListener("submit", (event) => {
      // Отправляет новое имя и ревизию выбранного аккаунта в PATCH.
      event.preventDefault();
      void this.#saveName();
    });
  }

  // Возвращает источники, полученные при последней актуальной загрузке настроек.
  get sources() {
    return this.#sources;
  }

  // Сообщает, прошла ли хотя бы одна полная конфигурационная загрузка для текущего потока.
  get loaded() {
    return this.#loaded;
  }

  // Возвращает публичную запись доступного аккаунта либо undefined для незагруженного ID.
  account(id) {
    return this.#accounts.find((account) => {
      // Сравнивает точные строковые ключи, не имена и не ID площадок.
      return account.id === id;
    });
  }

  // Открывает форму замены аккаунта, сохраняя trigger и fallback для возврата фокуса.
  reconnect(account, trigger, fallback) {
    this.#authorization.open(account, trigger, fallback);
  }

  // Раскрывает настройки и переводит фокус на кнопку редактирования сохранённого источника id.
  // Если запись уже исчезла, оставляет пользователя в панели состояний.
  focusSource(id) {
    // edit принадлежит ровно тому источнику, который указан в состоянии чата.
    const edit = findEntry(byID("sources"), id)?.querySelector('[data-focus-action="edit"]');
    if (!edit) return;
    byID("settings").open = true;
    edit.scrollIntoView({ block: "center" });
    edit.focus({ preventScroll: true });
  }

  // Перечитывает назначения текущего потока, аккаунты и источники. Разные назначения читаются
  // последовательно, чтобы количество параллельных запросов не росло вместе с конфигурацией.
  // Применяет результат только если загрузка ещё актуальна после всех чтений.
  async reload() {
    this.#request?.abort();
    // request отличает эту загрузку от следующей, refresh сохраняет фокус до её завершения.
    const request = new AbortController();
    const refresh = byID("refresh-settings");
    this.#request = request;
    refresh.disabled = false;
    refresh.setAttribute("aria-disabled", "true");
    notice(this.#status, "Загружаем настройки…");
    try {
      // destinations ограничены текущим потоком; accounts служат вариантами всех провайдеров.
      const [destinations, accounts] = await Promise.all([
        listItems(`${apiRoot}/streams/${encodeURIComponent(this.#streamID)}/destinations`, request.signal, ({ id, name }) => {
          // Сохраняет только идентификатор и подпись для выбора назначения.
          return { id, name };
        }),
        listItems(accountPath, request.signal),
      ]);
      // sources собирает источники в порядке назначений. destination выбирает очередной список.
      const sources = [];
      for (const destination of destinations) {
        // source содержит очередную запись назначения. Поэлементное добавление не упирается в число аргументов вызова.
        for (const source of await listItems(destinationSources(destination.id), request.signal)) sources.push(source);
      }
      if (this.#request !== request) return;
      this.#destinations = destinations;
      this.#accounts = accounts;
      this.#sources = sources;
      this.#loaded = true;
      notice(this.#status, "");
      this.#render();
      this.#onRead();
    } catch (error) {
      // error содержит отказ чтения актуального набора настроек.
      if (this.#request !== request) return;
      request.abort();
      byID("no-sources").hidden = true;
      byID("no-accounts").hidden = true;
      notice(this.#status, errorMessage(error));
    } finally {
      if (this.#request === request) {
        this.#request = undefined;
        refresh.removeAttribute("aria-disabled");
      }
    }
  }

  // Показывает доступные операции с каждым источником и аккаунтом. Состояния площадок находятся
  // отдельно в панели ленты, поэтому сохранённая запись аккаунта получает подпись «Сохранён».
  #render() {
    // addSource доступна при наличии назначений и аккаунтов; addAccount доступна после загрузки.
    const addSource = byID("add-source");
    const addAccount = byID("add-account");
    // active проверяет, нужно ли перенести фокус, когда добавление источника становится недоступно.
    const active = document.activeElement;
    addAccount.disabled = false;
    addSource.disabled = !this.#destinations.length || !this.#accounts.length;
    if (active === addSource && addSource.disabled) byID("refresh-settings").focus({ preventScroll: true });
    notice(byID("no-sources"), !this.#destinations.length ? "У потока пока нет медианазначений."
      : !this.#accounts.length ? "Подключите аккаунт, чтобы добавить источник."
      : this.#sources.length ? "" : "Источники ещё не добавлены.");
    notice(byID("no-accounts"), this.#accounts.length ? "" : "Аккаунты ещё не подключены.");
    replaceEntryRows(byID("sources"), this.#sources.map((source) => {
      // Возвращает конфигурацию и действия для этого источника.
      return this.#sourceRow(source);
    }), addSource.disabled ? byID("refresh-settings") : addSource);
    replaceEntryRows(byID("accounts"), this.#accounts.map((account) => {
      // Возвращает метаданные и действия только для этого аккаунта.
      return this.#accountRow(account);
    }), addAccount);
  }

  // Собирает строку конфигурации источника с родительским назначением и явным признаком включения.
  #sourceRow(source) {
    // destination нужен только для видимого имени назначения в строке.
    const destination = this.#destinations.find((item) => {
      // Сверяет сохранённый id родителя, который нельзя поменять редактированием источника.
      return item.id === source.destination_id;
    });
    // row содержит строку списка. actions группирует редактор и удаление этой записи.
    const row = element("li", "entry");
    const actions = element("div", "entry-actions");
    row.dataset.entryId = source.id;
    row.tabIndex = -1;
    row.append(element("strong", "entry-title", sourceLabel(source)), element("p", "entry-meta",
      `${providerLabel(source.provider)} · ${destination?.name || source.destination_id} · ${source.enabled ? "Включён" : "Отключён"}`));
    actions.append(actionButton("Изменить", (event) => {
      // Открывает свойства источника и берёт кнопку возврата фокуса из event.
      this.#editSource(source, event.currentTarget);
    }, "secondary", "edit"), this.#removeButton("Удалить источник", `${sourcePath}/${encodeURIComponent(source.id)}`,
      source.revision, `Удалить источник «${sourceLabel(source)}»?`));
    row.append(actions);
    return row;
  }

  // Собирает строку сохранённого аккаунта, не используя оперативные статусы как подтверждение токена.
  #accountRow(account) {
    // row представляет публичные метаданные, actions содержит допустимые изменения аккаунта.
    const row = element("li", "entry");
    const actions = element("div", "entry-actions");
    row.dataset.entryId = account.id;
    row.tabIndex = -1;
    row.append(element("strong", "entry-title", accountLabel(account)), element("p", "entry-meta", "Сохранён"));
    actions.append(actionButton("Переименовать", (event) => {
      // Открывает редактор имени с возвратом к кнопке из event.
      this.#editName(account, event.currentTarget);
    }, "secondary", "rename"), actionButton("Подключить заново", (event) => {
      // Начинает замену полномочий. При исчезновении аккаунта фокус получает кнопка добавления.
      this.reconnect(account, event.currentTarget, byID("add-account"));
    }, "secondary", "reconnect"), this.#removeButton("Удалить аккаунт", `${accountPath}/${encodeURIComponent(account.id)}`,
      account.revision, `Удалить аккаунт «${accountLabel(account)}»?`));
    row.append(actions);
    return row;
  }

  // Открывает редактор источника и запоминает trigger для возврата фокуса. При изменении
  // сохраняет родителя и заполняет текущие значения source.
  #editSource(source, trigger) {
    if (this.#sourceDialog.open) return;
    this.#sourceTarget = source;
    // fields связывает нативные элементы редактора с выбранной записью.
    const fields = this.#sourceForm.elements;
    byID("source-title").textContent = source ? "Изменение источника" : "Добавление источника";
    fillSelect(fields.destination, this.#destinations.map(({ id, name }) => {
      // Даёт выбор только из назначений потока в адресе Dashboard.
      return [id, name || id];
    }), source?.destination_id);
    fields.destination.disabled = Boolean(source);
    fillSelect(fields.account, this.#accounts.map((account) => {
      // Показывает провайдера вместе с именем, чтобы выбор не зависел от Platform назначения.
      return [account.id, accountLabel(account)];
    }), source?.account_id);
    fields.name.value = source?.name ?? "";
    fields.channel.value = source?.channel_hint ?? "";
    fields.enabled.checked = source?.enabled ?? true;
    this.#setChannelHint();
    notice(this.#sourceForm.querySelector(".notice"), "");
    returnFocusOnClose(this.#sourceDialog, trigger, byID("settings-title"));
    this.#sourceDialog.showModal();
  }

  // Устанавливает понятную подпись и нативное требование заполнения постоянного канала.
  #setChannelHint() {
    // account определяет провайдера, label и required используют проверенное правило его канала.
    const account = this.account(this.#sourceForm.elements.account.value);
    const [label, required] = channelHints[account?.provider] ?? ["Канал", true];
    byID("channel-label").textContent = label;
    this.#sourceForm.elements.channel.required = required;
  }

  // Сохраняет источник одним POST или PATCH. Провайдер, аккаунт, канал и options передаются
  // согласованно, а при изменении проверяется исходная ревизия.
  async #saveSource() {
    // fields содержит значения формы, account определяет единый согласованный вариант площадки.
    const fields = this.#sourceForm.elements;
    const account = this.account(fields.account.value);
    if (!account) return;
    // source определяет режим и ревизию, body переносит то, что пользователь указал в форме.
    const source = this.#sourceTarget;
    const body = { provider: account.provider, account_id: account.id, channel_hint: fields.channel.value,
      name: fields.name.value, enabled: fields.enabled.checked, options: { version: 1, [account.provider]: {} } };
    if (source) body.expected_revision = source.revision;
    // path выбирает существующий источник либо список выбранного назначения для создания.
    const path = source ? `${sourcePath}/${encodeURIComponent(source.id)}` : destinationSources(fields.destination.value);
    await this.#save(this.#sourceDialog, this.#sourceForm, path, source ? "PATCH" : "POST", body);
  }

  // Открывает редактор имени, фиксирует ревизию account и запоминает кнопку trigger.
  #editName(account, trigger) {
    if (this.#renameDialog.open) return;
    this.#renameTarget = account;
    this.#renameForm.elements.name.value = account.name;
    notice(this.#renameForm.querySelector(".notice"), "");
    returnFocusOnClose(this.#renameDialog, trigger, byID("add-account"));
    this.#renameDialog.showModal();
  }

  // Сохраняет имя без отправки реквизитов или изменения провайдера.
  async #saveName() {
    // account сохраняет исходную ревизию, с которой пользователь открыл редактор.
    const account = this.#renameTarget;
    await this.#save(this.#renameDialog, this.#renameForm, `${accountPath}/${encodeURIComponent(account.id)}`, "PATCH",
      { expected_revision: account.revision, name: this.#renameForm.elements.name.value });
  }

  // Отправляет одно изменение и блокирует повторную отправку и закрытие. Кнопка сохранения
  // остаётся доступной для фокуса, а при ошибке форма сохраняет текущие значения.
  async #save(dialog, form, path, method, body) {
    // submit остаётся на месте во время запроса, close блокирует выход, status содержит результат.
    const submit = form.querySelector('[type="submit"]');
    if (submit.getAttribute("aria-disabled") === "true") return;
    const close = form.querySelector("[data-close]");
    const status = form.querySelector(".notice");
    submit.setAttribute("aria-disabled", "true");
    if (document.activeElement === close) submit.focus({ preventScroll: true });
    close.disabled = true;
    // hold удерживает форму во время записи, чтобы запоздалый ответ не менял следующий редактор.
    const hold = (event) => {
      // Отключает Escape до завершения уже отправленного запроса.
      event.preventDefault();
    };
    dialog.addEventListener("cancel", hold);
    notice(status, "Сохраняем…");
    try {
      await requestJSON(path, { method, body });
    } catch (error) {
      // error определяет текст отказа для созданной или изменённой записи.
      notice(status, errorMessage(error));
      return;
    } finally {
      dialog.removeEventListener("cancel", hold);
      submit.removeAttribute("aria-disabled");
      close.disabled = false;
    }
    dialog.close();
    await this.reload();
  }

  // Создаёт кнопку удаления и отражает выполняющийся запрос, даже если строка уже пересобрана.
  #removeButton(text, path, revision, prompt) {
    // button позволяет сохранить фокус на том же действии до результата запроса.
    const button = actionButton(text, (event) => {
      // Использует актуальную кнопку записи из event и запрашивает подтверждение её удаления.
      void this.#remove(event.currentTarget, path, revision, prompt);
    }, "danger", "delete");
    if (this.#removing.has(path)) button.setAttribute("aria-disabled", "true");
    return button;
  }

  // Удаляет ровно выбранную запись после явного подтверждения. Ревизия передаётся в query
  // в точной десятичной форме. Ошибка оставляет видимый список для чтения и исправления.
  async #remove(button, path, revision, prompt) {
    if (this.#removing.has(path) || !window.confirm(prompt)) return;
    // row и list позволяют разблокировать актуальную кнопку после замены строки при чтении.
    const row = button.closest("[data-entry-id]");
    const list = row.parentElement;
    this.#removing.add(path);
    button.setAttribute("aria-disabled", "true");
    notice(this.#status, "Удаляем…");
    try {
      await requestJSON(`${path}?${new URLSearchParams({ expected_revision: revision.toString() })}`, { method: "DELETE" });
      await this.reload();
    } catch (error) {
      // error содержит отказ ревизии, зависимости либо связи с сервером.
      notice(this.#status, errorMessage(error));
    } finally {
      this.#removing.delete(path);
      findEntry(list, row.dataset.entryId)?.querySelector('[data-focus-action="delete"]')?.removeAttribute("aria-disabled");
    }
  }
}
