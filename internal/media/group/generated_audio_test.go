package group

import (
	"bytes"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

// TestGeneratedLiveHeadersRequiresFreshEncodingProfile проверяет явный допуск нормализатора:
// только нативный AAC с LIVE transcode получает обработчик; copy и неполные настройки отклоняются.
// t проверяет собственную коррекцию ASC и её запрет для чужого либо несовместимого результата.
func TestGeneratedLiveHeadersRequiresFreshEncodingProfile(t *testing.T) {
	// profile содержит допустимый профиль NVENC с новым обычным AAC LC 7.1.
	profile := core.GroupProfile{Mode: core.ModeTranscode,
		Format: core.MediaFormat{Video: core.VideoFormat{Codec: core.CodecH264, CodecProfile: "high", Width: 128, Height: 128, FrameRate: core.FrameRate{Numerator: 30, Denominator: 1}, PixelFormat: "yuv420p", BitDepth: 8, Color: core.ColorInfo{Range: "limited", Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"}},
			Audio: core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", Channels: 8, ChannelLayout: "7.1", SampleRateHz: 48000}},
		VideoEncoding: core.VideoEncoding{TargetBitrateBPS: 100000, MaxBitrateBPS: 200000, BufferBits: 200000, GOPFrames: 15, ClosedGOP: true},
		AudioEncoding: core.AudioEncoding{BitrateBPS: 64000}, LiveAudioMode: core.LiveAudioTranscode}
	profile.Encoding = core.DefaultEncoding(profile.Mode, core.VideoEncoderNVENC, core.RateControlVBR)
	// normalize содержит обработчик лишь подтверждённого собственного LIVE-рецепта.
	normalize := generatedLiveHeaders(profile)
	if normalize == nil {
		t.Fatal("собственный AAC не получил обработчик")
	}
	// native содержит независимый ASC 48 кГц с channel_configuration7; original сохраняет его для
	// проверки отсутствия мутации входного буфера.
	native := []byte{0x11, 0xb8, 0x56, 0xe5, 0}
	original := bytes.Clone(native)
	// corrected содержит только исправленную сигнализацию конфигурации12; err сообщает об отказе.
	corrected, err := normalize(fmp4.TrackDescription{Track: media.TrackAudio, Codec: core.CodecAAC, Headers: native})
	if err != nil || !bytes.Equal(corrected, []byte{0x11, 0xe0, 0x56, 0xe5, 0}) || !bytes.Equal(native, original) {
		t.Fatal("неверная коррекция собственного ASC", corrected, native, err)
	}
	// video содержит произвольные видеозаголовки, которые обработчик обязан сохранить побайтно.
	video := []byte{1, 2, 3}
	if corrected, err = normalize(fmp4.TrackDescription{Track: media.TrackVideo, Headers: video}); err != nil || !bytes.Equal(corrected, video) {
		t.Fatal("изменены видеозаголовки", corrected, err)
	}
	// raw перебирает неверную частоту, стерео и повреждённую сигнализацию вне известного рецепта.
	for _, raw := range [][]byte{{0x12, 0x38, 0x56, 0xe5, 0}, {0x11, 0x90}, {0x11, 0xb8}, {0x11, 0xb8, 0, 0, 0}} {
		if _, err = normalize(fmp4.TrackDescription{Track: media.TrackAudio, Codec: core.CodecAAC, Headers: raw}); err == nil {
			t.Fatal("неизвестный либо несовместимый ASC исправлен", raw)
		}
	}
	// scenario выбирает отсутствие собственного LIVE-кодирования или неверный профиль.
	for _, scenario := range []string{"copy", "missing-settings", "other-encoder", "stereo", "invalid"} {
		// other содержит отдельную копию профиля для одного отрицательного случая.
		other := profile
		switch scenario {
		case "copy":
			other.LiveAudioMode = core.LiveAudioCopy
		case "missing-settings":
			other.Encoding = core.EncodingSettings{}
		case "other-encoder":
			other.Encoding.AudioEncoder = "other"
		case "stereo":
			other.Format.Audio.Channels, other.Format.Audio.ChannelLayout = 2, "stereo"
		case "invalid":
			other.Format.Audio.CodecProfile = "HE"
		}
		if generatedLiveHeaders(other) != nil {
			t.Fatal("неподходящий профиль получил коррекцию", scenario)
		}
	}
}
