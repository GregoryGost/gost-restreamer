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
export function actionButton(text, action, style = "secondary") {
  // button получает только локальный текст действия и класс стиля.
  const button = element("button", `button ${style}`, text);
  button.type = "button";
  button.addEventListener("click", action);
  return button;
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
