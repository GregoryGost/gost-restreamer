package integration

import (
	"bytes"
	"context"
	encodingbinary "encoding/binary"
	"encoding/json"
	"errors"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"math/big"
	"os"
	"path/filepath"
	"runtime"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/fallback"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
	"github.com/GregoryGost/gost-restreamer/internal/media/group"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
)

// Задаёт профиль заглушки 128×128, 30 FPS и стереофонический AAC 48 kHz; AV1 использует copy, H.264 —
// transcode.
func fallbackPipelineProfile(codec core.Codec) core.GroupProfile {
	// mode выбирает copy для AV1 либо transcode для H.264, а profile задаёт соответствующий профиль
	// кодека main или high.
	mode, profile := core.ModeCopy, "main"
	if codec == core.CodecH264 {
		mode, profile = core.ModeTranscode, "high"
	}
	return core.GroupProfile{Mode: mode, Format: core.MediaFormat{Video: core.VideoFormat{Codec: codec, Width: 128, Height: 128, FrameRate: core.FrameRate{Numerator: 30, Denominator: 1}, CodecProfile: profile, PixelFormat: "yuv420p", BitDepth: 8, Color: core.ColorInfo{Range: "limited", Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"}}, Audio: core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", SampleRateHz: 48000, Channels: 2, ChannelLayout: "stereo"}}, VideoEncoding: core.VideoEncoding{TargetBitrateBPS: 200000, MaxBitrateBPS: 200000, BufferBits: 400000, GOPFrames: 15, ClosedGOP: true}, AudioEncoding: core.AudioEncoding{BitrateBPS: 64000}}
}

// Подстановка кодировщика принадлежит только CPU-тесту; остальные аргументы формирует ProductionBuilder.
func fallbackPipelineCPUBuilder(binary string) fallback.CommandBuilder {
	return func(request fallback.GenerateRequest) (fallback.Command, error) {
		// Подменяет рабочий рецепт генерации командами libx264 и libaom-av1 для CPU-проверки с совместимыми
		// параметрами кодировщиков.

		// c содержит штатную команду генерации заглушки, а err сообщает об ошибке её построения.
		c, err := fallback.ProductionBuilder(binary)(request)
		if err != nil {
			return c, err
		}
		// args собирает аргументы CPU-кодирования с сохранением остальных параметров штатного
		// рецепта.
		args := make([]string, 0, len(c.Args)+8)
		// i указывает позицию аргумента штатной команды; параметры заменяемых кодировщиков
		// пропускаются вместе со значениями.
		for i := 0; i < len(c.Args); i++ {
			// arg содержит текущий аргумент после необходимой замены аппаратного кодировщика
			// на CPU-кодировщик.
			arg := c.Args[i]
			if arg == "-svtav1-params" || arg == "-preset" || arg == "-no-scenecut" || arg == "-forced-idr" {
				i++
				continue
			}
			if arg == "h264_nvenc" {
				arg = "libx264"
			}
			if arg == "libsvtav1" {
				arg = "libaom-av1"
			}
			args = append(args, arg)
		}
		// output сохраняет выходной путь команды перед добавлением CPU-настроек кодирования.
		output := args[len(args)-1]
		args = args[:len(args)-1]
		if request.Profile.Format.Video.Codec == core.CodecAV1 {
			args = append(args, "-threads:v", "1", "-cpu-used", "8", "-lag-in-frames", "0")
		} else {
			args = append(args, "-threads:v", "1", "-sc_threshold", "0")
		}
		args = append(args, output)
		c.Args = args
		return c, nil
	}
}

// fallbackPipelineRunner добавляет диагностику теста к ограниченному запуску настоящих медиакоманд.
type fallbackPipelineRunner struct {
	// t принимает диагностические сообщения при отказе запущенной медиакоманды.
	t *testing.T
	// runner выполняет команду с заданными пределами времени, аргументов и диагностики.
	runner fallback.Runner
}

// fallbackActualSource хранит конфигурации и пакеты независимо проверенного источника для подачи в
// переключатель.
type fallbackActualSource struct {
	// init содержит начальные видео- и аудиоконфигурации демультиплексированного источника.
	init fmp4.Init
	// packets хранит последовательности видеопакетов в позиции 0 и аудиопакетов в позиции 1.
	packets [2][]media.Packet
	// next хранит позицию следующего ещё не поданного пакета каждой дорожки источника.
	next [2]int
}

// Проверяет формат отдельным Prober и демультиплексирует fMP4 в исходные последовательности видео и
// аудио. Источник должен содержать 48 видеопакетов и непустую аудиодорожку; ошибка завершает тест.
func fallbackActualPackets(t *testing.T, probe fallback.Prober, data []byte) fallbackActualSource {
	t.Helper()
	// file содержит временную копию исходного fMP4 для независимой проверки Prober, а err сообщает
	// об ошибке её создания.
	file, err := os.CreateTemp(t.TempDir(), "actual-source")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// Закрывает временный исходный файл после независимой проверки медиапакетов.

		// err сообщает об ошибке закрытия временного файла независимого источника.
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err = file.Write(data); err != nil {
		t.Fatal(err)
	}
	// observed содержит формат источника, независимо полученный проверкой файла через Prober.
	observed, err := probe.Source(context.Background(), file, core.AssetVideo)
	if err != nil {
		t.Fatal(err)
	}
	// d читает пакеты fMP4, связывая видео с независимо проверенным форматом источника.
	d, err := fmp4.NewDemuxer(bytes.NewReader(data), fmp4.DemuxOptions{Limits: integrationLimits(), SourceEpoch: 1, VideoRevision: 1, AudioRevision: 1, ResolveConfig: func(desc fmp4.TrackDescription) (media.CodecConfig, error) {
		// Разбирает AAC штатным способом, а видеоконфигурацию связывает с форматом, независимо полученным
		// проверкой исходного файла.

		if desc.Track == media.TrackAudio {
			return fmp4.ResolveCodecConfig(desc, 1, 1)
		}
		if observed.Format.Video == nil {
			return media.CodecConfig{}, core.ErrCorrupt
		}
		return media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: 1, Revision: 1, TimeBase: desc.TimeBase, Video: *observed.Format.Video}, desc.Headers)
	}})
	if err != nil {
		t.Fatal(err)
	}
	// result собирает начальные конфигурации и полные последовательности пакетов обеих дорожек.
	result := fallbackActualSource{init: d.Init()}
	for {
		// p содержит очередной пакет исходного контейнера, а e сообщает об окончании либо ошибке
		// его чтения.
		p, e := d.Next()
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			t.Fatal(e)
		}
		// index выбирает массив видеопакетов 0 или аудиопакетов 1 для прочитанного пакета.
		index := 0
		if p.Config().Info().Track == media.TrackAudio {
			index = 1
		}
		result.packets[index] = append(result.packets[index], p)
	}
	if len(result.packets[0]) != 48 || len(result.packets[1]) == 0 {
		t.Fatalf("независимый 1,6-секундный источник: %d video, %d audio", len(result.packets[0]), len(result.packets[1]))
	}
	return result
}

// Переводит знаковые метки дорожки в точное рациональное число секунд без округления.
func fallbackActualTime(ticks int64, base media.TimeBase) *big.Rat {
	return new(big.Rat).SetFrac(new(big.Int).Mul(big.NewInt(ticks), new(big.Int).SetUint64(uint64(base.Numerator))), new(big.Int).SetUint64(uint64(base.Denominator)))
}

// Создаёт независимое LIVE-кодирование и проверяет переходы заглушка → LIVE, паузу, возобновление и
// возврат по таймеру контроля потока с различными заголовками кодека. Проверяет полные единицы
// доступа, общий сдвиг A/V, возрастающие DTS и предел разрыва, затем декодирует смешанный результат.
func fallbackPipelineActualTransitions(t *testing.T, binary string, probe fallback.Prober, profile core.GroupProfile, prepared []byte) {
	t.Helper()
	// args задаёт независимое LIVE-кодирование тестового изображения и тона 750 Гц с длительностью
	// 1,6 секунды.
	args := []string{"-hide_banner", "-loglevel", "warning", "-xerror", "-f", "lavfi", "-i", "testsrc2=size=128x128:rate=30:duration=1.6", "-f", "lavfi", "-i", "sine=frequency=750:sample_rate=48000:duration=1.6", "-map", "0:v:0", "-map", "1:a:0", "-threads:v", "1", "-g:v", "3", "-bf:v", "0", "-fps_mode:v", "passthrough", "-vf", "setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709", "-color_range:v", "tv", "-color_primaries:v", "bt709", "-color_trc:v", "bt709", "-colorspace:v", "bt709"}
	if profile.Format.Video.Codec == core.CodecAV1 {
		args = append(args, "-c:v", "libaom-av1", "-cpu-used", "8", "-lag-in-frames", "0", "-enable-cdef", "0", "-b:v", "250000")
	} else {
		args = append(args, "-c:v", "libx264", "-profile:v", "high", "-refs:v", "1", "-sc_threshold", "0", "-b:v", "250000")
	}
	args = append(args, "-c:a", "aac", "-b:a", "64000", "-ar:a", "48000", "-ac:a", "2", "-f", "mp4", "-movflags", "delay_moov+default_base_moof+frag_every_frame", "-min_frag_duration", "1", "-use_editlist", "1", "-movie_timescale", "48000", "-avoid_negative_ts", "disabled", "pipe:1")
	// independent содержит fMP4 независимо закодированного LIVE-источника.
	independent := mediaCommand(t, binary, nil, args...)
	// fallbackSource содержит независимо разобранные пакеты подготовленной заглушки и позиции их
	// подачи.
	fallbackSource := fallbackActualPackets(t, probe, prepared)
	// liveSource содержит независимо разобранные пакеты LIVE-кодирования и позиции их подачи.
	liveSource := fallbackActualPackets(t, probe, independent)
	if bytes.Equal(fallbackSource.init.Video.Headers(), liveSource.init.Video.Headers()) {
		t.Fatal("независимые кодирования должны проверять переход между различными actual headers")
	}
	// queue ограничивает накопление выходных пакетов переключателя и срок повторной синхронизации.
	queue := media.QueueLimits{MaxPackets: 512, MaxBytes: 8 << 20, MaxPacketBytes: 1 << 20, MaxConfigBytes: 1 << 12, ResyncTimeout: 5 * time.Second}
	// watchdog задаёт сроки пропадания, восстановления и ожидания ключевого кадра для управляемых
	// переходов.
	watchdog := core.WatchdogPolicy{VideoTimeout: 100 * time.Millisecond, AudioTimeout: 100 * time.Millisecond, RecoveryWindow: 100 * time.Millisecond, KeyframeTimeout: time.Second}
	// sw переключает подготовленные источники в синтетическом времени, а err сообщает об ошибке его
	// создания.
	sw, err := group.NewSwitcher(group.SwitcherConfig{Profile: profile, Watchdog: watchdog, Queue: queue, Router: media.RouterLimits{MaxConsumers: 2, MaxRetainedBytes: 20 << 20, MaxConfigBytes: 1 << 12}, MaxAVSkew: 100 * time.Millisecond, MaxTransitionGap: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// Закрывает переключатель после проверки переходов между независимо кодированными источниками.
		_ = sw.Close()
	}()
	// sub читает пакеты общего выхода переключателя для проверки переходов между источниками.
	sub, err := sw.Router().Subscribe(queue)
	if err != nil {
		t.Fatal(err)
	}
	// sources сопоставляет режим источника с его пакетами, конфигурациями и текущими позициями
	// подачи.
	sources := map[group.Feed]*fallbackActualSource{group.Fallback: &fallbackSource, group.Live: &liveSource}
	// captured накапливает проверенные выходные пакеты для последующего декодирования смешанного
	// потока.
	var captured []media.Packet
	// shifts хранит допустимые общие сдвиги A/V в секундах для каждой выходной эпохи.
	shifts := map[uint64][]*big.Rat{}
	// epochs запоминает источник, которому принадлежит каждая уже встреченная выходная эпоха.
	epochs := map[uint64]group.Feed{}
	// last хранит предыдущий DTS в секундах отдельно для видео и аудио.
	last := map[media.Track]*big.Rat{}
	// horizon содержит наибольшее время окончания уже выданного пакета в секундах.
	horizon := new(big.Rat)
	// seenHorizon показывает, определена ли временная граница по хотя бы одному выходному пакету.
	seenHorizon := false
	// lastEpoch хранит эпоху последнего проверенного пакета для обнаружения границы переключения.
	lastEpoch := uint64(0)
	// modes накапливает последовательность наблюдавшихся смен активного источника.
	modes := []group.Feed{}
	// appendOutput проверяет выходной пакет относительно выбранного источника и сохраняет его для
	// независимого декодирования.
	appendOutput := func(p media.Packet, feed group.Feed) {
		// Сверяет выходной пакет с исходным источником и проверяет общий A/V-сдвиг, непрерывность DTS и предел
		// разрыва между эпохами.

		// source содержит исходные пакеты режима, которому приписан проверяемый выходной пакет.
		source := sources[feed]
		// index выбирает видеоряд 0 либо аудиоряд 1 для сопоставления пакета с исходником.
		index := 0
		if p.Config().Info().Track == media.TrackAudio {
			index = 1
		}
		// stamp содержит временные метки, длительность и флаг ключевого кадра проверяемого
		// пакета.
		stamp := p.Info()
		// base задаёт временную базу выходного пакета для точного перевода меток в секунды.
		base := p.Config().Info().TimeBase
		// dts содержит время декодирования выходного пакета, а pts — время его показа; оба
		// значения выражены точными рациональными секундами.
		dts, pts := fallbackActualTime(stamp.DTS, base), fallbackActualTime(stamp.PTS, base)
		// duration содержит точную длительность выходного пакета в секундах.
		duration := fallbackActualTime(stamp.DurationTicks, base)
		// expected задаёт ожидаемую длительность полного видеокадра либо AAC-пакета в секундах.
		expected := big.NewRat(1, 30)
		if index == 1 {
			expected = big.NewRat(1024, 48000)
		}
		if duration.Cmp(expected) != 0 || stamp.PTS != stamp.DTS {
			t.Fatalf("actual Switcher изменил полный AU/CTS: %+v", stamp)
		}
		// previous содержит предыдущий DTS этой дорожки для проверки строгого возрастания.
		if previous := last[p.Config().Info().Track]; previous != nil && dts.Cmp(previous) <= 0 {
			t.Fatal("actual переход сбросил DTS")
		}
		last[p.Config().Info().Track] = dts
		// epoch определяет выходную эпоху пакета и набор допустимых сдвигов её источника.
		epoch := p.Config().Info().SourceEpoch
		if epoch != lastEpoch {
			if index != 0 || !stamp.Keyframe {
				t.Fatal("новый actual сегмент начался без независимого video AU")
			}
			if seenHorizon {
				// gap содержит разрыв между DTS первого пакета новой эпохи и прежней
				// временной границей в секундах.
				gap := new(big.Rat).Sub(dts, horizon)
				if gap.Sign() < 0 || gap.Cmp(big.NewRat(1, 10)) > 0 {
					t.Fatalf("actual переход нарушил horizon/gap: %s", gap)
				}
			}
			epochs[epoch] = feed
			lastEpoch = epoch
		}
		// end содержит время окончания показа пакета для продвижения общей границы A/V.
		end := new(big.Rat).Add(pts, duration)
		if !seenHorizon || end.Cmp(horizon) > 0 {
			horizon = end
			seenHorizon = true
		}
		// candidates собирает общие сдвиги времени, согласующие этот пакет с ранее проверенными
		// пакетами эпохи.
		var candidates []*big.Rat
		// input обозначает очередной исходный пакет той же дорожки для поиска совпадающей
		// полезной нагрузки.
		for _, input := range source.packets[index] {
			if !bytes.Equal(input.Payload(), p.Payload()) {
				continue
			}
			// inputStamp содержит временные метки совпавшего по содержимому исходного
			// пакета.
			inputStamp := input.Info()
			// inputBase задаёт временную базу исходного пакета для сопоставления DTS и PTS.
			inputBase := input.Config().Info().TimeBase
			// offset содержит предполагаемый сдвиг выходного DTS относительно исходного в
			// секундах.
			offset := new(big.Rat).Sub(dts, fallbackActualTime(inputStamp.DTS, inputBase))
			if new(big.Rat).Sub(pts, fallbackActualTime(inputStamp.PTS, inputBase)).Cmp(offset) != 0 {
				continue
			}
			// prior содержит допустимые сдвиги ранее проверенных пакетов этой эпохи, а ok
			// показывает наличие таких ограничений.
			if prior, ok := shifts[epoch]; ok {
				// value обозначает очередной ранее допустимый сдвиг эпохи для
				// пересечения с текущим кандидатом.
				for _, value := range prior {
					if value.Cmp(offset) == 0 {
						candidates = append(candidates, offset)
						break
					}
				}
			} else {
				candidates = append(candidates, offset)
			}
		}
		if len(candidates) == 0 {
			t.Fatal("нет единого точного A/V offset, сохраняющего actual payload/PTS/DTS")
		}
		shifts[epoch] = candidates
		captured = append(captured, p)
	}
	// now задаёт начало синтетической шкалы времени управления переключателем.
	now := time.Unix(2000, 0)
	// pushSource подаёт непереданные пакеты выбранного источника до конца указанного видеоинтервала.
	pushSource := func(feed group.Feed, frame int, at time.Time) {
		// Подаёт пакеты выбранного источника до конца заданного видеоинтервала, сохраняя независимые позиции
		// дорожек.

		// source содержит пакеты и изменяемые позиции подачи выбранного источника.
		source := sources[feed]
		// end задаёт исключающую верхнюю границу DTS подаваемого интервала в секундах.
		end := big.NewRat(int64(frame+1), 30)
		// track выбирает очередь видео 0 либо аудио 1 для независимого продвижения источника.
		for track := range 2 {
			for source.next[track] < len(source.packets[track]) {
				// p содержит следующий ещё не поданный пакет выбранной дорожки.
				p := source.packets[track][source.next[track]]
				if fallbackActualTime(p.Info().DTS, p.Config().Info().TimeBase).Cmp(end) >= 0 {
					break
				}
				source.next[track]++
				// e сообщает об отказе подачи пакета переключателю; временная
				// недоступность источника допускается сценарием.
				if e := sw.Push(feed, 1, p, source.init.Video, source.init.Audio, at); e != nil && !errors.Is(e, group.ErrUnavailable) {
					t.Fatalf("actual %v: %v", feed, e)
				}
			}
		}
	}
	// frame задаёт номер одного из 48 видеоинтервалов, управляющих переходами и подачей источников.
	for frame := range 48 {
		// at содержит синтетическое время текущего видеоинтервала для паузы и контроля
		// источников.
		at := now.Add(time.Duration(frame) * time.Second / 30)
		if frame == 24 {
			_ = sw.SetPaused(true, at)
		}
		if frame == 32 {
			_ = sw.SetPaused(false, at)
		}
		pushSource(group.Fallback, frame, at)
		if frame >= 10 && frame < 39 {
			pushSource(group.Live, frame-10, at)
		}
		_ = sw.Tick(at)
		// status содержит состояние переключателя после подачи пакетов и шага контроля времени.
		status := sw.Status()
		if !status.Available {
			continue
		}
		if len(modes) == 0 || modes[len(modes)-1] != status.Source {
			modes = append(modes, status.Source)
		}
		// Push публикует синхронно; ограниченное ожидание Next завершает опустошение очереди.
		// ctx ограничивает ожидание очереди выхода десятью миллисекундами, а cancel освобождает
		// таймер после её опустошения.
		ctx, cancel := context.WithTimeout(context.Background(), 10*time.Millisecond)
		for {
			// d содержит очередную доставку переключателя, а e отличает окончание
			// ограниченного ожидания от ошибки чтения.
			d, e := sub.Next(ctx)
			if errors.Is(e, context.DeadlineExceeded) {
				break
			}
			if e != nil {
				cancel()
				t.Fatalf("actual output: %v", e)
			}
			// p содержит выходной пакет доставки для сопоставления с источником его эпохи.
			p := d.Packet
			appendOutput(p, epochsOrCurrent(epochs, p.Config().Info().SourceEpoch, status.Source))
		}
		cancel()
	}
	if len(modes) < 5 || modes[0] != group.Fallback || modes[1] != group.Live || modes[2] != group.Fallback || modes[3] != group.Live || modes[len(modes)-1] != group.Fallback {
		t.Fatalf("не выполнены actual recovery/pause/resume/watchdog переходы: %v", modes)
	}
	// epoch обозначает каждую встретившуюся эпоху для проверки найденного общего сдвига A/V.
	for epoch := range epochs {
		if len(shifts[epoch]) == 0 {
			t.Fatal("не проверен общий A/V offset")
		}
	}
	fallbackDecodeMixedElementary(t, binary, profile, captured)
}

// Возвращает уже закреплённый за выходной эпохой источник, а для ещё не встреченной эпохи использует
// текущий источник Switcher.
func epochsOrCurrent(epochs map[uint64]group.Feed, epoch uint64, current group.Feed) group.Feed {
	// feed содержит уже закреплённый за эпохой источник, а ok показывает наличие такого закрепления.
	if feed, ok := epochs[epoch]; ok {
		return feed
	}
	return current
}

// Собирает элементарные видео- и AAC-потоки из захваченных пакетов и декодирует их FFmpeg. Проверяет
// количество полных кадров и сэмплов и сохранение независимого тона 750 Hz после переключений
// источников.
func fallbackDecodeMixedElementary(t *testing.T, binary string, profile core.GroupProfile, packets []media.Packet) {
	t.Helper()
	// video накапливает элементарный видеопоток, а audio — AAC в ADTS для независимого декодирования
	// FFmpeg.
	var video, audio bytes.Buffer
	// videos считает захваченные видеокадры, а audios — полные AAC-пакеты для проверки объёма
	// декодированных данных.
	videos, audios := 0, 0
	// epoch хранит предыдущую видеоэпоху, чтобы добавлять SPS/PPS при смене H.264-источника.
	epoch := uint64(0)
	// p обозначает очередной захваченный пакет смешанного потока.
	for _, p := range packets {
		// c содержит конфигурацию дорожки текущего захваченного пакета.
		c := p.Config()
		if c.Info().Track == media.TrackVideo {
			videos++
			if profile.Format.Video.Codec == core.CodecAV1 {
				// MP4 уже задал границу полного TU; демультиплексор OBU получает её как пустой Temporal Delimiter.
				// payload содержит AV1 Temporal Unit для добавления границы перед
				// записью в элементарный поток.
				payload := p.Payload()
				if payload[0]>>3&15 != 2 {
					video.Write([]byte{0x12, 0})
				}
				video.Write(payload)
			} else {
				if c.Info().SourceEpoch != epoch {
					// headers содержит avcC новой эпохи для извлечения SPS/PPS в
					// виде Annex B.
					headers := c.Headers()
					if len(headers) < 8 {
						t.Fatal("обрезан avcC")
					}
					// position указывает следующий непрочитанный байт avcC при
					// извлечении наборов параметров.
					position := 6
					// count задаёт число SPS или PPS, обрабатываемых на текущем
					// проходе avcC.
					count := int(headers[5] & 31)
					// pass выбирает первый проход SPS либо второй проход PPS.
					for pass := range 2 {
						// i задаёт порядковый номер набора параметров внутри
						// текущего раздела avcC.
						for i := 0; i < count; i++ {
							if len(headers)-position < 2 {
								t.Fatal("обрезана длина avcC")
							}
							// n содержит длину очередного SPS/PPS в байтах для
							// проверки границ avcC.
							n := int(encodingbinary.BigEndian.Uint16(headers[position:]))
							position += 2
							if len(headers)-position < n {
								t.Fatal("обрезан SPS/PPS")
							}
							video.Write([]byte{0, 0, 0, 1})
							video.Write(headers[position : position+n])
							position += n
						}
						if pass == 0 {
							if position >= len(headers) {
								t.Fatal("нет PPS")
							}
							count = int(headers[position])
							position++
						}
					}
				}
				// data содержит ещё не преобразованные NAL-единицы видеопакета с
				// префиксами длины AVCC.
				data := p.Payload()
				for len(data) > 0 {
					if len(data) < 4 {
						t.Fatal("обрезан AVCC NAL")
					}
					// n содержит длину очередной NAL-единицы в байтах для
					// преобразования AVCC в Annex B.
					n := int(encodingbinary.BigEndian.Uint32(data))
					data = data[4:]
					if n <= 0 || n > len(data) {
						t.Fatal("неверная длина NAL")
					}
					video.Write([]byte{0, 0, 0, 1})
					video.Write(data[:n])
					data = data[n:]
				}
			}
			epoch = c.Info().SourceEpoch
			continue
		}
		// actual содержит формат AAC, разобранный из ASC, а err сообщает об ошибке разбора перед
		// сравнением с профилем.
		actual, err := codec.AudioFormatASC(c.Headers())
		if err != nil || actual != profile.Format.Audio {
			t.Fatalf("actual ASC: %+v %v", actual, err)
		}
		// headers содержит ASC аудиопакета для построения совместимого заголовка ADTS.
		headers := c.Headers()
		// objectType задаёт тип аудиообъекта из ASC; проверка требует AAC LC со значением 2.
		objectType := headers[0] >> 3
		// rateIndex содержит индекс частоты дискретизации ASC для переноса в ADTS.
		rateIndex := (headers[0]&7)<<1 | headers[1]>>7
		// channels содержит конфигурацию каналов ASC, допустимую для формирования ADTS.
		channels := headers[1] >> 3 & 15
		if objectType != 2 || rateIndex > 12 || channels == 0 || channels > 7 {
			t.Fatal("ASC не представим в ADTS")
		}
		// payload содержит полную AAC-единицу доступа без заголовка ADTS.
		payload := p.Payload()
		// length содержит размер ADTS-кадра вместе с семибайтовым заголовком.
		length := len(payload) + 7
		if length > 8191 {
			t.Fatal("AU превышает ADTS length")
		}
		audio.Write([]byte{0xff, 0xf1, (objectType-1)<<6 | rateIndex<<2 | channels>>2, (channels&3)<<6 | byte(length>>11), byte(length >> 3), byte(length&7)<<5 | 31, 0xfc})
		audio.Write(payload)
		audios++
	}
	// format задаёт входной формат элементарного видеопотока для декодера FFmpeg.
	format := "h264"
	if profile.Format.Video.Codec == core.CodecAV1 {
		format = "obu"
	}
	// decodedVideo содержит декодированные серые кадры для проверки их полного количества.
	decodedVideo := mediaCommand(t, binary, video.Bytes(), "-hide_banner", "-loglevel", "error", "-xerror", "-f", format, "-i", "pipe:0", "-map", "0:v:0", "-fps_mode", "passthrough", "-pix_fmt", "gray", "-f", "rawvideo", "pipe:1")
	if len(decodedVideo) != videos*128*128 {
		t.Fatalf("mixed actual video decode count: bytes=%d frames=%d", len(decodedVideo), videos)
	}
	// pcm содержит декодированный стереозвук s16le для проверки числа сэмплов и сохранения тона.
	pcm := mediaCommand(t, binary, audio.Bytes(), "-hide_banner", "-loglevel", "error", "-xerror", "-flags2:a", "+skip_manual", "-f", "aac", "-i", "pipe:0", "-map", "0:a:0", "-f", "s16le", "pipe:1")
	if len(pcm) != audios*1024*2*2 {
		t.Fatalf("mixed actual AAC sample count: bytes=%d AU=%d", len(pcm), audios)
	}
	// energy750 накапливает энергию тона 750 Гц, energy500 — 500 Гц, а energy1000 — 1000 Гц для
	// сравнения сохранённого LIVE-звука.
	var energy750, energy500, energy1000 float64
	// block задаёт номер полного AAC-интервала из 1024 сэмплов в декодированном PCM.
	for block := 0; block < audios; block++ {
		// s750 и c750 накапливают синусную и косинусную проекции на 750 Гц; s500 и c500 — на 500
		// Гц; s1000 и c1000 — на 1000 Гц в текущем AAC-интервале.
		var s750, c750, s500, c500, s1000, c1000 float64
		// n задаёт позицию сэмпла внутри текущего интервала из 1024 отсчётов.
		for n := range 1024 {
			// sample содержит знаковый 16-битный отсчёт первого канала декодированного PCM.
			sample := float64(int16(encodingbinary.LittleEndian.Uint16(pcm[(block*1024+n)*4:])))
			// x задаёт фазу одного герца для текущей позиции сэмпла при частоте 48000 Гц.
			x := 2 * math.Pi * float64(n) / 48000
			s750 += sample * math.Sin(750*x)
			c750 += sample * math.Cos(750*x)
			s500 += sample * math.Sin(500*x)
			c500 += sample * math.Cos(500*x)
			s1000 += sample * math.Sin(1000*x)
			c1000 += sample * math.Cos(1000*x)
		}
		energy750 += s750*s750 + c750*c750
		energy500 += s500*s500 + c500*c500
		energy1000 += s1000*s1000 + c1000*c1000
	}
	if energy750 == 0 || energy750 <= energy500+energy1000 {
		t.Fatalf("actual mixed AAC потерял независимый тон 750 Hz: energy=%g/%g/%g", energy750, energy500, energy1000)
	}
}

// Run запускает настоящий ограниченный Runner и при ошибке добавляет в журнал теста команду и
// диагностику процесса; результат и ошибка возвращаются без изменения.
func (r fallbackPipelineRunner) Run(ctx context.Context, c fallback.Command, f *os.File, w io.Writer) (fallback.RunResult, error) {
	// result содержит результат и диагностику ограниченного процесса, а err сообщает об ошибке
	// запуска или исполнения команды.
	result, err := r.runner.Run(ctx, c, f, w)
	if err != nil {
		r.t.Logf("finite process binary=%s args=%q error=%v diagnostics=%s", c.Binary, c.Args, err, result.Diagnostics)
	}
	return result, err
}

// При заданном GOST_MEDIA_FFMPEG проверяет загрузку изображения, подготовку заглушки и циклическое
// воспроизведение через файловый проигрыватель, UDS и Group. Проверяет переходы с независимым
// LIVE-источником, точные временные шаги и декодирование AAC-тишины в нулевой PCM.
func TestFallbackPipelineCPUGenerationFDLoopsAndDecode(t *testing.T) {
	// binary содержит явно заданный FFmpeg для необязательной CPU-проверки полного тракта заглушки.
	binary := os.Getenv("GOST_MEDIA_FFMPEG")
	if binary == "" {
		t.Skip("короткая CPU-проверка требует явно заданного GOST_MEDIA_FFMPEG")
	}
	// scenario выбирает кодек, рецепт генерации и длительность тестового изображения.
	for _, scenario := range []struct {
		// name задаёт имя подтеста генерации и проигрывания заглушки.
		name string
		// codec выбирает AV1 либо H.264 для подготовленного варианта.
		codec core.Codec
		// production включает штатный рецепт SVT-AV1 вместо CPU-подстановки генератора.
		production bool
		// imageDuration задаёт длительность изображения, передаваемую проверке и подготовке
		// заглушки.
		imageDuration time.Duration
	}{{"av1-cpu", core.CodecAV1, false, 1600 * time.Millisecond}, {"h264-cpu", core.CodecH264, false, 1600 * time.Millisecond}, {"av1-production-svt", core.CodecAV1, true, 1600 * time.Millisecond}, {"h264-short-image", core.CodecH264, false, time.Second}} {
		t.Run(scenario.name, func(t *testing.T) {
			// Проверяет выбранный генератор и длительность изображения через подготовку, переходы источников,
			// циклический проигрыватель и декодирование.

			// ctx ограничивает полный CPU-сценарий двадцатью секундами, а cancel освобождает
			// его таймер.
			ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
			defer cancel()
			// options задаёт бюджеты выполнения, наблюдений, пула соединений и повторов
			// временной SQLite.
			options := storage.Options{PreparationLimits: core.PreparationLimits{MaxQueued: 3, MaxProfiles: 8}, OutputExecutionLimits: core.OutputExecutionLimits{MaxReservations: 128, MaxIdentifierBytes: 1024, MaxBytes: 4 << 20}, ObservationLimits: core.ObservationLimits{MaxSessions: 8, MaxGroups: 32, MaxOutputs: 64, MaxFFmpegWorkers: 32, MaxIdentifierBytes: 1024, MaxErrorBytes: 65536, MaxBytes: 4 << 20}, MaxOpenConns: 4, MaxIdleConns: 4, ConnMaxLifetime: time.Minute, ConnMaxIdleTime: time.Minute, BusyTimeout: 10 * time.Millisecond, RetryAttempts: 3, RetryTimeout: time.Second, RetryDelay: time.Millisecond}
			// store содержит временное файловое хранилище сценария, а err сообщает об ошибке
			// его открытия.
			store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "pipeline.sqlite"), options)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				// Закрывает временное хранилище сценария подготовки заглушки.

				// err сообщает об ошибке закрытия временного хранилища сценария.
				if err := store.Close(); err != nil {
					t.Error(err)
				}
			})
			// svc выполняет доменные операции подготовки и запуска сессии поверх временного
			// хранилища.
			svc, err := core.NewService(store, store.Incarnation())
			if err != nil {
				t.Fatal(err)
			}
			// asset содержит заглушку, в которую загружается синтетическое изображение.
			asset, err := svc.CreateAsset(ctx, "CPU-заглушка")
			if err != nil {
				t.Fatal(err)
			}
			// watchdog задаёт сроки ожидания дорожек, восстановления и ключевого кадра
			// рабочей группы.
			watchdog := core.WatchdogPolicy{VideoTimeout: 2 * time.Second, AudioTimeout: 2 * time.Second, RecoveryWindow: 100 * time.Millisecond, KeyframeTimeout: time.Second}
			// stream содержит поток с тестовой заглушкой и политикой контроля источника.
			stream, err := svc.CreateStream(ctx, core.Stream{Name: "CPU-поток", PublicationKey: "cpu-fallback-key", Fallback: core.FallbackPolicy{AssetID: asset.ID, Watchdog: watchdog}})
			if err != nil {
				t.Fatal(err)
			}
			// profile содержит сохранённый профиль подготовки для выбранного кодека.
			profile, err := svc.CreateProfile(ctx, core.TranscodeProfile{Name: "CPU-профиль", Profile: fallbackPipelineProfile(scenario.codec)})
			if err != nil {
				t.Fatal(err)
			}
			// configured содержит группу, связывающую тестовый поток с выбранным профилем.
			configured, err := svc.CreateGroup(ctx, core.TranscodeGroup{Name: "CPU-группа", StreamID: stream.ID, ProfileID: profile.ID})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = svc.CreateDestination(ctx, core.Destination{Name: "CPU-выход", StreamID: stream.ID, GroupID: configured.ID, Platform: "custom", Endpoint: "rtmp://example.invalid/live", Enabled: true}); err != nil {
				t.Fatal(err)
			}
			// files хранит исходное изображение и подготовленные варианты во временном
			// каталоге.
			files, err := fallback.OpenFileStore(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				// Освобождает файловое хранилище после остановки подготовки и проигрывания.

				// err сообщает об ошибке закрытия файлового хранилища подготовки.
				if err := files.Close(); err != nil {
					t.Error(err)
				}
			})
			// runner запускает подготовку с ограничениями времени, аргументов и
			// диагностического вывода.
			runner := fallback.Runner{Limits: fallback.RunnerLimits{Timeout: 10 * time.Second, MaxArgs: 256, MaxCommandBytes: 1 << 16, MaxDiagnosticBytes: 1 << 20}}
			// 48 кадров при 30 FPS и 75 AAC AU по 1024 сэмпла при 48 kHz занимают ровно 1,6 секунды.
			// probeLimits задаёт границы проверки контейнера и длительность изображения
			// текущего сценария.
			probeLimits := fallback.ProbeLimits{MaxJSONBytes: 1 << 20, MaxWidth: 1024, MaxHeight: 1024, MaxPixels: 1024 * 1024, MaxDuration: 10 * time.Second, ImageDuration: scenario.imageDuration, MaxPackets: 4096, Container: integrationLimits()}
			// actualRunner добавляет диагностику теста к исполнению рабочих медиакоманд.
			actualRunner := fallbackPipelineRunner{t: t, runner: runner}
			// probe проверяет исходные и подготовленные файлы через ffprobe рядом с заданным
			// FFmpeg.
			probe := fallback.Prober{Binary: filepath.Join(filepath.Dir(binary), "ffprobe"), Runner: actualRunner, Limits: probeLimits}
			// config задаёт версию генератора и бюджеты загрузки, подготовки и завершения
			// менеджера.
			config := fallback.Config{ReconcileLimit: 10, GeneratorVersion: "cpu-integration", FFmpeg: binary, FFprobe: probe.Binary, MaxUploadBytes: 1 << 20, MaxOutputBytes: 8 << 20, PrepareTimeout: 15 * time.Second, ShutdownTimeout: 3 * time.Second, QueueInterval: time.Millisecond, Runner: runner.Limits, Probe: probeLimits}
			// builder выбирает CPU-подстановку либо штатный рецепт генерации для текущего
			// сценария.
			builder := fallbackPipelineCPUBuilder(binary)
			if scenario.production {
				builder = fallback.ProductionBuilder(binary)
			}
			// manager выполняет загрузку изображения и публикацию проверенного поколения
			// заглушки.
			manager, err := fallback.NewManagerWithTools(svc, files, config, fallback.Tools{Builder: builder, Inspector: probe, Runner: actualRunner, Normalize: probe.NormalizePrepared, VerifyLoopDuration: probe.VerifyLoopDuration})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				// Закрывает менеджер подготовки после проверки его опубликованного поколения.
				_ = manager.Close(context.Background())
			})
			// img содержит синтетическое непрозрачное изображение размером 128×128 для
			// подготовки заглушки.
			img := image.NewRGBA(image.Rect(0, 0, 128, 128))
			// y задаёт строку пикселей и зелёную составляющую синтетического изображения.
			for y := range 128 {
				// x задаёт столбец пикселя и красную составляющую синтетического
				// изображения.
				for x := range 128 {
					img.SetRGBA(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 40, A: 255})
				}
			}
			// pngData содержит PNG-байты изображения для загрузки в менеджер.
			var pngData bytes.Buffer
			if err = png.Encode(&pngData, img); err != nil {
				t.Fatal(err)
			}
			// job представляет принятую задачу загрузки и подготовки изображения.
			job, err := manager.Upload(ctx, core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage}, io.NopCloser(bytes.NewReader(pngData.Bytes())))
			if err != nil {
				t.Fatal(err)
			}
			// result содержит опубликованное поколение и признак подтверждённой очистки
			// подготовки.
			result, err := job.Wait(ctx)
			if err != nil || !result.Clean {
				t.Fatalf("рабочие upload/generator/probe/publication: generation=%s clean=%v error=%v", result.Generation.ID, result.Clean, err)
			}
			if len(result.Generation.RequiredProfiles) != 1 {
				t.Fatal("не подготовлен точный профиль")
			}
			// prepared содержит единственную подготовленную привязку профиля поколения к
			// варианту.
			prepared := result.Generation.RequiredProfiles[0]
			// variant содержит точные свойства подготовленного файла для его проверенного
			// открытия.
			variant, err := svc.GetVariant(ctx, prepared.VariantID)
			if err != nil {
				t.Fatal(err)
			}
			// preparedFile содержит открытый вариант с проверкой пути, размера и хеша.
			preparedFile, err := files.OpenExact(ctx, variant.ManagedPath, variant.SizeBytes, variant.ContentHash)
			if err != nil {
				t.Fatal(err)
			}
			// preparedBytes содержит байты подготовленного fMP4, а readErr сообщает об
			// ошибке их чтения.
			preparedBytes, readErr := io.ReadAll(preparedFile)
			// closeErr сообщает об ошибке закрытия дескриптора подготовленного варианта.
			closeErr := preparedFile.Close()
			if readErr != nil || closeErr != nil {
				t.Fatal(errors.Join(readErr, closeErr))
			}
			t.Logf("prepared packet oracle: %s", mediaCommand(t, probe.Binary, preparedBytes, "-v", "error", "-select_streams", "v:0", "-read_intervals", "%+#3", "-show_entries", "packet=pts,dts,duration,time_base", "-of", "json", "pipe:0"))
			// audioOracle содержит независимые данные ffprobe о пакетах, дорожках и
			// контейнере подготовленного AAC для диагностики.
			var audioOracle struct {
				// Packets содержит JSON каждого аудиопакета для вывода первого,
				// последнего и общего количества.
				Packets []json.RawMessage `json:"packets"`
				// Streams содержит исходное JSON-описание аудиодорожки с временной базой
				// и длительностью.
				Streams json.RawMessage `json:"streams"`
				// Format содержит исходное JSON-описание длительности контейнера.
				Format json.RawMessage `json:"format"`
			}
			if err = json.Unmarshal(mediaCommand(t, probe.Binary, preparedBytes, "-v", "error", "-select_streams", "a:0", "-show_entries", "stream=time_base,start_pts,duration_ts:format=duration:packet=pts,dts,duration,side_data_list:packet_side_data=side_data_type,skip_samples,discard_padding,skip_reason,discard_reason", "-of", "json", "pipe:0"), &audioOracle); err != nil {
				t.Fatal(err)
			}
			if len(audioOracle.Packets) > 0 {
				t.Logf("prepared AAC: count=%d first=%s last=%s streams=%s format=%s", len(audioOracle.Packets), audioOracle.Packets[0], audioOracle.Packets[len(audioOracle.Packets)-1], audioOracle.Streams, audioOracle.Format)
			}
			fallbackPipelineActualTransitions(t, binary, probe, profile.Profile, preparedBytes)
			// request связывает запуск сессии с точным поколением, вариантом и версией
			// генератора.
			request := core.StartSessionRequest{RequestID: "cpu-pipeline-start", StreamID: stream.ID, Bindings: []core.PreparedBinding{{GroupID: configured.ID, GenerationID: result.Generation.ID, VariantID: prepared.VariantID, GeneratorVersion: config.GeneratorVersion}}}
			// start содержит квитанцию запуска для получения родительского разрешения
			// сессии.
			start, err := svc.StartSession(ctx, request)
			if err != nil {
				t.Fatal(err)
			}
			// parent содержит разрешение сессии для реестра групп и его наблюдателя.
			parent, err := svc.Claim(ctx, start.RequestID)
			if err != nil {
				t.Fatal(err)
			}
			if err = svc.MarkIssued(ctx, parent); err != nil {
				t.Fatal(err)
			}
			if err = svc.Complete(ctx, parent, core.Completion{Success: true}); err != nil {
				t.Fatal(err)
			}
			// observer публикует наблюдения групп в рамках текущего выполнения сессии.
			observer, err := svc.OpenSessionObserver(ctx, parent)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				// Закрывает наблюдателя Session после завершения выходной группы.
				_ = observer.Close(context.Background())
			})
			// outputs содержит сохранённый снимок единственного выхода для получения ссылки
			// на группу.
			outputs, err := svc.ListOutputs(ctx, core.PageRequest{Query: core.ListQuery{Limit: 10, SessionID: parent.SessionID}})
			if err != nil || len(outputs.Items) != 1 {
				t.Fatalf("снимок выхода: %+v, %v", outputs, err)
			}

			// parentDirectory задаёт родительский каталог коротких путей UDS; на macOS
			// используется /tmp.
			parentDirectory := os.TempDir()
			if runtime.GOOS == "darwin" {
				parentDirectory = "/tmp"
			}
			// base задаёт отдельный короткий каталог UDS и рабочих файлов проигрывателя.
			base, err := os.MkdirTemp(parentDirectory, "fallback-cpu-")
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				// Удаляет короткий каталог UDS после остановки проигрывателя.

				// err сообщает об ошибке удаления короткого каталога проигрывателя.
				if err := os.RemoveAll(base); err != nil {
					t.Error(err)
				}
			})
			// queue ограничивает число и объём пакетов подписки и срок повторной
			// синхронизации.
			queue := media.QueueLimits{MaxPackets: 512, MaxBytes: 8 << 20, MaxPacketBytes: 1 << 20, MaxConfigBytes: 1 << 12, ResyncTimeout: 5 * time.Second}
			// routerLimits ограничивает число потребителей и удерживаемые байты
			// медиамаршрутизаторов.
			routerLimits := media.RouterLimits{MaxConsumers: 4, MaxRetainedBytes: 40 << 20, MaxConfigBytes: 1 << 12}
			// live предоставляет вход LIVE, пока проверяется непрерывное проигрывание
			// заглушки.
			live, err := media.NewRouter(routerLimits)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				// Закрывает входной маршрутизатор после освобождения группы заглушки.
				_ = live.Close()
			})
			// runtimeConfig задаёт рабочий проигрыватель, ограничения контейнера и временные
			// бюджеты группы.
			runtimeConfig := group.RuntimeConfig{Switcher: group.SwitcherConfig{Profile: profile.Profile, Watchdog: watchdog, Queue: queue, Router: routerLimits, MaxAVSkew: 100 * time.Millisecond, MaxTransitionGap: 100 * time.Millisecond}, Binary: binary, OpenFile: files.OpenVerified, Container: integrationLimits(), ResubscribeBackoff: time.Millisecond, ControlInterval: 10 * time.Millisecond, ShutdownTimeout: 3 * time.Second, FFmpeg: ffmpeg.Config{BaseDir: base, StartupTimeout: 5 * time.Second, VideoTimeout: 2 * time.Second, AudioTimeout: 2 * time.Second, WatchdogInterval: 20 * time.Millisecond, RestartBackoff: 10 * time.Millisecond, GracePeriod: 100 * time.Millisecond, KillTimeout: time.Second, ShutdownTimeout: 3 * time.Second, DiagnosticEvents: 128, DiagnosticBytes: 1 << 20, DiagnosticChunkBytes: 1024, MaxCommandBytes: 8192, MaxCommandArgs: 128}}
			// registry создаёт и удерживает группу с точным родительским разрешением сессии.
			registry, err := group.NewRegistry(ctx, group.RegistryConfig{Core: svc, Parent: parent, Observer: observer, Runtime: runtimeConfig, MaxGroups: 4, MaxReferences: 4})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				// Закрывает реестр групп перед освобождением наблюдателя и файлового хранилища.

				// err сообщает об ошибке завершения реестра групп и проигрывателей.
				if err := registry.Close(context.Background()); err != nil {
					t.Error(err)
				}
			})
			// ref удерживает проверяемую группу и предоставляет её выходной маршрутизатор.
			ref, err := registry.Acquire(ctx, outputs.Items[0].Snapshot, live)
			if err != nil {
				t.Fatal(err)
			}
			// sub читает выход группы для проверки непрерывных петель подготовленного
			// варианта.
			sub, err := ref.Router().Subscribe(queue)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				// Закрывает подписку на выход группы после проверки непрерывных петель.
				_ = sub.Close()
			}()
			// captured накапливает fMP4 проверенных выходных пакетов для независимого
			// декодирования.
			var captured bytes.Buffer
			// mux записывает актуальные пакеты первой выходной эпохи в общий захваченный
			// контейнер.
			var mux *fmp4.Muxer
			// counts считает проверенные видео- и аудиопакеты для контроля достаточной длины
			// проигрывания.
			counts := map[media.Track]int{}
			// last хранит предыдущий DTS каждой дорожки в секундах для проверки точного шага
			// на границах петель.
			last := map[media.Track]*big.Rat{}
			// firstPTS сохраняет первые времена показа обеих дорожек для проверки начального
			// сдвига A/V.
			firstPTS := map[media.Track]*big.Rat{}
			// epoch фиксирует первую выходную эпоху для проверки её сохранения между
			// петлями.
			var epoch uint64
			for counts[media.TrackVideo] < 150 {
				// delivery содержит следующую доставку группы, а err сообщает об отказе
				// или истечении срока её ожидания.
				delivery, err := sub.Next(ctx)
				if err != nil {
					t.Fatalf("FD-player/UDS/Group: %v status=%+v", err, ref.Group().Status())
				}
				// p содержит пакет текущей доставки для временных проверок и записи в
				// контейнер.
				p := delivery.Packet
				// track определяет дорожку текущего пакета и её отдельные счётчики.
				track := p.Config().Info().Track
				// info содержит временные метки и длительность текущего выходного
				// пакета.
				info := p.Info()
				// tb задаёт временную базу текущего пакета для точного перевода меток в
				// секунды.
				tb := p.Config().Info().TimeBase
				if !delivery.IsCurrent() {
					continue
				}
				if mux == nil {
					mux, err = fmp4.NewMuxer(&captured, fmp4.Init{Video: delivery.VideoConfig, Audio: delivery.AudioConfig}, integrationLimits())
					if err != nil {
						t.Fatal(err)
					}
					epoch = delivery.VideoConfig.Info().SourceEpoch
				}
				if delivery.VideoConfig.Info().SourceEpoch != epoch {
					t.Fatal("непрерывная петля изменила выходную epoch")
				}
				// stamp содержит DTS текущего пакета в точных рациональных секундах.
				stamp := new(big.Rat).SetFrac64(info.DTS*int64(tb.Numerator), int64(tb.Denominator))
				// duration содержит длительность полного пакета в точных рациональных
				// секундах.
				duration := new(big.Rat).SetFrac64(info.DurationTicks*int64(tb.Numerator), int64(tb.Denominator))
				// expected задаёт ожидаемый шаг видео 1/30 секунды либо AAC 1024/48000
				// секунды.
				expected := big.NewRat(1, 30)
				if track == media.TrackAudio {
					expected = big.NewRat(1024, 48000)
				}
				if duration.Cmp(expected) != 0 {
					t.Fatalf("%s[%d] duration=%s expected=%s packet=%+v config=%+v", track, counts[track], duration, expected, info, p.Config().Info())
				}
				// previous содержит DTS предыдущего пакета этой дорожки для проверки
				// непрерывного шага.
				if previous := last[track]; previous != nil {
					// step содержит фактическое расстояние между последовательными
					// DTS дорожки в секундах.
					if step := new(big.Rat).Sub(stamp, previous); step.Cmp(expected) != 0 {
						t.Fatalf("%s шаг/граница петли %d: %s expected=%s", track, counts[track], step, expected)
					}
				}
				last[track] = stamp
				if firstPTS[track] == nil {
					firstPTS[track] = new(big.Rat).SetFrac64(info.PTS*int64(tb.Numerator), int64(tb.Denominator))
				}
				if info.PTS != info.DTS {
					t.Fatal("copy player изменил PTS−DTS источника без B-кадров")
				}
				counts[track]++
				if err = mux.WritePacket(p); err != nil {
					t.Fatal(err)
				}
			}
			if counts[media.TrackAudio] < 100 || ref.Group().Status().Source != group.Fallback {
				t.Fatalf("нет циклического A/V fallback до OBS: counts=%v status=%+v", counts, ref.Group().Status())
			}
			// skew содержит разность первых PTS аудио и видео в секундах для проверки
			// начального согласования.
			skew := new(big.Rat).Sub(firstPTS[media.TrackAudio], firstPTS[media.TrackVideo])
			if skew.Sign() < 0 || skew.Cmp(big.NewRat(1024, 48000)) > 0 {
				t.Fatalf("первые полные A/V AU не согласованы: %s", skew)
			}
			mediaCommand(t, binary, captured.Bytes(), "-hide_banner", "-loglevel", "error", "-xerror", "-flags2:a", "+skip_manual", "-i", "pipe:0", "-map", "0:v:0", "-map", "0:a:0", "-f", "null", "-")
			// pcm содержит декодированную AAC-тишину в формате стерео s16le.
			pcm := mediaCommand(t, binary, captured.Bytes(), "-hide_banner", "-loglevel", "error", "-xerror", "-flags2:a", "+skip_manual", "-i", "pipe:0", "-map", "0:a:0", "-f", "s16le", "pipe:1")
			if len(pcm) != counts[media.TrackAudio]*1024*2*2 {
				t.Fatalf("PCM тишины потерял полные AAC AU: bytes=%d AU=%d", len(pcm), counts[media.TrackAudio])
			}
			// i указывает смещение очередного 16-битного PCM-отсчёта в байтах.
			for i := 0; i < len(pcm); i += 2 {
				// sample содержит знаковый PCM-отсчёт для проверки точного нуля
				// подготовленной тишины.
				if sample := int16(encodingbinary.LittleEndian.Uint16(pcm[i:])); sample != 0 {
					t.Fatalf("подготовленная AAC-тишина содержит ненулевой PCM sample[%d]=%d", i/2, sample)
				}
			}
			if err = ref.Close(); err != nil && !errors.Is(err, context.Canceled) {
				t.Fatal(err)
			}
		})
	}
}
