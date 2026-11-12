package rtmp

import (
	"errors"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// Возвращает фиксированный av1C Main с последовательностью 1920×804 без собственной частоты кадров.
func testAV1Config() []byte {
	// Заголовок последовательности Chrome Main 1920x804 с одним слоем.
	return []byte{0x81, 8, 0x0c, 0, 0x0a, 11, 0, 0, 0, 66, 167, 191, 228, 96, 13, 0, 64}
}

// Создаёт синтетический TU с последовательностью и префиксом показанного ключевого кадра для проверки
// разбираемой части заголовка.
func testAV1TU() []byte {
	// Синтетический TU проверяет только разобранную часть uncompressed_header.
	// Полная декодируемость проверяется отдельной интеграцией с FFmpeg.
	return append(append([]byte(nil), testAV1Config()[4:]...), 0x1a, 1, 0x10)
}

// Проверяет отсутствие готовности без FPS, наблюдаемые размеры и цвет и отказ считать
// show_existing_frame или межкадровый кадр независимым стартом.
func TestObservedAV1FormatRequiresShownKeyDimensionsAndSignaledRate(t *testing.T) {
	// config содержит фиксированный av1C без собственного FPS. tu содержит синтетический независимый
	// TU той же последовательности.
	config, tu := testAV1Config(), testAV1TU()
	// fps задаёт проверенную дробную частоту 30000/1001 для входа без timing_info.
	fps := core.FrameRate{Numerator: 30000, Denominator: 1001}
	// err содержит результат проверки TU без подтверждённой частоты кадров.
	if _, _, err := InspectAV1TU(config, tu, core.FrameRate{}); !errors.Is(err, ErrUnready) {
		t.Fatal(err)
	}
	// f содержит наблюдаемый формат видео. key показывает независимость первого кадра. err сообщает
	// об отказе проверки TU.
	f, key, err := InspectAV1TU(config, tu, fps)
	if err != nil {
		t.Fatal(err)
	}
	if !key || f.Width != 1920 || f.Height != 804 || !f.FrameRate.Equal(fps) || f.Color.Primaries != "unspecified" || f.BitDepth != 8 || f.PixelFormat != "yuv420p" {
		t.Fatalf("ложный наблюдаемый формат: %+v, %v", f, key)
	}
	// d содержит состояние AV1 для проверки show_existing_frame без подтверждённых размеров.
	d, err := parseAV1Config(config)
	if err != nil {
		t.Fatal(err)
	}
	// show_existing_frame не доказывает начальный независимый старт.
	if _, key, err = d.inspect([]byte{0x1a, 1, 0x80}, fps); !errors.Is(err, ErrUnready) || key {
		t.Fatalf("show_existing_frame объявлен ready: %v", err)
	}
	// Флаг ключевого кадра RTMP не участвует в доказательстве ключевого кадра.
	// bad содержит копию TU с изменяемым типом кадра или переопределением размеров.
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

// Проверяет частоту и каналы из ASC, включая явную частоту и отключённое SBR, и отказ при
// неподдержанном расширении или некорректном заполнении.
func TestASCDeterminesActualRateAndChannels(t *testing.T) {
	// tc содержит ASC и независимо ожидаемые частоту и число каналов.
	for _, tc := range []struct {
		// raw содержит проверяемую AudioSpecificConfig.
		raw []byte
		// rate задаёт ожидаемую частоту дискретизации в герцах.
		rate uint32
		// channels задаёт ожидаемое число аудиоканалов.
		channels uint16
	}{
		{[]byte{0x11, 0x90}, 48000, 2},
		{[]byte{0x12, 0x08}, 44100, 1},
		{[]byte{0x11, 0x88, 0x56, 0xe5, 0x00}, 48000, 1},
		{[]byte{0x11, 0x90, 0x56, 0xe5, 0x00}, 48000, 2},
		{[]byte{0x17, 0x80, 0x18, 0x1c, 0x88}, 12345, 1},
	} {
		// f содержит фактический формат ASC. err сообщает об отказе разбора.
		f, err := AudioFormatASC(tc.raw)
		// layout содержит ожидаемое обозначение mono или stereo.
		layout := "mono"
		if tc.channels == 2 {
			layout = "stereo"
		}
		if err != nil || f.Codec != core.CodecAAC || f.CodecProfile != "LC" || f.SampleRateHz != tc.rate || f.Channels != tc.channels || f.ChannelLayout != layout {
			t.Fatalf("ASC: %+v, %v", f, err)
		}
	}
	// bad содержит очередной повреждённый ASC либо неподдержанное расширение.
	for _, bad := range [][]byte{
		{}, {0x11}, {0x11, 0x80}, {0x11, 0x94}, {0x11, 0x92, 0, 0},
		{0x11, 0x90, 0}, {0x2b, 0x92, 8, 0}, // Эти случаи содержат PCE, SBR или некорректный основной ASC.
		{0x11, 0x88, 0, 0, 0},                // Неизвестное расширение должно быть отклонено.
		{0x11, 0x88, 0x56, 0xe0},             // Обрезанное расширение должно быть отклонено.
		{0x11, 0x88, 0x56, 0xe5, 0x80},       // Включённый SBR должен быть отклонён.
		{0x11, 0x88, 0x56, 0xe5, 0x01},       // Ненулевое заполнение должно быть отклонено.
		{0x11, 0x88, 0x56, 0xe5, 0x00, 0x00}, // Лишний байт после заполнения должен быть отклонён.
	} {
		// err содержит результат разбора недопустимого ASC.
		if _, err := AudioFormatASC(bad); !errors.Is(err, ErrCodec) {
			t.Fatal(err)
		}
	}
}

// Проверяет точную общую шкалу миллисекунд RTMP, целого и дробного FPS и AAC 44,1/48 кГц, включая
// отказ при переполнении.
func TestIngestClockExactlyRepresentsWireAndCodecDurations(t *testing.T) {
	// tc содержит входные частоты и ожидаемую общую шкалу с длительностями единиц.
	for _, tc := range []struct {
		// rate задаёт проверяемую частоту видео.
		rate core.FrameRate
		// audio задаёт частоту дискретизации AAC. scale задаёт ожидаемую общую частоту отсчётов.
		audio, scale uint32
		// vd задаёт ожидаемую длительность видеокадра в отсчётах. ad задаёт ожидаемую
		// длительность AAC-единицы в отсчётах.
		vd, ad int64
	}{
		{core.FrameRate{Numerator: 30, Denominator: 1}, 48000, 48000, 1600, 1024},
		{core.FrameRate{Numerator: 60, Denominator: 1}, 48000, 48000, 800, 1024},
		{core.FrameRate{Numerator: 30000, Denominator: 1001}, 48000, 240000, 8008, 5120},
		{core.FrameRate{Numerator: 30, Denominator: 1}, 44100, 441000, 14700, 10240},
	} {
		// scale содержит вычисленную общую шкалу. vd содержит длительность видеокадра. ad
		// содержит длительность AAC-единицы. err сообщает об отказе точного вычисления.
		scale, vd, ad, err := ingestClock(tc.rate, tc.audio)
		if err != nil || scale != tc.scale || vd != tc.vd || ad != tc.ad || scale%1000 != 0 {
			t.Fatalf("нет точного clock: %d %d %d %v", scale, vd, ad, err)
		}
	}
	// err содержит результат построения шкалы при переполнении допустимого диапазона.
	if _, _, _, err := ingestClock(core.FrameRate{Numerator: ^uint32(0), Denominator: 1}, 48000); !errors.Is(err, ErrCodec) {
		t.Fatal(err)
	}
}

// Проверяет разные позиции separate_uv_delta_q и film_grain_params_present: первый допускается, второй
// отклоняется.
func TestSequenceTailDistinguishesSeparateUVFromFilmGrain(t *testing.T) {
	// config содержит изменяемый av1C для раздельной проверки двух соседних битов хвоста.
	config := testAV1Config()
	config[len(config)-2] |= 1
	// err содержит результат разбора последовательности с допустимым separate_uv_delta_q.
	if _, err := parseAV1Config(config); err != nil {
		t.Fatalf("separate_uv_delta_q выдан за film grain: %v", err)
	}
	config = testAV1Config()
	config[len(config)-1] |= 0x80
	// err содержит результат разбора последовательности с запрещённым film grain.
	if _, err := parseAV1Config(config); !errors.Is(err, ErrCodec) {
		t.Fatalf("film grain пропущен из-за смещения: %v", err)
	}
}
