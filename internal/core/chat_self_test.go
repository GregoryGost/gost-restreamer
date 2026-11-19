package core

import (
	"encoding/json"
	"errors"
	"math"
	"strings"
	"testing"
	"time"
)

// TestChatClosedOptionsKeepReceiverOnFailure проверяет отсутствие defaults и частичного присваивания.
func TestChatClosedOptionsKeepReceiverOnFailure(t *testing.T) {
	// options содержит допустимый прежний вариант до каждого отказа; err — отказ конструктора.
	options, err := NewChatOptions(ChatTwitch)
	if err != nil {
		t.Fatal(err)
	}
	// original сохраняет прежнее значение для сравнения после строгого декодирования.
	original := options
	// data содержит очередную некорректную форму: повторный ключ, иной регистр либо лишний null-вариант.
	for _, data := range []string{`{"version":1,"version":1,"youtube":{}}`, `{"version":1,"YouTube":{}}`, `{"version":1,"youtube":{},"twitch":null}`, `{"version":1,"youtube":{"video_id":"chosen"}}`} {
		if err = json.Unmarshal([]byte(data), &options); err == nil || options != original {
			t.Fatal("Некорректные options приняты либо изменили получатель", data, err)
		}
	}
}

// TestChatCredentialAndMessageWeightsAreBounded проверяет полную пару, истёкший срок и арифметику бюджетов.
func TestChatCredentialAndMessageWeightsAreBounded(t *testing.T) {
	// expiry содержит структурно корректный истёкший срок без зависимости от часов теста.
	expiry := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	// bundle содержит полный пользовательский набор с обязательным разрешением чтения.
	bundle := ChatCredentialBundle{Twitch: &TwitchChatCredentials{ClientID: "client", UserID: "123", AccessToken: "access", RefreshToken: "refresh", Scopes: []string{"user:read:chat"}, ExpiresAt: expiry}}
	// err подтверждает, что истёкший срок не нарушает структурную допустимость полного набора.
	if err := bundle.Validate(ChatTwitch); err != nil {
		t.Fatal("Истёкший срок не должен повреждать форму", err)
	}
	bundle.Twitch.Scopes = []string{"user:read:chat", strings.Repeat("x", ChatCredentialBytes)}
	if !errors.Is(bundle.Validate(ChatTwitch), ErrInvalid) {
		t.Fatal("Совокупный набор превысил предел без отказа")
	}
	// err сообщает об обязательном отказе сложения за пределами uint64.
	if _, err := CheckedChatBytes(math.MaxUint64, 1); !errors.Is(err, ErrInvalid) {
		t.Fatal("Переполнение веса принято", err)
	}
	// incoming содержит минимальные нормализованные данные адаптера без серверной идентичности.
	incoming := ChatIncomingMessage{Provider: ChatTwitch, ChannelID: "123", Kind: ChatMessageText, Text: "Сообщение"}
	// before содержит логический вес входа, err — отказ подсчёта.
	before, err := incoming.LogicalBytes()
	if err != nil || incoming.Validate() != nil {
		t.Fatal(err)
	}
	// message содержит серверную привязку, которую также нужно учитывать в RAM-бюджете.
	message := ChatMessage{ChatIncomingMessage: incoming, SessionID: "session", ChatSourceID: "source", Seq: 1, ReceivedAt: expiry}
	// after содержит полный вес сообщения с обоими серверными ID.
	after, err := message.LogicalBytes()
	if err != nil || after != before+uint64(len(message.SessionID)+len(message.ChatSourceID)) || message.Validate() != nil {
		t.Fatal("Потеряны серверные поля веса", before, after, err)
	}
	message.Author.AvatarURL = "http://example.invalid/image"
	if !errors.Is(message.Validate(), ErrInvalid) {
		t.Fatal("Небезопасная схема изображения принята")
	}
}
