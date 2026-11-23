package session

import (
	"context"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/chat"
	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// TestChatRuntimeWakesOnConfirmedSessionWithoutOutputs проверяет ручной запуск и автозапуск
// через Session Manager без исходящих назначений. t ждёт появления подтверждённой Session.
func TestChatRuntimeWakesOnConfirmedSessionWithoutOutputs(t *testing.T) {
	t.Run("ручной запуск", func(t *testing.T) {
		// t проверяет Session, подтверждённую после явной команды запуска.
		testChatRuntimeStartupWake(t, false)
	})
	t.Run("автозапуск от OBS", func(t *testing.T) {
		// t проверяет Session, подтверждённую тем же менеджером после допуска OBS.
		testChatRuntimeStartupWake(t, true)
	})
}

// testChatRuntimeStartupWake проверяет пробуждение потребителя пустой ленты.
// obs выбирает запуск при допуске OBS, t ограничивает проверку своей временной базой.
func testChatRuntimeStartupWake(t *testing.T, obs bool) {
	t.Helper()
	// fixture предоставляет настоящий Session Manager и временную SQLite без включённых выходов.
	fixture := newManagerFixture(t, false, nil)
	// ctx и cancel ограничивают ожидание подтверждённого запуска и пробуждения потребителя.
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	// chats читает медиа-состояние из общего Core. err сообщает об отказе создания или чтения.
	chats, err := chat.New(ctx, fixture.service, fixture.store, core.DefaultChatLimits(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// cleanup и stop дают Chat Manager собственный срок завершения до освобождения SQLite.
		cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		// err сообщает о невозможности дождаться физических работ Chat Manager.
		if err := chats.Close(cleanup); err != nil {
			t.Error(err)
		}
	})
	// before фиксирует позицию ожидания до принятия команды запуска.
	before, err := chats.Snapshot(ctx, fixture.stream.ID)
	if err != nil || before.Session != nil || len(before.Messages) != 0 || before.Cursor == "" {
		t.Fatalf("Пустая лента потока до запуска: %+v, %v", before, err)
	}
	// page устанавливает согласованное ожидание от пустого снимка.
	page, err := chats.Events(ctx, fixture.stream.ID, before.Cursor, 16)
	if err != nil || page.ResyncRequired || page.Changed == nil || len(page.Events) != 0 {
		t.Fatalf("Ожидание первой сессии: %+v, %v", page, err)
	}
	// id адресует точную сессию, которая должна появиться у проснувшегося потребителя.
	var id core.ID
	if obs {
		// selected содержит принятую Session, feed подтверждает готовность локального входа OBS.
		selected, feed, err := fixture.manager.AdmitOBS(ctx, fixture.stream.ID, fixture.stream.PublicationKey)
		if err != nil || selected.State != core.SessionRunning || feed == nil {
			t.Fatalf("Автозапуск от OBS не вернул готовый вход RUNNING: %s, %v", selected.State, err)
		}
		id = selected.ID
	} else {
		// receipt адресует принятый запуск, подтверждаемый диспетчером Session Manager.
		receipt, err := fixture.manager.StartSession(ctx, core.StartSessionRequest{RequestID: "chat-empty-start", StreamID: fixture.stream.ID})
		if err != nil {
			t.Fatal(err)
		}
		// current содержит окончательный результат штатной команды запуска.
		current := waitReceipt(t, fixture, receipt)
		if current.Status != core.CommandSucceeded {
			t.Fatalf("Запуск Session без назначений не подтверждён: %+v", current)
		}
		id = receipt.SessionID
	}
	for !page.ResyncRequired {
		select {
		case <-page.Changed:
		case <-ctx.Done():
			t.Fatalf("Потребитель пустой ленты не проснулся после подтверждения RUNNING: %v", ctx.Err())
		}
		page, err = chats.Events(ctx, fixture.stream.ID, before.Cursor, 16)
		if err != nil || (page.Changed == nil && !page.ResyncRequired) {
			t.Fatalf("Продолжение после запуска Session: %+v, %v", page, err)
		}
	}
	// after содержит точный подтверждённый запуск новой эпохи и по-прежнему пустую ленту.
	after, err := chats.Snapshot(ctx, fixture.stream.ID)
	if err != nil || after.Session == nil || after.Session.SessionID != id || after.Cursor == before.Cursor || len(after.Messages) != 0 {
		t.Fatalf("Пробуждение не привело к ленте подтверждённого запуска: %+v, %v", after, err)
	}
}
