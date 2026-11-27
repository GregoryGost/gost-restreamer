package chat

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// chatLifecycleViewGate задерживает одно чтение Manager поверх работающей SQLite.
// Service продолжает пользоваться исходным Store, в том числе для команд управления.
type chatLifecycleViewGate struct {
	// UnitOfWork выполняет согласованное чтение после снятия барьера.
	core.UnitOfWork
	// armed включает барьер после начала платформенной работы.
	armed atomic.Bool
	// entered сообщает о задержанном чтении, release разрешает его продолжить.
	entered, release chan struct{}
}

// View при включённом барьере сообщает тесту о начале чтения и ждёт release или
// отмену ctx. Остальные запросы сразу выполняют read в настоящей транзакции.
func (g *chatLifecycleViewGate) View(ctx context.Context, read func(core.Repositories) error) error {
	if g.armed.CompareAndSwap(true, false) {
		close(g.entered)
		select {
		case <-g.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return g.UnitOfWork.View(ctx, read)
}

// chatLifecycleAdapter позволяет независимо наблюдать вход в Discover и Read,
// даже когда последующее подтверждение адаптера уже отвергается Manager.
type chatLifecycleAdapter struct {
	// chatRetryAdapter подтверждает подписки и моделирует управляемый временный обрыв.
	chatRetryAdapter
	// discover подменяет результат Discover, когда сценарию нужно отложить его возврат.
	discover func(context.Context, core.ChatSource, core.ChatCredentials) (core.ChatDiscovery, error)
	// readEntered получает токены физических вызовов до отправки статуса.
	readEntered chan core.ChatWorkerToken
}

// Discover выдаёт управляемый тестом результат для source и credentials либо
// передаёт запрос с ограничением ctx штатному синтетическому адаптеру.
func (a *chatLifecycleAdapter) Discover(ctx context.Context, source core.ChatSource, credentials core.ChatCredentials) (core.ChatDiscovery, error) {
	if a.discover != nil {
		return a.discover(ctx, source, credentials)
	}
	return a.chatRetryAdapter.Discover(ctx, source, credentials)
}

// Read отмечает начало физической попытки с токеном request до проверки callback.
// ctx ограничивает ожидание фикстуры, а sink получает подтверждение и сообщения.
func (a *chatLifecycleAdapter) Read(ctx context.Context, request core.ChatReadRequest, sink core.ChatAdapterSink) error {
	select {
	case a.readEntered <- request.Token:
	default:
	}
	return a.chatRetryAdapter.Read(ctx, request, sink)
}

// newChatLifecycleAdapter создаёт фикстуру с отдельными сигналами физических
// попыток, подтверждённых подписок и временного обрыва.
func newChatLifecycleAdapter() *chatLifecycleAdapter {
	return &chatLifecycleAdapter{
		chatRetryAdapter: chatRetryAdapter{
			chatTestAdapter: chatTestAdapter{readers: make(chan chatTestReader, 16)},
			disconnect:      make(chan struct{}, 1),
		},
		readEntered: make(chan core.ChatWorkerToken, 16),
	}
}

// startChatLifecycleManager запускает Manager с временным store и адаптером adapter.
// t дожидается его остановки перед освобождением SQLite, даже при ошибке сценария.
func startChatLifecycleManager(t *testing.T, ctx context.Context, fixture chatManagerTestFixture, store core.UnitOfWork, limits core.ChatLimits, adapter core.ChatAdapterPort) *Manager {
	t.Helper()
	// manager наблюдает за медиавыходами fixture, err сообщает об отказе запуска.
	manager, err := New(ctx, fixture.service, store, limits, []core.ChatAdapterPort{adapter})
	chatTestOK(t, err)
	t.Cleanup(func() {
		// Очистка ограничивает ожидание фоновых работ отдельным контекстом.
		// closeCtx и cancel задают срок, независимый от контекста проверяемой операции.
		closeCtx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		chatTestOK(t, manager.Close(closeCtx))
	})
	return manager
}

// TestChatManagerDoesNotRestartRevokedNetworkWhileViewWaits проверяет отзыв последнего
// точного выхода во время чтения SQLite. Начатый транспорт возвращает временный отказ,
// после которого ни обнаружение, ни чтение больше не должны обращаться к адаптеру.
func TestChatManagerDoesNotRestartRevokedNetworkWhileViewWaits(t *testing.T) {
	// stage выбирает физическую операцию, которая уже выполняется в момент отзыва.
	for _, stage := range []string{"Discover", "Read"} {
		t.Run(stage, func(t *testing.T) {
			// Сценарий сравнивает физические входы адаптера до и после точного отзыва.
			// ctx и cancel ограничивают фикстуру и освобождают её барьеры при отказе.
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			// fixture содержит наблюдателя одного пригодного выхода, gate задерживает только Manager.View.
			fixture := newChatManagerTestFixture(t, ctx)
			gate := &chatLifecycleViewGate{UnitOfWork: fixture.store, entered: make(chan struct{}), release: make(chan struct{})}
			// adapter сообщает о входе в сеть до проверки статуса. discoverEntered учитывает физические обнаружения.
			adapter := newChatLifecycleAdapter()
			discoverEntered := make(chan struct{}, 16)
			// finishDiscovery разрешает первой управляемой попытке Discover вернуть временный отказ.
			finishDiscovery := make(chan struct{})
			// Сигнализирует о каждом Discover и удерживает его до отзыва в сценарии обнаружения.
			adapter.discover = func(ctx context.Context, source core.ChatSource, credentials core.ChatCredentials) (core.ChatDiscovery, error) {
				select {
				case discoverEntered <- struct{}{}:
				default:
				}
				if stage == "Read" {
					return adapter.chatRetryAdapter.Discover(ctx, source, credentials)
				}
				select {
				case <-finishDiscovery:
					return core.ChatDiscovery{}, core.ErrNotReady
				case <-ctx.Done():
					return core.ChatDiscovery{}, ctx.Err()
				}
			}
			// backoff одинаков для всех попыток, чтобы проверка охватывала несколько сроков повтора.
			const backoff = 10 * time.Millisecond
			// limits убирает случайный разброс задержки в этой фикстуре.
			limits := core.DefaultChatLimits()
			limits.ReconnectMinBackoff, limits.ReconnectMaxBackoff = backoff, backoff
			// manager связывает платформенную работу и точный отзыв, output хранит её единственное доказательство.
			manager := startChatLifecycleManager(t, ctx, fixture, gate, limits, adapter)
			output := fixture.outputs[0]
			chatTestOK(t, output.Observe(ctx, core.OutputObservation{State: core.OutputPublishing}))
			chatTestReceive(t, ctx, discoverEntered)
			if stage == "Read" {
				chatTestReceive(t, ctx, adapter.readEntered)
				chatTestReceive(t, ctx, adapter.readers)
			}
			gate.armed.Store(true)
			manager.requestReconcile()
			chatTestReceive(t, ctx, gate.entered)
			chatTestOK(t, output.Close(ctx))
			chatTestReceive(t, ctx, output.Done())
			close(finishDiscovery)
			if stage == "Read" {
				adapter.disconnect <- struct{}{}
			}
			// retryWindow охватывает несколько разрешённых интервалов повтора при всё ещё задержанном View.
			retryWindow := time.NewTimer(10 * backoff)
			defer retryWindow.Stop()
			select {
			case <-discoverEntered:
				t.Fatal("Discover вызван после отзыва единственного выхода при задержанной сверке")
			case <-adapter.readEntered:
				t.Fatal("Read вызван после отзыва единственного выхода при задержанной сверке")
			case <-retryWindow.C:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			close(gate.release)
			chatTestOK(t, manager.Close(ctx))
			chatTestReceive(t, ctx, manager.Done())
			select {
			case <-discoverEntered:
				t.Fatal("Discover вызван после отзыва единственного выхода при завершении сверки")
			case <-adapter.readEntered:
				t.Fatal("Read вызван после отзыва единственного выхода при завершении сверки")
			default:
			}
		})
	}
}

// TestChatManagerCoalescedOutputHandoffKeepsReader проверяет, что второе
// доказательство того же Destination сохраняет общий Read, если оно возникло
// до отзыва первого, даже когда Manager применяет оба события за одну сверку.
func TestChatManagerCoalescedOutputHandoffKeepsReader(t *testing.T) {
	// ctx и cancel ограничивают ожидания обеих фаз и отменяют задержанное чтение при сбое.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// fixture имеет два выхода первого Destination и третий независимый Destination.
	fixture := newChatManagerTestFixture(t, ctx)
	// gate задерживает Manager между оперативным снимком и чтением SQLite.
	gate := &chatLifecycleViewGate{UnitOfWork: fixture.store, entered: make(chan struct{}), release: make(chan struct{})}
	// adapter даёт всем источникам один фактический канал.
	adapter := &chatTestAdapter{readers: make(chan chatTestReader, 16)}
	// manager должен сохранить физический Read при передаче доказательства тому же источнику.
	manager := startChatLifecycleManager(t, ctx, fixture, gate, core.DefaultChatLimits(), adapter)
	// first и successor относятся к одному Destination. marker относится к независимому Destination.
	first, successor, marker := fixture.outputs[0], fixture.outputs[1], fixture.outputs[2]
	chatTestOK(t, first.Observe(ctx, core.OutputObservation{State: core.OutputPublishing}))
	// reader получает первоначальный токен и остаётся действующим после сверки.
	reader := chatTestReceive(t, ctx, adapter.readers)
	gate.armed.Store(true)
	manager.requestReconcile()
	chatTestReceive(t, ctx, gate.entered)
	chatTestOK(t, successor.Observe(ctx, core.OutputObservation{State: core.OutputPublishing}))
	chatTestOK(t, first.Close(ctx))
	chatTestOK(t, marker.Observe(ctx, core.OutputObservation{State: core.OutputPublishing}))
	close(gate.release)
	waitChatTestSnapshot(t, ctx, manager, fixture.stream.ID, func(image core.ChatSnapshot) bool {
		// Новый сосед подтверждает, что сверка уже обработала события обеих замен.
		return chatTestHasState(image, fixture.sources[1].ID, core.ChatConnected)
	})
	if err := reader.publish(ctx, "after-output-handoff"); err != nil {
		t.Fatalf("Чтение с исходным токеном перестало принимать сообщения после передачи допуска: %v", err)
	}
	// image показывает, какой источник сохранил допуск на момент публикации сообщения.
	image := waitChatTestSnapshot(t, ctx, manager, fixture.stream.ID, func(image core.ChatSnapshot) bool {
		// Предикат ждёт публикацию сообщения, принятого с исходным токеном.
		return len(image.Messages) > 0
	})
	if len(image.Messages) != 1 || image.Messages[0].ProviderMessageID != "after-output-handoff" || image.Messages[0].ChatSourceID != fixture.sources[0].ID {
		t.Fatalf("Сообщение после передачи допуска получило другого владельца: %+v", image.Messages)
	}
	select {
	case <-reader.stopped:
		t.Fatal("Исходное физическое чтение закрыто при непрерывном допуске Destination")
	case <-adapter.readers:
		t.Fatal("Новый выход того же Destination запустил лишнее физическое чтение")
	default:
	}
}

// waitForMutationWithoutChoices отслеживает публичное продолжение cursor, пока
// CAS заблокирован. t сообщает о преждевременной выдаче результатов источника sourceID.
func waitForMutationWithoutChoices(t *testing.T, ctx context.Context, manager *Manager, streamID, sourceID core.ID, cursor core.ChatCursor) {
	t.Helper()
	// window ограничивает проверку отсутствия событий после подтверждённого выхода из Discover.
	window := time.NewTimer(100 * time.Millisecond)
	defer window.Stop()
	for {
		// page хранит события даже при кратковременном появлении неверного статуса.
		// err сообщает об отказе чтения текущей эпохи.
		page, err := manager.Events(ctx, streamID, cursor, 32)
		chatTestOK(t, err)
		if page.ResyncRequired {
			t.Fatal("Курсор сменился во время редактирования источника")
		}
		if slices.ContainsFunc(page.Events, func(event core.ChatEvent) bool {
			// Предикат выделяет только статус выдачи обнаруженных вариантов изменяемого источника.
			return event.Status != nil && event.Status.SourceID == sourceID && event.Status.Reason == core.ChatReasonSelectionRequired
		}) {
			t.Fatal("Менеджер опубликовал результат Discover до подтверждения исхода CAS")
		}
		cursor = page.Cursor
		select {
		case <-page.Changed:
		case <-window.C:
			return
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
}

// TestChatManagerHoldsDiscoveryChoicesUntilSourceMutationFinishes проверяет
// задержку результата Discover во время CAS и его применение после отказа CAS
// или совместимого с чтением переименования.
func TestChatManagerHoldsDiscoveryChoicesUntilSourceMutationFinishes(t *testing.T) {
	// operation задаёт исход штатного CAS источника после возврата из Discover.
	for _, operation := range []string{"Отказ CAS", "Переименование"} {
		t.Run(operation, func(t *testing.T) {
			// Сценарий сверяет публичную историю, ревизию конфигурации и продолжение обнаружения.
			// ctx и cancel ограничивают изменения и освобождают тестовые горутины.
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			// fixture даёт запись источника и единственное публикующее выполнение.
			fixture := newChatManagerTestFixture(t, ctx)
			// source содержит исходную ревизию. output должен допустить ровно этот источник.
			source := fixture.sources[0]
			output := fixture.outputs[0]
			// adapter управляет одним обнаружением. discoverEntered отмечает вход, discoverRelease
			// разрешает возврат двух вариантов, discoverReturned отмечает отдачу результата платформой.
			adapter := newChatLifecycleAdapter()
			discoverEntered, discoverRelease, discoverReturned := make(chan struct{}), make(chan struct{}), make(chan struct{})
			// attempts считает обращения к адаптеру, чтобы проверить сохранение результата при CAS.
			var attempts atomic.Int32
			// Callback удерживает первый ответ, а другие вызовы остаются достижимыми для диагностики.
			adapter.discover = func(ctx context.Context, _ core.ChatSource, _ core.ChatCredentials) (core.ChatDiscovery, error) {
				if attempts.Add(1) == 1 {
					close(discoverEntered)
					select {
					case <-discoverRelease:
					case <-ctx.Done():
						return core.ChatDiscovery{}, ctx.Err()
					}
					defer close(discoverReturned)
				}
				return core.ChatDiscovery{State: core.ChatWaitingChat, Broadcasts: []core.ChatBroadcast{
					{VideoID: "video-a", ChannelID: "chat-a", Title: "Первый эфир"},
					{VideoID: "video-b", ChannelID: "chat-b", Title: "Второй эфир"},
				}}, nil
			}
			// manager публикует события и подтверждает CAS через Service.
			manager := startChatLifecycleManager(t, ctx, fixture, fixture.store, core.DefaultChatLimits(), adapter)
			chatTestOK(t, output.Observe(ctx, core.OutputObservation{State: core.OutputPublishing}))
			chatTestReceive(t, ctx, discoverEntered)
			// before фиксирует позицию после начала Discover и до задержанного результата.
			before, err := manager.Snapshot(ctx, fixture.stream.ID)
			chatTestOK(t, err)

			// mutationEntered подтверждает закрытие допуска Manager. mutationRelease разрешает запись.
			// mutationDone получает итог операции независимо от того, продолжает ли основной тест ждать.
			mutationEntered, mutationRelease := make(chan struct{}), make(chan struct{})
			mutationDone := make(chan error, 1)
			// Горутина запускает штатную транзакцию конфигурации и удерживает только тестовый барьер.
			go func() {
				// err передаёт результат CAS через границу тестовой горутины.
				_, err := manager.MutateSource(ctx, source.ID, func(ctx context.Context) (core.ChatSource, error) {
					// Callback ждёт проверки истории под закрытым допуском и затем вызывает Service.
					close(mutationEntered)
					select {
					case <-mutationRelease:
					case <-ctx.Done():
						return core.ChatSource{}, ctx.Err()
					}
					// edited меняет только имя. revision подставляет устаревшее ожидание для отказа CAS.
					edited, revision := source, source.Revision
					edited.Name = "Новое имя"
					if operation == "Отказ CAS" {
						revision++
					}
					return fixture.service.UpdateChatSource(ctx, edited, revision)
				})
				mutationDone <- err
			}()
			chatTestReceive(t, ctx, mutationEntered)
			close(discoverRelease)
			chatTestReceive(t, ctx, discoverReturned)
			waitForMutationWithoutChoices(t, ctx, manager, fixture.stream.ID, source.ID, before.Cursor)
			close(mutationRelease)
			err = chatTestReceive(t, ctx, mutationDone)
			// revision — ожидаемая конфигурационная ревизия в возобновлённом результате.
			revision := source.Revision
			if operation == "Отказ CAS" {
				if !errors.Is(err, core.ErrConflict) {
					t.Fatalf("CAS с несовпадающей ревизией вернул %v", err)
				}
			} else {
				chatTestOK(t, err)
				revision++
			}

			// after удостоверяет, что прежняя сессия получила результат после открытия допуска.
			after := waitChatTestSnapshot(t, ctx, manager, fixture.stream.ID, func(image core.ChatSnapshot) bool {
				// Предикат ждёт публичное ожидание выбора эфира.
				return image.Session != nil && chatTestHasState(image, source.ID, core.ChatWaitingChat)
			})
			// choices связывает сохранённые варианты с действующей ревизией после CAS.
			choices, err := manager.BroadcastChoices(ctx, *after.Session, source.ID)
			chatTestOK(t, err)
			if attempts.Load() != 1 || choices.SourceRevision != revision || len(choices.Broadcasts) != 2 ||
				choices.Broadcasts[0].VideoID != "video-a" || choices.Broadcasts[1].VideoID != "video-b" {
				t.Fatalf("Результат Discover потерян или не связан с действующей ревизией %d: %+v, попыток %d", revision, choices, attempts.Load())
			}
			chatTestOK(t, manager.SelectBroadcast(ctx, core.ChatBroadcastSelection{Session: *after.Session, SourceID: source.ID, SourceRevision: revision, DiscoveryGeneration: choices.DiscoveryGeneration, VideoID: "video-b"}))
			// reader подтверждает фактический чат выбранного варианта.
			reader := chatTestReceive(t, ctx, adapter.readers)
			if reader.request.Key.ChannelID != "chat-b" {
				t.Fatalf("Read подключился к %q вместо выбранного чата", reader.request.Key.ChannelID)
			}
		})
	}
}

// TestChatManagerQueuedMessageSurvivesTransientReadReturn проверяет, что принятая
// запись сохраняет исходное право публикации при смене физического токена того же читателя.
func TestChatManagerQueuedMessageSurvivesTransientReadReturn(t *testing.T) {
	// ctx и cancel ограничивают подключение, повтор и публикацию общего пакета.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// fixture связывает один публикующий выход и источник с временной SQLite.
	fixture := newChatManagerTestFixture(t, ctx)
	// adapter моделирует управляемый обрыв без сетевого доступа.
	adapter := &chatRetryAdapter{chatTestAdapter: chatTestAdapter{readers: make(chan chatTestReader, 8)}, disconnect: make(chan struct{}, 1)}
	// limits откладывает flush одиночной записи до поступления сообщений новой попытки.
	limits := core.DefaultChatLimits()
	limits.FlushInterval, limits.FlushMessages = time.Minute, 2
	limits.ReconnectMinBackoff, limits.ReconnectMaxBackoff = time.Millisecond, time.Millisecond
	// manager позволяет наблюдать очередь через согласованный публичный снимок.
	manager := startChatLifecycleManager(t, ctx, fixture, fixture.store, limits, adapter)
	chatTestOK(t, fixture.outputs[0].Observe(ctx, core.OutputObservation{State: core.OutputPublishing}))
	// first получил первую физическую подписку того же логического читателя.
	first := chatTestReceive(t, ctx, adapter.readers)
	chatTestOK(t, first.publish(ctx, "queued-before-disconnect"))
	adapter.disconnect <- struct{}{}
	chatTestReceive(t, ctx, first.stopped)
	// second подтверждает восстановление подписки, включая замену её физического токена.
	second := chatTestReceive(t, ctx, adapter.readers)
	if second.request.Token == first.request.Token || second.request.Key != first.request.Key {
		t.Fatalf("Восстановленная подписка должна получить новый токен того же чата: %+v", second.request.Key)
	}
	// err должен отразить отзыв именно прежнего callback, даже если старое сообщение остаётся в очереди.
	if err := first.publish(ctx, "late-old-token"); !errors.Is(err, core.ErrStale) {
		t.Fatalf("Поздняя доставка завершённой подписки принята: %v", err)
	}
	chatTestOK(t, second.publish(ctx, "accepted-after-retry"))
	// image содержит пакет из обоих физических подключений в порядке приёма.
	image := waitChatTestSnapshot(t, ctx, manager, fixture.stream.ID, func(image core.ChatSnapshot) bool {
		// Предикат ждёт видимость сообщения новой попытки после обязательной пакетной публикации.
		return slices.ContainsFunc(image.Messages, func(message core.ChatMessage) bool {
			// message относится к записи, принятой уже после восстановления чтения.
			return message.ProviderMessageID == "accepted-after-retry"
		})
	})
	// expected задаёт очередь принятых синтетических идентификаторов в исходном порядке.
	expected := []string{"queued-before-disconnect", "accepted-after-retry"}
	if len(image.Messages) != len(expected) {
		t.Fatalf("Смена физического токена изменила размер пакета: %+v", image.Messages)
	}
	// index задаёт позицию приёма, id содержит идентификатор записи адаптера.
	for index, id := range expected {
		// message содержит публичную запись для проверки очередности и прежнего владельца.
		message := image.Messages[index]
		if message.ProviderMessageID != id || message.Seq != uint64(index+1) || message.ChatSourceID != fixture.sources[0].ID {
			t.Fatalf("Некорректная запись пакета на позиции %d: %+v", index, message)
		}
	}
}
