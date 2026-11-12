package storage

import (
	"context"
	"reflect"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// Проверяет освобождение удержаний сразу для невыданного отказа и только после точной родительской
// очистки для выданного запуска.
func TestFailedStartupReleasesPinsOnlyAfterIssuedParentCleanup(t *testing.T) {
	// issued выбирает выдачу родительской попытки перед отказом запуска.
	for _, issued := range []bool{false, true} {
		// name задаёт имя проверяемого сценария очистки.
		name := "unissued"
		if issued {
			name = "issued"
		}
		t.Run(name, func(t *testing.T) {
			// Проверяет, что отказ запуска требует очистки родителя и сохраняет удержания только после MarkIssued,
			// а невыданный запуск освобождает зависимости сразу.

			// f объединяет временный Store, Service, поток и опубликованную заглушку
			// проверяемого сценария.
			f := newDomainFixture(t, 1)
			// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
			ctx := context.Background()
			// start содержит принятую квитанцию первоначального запуска сессии.
			start := startDomainSession(t, f)
			// before сохраняет предварительный выход до отказа запуска родителя.
			before := readOutputs(t, f.store, start.SessionID)[0]
			// fence идентифицирует точную захваченную попытку проверяемой команды. err
			// сообщает об ошибке захвата точной команды исполнения.
			fence, err := f.service.Claim(ctx, start.RequestID)
			if err != nil {
				t.Fatal(err)
			}
			if issued {
				// err сообщает об ошибке выдачи точной попытки команды.
				if err := f.service.MarkIssued(ctx, fence); err != nil {
					t.Fatal(err)
				}
			}
			// err сообщает об ошибке сохранения результата точной команды.
			if err := f.service.Complete(ctx, fence, core.Completion{Failure: "Не удалось подготовить группу"}); err != nil {
				t.Fatal(err)
			}
			// session содержит сохраняемый запуск для проверки состояния и владения.
			session := readSession(t, f.store, start.SessionID)
			if session.State != core.SessionFailed || session.EndedAt == nil || session.CleanupPending != issued {
				t.Fatalf("неверное завершение запуска: %+v", session)
			}
			// after содержит предварительный выход после отказа запуска родителя.
			after := readOutputs(t, f.store, start.SessionID)[0]
			if after.Issued || after.Attempt != 0 || !reflect.DeepEqual(before.Snapshot, after.Snapshot) {
				t.Fatalf("отказ родителя изменил снимок или выдумал publisher: %+v", after)
			}
			// cleanups содержит сохраняемые намерения физической очистки после проверяемой
			// операции.
			cleanups := pendingCleanups(t, f.store, start.SessionID)
			if issued {
				if len(cleanups) != 1 || cleanups[0].OutputID != "" {
					t.Fatalf("нужна отдельная очистка родительской попытки: %+v", cleanups)
				}
				requireOutputVariantHolds(t, f.store, f.variant.ID, true)
				requireDomainError(t, f.service.DeleteVariant(ctx, f.variant.ID, f.variant.Revision), core.ErrInUse)
				requireDomainError(t, f.service.DeleteDestination(ctx, f.destinations[0].ID, f.destinations[0].Revision), core.ErrInUse)
				// wrong содержит копию точной идентичности с намеренно подменённым
				// полем.
				wrong := core.CleanupFenceFor(cleanups[0])
				wrong.Lifecycle++
				requireDomainError(t, f.service.AcknowledgeCleanup(ctx, wrong), core.ErrStale)
				requireOutputVariantHolds(t, f.store, f.variant.ID, true)
				requireDomainError(t, f.service.DeleteVariant(ctx, f.variant.ID, f.variant.Revision), core.ErrInUse)
				// err сообщает об ошибке подтверждения точного намерения очистки.
				if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(cleanups[0])); err != nil {
					t.Fatal(err)
				}
			} else if len(cleanups) != 0 {
				t.Fatalf("невыданная попытка требует очистки: %+v", cleanups)
			}
			requireOutputVariantHolds(t, f.store, f.variant.ID, false)
			// err сообщает об ошибке удаления больше не удерживаемого назначения.
			if err := f.service.DeleteDestination(ctx, f.destinations[0].ID, f.destinations[0].Revision); err != nil {
				t.Fatal(err)
			}
			// err сообщает об ошибке удаления больше не удерживаемой группы.
			if err := f.service.DeleteGroup(ctx, f.group.ID, f.group.Revision); err != nil {
				t.Fatal(err)
			}
			// err сообщает об ошибке удаления профиля при сохраняемых ссылках.
			if err := f.service.DeleteProfile(ctx, f.profile.ID, f.profile.Revision); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Проверяет сохранение удержаний до обоих независимых подтверждений Session/Publisher в любом порядке
// и идемпотентность повторного подтверждения.
func TestParentAndPublisherCleanupRetainPinsInEitherAcknowledgementOrder(t *testing.T) {
	// parentFirst выбирает порядок подтверждения очистки родителя и отправителя.
	for _, parentFirst := range []bool{false, true} {
		// name задаёт имя проверяемого сценария очистки.
		name := "publisher_first"
		if parentFirst {
			name = "parent_first"
		}
		t.Run(name, func(t *testing.T) {
			// Проверяет сохранение удержаний до обоих точных подтверждений очистки родителя и отправителя при
			// любом порядке подтверждения.

			// f объединяет временный Store, Service, поток и опубликованную заглушку
			// проверяемого сценария.
			f := newDomainFixture(t, 1)
			// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
			ctx := context.Background()
			// start содержит принятую квитанцию первоначального запуска сессии.
			start := startDomainSession(t, f)
			executeCommand(t, f.service, start.RequestID)
			// output содержит сохраняемый выход с самостоятельным снимком и намерением.
			output := readOutputs(t, f.store, start.SessionID)[0]
			executeCommand(t, f.service, outputStartReceipt(t, f, output.ID).RequestID)
			f = reopenFixture(t, f)
			// cleanups содержит сохраняемые намерения физической очистки после проверяемой
			// операции.
			cleanups := pendingCleanups(t, f.store, start.SessionID)
			if len(cleanups) != 2 {
				t.Fatalf("утрачено независимое владение родителя или publisher: %+v", cleanups)
			}
			if (cleanups[0].OutputID == "") != parentFirst {
				cleanups[0], cleanups[1] = cleanups[1], cleanups[0]
			}
			// err сообщает об ошибке подтверждения точного намерения очистки.
			if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(cleanups[0])); err != nil {
				t.Fatal(err)
			}
			// remaining содержит оставшиеся неподтверждённые намерения после повторного
			// старого подтверждения.
			remaining := pendingCleanups(t, f.store, start.SessionID)
			if len(remaining) != 1 || remaining[0].ID != cleanups[1].ID {
				t.Fatalf("первый ack поглотил чужую очистку: %+v", remaining)
			}
			requireOutputVariantHolds(t, f.store, f.variant.ID, true)
			requireDomainError(t, f.service.DeleteVariant(ctx, f.variant.ID, f.variant.Revision), core.ErrInUse)
			// err сообщает об ошибке подтверждения точного намерения очистки.
			if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(cleanups[0])); err != nil {
				t.Fatal(err)
			}
			requireOutputVariantHolds(t, f.store, f.variant.ID, true)
			requireDomainError(t, f.service.DeleteVariant(ctx, f.variant.ID, f.variant.Revision), core.ErrInUse)
			// err сообщает об ошибке подтверждения точного намерения очистки.
			if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(cleanups[1])); err != nil {
				t.Fatal(err)
			}
			// session содержит сохраняемый запуск для проверки состояния и владения.
			if session := readSession(t, f.store, start.SessionID); session.CleanupPending || session.State != core.SessionInterrupted {
				t.Fatalf("оба ack не завершили очистку прерванного запуска: %+v", session)
			}
			requireOutputVariantHolds(t, f.store, f.variant.ID, false)
		})
	}
}

// Добавляет отдельные профиль, группу и назначение, публикует общий новый комплект и обновляет
// тестовые ссылки на варианты обоих профилей.
func isolatedOutputDependency(t *testing.T, f *domainFixture) (core.TranscodeProfile, core.TranscodeGroup, core.FallbackVariant, core.Destination) {
	t.Helper()
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// profile содержит параметры отдельного профиля с отличающимся GOP.
	profile := storageProfile()
	profile.VideoEncoding.GOPFrames++
	// p содержит полный именованный профиль для проверяемого изменения. err сообщает об ошибке
	// сохранения полного именованного профиля.
	p, err := f.service.CreateProfile(ctx, core.TranscodeProfile{Name: "Отдельный профиль", Profile: profile})
	if err != nil {
		t.Fatal(err)
	}
	// g содержит конфигурацию группы тестового потока.
	g, err := f.service.CreateGroup(ctx, core.TranscodeGroup{StreamID: f.stream.ID, ProfileID: p.ID, Name: "Отдельная группа"})
	if err != nil {
		t.Fatal(err)
	}
	// a содержит стабильную заглушку проверяемого поколения.
	a, _ := publishFixtureGeneration(t, f.service, f.asset, "replacement-source", "replacement-content")
	f.asset = a
	// page содержит варианты нового опубликованного комплекта для обоих профилей.
	page, err := f.service.ListVariants(ctx, core.PageRequest{Query: core.ListQuery{Limit: 20, GenerationID: a.CurrentGenerationID}})
	if err != nil {
		t.Fatal(err)
	}
	// v содержит опубликованный вариант точного поколения.
	var v core.FallbackVariant
	// candidate содержит очередной вариант для выбора по полному профилю.
	for _, candidate := range page.Items {
		if candidate.Profile.Equal(p.Profile) {
			v = candidate
		} else if candidate.Profile.Equal(f.profile.Profile) {
			f.variant = candidate
		}
	}
	if v.ID == "" {
		t.Fatal("не подготовлен отдельный профиль")
	}
	// d содержит настройки проверяемого назначения.
	d, err := f.service.CreateDestination(ctx, core.Destination{StreamID: f.stream.ID, GroupID: g.ID, Name: "Отдельный выход", Platform: "custom", Endpoint: "rtmps://example.invalid/isolated", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	return p, g, v, d
}

// Проверяет точную очистку предварительной группы отсоединённого невыданного выхода под живой Session,
// изоляцию соседа и защиту удержаний последующей замены от старого подтверждения.
func TestDetachUnissuedPublisherCleansOnlyItsGroupUnderLiveIssuedParent(t *testing.T) {
	// stopped выбирает предварительную остановку выхода перед отсоединением.
	for _, stopped := range []bool{false, true} {
		// name задаёт имя проверяемого сценария очистки.
		name := "connecting"
		if stopped {
			name = "already_stopped"
		}
		t.Run(name, func(t *testing.T) {
			// Проверяет точную очистку группы отсоединённого невыданного выхода, включая предварительную
			// остановку, без изменения живого родителя, соседа и удержаний замены.

			// f объединяет временный Store, Service, поток и опубликованную заглушку
			// проверяемого сценария.
			f := newDomainFixture(t, 1)
			// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
			ctx := context.Background()
			// p содержит полный именованный профиль для проверяемого изменения. g содержит
			// конфигурацию группы тестового потока. variant содержит точный опубликованный
			// вариант тестового поколения. destination содержит назначение отдельной группы,
			// удержания которого проверяются независимо.
			p, g, variant, destination := isolatedOutputDependency(t, &f)
			// request содержит запрос запуска с двумя независимыми группами.
			request := f.startRequest("start-with-independent-groups")
			request.DestinationIDs = append(request.DestinationIDs, destination.ID)
			request.Bindings = append(request.Bindings, core.PreparedBinding{GenerationID: variant.GenerationID, GroupID: g.ID, VariantID: variant.ID, GeneratorVersion: variant.GeneratorVersion})
			// start содержит принятую квитанцию первоначального запуска сессии. err сообщает
			// об ошибке принятия ручного запуска потока.
			start, err := f.service.StartSession(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			executeCommand(t, f.service, start.RequestID)
			// target содержит невыданный выход отдельной группы, который отсоединяется.
			// neighbor сохраняет выданный выход исходной группы для проверки изоляции.
			var target, neighbor core.SessionOutput
			// output содержит сохраняемый выход с самостоятельным снимком и намерением.
			for _, output := range readOutputs(t, f.store, start.SessionID) {
				if output.Snapshot.Destination.ID == destination.ID {
					target = output
				} else {
					neighbor = output
				}
			}
			// neighborFence идентифицирует точную выданную попытку здорового соседа.
			neighborFence := executeCommand(t, f.service, outputStartReceipt(t, f, neighbor.ID).RequestID)
			// neighborObserver содержит наблюдателя здорового соседа для проверки
			// сохранности его допуска.
			neighborObserver := openOutputObserver(t, f.service, neighborFence)
			neighbor, err = f.service.GetOutput(ctx, neighbor.ID)
			if err != nil {
				t.Fatal(err)
			}
			if stopped {
				// err сообщает об ошибке принятия остановки отдельного выхода.
				if _, err := f.service.StopOutput(ctx, core.OutputCommand{RequestID: "stop-before-detach", OutputID: target.ID}); err != nil {
					t.Fatal(err)
				}
			}
			// err сообщает об ошибке принятия отсоединения выхода.
			if _, err := f.service.DetachOutput(ctx, core.OutputCommand{RequestID: "detach-without-publisher", OutputID: target.ID}); err != nil {
				t.Fatal(err)
			}
			// detached содержит отсоединённый выход с точным обязательством очистки группы.
			detached, err := f.service.GetOutput(ctx, target.ID)
			if err != nil {
				t.Fatal(err)
			}
			if detached.Attached || detached.DesiredRunning || !detached.CleanupPending || detached.Issued || detached.Attempt != 0 {
				t.Fatalf("очистка группы выдумала publisher или потеряла владение: %+v", detached)
			}
			// cleanups содержит сохраняемые намерения физической очистки после проверяемой
			// операции.
			cleanups := pendingCleanups(t, f.store, start.SessionID)
			if len(cleanups) != 1 || cleanups[0].OutputID != target.ID {
				t.Fatalf("detach не создал точную очистку своей группы: %+v", cleanups)
			}
			// wrong содержит копию точной идентичности с намеренно подменённым полем.
			wrong := core.CleanupFenceFor(cleanups[0])
			wrong.OutputID = neighbor.ID
			requireDomainError(t, f.service.AcknowledgeCleanup(ctx, wrong), core.ErrStale)
			requireOutputVariantHolds(t, f.store, variant.ID, true)
			requireDomainError(t, f.service.DeleteVariant(ctx, variant.ID, variant.Revision), core.ErrInUse)
			// err сообщает об ошибке подтверждения точного намерения очистки.
			if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(cleanups[0])); err != nil {
				t.Fatal(err)
			}
			// current содержит здоровый соседний выход после очистки отдельной группы. err
			// сообщает об ошибке чтения соседнего выхода.
			current, err := f.service.GetOutput(ctx, neighbor.ID)
			if err != nil || !reflect.DeepEqual(current, neighbor) {
				t.Fatalf("detach изменил здорового соседа: %+v, %v", current, err)
			}
			// session содержит сохраняемый запуск для проверки состояния и владения.
			if session := readSession(t, f.store, start.SessionID); session.State != core.SessionRunning || !session.Issued || session.CleanupPending {
				t.Fatalf("очистка группы затронула живого родителя: %+v", session)
			}
			// replacement содержит квитанцию нового выхода того же назначения.
			replacement, err := f.service.AddOutput(ctx, core.AddOutputRequest{RequestID: "attach-new-output", SessionID: start.SessionID, DestinationID: destination.ID, Binding: core.PreparedBinding{GenerationID: variant.GenerationID, GroupID: g.ID, VariantID: variant.ID, GeneratorVersion: variant.GeneratorVersion}})
			if err != nil {
				t.Fatal(err)
			}
			executeCommand(t, f.service, replacement.RequestID)
			// err сообщает об ошибке подтверждения точного намерения очистки.
			if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(cleanups[0])); err != nil {
				t.Fatal(err)
			}
			requireOutputVariantHolds(t, f.store, variant.ID, true)
			requireDomainError(t, f.service.DeleteVariant(ctx, variant.ID, variant.Revision), core.ErrInUse)
			// err сообщает об ошибке принятия отсоединения выхода.
			if _, err := f.service.DetachOutput(ctx, core.OutputCommand{RequestID: "detach-replacement", OutputID: replacement.OutputID}); err != nil {
				t.Fatal(err)
			}
			// pending содержит очистку новой замены после её отсоединения.
			pending := pendingCleanups(t, f.store, start.SessionID)
			if len(pending) != 1 || pending[0].OutputID != replacement.OutputID {
				t.Fatalf("потеряно владение новым выходом: %+v", pending)
			}
			// err сообщает об ошибке подтверждения точного намерения очистки.
			if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(cleanups[0])); err != nil {
				t.Fatal(err)
			}
			requireOutputVariantHolds(t, f.store, variant.ID, true)
			requireDomainError(t, f.service.DeleteVariant(ctx, variant.ID, variant.Revision), core.ErrInUse)
			// err сообщает об ошибке подтверждения точного намерения очистки.
			if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(pending[0])); err != nil {
				t.Fatal(err)
			}
			requireOutputVariantHolds(t, f.store, variant.ID, false)
			// err сообщает об ошибке удаления больше не удерживаемого назначения.
			if err := f.service.DeleteDestination(ctx, destination.ID, destination.Revision); err != nil {
				t.Fatal(err)
			}
			// err сообщает об ошибке удаления больше не удерживаемой группы.
			if err := f.service.DeleteGroup(ctx, g.ID, g.Revision); err != nil {
				t.Fatal(err)
			}
			// err сообщает об ошибке удаления профиля при сохраняемых ссылках.
			if err := f.service.DeleteProfile(ctx, p.ID, p.Revision); err != nil {
				t.Fatal(err)
			}
			// err сообщает об ошибке подтверждения точного намерения очистки.
			if err := f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(cleanups[0])); err != nil {
				t.Fatal(err)
			}
			requireOutputVariantHolds(t, f.store, f.variant.ID, true)
			requireDomainError(t, f.service.DeleteVariant(ctx, f.variant.ID, f.variant.Revision), core.ErrInUse)
			// err сообщает об ошибке записи наблюдаемого состояния точного выхода.
			if err := neighborObserver.Observe(ctx, core.OutputObservation{State: core.OutputRetrying, Error: "Временный отказ сети"}); err != nil {
				t.Fatalf("очистка группы отозвала допуск здорового соседа: %v", err)
			}
		})
	}
}
