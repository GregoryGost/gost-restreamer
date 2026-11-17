package rtmp

import (
	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
)

// outboundAudioFourCC возвращает точный Enhanced Audio FourCC кодека kind.
// AAC использует mp4a только для многоканального транспорта; метаданные сохраняют число10.
func outboundAudioFourCC(kind core.Codec) string {
	switch kind {
	case core.CodecAAC:
		return "mp4a"
	case core.CodecOpus:
		return "Opus"
	case core.CodecFLAC:
		return "fLaC"
	case core.CodecMP3:
		return ".mp3"
	case core.CodecAC3:
		return "ac-3"
	}
	return ""
}

// audioFramePrefix строит внешний префикс для фактического format и типа пакета kind0/1/2.
// AAC mono/stereo сохраняет прежние legacy байты; остальные формы используют однодорожечный Enhanced.
func audioFramePrefix(format core.AudioFormat, kind byte) []byte {
	if format.Codec == core.CodecAAC && format.Channels <= 2 {
		return []byte{0xaf, kind}
	}
	// fourCC содержит четырёхбайтовое транспортное обозначение проверенного кодека.
	fourCC := outboundAudioFourCC(format.Codec)
	if len(fourCC) != 4 {
		return nil
	}
	return []byte{0x90 | kind, fourCC[0], fourCC[1], fourCC[2], fourCC[3]}
}

// audioMetadataCodec возвращает числовой UI32 FourCC кодека kind либо прежнее AAC10.
// Float64 AMF number точно представляет все значения uint32.
func audioMetadataCodec(kind core.Codec) float64 {
	if kind == core.CodecAAC {
		return 10
	}
	// fourCC содержит транспортное обозначение нового проверенного кодека.
	fourCC := outboundAudioFourCC(kind)
	if len(fourCC) != 4 {
		return 0
	}
	return float64(uint32(fourCC[0])<<24 | uint32(fourCC[1])<<16 | uint32(fourCC[2])<<8 | uint32(fourCC[3]))
}

// outboundAudioSequenceBody проверяет config и готовит полное тело SequenceStart до записи.
// FLAC требует подтверждённого локального происхождения и получает STREAMINFO непрерывного потока.
// MP3/AC3 сохраняют внутренние заголовки, но отправляют пустое тело после внешнего FourCC.
func outboundAudioSequenceBody(config media.CodecConfig) ([]byte, error) {
	// info содержит описание конфигурации по значению, headers — отдельную копию заголовков кодека.
	info, headers := config.Info(), config.Headers()
	// actual содержит формат стандартных заголовков, err — отказ их разбора без желаемых настроек.
	actual, err := codec.AudioFormatHeaders(info.Audio.Codec, headers)
	if err != nil {
		return nil, err
	}
	if actual != info.Audio {
		return nil, ErrCodec
	}
	switch info.Audio.Codec {
	case core.CodecOpus:
		// head проверяет выполненную источником нормализацию; headErr сохраняет отказ разбора Head.
		// Исходящий транспорт не потребляет начальную AU повторно.
		head, headErr := codec.ParseOpusHead(headers)
		if headErr != nil {
			return nil, headErr
		}
		if head.PreSkip != 0 {
			return nil, ErrCodec
		}
	case core.CodecFLAC:
		if !info.FLACOwned {
			return nil, ErrCodec
		}
		headers, err = codec.FLACContinuousHeaders(headers)
		if err == nil {
			headers, err = codec.FLACWireHeader(headers)
		}
		if err != nil {
			return nil, err
		}
	case core.CodecMP3, core.CodecAC3:
		headers = nil
	}
	return append(audioFramePrefix(info.Audio, 0), headers...), nil
}

// prepareOutboundAudio проверяет p и готовит отдельную нагрузку перед любой записью.
// counter принадлежит физическому соединению; следующий счётчик сохраняется лишь после полной записи.
// AAC/Opus/MP3/AC3 байты не меняются; FLAC меняет только стратегию блоков, номер и обе CRC кадра.
func prepareOutboundAudio(p media.Packet, counter uint64) ([]byte, uint64, error) {
	// config и info задают фактический кодек, признак локального происхождения и точную шкалу пакета.
	config := p.Config()
	info := config.Info()
	// payload содержит отдельную копию исходной AU.
	payload := p.Payload()
	// err сохраняет отказ согласования длительности с фактическими отсчётами AU.
	if err := codec.ValidateAudioDuration(info.Audio.Codec, config.Headers(), payload, p.Info(), info.TimeBase); err != nil {
		return nil, counter, err
	}
	if info.Audio.Codec != core.CodecFLAC {
		return payload, counter, nil
	}
	if !info.FLACOwned {
		return nil, counter, ErrCodec
	}
	// frame содержит проверенное число отсчётов и границы исходного заголовка кадра.
	// err сохраняет отказ FLAC синтаксиса либо CRC.
	frame, err := codec.FLACFrameInfo(config.Headers(), payload)
	if err != nil {
		return nil, counter, err
	}
	payload, err = codec.RewriteFLACFrame(config.Headers(), payload, counter)
	if err != nil {
		return nil, counter, err
	}
	return payload, counter + uint64(frame.Samples), nil
}
