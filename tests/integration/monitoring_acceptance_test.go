package integration

import (
	"context"
	"errors"
	"net"
	"net/url"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/rtmp"
	"github.com/bluenviron/gortmplib"
	"github.com/bluenviron/gortmplib/pkg/amf0"
	"github.com/bluenviron/gortmplib/pkg/message"
)

// Запускает настоящий сервер приёма RTMP на свободном локальном порту и связывает его с Feed менеджера
// фикстуры. t.Cleanup отменяет обслуживание и ограниченно ждёт Shutdown и Serve.
func acceptanceMonitoringIngest(t *testing.T, f *sessionAcceptanceFixture) net.Listener {
	t.Helper()
	// server принимает RTMP и связывает публикации с медиавходом менеджера сессий, а err сообщает об
	// ошибке его создания.
	server, err := rtmp.NewServer(rtmp.Options{Core: f.service, Limits: publisherRTMPLimits(), Route: rtmp.ConventionalRoute, Feed: func(s core.Session) (*media.Router, error) {
		// Получает входной маршрутизатор текущей Session через настоящий SessionManager.
		return f.manager.Feed(s)
	}})
	if err != nil {
		t.Fatal(err)
	}
	// listener предоставляет свободный локальный TCP-порт для тестовых RTMP-клиентов.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// ctx управляет сроком обслуживания RTMP-приёмника, а cancel запрашивает его остановку при
	// очистке теста.
	ctx, cancel := context.WithCancel(context.Background())
	// done передаёт результат Serve для отдельного ожидания завершения приёмника.
	done := make(chan error, 1)
	go func() {
		// Обслуживает локальный вход RTMP отдельно от проверок мониторинга.
		done <- server.Serve(ctx, listener)
	}()
	t.Cleanup(func() {
		// Отменяет приёмник и ограниченно ожидает завершения его Serve.

		cancel()
		// ctx ограничивает завершение RTMP-приёмника четырьмя секундами, а stop освобождает
		// таймер очистки.
		ctx, stop := context.WithTimeout(context.Background(), 4*time.Second)
		defer stop()
		// err сообщает об ошибке завершения активных обработчиков RTMP-сервера.
		if err := server.Shutdown(ctx); err != nil {
			t.Error(err)
		}
		select {
		// err содержит результат Serve после остановки для проверки допустимой причины
		// завершения.
		case err := <-done:
			if err != nil && !errors.Is(err, context.Canceled) && !errors.Is(err, net.ErrClosed) {
				t.Error(err)
			}
		case <-ctx.Done():
			t.Error("локальный вход RTMP не присоединён")
		}
	})
	return listener
}

// Открывает RTMP-публикацию с ключом Stream и отправляет только заголовок последовательности AV1,
// оставляя проверку готовности для следующих действий сценария. Закрытие клиента зарегистрировано в
// t.Cleanup.
func acceptanceMonitoringClient(t *testing.T, f *sessionAcceptanceFixture, listener net.Listener, video media.CodecConfig) *gortmplib.Client {
	t.Helper()
	// client публикует RTMP на локальный вход с идентификатором потока и его постоянным ключом.
	client := &gortmplib.Client{URL: &url.URL{Scheme: "rtmp", Host: listener.Addr().String(), Path: "/" + string(f.stream.ID), Fragment: f.stream.PublicationKey}, Publish: true}
	// ctx ограничивает установление RTMP-публикации четырьмя секундами, а cancel освобождает таймер
	// подключения.
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	// err сообщает об отказе установления RTMP-публикации.
	if err := client.Initialize(ctx); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(client.Close)
	// err сообщает об ошибке отправки размера RTMP-чанка клиентом.
	if err := client.Write(&message.SetChunkSize{Value: 128}); err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке передачи AV1-заголовка последовательности до готовности входа.
	if err := publisherWriteMessage(client.NetConn(), 9, 1, 0, append([]byte{0x90, 'a', 'v', '0', '1'}, video.Headers()...)); err != nil {
		t.Fatal(err)
	}
	return client
}

// Отправляет видеопакеты до аудиоконфигурации, затем ASC и аудиопакеты с заданными метками. Метаданные
// частоты кадров пока отсутствуют, поэтому эти байты измеряются на входе RTMP до допуска LIVE.
func acceptanceMonitoringBeforeReady(t *testing.T, client *gortmplib.Client, audioConfig media.CodecConfig, video, audio []byte, pairs int) {
	t.Helper()
	// i задаёт номер видеопакета, посылаемого до аудиоконфигурации, и его метку с шагом 20 мс.
	for i := range pairs {
		// err сообщает об отказе передачи видеопакета до готовности LIVE.
		if err := publisherWriteMessage(client.NetConn(), 9, 1, uint32(20*i), append([]byte{0x91, 'a', 'v', '0', '1'}, video...)); err != nil {
			t.Fatal(err)
		}
	}
	// err сообщает об отказе передачи AAC-конфигурации до готовности LIVE.
	if err := publisherWriteMessage(client.NetConn(), 8, 1, 0, append([]byte{0xaf, 0}, audioConfig.Headers()...)); err != nil {
		t.Fatal(err)
	}
	// i задаёт номер аудиопакета до готовности LIVE и его метку с шагом 20 мс.
	for i := range pairs {
		// err сообщает об отказе передачи аудиопакета до готовности LIVE.
		if err := publisherWriteMessage(client.NetConn(), 8, 1, uint32(20*i), append([]byte{0xaf, 1}, audio...)); err != nil {
			t.Fatal(err)
		}
	}
}

// Отправляет структурные AV1 и AAC AU с одной миллисекундной меткой; ошибка видеозаписи останавливает
// пару до отправки аудио.
func acceptanceMonitoringWritePair(client *gortmplib.Client, stamp uint32, video, audio []byte) error {
	// err сообщает об отказе передачи видео, после которого аудиочасть пары не отправляется.
	if err := publisherWriteMessage(client.NetConn(), 9, 1, stamp, append([]byte{0x91, 'a', 'v', '0', '1'}, video...)); err != nil {
		return err
	}
	return publisherWriteMessage(client.NetConn(), 8, 1, stamp, append([]byte{0xaf, 1}, audio...))
}

// Проверяет измерения входящего RTMP до готовности LIVE и после замены публикации без наследования
// счётчиков старого соединения. Счётчики группы учитывают только фактически потреблённый LIVE; разрыв
// входа и завершение Session удаляют текущие метрики.
func TestAcceptanceSessionMonitoringMeasuresIngressBeforeReadyAndAfterOverride(t *testing.T) {
	// receiver принимает публикацию выхода для измерения фактических отправленных байтов.
	receiver, _ := newPublisherReceiver(t, false)
	// f объединяет рабочую сессию, менеджер и хранилище для проверки входных и выходных измерений.
	f := newSessionAcceptanceFixture(t, sessionAcceptanceBuildHelper(t), []string{"rtmp://" + receiver.listener.Addr().String() + "/live"}, []int{0})
	f.launch(t)
	// current содержит запущенную сессию, мониторинг которой проверяется до и после замены входа.
	current := f.running(t, f.start(t, "start-monitoring", f.destinations[0].ID))
	// fence определяет точное выполнение сессии, которому должны принадлежать оперативные измерения.
	fence := core.RuntimeSessionFence{SessionID: current.ID, Incarnation: current.Incarnation, Lifecycle: current.Lifecycle}
	// listener предоставляет локальный адрес рабочего RTMP-входа менеджера.
	listener := acceptanceMonitoringIngest(t, f)
	// v содержит AV1-конфигурацию, а a — AAC-конфигурацию синтетической публикации.
	v, a := publisherConfigs(t, publisherProfile(core.CodecAV1, 640, 360), 1)
	// video содержит структурную AV1-единицу доступа, а audio — структурную AAC-единицу для
	// известных счётчиков нагрузки.
	video, audio := publisherPayload(v, 20), publisherPayload(a, 0)
	// wantPairBytes задаёт число байтов полезной нагрузки одной синтетической A/V-пары.
	wantPairBytes := uint64(len(video) + len(audio))
	// first представляет первоначальную публикацию, ещё не объявленную готовой для LIVE.
	first := acceptanceMonitoringClient(t, f, listener, v)
	acceptanceMonitoringBeforeReady(t, first, a, video, audio, 1)
	// before сохраняет оперативный снимок до объявления входа готовым для LIVE.
	var before core.SessionRuntimeSnapshot
	sessionAcceptanceWait(t, func() bool {
		// Ожидает измерения входа, группы и выхода до объявления входного потока готовым.

		before, _ = f.service.RuntimeSnapshot(context.Background(), fence)
		return before.Input != nil && before.Input.Metrics != nil && len(before.Groups) == 1 && before.Groups[0].Metrics != nil && len(before.Outputs) == 1 && before.Outputs[0].Metrics != nil
	})
	// input содержит наблюдение первого подключения со счётчиками до готовности LIVE.
	input := before.Input
	if !input.Connected || input.Metrics.VideoFrames != 1 || input.Metrics.PayloadBytes != wantPairBytes || input.Metrics.Format.VideoCodec != core.CodecAV1 || input.Metrics.Format.Width != 640 || input.Metrics.Format.AudioCodec != core.CodecAAC || input.Metrics.LastFrame.IsZero() {
		t.Fatal("измерения до готовности потеряны", input)
	}
	// g содержит наблюдение группы, которая до готовности входа должна потреблять заглушку.
	g := before.Groups[0]
	if g.Metrics.LiveInput.VideoFrames != 0 || g.Metrics.LiveInput.PayloadBytes != 0 || g.Metrics.FallbackInput.VideoFrames == 0 || g.Metrics.Output.VideoFrames == 0 || g.Value == nil || g.Value.Source != core.ObservationSourceFallback {
		t.Fatal("вход до готовности смешан с реально потреблённым LIVE", g)
	}
	// o содержит измерения выхода для проверки фактических байтов и ёмкости очереди.
	o := before.Outputs[0].Metrics
	if o.WrittenBytes <= o.PayloadBytes || o.PayloadBytes == 0 || o.VideoFrames == 0 || o.Connections != 1 || o.QueueCapacity <= 0 || o.LastWrite.IsZero() {
		t.Fatal("назначение не содержит фактическую передачу и ёмкость очереди", o)
	}
	// second заменяет первоначальную RTMP-публикацию с независимыми входными счётчиками.
	second := acceptanceMonitoringClient(t, f, listener, v)
	acceptanceMonitoringBeforeReady(t, second, a, video, audio, 2)
	first.Close()
	// replacement сохраняет снимок после замены публикации для проверки нового LeaseID и счётчиков.
	var replacement core.SessionRuntimeSnapshot
	sessionAcceptanceWait(t, func() bool {
		// Ожидает новое подключение с другим LeaseID и независимыми счётчиками публикации.

		replacement, _ = f.service.RuntimeSnapshot(context.Background(), fence)
		return replacement.Input != nil && replacement.Input.Connected && replacement.Input.LeaseID != input.LeaseID && replacement.Input.Metrics != nil && replacement.Input.Metrics.VideoFrames == 2
	})
	if replacement.Input.Metrics.PayloadBytes != 2*wantPairBytes || !replacement.Input.Metrics.LastFrame.After(input.Metrics.LastFrame) || replacement.Groups[0].Metrics.LiveInput.VideoFrames != 0 {
		t.Fatal("Override унаследовал счётчики или прежний отзыв повредил новое соединение", replacement)
	}
	// err сообщает об отказе отправки метаданных частоты кадров, необходимых для готовности нового
	// входа.
	if err := second.Write(&message.DataAMF0{ChunkStreamID: 5, MessageStreamID: 1, Payload: amf0.Data{"onMetaData", amf0.Object{{Key: "framerate", Value: float64(60)}}}}); err != nil {
		t.Fatal(err)
	}
	// producerCtx ограничивает производителя пакетов пятью секундами, а stopProducer запрашивает его
	// остановку и освобождает таймер.
	producerCtx, stopProducer := context.WithTimeout(context.Background(), 5*time.Second)
	defer stopProducer()
	// produced передаёт итог отправки 120 A/V-пар либо причину её прекращения.
	produced := make(chan error, 1)
	// producerDone закрывается при выходе производителя для ограниченного ожидания его горутины.
	producerDone := make(chan struct{})
	go func() {
		// Периодически отправляет известные пары видео и аудио, завершаясь по отмене или ошибке записи.

		defer close(producerDone)
		// ticker задаёт интервал 20 мс между синтетическими A/V-парами готового входа.
		ticker := time.NewTicker(20 * time.Millisecond)
		defer ticker.Stop()
		// i задаёт номер одной из 120 пар после объявления готовности и её возрастающую метку
		// времени.
		for i := range 120 {
			select {
			case <-producerCtx.Done():
				produced <- producerCtx.Err()
				return
			case <-ticker.C:
			}
			// err сообщает об отказе отправки очередной пары, прекращающем производителя.
			if err := acceptanceMonitoringWritePair(second, uint32(200+20*i), video, audio); err != nil {
				produced <- err
				return
			}
		}
		produced <- nil
	}()
	t.Cleanup(func() {
		// Отменяет производителя, закрывает клиента и ограниченно ожидает завершения его горутины.

		stopProducer()
		second.Close()
		select {
		case <-producerDone:
		case <-time.After(time.Second):
			t.Error("производитель тестовых AU не присоединён")
		}
	})
	// live сохраняет снимок после начала фактического потребления LIVE-пакетов группой.
	var live core.SessionRuntimeSnapshot
	sessionAcceptanceWait(t, func() bool {
		// Ожидает фактическое потребление LIVE-пакетов группой после готовности нового входа.

		live, _ = f.service.RuntimeSnapshot(context.Background(), fence)
		return len(live.Groups) == 1 && live.Groups[0].Value != nil && live.Groups[0].Value.Source == core.ObservationSourceLive && live.Groups[0].Metrics != nil && live.Groups[0].Metrics.LiveInput.VideoFrames > 0
	})
	select {
	// err содержит итог работы производителя для проверки успешной передачи всех пар.
	case err := <-produced:
		if err != nil {
			t.Fatal(err)
		}
	case <-producerCtx.Done():
		t.Fatal(producerCtx.Err())
	}
	// final сохраняет снимок с ожидаемыми 122 входными и 120 потреблёнными группой видеопакетами.
	var final core.SessionRuntimeSnapshot
	sessionAcceptanceWait(t, func() bool {
		// Ожидает точное число принятых входных и потреблённых группой видеопакетов.

		final, _ = f.service.RuntimeSnapshot(context.Background(), fence)
		return final.Input != nil && final.Input.Metrics != nil && final.Input.Metrics.VideoFrames == 122 && final.Groups[0].Metrics.LiveInput.VideoFrames == 120
	})
	if final.Input.Metrics.PayloadBytes != 122*wantPairBytes || final.Groups[0].Metrics.LiveInput.PayloadBytes != 120*wantPairBytes || final.Groups[0].Metrics.Output.VideoFrames < live.Groups[0].Metrics.Output.VideoFrames {
		t.Fatal("известные AU/байты смешаны между входом, группой и назначением", final)
	}
	second.Close()
	sessionAcceptanceWait(t, func() bool {
		// Ожидает отзыв входных метрик и переключение группы на заглушку после закрытия клиента.

		// snapshot содержит состояние после закрытия входа, а err сообщает об ошибке чтения при
		// ожидании отзыва метрик.
		snapshot, err := f.service.RuntimeSnapshot(context.Background(), fence)
		return err == nil && snapshot.Input != nil && !snapshot.Input.Connected && snapshot.Input.Metrics == nil && snapshot.Groups[0].Value.Source == core.ObservationSourceFallback
	})
	// stop содержит квитанцию остановки сессии, а err сообщает об отказе принятия команды.
	stop, err := f.manager.StopSession(context.Background(), core.SessionCommand{RequestID: "stop-monitoring", SessionID: current.ID})
	if err != nil {
		t.Fatal(err)
	}
	sessionAcceptanceReceipt(t, f.service, stop.ID, core.CommandSucceeded)
	sessionAcceptanceWait(t, func() bool {
		// Ожидает исчезновение оперативного снимка после успешной остановки Session.

		// err показывает недоступность оперативного снимка после успешной остановки сессии.
		_, err := f.service.RuntimeSnapshot(context.Background(), fence)
		return errors.Is(err, core.ErrNotReady)
	})
}
