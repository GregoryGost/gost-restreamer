package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

// mediaProbe хранит независимые сведения ffprobe о дорожках и временных метках пакетов контейнера.
type mediaProbe struct {
	// Streams содержит описания всех дорожек, возвращённые ffprobe для проверки формата и временных
	// баз.
	Streams []struct {
		// Index задаёт индекс дорожки ffprobe для сопоставления пакетов с видео или аудио.
		Index int
		// CodecType содержит тип дорожки video либо audio из независимого ffprobe.
		CodecType string `json:"codec_type"`
		// CodecName содержит имя кодека, наблюдаемое ffprobe в проверяемом контейнере.
		CodecName string `json:"codec_name"`
		// TimeBase задаёт строковую рациональную временную базу дорожки ffprobe.
		TimeBase string `json:"time_base"`
		// Width содержит ширину видеокадра в пикселях, а Height — его высоту по данным ffprobe.
		Width, Height uint32
		// Profile содержит профиль кодека, определённый независимым ffprobe.
		Profile string
		// PixelFormat содержит наблюдаемый пиксельный формат видеодорожки.
		PixelFormat string `json:"pix_fmt"`
		// FrameRate содержит рациональную частоту кадров видеодорожки ffprobe.
		FrameRate string `json:"r_frame_rate"`
		// SampleRate содержит строковое число аудиосэмплов в секунду по данным ffprobe.
		SampleRate string `json:"sample_rate"`
		// Channels содержит наблюдаемое число каналов аудиодорожки.
		Channels uint16
		// ChannelLayout содержит наблюдаемое размещение аудиоканалов.
		ChannelLayout string `json:"channel_layout"`
	}
	// Packets содержит временные метки всех пакетов ffprobe с привязкой к индексу дорожки.
	Packets []struct {
		// StreamIndex связывает временные метки пакета с индексом его дорожки ffprobe.
		StreamIndex int `json:"stream_index"`
		// PTS содержит время показа пакета, а DTS — время декодирования в единицах временной
		// базы соответствующей дорожки.
		PTS, DTS int64
	}
}

// boundedMediaBuffer накапливает вывод медиакоманды, отклоняя запись, которая превысила бы заданный
// бюджет.
type boundedMediaBuffer struct {
	// Buffer хранит уже принятые байты вывода медиакоманды.
	bytes.Buffer
	// max задаёт максимальное число байтов, разрешённое в буфере вывода.
	max int
}

// Write добавляет данные только целиком в пределах max; при превышении бюджета возвращает ошибку без
// частичной записи.
func (b *boundedMediaBuffer) Write(p []byte) (int, error) {
	if len(p) > b.max-b.Len() {
		return 0, errors.New("превышен бюджет синтетической фикстуры")
	}
	return b.Buffer.Write(p)
}

// Запускает конечную медиакоманду с заданным stdin, сроком 20 секунд и отдельными бюджетами stdout и
// stderr. Ошибка процесса или диагностика исправления DTS завершает тест; результат возвращается
// самостоятельной копией.
func mediaCommand(t *testing.T, binary string, input []byte, args ...string) []byte {
	t.Helper()
	// ctx ограничивает выполнение конечной медиакоманды двадцатью секундами, а cancel освобождает
	// таймер после её завершения.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// cmd представляет запускаемую медиакоманду с заданным вводом и ограниченным выводом.
	cmd := exec.CommandContext(ctx, binary, args...)
	cmd.Stdin = bytes.NewReader(input)
	// out накапливает не более 16 МиБ стандартного вывода команды, а stderr — не более 1 МиБ её
	// диагностики.
	out, stderr := &boundedMediaBuffer{max: 16 << 20}, &boundedMediaBuffer{max: 1 << 20}
	cmd.Stdout, cmd.Stderr = out, stderr
	// err сообщает об отказе запуска или выполнения конечной медиакоманды.
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s: %v: %s", filepath.Base(binary), err, stderr.String())
	}
	if strings.Contains(stderr.String(), "Invalid DTS") || strings.Contains(stderr.String(), "Non-monotonic") {
		t.Fatalf("FFmpeg изменил время: %s", stderr.String())
	}
	return bytes.Clone(out.Bytes())
}

// Запускает ffprobe для всех потоков и пакетов входного контейнера и разбирает его JSON в независимый
// временной эталон.
func probeMedia(t *testing.T, binary string, data []byte) mediaProbe {
	t.Helper()
	// p принимает разобранные данные ffprobe для независимого сравнения пакетов.
	var p mediaProbe
	// err сообщает о невозможности разобрать JSON-вывод ffprobe.
	if err := json.Unmarshal(mediaCommand(t, binary, data, "-v", "error", "-show_packets", "-show_streams", "-of", "json", "pipe:0"), &p); err != nil {
		t.Fatal(err)
	}
	return p
}

// Возвращает явные пределы fMP4 для коротких интеграционных фикстур, включая размеры блоков, пакетов и
// конфигураций.
func integrationLimits() fmp4.Limits {
	return fmp4.Limits{MaxBoxBytes: 1 << 20, MaxInitBytes: 1 << 20, MaxMoofBytes: 1 << 18, MaxMdatBytes: 1 << 20, MaxSampleBytes: 1 << 18, MaxExtradataBytes: 4096, MaxSamples: 256}
}

// Создаёт разрешитель конфигураций для fMP4: AV1 сверяется с независимым ffprobe по размерам, кодеку и
// пиксельному формату, а остальные кодеки используют штатный ResolveCodecConfig.
func truthfulResolver(t *testing.T, probe mediaProbe) func(fmp4.TrackDescription) (media.CodecConfig, error) {
	t.Helper()
	return func(d fmp4.TrackDescription) (media.CodecConfig, error) {
		// Для AV1 сверяет описание контейнера с независимым ffprobe, а остальные кодеки разбирает штатным
		// способом.

		if d.Codec != core.CodecAV1 {
			return fmp4.ResolveCodecConfig(d, 11, 1)
		}
		// s обозначает очередную дорожку независимого ffprobe при поиске видео AV1.
		for _, s := range probe.Streams {
			if s.CodecType == "video" {
				// n содержит числитель частоты видеокадров ffprobe, а den — её
				// знаменатель для конфигурации AV1.
				var n, den uint32
				// err сообщает об ошибке разбора рациональной частоты кадров ffprobe.
				if _, err := fmt.Sscanf(s.FrameRate, "%d/%d", &n, &den); err != nil {
					return media.CodecConfig{}, err
				}
				if d.Width != s.Width || d.Height != s.Height || s.CodecName != "av1" || s.PixelFormat != "yuv420p" {
					return media.CodecConfig{}, errors.New("init не совпадает с независимым ffprobe")
				}
				return media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: 11, Revision: 1, TimeBase: d.TimeBase, Video: core.VideoFormat{Codec: core.CodecAV1, Width: s.Width, Height: s.Height, FrameRate: core.FrameRate{Numerator: n, Denominator: den}, CodecProfile: s.Profile, PixelFormat: s.PixelFormat, BitDepth: 8, Color: core.ColorInfo{Range: "limited", Primaries: "unspecified", Transfer: "unspecified", Matrix: "unspecified"}}}, d.Headers)
			}
		}
		return media.CodecConfig{}, errors.New("ffprobe не дал видеодорожку")
	}
}

// Читает демультиплексор до EOF, сохраняя не более 512 пакетов; ошибка контейнера или превышение
// бюджета завершает тест.
func readMediaPackets(t *testing.T, d *fmp4.Demuxer) []media.Packet {
	t.Helper()
	// packets накапливает прочитанные пакеты до конца контейнера с пределом 512 элементов.
	var packets []media.Packet
	for {
		// p содержит очередной пакет демультиплексора, а err сообщает об окончании либо ошибке
		// чтения.
		p, err := d.Next()
		if errors.Is(err, io.EOF) {
			return packets
		}
		if err != nil {
			t.Fatal(err)
		}
		packets = append(packets, p)
		if len(packets) > 512 {
			t.Fatal("слишком большая синтетическая фикстура")
		}
	}
}

// Переводит знаковую метку и строковую временную базу в точное рациональное число секунд; неверная
// база завершает тест.
func seconds(t *testing.T, ticks int64, base string) *big.Rat {
	t.Helper()
	// b содержит разобранную рациональную временную базу, а ok подтверждает допустимость её
	// строкового представления.
	b, ok := new(big.Rat).SetString(base)
	if !ok {
		t.Fatal(base)
	}
	return new(big.Rat).Mul(new(big.Rat).SetInt64(ticks), b)
}

// Сверяет количество и порядок пакетов каждой дорожки и точные рациональные PTS/DTS
// Go-демультиплексора с независимым ffprobe.
func assertProbePackets(t *testing.T, probe mediaProbe, packets []media.Packet) {
	t.Helper()
	// indices сопоставляет индексы дорожек ffprobe с типизированными дорожками медиапакетов.
	indices := map[int]media.Track{}
	// bases хранит строковые временные базы ffprobe отдельно для видео и аудио.
	bases := map[media.Track]string{}
	// s обозначает очередное описание дорожки ffprobe для построения таблиц соответствия.
	for _, s := range probe.Streams {
		// track определяет тип медиадорожки из наблюдаемого CodecType ffprobe.
		track := media.Track(s.CodecType)
		indices[s.Index] = track
		bases[track] = s.TimeBase
	}
	// stamp хранит пару точных времён пакета ffprobe для сравнения с Go-демультиплексором.
	type stamp struct { /* pts содержит точное время показа эталонного пакета, а dts — время его декодирования в рациональных секундах. */
		pts, dts *big.Rat
	}
	// want содержит ожидаемые PTS и DTS в секундах отдельно для каждой дорожки.
	want := map[media.Track][]stamp{}
	// p обозначает очередной пакет ffprobe для построения эталонной временной последовательности.
	for _, p := range probe.Packets {
		// track определяет дорожку пакета ffprobe через её индекс в контейнере.
		track := indices[p.StreamIndex]
		want[track] = append(want[track], stamp{seconds(t, p.PTS, bases[track]), seconds(t, p.DTS, bases[track])})
	}
	// seen считает уже сопоставленные пакеты каждой дорожки Go-демультиплексора.
	seen := map[media.Track]int{}
	// p обозначает очередной пакет Go-демультиплексора для сравнения с ffprobe.
	for _, p := range packets {
		// track определяет дорожку проверяемого Go-пакета и её эталонный ряд.
		track := p.Config().Info().Track
		// i задаёт порядковую позицию пакета внутри его дорожки для сравнения с эталоном.
		i := seen[track]
		seen[track]++
		if i >= len(want[track]) {
			t.Fatal("Go добавил пакет")
		}
		// base задаёт временную базу текущего Go-пакета.
		base := p.Config().Info().TimeBase
		// baseText содержит рациональную временную базу Go-пакета в строковом виде для точного
		// сравнения секунд.
		baseText := fmt.Sprintf("%d/%d", base.Numerator, base.Denominator)
		if seconds(t, p.Info().PTS, baseText).Cmp(want[track][i].pts) != 0 || seconds(t, p.Info().DTS, baseText).Cmp(want[track][i].dts) != 0 {
			t.Fatalf("%s[%d] потерял рациональные PTS/DTS: %+v, ожидалось %s/%s", track, i, p.Info(), want[track][i].pts, want[track][i].dts)
		}
	}
	// track обозначает проверяемую дорожку, а list содержит все ожидаемые временные метки её
	// пакетов.
	for track, list := range want {
		if seen[track] != len(list) {
			t.Fatalf("число пакетов %s: %d != %d", track, seen[track], len(list))
		}
	}
}

// integrationOwner представляет тестовый допуск выполнения, который не отзывается в этом сценарии.
type integrationOwner struct { /* done содержит переданный тестом канал завершения; тестовый владелец сам его не закрывает. */
	done chan struct{}
}

// Do отклоняет уже отменённый контекст, затем синхронно выполняет действие. Этот тестовый владелец не
// моделирует отзыв во время действия.
func (o integrationOwner) Do(ctx context.Context, fn func() error) error {
	// err сообщает, был ли контекст отменён до синхронного выполнения действия тестового владельца.
	if err := ctx.Err(); err != nil {
		return err
	}
	return fn()
}

// Valid всегда подтверждает допуск тестового владельца, который не отзывается в данном сценарии.
func (o integrationOwner) Valid() bool { return true }

// Done возвращает канал завершения, переданный тестом; сам владелец его не закрывает.
func (o integrationOwner) Done() <-chan struct{} { return o.done }

// splitWriter ограничивает отдельную запись семью байтами для проверки обработки коротких записей fMP4.
type splitWriter struct { /* w принимает короткие записи контейнера от тестовой обёртки. */
	w io.Writer
}

// Write передаёт вложенному io.Writer не более семи байтов за вызов, чтобы проверить обработку
// коротких записей контейнера.
func (w splitWriter) Write(p []byte) (int, error) {
	if len(p) > 7 {
		p = p[:7]
	}
	return w.w.Write(p)
}

// splitReader ограничивает отдельное чтение пятью байтами для проверки фрагментированного ввода fMP4.
type splitReader struct { /* r предоставляет исходные байты контейнера для чтения короткими фрагментами. */
	r io.Reader
}

// Read запрашивает у вложенного io.Reader не более пяти байтов за вызов, чтобы проверить чтение
// фрагментированного контейнера.
func (r splitReader) Read(p []byte) (int, error) {
	if len(p) > 5 {
		p = p[:5]
	}
	return r.r.Read(p)
}

// При заданном GOST_MEDIA_FFMPEG проверяет обмен fMP4 через реальные UDS и Supervisor при copy H.264 и
// преобразовании AV1 в H.264, включая отрицательные метки. Независимый ffprobe и декодирование сверяют
// временные шкалы, B-кадры и сохранность AAC; после завершения UDS удалены.
func TestAcceptanceCPUUDSSupervisorRoundTrip(t *testing.T) {
	// binary содержит явно заданный FFmpeg для необязательной CPU-проверки через реальные UDS.
	binary := os.Getenv("GOST_MEDIA_FFMPEG")
	if binary == "" {
		t.Skip("локальная CPU-интеграция запускается только с явно заданным FFmpeg")
	}
	// probeBinary задаёт путь ffprobe рядом с выбранным FFmpeg для независимых временных проверок.
	probeBinary := filepath.Join(filepath.Dir(binary), "ffprobe")
	// scenario выбирает копирование H.264 либо транскодирование AV1 и знак исходных временных меток.
	for _, scenario := range []string{"h264-copy", "av1-transcode", "h264-copy-negative", "av1-transcode-negative"} {
		t.Run(scenario, func(t *testing.T) {
			// Проверяет выбранные копирование H.264 либо транскодирование AV1 и знак исходных временных меток
			// через настоящий FFmpeg и UDS.

			// encoder содержит параметры кодирования синтетического исходника для выбранного
			// кодека.
			encoder := []string{"-c:v", "libx264", "-threads:v", "1", "-g", "15", "-bf", "2"}
			if strings.HasPrefix(scenario, "av1-transcode") {
				encoder = []string{"-c:v", "libaom-av1", "-threads:v", "1", "-cpu-used", "8", "-lag-in-frames", "0"}
			}
			// sign выбирает положительный либо отрицательный сдвиг исходных PTS видео и
			// аудио.
			sign := "+"
			if strings.HasSuffix(scenario, "-negative") {
				sign = "-"
			}
			// args задаёт генерацию короткого исходного A/V-потока с выбранными временными
			// сдвигами.
			args := []string{"-hide_banner", "-loglevel", "warning", "-xerror", "-copyts", "-f", "lavfi", "-i", "testsrc2=size=128x128:rate=30:duration=0.4", "-f", "lavfi", "-i", "sine=sample_rate=48000:frequency=750:duration=0.4", "-map", "0:v", "-map", "1:a", "-vf", "setpts=PTS" + sign + "15", "-af", "asetpts=PTS" + sign + "6000", "-fps_mode", "passthrough"}
			args = append(args, encoder...)
			args = append(args, "-c:a", "aac", "-f", "mp4", "-movflags", "delay_moov+default_base_moof+frag_every_frame", "-min_frag_duration", "1", "-use_editlist", "1", "-movie_timescale", "48000", "-avoid_negative_ts", "disabled", "pipe:1")
			// source содержит исходный fMP4 с синтетическим видео и тоном 750 Гц.
			source := mediaCommand(t, binary, nil, args...)
			// oracle содержит независимые сведения ffprobe об исходном контейнере.
			oracle := probeMedia(t, probeBinary, source)
			// d читает исходный fMP4 с проверяемым разрешением конфигураций, а err сообщает
			// об ошибке его создания.
			d, err := fmp4.NewDemuxer(bytes.NewReader(source), fmp4.DemuxOptions{Limits: integrationLimits(), SourceEpoch: 11, VideoRevision: 1, AudioRevision: 1, ResolveConfig: truthfulResolver(t, oracle)})
			if err != nil {
				t.Fatal(err)
			}
			// packets содержит все исходные пакеты, сверенные с независимым ffprobe.
			packets := readMediaPackets(t, d)
			assertProbePackets(t, oracle, packets)
			// init содержит начальные конфигурации дорожек и общий сдвиг контейнерного
			// времени для отправки FFmpeg.
			init := d.Init()
			init.Origin = fmp4.OriginMapping{OffsetTicks: 3, TimeBase: media.TimeBase{Numerator: 1, Denominator: 1}}
			// scale задаёт общую целочисленную шкалу времени контейнера для обеих дорожек.
			scale, err := fmp4.MovieTimeScale(init.Video.Info().TimeBase, init.Audio.Info().TimeBase)
			if err != nil {
				t.Fatal(err)
			}
			// parent задаёт родительский каталог коротких путей UDS; на macOS используется
			// /tmp.
			parent := os.TempDir()
			if runtime.GOOS == "darwin" {
				// Системный TMPDIR macOS слишком длинный для вложенных путей UDS.
				parent = "/tmp"
			}
			// base задаёт частный короткий каталог сокетов текущего CPU-сценария.
			base, err := os.MkdirTemp(parent, "gm-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				// Удаляет частный короткий каталог UDS после завершения супервизора.

				// err сообщает об ошибке удаления частного каталога UDS.
				if err := os.RemoveAll(base); err != nil {
					t.Error(err)
				}
			})
			// s запускает и завершает FFmpeg с реальными UDS и конечными бюджетами
			// диагностики.
			s, err := ffmpeg.New(ffmpeg.Config{BaseDir: base, StartupTimeout: 10 * time.Second, VideoTimeout: 5 * time.Second, AudioTimeout: 5 * time.Second, WatchdogInterval: 50 * time.Millisecond, MaxRestarts: 0, RestartBackoff: 10 * time.Millisecond, GracePeriod: 100 * time.Millisecond, KillTimeout: time.Second, ShutdownTimeout: 3 * time.Second, DiagnosticEvents: 1024, DiagnosticBytes: 1 << 20, DiagnosticChunkBytes: 256, MaxCommandBytes: 8192, MaxCommandArgs: 128})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				// Останавливает супервизор с отдельным сроком перед удалением его каталога.

				// ctx ограничивает остановку супервизора четырьмя секундами, а cancel
				// освобождает таймер очистки.
				ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
				defer cancel()
				// err сообщает об ошибке остановки супервизора при очистке теста.
				if err := s.Stop(ctx); err != nil {
					t.Error(err)
				}
			})
			// out накапливает выходной fMP4 из UDS с ограничением 16 МиБ.
			var out boundedMediaBuffer
			out.max = 16 << 20
			// inputWire сохраняет точные отправленные FFmpeg байты fMP4 с ограничением 16
			// МиБ для диагностики отказа.
			var inputWire boundedMediaBuffer
			inputWire.max = 16 << 20
			// returned накапливает реальные выходные пакеты FFmpeg, прочитанные из UDS.
			var returned []media.Packet
			// returnedInit сохраняет конфигурации дорожек выходного fMP4 для проверки кодека
			// и AAC-заголовков.
			var returnedInit fmp4.Init
			// input проверяет представимость времён и отправляет исходные пакеты короткими
			// записями в UDS, затем закрывает направление записи.
			input := func(ctx context.Context, c net.Conn, _ ffmpeg.Progress) error {
				// Проверяет представимость временных меток и отправляет fMP4 короткими записями, затем закрывает
				// только направление записи UDS.

				// m записывает входной fMP4 одновременно в UDS и диагностическую копию,
				// а err сообщает об ошибке его создания.
				m, err := fmp4.NewMuxer(splitWriter{io.MultiWriter(c, &inputWire)}, init, integrationLimits())
				if err != nil {
					return err
				}
				// validator проверяет представимость временных меток пакетов с выбранным
				// смещением контейнера.
				validator := fmp4.NewFFmpegTimingValidatorForOrigin(init.Origin)
				// p обозначает очередной исходный пакет для проверки времени и отправки
				// FFmpeg.
				for _, p := range packets {
					// err сообщает о непредставимых временных метках исходного
					// пакета для FFmpeg.
					if err := validator.Validate(p); err != nil {
						return err
					}
					// err сообщает об отказе записи исходного пакета в fMP4 по UDS.
					if err := m.WritePacket(p); err != nil {
						return err
					}
				}
				// u предоставляет закрытие направления записи реального Unix-соединения,
				// а ok подтверждает нужный тип соединения.
				if u, ok := c.(*net.UnixConn); ok {
					// err сообщает об ошибке закрытия направления записи входного
					// UDS после отправки контейнера.
					if err := u.CloseWrite(); err != nil {
						return err
					}
				} else {
					return errors.New("ожидался настоящий UnixConn")
				}
				<-ctx.Done()
				return ctx.Err()
			}
			// output читает выходной fMP4 короткими фрагментами, сохраняет пакеты и передаёт
			// супервизору прогресс соответствующей дорожки.
			output := func(ctx context.Context, c net.Conn, progress ffmpeg.Progress) error {
				// Читает fMP4 короткими фрагментами, сохраняет реальные выходные пакеты и сообщает супервизору
				// прогресс обеих дорожек.

				// d читает выходной fMP4 с копированием прочитанных байтов в out, а err
				// сообщает об ошибке создания демультиплексора.
				d, err := fmp4.NewDemuxer(splitReader{io.TeeReader(c, &out)}, fmp4.DemuxOptions{Limits: integrationLimits(), SourceEpoch: 21, VideoRevision: 1, AudioRevision: 1})
				if err != nil {
					return err
				}
				returnedInit = d.Init()
				for {
					// p содержит очередной выходной пакет FFmpeg, а err сообщает об
					// окончании либо ошибке чтения контейнера.
					p, err := d.Next()
					if errors.Is(err, io.EOF) {
						return nil
					}
					if err != nil {
						return err
					}
					returned = append(returned, p)
					// track выбирает видео либо аудио для сообщения супервизору о
					// прогрессе прочитанного пакета.
					track := ffmpeg.Video
					if p.Config().Info().Track == media.TrackAudio {
						track = ffmpeg.Audio
					}
					progress(track, p.Info().DTS)
				}
			}
			// command строит команду копирования H.264 либо транскодирования AV1 с общим
			// контейнерным временем и сохранением AAC.
			command := func(_ context.Context, a ffmpeg.Attempt) (ffmpeg.Command, error) {
				// Создаёт команду выбранного копирования либо транскодирования с сохранением времени и общей шкалой
				// контейнера.

				// args задаёт входной и выходной UDS и выбранный режим обработки дорожек
				// FFmpeg.
				args := []string{"-hide_banner", "-nostdin", "-loglevel", "warning", "-xerror", "-copyts", "-f", "mp4", "-fflags", "+ignidx", "-i", "async:unix:" + a.InputPath, "-map", "0:v", "-map", "0:a"}
				if strings.HasPrefix(scenario, "h264-copy") {
					args = append(args, "-c", "copy")
				} else {
					args = append(args, "-c:v", "libx264", "-threads:v", "1", "-g", "15", "-bf", "2", "-fps_mode", "passthrough", "-c:a", "copy")
				}
				args = append(args, "-f", "mp4", "-movflags", "delay_moov+default_base_moof+frag_every_frame", "-min_frag_duration", "1", "-use_editlist", "1", "-movie_timescale", strconv.FormatUint(uint64(scale), 10), "-avoid_negative_ts", "disabled", "unix:"+a.OutputPath)
				return ffmpeg.Command{Binary: binary, Args: args}, nil
			}
			// ctx ограничивает полный запуск и ожидание супервизора двадцатью секундами, а
			// cancel освобождает его таймер.
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			// err сообщает об отказе запуска рабочего процесса FFmpeg и его
			// UDS-обработчиков.
			if err := s.Start(ctx, ffmpeg.Worker{SessionID: "cpu-session", GroupID: "cpu-group", Owner: integrationOwner{make(chan struct{})}, Command: command, Input: input, Output: output}); err != nil {
				t.Fatal(err)
			}
			// result содержит итог супервизора с количеством попыток и подтверждением
			// очистки ресурсов.
			result, err := s.Wait(ctx)
			if err != nil {
				t.Fatal(err)
			}
			// diagnostics накапливает stderr завершившегося FFmpeg для проверки
			// переписывания временных меток.
			var diagnostics []byte
			for {
				// event содержит очередное событие диагностики супервизора, а err
				// сообщает об окончании либо ошибке чтения диагностики.
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
				mediaCommand(t, binary, inputWire.Bytes(), "-hide_banner", "-loglevel", "warning", "-xerror", "-copyts", "-f", "mp4", "-i", "pipe:0", "-map", "0:v", "-map", "0:a", "-c", "copy", "-f", "null", "-")
				t.Log("те же входные байты успешно прочитаны FFmpeg через pipe")
				mediaCommand(t, binary, inputWire.Bytes(), "-hide_banner", "-loglevel", "warning", "-xerror", "-copyts", "-f", "mp4", "-i", "pipe:0", "-map", "0:v", "-map", "0:a", "-f", "null", "-")
				t.Log("те же входные байты успешно декодированы FFmpeg через pipe")
				t.Fatalf("UDS/Supervisor: %+v: %s", result, diagnostics)
			}
			if bytes.Contains(diagnostics, []byte("Invalid DTS")) || bytes.Contains(diagnostics, []byte("Non-monotonic")) {
				t.Fatalf("DTS переписан: %s", diagnostics)
			}
			// returnedOracle содержит независимые сведения ffprobe о реальном выходном
			// контейнере FFmpeg.
			returnedOracle := probeMedia(t, probeBinary, out.Bytes())
			assertProbePackets(t, returnedOracle, returned)
			if strings.HasPrefix(scenario, "h264-copy") {
				assertProbePackets(t, oracle, returned)
			}
			if returnedInit.Video.Info().Video.Codec != core.CodecH264 || !bytes.Equal(returnedInit.Audio.Headers(), init.Audio.Headers()) {
				t.Fatal("выход потерял codec/extradata")
			}
			// bframes показывает наличие выходного видеопакета с различными PTS и DTS.
			bframes := false
			// videoFrames считает выходные видеопакеты для проверки сохранения двенадцати
			// кадров.
			videoFrames := 0
			// presentation хранит разницу количества исходных и выходных кадров для каждого
			// точного PTS в секундах.
			presentation := make(map[string]int)
			// wantAudio содержит исходные AAC-пакеты, а gotAudio — выходные AAC-пакеты для
			// попарной проверки содержимого и DTS.
			var wantAudio, gotAudio []media.Packet
			// p обозначает очередной исходный пакет для построения аудиоэталона и множества
			// видеовремён.
			for _, p := range packets {
				if p.Config().Info().Track == media.TrackAudio {
					wantAudio = append(wantAudio, p)
				} else {
					// b задаёт временную базу исходного видеопакета для учёта
					// точного времени показа.
					b := p.Config().Info().TimeBase
					presentation[seconds(t, p.Info().PTS, fmt.Sprintf("%d/%d", b.Numerator, b.Denominator)).RatString()]++
				}
			}
			// p обозначает очередной выходной пакет для подсчёта кадров и сравнения аудио.
			for _, p := range returned {
				if p.Config().Info().Track == media.TrackVideo {
					videoFrames++
					bframes = bframes || p.Info().PTS != p.Info().DTS
					// b задаёт временную базу выходного видеопакета для сравнения
					// его PTS с исходным.
					b := p.Config().Info().TimeBase
					presentation[seconds(t, p.Info().PTS, fmt.Sprintf("%d/%d", b.Numerator, b.Denominator)).RatString()]--
				} else {
					gotAudio = append(gotAudio, p)
				}
			}
			if videoFrames != 12 || !bframes || len(wantAudio) != len(gotAudio) {
				t.Fatalf("число кадров или B-кадры потеряны: video=%d B=%v audio=%d/%d", videoFrames, bframes, len(wantAudio), len(gotAudio))
			}
			// pts содержит точное время показа видеокадра в секундах, а count — разницу
			// числа исходных и выходных кадров с этим временем.
			for pts, count := range presentation {
				if count != 0 {
					t.Fatalf("транскодирование изменило исходный video PTS=%s: delta=%d", pts, count)
				}
			}
			// i задаёт позицию AAC-пакета в аудиодорожке, а want содержит исходный пакет
			// этой позиции.
			for i, want := range wantAudio {
				// got содержит соответствующий выходной AAC-пакет для проверки
				// сохранения байтов и DTS.
				got := gotAudio[i]
				// wb задаёт временную базу исходного AAC-пакета, а gb — временную базу
				// выходного для сравнения времени в секундах.
				wb, gb := want.Config().Info().TimeBase, got.Config().Info().TimeBase
				if !bytes.Equal(want.Payload(), got.Payload()) || seconds(t, want.Info().DTS, fmt.Sprintf("%d/%d", wb.Numerator, wb.Denominator)).Cmp(seconds(t, got.Info().DTS, fmt.Sprintf("%d/%d", gb.Numerator, gb.Denominator))) != 0 {
					t.Fatalf("AAC пакет %d изменился", i)
				}
			}
			// decoded содержит строки framemd5 независимого декодирования выходного
			// A/V-контейнера.
			decoded := mediaCommand(t, binary, out.Bytes(), "-hide_banner", "-loglevel", "warning", "-xerror", "-copyts", "-i", "pipe:0", "-map", "0:v", "-map", "0:a", "-fps_mode", "passthrough", "-f", "framemd5", "pipe:1")
			// videoDecoded считает декодированные видеокадры, а audioDecoded —
			// декодированные аудиокадры по выводу framemd5.
			videoDecoded, audioDecoded := 0, 0
			// line обозначает очередную строку framemd5 для подсчёта декодированных кадров
			// дорожек.
			for line := range strings.SplitSeq(string(decoded), "\n") {
				if strings.HasPrefix(line, "0,") {
					videoDecoded++
				}
				if strings.HasPrefix(line, "1,") {
					audioDecoded++
				}
			}
			if videoDecoded != 12 || audioDecoded == 0 {
				t.Fatalf("не все кадры декодированы: video=%d audio=%d", videoDecoded, audioDecoded)
			}
			// entries содержит оставшиеся объекты каталога UDS после завершения супервизора.
			entries, err := os.ReadDir(base)
			if err != nil || len(entries) != 0 {
				t.Fatalf("UDS остались после join: %v %v", entries, err)
			}
		})
	}
}
