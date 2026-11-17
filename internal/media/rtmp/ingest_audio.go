package rtmp

import (
	"bytes"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/bluenviron/gortmplib/pkg/message"
)

// ingestAudioCodec переводит поддержанный однодорожечный fourCC во внутренний кодек.
// Пустой результат означает неподдержанную транспортную форму; альтернативные имена не принимаются.
func ingestAudioCodec(fourCC message.FourCC) core.Codec {
	switch fourCC {
	case message.FourCCMP4A:
		return core.CodecAAC
	case message.FourCCOpus:
		return core.CodecOpus
	case message.FourCCFLAC:
		return core.CodecFLAC
	case message.FourCCMP3:
		return core.CodecMP3
	case message.FourCCAC3:
		return core.CodecAC3
	}
	return ""
}

// audioBindingConfig начинает аудиопоследовательность fourCC по исходному сообщению wireConn.
// MP3/AC3 требуют пустого тела после FourCC, FLAC — записи42байта, Opus —19байт Head с полями LE.
// Конфигурация появляется после подтверждения первой AU; внешний источник не получает FLACOwned.
func (p *publication) audioBindingConfig(fourCC message.FourCC) error {
	// selected задаёт кодек, для которого проверяются исходные заголовки.
	selected := ingestAudioCodec(fourCC)
	if selected == "" || selected == core.CodecAAC || p.wire == nil || p.audioRevision == ^uint64(0) {
		return ErrCodec
	}
	// raw содержит одно полностью собранное SequenceStart, проверенное wireConn.
	raw := p.wire.lastConfig
	if len(raw) < 5 || raw[0] != 0x90 || message.FourCC(be24(raw[1:4])<<8|uint32(raw[4])) != fourCC {
		return ErrCodec
	}
	// headers сохраняют отдельные заголовки; skip требует потребить единственную начальную AU.
	headers := bytes.Clone(raw[5:])
	skip := false
	// err сохраняет отказ нормативного record и последующей границы источника.
	var err error
	switch selected {
	case core.CodecOpus:
		// head содержит проверенные LE поля, headErr сообщает о недопустимом Head или preSkip.
		head, headErr := codec.ParseOpusHead(headers)
		if headErr != nil {
			return headErr
		}
		skip = head.PreSkip == 120
	case core.CodecFLAC:
		headers, err = codec.FLACStreamInfoFromWire(headers)
	case core.CodecMP3, core.CodecAC3:
		if len(headers) != 0 {
			return ErrCodec
		}
	}
	if err != nil {
		return err
	}
	if err = p.invalidate(); err != nil {
		return err
	}
	p.audioRevision++
	p.audioCodec, p.audioPending = selected, headers
	p.opusSkipPending = skip
	p.asc = nil
	p.audio = media.CodecConfig{}
	p.ready = false
	return nil
}

// completeAudioBinding уточняет конфигурацию по первой au без дополнительного чтения транспорта.
// Opus Head нормализуется после проверки AU; начальное потребление выполняет audioBindingPacket.
// p хранит результат на всю аудиопоследовательность, включая смены только видеоконфигурации.
func (p *publication) completeAudioBinding(au []byte) error {
	// actual и headers содержат подтверждённый формат и отдельные непустые заголовки для медиатракта.
	// err сохраняет отказ несовместимой либо повреждённой первой AU.
	actual, headers, err := codec.CompleteAudioConfig(p.audioCodec, p.audioPending, au)
	if err != nil {
		return err
	}
	if p.audioCodec == core.CodecOpus {
		headers, err = codec.NormalizeOpusHead(headers)
		if err != nil {
			return err
		}
	}
	// config связывает actual с текущим Source; err сообщает об отказе создания конфигурации.
	// Точную общую шкалу установит видеостарт.
	config, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackAudio, SourceEpoch: p.source.Epoch(), Revision: p.audioRevision, TimeBase: media.TimeBase{Numerator: 1, Denominator: actual.SampleRateHz}, Audio: actual}, headers)
	if err != nil {
		return err
	}
	p.audio, p.asc = config, headers
	p.audioPending = nil
	return nil
}

// audioBindingPacket проверяет ровно одну au и её исходную миллисекундную метку t.
// Начальное исключение Opus обрабатывается до ожидания видео даже при положительном начале времени.
// Передаваемые AU сохраняют исходные байты/PTS/DTS и получают точную длительность в общей шкале.
func (p *publication) audioBindingPacket(t time.Duration, au []byte) error {
	// err сообщает о неверной либо уменьшившейся сетевой метке текущей дорожки.
	if err := p.timestamp(t, false); err != nil {
		return err
	}
	if len(p.asc) == 0 {
		// err сохраняет отказ уточнения конфигурации по уже прочитанной AU.
		if err := p.completeAudioBinding(au); err != nil {
			return err
		}
	}
	// samples задаёт число отсчётов декодера на канал; err сообщает об отказе проверки AU.
	samples, err := codec.AudioSamples(p.audioCodec, p.asc, au)
	if err != nil {
		return err
	}
	p.router.ObserveIngress(p.publicationID, media.TrackAudio, uint64(len(au)), core.VideoFormat{}, p.audio.Info().Audio)
	if p.opusSkipPending {
		p.opusSkipPending = false
		return nil
	}
	if !p.ready {
		return nil
	}
	// ticks сохраняет абсолютную RTMP метку; duration выражает число отсчётов без округления к мс.
	ticks := int64(t/time.Millisecond) * int64(p.clock/1000)
	duration := int64(samples) * int64(p.clock/p.audio.Info().Audio.SampleRateHz)
	// packet содержит неизменённую AU и её точные метки; packetErr сохраняет отказ структуры.
	packet, packetErr := media.NewPacket(media.PacketInfo{PTS: ticks, DTS: ticks, DurationTicks: duration}, p.audio, au)
	if packetErr != nil {
		return packetErr
	}
	return p.source.Publish(packet)
}
