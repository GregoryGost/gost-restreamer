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

// outboundClock сохраняет общее начало исходящего потока и последние логические DTS двух дорожек.
type outboundClock struct {
	// hasOrigin показывает, установлено ли начало по первому независимому видеокадру.
	hasOrigin bool
	// origin содержит начало исходящей шкалы в миллисекундах до сетевого сворачивания.
	origin int64
	// last хранит последние логические DTS видео и аудио в миллисекундах.
	last [2]int64
	// hasLast показывает, установлена ли предыдущая метка каждой из двух дорожек.
	hasLast [2]bool
}

// OutboundWriteResult подтверждает полную запись одного AU, отдельно от заголовков.
type OutboundWriteResult struct {
	// PayloadBytes содержит размер исходной сжатой AU в байтах без RTMP-префикса и заголовков.
	PayloadBytes uint64
	// Track обозначает дорожку полностью записанной AU.
	Track media.Track
	// Independent показывает независимость записанного видеокадра по фактическим сжатым данным.
	Independent bool
}

// QuantizeOutboundMilliseconds округляет точную дробь к ближайшей миллисекунде.
// Половина округляется от нуля; промежуточные произведения не ограничены int64.
func QuantizeOutboundMilliseconds(ticks int64, base media.TimeBase) (int64, error) {
	// err сообщает о некорректной единице времени переводимой метки.
	if err := base.Validate(); err != nil {
		return 0, outboundError(OutboundTiming, err)
	}
	// n хранит числитель времени в миллисекундах. d хранит знаменатель. q принимает целую
	// округлённую метку. rem принимает остаток для округления половины от нуля.
	var n, d, q, rem big.Int
	n.SetInt64(ticks)
	n.Mul(&n, new(big.Int).SetUint64(uint64(base.Numerator)*1000))
	d.SetUint64(uint64(base.Denominator))
	// negative сохраняет знак метки перед вычислением модуля и округлением.
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

// Вычитает знаковые временные значения с проверкой границ int64 и окончательной ошибкой OutboundTiming
// при переполнении.
func outboundSubtract(a, b int64) (int64, error) {
	if b > 0 && a < math.MinInt64+b || b < 0 && a > math.MaxInt64+b {
		return 0, outboundError(OutboundTiming, codec.ErrTiming)
	}
	return a - b, nil
}

// Проецирует PTS/DTS в миллисекунды с общим началом первого независимого видео. Проверяет signed24
// смещение и неубывающие однозначные DTS дорожек, сохраняя логическую шкалу при сетевом сворачивании
// по модулю 2³².
func (c *outboundClock) project(p media.Packet, independent bool) (uint32, int32, error) {
	// info содержит метки пакета. config содержит описание его дорожки и единицу времени.
	info, config := p.Info(), p.Config().Info()
	if (config.Track == media.TrackAudio || config.Video.Codec == core.CodecAV1) && info.PTS != info.DTS {
		return 0, 0, outboundError(OutboundTiming, codec.ErrTiming)
	}
	// pts содержит округлённое время показа в миллисекундах. err сообщает о невозможности его
	// представления.
	pts, err := QuantizeOutboundMilliseconds(info.PTS, config.TimeBase)
	if err != nil {
		return 0, 0, err
	}
	// dts содержит округлённое время декодирования в миллисекундах.
	dts, err := QuantizeOutboundMilliseconds(info.DTS, config.TimeBase)
	if err != nil {
		return 0, 0, err
	}
	// cts содержит знаковое смещение времени показа относительно декодирования в миллисекундах.
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
	// logical содержит DTS относительно общего начала исходящего потока в миллисекундах.
	logical, err := outboundSubtract(dts, c.origin)
	if err != nil || logical < 0 {
		return 0, 0, outboundError(OutboundTiming, codec.ErrTiming)
	}
	if _, err = outboundSubtract(pts, c.origin); err != nil {
		return 0, 0, err
	}
	// i выбирает состояние видеодорожки под индексом 0 либо аудиодорожки под индексом 1.
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

// Сравнивает точные описания конфигураций и исходные байты заголовков.
func sameOutboundConfig(a, b media.CodecConfig) bool {
	return a.Info() == b.Info() && bytes.Equal(a.Headers(), b.Headers())
}

// WriteHeaders проверяет и подготавливает заголовки следующего восстановления.
// Метаданные и заголовки последовательностей пишутся перед следующим проверенным независимым AU,
// чтобы метаданные AV1 опирались на фактические размеры показанного ключевого кадра.
// Здоровое соединение сохраняет общее начало отсчёта и предыдущие логические DTS.
func (c *OutboundConn) WriteHeaders(ctx context.Context, video, audio media.CodecConfig) error {
	// err сообщает, что соединение уже закрыто и обновление заголовков нельзя зарегистрировать.
	if err := c.beginWrite(); err != nil {
		return err
	}
	defer c.writes.Done()
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	// err сообщает об отмене обновления заголовков после ожидания сериализатора.
	if err := ctx.Err(); err != nil {
		return err
	}
	// err сообщает о превышении бюджетов или несогласованности пары заголовков.
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

// Проверяет бюджеты и согласованность пары конфигураций с назначением, фактическим ASC и H.264 SPS
// либо допустимым av1C.
func (c *OutboundConn) validateHeaders(video, audio media.CodecConfig) error {
	if video.StoredBytes() > uint64(c.opts.Limits.MaxConfigBytes) || audio.StoredBytes() > uint64(c.opts.Limits.MaxConfigBytes) || video.StoredBytes()+5 > uint64(c.opts.Limits.MaxMessageBytes) || audio.StoredBytes()+2 > uint64(c.opts.Limits.MaxMessageBytes) {
		return outboundError(OutboundMedia, ErrLimit)
	}
	// err сообщает о некорректной видеоконфигурации.
	if err := video.Validate(); err != nil {
		return outboundError(OutboundMedia, err)
	}
	// err сообщает о некорректной аудиоконфигурации.
	if err := audio.Validate(); err != nil {
		return outboundError(OutboundMedia, err)
	}
	// vi содержит описание видеоконфигурации. ai содержит описание аудиоконфигурации.
	vi, ai := video.Info(), audio.Info()
	if vi.Track != media.TrackVideo || ai.Track != media.TrackAudio || vi.SourceEpoch != ai.SourceEpoch {
		return outboundError(OutboundMedia, ErrCodec)
	}
	// err сообщает о несовместимости заявленной пары дорожек с назначением.
	if err := codec.Compatible(core.MediaFormat{Video: vi.Video, Audio: ai.Audio}, c.target.profile); err != nil {
		return outboundError(OutboundMedia, err)
	}
	// actual содержит фактический аудиоформат ASC. err сообщает об отказе его разбора.
	actual, err := codec.AudioFormatASC(audio.Headers())
	if err != nil {
		return outboundError(OutboundMedia, err)
	}
	if actual != ai.Audio {
		return outboundError(OutboundMedia, ErrCodec)
	}
	switch vi.Video.Codec {
	case core.CodecH264:
		// actual содержит фактический формат H.264 из SPS. err сообщает о повреждении или
		// недостаточных сведениях SPS.
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

// Сериализует проверенный актуальный AU под общим допуском записи, при необходимости предваряя его
// метаданными и заголовками. Время и результат полной записи обновляются после успешной отправки всего
// сообщения.
func (c *OutboundConn) writePacket(ctx context.Context, p media.Packet, current func() bool) (OutboundWriteResult, error) {
	// result подтверждает размер нагрузки и свойства AU только после полной успешной записи.
	var result OutboundWriteResult
	// err содержит результат допуска, проверки актуальности и сериализованной записи AU.
	err := c.withCurrentWriter(ctx, current, func() error {
		// Под общим сериализатором проверяет конфигурацию и независимый старт пакета, рассчитывает временные
		// метки, передаёт ожидающие заголовки и AU; состояние часов и результат обновляет после полной записи.

		if p.StoredBytes() > uint64(c.opts.Limits.MaxMessageBytes) {
			return outboundError(OutboundMedia, ErrLimit)
		}
		// info содержит описание дорожки записываемого пакета.
		info := p.Config().Info()
		// cfg выбирает ожидаемую конфигурацию той же дорожки из состояния соединения.
		cfg := c.video
		if info.Track == media.TrackAudio {
			cfg = c.audio
		}
		if !sameOutboundConfig(p.Config(), cfg) {
			return outboundError(OutboundMedia, ErrCodec)
		}
		// independent показывает независимость AU по содержимому. err сообщает об отказе
		// проверки сжатых данных.
		independent, err := c.inspector.Inspect(p)
		if err != nil {
			return outboundError(OutboundMedia, err)
		}
		if c.requireIndependent && (info.Track != media.TrackVideo || !independent) {
			return outboundError(OutboundMedia, codec.ErrUnready)
		}
		// clock содержит копию часов, которая сохраняется только после успешной записи
		// сообщения.
		clock := c.clock
		// ts содержит сетевой DTS по модулю 2³² миллисекунд. cts содержит знаковое смещение PTS
		// относительно DTS.
		ts, cts, err := clock.project(p, independent)
		if err != nil {
			return err
		}
		// payload содержит отдельную копию сжатой нагрузки AU.
		payload := p.Payload()
		// prefix содержит аудио- или видеопрефикс тела RTMP перед исходной нагрузкой.
		prefix := []byte{0xaf, 1}
		// csid задаёт поток блоков для выбранной дорожки. typ задаёт тип аудио- или
		// видеосообщения RTMP.
		csid, typ := byte(4), byte(8)
		if info.Track == media.TrackVideo {
			csid, typ = 6, 9
			// frame содержит признак ключевого либо зависимого видеокадра для RTMP-префикса.
			frame := byte(2)
			if independent {
				frame = 1
			}
			if info.Video.Codec == core.CodecAV1 {
				prefix = []byte{0x80 | frame<<4 | 1, 'a', 'v', '0', '1'}
			} else {
				// v содержит младшие 24 бита знакового смещения PTS относительно DTS для
				// H.264.
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
		// body объединяет RTMP-префикс и сжатую нагрузку в полное тело сообщения.
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

// Записывает метаданные и исходные видео-/аудиозаголовки на заданной метке перед независимым AU;
// вызывающий удерживает сериализатор и допуск записи.
func (c *OutboundConn) writeHeadersLocked(ts uint32) error {
	// vi содержит выходной формат видео. ai содержит выходной формат аудио для метаданных.
	vi, ai := c.video.Info().Video, c.audio.Info().Audio
	// videoCodec содержит числовое обозначение H.264 либо FourCC AV1 для AMF-метаданных.
	videoCodec := float64(7)
	if vi.Codec == core.CodecAV1 {
		videoCodec = float64(0x61763031)
	}
	// metadata содержит onMetaData с фактическими размерами, FPS и аудиопараметрами.
	metadata := amf0.Data{"@setDataFrame", "onMetaData", amf0.ECMAArray{
		{Key: "videocodecid", Value: videoCodec}, {Key: "width", Value: float64(vi.Width)}, {Key: "height", Value: float64(vi.Height)},
		{Key: "framerate", Value: float64(vi.FrameRate.Numerator) / float64(vi.FrameRate.Denominator)},
		{Key: "audiocodecid", Value: float64(10)}, {Key: "audiosamplerate", Value: float64(ai.SampleRateHz)}, {Key: "audiochannels", Value: float64(ai.Channels)}, {Key: "stereo", Value: ai.Channels == 2},
	}}
	// b содержит сериализованные метаданные. err сообщает об отказе их проверки или кодирования.
	b, err := c.marshalAMF(metadata)
	if err != nil {
		return err
	}
	if err = c.writeFramed(5, 18, c.streamID.Load(), ts, b); err != nil {
		return err
	}
	// prefix содержит префикс сообщения видеоконфигурации H.264 либо AV1.
	prefix := []byte{0x17, 0, 0, 0, 0}
	if vi.Codec == core.CodecAV1 {
		prefix = []byte{0x90, 'a', 'v', '0', '1'}
	}
	// v содержит полное тело сообщения с исходными видеозаголовками.
	v := append(prefix, c.video.Headers()...)
	if err = c.writeFramed(6, 9, c.streamID.Load(), ts, v); err != nil {
		return err
	}
	// a содержит полное тело сообщения с исходным ASC аудиодорожки.
	a := append([]byte{0xaf, 0}, c.audio.Headers()...)
	if err = c.writeFramed(4, 8, c.streamID.Load(), ts, a); err != nil {
		return err
	}
	c.headersPending = false
	return nil
}
