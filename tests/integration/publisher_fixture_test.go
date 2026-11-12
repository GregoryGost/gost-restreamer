package integration

import (
	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"testing"
)

// publisherBits собирает синтетические битовые заголовки видео для структурных проверок протокола.
type publisherBits struct {
	// data хранит накопленные байты заголовка, последний из которых может быть заполнен частично.
	data []byte
	// count содержит число уже записанных битов для выбора позиции внутри последнего байта.
	count uint
}

// Добавляет n младших битов значения от старшего к младшему, продолжая заполнение текущего байта;
// изменяет накопленные data и count.
func (b *publisherBits) put(v uint64, n uint) {
	// i задаёт очередной записываемый бит среди n младших битов исходного значения.
	for i := n; i > 0; i-- {
		if b.count%8 == 0 {
			b.data = append(b.data, 0)
		}
		b.data[len(b.data)-1] |= byte(v>>(i-1)&1) << (7 - b.count%8)
		b.count++
	}
}

// Записывает uint32 в беззнаковом коде Exp-Golomb, используемом синтетическим SPS H.264.
func (b *publisherBits) ue(v uint32) {
	// n содержит значение Exp-Golomb после прибавления единицы в uint64 без переполнения uint32.
	n := uint64(v) + 1
	// w считает значимые биты n для длины нулевого префикса Exp-Golomb.
	w := uint(0)
	// x сдвигает копию n для подсчёта его битовой длины.
	for x := n; x != 0; x >>= 1 {
		w++
	}
	b.put(0, w-1)
	b.put(n, w)
}

// Заголовки записаны по синтаксису AV1 §5.5 и H.264 7.3.2.1.1/VUI.
// Срезы проверяют протокол и независимость; декодируемость устанавливает отдельная FFmpeg-проверка.
func publisherVideoHeaders(codec core.Codec, width, height, fps uint32) []byte {
	return publisherVideoHeadersAtRate(codec, width, height, core.FrameRate{Numerator: fps, Denominator: 1})
}

// Создаёт синтетические заголовки AV1 либо H.264 с заданными размерами и рациональной частотой кадров.
// Они служат структурным проверкам протокола; декодируемость устанавливают отдельные FFmpeg-сценарии.
func publisherVideoHeadersAtRate(codec core.Codec, width, height uint32, rate core.FrameRate) []byte {
	// b собирает битовое содержимое синтетического заголовка AV1 или SPS H.264.
	b := &publisherBits{}
	if codec == core.CodecAV1 {
		b.put(0, 5)
		b.put(1, 1)
		b.put(uint64(rate.Denominator), 32)
		b.put(uint64(rate.Numerator), 32)
		b.put(1, 1)
		b.put(1, 1)
		b.put(0, 1)
		b.put(0, 1)
		b.put(0, 5)
		b.put(0, 12)
		b.put(8, 5)
		b.put(0, 1)
		b.put(11, 4)
		b.put(11, 4)
		b.put(uint64(width-1), 12)
		b.put(uint64(height-1), 12)
		b.put(0, 1)
		b.put(0, 3)
		b.put(0, 5)
		b.put(0, 1)
		b.put(0, 1)
		b.put(0, 3)
		b.put(0, 1)
		b.put(0, 1)
		b.put(1, 1)
		b.put(1, 8)
		b.put(1, 8)
		b.put(1, 8)
		b.put(0, 1)
		b.put(0, 2)
		b.put(0, 1)
		b.put(0, 1)
		b.put(1, 1)
		return append([]byte{0x81, 8, 0x0c, 0, 0x0a, byte(len(b.data))}, b.data...)
	}
	b.put(100, 8)
	b.put(0, 8)
	b.put(51, 8)
	b.ue(0)
	b.ue(1)
	b.ue(0)
	b.ue(0)
	b.put(0, 1)
	b.put(0, 1)
	b.ue(0)
	b.ue(0)
	b.ue(0)
	b.ue(1)
	b.put(0, 1)
	b.ue((width+15)/16 - 1)
	b.ue((height+15)/16 - 1)
	b.put(1, 1)
	b.put(1, 1)
	if height%16 != 0 {
		b.put(1, 1)
		b.ue(0)
		b.ue(0)
		b.ue(0)
		b.ue((16 - height%16) / 2)
	} else {
		b.put(0, 1)
	}
	b.put(1, 1)
	b.put(0, 1)
	b.put(0, 1)
	b.put(1, 1)
	b.put(5, 3)
	b.put(0, 1)
	b.put(1, 1)
	b.put(1, 8)
	b.put(1, 8)
	b.put(1, 8)
	b.put(0, 1)
	b.put(1, 1)
	b.put(uint64(rate.Denominator), 32)
	b.put(uint64(2*rate.Numerator), 32)
	b.put(1, 1)
	b.put(0, 4)
	b.put(1, 1)
	for b.count%8 != 0 {
		b.put(0, 1)
	}
	// sps собирает NAL SPS H.264 с вставкой байтов предотвращения эмуляции стартового кода.
	sps := []byte{0x67}
	// zeros считает последовательные нулевые байты SPS для вставки 0x03 перед допустимым следующим
	// байтом.
	zeros := 0
	// v содержит очередной исходный байт SPS перед вставкой защитного байта.
	for _, v := range b.data {
		if zeros == 2 && v <= 3 {
			sps = append(sps, 3)
			zeros = 0
		}
		sps = append(sps, v)
		if v == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	// pps задаёт фиксированный синтетический NAL PPS, сопровождающий SPS.
	pps := []byte{0x68, 0xce, 0x3c, 0x80}
	// result собирает avcC с длинами и байтами SPS и PPS.
	result := []byte{1, 100, 0, 51, 0xff, 0xe1, byte(len(sps) >> 8), byte(len(sps))}
	result = append(result, sps...)
	result = append(result, 1, 0, byte(len(pps)))
	return append(result, pps...)
}

// Создаёт синтетические видео- и аудиоконфигурации заданной эпохи с ревизией 1. Фикстура рассчитана на
// целочисленный FPS и стереофонический AAC LC 48 kHz: видеобаза равна 1/N FPS, аудиобаза и ASC
// фиксированы.
func publisherConfigs(t testing.TB, p core.GroupProfile, epoch uint64) (media.CodecConfig, media.CodecConfig) {
	t.Helper()
	// v содержит видеоконфигурацию заданной эпохи; err содержит отказ проверки её синтетических
	// заголовков и полного формата.
	v, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: epoch, Revision: 1, TimeBase: media.TimeBase{Numerator: 1, Denominator: p.Format.Video.FrameRate.Numerator}, Video: p.Format.Video}, publisherVideoHeaders(p.Format.Video.Codec, p.Format.Video.Width, p.Format.Video.Height, p.Format.Video.FrameRate.Numerator))
	if err != nil {
		t.Fatal(err)
	}
	// a содержит конфигурацию стереофонического AAC LC с частотой 48 кГц той же эпохи и ревизии 1.
	a, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackAudio, SourceEpoch: epoch, Revision: 1, TimeBase: media.TimeBase{Numerator: 1, Denominator: 48000}, Audio: p.Format.Audio}, []byte{0x11, 0x90})
	if err != nil {
		t.Fatal(err)
	}
	return v, a
}
