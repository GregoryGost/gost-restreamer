package storage

import (
	"context"
	"fmt"
	"reflect"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// Проверяет ручной запуск, допуски исполнения, наблюдения и OBS при терминальных остатках подготовки
// текущей либо прежней incarnation без изменения старого обязательства.
func TestTerminalPreparationDoesNotBlockAnyRuntimeAdmissionStage(t *testing.T) {
	// previous выбирает повторное открытие хранилища перед новым запуском.
	for _, previous := range []bool{false, true} {
		// published выбирает публикацию либо отмену старой подготовки.
		for _, published := range []bool{false, true} {
			t.Run(fmt.Sprintf("previous=%t/published=%t", previous, published), func(t *testing.T) {
				// Проверяет, что терминальная опубликованная либо отменённая подготовка текущего или прежнего
				// владельца с незавершённой операцией не препятствует ни одной стадии нового эфира и сохраняет свои
				// обязательства.

				// f объединяет временный Store, Service, поток и опубликованную заглушку
				// проверяемого сценария.
				f := newDomainFixture(t, 1)
				// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
				ctx := context.Background()
				// g содержит поколение подготовки с проверяемым обязательством очистки.
				// fence сохраняет точный допуск проверяемой внешней попытки.
				g, fence := issuedFixturePreparation(t, f)
				// owner содержит владельца подготовки с зарегистрированной незавершённой
				// операцией. err сообщает об ошибке привязки владельца точной
				// подготовки.
				owner, err := f.service.BindPreparationOwner(ctx, fence)
				if err != nil {
					t.Fatal(err)
				}
				// finish завершает регистрацию операции старой подготовки после проверки
				// допуска эфира.
				finish, err := owner.Register(ctx)
				if err != nil {
					t.Fatal(err)
				}
				defer finish()
				if published {
					// p содержит полный комплект публикации старой подготовки.
					p := fixturePublication(g, "fresh-source", "fresh-content")
					// err сообщает об ошибке атомарной публикации полного поколения.
					if _, err := f.service.PublishPreparation(ctx, fence, p); err != nil {
						t.Fatal(err)
					}
					f.variant, err = f.service.GetVariant(ctx, p.Variants[0].ID)
					if err != nil {
						t.Fatal(err)
					}
				} else /* err сообщает об ошибке отмены выданной подготовки. */ if err := f.service.CancelPreparation(ctx, fence, "Отмена с незавершённой операцией"); err != nil {
					t.Fatal(err)
				}
				if owner.Valid() {
					t.Fatal("терминальная подготовка сохранила допуск новых действий")
				}
				if previous {
					f = reopenFixture(t, f)
				}
				// before сохраняет терминальную подготовку перед новым запуском.
				before, err := f.service.GetGeneration(ctx, g.ID)
				if err != nil || !before.Issued || !before.CleanupPending || !before.State.Terminal() {
					t.Fatalf("утрачено терминальное обязательство: %+v, %v", before, err)
				}
				// start содержит принятую квитанцию первоначального запуска сессии.
				start := startDomainSession(t, f)
				// parent содержит точную выданную попытку запуска сессии.
				parent := executeCommand(t, f.service, start.RequestID)
				// lease содержит допуск владельца новой сессии при остатках подготовки.
				lease, err := f.service.BindExecutionOwner(ctx, parent)
				if err != nil || !lease.Valid() {
					t.Fatal("остатки подготовки запретили родительский допуск", err)
				}
				// observer содержит наблюдателя нового исполнения.
				observer, err := f.service.OpenSessionObserver(ctx, parent)
				if err != nil || !observer.Valid() {
					t.Fatal("остатки подготовки запретили наблюдателя", err)
				}
				// output содержит сохраняемый выход с самостоятельным снимком и
				// намерением.
				output := readOutputs(t, f.store, start.SessionID)[0]
				// child содержит точную выданную попытку нового выхода.
				child := executeCommand(t, f.service, outputStartReceipt(t, f, output.ID).RequestID)
				// childLease содержит независимый допуск владельца выхода.
				childLease, err := f.service.BindExecutionOwner(ctx, child)
				if err != nil || !childLease.Valid() {
					t.Fatal("остатки подготовки запретили выход", err)
				}
				// obs содержит допуск установленного источника OBS.
				obs, err := f.service.ActivateOBS(ctx, f.stream.ID, f.stream.PublicationKey, func(*core.RuntimeLease) (func(), error) {
					// Возвращает nil вместо функции отзыва для успешного допуска OBS при терминальных остатках подготовки.
					return nil, nil
				})
				if err != nil || !obs.Valid() {
					t.Fatal("остатки подготовки запретили OBS", err)
				}
				// after содержит сохранённое состояние после проверяемой операции. err
				// сообщает об ошибке чтения полного поколения подготовки.
				if after, err := f.service.GetGeneration(ctx, g.ID); err != nil || !reflect.DeepEqual(before, after) {
					t.Fatalf("новый запуск изменил прежнюю подготовку: %+v, %v", after, err)
				}
				// pending содержит страницу поколений с ещё не подтверждённой очисткой.
				pending, err := f.service.ListGenerations(ctx, core.PageRequest{Query: core.ListQuery{Limit: 10, PendingOnly: true}})
				if err != nil || len(pending.Items) != 1 || pending.Items[0].ID != g.ID {
					t.Fatalf("PendingOnly потерял терминальные остатки: %+v, %v", pending, err)
				}
				// Сверка lease после последующей записи также не должна возвращать старый запрет.
				// err сообщает об ошибке условного переименования стабильной заглушки.
				if _, err := f.service.RenameAsset(ctx, f.asset.ID, readAssetRevision(t, f, ctx), "Переименованная заглушка"); err != nil {
					t.Fatal(err)
				}
				if !lease.Valid() || !childLease.Valid() || !obs.Valid() {
					t.Fatal("сверка записи отозвала новый допуск из-за старой подготовки")
				}
			})
		}
	}
}

// Читает текущую ревизию тестового Asset для следующей операции с CAS.
func readAssetRevision(t *testing.T, f domainFixture, ctx context.Context) uint64 {
	t.Helper()
	// a содержит стабильную заглушку проверяемого поколения. err сообщает об ошибке чтения заглушки
	// и указателей поколений.
	a, err := f.service.GetAsset(ctx, f.asset.ID)
	if err != nil {
		t.Fatal(err)
	}
	return a.Revision
}

// Вставляет согласованное активное тестовое поколение заданного состояния, моделируя конкурирующую
// подготовку между стадиями допуска.
func insertActivePreparationForAdmission(t *testing.T, f domainFixture, state core.PreparationState) {
	t.Helper()
	// now задаёт согласованное время прямой вставки поколения с точностью микросекунды.
	now := time.Now().UTC().Truncate(time.Microsecond)
	// g содержит поколение подготовки с проверяемым обязательством очистки.
	g := core.FallbackGeneration{ID: "competing-preparation", Revision: 1, CreatedAt: now, UpdatedAt: now, AssetID: f.asset.ID, Kind: core.AssetImage, GeneratorVersion: "generator", Incarnation: f.store.Incarnation(), AttemptToken: "competing-token", State: state, OriginalPath: "fallback/competing-preparation/original", StagingPath: "fallback/competing-preparation/original.stage"}
	if state != core.PreparationQueued {
		g.Attempt = 1
	}
	if state == core.PreparationIssued {
		g.Issued, g.CleanupPending = true, true
	}
	// err сообщает об ошибке прямой вставки активной подготовки между стадиями допуска эфира.
	if err := f.store.Write(context.Background(), func(r core.Repositories) error {
		// Вставляет активное поколение напрямую для моделирования несовместимой подготовки между стадиями
		// допуска эфира.
		return r.Artifacts.InsertGeneration(context.Background(), g)
	}); err != nil {
		t.Fatal(err)
	}
}

// Проверяет запрет Start, Claim, MarkIssued и RuntimeLease при QUEUED/CLAIMED/ISSUED-подготовке
// текущего владельца.
func TestActivePreparationExcludesStartClaimIssuanceAndLease(t *testing.T) {
	// state выбирает активную стадию конкурирующей подготовки.
	for _, state := range []core.PreparationState{core.PreparationQueued, core.PreparationClaimed, core.PreparationIssued} {
		// stage выбирает проверяемую стадию допуска запуска.
		for _, stage := range []string{"start", "claim", "issue", "lease"} {
			t.Run(string(state)+"/"+stage, func(t *testing.T) {
				// Проверяет ErrNotReady у StartSession, Claim, MarkIssued либо BindExecutionOwner при активной QUEUED,
				// CLAIMED или ISSUED подготовке.

				// f объединяет временный Store, Service, поток и опубликованную заглушку
				// проверяемого сценария.
				f := newDomainFixture(t, 1)
				// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
				ctx := context.Background()
				// request содержит запрос ручного запуска для проверки исключения
				// активной подготовки.
				request := f.startRequest("admission")
				// fence сохраняет точный допуск проверяемой внешней попытки.
				var fence core.ExecutionFence
				if stage != "start" {
					// err сообщает об ошибке принятия ручного запуска потока.
					if _, err := f.service.StartSession(ctx, request); err != nil {
						t.Fatal(err)
					}
				}
				if stage == "issue" || stage == "lease" {
					// err сообщает об ошибке предварительного захвата команды
					// запуска.
					var err error
					fence, err = f.service.Claim(ctx, request.RequestID)
					if err != nil {
						t.Fatal(err)
					}
				}
				if stage == "lease" {
					// err сообщает об ошибке выдачи точной попытки команды.
					if err := f.service.MarkIssued(ctx, fence); err != nil {
						t.Fatal(err)
					}
				}
				// Прямая запись моделирует встреченную несовместимую подготовку между стадиями.
				insertActivePreparationForAdmission(t, f, state)
				// err сообщает об ошибке проверяемой стадии допуска при активной
				// подготовке.
				var err error
				switch stage {
				case "start":
					_, err = f.service.StartSession(ctx, request)
				case "claim":
					_, err = f.service.Claim(ctx, request.RequestID)
				case "issue":
					err = f.service.MarkIssued(ctx, fence)
				case "lease":
					_, err = f.service.BindExecutionOwner(ctx, fence)
				}
				requireDomainError(t, err, core.ErrNotReady)
			})
		}
	}
}

// Проверяет однократное намерение очистки предварительного выхода выданной Session до запуска
// Publisher, без создания новой работы при повторной остановке.
func TestStopProvisionalOutputTracksLiabilityOnceBeforePublisherIssuance(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 2)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// start содержит принятую квитанцию первоначального запуска сессии.
	start := startDomainSession(t, f)
	// parent содержит точную выданную попытку запуска сессии. err сообщает об ошибке захвата точной
	// команды исполнения.
	parent, err := f.service.Claim(ctx, start.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке выдачи точной попытки команды.
	if err := f.service.MarkIssued(ctx, parent); err != nil {
		t.Fatal(err)
	}
	// outputs содержит все сохраняемые выходы проверяемой сессии.
	outputs := readOutputs(t, f.store, start.SessionID)
	// target сохраняет предварительный выход, который останавливается до выдачи Publisher. neighbor
	// сохраняет соседний выход для проверки изоляции остановки.
	target, neighbor := outputs[0], outputs[1]
	// stop содержит квитанцию остановки проверяемого исполнения.
	stop, err := f.service.StopOutput(ctx, core.OutputCommand{RequestID: "stop-provisional", OutputID: target.ID})
	if err != nil || stop.Status != core.CommandPending {
		t.Fatal("остановка не сохранила обязательство", err)
	}
	// stopped содержит остановленный предварительный выход с обязательством очистки.
	stopped, err := f.service.GetOutput(ctx, target.ID)
	if err != nil || !stopped.CleanupPending || stopped.Issued || stopped.Attempt != 0 || stopped.DesiredRunning || stopped.Generation <= target.Generation || !stopped.Attached {
		t.Fatalf("остановка потеряла обязательство предварительного выхода или выдумала сеть: %+v, %v", stopped, err)
	}
	// cleanups содержит сохраняемые намерения физической очистки после проверяемой операции.
	cleanups := pendingCleanups(t, f.store, start.SessionID)
	if len(cleanups) != 1 || cleanups[0].OutputID != target.ID || cleanups[0].Attempt != 0 || cleanups[0].Generation != target.Generation {
		t.Fatalf("неверное обязательство предварительного выхода: %+v", cleanups)
	}
	// err сообщает об ошибке сохранения результата точной команды.
	if err := f.service.Complete(ctx, parent, core.Completion{Success: true}); err != nil {
		t.Fatal("остановка одного выхода прервала запуск соседей", err)
	}
	// err сообщает об ошибке подтверждения точного намерения очистки.
	if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(cleanups[0])); err != nil {
		t.Fatal(err)
	}
	// cleared сохраняет выход после подтверждения предварительной очистки.
	cleared, err := f.service.GetOutput(ctx, target.ID)
	if err != nil {
		t.Fatal(err)
	}
	// repeated содержит квитанцию повторной остановки уже очищенного прикреплённого выхода.
	repeated, err := f.service.StopOutput(ctx, core.OutputCommand{RequestID: "stop-cleared-attached", OutputID: target.ID})
	if err != nil || repeated.Status != core.CommandSucceeded {
		t.Fatal("повторная остановка создала новую работу", err)
	}
	// after содержит сохранённое состояние после проверяемой операции.
	after, err := f.service.GetOutput(ctx, target.ID)
	if err != nil || after.CleanupPending || after.Issued || after.Attempt != 0 || after.Generation != cleared.Generation || len(pendingCleanups(t, f.store, start.SessionID)) != 0 {
		t.Fatalf("Attached повторно создал обязательство: %+v, %v", after, err)
	}
	// current содержит соседний выход после очистки проверяемого. err сообщает об ошибке чтения
	// сохраняемого снимка и намерения выхода.
	if current, err := f.service.GetOutput(ctx, neighbor.ID); err != nil || !reflect.DeepEqual(neighbor, current) {
		t.Fatalf("очистка изменила соседа: %+v, %v", current, err)
	}
	requireOutputVariantHolds(t, f.store, f.variant.ID, true)
}

// Проверяет новый ручной запуск при старой терминальной очистке и защиту его допусков, снимков и
// удержаний от подтверждений прежнего владельца.
func TestTerminalRuntimeCleanupDoesNotBlockNewExecutionOrReleaseItsHolds(t *testing.T) {
	// previous выбирает повторное открытие хранилища перед новым запуском.
	for _, previous := range []bool{false, true} {
		// parentFirst выбирает порядок подтверждения родительской и дочерней очистки.
		for _, parentFirst := range []bool{false, true} {
			t.Run(fmt.Sprintf("previous=%t/parentFirst=%t", previous, parentFirst), func(t *testing.T) {
				// Проверяет новый ручной запуск при старой терминальной очистке и сохранность его допусков, снимков и
				// удержаний при обоих порядках прежних подтверждений.

				// f объединяет временный Store, Service, поток и опубликованную заглушку
				// проверяемого сценария.
				f := newDomainFixture(t, 1)
				// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
				ctx := context.Background()
				// old содержит квитанцию прежнего запуска.
				old := startDomainSession(t, f)
				executeCommand(t, f.service, old.RequestID)
				// oldOutput содержит выход прежнего запуска.
				oldOutput := readOutputs(t, f.store, old.SessionID)[0]
				executeCommand(t, f.service, outputStartReceipt(t, f, oldOutput.ID).RequestID)
				// stop содержит квитанцию остановки проверяемого исполнения. err
				// сообщает об ошибке принятия остановки сессии.
				stop, err := f.service.StopSession(ctx, core.SessionCommand{RequestID: "stop-old-execution", SessionID: old.SessionID})
				if err != nil {
					t.Fatal(err)
				}
				executeCommand(t, f.service, stop.RequestID)
				if previous {
					f = reopenFixture(t, f)
				}
				// cleanups содержит сохраняемые намерения физической очистки после
				// проверяемой операции.
				cleanups := pendingCleanups(t, f.store, old.SessionID)
				if len(cleanups) != 2 {
					t.Fatalf("потеряны обязательства старого выполнения: %+v", cleanups)
				}
				if (cleanups[0].OutputID == "") != parentFirst {
					cleanups[0], cleanups[1] = cleanups[1], cleanups[0]
				}
				// fresh содержит квитанцию нового ручного запуска при старой очистке.
				fresh, err := f.service.StartSession(ctx, f.startRequest("new-manual-execution"))
				if err != nil || fresh.SessionID == old.SessionID {
					t.Fatal("старые остатки запретили новый ручной start", err)
				}
				// parent содержит точную выданную попытку запуска сессии.
				parent := executeCommand(t, f.service, fresh.RequestID)
				// parentLease содержит допуск владельца нового запуска.
				parentLease, err := f.service.BindExecutionOwner(ctx, parent)
				if err != nil {
					t.Fatal(err)
				}
				// output содержит сохраняемый выход с самостоятельным снимком и
				// намерением.
				output := readOutputs(t, f.store, fresh.SessionID)[0]
				// child содержит точную выданную попытку нового выхода.
				child := executeCommand(t, f.service, outputStartReceipt(t, f, output.ID).RequestID)
				// observer содержит наблюдателя нового исполнения.
				observer := openOutputObserver(t, f.service, child)
				// before сохраняет новый запуск перед подтверждением прежней очистки.
				before := readSession(t, f.store, fresh.SessionID)
				// beforeOutput сохраняет новый выход до подтверждения старой очистки.
				beforeOutput, err := f.service.GetOutput(ctx, output.ID)
				if err != nil {
					t.Fatal(err)
				}
				// cleanup содержит очередное точное намерение прежней очистки для
				// подтверждения.
				for _, cleanup := range cleanups {
					// err сообщает об ошибке подтверждения точного намерения
					// очистки.
					if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(cleanup)); err != nil {
						t.Fatal(err)
					}
					if !parentLease.Valid() || !observer.Valid() {
						t.Fatal("старый ack отозвал допуск нового выполнения")
					}
					requireOutputVariantHolds(t, f.store, f.variant.ID, true)
				}
				// after содержит сохранённое состояние после проверяемой операции.
				if after := readSession(t, f.store, fresh.SessionID); !reflect.DeepEqual(before, after) {
					t.Fatal("старый ack изменил новую Session")
				}
				// after содержит сохранённое состояние после проверяемой операции. err
				// сообщает об ошибке чтения сохраняемого снимка и намерения выхода.
				if after, err := f.service.GetOutput(ctx, output.ID); err != nil || !reflect.DeepEqual(beforeOutput, after) {
					t.Fatal("старый ack изменил новый Output", err)
				}
				_, err = f.service.ReservePreparation(ctx, core.PreparationRequest{AssetID: f.asset.ID, ExpectedRevision: readAssetRevision(t, f, ctx), Kind: core.AssetImage, GeneratorVersion: "generator"})
				requireDomainError(t, err, core.ErrInUse)
			})
		}
	}
}
