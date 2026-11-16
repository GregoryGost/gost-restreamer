package core

import (
	"math"
	"slices"
)

// validateAV1H264Encoding проверяет полный рецепт p без назначения начальных значений.
// Проверки качества, нативной представимости и точного профиля выполняются до запуска кодировщика.
func (p GroupProfile) validateAV1H264Encoding() error {
	// e задаёт выбранный видеокодировщик и его дополнительные параметры.
	e := p.Encoding
	// err сохраняет отказ точного сопоставления выбранного кодировщика, режима и пикселей.
	if err := p.validateAV1H264Tuple(); err != nil {
		return err
	}
	// err сохраняет нарушение присутствия качества или запрета скрытого VBV.
	if err := p.validateAV1H264Quality(); err != nil {
		return err
	}
	switch e.VideoEncoder {
	case VideoEncoderNVENC, VideoEncoderX264:
		// err сообщает отказ ограничений H.264, включая явные новые уровни.
		if err := p.validateH264Encoding(); err != nil {
			return err
		}
	case VideoEncoderSVTAV1:
		// err сообщает отказ ограничений SVT, включая категорию high.
		if err := p.validateSVTEncoding(); err != nil {
			return err
		}
	case VideoEncoderAOMAV1:
		// err сообщает отказ представимости управления битрейтом AOM.
		if err := p.validateAOMEncoding(); err != nil {
			return err
		}
	}
	// normalized содержит каноническую строку видео, err сообщает об ошибке её разбора.
	normalized, err := NormalizeCustom(e.VideoEncoder, e.VideoCustom)
	if err != nil {
		return err
	}
	if normalized != e.VideoCustom {
		return invalidEncoding("encoding.video_custom", "Параметры должны храниться в нормализованном виде.")
	}
	return nil
}

// validateAV1H264Tuple принимает только проверенные сочетания кодировщика, режима, профиля и пикселей p.
// Совпадение с более высоким профилем не считается разрешением использовать его подмножество.
func (p GroupProfile) validateAV1H264Tuple() error {
	// e и v задают выбранный кодировщик и желаемое видео; pixels накапливает допустимые точные пиксели.
	e, v := p.Encoding, p.Format.Video
	var pixels []string
	switch e.VideoEncoder {
	case VideoEncoderSVTAV1:
		if p.Mode == ModeCopy && v.Codec == CodecAV1 && v.CodecProfile == "main" {
			pixels = []string{"yuv420p", "yuv420p10le"}
		}
	case VideoEncoderAOMAV1:
		if p.Mode == ModeCopy && v.Codec == CodecAV1 {
			switch v.CodecProfile {
			case "main":
				pixels = []string{"yuv420p", "yuv420p10le", "gray"}
			case "high":
				pixels = []string{"yuv444p", "yuv444p10le"}
			case "professional":
				pixels = []string{"yuv422p", "yuv422p10le"}
			}
		}
	case VideoEncoderX264:
		if p.Mode == ModeTranscode && v.Codec == CodecH264 {
			switch v.CodecProfile {
			case "constrained-baseline", "main", "high":
				pixels = []string{"yuv420p"}
			case "high10":
				pixels = []string{"yuv420p10le"}
			case "high422":
				pixels = []string{"yuv422p", "yuv422p10le"}
			case "high444":
				pixels = []string{"yuv444p", "yuv444p10le"}
			}
		}
	case VideoEncoderNVENC:
		if p.Mode == ModeTranscode && v.Codec == CodecH264 {
			switch v.CodecProfile {
			case "baseline", "main", "high":
				pixels = []string{"yuv420p"}
			case "high444":
				pixels = []string{"yuv444p"}
			}
		}
	default:
		return invalidEncoding("encoding.video_encoder", "Кодировщик неприменим к AV1/H.264.")
	}
	if !slices.Contains(pixels, v.PixelFormat) {
		return invalidEncoding("format.video", "Сочетание кодировщика, видеорежима, профиля и формата пикселей не поддерживается.")
	}
	if e.VideoEncoder != VideoEncoderNVENC && e.NVENC != (NVENCSettings{}) || e.VideoEncoder != VideoEncoderSVTAV1 && e.SVTAV1 != (SVTAV1Settings{}) {
		return invalidEncoding("encoding", "Неприменимые блоки кодировщиков должны быть полностью нулевыми.")
	}
	if (e.VideoEncoder == VideoEncoderX264 || e.VideoEncoder == VideoEncoderAOMAV1) && e.VideoCustom != "" {
		return invalidEncoding("encoding.video_custom", "Выбранный программный кодировщик использует фиксированный рецепт без video_custom.")
	}
	return nil
}

// validateAV1H264Quality проверяет присутствие и диапазоны QP/CRF p в точных сотых долях.
// Чистое качество требует нулевых битрейтов и буфера, поэтому скрытый VBV невозможен.
func (p GroupProfile) validateAV1H264Quality() error {
	// e содержит настройки качества; ve хранит битрейты и буфер; maximum и minimum задают шкалу кодировщика.
	e, ve := p.Encoding, p.VideoEncoding
	maximum, minimum := 51, 0
	if e.VideoEncoder == VideoEncoderSVTAV1 || e.VideoEncoder == VideoEncoderAOMAV1 {
		maximum = 63
	}
	if e.VideoEncoder == VideoEncoderSVTAV1 {
		minimum = 1
	}
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
		if e.VideoEncoder == VideoEncoderAOMAV1 || !e.QP.Set || e.CRF.Set || e.QP.Value < minimum || e.QP.Value > maximum {
			return invalidEncoding("encoding.qp", "CQP требует единственный целый qp из диапазона выбранного кодировщика.")
		}
	case RateControlCRF:
		if e.VideoEncoder == VideoEncoderNVENC || !e.CRF.Set || e.QP.Set || e.CRF.ValueHundredths < minimum*100 || e.CRF.ValueHundredths > maximum*100 || e.VideoEncoder != VideoEncoderX264 && e.CRF.ValueHundredths%100 != 0 {
			return invalidEncoding("encoding.crf", "CRF требует единственное качество из диапазона и с шагом выбранного кодировщика.")
		}
	default:
		return invalidEncoding("encoding.rate_control", "Требуется поддержанный режим cbr, vbr, cqp или crf.")
	}
	if ve.TargetBitrateBPS != 0 || ve.MaxBitrateBPS != 0 || ve.BufferBits != 0 {
		return invalidEncoding("video_encoding", "Чистые CQP/CRF требуют нулевые target, max и buffer.")
	}
	if e.VideoEncoder == VideoEncoderX264 && (e.QP.Set && e.QP.Value == 0 || e.CRF.Set && e.CRF.ValueHundredths == 0) && p.Format.Video.CodecProfile != "high444" {
		return invalidEncoding("format.video.codec_profile", "Нулевое качество x264 требует явно выбранный high444.")
	}
	return nil
}

// validateH264Encoding проверяет GOP, пределы нативного управления битрейтом, профиль и параметры NVENC в p.
// Чистые режимы качества не задают явный level; уровни 6 проверяются отдельно.
func (p GroupProfile) validateH264Encoding() error {
	// e выбирает кодировщик, v задаёт формат, ve содержит ограничения GOP и битрейта.
	e, v, ve := p.Encoding, p.Format.Video, p.VideoEncoding
	if ve.MaxBFrames > 4 || ve.MaxBFrames >= ve.GOPFrames || (v.CodecProfile == "baseline" || v.CodecProfile == "constrained-baseline") && ve.MaxBFrames != 0 {
		return invalidEncoding("video_encoding.max_b_frames", "Допустимы 0..4 B-кадра меньше GOP; baseline и constrained-baseline требуют 0.")
	}
	if e.RateControl == RateControlCQP || e.RateControl == RateControlCRF {
		if v.Level != "" && v.Level != "auto" {
			return invalidEncoding("format.video.level", "Чистое качество H.264 допускает только пустой уровень либо auto.")
		}
	} else {
		if ve.TargetBitrateBPS == 0 || ve.TargetBitrateBPS > ve.MaxBitrateBPS || ve.MaxBitrateBPS > math.MaxUint32 || ve.BufferBits == 0 || ve.BufferBits > math.MaxInt32 {
			return invalidEncoding("video_encoding", "H.264 требует 1 ≤ target ≤ max ≤ 4294967295 и буфер 1..2147483647 бит.")
		}
		if e.VideoEncoder == VideoEncoderX264 && (ve.TargetBitrateBPS%1000 != 0 || ve.MaxBitrateBPS%1000 != 0 || ve.BufferBits%1000 != 0) {
			return invalidEncoding("video_encoding", "Битрейты и буфер x264 должны точно представляться целыми кбит/с и кбит.")
		}
		if e.RateControl == RateControlCBR && ve.TargetBitrateBPS != ve.MaxBitrateBPS {
			return invalidEncoding("video_encoding.max_bitrate_bps", "CBR требует max=target.")
		}
	}
	if e.VideoEncoder == VideoEncoderNVENC {
		// n задаёт фактические запрошенные параметры NVENC без утверждения свойств предустановки SDK.
		n := e.NVENC
		if e.RateControl == RateControlCQP {
			if n != (NVENCSettings{Preset: "p4", Tuning: n.Tuning, Multipass: "disabled", AQStrength: 8}) || n.Tuning != "hq" && n.Tuning != "lossless" || e.VideoCustom != "" {
				return invalidEncoding("encoding.nvenc", "CQP требует p4, hq/lossless, отключённые multipass/lookahead/AQ и пустой video_custom.")
			}
			if n.Tuning == "lossless" && (v.CodecProfile != "high444" || v.PixelFormat != "yuv444p" || e.QP.Value != 0 || ve.MaxBFrames != 0) {
				return invalidEncoding("encoding.nvenc.tuning", "Lossless требует high444/yuv444p8, CQP0 и отсутствие B-кадров.")
			}
		} else {
			if !slices.Contains([]string{"p1", "p2", "p3", "p4", "p5", "p6", "p7"}, n.Preset) || !slices.Contains([]string{"hq", "ll", "ull"}, n.Tuning) || !slices.Contains([]string{"disabled", "qres", "fullres"}, n.Multipass) {
				return invalidEncoding("encoding.nvenc", "CBR/VBR требуют поддержанные preset, tuning и multipass NVENC.")
			}
			if n.LookaheadFrames < 0 || n.LookaheadFrames > 31 || n.LookaheadFrames > 0 && uint64(n.LookaheadFrames)+uint64(ve.MaxBFrames) > 31 {
				return invalidEncoding("encoding.nvenc.lookahead_frames", "Допустимы 0..31; положительный lookahead вместе с B-кадрами не должен превышать 31.")
			}
			if n.AQStrength < 1 || n.AQStrength > 15 || !n.EnableSpatialAQ && n.AQStrength != 8 {
				return invalidEncoding("encoding.nvenc.aq_strength", "Допустимы 1..15; без пространственного AQ требуется 8.")
			}
		}
	}
	return p.validateH264Level()
}

// validateH264Level проверяет заявленные уровни 6.0/6.1/6.2 по таблицам H.264 A-1/A-2.
// Фиксированный рецепт использует один опорный кадр и B0; проверка не обещает полный контроль потока.
func (p GroupProfile) validateH264Level() error {
	// v и ve содержат формат и ограниченное управление битрейтом; mbps и rate выбирают пределы уровня в макроблоках в секунду и единицах таблицы.
	v, ve := p.Format.Video, p.VideoEncoding
	var mbps, rate uint64
	switch v.Level {
	case "6.0":
		mbps, rate = 4177920, 240000
	case "6.1":
		mbps, rate = 8355840, 480000
	case "6.2":
		mbps, rate = 16711680, 800000
	default:
		return nil
	}
	if p.Encoding.RateControl != RateControlCBR && p.Encoding.RateControl != RateControlVBR || ve.MaxBFrames != 0 {
		return invalidEncoding("format.video.level", "Уровни H.264 6 требуют CBR/VBR, B0 и фиксированный один опорный кадр.")
	}
	// widthMB и heightMB округляют размеры к макроблокам; sizeMB задаёт размер кадра в MB.
	widthMB, heightMB := (uint64(v.Width)+15)/16, (uint64(v.Height)+15)/16
	sizeMB := widthMB * heightMB
	if sizeMB > 139264 || widthMB*widthMB > 8*139264 || heightMB*heightMB > 8*139264 || sizeMB*uint64(v.FrameRate.Numerator) > mbps*uint64(v.FrameRate.Denominator) || sizeMB > 696320 {
		return invalidEncoding("format.video", "Размер, частота или буфер одного опорного кадра превышают пределы выбранного уровня H.264 6.")
	}
	// factor переводит VCL-пределы битрейта и CPB в биты с учётом точного профиля.
	factor := uint64(1000)
	switch v.CodecProfile {
	case "high":
		factor = 1250
	case "high10":
		factor = 3000
	case "high422", "high444":
		factor = 4000
	}
	if ve.MaxBitrateBPS > rate*factor || ve.BufferBits > rate*factor {
		return invalidEncoding("video_encoding", "Максимальный битрейт или буфер превышают VCL-предел выбранных уровня и профиля H.264.")
	}
	return nil
}

// validateSVTEncoding проверяет рецепт SVT с точными 8/10 битами и категорией main/high.
// p уже прошёл проверку качества и точной пары профиля/пикселей.
func (p GroupProfile) validateSVTEncoding() error {
	// v задаёт геометрию, e — рецепт, ve — битрейты/GOP, s — настройки нативного SVT.
	v, e, ve, s := p.Format.Video, p.Encoding, p.VideoEncoding, p.Encoding.SVTAV1
	if v.Width < 64 || v.Width > 16384 || v.Height < 64 || v.Height > 8704 || uint64(v.FrameRate.Numerator) > 240*uint64(v.FrameRate.Denominator) {
		return invalidEncoding("format.video", "SVT-AV1 требует ширину 64..16384, высоту 64..8704 и FPS не выше 240.")
	}
	if ve.GOPFrames < 2 || ve.MaxBFrames != 0 {
		return invalidEncoding("video_encoding.gop_frames", "SVT-AV1 требует GOP не меньше 2 и нулевое число B-кадров.")
	}
	if s.Preset < 0 || s.Preset > 13 || !slices.Contains([]string{"psnr", "vq"}, s.Tuning) && (e.RateControl != RateControlCRF || !slices.Contains([]string{"ssim", "ms_ssim", "vmaf"}, s.Tuning)) || (e.RateControl == RateControlCBR || e.RateControl == RateControlCQP) && s.Tuning != "psnr" {
		return invalidEncoding("encoding.svt_av1", "Требуются preset 0..13 и совместимый tuning: CBR/CQP psnr, VBR psnr/vq, CRF также ssim/ms_ssim/vmaf.")
	}
	if e.RateControl == RateControlCQP && s.AQMode != 0 || e.RateControl != RateControlCQP && s.AQMode != 2 || e.RateControl == RateControlCBR && s.LookaheadFrames != 0 || e.RateControl != RateControlCBR && s.LookaheadFrames != -1 && (s.LookaheadFrames < 6 || s.LookaheadFrames > 120) {
		return invalidEncoding("encoding.svt_av1", "CQP требует AQ0, остальные режимы AQ2; CBR требует lookahead0, остальные auto либо 6..120.")
	}
	if e.RateControl == RateControlCBR || e.RateControl == RateControlVBR {
		if ve.TargetBitrateBPS == 0 || ve.TargetBitrateBPS > 100000000 {
			return invalidEncoding("video_encoding.target_bitrate_bps", "Битрейт AV1 должен быть от 1 до 100000000 бит/с.")
		}
		if e.RateControl == RateControlVBR {
			if ve.MaxBitrateBPS != 0 || ve.BufferBits != 0 {
				return invalidEncoding("video_encoding", "SVT VBR требует нулевые max и buffer.")
			}
		} else if ve.MaxBitrateBPS != ve.TargetBitrateBPS || ve.BufferBits == 0 || ve.BufferBits > ve.TargetBitrateBPS*10 || ve.BufferBits*1000%ve.TargetBitrateBPS != 0 || ve.BufferBits*1000/ve.TargetBitrateBPS < 20 {
			return invalidEncoding("video_encoding", "SVT CBR требует max=target и точный целый буфер 20..10000 мс.")
		}
	}
	if v.Tier == "high" {
		return p.validateSVTHighTier()
	}
	return nil
}

// validateAOMEncoding проверяет p по реализованным режимам AOM, без публикации фиктивного жёсткого предела VBR.
func (p GroupProfile) validateAOMEncoding() error {
	// e задаёт режим управления битрейтом, ve — битрейт/GOP, v — неподменяемые уровень и категорию.
	e, ve, v := p.Encoding, p.VideoEncoding, p.Format.Video
	if v.Level != "" && v.Level != "auto" || v.Tier != "" && v.Tier != "main" {
		return invalidEncoding("format.video", "AOM допускает только пустой/auto уровень и пустую/main категорию.")
	}
	if ve.MaxBFrames != 0 {
		return invalidEncoding("video_encoding.max_b_frames", "Фиксированный AOM-рецепт требует нулевое число B-кадров.")
	}
	if e.RateControl == RateControlCRF {
		return nil
	}
	if ve.TargetBitrateBPS == 0 || ve.TargetBitrateBPS > 100000000 || ve.TargetBitrateBPS%1000 != 0 {
		return invalidEncoding("video_encoding.target_bitrate_bps", "AOM требует целый битрейт 1..100000000 бит/с, кратный 1000.")
	}
	if e.RateControl == RateControlVBR {
		if ve.MaxBitrateBPS != 0 || ve.BufferBits != 0 {
			return invalidEncoding("video_encoding", "AOM VBR требует нулевые max и buffer.")
		}
	} else if ve.MaxBitrateBPS != ve.TargetBitrateBPS || ve.BufferBits == 0 || ve.BufferBits > math.MaxInt32 || ve.BufferBits*1000%ve.TargetBitrateBPS != 0 || ve.BufferBits*1000/ve.TargetBitrateBPS == 0 {
		return invalidEncoding("video_encoding", "AOM CBR требует max=target и положительный буфер до 2147483647 бит, точно представимый целыми миллисекундами.")
	}
	return nil
}
