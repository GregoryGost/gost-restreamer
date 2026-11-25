package chatwire

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// TestMessageFrameMatchesHubAccounting сверяет полные байты SSE с расчётом Hub,
// в том числе экранирование и точное целое максимального значения.
func TestMessageFrameMatchesHubAccounting(t *testing.T) {
	// message содержит символы, увеличивающие закодированный вес, и точный порядок.
	message := core.ChatMessage{
		ChatIncomingMessage: core.ChatIncomingMessage{Text: "<🧪>\n", Provider: core.ChatTwitch},
		SessionID:           "session", Seq: math.MaxUint64, ReceivedAt: time.Unix(0, 0).UTC(),
	}
	// payload передаёт JSON сообщения, подготовленный для истории; err сообщает об отказе кодирования.
	payload, err := json.Marshal(Message(message))
	if err != nil {
		t.Fatal(err)
	}
	// event переносит одну и ту же запись по обоим путям сериализации.
	event := core.ChatEvent{Kind: core.ChatEventMessage, SessionID: message.SessionID, Cursor: "boundary", Message: &message}
	// accounted содержит JSON для учёта байтов, err сообщает о непредставимых данных.
	accounted, err := EventJSON(event, payload)
	if err != nil {
		t.Fatal(err)
	}
	// size задаёт точный бюджет кадра, посчитанный по полному JSON.
	size := FrameSize(event, accounted)
	// frame содержит фактические байты HTTP, err сообщает об отказе ограничения или сериализации.
	frame, err := Frame(event, size)
	if err != nil {
		t.Fatal(err)
	}
	if uint64(len(frame)) != size || !bytes.Equal(frame, append(append([]byte("id: boundary\nevent: chat.message\ndata: "), accounted...), '\n', '\n')) {
		t.Fatalf("не совпали размеры или байты: %s", frame)
	}
	if !strings.Contains(string(frame), `"seq":18446744073709551615`) || !strings.Contains(string(frame), `"parts":[],"badges":[]`) {
		t.Fatalf("утрачена точность либо форма массивов: %s", frame)
	}
	// err подтверждает отказ ещё до выделения кадра при уменьшении точного бюджета.
	if _, err := Frame(event, size-1); !errors.Is(err, core.ErrObservationLimit) {
		t.Fatalf("превышение кадра на байт принято: %v", err)
	}
}

// TestChatSnapshotWireKeepsServerFencePrivate проверяет узкую сессию и массивы
// в обеих стадиях жизни потока.
func TestChatSnapshotWireKeepsServerFencePrivate(t *testing.T) {
	// snapshot получает точное удостоверение, внутренние поля которого не уходят в HTTP.
	snapshot := core.ChatSnapshot{
		StreamID: "stream", Cursor: "boundary",
		Session: &core.RuntimeSessionFence{SessionID: "session", Incarnation: "private-incarnation", Lifecycle: math.MaxUint64},
	}
	// data содержит публичный активный снимок, err сообщает об отказе кодирования.
	data, err := json.Marshal(Snapshot(snapshot))
	if err != nil || string(data) != `{"stream_id":"stream","session":{"session_id":"session"},"sources":[],"statuses":[],"messages":[],"cursor":"boundary"}` {
		t.Fatalf("неверный снимок: %s %v", data, err)
	}
	snapshot.Session = nil
	data, err = json.Marshal(Snapshot(snapshot))
	if err != nil || !bytes.Contains(data, []byte(`"session":null`)) {
		t.Fatalf("нет явного null до запуска: %s %v", data, err)
	}
	// event сохраняет нулевое число известных потерь для upstream-пробела.
	event := core.ChatEvent{Kind: core.ChatEventGap, Cursor: "boundary", Reason: core.ChatReasonUpstreamGap}
	data, err = EventJSON(event, nil)
	if err != nil || !bytes.Contains(data, []byte(`"lost":0`)) {
		t.Fatalf("нет явного нулевого счётчика разрыва: %s %v", data, err)
	}
}
