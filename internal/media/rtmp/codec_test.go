package rtmp

import (
	"errors"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

func testAV1Config() []byte {
	// Заголовок последовательности Chrome Main 1920x804 с одним слоем.
	return []byte{0x81, 8, 0x0c, 0, 0x0a, 11, 0, 0, 0, 66, 167, 191, 228, 96, 13, 0, 64}
}
func testAV1TU() []byte {
	// Синтетический TU проверяет только разобранную часть uncompressed_header.
	// Полная декодируемость проверяется отдельной интеграцией с FFmpeg.
	return append(append([]byte(nil), testAV1Config()[4:]...), 0x1a, 1, 0x10)
}

func TestObservedAV1FormatRequiresShownKeyDimensionsAndSignaledRate(t *testing.T) {
	config, tu := testAV1Config(), testAV1TU()
	fps := core.FrameRate{Numerator: 30000, Denominator: 1001}
	if _, _, err := InspectAV1TU(config, tu, core.FrameRate{}); !errors.Is(err, ErrUnready) {
		t.Fatal(err)
	}
	f, key, err := InspectAV1TU(config, tu, fps)
	if err != nil {
		t.Fatal(err)
	}
	if !key || f.Width != 1920 || f.Height != 804 || !f.FrameRate.Equal(fps) || f.Color.Primaries != "unspecified" || f.BitDepth != 8 || f.PixelFormat != "yuv420p" {
		t.Fatalf("ложный наблюдаемый формат: %+v, %v", f, key)
	}
	d, err := parseAV1Config(config)
	if err != nil {
		t.Fatal(err)
	}
	// show_existing_frame не доказывает начальный независимый старт.
	if _, key, err = d.inspect([]byte{0x1a, 1, 0x80}, fps); !errors.Is(err, ErrUnready) || key {
		t.Fatalf("show_existing_frame объявлен ready: %v", err)
	}
	// Флаг ключевого кадра RTMP не участвует в доказательстве ключевого кадра.
	bad := append([]byte(nil), tu...)
	bad[len(bad)-1] = 0x30
	if _, key, err = InspectAV1TU(config, bad, fps); !errors.Is(err, ErrUnready) || key {
		t.Fatalf("inter объявлен независимым: %v", err)
	}
	bad = append([]byte(nil), tu...)
	bad[len(bad)-1] = 0x12
	if _, _, err = InspectAV1TU(config, bad, fps); !errors.Is(err, ErrCodec) {
		t.Fatalf("frame_size_override выдан за maxima: %v", err)
	}
}

func TestASCDeterminesActualRateAndChannels(t *testing.T) {
	for _, tc := range []struct {
		raw      []byte
		rate     uint32
		channels uint16
	}{
		{[]byte{0x11, 0x90}, 48000, 2},
		{[]byte{0x12, 0x08}, 44100, 1},
		{[]byte{0x11, 0x88, 0x56, 0xe5, 0x00}, 48000, 1},
		{[]byte{0x11, 0x90, 0x56, 0xe5, 0x00}, 48000, 2},
		{[]byte{0x17, 0x80, 0x18, 0x1c, 0x88}, 12345, 1},
	} {
		f, err := AudioFormatASC(tc.raw)
		layout := "mono"
		if tc.channels == 2 {
			layout = "stereo"
		}
		if err != nil || f.Codec != core.CodecAAC || f.CodecProfile != "LC" || f.SampleRateHz != tc.rate || f.Channels != tc.channels || f.ChannelLayout != layout {
			t.Fatalf("ASC: %+v, %v", f, err)
		}
	}
	for _, bad := range [][]byte{
		{}, {0x11}, {0x11, 0x80}, {0x11, 0x94}, {0x11, 0x92, 0, 0},
		{0x11, 0x90, 0}, {0x2b, 0x92, 8, 0}, // Эти случаи содержат PCE, SBR или некорректный основной ASC.
		{0x11, 0x88, 0, 0, 0},                // Неизвестное расширение должно быть отклонено.
		{0x11, 0x88, 0x56, 0xe0},             // Обрезанное расширение должно быть отклонено.
		{0x11, 0x88, 0x56, 0xe5, 0x80},       // Включённый SBR должен быть отклонён.
		{0x11, 0x88, 0x56, 0xe5, 0x01},       // Ненулевой padding должен быть отклонён.
		{0x11, 0x88, 0x56, 0xe5, 0x00, 0x00}, // Лишний байт после padding должен быть отклонён.
	} {
		if _, err := AudioFormatASC(bad); !errors.Is(err, ErrCodec) {
			t.Fatal(err)
		}
	}
}

func TestIngestClockExactlyRepresentsWireAndCodecDurations(t *testing.T) {
	for _, tc := range []struct {
		rate         core.FrameRate
		audio, scale uint32
		vd, ad       int64
	}{
		{core.FrameRate{Numerator: 30, Denominator: 1}, 48000, 48000, 1600, 1024},
		{core.FrameRate{Numerator: 60, Denominator: 1}, 48000, 48000, 800, 1024},
		{core.FrameRate{Numerator: 30000, Denominator: 1001}, 48000, 240000, 8008, 5120},
		{core.FrameRate{Numerator: 30, Denominator: 1}, 44100, 441000, 14700, 10240},
	} {
		scale, vd, ad, err := ingestClock(tc.rate, tc.audio)
		if err != nil || scale != tc.scale || vd != tc.vd || ad != tc.ad || scale%1000 != 0 {
			t.Fatalf("нет точного clock: %d %d %d %v", scale, vd, ad, err)
		}
	}
	if _, _, _, err := ingestClock(core.FrameRate{Numerator: ^uint32(0), Denominator: 1}, 48000); !errors.Is(err, ErrCodec) {
		t.Fatal(err)
	}
}

func TestSequenceTailDistinguishesSeparateUVFromFilmGrain(t *testing.T) {
	config := testAV1Config()
	config[len(config)-2] |= 1
	if _, err := parseAV1Config(config); err != nil {
		t.Fatalf("separate_uv_delta_q выдан за film grain: %v", err)
	}
	config = testAV1Config()
	config[len(config)-1] |= 0x80
	if _, err := parseAV1Config(config); !errors.Is(err, ErrCodec) {
		t.Fatalf("film grain пропущен из-за смещения: %v", err)
	}
}
