package encoding

import (
	"reflect"
	"strings"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// TestAudioBindingsKeepVideoRecipeAndSeparatePreparedSilence проверяет конечные аудиорецепты
// и побайтную неизменность видеорецепта при смене аудиопривязки. t получает ошибки компиляции.
func TestAudioBindingsKeepVideoRecipeAndSeparatePreparedSilence(t *testing.T) {
	// old содержит существующий программный x264-рецепт, baseline — его исходный набор аргументов.
	old := compilerProfile(core.CodecH264)
	old.LiveAudioMode = core.LiveAudioTranscode
	old.Encoding = core.DefaultEncoding(old.Mode, core.VideoEncoderX264, core.RateControlVBR)
	// baseline и err фиксируют контрольные видео- и AAC-аргументы прежнего профиля.
	baseline, err := Compile(old, Prepared)
	if err != nil {
		t.Fatal(err)
	}
	// scenario перечисляет точные ожидаемые argv новых конечных привязок.
	for _, scenario := range []struct {
		// kind и profile задают целевой кодек; encoder и bitrate задают его фиксированный запрос.
		kind             core.Codec
		profile, encoder string
		bitrate          uint64
		// suffix содержит обязательные параметры после общей частоты и канализации.
		suffix []string
	}{
		{core.CodecOpus, "celt", core.AudioEncoderOpus, 128000, []string{"-b:a", "128000", "-application", "lowdelay", "-frame_duration", "2.5", "-vbr", "on", "-mapping_family", "0", "-fec", "0"}},
		{core.CodecFLAC, "s16", core.AudioEncoderFLAC, 0, []string{"-sample_fmt:a", "s16", "-strict", "-2"}},
		{core.CodecMP3, "mpeg1-layer3", core.AudioEncoderMP3, 128000, []string{"-b:a", "128000", "-reservoir", "0", "-abr", "0"}},
		{core.CodecAC3, "ac3", core.AudioEncoderAC3, 192000, []string{"-b:a", "192000", "-channel_coupling", "0", "-stereo_rematrixing", "0"}},
	} {
		// p содержит настоящий новый профиль с исходным видеорецептом; before проверяет неизменность входа.
		p := old
		p.Encoding.AudioEncoder = scenario.encoder
		p.Format.Audio.Codec, p.Format.Audio.CodecProfile = scenario.kind, scenario.profile
		p.AudioEncoding.BitrateBPS = scenario.bitrate
		before := p
		// prepared содержит реальный файловый рецепт; compileErr сообщает об отказе компиляции;
		// want задаёт независимый точный аудиоэталон.
		prepared, compileErr := Compile(p, Prepared)
		want := append([]string{"-c:a", scenario.encoder, "-ar:a", "48000", "-ac:a", "2", "-channel_layout:a", "stereo"}, scenario.suffix...)
		if compileErr != nil || !reflect.DeepEqual(prepared.Video, baseline.Video) || !reflect.DeepEqual(prepared.Audio, want) || p != before {
			t.Fatal(scenario.kind, prepared, want, compileErr)
		}
		// live сохраняет ту же привязку, но не применяет параметры собственной AC3-тишины к слышимому LIVE.
		// compileErr сообщает об отказе компиляции LIVE-рецепта.
		live, compileErr := Compile(p, Live)
		if compileErr != nil || strings.Contains(strings.Join(live.Audio, " "), "-channel_coupling") || strings.Contains(strings.Join(live.Audio, " "), "-stereo_rematrixing") {
			t.Fatal(scenario.kind, live, compileErr)
		}
		if scenario.kind != core.CodecFLAC {
			p.LiveAudioMode = core.LiveAudioCopy
			live, compileErr = Compile(p, Live)
			if compileErr != nil || !reflect.DeepEqual(live.Audio, []string{"-c:a", "copy"}) {
				t.Fatal(live, compileErr)
			}
		}
	}
}

// TestAAC71NormalizationRequiresFreshNativeBinding проверяет допуск собственной
// AAC7.1: Prepared и явный LIVE transcode исправляют ASC, copy такого права не имеет.
// t сообщает о нарушении профиля или границы свежего результата.
func TestAAC71NormalizationRequiresFreshNativeBinding(t *testing.T) {
	// profile задаёт полный AAC7.1 рецепт текущей модели.
	profile := compilerProfile(core.CodecH264)
	profile.Encoding = core.DefaultEncoding(profile.Mode, core.VideoEncoderX264, core.RateControlVBR)
	profile.LiveAudioMode = core.LiveAudioTranscode
	profile.Format.Audio.Channels, profile.Format.Audio.ChannelLayout = 8, "7.1"
	if !UsesNativeAAC71(profile, Prepared) || !UsesNativeAAC71(profile, Live) {
		t.Fatal("текущий профиль потерял собственную AAC7.1-нормализацию")
	}
	profile.LiveAudioMode = core.LiveAudioCopy
	if !UsesNativeAAC71(profile, Prepared) || UsesNativeAAC71(profile, Live) {
		t.Fatal("граница AAC7.1 распространилась на LIVE copy")
	}
}
