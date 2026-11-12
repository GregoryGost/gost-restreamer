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

// Проверяет точную принадлежность бинарной диагностики и полных argv двум работникам и локальным
// попыткам при непрерывном и отложенном чтении, включая корреляцию итоговых ошибок.
func TestFFmpegDiagnosticsKeepWorkerIdentityAcrossQueuedAttempts(t *testing.T) {
	// delayed выбирает чтение диагностики после обеих попыток вместо непрерывного дочитывания.
	for _, delayed := range []bool{false, true} {
		t.Run(map[bool]string{false: "continuous_drain", true: "queued_old_attempt"}[delayed], func(t *testing.T) {
			// Проверяет неизменность принадлежности диагностики двух ролей FFmpeg при непрерывном чтении либо
			// чтении очереди после двух попыток.

			// f предоставляет точное доменное выполнение и настройки тестовой группы.
			f := runtimeUnitFixture(t)
			// parentDirectory задаёт короткий временный корень диагностики с учётом UDS на
			// Darwin.
			parentDirectory := os.TempDir()
			if runtime.GOOS == "darwin" {
				parentDirectory = "/tmp"
			}
			// base содержит каталог работников диагностики; err сохраняет результаты
			// создания ресурсов и выполнения сценария.
			base, err := os.MkdirTemp(parentDirectory, "group-diagnostic-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				// Удаляет временный каталог диагностики и сообщает ошибку через t.Error.

				// err сообщает об отказе удаления временного каталога диагностики после
				// теста.
				if err := os.RemoveAll(base); err != nil {
					t.Error(err)
				}
			})
			f.config.Runtime.FFmpeg.BaseDir = base
			// ctx ограничивает сценарий двух работников десятью секундами; cancel
			// освобождает таймер.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			// owner содержит допуск точного выполнения Session для обоих работников.
			owner, err := f.config.Core.BindExecutionOwner(ctx, f.config.Parent)
			if err != nil {
				t.Fatal(err)
			}
			// groupObserver содержит наблюдателя общей группы, открывающего разные
			// FFmpeg-роли.
			groupObserver, err := f.config.Observer.OpenGroup(ctx, f.snapshot.Group.ID, f.snapshot.Profile.ID)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				// Закрывает наблюдателя группы с фоновым контекстом, игнорируя ошибку.
				_ = groupObserver.Close(context.Background())
			})
			// output накапливает JSON-записи полного диагностического журнала.
			var output bytes.Buffer
			// sink сохраняет полный структурированный журнал в контрольный буфер с явными
			// пределами.
			sink, err := logging.New(logging.Config{MaxRecords: 64, MaxBytes: 1 << 20, MaxRecordBytes: 65536, ShutdownTimeout: time.Second, Level: slog.LevelInfo}, &output)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				// Закрывает приёмник журнала с фоновым контекстом, игнорируя ошибку резервного закрытия.
				_ = sink.Close(context.Background())
			})
			// sw содержит переключатель для проверки корреляции ошибок выходного fMP4.
			sw, _, _, _ := switcherFixture(t)
			// g содержит группу с точными идентификаторами и выбранным приёмником журнала.
			g := &Group{config: f.config.Runtime, parent: f.config.Parent, id: f.snapshot.Group.ID, observer: groupObserver, switcher: sw}
			g.config.Logger = sink.Logger().With("stream_id", "synthetic-stream")
			// worker объединяет ресурсы, привязку попыток и ожидаемую идентичность одной
			// FFmpeg-роли теста.
			type worker struct {
				// supervisor выполняет две физические попытки проверяемой роли.
				supervisor *ffmpeg.Supervisor
				// observer содержит наблюдателя конкретного экземпляра FFmpeg-работника.
				observer *core.FFmpegObserver
				// logger добавляет точную идентичность экземпляра и роли в записи.
				logger *slog.Logger
				// binding атомарно связывает текущую локальную попытку с доменным
				// номером наблюдения.
				binding atomic.Pointer[attemptBinding]
				// drained сообщает о полном дочитывании диагностики данного Supervisor.
				drained <-chan struct{}
				// role задаёт проигрыватель заглушки либо LIVE-перекодировщик.
				role core.FFmpegRole
				// coreStart задаёт начальный доменный номер, отличный у двух работников
				// и от локальных номеров.
				coreStart uint64
			}
			// workers содержит двух работников с разными ролями и диапазонами доменных
			// попыток.
			workers := [2]*worker{{role: core.FFmpegFallbackPlayer, coreStart: 17}, {role: core.FFmpegLiveTranscode, coreStart: 29}}
			// started сообщает о входе первой команды каждого работника; release разрешает
			// обеим командам продолжить выполнение.
			started, release := make(chan struct{}, 2), make(chan struct{})
			// argv задаёт полный контрольный запуск с бинарным stdout, синтетическими
			// данными stderr и буквальным адресом в аргументах.
			argv := []string{"/bin/sh", "-c", "printf '\\377\\000\\376payload'; printf 'synthetic-key=<full>&url=?token=whole' >&2; exit 1", "synthetic-argv", "rtmp://example.invalid/live?token=whole&key=full"}
			// w содержит работника, чьи наблюдатель, Supervisor и привязка настраиваются и
			// запускаются.
			for _, w := range workers {
				w.observer, err = groupObserver.OpenFFmpegRole(ctx, w.role)
				if err != nil {
					t.Fatal(err)
				}
				w.logger = g.ffmpegLogger(w.observer, w.role)
				// config разрешает одну повторную физическую попытку проверяемой роли.
				config := f.config.Runtime.FFmpeg
				config.MaxRestarts = 1
				w.supervisor, err = ffmpeg.New(config)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					// Останавливает Supervisor данной роли с фоновым контекстом, игнорируя ошибку.
					_ = w.supervisor.Stop(context.Background())
				})
				if !delayed {
					w.drained = g.drainDiagnostics(w.supervisor, w.logger)
				}
				err = w.supervisor.Start(ctx, ffmpeg.Worker{SessionID: string(g.parent.SessionID), GroupID: string(g.id), Owner: owner,
					Input: func(context.Context, net.Conn, ffmpeg.Progress) error {
						// Сразу завершает условный вход: сценарий проверяет диагностику команд без медиаподачи.
						return nil
					},
					Output: func(context.Context, net.Conn, ffmpeg.Progress) error {
						// Сразу завершает условный выход: сценарий проверяет принадлежность диагностики работникам и попыткам.
						return nil
					},
					Command: func(commandCtx context.Context, a ffmpeg.Attempt) (ffmpeg.Command, error) {
						// Связывает локальную попытку с отдельным номером наблюдения, задерживает первую до release и
						// запускает команду с полными синтетическими данными журнала.

						// ao содержит наблюдателя доменной попытки; beginErr
						// сообщает об отказе его открытия.
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
					// w содержит работника, чей ранний итог проверяется при
					// истечении общего срока.
					for _, w := range workers {
						select {
						// result содержит уже завершившийся исход работника для
						// диагностики не достигнутого барьера команды.
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
			// w содержит очередного работника для ожидания обеих попыток и проверки
			// корреляции ошибок.
			for _, w := range workers {
				// result содержит итог двух физических попыток; waitErr сообщает об
				// отказе их ожидания.
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
				// reader представляет пустой вход fMP4; writer немедленно закрывается
				// для контрольной ошибки чтения.
				reader, writer := net.Pipe()
				_ = writer.Close()
				err = g.readOutputObserved(ctx, reader, Live, w.binding.Load().coreID, core.MediaFormat{}, func(ffmpeg.Track, int64) bool {
					// Принимает любой отчёт прогресса в проверке пустого fMP4, где до него разбор не должен дойти.
					return true
				}, w.binding.Load())
				_ = reader.Close()
				if err == nil {
					t.Fatal("пустой fMP4 не вернул ошибку")
				}
			}
			if err = sink.Close(ctx); err != nil {
				t.Fatal(err)
			}
			// decoder последовательно разбирает сохранённые JSON-записи журнала.
			decoder := json.NewDecoder(&output)
			// seen отмечает операции отдельно по роли и локальному номеру физической
			// попытки.
			seen := make(map[core.FFmpegRole]map[uint64]map[string]bool)
			for {
				// record содержит очередную разобранную JSON-запись для проверки
				// идентичности и полных значений.
				var record map[string]any
				if err = decoder.Decode(&record); errors.Is(err, io.EOF) {
					break
				} else if err != nil {
					t.Fatal(err)
				}
				// role содержит роль работника, указанную в текущей записи.
				role := core.FFmpegRole(record["worker_role"].(string))
				// expected выбирает контрольного работника, которому должна принадлежать
				// запись.
				var expected *worker
				// w содержит кандидата при поиске работника с ролью текущей записи.
				for _, w := range workers {
					if role == w.role {
						expected = w
					}
				}
				if expected == nil || record["worker_instance_id"] != float64(expected.observer.Token()) || record["group_instance_id"] != float64(groupObserver.Token()) || record["stream_id"] != "synthetic-stream" || record["session_id"] != string(g.parent.SessionID) || record["group_id"] != string(g.id) {
					t.Fatal("утрачена точная принадлежность записи")
				}
				// attempt содержит локальный номер физической попытки из журнала.
				attempt := uint64(record["attempt"].(float64))
				// operation содержит вид операции текущей диагностической записи.
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
				// present показывает недопустимое наличие изменяемого core_attempt у
				// событий очереди диагностики.
				if _, present := record["core_attempt"]; present {
					t.Fatal("диагностика получила изменяемую текущую core attempt")
				}
				if operation == "command" {
					// got восстанавливает список аргументов из JSON-события команды.
					var got []string
					// arg содержит очередной сохранённый аргумент для восстановления
					// точного argv.
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
				// data содержит JSON-представление исходных бинарных байтов вывода.
				data := record["data"].(map[string]any)
				// decoded содержит восстановленные бинарные байты; decodeErr сообщает об
				// отказе разбора base64.
				decoded, decodeErr := base64.StdEncoding.DecodeString(data["bytes"].(string))
				// want содержит точные ожидаемые байты stdout либо stderr выбранной
				// операции.
				want := []byte{0xff, 0, 0xfe, 'p', 'a', 'y', 'l', 'o', 'a', 'd'}
				if operation == "stderr" {
					want = []byte("synthetic-key=<full>&url=?token=whole")
				}
				if decodeErr != nil || data["encoding"] != "base64" || !bytes.Equal(decoded, want) {
					t.Fatal("изменена полная бинарная диагностика")
				}
			}
			// w содержит очередного работника при проверке полного набора записей двух
			// попыток.
			for _, w := range workers {
				// attempt задаёт проверяемый локальный номер первой либо второй попытки.
				for attempt := uint64(1); attempt <= 2; attempt++ {
					// operation содержит очередную обязательную запись начала,
					// команды или вывода каждой попытки.
					for _, operation := range []string{"begin_attempt", "command", "stdout", "stderr"} {
						if !seen[w.role][attempt][operation] {
							t.Fatal("нет записи", w.role, attempt, operation)
						}
					}
				}
				// operation содержит очередную ожидаемую коррелированную ошибку wait
				// либо read_output второй попытки.
				for _, operation := range []string{"wait", "read_output"} {
					if !seen[w.role][2][operation] {
						t.Fatal("нет коррелированной ошибки", w.role, operation)
					}
				}
			}
		})
	}
}
