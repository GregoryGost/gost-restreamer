package core

import (
	"errors"
	"fmt"
)

// ErrInvalidProfile обозначает некорректные настройки кодирования или группы.
var ErrInvalidProfile = errors.New("некорректный профиль группы")

// GroupMode определяет способ обработки видеопотока группы.
type GroupMode string

const (
	ModeCopy      GroupMode = "copy"
	ModeTranscode GroupMode = "transcode"
)

// VideoEncoding задаёт параметры подготовки видео и структуру GOP.
type VideoEncoding struct {
	TargetBitrateBPS uint64
	MaxBitrateBPS    uint64
	BufferBits       uint64
	GOPFrames        uint32
	MaxBFrames       uint32
	ClosedGOP        bool
}

// Validate проверяет положительные ограничения и соотношение битрейтов.
func (e VideoEncoding) Validate() error {
	if e.TargetBitrateBPS == 0 {
		return fmt.Errorf("%w: VideoEncoding.TargetBitrateBPS должен быть положительным", ErrInvalidProfile)
	}
	if e.MaxBitrateBPS == 0 {
		return fmt.Errorf("%w: VideoEncoding.MaxBitrateBPS должен быть положительным", ErrInvalidProfile)
	}
	if e.TargetBitrateBPS > e.MaxBitrateBPS {
		return fmt.Errorf("%w: VideoEncoding.TargetBitrateBPS превышает MaxBitrateBPS", ErrInvalidProfile)
	}
	if e.BufferBits == 0 {
		return fmt.Errorf("%w: VideoEncoding.BufferBits должен быть положительным", ErrInvalidProfile)
	}
	if e.GOPFrames == 0 {
		return fmt.Errorf("%w: VideoEncoding.GOPFrames должен быть положительным", ErrInvalidProfile)
	}
	return nil
}

// AudioEncoding задаёт битрейт подготовки аудио.
type AudioEncoding struct {
	BitrateBPS uint64
}

// Validate проверяет положительность битрейта аудио.
func (e AudioEncoding) Validate() error {
	if e.BitrateBPS == 0 {
		return fmt.Errorf("%w: AudioEncoding.BitrateBPS должен быть положительным", ErrInvalidProfile)
	}
	return nil
}

// GroupProfile содержит полный явный выходной профиль группы.
type GroupProfile struct {
	Mode          GroupMode
	Format        MediaFormat
	VideoEncoding VideoEncoding
	AudioEncoding AudioEncoding
	Encoding      EncodingSettings
}

// Validate проверяет режим, обе дорожки и настройки их подготовки.
func (p GroupProfile) Validate() error {
	if p.Encoding != (EncodingSettings{}) {
		return p.validateEncodingProfile()
	}
	return p.validateLegacyProfile()
}

func (p GroupProfile) validateLegacyProfile() error {
	if p.Mode != ModeCopy && p.Mode != ModeTranscode {
		return fmt.Errorf("%w: GroupProfile.Mode должен быть copy или transcode", ErrInvalidProfile)
	}
	if err := p.Format.Validate(); err != nil {
		return fmt.Errorf("%w: GroupProfile.Format: %w", ErrInvalidProfile, err)
	}
	if err := p.VideoEncoding.Validate(); err != nil {
		return fmt.Errorf("GroupProfile.VideoEncoding: %w", err)
	}
	if err := p.AudioEncoding.Validate(); err != nil {
		return fmt.Errorf("GroupProfile.AudioEncoding: %w", err)
	}
	return nil
}

// Equal сравнивает все параметры корректных профилей, а не пригодность источников.
func (p GroupProfile) Equal(other GroupProfile) bool {
	if p.Validate() != nil || other.Validate() != nil {
		return false
	}
	return p.Mode == other.Mode && p.Format.Equal(other.Format) &&
		p.VideoEncoding == other.VideoEncoding && p.AudioEncoding == other.AudioEncoding && p.Encoding == other.Encoding
}
