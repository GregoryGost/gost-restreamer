package codec

import (
	"encoding/hex"
	"errors"
	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"testing"
)

// Создаёт описание H.264 128×128 с фиксированными avcC, SPS и PPS для проверок срезов.
func h264Fixture(t *testing.T) VideoDescription {
	t.Helper()
	// headers содержит фиксированную запись avcC с SPS и PPS для видео 128×128. err сообщает о
	// некорректной шестнадцатеричной записи.
	headers, err := hex.DecodeString("0164000bffe100186764000bacd942046c0440000003004000000f03c50a658001000668ebe3cb22c0fdf8f800")
	if err != nil {
		t.Fatal(err)
	}
	return VideoDescription{Headers: headers, Width: 128, Height: 128}
}

// Проверяет определение IDR по срезам вопреки флагу Packet и отказ для отсутствующего или неверного
// PPS, усечённых NAL и AU без срезов.
func TestH264GateRequiresSlicePPSAndActualIDR(t *testing.T) {
	// desc содержит тестовые заголовки и размеры H.264.
	desc := h264Fixture(t)
	// format содержит формат, извлечённый из тестового SPS. err сообщает об отказе разбора формата.
	format, err := H264Format(desc)
	if err != nil {
		t.Fatal(err)
	}
	// c содержит медиаконфигурацию с фактически разобранным форматом H.264.
	c, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: 1, Revision: 1, TimeBase: media.TimeBase{Numerator: 1, Denominator: 30}, Video: format}, desc.Headers)
	if err != nil {
		t.Fatal(err)
	}
	// first_mb_in_slice=0, slice_type=I, PPS=0, frame_num=0, idr_pic_id=0.
	// tc содержит нагрузку AU и ожидаемые результаты проверки структуры и независимости.
	for _, tc := range []struct {
		// payload содержит проверяемую единицу доступа H.264 в формате AVCC.
		payload []byte
		// key задаёт ожидаемый признак независимого кадра. valid задаёт ожидаемую допустимость
		// структуры AU.
		key, valid bool
	}{{[]byte{0, 0, 0, 3, 0x65, 0xb8, 0x40}, true, true}, {[]byte{0, 0, 0, 3, 0x41, 0xb8, 0x40}, false, true}, {[]byte{0, 0, 0, 1, 0x65}, false, false}, {[]byte{0, 0, 0, 3, 0x65, 0xb4, 0x40}, false, false}, {[]byte{0, 0, 0, 1, 0xe5}, false, false}, {[]byte{0, 0, 0, 1, 0x09}, false, false}, {[]byte{0, 0, 0, 9, 0x65}, false, false}} {
		// key содержит независимость, вычисленную по срезам. err сообщает об отказе проверки AU.
		key, err := InspectH264AU(desc, tc.payload)
		if (err == nil) != tc.valid || key != tc.key {
			t.Fatalf("AU=%x key=%v err=%v", tc.payload, key, err)
		}
		// p содержит пакет с намеренно противоположным флагом Keyframe для проверки приоритета
		// сжатых данных.
		p, err := media.NewPacket(media.PacketInfo{DTS: 0, PTS: 0, DurationTicks: 1, Keyframe: !tc.key}, c, tc.payload)
		if err != nil {
			t.Fatal(err)
		}
		// inspector проверяет пакет по фактическим заголовкам и срезам.
		var inspector Inspector
		key, err = inspector.Inspect(p)
		if (err == nil) != tc.valid || key != tc.key {
			t.Fatalf("флаг sample заменил разбор AU: %v %v", key, err)
		}
	}
	// bad содержит копию конфигурации, в которой NAL PPS заменяется другим типом.
	bad := h264Fixture(t)
	bad.Headers[36] = 0x67
	// err содержит результат проверки AU с неверным типом PPS в конфигурации.
	if _, err := InspectH264AU(bad, []byte{0, 0, 0, 3, 0x65, 0xb8, 0x40}); err == nil {
		t.Fatal("PPS другого типа принят")
	}
}

// Проверяет приём корректного AAC AU, отказ для ADTS/ADIF и несовпадающего с ASC формата, а также
// ошибку совместимости пустых форматов.
func TestAACBoundariesAndActualConfig(t *testing.T) {
	// headers содержит ASC стереофонического AAC-LC 48 кГц.
	headers := []byte{0x11, 0x90}
	// format содержит формат аудио из ASC. err сообщает об отказе разбора ASC.
	format, err := AudioFormatASC(headers)
	if err != nil {
		t.Fatal(err)
	}
	// c содержит медиаконфигурацию, согласованную с фактическим ASC.
	c, _ := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackAudio, SourceEpoch: 1, Revision: 1, TimeBase: media.TimeBase{Numerator: 1, Denominator: 48000}, Audio: format}, headers)
	// inspector проверяет соответствие AAC-пакета его конфигурации.
	var inspector Inspector
	// p содержит полную AAC-единицу с длительностью 1024 отсчёта.
	p, _ := media.NewPacket(media.PacketInfo{DurationTicks: 1024}, c, []byte{0x21, 0x10, 0x04, 0x60, 0x8c, 0x1c})
	if _, err = inspector.Inspect(p); err != nil {
		t.Fatal(err)
	}
	// raw содержит очередной недопустимый AAC-вход: пустой буфер, ADTS или ADIF.
	for _, raw := range [][]byte{nil, {0xff, 0xf1}, []byte("ADIFbad")} {
		if err = ValidateAACAU(headers, raw); !errors.Is(err, ErrCodec) {
			t.Fatal(err)
		}
	}
	format.SampleRateHz = 44100
	// bad содержит конфигурацию, чья заявленная частота 44,1 кГц расходится с ASC 48 кГц.
	bad, _ := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackAudio, SourceEpoch: 1, Revision: 1, TimeBase: media.TimeBase{Numerator: 1, Denominator: 48000}, Audio: format}, headers)
	p, _ = media.NewPacket(media.PacketInfo{}, bad, []byte{1})
	if _, err = inspector.Inspect(p); !errors.Is(err, ErrCodec) {
		t.Fatal(err)
	}
	if err = Compatible(core.MediaFormat{}, core.MediaFormat{}); !errors.Is(err, ErrCodec) {
		t.Fatal(err)
	}
}

// ppsBits собирает тестовые битовые потоки PPS и других коротких заголовков.
type ppsBits struct {
	// raw содержит уже записанные байты тестового битового потока.
	raw []byte
	// count содержит число записанных битов и определяет следующую позицию записи.
	count int
}

// Дописывает n младших битов значения от старшего к младшему в тестовый битовый поток.
func (b *ppsBits) put(v uint32, n int) {
	// i задаёт позицию очередного записываемого бита значения, от старшего к младшему.
	for i := n - 1; i >= 0; i-- {
		if b.count%8 == 0 {
			b.raw = append(b.raw, 0)
		}
		b.raw[b.count/8] |= byte(v>>uint(i)&1) << uint(7-b.count%8)
		b.count++
	}
}

// Записывает беззнаковое число кодом Exp-Golomb для построения PPS.
func (b *ppsBits) ue(v uint32) {
	v++
	// width считает число значащих битов увеличенного значения для кода Exp-Golomb.
	width := 0
	// n содержит сдвигаемый остаток увеличенного значения для вычисления длины кода.
	for n := v; n > 0; n >>= 1 {
		width++
	}
	b.put(0, width-1)
	b.put(v, width)
}

// Записывает знаковое число кодом Exp-Golomb для построения PPS.
func (b *ppsBits) se(v int32) {
	if v <= 0 {
		b.ue(uint32(-2 * v))
	} else {
		b.ue(uint32(2*v - 1))
	}
}

// Завершает тестовый RBSP единичным битом и нулевым заполнением до границы байта.
func (b *ppsBits) tail() {
	b.put(1, 1)
	for b.count%8 != 0 {
		b.put(0, 1)
	}
}

// Строит PPS с заданными ссылками, квантованием и флагами расширения для проверки допустимых и
// запрещённых сочетаний.
func ppsFixture(l0, bipred, groups uint32, qp, chroma int32, extension, scaling bool) []byte {
	// b собирает PPS с переданными полями и завершающими битами.
	b := ppsBits{}
	b.ue(0)
	b.ue(0)
	b.put(0, 2)
	b.ue(groups)
	b.ue(l0)
	b.ue(0)
	b.put(0, 1)
	b.put(bipred, 2)
	b.se(qp)
	b.se(0)
	b.se(chroma)
	b.put(4, 3)
	if extension {
		b.put(1, 1)
		if scaling {
			b.put(1, 1)
		} else {
			b.put(0, 1)
		}
		b.se(0)
	}
	b.tail()
	return append([]byte{0x68}, b.raw...)
}

// Проверяет допустимые и запрещённые поля полного PPS и приём совпадающего встроенного SPS перед
// независимым срезом.
func TestH264ValidatesCompletePPSAndSameInlineParameterSets(t *testing.T) {
	// tc содержит проверяемый PPS и ожидаемую допустимость его полей.
	for _, tc := range []struct {
		// name обозначает сочетание параметров PPS в имени подтеста.
		name string
		// pps содержит полную NAL PPS, подставляемую в тестовую конфигурацию.
		pps []byte
		// valid задаёт ожидаемое принятие PPS и независимого IDR.
		valid bool
	}{
		{"ordinary", ppsFixture(0, 0, 0, 0, 0, false, false), true},
		{"signed-qp", ppsFixture(0, 0, 0, -26, -12, false, false), true},
		{"transform-extension", ppsFixture(0, 0, 0, 0, 0, true, false), true},
		{"too-many-references", ppsFixture(32, 0, 0, 0, 0, false, false), false},
		{"reserved-bipred", ppsFixture(0, 3, 0, 0, 0, false, false), false},
		{"unsupported-slice-groups", ppsFixture(0, 0, 1, 0, 0, false, false), false},
		{"invalid-qp", ppsFixture(0, 0, 0, -27, 0, false, false), false},
		{"invalid-chroma", ppsFixture(0, 0, 0, 0, 13, false, false), false},
		{"unsupported-scaling", ppsFixture(0, 0, 0, 0, 0, true, true), false},
		{"truncated-pps", []byte{0x68, 0xc0}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Подставляет выбранный PPS в конфигурацию H.264 и проверяет принятие либо отказ; для допустимого
			// случая добавляет тот же SPS непосредственно в AU.

			// desc содержит тестовую конфигурацию H.264 для подстановки выбранного PPS.
			desc := h264Fixture(t)
			// n содержит длину SPS в байтах внутри avcC.
			n := int(be.Uint16(desc.Headers[6:8]))
			// sps сохраняет отдельную копию исходного SPS для добавления непосредственно в
			// AU.
			sps := append([]byte(nil), desc.Headers[8:8+n]...)
			desc.Headers = append(desc.Headers[:8+n], 1, byte(len(tc.pps)>>8), byte(len(tc.pps)))
			desc.Headers = append(desc.Headers, tc.pps...)
			// au содержит независимый IDR и, для допустимых случаев, такой же встроенный
			// SPS.
			au := []byte{0, 0, 0, 3, 0x65, 0xb8, 0x40}
			if tc.valid {
				au = append(append([]byte{byte(len(sps) >> 24), byte(len(sps) >> 16), byte(len(sps) >> 8), byte(len(sps))}, sps...), au...)
			}
			// key показывает независимость проверяемого AU. err содержит результат проверки
			// PPS, параметров и срезов.
			key, err := InspectH264AU(desc, au)
			if (err == nil) != tc.valid || key != tc.valid {
				t.Fatalf("PPS=%x key=%v err=%v", tc.pps, key, err)
			}
		})
	}
}
