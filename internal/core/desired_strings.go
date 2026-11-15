package core

import "slices"

// DesiredStringValues возвращает независимый конечный каталог настройки field.
// Имена video.level.h264 и video.level.av1 разделяют уровни кодеков; неизвестное поле даёт nil.
func DesiredStringValues(field string) []string {
	switch field {
	case "mode", "live_audio_mode":
		return []string{"copy", "transcode"}
	case "video.codec":
		return []string{"av1", "h264"}
	case "video.codec_profile":
		return []string{"baseline", "main", "high"}
	case "video.level.h264":
		return []string{"", "auto", "1", "1.0", "1b", "1.0b", "1.1", "1.2", "1.3", "2", "2.0", "2.1", "2.2", "3", "3.0", "3.1", "3.2", "4", "4.0", "4.1", "4.2", "5", "5.0", "5.1", "5.2"}
	case "video.level.av1":
		// values накапливает специальные обозначения и все поддержанные уровни AV1.
		values := []string{"", "auto"}
		// major задаёт старшую цифру уровня AV1.
		for major := byte('2'); major <= '7'; major++ {
			// minor задаёт младшую цифру уровня в пределах одной версии AV1.
			for minor := byte('0'); minor <= '3'; minor++ {
				values = append(values, string([]byte{major, '.', minor}))
			}
		}
		return values
	case "video.tier":
		return []string{"", "main"}
	case "video.pixel_format":
		return []string{"yuv420p"}
	case "video.color.range":
		return []string{"limited", "full", "tv", "pc"}
	case "video.color.primaries", "video.color.transfer", "video.color.matrix":
		return []string{"bt709"}
	case "audio.codec", "encoding.audio_encoder":
		return []string{"aac"}
	case "audio.codec_profile":
		return []string{"LC"}
	case "audio.channel_layout":
		return []string{"mono", "stereo"}
	case "encoding.video_encoder":
		return []string{VideoEncoderNVENC, VideoEncoderSVTAV1}
	case "encoding.rate_control":
		return []string{RateControlCBR, RateControlVBR}
	case "encoding.nvenc.preset":
		return []string{"p1", "p2", "p3", "p4", "p5", "p6", "p7"}
	case "encoding.nvenc.tuning":
		return []string{"hq", "ll", "ull"}
	case "encoding.nvenc.multipass":
		return []string{"disabled", "qres", "fullres"}
	case "encoding.svt_av1.tuning":
		return []string{"psnr", "vq"}
	default:
		return nil
	}
}

// validateDesiredStrings проверяет настройки p до выбора legacy/v2, сохраняя отдельные числовые
// правила каждой версии. Измеренный MediaFormat этой проверке не подчиняется.
func (p GroupProfile) validateDesiredStrings() error {
	// v и a содержат желаемые параметры видео и аудио проверяемого профиля.
	v, a := p.Format.Video, p.Format.Audio
	// field связывает публичный путь настройки, каталог и переданное значение.
	for _, field := range []struct {
		// path задаёт путь поля в безопасной ошибке, catalog выбирает конечный набор, value проверяется без нормализации.
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
	if p.Mode == ModeCopy && v.Codec != CodecAV1 || p.Mode == ModeTranscode && v.Codec != CodecH264 {
		return invalidEncoding("format.video.codec", "Для copy требуется av1, для transcode — h264.")
	}
	if v.BitDepth != 8 {
		return invalidEncoding("format.video.bit_depth", "Поддерживается только глубина 8 бит.")
	}
	if a.Channels != 1 && a.Channels != 2 || a.Channels == 1 && a.ChannelLayout != "mono" || a.Channels == 2 && a.ChannelLayout != "stereo" {
		return invalidEncoding("format.audio.channel_layout", "Требуется mono для одного канала или stereo для двух.")
	}
	if v.Codec == CodecAV1 {
		if v.CodecProfile != "main" || !slices.Contains(DesiredStringValues("video.level.av1"), v.Level) || !slices.Contains(DesiredStringValues("video.tier"), v.Tier) {
			return invalidEncoding("format.video", "Требуются поддержанные профиль, уровень и категория AV1.")
		}
	} else if !slices.Contains(DesiredStringValues("video.level.h264"), v.Level) || v.Tier != "" || v.CodecProfile == "baseline" && p.VideoEncoding.MaxBFrames > 0 {
		return invalidEncoding("format.video", "Требуются поддержанные параметры H264; baseline запрещает B-кадры.")
	}
	return nil
}
