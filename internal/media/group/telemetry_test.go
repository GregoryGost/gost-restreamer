package group

import (
	"testing"
)

// Проверяет сигналы только существенных изменений состояния, отдельные ревизии состояния и
// конфигурации и сохранение накоплений выхода после новой попытки.
func TestSwitcherStateRevisionAndPublishedCountersHaveSeparateLifetimes(t *testing.T) {
	// s содержит переключатель; v и a задают контрольную пару дорожек; now задаёт общее время
	// наблюдений состояния.
	s, v, a, now := switcherFixture(t)
	// before сохраняет первоначальный статус; changed содержит сигнал существенного изменения
	// состояния.
	before, changed := s.StateChange()
	allowUnavailable(t, s.Tick(now))
	// got содержит статус после повторного Tick для проверки неизменной ревизии.
	if got := s.Status(); got.StateRevision != before.StateRevision {
		t.Fatal("повторная проверка изменила состояние", got)
	}
	select {
	case <-changed:
		t.Fatal("повторная проверка отправила событие")
	default:
	}
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, v, v, a, 0, 0, 1600, true, now))
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, a, v, a, 0, 0, 1024, false, now))
	// ready сохраняет статус после первой пригодной fallback-пары.
	ready := s.Status()
	if ready.StateRevision <= before.StateRevision {
		t.Fatal("готовность не изменила ревизию", ready)
	}
	select {
	case <-changed:
	default:
		t.Fatal("готовность не пробудила наблюдателя")
	}
	// wantBytes задаёт ожидаемое число полезных байтов одного AV1 TU и одного AAC AU.
	wantBytes := uint64(len(av1TU(true)) + 6)
	// got содержит накопления успешно опубликованной начальной пары.
	if got := s.OutputSnapshot(); got.VideoFrames != 1 || got.AudioUnits != 1 || got.PayloadBytes != wantBytes || got.SampledAt.IsZero() {
		t.Fatalf("неверные опубликованные единицы: %+v", got)
	}
	allowUnavailable(t, s.SetPaused(true, now))
	// got содержит статус ручной паузы для разделения ревизии состояния и конфигурации.
	if got := s.Status(); got.StateRevision <= ready.StateRevision || got.Revision != ready.Revision {
		t.Fatal("смешаны ревизии состояния и конфигурации", got)
	}
	// err сообщает об отказе начала новой fallback-попытки.
	if err := s.BeginAttempt(Fallback, 2); err != nil {
		t.Fatal(err)
	}
	if s.Status().Available {
		t.Fatal("новая попытка сохранила готовность")
	}
	// got содержит выходные накопления после новой попытки для проверки сохранения счётчиков группы.
	if got := s.OutputSnapshot(); got.PayloadBytes != wantBytes {
		t.Fatal("новая попытка сбросила счётчики группы", got)
	}
}
