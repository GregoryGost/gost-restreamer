package fallback

import (
	"context"
	"errors"
	"io"
	"os"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

// NormalizePrepared применяется только к выходу принадлежащего менеджеру нативного AAC-кодировщика.
// Удаляется лишь целый пакет задержки кодировщика, явно помеченный Skip Samples в фактическом MP4.
func (p Prober) NormalizePrepared(ctx context.Context, input *os.File, output io.Writer) error {
	actual, err := p.observedFormat(ctx, input)
	if err != nil {
		return err
	}
	timing, err := p.inspectPreparedTiming(ctx, input, actual)
	if err != nil {
		return err
	}
	if _, err = input.Seek(0, io.SeekStart); err != nil {
		return err
	}
	d, err := fmp4.NewDemuxer(contextReader{ctx, input}, fmp4.DemuxOptions{Limits: p.Limits.Container, SourceEpoch: 1, VideoRevision: 1, AudioRevision: 1, ResolveConfig: func(desc fmp4.TrackDescription) (media.CodecConfig, error) {
		info := media.CodecConfigInfo{Track: desc.Track, SourceEpoch: 1, Revision: 1, TimeBase: desc.TimeBase}
		if desc.Track == media.TrackVideo {
			if desc.Codec != actual.Video.Codec || desc.Width != actual.Video.Width || desc.Height != actual.Video.Height {
				return media.CodecConfig{}, core.ErrCorrupt
			}
			info.Video = actual.Video
		} else {
			info.Audio = actual.Audio
		}
		return media.NewCodecConfig(info, desc.Headers)
	}})
	if err != nil {
		return err
	}
	init := d.Init()
	var prefix []media.Packet
	var prefixBytes uint64
	video, audio := false, false
	for !video || !audio {
		packet, e := d.Next()
		if e != nil {
			return e
		}
		if uint64(len(prefix)) >= p.Limits.MaxPackets || packet.StoredBytes() > p.Limits.Container.MaxInitBytes-prefixBytes {
			return ErrLimit
		}
		prefix = append(prefix, packet)
		prefixBytes += packet.StoredBytes()
		info := packet.Config().Info()
		if !video && info.Track == media.TrackVideo {
			if !exactDuration(packet.Info().DurationTicks, info.TimeBase, actual.Video.FrameRate) {
				return core.ErrCorrupt
			}
			if actual.Video.Codec == core.CodecAV1 {
				headers, format, e := codec.AV1ConfigFromTU(init.Video.Headers(), packet.Payload(), actual.Video.FrameRate)
				if e != nil {
					return e
				}
				if e = codec.Compatible(core.MediaFormat{Video: format, Audio: actual.Audio}, actual); e != nil {
					return e
				}
				info.Video = format
				init.Video, e = media.NewCodecConfig(info, headers)
				if e != nil {
					return e
				}
			}
			video = true
		}
		if info.Track == media.TrackAudio {
			audio = true
		}
	}
	init.Origin, err = preparedOrigin(*timing.packets[media.TrackVideo][0].DTS, timing.bases[media.TrackVideo], init.Video.Info().TimeBase, init.Audio.Info().TimeBase)
	if err != nil {
		return err
	}
	mux, err := fmp4.NewMuxer(output, init, p.Limits.Container)
	if err != nil {
		return err
	}
	var inspector codec.Inspector
	counts := map[media.Track]int{}
	write := func(packet media.Packet) error {
		track := packet.Config().Info().Track
		index := counts[track]
		counts[track]++
		if err := timing.match(packet, index); err != nil {
			return err
		}
		if track == media.TrackAudio && index == 0 {
			if err := codec.ValidateAACAU(init.Audio.Headers(), packet.Payload()); err != nil {
				return err
			}
			return nil
		}
		config := init.Audio
		if packet.Config().Info().Track == media.TrackVideo {
			config = init.Video
		}
		rebuilt, e := media.NewPacket(packet.Info(), config, packet.Payload())
		if e != nil {
			return e
		}
		key, e := inspector.Inspect(rebuilt)
		if e != nil {
			return e
		}
		if track == media.TrackVideo && index == 0 && !key {
			return core.ErrNotReady
		}
		return mux.WritePacket(rebuilt)
	}
	for _, packet := range prefix {
		if err = write(packet); err != nil {
			return err
		}
	}
	for total := uint64(len(prefix)); ; total++ {
		packet, e := d.Next()
		if errors.Is(e, io.EOF) {
			if counts[media.TrackVideo] != len(timing.packets[media.TrackVideo]) || counts[media.TrackAudio] != len(timing.packets[media.TrackAudio]) {
				return core.ErrCorrupt
			}
			return nil
		}
		if e != nil {
			return e
		}
		if total >= p.Limits.MaxPackets {
			return ErrLimit
		}
		if err = write(packet); err != nil {
			return err
		}
	}
}
