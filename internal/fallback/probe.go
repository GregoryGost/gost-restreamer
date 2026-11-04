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

type ProbeLimits struct {
	MaxJSONBytes               uint64
	MaxWidth, MaxHeight        uint32
	MaxPixels                  uint64
	MaxDuration, ImageDuration time.Duration
	MaxPackets                 uint64
	Container                  fmp4.Limits
}

func (l ProbeLimits) Validate() error {
	if l.MaxJSONBytes == 0 || l.MaxJSONBytes > math.MaxInt32 || l.MaxWidth == 0 || l.MaxHeight == 0 || l.MaxPixels == 0 || l.MaxDuration <= 0 || l.ImageDuration <= 0 || l.ImageDuration > l.MaxDuration || l.MaxPackets == 0 {
		return ErrInvalid
	}
	return l.Container.Validate()
}

type Prober struct {
	Binary string
	Runner ProcessRunner
	Limits ProbeLimits
}
type probeStream struct {
	Type       string `json:"codec_type"`
	Codec      string `json:"codec_name"`
	Profile    string `json:"profile"`
	Width      uint32 `json:"width"`
	Height     uint32 `json:"height"`
	Rate       string `json:"avg_frame_rate"`
	Pixel      string `json:"pix_fmt"`
	Level      int    `json:"level"`
	ColorRange string `json:"color_range"`
	Primaries  string `json:"color_primaries"`
	Transfer   string `json:"color_transfer"`
	Matrix     string `json:"color_space"`
	SampleRate string `json:"sample_rate"`
	Channels   uint16 `json:"channels"`
	Layout     string `json:"channel_layout"`
	StartTime  string `json:"start_time"`
}
type probeDocument struct {
	Streams []probeStream `json:"streams"`
	Format  struct {
		Name     string `json:"format_name"`
		Duration string `json:"duration"`
	} `json:"format"`
}

func (p Prober) inspect(ctx context.Context, f *os.File) (probeDocument, error) {
	if p.Binary == "" || p.Runner == nil || p.Limits.Validate() != nil {
		return probeDocument{}, ErrInvalid
	}
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return probeDocument{}, err
	}
	var out bytes.Buffer
	cmd := Command{Binary: p.Binary, Args: []string{"-v", "error", "-protocol_whitelist", "fd,pipe", "-format_whitelist", "mov,matroska,png_pipe,jpeg_pipe", "-fd", "3", "-i", "fd:", "-show_streams", "-show_format", "-of", "json"}}
	_, err := p.Runner.Run(ctx, cmd, f, &boundedWriter{w: &out, remaining: p.Limits.MaxJSONBytes})
	if err != nil {
		return probeDocument{}, err
	}
	var doc probeDocument
	if err = json.Unmarshal(out.Bytes(), &doc); err != nil {
		return doc, err
	}
	if len(doc.Streams) == 0 || len(doc.Streams) > 2 {
		return doc, ErrInvalid
	}
	return doc, nil
}
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
		if _, err := f.Seek(0, io.SeekStart); err != nil {
			return Source{}, err
		}
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
	doc, err := p.inspect(ctx, f)
	if err != nil {
		return Source{}, err
	}
	s := Source{Kind: kind}
	switch doc.Format.Name {
	case "mov,mp4,m4a,3gp,3g2,mj2":
		s.Demuxer = "mov"
	case "matroska,webm":
		s.Demuxer = "matroska"
	default:
		return s, ErrInvalid
	}
	duration, err := strconv.ParseFloat(doc.Format.Duration, 64)
	if err != nil || math.IsNaN(duration) || math.IsInf(duration, 0) || duration <= 0 || duration > p.Limits.MaxDuration.Seconds() {
		return s, ErrLimit
	}
	s.Duration = time.Duration(duration * float64(time.Second))
	for _, t := range doc.Streams {
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
func (s probeStream) video() (core.VideoFormat, error) {
	rate, err := parseRate(s.Rate)
	if err != nil {
		return core.VideoFormat{}, err
	}
	depth := uint8(8)
	if s.Pixel != "yuv420p" {
		return core.VideoFormat{}, ErrInvalid
	}
	profile := strings.ToLower(s.Profile)
	if profile == "constrained baseline" {
		profile = "constrained-baseline"
	}
	level := ""
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
	rangeName := s.ColorRange
	if rangeName == "tv" {
		rangeName = "limited"
	}
	if rangeName == "pc" {
		rangeName = "full"
	}
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
func (s probeStream) audio() (core.AudioFormat, error) {
	rate, err := strconv.ParseUint(s.SampleRate, 10, 32)
	if err != nil {
		return core.AudioFormat{}, ErrInvalid
	}
	a := core.AudioFormat{Codec: core.Codec(s.Codec), CodecProfile: s.Profile, SampleRateHz: uint32(rate), Channels: s.Channels, ChannelLayout: s.Layout}
	return a, a.Validate()
}
func parseRate(s string) (core.FrameRate, error) {
	v := strings.Split(s, "/")
	if len(v) != 2 {
		return core.FrameRate{}, ErrInvalid
	}
	n, e := strconv.ParseUint(v[0], 10, 32)
	if e != nil {
		return core.FrameRate{}, ErrInvalid
	}
	d, e := strconv.ParseUint(v[1], 10, 32)
	if e != nil {
		return core.FrameRate{}, ErrInvalid
	}
	r := core.FrameRate{Numerator: uint32(n), Denominator: uint32(d)}
	return r, r.Validate()
}

func (p Prober) observedFormat(ctx context.Context, f *os.File) (core.MediaFormat, error) {
	doc, err := p.inspect(ctx, f)
	if err != nil {
		return core.MediaFormat{}, err
	}
	var actual core.MediaFormat
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
	duration, err := strconv.ParseFloat(doc.Format.Duration, 64)
	if err != nil || math.IsNaN(duration) || math.IsInf(duration, 0) || duration <= 0 || duration > p.Limits.MaxDuration.Seconds() {
		return actual, ErrLimit
	}
	return actual, nil
}

// Verify проверяет фактические заголовки и единицы доступа, частоту кадров,
// структуру GOP и продвижение обеих дорожек всего файла.
func (p Prober) Verify(ctx context.Context, f *os.File, profile core.GroupProfile) (core.MediaFormat, error) {
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
	d, err := fmp4.NewDemuxer(contextReader{ctx, f}, fmp4.DemuxOptions{Limits: p.Limits.Container, SourceEpoch: 1, VideoRevision: 1, AudioRevision: 1, ResolveConfig: func(desc fmp4.TrackDescription) (media.CodecConfig, error) {
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
	counts := map[media.Track]uint64{}
	last := map[media.Track]int64{}
	lastKey := uint64(0)
	var inspector codec.Inspector
	for total := uint64(0); ; total++ {
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
		info, c := packet.Info(), packet.Config()
		track := c.Info().Track
		if info.DurationTicks <= 0 || counts[track] > 0 && info.DTS <= last[track] {
			return actual, core.ErrCorrupt
		}
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
func exactDuration(ticks int64, b media.TimeBase, fps core.FrameRate) bool {
	l := new(big.Int).Mul(big.NewInt(ticks), new(big.Int).SetUint64(uint64(b.Numerator)*uint64(fps.Numerator)))
	r := new(big.Int).SetUint64(uint64(b.Denominator) * uint64(fps.Denominator))
	return l.Cmp(r) == 0
}
func exactAudioDuration(ticks int64, b media.TimeBase, rate uint32) bool {
	l := new(big.Int).Mul(big.NewInt(ticks), new(big.Int).SetUint64(uint64(b.Numerator)*uint64(rate)))
	r := new(big.Int).SetUint64(uint64(b.Denominator) * 1024)
	return l.Cmp(r) == 0
}
