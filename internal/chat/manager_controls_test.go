package chat

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// TestChatManagerRestoresSharedTerminalStateAfterMutation проверяет статус общей подписки после отката одного источника.
func TestChatManagerRestoresSharedTerminalStateAfterMutation(t *testing.T) {
	// ctx и cancel ограничивают изменение конфигурации и остановку общего читателя.
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	// limits оставляет принятую запись в очереди до конечного статуса.
	limits := core.DefaultChatLimits()
	limits.FlushInterval = time.Minute
	// fixture предоставляет двух участников, manager удерживает барьер первого.
	// adapter сообщает об открытии подписок, reader обслуживает обоих участников.
	fixture, manager, adapter, reader := startChatControlTest(t, ctx, limits)
	chatTestOK(t, reader.publish(ctx, "до-конечного-статуса"))
	// entered отмечает вход в CAS, release разрешает его завершение конфликтом, result получает итог операции.
	entered, release := make(chan struct{}), make(chan struct{})
	result := make(chan error, 1)
	// Callback выполняет управляемое изменение только меньшего источника.
	go func() {
		// err сохраняет итог операции. Callback ждёт указания завершить CAS конфликтом.
		_, err := manager.MutateSource(ctx, fixture.sources[0].ID, func(operation context.Context) (core.ChatSource, error) {
			// Callback сообщает, что конфигурационный барьер установлен, и ждёт управляемого отказа.
			close(entered)
			select {
			case <-release:
				return core.ChatSource{}, core.ErrConflict
			case <-operation.Done():
				return core.ChatSource{}, operation.Err()
			}
		})
		result <- err
	}()
	chatTestReceive(t, ctx, entered)
	chatTestOK(t, reader.sink.Status(ctx, reader.request.Token, core.ChatAuthRequired, core.ChatReasonCredentials))
	chatTestReceive(t, ctx, reader.stopped)
	close(release)
	if !errors.Is(chatTestReceive(t, ctx, result), core.ErrConflict) {
		t.Fatal("Откат источника потерял причину конфликта")
	}
	// err сообщает об отклонении сообщения от завершённого читателя.
	if err := reader.publish(ctx, "после-конечного-статуса"); !errors.Is(err, core.ErrStale) {
		t.Fatalf("Завершённый читатель принял новое сообщение: %v", err)
	}
	manager.mu.Lock()
	// hub принадлежит всё ещё действующей Session. Обработка начинается после терминального статуса и снятия барьера.
	hub := manager.sessions[fixture.stream.ID].hub
	manager.mu.Unlock()
	hub.flush()
	// snapshot показывает итоговые статусы и принятые сообщения, err сообщает об отказе чтения.
	snapshot, err := manager.Snapshot(ctx, fixture.stream.ID)
	chatTestOK(t, err)
	if !chatTestHasState(snapshot, fixture.sources[0].ID, core.ChatAuthRequired) || !chatTestHasState(snapshot, fixture.sources[1].ID, core.ChatAuthRequired) {
		t.Fatalf("Источник после отката потерял конечное состояние общей подписки: %+v", snapshot.Statuses)
	}
	if len(snapshot.Messages) != 1 || snapshot.Messages[0].ChatSourceID != fixture.sources[0].ID || snapshot.Messages[0].ProviderMessageID != "до-конечного-статуса" {
		t.Fatalf("Терминальный статус потерял принятое сообщение: %+v", snapshot.Messages)
	}
	select {
	case <-adapter.readers:
		t.Fatal("Восстановление статуса открыло новую подписку без новых полномочий")
	default:
	}
}

// TestChatManagerClosesIngressBeforeTerminalVisibility проверяет отзыв приёма до публикации конечного статуса.
func TestChatManagerClosesIngressBeforeTerminalVisibility(t *testing.T) {
	// ctx и cancel ограничивают два конкурентных вызова от одного читателя.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// reader предоставляет токен и вход действующей синтетической подписки.
	_, _, _, reader := startChatControlTest(t, ctx, core.DefaultChatLimits())
	// attempt даёт наблюдение отзыва приёма, hub позволяет задержать публикацию статуса.
	attempt := reader.sink.(*readerAttempt)
	hub := attempt.reader.key.session.hub
	// result принимает итог терминального callback после освобождения Hub.
	result := make(chan error, 1)
	// interval будит проверку атомарного снимка, deadline ограничивает ожидание отзыва.
	interval := time.NewTicker(time.Millisecond)
	defer interval.Stop()
	deadline := time.NewTimer(time.Second)
	defer deadline.Stop()
	hub.mu.Lock()
	// Callback отзывает приём и ждёт возможности опубликовать AUTH_REQUIRED.
	go func() {
		result <- reader.sink.Status(ctx, reader.request.Token, core.ChatAuthRequired, core.ChatReasonCredentials)
	}()
	for attempt.ingress.Load() != nil {
		select {
		case <-deadline.C:
			hub.mu.Unlock()
			chatTestOK(t, chatTestReceive(t, ctx, result))
			t.Fatal("Приём остался открыт в ожидании публикации конечного статуса")
		case <-interval.C:
		}
	}
	// err должен подтвердить отзыв, пока новый статус ещё недоступен подписчикам.
	err := reader.publish(ctx, "во-время-конечного-статуса")
	hub.mu.Unlock()
	chatTestOK(t, chatTestReceive(t, ctx, result))
	if !errors.Is(err, core.ErrStale) {
		t.Fatalf("Вход после отзыва принял сообщение: %v", err)
	}
}

// TestChatManagerSignalsSnapshotAfterSourceDisable проверяет уведомление после исчезновения активного статуса.
func TestChatManagerSignalsSnapshotAfterSourceDisable(t *testing.T) {
	// ctx и cancel ограничивают изменение и получение одной страницы событий.
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	// fixture содержит два источника, manager и reader предоставляют текущую ленту и синтетического читателя.
	fixture, manager, _, reader := startChatControlTest(t, ctx, core.DefaultChatLimits())
	// before содержит позицию со статусом CONNECTED, err сообщает об отказе чтения.
	before, err := manager.Snapshot(ctx, fixture.stream.ID)
	chatTestOK(t, err)
	// subscriber открывает доставку с той же позиции до изменения.
	subscriber, err := manager.Subscribe(ctx, fixture.stream.ID, before.Cursor)
	chatTestOK(t, err)
	defer subscriber.Close()
	// source содержит точную исходную ревизию отключаемого участника.
	source := fixture.sources[0]
	source.Enabled = false
	// Callback подтверждает отключение в SQLite через реальный CAS.
	_, err = manager.MutateSource(ctx, source.ID, func(operation context.Context) (core.ChatSource, error) {
		return fixture.service.UpdateChatSource(operation, source, source.Revision)
	})
	chatTestOK(t, err)
	// live содержит запрос на новый снимок уже допущенного клиента.
	live, err := subscriber.Events(ctx, 8)
	chatTestOK(t, err)
	if !live.ResyncRequired || len(live.Events) != 1 || live.Events[0].Kind != core.ChatEventResync {
		t.Fatalf("Клиент не получил уведомление об удалении статуса: %+v", live)
	}
	// replay должен сообщить ту же позицию клиенту, подключившемуся после отключения.
	replay, err := manager.Events(ctx, fixture.stream.ID, before.Cursor, 8)
	chatTestOK(t, err)
	if !replay.ResyncRequired || len(replay.Events) != 1 || replay.Events[0].Kind != core.ChatEventResync || replay.Cursor != live.Cursor {
		t.Fatalf("Журнал потерял позицию удаления статуса: %+v", replay)
	}
	// after содержит последнюю конфигурацию и независимый статус соседнего участника.
	after, err := manager.Snapshot(ctx, fixture.stream.ID)
	chatTestOK(t, err)
	if after.Cursor == before.Cursor || !chatTestHasState(after, source.ID, core.ChatDisabled) || !chatTestHasState(after, fixture.sources[1].ID, core.ChatConnected) {
		t.Fatalf("Снимок не отразил отключение только изменённого источника: %+v", after)
	}
	chatTestOK(t, reader.publish(ctx, "оставшийся-источник"))
}

// startChatControlTest открывает два источника общей беседы на временной SQLite.
// Тест владеет остановкой Manager и получает точное подтверждение первого чтения.
func startChatControlTest(t *testing.T, ctx context.Context, limits core.ChatLimits) (chatManagerTestFixture, *Manager, *chatTestAdapter, chatTestReader) {
	t.Helper()
	// fixture готовит текущий сохранённый запуск и его наблюдателей.
	fixture := newChatManagerTestFixture(t, ctx)
	// adapter передаёт тесту синтетические экземпляры подписки без сети.
	adapter := &chatTestAdapter{readers: make(chan chatTestReader, 8)}
	// manager содержит проверяемый реестр, err сообщает об отказе запуска.
	manager, err := New(ctx, fixture.service, fixture.store, limits, []core.ChatAdapterPort{adapter})
	chatTestOK(t, err)
	t.Cleanup(func() {
		// Закрывает Manager раньше SQLite. stopCtx и cancel задают отдельный конечный срок.
		stopCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		chatTestOK(t, manager.Close(stopCtx))
	})
	// output подтверждает передачу очередного выхода после создания чатов.
	for _, output := range fixture.outputs {
		chatTestOK(t, output.Observe(ctx, core.OutputObservation{State: core.OutputPublishing}))
	}
	// reader представляет одну подписку до любых административных изменений.
	reader := chatTestReceive(t, ctx, adapter.readers)
	waitChatTestSnapshot(t, ctx, manager, fixture.stream.ID, func(image core.ChatSnapshot) bool {
		// Предикат ждёт, пока два источника получат подтверждённое состояние общей подписки.
		return chatTestHasState(image, fixture.sources[0].ID, core.ChatConnected) && chatTestHasState(image, fixture.sources[1].ID, core.ChatConnected)
	})
	return fixture, manager, adapter, reader
}

// TestChatManagerConfirmsSourceEditsAndDeletion проверяет применение CAS и отзыв только изменённого источника.
func TestChatManagerConfirmsSourceEditsAndDeletion(t *testing.T) {
	// ctx и cancel ограничивают ожидание реестра и синтетического читателя.
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	// fixture предоставляет текущие источники, manager применяет изменения, reader обслуживает обоих.
	fixture, manager, _, reader := startChatControlTest(t, ctx, core.DefaultChatLimits())
	// source содержит меньший ID, у которого меняется только имя.
	source := fixture.sources[0]
	source.Name = "Новое имя источника"
	// renamed получает сохранённые метаданные, err сообщает об ошибке CAS.
	renamed, err := fixture.service.UpdateChatSource(ctx, source, source.Revision)
	chatTestOK(t, err)
	// result подтверждает применение точной сохранённой ревизии.
	result, err := manager.ApplySource(ctx, renamed.ID, renamed.Revision)
	chatTestOK(t, err)
	if result.Revision != renamed.Revision || result.Reconnected {
		t.Fatalf("Переименование изменило готовое чтение: %+v", result)
	}
	if _, err = manager.ApplySource(ctx, renamed.ID, source.Revision); !errors.Is(err, core.ErrStale) {
		t.Fatalf("Ревизия, заменённая переименованием, принята: %v", err)
	}
	if _, err = manager.MutateSource(ctx, renamed.ID, func(context.Context) (core.ChatSource, error) {
		// Имитирует подтверждённый конфликт CAS с сохранением прежней конфигурации.
		return core.ChatSource{}, core.ErrConflict
	}); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("Исход конфликта CAS потерян: %v", err)
	}
	chatTestOK(t, reader.publish(ctx, "до-удаления"))
	waitChatTestSnapshot(t, ctx, manager, fixture.stream.ID, func(image core.ChatSnapshot) bool {
		// Предикат подтверждает прежнюю атрибуцию после изменения имени и отката.
		return len(image.Messages) == 1 && image.Messages[0].ChatSourceID == source.ID
	})
	if err = manager.ApplySourceDelete(ctx, renamed.ID); !errors.Is(err, core.ErrStale) {
		t.Fatalf("Существующая строка принята как удалённая: %v", err)
	}
	if err = manager.DeleteSource(ctx, renamed.ID, source.Revision); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("Удаление по старой ревизии принято: %v", err)
	}
	chatTestOK(t, manager.DeleteSource(ctx, renamed.ID, renamed.Revision))
	chatTestOK(t, manager.ApplySourceDelete(ctx, renamed.ID))
	chatTestOK(t, reader.publish(ctx, "после-удаления"))
	waitChatTestSnapshot(t, ctx, manager, fixture.stream.ID, func(image core.ChatSnapshot) bool {
		// Предикат проверяет, что удаление оставило только соседа и сохранило первую запись ленты.
		return len(image.Sources) == 1 && len(image.Messages) == 2 && image.Messages[0].ChatSourceID == source.ID && image.Messages[1].ChatSourceID == fixture.sources[1].ID
	})
}

// TestChatManagerReplacesAccountAccessAfterConfirmation проверяет отличия имени аккаунта и ручной авторизации.
func TestChatManagerReplacesAccountAccessAfterConfirmation(t *testing.T) {
	// ctx и cancel ограничивают синхронные операции и физическое завершение читателей.
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	// fixture, manager, adapter и reader связывают сохранённый аккаунт с первой подпиской.
	fixture, manager, adapter, reader := startChatControlTest(t, ctx, core.DefaultChatLimits())
	// account содержит только метаданные, err передаёт результат чтения.
	account, err := fixture.service.GetChatAccount(ctx, fixture.sources[0].AccountID)
	chatTestOK(t, err)
	account.Name = "Отображаемое имя"
	// renamed содержит новую конфигурационную ревизию без замены полномочий.
	renamed, err := fixture.service.UpdateChatAccount(ctx, account, account.Revision)
	chatTestOK(t, err)
	// err должен сообщить, что одного чтения текущей конфигурации для замены недостаточно.
	if _, err = manager.mutateAccount(ctx, account.ID, func(context.Context) (credentialReplacement, error) {
		// Возвращает метаданные без записи и без подтверждённой квитанции прежней замены.
		return credentialReplacement{account: renamed}, nil
	}); !errors.Is(err, core.ErrStale) {
		t.Fatalf("Чтение той же ревизии принято как смена полномочий: %v", err)
	}
	chatTestOK(t, reader.publish(ctx, "после-имени"))
	waitChatTestSnapshot(t, ctx, manager, fixture.stream.ID, func(image core.ChatSnapshot) bool {
		// Предикат убеждается, что старый reader по-прежнему публикует после переименования.
		return len(image.Messages) == 1
	})
	// replacement содержит синтетический заменяющий ключ.
	replacement := core.ChatCredentialBundle{YouTube: &core.YouTubeChatCredentials{APIKey: "next-synthetic-api-key"}}
	// result сверяет новый доступ и подтверждает отзыв единственного разделяемого reader.
	result, err := manager.ownedCredentials.Replace(ctx, manager, renamed.ID, renamed.Provider, replacement, "new-chat-access", renamed.Revision)
	chatTestOK(t, err)
	if result.Revision != renamed.Revision+1 || !result.Reconnected {
		t.Fatalf("Замена полномочий не применена к действующему чтению: %+v", result)
	}
	chatTestReceive(t, ctx, reader.stopped)
	if err = reader.publish(ctx, "устаревший-доступ"); !errors.Is(err, core.ErrStale) {
		t.Fatalf("Прежняя подписка приняла сообщение: %v", err)
	}
	// newReader запущен с новой парой полномочий после физического освобождения слота.
	newReader := chatTestReceive(t, ctx, adapter.readers)
	if newReader.request.Token == reader.request.Token || newReader.request.Credentials.Bundle.YouTube.APIKey != replacement.YouTube.APIKey {
		t.Fatal("Новый reader получил прежнее поколение или прежние полномочия")
	}
	// externallySaved представляет подтверждённую замену вызовом Service до применения её Manager.
	externallySaved, err := fixture.service.ReplaceChatCredentials(ctx, renamed.ID, renamed.Provider, replacement, "confirmed-chat-access", result.Revision)
	chatTestOK(t, err)
	result, err = manager.ApplyAccount(ctx, renamed.ID, externallySaved.Account.Revision)
	chatTestOK(t, err)
	if !result.Reconnected {
		t.Fatal("Подтверждённая ручная замена сохранила прежнее чтение")
	}
	chatTestReceive(t, ctx, newReader.stopped)
	// currentReader обслуживает последнее поколение после подтверждения внешнего сохранения.
	currentReader := chatTestReceive(t, ctx, adapter.readers)
	if _, err = manager.ApplyAccount(ctx, account.ID, renamed.Revision+1); !errors.Is(err, core.ErrStale) {
		t.Fatalf("Устаревшее подтверждение принятой ранее замены применено повторно: %v", err)
	}
	chatTestOK(t, currentReader.publish(ctx, "после-устаревшего-подтверждения"))
	waitChatTestSnapshot(t, ctx, manager, fixture.stream.ID, func(image core.ChatSnapshot) bool {
		// Предикат подтверждает, что актуальный читатель продолжает публикацию.
		return len(image.Messages) == 2
	})
}

// TestChatManagerSubscriptionKeepsTerminalEpoch проверяет общий предел клиентов и последнюю запись закрываемой ленты.
func TestChatManagerSubscriptionKeepsTerminalEpoch(t *testing.T) {
	// ctx и cancel ограничивают ожидание последнего события сессии.
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	// limits допускает только одну активную подписку, а очередь вмещает терминальное событие.
	limits := core.DefaultChatLimits()
	limits.SSEClients, limits.SSEClientsPerStream = 1, 1
	// fixture даёт точного родителя, manager выдаёт клиентские очереди.
	fixture, manager, _, _ := startChatControlTest(t, ctx, limits)
	// snapshot содержит начальную позицию новой клиентской очереди, err сообщает об отказе чтения.
	snapshot, err := manager.Snapshot(ctx, fixture.stream.ID)
	chatTestOK(t, err)
	// subscriber сохраняет свою эпоху после отзыва точного родителя.
	subscriber, err := manager.Subscribe(ctx, fixture.stream.ID, snapshot.Cursor)
	chatTestOK(t, err)
	defer subscriber.Close()
	if _, err = manager.Subscribe(ctx, fixture.stream.ID, snapshot.Cursor); !errors.Is(err, core.ErrBusy) {
		t.Fatalf("Лимит одновременно допущенных клиентов не сработал: %v", err)
	}
	chatTestOK(t, fixture.parent.Close(ctx))
	waitChatTestSnapshot(t, ctx, manager, fixture.stream.ID, func(image core.ChatSnapshot) bool {
		// Предикат ждёт перехода новых клиентов к пустому снимку Stream.
		return image.Session == nil
	})
	// page показывает, что обычное чтение прежней позиции требует нового snapshot.
	page, err := manager.Events(ctx, fixture.stream.ID, snapshot.Cursor, 8)
	chatTestOK(t, err)
	if !page.ResyncRequired {
		t.Fatal("Курсор закрытой эпохи принят без повторного снимка")
	}
	chatTestReceive(t, ctx, subscriber.Done())
	// terminal содержит кадры ранее допущенного клиента после завершения родителя.
	terminal, err := subscriber.Events(ctx, limits.SSEQueue.Count+1)
	chatTestOK(t, err)
	if len(terminal.Events) == 0 {
		t.Fatal("Клиент потерял закрытие точной сессии")
	}
	// event завершает старую клиентскую очередь после всех ранее опубликованных кадров.
	event := terminal.Events[len(terminal.Events)-1]
	if event.Kind != core.ChatEventSessionClosed || event.SessionID != fixture.session.ID {
		t.Fatalf("Клиент потерял закрытие точной сессии: %+v", event)
	}
}

// TestChatManagerAttributesInputLossToEligibleSource проверяет общий разрыв и счётчик источника.
func TestChatManagerAttributesInputLossToEligibleSource(t *testing.T) {
	// ctx и cancel ограничивают ожидание событий отказа.
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	// limits не вмещает даже минимальную запись во входной очереди.
	limits := core.DefaultChatLimits()
	limits.InputQueue.Bytes = 128
	// fixture предоставляет двух участников, manager и reader используют одну общую подписку.
	fixture, manager, _, reader := startChatControlTest(t, ctx, limits)
	// initial и err сохраняют последнюю позицию до входного отказа.
	initial, err := manager.Snapshot(ctx, fixture.stream.ID)
	chatTestOK(t, err)
	if err = reader.publish(ctx, "не-вмещается"); !errors.Is(err, core.ErrNotReady) {
		t.Fatalf("Запись сверх входного бюджета принята: %v", err)
	}
	// result содержит публичные счётчики двух источников после обработки отказа.
	result := waitChatTestSnapshot(t, ctx, manager, fixture.stream.ID, func(image core.ChatSnapshot) bool {
		// Предикат ждёт единственную потерю у минимального подходящего Source.
		return len(image.Statuses) == 2 && image.Statuses[0].SourceID == fixture.sources[0].ID && image.Statuses[0].Dropped == 1
	})
	if result.Statuses[1].SourceID != fixture.sources[1].ID || result.Statuses[1].Dropped != 0 || len(result.Messages) != 0 {
		t.Fatalf("Потеря затронула соседний источник или историю: %+v", result)
	}
	// page содержит общий разрыв и изменение состояния того же источника.
	page, err := manager.Events(ctx, fixture.stream.ID, initial.Cursor, 8)
	chatTestOK(t, err)
	if len(page.Events) != 2 || page.Events[0].Kind != core.ChatEventGap || page.Events[0].Lost != 1 || page.Events[1].Kind != core.ChatEventSourceStatus {
		t.Fatalf("События не подтверждают общий разрыв и отдельный счётчик: %+v", page)
	}
}

// chatRetryAdapter даёт тесту управляемый временный обрыв подтверждённой подписки.
type chatRetryAdapter struct {
	// chatTestAdapter предоставляет тип провайдера и результат обнаружения беседы.
	chatTestAdapter
	// disconnect завершает текущую попытку так, как если бы оборвалась сеть.
	disconnect chan struct{}
}

// Read подтверждает подписку и завершает её по сигналу обрыва или при отмене владельцем.
func (a *chatRetryAdapter) Read(ctx context.Context, request core.ChatReadRequest, sink core.ChatAdapterSink) error {
	// stopped закрывается только при фактическом возврате из Read.
	stopped := make(chan struct{})
	defer close(stopped)
	// err сообщает, что точное чтение уже отозвано до подтверждения.
	if err := sink.Status(ctx, request.Token, core.ChatConnected, core.ChatReasonNone); err != nil {
		return err
	}
	select {
	case a.readers <- chatTestReader{request: request, sink: sink, stopped: stopped}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	case <-a.disconnect:
		return core.ErrNotReady
	case <-ctx.Done():
		return ctx.Err()
	}
}

// TestChatManagerRetriesWithNewTokenAndStopsOnAuthRequired проверяет границы отдельных попыток.
func TestChatManagerRetriesWithNewTokenAndStopsOnAuthRequired(t *testing.T) {
	// ctx и cancel ограничивают повторы и ожидание сигналов синтетического транспорта.
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	// fixture предоставляет ровно одно подтверждаемое выполнение среди нескольких выходов.
	fixture := newChatManagerTestFixture(t, ctx)
	// adapter позволяет независимо учесть освобождение и повторное получение подписки.
	adapter := &chatRetryAdapter{chatTestAdapter: chatTestAdapter{readers: make(chan chatTestReader, 4)}, disconnect: make(chan struct{}, 1)}
	// limits задаёт короткие паузы и одно разрешённое подключение на окно.
	limits := core.DefaultChatLimits()
	limits.ReconnectAttempts, limits.ReconnectWindow = 1, 25*time.Millisecond
	limits.ReconnectMinBackoff, limits.ReconnectMaxBackoff = time.Millisecond, 5*time.Millisecond
	// manager содержит проверяемый владелец повторов, err сообщает об отказе запуска.
	manager, err := New(ctx, fixture.service, fixture.store, limits, []core.ChatAdapterPort{adapter})
	chatTestOK(t, err)
	t.Cleanup(func() {
		// Дожидается возврата синтетического транспорта до закрытия SQLite. closeCtx и stop ограничивают срок.
		closeCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		chatTestOK(t, manager.Close(closeCtx))
	})
	chatTestOK(t, fixture.outputs[0].Observe(ctx, core.OutputObservation{State: core.OutputPublishing}))
	// first содержит исходный токен и подтверждённое чтение.
	first := chatTestReceive(t, ctx, adapter.readers)
	// snapshot фиксирует позицию до сообщения адаптера о возможной потере платформы.
	snapshot, err := manager.Snapshot(ctx, fixture.stream.ID)
	chatTestOK(t, err)
	chatTestOK(t, first.sink.Gap(ctx, first.request.Token, core.ChatReasonUpstreamGap))
	// page содержит явное событие upstream-потери без добавления записи в историю сообщений.
	page, err := manager.Events(ctx, fixture.stream.ID, snapshot.Cursor, 16)
	chatTestOK(t, err)
	if len(page.Events) != 1 || page.Events[0].Kind != core.ChatEventGap || page.Events[0].Reason != core.ChatReasonUpstreamGap {
		t.Fatalf("Сигнал платформенной потери не доставлен: %+v", page)
	}
	adapter.disconnect <- struct{}{}
	chatTestReceive(t, ctx, first.stopped)
	// second должен принадлежать новой физической попытке того же действующего владельца.
	second := chatTestReceive(t, ctx, adapter.readers)
	if second.request.Token == first.request.Token || second.request.Key != first.request.Key {
		t.Fatal("Повторная попытка получила прежний токен либо другую беседу")
	}
	if err = first.publish(ctx, "старый-токен"); !errors.Is(err, core.ErrStale) {
		t.Fatalf("Сообщение от завершённой попытки принято: %v", err)
	}
	chatTestOK(t, second.sink.Status(ctx, second.request.Token, core.ChatAuthRequired, core.ChatReasonCredentials))
	chatTestReceive(t, ctx, second.stopped)
	waitChatTestSnapshot(t, ctx, manager, fixture.stream.ID, func(image core.ChatSnapshot) bool {
		// Предикат подтверждает конечный статус полномочий и отсутствие служебной записи в сообщениях.
		return chatTestHasState(image, fixture.sources[0].ID, core.ChatAuthRequired) && len(image.Messages) == 0
	})
}
