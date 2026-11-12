package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/fallback"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

// Создаёт ограниченные Runner и Prober с ffprobe рядом с выбранным FFmpeg для коротких CPU-источников.
func liveAudioProber(t *testing.T, executable string) (fallback.Prober, fallback.Runner) {
	t.Helper()
	// runner ограничивает время, аргументы и диагностику настоящих FFmpeg-команд подготовки
	// аудиофикстур.
	runner := fallback.Runner{Limits: fallback.RunnerLimits{Timeout: 10 * time.Second, MaxArgs: 256, MaxCommandBytes: 1 << 16, MaxDiagnosticBytes: 1 << 20}}
	return fallback.Prober{Binary: filepath.Join(filepath.Dir(executable), "ffprobe"), Runner: fallbackPipelineRunner{t: t, runner: runner}, Limits: fallback.ProbeLimits{MaxJSONBytes: 1 << 20, MaxWidth: 1024, MaxHeight: 1024, MaxPixels: 1 << 20, MaxDuration: 10 * time.Second, ImageDuration: 1600 * time.Millisecond, MaxPackets: 4096, Container: integrationLimits()}}, runner
}

// Создаёт PNG либо короткое H.264-видео для подготовки заглушки; video-tone содержит проверенный
// слышимый тон 997 Hz, остальные видео не содержат аудиодорожки.
func liveAudioSourceAsset(t *testing.T, executable, kind string) ([]byte, core.AssetKind) {
	t.Helper()
	if kind == "image" {
		// img содержит цветное изображение 128×128 для проверки генерации заглушки из PNG.
		img := image.NewRGBA(image.Rect(0, 0, 128, 128))
		// y обозначает строку пикселей тестового изображения.
		for y := range 128 {
			// x обозначает столбец пикселей тестового изображения.
			for x := range 128 {
				img.SetRGBA(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 40, A: 255})
			}
		}
		// data накапливает PNG-представление исходного изображения заглушки.
		var data bytes.Buffer
		// err сообщает об ошибке кодирования тестового изображения в PNG.
		if err := png.Encode(&data, img); err != nil {
			t.Fatal(err)
		}
		return data.Bytes(), core.AssetImage
	}
	// args задаёт генерацию короткого H.264-видео с необязательным слышимым тоном AAC.
	args := []string{"-hide_banner", "-loglevel", "error", "-xerror", "-f", "lavfi", "-i", "testsrc2=size=128x128:rate=30:duration=1.6"}
	if kind == "video-tone" {
		args = append(args, "-f", "lavfi", "-i", "sine=frequency=997:sample_rate=48000:duration=1.6")
	}
	args = append(args, "-map", "0:v:0", "-c:v", "libx264", "-threads:v", "1", "-pix_fmt", "yuv420p", "-g:v", "15", "-bf:v", "0")
	if kind == "video-tone" {
		args = append(args, "-map", "1:a:0", "-c:a", "aac", "-b:a", "96000", "-ar:a", "48000", "-ac:a", "2")
	}
	args = append(args, "-f", "mp4", "-movflags", "delay_moov+default_base_moof+frag_every_frame", "pipe:1")
	// data содержит созданное видео для использования в качестве исходника заглушки.
	data := mediaCommand(t, executable, nil, args...)
	if kind == "video-tone" {
		// pcm содержит декодированное аудио исходника с тоном для независимой проверки слышимого
		// сигнала.
		pcm := mediaCommand(t, executable, data, "-v", "error", "-i", "pipe:0", "-map", "0:a:0", "-f", "f32le", "pipe:1")
		// peak хранит максимальную абсолютную амплитуду исходного float32-аудио для проверки
		// наличия тона.
		peak := float64(0)
		// i обозначает смещение очередного четырёхбайтового PCM-сэмпла в декодированном
		// исходнике.
		for i := 0; i+4 <= len(pcm); i += 4 {
			peak = max(peak, math.Abs(float64(math.Float32frombits(binary.LittleEndian.Uint32(pcm[i:])))))
		}
		if peak < 0.02 {
			t.Fatal("Исходное видео не содержит заведомо слышимого тона")
		}
	}
	return data, core.AssetVideo
}

// Проверяет исходник, рассчитывает общий период A/V, кодирует и нормализует вариант с
// CPU-подстановкой, затем проверяет период. Возвращает самостоятельную копию подготовленного fMP4;
// любая ошибка завершает тест.
func liveAudioPrepared(t *testing.T, executable string, profile core.GroupProfile, kind string) []byte {
	t.Helper()
	// data содержит исходные байты изображения или видео; assetKind задаёт соответствующий вид
	// материала заглушки.
	data, assetKind := liveAudioSourceAsset(t, executable, kind)
	// prober проверяет и нормализует материал заглушки; runner выполняет ограниченную CPU-генерацию
	// варианта.
	prober, runner := liveAudioProber(t, executable)
	// source открывает временный исходник для проверки и кодирования; err сообщает об ошибке
	// создания файла.
	source, err := os.CreateTemp(t.TempDir(), "source")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// Закрывает исходный файл после генерации и проверки подготовленного периода.

		// err сообщает об ошибке закрытия исходного материала после подготовки заглушки.
		if err := source.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err = source.Write(data); err != nil {
		t.Fatal(err)
	}
	// observed содержит независимо измеренные свойства исходного материала для выбора периода и
	// команды генерации.
	observed, err := prober.Source(context.Background(), source, assetKind)
	if err != nil {
		t.Fatalf("Проверка исходника: %v", err)
	}
	// period задаёт общий допустимый период аудио и видео подготовленной заглушки.
	period, err := fallback.EffectiveLoopDuration(observed.Duration, profile, prober.Limits.MaxDuration)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = source.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	// command задаёт CPU-генерацию заглушки по измеренному источнику, профилю и общему периоду.
	command, err := encodingPipelineCPUBuilder(executable)(fallback.GenerateRequest{Source: observed, Profile: profile, LoopDuration: period})
	if err != nil {
		t.Fatal(err)
	}
	// encoded сохраняет результат кодирования заглушки с пределом восемь МиБ.
	var encoded boundedMediaBuffer
	encoded.max = 8 << 20
	// result содержит диагностику настоящей команды генерации для объяснения возможного отказа.
	result, err := runner.Run(context.Background(), command, source, &encoded)
	if err != nil {
		t.Fatalf("Генерация: %v %s", err, result.Diagnostics)
	}
	// file открывает промежуточный кодированный материал для нормализации контейнера.
	file, err := os.CreateTemp(t.TempDir(), "encoded")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// Закрывает промежуточный кодированный файл после нормализации.

		// err сообщает об ошибке закрытия промежуточного кодированного файла.
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err = file.Write(encoded.Bytes()); err != nil {
		t.Fatal(err)
	}
	// prepared сохраняет нормализованный fMP4 заглушки с пределом восемь МиБ.
	var prepared boundedMediaBuffer
	prepared.max = 8 << 20
	if err = prober.NormalizePrepared(context.Background(), file, &prepared); err != nil {
		t.Fatalf("Нормализация исходника %s duration=%s period=%s: %v", kind, observed.Duration, period, err)
	}
	// verified открывает нормализованный материал для независимой проверки длительности петли.
	verified, err := os.CreateTemp(t.TempDir(), "prepared")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// Закрывает подготовленный файл после независимой проверки длительности петли.

		// err сообщает об ошибке закрытия файла, использованного для проверки периода заглушки.
		if err := verified.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err = verified.Write(prepared.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err = prober.VerifyLoopDuration(context.Background(), verified, period); err != nil {
		t.Fatalf("Проверка периода исходника %s duration=%s period=%s: %v", kind, observed.Duration, period, err)
	}
	return bytes.Clone(prepared.Bytes())
}

// Подсчитывает ненулевые сэмплы float32, записанные младшим байтом вперёд; неполный завершающий сэмпл
// не учитывается.
func liveAudioNonzeroSamples(pcm []byte) int {
	// n считает ненулевые полные float32-сэмплы для независимой проверки PCM-тишины.
	n := 0
	// i обозначает байтовое смещение полного четырёхбайтового PCM-сэмпла.
	for i := 0; i+4 <= len(pcm); i += 4 {
		if math.Float32frombits(binary.LittleEndian.Uint32(pcm[i:])) != 0 {
			n++
		}
	}
	return n
}

// Оборачивает один AAC AU в ADTS по его ASC. Непредставимые параметры AAC LC и длина кадра завершают
// тест ошибкой.
func liveAudioADTS(t *testing.T, headers, payload []byte) []byte {
	t.Helper()
	if len(headers) < 2 {
		t.Fatal("Нет ASC")
	}
	// object содержит AAC Audio Object Type из ASC для проверки возможности заголовка ADTS LC.
	object := headers[0] >> 3
	// rate содержит индекс частоты дискретизации AAC из ASC для переноса в ADTS.
	rate := (headers[0]&7)<<1 | headers[1]>>7
	// channels содержит конфигурацию каналов AAC из ASC для проверки и записи заголовка ADTS.
	channels := headers[1] >> 3 & 15
	// n задаёт полную длину кадра ADTS вместе с семибайтовым заголовком.
	n := len(payload) + 7
	if object != 2 || rate > 12 || channels == 0 || channels > 7 || n > 8191 {
		t.Fatal("Фикстура не представима как AAC LC ADTS")
	}
	return append([]byte{0xff, 0xf1, (object-1)<<6 | rate<<2 | channels>>2, (channels&3)<<6 | byte(n>>11), byte(n >> 3), byte(n&7)<<5 | 31, 0xfc}, payload...)
}

// Подстановка меняет аппаратные видеопараметры; аудиоплан остаётся результатом рабочего компилятора.
func liveAudioCPUCommand(c ffmpeg.NVIDIAConfig, a ffmpeg.Attempt) (ffmpeg.Command, error) {
	// command содержит рабочий план NVIDIA до CPU-подстановки видеопараметров; err сообщает об
	// ошибке его построения.
	command, err := ffmpeg.BuildNVIDIACommand(c, a)
	if err != nil {
		return command, err
	}
	// args собирает CPU-аргументы, сохраняя аудиоплан рабочего компилятора.
	var args []string
	// beforeInput показывает, относится ли текущий параметр к входу до первого -i.
	beforeInput := true
	// i обозначает позицию аргумента рабочего плана с пропуском удаляемых пар параметр-значение.
	for i := 0; i < len(command.Args); i++ {
		// arg содержит текущий аргумент, преобразуемый из NVIDIA-видеопути в CPU-видеопуть.
		arg := command.Args[i]
		if arg == "-i" {
			beforeInput = false
		}
		if arg == "-hwaccel" || arg == "-hwaccel_output_format" || arg == "-pix_fmt" || arg == "-no-scenecut" || arg == "-forced-idr" || arg == "-rc" || arg == "-preset" || arg == "-tune" || arg == "-multipass" || arg == "-rc-lookahead" || arg == "-spatial-aq" || arg == "-temporal-aq" || arg == "-aq-strength" {
			i++
			continue
		}
		if beforeInput && arg == "-c:v" {
			i++
			continue
		}
		if arg == "h264_nvenc" {
			arg = "libx264"
		}
		if strings.Contains(arg, "scale_cuda=") {
			arg = strings.ReplaceAll(arg, "scale_cuda=", "scale=")
			arg = strings.ReplaceAll(arg, ":format=yuv420p", ",format=yuv420p")
		}
		args = append(args, arg)
	}
	if len(args) == 0 {
		return command, fmt.Errorf("Пустая команда CPU")
	}
	// output сохраняет конечный адрес выходного UDS перед добавлением CPU-параметров кодировщика.
	output := args[len(args)-1]
	args = append(args[:len(args)-1], "-threads:v", "1", "-sc_threshold", "0", "-preset", "veryfast", output)
	command.Args = args
	return command, nil
}

// Извлекает AAC AU из сообщений RTMP и непрерывно декодирует их в float32 PCM, сохраняя копии полезных
// нагрузок. Проверяет наличие ровно 1024 сэмплов на канал для каждого AU.
func liveAudioWirePCM(t *testing.T, executable string, messages []publisherWireMessage, channels int) ([]byte, [][]byte) {
	t.Helper()
	// asc хранит последний AAC AudioSpecificConfig; adts накапливает все AU с заголовками для
	// непрерывного декодирования.
	var asc, adts []byte
	// payloads сохраняет самостоятельные копии принятых AAC-нагрузок для побайтной и звуковой
	// проверки.
	var payloads [][]byte
	// message представляет очередное RTMP-сообщение, из которого извлекается конфигурация или AU
	// AAC.
	for _, message := range messages {
		if message.typ != 8 || len(message.body) < 2 {
			continue
		}
		if message.body[1] == 0 {
			asc = bytes.Clone(message.body[2:])
		}
		if message.body[1] == 1 {
			adts = append(adts, liveAudioADTS(t, asc, message.body[2:])...)
			payloads = append(payloads, bytes.Clone(message.body[2:]))
		}
	}
	// pcm содержит непрерывно декодированное float32-аудио всех принятых AU для проверки 1024
	// сэмплов на канал.
	pcm := mediaCommand(t, executable, adts, "-v", "error", "-xerror", "-flags2:a", "+skip_manual", "-f", "aac", "-i", "pipe:0", "-map", "0:a:0", "-f", "f32le", "pipe:1")
	if len(pcm) != len(payloads)*1024*channels*4 {
		t.Fatalf("Неполные AU после непрерывного декодирования: %d/%d", len(pcm), len(payloads))
	}
	return pcm, payloads
}

// При заданном GOST_LIVE_AUDIO_DIAGNOSTICS_DIR сохраняет ограниченный JSON со штампами, конфигурациями
// и байтами пакетов; без переменной файлов не создаёт.
func liveAudioDumpPackets(t *testing.T, name string, packets []media.Packet) {
	t.Helper()
	// directory задаёт явно выбранный каталог необязательной пакетной диагностики интеграционного
	// теста.
	directory := os.Getenv("GOST_LIVE_AUDIO_DIAGNOSTICS_DIR")
	if directory == "" {
		return
	}
	// record описывает одну диагностическую запись с метками пакета, конфигурацией и полными
	// медиабайтами.
	type record struct {
		// Info сохраняет временные метки и признаки диагностируемого пакета.
		Info media.PacketInfo
		// Config сохраняет параметры дорожки и владельца конфигурации диагностируемого пакета.
		Config media.CodecConfigInfo
		// Headers содержит заголовки кодека; Payload содержит полную полезную нагрузку
		// диагностируемого пакета.
		Headers, Payload []byte
	}
	// records накапливает диагностические представления всех переданных пакетов перед ограниченной
	// JSON-сериализацией.
	var records []record
	// packet представляет медиапакет, чьи метки, конфигурация и байты сохраняются в диагностике.
	for _, packet := range packets {
		records = append(records, record{packet.Info(), packet.Config().Info(), packet.Config().Headers(), packet.Payload()})
	}
	// data содержит JSON пакетной диагностики; err сообщает об ошибке его сериализации.
	data, err := json.Marshal(records)
	if err != nil || len(data) > 16<<20 {
		t.Fatalf("Сериализация ограниченной диагностики: bytes=%d err=%v", len(data), err)
	}
	if err = os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(directory, name+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

// Независимая конечная команда получает те же принятые PTS/DTS, без Group, Supervisor и рабочего компилятора.
func liveAudioTranscodeReference(t *testing.T, executable string, source []media.Packet, profile core.GroupProfile) []media.Packet {
	t.Helper()
	// epochs разделяет принятые входные пакеты по эпохам публикации для независимых эталонных
	// преобразований.
	epochs := map[uint64][]media.Packet{}
	// order сохраняет порядок первого появления эпох публикации во входной последовательности.
	var order []uint64
	// packet представляет входной медиапакет, распределяемый по эпохе источника.
	for _, packet := range source {
		// epoch определяет публикацию-владельца конфигурации текущего пакета.
		epoch := packet.Config().Info().SourceEpoch
		// exists показывает, добавлена ли текущая эпоха в порядок эталонных публикаций.
		if _, exists := epochs[epoch]; !exists {
			order = append(order, epoch)
		}
		epochs[epoch] = append(epochs[epoch], packet)
	}
	// references накапливает эталонные AAC AU независимых преобразований для всех допустимых точек
	// допуска.
	var references []media.Packet
	// referenceID нумерует независимые точки допуска и ограничивает число эталонных преобразований.
	referenceID := uint64(0)
	// epoch выбирает следующую публикацию в порядке её появления на входе.
	for _, epoch := range order {
		// publication содержит пакеты одной эпохи источника для поиска независимых видеоточек.
		publication := epochs[epoch]
		// start обозначает позицию независимого кадра в публикации; independent содержит этот
		// кадр для выбора точки допуска.
		for start, independent := range publication {
			if independent.Config().Info().Track != media.TrackVideo || !independent.Info().Keyframe {
				continue
			}
			// cut задаёт точное время независимого видеокадра, до которого аудио не
			// включается в эталон.
			cut := fallbackActualTime(independent.Info().PTS, independent.Config().Info().TimeBase)
			// packets собирает хвост публикации от выбранного ключевого кадра с аудио,
			// начинающимся не раньше него.
			var packets []media.Packet
			// packet представляет пакет хвоста публикации, проверяемый относительно точки
			// допуска.
			for _, packet := range publication[start:] {
				if packet.Config().Info().Track == media.TrackAudio && fallbackActualTime(packet.Info().PTS, packet.Config().Info().TimeBase).Cmp(cut) < 0 {
					continue
				}
				packets = append(packets, packet)
			}
			referenceID++
			if referenceID > 128 {
				t.Fatal("Превышен конечный бюджет независимых точек допуска")
			}
			// init собирает первые доступные конфигурации видео и аудио выбранного
			// эталонного хвоста.
			var init fmp4.Init
			// packet представляет пакет, из которого извлекаются начальные конфигурации
			// дорожек.
			for _, packet := range packets {
				if packet.Config().Info().Track == media.TrackVideo && init.Video.Validate() != nil {
					init.Video = packet.Config()
				}
				if packet.Config().Info().Track == media.TrackAudio && init.Audio.Validate() != nil {
					init.Audio = packet.Config()
				}
			}
			if init.Audio.Validate() != nil {
				continue
			}
			// input сохраняет входной fMP4 эталонного хвоста с пределом 16 МиБ.
			var input boundedMediaBuffer
			input.max = 16 << 20
			// mux кодирует выбранный хвост публикации во входной fMP4; err сообщает об
			// ошибке создания мультиплексора.
			mux, err := fmp4.NewMuxer(&input, init, integrationLimits())
			if err != nil {
				t.Fatal(err)
			}
			// packet представляет очередной AU эталонного хвоста, записываемый в независимый
			// входной контейнер.
			for _, packet := range packets {
				if err = mux.WritePacket(packet); err != nil {
					t.Fatal(err)
				}
			}
			// v задаёт целевой видеоформат эталона; a задаёт целевой аудиоформат для
			// независимой команды FFmpeg.
			v, a := profile.Format.Video, profile.Format.Audio
			// n преобразует численные параметры целевого профиля в десятичные аргументы
			// эталонной команды.
			n := func(value uint64) string {
				// Преобразует численные параметры эталонной команды в точные десятичные аргументы.
				return strconv.FormatUint(value, 10)
			}
			// filter задаёт FPS, масштаб и формат пикселей независимого CPU-эталона.
			filter := fmt.Sprintf("fps=fps=%d/%d:round=near:eof_action=round,scale=w=%d:h=%d,format=yuv420p", v.FrameRate.Numerator, v.FrameRate.Denominator, v.Width, v.Height)
			// wire содержит реальный выход независимого FFmpeg-преобразования выбранного
			// хвоста публикации.
			wire := mediaCommand(t, executable, input.Bytes(), "-hide_banner", "-loglevel", "warning", "-xerror", "-copyts", "-flags2:a", "+skip_manual", "-f", "mp4", "-fflags", "+ignidx", "-i", "pipe:0", "-map", "0:v:0", "-map", "0:a:0", "-vf", filter, "-fps_mode:v", "passthrough", "-enc_time_base:v", "filter", "-c:v", "libx264", "-threads:v", "1", "-preset", "veryfast", "-profile:v", "high", "-g:v", "6", "-bf:v", "0", "-sc_threshold", "0", "-c:a", "aac", "-profile:a", "aac_low", "-b:a", n(profile.AudioEncoding.BitrateBPS), "-ar:a", n(uint64(a.SampleRateHz)), "-ac:a", n(uint64(a.Channels)), "-channel_layout:a", a.ChannelLayout, "-aac_coder", "fast", "-f", "mp4", "-movflags", "delay_moov+default_base_moof+frag_every_frame", "-min_frag_duration", "1", "-use_editlist", "1", "-movie_timescale", "48000", "-video_track_timescale", n(uint64(v.FrameRate.Numerator)), "-avoid_negative_ts", "disabled", "pipe:1")
			// demux читает выходной MP4 независимого эталона с отдельной эпохой для
			// выбранной точки допуска.
			demux, err := fmp4.NewDemuxer(bytes.NewReader(wire), fmp4.DemuxOptions{Limits: integrationLimits(), SourceEpoch: referenceID, VideoRevision: 1, AudioRevision: 1})
			if err != nil {
				t.Fatal(err)
			}
			// decoded содержит эталонные медиапакеты независимой команды FFmpeg.
			decoded := readMediaPackets(t, demux)
			liveAudioDumpPackets(t, fmt.Sprintf("reference-cut-%d", referenceID), decoded)
			// audio считает эталонные AAC AU для проверки полного числа декодированных
			// сэмплов.
			audio := 0
			// durations считает встречаемость длительностей эталонных AAC AU для
			// диагностического вывода.
			durations := map[int64]int{}
			// packet представляет выходной эталонный пакет для подсчёта аудио и его
			// длительностей.
			for _, packet := range decoded {
				if packet.Config().Info().Track == media.TrackAudio {
					audio++
					durations[packet.Info().DurationTicks]++
				}
			}
			// pcm содержит декодированное аудио эталона для проверки полных блоков по 1024
			// сэмпла на канал.
			pcm := mediaCommand(t, executable, wire, "-v", "error", "-xerror", "-copyts", "-flags2:a", "+skip_manual", "-i", "pipe:0", "-map", "0:a:0", "-f", "f32le", "pipe:1")
			if len(pcm) != audio*1024*int(a.Channels)*4 {
				t.Fatalf("Эталон не сохранил 1024 декодированных сэмпла на AU: %d bytes/%d AU", len(pcm), audio)
			}
			if referenceID == 1 {
				t.Logf("Независимый эталон FFmpeg: AAC=%d PCM на канал=%d ASC=%x длительности=%v", audio, len(pcm)/(int(a.Channels)*4), demux.Init().Audio.Headers(), durations)
			}
			// packet представляет эталонный AU, из которого в итоговый набор выбираются
			// только аудиопакеты.
			for _, packet := range decoded {
				if packet.Config().Info().Track == media.TrackAudio {
					references = append(references, packet)
				}
			}
		}
	}
	t.Logf("Независимые временные эталоны: %d допустимых ключевых точек, %d AAC AU", referenceID, len(references))
	return references
}

// Исключает известные пакеты тишины и проверяет преобладание тона 750 Hz над контрольными частотами
// 500 и 1000 Hz в декодированном LIVE-аудио.
func liveAudioAssertTone(t *testing.T, pcm []byte, payloads [][]byte, quiet map[[32]byte]bool, channels, sampleRate int) {
	t.Helper()
	// wanted накапливает энергию тона 750 Гц; adjacent накапливает энергию контрольных частот 500 и
	// 1000 Гц.
	var wanted, adjacent float64
	// block обозначает позицию AAC AU в непрерывном PCM; payload содержит его нагрузку для
	// исключения известных пакетов тишины.
	for block, payload := range payloads {
		if quiet[sha256.Sum256(payload)] {
			continue
		}
		// frequency задаёт частоту искомого тона либо контрольного сигнала в герцах.
		for _, frequency := range []int{750, 500, 1000} {
			// sine накапливает синусную проекцию текущего блока; cosine накапливает его
			// косинусную проекцию на проверяемую частоту.
			var sine, cosine float64
			// n обозначает позицию сэмпла внутри блока из 1024 сэмплов одного канала.
			for n := range 1024 {
				// value содержит float32-сэмпл первого канала текущего аудиоблока.
				value := float64(math.Float32frombits(binary.LittleEndian.Uint32(pcm[(block*1024+n)*channels*4:])))
				// angle задаёт фазу проверяемой частоты для текущего сэмпла в радианах.
				angle := 2 * math.Pi * float64(frequency*n) / float64(sampleRate)
				sine += value * math.Sin(angle)
				cosine += value * math.Cos(angle)
			}
			// energy содержит квадрат амплитуды текущей частотной проекции аудиоблока.
			energy := sine*sine + cosine*cosine
			if frequency == 750 {
				wanted += energy
			} else {
				adjacent += energy
			}
		}
	}
	if wanted == 0 || wanted <= adjacent {
		t.Fatalf("LIVE потерял независимый тон 750 Hz: энергия=%g, соседние частоты=%g", wanted, adjacent)
	}
}
