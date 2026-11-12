package fallback

import (
	"bytes"
	"context"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

// Создаёт проверяемый профиль H.264 либо AV1 с видео 128×128 и стереофоническим AAC 48 kHz для тестов
// подготовки.
func selfProfile(codec core.Codec) core.GroupProfile {
	// p содержит базовый профиль видео 128×128 и AAC 48 кГц, адаптируемый к выбранному видеокодеку.
	p := core.GroupProfile{Mode: core.ModeTranscode, Format: core.MediaFormat{Video: core.VideoFormat{Codec: codec, Width: 128, Height: 128, FrameRate: core.FrameRate{Numerator: 30, Denominator: 1}, CodecProfile: "high", PixelFormat: "yuv420p", BitDepth: 8, Color: core.ColorInfo{Range: "limited", Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"}}, Audio: core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", SampleRateHz: 48000, Channels: 2, ChannelLayout: "stereo"}}, VideoEncoding: core.VideoEncoding{TargetBitrateBPS: 100000, MaxBitrateBPS: 200000, BufferBits: 200000, GOPFrames: 15, ClosedGOP: true}, AudioEncoding: core.AudioEncoding{BitrateBPS: 64000}}
	if codec == core.CodecAV1 {
		p.VideoEncoding.MaxBitrateBPS = p.VideoEncoding.TargetBitrateBPS
		p.Mode = core.ModeCopy
		p.Format.Video.CodecProfile = "main"
		p.Format.Video.Tier = "main"
	}
	return p
}

// Возвращает конечные пределы размеров, длительности, JSON, пакетов и fMP4 для тестовых инспекций.
func selfProbeLimits() ProbeLimits {
	return ProbeLimits{MaxJSONBytes: 1 << 20, MaxWidth: 4096, MaxHeight: 4096, MaxPixels: 4096 * 4096, MaxDuration: time.Minute, ImageDuration: 1600 * time.Millisecond, MaxPackets: 10000, Container: fmp4.Limits{MaxBoxBytes: 4 << 20, MaxInitBytes: 1 << 20, MaxMoofBytes: 1 << 20, MaxMdatBytes: 4 << 20, MaxSampleBytes: 1 << 20, MaxExtradataBytes: 1 << 16, MaxSamples: 10000}}
}

// Создаёт Runner с конечным сроком и бюджетами команды и диагностики для локальных тестовых процессов.
func selfRunner() Runner {
	return Runner{Limits: RunnerLimits{Timeout: 20 * time.Second, MaxArgs: 256, MaxCommandBytes: 1 << 16, MaxDiagnosticBytes: 1 << 20}}
}

// Регистрирует закрытие файла после теста с учётом ошибки закрытия.
func selfClose(t *testing.T, f *os.File) {
	t.Helper()
	t.Cleanup(func() {
		// Закрывает переданный тестовый файл после сценария и сообщает ошибку через t.Error.

		// err сообщает об отказе закрытия тестового файла.
		if err := f.Close(); err != nil {
			t.Error(err)
		}
	})
}

// Проверяет рабочий выбор h264_nvenc/libsvtav1, вход FD 3 и отдельную AAC-тишину, а также отказ для
// HDR-профиля.
func TestProductionBuildersKeepHardwarePolicyAndFDInput(t *testing.T) {
	// c выбирает проверяемый кодек AV1 или H.264.
	for _, c := range []core.Codec{core.CodecAV1, core.CodecH264} {
		// p содержит профиль подготовки для выбранного кодека.
		p := selfProfile(c)
		// cmd содержит рабочую команду подготовки изображения. err сообщает об отказе её
		// построения.
		cmd, err := ProductionBuilder("ffmpeg")(GenerateRequest{Source: Source{Kind: core.AssetImage, Duration: time.Second, Demuxer: "png_pipe"}, Profile: p, LoopDuration: 1600 * time.Millisecond})
		if err != nil {
			t.Fatal(err)
		}
		// joined содержит объединённые аргументы для проверки входа, тишины и кодировщика.
		joined := strings.Join(cmd.Args, " ")
		// s содержит очередной обязательный фрагмент команды подготовки.
		for _, s := range []string{"-fd 3 -i fd:", "anullsrc=r=48000:cl=stereo", "-map 0:v:0 -map 1:a:0", "-c:a aac", "pipe:1"} {
			if !strings.Contains(joined, s) {
				t.Fatalf("нет %s: %s", s, joined)
			}
		}
		// want содержит ожидаемое рабочее имя видеокодировщика.
		want := "h264_nvenc"
		if c == core.CodecAV1 {
			want = "libsvtav1"
		}
		if !strings.Contains(joined, "-c:v "+want) {
			t.Fatal(joined)
		}
		p.Format.Video.Color.Transfer = "smpte2084"
		if _, err = ProductionBuilder("ffmpeg")(GenerateRequest{Source: Source{Kind: core.AssetImage, Duration: time.Second, Demuxer: "png_pipe"}, Profile: p, LoopDuration: 1600 * time.Millisecond}); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	}
}

// Проверяет замену звука исходного видео на тишину формата профиля даже при выбранном LIVE copy.
func TestProductionBuilderReplacesSourceAudioWithProfileSilence(t *testing.T) {
	// p содержит профиль H.264 с выбранным копированием LIVE-аудио.
	p := selfProfile(core.CodecH264)
	p.LiveAudioMode = core.LiveAudioCopy
	// audio содержит исходный аудиоформат, намеренно отличный от выходного профиля.
	audio := p.Format.Audio
	audio.SampleRateHz = 44100
	audio.Channels, audio.ChannelLayout = 1, "mono"
	// command содержит команду, которая должна заменить исходное аудио тишиной профиля. err сообщает
	// об отказе её построения.
	command, err := ProductionBuilder("ffmpeg")(GenerateRequest{Source: Source{Kind: core.AssetVideo, Format: core.SourceFormat{Audio: &audio}, Duration: time.Second, Demuxer: "mov"}, Profile: p, LoopDuration: 1600 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	// args содержит объединённые аргументы для проверки выбора аудиоисточника.
	args := strings.Join(command.Args, " ")
	// required содержит очередной обязательный аргумент кодирования тишины профиля.
	for _, required := range []string{"anullsrc=r=48000:cl=stereo", "-map 0:v:0 -map 1:a:0", "-c:a aac", "-ar:a 48000", "-ac:a 2"} {
		if !strings.Contains(args, required) {
			t.Fatal(required, args)
		}
	}
	if strings.Contains(args, "-map 0:a:0") || strings.Contains(args, "-c:a copy") {
		t.Fatal("подготовка использует звук источника", args)
	}
}

// При специальном аргументе запуска выполняет роль дочернего процесса: копирует FD 3, переполняет
// вывод или диагностику либо ждёт остановки.
func TestRunnerChild(t *testing.T) {
	if len(os.Args) < 2 || os.Args[len(os.Args)-2] != "fallback-child" {
		return
	}
	switch os.Args[len(os.Args)-1] {
	case "copy":
		// f представляет переданный дочернему процессу входной дескриптор FD 3.
		f := os.NewFile(3, "input")
		_, _ = io.Copy(os.Stdout, f)
		_ = f.Close()
	case "overflow":
		_, _ = os.Stdout.Write(bytes.Repeat([]byte("x"), 4096))
	case "diagnostic":
		_, _ = os.Stderr.Write(bytes.Repeat([]byte("d"), 4096))
	case "wait":
		<-time.After(time.Minute)
	}
	os.Exit(0)
}

// Проверяет передачу FD 3, ограничения вывода и диагностики, отмену и отказ по бюджету команды у
// конечного Runner.
func TestFiniteRunnerBoundsOutputDiagnosticsAndJoins(t *testing.T) {
	// binary содержит путь текущего тестового исполняемого файла для запуска дочернего режима. err
	// сообщает об отказе его определения.
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// r содержит исполнитель с конечными тестовыми бюджетами.
	r := selfRunner()
	// mode выбирает копирование, переполнение вывода, переполнение диагностики либо ожидание.
	for _, mode := range []string{"copy", "overflow", "diagnostic", "wait"} {
		t.Run(mode, func(t *testing.T) {
			// Проверяет выбранный режим дочернего процесса: копирование FD, предел вывода, предел диагностики либо
			// отмену до запуска.

			// input содержит временный файл для передачи дочернему процессу через FD 3. err
			// сообщает об отказе его создания.
			input, err := os.CreateTemp(t.TempDir(), "input")
			if err != nil {
				t.Fatal(err)
			}
			selfClose(t, input)
			if _, err = input.WriteString("owned descriptor"); err != nil {
				t.Fatal(err)
			}
			if _, err = input.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			// local содержит отдельную копию исполнителя с малым бюджетом диагностики
			// текущего подтеста.
			local := r
			local.Limits.MaxDiagnosticBytes = 32
			// ctx позволяет отменить запуск дочернего процесса. cancel освобождает контекст
			// и заранее отменяет его в режиме wait.
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			if mode == "wait" {
				cancel()
			}
			// out принимает ограниченный стандартный вывод дочернего процесса.
			var out bytes.Buffer
			_, err = local.Run(ctx, Command{Binary: binary, Args: []string{"-test.run=^TestRunnerChild$", "fallback-child", mode}}, input, &boundedWriter{w: &out, remaining: 32})
			if mode == "copy" {
				if err != nil || out.String() != "owned descriptor" {
					t.Fatal(out.String(), err)
				}
			} else if mode == "wait" {
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			} else if !errors.Is(err, ErrLimit) {
				t.Fatal(err)
			}
		})
	}
	r.Limits.MaxCommandBytes = 1
	if _, err = r.Run(context.Background(), Command{Binary: binary}, nil, nil); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
}

// Проверяет отказ до запуска для пустой команды, нулевых байтов и превышения бюджетов, а также
// отклонение неподдержанного HDR и глубины источника.
func TestRunnerRejectsUntrustedCommandBudgetsBeforeStart(t *testing.T) {
	// r содержит исполнитель для проверки отказов до запуска команды.
	r := selfRunner()
	// command содержит очередную недопустимую команду с пустым именем, нулевым байтом или
	// превышенным бюджетом.
	for _, command := range []Command{{}, {Binary: "bad\x00binary"}, {Binary: "missing", Args: []string{"bad\x00arg"}}, {Binary: "missing", Args: make([]string, r.Limits.MaxArgs+1)}, {Binary: "missing", Args: []string{strings.Repeat("x", int(r.Limits.MaxCommandBytes))}}} {
		// err содержит результат проверки и попытки выполнения недопустимой команды.
		if _, err := r.Run(context.Background(), command, nil, nil); err == nil || (!errors.Is(err, ErrInvalid) && !errors.Is(err, ErrLimit)) {
			t.Fatal(command, err)
		}
	}
	// source содержит исходный видеоформат с неподдержанным HDR либо десятибитной глубиной.
	for _, source := range []core.VideoFormat{{PixelFormat: "yuv420p", BitDepth: 8, Color: core.ColorInfo{Transfer: "smpte2084"}}, {PixelFormat: "yuv420p10le", BitDepth: 10}} {
		// err содержит результат построения команды для неподдержанного исходного формата.
		if _, err := ProductionBuilder("ffmpeg")(GenerateRequest{Source: Source{Kind: core.AssetVideo, Format: core.SourceFormat{Video: &source}, Duration: time.Second, Demuxer: "mov"}, Profile: selfProfile(core.CodecH264), LoopDuration: 1600 * time.Millisecond}); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	}
}

// Проверяет фактические размеры PNG, отсутствие аудиоформата, ограничение ширины и отказ для
// неизвестного вида источника.
func TestSourceImagesUseMeasuredDimensionsAndBudgets(t *testing.T) {
	// file содержит временное изображение для проверки фактических размеров PNG. err сообщает об
	// отказе его создания.
	file, err := os.CreateTemp(t.TempDir(), "image")
	if err != nil {
		t.Fatal(err)
	}
	selfClose(t, file)
	if err = png.Encode(file, image.NewRGBA(image.Rect(0, 0, 128, 64))); err != nil {
		t.Fatal(err)
	}
	// p содержит проверяющий порт с ограничениями размеров изображения.
	p := Prober{Limits: selfProbeLimits()}
	// s содержит фактически распознанный PNG-источник.
	s, err := p.Source(context.Background(), file, core.AssetImage)
	if err != nil || s.Width != 128 || s.Height != 64 || s.Format.ImageFormat != "png" || s.Format.Audio != nil {
		t.Fatal(s, err)
	}
	p.Limits.MaxWidth = 127
	if _, err = p.Source(context.Background(), file, core.AssetImage); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
	if _, err = p.Source(context.Background(), file, "other"); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}

// TestCPUImageGenerationAndActualProbe при заданном GOST_MEDIA_FFMPEG подготавливает PNG в H.264
// и AV1 с кодировщиками CPU, проверяет сохранность пакетов при нормализации и фактический формат
// вариантов через Prober. После завершения проверяет Joined области владельца процессов.
func TestCPUImageGenerationAndActualProbe(t *testing.T) {
	// binary содержит явно включённый путь FFmpeg; пустая строка пропускает CPU-проверку.
	binary := os.Getenv("GOST_MEDIA_FFMPEG")
	if binary == "" {
		t.Skip("локальная CPU-проверка не включена")
	}
	// source содержит временный PNG для реального кодирования. err сообщает об отказе его создания.
	source, err := os.CreateTemp(t.TempDir(), "source.png")
	if err != nil {
		t.Fatal(err)
	}
	selfClose(t, source)
	// img содержит исходное изображение 128×128 с изменяющимися по координатам цветами.
	img := image.NewRGBA(image.Rect(0, 0, 128, 128))
	// y задаёт строку пикселей исходного изображения.
	for y := range 128 {
		// x задаёт столбец пикселей исходного изображения.
		for x := range 128 {
			img.Set(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 90, A: 255})
		}
	}
	if err = png.Encode(source, img); err != nil {
		t.Fatal(err)
	}
	// r содержит исполнитель реальных команд с тестовыми ограничениями.
	r := selfRunner()
	// registry содержит тестовый реестр владения процессами.
	registry := fallbackProcessRegistry(t)
	// scope содержит область реальных процессов подготовки изображения.
	scope, err := registry.Create(context.Background(), "cpu-image-preparation")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает область процессов подготовки изображения при завершении теста, игнорируя ошибку этого
		// резервного закрытия.
		_ = scope.Close()
	})
	r.Scope = scope
	// p содержит проверяющий порт ffprobe рядом с выбранным FFmpeg.
	p := Prober{Binary: filepath.Join(filepath.Dir(binary), "ffprobe"), Runner: r, Limits: selfProbeLimits()}
	p.Limits.ImageDuration = time.Second
	// observed содержит фактически проверенное описание исходного PNG.
	observed, err := p.Source(context.Background(), source, core.AssetImage)
	if err != nil {
		t.Fatal(err)
	}
	// c выбирает видеокодек текущего CPU-подтеста.
	for _, c := range []core.Codec{core.CodecH264, core.CodecAV1} {
		t.Run(string(c), func(t *testing.T) {
			// Генерирует и нормализует вариант выбранного кодека с явным кодировщиком CPU, затем проверяет
			// фактический формат и сохранность пакетов.

			// profile содержит выходной профиль выбранного видеокодека.
			profile := selfProfile(c)
			// encoder содержит явный CPU-кодировщик libx264 или libsvtav1 для текущего
			// подтеста.
			encoder := "libx264"
			if c == core.CodecAV1 {
				encoder = "libsvtav1"
			}
			// loop содержит выровненную длину петли. err сообщает об отказе её вычисления.
			loop, err := EffectiveLoopDuration(observed.Duration, profile, p.Limits.MaxDuration)
			if err != nil {
				t.Fatal(err)
			}
			// cmd содержит команду подготовки с явной тестовой подстановкой CPU-кодировщика.
			cmd, err := buildCommand(binary, GenerateRequest{Source: observed, Profile: profile, LoopDuration: loop}, encoder)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = source.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			// output содержит временный файл с выходом генератора, затем заменяется
			// нормализованным файлом.
			output, err := os.CreateTemp(t.TempDir(), "variant.mp4")
			if err != nil {
				t.Fatal(err)
			}
			selfClose(t, output)
			// result содержит диагностику реального кодирования варианта.
			result, err := r.Run(context.Background(), cmd, source, &boundedWriter{w: output, remaining: 8 << 20})
			if err != nil {
				t.Fatalf("generator: %v: %s", err, result.Diagnostics)
			}
			{
				// normalized содержит временный файл нормализации. e сообщает об отказе
				// его создания.
				normalized, e := os.CreateTemp(t.TempDir(), "normalized.mp4")
				if e != nil {
					t.Fatal(e)
				}
				selfClose(t, normalized)
				if e = p.NormalizePrepared(context.Background(), output, &boundedWriter{w: normalized, remaining: 8 << 20}); e != nil {
					t.Fatalf("normalize: %v", e)
				}
				selfComparePackets(t, p, output, normalized, true)
				output = normalized
			}
			// actual содержит фактически проверенный формат нормализованного варианта.
			actual, err := p.Verify(context.Background(), output, profile)
			if err != nil {
				t.Fatalf("verify actual %+v: %v", actual, err)
			}
		})
	}
	if err = scope.Seal(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = scope.Close(); err != nil {
		t.Fatal(err)
	}
	// proof содержит сохранённое доказательство завершения области реальных процессов.
	proof, err := scope.Resolve(context.Background())
	if err != nil || !proof.Joined {
		t.Fatalf("CPU-подготовка не присоединила процессы: %+v %v", proof, err)
	}
}

// Демультиплексирует исходный и нормализованный файлы и сравнивает метки, временные базы, дорожки и
// байты пакетов; при skipPriming исключает первый исходный AAC-пакет.
func selfComparePackets(t *testing.T, p Prober, raw, normalized *os.File, skipPriming bool) {
	t.Helper()
	// read демультиплексирует все пакеты выбранного файла с использованием его наблюдаемого формата.
	read := func(f *os.File) []media.Packet {
		// Читает все пакеты файла через fMP4 с фактическим форматом ffprobe для сравнения исходной и
		// нормализованной последовательностей.

		// actual содержит наблюдаемый формат файла для восстановления конфигураций дорожек. err
		// сообщает об отказе его определения.
		actual, err := p.observedFormat(context.Background(), f)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = f.Seek(0, io.SeekStart); err != nil {
			t.Fatal(err)
		}
		// d содержит демультиплексор выбранного файла.
		d, err := fmp4.NewDemuxer(f, fmp4.DemuxOptions{Limits: p.Limits.Container, SourceEpoch: 1, VideoRevision: 1, AudioRevision: 1, ResolveConfig: func(desc fmp4.TrackDescription) (media.CodecConfig, error) {
			// Создаёт конфигурацию дорожки из её шкалы и заголовков fMP4 и фактически измеренного формата.

			// info объединяет шкалу и заголовки дорожки контейнера с наблюдаемым форматом.
			info := media.CodecConfigInfo{Track: desc.Track, SourceEpoch: 1, Revision: 1, TimeBase: desc.TimeBase}
			if desc.Track == media.TrackVideo {
				info.Video = actual.Video
			} else {
				info.Audio = actual.Audio
			}
			return media.NewCodecConfig(info, desc.Headers)
		}})
		if err != nil {
			t.Fatal(err)
		}
		// packets накапливает все единицы доступа в порядке чтения файла.
		var packets []media.Packet
		for {
			// packet содержит очередную единицу доступа. err сообщает об окончании файла или
			// отказе чтения.
			packet, err := d.Next()
			if err == io.EOF {
				return packets
			}
			if err != nil {
				t.Fatal(err)
			}
			packets = append(packets, packet)
		}
	}
	// a содержит пакеты исходного файла. b содержит пакеты нормализованного файла.
	a, b := read(raw), read(normalized)
	if skipPriming {
		// i задаёт позицию пакета исходного файла. packet содержит этот пакет для поиска первой
		// AAC-единицы задержки.
		for i, packet := range a {
			if packet.Config().Info().Track == media.TrackAudio {
				a = append(a[:i], a[i+1:]...)
				break
			}
		}
	}
	if len(a) != len(b) {
		t.Fatalf("число samples изменилось: %d/%d", len(a), len(b))
	}
	// i задаёт позицию соответствующих пакетов исходной и нормализованной последовательностей.
	for i := range a {
		if a[i].Info() != b[i].Info() || a[i].Config().Info().TimeBase != b[i].Config().Info().TimeBase || a[i].Config().Info().Track != b[i].Config().Info().Track || !bytes.Equal(a[i].Payload(), b[i].Payload()) {
			t.Fatalf("sample %d изменён при нормализации", i)
		}
	}
}

// При включённом FFmpeg проверяет подготовку видео без аудио с добавлением стереофонической дорожки
// AAC и сохранением пакетов при нормализации, кроме удаляемого начального AAC-пакета.
func TestCPUVideoWithoutAudioReceivesVerifiedSilence(t *testing.T) {
	// binary содержит явно включённый путь FFmpeg для CPU-проверки видео без аудио.
	binary := os.Getenv("GOST_MEDIA_FFMPEG")
	if binary == "" {
		t.Skip("локальная CPU-проверка не включена")
	}
	// source содержит временное исходное видео без аудиодорожки. err сообщает об отказе его
	// создания.
	source, err := os.CreateTemp(t.TempDir(), "source.mp4")
	if err != nil {
		t.Fatal(err)
	}
	selfClose(t, source)
	// r содержит ограниченный исполнитель реальных команд.
	r := selfRunner()
	// command содержит команду создания видео без аудио, затем команду подготовки заглушки.
	command := Command{Binary: binary, Args: []string{"-hide_banner", "-nostdin", "-loglevel", "warning", "-xerror", "-f", "lavfi", "-i", "testsrc2=size=128x128:rate=30:duration=1.13", "-an", "-c:v", "libx264", "-bf", "0", "-vf", "setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709", "-f", "mp4", "-movflags", "delay_moov+default_base_moof+frag_every_frame", "pipe:1"}}
	// result содержит диагностику создания исходного видео. err сообщает об отказе команды.
	if result, err := r.Run(context.Background(), command, nil, &boundedWriter{w: source, remaining: 8 << 20}); err != nil {
		t.Fatalf("source: %v: %s", err, result.Diagnostics)
	}
	// p содержит проверяющий порт ffprobe выбранного комплекта FFmpeg.
	p := Prober{Binary: filepath.Join(filepath.Dir(binary), "ffprobe"), Runner: r, Limits: selfProbeLimits()}
	// observed содержит проверенное описание исходника, в котором аудиоформат должен отсутствовать.
	observed, err := p.Source(context.Background(), source, core.AssetVideo)
	if err != nil || observed.Format.Audio != nil {
		t.Fatal(observed, err)
	}
	// profile содержит выходной профиль H.264 и стереофонического AAC.
	profile := selfProfile(core.CodecH264)
	// loop содержит выровненную длину петли для исходного видео.
	loop, err := EffectiveLoopDuration(observed.Duration, profile, p.Limits.MaxDuration)
	if err != nil {
		t.Fatal(err)
	}
	command, err = buildCommand(binary, GenerateRequest{Source: observed, Profile: profile, LoopDuration: loop}, "libx264")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = source.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	// output содержит временный результат подготовки с добавленным AAC.
	output, err := os.CreateTemp(t.TempDir(), "output.mp4")
	if err != nil {
		t.Fatal(err)
	}
	selfClose(t, output)
	// result содержит диагностику подготовки заглушки. err сообщает об отказе команды.
	if result, err := r.Run(context.Background(), command, source, &boundedWriter{w: output, remaining: 8 << 20}); err != nil {
		t.Fatalf("generate: %v: %s", err, result.Diagnostics)
	}
	// normalized содержит временный результат удаления подтверждённой задержки AAC.
	normalized, err := os.CreateTemp(t.TempDir(), "normalized.mp4")
	if err != nil {
		t.Fatal(err)
	}
	selfClose(t, normalized)
	if err = p.NormalizePrepared(context.Background(), output, &boundedWriter{w: normalized, remaining: 8 << 20}); err != nil {
		t.Fatal(err)
	}
	selfComparePackets(t, p, output, normalized, true)
	// actual содержит фактически проверенный выходной формат для подтверждения AAC и числа каналов.
	actual, err := p.Verify(context.Background(), normalized, profile)
	if err != nil || actual.Audio.Codec != core.CodecAAC || actual.Audio.Channels != 2 {
		t.Fatal(actual, err)
	}
}

// При включённом FFmpeg проверяет замену исходного тона на нулевой PCM и дополнение обеих дорожек до
// полного периода, включая источник с более длинным аудио.
func TestCPUShortVideoReplacesToneWithSilenceAndPadsAFullLoop(t *testing.T) {
	// binaryPath содержит явно включённый путь FFmpeg для проверки замены тона тишиной.
	binaryPath := os.Getenv("GOST_MEDIA_FFMPEG")
	if binaryPath == "" {
		t.Skip("локальная CPU-проверка не включена")
	}
	// test содержит длительности исходных дорожек и ожидаемую полную длину петли.
	for _, test := range []struct {
		// name обозначает сочетание длительностей дорожек в имени подтеста.
		name string
		// videoDuration задаёт длительность исходного видео. audioDuration задаёт длительность
		// исходного тона.
		videoDuration, audioDuration time.Duration
		// loop задаёт ожидаемую длину полной петли A/V.
		loop time.Duration
	}{
		{name: "equal-source-tracks", videoDuration: 1130 * time.Millisecond, audioDuration: 1130 * time.Millisecond, loop: 1600 * time.Millisecond},
		{name: "longer-source-audio", videoDuration: 1600 * time.Millisecond, audioDuration: 1800 * time.Millisecond, loop: 3200 * time.Millisecond},
	} {
		t.Run(test.name, func(t *testing.T) {
			// Проверяет замену исходного тона тишиной и дополнение обеих дорожек до полной петли при выбранных
			// длительностях видео и аудио.

			// r содержит ограниченный исполнитель реальных команд текущего сценария.
			r := selfRunner()
			// source содержит временный исходник с видео и ненулевым аудиотоном. err
			// сообщает об отказе его создания.
			source, err := os.CreateTemp(t.TempDir(), "source-with-audio.mp4")
			if err != nil {
				t.Fatal(err)
			}
			selfClose(t, source)
			// command содержит команду создания исходного тона, затем команду подготовки с
			// тишиной.
			command := Command{Binary: binaryPath, Args: []string{"-hide_banner", "-nostdin", "-loglevel", "warning", "-xerror", "-f", "lavfi", "-i", "testsrc2=size=128x128:rate=30:duration=" + durationArgument(test.videoDuration), "-f", "lavfi", "-i", "sine=frequency=750:sample_rate=48000:duration=" + durationArgument(test.audioDuration), "-map", "0:v:0", "-map", "1:a:0", "-c:v", "libx264", "-bf", "0", "-c:a", "aac", "-ar", "48000", "-ac", "2", "-vf", "setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709", "-t", durationArgument(test.audioDuration), "-f", "mp4", "-movflags", "+faststart", "-y", source.Name()}}
			// result содержит диагностику создания исходного A/V. err сообщает об отказе
			// команды.
			if result, err := r.Run(context.Background(), command, nil, nil); err != nil {
				t.Fatalf("source: %v: %s", err, result.Diagnostics)
			}
			// p содержит проверяющий порт ffprobe для исходного и нормализованного файлов.
			p := Prober{Binary: filepath.Join(filepath.Dir(binaryPath), "ffprobe"), Runner: r, Limits: selfProbeLimits()}
			// observed содержит фактический формат и длительность исходного A/V.
			observed, err := p.Source(context.Background(), source, core.AssetVideo)
			if err != nil || observed.Format.Audio == nil {
				t.Fatal(observed, err)
			}
			// profile содержит выходной профиль H.264 и AAC для петли.
			profile := selfProfile(core.CodecH264)
			// loop содержит фактически рассчитанную полную длину петли.
			loop, err := EffectiveLoopDuration(observed.Duration, profile, p.Limits.MaxDuration)
			if err != nil || loop != test.loop {
				t.Fatal(observed.Duration, loop, err)
			}
			if test.audioDuration > test.videoDuration && observed.Duration <= test.videoDuration {
				t.Fatalf("контейнер не сохранил более длинную аудиодорожку: %s <= %s", observed.Duration, test.videoDuration)
			}
			command, err = buildCommand(binaryPath, GenerateRequest{Source: observed, Profile: profile, LoopDuration: loop}, "libx264")
			if err != nil {
				t.Fatal(err)
			}
			// input содержит отдельно открытый исходник для передачи генератору через FD 3.
			input, err := os.Open(source.Name())
			if err != nil {
				t.Fatal(err)
			}
			selfClose(t, input)
			// raw содержит временный ненормализованный выход подготовки.
			raw, err := os.CreateTemp(t.TempDir(), "raw.mp4")
			if err != nil {
				t.Fatal(err)
			}
			selfClose(t, raw)
			// result содержит диагностику генерации петли. err сообщает об отказе команды.
			if result, err := r.Run(context.Background(), command, input, &boundedWriter{w: raw, remaining: 8 << 20}); err != nil {
				t.Fatalf("generate: %v: %s", err, result.Diagnostics)
			}
			// normalized содержит временную нормализованную петлю для проверки пакетов и
			// декодирования.
			normalized, err := os.CreateTemp(t.TempDir(), "normalized.mp4")
			if err != nil {
				t.Fatal(err)
			}
			selfClose(t, normalized)
			if err = p.NormalizePrepared(context.Background(), raw, &boundedWriter{w: normalized, remaining: 8 << 20}); err != nil {
				t.Fatal(err)
			}
			selfComparePackets(t, p, raw, normalized, true)
			if _, err = p.Verify(context.Background(), normalized, profile); err != nil {
				t.Fatal(err)
			}
			if err = p.VerifyLoopDuration(context.Background(), normalized, loop); err != nil {
				t.Fatal(err)
			}
			// doc содержит метаданные нормализованной петли для проверки нулевого начала
			// дорожек.
			doc, err := p.inspect(context.Background(), normalized)
			if err != nil {
				t.Fatal(err)
			}
			// stream обозначает очередную дорожку нормализованной петли.
			for _, stream := range doc.Streams {
				// start содержит время начала дорожки в секундах. err сообщает о
				// некорректной записи этого времени.
				start, err := strconv.ParseFloat(stream.StartTime, 64)
				if err != nil || start != 0 {
					t.Fatalf("начало %s=%s: %v", stream.Type, stream.StartTime, err)
				}
			}
			// decode декодирует выбранную дорожку через новый дескриптор нормализованного
			// файла и возвращает ограниченный вывод.
			decode := func(args ...string) []byte {
				// Декодирует выбранную дорожку нормализованного файла настоящим FFmpeg через новый файловый дескриптор
				// и возвращает ограниченный вывод.

				// file содержит отдельный дескриптор нормализованной петли для одного
				// запуска декодирования. err сообщает об отказе открытия.
				file, err := os.Open(normalized.Name())
				if err != nil {
					t.Fatal(err)
				}
				defer func() {
					// Закрывает открытый для декодирования файл и сообщает ошибку через t.Error.

					// err сообщает об отказе закрытия дескриптора после
					// декодирования.
					if err := file.Close(); err != nil {
						t.Error(err)
					}
				}()
				// out принимает ограниченный вывод декодирования выбранной дорожки.
				var out bytes.Buffer
				// argv накапливает аргументы входа, выбора дорожки и формата выхода
				// FFmpeg.
				argv := []string{"-hide_banner", "-nostdin", "-loglevel", "warning", "-xerror", "-protocol_whitelist", "fd,pipe", "-fd", "3", "-i", "fd:"}
				argv = append(argv, args...)
				argv = append(argv, "pipe:1")
				// result содержит диагностику декодирования выбранной дорожки.
				result, err := r.Run(context.Background(), Command{Binary: binaryPath, Args: argv}, file, &boundedWriter{w: &out, remaining: 2 << 20})
				if err != nil {
					t.Fatalf("decode: %v: %s", err, result.Diagnostics)
				}
				return out.Bytes()
			}
			// video содержит текст framemd5, по которому считаются декодированные
			// видеокадры.
			video := decode("-map", "0:v:0", "-an", "-fps_mode:v", "passthrough", "-f", "framemd5")
			// frames считает строки данных кадров в framemd5 без заголовков.
			frames := 0
			// line содержит очередную строку вывода framemd5.
			for line := range strings.SplitSeq(string(video), "\n") {
				if line != "" && !strings.HasPrefix(line, "#") {
					frames++
				}
			}
			// wantFrames задаёт ожидаемое число кадров полной петли при 30 кадрах в секунду.
			wantFrames := int(loop * 30 / time.Second)
			if frames != wantFrames {
				t.Fatalf("число декодированных видеокадров %d !=%d", frames, wantFrames)
			}
			// pcm содержит декодированное стереофоническое аудио в формате pcm_s16le.
			pcm := decode("-map", "0:a:0", "-vn", "-c:a", "pcm_s16le", "-f", "s16le")
			// wantSamples задаёт ожидаемое число сэмплов на канал полной петли при 48 кГц.
			wantSamples := int(loop * 48000 / time.Second)
			if len(pcm) != wantSamples*2*2 {
				t.Fatalf("число аудиосэмплов PCM на канал %d !=%d", len(pcm)/4, wantSamples)
			}
			// i задаёт позицию байта PCM. sample содержит его значение для проверки
			// полностью нулевой тишины.
			for i, sample := range pcm {
				if sample != 0 {
					t.Fatalf("подготовленная тишина содержит ненулевой PCM byte[%d]=%d", i, sample)
				}
			}
		})
	}
}
