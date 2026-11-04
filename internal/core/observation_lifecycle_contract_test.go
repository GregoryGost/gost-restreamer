package core

import (
	"context"
	"errors"
	"reflect"
	"testing"
)

func requireObservationDone(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	default:
		t.Fatal("канал завершения не закрыт")
	}
}

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

func TestFFmpegAttemptCloseRetiresOnlyCurrentAttempt(t *testing.T) {
	ctx := context.Background()
	s, _, parent, _ := newObservationAcceptanceService(t, acceptanceObservationLimits())
	session, err := s.OpenSessionObserver(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	group, err := session.OpenGroup(ctx, "group", "profile")
	if err != nil {
		t.Fatal(err)
	}
	worker, err := group.OpenFFmpeg(ctx)
	if err != nil {
		t.Fatal(err)
	}
	first, err := worker.BeginAttempt(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Observe(ctx, FFmpegObservation{State: ObservationFailed, Error: "ошибка первой попытки"}); err != nil {
		t.Fatal(err)
	}
	before, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if err != nil || len(before.FFmpeg) != 1 || before.FFmpeg[0].Value == nil || before.FFmpeg[0].Stamp.ObservedAt.IsZero() {
		t.Fatalf("исходное наблюдение отсутствует: %+v, %v", before, err)
	}
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
	after, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if err != nil || len(after.FFmpeg) != 1 || after.FFmpeg[0].Value != nil || !after.FFmpeg[0].Stamp.ObservedAt.IsZero() || after.FFmpeg[0].Stamp.InstanceID != before.FFmpeg[0].Stamp.InstanceID {
		t.Fatalf("попытка не убрана из текущего снимка либо удалён worker: %+v, %v", after, err)
	}
	if err := first.Close(ctx); err != nil && !errors.Is(err, ErrStale) {
		t.Fatalf("повторное закрытие попытки: %v", err)
	}
	repeated, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if err != nil || !reflect.DeepEqual(repeated, after) || first.Valid() || !worker.Valid() || !group.Valid() || !session.Valid() {
		t.Fatalf("повторное закрытие изменило снимок или владение: %+v, %v", repeated, err)
	}
	requireObservationDone(t, first.Done())
	requireObservationOpen(t, worker.Done())
	requireObservationOpen(t, group.Done())
	requireObservationOpen(t, session.Done())
	if _, err := worker.BeginAttempt(ctx, 1); !errors.Is(err, ErrStale) {
		t.Fatalf("повтор номера завершённой попытки принят: %v", err)
	}
	second, err := worker.BeginAttempt(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err := second.Observe(ctx, FFmpegObservation{State: ObservationReady}); err != nil {
		t.Fatal(err)
	}
	current, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if err != nil {
		t.Fatal(err)
	}
	if err := first.Close(ctx); err != nil && !errors.Is(err, ErrStale) {
		t.Fatalf("закрытие старой попытки после замены: %v", err)
	}
	unchanged, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if err != nil || !reflect.DeepEqual(unchanged, current) || first.Valid() || !second.Valid() || !worker.Valid() || !group.Valid() || !session.Valid() {
		t.Fatalf("старая попытка изменила следующую: %+v, %v", unchanged, err)
	}
	requireObservationDone(t, first.Done())
	requireObservationOpen(t, second.Done())
	requireObservationOpen(t, worker.Done())
	requireObservationOpen(t, group.Done())
	requireObservationOpen(t, session.Done())
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
	retired, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if err != nil || len(retired.FFmpeg) != 0 {
		t.Fatalf("закрытый worker остался в снимке: %+v, %v", retired, err)
	}
	if err := second.Close(ctx); err != nil && !errors.Is(err, ErrStale) {
		t.Fatalf("закрытие попытки удалённого worker: %v", err)
	}
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

func TestObservationFailuresAndCancellationPreserveCurrentSnapshot(t *testing.T) {
	ctx := context.Background()
	s, u, parent, child := newObservationAcceptanceService(t, acceptanceObservationLimits())
	session, err := s.OpenSessionObserver(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	group, err := session.OpenGroup(ctx, "group", "profile")
	if err != nil {
		t.Fatal(err)
	}
	worker, err := group.OpenFFmpeg(ctx)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := worker.BeginAttempt(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	output, err := s.OpenOutputObserver(ctx, child)
	if err != nil {
		t.Fatal(err)
	}
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
	before, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if err != nil {
		t.Fatal(err)
	}
	views := u.views
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
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	for _, operation := range []struct {
		name string
		run  func() error
	}{
		{"session_observe", func() error { return session.Observe(cancelled, SessionObservation{State: ObservationFailed}) }},
		{"group_observe", func() error {
			return group.Observe(cancelled, GroupObservation{State: ObservationFailed, Source: ObservationSourceNone})
		}},
		{"output_observe", func() error { return output.Observe(cancelled, OutputObservation{State: OutputRetrying}) }},
		{"attempt_observe", func() error { return attempt.Observe(cancelled, FFmpegObservation{State: ObservationFailed}) }},
		{"attempt_close", func() error { return attempt.Close(cancelled) }},
		{"worker_close", func() error { return worker.Close(cancelled) }},
		{"begin_attempt", func() error { _, err := worker.BeginAttempt(cancelled, 2); return err }},
		{"open_group", func() error { _, err := session.OpenGroup(cancelled, "another", "profile"); return err }},
		{"open_worker", func() error { _, err := group.OpenFFmpeg(cancelled); return err }},
		{"open_session", func() error { _, err := s.OpenSessionObserver(cancelled, parent); return err }},
		{"open_output", func() error { _, err := s.OpenOutputObserver(cancelled, child); return err }},
		{"snapshot", func() error { _, err := s.RuntimeSnapshot(cancelled, observationSessionFence(parent)); return err }},
	} {
		t.Run(operation.name, func(t *testing.T) {
			if err := operation.run(); !errors.Is(err, context.Canceled) {
				t.Fatalf("отмена потеряна: %v", err)
			}
		})
	}
	after, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if err != nil || !reflect.DeepEqual(before, after) || !session.Valid() || !group.Valid() || !worker.Valid() || !attempt.Valid() || !output.Valid() {
		t.Fatalf("отказ или отмена изменили прежний снимок: %+v, %v", after, err)
	}
	if views != u.views || u.writes != 0 {
		t.Fatalf("отказавшее RAM-наблюдение обратилось к БД: views=%d/%d writes=%d", u.views, views, u.writes)
	}
}

func TestRejectedObserverRegistrationPreservesExistingOwner(t *testing.T) {
	ctx := context.Background()
	s, u, parent, child := newObservationAcceptanceService(t, acceptanceObservationLimits())
	owner, err := s.OpenOutputObserver(ctx, child)
	if err != nil {
		t.Fatal(err)
	}
	if err := owner.Observe(ctx, OutputObservation{State: OutputPublishing}); err != nil {
		t.Fatal(err)
	}
	before, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		run  func() error
		want error
	}{
		{"session_fence_for_output", func() error { _, err := s.OpenOutputObserver(ctx, parent); return err }, ErrInvalid},
		{"output_fence_for_session", func() error { _, err := s.OpenSessionObserver(ctx, child); return err }, ErrInvalid},
		{"invalid_fence", func() error {
			invalid := child
			invalid.Attempt = 0
			_, err := s.OpenOutputObserver(ctx, invalid)
			return err
		}, ErrInvalid},
		{"unfinished_receipt", func() error {
			receipt := u.repository.receipts[child.CommandID]
			receipt.Status = CommandIssued
			u.repository.receipts[child.CommandID] = receipt
			_, err := s.OpenOutputObserver(ctx, child)
			return err
		}, ErrStale},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if err := tc.run(); !errors.Is(err, tc.want) {
				t.Fatalf("неверный допуск регистрации: %v", err)
			}
		})
	}
	after, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if err != nil || !reflect.DeepEqual(before, after) || !owner.Valid() {
		t.Fatalf("отказ регистрации уничтожил текущего владельца: %+v, %v", after, err)
	}
}
