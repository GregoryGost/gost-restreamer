package codec

import (
	"bytes"
	"fmt"
	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
)

// Compatible проверяет фактический поддержанный формат против полного выходного профиля.
func Compatible(actual, desired core.MediaFormat) error {
	// err сообщает о несовпадении подтверждённого формата с выходным профилем.
	if err := core.CompatibleFormat(actual, desired); err != nil {
		return fmt.Errorf("%w: %w", ErrCodec, err)
	}
	return nil
}

// Inspector сохраняет доказанные сведения AV1 между TU одной конфигурации.
// Экземпляр принадлежит одному сериализованному источнику.
type Inspector struct {
	// video сохраняет конфигурацию, для которой разобрано текущее состояние AV1.
	video media.CodecConfig
	// av1 хранит параметры последовательности и подтверждённые сведения, сохраняемые между TU одной
	// конфигурации.
	av1 AV1Description
}

// Inspect сверяет фактические заголовки и AU с конфигурацией пакета и возвращает независимость видео
// по содержимому. Для AAC проверяет ASC и префикс AU, возвращая false; состояние AV1 сохраняется между
// пакетами одной конфигурации.
func (v *Inspector) Inspect(packet media.Packet) (bool, error) {
	// err сообщает о некорректной структуре проверяемого пакета.
	if err := packet.Validate(); err != nil {
		return false, err
	}
	// c содержит неизменяемую конфигурацию дорожки проверяемого пакета.
	c := packet.Config()
	// info содержит описание дорожки, с которым сравниваются фактические заголовки и данные.
	info := c.Info()
	if info.Track == media.TrackAudio {
		// actual содержит формат аудио, полученный из ASC. err сообщает об отказе разбора ASC.
		actual, err := AudioFormatASC(c.Headers())
		if err != nil {
			return false, err
		}
		if actual != info.Audio {
			return false, ErrCodec
		}
		if err = ValidateAACAU(c.Headers(), packet.Payload()); err != nil {
			return false, err
		}
		return false, nil
	}
	// actual принимает подтверждённый формат видео после разбора кодека.
	var actual core.VideoFormat
	// key принимает признак независимого видеокадра, вычисленный по сжатым данным.
	var key bool
	// err принимает ошибку разбора видео, проверки единицы доступа или совместимости формата.
	var err error
	switch info.Video.Codec {
	case core.CodecAV1:
		if v.video.Info() != info || !bytes.Equal(v.video.Headers(), c.Headers()) {
			v.av1, err = ParseAV1Config(c.Headers())
			if err != nil {
				return false, err
			}
			v.video = c
		}
		actual, key, err = v.av1.Inspect(packet.Payload(), info.Video.FrameRate)
	case core.CodecH264:
		// desc объединяет заголовки H.264 и заявленные размеры для их сверки с SPS.
		desc := VideoDescription{Headers: c.Headers(), Width: info.Video.Width, Height: info.Video.Height}
		actual, err = H264Format(desc)
		if err == nil {
			key, err = InspectH264AU(desc, packet.Payload())
		}
	default:
		return false, ErrCodec
	}
	if err != nil {
		return false, err
	}
	// desired объединяет заявленный формат видео и допустимый AAC, чтобы сравнить фактическое видео
	// через общий контракт форматов.
	desired := core.MediaFormat{Video: info.Video, Audio: core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", SampleRateHz: 48000, Channels: 2, ChannelLayout: "stereo"}}
	if err = Compatible(core.MediaFormat{Video: actual, Audio: desired.Audio}, desired); err != nil {
		return false, err
	}
	return key, nil
}
