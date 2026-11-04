package fmp4

import (
	"errors"
	"fmt"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
)

// ResolveCodecConfig восстанавливает описание прогрессивного H.264 и AAC-LC mono/stereo.
// FPS берётся из TimingInfo в VUI, цвет без сигнализации остаётся unspecified.
// Для AV1 без подтверждённых фактических размеров кадров нужен ResolveConfig.
func ResolveCodecConfig(desc TrackDescription, epoch, revision uint64) (media.CodecConfig, error) {
	if err := desc.TimeBase.Validate(); err != nil {
		return media.CodecConfig{}, err
	}
	info := media.CodecConfigInfo{Track: desc.Track, SourceEpoch: epoch, Revision: revision, TimeBase: desc.TimeBase}
	var err error
	switch desc.Codec {
	case core.CodecH264:
		info.Video, err = videoFromAVCC(desc)
	case core.CodecAAC:
		info.Audio, err = audioFromASC(desc.Headers)
	default:
		err = fmt.Errorf("%w: описание %s требует codec resolver", ErrMetadataUnavailable, desc.Codec)
	}
	if err != nil {
		return media.CodecConfig{}, err
	}
	if desc.Codec == core.CodecAAC && (desc.Track != media.TrackAudio || desc.SampleRateHz != info.Audio.SampleRateHz || desc.Channels != info.Audio.Channels) {
		return media.CodecConfig{}, fmt.Errorf("%w: sample entry противоречит ASC", ErrInvalid)
	}
	if desc.Codec == core.CodecH264 && desc.Track != media.TrackVideo {
		return media.CodecConfig{}, ErrInvalid
	}
	return media.NewCodecConfig(info, desc.Headers)
}

func validateWireConfig(c media.CodecConfig, desc TrackDescription) error {
	info := c.Info()
	if err := c.Validate(); err != nil {
		return err
	}
	if info.Track != desc.Track || !sameHeaders(c, desc.Headers) {
		return ErrInvalid
	}
	if _, err := rescale(1, info.TimeBase, desc.TimeBase); err != nil {
		return err
	}
	if info.Track == media.TrackVideo {
		if info.Video.Codec != desc.Codec || info.Video.Width != desc.Width || info.Video.Height != desc.Height {
			return ErrInvalid
		}
		if desc.Codec == core.CodecH264 {
			if _, err := spsFromAVCC(desc); err != nil {
				return err
			}
		}
	} else {
		actual, err := audioFromASC(desc.Headers)
		if err != nil {
			return err
		}
		if info.Audio.SampleRateHz != desc.SampleRateHz || info.Audio.Channels != desc.Channels || info.Audio.SampleRateHz != actual.SampleRateHz || info.Audio.Channels != actual.Channels {
			return fmt.Errorf("%w: описание audio противоречит ASC", ErrInvalid)
		}
	}
	return nil
}
func spsFromAVCC(d TrackDescription) (h264.SPS, error) {
	s, e := codec.H264SPS(codec.VideoDescription{Headers: d.Headers, Width: d.Width, Height: d.Height})
	if e != nil {
		return s, translateCodecError(e)
	}
	return s, nil
}
func videoFromAVCC(d TrackDescription) (core.VideoFormat, error) {
	v, e := codec.H264Format(codec.VideoDescription{Headers: d.Headers, Width: d.Width, Height: d.Height})
	if e != nil {
		return v, translateCodecError(e)
	}
	return v, nil
}
func audioFromASC(b []byte) (core.AudioFormat, error) {
	a, e := codec.AudioFormatASC(b)
	if e != nil {
		return a, translateCodecError(e)
	}
	return a, nil
}

func translateCodecError(e error) error {
	target := ErrInvalid
	if errors.Is(e, codec.ErrUnready) {
		target = ErrMetadataUnavailable
	}
	if errors.Is(e, codec.ErrTiming) {
		target = ErrUnsupportedTiming
	}
	return fmt.Errorf("%w: %w", target, e)
}
