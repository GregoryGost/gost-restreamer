package fallback

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

// probeJSONRunner подставляет JSON метаданных или выбранную ошибку вместо запуска ffprobe.
type probeJSONRunner struct {
	// data содержит JSON, записываемый получателю тестовой команды.
	data []byte
	// err при наличии задаёт отказ, возвращаемый до записи JSON.
	err error
}

// Проверяет отказ Source и Verify для неизвестной или чрезмерной длительности, размеров, частоты и
// неподдержанных кодеков, а также предел числа пакетов.
func TestSourceAndVerificationRejectUnmeasuredOrExcessiveMetadata(t *testing.T) {
	// scenario выбирает поле метаданных для замены недопустимым значением.
	for _, scenario := range []struct { /* name обозначает нарушение в имени подтеста. old содержит заменяемый фрагмент эталонного JSON. new содержит недопустимую подстановку. */
		name, old, new string
	}{
		{"duration-unknown", "1.600000", "N/A"},
		{"duration-negative", "1.600000", "-1"},
		{"duration-limit", "1.600000", "61"},
		{"width-limit", "\"width\":128", "\"width\":5000"},
		{"height-empty", "\"height\":128", "\"height\":0"},
		{"fps-unknown", "30/1", "0/0"},
		{"unsupported-pixel", "yuv420p", "yuv420p10le"},
		{"audio-rate-invalid", "\"48000\"", "\"invalid\""},
		{"audio-codec-invalid", "\"aac\"", "\"mp3\""},
		{"video-codec-invalid", "\"h264\"", "\"vp9\""},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// Подменяет выбранное поле метаданных недопустимым значением и требует отказа как проверки источника,
			// так и проверки результата.

			// p содержит проверяющий порт с повреждёнными метаданными выбранного сценария.
			p := Prober{Binary: "probe", Runner: probeJSONRunner{data: []byte(strings.ReplaceAll(selfProbeJSON, scenario.old, scenario.new))}, Limits: selfProbeLimits()}
			// f содержит контрольный fMP4 с допустимыми пакетами.
			f := selfFragmented(t, "valid")
			// err содержит результат проверки источника с недопустимыми метаданными.
			if _, err := p.Source(context.Background(), f, core.AssetVideo); err == nil {
				t.Fatal("непригодный источник принят")
			}
			// err содержит результат проверки варианта с недопустимыми метаданными.
			if _, err := p.Verify(context.Background(), f, selfProfile(core.CodecH264)); err == nil {
				t.Fatal("непригодный результат принят")
			}
		})
	}
	// p содержит проверяющий порт, лимит пакетов которого уменьшается до единицы.
	p := Prober{Binary: "probe", Runner: probeJSONRunner{data: []byte(selfProbeJSON)}, Limits: selfProbeLimits()}
	p.Limits.MaxPackets = 1
	// profile содержит ожидаемый H.264-формат с цветом, соответствующим тестовому SPS.
	profile := selfProfile(core.CodecH264)
	profile.Format.Video.Color = core.ColorInfo{Range: "limited", Primaries: "unspecified", Transfer: "unspecified", Matrix: "unspecified"}
	// err содержит результат проверки файла при превышенном количестве пакетов.
	if _, err := p.Verify(context.Background(), selfFragmented(t, "valid"), profile); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
}

// Проверяет приём MOV/Matroska с одной видеодорожкой и отказ лишних, неизвестных или отсутствующих
// дорожек и неподдержанного контейнера.
func TestSourceRequiresOneVideoAndAtMostOneAudioInSupportedContainer(t *testing.T) {
	// video содержит JSON допустимой видеодорожки H.264 с нулевым началом.
	video := `{"codec_type":"video","start_time":"0.000000","codec_name":"h264","profile":"High","width":128,"height":128,"avg_frame_rate":"30/1","pix_fmt":"yuv420p","level":11}`
	// audio содержит JSON допустимой стереофонической AAC-дорожки с нулевым началом.
	audio := `{"codec_type":"audio","start_time":"0.000000","codec_name":"aac","profile":"LC","sample_rate":"48000","channels":2,"channel_layout":"stereo"}`
	// valid содержит JSON допустимого MOV с обеими дорожками.
	valid := []byte(`{"streams":[` + video + `,` + audio + `],"format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2","duration":"1.6"}}`)
	// p содержит проверяющий порт для исходного допустимого MOV.
	p := Prober{Binary: "probe", Runner: probeJSONRunner{data: valid}, Limits: selfProbeLimits()}
	// s содержит проверенное описание видеоисточника. err сообщает об отказе его определения.
	s, err := p.Source(context.Background(), selfFragmented(t, "valid"), core.AssetVideo)
	if err != nil || s.Demuxer != "mov" || s.Format.Video == nil || s.Format.Audio == nil {
		t.Fatalf("допустимые video+audio с нулевым началом отклонены: %+v, %v", s, err)
	}
	// scenario выбирает недопустимый состав дорожек или неподдержанный контейнер.
	for _, scenario := range []struct { /* name обозначает нарушение в имени подтеста. streams содержит список JSON дорожек. container содержит имя контейнера для проверки. */
		name, streams, container string
	}{
		{"no-video", audio, "mov,mp4,m4a,3gp,3g2,mj2"},
		{"duplicate-video", video + "," + video, "mov,mp4,m4a,3gp,3g2,mj2"},
		{"too-many-tracks", video + "," + audio + "," + audio, "mov,mp4,m4a,3gp,3g2,mj2"},
		{"unknown-track", video + `,{"codec_type":"subtitle","start_time":"0.000000"}`, "mov,mp4,m4a,3gp,3g2,mj2"},
		{"external-playlist", video, "hls"},
		{"empty", "", "mov,mp4,m4a,3gp,3g2,mj2"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// Проверяет отказ при выбранном составе дорожек или неподдержанном контейнере из таблицы.

			// data содержит полный JSON выбранного недопустимого контейнера.
			data := []byte(`{"streams":[` + scenario.streams + `],"format":{"format_name":"` + scenario.container + `","duration":"1.6"}}`)
			// p содержит проверяющий порт с метаданными выбранного сценария.
			p := Prober{Binary: "probe", Runner: probeJSONRunner{data: data}, Limits: selfProbeLimits()}
			// err содержит результат проверки источника с недопустимым контейнером или
			// набором дорожек.
			if _, err := p.Source(context.Background(), selfFragmented(t, "valid"), core.AssetVideo); err == nil {
				t.Fatal("непригодный контейнер принят")
			}
		})
	}
	// matroska содержит проверяющий порт с теми же дорожками в контейнере Matroska.
	matroska := Prober{Binary: "probe", Runner: probeJSONRunner{data: []byte(strings.Replace(selfProbeJSON, "mov,mp4,m4a,3gp,3g2,mj2", "matroska,webm", 1))}, Limits: selfProbeLimits()}
	// s содержит описание источника Matroska. err сообщает об отказе его проверки.
	if s, err := matroska.Source(context.Background(), selfFragmented(t, "valid"), core.AssetVideo); err != nil || s.Demuxer != "matroska" {
		t.Fatal(s, err)
	}
}

// Run проверяет тестовую команду и входной дескриптор, затем возвращает управляемую ошибку либо
// записывает заданный JSON.
func (r probeJSONRunner) Run(_ context.Context, c Command, input *os.File, out io.Writer) (RunResult, error) {
	if input == nil || c.Binary != "probe" {
		return RunResult{}, ErrInvalid
	}
	if r.err != nil {
		return RunResult{}, r.err
	}
	// err сообщает об отказе записи управляемого JSON.
	_, err := out.Write(r.data)
	return RunResult{}, err
}

// selfProbeJSON содержит метаданные H.264 128×128/30 и стереофонического AAC 48 кГц для контрольных
// fMP4.
const selfProbeJSON = `{"streams":[{"codec_type":"video","start_time":"0.000000","codec_name":"h264","profile":"High","width":128,"height":128,"avg_frame_rate":"30/1","pix_fmt":"yuv420p","level":11,"color_range":"tv","color_primaries":"unspecified","color_transfer":"unspecified","color_space":"unspecified"},{"codec_type":"audio","start_time":"0.000000","codec_name":"aac","profile":"LC","sample_rate":"48000","channels":2,"channel_layout":"stereo"}],"format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2","duration":"1.600000"}}`

// Создаёт временный fMP4 с H.264/AAC и управляемым дефектом независимости, NAL, длительности или
// аудиодорожки; файл закрывается после теста.
func selfFragmented(t *testing.T, scenario string) *os.File {
	t.Helper()
	// headers содержит фиксированный avcC с SPS и PPS контрольного видео. err сообщает о
	// некорректной шестнадцатеричной записи.
	headers, err := hex.DecodeString("0164000bffe100186764000bacd942046c0440000003004000000f03c50a658001000668ebe3cb22c0fdf8f800")
	if err != nil {
		t.Fatal(err)
	}
	// p содержит профиль контрольного H.264 с цветом, соответствующим SPS.
	p := selfProfile(core.CodecH264)
	p.Format.Video.Color = core.ColorInfo{Range: "limited", Primaries: "unspecified", Transfer: "unspecified", Matrix: "unspecified"}
	// v содержит видеоконфигурацию контрольного файла со шкалой 1/30 секунды.
	v, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: 1, Revision: 1, TimeBase: media.TimeBase{Numerator: 1, Denominator: 30}, Video: p.Format.Video}, headers)
	if err != nil {
		t.Fatal(err)
	}
	// a содержит AAC-конфигурацию со шкалой 1/48000 секунды.
	a, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackAudio, SourceEpoch: 1, Revision: 1, TimeBase: media.TimeBase{Numerator: 1, Denominator: 48000}, Audio: p.Format.Audio}, []byte{0x11, 0x90})
	if err != nil {
		t.Fatal(err)
	}
	// out накапливает байты контрольного fMP4.
	var out bytes.Buffer
	// mux записывает контрольные пакеты видео и аудио в память.
	mux, err := fmp4.NewMuxer(&out, fmp4.Init{Video: v, Audio: a}, selfProbeLimits().Container)
	if err != nil {
		t.Fatal(err)
	}
	// i задаёт номер очередной из трёх пар контрольных пакетов.
	for i := range 3 {
		// payload содержит IDR H.264 либо его намеренно повреждённый вариант.
		payload := []byte{0, 0, 0, 3, 0x65, 0xb8, 0x40}
		if scenario == "false-key" {
			payload[4] = 0x41
		}
		if scenario == "bad-nal" {
			payload = []byte{0, 0, 0, 99, 0x65}
		}
		// duration задаёт длительность текущего видео- или аудиопакета, изменяемую отрицательным
		// сценарием.
		duration := int64(1)
		if scenario == "video-duration" {
			duration = 2
		}
		// packet содержит очередной контрольный пакет. err сообщает об отказе его создания.
		packet, err := media.NewPacket(media.PacketInfo{PTS: int64(i), DTS: int64(i), DurationTicks: duration, Keyframe: true}, v, payload)
		if err != nil {
			t.Fatal(err)
		}
		if err = mux.WritePacket(packet); err != nil {
			t.Fatal(err)
		}
		if scenario == "no-audio" {
			continue
		}
		duration = 1024
		if scenario == "audio-duration" {
			duration = 1000
		}
		// au содержит полный AAC AU либо подставленную запрещённую запись ADTS.
		au := []byte{0x21, 0x10, 0x04, 0x60, 0x8c, 0x1c}
		if scenario == "adts" {
			au = []byte{0xff, 0xf1, 0, 0}
		}
		packet, err = media.NewPacket(media.PacketInfo{PTS: int64(i) * 1024, DTS: int64(i) * 1024, DurationTicks: duration}, a, au)
		if err != nil {
			t.Fatal(err)
		}
		if err = mux.WritePacket(packet); err != nil {
			t.Fatal(err)
		}
	}
	// f содержит временный файл с построенным контрольным fMP4.
	f, err := os.CreateTemp(t.TempDir(), "variant")
	if err != nil {
		t.Fatal(err)
	}
	selfClose(t, f)
	if _, err = f.Write(out.Bytes()); err != nil {
		t.Fatal(err)
	}
	return f
}

// Проверяет успешный разбор контрольного fMP4 и отказ при ложном ключевом флаге, испорченном NAL,
// неверной длительности, отсутствии аудио и ADTS.
func TestProbeRequiresActualIndependentAUsAndCompleteDurations(t *testing.T) {
	// scenario выбирает допустимый файл либо конкретное нарушение сжатых данных и длительностей.
	for _, scenario := range []string{"valid", "false-key", "bad-nal", "video-duration", "audio-duration", "no-audio", "adts"} {
		t.Run(scenario, func(t *testing.T) {
			// Проверяет фактические AU и длительности выбранного файла: допускается только valid, остальные
			// сценарии должны завершиться ошибкой.

			// f содержит контрольный fMP4 выбранного сценария.
			f := selfFragmented(t, scenario)
			// p содержит ожидаемый профиль с цветом контрольного SPS.
			p := selfProfile(core.CodecH264)
			p.Format.Video.Color = core.ColorInfo{Range: "limited", Primaries: "unspecified", Transfer: "unspecified", Matrix: "unspecified"}
			// probe содержит проверяющий порт с фиксированным JSON и настоящим разбором
			// fMP4.
			probe := Prober{Binary: "probe", Runner: probeJSONRunner{data: []byte(selfProbeJSON)}, Limits: selfProbeLimits()}
			// actual содержит наблюдаемый формат проверенного файла. err сообщает о
			// нарушении данных или контракта профиля.
			actual, err := probe.Verify(context.Background(), f, p)
			if scenario == "valid" {
				if err != nil || actual.Video.Width != 128 {
					t.Fatal(actual, err)
				}
			} else if err == nil {
				t.Fatalf("некорректный файл принят: %s", scenario)
			}
		})
	}
}

// Проверяет корректный Source и отказ при превышении JSON, повреждённых метаданных, ошибке процесса и
// неверной рациональной частоте.
func TestProbeSourceJSONAndLimitsFailClosed(t *testing.T) {
	// f содержит допустимый контрольный fMP4 для отрицательных сценариев JSON.
	f := selfFragmented(t, "valid")
	// p содержит проверяющий порт, у которого последовательно меняются бюджет и результат ffprobe.
	p := Prober{Binary: "probe", Runner: probeJSONRunner{data: []byte(selfProbeJSON)}, Limits: selfProbeLimits()}
	// s содержит исходно допустимое описание источника. err сообщает об отказе проверки.
	s, err := p.Source(context.Background(), f, core.AssetVideo)
	if err != nil || s.Duration <= 0 || s.Demuxer != "mov" || s.Format.Audio == nil {
		t.Fatal(s, err)
	}
	p.Limits.MaxJSONBytes = 2
	if _, err = p.Source(context.Background(), f, core.AssetVideo); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
	p.Limits = selfProbeLimits()
	p.Runner = probeJSONRunner{data: []byte("{")}
	if _, err = p.Source(context.Background(), f, core.AssetVideo); err == nil {
		t.Fatal("повреждённый JSON принят")
	}
	p.Runner = probeJSONRunner{err: io.ErrUnexpectedEOF}
	if _, err = p.Source(context.Background(), f, core.AssetVideo); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal(err)
	}
	// rate содержит очередную некорректную рациональную запись частоты.
	for _, rate := range []string{"", "0/1", "1/0", "1/2/3", "x/2", "2/x"} {
		if _, err = parseRate(rate); err == nil {
			t.Fatal(rate)
		}
	}
}
