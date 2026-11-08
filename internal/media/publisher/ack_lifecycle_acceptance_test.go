package publisher

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"testing/synctest"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/media/rtmp"
)

// Снимок транспорта намеренно повторяет уже переданное событие: Publisher не должен суммировать его второй раз.
type ackAcceptanceConnection struct {
	*testConnection
	snapshot   rtmp.OutboundSnapshot
	beforeJoin func()
}

func (c *ackAcceptanceConnection) Snapshot() rtmp.OutboundSnapshot { return c.snapshot }
func (c *ackAcceptanceConnection) Wait(ctx context.Context) error {
	if err := c.testConnection.Wait(ctx); err != nil {
		return err
	}
	if c.beforeJoin != nil {
		c.beforeJoin()
	}
	return nil
}

func TestACKLifecycleAcceptanceFailedStartupReconnectAndNewRun(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		router, _, _, _ := testRouter(t)
		origin := time.Now().UTC()
		attempts := 0
		first := &ackAcceptanceConnection{testConnection: newTestConnection(), snapshot: rtmp.OutboundSnapshot{AckAheadEventsTotal: 1, AckAheadBytesLast: 8, AckAheadBytesMax: 8, AckLastAnomalyAt: origin.Add(time.Second)}}
		second := &ackAcceptanceConnection{testConnection: newTestConnection()}
		p, _ := testPublisher(t, router, func(_ context.Context, _ rtmp.OutboundTarget, o rtmp.OutboundOptions) (connection, error) {
			attempts++
			if o.OnACKAnomaly == nil {
				t.Error("OnACKAnomaly не установлен до DialOutbound")
				return nil, errors.New("нет OnACKAnomaly")
			}
			switch attempts {
			case 1:
				o.OnACKAnomaly(4, origin)
				return nil, errors.New("согласование прервано после ACK")
			case 2:
				first.opts = o
				o.OnACKAnomaly(8, origin.Add(time.Second))
				return first, nil
			default:
				second.opts = o
				o.OnACKAnomaly(2, origin.Add(3*time.Second))
				return second, nil
			}
		})
		first.beforeJoin = func() { first.opts.OnACKAnomaly(6, origin.Add(2*time.Second)) }
		startTestPublisher(p)
		synctest.Wait()
		if s := p.Snapshot(); s.AckAheadEventsTotal != 1 || s.AckAheadBytesLast != 4 || s.Connections != 0 {
			t.Fatalf("событие установления соединения потеряно: %+v", s)
		}
		time.Sleep(p.options.RetryInitial)
		synctest.Wait()
		for range 3 {
			if s := p.Snapshot(); s.AckAheadEventsTotal != 2 || s.AckAheadBytesLast != 8 || s.AckAheadBytesMax != 8 || s.Connections != 1 {
				t.Fatalf("быстрые попытки/повторный Snapshot: %+v", s)
			}
		}
		first.mu.Lock()
		first.err = errors.New("разрыв сети")
		first.mu.Unlock()
		_ = first.Close()
		synctest.Wait()
		time.Sleep(2 * p.options.RetryInitial)
		synctest.Wait()
		if s := p.Snapshot(); s.AckAheadEventsTotal != 4 || s.AckAheadBytesLast != 2 || s.AckAheadBytesMax != 8 || s.AckLastAnomalyAt != origin.Add(3*time.Second) || s.Connections != 2 || s.Reconnects != 1 {
			t.Fatalf("переподключение или завершение потеряло либо повторило события: %+v", s)
		}
		if err := p.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
		if !p.Result().NetworkJoined {
			t.Fatal("Stop не дождался завершения OnACKAnomaly")
		}
		fresh, _ := testPublisher(t, router, func(context.Context, rtmp.OutboundTarget, rtmp.OutboundOptions) (connection, error) {
			return newTestConnection(), nil
		})
		if s := fresh.Snapshot(); s.AckAheadEventsTotal != 0 || s.AckAheadBytesLast != 0 || s.AckAheadBytesMax != 0 || !s.AckLastAnomalyAt.IsZero() {
			t.Fatalf("новый запуск унаследовал метрики: %+v", s)
		}
	})
}

func TestACKLifecycleAcceptanceConcurrentSnapshotsRemainCoherent(t *testing.T) {
	router, _, _, _ := testRouter(t)
	options := make(chan rtmp.OutboundOptions, 1)
	c := newTestConnection()
	p, _ := testPublisher(t, router, func(_ context.Context, _ rtmp.OutboundTarget, o rtmp.OutboundOptions) (connection, error) {
		c.opts = o
		options <- o
		return c, nil
	})
	startTestPublisher(p)
	o := <-options
	if o.OnACKAnomaly == nil {
		t.Fatal("нет OnACKAnomaly")
	}
	origin := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	var wg sync.WaitGroup
	wg.Go(func() {
		for n := uint64(1); n <= 1000; n++ {
			o.OnACKAnomaly(n, origin.Add(time.Duration(n)*time.Nanosecond))
		}
	})
	for range 1000 {
		s := p.Snapshot()
		n := s.AckAheadEventsTotal
		if s.AckAheadBytesLast != n || s.AckAheadBytesMax != n || n > 0 && s.AckLastAnomalyAt != origin.Add(time.Duration(n)*time.Nanosecond) {
			t.Fatalf("несогласованный набор ACK: %+v", s)
		}
	}
	wg.Wait()
	if s := p.Snapshot(); s.AckAheadEventsTotal != 1000 || s.AckAheadBytesMax != 1000 {
		t.Fatalf("потеряны вызовы OnACKAnomaly: %+v", s)
	}
}

func TestACKLifecycleAcceptanceSaturationDoesNotDisableCallback(t *testing.T) {
	router, _, _, _ := testRouter(t)
	var callback func(uint64, time.Time)
	p, _ := testPublisher(t, router, func(_ context.Context, _ rtmp.OutboundTarget, o rtmp.OutboundOptions) (connection, error) {
		callback = o.OnACKAnomaly
		return nil, errors.New("согласование прервано")
	})
	if _, err := p.attempt(context.Background()); err == nil {
		t.Fatal("ожидался отказ установления соединения")
	}
	if callback == nil {
		t.Fatal("нет OnACKAnomaly при установлении соединения")
	}
	// Предел W Publisher не должен лишать новые соединения точных наблюдений ACK.
	p.noteWrite(math.MaxInt)
	p.noteWrite(math.MaxInt)
	p.noteWrite(2)
	at := time.Now().UTC()
	callback(4, at)
	callback(9, at.Add(time.Nanosecond))
	callback(2, at.Add(2*time.Nanosecond))
	if s := p.Snapshot(); !s.Saturated || s.WrittenBytes != math.MaxUint64 || s.AckAheadEventsTotal != 3 || s.AckAheadBytesLast != 2 || s.AckAheadBytesMax != 9 || s.AckLastAnomalyAt != at.Add(2*time.Nanosecond) {
		t.Fatalf("насыщение W отключило ACK: %+v", s)
	}
}
