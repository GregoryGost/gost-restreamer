package group

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"reflect"
	"runtime"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
	"github.com/GregoryGost/gost-restreamer/internal/monitoring/logging"
)

func TestFFmpegDiagnosticsKeepWorkerIdentityAcrossQueuedAttempts(t *testing.T) {
	for _, delayed := range []bool{false, true} {
		t.Run(map[bool]string{false: "continuous_drain", true: "queued_old_attempt"}[delayed], func(t *testing.T) {
			f := runtimeUnitFixture(t)
			parentDirectory := os.TempDir()
			if runtime.GOOS == "darwin" {
				parentDirectory = "/tmp"
			}
			base, err := os.MkdirTemp(parentDirectory, "group-diagnostic-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := os.RemoveAll(base); err != nil {
					t.Error(err)
				}
			})
			f.config.Runtime.FFmpeg.BaseDir = base
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			owner, err := f.config.Core.BindExecutionOwner(ctx, f.config.Parent)
			if err != nil {
				t.Fatal(err)
			}
			groupObserver, err := f.config.Observer.OpenGroup(ctx, f.snapshot.Group.ID, f.snapshot.Profile.ID)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = groupObserver.Close(context.Background()) })
			var output bytes.Buffer
			sink, err := logging.New(logging.Config{MaxRecords: 64, MaxBytes: 1 << 20, MaxRecordBytes: 65536, ShutdownTimeout: time.Second, Level: slog.LevelInfo}, &output)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = sink.Close(context.Background()) })
			sw, _, _, _ := switcherFixture(t)
			g := &Group{config: f.config.Runtime, parent: f.config.Parent, id: f.snapshot.Group.ID, observer: groupObserver, switcher: sw}
			g.config.Logger = sink.Logger().With("stream_id", "synthetic-stream")
			type worker struct {
				supervisor *ffmpeg.Supervisor
				observer   *core.FFmpegObserver
				logger     *slog.Logger
				binding    atomic.Pointer[attemptBinding]
				drained    <-chan struct{}
				role       core.FFmpegRole
				coreStart  uint64
			}
			workers := [2]*worker{{role: core.FFmpegFallbackPlayer, coreStart: 17}, {role: core.FFmpegLiveTranscode, coreStart: 29}}
			started, release := make(chan struct{}, 2), make(chan struct{})
			argv := []string{"/bin/sh", "-c", "printf '\\377\\000\\376payload'; printf 'synthetic-key=<full>&url=?token=whole' >&2; exit 1", "synthetic-argv", "rtmp://example.invalid/live?token=whole&key=full"}
			for _, w := range workers {
				w.observer, err = groupObserver.OpenFFmpegRole(ctx, w.role)
				if err != nil {
					t.Fatal(err)
				}
				w.logger = g.ffmpegLogger(w.observer, w.role)
				config := f.config.Runtime.FFmpeg
				config.MaxRestarts = 1
				w.supervisor, err = ffmpeg.New(config)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = w.supervisor.Stop(context.Background()) })
				if !delayed {
					w.drained = g.drainDiagnostics(w.supervisor, w.logger)
				}
				err = w.supervisor.Start(ctx, ffmpeg.Worker{SessionID: string(g.parent.SessionID), GroupID: string(g.id), Owner: owner,
					Input:  func(context.Context, net.Conn, ffmpeg.Progress) error { return nil },
					Output: func(context.Context, net.Conn, ffmpeg.Progress) error { return nil },
					Command: func(commandCtx context.Context, a ffmpeg.Attempt) (ffmpeg.Command, error) {
						ao, beginErr := w.observer.BeginAttempt(commandCtx, w.coreStart+a.ID-1)
						if beginErr != nil {
							return ffmpeg.Command{}, beginErr
						}
						w.binding.Store(bindFFmpegAttempt(a.ID, w.coreStart+a.ID-1, ao, w.logger))
						if a.ID == 1 {
							started <- struct{}{}
							select {
							case <-release:
							case <-commandCtx.Done():
								return ffmpeg.Command{}, commandCtx.Err()
							}
						}
						return ffmpeg.Command{Binary: argv[0], Args: argv[1:]}, nil
					}})
				if err != nil {
					t.Fatal(err)
				}
			}
			for range workers {
				select {
				case <-started:
				case <-ctx.Done():
					for _, w := range workers {
						select {
						case <-w.supervisor.Done():
							result, _ := w.supervisor.Wait(context.Background())
							t.Logf("исход %s до команды: %+v", w.role, result)
						default:
						}
					}
					t.Fatal(ctx.Err())
				}
			}
			close(release)
			for _, w := range workers {
				result, waitErr := w.supervisor.Wait(ctx)
				if waitErr != nil || result.Attempts != 2 || result.CleanupErr != nil || result.Err == nil {
					t.Fatalf("неверное завершение worker: %+v %v", result, waitErr)
				}
				if delayed {
					w.drained = g.drainDiagnostics(w.supervisor, w.logger)
				}
				select {
				case <-w.drained:
				case <-ctx.Done():
					t.Fatal(ctx.Err())
				}
				g.finishFFmpegObservation(w.binding.Load(), errors.New("синтетическая ошибка ожидания"), false)
				reader, writer := net.Pipe()
				_ = writer.Close()
				err = g.readOutputObserved(ctx, reader, Live, w.binding.Load().coreID, core.MediaFormat{}, func(ffmpeg.Track, int64) bool { return true }, w.binding.Load())
				_ = reader.Close()
				if err == nil {
					t.Fatal("пустой fMP4 не вернул ошибку")
				}
			}
			if err = sink.Close(ctx); err != nil {
				t.Fatal(err)
			}
			decoder := json.NewDecoder(&output)
			seen := make(map[core.FFmpegRole]map[uint64]map[string]bool)
			for {
				var record map[string]any
				if err = decoder.Decode(&record); errors.Is(err, io.EOF) {
					break
				} else if err != nil {
					t.Fatal(err)
				}
				role := core.FFmpegRole(record["worker_role"].(string))
				var expected *worker
				for _, w := range workers {
					if role == w.role {
						expected = w
					}
				}
				if expected == nil || record["worker_instance_id"] != float64(expected.observer.Token()) || record["group_instance_id"] != float64(groupObserver.Token()) || record["stream_id"] != "synthetic-stream" || record["session_id"] != string(g.parent.SessionID) || record["group_id"] != string(g.id) {
					t.Fatal("утрачена точная принадлежность записи")
				}
				attempt := uint64(record["attempt"].(float64))
				operation := record["operation"].(string)
				if attempt != 1 && attempt != 2 {
					t.Fatal("подменён локальный номер попытки", attempt)
				}
				if seen[role] == nil {
					seen[role] = make(map[uint64]map[string]bool)
				}
				if seen[role][attempt] == nil {
					seen[role][attempt] = make(map[string]bool)
				}
				seen[role][attempt][operation] = true
				if record["msg"] != "ffmpeg_diagnostic" {
					if record["core_attempt"] != float64(expected.coreStart+attempt-1) {
						t.Fatal("подменён core attempt")
					}
					continue
				}
				if _, present := record["core_attempt"]; present {
					t.Fatal("диагностика получила изменяемую текущую core attempt")
				}
				if operation == "command" {
					var got []string
					for _, arg := range record["argv"].([]any) {
						got = append(got, arg.(string))
					}
					if !reflect.DeepEqual(got, argv) {
						t.Fatal("изменены полные argv")
					}
					continue
				}
				if record["sequence"] != float64(1) {
					t.Fatal("изменена последовательность чанков")
				}
				data := record["data"].(map[string]any)
				decoded, decodeErr := base64.StdEncoding.DecodeString(data["bytes"].(string))
				want := []byte{0xff, 0, 0xfe, 'p', 'a', 'y', 'l', 'o', 'a', 'd'}
				if operation == "stderr" {
					want = []byte("synthetic-key=<full>&url=?token=whole")
				}
				if decodeErr != nil || data["encoding"] != "base64" || !bytes.Equal(decoded, want) {
					t.Fatal("изменена полная бинарная диагностика")
				}
			}
			for _, w := range workers {
				for attempt := uint64(1); attempt <= 2; attempt++ {
					for _, operation := range []string{"begin_attempt", "command", "stdout", "stderr"} {
						if !seen[w.role][attempt][operation] {
							t.Fatal("нет записи", w.role, attempt, operation)
						}
					}
				}
				for _, operation := range []string{"wait", "read_output"} {
					if !seen[w.role][2][operation] {
						t.Fatal("нет коррелированной ошибки", w.role, operation)
					}
				}
			}
		})
	}
}
