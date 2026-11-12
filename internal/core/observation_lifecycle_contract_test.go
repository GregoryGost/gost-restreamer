package core

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

// Проверяет, что канал отзыва наблюдателя уже закрыт без ожидания.
func requireObservationDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	default:
		t.Fatal("канал завершения не закрыт")
	}
}

// Проверяет наличие ещё открытого канала действующего владельца наблюдений.
func requireObservationOpen(t *testing.T, done <-chan struct{}) {
	t.Helper()
	if done == nil {
		t.Fatal("канал завершения отсутствует")
	}
	select {
	case <-done:
		t.Fatal("действующий владелец завершён")
	default:
	}
}

// Проверяет очистку наблюдений только текущей попытки FFmpeg с сохранением FFmpegObserver и родителей,
// а также невозможность старых Close/Observe затронуть следующую попытку.
func TestFFmpegAttemptCloseRetiresOnlyCurrentAttempt(t *testing.T) {
	// ctx задаёт контекст операций проверяемого сценария оперативных наблюдений.
	ctx := context.Background()
	// s предоставляет доменные операции с общим реестром наблюдений. parent идентифицирует точное
	// выполнение родительской сессии.
	s, _, parent, _ := newObservationAcceptanceService(t, acceptanceObservationLimits())
	// session содержит наблюдателя текущего выполнения сессии. err сообщает об ошибке регистрации
	// наблюдателя точной сессии.
	session, err := s.OpenSessionObserver(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	// group содержит наблюдателя отдельной оперативной группы.
	group, err := session.OpenGroup(ctx, "group", "profile")
	if err != nil {
		t.Fatal(err)
	}
	// worker содержит наблюдателя работника FFmpeg внутри группы.
	worker, err := group.OpenFFmpeg(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// first содержит наблюдателя первой попытки FFmpeg.
	first, err := worker.BeginAttempt(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке записи оперативного состояния точного владельца.
	if err := first.Observe(ctx, FFmpegObservation{State: ObservationFailed, Error: "ошибка первой попытки"}); err != nil {
		t.Fatal(err)
	}
	// before сохраняет согласованный снимок до проверяемого действия.
	before, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if err != nil || len(before.FFmpeg) != 1 || before.FFmpeg[0].Value == nil || before.FFmpeg[0].Stamp.ObservedAt.IsZero() {
		t.Fatalf("исходное наблюдение отсутствует: %+v, %v", before, err)
	}
	// err сообщает об ошибке закрытия точного владельца наблюдений.
	if err := first.Close(ctx); err != nil {
		t.Fatal(err)
	}
	requireObservationDone(t, first.Done())
	if first.Valid() || !worker.Valid() || !group.Valid() || !session.Valid() {
		t.Fatal("закрытие попытки изменило допуск worker или его родителей")
	}
	requireObservationOpen(t, worker.Done())
	requireObservationOpen(t, group.Done())
	requireObservationOpen(t, session.Done())
	// after содержит согласованный снимок после проверяемого действия для проверки сохранности.
	after, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if err != nil || len(after.FFmpeg) != 1 || after.FFmpeg[0].Value != nil || !after.FFmpeg[0].Stamp.ObservedAt.IsZero() || after.FFmpeg[0].Stamp.InstanceID != before.FFmpeg[0].Stamp.InstanceID {
		t.Fatalf("попытка не убрана из текущего снимка либо удалён worker: %+v, %v", after, err)
	}
	// err сообщает об ошибке закрытия точного владельца наблюдений.
	if err := first.Close(ctx); err != nil && !errors.Is(err, ErrStale) {
		t.Fatalf("повторное закрытие попытки: %v", err)
	}
	// repeated содержит снимок после повторного закрытия попытки.
	repeated, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if err != nil || !reflect.DeepEqual(repeated, after) || first.Valid() || !worker.Valid() || !group.Valid() || !session.Valid() {
		t.Fatalf("повторное закрытие изменило снимок или владение: %+v, %v", repeated, err)
	}
	requireObservationDone(t, first.Done())
	requireObservationOpen(t, worker.Done())
	requireObservationOpen(t, group.Done())
	requireObservationOpen(t, session.Done())
	// err сообщает об ошибке регистрации точной попытки FFmpeg.
	if _, err := worker.BeginAttempt(ctx, 1); !errors.Is(err, ErrStale) {
		t.Fatalf("повтор номера завершённой попытки принят: %v", err)
	}
	// second содержит наблюдателя следующей попытки FFmpeg, заменяющей первую.
	second, err := worker.BeginAttempt(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке записи оперативного состояния точного владельца.
	if err := second.Observe(ctx, FFmpegObservation{State: ObservationReady}); err != nil {
		t.Fatal(err)
	}
	// current содержит снимок действующей замены для проверки защиты от поздних операций.
	current, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке закрытия точного владельца наблюдений.
	if err := first.Close(ctx); err != nil && !errors.Is(err, ErrStale) {
		t.Fatalf("закрытие старой попытки после замены: %v", err)
	}
	// unchanged содержит повторный снимок для проверки отсутствия изменений.
	unchanged, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if err != nil || !reflect.DeepEqual(unchanged, current) || first.Valid() || !second.Valid() || !worker.Valid() || !group.Valid() || !session.Valid() {
		t.Fatalf("старая попытка изменила следующую: %+v, %v", unchanged, err)
	}
	requireObservationDone(t, first.Done())
	requireObservationOpen(t, second.Done())
	requireObservationOpen(t, worker.Done())
	requireObservationOpen(t, group.Done())
	requireObservationOpen(t, session.Done())
	// err сообщает об ошибке записи оперативного состояния точного владельца.
	if err := first.Observe(ctx, FFmpegObservation{State: ObservationFailed, Error: "запоздалый отказ"}); !errors.Is(err, ErrStale) {
		t.Fatalf("запоздалое наблюдение принято: %v", err)
	}
	unchanged, err = s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if err != nil || !reflect.DeepEqual(unchanged, current) || !second.Valid() {
		t.Fatalf("старая попытка изменила следующую: %+v, %v", unchanged, err)
	}
	if len(current.FFmpeg) != 1 || current.FFmpeg[0].Value == nil || *current.FFmpeg[0].Value != (FFmpegObservation{State: ObservationReady}) || current.FFmpeg[0].Stamp.AttemptID != 2 || current.FFmpeg[0].Stamp.Revision <= before.FFmpeg[0].Stamp.Revision {
		t.Fatalf("новая попытка не опубликовала независимое значение: %+v", current)
	}
	// err сообщает об ошибке закрытия точного владельца наблюдений.
	if err := worker.Close(ctx); err != nil {
		t.Fatal(err)
	}
	requireObservationDone(t, worker.Done())
	requireObservationDone(t, second.Done())
	if worker.Valid() || second.Valid() || !group.Valid() || !session.Valid() {
		t.Fatal("закрытие worker нарушило границу владения")
	}
	requireObservationOpen(t, group.Done())
	requireObservationOpen(t, session.Done())
	// retired содержит снимок после удаления работника FFmpeg.
	retired, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if err != nil || len(retired.FFmpeg) != 0 {
		t.Fatalf("закрытый worker остался в снимке: %+v, %v", retired, err)
	}
	// err сообщает об ошибке закрытия точного владельца наблюдений.
	if err := second.Close(ctx); err != nil && !errors.Is(err, ErrStale) {
		t.Fatalf("закрытие попытки удалённого worker: %v", err)
	}
	// err сообщает об ошибке записи оперативного состояния точного владельца.
	if err := second.Observe(ctx, FFmpegObservation{State: ObservationReady}); !errors.Is(err, ErrStale) {
		t.Fatalf("наблюдение удалённого worker принято: %v", err)
	}
	unchanged, err = s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if err != nil || !reflect.DeepEqual(unchanged, retired) || first.Valid() || second.Valid() || worker.Valid() || !group.Valid() || !session.Valid() {
		t.Fatalf("попытка удалённого worker изменила снимок или родителей: %+v, %v", unchanged, err)
	}
	requireObservationDone(t, first.Done())
	requireObservationDone(t, second.Done())
	requireObservationDone(t, worker.Done())
	requireObservationOpen(t, group.Done())
	requireObservationOpen(t, session.Done())
}

// Проверяет сохранение значений, владельцев и счётчиков обращений к БД при некорректных наблюдениях и
// отменённых операциях реестра.
func TestObservationFailuresAndCancellationPreserveCurrentSnapshot(t *testing.T) {
	// ctx задаёт контекст операций проверяемого сценария оперативных наблюдений.
	ctx := context.Background()
	// s предоставляет доменные операции с общим реестром наблюдений. u предоставляет изменяемые
	// тестовые репозитории и учитывает обращения к БД. parent идентифицирует точное выполнение
	// родительской сессии. child идентифицирует точную попытку дочернего выхода.
	s, u, parent, child := newObservationAcceptanceService(t, acceptanceObservationLimits())
	// session содержит наблюдателя текущего выполнения сессии. err сообщает об ошибке регистрации
	// наблюдателя точной сессии.
	session, err := s.OpenSessionObserver(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	// group содержит наблюдателя отдельной оперативной группы.
	group, err := session.OpenGroup(ctx, "group", "profile")
	if err != nil {
		t.Fatal(err)
	}
	// worker содержит наблюдателя работника FFmpeg внутри группы.
	worker, err := group.OpenFFmpeg(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// attempt содержит наблюдателя текущей попытки FFmpeg.
	attempt, err := worker.BeginAttempt(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	// output содержит наблюдателя точной попытки выхода.
	output, err := s.OpenOutputObserver(ctx, child)
	if err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке очередной записи исходного корректного наблюдения.
	for _, err := range []error{
		session.Observe(ctx, SessionObservation{State: ObservationReady}),
		group.Observe(ctx, GroupObservation{State: ObservationReady, Source: ObservationSourceLive}),
		attempt.Observe(ctx, FFmpegObservation{State: ObservationReady}),
		output.Observe(ctx, OutputObservation{State: OutputPublishing}),
	} {
		if err != nil {
			t.Fatal(err)
		}
	}
	// before сохраняет согласованный снимок до проверяемого действия.
	before, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if err != nil {
		t.Fatal(err)
	}
	// views сохраняет число чтений БД перед проверяемой операцией RAM.
	views := u.views
	// err сообщает об ошибке отклонения очередного недопустимого наблюдения.
	for _, err := range []error{
		session.Observe(ctx, SessionObservation{State: "invalid"}),
		group.Observe(ctx, GroupObservation{State: "invalid", Source: ObservationSourceLive}),
		group.Observe(ctx, GroupObservation{State: ObservationReady, Source: "invalid"}),
		attempt.Observe(ctx, FFmpegObservation{State: "invalid"}),
		output.Observe(ctx, OutputObservation{State: OutputStopped}),
		session.Observe(ctx, SessionObservation{State: ObservationFailed, Error: string([]byte{0xff})}),
		group.Observe(ctx, GroupObservation{State: ObservationFailed, Source: ObservationSourceNone, Error: "a\x00b"}),
	} {
		if !errors.Is(err, ErrInvalid) {
			t.Fatalf("неверное наблюдение принято: %v", err)
		}
	}
	// cancelled задаёт контекст, отменяемый до вызова операций реестра. cancel отменяет проверяемый
	// контекст до обращения к реестру.
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	// operation содержит очередную операцию, вызываемую с отменённым контекстом.
	for _, operation := range []struct {
		// name задаёт имя проверяемого сценария регистрации или отмены.
		name string
		// run вызывает выбранную операцию реестра для проверки её результата.
		run func() error
	}{
		{"session_observe", func() error {
			// Пробует изменить наблюдение Session с уже отменённым контекстом.
			return session.Observe(cancelled, SessionObservation{State: ObservationFailed})
		}},
		{"group_observe", func() error {
			// Пробует изменить наблюдение группы с уже отменённым контекстом.

			return group.Observe(cancelled, GroupObservation{State: ObservationFailed, Source: ObservationSourceNone})
		}},
		{"output_observe", func() error {
			// Пробует изменить наблюдение выхода с уже отменённым контекстом.
			return output.Observe(cancelled, OutputObservation{State: OutputRetrying})
		}},
		{"attempt_observe", func() error {
			// Пробует изменить наблюдение попытки FFmpeg с уже отменённым контекстом.
			return attempt.Observe(cancelled, FFmpegObservation{State: ObservationFailed})
		}},
		{"attempt_close", func() error {
			// Пробует закрыть попытку FFmpeg с уже отменённым контекстом.
			return attempt.Close(cancelled)
		}},
		{"worker_close", func() error {
			// Пробует закрыть владельца FFmpeg с уже отменённым контекстом.
			return worker.Close(cancelled)
		}},
		{"begin_attempt", func() error {
			// Пробует открыть следующую попытку FFmpeg с уже отменённым контекстом.
			// err сообщает об ошибке регистрации точной попытки FFmpeg.
			_, err := worker.BeginAttempt(cancelled, 2)
			return err
		}},
		{"open_group", func() error {
			// Пробует открыть дополнительную группу с уже отменённым контекстом.
			// err сообщает об ошибке регистрации независимой оперативной группы.
			_, err := session.OpenGroup(cancelled, "another", "profile")
			return err
		}},
		{"open_worker", func() error {
			// Пробует открыть владельца FFmpeg с уже отменённым контекстом.
			// err сообщает об ошибке регистрации работника FFmpeg.
			_, err := group.OpenFFmpeg(cancelled)
			return err
		}},
		{"open_session", func() error {
			// Пробует заменить наблюдателя Session с уже отменённым контекстом.
			// err сообщает об ошибке регистрации наблюдателя точной сессии.
			_, err := s.OpenSessionObserver(cancelled, parent)
			return err
		}},
		{"open_output", func() error {
			// Пробует заменить наблюдателя выхода с уже отменённым контекстом.
			// err сообщает об ошибке регистрации наблюдателя точной попытки выхода.
			_, err := s.OpenOutputObserver(cancelled, child)
			return err
		}},
		{"snapshot", func() error {
			// Пробует получить оперативный снимок с уже отменённым контекстом.
			// err сообщает об ошибке чтения текущих наблюдений сессии.
			_, err := s.RuntimeSnapshot(cancelled, observationSessionFence(parent))
			return err
		}},
	} {
		t.Run(operation.name, func(t *testing.T) {
			// Проверяет context.Canceled выбранной операции до изменения снимка или обращения к БД.

			// err сообщает об ошибке выполнения выбранного сценария регистрации или отмены.
			if err := operation.run(); !errors.Is(err, context.Canceled) {
				t.Fatalf("отмена потеряна: %v", err)
			}
		})
	}
	// after содержит согласованный снимок после проверяемого действия для проверки сохранности.
	after, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if err != nil || !reflect.DeepEqual(before, after) || !session.Valid() || !group.Valid() || !worker.Valid() || !attempt.Valid() || !output.Valid() {
		t.Fatalf("отказ или отмена изменили прежний снимок: %+v, %v", after, err)
	}
	if views != u.views || u.writes != 0 {
		t.Fatalf("отказавшее RAM-наблюдение обратилось к БД: views=%d/%d writes=%d", u.views, views, u.writes)
	}
}

// Проверяет сохранение действующего OutputObserver и снимка при неверном виде ExecutionFence, нулевой
// попытке и незавершённой квитанции.
func TestRejectedObserverRegistrationPreservesExistingOwner(t *testing.T) {
	// ctx задаёт контекст операций проверяемого сценария оперативных наблюдений.
	ctx := context.Background()
	// s предоставляет доменные операции с общим реестром наблюдений. u предоставляет изменяемые
	// тестовые репозитории и учитывает обращения к БД. parent идентифицирует точное выполнение
	// родительской сессии. child идентифицирует точную попытку дочернего выхода.
	s, u, parent, child := newObservationAcceptanceService(t, acceptanceObservationLimits())
	// owner содержит наблюдателя точной попытки выхода. err сообщает об ошибке регистрации
	// наблюдателя точной попытки выхода.
	owner, err := s.OpenOutputObserver(ctx, child)
	if err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке записи оперативного состояния точного владельца.
	if err := owner.Observe(ctx, OutputObservation{State: OutputPublishing}); err != nil {
		t.Fatal(err)
	}
	// before сохраняет согласованный снимок до проверяемого действия.
	before, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if err != nil {
		t.Fatal(err)
	}
	// tc содержит выбранный отказ регистрации и ожидаемую категорию ошибки.
	for _, tc := range []struct {
		// name задаёт имя проверяемого сценария регистрации или отмены.
		name string
		// run вызывает выбранную операцию реестра для проверки её результата.
		run func() error
		// want задаёт ожидаемую категорию отказа проверяемого сценария.
		want error
	}{
		{"session_fence_for_output", func() error {
			// Пробует открыть наблюдателя выхода с ExecutionFence родительской Session.
			// err сообщает об ошибке регистрации наблюдателя точной попытки выхода.
			_, err := s.OpenOutputObserver(ctx, parent)
			return err
		}, ErrInvalid},
		{"output_fence_for_session", func() error {
			// Пробует открыть наблюдателя Session с ExecutionFence выхода.
			// err сообщает об ошибке регистрации наблюдателя точной сессии.
			_, err := s.OpenSessionObserver(ctx, child)
			return err
		}, ErrInvalid},
		{"invalid_fence", func() error {
			// Обнуляет Attempt копии ExecutionFence перед регистрацией наблюдателя выхода.

			// invalid содержит заведомо недопустимый вход для проверки отказа без замены
			// владельца.
			invalid := child
			invalid.Attempt = 0
			// err сообщает об ошибке регистрации наблюдателя точной попытки выхода.
			_, err := s.OpenOutputObserver(ctx, invalid)
			return err
		}, ErrInvalid},
		{"unfinished_receipt", func() error {
			// Меняет квитанцию выхода с SUCCEEDED на ISSUED перед попыткой заменить наблюдателя.

			// receipt содержит копию квитанции выхода для изменения её статуса.
			receipt := u.repository.receipts[child.CommandID]
			receipt.Status = CommandIssued
			u.repository.receipts[child.CommandID] = receipt
			// err сообщает об ошибке регистрации наблюдателя точной попытки выхода.
			_, err := s.OpenOutputObserver(ctx, child)
			return err
		}, ErrStale},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет ожидаемый ErrInvalid либо ErrStale регистрации, после которого прежний наблюдатель и
			// снимок сохраняются.

			// err сообщает об ошибке выполнения выбранного сценария регистрации или отмены.
			if err := tc.run(); !errors.Is(err, tc.want) {
				t.Fatalf("неверный допуск регистрации: %v", err)
			}
		})
	}
	// after содержит согласованный снимок после проверяемого действия для проверки сохранности.
	after, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if err != nil || !reflect.DeepEqual(before, after) || !owner.Valid() {
		t.Fatalf("отказ регистрации уничтожил текущего владельца: %+v, %v", after, err)
	}
}
