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

// lifecycleAcceptanceRepository возвращает согласованные Session, Output и квитанцию для одного точного
// тестового исполнения.
type lifecycleAcceptanceRepository struct {
	// RuntimeRepository предоставляет форму остальных операций исполнения, которые этот тестовый
	// репозиторий не реализует.
	core.RuntimeRepository
	// ArtifactRepository предоставляет форму остальных операций артефактов, которые этот тестовый
	// репозиторий не реализует.
	core.ArtifactRepository
	// fence задаёт точные идентификаторы и поколения сессии, выхода и успешной команды.
	fence core.ExecutionFence
}

// Session возвращает работающую сессию точного тестового ExecutionFence.
func (r lifecycleAcceptanceRepository) Session(context.Context, core.ID) (core.Session, error) {
	return core.Session{ID: r.fence.SessionID, Incarnation: r.fence.Incarnation, Lifecycle: r.fence.Lifecycle, State: core.SessionRunning}, nil
}

// Output возвращает выданный подключённый выход с требуемым состоянием и точной идентичностью
// тестового исполнения.
func (r lifecycleAcceptanceRepository) Output(context.Context, core.ID) (core.SessionOutput, error) {
	return core.SessionOutput{ID: r.fence.OutputID, SessionID: r.fence.SessionID, Attached: true, DesiredRunning: true, Issued: true, Generation: r.fence.OutputGeneration, Attempt: r.fence.Attempt}, nil
}

// ReceiptByID возвращает успешную квитанцию запуска точного тестового исполнения выхода.
func (r lifecycleAcceptanceRepository) ReceiptByID(context.Context, core.ID) (core.CommandReceipt, error) {
	// f содержит точную идентичность для построения успешной квитанции запуска выхода.
	f := r.fence
	return core.CommandReceipt{ID: f.CommandID, Kind: core.CommandStartOutput, Status: core.CommandSucceeded, Incarnation: f.Incarnation, SessionID: f.SessionID, Lifecycle: f.Lifecycle, OutputID: f.OutputID, OutputGeneration: f.OutputGeneration, Attempt: f.Attempt}, nil
}

// Generations возвращает пустой список подготовок для проверки допуска исполнения выхода.
func (r lifecycleAcceptanceRepository) Generations(context.Context, core.ListQuery) ([]core.FallbackGeneration, error) {
	return nil, nil
}

// lifecycleAcceptanceUOW связывает настоящий RuntimeAuthority с управляемыми чтениями без долговечных
// записей.
type lifecycleAcceptanceUOW struct {
	// authority предоставляет общий настоящий допуск выполнения теста.
	authority *core.RuntimeAuthority
	// repository возвращает согласованные долговечные представления тестового исполнения.
	repository lifecycleAcceptanceRepository
}

// RuntimeAuthority возвращает общего настоящего владельца допусков теста.
func (u *lifecycleAcceptanceUOW) RuntimeAuthority() *core.RuntimeAuthority { return u.authority }

// View проверяет отмену и выполняет чтение через управляемый репозиторий без записи.
func (u *lifecycleAcceptanceUOW) View(ctx context.Context, fn func(core.Repositories) error) error {
	// err содержит причину отмены контекста до чтения через тестовый репозиторий.
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn(core.Repositories{Runtime: u.repository, Artifacts: u.repository})
}

// Write отвергает неожиданную долговечную запись со стороны Publisher.
func (u *lifecycleAcceptanceUOW) Write(context.Context, func(core.Repositories) error) error {
	return errors.New("неожиданная долговечная запись Publisher")
}

// Проверяет срок Stop при настоящем ожидании общей блокировки RuntimeAuthority, отсутствие сети после
// Stop и запрет освобождения до отдельного отзыва долговечного исполнения.
func TestPublisherAcceptanceStopDuringRealAuthorityGateWait(t *testing.T) {
	// activate выбирает ожидающий Activate либо Abort подготовленного Publisher под занятым общим
	// допуском.
	for _, activate := range []bool{false, true} {
		t.Run(map[bool]string{false: "prepared_abort", true: "pending_activate"}[activate], func(t *testing.T) {
			// Проверяет остановку подготовленного либо ожидающего активации Publisher при занятом настоящем
			// допуске RuntimeAuthority.

			synctest.Test(t, func(t *testing.T) {
				// В управляемом времени удерживает GuardWrite, проверяет срок Stop 10 мс, отсутствие сети и
				// неподтверждённое завершение до возврата Seal.

				// ctx задаёт общий контекст операций настоящего RuntimeAuthority.
				ctx := context.Background()
				// a содержит настоящего владельца допусков; err сохраняет результаты его
				// создания и последующих операций исполнения.
				a, err := core.NewRuntimeAuthority("synthetic-owner", core.ObservationLimits{MaxSessions: 2, MaxGroups: 2, MaxOutputs: 2, MaxFFmpegWorkers: 2, MaxIdentifierBytes: 1024, MaxErrorBytes: 1024, MaxBytes: 1 << 20}, core.OutputExecutionLimits{MaxReservations: 1, MaxIdentifierBytes: 1024, MaxBytes: 1 << 20})
				if err != nil {
					t.Fatal(err)
				}
				// f задаёт точную идентичность контрольного исполнения выхода.
				f := core.ExecutionFence{CommandID: "synthetic-command", Incarnation: "synthetic-owner", SessionID: "synthetic-session", Lifecycle: 1, OutputID: "synthetic-output", OutputGeneration: 1, Attempt: 1}
				// u связывает настоящего владельца допусков с согласованным тестовым
				// репозиторием.
				u := &lifecycleAcceptanceUOW{authority: a, repository: lifecycleAcceptanceRepository{fence: f}}
				// s содержит доменный сервис, использующий настоящий RuntimeAuthority
				// теста.
				s, err := core.NewService(u, f.Incarnation)
				if err != nil {
					t.Fatal(err)
				}
				// lease содержит допуск точного исполнения Session, к которому привязан
				// выход.
				lease, err := s.BindExecutionOwner(ctx, f)
				if err != nil {
					t.Fatal(err)
				}
				// reservation содержит резервацию точного исполнения выхода до активации
				// сети.
				reservation, err := s.ReserveOutputExecution(ctx, lease, core.OutputSnapshot{})
				if err != nil {
					t.Fatal(err)
				}
				// observer содержит настоящего наблюдателя контрольного выхода.
				observer, err := s.OpenOutputObserver(ctx, f)
				if err != nil {
					t.Fatal(err)
				}
				// r содержит Router для Publisher с запрещённым открытием сети после
				// Stop.
				r, _, _, _ := testRouter(t)
				// dials считает недопустимые сетевые попытки для проверки их отсутствия.
				var dials atomic.Uint64
				// p содержит Publisher с настоящей резервацией и учитываемым завершением
				// Seal.
				p := &Publisher{router: r, execution: coreExecution{reservation: reservation}, reservation: reservation, done: make(chan struct{}), sealDone: make(chan struct{}), state: core.OutputStopped, dial: func(context.Context, rtmp.OutboundTarget, rtmp.OutboundOptions) (connection, error) {
					// Считает запрещённую попытку сети после Stop и возвращает ошибку.

					dials.Add(1)
					return nil, errors.New("неожиданная сеть после Stop")
				}}
				// entered сообщает о захвате GuardWrite; release разрешает его возврат;
				// writeDone передаёт окончательный исход удерживающей операции.
				entered, release, writeDone := make(chan struct{}), make(chan struct{}), make(chan error, 1)
				go func() {
					// Удерживает настоящий GuardWrite до release и передаёт его результат через writeDone.

					writeDone <- a.GuardWrite(ctx, u, time.Second, func() (bool, error) {
						// Сообщает о захвате допуска записи и ждёт release, возвращая отсутствие сохраняемого изменения.
						close(entered)
						<-release
						return false, nil
					})
				}()
				<-entered
				// activationDone передаёт исход Activate, ожидающего занятого допуска.
				activationDone := make(chan error, 1)
				if activate {
					go func() {
						// Выполняет Activate параллельно удерживаемому GuardWrite и передаёт результат через activationDone.
						activationDone <- p.Activate(ctx, observer)
					}()
					synctest.Wait()
					p.mu.Lock()
					// pending сохраняет признак незавершённой активации перед
					// вызовом Stop.
					pending := p.activating
					p.mu.Unlock()
					if !pending {
						t.Fatal("Activate не дошёл до ожидаемого допуска")
					}
				}
				// stopCtx ограничивает Stop десятью миллисекундами; cancel освобождает
				// таймер ожидания.
				stopCtx, cancel := context.WithTimeout(ctx, 10*time.Millisecond)
				defer cancel()
				// start фиксирует начало Stop для проверки точной длины ожидания в
				// управляемом времени.
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
				for range 3 {
					if err = p.Stop(stopCtx); !errors.Is(err, ErrCleanupPending) {
						t.Fatal(err)
					}
				}
				close(release)
				if err = <-writeDone; err != nil {
					t.Fatal(err)
				}
				synctest.Wait()
				// result содержит итог Publisher после освобождения настоящего допуска и
				// завершения Seal.
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
