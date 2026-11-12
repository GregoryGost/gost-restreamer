package core

import (
	"context"
	"errors"
	"testing"
	"time"
)

// runtimeSnapshotRepository подставляет заданную сессию в проверки долговечного владельца допуска.
type runtimeSnapshotRepository struct {
	// RuntimeRepository сохраняет остальные методы контракта репозитория, не используемые этим
	// сценарием.
	RuntimeRepository
	// session содержит сессию, возвращаемую проверкам исполнения и OBS.
	session Session
}

// Возвращает заданную сессию для проверки сохранённого владельца допуска.
func (r runtimeSnapshotRepository) Session(context.Context, ID) (Session, error) {
	return r.session, nil
}

// Возвращает заданную сессию как текущую для проверки допуска OBS.
func (r runtimeSnapshotRepository) ActiveSession(context.Context, ID) (Session, error) {
	return r.session, nil
}

// runtimeSnapshotUOW предоставляет заданный снимок репозиториев или контролируемый отказ чтения.
type runtimeSnapshotUOW struct {
	// repository хранит подставляемую сессию для согласованного чтения.
	repository runtimeSnapshotRepository
	// viewErr задаёт отказ проверки сохранённого состояния, если он требуется сценарию.
	viewErr error
}

// Передаёт тестовые репозитории исполнения читающей операции либо возвращает заданный отказ сверки.
func (u *runtimeSnapshotUOW) View(_ context.Context, fn func(Repositories) error) error {
	if u.viewErr != nil {
		return u.viewErr
	}
	return fn(Repositories{Runtime: u.repository, Artifacts: emptyPreparationRepository{}})
}

// Завершает тестовую запись без выполнения переданной функции; используется там, где проверяется
// только возможность допуска исполнения.
func (*runtimeSnapshotUOW) Write(context.Context, func(Repositories) error) error { return nil }

// runtimeAuthorityUOW добавляет общую RuntimeAuthority к тестовому UnitOfWork.
type runtimeAuthorityUOW struct {
	// runtimeSnapshotUOW предоставляет управляемое согласованное чтение тестовой сессии.
	runtimeSnapshotUOW
	// authority связывает пользователей тестового UnitOfWork с одной authority.
	authority *RuntimeAuthority
}

// Возвращает общую тестовую RuntimeAuthority для нескольких пользователей UnitOfWork.
func (u *runtimeAuthorityUOW) RuntimeAuthority() *RuntimeAuthority { return u.authority }

// Проверяет отказ допуска OBS и исполнения без общей RuntimeAuthority до вызова установки источника.
func TestRuntimeAdmissionExplicitlyRequiresSharedCapability(t *testing.T) {
	// service содержит сервис без общей authority, а err — результат его создания.
	service, err := NewService(&runtimeSnapshotUOW{}, "owner")
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.ActivateOBS(context.Background(), "stream", "key", func(*RuntimeLease) (func(), error) {
		// Отмечает ошибкой вызов установки OBS, который должен быть отклонён при отсутствии общей
		// RuntimeAuthority.

		t.Fatal("установка без общей RuntimeAuthority")
		return nil, nil
	})
	if !errors.Is(err, ErrRuntimeCapability) {
		t.Fatal(err)
	}
	_, err = service.BindExecutionOwner(context.Background(), ExecutionFence{})
	if !errors.Is(err, ErrRuntimeCapability) {
		t.Fatal(err)
	}
}

// Проверяет отзыв допуска при невозможности послезаписной сверки без подмены результата и повтора
// записи, а затем выдачу нового допуска после успешной проверки.
func TestRuntimeReconciliationFailsClosedWithoutReplacingWriteOutcome(t *testing.T) {
	// operationErr задаёт успешный либо неоднозначный результат записи перед отказом сверки.
	for _, operationErr := range []error{nil, errors.New("неоднозначный результат записи")} {
		// authority содержит общий координатор допусков, а err — результат проверки его
		// бюджетов.
		authority, err := NewRuntimeAuthority("owner", ObservationLimits{MaxSessions: 4, MaxGroups: 8, MaxOutputs: 8, MaxFFmpegWorkers: 8, MaxIdentifierBytes: 1024, MaxErrorBytes: 4096, MaxBytes: 65536}, OutputExecutionLimits{MaxReservations: 8, MaxIdentifierBytes: 1024, MaxBytes: 65536})
		if err != nil {
			t.Fatal(err)
		}
		// uow предоставляет подменяемый снимок сессии и общую RuntimeAuthority.
		uow := &runtimeAuthorityUOW{authority: authority}
		uow.repository.session = Session{ID: "session", StreamID: "stream", Incarnation: "owner", Lifecycle: 1, State: SessionRunning, Snapshot: SessionSnapshot{Stream: Stream{PublicationKey: "key"}}}
		// service содержит сервис для проверки отзыва при ошибке послезаписной сверки.
		service, err := NewService(uow, "owner")
		if err != nil {
			t.Fatal(err)
		}
		// lease содержит исходный допуск OBS, который должен быть отозван при недоказанной
		// свежести.
		lease, err := service.ActivateOBS(context.Background(), "stream", "key", func(*RuntimeLease) (func(), error) {
			// Возвращает nil вместо функции отзыва для создания исходного допуска OBS перед проверкой ошибки
			// сверки.
			return nil, nil
		})
		if err != nil {
			t.Fatal(err)
		}
		// calls считает исполнения записи, чтобы обнаружить недопустимый повтор.
		calls := 0
		err = authority.GuardWrite(context.Background(), uow, time.Second, func() (bool, error) {
			// Имитирует возможное сохранение с заданным результатом записи и одновременно делает последующее
			// чтение недоступным.

			calls++
			uow.viewErr = errors.New("состояние не удалось подтвердить")
			return true, operationErr
		})
		if err != operationErr || calls != 1 || lease.Valid() {
			t.Fatalf("неопределённость изменила результат/повторы или сохранила допуск: %v, %d", err, calls)
		}
		select {
		case <-lease.Done():
		default:
			t.Fatal("Done не закрыт")
		}
		_, err = service.ActivateOBS(context.Background(), "stream", "key", func(*RuntimeLease) (func(), error) {
			// Отмечает ошибкой вызов установки OBS, пока сохранённое состояние не удалось прочитать.

			t.Fatal("установка без проверки сохранённого состояния")
			return nil, nil
		})
		if err == nil {
			t.Fatal("ошибка чтения не закрыла новый допуск")
		}
		uow.viewErr = nil
		// fresh содержит новый допуск OBS после восстановления чтения сохранённого состояния.
		fresh, err := service.ActivateOBS(context.Background(), "stream", "key", func(*RuntimeLease) (func(), error) {
			// Возвращает nil вместо функции отзыва для нового допуска OBS после восстановления чтения.
			return nil, nil
		})
		if err != nil || !fresh.Valid() || fresh.ID() == lease.ID() {
			t.Fatalf("новая проверка сохранённого состояния не восстановила допуск: %v", err)
		}
		authority.Close()
	}
}
