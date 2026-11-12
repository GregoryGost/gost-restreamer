package fallback

import (
	"context"
	"errors"
	"io"
	"os"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

// NormalizePrepared применяется только к выходу принадлежащего менеджеру нативного AAC-кодировщика.
// Удаляется лишь целый пакет задержки кодировщика, явно помеченный Skip Samples в фактическом MP4.
func (p Prober) NormalizePrepared(ctx context.Context, input *os.File, output io.Writer) error {
	// actual содержит наблюдаемый формат подготовленного файла. err сообщает об отказе определения
	// формата.
	actual, err := p.observedFormat(ctx, input)
	if err != nil {
		return err
	}
	// timing содержит проверенные метки ffprobe и подтверждение целого пакета задержки AAC.
	timing, err := p.inspectPreparedTiming(ctx, input, actual)
	if err != nil {
		return err
	}
	if _, err = input.Seek(0, io.SeekStart); err != nil {
		return err
	}
	// d читает исходный fMP4 с восстановлением конфигураций по фактическому формату.
	d, err := fmp4.NewDemuxer(contextReader{ctx, input}, fmp4.DemuxOptions{Limits: p.Limits.Container, SourceEpoch: 1, VideoRevision: 1, AudioRevision: 1, ResolveConfig: func(desc fmp4.TrackDescription) (media.CodecConfig, error) {
		// Восстанавливает конфигурацию дорожки по фактическому формату и заголовкам fMP4; расхождение кодека
		// или размеров видео возвращает ErrCorrupt.

		// info накапливает описание дорожки из контейнера и подтверждённого формата.
		info := media.CodecConfigInfo{Track: desc.Track, SourceEpoch: 1, Revision: 1, TimeBase: desc.TimeBase}
		if desc.Track == media.TrackVideo {
			if desc.Codec != actual.Video.Codec || desc.Width != actual.Video.Width || desc.Height != actual.Video.Height {
				return media.CodecConfig{}, core.ErrCorrupt
			}
			info.Video = actual.Video
		} else {
			info.Audio = actual.Audio
		}
		return media.NewCodecConfig(info, desc.Headers)
	}})
	if err != nil {
		return err
	}
	// init содержит пару конфигураций и отображение начала временной шкалы для нормализованного
	// выхода.
	init := d.Init()
	// prefix удерживает начальные пакеты до обнаружения обеих дорожек.
	var prefix []media.Packet
	// prefixBytes считает логический вес удержанных начальных пакетов в байтах.
	var prefixBytes uint64
	// video отмечает обнаружение первого проверенного видеопакета. audio отмечает наличие первого
	// аудиопакета.
	video, audio := false, false
	for !video || !audio {
		// packet содержит очередной начальный пакет fMP4. e сообщает об отказе его чтения.
		packet, e := d.Next()
		if e != nil {
			return e
		}
		if uint64(len(prefix)) >= p.Limits.MaxPackets || packet.StoredBytes() > p.Limits.Container.MaxInitBytes-prefixBytes {
			return ErrLimit
		}
		prefix = append(prefix, packet)
		prefixBytes += packet.StoredBytes()
		// info содержит описание дорожки очередного начального пакета.
		info := packet.Config().Info()
		if !video && info.Track == media.TrackVideo {
			if !exactDuration(packet.Info().DurationTicks, info.TimeBase, actual.Video.FrameRate) {
				return core.ErrCorrupt
			}
			if actual.Video.Codec == core.CodecAV1 {
				// headers содержит конфигурацию AV1 из фактического первого TU. format
				// содержит подтверждённый формат видео. e сообщает об отказе
				// восстановления конфигурации.
				headers, format, e := codec.AV1ConfigFromTU(init.Video.Headers(), packet.Payload(), actual.Video.FrameRate)
				if e != nil {
					return e
				}
				if e = codec.Compatible(core.MediaFormat{Video: format, Audio: actual.Audio}, actual); e != nil {
					return e
				}
				info.Video = format
				init.Video, e = media.NewCodecConfig(info, headers)
				if e != nil {
					return e
				}
			}
			video = true
		}
		if info.Track == media.TrackAudio {
			audio = true
		}
	}
	init.Origin, err = preparedOrigin(*timing.packets[media.TrackVideo][0].DTS, timing.bases[media.TrackVideo], init.Video.Info().TimeBase, init.Audio.Info().TimeBase)
	if err != nil {
		return err
	}
	// mux записывает нормализованные пакеты и исправленное начало шкалы в выходной fMP4.
	mux, err := fmp4.NewMuxer(output, init, p.Limits.Container)
	if err != nil {
		return err
	}
	// inspector сохраняет состояние проверки сжатых единиц доступа при записи нормализованного
	// потока.
	var inspector codec.Inspector
	// counts считает прочитанные единицы доступа отдельно для каждой дорожки.
	counts := map[media.Track]int{}
	// write сверяет пакет с метками ffprobe, удаляет подтверждённую задержку AAC и записывает
	// пересобранный пакет.
	write := func(packet media.Packet) error {
		// Сверяет очередную AU с измеренными метками, пропускает только первую проверенную AAC AU задержки
		// кодировщика и записывает остальные с нормализованными конфигурациями; первое видео должно быть
		// независимым.

		// track определяет дорожку обрабатываемого пакета.
		track := packet.Config().Info().Track
		// index содержит порядковый номер пакета внутри своей дорожки до увеличения счётчика.
		index := counts[track]
		counts[track]++
		// err сообщает о расхождении временных меток или длительности с ffprobe.
		if err := timing.match(packet, index); err != nil {
			return err
		}
		if track == media.TrackAudio && index == 0 {
			// err сообщает о некорректной AAC-единице задержки, которую предполагается
			// удалить.
			if err := codec.ValidateAACAU(init.Audio.Headers(), packet.Payload()); err != nil {
				return err
			}
			return nil
		}
		// config выбирает нормализованную конфигурацию дорожки для пересборки пакета.
		config := init.Audio
		if packet.Config().Info().Track == media.TrackVideo {
			config = init.Video
		}
		// rebuilt содержит пакет с прежними метками и нормализованной конфигурацией. e сообщает
		// об отказе его создания.
		rebuilt, e := media.NewPacket(packet.Info(), config, packet.Payload())
		if e != nil {
			return e
		}
		// key показывает независимость видеокадра, установленную по сжатым данным.
		key, e := inspector.Inspect(rebuilt)
		if e != nil {
			return e
		}
		if track == media.TrackVideo && index == 0 && !key {
			return core.ErrNotReady
		}
		return mux.WritePacket(rebuilt)
	}
	// packet обозначает начальный пакет, уже прочитанный до создания выходного мультиплексора.
	for _, packet := range prefix {
		if err = write(packet); err != nil {
			return err
		}
	}
	// total считает все прочитанные пакеты для соблюдения общего лимита.
	for total := uint64(len(prefix)); ; total++ {
		// packet содержит очередной пакет после начального префикса. e сообщает об окончании
		// файла или отказе чтения.
		packet, e := d.Next()
		if errors.Is(e, io.EOF) {
			if counts[media.TrackVideo] != len(timing.packets[media.TrackVideo]) || counts[media.TrackAudio] != len(timing.packets[media.TrackAudio]) {
				return core.ErrCorrupt
			}
			return nil
		}
		if e != nil {
			return e
		}
		if total >= p.Limits.MaxPackets {
			return ErrLimit
		}
		if err = write(packet); err != nil {
			return err
		}
	}
}
