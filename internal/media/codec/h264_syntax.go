package codec

import "github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"

// validSPSRaw проверяет полный синтаксис SPS raw и завершающие биты RBSP.
// Внешний разборщик извлекает значения, но не проверяет конец SPS; этот проход без выделения
// таблиц не позволяет принять усечённый хвост или произвольные лишние байты.
func validSPSRaw(raw []byte) bool {
	// r читает SPS после заголовка NAL и удаления защитных байтов.
	r := bitReader{b: h264.EmulationPreventionRemove(raw[1:])}
	// profile задаёт ветвь синтаксиса с расширенными параметрами цветовых плоскостей.
	profile := r.read(8)
	r.read(16)
	r.ue()
	switch profile {
	case 100, 110, 122, 244, 44, 83, 86, 118, 128, 138, 139, 134, 135:
		// chroma определяет наличие отдельного флага плоскостей и число матриц масштабирования.
		chroma := r.ue()
		if chroma > 3 {
			return false
		}
		if chroma == 3 {
			r.read(1)
		}
		r.ue()
		r.ue()
		r.read(1)
		if r.read(1) != 0 {
			// count ограничивает число матриц масштабирования фиксированным синтаксисом SPS.
			count := 8
			if chroma == 3 {
				count = 12
			}
			// index выбирает матрицу 4×4 либо 8×8.
			for index := range count {
				if r.read(1) != 0 {
					// size задаёт фиксированное число коэффициентов этой матрицы.
					size := 16
					if index >= 6 {
						size = 64
					}
					if !skipScalingList(&r, size) {
						return false
					}
				}
			}
		}
	case 66, 77, 88:
	default:
		return false
	}
	if r.ue() > 12 {
		return false
	}
	// order выбирает синтаксис счётчика порядка картинок.
	order := r.ue()
	switch order {
	case 0:
		if r.ue() > 12 {
			return false
		}
	case 1:
		r.read(1)
		r.se()
		r.se()
		// count ограничивает число смещений в цикле порядка картинок.
		count := r.ue()
		if count > 255 {
			return false
		}
		for range count {
			r.se()
		}
	case 2:
	default:
		return false
	}
	if r.ue() > 16 {
		return false
	}
	r.read(1)
	r.ue()
	r.ue()
	if r.read(1) == 0 {
		r.read(1)
	}
	r.read(1)
	if r.read(1) != 0 {
		for range 4 {
			r.ue()
		}
	}
	if r.read(1) != 0 {
		skipH264VUI(&r)
	}
	return r.err == nil && validRBSPTail(r)
}

// skipScalingList проверяет size коэффициентов матрицы без сохранения значений.
// После nextScale=0 новые delta_scale не читаются, как требует H.264 §7.3.2.1.1.1.
func skipScalingList(r *bitReader, size int) bool {
	// last содержит последний ненулевой масштаб; next определяет наличие следующего delta_scale.
	last, next := int64(8), int64(8)
	for range size {
		if next != 0 {
			// delta задаёт знаковое изменение масштаба в нормативном диапазоне.
			delta := r.se()
			if delta < -128 || delta > 127 {
				return false
			}
			next = (last + delta + 256) % 256
		}
		if next != 0 {
			last = next
		}
	}
	return r.err == nil
}

// skipH264VUI проходит все необязательные поля VUI читателем r до конца bitstream_restriction.
// Сведения извлекаются основным разборщиком SPS; здесь важны границы каждого присутствующего поля.
func skipH264VUI(r *bitReader) {
	if r.read(1) != 0 && r.read(8) == 255 {
		r.read(32)
	}
	if r.read(1) != 0 {
		r.read(1)
	}
	if r.read(1) != 0 {
		r.read(4)
		if r.read(1) != 0 {
			r.read(24)
		}
	}
	if r.read(1) != 0 {
		r.ue()
		r.ue()
	}
	if r.read(1) != 0 {
		r.read(32)
		r.read(32)
		r.read(1)
	}
	// nal и vcl показывают наличие двух независимых наборов параметров HRD.
	nal := r.read(1) != 0
	if nal {
		skipH264HRD(r)
	}
	vcl := r.read(1) != 0
	if vcl {
		skipH264HRD(r)
	}
	if nal || vcl {
		r.read(1)
	}
	r.read(1)
	if r.read(1) != 0 {
		r.read(1)
		for range 6 {
			r.ue()
		}
	}
}

// skipH264HRD проверяет ограниченный список CPB и длины временных полей HRD в r.
func skipH264HRD(r *bitReader) {
	// count задаёт число CPB за вычетом единицы и не может превышать 31.
	count := r.ue()
	if count > 31 {
		r.err = ErrCodec
		return
	}
	r.read(8)
	for range count + 1 {
		r.ue()
		r.ue()
		r.read(1)
	}
	r.read(20)
}
