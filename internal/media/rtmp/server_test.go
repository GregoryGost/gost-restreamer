package rtmp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"net/url"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
	"github.com/bluenviron/gortmplib"
	"github.com/bluenviron/gortmplib/pkg/amf0"
	"github.com/bluenviron/gortmplib/pkg/message"
)

// ingestFixture объединяет изолированное хранилище, доменный сервис, активную сессию и маршрутизатор для
// сетевых проверок приёма.
type ingestFixture struct {
	// store содержит временное SQLite-хранилище тестового входа.
	store *storage.Store
	// svc содержит доменный сервис текущего воплощения хранилища.
	svc *core.Service
	// stream содержит поток с постоянным ключом и выбранной заглушкой.
	stream core.Stream
	// session сохраняет квитанцию запуска активной тестовой Session.
	session core.CommandReceipt
	// router принимает пакеты допущенной публикации и выдаёт их подписчикам.
	router *media.Router
}

// Создаёт изолированную SQLite, core.Service, подготовленную заглушку, активную сессию и Router;
// закрытие хранилища и маршрутизатора регистрирует в очистке теста.
func newIngestFixture(t *testing.T) ingestFixture {
	t.Helper()
	// ctx задаёт контекст создания изолированных тестовых данных.
	ctx := context.Background()
	// store содержит открытое временное хранилище. err сообщает об отказе открытия.
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "ingest.sqlite"), storage.Options{PreparationLimits: core.PreparationLimits{MaxQueued: 8, MaxProfiles: 64}, OutputExecutionLimits: core.OutputExecutionLimits{MaxReservations: 128, MaxIdentifierBytes: 1024, MaxBytes: 4 << 20}, ObservationLimits: core.ObservationLimits{MaxSessions: 16, MaxGroups: 64, MaxOutputs: 128, MaxFFmpegWorkers: 64, MaxIdentifierBytes: 1024, MaxErrorBytes: 65536, MaxBytes: 4 << 20}, MaxOpenConns: 4, MaxIdleConns: 4, ConnMaxLifetime: time.Minute, ConnMaxIdleTime: time.Minute, BusyTimeout: 10 * time.Millisecond, RetryAttempts: 3, RetryTimeout: time.Second, RetryDelay: time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает временное хранилище после теста и сообщает ошибку закрытия.

		// err сообщает об отказе закрытия временного хранилища.
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	// svc содержит доменный сервис открытого хранилища.
	svc, err := core.NewService(store, store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	// asset содержит созданную заглушку тестового потока.
	asset, err := svc.CreateAsset(ctx, "Оригинал")
	if err != nil {
		t.Fatal(err)
	}
	// generation содержит зарезервированное синтетическое поколение заглушки.
	generation, err := svc.ReservePreparation(ctx, core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage, GeneratorVersion: "synthetic"})
	if err != nil {
		t.Fatal(err)
	}
	// preparation содержит выданный допуск подготовки для публикации синтетического поколения.
	preparation, err := svc.ClaimPreparation(ctx, generation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.MarkPreparationIssued(ctx, preparation); err != nil {
		t.Fatal(err)
	}
	if _, err = svc.PublishPreparation(ctx, preparation, core.GenerationPublication{SourceHash: "synthetic-content", SizeBytes: 10, SourceFormat: core.SourceFormat{ImageFormat: "png"}}); err != nil {
		t.Fatal(err)
	}
	if err = svc.AcknowledgePreparationCleanup(ctx, preparation); err != nil {
		t.Fatal(err)
	}
	// stream содержит созданный поток с ключом и выбранной заглушкой.
	stream, err := svc.CreateStream(ctx, core.Stream{Name: "Вход", PublicationKey: "synthetic-ingest-key", Fallback: core.FallbackPolicy{AssetID: asset.ID, Watchdog: core.WatchdogPolicy{VideoTimeout: time.Second, AudioTimeout: time.Second, RecoveryWindow: time.Second, KeyframeTimeout: time.Second}}})
	if err != nil {
		t.Fatal(err)
	}
	// start содержит квитанцию запуска сессии, доводимую до успешного Complete.
	start, err := svc.StartSession(ctx, core.StartSessionRequest{RequestID: "start-ingest", StreamID: stream.ID})
	if err != nil {
		t.Fatal(err)
	}
	complete(t, svc, start)
	// router содержит отдельный маршрутизатор с конечными бюджетами.
	router, err := media.NewRouter(media.RouterLimits{MaxConsumers: 8, MaxRetainedBytes: 16 << 20, MaxConfigBytes: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает маршрутизатор тестового входа после теста.
		_ = router.Close()
	})
	return ingestFixture{store, svc, stream, start, router}
}

// Проводит тестовую команду через Claim, MarkIssued и успешный Complete, завершая тест при любом
// отказе.
func complete(t *testing.T, svc *core.Service, r core.CommandReceipt) {
	t.Helper()
	// ctx задаёт контекст выполнения тестовой команды.
	ctx := context.Background()
	// f содержит допуск команды после Claim. err сообщает об отказе захвата.
	f, err := svc.Claim(ctx, r.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.MarkIssued(ctx, f); err != nil {
		t.Fatal(err)
	}
	if err = svc.Complete(ctx, f, core.Completion{Success: true}); err != nil {
		t.Fatal(err)
	}
}

// Создаёт подписку с явными бюджетами и сроком синхронизации две секунды; закрывает её при очистке
// теста.
func newSubscription(t *testing.T, r *media.Router) *media.Subscription {
	t.Helper()
	// s содержит созданную подписку с двухсекундным сроком синхронизации. err сообщает об отказе
	// резервирования.
	s, err := r.Subscribe(media.QueueLimits{MaxPackets: 30, MaxBytes: 1 << 20, MaxPacketBytes: 1 << 18, MaxConfigBytes: 1 << 16, ResyncTimeout: 2 * time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает созданную тестовую подписку после теста.
		_ = s.Close()
	})
	return s
}

// Запускает приёмный Server на отдельном локальном TCP-порту с выбранным FeedResolver и регистрирует
// отмену, Shutdown и ожидание Serve в очистке теста.
func runServer(t *testing.T, f ingestFixture, feed FeedResolver) (*Server, string) {
	t.Helper()
	if feed == nil {
		feed = func(session core.Session) (*media.Router, error) {
			// Возвращает маршрутизатор только для ожидаемой Session, отклоняя несовпадающий ID с core.ErrStale.

			if session.ID != f.session.SessionID {
				return nil, core.ErrStale
			}
			return f.router, nil
		}
	}
	// s содержит входящий сервер с выбранным FeedResolver. err сообщает об отказе создания.
	s, err := NewServer(Options{Limits: testLimits(), Core: f.svc, Route: ConventionalRoute, Feed: feed})
	if err != nil {
		t.Fatal(err)
	}
	// l содержит локальный TCP-слушатель со свободным портом.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// ctx задаёт срок жизни Serve. cancel останавливает сервер при очистке теста.
	ctx, cancel := context.WithCancel(context.Background())
	// done передаёт результат Serve и позволяет дождаться его завершения.
	done := make(chan error, 1)
	go func() {
		// Запускает Serve на локальном слушателе и передаёт результат завершения через done.
		done <- s.Serve(ctx, l)
	}()
	t.Cleanup(func() {
		// Отменяет Serve, вызывает Shutdown с отдельным сроком и проверяет фактический выход Serve с
		// допустимой причиной.

		cancel()
		// stopCtx ограничивает Shutdown двумя секундами. stopCancel освобождает срок.
		stopCtx, stopCancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer stopCancel()
		// err сообщает об отказе завершения сервера в срок.
		if err := s.Shutdown(stopCtx); err != nil {
			t.Error(err)
		}
		select {
		// err содержит фактический результат завершившегося Serve.
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, net.ErrClosed) {
				t.Error(err)
			}
		case <-stopCtx.Done():
			t.Error("Serve не завершился")
		}
	})
	return s, l.Addr().String()
}

// Подключает тестовый gortmplib.Client к Stream с заданным ключом и сроком запуска две секунды;
// успешное соединение закрывает очистка теста.
func connectClient(t *testing.T, address string, stream core.Stream, key string) (*gortmplib.Client, error) {
	t.Helper()
	// u содержит URL публикации, где ключ передаётся тестовой библиотеке отдельно через Fragment.
	u := &url.URL{Scheme: "rtmp", Host: address, Path: "/" + string(stream.ID), Fragment: key}
	// c содержит клиент gortmplib в режиме публикации.
	c := &gortmplib.Client{URL: u, Publish: true}
	// ctx ограничивает подключение двумя секундами. cancel освобождает срок.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// err сообщает об отказе согласования и допуска публикации.
	if err := c.Initialize(ctx); err != nil {
		return nil, err
	}
	t.Cleanup(c.Close)
	// err сообщает об отказе установки срока тестового сокета.
	if err := c.NetConn().SetDeadline(time.Now().Add(2 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return c, nil
}

// Дописывает все сырые RTMP-байты в сокет клиента, завершая тест при ошибке или отсутствии
// продвижения.
func writeWire(t *testing.T, c *gortmplib.Client, b []byte) {
	t.Helper()
	for len(b) > 0 {
		// n содержит число записанных сырых байтов RTMP. err сообщает об отказе записи.
		n, err := c.NetConn().Write(b)
		if err != nil {
			t.Fatal(err)
		}
		if n == 0 {
			t.Fatal(io.ErrShortWrite)
		}
		b = b[n:]
	}
}

// Отправляет метаданные 30 FPS, конфигурации AV1/AAC и независимую пару A/V на заданной метке.
func sendReady(t *testing.T, c *gortmplib.Client, at uint32) {
	t.Helper()
	// err сообщает об отказе отправки метаданных 30 FPS перед заголовками и A/V.
	if err := c.Write(&message.DataAMF0{ChunkStreamID: 5, MessageStreamID: 1, Payload: amf0.Data{"onMetaData", amf0.Object{{Key: "framerate", Value: float64(30)}}}}); err != nil {
		t.Fatal(err)
	}
	writeWire(t, c, chunk0(6, 9, append([]byte{0x90, 'a', 'v', '0', '1'}, testAV1Config()...)))
	writeWire(t, c, chunk0(4, 8, []byte{0xaf, 0, 0x11, 0x90}))
	sendVideo(t, c, at, testAV1TU())
	sendAudio(t, c, at, []byte{1, 2, 3})
}

// Отправляет Enhanced AV1 CodedFrames с заданной 24-битной меткой и исходным TU.
func sendVideo(t *testing.T, c *gortmplib.Client, at uint32, tu []byte) {
	t.Helper()
	// b содержит полное AV1 CodedFrames с подставляемой абсолютной меткой.
	b := chunk0(6, 9, append([]byte{0x91, 'a', 'v', '0', '1'}, tu...))
	b[1] = byte(at >> 16)
	b[2] = byte(at >> 8)
	b[3] = byte(at)
	writeWire(t, c, b)
}

// Отправляет обычный RTMP AAC AU с заданной 24-битной меткой и исходной нагрузкой.
func sendAudio(t *testing.T, c *gortmplib.Client, at uint32, au []byte) {
	t.Helper()
	// b содержит полное legacy AAC-сообщение с подставляемой абсолютной меткой.
	b := chunk0(4, 8, append([]byte{0xaf, 1}, au...))
	b[1] = byte(at >> 16)
	b[2] = byte(at >> 8)
	b[3] = byte(at)
	writeWire(t, c, b)
}

// Получает следующую доставку за две секунды; ошибка или таймаут завершают тест.
func delivery(t *testing.T, s *media.Subscription) media.Delivery {
	t.Helper()
	// ctx ограничивает получение пакета двумя секундами. cancel освобождает срок.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// d содержит следующую доставку подписки. err сообщает об отказе её получения.
	d, err := s.Next(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return d
}

// Ждёт исход соединения через Results не более двух секунд и возвращает его ошибку.
func serverResult(t *testing.T, s *Server) error {
	t.Helper()
	select {
	// r содержит исход завершённого соединения из канала Results.
	case r := <-s.Results():
		return r.Err
	case <-time.After(2 * time.Second):
		t.Fatal("нет результата соединения")
		return nil
	}
}

// Проверяет авторизацию OBS, исходные заголовки и время через RTMP/fMP4, сохранение источника при
// неверном ключе, приём во время паузы и отзыв по StopSession.
func TestLocalEnhancedRTMPUsesCoreAuthorizationAndExactCodecHeaders(t *testing.T) {
	// f содержит изолированные доменные данные, активную Session и маршрутизатор входящего потока.
	f := newIngestFixture(t)
	// s содержит локальный входящий сервер. address содержит его TCP-адрес для тестового клиента.
	s, address := runServer(t, f, nil)
	// sub содержит подписку на пакеты тестового маршрутизатора.
	sub := newSubscription(t, f.router)
	// c содержит авторизованного клиента публикации. err сообщает об отказе его подключения.
	c, err := connectClient(t, address, f.stream, f.stream.PublicationKey)
	if err != nil {
		t.Fatal(err)
	}
	sendReady(t, c, 100)
	// v содержит первую видеодоставку. a содержит согласованную аудиодоставку.
	v, a := delivery(t, sub), delivery(t, sub)
	if v.Packet.Config().Info().Track != media.TrackVideo || a.Packet.Config().Info().Track != media.TrackAudio || !v.Packet.Info().Keyframe || v.Packet.Info().PTS != 4800 || v.Packet.Info().DTS != 4800 || v.Packet.Info().DurationTicks != 1600 || a.Packet.Info().DurationTicks != 1024 || !v.IsCurrent() {
		t.Fatal("потеряны дорожки, независимость или метка TU")
	}
	if !bytes.Equal(v.VideoConfig.Headers(), testAV1Config()) || !bytes.Equal(v.AudioConfig.Headers(), []byte{0x11, 0x90}) {
		t.Fatal("потеряны исходные codec headers")
	}
	// Пакеты действующего входа принимаются fMP4 без подмены шкалы или длительностей.
	// encoded накапливает fMP4 с фактически принятыми RTMP-пакетами.
	var encoded bytes.Buffer
	// fl содержит конечные ограничения контейнерного перехода.
	fl := fmp4.Limits{MaxBoxBytes: 1 << 20, MaxInitBytes: 1 << 20, MaxMoofBytes: 1 << 16, MaxMdatBytes: 1 << 20, MaxSampleBytes: 1 << 18, MaxExtradataBytes: 1 << 16, MaxSamples: 20}
	// mux записывает принятые A/V-пакеты с исходными конфигурациями в fMP4.
	mux, err := fmp4.NewMuxer(&encoded, fmp4.Init{Video: v.VideoConfig, Audio: v.AudioConfig}, fl)
	if err != nil {
		t.Fatal(err)
	}
	// p обозначает видеопакет либо аудиопакет, передаваемый в fMP4.
	for _, p := range []media.Packet{v.Packet, a.Packet} {
		if err = mux.WritePacket(p); err != nil {
			t.Fatal(err)
		}
	}
	// demux читает записанный fMP4 для сравнения с исходными доставками.
	demux, err := fmp4.NewDemuxer(bytes.NewReader(encoded.Bytes()), fmp4.DemuxOptions{Limits: fl})
	if err != nil {
		t.Fatal(err)
	}
	// want содержит очередной исходный пакет для сравнения после контейнерного перехода.
	for _, want := range []media.Packet{v.Packet, a.Packet} {
		// got содержит пакет после fMP4. e сообщает об отказе чтения.
		got, e := demux.Next()
		if e != nil || got.Info() != want.Info() || got.Config().Info() != want.Config().Info() || !bytes.Equal(got.Config().Headers(), want.Config().Headers()) {
			t.Fatalf("fMP4 изменил ingress: %+v, %v", got.Info(), e)
		}
	}
	if _, err = connectClient(t, address, f.stream, "wrong-synthetic-key"); err == nil {
		t.Fatal("неверный ключ принят")
	}
	if err = serverResult(t, s); err == nil {
		t.Fatal("нет отказа core")
	}
	if !v.IsCurrent() {
		t.Fatal("неавторизованное соединение заменило источник")
	}
	if _, err = f.svc.PauseSession(context.Background(), core.SessionCommand{RequestID: "pause-ingest", SessionID: f.session.SessionID}); err != nil {
		t.Fatal(err)
	}
	sendVideo(t, c, 133, []byte{0x1a, 2, 0x30, 0})
	sendAudio(t, c, 133, []byte{4, 5, 6})
	// got содержит видеодоставку во время ручной паузы для проверки сохранности входа.
	if got := delivery(t, sub); got.Packet.Info().DTS != 6384 || got.Packet.Info().Keyframe {
		t.Fatal("пауза повредила OBS либо inter выдан за key")
	}
	_ = delivery(t, sub)
	if _, err = f.svc.StopSession(context.Background(), core.SessionCommand{RequestID: "stop-ingest", SessionID: f.session.SessionID}); err != nil {
		t.Fatal(err)
	}
	if v.IsCurrent() {
		t.Fatal("StopSession не отозвал feed до возврата")
	}
	if _, err = c.Read(); err == nil {
		t.Fatal("отозванное соединение осталось открытым")
	}
	_ = serverResult(t, s)
}

// Проверяет замену Source и закрытие прежнего соединения при Override, безопасность позднего
// отключения и отзыв последнего Source после смены заголовков.
func TestAuthorizedOverrideInvalidatesOldCallbacksAndConnection(t *testing.T) {
	// f содержит изолированные доменные данные, активную Session и маршрутизатор входящего потока.
	f := newIngestFixture(t)
	// s содержит локальный входящий сервер. address содержит его TCP-адрес для тестового клиента.
	s, address := runServer(t, f, nil)
	// sub содержит подписку на пакеты тестового маршрутизатора.
	sub := newSubscription(t, f.router)
	// old содержит первоначального авторизованного клиента. err сообщает об отказе подключения.
	old, err := connectClient(t, address, f.stream, f.stream.PublicationKey)
	if err != nil {
		t.Fatal(err)
	}
	sendReady(t, old, 0)
	// before содержит доставку первого допуска до Override.
	before := delivery(t, sub)
	_ = delivery(t, sub)
	// fresh содержит нового клиента, заменяющего прежнюю публикацию.
	fresh, err := connectClient(t, address, f.stream, f.stream.PublicationKey)
	if err != nil {
		t.Fatal(err)
	}
	sendReady(t, fresh, 0)
	// after содержит доставку нового допуска после Override.
	after := delivery(t, sub)
	_ = delivery(t, sub)
	if before.IsCurrent() || !after.IsCurrent() || before.VideoConfig.Info().SourceEpoch == after.VideoConfig.Info().SourceEpoch {
		t.Fatal("Override не заменил identity")
	}
	if _, err = old.Read(); err == nil {
		t.Fatal("старое соединение не закрыто")
	}
	_ = serverResult(t, s)
	if !after.IsCurrent() {
		t.Fatal("поздний disconnect прежней lease очистил новый Source")
	}
	// Смена заголовков разрывает непрерывность декодирования при прежнем допуске публикации.
	sendReady(t, fresh, 0)
	// next содержит доставку нового SourceEpoch после повторных заголовков того же клиента.
	next := delivery(t, sub)
	_ = delivery(t, sub)
	if after.IsCurrent() || !next.IsCurrent() || after.VideoConfig.Info().SourceEpoch == next.VideoConfig.Info().SourceEpoch {
		t.Fatal("смена headers сохранила несовместимую очередь")
	}
	if _, err = f.svc.StopSession(context.Background(), core.SessionCommand{RequestID: "stop-rotated", SessionID: f.session.SessionID}); err != nil {
		t.Fatal(err)
	}
	if next.IsCurrent() {
		t.Fatal("revoke не отозвал последний Source lease")
	}
}

// failPublishConn подставляет отказ записи подтверждения Publish.Start для проверки границы принятия
// публикации.
type failPublishConn struct {
	// Conn содержит исходное соединение, которому передаются остальные записи.
	net.Conn
	// fail атомарно включает отказ только для сообщения Publish.Start.
	fail atomic.Bool
}

// Возвращает io.ErrClosedPipe на записи Publish.Start при включённом флаге отказа; остальные данные
// передаёт вложенному соединению.
func (c *failPublishConn) Write(b []byte) (int, error) {
	if c.fail.Load() && bytes.Contains(b, []byte("NetStream.Publish.Start")) {
		return 0, io.ErrClosedPipe
	}
	return c.Conn.Write(b)
}

// Проверяет, что ошибка записи Publish.Start новому клиенту сохраняет прежний действующий источник.
func TestFailedNetworkAcceptDoesNotReplaceSource(t *testing.T) {
	// f содержит изолированные доменные данные, активную Session и маршрутизатор входящего потока.
	f := newIngestFixture(t)
	// address содержит TCP-адрес сервера первоначальной действующей публикации.
	_, address := runServer(t, f, nil)
	// sub содержит подписку на пакеты тестового маршрутизатора.
	sub := newSubscription(t, f.router)
	// old содержит клиента прежней рабочей публикации. err сообщает об отказе его подключения.
	old, err := connectClient(t, address, f.stream, f.stream.PublicationKey)
	if err != nil {
		t.Fatal(err)
	}
	sendReady(t, old, 0)
	// current содержит доставку рабочей публикации, которая должна пережить отказ нового клиента.
	current := delivery(t, sub)
	_ = delivery(t, sub)
	// l содержит отдельный слушатель соединения с управляемым отказом записи.
	l, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// Закрывает локальный слушатель после проверки отказа сетевого принятия публикации.
		_ = l.Close()
	}()
	// failing принимает обёртку нового сокета для включения отказа Publish.Start из FeedResolver.
	var failing *failPublishConn
	// s содержит отдельный сервер, у которого подтверждение новой публикации будет отклонено
	// транспортом.
	s, err := NewServer(Options{Limits: testLimits(), Core: f.svc, Route: ConventionalRoute, Feed: func(core.Session) (*media.Router, error) {
		// Включает отказ записи NetStream.Publish.Start перед возвратом текущего маршрутизатора.
		failing.fail.Store(true)
		return f.router, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	// done передаёт результат Handle соединения с управляемым отказом.
	done := make(chan error, 1)
	go func() {
		// Принимает сокет, оборачивает его соединением с отказом NetStream.Publish.Start и передаёт результат
		// Handle через done.

		// conn содержит принятый сокет нового клиента. err сообщает об отказе Accept.
		conn, err := l.Accept()
		if err != nil {
			done <- err
			return
		}
		failing = &failPublishConn{Conn: conn}
		done <- s.Handle(context.Background(), failing)
	}()
	if _, err = connectClient(t, l.Addr().String(), f.stream, f.stream.PublicationKey); err == nil {
		t.Fatal("неуспешный Accept завершился успехом")
	}
	select {
	case err = <-done:
		if !errors.Is(err, io.ErrClosedPipe) {
			t.Fatal(err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Handle не завершился")
	}
	if !current.IsCurrent() {
		t.Fatal("неуспешный Accept отозвал прежнюю публикацию")
	}
}

// Проверяет, что задержанное подключение, авторизованное для прежней сессии, получает ErrStale и не
// заменяет источник новой сессии.
func TestAuthorizationCannotAttachNextSessionAcrossAcceptWindow(t *testing.T) {
	// f содержит изолированные доменные данные, активную Session и маршрутизатор входящего потока.
	f := newIngestFixture(t)
	// entered сообщает о первом разрешении Session. release разрешает завершить задержанное
	// разрешение после смены Session.
	entered, release := make(chan struct{}), make(chan struct{})
	// called атомарно считает вызовы FeedResolver для задержки только первого подключения.
	var called atomic.Int32
	defer close(release)
	// s содержит локальный входящий сервер. address содержит его TCP-адрес для тестового клиента.
	s, address := runServer(t, f, func(session core.Session) (*media.Router, error) {
		// При первом разрешении Session сообщает о входе и ждёт release, создавая окно между авторизацией и
		// активацией публикации.

		if called.Add(1) == 1 {
			close(entered)
			<-release
		}
		return f.router, nil
	})
	// u содержит URL публикации прежнего подключения.
	u := &url.URL{Scheme: "rtmp", Host: address, Path: "/" + string(f.stream.ID), Fragment: f.stream.PublicationKey}
	// old содержит клиента, чьё подключение задержано между авторизацией и активацией.
	old := &gortmplib.Client{URL: u, Publish: true}
	// ctx ограничивает гонку сессий двумя секундами. cancel освобождает срок.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// initialized передаёт результат задержанного Initialize.
	initialized := make(chan error, 1)
	go func() {
		// Инициализирует прежнего клиента в отдельной горутине и передаёт результат для проверки устаревшего
		// допуска.
		initialized <- old.Initialize(ctx)
	}()
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal("нет первоначальной авторизации")
	}
	// stop содержит квитанцию остановки первой Session. err сообщает об отказе её создания.
	stop, err := f.svc.StopSession(ctx, core.SessionCommand{RequestID: "stop-first", SessionID: f.session.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	complete(t, f.svc, stop)
	// cleanups принимает неподтверждённые намерения очистки первой Session.
	var cleanups []core.CleanupIntent
	if err = f.store.View(ctx, func(r core.Repositories) error {
		// Читает незавершённые намерения очистки артефактов первой Session через View и сохраняет их для
		// подтверждения.

		// e принимает результат чтения намерений очистки внутри согласованного View.
		var e error
		cleanups, e = r.Artifacts.Cleanups(ctx, core.ListQuery{Limit: 100, SessionID: f.session.SessionID, PendingOnly: true})
		return e
	}); err != nil {
		t.Fatal(err)
	}
	// c обозначает очередное намерение очистки первой Session для подтверждения.
	for _, c := range cleanups {
		if err = f.svc.AcknowledgeCleanup(ctx, core.CleanupFenceFor(c)); err != nil {
			t.Fatal(err)
		}
	}
	// next содержит квитанцию запуска следующей Session того же Stream.
	next, err := f.svc.StartSession(ctx, core.StartSessionRequest{RequestID: "start-next", StreamID: f.stream.ID})
	if err != nil {
		t.Fatal(err)
	}
	complete(t, f.svc, next)
	// fresh содержит клиента новой действующей Session.
	fresh, err := connectClient(t, address, f.stream, f.stream.PublicationKey)
	if err != nil {
		t.Fatal(err)
	}
	// sub содержит подписку для проверки потока новой Session.
	sub := newSubscription(t, f.router)
	sendReady(t, fresh, 0)
	// d содержит доставку новой публикации, которую поздняя активация прежней Session не должна
	// отозвать.
	d := delivery(t, sub)
	_ = delivery(t, sub)
	release <- struct{}{}
	select {
	case err = <-initialized:
		if err == nil {
			old.Close()
		}
	case <-ctx.Done():
		t.Fatal("Accept первой Session не завершился")
	}
	if err = serverResult(t, s); !errors.Is(err, core.ErrStale) {
		t.Fatalf("ожидался stale fence первой Session: %v", err)
	}
	if !d.IsCurrent() {
		t.Fatal("авторизация первой Session заменила feed второй")
	}
}

// observedReadConn сообщает о начале первого чтения вложенного соединения для управления отменой
// согласования.
type observedReadConn struct {
	// Conn содержит исходный транспорт, который выполняет чтения.
	net.Conn
	// entered закрывается при первом вызове Read.
	entered chan struct{}
	// once атомарно защищает единственный сигнал начала чтения.
	once atomic.Bool
}

// Однократно сигнализирует начало первого чтения и затем читает вложенное соединение для управления
// сценарием отмены.
func (c *observedReadConn) Read(b []byte) (int, error) {
	if !c.once.Swap(true) {
		close(c.entered)
	}
	return c.Conn.Read(b)
}

// Проверяет общий предел соединений, завершение согласования по отмене, видимые пропуски заполненного
// Results и отказ после Shutdown.
func TestConnectionBudgetCancellationAndBoundedResultQueue(t *testing.T) {
	// f содержит изолированные доменные данные, активную Session и маршрутизатор входящего потока.
	f := newIngestFixture(t)
	// limits ограничивает сервер одним активным соединением и одним результатом.
	limits := testLimits()
	limits.MaxConnections = 1
	// s содержит сервер для проверки общего допуска и заполненного Results. err сообщает об отказе
	// создания.
	s, err := NewServer(Options{Limits: limits, Core: f.svc, Route: ConventionalRoute, Feed: func(core.Session) (*media.Router, error) {
		// Возвращает маршрутизатор тестового входа для проверки общего предела соединений.
		return f.router, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	// a содержит обслуживаемый сервером конец первой пары. b содержит противоположный конец,
	// удерживающий согласование.
	a, b := net.Pipe()
	defer func() {
		// Закрывает принимающий конец первой пары net.Pipe после проверки отмены.
		_ = b.Close()
	}()
	// entered сообщает о начале чтения первого согласования.
	entered := make(chan struct{})
	// ctx позволяет отменить первую обработку. cancel инициирует остановку её согласования.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// done передаёт результат первой обработки для проверки завершения после отмены.
	done := make(chan error, 1)
	go func() {
		// Обслуживает первое соединение с уведомлением о начале чтения и передаёт результат через done.
		done <- s.Handle(ctx, &observedReadConn{Conn: a, entered: entered})
	}()
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("нет начального чтения")
	}
	// a2 содержит второй обслуживаемый конец сверх лимита. b2 содержит противоположный конец этой
	// пары.
	a2, b2 := net.Pipe()
	defer func() {
		// Закрывает принимающий конец второй пары net.Pipe после отказа по бюджету соединений.
		_ = b2.Close()
	}()
	if err = s.Handle(context.Background(), a2); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
	cancel()
	select {
	// err содержит результат отменённого первого согласования.
	case err := <-done:
		if err == nil {
			t.Error("отменённый handshake завершился успехом")
		}
	case <-time.After(time.Second):
		t.Fatal("отмена не присоединила handshake")
	}
	for range 3 {
		// a3 содержит очередной обслуживаемый конец для повреждённого согласования. b3 содержит
		// конец, передающий неверную версию.
		a3, b3 := net.Pipe()
		// done3 передаёт исход очередного повреждённого согласования.
		done3 := make(chan error, 1)
		go func() {
			// Обслуживает очередное соединение с повреждённым согласованием и передаёт результат через done3.
			done3 <- s.Handle(context.Background(), a3)
		}()
		if _, err = b3.Write([]byte{0}); err != nil {
			t.Fatal(err)
		}
		_ = b3.Close()
		select {
		case err = <-done3:
			if !errors.Is(err, ErrFraming) {
				t.Fatal(err)
			}
		case <-time.After(time.Second):
			t.Fatal("ошибочный handshake не завершился")
		}
	}
	if s.DroppedResults() != 3 {
		t.Fatalf("не видны потери полных результатов: %d", s.DroppedResults())
	}
	// stopCtx ограничивает финальный Shutdown одной секундой. stopCancel освобождает срок.
	stopCtx, stopCancel := context.WithTimeout(context.Background(), time.Second)
	defer stopCancel()
	if err = s.Shutdown(stopCtx); err != nil {
		t.Fatal(err)
	}
	// a4 содержит конец, передаваемый уже остановленному серверу. b4 содержит противоположный конец
	// пары.
	a4, b4 := net.Pipe()
	defer func() {
		// Закрывает принимающий конец пары net.Pipe после проверки отказа остановленного сервера.
		_ = b4.Close()
	}()
	if err = s.Handle(context.Background(), a4); !errors.Is(err, core.ErrClosed) {
		t.Fatal(err)
	}
}

// Проверяет ограниченное завершение согласования с молчащим клиентом и последующий успешный Shutdown.
func TestHandshakeDeadlineTerminatesIdlePeer(t *testing.T) {
	// f содержит изолированные доменные данные, активную Session и маршрутизатор входящего потока.
	f := newIngestFixture(t)
	// limits задаёт короткий двадцатимиллисекундный срок согласования.
	limits := testLimits()
	limits.HandshakeTimeout = 20 * time.Millisecond
	// s содержит сервер для проверки молчащего клиента. err сообщает об отказе создания.
	s, err := NewServer(Options{Limits: limits, Core: f.svc, Route: ConventionalRoute, Feed: func(core.Session) (*media.Router, error) {
		// Возвращает маршрутизатор тестового входа для проверки срока согласования.
		return f.router, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	// a содержит обслуживаемый конец пары. b содержит молчащую противоположную сторону.
	a, b := net.Pipe()
	defer func() {
		// Закрывает принимающий конец net.Pipe после проверки неактивного клиента.
		_ = b.Close()
	}()
	if err = s.Handle(context.Background(), a); err == nil {
		t.Fatal("idle handshake не завершился")
	}
	// ctx ограничивает последующий Shutdown одной секундой. cancel освобождает срок.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err = s.Shutdown(ctx); err != nil {
		t.Fatal(err)
	}
}
