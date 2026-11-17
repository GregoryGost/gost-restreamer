package fallback

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/GregoryGost/gost-restreamer/internal/media/encoding"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

// NormalizePrepared применяется только к выходу принадлежащего менеджеру нативного AAC-кодировщика.
// ctx ограничивает чтение input и запись нормализованных пакетов в output.
// Удаляется лишь целый пакет задержки кодировщика, явно помеченный Skip Samples в фактическом MP4.
// Метод без явного профиля не исправляет сигнализацию карты каналов.
func (p Prober) NormalizePrepared(ctx context.Context, input *os.File, output io.Writer) error {
	return p.normalizePrepared(ctx, input, output, nil, nil)
}

// NormalizePreparedForProfile обрабатывает свежий результат собственного генератора с явным
// профилем. Для проверенной связки FFmpeg 7.1.5 с нативным AAC допускается коррекция индексного ASC
// обычной 7.1; входные файлы и готовые варианты через этот метод не проходят. AAC сохраняет
// нормализацию целого пакета задержки. Остальные привязки обрабатывают собственную тишину
// выбранного аудиокодека отдельно. ctx ограничивает чтение input и запись output; profile задаёт рецепт.
func (p Prober) NormalizePreparedForProfile(ctx context.Context, input *os.File, output io.Writer, profile core.GroupProfile) error {
	// err сообщает о некорректном профиле до чтения файла или записи результата.
	if err := profile.Validate(); err != nil {
		return err
	}
	if profile.Format.Audio.Codec != core.CodecAAC {
		return p.normalizePreparedAudio(ctx, input, output, profile, nil)
	}
	if encoding.UsesNativeAAC71(profile, encoding.Prepared) {
		return p.normalizePrepared(ctx, input, output, &profile.Format.Audio, nil)
	}
	return p.normalizePrepared(ctx, input, output, nil, nil)
}

// NormalizePreparedForSource передаёт проверенный снимок source свежему результату profile.
// HDR требует полной проверки source; SDR использует NormalizePreparedForProfile.
// ctx ограничивает обработку input; output получает временный файл до публикации поколения.
func (p Prober) NormalizePreparedForSource(ctx context.Context, input *os.File, output io.Writer, profile core.GroupProfile, source Source) error {
	if !isHDRVideo(profile.Format.Video) {
		return p.NormalizePreparedForProfile(ctx, input, output, profile)
	}
	if profile.Validate() != nil || !source.HDRVerified || source.Format.Video == nil {
		return core.ErrNotReady
	}
	if profile.Format.Audio.Codec != core.CodecAAC {
		return p.normalizePreparedAudio(ctx, input, output, profile, &source.HDR)
	}
	// expected включает узкую коррекцию ASC лишь для принадлежащего генератору обычного AAC 7.1.
	var expected *core.AudioFormat
	if encoding.UsesNativeAAC71(profile, encoding.Prepared) {
		expected = &profile.Format.Audio
	}
	return p.normalizePrepared(ctx, input, output, expected, &source.HDR)
}

// normalizePrepared нормализует временную шкалу собственного результата; expected ненулевой только для
// свежего результата собственного AAC 7.1-кодирования. Заголовки исправляются до ResolveConfig
// и строгой проверки контейнера, а наблюдаемый формат снова выводится из исправленного ASC,
// без подстановки желаемого формата. Заголовки видео обязательны,
// кроме VP9, запись которого читается из контейнера; полная сигнализация AV1 проверяется по TU.
// sourceHDR разрешает направленное согласование метаданных только свежего собственного результата.
// ctx ограничивает чтение input и запись output; p задаёт пределы контейнера и числа пакетов.
func (p Prober) normalizePrepared(ctx context.Context, input *os.File, output io.Writer, expected *core.AudioFormat, sourceHDR *media.HDRMetadata) error {
	// actual содержит наблюдаемый формат подготовленного файла. err сообщает об отказе определения
	// формата.
	actual, err := p.observedFormat(ctx, input)
	if err != nil {
		return fmt.Errorf("измерение свежего результата: %w", err)
	}
	// timing содержит проверенные метки ffprobe и подтверждение целого пакета задержки AAC.
	timing, err := p.inspectPreparedTiming(ctx, input, actual)
	if err != nil {
		return fmt.Errorf("проверка времени свежего результата: %w", err)
	}
	if _, err = input.Seek(0, io.SeekStart); err != nil {
		return err
	}
	// normalizeHeaders при явном допуске исправляет только AAC и сверяет разобранный результат
	// с ожидаемым профилем; видео и все остальные рецепты сохраняют исходные заголовки.
	var normalizeHeaders func(fmp4.TrackDescription) ([]byte, error)
	if expected != nil {
		// desc содержит описание дорожки и её исходные заголовки, проверяемые профильным нормализатором.
		normalizeHeaders = func(desc fmp4.TrackDescription) ([]byte, error) {
			if desc.Track != media.TrackAudio {
				return desc.Headers, nil
			}
			// headers содержит проверенный ASC после узкой коррекции; err сообщает об иной
			// или повреждённой сигнализации, которую исправлять нельзя.
			headers, err := codec.NormalizeNativeAAC71ASC(desc.Headers)
			if err != nil {
				return nil, err
			}
			// format выводится из реальных исправленных байтов и должен совпасть с профилем
			// и фактически измеренной частотой исходного результата генератора.
			format, err := codec.AudioFormatASC(headers)
			if err != nil {
				return nil, err
			}
			if format != *expected || format.SampleRateHz != actual.Audio.SampleRateHz || format.Channels != actual.Audio.Channels {
				return nil, core.ErrCorrupt
			}
			actual.Audio = format
			return headers, nil
		}
	}
	// d читает исходный fMP4 с восстановлением конфигураций по фактическому формату.
	// desc содержит фактическое описание дорожки контейнера для сверки и восстановления конфигурации.
	d, err := fmp4.NewDemuxer(contextReader{ctx, input}, fmp4.DemuxOptions{Limits: p.Limits.Container, SourceEpoch: 1, VideoRevision: 1, AudioRevision: 1, NormalizeHeaders: normalizeHeaders, FreshHDRSource: sourceHDR, ResolveConfig: func(desc fmp4.TrackDescription) (media.CodecConfig, error) {
		// Восстанавливает конфигурацию дорожки по фактическому формату и заголовкам fMP4; расхождение кодека
		// или размеров видео возвращает ErrCorrupt.

		// info накапливает описание дорожки из контейнера и подтверждённого формата.
		info := media.CodecConfigInfo{Track: desc.Track, SourceEpoch: 1, Revision: 1, TimeBase: desc.TimeBase}
		if desc.Track == media.TrackVideo {
			if !containerVideoMatches(desc, actual.Video) {
				return media.CodecConfig{}, core.ErrCorrupt
			}
			info.Video = actual.Video
			info.HDR = desc.HDR
		} else {
			info.Audio = actual.Audio
		}
		return media.NewCodecConfig(info, desc.Headers)
	}})
	if err != nil {
		return fmt.Errorf("чтение свежего fMP4: %w", err)
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
	// firstMetadata сохраняет только фактические внутрипоточные сведения первого AU собственного результата.
	var firstMetadata media.HDRMetadata
	for !video || !audio {
		// packet содержит очередной начальный пакет fMP4. e сообщает об отказе его чтения.
		packet, e := d.Next()
		if e != nil {
			return fmt.Errorf("начальный пакет fMP4: %w", e)
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
				headers, format, e := codec.AV1ConfigFromTUWithMetadata(init.Video.Headers(), packet.Payload(), actual.Video.FrameRate)
				if e != nil {
					return fmt.Errorf("конфигурация AV1 первого TU: %w", e)
				}
				if e = codec.CompatibleActual(core.MediaFormat{Video: format, Audio: actual.Audio}, actual); e != nil {
					return e
				}
				info.Video = format
				init.Video, e = media.NewCodecConfig(info, headers)
				if e != nil {
					return e
				}
			}
			{
				// metadataInspector проверяет первый исходный AU до переноса отсутствующих сведений источника.
				var metadataInspector codec.Inspector
				// firstPacket использует фактические заголовки последовательности первого TU после
				// узкой AV1-нормализации; inspectErr сообщает об отказе пересборки пакета.
				firstPacket, inspectErr := media.NewPacket(packet.Info(), init.Video, packet.Payload())
				if inspectErr != nil {
					return inspectErr
				}
				// metadata содержит только проверенную внутрипоточную сигнализацию, отдельно от HDR контейнера.
				_, metadata, inspectErr := metadataInspector.InspectMetadata(firstPacket)
				if inspectErr != nil {
					return fmt.Errorf("HDR первого AU: %w", inspectErr)
				}
				firstMetadata = metadata
				info = init.Video.Info()
				if sourceHDR != nil {
					info.HDR, e = codec.ReconcileFreshHDR(*sourceHDR, info.HDR, metadata)
				} else {
					info.HDR, e = combineSourceHDR(metadata, info.HDR)
				}
				if e != nil {
					return e
				}
				init.Video, e = media.NewCodecConfig(info, init.Video.Headers())
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
	// packet содержит очередную полную единицу доступа, сверяемую с измеренными метками и записываемую после нормализации.
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
			return fmt.Errorf("сжатый пакет %s[%d]: %w", track, index, e)
		}
		// key показывает независимость видеокадра, установленную по сжатым данным.
		var key bool
		if track == media.TrackVideo {
			// metadata содержит только внутрипоточные сведения текущей последовательности,
			// не принимая перенесённые из источника поля Info за сигнализацию кодека.
			var metadata media.HDRMetadata
			key, metadata, e = inspector.InspectMetadata(rebuilt)
			if e == nil && metadata != firstMetadata {
				return core.ErrNotReady
			}
		} else {
			key, e = inspector.Inspect(rebuilt)
		}
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
