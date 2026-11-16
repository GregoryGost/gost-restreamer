package codec

import (
	"bytes"
	"fmt"
	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
)

// CompatibleActual сверяет два описания фактического потока без каталога настроек кодировщика.
// Отсутствующие ограничения level/tier в declared сохраняют прежнюю семантику сравнения.
func CompatibleActual(actual, declared core.MediaFormat) error {
	// err сообщает о структурном расхождении заголовков и заявленного описания.
	if err := core.CompatibleActualFormat(actual, declared); err != nil {
		return fmt.Errorf("%w: %w", ErrCodec, err)
	}
	return nil
}

// CompatibleVideoActual сравнивает только видео actual и declared, используя одинаковую
// корректную аудиодорожку для структурного контракта обеих обязательных дорожек.
func CompatibleVideoActual(actual, declared core.VideoFormat) error {
	// audio задаёт нейтральную для сравнения видео, неизменную AAC-LC дорожку.
	audio := core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", SampleRateHz: 48000, Channels: 2, ChannelLayout: "stereo"}
	return CompatibleActual(core.MediaFormat{Video: actual, Audio: audio}, core.MediaFormat{Video: declared, Audio: audio})
}

// Inspector сохраняет доказанные сведения AV1 между TU одной конфигурации.
// Экземпляр принадлежит одному сериализованному источнику.
type Inspector struct {
	// video сохраняет конфигурацию, для которой разобрано текущее состояние AV1.
	video media.CodecConfig
	// av1 хранит параметры последовательности и подтверждённые сведения, сохраняемые между TU одной
	// конфигурации.
	av1 AV1Description
	// vp9 сохраняет геометрию опорных картинок между кадрами одной записи.
	vp9 VP9Description
	// hevc сохраняет разрешённые in-band PPS для неполного hev1 массива.
	hevc HEVCDescription
	// hdr сохраняет статические сведения кодека отдельно от внешнего переопределения Info.HDR.
	hdr HDRState
	// metadataConfig определяет конфигурацию действия метаданных независимо от разборщика видео.
	metadataConfig media.CodecConfig
}

// Inspect сверяет фактические заголовки и AU с конфигурацией пакета и возвращает независимость видео
// по содержимому. Для аудио проверяет фактические заголовки и AU, возвращая false; новые кодеки требуют
// точную положительную длительность. packet содержит проверяемую единицу доступа; v хранит
// состояние AV1/VP9/HEVC между пакетами одной конфигурации.
func (v *Inspector) Inspect(packet media.Packet) (bool, error) {
	// err сообщает о некорректной структуре проверяемого пакета.
	if err := packet.Validate(); err != nil {
		return false, err
	}
	// c содержит неизменяемую конфигурацию дорожки проверяемого пакета.
	c := packet.Config()
	// info содержит описание дорожки, с которым сравниваются фактические заголовки и данные.
	info := c.Info()
	if info.Track == media.TrackAudio {
		// actual содержит формат аудио из заголовка кодека; err сообщает об отказе его разбора.
		actual, err := AudioFormatHeaders(info.Audio.Codec, c.Headers())
		if err != nil {
			return false, err
		}
		if actual != info.Audio {
			return false, ErrCodec
		}
		if _, err = AudioSamples(info.Audio.Codec, c.Headers(), packet.Payload()); err != nil {
			return false, err
		}
		if err = ValidateAudioDuration(info.Audio.Codec, c.Headers(), packet.Payload(), packet.Info(), info.TimeBase); err != nil {
			return false, err
		}
		return false, nil
	}
	// actual принимает подтверждённый формат видео после разбора кодека.
	var actual core.VideoFormat
	// key принимает признак независимого видеокадра, вычисленный по сжатым данным.
	var key bool
	// err принимает ошибку разбора видео, проверки единицы доступа или совместимости формата.
	var err error
	switch info.Video.Codec {
	case core.CodecAV1:
		if v.video.Info() != info || !bytes.Equal(v.video.Headers(), c.Headers()) {
			v.av1, err = ParseAV1Config(c.Headers())
			if err != nil {
				return false, err
			}
			v.video = c
		}
		actual, key, err = v.av1.Inspect(packet.Payload(), info.Video.FrameRate)
	case core.CodecVP9:
		if v.video.Info() != info || !bytes.Equal(v.video.Headers(), c.Headers()) {
			v.vp9, err = ParseVP9Config(c.Headers())
			if err != nil {
				return false, err
			}
			if err = validateVP9Config(c.Headers(), info.Video, info.HDR.ColorConfig); err != nil {
				return false, err
			}
			// Сам кадр VP9 ограничивает coarse colorspace даже тогда, когда record его не уточнил.
			// Проверенные effective CICP участвуют в этой проверке; raw record остаётся неизменным.
			v.vp9.Color.Primaries = info.Video.Color.Primaries
			v.vp9.Color.Transfer = info.Video.Color.Transfer
			v.vp9.Color.Matrix = info.Video.Color.Matrix
			v.video = c
		}
		actual, key, err = v.vp9.Inspect(packet.Payload(), info.Video.FrameRate)
	case core.CodecHEVC:
		// desc содержит hvcC и заявленную геометрию для сверки SPS.
		desc := VideoDescription{Headers: c.Headers(), Width: info.Video.Width, Height: info.Video.Height}
		if v.video.Info() != info || !bytes.Equal(v.video.Headers(), c.Headers()) {
			v.hevc, err = hevcContext(desc.Headers, packet.Payload())
			if err != nil {
				return false, err
			}
			v.video = c
		}
		actual, err = v.hevc.videoFormat(desc, info.Video.FrameRate)
		if err == nil {
			key, err = v.hevc.Inspect(packet.Payload())
		}
	case core.CodecH264:
		// desc объединяет заголовки H.264 и заявленные размеры для их сверки с SPS.
		desc := VideoDescription{Headers: c.Headers(), Width: info.Video.Width, Height: info.Video.Height}
		actual, err = H264Format(desc)
		if err == nil {
			key, err = InspectH264AU(desc, packet.Payload())
		}
	default:
		return false, ErrCodec
	}
	if err != nil {
		return false, err
	}
	if (info.Video.Codec == core.CodecVP9 || info.Video.Codec == core.CodecHEVC) && packet.Info().DurationTicks > 0 {
		// measured извлекается из duration/timebase самого AU и не использует desired.
		// measureErr сообщает о непредставимой точной частоте текущего AU.
		measured, measureErr := FrameRateFromDuration(packet.Info().DurationTicks, info.TimeBase)
		if measureErr != nil {
			return false, measureErr
		}
		if !measured.Equal(info.Video.FrameRate) {
			return false, ErrTiming
		}
	}
	if err = CompatibleVideoConstraints(actual, info.Video, info.HDR.ColorConfig); err != nil {
		return false, err
	}
	return key, nil
}

// InspectMetadata возвращает метаданные кодека после проверки AU; внешний Info.HDR не принимается
// за доказательство сведений сжатого потока. Старые SDR SEI/metadata OBU остаются непрозрачными.
func (v *Inspector) InspectMetadata(packet media.Packet) (bool, media.HDRMetadata, error) {
	// key и err содержат результат проверки действительного независимого старта и формата сжатого потока.
	key, err := v.Inspect(packet)
	if err != nil {
		return false, media.HDRMetadata{}, err
	}
	// config содержит конфигурацию, к которой относятся правила действия метаданных кодека.
	config := packet.Config()
	// info содержит характеристику transfer и кодек для выбора семантической ветви HDR.
	info := config.Info()
	if info.Track != media.TrackVideo {
		return key, media.HDRMetadata{}, nil
	}
	if info.Video.Codec != core.CodecHEVC && info.Video.Color.Transfer != "smpte2084" && info.Video.Color.Transfer != "arib-std-b67" && info.HDR == (media.HDRMetadata{}) {
		return key, media.HDRMetadata{}, nil
	}
	if v.metadataConfig.Info() != info || !bytes.Equal(v.metadataConfig.Headers(), config.Headers()) {
		v.hdr.Reset()
		v.metadataConfig = config
	}
	// metadata содержит только метаданные кодека; внешнее переопределение выбирается владельцем публикации.
	metadata, err := v.hdr.Observe(info.Video.Codec, config.Headers(), packet.Payload(), key)
	return key, metadata, err
}
