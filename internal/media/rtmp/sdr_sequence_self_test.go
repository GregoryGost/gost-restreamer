package rtmp

import (
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// TestSDRSequenceCICPAndHighTier проверяет нормативные независимые поля цвета и tier,
// включая десятибитный Main и сохранение неизвестных числовых кодов без подстановки BT.709.
func TestSDRSequenceCICPAndHighTier(t *testing.T) {
	// tc задаёт три независимых кода и их ожидаемые имена.
	for _, tc := range []struct {
		// p, tr и m задают основные цвета, передаточную характеристику и матрицу CICP.
		p, tr, m byte
		// color задаёт ожидаемые имена в наблюдаемом формате.
		color core.ColorInfo
	}{
		{4, 4, 4, core.ColorInfo{Range: "limited", Primaries: "bt470m", Transfer: "gamma22", Matrix: "fcc"}},
		{5, 5, 5, core.ColorInfo{Range: "limited", Primaries: "bt470bg", Transfer: "gamma28", Matrix: "bt470bg"}},
		{7, 7, 7, core.ColorInfo{Range: "limited", Primaries: "smpte240m", Transfer: "smpte240m", Matrix: "smpte240m"}},
		{8, 8, 9, core.ColorInfo{Range: "limited", Primaries: "film", Transfer: "linear", Matrix: "bt2020nc"}},
		{9, 13, 10, core.ColorInfo{Range: "limited", Primaries: "bt2020", Transfer: "iec61966-2-1", Matrix: "bt2020c"}},
		{6, 15, 6, core.ColorInfo{Range: "limited", Primaries: "smpte170m", Transfer: "bt2020-12", Matrix: "smpte170m"}},
		{199, 200, 201, core.ColorInfo{Range: "limited", Primaries: "cicp:199", Transfer: "cicp:200", Matrix: "cicp:201"}},
	} {
		// s содержит нормативную последовательность с явными Main 10-bit и high tier уровня 4.0.
		s := contractSDRSequence()
		s.highDepth, s.highTier = true, true
		s.primaries, s.transfer, s.matrix = tc.p, tc.tr, tc.m
		// config содержит последовательность и согласованный префикс av1C.
		config := contractAV1Config(s)
		// actual, key и err содержат наблюдаемый формат, независимость и отказ разбора TU.
		actual, key, err := InspectAV1TU(config, contractAV1TU(config), core.FrameRate{})
		if err != nil || !key || actual.Color != tc.color || actual.Tier != "high" || actual.Level != "4.0" || actual.BitDepth != 10 || actual.PixelFormat != "yuv420p10le" {
			t.Fatalf("Неверные поля последовательности: %+v key=%v err=%v", actual, key, err)
		}
	}
}
