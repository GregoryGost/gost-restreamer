package fallback

import (
	"bytes"
	"context"
	"encoding/hex"
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
	"github.com/GregoryGost/gost-restreamer/internal/media/encoding"
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
	// ExtraData содержит шестнадцатеричный дамп реального codec extradata из ffprobe.
	ExtraData string `json:"extradata"`
	// ExtraDataSize задаёт объявленный размер codec extradata в байтах для проверки полного дампа.
	ExtraDataSize uint32 `json:"extradata_size"`
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
	cmd := Command{Binary: p.Binary, Args: []string{"-v", "error", "-protocol_whitelist", "fd,pipe", "-format_whitelist", "mov,matroska,png_pipe,jpeg_pipe", "-fd", "3", "-i", "fd:", "-show_streams", "-show_format", "-show_data", "-of", "json"}}
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
			if s.Demuxer == "mov" && (t.Codec == "hevc" || t.Codec == "vp9" || t.Transfer == "smpte2084" || t.Transfer == "arib-std-b67") {
				// description подтверждает nclx и запись кодека того же файла, прежде чем разрешить уточнение CICP.
				var description sourceVideoEntry
				v, description, e = p.sourceVideoFormat(ctx, f, t)
				if e == nil {
					s.HDR.ColorConfig = description.provenance
				}
			}
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

// Преобразует ffprobe в наблюдаемый H.264/AV1/HEVC/VP9. Поддержанные плоскости и глубина проверяются явно,
// av1C/hvcC подтверждают профиль, цвет и tier; отсутствующий цвет остаётся unspecified.
func (s probeStream) video() (core.VideoFormat, error) {
	return s.videoWithExternalColor(media.ColorConfigProvenance{})
}

// videoWithExternalColor использует provenance только от проверенного nclx того же MOV/MP4.
// s по-прежнему содержит фактические поля ffprobe; признак происхождения не заменяет проверку их значений
// против дескриптора у вызывающего. Нулевой признак происхождения сохраняет прежние строгие проверки.
func (s probeStream) videoWithExternalColor(provenance media.ColorConfigProvenance) (core.VideoFormat, error) {
	// rate содержит проверенную рациональную частоту кадров. err сообщает о некорректной записи FPS.
	rate, err := parseRate(s.Rate)
	if err != nil {
		return core.VideoFormat{}, err
	}
	// pixel нормализует устаревшие имена ffprobe yuvj только при подтверждённом full range.
	// Они описывают те же восьмибитные плоскости, а диапазон уже является отдельным полем.
	pixel := s.Pixel
	if pixel == "yuvj420p" || pixel == "yuvj422p" || pixel == "yuvj444p" {
		if s.ColorRange != "pc" && s.ColorRange != "full" {
			return core.VideoFormat{}, ErrInvalid
		}
		pixel = "yuv" + strings.TrimPrefix(pixel, "yuvj")
	}
	// depth задаёт точную глубину известного плоскостного формата.
	depth := map[string]uint8{"yuv420p": 8, "yuv422p": 8, "yuv444p": 8, "gray": 8, "yuv420p10le": 10, "yuv422p10le": 10, "yuv444p10le": 10}[pixel]
	if depth == 0 {
		return core.VideoFormat{}, ErrInvalid
	}
	// profile содержит приведённое к доменному обозначению имя профиля видеокодека.
	profile := strings.ToLower(s.Profile)
	if profile == "constrained baseline" {
		profile = "constrained-baseline"
	}
	switch profile {
	case "high 10":
		profile = "high10"
	case "high 4:2:2":
		profile = "high422"
	case "high 4:4:4 predictive":
		profile = "high444"
	case "main 10":
		profile = "main10"
	case "profile 0":
		profile = "profile0"
	case "profile 2":
		profile = "profile2"
	}
	// level содержит доменное обозначение уровня H.264 или AV1.
	level := ""
	// tier содержит AV1 tier из av1C; только прежнее описание без extradata предполагает main.
	tier := ""
	switch s.Codec {
	case "h264":
		level = fmt.Sprintf("%d.%d", s.Level/10, s.Level%10)
	case "av1":
		level = fmt.Sprintf("%d.%d", 2+s.Level/4, s.Level%4)
		tier = "main"
		if s.Level == 31 {
			level = "unspecified"
		}
		if s.ExtraData != "" {
			// headers содержит реальный av1C; e сообщает об усечённом или повреждённом дампе.
			headers, e := probeExtraData(s.ExtraData, s.ExtraDataSize)
			if e != nil || len(headers) < 4 {
				return core.VideoFormat{}, core.ErrCorrupt
			}
			if headers[2]&0x80 != 0 {
				tier = "high"
			}
		}
	case "hevc":
		if s.Level > 0 {
			level = fmt.Sprintf("%d.%d", s.Level/30, s.Level%30/3)
		}
		tier = "main"
	case "vp9":
		// VP9 record не задаёт FPS или level; фактические размеры и FPS сообщает ffprobe.
		if profile != "profile0" && profile != "profile2" || profile == "profile0" && (pixel != "yuv420p" || depth != 8) || profile == "profile2" && (pixel != "yuv420p10le" || depth != 10) {
			return core.VideoFormat{}, ErrInvalid
		}
		if s.Level == 0 {
			level = "unspecified"
		} else if s.Level > 0 {
			level = fmt.Sprintf("%d.%d", s.Level/10, s.Level%10)
		}
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
	v := core.VideoFormat{Codec: core.Codec(s.Codec), Width: s.Width, Height: s.Height, FrameRate: rate, CodecProfile: profile, Level: level, Tier: tier, PixelFormat: pixel, BitDepth: depth, Color: core.ColorInfo{Range: rangeName, Primaries: s.Primaries, Transfer: s.Transfer, Matrix: s.Matrix}}
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
	if v.Codec == core.CodecAV1 && s.ExtraData != "" {
		// headers содержит повторно проверенную конфигурацию для сверки всех сообщённых полей;
		// e сообщает об отказе чтения или расхождении фактического av1C и ffprobe.
		headers, e := probeExtraData(s.ExtraData, s.ExtraDataSize)
		if e != nil {
			return core.VideoFormat{}, e
		}
		if e = codec.ValidateVideoHeaders(headers, v, provenance); e != nil {
			return core.VideoFormat{}, e
		}
	}
	if v.Codec == core.CodecHEVC {
		// headers содержит фактический hvcC для проверки уровня, категории и ограничений кодека;
		// parseErr сообщает о повреждённом либо усечённом дампе.
		headers, parseErr := probeExtraData(s.ExtraData, s.ExtraDataSize)
		if parseErr != nil {
			return core.VideoFormat{}, parseErr
		}
		// coded содержит фактические сведения SPS без подстановки желаемых значений.
		coded, parseErr := codec.HEVCFormat(codec.VideoDescription{Headers: headers, Width: v.Width, Height: v.Height})
		if parseErr != nil && (!errors.Is(parseErr, codec.ErrUnready) || coded.Codec != core.CodecHEVC || coded.FrameRate.Validate() == nil) {
			return core.VideoFormat{}, parseErr
		}
		v.Tier = coded.Tier
		if coded.FrameRate.Numerator == 0 {
			coded.FrameRate = v.FrameRate
		}
		if parseErr = codec.CompatibleVideoConstraints(coded, v, provenance); parseErr != nil {
			return core.VideoFormat{}, parseErr
		}
	}
	return v, v.Validate()
}

// audio восстанавливает доступное описание аудио из фактических полей s и заголовков ffprobe.
// Для Opus/FLAC/MP3/AC-3 делегирует проверку конечной привязке; полные AU отдельно подтверждает контейнер.
// Для AAC карта ASC/PCE имеет приоритет над channel_layout: FFmpeg может не назвать PCE либо выдать
// обычную 7.1 вместо нормативной 7.1(wide). Без ASC сохраняется прежняя проверка mono/stereo;
// многоканальное описание без ASC не подтверждает расположение каналов.
func (s probeStream) audio() (core.AudioFormat, error) {
	// rate содержит частоту дискретизации в герцах. err сообщает о некорректной десятичной записи.
	rate, err := strconv.ParseUint(s.SampleRate, 10, 32)
	if err != nil {
		return core.AudioFormat{}, ErrInvalid
	}
	if s.Codec != string(core.CodecAAC) {
		return s.boundAudio(uint32(rate))
	}

	// a содержит аудиоформат, составленный исключительно из сообщённых ffprobe полей.
	a := core.AudioFormat{Codec: core.Codec(s.Codec), CodecProfile: s.Profile, SampleRateHz: uint32(rate), Channels: s.Channels, ChannelLayout: s.Layout}
	if s.ExtraData != "" {
		// headers содержит исходные байты ASC из дампа, а err сообщает об усечении или неверной записи.
		headers, err := probeExtraData(s.ExtraData, s.ExtraDataSize)
		if err != nil {
			return core.AudioFormat{}, err
		}
		// actual содержит полную карту AAC, установленную общим разбором ASC/PCE без desired-профиля.
		actual, err := codec.AudioFormatASC(headers)
		if err != nil {
			return core.AudioFormat{}, err
		}
		if a.Codec != actual.Codec || a.CodecProfile != actual.CodecProfile || a.SampleRateHz != actual.SampleRateHz || a.Channels != actual.Channels {
			return core.AudioFormat{}, core.ErrCorrupt
		}
		return actual, nil
	}
	if s.ExtraDataSize != 0 || a.Channels > 2 {
		return core.AudioFormat{}, ErrInvalid
	}
	return a, a.Validate()
}

// probeExtraData восстанавливает байты из ограниченного дампа ffprobe. Проверяет непрерывность
// смещений, размер каждой строки и полный объявленный размер; ASCII-колонку не интерпретирует.
// Объём входа уже ограничен MaxJSONBytes, а декодированные данные не могут быть длиннее дампа.
func probeExtraData(dump string, size uint32) ([]byte, error) {
	// data накапливает исходные байты без преобразования структуры ASC/PCE.
	var data []byte
	// line содержит одну строку дампа: смещение, шестнадцатеричные байты и ASCII-представление.
	for line := range strings.SplitSeq(dump, "\n") {
		if strings.TrimSpace(line) == "" {
			continue
		}
		// address содержит смещение строки; columns — обе колонки после двоеточия;
		// found подтверждает наличие разделителя смещения.
		address, columns, found := strings.Cut(line, ":")
		if !found {
			return nil, ErrInvalid
		}
		// offset задаёт прочитанное смещение; err сообщает о неверной шестнадцатеричной записи.
		offset, err := strconv.ParseUint(address, 16, 64)
		if err != nil || offset != uint64(len(data)) {
			return nil, ErrInvalid
		}
		// encoded содержит только шестнадцатеричную колонку без начального пробела и ASCII-хвоста.
		encoded, _, found := strings.Cut(strings.TrimLeft(columns, " "), "  ")
		if !found {
			return nil, ErrInvalid
		}
		// block содержит не более 16 исходных байтов одной строки ffprobe.
		block, err := hex.DecodeString(strings.Join(strings.Fields(encoded), ""))
		if err != nil || len(block) == 0 || len(block) > 16 {
			return nil, ErrInvalid
		}
		data = append(data, block...)
	}
	if len(data) == 0 || uint64(len(data)) != uint64(size) {
		return nil, ErrInvalid
	}
	return data, nil
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

// observedFormat измеряет обе дорожки файла f; ctx ограничивает ffprobe. Заголовки
// кодека обязательны: отсутствующий AV1 tier не становится main по предположению адаптера.
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
			if actual.Video.Codec != "" || s.Codec != "vp9" && (s.ExtraData == "" || s.ExtraDataSize == 0) {
				return actual, ErrInvalid
			}
			actual.Video, err = s.video()
			if doc.Format.Name == "mov,mp4,m4a,3gp,3g2,mj2" && (s.Codec == "hevc" || s.Codec == "vp9" || s.Transfer == "smpte2084" || s.Transfer == "arib-std-b67") {
				actual.Video, _, err = p.sourceVideoFormat(ctx, f, s)
			}
			{
				// transfer 4/5 получает канонические имена gamma22/28 без смены числового CICP.
				switch actual.Video.Color.Transfer {
				case "bt470m":
					actual.Video.Color.Transfer = "gamma22"
				case "bt470bg":
					actual.Video.Color.Transfer = "gamma28"
				}
			}
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
// ctx ограничивает проверку f; profile задаёт ожидаемый полный формат и ограничения кодирования.
func (p Prober) Verify(ctx context.Context, f *os.File, profile core.GroupProfile) (core.MediaFormat, error) {
	// actual содержит полный наблюдаемый формат файла. err сообщает об отказе его определения.
	actual, err := p.observedFormat(ctx, f)
	if err != nil {
		return actual, err
	}
	if core.CompatibleProfileFormat(actual, profile) != nil {
		return actual, fmt.Errorf("%w: фактический формат отличается от профиля", core.ErrNotReady)
	}
	if _, err = f.Seek(0, io.SeekStart); err != nil {
		return actual, err
	}
	// d читает подготовленный fMP4 со сверкой конфигураций контейнера и фактического формата.
	// desc содержит фактические заголовки и описание дорожки fMP4 для проверки подготовленного файла.
	d, err := fmp4.NewDemuxer(contextReader{ctx, f}, fmp4.DemuxOptions{Limits: p.Limits.Container, SourceEpoch: 1, VideoRevision: 1, AudioRevision: 1, NormalizeHeaders: preparedHeaders, ResolveConfig: func(desc fmp4.TrackDescription) (media.CodecConfig, error) {
		// Сверяет кодек, размеры и заголовки дорожки fMP4 с фактическим форматом ffprobe; конфигурацию H.264 и
		// аудиоформат получает из заголовков, а расхождения возвращает как ErrCorrupt.

		// info накапливает проверенное описание дорожки по заголовкам контейнера и наблюдаемому
		// формату.
		info := media.CodecConfigInfo{Track: desc.Track, SourceEpoch: 1, Revision: 1, TimeBase: desc.TimeBase}
		if desc.Track == media.TrackVideo {
			if !containerVideoMatches(desc, actual.Video) {
				return media.CodecConfig{}, core.ErrCorrupt
			}
			info.HDR = desc.HDR
			switch desc.Codec {
			case core.CodecHEVC, core.CodecVP9:
				// actual получен отдельным ffprobe; Inspector сверяет каждый реальный header/AU.
				info.Video = actual.Video
			case core.CodecAV1:
				_, err = codec.ParseAV1Config(desc.Headers)
				info.Video = actual.Video
			default:
				info.Video, err = codec.H264Format(codec.VideoDescription{Headers: desc.Headers, Width: desc.Width, Height: desc.Height})
				if err == nil && codec.CompatibleActual(core.MediaFormat{Video: info.Video, Audio: actual.Audio}, actual) != nil {
					err = core.ErrCorrupt
				}
			}
		} else {
			info.Audio, err = codec.AudioFormatHeaders(desc.Codec, desc.Headers)
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
	if err = encoding.ValidateProfileVideoHeaders(profile, d.Init().Video); err != nil {
		return actual, err
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
	// verifiedHDR фиксирует фактические HDR-сведения первого видео.
	var verifiedHDR media.HDRMetadata
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
		var key bool
		if track == media.TrackVideo {
			// metadata содержит только in-band сведения, отдельно от стандартных boxes конфигурации.
			var metadata media.HDRMetadata
			key, metadata, e = inspector.InspectMetadata(packet)
			if e == nil {
				metadata, e = combineSourceHDR(metadata, c.Info().HDR)
			}
			if e == nil && counts[track] == 0 {
				verifiedHDR = metadata
			} else if e == nil && metadata != verifiedHDR {
				e = core.ErrNotReady
			}
		} else {
			key, e = inspector.Inspect(packet)
		}
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
		} else if !exactPreparedAudioDuration(info.DurationTicks, c.Info().TimeBase, actual.Audio) {
			return actual, fmt.Errorf("%w: audio duration %d в %v", core.ErrCorrupt, info.DurationTicks, c.Info().TimeBase)
		}
		if track == media.TrackAudio {
			if e = verifyPreparedAudio(packet, counts[track]); e != nil {
				return actual, e
			}
		}
		counts[track]++
		last[track] = info.DTS
	}
	if counts[media.TrackVideo] == 0 || counts[media.TrackAudio] == 0 {
		return actual, core.ErrNotReady
	}
	return actual, nil
}

// containerVideoMatches сверяет codec и geometry desc с measured, независимо измеренными из файла.
// Для HEVC/VP9 sample-entry dimensions задают верхнюю границу; coded geometry подтверждает Inspector.
// Прежние AV1/H.264 сохраняют точное сравнение размеров собственного контейнерного пути.
func containerVideoMatches(desc fmp4.TrackDescription, measured core.VideoFormat) bool {
	if desc.Codec != measured.Codec {
		return false
	}
	if desc.Codec == core.CodecHEVC || desc.Codec == core.CodecVP9 {
		return desc.Width >= measured.Width && desc.Height >= measured.Height
	}
	return desc.Width == measured.Width && desc.Height == measured.Height
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
