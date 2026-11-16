package encoding

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// compileVideo строит единственный видеорецепт проверенного p по выбранному кодировщику.
// target различает LIVE и подготовку; физический формат кадров задаёт построитель фильтра.
// Ошибка означает, что выбранный кодировщик не поддержан в указанном режиме.
func compileVideo(p core.GroupProfile, target Target) ([]string, error) {
	// v содержит целевое видео, e — настройки кодировщика, ve — битрейт и GOP.
	v, e, ve := p.Format.Video, p.Encoding, p.VideoEncoding
	// number переводит value без потери точности в десятичный аргумент FFmpeg.
	number := func(value uint64) string { return strconv.FormatUint(value, 10) }
	// video накапливает пары аргументов выбранного кодировщика без скрытой подмены.
	video := []string{"-c:v", e.VideoEncoder, "-g:v", strconv.FormatUint(uint64(ve.GOPFrames), 10), "-flags:v", "+cgop"}
	// quality отделяет чистое качество от управления битрейтом с явным буфером.
	quality := e.RateControl == core.RateControlCQP || e.RateControl == core.RateControlCRF
	if !quality {
		video = append(video, "-b:v", strconv.FormatUint(ve.TargetBitrateBPS, 10))
		if e.VideoEncoder != core.VideoEncoderAOMAV1 && e.VideoEncoder != core.VideoEncoderSVTAV1 && e.VideoEncoder != core.VideoEncoderVP9 || e.RateControl == core.RateControlCBR {
			video = append(video, "-maxrate:v", strconv.FormatUint(ve.MaxBitrateBPS, 10), "-bufsize:v", strconv.FormatUint(ve.BufferBits, 10))
		}
	}
	switch e.VideoEncoder {
	case core.VideoEncoderX264:
		// profile переводит единственное публичное constrained-baseline в baseline кодировщика.
		profile := v.CodecProfile
		if profile == "constrained-baseline" {
			profile = "baseline"
		}
		// params фиксирует закрытый GOP, отсутствие смены сцен и единственный опорный кадр.
		params := "open-gop=0:scenecut=0:ref=1"
		if e.RateControl == core.RateControlCBR {
			params += ":nal-hrd=cbr"
			video = append(video, "-minrate:v", strconv.FormatUint(ve.TargetBitrateBPS, 10))
		}
		video = append(video, "-preset", "medium", "-profile:v", profile, "-bf:v", strconv.FormatUint(uint64(ve.MaxBFrames), 10), "-forced-idr", "1", "-sc_threshold", "0", "-refs:v", "1", "-x264-params", params)
		switch e.RateControl {
		case core.RateControlCQP:
			video = append(video, "-qp", strconv.Itoa(e.QP.Value))
		case core.RateControlCRF:
			video = append(video, "-crf", crfArgument(e.CRF.ValueHundredths))
		}
	case core.VideoEncoderAOMAV1:
		if target != Prepared {
			return nil, core.ErrInvalid
		}
		// profile задаёт профиль AV1 кодировщика, согласованный с проверкой цветности и глубины в домене.
		profile := "0"
		switch v.CodecProfile {
		case "high":
			profile = "1"
		case "professional":
			profile = "2"
		}
		video = append(video, "-usage", "good", "-cpu-used", "8", "-row-mt", "1", "-lag-in-frames", "0", "-auto-alt-ref", "0", "-keyint_min:v", strconv.FormatUint(uint64(ve.GOPFrames), 10), "-profile:v", profile, "-bf:v", "0")
		switch e.RateControl {
		case core.RateControlCRF:
			video = append(video, "-b:v", "0", "-crf", strconv.Itoa(e.CRF.ValueHundredths/100))
		case core.RateControlCBR:
			video = append(video, "-minrate:v", strconv.FormatUint(ve.TargetBitrateBPS, 10))
		}
	case core.VideoEncoderNVENC:
		// nv содержит явно материализованные настройки аппаратного кодировщика.
		nv := e.NVENC
		// profile отображает high444 в имя профиля NVENC; остальные имена совпадают.
		profile := v.CodecProfile
		if profile == "high444" {
			profile = "high444p"
		}
		// rc переводит CQP в constqp кодировщика без неоднозначного общего -qp.
		rc := e.RateControl
		if rc == core.RateControlCQP {
			rc = "constqp"
		}
		video = append(video, "-profile:v", profile, "-bf:v", strconv.FormatUint(uint64(ve.MaxBFrames), 10), "-no-scenecut", "1", "-forced-idr", "1", "-rc", rc,
			"-preset", nv.Preset, "-tune", nv.Tuning, "-multipass", nv.Multipass, "-rc-lookahead", strconv.Itoa(nv.LookaheadFrames), "-spatial-aq", boolArgument(nv.EnableSpatialAQ), "-temporal-aq", boolArgument(nv.EnableTemporalAQ), "-aq-strength", strconv.Itoa(nv.AQStrength))
		if quality {
			video = append(video, "-init_qpI", strconv.Itoa(e.QP.Value), "-init_qpP", strconv.Itoa(e.QP.Value), "-init_qpB", strconv.Itoa(e.QP.Value))
		}
		if v.CodecProfile == "high444" {
			video = append(video, "-coder", "cavlc")
		}
		if strings.HasPrefix(v.Level, "6.") {
			video = append(video, "-dpb_size", "1")
		}
		video = appendCustom(video, e.VideoCustom)
	case core.VideoEncoderSVTAV1:
		if target != Prepared {
			return nil, core.ErrInvalid
		}
		// rc задаёт управление битрейтом SVT; prediction выбирает предсказание с малой задержкой или произвольным доступом.
		rc, prediction := "2", "1"
		if e.RateControl != core.RateControlCBR {
			rc, prediction = "1", "2"
		}
		// tuning переводит проверенное имя метрики в номер метрики SVT.
		tuning := map[string]int{"psnr": 1, "vq": 0, "ssim": 2, "ms_ssim": 4, "vmaf": 5}[e.SVTAV1.Tuning]
		// params фиксирует однослойный закрытый поток без overlays, TF и динамического GOP.
		params := fmt.Sprintf("rc=%s:mbr=0:pred-struct=%s:scd=0:enable-overlays=0:enable-dg=0:hierarchical-levels=0:enable-tf=0:aq-mode=%d:tune=%d", rc, prediction, e.SVTAV1.AQMode, tuning)
		if quality {
			params = fmt.Sprintf("pred-struct=2:scd=0:enable-overlays=0:enable-dg=0:hierarchical-levels=0:enable-tf=0:aq-mode=%d:tune=%d", e.SVTAV1.AQMode, tuning)
			if e.RateControl == core.RateControlCQP {
				video = append(video, "-qp", strconv.Itoa(e.QP.Value))
			} else {
				video = append(video, "-crf", strconv.Itoa(e.CRF.ValueHundredths/100))
			}
		}
		if e.SVTAV1.LookaheadFrames >= 0 {
			params += ":lookahead=" + strconv.Itoa(e.SVTAV1.LookaheadFrames)
		}
		if v.Tier == "high" {
			// columns и rows задают проверенную доменом сетку тайлов в логарифмических единицах.
			// err сообщает о невозможной для заявленного уровня геометрии или частоте тайлов.
			columns, rows, err := core.AV1TileLayout(p)
			if err != nil {
				return nil, err
			}
			params += fmt.Sprintf(":tier=1:tile-columns=%d:tile-rows=%d", columns, rows)
		}
		if e.VideoCustom != "" {
			params += ":" + strings.ReplaceAll(e.VideoCustom, " ", ":")
		}
		video = append(video, "-svtav1-params", params, "-preset", strconv.Itoa(e.SVTAV1.Preset))
	case core.VideoEncoderX265:
		// params фиксирует IDR на границе GOP, B0 и один опорный кадр; VBV добавляется только при заданном битрейте.
		params := "open-gop=0:scenecut=0:bframes=0:ref=1"
		if e.RateControl == core.RateControlCBR {
			params += ":strict-cbr=1"
		}
		video = append(video, "-preset", "medium", "-profile:v", v.CodecProfile, "-bf:v", "0", "-refs:v", "1", "-forced-idr", "1", "-x265-params", params)
		switch e.RateControl {
		case core.RateControlCQP:
			video = append(video, "-qp", strconv.Itoa(e.QP.Value))
		case core.RateControlCRF:
			video = append(video, "-crf", crfArgument(e.CRF.ValueHundredths))
		}
	case core.VideoEncoderHEVCNVENC:
		// rc переводит чистое QP в нативный constqp, не включая lossless или автоматическую замену кодировщика.
		rc := e.RateControl
		if rc == core.RateControlCQP {
			rc = "constqp"
		}
		video = append(video, "-profile:v", v.CodecProfile, "-bf:v", "0", "-refs:v", "1", "-no-scenecut", "1", "-forced-idr", "1", "-rc", rc,
			"-preset", "p4", "-tune", "hq", "-multipass", "disabled", "-rc-lookahead", "0", "-spatial-aq", "0", "-temporal-aq", "0", "-aq-strength", "8")
		if quality {
			video = append(video, "-init_qpI", strconv.Itoa(e.QP.Value), "-init_qpP", strconv.Itoa(e.QP.Value), "-init_qpB", strconv.Itoa(e.QP.Value))
		}
	case core.VideoEncoderVP9:
		// profile задаёт нативный номер единственного разрешённого сочетания глубины и цветности.
		profile := "0"
		if v.CodecProfile == "profile2" {
			profile = "2"
		}
		video = append(video, "-deadline", "good", "-cpu-used", "8", "-row-mt", "1", "-lag-in-frames", "0", "-auto-alt-ref", "0", "-keyint_min:v", number(uint64(ve.GOPFrames)), "-profile:v", profile, "-bf:v", "0")
		switch e.RateControl {
		case core.RateControlCRF:
			video = append(video, "-b:v", "0", "-crf", strconv.Itoa(e.CRF.ValueHundredths/100))
		case core.RateControlCBR:
			video = append(video, "-minrate:v", number(ve.TargetBitrateBPS))
		}
	default:
		return nil, core.ErrInvalid
	}
	if v.Level != "" && v.Level != "auto" {
		video = append(video, "-level:v", v.Level)
	}
	video = append(video, "-color_range:v", colorRangeArgument(v.Color.Range), "-color_primaries:v", v.Color.Primaries, "-color_trc:v", v.Color.Transfer, "-colorspace:v", v.Color.Matrix)
	return video, nil
}

// crfArgument точно отображает сотые доли value в десятичный аргумент без float64.
func crfArgument(value int) string {
	return strings.TrimRight(strings.TrimRight(fmt.Sprintf("%d.%02d", value/100, value%100), "0"), ".")
}

// boolArgument отображает value в явный флаг FFmpeg 0 или 1.
func boolArgument(value bool) string {
	if value {
		return "1"
	}
	return "0"
}

// colorRangeArgument переводит rangeName в tv/pc кодировщика, сохраняя уже допустимые обозначения.
func colorRangeArgument(rangeName string) string {
	if rangeName == "limited" {
		return "tv"
	}
	if rangeName == "full" {
		return "pc"
	}
	return rangeName
}
