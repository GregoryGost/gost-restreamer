package rtmp

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math/big"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

// Сверяет PTS и DTS пакета с заданными миллисекундами как точные рациональные секунды независимо от
// TimeBase.
func acceptanceWireTime(p media.Packet, milliseconds int64) bool {
	// b содержит единицу времени сравниваемого пакета.
	b := p.Config().Info().TimeBase
	// unit содержит точную длительность одного отсчёта пакета в секундах.
	unit := new(big.Rat).SetFrac64(int64(b.Numerator), int64(b.Denominator))
	// want содержит ожидаемые миллисекунды в точных рациональных секундах.
	want := new(big.Rat).SetFrac64(milliseconds, 1000)
	return new(big.Rat).Mul(new(big.Rat).SetInt64(p.Info().PTS), unit).Cmp(want) == 0 && new(big.Rat).Mul(new(big.Rat).SetInt64(p.Info().DTS), unit).Cmp(want) == 0
}

// Проверяет удаление раннего аудио при смене заголовков и отзыв последнего Source остановкой через
// другой core.Service того же хранилища.
func TestAcceptanceIngestHeaderRotationDropsOldAudioAndRevokesLatestSource(t *testing.T) {
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
	// before содержит доставку до смены заголовков.
	before := delivery(t, sub)
	_ = delivery(t, sub)
	// Новый заголовок уже разорвал непрерывность, но ещё не доказал новый независимый кадр.
	writeWire(t, c, chunk0(6, 9, append([]byte{0x90, 'a', 'v', '0', '1'}, testAV1Config()...)))
	sendAudio(t, c, 200, []byte{0xde, 0xad})
	sendVideo(t, c, 300, testAV1TU())
	sendAudio(t, c, 300, []byte{0xbe, 0xef})
	// after содержит новый независимый видеокадр после смены заголовков.
	after := delivery(t, sub)
	// audio содержит согласованное с новым видеокадром аудио.
	audio := delivery(t, sub)
	if !acceptanceWireTime(after.Packet, 300) || !acceptanceWireTime(audio.Packet, 300) || audio.Packet.Config().Info().Track != media.TrackAudio {
		t.Fatalf("после смены header вышла старая очередь: video=%+v audio=%+v", after.Packet.Info(), audio.Packet.Info())
	}
	if before.IsCurrent() || !after.IsCurrent() || before.VideoConfig.Info().SourceEpoch == after.VideoConfig.Info().SourceEpoch {
		t.Fatal("смена init не исключила старую decoder identity")
	}
	// other содержит второй сервис того же хранилища для проверки общего отзыва допуска.
	other, err := core.NewService(f.store, f.store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	// err сообщает об отказе остановки Session через второй сервис.
	if _, err := other.StopSession(context.Background(), core.SessionCommand{RequestID: "stop-current-header", SessionID: f.session.SessionID}); err != nil {
		t.Fatal(err)
	}
	if after.IsCurrent() {
		t.Fatal("stop другого Service не отозвал последний Source после ротации")
	}
	// err содержит результат чтения клиента после отзыва входящей публикации.
	if _, err := c.Read(); err == nil {
		t.Fatal("остановленный источник сохранил соединение")
	}
	// err содержит фактическую причину завершения сервера после отзыва.
	if err := serverResult(t, s); err == nil {
		t.Fatal("отзыв не завершил ingest")
	}
}

// Проверяет точное сохранение конфигураций, нагрузки и исходных миллисекунд четырёх реально принятых
// RTMP-пакетов через fMP4.
func TestAcceptanceIngestPacketsEnterFMP4WithoutClockSubstitution(t *testing.T) {
	// f содержит изолированные доменные данные, активную Session и маршрутизатор входящего потока.
	f := newIngestFixture(t)
	// address содержит TCP-адрес локального сервера входа.
	_, address := runServer(t, f, nil)
	// sub содержит подписку на пакеты тестового маршрутизатора.
	sub := newSubscription(t, f.router)
	// c содержит авторизованного клиента публикации. err сообщает об отказе его подключения.
	c, err := connectClient(t, address, f.stream, f.stream.PublicationKey)
	if err != nil {
		t.Fatal(err)
	}
	sendReady(t, c, 123)
	// v содержит начальную видеодоставку. a содержит начальную аудиодоставку.
	v, a := delivery(t, sub), delivery(t, sub)
	sendVideo(t, c, 156, testAV1TU())
	sendAudio(t, c, 145, []byte{4, 5, 6})
	// packets содержит четыре реально принятых пакета в порядке выдачи маршрутизатором.
	packets := []media.Packet{v.Packet, a.Packet, delivery(t, sub).Packet, delivery(t, sub).Packet}
	// wire накапливает fMP4 для проверки сохранности меток, конфигураций и нагрузки.
	var wire bytes.Buffer
	// limits задаёт конечные бюджеты контейнерного перехода.
	limits := fmp4.Limits{MaxBoxBytes: 1 << 20, MaxInitBytes: 1 << 20, MaxMoofBytes: 4096, MaxMdatBytes: 65536, MaxSampleBytes: 32768, MaxExtradataBytes: 4096, MaxSamples: 32}
	// m записывает исходные пакеты в fMP4 с неизменными конфигурациями.
	m, err := fmp4.NewMuxer(&wire, fmp4.Init{Video: v.VideoConfig, Audio: v.AudioConfig}, limits)
	if err != nil {
		t.Fatal("фактический init ingest не принят fMP4:", err)
	}
	// p обозначает очередной фактически принятый пакет, передаваемый в fMP4.
	for _, p := range packets {
		// err сообщает об отказе записи исходного пакета в контейнер.
		if err := m.WritePacket(p); err != nil {
			t.Fatal("фактический пакет ingest не принят fMP4:", err)
		}
	}
	// d читает построенный fMP4 для точного сравнения.
	d, err := fmp4.NewDemuxer(bytes.NewReader(wire.Bytes()), fmp4.DemuxOptions{Limits: limits})
	if err != nil {
		t.Fatal(err)
	}
	// i задаёт позицию пакета в исходной последовательности. want содержит ожидаемый пакет этой
	// позиции.
	for i, want := range packets {
		// got содержит прочитанный из fMP4 пакет. err сообщает об отказе чтения.
		got, err := d.Next()
		if err != nil {
			t.Fatal(err)
		}
		if got.Info() != want.Info() || got.Config().Info() != want.Config().Info() || !bytes.Equal(got.Config().Headers(), want.Config().Headers()) || !bytes.Equal(got.Payload(), want.Payload()) {
			t.Fatalf("пакет %d изменился между фактическим ingest и fMP4: %+v != %+v", i, got.Info(), want.Info())
		}
		// wantMilliseconds задаёт независимую ожидаемую RTMP-метку текущего пакета.
		wantMilliseconds := []int64{123, 123, 156, 145}[i]
		if !acceptanceWireTime(got, wantMilliseconds) {
			t.Fatal("исходные wire-миллисекунды изменены")
		}
	}
	// err содержит результат чтения после четырёх ожидаемых пакетов для проверки EOF.
	if _, err := d.Next(); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
}

// Проверяет доставку крупного TU исправному потребителю при частном ограничении соседа и явное
// завершение синхронизации медленной подписки.
func TestAcceptanceIngestOversizeConsumerDoesNotSetInputLimit(t *testing.T) {
	// f содержит изолированные доменные данные, активную Session и маршрутизатор входящего потока.
	f := newIngestFixture(t)
	// address содержит TCP-адрес локального сервера входа.
	_, address := runServer(t, f, nil)
	// fast содержит подписку, непрерывно получающую пакеты с достаточным бюджетом.
	fast := newSubscription(t, f.router)
	// slow содержит подписку с малым частным бюджетом пакета. err сообщает об отказе её создания.
	slow, err := f.router.Subscribe(media.QueueLimits{MaxPackets: 2, MaxBytes: 128, MaxPacketBytes: 32, MaxConfigBytes: 32, ResyncTimeout: time.Second})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает медленную подписку после проверки изоляции её ограничения размера пакета.
		_ = slow.Close()
	})
	// c содержит авторизованного клиента входящей публикации.
	c, err := connectClient(t, address, f.stream, f.stream.PublicationKey)
	if err != nil {
		t.Fatal(err)
	}
	sendReady(t, c, 0)
	_ = delivery(t, fast)
	_ = delivery(t, fast)
	// Валидный для адаптера TU превышает частный бюджет медленной подписки.
	// tu содержит допустимый AV1 TU с дополнительным OBU заполнения сверх частного бюджета соседа.
	tu := append([]byte(nil), testAV1TU()...)
	tu = append(tu, 0x7a, 64)
	tu = append(tu, make([]byte, 64)...)
	sendVideo(t, c, 33, tu)
	// got содержит доставку исправного потребителя после слишком большого для соседа TU.
	if got := delivery(t, fast); !acceptanceWireTime(got.Packet, 33) || len(got.Packet.Payload()) != len(tu) {
		t.Fatal("частный MaxPacketBytes стал глобальным")
	}
	// ctx ограничивает ожидание завершения медленной подписки двумя секундами. cancel освобождает
	// срок.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// err содержит причину завершения синхронизации медленной подписки.
	if _, err := slow.Next(ctx); !errors.Is(err, media.ErrResyncTimeout) {
		t.Fatal("медленная очередь не завершила resync явной причиной:", err)
	}
}
