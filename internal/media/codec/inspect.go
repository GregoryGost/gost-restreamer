package codec

import (
	"bytes"
	"fmt"
	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
)

// Compatible проверяет фактический поддержанный формат против полного выходного профиля.
func Compatible(actual, desired core.MediaFormat) error {
	if err := core.CompatibleFormat(actual, desired); err != nil {
		return fmt.Errorf("%w: %w", ErrCodec, err)
	}
	return nil
}

// Inspector сохраняет доказанные сведения AV1 между TU одной конфигурации.
// Экземпляр принадлежит одному сериализованному источнику.
type Inspector struct {
	video media.CodecConfig
	av1   AV1Description
}

func (v *Inspector) Inspect(packet media.Packet) (bool, error) {
	if err := packet.Validate(); err != nil {
		return false, err
	}
	c := packet.Config()
	info := c.Info()
	if info.Track == media.TrackAudio {
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
	var actual core.VideoFormat
	var key bool
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
	desired := core.MediaFormat{Video: info.Video, Audio: core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", SampleRateHz: 48000, Channels: 2, ChannelLayout: "stereo"}}
	if err = Compatible(core.MediaFormat{Video: actual, Audio: desired.Audio}, desired); err != nil {
		return false, err
	}
	return key, nil
}
