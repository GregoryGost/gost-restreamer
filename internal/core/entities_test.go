package core_test

import (
	"errors"
	"math"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

func TestSessionPublicationRequiresRunningAndSavedKey(t *testing.T) {
	for _, state := range []core.SessionState{core.SessionStarting, core.SessionRunning, core.SessionStopping, core.SessionStopped, core.SessionFailed, core.SessionInterrupted, "неизвестно"} {
		for _, paused := range []bool{false, true} {
			s := core.Session{State: state, DesiredPaused: paused, Snapshot: core.SessionSnapshot{Stream: core.Stream{PublicationKey: "synthetic-publication-key"}}}
			if got := s.AllowsOBS("synthetic-publication-key"); got != (state == core.SessionRunning) {
				t.Fatalf("состояние %s, пауза %t: допуск = %t", state, paused, got)
			}
			if s.AllowsOBS("чужой-ключ") || s.AllowsOBS("") {
				t.Fatalf("состояние %s допускает неверный ключ", state)
			}
		}
	}
}

func TestRevisionCannotWrap(t *testing.T) {
	for _, current := range []uint64{0, 1, math.MaxInt64, math.MaxUint64 - 1} {
		next, err := core.NextRevision(current)
		if err != nil || next <= current || next-current != 1 {
			t.Fatalf("следующая версия после %d = %d, ошибка %v", current, next, err)
		}
	}
	if _, err := core.NextRevision(math.MaxUint64); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("переполнение должно дать конфликт: %v", err)
	}
}

func TestManagedPathsStayWithinOwnedRoot(t *testing.T) {
	for _, p := range []string{"originals/source.bin", "variants/generation-1.nut", "папка/файл"} {
		if err := core.ValidateManagedPath(p); err != nil {
			t.Errorf("допустимый относительный путь %q: %v", p, err)
		}
	}
	for _, p := range []string{"", ".", "..", "../secret", "/absolute", "a/../secret", "a//b", "a/./b", "a/", "a\\b", "a\x00b", string([]byte{0xff})} {
		if err := core.ValidateManagedPath(p); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("опасный или неоднозначный путь %q принят: %v", p, err)
		}
	}
}

func TestDestinationSupportsCustomRTMPAndRejectsOtherProtocols(t *testing.T) {
	base := core.Destination{StreamID: "stream", GroupID: "group", Name: "Назначение", Platform: "custom-platform", StreamKey: ""}
	for _, endpoint := range []string{"rtmp://example.invalid/live", "rtmps://example.invalid:443/live"} {
		d := base
		d.Endpoint = endpoint
		if err := d.Validate(); err != nil {
			t.Errorf("пользовательское назначение %q отклонено: %v", endpoint, err)
		}
	}
	for _, endpoint := range []string{"https://example.invalid/live", "file:///tmp/media", "rtmp:///live", "rtmp://", "rtmp://example.invalid/live#fragment", "://"} {
		d := base
		d.Endpoint = endpoint
		if err := d.Validate(); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("недопустимое назначение %q принято: %v", endpoint, err)
		}
	}
}
