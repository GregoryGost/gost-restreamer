package chat

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
)

// authorizationDeleteStore имитирует потерю подтверждения успешного DELETE и недоступность
// сверки. Изменения до этой границы выполняет мигрированная SQLite.
type authorizationDeleteStore struct {
	// Store хранит рабочую схему, CAS и внешние ключи.
	*storage.Store
	// loseDeleteAck выбирает, потеряется ли ответ ближайшей успешной записи.
	loseDeleteAck atomic.Bool
	// unavailableRead удерживает отказ чтения после потерянного подтверждения до разрешения теста.
	unavailableRead atomic.Bool
}

// authorizationDeleteReceiptStore ограничивает отказ проверки квитанции удаляемым аккаунтом,
// чтобы чтение соседнего аккаунта могло отличить занятый слот от ошибки SQLite.
type authorizationDeleteReceiptStore struct {
	// credentialReceiptTestStore теряет ответ после фактического CAS и хранит число записей.
	*credentialReceiptTestStore
	// accountID адресует единственную запись, для которой недоступна сверка результата.
	accountID core.ID
}

// GetChatCredentialResult читает id в пределах ctx. Соседние аккаунты всегда читаются с SQLite.
func (s *authorizationDeleteReceiptStore) GetChatCredentialResult(ctx context.Context, id core.ID) (core.ChatCredentialResult, error) {
	if id != s.accountID {
		return s.Service.GetChatCredentialResult(ctx, id)
	}
	return s.credentialReceiptTestStore.GetChatCredentialResult(ctx, id)
}

// Write передаёт callback настоящей транзакции и в выбранном случае теряет её успешный ответ.
func (s *authorizationDeleteStore) Write(ctx context.Context, callback func(core.Repositories) error) error {
	// err содержит доказанный результат SQLite до имитации потери ответа.
	err := s.Store.Write(ctx, callback)
	if err == nil && s.loseDeleteAck.CompareAndSwap(true, false) {
		s.unavailableRead.Store(true)
		return context.DeadlineExceeded
	}
	return err
}

// View выполняет согласованное чтение через SQLite при доступном подтверждении.
func (s *authorizationDeleteStore) View(ctx context.Context, callback func(core.Repositories) error) error {
	if s.unavailableRead.Load() {
		return core.ErrBusy
	}
	return s.Store.View(ctx, callback)
}

// TestChatDeleteAccountRetainsUncertainSlotUntilProof проверяет вместимость CredentialOwner
// с реальными CAS и FK. Потеря ответа DELETE не освобождает место, пока SQLite не докажет,
// что прежней записи больше нет. Повторный DELETE с подтверждённым отсутствием возвращает 404.
func TestChatDeleteAccountRetainsUncertainSlotUntilProof(t *testing.T) {
	// ctx ограничивает операции фикстуры, cancel освобождает её таймер.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// fixture содержит действующие назначения и доступ к мигрированной SQLite.
	fixture := newChatManagerTestFixture(t, ctx)
	// original находится под ограничением FK до удаления использующих его Source, err сообщает об отказе чтения.
	original, err := fixture.service.GetChatCredentialResult(ctx, fixture.sources[0].AccountID)
	chatTestOK(t, err)
	// other представляет независимый существующий аккаунт, которому требуется свободное место.
	other := createCredentialTestAccount(t, ctx, fixture.service, "account-after-proven-delete")
	// store теряет ответ только после выбранного COMMIT.
	store := &authorizationDeleteStore{Store: fixture.store}
	// service использует управляемую границу подтверждения DELETE.
	service, err := core.NewService(store, fixture.store.Incarnation())
	chatTestOK(t, err)
	// receiptStore имитирует потерю ответа о записи полномочий и недоступное чтение их квитанции.
	receiptStore := &authorizationDeleteReceiptStore{
		credentialReceiptTestStore: &credentialReceiptTestStore{Service: fixture.service, unreadable: true},
		accountID:                  original.Account.ID,
	}
	// limits позволяет ровно одному аккаунту удерживать долгоживущую необработанную квитанцию.
	limits := core.DefaultChatLimits()
	limits.AccountOwners = 1
	// owner сохраняет реальные полномочия и учитывает потерю ответа, err сообщает об отказе настройки.
	owner, err := NewCredentialOwner(receiptStore, limits, nil)
	chatTestOK(t, err)
	t.Cleanup(func() {
		// Закрывает явно внедрённого владельца после Manager и до временной SQLite.
		// cleanup ограничивает ожидание завершения, stop освобождает таймер.
		cleanup, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		chatTestOK(t, owner.Close(cleanup))
	})
	// manager выполняет DELETE и замену поверх одной SQLite, err сообщает об отказе запуска.
	manager, err := New(ctx, service, store, limits, nil, owner)
	chatTestOK(t, err)
	t.Cleanup(func() {
		// Завершает фоновую работу перед закрытием владельца полномочий.
		// cleanup ограничивает ожидание, stop освобождает независимый таймер.
		cleanup, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		chatTestOK(t, manager.Close(cleanup))
	})
	// bundle содержит проверенную замену; исходная попытка записи получит искусственный сбой лишь после COMMIT.
	bundle := core.ChatCredentialBundle{YouTube: &core.YouTubeChatCredentials{APIKey: "synthetic-verified-replacement"}}
	if _, err = owner.Replace(ctx, manager, original.Account.ID, core.ChatYouTube, bundle, "replacement-with-lost-ack", original.Account.Revision); !errors.Is(err, ErrCredentialCommitUnknown) {
		t.Fatalf("Подготовка не оставила неподтверждённой квитанции: %v", err)
	}
	// committed содержит подтверждённую настоящей SQLite новую ревизию удаляемого аккаунта.
	committed, err := fixture.service.GetChatCredentialResult(ctx, original.Account.ID)
	chatTestOK(t, err)
	if committed.Account.Revision != original.Account.Revision+1 || committed.Credentials.CredentialWriteID != "replacement-with-lost-ack" {
		t.Fatal("Фикстура не зафиксировала замену перед потерей подтверждения")
	}

	if err = manager.DeleteAccount(ctx, committed.Account.ID, original.Account.Revision); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("DELETE с устаревшей ревизией вернул %v", err)
	}
	assertAuthorizationOwnerBusy(t, ctx, owner, other.Account.ID)
	if err = manager.DeleteAccount(ctx, committed.Account.ID, committed.Account.Revision); !errors.Is(err, core.ErrInUse) {
		t.Fatalf("DELETE используемого аккаунта вернул %v", err)
	}
	assertAuthorizationOwnerBusy(t, ctx, owner, other.Account.ID)

	// source задаёт один из реальных внешних ключей на удаляемый аккаунт.
	for _, source := range fixture.sources {
		chatTestOK(t, fixture.service.DeleteChatSource(ctx, source.ID, source.Revision))
	}
	store.loseDeleteAck.Store(true)
	if err = manager.DeleteAccount(ctx, committed.Account.ID, committed.Account.Revision); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Потеря подтверждения DELETE дала ошибку %v", err)
	}
	if _, err = fixture.service.GetChatAccount(ctx, committed.Account.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("Удаление не зафиксировано перед потерей подтверждения: %v", err)
	}
	assertAuthorizationOwnerBusy(t, ctx, owner, other.Account.ID)
	store.unavailableRead.Store(false)
	if err = manager.DeleteAccount(ctx, committed.Account.ID, committed.Account.Revision); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("Повтор подтверждённого DELETE вернул %v", err)
	}
	// result подтверждает обслуживание другого аккаунта, err сообщает об отказе допуска или чтения.
	result, err := owner.Credentials(ctx, other.Account.ID)
	chatTestOK(t, err)
	if result.AccountID != other.Account.ID || receiptStore.writes != 1 {
		t.Fatal("Повторный DELETE повторил сохранение полномочий или освободил не тот слот")
	}
}

// assertAuthorizationOwnerBusy проверяет, что после недоказанного удаления id не занял
// единственное удерживаемое место владельца полномочий.
func assertAuthorizationOwnerBusy(t *testing.T, ctx context.Context, owner *CredentialOwner, id core.ID) {
	t.Helper()
	// err отличает занятый слот от чтения соседнего аккаунта.
	if _, err := owner.Credentials(ctx, id); !errors.Is(err, core.ErrBusy) {
		t.Fatalf("CredentialOwner освободил место без доказанного удаления: %v", err)
	}
}

// TestChatConnectDeadlineRejectsLateVerifiedBundle проверяет результат, поступивший
// уже после локального TTL. Даже успешный ответ не даёт права менять действующие полномочия.
func TestChatConnectDeadlineRejectsLateVerifiedBundle(t *testing.T) {
	// ctx ограничивает сценарий и выпускает задержанный адаптер при досрочной остановке теста.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// store и service предоставляют мигрированную SQLite и её серверные чтения.
	store, service := openChatTestStore(t, ctx)
	// original содержит действующие реквизиты VK до попытки их обновления, err сообщает об отказе записи.
	original, err := service.CreateChatAccount(ctx, core.ChatAccount{Provider: core.ChatVKVideoLive},
		core.ChatCredentialBundle{VKVideoLive: &core.VKVideoLiveChatCredentials{
			ClientID: "initial-app", ClientSecret: "initial-secret", AccessToken: "initial-access",
			ExpiresAt: time.Now().Add(time.Hour).UTC().Truncate(time.Microsecond),
		}}, "original-vk-receipt")
	chatTestOK(t, err)
	// expired отмечает истечение контекста проверки, release разрешает вернуть поздний ответ.
	expired, release := make(chan struct{}), make(chan struct{})
	// adapter моделирует обмен, физически возвращающий подтверждение после истечения TTL.
	adapter := vkManagerAdapter{
		verify: func(check context.Context, id, secret string) (core.VKVideoLiveChatCredentials, error) {
			// check ограничивает сетевую проверку; id и secret становятся реквизитами запоздалого набора.
			<-check.Done()
			close(expired)
			select {
			case <-release:
				return vkManagerCredentials(id, secret, "late-vk-access", time.Now().Add(time.Hour)), nil
			case <-ctx.Done():
				return core.VKVideoLiveChatCredentials{}, ctx.Err()
			}
		},
	}
	// limits задаёт наблюдаемый TTL короче всей проверки, но дольше времени подготовки.
	limits := core.DefaultChatLimits()
	limits.AuthorizationTTL = 400 * time.Millisecond
	// manager выполняет единую операцию Connect с управляемой сетевой границей.
	manager, err := New(ctx, service, store, limits, []core.ChatAdapterPort{adapter})
	chatTestOK(t, err)
	t.Cleanup(func() {
		// Прерывает проверку и освобождает Store, даже если сценарий остановился до отправки ответа.
		// cleanup ограничивает завершение, stop освобождает таймер.
		cleanup, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		chatTestOK(t, manager.Close(cleanup))
	})
	// attempt адресует замену, чей допустимый срок истечёт до завершения обмена.
	attempt, err := manager.Connect(ctx, core.ChatAuthorizationRequest{Provider: core.ChatVKVideoLive,
		AccountID: original.Account.ID, ExpectedRevision: original.Account.Revision, ClientID: "updated-app", ClientSecret: "updated-secret"})
	chatTestOK(t, err)
	chatTestReceive(t, ctx, expired)
	// view даёт состояние после истечения контекста адаптера, ещё удерживаемого до физического возврата.
	view, err := manager.Authorization(ctx, attempt.AttemptID)
	chatTestOK(t, err)
	if view.State != core.ChatAuthorizationExpired || view.Cancelable {
		t.Fatalf("Попытка осталась открытой после истечения TTL: %+v", view)
	}
	if err = manager.CancelAuthorization(ctx, attempt.AttemptID); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("Истёкшая попытка подтвердила новую отмену: %v", err)
	}
	close(release)
	// tick ждёт физическое окончание рабочего вызова после отправки позднего подтверждения.
	tick := time.NewTicker(time.Millisecond)
	defer tick.Stop()
	for {
		manager.mu.Lock()
		// running отмечает, закончил ли worker разбор уже полученного от адаптера ответа.
		running := manager.authorizations[attempt.AttemptID].running
		manager.mu.Unlock()
		if !running {
			break
		}
		chatTestReceive(t, ctx, tick.C)
	}
	// saved проверяет обе ревизии, WriteID и набор непосредственно в SQLite после возврата worker.
	saved, err := service.GetChatCredentialResult(ctx, original.Account.ID)
	chatTestOK(t, err)
	if !saved.Matches(original) {
		t.Fatal("Запоздалое подтверждение изменило действующий набор полномочий")
	}
}
