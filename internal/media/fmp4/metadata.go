package fmp4

import (
	"errors"
	"fmt"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/h264"
)

// ResolveCodecConfig восстанавливает описание прогрессивного H.264 и AAC-LC mono/stereo.
// FPS берётся из TimingInfo в VUI, цвет без сигнализации остаётся unspecified.
// Для AV1 без подтверждённых фактических размеров кадров нужен ResolveConfig.
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
	case core.CodecAAC:
		info.Audio, err = audioFromASC(desc.Headers)
	default:
		err = fmt.Errorf("%w: описание %s требует codec resolver", ErrMetadataUnavailable, desc.Codec)
	}
	if err != nil {
		return media.CodecConfig{}, err
	}
	if desc.Codec == core.CodecAAC && (desc.Track != media.TrackAudio || desc.SampleRateHz != info.Audio.SampleRateHz || desc.Channels != info.Audio.Channels) {
		return media.CodecConfig{}, fmt.Errorf("%w: sample entry противоречит ASC", ErrInvalid)
	}
	if desc.Codec == core.CodecH264 && desc.Track != media.TrackVideo {
		return media.CodecConfig{}, ErrInvalid
	}
	return media.NewCodecConfig(info, desc.Headers)
}

// Сверяет конфигурацию с фактическими заголовками и полями записи MP4 и требует точного отображения
// одного отсчёта в шкалу дорожки.
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
		if info.Video.Codec != desc.Codec || info.Video.Width != desc.Width || info.Video.Height != desc.Height {
			return ErrInvalid
		}
		if desc.Codec == core.CodecH264 {
			// err сообщает о некорректном SPS или несовпадении размеров H.264 с записью MP4.
			if _, err := spsFromAVCC(desc); err != nil {
				return err
			}
		}
	} else {
		// actual содержит формат AAC, восстановленный непосредственно из ASC; err сообщает об
		// отказе его разбора.
		actual, err := audioFromASC(desc.Headers)
		if err != nil {
			return err
		}
		if info.Audio.SampleRateHz != desc.SampleRateHz || info.Audio.Channels != desc.Channels || info.Audio.SampleRateHz != actual.SampleRateHz || info.Audio.Channels != actual.Channels {
			return fmt.Errorf("%w: описание audio противоречит ASC", ErrInvalid)
		}
	}
	return nil
}

// Разбирает SPS и сверяет размеры через общий кодек, переводя его ошибки в категории fMP4.
func spsFromAVCC(d TrackDescription) (h264.SPS, error) {
	// s содержит разобранный SPS H.264; e сохраняет исходную ошибку общего кодека для перевода в
	// категорию fMP4.
	s, e := codec.H264SPS(codec.VideoDescription{Headers: d.Headers, Width: d.Width, Height: d.Height})
	if e != nil {
		return s, translateCodecError(e)
	}
	return s, nil
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
	if errors.Is(e, codec.ErrUnready) {
		target = ErrMetadataUnavailable
	}
	if errors.Is(e, codec.ErrTiming) {
		target = ErrUnsupportedTiming
	}
	return fmt.Errorf("%w: %w", target, e)
}
