package codec

import (
	"bytes"
	"fmt"
	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
)

// audioBinding возвращает конечный фактический формат 48 кГц, стерео для аудиокодека kind.
// Вызывающий использует только Opus/FLAC/MP3/AC3; AAC имеет отдельный разборщик ASC.
func audioBinding(kind core.Codec) core.AudioFormat {
	// profile выбирается по подтверждённым параметрам кодека, без желаемых настроек профиля.
	profile := ""
	switch kind {
	case core.CodecOpus:
		profile = "celt"
	case core.CodecFLAC:
		profile = "s16"
	case core.CodecMP3:
		profile = "mpeg1-layer3"
	case core.CodecAC3:
		profile = "ac3"
	}
	return core.AudioFormat{Codec: kind, CodecProfile: profile, SampleRateHz: 48000, Channels: 2, ChannelLayout: "stereo"}
}

// AudioFormatHeaders получает фактический формат кодека kind из полных заголовков headers.
// Для MP3 headers содержит четыре байта первого кадра, AC3 — dac3, FLAC — 34 байта STREAMINFO,
// Opus — 19 байтов OpusHead с младшим байтом первым. AAC сохраняет прежнюю проверку полного ASC.
func AudioFormatHeaders(kind core.Codec, headers []byte) (core.AudioFormat, error) {
	// err сохраняет отказ специализированного разборщика до возврата подтверждённого формата.
	var err error
	switch kind {
	case core.CodecAAC:
		return AudioFormatASC(headers)
	case core.CodecOpus:
		_, err = ParseOpusHead(headers)
	case core.CodecFLAC:
		_, err = ParseFLACStreamInfo(headers)
	case core.CodecMP3:
		if len(headers) != 4 {
			return core.AudioFormat{}, ErrCodec
		}
		_, err = mp3Header(headers)
	case core.CodecAC3:
		err = ac3Config(headers)
	default:
		err = ErrCodec
	}
	if err != nil {
		return core.AudioFormat{}, err
	}
	return audioBinding(kind), nil
}

// CompleteAudioConfig уточняет kind по полным headers и первой проверенной firstAU.
// Пустые сетевые заголовки MP3/AC3 заменяются фактическим дескриптором; непустые сверяются по смыслу.
// Возвращаемые headers независимы от входа. Начальное исключение Opus здесь не потребляется.
func CompleteAudioConfig(kind core.Codec, headers, firstAU []byte) (core.AudioFormat, []byte, error) {
	// completed получает отдельный нормативный дескриптор, err — результат его восстановления.
	completed := bytes.Clone(headers)
	var err error
	if len(completed) == 0 {
		switch kind {
		case core.CodecMP3:
			completed, err = MP3Headers(firstAU)
		case core.CodecAC3:
			completed, err = AC3Headers(firstAU)
		default:
			err = ErrCodec
		}
	}
	if err != nil {
		return core.AudioFormat{}, nil, err
	}
	// actual содержит формат полного нормативного заголовка, formatErr — ошибку его проверки.
	actual, formatErr := AudioFormatHeaders(kind, completed)
	if formatErr != nil {
		return core.AudioFormat{}, nil, formatErr
	}
	// sampleErr сообщает о противоречии firstAU и фактического заголовка.
	if _, sampleErr := AudioSamples(kind, completed, firstAU); sampleErr != nil {
		return core.AudioFormat{}, nil, sampleErr
	}
	return actual, completed, nil
}

// AudioSamples проверяет headers и ровно одну полную единицу доступа payload кодека kind.
// Результат — число отсчётов декодера на канал; желаемая длительность профиля не используется.
// Спектральные данные AAC/Opus/MP3 не декодируются; сохраняется ограниченная проверка их синтаксиса.
func AudioSamples(kind core.Codec, headers, payload []byte) (uint32, error) {
	// err сообщает об отказе полного конфигурационного заголовка.
	if _, err := AudioFormatHeaders(kind, headers); err != nil {
		return 0, err
	}
	switch kind {
	case core.CodecAAC:
		// err сохраняет прежнюю проверку AAC без изменения контракта длительности.
		if err := ValidateAACAU(headers, payload); err != nil {
			return 0, err
		}
		return 1024, nil
	case core.CodecOpus:
		return opusSamples(payload)
	case core.CodecFLAC:
		// frame содержит фактическое число отсчётов, err сообщает об отказе заголовка, подкадров или CRC.
		frame, err := FLACFrameInfo(headers, payload)
		return frame.Samples, err
	case core.CodecMP3:
		// err сообщает о неверной длине либо зависимости очередного кадра от резервуара предыдущих данных.
		// Дополнение, mode_extension и служебные флаги не являются конфигурацией декодера.
		if _, err := MP3Headers(payload); err != nil {
			return 0, err
		}
		return 1152, nil
	case core.CodecAC3:
		return ac3Samples(headers, payload)
	default:
		return 0, ErrCodec
	}
}

// ValidateAudioDuration требует положительную фактическую длительность новых четырёх кодеков.
// kind, headers и payload определяют число отсчётов; info и base должны выражать его точно.
// AAC возвращает nil, сохраняя прежний контракт, включая неизвестную длительность0.
func ValidateAudioDuration(kind core.Codec, headers, payload []byte, info media.PacketInfo, base media.TimeBase) error {
	if kind == core.CodecAAC {
		return nil
	}
	// samples содержит фактическое число отсчётов 48 кГц; err сообщает об отказе единицы доступа.
	samples, err := AudioSamples(kind, headers, payload)
	if err != nil {
		return err
	}
	if info.DurationTicks <= 0 || base.Validate() != nil {
		return fmt.Errorf("%w: требуется положительная audio duration", ErrTiming)
	}
	// numerator и denominator выражают фактическую длительность в отсчётах base.
	// Число отсчётов ограничено 4608, поэтому оба произведения полностью помещаются в uint64.
	numerator, denominator := uint64(samples)*uint64(base.Denominator), uint64(base.Numerator)*48000
	if numerator%denominator != 0 || numerator/denominator != uint64(info.DurationTicks) {
		return fmt.Errorf("%w: audio duration противоречит actual AU", ErrTiming)
	}
	return nil
}
