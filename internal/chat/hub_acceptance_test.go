package chat

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// TestChatHubChecksAttemptAtFIFOAdmission проверяет отзыв уже подготовленного вызова у входа в очередь.
func TestChatHubChecksAttemptAtFIFOAdmission(t *testing.T) {
	// limits удерживает запись до явной обработки, hub изолирует очередь одной Session.
	limits := core.DefaultChatLimits()
	limits.FlushInterval = time.Minute
	hub := newChatTestHub(t, limits, "session")
	// ended имитирует отзыв предыдущей попытки, attribution сохраняет ранее вычисленный допуск.
	var ended atomic.Bool
	attribution := chatInputAttribution{sourceJSONBytes: 6, lossSource: "source", attemptEnded: &ended}
	// publish связывает тестовую запись с обработчиком, не меняя историю до явного flush.
	publish := func(func(core.ID) func(bool)) {}
	chatTestOK(t, hub.enqueue(chatTestIncoming("before"), 0, publish, attribution))
	hub.inputMu.Lock()
	ended.Store(true)
	hub.inputMu.Unlock()
	// oversize проверяет тот же отзыв в пути учёта потерь.
	oversize := chatTestIncoming("oversize")
	oversize.Parts = make([]core.ChatPart, core.ChatMaxParts+1)
	// message содержит очередной подготовленный callback, err показывает результат точного допуска.
	for _, message := range []core.ChatIncomingMessage{chatTestIncoming("after"), oversize} {
		if err := hub.enqueue(message, 0, publish, attribution); !errors.Is(err, core.ErrStale) {
			t.Fatalf("Вход принял отозванный callback или учёл его как потерю: %v", err)
		}
	}
	hub.inputMu.Lock()
	// count содержит только сообщение до отзыва, lost проверяет отсутствие ложных потерь.
	count, lost := hub.input.len(), hub.inputLost
	hub.inputMu.Unlock()
	if count != 1 || lost != 0 {
		t.Fatalf("Отзыв нарушил очередь приёма: записей %d, потерь %d", count, lost)
	}
}

// newChatTestHub открывает одну изолированную эпоху Hub с выбранными limits.
// t сообщает ошибки создания и освобождает Hub, session различает проверяемый запуск.
func newChatTestHub(t *testing.T, limits core.ChatLimits, session core.ID) *chatHub {
	t.Helper()
	// hub представляет проверяемую ленту, err сообщает отказ её создания.
	hub, err := newChatHub(limits, time.Now, "test-stream", core.RuntimeSessionFence{SessionID: session, Incarnation: "test-incarnation", Lifecycle: 1})
	chatTestOK(t, err)
	t.Cleanup(func() {
		// Останавливает изолированный Hub до выхода из виртуальной среды времени.
		chatTestOK(t, hub.Close(context.Background()))
	})
	return hub
}

// TestChatHubByteBudgetsKeepIndependentWindows проверяет, что байтовое ограничение
// конкретного окна не меняет историю других потребителей. t сравнивает два снимка
// одной позиции, вытеснение replay и отставание подписчика при допустимом числе событий.
func TestChatHubByteBudgetsKeepIndependentWindows(t *testing.T) {
	// t управляет изолированной лентой с виртуальными часами.
	synctest.Test(t, func(t *testing.T) {
		// ctx ограничивает операции чтения и изменения тестового Hub.
		ctx := t.Context()
		// limits пропускает несколько записей по count, но не две длинные записи по байтам.
		limits := core.DefaultChatLimits()
		limits.Replay.Bytes, limits.SSEQueue.Bytes, limits.FlushMessages = 2500, 1024, 1
		// hub хранит оба сообщения в истории независимо от байтовых бюджетов replay и подписки.
		hub := newChatTestHub(t, limits, "session")
		// start фиксирует курсор до приёма записей.
		start := captureChatTestMessages(t, hub, limits)
		// subscription переполнится по размеру первой записи, а не по числу записей.
		subscription, err := hub.subscribe(ctx, start.Cursor)
		chatTestOK(t, err)
		defer subscription.Close()
		// id различает две независимые записи одинакового размера.
		for _, id := range []string{"long-one", "long-two"} {
			// message занимает 1400 байт только текстом. Две записи не поместятся в 2500 байт.
			message := chatTestIncoming(id)
			message.Text = strings.Repeat("x", 1400)
			chatTestOK(t, enqueueChatTestMessage(hub, message, "source", true))
			synctest.Wait()
		}
		// page подтверждает потерю replay от исходной позиции.
		page, err := hub.events(ctx, start.Cursor, 8)
		chatTestOK(t, err)
		if !page.ResyncRequired {
			t.Fatalf("Лимит байтов не вытеснил старый replay: %+v", page)
		}
		page, err = subscription.Events(ctx, 8)
		chatTestOK(t, err)
		if !page.ResyncRequired || len(page.Events) != 1 || page.Events[0].Kind != core.ChatEventResync {
			t.Fatalf("Лимит байтов подписки не ограничил выдачу: %+v", page)
		}
		// full содержит историю при штатном бюджете, narrow ограничивает следующую проекцию.
		full := captureChatTestMessages(t, hub, limits)
		narrow := limits
		narrow.SnapshotBytes, narrow.SnapshotMetadataBytes = 2500, 1000
		// tail содержит только самый свежий допустимый хвост с тем же курсором.
		tail := captureChatTestMessages(t, hub, narrow)
		if len(full.Messages) != 2 || len(tail.Messages) != 1 || tail.Messages[0].ProviderMessageID != "long-two" || tail.Cursor != full.Cursor {
			t.Fatalf("Ограничение снимка нарушило согласованность истории и позиции: полный %d, конец %+v", len(full.Messages), tail)
		}
		narrow.SnapshotMetadataBytes = 1
		if _, err = hub.captureSnapshot(nil).build(ctx, narrow); !errors.Is(err, core.ErrNotReady) {
			t.Fatalf("Превышение бюджета метаданных снимка вернуло %v", err)
		}
	})
}

// TestChatHubMessageJSONPreservesUint64AndPublicFields проверяет точную запись числа
// выше 2^53 и исходные безопасные данные адаптера в публичной ленте. t сверяет её как JSON.
func TestChatHubMessageJSONPreservesUint64AndPublicFields(t *testing.T) {
	// timestamp задаёт независимую платформенную метку нормализованного сообщения.
	timestamp := time.Date(2026, 1, 1, 1, 2, 3, 0, time.UTC)
	// incoming содержит Unicode и безопасные ссылки во всех вложенных публичных моделях.
	incoming := chatTestIncoming("wire-message")
	incoming.ProviderTimestamp = &timestamp
	incoming.Author.Color, incoming.Author.AvatarURL = "#153651", "https://example.invalid/avatar"
	incoming.Parts = []core.ChatPart{{Kind: core.ChatPartEmote, Text: "🙂", EmoteID: "emote", ImageURL: "https://example.invalid/emote", URL: "https://example.invalid/emote-page"}}
	incoming.Badges = []core.ChatBadge{{ID: "badge", Name: "Модератор", ImageURL: "https://example.invalid/badge", URL: "https://example.invalid/badge-page"}}
	// message добавляет серверные значения, включая целое, не представимое как float64.
	message := core.ChatMessage{ChatIncomingMessage: incoming, SessionID: "session", ChatSourceID: "source", Seq: 9007199254740995, ReceivedAt: timestamp.Add(time.Second)}
	// payload содержит опубликованный JSON, err сообщает об отказе кодирования или декодирования.
	payload, err := chatJSONMessage(message)
	chatTestOK(t, err)
	// decoded читает только проверяемые части публичного кадра с точным типом числа.
	var decoded struct {
		// Seq содержит текст JSON-числа без округления.
		Seq json.Number `json:"seq"`
		// Text содержит исходный текст без HTML-интерпретации.
		Text string `json:"text"`
		// ProviderTimestamp хранит независимое время площадки.
		ProviderTimestamp time.Time `json:"provider_timestamp"`
		// Author, Parts и Badges содержат открытые поля автора, эмодзи и бейджей.
		Author map[string]string   `json:"author"`
		Parts  []map[string]string `json:"parts"`
		Badges []map[string]string `json:"badges"`
	}
	chatTestOK(t, json.Unmarshal(payload, &decoded))
	if decoded.Seq.String() != "9007199254740995" || !strings.Contains(string(payload), `"seq":9007199254740995`) ||
		decoded.Text != incoming.Text || !decoded.ProviderTimestamp.Equal(timestamp) ||
		decoded.Author["avatar_url"] != incoming.Author.AvatarURL || decoded.Author["color"] != incoming.Author.Color ||
		len(decoded.Parts) != 1 || decoded.Parts[0]["image_url"] != incoming.Parts[0].ImageURL || decoded.Parts[0]["emote_id"] != "emote" ||
		len(decoded.Badges) != 1 || decoded.Badges[0]["name"] != "Модератор" || decoded.Badges[0]["url"] != incoming.Badges[0].URL {
		t.Fatalf("Публичный JSON изменил точный порядок либо открытые данные: %s", payload)
	}
}

// TestChatHubHistoryAndDedupeUseIndependentBounds проверяет вытеснение ключей и сообщений,
// изоляцию каналов и провайдеров, а также явную потерю записи, превышающей входной бюджет.
// t сохраняет своевременного подписчика на всю последовательность.
func TestChatHubHistoryAndDedupeUseIndependentBounds(t *testing.T) {
	// t изолирует виртуальное время, журнал и подписчика этой ленты.
	synctest.Test(t, func(t *testing.T) {
		// ctx ограничивает чтение подписки и реестра.
		ctx := t.Context()
		// limits содержит байтовый предел истории для одной длинной записи и два ключа дедупликации.
		limits := core.DefaultChatLimits()
		limits.InputQueue.Bytes = 2048
		limits.History.Bytes = 2500
		limits.Dedupe.Count, limits.FlushMessages = 2, 1
		// hub представляет одну проверяемую Session.
		hub := newChatTestHub(t, limits, "session")
		// before и client фиксируют своевременную подписку до приёма сообщений. err сообщает об отказе.
		before := captureChatTestMessages(t, hub, limits)
		client, err := hub.subscribe(ctx, before.Cursor)
		chatTestOK(t, err)
		defer client.Close()
		// source сообщает идентичность исходной конфигурации на границе каждого тестового callback.
		source := core.ID("source")
		// base, otherChat и otherProvider имеют общий ID сообщения в разных областях дедупликации.
		base := chatTestIncoming("shared-id")
		base.Text = strings.Repeat("x", 1400)
		otherChat, otherProvider := base, base
		otherChat.ChannelID = "another-chat"
		otherProvider.Provider = core.ChatTwitch
		// messages задаёт новое поступление a после вытеснения его ключа двумя другими записями.
		messages := []core.ChatIncomingMessage{base, otherChat, otherProvider, base}
		// index определяет ожидаемый серверный порядок, incoming содержит очередной внешний ключ.
		for index, incoming := range messages {
			chatTestOK(t, enqueueChatTestMessage(hub, incoming, source, true))
			synctest.Wait()
			// page читает очередную запись сразу, не допуская обычного отставания подписчика.
			page, err := client.Events(ctx, 4)
			chatTestOK(t, err)
			if page.ResyncRequired || len(page.Events) != 1 || page.Events[0].Kind != core.ChatEventMessage || page.Events[0].Message == nil {
				t.Fatalf("Потеря допустимой записи при вытеснении истории или другого ключа: %+v", page)
			}
			// message содержит сохранённые исходный ключ и следующее серверное число.
			message := page.Events[0].Message
			if message.Provider != incoming.Provider || message.ChannelID != incoming.ChannelID || message.Seq != uint64(index+1) {
				t.Fatalf("Дедупликация смешала ключи или изменила серверный порядок: %+v", message)
			}
		}
		// after хранит только новейшую запись. Живой клиент уже получил все четыре.
		after := captureChatTestMessages(t, hub, limits)
		if len(after.Messages) != 1 || after.Messages[0].Seq != 4 {
			t.Fatalf("Бюджет байтов истории не оставил новейший непрерывный хвост: %+v", after.Messages)
		}
		// oversized превышает входной бюджет, хотя структурный предел сообщения выше.
		oversized := chatTestIncoming("over-input")
		oversized.Text = strings.Repeat("x", 2400)
		if err = enqueueChatTestMessage(hub, oversized, source, true); !errors.Is(err, core.ErrNotReady) {
			t.Fatalf("Сообщение сверх байтов входной очереди принято: %v", err)
		}
		synctest.Wait()
		// gap передаёт число потерь, вызванное только отказом входного бюджета.
		gap, err := client.Events(ctx, 4)
		chatTestOK(t, err)
		if len(gap.Events) != 1 || gap.Events[0].Kind != core.ChatEventGap || gap.Events[0].Lost != 1 {
			t.Fatalf("Отказ по байтам входа не отражён единственной потерей: %+v", gap)
		}
	})
}

// enqueueChatTestMessage принимает в hub сообщение value, представленное исходным источником source.
// commit имитирует заключительную проверку допуска после постановки записи в очередь.
func enqueueChatTestMessage(hub *chatHub, value core.ChatIncomingMessage, source core.ID, commit bool) error {
	return hub.enqueue(value, 0, func(stage func(core.ID) func(bool)) {
		// stage готовит сообщение с выбранным Source. Окончательный допуск определяет его видимость.
		stage(source)(commit)
	})
}

// captureChatTestMessages строит через Hub согласованный снимок с limits на границе теста t.
func captureChatTestMessages(t *testing.T, hub *chatHub, limits core.ChatLimits) core.ChatSnapshot {
	t.Helper()
	// image получает снимок сообщений и согласованный cursor, err сообщает об отказе выдачи.
	image, err := hub.captureSnapshot(nil).build(t.Context(), limits)
	chatTestOK(t, err)
	return image
}

// TestChatHubReplaySnapshotAndDedupeWindow проверяет согласованность снимка и replay,
// удержание порядка независимо от timestamp, изменение атрибуции только для новых записей,
// границу TTL и восстановление по устаревшему или чужому курсору.
// t сообщает о расхождениях в последовательности и содержимом ленты.
func TestChatHubReplaySnapshotAndDedupeWindow(t *testing.T) {
	// t ограничивает сценарий виртуальными часами без зависимостей от внешнего I/O.
	synctest.Test(t, func(t *testing.T) {
		// ctx задаёт срок операций Hub внутри проверки.
		ctx := t.Context()
		// limits оставляет только два сообщения истории и два события replay.
		limits := core.DefaultChatLimits()
		limits.History.Count, limits.Replay.Count, limits.FlushMessages = 2, 2, 1
		limits.DedupeTTL = 2 * time.Second
		// hub задаёт исходную эпоху общей ленты.
		hub := newChatTestHub(t, limits, "session-one")
		// start запоминает пустой снимок до первого полученного сообщения.
		start := captureChatTestMessages(t, hub, limits)
		if start.Cursor == "" || len(start.Messages) != 0 {
			t.Fatalf("Неверная исходная позиция пустой ленты: %+v", start)
		}
		// a и b содержат два времени площадки, идущие в порядке, обратном порядку поступления.
		a, b := chatTestIncoming("a"), chatTestIncoming("b")
		// older и newer обозначают различные платформенные часы в UTC.
		older, newer := time.Now().Add(-2*time.Hour).UTC(), time.Now().Add(-time.Hour).UTC()
		a.ProviderTimestamp, b.ProviderTimestamp = &newer, &older
		a.Parts = []core.ChatPart{{Kind: core.ChatPartText, Text: "исходный фрагмент"}}
		chatTestOK(t, enqueueChatTestMessage(hub, a, "source-a", true))
		synctest.Wait()
		// first фиксирует позицию после первого события.
		first := captureChatTestMessages(t, hub, limits)
		if len(first.Messages) != 1 || first.Messages[0].Seq != 1 || first.Messages[0].ChatSourceID != "source-a" || first.Messages[0].SessionID != "session-one" {
			t.Fatalf("Hub не назначил поля первой записи: %+v", first)
		}
		first.Messages[0].Parts[0].Text = "изменение клиента"
		// now проверяет время приёма второй записи. Пакетизация не должна менять взятое при admission время.
		synctest.Sleep(time.Microsecond)
		now := time.Now().UTC()
		chatTestOK(t, enqueueChatTestMessage(hub, a, "source-b", true))
		chatTestOK(t, enqueueChatTestMessage(hub, b, "source-b", true))
		synctest.Wait()
		// image проверяет первую атрибуцию и отсутствие повторной доставки ключа через другой источник.
		image := captureChatTestMessages(t, hub, limits)
		if len(image.Messages) != 2 || image.Messages[0].ChatSourceID != "source-a" || image.Messages[0].Parts[0].Text != "исходный фрагмент" ||
			image.Messages[1].Seq != 2 || image.Messages[1].ProviderMessageID != "b" || !image.Messages[1].ReceivedAt.Equal(now) ||
			!image.Messages[0].ProviderTimestamp.After(*image.Messages[1].ProviderTimestamp) {
			t.Fatalf("Нарушены дедупликация, копирование снимка или серверный порядок: %+v", image.Messages)
		}
		// replay обязан воспроизвести ровно запись, принятую после снимка first.
		replay, err := hub.events(ctx, first.Cursor, 2)
		chatTestOK(t, err)
		if replay.ResyncRequired || len(replay.Events) != 1 || replay.Events[0].Kind != core.ChatEventMessage || replay.Events[0].Message == nil ||
			replay.Events[0].Message.ProviderMessageID != "b" || replay.Events[0].Cursor != image.Cursor {
			t.Fatalf("Replay после снимка пропустил или повторил запись: %+v", replay)
		}
		// До TTL повтор подавлен. После TTL принимается тот же ключ с новой атрибуцией.
		synctest.Sleep(limits.DedupeTTL)
		chatTestOK(t, enqueueChatTestMessage(hub, a, "source-b", true))
		synctest.Wait()
		image = captureChatTestMessages(t, hub, limits)
		if len(image.Messages) != 2 || image.Messages[0].ProviderMessageID != "b" || image.Messages[1].Seq != 3 ||
			image.Messages[1].ProviderMessageID != "a" || image.Messages[1].ChatSourceID != "source-b" {
			t.Fatalf("Граница TTL или вытеснение старой истории нарушены: %+v", image.Messages)
		}
		// cursor содержит либо вытесненную позицию, либо изменённый непрозрачный курсор.
		for _, cursor := range []core.ChatCursor{start.Cursor, image.Cursor + "x"} {
			replay, err = hub.events(ctx, cursor, 2)
			chatTestOK(t, err)
			if !replay.ResyncRequired {
				t.Fatalf("Негодный курсор не потребовал нового снимка: %+v", replay)
			}
		}
		// next открывает иную точную Session того же Stream.
		next := newChatTestHub(t, limits, "session-two")
		replay, err = next.events(ctx, image.Cursor, 2)
		chatTestOK(t, err)
		if !replay.ResyncRequired || len(captureChatTestMessages(t, next, limits).Messages) != 0 {
			t.Fatalf("Курсор либо история прежней эпохи проникли в новую: %+v", replay)
		}
	})
}

// TestChatHubFlushRollbackAndSizeLimits проверяет, что при окончательном отзыве сообщение
// не расходует порядок и ключ дедупликации, а отказ по конечному JSON даёт явный Gap.
// t задаёт две точки виртуального времени до и на таймере пакетизации.
func TestChatHubFlushRollbackAndSizeLimits(t *testing.T) {
	// t запускает Hub и его часы в изолированной среде.
	synctest.Test(t, func(t *testing.T) {
		// limits разрешает короткий текст, но не сообщение после JSON-экранирования.
		limits := core.DefaultChatLimits()
		limits.FlushInterval, limits.MessageBytes = 50*time.Millisecond, 1024
		// hub хранит очередь сообщений с проверкой итоговой выдачи.
		hub := newChatTestHub(t, limits, "session")
		// start фиксирует исходную позицию для проверки явного Gap.
		start := captureChatTestMessages(t, hub, limits)
		// same может быть доставлено соседним допущенным источником после отзыва первоначального.
		same := chatTestIncoming("same")
		chatTestOK(t, enqueueChatTestMessage(hub, same, "revoked", false))
		synctest.Sleep(limits.FlushInterval - time.Nanosecond)
		synctest.Wait()
		if len(captureChatTestMessages(t, hub, limits).Messages) != 0 {
			t.Fatal("Неподтверждённое сообщение опубликовано до завершения пакета")
		}
		synctest.Sleep(time.Nanosecond)
		synctest.Wait()
		if len(captureChatTestMessages(t, hub, limits).Messages) != 0 {
			t.Fatal("Отозванная после постановки в очередь запись осталась в истории")
		}
		chatTestOK(t, enqueueChatTestMessage(hub, same, "neighbor", true))
		// oversized укладывается во входную структуру. Экранирование каждого < превышает конечный JSON-бюджет.
		oversized := chatTestIncoming("oversized")
		oversized.Text = strings.Repeat("<", 600)
		chatTestOK(t, enqueueChatTestMessage(hub, oversized, "neighbor", true))
		synctest.Sleep(limits.FlushInterval)
		synctest.Wait()
		// image хранит единственную допустимую запись, даже если такой ключ ранее был отозван.
		image := captureChatTestMessages(t, hub, limits)
		if !slices.EqualFunc(image.Messages, []string{"same"}, func(message core.ChatMessage, id string) bool {
			// message должен сохранить первый номер и пройти проверку через живого соседа id.
			return message.ProviderMessageID == id && message.Seq == 1 && message.ChatSourceID == "neighbor"
		}) {
			t.Fatalf("Откат либо отказ по размеру оставил след в истории: %+v", image.Messages)
		}
		// replay должен содержать явное уведомление о единственной локальной потере.
		replay, err := hub.events(t.Context(), start.Cursor, 8)
		chatTestOK(t, err)
		if replay.ResyncRequired || !slices.ContainsFunc(replay.Events, func(event core.ChatEvent) bool {
			// event проверяется по безопасному коду потери и точному числу отброшенных сообщений.
			return event.Kind == core.ChatEventGap && event.Reason == core.ChatReasonLimit && event.Lost == 1
		}) {
			t.Fatalf("Нет уведомления о потере при превышении JSON-бюджета: %+v", replay)
		}
	})
}

// TestChatHubBoundsSlowSubscriberAndInputQueue проверяет независимые конечные очереди.
// t удерживает одну проверку допуска, заполняет вход, затем наблюдает восстановление другого клиента.
func TestChatHubBoundsSlowSubscriberAndInputQueue(t *testing.T) {
	// t задаёт виртуальное ожидание Hub и синхронизированных callbacks.
	synctest.Test(t, func(t *testing.T) {
		// ctx ограничивает ожидания в проверке.
		ctx := t.Context()
		// limits допускает одну запись входа, одно событие у медленной подписки и немедленный flush.
		limits := core.DefaultChatLimits()
		limits.InputQueue.Count, limits.SSEQueue.Count, limits.FlushMessages = 1, 1, 1
		// hub удерживает историю и журнал независимо от живого клиента.
		hub := newChatTestHub(t, limits, "session")
		// start запоминает позицию до постановки записей в очередь.
		start := captureChatTestMessages(t, hub, limits)
		// slow намеренно оставляет события непрочитанными до переполнения.
		slow, err := hub.subscribe(ctx, start.Cursor)
		chatTestOK(t, err)
		defer slow.Close()
		// held и release позволяют отделить активную проверку от очереди ожидающих подтверждения записей.
		held := make(chan struct{})
		release := make(chan struct{})
		defer close(release)
		chatTestOK(t, hub.enqueue(chatTestIncoming("one"), 0, func(stage func(core.ID) func(bool)) {
			// Callback удерживает проверку первого сообщения до снятия тестового барьера.
			close(held)
			<-release
			stage("source")(true)
		}))
		<-held
		chatTestOK(t, enqueueChatTestMessage(hub, chatTestIncoming("two"), "source", true))
		if err = enqueueChatTestMessage(hub, chatTestIncoming("lost"), "source", true); !errors.Is(err, core.ErrNotReady) {
			t.Fatalf("Переполненный вход принял ещё одно сообщение: %v", err)
		}
		release <- struct{}{}
		synctest.Wait()
		// image подтверждает, что медленный подписчик не остановил приём второй записи.
		image := captureChatTestMessages(t, hub, limits)
		if len(image.Messages) != 2 || image.Messages[0].ProviderMessageID != "one" || image.Messages[1].ProviderMessageID != "two" {
			t.Fatalf("Вход потерял допущенную запись или принял лишнюю: %+v", image.Messages)
		}
		// replay прочитывает тот же диапазон напрямую из независимого от клиента журнала.
		replay, err := hub.events(ctx, start.Cursor, 8)
		chatTestOK(t, err)
		if replay.ResyncRequired || len(replay.Events) != 3 {
			t.Fatalf("Медленный клиент повредил общий журнал: %+v", replay)
		}
		if !slices.ContainsFunc(replay.Events, func(event core.ChatEvent) bool {
			// event несёт только одну потерю при переполнении входа.
			return event.Kind == core.ChatEventGap && event.Lost == 1
		}) {
			t.Fatalf("Переполнение входа не дало точный Gap: %+v", replay)
		}
		// slowPage сообщает медленному клиенту, что его собственная очередь утратила согласованность.
		slowPage, err := slow.Events(ctx, 8)
		chatTestOK(t, err)
		if !slowPage.ResyncRequired || !slices.ContainsFunc(slowPage.Events, func(event core.ChatEvent) bool {
			// event может содержать явный сигнал запроса нового снимка.
			return event.Kind == core.ChatEventResync
		}) {
			t.Fatalf("Медленный клиент не получил запрос нового снимка: %+v", slowPage)
		}
		// final подписывается от свежего снимка и должен получить точное закрытие этой эпохи.
		final, err := hub.subscribe(ctx, image.Cursor)
		chatTestOK(t, err)
		defer final.Close()
		chatTestOK(t, hub.Close(ctx))
		<-hub.Done()
		// terminal содержит финальное событие данной сессии.
		terminal, err := final.Events(ctx, 8)
		chatTestOK(t, err)
		if len(terminal.Events) != 1 || terminal.Events[0].Kind != core.ChatEventSessionClosed || terminal.Events[0].SessionID != "session" {
			t.Fatalf("При закрытии Hub потеряно точное терминальное событие: %+v", terminal)
		}
		<-slow.Done()
		terminal, err = slow.Events(ctx, 8)
		chatTestOK(t, err)
		if len(terminal.Events) != 1 || terminal.Events[0].Kind != core.ChatEventSessionClosed || terminal.Events[0].SessionID != "session" {
			t.Fatalf("Отставание лишило клиента терминального события его сессии: %+v", terminal)
		}
	})
}
