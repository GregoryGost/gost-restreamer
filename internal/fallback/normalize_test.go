package fallback

import (
	"encoding/hex"
	"os"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

// av1ProbeJSON содержит управляемые метаданные AV1 128×128/30 и стереофонического AAC 48 кГц с петлёй
// 1,6 с.
const av1ProbeJSON = `{"streams":[{"codec_type":"video","codec_name":"av1","profile":"Main","width":128,"height":128,"avg_frame_rate":"30/1","pix_fmt":"yuv420p","level":0,"color_range":"tv","color_primaries":"bt709","color_transfer":"bt709","color_space":"bt709"},{"codec_type":"audio","codec_name":"aac","profile":"LC","sample_rate":"48000","channels":2,"channel_layout":"stereo"}],"format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2","duration":"1.600000"}}`

// Создаёт временный fMP4 с заданным числом AV1- и AAC-пакетов, устаревшим av1C и начальным
// AAC-пакетом; badTU удаляет заголовок последовательности из TU.
func selfRawAV1Counts(t *testing.T, badTU bool, videoCount, audioCount int) *os.File {
	t.Helper()
	// decode переводит фиксированные шестнадцатеричные данные AV1 в байты и завершает тест при
	// ошибке эталона.
	decode := func(s string) []byte {
		// Декодирует шестнадцатеричные заголовки и TU тестового AV1; ошибка исходных данных немедленно
		// завершает тест.

		// v содержит декодированные байты эталона. err сообщает о некорректной шестнадцатеричной
		// записи.
		v, err := hex.DecodeString(s)
		if err != nil {
			t.Fatal(err)
		}
		return v
	}
	// Исходные заголовки и первый TU измерены у libsvtav1; av1C содержит устаревшие флаги инструментов.
	// headers содержит av1C с устаревшими относительно первого TU флагами инструментов.
	headers := decode("81000c000a0e0200000499bfff00be0404040410")
	// tu содержит независимый AV1 TU, из которого при badTU удаляется заголовок последовательности.
	tu := decode("0a0e0200000499bfff1abe6404040410328401100086800410208005e010867aff68da307d4a1db42fe03b916b8008ee5bb9d4ec58d98d41619596b86567475babe5b4b1bd10f07a853f622967ac8dd6d23c722b5828e9adb9664b07e7ec971eade5375099eedda16c77b1d75e2ea2121a087d1156695662548b288b16ea2c931c2d50cae25c0c9f790c92234a87eac96b05f1bb8d0180")
	if badTU {
		tu = tu[16:]
	}
	// profile содержит целевой тестовый формат AV1 и AAC.
	profile := selfProfile(core.CodecAV1)
	// v содержит конфигурацию AV1 с исходным av1C. err сообщает об отказе создания.
	v, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: 1, Revision: 1, TimeBase: media.TimeBase{Numerator: 1, Denominator: 30}, Video: profile.Format.Video}, headers)
	if err != nil {
		t.Fatal(err)
	}
	// a содержит конфигурацию AAC-LC со шкалой 48 кГц.
	a, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackAudio, SourceEpoch: 1, Revision: 1, TimeBase: media.TimeBase{Numerator: 1, Denominator: 48000}, Audio: profile.Format.Audio}, []byte{0x11, 0x90})
	if err != nil {
		t.Fatal(err)
	}
	// f содержит временный исходный fMP4 для проверки нормализации.
	f, err := os.CreateTemp(t.TempDir(), "raw")
	if err != nil {
		t.Fatal(err)
	}
	selfClose(t, f)
	// mux записывает исходные видео- и аудиопакеты со сдвигом, допускающим отрицательную задержку
	// AAC.
	mux, err := fmp4.NewMuxer(f, fmp4.Init{Video: v, Audio: a, Origin: fmp4.OriginMapping{OffsetTicks: 1, TimeBase: media.TimeBase{Numerator: 1, Denominator: 1}}}, selfProbeLimits().Container)
	if err != nil {
		t.Fatal(err)
	}
	// i задаёт очередной номер видео- и аудиопакета до исчерпания обоих счётчиков.
	for i := 0; i < max(videoCount, audioCount); i++ {
		// track выбирает видео или аудио для текущего номера пакета.
		for _, track := range []media.Track{media.TrackVideo, media.TrackAudio} {
			if track == media.TrackVideo && i >= videoCount || track == media.TrackAudio && i >= audioCount {
				continue
			}
			// config выбирает конфигурацию дорожки. payload содержит её тестовую AU. info
			// задаёт метки и длительность, включая отрицательный начальный AAC.
			config, payload, info := v, tu, media.PacketInfo{PTS: int64(i), DTS: int64(i), DurationTicks: 1, Keyframe: true}
			if track == media.TrackAudio {
				config = a
				payload = []byte{0x21, 0x10, 0x04, 0x60, 0x8c, 0x1c}
				info = media.PacketInfo{PTS: int64(i-1) * 1024, DTS: int64(i-1) * 1024, DurationTicks: 1024}
			}
			// packet содержит текущую тестовую единицу доступа. err сообщает об отказе её
			// создания.
			packet, err := media.NewPacket(info, config, payload)
			if err != nil {
				t.Fatal(err)
			}
			if err = mux.WritePacket(packet); err != nil {
				t.Fatal(err)
			}
		}
	}
	return f
}
