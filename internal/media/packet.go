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
	// PTS задаёт знаковое время показа единицы доступа в отсчётах TimeBase конфигурации.
	PTS int64
	// DTS задаёт знаковое время декодирования единицы доступа в отсчётах TimeBase конфигурации.
	DTS int64
	// DurationTicks сохраняет переданную длительность в отсчётах TimeBase конфигурации; ноль
	// разрешён, отрицательное значение отклоняется.
	DurationTicks int64
	// Keyframe сохраняет заявленный признак независимого видеокадра без проверки битового потока;
	// для аудио должен быть false.
	Keyframe bool
}

// Packet хранит неизменяемую единицу доступа со снимком конфигурации дорожки.
type Packet struct {
	// info хранит знаковые временные метки, неотрицательную длительность и заявленный признак
	// независимого видеокадра.
	info PacketInfo
	// config сохраняет конфигурацию дорожки, которой принадлежат сжатые данные.
	config CodecConfig
	// payload хранит собственную копию сжатой единицы доступа.
	payload []byte
}

// NewPacket проверяет пакет и сохраняет собственную копию полезной нагрузки.
func NewPacket(info PacketInfo, config CodecConfig, payload []byte) (Packet, error) {
	// packet объединяет входные данные для проверки перед копированием полезной нагрузки.
	packet := Packet{info: info, config: config, payload: payload}
	// err сообщает о некорректной конфигурации, длительности, признаке кадра или пустой нагрузке.
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
	// err сообщает о некорректной конфигурации дорожки пакета.
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
