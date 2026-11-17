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
	// ModeCopy задаёт передачу LIVE-видео без перекодирования.
	ModeCopy GroupMode = "copy"
	// ModeTranscode задаёт перекодирование LIVE-видео по выходному профилю.
	ModeTranscode GroupMode = "transcode"
)

// LiveAudioMode задаёт обработку звуковой дорожки LIVE независимо от видеорежима.
type LiveAudioMode string

const (
	// LiveAudioCopy сохраняет аудиодорожку LIVE без перекодирования.
	LiveAudioCopy LiveAudioMode = "copy"
	// LiveAudioTranscode включает перекодирование LIVE-аудио в режиме transcode.
	LiveAudioTranscode LiveAudioMode = "transcode"
)

// VideoEncoding задаёт параметры подготовки видео и структуру GOP.
type VideoEncoding struct {
	// TargetBitrateBPS задаёт целевой видеобитрейт в битах в секунду.
	TargetBitrateBPS uint64
	// MaxBitrateBPS ограничивает видеобитрейт сверху в битах в секунду.
	MaxBitrateBPS uint64
	// BufferBits задаёт размер буфера управления битрейтом в битах.
	BufferBits uint64
	// GOPFrames задаёт длину GOP в кадрах.
	GOPFrames uint32
	// MaxBFrames задаёт максимальное число B-кадров.
	MaxBFrames uint32
	// ClosedGOP указывает, должен ли GOP быть закрытым.
	ClosedGOP bool
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
	// BitrateBPS задаёт целевой аудиобитрейт в битах в секунду; FLAC требует неприменимый ноль.
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
	// Mode выбирает копирование или перекодирование LIVE-видео.
	Mode GroupMode
	// LiveAudioMode явно выбирает обработку LIVE-аудио; пустое значение недопустимо.
	LiveAudioMode LiveAudioMode
	// Format описывает требуемый формат обеих выходных дорожек.
	Format MediaFormat
	// VideoEncoding содержит параметры кодирования видео и GOP.
	VideoEncoding VideoEncoding
	// AudioEncoding содержит параметры кодирования звука.
	AudioEncoding AudioEncoding
	// Encoding содержит полный материализованный запрос конкретных кодировщиков.
	Encoding EncodingSettings
}

// Validate проверяет режим, обе дорожки и настройки их подготовки.
func (p GroupProfile) Validate() error {
	// err содержит отказ общей проверки желаемых строк и их непосредственных зависимостей.
	if err := p.validateDesiredStrings(); err != nil {
		return err
	}
	if p.LiveAudioMode != LiveAudioCopy && p.LiveAudioMode != LiveAudioTranscode {
		return invalidEncoding("live_audio_mode", "Допустимы только copy и transcode.")
	}
	if p.LiveAudioMode == LiveAudioTranscode && p.Mode != ModeTranscode {
		return invalidEncoding("live_audio_mode", "Перекодирование LIVE-аудио доступно только в режиме transcode.")
	}
	return p.validateEncodingProfile()
}

// EffectiveLiveAudioMode возвращает явно сохранённый режим p без назначения значений.
// Использующий профиль код предварительно обязан проверить его через Validate.
func (p GroupProfile) EffectiveLiveAudioMode() LiveAudioMode { return p.LiveAudioMode }

// Equal сравнивает все параметры корректных профилей, а не пригодность источников.
// other задаёт второй полный профиль для сравнения с p; некорректность любого из них даёт false.
func (p GroupProfile) Equal(other GroupProfile) bool {
	if p.Validate() != nil || other.Validate() != nil {
		return false
	}
	return p.Mode == other.Mode && p.LiveAudioMode == other.LiveAudioMode && p.Format.Equal(other.Format) &&
		p.VideoEncoding == other.VideoEncoding && p.AudioEncoding == other.AudioEncoding && p.Encoding == other.Encoding
}
