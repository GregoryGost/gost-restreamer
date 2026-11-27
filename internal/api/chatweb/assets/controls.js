import { element } from "./feed.js";
import { ChatHTTPError } from "./transport.js";

// Возвращает узел, определённый разметкой страницы с заданным идентификатором.
export function byID(id) {
  return document.getElementById(id);
}

// Показывает либо скрывает короткое сообщение в зарезервированном для него узле.
export function notice(node, text) {
  node.textContent = text;
  node.hidden = !text;
}

// Возвращает понятное описание известного отказа. Ответ сервера не становится текстом интерфейса.
export function errorMessage(error) {
  if (!(error instanceof ChatHTTPError)) return "Не удалось получить ответ сервера.";
  switch (error.status) {
    case 404: return "Запись больше недоступна. Обновите настройки.";
    case 409: return "Запись изменилась или используется. Проверьте актуальное состояние.";
    case 413: return "Данные превышают допустимый размер.";
    case 422: return "Проверьте заполнение полей.";
    case 503: return "Сервер временно не может выполнить запрос.";
    default: return "Сервер отклонил запрос.";
  }
}

// Создаёт кнопку действия и привязывает обработчик к текущей записи экрана.
// focusAction обозначает одно и то же действие при обновлении строки списка.
export function actionButton(text, action, style = "secondary", focusAction) {
  // button получает только локальный текст действия и класс стиля.
  const button = element("button", `button ${style}`, text);
  button.type = "button";
  if (focusAction) button.dataset.focusAction = focusAction;
  button.addEventListener("click", action);
  return button;
}

// Возвращает строку списка по точному идентификатору записи независимо от её подписи и позиции.
export function findEntry(list, id) {
  return Array.from(list.children).find((row) => {
    // Сравнивает идентификатор без интерпретации символов как части CSS-селектора.
    return row.dataset.entryId === id;
  });
}

// Находит прежнее действие в новой строке. Если оно исчезло, выбирает другое действие той же
// записи, а при отсутствии действий возвращает саму строку. Строки доступны для фокуса из кода.
function findEntryFocus(list, id, action) {
  // row обозначает актуальную строку, buttons — её доступные для фокуса действия.
  const row = findEntry(list, id);
  if (!row) return null;
  const buttons = Array.from(row.querySelectorAll("button[data-focus-action]:not(:disabled)"));
  return buttons.find((button) => {
    // Ищет то же действие внутри прежней записи, даже когда соседние кнопки изменились.
    return button.dataset.focusAction === action;
  }) ?? buttons[0] ?? row;
}

// Заменяет строки и восстанавливает фокус, только когда он был внутри этого списка. fallback
// получает фокус, если запись исчезла целиком. Положение страницы при обновлении сохраняется.
export function replaceEntryRows(list, rows, fallback) {
  // active хранит элемент до замены, row связывает его с записью, ownsFocus отмечает этот список.
  const active = document.activeElement;
  const row = active?.closest("[data-entry-id]");
  const ownsFocus = row?.parentElement === list;
  list.replaceChildren(...rows);
  if (ownsFocus && !active.isConnected && document.activeElement === document.body) {
    (findEntryFocus(list, row.dataset.entryId, active.dataset.focusAction) ?? fallback).focus({ preventScroll: true });
  }
}

// Запоминает кнопку открытия диалога. После нативного закрытия восстанавливает фокус, если
// браузер не нашёл прежнюю кнопку, например когда список обновился во время работы с диалогом.
export function returnFocusOnClose(dialog, trigger, fallback) {
  // row и list запоминают, где искать кнопку с тем же действием после обновления списка.
  const row = trigger.closest("[data-entry-id]");
  const list = row?.parentElement;
  dialog.addEventListener("close", () => {
    // Сохраняет фокус, если браузер или пользователь уже перевёл его на доступный элемент.
    if (dialog.open || document.activeElement !== document.body) return;
    // target выбирает исходную кнопку, её актуальную замену или запасной элемент страницы.
    const target = trigger.isConnected && !trigger.disabled ? trigger
      : list ? findEntryFocus(list, row.dataset.entryId, trigger.dataset.focusAction) : null;
    (target ?? fallback).focus({ preventScroll: true });
  }, { once: true });
}

// Заполняет нативный список парами идентификатор и подпись, восстанавливая выбранное значение,
// если оно ещё доступно. Необязательная пустая подпись создаёт явный первый вариант.
export function fillSelect(select, choices, value, emptyLabel) {
  // options включает пустой вариант, если он нужен этому фильтру или полю.
  const options = emptyLabel === undefined ? choices : [["", emptyLabel], ...choices];
  // unchanged сохраняет раскрытый нативный список при обновлении соседних оперативных статусов.
  const unchanged = select.options.length === options.length && options.every(([id, caption], index) => {
    // Сравнивает id и caption с видимым вариантом на той же позиции index.
    return select.options[index].value === id && select.options[index].text === caption;
  });
  if (!unchanged) {
    select.replaceChildren();
    // id остаётся исходным идентификатором, caption содержит видимое имя варианта.
    for (const [id, caption] of options) select.add(new Option(caption, id));
  }
  if (Array.from(select.options).some((option) => {
    // Сверяет исходный выбор с доступными вариантами после обновления списка.
    return option.value === value;
  })) select.value = value;
}

// Привязывает кнопку закрытия обычного редактора к его нативному диалогу.
export function bindClose(dialog) {
  dialog.querySelector("[data-close]").addEventListener("click", () => {
    // Закрывает только тот диалог, которому принадлежит кнопка.
    dialog.close();
  });
}
