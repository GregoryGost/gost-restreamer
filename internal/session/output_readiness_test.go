//go:build unix

package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
)

func TestOutputReadinessMediaProcess(t *testing.T) {
	if len(os.Args) < 4 || os.Args[len(os.Args)-3] != "--" {
		return
	}
	if err := os.WriteFile(os.Args[len(os.Args)-1], []byte(strconv.Itoa(os.Getpid())), 0600); err != nil {
		t.Fatal(err)
	}
	os.Args = os.Args[:len(os.Args)-1]
	TestSessionMediaProcess(t)
}

func TestManagerOutputRechecksCurrentFallbackWithinStartupBudget(t *testing.T) {
	for _, scenario := range []string{"recovers", "deadline", "canceled", "terminal"} {
		t.Run(scenario, func(t *testing.T) {
			marker := filepath.Join(t.TempDir(), "player.pid")
			f := newManagerFixture(t, true, func(c *Config) {
				// Истекает пригодность пакетов, а не срок жизни настоящего worker.
				c.Group.FFmpeg.VideoTimeout = 15 * time.Second
				c.Group.FFmpeg.AudioTimeout = 15 * time.Second
				c.Group.Player = func(_ context.Context, _ string, _ *os.File, a ffmpeg.Attempt) (ffmpeg.Command, error) {
					binary, err := os.Executable()
					return ffmpeg.Command{Binary: binary, Args: []string{"-test.run=^TestOutputReadinessMediaProcess$", "--", a.OutputPath, marker}}, err
				}
			})
			ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
			defer cancel()
			parent, session, feed, node := startupParent(t, ctx, f)
			var neighbor *outputNode
			waitStartupCondition(t, ctx, "соседний Publisher не активирован", func() bool {
				node.mu.Lock()
				defer node.mu.Unlock()
				for _, candidate := range node.outputs {
					candidate.mu.Lock()
					active := candidate.publisher != nil && candidate.observer != nil
					candidate.mu.Unlock()
					if active {
						neighbor = candidate
						return true
					}
				}
				return false
			})
			neighbor.mu.Lock()
			neighborPublisher := neighbor.publisher
			neighbor.mu.Unlock()
			destination, err := f.service.CreateDestination(ctx, core.Destination{Name: "Дополнительный выход", StreamID: f.stream.ID, GroupID: f.binding.GroupID, Platform: "custom", Endpoint: "rtmp://127.0.0.1:1/live", StreamKey: "additional-key"})
			if err != nil {
				t.Fatal(err)
			}
			// Диспетчер ждёт, пока настоящий membership пройдёт начальный барьер.
			f.manager.mu.Lock()
			added, err := f.service.AddOutput(ctx, core.AddOutputRequest{RequestID: "current-fallback-add", SessionID: parent.SessionID, DestinationID: destination.ID, Binding: f.binding})
			if err == nil {
				f.manager.commands[added.ID] = true
			}
			f.manager.mu.Unlock()
			if err != nil {
				t.Fatal(err)
			}
			output, err := f.service.GetOutput(ctx, added.OutputID)
			if err != nil {
				t.Fatal(err)
			}
			member, err := node.acquire(ctx, output)
			if err != nil {
				t.Fatal(err)
			}
			select {
			case <-member.ready:
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			member.mu.Lock()
			ref, readyErr := member.reference, member.err
			member.mu.Unlock()
			if readyErr != nil || ref == nil || ref.Group().Ready() != nil {
				t.Fatal("membership не получил фактическую готовность", readyErr)
			}
			data, err := os.ReadFile(marker)
			if err != nil {
				t.Fatal(err)
			}
			pid, err := strconv.Atoi(string(data))
			if err != nil {
				t.Fatal(err)
			}
			if err = syscall.Kill(pid, syscall.SIGSTOP); err != nil {
				t.Fatal(err)
			}
			paused := true
			defer func() {
				if paused {
					_ = syscall.Kill(pid, syscall.SIGCONT)
				}
			}()
			waitStartupCondition(t, ctx, "готовность не отозвана после остановки пакетов", func() bool {
				return ref.Group().Ready() != nil
			})
			select {
			case <-ref.Group().Done():
				t.Fatal("вместо временной неготовности завершилась группа")
			default:
			}
			if scenario == "terminal" {
				if err = syscall.Kill(pid, syscall.SIGCONT); err != nil {
					t.Fatal(err)
				}
				paused = false
				if err = ref.Group().Stop(ctx); err != nil {
					t.Fatal(err)
				}
			}
			f.manager.mu.Lock()
			delete(f.manager.commands, added.ID)
			f.manager.mu.Unlock()
			f.manager.dispatch(added)
			if scenario == "recovers" || scenario == "canceled" {
				waitStartupCondition(t, ctx, "выход не начал проверку готовности", func() bool {
					current, err := f.service.GetReceipt(ctx, added.ID)
					if err != nil {
						t.Fatal(err)
					}
					if current.Status.Terminal() {
						t.Fatalf("временная неготовность завершила команду до восстановления: %+v", current)
					}
					return current.Status == core.CommandIssued
				})
				if scenario == "recovers" {
					// Источник восстанавливается в пределах того же срока запуска.
					timer := time.NewTimer(200 * time.Millisecond)
					defer timer.Stop()
					select {
					case <-timer.C:
					case <-ctx.Done():
						t.Fatal(ctx.Err())
					}
					if current, err := f.service.GetReceipt(ctx, added.ID); err != nil || current.Status.Terminal() {
						t.Fatal("команда не дождалась восстановления", current, err)
					}
					if err = syscall.Kill(pid, syscall.SIGCONT); err != nil {
						t.Fatal(err)
					}
					paused = false
				} else {
					node.stopOutput(added.OutputID)
				}
			}
			current := startupReceipt(t, ctx, f, added)
			if scenario == "recovers" {
				if current.Status != core.CommandSucceeded {
					t.Fatal("восстановившийся fallback не запустил выход", current)
				}
			} else if current.Status != core.CommandFailed {
				t.Fatal("непригодный fallback дал успешную квитанцию", current)
			}
			if scenario == "deadline" && current.Failure != context.DeadlineExceeded.Error() {
				t.Fatal("не сохранено истечение исходного срока", current)
			}
			if scenario == "canceled" && current.Failure != context.Canceled.Error() {
				t.Fatal("не сохранена отмена ожидания", current)
			}
			if scenario == "terminal" && !errors.Is(ref.Group().Ready(), core.ErrStale) {
				t.Fatal("терминальная группа сохранила готовность")
			}
			if currentFeed, err := f.manager.Feed(session); err != nil || currentFeed != feed {
				t.Fatal("проверка выхода отозвала Feed родителя", err)
			}
			neighbor.mu.Lock()
			preserved := neighbor.publisher == neighborPublisher
			if scenario != "terminal" {
				preserved = preserved && neighbor.failure == nil
			}
			neighbor.mu.Unlock()
			if !preserved {
				t.Fatal("проверка нового выхода заменила соседний Publisher")
			}
		})
	}
}
