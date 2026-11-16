package core

// DesiredStringValues возвращает независимый конечный каталог настройки field.
// Имена video.level.h264 и video.level.av1 разделяют уровни кодеков; неизвестное поле даёт nil.
func DesiredStringValues(field string) []string {
	switch field {
	case "mode", "live_audio_mode":
		return []string{"copy", "transcode"}
	case "video.codec":
		return []string{"av1", "h264", "hevc", "vp9"}
	case "video.codec_profile":
		return []string{"baseline", "constrained-baseline", "main", "high", "high10", "high422", "high444", "professional", "main10", "profile0", "profile2"}
	case "video.level.h264":
		return []string{"", "auto", "1", "1.0", "1b", "1.0b", "1.1", "1.2", "1.3", "2", "2.0", "2.1", "2.2", "3", "3.0", "3.1", "3.2", "4", "4.0", "4.1", "4.2", "5", "5.0", "5.1", "5.2", "6.0", "6.1", "6.2"}
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
	case "video.level.hevc", "video.level.vp9":
		return []string{"", "auto"}
	case "video.tier":
		return []string{"", "main", "high"}
	case "video.pixel_format":
		return []string{"yuv420p", "yuv420p10le", "yuv422p", "yuv422p10le", "yuv444p", "yuv444p10le", "gray"}
	case "video.color.range":
		return []string{"limited", "full", "tv", "pc"}
	case "video.color.primaries":
		return []string{"bt709", "bt470m", "bt470bg", "smpte170m", "smpte240m", "film", "bt2020"}
	case "video.color.transfer":
		return []string{"bt709", "gamma22", "gamma28", "smpte170m", "smpte240m", "linear", "iec61966-2-1", "bt2020-10", "bt2020-12", "smpte2084", "arib-std-b67"}
	case "video.color.matrix":
		return []string{"bt709", "fcc", "bt470bg", "smpte170m", "smpte240m", "bt2020nc", "bt2020c"}
	case "audio.codec":
		return []string{"aac", "opus", "flac", "mp3", "ac3"}
	case "encoding.audio_encoder":
		return []string{AudioEncoderAAC, AudioEncoderOpus, AudioEncoderFLAC, AudioEncoderMP3, AudioEncoderAC3}
	case "audio.codec_profile":
		return []string{"LC", "celt", "s16", "mpeg1-layer3", "ac3"}
	case "audio.channel_layout":
		return []string{"mono", "stereo", "2.1", "3.0", "4.0", "5.0", "5.1", "7.1"}
	case "encoding.video_encoder":
		return []string{VideoEncoderNVENC, VideoEncoderSVTAV1, VideoEncoderX264, VideoEncoderAOMAV1, VideoEncoderX265, VideoEncoderHEVCNVENC, VideoEncoderVP9}
	case "encoding.rate_control":
		return []string{RateControlCBR, RateControlVBR, RateControlCQP, RateControlCRF}
	case "encoding.nvenc.preset":
		return []string{"p1", "p2", "p3", "p4", "p5", "p6", "p7"}
	case "encoding.nvenc.tuning":
		return []string{"hq", "ll", "ull", "lossless"}
	case "encoding.nvenc.multipass":
		return []string{"disabled", "qres", "fullres"}
	case "encoding.svt_av1.tuning":
		return []string{"psnr", "vq", "ssim", "ms_ssim", "vmaf"}
	default:
		return nil
	}
}

// ValidAACLayout проверяет точную пару схемы AAC LC и числа каналов.
// Наблюдаемая нормативная 7.1(wide-back) не является желаемой схемой 7.1.
func ValidAACLayout(layout string, channels uint16) bool {
	switch layout {
	case "mono":
		return channels == 1
	case "stereo":
		return channels == 2
	case "2.1", "3.0":
		return channels == 3
	case "4.0":
		return channels == 4
	case "5.0":
		return channels == 5
	case "5.1":
		return channels == 6
	case "7.1":
		return channels == 8
	default:
		return false
	}
}
