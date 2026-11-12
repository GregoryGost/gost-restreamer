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

// executionAcceptanceUOW добавляет управляемый отказ чтения и счётчик View к настоящему Store.
type executionAcceptanceUOW struct {
	// Store предоставляет настоящие миграции, транзакции и общую RuntimeAuthority временной SQLite.
	*storage.Store
	// viewErr задаёт контролируемый отказ согласованной сверки.
	viewErr error
	// views считает обращения к согласованному чтению настоящего хранилища.
	views int
}

// Учитывает чтение и передаёт его настоящему Store либо возвращает заданную ошибку сверки.
func (u *executionAcceptanceUOW) View(ctx context.Context, f func(core.Repositories) error) error {
	u.views++
	if u.viewErr != nil {
		return u.viewErr
	}
	return u.Store.View(ctx, f)
}

// executionAcceptanceFixture объединяет два Service и точные выданные попытки выходов одного временного
// Store.
type executionAcceptanceFixture struct {
	// u предоставляет настоящий Store с подменяемой доступностью чтения.
	u *executionAcceptanceUOW
	// s предоставляет основной Service временного Store. other предоставляет второй Service того же
	// временного Store.
	s, other *core.Service
	// fences сохраняет точные выданные попытки тестовых выходов.
	fences []core.ExecutionFence
	// snapshots сохраняет самостоятельные снимки тестовых выходов.
	snapshots []core.OutputSnapshot
}

// Открывает временную файловую SQLite с рабочими миграциями и двумя Service, создаёт заданное число
// выходов и выдаёт их попытки; Store закрывается очисткой теста.
func newExecutionAcceptanceFixture(t *testing.T, count, budget int) executionAcceptanceFixture {
	t.Helper()
	// ctx задаёт контекст проверяемых операций физического исполнения выхода.
	ctx := context.Background()
	// store владеет временной файловой SQLite с рабочими миграциями Goose. err сообщает об ошибке
	// открытия временной файловой SQLite.
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
		// Закрывает созданный временный Store после теста и сообщает ошибку освобождения ресурсов.

		// err сообщает об ошибке закрытия временного Store.
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	// d содержит фактически прочитанные свойства SQLite и версию применённой схемы.
	d, err := store.Diagnostics(ctx)
	if err != nil || d.SchemaVersion != 1 || d.SQLiteVersion == "" {
		t.Fatalf("Проверка рабочего runner Goose/SQLite: %+v, %v", d, err)
	}
	// f объединяет настоящий временный Store, два Service и точные попытки тестовых выходов.
	f := executionAcceptanceFixture{u: &executionAcceptanceUOW{Store: store}}
	f.s, err = core.NewService(f.u, store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	f.other, err = core.NewService(f.u, store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	// sessionSnapshot задаёт корректный начальный снимок тестового запуска.
	sessionSnapshot, _ := contractSnapshots(t)
	// session задаёт выданный RUNNING-запуск, которому принадлежат тестовые выходы.
	session := core.Session{Record: contractRecord("execution-session"), Issued: true, StreamID: sessionSnapshot.Stream.ID, Incarnation: store.Incarnation(), State: core.SessionRunning, Lifecycle: 1, Snapshot: sessionSnapshot}
	err = store.Write(ctx, func(r core.Repositories) error {
		// В одной транзакции создаёт Session в состоянии RUNNING, выходы и PENDING-квитанции для дальнейшего
		// штатного Claim и MarkIssued.

		// err сообщает об ошибке сохранения тестовой сессии.
		if err := r.Runtime.InsertSession(ctx, session); err != nil {
			return err
		}
		// i указывает создаваемый или выдаваемый тестовый выход.
		for i := range count {
			// snapshot содержит неизменяемый снимок выхода для точного резервирования.
			_, snapshot := contractSnapshots(t)
			// id идентифицирует очередной сохраняемый тестовый выход.
			id := core.ID(fmt.Sprintf("execution-output-%d", i))
			// o задаёт новый прикреплённый выход с самостоятельным снимком и поколением 1.
			o := core.SessionOutput{Record: contractRecord(id), SessionID: session.ID, Snapshot: snapshot, Attached: true, DesiredRunning: true, Generation: 1}
			// err сообщает об ошибке сохранения тестового выхода.
			if err := r.Runtime.InsertOutput(ctx, o); err != nil {
				return err
			}
			// identity задаёт уникальное синтетическое содержимое команды запуска выхода.
			identity := fmt.Appendf(nil, "synthetic-start-%d", i)
			// sum содержит SHA-256 синтетической команды для согласованного Fingerprint.
			sum := sha256.Sum256(identity)
			// c задаёт PENDING-квитанцию очередного запуска выхода.
			c := core.CommandReceipt{Record: contractRecord(core.ID(fmt.Sprintf("execution-command-%d", i))), RequestID: string(identity), Kind: core.CommandStartOutput, TargetID: id, Identity: identity, Fingerprint: hex.EncodeToString(sum[:]), SessionID: session.ID, OutputID: id, Incarnation: store.Incarnation(), Lifecycle: 1, OutputGeneration: 1, Status: core.CommandPending}
			// err сообщает об ошибке сохранения тестовой квитанции.
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
	// i указывает создаваемый или выдаваемый тестовый выход.
	for i := range count {
		// fence идентифицирует точную попытку проверяемого выхода. err сообщает об ошибке
		// захвата команды запуска выхода.
		fence, err := f.s.Claim(ctx, fmt.Sprintf("synthetic-start-%d", i))
		if err != nil {
			t.Fatal(err)
		}
		// err сообщает об ошибке выдачи точной попытки запуска.
		if err := f.s.MarkIssued(ctx, fence); err != nil {
			t.Fatal(err)
		}
		f.fences = append(f.fences, fence)
	}
	return f
}

// Привязывает RuntimeLease и резервирует физическое исполнение выбранного тестового выхода по его
// точному снимку.
func (f executionAcceptanceFixture) reserve(t *testing.T, i int) (*core.RuntimeLease, *core.OutputExecutionReservation) {
	t.Helper()
	// l содержит логический допуск точного исполнения выхода. err сообщает об ошибке выдачи
	// логического допуска точного исполнения.
	l, err := f.s.BindExecutionOwner(context.Background(), f.fences[i])
	if err != nil {
		t.Fatal(err)
	}
	// r удерживает физический резерв проверяемого выхода.
	r, err := f.s.ReserveOutputExecution(context.Background(), l, f.snapshots[i])
	if err != nil {
		t.Fatal(err)
	}
	return l, r
}

// Сохраняет успешный результат запуска, открывает OutputObserver и активирует точный резерв выбранного
// выхода.
func (f executionAcceptanceFixture) activate(t *testing.T, i int, r *core.OutputExecutionReservation) *core.OutputObserver {
	t.Helper()
	// ctx задаёт контекст проверяемых операций физического исполнения выхода.
	ctx := context.Background()
	// err сообщает об ошибке фиксации результата запуска выхода.
	if err := f.s.Complete(ctx, f.fences[i], core.Completion{Success: true}); err != nil {
		t.Fatal(err)
	}
	// o содержит наблюдателя успешно завершённой точной попытки выхода. err сообщает об ошибке
	// регистрации наблюдателя точной попытки выхода.
	o, err := f.s.OpenOutputObserver(ctx, f.fences[i])
	if err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке активации физического резерва точным наблюдателем.
	if err := r.Activate(ctx, o); err != nil {
		t.Fatal(err)
	}
	return o
}

// Проверяет ожидаемую категорию отказа исполнения через errors.Is.
func requireExecutionError(t *testing.T, got, want error) {
	t.Helper()
	if !errors.Is(got, want) {
		t.Fatalf("ошибка %v, ожидалась %v", got, want)
	}
}

// Проверяет на файловой SQLite отклонение подмены каждой существенной части снимка и единственность
// резерва между двумя Service.
func TestOutputExecutionAcceptanceAuthoritativeSnapshot(t *testing.T) {
	// f объединяет настоящий временный Store, два Service и точные попытки тестовых выходов.
	f := newExecutionAcceptanceFixture(t, 1, 1)
	// l содержит логический допуск точного исполнения выхода. err сообщает об ошибке выдачи
	// логического допуска точного исполнения.
	l, err := f.s.BindExecutionOwner(context.Background(), f.fences[0])
	if err != nil {
		t.Fatal(err)
	}
	// Каждое значение относится к принятому снимку, включая поля вне сетевого назначения.
	// tc содержит выбранную подмену существенного поля принятого снимка.
	for _, tc := range []struct {
		// name задаёт имя подменяемого поля принятого снимка.
		name string
		// mutate вносит выбранную подмену в отдельную копию принятого снимка.
		mutate func(*core.OutputSnapshot)
	}{
		{"destination_name", func(v *core.OutputSnapshot) {
			// Меняет имя назначения для проверки полного сравнения принятого снимка.
			v.Destination.Name += "x"
		}},
		{"destination_revision", func(v *core.OutputSnapshot) {
			// Увеличивает Revision назначения для проверки полного сравнения принятого снимка.
			v.Destination.Revision++
		}},
		{"destination_endpoint", func(v *core.OutputSnapshot) {
			// Меняет Endpoint назначения для проверки полного сравнения принятого снимка.
			v.Destination.Endpoint += "?x"
		}},
		{"destination_key", func(v *core.OutputSnapshot) {
			// Меняет ключ назначения для проверки полного сравнения принятого снимка.
			v.Destination.StreamKey += "x"
		}},
		{"group", func(v *core.OutputSnapshot) {
			// Меняет имя группы для проверки полного сравнения принятого снимка.
			v.Group.Name += "x"
		}},
		{"profile", func(v *core.OutputSnapshot) {
			// Увеличивает GOPFrames профиля для проверки полного сравнения принятого снимка.
			v.Profile.Profile.VideoEncoding.GOPFrames++
		}},
		{"fallback", func(v *core.OutputSnapshot) {
			// Меняет таймаут аудио политики заглушки для проверки полного сравнения принятого снимка.
			v.Fallback.Watchdog.AudioTimeout++
		}},
		{"artifact_path", func(v *core.OutputSnapshot) {
			// Меняет путь файла манифеста для проверки полного сравнения принятого снимка.
			v.Artifact.ManagedPath += "x"
		}},
		{"artifact_size", func(v *core.OutputSnapshot) {
			// Увеличивает размер файла манифеста для проверки полного сравнения принятого снимка.
			v.Artifact.SizeBytes++
		}},
		{"artifact_hash", func(v *core.OutputSnapshot) {
			// Меняет хеш содержимого манифеста для проверки полного сравнения принятого снимка.
			v.Artifact.ContentHash += "x"
		}},
		{"artifact_source", func(v *core.OutputSnapshot) {
			// Меняет хеш исходника манифеста для проверки полного сравнения принятого снимка.
			v.Artifact.SourceHash += "x"
		}},
		{"artifact_generation", func(v *core.OutputSnapshot) {
			// Меняет GenerationID манифеста для проверки полного сравнения принятого снимка.
			v.Artifact.GenerationID += "x"
		}},
		{"artifact_format", func(v *core.OutputSnapshot) {
			// Увеличивает проверенную ширину видео манифеста для проверки полного сравнения принятого снимка.
			v.Artifact.VerifiedFormat.Video.Width++
		}},
		{"canonical_bytes", func(v *core.OutputSnapshot) {
			// Меняет один бит канонического профиля манифеста для проверки побайтового сравнения снимка.
			v.Artifact.CanonicalProfile[0] ^= 1
		}},
		{"canonical_absence", func(v *core.OutputSnapshot) {
			// Убирает канонические байты профиля для проверки различия присутствия и отсутствия.
			v.Artifact.CanonicalProfile = nil
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет ErrStale резервирования подменённого поля через другой Service того же Store.

			// v содержит отдельную копию принятого снимка с выбранной подменой.
			v := f.snapshots[0]
			v.Artifact.CanonicalProfile = bytes.Clone(v.Artifact.CanonicalProfile)
			tc.mutate(&v)
			// err сообщает об ошибке резервирования физического исполнения выхода.
			_, err := f.other.ReserveOutputExecution(context.Background(), l, v)
			requireExecutionError(t, err, core.ErrStale)
		})
	}
	// r удерживает физический резерв проверяемого выхода.
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

// Проверяет на SQLite, что логический отзыв не допускает повтор физического исполнения и не
// освобождает бюджет до сохраняемой остановки и окончания действий; освобождение RAM не подменяет
// очистку SQL.
func TestOutputExecutionAcceptanceQuarantineCannotReplay(t *testing.T) {
	// cause выбирает логический отзыв, замену наблюдателя или ошибку сверки.
	for _, cause := range []string{"revoke", "replace_observer", "close_observer", "uncertain_reconciliation"} {
		t.Run(cause, func(t *testing.T) {
			// Проверяет, что выбранный отзыв допуска сохраняет карантин физического исполнения и бюджет до
			// долговечной остановки, успешной сверки и завершения операций.

			// ctx задаёт контекст проверяемых операций физического исполнения выхода.
			ctx := context.Background()
			// f объединяет настоящий временный Store, два Service и точные попытки тестовых
			// выходов.
			f := newExecutionAcceptanceFixture(t, 2, 1)
			// l содержит логический допуск точного исполнения выхода. r удерживает
			// физический резерв проверяемого выхода.
			l, r := f.reserve(t, 0)
			// o содержит наблюдателя активированного физического исполнения.
			o := f.activate(t, 0, r)
			// admitted занимает слот установления соединения до подтверждённого завершения.
			// err сообщает об ошибке допуска внешней операции в фиксированный слот.
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
				// uncertain задаёт синтетическую ошибку возможного COMMIT.
				uncertain := errors.New("синтетический неоднозначный COMMIT")
				f.u.viewErr = errors.New("синтетический отказ чтения")
				err = f.u.RuntimeAuthority().GuardWrite(ctx, f.u, time.Second, func() (bool, error) {
					// Возвращает синтетическую ошибку неоднозначного COMMIT для проверки отзыва при недоступной сверке.
					return true, uncertain
				})
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
			// fresh содержит повторно выданный логический допуск того же точного исполнения.
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
			// otherLease содержит логический допуск другого выхода той же сессии.
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
			// readFailure задаёт синтетическую недоступность согласованного чтения
			// результата.
			readFailure := errors.New("синтетический отказ authoritative View")
			f.u.viewErr = readFailure
			requireExecutionError(t, f.s.ConfirmOutputExecutionResolution(ctx, r), readFailure)
			requireExecutionError(t, r.ReleaseJoined(), core.ErrNotReady)
			f.u.viewErr = nil
			if err = f.s.ConfirmOutputExecutionResolution(ctx, r); err != nil {
				t.Fatal(err)
			}
			// views сохраняет число чтений БД перед освобождением RAM-резерва.
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
			// oState содержит сохраняемый выход после освобождения RAM-резерва для проверки
			// сохранности обязательств очистки.
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

// Проверяет на SQLite независимость слотов, точный Finish и освобождение после закрытия
// RuntimeAuthority только при завершении всех допущенных действий.
func TestOutputExecutionAcceptanceSlotsAndCloseJoin(t *testing.T) {
	// ctx задаёт контекст проверяемых операций физического исполнения выхода.
	ctx := context.Background()
	// f объединяет настоящий временный Store, два Service и точные попытки тестовых выходов.
	f := newExecutionAcceptanceFixture(t, 1, 1)
	// r удерживает физический резерв проверяемого выхода.
	_, r := f.reserve(t, 0)
	// err сообщает об ошибке допуска внешней операции в фиксированный слот.
	_, err := r.Admit(ctx, core.OutputOperationWrite)
	requireExecutionError(t, err, core.ErrNotReady)
	f.activate(t, 0, r)
	// slots сохраняет допущенные операции установления, чтения и записи для проверки независимых
	// слотов.
	var slots []*core.OutputOperation
	// kind выбирает один из трёх фиксированных видов внешней операции.
	for _, kind := range []core.OutputOperationKind{core.OutputOperationEstablishment, core.OutputOperationRead, core.OutputOperationWrite} {
		// op содержит допущенную внешнюю операцию, завершаемую после её фактического окончания.
		// err сообщает об ошибке допуска внешней операции в фиксированный слот.
		op, err := r.Admit(ctx, kind)
		if err != nil {
			t.Fatal(err)
		}
		slots = append(slots, op)
		_, err = r.Admit(ctx, kind)
		requireExecutionError(t, err, core.ErrInUse)
	}
	slots[2].Finish()
	// next содержит новую операцию записи после завершения прежнего слота.
	next, err := r.Admit(ctx, core.OutputOperationWrite)
	if err != nil {
		t.Fatal(err)
	}
	slots[2].Finish()
	_, err = r.Admit(ctx, core.OutputOperationWrite)
	requireExecutionError(t, err, core.ErrInUse)
	f.u.RuntimeAuthority().Close()
	requireExecutionError(t, r.ReleaseJoined(), core.ErrNotReady)
	// wg ожидает завершения всех конкурентных операций допуска и Finish.
	var wg sync.WaitGroup
	// op содержит допущенную внешнюю операцию, завершаемую после её фактического окончания.
	for _, op := range append(slots, next) {
		wg.Go(func() {
			// Дважды завершает собственный слот после закрытия RuntimeAuthority для проверки идемпотентности и
			// освобождения резерва.
			op.Finish()
			op.Finish()
		})
	}
	wg.Wait()
	if err = r.ReleaseJoined(); err != nil {
		t.Fatal(err)
	}
	_, err = r.Admit(ctx, core.OutputOperationRead)
	requireExecutionError(t, err, core.ErrClosed)
}

// Проверяет сохранение резерва до долговечного отказа первоначальной подготовки выхода и запрет
// повторного допуска завершённой попытки.
func TestOutputExecutionAcceptanceFailureBeforeComplete(t *testing.T) {
	// ctx задаёт контекст проверяемых операций физического исполнения выхода.
	ctx := context.Background()
	// f объединяет настоящий временный Store, два Service и точные попытки тестовых выходов.
	f := newExecutionAcceptanceFixture(t, 1, 1)
	// r удерживает физический резерв проверяемого выхода.
	_, r := f.reserve(t, 0)
	// err сообщает об ошибке регистрации наблюдателя точной попытки выхода.
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
