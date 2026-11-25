package chat

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"testing/synctest"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/GregoryGost/gost-restreamer/internal/chatwire"
	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// chatTestClockStore предоставляет снимок одного потока для проверок с виртуальными часами.
type chatTestClockStore struct {
	// UnitOfWork оставляет запись недоступной для проверки генератора.
	core.UnitOfWork
	// ConfigRepository оставляет операции вне чтения тестового потока недоступными.
	core.ConfigRepository
	// RuntimeRepository оставляет операции вне чтения Session и квитанции её запуска недоступными.
	core.RuntimeRepository
	// ChatRepository оставляет операции вне чтения списка источников недоступными.
	core.ChatRepository
	// ArtifactRepository оставляет доступными только сведения об отсутствии подготовки.
	core.ArtifactRepository
	// authority задаёт подлинный реестр допуска публикации без ожидания SQLite.
	authority *core.RuntimeAuthority
	// running и receipt содержат согласованный результат запуска, если он сохранён к началу сценария.
	running *core.Session
	receipt core.CommandReceipt
}

// RuntimeAuthority возвращает тот же реестр каждому потребителю фикстуры.
func (s *chatTestClockStore) RuntimeAuthority() *core.RuntimeAuthority { return s.authority }

// View читает одну неизменяемую версию конфигурации в переданной функции, пока ctx действует.
func (s *chatTestClockStore) View(ctx context.Context, read func(core.Repositories) error) error {
	// err сообщает об отмене чтения до доступа к фикстуре.
	if err := ctx.Err(); err != nil {
		return err
	}
	return read(core.Repositories{Config: s, Runtime: s, Chat: s, Artifacts: s})
}

// Stream возвращает сконфигурированный поток по его точному id.
func (*chatTestClockStore) Stream(_ context.Context, id core.ID) (core.Stream, error) {
	if id != "virtual-test-feed" {
		return core.Stream{}, core.ErrNotFound
	}
	return core.Stream{Record: core.Record{ID: id}}, nil
}

// ActiveSession возвращает сохранённый RUNNING только для потока id.
func (s *chatTestClockStore) ActiveSession(_ context.Context, id core.ID) (core.Session, error) {
	if s.running == nil || id != s.running.StreamID {
		return core.Session{}, core.ErrNotFound
	}
	return *s.running, nil
}

// Session возвращает тот же сохранённый запуск по идентификатору сессии id для сверки топологии.
func (s *chatTestClockStore) Session(_ context.Context, id core.ID) (core.Session, error) {
	if s.running == nil || id != s.running.ID {
		return core.Session{}, core.ErrNotFound
	}
	return *s.running, nil
}

// ReceiptByID возвращает успешную квитанцию команды id для открытия наблюдателя запуска.
func (s *chatTestClockStore) ReceiptByID(_ context.Context, id core.ID) (core.CommandReceipt, error) {
	if id != s.receipt.ID || s.running == nil {
		return core.CommandReceipt{}, core.ErrNotFound
	}
	return s.receipt, nil
}

// Generations подтверждает отсутствие подготовки, которая могла бы помешать запуску.
func (*chatTestClockStore) Generations(context.Context, core.ListQuery) ([]core.FallbackGeneration, error) {
	return nil, nil
}

// ChatSources возвращает пустой список настроенных источников.
func (*chatTestClockStore) ChatSources(context.Context, core.ChatSourceQuery) ([]core.ChatSource, error) {
	return nil, nil
}

// newChatTestClockManager запускает Manager без сетевых адаптеров с заданными limits.
// store остаётся неизменным после настройки реестра. t закрывает Manager и реестр при выходе.
func newChatTestClockManager(t *testing.T, store *chatTestClockStore, limits core.ChatLimits) (*Manager, *core.Service) {
	t.Helper()
	// ctx ограничивает настройку и фоновые задачи Manager.
	ctx := t.Context()
	// authority обеспечивает реальные допуски, err сообщает об ошибке настройки.
	authority, err := core.NewRuntimeAuthority("virtual-chat", core.ObservationLimits{
		MaxSessions: 2, MaxGroups: 2, MaxOutputs: 2, MaxFFmpegWorkers: 2,
		MaxIdentifierBytes: 1024, MaxErrorBytes: 4096, MaxBytes: 1 << 20,
	}, core.OutputExecutionLimits{MaxReservations: 2, MaxIdentifierBytes: 1024, MaxBytes: 1 << 20})
	chatTestOK(t, err)
	t.Cleanup(authority.Close)
	store.authority = authority
	// service проверяет допуски по согласованным записям фикстуры.
	service, err := core.NewService(store, "virtual-chat")
	chatTestOK(t, err)
	chatTestOK(t, service.ConfigureTelemetry(ctx, core.TelemetryLimits{
		MediaStaleAfter: time.Second, MaxGPUDevices: 1, MaxGPUUUIDBytes: 64, MaxGPUBytes: 4096,
		Events: core.EventLimits{MaxEvents: 64, MaxBytes: 65536, MaxEventBytes: 8192, MaxPageSize: 64},
	}))
	limits.FlushMessages = 1
	// manager создаёт настоящие ленты и применяет смену топологии по сигналу реестра.
	manager, err := New(ctx, service, store, limits, nil)
	chatTestOK(t, err)
	t.Cleanup(func() {
		// Закрывает Manager и ждёт выхода его фоновых задач, даже если проверка прервалась.
		// cleanup и cancel ограничивают ожидание независимо от срока запроса.
		cleanup, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		chatTestOK(t, manager.Close(cleanup))
	})
	return manager, service
}

// TestChatManagerTestFeedCadenceAndStop проверяет первое сообщение без хода часов,
// единственный неизменённый таймер при повторном старте, остановку и продолжение истории.
func TestChatManagerTestFeedCadenceAndStop(t *testing.T) {
	// Внутренний t владеет виртуальным временем и всеми фоновыми задачами Manager.
	synctest.Test(t, func(t *testing.T) {
		// ctx завершает запросы вместе со сценарием.
		ctx := t.Context()
		// manager ведёт предэфирную ленту, service позволяет проверить отзыв её допуска при закрытии.
		manager, service := newChatTestClockManager(t, &chatTestClockStore{}, core.DefaultChatLimits())
		// streamID совпадает с единственным сохранённым потоком фикстуры.
		const streamID core.ID = "virtual-test-feed"
		// startedAt фиксирует исходную точку виртуальных часов, effective содержит принятый интервал.
		// err сообщает об отказе запуска генератора.
		startedAt := time.Now()
		effective, err := manager.StartTest(ctx, streamID, 300*time.Millisecond)
		chatTestOK(t, err)
		if effective != 300*time.Millisecond {
			t.Fatalf("Принятый интервал равен %v", effective)
		}
		synctest.Wait()
		// snapshot фиксирует сообщение до первого срабатывания таймера.
		snapshot := requireChatTestHistoryLength(t, manager, streamID, 1)
		if snapshot.Session != nil || snapshot.Messages[0].Seq != 1 || !snapshot.Messages[0].ReceivedAt.Equal(startedAt) {
			t.Fatalf("Первое сообщение не опубликовано сразу в тестовой ленте: %+v", snapshot)
		}
		synctest.Sleep(100 * time.Millisecond)
		effective, err = manager.StartTest(ctx, streamID, 100*time.Millisecond)
		chatTestOK(t, err)
		if effective != 300*time.Millisecond {
			t.Fatalf("Повторный старт заменил действующий интервал: %v", effective)
		}
		synctest.Sleep(199 * time.Millisecond)
		synctest.Wait()
		requireChatTestHistoryLength(t, manager, streamID, 1)
		synctest.Sleep(time.Millisecond)
		synctest.Wait()
		snapshot = requireChatTestHistoryLength(t, manager, streamID, 2)
		if snapshot.Messages[1].Seq != 2 || !snapshot.Messages[1].ReceivedAt.Equal(startedAt.Add(300*time.Millisecond)) {
			t.Fatalf("Повторный старт сдвинул публикацию или изменил порядок: %+v", snapshot.Messages)
		}

		chatTestOK(t, manager.StopTest(ctx, streamID))
		synctest.Sleep(time.Second)
		synctest.Wait()
		// stopped подтверждает, что после Stop история и позиция не сдвинулись.
		stopped := requireChatTestHistoryLength(t, manager, streamID, 2)
		if stopped.Cursor != snapshot.Cursor {
			t.Fatal("После остановки изменился курсор тестовой ленты")
		}
		effective, err = manager.StartTest(ctx, streamID, 100*time.Millisecond)
		chatTestOK(t, err)
		if effective != 100*time.Millisecond {
			t.Fatalf("Новый запуск не принял новый интервал: %v", effective)
		}
		synctest.Wait()
		snapshot = requireChatTestHistoryLength(t, manager, streamID, 3)
		if snapshot.Messages[2].Seq != 3 {
			t.Fatalf("Новый запуск не продолжил порядок: %+v", snapshot.Messages)
		}
		synctest.Sleep(100 * time.Millisecond)
		synctest.Wait()
		snapshot = requireChatTestHistoryLength(t, manager, streamID, 4)
		if snapshot.Messages[3].Seq != 4 || snapshot.Messages[3].ReceivedAt.Sub(snapshot.Messages[2].ReceivedAt) != 100*time.Millisecond {
			t.Fatalf("Новый запуск не применил меньший интервал: %+v", snapshot.Messages)
		}
		// admission наблюдает точное завершение допуска текущей ленты после Close.
		admission, err := service.AdmitChatTest(ctx, streamID)
		chatTestOK(t, err)
		chatTestOK(t, manager.Close(ctx))
		select {
		case <-admission.Done():
		default:
			t.Fatal("Закрытие Manager оставило допуск тестовой ленты")
		}
	})
}

// TestChatManagerTestFeedSampleCycle проверяет весь перемешанный набор через публичный снимок Manager.
// Таймер идёт на виртуальных часах t. Порядок реплик зависит от перемешивания.
func TestChatManagerTestFeedSampleCycle(t *testing.T) {
	// Внутренний t управляет виртуальными часами и завершением фоновых задач.
	synctest.Test(t, func(t *testing.T) {
		// streamID адресует единственный сохранённый поток фикстуры.
		const streamID core.ID = "virtual-test-feed"
		// interval задаёт шаг виртуальных часов между публикациями.
		// cycleSize задаёт число реплик одного полного прохода генератора.
		const interval = 100 * time.Millisecond
		const cycleSize = 12
		// manager публикует тестовые записи без настроенных аккаунтов; err сообщает об отказе запуска.
		manager, _ := newChatTestClockManager(t, &chatTestClockStore{}, core.DefaultChatLimits())
		_, err := manager.StartTest(t.Context(), streamID, interval)
		chatTestOK(t, err)
		synctest.Wait()
		synctest.Sleep((cycleSize - 1) * interval)
		synctest.Wait()
		// snapshot содержит один полный проход в фактическом порядке поступления.
		snapshot := requireChatTestHistoryLength(t, manager, streamID, cycleSize)
		if snapshot.Session != nil || len(snapshot.Sources) != 0 {
			t.Fatalf("Тестовые площадки изменили конфигурацию ленты: %+v", snapshot)
		}
		// platforms, authors и texts собирают отличающиеся площадки, имена и реплики.
		platforms := make(map[core.ChatProvider]bool)
		authors := make(map[string]bool)
		texts := make(map[string]bool)
		// short и long отмечают короткую реакцию и развёрнутую реплику.
		// emoji и badge отмечают структурированный эмодзи и метку автора.
		var short, long, emoji, badge bool
		// index задаёт ожидаемую позицию, message содержит опубликованную запись.
		for index, message := range snapshot.Messages {
			chatTestOK(t, message.Validate())
			if message.Seq != uint64(index+1) || message.Provider != core.ChatTest ||
				message.TestProvider.Validate() != nil || strings.TrimSpace(message.Author.DisplayName) == "" ||
				strings.TrimSpace(message.Text) == "" || texts[message.Text] {
				t.Fatalf("Неверная или повторная тестовая реплика: %+v", message)
			}
			platforms[message.TestProvider] = true
			authors[message.Author.DisplayName] = true
			texts[message.Text] = true
			// length сравнивает видимую длину реплик без учёта числа байтов UTF-8.
			length := utf8.RuneCountInString(message.Text)
			short = short || length <= 15
			long = long || length >= 80
			if len(message.Parts) > 0 {
				// rendered собирает фрагменты в порядке, в котором их отображает лента.
				var rendered strings.Builder
				// part содержит очередной текстовый фрагмент либо эмодзи.
				for _, part := range message.Parts {
					rendered.WriteString(part.Text)
					emoji = emoji || part.Kind == core.ChatPartEmote && strings.ContainsFunc(part.Text, func(r rune) bool {
						// Предикат ищет в обозначении эмодзи r из категории символов Unicode.
						return unicode.Is(unicode.So, r)
					})
				}
				if rendered.String() != message.Text {
					t.Fatalf("Фрагменты дали текст %q, ожидается %q", rendered.String(), message.Text)
				}
			}
			// tag содержит очередную видимую метку автора.
			for _, tag := range message.Badges {
				badge = badge || strings.TrimSpace(tag.Name) != ""
			}
		}
		if !platforms[core.ChatYouTube] || !platforms[core.ChatTwitch] || !platforms[core.ChatVKVideoLive] ||
			len(authors) < 3 || !short || !long || !emoji || !badge {
			t.Fatalf("Тестовой ленте не хватает разнообразия: площадки %v, авторы %v, короткий текст %t, длинный текст %t, эмодзи %t, метка %t",
				platforms, authors, short, long, emoji, badge)
		}
		synctest.Sleep(interval)
		synctest.Wait()
		snapshot = requireChatTestHistoryLength(t, manager, streamID, cycleSize+1)
		if snapshot.Messages[cycleSize].Seq != cycleSize+1 || !texts[snapshot.Messages[cycleSize].Text] {
			t.Fatalf("После первого прохода генератор не начал следующий: %+v", snapshot.Messages[cycleSize])
		}
	})
}

// TestChatManagerTestFeedFitsHistoryBudget проверяет, что тесный байтовый предел истории
// оставляет последнюю реплику с площадкой и порядковым номером на каждом тике.
func TestChatManagerTestFeedFitsHistoryBudget(t *testing.T) {
	// Внутренний t задаёт виртуальные тики и ограничивает срок жизни Manager.
	synctest.Test(t, func(t *testing.T) {
		// streamID адресует единственный поток, interval задаёт период публикации.
		const streamID core.ID = "virtual-test-feed"
		const interval = 100 * time.Millisecond
		// limits задаёт место для компактной записи, включая JSON-оболочку и платформу.
		limits := core.DefaultChatLimits()
		limits.History.Bytes = 300
		// manager запускает генератор с ограниченной историей; err сообщает об отказе первой публикации.
		manager, _ := newChatTestClockManager(t, &chatTestClockStore{}, limits)
		_, err := manager.StartTest(t.Context(), streamID, interval)
		chatTestOK(t, err)
		// seq фиксирует непрерывный порядок на протяжении полного перемешанного прохода.
		for seq := uint64(1); seq <= 12; seq++ {
			synctest.Wait()
			// snapshot содержит удержанную запись, latest содержит её публичные поля.
			snapshot := requireChatTestHistoryLength(t, manager, streamID, 1)
			latest := snapshot.Messages[0]
			// encoded включает полные поля проводного формата, err сообщает об отказе сериализации.
			encoded, err := json.Marshal(chatwire.Message(latest))
			chatTestOK(t, err)
			if latest.Seq != seq || latest.Provider != core.ChatTest || latest.TestProvider.Validate() != nil ||
				strings.TrimSpace(latest.Text) == "" || uint64(len(encoded)) > limits.History.Bytes {
				t.Fatalf("При тике %d в историю попала неверная запись из %d байт: %s", seq, len(encoded), encoded)
			}
			synctest.Sleep(interval)
		}
	})
}

// TestChatManagerTestFeedFitsEventBudgets проверяет доставку каждого тика быстрому
// подписчику и чтение того же кадра с прежнего курсора при тесных транспортных лимитах.
func TestChatManagerTestFeedFitsEventBudgets(t *testing.T) {
	// limitsByResource задаёт независимые ограничения. Каждое вмещает короткую запись,
	// но одно тяжёлое сообщение превысило бы предел даже при пустой очереди или журнале.
	limitsByResource := map[string]core.ChatLimits{}
	// frame, queue и journal ограничивают целый кадр, элемент очереди и запись для повторной доставки.
	frame, queue, journal := core.DefaultChatLimits(), core.DefaultChatLimits(), core.DefaultChatLimits()
	frame.SSEFrameBytes, queue.SSEQueue.Bytes, journal.Replay.Bytes = 500, 650, 450
	limitsByResource["кадр"] = frame
	limitsByResource["очередь"] = queue
	limitsByResource["журнал"] = journal
	// name называет проверяемый ресурс, limits передаёт только один тесный бюджет.
	for name, limits := range limitsByResource {
		// Сценарий вводит один предел и проверяет доставку без отставания подписчика.
		t.Run(name, func(t *testing.T) {
			// Внутренний t выпускает по одному сообщению между чтениями подписки.
			synctest.Test(t, func(t *testing.T) {
				// ctx ограничивает операции сценарием. streamID и interval задают тестовую ленту и период.
				ctx := t.Context()
				const streamID core.ID = "virtual-test-feed"
				const interval = 100 * time.Millisecond
				// manager использует ограничение одного ресурса, err сообщает об отказе старта.
				manager, _ := newChatTestClockManager(t, &chatTestClockStore{}, limits)
				_, err := manager.StartTest(ctx, streamID, interval)
				chatTestOK(t, err)
				// cursor запоминает подтверждённую клиентом позицию. subscription получает новые события.
				cursor := requireChatTestHistoryLength(t, manager, streamID, 1).Cursor
				subscription, err := manager.Subscribe(ctx, streamID, cursor)
				chatTestOK(t, err)
				defer subscription.Close()
				// seq нумерует остаток сообщений первого прохода и первое сообщение следующего.
				for seq := uint64(2); seq <= 13; seq++ {
					synctest.Sleep(interval)
					synctest.Wait()
					// live содержит новый кадр подписки, err сообщает об отказе чтения.
					live, err := subscription.Events(ctx, 1)
					chatTestOK(t, err)
					if live.ResyncRequired || len(live.Events) != 1 || live.Events[0].Message == nil ||
						live.Events[0].Message.Seq != seq || live.Events[0].Message.TestProvider.Validate() != nil {
						t.Fatalf("Подписчик пропустил реплику %d: %+v", seq, live)
					}
					// replay содержит тот же кадр при чтении журнала со старой позиции.
					replay, err := manager.Events(ctx, streamID, cursor, 1)
					chatTestOK(t, err)
					if replay.ResyncRequired || len(replay.Events) != 1 || replay.Events[0].Message == nil ||
						replay.Events[0].Message.Seq != seq || replay.Cursor != live.Cursor {
						t.Fatalf("При повторной доставке потерялась реплика %d: %+v", seq, replay)
					}
					cursor = live.Cursor
				}
			})
		})
	}
}

// TestChatManagerClearWaitsForRunningHub проверяет запросы очистки после сохранения RUNNING,
// пока ещё нет наблюдателя, по которому Manager может создать ленту эфира. До этого момента
// клиент продолжает ждать. После появления ленты событие очистки получает её SessionID.
func TestChatManagerClearWaitsForRunningHub(t *testing.T) {
	// Внутренний t управляет сроками запросов и фоновыми работами Manager через виртуальные часы.
	synctest.Test(t, func(t *testing.T) {
		// ctx ограничивает операции до конца сценария.
		ctx := t.Context()
		// session хранит результат уже сохранённого запуска, а fence адресует точную команду запуска.
		session := core.Session{Record: chatTestRecord("live-clear-session"), StreamID: "virtual-test-feed",
			State: core.SessionRunning, Incarnation: "virtual-chat", Lifecycle: 1, Issued: true}
		fence := core.ExecutionFence{CommandID: "live-clear-start", SessionID: session.ID,
			Incarnation: session.Incarnation, Lifecycle: session.Lifecycle, Attempt: 1}
		// store сразу возвращает RUNNING и квитанцию, хотя оперативный наблюдатель появится позднее.
		store := &chatTestClockStore{running: &session, receipt: core.CommandReceipt{
			Record: chatTestRecord(fence.CommandID), Kind: core.CommandStartSession, TargetID: session.StreamID,
			SessionID: session.ID, Incarnation: session.Incarnation, Lifecycle: session.Lifecycle,
			Attempt: fence.Attempt, Status: core.CommandSucceeded,
		}}
		// manager обслуживает запросы чата, а service открывает наблюдателя сохранённого запуска.
		manager, service := newChatTestClockManager(t, store, core.DefaultChatLimits())
		// start удерживает ожидающего клиента SSE, release освобождает его допуск, err сообщает об отказе.
		start, release, err := manager.OpenEventStream(ctx, session.StreamID, "")
		chatTestOK(t, err)
		defer release()
		if start.Subscription != nil || start.Waiting.Cursor == "" {
			t.Fatalf("До появления ленты клиент должен ждать с курсором: %+v", start)
		}

		// expiring задаёт срок первого запроса, cancel освобождает его таймер при досрочном выходе.
		expiring, cancel := context.WithTimeout(ctx, time.Millisecond)
		defer cancel()
		// result принимает исход каждого запроса, даже если проверка завершится досрочно.
		result := make(chan error, 1)
		// Первый запрос достигает своего срока до регистрации наблюдателя.
		go func() { result <- manager.Clear(expiring, session.StreamID) }()
		synctest.Wait()
		select {
		// premature показывает, что Clear ответил до появления ленты.
		case premature := <-result:
			t.Fatalf("Очистка завершилась до появления ленты эфира: %v", premature)
		default:
		}
		// waiting подтверждает, что прежний курсор не получил очистку в ожидании ленты эфира.
		waiting, err := manager.ContinueEventStream(ctx, session.StreamID, start.Waiting.Cursor)
		chatTestOK(t, err)
		if waiting.Subscription != nil || waiting.Waiting.Cursor != start.Waiting.Cursor ||
			waiting.Waiting.ResyncRequired || len(waiting.Waiting.Events) != 0 {
			t.Fatalf("Ожидающий клиент получил изменение ленты до установки Hub: %+v", waiting)
		}
		synctest.Sleep(time.Millisecond)
		err = <-result
		if !errors.Is(err, core.ErrNotReady) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("Очистка без ленты после срока ожидания вернула %v", err)
		}

		// request и stop ограничивают ожидание второго запроса, если он не проснётся при появлении Hub.
		request, stop := context.WithTimeout(ctx, time.Second)
		defer stop()
		// Второй запрос ждёт согласования топологии после открытия наблюдателя.
		go func() { result <- manager.Clear(request, session.StreamID) }()
		synctest.Wait()
		// observer предоставляет Manager проверяемого владельца ленты сохранённого запуска.
		observer, err := service.OpenSessionObserver(ctx, fence)
		chatTestOK(t, err)
		observer.NotifyChatRunning()
		chatTestOK(t, <-result)

		manager.mu.Lock()
		// live находит созданный Manager Hub, чтобы получить начало его подписанной последовательности.
		live := manager.sessions[session.StreamID]
		manager.mu.Unlock()
		if live == nil {
			t.Fatal("После успешной очистки лента эфира отсутствует")
		}
		// page показывает события реальной ленты с момента её создания.
		page, err := manager.Events(ctx, session.StreamID, live.hub.cursor(0), 2)
		chatTestOK(t, err)
		if page.ResyncRequired || len(page.Events) != 1 || page.Events[0].Kind != core.ChatEventCleared ||
			page.Events[0].SessionID != session.ID || page.Events[0].Cursor != page.Cursor {
			t.Fatalf("Новая лента эфира должна содержать одно событие очистки: %+v", page)
		}
	})
}

// requireChatTestHistoryLength возвращает снимок ленты streamID и проверяет длину его истории want.
func requireChatTestHistoryLength(t *testing.T, manager *Manager, streamID core.ID, want int) core.ChatSnapshot {
	t.Helper()
	// snapshot содержит согласованную историю, err сообщает об отказе её чтения.
	snapshot, err := manager.Snapshot(t.Context(), streamID)
	chatTestOK(t, err)
	if len(snapshot.Messages) != want {
		t.Fatalf("В снимке %d сообщений, ожидается %d: %+v", len(snapshot.Messages), want, snapshot.Messages)
	}
	return snapshot
}

// TestChatHubClearDropsPreviouslyPublishedMessages проверяет одну границу очистки для снимка,
// replay, непрочитанной очереди и уже извлечённого, но ещё не записанного SSE-кадра.
func TestChatHubClearDropsPreviouslyPublishedMessages(t *testing.T) {
	// Внутренний t останавливает часы между поступлением старых и новых сообщений Hub.
	synctest.Test(t, func(t *testing.T) {
		// ctx ограничивает проверяемые операции, limits разрешает обработку без ожидания пакета.
		ctx := t.Context()
		limits := core.DefaultChatLimits()
		limits.FlushMessages = 1
		// hub моделирует действующую Session с отдельными очередями двух SSE-клиентов.
		hub := newChatTestHub(t, limits, "live-clear-session")
		// before задаёт позицию на момент подключения обоих клиентов.
		before := captureChatTestMessages(t, hub, limits)
		// buffered оставляет старый кадр в очереди, extracted извлекает его до очистки.
		buffered, err := hub.subscribe(ctx, before.Cursor)
		chatTestOK(t, err)
		defer buffered.Close()
		extracted, err := hub.subscribe(ctx, before.Cursor)
		chatTestOK(t, err)
		defer extracted.Close()

		chatTestOK(t, enqueueChatTestMessage(hub, chatTestIncoming("before"), "source-a", true))
		synctest.Wait()
		// pending содержит ранее выданное сообщение, проверяемое перед физической записью.
		pending, err := extracted.Events(ctx, 1)
		chatTestOK(t, err)
		if len(pending.Events) != 1 || pending.Events[0].Message == nil || pending.Events[0].Message.ProviderMessageID != "before" ||
			!extracted.Current(pending.Events[0]) {
			t.Fatalf("До очистки клиент не получил применимое сообщение: %+v", pending)
		}

		chatTestOK(t, hub.clearMessages(ctx))
		if extracted.Current(pending.Events[0]) {
			t.Fatal("Кадр, извлечённый до очистки, остался применимым")
		}
		// empty проверяет, что история очищена на позиции действующего запуска.
		empty := captureChatTestMessages(t, hub, limits)
		if empty.Session == nil || empty.Session.SessionID != "live-clear-session" || len(empty.Messages) != 0 || empty.Cursor == before.Cursor {
			t.Fatalf("Неверный снимок сразу после очистки: %+v", empty)
		}

		chatTestOK(t, enqueueChatTestMessage(hub, chatTestIncoming("before"), "source-a", true))
		chatTestOK(t, enqueueChatTestMessage(hub, chatTestIncoming("after"), "source-a", true))
		synctest.Wait()
		// page содержит границу очистки и следующее сообщение из очереди клиента, который её не читал.
		page, err := buffered.Events(ctx, 8)
		chatTestOK(t, err)
		if page.ResyncRequired || len(page.Events) != 2 || page.Events[0].Kind != "chat.cleared" ||
			page.Events[0].Cursor != empty.Cursor || page.Events[0].SessionID != "live-clear-session" || page.Events[0].Message != nil ||
			page.Events[1].Kind != "chat.message" || page.Events[1].Message == nil ||
			page.Events[1].Message.ProviderMessageID != "after" || page.Events[1].Message.Seq != 2 {
			t.Fatalf("Очередь подписчика не сохранила границу очистки и новый порядок: %+v", page)
		}
		// current содержит согласованную с SSE историю после подавления дубликата.
		current := captureChatTestMessages(t, hub, limits)
		if len(current.Messages) != 1 || current.Messages[0].ProviderMessageID != "after" || current.Cursor != page.Cursor {
			t.Fatalf("Снимок и доставка после очистки расходятся: %+v", current)
		}
		// replay допускает пересинхронизацию или выдачу границы, но не историю до очистки.
		replay, err := hub.events(ctx, before.Cursor, 8)
		chatTestOK(t, err)
		if !replay.ResyncRequired && (len(replay.Events) != 2 || replay.Events[0].Kind != "chat.cleared" ||
			replay.Events[1].Message == nil || replay.Events[1].Message.ProviderMessageID != "after") {
			t.Fatalf("Продолжение со старого курсора пересекло границу очистки: %+v", replay)
		}
		// event фиксирует каждый применимый кадр даже при отдельном флаге пересинхронизации.
		for _, event := range replay.Events {
			if event.Message != nil && event.Message.ProviderMessageID == "before" {
				t.Fatalf("Replay выдал удалённое сообщение: %+v", event)
			}
		}
	})
}
