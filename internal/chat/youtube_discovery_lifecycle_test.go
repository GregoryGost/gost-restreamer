package chat

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// youtubeDiscoveryLifecycleAdapter управляет ответами поиска отдельно от перепроверки видео.
type youtubeDiscoveryLifecycleAdapter struct {
	// managerYouTubeFixtureAdapter предоставляет перепроверку и наблюдение за начатыми подписками.
	managerYouTubeFixtureAdapter
	// searches сообщает о начале каждого общего поиска.
	searches chan struct{}
	// discovered передаёт результат очередного поиска.
	discovered chan core.ChatDiscovery
}

// Discover сообщает о начале поиска и возвращает наблюдение из обнаруженных результатов.
// ctx позволяет закрыть операцию, если сценарий завершился до отправки результата.
func (a *youtubeDiscoveryLifecycleAdapter) Discover(ctx context.Context, _ core.ChatSource, _ core.ChatCredentials) (core.ChatDiscovery, error) {
	select {
	case a.searches <- struct{}{}:
	case <-ctx.Done():
		return core.ChatDiscovery{}, ctx.Err()
	}
	select {
	// found содержит результаты независимо управляемого поискового вызова.
	case found := <-a.discovered:
		return found, nil
	case <-ctx.Done():
		return core.ChatDiscovery{}, ctx.Err()
	}
}

// TestYouTubeManagerSeparatesSelectedAndAutomaticBroadcastEnd показывает, что подтверждённое
// завершение снимает автоматическое назначение, а явный выбор оператора остаётся точным.
func TestYouTubeManagerSeparatesSelectedAndAutomaticBroadcastEnd(t *testing.T) {
	// explicit задаёт способ выбора видео перед одной и той же ошибкой перепроверки.
	for _, explicit := range []bool{false, true} {
		// name сохраняет способ выбора в имени независимого подтеста.
		name := "automatic"
		if explicit {
			name = "explicit"
		}
		t.Run(name, func(t *testing.T) {
			// Подтест использует одну открытую Session и раздельные сигналы поиска и videos.list.
			// ctx и cancel ограничивают работу Manager и всех входящих callback.
			ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
			defer cancel()
			// adapter позволяет отдельно подтвердить конец исходного видео и найти новое.
			adapter := &youtubeDiscoveryLifecycleAdapter{
				managerYouTubeFixtureAdapter: managerYouTubeFixtureAdapter{
					chatTestAdapter: chatTestAdapter{readers: make(chan chatTestReader, 2)},
					resolving:       make(chan string, 2),
					resolved:        make(chan core.ChatDiscovery, 2),
				},
				searches: make(chan struct{}, 2), discovered: make(chan core.ChatDiscovery, 2),
			}
			// fixture подтверждает публикацию реального выхода, manager управляет чтением.
			fixture, manager := startYouTubeManagerFixture(t, ctx, adapter)
			// source определяет источник, чей выбор привязан к этой Session.
			source := fixture.sources[0]
			// choice содержит видео без подтверждённого чата, которое потребуется перепроверить.
			choice := core.ChatBroadcast{VideoID: "first-video"}
			// candidates добавляет второе видео только в сценарии явного выбора.
			candidates := []core.ChatBroadcast{choice}
			if explicit {
				candidates = append(candidates, core.ChatBroadcast{VideoID: "other-video", ChannelID: "other-chat"})
			}
			chatTestReceive(t, ctx, adapter.searches)
			adapter.discovered <- core.ChatDiscovery{
				State: core.ChatWaitingChat, Reason: core.ChatReasonUnavailable, Broadcasts: candidates,
			}
			if explicit {
				// waiting фиксирует текущую Session и основание для явного выбора.
				waiting := waitChatTestSnapshot(t, ctx, manager, fixture.stream.ID, func(snapshot core.ChatSnapshot) bool {
					// Предикат ждёт состояние выбора и подтверждённую эпоху ленты.
					return snapshot.Session != nil && slices.ContainsFunc(snapshot.Statuses, func(status core.ChatSourceStatus) bool {
						// Предикат проверяет причину выбора именно для этого источника.
						return status.SourceID == source.ID && status.Reason == core.ChatReasonSelectionRequired
					})
				})
				// choices содержит ревизию и поколение для запроса выбора, err сообщает об отказе чтения.
				choices, err := manager.BroadcastChoices(ctx, *waiting.Session, source.ID)
				chatTestOK(t, err)
				chatTestOK(t, manager.SelectBroadcast(ctx, core.ChatBroadcastSelection{
					Session: *waiting.Session, SourceID: source.ID, SourceRevision: choices.SourceRevision,
					DiscoveryGeneration: choices.DiscoveryGeneration, VideoID: choice.VideoID,
				}))
			}
			if chatTestReceive(t, ctx, adapter.resolving) != choice.VideoID {
				t.Fatal("Ожидание чата не перепроверило исходное видео")
			}
			adapter.resolved <- core.ChatDiscovery{State: core.ChatWaitingChat, Reason: core.ChatReasonEnded}
			if explicit {
				waitChatTestSnapshot(t, ctx, manager, fixture.stream.ID, func(snapshot core.ChatSnapshot) bool {
					// Предикат ждёт остановки единственного обработчика обнаружения после публикации статуса.
					manager.mu.Lock()
					// finished подтверждает, что обработчик больше не сможет запустить отложенный запрос.
					finished := manager.discovering == 0
					manager.mu.Unlock()
					return finished && slices.ContainsFunc(snapshot.Statuses, func(status core.ChatSourceStatus) bool {
						// Предикат связывает подтверждённый конец с исходным источником.
						return status.SourceID == source.ID && status.State == core.ChatFailed && status.Reason == core.ChatReasonEnded
					})
				})
				manager.mu.Lock()
				// readerCount учитывает уже назначенное чтение, даже если горутина ещё не вызвала адаптер.
				readerCount := len(manager.readers)
				manager.mu.Unlock()
				if readerCount != 0 {
					t.Fatal("Manager назначил чтение после завершения выбранного эфира")
				}
				select {
				case <-adapter.searches:
					t.Fatal("Manager заменил явный выбор новым поиском после окончания эфира")
				case <-adapter.resolving:
					t.Fatal("Manager повторно запросил завершённое видео после окончательного отказа")
				case <-adapter.readers:
					t.Fatal("Manager начал чтение другого чата после завершения выбранного")
				default:
				}
				return
			}
			chatTestReceive(t, ctx, adapter.searches)
			adapter.discovered <- core.ChatDiscovery{Broadcasts: []core.ChatBroadcast{
				{VideoID: "new-video", ChannelID: "new-chat"},
			}}
			// reader подтверждает, что повторный поиск действительно запустил чтение нового чата.
			reader := chatTestReceive(t, ctx, adapter.readers)
			if reader.request.Key.ChannelID != "new-chat" {
				t.Fatal("После окончания автоматически найденного эфира запущен неверный чат")
			}
		})
	}
}

// TestYouTubeManagerReportsValidationSlotExhaustion проверяет отдельный допуск к проверке ключа,
// когда активные запросы обнаружения держат все HTTP-слоты.
func TestYouTubeManagerReportsValidationSlotExhaustion(t *testing.T) {
	// ctx и cancel ограничивают подготовку хранилища и время всех проверок.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// store и service предоставляют мигрированную временную SQLite.
	store, service := openChatTestStore(t, ctx)
	// adapter учитывает фактические обращения к проверке ключа.
	adapter := &youtubeCountingKeyValidator{}
	// limits допускает ровно один одновременно выполняемый запрос.
	limits := core.DefaultChatLimits()
	limits.HTTPRequests = 1
	// manager использует общий сетевой допуск и чистое хранилище, err сообщает об отказе создания.
	manager, err := New(ctx, service, store, limits, []core.ChatAdapterPort{adapter})
	chatTestOK(t, err)
	// Callback останавливает Manager перед освобождением временного хранилища.
	t.Cleanup(func() {
		// closeCtx и stop ограничивают завершение Manager до очистки временной базы.
		closeCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		chatTestOK(t, manager.Close(closeCtx))
	})
	// request задаёт один проверяемый синтетический ключ для обеих попыток.
	request := youtubeFixtureRequest("", 0, youtubeFixtureReplacementKey)
	manager.discoverSlots <- struct{}{}
	// denied хранит публичный результат отказа, reason — причину, err — сигнал исчерпания допуска.
	denied, reason, err := manager.ConnectYouTube(ctx, request)
	<-manager.discoverSlots
	if denied.ID != "" || reason != core.ChatReasonLimit || !errors.Is(err, core.ErrBusy) || len(adapter.keys) != 0 {
		t.Fatal("Проверка ключа обошла общий сетевой лимит")
	}
	// result принимает сохранённый аккаунт, если после освобождения слота проверка прошла.
	result, reason, err := manager.ConnectYouTube(ctx, request)
	chatTestOK(t, err)
	if result.ID == "" || reason != core.ChatReasonNone || len(adapter.keys) != 1 {
		t.Fatal("Освобождение сетевого допуска не восстановило проверку ключа")
	}
}
