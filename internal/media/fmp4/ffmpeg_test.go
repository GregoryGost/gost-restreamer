package fmp4

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
)

// probeStream хранит сведения одной дорожки из JSON ffprobe для восстановления формата и независимой
// проверки времени.
type probeStream struct {
	// Index связывает описание дорожки с индексом её пакетов в ответе ffprobe.
	Index int
	// CodecName сохраняет имя кодека, сообщённое ffprobe в описании дорожки.
	CodecName string `json:"codec_name"`
	// CodecType задаёт наблюдаемую роль дорожки video либо audio для сопоставления пакетов.
	CodecType string `json:"codec_type"`
	// TimeBase содержит рациональную длительность отсчёта ffprobe в строковой форме N/D.
	TimeBase string `json:"time_base"`
	// Width и Height содержат наблюдаемые ширину и высоту видео в пикселях.
	Width, Height uint32
	// Profile сохраняет сообщённое ffprobe название профиля кодека.
	Profile string
	// PixelFormat сохраняет наблюдаемый формат пикселей для полного описания видео.
	PixelFormat string `json:"pix_fmt"`
	// FrameRate содержит наблюдаемую среднюю частоту кадров как строковую дробь.
	FrameRate string `json:"avg_frame_rate"`
	// SampleRate содержит сообщённую ffprobe частоту дискретизации аудио строкой в герцах.
	SampleRate string `json:"sample_rate"`
	// Channels содержит наблюдаемое число аудиоканалов.
	Channels uint16
	// ChannelLayout сохраняет наблюдаемую схему расположения аудиоканалов.
	ChannelLayout string `json:"channel_layout"`
	// ColorRange содержит обозначение диапазона цветовых значений, сообщённое ffprobe; пустое
	// значение при восстановлении формата означает unspecified.
	ColorRange string `json:"color_range"`
	// ColorPrimaries сохраняет сообщённое ffprobe обозначение основных цветов.
	ColorPrimaries string `json:"color_primaries"`
	// ColorTransfer сохраняет наблюдаемое обозначение передаточной характеристики цвета.
	ColorTransfer string `json:"color_transfer"`
	// ColorSpace сохраняет наблюдаемое обозначение матрицы цветового преобразования.
	ColorSpace string `json:"color_space"`
}

// probePacket хранит исходные временные метки и принадлежность пакета дорожке по независимому выводу
// ffprobe.
type probePacket struct {
	// StreamIndex связывает пакет с соответствующей записью Streams и её TimeBase.
	StreamIndex int `json:"stream_index"`
	// PTS и DTS содержат знаковые времена показа и декодирования в отсчётах соответствующей дорожки
	// ffprobe.
	PTS, DTS int64
	// Flags сохраняет строку флагов пакета из ffprobe; сравнение временных меток её не использует.
	Flags string
}

// probeResult объединяет наблюдаемые дорожки и пакеты одного файла для независимого сравнения с Go.
type probeResult struct {
	// Streams содержит описания дорожек, их шкалы и параметры формата из ffprobe.
	Streams []probeStream
	// Packets содержит пакеты в порядке вывода ffprobe до разделения по дорожкам.
	Packets []probePacket
}

// cappedBuffer сохраняет вывод тестовой медиакоманды с отказом записи при превышении заданного объёма.
type cappedBuffer struct {
	// Buffer хранит уже принятые байты вывода команды.
	bytes.Buffer
	// limit задаёт максимальное число принимаемых байтов; превышающая остаток запись отклоняется
	// целиком.
	limit int
}

// Отклоняет запись целиком при превышении тестового лимита буфера вывода, иначе сохраняет все
// переданные байты.
func (b *cappedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, ErrLimit
	}
	return b.Buffer.Write(p)
}

// Запускает медиакоманду с переданным вводом, сроком 20 секунд и ограниченными stdout/stderr; ошибка
// или сообщения о переписывании DTS завершают тест.
func runMediaCommand(t *testing.T, path string, input []byte, args ...string) []byte {
	t.Helper()
	// ctx задаёт срок отмены одной медиакоманды; cancel освобождает её таймер после возврата.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// cmd содержит запускаемую медиакоманду с контекстом отмены и заданными аргументами.
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Stdin = bytes.NewReader(input)
	// out принимает ограниченный стандартный вывод команды, возвращаемый вызывающему тесту.
	out := &cappedBuffer{limit: 16 << 20}
	// stderr принимает ограниченную диагностику команды для объяснения отказа и поиска переписывания
	// DTS.
	stderr := &cappedBuffer{limit: 1 << 20}
	cmd.Stdout = out
	cmd.Stderr = stderr
	// err содержит результат запуска и ожидания медиакоманды, при отказе которого тест завершается с
	// диагностикой.
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s: %v: %s", filepath.Base(path), err, stderr.String())
	}
	if strings.Contains(stderr.String(), "Invalid DTS") || strings.Contains(stderr.String(), "Non-monotonic") {
		t.Fatalf("переписаны временные метки: %s", stderr.String())
	}
	return out.Bytes()
}

// Получает потоки и пакеты через ffprobe в JSON и разбирает их в независимый эталон времени.
func probeMedia(t *testing.T, path string, input []byte) probeResult {
	t.Helper()
	// data содержит JSON описаний дорожек и пакетов, полученный от ffprobe через ограниченный
	// запуск.
	data := runMediaCommand(t, path, input, "-v", "error", "-show_streams", "-show_packets", "-of", "json", "pipe:0")
	// result принимает независимое описание дорожек и временных меток из JSON ffprobe.
	var result probeResult
	// err сообщает о неверном JSON ffprobe либо несовместимом представлении его полей.
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

// Разбирает строку N/D в два uint32 для точного сравнения шкал; неверная запись завершает тест.
func rationalBase(t *testing.T, value string) media.TimeBase {
	t.Helper()
	// parts содержит числитель и знаменатель строковой рациональной шкалы.
	parts := strings.Split(value, "/")
	if len(parts) != 2 {
		t.Fatal(value)
	}
	// n содержит числитель шкалы, помещающийся в uint32; err сообщает об отказе его числового
	// разбора.
	n, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	// d содержит знаменатель шкалы, помещающийся в uint32.
	d, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	return media.TimeBase{Numerator: uint32(n), Denominator: uint32(d)}
}

// Строит конфигурацию из фактических сведений ffprobe и заголовков контейнера, назначая SourceEpoch=41
// и Revision=1; отсутствие дорожки возвращает ErrMetadataUnavailable.
func observedConfig(t *testing.T, streams []probeStream, desc TrackDescription) (media.CodecConfig, error) {
	t.Helper()
	// s обозначает очередное наблюдаемое описание ffprobe, сопоставляемое с ролью записи MP4.
	for _, s := range streams {
		if s.CodecType != string(desc.Track) {
			continue
		}
		// info накапливает полный формат по фактическим сведениям ffprobe и заголовкам
		// контейнера с тестовой эпохой 41 и ревизией 1.
		info := media.CodecConfigInfo{Track: desc.Track, SourceEpoch: 41, Revision: 1, TimeBase: desc.TimeBase}
		if desc.Track == media.TrackVideo {
			// fps содержит точную дробь наблюдаемой частоты кадров для переноса в
			// VideoFormat.
			fps := rationalBase(t, s.FrameRate)
			// color преобразует отсутствующее строковое свойство цвета в unspecified,
			// сохраняя непустое значение ffprobe.
			color := func(s string) string {
				// Заменяет отсутствующее строковое свойство цвета значением unspecified, сохраняя непустое наблюдаемое
				// значение.

				if s == "" {
					return "unspecified"
				}
				return s
			}
			info.Video = core.VideoFormat{Codec: desc.Codec, Width: s.Width, Height: s.Height, FrameRate: core.FrameRate{Numerator: fps.Numerator, Denominator: fps.Denominator}, CodecProfile: s.Profile, PixelFormat: s.PixelFormat, BitDepth: 8, Color: core.ColorInfo{Range: color(s.ColorRange), Primaries: color(s.ColorPrimaries), Transfer: color(s.ColorTransfer), Matrix: color(s.ColorSpace)}}
		} else {
			// rate содержит частоту дискретизации аудио в герцах; err сообщает об отказе её
			// разбора из строки ffprobe.
			rate, err := strconv.ParseUint(s.SampleRate, 10, 32)
			if err != nil {
				return media.CodecConfig{}, err
			}
			info.Audio = core.AudioFormat{Codec: core.CodecAAC, CodecProfile: s.Profile, SampleRateHz: uint32(rate), Channels: s.Channels, ChannelLayout: s.ChannelLayout}
		}
		return media.NewCodecConfig(info, desc.Headers)
	}
	return media.CodecConfig{}, ErrMetadataUnavailable
}

// Преобразует отсчёты и TimeBase в точное рациональное число секунд без округления.
func timingValue(v int64, b media.TimeBase) *big.Rat {
	return new(big.Rat).Mul(new(big.Rat).SetInt64(v), new(big.Rat).SetFrac64(int64(b.Numerator), int64(b.Denominator)))
}

// TestFFmpegCPUFragmentedRoundTrip при заданном GOST_FMP4_FFMPEG проверяет цепочку FFmpeg →
// Demuxer → Muxer → FFmpeg для H.264 и AV1 с AAC и положительным либо отрицательным началом.
// Сверяет число пакетов и точные рациональные PTS/DTS с ffprobe, сохранность заголовков и наличие
// декодированных кадров в выводе framemd5; равенство контрольных сумм кадров здесь не проверяется.
func TestFFmpegCPUFragmentedRoundTrip(t *testing.T) {
	// ffmpeg задаёт явно разрешённый путь к медиапрограмме; пустое значение оставляет CPU-интеграцию
	// выключенной.
	ffmpeg := os.Getenv("GOST_FMP4_FFMPEG")
	if ffmpeg == "" {
		t.Skip("локальная CPU-интеграция не включена")
	}
	// ffprobe указывает на программу наблюдения рядом с выбранным FFmpeg.
	ffprobe := filepath.Join(filepath.Dir(ffmpeg), "ffprobe")
	// scenario выбирает кодек видео и положительное либо отрицательное начало временной шкалы теста.
	for _, scenario := range []string{"h264", "av1", "h264-negative", "av1-negative"} {
		t.Run(scenario, func(t *testing.T) {
			// Для выбранных кодека и знака начала сверяет PTS/DTS с ffprobe при разборе, повторной упаковке и
			// передаче через FFmpeg, затем проверяет декодирование результата.

			// codec содержит выбранный по имени сценария кодек h264 либо av1.
			codec, _, _ := strings.Cut(scenario, "-")
			// sign задаёт знак смещения, добавляемого фильтрами к видео- и аудиовремени
			// синтетического источника.
			sign := "+"
			if strings.HasSuffix(scenario, "-negative") {
				sign = "-"
			}
			// encoder содержит параметры соответствующего CPU-кодировщика для построения
			// исходного fMP4.
			encoder := []string{"-c:v", "libx264", "-threads:v", "1", "-bf", "2", "-g", "15"}
			if codec == "av1" {
				encoder = []string{"-c:v", "libaom-av1", "-threads:v", "1", "-cpu-used", "8", "-lag-in-frames", "0"}
			}
			// args накапливает аргументы синтетических источников, сдвига времени,
			// кодирования и фрагментированного вывода.
			args := []string{"-hide_banner", "-loglevel", "warning", "-xerror", "-copyts", "-f", "lavfi", "-i", "testsrc2=size=128x128:rate=30:duration=0.4", "-f", "lavfi", "-i", "sine=frequency=1000:sample_rate=48000:duration=0.4", "-map", "0:v", "-map", "1:a", "-vf", "setpts=PTS" + sign + "15", "-af", "asetpts=PTS" + sign + "6000", "-fps_mode", "passthrough"}
			args = append(args, encoder...)
			args = append(args, "-c:a", "aac", "-f", "mp4", "-movflags", "delay_moov+default_base_moof+frag_every_frame", "-min_frag_duration", "1", "-use_editlist", "1", "-movie_timescale", "48000", "-avoid_negative_ts", "disabled", "pipe:1")
			// source содержит исходный fMP4 с видео и AAC, созданный выбранным сценарием
			// FFmpeg.
			source := runMediaCommand(t, ffmpeg, nil, args...)
			// oracle хранит независимые сведения ffprobe о формате и временных метках
			// исходного файла.
			oracle := probeMedia(t, ffprobe, source)
			if sign == "-" {
				// negative отмечает наличие хотя бы одного отрицательного DTS,
				// обязательное для сценария с отрицательным началом.
				negative := false
				// p обозначает очередной пакет ffprobe, проверяемый на отрицательное
				// время декодирования.
				for _, p := range oracle.Packets {
					negative = negative || p.DTS < 0
				}
				if !negative {
					t.Fatal("FFmpeg не сохранил отрицательное начало теста")
				}
			}
			if codec == "h264" {
				// production содержит разборщик со штатным восстановлением H.264/AAC из
				// VUI и ASC; err сообщает об отказе этой инициализации.
				production, err := NewDemuxer(bytes.NewReader(source), DemuxOptions{Limits: testLimits(), SourceEpoch: 51, VideoRevision: 2, AudioRevision: 3})
				if err != nil {
					t.Fatal(err)
				}
				if production.Init().Video.Info().Video.FrameRate != (core.FrameRate{Numerator: 30, Denominator: 1}) || production.Init().Audio.Info().Audio.SampleRateHz != 48000 {
					t.Fatal("VUI/ASC не восстановлены")
				}
			}
			// d содержит разборщик короткими чтениями с восстановлением по ffprobe; err
			// сообщает об отказе чтения инициализации.
			d, err := NewDemuxer(shortReader{bytes.NewReader(source), 5}, DemuxOptions{Limits: testLimits(), ResolveConfig: func(desc TrackDescription) (media.CodecConfig, error) {
				// Восстанавливает конфигурацию дорожки из заголовков контейнера и независимо полученных сведений
				// ffprobe исходного файла.
				return observedConfig(t, oracle.Streams, desc)
			}})
			if err != nil {
				t.Fatal(err)
			}
			// packets накапливает все пакеты, восстановленные Go из исходного fMP4, для
			// последующих точных сравнений.
			var packets []media.Packet
			for {
				// p содержит очередной пакет исходного файла; err отличает окончание EOF
				// от отказа разбора.
				p, err := d.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				packets = append(packets, p)
			}
			comparePackets(t, oracle, packets)
			// init хранит восстановленные конфигурации, к которым перед повторной упаковкой
			// добавляется общий трёхсекундный сдвиг.
			init := d.Init()
			init.Origin = OriginMapping{OffsetTicks: 3, TimeBase: media.TimeBase{Numerator: 1, Denominator: 1}}
			// output накапливает байты повторно упакованного Go-транспорта.
			var output bytes.Buffer
			// m содержит мультиплексор, пишущий повторный транспорт короткими порциями в
			// output.
			m, err := NewMuxer(shortWriter{&output, 7}, init, testLimits())
			if err != nil {
				t.Fatal(err)
			}
			// v проверяет представимость каждого пакета на порте FFmpeg с выбранным общим
			// сдвигом.
			v := NewFFmpegTimingValidatorForOrigin(init.Origin)
			// p обозначает очередной восстановленный пакет для проверки времени и повторной
			// упаковки.
			for _, p := range packets {
				if err = v.Validate(p); err != nil {
					t.Fatal(err)
				}
				if err = m.WritePacket(p); err != nil {
					t.Fatal(err)
				}
			}
			// remuxed содержит независимые сведения ffprobe о повторно упакованном Go-файле.
			remuxed := probeMedia(t, ffprobe, output.Bytes())
			comparePackets(t, remuxed, packets)
			// movieScale задаёт точную общую шкалу двух дорожек при повторном контейнерном
			// обмене через FFmpeg.
			movieScale, err := MovieTimeScale(init.Video.Info().TimeBase, init.Audio.Info().TimeBase)
			if err != nil {
				t.Fatal(err)
			}
			// transported содержит результат передачи Go-файла через FFmpeg без
			// перекодирования аудио и видео.
			transported := runMediaCommand(t, ffmpeg, output.Bytes(), "-hide_banner", "-loglevel", "warning", "-xerror", "-copyts", "-i", "pipe:0", "-map", "0:v", "-map", "0:a", "-c", "copy", "-f", "mp4", "-movflags", "delay_moov+default_base_moof+frag_every_frame", "-min_frag_duration", "1", "-use_editlist", "1", "-movie_timescale", strconv.FormatUint(uint64(movieScale), 10), "-avoid_negative_ts", "disabled", "pipe:1")
			comparePackets(t, probeMedia(t, ffprobe, transported), packets)
			// returned содержит разборщик транспорта после FFmpeg для проверки возвращённых
			// заголовков и пакетов.
			returned, err := NewDemuxer(bytes.NewReader(transported), DemuxOptions{Limits: testLimits(), ResolveConfig: func(desc TrackDescription) (media.CodecConfig, error) {
				// Восстанавливает конфигурацию вернувшейся дорожки по её заголовкам и исходным сведениям ffprobe для
				// сравнения после FFmpeg.
				return observedConfig(t, oracle.Streams, desc)
			}})
			if err != nil {
				t.Fatal(err)
			}
			// initialDescriptions хранит исходные описания дорожек, returnedDescriptions —
			// их описания после контейнерного обмена через FFmpeg.
			initialDescriptions, returnedDescriptions := d.Descriptions(), returned.Descriptions()
			// index выбирает соответствующее возвращённое описание; desc содержит исходное
			// описание для сравнения точных байтов заголовков.
			for index, desc := range initialDescriptions {
				if !bytes.Equal(desc.Headers, returnedDescriptions[index].Headers) {
					t.Fatalf("FFmpeg изменил extradata %s", desc.Track)
				}
			}
			// returnedPackets накапливает пакеты после полного прохода Go и FFmpeg для
			// сравнения с исходным эталоном.
			var returnedPackets []media.Packet
			for {
				// p содержит очередной вернувшийся пакет; err отличает EOF от ошибки
				// повторного разбора.
				p, err := returned.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				returnedPackets = append(returnedPackets, p)
			}
			comparePackets(t, oracle, returnedPackets)
			// decoded содержит вывод успешного декодирования в framemd5 для проверки наличия
			// декодированных данных.
			decoded := runMediaCommand(t, ffmpeg, transported, "-hide_banner", "-loglevel", "warning", "-xerror", "-i", "pipe:0", "-map", "0:v", "-map", "0:a", "-f", "framemd5", "pipe:1")
			if bytes.Count(decoded, []byte("\n")) < 20 {
				t.Fatal("недостаточно декодированных кадров")
			}
		})
	}
}

// Сверяет число пакетов и точные рациональные PTS/DTS каждой дорожки с ffprobe независимо от порядка
// перемежения дорожек.
func comparePackets(t *testing.T, oracle probeResult, packets []media.Packet) {
	t.Helper()
	// byTrack группирует ожидаемые пакеты ffprobe по роли дорожки, сохраняя порядок внутри каждой.
	byTrack := map[media.Track][]probePacket{}
	// bases хранит независимую рациональную шкалу каждой дорожки ffprobe.
	bases := map[media.Track]media.TimeBase{}
	// indices связывает числовой индекс дорожки ffprobe с ролью audio либо video.
	indices := map[int]media.Track{}
	// s обозначает очередное описание ffprobe, из которого берутся индекс, роль и шкала дорожки.
	for _, s := range oracle.Streams {
		// track содержит роль наблюдаемой дорожки для заполнения карт индексов и шкал.
		track := media.Track(s.CodecType)
		indices[s.Index] = track
		bases[track] = rationalBase(t, s.TimeBase)
	}
	// p обозначает очередной пакет ffprobe, добавляемый в эталон своей дорожки.
	for _, p := range oracle.Packets {
		// track определяет роль пакета по его индексу в выводе ffprobe.
		track := indices[p.StreamIndex]
		byTrack[track] = append(byTrack[track], p)
	}
	// seen считает уже сопоставленные Go-пакеты отдельно для каждой дорожки.
	seen := map[media.Track]int{}
	// p обозначает очередной Go-пакет, чьи точные PTS и DTS сравниваются с независимым эталоном.
	for _, p := range packets {
		// track выбирает последовательность эталонных пакетов и счётчик текущей дорожки.
		track := p.Config().Info().Track
		// index задаёт порядковый номер сравниваемого пакета внутри его дорожки.
		index := seen[track]
		seen[track]++
		if index >= len(byTrack[track]) {
			t.Fatal("лишний пакет")
		}
		// expected содержит соответствующий пакет ffprobe с эталонными PTS и DTS.
		expected := byTrack[track][index]
		// base задаёт длительность отсчёта временных меток сравниваемого Go-пакета.
		base := p.Config().Info().TimeBase
		// info содержит исходные PTS и DTS Go-пакета до перевода в рациональные секунды.
		info := p.Info()
		if timingValue(info.PTS, base).Cmp(timingValue(expected.PTS, bases[track])) != 0 || timingValue(info.DTS, base).Cmp(timingValue(expected.DTS, bases[track])) != 0 {
			t.Fatalf("%s[%d]: Go PTS/DTS=%s/%s, ffprobe=%s/%s", track, index, timingValue(info.PTS, base), timingValue(info.DTS, base), timingValue(expected.PTS, bases[track]), timingValue(expected.DTS, bases[track]))
		}
	}
	// track обозначает проверяемую дорожку, expected содержит полный список её ожидаемых пакетов для
	// итоговой сверки количества.
	for track, expected := range byTrack {
		if seen[track] != len(expected) {
			t.Fatalf("число %s: %d != %d", track, seen[track], len(expected))
		}
	}
}
