package session

import (
	"context"
	"errors"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
)

type startupOutputReadStore struct {
	*storage.Store
	armed       atomic.Bool
	readFailure atomic.Bool
	rechecks    atomic.Uint32
	captured    chan core.SessionOutput
	release     chan struct{}
	once        sync.Once
	captureList bool
}

func (s *startupOutputReadStore) unblock() { s.once.Do(func() { close(s.release) }) }

func (s *startupOutputReadStore) View(ctx context.Context, callback func(core.Repositories) error) error {
	var output *core.SessionOutput
	err := s.Store.View(ctx, func(r core.Repositories) error {
		r.Runtime = startupOutputReadRepository{RuntimeRepository: r.Runtime, output: &output, captureList: s.captureList}
		return callback(r)
	})
	if err != nil || output == nil {
		return err
	}
	if s.readFailure.Load() {
		s.rechecks.Add(1)
		return core.ErrBusy
	}
	if s.armed.CompareAndSwap(true, false) {
		// Снимок уже прочитан, а транзакция и RuntimeAuthority освобождены перед Stop.
		s.captured <- *output
		select {
		case <-s.release:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return nil
}

type startupOutputReadRepository struct {
	core.RuntimeRepository
	output      **core.SessionOutput
	captureList bool
}

func (r startupOutputReadRepository) Output(ctx context.Context, id core.ID) (core.SessionOutput, error) {
	output, err := r.RuntimeRepository.Output(ctx, id)
	if err == nil {
		*r.output = &output
	}
	return output, err
}

func (r startupOutputReadRepository) Outputs(ctx context.Context, query core.ListQuery) ([]core.SessionOutput, error) {
	outputs, err := r.RuntimeRepository.Outputs(ctx, query)
	if err == nil && r.captureList && len(outputs) > 0 {
		*r.output = &outputs[0]
	}
	return outputs, err
}

func TestManagerStartupRevalidatesFailedMembershipAfterOutputRead(t *testing.T) {
	for _, test := range []struct {
		name                   string
		remaining              int
		detach, active, failed bool
	}{
		{name: "stop_last"},
		{name: "detach_last", detach: true},
		{name: "stop_shared", remaining: 1},
		{name: "detach_shared", remaining: 1, detach: true},
		{name: "active_failure", active: true},
		{name: "unconfirmed_exclusion", failed: true},
	} {
		t.Run(test.name, func(t *testing.T) {
			entered, releasePlayer := make(chan struct{}), make(chan struct{})
			var playerEntered, playerReleased sync.Once
			unblockPlayer := func() { playerReleased.Do(func() { close(releasePlayer) }) }
			f := newManagerFixture(t, true, func(c *Config) {
				original := c.Group.Player
				c.Group.Player = func(ctx context.Context, binary string, file *os.File, attempt ffmpeg.Attempt) (ffmpeg.Command, error) {
					playerEntered.Do(func() { close(entered) })
					select {
					case <-releasePlayer:
						return original(ctx, binary, file, attempt)
					case <-ctx.Done():
						return ffmpeg.Command{}, ctx.Err()
					}
				}
			})
			wrapped := &startupOutputReadStore{Store: f.store, captured: make(chan core.SessionOutput, 1), release: make(chan struct{})}
			f = replaceAdmissionManager(t, f, wrapped)
			t.Cleanup(func() { wrapped.unblock(); unblockPlayer() })
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			if test.remaining > 0 {
				neighbor := f.destination
				neighbor.Record = core.Record{}
				neighbor.Name = "Соседний выход"
				if _, err := f.service.CreateDestination(ctx, neighbor); err != nil {
					t.Fatal(err)
				}
			}
			start, err := f.manager.StartSession(ctx, core.StartSessionRequest{RequestID: "startup-output-read", StreamID: f.stream.ID})
			if err != nil {
				t.Fatal(err)
			}
			awaitPublication(t, ctx, entered)
			wrapped.armed.Store(true)
			var before core.SessionOutput
			select {
			case before = <-wrapped.captured:
			case <-ctx.Done():
				t.Fatal("барьер не прочитал Output", ctx.Err())
			}
			if !before.Attached || !before.DesiredRunning || before.CleanupPending {
				t.Fatal("задержан неактивный снимок", before)
			}
			starting, err := f.service.GetSession(ctx, start.SessionID)
			if err != nil || starting.State != core.SessionStarting {
				t.Fatal("барьер не удерживает STARTING", starting, err)
			}
			f.manager.mu.Lock()
			node := f.manager.sessions[start.SessionID]
			f.manager.mu.Unlock()
			node.mu.Lock()
			member := node.members[before.ID]
			members := len(node.members)
			node.mu.Unlock()
			if member == nil || members != test.remaining+1 {
				t.Fatal("не все выбранные memberships учтены", members)
			}
			waitStartupCondition(t, ctx, "не выданы ссылки выбранных выходов", func() bool {
				node.mu.Lock()
				defer node.mu.Unlock()
				for _, candidate := range node.members {
					candidate.mu.Lock()
					ref := candidate.reference
					candidate.mu.Unlock()
					if ref == nil {
						return false
					}
				}
				return true
			})
			var stopped core.CommandReceipt
			if test.active {
				// Ошибка активного membership без долговечного Stop остаётся общим отказом.
				member.stop()
			} else {
				command := core.OutputCommand{RequestID: "stop-after-output-read", OutputID: before.ID}
				if test.detach {
					stopped, err = f.manager.DetachOutput(ctx, command)
				} else {
					stopped, err = f.manager.StopOutput(ctx, command)
				}
				if err != nil {
					t.Fatal(err)
				}
				after, err := f.service.GetOutput(ctx, before.ID)
				if err != nil || after.DesiredRunning || after.Attached == test.detach || after.Generation == before.Generation {
					t.Fatal("Stop/Detach не зафиксирован после старого чтения", after, err)
				}
				if !reflect.DeepEqual(before.Snapshot, after.Snapshot) {
					t.Fatal("Stop/Detach переписал снимок выхода")
				}
			}
			awaitPublication(t, ctx, member.ready)
			member.mu.Lock()
			memberErr, stoppedRef := member.err, member.reference
			member.mu.Unlock()
			if !errors.Is(memberErr, context.Canceled) {
				t.Fatal("старый снимок не пересёк отмену membership", memberErr)
			}
			wrapped.readFailure.Store(test.failed)
			wrapped.unblock()
			unblockPlayer()
			current := waitReceipt(t, f, start)
			want := core.CommandSucceeded
			if test.active || test.failed {
				want = core.CommandFailed
			}
			if current.Status != want {
				t.Fatalf("неверный результат parent после отмены membership: %+v; ожидался %s", current, want)
			}
			if test.failed && (wrapped.rechecks.Load() == 0 || !strings.Contains(current.Failure, core.ErrBusy.Error())) {
				t.Fatal("ошибка повторного чтения не сохранилась", current.Failure)
			}
			actual, err := f.service.GetSession(ctx, start.SessionID)
			if err != nil || !reflect.DeepEqual(actual.Snapshot, starting.Snapshot) {
				t.Fatal("изменён первоначальный снимок Session", err)
			}
			if want == core.CommandFailed {
				if actual.State != core.SessionFailed {
					t.Fatal("общий отказ не завершил Session", actual)
				}
				return
			}
			if actual.State != core.SessionRunning {
				t.Fatal("исключение Output остановило Session", actual)
			}
			waitStartupCondition(t, ctx, "RUNNING не допускает Feed", func() bool {
				router, err := f.manager.Feed(actual)
				return err == nil && router != nil
			})
			if current := waitReceipt(t, f, stopped); current.Status != core.CommandSucceeded {
				t.Fatal("исключённый Output не очищен", current)
			}
			outputs, err := f.service.ListOutputs(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8, SessionID: start.SessionID}})
			if err != nil || len(outputs.Items) != test.remaining+1 {
				t.Fatal("изменён состав истории Outputs", outputs, err)
			}
			for _, output := range outputs.Items {
				if output.ID == before.ID {
					if output.DesiredRunning || output.CleanupPending || output.Attached == test.detach {
						t.Fatal("исключённый Output не сохранил результат Stop/Detach", output)
					}
					continue
				}
				if !output.Attached || !output.DesiredRunning {
					t.Fatal("остановлен здоровый сосед", output)
				}
				node.mu.Lock()
				neighbor := node.members[output.ID]
				node.mu.Unlock()
				if neighbor == nil {
					t.Fatal("нет membership здорового соседа")
				}
				neighbor.mu.Lock()
				ref := neighbor.reference
				neighbor.mu.Unlock()
				if ref == nil || ref.Group().Ready() != nil {
					t.Fatal("сосед не достиг фактической fallback-готовности")
				}
				if stoppedRef == nil || ref.Group() != stoppedRef.Group() {
					t.Fatal("выходы не разделяют одну локальную группу")
				}
			}
		})
	}
}

func TestManagerStartupExcludesOutputStoppedBeforeMembershipAcquisition(t *testing.T) {
	f := newManagerFixture(t, true, nil)
	wrapped := &startupOutputReadStore{Store: f.store, captured: make(chan core.SessionOutput, 1), release: make(chan struct{}), captureList: true}
	f = replaceAdmissionManager(t, f, wrapped)
	t.Cleanup(wrapped.unblock)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	wrapped.armed.Store(true)
	start, err := f.manager.StartSession(ctx, core.StartSessionRequest{RequestID: "stop-before-membership", StreamID: f.stream.ID})
	if err != nil {
		t.Fatal(err)
	}
	var selected core.SessionOutput
	select {
	case selected = <-wrapped.captured:
	case <-ctx.Done():
		t.Fatal("initialize не прочитал выбранный Output", ctx.Err())
	}
	f.manager.mu.Lock()
	node := f.manager.sessions[start.SessionID]
	f.manager.mu.Unlock()
	node.mu.Lock()
	members := len(node.members)
	node.mu.Unlock()
	if members != 0 {
		t.Fatal("membership уже создан до остановки", members)
	}
	stop, err := f.manager.StopOutput(ctx, core.OutputCommand{RequestID: "stop-before-acquisition", OutputID: selected.ID})
	if err != nil {
		t.Fatal(err)
	}
	if current := waitReceipt(t, f, stop); current.Status != core.CommandSucceeded {
		t.Fatal("остановка до Acquire не завершена", current)
	}
	node.mu.Lock()
	excluded := node.excluded[selected.ID]
	node.mu.Unlock()
	if excluded != selected.Generation {
		t.Fatal("очистка не исключила точное первоначальное поколение", excluded, selected.Generation)
	}
	wrapped.unblock()
	if current := waitReceipt(t, f, start); current.Status != core.CommandSucceeded {
		t.Fatal("ошибка Acquire исключённого поколения остановила parent", current)
	}
	output, err := f.service.GetOutput(ctx, selected.ID)
	if err != nil || output.DesiredRunning || output.CleanupPending || !reflect.DeepEqual(output.Snapshot, selected.Snapshot) {
		t.Fatal("остановка до Acquire утратила состояние выхода", output, err)
	}
}
