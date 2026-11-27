import assert from "node:assert/strict";
import { test } from "node:test";
import { httpsURL } from "./assets/feed.js";
import { ChatStream, parseExactJSON, stringifyExactJSON, supportsExactJSON } from "./assets/transport.js";

// Тест проверяет точную передачу серверной последовательности и CAS-ревизии без подмены строковых ID.
test("публичный JSON сохраняет целые и непрозрачные строки", () => {
  // input содержит граничные значения uint64, соседние значения выше 2^53 и точные строки.
  const input = '{"seq":18446744073709551615,"revision":9007199254740993,"cursor":"9007199254740993/Q+","id":"18446744073709551615","rate":1.25}';
  assert.equal(supportsExactJSON(), true);
  // result используется в форме отправки без преобразования к Number.
  const result = parseExactJSON(input);
  assert.equal(result.seq, 18446744073709551615n);
  assert.equal(result.revision, 9007199254740993n);
  assert.equal(result.cursor, "9007199254740993/Q+");
  assert.equal(result.id, "18446744073709551615");
  assert.equal(stringifyExactJSON(result), input);
});

// Тест проверяет, что недопустимые изображения и ссылки не получают адрес в DOM.
test("отображаемые URL требуют HTTPS без реквизитов", () => {
  assert.equal(httpsURL("https://example.test/картинка.png"), "https://example.test/%D0%BA%D0%B0%D1%80%D1%82%D0%B8%D0%BD%D0%BA%D0%B0.png");
  // value содержит очередной недопустимый относительный адрес, схему либо URL с реквизитами.
  for (const value of ["http://example.test", "javascript:alert(1)", "//example.test", "data:image/png;base64,AAAA", "https://x:y@example.test"]) {
    assert.equal(httpsURL(value), null);
  }
});

// FakeSource даёт тесту управлять доставкой именованных кадров, включая запоздалый callback после close.
class FakeSource {
  // instances хранит все открытые тестом соединения в порядке открытия.
  static instances = [];
  // url содержит адрес открытия для проверяемого query cursor.
  url;
  // handlers хранит callbacks именованных событий тестируемого клиента.
  handlers = new Map();
  // closed сообщает, был ли остановлен автоматический повтор экземпляра.
  closed = false;

  // Имитирует создание EventSource и запоминает фактический адрес запроса.
  constructor(url) {
    this.url = url;
    FakeSource.instances.push(this);
  }

  // Запоминает обработчик именованного кадра от тестируемого клиента.
  addEventListener(kind, handler) {
    this.handlers.set(kind, handler);
  }

  // Отмечает закрытие экземпляра без удаления обработчиков запоздалых кадров.
  close() {
    this.closed = true;
  }

  // Доставляет типизированное событие и его транспортный id, совпадающий с cursor по умолчанию.
  emit(data, id = data.cursor) {
    this.handlers.get(data.kind)({ lastEventId: id, data: stringifyExactJSON(data) });
  }

  // Возвращает точную строковую позицию из адреса создания экземпляра.
  get cursor() {
    return new URL(this.url, "http://localhost").searchParams.get("cursor");
  }
}

// Тест проходит снимок, сообщение, очистку, сетевой сбой, запоздалый кадр и несовпадающий id.
// Последний требует нового снимка. Таймеры сдвигаются вручную, HTTP и SSE заменены подменами.
test("SSE восстанавливается с применённого id и отбрасывает старый экземпляр", async (context) => {
  context.mock.timers.enable({ apis: ["setTimeout"] });
  // snapshots и events записывают применённые значения, requests отмечает число чтений снимка.
  const snapshots = [], events = [];
  let requests = 0;
  globalThis.EventSource = FakeSource;
  context.after(() => {
    // Удаляет замену глобального браузерного конструктора после теста Node.
    delete globalThis.EventSource;
  });
  context.mock.method(globalThis, "fetch", async () => {
    // Выдаёт два независимых курсора снимков без сетевого запроса.
    return new Response(JSON.stringify({ cursor: ++requests === 1 ? "initial+/#" : "new+epoch/#" }));
  });
  // stream открывает снимки и экземпляры источника через подмены.
  const stream = new ChatStream("9007199254740993", {
    // Записывает курсор применённого снимка.
    snapshot(data) { snapshots.push(data.cursor); },
    // Записывает последовательность сообщения или вид управляющего события в порядке применения.
    event(data) { events.push(data.message?.seq ?? data.kind); },
  });
  context.after(() => {
    // Закрывает принадлежащее тесту соединение и очищает остаток таймеров.
    stream.stop();
  });
  stream.start();
  await new Promise(setImmediate);
  // first содержит источник исходного снимка, который будет закрыт после сетевой ошибки.
  const first = FakeSource.instances[0];
  assert.equal(first.cursor, "initial+/#");
  first.emit({ kind: "chat.message", cursor: "exact+/#", message: { seq: 9007199254740993n } });
  first.emit({ kind: "chat.cleared", cursor: "cleared+/#" });
  first.onerror();
  assert.equal(first.closed, true);
  first.emit({ kind: "chat.message", cursor: "obsolete", message: { seq: 9007199254740994n } });
  context.mock.timers.tick(1000);
  // second должен открыться по применённому кадру, а не по позднему и не по первому снимку.
  const second = FakeSource.instances[1];
  assert.equal(second.cursor, "cleared+/#");
  second.emit({ kind: "chat.message", cursor: "mismatched", message: { seq: 9007199254740995n } }, "different");
  assert.equal(second.closed, true);
  context.mock.timers.tick(1000);
  await new Promise(setImmediate);
  assert.deepEqual(events, [9007199254740993n, "chat.cleared"]);
  assert.deepEqual(snapshots, ["initial+/#", "new+epoch/#"]);
  assert.equal(FakeSource.instances[2].cursor, "new+epoch/#");
});

// Тест подтверждает, что остановка страницы до получения снимка исключает его применение.
test("запоздалый снимок не открывает SSE после остановки", async (context) => {
  // respond завершает незаконченный HTTP-запрос тогда, когда страница уже остановлена.
  let respond;
  context.mock.method(globalThis, "fetch", () => {
    // Имитирует ответ, уже доставляемый одновременно с отменой клиентского запроса.
    return new Promise((resolve) => {
      // Передаёт управление временем доставки самому тесту.
      respond = resolve;
    });
  });
  // applied отмечает любую доставку остановленной странице, stream применяет снимок через callbacks.
  let applied = false;
  const stream = new ChatStream("stream", {
    // Отмечает применение снимка, чтобы проверить результат снаружи обработчика ошибок клиента.
    snapshot() { applied = true; },
    // Отмечает неожиданную доставку события после остановки страницы.
    event() { applied = true; },
  });
  stream.start();
  stream.stop();
  respond(new Response('{"cursor":"cursor"}'));
  await new Promise(setImmediate);
  assert.equal(applied, false);
});
