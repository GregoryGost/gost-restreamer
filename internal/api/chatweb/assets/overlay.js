import { FeedView, streamIDFromPage } from "./feed.js";
import { ChatStream, supportsExactJSON } from "./transport.js";

// Overlay оставляет холст прозрачным до доступного снимка. При смене Session удаляет прежнюю ленту.
function showOverlay() {
  // streamID адресует выбранный в URL поток без преобразования к числу.
  const streamID = streamIDFromPage();
  if (!streamID || !supportsExactJSON() || typeof EventSource !== "function") return;
  // feed удерживает ограниченный DOM-хвост, stream читает для него снимки и события сервера.
  const feed = new FeedView(document.getElementById("messages"), 50);
  feed.replace([], false);
  // sessionID различает эфир и ленту до эфира, которую можно сразу убрать при смене эпохи.
  let sessionID = "";
  const stream = new ChatStream(streamID, {
    // Подменяет историю согласованным снимком. Пустой снимок оставляет прозрачный холст.
    snapshot(data) {
      sessionID = data.session?.session_id ?? "";
      feed.replace(data.messages, false);
    },
    // Добавляет сообщения и применяет серверную очистку. Статусы остаются за пределами холста.
    event(event) {
      if (event.kind === "chat.message") feed.append(event.message);
      else if (event.kind === "chat.cleared") feed.replace([], false);
    },
    // Убирает сообщения закрытой сессии или тестовой эпохи до нового снимка.
    reset(kind) {
      if (kind === "chat.session_closed") sessionID = "";
      if (!sessionID) feed.replace([], false);
    },
  });
  stream.start();
  window.addEventListener("pagehide", () => {
    // Освобождает запросы перед уходом со страницы или помещением документа в кэш браузера.
    stream.stop();
  }, { once: true });
}

window.addEventListener("pageshow", showOverlay);
