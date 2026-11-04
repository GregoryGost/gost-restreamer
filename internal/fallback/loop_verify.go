package fallback

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

// VerifyLoopDuration сверяет обе фактические дорожки с явно вычисленной длиной петли.
func (p Prober) VerifyLoopDuration(ctx context.Context, file *os.File, expected time.Duration) error {
	if expected <= 0 {
		return ErrInvalid
	}
	actual, err := p.observedFormat(ctx, file)
	if err != nil {
		return err
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	d, err := fmp4.NewDemuxer(contextReader{ctx, file}, fmp4.DemuxOptions{Limits: p.Limits.Container, SourceEpoch: 1, VideoRevision: 1, AudioRevision: 1, ResolveConfig: func(desc fmp4.TrackDescription) (media.CodecConfig, error) {
		info := media.CodecConfigInfo{Track: desc.Track, SourceEpoch: 1, Revision: 1, TimeBase: desc.TimeBase}
		if desc.Track == media.TrackVideo {
			info.Video = actual.Video
		} else {
			info.Audio = actual.Audio
		}
		return media.NewCodecConfig(info, desc.Headers)
	}})
	if err != nil {
		return err
	}
	ends := map[media.Track]*big.Rat{}
	for n := uint64(0); ; n++ {
		packet, e := d.Next()
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			return e
		}
		if n >= p.Limits.MaxPackets {
			return ErrLimit
		}
		track, base, info := packet.Config().Info().Track, packet.Config().Info().TimeBase, packet.Info()
		if info.PTS != info.DTS || info.DurationTicks <= 0 {
			return core.ErrNotReady
		}
		begin := ticks(info.DTS, base)
		if end := ends[track]; end == nil {
			if begin.Sign() != 0 {
				return core.ErrNotReady
			}
		} else if begin.Cmp(end) != 0 {
			return core.ErrCorrupt
		}
		ends[track] = new(big.Rat).Add(begin, ticks(info.DurationTicks, base))
	}
	want := new(big.Rat).SetFrac64(int64(expected), int64(time.Second))
	for _, track := range []media.Track{media.TrackVideo, media.TrackAudio} {
		if ends[track] == nil || ends[track].Cmp(want) != 0 {
			return fmt.Errorf("%w: длина %s не соответствует вычисленной петле %s", core.ErrNotReady, track, expected)
		}
	}
	return nil
}
