package fallback

import (
	"math/big"
	"strings"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

// preparedAudioSamples возвращает число сэмплов полной единицы собственного файлового рецепта kind.
// Неизвестный кодек даёт ноль; поддержанный AAC сохраняет прежние 1024 сэмпла.
func preparedAudioSamples(kind core.Codec) uint32 {
	switch kind {
	case core.CodecAAC:
		return 1024
	case core.CodecOpus:
		return 120
	case core.CodecFLAC:
		return 4608
	case core.CodecMP3:
		return 1152
	case core.CodecAC3:
		return 1536
	}
	return 0
}

// exactPreparedAudioDuration сравнивает duration в base с полной файловой AU format без округления.
func exactPreparedAudioDuration(duration int64, base media.TimeBase, format core.AudioFormat) bool {
	// samples содержит конечный размер файловой AU, left и right сравнивают длительности в целых числах.
	samples := preparedAudioSamples(format.Codec)
	left := new(big.Int).Mul(big.NewInt(duration), new(big.Int).SetUint64(uint64(base.Numerator)*uint64(format.SampleRateHz)))
	right := new(big.Int).SetUint64(uint64(base.Denominator) * uint64(samples))
	return samples != 0 && duration > 0 && left.Cmp(right) == 0
}

// boundAudio восстанавливает новую конечную аудиопривязку из фактических полей s и rate ffprobe.
// MP3/AC3 не имеют extradata у ffprobe; их заголовки каждой AU отдельно подтверждает demux/Inspector.
func (s probeStream) boundAudio(rate uint32) (core.AudioFormat, error) {
	if rate != 48000 || s.Channels != 2 || s.Layout != "stereo" {
		return core.AudioFormat{}, core.ErrInvalidFormat
	}
	// kind выбирает реальный codec_name; profile задаёт соответствующий конечный порт приложения.
	kind := core.Codec(s.Codec)
	profile := ""
	switch kind {
	case core.CodecOpus:
		profile = "celt"
	case core.CodecFLAC:
		profile = "s16"
	case core.CodecMP3:
		profile = "mpeg1-layer3"
	case core.CodecAC3:
		profile = "ac3"
	default:
		return core.AudioFormat{}, core.ErrInvalidFormat
	}
	if s.Profile != "" && s.Profile != profile {
		return core.AudioFormat{}, core.ErrCorrupt
	}
	// actual отражает наблюдаемые rate/layout; профиль каждого кодека подтверждается реальными заголовками.
	actual := core.AudioFormat{Codec: kind, CodecProfile: profile, SampleRateHz: rate, Channels: 2, ChannelLayout: "stereo"}
	if kind == core.CodecOpus || kind == core.CodecFLAC {
		// headers восстанавливает extradata без подстановки desired; err сообщает о неверном дампе.
		headers, err := probeExtraData(s.ExtraData, s.ExtraDataSize)
		if err != nil {
			return core.AudioFormat{}, err
		}
		// coded подтверждает канализацию и параметры кодека по реальной конфигурации.
		coded, err := codec.AudioFormatHeaders(kind, headers)
		if err != nil || coded != actual {
			return core.AudioFormat{}, core.ErrCorrupt
		}
	} else if strings.TrimSpace(s.ExtraData) != "" || s.ExtraDataSize != 0 {
		return core.AudioFormat{}, core.ErrCorrupt
	}
	return actual, actual.Validate()
}

// verifyPreparedAudio сверяет packet с самостоятельным файловым binding и ordinal его AU.
// FLAC с фиксированными блоками обязан иметь полный block4608 и исходный последовательный frame number.
// Здесь нет переписывания содержимого или назначения FLACOwned для произвольного файла.
func verifyPreparedAudio(packet media.Packet, ordinal uint64) error {
	// config задаёт реальные headers; kind выбирает конечную привязку вместо desired-подстановки.
	config := packet.Config()
	kind := config.Info().Audio.Codec
	if kind == core.CodecAAC {
		return nil
	}
	// samples и err подтверждают полный сжатый AU перед проверкой файлового quantum.
	samples, err := codec.AudioSamples(kind, config.Headers(), packet.Payload())
	if err != nil {
		return err
	}
	if samples != preparedAudioSamples(kind) {
		return core.ErrCorrupt
	}
	if kind == core.CodecFLAC {
		// frame сохраняет фактическую blocking strategy и number этого самостоятельного файла.
		frame, parseErr := codec.FLACFrameInfo(config.Headers(), packet.Payload())
		if parseErr != nil {
			return parseErr
		}
		if frame.Variable || frame.Number != ordinal {
			return core.ErrCorrupt
		}
	}
	return codec.ValidateAudioDuration(kind, config.Headers(), packet.Payload(), packet.Info(), config.Info().TimeBase)
}

// preparedHeaders проверяет исходный desc готового файла без его исправления.
// Head120 в подготовленном артефакте означает незавершённую собственную нормализацию и отклоняется.
func preparedHeaders(desc fmp4.TrackDescription) ([]byte, error) {
	if desc.Track == media.TrackAudio && desc.Codec == core.CodecOpus {
		// head содержит исходный preSkip до уточнения конфигурации; err сообщает о неверном Head.
		head, err := codec.ParseOpusHead(desc.Headers)
		if err != nil {
			return nil, err
		}
		if head.PreSkip != 0 {
			return nil, core.ErrNotReady
		}
	}
	return desc.Headers, nil
}
