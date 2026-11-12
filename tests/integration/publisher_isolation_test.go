package integration

import (
	"context"
	"net"
	"sync"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/GregoryGost/gost-restreamer/internal/media/publisher"
)

// Блокирует чтение одного RTMP-приёмника до переполнения его очереди, подтверждая доставку каждого AU
// здоровому соседу. После снятия блокировки проверяет повтор заголовков и восстановление медиа без
// переподключения обоих выходов, затем физическое завершение отправителей Publisher.
func TestPublisherSlowQueueOverflowKeepsHealthyOutputAndResyncs(t *testing.T) {
	// hold удерживает чтение медленного приёмника до переполнения его отдельной очереди.
	hold := make(chan struct{})
	// release обеспечивает однократное освобождение hold при явном восстановлении и очистке.
	var release sync.Once
	// unlock разрешает медленному назначению продолжить чтение безопасно для повторного вызова.
	unlock := func() {
		// Однократно разрешает медленному приёмнику продолжить чтение.

		release.Do(func() {
			// Закрывает канал, удерживающий чтение медленного назначения.
			close(hold)
		})
	}
	defer unlock()
	// slow содержит приёмник с задержанным чтением медиаданных.
	slow, _ := newPublisherReceiver(t, false, hold)
	// healthy содержит независимый приёмник, подтверждающий доставку каждого публикуемого AU.
	healthy, _ := newPublisherReceiver(t, false)
	// endpoints задаёт отдельные RTMP-адреса медленного и исправного выходов.
	endpoints := []string{"rtmp://" + slow.listener.Addr().String() + "/slow", "rtmp://" + healthy.listener.Addr().String() + "/healthy"}
	// f содержит общий медиатракт с двумя независимыми выходами одного AV1-профиля.
	f := newPublisherPipeline(t, []core.GroupProfile{publisherProfile(core.CodecAV1, 2560, 1440)}, endpoints, []string{"slow-key", "healthy-key"}, []int{0, 0})
	// slowPublisher сохраняет отправителя ограниченной медленной очереди; healthyPublisher сохраняет
	// отправителя исправного соседа.
	var slowPublisher, healthyPublisher *publisher.Publisher
	// o содержит очередной сохранённый выход при настройке независимых отправителей.
	for _, o := range f.outputs {
		// configure сохраняет обычные настройки здорового выхода либо задаёт малую очередь и
		// TCP-буфер медленного.
		configure := func(options *publisher.Options) {
			// Сохраняет штатные параметры исправного выхода без дополнительных ограничений.
		}
		if o.Snapshot.Destination.Endpoint == endpoints[0] {
			configure = func(options *publisher.Options) {
				// Уменьшает очередь и буфер сокета медленного выхода для воспроизводимого переполнения.

				options.Queue.MaxPackets = 8
				options.Queue.MaxBytes = 1 << 20
				options.Transport.Limits.WriteTimeout = 5 * time.Second
				options.Transport.DialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
					// Создаёт TCP-соединение с малым буфером записи, закрывая его при ошибке настройки.

					// connection содержит новое TCP-соединение; e содержит отказ
					// подключения или настройки его буфера.
					connection, e := (&net.Dialer{}).DialContext(ctx, network, address)
					if e == nil {
						// tcp содержит TCP-соединение для уменьшения буфера
						// записи; ok подтверждает подходящий транспортный тип.
						if tcp, ok := connection.(*net.TCPConn); ok {
							if e = tcp.SetWriteBuffer(1024); e != nil {
								_ = tcp.Close()
								return nil, e
							}
						}
					}
					return connection, e
				}
			}
		}
		// p содержит активированного отправителя текущего выхода для последующего разделения
		// slow и healthy.
		p := publisherActivate(t, f, o, f.live, nil, configure)
		if o.Snapshot.Destination.Endpoint == endpoints[0] {
			slowPublisher = p
		} else {
			healthyPublisher = p
		}
	}
	publisherWait(t, func() bool {
		// Ожидает установление обоих независимых соединений перед передачей больших сообщений.

		return slowPublisher.Snapshot().Connections == 1 && healthyPublisher.Snapshot().Connections == 1
	})
	// source содержит текущую публикацию общего LIVE Router; err содержит отказ её открытия.
	source, err := f.live.BeginSource()
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// Отключает тестовый источник от маршрутизатора после проверки восстановления очереди.
		_ = source.Disconnect()
	}()
	// v содержит AV1-конфигурацию текущей эпохи; a содержит согласованную AAC-конфигурацию.
	v, a := publisherConfigs(t, publisherProfile(core.CodecAV1, 2560, 1440), source.Epoch())
	if err = source.UpdateConfigs(v, a); err != nil {
		t.Fatal(err)
	}
	// Дополняющий OBU проверяет большие сообщения RTMP без заявления о декодируемости.
	// payload содержит большой структурно допустимый AV1 AU с дополнительным OBU для нагрузки RTMP и
	// очереди.
	payload := append(publisherPayload(v, 20), 0x7a, 0x80, 0x80, 0x10)
	payload = append(payload, make([]byte, 256<<10)...)
	// probe содержит пакет увеличенного AV1 AU для независимой структурной проверки.
	probe, err := media.NewPacket(media.PacketInfo{Keyframe: true, DurationTicks: 1}, v, payload)
	if err != nil {
		t.Fatal(err)
	}
	// inspector проверяет, что нагрузочный AU остаётся допустимым ключевым кадром.
	var inspector codec.Inspector
	// key подтверждает ключевой кадр большого AU; e содержит отказ его структурной проверки.
	if key, e := inspector.Inspect(probe); e != nil || !key {
		t.Fatalf("неверный независимый padding AU: %v", e)
	}
	// audio хранит следующую AAC-метку в шкале 1/48000.
	var audio int64
	// video хранит следующую видеометку источника для уникального подтверждения доставки.
	video := int64(0)
	// publish передаёт один видеоинтервал с аудио и ждёт его получения здоровым выходом перед
	// следующим.
	publish := func(large bool) {
		// Публикует один видеоинтервал с соответствующим аудио и ждёт его подтверждённой доставки исправному
		// выходу.

		t.Helper()
		// body выбирает обычный или увеличенный AV1 AU текущего видеоинтервала.
		body := publisherPayload(v, 20)
		if large {
			body = payload
		}
		// p содержит текущий медиапакет; e содержит отказ его создания или публикации.
		p, e := media.NewPacket(media.PacketInfo{PTS: video, DTS: video, DurationTicks: 1, Keyframe: true}, v, body)
		if e != nil {
			t.Fatal(e)
		}
		if e = source.Publish(p); e != nil {
			t.Fatal(e)
		}
		for audio*60 < (video+1)*48000 {
			p, e = media.NewPacket(media.PacketInfo{PTS: audio, DTS: audio, DurationTicks: 1024}, a, publisherPayload(a, 0))
			if e != nil {
				t.Fatal(e)
			}
			if e = source.Publish(p); e != nil {
				t.Fatal(e)
			}
			audio += 1024
		}
		// stamp содержит ожидаемую миллисекундную RTMP-метку текущего видео для проверки
		// фактической доставки.
		stamp := uint32(publisherNearestMS(video, v.Info().TimeBase))
		healthy.wait(t, func(messages []publisherWireMessage) bool {
			// Проверяет получение исправным приёмником текущего видеопакета с ожидаемой временной меткой.

			// m содержит очередное сообщение исправного приёмника при поиске подтверждения
			// текущей метки.
			for _, m := range messages {
				if m.typ == 9 && len(m.body) > 5 && m.body[0]&15 == 1 && m.timestamp == stamp {
					return true
				}
			}
			return false
		})
		video++
	}
	for range 40 {
		publish(true)
		if slowPublisher.Snapshot().QueueDrops.Overflow > 0 {
			break
		}
	}
	if slowPublisher.Snapshot().QueueDrops.Overflow == 0 {
		t.Fatal("реально заблокированный выход не достиг ограниченного overflow")
	}
	if healthyPublisher.Snapshot().QueueDrops.Overflow != 0 {
		t.Fatal("медленный socket переполнил здоровую очередь при подтверждённой доставке каждого AU")
	}
	// before сохраняет число доставленных здоровому выходу медиасообщений до освобождения медленного
	// соседа.
	before := publisherMediaCount(healthy.snapshot())
	// recoveryStamp задаёт первую ожидаемую RTMP-метку новых кадров после восстановления медленного
	// чтения.
	recoveryStamp := uint32(publisherNearestMS(video, v.Info().TimeBase))
	unlock()
	for range 8 {
		publish(false)
	}
	slow.wait(t, func(messages []publisherWireMessage) bool {
		// Ожидает повторные заголовки и новые медиапакеты после освобождения медленного приёмника.

		// headers считает повторные видеозаголовки; recovered отмечает видеопакет новой метки
		// после снятия блокировки.
		headers, recovered := 0, false
		// m содержит очередное сообщение медленного приёмника для проверки повторных заголовков
		// и восстановления медиа.
		for _, m := range messages {
			if m.typ == 9 && len(m.body) > 5 {
				if m.body[0]&15 == 0 {
					headers++
				}
				if m.body[0]&15 == 1 && m.timestamp >= recoveryStamp {
					recovered = true
				}
			}
		}
		return headers >= 2 && recovered
	})
	healthy.wait(t, func(messages []publisherWireMessage) bool {
		// Проверяет продолжение передачи исправного выхода после восстановления очереди соседа.
		return publisherMediaCount(messages) > before+4
	})
	if healthy.accepted.Load() != 1 || slow.accepted.Load() != 1 {
		t.Fatalf("queue recovery закрыла исправный socket: healthy=%d slow=%d", healthy.accepted.Load(), slow.accepted.Load())
	}
	if err = slowPublisher.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if err = healthyPublisher.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !slowPublisher.Result().NetworkJoined || !healthyPublisher.Result().NetworkJoined {
		t.Fatal("остановка не подтвердила физическое завершение")
	}
}
