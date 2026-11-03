package media

import (
	"bytes"
	"errors"
	"fmt"
)

// ErrInvalidPacket обозначает некорректный сжатый пакет.
var ErrInvalidPacket = errors.New("некорректный сжатый пакет")

// PacketInfo содержит временные метки одной единицы доступа в единицах конфигурации.
type PacketInfo struct {
	PTS           int64
	DTS           int64
	DurationTicks int64
	Keyframe      bool
}

// Packet хранит неизменяемую единицу доступа со снимком конфигурации дорожки.
type Packet struct {
	info    PacketInfo
	config  CodecConfig
	payload []byte
}

// NewPacket проверяет пакет и сохраняет собственную копию полезной нагрузки.
func NewPacket(info PacketInfo, config CodecConfig, payload []byte) (Packet, error) {
	packet := Packet{info: info, config: config, payload: payload}
	if err := packet.Validate(); err != nil {
		return Packet{}, err
	}
	packet.payload = bytes.Clone(payload)
	return packet, nil
}

// Info возвращает временные метки и признак независимого старта по значению.
func (p Packet) Info() PacketInfo {
	return p.info
}

// Config возвращает неизменяемый снимок конфигурации по значению.
func (p Packet) Config() CodecConfig {
	return p.config
}

// Payload возвращает отдельную копию полезной нагрузки.
func (p Packet) Payload() []byte {
	return bytes.Clone(p.payload)
}

// StoredBytes возвращает суммарный логический вес полезной нагрузки и заголовков.
func (p Packet) StoredBytes() uint64 {
	return uint64(len(p.payload)) + p.config.StoredBytes()
}

// Validate проверяет структуру пакета без проверки последовательности и декодируемости.
func (p Packet) Validate() error {
	if err := p.config.Validate(); err != nil {
		return fmt.Errorf("%w: Packet.Config: %w", ErrInvalidPacket, err)
	}
	if p.info.DurationTicks < 0 {
		return fmt.Errorf("%w: Packet.DurationTicks не должен быть отрицательным", ErrInvalidPacket)
	}
	if p.config.info.Track == TrackAudio && p.info.Keyframe {
		return fmt.Errorf("%w: Packet.Keyframe недопустим для audio", ErrInvalidPacket)
	}
	if len(p.payload) == 0 {
		return fmt.Errorf("%w: Packet.Payload должен быть непустым", ErrInvalidPacket)
	}
	return nil
}
