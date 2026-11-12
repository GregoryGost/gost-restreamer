package fallback

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"image"
	_ "image/jpeg"
	_ "image/png"
	"io"
	"math"
	"math/big"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

// ProbeLimits ограничивает метаданные, геометрию, длительность и объём проверяемого медиапотока.
type ProbeLimits struct {
	// MaxJSONBytes ограничивает объём JSON ffprobe в байтах.
	MaxJSONBytes uint64
	// MaxWidth задаёт наибольшую допустимую ширину в пикселях. MaxHeight задаёт наибольшую
	// допустимую высоту в пикселях.
	MaxWidth, MaxHeight uint32
	// MaxPixels ограничивает произведение ширины и высоты изображения или видео.
	MaxPixels uint64
	// MaxDuration ограничивает длительность проверяемого файла. ImageDuration задаёт конечную
	// длительность неподвижного изображения для подготовки.
	MaxDuration, ImageDuration time.Duration
	// MaxPackets ограничивает число проверяемых единиц доступа.
	MaxPackets uint64
	// Container задаёт ограничения разбора и записи fMP4.
	Container fmp4.Limits
}

// Validate проверяет конечные пределы метаданных, размеров, длительности и пакетов, затем ограничения
// контейнера.
func (l ProbeLimits) Validate() error {
	if l.MaxJSONBytes == 0 || l.MaxJSONBytes > math.MaxInt32 || l.MaxWidth == 0 || l.MaxHeight == 0 || l.MaxPixels == 0 || l.MaxDuration <= 0 || l.ImageDuration <= 0 || l.ImageDuration > l.MaxDuration || l.MaxPackets == 0 {
		return ErrInvalid
	}
	return l.Container.Validate()
}

// Prober проверяет исходные и подготовленные файлы через ограниченный ffprobe и разбор сжатых дорожек.
type Prober struct {
	// Binary задаёт исполняемый файл ffprobe.
	Binary string
	// Runner выполняет ffprobe с ограничениями времени и вывода.
	Runner ProcessRunner
	// Limits содержит конечные бюджеты проверки файлов.
	Limits ProbeLimits
}

// probeStream принимает фактическое описание одной дорожки ffprobe.
type probeStream struct {
	// Type содержит тип дорожки из codec_type.
	Type string `json:"codec_type"`
	// Codec содержит имя кодека из codec_name.
	Codec string `json:"codec_name"`
	// Profile содержит обозначение профиля кодека.
	Profile string `json:"profile"`
	// Width задаёт сообщённую ffprobe ширину видео в пикселях.
	Width uint32 `json:"width"`
	// Height задаёт сообщённую ffprobe высоту видео в пикселях.
	Height uint32 `json:"height"`
	// Rate содержит рациональную запись средней частоты кадров.
	Rate string `json:"avg_frame_rate"`
	// Pixel содержит имя формата пикселей ffprobe.
	Pixel string `json:"pix_fmt"`
	// Level содержит числовой уровень видеокодека.
	Level int `json:"level"`
	// ColorRange содержит обозначение диапазона цвета ffprobe.
	ColorRange string `json:"color_range"`
	// Primaries содержит обозначение основных цветов.
	Primaries string `json:"color_primaries"`
	// Transfer содержит обозначение передаточной характеристики.
	Transfer string `json:"color_transfer"`
	// Matrix содержит обозначение цветовой матрицы.
	Matrix string `json:"color_space"`
	// SampleRate содержит десятичную запись частоты дискретизации аудио в герцах.
	SampleRate string `json:"sample_rate"`
	// Channels задаёт число аудиоканалов.
	Channels uint16 `json:"channels"`
	// Layout содержит обозначение расположения аудиоканалов.
	Layout string `json:"channel_layout"`
	// StartTime содержит заявленное время начала дорожки в секундах.
	StartTime string `json:"start_time"`
}

// probeDocument принимает JSON ffprobe с описаниями дорожек и контейнера.
type probeDocument struct {
	// Streams содержит обнаруженные дорожки файла.
	Streams []probeStream `json:"streams"`
	// Format содержит сообщённые ffprobe имя контейнера и длительность файла.
	Format struct {
		// Name содержит имя или список совместимых имён контейнера ffprobe.
		Name string `json:"format_name"`
		// Duration содержит десятичную запись длительности контейнера в секундах.
		Duration string `json:"duration"`
	} `json:"format"`
}

// Читает ограниченный JSON ffprobe через FD 3 с начала файла; допускает от одной до двух дорожек и
// только перечисленные демультиплексоры.
func (p Prober) inspect(ctx context.Context, f *os.File) (probeDocument, error) {
	if p.Binary == "" || p.Runner == nil || p.Limits.Validate() != nil {
		return probeDocument{}, ErrInvalid
	}
	// err сообщает об отказе перемотки файла перед проверкой ffprobe.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return probeDocument{}, err
	}
	// out принимает ограниченный JSON с описаниями файла.
	var out bytes.Buffer
	// cmd содержит команду ffprobe для чтения через FD 3 с ограниченным списком протоколов и
	// форматов.
	cmd := Command{Binary: p.Binary, Args: []string{"-v", "error", "-protocol_whitelist", "fd,pipe", "-format_whitelist", "mov,matroska,png_pipe,jpeg_pipe", "-fd", "3", "-i", "fd:", "-show_streams", "-show_format", "-of", "json"}}
	// err сообщает об отказе запуска ffprobe или записи его ограниченного вывода.
	_, err := p.Runner.Run(ctx, cmd, f, &boundedWriter{w: &out, remaining: p.Limits.MaxJSONBytes})
	if err != nil {
		return probeDocument{}, err
	}
	// doc принимает разобранное описание дорожек и контейнера.
	var doc probeDocument
	if err = json.Unmarshal(out.Bytes(), &doc); err != nil {
		return doc, err
	}
	if len(doc.Streams) == 0 || len(doc.Streams) > 2 {
		return doc, ErrInvalid
	}
	return doc, nil
}

// Проверяет ненулевые размеры изображения и отдельные пределы ширины, высоты и числа пикселей.
func (p Prober) dimensions(w, h uint32) error {
	if w == 0 || h == 0 || w > p.Limits.MaxWidth || h > p.Limits.MaxHeight || uint64(w)*uint64(h) > p.Limits.MaxPixels {
		return ErrLimit
	}
	return nil
}

// Source проверяет содержимое изображения либо фактическое описание дорожек видео.
func (p Prober) Source(ctx context.Context, f *os.File, kind core.AssetKind) (Source, error) {
	if p.Limits.Validate() != nil {
		return Source{}, ErrInvalid
	}
	if kind == core.AssetImage {
		// err сообщает об отказе перемотки файла перед чтением заголовка изображения.
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return Source{}, err
		}
		// c содержит размеры и цветовую модель изображения. format содержит распознанное имя
		// формата. err сообщает об отказе чтения заголовка.
		c, format, err := image.DecodeConfig(contextReader{ctx, f})
		if err != nil {
			return Source{}, err
		}
		if format != "png" && format != "jpeg" || c.Width <= 0 || c.Height <= 0 || uint64(c.Width) > math.MaxUint32 || uint64(c.Height) > math.MaxUint32 {
			return Source{}, ErrInvalid
		}
		if err = p.dimensions(uint32(c.Width), uint32(c.Height)); err != nil {
			return Source{}, err
		}
		return Source{Kind: kind, Width: uint32(c.Width), Height: uint32(c.Height), Format: core.SourceFormat{ImageFormat: format}, Duration: p.Limits.ImageDuration, Demuxer: format + "_pipe"}, nil
	}
	if kind != core.AssetVideo {
		return Source{}, ErrInvalid
	}
	// doc содержит описание видеофайла ffprobe. err сообщает об отказе его получения.
	doc, err := p.inspect(ctx, f)
	if err != nil {
		return Source{}, err
	}
	// s накапливает проверенные свойства исходного видеофайла.
	s := Source{Kind: kind}
	switch doc.Format.Name {
	case "mov,mp4,m4a,3gp,3g2,mj2":
		s.Demuxer = "mov"
	case "matroska,webm":
		s.Demuxer = "matroska"
	default:
		return s, ErrInvalid
	}
	// duration содержит длительность контейнера в секундах для проверки конечности и предела.
	duration, err := strconv.ParseFloat(doc.Format.Duration, 64)
	if err != nil || math.IsNaN(duration) || math.IsInf(duration, 0) || duration <= 0 || duration > p.Limits.MaxDuration.Seconds() {
		return s, ErrLimit
	}
	s.Duration = time.Duration(duration * float64(time.Second))
	// t содержит очередную дорожку исходного файла.
	for _, t := range doc.Streams {
		// origin содержит точное время начала дорожки в секундах. known показывает, удалось ли
		// его подтвердить из start_time.
		origin, known := new(big.Rat).SetString(t.StartTime)
		if !known || origin.Sign() != 0 {
			return s, fmt.Errorf("%w: поддержан только подтверждённый нулевой source A/V offset", core.ErrNotReady)
		}
		switch t.Type {
		case "video":
			if s.Format.Video != nil {
				return s, ErrInvalid
			}
			if err = p.dimensions(t.Width, t.Height); err != nil {
				return s, err
			}
			// v содержит проверенное описание исходного видео. e сообщает о неподдержанном
			// или неполном видеопараметре.
			v, e := t.video()
			if e != nil {
				return s, e
			}
			s.Format.Video = &v
			s.Width, s.Height = t.Width, t.Height
		case "audio":
			if s.Format.Audio != nil {
				return s, ErrInvalid
			}
			// a содержит проверенное описание исходного аудио. e сообщает о некорректных или
			// неполных аудиопараметрах.
			a, e := t.audio()
			if e != nil {
				return s, e
			}
			s.Format.Audio = &a
		default:
			return s, ErrInvalid
		}
	}
	if s.Format.Video == nil {
		return s, ErrInvalid
	}
	return s, nil
}

// Преобразует проверяемые поля ffprobe в формат H.264 или AV1 с yuv420p; отсутствующие сведения о
// цвете сохраняет как unspecified.
func (s probeStream) video() (core.VideoFormat, error) {
	// rate содержит проверенную рациональную частоту кадров. err сообщает о некорректной записи FPS.
	rate, err := parseRate(s.Rate)
	if err != nil {
		return core.VideoFormat{}, err
	}
	// depth задаёт восьмибитную глубину единственного поддержанного формата yuv420p.
	depth := uint8(8)
	if s.Pixel != "yuv420p" {
		return core.VideoFormat{}, ErrInvalid
	}
	// profile содержит приведённое к доменному обозначению имя профиля видеокодека.
	profile := strings.ToLower(s.Profile)
	if profile == "constrained baseline" {
		profile = "constrained-baseline"
	}
	// level содержит доменное обозначение уровня H.264 или AV1.
	level := ""
	// tier содержит main для AV1 либо пустую строку для H.264.
	tier := ""
	switch s.Codec {
	case "h264":
		level = fmt.Sprintf("%d.%d", s.Level/10, s.Level%10)
	case "av1":
		level = fmt.Sprintf("%d.%d", 2+s.Level/4, s.Level%4)
		tier = "main"
	default:
		return core.VideoFormat{}, ErrInvalid
	}
	// rangeName содержит диапазон цвета после преобразования tv/pc в limited/full.
	rangeName := s.ColorRange
	if rangeName == "tv" {
		rangeName = "limited"
	}
	if rangeName == "pc" {
		rangeName = "full"
	}
	// v накапливает наблюдаемый видеоформат без подстановки отсутствующих цветовых сведений.
	v := core.VideoFormat{Codec: core.Codec(s.Codec), Width: s.Width, Height: s.Height, FrameRate: rate, CodecProfile: profile, Level: level, Tier: tier, PixelFormat: s.Pixel, BitDepth: depth, Color: core.ColorInfo{Range: rangeName, Primaries: s.Primaries, Transfer: s.Transfer, Matrix: s.Matrix}}
	// Источник может не сигнализировать цвет; неизвестные значения не выдаются за профиль.
	if v.Color.Range == "" {
		v.Color.Range = "unspecified"
	}
	if v.Color.Primaries == "" {
		v.Color.Primaries = "unspecified"
	}
	if v.Color.Transfer == "" {
		v.Color.Transfer = "unspecified"
	}
	if v.Color.Matrix == "" {
		v.Color.Matrix = "unspecified"
	}
	return v, v.Validate()
}

// Разбирает частоту дискретизации и проверяет аудиоформат, не подставляя отсутствующие каналы или
// раскладку.
func (s probeStream) audio() (core.AudioFormat, error) {
	// rate содержит частоту дискретизации в герцах. err сообщает о некорректной десятичной записи.
	rate, err := strconv.ParseUint(s.SampleRate, 10, 32)
	if err != nil {
		return core.AudioFormat{}, ErrInvalid
	}
	// a содержит аудиоформат, составленный исключительно из сообщённых ffprobe полей.
	a := core.AudioFormat{Codec: core.Codec(s.Codec), CodecProfile: s.Profile, SampleRateHz: uint32(rate), Channels: s.Channels, ChannelLayout: s.Layout}
	return a, a.Validate()
}

// Разбирает положительную рациональную частоту вида numerator/denominator в пределах uint32.
func parseRate(s string) (core.FrameRate, error) {
	// v содержит две части рациональной записи частоты.
	v := strings.Split(s, "/")
	if len(v) != 2 {
		return core.FrameRate{}, ErrInvalid
	}
	// n содержит числитель частоты. e сообщает о невозможности прочитать его как uint32.
	n, e := strconv.ParseUint(v[0], 10, 32)
	if e != nil {
		return core.FrameRate{}, ErrInvalid
	}
	// d содержит знаменатель частоты после проверки десятичной записи.
	d, e := strconv.ParseUint(v[1], 10, 32)
	if e != nil {
		return core.FrameRate{}, ErrInvalid
	}
	// r содержит прочитанную дробь для проверки положительности её частей.
	r := core.FrameRate{Numerator: uint32(n), Denominator: uint32(d)}
	return r, r.Validate()
}

// Получает ровно по одной видео- и аудиодорожке из ffprobe, проверяет полный формат, размеры и
// конечную длительность файла.
func (p Prober) observedFormat(ctx context.Context, f *os.File) (core.MediaFormat, error) {
	// doc содержит описание подготовленного файла ffprobe. err сообщает об отказе его получения.
	doc, err := p.inspect(ctx, f)
	if err != nil {
		return core.MediaFormat{}, err
	}
	// actual накапливает ровно по одному наблюдаемому формату видео и аудио.
	var actual core.MediaFormat
	// s содержит очередную дорожку для построения полного наблюдаемого формата.
	for _, s := range doc.Streams {
		switch s.Type {
		case "video":
			if actual.Video.Codec != "" {
				return actual, ErrInvalid
			}
			actual.Video, err = s.video()
		case "audio":
			if actual.Audio.Codec != "" {
				return actual, ErrInvalid
			}
			actual.Audio, err = s.audio()
		default:
			err = ErrInvalid
		}
		if err != nil {
			return actual, err
		}
	}
	if err = actual.Validate(); err != nil {
		return actual, err
	}
	if err = p.dimensions(actual.Video.Width, actual.Video.Height); err != nil {
		return actual, err
	}
	// duration содержит сообщённую длительность подготовленного файла в секундах.
	duration, err := strconv.ParseFloat(doc.Format.Duration, 64)
	if err != nil || math.IsNaN(duration) || math.IsInf(duration, 0) || duration <= 0 || duration > p.Limits.MaxDuration.Seconds() {
		return actual, ErrLimit
	}
	return actual, nil
}

// Verify проверяет фактические заголовки и единицы доступа, частоту кадров,
// структуру GOP и продвижение обеих дорожек всего файла.
func (p Prober) Verify(ctx context.Context, f *os.File, profile core.GroupProfile) (core.MediaFormat, error) {
	// actual содержит полный наблюдаемый формат файла. err сообщает об отказе его определения.
	actual, err := p.observedFormat(ctx, f)
	if err != nil {
		return actual, err
	}
	if codec.Compatible(actual, profile.Format) != nil {
		return actual, fmt.Errorf("%w: фактический формат отличается от профиля", core.ErrNotReady)
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return actual, err
	}
	// d читает подготовленный fMP4 со сверкой конфигураций контейнера и фактического формата.
	d, err := fmp4.NewDemuxer(contextReader{ctx, f}, fmp4.DemuxOptions{Limits: p.Limits.Container, SourceEpoch: 1, VideoRevision: 1, AudioRevision: 1, ResolveConfig: func(desc fmp4.TrackDescription) (media.CodecConfig, error) {
		// Сверяет кодек, размеры и заголовки дорожки fMP4 с фактическим форматом ffprobe; конфигурацию H.264 и
		// AAC получает из заголовков, а расхождения возвращает как ErrCorrupt.

		// info накапливает проверенное описание дорожки по заголовкам контейнера и наблюдаемому
		// формату.
		info := media.CodecConfigInfo{Track: desc.Track, SourceEpoch: 1, Revision: 1, TimeBase: desc.TimeBase}
		if desc.Track == media.TrackVideo {
			if desc.Codec != actual.Video.Codec || desc.Width != actual.Video.Width || desc.Height != actual.Video.Height {
				return media.CodecConfig{}, core.ErrCorrupt
			}
			if desc.Codec == core.CodecAV1 {
				_, err = codec.ParseAV1Config(desc.Headers)
				info.Video = actual.Video
			} else {
				info.Video, err = codec.H264Format(codec.VideoDescription{Headers: desc.Headers, Width: desc.Width, Height: desc.Height})
				if err == nil && codec.Compatible(core.MediaFormat{Video: info.Video, Audio: actual.Audio}, actual) != nil {
					err = core.ErrCorrupt
				}
			}
		} else {
			info.Audio, err = codec.AudioFormatASC(desc.Headers)
			if err == nil && info.Audio != actual.Audio {
				err = core.ErrCorrupt
			}
		}
		if err != nil {
			return media.CodecConfig{}, err
		}
		return media.NewCodecConfig(info, desc.Headers)
	}})
	if err != nil {
		return actual, fmt.Errorf("инициализация fMP4: %w", err)
	}
	// counts считает проверенные пакеты отдельно для каждой дорожки.
	counts := map[media.Track]uint64{}
	// last хранит последний проверенный DTS каждой дорожки для контроля строгого продвижения.
	last := map[media.Track]int64{}
	// lastKey хранит номер последнего независимого видеокадра для проверки длины GOP.
	lastKey := uint64(0)
	// inspector проверяет фактические сжатые единицы доступа и сохраняет состояние
	// последовательности AV1.
	var inspector codec.Inspector
	// total считает все прочитанные пакеты для соблюдения MaxPackets.
	for total := uint64(0); ; total++ {
		// packet содержит очередную единицу доступа fMP4. e сообщает об окончании файла или
		// отказе чтения.
		packet, e := d.Next()
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			return actual, e
		}
		if total >= p.Limits.MaxPackets {
			return actual, ErrLimit
		}
		// info содержит метки и признаки пакета. c содержит конфигурацию его дорожки.
		info, c := packet.Info(), packet.Config()
		// track обозначает дорожку пакета для отдельных счётчиков и проверок времени.
		track := c.Info().Track
		if info.DurationTicks <= 0 || counts[track] > 0 && info.DTS <= last[track] {
			return actual, core.ErrCorrupt
		}
		// key показывает независимость кадра по фактическим сжатым данным.
		key, e := inspector.Inspect(packet)
		if e != nil {
			return actual, fmt.Errorf("единица доступа %s[%d]: %w", track, counts[track], e)
		}
		if track == media.TrackVideo {
			if counts[track] == 0 && !key {
				return actual, core.ErrNotReady
			}
			if key {
				lastKey = counts[track]
			}
			if counts[track]-lastKey >= uint64(profile.VideoEncoding.GOPFrames) {
				return actual, core.ErrNotReady
			}
			if !exactDuration(info.DurationTicks, c.Info().TimeBase, actual.Video.FrameRate) {
				return actual, fmt.Errorf("%w: video duration %d в %v", core.ErrCorrupt, info.DurationTicks, c.Info().TimeBase)
			}
			if profile.VideoEncoding.MaxBFrames == 0 && info.PTS != info.DTS {
				return actual, core.ErrCorrupt
			}
		} else if !exactAudioDuration(info.DurationTicks, c.Info().TimeBase, actual.Audio.SampleRateHz) {
			return actual, fmt.Errorf("%w: AAC duration %d в %v", core.ErrCorrupt, info.DurationTicks, c.Info().TimeBase)
		}
		counts[track]++
		last[track] = info.DTS
	}
	if counts[media.TrackVideo] == 0 || counts[media.TrackAudio] == 0 {
		return actual, core.ErrNotReady
	}
	return actual, nil
}

// Сравнивает длительность пакета с одним видеокадром через целочисленное равенство без округления.
func exactDuration(ticks int64, b media.TimeBase, fps core.FrameRate) bool {
	// l содержит длительность видео, умноженную на числители шкалы и частоты без переполнения.
	l := new(big.Int).Mul(big.NewInt(ticks), new(big.Int).SetUint64(uint64(b.Numerator)*uint64(fps.Numerator)))
	// r содержит соответствующее произведение знаменателей для проверки точной длительности кадра.
	r := new(big.Int).SetUint64(uint64(b.Denominator) * uint64(fps.Denominator))
	return l.Cmp(r) == 0
}

// Проверяет, что длительность пакета точно равна 1024 аудиосэмплам в заданной временной базе.
func exactAudioDuration(ticks int64, b media.TimeBase, rate uint32) bool {
	// l содержит длительность аудио, умноженную на числитель шкалы и частоту дискретизации без
	// переполнения.
	l := new(big.Int).Mul(big.NewInt(ticks), new(big.Int).SetUint64(uint64(b.Numerator)*uint64(rate)))
	// r содержит знаменатель шкалы, умноженный на 1024 сэмпла полной AAC-единицы.
	r := new(big.Int).SetUint64(uint64(b.Denominator) * 1024)
	return l.Cmp(r) == 0
}
