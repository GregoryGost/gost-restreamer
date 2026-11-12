package rtmp

import (
	"fmt"
	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"math"
)

// AudioFormatASC разбирает общий поддержанный формат AAC.
func AudioFormatASC(b []byte) (core.AudioFormat, error) { return codec.AudioFormatASC(b) }

// InspectAV1TU проверяет TU общим независимым от транспорта парсером.
func InspectAV1TU(c, b []byte, f core.FrameRate) (core.VideoFormat, bool, error) {
	return codec.InspectAV1TU(c, b, f)
}

// av1Description связывает состояние общего парсера AV1 с публикацией RTMP.
type av1Description struct { /* AV1Description хранит разобранную последовательность и подтверждённые между TU сведения о формате. */
	codec.AV1Description
}

// Разбирает av1C общим кодеком, сохраняя состояние последовательности для дальнейших TU публикации.
func parseAV1Config(b []byte) (av1Description, error) {
	// d содержит разобранное описание AV1. e сообщает о повреждении или неподдержанной конфигурации.
	d, e := codec.ParseAV1Config(b)
	return av1Description{d}, e
}

// Проверяет TU общим кодеком с сохранением известных размеров и частоты между пакетами.
func (d *av1Description) inspect(b []byte, f core.FrameRate) (core.VideoFormat, bool, error) {
	return d.Inspect(b, f)
}

// Передаёт разбиение OBU общему ограниченному парсеру кодека без изменения входных байтов.
func splitOBUs(b []byte) ([]codec.OBU, error) { return codec.SplitOBUs(b) }

// Вычисляет наибольший общий делитель для точной общей шкалы видео, AAC и миллисекунд RTMP.
func gcd(a, b uint64) uint64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// Представляет конечный framerate от 1 до 240 дробью со знаменателем до 1001 и погрешностью менее
// 1e-8; непредставимые значения возвращают ErrCodec.
func metadataFrameRate(v float64) (core.FrameRate, error) {
	if math.IsNaN(v) || math.IsInf(v, 0) || v < 1 || v > 240 {
		return core.FrameRate{}, ErrCodec
	}
	// den перебирает знаменатели от 1 до 1001 для представления FPS дробью с погрешностью менее
	// 1e-8.
	for den := uint32(1); den <= 1001; den++ {
		// n содержит округлённый числитель для текущего знаменателя.
		n := math.Round(v * float64(den))
		if math.Abs(n/float64(den)-v) < 1e-8 {
			return core.FrameRate{Numerator: uint32(n), Denominator: den}, nil
		}
	}
	return core.FrameRate{}, ErrCodec
}

// ingestClock сохраняет миллисекундную точность RTMP и точные длительности
// кадра из переданных сведений и AAC-LC AU из ASC. Общее начало обеих дорожек не меняется.
func ingestClock(rate core.FrameRate, audioRate uint32) (uint32, int64, int64, error) {
	if rate.Validate() != nil || audioRate == 0 {
		return 0, 0, 0, ErrCodec
	}
	// g содержит общий делитель частей FPS для сокращения дроби.
	g := gcd(uint64(rate.Numerator), uint64(rate.Denominator))
	// num содержит сокращённый числитель FPS. den содержит сокращённый знаменатель FPS.
	num, den := uint64(rate.Numerator)/g, uint64(rate.Denominator)/g
	// scale накапливает общую шкалу, точно представляющую миллисекунды RTMP, видеокадры и AAC.
	scale := uint64(1000)
	// value содержит очередную частоту, на которую должна делиться общая шкала.
	for _, value := range []uint64{num, uint64(audioRate)} {
		// factor задаёт минимальный множитель расширения текущей шкалы для выбранной частоты.
		factor := value / gcd(scale, value)
		if scale > math.MaxUint32/factor {
			return 0, 0, 0, fmt.Errorf("%w: timescale", ErrCodec)
		}
		scale *= factor
	}
	// video содержит точную длительность видеокадра в отсчётах общей шкалы.
	video := scale / num * den
	// audio содержит точную длительность 1024 аудиосэмплов в отсчётах общей шкалы.
	audio := scale / uint64(audioRate) * 1024
	if video == 0 || video > math.MaxUint32 || audio == 0 || audio > math.MaxUint32 {
		return 0, 0, 0, fmt.Errorf("%w: длительность sample", ErrCodec)
	}
	return uint32(scale), int64(video), int64(audio), nil
}
