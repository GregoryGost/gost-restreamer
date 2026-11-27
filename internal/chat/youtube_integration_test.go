package chat

import (
	"context"
	"encoding/json"
	"errors"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
)

const (
	// youtubeFixtureRejectedKey вызывает подтверждённый отказ проверки до записи.
	youtubeFixtureRejectedKey = "rejected-synthetic-google-key"
	// youtubeFixtureCancelledKey удерживает только сетевую проверку до её отмены.
	youtubeFixtureCancelledKey = "cancelled-synthetic-google-key"
	// youtubeFixtureReplacementKey представляет проверенную замену первоначального ключа тестовой SQLite.
	youtubeFixtureReplacementKey = "replacement-synthetic-google-key"
)

// managerYouTubeFixtureAdapter подставляет события платформы на уровне порта,
// оставляя реальными хранение, учёт попыток, применение конфигурации и дедупликацию Manager/Hub.
type managerYouTubeFixtureAdapter struct {
	// chatTestAdapter предоставляет стандартный поиск и канал состоявшихся попыток.
	chatTestAdapter
	// verifying сообщает, что отменяемая проверка дошла до платформенной границы.
	verifying chan struct{}
	// disconnect задаёт контролируемое завершение текущего физического чтения с позицией.
	disconnect chan *core.ChatReadRetry
	// resolving содержит запросы перепроверки выбранного видео.
	resolving chan string
	// resolved управляет поступлением подтверждения точного видео в Manager.
	resolved chan core.ChatDiscovery
}

// ValidateAPIKey обрабатывает синтетические key для проверки маппинга безопасной причины и ctx.
func (a *managerYouTubeFixtureAdapter) ValidateAPIKey(ctx context.Context, key string) (core.ChatReason, error) {
	switch key {
	case youtubeFixtureRejectedKey:
		// HTTP-адаптер возвращает безопасную причину подтверждённого отказа без ошибки транспорта.
		return core.ChatReasonCredentials, nil
	case youtubeFixtureCancelledKey:
		close(a.verifying)
		<-ctx.Done()
		return core.ChatReasonNone, ctx.Err()
	default:
		return core.ChatReasonNone, nil
	}
}

// Read подтверждает request через sink, передаёт тесту точный токен и ждёт контекст ctx или разрыв.
func (a *managerYouTubeFixtureAdapter) Read(ctx context.Context, request core.ChatReadRequest, sink core.ChatAdapterSink) error {
	// stopped фиксирует освобождение физического чтения после отмены или внешнего сбоя.
	stopped := make(chan struct{})
	defer close(stopped)
	// err сообщает, допускает ли Manager точный токен ещё до подключения.
	if err := sink.Status(ctx, request.Token, core.ChatConnected, core.ChatReasonNone); err != nil {
		return err
	}
	select {
	case a.readers <- chatTestReader{request: request, sink: sink, stopped: stopped}:
	case <-ctx.Done():
		return ctx.Err()
	}
	select {
	// retry передаёт позицию после последнего полностью принятого ответа.
	case retry := <-a.disconnect:
		return retry
	case <-ctx.Done():
		return ctx.Err()
	}
}

// DiscoverBroadcast сообщает видео videoID, которое Manager решил подтвердить после ожидания.
// ctx позволяет завершить задержанную проверку при отзыве текущей привязки.
func (a *managerYouTubeFixtureAdapter) DiscoverBroadcast(ctx context.Context, _ core.ChatSource, _ core.ChatCredentials, videoID string) (core.ChatDiscovery, error) {
	select {
	case a.resolving <- videoID:
	case <-ctx.Done():
		return core.ChatDiscovery{}, ctx.Err()
	}
	select {
	// found содержит независимо заданное текущее состояние именно проверенного видео.
	case found := <-a.resolved:
		return found, nil
	case <-ctx.Done():
		return core.ChatDiscovery{}, ctx.Err()
	}
}

// startYouTubeManagerFixture запускает Manager и активирует один наблюдаемый выход
// в отдельной SQLite. ctx ограничивает открытие, t закрывает Manager до закрытия Store.
func startYouTubeManagerFixture(t *testing.T, ctx context.Context, adapter core.ChatAdapterPort) (chatManagerTestFixture, *Manager) {
	t.Helper()
	// fixture хранит сохранённую конфигурацию и неопубликованные наблюдатели.
	fixture := newChatManagerTestFixture(t, ctx)
	// limits сохраняет штатный учёт ресурсов с короткими паузами для повторов в тесте.
	limits := core.DefaultChatLimits()
	limits.ReconnectMinBackoff = time.Millisecond
	limits.ReconnectMaxBackoff = 5 * time.Millisecond
	// manager использует настоящий runtime и SQLite. err сообщает о невозможности их связать.
	manager, err := New(ctx, fixture.service, fixture.store, limits, []core.ChatAdapterPort{adapter})
	chatTestOK(t, err)
	t.Cleanup(func() {
		// closeCtx и stop задают независимый срок очистки реестра до закрытия SQLite.
		closeCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		chatTestOK(t, manager.Close(closeCtx))
	})
	chatTestOK(t, fixture.outputs[0].Observe(ctx, core.OutputObservation{State: core.OutputPublishing}))
	return fixture, manager
}

// youtubeFixtureRequest передаёт серверный key для создания или замены accountID по revision.
func youtubeFixtureRequest(accountID core.ID, revision uint64, key string) core.ChatAuthorizationRequest {
	return core.ChatAuthorizationRequest{
		Provider: core.ChatYouTube, AccountID: accountID, ExpectedRevision: revision,
		Bundle: core.ChatCredentialBundle{YouTube: &core.YouTubeChatCredentials{APIKey: key}},
	}
}

// TestYouTubeManagerValidatedReplacementProtectsReaderAndCursor проверяет транзакционную границу
// ключа и поведение читателя при повторе и смене аккаунта внутри живой Session.
func TestYouTubeManagerValidatedReplacementProtectsReaderAndCursor(t *testing.T) {
	// ctx и cancel ограничивают ожидание SQLite, сообщений Hub и физических подписок.
	ctx, cancel := context.WithTimeout(t.Context(), 10*time.Second)
	defer cancel()
	// adapter моделирует отказ проверки и повторное чтение, не меняя тестовую SQLite напрямую.
	adapter := &managerYouTubeFixtureAdapter{
		chatTestAdapter: chatTestAdapter{readers: make(chan chatTestReader, 4)},
		verifying:       make(chan struct{}), disconnect: make(chan *core.ChatReadRetry, 1),
	}
	// fixture и manager дают наблюдаемый рабочий поток и его операции подключения.
	fixture, manager := startYouTubeManagerFixture(t, ctx, adapter)
	// first обращается к действующему исходному аккаунту, accountID связывает операции CAS с ним.
	first := chatTestReceive(t, ctx, adapter.readers)
	accountID := fixture.sources[0].AccountID
	// original содержит обе исходные ревизии и ровно исходный полный набор полномочий.
	original, err := fixture.service.GetChatCredentialResult(ctx, accountID)
	chatTestOK(t, err)
	if first.request.ResumeToken != "" {
		t.Error("Новый логический читатель получил чужую позицию")
	}
	chatTestOK(t, first.publish(ctx, "first-message"))
	// refused, reason и err отражают проверенный отказ до изменения доступа.
	refused, reason, err := manager.ConnectYouTube(ctx, youtubeFixtureRequest(accountID, original.Account.Revision, youtubeFixtureRejectedKey))
	if refused.ID != "" || reason != core.ChatReasonCredentials || err == nil {
		t.Fatalf("Отклонённый ключ принят: аккаунт=%t, причина=%q, ошибка=%t", refused.ID != "", reason, err != nil)
	}
	// validationCtx и cancelValidation отзывают один зависший запрос, не затрагивая Manager.
	validationCtx, cancelValidation := context.WithCancel(ctx)
	defer cancelValidation()
	// cancelled принимает результат фактически отменённой проверки.
	cancelled := make(chan error, 1)
	go func() {
		// Горутина выполняет замену и передаёт её результат после отмены платформенного запроса.
		// validateErr возвращается до любой записи ключа.
		_, _, validateErr := manager.ConnectYouTube(validationCtx, youtubeFixtureRequest(accountID, original.Account.Revision, youtubeFixtureCancelledKey))
		cancelled <- validateErr
	}()
	chatTestReceive(t, ctx, adapter.verifying)
	cancelValidation()
	if !errors.Is(chatTestReceive(t, ctx, cancelled), context.Canceled) {
		t.Fatal("Отмена проверки не прервала замену")
	}
	// unchanged проверяет фактическую сохранённую запись после обоих видов отказа.
	unchanged, err := fixture.service.GetChatCredentialResult(ctx, accountID)
	chatTestOK(t, err)
	if !unchanged.Matches(original) {
		t.Fatal("Отказ или отмена проверки изменили SQLite")
	}
	chatTestOK(t, first.publish(ctx, "after-refusal"))
	adapter.disconnect <- &core.ChatReadRetry{ResumeToken: "only-this-logical-reader"}
	chatTestReceive(t, ctx, first.stopped)
	// resumed имеет нового владельца callbacks, но прежнюю логическую беседу и ее позицию.
	resumed := chatTestReceive(t, ctx, adapter.readers)
	if resumed.request.ResumeToken != "only-this-logical-reader" || resumed.request.Token == first.request.Token ||
		resumed.request.Key != first.request.Key {
		t.Fatal("Повтор физического чтения нарушил границу логического владельца")
	}
	chatTestOK(t, resumed.publish(ctx, "first-message"))
	chatTestOK(t, resumed.publish(ctx, "after-resume"))
	// image ждёт обработки повторённого сообщения и подтверждает дедупликацию на настоящем Hub.
	image := waitChatTestSnapshot(t, ctx, manager, fixture.stream.ID, func(snapshot core.ChatSnapshot) bool {
		// Предикат ждёт последнюю публикацию, после которой видны все принятые повторы.
		return slices.ContainsFunc(snapshot.Messages, func(message core.ChatMessage) bool {
			// Предикат сравнивает платформенный ID последней публикации.
			return message.ProviderMessageID == "after-resume"
		})
	})
	if len(image.Messages) != 3 || image.Messages[0].ProviderMessageID != "first-message" ||
		image.Messages[1].ProviderMessageID != "after-refusal" || image.Messages[2].ProviderMessageID != "after-resume" {
		t.Fatalf("Hub не сохранил порядок или опубликовал повтор: %d сообщений", len(image.Messages))
	}

	// account возвращает сохранённую публичную ревизию после отсечения прежнего доступа.
	account, reason, err := manager.ConnectYouTube(ctx, youtubeFixtureRequest(accountID, original.Account.Revision, youtubeFixtureReplacementKey))
	chatTestOK(t, err)
	if account.ID != accountID || account.Revision != original.Account.Revision+1 || reason != core.ChatReasonNone {
		t.Fatal("Подтверждённая замена не вернула новую ревизию того же аккаунта")
	}
	chatTestReceive(t, ctx, resumed.stopped)
	if err := resumed.publish(ctx, "stale-attempt"); !errors.Is(err, core.ErrStale) {
		t.Fatalf("Предыдущее чтение ещё может публиковать: %v", err)
	}
	// replacement начинает новый логический reader без позиции от старых полномочий.
	replacement := chatTestReceive(t, ctx, adapter.readers)
	if replacement.request.ResumeToken != "" || replacement.request.Token == resumed.request.Token ||
		replacement.request.Credentials.Bundle.YouTube.APIKey != youtubeFixtureReplacementKey {
		t.Fatal("Новая подписка унаследовала позицию или прежние полномочия")
	}
	// stored подтверждает, что новый ключ действительно был сохранён вместе с отдельной ревизией.
	stored, err := fixture.service.GetChatCredentialResult(ctx, accountID)
	chatTestOK(t, err)
	if stored.Credentials.Bundle.YouTube.APIKey != youtubeFixtureReplacementKey ||
		stored.Credentials.CredentialRevision != original.Credentials.CredentialRevision+1 {
		t.Fatal("Сохранённый набор не соответствует подтверждённой замене")
	}
	// publicResponse сериализует только пользовательский ответ и публичное состояние чата.
	publicResponse, err := json.Marshal([]any{refused, account, image})
	chatTestOK(t, err)
	// secret содержит один из синтетических серверных ключей, которых не должно быть в результате.
	for _, secret := range []string{original.Credentials.Bundle.YouTube.APIKey, youtubeFixtureReplacementKey, youtubeFixtureRejectedKey, youtubeFixtureCancelledKey} {
		if strings.Contains(string(publicResponse), secret) {
			t.Fatal("Публичный ответ содержит серверный ключ")
		}
	}
}

// TestYouTubeManagerRevalidatesSelectedVideoBeforeRead проверяет, что чат из первоначального набора
// используется только после проверки выбранного видео, а временный отказ сохраняет точный выбор.
func TestYouTubeManagerRevalidatesSelectedVideoBeforeRead(t *testing.T) {
	// ctx и cancel ограничивают закончившееся обнаружение и время до выбора.
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	// adapter выдаёт два эфира и отдельно подтверждает факт доступности выбранного видео.
	adapter := &managerYouTubeFixtureAdapter{
		chatTestAdapter: chatTestAdapter{readers: make(chan chatTestReader, 2), broadcasts: []core.ChatBroadcast{
			{VideoID: "first-video", ChannelID: "other-chat"},
			{VideoID: "selected-video", ChannelID: "obsolete-chat"},
		}},
		resolving: make(chan string, 4), resolved: make(chan core.ChatDiscovery, 4),
	}
	// fixture и manager публикуют один Source после открытия его выхода.
	fixture, manager := startYouTubeManagerFixture(t, ctx, adapter)
	// source фиксирует конфигурационную ревизию ссылки на найденные варианты.
	source := fixture.sources[0]
	// waiting содержит точную Session, в которой допускается выбор.
	waiting := waitChatTestSnapshot(t, ctx, manager, fixture.stream.ID, func(snapshot core.ChatSnapshot) bool {
		// Предикат ожидает публичного состояния выбора без автоназначения чата.
		return snapshot.Session != nil && chatTestHasState(snapshot, source.ID, core.ChatWaitingChat)
	})
	// choices содержит готовое поколение и независимую копию найденных вариантов.
	choices, err := manager.BroadcastChoices(ctx, *waiting.Session, source.ID)
	chatTestOK(t, err)
	if len(choices.Broadcasts) != 2 {
		t.Fatalf("Неоднозначный поиск вернул %d вариантов", len(choices.Broadcasts))
	}
	select {
	case <-adapter.readers:
		t.Fatal("Несколько кандидатов привели к автоматическому чтению")
	default:
	}
	chatTestOK(t, manager.SelectBroadcast(ctx, core.ChatBroadcastSelection{
		Session: *waiting.Session, SourceID: source.ID, SourceRevision: choices.SourceRevision,
		DiscoveryGeneration: choices.DiscoveryGeneration, VideoID: "selected-video",
	}))
	if chatTestReceive(t, ctx, adapter.resolving) != "selected-video" {
		t.Fatal("Повторная проверка получила другое видео")
	}
	select {
	case <-adapter.readers:
		t.Fatal("Чтение началось по устаревшему chat ID до ответа перепроверки")
	default:
	}
	// videos.list не подтвердил запрошенный ID. Адаптер сообщает временный отказ, как при ответе с чужим видео.
	adapter.resolved <- core.ChatDiscovery{State: core.ChatRetrying, Reason: core.ChatReasonNetwork}
	if chatTestReceive(t, ctx, adapter.resolving) != "selected-video" {
		t.Fatal("Временный отказ изменил выбранное видео")
	}
	// page читает уже состоявшийся переход к повтору, который мог исчезнуть из текущего снимка.
	page, err := manager.Events(ctx, fixture.stream.ID, waiting.Cursor, 32)
	chatTestOK(t, err)
	if page.ResyncRequired || !slices.ContainsFunc(page.Events, func(event core.ChatEvent) bool {
		// Предикат проверяет публичную причину после отказа проверки точного видео.
		return event.Status != nil && event.Status.SourceID == source.ID &&
			event.Status.State == core.ChatRetrying && event.Status.Reason == core.ChatReasonNetwork
	}) {
		t.Fatal("Временный отказ проверки не появился как RETRYING/network")
	}
	adapter.resolved <- core.ChatDiscovery{
		State: core.ChatWaitingChat, Reason: core.ChatReasonUnavailable,
		Broadcasts: []core.ChatBroadcast{{VideoID: "selected-video"}},
	}
	if chatTestReceive(t, ctx, adapter.resolving) != "selected-video" {
		t.Fatal("В ожидании чата была потеряна привязка к выбранному видео")
	}
	adapter.resolved <- core.ChatDiscovery{Broadcasts: []core.ChatBroadcast{{VideoID: "selected-video", ChannelID: "confirmed-chat"}}}
	// reader наблюдает фактический канал только после нового подтверждения выбранного видео.
	reader := chatTestReceive(t, ctx, adapter.readers)
	if reader.request.Key.ChannelID != "confirmed-chat" || reader.request.Key.Session != *waiting.Session {
		t.Fatalf("Чтение использовало чужую Session или неподтверждённый чат: %+v", reader.request.Key)
	}
}

// youtubeDistinctReadersAdapter оставляет настоящему Manager допуск отдельных попыток.
// Фикстура chatTestAdapter удерживает каждое чтение до отмены его собственного контекста.
type youtubeDistinctReadersAdapter struct {
	// chatTestAdapter предоставляет подтверждения и сигналы остановки чтений.
	chatTestAdapter
}

// Discover возвращает разные фактические беседы по идентичности source.
// Контекст и полномочия передаёт Manager. Фикстура строит ID беседы из конфигурации источника.
func (*youtubeDistinctReadersAdapter) Discover(_ context.Context, source core.ChatSource, _ core.ChatCredentials) (core.ChatDiscovery, error) {
	return core.ChatDiscovery{ChannelID: "chat-" + string(source.ID)}, nil
}

// TestYouTubeManagerEndedReaderLeavesOtherReaderPublishing подаёт признак завершения
// одного из двух действующих чатов в одной Session и проверяет их состояния и историю Hub.
func TestYouTubeManagerEndedReaderLeavesOtherReaderPublishing(t *testing.T) {
	// ctx и cancel ограничивают открытие двух чтений, остановку первого и публикацию второго.
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	// adapter связывает каждый Source с отдельной фактической беседой без обращения к Google.
	adapter := &youtubeDistinctReadersAdapter{chatTestAdapter: chatTestAdapter{readers: make(chan chatTestReader, 4)}}
	// fixture открывает один публикующий выход, manager владеет действиями обоих читателей.
	fixture, manager := startYouTubeManagerFixture(t, ctx, adapter)
	chatTestOK(t, fixture.outputs[2].Observe(ctx, core.OutputObservation{State: core.OutputPublishing}))
	// first и second получают два одновременно действующих физических чтения.
	first, second := chatTestReceive(t, ctx, adapter.readers), chatTestReceive(t, ctx, adapter.readers)
	// firstID и secondID задают ожидание по каждому выходу независимо от порядка подключения.
	firstID, secondID := fixture.sources[0].ID, fixture.sources[1].ID
	if second.request.Key.ChannelID == "chat-"+string(firstID) {
		first, second = second, first
	}
	if first.request.Key.ChannelID != "chat-"+string(firstID) || second.request.Key.ChannelID != "chat-"+string(secondID) ||
		first.request.Key.Session != second.request.Key.Session || first.request.Token == second.request.Token {
		t.Fatal("Не открылись два независимых чтения одной Session")
	}
	chatTestOK(t, first.publish(ctx, "first-before-end"))
	chatTestOK(t, second.publish(ctx, "second-before-end"))
	waitChatTestSnapshot(t, ctx, manager, fixture.stream.ID, func(snapshot core.ChatSnapshot) bool {
		// Предикат подтверждает публикующие источники до имитации конца первого чата.
		return len(snapshot.Messages) == 2 && chatTestHasState(snapshot, firstID, core.ChatConnected) &&
			chatTestHasState(snapshot, secondID, core.ChatConnected)
	})
	chatTestOK(t, first.sink.Status(ctx, first.request.Token, core.ChatFailed, core.ChatReasonEnded))
	chatTestReceive(t, ctx, first.stopped)
	// err показывает, был ли отозван точный вход завершившегося чтения.
	if err := first.publish(ctx, "first-after-end"); !errors.Is(err, core.ErrStale) {
		t.Fatalf("Завершившееся чтение по-прежнему принимает сообщения: %v", err)
	}
	chatTestOK(t, second.publish(ctx, "second-after-end"))
	// after ждёт появления сообщения второго источника после фактической остановки первого.
	after := waitChatTestSnapshot(t, ctx, manager, fixture.stream.ID, func(snapshot core.ChatSnapshot) bool {
		// Предикат проверяет доставку через настоящий Hub после завершения первого чтения.
		return slices.ContainsFunc(snapshot.Messages, func(message core.ChatMessage) bool {
			// Предикат ищет сообщение, принятое по исходному токену второго читателя.
			return message.ProviderMessageID == "second-after-end" && message.ChatSourceID == secondID
		})
	})
	if len(after.Messages) != 3 || !chatTestHasState(after, secondID, core.ChatConnected) ||
		!slices.ContainsFunc(after.Statuses, func(status core.ChatSourceStatus) bool {
			// Предикат сверяет, что завершение отнесено только к первому источнику.
			return status.SourceID == firstID && status.State == core.ChatFailed && status.Reason == core.ChatReasonEnded
		}) {
		t.Fatal("Завершение одного чата изменило состояние или историю второго")
	}
}

// TestYouTubeManagerCreatesOnlyValidatedAccount проверяет первичную запись ключа,
// её начальные ревизии, отсутствие строки при отказе и публичное представление аккаунта.
func TestYouTubeManagerCreatesOnlyValidatedAccount(t *testing.T) {
	// ctx и cancel ограничивают открытие SQLite и создание аккаунта.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// store и service используют мигрированную временную SQLite без активных выходов.
	store, service := openChatTestStore(t, ctx)
	// adapter подтверждает или отклоняет ключ независимо от наличия эфира.
	adapter := &managerYouTubeFixtureAdapter{}
	// manager связывает проверку с долговременным хранением. err сообщает об отказе создания.
	manager, err := New(ctx, service, store, core.DefaultChatLimits(), []core.ChatAdapterPort{adapter})
	chatTestOK(t, err)
	t.Cleanup(func() {
		// closeCtx и stop ограничивают остановку Manager до закрытия SQLite.
		closeCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		chatTestOK(t, manager.Close(closeCtx))
	})
	// denied, reason и err проверяют отказ внешней валидации до начала записи нового аккаунта.
	denied, reason, err := manager.ConnectYouTube(ctx, youtubeFixtureRequest("", 0, youtubeFixtureRejectedKey))
	if denied.ID != "" || reason != core.ChatReasonCredentials || err == nil {
		t.Fatal("Отклонённый ключ создал публичный аккаунт")
	}
	// request запрашивает создание аккаунта с метаданными и подтверждённым синтетическим ключом.
	request := youtubeFixtureRequest("", 0, youtubeFixtureReplacementKey)
	request.Name = "Канал YouTube"
	// account содержит публичный результат независимо от отсутствия активной Session.
	account, reason, err := manager.ConnectYouTube(ctx, request)
	chatTestOK(t, err)
	if reason != core.ChatReasonNone || account.ID == "" || account.Revision != 1 || account.Name != request.Name {
		t.Fatal("Принятый ключ не создал аккаунт с ожидаемой начальной ревизией")
	}
	// accounts подтверждает, что существует только успешно проверенная запись.
	accounts, err := service.ListChatAccounts(ctx, core.ChatAccountQuery{Limit: 10, Provider: core.ChatYouTube})
	chatTestOK(t, err)
	if len(accounts) != 1 || accounts[0].ID != account.ID {
		t.Fatal("В SQLite отсутствует подтверждённый аккаунт или появилась отклонённая запись")
	}
	// credentials читает ключ с независимой серверной стороны после фиксации транзакции.
	credentials, err := service.GetChatCredentials(ctx, account.ID)
	chatTestOK(t, err)
	if credentials.CredentialRevision != 1 || credentials.Bundle.YouTube.APIKey != youtubeFixtureReplacementKey {
		t.Fatal("В SQLite отсутствует проверенный ключ или неверна его начальная ревизия")
	}
	// public сериализует публичные метаданные без обращения к серверному набору.
	public, err := json.Marshal(account)
	chatTestOK(t, err)
	if strings.Contains(string(public), youtubeFixtureReplacementKey) {
		t.Fatal("Публичный результат первичной записи содержит ключ")
	}
}

// youtubeCreateCommitFixture исполняет Write на временной SQLite и позволяет потерять
// подтверждение после COMMIT либо вернуть отказ из транзакции, чтобы она откатилась.
type youtubeCreateCommitFixture struct {
	// Store выполняет реальные транзакции и сохраняет доступ к RuntimeAuthority.
	*storage.Store
	// failure возвращается вызывающему при обоих вариантах записи.
	failure error
	// commit выбирает, в какой момент фикстура возвращает failure.
	commit bool
	// afterCommit позволяет отменить исходный запрос после успешной транзакции.
	afterCommit func()
	// readFailure имитирует ошибку последующего чтения через обёрнутый Service.
	readFailure error
	// writes считает все попытки создать аккаунт из одного запроса подключения.
	writes int
}

// Write передаёт callback в реальную транзакцию Store с контекстом ctx.
// При commit ошибка возвращается только после успешной фиксации.
func (f *youtubeCreateCommitFixture) Write(ctx context.Context, callback func(core.Repositories) error) error {
	f.writes++
	// err сохраняет исход транзакции до искусственной потери подтверждения.
	err := f.Store.Write(ctx, func(r core.Repositories) error {
		// Callback выполняет исходную вставку. Только сценарий отката завершает его ошибкой.
		// writeErr сообщает о реальном отказе репозитория.
		if writeErr := callback(r); writeErr != nil {
			return writeErr
		}
		if !f.commit {
			return f.failure
		}
		return nil
	})
	if err != nil {
		return err
	}
	if f.afterCommit != nil {
		f.afterCommit()
	}
	return f.failure
}

// View проводит сверку через Store или возвращает заданный отказ чтения до вызова callback.
// ctx и callback сохраняют контракт обычного согласованного чтения.
func (f *youtubeCreateCommitFixture) View(ctx context.Context, callback func(core.Repositories) error) error {
	if f.readFailure != nil {
		return f.readFailure
	}
	return f.Store.View(ctx, callback)
}

// youtubeCountingKeyValidator учитывает проверки ключа при одном подключении аккаунта.
type youtubeCountingKeyValidator struct {
	// managerYouTubeFixtureAdapter предоставляет остальные операции локальной платформы.
	managerYouTubeFixtureAdapter
	// keys хранит последовательность синтетических ключей, переданных синхронному валидатору.
	keys []string
}

// ValidateAPIKey запоминает key и делегирует его проверку исходной фикстуре с тем же ctx.
func (a *youtubeCountingKeyValidator) ValidateAPIKey(ctx context.Context, key string) (core.ChatReason, error) {
	a.keys = append(a.keys, key)
	return a.managerYouTubeFixtureAdapter.ValidateAPIKey(ctx, key)
}

// TestYouTubeManagerReconcilesAmbiguousCreate проверяет точную квитанцию после потери
// подтверждения COMMIT, отдельный срок сверки и отсутствие повторного создания аккаунта.
func TestYouTubeManagerReconcilesAmbiguousCreate(t *testing.T) {
	// writeFailure имитирует потерю результата записи, readFailure — недоступность сверки.
	writeFailure := errors.New("подтверждение записи потеряно")
	readFailure := errors.New("чтение квитанции недоступно")
	// cases различает успешную сверку, отсутствие зафиксированной записи и неподтверждённый результат.
	cases := []struct {
		// name описывает наблюдаемый исход потери подтверждения.
		name string
		// commit выбирает COMMIT либо откат настоящей SQLite.
		commit bool
		// cancelRequest моделирует отмену клиентского запроса в момент потери подтверждения.
		cancelRequest bool
		// readErr задаёт дополнительный отказ чтения.
		readErr error
		// wantErr задаёт внешний результат Manager.
		wantErr error
	}{
		{"committed after request cancellation", true, true, nil, nil},
		{"rolled back", false, false, nil, writeFailure},
		{"committed but read failed", true, false, readFailure, ErrCredentialCommitUnknown},
	}
	// tc задаёт один исход записи и сверки в независимой SQLite.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Подтест сравнивает ответ Manager с результатом отдельного чтения настоящей SQLite.
			// ctx и cancel ограничивают срок жизни Manager и независимое чтение.
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			// store выполняет транзакции, direct читает их исход вне обёртки.
			store, direct := openChatTestStore(t, ctx)
			// requestCtx и cancelRequest меняют срок клиентского запроса, сохраняя срок жизни Manager.
			requestCtx, cancelRequest := context.WithCancel(ctx)
			defer cancelRequest()
			// wrapped имитирует утрату подтверждения при записи в мигрированное хранилище.
			wrapped := &youtubeCreateCommitFixture{Store: store, commit: tc.commit, failure: writeFailure, readFailure: tc.readErr}
			if tc.cancelRequest {
				wrapped.afterCommit = cancelRequest
			}
			// service выполняет создание и сверку через обёртку, err сообщает о её настройке.
			service, err := core.NewService(wrapped, store.Incarnation())
			chatTestOK(t, err)
			// adapter проверяет только синтетический ключ, manager использует настоящий сервис и хранилище.
			adapter := &youtubeCountingKeyValidator{}
			manager, err := New(ctx, service, store, core.DefaultChatLimits(), []core.ChatAdapterPort{adapter})
			chatTestOK(t, err)
			t.Cleanup(func() {
				// Закрывает Manager перед освобождением файловой SQLite.
				// closeCtx и stop дают Manager собственный срок очистки перед закрытием Store.
				closeCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
				defer stop()
				chatTestOK(t, manager.Close(closeCtx))
			})
			// request задаёт новый аккаунт с синтетическими полномочиями и явным публичным именем.
			request := youtubeFixtureRequest("", 0, youtubeFixtureReplacementKey)
			request.Name = "Проверенный канал"
			// account, reason и err принимают публичный результат попытки создания.
			account, reason, err := manager.ConnectYouTube(requestCtx, request)
			if !errors.Is(err, tc.wantErr) || tc.readErr != nil && !errors.Is(err, tc.readErr) || reason != core.ChatReasonNone {
				t.Fatalf("Получен неверный результат сверки: причина=%q, ошибка=%v", reason, err)
			}
			if tc.cancelRequest && requestCtx.Err() != context.Canceled {
				t.Fatal("Сценарий должен отменить запрос после COMMIT")
			}
			if wrapped.writes != 1 || !slices.Equal(adapter.keys, []string{youtubeFixtureReplacementKey}) {
				t.Fatalf("После ошибки выполнена повторная проверка или запись: проверок=%d, записей=%d", len(adapter.keys), wrapped.writes)
			}
			// accounts читает фактическое число долговечных записей и их публичные метаданные.
			accounts, err := direct.ListChatAccounts(ctx, core.ChatAccountQuery{Limit: 10, Provider: core.ChatYouTube})
			chatTestOK(t, err)
			if !tc.commit {
				if len(accounts) != 0 || account.ID != "" {
					t.Fatal("После подтверждённого отката появился аккаунт")
				}
				return
			}
			if len(accounts) != 1 || accounts[0].Name != request.Name || accounts[0].Revision != 1 {
				t.Fatalf("SQLite не сохранила один аккаунт с исходной ревизией: записей=%d", len(accounts))
			}
			// receipt читает обе ревизии, уже выданный ID и сохранённый проверенный набор.
			receipt, err := direct.GetChatCredentialResult(ctx, accounts[0].ID)
			chatTestOK(t, err)
			if receipt.Validate() != nil || receipt.Credentials.CredentialRevision != 1 ||
				receipt.Credentials.Bundle.YouTube.APIKey != youtubeFixtureReplacementKey {
				t.Fatal("Сохранённая квитанция расходится с проверенным ключом или начальной ревизией")
			}
			if tc.wantErr == nil && account != receipt.Account || tc.wantErr != nil && account.ID != "" {
				t.Fatal("Публичный результат не соответствует подтверждённой записи")
			}
		})
	}
}
