package fallback

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"math/big"
	"os"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/GregoryGost/gost-restreamer/internal/media/encoding"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

// freshSilentAudio проверяет последовательность собственного свежего аудиокодировщика.
// После полной проверки периода менеджер отдельно проверяет staging-файл до публикации поколения.
type freshSilentAudio struct {
	// config содержит собственную выходную конфигурацию с нормализованным началом.
	config media.CodecConfig
	// count считает входные AU после удаления целой начальной Opus AU самим demux.
	count uint64
	// written считает полные AU нормализованного периода без начальной MP3 AU.
	written uint64
	// previous сохраняет фактический предыдущий DTS в исходной шкале для проверки непрерывности.
	previous int64
	// partial отмечает единственную разрешённую неполную контейнерную длительность последней Opus/MP3 AU.
	partial bool
	// rawEnd сохраняет фактический конец последней AU до нормализации для проверки MP3-периода.
	rawEnd *big.Rat
}

// packet проверяет raw как свежий результат собственного кодировщика, возвращая новый полный пакет.
// Для MP3 первая AU исключается; nil означает только этот подтверждённый начальный случай.
// Произвольные входные файлы и LIVE не должны вызывать этот normalizer.
func (s *freshSilentAudio) packet(raw media.Packet) (*media.Packet, error) {
	// config содержит реальные headers/исходную шкалу, info — фактические container timestamps.
	config, info := raw.Config(), raw.Info()
	// kind определяет конечную привязку; quantum задаёт полный размер собственной файловой AU.
	kind := config.Info().Audio.Codec
	quantum := preparedAudioSamples(kind)
	// samples подтверждает реальный размер AU; err сохраняет ошибку разбора сжатых данных.
	samples, err := codec.AudioSamples(kind, config.Headers(), raw.Payload())
	if err != nil {
		return nil, err
	}
	if samples != quantum || info.PTS != info.DTS || s.partial {
		return nil, core.ErrCorrupt
	}
	if kind == core.CodecOpus || kind == core.CodecMP3 {
		if info.DurationTicks < 0 || kind == core.CodecMP3 && info.DurationTicks == 0 || ticks(info.DurationTicks, config.Info().TimeBase).Cmp(ticks(int64(quantum), s.config.Info().TimeBase)) > 0 {
			return nil, core.ErrCorrupt
		}
		s.partial = !exactPreparedAudioDuration(info.DurationTicks, config.Info().TimeBase, config.Info().Audio)
	} else if !exactPreparedAudioDuration(info.DurationTicks, config.Info().TimeBase, config.Info().Audio) {
		return nil, core.ErrCorrupt
	}
	if s.count > 0 {
		// step выражает разность двух исходных DTS без переполнения знакового int64.
		step := ticks(info.DTS, config.Info().TimeBase)
		step.Sub(step, ticks(s.previous, config.Info().TimeBase))
		if step.Cmp(ticks(int64(quantum), s.config.Info().TimeBase)) != 0 {
			return nil, core.ErrCorrupt
		}
	} else {
		switch kind {
		case core.CodecOpus, core.CodecFLAC:
			if info.DTS != 0 {
				return nil, core.ErrNotReady
			}
		case core.CodecMP3, core.CodecAC3:
			if info.DTS >= 0 || ticks(info.DTS, config.Info().TimeBase).Cmp(ticks(-int64(quantum), s.config.Info().TimeBase)) <= 0 {
				return nil, core.ErrNotReady
			}
		}
	}
	s.previous = info.DTS
	s.rawEnd = ticks(info.DTS, config.Info().TimeBase)
	s.rawEnd.Add(s.rawEnd, ticks(info.DurationTicks, config.Info().TimeBase))
	s.count++
	if kind == core.CodecMP3 && s.count == 1 {
		return nil, nil
	}
	if s.written > uint64(math.MaxInt64)/uint64(quantum) {
		return nil, ErrLimit
	}
	// payload клонирует AU; только собственная AC3-тишина проходит полную проверку коэффициентов.
	payload := raw.Payload()
	if kind == core.CodecAC3 {
		payload, err = codec.NormalizeAC3Silence(config.Headers(), payload)
		if err != nil {
			return nil, err
		}
	}
	if kind == core.CodecFLAC {
		// frame подтверждает фиксированный размер блока4608 и последовательный исходный номер без переписывания subframes.
		frame, parseErr := codec.FLACFrameInfo(config.Headers(), payload)
		if parseErr != nil {
			return nil, parseErr
		}
		if frame.Variable || frame.Number != s.written {
			return nil, core.ErrCorrupt
		}
	}
	// stamp задаёт origin0 и полную длительность исключительно доказанной собственной тишины.
	stamp := media.PacketInfo{PTS: int64(s.written) * int64(quantum), DTS: int64(s.written) * int64(quantum), DurationTicks: int64(quantum)}
	// packet сохраняет проверенные байты и нормализованную шкалу; err сообщает об отказе создания.
	packet, err := media.NewPacket(stamp, s.config, payload)
	if err != nil {
		return nil, err
	}
	s.written++
	return &packet, nil
}

// normalizePreparedAudio обрабатывает только собственную свежую тишину четырёх аудиопривязок.
// ctx ограничивает чтение input; output является staging-результатом и не публикуется до успеха
// полной проверки периода и последующих Verify/VerifyLoopDuration. profile задаёт ожидаемый рецепт,
// sourceHDR переносит проверенный статический снимок выбранного HDR-источника.
func (p Prober) normalizePreparedAudio(ctx context.Context, input *os.File, output io.Writer, profile core.GroupProfile, sourceHDR *media.HDRMetadata) error {
	// actual содержит фактически измеренные дорожки; err сохраняет ошибку ffprobe или проверки заголовков.
	actual, err := p.observedFormat(ctx, input)
	if err != nil {
		return fmt.Errorf("измерение свежего аудиорезультата: %w", err)
	}
	if profile.Format.Audio.Codec == core.CodecAAC || core.CompatibleProfileFormat(actual, profile) != nil {
		return core.ErrInvalidFormat
	}
	if _, err = input.Seek(0, io.SeekStart); err != nil {
		return err
	}
	// seenOpusStart подтверждает исходный Head120 до того, как demux потребит целую начальную AU.
	seenOpusStart := false
	// options требует реальные байты кодека и разрешает узкий допуск конечной неполной длительности только собственным Opus/MP3.
	// NormalizeHeaders проверяет исходный desc, не исправляя preSkip до потребления начальной AU.
	// ResolveConfig соединяет проверенный actual с фактическими заголовками и HDR того же контейнера.
	options := fmp4.DemuxOptions{Limits: p.Limits.Container, SourceEpoch: 1, VideoRevision: 1, AudioRevision: 1, FreshHDRSource: sourceHDR, FreshSilentOpus: actual.Audio.Codec == core.CodecOpus, FreshSilentMP3: actual.Audio.Codec == core.CodecMP3,
		NormalizeHeaders: func(desc fmp4.TrackDescription) ([]byte, error) {
			if desc.Track == media.TrackAudio && desc.Codec == core.CodecOpus {
				// head сохраняет фактическую начальную задержку; parseErr сообщает о неверной привязке Head.
				head, parseErr := codec.ParseOpusHead(desc.Headers)
				if parseErr != nil {
					return nil, parseErr
				}
				if head.PreSkip != 120 {
					return nil, core.ErrNotReady
				}
				seenOpusStart = true
			}
			return desc.Headers, nil
		},
		// desc содержит фактическое описание дорожки и заголовки fMP4 для восстановления конфигурации.
		ResolveConfig: func(desc fmp4.TrackDescription) (media.CodecConfig, error) {
			// info хранит фактический формат и исходную шкалу без доверия UUID или desired вместо headers.
			info := media.CodecConfigInfo{Track: desc.Track, SourceEpoch: 1, Revision: 1, TimeBase: desc.TimeBase}
			if desc.Track == media.TrackVideo {
				if !containerVideoMatches(desc, actual.Video) {
					return media.CodecConfig{}, core.ErrCorrupt
				}
				info.Video, info.HDR = actual.Video, desc.HDR
			} else {
				info.Audio = actual.Audio
			}
			return media.NewCodecConfig(info, desc.Headers)
		}}
	// demux сохраняет исходные длительности контейнера; разрешённые конечные Opus/MP3 пока не исправлены.
	demux, err := fmp4.NewDemuxer(contextReader{ctx, input}, options)
	if err != nil {
		return fmt.Errorf("инициализация свежего аудиоконтейнера: %w", err)
	}
	if actual.Audio.Codec == core.CodecOpus && !seenOpusStart {
		return core.ErrNotReady
	}
	// init содержит реальные конфигурации; prefix удерживает только начало до первого видео.
	init := demux.Init()
	var prefix []media.Packet
	// prefixBytes ограничивает удержанный префикс прежним бюджетом инициализации.
	prefixBytes := uint64(0)
	// firstMetadata сохраняет внутрипоточный HDR первого видео отдельно от перенесённого sourceHDR.
	var firstMetadata media.HDRMetadata
	for {
		// packet содержит очередную начальную AU; readErr сообщает о конце или ошибке контейнера.
		packet, readErr := demux.Next()
		if readErr != nil {
			return readErr
		}
		if uint64(len(prefix)) >= p.Limits.MaxPackets || packet.StoredBytes() > p.Limits.Container.MaxInitBytes-prefixBytes {
			return ErrLimit
		}
		prefix = append(prefix, packet)
		prefixBytes += packet.StoredBytes()
		if packet.Config().Info().Track == media.TrackVideo {
			init.Video, firstMetadata, err = freshPreparedVideo(profile, init.Video, packet, actual, sourceHDR)
			if err != nil {
				return err
			}
			init.Origin, err = preparedOrigin(packet.Info().DTS, packet.Config().Info().TimeBase, init.Video.Info().TimeBase, init.Audio.Info().TimeBase)
			if err != nil {
				return err
			}
			break
		}
	}
	// audioInfo задаёт собственную шкалу сэмплов; FLACOwned подтверждено этой попыткой кодирования.
	audioInfo := init.Audio.Info()
	audioInfo.TimeBase = media.TimeBase{Numerator: 1, Denominator: actual.Audio.SampleRateHz}
	audioInfo.FLACOwned = actual.Audio.Codec == core.CodecFLAC
	init.Audio, err = media.NewCodecConfig(audioInfo, init.Audio.Headers())
	if err != nil {
		return err
	}
	// mux записывает только самостоятельный FLAC с блоками фиксированного размера; переписывание непрерывного потока здесь не используется.
	mux, err := fmp4.NewMuxer(output, init, p.Limits.Container)
	if err != nil {
		return err
	}
	// audio проверяет native форму и нормализацию собственной тишины; period проверяет обе дорожки.
	audio := freshSilentAudio{config: init.Audio}
	period := newPreparedPeriod(actual)
	// inspector проверяет фактическое видео и неизменность его внутрипоточных HDR-сведений.
	var inspector codec.Inspector
	// write пересобирает packet только с проверенной конфигурацией и нормализованным собственным audio.
	write := func(packet media.Packet) error {
		if packet.Config().Info().Track == media.TrackAudio {
			// normalized содержит полную AU либо nil для одной начальной MP3 AU; normalizeErr сохраняет отказ.
			normalized, normalizeErr := audio.packet(packet)
			if normalizeErr != nil {
				return fmt.Errorf("нормализация %s AU[%d]: %w", actual.Audio.Codec, audio.count, normalizeErr)
			}
			if normalized == nil {
				return nil
			}
			packet = *normalized
		} else {
			packet, err = media.NewPacket(packet.Info(), init.Video, packet.Payload())
			if err != nil {
				return err
			}
			// metadata сохраняет только фактическую сигнализацию AU; inspectErr сообщает об ошибке codec.
			_, metadata, inspectErr := inspector.InspectMetadata(packet)
			if inspectErr != nil {
				return inspectErr
			}
			if metadata != firstMetadata {
				return core.ErrNotReady
			}
		}
		// info и base описывают нормализованную AU для точной проверки [0,T) без округления.
		info, base := packet.Info(), packet.Config().Info().TimeBase
		if err = period.add(packet.Config().Info().Track, base, info.PTS, info.DTS, info.DurationTicks); err != nil {
			return err
		}
		return mux.WritePacket(packet)
	}
	// packet обозначает уже удержанную начальную AU в исходном порядке.
	for _, packet := range prefix {
		if err = write(packet); err != nil {
			return err
		}
	}
	// total считает все AU после префикса, сохраняя общий MaxPackets.
	for total := uint64(len(prefix)); ; total++ {
		// packet содержит очередную AU; readErr различает чистый конец и повреждение файла.
		packet, readErr := demux.Next()
		if errors.Is(readErr, io.EOF) {
			break
		}
		if readErr != nil {
			return readErr
		}
		if total >= p.Limits.MaxPackets {
			return ErrLimit
		}
		if err = write(packet); err != nil {
			return err
		}
	}
	if audio.written == 0 || actual.Audio.Codec == core.CodecMP3 && audio.count != audio.written+1 {
		return core.ErrNotReady
	}
	// duration подтверждает, что нормализованные N полных AU занимают весь измеренный период видео.
	duration, err := period.duration()
	if err != nil {
		return err
	}
	if actual.Audio.Codec == core.CodecMP3 && audio.rawEnd.Cmp(duration) != 0 {
		return core.ErrNotReady
	}
	if duration.Cmp(ticks(int64(p.Limits.MaxDuration), media.TimeBase{Numerator: 1, Denominator: 1000000000})) > 0 {
		return ErrLimit
	}
	return nil
}

// freshPreparedVideo восстанавливает конфигурацию первого packet собственного profile по actual.
// sourceHDR переносится лишь после проверки фактического in-band HDR; результат сохраняет firstMetadata
// отдельно, чтобы последующие AU не меняли статический снимок. Начальное видео должно быть независимым.
func freshPreparedVideo(profile core.GroupProfile, config media.CodecConfig, packet media.Packet, actual core.MediaFormat, sourceHDR *media.HDRMetadata) (media.CodecConfig, media.HDRMetadata, error) {
	// info сохраняет фактический контейнерный формат до восстановления AV1 sequence header.
	info := config.Info()
	if actual.Video.Codec == core.CodecAV1 {
		// headers и format выводятся из реального TU; err сообщает о несовместимом или неполном AV1.
		headers, format, err := codec.AV1ConfigFromTUWithMetadata(config.Headers(), packet.Payload(), actual.Video.FrameRate)
		if err != nil {
			return media.CodecConfig{}, media.HDRMetadata{}, err
		}
		if err = codec.CompatibleActual(core.MediaFormat{Video: format, Audio: actual.Audio}, actual); err != nil {
			return media.CodecConfig{}, media.HDRMetadata{}, err
		}
		info.Video = format
		config, err = media.NewCodecConfig(info, headers)
		if err != nil {
			return media.CodecConfig{}, media.HDRMetadata{}, err
		}
	}
	// rebuilt соединяет AU с подтверждёнными headers; err сообщает о невозможном пакете.
	rebuilt, err := media.NewPacket(packet.Info(), config, packet.Payload())
	if err != nil {
		return media.CodecConfig{}, media.HDRMetadata{}, err
	}
	// inspector проверяет фактическую независимость и HDR только первого AU.
	var inspector codec.Inspector
	// key подтверждает независимость; metadata содержит реальные in-band сведения без подстановки исходных сведений.
	key, metadata, err := inspector.InspectMetadata(rebuilt)
	if err != nil {
		return media.CodecConfig{}, media.HDRMetadata{}, err
	}
	if !key {
		return media.CodecConfig{}, media.HDRMetadata{}, core.ErrNotReady
	}
	info = config.Info()
	if sourceHDR != nil {
		info.HDR, err = codec.ReconcileFreshHDR(*sourceHDR, info.HDR, metadata)
	} else {
		info.HDR, err = combineSourceHDR(metadata, info.HDR)
	}
	if err != nil {
		return media.CodecConfig{}, media.HDRMetadata{}, err
	}
	config, err = media.NewCodecConfig(info, config.Headers())
	if err == nil {
		err = encoding.ValidateProfileVideoHeaders(profile, config)
	}
	return config, metadata, err
}
