package codec

import (
	"bytes"
	"encoding/binary"
	"errors"
	"strconv"
	"testing"
)

// hevcQuantizationPPS формирует базовый PPS по H.265 §7.3.2.3.1 со ссылкой на SPS0.
// id выбирает PPS; qp/cb/cr задают три независимых se(v) для проверки нормативных границ квантования.
func hevcQuantizationPPS(id uint32, qp, cb, cr int32) []byte {
	// bits записывает полный RBSP с отключёнными необязательными блоками и завершающими битами.
	bits := ppsBits{}
	bits.ue(id)
	bits.ue(0)
	bits.put(0, 7)
	bits.ue(0)
	bits.ue(0)
	bits.se(qp)
	bits.put(0, 3)
	bits.se(cb)
	bits.se(cr)
	bits.put(0, 10)
	bits.ue(0)
	bits.put(0, 2)
	bits.tail()
	return append([]byte{0x44, 1}, bits.raw...)
}

// hevcQuantizationConfig создаёт hvcC из буквальных VPS/SPS и переданного pps.
// t проверяет hex; depth выбирает реальный SPS Main8 либо Main10 с QpBdOffsetY=0 либо 12.
func hevcQuantizationConfig(t *testing.T, depth int, pps []byte) []byte {
	t.Helper()
	// summary/vps/sps сохраняют согласованные сводку hvcC и полные NAL реальной синтетической фикстуры.
	summary := aacHex(t, "0101600000009000000000001ef000fcfdf8f800000f03")
	vps := aacHex(t, "40010c01ffff01600000030090000003000003001eba0240")
	sps := aacHex(t, "42010101600000030090000003000003001ea020810596ea4932bc05a810100820000003002000000303c1")
	if depth == 10 {
		summary = aacHex(t, "0102200000009000000000001ef000fcfdfafa00000f03")
		vps = aacHex(t, "40010c01ffff02200000030090000003000003001eba0240")
		sps = aacHex(t, "42010102200000030090000003000003001ea01020204d96e92930bc05a848804f080000030008000003006040")
	}
	// result дописывает три неполных массива: дополнительный PPS разрешён внутри hev1 AU.
	result := bytes.Clone(summary)
	// index/nal выбирают VPS, SPS и переданный PPS с единственной NAL на массив.
	for index, nal := range [][]byte{vps, sps, pps} {
		result = append(result, byte(32+index), 0, 1, byte(len(nal)>>8), byte(len(nal)))
		result = append(result, nal...)
	}
	return result
}

// hevcParameterAU обрамляет nalus четырёхбайтовыми длинами без изменения исходного RBSP.
// Это фикстура обрамления контейнера и AU; она не вычисляет ожидаемую допустимость PPS.
func hevcParameterAU(nalus ...[]byte) []byte {
	// result накапливает NAL в заданном вызывающим порядке.
	var result []byte
	// nal выбирает очередной набор параметров или срез исходного AU.
	for _, nal := range nalus {
		result = binary.BigEndian.AppendUint32(result, uint32(len(nal)))
		result = append(result, nal...)
	}
	return result
}

// TestHEVCQuantizationPPSBounds проверяет H.265 V11 §7.4.3.3.1 и §7.4.3.2.1:
// init_qp_minus26 имеет пределы −26/−38..25 для 8/10 бит, оба смещения цветности — −12..12.
// t проверяет границы и соседние недопустимые значения через hvcC, первый AU неполной конфигурации и дополнительный PPS.
func TestHEVCQuantizationPPSBounds(t *testing.T) {
	// depth выбирает два поддержанных SPS; lower задаёт буквальный нормативный нижний предел.
	for _, depth := range []int{8, 10} {
		// lower учитывает QpBdOffsetY=12 у SPS Main10 без вызова рабочего валидатора.
		lower := int32(-26)
		if depth == 10 {
			lower = -38
		}
		// scenario задаёт отдельную границу, три PPS числа и независимый ожидаемый исход.
		for _, scenario := range []struct {
			// name различает поле и край диапазона; qp/cb/cr хранят точные se(v).
			name       string
			qp, cb, cr int32
			// valid означает нормативно допустимый PPS, а не результат текущего парсера.
			valid bool
		}{
			{"zero", 0, 0, 0, true}, {"qp-lower", lower, 0, 0, true}, {"qp-upper", 25, 0, 0, true},
			{"qp-below", lower - 1, 0, 0, false}, {"qp-above", 26, 0, 0, false},
			{"cb-lower", 0, -12, 0, true}, {"cb-upper", 0, 12, 0, true},
			{"cb-below", 0, -13, 0, false}, {"cb-above", 0, 13, 0, false},
			{"cr-lower", 0, 0, -12, true}, {"cr-upper", 0, 0, 12, true},
			{"cr-below", 0, 0, -13, false}, {"cr-above", 0, 0, 13, false},
		} {
			// Подтест через t сверяет все достижимые пути с одним нормативным ожиданием.
			t.Run(strconv.Itoa(depth)+"/"+scenario.name, func(t *testing.T) {
				// pps/headers задают проверяемый PPS0 во внешней конфигурационной записи текущей глубины.
				pps := hevcQuantizationPPS(0, scenario.qp, scenario.cb, scenario.cr)
				headers := hevcQuantizationConfig(t, depth, pps)
				// check сопоставляет err с нормативным исходом пути path: повреждённый PPS должен дать ErrCodec.
				check := func(path string, err error) {
					if scenario.valid && err != nil {
						t.Errorf("%s отверг допустимый PPS: %v", path, err)
					} else if !scenario.valid && !errors.Is(err, ErrCodec) {
						t.Errorf("%s: corrupt PPS qp=%d cb=%d cr=%d требует ErrCodec, получено %v", path, scenario.qp, scenario.cb, scenario.cr, err)
					}
				}
				// parseErr содержит первый результат разбора всего hvcC вместе со ссылкой на SPS.
				_, parseErr := ParseHEVCConfig(headers)
				check("out-of-band", parseErr)
				if depth != 8 {
					return
				}
				// baseline содержит корректные VPS/SPS/PPS0 для проверки добавленного PPS1.
				// baselineErr сообщает об отказе базовой фикстуры до проверки дополнительного PPS.
				baseline, baselineErr := ParseHEVCConfig(hevcQuantizationConfig(t, 8, hevcQuantizationPPS(0, 0, 0, 0)))
				if baselineErr != nil {
					t.Fatal("Некорректная базовая фикстура", baselineErr)
				}
				// slice содержит буквальный IDR I с PPS0 из реального 64×64/30 AU.
				slice := aacHex(t, "2801af0860f9eead5ffea9b7f2614b9762feb980")
				// inline проверяет дополнительный PPS1 перед исходной независимой картинкой.
				inline := hevcParameterAU(hevcQuantizationPPS(1, scenario.qp, scenario.cb, scenario.cr), slice)
				// inspectErr содержит отказ либо успешную проверку PPS, добавленного неполному массиву.
				_, inspectErr := baseline.Inspect(inline)
				check("additional-in-band", inspectErr)
				// incomplete оставляет только исходную сводку, fullAU передаёт все PS в первом AU.
				incomplete := bytes.Clone(headers[:23])
				incomplete[22] = 0
				fullAU := hevcParameterAU(baseline.NALUs[0], baseline.NALUs[1], pps, slice)
				// completeErr проверяет ту же синтаксическую границу при восстановлении состояния из первого AU.
				_, completeErr := CompleteHEVCConfig(incomplete, fullAU)
				check("incomplete-first-AU", completeErr)
			})
		}
	}
}
