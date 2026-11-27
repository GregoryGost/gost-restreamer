package core_test

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
)

// openChatAdmissionStore открывает SQLite с рабочими миграциями в каталоге t и регистрирует
// закрытие хранилища после освобождения допусков, полученных в тесте.
func openChatAdmissionStore(t *testing.T) *storage.Store {
	t.Helper()
	// ctx ограничивает открытие SQLite временем жизни теста.
	ctx := t.Context()
	// store владеет временной БД с рабочими миграциями, err сообщает об отказе её открытия.
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "chat-admission.sqlite"), storage.Options{
		MaxOpenConns: 4, MaxIdleConns: 4, ConnMaxLifetime: time.Minute, ConnMaxIdleTime: time.Minute,
		BusyTimeout: 10 * time.Millisecond, RetryAttempts: 3, RetryTimeout: time.Second, RetryDelay: time.Millisecond,
		PreparationLimits:     core.PreparationLimits{MaxQueued: 8, MaxProfiles: 8},
		ObservationLimits:     core.ObservationLimits{MaxSessions: 8, MaxGroups: 8, MaxOutputs: 16, MaxFFmpegWorkers: 8, MaxIdentifierBytes: 1024, MaxErrorBytes: 4096, MaxBytes: 1 << 20},
		OutputExecutionLimits: core.OutputExecutionLimits{MaxReservations: 8, MaxIdentifierBytes: 1024, MaxBytes: 1 << 20},
	})
	requireChatAdmissionOK(t, err)
	t.Cleanup(func() {
		// Закрывает БД после освобождения допусков, closeErr сообщает об отказе очистки.
		if closeErr := store.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	return store
}

// chatAdmissionStreams создаёт через service два несвязанных потока до эфира для проверки
// изоляции допусков теста t.
func chatAdmissionStreams(t *testing.T, service *core.Service) (core.Stream, core.Stream) {
	t.Helper()
	// ctx ограничивает время подготовки потоков временем жизни t.
	ctx := t.Context()
	// stream является целью проверяемой операции, а err сообщает об отказе создания.
	stream, err := service.CreateStream(ctx, core.Stream{Name: "Первый предпросмотр", PublicationKey: "local-first"})
	requireChatAdmissionOK(t, err)
	// other используется для контроля независимости второго потока при операции над stream.
	other, err := service.CreateStream(ctx, core.Stream{Name: "Соседний предпросмотр", PublicationKey: "local-independent"})
	requireChatAdmissionOK(t, err)
	return stream, other
}

// TestChatTestAdmissionRevokesAtConfirmedRunning проверяет допуск на настоящем COMMIT
// команды запуска без выходов. Переход не зависит от наблюдателей Session и публикации адаптера.
func TestChatTestAdmissionRevokesAtConfirmedRunning(t *testing.T) {
	// ctx ограничивает операции изолированной SQLite временем жизни теста.
	ctx := t.Context()
	// store владеет временной SQLite, service использует её общий реестр, err сообщает об отказе создания Service.
	store := openChatAdmissionStore(t)
	service, err := core.NewService(store, store.Incarnation())
	requireChatAdmissionOK(t, err)
	// stream запускается в тесте, other сохраняет независимый предпросмотр.
	stream, other := chatAdmissionStreams(t, service)
	// now задаёт времена исходных записей с точностью их представления в SQLite.
	now := time.Now().UTC().Truncate(time.Microsecond)
	// pending содержит соответствующую уже выданному запуску Session без наблюдателей и выходов.
	pending := core.Session{
		Record:   core.Record{ID: "pending-chat-session", Revision: 1, CreatedAt: now, UpdatedAt: now},
		StreamID: stream.ID, Incarnation: store.Incarnation(), Lifecycle: 1,
		State: core.SessionStarting, Issued: true, Snapshot: core.SessionSnapshot{Stream: stream},
	}
	// identity задаёт синтетическую команду, digest обеспечивает соответствие её отпечатка.
	identity := []byte("complete-chat-session")
	digest := sha256.Sum256(identity)
	// receipt подтверждает ISSUED точной попытки, которую штатный Complete переводит в SUCCEEDED.
	receipt := core.CommandReceipt{
		Record:    core.Record{ID: "complete-chat-session", Revision: 1, CreatedAt: now, UpdatedAt: now},
		RequestID: "complete-chat-session", Kind: core.CommandStartSession, TargetID: stream.ID,
		SessionID: pending.ID, Incarnation: pending.Incarnation, Lifecycle: 1, Attempt: 1,
		Identity: identity, Fingerprint: hex.EncodeToString(digest[:]), Status: core.CommandIssued,
	}
	err = store.Write(ctx, func(r core.Repositories) error {
		// r атомарно помещает выданную сессию и точную квитанцию проверяемой команды без медиаресурсов.
		// insertErr сообщает об отказе записи сессии до вставки зависимой квитанции.
		if insertErr := r.Runtime.InsertSession(ctx, pending); insertErr != nil {
			return insertErr
		}
		return r.Runtime.InsertReceipt(ctx, receipt)
	})
	requireChatAdmissionOK(t, err)

	// admission допускает публикации при STARTING, untouched относится к независимому потоку.
	admission, err := service.AdmitChatTest(ctx, stream.ID)
	requireChatAdmissionOK(t, err)
	t.Cleanup(admission.Close)
	untouched, err := service.AdmitChatTest(ctx, other.ID)
	requireChatAdmissionOK(t, err)
	t.Cleanup(untouched.Close)
	// fence идентифицирует точную команду, stale имеет чужой номер попытки.
	fence := core.ExecutionFence{CommandID: receipt.ID, SessionID: pending.ID, Incarnation: pending.Incarnation, Lifecycle: 1, Attempt: 1}
	stale := fence
	stale.Attempt++
	if err = service.Complete(ctx, stale, core.Completion{Success: true}); !errors.Is(err, core.ErrStale) {
		t.Fatalf("Чужая попытка завершения вернула %v", err)
	}
	// published считает выполненные допускаемые записи независимо от результата команды.
	published := 0
	// publish изменяет только локальный счётчик, моделируя атомарную запись в Hub.
	publish := func() { published++ }
	requireChatAdmissionOK(t, admission.Publish(ctx, publish))
	if published != 1 {
		t.Fatalf("STARTING или ошибочное завершение заблокировали запись: %d", published)
	}

	requireChatAdmissionOK(t, service.Complete(ctx, fence, core.Completion{Success: true}))
	select {
	case <-admission.Done():
	default:
		t.Fatal("COMMIT состояния RUNNING не отозвал тестовый допуск до возврата")
	}
	if err = admission.Publish(ctx, publish); !errors.Is(err, core.ErrConflict) || published != 1 {
		t.Fatalf("Старый допуск выполнил запись после RUNNING: число записей %d, ошибка %v", published, err)
	}
	// next проверяет, что новый запрос тоже не получает допуск для подтверждённого эфира.
	next, err := service.AdmitChatTest(ctx, stream.ID)
	if !errors.Is(err, core.ErrConflict) || next != nil {
		t.Fatalf("Подтверждённый эфир допустил новый генератор: %v, %v", next, err)
	}
	requireChatAdmissionOK(t, untouched.Publish(ctx, publish))
	if published != 2 {
		t.Fatalf("Запуск первого потока отозвал допуск соседнего: %d", published)
	}
}

// TestChatTestAdmissionManualCloseStaysLocal проверяет, что локальный Close будит владельца
// предпросмотра без общей сверки реальных чатов и оставляет соседнюю ленту работоспособной.
func TestChatTestAdmissionManualCloseStaysLocal(t *testing.T) {
	// ctx ограничивает синхронные операции, store владеет изолированной БД.
	ctx := t.Context()
	store := openChatAdmissionStore(t)
	// service использует общий реестр store, err сообщает об отказе создания Service.
	service, err := core.NewService(store, store.Incarnation())
	requireChatAdmissionOK(t, err)
	requireChatAdmissionOK(t, service.ConfigureTelemetry(ctx, core.TelemetryLimits{
		Events:          core.EventLimits{MaxEvents: 2, MaxBytes: 4096, MaxEventBytes: 2048, MaxPageSize: 2},
		MediaStaleAfter: time.Second, MaxGPUDevices: 1, MaxGPUUUIDBytes: 64, MaxGPUBytes: 4096,
	}))
	// stream предоставляет закрываемый допуск, other служит контролем изоляции.
	stream, other := chatAdmissionStreams(t, service)
	// closed получает локальное закрытие, neighbor остаётся активным.
	closed := admitChatPreview(t, service, stream.ID)
	neighbor := admitChatPreview(t, service, other.ID)
	// before фиксирует общий сигнал чатов непосредственно перед Close.
	before, err := service.RuntimeAnchor(ctx)
	requireChatAdmissionOK(t, err)
	if before.ChatChanged == nil {
		t.Fatal("Исходный снимок не содержит сигнала изменения чатов")
	}

	closed.Close()
	requireChatAdmissionState(t, closed, true)
	requireChatAdmissionState(t, neighbor, false)
	select {
	case <-before.ChatChanged:
		t.Fatal("Закрытие одного предпросмотра разбудило общую сверку чатов")
	default:
	}
}

// chatDeleteRollbackStore выполняет настоящую транзакцию SQLite, но отклоняет её после
// успешного удаления строки и до COMMIT, сохраняя общий RuntimeAuthority хранилища.
type chatDeleteRollbackStore struct {
	// Store обслуживает все остальные операции реального хранилища.
	*storage.Store
	// cause возвращается транзакции для принудительного отката удаления.
	cause error
}

// Write исполняет write с контекстом ctx и откатывает её изменения, если write завершилась успешно.
func (s chatDeleteRollbackStore) Write(ctx context.Context, write func(core.Repositories) error) error {
	return s.Store.Write(ctx, func(repos core.Repositories) error {
		// Вызывает исходную операцию с транзакционными repos и возвращает причину отказа.
		// err содержит исходную ошибку операции, если удаление не состоялось.
		if err := write(repos); err != nil {
			return err
		}
		return s.cause
	})
}

// TestChatTestAdmissionRevokesAtConfirmedDeletion проверяет через публичный DeleteStream,
// что конфликт ревизии и откат сохраняют предпросмотр, а COMMIT отзывает только целевой допуск.
func TestChatTestAdmissionRevokesAtConfirmedDeletion(t *testing.T) {
	// ctx ограничивает время операций, store владеет временной SQLite.
	ctx := t.Context()
	store := openChatAdmissionStore(t)
	// service выполняет обычные транзакции, err сообщает об отказе создания Service.
	service, err := core.NewService(store, store.Incarnation())
	requireChatAdmissionOK(t, err)
	// rollbackCause подтверждает, что удаление дошло до точки принудительного отката.
	rollbackCause := errors.New("принудительный откат удаления")
	// rollingBack выполняет удаление в том же хранилище с откатом перед COMMIT.
	rollingBack, err := core.NewService(chatDeleteRollbackStore{Store: store, cause: rollbackCause}, store.Incarnation())
	requireChatAdmissionOK(t, err)
	// stream проверяется на удаление, other существует независимо от него.
	stream, other := chatAdmissionStreams(t, service)
	// target и neighbor представляют два уже открытых предпросмотра.
	target := admitChatPreview(t, service, stream.ID)
	neighbor := admitChatPreview(t, service, other.ID)

	if err = service.DeleteStream(ctx, stream.ID, stream.Revision+1); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("Удаление с чужой ревизией дало %v, ожидается ErrConflict", err)
	}
	requireChatAdmissionState(t, target, false)
	if err = rollingBack.DeleteStream(ctx, stream.ID, stream.Revision); !errors.Is(err, rollbackCause) {
		t.Fatalf("Удаление с принудительным откатом дало %v, ожидается причина отката", err)
	}
	requireChatAdmissionState(t, target, false)

	requireChatAdmissionOK(t, service.DeleteStream(ctx, stream.ID, stream.Revision))
	requireChatAdmissionState(t, target, true)
	requireChatAdmissionState(t, neighbor, false)
	// missing проверяет отказ нового допуска после удаления, err должен сообщить об отсутствии потока.
	missing, err := service.AdmitChatTest(ctx, stream.ID)
	if missing != nil || !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("Удалённый поток дал допуск %v и ошибку %v, ожидается ErrNotFound", missing, err)
	}
}

// admitChatPreview получает у service допуск текущего streamID и освобождает его при завершении t.
func admitChatPreview(t *testing.T, service *core.Service, streamID core.ID) *core.ChatTestAdmission {
	t.Helper()
	// admission разрешает публикации тестовой ленты, err сообщает об отказе допуска.
	admission, err := service.AdmitChatTest(t.Context(), streamID)
	requireChatAdmissionOK(t, err)
	t.Cleanup(admission.Close)
	return admission
}

// requireChatAdmissionState сверяет для t уведомление и возможность записи через admission.
// revoked задаёт ожидаемое состояние после завершения проверяемой операции.
func requireChatAdmissionState(t *testing.T, admission *core.ChatTestAdmission, revoked bool) {
	t.Helper()
	select {
	case <-admission.Done():
		if !revoked {
			t.Fatal("Отозван допуск действующего предпросмотра")
		}
	default:
		if revoked {
			t.Fatal("Допуск больше не действует, но Done остался открыт")
		}
	}
	// published показывает, разрешил ли допуск выполнить запись, а err сообщает об отказе.
	published := false
	err := admission.Publish(t.Context(), func() {
		// Отмечает выполнение записи через проверяемый допуск.
		published = true
	})
	if revoked {
		if published || !errors.Is(err, core.ErrConflict) {
			t.Fatalf("Отозванный допуск дал публикацию %t и ошибку %v", published, err)
		}
	} else if !published || err != nil {
		t.Fatalf("Действующий допуск дал публикацию %t и ошибку %v", published, err)
	}
}

// requireChatAdmissionOK завершает t при err в подготовке изолированного допуска или операции с ним.
func requireChatAdmissionOK(t *testing.T, err error) {
	t.Helper()
	if err != nil {
		t.Fatal(err)
	}
}
