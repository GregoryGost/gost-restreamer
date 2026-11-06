package core

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

func acceptanceTelemetryLimits(events int) TelemetryLimits {
	return TelemetryLimits{MediaStaleAfter: 10 * time.Second, Events: EventLimits{MaxEvents: events, MaxPageSize: events, MaxBytes: 1 << 20, MaxEventBytes: 16384}, MaxGPUDevices: 2, MaxGPUUUIDBytes: 64, MaxGPUBytes: 4096}
}

func TestAcceptanceTelemetryKeepsStateMetricsAndCursorConsistent(t *testing.T) {
	ctx := context.Background()
	s, u, parent, _ := newObservationAcceptanceService(t, acceptanceObservationLimits())
	if err := s.ConfigureTelemetry(ctx, acceptanceTelemetryLimits(16)); err != nil {
		t.Fatal(err)
	}
	so, err := s.OpenSessionObserver(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	g, err := so.OpenGroup(ctx, "profile-group", "profile")
	if err != nil {
		t.Fatal(err)
	}
	state := GroupObservation{State: ObservationReady, Source: ObservationSourceLive, SourceRevision: 2, Error: "synthetic-private-diagnostic"}
	if err = g.Observe(ctx, state); err != nil {
		t.Fatal(err)
	}
	anchor, err := s.RuntimeAnchor(ctx)
	if err != nil || len(anchor.Sessions) != 1 || anchor.Sessions[0].Groups[0].Value.Error != state.Error {
		t.Fatal("RuntimeAnchor не сохранил текущее наблюдение", anchor, err)
	}
	views := u.views
	u.viewErr = errors.New("хранилище недоступно")
	at := time.Now().UTC()
	m := GroupMetrics{LiveInput: MediaMetrics{SampledAt: at, VideoFrames: 11, PayloadBytes: 101}, FallbackInput: MediaMetrics{SampledAt: at, VideoFrames: 22, PayloadBytes: 202}, Output: MediaMetrics{SampledAt: at, VideoFrames: 33, PayloadBytes: 303}}
	if err = g.ObserveMetrics(ctx, m); err != nil {
		t.Fatal(err)
	}
	select {
	case <-anchor.Changed:
	default:
		t.Fatal("RuntimeAnchor не разбудил читателя при новом событии")
	}
	page, err := s.RuntimeEvents(ctx, anchor.Cursor, 16)
	if err != nil || page.Gap || page.Reset || len(page.Events) != 1 || page.Events[0].Kind != RuntimeMetricsEvent || *page.Events[0].Group != m {
		t.Fatal("событие после RuntimeAnchor потеряно или повторено", page, err)
	}
	snapshot, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if err != nil || !snapshot.CursorValid || snapshot.Cursor != page.Cursor || *snapshot.Groups[0].Value != state || *snapshot.Groups[0].Metrics != m {
		t.Fatal("метрики стёрли состояние либо снимок оторван от курсора", snapshot, err)
	}
	snapshot.Groups[0].Metrics.Output.PayloadBytes = 0
	page.Events[0].Group.LiveInput.PayloadBytes = 0
	if err = g.Observe(ctx, GroupObservation{State: ObservationRetrying, Source: ObservationSourceFallback, SourceRevision: 1}); !errors.Is(err, ErrStale) {
		t.Fatal("позднее состояние принято", err)
	}
	state.SourceRevision = 3
	if err = g.Observe(ctx, state); err != nil {
		t.Fatal(err)
	}
	unchanged, err := s.RuntimeEvents(ctx, page.Cursor, 16)
	if err != nil || len(unchanged.Events) != 0 || unchanged.Cursor != page.Cursor {
		t.Fatal("неизменное состояние породило повторное событие", unchanged, err)
	}
	if err = g.ObserveMetrics(ctx, m); !errors.Is(err, ErrStale) {
		t.Fatal("повторное измерение принято", err)
	}
	state.SourceRevision, state.Source, state.State, state.Error = 4, ObservationSourceFallback, ObservationRetrying, ""
	if err = g.Observe(ctx, state); err != nil {
		t.Fatal(err)
	}
	snapshot, err = s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if err != nil || *snapshot.Groups[0].Metrics != m || *snapshot.Groups[0].Value != state {
		t.Fatal("состояние изменило метрики или внешняя ссылка изменила источник", snapshot, err)
	}
	all, err := s.RuntimeEvents(ctx, RuntimeCursor{}, 16)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(all.Events)
	if err != nil || strings.Contains(string(encoded), "synthetic-private-diagnostic") {
		t.Fatal("событие RAM экспортирует диагностическую строку", err)
	}
	if u.views != views || u.writes != 0 {
		t.Fatal("наблюдение обратилось к хранилищу", u.views, views, u.writes)
	}
}

func TestAcceptanceTelemetryEvictionPurgeAndSystemIsolation(t *testing.T) {
	ctx := context.Background()
	s, _, parent, _ := newObservationAcceptanceService(t, acceptanceObservationLimits())
	if err := s.ConfigureTelemetry(ctx, acceptanceTelemetryLimits(2)); err != nil {
		t.Fatal(err)
	}
	so, err := s.OpenSessionObserver(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	initial, err := s.RuntimeAnchor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []ObservationState{ObservationStarting, ObservationReady, ObservationRetrying} {
		if err = so.Observe(ctx, SessionObservation{State: state}); err != nil {
			t.Fatal(err)
		}
	}
	page, err := s.RuntimeEvents(ctx, initial.Cursor, 2)
	if err != nil || !page.Gap || page.Reset || len(page.Events) != 2 || page.Events[0].State != ObservationReady || page.Events[1].State != ObservationRetrying {
		t.Fatal("вытеснение не выявило пропуск целых событий", page, err)
	}
	foreign := page.Cursor
	foreign.Epoch = "another-process"
	reset, err := s.RuntimeEvents(ctx, foreign, 2)
	if err != nil || !reset.Gap || !reset.Reset {
		t.Fatal("чужой курсор не требует сброса", reset, err)
	}
	if _, err = s.RuntimeEvents(ctx, RuntimeCursor{}, 3); !errors.Is(err, ErrInvalid) {
		t.Fatal("лимит страницы не соблюдён", err)
	}
	system, err := s.OpenSystemObserver(ctx)
	if err != nil {
		t.Fatal(err)
	}
	at := time.Now()
	if err = system.ObserveGPU(ctx, GPUSnapshot{StaleAfter: 10 * time.Second, SampledAt: at, Status: MetricUnsupported, Reason: ReasonUnsupportedPlatform}); err != nil {
		t.Fatal(err)
	}
	beforeClose, err := s.RuntimeAnchor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = so.Close(ctx); err != nil {
		t.Fatal(err)
	}
	afterClose, err := s.RuntimeAnchor(ctx)
	if err != nil || len(afterClose.Sessions) != 0 || afterClose.GPU == nil || afterClose.GPU.Status != MetricUnsupported || !system.Valid() {
		t.Fatal("удаление Session повредило системное наблюдение", afterClose, err)
	}
	purged, err := s.RuntimeEvents(ctx, RuntimeCursor{}, 2)
	if err != nil || !purged.Gap {
		t.Fatal("очистка скрыла пропуск", purged, err)
	}
	for _, event := range purged.Events {
		if event.Session == observationSessionFence(parent) && (event.Kind != RuntimeRemovedEvent || event.Input != nil || event.Group != nil || event.Output != nil || event.FFmpeg != nil) {
			t.Fatal("после удаления осталось содержимое Session", event)
		}
	}
	if afterClose.Cursor.Sequence <= beforeClose.Cursor.Sequence {
		t.Fatal("очистка переиспользовала номер события")
	}
	if err = system.Close(ctx); err != nil {
		t.Fatal(err)
	}
	replacement, err := s.OpenSystemObserver(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = system.ObserveGPU(ctx, GPUSnapshot{StaleAfter: 10 * time.Second, SampledAt: at.Add(time.Second), Status: MetricAvailable}); !errors.Is(err, ErrStale) {
		t.Fatal("старый системный токен изменил замену", err)
	}
	if err = system.Close(ctx); err != nil || !replacement.Valid() {
		t.Fatal("старый Close удалил замену", err)
	}
}

func TestAcceptanceTelemetryAttemptResetAndBudgetRejectionAreAtomic(t *testing.T) {
	ctx := context.Background()
	s, u, parent, _ := newObservationAcceptanceService(t, acceptanceObservationLimits())
	if err := s.ConfigureTelemetry(ctx, acceptanceTelemetryLimits(16)); err != nil {
		t.Fatal(err)
	}
	so, err := s.OpenSessionObserver(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	g, err := so.OpenGroup(ctx, "group", "profile")
	if err != nil {
		t.Fatal(err)
	}
	w, err := g.OpenFFmpegRole(ctx, FFmpegLiveTranscode)
	if err != nil {
		t.Fatal(err)
	}
	first, err := w.BeginAttempt(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	m := FFmpegMetrics{SampledAt: time.Now(), HasProgress: true, Progress: FFmpegProgress{Frame: OptionalUint64{Present: true, Value: 50}}}
	if err = first.ObserveMetrics(ctx, m); err != nil {
		t.Fatal(err)
	}
	second, err := w.BeginAttempt(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err = first.ObserveMetrics(ctx, FFmpegMetrics{SampledAt: m.SampledAt.Add(time.Second)}); !errors.Is(err, ErrStale) {
		t.Fatal("старая попытка опубликовала прогресс", err)
	}
	snapshot, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if err != nil || snapshot.FFmpeg[0].Metrics != nil || snapshot.FFmpeg[0].Stamp.AttemptID != 2 || snapshot.FFmpeg[0].Role != FFmpegLiveTranscode {
		t.Fatal("новая попытка унаследовала прогресс", snapshot, err)
	}
	if err = second.ObserveMetrics(ctx, m); err != nil {
		t.Fatal("новая попытка унаследовала исходную метку времени", err)
	}
	before, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if err != nil {
		t.Fatal(err)
	}
	if err = g.Observe(ctx, GroupObservation{State: ObservationFailed, Source: ObservationSourceNone, Error: strings.Repeat("x", 257)}); !errors.Is(err, ErrObservationLimit) {
		t.Fatal("переполнение строки принято", err)
	}
	after, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if err != nil || !reflect.DeepEqual(before, after) {
		t.Fatal("отказ бюджета частично изменил снимок", err)
	}
	u.authority.events.sequence = math.MaxUint64
	if err = second.Observe(ctx, FFmpegObservation{State: ObservationReady}); err != nil {
		t.Fatal("исчерпание последовательности отменило текущее значение", err)
	}
	if _, err = s.RuntimeAnchor(ctx); !errors.Is(err, ErrEventSequenceExhausted) {
		t.Fatal("исчерпание последовательности выдало успешный RuntimeAnchor", err)
	}
	if _, err = s.RuntimeEvents(ctx, before.Cursor, 16); !errors.Is(err, ErrEventSequenceExhausted) {
		t.Fatal("исчерпание последовательности выдало успешный курсор", err)
	}
	if err = so.Close(ctx); err != nil {
		t.Fatal("исчерпание последовательности помешало освобождению", err)
	}
}

func TestAcceptanceTerminalOutputObservationPreservesPhysicalAndControlOwnership(t *testing.T) {
	for _, terminal := range []OutputState{OutputFailed, OutputStopped} {
		t.Run(string(terminal), func(t *testing.T) {
			ctx := context.Background()
			s, u, _, fence := newObservationAcceptanceService(t, acceptanceObservationLimits())
			if err := s.ConfigureTelemetry(ctx, acceptanceTelemetryLimits(8)); err != nil {
				t.Fatal(err)
			}
			lease, err := s.BindExecutionOwner(ctx, fence)
			if err != nil {
				t.Fatal(err)
			}
			reservation, err := s.ReserveOutputExecution(ctx, lease, u.repository.outputs[fence.OutputID].Snapshot)
			if err != nil {
				t.Fatal(err)
			}
			observer, err := s.OpenOutputObserver(ctx, fence)
			if err != nil {
				t.Fatal(err)
			}
			if err = reservation.Activate(ctx, observer); err != nil {
				t.Fatal(err)
			}
			blockedWrite, err := reservation.Admit(ctx, OutputOperationWrite)
			if err != nil {
				t.Fatal(err)
			}
			defer blockedWrite.Finish()
			anchor, err := s.RuntimeAnchor(ctx)
			if err != nil {
				t.Fatal(err)
			}
			beforeOutput := u.repository.outputs[fence.OutputID]
			beforeReceipt := u.repository.receipts[fence.CommandID]
			views := u.views
			u.viewErr = errors.New("хранилище недоступно")
			value := OutputObservation{State: terminal, Error: "synthetic-terminal-diagnostic"}
			if err = observer.Observe(ctx, value); !errors.Is(err, ErrInvalid) {
				t.Fatal("обычный Observe изменил контракт терминальных состояний", err)
			}
			if err = observer.ObserveTerminal(ctx, OutputObservation{State: OutputPublishing}); !errors.Is(err, ErrInvalid) {
				t.Fatal("ObserveTerminal принял нетерминальное состояние", err)
			}
			if err = observer.ObserveTerminal(ctx, value); err != nil {
				t.Fatal(err)
			}
			snapshot, err := s.RuntimeSnapshot(ctx, observationSessionFence(fence))
			if err != nil || len(snapshot.Outputs) != 1 || *snapshot.Outputs[0].Value != value || snapshot.Outputs[0].Execution != fence {
				t.Fatal("ошибка не опубликована немедленно точным владельцем", snapshot, err)
			}
			page, err := s.RuntimeEvents(ctx, anchor.Cursor, 8)
			if err != nil || len(page.Events) != 1 || page.Events[0].OutputState != terminal || page.Events[0].Reason != ReasonObservationFailed || page.Events[0].Execution != fence {
				t.Fatal("безопасное терминальное событие потеряно", page, err)
			}
			encoded, err := json.Marshal(page.Events)
			if err != nil || strings.Contains(string(encoded), value.Error) {
				t.Fatal("событие содержит текст диагностики", err)
			}
			select {
			case <-reservation.Done():
				t.Fatal("наблюдение запечатало физическое исполнение")
			default:
			}
			read, err := reservation.Admit(ctx, OutputOperationRead)
			if err != nil {
				t.Fatal("наблюдение отозвало допуск исполнения", err)
			}
			read.Finish()
			if err = reservation.ReleaseJoined(); !errors.Is(err, ErrNotReady) {
				t.Fatal("наблюдение выдумало физическое завершение", err)
			}
			if u.views != views || u.writes != 0 || !reflect.DeepEqual(beforeOutput, u.repository.outputs[fence.OutputID]) || !reflect.DeepEqual(beforeReceipt, u.repository.receipts[fence.CommandID]) {
				t.Fatal("терминальное наблюдение изменило долговечное управление")
			}
			u.viewErr = nil
			replacement, err := s.OpenOutputObserver(ctx, fence)
			if err != nil {
				t.Fatal(err)
			}
			if err = observer.ObserveTerminal(ctx, value); !errors.Is(err, ErrStale) || !replacement.Valid() {
				t.Fatal("старый токен изменил замену", err)
			}
		})
	}
}
