package storage

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// chatAcceptanceBundle возвращает полный синтетический набор provider с различимым содержимым token.
// Истёкшее время проверяет читаемость сохранённых токенов независимо от текущих часов.
func chatAcceptanceBundle(provider core.ChatProvider, token string) core.ChatCredentialBundle {
	switch provider {
	case core.ChatYouTube:
		return core.ChatCredentialBundle{YouTube: &core.YouTubeChatCredentials{APIKey: token}}
	case core.ChatTwitch:
		return core.ChatCredentialBundle{Twitch: &core.TwitchChatCredentials{ClientID: "synthetic-client", UserID: "123", AccessToken: "access-" + token, RefreshToken: "refresh-" + token, Scopes: []string{"user:read:chat"}, ExpiresAt: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)}}
	default:
		return core.ChatCredentialBundle{VKVideoLive: &core.VKVideoLiveChatCredentials{ClientID: "synthetic-client", ClientSecret: "secret-" + token, AccessToken: "access-" + token, ExpiresAt: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)}}
	}
}

// chatAcceptanceAccount создаёт через svc аккаунт provider с синтетическими реквизитами.
// t останавливает сценарий при отказе и проверяет обе начальные ревизии и отдельное чтение.
func chatAcceptanceAccount(t *testing.T, svc *core.Service, provider core.ChatProvider) core.ChatCredentialResult {
	t.Helper()
	// result содержит обе области новой записи; err сообщает отказ атомарного создания.
	result, err := svc.CreateChatAccount(context.Background(), core.ChatAccount{Provider: provider, Name: "Начальное имя"}, chatAcceptanceBundle(provider, "initial"), "initial-write")
	if err != nil || result.Account.Revision != 1 || result.Credentials.CredentialRevision != 1 || result.Account.ID != result.Credentials.AccountID {
		t.Fatalf("Создание аккаунта: %+v, %v", result.Account, err)
	}
	// stored проверяет совпадение полного результата после реального COMMIT.
	stored, err := svc.GetChatCredentialResult(context.Background(), result.Account.ID)
	if err != nil || !stored.Matches(result) {
		t.Fatalf("Создание сохранило неполный результат: %v", err)
	}
	return result
}

// chatAcceptanceSource создаёт Source для destination/account; t сообщает об ошибках.
// Постоянный канал выбирается по provider, независимо от свободного Destination.Platform.
func chatAcceptanceSource(t *testing.T, svc *core.Service, destination core.ID, account core.ChatAccount) core.ChatSource {
	t.Helper()
	// options содержит явную закрытую форму; err сообщает ошибку конструктора либо записи.
	options, err := core.NewChatOptions(account.Provider)
	if err != nil {
		t.Fatal(err)
	}
	// hint представляет постоянную ссылку выбранной платформы.
	hint := "UCchannel"
	switch account.Provider {
	case core.ChatTwitch:
		hint = ""
	case core.ChatVKVideoLive:
		hint = "channel"
	}
	// source содержит результат создания с обязательными связями и явным Enabled.
	source, err := svc.CreateChatSource(context.Background(), core.ChatSource{DestinationID: destination, AccountID: account.ID, Provider: account.Provider, ChannelHint: hint, Options: options, Enabled: true})
	if err != nil || source.Revision != 1 {
		t.Fatalf("Создание Source: %+v, %v", source, err)
	}
	return source
}

// TestChatAccountVersionsSeparateMetadataRefreshAndReconnect проверяет два независимых CAS.
// t сверяет сохранённый результат после отказов, rename, refresh и ручной замены.
func TestChatAccountVersionsSeparateMetadataRefreshAndReconnect(t *testing.T) {
	// store и svc обслуживают только временную SQLite; err сообщает отказ создания сервиса.
	store := openTestStore(t)
	svc, err := core.NewService(store, store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	// ctx ограничивает операции теста; initial фиксирует обе исходные области.
	ctx := context.Background()
	initial := chatAcceptanceAccount(t, svc, core.ChatTwitch)
	// renamed передаёт только новое имя, сохраняя Provider.
	renamed := initial.Account
	renamed.Name = ""
	renamed, err = svc.UpdateChatAccount(ctx, renamed, 1)
	if err != nil || renamed.Revision != 2 || !renamed.CreatedAt.Equal(initial.Account.CreatedAt) {
		t.Fatalf("Rename не сохранил Record: %+v, %v", renamed, err)
	}
	// credentials должны остаться побайтно тем же полным набором после metadata edit.
	credentials, err := svc.GetChatCredentials(ctx, renamed.ID)
	if err != nil || !reflect.DeepEqual(credentials, initial.Credentials) {
		t.Fatalf("Rename изменил полномочия: %v", err)
	}
	// refreshed использует исходную credential revision, хотя config revision уже изменилась.
	refreshed, err := svc.RefreshChatCredentials(ctx, renamed.ID, core.ChatTwitch, chatAcceptanceBundle(core.ChatTwitch, "refreshed"), "refresh-write", 1)
	if err != nil || !reflect.DeepEqual(refreshed.Account, renamed) || refreshed.Credentials.CredentialRevision != 2 {
		t.Fatalf("Refresh изменил конфигурацию или зависит от её версии: %v", err)
	}
	// before сохраняет весь граф перед каждым отклонённым CAS, включая обе области аккаунта.
	before := captureFormatFiveSQLRows(t, store.sqlDB)
	_, err = svc.ReplaceChatCredentials(ctx, renamed.ID, core.ChatTwitch, chatAcceptanceBundle(core.ChatTwitch, "stale"), "stale-write", 1)
	requireDomainError(t, err, core.ErrConflict)
	if !reflect.DeepEqual(before, captureFormatFiveSQLRows(t, store.sqlDB)) {
		t.Fatal("Устаревшая replacement изменила сохранённый граф")
	}
	// replacement принимает актуальную credential revision внутри Write при прежней config revision.
	replacement, err := svc.ReplaceChatCredentials(ctx, renamed.ID, core.ChatTwitch, chatAcceptanceBundle(core.ChatTwitch, "replacement"), "replacement-write", 2)
	if err != nil || replacement.Account.Revision != 3 || replacement.Credentials.CredentialRevision != 3 || replacement.Account.Name != renamed.Name || !reflect.DeepEqual(replacement.Credentials.Bundle, chatAcceptanceBundle(core.ChatTwitch, "replacement")) {
		t.Fatalf("Reconnect не заменил обе области атомарно: %v", err)
	}
	before = captureFormatFiveSQLRows(t, store.sqlDB)
	_, err = svc.RefreshChatCredentials(ctx, renamed.ID, core.ChatTwitch, chatAcceptanceBundle(core.ChatTwitch, "late"), "late-write", 2)
	requireDomainError(t, err, core.ErrStale)
	if !reflect.DeepEqual(before, captureFormatFiveSQLRows(t, store.sqlDB)) {
		t.Fatal("Устаревший refresh изменил сохранённый граф")
	}
	before = captureFormatFiveSQLRows(t, store.sqlDB)
	_, err = svc.ReplaceChatCredentials(ctx, renamed.ID, core.ChatTwitch, chatAcceptanceBundle(core.ChatTwitch, "second"), "second-write", 2)
	requireDomainError(t, err, core.ErrConflict)
	if !reflect.DeepEqual(before, captureFormatFiveSQLRows(t, store.sqlDB)) {
		t.Fatal("Повторная replacement изменила сохранённый граф")
	}
	before = captureFormatFiveSQLRows(t, store.sqlDB)
	requireDomainError(t, svc.DeleteChatAccount(ctx, renamed.ID, 2), core.ErrConflict)
	if !reflect.DeepEqual(before, captureFormatFiveSQLRows(t, store.sqlDB)) {
		t.Fatal("Устаревший DELETE Account изменил сохранённый граф")
	}
	// stored должен совпадать с единственной принятой заменой после всех stale-операций.
	stored, err := svc.GetChatCredentialResult(ctx, renamed.ID)
	if err != nil || !stored.Matches(replacement) {
		t.Fatalf("Отказ CAS изменил сохранённые данные: %v", err)
	}
	// canceled проверяет отмену до принятия сохранения; cancel немедленно завершает контекст.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = svc.RefreshChatCredentials(canceled, renamed.ID, core.ChatTwitch, chatAcceptanceBundle(core.ChatTwitch, "cancelled"), "cancelled-write", 3)
	requireDomainError(t, err, context.Canceled)
	stored, err = svc.GetChatCredentialResult(ctx, renamed.ID)
	if err != nil || !stored.Matches(replacement) {
		t.Fatalf("Отмена отозвала действующие полномочия: %v", err)
	}
	requireDomainError(t, svc.DeleteChatAccount(ctx, renamed.ID, 3), nil)
	_, err = svc.GetChatAccount(ctx, renamed.ID)
	requireDomainError(t, err, core.ErrNotFound)
	_, err = svc.GetChatCredentials(ctx, renamed.ID)
	requireDomainError(t, err, core.ErrNotFound)
}

// TestChatConcurrentEditsKeepBothScopesAndChooseOneReplacement проверяет реальные конкурентные Write.
// t ожидает успех rename и refresh, затем ровно одну replacement при общей config revision.
func TestChatConcurrentEditsKeepBothScopesAndChooseOneReplacement(t *testing.T) {
	// store и svc обслуживают независимую файловую SQLite; err сообщает отказ создания сервиса.
	store := openTestStore(t)
	svc, err := core.NewService(store, store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	// initial задаёт обе версии; ctx и cancel ограничивают завершение всех тестовых горутин.
	initial := chatAcceptanceAccount(t, svc, core.ChatTwitch)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// start одновременно допускает две операции, results собирает их окончательные ошибки.
	start := make(chan struct{})
	results := make(chan error, 2)
	go func() {
		// Callback ждёт общий старт и меняет только имя при исходной config revision.
		<-start
		// value содержит локальную копию метаданных, writeErr — результат CAS.
		value := initial.Account
		value.Name = "Конкурентное имя"
		_, writeErr := svc.UpdateChatAccount(ctx, value, 1)
		results <- writeErr
	}()
	go func() {
		// Callback ждёт общий старт и сохраняет полный обмен по исходной credential revision.
		<-start
		// writeErr содержит итог независимого credential CAS.
		_, writeErr := svc.RefreshChatCredentials(ctx, initial.Account.ID, core.ChatTwitch, chatAcceptanceBundle(core.ChatTwitch, "concurrent"), "concurrent-refresh", 1)
		results <- writeErr
	}()
	close(start)
	for range 2 {
		// writeErr передаёт результат одной завершённой операции без гонки на тестовом объекте.
		select {
		case writeErr := <-results:
			if writeErr != nil {
				t.Fatal(writeErr)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	// stored должен содержать оба независимо сохранённых изменения.
	stored, err := svc.GetChatCredentialResult(ctx, initial.Account.ID)
	if err != nil || stored.Account.Name != "Конкурентное имя" || stored.Account.Revision != 2 || stored.Credentials.CredentialRevision != 2 || !reflect.DeepEqual(stored.Credentials.Bundle, chatAcceptanceBundle(core.ChatTwitch, "concurrent")) {
		t.Fatalf("Конкурентная запись потеряла одну из областей: %v", err)
	}
	start = make(chan struct{})
	// token различает полные результаты конкурирующих replacement; callback не выполняет внешний обмен.
	for _, token := range []string{"first", "second"} {
		go func() {
			// Callback сохраняет только заранее подготовленный полный набор и передаёт ошибку.
			<-start
			// writeErr сообщает, выиграла ли эта замена общий config CAS.
			_, writeErr := svc.ReplaceChatCredentials(ctx, initial.Account.ID, core.ChatTwitch, chatAcceptanceBundle(core.ChatTwitch, token), core.ID(token), 2)
			results <- writeErr
		}()
	}
	close(start)
	// successes и conflicts считают два допустимых исхода конкуренции при одном expected.
	successes, conflicts := 0, 0
	for range 2 {
		select {
		// writeErr сохраняет результат одной replacement, не скрывая неожиданные причины.
		case writeErr := <-results:
			if writeErr == nil {
				successes++
			} else if errors.Is(writeErr, core.ErrConflict) {
				conflicts++
			} else {
				t.Fatal(writeErr)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	stored, err = svc.GetChatCredentialResult(ctx, initial.Account.ID)
	if err != nil || successes != 1 || conflicts != 1 || stored.Account.Revision != 3 || stored.Credentials.CredentialRevision != 3 {
		t.Fatalf("Один CAS принял несколько замен: success=%d conflict=%d, %v", successes, conflicts, err)
	}
	if !reflect.DeepEqual(stored.Credentials.Bundle, chatAcceptanceBundle(core.ChatTwitch, string(stored.Credentials.CredentialWriteID))) {
		t.Fatal("Одновременные replacement смешали token pair и квитанцию")
	}
}

// TestChatSourceCRUDKeepsParentsAndSwitchesProviderAtomically проверяет обязательные ссылки и CAS.
// t различает отсутствие родителей, несовместимую платформу и попытку переноса Source.
func TestChatSourceCRUDKeepsParentsAndSwitchesProviderAtomically(t *testing.T) {
	// fixture задаёт действительные Destination с произвольным Platform="custom".
	fixture := newDomainFixture(t, 2)
	// ctx задаёт операции в изолированной БД; youtube и twitch дают разные допустимые аккаунты.
	ctx := context.Background()
	youtube := chatAcceptanceAccount(t, fixture.service, core.ChatYouTube)
	twitch := chatAcceptanceAccount(t, fixture.service, core.ChatTwitch)
	// source сохраняет неизменного родителя и начальные времена для всех следующих проверок.
	source := chatAcceptanceSource(t, fixture.service, fixture.destinations[0].ID, youtube.Account)
	// duplicate подтверждает отсутствие глобальной уникальности пары Destination/provider/channel.
	duplicate := chatAcceptanceSource(t, fixture.service, fixture.destinations[0].ID, youtube.Account)
	if duplicate.ID == source.ID {
		t.Fatal("Повторная конфигурация не получила новую идентичность")
	}
	// invalid хранит локальную попытку изменения; err сохраняет доменный отказ.
	invalid := source
	invalid.DestinationID = "missing-destination"
	_, err := fixture.service.CreateChatSource(ctx, invalid)
	requireDomainError(t, err, core.ErrNotFound)
	invalid = source
	invalid.AccountID = "missing-account"
	_, err = fixture.service.CreateChatSource(ctx, invalid)
	requireDomainError(t, err, core.ErrNotFound)
	invalid.AccountID = twitch.Account.ID
	_, err = fixture.service.CreateChatSource(ctx, invalid)
	requireDomainError(t, err, core.ErrInvalid)
	invalid = source
	invalid.DestinationID = fixture.destinations[1].ID
	_, err = fixture.service.UpdateChatSource(ctx, invalid, 1)
	requireDomainError(t, err, core.ErrConflict)
	// options задаёт согласованную новую платформу; updated заменяет весь связанный набор и false.
	options, err := core.NewChatOptions(core.ChatTwitch)
	if err != nil {
		t.Fatal(err)
	}
	updated := source
	updated.AccountID, updated.Provider, updated.ChannelHint, updated.Options, updated.Enabled = twitch.Account.ID, core.ChatTwitch, "", options, false
	updated, err = fixture.service.UpdateChatSource(ctx, updated, 1)
	if err != nil || updated.Revision != 2 || updated.Enabled || updated.ChannelHint != "" || updated.DestinationID != source.DestinationID || !updated.CreatedAt.Equal(source.CreatedAt) {
		t.Fatalf("Замена потеряла согласованные поля: %+v, %v", updated, err)
	}
	// before фиксирует весь граф отдельно для каждого отказа устаревшей операции или FK.
	before := captureFormatFiveSQLRows(t, fixture.store.sqlDB)
	_, err = fixture.service.UpdateChatSource(ctx, source, 1)
	requireDomainError(t, err, core.ErrConflict)
	if !reflect.DeepEqual(before, captureFormatFiveSQLRows(t, fixture.store.sqlDB)) {
		t.Fatal("Устаревший UPDATE Source изменил сохранённый граф")
	}
	before = captureFormatFiveSQLRows(t, fixture.store.sqlDB)
	requireDomainError(t, fixture.service.DeleteChatSource(ctx, source.ID, 1), core.ErrConflict)
	if !reflect.DeepEqual(before, captureFormatFiveSQLRows(t, fixture.store.sqlDB)) {
		t.Fatal("Устаревший DELETE Source изменил сохранённый граф")
	}
	// stored проверяет отсутствие частичной записи после устаревших операций.
	stored, err := fixture.service.GetChatSource(ctx, source.ID)
	if err != nil || !reflect.DeepEqual(stored, updated) {
		t.Fatalf("Отказ Source CAS изменил конфигурацию: %v", err)
	}
	before = captureFormatFiveSQLRows(t, fixture.store.sqlDB)
	requireDomainError(t, fixture.service.DeleteChatAccount(ctx, twitch.Account.ID, 1), core.ErrInUse)
	if !reflect.DeepEqual(before, captureFormatFiveSQLRows(t, fixture.store.sqlDB)) {
		t.Fatal("Запрещённый DELETE используемого Account изменил сохранённый граф")
	}
	// statement пытается обойти сервис; SQLite обязан независимо сохранить обязательные связи.
	for _, statement := range []string{
		"UPDATE chat_sources SET account_id='missing-account' WHERE id=?",
		"UPDATE chat_sources SET destination_id='missing-destination' WHERE id=?",
		"UPDATE chat_sources SET provider='youtube' WHERE id=?",
		"UPDATE chat_sources SET enabled=2 WHERE id=?",
		"UPDATE chat_sources SET account_id=NULL WHERE id=?",
		"UPDATE chat_sources SET destination_id=NULL WHERE id=?",
	} {
		// before сохраняет все строки для проверки атомарности отклонённого SQL.
		before := captureFormatFiveSQLRows(t, fixture.store.sqlDB)
		if _, err = fixture.store.sqlDB.ExecContext(ctx, statement, source.ID); err == nil {
			t.Fatalf("SQLite принял нарушение графа: %s", statement)
		}
		if !reflect.DeepEqual(before, captureFormatFiveSQLRows(t, fixture.store.sqlDB)) {
			t.Fatalf("Отклонённый SQL изменил граф: %s", statement)
		}
	}
	requireDomainError(t, fixture.service.DeleteChatSource(ctx, source.ID, 2), nil)
	requireDomainError(t, fixture.service.DeleteChatAccount(ctx, twitch.Account.ID, 1), nil)
	_, err = fixture.service.GetChatSource(ctx, source.ID)
	requireDomainError(t, err, core.ErrNotFound)
}

// TestChatDestinationCASAndHoldsProtectWholeGraph проверяет атомарность CASCADE и самостоятельность history.
// t использует настоящие удержания из сохранённого запуска; внешние процессы не запускаются.
func TestChatDestinationCASAndHoldsProtectWholeGraph(t *testing.T) {
	// store, svc и state содержат заполненную текущую БД и независимые эталоны Session/Output.
	store, svc, state, _ := openCurrentEncodingState(t)
	// ctx задаёт только локальные операции; outputs находит Destination исторического выхода.
	ctx := context.Background()
	outputs := readOutputs(t, store, state.Receipt.SessionID)
	if len(outputs) != 2 {
		t.Fatal("Не создан эталон самостоятельной истории")
	}
	// destination читает действующую конфигурацию, account и source дополняют граф чата.
	destination, err := svc.GetDestination(ctx, outputs[0].Snapshot.Destination.ID)
	if err != nil {
		t.Fatal(err)
	}
	account := chatAcceptanceAccount(t, svc, core.ChatYouTube)
	source := chatAcceptanceSource(t, svc, destination.ID, account.Account)
	// before включает все доменные строки, квитанции и удержания перед запрещённым DELETE.
	before := captureFormatFiveSQLRows(t, store.sqlDB)
	requireDomainError(t, svc.DeleteDestination(ctx, destination.ID, destination.Revision+1), core.ErrConflict)
	if !reflect.DeepEqual(before, captureFormatFiveSQLRows(t, store.sqlDB)) {
		t.Fatal("Stale DELETE оставил частичный CASCADE")
	}
	before = captureFormatFiveSQLRows(t, store.sqlDB)
	requireDomainError(t, svc.DeleteDestination(ctx, destination.ID, destination.Revision), core.ErrInUse)
	if !reflect.DeepEqual(before, captureFormatFiveSQLRows(t, store.sqlDB)) {
		t.Fatal("Удержание не защитило граф от частичного CASCADE")
	}
	// Callback через r освобождает только тестовые удержания, имитируя завершённую очистку ресурсов.
	if err = store.Write(ctx, func(r core.Repositories) error { return r.Runtime.ReleaseHolds(ctx, state.Receipt.SessionID, "") }); err != nil {
		t.Fatal(err)
	}
	if err = svc.DeleteDestination(ctx, destination.ID, destination.Revision); err != nil {
		t.Fatalf("Разрешённое удаление Destination: %v", err)
	}
	_, err = svc.GetChatSource(ctx, source.ID)
	requireDomainError(t, err, core.ErrNotFound)
	// stored должен сохранить независимо используемый аккаунт после CASCADE его Source.
	stored, err := svc.GetChatCredentialResult(ctx, account.Account.ID)
	if err != nil || !stored.Matches(account) {
		t.Fatalf("CASCADE изменил Account: %v", err)
	}
	assertCurrentEncodingSnapshots(t, svc, state)
	if err = svc.DeleteChatAccount(ctx, account.Account.ID, 1); err != nil {
		t.Fatal(err)
	}
	assertCurrentEncodingSnapshots(t, svc, state)
}

// TestChatTransactionsRollbackAndRepositoryLifetime проверяет реальные откаты и область доступа.
// t требует сохранности обеих областей после отказа callback и запрещает доступ вышедшему репозиторию.
func TestChatTransactionsRollbackAndRepositoryLifetime(t *testing.T) {
	// store и svc предоставляют только временный владелец БД; err сообщает отказ сервиса.
	store := openTestStore(t)
	svc, err := core.NewService(store, store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	// ctx адресует локальные операции, initial — неизменяемый эталон полного аккаунта.
	ctx := context.Background()
	initial := chatAcceptanceAccount(t, svc, core.ChatTwitch)
	// escaped должен потерять право доступа после возврата View.
	var escaped core.ChatRepository
	// before включает все строки перед запрещённой записью через View и откатываемой Write.
	before := captureFormatFiveSQLRows(t, store.sqlDB)
	err = store.View(ctx, func(r core.Repositories) error {
		// Callback сохраняет r.Chat и пробует запись через согласованное чтение r.
		escaped = r.Chat
		return r.Chat.DeleteChatAccount(ctx, initial.Account.ID, 1)
	})
	requireDomainError(t, err, core.ErrInvalid)
	if !reflect.DeepEqual(before, captureFormatFiveSQLRows(t, store.sqlDB)) {
		t.Fatal("Запись через View изменила сохранённый граф")
	}
	_, err = escaped.ChatAccount(ctx, initial.Account.ID)
	requireDomainError(t, err, core.ErrClosed)
	// failure отличает контролируемый отказ от SQLite-ошибки и не маскирует её.
	failure := errors.New("синтетический отказ после двух областей записи")
	before = captureFormatFiveSQLRows(t, store.sqlDB)
	err = store.Write(ctx, func(r core.Repositories) error {
		// Callback через r сначала меняет config, затем credentials и намеренно откатывает обе записи.
		// account содержит следующую конфигурацию; credentials — независимый полный набор.
		account := initial.Account
		credentials := initial.Credentials
		account.Revision, account.Name = 2, "Не принятое имя"
		credentials.CredentialRevision, credentials.CredentialWriteID, credentials.Bundle = 2, "rolled-back", chatAcceptanceBundle(core.ChatTwitch, "rolled-back")
		// writeErr сохраняет настоящую ошибку первого либо второго репозитория.
		if writeErr := r.Chat.UpdateChatAccount(ctx, account, 1); writeErr != nil {
			return writeErr
		}
		if writeErr := r.Chat.RefreshChatCredentials(ctx, credentials, 1); writeErr != nil {
			return writeErr
		}
		return failure
	})
	requireDomainError(t, err, failure)
	if !reflect.DeepEqual(before, captureFormatFiveSQLRows(t, store.sqlDB)) {
		t.Fatal("ROLLBACK оставил частично сохранённый граф")
	}
	// stored должен содержать весь первоначальный набор после реального ROLLBACK.
	stored, err := svc.GetChatCredentialResult(ctx, initial.Account.ID)
	if err != nil || !stored.Matches(initial) || store.sqlDB.Stats().InUse != 0 {
		t.Fatalf("ROLLBACK потерял исходные данные или соединение: %v", err)
	}
}

// chatCommitErrorUnitOfWork моделирует потерю подтверждения после настоящего сохранения SQLite.
// Это проверка серверной сверки результата, а не симуляция сетевого refresh или драйверного COMMIT.
type chatCommitErrorUnitOfWork struct {
	// UnitOfWork выполняет настоящие транзакции и предоставляет обычный View для сверки.
	core.UnitOfWork
	// failure возвращается только после успешного COMMIT без повторного callback.
	failure error
	// calls считает попытки сохранения в последовательном сценарии.
	calls int
}

// Write выполняет callback один раз через реальный UnitOfWork и скрывает успешное подтверждение.
// ctx ограничивает транзакцию; настоящая ошибка записи сохраняется без подмены.
func (u *chatCommitErrorUnitOfWork) Write(ctx context.Context, callback func(core.Repositories) error) error {
	u.calls++
	// err сохраняет настоящую ошибку, если COMMIT не подтвердился до синтетического отказа.
	if err := u.UnitOfWork.Write(ctx, callback); err != nil {
		return err
	}
	return u.failure
}

// TestChatCredentialResultResolvesLostCommitAcknowledgement проверяет сверку полного результата.
// t требует сохранения WriteID и обеих ревизий; совпадение одной квитанции не подтверждает иной набор.
func TestChatCredentialResultResolvesLostCommitAcknowledgement(t *testing.T) {
	// store содержит настоящую SQLite; uncertain возвращает ошибку после успешной записи.
	store := openTestStore(t)
	uncertain := &chatCommitErrorUnitOfWork{UnitOfWork: store, failure: errors.New("потеря подтверждения записи")}
	// svc выполняет операции через контролируемую ошибку; err сообщает отказ создания сервиса.
	svc, err := core.NewService(uncertain, store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	// expected остаётся доступным при ошибке, чтобы owner мог сверить факт записи.
	expected, err := svc.CreateChatAccount(context.Background(), core.ChatAccount{Provider: core.ChatYouTube}, chatAcceptanceBundle(core.ChatYouTube, "synthetic-key"), "durable-write")
	requireDomainError(t, err, uncertain.failure)
	// stored перечитывает обе области одним snapshot, не повторяя внешнюю авторизацию.
	stored, err := svc.GetChatCredentialResult(context.Background(), expected.Account.ID)
	if err != nil || !stored.Matches(expected) || uncertain.calls != 1 {
		t.Fatalf("Не удалось подтвердить сохранённый результат: calls=%d, %v", uncertain.calls, err)
	}
	// changed моделирует тот же WriteID при другом полном результате.
	changed := expected
	changed.Credentials.Bundle = chatAcceptanceBundle(core.ChatYouTube, "different-key")
	if stored.Matches(changed) {
		t.Fatal("Один WriteID ошибочно подтвердил другой полный набор")
	}
	changed = expected
	changed.Account.Revision++
	if stored.Matches(changed) {
		t.Fatal("Сверка проигнорировала config revision")
	}
	changed = expected
	changed.Credentials.CredentialRevision++
	if stored.Matches(changed) {
		t.Fatal("Сверка проигнорировала credential revision")
	}
	expected, err = svc.ReplaceChatCredentials(context.Background(), expected.Account.ID, core.ChatYouTube, chatAcceptanceBundle(core.ChatYouTube, "replacement"), "replacement-write", 1)
	requireDomainError(t, err, uncertain.failure)
	stored, err = svc.GetChatCredentialResult(context.Background(), expected.Account.ID)
	if err != nil || !stored.Matches(expected) || uncertain.calls != 2 {
		t.Fatalf("Не подтверждена replacement после ошибки: %v", err)
	}
	expected, err = svc.RefreshChatCredentials(context.Background(), expected.Account.ID, core.ChatYouTube, chatAcceptanceBundle(core.ChatYouTube, "replacement"), "refresh-write", 2)
	requireDomainError(t, err, uncertain.failure)
	stored, err = svc.GetChatCredentialResult(context.Background(), expected.Account.ID)
	if err != nil || !stored.Matches(expected) || uncertain.calls != 3 {
		t.Fatalf("Не подтверждён refresh после ошибки: %v", err)
	}
}

// TestChatRefreshCannotReplaceAccountIdentity проверяет, что refresh не обходит config CAS reconnect.
// t требует отказа смены идентичности при сохранении прежнего набора и обеих ревизий.
func TestChatRefreshCannotReplaceAccountIdentity(t *testing.T) {
	// provider выбирает платформу для отдельной временной БД и её закрытого набора.
	for _, provider := range []core.ChatProvider{core.ChatYouTube, core.ChatTwitch, core.ChatVKVideoLive} {
		// Подтест через t проверяет свой набор identity-полей без влияния соседних провайдеров.
		t.Run(string(provider), func(t *testing.T) {
			// store и svc обслуживают одну SQLite; err сообщает отказ создания сервиса.
			store := openTestStore(t)
			svc, err := core.NewService(store, store.Incarnation())
			if err != nil {
				t.Fatal(err)
			}
			// initial сохраняет обе исходные области до запрещённой смены identity.
			initial := chatAcceptanceAccount(t, svc, provider)
			// field выбирает каждое неизменное поле доступа в соответствующем полном наборе.
			for field := range 2 {
				// changed создаёт новый набор, не меняя указатели из initial.
				changed := chatAcceptanceBundle(provider, "initial")
				switch provider {
				case core.ChatYouTube:
					changed.YouTube.APIKey = "other-key"
				case core.ChatTwitch:
					if field == 0 {
						changed.Twitch.ClientID = "other-client"
					} else {
						changed.Twitch.UserID = "456"
					}
				case core.ChatVKVideoLive:
					if field == 0 {
						changed.VKVideoLive.ClientID = "other-client"
					} else {
						changed.VKVideoLive.ClientSecret = "other-secret"
					}
				}
				_, err = svc.RefreshChatCredentials(context.Background(), initial.Account.ID, provider, changed, "wrong-identity", 1)
				requireDomainError(t, err, core.ErrConflict)
				// stored проверяет неизменность полной записи после каждого отказа.
				stored, err := svc.GetChatCredentialResult(context.Background(), initial.Account.ID)
				if err != nil || !stored.Matches(initial) {
					t.Fatalf("Запрещённая смена identity изменила аккаунт: %v", err)
				}
			}
		})
	}
}

// TestChatReadScopesDetectCorruptionWithoutReadingSecrets проверяет независимые SQL-проекции.
// t требует обычные config-чтения при повреждённом credential payload и явный ErrCorrupt при server-read.
func TestChatReadScopesDetectCorruptionWithoutReadingSecrets(t *testing.T) {
	// fixture и ctx задают изолированный граф с настоящей схемой.
	fixture := newDomainFixture(t, 1)
	ctx := context.Background()
	// account и source содержат эталоны config без встроенных полномочий.
	account := chatAcceptanceAccount(t, fixture.service, core.ChatYouTube)
	source := chatAcceptanceSource(t, fixture.service, fixture.destinations[0].ID, account.Account)
	// configPayload и credentialPayload сохраняют раздельные нагрузки для проверки проекции.
	var configPayload, credentialPayload []byte
	// err сообщает об отказе прямого чтения двух независимо сохраняемых нагрузок.
	if err := fixture.store.sqlDB.QueryRow("SELECT payload,credential_payload FROM chat_accounts WHERE id=?", account.Account.ID).Scan(&configPayload, &credentialPayload); err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(configPayload, []byte("initial")) || !bytes.Contains(credentialPayload, []byte("initial")) {
		t.Fatal("Полномочия попали в конфигурационную нагрузку либо не сохранены отдельно")
	}
	// err сообщает отказ контролируемого повреждения только собственной credential-нагрузки.
	if _, err := fixture.store.sqlDB.Exec("UPDATE chat_accounts SET credential_payload=? WHERE id=?", []byte(`{}`), account.Account.ID); err != nil {
		t.Fatal(err)
	}
	// stored и accounts должны читаться без декодирования полномочий.
	stored, err := fixture.service.GetChatAccount(ctx, account.Account.ID)
	if err != nil || !reflect.DeepEqual(stored, account.Account) {
		t.Fatalf("Config read декодировал повреждённые credentials: %v", err)
	}
	accounts, err := fixture.service.ListChatAccounts(ctx, core.ChatAccountQuery{Limit: 10})
	if err != nil || len(accounts) != 1 || !reflect.DeepEqual(accounts[0], account.Account) {
		t.Fatalf("Config list прочитал credentials: %v", err)
	}
	_, err = fixture.service.GetChatSource(ctx, source.ID)
	if err != nil {
		t.Fatal(err)
	}
	_, err = fixture.service.GetChatCredentials(ctx, account.Account.ID)
	requireDomainError(t, err, core.ErrCorrupt)
	// statement создаёт несовпадение индексируемых credential-метаданных при правильной нагрузке.
	for _, statement := range []string{
		"UPDATE chat_accounts SET credential_write_id='other-write' WHERE id=?",
		"UPDATE chat_accounts SET credential_revision='00000000000000000002' WHERE id=?",
		"UPDATE chat_accounts SET credential_updated_at=credential_updated_at+1 WHERE id=?",
	} {
		if _, err = fixture.store.sqlDB.Exec("UPDATE chat_accounts SET credential_payload=?,credential_write_id=?,credential_revision='00000000000000000001',credential_updated_at=? WHERE id=?", credentialPayload, account.Credentials.CredentialWriteID, account.Credentials.CredentialUpdatedAt.UnixMicro(), account.Account.ID); err != nil {
			t.Fatal(err)
		}
		if _, err = fixture.store.sqlDB.Exec(statement, account.Account.ID); err != nil {
			t.Fatal(err)
		}
		_, err = fixture.service.GetChatCredentials(ctx, account.Account.ID)
		requireDomainError(t, err, core.ErrCorrupt)
	}
	if _, err = fixture.store.sqlDB.Exec("UPDATE chat_sources SET enabled=0 WHERE id=?", source.ID); err != nil {
		t.Fatal(err)
	}
	_, err = fixture.service.GetChatSource(ctx, source.ID)
	requireDomainError(t, err, core.ErrCorrupt)
	// payload меняется только в config-области; её декодирование обязано заметить внешнюю ревизию.
	if _, err = fixture.store.sqlDB.Exec("UPDATE chat_accounts SET revision='00000000000000000002' WHERE id=?", account.Account.ID); err != nil {
		t.Fatal(err)
	}
	_, err = fixture.service.GetChatAccount(ctx, account.Account.ID)
	requireDomainError(t, err, core.ErrCorrupt)
}

// setChatAcceptanceRevisions задаёт согласованные большие счётчики только в собственной БД store.
// t сообщает об ошибке; value возвращает точный эталон обеих payload с config и credential ревизиями.
func setChatAcceptanceRevisions(t *testing.T, store *Store, value core.ChatCredentialResult, config, credential uint64) core.ChatCredentialResult {
	t.Helper()
	value.Account.Revision, value.Credentials.CredentialRevision = config, credential
	// accountPayload и credentialPayload кодируются независимо; err сохраняет отказ сериализации.
	accountPayload, err := core.EncodeMetadata(value.Account)
	if err != nil {
		t.Fatal(err)
	}
	credentialPayload, err := core.EncodeMetadata(value.Credentials)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = store.sqlDB.Exec("UPDATE chat_accounts SET revision=?,payload=?,credential_revision=?,credential_payload=? WHERE id=?", fmt.Sprintf("%020d", config), accountPayload, fmt.Sprintf("%020d", credential), credentialPayload, value.Account.ID); err != nil {
		t.Fatal(err)
	}
	return value
}

// TestChatRevisionsAreExactAndOverflowRollsBackBothScopes проверяет пределы uint64 без float64.
// t требует атомарного отказа replacement при исчерпании любого из двух счётчиков.
func TestChatRevisionsAreExactAndOverflowRollsBackBothScopes(t *testing.T) {
	// config выбирает переполняющуюся область, credential задаёт вторую текущую ревизию.
	for config, credential := range map[uint64]uint64{math.MaxUint64: 7, 7: math.MaxUint64} {
		// Подтест через t изолирует каждый край uint64 и не изменяет другую проверяемую БД.
		t.Run(fmt.Sprintf("config-%d-credential-%d", config, credential), func(t *testing.T) {
			// store и svc обслуживают один изолированный набор; err сообщает отказ сервиса.
			store := openTestStore(t)
			svc, err := core.NewService(store, store.Incarnation())
			if err != nil {
				t.Fatal(err)
			}
			// initial задаёт точный положительный граничный счётчик в каждой области.
			initial := setChatAcceptanceRevisions(t, store, chatAcceptanceAccount(t, svc, core.ChatTwitch), config, credential)
			// before сохраняет все SQL-значения до операции, которая обязана полностью отказаться.
			before := captureFormatFiveSQLRows(t, store.sqlDB)
			_, err = svc.ReplaceChatCredentials(context.Background(), initial.Account.ID, core.ChatTwitch, chatAcceptanceBundle(core.ChatTwitch, "overflow"), "overflow-write", config)
			requireDomainError(t, err, core.ErrConflict)
			if !reflect.DeepEqual(before, captureFormatFiveSQLRows(t, store.sqlDB)) {
				t.Fatal("Переполнение replacement частично изменило одну область")
			}
			before = captureFormatFiveSQLRows(t, store.sqlDB)
			if config == math.MaxUint64 {
				_, err = svc.UpdateChatAccount(context.Background(), initial.Account, config)
			} else {
				_, err = svc.RefreshChatCredentials(context.Background(), initial.Account.ID, core.ChatTwitch, chatAcceptanceBundle(core.ChatTwitch, "overflow"), "overflow-write", credential)
			}
			requireDomainError(t, err, core.ErrConflict)
			if !reflect.DeepEqual(before, captureFormatFiveSQLRows(t, store.sqlDB)) {
				t.Fatal("Переполнение частично изменило одну область")
			}
		})
	}
	// store и svc нужны для допустимого счётчика выше границы точности JavaScript.
	store := openTestStore(t)
	svc, err := core.NewService(store, store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	// initial выбирает соседние, различимые uint64 выше 2^53 и сохраняет их без округления.
	initial := setChatAcceptanceRevisions(t, store, chatAcceptanceAccount(t, svc, core.ChatYouTube), 9007199254740993, 9007199254740995)
	// updated подтверждает точное увеличение обеих ревизий, включая нечётные значения.
	updated, err := svc.ReplaceChatCredentials(context.Background(), initial.Account.ID, core.ChatYouTube, chatAcceptanceBundle(core.ChatYouTube, "exact"), "exact-write", 9007199254740993)
	if err != nil || updated.Account.Revision != 9007199254740994 || updated.Credentials.CredentialRevision != 9007199254740996 {
		t.Fatalf("Потеряна точность uint64: %+v, %v", updated.Account, err)
	}
	// revision и credentialRevision читают независимое точное TEXT-представление SQL.
	var revision, credentialRevision string
	if err = store.sqlDB.QueryRow("SELECT revision,credential_revision FROM chat_accounts WHERE id=?", initial.Account.ID).Scan(&revision, &credentialRevision); err != nil || revision != "00009007199254740994" || credentialRevision != "00009007199254740996" {
		t.Fatalf("Неканоничные uint64 SQL: %q/%q, %v", revision, credentialRevision, err)
	}
	// column выбирается из фиксированного списка SQL-полей с одинаковым uint64-контрактом.
	for _, column := range []string{"revision", "credential_revision"} {
		// invalid задаёт ближайшие переполнение, ноль и неверные формы TEXT-счётчика.
		for _, invalid := range []string{"18446744073709551616", "00000000000000000000", "1", "-0000000000000000001", "0000000000000000000x"} {
			// before сохраняет обе области и весь прежний граф до отказа SQL CHECK.
			before := captureFormatFiveSQLRows(t, store.sqlDB)
			if _, err = store.sqlDB.Exec("UPDATE chat_accounts SET "+column+"=? WHERE id=?", invalid, initial.Account.ID); err == nil {
				t.Fatalf("SQL принял неверный %s=%q", column, invalid)
			}
			if !reflect.DeepEqual(before, captureFormatFiveSQLRows(t, store.sqlDB)) {
				t.Fatalf("Отклонённый SQL изменил граф при %s=%q", column, invalid)
			}
		}
	}
}

// TestChatListsKeepStableBoundariesAndCombineTypedFilters проверяет порядок при одинаковом CreatedAt.
// t ожидает строгую границу ID и пересечение Destination/Account/Provider/Stream без утечки соседних строк.
func TestChatListsKeepStableBoundariesAndCombineTypedFilters(t *testing.T) {
	// fixture содержит два назначения одного потока; ctx ограничивает локальные операции.
	fixture := newDomainFixture(t, 2)
	ctx := context.Background()
	// otherStream и otherGroup нужны для проверки Stream-фильтра через Destination.
	otherStream, err := fixture.service.CreateStream(ctx, core.Stream{Name: "Другой поток", PublicationKey: "other-chat-stream"})
	if err != nil {
		t.Fatal(err)
	}
	otherGroup, err := fixture.service.CreateGroup(ctx, core.TranscodeGroup{StreamID: otherStream.ID, ProfileID: fixture.profile.ID, Name: "Другая группа"})
	if err != nil {
		t.Fatal(err)
	}
	// otherDestination принадлежит другому потоку при той же платформенной строке медиавыхода.
	otherDestination, err := fixture.service.CreateDestination(ctx, core.Destination{StreamID: otherStream.ID, GroupID: otherGroup.ID, Name: "Другой выход", Platform: "custom", Endpoint: "rtmp://example.invalid/live"})
	if err != nil {
		t.Fatal(err)
	}
	// base задаёт равные времена, accounts — независимые config/credential значения с известными ID.
	base := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	accounts := []core.ChatCredentialResult{}
	// index и provider задают три аккаунта, два из которых принадлежат YouTube.
	for index, provider := range []core.ChatProvider{core.ChatYouTube, core.ChatYouTube, core.ChatTwitch} {
		// id адресует аккаунт в заранее известном порядке, record задаёт одинаковую микросекунду.
		id := core.ID(fmt.Sprintf("account-%d", index))
		record := core.Record{ID: id, Revision: 1, CreatedAt: base, UpdatedAt: base}
		accounts = append(accounts, core.ChatCredentialResult{Account: core.ChatAccount{Record: record, Provider: provider}, Credentials: core.ChatCredentials{AccountID: id, Provider: provider, CredentialRevision: 1, CredentialUpdatedAt: base, CredentialWriteID: "initial", Bundle: chatAcceptanceBundle(provider, "initial")}})
	}
	// sources задаёт четыре строки с известными ID, одинаковыми временами и различающимися родителями.
	var sources []core.ChatSource
	// index выбирает отдельную комбинацию Destination/Account и её provider.
	for index := range 4 {
		// destination и account задают базовую пару; последующие ветви меняют только нужный фильтр.
		destination := fixture.destinations[0].ID
		account := accounts[0].Account
		switch index {
		case 1:
			destination = fixture.destinations[1].ID
		case 2:
			account = accounts[2].Account
		case 3:
			destination, account = otherDestination.ID, accounts[1].Account
		}
		// options сохраняет согласованную закрытую форму, optionErr сообщает отказ её создания;
		// hint выбирает постоянный канал.
		options, optionErr := core.NewChatOptions(account.Provider)
		if optionErr != nil {
			t.Fatal(optionErr)
		}
		hint := "UCchannel"
		if account.Provider == core.ChatTwitch {
			hint = ""
		}
		sources = append(sources, core.ChatSource{Record: core.Record{ID: core.ID(fmt.Sprintf("source-%d", index)), Revision: 1, CreatedAt: base, UpdatedAt: base}, DestinationID: destination, AccountID: account.ID, Provider: account.Provider, ChannelHint: hint, Options: options})
	}
	// Callback через r сохраняет определённые эталоны через настоящий repository, без AutoMigrate.
	err = fixture.store.Write(ctx, func(r core.Repositories) error {
		// account содержит полный согласованный набор с одинаковым CreatedAt.
		for _, account := range accounts {
			// writeErr сохраняет настоящую ошибку атомарной вставки.
			if writeErr := r.Chat.InsertChatAccount(ctx, account); writeErr != nil {
				return writeErr
			}
		}
		// index выбирает эталонные строки в обратном порядке, чтобы проверить SQL-порядок страницы.
		for index := len(sources) - 1; index >= 0; index-- {
			// writeErr сохраняет причину отказа вставки полного Source или его обязательных связей.
			if writeErr := r.Chat.InsertChatSource(ctx, sources[index]); writeErr != nil {
				return writeErr
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// page возвращает первую пару при равных временах независимо от порядка вставки.
	page, err := fixture.service.ListChatSources(ctx, core.ChatSourceQuery{Limit: 2})
	if err != nil || len(page) != 2 || page[0].ID != "source-0" || page[1].ID != "source-1" {
		t.Fatalf("Нестабильная первая страница: %+v, %v", page, err)
	}
	page, err = fixture.service.ListChatSources(ctx, core.ChatSourceQuery{Limit: 2, After: &core.ListBoundary{CreatedAt: base, ID: "source-1"}})
	if err != nil || len(page) != 2 || page[0].ID != "source-2" || page[1].ID != "source-3" {
		t.Fatalf("Граница повторила или потеряла строку: %+v, %v", page, err)
	}
	page, err = fixture.service.ListChatSources(ctx, core.ChatSourceQuery{Limit: 10, StreamID: fixture.stream.ID, Provider: core.ChatYouTube, AccountID: accounts[0].Account.ID, DestinationID: fixture.destinations[1].ID})
	if err != nil || len(page) != 1 || page[0].ID != "source-1" {
		t.Fatalf("Фильтры не пересечены: %+v, %v", page, err)
	}
	// Отдельный Provider-фильтр исключает Twitch без помощи ограничений родителей.
	page, err = fixture.service.ListChatSources(ctx, core.ChatSourceQuery{Limit: 10, Provider: core.ChatYouTube})
	if err != nil || len(page) != 3 || page[0].ID != "source-0" || page[1].ID != "source-1" || page[2].ID != "source-3" {
		t.Fatalf("Provider-фильтр вернул чужую платформу или потерял источник: %+v, %v", page, err)
	}
	// Отдельный Account-фильтр исключает оба чужих аккаунта, включая другой YouTube.
	page, err = fixture.service.ListChatSources(ctx, core.ChatSourceQuery{Limit: 10, AccountID: accounts[0].Account.ID})
	if err != nil || len(page) != 2 || page[0].ID != "source-0" || page[1].ID != "source-1" {
		t.Fatalf("Account-фильтр вернул чужой аккаунт или потерял источник: %+v, %v", page, err)
	}
	page, err = fixture.service.ListChatSources(ctx, core.ChatSourceQuery{Limit: 10, StreamID: otherStream.ID})
	if err != nil || len(page) != 1 || page[0].ID != "source-3" {
		t.Fatalf("Stream-фильтр не использовал Destination: %+v, %v", page, err)
	}
	// accountPage проверяет независимую страницу Accounts с тем же контрактом границы.
	accountPage, err := fixture.service.ListChatAccounts(ctx, core.ChatAccountQuery{Limit: 1, Provider: core.ChatYouTube, After: &core.ListBoundary{CreatedAt: base, ID: "account-0"}})
	if err != nil || len(accountPage) != 1 || accountPage[0].ID != "account-1" {
		t.Fatalf("Нарушен provider или Account boundary: %+v, %v", accountPage, err)
	}
	// Отдельный Provider-фильтр аккаунтов исключает YouTube без помощи границы страницы.
	accountPage, err = fixture.service.ListChatAccounts(ctx, core.ChatAccountQuery{Limit: 10, Provider: core.ChatTwitch})
	if err != nil || len(accountPage) != 1 || accountPage[0].ID != "account-2" {
		t.Fatalf("Provider-фильтр аккаунтов вернул чужую платформу или потерял аккаунт: %+v, %v", accountPage, err)
	}
}
