package chat

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
)

// chatTestOK останавливает тест t при неожиданном отказе err операции над изолированной фикстурой.
func chatTestOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}

// chatTestReceive получает ожидаемый сигнал ch либо завершает тест t по ограниченному ctx.
// T задаёт тип сообщения проверяемой операции, включая сигнал закрытия канала.
func chatTestReceive[T any](t *testing.T, ctx context.Context, ch <-chan T) T {
	t.Helper()
	select {
	// value содержит пришедшее значение или нулевое значение сигнала закрытия.
	case value := <-ch:
		return value
	case <-ctx.Done():
		t.Fatal(ctx.Err())
		// zero нужен только для типизации ветки после Fatal.
		var zero T
		return zero
	}
}

// openChatTestStore запускает штатные миграции в отдельной файловой SQLite для теста t.
// ctx ограничивает открытие, а очистка t закрывает Store после зависимых ресурсов.
func openChatTestStore(t *testing.T, ctx context.Context) (*storage.Store, *core.Service) {
	t.Helper()
	// store обслуживает исключительно временный файл. err передаёт отказ открытия или создания Service.
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "chat.sqlite"), storage.Options{
		MaxOpenConns: 4, MaxIdleConns: 4, ConnMaxLifetime: time.Minute, ConnMaxIdleTime: time.Minute,
		BusyTimeout: 10 * time.Millisecond, RetryAttempts: 3, RetryTimeout: time.Second, RetryDelay: time.Millisecond,
		PreparationLimits:     core.PreparationLimits{MaxQueued: 8, MaxProfiles: 8},
		ObservationLimits:     core.ObservationLimits{MaxSessions: 8, MaxGroups: 8, MaxOutputs: 16, MaxFFmpegWorkers: 8, MaxIdentifierBytes: 1024, MaxErrorBytes: 4096, MaxBytes: 1 << 20},
		OutputExecutionLimits: core.OutputExecutionLimits{MaxReservations: 8, MaxIdentifierBytes: 1024, MaxBytes: 1 << 20},
	})
	chatTestOK(t, err)
	t.Cleanup(func() {
		// Закрывает мигрированную временную SQLite после завершения Manager и адаптеров.
		chatTestOK(t, store.Close())
	})
	// service связывает операции Core с инкарнацией этой временной SQLite.
	service, err := core.NewService(store, store.Incarnation())
	chatTestOK(t, err)
	chatTestOK(t, service.ConfigureTelemetry(ctx, core.TelemetryLimits{
		MediaStaleAfter: time.Second, MaxGPUDevices: 1, MaxGPUUUIDBytes: 64, MaxGPUBytes: 4096,
		Events: core.EventLimits{MaxEvents: 64, MaxBytes: 65536, MaxEventBytes: 8192, MaxPageSize: 64},
	}))
	return store, service
}

// chatTestIncoming создаёт минимальное допустимое сообщение с исходным идентификатором id.
func chatTestIncoming(id string) core.ChatIncomingMessage {
	return core.ChatIncomingMessage{Provider: core.ChatYouTube, ChannelID: "actual-chat", ProviderMessageID: id, Kind: core.ChatMessageText, Text: "Сообщение 🙂", Author: core.ChatAuthor{ID: "author", DisplayName: "Автор"}}
}
