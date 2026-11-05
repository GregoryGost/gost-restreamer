package publisher

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"testing/synctest"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media/rtmp"
)

type lifecycleAcceptanceRepository struct {
	core.RuntimeRepository
	core.ArtifactRepository
	fence core.ExecutionFence
}

func (r lifecycleAcceptanceRepository) Session(context.Context, core.ID) (core.Session, error) {
	return core.Session{Record: core.Record{ID: r.fence.SessionID}, Incarnation: r.fence.Incarnation, Lifecycle: r.fence.Lifecycle, State: core.SessionRunning}, nil
}
func (r lifecycleAcceptanceRepository) Output(context.Context, core.ID) (core.SessionOutput, error) {
	return core.SessionOutput{Record: core.Record{ID: r.fence.OutputID}, SessionID: r.fence.SessionID, Attached: true, DesiredRunning: true, Issued: true, Generation: r.fence.OutputGeneration, Attempt: r.fence.Attempt}, nil
}
func (r lifecycleAcceptanceRepository) ReceiptByID(context.Context, core.ID) (core.CommandReceipt, error) {
	f := r.fence
	return core.CommandReceipt{Record: core.Record{ID: f.CommandID}, Kind: core.CommandStartOutput, Status: core.CommandSucceeded, Incarnation: f.Incarnation, SessionID: f.SessionID, Lifecycle: f.Lifecycle, OutputID: f.OutputID, OutputGeneration: f.OutputGeneration, Attempt: f.Attempt}, nil
}
func (r lifecycleAcceptanceRepository) Generations(context.Context, core.ListQuery) ([]core.FallbackGeneration, error) {
	return nil, nil
}

type lifecycleAcceptanceUOW struct {
	authority  *core.RuntimeAuthority
	repository lifecycleAcceptanceRepository
}

func (u *lifecycleAcceptanceUOW) RuntimeAuthority() *core.RuntimeAuthority { return u.authority }
func (u *lifecycleAcceptanceUOW) View(ctx context.Context, fn func(core.Repositories) error) error {
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(core.Repositories{Runtime: u.repository, Artifacts: u.repository})
}
func (u *lifecycleAcceptanceUOW) Write(context.Context, func(core.Repositories) error) error {
	return errors.New("неожиданная долговечная запись Publisher")
}

func TestPublisherAcceptanceStopDuringRealAuthorityGateWait(t *testing.T) {
	for _, activate := range []bool{false, true} {
		t.Run(map[bool]string{false: "prepared_abort", true: "pending_activate"}[activate], func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx := context.Background()
				a, err := core.NewRuntimeAuthority("synthetic-owner", core.ObservationLimits{MaxSessions: 2, MaxGroups: 2, MaxOutputs: 2, MaxFFmpegWorkers: 2, MaxIdentifierBytes: 1024, MaxErrorBytes: 1024, MaxBytes: 1 << 20}, core.OutputExecutionLimits{MaxReservations: 1, MaxIdentifierBytes: 1024, MaxBytes: 1 << 20})
				if err != nil {
					t.Fatal(err)
				}
				f := core.ExecutionFence{CommandID: "synthetic-command", Incarnation: "synthetic-owner", SessionID: "synthetic-session", Lifecycle: 1, OutputID: "synthetic-output", OutputGeneration: 1, Attempt: 1}
				u := &lifecycleAcceptanceUOW{authority: a, repository: lifecycleAcceptanceRepository{fence: f}}
				s, err := core.NewService(u, f.Incarnation)
				if err != nil {
					t.Fatal(err)
				}
				lease, err := s.BindExecutionOwner(ctx, f)
				if err != nil {
					t.Fatal(err)
				}
				reservation, err := s.ReserveOutputExecution(ctx, lease, core.OutputSnapshot{})
				if err != nil {
					t.Fatal(err)
				}
				observer, err := s.OpenOutputObserver(ctx, f)
				if err != nil {
					t.Fatal(err)
				}
				r, _, _, _ := testRouter(t)
				var dials atomic.Uint64
				p := &Publisher{router: r, execution: coreExecution{reservation: reservation}, reservation: reservation, done: make(chan struct{}), sealDone: make(chan struct{}), state: core.OutputStopped, dial: func(context.Context, rtmp.OutboundTarget, rtmp.OutboundOptions) (connection, error) {
					dials.Add(1)
					return nil, errors.New("неожиданная сеть после Stop")
				}}
				entered, release, writeDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
				go func() {
					writeDone <- a.GuardWrite(ctx, u, func() (bool, error) { close(entered); <-release; return false, nil })
				}()
				<-entered
				activationDone := make(chan error, 1)
				if activate {
					go func() { activationDone <- p.Activate(ctx, observer) }()
					synctest.Wait()
					p.mu.Lock()
					pending := p.activating
					p.mu.Unlock()
					if !pending {
						t.Fatal("Activate не дошёл до ожидаемого допуска")
					}
				}
				stopCtx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
				defer cancel()
				start := time.Now()
				err = p.Stop(stopCtx)
				if !errors.Is(err, ErrCleanupPending) || !errors.Is(err, context.DeadlineExceeded) || time.Since(start) != 10*time.Millisecond {
					t.Fatalf("Stop превысил срок: elapsed=%s err=%v", time.Since(start), err)
				}
				if p.Result().NetworkJoined || dials.Load() != 0 {
					t.Fatalf("завершение/сеть до возврата учтённого Seal: %+v dials=%d", p.Result(), dials.Load())
				}
				if err = p.ReleaseJoined(); !errors.Is(err, ErrCleanupPending) {
					t.Fatal(err)
				}
				if activate {
					if err = <-activationDone; !errors.Is(err, context.Canceled) {
						t.Fatalf("Activate после Stop: %v", err)
					}
				}
				for i := 0; i < 3; i++ {
					if err = p.Stop(stopCtx); !errors.Is(err, ErrCleanupPending) {
						t.Fatal(err)
					}
				}
				close(release)
				if err = <-writeDone; err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				result, err := p.Wait(ctx)
				if err != nil || !result.NetworkJoined || dials.Load() != 0 {
					t.Fatalf("позднее освобождение допуска/завершение: %+v err=%v dials=%d", result, err, dials.Load())
				}
				if err = p.ReleaseJoined(); !errors.Is(err, core.ErrNotReady) {
					t.Fatal("Stop/Join освободил пригодный долговечный ExecutionFence", err)
				}
				a.Close()
				if err = p.ReleaseJoined(); err != nil {
					t.Fatal(err)
				}
			})
		})
	}
}
