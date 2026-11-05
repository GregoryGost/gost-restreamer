package rtmp

import (
	"bytes"
	"context"
	"math"
	"math/big"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/bluenviron/gortmplib/pkg/amf0"
)

type outboundClock struct {
	hasOrigin bool
	origin    int64
	last      [2]int64
	hasLast   [2]bool
}

// OutboundWriteResult подтверждает полную запись одного AU, отдельно от заголовков.
type OutboundWriteResult struct {
	PayloadBytes uint64
	Track        media.Track
	Independent  bool
}

// QuantizeOutboundMilliseconds округляет точную дробь к ближайшей миллисекунде.
// Половина округляется от нуля; промежуточные произведения не ограничены int64.
func QuantizeOutboundMilliseconds(ticks int64, base media.TimeBase) (int64, error) {
	if err := base.Validate(); err != nil {
		return 0, outboundError(OutboundTiming, err)
	}
	var n, d, q, rem big.Int
	n.SetInt64(ticks)
	n.Mul(&n, new(big.Int).SetUint64(uint64(base.Numerator)*1000))
	d.SetUint64(uint64(base.Denominator))
	negative := n.Sign() < 0
	n.Abs(&n)
	q.QuoRem(&n, &d, &rem)
	rem.Lsh(&rem, 1)
	if rem.Cmp(&d) >= 0 {
		q.Add(&q, big.NewInt(1))
	}
	if negative {
		q.Neg(&q)
	}
	if !q.IsInt64() {
		return 0, outboundError(OutboundTiming, codec.ErrTiming)
	}
	return q.Int64(), nil
}

func outboundSubtract(a, b int64) (int64, error) {
	if b > 0 && a < math.MinInt64+b || b < 0 && a > math.MaxInt64+b {
		return 0, outboundError(OutboundTiming, codec.ErrTiming)
	}
	return a - b, nil
}

func (c *outboundClock) project(p media.Packet, independent bool) (uint32, int32, error) {
	info, config := p.Info(), p.Config().Info()
	if (config.Track == media.TrackAudio || config.Video.Codec == core.CodecAV1) && info.PTS != info.DTS {
		return 0, 0, outboundError(OutboundTiming, codec.ErrTiming)
	}
	pts, err := QuantizeOutboundMilliseconds(info.PTS, config.TimeBase)
	if err != nil {
		return 0, 0, err
	}
	dts, err := QuantizeOutboundMilliseconds(info.DTS, config.TimeBase)
	if err != nil {
		return 0, 0, err
	}
	cts, err := outboundSubtract(pts, dts)
	if err != nil || cts < -(1<<23) || cts > (1<<23)-1 {
		return 0, 0, outboundError(OutboundTiming, codec.ErrTiming)
	}
	if !c.hasOrigin {
		if config.Track != media.TrackVideo || !independent {
			return 0, 0, outboundError(OutboundMedia, codec.ErrUnready)
		}
		c.origin = min(pts, dts)
		c.hasOrigin = true
	}
	logical, err := outboundSubtract(dts, c.origin)
	if err != nil || logical < 0 {
		return 0, 0, outboundError(OutboundTiming, codec.ErrTiming)
	}
	if _, err = outboundSubtract(pts, c.origin); err != nil {
		return 0, 0, err
	}
	i := 0
	if config.Track == media.TrackAudio {
		i = 1
	}
	if c.hasLast[i] && (logical < c.last[i] || logical-c.last[i] >= 1<<31) {
		return 0, 0, outboundError(OutboundTiming, codec.ErrTiming)
	}
	c.last[i] = logical
	c.hasLast[i] = true
	return uint32(uint64(logical) % (uint64(1) << 32)), int32(cts), nil
}

func sameOutboundConfig(a, b media.CodecConfig) bool {
	return a.Info() == b.Info() && bytes.Equal(a.Headers(), b.Headers())
}

// WriteHeaders проверяет и подготавливает заголовки следующего восстановления.
// Метаданные и заголовки последовательностей пишутся перед следующим проверенным независимым AU,
// чтобы метаданные AV1 опирались на фактические размеры показанного ключевого кадра.
// Здоровое соединение сохраняет общее начало отсчёта и предыдущие логические DTS.
func (c *OutboundConn) WriteHeaders(ctx context.Context, video, audio media.CodecConfig) error {
	if err := c.beginWrite(); err != nil {
		return err
	}
	defer c.writes.Done()
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	if err := c.validateHeaders(video, audio); err != nil {
		return err
	}
	c.video = video
	c.audio = audio
	c.headersPending = true
	c.requireIndependent = true
	c.inspector = codec.Inspector{}
	return nil
}

func (c *OutboundConn) validateHeaders(video, audio media.CodecConfig) error {
	if video.StoredBytes() > uint64(c.opts.Limits.MaxConfigBytes) || audio.StoredBytes() > uint64(c.opts.Limits.MaxConfigBytes) || video.StoredBytes()+5 > uint64(c.opts.Limits.MaxMessageBytes) || audio.StoredBytes()+2 > uint64(c.opts.Limits.MaxMessageBytes) {
		return outboundError(OutboundMedia, ErrLimit)
	}
	if err := video.Validate(); err != nil {
		return outboundError(OutboundMedia, err)
	}
	if err := audio.Validate(); err != nil {
		return outboundError(OutboundMedia, err)
	}
	vi, ai := video.Info(), audio.Info()
	if vi.Track != media.TrackVideo || ai.Track != media.TrackAudio || vi.SourceEpoch != ai.SourceEpoch {
		return outboundError(OutboundMedia, ErrCodec)
	}
	if err := codec.Compatible(core.MediaFormat{Video: vi.Video, Audio: ai.Audio}, c.target.profile); err != nil {
		return outboundError(OutboundMedia, err)
	}
	actual, err := codec.AudioFormatASC(audio.Headers())
	if err != nil {
		return outboundError(OutboundMedia, err)
	}
	if actual != ai.Audio {
		return outboundError(OutboundMedia, ErrCodec)
	}
	switch vi.Video.Codec {
	case core.CodecH264:
		actual, err := codec.H264Format(codec.VideoDescription{Headers: video.Headers(), Width: vi.Video.Width, Height: vi.Video.Height})
		if err != nil {
			return outboundError(OutboundMedia, err)
		}
		if err = codec.Compatible(core.MediaFormat{Video: actual, Audio: ai.Audio}, c.target.profile); err != nil {
			return outboundError(OutboundMedia, err)
		}
	case core.CodecAV1:
		if _, err = codec.ParseAV1Config(video.Headers()); err != nil {
			return outboundError(OutboundMedia, err)
		}
	default:
		return outboundError(OutboundMedia, ErrCodec)
	}
	return nil
}

// WritePacket допускает и завершает целые RTMP-сообщения. Любая ошибка записи
// закрывает сокет; повторная отправка оставшейся части сообщения не допускается.
func (c *OutboundConn) WritePacket(ctx context.Context, p media.Packet) (OutboundWriteResult, error) {
	return c.writePacket(ctx, p, nil)
}

// WriteDelivery повторно проверяет актуальность после ожидания сериализатора.
// Устаревшая доставка возвращается без записи и без закрытия здорового сокета.
func (c *OutboundConn) WriteDelivery(ctx context.Context, d media.Delivery) (OutboundWriteResult, error) {
	return c.writePacket(ctx, d.Packet, d.IsCurrent)
}

func (c *OutboundConn) writePacket(ctx context.Context, p media.Packet, current func() bool) (OutboundWriteResult, error) {
	var result OutboundWriteResult
	err := c.withCurrentWriter(ctx, current, func() error {
		if p.StoredBytes() > uint64(c.opts.Limits.MaxMessageBytes) {
			return outboundError(OutboundMedia, ErrLimit)
		}
		info := p.Config().Info()
		cfg := c.video
		if info.Track == media.TrackAudio {
			cfg = c.audio
		}
		if !sameOutboundConfig(p.Config(), cfg) {
			return outboundError(OutboundMedia, ErrCodec)
		}
		independent, err := c.inspector.Inspect(p)
		if err != nil {
			return outboundError(OutboundMedia, err)
		}
		if c.requireIndependent && (info.Track != media.TrackVideo || !independent) {
			return outboundError(OutboundMedia, codec.ErrUnready)
		}
		clock := c.clock
		ts, cts, err := clock.project(p, independent)
		if err != nil {
			return err
		}
		payload := p.Payload()
		prefix := []byte{0xaf, 1}
		csid, typ := byte(4), byte(8)
		if info.Track == media.TrackVideo {
			csid, typ = 6, 9
			frame := byte(2)
			if independent {
				frame = 1
			}
			if info.Video.Codec == core.CodecAV1 {
				prefix = []byte{0x80 | frame<<4 | 1, 'a', 'v', '0', '1'}
			} else {
				v := uint32(cts) & 0xffffff
				prefix = []byte{frame<<4 | 7, 1, byte(v >> 16), byte(v >> 8), byte(v)}
			}
		}
		if uint64(len(prefix))+uint64(len(payload)) > uint64(c.opts.Limits.MaxMessageBytes) {
			return outboundError(OutboundMedia, ErrLimit)
		}
		if c.headersPending {
			if err = c.writeHeadersLocked(ts); err != nil {
				return err
			}
		}
		body := make([]byte, len(prefix)+len(payload))
		copy(body, prefix)
		copy(body[len(prefix):], payload)
		if err = c.writeFramed(csid, typ, c.streamID.Load(), ts, body); err != nil {
			return err
		}
		c.clock = clock
		c.requireIndependent = false
		result = OutboundWriteResult{PayloadBytes: uint64(len(payload)), Track: info.Track, Independent: independent}
		return nil
	})
	return result, err
}

func (c *OutboundConn) writeHeadersLocked(ts uint32) error {
	vi, ai := c.video.Info().Video, c.audio.Info().Audio
	videoCodec := float64(7)
	if vi.Codec == core.CodecAV1 {
		videoCodec = float64(0x61763031)
	}
	metadata := amf0.Data{"@setDataFrame", "onMetaData", amf0.ECMAArray{
		{Key: "videocodecid", Value: videoCodec}, {Key: "width", Value: float64(vi.Width)}, {Key: "height", Value: float64(vi.Height)},
		{Key: "framerate", Value: float64(vi.FrameRate.Numerator) / float64(vi.FrameRate.Denominator)},
		{Key: "audiocodecid", Value: float64(10)}, {Key: "audiosamplerate", Value: float64(ai.SampleRateHz)}, {Key: "audiochannels", Value: float64(ai.Channels)}, {Key: "stereo", Value: ai.Channels == 2},
	}}
	b, err := c.marshalAMF(metadata)
	if err != nil {
		return err
	}
	if err = c.writeFramed(5, 18, c.streamID.Load(), ts, b); err != nil {
		return err
	}
	prefix := []byte{0x17, 0, 0, 0, 0}
	if vi.Codec == core.CodecAV1 {
		prefix = []byte{0x90, 'a', 'v', '0', '1'}
	}
	v := append(prefix, c.video.Headers()...)
	if err = c.writeFramed(6, 9, c.streamID.Load(), ts, v); err != nil {
		return err
	}
	a := append([]byte{0xaf, 0}, c.audio.Headers()...)
	if err = c.writeFramed(4, 8, c.streamID.Load(), ts, a); err != nil {
		return err
	}
	c.headersPending = false
	return nil
}
