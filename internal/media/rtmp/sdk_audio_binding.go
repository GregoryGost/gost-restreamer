package rtmp

import (
	"errors"
	"fmt"

	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/bluenviron/gortmplib/pkg/message"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/flac"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/opus"
)

// audioBindingSDKMessage заменяет только описание текущего завершённого SequenceStart.
// Вызывающий проверяет единственное завершение без ошибок транспорта, включая успешный разбор SDK.
// cause допускает лишь точную ошибку повторённого закреплённого парсера; иной отказ не скрывается.
// Исходные байты сохраняются в wireConn; возвращённое описание никогда не сериализуется SDK.
func (c *wireConn) audioBindingSDKMessage(cause error) (message.Message, bool, error) {
	// raw содержит полное исходное тело текущего сообщения.
	raw := c.lastConfig
	if len(raw) < 5 || raw[0] != 0x90 {
		return nil, false, nil
	}
	// selected сохраняет фактический внешний FourCC, по которому выбирается разбор заголовков.
	selected := string(raw[1:5])
	if selected != "Opus" && selected != "fLaC" {
		return nil, false, nil
	}
	if cause != nil {
		// pinnedErr воспроизводит ошибку закреплённого SDK; label задаёт её точную внешнюю обёртку.
		var pinnedErr error
		label := "invalid Opus ID header"
		if selected == "Opus" {
			// original повторяет разбор SDK только для проверки причинности отказа.
			original := opus.IDHeader{}
			pinnedErr = original.Unmarshal(raw[5:])
		} else {
			// original воспроизводит ошибочный разбор SDK без пропуска сигнатуры fLaC.
			original := flac.StreamInfo{}
			pinnedErr = original.Unmarshal(raw[5:])
			label = "invalid FLAC STREAMINFO"
		}
		// unwrapped исключает произвольную ошибку транспорта с похожим внешним сообщением.
		unwrapped := errors.Unwrap(cause)
		if pinnedErr == nil || unwrapped == nil || errors.Unwrap(unwrapped) != nil || unwrapped.Error() != pinnedErr.Error() || cause.Error() != fmt.Sprintf("%s: %v", label, pinnedErr) {
			return nil, false, nil
		}
	}
	// descriptor передаёт адресацию сообщения; publication берёт исходные заголовки из raw.
	descriptor := &message.AudioExSequenceStart{ChunkStreamID: c.lastConfigChunkStream, MessageStreamID: c.lastConfigMessageStream}
	if selected == "Opus" {
		// head содержит нормативные LE значения, err сообщает о неподдержанном или повреждённом Head.
		head, err := codec.ParseOpusHead(raw[5:])
		if err != nil {
			return nil, true, err
		}
		descriptor.FourCC = message.FourCCOpus
		descriptor.OpusConfig = &opus.IDHeader{Version: 1, ChannelCount: 2, PreSkip: head.PreSkip, InputSampleRate: head.InputSampleRate}
	} else {
		// headers содержат отдельный STREAMINFO без сигнатуры и заголовка блока метаданных.
		// err сохраняет отказ нормативной записи42байта либо неподдержанного формата PCM.
		headers, err := codec.FLACStreamInfoFromWire(raw[5:])
		if err != nil {
			return nil, true, err
		}
		descriptor.FourCC = message.FourCCFLAC
		descriptor.FlacConfig = &flac.StreamInfo{}
		if err = descriptor.FlacConfig.Unmarshal(headers); err != nil {
			return nil, true, err
		}
	}
	return descriptor, true, nil
}
