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
	period := newPreparedPeriod(actual)
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
		if e = period.add(track, base, info.PTS, info.DTS, info.DurationTicks); e != nil {
			return e
		}
	}
	want := new(big.Rat).SetFrac64(int64(expected), int64(time.Second))
	got, err := period.duration()
	if err != nil {
		return err
	}
	if got.Cmp(want) != 0 {
		return fmt.Errorf("%w: продолжительность не соответствует вычисленной петле %s", core.ErrNotReady, expected)
	}
	return nil
}
