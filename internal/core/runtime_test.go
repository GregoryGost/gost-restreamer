package core

import (
	"context"
	"errors"
	"testing"
	"time"
)

type runtimeSnapshotRepository struct {
	RuntimeRepository
	session Session
}

func (r runtimeSnapshotRepository) Session(context.Context, ID) (Session, error) {
	return r.session, nil
}
func (r runtimeSnapshotRepository) ActiveSession(context.Context, ID) (Session, error) {
	return r.session, nil
}

type runtimeSnapshotUOW struct {
	repository runtimeSnapshotRepository
	viewErr    error
}

func (u *runtimeSnapshotUOW) View(_ context.Context, fn func(Repositories) error) error {
	if u.viewErr != nil {
		return u.viewErr
	}
	return fn(Repositories{Runtime: u.repository, Artifacts: emptyPreparationRepository{}})
}
func (*runtimeSnapshotUOW) Write(context.Context, func(Repositories) error) error { return nil }

type runtimeAuthorityUOW struct {
	runtimeSnapshotUOW
	authority *RuntimeAuthority
}

func (u *runtimeAuthorityUOW) RuntimeAuthority() *RuntimeAuthority { return u.authority }

func TestRuntimeAdmissionExplicitlyRequiresSharedCapability(t *testing.T) {
	service, err := NewService(&runtimeSnapshotUOW{}, "owner")
	if err != nil {
		t.Fatal(err)
	}
	_, err = service.ActivateOBS(context.Background(), "stream", "key", func(*RuntimeLease) (func(), error) {
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

func TestRuntimeReconciliationFailsClosedWithoutReplacingWriteOutcome(t *testing.T) {
	for _, operationErr := range []error{nil, errors.New("неоднозначный результат записи")} {
		authority, err := NewRuntimeAuthority("owner", ObservationLimits{MaxSessions: 4, MaxGroups: 8, MaxOutputs: 8, MaxFFmpegWorkers: 8, MaxIdentifierBytes: 1024, MaxErrorBytes: 4096, MaxBytes: 65536}, OutputExecutionLimits{MaxReservations: 8, MaxIdentifierBytes: 1024, MaxBytes: 65536})
		if err != nil {
			t.Fatal(err)
		}
		uow := &runtimeAuthorityUOW{authority: authority}
		uow.repository.session = Session{Record: Record{ID: "session"}, StreamID: "stream", Incarnation: "owner", Lifecycle: 1, State: SessionRunning, Snapshot: SessionSnapshot{Stream: Stream{PublicationKey: "key"}}}
		service, err := NewService(uow, "owner")
		if err != nil {
			t.Fatal(err)
		}
		lease, err := service.ActivateOBS(context.Background(), "stream", "key", func(*RuntimeLease) (func(), error) { return nil, nil })
		if err != nil {
			t.Fatal(err)
		}
		calls := 0
		err = authority.GuardWrite(context.Background(), uow, time.Second, func() (bool, error) {
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
			t.Fatal("установка без проверки сохранённого состояния")
			return nil, nil
		})
		if err == nil {
			t.Fatal("ошибка чтения не закрыла новый допуск")
		}
		uow.viewErr = nil
		fresh, err := service.ActivateOBS(context.Background(), "stream", "key", func(*RuntimeLease) (func(), error) { return nil, nil })
		if err != nil || !fresh.Valid() || fresh.ID() == lease.ID() {
			t.Fatalf("новая проверка сохранённого состояния не восстановила допуск: %v", err)
		}
		authority.Close()
	}
}
