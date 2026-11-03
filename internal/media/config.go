package media

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// ErrInvalidConfig обозначает некорректную конфигурацию дорожки.
var ErrInvalidConfig = errors.New("некорректная конфигурация дорожки")

// Track определяет тип сжатой дорожки.
type Track string

const (
	TrackVideo Track = "video"
	TrackAudio Track = "audio"
)

// CodecConfigInfo связывает наблюдаемый формат с поколением и единицей времени.
type CodecConfigInfo struct {
	Track       Track
	SourceEpoch uint64
	Revision    uint64
	TimeBase    TimeBase
	Video       core.VideoFormat
	Audio       core.AudioFormat
}

// CodecConfig хранит неизменяемый снимок описания дорожки и её заголовков.
type CodecConfig struct {
	info    CodecConfigInfo
	headers []byte
}

// NewCodecConfig проверяет конфигурацию и сохраняет собственную копию заголовков.
func NewCodecConfig(info CodecConfigInfo, headers []byte) (CodecConfig, error) {
	config := CodecConfig{info: info, headers: headers}
	if err := config.Validate(); err != nil {
		return CodecConfig{}, err
	}
	config.headers = bytes.Clone(headers)
	return config, nil
}

// Info возвращает описание конфигурации по значению.
func (c CodecConfig) Info() CodecConfigInfo {
	return c.info
}

// Headers возвращает отдельную копию заголовков.
func (c CodecConfig) Headers() []byte {
	return bytes.Clone(c.headers)
}

// StoredBytes возвращает логический вес сжатых заголовков в байтах.
func (c CodecConfig) StoredBytes() uint64 {
	return uint64(len(c.headers))
}

// Validate проверяет описание и наличие заголовков без разбора битового потока.
func (c CodecConfig) Validate() error {
	if c.info.SourceEpoch == 0 {
		return fmt.Errorf("%w: CodecConfig.SourceEpoch должен быть положительным", ErrInvalidConfig)
	}
	if c.info.Revision == 0 {
		return fmt.Errorf("%w: CodecConfig.Revision должен быть положительным", ErrInvalidConfig)
	}
	if err := c.info.TimeBase.Validate(); err != nil {
		return fmt.Errorf("%w: CodecConfig.TimeBase: %w", ErrInvalidConfig, err)
	}
	switch c.info.Track {
	case TrackVideo:
		if c.info.Audio != (core.AudioFormat{}) {
			return fmt.Errorf("%w: CodecConfig.Audio должен быть нулевым для video", ErrInvalidConfig)
		}
		if err := c.info.Video.Validate(); err != nil {
			return fmt.Errorf("%w: CodecConfig.Video: %w", ErrInvalidConfig, err)
		}
	case TrackAudio:
		if c.info.Video != (core.VideoFormat{}) {
			return fmt.Errorf("%w: CodecConfig.Video должен быть нулевым для audio", ErrInvalidConfig)
		}
		if err := c.info.Audio.Validate(); err != nil {
			return fmt.Errorf("%w: CodecConfig.Audio: %w", ErrInvalidConfig, err)
		}
	default:
		return fmt.Errorf("%w: CodecConfig.Track должен быть video или audio", ErrInvalidConfig)
	}
	if len(c.headers) == 0 {
		return fmt.Errorf("%w: CodecConfig.Headers должны быть непустыми", ErrInvalidConfig)
	}
	return nil
}
