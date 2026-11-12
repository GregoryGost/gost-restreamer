package publisher

import (
	"context"
	"testing"
	"testing/synctest"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media/rtmp"
)

// Проверяет публикацию окончательной ошибки выхода до завершения заблокированного Wait транспорта без
// отзыва наблюдателя.
func TestTerminalObservationPrecedesTransportJoin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Проверяет в управляемом времени публикацию окончательного отказа до завершения задержанного
		// транспорта, сохраняя действующего наблюдателя.

		// ctx задаёт общий контекст настоящих допусков и наблюдений сценария.
		ctx := context.Background()
		// authority содержит настоящий RuntimeAuthority; err сохраняет результаты создания
		// сервиса, наблюдателя и чтения событий.
		authority, err := core.NewRuntimeAuthority("publisher-owner", core.ObservationLimits{MaxSessions: 1, MaxGroups: 1, MaxOutputs: 1, MaxFFmpegWorkers: 1, MaxIdentifierBytes: 1024, MaxErrorBytes: 1024, MaxBytes: 1 << 20}, core.OutputExecutionLimits{MaxReservations: 1, MaxIdentifierBytes: 1024, MaxBytes: 1 << 20})
		if err != nil {
			t.Fatal(err)
		}
		defer authority.Close()
		// fence задаёт точную идентичность выхода для публикации оперативного отказа.
		fence := core.ExecutionFence{CommandID: "command", Incarnation: "publisher-owner", SessionID: "session", Lifecycle: 1, OutputID: "output", OutputGeneration: 1, Attempt: 1}
		// service связывает настоящий реестр наблюдений с управляемым репозиторием исполнения.
		service, err := core.NewService(&lifecycleAcceptanceUOW{authority: authority, repository: lifecycleAcceptanceRepository{fence: fence}}, fence.Incarnation)
		if err != nil {
			t.Fatal(err)
		}
		if err = service.ConfigureTelemetry(ctx, core.TelemetryLimits{Events: core.EventLimits{MaxEvents: 16, MaxPageSize: 16, MaxBytes: 65536, MaxEventBytes: 4096}, MediaStaleAfter: time.Second, MaxGPUDevices: 1, MaxGPUUUIDBytes: 64, MaxGPUBytes: 4096}); err != nil {
			t.Fatal(err)
		}
		// observer содержит действующего наблюдателя выхода до и после окончательной сетевой
		// ошибки.
		observer, err := service.OpenOutputObserver(ctx, fence)
		if err != nil {
			t.Fatal(err)
		}
		if err = observer.Observe(ctx, core.OutputObservation{State: core.OutputPublishing}); err != nil {
			t.Fatal(err)
		}
		// before сохраняет исходный курсор событий перед отказом Publisher.
		before, err := service.RuntimeSnapshot(ctx, core.RuntimeSessionFence{SessionID: fence.SessionID, Incarnation: fence.Incarnation, Lifecycle: fence.Lifecycle})
		if err != nil {
			t.Fatal(err)
		}
		// router содержит Router; source содержит источник; video и audio задают контрольную
		// пару для запуска передачи.
		router, source, video, audio := testRouter(t)
		// conn содержит транспорт с окончательным отказом заголовков и задержанным Wait.
		conn := newTestConnection()
		// release разрешает Wait транспорта завершиться после проверки публикации отказа.
		release := make(chan struct{})
		defer close(release)
		conn.join = release
		conn.onHeaders = func() error {
			// Возвращает OutboundDenied при записи заголовков, моделируя окончательный отказ публикации.
			return &rtmp.OutboundError{Kind: rtmp.OutboundDenied}
		}
		// p содержит Publisher с настоящим наблюдателем и управляемым отказом транспорта.
		p, _ := testPublisher(t, router, func(_ context.Context, _ rtmp.OutboundTarget, options rtmp.OutboundOptions) (connection, error) {
			// Сохраняет транспортные настройки и возвращает соединение с задержанным Wait.

			conn.opts = options
			return conn, nil
		})
		p.observer = observer
		startTestPublisher(p)
		synctest.Wait()
		publishPair(t, source, video, audio, 0)
		synctest.Wait()
		if p.Result().NetworkJoined {
			t.Fatal("незавершённый транспорт объявлен присоединённым")
		}
		// page содержит события после исходного курсора для поиска окончательного отказа до
		// присоединения сети.
		page, err := service.RuntimeEvents(ctx, before.Cursor, 16)
		if err != nil {
			t.Fatal(err)
		}
		// found отмечает наличие события FAILED с причиной ObservationFailed.
		found := false
		// event содержит очередное оперативное событие для проверки окончательного отказа
		// выхода.
		for _, event := range page.Events {
			if event.OutputState == core.OutputFailed && event.Reason == core.ReasonObservationFailed {
				found = true
			}
		}
		if !found {
			t.Fatal("окончательная ошибка не опубликована до физического завершения", page.Events)
		}
		if !observer.Valid() {
			t.Fatal("оперативный отказ отозвал наблюдателя")
		}
	})
}
