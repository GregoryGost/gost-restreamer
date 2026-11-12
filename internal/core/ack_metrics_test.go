package core

import (
	"context"
	"errors"
	"math"
	"testing"
	"time"
)

// Проверяет точные uint64 и UTC диагностических показателей ACK в RAM и событиях без БД, независимость
// копий и защиту нового наблюдателя от старых выборок.
func TestOutputACKMetricsRemainInRAMAndUseExactObserver(t *testing.T) {
	// ctx задаёт контекст операций проверки оперативных ACK-метрик.
	ctx := context.Background()
	// s предоставляет доменные операции с телеметрией, u учитывает обращения к хранилищу, parent
	// идентифицирует сессию, а fence — точную попытку выхода.
	s, u, parent, fence := telemetryTestService(t, telemetryTestLimits())
	// observer принимает метрики точной попытки выхода, а err — результат его регистрации.
	observer, err := s.OpenOutputObserver(ctx, fence)
	if err != nil {
		t.Fatal(err)
	}
	// views сохраняет исходное число чтений БД, а writes — исходное число записей для проверки
	// работы только в RAM.
	views, writes := u.views, u.writes
	u.viewErr = errors.New("хранилище недоступно")
	// at задаёт время выборки с ненулевым часовым поясом для проверки нормализации UTC.
	at := time.Now().In(time.FixedZone("UTC+3", 3*60*60))
	// metrics содержит исходную выборку выхода с нулевыми диагностическими счётчиками ACK.
	metrics := OutputMetrics{MediaMetrics: testMedia(at)}
	if err = observer.ObserveMetrics(ctx, metrics); err != nil {
		t.Fatal(err)
	}
	// snapshot содержит согласованную копию текущих наблюдений сессии.
	snapshot, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if err != nil {
		t.Fatal(err)
	}
	// initial содержит первое измерение выхода для проверки отсутствия вымышленных ACK-аномалий.
	initial := snapshot.Outputs[0].Metrics
	if initial.AckAheadEventsTotal != 0 || initial.AckAheadBytesLast != 0 || initial.AckAheadBytesMax != 0 || !initial.AckLastAnomalyAt.IsZero() {
		t.Fatalf("начальное наблюдение содержит событие ACK: %+v", initial)
	}
	// anchor фиксирует позицию событий перед записью ACK-аномалии.
	anchor, err := s.RuntimeAnchor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	metrics.SampledAt = at.Add(time.Second)
	metrics.AckAheadEventsTotal = math.MaxUint64
	metrics.AckAheadBytesLast = 9007199254740993
	metrics.AckAheadBytesMax = math.MaxUint64
	metrics.AckLastAnomalyAt = at
	if err = observer.ObserveMetrics(ctx, metrics); err != nil {
		t.Fatal(err)
	}
	// page содержит события, появившиеся после записи ACK-аномалии.
	page, err := s.RuntimeEvents(ctx, anchor.Cursor, 16)
	if err != nil || len(page.Events) != 1 || page.Events[0].Output == nil {
		t.Fatalf("событие измерения потеряно: %+v %v", page, err)
	}
	// want содержит ожидаемые точные ACK-счётчики и временные метки в UTC.
	want := metrics
	want.MediaMetrics = utcMedia(want.MediaMetrics)
	want.AckLastAnomalyAt = at.UTC()
	snapshot, err = s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if err != nil || *snapshot.Outputs[0].Metrics != want || *page.Events[0].Output != want || page.Events[0].Output.AckLastAnomalyAt.Location() != time.UTC {
		t.Fatalf("снимок или событие изменили точность ACK либо UTC: %+v %+v %v", snapshot, page, err)
	}
	page.Events[0].Output.AckAheadBytesLast = 0
	snapshot.Outputs[0].Metrics.AckAheadEventsTotal = 0
	metrics.AckAheadEventsTotal = 1
	if err = observer.ObserveMetrics(ctx, metrics); !errors.Is(err, ErrStale) {
		t.Fatalf("повторная выборка изменила метрики: %v", err)
	}
	// again содержит повторный снимок после изменения возвращённых копий и отклонённой старой
	// выборки.
	again, err := s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if err != nil || *again.Outputs[0].Metrics != want || u.views != views || u.writes != writes {
		t.Fatalf("метрики изменены через ссылку, stale либо хранилище: %+v %v", again, err)
	}
	u.viewErr = nil
	// replacement содержит нового наблюдателя той же попытки, который должен отозвать прежний token.
	replacement, err := s.OpenOutputObserver(ctx, fence)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, err = s.RuntimeSnapshot(ctx, observationSessionFence(parent))
	if err != nil || snapshot.Outputs[0].Metrics != nil {
		t.Fatalf("новый владелец унаследовал измерение: %+v %v", snapshot, err)
	}
	metrics.SampledAt = at.Add(2 * time.Second)
	if err = observer.ObserveMetrics(ctx, metrics); !errors.Is(err, ErrStale) || !replacement.Valid() {
		t.Fatalf("старый владелец изменил замену: %v", err)
	}
	if err = replacement.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if _, err = s.RuntimeSnapshot(ctx, observationSessionFence(parent)); !errors.Is(err, ErrNotReady) {
		t.Fatalf("закрытый владелец сохранил метрики: %v", err)
	}
}
