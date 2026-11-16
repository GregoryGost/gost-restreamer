package api

import (
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// TestMeasuredAudioBindingProfilesRemainCodecSpecific проверяет каталоги Variant и REST/SSE без изменения raw-формата.
// t сверяет четыре известные привязки, пустоту и чужой профиль в наблюдении.
func TestMeasuredAudioBindingProfilesRemainCodecSpecific(t *testing.T) {
	// scenario связывает кодек с единственным распознаваемым профилем собственного каталога.
	for _, scenario := range []struct {
		// codec выбирает аудиокаталог; profile содержит точное обозначение проверенного формата.
		codec   core.Codec
		profile string
	}{{"aac", "LC"}, {"opus", "celt"}, {"flac", "s16"}, {"mp3", "mpeg1-layer3"}, {"ac3", "ac3"}} {
		// raw содержит исходное наблюдение, а result — публичную проекцию без мутации исходника.
		raw := core.MediaFormat{Audio: core.AudioFormat{Codec: scenario.codec, CodecProfile: scenario.profile, SampleRateHz: 48000, Channels: 2, ChannelLayout: "stereo"}}
		result := measuredFormatDTO(raw)
		if result.Audio.Codec != string(scenario.codec) || result.Audio.CodecProfile != scenario.profile || raw.Audio.CodecProfile != scenario.profile {
			t.Fatal("Изменена известная привязка", raw, result)
		}
		raw.Audio.CodecProfile = "LC"
		if scenario.codec == "aac" {
			raw.Audio.CodecProfile = "s16"
		}
		result = measuredFormatDTO(raw)
		if result.Audio.CodecProfile != "UNKNOWN" || raw.Audio.CodecProfile == "UNKNOWN" {
			t.Fatal("Чужой профиль признан известным либо изменено raw", raw, result)
		}
		raw.Audio.CodecProfile = ""
		if measuredFormatDTO(raw).Audio.CodecProfile != "" {
			t.Fatal("Потеряна пустота измерения")
		}
		// metrics использует общий сериализатор REST, SSE и атомарного снимка оперативных метрик.
		metrics := mediaDTO(core.MediaMetrics{Format: core.ObservedFormat{AudioCodec: scenario.codec}})
		if metrics.Format.AudioCodec != string(scenario.codec) {
			t.Fatal("Потерян известный кодек метрик", metrics)
		}
	}
	// metrics подтверждает независимый UNKNOWN для неизвестного непустого кодека метрик.
	metrics := mediaDTO(core.MediaMetrics{Format: core.ObservedFormat{AudioCodec: "future-audio"}})
	if metrics.Format.AudioCodec != "UNKNOWN" {
		t.Fatal(metrics)
	}
}
