package chat

import (
	"context"
	"errors"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// credentialTestBundle создаёт полный синтетический набор Twitch с различимыми токенами suffix.
func credentialTestBundle(suffix string) core.ChatCredentialBundle {
	return core.ChatCredentialBundle{Twitch: &core.TwitchChatCredentials{
		ClientID: "test-client", UserID: "123", AccessToken: "test-access-" + suffix, RefreshToken: "test-refresh-" + suffix,
		Scopes: []string{"user:read:chat"}, ExpiresAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC),
	}}
}

// createCredentialTestAccount сохраняет новый тестовый аккаунт через service в контексте ctx.
// t сообщает о невозможности создать независимого владельца полномочий.
func createCredentialTestAccount(t *testing.T, ctx context.Context, service *core.Service, suffix string) core.ChatCredentialResult {
	t.Helper()
	// account содержит полный результат SQLite, err сообщает об отказе сохранения.
	account, err := service.CreateChatAccount(ctx, core.ChatAccount{Provider: core.ChatTwitch}, credentialTestBundle(suffix), core.ID("write-"+suffix))
	chatTestOK(t, err)
	return account
}

// credentialTestMutator применяет callback внутри тестовой границы между CredentialOwner и Manager.
// Тесты с ним проверяют CAS и сверку SQLite без утверждений о переустановке reader.
type credentialTestMutator struct{}

// mutateAccount передаёт ctx в commit. id подтверждает, что квитанция относится к тому же аккаунту.
func (credentialTestMutator) mutateAccount(ctx context.Context, id core.ID, commit func(context.Context) (credentialReplacement, error)) (core.ChatApplyResult, error) {
	// current содержит принятый аккаунт. err сообщает об отказе CAS или сверки.
	current, err := commit(ctx)
	if err != nil {
		return core.ChatApplyResult{}, err
	}
	if current.account.ID != id {
		return core.ChatApplyResult{}, core.ErrStale
	}
	return core.ChatApplyResult{Revision: current.account.Revision}, nil
}

// TestChatCredentialOwnerSerializesExchangeAndReleasesSlots проверяет сериализацию в пределах
// аккаунта, доступ к соседу, ограничение слотов, отмену ожидающего клиента и сообщение Done.
// t сообщает об ошибках обмена и сохраняемых ревизий.
func TestChatCredentialOwnerSerializesExchangeAndReleasesSlots(t *testing.T) {
	// ctx и cancel ограничивают всё ожидание управляемого обмена и временной SQLite.
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	// service использует рабочие транзакции изолированного SQLite.
	_, service := openChatTestStore(t, ctx)
	// a и b представляют независимые аккаунты с различимыми исходными полномочиями.
	a := createCredentialTestAccount(t, ctx, service, "a")
	b := createCredentialTestAccount(t, ctx, service, "b")
	// c представляет третьего владельца, для которого не останется слота.
	c := createCredentialTestAccount(t, ctx, service, "c")
	// started подтверждает вход первого обмена a, release разрешает сохранить единственный новый набор.
	started := make(chan struct{})
	release := make(chan struct{})
	// bStarted отмечает выполненный вход соседа, bRelease разрешает ему освободить свой слот.
	bStarted := make(chan struct{})
	bRelease := make(chan struct{})
	// exchanges считает обращения за обменом a с исходным токеном.
	var exchanges atomic.Int32
	// limits предоставляет два независимых слота аккаунтов.
	limits := core.DefaultChatLimits()
	limits.AccountOwners = 2
	// owner сериализует обмен a, err сообщает создание или отказ на проверяемой границе.
	owner, err := NewCredentialOwner(service, limits, func(ctx context.Context, current core.ChatCredentials) (CredentialRefreshResult, error) {
		// Обмен удерживает первые два аккаунта в отдельных слотах. Уже обновлённый a не требует обмена.
		if current.AccountID == b.Account.ID {
			close(bStarted)
			select {
			case <-bRelease:
				return CredentialRefreshResult{}, nil
			case <-ctx.Done():
				return CredentialRefreshResult{}, ctx.Err()
			}
		}
		if current.AccountID != a.Account.ID || current.CredentialRevision != 1 {
			return CredentialRefreshResult{}, nil
		}
		if exchanges.Add(1) != 1 {
			return CredentialRefreshResult{}, errors.New("повтор исходного обмена")
		}
		close(started)
		select {
		case <-release:
			// bundle заменяет обе части полномочий по одной подтверждённой операции.
			bundle := credentialTestBundle("refreshed")
			return CredentialRefreshResult{Bundle: &bundle, PreviousMayBeConsumed: true}, nil
		case <-ctx.Done():
			return CredentialRefreshResult{PreviousMayBeConsumed: true}, ctx.Err()
		}
	})
	chatTestOK(t, err)
	t.Cleanup(func() {
		// Закрывает владельца даже в случае раннего отказа до release.
		chatTestOK(t, owner.Close(context.Background()))
	})
	// result сохраняет данные и ошибку каждого управляемого вызова в его канале.
	type result struct {
		// value содержит принятые полномочия. err объясняет недоступность результата.
		value core.ChatCredentials
		err   error
	}
	// first принимает результат первичного обмена.
	first := make(chan result, 1)
	go func() {
		// value и err передают итог первичного обмена через first.
		value, err := owner.Credentials(ctx, a.Account.ID)
		first <- result{value, err}
	}()
	chatTestReceive(t, ctx, started)
	// waiter и stop ограничивают пользователя, ожидающего тот же аккаунт.
	waiter, stop := context.WithTimeout(ctx, 25*time.Millisecond)
	defer stop()
	if _, err = owner.Credentials(waiter, a.Account.ID); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Отменённое ожидание того же аккаунта вернуло %v", err)
	}
	// concurrent получает результат b после проверки независимого допуска от a.
	concurrent := make(chan result, 1)
	go func() {
		// value и err передают результат соседнего аккаунта.
		value, err := owner.Credentials(ctx, b.Account.ID)
		concurrent <- result{value, err}
	}()
	chatTestReceive(t, ctx, bStarted)
	if _, err = owner.Credentials(ctx, c.Account.ID); !errors.Is(err, core.ErrBusy) {
		t.Fatalf("Два занятых слота аккаунтов не ограничили третьего: %v", err)
	}
	// second принимает результат повторного чтения a во время или после первой записи.
	second := make(chan result, 1)
	go func() {
		// value и err передают итог второго чтения. Оно должно дождаться нового набора.
		value, err := owner.Credentials(ctx, a.Account.ID)
		second <- result{value, err}
	}()
	close(release)
	// one и two подтверждают один и тот же принятый полный набор.
	one, two := chatTestReceive(t, ctx, first), chatTestReceive(t, ctx, second)
	if one.err != nil || two.err != nil || exchanges.Load() != 1 ||
		one.value.CredentialRevision != 2 || two.value.CredentialWriteID != one.value.CredentialWriteID ||
		two.value.Bundle.Twitch.RefreshToken != "test-refresh-refreshed" || one.value.Bundle.Twitch.AccessToken != "test-access-refreshed" {
		t.Fatalf("Два чтения не получили единственный подтверждённый обмен, вызовов %d: %v / %v", exchanges.Load(), one.err, two.err)
	}
	// current подтверждает, что завершённый слот a освободился для c, пока b занят.
	current, err := owner.Credentials(ctx, c.Account.ID)
	if err != nil || current.AccountID != c.Account.ID || current.CredentialRevision != 1 {
		t.Fatalf("Слот завершённого аккаунта удерживается: %v", err)
	}
	close(bRelease)
	// neighbor подтверждает независимый неизменённый набор второго владельца.
	neighbor := chatTestReceive(t, ctx, concurrent)
	if neighbor.err != nil || neighbor.value.AccountID != b.Account.ID || neighbor.value.CredentialRevision != 1 {
		t.Fatalf("Обмен первого аккаунта нарушил работу соседнего владельца: %v", neighbor.err)
	}
	chatTestOK(t, owner.Close(ctx))
	chatTestReceive(t, ctx, owner.Done())
	if _, err = owner.Credentials(ctx, a.Account.ID); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("Завершённый владелец принял новую операцию: %v", err)
	}
}

// credentialReceiptTestStore управляет отказом ответа после настоящего COMMIT в SQLite.
type credentialReceiptTestStore struct {
	// Service предоставляет обычное чтение и CAS реальной базы.
	*core.Service
	// unreadable блокирует чтение после первой попытки записи до снятия отказа в тесте.
	unreadable bool
	// editAfterCommit изменяет metadata между COMMIT и его сверкой.
	editAfterCommit bool
	// writes считает обращения Owner к сохранению после обмена.
	writes int
}

// GetChatCredentialResult читает результат id в контексте ctx после снятия имитируемого отказа.
func (s *credentialReceiptTestStore) GetChatCredentialResult(ctx context.Context, id core.ID) (core.ChatCredentialResult, error) {
	if s.unreadable && s.writes != 0 {
		return core.ChatCredentialResult{}, core.ErrBusy
	}
	return s.Service.GetChatCredentialResult(ctx, id)
}

// RefreshChatCredentials сохраняет bundle в SQLite и имитирует потерю ответа на COMMIT.
// id, provider, writeID и expected определяют точный CAS. ctx ограничивает реальную запись.
func (s *credentialReceiptTestStore) RefreshChatCredentials(ctx context.Context, id core.ID, provider core.ChatProvider, bundle core.ChatCredentialBundle, writeID core.ID, expected uint64) (core.ChatCredentialResult, error) {
	s.writes++
	// written подтверждает реальный результат записи, err сообщает отказ записи либо контрольного rename.
	written, err := s.Service.RefreshChatCredentials(ctx, id, provider, bundle, writeID, expected)
	if err != nil {
		return written, err
	}
	if s.editAfterCommit {
		// next сохраняет новое имя без изменения уже записанного CredentialWriteID.
		next := written.Account
		next.Name = "Новое имя"
		if _, err = s.UpdateChatAccount(ctx, next, next.Revision); err != nil {
			return written, err
		}
	}
	return written, context.DeadlineExceeded
}

// ReplaceChatCredentials выполняет ручной CAS в реальном SQLite и теряет ответ о подтверждении.
// Параметры ctx, id, provider, bundle, writeID и expected соответствуют первоначальной операции.
func (s *credentialReceiptTestStore) ReplaceChatCredentials(ctx context.Context, id core.ID, provider core.ChatProvider, bundle core.ChatCredentialBundle, writeID core.ID, expected uint64) (core.ChatCredentialResult, error) {
	s.writes++
	// written содержит ожидаемый подтверждённый результат, err — настоящий отказ CAS.
	written, err := s.Service.ReplaceChatCredentials(ctx, id, provider, bundle, writeID, expected)
	if err != nil {
		return written, err
	}
	return written, context.DeadlineExceeded
}

// TestChatCredentialOwnerReconcilesEntireReceiptWithoutRepeatingExchange проверяет потерю ответа
// после сохранения, недоступность чтения и совпавший WriteID при несовпавшей конфигурации.
// t сверяет полный результат SQLite независимо от статуса ответа.
func TestChatCredentialOwnerReconcilesEntireReceiptWithoutRepeatingExchange(t *testing.T) {
	// variants задаёт оба исхода сверки одного и того же WriteID.
	variants := []struct {
		// name различает сценарии. edited означает, что после COMMIT изменилась конфигурация.
		name   string
		edited bool
	}{{"совпал полный результат", false}, {"совпал только WriteID и набор", true}}
	// variant выбирает наблюдаемый исход подтверждения записи.
	for _, variant := range variants {
		// t изолирует реальные записи и отказ чтения одного сценария.
		t.Run(variant.name, func(t *testing.T) {
			// ctx ограничивает срок операций временной базы.
			ctx := t.Context()
			// service обслуживает штатные CAS обеих областей.
			_, service := openChatTestStore(t, ctx)
			// initial содержит неизменившуюся начальную конфигурацию и один исходный токен.
			initial := createCredentialTestAccount(t, ctx, service, "initial")
			// storage и exchanges моделируют неопределённый исход первого обмена.
			storage := &credentialReceiptTestStore{Service: service, unreadable: true, editAfterCommit: variant.edited}
			var exchanges int
			// owner выполняет ровно одну попытку внешней замены.
			owner, err := NewCredentialOwner(storage, core.DefaultChatLimits(), func(context.Context, core.ChatCredentials) (CredentialRefreshResult, error) {
				// Обмен возвращает полный новый набор и объявляет прежний токен израсходованным.
				exchanges++
				// bundle содержит новый полный набор при однократной ротации.
				bundle := credentialTestBundle("rotated")
				return CredentialRefreshResult{Bundle: &bundle, PreviousMayBeConsumed: true}, nil
			})
			chatTestOK(t, err)
			defer func() {
				// Закрывает владельца до закрытия Store на обоих выходах проверки.
				chatTestOK(t, owner.Close(context.Background()))
			}()
			if _, err = owner.Credentials(ctx, initial.Account.ID); !errors.Is(err, ErrCredentialCommitUnknown) {
				t.Fatalf("Недоступная сверка сразу после COMMIT: %v", err)
			}
			if _, err = owner.Credentials(ctx, initial.Account.ID); !errors.Is(err, ErrCredentialCommitUnknown) {
				t.Fatalf("Неопределённая запись перестала блокировать повторный обмен: %v", err)
			}
			storage.unreadable = false
			// value содержит результат третьего вызова, который сначала сверяет прежнюю запись.
			value, err := owner.Credentials(ctx, initial.Account.ID)
			if variant.edited {
				if !errors.Is(err, core.ErrStale) {
					t.Fatalf("Один и тот же WriteID при изменённой ревизии конфигурации подтвердил запись: %v", err)
				}
			} else if err != nil || value.CredentialRevision != 2 || value.Bundle.Twitch.RefreshToken != "test-refresh-rotated" {
				t.Fatalf("Подтверждённый полный результат не возвращён: %v", err)
			}
			if exchanges != 1 || storage.writes != 1 {
				t.Fatalf("При сверке повторён обмен или CAS: %d / %d", exchanges, storage.writes)
			}
			// actual подтверждает принятую пару токенов независимо от ответа об успехе.
			actual, err := service.GetChatCredentialResult(ctx, initial.Account.ID)
			if err != nil || actual.Credentials.CredentialRevision != 2 || actual.Credentials.Bundle.Twitch.AccessToken != "test-access-rotated" ||
				actual.Credentials.Bundle.Twitch.RefreshToken != "test-refresh-rotated" || actual.Credentials.CredentialWriteID == initial.Credentials.CredentialWriteID {
				t.Fatalf("Сохранён неполный либо повторный обмен: %v", err)
			}
		})
	}
}

// TestChatCredentialOwnerResumesOnlyIdenticalReplacement проверяет применение ранее подтверждённой
// ручной замены. t различает повтор всей операции и повтор WriteID с другим набором.
func TestChatCredentialOwnerResumesOnlyIdenticalReplacement(t *testing.T) {
	// changed выбирает совпадающий или изменённый набор реквизитов повторной замены.
	for _, changed := range []bool{false, true} {
		// name указывает случай, в котором ожидается отказ повторного обращения.
		name := "тот же набор"
		if changed {
			name = "другой набор"
		}
		// t даёт отдельную SQLite каждой попытке потери ответа.
		t.Run(name, func(t *testing.T) {
			// ctx ограничивает чтение и изменение тестового аккаунта.
			ctx := t.Context()
			// service предоставляет реальное сохранение обеих ревизий.
			_, service := openChatTestStore(t, ctx)
			// account хранит исходные версии, id адресует изменяемый аккаунт.
			account := createCredentialTestAccount(t, ctx, service, "replacement")
			id := account.Account.ID
			// store имитирует потерю ответа после успешной ручной замены.
			store := &credentialReceiptTestStore{Service: service, unreadable: true}
			// owner сверяет возможный COMMIT. err содержит результат каждой последующей операции.
			owner, err := NewCredentialOwner(store, core.DefaultChatLimits(), nil)
			chatTestOK(t, err)
			defer func() {
				// Закрывает Owner перед освобождением SQLite.
				chatTestOK(t, owner.Close(context.Background()))
			}()
			// replacement задаёт принятый новый набор, retry содержит набор из повторного обращения.
			replacement := credentialTestBundle("replacement-accepted")
			retry := replacement
			if changed {
				retry = credentialTestBundle("replacement-conflict")
			}
			if _, err := owner.Replace(ctx, credentialTestMutator{}, id, core.ChatTwitch, replacement, "single-write", 1); !errors.Is(err, ErrCredentialCommitUnknown) {
				t.Fatalf("Неопределённый исход ручного CAS: %v", err)
			}
			store.unreadable = false
			// result должен подтвердить ранее принятый CAS только при полном совпадении повторного набора.
			result, err := owner.Replace(ctx, credentialTestMutator{}, id, core.ChatTwitch, retry, "single-write", 1)
			if changed {
				if !errors.Is(err, core.ErrConflict) {
					t.Fatalf("Другой набор ошибочно принят под прежним WriteID: %v", err)
				}
			} else if err != nil || result.Revision != 2 {
				t.Fatalf("Повтор идентичной замены не подтвердил её результат: %+v, %v", result, err)
			}
			// stored подтверждает неизменившийся итог единственной записи после повторного обращения.
			stored, err := service.GetChatCredentialResult(ctx, id)
			if err != nil || store.writes != 1 || stored.Account.Revision != 2 || stored.Credentials.CredentialRevision != 2 ||
				stored.Credentials.Bundle.Twitch.RefreshToken != "test-refresh-replacement-accepted" {
				t.Fatalf("Повторное обращение изменило сохранённую замену: записей %d, %v", store.writes, err)
			}
		})
	}
}

// TestChatCredentialOwnerReplacementRecoversSpentToken проверяет защиту от повторного обмена
// израсходованного токена, отказ устаревшего CAS и принятие подтверждённой замены.
func TestChatCredentialOwnerReplacementRecoversSpentToken(t *testing.T) {
	// ctx управляет изолированными операциями SQLite и CredentialOwner.
	ctx := t.Context()
	// service сохраняет обе ревизии аккаунта в настоящей SQLite.
	_, service := openChatTestStore(t, ctx)
	// initial содержит исходный полный набор.
	initial := createCredentialTestAccount(t, ctx, service, "spent")
	// attempts считает обращения к обмену исходного набора после его возможного потребления.
	var attempts int
	// owner управляет исходным токеном. err сообщает об отказе на очередной границе.
	owner, err := NewCredentialOwner(service, core.DefaultChatLimits(), func(_ context.Context, current core.ChatCredentials) (CredentialRefreshResult, error) {
		// Только исходный набор требует обмена. Его ответ может пропасть после потребления токена.
		if current.CredentialRevision == 1 {
			attempts++
			return CredentialRefreshResult{PreviousMayBeConsumed: true}, errors.New("секрет-ответа-площадки")
		}
		return CredentialRefreshResult{}, nil
	})
	chatTestOK(t, err)
	defer func() {
		// Завершает CredentialOwner до закрытия базы.
		chatTestOK(t, owner.Close(context.Background()))
	}()
	// id адресует аккаунт с потраченным токеном, replacement представляет заново проверенный набор.
	id := initial.Account.ID
	replacement := credentialTestBundle("manual")
	if _, err = owner.Credentials(ctx, id); !errors.Is(err, ErrCredentialExchangeFailed) || strings.Contains(err.Error(), "секрет-ответа-площадки") {
		t.Fatalf("Отказ обмена не санитизирован: %v", err)
	}
	if _, err = owner.Credentials(ctx, id); !errors.Is(err, ErrCredentialRefreshConsumed) || attempts != 1 {
		t.Fatalf("Израсходованный токен повторно допущен: попыток %d, %v", attempts, err)
	}
	// current хранит переименованный аккаунт. Старый config CAS после rename должен проиграть.
	current := initial.Account
	current.Name = "Переименованный аккаунт"
	current, err = service.UpdateChatAccount(ctx, current, current.Revision)
	chatTestOK(t, err)
	if _, err = owner.Replace(ctx, credentialTestMutator{}, id, core.ChatTwitch, replacement, "stale-write", initial.Account.Revision); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("Старый CAS ручной замены: %v", err)
	}
	if _, err = owner.Credentials(ctx, id); !errors.Is(err, ErrCredentialRefreshConsumed) || attempts != 1 {
		t.Fatalf("Отказ CAS снял отметку об израсходованном наборе: %v", err)
	}
	// applied подтверждает ручную замену поверх актуальной ревизии.
	applied, err := owner.Replace(ctx, credentialTestMutator{}, id, core.ChatTwitch, replacement, "new-write", current.Revision)
	if err != nil || applied.Revision != current.Revision+1 {
		t.Fatalf("Актуальная замена не принята: %+v, %v", applied, err)
	}
	// final содержит используемый теперь полный набор, отдельно от metadata.
	final, err := owner.Credentials(ctx, id)
	if err != nil || final.CredentialRevision != 2 || final.CredentialWriteID != "new-write" || final.Bundle.Twitch.RefreshToken != "test-refresh-manual" || attempts != 1 {
		t.Fatalf("Подтверждённая замена не сняла запрет старого токена: %v", err)
	}
}

// TestChatCredentialOwnerCloseCancelsAndWaitsForExchange отделяет запрос остановки
// от освобождения ресурса адаптером. t проверяет, что Done не объявляет ложное завершение.
func TestChatCredentialOwnerCloseCancelsAndWaitsForExchange(t *testing.T) {
	// ctx и cancel ограничивают проверку физического выхода управляемого обмена.
	ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
	defer cancel()
	// service предоставляет штатные операции области полномочий в отдельной базе.
	_, service := openChatTestStore(t, ctx)
	// account предоставляет токен, который адаптер проверяет при закрытии.
	account := createCredentialTestAccount(t, ctx, service, "close")
	// started и cancelled подтверждают получение запроса и его отмену. release отделяет отмену
	// от окончания очистки внешней операции, а completed принимает её результат.
	started := make(chan struct{})
	cancelled := make(chan struct{})
	release := make(chan struct{}, 1)
	completed := make(chan error, 1)
	// owner допускает единственный обмен. err сообщает об отказах его создания и остановки.
	owner, err := NewCredentialOwner(service, core.DefaultChatLimits(), func(ctx context.Context, _ core.ChatCredentials) (CredentialRefreshResult, error) {
		// Имитация адаптера завершает отменяемую операцию после наблюдаемой очистки ресурса.
		close(started)
		<-ctx.Done()
		close(cancelled)
		<-release
		return CredentialRefreshResult{}, ctx.Err()
	})
	chatTestOK(t, err)
	defer func() {
		// Освобождает барьер и ожидает завершение владельца даже при раннем отказе теста.
		select {
		case release <- struct{}{}:
		default:
		}
		chatTestOK(t, owner.Close(context.Background()))
	}()
	go func() {
		// err сохраняет отказ отменённой операции после фактического выхода адаптера.
		_, err := owner.Credentials(ctx, account.Account.ID)
		completed <- err
	}()
	chatTestReceive(t, ctx, started)
	// deadline и stop ограничивают только первый вызов ожидания Close.
	deadline, stop := context.WithTimeout(ctx, 25*time.Millisecond)
	defer stop()
	if err = owner.Close(deadline); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Close подтвердил остановку до выхода адаптера: %v", err)
	}
	chatTestReceive(t, ctx, cancelled)
	select {
	case <-owner.Done():
		t.Fatal("Done закрыт до освобождения ресурса обмена")
	default:
	}
	release <- struct{}{}
	if err = chatTestReceive(t, ctx, completed); !errors.Is(err, context.Canceled) {
		t.Fatalf("Обмен не получил отмену владельца: %v", err)
	}
	chatTestReceive(t, ctx, owner.Done())
	chatTestOK(t, owner.Close(ctx))
}
