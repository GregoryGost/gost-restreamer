import { AuthorizationView } from "./authorization.js";
import { actionButton, bindClose, byID, errorMessage, fillSelect, notice } from "./controls.js";
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
    byID("add-source").addEventListener("click", () => {
      // Открывает редактор с пустыми полями и текущим набором вариантов.
      this.#editSource(null);
    });
    byID("add-account").addEventListener("click", () => {
      // Вызывает ту же форму подключения, которая используется для замены.
      this.#authorization.open();
    });
    byID("refresh-settings").addEventListener("click", () => {
      // Читает текущую конфигурацию после конфликта ревизии или неизвестного исхода подключения.
      void this.reload();
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

  // Открывает форму повторного подключения после действия в настройках или оперативном статусе.
  reconnect(account) {
    this.#authorization.open(account);
  }

  // Перечитывает назначения текущего потока, аккаунты и источники. Разные назначения читаются
  // последовательно, чтобы количество параллельных запросов не росло вместе с конфигурацией.
  // Применяет результат только если загрузка ещё актуальна после всех чтений.
  async reload() {
    this.#request?.abort();
    // request отличает эту загрузку от последующего вызова reload.
    const request = new AbortController();
    this.#request = request;
    byID("refresh-settings").disabled = true;
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
      if (this.#request === request) byID("refresh-settings").disabled = false;
    }
  }

  // Показывает доступные операции с каждым источником и аккаунтом. Состояния площадок находятся
  // отдельно в панели ленты, поэтому сохранённая запись аккаунта получает подпись «Сохранён».
  #render() {
    byID("add-account").disabled = false;
    byID("add-source").disabled = !this.#destinations.length || !this.#accounts.length;
    notice(byID("no-sources"), !this.#destinations.length ? "У потока пока нет медианазначений."
      : !this.#accounts.length ? "Подключите аккаунт, чтобы добавить источник."
      : this.#sources.length ? "" : "Источники ещё не добавлены.");
    notice(byID("no-accounts"), this.#accounts.length ? "" : "Аккаунты ещё не подключены.");
    byID("sources").replaceChildren(...this.#sources.map((source) => {
      // Возвращает конфигурацию и действия для этого источника.
      return this.#sourceRow(source);
    }));
    byID("accounts").replaceChildren(...this.#accounts.map((account) => {
      // Возвращает метаданные и действия только для этого аккаунта.
      return this.#accountRow(account);
    }));
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
    row.append(element("strong", "entry-title", sourceLabel(source)), element("p", "entry-meta",
      `${providerLabel(source.provider)} · ${destination?.name || source.destination_id} · ${source.enabled ? "Включён" : "Отключён"}`));
    actions.append(actionButton("Изменить", () => {
      // Открывает свойства источника с ревизией из этой строки.
      this.#editSource(source);
    }), actionButton("Удалить источник", (event) => {
      // Запрашивает подтверждение удаления именно выбранного источника.
      void this.#remove(event.currentTarget, `${sourcePath}/${encodeURIComponent(source.id)}`, source.revision, `Удалить источник «${sourceLabel(source)}»?`);
    }, "danger"));
    row.append(actions);
    return row;
  }

  // Собирает строку сохранённого аккаунта, не используя оперативные статусы как подтверждение токена.
  #accountRow(account) {
    // row представляет публичные метаданные, actions содержит допустимые изменения аккаунта.
    const row = element("li", "entry");
    const actions = element("div", "entry-actions");
    row.append(element("strong", "entry-title", accountLabel(account)), element("p", "entry-meta", "Сохранён"));
    actions.append(actionButton("Переименовать", () => {
      // Открывает редактирование без изменения полномочий.
      this.#editName(account);
    }), actionButton("Подключить заново", () => {
      // Начинает ручную замену полномочий через общую форму.
      this.reconnect(account);
    }), actionButton("Удалить аккаунт", (event) => {
      // Просит сервер проверить отсутствие зависимых источников по текущей ревизии.
      void this.#remove(event.currentTarget, `${accountPath}/${encodeURIComponent(account.id)}`, account.revision, `Удалить аккаунт «${accountLabel(account)}»?`);
    }, "danger"));
    row.append(actions);
    return row;
  }

  // Открывает редактор источника. При изменении сохраняет родителя и заполняет текущие значения.
  #editSource(source) {
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

  // Открывает редактор имени и фиксирует ревизию выбранных метаданных.
  #editName(account) {
    if (this.#renameDialog.open) return;
    this.#renameTarget = account;
    this.#renameForm.elements.name.value = account.name;
    notice(this.#renameForm.querySelector(".notice"), "");
    this.#renameDialog.showModal();
  }

  // Сохраняет имя без отправки реквизитов или изменения провайдера.
  async #saveName() {
    // account сохраняет исходную ревизию, с которой пользователь открыл редактор.
    const account = this.#renameTarget;
    await this.#save(this.#renameDialog, this.#renameForm, `${accountPath}/${encodeURIComponent(account.id)}`, "PATCH",
      { expected_revision: account.revision, name: this.#renameForm.elements.name.value });
  }

  // Отправляет одно изменение, временно блокируя диалог. По успеху перечитывает конфигурацию,
  // а по отказу показывает его рядом с сохранёнными значениями формы.
  async #save(dialog, form, path, method, body) {
    // submit исключает повторную отправку, close блокирует закрытие кнопкой, status содержит результат.
    const submit = form.querySelector('[type="submit"]');
    if (submit.disabled) return;
    const close = form.querySelector("[data-close]");
    const status = form.querySelector(".notice");
    submit.disabled = close.disabled = true;
    // hold удерживает форму во время записи, чтобы запоздалый ответ не менял следующий редактор.
    const hold = (event) => {
      // Отключает Escape до завершения уже отправленного запроса.
      event.preventDefault();
    };
    dialog.addEventListener("cancel", hold);
    notice(status, "Сохраняем…");
    try {
      await requestJSON(path, { method, body });
      dialog.close();
      await this.reload();
    } catch (error) {
      // error определяет текст отказа для созданной или изменённой записи.
      notice(status, errorMessage(error));
    } finally {
      dialog.removeEventListener("cancel", hold);
      submit.disabled = close.disabled = false;
    }
  }

  // Удаляет ровно выбранную запись после явного подтверждения. Ревизия передаётся в query
  // в точной десятичной форме. Ошибка оставляет видимый список для чтения и исправления.
  async #remove(button, path, revision, prompt) {
    if (button.disabled || !window.confirm(prompt)) return;
    button.disabled = true;
    notice(this.#status, "Удаляем…");
    try {
      await requestJSON(`${path}?${new URLSearchParams({ expected_revision: revision.toString() })}`, { method: "DELETE" });
      await this.reload();
    } catch (error) {
      // error содержит отказ ревизии, зависимости либо связи с сервером.
      notice(this.#status, errorMessage(error));
    } finally {
      button.disabled = false;
    }
  }
}
