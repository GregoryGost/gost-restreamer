package core_test

import (
	"errors"
	"math"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// Проверяет допуск OBS только по сохранённому ключу RUNNING-сессии независимо от ручной паузы.
func TestSessionPublicationRequiresRunningAndSavedKey(t *testing.T) {
	// state содержит проверяемое долговечное состояние сессии, включая неизвестное значение.
	for _, state := range []core.SessionState{core.SessionStarting, core.SessionRunning, core.SessionStopping, core.SessionStopped, core.SessionFailed, core.SessionInterrupted, "неизвестно"} {
		// paused задаёт наличие ручной паузы для каждого проверяемого состояния.
		for _, paused := range []bool{false, true} {
			// s моделирует сессию с заданными состоянием, паузой и сохранённым ключом OBS.
			s := core.Session{State: state, DesiredPaused: paused, Snapshot: core.SessionSnapshot{Stream: core.Stream{PublicationKey: "synthetic-publication-key"}}}
			// got содержит фактическое решение о допуске OBS с сохранённым ключом.
			if got := s.AllowsOBS("synthetic-publication-key"); got != (state == core.SessionRunning) {
				t.Fatalf("состояние %s, пауза %t: допуск = %t", state, paused, got)
			}
			if s.AllowsOBS("чужой-ключ") || s.AllowsOBS("") {
				t.Fatalf("состояние %s допускает неверный ключ", state)
			}
		}
	}
}

// Проверяет увеличение ревизии на единицу во всём диапазоне и ErrConflict вместо переполнения uint64.
func TestRevisionCannotWrap(t *testing.T) {
	// current содержит очередную граничную ревизию для проверки увеличения.
	for _, current := range []uint64{0, 1, math.MaxInt64, math.MaxUint64 - 1} {
		// next содержит увеличенную ревизию, а err — результат проверки её представимости.
		next, err := core.NextRevision(current)
		if err != nil || next <= current || next-current != 1 {
			t.Fatalf("следующая версия после %d = %d, ошибка %v", current, next, err)
		}
	}
	// err сообщает об ошибке попытки увеличить предельную ревизию.
	if _, err := core.NextRevision(math.MaxUint64); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("переполнение должно дать конфликт: %v", err)
	}
}

// Проверяет принятие нормализованных относительных путей и отказ для выхода через .., неоднозначных
// разделителей и некорректного текста.
func TestManagedPathsStayWithinOwnedRoot(t *testing.T) {
	// p содержит очередной допустимый относительный путь.
	for _, p := range []string{"originals/source.bin", "variants/generation-1.nut", "папка/файл"} {
		// err сообщает об ошибке проверки допустимого относительного пути.
		if err := core.ValidateManagedPath(p); err != nil {
			t.Errorf("допустимый относительный путь %q: %v", p, err)
		}
	}
	// p содержит очередной опасный или неоднозначный путь, который должен быть отклонён.
	for _, p := range []string{"", ".", "..", "../secret", "/absolute", "a/../secret", "a//b", "a/./b", "a/", "a\\b", "a\x00b", string([]byte{0xff})} {
		// err сообщает об ошибке отклонения опасного или неоднозначного пути.
		if err := core.ValidateManagedPath(p); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("опасный или неоднозначный путь %q принят: %v", p, err)
		}
	}
}

// Проверяет пользовательскую площадку с пустым ключом для RTMP/RTMPS и отказ для иных протоколов,
// пустого адреса и фрагмента URL.
func TestDestinationSupportsCustomRTMPAndRejectsOtherProtocols(t *testing.T) {
	// base задаёт корректные ссылки и пользовательскую площадку с пустым ключом.
	base := core.Destination{StreamID: "stream", GroupID: "group", Name: "Назначение", Platform: "custom-platform", StreamKey: ""}
	// endpoint содержит очередной допустимый адрес RTMP или RTMPS.
	for _, endpoint := range []string{"rtmp://example.invalid/live", "rtmps://example.invalid:443/live"} {
		// d содержит копию назначения для проверки допустимого адреса.
		d := base
		d.Endpoint = endpoint
		// err сообщает об ошибке проверки назначения с допустимым адресом.
		if err := d.Validate(); err != nil {
			t.Errorf("пользовательское назначение %q отклонено: %v", endpoint, err)
		}
	}
	// endpoint содержит очередной адрес с неверным протоколом, пустым хостом или фрагментом.
	for _, endpoint := range []string{"https://example.invalid/live", "file:///tmp/media", "rtmp:///live", "rtmp://", "rtmp://example.invalid/live#fragment", "://"} {
		// d содержит копию назначения для проверки недопустимого адреса.
		d := base
		d.Endpoint = endpoint
		// err сообщает об ошибке отклонения назначения с недопустимым адресом.
		if err := d.Validate(); !errors.Is(err, core.ErrInvalid) {
			t.Errorf("недопустимое назначение %q принято: %v", endpoint, err)
		}
	}
}
