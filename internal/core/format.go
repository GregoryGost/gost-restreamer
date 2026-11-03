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
	CodecAV1  Codec = "av1"
	CodecH264 Codec = "h264"
	CodecAAC  Codec = "aac"
)

// FrameRate задаёт число кадров в секунду положительной дробью.
type FrameRate struct {
	Numerator   uint32
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
	Range     string
	Primaries string
	Transfer  string
	Matrix    string
}

// Validate проверяет наличие каждой характеристики цвета.
func (c ColorInfo) Validate() error {
	for _, field := range []struct {
		name  string
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
	Codec        Codec
	Width        uint32
	Height       uint32
	FrameRate    FrameRate
	CodecProfile string
	Level        string
	Tier         string
	PixelFormat  string
	BitDepth     uint8
	Color        ColorInfo
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
	if err := f.Color.Validate(); err != nil {
		return fmt.Errorf("VideoFormat.Color: %w", err)
	}
	return nil
}

// AudioFormat содержит структурное описание аудиодорожки.
type AudioFormat struct {
	Codec         Codec
	CodecProfile  string
	SampleRateHz  uint32
	Channels      uint16
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
	Video VideoFormat
	Audio AudioFormat
}

// Validate проверяет обе обязательные дорожки.
func (f MediaFormat) Validate() error {
	if err := f.Video.Validate(); err != nil {
		return fmt.Errorf("MediaFormat.Video: %w", err)
	}
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
