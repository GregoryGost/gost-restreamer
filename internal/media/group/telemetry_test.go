package group

import (
	"testing"
)

func TestSwitcherStateRevisionAndPublishedCountersHaveSeparateLifetimes(t *testing.T) {
	s, v, a, now := switcherFixture(t)
	before, changed := s.StateChange()
	allowUnavailable(t, s.Tick(now))
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
	ready := s.Status()
	if ready.StateRevision <= before.StateRevision {
		t.Fatal("готовность не изменила ревизию", ready)
	}
	select {
	case <-changed:
	default:
		t.Fatal("готовность не пробудила наблюдателя")
	}
	wantBytes := uint64(len(av1TU(true)) + 6)
	if got := s.OutputSnapshot(); got.VideoFrames != 1 || got.AudioUnits != 1 || got.PayloadBytes != wantBytes || got.SampledAt.IsZero() {
		t.Fatalf("неверные опубликованные единицы: %+v", got)
	}
	allowUnavailable(t, s.SetPaused(true, now))
	if got := s.Status(); got.StateRevision <= ready.StateRevision || got.Revision != ready.Revision {
		t.Fatal("смешаны ревизии состояния и конфигурации", got)
	}
	if err := s.BeginAttempt(Fallback, 2); err != nil {
		t.Fatal(err)
	}
	if s.Status().Available {
		t.Fatal("новая попытка сохранила готовность")
	}
	if got := s.OutputSnapshot(); got.PayloadBytes != wantBytes {
		t.Fatal("новая попытка сбросила счётчики группы", got)
	}
}
