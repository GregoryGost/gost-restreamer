package core_test

import (
	"bytes"
	"errors"
	"slices"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// TestCurrentEncodingDefaultsPreservePresence проверяет единственные начальные настройки и отсутствие скрытого качества.
// t сравнивает полные применимые блоки, включая нули, false и автоматический lookahead SVT.
func TestCurrentEncodingDefaultsPreservePresence(t *testing.T) {
	// nvenc содержит материализованные параметры H.264 без запроса качества и AQ.
	nvenc := core.DefaultEncoding(core.ModeTranscode, core.VideoEncoderNVENC, core.RateControlVBR)
	if nvenc.AudioEncoder != core.AudioEncoderAAC || !nvenc.RescaleOutput || nvenc.QP.Set || nvenc.CRF.Set || nvenc.NVENC != (core.NVENCSettings{Preset: "p4", Tuning: "hq", Multipass: "disabled", AQStrength: 8}) || nvenc.SVTAV1 != (core.SVTAV1Settings{}) {
		t.Fatal("Начальные настройки NVENC потеряли материализованные значения", nvenc)
	}
	// mode выбирает управление битрейтом или качеством; качество всегда назначает вызывающий код.
	for _, mode := range []string{core.RateControlCBR, core.RateControlVBR, core.RateControlCQP, core.RateControlCRF} {
		// svt содержит применимые начальные настройки SVT; expected задаёт независимое ожидание параметров.
		svt := core.DefaultEncoding(core.ModeCopy, core.VideoEncoderSVTAV1, mode)
		expected := core.SVTAV1Settings{Preset: 8, Tuning: "psnr", AQMode: 2}
		if mode != core.RateControlCBR {
			expected.LookaheadFrames = -1
		}
		if mode == core.RateControlCQP {
			expected.AQMode = 0
		}
		if svt.SVTAV1 != expected || svt.RescaleOutput || svt.QP != (core.OptionalQP{}) || svt.CRF != (core.OptionalCRF{}) || svt.NVENC != (core.NVENCSettings{}) {
			t.Fatal("Начальные настройки SVT потеряли применимость или присутствие", mode, svt)
		}
	}
	// incomplete отличается от полного профиля только отсутствующим Encoding либо LIVE-селектором.
	for _, incomplete := range []core.GroupProfile{
		{Mode: core.ModeCopy, LiveAudioMode: core.LiveAudioCopy, Format: validProfile().Format, VideoEncoding: validProfile().VideoEncoding, AudioEncoding: validProfile().AudioEncoding},
		{Mode: core.ModeCopy, Format: validProfile().Format, VideoEncoding: validProfile().VideoEncoding, AudioEncoding: validProfile().AudioEncoding, Encoding: validProfile().Encoding},
	} {
		// before позволяет отличить отказ от скрытой материализации настроек.
		before := incomplete
		if !errors.Is(incomplete.Validate(), core.ErrInvalidProfile) || incomplete != before {
			t.Fatal("Validate назначил начальные настройки либо принял неполный профиль", incomplete)
		}
	}
}

// TestCurrentAudioBindingsAreIndependentOfVideo проверяет применимые комбинации всех семи видеокодировщиков.
// t сохраняет реальную аудиодорожку при проверке видео; FLAC в copy остаётся запрещённым.
func TestCurrentAudioBindingsAreIndependentOfVideo(t *testing.T) {
	// encoder перебирает текущие реализации без селектора версии возможностей.
	for _, encoder := range []string{core.VideoEncoderNVENC, core.VideoEncoderSVTAV1, core.VideoEncoderX264, core.VideoEncoderAOMAV1, core.VideoEncoderX265, core.VideoEncoderHEVCNVENC, core.VideoEncoderVP9} {
		// profile содержит допустимый видеорецепт, не зависящий от аудиопривязки.
		profile := expandedProfile(encoder, core.RateControlVBR)
		if encoder == core.VideoEncoderX265 || encoder == core.VideoEncoderHEVCNVENC || encoder == core.VideoEncoderVP9 {
			profile = hdrRecipe(encoder, core.RateControlVBR, core.ModeTranscode, false)
		}
		// binding задаёт точные codec/profile/encoder/bitrate без заимствования каталога реализации.
		for _, binding := range []struct {
			// codec и profile задают желаемое аудио; encoder и bitrate фиксируют рецепт.
			codec            core.Codec
			profile, encoder string
			bitrate          uint64
		}{
			{core.CodecAAC, "LC", "aac", 128000},
			{core.CodecOpus, "celt", "libopus", 128000},
			{core.CodecFLAC, "s16", "flac", 0},
			{core.CodecMP3, "mpeg1-layer3", "libmp3lame", 128000},
			{core.CodecAC3, "ac3", "ac3", 192000},
		} {
			// candidate сохраняет независимые поля обеих дорожек и полный выбранный рецепт.
			candidate := profile
			candidate.Format.Audio = core.AudioFormat{Codec: binding.codec, CodecProfile: binding.profile, SampleRateHz: 48000, Channels: 2, ChannelLayout: "stereo"}
			candidate.Encoding.AudioEncoder, candidate.AudioEncoding.BitrateBPS = binding.encoder, binding.bitrate
			if binding.codec == core.CodecFLAC && candidate.Mode == core.ModeTranscode {
				candidate.LiveAudioMode = core.LiveAudioTranscode
			}
			if binding.codec == core.CodecFLAC && candidate.Mode == core.ModeCopy {
				if candidate.Validate() == nil {
					t.Fatal("FLAC принят при video copy", encoder)
				}
				continue
			}
			// before защищает исходные значения; raw и err проверяют полную форму хранения.
			before := candidate
			raw, err := core.EncodeProfile(candidate)
			if err != nil || candidate != before {
				t.Fatal("Независимая аудиопривязка отвергнута либо заменена", encoder, binding, err)
			}
			// decoded должен сохранить реальную аудиодорожку, а не временную AAC-проекцию.
			decoded, err := core.DecodeProfile(raw)
			if err != nil || decoded != candidate || core.CompatibleProfileFormat(decoded.Format, candidate) != nil {
				t.Fatal("Аудиопривязка потеряна в полном профиле", encoder, binding, err)
			}
		}
	}
}

// TestCurrentCatalogReturnsIndependentFullSets проверяет общий каталог без изменения последующими вызовами.
// t сверяет полный список кодеков и кодировщиков, затем изменяет только возвращённую копию.
func TestCurrentCatalogReturnsIndependentFullSets(t *testing.T) {
	// catalog содержит ожидаемый конечный набор для конкретного публичного поля.
	for _, catalog := range []struct {
		// field выбирает настройку, values содержит полный независимый список допустимых строк.
		field  string
		values []string
	}{
		{"video.codec", []string{"av1", "h264", "hevc", "vp9"}},
		{"audio.codec", []string{"aac", "opus", "flac", "mp3", "ac3"}},
		{"encoding.video_encoder", []string{"h264_nvenc", "libsvtav1", "libx264", "libaom-av1", "libx265", "hevc_nvenc", "libvpx-vp9"}},
		{"encoding.audio_encoder", []string{"aac", "libopus", "flac", "libmp3lame", "ac3"}},
	} {
		// values содержит новую копию каталога для проверки полноты и отсутствия общего изменяемого среза.
		values := core.DesiredStringValues(catalog.field)
		if !slices.Equal(values, catalog.values) {
			t.Fatal("Каталог потерял текущую привязку", catalog.field, values)
		}
		values[0] = "changed"
		if !slices.Equal(core.DesiredStringValues(catalog.field), catalog.values) {
			t.Fatal("Изменение возвращённого каталога затронуло следующий вызов", catalog.field)
		}
	}
}

// TestCurrentProfileRejectsCapabilitySelectors проверяет строгий текущий контракт хранения.
// t добавляет старый селектор к полному валидному профилю; отказ должен сохранять получателя.
func TestCurrentProfileRejectsCapabilitySelectors(t *testing.T) {
	// profile и raw задают полный текущий профиль, err сообщает об ошибке построения фикстуры.
	profile := literalHEVCHDRProfile()
	raw, err := core.EncodeProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	// injection добавляет лишний селектор в корень либо материализованный Encoding.
	for _, injection := range []struct {
		// from указывает точное место вставки, to содержит несовместимую форму.
		from, to string
	}{
		{`"Mode":`, `"Version":1,"Mode":`},
		{`"Encoding":{"QP":`, `"Encoding":{"Version":6,"QP":`},
	} {
		// input содержит одну несовместимую форму, receiver сохраняет все поля при отказе.
		input := bytes.Replace(raw, []byte(injection.from), []byte(injection.to), 1)
		receiver := profile
		if bytes.Equal(input, raw) || !errors.Is(receiver.UnmarshalJSON(input), core.ErrInvalidMetadata) || receiver != profile {
			t.Fatal("Несовместимая форма принята либо изменила профиль", injection)
		}
	}
}

// TestCurrentRequiredSetRejectsCanonicalDuplicates проверяет уникальность профиля во всех состояниях поколения.
// t меняет ID и пути второго требования, поэтому отказ может объясняться только повторным каноническим профилем.
func TestCurrentRequiredSetRejectsCanonicalDuplicates(t *testing.T) {
	// state выбирает активное, опубликованное либо неуспешное поколение без исключений для дублей.
	for _, state := range []core.PreparationState{core.PreparationQueued, core.PreparationClaimed, core.PreparationIssued, core.PreparationPublished, core.PreparationFailed, core.PreparationInterrupted} {
		// generation содержит согласованные профили и пути до добавления второго требования.
		generation := validGeneration(t)
		generation.State = state
		// duplicate сохраняет полный профиль, но получает отдельные идентификатор и пути.
		duplicate := generation.RequiredProfiles[0]
		duplicate.VariantID = "second-variant"
		duplicate.ManagedPath = "fallback/generation/second.mp4"
		duplicate.StagingPath = "fallback/generation/second.stage"
		generation.RequiredProfiles = append(generation.RequiredProfiles, duplicate)
		if !errors.Is(generation.Validate(), core.ErrInvalid) {
			t.Fatal("Состояние поколения разрешило дублировать канонический профиль", state)
		}
	}
}
