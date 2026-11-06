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

func telemetryTestLimits() TelemetryLimits {
	return TelemetryLimits{MediaStaleAfter: 10 * time.Second, Events: EventLimits{MaxEvents: 16, MaxBytes: 65536, MaxEventBytes: 8192, MaxPageSize: 16}, MaxGPUDevices: 2, MaxGPUUUIDBytes: 64, MaxGPUBytes: 4096}
}
func telemetryTestService(t *testing.T, limits TelemetryLimits) (*Service, *observationAcceptanceUOW, ExecutionFence, ExecutionFence) {
	t.Helper()
	s, u, parent, output := newObservationAcceptanceService(t, acceptanceObservationLimits())
	if err := s.ConfigureTelemetry(context.Background(), limits); err != nil {
		t.Fatal(err)
	}
	return s, u, parent, output
}
func testMedia(at time.Time) MediaMetrics {
	return MediaMetrics{SampledAt: at, VideoFrames: 30, PayloadBytes: 1000, Rates: MediaRates{FPS: Rate{Status: MetricAvailable, Value: 30, WindowStart: at.Add(-time.Second), WindowEnd: at, Duration: time.Second}, PayloadBitrate: Rate{Status: MetricWarmingUp}, WrittenBitrate: Rate{Status: MetricWarmingUp}}}
}
func TestTelemetryStateAndMetricsStayIndependentAndFenced(t *testing.T) {
	ctx := context.Background()
	s, _, parent, output := telemetryTestService(t, telemetryTestLimits())
	session, err := s.OpenSessionObserver(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	group, err := session.OpenGroup(ctx, "group", "profile")
	if err != nil {
		t.Fatal(err)
	}
	worker, err := group.OpenFFmpegRole(ctx, FFmpegLiveTranscode)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := worker.BeginAttempt(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	out, err := s.OpenOutputObserver(ctx, output)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	media := testMedia(now)
	gm := GroupMetrics{LiveInput: media, FallbackInput: media, Output: media}
	if err = group.Observe(ctx, GroupObservation{State: ObservationReady, Source: ObservationSourceLive, SourceRevision: 2, Error: "synthetic-private-error"}); err != nil {
		t.Fatal(err)
	}
	if err = group.ObserveMetrics(ctx, gm); err != nil {
		t.Fatal(err)
	}
	if err = out.ObserveMetrics(ctx, OutputMetrics{MediaMetrics: media, QueuePackets: 1, QueueCapacity: 2, QueueBytes: 8, QueueByteCapacity: 16, QueueDrops: QueueDropMetrics{Overflow: 3}, DeliveryDrops: DeliveryDropMetrics{Transport: 4}}); err != nil {
		t.Fatal(err)
	}
	if err = out.Observe(ctx, OutputObservation{State: OutputPublishing}); err != nil {
		t.Fatal(err)
	}
	if err = attempt.Observe(ctx, FFmpegObservation{State: ObservationReady}); err != nil {
		t.Fatal(err)
	}
	fm := FFmpegMetrics{SampledAt: now, HasProgress: true, LastProgressAt: now, Status: MetricAvailable, Progress: FFmpegProgress{Frame: OptionalUint64{Value: math.MaxUint64, Present: true}, OutTimeUS: OptionalInt64{Value: -1234, Present: true}, Speed: OptionalFloat64{Value: 1.25, Present: true}}}
	if err = attempt.ObserveMetrics(ctx, fm); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.Groups[0].Value.Error != "synthetic-private-error" || snapshot.Groups[0].Metrics.Output.PayloadBytes != 1000 || snapshot.Outputs[0].Value.State != OutputPublishing || snapshot.Outputs[0].Metrics.DeliveryDrops.Transport != 4 || snapshot.FFmpeg[0].Role != FFmpegLiveTranscode || snapshot.FFmpeg[0].Metrics.Progress.OutTimeUS.Value != -1234 {
		t.Fatalf("состояние затёрто: %+v", snapshot)
	}
	if err = group.Observe(ctx, GroupObservation{State: ObservationFailed, Source: ObservationSourceNone, SourceRevision: 1}); !errors.Is(err, ErrStale) {
		t.Fatal(err)
	}
	if err = group.ObserveMetrics(ctx, gm); !errors.Is(err, ErrStale) {
		t.Fatal(err)
	}
	snapshot.Groups[0].Metrics.Output.PayloadBytes = 0
	snapshot.FFmpeg[0].Metrics.Progress.OutTimeUS.Value = 0
	again, _ := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if again.Groups[0].Metrics.Output.PayloadBytes != 1000 || again.FFmpeg[0].Metrics.Progress.OutTimeUS.Value != -1234 {
		t.Fatal("изменяемый alias")
	}
	page, err := s.RuntimeEvents(ctx, RuntimeCursor{}, 16)
	if err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(page.Events)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "synthetic-private-error") {
		t.Fatal("сырой текст ошибки оказался в событии")
	}
	before := page.HighWater
	if err = group.Observe(ctx, GroupObservation{State: ObservationReady, Source: ObservationSourceLive, SourceRevision: 3, Error: "synthetic-private-error"}); err != nil {
		t.Fatal(err)
	}
	page, _ = s.RuntimeEvents(ctx, before, 16)
	if len(page.Events) != 0 {
		t.Fatal("неизменное состояние создало событие")
	}
	second, err := worker.BeginAttempt(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err = attempt.ObserveMetrics(ctx, fm); !errors.Is(err, ErrStale) {
		t.Fatal(err)
	}
	again, _ = s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if again.FFmpeg[0].Metrics != nil {
		t.Fatal("метрики прежней попытки сохранились")
	}
	fm.SampledAt = now.Add(time.Second)
	if err = second.ObserveMetrics(ctx, fm); err != nil {
		t.Fatal(err)
	}
	if err = second.Close(ctx); err != nil {
		t.Fatal(err)
	}
	again, _ = s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if again.FFmpeg[0].Metrics != nil {
		t.Fatal("закрытая попытка сохранила метрики")
	}
}
func TestTelemetryBudgetsAreReservedBeforeRegistration(t *testing.T) {
	ctx := context.Background()
	limits := acceptanceObservationLimits()
	limits.MaxBytes = 2048
	s, _, parent, _ := newObservationAcceptanceService(t, limits)
	if err := s.ConfigureTelemetry(ctx, telemetryTestLimits()); err != nil {
		t.Fatal(err)
	}
	session, err := s.OpenSessionObserver(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	before, _ := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if _, err = session.OpenGroup(ctx, "group", "profile"); !errors.Is(err, ErrObservationLimit) {
		t.Fatal(err)
	}
	after, _ := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if !reflect.DeepEqual(before, after) {
		t.Fatal("отказ бюджета изменил владельца")
	}
	if err = session.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RuntimeSnapshot(ctx, observationSessionFence(parent)); !errors.Is(err, ErrNotReady) {
		t.Fatal(err)
	}
}
func TestEventsDetectEvictionPurgeResetAndAnchor(t *testing.T) {
	ctx := context.Background()
	l := telemetryTestLimits()
	l.Events.MaxEvents = 2
	l.Events.MaxPageSize = 2
	s, u, parent, _ := telemetryTestService(t, l)
	session, _ := s.OpenSessionObserver(ctx, parent)
	anchor, err := s.RuntimeAnchor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	for _, state := range []ObservationState{ObservationStarting, ObservationReady, ObservationFailed} {
		if err = session.Observe(ctx, SessionObservation{State: state}); err != nil {
			t.Fatal(err)
		}
	}
	select {
	case <-anchor.Changed:
	default:
		t.Fatal("сигнал изменений потерян")
	}
	page, err := s.RuntimeEvents(ctx, anchor.Cursor, 1)
	if err != nil || !page.Gap || len(page.Events) != 1 || page.Events[0].State != ObservationReady {
		t.Fatalf("вытеснение: %+v %v", page, err)
	}
	page, err = s.RuntimeEvents(ctx, page.Cursor, 2)
	if err != nil || len(page.Events) != 1 || page.Events[0].State != ObservationFailed {
		t.Fatalf("следующая страница: %+v %v", page, err)
	}
	future := page.HighWater
	future.Sequence++
	if _, err = s.RuntimeEvents(ctx, future, 1); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	foreign := page.Cursor
	foreign.Epoch = "foreign"
	reset, err := s.RuntimeEvents(ctx, foreign, 2)
	if err != nil || !reset.Reset || !reset.Gap {
		t.Fatalf("сброс: %+v %v", reset, err)
	}
	if err = session.Close(ctx); err != nil {
		t.Fatal(err)
	}
	page, err = s.RuntimeEvents(ctx, anchor.Cursor, 2)
	if err != nil || !page.Gap || len(page.Events) != 1 || page.Events[0].Kind != RuntimeRemovedEvent || page.Events[0].Input != nil {
		t.Fatalf("удаление данных: %+v %v", page, err)
	}
	empty, err := s.RuntimeAnchor(ctx)
	if err != nil || len(empty.Sessions) != 0 {
		t.Fatalf("удалённая Session: %+v %v", empty, err)
	}
	if u.writes != 0 {
		t.Fatal("события записали БД")
	}
	for _, limit := range []int{0, 3} {
		if _, err = s.RuntimeEvents(ctx, RuntimeCursor{}, limit); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	}
}
func TestEventOversizeAndExhaustionKeepCurrentValues(t *testing.T) {
	ctx := context.Background()
	l := telemetryTestLimits()
	l.Events.MaxEventBytes = 256
	s, u, parent, _ := telemetryTestService(t, l)
	session, _ := s.OpenSessionObserver(ctx, parent)
	if err := session.Observe(ctx, SessionObservation{State: ObservationReady}); err != nil {
		t.Fatal(err)
	}
	page, err := s.RuntimeEvents(ctx, RuntimeCursor{}, 1)
	if err != nil || !page.Gap || len(page.Events) != 0 || page.Cursor.Sequence != 1 {
		t.Fatalf("превышение размера: %+v %v", page, err)
	}
	u.authority.events.sequence = math.MaxUint64
	if err = session.Observe(ctx, SessionObservation{State: ObservationFailed}); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RuntimeEvents(ctx, RuntimeCursor{}, 1); !errors.Is(err, ErrEventSequenceExhausted) {
		t.Fatal(err)
	}
	if _, err = s.RuntimeAnchor(ctx); !errors.Is(err, ErrEventSequenceExhausted) {
		t.Fatal(err)
	}
	snapshot, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if err != nil || snapshot.Session.Value.State != ObservationFailed || snapshot.CursorValid {
		t.Fatalf("ложная позиция либо откат: %+v %v", snapshot, err)
	}
}
func TestTelemetryConfigurationAndOwnerClose(t *testing.T) {
	ctx := context.Background()
	s, u, parent, _ := telemetryTestService(t, telemetryTestLimits())
	if err := s.ConfigureTelemetry(ctx, telemetryTestLimits()); err != nil {
		t.Fatal(err)
	}
	different := telemetryTestLimits()
	different.Events.MaxEvents++
	if err := s.ConfigureTelemetry(ctx, different); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	if err := (TelemetryLimits{}).Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	bad := telemetryTestLimits()
	bad.MediaStaleAfter = 0
	if err := bad.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	bad = telemetryTestLimits()
	bad.MaxGPUUUIDBytes = math.MaxUint64
	if err := bad.Validate(); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	session, err := s.OpenSessionObserver(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	if session.Token() == 0 {
		t.Fatal("нет token")
	}
	system, err := s.OpenSystemObserver(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = s.OpenSystemObserver(ctx); !errors.Is(err, ErrConflict) {
		t.Fatal(err)
	}
	u.authority.Close()
	if system.Valid() {
		t.Fatal("системный допуск сохранился")
	}
	select {
	case <-system.Done():
	default:
		t.Fatal("нет подтверждения завершения-сигнала")
	}
	u.authority.Close()
}
func TestSnapshotFreshnessAdvancesWithoutNewSamplerWrites(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		s, _, parent, _ := telemetryTestService(t, telemetryTestLimits())
		session, _ := s.OpenSessionObserver(ctx, parent)
		group, _ := session.OpenGroup(ctx, "group", "profile")
		worker, _ := group.OpenFFmpegRole(ctx, FFmpegFallbackPlayer)
		attempt, _ := worker.BeginAttempt(ctx, 1)
		now := time.Now()
		m := testMedia(now)
		if err := group.ObserveMetrics(ctx, GroupMetrics{LiveInput: m, FallbackInput: m, Output: m}); err != nil {
			t.Fatal(err)
		}
		if err := attempt.ObserveMetrics(ctx, FFmpegMetrics{SampledAt: now, HasProgress: true, LastProgressAt: now, Status: MetricAvailable}); err != nil {
			t.Fatal(err)
		}
		time.Sleep(11 * time.Second)
		snapshot, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.Groups[0].Metrics.Output.Rates.FPS.Status != MetricStale || snapshot.Groups[0].Metrics.Output.Rates.FPS.Value != 0 || snapshot.FFmpeg[0].Metrics.Status != MetricStale {
			t.Fatalf("устаревшие значения объявлены свежими: %+v", snapshot)
		}
	})
}

type telemetryInputRepository struct {
	*observationAcceptanceRepository
}

func (r telemetryInputRepository) ActiveSession(context.Context, ID) (Session, error) {
	return r.session, nil
}

type telemetryInputUOW struct{ *observationAcceptanceUOW }

func (u *telemetryInputUOW) View(ctx context.Context, fn func(Repositories) error) error {
	u.views++
	return fn(Repositories{Runtime: telemetryInputRepository{&u.repository}, Artifacts: emptyPreparationRepository{}})
}
func TestInputUsesExactCurrentPublicationAndObserverToken(t *testing.T) {
	ctx := context.Background()
	_, u, parent, _ := telemetryTestService(t, telemetryTestLimits())
	u.repository.session.StreamID = "stream"
	u.repository.session.Snapshot.Stream.PublicationKey = "test-key"
	service, err := NewService(&telemetryInputUOW{u}, "owner")
	if err != nil {
		t.Fatal(err)
	}
	observer, err := service.OpenSessionObserver(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	install := func(*RuntimeLease) (func(), error) { return nil, nil }
	first, err := service.ActivateOBS(ctx, "stream", "test-key", install)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, _ := service.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if snapshot.Input == nil || !snapshot.Input.Connected || snapshot.Input.LeaseID != first.ID() {
		t.Fatalf("нет connected: %+v", snapshot)
	}
	now := time.Now()
	metrics := InputMetrics{MediaMetrics: testMedia(now), SourceEpoch: 1}
	if err = observer.ObserveInput(ctx, first.ID(), metrics); err != nil {
		t.Fatal(err)
	}
	before, _ := service.RuntimeSnapshot(ctx, observationSessionFence(parent))
	failed := errors.New("установка отклонена")
	if _, err = service.ActivateOBS(ctx, "stream", "test-key", func(*RuntimeLease) (func(), error) { return nil, failed }); !errors.Is(err, failed) {
		t.Fatal(err)
	}
	after, _ := service.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if !reflect.DeepEqual(before, after) {
		t.Fatal("ошибка install изменила прежний input")
	}
	second, err := service.ActivateOBS(ctx, "stream", "test-key", install)
	if err != nil {
		t.Fatal(err)
	}
	after, _ = service.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if !after.Input.Connected || after.Input.LeaseID != second.ID() || after.Input.Metrics != nil || first.Valid() {
		t.Fatalf("Override не сбросил input: %+v", after.Input)
	}
	if err = observer.ObserveInput(ctx, first.ID(), metrics); !errors.Is(err, ErrStale) {
		t.Fatal(err)
	}
	if err = first.Revoke(ctx); err != nil {
		t.Fatal(err)
	}
	after, _ = service.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if !after.Input.Connected || after.Input.LeaseID != second.ID() {
		t.Fatal("старый revoke изменил новый input")
	}
	metrics.SampledAt = now.Add(time.Second)
	if err = observer.ObserveInput(ctx, second.ID(), metrics); err != nil {
		t.Fatal(err)
	}
	replacement, err := service.OpenSessionObserver(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	after, _ = service.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if observer.Valid() || after.Input.Metrics != nil || after.Input.LeaseID != second.ID() || !after.Input.Connected {
		t.Fatalf("новый token унаследовал измерение: %+v", after.Input)
	}
	if err = observer.ObserveInput(ctx, second.ID(), metrics); !errors.Is(err, ErrStale) {
		t.Fatal(err)
	}
	if err = replacement.ObserveInput(ctx, second.ID(), metrics); err != nil {
		t.Fatal(err)
	}
	if err = second.Revoke(ctx); err != nil {
		t.Fatal(err)
	}
	after, _ = service.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if after.Input.Connected || after.Input.Metrics != nil {
		t.Fatal("disconnect выдал прежние скорости")
	}
	if err = replacement.ObserveInput(ctx, second.ID(), metrics); !errors.Is(err, ErrStale) {
		t.Fatal(err)
	}
}
func testGPU(at time.Time) GPUSnapshot {
	field := GPUMetric{Status: MetricAvailable, Value: 37, SampledAt: at, LastSuccessAt: at, SamplingPeriodUS: 1000}
	device := GPUDevice{UUID: "GPU-test", Utilization: field, MemoryUtilization: field, EncoderUtilization: field, DecoderUtilization: field, MemoryUsedBytes: field, MemoryTotalBytes: field, MemoryFreeBytes: field}
	device.MemoryTotalBytes.Value = math.MaxUint64
	return GPUSnapshot{SampledAt: at, StaleAfter: time.Second, Status: MetricAvailable, Devices: []GPUDevice{device}}
}
func TestGPUOwnershipBoundsCopiesPurgeAndFreshness(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		ctx := context.Background()
		s, _, parent, _ := telemetryTestService(t, telemetryTestLimits())
		session, _ := s.OpenSessionObserver(ctx, parent)
		owner, err := s.OpenSystemObserver(ctx)
		if err != nil {
			t.Fatal(err)
		}
		now := time.Now()
		value := testGPU(now)
		if err = owner.ObserveGPU(ctx, value); err != nil {
			t.Fatal(err)
		}
		value.Devices[0].UUID = "changed"
		value.Devices[0].Utilization.Value = 99
		anchor, err := s.RuntimeAnchor(ctx)
		if err != nil || anchor.GPU == nil || anchor.GPU.Devices[0].UUID != "GPU-test" || anchor.GPU.Devices[0].Utilization.Value != 37 || anchor.GPU.Devices[0].MemoryTotalBytes.Value != math.MaxUint64 {
			t.Fatalf("копирование GPU/uint64: %+v %v", anchor.GPU, err)
		}
		invalid := testGPU(now.Add(time.Nanosecond))
		invalid.Devices[0].UUID = strings.Repeat("x", 65)
		if err = owner.ObserveGPU(ctx, invalid); !errors.Is(err, ErrObservationLimit) {
			t.Fatal(err)
		}
		if err = owner.ObserveGPU(ctx, testGPU(now)); !errors.Is(err, ErrStale) {
			t.Fatal(err)
		}
		if err = session.Close(ctx); err != nil {
			t.Fatal(err)
		}
		anchor, _ = s.RuntimeAnchor(ctx)
		if anchor.GPU == nil || len(anchor.Sessions) != 0 {
			t.Fatal("удаление Session затронуло GPU")
		}
		time.Sleep(2 * time.Second)
		anchor, _ = s.RuntimeAnchor(ctx)
		if anchor.GPU.Devices[0].Utilization.Status != MetricStale || anchor.GPU.Devices[0].Utilization.Value != 37 || !anchor.GPU.Devices[0].Utilization.LastSuccessAt.Equal(now) {
			t.Fatalf("свежесть GPU: %+v", anchor.GPU)
		}
		if err = owner.Close(ctx); err != nil {
			t.Fatal(err)
		}
		anchor, _ = s.RuntimeAnchor(ctx)
		if anchor.GPU != nil {
			t.Fatal("GPU сохранён после close")
		}
		replacement, err := s.OpenSystemObserver(ctx)
		if err != nil {
			t.Fatal(err)
		}
		if replacement.Token() == owner.Token() {
			t.Fatal("token переиспользован")
		}
		if err = owner.ObserveGPU(ctx, testGPU(time.Now())); !errors.Is(err, ErrStale) {
			t.Fatal(err)
		}
		if err = owner.Close(ctx); err != nil {
			t.Fatal(err)
		}
		if !replacement.Valid() {
			t.Fatal("старый close отозвал нового владельца")
		}
	})
}
func TestRatesValidateMonotonicDurationWithoutWallClockEquality(t *testing.T) {
	at := time.Now()
	r := Rate{Status: MetricAvailable, Value: 60, WindowStart: at, WindowEnd: at.Add(-time.Second), Duration: time.Second}
	if !r.valid() {
		t.Fatal("коррекция wall clock отвергла положительное монотонное окно")
	}
	for _, invalid := range []Rate{{Status: MetricAvailable, Value: 1, WindowStart: at, WindowEnd: at, Duration: 0}, {Status: MetricAvailable, Value: math.NaN(), WindowStart: at, WindowEnd: at, Duration: time.Second}, {Status: MetricUnavailable, Value: 10}, {Status: "invalid"}} {
		if invalid.valid() {
			t.Fatalf("неверная скорость принята: %+v", invalid)
		}
	}
}

func TestOutputTerminalObservationDoesNotRevokeExecution(t *testing.T) {
	ctx := context.Background()
	s, u, parent, fence := telemetryTestService(t, telemetryTestLimits())
	owner, err := s.OpenOutputObserver(ctx, fence)
	if err != nil {
		t.Fatal(err)
	}
	if err = owner.Observe(ctx, OutputObservation{State: OutputPublishing}); err != nil {
		t.Fatal(err)
	}
	views, writes := u.views, u.writes
	if err = owner.ObserveTerminal(ctx, OutputObservation{State: OutputFailed, Error: "synthetic-transport-failure"}); err != nil {
		t.Fatal(err)
	}
	snapshot, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if err != nil || snapshot.Outputs[0].Value.State != OutputFailed || !owner.Valid() || u.views != views || u.writes != writes {
		t.Fatalf("RAM-отказ изменил допуск либо долговечное состояние: %+v %v", snapshot, err)
	}
	page, err := s.RuntimeEvents(ctx, RuntimeCursor{}, 16)
	if err != nil {
		t.Fatal(err)
	}
	last := page.Events[len(page.Events)-1]
	if last.OutputState != OutputFailed || last.Reason != ReasonObservationFailed {
		t.Fatalf("отказ отсутствует в потоке: %+v", last)
	}
	if err = owner.ObserveTerminal(ctx, OutputObservation{State: OutputPublishing}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if err = owner.ObserveTerminal(ctx, OutputObservation{State: OutputStopped}); err != nil {
		t.Fatal(err)
	}
}

func TestTelemetryRejectsNonFiniteProgressEvenWhenFieldAbsent(t *testing.T) {
	ctx := context.Background()
	s, _, parent, _ := telemetryTestService(t, telemetryTestLimits())
	session, _ := s.OpenSessionObserver(ctx, parent)
	group, _ := session.OpenGroup(ctx, "group", "profile")
	worker, _ := group.OpenFFmpegRole(ctx, FFmpegLiveTranscode)
	attempt, _ := worker.BeginAttempt(ctx, 1)
	value := FFmpegMetrics{SampledAt: time.Now(), Progress: FFmpegProgress{FPS: OptionalFloat64{Value: math.NaN()}}}
	if err := attempt.ObserveMetrics(ctx, value); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if err := group.Observe(ctx, GroupObservation{State: ObservationReady, Source: ObservationSourceLive, SourceRevision: 2}); err != nil {
		t.Fatal(err)
	}
	if err := group.Observe(ctx, GroupObservation{State: ObservationFailed, Source: ObservationSourceNone}); !errors.Is(err, ErrStale) {
		t.Fatal("запоздалое неверсионированное состояние принято", err)
	}
}

func TestGPUBudgetRejectionPreservesPreviousSnapshot(t *testing.T) {
	ctx := context.Background()
	limits := telemetryTestLimits()
	limits.MaxGPUBytes = 1500
	s, _, _, _ := telemetryTestService(t, limits)
	owner, err := s.OpenSystemObserver(ctx)
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now()
	first := testGPU(now)
	if err = owner.ObserveGPU(ctx, first); err != nil {
		t.Fatal(err)
	}
	rejected := testGPU(now.Add(time.Second))
	other := rejected.Devices[0]
	other.UUID = "GPU-other"
	rejected.Devices = append(rejected.Devices, other)
	if err = owner.ObserveGPU(ctx, rejected); !errors.Is(err, ErrObservationLimit) {
		t.Fatal(err)
	}
	anchor, err := s.RuntimeAnchor(ctx)
	if err != nil || len(anchor.GPU.Devices) != 1 || anchor.GPU.Devices[0].UUID != "GPU-test" || !anchor.GPU.SampledAt.Equal(now) {
		t.Fatalf("отказ бюджета изменил прежний снимок: %+v %v", anchor.GPU, err)
	}
}
