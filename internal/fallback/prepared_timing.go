package fallback

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"os"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
)

type packetSide struct {
	Type    string `json:"side_data_type"`
	Skip    uint64 `json:"skip_samples"`
	Discard uint64 `json:"discard_padding"`
}
type packetStamp struct {
	Type     string       `json:"codec_type"`
	Index    int          `json:"stream_index"`
	PTS      *int64       `json:"pts"`
	DTS      *int64       `json:"dts"`
	Duration *int64       `json:"duration"`
	Side     []packetSide `json:"side_data_list"`
}
type packetDocument struct {
	Streams []struct {
		Index int    `json:"index"`
		Type  string `json:"codec_type"`
		Base  string `json:"time_base"`
	} `json:"streams"`
	Packets []packetStamp `json:"packets"`
}
type preparedTiming struct {
	packets map[media.Track][]packetStamp
	bases   map[media.Track]media.TimeBase
}

func ticks(t int64, b media.TimeBase) *big.Rat {
	return new(big.Rat).Mul(new(big.Rat).SetInt64(t), new(big.Rat).SetFrac64(int64(b.Numerator), int64(b.Denominator)))
}

func (p Prober) inspectPreparedTiming(ctx context.Context, input *os.File, actual core.MediaFormat) (*preparedTiming, error) {
	if _, err := input.Seek(0, io.SeekStart); err != nil {
		return nil, err
	}
	var out bytes.Buffer
	cmd := Command{Binary: p.Binary, Args: []string{"-v", "error", "-protocol_whitelist", "fd,pipe", "-format_whitelist", "mov", "-fflags", "+nofillin", "-fd", "3", "-i", "fd:", "-show_packets", "-show_streams", "-show_entries", "packet=codec_type,stream_index,pts,dts,duration:packet_side_data=side_data_type,skip_samples,discard_padding:stream=index,codec_type,time_base", "-of", "json"}}
	if _, err := p.Runner.Run(ctx, cmd, input, &boundedWriter{w: &out, remaining: p.Limits.MaxJSONBytes}); err != nil {
		return nil, err
	}
	var doc packetDocument
	if err := json.Unmarshal(out.Bytes(), &doc); err != nil {
		return nil, err
	}
	if len(doc.Streams) != 2 || len(doc.Packets) == 0 || uint64(len(doc.Packets)) > p.Limits.MaxPackets {
		return nil, ErrLimit
	}
	plan := &preparedTiming{packets: map[media.Track][]packetStamp{}, bases: map[media.Track]media.TimeBase{}}
	indices := map[int]media.Track{}
	for _, stream := range doc.Streams {
		track := media.Track(stream.Type)
		if track != media.TrackVideo && track != media.TrackAudio {
			return nil, ErrInvalid
		}
		if _, ok := plan.bases[track]; ok {
			return nil, ErrInvalid
		}
		if _, ok := indices[stream.Index]; ok {
			return nil, ErrInvalid
		}
		rate, err := parseRate(stream.Base)
		if err != nil {
			return nil, err
		}
		plan.bases[track] = media.TimeBase{Numerator: rate.Numerator, Denominator: rate.Denominator}
		indices[stream.Index] = track
	}
	for _, packet := range doc.Packets {
		track, ok := indices[packet.Index]
		if ok && packet.Duration == nil {
			// ffprobe без синтетического заполнения не всегда сообщает duration.
			// Ожидание вычисляется из формата и обязательно сверяется с duration самого fMP4.
			base := plan.bases[track]
			numerator, denominator := uint64(1024), uint64(actual.Audio.SampleRateHz)
			if track == media.TrackVideo {
				numerator, denominator = uint64(actual.Video.FrameRate.Denominator), uint64(actual.Video.FrameRate.Numerator)
			}
			n := new(big.Int).Mul(new(big.Int).SetUint64(numerator), new(big.Int).SetUint64(uint64(base.Denominator)))
			d := new(big.Int).Mul(new(big.Int).SetUint64(denominator), new(big.Int).SetUint64(uint64(base.Numerator)))
			q, r := new(big.Int), new(big.Int)
			q.QuoRem(n, d, r)
			if r.Sign() != 0 || !q.IsInt64() {
				return nil, core.ErrCorrupt
			}
			duration := q.Int64()
			packet.Duration = &duration
		}
		if !ok || string(track) != packet.Type || packet.PTS == nil || packet.DTS == nil || packet.Duration == nil || *packet.Duration <= 0 {
			return nil, fmt.Errorf("%w: ffprobe не подтвердил время packet %s index %d", core.ErrCorrupt, packet.Type, packet.Index)
		}
		plan.packets[track] = append(plan.packets[track], packet)
	}
	audio, video := plan.packets[media.TrackAudio], plan.packets[media.TrackVideo]
	if len(audio) < 2 || len(video) == 0 {
		return nil, core.ErrNotReady
	}
	first := audio[0]
	if len(first.Side) != 1 || first.Side[0].Type != "Skip Samples" || first.Side[0].Skip != 1024 || first.Side[0].Discard != 0 || !exactAudioDuration(*first.Duration, plan.bases[media.TrackAudio], actual.Audio.SampleRateHz) {
		return nil, fmt.Errorf("%w: не подтверждён целый пакет задержки AAC-кодировщика", core.ErrNotReady)
	}
	firstEnd := new(big.Rat).Add(ticks(*first.DTS, plan.bases[media.TrackAudio]), ticks(*first.Duration, plan.bases[media.TrackAudio]))
	if *first.PTS != *first.DTS || firstEnd.Sign() != 0 || *audio[1].DTS != 0 {
		return nil, fmt.Errorf("%w: неподдержанное начальное смещение A/V", core.ErrNotReady)
	}
	period := newPreparedPeriod(actual)
	for track, packets := range plan.packets {
		start := 0
		if track == media.TrackAudio {
			start = 1
		}
		for _, packet := range packets[start:] {
			if len(packet.Side) != 0 {
				return nil, core.ErrNotReady
			}
			if err := period.add(track, plan.bases[track], *packet.PTS, *packet.DTS, *packet.Duration); err != nil {
				return nil, err
			}
		}
	}
	if _, err := period.duration(); err != nil {
		return nil, err
	}
	return plan, nil
}
func (t *preparedTiming) match(packet media.Packet, index int) error {
	track := packet.Config().Info().Track
	if index >= len(t.packets[track]) {
		return core.ErrCorrupt
	}
	expected := t.packets[track][index]
	info := packet.Info()
	base := packet.Config().Info().TimeBase
	probeBase := t.bases[track]
	for _, pair := range [][2]int64{{info.PTS, *expected.PTS}, {info.DTS, *expected.DTS}, {info.DurationTicks, *expected.Duration}} {
		if ticks(pair[0], base).Cmp(ticks(pair[1], probeBase)) != 0 {
			return fmt.Errorf("%w: %s[%d] fMP4 %s != ffprobe %s", core.ErrCorrupt, track, index, ticks(pair[0], base), ticks(pair[1], probeBase))
		}
	}
	return nil
}
