package chat

import (
	"context"
	"errors"
	"math"
	"sync"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// TestManagerConnectChecksFullYouTubeCredentialSize отклоняет запрос до допуска, если полная
// запись превышает настроенный бюджет, хотя строка ключа помещается.
func TestManagerConnectChecksFullYouTubeCredentialSize(t *testing.T) {
	// ctx ограничивает срок временной БД и Manager.
	ctx := t.Context()
	// store и service владеют изолированной SQLite и её доменными операциями.
	store, service := openChatTestStore(t, ctx)
	// key задаёт синтетический ключ, limits допускает только его исходную строку по размеру.
	const key = "synthetic-key-with-small-budget"
	limits := core.DefaultChatLimits()
	limits.CredentialBytes = uint64(len(key))
	// manager проверяет допуск в RAM перед вызовом адаптера без сети, err относится к запуску.
	manager, err := New(ctx, service, store, limits, []core.ChatAdapterPort{&managerYouTubeFixtureAdapter{}})
	chatTestOK(t, err)
	t.Cleanup(func() {
		// Завершает Manager до закрытия тестового Store.
		chatTestOK(t, manager.Close(context.Background()))
	})
	// request содержит только ключ допустимой структуры и его площадку.
	request := youtubeFixtureRequest("", 0, key)
	// view и err различают отказ во входе и выдачу ссылки на принятую попытку.
	view, err := manager.Connect(ctx, request)
	if !errors.Is(err, core.ErrInvalid) || view.AttemptID != "" {
		t.Fatalf("Принят ключ сверх бюджета полного набора: попытка=%q, ошибка=%v", view.AttemptID, err)
	}
}

// TestManagerEmptyTopologyWithLargeConfiguredLimits проверяет, что лимиты
// ёмкости не требуют выделения всей памяти при пустой топологии.
func TestManagerEmptyTopologyWithLargeConfiguredLimits(t *testing.T) {
	// limits задаёт большие допустимые пределы без активных источников и сессий.
	limits := core.DefaultChatLimits()
	limits.Sources, limits.SourcesPerSession, limits.Sessions = math.MaxInt, math.MaxInt, math.MaxInt
	chatTestOK(t, limits.Validate())
	// manager обрабатывает пустую топологию без обращения к хранилищу.
	manager := &Manager{ctx: t.Context(), limits: limits}
	manager.applyTopologyLocked(nil)
	// candidates сохраняет пустой результат поиска, err сообщает о нарушении чтения.
	candidates, err := manager.readSourceCandidates(core.Repositories{}, nil, nil)
	chatTestOK(t, err)
	if candidates == nil || len(candidates) != 0 {
		t.Fatalf("Пустая топология дала неожиданный набор: %+v", candidates)
	}
}

// TestManagerConnectRetainsSharedQuotaUntilVerificationReturns проверяет, что отменённый
// вызов VK Видео Live продолжает занимать общий допуск до физического выхода адаптера.
func TestManagerConnectRetainsSharedQuotaUntilVerificationReturns(t *testing.T) {
	// ctx ограничивает сценарий, а stop освобождает зависший синтетический адаптер при раннем выходе.
	ctx, stop := context.WithTimeout(t.Context(), 4*time.Second)
	defer stop()
	// store и service обслуживают временную SQLite.
	store, service := openChatTestStore(t, ctx)
	// canceled отмечает полученную адаптером отмену, release позволяет вернуть запоздалый ответ.
	canceled, release := make(chan struct{}), make(chan struct{})
	// probe запрашивает состояние контекста проверки после отмены HTTP-запроса.
	probe := make(chan error)
	// adapter игнорирует отмену после её наблюдения, пока тест не разрешит физический возврат.
	adapter := vkManagerAdapter{
		// verify возвращает валидный набор поздно, чтобы проверить запрет сохранения после отмены.
		verify: func(check context.Context, clientID, clientSecret string) (core.VKVideoLiveChatCredentials, error) {
			select {
			case probe <- check.Err():
			case <-ctx.Done():
				return core.VKVideoLiveChatCredentials{}, ctx.Err()
			}
			<-check.Done()
			close(canceled)
			select {
			case <-release:
				return vkManagerCredentials(clientID, clientSecret, "synthetic-late-token", time.Now().Add(time.Hour)), nil
			case <-ctx.Done():
				return core.VKVideoLiveChatCredentials{}, ctx.Err()
			}
		},
	}
	// limits допускает лишь одну физическую попытку для всех платформ.
	limits := core.DefaultChatLimits()
	limits.Authorizations = 1
	// twitchAdapter обеспечивает доступность второго способа подключения без сети.
	twitchAdapter := &managerTwitchDeviceFixture{ctx: ctx, interval: time.Second, ttl: time.Minute}
	// manager выдаёт публичные адреса попыток, err сообщает отказ создания фикстуры.
	manager, err := New(ctx, service, store, limits, []core.ChatAdapterPort{adapter, twitchAdapter})
	chatTestOK(t, err)
	t.Cleanup(func() {
		// Остановка освобождает Manager перед закрытием тестовой SQLite.
		chatTestOK(t, manager.Close(context.Background()))
	})
	// request и cancelRequest отделяют срок HTTP-вызова от срока проверки Manager.
	request, cancelRequest := context.WithCancel(ctx)
	defer cancelRequest()
	// pending содержит публичное состояние первой попытки до окончания внешней проверки.
	pending, err := manager.Connect(request, core.ChatAuthorizationRequest{
		Provider: core.ChatVKVideoLive, ClientID: "synthetic-vk-client", ClientSecret: "synthetic-vk-secret",
	})
	chatTestOK(t, err)
	cancelRequest()
	if pending.Provider != core.ChatVKVideoLive || pending.State != core.ChatAuthorizationPending || !pending.Cancelable ||
		pending.UserCode != "" || pending.VerificationURI != "" {
		t.Fatalf("Неожиданная публичная попытка: %+v", pending)
	}
	chatTestOK(t, chatTestReceive(t, ctx, probe))
	chatTestOK(t, manager.CancelAuthorization(ctx, pending.AttemptID))
	chatTestOK(t, manager.CancelAuthorization(ctx, pending.AttemptID))
	chatTestReceive(t, ctx, canceled)
	// view подтверждает логическое завершение, err относится только к чтению публичного состояния.
	view, err := manager.Authorization(ctx, pending.AttemptID)
	chatTestOK(t, err)
	if view.State != core.ChatAuthorizationCancelled || view.Cancelable {
		t.Fatalf("Отмена не закрыла публичную попытку: %+v", view)
	}
	if _, err = manager.Connect(ctx, core.ChatAuthorizationRequest{Provider: core.ChatTwitch, ClientID: "synthetic-twitch-client"}); !errors.Is(err, core.ErrBusy) {
		t.Fatalf("Второй адаптер получил допуск до выхода первой попытки: %v", err)
	}
	if twitchAdapter.starts.Load() != 0 {
		t.Fatal("Адаптер Twitch получил вызов при исчерпанном общем допуске")
	}
	close(release)
	chatTestOK(t, manager.Close(ctx))
	// accounts показывает отсутствие записи от отменённого подключения.
	accounts, err := service.ListChatAccounts(ctx, core.ChatAccountQuery{Limit: 10})
	chatTestOK(t, err)
	if len(accounts) != 0 {
		t.Fatal("Запоздавшая проверка создала аккаунт после отмены")
	}
}

// TestManagerDeleteAccountRetiresLaneOnLastRelease проверяет, что CAS сохраняет
// занятый слот при конфликте, а подтверждённое удаление освобождает его после последнего пользователя.
func TestManagerDeleteAccountRetiresLaneOnLastRelease(t *testing.T) {
	// ctx ограничивает операции временной SQLite.
	ctx, stop := context.WithTimeout(t.Context(), 4*time.Second)
	defer stop()
	// store и service предоставляют реальные конфигурационные чтения и CAS.
	store, service := openChatTestStore(t, ctx)
	// original и neighbor различают прежний слот и ожидающий свободного места аккаунт.
	original := createCredentialTestAccount(t, ctx, service, "synthetic-deleted-lane")
	neighbor := createCredentialTestAccount(t, ctx, service, "synthetic-next-lane")
	// limits позволяет проверить восстановление единственного места AccountOwners.
	limits := core.DefaultChatLimits()
	limits.AccountOwners = 1
	// manager владеет экземпляром CredentialOwner, err сообщает об отказе запуска.
	manager, err := New(ctx, service, store, limits, nil)
	chatTestOK(t, err)
	t.Cleanup(func() {
		// Manager завершается прежде закрытия тестовой базы.
		chatTestOK(t, manager.Close(context.Background()))
	})
	// owner предоставляет внутренний слот, который сохраняют пользователи полномочий.
	owner := manager.ownedCredentials
	// operation и lane принадлежат первому пользователю, release завершает его участие,
	// err сообщает об отказе получить слот.
	operation, lane, release, err := owner.begin(ctx, original.Account.ID)
	chatTestOK(t, err)
	release = sync.OnceFunc(release)
	defer release()
	chatTestOK(t, lane.enter(operation))
	lane.spent = credentialReceipt{revision: original.Credentials.CredentialRevision, writeID: original.Credentials.CredentialWriteID}
	lane.leave()
	if err = manager.DeleteAccount(ctx, original.Account.ID, original.Account.Revision+1); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("Удаление с устаревшей ревизией вернуло %v", err)
	}
	owner.mu.Lock()
	// retired показывает, не принял ли владелец отказ CAS за подтверждённое удаление.
	retired := lane.retired
	owner.mu.Unlock()
	if retired {
		t.Fatal("Конфликт CAS ошибочно отозвал слот")
	}
	chatTestOK(t, manager.DeleteAccount(ctx, original.Account.ID, original.Account.Revision))
	// same и finish относятся к новому пользователю всё ещё единственного слота удалённого ID.
	_, same, finish, err := owner.begin(ctx, original.Account.ID)
	chatTestOK(t, err)
	finish = sync.OnceFunc(finish)
	defer finish()
	if same != lane {
		t.Fatal("Удаление создало второй слот до выхода действующего пользователя")
	}
	release()
	if _, err = owner.Credentials(ctx, neighbor.Account.ID); !errors.Is(err, core.ErrBusy) {
		t.Fatalf("Реестр освободил место до выхода последнего пользователя: %v", err)
	}
	finish()
	// credentials подтверждает, что отметка об израсходованном токене не удержала удалённый слот.
	credentials, err := owner.Credentials(ctx, neighbor.Account.ID)
	chatTestOK(t, err)
	if credentials.AccountID != neighbor.Account.ID {
		t.Fatal("Освободившийся слот не позволил прочитать соседний аккаунт")
	}
}
