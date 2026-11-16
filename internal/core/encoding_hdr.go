package core

import (
	"math"
	"slices"
)

// validateDesiredStrings проверяет конечные каталоги желаемых настроек p и их зависимости.
// HDR допускает только известные BT.2020/PQ либо HLG, 10 бит и известный диапазон.
// HEVC/VP9 SDR ограничены BT.709; известный наблюдаемый профиль вне матрицы не становится настройкой.
func (p GroupProfile) validateDesiredStrings() error {
	// v и a содержат проверяемые дорожки до проверки кодировщика.
	v, a := p.Format.Video, p.Format.Audio
	// field связывает публичный путь ошибки с независимым каталогом и исходным значением.
	for _, field := range []struct {
		// path задаёт поле ошибки; catalog выбирает каталог; value не нормализуется.
		path, catalog, value string
	}{
		{"mode", "mode", string(p.Mode)},
		{"format.video.codec", "video.codec", string(v.Codec)},
		{"format.video.codec_profile", "video.codec_profile", v.CodecProfile},
		{"format.video.pixel_format", "video.pixel_format", v.PixelFormat},
		{"format.video.color.range", "video.color.range", v.Color.Range},
		{"format.video.color.primaries", "video.color.primaries", v.Color.Primaries},
		{"format.video.color.transfer", "video.color.transfer", v.Color.Transfer},
		{"format.video.color.matrix", "video.color.matrix", v.Color.Matrix},
		{"format.audio.codec", "audio.codec", string(a.Codec)},
		{"format.audio.codec_profile", "audio.codec_profile", a.CodecProfile},
		{"format.audio.channel_layout", "audio.channel_layout", a.ChannelLayout},
	} {
		if !slices.Contains(DesiredStringValues(field.catalog), field.value) {
			return invalidEncoding(field.path, "Значение отсутствует в поддерживаемом наборе настроек.")
		}
	}
	if slices.Contains([]string{"yuv420p10le", "yuv422p10le", "yuv444p10le"}, v.PixelFormat) {
		if v.BitDepth != 10 {
			return invalidEncoding("format.video.bit_depth", "Выбранный формат пикселей требует глубину 10 бит.")
		}
	} else if v.BitDepth != 8 {
		return invalidEncoding("format.video.bit_depth", "Выбранный формат пикселей требует глубину 8 бит.")
	}
	if v.Color.Transfer == "smpte2084" || v.Color.Transfer == "arib-std-b67" {
		if v.BitDepth != 10 || v.Color.Primaries != "bt2020" || v.Color.Matrix != "bt2020nc" {
			return invalidEncoding("format.video.color", "HDR требует глубину 10 бит, primaries bt2020 и matrix bt2020nc.")
		}
	} else if v.Codec == CodecHEVC || v.Codec == CodecVP9 {
		if v.Color.Primaries != "bt709" || v.Color.Transfer != "bt709" || v.Color.Matrix != "bt709" {
			return invalidEncoding("format.video.color", "SDR HEVC/VP9 требуют primaries, transfer и matrix bt709.")
		}
	} else if v.Color.Matrix == "bt2020c" && (v.PixelFormat == "gray" || !slices.Contains([]string{"bt709", "smpte170m", "bt2020-10", "bt2020-12"}, v.Color.Transfer)) {
		return invalidEncoding("format.video.color", "Матрица bt2020c требует цветное изображение и совместимую передаточную характеристику.")
	}
	switch v.Codec {
	case CodecAV1:
		if p.Mode != ModeCopy || !validAV1Level(v.Level) || !slices.Contains([]string{"", "main", "high"}, v.Tier) {
			return invalidEncoding("format.video", "AV1 требует режим copy, известный уровень и категорию main/high либо пустую категорию.")
		}
	case CodecH264:
		if p.Mode != ModeTranscode || v.Tier != "" || !slices.Contains(DesiredStringValues("video.level.h264"), v.Level) {
			return invalidEncoding("format.video", "H.264 требует transcode, известный уровень и пустую категорию.")
		}
	case CodecHEVC:
		if !slices.Contains([]string{"", "auto"}, v.Level) || !slices.Contains([]string{"", "main"}, v.Tier) {
			return invalidEncoding("format.video", "HEVC допускает только пустой/auto уровень и пустую/main категорию.")
		}
	case CodecVP9:
		if !slices.Contains([]string{"", "auto"}, v.Level) || v.Tier != "" {
			return invalidEncoding("format.video", "VP9 допускает только пустой/auto уровень и пустую категорию.")
		}
	}
	return nil
}

// validateHEVCVP9Encoding проверяет HEVC/VP9 в p после общей проверки дорожек и GOP.
// Привязки кодировщиков, качества и нативные пределы не зависят от аудиокодека.
func (p GroupProfile) validateHEVCVP9Encoding() error {
	// e содержит выбранный кодировщик, v — видеодорожку, ve — битрейты и GOP.
	e, v, ve := p.Encoding, p.Format.Video, p.VideoEncoding
	if ve.MaxBFrames != 0 {
		return invalidEncoding("video_encoding.max_b_frames", "Фиксированные HEVC/VP9-рецепты требуют нулевое число B-кадров.")
	}
	if e.NVENC != (NVENCSettings{}) || e.SVTAV1 != (SVTAV1Settings{}) {
		return invalidEncoding("encoding", "HEVC/VP9 используют фиксированный рецепт без блоков nvenc и svt_av1.")
	}
	if e.VideoCustom != "" {
		return invalidEncoding("encoding.video_custom", "Выбранный кодировщик использует фиксированный рецепт без video_custom.")
	}
	// validTuple отмечает единственную применимую пару профиля и пикселей выбранного кодека.
	validTuple := false
	switch v.Codec {
	case CodecHEVC:
		validTuple = (e.VideoEncoder == VideoEncoderX265 || e.VideoEncoder == VideoEncoderHEVCNVENC) &&
			(v.CodecProfile == "main" && v.PixelFormat == "yuv420p" || v.CodecProfile == "main10" && v.PixelFormat == "yuv420p10le")
	case CodecVP9:
		validTuple = e.VideoEncoder == VideoEncoderVP9 &&
			(v.CodecProfile == "profile0" && v.PixelFormat == "yuv420p" || v.CodecProfile == "profile2" && v.PixelFormat == "yuv420p10le")
	}
	if !validTuple {
		return invalidEncoding("format.video", "Сочетание кодировщика, видеокодека, профиля и формата пикселей не поддерживается.")
	}
	// err сообщает отказ обязательности и диапазона качества до проверки нативных битрейтов.
	if err := p.validateHEVCVP9Quality(); err != nil {
		return err
	}
	if e.RateControl == RateControlCBR || e.RateControl == RateControlVBR {
		// err сообщает отказ точной нативной представимости битрейта или буфера.
		if err := p.validateHEVCVP9Bitrate(); err != nil {
			return err
		}
	}

	return nil
}

// validateHEVCVP9Quality проверяет присутствие, диапазон и шаг качества нового кодировщика p.
// Нулевой QP/CRF не обещает сжатия без потерь; чистое качество требует полностью нулевой VBV.
func (p GroupProfile) validateHEVCVP9Quality() error {
	// e содержит выбранный кодировщик и явное качество; ve содержит проверяемые поля битрейта.
	e, ve := p.Encoding, p.VideoEncoding
	if !e.QP.Set && e.QP.Value != 0 || !e.CRF.Set && e.CRF.ValueHundredths != 0 {
		return invalidEncoding("encoding", "Отсутствующее качество должно содержать ноль.")
	}
	switch e.RateControl {
	case RateControlCBR, RateControlVBR:
		if e.QP.Set || e.CRF.Set {
			return invalidEncoding("encoding", "CBR/VBR не допускают qp и crf.")
		}
		return nil
	case RateControlCQP:
		if e.VideoEncoder == VideoEncoderVP9 || !e.QP.Set || e.CRF.Set || e.QP.Value < 0 || e.QP.Value > 51 {
			return invalidEncoding("encoding.qp", "CQP доступен HEVC и требует единственный целый qp от 0 до 51.")
		}
	case RateControlCRF:
		if e.VideoEncoder == VideoEncoderHEVCNVENC || !e.CRF.Set || e.QP.Set || e.CRF.ValueHundredths < 0 ||
			e.VideoEncoder == VideoEncoderX265 && e.CRF.ValueHundredths > 5100 ||
			e.VideoEncoder == VideoEncoderVP9 && (e.CRF.ValueHundredths > 6300 || e.CRF.ValueHundredths%100 != 0) {
			return invalidEncoding("encoding.crf", "CRF доступен x265 с шагом 0.01 в диапазоне 0..51 и VP9 с целым качеством 0..63.")
		}
	default:
		return invalidEncoding("encoding.rate_control", "Требуется поддержанный режим cbr, vbr, cqp или crf.")
	}
	if ve.TargetBitrateBPS != 0 || ve.MaxBitrateBPS != 0 || ve.BufferBits != 0 {
		return invalidEncoding("video_encoding", "Чистые CQP/CRF требуют нулевые target, max и buffer.")
	}
	return nil
}

// validateHEVCVP9Bitrate проверяет битрейтные режимы p на точную представимость нативными полями.
// Произведения VP9 ограничены до умножения, поэтому проверка миллисекунд не переполняется.
func (p GroupProfile) validateHEVCVP9Bitrate() error {
	// e задаёт кодировщик и RC; ve содержит величины в битах и битах в секунду.
	e, ve := p.Encoding, p.VideoEncoding
	if e.VideoEncoder == VideoEncoderVP9 {
		if ve.TargetBitrateBPS == 0 || ve.TargetBitrateBPS > 1000000000 || ve.TargetBitrateBPS%1000 != 0 {
			return invalidEncoding("video_encoding.target_bitrate_bps", "VP9 требует target от 1000 до 1000000000 бит/с, кратный 1000.")
		}
		if e.RateControl == RateControlVBR {
			if ve.MaxBitrateBPS != 0 || ve.BufferBits != 0 {
				return invalidEncoding("video_encoding", "VP9 VBR задаёт только target; max и buffer должны быть нулевыми.")
			}
		} else if ve.MaxBitrateBPS != ve.TargetBitrateBPS || ve.BufferBits == 0 || ve.BufferBits > math.MaxInt32 || ve.BufferBits*1000%ve.TargetBitrateBPS != 0 || ve.BufferBits*1000/ve.TargetBitrateBPS == 0 || ve.BufferBits*1000/ve.TargetBitrateBPS > math.MaxUint32 {
			return invalidEncoding("video_encoding", "VP9 CBR требует max=target и буфер до 2147483647 бит, точно представимый положительными uint32 миллисекундами.")
		}
		return nil
	}
	if ve.TargetBitrateBPS == 0 || ve.TargetBitrateBPS > ve.MaxBitrateBPS || ve.MaxBitrateBPS > math.MaxUint32 || ve.BufferBits == 0 || ve.BufferBits > math.MaxInt32 {
		return invalidEncoding("video_encoding", "HEVC требует 1 ≤ target ≤ max ≤ 4294967295 и буфер 1..2147483647 бит.")
	}
	if e.VideoEncoder == VideoEncoderX265 && (ve.TargetBitrateBPS%1000 != 0 || ve.MaxBitrateBPS%1000 != 0 || ve.BufferBits%1000 != 0) {
		return invalidEncoding("video_encoding", "Битрейты и буфер x265 должны точно представляться целыми кбит/с и кбит.")
	}
	if e.RateControl == RateControlCBR && ve.TargetBitrateBPS != ve.MaxBitrateBPS {
		return invalidEncoding("video_encoding.max_bitrate_bps", "CBR требует max=target.")
	}
	return nil
}
