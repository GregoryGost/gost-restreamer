package integration

import (
	"context"
	"errors"
	"net"
	"path/filepath"
	"reflect"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/publisher"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
)

// publisherCompensationFixture объединяет работающего родителя и два выхода для проверки компенсации
// отправителя до активации сети.
type publisherCompensationFixture struct {
	// service предоставляет операции квитанций, разрешений и подтверждения очистки тестовой сессии.
	service *core.Service
	// parent содержит разрешение работающей сессии, которую компенсация дочернего выхода не должна
	// менять.
	parent core.ExecutionFence
	// observer удерживает актуальное право наблюдения родительской сессии.
	observer *core.SessionObserver
	// outputs содержит два сохранённых выхода для выбора компенсируемого и соседнего ресурсов.
	outputs []core.SessionOutput
	// router предоставляет общий медиамаршрутизатор без создания групп в сценарии компенсации.
	router *media.Router
}

// Создаёт работающую сессию и два выхода в изолированной SQLite с бюджетом одного наблюдателя выхода.
// Возвращает общий Router без медиагрупп; освобождение хранилища, наблюдателя и Router
// зарегистрировано в t.Cleanup.
func newPublisherCompensationFixture(t *testing.T) publisherCompensationFixture {
	t.Helper()
	// ctx используется для создания тестовой сессии и синтетической подготовки заглушки.
	ctx := context.Background()
	// store содержит временную SQLite с бюджетом одного наблюдателя выхода, а err сообщает об ошибке
	// её открытия.
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "compensation.sqlite"), storage.Options{
		OutputExecutionLimits: core.OutputExecutionLimits{MaxReservations: 2, MaxIdentifierBytes: 1024, MaxBytes: 1 << 20},
		PreparationLimits:     core.PreparationLimits{MaxQueued: 2, MaxProfiles: 2},
		ObservationLimits:     core.ObservationLimits{MaxSessions: 1, MaxGroups: 1, MaxOutputs: 1, MaxFFmpegWorkers: 1, MaxIdentifierBytes: 1024, MaxErrorBytes: 4096, MaxBytes: 1 << 20},
		MaxOpenConns:          4, MaxIdleConns: 4, ConnMaxLifetime: time.Minute, ConnMaxIdleTime: time.Minute,
		BusyTimeout: 20 * time.Millisecond, RetryAttempts: 4, RetryTimeout: time.Second, RetryDelay: time.Millisecond,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает временное хранилище проверки компенсации неактивированного выхода.

		// err сообщает об ошибке закрытия временного хранилища компенсации.
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	// diagnostics содержит фактические версии SQLite и схемы Goose для проверки временного
	// хранилища.
	diagnostics, err := store.Diagnostics(ctx)
	if err != nil || diagnostics.SchemaVersion != 1 {
		t.Fatalf("схема Goose: %+v, %v", diagnostics, err)
	}
	t.Logf("SQLite=%s Goose=%d", diagnostics.SQLiteVersion, diagnostics.SchemaVersion)
	// svc предоставляет доменные операции фикстуры поверх временного хранилища.
	svc, err := core.NewService(store, store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	// asset содержит заглушку, для которой вручную публикуется синтетическое поколение.
	asset, err := svc.CreateAsset(ctx, "Заглушка")
	if err != nil {
		t.Fatal(err)
	}
	// stream содержит тестовый поток с постоянным ключом и политикой контроля входа.
	stream, err := svc.CreateStream(ctx, core.Stream{Name: "Поток", PublicationKey: "synthetic-ingest", Fallback: core.FallbackPolicy{AssetID: asset.ID, Watchdog: core.WatchdogPolicy{VideoTimeout: time.Second, AudioTimeout: time.Second, RecoveryWindow: time.Millisecond, KeyframeTimeout: time.Second}}})
	if err != nil {
		t.Fatal(err)
	}
	// profile содержит профиль AV1 2560×1440 для сохранённых снимков двух выходов.
	profile, err := svc.CreateProfile(ctx, core.TranscodeProfile{Name: "Профиль", Profile: publisherProfile(core.CodecAV1, 2560, 1440)})
	if err != nil {
		t.Fatal(err)
	}
	// group связывает тестовый поток с общим профилем обоих назначений.
	group, err := svc.CreateGroup(ctx, core.TranscodeGroup{Name: "Группа", StreamID: stream.ID, ProfileID: profile.ID})
	if err != nil {
		t.Fatal(err)
	}
	// key задаёт синтетический ключ одного из двух независимых назначений.
	for _, key := range []string{"target-key", "neighbor-key"} {
		if _, err = svc.CreateDestination(ctx, core.Destination{Name: "Назначение", StreamID: stream.ID, GroupID: group.ID, Platform: "custom", Endpoint: "rtmp://127.0.0.1:1/live", StreamKey: key, Enabled: true}); err != nil {
			t.Fatal(err)
		}
	}
	// gen содержит резерв подготовки и точную обязательную привязку единственного профиля.
	gen, err := svc.ReservePreparation(ctx, core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage, GeneratorVersion: "controlled-generator"})
	if err != nil {
		t.Fatal(err)
	}
	// pf разрешает синтетическую публикацию подготовленного поколения и подтверждение его очистки.
	pf, err := svc.ClaimPreparation(ctx, gen.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.MarkPreparationIssued(ctx, pf); err != nil {
		t.Fatal(err)
	}
	if len(gen.RequiredProfiles) != 1 {
		t.Fatalf("набор профилей: %+v", gen.RequiredProfiles)
	}
	// r содержит единственный требуемый вариант зарезервированного поколения.
	r := gen.RequiredProfiles[0]
	// publication содержит синтетические свойства исходника и варианта для доменной подготовки без
	// генерации медиа.
	publication := core.GenerationPublication{SourceHash: "controlled-source", SizeBytes: 1, SourceFormat: core.SourceFormat{ImageFormat: "png"}, Variants: []core.FallbackVariant{{ID: r.VariantID, GenerationID: gen.ID, AssetID: asset.ID, SourceHash: "controlled-source", Profile: r.Profile, VerifiedFormat: r.Profile.Format, GeneratorVersion: gen.GeneratorVersion, ManagedPath: r.ManagedPath, SizeBytes: 1, ContentHash: "controlled-result"}}}
	if _, err = svc.PublishPreparation(ctx, pf, publication); err != nil {
		t.Fatal(err)
	}
	if err = svc.AcknowledgePreparationCleanup(ctx, pf); err != nil {
		t.Fatal(err)
	}
	// receipt содержит квитанцию запуска сессии с точной подготовленной привязкой.
	receipt, err := svc.StartSession(ctx, core.StartSessionRequest{RequestID: "start-compensation", StreamID: stream.ID, Bindings: []core.PreparedBinding{{GroupID: group.ID, GenerationID: gen.ID, VariantID: r.VariantID, GeneratorVersion: gen.GeneratorVersion}}})
	if err != nil {
		t.Fatal(err)
	}
	// parent содержит разрешение запуска, которое переводится в успешное выполнение родительской
	// сессии.
	parent, err := svc.Claim(ctx, receipt.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.MarkIssued(ctx, parent); err != nil {
		t.Fatal(err)
	}
	if err = svc.Complete(ctx, parent, core.Completion{Success: true}); err != nil {
		t.Fatal(err)
	}
	// observer регистрирует текущего наблюдателя работающей сессии.
	observer, err := svc.OpenSessionObserver(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает наблюдателя родительской Session после проверки изоляции компенсации.

		// err сообщает об ошибке закрытия наблюдателя родительской сессии.
		if err := observer.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	// outputs содержит два сохранённых выхода успешно запущенной сессии.
	outputs, err := svc.ListOutputs(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8, SessionID: parent.SessionID}})
	if err != nil || len(outputs.Items) != 2 {
		t.Fatalf("выходы: %+v, %v", outputs, err)
	}
	// router предоставляет общий ограниченный источник пакетов для подготовленных отправителей.
	router, err := media.NewRouter(publisherRouterLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает маршрутизатор после освобождения подготовленных отправителей.

		// err сообщает об ошибке закрытия общего маршрутизатора фикстуры.
		if err := router.Close(); err != nil {
			t.Error(err)
		}
	})
	return publisherCompensationFixture{svc, parent, observer, outputs.Items, router}
}

// Находит ожидающую команду запуска выбранного выхода, выполняет Claim и MarkIssued и связывает
// точного владельца исполнения. Отсутствие такой команды или ошибка перехода завершает тест.
func publisherCompensationIssue(t *testing.T, f publisherCompensationFixture, output core.SessionOutput) (core.ExecutionFence, *core.RuntimeLease) {
	t.Helper()
	// ctx используется для выдачи разрешения ожидающей команде выбранного выхода.
	ctx := context.Background()
	// receipts содержит квитанции выбранного выхода, а err сообщает об ошибке их чтения.
	receipts, err := f.service.ListReceipts(ctx, core.PageRequest{Query: core.ListQuery{Limit: 16, SessionID: f.parent.SessionID, OutputID: output.ID}})
	if err != nil {
		t.Fatal(err)
	}
	// receipt обозначает очередную квитанцию для поиска ожидающей команды запуска выхода.
	for _, receipt := range receipts.Items {
		if receipt.Kind != core.CommandStartOutput || receipt.Status != core.CommandPending {
			continue
		}
		// fence содержит точное разрешение запуска выхода, а err сообщает об ошибке его выдачи.
		fence, err := f.service.Claim(ctx, receipt.RequestID)
		if err != nil {
			t.Fatal(err)
		}
		if err = f.service.MarkIssued(ctx, fence); err != nil {
			t.Fatal(err)
		}
		// owner связывает текущее выполнение выхода с проверяемым владельцем RuntimeLease.
		owner, err := f.service.BindExecutionOwner(ctx, fence)
		if err != nil {
			t.Fatal(err)
		}
		return fence, owner
	}
	t.Fatal("нет собственной квитанции запуска выхода")
	return core.ExecutionFence{}, nil
}

// Проверяет компенсацию подготовленного Publisher после успешной квитанции, если наблюдатель
// недоступен по бюджету либо уже заменён. Сеть не запускается, успех старта сохраняется, а
// освобождение резерва требует физического завершения и подтверждения управления, не меняя родителя и
// соседний выход.
func TestPublisherStartupCompensationAfterSuccessfulReceipt(t *testing.T) {
	// phase выбирает отказ наблюдателя из-за общего бюджета либо замену уже выданного наблюдателя.
	for _, phase := range []string{"observer-budget", "replaced-observer"} {
		t.Run(phase, func(t *testing.T) {
			// Проверяет компенсацию после успешной квитанции при выбранной нехватке бюджета наблюдений либо замене
			// наблюдателя.

			// f предоставляет родительскую сессию и соседний выход для проверки изоляции
			// компенсации.
			f := newPublisherCompensationFixture(t)
			// ctx ограничивает сценарий компенсации пятью секундами, а cancel освобождает
			// его таймер.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			// target содержит компенсируемый выход, а neighbor — соседний выход, состояние
			// которого должно сохраниться.
			target, neighbor := f.outputs[0], f.outputs[1]
			if phase == "observer-budget" {
				// fence содержит разрешение соседнего выхода для занятия единственного
				// слота наблюдателя.
				fence, _ := publisherCompensationIssue(t, f, neighbor)
				// err сообщает об отказе успешного завершения запуска соседнего выхода.
				if err := f.service.Complete(ctx, fence, core.Completion{Success: true}); err != nil {
					t.Fatal(err)
				}
				// observer занимает бюджет наблюдений соседним выходом, а err сообщает
				// об ошибке его регистрации.
				observer, err := f.service.OpenOutputObserver(ctx, fence)
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					// Закрывает наблюдателя соседнего выхода, занимающего общий бюджет наблюдений.

					// err сообщает об ошибке закрытия наблюдателя соседнего выхода.
					if err := observer.Close(context.Background()); err != nil {
						t.Error(err)
					}
				})
			}
			// neighborBefore фиксирует состояние соседнего выхода до компенсации, а err
			// сообщает об ошибке его чтения.
			neighborBefore, err := f.service.GetOutput(ctx, neighbor.ID)
			if err != nil {
				t.Fatal(err)
			}
			// fence содержит точное разрешение компенсируемого выхода, а owner — связанного
			// с ним владельца выполнения.
			fence, owner := publisherCompensationIssue(t, f, target)
			// dials считает запрещённые до активации попытки открытия сетевого соединения.
			var dials atomic.Uint64
			// options задаёт ограниченные очередь, повторы и транспорт подготовленного
			// отправителя.
			options := publisher.Options{Queue: publisherQueue(), RetryInitial: time.Millisecond, RetryMax: 8 * time.Millisecond, ResubscribeDelay: time.Millisecond, Transport: publisherTransportOptions(nil)}
			options.Transport.DialContext = func(context.Context, string, string) (net.Conn, error) {
				// Считает и отклоняет сетевые подключения, которые запрещены до успешной активации отправителя.

				dials.Add(1)
				return nil, errors.New("сеть до активации запрещена")
			}
			// prepare создаёт подготовленный отправитель с переданным владельцем для
			// проверки исключительного резерва выполнения.
			prepare := func(lease *core.RuntimeLease) (*publisher.Publisher, error) {
				// Подготавливает отправителя с заданным владельцем для проверки исключительного резерва исполнения.

				return publisher.Prepare(ctx, f.service, lease, target.Snapshot, f.router, options)
			}
			// p содержит подготовленный отправитель, который не должен начать сеть до
			// успешной активации.
			p, err := prepare(owner)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				// Прерывает ещё подготовленного отправителя с отдельным сроком после проверки компенсации.

				// cleanup ограничивает прерывание подготовленного отправителя одной
				// секундой, а stop освобождает таймер очистки.
				cleanup, stop := context.WithTimeout(context.Background(), time.Second)
				defer stop()
				// err сообщает об ошибке прерывания исходного подготовленного
				// отправителя при очистке теста.
				if err := p.Abort(cleanup); err != nil {
					t.Error(err)
				}
			})
			if err = f.service.Complete(ctx, fence, core.Completion{Success: true}); err != nil {
				t.Fatal(err)
			}
			// startReceipt сохраняет успешную квитанцию запуска для проверки её неизменности
			// после компенсации.
			startReceipt, err := f.service.GetReceipt(ctx, fence.CommandID)
			if err != nil || startReceipt.Status != core.CommandSucceeded {
				t.Fatalf("результат подготовки: %+v, %v", startReceipt, err)
			}
			if phase == "observer-budget" {
				// observer содержит результат запрещённой бюджетом регистрации, а err
				// должен подтверждать исчерпание лимита наблюдений.
				if observer, err := f.service.OpenOutputObserver(ctx, fence); !errors.Is(err, core.ErrObservationLimit) || observer != nil {
					t.Fatalf("бюджет наблюдателя: %v, %v", observer, err)
				}
			} else {
				// old содержит первоначального наблюдателя выхода, а err сообщает об
				// ошибке его регистрации перед заменой.
				old, err := f.service.OpenOutputObserver(ctx, fence)
				if err != nil {
					t.Fatal(err)
				}
				// current содержит нового наблюдателя, который отзывает право old на
				// активацию отправителя.
				current, err := f.service.OpenOutputObserver(ctx, fence)
				if err != nil || old.Valid() || !current.Valid() {
					t.Fatalf("замена наблюдателя: %v", err)
				}
				if err = p.Activate(ctx, old); !errors.Is(err, core.ErrStale) {
					t.Fatalf("устаревший токен принят: %v", err)
				}
			}
			// duplicate содержит результат повторной подготовки тем же владельцем, а err
			// проверяется на занятый резерв выполнения.
			if duplicate, err := prepare(owner); !errors.Is(err, core.ErrInUse) || duplicate != nil {
				t.Fatalf("повторная подготовка: %v, %v", duplicate, err)
			}
			if err = f.service.ConfirmOutputExecutionResolution(ctx, p.Reservation()); !errors.Is(err, core.ErrNotReady) {
				t.Fatalf("пригодный ExecutionFence со статусом SUCCEEDED освобождён: %v", err)
			}
			publisherCompensationNoNetwork(t, p, &dials)
			// runtime содержит оперативный снимок родителя для проверки отсутствия
			// наблюдаемого состояния неактивного выхода.
			runtime, err := f.service.RuntimeSnapshot(ctx, f.observer.SessionFence())
			if err != nil {
				t.Fatal(err)
			}
			// observed обозначает очередное наблюдение выхода при поиске нежелательного
			// состояния компенсируемого ресурса.
			for _, observed := range runtime.Outputs {
				if observed.Execution.OutputID == target.ID && observed.Value != nil {
					t.Fatalf("неактивный выход получил наблюдаемое состояние: %+v", observed)
				}
			}
			// stopReceipt содержит квитанцию остановки компенсируемого выхода, ожидающую
			// завершения очистки.
			stopReceipt, err := f.service.StopOutput(ctx, core.OutputCommand{RequestID: "compensate-output", OutputID: target.ID})
			if err != nil || stopReceipt.Status != core.CommandPending {
				t.Fatalf("компенсация: %+v, %v", stopReceipt, err)
			}
			if err = p.Abort(ctx); err != nil {
				t.Fatal(err)
			}
			// result содержит итог прерванного отправителя для проверки подтверждённого
			// завершения сетевых ресурсов.
			result, err := p.Wait(ctx)
			if err != nil || !result.NetworkJoined {
				t.Fatalf("не завершён Publisher: %+v, %v", result, err)
			}
			if err = p.ReleaseJoined(); !errors.Is(err, core.ErrNotReady) {
				t.Fatalf("завершение без подтверждения управления освободило резерв: %v", err)
			}
			publisherCompensationNoNetwork(t, p, &dials)
			// output содержит долговечное состояние компенсируемого выхода до и после
			// подтверждения очистки.
			output, err := f.service.GetOutput(ctx, target.ID)
			if err != nil || output.DesiredRunning || !output.Attached || !output.Issued || !output.CleanupPending || output.Generation != fence.OutputGeneration+1 || output.Attempt != fence.Attempt {
				t.Fatalf("состояние компенсации: %+v, %v", output, err)
			}
			// cleanups содержит неподтверждённые намерения очистки сессии для поиска
			// единственного дочернего ресурса.
			cleanups, err := f.service.ListCleanups(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8, SessionID: f.parent.SessionID, PendingOnly: true}})
			if err != nil || len(cleanups.Items) != 1 {
				t.Fatalf("точная дочерняя очистка: %+v, %v", cleanups, err)
			}
			// intent содержит точное намерение очистки прежнего поколения и попытки
			// компенсируемого выхода.
			intent := cleanups.Items[0]
			if intent.Kind != core.CleanupRuntime || intent.OutputID != target.ID || intent.Incarnation != fence.Incarnation || intent.Lifecycle != fence.Lifecycle || intent.Generation != fence.OutputGeneration || intent.Attempt != fence.Attempt {
				t.Fatalf("неверный владелец очистки: %+v", intent)
			}
			// wrong содержит намеренно неверное подтверждение с увеличенным номером попытки.
			wrong := core.CleanupFenceFor(intent)
			wrong.Attempt++
			if err = f.service.AcknowledgeCleanup(ctx, wrong); !errors.Is(err, core.ErrStale) {
				t.Fatalf("принято чужое подтверждение: %v", err)
			}
			// unchanged содержит намерение очистки после отклонённого чужого подтверждения
			// для проверки его неизменности.
			unchanged, err := f.service.GetCleanup(ctx, intent.ID)
			if err != nil || !reflect.DeepEqual(unchanged, intent) {
				t.Fatalf("отклонённое подтверждение изменило очистку: %+v, %v", unchanged, err)
			}
			// Группа и её ссылка не создавались; физически завершён единственный ресурс этого выхода.
			if err = f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(intent)); err != nil {
				t.Fatal(err)
			}
			output, err = f.service.GetOutput(ctx, target.ID)
			if err != nil || output.CleanupPending || output.Issued || output.DesiredRunning {
				t.Fatalf("очистка выхода не завершена: %+v, %v", output, err)
			}
			cleanups, err = f.service.ListCleanups(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8, SessionID: f.parent.SessionID, PendingOnly: true}})
			if err != nil || len(cleanups.Items) != 0 {
				t.Fatalf("осталась дочерняя очистка: %+v, %v", cleanups, err)
			}
			// session содержит состояние родителя после очистки выхода для проверки
			// сохранения RUNNING и наблюдателя.
			session, err := f.service.GetSession(ctx, f.parent.SessionID)
			if err != nil || session.State != core.SessionRunning || !session.Issued || !f.observer.Valid() {
				t.Fatalf("завершение дочернего ресурса изменило родителя: %+v, %v", session, err)
			}
			// neighborAfter содержит состояние соседнего выхода после компенсации для
			// сравнения с neighborBefore.
			neighborAfter, err := f.service.GetOutput(ctx, neighbor.ID)
			if err != nil || !reflect.DeepEqual(neighborBefore, neighborAfter) {
				t.Fatalf("компенсация изменила соседний выход: %+v, %v", neighborAfter, err)
			}
			// receipt содержит перечитанную квитанцию остановки, а затем исходного запуска
			// для проверки их окончательных результатов.
			receipt, err := f.service.GetReceipt(ctx, stopReceipt.ID)
			if err != nil || receipt.Status != core.CommandSucceeded {
				t.Fatalf("stop не завершён очисткой: %+v, %v", receipt, err)
			}
			receipt, err = f.service.GetReceipt(ctx, fence.CommandID)
			if err != nil || !reflect.DeepEqual(receipt, startReceipt) {
				t.Fatalf("успешная квитанция переписана: %+v, %v", receipt, err)
			}
			if _, err = f.service.StartOutput(ctx, core.OutputCommand{RequestID: "restart-compensated-output", OutputID: target.ID}); err != nil {
				t.Fatal(err)
			}
			// nextFence содержит разрешение нового запуска выхода, а nextOwner — его
			// владельца для проверки удержания прежнего резерва.
			nextFence, nextOwner := publisherCompensationIssue(t, f, target)
			// duplicate содержит результат подготовки нового запуска при ещё удержанном
			// прежнем резерве, а err проверяется на ErrInUse.
			if duplicate, err := prepare(nextOwner); !errors.Is(err, core.ErrInUse) || duplicate != nil {
				t.Fatalf("очистка обошла удержание резерва исполнения: %v, %v", duplicate, err)
			}
			if err = f.service.ConfirmOutputExecutionResolution(ctx, p.Reservation()); err != nil {
				t.Fatal(err)
			}
			if err = p.ReleaseJoined(); err != nil {
				t.Fatal(err)
			}
			if err = p.ReleaseJoined(); err != nil {
				t.Fatalf("повторное освобождение: %v", err)
			}
			// next содержит отправитель нового запуска после подтверждения управления и
			// освобождения прежнего резерва.
			next, err := prepare(nextOwner)
			if err != nil {
				t.Fatalf("подтверждённое завершение не освободило слот: %v", err)
			}
			t.Cleanup(func() {
				// Прерывает следующего подготовленного отправителя перед освобождением фикстуры.

				// cleanup ограничивает прерывание следующего подготовленного отправителя
				// одной секундой, а stop освобождает таймер очистки.
				cleanup, stop := context.WithTimeout(context.Background(), time.Second)
				defer stop()
				// err сообщает об ошибке прерывания следующего подготовленного
				// отправителя при очистке теста.
				if err := next.Abort(cleanup); err != nil {
					t.Error(err)
				}
			})
			if err = f.service.Complete(ctx, nextFence, core.Completion{Success: false, Failure: "Локальная подготовка отменена"}); err != nil {
				t.Fatal(err)
			}
			if err = next.Abort(ctx); err != nil {
				t.Fatal(err)
			}
			if err = f.service.ConfirmOutputExecutionResolution(ctx, next.Reservation()); err != nil {
				t.Fatal(err)
			}
			if err = next.ReleaseJoined(); err != nil {
				t.Fatal(err)
			}
			publisherCompensationNoNetwork(t, next, &dials)
			publisherCompensationNoNetwork(t, p, &dials)
		})
	}
}

// Проверяет отсутствие попыток соединения, переданных и поставленных в очередь AU у Publisher, который
// не был активирован.
func publisherCompensationNoNetwork(t *testing.T, p *publisher.Publisher, dials *atomic.Uint64) {
	t.Helper()
	// s содержит снимок отправителя для проверки отсутствия попыток сети, отправленных и ожидающих
	// пакетов до активации.
	s := p.Snapshot()
	if dials.Load() != 0 || s.State == core.OutputPublishing || s.Attempts != 0 || s.Connections != 0 || s.WrittenBytes != 0 || s.AccessUnits != 0 || s.QueuedPackets != 0 || s.PendingPackets != 0 {
		t.Fatalf("неактивный Publisher начал передачу: dials=%d, %+v", dials.Load(), s)
	}
}
