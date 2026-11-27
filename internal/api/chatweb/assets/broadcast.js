import { bindClose, byID, errorMessage, fillSelect, notice } from "./controls.js";
import { apiRoot, ChatHTTPError, requestJSON } from "./transport.js";

// Выбирает YouTube-эфир из опубликованного набора точной Session. Хранит поколение и ревизию,
// полученные вместе с вариантами. Ответ старого диалога не может обновить новый диалог.
export class BroadcastView {
  // Модальный диалог открывает и закрывает отдельную операцию выбора.
  #dialog = byID("broadcast-dialog");
  // Форма читает выбранное видео из серверного набора.
  #form = byID("broadcast-form");
  // Текущая Session из согласованной ленты. Пустое значение означает отсутствие запуска.
  #session = "";
  // Операция открытого диалога объединяет запрос, серверные варианты и точный ресурс источника.
  #current;

  // Связывает форму с POST выбора и освобождением запроса при закрытии.
  constructor() {
    bindClose(this.#dialog);
    this.#dialog.addEventListener("cancel", () => {
      // Снимает старую операцию до нативного закрытия через Escape.
      this.#clear();
    });
    this.#dialog.querySelector("[data-close]").addEventListener("click", () => {
      // Не позволяет результату предыдущего диалога примениться до события close.
      this.#clear();
    });
    this.#form.addEventListener("submit", (event) => {
      // Отправляет точный выбор текущего открытого диалога без перехода к документу REST.
      event.preventDefault();
      void this.#choose();
    });
  }

  // Запоминает текущий запуск. При смене Session прежний выбор перестаёт быть доступным.
  setSession(sessionID) {
    if (this.#session === sessionID) return;
    this.#session = sessionID;
    this.#clear();
    if (this.#dialog.open) this.#dialog.close();
  }

  // Открывает редактор и читает варианты для точной пары источник и Session.
  async open(sourceID) {
    if (!this.#session || this.#dialog.open) return;
    // current хранит свою Session и контроллер, отличающий это открытие от следующего.
    const current = {
      sourceID, sessionID: this.#session, request: new AbortController(),
      path: `${apiRoot}/chat-sources/${encodeURIComponent(sourceID)}/broadcast-selection`, data: null,
    };
    this.#current = current;
    // status сообщает о загрузке или отказе, submit ожидает подтверждённых вариантов.
    const status = this.#form.querySelector(".notice");
    const submit = this.#form.querySelector('[type="submit"]');
    submit.disabled = true;
    this.#form.elements.video.replaceChildren();
    notice(status, "Загружаем варианты эфира…");
    this.#dialog.showModal();
    try {
      // data содержит живой набор, его ревизию и поколение для последующей проверки выбора.
      const { data } = await requestJSON(`${current.path}?${new URLSearchParams({ session_id: current.sessionID })}`, { signal: current.request.signal });
      if (this.#current !== current || !this.#dialog.open) return;
      if (data.session_id !== current.sessionID || data.source_id !== current.sourceID) throw new Error("Несогласованный список эфиров.");
      current.data = data;
      fillSelect(this.#form.elements.video, data.broadcasts.map(({ video_id, title }) => {
        // Использует читаемое название эфира, сохраняя точное значение video_id.
        return [video_id, title || video_id];
      }));
      submit.disabled = !data.broadcasts.length;
      notice(status, data.broadcasts.length ? "" : "Доступных эфиров пока нет.");
    } catch (error) {
      // error относится только к неотменённому запросу текущего диалога.
      if (this.#current === current && this.#dialog.open) notice(status, errorMessage(error));
    }
  }

  // Передаёт video_id вместе с поколением обнаружения, ревизией и Session сохранённого набора.
  // При 409 пользователь снова открывает форму и выбирает из актуального набора.
  async #choose() {
    // current фиксирует операцию, submit исключает второй выбор до ответа.
    const current = this.#current;
    const submit = this.#form.querySelector('[type="submit"]');
    if (!current?.data || submit.disabled) return;
    // status отображает результат выбора, data сохраняет исходную целочисленную пару ревизий.
    const status = this.#form.querySelector(".notice");
    const data = current.data;
    submit.disabled = true;
    notice(status, "Сохраняем выбор…");
    try {
      await requestJSON(current.path, { method: "POST", signal: current.request.signal, body: {
        session_id: current.sessionID, expected_revision: data.source_revision,
        discovery_generation: data.discovery_generation, video_id: this.#form.elements.video.value,
      } });
      if (this.#current === current && this.#dialog.open) {
        this.#clear();
        this.#dialog.close();
      }
    } catch (error) {
      // error отличает утраченное поколение набора от сетевого либо иного отказа.
      if (this.#current !== current || !this.#dialog.open) return;
      if (error instanceof ChatHTTPError && error.status === 409) {
        notice(status, "Список эфиров изменился. Закройте окно и откройте выбор снова.");
      } else {
        notice(status, errorMessage(error));
        submit.disabled = false;
      }
    }
  }

  // Освобождает браузерный запрос и исключает применение его запоздалого ответа.
  #clear() {
    this.#current?.request.abort();
    this.#current = null;
  }
}
