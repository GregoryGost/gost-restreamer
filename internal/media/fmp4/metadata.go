package fmp4

import (
	"bytes"
	"errors"
	"fmt"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
)

// ResolveCodecConfig восстанавливает прогрессивный H.264, AAC-LC с точной картой ASC/PCE
// и начальные Opus/FLAC/MP3/AC3 из полных фактических заголовков. Demuxer дополнительно проверяет первую AU.
// FPS берётся из TimingInfo в VUI, цвет без сигнализации остаётся unspecified.
// Для AV1 без подтверждённых фактических размеров кадров нужен ResolveConfig.
// desc содержит фактическую запись контейнера, epoch и revision задают идентичность результата.
func ResolveCodecConfig(desc TrackDescription, epoch, revision uint64) (media.CodecConfig, error) {
	// err сообщает о недопустимой рациональной шкале прочитанной дорожки.
	if err := desc.TimeBase.Validate(); err != nil {
		return media.CodecConfig{}, err
	}
	// info накапливает восстановленный формат дорожки с переданными эпохой и ревизией конфигурации.
	info := media.CodecConfigInfo{Track: desc.Track, SourceEpoch: epoch, Revision: revision, TimeBase: desc.TimeBase}
	// err сохраняет отказ восстановления формата из заголовков либо отсутствие подходящего
	// восстановителя кодека.
	var err error
	switch desc.Codec {
	case core.CodecH264:
		info.Video, err = videoFromAVCC(desc)
	case core.CodecHEVC:
		if desc.ActualVideo.Validate() == nil {
			info.Video = desc.ActualVideo
		} else {
			info.Video, err = codec.HEVCFormat(codec.VideoDescription{Headers: desc.Headers, Width: desc.Width, Height: desc.Height})
		}
		if err != nil {
			err = translateCodecError(err)
		}
	case core.CodecVP9:
		if desc.ActualVideo.Validate() != nil {
			err = ErrMetadataUnavailable
		} else {
			info.Video = desc.ActualVideo
		}
	case core.CodecAAC:
		info.Audio, err = audioFromASC(desc.Headers)
	case core.CodecOpus, core.CodecFLAC, core.CodecMP3, core.CodecAC3:
		info.Audio, err = codec.AudioFormatHeaders(desc.Codec, desc.Headers)
		if err != nil {
			err = translateCodecError(err)
		}
	default:
		err = fmt.Errorf("%w: описание %s требует codec resolver", ErrMetadataUnavailable, desc.Codec)
	}
	if err != nil {
		return media.CodecConfig{}, err
	}
	if newAudioCodec(desc.Codec) && (desc.Track != media.TrackAudio || desc.SampleRateHz != info.Audio.SampleRateHz || desc.Channels != info.Audio.Channels) {
		return media.CodecConfig{}, fmt.Errorf("%w: запись аудио противоречит конфигурации кодека", ErrInvalid)
	}
	if desc.Codec == core.CodecAAC && (desc.Track != media.TrackAudio || desc.SampleRateHz != info.Audio.SampleRateHz || desc.Channels != info.Audio.Channels) {
		return media.CodecConfig{}, fmt.Errorf("%w: sample entry противоречит ASC", ErrInvalid)
	}
	if desc.Codec == core.CodecH264 && desc.Track != media.TrackVideo {
		return media.CodecConfig{}, ErrInvalid
	}
	// config сохраняет подтверждённый формат; err сообщает об отказе структурной проверки.
	config, err := media.NewCodecConfig(info, desc.Headers)
	if err != nil {
		return media.CodecConfig{}, err
	}
	config, err = reconstructHDR(config, desc)
	if err != nil {
		return media.CodecConfig{}, err
	}
	if err = validateWireConfig(config, desc); err != nil {
		return media.CodecConfig{}, err
	}
	return config, nil
}

// Сверяет конфигурацию с фактическими заголовками и полями записи MP4 и требует точного отображения
// одного отсчёта в шкалу дорожки. c содержит проверяемую конфигурацию, desc — исходную запись MP4.
func validateWireConfig(c media.CodecConfig, desc TrackDescription) error {
	// info содержит полный формат, который сопоставляется с фактическими полями и заголовками записи
	// MP4.
	info := c.Info()
	// err сообщает о недопустимости переданной полной конфигурации дорожки.
	if err := c.Validate(); err != nil {
		return err
	}
	if info.Track != desc.Track || !sameHeaders(c, desc.Headers) {
		return ErrInvalid
	}
	// err сообщает, что единица шкалы конфигурации не отображается точно в шкалу дорожки MP4.
	if _, err := rescale(1, info.TimeBase, desc.TimeBase); err != nil {
		return err
	}
	if info.Track == media.TrackVideo {
		if info.Video.Codec != desc.Codec || (desc.Codec != core.CodecVP9 && (info.Video.Width != desc.Width || info.Video.Height != desc.Height)) || desc.Codec == core.CodecVP9 && (info.Video.Width > desc.Width || info.Video.Height > desc.Height) {
			return ErrInvalid
		}
		if desc.Codec == core.CodecHEVC && len(desc.FirstSample) > 0 {
			// actual независимо перепроверяет измерение восстановителя по исходной записи и первой единице доступа.
			// measureErr сообщает об отказе независимой проверки фактических сведений первого AU.
			actual, measureErr := codec.CompleteVideoFormat(desc.Codec, codec.VideoDescription{Headers: desc.Headers, Width: desc.Width, Height: desc.Height}, desc.FirstSample, desc.FirstSampleInfo, desc.TimeBase)
			if measureErr != nil {
				return translateCodecError(measureErr)
			}
			if measureErr = codec.CompatibleVideoConstraints(actual, info.Video, info.HDR.ColorConfig); measureErr != nil {
				return measureErr
			}
		}
		// err сообщает о любом расхождении полного описания с исходным SPS либо sequence.
		if err := validateVideoConfig(c); err != nil {
			return err
		}
		if desc.Color != nil {
			// color содержит nclx, построенный из проверенного формата; err сообщает об
			// отсутствии точного числового представления цветовых полей.
			color, err := marshalColor(info.Video.Color)
			if err != nil || !bytes.Equal(color, desc.Color) {
				return fmt.Errorf("%w: colr расходится с заголовками кодека", ErrInvalid)
			}
		}
	} else {
		if newAudioCodec(desc.Codec) {
			return validateAudioWireConfig(c, desc)
		}
		// actual содержит формат AAC, восстановленный непосредственно из ASC; err сообщает об
		// отказе его разбора.
		actual, err := audioFromASC(desc.Headers)
		if err != nil {
			return err
		}
		if info.Audio.SampleRateHz != desc.SampleRateHz || info.Audio.Channels != desc.Channels || info.Audio != actual {
			return fmt.Errorf("%w: описание audio противоречит ASC", ErrInvalid)
		}
	}
	return nil
}

// validateVideoConfig сверяет исходные headers конфигурации с effective actual и его внешними CICP.
// Нулевое происхождение сохраняет строгое сравнение, включая прежний SDR.
func validateVideoConfig(config media.CodecConfig) error {
	return translateCodecResult(codec.ValidateVideoConfig(config))
}

// Восстанавливает формат H.264 из avcC через общий кодек с переводом категорий ошибок в fMP4.
func videoFromAVCC(d TrackDescription) (core.VideoFormat, error) {
	// v содержит восстановленный видеоформат H.264; e сохраняет отказ разбора или нехватку сведений
	// в avcC.
	v, e := codec.H264Format(codec.VideoDescription{Headers: d.Headers, Width: d.Width, Height: d.Height})
	if e != nil {
		return v, translateCodecError(e)
	}
	return v, nil
}

// Восстанавливает формат AAC-LC из ASC через общий кодек с переводом категорий ошибок в fMP4.
func audioFromASC(b []byte) (core.AudioFormat, error) {
	// a содержит восстановленный формат AAC-LC; e сохраняет отказ разбора AudioSpecificConfig.
	a, e := codec.AudioFormatASC(b)
	if e != nil {
		return a, translateCodecError(e)
	}
	return a, nil
}

// Сохраняет исходную ошибку кодека в обёртке fMP4: нехватку сведений относит к ErrMetadataUnavailable,
// время — к ErrUnsupportedTiming, остальные ошибки — к ErrInvalid.
func translateCodecError(e error) error {
	// target выбирает категорию fMP4 для исходной ошибки кодека, сохраняя её в цепочке обёрток.
	target := ErrInvalid
	if errors.Is(e, codec.ErrUnsupported) {
		target = ErrUnsupportedCodec
	}
	if errors.Is(e, codec.ErrUnready) {
		target = ErrMetadataUnavailable
	}
	if errors.Is(e, codec.ErrTiming) {
		target = ErrUnsupportedTiming
	}
	return fmt.Errorf("%w: %w", target, e)
}

// translateCodecResult сохраняет nil при успехе и переводит только фактический отказ кодека.
func translateCodecResult(err error) error {
	if err == nil {
		return nil
	}
	return translateCodecError(err)
}
