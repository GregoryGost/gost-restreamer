package publisher

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/rtmp"
)

// testExecution моделирует допуск Publisher с управляемыми активацией, отзывом и отдельным разрешением
// освобождения.
type testExecution struct {
	// mu защищает тестовое состояние исполнения и счётчики его вызовов.
	mu sync.Mutex
	// closed закрывается при первом Seal тестового исполнения.
	closed chan struct{}
	// sealed запрещает новые операции; eligible разрешает ReleaseJoined; released отмечает
	// фактическое освобождение.
	sealed, eligible, released bool
	// sealBlocked задерживает Seal до отдельного разрешения теста, если канал задан.
	sealBlocked <-chan struct{}
	// activateHook задаёт проверяемое поведение активации, включая задержку и отказ.
	activateHook func(context.Context, *core.OutputObserver) error
	// activations считает вызовы активации; seals считает вызовы запрета новых операций.
	activations, seals int
}

// Создаёт управляемую резервацию с каналом отзыва для тестов жизненного цикла Publisher.
func newTestExecution() *testExecution { return &testExecution{closed: make(chan struct{})} }

// Считает попытки активации и выполняет управляемый обработчик допуска при его наличии.
func (e *testExecution) activate(ctx context.Context, observer *core.OutputObserver) error {
	e.mu.Lock()
	e.activations++
	// hook сохраняет текущий обработчик активации для вызова вне блокировки исполнения.
	hook := e.activateHook
	e.mu.Unlock()
	if hook != nil {
		return hook(ctx, observer)
	}
	return nil
}

// Разрешает тестовую операцию до Seal и возвращает ErrStale после запрета новых операций.
func (e *testExecution) admit(context.Context, rtmp.OutboundOperation) (func(), error) {
	e.mu.Lock()
	defer e.mu.Unlock()
	if e.sealed {
		return nil, core.ErrStale
	}
	return func() {
		// Завершает условную операцию без дополнительных действий: тестовый допуск проверяет запрет новых
		// операций через Seal.
	}, nil
}

// Ждёт управляемый барьер при наличии, считает вызовы и единожды закрывает канал отзыва.
func (e *testExecution) seal() {
	if e.sealBlocked != nil {
		<-e.sealBlocked
	}
	e.mu.Lock()
	defer e.mu.Unlock()
	e.seals++
	if !e.sealed {
		e.sealed = true
		close(e.closed)
	}
}

// Возвращает канал отзыва тестового исполнения.
func (e *testExecution) done() <-chan struct{} { return e.closed }

// Разрешает освобождение только после установки eligible и отмечает успешное освобождение.
func (e *testExecution) release() error {
	e.mu.Lock()
	defer e.mu.Unlock()
	if !e.eligible {
		return core.ErrNotReady
	}
	e.released = true
	return nil
}

// testObserver сохраняет последовательность оперативных состояний тестового Publisher.
type testObserver struct {
	// mu защищает список записанных наблюдений состояния.
	mu sync.Mutex
	// values хранит наблюдения в порядке вызовов Observe.
	values []core.OutputObservation
}

// testAttemptTimes собирает времена сетевых попыток для проверки задержек повторов.
type testAttemptTimes struct {
	// mu защищает накопление и копирование времён попыток.
	mu sync.Mutex
	// values хранит время начала каждой сетевой попытки в порядке вызовов.
	values []time.Time
}

// Сохраняет время очередной попытки под блокировкой и возвращает число попыток.
func (a *testAttemptTimes) append() int {
	a.mu.Lock()
	defer a.mu.Unlock()
	a.values = append(a.values, time.Now())
	return len(a.values)
}

// Возвращает независимую копию времён попыток под блокировкой.
func (a *testAttemptTimes) snapshot() []time.Time {
	a.mu.Lock()
	defer a.mu.Unlock()
	return append([]time.Time(nil), a.values...)
}

// Observe сохраняет последовательность наблюдений состояния под блокировкой.
func (o *testObserver) Observe(_ context.Context, v core.OutputObservation) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	o.values = append(o.values, v)
	return nil
}

// testConnection моделирует управляемую запись, закрытие и задерживаемое завершение транспорта.
type testConnection struct {
	// mu защищает ошибки, счётчики, пакеты и признак ожидания тестового соединения.
	mu sync.Mutex
	// done закрывается при первом Close условного транспорта.
	done chan struct{}
	// err содержит заданную причину завершения тестового соединения.
	err error
	// opts сохраняет настройки попытки с обработчиками допуска и учёта записей.
	opts rtmp.OutboundOptions
	// headers считает вызовы записи пары заголовков.
	headers int
	// packets хранит пакеты, чья условная запись успешно завершилась.
	packets []media.Packet
	// onHeaders задаёт дополнительный результат записи заголовков.
	onHeaders func() error
	// beforeDelivery позволяет изменить источник перед повторной проверкой актуальности выдачи.
	beforeDelivery func()
	// onPacket задаёт проверяемое поведение записи пакета, включая задержку и отказ.
	onPacket func(context.Context, media.Packet) error
	// join задерживает возврат Wait до отдельного разрешения теста.
	join <-chan struct{}
	// waited отмечает фактически завершившийся Wait транспорта.
	waited bool
}

// Создаёт управляемое соединение с отдельным каналом завершения.
func newTestConnection() *testConnection { return &testConnection{done: make(chan struct{})} }

// WriteHeaders получает допуск записи, считает заголовки и условные байты, затем вызывает управляемый
// обработчик.
func (c *testConnection) WriteHeaders(ctx context.Context, _, _ media.CodecConfig) error {
	// finish завершает допуск записи заголовков; err сообщает об отказе получения допуска.
	finish, err := c.opts.Admit(ctx, rtmp.OutboundWrite)
	if err != nil {
		return err
	}
	defer finish()
	c.mu.Lock()
	c.headers++
	// hook сохраняет обработчик заголовков для вызова вне блокировки соединения.
	hook := c.onHeaders
	c.mu.Unlock()
	c.opts.OnWrite(10)
	if hook != nil {
		return hook()
	}
	return nil
}

// WriteDelivery повторно проверяет актуальность delivery, получает допуск записи и сохраняет пакет
// после управляемого обработчика, учитывая условные байты транспорта.
func (c *testConnection) WriteDelivery(ctx context.Context, delivery media.Delivery) (rtmp.OutboundWriteResult, error) {
	if c.beforeDelivery != nil {
		c.beforeDelivery()
	}
	if !delivery.IsCurrent() {
		return rtmp.OutboundWriteResult{}, media.ErrStaleSource
	}
	// p содержит медиапакет текущей выдачи подписки для условной записи.
	p := delivery.Packet
	// finish завершает допуск записи пакета; err сообщает об отказе его получения.
	finish, err := c.opts.Admit(ctx, rtmp.OutboundWrite)
	if err != nil {
		return rtmp.OutboundWriteResult{}, err
	}
	defer finish()
	if c.onPacket != nil {
		// err содержит отказ управляемого обработчика до сохранения пакета как отправленного.
		if err := c.onPacket(ctx, p); err != nil {
			return rtmp.OutboundWriteResult{}, err
		}
	}
	c.mu.Lock()
	c.packets = append(c.packets, p)
	c.mu.Unlock()
	c.opts.OnWrite(len(p.Payload()) + 12)
	return rtmp.OutboundWriteResult{PayloadBytes: uint64(len(p.Payload()))}, nil
}

// Close единожды закрывает канал соединения под блокировкой.
func (c *testConnection) Close() error {
	c.mu.Lock()
	defer c.mu.Unlock()
	select {
	case <-c.done:
	default:
		close(c.done)
	}
	return nil
}

// Wait ожидает управляемый барьер завершения с отменой и отмечает подтверждённое ожидание транспорта.
func (c *testConnection) Wait(ctx context.Context) error {
	if c.join != nil {
		select {
		case <-c.join:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	c.mu.Lock()
	c.waited = true
	c.mu.Unlock()
	return nil
}

// Done возвращает канал закрытия управляемого соединения.
func (c *testConnection) Done() <-chan struct{} { return c.done }

// Err возвращает заданную ошибку соединения под блокировкой.
func (c *testConnection) Err() error { c.mu.Lock(); defer c.mu.Unlock(); return c.err }

// Snapshot возвращает пустой снимок транспорта для тестов накоплений Publisher.
func (c *testConnection) Snapshot() rtmp.OutboundSnapshot { return rtmp.OutboundSnapshot{} }

// Создаёт временный Router с источником и двумя конфигурациями, регистрируя закрытие после теста.
func testRouter(t *testing.T) (*media.Router, *media.Source, media.CodecConfig, media.CodecConfig) {
	t.Helper()
	// r содержит тестовый Router; err сохраняет ошибки создания Router, источника и двух
	// конфигураций.
	r, err := media.NewRouter(media.RouterLimits{MaxConsumers: 8, MaxRetainedBytes: 65536, MaxConfigBytes: 64})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает тестовый Router после сценария, игнорируя ошибку.
		_ = r.Close()
	})
	// s содержит активный источник тестового Router.
	s, err := r.BeginSource()
	if err != nil {
		t.Fatal(err)
	}
	// v содержит контрольную конфигурацию H.264 1080p60 в шкале 1/1000.
	v, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: s.Epoch(), Revision: 1, TimeBase: media.TimeBase{Numerator: 1, Denominator: 1000}, Video: core.VideoFormat{Codec: core.CodecH264, Width: 1920, Height: 1080, FrameRate: core.FrameRate{Numerator: 60, Denominator: 1}, CodecProfile: "high", PixelFormat: "yuv420p", BitDepth: 8, Color: core.ColorInfo{Range: "limited", Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"}}}, []byte{1})
	if err != nil {
		t.Fatal(err)
	}
	// a содержит контрольную конфигурацию стерео AAC 48000 Гц в шкале 1/1000.
	a, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackAudio, SourceEpoch: s.Epoch(), Revision: 1, TimeBase: media.TimeBase{Numerator: 1, Denominator: 1000}, Audio: core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", SampleRateHz: 48000, Channels: 2, ChannelLayout: "stereo"}}, []byte{2})
	if err != nil {
		t.Fatal(err)
	}
	// err сообщает об отказе установки контрольной пары конфигураций источника.
	if err := s.UpdateConfigs(v, a); err != nil {
		t.Fatal(err)
	}
	return r, s, v, a
}

// Создаёт Publisher с управляемыми исполнением, наблюдателем и подключением и регистрирует Stop после
// теста.
func testPublisher(t *testing.T, r *media.Router, dial func(context.Context, rtmp.OutboundTarget, rtmp.OutboundOptions) (connection, error)) (*Publisher, *testExecution) {
	t.Helper()
	// e содержит управляемое тестовое исполнение Publisher.
	e := newTestExecution()
	// p содержит Publisher с тестовым подключением, наблюдателем и конечными бюджетами очереди.
	p := &Publisher{router: r, execution: e, done: make(chan struct{}), sealDone: make(chan struct{}), state: core.OutputStopped, observer: &testObserver{}, dial: dial,
		options: Options{Queue: media.QueueLimits{MaxPackets: 16, MaxBytes: 4096, MaxPacketBytes: 128, MaxConfigBytes: 64, ResyncTimeout: time.Second}, RetryInitial: 5 * time.Millisecond, RetryMax: 20 * time.Millisecond, ResubscribeDelay: 10 * time.Millisecond}}
	t.Cleanup(func() {
		// Останавливает тестовый Publisher и сообщает ошибку ожидания через t.Error.

		// err сообщает об отказе резервной остановки тестового Publisher.
		if err := p.Stop(context.Background()); err != nil {
			t.Error(err)
		}
	})
	return p, e
}

// Запускает цикл Publisher с собственным отменяемым контекстом, обходя рабочий порт Activate для
// компонентных тестов.
func startTestPublisher(p *Publisher) {
	// ctx задаёт самостоятельный срок сетевого цикла; cancel сохраняется в Publisher для Stop.
	ctx, cancel := context.WithCancel(context.Background())
	p.cancel, p.active = cancel, true
	go p.run(ctx)
}

// Публикует в источник видеоключ и аудиопакет с заданной меткой для проверки готовности обеих дорожек.
func publishPair(t *testing.T, s *media.Source, v, a media.CodecConfig, tick int64) {
	t.Helper()
	// c содержит очередную конфигурацию видео либо аудио при публикации начальной пары.
	for _, c := range []media.CodecConfig{v, a} {
		// p содержит контрольный пакет дорожки; err сохраняет результаты его создания и
		// публикации.
		p, err := media.NewPacket(media.PacketInfo{PTS: tick, DTS: tick, Keyframe: c.Info().Track == media.TrackVideo}, c, []byte{3, 4, 5})
		if err != nil {
			t.Fatal(err)
		}
		if err = s.Publish(p); err != nil {
			t.Fatal(err)
		}
	}
}

// Проверяет неограниченные повторные попытки с задержками до верхнего предела, учёт байтов неудачного
// подключения и защиту удвоения от переполнения.
func TestRetryBackoffHasNoAttemptLimitAndSaturatesDelay(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Проверяет в управляемом времени семь сетевых попыток с задержками 5, 10 и далее 20 мс без
		// ограничения числа попыток и с точным учётом записей.

		// r содержит Router для сценария повторяемых сетевых отказов.
		r, _, _, _ := testRouter(t)
		// attempts сохраняет времена всех сетевых попыток в управляемом времени.
		var attempts testAttemptTimes
		// p выполняет повторяемые сетевые подключения с учётом семи записанных байтов на
		// попытку.
		p, _ := testPublisher(t, r, func(_ context.Context, _ rtmp.OutboundTarget, o rtmp.OutboundOptions) (connection, error) {
			// Отмечает время попытки, учитывает 7 принятых байтов и возвращает временную сетевую ошибку.

			attempts.append()
			o.OnWrite(7)
			return nil, errors.New("сеть недоступна")
		})
		startTestPublisher(p)
		synctest.Wait()
		// delay задаёт очередное продвижение управляемого времени до следующего повтора в
		// миллисекундах.
		for _, delay := range []time.Duration{5, 10, 20, 20, 20, 20} {
			time.Sleep(delay * time.Millisecond)
			synctest.Wait()
		}
		// times содержит фактические времена семи попыток для проверки последовательных
		// интервалов.
		times := attempts.snapshot()
		if len(times) != 7 {
			t.Fatalf("попыток: %d", len(times))
		}
		// i обозначает позицию проверяемого интервала; want задаёт его ожидаемую длину в
		// миллисекундах.
		for i, want := range []time.Duration{5, 10, 20, 20, 20, 20} {
			// got содержит фактический интервал между соседними сетевыми попытками.
			if got := times[i+1].Sub(times[i]); got != want*time.Millisecond {
				t.Fatalf("интервал %d: %v", i, got)
			}
		}
		// s содержит накопления попыток и байтов после семи сетевых отказов.
		s := p.Snapshot()
		if s.Attempts != 7 || s.Connections != 0 || s.WrittenBytes != 49 || s.State != core.OutputRetrying {
			t.Fatalf("снимок: %+v", s)
		}
		if nextDelay(time.Duration(math.MaxInt64-1), time.Duration(math.MaxInt64)) != time.Duration(math.MaxInt64) {
			t.Fatal("переполнение задержки")
		}
	})
}

// Проверяет Abort без открытия сети и отдельное разрешение освобождения уже завершённой резервации.
func TestAbortDoesNotDialAndJoinRequiresExplicitResolution(t *testing.T) {
	// r содержит Router для проверки Abort без открытия сети.
	r, _, _, _ := testRouter(t)
	// p содержит подготовленный Publisher; e позволяет отдельно разрешить освобождение его
	// исполнения.
	p, e := testPublisher(t, r, func(context.Context, rtmp.OutboundTarget, rtmp.OutboundOptions) (connection, error) {
		// Сообщает о запрещённой сети до Activate и возвращает ErrInvalid.

		t.Error("сеть до Activate")
		return nil, core.ErrInvalid
	})
	// err сообщает об отказе Abort подготовленного Publisher.
	if err := p.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	if !p.Result().NetworkJoined || p.Result().Err != nil {
		t.Fatalf("result: %+v", p.Result())
	}
	// err содержит ожидаемый ErrNotReady при освобождении без отдельного разрешения.
	if err := p.ReleaseJoined(); !errors.Is(err, core.ErrNotReady) {
		t.Fatal(err)
	}
	e.mu.Lock()
	e.eligible = true
	e.mu.Unlock()
	// err содержит результат освобождения после установки eligible.
	if err := p.ReleaseJoined(); err != nil {
		t.Fatal(err)
	}
	// err содержит результат повторного разрешённого освобождения для проверки идемпотентности.
	if err := p.ReleaseJoined(); err != nil {
		t.Fatal(err)
	}
}

// Проверяет, что срок Stop оставляет завершение неподтверждённым до позднего подключения и Wait
// транспорта, а позднее соединение закрывается и присоединяется.
func TestStopWaitsForLateDialAndLateTransportJoin(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Проверяет, что Stop отдельно ждёт поздний возврат подключения и задержанный Wait транспорта,
		// сохраняя ErrCleanupPending до обоих завершений.

		// r содержит Router для проверки позднего подключения и завершения транспорта.
		r, _, _, _ := testRouter(t)
		// late разрешает завершить задержанный dial после истечения Stop.
		late := make(chan struct{})
		// join разрешает завершить Wait уже закрытого позднего транспорта.
		join := make(chan struct{})
		// c содержит условное соединение с независимо задержанным Wait.
		c := newTestConnection()
		c.join = join
		// p выполняет dial, который возвращает соединение только после разрешения late.
		p, _ := testPublisher(t, r, func(_ context.Context, _ rtmp.OutboundTarget, o rtmp.OutboundOptions) (connection, error) {
			// Ждёт late, затем возвращает условное соединение с сохранёнными настройками даже после отмены
			// подключения.

			<-late
			c.opts = o
			return c, nil
		})
		startTestPublisher(p)
		synctest.Wait()
		// ctx ограничивает Stop одной миллисекундой управляемого времени; cancel освобождает
		// таймер.
		ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
		defer cancel()
		// err содержит ожидаемый ErrCleanupPending при незавершённом dial.
		if err := p.Stop(ctx); !errors.Is(err, ErrCleanupPending) {
			t.Fatal(err)
		}
		if p.Result().NetworkJoined {
			t.Fatal("late dial ещё работает")
		}
		// err содержит ожидаемый запрет освобождения исполнения до позднего подключения и Wait.
		if err := p.ReleaseJoined(); !errors.Is(err, ErrCleanupPending) {
			t.Fatal(err)
		}
		close(late)
		synctest.Wait()
		select {
		case <-c.Done():
		default:
			t.Fatal("late connection не закрыто")
		}
		if p.Result().NetworkJoined {
			t.Fatal("Wait транспорта ещё работает")
		}
		close(join)
		synctest.Wait()
		// result содержит окончательный итог после обоих разрешений; err сообщает об отказе
		// ожидания Publisher.
		result, err := p.Wait(context.Background())
		if err != nil || !result.NetworkJoined || result.Err != nil {
			t.Fatalf("%+v %v", result, err)
		}
	})
}

// Проверяет переход в PUBLISHING только после завершённой записи обеих дорожек и отсутствие повторного
// учёта отправленных единиц как потерь при Stop.
func TestPublishingRequiresActualVideoAndAudioAndNoPostSendDrop(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Проверяет, что PUBLISHING появляется лишь после полной начальной пары A/V, а успешно записанные AU
		// позднее не становятся потерями.

		// r содержит Router; s содержит его источник; v задаёт конфигурацию видео; a задаёт
		// конфигурацию аудио для проверки фактической готовности.
		r, s, v, a := testRouter(t)
		// audio разрешает завершение задержанной записи начального AAC.
		audio := make(chan struct{})
		// c содержит соединение с управляемой задержкой аудиопакета.
		c := newTestConnection()
		c.onPacket = func(ctx context.Context, p media.Packet) error {
			// Задерживает запись AAC до audio либо отмены, позволяя проверить состояние после отправки только
			// видео.

			if p.Config().Info().Track == media.TrackAudio {
				select {
				case <-audio:
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			return nil
		}
		// p передаёт контрольную пару через соединение с задержанным AAC.
		p, _ := testPublisher(t, r, func(_ context.Context, _ rtmp.OutboundTarget, o rtmp.OutboundOptions) (connection, error) {
			// Сохраняет настройки и возвращает соединение с задержанной записью AAC.

			c.opts = o
			return c, nil
		})
		startTestPublisher(p)
		synctest.Wait()
		publishPair(t, s, v, a, 0)
		synctest.Wait()
		// before содержит снимок после отправки видео и до завершения аудиозаписи.
		before := p.Snapshot()
		if before.AccessUnits != 1 || before.VideoFrames != 1 || before.State != core.OutputConnecting || before.InFlightPacketBytes == 0 {
			t.Fatalf("до AAC: %+v", before)
		}
		close(audio)
		synctest.Wait()
		// after содержит снимок после полной передачи обеих дорожек.
		after := p.Snapshot()
		if after.AccessUnits != 2 || after.PayloadBytes != 6 || after.State != core.OutputPublishing || after.LastMedia.IsZero() || after.LastWrite.IsZero() {
			t.Fatalf("после AAC: %+v", after)
		}
		// err сообщает об отказе остановки после успешной начальной пары.
		if err := p.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
		// got содержит потери выдач после Stop для проверки отсутствия потерь уже отправленных
		// AU.
		if got := p.Snapshot().DeliveryDrops; got != (DeliveryDrops{}) {
			t.Fatalf("успешная AU потеряна позднее: %+v", got)
		}
	})
}

// Проверяет повтор заголовков при смене источника и обновление истёкшей подписки без переподключения
// исправного транспорта.
func TestRecoveryRepeatsHeadersWithoutReplacingHealthyConnection(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Проверяет повтор заголовков при смене источника и обновление просроченной подписки с сохранением
		// исправного соединения.

		// r содержит Router; s содержит первый источник; v и a задают исходные видео- и
		// аудиоконфигурации для проверки смены поколения.
		r, s, v, a := testRouter(t)
		// c содержит единственное соединение, сохраняемое при смене источника и повторной
		// синхронизации.
		c := newTestConnection()
		// dials считает сетевые подключения для проверки отсутствия лишнего переподключения.
		dials := 0
		// p выполняет передачу последовательных поколений через одно исправное соединение.
		p, _ := testPublisher(t, r, func(_ context.Context, _ rtmp.OutboundTarget, o rtmp.OutboundOptions) (connection, error) {
			// Считает подключения, сохраняет настройки и возвращает одно условное соединение для проверки
			// восстановления.

			dials++
			c.opts = o
			return c, nil
		})
		startTestPublisher(p)
		synctest.Wait()
		publishPair(t, s, v, a, 0)
		synctest.Wait()
		// s2 содержит новый источник Router; err сохраняет результаты его создания и новых
		// конфигураций.
		s2, err := r.ReplaceSource()
		if err != nil {
			t.Fatal(err)
		}
		// vi содержит описание исходного видео; ai содержит описание исходного аудио для
		// привязки к новой эпохе.
		vi, ai := v.Info(), a.Info()
		vi.SourceEpoch, ai.SourceEpoch = s2.Epoch(), s2.Epoch()
		// v2 содержит видеоконфигурацию нового источника.
		v2, err := media.NewCodecConfig(vi, []byte{1})
		if err != nil {
			t.Fatal(err)
		}
		// a2 содержит аудиоконфигурацию нового источника.
		a2, err := media.NewCodecConfig(ai, []byte{2})
		if err != nil {
			t.Fatal(err)
		}
		// err сообщает об отказе установки конфигураций новой эпохи.
		if err := s2.UpdateConfigs(v2, a2); err != nil {
			t.Fatal(err)
		}
		publishPair(t, s2, v2, a2, 100)
		synctest.Wait()
		c.mu.Lock()
		// headers содержит число записанных пар заголовков после смены источника.
		headers := c.headers
		c.mu.Unlock()
		if dials != 1 || headers != 2 || p.Snapshot().AccessUnits != 4 {
			t.Fatalf("dials=%d headers=%d snapshot=%+v", dials, headers, p.Snapshot())
		}
		// Пустая подписка при восстановлении истекает, сохраняя соединение.
		// err сообщает об отказе отключения нового источника перед проверкой истечения
		// синхронизации.
		if err := s2.Disconnect(); err != nil {
			t.Fatal(err)
		}
		time.Sleep(time.Second)
		synctest.Wait()
		time.Sleep(10 * time.Millisecond)
		synctest.Wait()
		if dials != 1 || p.Result().NetworkJoined {
			t.Fatal("resync закрыл исправное соединение")
		}
	})
}

// Проверяет единственный перенос потерь закрытой подписки и насыщение счётчика байтов с сохраняемым
// признаком переполнения.
func TestQueueDropTransferAndCounterSaturation(t *testing.T) {
	// r содержит Router; s содержит источник; v и a задают контрольные конфигурации для заполнения
	// закрываемой подписки.
	r, s, v, a := testRouter(t)
	// p содержит Publisher без сетевого цикла для проверки переноса потерь и насыщения счётчиков.
	p, _ := testPublisher(t, r, nil)
	// sub содержит активную контрольную подписку; err сообщает об отказе её создания.
	sub, err := r.Subscribe(p.options.Queue)
	if err != nil {
		t.Fatal(err)
	}
	p.attachSubscription(sub)
	publishPair(t, s, v, a, 0)
	if p.Snapshot().QueuedPackets != 2 {
		t.Fatal("нет очереди")
	}
	p.closeSubscription()
	// first содержит снимок после первого закрытия подписки.
	first := p.Snapshot()
	p.closeSubscription()
	// second содержит снимок после повторного закрытия для проверки единственного переноса потерь.
	second := p.Snapshot()
	if first.QueueDrops.Closed != 2 || second.QueueDrops.Closed != 2 || second.PacketBytes != 0 {
		t.Fatalf("%+v %+v", first, second)
	}
	p.writeMu.Lock()
	p.writtenBytes = math.MaxUint64 - 1
	p.writeMu.Unlock()
	p.noteWrite(3)
	// got содержит снимок после переполняющего увеличения WrittenBytes.
	if got := p.Snapshot(); got.WrittenBytes != math.MaxUint64 || !got.Saturated {
		t.Fatalf("%+v", got)
	}
	// n содержит насыщаемый контрольный счётчик, изначально равный MaxUint64.
	var n uint64 = math.MaxUint64
	// sticky отмечает насыщение и должен сохраняться после последующего прибавления нуля.
	sticky := false
	saturate(&n, 1, &sticky)
	saturate(&n, 0, &sticky)
	if !sticky || n != math.MaxUint64 {
		t.Fatal("насыщение не сохранилось")
	}
}

// Проверяет закрытие и Wait транспорта при отзыве исполнения и сохранение ErrStale в окончательном
// результате.
func TestAuthorityRevokeJoinsAndPreservesTerminalReason(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Проверяет, что отзыв исполнения завершает сеть с ErrStale и дожидается транспортного Wait.

		// r содержит Router для проверки отзыва исполнения работающего Publisher.
		r, _, _, _ := testRouter(t)
		// c содержит соединение, чей Wait должен завершиться после отзыва.
		c := newTestConnection()
		// p содержит активный Publisher; e позволяет инициировать отзыв через Seal.
		p, e := testPublisher(t, r, func(_ context.Context, _ rtmp.OutboundTarget, o rtmp.OutboundOptions) (connection, error) {
			// Сохраняет настройки и возвращает условное соединение для проверки отзыва исполнения.

			c.opts = o
			return c, nil
		})
		startTestPublisher(p)
		synctest.Wait()
		e.seal()
		synctest.Wait()
		// result содержит итог отозванного Publisher; err сообщает об отказе ожидания
		// завершения.
		result, err := p.Wait(context.Background())
		if err != nil || !result.NetworkJoined || !errors.Is(result.Err, core.ErrStale) {
			t.Fatalf("%+v %v", result, err)
		}
		c.mu.Lock()
		// waited сохраняет признак возврата транспортного Wait для проверки фактического
		// присоединения.
		waited := c.waited
		c.mu.Unlock()
		if !waited {
			t.Fatal("transport join пропущен")
		}
	})
}

// Проверяет учёт частичной записи отдельно от полных медиасообщений и однократное разделение потерь
// текущей выдачи подписки и оставшейся очереди.
func TestPartialTransportFailureCountsBytesAndAbandonsOnlyOwnedAU(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Проверяет отдельный учёт частично записанных байтов, потерянной выданной AU и закрытого остатка
		// очереди без повторного учёта при Stop.

		// r содержит Router; source содержит источник; video и audio задают контрольную пару для
		// отказа частичной записи.
		r, source, video, audio := testRouter(t)
		// c содержит соединение, учитывающее два записанных байта перед отказом AU.
		c := newTestConnection()
		c.onPacket = func(context.Context, media.Packet) error {
			// Учитывает два фактически принятых байта и возвращает ошибку частичной записи AU.

			c.opts.OnWrite(2)
			return errors.New("частичная запись")
		}
		// p выполняет передачу через условный транспорт с частичной ошибкой записи.
		p, _ := testPublisher(t, r, func(_ context.Context, _ rtmp.OutboundTarget, o rtmp.OutboundOptions) (connection, error) {
			// Сохраняет настройки и возвращает соединение с контролируемым отказом записи.

			c.opts = o
			return c, nil
		})
		startTestPublisher(p)
		synctest.Wait()
		publishPair(t, source, video, audio, 0)
		synctest.Wait()
		// s содержит снимок частичных байтов, одной потерянной выдачи и закрытого остатка
		// очереди.
		s := p.Snapshot()
		if s.WrittenBytes != 12 || s.PayloadBytes != 0 || s.AccessUnits != 0 || s.DeliveryDrops.Transport != 1 || s.QueueDrops.Closed != 1 || s.State != core.OutputRetrying {
			t.Fatalf("снимок после частичной записи: %+v", s)
		}
		// err сообщает об отказе Stop после частичной записи.
		if err := p.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
		// got содержит потери после Stop для проверки отсутствия повторного учёта текущей
		// выдачи.
		if got := p.Snapshot().DeliveryDrops; got.Transport != 1 || got.Cancelled != 0 {
			t.Fatalf("повторный учёт: %+v", got)
		}
	})
}

// Проверяет сохранение задержки после одного согласования соединения и её сброс только после
// фактической передачи начальной пары видео и аудио.
func TestBackoffResetsOnlyAfterActualInitialAVWrites(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Проверяет, что подключение без начальной пары A/V не сбрасывает задержку повтора, а полная отправка
		// обеих дорожек сбрасывает её до 5 мс.

		// r содержит Router; source содержит источник; video и audio задают пару для проверки
		// сброса сетевой задержки.
		r, source, video, audio := testRouter(t)
		// first содержит соединение, разрываемое до медиа; second содержит соединение,
		// разрываемое после полной начальной пары.
		first, second := newTestConnection(), newTestConnection()
		// attempts собирает времена подключений до и после фактической передачи дорожек.
		var attempts testAttemptTimes
		// p выдаёт соединения на третьей и четвёртой попытках и проверяет сохранение и сброс
		// задержки.
		p, _ := testPublisher(t, r, func(_ context.Context, _ rtmp.OutboundTarget, o rtmp.OutboundOptions) (connection, error) {
			// Отмечает время попыток, выдаёт соединения только на третьей и четвёртой попытках, остальные
			// завершает временной сетевой ошибкой.

			switch attempts.append() {
			case 3:
				first.opts = o
				return first, nil
			case 4:
				second.opts = o
				return second, nil
			default:
				return nil, errors.New("сеть недоступна")
			}
		})
		startTestPublisher(p)
		synctest.Wait()
		time.Sleep(5 * time.Millisecond)
		synctest.Wait()
		time.Sleep(10 * time.Millisecond)
		synctest.Wait()
		first.mu.Lock()
		first.err = errors.New("соединение разорвано до медиаданных")
		first.mu.Unlock()
		_ = first.Close()
		synctest.Wait()
		time.Sleep(19 * time.Millisecond)
		synctest.Wait()
		if len(attempts.snapshot()) != 3 {
			t.Fatal("publish handshake сбросил backoff")
		}
		time.Sleep(time.Millisecond)
		synctest.Wait()
		publishPair(t, source, video, audio, 0)
		synctest.Wait()
		if p.Snapshot().AccessUnits != 2 {
			t.Fatal("начальная пара не записана")
		}
		second.mu.Lock()
		second.err = errors.New("соединение разорвано после медиаданных")
		second.mu.Unlock()
		_ = second.Close()
		synctest.Wait()
		time.Sleep(5 * time.Millisecond)
		synctest.Wait()
		// times содержит времена пяти попыток для проверки возврата задержки к пяти
		// миллисекундам после A/V.
		times := attempts.snapshot()
		if len(times) != 5 || times[4].Sub(times[3]) != 5*time.Millisecond {
			t.Fatalf("backoff не сброшен после A/V: %v", times)
		}
		// s содержит итог числа соединений и переподключений после двух успешных установлений.
		if s := p.Snapshot(); s.Connections != 2 || s.Reconnects != 1 {
			t.Fatalf("publish-соединения: %+v", s)
		}
	})
}

// Проверяет независимую передачу исправного выхода при заблокированном соседе и сохранение его
// соединения после остановки соседа.
func TestSlowAndFailedPublishersDoNotBlockHealthyOutput(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Проверяет независимость здорового выхода от заблокированной записи и последующей остановки
		// медленного Publisher.

		// r содержит общий Router; source содержит источник; video и audio задают пару для двух
		// независимых выходов.
		r, source, video, audio := testRouter(t)
		// slowConn содержит блокируемый транспорт; healthyConn содержит исправный транспорт
		// соседнего выхода.
		slowConn, healthyConn := newTestConnection(), newTestConnection()
		slowConn.onPacket = func(ctx context.Context, _ media.Packet) error {
			// Блокирует запись AU до отмены контекста и возвращает её причину.
			<-ctx.Done()
			return ctx.Err()
		}
		// slow содержит Publisher с записью, ожидающей отмены контекста.
		slow, _ := testPublisher(t, r, func(_ context.Context, _ rtmp.OutboundTarget, o rtmp.OutboundOptions) (connection, error) {
			// Сохраняет настройки и возвращает соединение медленного выхода.

			slowConn.opts = o
			return slowConn, nil
		})
		// healthy содержит независимый исправный Publisher того же Router.
		healthy, _ := testPublisher(t, r, func(_ context.Context, _ rtmp.OutboundTarget, o rtmp.OutboundOptions) (connection, error) {
			// Сохраняет настройки и возвращает соединение здорового соседнего выхода.

			healthyConn.opts = o
			return healthyConn, nil
		})
		startTestPublisher(slow)
		startTestPublisher(healthy)
		synctest.Wait()
		publishPair(t, source, video, audio, 0)
		synctest.Wait()
		// got содержит снимок здорового выхода до остановки медленного соседа.
		if got := healthy.Snapshot(); got.AccessUnits != 2 || got.State != core.OutputPublishing {
			t.Fatalf("здоровый выход заблокирован: %+v", got)
		}
		// err сообщает об отказе остановки медленного Publisher.
		if err := slow.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
		publishPair(t, source, video, audio, 1)
		synctest.Wait()
		// got содержит снимок здорового выхода после остановки соседа и передачи новой пары.
		if got := healthy.Snapshot(); got.AccessUnits != 4 || got.Connections != 1 {
			t.Fatalf("остановка соседа: %+v", got)
		}
		// got содержит итог потерь остановленного медленного Publisher.
		if got := slow.Snapshot(); got.DeliveryDrops.Cancelled != 1 || got.QueueDrops.Closed != 1 {
			t.Fatalf("потери остановленного выхода: %+v", got)
		}
	})
}

// Проверяет отбрасывание выдачи подписки, устаревшей перед записью, повтор заголовков и восстановление
// нового поколения в том же соединении.
func TestStaleDeliveryAfterWriterWaitRecoversOnSameSocket(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Проверяет восстановление устаревшей выдачи на том же соединении после смены конфигурации перед
		// записью.

		// r содержит Router; source содержит источник; video и audio задают исходную пару для
		// смены конфигурации перед записью.
		r, source, video, audio := testRouter(t)
		// c содержит соединение, изменяющее источник перед первой проверкой выдачи.
		c := newTestConnection()
		// once исключает повторное изменение видеоконфигурации в beforeDelivery.
		once := false
		c.beforeDelivery = func() {
			// При первой выдаче увеличивает Revision видео, обновляет конфигурации и публикует свежую пару, делая
			// прежнюю выдачу устаревшей.

			if once {
				return
			}
			once = true
			// vi содержит описание видео с увеличиваемой ревизией конфигурации.
			vi := video.Info()
			vi.Revision++
			// freshVideo содержит новую видеоконфигурацию; err сообщает об отказе её
			// создания.
			freshVideo, err := media.NewCodecConfig(vi, []byte{9})
			if err != nil {
				t.Error(err)
				return
			}
			// err сообщает об отказе установки свежей видеоконфигурации перед записью старой
			// выдачи.
			if err := source.UpdateConfigs(freshVideo, audio); err != nil {
				t.Error(err)
				return
			}
			publishPair(t, source, freshVideo, audio, 1)
		}
		// p передаёт свежую пару после отбрасывания устаревшей выдачи без замены соединения.
		p, _ := testPublisher(t, r, func(_ context.Context, _ rtmp.OutboundTarget, o rtmp.OutboundOptions) (connection, error) {
			// Сохраняет настройки и возвращает соединение, меняющее конфигурацию перед первой записью.

			c.opts = o
			return c, nil
		})
		startTestPublisher(p)
		synctest.Wait()
		publishPair(t, source, video, audio, 0)
		synctest.Wait()
		// s содержит снимок восстановленного поколения для проверки передач и раздельного учёта
		// потерь.
		s := p.Snapshot()
		if s.AccessUnits != 2 || s.Connections != 1 || s.DeliveryDrops.Stale != 1 || s.DeliveryDrops.Transport != 0 || s.QueueDrops.ConfigReset != 1 || s.State != core.OutputPublishing {
			t.Fatalf("recovery: %+v", s)
		}
		c.mu.Lock()
		// headers содержит число пар заголовков после смены ревизии перед записью.
		headers := c.headers
		c.mu.Unlock()
		if headers != 2 {
			t.Fatalf("headers: %d", headers)
		}
	})
}

// Проверяет окончательное завершение одного выхода при ошибке времени транспорта без остановки и
// переподключения здорового соседа.
func TestTerminalMediaFailureLeavesHealthyPublisherRunning(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Проверяет изоляцию окончательной ошибки времени одного выхода: здоровый сосед продолжает передачу
		// без нового соединения.

		// r содержит общий Router; source содержит источник; video и audio задают пару для
		// проверки изоляции окончательного отказа.
		r, source, video, audio := testRouter(t)
		// badConn содержит транспорт с OutboundTiming; healthyConn содержит исправный транспорт
		// соседнего выхода.
		badConn, healthyConn := newTestConnection(), newTestConnection()
		badConn.onPacket = func(context.Context, media.Packet) error {
			// Возвращает окончательную OutboundTiming при записи AU ошибочного выхода.
			return &rtmp.OutboundError{Kind: rtmp.OutboundTiming}
		}
		// bad содержит Publisher, окончательно завершающийся при ошибке времени медиаданных.
		bad, _ := testPublisher(t, r, func(_ context.Context, _ rtmp.OutboundTarget, o rtmp.OutboundOptions) (connection, error) {
			// Сохраняет настройки и возвращает соединение с окончательной ошибкой медиавремени.

			badConn.opts = o
			return badConn, nil
		})
		// healthy содержит исправный Publisher, продолжающий передачу после отказа соседа.
		healthy, _ := testPublisher(t, r, func(_ context.Context, _ rtmp.OutboundTarget, o rtmp.OutboundOptions) (connection, error) {
			// Сохраняет настройки и возвращает соединение здорового соседнего Publisher.

			healthyConn.opts = o
			return healthyConn, nil
		})
		startTestPublisher(bad)
		startTestPublisher(healthy)
		synctest.Wait()
		publishPair(t, source, video, audio, 0)
		synctest.Wait()
		// result содержит итог окончательно отказавшего Publisher; err сообщает об отказе
		// ожидания его завершения.
		result, err := bad.Wait(context.Background())
		if err != nil || !rtmp.IsOutboundTerminal(result.Err) || !result.NetworkJoined {
			t.Fatalf("terminal: %+v %v", result, err)
		}
		publishPair(t, source, video, audio, 1)
		synctest.Wait()
		// s содержит снимок здорового соседа после отказа другого выхода.
		if s := healthy.Snapshot(); s.AccessUnits != 4 || s.Connections != 1 || s.NetworkJoined {
			t.Fatalf("здоровый сосед: %+v", s)
		}
		// s содержит снимок отказавшего Publisher для проверки одной попытки и потерь.
		if s := bad.Snapshot(); s.DeliveryDrops.Terminal != 1 || s.QueueDrops.Closed != 1 || s.Attempts != 1 || s.State != core.OutputFailed {
			t.Fatalf("окончательный отказ: %+v", s)
		}
	})
}

// Проверяет конечное ожидание Stop при заблокированном Seal и немедленную отмену сети с поздним
// подтверждением завершения.
func TestStopDeadlineDoesNotWaitForBlockedAuthoritySeal(t *testing.T) {
	// active выбирает подготовленный либо уже работающий Publisher при заблокированном Seal.
	for _, active := range []bool{false, true} {
		t.Run(map[bool]string{false: "prepared", true: "active"}[active], func(t *testing.T) {
			// Проверяет ограниченное ожидание Stop при заблокированном Seal для подготовленного либо активного
			// Publisher.

			synctest.Test(t, func(t *testing.T) {
				// В управляемом времени задерживает Seal, проверяет отмену активной сети и допускает NetworkJoined
				// только после освобождения запрета операций.

				// r содержит Router для проверки срока Stop при задержанном запрете
				// операций.
				r, _, _, _ := testRouter(t)
				// c содержит условное соединение, отмена которого не должна ждать Seal.
				c := newTestConnection()
				// p содержит проверяемый Publisher; e предоставляет управляемый барьер
				// его Seal.
				p, e := testPublisher(t, r, func(_ context.Context, _ rtmp.OutboundTarget, o rtmp.OutboundOptions) (connection, error) {
					// Сохраняет настройки и возвращает соединение для проверки Stop при задержанном Seal.

					c.opts = o
					return c, nil
				})
				// blocked разрешает фактически завершить задержанный Seal.
				blocked := make(chan struct{})
				e.sealBlocked = blocked
				if active {
					startTestPublisher(p)
					synctest.Wait()
				}
				// ctx ограничивает Stop одной миллисекундой; cancel освобождает таймер
				// управляемого ожидания.
				ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
				defer cancel()
				// err содержит ожидаемый ErrCleanupPending при первом Stop с
				// заблокированным Seal.
				if err := p.Stop(ctx); !errors.Is(err, ErrCleanupPending) {
					t.Fatal(err)
				}
				if p.Result().NetworkJoined {
					t.Fatal("seal ещё работает")
				}
				// err содержит ожидаемый ErrCleanupPending при повторном Stop до
				// разрешения Seal.
				if err := p.Stop(ctx); !errors.Is(err, ErrCleanupPending) {
					t.Fatal(err)
				}
				if active {
					select {
					case <-c.Done():
					default:
						t.Fatal("authority gate задержал отмену сети")
					}
				}
				close(blocked)
				synctest.Wait()
				// result содержит итог после завершения Seal; err сообщает об отказе
				// ожидания.
				if result, err := p.Wait(context.Background()); err != nil || !result.NetworkJoined || result.Err != nil {
					t.Fatalf("%+v %v", result, err)
				}
			})
		})
	}
}

// Проверяет отмену незавершённой Activate и запрет NetworkJoined до возврата активации и Seal, включая
// поздний успех допуска после Stop.
func TestStopCancelsPendingActivationAndTracksItsCompletion(t *testing.T) {
	// late выбирает поздний успех активации вместо возврата ошибки отменённого допуска.
	for _, late := range []bool{false, true} {
		t.Run(map[bool]string{false: "cancelled_gate", true: "late_success"}[late], func(t *testing.T) {
			// Проверяет остановку незавершённой Activate как при ошибке отмены допуска, так и при позднем успешном
			// возврате.

			synctest.Test(t, func(t *testing.T) {
				// В управляемом времени независимо задерживает Activate и Seal, требуя ожидания обоих и отсутствия
				// сети после Stop.

				// r содержит Router для проверки незавершённой Activate без последующей
				// сети.
				r, _, _, _ := testRouter(t)
				// p содержит Publisher с управляемой активацией; e задерживает Activate
				// и Seal независимо.
				p, e := testPublisher(t, r, func(context.Context, rtmp.OutboundTarget, rtmp.OutboundOptions) (connection, error) {
					// Сообщает о запрещённой сети после остановки незавершённой активации и возвращает ErrInvalid.

					t.Error("сеть после остановки незавершённой активации")
					return nil, core.ErrInvalid
				})
				// entered сообщает о входе в активацию; cancelled сообщает о полученной
				// ею отмене.
				entered, cancelled := make(chan struct{}), make(chan struct{})
				// activationGate разрешает поздний возврат Activate; sealGate разрешает
				// завершить Seal.
				activationGate, sealGate := make(chan struct{}), make(chan struct{})
				// activationOnce исключает повторное закрытие activationGate; sealOnce
				// исключает повторное закрытие sealGate.
				var activationOnce, sealOnce sync.Once
				// releaseActivation единожды разрешает возврат задержанной активации.
				releaseActivation := func() {
					// Единожды разрешает задержанной Activate продолжить работу.

					activationOnce.Do(func() {
						// Закрывает activationGate при первом разрешении возврата Activate.
						close(activationGate)
					})
				}
				// releaseSeal единожды разрешает возврат задержанного запрета операций.
				releaseSeal := func() {
					// Единожды разрешает завершение задержанного Seal.

					sealOnce.Do(func() {
						// Закрывает sealGate при первом разрешении завершения Seal.
						close(sealGate)
					})
				}
				t.Cleanup(func() {
					// Освобождает оба барьера при завершении теста, чтобы ожидающие операции могли закончиться.
					releaseActivation()
					releaseSeal()
				})
				e.sealBlocked = sealGate
				e.activateHook = func(ctx context.Context, _ *core.OutputObserver) error {
					// Сообщает о начале Activate, ждёт отмены и либо возвращает её ошибку, либо задержанный успех после
					// activationGate.

					close(entered)
					<-ctx.Done()
					close(cancelled)
					if late {
						<-activationGate
						return nil
					}
					return ctx.Err()
				}
				// activationResult передаёт результат конкурентного вызова Activate.
				activationResult := make(chan error, 1)
				go func() {
					// Вызывает Activate в отдельной горутине и передаёт результат через activationResult.
					activationResult <- p.Activate(context.Background(), &core.OutputObserver{})
				}()
				<-entered
				// err содержит ожидаемый ErrConflict при повторной Activate до возврата
				// первой.
				if err := p.Activate(context.Background(), &core.OutputObserver{}); !errors.Is(err, core.ErrConflict) {
					t.Fatalf("повторная активация: %v", err)
				}
				// ctx ограничивает Stop одной миллисекундой; cancel освобождает таймер
				// проверки незавершённых операций.
				ctx, cancel := context.WithTimeout(context.Background(), time.Millisecond)
				defer cancel()
				// err содержит ожидаемые ErrCleanupPending и DeadlineExceeded при Stop
				// до завершения Activate и Seal.
				if err := p.Stop(ctx); !errors.Is(err, ErrCleanupPending) || !errors.Is(err, context.DeadlineExceeded) {
					t.Fatalf("остановка: %v", err)
				}
				<-cancelled
				if p.Result().NetworkJoined {
					t.Fatal("активация или Seal ещё не завершены")
				}
				// err содержит ожидаемый запрет ReleaseJoined при незавершённых
				// активации либо Seal.
				if err := p.ReleaseJoined(); !errors.Is(err, ErrCleanupPending) {
					t.Fatal(err)
				}
				// err содержит ожидаемый ErrCleanupPending при повторном Stop до
				// разрешения барьеров.
				if err := p.Stop(ctx); !errors.Is(err, ErrCleanupPending) {
					t.Fatal(err)
				}
				if late {
					// Завершение Seal отдельно не доказывает возврат поздней Activate.
					releaseSeal()
					synctest.Wait()
					if p.Result().NetworkJoined {
						t.Fatal("поздняя Activate ещё работает")
					}
					releaseActivation()
				}
				// err содержит итог первой Activate, которая должна вернуть
				// context.Canceled после Stop.
				if err := <-activationResult; !errors.Is(err, context.Canceled) {
					t.Fatalf("активация после Stop: %v", err)
				}
				if !late {
					if p.Result().NetworkJoined {
						t.Fatal("Seal ещё работает")
					}
					releaseSeal()
				}
				synctest.Wait()
				for range 2 {
					// result содержит итог после завершения активации и Seal; err
					// сообщает об отказе повторного Wait.
					if result, err := p.Wait(context.Background()); err != nil || !result.NetworkJoined || result.Err != nil {
						t.Fatalf("завершение: %+v %v", result, err)
					}
					// err содержит результат повторного Stop уже завершённого
					// Publisher.
					if err := p.Stop(context.Background()); err != nil {
						t.Fatal(err)
					}
				}
				// err содержит ожидаемый ErrConflict при Activate после окончательного
				// завершения.
				if err := p.Activate(context.Background(), &core.OutputObserver{}); !errors.Is(err, core.ErrConflict) {
					t.Fatal(err)
				}
				e.mu.Lock()
				defer e.mu.Unlock()
				if e.activations != 1 || e.seals != 1 || e.released {
					t.Fatalf("активации=%d Seal=%d release=%v", e.activations, e.seals, e.released)
				}
			})
		})
	}
}

// Проверяет повтор допуска после ошибки Activate без открытия сети, Seal или освобождения до
// отдельного Abort вызывающего.
func TestFailedActivationCanRetryWithoutReleasingCallerOwnership(t *testing.T) {
	// r содержит Router для проверки повторной активации после отказа допуска.
	r, _, _, _ := testRouter(t)
	// p содержит Publisher без допустимой сети; e возвращает ErrNotReady из активации.
	p, e := testPublisher(t, r, func(context.Context, rtmp.OutboundTarget, rtmp.OutboundOptions) (connection, error) {
		// Сообщает о запрещённой сети после ошибки активации и возвращает ErrInvalid.

		t.Error("сеть после ошибки активации")
		return nil, core.ErrInvalid
	})
	e.activateHook = func(context.Context, *core.OutputObserver) error {
		// Возвращает ErrNotReady из допуска активации, позволяя проверить повтор до явного Abort вызывающего
		// кода.
		return core.ErrNotReady
	}
	for range 2 {
		// err содержит ожидаемый ErrNotReady очередной Activate до отдельного Abort.
		if err := p.Activate(context.Background(), &core.OutputObserver{}); !errors.Is(err, core.ErrNotReady) {
			t.Fatal(err)
		}
		if p.Result().NetworkJoined {
			t.Fatal("ошибка активации не заменяет Abort вызывающего кода")
		}
	}
	e.mu.Lock()
	if e.activations != 2 || e.sealed || e.released {
		t.Errorf("активации=%d sealed=%v released=%v", e.activations, e.sealed, e.released)
	}
	e.mu.Unlock()
	// err сообщает об отказе явного Abort после двух неудачных активаций.
	if err := p.Abort(context.Background()); err != nil {
		t.Fatal(err)
	}
	// err содержит ожидаемый ErrNotReady при освобождении без разрешения владельца.
	if err := p.ReleaseJoined(); !errors.Is(err, core.ErrNotReady) {
		t.Fatal(err)
	}
}

// Проверяет, что отмена контекста внутри успешного допуска не позволяет запустить сеть и приводит к
// завершению Publisher.
func TestCancelledSuccessfulActivationNeverStartsNetwork(t *testing.T) {
	// r содержит Router для проверки отмены внутри успешной активации.
	r, _, _, _ := testRouter(t)
	// p содержит Publisher без допустимой сети; e отменяет контекст, возвращая успех допуска.
	p, e := testPublisher(t, r, func(context.Context, rtmp.OutboundTarget, rtmp.OutboundOptions) (connection, error) {
		// Сообщает о запрещённой сети после отмены активации и возвращает ErrInvalid.

		t.Error("сеть после отмены активации")
		return nil, core.ErrInvalid
	})
	// ctx задаёт отменяемый срок Activate; cancel вызывается внутри контрольного допуска.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	e.activateHook = func(context.Context, *core.OutputObserver) error {
		// Отменяет контекст активации и возвращает успех допуска, создавая проверяемое окно отмены перед
		// запуском сети.

		cancel()
		return nil
	}
	// err содержит ожидаемый context.Canceled после формально успешного, но отменённого допуска.
	if err := p.Activate(ctx, &core.OutputObserver{}); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	// result содержит итог завершения без запуска сети; err сообщает об отказе ожидания.
	if result, err := p.Wait(context.Background()); err != nil || !result.NetworkJoined {
		t.Fatalf("%+v %v", result, err)
	}
}

// Проверяет единственную текущую активацию после прежней ошибки и отказ конкурентных повторов до и
// после успешного допуска.
func TestConcurrentActivationHasOneAdmissionAfterFailedAttempt(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Проверяет единственный конкурентный допуск Activate после ранее неуспешной попытки и отсутствие
		// автоматического освобождения исполнения.

		// r содержит Router для проверки единственности конкурентной активации после прежнего
		// отказа.
		r, _, _, _ := testRouter(t)
		// p содержит Publisher с проверяемой активацией; e считает допуски и управляет их
		// возвратом.
		p, e := testPublisher(t, r, func(context.Context, rtmp.OutboundTarget, rtmp.OutboundOptions) (connection, error) {
			// Сообщает о недопустимой сети с условным наблюдателем и возвращает ErrInvalid.

			t.Error("недействительный тестовый observer не допускает сеть")
			return nil, core.ErrInvalid
		})
		e.activateHook = func(context.Context, *core.OutputObserver) error {
			// Возвращает ErrNotReady для первой попытки активации перед конкурентной проверкой.
			return core.ErrNotReady
		}
		// err содержит ожидаемый ErrNotReady первоначальной попытки активации.
		if err := p.Activate(context.Background(), &core.OutputObserver{}); !errors.Is(err, core.ErrNotReady) {
			t.Fatal(err)
		}
		// entered сообщает о входе во второй допуск; gate разрешает ему успешно вернуться.
		entered, gate := make(chan struct{}), make(chan struct{})
		// once исключает повторное закрытие барьера активации.
		var once sync.Once
		// release единожды освобождает задержанный успешный допуск Activate.
		release := func() {
			// Единожды освобождает задержанный допуск активации через sync.Once.

			once.Do(func() {
				// Закрывает gate при первом освобождении допуска Activate.
				close(gate)
			})
		}
		t.Cleanup(release)
		// observer содержит точный указатель наблюдателя, который должен получить допуск.
		observer := &core.OutputObserver{}
		e.activateHook = func(_ context.Context, got *core.OutputObserver) error {
			// Проверяет точный указатель наблюдателя, сообщает entered и ждёт gate перед успешным допуском
			// активации.

			if got != observer {
				t.Error("observer был подменён")
			}
			close(entered)
			<-gate
			return nil
		}
		// result передаёт исход единственного разрешённого конкурентного Activate.
		result := make(chan error, 1)
		go func() {
			// Запускает ожидающую Activate в отдельной горутине и передаёт результат через result.
			result <- p.Activate(context.Background(), observer)
		}()
		<-entered
		for range 2 {
			// err содержит ожидаемый ErrConflict конкурентного повтора до завершения
			// допуска.
			if err := p.Activate(context.Background(), observer); !errors.Is(err, core.ErrConflict) {
				t.Fatal(err)
			}
		}
		release()
		// err содержит итог единственного разрешённого Activate после освобождения барьера.
		if err := <-result; err != nil {
			t.Fatal(err)
		}
		// err содержит ожидаемый ErrConflict повторной Activate после успешного допуска.
		if err := p.Activate(context.Background(), observer); !errors.Is(err, core.ErrConflict) {
			t.Fatal(err)
		}
		// err сообщает об отказе остановки активированного Publisher.
		if err := p.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
		e.mu.Lock()
		defer e.mu.Unlock()
		if e.activations != 2 || e.seals != 1 || e.released {
			t.Fatalf("активации=%d Seal=%d release=%v", e.activations, e.seals, e.released)
		}
	})
}
