package chat

import (
	"context"
	"errors"
	"testing"
	"testing/synctest"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/chat/twitch"
	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// twitchPollingFixture записывает моменты опросов и выдаёт последовательность временных ответов.
type twitchPollingFixture struct {
	// chatTestAdapter выполняет контракт чтения, который в этом тесте не вызывается.
	chatTestAdapter
	// started задаёт начало отсчёта времени виртуальных опросов.
	started time.Time
	// polls хранит моменты вызовов. Тест читает их только после synctest.Wait.
	polls []time.Duration
}

// Provider возвращает платформу зарегистрированной попытки Device Code.
func (*twitchPollingFixture) Provider() core.ChatProvider { return core.ChatTwitch }

// BeginDevice выдаёт синтетический код со сроком короче локального TTL.
func (*twitchPollingFixture) BeginDevice(context.Context, string) (twitch.DeviceChallenge, error) {
	return twitch.DeviceChallenge{
		DeviceCode: "synthetic-private-device", UserCode: "ABCDEFGH", VerificationURI: "https://www.twitch.tv/activate",
		Interval: 2 * time.Second, ExpiresIn: 18 * time.Second,
	}, nil
}

// PollDevice имитирует незавершённый вход, таймаут и замедление по требованию Twitch.
func (a *twitchPollingFixture) PollDevice(context.Context, string, string) (twitch.DevicePoll, error) {
	a.polls = append(a.polls, time.Since(a.started))
	switch len(a.polls) {
	case 2:
		return twitch.DevicePoll{}, context.DeadlineExceeded
	case 3:
		return twitch.DevicePoll{State: twitch.DeviceSlowDown}, nil
	default:
		return twitch.DevicePoll{State: twitch.DevicePending}, nil
	}
}

// TestTwitchDevicePollingBacksOffWithinProviderTTL измеряет время сетевых обращений
// на виртуальных часах. Проверяется фактический интервал после таймаута и slow_down,
// а также очистка параметров подтверждения при истечении срока Twitch.
func TestTwitchDevicePollingBacksOffWithinProviderTTL(t *testing.T) {
	// Функция запускает публичный вход без SQLite и сети, поскольку подтверждения не будет.
	synctest.Test(t, func(t *testing.T) {
		// ctx и cancel управляют временем жизни попытки. Перед выходом тест ждёт её остановки.
		ctx, cancel := context.WithCancel(t.Context())
		// adapter проверяет расположение каждого запроса на виртуальной шкале.
		adapter := &twitchPollingFixture{started: time.Now()}
		// limits содержит обычные конечные лимиты. Провайдер выдаёт более короткий срок кода.
		limits := core.DefaultChatLimits()
		limits.Authorizations = 1
		// manager хранит только состояние, которое необходимо для авторизации без медиавыходов.
		manager := &Manager{
			ctx: ctx, limits: limits, adapters: map[core.ChatProvider]core.ChatAdapterPort{core.ChatTwitch: adapter},
			discoverSlots: make(chan struct{}, limits.HTTPRequests), authorizations: make(map[core.ID]*chatAuthorizationAttempt),
		}
		// Функция прекращает попытку и ждёт физического освобождения её работы.
		defer func() {
			cancel()
			manager.work.Wait()
		}()
		// pending содержит публичные данные до получения решения Twitch, err сообщает об отказе старта.
		pending, err := manager.ConnectTwitch(ctx, core.ChatAuthorizationRequest{Provider: core.ChatTwitch, ClientID: "synthetic-client"})
		chatTestOK(t, err)
		if pending.State != core.ChatAuthorizationPending || pending.ExpiresAt.Sub(adapter.started) != 18*time.Second || pending.UserCode == "" {
			t.Fatalf("Получены неполные данные для подтверждения: %+v", pending)
		}
		// err подтверждает общий лимит, когда второй запрос создавал бы другой новый аккаунт.
		if _, err := manager.ConnectTwitch(ctx, core.ChatAuthorizationRequest{Provider: core.ChatTwitch, ClientID: "synthetic-other-client"}); !errors.Is(err, core.ErrBusy) {
			t.Fatalf("Переполненный реестр принял новую попытку: %v", err)
		}

		time.Sleep(17 * time.Second)
		synctest.Wait()
		// want отражает минимальный интервал, его удвоение после таймаута и прибавку 5 секунд после slow_down.
		want := [...]time.Duration{2 * time.Second, 4 * time.Second, 8 * time.Second, 17 * time.Second}
		if len(adapter.polls) != len(want) {
			t.Fatalf("Выполнены опросы в моменты %v, ожидаются %v", adapter.polls, want)
		}
		// index связывает требуемый момент опроса expected с записанным запросом.
		for index, expected := range want {
			if adapter.polls[index] != expected {
				t.Fatalf("Опросы выполнены в моменты %v, ожидаются %v", adapter.polls, want)
			}
		}
		time.Sleep(time.Second)
		synctest.Wait()
		// expired содержит доступный итог попытки после удаления публичного кода.
		expired, err := manager.Authorization(ctx, pending.AttemptID)
		chatTestOK(t, err)
		if expired.State != core.ChatAuthorizationExpired || expired.UserCode != "" || expired.VerificationURI != "" {
			t.Fatalf("После истечения срока остались данные подтверждения: %+v", expired)
		}
		// err отличает завершившийся срок от ещё доступной отмены.
		if err := manager.CancelAuthorization(ctx, pending.AttemptID); !errors.Is(err, core.ErrConflict) {
			t.Fatalf("Истёкшая попытка допускает отмену: %v", err)
		}
		time.Sleep(limits.AuthorizationTTL)
		// err подтверждает освобождение терминальных данных после конечного срока хранения.
		if _, err := manager.Authorization(ctx, pending.AttemptID); !errors.Is(err, core.ErrNotFound) {
			t.Fatalf("Не освобождена запись об истёкшей попытке: %v", err)
		}
	})
}
