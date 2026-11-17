package fmp4

import (
	"bytes"
	"fmt"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
)

// newAudioCodec определяет kind, для которого обязательны проверка фактической AU и её длительности.
// Старый AAC не включён: его прежний контейнерный контракт остаётся самостоятельным.
func newAudioCodec(kind core.Codec) bool {
	switch kind {
	case core.CodecOpus, core.CodecFLAC, core.CodecMP3, core.CodecAC3:
		return true
	}
	return false
}

// marshalAudioEntry строит стандартную запись нового аудиокодека из c в пределах l.
// Внутренний дескриптор MP3 не попадает в DSI контейнера; OpusHead преобразуется в структуру dOps.
func marshalAudioEntry(c media.CodecConfig, l Limits) ([]byte, error) {
	// info задаёт фактический формат, headers содержит отдельную копию записи кодека.
	info, headers := c.Info(), c.Headers()
	// actual содержит формат из записи кодека, err сообщает об ошибке разбора; желаемые параметры не подставляются.
	actual, err := codec.AudioFormatHeaders(info.Audio.Codec, headers)
	if err != nil {
		return nil, translateCodecError(err)
	}
	if actual != info.Audio {
		return nil, fmt.Errorf("%w: запись аудиокодека расходится с форматом", ErrInvalid)
	}
	if uint64(len(headers)) > l.MaxExtradataBytes {
		return nil, ErrLimit
	}
	// entry задаёт AudioSampleEntry версии 0 с фактическими частотой и числом каналов, а также PCM16 для FLAC.
	entry := make([]byte, 28)
	be.PutUint16(entry[6:8], 1)
	be.PutUint16(entry[16:18], actual.Channels)
	be.PutUint16(entry[18:20], 16)
	be.PutUint32(entry[24:28], actual.SampleRateHz<<16)
	switch actual.Codec {
	case core.CodecOpus:
		// record содержит dOps с собственной версией и сетевым порядком байтов; convertErr сообщает об отказе преобразования LE OpusHead.
		record, convertErr := codec.OpusHeadToDOps(headers)
		if convertErr != nil {
			return nil, translateCodecError(convertErr)
		}
		// head хранит начальное исключение декодера для запрета его повторного применения; parseErr сообщает о неверном OpusHead.
		head, parseErr := codec.ParseOpusHead(headers)
		if parseErr != nil {
			return nil, translateCodecError(parseErr)
		}
		if head.PreSkip != 0 {
			return nil, ErrUnsupportedCodec
		}
		return box("Opus", entry, box("dOps", record)), nil
	case core.CodecFLAC:
		return box("fLaC", entry, full("dfLa", 0, 0, []byte{0x80, 0, 0, 34}, headers)), nil
	case core.CodecMP3:
		// es сигнализирует MPEG-1 LayerIII через OTI0x6B без AAC DecoderSpecificInfo.
		es := descriptor(3, join([]byte{0, 2, 0}, descriptor(4, join([]byte{0x6b, 0x15, 0, 0, 0}, make([]byte, 8))), descriptor(6, []byte{2})))
		return box("mp4a", entry, full("esds", 0, 0, es)), nil
	case core.CodecAC3:
		return box("ac-3", entry, box("dac3", headers)), nil
	}
	return nil, ErrUnsupportedCodec
}

// parseAudioEntry восстанавливает desc из единственной стандартной entry в пределах limits.
// Отсутствующий дескриптор MP3 восстанавливается по первой фактической AU; остальные headers копируются.
func parseAudioEntry(entry rawBox, desc *TrackDescription, limits Limits) error {
	if len(entry.data) < 28 || be.Uint16(entry.data[8:10]) != 0 {
		return ErrInvalid
	}
	desc.Track = media.TrackAudio
	desc.Channels = be.Uint16(entry.data[16:18])
	desc.SampleRateHz = be.Uint32(entry.data[24:28]) >> 16
	// kind выбирает стандартный дочерний блок конфигурации кодека, независимо от UUID.
	kind := "esds"
	switch entry.kind {
	case "mp4a":
		desc.Codec = core.CodecAAC
	case "Opus":
		desc.Codec, kind = core.CodecOpus, "dOps"
	case "fLaC":
		desc.Codec, kind = core.CodecFLAC, "dfLa"
	case "ac-3":
		desc.Codec, kind = core.CodecAC3, "dac3"
	default:
		return ErrUnsupportedCodec
	}
	// extra содержит единственный блок конфигурации кодека, err сообщает о неверном обрамлении или повторе.
	extra, err := find(entry.data[28:], kind)
	if err != nil {
		return err
	}
	if desc.Codec != core.CodecAAC && uint64(len(extra.data)) > limits.MaxExtradataBytes+8 {
		return ErrLimit
	}
	switch desc.Codec {
	case core.CodecAAC:
		if len(extra.data) < 4 {
			return ErrInvalid
		}
		// oti отличает MPEG-1 LayerIII от AAC; parseErr сообщает об ошибке поиска, после которой сохраняется прежний разбор ASC.
		oti, parseErr := audioObjectType(extra.data[4:])
		if parseErr == nil && oti == 0x6b {
			desc.Codec = core.CodecMP3
			if err = validateMP3ESDS(extra.data); err != nil {
				return err
			}
		} else {
			desc.Headers, err = parseASC(extra.data[4:], limits.MaxExtradataBytes)
		}
	case core.CodecOpus:
		desc.Headers, err = codec.OpusHeadFromDOps(extra.data)
		if err != nil {
			return translateCodecError(err)
		}
	case core.CodecFLAC:
		if len(extra.data) != 42 || be.Uint32(extra.data[:4]) != 0 || !bytes.Equal(extra.data[4:8], []byte{0x80, 0, 0, 34}) {
			return ErrInvalid
		}
		desc.Headers = bytes.Clone(extra.data[8:])
	case core.CodecAC3:
		if len(extra.data) != 3 {
			return ErrInvalid
		}
		desc.Headers = bytes.Clone(extra.data)
	}
	if err != nil {
		return err
	}
	if newAudioCodec(desc.Codec) {
		desc.SampleEntry = entry.kind
	}
	if newAudioCodec(desc.Codec) && (be.Uint16(entry.data[18:20]) != 16 || be.Uint32(entry.data[24:28])&0xffff != 0) {
		return ErrInvalid
	}
	if uint64(len(desc.Headers)) > limits.MaxExtradataBytes {
		return ErrLimit
	}
	return nil
}

// readDescriptor извлекает один MPEG-4 дескриптор из data, возвращая его тип, тело и остаток.
// Ограниченная четырьмя байтами длина и полное тело обязательны до создания срезов.
func readDescriptor(data []byte) (byte, []byte, []byte, error) {
	if len(data) < 2 {
		return 0, nil, nil, ErrInvalid
	}
	// tag содержит тип дескриптора; size накапливает объявленную длину тела.
	tag, size := data[0], uint32(0)
	data = data[1:]
	for range 4 {
		if len(data) == 0 {
			return 0, nil, nil, ErrInvalid
		}
		// value содержит очередные семь бит длины и признак продолжения.
		value := data[0]
		data = data[1:]
		size = size<<7 | uint32(value&0x7f)
		if value&0x80 == 0 {
			if uint64(size) > uint64(len(data)) {
				return 0, nil, nil, ErrInvalid
			}
			return tag, data[:size], data[size:], nil
		}
	}
	return 0, nil, nil, ErrInvalid
}

// audioObjectType находит ObjectTypeIndication стандартного DecoderConfigDescriptor в data.
// Для AAC сохраняется прежний разбор ASC; здесь проверяется только нужная вложенность ES.
func audioObjectType(data []byte) (byte, error) {
	// tag задаёт тип ES_Descriptor, body — его тело, rest — остаток; err сообщает об усечении.
	tag, body, rest, err := readDescriptor(data)
	if err != nil || tag != 3 || len(body) < 3 || body[2] != 0 || len(rest) != 0 {
		return 0, ErrInvalid
	}
	body = body[3:]
	for len(body) > 0 {
		tag, data, body, err = readDescriptor(body)
		if err != nil {
			return 0, err
		}
		if tag == 4 {
			if len(data) < 13 {
				return 0, ErrInvalid
			}
			return data[0], nil
		}
	}
	return 0, ErrInvalid
}

// validateMP3ESDS проверяет data как FullBox esds с OTI0x6B без DecoderSpecificInfo.
// Отсутствие DSI отделяет внутреннее описание фактического MP3 от его стандартной сигнализации в контейнере.
func validateMP3ESDS(data []byte) error {
	if len(data) < 4 || be.Uint32(data[:4]) != 0 {
		return ErrInvalid
	}
	// tag, body и rest описывают тип, тело и остаток единственного ES; err сообщает об усечении дескриптора.
	tag, body, rest, err := readDescriptor(data[4:])
	if err != nil || tag != 3 || len(rest) != 0 || len(body) < 3 || body[2] != 0 {
		return ErrInvalid
	}
	body = body[3:]
	// found отмечает единственный DecoderConfigDescriptor в ES.
	found := false
	for len(body) > 0 {
		// child содержит тело очередного дескриптора после ограниченного чтения.
		var child []byte
		tag, child, body, err = readDescriptor(body)
		if err != nil {
			return err
		}
		if tag == 5 {
			return ErrInvalid
		}
		if tag == 4 {
			if found || len(child) != 13 || child[0] != 0x6b || child[1] != 0x15 {
				return ErrInvalid
			}
			found = true
		} else if tag != 6 {
			return ErrInvalid
		}
	}
	if !found {
		return ErrInvalid
	}
	return nil
}

// validateAudioWireConfig сверяет c со стандартной записью desc и фактическими заголовками кодека.
// UUID не может заменить эти сведения и не назначает FLACOwned.
func validateAudioWireConfig(c media.CodecConfig, desc TrackDescription) error {
	// info содержит заявленный фактический формат; actual и err обозначают независимо восстановленный формат и ошибку разбора.
	info := c.Info()
	actual, err := codec.AudioFormatHeaders(desc.Codec, desc.Headers)
	if err != nil {
		return translateCodecError(err)
	}
	if info.Audio != actual || desc.SampleRateHz != actual.SampleRateHz || desc.Channels != actual.Channels {
		return ErrInvalid
	}
	return nil
}

// validateAudioSample проверяет payload и длительность info в шкале base у новой аудиодорожки.
// kind выбирает кодек, headers содержит его конфигурацию для проверки фактической AU.
// freshSilentOpus и freshSilentMP3 разрешают чтение сырой длительности свежей собственной тишины.
// Время не исправляется; полная CELT120 либо MP3 AU1152 остаётся обязательной, ноль у MP3 запрещён.
func validateAudioSample(kind core.Codec, headers, payload []byte, info media.PacketInfo, base media.TimeBase, freshSilentOpus, freshSilentMP3 bool) error {
	if !newAudioCodec(kind) {
		return nil
	}
	if freshSilentOpus && kind == core.CodecOpus || freshSilentMP3 && kind == core.CodecMP3 {
		// samples содержит число отсчётов полной AU, err сообщает об отказе разбора до проверки контейнерной длительности.
		samples, err := codec.AudioSamples(kind, headers, payload)
		if err != nil {
			return translateCodecError(err)
		}
		// duration выражает исходную длительность в отсчётах декодера; timingErr сообщает о неточном переводе. Значение не подставляется по TOC.
		duration, timingErr := rescale(info.DurationTicks, base, media.TimeBase{Numerator: 1, Denominator: 48000})
		if timingErr != nil || duration < 0 || duration > int64(samples) || kind == core.CodecMP3 && duration == 0 {
			return ErrUnsupportedTiming
		}
		return nil
	}
	return translateCodecResult(codec.ValidateAudioDuration(kind, headers, payload, info, base))
}

// completeAudioActual завершает конфигурацию item по фактической payload до публикации Init.
// При preSkip120 удаляет ровно одну проверенную AU, сохраняя время и байты удержанного сегмента.
func (d *Demuxer) completeAudioActual(item *sample, payload []byte) (bool, error) {
	// track задаёт завершаемую дорожку, desc отделяет фактические исходные сведения от пользовательской функции.
	track := item.track
	desc := track.description
	// actual содержит фактический формат, headers — проверенные заголовки; err сообщает об отказе полной AU или записи.
	actual, headers, err := codec.CompleteAudioConfig(desc.Codec, desc.Headers, payload)
	if err != nil {
		return false, translateCodecError(err)
	}
	if desc.SampleRateHz != actual.SampleRateHz || desc.Channels != actual.Channels {
		return false, ErrInvalid
	}
	if err = validateAudioSample(desc.Codec, headers, payload, item.info, desc.TimeBase, d.options.FreshSilentOpus, d.options.FreshSilentMP3); err != nil {
		return false, err
	}
	if desc.Codec == core.CodecOpus {
		// head содержит настоящее начальное исключение декодера, parseErr сообщает о неверном заголовке; после удаления первой AU headers уже имеют ноль.
		head, parseErr := codec.ParseOpusHead(headers)
		if parseErr != nil {
			return false, translateCodecError(parseErr)
		}
		if head.PreSkip == 120 {
			if track.opusSkipped {
				return false, ErrInvalid
			}
			track.description.Headers, err = codec.NormalizeOpusHead(headers)
			if err != nil {
				return false, translateCodecError(err)
			}
			track.opusSkipped = true
			return true, nil
		}
	}
	desc.Headers, desc.FirstSample, desc.FirstSampleInfo = bytes.Clone(headers), bytes.Clone(payload), item.info
	// info использует контейнерную шкалу, пока вызывающий код или UUID не предоставит проверенную исходную шкалу.
	info := media.CodecConfigInfo{Track: media.TrackAudio, SourceEpoch: d.options.SourceEpoch, Revision: d.options.AudioRevision, TimeBase: desc.TimeBase, Audio: actual}
	if track.audioClaim != nil {
		info = *track.audioClaim
		if info.Track != media.TrackAudio || info.Audio != actual {
			return false, ErrInvalid
		}
	}
	if d.options.ResolveConfig != nil {
		// callbackDesc клонирует каждый буфер: пользовательская функция не может заменить проверяемые исходные байты.
		callbackDesc := desc
		callbackDesc.Headers, callbackDesc.Color, callbackDesc.FirstSample = bytes.Clone(desc.Headers), bytes.Clone(desc.Color), bytes.Clone(desc.FirstSample)
		// resolved содержит результат пользовательского восстановителя; resolveErr сообщает об отказе, а сам результат ещё подлежит проверке.
		resolved, resolveErr := d.options.ResolveConfig(callbackDesc)
		if resolveErr != nil {
			return false, resolveErr
		}
		if err = validateWireConfig(resolved, desc); err != nil {
			return false, err
		}
		if track.audioClaim == nil {
			info = resolved.Info()
		}
	}
	applyIdentity(&info, d.options)
	// FLACOwned не восстанавливается из файла, UUID или общей функции восстановления: доверенный компонент отмечает его отдельно.
	info.FLACOwned = false
	// config хранит проверенный снимок конфигурации с непустыми фактическими заголовками.
	config, err := media.NewCodecConfig(info, headers)
	if err != nil {
		return false, err
	}
	if err = validateWireConfig(config, desc); err != nil {
		return false, err
	}
	if err = d.finishAudioTimeBase(track, item, info.TimeBase); err != nil {
		return false, err
	}
	track.config, track.incomplete = config, false
	track.description.Headers = bytes.Clone(headers)
	track.description.FirstSample, track.description.FirstSampleInfo = bytes.Clone(payload), item.info
	d.init.Audio = config
	return false, nil
}

// finishAudioTimeBase переводит item и ещё не выданные пакеты track из контейнерной шкалы в target.
// Время остаётся точным; нецелый перевод отклоняется до публикации завершённой конфигурации.
func (d *Demuxer) finishAudioTimeBase(track *trackState, item *sample, target media.TimeBase) error {
	// source задаёт шкалу, в которой parseTraf прочитал незавершённую аудиодорожку.
	source := track.description.TimeBase
	if source == target {
		return nil
	}
	// convert переводит info без изменения Keyframe; каждый err сохраняет причину неточного пересчёта.
	convert := func(info media.PacketInfo) (media.PacketInfo, error) {
		// err объединяет последовательные точные переводы трёх временных величин.
		var err error
		info.PTS, err = rescale(info.PTS, source, target)
		if err != nil {
			return info, err
		}
		info.DTS, err = rescale(info.DTS, source, target)
		if err != nil {
			return info, err
		}
		info.DurationTicks, err = rescale(info.DurationTicks, source, target)
		return info, err
	}
	// err сообщает о неточном переводе текущего либо уже разобранного времени.
	var err error
	item.info, err = convert(item.info)
	if err != nil {
		return err
	}
	// index указывает на описание ещё не прочитанной нагрузки в том же moof.
	for index := range d.samples {
		if d.samples[index].track == track {
			d.samples[index].info, err = convert(d.samples[index].info)
			if err != nil {
				return err
			}
		}
	}
	// index указывает на удержанный пакет этой дорожки, прочитанный при уточнении другой дорожки.
	for index := range d.prefix {
		if d.prefix[index].sample.track == track {
			d.prefix[index].sample.info, err = convert(d.prefix[index].sample.info)
			if err != nil {
				return err
			}
		}
	}
	track.lastDTS, err = rescale(track.lastDTS, source, target)
	return err
}
