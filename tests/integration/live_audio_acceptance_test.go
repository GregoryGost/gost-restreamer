package integration

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
	"github.com/GregoryGost/gost-restreamer/internal/media/group"
	"github.com/GregoryGost/gost-restreamer/internal/media/publisher"
)

// При заданном GOST_MEDIA_FFMPEG проверяет цифровую AAC-тишину заглушек из изображения, видео без
// звука и видео со звуком, включая B-кадры. Сверяет полные AU и знаковые временные метки и декодирует
// три последовательных периода без ненулевых сэмплов.
func TestLiveAudioPreparedAssetsDecodeExactSilenceAcrossPeriods(t *testing.T) {
	// executable задаёт явно выбранный FFmpeg для дополнительной проверки цифровой тишины
	// подготовленных материалов.
	executable := os.Getenv("GOST_MEDIA_FFMPEG")
	if executable == "" {
		t.Skip("Локальная CPU-проверка требует GOST_MEDIA_FFMPEG")
	}
	// p задаёт базовый H.264-профиль заглушки с копированием LIVE AAC.
	p := fallbackPipelineProfile(core.CodecH264)
	p.LiveAudioMode = core.LiveAudioCopy
	// kind выбирает изображение, немое видео либо видео с тоном, включая вариант с B-кадрами.
	for _, kind := range []string{"image", "video-no-audio", "video-tone", "video-tone-bframes"} {
		t.Run(kind, func(t *testing.T) {
			// Проверяет цифровую тишину выбранного исходного материала на полном подготовленном периоде и трёх
			// повторениях AAC.

			// profile содержит настройки текущего варианта, включая число B-кадров.
			profile := p
			// sourceKind выбирает фактический вид исходного материала независимо от варианта
			// B-кадров.
			sourceKind := kind
			if kind == "video-tone-bframes" {
				profile.VideoEncoding.MaxBFrames = 2
				sourceKind = "video-tone"
			}
			// accessUnits задаёт ожидаемое число полных AAC AU общего подготовленного
			// периода.
			accessUnits := 75
			if sourceKind == "video-tone" {
				// Контейнер с задержкой AAC длится 1,621333 с; следующий общий период при 30 FPS и 48 kHz равен 3,2 с.
				accessUnits = 150
			}
			// prepared содержит реально подготовленный и нормализованный fMP4 текущего
			// материала заглушки.
			prepared := liveAudioPrepared(t, executable, profile, sourceKind)
			// pcm содержит непрерывно декодированное аудио подготовленного периода для
			// проверки точного числа сэмплов и тишины.
			pcm := mediaCommand(t, executable, prepared, "-v", "error", "-xerror", "-copyts", "-flags2:a", "+skip_manual", "-i", "pipe:0", "-map", "0:a:0", "-f", "f32le", "pipe:1")
			if len(pcm) != accessUnits*1024*2*4 || liveAudioNonzeroSamples(pcm) != 0 {
				t.Fatalf("Подготовленный период содержит звук или неполные AU: bytes=%d nonzero=%d", len(pcm), liveAudioNonzeroSamples(pcm))
			}
			// packets содержит подготовленные AU по дорожкам; err сообщает об ошибке их
			// чтения или проверки формата.
			packets, err := encodingPreparedPackets(prepared, profile.Format)
			if err != nil {
				t.Fatal(err)
			}
			if len(packets[media.TrackAudio]) != accessUnits || len(packets[media.TrackVideo]) != accessUnits*1024*30/48000 {
				t.Fatalf("Неверное число полных AU: video=%d audio=%d", len(packets[media.TrackVideo]), len(packets[media.TrackAudio]))
			}
			// presentation собирает точные PTS видео для независимой проверки состава кадров
			// при B-перестановке.
			var presentation []*big.Rat
			// i обозначает позицию видеопакета в порядке декодирования; packet содержит
			// проверяемый видеопакет заглушки.
			for i, packet := range packets[media.TrackVideo] {
				// stamp содержит времена и длительность видеопакета; base задаёт единицу
				// этих значений.
				stamp, base := packet.Info(), packet.Config().Info().TimeBase
				if fallbackActualTime(stamp.DurationTicks, base).Cmp(big.NewRat(1, 30)) != 0 {
					t.Fatal("Изменена длительность видеокадра")
				}
				// wantDTS задаёт ожидаемую знаковую метку декодирования видео с учётом
				// задержки B-кадров.
				wantDTS := big.NewRat(int64(i), 30)
				if profile.VideoEncoding.MaxBFrames == 2 {
					wantDTS.Sub(wantDTS, big.NewRat(2, 30))
				}
				if fallbackActualTime(stamp.DTS, base).Cmp(wantDTS) != 0 {
					t.Fatalf("Изменён знаковый DTS видео[%d]: %+v != %s", i, stamp, wantDTS)
				}
				presentation = append(presentation, fallbackActualTime(stamp.PTS, base))
			}
			slices.SortFunc(presentation, func(a, b *big.Rat) int {
				// Сортирует рациональные PTS для независимой проверки состава кадров при перестановке B-кадров.
				return a.Cmp(b)
			})
			// i обозначает позицию кадра в порядке показа; pts содержит его фактическое
			// время для проверки сетки 30 FPS.
			for i, pts := range presentation {
				if pts.Cmp(big.NewRat(int64(i), 30)) != 0 {
					t.Fatalf("Изменён порядок/состав представления видео: PTS[%d]=%s", i, pts)
				}
			}
			// i обозначает порядковый номер AAC AU; packet содержит проверяемый аудиопакет
			// заглушки.
			for i, packet := range packets[media.TrackAudio] {
				// stamp содержит времена и длительность AAC AU; base задаёт единицу этих
				// значений.
				stamp, base := packet.Info(), packet.Config().Info().TimeBase
				if stamp.PTS != stamp.DTS || fallbackActualTime(stamp.DTS, base).Cmp(big.NewRat(int64(i*1024), 48000)) != 0 || fallbackActualTime(stamp.DurationTicks, base).Cmp(big.NewRat(1024, 48000)) != 0 {
					t.Fatalf("Нарушен общий восстановленный origin AAC[%d]: %+v", i, stamp)
				}
			}
			// repeated накапливает три последовательных периода AAC в ADTS для проверки
			// тишины на границах петли.
			var repeated []byte
			for range 3 {
				// packet представляет очередной AAC AU периода, добавляемый к
				// повторяющемуся аудиопотоку.
				for _, packet := range packets[media.TrackAudio] {
					repeated = append(repeated, liveAudioADTS(t, packet.Config().Headers(), packet.Payload())...)
				}
			}
			pcm = mediaCommand(t, executable, repeated, "-v", "error", "-xerror", "-flags2:a", "+skip_manual", "-f", "aac", "-i", "pipe:0", "-map", "0:a:0", "-f", "f32le", "pipe:1")
			if len(pcm) != 3*accessUnits*1024*2*4 || liveAudioNonzeroSamples(pcm) != 0 {
				t.Fatalf("Граница петли содержит звук: bytes=%d nonzero=%d", len(pcm), liveAudioNonzeroSamples(pcm))
			}
		})
	}
}

// liveAudioCapture хранит ограниченный синхронизированный захват пакетов маршрутизатора для независимых
// проверок.
type liveAudioCapture struct {
	// mu защищает накопленные пакеты и признак превышения бюджета от конкурентного доступа.
	mu sync.Mutex
	// packets сохраняет захваченные медиапакеты в порядке получения подпиской.
	packets []media.Packet
	// overflow отмечает превышение предела 8192 пакетов для явного отказа проверки.
	overflow bool
}

// Возвращает копию накопленного списка пакетов под блокировкой, чтобы анализ не конкурировал с
// захватом.
func (c *liveAudioCapture) snapshot() []media.Packet {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.packets)
}

// Подписывает ограниченный захват пакетов на Router и возвращает функцию отмены с ожиданием завершения
// горутины. t.Cleanup повторяет остановку и сообщает переполнение бюджета 8192 пакетов.
func liveAudioStartCapture(t *testing.T, router *media.Router) (*liveAudioCapture, func()) {
	t.Helper()
	// capture владеет накопленными пакетами текущей подписки и признаком переполнения.
	capture := &liveAudioCapture{}
	// sub получает независимую очередь пакетов маршрутизатора; err сообщает об ошибке подписки.
	sub, err := router.Subscribe(publisherQueue())
	if err != nil {
		t.Fatal(err)
	}
	// ctx задаёт срок работы захвата; cancel отменяет чтение при остановке проверки.
	ctx, cancel := context.WithCancel(context.Background())
	// done закрывается после завершения горутины захвата для ожидания её остановки.
	done := make(chan struct{})
	go func() {
		// Собирает пакеты маршрутизатора под блокировкой с явным пределом захвата.

		defer close(done)
		for {
			// delivery содержит очередной захваченный пакет; err сообщает об окончании либо
			// ошибке чтения подписки.
			delivery, err := sub.Next(ctx)
			if err != nil {
				return
			}
			capture.mu.Lock()
			if len(capture.packets) >= 8192 {
				capture.overflow = true
				capture.mu.Unlock()
				return
			}
			capture.packets = append(capture.packets, delivery.Packet)
			capture.mu.Unlock()
		}
	}()
	// stop отменяет чтение, закрывает подписку и дожидается завершения захвата.
	stop := func() {
		// Отменяет чтение, закрывает подписку и дожидается завершения захвата.
		cancel()
		_ = sub.Close()
		<-done
	}
	t.Cleanup(func() {
		// Завершает захват и сообщает тесту о превышении бюджета пакетов.

		stop()
		capture.mu.Lock()
		defer capture.mu.Unlock()
		if capture.overflow {
			t.Error("Превышен бюджет захвата AU")
		}
	})
	return capture, stop
}

// Строит множество SHA256 полезных нагрузок аудиопакетов для отличения исходного LIVE от
// подготовленной тишины.
func liveAudioKnown(packets []media.Packet) map[[32]byte]bool {
	// known содержит SHA256 аудионагрузок заданного источника для отличения LIVE от подготовленной
	// тишины.
	known := map[[32]byte]bool{}
	// p представляет пакет источника, из которого в набор известных нагрузок выбирается аудио.
	for _, p := range packets {
		if p.Config().Info().Track == media.TrackAudio {
			known[sha256.Sum256(p.Payload())] = true
		}
	}
	return known
}

// Подсчитывает RTMP AAC AU, совпавшие с известным источником и не входящие в множество пакетов тишины.
func liveAudioMatched(messages []publisherWireMessage, known, quiet map[[32]byte]bool) int {
	// n считает принятые AAC AU, совпадающие с известным LIVE и не являющиеся известной тишиной.
	n := 0
	// m представляет RTMP-сообщение, проверяемое на полный AAC AU.
	for _, m := range messages {
		if m.typ == 8 && len(m.body) > 2 && m.body[1] == 1 {
			// hash идентифицирует точные байты принятой AAC-нагрузки для сравнения с LIVE и
			// тишиной.
			hash := sha256.Sum256(m.body[2:])
			if known[hash] && !quiet[hash] {
				n++
			}
		}
	}
	return n
}

// Проверяет возрастающие DTS, длительности и согласованное начало A/V каждого выходного сегмента. LIVE
// AAC в copy сверяется с принятыми пакетами по содержимому и общему сдвигу времени, а transcode — с
// последовательностями независимого FFmpeg-эталона.
func liveAudioAssertGroupTiming(t *testing.T, packets []media.Packet, profile core.GroupProfile, source, reference []media.Packet, quiet map[[32]byte]bool) {
	t.Helper()
	// segment хранит первые времена видео и аудио одной выходной эпохи для проверки их
	// согласованного начала.
	type segment struct { /* video содержит начало видеодорожки сегмента в секундах; audio содержит начало его аудиодорожки в тех же единицах. */
		video, audio *big.Rat
	}
	// segments связывает выходную эпоху с первыми временами её видео и аудио.
	segments := map[uint64]*segment{}
	// last хранит предыдущий DTS каждой дорожки для проверки строгого возрастания через переходы.
	last := map[media.Track]*big.Rat{}
	// sourceTimes связывает точную AAC-нагрузку с принятыми временами DTS исходного LIVE.
	sourceTimes := map[[32]byte][]*big.Rat{}
	// sourceDurations связывает точную AAC-нагрузку с длительностями, измеренными между соседними
	// принятыми DTS.
	sourceDurations := map[[32]byte][]*big.Rat{}
	// previousIngress хранит последний входной AAC AU каждой публикации для независимого измерения
	// длительности предыдущего.
	previousIngress := map[uint64]media.Packet{}
	// packet представляет фактически принятый входной AU для построения независимого аудиоэталона.
	for _, packet := range source {
		if packet.Config().Info().Track != media.TrackAudio {
			continue
		}
		// hash идентифицирует входную AAC-нагрузку при сопоставлении времён и исключении тишины.
		hash := sha256.Sum256(packet.Payload())
		// config содержит временную базу и эпоху источника входного AAC AU.
		config := packet.Config().Info()
		// previous содержит предыдущий AAC AU той же публикации; exists показывает наличие
		// соседней точки для измерения длительности.
		if previous, exists := previousIngress[config.SourceEpoch]; exists {
			// previousHash идентифицирует предыдущую входную AAC-нагрузку, которой
			// назначается измеренная длительность.
			previousHash := sha256.Sum256(previous.Payload())
			// duration выражает разницу DTS соседних входных AAC AU точной дробью секунд.
			duration := new(big.Rat).Sub(fallbackActualTime(packet.Info().DTS, config.TimeBase), fallbackActualTime(previous.Info().DTS, previous.Config().Info().TimeBase))
			sourceDurations[previousHash] = append(sourceDurations[previousHash], duration)
		}
		previousIngress[config.SourceEpoch] = packet
		if quiet[hash] {
			continue
		}
		sourceTimes[hash] = append(sourceTimes[hash], fallbackActualTime(packet.Info().DTS, config.TimeBase))
	}
	// referencePosition хранит допустимую позицию независимого аудиоэталона и постоянный временной
	// сдвиг.
	type referencePosition struct {
		// owner определяет независимую эталонную последовательность по эпохе её конфигурации.
		owner uint64
		// index обозначает позицию AAC AU внутри независимой эталонной последовательности.
		index int
		// offset содержит постоянный сдвиг DTS выходной группы относительно выбранного эталона.
		offset *big.Rat
	}
	// referenceStreams разделяет эталонные AAC AU по независимым точкам допуска.
	referenceStreams := map[uint64][]media.Packet{}
	// packet представляет пакет эталона, из которого выбирается аудио для последовательного
	// сопоставления.
	for _, packet := range reference {
		if packet.Config().Info().Track == media.TrackAudio {
			// owner определяет эталонную последовательность текущего AAC AU.
			owner := packet.Config().Info().SourceEpoch
			referenceStreams[owner] = append(referenceStreams[owner], packet)
		}
	}
	// nativePositions хранит оставшиеся допустимые позиции эталона для каждой выходной эпохи
	// транскодирования.
	nativePositions := map[uint64][]referencePosition{}
	// lastAudio хранит позицию последнего аудиопакета каждой выходной эпохи для проверки его
	// длительности в copy.
	lastAudio := map[uint64]int{}
	// liveEpochs отмечает выходные эпохи с распознанным LIVE-аудио, отличая их от файловой тишины.
	liveEpochs := map[uint64]bool{}
	// i обозначает позицию выходного пакета; packet используется для поиска последнего AAC AU эпохи
	// и признаков LIVE.
	for i, packet := range packets {
		if packet.Config().Info().Track == media.TrackAudio {
			lastAudio[packet.Config().Info().SourceEpoch] = i
			// hash идентифицирует выходную AAC-нагрузку при определении LIVE-эпохи.
			hash := sha256.Sum256(packet.Payload())
			if profile.EffectiveLiveAudioMode() == core.LiveAudioCopy && len(sourceTimes[hash]) > 0 || profile.EffectiveLiveAudioMode() == core.LiveAudioTranscode && !quiet[hash] {
				liveEpochs[packet.Config().Info().SourceEpoch] = true
			}
		}
	}
	// shifts хранит допустимые постоянные сдвиги входного AAC-времени для каждой выходной эпохи
	// copy.
	shifts := map[uint64][]*big.Rat{}
	// index обозначает позицию пакета в захвате группы; packet содержит AU, чья временная структура
	// проверяется.
	for index, packet := range packets {
		// config содержит параметры дорожки и эпоху пакета; stamp содержит его временные метки и
		// длительность.
		config, stamp := packet.Config().Info(), packet.Info()
		// dts выражает фактическое время декодирования выходного AU точной дробью секунд.
		dts := fallbackActualTime(stamp.DTS, config.TimeBase)
		// previous содержит прежний DTS той же дорожки для проверки возрастания без сброса при
		// переходе.
		if previous := last[config.Track]; previous != nil && dts.Cmp(previous) <= 0 {
			t.Fatalf("Переход сбросил DTS %s: %s -> %s", config.Track, previous, dts)
		}
		last[config.Track] = dts
		// wantDuration задаёт штатную длительность кадра видео либо блока AAC текущего выходного
		// профиля.
		wantDuration := big.NewRat(int64(profile.Format.Video.FrameRate.Denominator), int64(profile.Format.Video.FrameRate.Numerator))
		if config.Track == media.TrackAudio {
			wantDuration = big.NewRat(1024, int64(profile.Format.Audio.SampleRateHz))
		}
		// duration выражает фактическую длительность выходного пакета в секундах.
		duration := fallbackActualTime(stamp.DurationTicks, config.TimeBase)
		// validDuration показывает соответствие длительности профилю, принятому copy-входу либо
		// независимому transcode-эталону.
		validDuration := duration.Cmp(wantDuration) == 0
		if config.Track == media.TrackAudio && liveEpochs[config.SourceEpoch] && profile.EffectiveLiveAudioMode() == core.LiveAudioCopy {
			// Ожидание берётся из соседних принятых DTS; последний AU может сохранить исходную длительность из ASC.
			validDuration = profile.EffectiveLiveAudioMode() == core.LiveAudioCopy && index == lastAudio[config.SourceEpoch] && validDuration
			// accepted представляет измеренную по входным соседним DTS допустимую
			// длительность данной AAC-нагрузки.
			for _, accepted := range sourceDurations[sha256.Sum256(packet.Payload())] {
				validDuration = validDuration || duration.Cmp(accepted) == 0
			}
		}
		if config.Track == media.TrackAudio && liveEpochs[config.SourceEpoch] && profile.EffectiveLiveAudioMode() == core.LiveAudioTranscode {
			// remaining собирает позиции эталона, совместимые с очередным выходным AAC AU.
			var remaining []referencePosition
			// match проверяет длительность и постоянный сдвиг DTS выбранной позиции
			// независимого эталона.
			match := func(owner uint64, index int, offset *big.Rat) {
				// Добавляет допустимую позицию независимого эталона, если длительность и постоянный сдвиг DTS
				// совпадают.

				// stream содержит AAC-последовательность проверяемой независимой точки
				// допуска.
				stream := referenceStreams[owner]
				if index >= len(stream) {
					return
				}
				// expected содержит эталонный AAC AU на проверяемой позиции
				// последовательности.
				expected := stream[index]
				// base задаёт временную базу текущего эталонного AU.
				base := expected.Config().Info().TimeBase
				if duration.Cmp(fallbackActualTime(expected.Info().DurationTicks, base)) != 0 {
					return
				}
				// shift измеряет сдвиг DTS проверяемого выхода относительно текущего
				// эталонного AU.
				shift := new(big.Rat).Sub(dts, fallbackActualTime(expected.Info().DTS, base))
				if offset == nil || shift.Cmp(offset) == 0 {
					remaining = append(remaining, referencePosition{owner, index, shift})
				}
			}
			// prior содержит допустимые позиции эталона после предыдущего AU этой выходной
			// эпохи; exists показывает, начато ли её сопоставление.
			if prior, exists := nativePositions[config.SourceEpoch]; exists {
				// position представляет ранее допустимую позицию, от которой проверяется
				// следующий эталонный AU.
				for _, position := range prior {
					match(position.owner, position.index+1, position.offset)
				}
			} else {
				// owner определяет независимую эталонную последовательность; stream
				// содержит её AU для поиска начального совпадения.
				for owner, stream := range referenceStreams {
					// index обозначает возможную начальную позицию выходной эпохи в
					// эталонной последовательности.
					for index := range stream {
						match(owner, index, nil)
					}
				}
			}
			validDuration = len(remaining) > 0
			nativePositions[config.SourceEpoch] = remaining
		}
		if stamp.PTS != stamp.DTS || !validDuration {
			t.Fatalf("Изменена независимая ожидаемая временная структура %s: %+v epoch=%d live=%v", config.Track, stamp, config.SourceEpoch, liveEpochs[config.SourceEpoch])
		}
		// current содержит накопленные начала видео и аудио текущей выходной эпохи.
		current := segments[config.SourceEpoch]
		if current == nil {
			current = &segment{}
			segments[config.SourceEpoch] = current
		}
		if config.Track == media.TrackVideo && current.video == nil {
			current.video = dts
		}
		if config.Track == media.TrackAudio && current.audio == nil {
			current.audio = dts
		}
		if config.Track != media.TrackAudio || profile.EffectiveLiveAudioMode() != core.LiveAudioCopy {
			continue
		}
		// times содержит исходные DTS всех входных AU с той же точной AAC-нагрузкой.
		times := sourceTimes[sha256.Sum256(packet.Payload())]
		if len(times) == 0 {
			continue
		}
		// candidates собирает постоянные сдвиги copy-времени, совместимые с текущим и
		// предыдущими AU эпохи.
		var candidates []*big.Rat
		// original представляет исходное входное DTS совпавшей AAC-нагрузки.
		for _, original := range times {
			// shift измеряет сдвиг выходного DTS относительно одного совпавшего входного AAC
			// AU.
			shift := new(big.Rat).Sub(dts, original)
			// previous содержит ранее допустимые сдвиги copy-времени эпохи; exists
			// показывает наличие предыдущего сопоставления.
			previous, exists := shifts[config.SourceEpoch]
			if !exists {
				candidates = append(candidates, shift)
				continue
			}
			// allowed представляет ранее допустимый постоянный сдвиг времени для текущей
			// copy-эпохи.
			for _, allowed := range previous {
				if shift.Cmp(allowed) == 0 {
					candidates = append(candidates, shift)
					break
				}
			}
		}
		if len(candidates) == 0 {
			t.Fatalf("LIVE AAC потерял единый сдвиг независимой ожидаемой шкалы: mode=%s epoch=%d", profile.EffectiveLiveAudioMode(), config.SourceEpoch)
		}
		shifts[config.SourceEpoch] = candidates
	}
	// epoch определяет проверяемый выходной сегмент; current содержит начала его дорожек для
	// контроля рассогласования до 0,1 секунды.
	for epoch, current := range segments {
		if current.video != nil && current.audio != nil && new(big.Rat).Abs(new(big.Rat).Sub(current.video, current.audio)).Cmp(big.NewRat(1, 10)) > 0 {
			t.Fatalf("Начало сегмента рассогласовало A/V: epoch=%d video=%s audio=%s", epoch, current.video, current.audio)
		}
	}
}

// Непрерывно декодирует выход RTMP и проверяет LIVE-тон и длинные участки цифровой тишины. После
// переключения допустимый остаток декодера измеряется отдельным контрольным декодированием той же
// последовательности.
func liveAudioAssertMixedSilence(t *testing.T, executable string, messages []publisherWireMessage, quiet map[[32]byte]bool, channels, sampleRate int) {
	t.Helper()
	// pcm содержит непрерывно декодированное аудио назначения; payloads сохраняет соответствующие
	// AAC-нагрузки по блокам.
	pcm, payloads := liveAudioWirePCM(t, executable, messages, channels)
	liveAudioAssertTone(t, pcm, payloads, quiet, channels, sampleRate)
	// asc сохраняет первую принятую конфигурацию AAC для контрольного декодирования перехода к
	// тишине.
	var asc []byte
	// m представляет принятое сообщение, из которого ищется начальный AudioSpecificConfig.
	for _, m := range messages {
		if m.typ == 8 && len(m.body) > 2 && m.body[1] == 0 {
			asc = m.body[2:]
			break
		}
	}
	// blockBytes задаёт размер PCM одного AAC AU в байтах с учётом числа каналов и float32.
	blockBytes := 1024 * channels * 4
	// longRuns считает длинные участки известных тихих AU; liveBlocks считает AU вне набора тишины.
	longRuns, liveBlocks := 0, 0
	// start обозначает начальную позицию очередного тихого участка либо текущего LIVE-блока.
	for start := 0; start < len(payloads); {
		if !quiet[sha256.Sum256(payloads[start])] {
			liveBlocks++
			start++
			continue
		}
		// end обозначает исключённую границу непрерывного участка известных тихих AU.
		end := start
		for end < len(payloads) && quiet[sha256.Sum256(payloads[end])] {
			end++
		}
		if end-start < 8 {
			start = end
			continue
		}
		longRuns++
		// actual содержит непрерывно декодированный PCM выбранного тихого участка с возможным
		// остатком предыдущего LIVE.
		actual := pcm[start*blockBytes : end*blockBytes]
		if start == 0 {
			if liveAudioNonzeroSamples(actual) != 0 {
				t.Fatal("Начальный fallback содержит ненулевой PCM")
			}
		} else {
			// Длительность остатка измеряется на той же фикстуре и декодере; универсальный предел не задаётся.
			// prefix выбирает до двух AU перед тихим участком для независимого измерения
			// остатка того же декодера.
			prefix := max(0, start-2)
			// control накапливает ADTS контрольной последовательности с коротким
			// предшествующим LIVE и тихим участком.
			var control []byte
			// payload представляет очередной AAC AU контрольной последовательности перехода
			// к тишине.
			for _, payload := range payloads[prefix:end] {
				control = append(control, liveAudioADTS(t, asc, payload)...)
			}
			// decoded содержит контрольный PCM перехода, обрезаемый до начала проверяемого
			// тихого участка.
			decoded := mediaCommand(t, executable, control, "-v", "error", "-xerror", "-flags2:a", "+skip_manual", "-f", "aac", "-i", "pipe:0", "-map", "0:a:0", "-f", "f32le", "pipe:1")
			decoded = decoded[(start-prefix)*blockBytes:]
			// lastControl хранит индекс последнего ненулевого сэмпла контрольного
			// декодирования либо -1 при полной тишине.
			lastControl := -1
			// i обозначает байтовое смещение очередного float32-сэмпла контрольного PCM.
			for i := 0; i < len(decoded); i += 4 {
				if liveAudioNonzeroSamples(decoded[i:i+4]) != 0 {
					lastControl = i / 4
				}
			}
			// lastActual хранит индекс последнего ненулевого сэмпла фактического PCM
			// выбранного тихого участка.
			lastActual := -1
			// i обозначает байтовое смещение очередного float32-сэмпла фактического PCM
			// назначения.
			for i := 0; i < len(actual); i += 4 {
				if liveAudioNonzeroSamples(actual[i:i+4]) != 0 {
					lastActual = i / 4
				}
			}
			if lastControl >= 2*1024*channels || lastActual > lastControl+1024*channels {
				t.Fatalf("PCM-звук продолжается после измеренного остатка декодера: actual=%d control=%d run=%d AU", lastActual, lastControl, end-start)
			}
			// quietFrom задаёт байтовое начало области после обоих измеренных остатков, где
			// требуется устойчивая цифровая тишина.
			quietFrom := (max(lastControl, lastActual) + 1) * 4
			if len(actual)-quietFrom < 4*blockBytes || liveAudioNonzeroSamples(actual[quietFrom:]) != 0 {
				t.Fatal("Нет устойчивой цифровой тишины после перехода")
			}
			t.Logf("Измеренный остаток LIVE: actual<=%g ms, control<=%g ms, тихий участок=%d AU", float64(lastActual+1)*1000/float64(channels*sampleRate), float64(lastControl+1)*1000/float64(channels*sampleRate), end-start)
		}
		start = end
	}
	if longRuns < 2 || liveBlocks < 12 {
		t.Fatalf("Не проверены LIVE и длинные тихие участки: fallback=%d live=%d", longRuns, liveBlocks)
	}
}

// При заданном GOST_MEDIA_FFMPEG проверяет общие группы, AAC copy и явное преобразование AAC при
// паузе, возобновлении и замене публикации. Несовместимый AAC сохраняет заглушку; исправные назначения
// остаются на одном соединении, а принятые пакеты и независимый FFmpeg-эталон определяют ожидаемые
// временные шкалы.
func TestLiveAudioCPUSharedGroupsPublisherTransitionsAndMismatch(t *testing.T) {
	// executable задаёт явно выбранный FFmpeg для дополнительной проверки общих групп и LIVE AAC.
	executable := os.Getenv("GOST_MEDIA_FFMPEG")
	if executable == "" {
		t.Skip("Локальная CPU-проверка требует GOST_MEDIA_FFMPEG")
	}
	// base задаёт основной H.264-профиль с AAC copy, коротким GOP и явными параметрами
	// аудиокодировщика.
	base := fallbackPipelineProfile(core.CodecH264)
	base.LiveAudioMode = core.LiveAudioCopy
	base.VideoEncoding.GOPFrames = 6
	base.Encoding = core.DefaultEncoding(base.Mode, "h264_nvenc", "vbr")
	base.Encoding.AudioCustom = "aac_coder=fast"
	base.AudioEncoding.BitrateBPS = 128000
	// smaller задаёт отдельную группу 64×64 при 15 FPS с сохранением исходного LIVE AAC.
	smaller := base
	smaller.Format.Video.Width, smaller.Format.Video.Height = 64, 64
	smaller.Format.Video.FrameRate = core.FrameRate{Numerator: 15, Denominator: 1}
	smaller.AudioEncoding.BitrateBPS = 96000
	// transcode задаёт явное преобразование LIVE AAC в моно 24 кГц с битрейтом 32000 бит/с.
	transcode := base
	transcode.LiveAudioMode = core.LiveAudioTranscode
	transcode.Format.Audio.SampleRateHz, transcode.Format.Audio.Channels, transcode.Format.Audio.ChannelLayout = 24000, 1, "mono"
	transcode.AudioEncoding.BitrateBPS = 32000
	// mismatch задаёт copy-профиль AAC 32 кГц, несовместимый с входным LIVE 48 кГц.
	mismatch := base
	mismatch.Format.Audio.SampleRateHz = 32000
	// profiles содержит четыре различных профиля общих групп для проверки copy, transcode и
	// несовместимости.
	profiles := []core.GroupProfile{base, smaller, transcode, mismatch}
	// prepared сохраняет самостоятельный подготовленный файл тишины для каждого профиля группы.
	var prepared [][]byte
	// quiet хранит отдельный набор точных AAC-нагрузок тишины для каждого профиля.
	var quiet []map[[32]byte]bool
	// p представляет профиль группы, для которого готовится файловая заглушка и эталон тишины.
	for _, p := range profiles {
		// data содержит подготовленную заглушку из изображения для текущего профиля.
		data := liveAudioPrepared(t, executable, p, "image")
		prepared = append(prepared, data)
		// packets содержит AU подготовленной заглушки; err сообщает об ошибке их чтения или
		// проверки формата.
		packets, err := encodingPreparedPackets(data, p.Format)
		if err != nil {
			t.Fatal(err)
		}
		quiet = append(quiet, liveAudioKnown(packets[media.TrackAudio]))
	}
	// receivers содержит пять независимых локальных RTMP-приёмников для назначений теста.
	var receivers []*publisherReceiver
	// endpoints содержит адреса пяти локальных назначений в том же порядке, что и приёмники.
	var endpoints []string
	for range 5 {
		// r принимает сообщения очередного локального RTMP-назначения без TLS.
		r, _ := newPublisherReceiver(t, false)
		receivers = append(receivers, r)
		endpoints = append(endpoints, "rtmp://"+r.listener.Addr().String()+"/audio")
	}
	// groupIndex связывает каждое назначение с профилем группы; первые два назначения используют
	// одну группу.
	groupIndex := []int{0, 0, 1, 2, 3}
	// f владеет тестовым медиатрактом четырёх групп и пяти независимо отправляемых назначений.
	f := newPublisherPipeline(t, profiles, endpoints, []string{"shared-a", "shared-b", "smaller", "transcode", "mismatch"}, groupIndex)
	f.config.Binary = executable
	// diagnostic сохраняет структурированный журнал медиатракта в ограниченном буфере четыре МиБ.
	diagnostic := boundedMediaBuffer{max: 4 << 20}
	f.config.Logger = slog.New(slog.NewJSONHandler(&diagnostic, nil))
	// paths содержит путь подготовленного файла заглушки для каждого профиля группы.
	paths := make([]string, len(profiles))
	// i обозначает профиль в списке заглушек; data содержит байты его подготовленного файла.
	for i, data := range prepared {
		paths[i] = filepath.Join(t.TempDir(), "prepared.mp4")
		// err сообщает об ошибке сохранения файла подготовленной заглушки текущего профиля.
		if err := os.WriteFile(paths[i], data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	f.config.OpenFile = func(_ context.Context, m core.ArtifactManifest) (*os.File, error) {
		// Открывает подготовленный файл только для точного профиля манифеста.

		// i обозначает позицию профиля и файла; p проверяется на точное равенство профилю
		// манифеста.
		for i, p := range profiles {
			if p.Equal(m.Profile) {
				return os.Open(paths[i])
			}
		}
		return nil, core.ErrCorrupt
	}
	// workerMu защищает счётчики создаваемых работников и записанные аргументы команд.
	var workerMu sync.Mutex
	// workers считает созданные транскодирующие работники отдельно для каждого профиля группы.
	workers := map[int]int{}
	// commands сохраняет копии фактических аргументов работников для проверки передачи явных
	// настроек AAC.
	var commands [][]string
	f.config.Transcode = func(_ context.Context, c ffmpeg.NVIDIAConfig, a ffmpeg.Attempt) (ffmpeg.Command, error) {
		// Создаёт CPU-команду из рабочего компилятора и под блокировкой учитывает число работников каждого
		// профиля и аргументы.

		// command содержит CPU-подстановку рабочего плана; err сообщает об ошибке построения
		// команды.
		command, err := liveAudioCPUCommand(c, a)
		workerMu.Lock()
		defer workerMu.Unlock()
		// i обозначает профиль в счётчике работников; p сопоставляется с профилем создаваемой
		// команды.
		for i, p := range profiles {
			if p.Equal(c.Profile) {
				workers[i]++
			}
		}
		commands = append(commands, slices.Clone(command.Args))
		return command, err
	}
	// registry владеет общими группами теста; err сообщает об ошибке создания реестра.
	registry, err := group.NewRegistry(context.Background(), group.RegistryConfig{Core: f.service, Parent: f.parent, Observer: f.observer, Runtime: f.config, MaxGroups: 4, MaxReferences: 8})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает реестр общих групп после остановки отправителей и захвата.

		// err сообщает об ошибке закрытия реестра групп при окончательной очистке теста.
		if err := registry.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	// refs удерживает группу каждого из пяти назначений, включая две ссылки на одну общую группу.
	refs := make([]*group.Reference, 5)
	// pubs хранит отправитель каждого назначения для остановки и независимой проверки
	// переподключений.
	pubs := make([]*publisher.Publisher, 5)
	// captures связывает экземпляр общей группы с единственным независимым захватом её выходных
	// пакетов.
	captures := map[*group.Group]*liveAudioCapture{}
	// tapStops хранит функции остановки захватов групп для получения окончательного снимка пакетов.
	var tapStops []func()
	// output представляет выход тестовой сессии, для которого удерживается группа и запускается
	// отправитель.
	for _, output := range f.outputs {
		// i определяет позицию назначения по его сохранённому endpoint в списке локальных
		// приёмников.
		i := slices.Index(endpoints, output.Snapshot.Destination.Endpoint)
		if i < 0 {
			t.Fatal("Неизвестное тестовое назначение")
		}
		// ref удерживает общую группу текущего выхода; err сообщает об ошибке её получения.
		ref, err := registry.Acquire(context.Background(), output.Snapshot, f.live)
		if err != nil {
			t.Fatal(err)
		}
		refs[i] = ref
		if captures[ref.Group()] == nil {
			// capture накапливает ограниченный захват пакетов ранее ещё не наблюдаемой общей
			// группы.
			capture := &liveAudioCapture{}
			captures[ref.Group()] = capture
			// sub получает независимую подписку на маршрутизатор группы; err сообщает об
			// ошибке её создания.
			sub, err := ref.Router().Subscribe(publisherQueue())
			if err != nil {
				t.Fatal(err)
			}
			// ctx задаёт срок чтения текущей группы; cancel отменяет чтение при остановке
			// захвата.
			ctx, cancel := context.WithCancel(context.Background())
			// done закрывается после завершения сборщика пакетов текущей общей группы.
			done := make(chan struct{})
			go func() {
				// Собирает пакеты одной общей группы в ограниченный синхронизированный буфер.

				defer close(done)
				for {
					// d содержит очередной выходной пакет группы; err сообщает об
					// окончании либо ошибке чтения подписки.
					d, err := sub.Next(ctx)
					if err != nil {
						return
					}
					capture.mu.Lock()
					if len(capture.packets) >= 8192 {
						capture.overflow = true
						capture.mu.Unlock()
						return
					}
					capture.packets = append(capture.packets, d.Packet)
					capture.mu.Unlock()
				}
			}()
			// stopTap отменяет чтение группы, закрывает подписку и дожидается завершения её
			// сборщика.
			stopTap := func() {
				// Отменяет чтение общей группы, закрывает подписку и ждёт завершения её сборщика.

				cancel()
				_ = sub.Close()
				<-done
			}
			tapStops = append(tapStops, stopTap)
			t.Cleanup(func() {
				// Завершает захват общей группы и проверяет отсутствие переполнения его бюджета.

				stopTap()
				capture.mu.Lock()
				defer capture.mu.Unlock()
				if capture.overflow {
					t.Error("Превышен бюджет захвата AU")
				}
			})
		}
		pubs[i] = publisherActivate(t, f, output, ref.Router(), nil)
	}
	if len(captures) != 4 || refs[0].Group() != refs[1].Group() {
		t.Fatal("Назначения не разделяют одну обработку группы")
	}
	t.Cleanup(func() {
		// При отказе записывает состояния групп и отправителей, завершает группы и добавляет команды и журнал
		// к диагностике.

		if !t.Failed() {
			return
		}
		// i обозначает назначение в диагностике; ref даёт состояние связанной с ним группы перед
		// остановкой.
		for i, ref := range refs {
			t.Logf("Группа/назначение %d перед остановкой: status=%+v publisher=%+v", i, ref.Group().Status(), pubs[i].Snapshot())
		}
		// err сообщает об ошибке закрытия групп при сборе диагностики неуспешного теста.
		if err := registry.Close(context.Background()); err != nil {
			t.Logf("Завершение диагностируемых групп: %v", err)
		}
		// log содержит окончание ограниченного журнала медиатракта для диагностического вывода
		// при отказе.
		log := diagnostic.String()
		if len(log) > 32000 {
			log = log[len(log)-32000:]
		}
		t.Logf("Диагностика медиатракта: %s", log)
		workerMu.Lock()
		defer workerMu.Unlock()
		t.Logf("Число workers: %+v; команды: %q", workers, commands)
	})
	// waitSource ожидает доступность выбранного источника у заданного набора групп.
	waitSource := func(indices []int, want group.Feed) {
		// Ожидает выбранный источник только у указанных групп.

		t.Helper()
		publisherWait(t, func() bool {
			// Проверяет готовность каждой выбранной группы и совпадение её источника с ожидаемым.

			// i выбирает назначение, чья группа должна перейти к ожидаемому источнику.
			for _, i := range indices {
				// s содержит текущую доступность и источник проверяемой группы.
				s := refs[i].Group().Status()
				if !s.Available || s.Source != want {
					return false
				}
			}
			return true
		})
	}
	waitSource([]int{0, 1, 2, 3, 4}, group.Fallback)
	// r представляет приёмник, на котором ожидается длинный начальный участок файловой тишины.
	for _, r := range receivers {
		r.wait(t, func(messages []publisherWireMessage) bool {
			// Ожидает длинный начальный участок AAC заглушки на каждом назначении.

			// n считает полные AAC AU начальной заглушки для ожидания достаточной длины
			// участка.
			n := 0
			// m представляет принятое RTMP-сообщение, учитываемое при подсчёте AAC AU
			// заглушки.
			for _, m := range messages {
				if m.typ == 8 && len(m.body) > 2 && m.body[1] == 1 {
					n++
				}
			}
			return n >= 155
		})
	}
	// inputProfile задаёт настоящий LIVE AV1 128×128 со звуковым тоном AAC.
	inputProfile := publisherProfile(core.CodecAV1, 128, 128)
	// source содержит реальные входные AU четырёхсекундного LIVE; inputInit содержит конфигурации
	// его дорожек.
	_, source, inputInit := publisherRealFixture(t, executable, inputProfile, "4")
	// known содержит точные исходные AAC-нагрузки для проверки сохранности LIVE в copy.
	known := liveAudioKnown(source)
	// i обозначает позицию входного пакета; packet проверяется на независимый видеокадр для
	// добавления заголовка AV1.
	for i, packet := range source {
		if packet.Config().Info().Track == media.TrackVideo && packet.Info().Keyframe {
			// rebuilt содержит AV1-пакет с заголовком последовательности перед независимым
			// кадром; e сообщает об ошибке его проверки.
			rebuilt, e := media.NewPacket(packet.Info(), packet.Config(), append(inputInit.Video.Headers()[4:], packet.Payload()...))
			if e != nil {
				t.Fatal(e)
			}
			source[i] = rebuilt
		}
	}
	// ingress захватывает фактически принятые входные пакеты; stopIngress завершает захват перед
	// независимым анализом.
	ingress, stopIngress := liveAudioStartCapture(t, f.live)
	// stop прекращает первую LIVE-публикацию; done позволяет дождаться завершения её отправки.
	_, stop, done := publisherStartIngest(t, f, source, inputInit)
	defer stop()
	waitSource([]int{0, 1, 2, 3}, group.Live)
	// i выбирает одно из трёх назначений с копированием LIVE AAC для ожидания известных входных AU.
	for i := range 3 {
		receivers[i].wait(t, func(messages []publisherWireMessage) bool {
			// Ожидает известные исходные AAC-пакеты на выходах с копированием LIVE-аудио.

			return liveAudioMatched(messages, known, quiet[groupIndex[i]]) >= 20
		})
	}
	workerMu.Lock()
	if workers[0] != 1 || workers[1] != 1 || workers[2] != 1 || workers[3] != 0 {
		t.Errorf("Неверное число общих workers/допуск несовместимого AAC: %+v", workers)
	}
	workerMu.Unlock()
	// status содержит состояние несовместимой группы для проверки сохранения доступной заглушки.
	if status := refs[4].Group().Status(); !status.Available || status.Source != group.Fallback {
		t.Fatalf("Несовместимый LIVE вытеснил пригодный fallback: %+v", status)
	}
	if _, err = f.service.PauseSession(context.Background(), core.SessionCommand{RequestID: "audio-pause", SessionID: f.parent.SessionID}); err != nil {
		t.Fatal(err)
	}
	waitSource([]int{0, 1, 2, 3}, group.Fallback)
	// i выбирает совместимое назначение для проверки поступления заглушки во время ручной паузы.
	for i := range 4 {
		// before фиксирует число принятых медиасообщений перед ожиданием новых пакетов ручной
		// паузы.
		before := publisherMediaCount(receivers[i].snapshot())
		receivers[i].wait(t, func(m []publisherWireMessage) bool {
			// Ожидает новые пакеты заглушки на каждом совместимом выходе во время ручной паузы.
			return publisherMediaCount(m) > before+18
		})
	}
	if _, err = f.service.ResumeSession(context.Background(), core.SessionCommand{RequestID: "audio-resume", SessionID: f.parent.SessionID}); err != nil {
		t.Fatal(err)
	}
	waitSource([]int{0, 1, 2, 3}, group.Live)
	// previous сохраняет эпоху первой группы до замены входной публикации.
	previous := refs[0].Group().Status().Epoch
	// overrideStop прекращает заменяющую LIVE-публикацию; overrideDone позволяет дождаться
	// завершения её отправки.
	_, overrideStop, overrideDone := publisherStartIngest(t, f, source, inputInit)
	defer overrideStop()
	publisherWait(t, func() bool {
		// Ожидает новую эпоху LIVE после замены входной публикации.

		// s содержит состояние первой группы для ожидания LIVE на новой эпохе после замены
		// публикации.
		s := refs[0].Group().Status()
		return s.Available && s.Source == group.Live && s.Epoch > previous
	})
	stop()
	<-done
	waitSource([]int{0, 1, 2, 3}, group.Live)
	overrideStop()
	<-overrideDone
	waitSource([]int{0, 1, 2, 3, 4}, group.Fallback)
	// r представляет назначение, на котором ожидается длинный финальный участок заглушки.
	for _, r := range receivers {
		// before фиксирует число медиасообщений до ожидания новых пакетов финальной заглушки.
		before := publisherMediaCount(r.snapshot())
		r.wait(t, func(m []publisherWireMessage) bool {
			// Ожидает длинный участок заглушки на всех назначениях после остановки последней публикации.
			return publisherMediaCount(m) > before+30
		})
	}
	// p представляет отправитель назначения, останавливаемый перед окончательным анализом принятого
	// потока.
	for _, p := range pubs {
		if err = p.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	// r представляет приёмник для ожидания EOF и проверки единственного соединения через все
	// переходы.
	for _, r := range receivers {
		r.waitClosed(t)
		if r.accepted.Load() != 1 {
			t.Fatalf("Переходы переподключили исправный выход: %d", r.accepted.Load())
		}
	}
	// Лог читается после остановки групп, чтобы не конкурировать с записью обработчика.
	if err = registry.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	// stopTap завершает очередной захват группы перед чтением окончательного набора пакетов.
	for _, stopTap := range tapStops {
		stopTap()
	}
	stopIngress()
	// ingressPackets содержит фактически принятые входные пакеты, определяющие независимые временные
	// ожидания.
	ingressPackets := ingress.snapshot()
	if len(ingressPackets) == 0 {
		t.Fatal("Не захвачен фактически принятый ingress")
	}
	liveAudioDumpPackets(t, "accepted-ingress", ingressPackets)
	liveAudioDumpPackets(t, "explicit-transcode-group", captures[refs[3].Group()].snapshot())
	// reference содержит независимые эталонные AAC AU явного преобразования для всех допустимых
	// входных точек.
	reference := liveAudioTranscodeReference(t, executable, ingressPackets, transcode)
	if !strings.Contains(diagnostic.String(), "LIVE AAC copy") {
		t.Fatal("Нет понятной диагностики несовместимого LIVE при исправном fallback")
	}
	workerMu.Lock()
	// savedCommands копирует аргументы созданных работников под блокировкой для проверки явных
	// настроек AAC.
	savedCommands := slices.Clone(commands)
	workerMu.Unlock()
	// explicitSettings показывает наличие команды с целевой частотой, битрейтом, числом каналов и
	// выбранным AAC-кодировщиком.
	explicitSettings := false
	// args представляет сохранённые аргументы очередного фактического работника.
	for _, args := range savedCommands {
		// joined содержит аргументы работника одной строкой для проверки передачи полного набора
		// явных AAC-настроек.
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "-ar:a 24000") {
			explicitSettings = strings.Contains(joined, "-b:a 32000") && strings.Contains(joined, "-ac:a 1") && strings.Contains(joined, "-aac_coder fast")
		}
	}
	if !explicitSettings {
		t.Fatal("Явные настройки AAC не достигли рабочего процесса")
	}
	// i обозначает назначение и соответствующий профиль; r содержит независимо захваченный
	// RTMP-поток этого назначения.
	for i, r := range receivers {
		// messages содержит окончательный снимок сообщений текущего назначения для временных и
		// звуковых проверок.
		messages := r.snapshot()
		// groupPackets содержит окончательный захват общей группы текущего назначения для
		// сравнения с RTMP.
		groupPackets := captures[refs[i].Group()].snapshot()
		publisherAssertMappedTimeline(t, messages, groupPackets)
		// p задаёт ожидаемый профиль общей группы текущего назначения.
		p := profiles[groupIndex[i]]
		liveAudioAssertGroupTiming(t, groupPackets, p, ingressPackets, reference, quiet[groupIndex[i]])
		// probe содержит независимо измеренные ffprobe дорожки фактически принятого FLV-потока.
		probe := probeMedia(t, filepath.Join(filepath.Dir(executable), "ffprobe"), publisherFLV(messages))
		// seenVideo отмечает наличие видеодорожки назначения; seenAudio отмечает наличие его
		// аудиодорожки.
		seenVideo, seenAudio := false, false
		// s представляет независимо измеренную дорожку назначения для проверки кодека, размера,
		// частоты и числа каналов.
		for _, s := range probe.Streams {
			if s.CodecType == "video" {
				seenVideo = true
				if s.CodecName != "h264" || s.Width != p.Format.Video.Width || s.Height != p.Format.Video.Height {
					t.Fatalf("Неверное преобразование видео: %+v", s)
				}
			}
			if s.CodecType == "audio" {
				seenAudio = true
				if s.SampleRate != fmt.Sprint(p.Format.Audio.SampleRateHz) || s.Channels != p.Format.Audio.Channels {
					t.Fatalf("Неверный формат AAC: %+v", s)
				}
			}
		}
		if !seenVideo || !seenAudio {
			t.Fatal("Получатель не увидел обе дорожки")
		}
		if i < 3 {
			// m представляет сообщение copy-назначения, проверяемое на сохранность
			// AAC-нагрузки.
			for _, m := range messages {
				if m.typ == 8 && len(m.body) > 2 && m.body[1] == 1 {
					// hash идентифицирует принятую AAC-нагрузку для проверки её
					// принадлежности исходному LIVE либо подготовленной тишине.
					hash := sha256.Sum256(m.body[2:])
					if !quiet[groupIndex[i]][hash] && !known[hash] {
						t.Fatalf("Publisher изменил AAC payload: %x", hash)
					}
				}
			}
			liveAudioAssertMixedSilence(t, executable, messages, quiet[groupIndex[i]], 2, int(p.Format.Audio.SampleRateHz))
		} else if i == 3 {
			liveAudioAssertMixedSilence(t, executable, messages, quiet[groupIndex[i]], 1, int(p.Format.Audio.SampleRateHz))
			if liveAudioMatched(messages, known, quiet[groupIndex[i]]) > 0 {
				t.Fatal("Явный transcode сохранил входные AAC AU вместо преобразования формата")
			}
		} else {
			// pcm содержит непрерывно декодированное аудио несовместимой группы для проверки
			// сохранения цифровой тишины.
			pcm, _ := liveAudioWirePCM(t, executable, messages, 2)
			if liveAudioNonzeroSamples(pcm) != 0 {
				t.Fatal("Несовместимая группа передала звук вместо тишины fallback")
			}
		}
	}
}
