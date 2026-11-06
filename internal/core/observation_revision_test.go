package core

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func TestRepeatedObservationsAdvanceStampWithoutDuplicateEvents(t *testing.T) {
	for _, kind := range []string{"session", "group", "output", "output_terminal", "ffmpeg"} {
		t.Run(kind, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				ctx := context.Background()
				s, u, parent, output := telemetryTestService(t, telemetryTestLimits())
				session, err := s.OpenSessionObserver(ctx, parent)
				if err != nil {
					t.Fatal(err)
				}
				const failure = "синтетическая закрытая диагностика"
				handle := session.observerHandle
				observe := func() error { return session.Observe(ctx, SessionObservation{State: ObservationReady, Error: failure}) }
				switch kind {
				case "group", "ffmpeg":
					group, err := session.OpenGroup(ctx, "group", "profile")
					if err != nil {
						t.Fatal(err)
					}
					handle = group.observerHandle
					observe = func() error {
						return group.Observe(ctx, GroupObservation{State: ObservationReady, Source: ObservationSourceLive, Error: failure})
					}
					if kind == "ffmpeg" {
						worker, err := group.OpenFFmpegRole(ctx, FFmpegLiveTranscode)
						if err != nil {
							t.Fatal(err)
						}
						attempt, err := worker.BeginAttempt(ctx, 1)
						if err != nil {
							t.Fatal(err)
						}
						handle = attempt.observerHandle
						observe = func() error { return attempt.Observe(ctx, FFmpegObservation{State: ObservationReady, Error: failure}) }
					}
				case "output", "output_terminal":
					owner, err := s.OpenOutputObserver(ctx, output)
					if err != nil {
						t.Fatal(err)
					}
					handle = owner.observerHandle
					observe = func() error { return owner.Observe(ctx, OutputObservation{State: OutputPublishing, Error: failure}) }
					if kind == "output_terminal" {
						observe = func() error {
							return owner.ObserveTerminal(ctx, OutputObservation{State: OutputFailed, Error: failure})
						}
					}
				}
				views := u.views
				anchor, err := s.RuntimeAnchor(ctx)
				if err != nil {
					t.Fatal(err)
				}
				for i := uint64(1); i <= 3; i++ {
					time.Sleep(time.Second)
					if err := observe(); err != nil {
						t.Fatal(err)
					}
					snapshot, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
					if err != nil {
						t.Fatal(err)
					}
					stamp := snapshot.Session.Stamp
					switch kind {
					case "group":
						stamp = snapshot.Groups[0].Stamp
					case "output", "output_terminal":
						stamp = snapshot.Outputs[0].Stamp
					case "ffmpeg":
						stamp = snapshot.FFmpeg[0].Stamp
					}
					if stamp.Revision != i || !stamp.ObservedAt.Equal(time.Now()) || stamp.Token != handle.Token() {
						t.Fatalf("принятое наблюдение не обновило метку: %+v", stamp)
					}
				}
				page, err := s.RuntimeEvents(ctx, anchor.Cursor, 16)
				if err != nil || page.Gap || len(page.Events) != 1 {
					t.Fatalf("неизменное состояние повторило событие: %+v, %v", page, err)
				}
				event := page.Events[0]
				if event.Kind != RuntimeStateEvent || event.Owner != ownerKind(handle.node.kind) || event.Stamp.Revision != 1 || event.Stamp.Token != handle.Token() || event.Session != observationSessionFence(parent) || event.Reason != ReasonObservationFailed {
					t.Fatalf("событие потеряло точного владельца: %+v", event)
				}
				if kind == "ffmpeg" && (event.Stamp.AttemptID != 1 || event.Role != FFmpegLiveTranscode || event.GroupInstanceID != handle.node.parent) {
					t.Fatalf("событие потеряло попытку FFmpeg: %+v", event)
				}
				if (kind == "output" || kind == "output_terminal") && event.Execution != output {
					t.Fatalf("событие потеряло ExecutionFence: %+v", event)
				}
				encoded, err := json.Marshal(event)
				if err != nil || strings.Contains(string(encoded), failure) {
					t.Fatal("безопасное событие содержит текст диагностики", err)
				}
				handle.node.stamp.Revision = math.MaxUint64
				before, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
				if err != nil {
					t.Fatal(err)
				}
				if err = observe(); !errors.Is(err, ErrConflict) {
					t.Fatal("повторное наблюдение обошло исчерпание ревизии", err)
				}
				after, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
				if err != nil || !reflect.DeepEqual(before, after) || u.views != views || u.writes != 0 {
					t.Fatal("отказ переполнения изменил снимок или обратился к БД", err)
				}
			})
		})
	}
}

func TestGroupObservationSourceRevisionAndMetricsRemainIndependent(t *testing.T) {
	ctx := context.Background()
	s, _, parent, _ := telemetryTestService(t, telemetryTestLimits())
	session, err := s.OpenSessionObserver(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	group, err := session.OpenGroup(ctx, "group", "profile")
	if err != nil {
		t.Fatal(err)
	}
	media := testMedia(time.Now().UTC())
	metrics := GroupMetrics{LiveInput: media, FallbackInput: media, Output: media}
	if err = group.ObserveMetrics(ctx, metrics); err != nil {
		t.Fatal(err)
	}
	anchor, err := s.RuntimeAnchor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	value := GroupObservation{State: ObservationReady, Source: ObservationSourceLive}
	for revision := uint64(1); revision <= 3; revision++ {
		value.SourceRevision = revision
		if err = group.Observe(ctx, value); err != nil {
			t.Fatal(err)
		}
	}
	before, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if err != nil || before.Groups[0].Stamp.Revision != 3 || *before.Groups[0].Value != value || *before.Groups[0].Metrics != metrics || before.Groups[0].MetricsStamp.Revision != 1 {
		t.Fatal("новая ревизия источника потеряна или изменила метрики", before, err)
	}
	for _, revision := range []uint64{0, 1, 2, 3} {
		stale := value
		stale.SourceRevision = revision
		if err = group.Observe(ctx, stale); !errors.Is(err, ErrStale) {
			t.Fatal("старая ревизия источника принята", revision, err)
		}
	}
	after, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("позднее наблюдение изменило снимок или курсор", err)
	}
	page, err := s.RuntimeEvents(ctx, anchor.Cursor, 16)
	if err != nil || len(page.Events) != 1 || page.Events[0].SourceRevision != 1 || page.Events[0].Stamp.Revision != 1 {
		t.Fatal("ревизия источника повторила событие", page, err)
	}
	value.SourceRevision, value.Source, value.State = 4, ObservationSourceFallback, ObservationRetrying
	if err = group.Observe(ctx, value); err != nil {
		t.Fatal(err)
	}
	next, err := s.RuntimeEvents(ctx, page.Cursor, 16)
	if err != nil || len(next.Events) != 1 || next.Events[0].SourceRevision != 4 || next.Events[0].Stamp.Revision != 4 || next.Events[0].Source != ObservationSourceFallback {
		t.Fatal("переход после повторов потерял актуальную ревизию", next, err)
	}
}

func TestRejectedGroupObservationPreservesSourceRevision(t *testing.T) {
	for _, rejection := range []string{"invalid", "budget", "overflow"} {
		t.Run(rejection, func(t *testing.T) {
			ctx := context.Background()
			s, _, parent, _ := telemetryTestService(t, telemetryTestLimits())
			session, err := s.OpenSessionObserver(ctx, parent)
			if err != nil {
				t.Fatal(err)
			}
			group, err := session.OpenGroup(ctx, "group", "profile")
			if err != nil {
				t.Fatal(err)
			}
			value := GroupObservation{State: ObservationReady, Source: ObservationSourceLive, SourceRevision: 1}
			if err = group.Observe(ctx, value); err != nil {
				t.Fatal(err)
			}
			rejected := value
			rejected.SourceRevision = 2
			want := ErrInvalid
			switch rejection {
			case "invalid":
				rejected.Error = "недопустимый\x00текст"
			case "budget":
				rejected.Error = strings.Repeat("x", 257)
				want = ErrObservationLimit
			case "overflow":
				group.node.stamp.Revision = math.MaxUint64
				want = ErrConflict
			}
			before, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
			if err != nil {
				t.Fatal(err)
			}
			if err = group.Observe(ctx, rejected); !errors.Is(err, want) {
				t.Fatal("неверный результат отклонённого наблюдения", err)
			}
			after, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
			if err != nil || !reflect.DeepEqual(before, after) {
				t.Fatal("отказ изменил значение, ревизию источника или курсор", err)
			}
			if rejection == "overflow" {
				group.node.stamp.Revision = 1
			}
			value.SourceRevision = 2
			if err = group.Observe(ctx, value); err != nil {
				t.Fatal("отказ преждевременно продвинул внутреннюю ревизию источника", err)
			}
		})
	}
}
