package core

import "slices"

// validateAudioEncoding проверяет кодек, профиль, частоту, каналы, encoder и битрейт p как одно целое.
// AAC проверяется по собственной карте каналов; остальные привязки имеют фиксированные
// частоту, stereo и битрейт. FLAC требует явного LIVE transcode.
func (p GroupProfile) validateAudioEncoding() error {
	// a содержит желаемый аудиоформат, encoder выбирает точный кодировщик, bitrate задаёт битрейт привязки.
	a := p.Format.Audio
	var encoder string
	var bitrate uint64
	switch a.Codec {
	case CodecAAC:
		if p.Encoding.AudioEncoder != AudioEncoderAAC || a.CodecProfile != "LC" || !ValidAACLayout(a.ChannelLayout, a.Channels) || !slices.Contains([]uint32{7350, 8000, 11025, 12000, 16000, 22050, 24000, 32000, 44100, 48000, 64000}, a.SampleRateHz) {
			return invalidEncoding("format.audio", "Требуются AAC LC, поддержанная схема каналов и частота дискретизации 7350..64000 Гц.")
		}
		if p.AudioEncoding.BitrateBPS == 0 || p.AudioEncoding.BitrateBPS > 6*uint64(a.SampleRateHz)*uint64(a.Channels) {
			return invalidEncoding("audio_encoding.bitrate_bps", "Битрейт AAC должен быть положительным и не превышать 6 × частота × число каналов.")
		}
		// normalized хранит каноническую строку AAC, err сообщает об ошибке её разбора.
		normalized, err := NormalizeCustom(p.Encoding.AudioEncoder, p.Encoding.AudioCustom)
		if err != nil {
			return err
		}
		if normalized != p.Encoding.AudioCustom {
			return invalidEncoding("encoding.audio_custom", "Параметры должны храниться в нормализованном виде.")
		}
		return nil
	case CodecOpus:
		encoder, bitrate = AudioEncoderOpus, 128000
		if a.CodecProfile != "celt" {
			return invalidEncoding("format.audio.codec_profile", "Opus требует профиль celt.")
		}
	case CodecFLAC:
		encoder = AudioEncoderFLAC
		if a.CodecProfile != "s16" {
			return invalidEncoding("format.audio.codec_profile", "FLAC требует профиль s16.")
		}
		if p.Mode != ModeTranscode || p.LiveAudioMode != LiveAudioTranscode {
			return invalidEncoding("live_audio_mode", "FLAC требует режим видео transcode и явный live_audio_mode=transcode.")
		}
	case CodecMP3:
		encoder, bitrate = AudioEncoderMP3, 128000
		if a.CodecProfile != "mpeg1-layer3" {
			return invalidEncoding("format.audio.codec_profile", "MP3 требует профиль mpeg1-layer3.")
		}
	case CodecAC3:
		encoder, bitrate = AudioEncoderAC3, 192000
		if a.CodecProfile != "ac3" {
			return invalidEncoding("format.audio.codec_profile", "AC-3 требует профиль ac3.")
		}
	default:
		return invalidEncoding("format.audio.codec", "Значение отсутствует в поддерживаемом наборе аудиопривязок.")
	}
	if a.SampleRateHz != 48000 || a.Channels != 2 || a.ChannelLayout != "stereo" {
		return invalidEncoding("format.audio", "Выбранная аудиопривязка требует 48000 Гц и два канала stereo.")
	}
	if p.Encoding.AudioEncoder != encoder {
		return invalidEncoding("encoding.audio_encoder", "Аудиокодировщик должен точно соответствовать выбранной аудиопривязке.")
	}
	if p.AudioEncoding.BitrateBPS != bitrate {
		return invalidEncoding("audio_encoding.bitrate_bps", "Битрейт должен соответствовать фиксированному рецепту; FLAC требует ноль.")
	}
	if p.Encoding.AudioCustom != "" {
		return invalidEncoding("encoding.audio_custom", "Выбранные аудиокодировщики требуют пустую строку дополнительных параметров.")
	}
	return nil
}
