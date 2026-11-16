package integration

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"log/slog"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/fallback"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
	"github.com/GregoryGost/gost-restreamer/internal/media/group"
)

// hdrPixelFrame возвращает сырой кадр YUV420P10 128×128 с нейтральными U/V.
// Для LIVE плоскость Y повторяет все кодовые значения 0..1023; prepared выбирает
// постоянный Y=64, чтобы заглушка не могла удовлетворить пиксельному ожиданию LIVE.
func hdrPixelFrame(prepared bool) []byte {
	// frame хранит три плоскости с двухбайтовыми отсчётами и младшим байтом первым.
	frame := make([]byte, 128*128*3)
	// index выбирает очередной отсчёт яркости либо одной из плоскостей цветности.
	for index := 0; index < len(frame)/2; index++ {
		// value оставляет цветность нейтральной, а яркость задаёт независимо от медиакода приложения.
		value := uint16(512)
		if index < 128*128 {
			value = uint16(index % 1024)
			if prepared {
				value = 64
			}
		}
		binary.LittleEndian.PutUint16(frame[index*2:], value)
	}
	return frame
}

// hdrPixelMismatchCount сверяет каждый отсчёт Y с expected для всех frames кадров.
// t сообщает об ошибке размера decoded; frameBytes учитывает реальную цветность,
// поэтому проверка самой яркости не требует преобразования пикселей при декодировании.
func hdrPixelMismatchCount(t *testing.T, decoded, expected []byte, frameBytes, frames int) int {
	t.Helper()
	if len(decoded) != frameBytes*frames || len(expected) != 128*128*2 {
		t.Fatalf("Неверный размер сырых кадров: получено=%d, кадр=%d, кадров=%d, Y=%d", len(decoded), frameBytes, frames, len(expected))
	}
	// mismatches считает изменённые кодовые значения, без допуска ошибки и округления.
	mismatches := 0
	// frame выбирает декодированный кадр в порядке показа.
	for frame := 0; frame < frames; frame++ {
		// offset задаёт байтовую позицию очередного двухбайтового отсчёта Y.
		for offset := 0; offset < len(expected); offset += 2 {
			if binary.LittleEndian.Uint16(decoded[frame*frameBytes+offset:]) != binary.LittleEndian.Uint16(expected[offset:]) {
				mismatches++
			}
		}
	}
	return mismatches
}

// hdrPixelProbe независимо проверяет сжатый wire через ffprobe рядом с binaryPath.
// t фиксирует отказ; codecName, profile и pixel задают буквальные ожидаемые свойства
// источника, контроля либо результата, а PQ/BT.2020/full одинаковы на всём пути.
func hdrPixelProbe(t *testing.T, binaryPath string, wire []byte, codecName, profile, pixel string) {
	t.Helper()
	// probe принимает фактические свойства единственной видеодорожки без desired-подстановки.
	var probe struct {
		// Streams содержит независимое измерение заголовков контейнера и кодека.
		Streams []struct {
			// Codec и Profile различают настоящий кодек и его десятибитный профиль.
			Codec   string `json:"codec_name"`
			Profile string `json:"profile"`
			// Pixel задаёт фактическую глубину и цветность, Range — диапазон кодовых значений.
			Pixel string `json:"pix_fmt"`
			Range string `json:"color_range"`
			// Primaries, Transfer и Matrix задают фактические CICP сжатой последовательности.
			Primaries string `json:"color_primaries"`
			Transfer  string `json:"color_transfer"`
			Matrix    string `json:"color_space"`
			// Width и Height сохраняют размер пространственного эталона без масштабирования.
			Width  int `json:"width"`
			Height int `json:"height"`
		} `json:"streams"`
	}
	// err сообщает об ошибке чтения JSON независимого инструмента.
	if err := json.Unmarshal(mediaCommand(t, filepath.Join(filepath.Dir(binaryPath), "ffprobe"), wire, "-v", "error", "-select_streams", "v:0", "-show_streams", "-of", "json", "pipe:0"), &probe); err != nil || len(probe.Streams) != 1 {
		t.Fatal("Не измерена видеодорожка", err, probe)
	}
	// actual содержит именно измеренное описание единственной видеодорожки.
	actual := probe.Streams[0]
	if actual.Codec != codecName || actual.Profile != profile || actual.Pixel != pixel || actual.Range != "pc" || actual.Primaries != "bt2020" || actual.Transfer != "smpte2084" || actual.Matrix != "bt2020nc" || actual.Width != 128 || actual.Height != 128 {
		t.Fatal("Фактический формат не соответствует пиксельной проверке", actual)
	}
}

// hdrPixelFixture создаёт независимые 48 кадров и AAC через binaryPath без компилятора приложения.
// t выделяет частные файлы. prepared выбирает H.264 High444 10-bit с тишиной для
// штатного проигрывателя; LIVE использует VP9 profile2 с тоном. Параметр lossless
// применяется лишь к синтетическому VP9-источнику; рабочий выход использует обычный CQP0.
// До передачи в Go каждый исходный Y сравнивается с буквальным несжатым кадром.
func hdrPixelFixture(t *testing.T, binaryPath string, prepared bool) ([]byte, []media.Packet, fmp4.Init) {
	t.Helper()
	// frame задаёт независимый эталон яркости и нейтральную цветность.
	frame := hdrPixelFrame(prepared)
	// args задаёт конечные 1,6 секунды без изменения размера, FPS, диапазона и глубины.
	args := []string{"-v", "error", "-nostdin", "-xerror", "-f", "rawvideo", "-pixel_format", "yuv420p10le", "-video_size", "128x128", "-framerate", "30", "-i", "pipe:0", "-f", "lavfi", "-i"}
	// pixel и frameBytes описывают реальный результат независимого кодировщика.
	pixel, frameBytes := "yuv420p10le", 128*128*3
	if prepared {
		pixel, frameBytes = "yuv444p10le", 128*128*6
		args = append(args, "anullsrc=r=48000:cl=stereo:d=1.6", "-map", "0:v", "-map", "1:a", "-c:v", "libx264", "-preset", "medium", "-profile:v", "high444", "-qp", "0", "-x264-params", "open-gop=0:scenecut=0:ref=1")
	} else {
		args = append(args, "sine=frequency=750:sample_rate=48000:duration=1.6", "-map", "0:v", "-map", "1:a", "-c:v", "libvpx-vp9", "-deadline", "good", "-cpu-used", "8", "-profile:v", "2", "-lossless", "1", "-lag-in-frames", "0", "-auto-alt-ref", "0", "-b:v", "0", "-keyint_min:v", "6")
	}
	// Полный диапазон сохраняет смысл всех 0..1023; setparams задаёт CICP также самому сырому кадру.
	args = append(args, "-frames:v", "48", "-threads:v", "1", "-g:v", "6", "-bf:v", "0", "-vf", "format="+pixel+",setparams=range=full:color_primaries=bt2020:color_trc=smpte2084:colorspace=bt2020nc", "-pix_fmt:v", pixel, "-color_range:v", "pc", "-color_primaries:v", "bt2020", "-color_trc:v", "smpte2084", "-colorspace:v", "bt2020nc", "-c:a", "aac", "-b:a", "64000", "-ar:a", "48000", "-ac:a", "2", "-f", "mp4", "-movflags", "delay_moov+default_base_moof+frag_every_frame", "-min_frag_duration", "1", "-use_editlist", "1", "-movie_timescale", "48000", "-video_track_timescale", "30", "-avoid_negative_ts", "disabled", "pipe:1")
	// wire содержит настоящие сжатые AU; decoded подтверждает исходные значения до Go-тракта.
	wire := mediaCommand(t, binaryPath, bytes.Repeat(frame, 48), args...)
	decoded := mediaCommand(t, binaryPath, wire, "-v", "error", "-xerror", "-i", "pipe:0", "-map", "0:v", "-pix_fmt", pixel, "-fps_mode", "passthrough", "-f", "rawvideo", "pipe:1")
	if hdrPixelMismatchCount(t, decoded, frame[:128*128*2], frameBytes, 48) != 0 {
		t.Fatal("Синтетический источник уже потерял исходные кодовые значения")
	}
	if prepared {
		hdrPixelProbe(t, binaryPath, wire, "h264", "High 4:4:4 Predictive", pixel)
		// path содержит только принадлежащий тесту свежий файл с AAC-задержкой кодировщика.
		path := filepath.Join(t.TempDir(), "fallback-source.mp4")
		// err сохраняет отказ записи свежего контейнера.
		if err := os.WriteFile(path, wire, 0600); err != nil {
			t.Fatal(err)
		}
		// file и err сохраняют открытый дескриптор источника и отказ его открытия.
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		defer func() { /* Закрывает частный исходный файл после нормализации AAC. */
			// err сообщает об отказе закрытия частного дескриптора.
			if err := file.Close(); err != nil {
				t.Error(err)
			}
		}()
		// prober использует существующую нормализацию целого AAC AU задержки без изменения видео.
		prober := fallback.Prober{Binary: filepath.Join(filepath.Dir(binaryPath), "ffprobe"), Runner: fallback.Runner{Limits: fallback.RunnerLimits{Timeout: 10 * time.Second, MaxArgs: 128, MaxCommandBytes: 16384, MaxDiagnosticBytes: 1 << 20}}, Limits: fallback.ProbeLimits{MaxJSONBytes: 1 << 20, MaxWidth: 1024, MaxHeight: 1024, MaxPixels: 1 << 20, MaxDuration: 10 * time.Second, ImageDuration: time.Second, MaxPackets: 4096, Container: integrationLimits()}}
		// normalized принимает полный файл без повторяемой начальной задержки AAC.
		var normalized bytes.Buffer
		if err = prober.NormalizePrepared(context.Background(), file, &normalized); err != nil {
			t.Fatal("Нормализация заглушки", err)
		}
		wire = normalized.Bytes()
	} else {
		hdrPixelProbe(t, binaryPath, wire, "vp9", "Profile 2", pixel)
	}
	// demux и err проверяют реальный контейнер стандартным разрешителем конфигураций.
	demux, err := fmp4.NewDemuxer(bytes.NewReader(wire), fmp4.DemuxOptions{Limits: integrationLimits(), SourceEpoch: 1, VideoRevision: 1, AudioRevision: 1})
	if err != nil {
		t.Fatal(err)
	}
	// packets сохраняет реальные времена и AU без переписывания желаемым профилем.
	packets := readMediaPackets(t, demux)
	// inspector независимо от флагов контейнера разбирает каждый сжатый заголовок.
	var inspector codec.Inspector
	// packet содержит очередную полную единицу доступа обеих дорожек.
	for _, packet := range packets {
		if _, err = inspector.Inspect(packet); err != nil {
			t.Fatal(err)
		}
	}
	return wire, packets, demux.Init()
}

// TestDefaultHDRLivePreservesTenBitCodewords проверяет через t весь штатный путь
// Registry/Group → UDS → FFmpeg → выбранный LIVE → decode. Режим x264 High444 CQP0
// сохраняет кодовые значения: FFmpeg n7.1.5 передаёт QP непосредственно в X264_RC_CQP,
// а x264 b35605a при QP0 включает кодирование без потерь также для 10-битного входа.
// Это узкий пиксельный эталон, а не требование равенства для режимов с потерями.
func TestDefaultHDRLivePreservesTenBitCodewords(t *testing.T) {
	// binaryPath включает только явно разрешённые локальные CPU-медиапроверки.
	binaryPath := os.Getenv("GOST_MEDIA_FFMPEG")
	if binaryPath == "" {
		t.Skip("CPU-проверка требует GOST_MEDIA_FFMPEG")
	}
	// sourceWire, input и inputInit содержат независимо проверенный настоящий VP9 profile2/10-bit.
	sourceWire, input, inputInit := hdrPixelFixture(t, binaryPath, false)
	// expected берётся из буквального сырого шаблона, а не результата рабочего преобразования.
	expected := hdrPixelFrame(false)[:128*128*2]
	// control намеренно теряет два младших бита до обычного x264 CQP0, сохраняя корректные HDR-теги.
	control := mediaCommand(t, binaryPath, sourceWire, "-v", "error", "-nostdin", "-xerror", "-i", "pipe:0", "-map", "0:v", "-frames:v", "1", "-vf", "zscale=dither=none,format=yuv420p,zscale=dither=none,format=yuv444p10le", "-c:v", "libx264", "-preset", "medium", "-profile:v", "high444", "-qp", "0", "-threads:v", "1", "-color_range:v", "pc", "-color_primaries:v", "bt2020", "-color_trc:v", "smpte2084", "-colorspace:v", "bt2020nc", "-f", "mp4", "-movflags", "empty_moov+frag_keyframe", "pipe:1")
	hdrPixelProbe(t, binaryPath, control, "h264", "High 4:4:4 Predictive", "yuv444p10le")
	// controlPixels подтверждает, что тот же эталон отвергает реальное 10→8→10 даже при правильных тегах.
	controlPixels := mediaCommand(t, binaryPath, control, "-v", "error", "-xerror", "-i", "pipe:0", "-map", "0:v", "-pix_fmt", "yuv444p10le", "-f", "rawvideo", "pipe:1")
	// damaged считает действительно изменённые значения Y в корректно декодированном контроле.
	damaged := hdrPixelMismatchCount(t, controlPixels, expected, 128*128*6, 1)
	if damaged == 0 {
		t.Fatal("Пиксельный эталон не обнаружил контрольное усечение до восьми бит")
	}
	t.Logf("Контроль 10→8→10: изменено %d из 16384 отсчётов Y при верных HDR-тегах", damaged)
	// profile использует существующий публичный high444/10-bit/CQP0 без дополнительных флагов.
	profile := hdrLiveProfile(core.VideoEncoderX264, "smpte2084")
	profile.Format.Video.CodecProfile, profile.Format.Video.PixelFormat = "high444", "yuv444p10le"
	profile.Format.Video.Color.Range = "full"
	profile.Encoding = core.DefaultEncoding(core.ModeTranscode, core.VideoEncoderX264, core.RateControlCQP)
	profile.Encoding.QP = core.OptionalQP{Set: true, Value: 0}
	profile.Encoding.RescaleOutput = false
	// err сообщает о недопустимом конечном профиле до создания процессов и SQLite.
	if err := profile.Validate(); err != nil {
		t.Fatal("Недопустимый пиксельный профиль", err)
	}
	// prepared содержит постоянную яркость и AAC-тишину, различимые с эталоном LIVE.
	prepared, _, _ := hdrPixelFixture(t, binaryPath, true)
	// fixture владеет отдельной временной SQLite и доменной Session без сетевого отправителя.
	fixture := newPublisherPipeline(t, []core.GroupProfile{profile}, []string{"rtmp://127.0.0.1/unused"}, []string{"synthetic-key"}, []int{0})
	// path задаёт готовый частный файл штатного проигрывателя.
	path := filepath.Join(t.TempDir(), "fallback.mp4")
	// err сохраняет отказ записи готового файла.
	if err := os.WriteFile(path, prepared, 0600); err != nil {
		t.Fatal(err)
	}
	fixture.config.Binary = binaryPath
	fixture.config.OpenFile = func(context.Context, core.ArtifactManifest) (*os.File, error) { /* Открывает частный файл; контекст и манифест не меняют выбранную тестовую фикстуру. */
		return os.Open(path)
	}
	fixture.config.FFmpeg.StartupTimeout = 10 * time.Second
	fixture.config.Logger = slog.New(slog.NewTextHandler(os.Stderr, nil))
	if fixture.config.Transcode != nil || fixture.config.Player != nil {
		t.Fatal("Требуются штатные построители LIVE и заглушки")
	}
	// registry и err сохраняют владельца реальных работников и причинные отказы выполнения.
	registry, err := group.NewRegistry(context.Background(), group.RegistryConfig{Core: fixture.service, Parent: fixture.parent, Observer: fixture.observer, Runtime: fixture.config, MaxGroups: 2, MaxReferences: 2})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { /* Закрывает и присоединяет работников до удаления UDS и SQLite. */
		// closeErr сообщает о неподтверждённой очистке собственной группы.
		if closeErr := registry.Close(context.Background()); closeErr != nil {
			t.Error(closeErr)
		}
	})
	// reference удерживает настоящую группу и её стабильный выходной Router.
	reference, err := registry.Acquire(context.Background(), fixture.outputs[0].Snapshot, fixture.live)
	if err != nil {
		t.Fatal(err)
	}
	// readyContext ограничивает ожидание штатной заглушки; stopReady освобождает таймер.
	readyContext, stopReady := context.WithTimeout(context.Background(), 10*time.Second)
	err = reference.Group().WaitReady(readyContext)
	stopReady()
	if err != nil {
		t.Fatal("Штатная заглушка не готова", err)
	}
	// tap принимает реальные выходные AU после выбора источника группой.
	tap, err := reference.Router().Subscribe(publisherQueue())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { /* Освобождает резерв выходной подписки после завершения захвата. */
		_ = tap.Close()
	}()
	// source обозначает единственную публикацию настоящих VP9/AAC AU.
	source, err := fixture.live.BeginSource()
	if err != nil {
		t.Fatal(err)
	}
	if err = source.UpdateConfigs(inputInit.Video, inputInit.Audio); err != nil {
		t.Fatal(err)
	}
	// ctx ограничивает весь LIVE; cancel прерывает производителя на любом пути выхода.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// producerDone подтверждает завершение подачи и переносит её причинную ошибку.
	producerDone := make(chan error, 1)
	go func() { /* Подаёт пять конечных петель с исходными AU и точными возрастающими метками. */
		// start связывает срок подачи каждого AU с локальными монотонными часами.
		start := time.Now()
		// cycle добавляет точные 1,6 секунды к обеим шкалам без изменения сжатых данных.
		for cycle := int64(0); cycle < 5; cycle++ {
			// packet обозначает очередной исходный AU видео либо AAC.
			for _, packet := range input {
				// info и base сохраняют исходные метки и единицу времени данной дорожки.
				info, base := packet.Info(), packet.Config().Info().TimeBase
				if packet.Config().Info().Track == media.TrackAudio && info.DTS < 0 {
					continue
				}
				// shift добавляет длительность завершённых петель в точных единицах дорожки.
				shift := cycle * 8 * int64(base.Denominator) / (5 * int64(base.Numerator))
				info.PTS, info.DTS = info.PTS+shift, info.DTS+shift
				// at задаёт срок подачи пакета относительно начала синтетического LIVE.
				at := time.Duration(info.DTS) * time.Second * time.Duration(base.Numerator) / time.Duration(base.Denominator)
				// timer ограничивает ожидание конкретного AU и освобождается при отмене.
				timer := time.NewTimer(max(time.Until(start.Add(at)), 0))
				select {
				case <-ctx.Done():
					timer.Stop()
					producerDone <- nil
					return
				case <-timer.C:
				}
				// repeated сохраняет исходные байты и конфигурацию; publishErr передаёт отказ создания либо публикации.
				repeated, publishErr := media.NewPacket(info, packet.Config(), packet.Payload())
				if publishErr == nil {
					publishErr = source.Publish(repeated)
				}
				if publishErr != nil {
					producerDone <- publishErr
					return
				}
			}
		}
		producerDone <- nil
	}()
	defer func() { /* Отменяет и присоединяет производителя до освобождения входного Router. */
		cancel()
		// producerErr сохраняет причинный отказ конечной подачи LIVE.
		if producerErr := <-producerDone; producerErr != nil {
			t.Error(producerErr)
		}
	}()
	// output накапливает только актуальный LIVE, начиная с независимого видеокадра.
	var output []media.Packet
	// actual хранит реальные конфигурации; videoCount ограничивает захват восемнадцатью кадрами.
	var actual fmp4.Init
	videoCount := 0
	for videoCount < 18 {
		// delivery и nextErr содержат очередной выход группы и ошибку ограниченного ожидания.
		delivery, nextErr := tap.Next(ctx)
		if nextErr != nil {
			t.Fatal("Не получен выбранный LIVE", nextErr, reference.Group().Status())
		}
		if reference.Group().Status().Source != group.Live || !delivery.IsCurrent() {
			continue
		}
		if len(output) == 0 && (delivery.Packet.Config().Info().Track != media.TrackVideo || !delivery.Packet.Info().Keyframe) {
			continue
		}
		actual = fmp4.Init{Video: delivery.VideoConfig, Audio: delivery.AudioConfig}
		output = append(output, delivery.Packet)
		if delivery.Packet.Config().Info().Track == media.TrackVideo {
			videoCount++
		}
	}
	if err = core.CompatibleProfileFormat(core.MediaFormat{Video: actual.Video.Info().Video, Audio: actual.Audio.Info().Audio}, profile); err != nil {
		t.Fatal("Фактический выход группы", err)
	}
	if err = reference.Group().Ready(); err != nil {
		t.Fatal("Заглушка отказала во время LIVE", err)
	}
	// encoded сохраняет только AU рабочего выхода, без повторного видеокодирования.
	var encoded bytes.Buffer
	// mux и err проверяют контейнерную упаковку реального результата.
	mux, err := fmp4.NewMuxer(&encoded, actual, integrationLimits())
	if err != nil {
		t.Fatal(err)
	}
	// audioCount исключает ложный успех без фактической выбранной AAC-дорожки.
	audioCount := 0
	// packet содержит очередной выбранный выход группы для независимого декодирования.
	for _, packet := range output {
		if packet.Config().Info().Track == media.TrackAudio {
			audioCount++
		}
		if err = mux.WritePacket(packet); err != nil {
			t.Fatal(err)
		}
	}
	if audioCount < 10 {
		t.Fatal("Недостаточно фактических LIVE AAC AU", audioCount)
	}
	hdrPixelProbe(t, binaryPath, encoded.Bytes(), "h264", "High 4:4:4 Predictive", "yuv444p10le")
	// decoded содержит все реальные выходные YUV444P10-кадры без смены пиксельного формата.
	decoded := mediaCommand(t, binaryPath, encoded.Bytes(), "-v", "error", "-xerror", "-i", "pipe:0", "-map", "0:v", "-pix_fmt", "yuv444p10le", "-fps_mode", "passthrough", "-f", "rawvideo", "pipe:1")
	// mismatches сравнивает каждый Y с исходным буквальным шаблоном на всей выбранной последовательности.
	if mismatches := hdrPixelMismatchCount(t, decoded, expected, 128*128*6, 18); mismatches != 0 {
		t.Fatalf("Штатный LIVE изменил %d из %d десятибитных отсчётов Y", mismatches, 18*128*128)
	}
	t.Log("Штатный LIVE сохранил все 1024 значения Y в каждом из 18 декодированных кадров")
	cancel()
	if err = registry.Close(context.Background()); err != nil || !registry.Joined() {
		t.Fatal("Группа не подтвердила завершение", err)
	}
	// entries подтверждает удаление всех UDS-пространств после присоединения работников.
	entries, err := os.ReadDir(fixture.config.FFmpeg.BaseDir)
	if err != nil || len(entries) != 0 {
		t.Fatal("Остались собственные UDS-ресурсы", entries, err)
	}
}
