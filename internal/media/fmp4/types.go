// Package fmp4 реализует ограниченный последовательный транспорт сжатых A/V-пакетов.
// Владелец Reader/Writer отвечает за отмену, дедлайны и закрытие соединения.
// Экземпляры mux/demux предназначены для одного последовательного вызывающего.
package fmp4

import (
	"errors"
	"fmt"
	"math"
	"math/big"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
)

var (
	ErrInvalid             = errors.New("некорректный fMP4")
	ErrLimit               = errors.New("превышен бюджет fMP4")
	ErrUnsupportedTiming   = errors.New("время не представимо на выбранном порте")
	ErrConfigChanged       = errors.New("конфигурация fMP4 требует нового транспорта")
	ErrMetadataUnavailable = errors.New("метаданных fMP4 недостаточно для полной конфигурации")
)

// Limits ограничивает размеры блоков и число описаний samples.
// Бюджеты box/init/moof/mdat включают заголовки; sample/extradata — только данные.
type Limits struct {
	MaxBoxBytes, MaxInitBytes, MaxMoofBytes, MaxMdatBytes uint64
	MaxSampleBytes, MaxExtradataBytes                     uint64
	MaxSamples                                            uint32
}

func (l Limits) Validate() error {
	for _, n := range []uint64{l.MaxBoxBytes, l.MaxInitBytes, l.MaxMoofBytes, l.MaxMdatBytes, l.MaxSampleBytes, l.MaxExtradataBytes, uint64(l.MaxSamples)} {
		if n == 0 || n > uint64(math.MaxInt32) {
			return fmt.Errorf("%w: бюджеты должны быть положительными и не превышать MaxInt32", ErrLimit)
		}
	}
	if l.MaxSampleBytes > l.MaxMdatBytes || l.MaxExtradataBytes > l.MaxInitBytes || l.MaxMoofBytes > l.MaxBoxBytes {
		return fmt.Errorf("%w: несогласованные бюджеты", ErrLimit)
	}
	return nil
}

// OriginMapping добавляет один и тот же рациональный сдвиг обеим дорожкам.
// Edit list снимает его при чтении. Нулевой OffsetTicks означает тождество.
type OriginMapping struct {
	OffsetTicks int64
	TimeBase    media.TimeBase
}

func (o OriginMapping) ticks(scale uint32) (int64, error) {
	if o.OffsetTicks == 0 {
		return 0, nil
	}
	if o.OffsetTicks < 0 {
		return 0, fmt.Errorf("%w: отрицательный сдвиг origin", ErrUnsupportedTiming)
	}
	return rescale(o.OffsetTicks, o.TimeBase, media.TimeBase{Numerator: 1, Denominator: scale})
}

// Init фиксирует пару конфигураций на весь срок одного транспорта.
// Headers содержат avcC, av1C или AudioSpecificConfig без заголовка MP4 box.
type Init struct {
	Video, Audio media.CodecConfig
	Origin       OriginMapping
}

func (i Init) Validate() error {
	if err := i.Video.Validate(); err != nil {
		return err
	}
	if err := i.Audio.Validate(); err != nil {
		return err
	}
	v, a := i.Video.Info(), i.Audio.Info()
	if v.Track != media.TrackVideo || a.Track != media.TrackAudio || v.SourceEpoch != a.SourceEpoch {
		return fmt.Errorf("%w: несогласованная пара A/V", ErrInvalid)
	}
	_, err := MovieTimeScale(v.TimeBase, a.TimeBase)
	return err
}

// MovieTimeScale возвращает точную общую шкалу без округления.
func MovieTimeScale(bases ...media.TimeBase) (uint32, error) {
	result := uint64(1)
	for _, b := range bases {
		if err := b.Validate(); err != nil {
			return 0, err
		}
		d := uint64(b.Denominator) / gcd(uint64(b.Numerator), uint64(b.Denominator))
		result = result / gcd(result, d) * d
		if result > math.MaxUint32 {
			return 0, fmt.Errorf("%w: общий timescale", ErrUnsupportedTiming)
		}
	}
	return uint32(result), nil
}
func gcd(a, b uint64) uint64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}
func trackScale(b media.TimeBase) (uint32, error) { return MovieTimeScale(b) }
func rescale(v int64, from, to media.TimeBase) (int64, error) {
	if from.Validate() != nil || to.Validate() != nil {
		return 0, ErrUnsupportedTiming
	}
	n := big.NewInt(v)
	n.Mul(n, new(big.Int).SetUint64(uint64(from.Numerator)*uint64(to.Denominator)))
	d := new(big.Int).SetUint64(uint64(from.Denominator) * uint64(to.Numerator))
	q, r := new(big.Int), new(big.Int)
	q.QuoRem(n, d, r)
	if r.Sign() != 0 || !q.IsInt64() {
		return 0, fmt.Errorf("%w: нет точного рационального отображения", ErrUnsupportedTiming)
	}
	return q.Int64(), nil
}
func add(a, b int64) (int64, error) {
	if b > 0 && a > math.MaxInt64-b || b < 0 && a < math.MinInt64-b {
		return 0, ErrUnsupportedTiming
	}
	return a + b, nil
}
func newDifference(a, b int64) *big.Int { return new(big.Int).Sub(big.NewInt(a), big.NewInt(b)) }

// TrackDescription содержит только сведения, полученные из MP4 headers.
type TrackDescription struct {
	ID            uint32
	Track         media.Track
	Codec         core.Codec
	TimeBase      media.TimeBase
	Width, Height uint32
	SampleRateHz  uint32
	Channels      uint16
	Headers       []byte
}

// DemuxOptions задаёт новую идентичность и восстановление полного описания.
// Нулевой ResolveConfig использует ResolveCodecConfig для H.264/AAC.
// Пользовательский ResolveConfig получает фактические Headers.
type DemuxOptions struct {
	Limits                                    Limits
	SourceEpoch, VideoRevision, AudioRevision uint64
	ResolveConfig                             func(TrackDescription) (media.CodecConfig, error)
}

// FFmpegTimingValidator проверяет ограничения CLI до сериализации пакета.
type FFmpegTimingValidator struct {
	last    map[media.Track]int64
	configs map[media.Track]media.CodecConfig
	origin  OriginMapping
	mapped  bool
}

func NewFFmpegTimingValidator() *FFmpegTimingValidator {
	return &FFmpegTimingValidator{last: make(map[media.Track]int64), configs: make(map[media.Track]media.CodecConfig)}
}

// NewFFmpegTimingValidatorForOrigin дополнительно проверяет диапазон после сдвига mux.
func NewFFmpegTimingValidatorForOrigin(origin OriginMapping) *FFmpegTimingValidator {
	v := NewFFmpegTimingValidator()
	v.origin = origin
	v.mapped = true
	return v
}
func (v *FFmpegTimingValidator) Validate(p media.Packet) error {
	if err := p.Validate(); err != nil {
		return err
	}
	info := p.Info()
	c := p.Config()
	track := c.Info().Track
	if info.PTS < info.DTS {
		return fmt.Errorf("%w: PTS < DTS", ErrUnsupportedTiming)
	}
	scale, err := trackScale(c.Info().TimeBase)
	if err != nil {
		return err
	}
	base := media.TimeBase{Numerator: 1, Denominator: scale}
	pts, err := rescale(info.PTS, c.Info().TimeBase, base)
	if err != nil {
		return err
	}
	dts, err := rescale(info.DTS, c.Info().TimeBase, base)
	if err != nil {
		return err
	}
	duration, err := rescale(info.DurationTicks, c.Info().TimeBase, base)
	if err != nil {
		return err
	}
	cts := newDifference(pts, dts)
	if !cts.IsInt64() || cts.Int64() > math.MaxInt32 || duration > math.MaxUint32 {
		return ErrUnsupportedTiming
	}
	if track == media.TrackVideo && c.Info().Video.Codec == core.CodecAV1 && pts != dts {
		return ErrUnsupportedTiming
	}
	if v.mapped {
		shift, err := v.origin.ticks(scale)
		if err != nil {
			return err
		}
		storedDTS, err := add(dts, shift)
		if err != nil || storedDTS < 0 {
			return ErrUnsupportedTiming
		}
		if _, err = add(pts, shift); err != nil {
			return err
		}
	}
	if v.configs == nil {
		v.configs = make(map[media.Track]media.CodecConfig)
		v.last = make(map[media.Track]int64)
	}
	if previous, ok := v.configs[track]; ok && !sameConfig(previous, c) {
		return ErrConfigChanged
	}
	if previous, ok := v.last[track]; ok && info.DTS <= previous {
		return fmt.Errorf("%w: DTS не продвигается", ErrUnsupportedTiming)
	}
	v.last[track] = info.DTS
	v.configs[track] = c
	return nil
}
