package chat

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// vkManagerChannelID задаёт числовой идентификатор, полученный при успешном обнаружении.
const vkManagerChannelID = "9007199254740993"

// vkManagerAdapter подменяет границу DevAPI и WebSocket управляемыми ответами в памяти.
type vkManagerAdapter struct {
	// verify получает пару из ручного подключения или фонового выпуска app-токена.
	verify func(context.Context, string, string) (core.VKVideoLiveChatCredentials, error)
	// discover позволяет проверить токен, который Manager передал запросу канала.
	discover func(context.Context, core.ChatSource, core.ChatCredentials) (core.ChatDiscovery, error)
	// read позволяет удержать точную попытку чтения и проверить выданные ей полномочия.
	read func(context.Context, core.ChatReadRequest, core.ChatAdapterSink) error
}

// Provider регистрирует заглушку как адаптер чата VK Видео Live.
func (vkManagerAdapter) Provider() core.ChatProvider { return core.ChatVKVideoLive }

// VerifyCredentials направляет проверку реквизитов в сценарий verify в пределах ctx.
func (a vkManagerAdapter) VerifyCredentials(ctx context.Context, clientID, clientSecret string) (core.VKVideoLiveChatCredentials, error) {
	return a.verify(ctx, clientID, clientSecret)
}

// Discover возвращает ответ сценария discover для источника source и набора credentials.
// Без специально заданного ответа оба источника разрешаются в один фактический канал.
func (a vkManagerAdapter) Discover(ctx context.Context, source core.ChatSource, credentials core.ChatCredentials) (core.ChatDiscovery, error) {
	if a.discover != nil {
		return a.discover(ctx, source, credentials)
	}
	return vkManagerDiscovered(), nil
}

// Read предоставляет сценарию точные request и sink, ограниченные временем жизни ctx.
func (a vkManagerAdapter) Read(ctx context.Context, request core.ChatReadRequest, sink core.ChatAdapterSink) error {
	return a.read(ctx, request, sink)
}

// vkManagerDiscovered возвращает успешное разрешение постоянного канала тестового источника.
func vkManagerDiscovered() core.ChatDiscovery {
	return core.ChatDiscovery{ChannelID: vkManagerChannelID, State: core.ChatConnected}
}

// vkManagerCredentials строит полный синтетический набор для заданных реквизитов, токена и срока.
// Срок сохраняется в точности, поддерживаемой SQLite.
func vkManagerCredentials(clientID, clientSecret, token string, expiry time.Time) core.VKVideoLiveChatCredentials {
	return core.VKVideoLiveChatCredentials{
		ClientID: clientID, ClientSecret: clientSecret, AccessToken: token, ExpiresAt: expiry.UTC().Truncate(time.Microsecond),
	}
}

// vkManagerIssue создаёт проверенный синтетический токен для фиксированного тестового приложения.
func vkManagerIssue(token string) core.VKVideoLiveChatCredentials {
	return vkManagerCredentials("vk-test-client", "vk-test-secret", token, time.Now().Add(time.Hour))
}

// newVKManagerTest запускает Manager с временной SQLite и одним общим HTTP-слотом.
// Одно место в окне повторов позволяет отличить восстановление текущей попытки от её перезапуска.
func newVKManagerTest(t *testing.T, ctx context.Context, fixture chatManagerTestFixture, adapter vkManagerAdapter) *Manager {
	t.Helper()
	// limits выделяет один слот для HTTP и немедленную публикацию одного сообщения в Hub.
	limits := core.DefaultChatLimits()
	limits.HTTPRequests = 1
	limits.ReconnectAttempts = 1
	limits.ReconnectWindow = time.Minute
	limits.FlushMessages = 1
	// manager получает реальные хранилище и медиадопуск. err сообщает об отказе запуска.
	manager, err := New(ctx, fixture.service, fixture.store, limits, []core.ChatAdapterPort{adapter})
	chatTestOK(t, err)
	// Очистка останавливает и дожидается работ Manager до закрытия временной базы.
	t.Cleanup(func() {
		// cleanup и cancel выделяют независимый срок для штатного завершения.
		cleanup, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		chatTestOK(t, manager.Close(cleanup))
	})
	return manager
}

// vkManagerBindSource сохраняет настройки VK для уже существующего назначения фикстуры.
func vkManagerBindSource(t *testing.T, ctx context.Context, fixture chatManagerTestFixture, index int, account core.ID) core.ChatSource {
	t.Helper()
	// source сохраняет прежние ID источника и назначения для действующего медианаблюдателя.
	source := fixture.sources[index]
	// options и err получают и проверяют закрытый вариант настроек VK Видео Live.
	options, err := core.NewChatOptions(core.ChatVKVideoLive)
	chatTestOK(t, err)
	source.Provider, source.AccountID, source.ChannelHint, source.Options = core.ChatVKVideoLive, account, "synthetic-live", options
	// stored содержит новую ревизию того же источника, а err сообщает результат CAS.
	stored, err := fixture.service.UpdateChatSource(ctx, source, source.Revision)
	chatTestOK(t, err)
	return stored
}

// vkManagerSeedAccount сохраняет заранее заданный набор, чтобы тест мог воспроизвести
// уже существующий аккаунт с истёкшим или ещё действующим токеном.
func vkManagerSeedAccount(t *testing.T, ctx context.Context, fixture chatManagerTestFixture, value core.VKVideoLiveChatCredentials) core.ChatCredentialResult {
	t.Helper()
	// result содержит конфигурацию и начальную квитанцию, а err сообщает об отказе записи.
	result, err := fixture.service.CreateChatAccount(ctx, core.ChatAccount{Provider: core.ChatVKVideoLive, Name: "Тестовый VK"}, core.ChatCredentialBundle{VKVideoLive: &value}, "vk-seed-write")
	chatTestOK(t, err)
	return result
}

// vkManagerReader позволяет тесту наблюдать один физический вызов Read и его освобождение.
type vkManagerReader struct {
	// request содержит исходную квитанцию, токен попытки и ограничители Manager.
	request core.ChatReadRequest
	// sink передаёт сообщения и управляющие вызовы этой попытки.
	sink core.ChatAdapterSink
	// stopped закрывается при выходе из Read после отмены его контекста.
	stopped <-chan struct{}
}

// vkManagerKeepReader передаёт running начавшуюся попытку и удерживает её до отмены ctx.
// Сценарий сам выбирает момент подтверждения подписки через переданный sink.
func vkManagerKeepReader(ctx context.Context, request core.ChatReadRequest, sink core.ChatAdapterSink, running chan<- vkManagerReader) error {
	// stopped сообщает о физическом выходе из операции после отмены.
	stopped := make(chan struct{})
	defer close(stopped)
	select {
	case running <- vkManagerReader{request: request, sink: sink, stopped: stopped}:
	case <-ctx.Done():
		return ctx.Err()
	}
	<-ctx.Done()
	return ctx.Err()
}

// publish передаёт одно допустимое сообщение id от исходной попытки VK.
func (r vkManagerReader) publish(ctx context.Context, id string) error {
	// message использует тот же фактический идентификатор, что и исходная подписка.
	message := chatTestIncoming(id)
	message.Provider, message.ChannelID = core.ChatVKVideoLive, r.request.Key.ChannelID
	return r.sink.Message(ctx, r.request.Token, message)
}

// vkManagerMessage ждёт конкретное сообщение и проверяет его атрибуцию источнику source.
// Возвращённый снимок позволяет проверить отсутствие повторов на той же публичной ленте.
func vkManagerMessage(t *testing.T, ctx context.Context, manager *Manager, stream, source core.ID, id string) core.ChatSnapshot {
	t.Helper()
	// image содержит историю по достижении ожидаемой позиции. Предикат ищет строго исходный ID.
	image := waitChatTestSnapshot(t, ctx, manager, stream, func(snapshot core.ChatSnapshot) bool {
		// Предикат сравнивает платформенные ID, одинаковые для разных привязок одного чата.
		return slices.ContainsFunc(snapshot.Messages, func(message core.ChatMessage) bool {
			return message.ProviderMessageID == id
		})
	})
	// index находит принятую запись для проверки её платформы и источника.
	index := slices.IndexFunc(image.Messages, func(message core.ChatMessage) bool {
		// Предикат выбирает ту же запись, по которой завершилось ожидание снимка.
		return message.ProviderMessageID == id
	})
	// message представляет опубликованную Hub запись с присвоенной атрибуцией.
	message := image.Messages[index]
	if message.ChatSourceID != source || message.Provider != core.ChatVKVideoLive || message.ChannelID != vkManagerChannelID {
		t.Fatalf("Сообщение %q отнесено к другим платформе, каналу или источнику", id)
	}
	return image
}

// vkManagerConnectResult сохраняет публичный результат асинхронного подключения.
type vkManagerConnectResult struct {
	// account содержит метаданные подтверждённой записи без app-токена.
	account core.ChatAccount
	// reason содержит безопасную причину отказа.
	reason core.ChatReason
	// err сообщает исход подключения.
	err error
}

// TestVKVideoLiveManagerVerifiesBeforeSavingAndWaitsForMedia проверяет отсутствие
// записи до верификации, полный результат в SQLite и ожидание первого PUBLISHING.
func TestVKVideoLiveManagerVerifiesBeforeSavingAndWaitsForMedia(t *testing.T) {
	// ctx и cancel ограничивают все шаги, включая ожидание остановки заглушки.
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	// fixture содержит Media-квитанции без первого наблюдения PUBLISHING.
	fixture := newChatManagerTestFixture(t, ctx)
	// verified хранит проверенный результат. verifyStarted и allowVerify разделяют начало и принятие проверки.
	verified := vkManagerIssue("vk-first-token")
	verifyStarted, allowVerify := make(chan struct{}, 1), make(chan struct{})
	// discoveries и readers отслеживают факт обнаружения и единственную попытку чтения.
	discoveries, readers := make(chan core.ChatCredentials, 4), make(chan vkManagerReader, 4)
	// adapter проверяет входную пару и отделяет её проверку от запуска получателя.
	adapter := vkManagerAdapter{
		// Проверка задерживает подтверждение, позволяя одновременно прочитать временную SQLite.
		verify: func(ctx context.Context, clientID, clientSecret string) (core.VKVideoLiveChatCredentials, error) {
			if clientID != verified.ClientID || clientSecret != verified.ClientSecret {
				return core.VKVideoLiveChatCredentials{}, core.ErrInvalid
			}
			verifyStarted <- struct{}{}
			select {
			case <-allowVerify:
				return verified, nil
			case <-ctx.Done():
				return core.VKVideoLiveChatCredentials{}, ctx.Err()
			}
		},
		// Поиск передаёт набор, фактически использованный для получения канала.
		discover: func(ctx context.Context, _ core.ChatSource, credentials core.ChatCredentials) (core.ChatDiscovery, error) {
			select {
			case discoveries <- credentials:
				return vkManagerDiscovered(), nil
			case <-ctx.Done():
				return core.ChatDiscovery{}, ctx.Err()
			}
		},
		// Читатель удерживает подписку до отмены Manager.
		read: func(ctx context.Context, request core.ChatReadRequest, sink core.ChatAdapterSink) error {
			return vkManagerKeepReader(ctx, request, sink, readers)
		},
	}
	// manager обслуживает тестовый поток с изолированным адаптером.
	manager := newVKManagerTest(t, ctx, fixture, adapter)
	// result передаёт исход подключения, ожидающего управляемого ответа.
	result := make(chan vkManagerConnectResult, 1)
	// Горутина завершает подключение после подтверждения либо отмены ctx.
	go func() {
		// account, reason и err содержат публичный результат проверки и сохранения.
		account, reason, err := manager.ConnectVKVideoLive(ctx, VKVideoLiveConnectRequest{ClientID: verified.ClientID, ClientSecret: verified.ClientSecret, Name: "Проверенный VK"})
		result <- vkManagerConnectResult{account: account, reason: reason, err: err}
	}()
	chatTestReceive(t, ctx, verifyStarted)
	// accounts показывает текущее состояние до ответа проверки, err сообщает об отказе чтения.
	accounts, err := fixture.service.ListChatAccounts(ctx, core.ChatAccountQuery{Provider: core.ChatVKVideoLive, Limit: 10})
	chatTestOK(t, err)
	if len(accounts) != 0 {
		t.Fatal("Аккаунт VK появился до подтверждения выдачи app-токена")
	}
	close(allowVerify)
	// connected содержит подтверждённый аккаунт, но без данных для входа.
	connected := chatTestReceive(t, ctx, result)
	chatTestOK(t, connected.err)
	if connected.account.ID == "" || connected.account.Provider != core.ChatVKVideoLive || connected.account.Name != "Проверенный VK" || connected.reason != core.ChatReasonNone {
		t.Fatal("Подключение не вернуло подтверждённые публичные метаданные аккаунта VK")
	}
	// stored позволяет проверить полную квитанцию. err сообщает об отказе чтения SQLite.
	stored, err := fixture.service.GetChatCredentialResult(ctx, connected.account.ID)
	chatTestOK(t, err)
	if stored.Account != connected.account || stored.Credentials.Bundle.VKVideoLive == nil || *stored.Credentials.Bundle.VKVideoLive != verified {
		t.Fatal("SQLite не содержит полный проверенный набор VK")
	}
	// source привязывает проверенный аккаунт к выходу, который ещё не передавал медиа.
	source := vkManagerBindSource(t, ctx, fixture, 0, connected.account.ID)
	// ApplySource синхронизирует новую конфигурацию с работающим Manager до проверки медиадопуска.
	_, err = manager.ApplySource(ctx, source.ID, source.Revision)
	chatTestOK(t, err)
	waitChatTestSnapshot(t, ctx, manager, fixture.stream.ID, func(image core.ChatSnapshot) bool {
		// Предикат ждёт согласованного назначения платформы при отсутствии PUBLISHING.
		return chatTestHasState(image, source.ID, core.ChatWaitingOutput)
	})
	select {
	case <-discoveries:
		t.Fatal("Обнаружение чата VK началось до фактической публикации медиа")
	case <-readers:
		t.Fatal("Чтение чата VK началось до фактической публикации медиа")
	default:
	}
	chatTestOK(t, fixture.outputs[0].Observe(ctx, core.OutputObservation{State: core.OutputPublishing}))
	// used подтверждает, что поиск получил сохранённый набор без перевыпуска.
	used := chatTestReceive(t, ctx, discoveries)
	if used.CredentialWriteID != stored.Credentials.CredentialWriteID || used.CredentialRevision != stored.Credentials.CredentialRevision {
		t.Fatal("Поиск не переиспользовал проверенный действующий app-токен")
	}
	// reader подтверждает переход к чтению именно обнаруженного канала.
	reader := chatTestReceive(t, ctx, readers)
	if reader.request.Key.ChannelID != vkManagerChannelID || reader.request.Key.AccountID != connected.account.ID ||
		reader.request.Credentials.CredentialWriteID != stored.Credentials.CredentialWriteID {
		t.Fatal("Чтение получило другой канал, аккаунт или набор полномочий")
	}
	chatTestOK(t, reader.sink.Status(ctx, reader.request.Token, core.ChatConnected, core.ChatReasonNone))
	chatTestOK(t, reader.publish(ctx, "first-vk-message"))
	vkManagerMessage(t, ctx, manager, fixture.stream.ID, source.ID, "first-vk-message")
	select {
	case <-verifyStarted:
		t.Fatal("Действующий app-токен вызвал лишний выпуск при начале чтения")
	default:
	}
	chatTestOK(t, manager.Close(ctx))
	chatTestReceive(t, ctx, reader.stopped)
	if err = reader.publish(ctx, "after-close"); !errors.Is(err, core.ErrStale) {
		t.Fatalf("После остановки Manager ожидался отзыв попытки VK, получено %v", err)
	}
}
