package rtmp

import (
	"errors"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/bluenviron/gortmplib/pkg/message"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/mpeg4audio"
)

// readMessage вызывает существующий SDK reader с прежними ACK, буфером и счётчиками.
// Дополнение восстанавливает descriptor нормативных AAC config12, VP9 record без FullBox,
// Opus LE и FLAC record. Новая полностью выданная конфигурация должна завершиться именно
// в этом вызове, без причинной ошибки Read/Write; прежний или заранее захваченный ASC
// не используется. read должен принадлежать SDK, работающему поверх этого wireConn.
// Ограничитель выдаёт один chunk за чтение; SDK v1.0.1 возвращает первое завершённое сообщение.
// Поэтому новая конфигурация с ровно одним завершением и пустым pending относится текущему Read;
// несколько завершений или завершение до начала вызова означают неоднозначность и не обходятся.
func (c *wireConn) readMessage(read func() (message.Message, error)) (message.Message, error) {
	// before фиксирует завершения до SDK Read; failure фиксирует ошибки транспорта до этого вызова.
	before, failure := c.completedID, c.failureID.Load()
	// msg и err сохраняют фактический результат единственного вызова существующего SDK.
	msg, err := read()
	if c.failureID.Load() != failure || before == ^uint64(0) || c.completedID != before+1 || c.lastConfigID != c.completedID || len(c.pending) != 0 {
		// start и audioStart распознают успешный SDK descriptor, который нельзя использовать
		// без подтверждённого текущего raw: SDK неверно читает числовые поля Opus даже без ошибки.
		start, audioStart := msg.(*message.AudioExSequenceStart)
		if err == nil && audioStart && (start.FourCC == message.FourCCOpus || start.FourCC == message.FourCCFLAC) {
			return nil, ErrFraming
		}
		return msg, err
	}
	// adapted и matched относятся только к текущему raw Opus/FLAC, bindingErr сохраняет
	// ошибку нормативного record. SDK может успешно разобрать Opus с неверным endian.
	if adapted, matched, bindingErr := c.audioBindingSDKMessage(err); matched {
		return adapted, bindingErr
	}
	if err == nil {
		return msg, nil
	}
	// video и adapted содержат локальный descriptor нормативного VP9 record, если отказ
	// вызван только несовпадением record и FullBox в закреплённом SDK.
	if video, adapted := c.vp9SDKMessage(err); adapted {
		return video, nil
	}
	// raw содержит полное исходное сообщение, offset выбирает его проверенную аудиоформу.
	raw := c.lastConfig
	offset := 0
	// expected содержит точный внешний текст ошибки ASC-unmarshal выбранной pinned формы.
	expected := ""
	if len(raw) > 2 && raw[0]>>4 == message.CodecMPEG4Audio && raw[1] == 0 {
		offset = 2
		expected = "unable to parse MPEG4 audio config: unsupported channel configuration (12)"
	} else if len(raw) > 5 && raw[0] == 0x90 && string(raw[1:5]) == "mp4a" {
		offset = 5
		expected = "invalid MPEG-4 audio config: unsupported channel configuration (12)"
	}
	// cause проверяет именно одноуровневую обёртку ошибки pinned ASC parser.
	cause := errors.Unwrap(err)
	if offset == 0 || err.Error() != expected || cause == nil || cause.Error() != "unsupported channel configuration (12)" || errors.Unwrap(cause) != nil {
		return msg, err
	}
	// actual подтверждает полный AAC-LC ASC общим bounded parser, включая GASpecificConfig и хвост.
	// parseErr сохраняет отказ общей проверки, который не заменяет исходную ошибку SDK.
	actual, parseErr := codec.AudioFormatASC(raw[offset:])
	if parseErr != nil || actual.ChannelLayout != "7.1" || actual.Channels != 8 {
		return msg, err
	}
	// pinned повторяет проверку на тех же исходных байтах, исключая другую причину unmarshal.
	pinned := mpeg4audio.AudioSpecificConfig{}
	// pinnedErr подтверждает точный неподдержанный номер конфигурации в используемой версии SDK.
	pinnedErr := pinned.Unmarshal(raw[offset:])
	if pinnedErr == nil || pinnedErr.Error() != cause.Error() || pinned.ChannelConfig != 12 || pinned.Type != mpeg4audio.ObjectTypeAACLC || pinned.SampleRate != int(actual.SampleRateHz) {
		return msg, err
	}
	// descriptor передаёт реальные Type/Rate/config12/Count8 только внутри SDK; Marshal не вызывается,
	// поскольку pinned Marshal преобразовал бы восемь каналов обратно в configuration7.
	descriptor := &mpeg4audio.AudioSpecificConfig{Type: mpeg4audio.ObjectTypeAACLC, SampleRate: int(actual.SampleRateHz), ChannelConfig: 12}
	descriptor.ChannelCount = 8 //nolint:staticcheck // SA1019: pinned SDK сохраняет это поле для совместимости descriptor; Marshal здесь запрещён.
	if offset == 5 {
		return &message.AudioExSequenceStart{ChunkStreamID: c.lastConfigChunkStream, MessageStreamID: c.lastConfigMessageStream, FourCC: message.FourCCMP4A, AACConfig: descriptor}, nil
	}
	return &message.Audio{ChunkStreamID: c.lastConfigChunkStream, MessageStreamID: c.lastConfigMessageStream, DTS: time.Duration(c.lastConfigTimestamp) * time.Millisecond, Codec: message.CodecMPEG4Audio, Rate: message.AudioRate(raw[0] >> 2 & 3), Depth: message.AudioDepth(raw[0] >> 1 & 1), IsStereo: raw[0]&1 != 0, AACType: message.AudioAACTypeConfig, AACConfig: descriptor}, nil
}
