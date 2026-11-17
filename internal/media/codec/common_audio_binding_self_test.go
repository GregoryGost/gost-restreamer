package codec

import (
	"bytes"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
)

// TestAudioParsersRejectEveryTruncatedAU проверяет каждое усечение кадров поддержанных форматов.
// t использует корректный дескриптор каждой AU; усечённый payload не должен пройти уточнение конфигурации.
func TestAudioParsersRejectEveryTruncatedAU(t *testing.T) {
	// mp3 содержит один полный независимый кадр MPEG1L3 с пустыми основными данными.
	mp3 := make([]byte, 384)
	copy(mp3, []byte{0xff, 0xfb, 0x94, 0})
	// ac3 содержит независимо размеченный полный кадр с корректными двумя CRC.
	ac3 := ac3BindingFixture(t)
	// entry выбирает фактические заголовки и полную AU для последовательной проверки всех префиксов.
	for _, entry := range []struct {
		// kind задаёт конкретный разборщик, headers — нормативную запись кодека, payload — полный кадр.
		kind             core.Codec
		headers, payload []byte
	}{
		{core.CodecFLAC, flacBindingHeaders(4608), flacBindingConstant(4608, 0, 1)},
		{core.CodecMP3, bytes.Clone(mp3[:4]), mp3},
		{core.CodecAC3, []byte{0x10, 0x11, 0x40}, ac3},
	} {
		// length перебирает каждый неполный префикс, включая пустой и отсутствие последнего байта.
		for length := range len(entry.payload) {
			// err сообщает о результате проверки известного усечённого префикса.
			if _, err := AudioSamples(entry.kind, entry.headers, entry.payload[:length]); err == nil {
				t.Fatalf("kind=%s: усечённые%d/%dbytes приняты", entry.kind, length, len(entry.payload))
			}
		}
		// actual и completed проверяют нормативную запись полного кадра; err сообщает об отказе уточнения конфигурации.
		actual, completed, err := CompleteAudioConfig(entry.kind, entry.headers, entry.payload)
		if err != nil || actual != audioBinding(entry.kind) || !bytes.Equal(completed, entry.headers) {
			t.Fatalf("kind=%s actual=%+v headers=%x err=%v", entry.kind, actual, completed, err)
		}
	}
}

// TestFLACOwnedRuntimeBoundary проверяет узкое рабочее свидетельство без расширения других кодеков.
// t подтверждает сравнимость Info, копирование заголовков и отказ FLACOwned на неподходящей дорожке.
func TestFLACOwnedRuntimeBoundary(t *testing.T) {
	// info содержит допустимое локальное свидетельство происхождения FLAC для выходной границы.
	info := media.CodecConfigInfo{Track: media.TrackAudio, SourceEpoch: 1, Revision: 1, TimeBase: media.TimeBase{Numerator: 1, Denominator: 48000}, Audio: audioBinding(core.CodecFLAC), FLACOwned: true}
	// headers содержит фактический STREAMINFO; config и err — неизменяемую рабочую конфигурацию либо отказ.
	headers := flacBindingHeaders(4608)
	config, err := media.NewCodecConfig(info, headers)
	if err != nil || config.Info() != info {
		t.Fatal(err)
	}
	headers[0] = 1
	if config.Headers()[0] != 0 {
		t.Fatal("runtime proof обошёл копирование headers")
	}
	info.Audio = audioBinding(core.CodecMP3)
	if _, err = media.NewCodecConfig(info, []byte{0xff, 0xfb, 0x94, 0}); err == nil {
		t.Fatal("FLACOwned назначен MP3")
	}
	info.Track = media.TrackVideo
	if _, err = media.NewCodecConfig(info, []byte{1}); err == nil {
		t.Fatal("FLACOwned назначен video")
	}
}
