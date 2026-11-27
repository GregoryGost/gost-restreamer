package chat

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// TestVKVideoLiveManagerControlWaitsForTemporaryBarrier проверяет отменяемое ожидание
// Status и Gap во время временного запрета и повторный допуск той же попытки после отката.
func TestVKVideoLiveManagerControlWaitsForTemporaryBarrier(t *testing.T) {
	// ctx и cancel ограничивают жизнь сценария, включая удерживаемую конфигурационную операцию.
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	// fixture даёт действующий Output и SQLite с согласованным Source.
	fixture := newChatManagerTestFixture(t, ctx)
	// initial и source дают существующий пригодный набор и одну активируемую привязку.
	initial := vkManagerSeedAccount(t, ctx, fixture, vkManagerIssue("vk-controls-token"))
	source := vkManagerBindSource(t, ctx, fixture, 0, initial.Account.ID)
	// readers сообщает о каждой физической попытке до её CONNECTED.
	readers := make(chan vkManagerReader, 2)
	// adapter моделирует полученное протокольное подтверждение отдельно от вызовов sink.
	adapter := vkManagerAdapter{
		// Проверка сообщает о неожиданном выпуске для действующего набора.
		verify: func(context.Context, string, string) (core.VKVideoLiveChatCredentials, error) {
			return core.VKVideoLiveChatCredentials{}, core.ErrInvalid
		},
		// Читатель оставляет управление событиями тесту, пока Manager держит слот попытки.
		read: func(ctx context.Context, request core.ChatReadRequest, sink core.ChatAdapterSink) error {
			return vkManagerKeepReader(ctx, request, sink, readers)
		},
	}
	// manager выделяет допуск одному включённому источнику.
	manager := newVKManagerTest(t, ctx, fixture, adapter)
	chatTestOK(t, fixture.outputs[0].Observe(ctx, core.OutputObservation{State: core.OutputPublishing}))
	// reader содержит управляющие функции исходного поколения до публикации CONNECTED.
	reader := chatTestReceive(t, ctx, readers)
	// before запоминает позицию ленты до блокировки. err сообщает об отказе снимка.
	before, err := manager.Snapshot(ctx, fixture.stream.ID)
	chatTestOK(t, err)
	if !chatTestHasState(before, source.ID, core.ChatConnecting) {
		t.Fatal("Фикстура должна удерживать подтверждение статуса исходного читателя")
	}
	// entered, release и changed отделяют начало барьера, разрешение отката и его завершение.
	entered, release, changed := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	// Горутина держит начатый конфигурационный барьер, пока тест выполняет управляющие вызовы.
	go func() {
		// err содержит результат отклонённой мутации. Функция ждёт разрешения завершить её без записи.
		_, err := manager.MutateSource(ctx, source.ID, func(ctx context.Context) (core.ChatSource, error) {
			close(entered)
			select {
			case <-release:
				return core.ChatSource{}, core.ErrConflict
			case <-ctx.Done():
				return core.ChatSource{}, ctx.Err()
			}
		})
		changed <- err
	}()
	chatTestReceive(t, ctx, entered)
	// blockedCalls хранит две независимые операции. Каждая передаёт исходный токен.
	blockedCalls := []func(context.Context) error{
		// Подтверждение подписки не должно прекращать чтение из-за временного барьера.
		func(ctx context.Context) error {
			return reader.sink.Status(ctx, reader.request.Token, core.ChatConnected, core.ChatReasonNone)
		},
		// Сигнал о потере также ждёт допуска и не должен добавляться при отмене вызова.
		func(ctx context.Context) error {
			return reader.sink.Gap(ctx, reader.request.Token, core.ChatReasonUpstreamGap)
		},
	}
	// index указывает проверяемую операцию, call выполняет её с ограничением ожидания.
	for index, call := range blockedCalls {
		// limited и stop отменяют только этот вызов, сохраняя открытым конфигурационный барьер.
		limited, stop := context.WithTimeout(ctx, 40*time.Millisecond)
		// blockedErr должен описать прекращение ожидания вместо немедленного отзыва попытки.
		blockedErr := call(limited)
		stop()
		if !errors.Is(blockedErr, context.DeadlineExceeded) || !errors.Is(limited.Err(), context.DeadlineExceeded) {
			t.Fatalf("Управляющий вызов %d не дождался отмены при временном барьере: %v", index, blockedErr)
		}
	}
	close(release)
	if err = chatTestReceive(t, ctx, changed); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("Фикстура не завершила конфигурационную операцию откатом: %v", err)
	}
	// unchanged содержит публичные события после двух отменённых вызовов. err сообщает об отказе чтения ленты.
	unchanged, err := manager.Events(ctx, fixture.stream.ID, before.Cursor, 16)
	chatTestOK(t, err)
	// event позволяет обнаружить запрещённую публикацию отменённого управления.
	for _, event := range unchanged.Events {
		if event.Kind == core.ChatEventGap || event.Status != nil && event.Status.SourceID == source.ID && event.Status.State == core.ChatConnected {
			t.Fatal("Отменённый управляющий вызов опубликовал событие после снятия барьера")
		}
	}

	// call повторяет управляющие события той же физической попытки после сохранения старой конфигурации.
	for _, call := range blockedCalls {
		chatTestOK(t, call(ctx))
	}
	chatTestOK(t, reader.publish(ctx, "after-control-barrier"))
	// view проверяет, что сохранённая подписка публикует под исходным Source.
	view := vkManagerMessage(t, ctx, manager, fixture.stream.ID, source.ID, "after-control-barrier")
	if !chatTestHasState(view, source.ID, core.ChatConnected) {
		t.Fatal("Подтверждение после отката не обновило статус исходного источника")
	}
	// page содержит события после повторного допуска. err сообщает об отказе чтения текущей сессии.
	page, err := manager.Events(ctx, fixture.stream.ID, before.Cursor, 16)
	chatTestOK(t, err)
	// gaps считает только действительные сигналы о потерях этой сессии.
	gaps := 0
	// event содержит очередное событие после исходного курсора.
	for _, event := range page.Events {
		if event.Kind == core.ChatEventGap && event.SessionID == fixture.session.ID && event.Reason == core.ChatReasonUpstreamGap {
			gaps++
		}
	}
	if gaps != 1 || page.ResyncRequired {
		t.Fatalf("От исходной попытки ожидалось одно допустимое событие Gap, получено %d", gaps)
	}
	select {
	case <-readers:
		t.Fatal("Временный барьер создал новую физическую попытку")
	default:
	}
}
