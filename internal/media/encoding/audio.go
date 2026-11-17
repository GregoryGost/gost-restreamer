package encoding

import (
	"strconv"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// compileAAC строит AAC LC из полного профиля p для target, включая нативную сигнализацию 7.1.
// LIVE copy возвращает только копирование без права исправлять заголовки источника.
func compileAAC(p core.GroupProfile, target Target) []string {
	if target == Live && p.EffectiveLiveAudioMode() == core.LiveAudioCopy {
		return []string{"-c:a", "copy"}
	}
	// a задаёт целевые параметры AAC; audio накапливает аргументы кодировщика.
	a := p.Format.Audio
	audio := []string{"-c:a", "aac", "-profile:a", "aac_low", "-b:a", strconv.FormatUint(p.AudioEncoding.BitrateBPS, 10), "-ar:a", strconv.FormatUint(uint64(a.SampleRateHz), 10), "-ac:a", strconv.FormatUint(uint64(a.Channels), 10), "-channel_layout:a", a.ChannelLayout}
	if a.ChannelLayout == "7.1" {
		audio = append(audio, "-aac_pce", "0")
	}
	return appendCustom(audio, p.Encoding.AudioCustom)
}

// compileAudio строит конечный аудиорецепт проверенного p для target.
// LIVE copy не меняет входные AU; Prepared создаёт собственную тишину выбранного кодека.
func compileAudio(p core.GroupProfile, target Target) ([]string, error) {
	if p.Format.Audio.Codec == core.CodecAAC {
		return compileAAC(p, target), nil
	}
	if target == Live && p.EffectiveLiveAudioMode() == core.LiveAudioCopy {
		return []string{"-c:a", "copy"}, nil
	}
	// audio задаёт единственный выбранный кодировщик и явное преобразование к 48 kHz stereo.
	audio := []string{"-c:a", p.Encoding.AudioEncoder, "-ar:a", "48000", "-ac:a", "2", "-channel_layout:a", "stereo"}
	switch p.Format.Audio.Codec {
	case core.CodecOpus:
		audio = append(audio, "-b:a", "128000", "-application", "lowdelay", "-frame_duration", "2.5", "-vbr", "on", "-mapping_family", "0", "-fec", "0")
	case core.CodecFLAC:
		audio = append(audio, "-sample_fmt:a", "s16", "-strict", "-2")
	case core.CodecMP3:
		audio = append(audio, "-b:a", "128000", "-reservoir", "0", "-abr", "0")
	case core.CodecAC3:
		audio = append(audio, "-b:a", "192000")
		if target == Prepared {
			audio = append(audio, "-channel_coupling", "0", "-stereo_rematrixing", "0")
		}
	default:
		return nil, core.ErrInvalid
	}
	return audio, nil
}
