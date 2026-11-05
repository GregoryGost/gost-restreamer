package integration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"net/url"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
)

// Сбой откатывает только переход прежней Session; подтверждение очистки имеет отдельный барьер.
type ownerLossFaultStore struct {
	*storage.Store
	mu                        sync.Mutex
	old                       core.ID
	retirement                bool
	retirementFailed, healed  chan struct{}
	acknowledgement, ackTried chan struct{}
}

func (s *ownerLossFaultStore) Write(ctx context.Context, callback func(core.Repositories) error) error {
	return s.Store.Write(ctx, func(r core.Repositories) error {
		r.Runtime = ownerLossRuntimeRepository{RuntimeRepository: r.Runtime, store: s}
		r.Artifacts = ownerLossArtifactRepository{ArtifactRepository: r.Artifacts, store: s}
		return callback(r)
	})
}

type ownerLossRuntimeRepository struct {
	core.RuntimeRepository
	store *ownerLossFaultStore
}

func (r ownerLossRuntimeRepository) UpdateSession(ctx context.Context, value core.Session, revision uint64) error {
	r.store.mu.Lock()
	blocked := value.ID == r.store.old && r.store.retirement && value.State != core.SessionRunning
	r.store.mu.Unlock()
	if blocked {
		select {
		case <-r.store.healed:
		default:
			select {
			case r.store.retirementFailed <- struct{}{}:
			default:
			}
			return core.ErrBusy
		}
	}
	return r.RuntimeRepository.UpdateSession(ctx, value, revision)
}

type ownerLossArtifactRepository struct {
	core.ArtifactRepository
	store *ownerLossFaultStore
}

func (r ownerLossArtifactRepository) UpdateCleanup(ctx context.Context, value core.CleanupIntent, revision uint64) error {
	r.store.mu.Lock()
	old := r.store.old
	r.store.mu.Unlock()
	if value.SessionID == old && value.Kind == core.CleanupRuntime && value.Acknowledged {
		select {
		case <-r.store.acknowledgement:
		default:
			select {
			case r.store.ackTried <- struct{}{}:
			default:
			}
			return core.ErrBusy
		}
	}
	return r.ArtifactRepository.UpdateCleanup(ctx, value, revision)
}

func ownerLossFence(receipt core.CommandReceipt) core.ExecutionFence {
	return core.ExecutionFence{CommandID: receipt.ID, Incarnation: receipt.Incarnation, SessionID: receipt.SessionID, Lifecycle: receipt.Lifecycle, OutputID: receipt.OutputID, OutputGeneration: receipt.OutputGeneration, Attempt: receipt.Attempt}
}

func ownerLossAssertHolds(t *testing.T, f *sessionAcceptanceFixture, id core.ID, withOutput, retained bool) {
	t.Helper()
	if withOutput {
		sessionAcceptanceAssertHolds(t, f, id, retained)
		return
	}
	dsn := url.URL{Scheme: "file", Path: filepath.Join(f.directory, "state.sqlite"), RawQuery: "mode=ro&_busy_timeout=1000"}
	db, err := sql.Open("sqlite3", dsn.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	var parent, outputs int
	err = db.QueryRowContext(ctx, "SELECT count(*) FROM session_holds WHERE session_id = ? AND stream_id = ? AND asset_id = ? AND generation_id = ?", id, f.stream.ID, f.asset.ID, f.generation.ID).Scan(&parent)
	if err != nil {
		t.Fatal(err)
	}
	if err = db.QueryRowContext(ctx, "SELECT count(*) FROM output_holds WHERE session_id = ?", id).Scan(&outputs); err != nil {
		t.Fatal(err)
	}
	want := 0
	if retained {
		want = 1
	}
	if parent != want || outputs != 0 {
		t.Fatalf("точные удержания Session без выходов: parent=%d outputs=%d, ожидалось parent=%d outputs=0", parent, outputs, want)
	}
}

func TestSessionManagerOwnerLossRetiresDurablyAndIsolatesNewManualStart(t *testing.T) {
	helper := sessionAcceptanceBuildHelper(t)
	for _, withOutput := range []bool{false, true} {
		for _, unavailable := range []bool{false, true} {
			t.Run(fmt.Sprintf("output-%v-retirement-unavailable-%v", withOutput, unavailable), func(t *testing.T) {
				var endpoints []string
				var indices []int
				receiver, _ := newPublisherReceiver(t, false)
				if withOutput {
					endpoints, indices = []string{"rtmp://" + receiver.listener.Addr().String() + "/live"}, []int{0}
				}
				f := newSessionAcceptanceFixture(t, helper, endpoints, indices)
				fault := &ownerLossFaultStore{Store: f.store, retirement: unavailable, retirementFailed: make(chan struct{}, 1), healed: make(chan struct{}), acknowledgement: make(chan struct{}), ackTried: make(chan struct{}, 1)}
				var healOnce, acknowledgeOnce sync.Once
				heal := func() { healOnce.Do(func() { close(fault.healed) }) }
				acknowledge := func() { acknowledgeOnce.Do(func() { close(fault.acknowledgement) }) }
				service, err := core.NewService(fault, f.store.Incarnation())
				if err != nil {
					t.Fatal(err)
				}
				f.service = service
				f.configureOwners(t, helper)
				f.launch(t)
				t.Cleanup(func() { heal(); acknowledge() })
				start := f.start(t, "start-before-owner-loss")
				old := f.running(t, start)
				original := sessionAcceptanceReceipt(t, service, start.ID, core.CommandSucceeded)
				if withOutput {
					receiver.wait(t, func(messages []publisherWireMessage) bool { return publisherMediaCount(messages) >= 8 })
				}
				ownerLossAssertHolds(t, f, old.ID, withOutput, true)
				lease, err := service.BindExecutionOwner(context.Background(), ownerLossFence(original))
				if err != nil || !lease.Valid() {
					t.Fatalf("нет действующего точного владельца успешного parent: %v", err)
				}
				fault.mu.Lock()
				fault.old = old.ID
				fault.mu.Unlock()
				if err = lease.Revoke(context.Background()); err != nil {
					t.Fatal(err)
				}
				recoveryWaitEvent(t, lease.Done(), "точный owner не отозван")
				if _, err = f.manager.Feed(old); err == nil {
					t.Fatal("Feed сохранил допуск после отзыва owner")
				}
				if unavailable {
					recoveryWaitEvent(t, fault.retirementFailed, "не воспроизведена ошибка долговечного завершения")
					persisted, readErr := service.GetSession(context.Background(), old.ID)
					if readErr != nil || persisted.State != core.SessionRunning || !persisted.Issued {
						t.Fatalf("сбой не сохранил прежнюю SQL-границу: %+v %v", persisted, readErr)
					}
					ownerLossAssertHolds(t, f, old.ID, withOutput, true)
					if rejected, startErr := f.manager.StartSession(context.Background(), core.StartSessionRequest{RequestID: "reject-while-old-sql-slot-active", StreamID: f.stream.ID}); !errors.Is(startErr, core.ErrInUse) || rejected.ID != "" {
						t.Fatalf("неподтверждённая retirement освободила активный SQL slot: %+v %v", rejected, startErr)
					}
					heal()
				}
				var terminal core.Session
				sessionAcceptanceWait(t, func() bool {
					terminal, err = service.GetSession(context.Background(), old.ID)
					return err == nil && terminal.State.Terminal()
				}, func() { f.captureFailure(t) })
				if terminal.EndedAt == nil || !terminal.Issued || !terminal.CleanupPending {
					t.Fatalf("retirement скрыла незавершённую очистку: %+v", terminal)
				}
				ownerLossAssertHolds(t, f, old.ID, withOutput, true)
				if receipt, readErr := service.GetReceipt(context.Background(), original.ID); readErr != nil || !reflect.DeepEqual(receipt, original) {
					t.Fatalf("retirement переписала успешный start: %+v %v", receipt, readErr)
				}
				fresh := f.running(t, f.start(t, "manual-start-after-owner-loss"))
				if fresh.ID == old.ID || fresh.Incarnation != old.Incarnation {
					t.Fatal("новый ручной start не создал независимую Session той же incarnation")
				}
				feed, err := f.manager.Feed(fresh)
				if err != nil {
					t.Fatal(err)
				}
				ownerLossAssertHolds(t, f, old.ID, withOutput, true)
				ownerLossAssertHolds(t, f, fresh.ID, withOutput, true)
				before := publisherMediaCount(receiver.snapshot())
				if withOutput {
					receiver.wait(t, func(messages []publisherWireMessage) bool { return publisherMediaCount(messages) >= before+8 })
				}
				connections := receiver.accepted.Load()
				// Повторный отзыв старого token и запоздавшее действие не получают права на новое исполнение.
				called := false
				if err = lease.Do(context.Background(), func() error { called = true; return nil }); !errors.Is(err, core.ErrStale) || called {
					t.Fatalf("старый callback получил допуск: called=%v err=%v", called, err)
				}
				if err = lease.Revoke(context.Background()); err != nil {
					t.Fatal(err)
				}
				acknowledge()
				sessionAcceptanceWait(t, func() bool {
					previous, readErr := service.GetSession(context.Background(), old.ID)
					return readErr == nil && !previous.CleanupPending && !previous.Issued
				}, func() { f.captureFailure(t) })
				ownerLossAssertHolds(t, f, old.ID, withOutput, false)
				ownerLossAssertHolds(t, f, fresh.ID, withOutput, true)
				after, err := service.GetSession(context.Background(), fresh.ID)
				if err != nil || after.State != core.SessionRunning || !after.Issued || after.Lifecycle != fresh.Lifecycle {
					t.Fatalf("поздняя retirement изменила новую Session: %+v %v", after, err)
				}
				if current, feedErr := f.manager.Feed(after); feedErr != nil || current != feed {
					t.Fatalf("поздняя retirement изменила новый Feed: %v", feedErr)
				}
				before = publisherMediaCount(receiver.snapshot())
				if withOutput {
					receiver.wait(t, func(messages []publisherWireMessage) bool { return publisherMediaCount(messages) >= before+8 })
				}
				if receiver.accepted.Load() != connections {
					t.Fatal("поздняя retirement пересоздала соединение нового выполнения")
				}
				if _, snapshotErr := service.RuntimeSnapshot(context.Background(), core.RuntimeSessionFence{SessionID: old.ID, Incarnation: old.Incarnation, Lifecycle: old.Lifecycle}); !errors.Is(snapshotErr, core.ErrNotReady) {
					t.Fatalf("retirement сохранила старую телеметрию: %v", snapshotErr)
				}
			})
		}
	}
}
