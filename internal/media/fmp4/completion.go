package fmp4

import (
	"bytes"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
)

// bufferedSample хранит один прочитанный при инициализации AU и его исходные временные сведения.
type bufferedSample struct {
	// sample содержит исходную дорожку и точные времена, payload — неизменённые сжатые байты.
	sample  sample
	payload []byte
}

// completeActual уточняет только новые неполные видео- и аудиоконфигурации по ограниченному префиксу AU.
// Префикс хранит исходные байты и порядок; удаляется только доказанная целиком пропускаемая Opus AU.
func (d *Demuxer) completeActual() error {
	// remaining считает незавершённые дорожки; прежние полностью известные AAC и видео не требуют предварительного чтения.
	remaining := 0
	// track обозначает очередную дорожку с недостаточными фактическими сведениями.
	for _, track := range d.tracks {
		if track.incomplete {
			remaining++
		}
	}
	if remaining == 0 {
		return nil
	}
	// total ограничивает все прочитанные байты, count — AU, включая единственную удалённую Opus AU.
	total, count := uint64(0), uint64(0)
	for remaining > 0 {
		if count >= uint64(d.options.Limits.MaxSamples) {
			return ErrLimit
		}
		// item хранит описание и исходные метки AU, payload — её байты; err сообщает об отказе до публикации Init.
		item, payload, err := d.readSample()
		if err != nil {
			return err
		}
		if uint64(len(payload)) > d.options.Limits.MaxInitBytes-total {
			return ErrLimit
		}
		total += uint64(len(payload))
		count++
		// drop разрешает пропуск только единственной целой начальной Opus AU, независимо от ожидания готовности видео.
		drop := false
		if item.track.incomplete {
			if item.track.description.Track == media.TrackAudio {
				drop, err = d.completeAudioActual(&item, payload)
			} else {
				err = d.completeVideoActual(item.track, item, payload)
			}
			if err != nil {
				return err
			}
			if !item.track.incomplete {
				remaining--
			}
		}
		if !drop {
			// item сохраняет точное время в окончательной шкале завершённой дорожки.
			d.prefix = append(d.prefix, bufferedSample{sample: item, payload: payload})
		}
	}
	if d.init.Video.Info().SourceEpoch != d.init.Audio.Info().SourceEpoch {
		return ErrInvalid
	}
	return nil
}

// completeVideoActual завершает pending по прочитанным item/payload без перестановки префикса.
// Восстановитель получает копии фактических заголовков и первого независимого AU.
func (d *Demuxer) completeVideoActual(pending *trackState, item sample, payload []byte) error {
	// desc даёт восстановителю отдельные копии заголовков, цвета и фактической нагрузки с измеренным форматом.
	desc := pending.description
	desc.Headers = bytes.Clone(desc.Headers)
	desc.Color = bytes.Clone(desc.Color)
	desc.FirstSample = bytes.Clone(payload)
	desc.FirstSampleInfo = item.info
	// actual и err подтверждают измеренный формат первого независимого AU.
	actual, err := codec.CompleteVideoFormat(desc.Codec, codec.VideoDescription{Headers: desc.Headers, Width: desc.Width, Height: desc.Height}, payload, item.info, desc.TimeBase)
	if err != nil {
		return translateCodecError(err)
	}
	desc.ActualVideo = actual
	// config содержит полную фактическую конфигурацию повторного восстановителя без подстановки желаемых значений.
	var config media.CodecConfig
	if d.options.ResolveConfig != nil {
		config, err = d.options.ResolveConfig(desc)
	} else {
		config, err = ResolveCodecConfig(desc, d.options.SourceEpoch, d.options.VideoRevision)
	}
	if err != nil {
		return err
	}
	config, err = reconstructHDR(config, desc)
	if err != nil {
		return err
	}
	// info получает полные фактические геометрию и FPS, даже если пользовательский восстановитель их проигнорировал.
	info := config.Info()
	if err = codec.CompatibleVideoConstraints(desc.ActualVideo, info.Video, info.HDR.ColorConfig); err != nil {
		return err
	}
	if info.TimeBase != desc.TimeBase {
		return ErrUnsupportedTiming
	}
	applyIdentity(&info, d.options)
	config, err = media.NewCodecConfig(info, desc.Headers)
	if err != nil {
		return err
	}
	config, err = reconstructHDR(config, desc)
	if err != nil {
		return err
	}
	if err = validateWireConfig(config, desc); err != nil {
		return err
	}
	pending.incomplete = false
	pending.config = config
	pending.description.ActualVideo = desc.ActualVideo
	d.init.Video = config
	return nil
}

// next выдаёт удержанный префикс перед дальнейшим чтением без потери AU или перестановки времени.
// Новые пути кодеков проверяют фактические геометрию, CFR и запрет наборов параметров внутри hvc1 перед выдачей.
func (d *Demuxer) next() (media.Packet, error) {
	// item и payload получают удержанную либо следующую единицу доступа контейнера; err сохраняет отказ чтения и проверки.
	var item sample
	var payload []byte
	var err error
	if len(d.prefix) > 0 {
		item, payload = d.prefix[0].sample, d.prefix[0].payload
		d.prefix[0] = bufferedSample{}
		d.prefix = d.prefix[1:]
	} else {
		item, payload, err = d.readSample()
		if err != nil {
			return media.Packet{}, err
		}
	}
	if item.track.description.Track == media.TrackAudio {
		if err = validateAudioSample(item.track.description.Codec, item.track.config.Headers(), payload, item.info, item.track.config.Info().TimeBase, d.options.FreshSilentOpus, d.options.FreshSilentMP3); err != nil {
			return media.Packet{}, err
		}
	}
	if item.track.description.SampleEntry == "hvc1" {
		if err = codec.ValidateHVC1AU(payload); err != nil {
			return media.Packet{}, translateCodecError(err)
		}
	}
	// packet сохраняет исходные байты и время с окончательной проверенной конфигурацией.
	packet, err := media.NewPacket(item.info, item.track.config, payload)
	if err != nil {
		return packet, err
	}
	if item.track.description.Codec == core.CodecVP9 || item.track.description.Codec == core.CodecHEVC {
		if _, err = item.track.inspector.Inspect(packet); err != nil {
			return media.Packet{}, translateCodecError(err)
		}
	}
	return packet, nil
}
