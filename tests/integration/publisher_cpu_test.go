package integration

import (
	"bytes"
	"context"
	"encoding/binary"
	"math/big"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/fallback"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
	"github.com/GregoryGost/gost-restreamer/internal/media/group"
)

// Оракул использует точные дроби: округление осуществляется после независимого PTS/DTS.
func publisherNearestMS(ticks int64, base media.TimeBase) int64 {
	// value представляет временную метку пакета в миллисекундах точной дробью до округления.
	value := new(big.Rat).Mul(new(big.Rat).SetInt64(ticks), new(big.Rat).SetFrac64(int64(base.Numerator)*1000, int64(base.Denominator)))
	// sign сохраняет знак исходной метки, чтобы округлять отрицательное время тем же правилом по
	// модулю.
	sign := value.Sign()
	value.Abs(value)
	value.Add(value, big.NewRat(1, 2))
	// rounded содержит целое число миллисекунд после округления половины от нуля.
	rounded := new(big.Int).Quo(value.Num(), value.Denom())
	if sign < 0 {
		rounded.Neg(rounded)
	}
	return rounded.Int64()
}

// Создаёт декодируемый AV1 либо H.264 источник 128×128, 60 FPS с тоном AAC и демультиплексирует его в
// пакеты. Независимый ffprobe проверяет количество и точные временные метки пакетов.
func publisherRealFixture(t *testing.T, binaryPath string, p core.GroupProfile, duration string) ([]byte, []media.Packet, fmp4.Init) {
	t.Helper()
	// args задаёт создание короткого видеопотока 128×128 при 60 FPS с тоном AAC для проверки
	// отправителя.
	args := []string{"-hide_banner", "-loglevel", "warning", "-xerror", "-f", "lavfi", "-i", "testsrc2=size=128x128:rate=60:duration=" + duration, "-f", "lavfi", "-i", "sine=frequency=750:sample_rate=48000:duration=" + duration, "-map", "0:v", "-map", "1:a", "-threads:v", "1", "-g:v", "6", "-bf:v", "0", "-fps_mode:v", "passthrough", "-vf", "setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709", "-color_range:v", "tv", "-color_primaries:v", "bt709", "-color_trc:v", "bt709", "-colorspace:v", "bt709"}
	if p.Format.Video.Codec == core.CodecAV1 {
		args = append(args, "-c:v", "libaom-av1", "-cpu-used", "8", "-lag-in-frames", "0", "-b:v", "250000")
	} else {
		args = append(args, "-c:v", "libx264", "-profile:v", "high", "-sc_threshold", "0", "-b:v", "250000")
	}
	args = append(args, "-enc_time_base:v", "1:60", "-video_track_timescale", "60", "-c:a", "aac", "-b:a", "64000", "-ar:a", "48000", "-ac:a", "2", "-f", "mp4", "-movflags", "delay_moov+default_base_moof+frag_every_frame", "-min_frag_duration", "1", "-use_editlist", "1", "-movie_timescale", "48000", "-avoid_negative_ts", "disabled", "pipe:1")
	// data содержит полученный от FFmpeg фрагментированный MP4 с тестовым видео и звуком.
	data := mediaCommand(t, binaryPath, nil, args...)
	// probe содержит независимо измеренные ffprobe дорожки и временные метки созданного MP4.
	probe := probeMedia(t, filepath.Join(filepath.Dir(binaryPath), "ffprobe"), data)
	// d читает тестовый MP4 с привязкой AV1 к заданному профилю; err сообщает об ошибке создания
	// демультиплексора.
	d, err := fmp4.NewDemuxer(bytes.NewReader(data), fmp4.DemuxOptions{Limits: integrationLimits(), SourceEpoch: 1, VideoRevision: 1, AudioRevision: 1, ResolveConfig: func(desc fmp4.TrackDescription) (media.CodecConfig, error) {
		// Разбирает AAC и H.264 штатным способом, а AV1 связывает с проверяемым профилем тестового источника.

		if desc.Track == media.TrackAudio || desc.Codec == core.CodecH264 {
			return fmp4.ResolveCodecConfig(desc, 1, 1)
		}
		return media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: 1, Revision: 1, TimeBase: desc.TimeBase, Video: p.Format.Video}, desc.Headers)
	}})
	if err != nil {
		t.Fatal(err)
	}
	// packets содержит медиапакеты созданного источника, сверяемые с независимым ffprobe.
	packets := readMediaPackets(t, d)
	assertProbePackets(t, probe, packets)
	return data, packets, d.Init()
}

// Собирает FLV из принятых RTMP-аудио- и видеосообщений, сохраняя их полезные нагрузки и
// миллисекундные метки; остальные сообщения пропускает.
func publisherFLV(messages []publisherWireMessage) []byte {
	// out накапливает FLV для независимого декодирования сообщений, принятых по RTMP.
	var out bytes.Buffer
	out.Write([]byte{'F', 'L', 'V', 1, 5, 0, 0, 0, 9, 0, 0, 0, 0})
	// m представляет очередное принятое RTMP-сообщение, из которого выбирается аудио или видео.
	for _, m := range messages {
		if m.typ != 8 && m.typ != 9 {
			continue
		}
		// n задаёт длину полезной нагрузки текущего FLV-тега в байтах.
		n := len(m.body)
		out.Write([]byte{m.typ, byte(n >> 16), byte(n >> 8), byte(n), byte(m.timestamp >> 16), byte(m.timestamp >> 8), byte(m.timestamp), byte(m.timestamp >> 24), 0, 0, 0})
		out.Write(m.body)
		// previous хранит четырёхбайтовый размер предыдущего FLV-тега вместе с его заголовком.
		var previous [4]byte
		binary.BigEndian.PutUint32(previous[:], uint32(n+11))
		out.Write(previous[:])
	}
	return out.Bytes()
}

// При заданном GOST_MEDIA_FFMPEG проверяет настоящий приём RTMP, Group и собственный RTMP/RTMPS
// Publisher на переходах заглушка → LIVE → заглушка. Декодирование, ffprobe и захват пакетов группы
// проверяют формат, временную шкалу и метрики, а AV1 дополнительно сверяется побайтно с источником.
func TestPublisherCPURealIngestGroupAndNativeOutputDecode(t *testing.T) {
	// binaryPath задаёт явно выбранный FFmpeg для дополнительной CPU-интеграции.
	binaryPath := os.Getenv("GOST_MEDIA_FFMPEG")
	if binaryPath == "" {
		t.Skip("CPU-интеграция требует явно заданный FFmpeg")
	}
	// outputCodec выбирает проверяемый видеокодек назначения: AV1 или H.264.
	for _, outputCodec := range []core.Codec{core.CodecAV1, core.CodecH264} {
		t.Run(string(outputCodec), func(t *testing.T) {
			// Проверяет декодирование выбранного выходного кодека после настоящих переключений заглушки и LIVE
			// через приёмник и отправителя.

			// p описывает выходной профиль группы с выбранным кодеком и размером 128×128.
			p := publisherProfile(outputCodec, 128, 128)
			// inputProfile описывает входной LIVE AV1 того же размера для копирования или
			// CPU-преобразования.
			inputProfile := publisherProfile(core.CodecAV1, 128, 128)
			// fallbackBytes содержит кодированный источник для подготовки заглушки
			// длительностью 1,6 секунды.
			fallbackBytes, _, _ := publisherRealFixture(t, binaryPath, p, "1.6")
			// source содержит пакеты настоящего LIVE-источника; inputInit хранит заголовки
			// его дорожек для публикации.
			_, source, inputInit := publisherRealFixture(t, binaryPath, inputProfile, "1.6")
			// В Enhanced RTMP независимая точка содержит заголовок последовательности вместе с OBU кадра.
			// i обозначает позицию пакета во входном LIVE; packet проверяется на независимый
			// видеокадр для добавления заголовка AV1.
			for i, packet := range source {
				if packet.Config().Info().Track == media.TrackVideo && packet.Info().Keyframe {
					// payload объединяет заголовок последовательности AV1 и кадр для
					// независимой точки Enhanced RTMP.
					payload := append(inputInit.Video.Headers()[4:], packet.Payload()...)
					// rebuilt содержит пакет с дополненной полезной нагрузкой AV1; e
					// сообщает об ошибке его проверки.
					rebuilt, e := media.NewPacket(packet.Info(), packet.Config(), payload)
					if e != nil {
						t.Fatal(e)
					}
					source[i] = rebuilt
				}
			}
			// r принимает отправленные группой RTMP-сообщения; trust задаёт доверие
			// тестовому TLS-сертификату для H.264.
			r, trust := newPublisherReceiver(t, outputCodec == core.CodecH264)
			// scheme выбирает RTMP либо RTMPS в зависимости от наличия тестового
			// TLS-приёмника.
			scheme := "rtmp"
			if trust != nil {
				scheme = "rtmps"
			}
			// f владеет тестовым медиатрактом с одной группой и назначением на локальный
			// приёмник.
			f := newPublisherPipeline(t, []core.GroupProfile{p}, []string{scheme + "://" + r.listener.Addr().String() + "/cpu"}, []string{"synthetic-key"}, []int{0})
			// filePath задаёт временный файл нормализованной заглушки для проигрывания
			// группой.
			filePath := filepath.Join(t.TempDir(), "fallback.mp4")
			// inputPath задаёт временный файл исходного кодированного материала заглушки.
			inputPath := filepath.Join(t.TempDir(), "encoded.mp4")
			// err сообщает об ошибке сохранения исходного MP4 для подготовки заглушки.
			if err := os.WriteFile(inputPath, fallbackBytes, 0600); err != nil {
				t.Fatal(err)
			}
			// encoded открывает исходный материал заглушки для нормализации; err сообщает об
			// ошибке открытия.
			encoded, err := os.Open(inputPath)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				// Закрывает исходный файл кодированной заглушки после её нормализации.
				_ = encoded.Close()
			}()
			// runner ограничивает выполнение FFmpeg при подготовке настоящей файловой
			// заглушки.
			runner := fallback.Runner{Limits: fallback.RunnerLimits{Timeout: 10 * time.Second, MaxArgs: 128, MaxCommandBytes: 16384, MaxDiagnosticBytes: 1 << 20}}
			// prober проверяет и нормализует подготовленную заглушку с отдельным ffprobe и
			// конечными пределами контейнера.
			prober := fallback.Prober{Binary: filepath.Join(filepath.Dir(binaryPath), "ffprobe"), Runner: fallbackPipelineRunner{t: t, runner: runner}, Limits: fallback.ProbeLimits{MaxJSONBytes: 1 << 20, MaxWidth: 1024, MaxHeight: 1024, MaxPixels: 1 << 20, MaxDuration: 10 * time.Second, ImageDuration: 1600 * time.Millisecond, MaxPackets: 4096, Container: integrationLimits()}}
			// prepared накапливает нормализованный MP4 перед сохранением файла заглушки.
			var prepared bytes.Buffer
			if err = prober.NormalizePrepared(context.Background(), encoded, &prepared); err != nil {
				t.Fatal(err)
			}
			if err = os.WriteFile(filePath, prepared.Bytes(), 0600); err != nil {
				t.Fatal(err)
			}
			// loop открывает подготовленную заглушку для проверки длительности её петли.
			loop, err := os.Open(filePath)
			if err != nil {
				t.Fatal(err)
			}
			if err = prober.VerifyLoopDuration(context.Background(), loop, 1600*time.Millisecond); err != nil {
				_ = loop.Close()
				t.Fatal(err)
			}
			if err = loop.Close(); err != nil {
				t.Fatal(err)
			}
			f.config.Binary = binaryPath
			f.config.OpenFile = func(context.Context, core.ArtifactManifest) (*os.File, error) {
				// Открывает подготовленный файл заглушки для настоящего проигрывания FFmpeg.
				return os.Open(filePath)
			}
			f.config.Transcode = func(_ context.Context, _ ffmpeg.NVIDIAConfig, a ffmpeg.Attempt) (ffmpeg.Command, error) {
				// Создаёт CPU-транскодирование AV1 в H.264 с копированием AAC и сохранением временных меток.

				return ffmpeg.Command{Binary: binaryPath, Args: []string{"-hide_banner", "-nostdin", "-loglevel", "warning", "-xerror", "-copyts", "-f", "mp4", "-fflags", "+ignidx", "-i", "async:unix:" + a.InputPath, "-map", "0:v", "-map", "0:a", "-c:v", "libx264", "-threads:v", "1", "-profile:v", "high", "-g:v", "6", "-bf:v", "0", "-sc_threshold", "0", "-fps_mode:v", "passthrough", "-c:a", "copy", "-f", "mp4", "-movflags", "delay_moov+default_base_moof+frag_every_frame", "-min_frag_duration", "1", "-use_editlist", "1", "-movie_timescale", "48000", "-avoid_negative_ts", "disabled", "unix:" + a.OutputPath}}, nil
			}
			// registry владеет локальными группами, через которые тест передаёт заглушку и
			// LIVE.
			registry, err := group.NewRegistry(context.Background(), group.RegistryConfig{Core: f.service, Parent: f.parent, Observer: f.observer, Runtime: f.config, MaxGroups: 2, MaxReferences: 2})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				// Закрывает реестр групп после освобождения отправителя и подписки проверки.

				// e сообщает об ошибке закрытия реестра групп при очистке теста.
				if e := registry.Close(context.Background()); e != nil {
					t.Error(e)
				}
			})
			// ref удерживает группу выбранного снимка и даёт доступ к её выходному
			// маршрутизатору.
			ref, err := registry.Acquire(context.Background(), f.outputs[0].Snapshot, f.live)
			if err != nil {
				t.Fatal(err)
			}
			// tap предоставляет независимую подписку на выход группы для сопоставления с
			// принятым RTMP.
			tap, err := ref.Router().Subscribe(publisherQueue())
			if err != nil {
				t.Fatal(err)
			}
			// tapContext ограничивает срок чтения пакетов группы; stopTap отменяет это
			// чтение при очистке.
			tapContext, stopTap := context.WithCancel(context.Background())
			// tapDone закрывается после завершения горутины захвата пакетов группы.
			tapDone := make(chan struct{})
			// tapped сохраняет выходные пакеты группы для последующей независимой сверки
			// временной шкалы.
			var tapped []media.Packet
			// tapMutex защищает накопленные пакеты группы от одновременного чтения и
			// добавления.
			var tapMutex sync.Mutex
			go func() {
				// Собирает пакеты выхода группы под блокировкой для независимого сравнения временных меток с RTMP.

				defer close(tapDone)
				for {
					// delivery содержит очередной выходной пакет группы; e сообщает
					// об окончании или ошибке чтения подписки.
					delivery, e := tap.Next(tapContext)
					if e != nil {
						return
					}
					tapMutex.Lock()
					tapped = append(tapped, delivery.Packet)
					tapMutex.Unlock()
				}
			}()
			t.Cleanup(func() {
				// Отменяет чтение, закрывает подписку и ожидает завершения сборщика пакетов группы.
				stopTap()
				_ = tap.Close()
				<-tapDone
			})
			// pub управляет настоящим отправителем выбранного назначения во время переходов
			// между источниками.
			pub := publisherActivate(t, f, f.outputs[0], ref.Router(), trust)
			t.Cleanup(func() {
				// Записывает состояния группы и отправителя только при отказе проверки декодирования.

				if t.Failed() {
					t.Logf("Group status=%+v; Publisher=%+v; result=%+v", ref.Group().Status(), pub.Snapshot(), pub.Result())
				}
			})
			r.wait(t, func(m []publisherWireMessage) bool {
				// Ожидает начальную передачу заглушки до публикации LIVE.
				return publisherMediaCount(m) >= 12
			})
			// stop прекращает тестовую публикацию LIVE; done позволяет дождаться завершения
			// её отправки.
			_, stop, done := publisherStartIngest(t, f, source, inputInit)
			defer stop()
			publisherWait(t, func() bool {
				// Ожидает готовность группы на источнике LIVE.
				return ref.Group().Status().Available && ref.Group().Status().Source == group.Live
			})
			// wantLive задаёт число принятых медиасообщений, достаточное для проверки
			// участка LIVE.
			wantLive := publisherMediaCount(r.snapshot()) + 30
			r.wait(t, func(m []publisherWireMessage) bool {
				// Ожидает достаточное число медиапакетов во время передачи LIVE.
				return publisherMediaCount(m) >= wantLive
			})
			stop()
			<-done
			publisherWait(t, func() bool {
				// Ожидает возврат группы к готовой заглушке после остановки входа.
				return ref.Group().Status().Available && ref.Group().Status().Source == group.Fallback
			})
			// wantFallback задаёт число принятых медиасообщений после возврата группы к
			// заглушке.
			wantFallback := publisherMediaCount(r.snapshot()) + 12
			r.wait(t, func(m []publisherWireMessage) bool {
				// Ожидает дополнительные пакеты заглушки после перехода с LIVE.
				return publisherMediaCount(m) >= wantFallback
			})
			if err = pub.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			r.waitClosed(t)
			if r.accepted.Load() != 1 {
				t.Fatalf("реальные переходы закрыли здоровый socket: %d", r.accepted.Load())
			}
			// captured содержит окончательный снимок RTMP-сообщений после остановки
			// отправителя.
			captured := r.snapshot()
			tapMutex.Lock()
			// groupPackets копирует захваченные пакеты группы под блокировкой для
			// независимой проверки временной шкалы.
			groupPackets := append([]media.Packet(nil), tapped...)
			tapMutex.Unlock()
			publisherAssertMappedTimeline(t, captured, groupPackets)
			// flv содержит принятый RTMP-медиапоток в контейнере для ffprobe и
			// декодирования.
			flv := publisherFLV(captured)
			// probe содержит независимо прочитанные дорожки и пакеты фактически принятого
			// назначения.
			probe := probeMedia(t, filepath.Join(filepath.Dir(binaryPath), "ffprobe"), flv)
			// s представляет дорожку принятого потока, у которой проверяются видеокодек и
			// размер кадра.
			for _, s := range probe.Streams {
				if s.CodecType == "video" && (s.CodecName != string(outputCodec) || s.Width != 128 || s.Height != 128) {
					t.Fatalf("получатель увидел неверный формат: %+v", s)
				}
			}
			// videoCount считает видеопакеты ffprobe; audioCount считает аудиопакеты для
			// сопоставления с декодированными кадрами.
			videoCount, audioCount := 0, 0
			// lastVideo хранит предыдущее DTS видео; lastAudio хранит предыдущее DTS AAC для
			// проверки отсутствия сброса времени.
			var lastVideo, lastAudio *big.Rat
			// packet представляет очередной независимо прочитанный пакет принятого FLV.
			for _, packet := range probe.Packets {
				// streamType определяет аудио или видео для текущего пакета; base задаёт
				// единицу его временных меток по дорожке.
				var streamType, base string
				// s представляет дорожку ffprobe, чей индекс сопоставляется с текущим
				// пакетом.
				for _, s := range probe.Streams {
					if s.Index == packet.StreamIndex {
						streamType, base = s.CodecType, s.TimeBase
					}
				}
				// dts выражает время декодирования принятого пакета точной дробью
				// секунд.
				dts := seconds(t, packet.DTS, base)
				// pts выражает время показа принятого пакета точной дробью секунд для
				// проверки отсутствия B-кадров.
				pts := seconds(t, packet.PTS, base)
				if pts.Cmp(dts) != 0 {
					t.Fatalf("CPU-путь без B-кадров изменил PTS−DTS: %s/%s", pts, dts)
				}
				if streamType == "video" {
					if lastVideo != nil && dts.Cmp(lastVideo) < 0 {
						t.Fatal("video DTS сброшен")
					}
					lastVideo = dts
					videoCount++
				} else {
					if lastAudio != nil && dts.Cmp(lastAudio) < 0 {
						t.Fatal("AAC DTS сброшен")
					}
					lastAudio = dts
					audioCount++
				}
			}
			// decoded содержит контрольные суммы кадров после настоящего декодирования
			// принятого аудио и видео.
			decoded := mediaCommand(t, binaryPath, flv, "-hide_banner", "-loglevel", "warning", "-xerror", "-copyts", "-i", "pipe:0", "-map", "0:v", "-map", "0:a", "-fps_mode", "passthrough", "-f", "framemd5", "pipe:1")
			// got считает декодированные видеокадры для сверки с числом видеопакетов
			// ffprobe.
			if got := strings.Count(string(decoded), "\n0,"); got != videoCount || videoCount < 12 {
				t.Fatalf("декодированы video=%d из %d", got, videoCount)
			}
			// got считает декодированные аудиокадры для сверки с числом пакетов AAC ffprobe.
			if got := strings.Count(string(decoded), "\n1,"); got != audioCount || audioCount < 12 {
				t.Fatalf("декодированы AAC=%d из %d", got, audioCount)
			}
			if outputCodec == core.CodecAV1 {
				// known хранит точные полезные нагрузки входных AV1-пакетов для проверки
				// их сохранности на выходе.
				known := map[string]bool{}
				// packet представляет входной пакет, чья видеонагрузка добавляется в
				// набор известных AV1-кадров.
				for _, packet := range source {
					if packet.Config().Info().Track == media.TrackVideo {
						known[string(packet.Payload())] = true
					}
				}
				// matched считает принятые AV1-кадры с побайтно сохранённой входной
				// полезной нагрузкой.
				matched := 0
				// m представляет принятое RTMP-сообщение, проверяемое на совпадение с
				// входным AV1.
				for _, m := range captured {
					if m.typ == 9 && len(m.body) > 5 && m.body[0]&15 == 1 && known[string(m.body[5:])] {
						matched++
					}
				}
				if matched < 6 {
					t.Fatalf("не доказано побайтное сохранение actual AV1 TU: %d", matched)
				}
			}
			publisherAssertMetrics(t, pub.Snapshot().WrittenBytes, pub.Snapshot().PayloadBytes, pub.Snapshot().AccessUnits, pub.Snapshot().VideoFrames, r)
		})
	}
}

// Сопоставляет каждый принятый RTMP AU с пакетом Group по дорожке и содержимому. Все сообщения должны
// объясняться одним общим началом времени после независимого округления PTS/DTS в миллисекунды с
// сохранением CTS.
func publisherAssertMappedTimeline(t *testing.T, wire []publisherWireMessage, groupPackets []media.Packet) {
	t.Helper()
	// point хранит независимо округлённые времена показа и декодирования одного выходного пакета
	// группы.
	type point struct { /* pts задаёт время показа пакета в миллисекундах; dts задаёт его время декодирования в тех же единицах. */
		pts, dts int64
	}
	// wanted связывает дорожку и полезную нагрузку с допустимыми временными точками пакетов группы.
	wanted := map[string][]point{}
	// p представляет выходной пакет группы, добавляемый в независимый временной оракул.
	for _, p := range groupPackets {
		wanted[string(p.Config().Info().Track)+string(p.Payload())] = append(wanted[string(p.Config().Info().Track)+string(p.Payload())], point{publisherNearestMS(p.Info().PTS, p.Config().Info().TimeBase), publisherNearestMS(p.Info().DTS, p.Config().Info().TimeBase)})
	}
	// origins хранит возможные общие начала RTMP-шкалы, совместимые со всеми уже проверенными AU.
	var origins []int64
	// checked считает проверенные RTMP-единицы доступа и определяет обработку первой видеоточки.
	checked := 0
	// m представляет очередное RTMP-сообщение, сопоставляемое с пакетами группы.
	for _, m := range wire {
		// track определяет дорожку сообщения; payload содержит извлечённую медианагрузку; cts
		// задаёт знаковый сдвиг PTS относительно DTS в миллисекундах.
		track, payload, cts := media.TrackAudio, []byte(nil), int64(0)
		if m.typ == 8 && len(m.body) > 2 && m.body[1] == 1 {
			payload = m.body[2:]
		}
		if m.typ == 9 && len(m.body) > 5 && (m.body[0]&0x80 != 0 && m.body[0]&15 == 1 || m.body[0]&0x80 == 0 && m.body[1] == 1) {
			track, payload = media.TrackVideo, m.body[5:]
			if m.body[0]&0x80 == 0 {
				// raw содержит 24-битное представление композиционного сдвига H.264 до
				// расширения знака.
				raw := publisherU24(m.body[2:5])
				if raw&0x800000 != 0 {
					cts = int64(raw) - 1<<24
				} else {
					cts = int64(raw)
				}
			}
		}
		if payload == nil {
			continue
		}
		// candidates содержит временные точки пакетов группы с той же дорожкой и полезной
		// нагрузкой.
		candidates := wanted[string(track)+string(payload)]
		if checked == 0 {
			if track != media.TrackVideo {
				t.Fatal("первый wire AU не video")
			}
			// p представляет кандидата первой видеоточки для определения общего начала
			// RTMP-времени.
			for _, p := range candidates {
				if p.pts-p.dts == cts && uint32(p.dts-min(p.pts, p.dts)) == m.timestamp {
					origins = append(origins, min(p.pts, p.dts))
				}
			}
		}
		// remaining собирает начала шкалы, которые остаются допустимыми после текущего
		// RTMP-пакета.
		var remaining []int64
		// origin представляет проверяемое общее начало миллисекундной шкалы RTMP.
		for _, origin := range origins {
			// p представляет временную точку группы, проверяемую на совпадение DTS и CTS
			// текущего RTMP-пакета.
			for _, p := range candidates {
				if p.dts-origin >= 0 && uint32(p.dts-origin) == m.timestamp && p.pts-p.dts == cts {
					remaining = append(remaining, origin)
					break
				}
			}
		}
		origins = remaining
		checked++
		if len(origins) == 0 {
			t.Fatalf("AU %d %s timestamp=%d CTS=%d не соответствует exact Group Q(t)−origin", checked, track, m.timestamp, cts)
		}
	}
	if checked < 12 {
		t.Fatalf("недостаточно AU для независимой проверки timeline: %d", checked)
	}
}

// Сверяет счётчики Publisher с независимо принятыми полными AU и полезными байтами; written должен
// совпасть с числом прочитанных приёмником байтов RTMP.
func publisherAssertMetrics(t *testing.T, written, payload, units, videos uint64, r *publisherReceiver) {
	t.Helper()
	// wantPayload считает принятые полезные медиабайты; wantUnits считает полные AU; wantVideo
	// считает полные видео-AU для сверки метрик отправителя.
	var wantPayload, wantUnits, wantVideo uint64
	// m представляет принятое RTMP-сообщение, используемое в независимом подсчёте медиаметрик.
	for _, m := range r.snapshot() {
		if m.typ == 8 && len(m.body) >= 2 && m.body[1] == 1 {
			wantUnits++
			wantPayload += uint64(len(m.body) - 2)
		}
		if m.typ == 9 && len(m.body) >= 5 {
			// media показывает, содержит ли видеосообщение полный AU, а не заголовок
			// последовательности.
			media := m.body[0]&0x80 != 0 && m.body[0]&15 == 1 || m.body[0]&0x80 == 0 && m.body[1] == 1
			if media {
				wantUnits++
				wantVideo++
				wantPayload += uint64(len(m.body) - 5)
			}
		}
	}
	if payload != wantPayload || units != wantUnits || videos != wantVideo {
		t.Fatalf("метрики считают не полные AU: got=%d/%d/%d wire=%d/%d/%d", payload, units, videos, wantPayload, wantUnits, wantVideo)
	}
	if written != r.bytes.Load() {
		t.Fatalf("plaintext Write bytes=%d, независимый receiver=%d", written, r.bytes.Load())
	}
}
