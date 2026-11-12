package integration

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/big"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
	"github.com/GregoryGost/gost-restreamer/internal/media/rtmp"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
	"github.com/bluenviron/gortmplib"
	"github.com/bluenviron/gortmplib/pkg/amf0"
	"github.com/bluenviron/gortmplib/pkg/message"
)

// При заданном GOST_MEDIA_FFMPEG проверяет прохождение настоящих AV1/AAC-пакетов через RTMP-приём и
// fMP4 без изменения конфигураций, содержимого и временных меток. Отдельное декодирование подтверждает
// наличие обеих дорожек.
func TestAcceptanceCPUEncodedAV1ThroughRealIngestAndFMP4(t *testing.T) {
	// binaryPath содержит явно разрешённый путь FFmpeg, без которого CPU-сценарий пропускается.
	binaryPath := os.Getenv("GOST_MEDIA_FFMPEG")
	if binaryPath == "" {
		t.Skip("локальная CPU-интеграция запускается только с явно заданным FFmpeg")
	}
	// source содержит реально закодированный AV1/AAC fMP4 для прохождения через ingest.
	source := mediaCommand(t, binaryPath, nil, "-hide_banner", "-loglevel", "warning", "-xerror", "-f", "lavfi", "-i", "testsrc2=size=128x128:rate=30:duration=0.4", "-f", "lavfi", "-i", "sine=sample_rate=48000:duration=0.4", "-map", "0:v", "-map", "1:a", "-c:v", "libaom-av1", "-threads:v", "1", "-cpu-used", "8", "-lag-in-frames", "0", "-c:a", "aac", "-f", "mp4", "-movflags", "delay_moov+default_base_moof+frag_every_frame", "-min_frag_duration", "1", "-use_editlist", "1", "-movie_timescale", "48000", "-avoid_negative_ts", "disabled", "pipe:1")
	// oracle содержит независимый ffprobe-разбор исходных дорожек и пакетов.
	oracle := probeMedia(t, filepath.Join(filepath.Dir(binaryPath), "ffprobe"), source)
	// d содержит демультиплексор исходного fMP4; err содержит отказ открытия с фактическими
	// конфигурациями.
	d, err := fmp4.NewDemuxer(bytes.NewReader(source), fmp4.DemuxOptions{Limits: integrationLimits(), ResolveConfig: truthfulResolver(t, oracle)})
	if err != nil {
		t.Fatal(err)
	}
	// packets сохраняет исходные медиапакеты после проверки относительно ffprobe.
	packets := readMediaPackets(t, d)
	assertProbePackets(t, oracle, packets)
	// ctx ограничивает доменную настройку и приём десятью секундами; cancel освобождает таймер.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// store содержит изолированную SQLite с реальными миграциями и явными бюджетами.
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "ingest.sqlite"), storage.Options{PreparationLimits: core.PreparationLimits{MaxQueued: 16, MaxProfiles: 64}, OutputExecutionLimits: core.OutputExecutionLimits{MaxReservations: 128, MaxIdentifierBytes: 1024, MaxBytes: 4 << 20}, ObservationLimits: core.ObservationLimits{MaxSessions: 16, MaxGroups: 64, MaxOutputs: 128, MaxFFmpegWorkers: 64, MaxIdentifierBytes: 1024, MaxErrorBytes: 65536, MaxBytes: 4 << 20}, MaxOpenConns: 4, MaxIdleConns: 4, ConnMaxLifetime: time.Minute, ConnMaxIdleTime: time.Minute, BusyTimeout: 10 * time.Millisecond, RetryAttempts: 3, RetryTimeout: time.Second, RetryDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает временное хранилище после проверки настоящего входа RTMP.

		// err содержит результат закрытия Store после остановки приёмника.
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	// svc содержит доменный сервис единственного Store.
	svc, err := core.NewService(store, store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	// asset содержит минимальную заглушку без медиагрупп для допуска входной Session.
	asset, err := svc.CreateAsset(ctx, "Оригинал")
	if err != nil {
		t.Fatal(err)
	}
	// stream содержит Stream с синтетическим постоянным ключом и политикой заглушки.
	stream, err := svc.CreateStream(ctx, core.Stream{Name: "Вход", PublicationKey: "synthetic-key", Fallback: core.FallbackPolicy{AssetID: asset.ID, Watchdog: core.WatchdogPolicy{VideoTimeout: time.Second, AudioTimeout: time.Second, RecoveryWindow: time.Second, KeyframeTimeout: time.Second}}})
	if err != nil {
		t.Fatal(err)
	}
	// preparation содержит зарезервированное поколение минимальных метаданных заглушки.
	preparation, err := svc.ReservePreparation(ctx, core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage, GeneratorVersion: "fixture-generator"})
	if err != nil {
		t.Fatal(err)
	}
	// preparationFence содержит точную попытку этой подготовки для фиксации и публикации.
	preparationFence, err := svc.ClaimPreparation(ctx, preparation.ID)
	if err != nil {
		t.Fatal(err)
	}
	// err содержит отказ фиксации выдачи подготовки метаданных.
	if err := svc.MarkPreparationIssued(ctx, preparationFence); err != nil {
		t.Fatal(err)
	}
	// Фикстура задаёт только метаданные заглушки без групп; медиабайты здесь проверяются на границе приёма потока.
	// err содержит отказ публикации минимального поколения без требуемых вариантов.
	if _, err := svc.PublishPreparation(ctx, preparationFence, core.GenerationPublication{SourceHash: "synthetic-source", SizeBytes: 10, SourceFormat: core.SourceFormat{ImageFormat: "png"}}); err != nil {
		t.Fatal(err)
	}
	// err содержит отказ подтверждения очистки подготовки перед запуском Session.
	if err := svc.AcknowledgePreparationCleanup(ctx, preparationFence); err != nil {
		t.Fatal(err)
	}
	// start содержит принятую команду запуска Stream без медиагрупп.
	start, err := svc.StartSession(ctx, core.StartSessionRequest{RequestID: "start-cpu-source", StreamID: stream.ID})
	if err != nil {
		t.Fatal(err)
	}
	// fence содержит точную захваченную команду запуска для MarkIssued и Complete.
	fence, err := svc.Claim(ctx, start.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	// err содержит отказ фиксации выдачи команды запуска.
	if err := svc.MarkIssued(ctx, fence); err != nil {
		t.Fatal(err)
	}
	// err содержит отказ успешного завершения команды запуска для допуска OBS.
	if err := svc.Complete(ctx, fence, core.Completion{Success: true}); err != nil {
		t.Fatal(err)
	}
	// router содержит ограниченный маршрутизатор фактически принятых медиапакетов.
	router, err := media.NewRouter(media.RouterLimits{MaxConsumers: 2, MaxRetainedBytes: 4 << 20, MaxConfigBytes: 4096})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает маршрутизатор принятых медиапакетов.
		_ = router.Close()
	})
	// sub содержит ограниченную подписку, через которую тест читает результат ingest.
	sub, err := router.Subscribe(media.QueueLimits{MaxPackets: 512, MaxBytes: 2 << 20, MaxPacketBytes: 1 << 18, MaxConfigBytes: 4096, ResyncTimeout: 5 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Освобождает подписку на пакеты после проверки fMP4.
		_ = sub.Close()
	})
	// limits задаёт конечные бюджеты входного RTMP и сроки соединений сценария.
	limits := rtmp.Limits{MaxConnections: 2, MaxChunkStreams: 8, MaxMessageBytes: 1 << 20, MaxChunkBytes: 65536, MaxFragments: 4096, MaxRetainedBytes: 32 << 20, MaxControlBytes: 64, MaxConfigBytes: 4096, MaxAMFBytes: 4096, MaxAMFDepth: 8, MaxAMFElements: 128, MaxReadAheadBytes: 65554, HandshakeTimeout: 3 * time.Second, ReadTimeout: 5 * time.Second, WriteTimeout: 3 * time.Second, ShutdownTimeout: 2 * time.Second}
	// server содержит настоящий RTMP-сервер, допускающий только точную созданную Session.
	server, err := rtmp.NewServer(rtmp.Options{Limits: limits, Core: svc, Route: rtmp.ConventionalRoute, Feed: func(session core.Session) (*media.Router, error) {
		// Возвращает маршрутизатор только точной тестовой Session, отклоняя устаревшее выполнение.

		if session.ID != start.SessionID {
			return nil, core.ErrStale
		}
		return router, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	// listener содержит локальный TCP-слушатель на свободном порту для настоящего RTMP-входа.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// serveCtx управляет приёмником внутри срока сценария; stopServe отменяет его при очистке.
	serveCtx, stopServe := context.WithCancel(ctx)
	// serveDone передаёт результат единственной горутины Serve для ограниченного присоединения.
	serveDone := make(chan error, 1)
	go func() {
		// Обслуживает RTMP-вход параллельно отправке и проверке пакетов.
		serveDone <- server.Serve(serveCtx, listener)
	}()
	t.Cleanup(func() {
		// Отменяет приёмник и ограниченно ожидает его завершения с проверкой причины остановки.

		stopServe()
		// cleanup задаёт отдельный трёхсекундный срок остановки после отмены; stop освобождает
		// таймер.
		cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		// err содержит результат Shutdown RTMP-сервера при очистке.
		if err := server.Shutdown(cleanup); err != nil {
			t.Error(err)
		}
		select {
		// err содержит возвращённую причину Serve после stopServe для проверки штатной отмены.
		case err := <-serveDone:
			if !errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
				t.Errorf("неожиданный результат Serve после отмены: %v", err)
			}
		case <-cleanup.Done():
			t.Error("ingest не завершился")
		}
	})
	// client публикует реальные AV1/AAC-пакеты с постоянным ключом созданного Stream.
	client := &gortmplib.Client{URL: &url.URL{Scheme: "rtmp", Host: listener.Addr().String(), Path: "/" + string(stream.ID), Fragment: stream.PublicationKey}, Publish: true}
	// err содержит отказ начального RTMP-согласования клиента.
	if err := client.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	// err содержит отказ установки конечного срока записи исходного клиента.
	if err := client.NetConn().SetWriteDeadline(time.Now().Add(5 * time.Second)); err != nil {
		t.Fatal(err)
	}
	// err содержит отказ установки RTMP-блока размером 128 байтов.
	if err := client.Write(&message.SetChunkSize{Value: 128}); err != nil {
		t.Fatal(err)
	}
	// err содержит отказ передачи метаданных исходной частоты видео 30 FPS.
	if err := client.Write(&message.DataAMF0{ChunkStreamID: 5, MessageStreamID: 1, Payload: amf0.Data{"onMetaData", amf0.Object{{Key: "framerate", Value: float64(30)}}}}); err != nil {
		t.Fatal(err)
	}
	// writeRaw собирает и отправляет RTMP-сообщение с 128-байтовыми фрагментами и добавляет
	// результат ingest к диагностике отказа.
	writeRaw := func(id, typ byte, at uint32, body []byte) {
		// Формирует RTMP-сообщение с фрагментами по 128 байт и добавляет результат приёмника к диагностике
		// ошибки отправки.

		t.Helper()
		// header содержит начальный RTMP-заголовок сообщения с его длиной, временем, типом и
		// потоком.
		header := []byte{id, byte(at >> 16), byte(at >> 8), byte(at), byte(len(body) >> 16), byte(len(body) >> 8), byte(len(body)), typ, 1, 0, 0, 0}
		// data собирает точные байты заголовка, тела и продолжений RTMP-сообщения.
		var data []byte
		data = append(data, header...)
		for len(body) > 0 {
			// n задаёт размер очередного фрагмента тела, не превышающий 128 байтов.
			n := min(len(body), 128)
			data = append(data, body[:n]...)
			body = body[n:]
			if len(body) > 0 {
				data = append(data, 0xc0|id)
			}
		}
		// err содержит отказ передачи полного собранного RTMP-сообщения.
		if _, err := io.Copy(client.NetConn(), bytes.NewReader(data)); err != nil {
			select {
			// result содержит ближайший результат входного соединения для диагностики ошибки
			// записи источника.
			case result := <-server.Results():
				t.Fatalf("запись источника: %v; ingest: %v", err, result.Err)
			case <-ctx.Done():
				t.Fatal(err)
			}
		}
	}
	// init содержит исходные конфигурации дорожек, чьи заголовки передаются RTMP без изменения.
	init := d.Init()
	writeRaw(6, 9, 0, append([]byte{0x90, 'a', 'v', '0', '1'}, init.Video.Headers()...))
	if init.Audio.Info().Audio.SampleRateHz != 48000 || init.Audio.Info().Audio.Channels != 1 {
		t.Fatal("генератор не дал ожидаемый AAC-LC 48 kHz mono")
	}
	writeRaw(4, 8, 0, append([]byte{0xaf, 0}, init.Audio.Headers()...))
	// expectedPacket сохраняет ожидаемое RTMP-время и точные байты принятой единицы доступа.
	type expectedPacket struct {
		// at содержит ожидаемую временную метку пакета в миллисекундах RTMP.
		at int64
		// payload содержит точные байты AU, ожидаемые после входной RTMP-распаковки.
		payload []byte
	}
	// want собирает ожидаемые пакеты отдельно по видео и аудио с учётом начального отсечения аудио.
	want := map[media.Track][]expectedPacket{}
	// firstVideo сохраняет миллисекундное время первого видеопакета со сдвигом 200 мс.
	firstVideo := int64(-1)
	// p содержит очередной исходный пакет при поиске первого видео.
	for _, p := range packets {
		if p.Config().Info().Track == media.TrackVideo {
			// b содержит временную базу первого видеопакета для точного перевода PTS в
			// миллисекунды.
			b := p.Config().Info().TimeBase
			// value содержит целочисленное время первого видео в миллисекундах до добавления
			// 200 мс.
			value := new(big.Int).Mul(big.NewInt(p.Info().PTS), big.NewInt(int64(b.Numerator)*1000))
			value.Quo(value, big.NewInt(int64(b.Denominator)))
			firstVideo = value.Int64() + 200
			break
		}
	}
	// p содержит очередной исходный AV1 или AAC пакет для RTMP-передачи.
	for _, p := range packets {
		// track определяет видео или аудио текущего пакета.
		track := p.Config().Info().Track
		// b содержит исходную временную базу текущего PTS.
		b := p.Config().Info().TimeBase
		// value содержит целочисленное время текущего PTS в миллисекундах без переполнения
		// промежуточного произведения.
		value := new(big.Int).Mul(big.NewInt(p.Info().PTS), big.NewInt(int64(b.Numerator)*1000))
		value.Quo(value, big.NewInt(int64(b.Denominator)))
		// at задаёт RTMP-время текущего пакета после сдвига на 200 мс.
		at := value.Int64() + 200
		// payload содержит исходный AU и для ключевого AV1 дополняется последовательностью
		// заголовка перед отправкой.
		payload := p.Payload()
		if track == media.TrackVideo {
			// flag выбирает ключевой или межкадровый Enhanced RTMP AV1-пакет.
			flag := byte(0xa1)
			if p.Info().Keyframe {
				flag = 0x91
				payload = append(init.Video.Headers()[4:], payload...)
			}
			writeRaw(6, 9, uint32(at), append([]byte{flag, 'a', 'v', '0', '1'}, payload...))
		} else {
			writeRaw(4, 8, uint32(at), append([]byte{0xaf, 1}, payload...))
		}
		if track == media.TrackVideo || at >= firstVideo {
			want[track] = append(want[track], expectedPacket{at, payload})
		}
	}
	// count содержит ожидаемое число принятых видеопакетов и неотсечённых аудиопакетов.
	count := len(want[media.TrackVideo]) + len(want[media.TrackAudio])
	// seen считает фактически полученные пакеты каждой дорожки для сравнения с want.
	seen := map[media.Track]int{}
	// gotPackets сохраняет проверенные пакеты ingest для последующего точного fMP4-цикла.
	var gotPackets []media.Packet
	// out накапливает fMP4 из фактически доставленных пакетов.
	var out bytes.Buffer
	// mux создаётся по конфигурациям первой доставки и записывает принятые AU в out.
	var mux *fmp4.Muxer
	for range count {
		// delivery содержит очередную доставку подписки; err содержит отказ чтения до проверки
		// результата ingest.
		delivery, err := sub.Next(ctx)
		if err != nil {
			select {
			// result содержит диагностический результат RTMP-соединения после отказа чтения
			// подписки.
			case result := <-server.Results():
				t.Fatalf("ingest: %v; Next: %v", result.Err, err)
			default:
				t.Fatal(err)
			}
		}
		// p содержит фактически принятый пакет текущей доставки.
		p := delivery.Packet
		if !bytes.Equal(delivery.VideoConfig.Headers(), init.Video.Headers()) || !bytes.Equal(delivery.AudioConfig.Headers(), init.Audio.Headers()) {
			t.Fatal("ingest изменил исходные AV1/AAC headers реального кодировщика")
		}
		// track определяет дорожку принятого пакета для выбора ожидаемой последовательности.
		track := p.Config().Info().Track
		// index задаёт порядковую позицию текущего пакета внутри принятой дорожки.
		index := seen[track]
		seen[track]++
		if index >= len(want[track]) {
			t.Fatal("ingest добавил пакет")
		}
		// expected содержит ожидаемые время и payload соответствующего пакета дорожки.
		expected := want[track][index]
		// base содержит временную базу принятого пакета для точного рационального сравнения.
		base := p.Config().Info().TimeBase
		// pts содержит точное время PTS в секундах без округления для сравнения с
		// expected.at/1000.
		pts := new(big.Rat).Mul(new(big.Rat).SetInt64(p.Info().PTS), new(big.Rat).SetFrac64(int64(base.Numerator), int64(base.Denominator)))
		if !delivery.IsCurrent() || pts.Cmp(new(big.Rat).SetFrac64(expected.at, 1000)) != 0 || p.Info().PTS != p.Info().DTS || !bytes.Equal(p.Payload(), expected.payload) {
			t.Fatalf("ingest изменил wire TU/AU %s[%d]", track, index)
		}
		if mux == nil {
			mux, err = fmp4.NewMuxer(&out, fmp4.Init{Video: delivery.VideoConfig, Audio: delivery.AudioConfig}, integrationLimits())
			if err != nil {
				t.Fatal(err)
			}
		}
		// err содержит отказ записи проверенного принятого пакета в fMP4.
		if err := mux.WritePacket(p); err != nil {
			t.Fatal(err)
		}
		gotPackets = append(gotPackets, p)
	}
	if seen[media.TrackVideo] != 12 || seen[media.TrackAudio] == 0 {
		t.Fatalf("неполный encoded ingest: %+v", seen)
	}
	// returned содержит демультиплексор контейнера, собранного из фактического ingest.
	returned, err := fmp4.NewDemuxer(bytes.NewReader(out.Bytes()), fmp4.DemuxOptions{Limits: integrationLimits()})
	if err != nil {
		t.Fatal(err)
	}
	// want содержит очередной проверенный пакет ingest для сравнения после fMP4-цикла.
	for _, want := range gotPackets {
		// got содержит пакет после повторного чтения fMP4; err содержит отказ его чтения.
		got, err := returned.Next()
		if err != nil || got.Info() != want.Info() || !bytes.Equal(got.Payload(), want.Payload()) {
			t.Fatalf("fMP4 изменил фактический ingest: %v", err)
		}
	}
	// err содержит результат чтения после ожидаемых пакетов, который должен быть EOF.
	if _, err := returned.Next(); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
	// decode содержит результат реального декодирования обеих дорожек из fMP4 в framemd5.
	decode := mediaCommand(t, binaryPath, out.Bytes(), "-hide_banner", "-loglevel", "warning", "-xerror", "-copyts", "-i", "pipe:0", "-map", "0:v", "-map", "0:a", "-fps_mode", "passthrough", "-f", "framemd5", "pipe:1")
	if bytes.Count(decode, []byte("\n0,")) != 12 || bytes.Count(decode, []byte("\n1,")) == 0 {
		t.Fatal("пакеты настоящего ingest не декодируются в обе дорожки")
	}
}
