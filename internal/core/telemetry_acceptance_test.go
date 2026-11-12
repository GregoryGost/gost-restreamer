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

// Задаёт тестовые пределы телеметрии с выбранным числом событий и конечными бюджетами нагрузки.
func acceptanceTelemetryLimits(events int) TelemetryLimits {
	return TelemetryLimits{MediaStaleAfter: 10 * time.Second, Events: EventLimits{MaxEvents: events, MaxPageSize: events, MaxBytes: 1 << 20, MaxEventBytes: 16384}, MaxGPUDevices: 2, MaxGPUUUIDBytes: 64, MaxGPUBytes: 4096}
}

// Проверяет согласованность состояния, метрик и курсора снимка с последующими событиями без БД,
// повторов и разделяемых изменяемых нагрузок.
func TestAcceptanceTelemetryKeepsStateMetricsAndCursorConsistent(t *testing.T) {
	// ctx задаёт контекст операций проверяемого сценария оперативных наблюдений.
	ctx := context.Background()
	// s предоставляет доменные операции с общим реестром наблюдений. u предоставляет изменяемые
	// тестовые репозитории и учитывает обращения к БД. parent идентифицирует точное выполнение
	// родительской сессии.
	s, u, parent, _ := newObservationAcceptanceService(t, acceptanceObservationLimits())
	// err сообщает об ошибке настройки общих пределов телеметрии.
	if err := s.ConfigureTelemetry(ctx, acceptanceTelemetryLimits(16)); err != nil {
		t.Fatal(err)
	}
	// so содержит наблюдателя текущего выполнения сессии. err сообщает об ошибке регистрации
	// наблюдателя точной сессии.
	so, err := s.OpenSessionObserver(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	// g содержит наблюдателя отдельной оперативной группы.
	g, err := so.OpenGroup(ctx, "profile-group", "profile")
	if err != nil {
		t.Fatal(err)
	}
	// state задаёт наблюдаемое состояние группы с источником и ревизией.
	state := GroupObservation{State: ObservationReady, Source: ObservationSourceLive, SourceRevision: 2, Error: "synthetic-private-diagnostic"}
	if err = g.Observe(ctx, state); err != nil {
		t.Fatal(err)
	}
	// anchor фиксирует согласованный снимок и исходную позицию событий.
	anchor, err := s.RuntimeAnchor(ctx)
	if err != nil || len(anchor.Sessions) != 1 || anchor.Sessions[0].Groups[0].Value.Error != state.Error {
		t.Fatal("RuntimeAnchor не сохранил текущее наблюдение", anchor, err)
	}
	// views сохраняет число чтений БД перед проверяемой операцией RAM.
	views := u.views
	u.viewErr = errors.New("хранилище недоступно")
	// at задаёт время синтетической выборки медиа или GPU.
	at := time.Now().UTC()
	// m содержит согласованный комплект синтетических измерений группы.
	m := GroupMetrics{LiveInput: MediaMetrics{SampledAt: at, VideoFrames: 11, PayloadBytes: 101}, FallbackInput: MediaMetrics{SampledAt: at, VideoFrames: 22, PayloadBytes: 202}, Output: MediaMetrics{SampledAt: at, VideoFrames: 33, PayloadBytes: 303}}
	if err = g.ObserveMetrics(ctx, m); err != nil {
		t.Fatal(err)
	}
	select {
	case <-anchor.Changed:
	default:
		t.Fatal("RuntimeAnchor не разбудил читателя при новом событии")
	}
	// page содержит ограниченную страницу оперативных событий после исходного курсора.
	page, err := s.RuntimeEvents(ctx, anchor.Cursor, 16)
	if err != nil || page.Gap || page.Reset || len(page.Events) != 1 || page.Events[0].Kind != RuntimeMetricsEvent || *page.Events[0].Group != m {
		t.Fatal("событие после RuntimeAnchor потеряно или повторено", page, err)
	}
	// snapshot содержит согласованную копию текущих наблюдений сессии.
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
	// unchanged содержит события после повторной записи того же состояния.
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
	// all содержит все доступные оперативные события для проверки безопасной нагрузки.
	all, err := s.RuntimeEvents(ctx, RuntimeCursor{}, 16)
	if err != nil {
		t.Fatal(err)
	}
	// encoded содержит JSON оперативных событий для проверки отсутствия текста диагностики.
	encoded, err := json.Marshal(all.Events)
	if err != nil || strings.Contains(string(encoded), "synthetic-private-diagnostic") {
		t.Fatal("событие RAM экспортирует диагностическую строку", err)
	}
	if u.views != views || u.writes != 0 {
		t.Fatal("наблюдение обратилось к хранилищу", u.views, views, u.writes)
	}
}

// Проверяет разрывы после вытеснения и удаления, сброс чужой эпохи событий и независимость GPU от
// жизненного цикла Session.
func TestAcceptanceTelemetryEvictionPurgeAndSystemIsolation(t *testing.T) {
	// ctx задаёт контекст операций проверяемого сценария оперативных наблюдений.
	ctx := context.Background()
	// s предоставляет доменные операции с общим реестром наблюдений. parent идентифицирует точное
	// выполнение родительской сессии.
	s, _, parent, _ := newObservationAcceptanceService(t, acceptanceObservationLimits())
	// err сообщает об ошибке настройки общих пределов телеметрии.
	if err := s.ConfigureTelemetry(ctx, acceptanceTelemetryLimits(2)); err != nil {
		t.Fatal(err)
	}
	// so содержит наблюдателя текущего выполнения сессии. err сообщает об ошибке регистрации
	// наблюдателя точной сессии.
	so, err := s.OpenSessionObserver(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	// initial фиксирует начальный курсор до переполнения журнала.
	initial, err := s.RuntimeAnchor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// state задаёт очередное состояние сессии для переполнения журнала событий.
	for _, state := range []ObservationState{ObservationStarting, ObservationReady, ObservationRetrying} {
		if err = so.Observe(ctx, SessionObservation{State: state}); err != nil {
			t.Fatal(err)
		}
	}
	// page содержит ограниченную страницу оперативных событий после исходного курсора.
	page, err := s.RuntimeEvents(ctx, initial.Cursor, 2)
	if err != nil || !page.Gap || page.Reset || len(page.Events) != 2 || page.Events[0].State != ObservationReady || page.Events[1].State != ObservationRetrying {
		t.Fatal("вытеснение не выявило пропуск целых событий", page, err)
	}
	// foreign содержит курсор с чужой эпохой для проверки сброса.
	foreign := page.Cursor
	foreign.Epoch = "another-process"
	// reset содержит страницу после обнаружения чужой эпохи.
	reset, err := s.RuntimeEvents(ctx, foreign, 2)
	if err != nil || !reset.Gap || !reset.Reset {
		t.Fatal("чужой курсор не требует сброса", reset, err)
	}
	if _, err = s.RuntimeEvents(ctx, RuntimeCursor{}, 3); !errors.Is(err, ErrInvalid) {
		t.Fatal("лимит страницы не соблюдён", err)
	}
	// system содержит независимого системного наблюдателя GPU.
	system, err := s.OpenSystemObserver(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// at задаёт время синтетической выборки медиа или GPU.
	at := time.Now()
	if err = system.ObserveGPU(ctx, GPUSnapshot{StaleAfter: 10 * time.Second, SampledAt: at, Status: MetricUnsupported, Reason: ReasonUnsupportedPlatform}); err != nil {
		t.Fatal(err)
	}
	// beforeClose фиксирует общий снимок до удаления наблюдений сессии.
	beforeClose, err := s.RuntimeAnchor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if err = so.Close(ctx); err != nil {
		t.Fatal(err)
	}
	// afterClose содержит общий снимок после удаления сессии для проверки сохранности GPU.
	afterClose, err := s.RuntimeAnchor(ctx)
	if err != nil || len(afterClose.Sessions) != 0 || afterClose.GPU == nil || afterClose.GPU.Status != MetricUnsupported || !system.Valid() {
		t.Fatal("удаление Session повредило системное наблюдение", afterClose, err)
	}
	// purged содержит события после очистки данных сессии для проверки разрыва.
	purged, err := s.RuntimeEvents(ctx, RuntimeCursor{}, 2)
	if err != nil || !purged.Gap {
		t.Fatal("очистка скрыла пропуск", purged, err)
	}
	// event содержит выбранное оперативное событие для проверки владельца и безопасной нагрузки.
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
	// replacement содержит нового системного владельца GPU после закрытия прежнего.
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

// Проверяет сброс прогресса при новой попытке, атомарность отказа бюджета и сохранение текущих
// значений и возможности очистки при исчерпании событий.
func TestAcceptanceTelemetryAttemptResetAndBudgetRejectionAreAtomic(t *testing.T) {
	// ctx задаёт контекст операций проверяемого сценария оперативных наблюдений.
	ctx := context.Background()
	// s предоставляет доменные операции с общим реестром наблюдений. u предоставляет изменяемые
	// тестовые репозитории и учитывает обращения к БД. parent идентифицирует точное выполнение
	// родительской сессии.
	s, u, parent, _ := newObservationAcceptanceService(t, acceptanceObservationLimits())
	// err сообщает об ошибке настройки общих пределов телеметрии.
	if err := s.ConfigureTelemetry(ctx, acceptanceTelemetryLimits(16)); err != nil {
		t.Fatal(err)
	}
	// so содержит наблюдателя текущего выполнения сессии. err сообщает об ошибке регистрации
	// наблюдателя точной сессии.
	so, err := s.OpenSessionObserver(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	// g содержит наблюдателя отдельной оперативной группы.
	g, err := so.OpenGroup(ctx, "group", "profile")
	if err != nil {
		t.Fatal(err)
	}
	// w содержит наблюдателя работника FFmpeg внутри группы.
	w, err := g.OpenFFmpegRole(ctx, FFmpegLiveTranscode)
	if err != nil {
		t.Fatal(err)
	}
	// first содержит наблюдателя первой попытки FFmpeg.
	first, err := w.BeginAttempt(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	// m содержит исходный progress первой попытки FFmpeg.
	m := FFmpegMetrics{SampledAt: time.Now(), HasProgress: true, Progress: FFmpegProgress{Frame: OptionalUint64{Present: true, Value: 50}}}
	if err = first.ObserveMetrics(ctx, m); err != nil {
		t.Fatal(err)
	}
	// second содержит наблюдателя следующей попытки FFmpeg, заменяющей первую.
	second, err := w.BeginAttempt(ctx, 2)
	if err != nil {
		t.Fatal(err)
	}
	if err = first.ObserveMetrics(ctx, FFmpegMetrics{SampledAt: m.SampledAt.Add(time.Second)}); !errors.Is(err, ErrStale) {
		t.Fatal("старая попытка опубликовала прогресс", err)
	}
	// snapshot содержит согласованную копию текущих наблюдений сессии.
	snapshot, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if err != nil || snapshot.FFmpeg[0].Metrics != nil || snapshot.FFmpeg[0].Stamp.AttemptID != 2 || snapshot.FFmpeg[0].Role != FFmpegLiveTranscode {
		t.Fatal("новая попытка унаследовала прогресс", snapshot, err)
	}
	if err = second.ObserveMetrics(ctx, m); err != nil {
		t.Fatal("новая попытка унаследовала исходную метку времени", err)
	}
	// before сохраняет согласованный снимок до проверяемого действия.
	before, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if err != nil {
		t.Fatal(err)
	}
	if err = g.Observe(ctx, GroupObservation{State: ObservationFailed, Source: ObservationSourceNone, Error: strings.Repeat("x", 257)}); !errors.Is(err, ErrObservationLimit) {
		t.Fatal("переполнение строки принято", err)
	}
	// after содержит согласованный снимок после проверяемого действия для проверки сохранности.
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

// Проверяет немедленное терминальное RAM-наблюдение с точным ExecutionFence без физического
// освобождения, записи управления и раскрытия текста ошибки в событии.
func TestAcceptanceTerminalOutputObservationPreservesPhysicalAndControlOwnership(t *testing.T) {
	// terminal задаёт проверяемое терминальное состояние FAILED или STOPPED.
	for _, terminal := range []OutputState{OutputFailed, OutputStopped} {
		t.Run(string(terminal), func(t *testing.T) {
			// Проверяет для FAILED и STOPPED немедленное терминальное RAM-наблюдение без SQL, отзыва физического
			// допуска, освобождения слотов и изменения квитанции.

			// ctx задаёт контекст операций проверяемого сценария оперативных наблюдений.
			ctx := context.Background()
			// s предоставляет доменные операции с общим реестром наблюдений. u предоставляет
			// изменяемые тестовые репозитории и учитывает обращения к БД. fence
			// идентифицирует точную попытку выхода для проверки терминального RAM-состояния.
			s, u, _, fence := newObservationAcceptanceService(t, acceptanceObservationLimits())
			// err сообщает об ошибке настройки общих пределов телеметрии.
			if err := s.ConfigureTelemetry(ctx, acceptanceTelemetryLimits(8)); err != nil {
				t.Fatal(err)
			}
			// lease содержит логический допуск точного сохраняемого исполнения. err сообщает
			// об ошибке выдачи логического допуска точного исполнения.
			lease, err := s.BindExecutionOwner(ctx, fence)
			if err != nil {
				t.Fatal(err)
			}
			// reservation удерживает физический резерв точного выхода до подтверждённого
			// освобождения.
			reservation, err := s.ReserveOutputExecution(ctx, lease, u.repository.outputs[fence.OutputID].Snapshot)
			if err != nil {
				t.Fatal(err)
			}
			// observer содержит наблюдателя точной попытки выхода.
			observer, err := s.OpenOutputObserver(ctx, fence)
			if err != nil {
				t.Fatal(err)
			}
			if err = reservation.Activate(ctx, observer); err != nil {
				t.Fatal(err)
			}
			// blockedWrite занимает слот записи, имитируя ещё не завершённую внешнюю
			// операцию.
			blockedWrite, err := reservation.Admit(ctx, OutputOperationWrite)
			if err != nil {
				t.Fatal(err)
			}
			defer blockedWrite.Finish()
			// anchor фиксирует согласованный снимок и исходную позицию событий.
			anchor, err := s.RuntimeAnchor(ctx)
			if err != nil {
				t.Fatal(err)
			}
			// beforeOutput сохраняет долговечный выход до чисто оперативного терминального
			// наблюдения.
			beforeOutput := u.repository.outputs[fence.OutputID]
			// beforeReceipt сохраняет квитанцию до чисто оперативного терминального
			// наблюдения.
			beforeReceipt := u.repository.receipts[fence.CommandID]
			// views сохраняет число чтений БД перед проверяемой операцией RAM.
			views := u.views
			u.viewErr = errors.New("хранилище недоступно")
			// value содержит терминальное состояние выхода с синтетической диагностикой.
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
			// snapshot содержит согласованную копию текущих наблюдений сессии.
			snapshot, err := s.RuntimeSnapshot(ctx, observationSessionFence(fence))
			if err != nil || len(snapshot.Outputs) != 1 || *snapshot.Outputs[0].Value != value || snapshot.Outputs[0].Execution != fence {
				t.Fatal("ошибка не опубликована немедленно точным владельцем", snapshot, err)
			}
			// page содержит ограниченную страницу оперативных событий после исходного
			// курсора.
			page, err := s.RuntimeEvents(ctx, anchor.Cursor, 8)
			if err != nil || len(page.Events) != 1 || page.Events[0].OutputState != terminal || page.Events[0].Reason != ReasonObservationFailed || page.Events[0].Execution != fence {
				t.Fatal("безопасное терминальное событие потеряно", page, err)
			}
			// encoded содержит JSON оперативных событий для проверки отсутствия текста
			// диагностики.
			encoded, err := json.Marshal(page.Events)
			if err != nil || strings.Contains(string(encoded), value.Error) {
				t.Fatal("событие содержит текст диагностики", err)
			}
			select {
			case <-reservation.Done():
				t.Fatal("наблюдение запечатало физическое исполнение")
			default:
			}
			// read содержит допущенную операцию чтения после терминального RAM-наблюдения.
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
			// replacement содержит нового наблюдателя после отзыва прежнего владельца.
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
