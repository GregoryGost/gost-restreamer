package codec

import (
	"bytes"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
)

// skipHEVCPTL читает полный PTL одного слоя, возвращая исходные 12 байтов.
func skipHEVCPTL(r *bitReader) []byte {
	// start указывает начало выровненного по байтам general_profile_tier_level.
	start := r.p / 8
	r.read(8)
	r.read(32)
	r.read(32)
	r.read(16)
	r.read(8)
	if r.err != nil || r.p%8 != 0 {
		return nil
	}
	return r.b[start : r.p/8]
}

// validHEVCVPS проверяет полный однослойный VPS, ссылку SPS и согласованность PTL.
// Многослойность, расширения и VPS HRD вне реализованной привязки; временные параметры VUI остаётся источником FPS.
func validHEVCVPS(raw []byte, id uint8, ptl []byte) bool {
	// r читает VPS после снятия обрамления NAL и защитных байтов.
	r := bitReader{b: h264.EmulationPreventionRemove(raw[2:])}
	if r.read(4) != uint32(id) || r.read(2) != 3 || r.read(6) != 0 || r.read(3) != 0 || r.read(1) != 1 || r.read(16) != 65535 {
		return false
	}
	// vpsPTL сохраняет профиль и верхний уровень VPS; его уровень может быть выше SPS.
	vpsPTL := skipHEVCPTL(&r)
	if len(vpsPTL) != 12 || len(ptl) != 12 || !bytes.Equal(vpsPTL[:11], ptl[:11]) || vpsPTL[11] < ptl[11] {
		return false
	}
	r.read(1)
	// buffer и reorder содержат разные последовательные поля DPB, несмотря на общий способ чтения.
	buffer, reorder := r.ue(), r.ue()
	if buffer > 15 || reorder > buffer {
		return false
	}
	r.ue()
	if r.read(6) != 0 || r.ue() != 0 {
		return false
	}
	if r.read(1) != 0 {
		r.read(32)
		r.read(32)
		if r.read(1) != 0 {
			r.ue()
		}
		if r.ue() != 0 {
			return false
		}
	}
	if r.read(1) != 0 {
		return false
	}
	return r.err == nil && validRBSPTail(r)
}

// skipHEVCScaling проверяет ограниченные scaling_list_data без хранения коэффициентов.
func skipHEVCScaling(r *bitReader) {
	// size выбирает один из четырёх фиксированных размеров матриц.
	for size := 0; size < 4; size++ {
		// step задаёт число пропускаемых matrixId для матрицы 32×32.
		step := 1
		if size == 3 {
			step = 3
		}
		// matrix выбирает очередную нормативную матрицу данного размера.
		for matrix := 0; matrix < 6; matrix += step {
			if r.read(1) == 0 {
				if r.ue() > uint32(matrix) {
					r.err = ErrCodec
					return
				}
			} else {
				if size > 1 {
					r.se()
				}
				// count ограничивает число delta_coef одним блоком 4×4 либо 8×8.
				count := 16
				if size > 0 {
					count = 64
				}
				for range count {
					r.se()
				}
			}
		}
	}
}

// skipHEVCRPS проверяет ограниченные наборы кратковременных опорных картинок, сохраняя лишь число delta POC.
func skipHEVCRPS(r *bitReader, count uint32) {
	// deltaCounts хранит число дельт каждого уже разобранного набора для предсказания следующего.
	var deltaCounts [64]uint32
	if count > 64 {
		r.err = ErrCodec
		return
	}
	// index выбирает очередной набор, ссылки могут указывать только на предыдущий.
	for index := uint32(0); index < count; index++ {
		if index > 0 && r.read(1) != 0 {
			r.read(1)
			r.ue()
			// used считает выбранные дельты предыдущего набора и дополнительный deltaRps.
			used := uint32(0)
			for range deltaCounts[index-1] + 1 {
				// useDelta определяет участие дельты; второй бит существует лишь при used_by_curr_pic_flag=0.
				useDelta := r.read(1) != 0
				if !useDelta {
					useDelta = r.read(1) != 0
				}
				if useDelta {
					used++
				}
			}
			deltaCounts[index] = used
		} else {
			// negative/positive ограничивают число явных ссылок ёмкостью DPB реализованного пути.
			negative, positive := r.ue(), r.ue()
			if negative > 16 || positive > 16 || negative+positive > 16 {
				r.err = ErrCodec
				return
			}
			deltaCounts[index] = negative + positive
			for range negative + positive {
				r.ue()
				r.read(1)
			}
		}
		if r.err != nil {
			return
		}
	}
}

// skipHEVCHRD читает общие поля HRD и единственный временной подслой с ограниченным CPB.
func skipHEVCHRD(r *bitReader) {
	// nal/vcl обозначают присутствующие независимые наборы HRD; subpicture управляет DU полями.
	nal, vcl := r.read(1) != 0, r.read(1) != 0
	subpicture := false
	if nal || vcl {
		subpicture = r.read(1) != 0
		if subpicture {
			r.read(8)
			r.read(5)
			r.read(1)
			r.read(5)
		}
		r.read(4)
		r.read(4)
		if subpicture {
			r.read(4)
		}
		r.read(5)
		r.read(5)
		r.read(5)
	}
	// fixed сообщает наличие фиксированной кадровой длительности; lowDelay определяет наличие счётчика CPB.
	fixed := r.read(1) != 0
	if !fixed {
		fixed = r.read(1) != 0
	}
	// lowDelay определяет отсутствие явного счётчика CPB для низкой задержки.
	lowDelay := false
	if fixed {
		r.ue()
	} else {
		lowDelay = r.read(1) != 0
	}
	// cpbCount задаёт число записей CPB за вычетом единицы.
	cpbCount := uint32(0)
	if !lowDelay {
		cpbCount = r.ue()
	}
	if cpbCount > 31 {
		r.err = ErrCodec
		return
	}
	// present выбирает отдельный набор HRD NAL или VCL.
	for _, present := range []bool{nal, vcl} {
		if present {
			for range cpbCount + 1 {
				r.ue()
				r.ue()
				if subpicture {
					r.ue()
					r.ue()
				}
				r.read(1)
			}
		}
	}
}

// skipHEVCVUI проходит полный VUI, включая HRD и ограничения сжатого потока.
func skipHEVCVUI(r *bitReader) {
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
	r.read(3)
	if r.read(1) != 0 {
		for range 4 {
			r.ue()
		}
	}
	if r.read(1) != 0 {
		r.read(32)
		r.read(32)
		if r.read(1) != 0 {
			r.ue()
		}
		if r.read(1) != 0 {
			skipHEVCHRD(r)
		}
	}
	if r.read(1) != 0 {
		r.read(3)
		for range 5 {
			r.ue()
		}
	}
}

// validHEVCSPS проверяет полный однослойный SPS и хвост RBSP, дополняя разборщик значений.
// Дополнительные расширения SPS не подменяются частичным успешным разбором.
func validHEVCSPS(raw []byte) bool {
	// r читает исходный SPS без защитных байтов и двухбайтового заголовка NAL.
	r := bitReader{b: h264.EmulationPreventionRemove(raw[2:])}
	r.read(4)
	if r.read(3) != 0 {
		return false
	}
	r.read(1)
	skipHEVCPTL(&r)
	if r.ue() > 15 || r.ue() != 1 {
		return false
	}
	r.ue()
	r.ue()
	if r.read(1) != 0 {
		for range 4 {
			r.ue()
		}
	}
	r.ue()
	r.ue()
	// pocBits ограничивает длину POC LSB для последующего синтаксиса долговременных ссылок.
	pocBits := r.ue()
	if pocBits > 12 {
		return false
	}
	pocBits += 4
	if pocBits > 16 {
		return false
	}
	r.read(1)
	// buffer и reorder содержат разные последовательные поля DPB, несмотря на общий способ чтения.
	buffer, reorder := r.ue(), r.ue()
	if buffer > 15 || reorder > buffer {
		return false
	}
	r.ue()
	for range 6 {
		r.ue()
	}
	// scalingEnabled управляет наличием отдельного sps_scaling_list_data_present_flag.
	scalingEnabled := r.read(1) != 0
	if scalingEnabled && r.read(1) != 0 {
		skipHEVCScaling(&r)
	}
	r.read(2)
	if r.read(1) != 0 {
		r.read(8)
		r.ue()
		r.ue()
		r.read(1)
	}
	// count ограничивает кратковременные наборы перед разбором ссылок.
	count := r.ue()
	skipHEVCRPS(&r, count)
	if r.read(1) != 0 {
		// longCount задаёт число долговременных записей с фиксированной длиной POC.
		longCount := r.ue()
		if longCount > 32 {
			return false
		}
		for range longCount {
			r.read(int(pocBits))
			r.read(1)
		}
	}
	r.read(2)
	if r.read(1) != 0 {
		skipHEVCVUI(&r)
	}
	if r.read(1) != 0 {
		return false
	}
	return r.err == nil && validRBSPTail(r)
}

// validHEVCPPS проверяет полный базовый PPS raw, границы квантования, плиток и хвост RBSP.
// bitDepthLumaMinus8 задаёт фактическую глубину SPS для нижней границы init_qp_minus26.
// При nil проверяется лишь общий предел −74 для нормативных глубин 8–16 бит;
// точная проверка повторяется после получения SPS. Расширения вне Main/Main10 не поддержаны.
func validHEVCPPS(raw []byte, bitDepthLumaMinus8 *uint32) bool {
	// r читает полный PPS после удаления заголовка NAL и защитных байтов.
	r := bitReader{b: h264.EmulationPreventionRemove(raw[2:])}
	if r.ue() > 63 || r.ue() > 15 {
		return false
	}
	r.read(7)
	// refsL0 и refsL1 ограничивают разные списки активных опорных картинок PPS.
	refsL0, refsL1 := r.ue(), r.ue()
	if refsL0 > 14 || refsL1 > 14 {
		return false
	}
	// minQP задаёт нижнюю границу init_qp_minus26: −(26+QpBdOffsetY), где QpBdOffsetY=6*bit_depth_luma_minus8.
	// Без SPS значение −74 исключает только PPS, недопустимые при любой нормативной глубине.
	minQP := int64(-74)
	if bitDepthLumaMinus8 != nil {
		minQP = -26 - 6*int64(*bitDepthLumaMinus8)
	}
	// initQP содержит знаковое смещение начального QP среза, верхняя граница которого не зависит от SPS.
	initQP := r.se()
	if initQP < minQP || initQP > 25 {
		return false
	}
	r.read(2)
	if r.read(1) != 0 {
		r.ue()
	}
	// cbQP и crQP задают независимые смещения QP цветоразностных плоскостей в пределах −12..12.
	cbQP, crQP := r.se(), r.se()
	if cbQP < -12 || cbQP > 12 || crQP < -12 || crQP > 12 {
		return false
	}
	r.read(4)
	// tiles включает таблицы плиток; entropySync обозначает синхронизацию контекстов кодирования строк CTU.
	tiles := r.read(1) != 0
	entropySync := r.read(1) != 0
	_ = entropySync
	if tiles {
		// columns и rows содержат число столбцов и строк плиток за вычетом единицы, ограниченное конечным портом.
		columns, rows := r.ue(), r.ue()
		if columns > 19 || rows > 21 {
			return false
		}
		if r.read(1) == 0 {
			for range columns {
				r.ue()
			}
			for range rows {
				r.ue()
			}
		}
		r.read(1)
	}
	r.read(1)
	if r.read(1) != 0 {
		r.read(1)
		if r.read(1) == 0 {
			r.se()
			r.se()
		}
	}
	if r.read(1) != 0 {
		skipHEVCScaling(&r)
	}
	r.read(1)
	r.ue()
	r.read(1)
	if r.read(1) != 0 {
		return false
	}
	return r.err == nil && validRBSPTail(r)
}
