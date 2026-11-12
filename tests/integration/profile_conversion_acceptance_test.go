package integration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

// conversionScenario задаёт независимый временной эталон преобразования AV1 в H.264 с выбранной частотой
// кадров.
type conversionScenario struct {
	// name обозначает сценарий в выводе подтеста.
	name string
	// inputStep задаёт шаг видео в отсчётах 1/60000 секунды; videoOrigin задаёт начало видео в тех
	// же отсчётах; audioOrigin задаёт начало AAC в отсчётах 1/48000 секунды.
	inputStep, videoOrigin, audioOrigin int64
	// outputRate задаёт требуемую рациональную частоту кадров выходного H.264.
	outputRate core.FrameRate
	// firstVideoTick задаёт первый ожидаемый кадр выходной сетки; endVideoTick задаёт исключённую
	// границу её окончания.
	firstVideoTick, endVideoTick int64
	// originShiftSeconds задаёт общий сдвиг OriginMapping в секундах для проверки сохранности
	// знаковых времён.
	originShiftSeconds int64
	// hardware выбирает VideoToolbox вместо CPU-кодировщика и соответствующие ожидания B-кадров и
	// диагностики.
	hardware bool
}

// Создаёт FFmpeg-источник AV1 1440p60 со стереофоническим AAC 48 kHz и проверяет наличие 24
// видеокадров и 16 AAC AU. Возвращает демультиплексированные пакеты и конфигурации для независимого
// назначения временных меток.
func conversionFixture(t *testing.T, binary string) ([]media.Packet, fmp4.Init) {
	t.Helper()
	// source содержит настоящий короткий MP4 с AV1 1440p60 и стереофоническим AAC 48 кГц.
	source := mediaCommand(t, binary, nil,
		"-hide_banner", "-loglevel", "warning", "-xerror",
		"-f", "lavfi", "-i", "testsrc2=size=2560x1440:rate=60:duration=0.4",
		"-f", "lavfi", "-i", "sine=frequency=750:sample_rate=48000:duration=0.32",
		"-map", "0:v", "-map", "1:a", "-c:v", "libaom-av1", "-threads:v", "2", "-cpu-used", "8", "-usage", "realtime", "-lag-in-frames", "0", "-g:v", "12", "-crf", "50",
		"-c:a", "aac", "-b:a", "128000", "-ac:a", "2", "-ar:a", "48000",
		"-f", "mp4", "-movflags", "delay_moov+default_base_moof+frag_every_frame", "-min_frag_duration", "1", "-use_editlist", "1", "-movie_timescale", "240000", "-avoid_negative_ts", "disabled", "pipe:1")
	// resolver проверяет формат видео фикстуры и создаёт конфигурации её дорожек для чтения пакетов.
	resolver := func(d fmp4.TrackDescription) (media.CodecConfig, error) {
		// Разбирает AAC штатным способом и проверяет AV1 1440p перед привязкой видеоконфигурации тестового
		// источника.

		if d.Track == media.TrackAudio {
			return fmp4.ResolveCodecConfig(d, 51, 1)
		}
		if d.Codec != core.CodecAV1 || d.Width != 2560 || d.Height != 1440 {
			return media.CodecConfig{}, errors.New("фикстура потеряла AV1 1440p")
		}
		return media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: 51, Revision: 1, TimeBase: d.TimeBase,
			Video: core.VideoFormat{Codec: core.CodecAV1, Width: 2560, Height: 1440, FrameRate: core.FrameRate{Numerator: 60, Denominator: 1}, CodecProfile: "main", PixelFormat: "yuv420p", BitDepth: 8, Color: core.ColorInfo{Range: "limited", Primaries: "unspecified", Transfer: "unspecified", Matrix: "unspecified"}}}, d.Headers)
	}
	// d читает созданный MP4 с проверенной конфигурацией; err сообщает об ошибке создания
	// демультиплексора.
	d, err := fmp4.NewDemuxer(bytes.NewReader(source), fmp4.DemuxOptions{Limits: integrationLimits(), SourceEpoch: 51, VideoRevision: 1, AudioRevision: 1, ResolveConfig: resolver})
	if err != nil {
		t.Fatal(err)
	}
	// packets содержит исходные AU для проверки их количества и назначения временных меток сценария.
	packets := readMediaPackets(t, d)
	// video считает исходные видеокадры; audio считает исходные AAC AU для проверки состава
	// фикстуры.
	video, audio := 0, 0
	// p представляет исходный AU, учитываемый по типу дорожки.
	for _, p := range packets {
		if p.Config().Info().Track == media.TrackVideo {
			video++
		} else {
			audio++
		}
	}
	if video != 24 || audio != 16 {
		t.Fatalf("фикстура должна содержать 24 AV1 кадра и 16 AAC AU: %d/%d", video, audio)
	}
	return packets, d.Init()
}

// Создаёт новые пакеты с прежними полезными нагрузками и заданной сценарием сеткой видео и AAC.
// Настраивает временные базы и общий OriginMapping, сохраняя исходные пакеты неизменными.
func scheduledConversionInput(t *testing.T, source []media.Packet, original fmp4.Init, scenario conversionScenario) ([]media.Packet, fmp4.Init) {
	t.Helper()
	// init содержит копию исходных конфигураций дорожек с временными базами и общим сдвигом
	// сценария.
	init := original
	// info временно хранит параметры конфигурации дорожки для переназначения временной базы и
	// частоты кадров.
	info := init.Video.Info()
	info.TimeBase = media.TimeBase{Numerator: 1, Denominator: 60000}
	info.Video.FrameRate = core.FrameRate{Numerator: 60000, Denominator: uint32(scenario.inputStep)}
	// err сообщает об ошибках создания изменённых конфигураций видео и аудио.
	var err error
	init.Video, err = media.NewCodecConfig(info, init.Video.Headers())
	if err != nil {
		t.Fatal(err)
	}
	info = init.Audio.Info()
	info.TimeBase = media.TimeBase{Numerator: 1, Denominator: 48000}
	init.Audio, err = media.NewCodecConfig(info, init.Audio.Headers())
	if err != nil {
		t.Fatal(err)
	}
	init.Origin = fmp4.OriginMapping{OffsetTicks: scenario.originShiftSeconds, TimeBase: media.TimeBase{Numerator: 1, Denominator: 1}}
	// video считает назначенные видеокадры для их сетки; audio считает назначенные AAC AU для шага
	// 1024 сэмпла.
	video, audio := int64(0), int64(0)
	// packets накапливает новые пакеты с временными метками сценария и прежними полезными
	// нагрузками.
	packets := make([]media.Packet, 0, len(source))
	// p представляет исходный AU, которому назначается новое время без изменения его содержимого.
	for _, p := range source {
		// config выбирает конфигурацию дорожки нового пакета; stamp хранит изменяемую копию его
		// временных меток.
		config, stamp := init.Video, p.Info()
		if p.Config().Info().Track == media.TrackVideo {
			stamp.PTS, stamp.DTS, stamp.DurationTicks = scenario.videoOrigin+video*scenario.inputStep, scenario.videoOrigin+video*scenario.inputStep, scenario.inputStep
			video++
		} else {
			config = init.Audio
			stamp.PTS, stamp.DTS, stamp.DurationTicks = scenario.audioOrigin+audio*1024, scenario.audioOrigin+audio*1024, 1024
			audio++
		}
		// packet содержит AU с назначенной сеткой сценария; err сообщает об ошибке проверки
		// нового пакета.
		packet, err := media.NewPacket(stamp, config, p.Payload())
		if err != nil {
			t.Fatal(err)
		}
		packets = append(packets, packet)
	}
	return packets, init
}

// Переводит знаковые метки дорожки в рациональные секунды для точного сравнения временных шкал.
func conversionTimeline(t *testing.T, ticks int64, base media.TimeBase) *big.Rat {
	t.Helper()
	return new(big.Rat).Mul(new(big.Rat).SetInt64(ticks), new(big.Rat).SetFrac64(int64(base.Numerator), int64(base.Denominator)))
}

// Сверяет H.264 1080p и AAC с заданными сценарием числом пакетов, PTS/DTS, длительностями и началами и
// концами дорожек. Отдельно учитывает B-кадры, округление FPS и начальную задержку AAC, чтобы
// обнаружить раздельное обнуление A/V.
func assertConversionTimeline(t *testing.T, packets []media.Packet, init fmp4.Init, scenario conversionScenario) {
	t.Helper()
	// video содержит фактический выходной формат для проверки H.264 1080p и заданного FPS.
	video := init.Video.Info().Video
	if video.Codec != core.CodecH264 || video.Width != 1920 || video.Height != 1080 || !video.FrameRate.Equal(scenario.outputRate) {
		t.Fatalf("не тот выходной формат: %+v", video)
	}
	// audio содержит фактический выходной формат для проверки AAC 48 кГц и двух каналов.
	if audio := init.Audio.Info().Audio; audio.Codec != core.CodecAAC || audio.SampleRateHz != 48000 || audio.Channels != 2 {
		t.Fatalf("не тот аудиопрофиль: %+v", audio)
	}
	// videos собирает выходные видеопакеты; audios собирает выходные AAC-пакеты для отдельных
	// временных проверок.
	var videos, audios []media.Packet
	// p представляет выходной пакет, распределяемый по видео- или аудиодорожке.
	for _, p := range packets {
		if p.Config().Info().Track == media.TrackVideo {
			videos = append(videos, p)
		} else {
			audios = append(audios, p)
		}
	}
	// wantVideoCount задаёт ожидаемое число кадров между границами выходной сетки сценария.
	wantVideoCount := int(scenario.endVideoTick - scenario.firstVideoTick)
	if len(videos) != wantVideoCount || len(audios) != 17 {
		t.Fatalf("число пакетов не совпало с заданным входом: video=%d/%d audio=%d/17", len(videos), wantVideoCount, len(audios))
	}
	// frameBase выражает длительность одного кадра заданной выходной частоты.
	frameBase := media.TimeBase{Numerator: scenario.outputRate.Denominator, Denominator: scenario.outputRate.Numerator}
	// presentation собирает точные PTS видео в секундах для проверки порядка показа независимо от
	// DTS.
	var presentation []*big.Rat
	// bframes показывает, встретились ли разные PTS и DTS, необходимые для проверки CPU-кодирования
	// с B-кадрами.
	bframes := false
	// lastDTS хранит предыдущее время декодирования видео для проверки строгого возрастания.
	var lastDTS *big.Rat
	// p представляет видеопакет, чьи времена и длительность сверяются с эталоном.
	for _, p := range videos {
		// stamp содержит временные метки текущего видеопакета; base задаёт единицу этих меток.
		stamp, base := p.Info(), p.Config().Info().TimeBase
		// pts выражает время показа видео точной дробью секунд; dts выражает время декодирования
		// в тех же единицах.
		pts, dts := conversionTimeline(t, stamp.PTS, base), conversionTimeline(t, stamp.DTS, base)
		if lastDTS != nil && dts.Cmp(lastDTS) <= 0 || pts.Cmp(dts) < 0 {
			t.Fatal("видеодорожка потеряла монотонный DTS/допустимый CTS", stamp)
		}
		lastDTS, bframes = dts, bframes || pts.Cmp(dts) != 0
		// got содержит фактическую длительность кадра; want задаёт длительность кадра по
		// выходной частоте.
		if got, want := conversionTimeline(t, stamp.DurationTicks, base), conversionTimeline(t, 1, frameBase); got.Cmp(want) != 0 {
			t.Fatalf("длительность кадра %s, требуется %s", got, want)
		}
		presentation = append(presentation, pts)
	}
	if !bframes && !scenario.hardware {
		t.Fatal("фикстура не проверила новые DTS B-кадров")
	}
	slices.SortFunc(presentation, func(a, b *big.Rat) int {
		// Сортирует точные рациональные PTS по времени представления независимо от порядка декодирования
		// B-кадров.
		return a.Cmp(b)
	})
	// i обозначает позицию кадра в порядке показа; pts содержит его фактическое время показа.
	for i, pts := range presentation {
		// want задаёт точный PTS очередного кадра выходной сетки сценария.
		want := conversionTimeline(t, scenario.firstVideoTick+int64(i), frameBase)
		if pts.Cmp(want) != 0 {
			t.Fatalf("PTS кадра %d = %s, требуется %s", i, pts, want)
		}
	}
	// AAC n7.1.5 добавляет initial_padding=1024. Входные 16 AU декодируются
	// в 16384 сэмпла: получаются 17 выходных AU без ресемплинга и без усечения.
	// i обозначает порядковый номер выходного AAC AU; p содержит этот AU для проверки сетки сэмплов.
	for i, p := range audios {
		// stamp содержит времена и длительность AAC-пакета; base задаёт их единицу времени.
		stamp, base := p.Info(), p.Config().Info().TimeBase
		// want задаёт ожидаемое время AAC AU с учётом начальной задержки 1024 сэмпла.
		want := conversionTimeline(t, scenario.audioOrigin-1024+int64(i)*1024, media.TimeBase{Numerator: 1, Denominator: 48000})
		if conversionTimeline(t, stamp.PTS, base).Cmp(want) != 0 || conversionTimeline(t, stamp.DTS, base).Cmp(want) != 0 || conversionTimeline(t, stamp.DurationTicks, base).Cmp(big.NewRat(1024, 48000)) != 0 {
			t.Fatalf("AAC[%d] не сохранил заданную сетку сэмплов и начальную задержку: %+v, требуется %s", i, stamp, want)
		}
	}
	// firstAudio содержит время первого выходного AAC AU до компенсации начальной задержки
	// кодировщика.
	firstAudio := conversionTimeline(t, audios[0].Info().PTS, audios[0].Config().Info().TimeBase)
	// effectiveAudioStart содержит фактическое начало аудиоматериала после учёта начальной задержки
	// AAC.
	effectiveAudioStart := new(big.Rat).Add(firstAudio, big.NewRat(1024, 48000))
	// wantAudioStart задаёт независимое начало аудиоматериала по исходному сценарию.
	wantAudioStart := big.NewRat(scenario.audioOrigin, 48000)
	if effectiveAudioStart.Cmp(wantAudioStart) != 0 {
		t.Fatal("начальная задержка AAC изменила эффективное начало дорожки")
	}
	// firstVideo содержит минимальный PTS выходного видео после сортировки кадров по показу.
	firstVideo := presentation[0]
	// quantization измеряет сдвиг первого видеокадра относительно заданного входного начала для
	// проверки предела половины кадра.
	quantization := new(big.Rat).Sub(firstVideo, big.NewRat(scenario.videoOrigin, 60000))
	if new(big.Rat).Abs(quantization).Cmp(new(big.Rat).Quo(conversionTimeline(t, 1, frameBase), big.NewRat(2, 1))) > 0 {
		t.Fatal("начало видеодорожки ушло более чем на половину кадра")
	}
	// wantAVStart задаёт ожидаемую разницу начал видео и аудио без раздельного обнуления дорожек.
	wantAVStart := new(big.Rat).Sub(conversionTimeline(t, scenario.firstVideoTick, frameBase), wantAudioStart)
	if new(big.Rat).Sub(firstVideo, effectiveAudioStart).Cmp(wantAVStart) != 0 {
		t.Fatal("раздельное обнуление дорожек изменило A/V origin")
	}
	// lastAudio содержит последний выходной AAC AU для вычисления окончания аудиодорожки.
	lastAudio := audios[len(audios)-1]
	// audioEnd выражает фактическое окончание последнего AAC AU в секундах.
	audioEnd := conversionTimeline(t, lastAudio.Info().PTS+lastAudio.Info().DurationTicks, lastAudio.Config().Info().TimeBase)
	// wantAudioEnd задаёт ожидаемое окончание 16384 входных аудиосэмплов относительно сценарного
	// начала.
	wantAudioEnd := big.NewRat(scenario.audioOrigin+16384, 48000)
	if audioEnd.Cmp(wantAudioEnd) != 0 {
		t.Fatalf("конец AAC=%s, требуется %s", audioEnd, wantAudioEnd)
	}
	// wantVideoEnd задаёт ожидаемую исключённую границу выходной видеосетки в секундах.
	wantVideoEnd := conversionTimeline(t, scenario.endVideoTick, frameBase)
	// actualVideoEnd содержит окончание последнего показанного видеокадра с учётом его длительности.
	actualVideoEnd := new(big.Rat).Add(presentation[len(presentation)-1], conversionTimeline(t, 1, frameBase))
	if actualVideoEnd.Cmp(wantVideoEnd) != 0 {
		t.Fatal("неверное округлённое окончание видеодорожки")
	}
	if new(big.Rat).Sub(actualVideoEnd, audioEnd).Cmp(new(big.Rat).Sub(wantVideoEnd, wantAudioEnd)) != 0 {
		t.Fatal("потеряно заданное соотношение концов A/V-дорожек")
	}
}

// Декодирует полные видео- и аудиоединицы доступа с сохранением знаковых меток и проверяет framemd5.
// Ожидаемые сетка видеокадров, начальная задержка AAC и количество сэмплов берутся из сценария.
func assertDecodedConversion(t *testing.T, binary string, wire []byte, scenario conversionScenario) {
	t.Helper()
	// Проверяем полные AU со знаковыми метками: обычный декодер MOV обрезает начало по шкале воспроизведения.
	// decoded содержит framemd5 полного декодирования AU с сохранением знаковых меток и начальной
	// задержки AAC.
	decoded := mediaCommand(t, binary, wire, "-hide_banner", "-loglevel", "warning", "-xerror", "-copyts", "-flags2:a", "+skip_manual", "-i", "pipe:0", "-map", "0:v", "-map", "0:a", "-fps_mode", "passthrough", "-f", "framemd5", "pipe:1")
	// counts раздельно считает декодированные кадры видео и аудио по индексам дорожек.
	counts := [2]int{}
	// samples считает длительности уже обработанных аудиокадров в сэмплах для проверки следующего
	// PTS.
	samples := int64(0)
	// bases связывает индекс декодированной дорожки с её временной базой из заголовка framemd5.
	bases := map[int]string{}
	// line представляет строку framemd5 с временной базой либо данными декодированного кадра.
	for line := range strings.SplitSeq(string(decoded), "\n") {
		if strings.HasPrefix(line, "#tb ") {
			// index принимает индекс дорожки из объявления временной базы framemd5.
			var index int
			// base принимает текст временной базы декодированной дорожки из заголовка
			// framemd5.
			var base string
			// err сообщает об ошибке разбора объявления временной базы framemd5.
			if _, err := fmt.Sscanf(line, "#tb %d: %s", &index, &base); err != nil {
				t.Fatal(err)
			}
			bases[index] = base
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		// fields содержит колонки контрольной суммы кадра для извлечения индекса дорожки, PTS и
		// длительности.
		fields := strings.Split(line, ",")
		if len(fields) < 6 {
			t.Fatal("неполный framemd5", line)
		}
		// index определяет дорожку текущего декодированного кадра; err сообщает об ошибке
		// разбора её номера.
		index, err := strconv.Atoi(strings.TrimSpace(fields[0]))
		if err != nil || index < 0 || index > 1 {
			t.Fatal("неизвестная дорожка", line)
		}
		// pts содержит знаковое время показа декодированного кадра в единицах его дорожки.
		pts, err := strconv.ParseInt(strings.TrimSpace(fields[2]), 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		// duration содержит длительность декодированного кадра, используемую для подсчёта
		// аудиосэмплов.
		duration, err := strconv.ParseInt(strings.TrimSpace(fields[3]), 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			// want задаёт точное время показа очередного декодированного видеокадра по
			// сценарию.
			want := big.NewRat((scenario.firstVideoTick+int64(counts[0]))*int64(scenario.outputRate.Denominator), int64(scenario.outputRate.Numerator))
			if seconds(t, pts, bases[index]).Cmp(want) != 0 {
				t.Fatal("декодирование изменило заданные метки представления видео", line, want)
			}
		} else {
			// want задаёт точное время очередного аудиокадра с учётом задержки AAC и уже
			// учтённых сэмплов.
			want := big.NewRat(scenario.audioOrigin-1024+samples, 48000)
			if seconds(t, pts, bases[index]).Cmp(want) != 0 {
				t.Fatal("декодирование изменило начало AAC или начальную задержку", line, want)
			}
			samples += duration
		}
		counts[index]++
	}
	if counts[0] != int(scenario.endVideoTick-scenario.firstVideoTick) || counts[1] != 17 || samples != 17408 {
		t.Fatalf("число декодированных кадров и сэмплов: %v, сэмплов=%d", counts, samples)
	}
}

// Запускает ограниченную попытку Supervisor с настоящим обменом fMP4 через UDS и выбранным CPU либо
// VideoToolbox-кодировщиком. После физического завершения проверяет диагностику времени и удаление
// UDS, возвращая пакеты, конфигурации и копию выходного контейнера.
func runConversionWorker(t *testing.T, binary string, packets []media.Packet, init fmp4.Init, scenario conversionScenario) ([]media.Packet, fmp4.Init, []byte) {
	t.Helper()
	// base задаёт частный короткий каталог UDS для настоящей попытки FFmpeg; err сообщает об ошибке
	// его создания.
	base, err := os.MkdirTemp("/tmp", "gpc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Удаляет частный каталог UDS после завершения преобразования.

		// err сообщает об ошибке удаления частного каталога UDS после проверки.
		if err := os.RemoveAll(base); err != nil {
			t.Error(err)
		}
	})
	// s владеет ограниченной попыткой FFmpeg и её соединениями UDS.
	s, err := ffmpeg.New(ffmpeg.Config{BaseDir: base, StartupTimeout: 10 * time.Second, VideoTimeout: 5 * time.Second, AudioTimeout: 5 * time.Second, WatchdogInterval: 50 * time.Millisecond, MaxRestarts: 0, RestartBackoff: 10 * time.Millisecond, GracePeriod: 100 * time.Millisecond, KillTimeout: time.Second, ShutdownTimeout: 3 * time.Second, DiagnosticEvents: 1024, DiagnosticBytes: 1 << 20, DiagnosticChunkBytes: 256, MaxCommandBytes: 8192, MaxCommandArgs: 128})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Останавливает супервизор с отдельным сроком перед удалением каталога.

		// ctx ограничивает окончательную остановку супервизора четырьмя секундами; cancel
		// освобождает таймер этого срока.
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		// err сообщает об ошибке остановки супервизора при очистке теста.
		if err := s.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	// returned собирает все выходные пакеты преобразования для независимой проверки времён.
	var returned []media.Packet
	// resultInit хранит конфигурации фактически прочитанного выходного MP4.
	var resultInit fmp4.Init
	// out сохраняет выходные байты MP4 с пределом 16 МиБ для независимого ffprobe и декодирования.
	out := &boundedMediaBuffer{max: 16 << 20}
	// inputWire сохраняет входные байты MP4 с пределом 16 МиБ для диагностики отказов
	// преобразования.
	inputWire := &boundedMediaBuffer{max: 16 << 20}
	// input обслуживает входной UDS: проверяет времена, передаёт пакеты и завершает направление
	// записи.
	input := func(ctx context.Context, c net.Conn, _ ffmpeg.Progress) error {
		// Проверяет временные метки и передаёт fMP4, сохраняя копию входных байтов и закрывая направление
		// записи после всех пакетов.

		// mux пишет входной fMP4 одновременно в UDS и диагностическую копию; err сообщает об
		// ошибке создания мультиплексора.
		mux, err := fmp4.NewMuxer(io.MultiWriter(c, inputWire), init, integrationLimits())
		if err != nil {
			return err
		}
		// validator проверяет допустимость времён входных пакетов с учётом общего OriginMapping.
		validator := fmp4.NewFFmpegTimingValidatorForOrigin(init.Origin)
		// packet представляет очередной входной AU, передаваемый настоящему FFmpeg.
		for _, packet := range packets {
			// err сообщает об ошибке временной проверки входного пакета перед отправкой
			// FFmpeg.
			if err := validator.Validate(packet); err != nil {
				return err
			}
			// err сообщает об ошибке записи входного пакета в fMP4 и UDS.
			if err := mux.WritePacket(packet); err != nil {
				return err
			}
		}
		// u предоставляет закрытие направления записи UDS; ok подтверждает ожидаемый тип
		// Unix-соединения.
		u, ok := c.(*net.UnixConn)
		if !ok {
			return errors.New("ожидался UnixConn")
		}
		// err сообщает об ошибке завершения записи входного fMP4 через UDS.
		if err := u.CloseWrite(); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	}
	// output обслуживает выходной UDS: сохраняет контейнер, читает пакеты и сообщает прогресс каждой
	// дорожки.
	output := func(_ context.Context, c net.Conn, progress ffmpeg.Progress) error {
		// Читает выходной fMP4 с пределом числа пакетов, сохраняет байты и сообщает прогресс обеих дорожек.

		// d читает фактический выходной MP4 с сохранением его байтов; err сообщает об ошибке
		// создания демультиплексора.
		d, err := fmp4.NewDemuxer(io.TeeReader(c, out), fmp4.DemuxOptions{Limits: integrationLimits(), SourceEpoch: 52, VideoRevision: 1, AudioRevision: 1})
		if err != nil {
			return err
		}
		resultInit = d.Init()
		for {
			// p содержит очередной выходной AU; err сообщает об окончании контейнера либо
			// ошибке чтения.
			p, err := d.Next()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			if len(returned) >= 128 {
				return errors.New("превышен бюджет выходных пакетов")
			}
			returned = append(returned, p)
			// track выбирает аудио- или видеосчётчик прогресса супервизора для выходного
			// пакета.
			track := ffmpeg.Video
			if p.Config().Info().Track == media.TrackAudio {
				track = ffmpeg.Audio
			}
			progress(track, p.Info().DTS)
		}
	}
	// command создаёт CPU- или VideoToolbox-команду преобразования с временными шкалами выбранного
	// сценария.
	command := func(_ context.Context, a ffmpeg.Attempt) (ffmpeg.Command, error) {
		// Создаёт преобразование частоты кадров и размера с выбранным CPU-кодировщиком либо VideoToolbox и
		// явными временными шкалами.

		// filter задаёт округление FPS, масштабирование до 1080p и формат пикселей для выходного
		// видео.
		filter := fmt.Sprintf("fps=fps=%d/%d:round=near:eof_action=round,scale=w=1920:h=1080:flags=bilinear,format=yuv420p", scenario.outputRate.Numerator, scenario.outputRate.Denominator)
		// args задаёт прямые аргументы FFmpeg для преобразования с сохранением общего времени
		// аудио и видео.
		args := []string{"-hide_banner", "-nostdin", "-loglevel", "warning", "-xerror", "-copyts", "-flags2:a", "+skip_manual", "-f", "mp4", "-fflags", "+ignidx", "-i", "async:unix:" + a.InputPath, "-map", "0:v", "-map", "0:a", "-vf", filter}
		if scenario.hardware {
			// VideoToolbox не сообщает VUI timing; фиксируем профиль в SPS,
			// а фактическую сетку независимо проверяем по всем PTS и длительностям.
			// vui задаёт параметры временной сетки SPS H.264 для выходного
			// VideoToolbox-профиля.
			vui := fmt.Sprintf("h264_metadata=tick_rate=%d/%d", 2*scenario.outputRate.Numerator, scenario.outputRate.Denominator)
			args = append(args, "-c:v", "h264_videotoolbox", "-allow_sw", "0", "-b:v", "3000000", "-g:v", "12", "-bf:v", "0", "-bsf:v", vui)
		} else {
			args = append(args, "-c:v", "libx264", "-threads:v", "1", "-preset", "ultrafast", "-g:v", "12", "-bf:v", "2")
		}
		args = append(args, "-fps_mode:v", "passthrough", "-enc_time_base:v", "filter", "-c:a", "aac", "-b:a", "64000", "-ar:a", "48000", "-ac:a", "2", "-f", "mp4", "-movflags", "delay_moov+default_base_moof+frag_every_frame", "-min_frag_duration", "1", "-use_editlist", "1", "-movie_timescale", "240000", "-video_track_timescale", strconv.FormatUint(uint64(scenario.outputRate.Numerator), 10), "-avoid_negative_ts", "disabled", "unix:"+a.OutputPath)
		return ffmpeg.Command{Binary: binary, Args: args}, nil
	}
	// ctx ограничивает всю попытку преобразования 25 секундами; cancel освобождает её контекст и
	// таймер.
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	// err сообщает об ошибке запуска настоящей попытки преобразования через супервизор.
	if err := s.Start(ctx, ffmpeg.Worker{SessionID: "conversion-session", GroupID: "conversion-group", Owner: integrationOwner{make(chan struct{})}, Command: command, Input: input, Output: output}); err != nil {
		t.Fatal(err)
	}
	// result содержит физический исход попытки и подтверждение очистки после ожидания супервизора.
	result, err := s.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	// diagnostics накапливает stderr завершённой попытки для проверки недопустимых исправлений
	// времени.
	var diagnostics []byte
	for {
		// event содержит очередное диагностическое событие попытки; err сообщает об окончании
		// либо ошибке его чтения.
		event, err := s.NextDiagnostic(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if event.Kind == "stderr" {
			diagnostics = append(diagnostics, event.Data...)
		}
	}
	if result.Err != nil || result.CleanupErr != nil || !result.Clean || result.Attempts != 1 {
		// probe содержит независимую диагностику входных AAC-пакетов при неуспешном
		// преобразовании.
		probe := mediaCommand(t, filepath.Join(filepath.Dir(binary), "ffprobe"), inputWire.Bytes(), "-v", "error", "-select_streams", "a", "-show_packets", "-show_entries", "packet=pts,dts,duration,side_data_list", "-of", "json", "pipe:0")
		t.Logf("AAC на входе FFmpeg: %s", probe)
		t.Fatalf("преобразование через Supervisor/UDS: %+v: %s", result, diagnostics)
	}
	// initialWarningCount считает разрешённые точные начальные предупреждения x264 в диагностике
	// попытки.
	initialWarningCount, err := conversionTimingDiagnostics(diagnostics, scenario)
	if err != nil {
		t.Fatal(err)
	}
	if initialWarningCount == 1 {
		t.Logf("x264 b35605a сравнил первый отрицательный PTS с начальным i_largest_pts=-1; полная сетка проверяется отдельно: %s", diagnostics)
	}
	// entries содержит остаток частного каталога после завершения попытки для проверки удаления UDS.
	entries, err := os.ReadDir(base)
	if err != nil || len(entries) != 0 {
		t.Fatal("UDS не удалены после join", entries, err)
	}
	return returned, resultInit, bytes.Clone(out.Bytes())
}

// initialX264PTSWarning распознаёт только точную строку начального предупреждения libx264 с адресом
// экземпляра кодировщика.
var initialX264PTSWarning = regexp.MustCompile(`^\[libx264 @ 0x[0-9a-fA-F]+\] non-strictly-monotonic PTS$`)

// Отклоняет сообщения о недопустимом времени, кроме одной точной строки libx264 non-strictly-monotonic
// PTS в CPU-сценарии с отрицательным первым PTS. Возвращает число разрешённых предупреждений и ошибку
// при любом другом совпадении.
func conversionTimingDiagnostics(diagnostics []byte, scenario conversionScenario) (int, error) {
	// warnings считает единственное допустимое начальное предупреждение x264, чтобы отклонять
	// повторы.
	warnings := 0
	// line представляет очередную строку диагностики FFmpeg для проверки временных предупреждений.
	for line := range strings.SplitSeq(string(diagnostics), "\n") {
		// lower содержит строку диагностики в нижнем регистре для поиска сообщений о
		// недопустимом времени.
		lower := strings.ToLower(line)
		if !strings.Contains(lower, "non-monotonic") && !strings.Contains(lower, "non-strictly-monotonic") && !strings.Contains(lower, "invalid dts") {
			continue
		}
		// В x264 b35605a i_largest_pts начинается с -1, затем безусловно принимает PTS.
		// Только при первом отрицательном PTS на CPU допускается эта точная строка один раз.
		if !scenario.hardware && scenario.firstVideoTick < 0 && initialX264PTSWarning.MatchString(line) && warnings == 0 {
			warnings++
			continue
		}
		return warnings, fmt.Errorf("FFmpeg сообщил недопустимую диагностику времени: %s", diagnostics)
	}
	return warnings, nil
}

// Проверяет узкое исключение для единственного начального предупреждения x264 и отклонение повторов,
// иных кодировщиков, положительных меток и сообщений об исправлении DTS.
func TestConversionTimingDiagnosticsRejectsTimestampRepairs(t *testing.T) {
	// warning задаёт точный образец допустимого предупреждения x264 для отрицательного первого PTS.
	warning := "[libx264 @ 0x123ab] non-strictly-monotonic PTS\n"
	// negative задаёт CPU-сценарий с отрицательным первым кадром для проверки узкого исключения
	// диагностики.
	negative := conversionScenario{firstVideoTick: -4}
	// tc представляет случай допустимой либо отвергаемой диагностики времени FFmpeg.
	for _, tc := range []struct {
		// name обозначает проверяемый случай классификации диагностики в выводе подтеста.
		name string
		// diagnostics содержит строки FFmpeg, на которых проверяется допустимость временных
		// предупреждений.
		diagnostics string
		// scenario задаёт знак первого кадра и выбор кодировщика, влияющие на разрешённое
		// исключение.
		scenario conversionScenario
		// wantCount задаёт ожидаемое число принятых начальных предупреждений x264.
		wantCount int
		// wantError задаёт ожидаемый отказ при недопустимой диагностике времени.
		wantError bool
	}{
		{name: "clean_negative", scenario: negative},
		{name: "first_negative_x264", diagnostics: warning, scenario: negative, wantCount: 1},
		{name: "duplicate_x264", diagnostics: warning + warning, scenario: negative, wantCount: 1, wantError: true},
		{name: "positive_x264", diagnostics: warning, scenario: conversionScenario{firstVideoTick: 4}, wantError: true},
		{name: "hardware_x264", diagnostics: warning, scenario: conversionScenario{firstVideoTick: -4, hardware: true}, wantError: true},
		{name: "other_encoder", diagnostics: "[h264_videotoolbox @ 0x123ab] non-strictly-monotonic PTS\n", scenario: negative, wantError: true},
		{name: "different_message", diagnostics: "[libx264 @ 0x123ab] non-strictly-monotonic PTS repaired\n", scenario: negative, wantError: true},
		{name: "invalid_dts", diagnostics: warning + "[mp4 @ 0x123ab] Invalid DTS: 1 PTS: 0\n", scenario: negative, wantCount: 1, wantError: true},
		{name: "nonmonotonic_dts", diagnostics: warning + "[mp4 @ 0x123ab] Non-monotonic DTS; changing to 1\n", scenario: negative, wantCount: 1, wantError: true},
		{name: "lowercase_dts", diagnostics: "[mp4 @ 0x123ab] non-monotonic dts\n", scenario: negative, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет допустимость выбранной диагностики времени, включая единственное начальное предупреждение
			// x264 для отрицательного PTS.

			// count содержит число принятых предупреждений; err сообщает об обнаруженной
			// недопустимой диагностике времени.
			count, err := conversionTimingDiagnostics([]byte(tc.diagnostics), tc.scenario)
			if (err != nil) != tc.wantError || count != tc.wantCount {
				t.Fatalf("классификация диагностики: count=%d err=%v, требуется count=%d error=%v", count, err, tc.wantCount, tc.wantError)
			}
		})
	}
}

// При заданном GOST_MEDIA_FFMPEG требует FFmpeg 7.1.5 и проверяет преобразование AV1 1440p в H.264
// 1080p с целыми и рациональными FPS и знаковыми началами A/V. Независимо сверяет пакеты,
// декодирование и ffprobe; VideoToolbox добавляется только при GOST_MEDIA_VIDEOTOOLBOX=1.
func TestAcceptanceProfileConversionCPUAndOptionalVideoToolbox(t *testing.T) {
	// binary задаёт явно выбранный FFmpeg для дополнительной интеграции преобразования профиля.
	binary := os.Getenv("GOST_MEDIA_FFMPEG")
	if binary == "" {
		t.Skip("локальное преобразование требует явно заданного GOST_MEDIA_FFMPEG")
	}
	// version содержит вывод FFmpeg для проверки версии 7.1.5, которой соответствуют временные
	// эталоны.
	version := mediaCommand(t, binary, nil, "-version")
	if !strings.HasPrefix(string(version), "ffmpeg version 7.1.5 ") {
		t.Fatalf("временные эталоны проверены для FFmpeg 7.1.5: %s", strings.SplitN(string(version), "\n", 2)[0])
	}
	// packets содержит проверенную исходную AV1/AAC-фикстуру; init содержит её конфигурации для
	// сценарного назначения времён.
	packets, init := conversionFixture(t, binary)
	// scenarios задаёт целые и рациональные FPS, положительные и отрицательные начала и
	// необязательный аппаратный путь.
	scenarios := []conversionScenario{
		{name: "cpu_60_to_30_positive", inputStep: 1000, videoOrigin: 7000, audioOrigin: 6000, outputRate: core.FrameRate{Numerator: 30, Denominator: 1}, firstVideoTick: 4, endVideoTick: 16},
		{name: "cpu_60_to_30_positive_shifted", inputStep: 1000, videoOrigin: 7000, audioOrigin: 6000, outputRate: core.FrameRate{Numerator: 30, Denominator: 1}, firstVideoTick: 4, endVideoTick: 16, originShiftSeconds: 1},
		{name: "cpu_60_to_30_negative", inputStep: 1000, videoOrigin: -7000, audioOrigin: -6000, outputRate: core.FrameRate{Numerator: 30, Denominator: 1}, firstVideoTick: -4, endVideoTick: 9, originShiftSeconds: 1},
		{name: "cpu_rational", inputStep: 1001, videoOrigin: 7007, audioOrigin: 6000, outputRate: core.FrameRate{Numerator: 30000, Denominator: 1001}, firstVideoTick: 4, endVideoTick: 16},
	}
	if os.Getenv("GOST_MEDIA_VIDEOTOOLBOX") == "1" {
		scenarios = append(scenarios, conversionScenario{name: "videotoolbox_60_to_30", inputStep: 1000, videoOrigin: 7000, audioOrigin: 6000, outputRate: core.FrameRate{Numerator: 30, Denominator: 1}, firstVideoTick: 4, endVideoTick: 16, hardware: true})
	}
	// scenario представляет проверяемый эталон преобразования и его ожидаемые временные границы.
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			// Проверяет заданные сетки PTS, DTS, длительностей и A/V после выбранного преобразования через UDS и
			// независимое декодирование.

			// input содержит пакеты с временами текущего сценария; inputInit содержит
			// согласованные временные базы и общий сдвиг.
			input, inputInit := scheduledConversionInput(t, packets, init, scenario)
			// returned содержит реальные выходные пакеты; outputInit содержит их
			// конфигурации; wire сохраняет выходной контейнер для независимых проверок.
			returned, outputInit, wire := runConversionWorker(t, binary, input, inputInit, scenario)
			assertConversionTimeline(t, returned, outputInit, scenario)
			assertDecodedConversion(t, binary, wire, scenario)
			assertProbePackets(t, probeMedia(t, filepath.Join(filepath.Dir(binary), "ffprobe"), wire), returned)
		})
	}
}
