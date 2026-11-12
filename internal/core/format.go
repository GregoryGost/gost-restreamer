// Package core определяет доменные значения и их инварианты.
package core

import (
	"errors"
	"fmt"
	"strings"
)

// ErrInvalidFormat обозначает некорректное описание формата.
var ErrInvalidFormat = errors.New("некорректное описание формата")

// Codec обозначает кодек сжатой дорожки.
type Codec string

const (
	// CodecAV1 обозначает кодек AV1 для видеодорожки.
	CodecAV1 Codec = "av1"
	// CodecH264 обозначает кодек H.264 для видеодорожки.
	CodecH264 Codec = "h264"
	// CodecAAC обозначает кодек AAC для аудиодорожки.
	CodecAAC Codec = "aac"
)

// FrameRate задаёт число кадров в секунду положительной дробью.
type FrameRate struct {
	// Numerator содержит числитель частоты кадров.
	Numerator uint32
	// Denominator содержит положительный знаменатель частоты кадров.
	Denominator uint32
}

// Validate проверяет положительность обеих частей дроби.
func (r FrameRate) Validate() error {
	if r.Numerator == 0 {
		return fmt.Errorf("%w: FrameRate.Numerator должен быть положительным", ErrInvalidFormat)
	}
	if r.Denominator == 0 {
		return fmt.Errorf("%w: FrameRate.Denominator должен быть положительным", ErrInvalidFormat)
	}
	return nil
}

// Equal сравнивает корректные частоты как математические дроби.
func (r FrameRate) Equal(other FrameRate) bool {
	if r.Validate() != nil || other.Validate() != nil {
		return false
	}
	// Произведение двух uint32 целиком помещается в uint64.
	return uint64(r.Numerator)*uint64(other.Denominator) == uint64(other.Numerator)*uint64(r.Denominator)
}

// ColorInfo содержит заявленные характеристики цвета.
type ColorInfo struct {
	// Range указывает диапазон значений цвета.
	Range string
	// Primaries указывает основные цвета изображения.
	Primaries string
	// Transfer указывает передаточную характеристику изображения.
	Transfer string
	// Matrix указывает матрицу преобразования цвета.
	Matrix string
}

// Validate проверяет наличие каждой характеристики цвета.
func (c ColorInfo) Validate() error {
	// field содержит очередную характеристику цвета вместе с её именем для диагностики.
	for _, field := range []struct {
		// name указывает имя проверяемой характеристики цвета в диагностике.
		name string
		// value содержит текст проверяемой характеристики цвета.
		value string
	}{
		{"Range", c.Range},
		{"Primaries", c.Primaries},
		{"Transfer", c.Transfer},
		{"Matrix", c.Matrix},
	} {
		if strings.TrimSpace(field.value) == "" {
			return fmt.Errorf("%w: ColorInfo.%s должен быть задан", ErrInvalidFormat, field.name)
		}
	}
	return nil
}

// VideoFormat содержит структурное описание видеодорожки.
type VideoFormat struct {
	// Codec указывает кодек сжатой дорожки.
	Codec Codec
	// Width содержит ширину видеокадра в пикселях.
	Width uint32
	// Height содержит высоту видеокадра в пикселях.
	Height uint32
	// FrameRate задаёт частоту видеокадров точной положительной дробью.
	FrameRate FrameRate
	// CodecProfile содержит профиль выбранного кодека.
	CodecProfile string
	// Level содержит ограничение уровня видеокодека.
	Level string
	// Tier содержит ограничение категории видеокодека.
	Tier string
	// PixelFormat содержит обозначение формата пикселей.
	PixelFormat string
	// BitDepth содержит глубину компонента пикселя в битах.
	BitDepth uint8
	// Color содержит заявленные характеристики цвета.
	Color ColorInfo
}

// Validate проверяет структуру формата без проверки возможностей кодека.
func (f VideoFormat) Validate() error {
	if f.Codec != CodecAV1 && f.Codec != CodecH264 {
		return fmt.Errorf("%w: VideoFormat.Codec должен быть av1 или h264", ErrInvalidFormat)
	}
	if f.Width == 0 {
		return fmt.Errorf("%w: VideoFormat.Width должен быть положительным", ErrInvalidFormat)
	}
	if f.Height == 0 {
		return fmt.Errorf("%w: VideoFormat.Height должен быть положительным", ErrInvalidFormat)
	}
	// err содержит причину отказа проверки частоты видеокадров.
	if err := f.FrameRate.Validate(); err != nil {
		return fmt.Errorf("VideoFormat.FrameRate: %w", err)
	}
	if strings.TrimSpace(f.CodecProfile) == "" {
		return fmt.Errorf("%w: VideoFormat.CodecProfile должен быть задан", ErrInvalidFormat)
	}
	if strings.TrimSpace(f.PixelFormat) == "" {
		return fmt.Errorf("%w: VideoFormat.PixelFormat должен быть задан", ErrInvalidFormat)
	}
	if f.BitDepth == 0 {
		return fmt.Errorf("%w: VideoFormat.BitDepth должен быть положительным", ErrInvalidFormat)
	}
	// err содержит причину отказа проверки характеристик цвета.
	if err := f.Color.Validate(); err != nil {
		return fmt.Errorf("VideoFormat.Color: %w", err)
	}
	return nil
}

// AudioFormat содержит структурное описание аудиодорожки.
type AudioFormat struct {
	// Codec указывает кодек сжатой дорожки.
	Codec Codec
	// CodecProfile содержит профиль выбранного кодека.
	CodecProfile string
	// SampleRateHz содержит частоту дискретизации аудио в герцах.
	SampleRateHz uint32
	// Channels содержит число аудиоканалов.
	Channels uint16
	// ChannelLayout содержит схему расположения аудиоканалов.
	ChannelLayout string
}

// Validate проверяет наличие обязательных параметров аудио AAC.
func (f AudioFormat) Validate() error {
	if f.Codec != CodecAAC {
		return fmt.Errorf("%w: AudioFormat.Codec должен быть aac", ErrInvalidFormat)
	}
	if strings.TrimSpace(f.CodecProfile) == "" {
		return fmt.Errorf("%w: AudioFormat.CodecProfile должен быть задан", ErrInvalidFormat)
	}
	if f.SampleRateHz == 0 {
		return fmt.Errorf("%w: AudioFormat.SampleRateHz должен быть положительным", ErrInvalidFormat)
	}
	if f.Channels == 0 {
		return fmt.Errorf("%w: AudioFormat.Channels должен быть положительным", ErrInvalidFormat)
	}
	if strings.TrimSpace(f.ChannelLayout) == "" {
		return fmt.Errorf("%w: AudioFormat.ChannelLayout должен быть задан", ErrInvalidFormat)
	}
	return nil
}

// MediaFormat описывает обе обязательные дорожки группы.
type MediaFormat struct {
	// Video содержит описание обязательной видеодорожки.
	Video VideoFormat
	// Audio содержит описание обязательной аудиодорожки.
	Audio AudioFormat
}

// Validate проверяет обе обязательные дорожки.
func (f MediaFormat) Validate() error {
	// err содержит причину отказа проверки обязательной видеодорожки.
	if err := f.Video.Validate(); err != nil {
		return fmt.Errorf("MediaFormat.Video: %w", err)
	}
	// err содержит причину отказа проверки обязательной аудиодорожки.
	if err := f.Audio.Validate(); err != nil {
		return fmt.Errorf("MediaFormat.Audio: %w", err)
	}
	return nil
}

// Equal сравнивает все поля корректных форматов с учётом равенства частот.
func (f MediaFormat) Equal(other MediaFormat) bool {
	if f.Validate() != nil || other.Validate() != nil {
		return false
	}
	if !f.Video.FrameRate.Equal(other.Video.FrameRate) {
		return false
	}
	other.Video.FrameRate = f.Video.FrameRate
	return f == other
}

// CompatibleFormat сверяет измеренные параметры с заданными ограничениями профиля.
// Незаданные level/tier не требуют совпадения конкретного значения кодера.
func CompatibleFormat(actual, desired MediaFormat) error {
	if actual.Validate() != nil || desired.Validate() != nil {
		return ErrInvalidFormat
	}
	// a содержит копию фактических параметров видео, а d — копию требуемых параметров для
	// нормализации допустимых различий.
	a, d := actual.Video, desired.Video
	if !a.FrameRate.Equal(d.FrameRate) {
		return ErrInvalidFormat
	}
	a.FrameRate = d.FrameRate
	// normalize приводит псевдонимы tv и pc к сопоставимым обозначениям диапазона цвета.
	normalize := func(c ColorInfo) ColorInfo {
		// Приводит только псевдонимы диапазона tv и pc к limited и full в копии ColorInfo для сравнения
		// измеренного и заданного форматов.

		switch c.Range {
		case "tv":
			c.Range = "limited"
		case "pc":
			c.Range = "full"
		}
		return c
	}
	a.Color = normalize(a.Color)
	d.Color = normalize(d.Color)
	if d.Level == "" || d.Level == "auto" {
		a.Level = d.Level
	}
	if d.Tier == "" {
		a.Tier = ""
	}
	if a != d || actual.Audio != desired.Audio || a.PixelFormat != "yuv420p" || a.BitDepth != 8 || a.Color.Transfer == "smpte2084" || a.Color.Transfer == "arib-std-b67" {
		return ErrInvalidFormat
	}
	return nil
}
