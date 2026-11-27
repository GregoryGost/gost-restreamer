import { byID, errorMessage } from "./controls.js";
import { apiRoot, ChatHTTPError, requestJSON } from "./transport.js";

// refreshDelay задаёт интервал чтений после завершения запроса, readTimeout ограничивает ожидание ответа.
const refreshDelay = 2000;
const readTimeout = 8000;

// Возвращает адрес проекции оформления для точного идентификатора потока.
function appearancePath(streamID) {
  return apiRoot + "/chat/streams/" + encodeURIComponent(streamID) + "/appearance";
}

// Принимает точное целое JSON в диапазоне, безопасном для Number и CSS.
function boundedNumber(value, min, max) {
  if (typeof value !== "bigint" || value < min || value > max) throw new Error("Некорректное значение оформления.");
  return Number(value);
}

// Проверяет цвет перед заполнением формы и построением значения CSS.
function checkedColor(value) {
  if (typeof value !== "string" || !/^#[0-9a-f]{6}$/i.test(value)) throw new Error("Некорректный цвет оформления.");
  return value;
}

// Проверяет общий для всех слоёв диапазон прозрачности.
function checkedTransparency(value) {
  return boundedNumber(value, 0, 100);
}

// Проверяет явное состояние и RGBA-параметры слоя, в том числе выключенного.
function checkedLayer({ enabled, color, transparency }) {
  if (typeof enabled !== "boolean") throw new Error("Некорректное состояние слоя оформления.");
  return { enabled, color: checkedColor(color), transparency: checkedTransparency(transparency) };
}

// Проверяет ответ перед отображением. Ревизия сохраняет точность BigInt для условной записи.
function checkedProfile({ revision, appearance }) {
  if (typeof revision !== "bigint" || revision < 1n) throw new Error("Некорректная ревизия оформления.");
  // outline и text описывают независимые заливки контура и самого текста.
  const outline = appearance.text_outline;
  const text = appearance.message_text;
  return { revision, appearance: {
    dashboard_background: checkedLayer(appearance.dashboard_background),
    overlay_background: checkedLayer(appearance.overlay_background),
    message_background: checkedLayer(appearance.message_background),
    overlay_transparency: checkedTransparency(appearance.overlay_transparency),
    text_outline: { ...checkedLayer(outline), width_px: boundedNumber(outline.width_px, 0, 8) },
    message_text: { color: checkedColor(text.color), size_px: boundedNumber(text.size_px, 8, 72),
      transparency: checkedTransparency(text.transparency) },
  } };
}

// Возвращает альфа-канал, обратный указанному пользователем проценту прозрачности.
function alpha(transparency) {
  return (100 - transparency) / 100;
}

// Строит цвет CSS из проверенного RGB и отдельного процента прозрачности.
function translucentColor({ color, transparency }) {
  // rgb содержит три канала проверенного цвета в десятичной записи.
  const rgb = [1, 3, 5].map((offset) => {
    // offset указывает начало шестнадцатеричного канала.
    return Number.parseInt(color.slice(offset, offset + 2), 16);
  });
  return "rgba(" + rgb.join(", ") + ", " + alpha(transparency) + ")";
}

// Возвращает цвет включённого фона, прозрачный слой либо пустое свойство для исходного CSS.
function backgroundColor(layer) {
  if (!layer) return "";
  return layer.enabled ? translucentColor(layer) : "transparent";
}

// Применяет оформление к существующим сообщениям. target — лента Dashboard или холст Overlay.
// Null снимает свойства модуля и возвращает исходный CSS.
export function applyChatAppearance(target, appearance, overlay = false) {
  // surface выбирает общий слой, message описывает отдельную подложку каждого сообщения,
  // outline и text задают контур и заливку глифов.
  const surface = overlay ? appearance?.overlay_background : appearance?.dashboard_background;
  const message = appearance?.message_background;
  const outline = appearance?.text_outline;
  const text = appearance?.message_text;
  // properties перечисляет CSS-свойства, которыми владеет модуль. Размеры выражены в rem,
  // чтобы масштабироваться вместе с пользовательским размером текста.
  const properties = {
    "--chat-surface": backgroundColor(surface),
    "--chat-message-background": backgroundColor(message),
    "--chat-text-color": text ? translucentColor(text) : "",
    "--chat-text-size": text ? text.size_px / 16 + "rem" : "",
    "--chat-outline-color": outline?.enabled ? translucentColor(outline) : "",
    "--chat-outline-width": outline?.enabled ? outline.width_px / 16 + "rem" : "",
    "--chat-opacity": overlay && appearance ? String(alpha(appearance.overlay_transparency)) : "",
  };
  // property выбирает имя из таблицы, value задаёт стиль или возвращает исходный CSS.
  for (const [property, value] of Object.entries(properties)) target.style.setProperty(property, value);
}

// Связывает имя поля статической формы с верхним ключом или полем вложенной группы профиля.
function formField(appearance, input) {
  // group указывает верхний ключ, field выбирает вложенный ключ при наличии группы.
  const [group, field] = input.name.split(".");
  return field ? [appearance[group] ??= {}, field] : [appearance, group];
}

// Заполняет форму переданным profile. Без профиля собирает полный набор введённых значений для PUT.
function exchangeForm(form, profile) {
  // result содержит значения для заполнения или постепенно собираемый черновик.
  const result = profile ?? {};
  // input соответствует одному полю сохраняемого HTTP-профиля.
  for (const input of form.querySelectorAll("input[name]")) {
    // container содержит поле текущей группы, key адресует значение этого поля.
    const [container, key] = formField(result, input);
    if (profile) {
      if (input.type === "checkbox") input.checked = container[key];
      else input.value = container[key];
    } else {
      container[key] = input.type === "checkbox" ? input.checked
        : input.type === "number" ? input.valueAsNumber : input.value;
    }
  }
  return result;
}

// Подключает редактор Dashboard. show применяет только подтверждённые сервером настройки.
// После конфликта или неизвестного исхода записи требует новой загрузки профиля.
export function editChatAppearance(form, streamID, show) {
  // fields содержит настройки, save запускает запись, reload загружает сохранённый профиль,
  // status объявляет изменение состояния операции.
  const fields = byID("appearance-fields");
  const save = byID("appearance-save");
  const reload = byID("appearance-reload");
  const status = byID("appearance-status");
  // path задаёт поток, revision хранит последнюю подтверждённую ревизию для записи.
  const path = appearancePath(streamID);
  let revision = null;
  // pending хранит контроллер и базу текущей операции, edited отмечает ввод после её начала,
  // interrupted запоминает базу черновика для сверки при возврате из кэша страницы.
  let pending = null;
  let edited = false;
  let interrupted = null;

  // Объявляет доступность действий без снятия фокуса во время ожидания.
  function syncActions() {
    save.setAttribute("aria-disabled", String(revision === null || pending !== null));
    reload.setAttribute("aria-disabled", String(pending !== null));
  }

  // Загружает или сохраняет профиль. Аргумент draft содержит отправляемые значения или null для чтения.
  // previous передаёт прежнюю ревизию и состояние полей после прерванной операции.
  async function exchange(draft = null, previous = null) {
    if (pending || (draft && revision === null)) return;
    // baseRevision относится к текущим значениям полей. request отменяет операцию при уходе
    // со страницы, а отдельный сигнал ограничивает ожидание по времени.
    const baseRevision = previous?.revision ?? revision;
    const request = new AbortController();
    pending = { request, baseRevision };
    edited = false;
    syncActions();
    status.textContent = draft ? "Сохраняем оформление…" : "Загружаем оформление…";
    try {
      // result возвращает полную проекцию, а условная запись передаёт точную ревизию.
      const result = await requestJSON(path, {
        method: draft ? "PUT" : "GET", body: draft ? { expected_revision: revision, appearance: draft } : undefined,
        signal: AbortSignal.any([request.signal, AbortSignal.timeout(readTimeout)]),
      });
      if (pending?.request !== request) return;
      // profile содержит проверенные CSS-значения и ревизию для следующей записи.
      const profile = checkedProfile(result.data);
      // retainDraft сохраняет поля, которые владелец продолжал редактировать или оставил в кэше.
      const retainDraft = edited || previous?.keepDraft;
      show(profile.appearance);
      if (!draft && !retainDraft) exchangeForm(form, profile.appearance);
      fields.disabled = false;
      if (!draft && retainDraft && baseRevision !== profile.revision) {
        revision = null;
        status.textContent = "Оформление или настройки потока изменились. Черновик сохранён в форме. Перед новой записью загрузите сохранённое оформление.";
        return;
      }
      revision = profile.revision;
      status.textContent = retainDraft ? "Показано сохранённое оформление. Проверьте изменения в форме."
        : draft ? "Оформление сохранено." : "";
    } catch (error) {
      // error отличает отказ валидации, конфликт и неизвестный результат записи.
      if (pending?.request !== request) return;
      if (!draft || (error instanceof ChatHTTPError && error.status === 422)) {
        if (!draft) revision = null;
        status.textContent = errorMessage(error);
      } else {
        revision = null;
        status.textContent = error instanceof ChatHTTPError && error.status === 409
          ? "Оформление или настройки потока изменились. Загрузите сохранённое оформление."
          : "Не удалось подтвердить сохранение. Загрузите сохранённое оформление.";
      }
    } finally {
      if (pending?.request === request) {
        pending = null;
        syncActions();
      }
    }
  }

  form.addEventListener("input", () => {
    // Ввод во время запроса сохраняется в форме даже при успешном ответе.
    if (pending) edited = true;
    else if (revision !== null) status.textContent = "";
  });
  form.addEventListener("submit", (event) => {
    // После нативной проверки диапазонов event отменяет отправку документа браузером.
    event.preventDefault();
    if (!pending && revision !== null) void exchange(exchangeForm(form));
  });
  reload.addEventListener("click", () => {
    // Явно загружает сохранённое состояние из сервера.
    void exchange();
  });
  window.addEventListener("pagehide", () => {
    // Прерывает запрос. Исход незавершённой записи нужно подтвердить новым чтением.
    if (!pending) return;
    pending.request.abort();
    interrupted = { revision: pending.baseRevision, keepDraft: !fields.disabled };
    pending = null;
    revision = null;
    syncActions();
  });
  window.addEventListener("pageshow", (event) => {
    // При возврате event разрешает перечитать прерванную операцию, сохраняя ранее загруженные поля.
    if (!event.persisted || !interrupted) return;
    // previous передаёт исходную базу черновика читающей операции.
    const previous = interrupted;
    interrupted = null;
    void exchange(null, previous);
  });
  save.disabled = false;
  void exchange();
}

// Читает оформление для одного показа Overlay. Запросы идут последовательно.
// Возвращаемая функция отменяет текущий запрос и следующее запланированное чтение.
export function watchOverlayAppearance(target, streamID) {
  // path задаёт поток. controller отменяет весь цикл независимо от тайм-аутов запросов.
  const path = appearancePath(streamID);
  const controller = new AbortController();
  // timer хранит ближайшее чтение, revision исключает применение той же конфигурации повторно.
  let timer;
  let revision = null;

  // При временном отказе сохраняет последнее подтверждённое оформление и планирует новое чтение.
  async function refresh() {
    if (controller.signal.aborted) return;
    try {
      // data содержит актуальный профиль. Тайм-аут освобождает зависшее чтение.
      const { data } = await requestJSON(path, { signal: AbortSignal.any([controller.signal, AbortSignal.timeout(readTimeout)]) });
      if (controller.signal.aborted) return;
      // profile содержит проверенные значения и точную ревизию.
      const profile = checkedProfile(data);
      if (profile.revision !== revision) {
        applyChatAppearance(target, profile.appearance, true);
        revision = profile.revision;
      }
    } catch (error) {
      // error отличает удалённый поток от временного отказа запроса.
      if (controller.signal.aborted) return;
      if (error instanceof ChatHTTPError && error.status === 404) {
        applyChatAppearance(target, null, true);
        return;
      }
    }
    timer = setTimeout(refresh, refreshDelay);
  }

  void refresh();
  return () => {
    // Прерывает текущее чтение и предотвращает следующее после ухода со страницы.
    controller.abort();
    clearTimeout(timer);
  };
}
