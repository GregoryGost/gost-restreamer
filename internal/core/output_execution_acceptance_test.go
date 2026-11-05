package core_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
)

type executionAcceptanceUOW struct {
	*storage.Store
	viewErr error
	views   int
}

func (u *executionAcceptanceUOW) View(ctx context.Context, f func(core.Repositories) error) error {
	u.views++
	if u.viewErr != nil {
		return u.viewErr
	}
	return u.Store.View(ctx, f)
}

type executionAcceptanceFixture struct {
	u         *executionAcceptanceUOW
	s, other  *core.Service
	fences    []core.ExecutionFence
	snapshots []core.OutputSnapshot
}

func newExecutionAcceptanceFixture(t *testing.T, count, budget int) executionAcceptanceFixture {
	t.Helper()
	ctx := context.Background()
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "execution.sqlite"), storage.Options{
		OutputExecutionLimits: core.OutputExecutionLimits{MaxReservations: budget, MaxIdentifierBytes: 1024, MaxBytes: 1 << 20},
		PreparationLimits:     core.PreparationLimits{MaxQueued: 8, MaxProfiles: 8},
		ObservationLimits:     core.ObservationLimits{MaxSessions: 8, MaxGroups: 8, MaxOutputs: 16, MaxFFmpegWorkers: 8, MaxIdentifierBytes: 1024, MaxErrorBytes: 4096, MaxBytes: 1 << 20},
		MaxOpenConns:          4, MaxIdleConns: 4, ConnMaxLifetime: time.Minute, ConnMaxIdleTime: time.Minute,
		BusyTimeout: time.Millisecond * 10, RetryAttempts: 3, RetryTimeout: time.Second, RetryDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	d, err := store.Diagnostics(ctx)
	if err != nil || d.SchemaVersion != 1 || d.SQLiteVersion == "" {
		t.Fatalf("Проверка рабочего runner Goose/SQLite: %+v, %v", d, err)
	}
	f := executionAcceptanceFixture{u: &executionAcceptanceUOW{Store: store}}
	f.s, err = core.NewService(f.u, store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	f.other, err = core.NewService(f.u, store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	sessionSnapshot, _ := contractSnapshots(t)
	session := core.Session{Record: contractRecord("execution-session"), Issued: true, StreamID: sessionSnapshot.Stream.ID, Incarnation: store.Incarnation(), State: core.SessionRunning, Lifecycle: 1, Snapshot: sessionSnapshot}
	err = store.Write(ctx, func(r core.Repositories) error {
		if err := r.Runtime.InsertSession(ctx, session); err != nil {
			return err
		}
		for i := 0; i < count; i++ {
			_, snapshot := contractSnapshots(t)
			id := core.ID(fmt.Sprintf("execution-output-%d", i))
			o := core.SessionOutput{Record: contractRecord(id), SessionID: session.ID, Snapshot: snapshot, Attached: true, DesiredRunning: true, Generation: 1}
			if err := r.Runtime.InsertOutput(ctx, o); err != nil {
				return err
			}
			identity := []byte(fmt.Sprintf("synthetic-start-%d", i))
			sum := sha256.Sum256(identity)
			c := core.CommandReceipt{Record: contractRecord(core.ID(fmt.Sprintf("execution-command-%d", i))), RequestID: string(identity), Kind: core.CommandStartOutput, TargetID: id, Identity: identity, Fingerprint: hex.EncodeToString(sum[:]), SessionID: session.ID, OutputID: id, Incarnation: store.Incarnation(), Lifecycle: 1, OutputGeneration: 1, Status: core.CommandPending}
			if err := r.Runtime.InsertReceipt(ctx, c); err != nil {
				return err
			}
			f.snapshots = append(f.snapshots, snapshot)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	for i := 0; i < count; i++ {
		fence, err := f.s.Claim(ctx, fmt.Sprintf("synthetic-start-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		if err := f.s.MarkIssued(ctx, fence); err != nil {
			t.Fatal(err)
		}
		f.fences = append(f.fences, fence)
	}
	return f
}

func (f executionAcceptanceFixture) reserve(t *testing.T, i int) (*core.RuntimeLease, *core.OutputExecutionReservation) {
	t.Helper()
	l, err := f.s.BindExecutionOwner(context.Background(), f.fences[i])
	if err != nil {
		t.Fatal(err)
	}
	r, err := f.s.ReserveOutputExecution(context.Background(), l, f.snapshots[i])
	if err != nil {
		t.Fatal(err)
	}
	return l, r
}

func (f executionAcceptanceFixture) activate(t *testing.T, i int, r *core.OutputExecutionReservation) *core.OutputObserver {
	t.Helper()
	ctx := context.Background()
	if err := f.s.Complete(ctx, f.fences[i], core.Completion{Success: true}); err != nil {
		t.Fatal(err)
	}
	o, err := f.s.OpenOutputObserver(ctx, f.fences[i])
	if err != nil {
		t.Fatal(err)
	}
	if err := r.Activate(ctx, o); err != nil {
		t.Fatal(err)
	}
	return o
}

func requireExecutionError(t *testing.T, got, want error) {
	t.Helper()
	if !errors.Is(got, want) {
		t.Fatalf("ошибка %v, ожидалась %v", got, want)
	}
}

func TestOutputExecutionAcceptanceAuthoritativeSnapshot(t *testing.T) {
	f := newExecutionAcceptanceFixture(t, 1, 1)
	l, err := f.s.BindExecutionOwner(context.Background(), f.fences[0])
	if err != nil {
		t.Fatal(err)
	}
	// Каждое значение относится к принятому снимку, включая поля вне сетевого назначения.
	for _, tc := range []struct {
		name   string
		mutate func(*core.OutputSnapshot)
	}{
		{"destination_name", func(v *core.OutputSnapshot) { v.Destination.Name += "x" }},
		{"destination_revision", func(v *core.OutputSnapshot) { v.Destination.Revision++ }},
		{"destination_endpoint", func(v *core.OutputSnapshot) { v.Destination.Endpoint += "?x" }},
		{"destination_key", func(v *core.OutputSnapshot) { v.Destination.StreamKey += "x" }},
		{"group", func(v *core.OutputSnapshot) { v.Group.Name += "x" }},
		{"profile", func(v *core.OutputSnapshot) { v.Profile.Profile.VideoEncoding.GOPFrames++ }},
		{"fallback", func(v *core.OutputSnapshot) { v.Fallback.Watchdog.AudioTimeout++ }},
		{"artifact_path", func(v *core.OutputSnapshot) { v.Artifact.ManagedPath += "x" }},
		{"artifact_size", func(v *core.OutputSnapshot) { v.Artifact.SizeBytes++ }},
		{"artifact_hash", func(v *core.OutputSnapshot) { v.Artifact.ContentHash += "x" }},
		{"artifact_source", func(v *core.OutputSnapshot) { v.Artifact.SourceHash += "x" }},
		{"artifact_generation", func(v *core.OutputSnapshot) { v.Artifact.GenerationID += "x" }},
		{"artifact_format", func(v *core.OutputSnapshot) { v.Artifact.VerifiedFormat.Video.Width++ }},
		{"canonical_bytes", func(v *core.OutputSnapshot) { v.Artifact.CanonicalProfile[0] ^= 1 }},
		{"canonical_absence", func(v *core.OutputSnapshot) { v.Artifact.CanonicalProfile = nil }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			v := f.snapshots[0]
			v.Artifact.CanonicalProfile = bytes.Clone(v.Artifact.CanonicalProfile)
			tc.mutate(&v)
			_, err := f.other.ReserveOutputExecution(context.Background(), l, v)
			requireExecutionError(t, err, core.ErrStale)
		})
	}
	r, err := f.other.ReserveOutputExecution(context.Background(), l, f.snapshots[0])
	if err != nil {
		t.Fatal(err)
	}
	if r.ExecutionFence() != f.fences[0] {
		t.Fatal("изменён точный ExecutionFence")
	}
	_, err = f.s.ReserveOutputExecution(context.Background(), l, f.snapshots[0])
	requireExecutionError(t, err, core.ErrInUse)
}

func TestOutputExecutionAcceptanceQuarantineCannotReplay(t *testing.T) {
	for _, cause := range []string{"revoke", "replace_observer", "close_observer", "uncertain_reconciliation"} {
		t.Run(cause, func(t *testing.T) {
			ctx := context.Background()
			f := newExecutionAcceptanceFixture(t, 2, 1)
			l, r := f.reserve(t, 0)
			o := f.activate(t, 0, r)
			admitted, err := r.Admit(ctx, core.OutputOperationEstablishment)
			if err != nil {
				t.Fatal(err)
			}
			switch cause {
			case "revoke":
				if err = l.Revoke(ctx); err != nil {
					t.Fatal(err)
				}
			case "replace_observer":
				_, err = f.other.OpenOutputObserver(ctx, f.fences[0])
				if err != nil {
					t.Fatal(err)
				}
			case "close_observer":
				if err = o.Close(ctx); err != nil {
					t.Fatal(err)
				}
			case "uncertain_reconciliation":
				uncertain := errors.New("синтетический неоднозначный COMMIT")
				f.u.viewErr = errors.New("синтетический отказ чтения")
				err = f.u.RuntimeAuthority().GuardWrite(ctx, f.u, func() (bool, error) { return true, uncertain })
				requireExecutionError(t, err, uncertain)
				if l.Valid() || o.Valid() {
					t.Fatal("ошибка сверки не закрыла допуск")
				}
				f.u.viewErr = nil
			}
			select {
			case <-r.Done():
			default:
				t.Fatal("отзыв не запечатал резервирование")
			}
			_, err = r.Admit(ctx, core.OutputOperationWrite)
			requireExecutionError(t, err, core.ErrStale)
			fresh, err := f.other.BindExecutionOwner(ctx, f.fences[0])
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.other.OpenOutputObserver(ctx, f.fences[0])
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.other.ReserveOutputExecution(ctx, fresh, f.snapshots[0])
			requireExecutionError(t, err, core.ErrInUse)
			requireExecutionError(t, f.other.ConfirmOutputExecutionResolution(ctx, r), core.ErrNotReady)
			admitted.Finish()
			requireExecutionError(t, r.ReleaseJoined(), core.ErrNotReady)
			otherLease, err := f.s.BindExecutionOwner(ctx, f.fences[1])
			if err != nil {
				t.Fatal(err)
			}
			_, err = f.s.ReserveOutputExecution(ctx, otherLease, f.snapshots[1])
			requireExecutionError(t, err, core.ErrOutputExecutionLimit)
			_, err = f.other.StopOutput(ctx, core.OutputCommand{RequestID: "resolve-output", OutputID: f.fences[0].OutputID})
			if err != nil {
				t.Fatal(err)
			}
			readFailure := errors.New("синтетический отказ authoritative View")
			f.u.viewErr = readFailure
			requireExecutionError(t, f.s.ConfirmOutputExecutionResolution(ctx, r), readFailure)
			requireExecutionError(t, r.ReleaseJoined(), core.ErrNotReady)
			f.u.viewErr = nil
			if err = f.s.ConfirmOutputExecutionResolution(ctx, r); err != nil {
				t.Fatal(err)
			}
			views := f.u.views
			f.u.viewErr = readFailure
			if err = r.ReleaseJoined(); err != nil {
				t.Fatal(err)
			}
			if err = r.ReleaseJoined(); err != nil {
				t.Fatal(err)
			}
			if f.u.views != views {
				t.Fatal("освобождение памяти вызвало SQL")
			}
			f.u.viewErr = nil
			if _, err = f.s.ReserveOutputExecution(ctx, otherLease, f.snapshots[1]); err != nil {
				t.Fatal("бюджет после подтверждения разрешения не вернулся", err)
			}
			oState, err := f.s.GetOutput(ctx, f.fences[0].OutputID)
			if err != nil {
				t.Fatal(err)
			}
			if !oState.CleanupPending || !oState.Issued {
				t.Fatal("освобождение памяти выдумало очистку родительских ресурсов")
			}
		})
	}
}

func TestOutputExecutionAcceptanceSlotsAndCloseJoin(t *testing.T) {
	ctx := context.Background()
	f := newExecutionAcceptanceFixture(t, 1, 1)
	_, r := f.reserve(t, 0)
	_, err := r.Admit(ctx, core.OutputOperationWrite)
	requireExecutionError(t, err, core.ErrNotReady)
	f.activate(t, 0, r)
	var slots []*core.OutputOperation
	for _, kind := range []core.OutputOperationKind{core.OutputOperationEstablishment, core.OutputOperationRead, core.OutputOperationWrite} {
		op, err := r.Admit(ctx, kind)
		if err != nil {
			t.Fatal(err)
		}
		slots = append(slots, op)
		_, err = r.Admit(ctx, kind)
		requireExecutionError(t, err, core.ErrInUse)
	}
	slots[2].Finish()
	next, err := r.Admit(ctx, core.OutputOperationWrite)
	if err != nil {
		t.Fatal(err)
	}
	slots[2].Finish()
	_, err = r.Admit(ctx, core.OutputOperationWrite)
	requireExecutionError(t, err, core.ErrInUse)
	f.u.RuntimeAuthority().Close()
	requireExecutionError(t, r.ReleaseJoined(), core.ErrNotReady)
	var wg sync.WaitGroup
	for _, op := range append(slots, next) {
		wg.Go(func() { op.Finish(); op.Finish() })
	}
	wg.Wait()
	if err = r.ReleaseJoined(); err != nil {
		t.Fatal(err)
	}
	_, err = r.Admit(ctx, core.OutputOperationRead)
	requireExecutionError(t, err, core.ErrClosed)
}

func TestOutputExecutionAcceptanceFailureBeforeComplete(t *testing.T) {
	ctx := context.Background()
	f := newExecutionAcceptanceFixture(t, 1, 1)
	_, r := f.reserve(t, 0)
	_, err := f.s.OpenOutputObserver(ctx, f.fences[0])
	requireExecutionError(t, err, core.ErrStale)
	r.Seal()
	requireExecutionError(t, r.ReleaseJoined(), core.ErrNotReady)
	if err = f.s.Complete(ctx, f.fences[0], core.Completion{Failure: "локальная подготовка отменена"}); err != nil {
		t.Fatal(err)
	}
	if err = f.other.ConfirmOutputExecutionResolution(ctx, r); err != nil {
		t.Fatal(err)
	}
	if err = r.ReleaseJoined(); err != nil {
		t.Fatal(err)
	}
	_, err = f.s.BindExecutionOwner(ctx, f.fences[0])
	requireExecutionError(t, err, core.ErrStale)
}
