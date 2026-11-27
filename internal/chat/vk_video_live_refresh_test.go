package chat

import (
	"context"
	"errors"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// vkManagerSavedToken сравнивает перевыпущенный набор из SQLite с исходной квитанцией и ожидаемым результатом.
func vkManagerSavedToken(t *testing.T, ctx context.Context, fixture chatManagerTestFixture, initial core.ChatCredentialResult, want core.VKVideoLiveChatCredentials) core.ChatCredentials {
	t.Helper()
	// actual содержит обе долговечные ревизии. err сообщает об отказе чтения SQLite.
	actual, err := fixture.service.GetChatCredentialResult(ctx, initial.Account.ID)
	chatTestOK(t, err)
	if actual.Account != initial.Account || actual.Credentials.CredentialRevision != initial.Credentials.CredentialRevision+1 ||
		actual.Credentials.CredentialWriteID == initial.Credentials.CredentialWriteID ||
		actual.Credentials.Bundle.VKVideoLive == nil || *actual.Credentials.Bundle.VKVideoLive != want {
		t.Fatal("Перевыпуск должен заменить квитанцию и весь набор ровно один раз при прежней конфигурационной ревизии")
	}
	return actual.Credentials
}

// TestVKVideoLiveManagerRefreshesExpiredAndRejectedTokens проверяет истечение, ранний отказ
// и повторное отклонение Bearer. Один слот HTTP и одна попытка в окне исключают новый цикл Manager.
func TestVKVideoLiveManagerRefreshesExpiredAndRejectedTokens(t *testing.T) {
	// trigger задаёт, какой этап впервые узнает о непригодности исходного токена.
	for _, trigger := range []string{"expired", "discovery-401", "reader-401", "discovery-repeated-401"} {
		// Подтест отделяет срок жизни, ранний отказ обнаружения и ранний отказ получателя.
		t.Run(trigger, func(t *testing.T) {
			// ctx и cancel ограничивают операции с настоящей SQLite и ожидание публикации.
			ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
			defer cancel()
			// fixture создаёт источники и медианаблюдателей без запуска внешних процессов.
			fixture := newChatManagerTestFixture(t, ctx)
			// original и renewed задают два различимых подтверждённых результата выдачи.
			original, renewed := vkManagerIssue("vk-original-token"), vkManagerIssue("vk-renewed-token")
			if trigger == "expired" {
				original.ExpiresAt = time.Now().Add(-time.Hour).UTC().Truncate(time.Microsecond)
			}
			// initial связывает оригинальный токен с настоящей долговечной квитанцией.
			initial := vkManagerSeedAccount(t, ctx, fixture, original)
			// source задаёт источник, который должен пройти обнаружение с актуальным токеном.
			source := vkManagerBindSource(t, ctx, fixture, 0, initial.Account.ID)
			// issues считает обращения к выпуску. discovers отмечает полученные поиском Bearer.
			var issues atomic.Int32
			discovers := make(chan string, 4)
			// readers получает только готовый результат внутри исходной попытки чтения.
			readers := make(chan vkManagerReader, 4)
			// adapter моделирует отклонение Bearer на конкретной границе.
			adapter := vkManagerAdapter{
				// Проверка отличает перевыпуск от сохранения нового исходного аккаунта.
				verify: func(_ context.Context, clientID, clientSecret string) (core.VKVideoLiveChatCredentials, error) {
					issues.Add(1)
					if clientID != original.ClientID || clientSecret != original.ClientSecret {
						return core.VKVideoLiveChatCredentials{}, core.ErrInvalid
					}
					return renewed, nil
				},
				// Поиск возвращает 401 за исходный Bearer либо за оба Bearer в отрицательном сценарии.
				discover: func(ctx context.Context, _ core.ChatSource, credentials core.ChatCredentials) (core.ChatDiscovery, error) {
					// token хранит Bearer конкретного запроса к каналу.
					token := credentials.Bundle.VKVideoLive.AccessToken
					select {
					case discovers <- token:
					case <-ctx.Done():
						return core.ChatDiscovery{}, ctx.Err()
					}
					if trigger == "discovery-repeated-401" || trigger == "discovery-401" && token == original.AccessToken {
						return core.ChatDiscovery{}, core.ErrChatAppTokenRejected
					}
					return vkManagerDiscovered(), nil
				},
				// Читатель моделирует освобождение слота запроса WS-токена после 401 и повтор с новым Bearer.
				read: func(ctx context.Context, request core.ChatReadRequest, sink core.ChatAdapterSink) error {
					if trigger == "reader-401" {
						if request.Credentials.CredentialWriteID != initial.Credentials.CredentialWriteID || request.RefreshRejectedCredentials == nil {
							return core.ErrInvalid
						}
						// release и err сообщают допуск исходного конечного запроса перед его отказом.
						release, err := request.AcquireHTTP(ctx)
						if err != nil {
							return err
						}
						release()
						// fresh содержит набор после 401. err сообщает об отказе восстановления в текущем Read.
						fresh, err := request.RefreshRejectedCredentials(ctx)
						if err != nil {
							return err
						}
						if fresh.Bundle.VKVideoLive == nil || fresh.Bundle.VKVideoLive.AccessToken != renewed.AccessToken ||
							fresh.CredentialWriteID == initial.Credentials.CredentialWriteID {
							return core.ErrInvalid
						}
						// Повторный запрос WS-токена должен снова получить тот же общий слот HTTP.
						release, err = request.AcquireHTTP(ctx)
						if err != nil {
							return err
						}
						release()
					}
					return vkManagerKeepReader(ctx, request, sink, readers)
				},
			}
			// manager связывает владельца полномочий с действительными медиадоказательствами.
			manager := newVKManagerTest(t, ctx, fixture, adapter)
			chatTestOK(t, fixture.outputs[0].Observe(ctx, core.OutputObservation{State: core.OutputPublishing}))
			// firstToken позволяет заметить запрос с уже истёкшими полномочиями.
			firstToken := chatTestReceive(t, ctx, discovers)
			if trigger == "expired" && firstToken != renewed.AccessToken || trigger != "expired" && firstToken != original.AccessToken {
				t.Fatal("Обнаружение использовало неожиданный исходный токен")
			}
			if trigger == "discovery-401" || trigger == "discovery-repeated-401" {
				if chatTestReceive(t, ctx, discovers) != renewed.AccessToken {
					t.Fatal("Повтор обнаружения после 401 не использовал подтверждённый новый токен")
				}
			}
			if trigger == "discovery-repeated-401" {
				waitChatTestSnapshot(t, ctx, manager, fixture.stream.ID, func(image core.ChatSnapshot) bool {
					// Предикат требует временный отказ для источника, поскольку 401 не отзывает пару приложения.
					return chatTestHasState(image, source.ID, core.ChatRetrying)
				})
				select {
				case <-readers:
					t.Fatal("Чтение началось после двух 401 при обнаружении")
				case <-discovers:
					t.Fatal("Два 401 запустили третий запрос в пределах одной попытки")
				default:
				}
			} else {
				// reader остаётся первой физической попыткой в этом окне повторов.
				reader := chatTestReceive(t, ctx, readers)
				chatTestOK(t, reader.sink.Status(ctx, reader.request.Token, core.ChatConnected, core.ChatReasonNone))
				chatTestOK(t, reader.publish(ctx, "message-after-renewal"))
				vkManagerMessage(t, ctx, manager, fixture.stream.ID, source.ID, "message-after-renewal")
			}
			vkManagerSavedToken(t, ctx, fixture, initial, renewed)
			if issues.Load() != 1 {
				t.Fatalf("Для одного непригодного токена ожидался один выпуск, получено %d", issues.Load())
			}
		})
	}
}

// TestVKVideoLiveManagerHonorsRetryAfter проверяет задержку следующего обнаружения
// после временного отказа перевыпуска или повторного отклонения Bearer.
func TestVKVideoLiveManagerHonorsRetryAfter(t *testing.T) {
	// testCase задаёт исход реактивного перевыпуска.
	for _, testCase := range []struct {
		name          string // name различает этапы временного отказа.
		failedRefresh bool   // failedRefresh отмечает ошибку выпуска после первого 401.
	}{
		{name: "second_rejection"},
		{name: "failed_refresh", failedRefresh: true},
	} {
		// Подтест сравнивает моменты двух плановых попыток обнаружения.
		t.Run(testCase.name, func(t *testing.T) {
			// ctx и cancel ограничивают ожидание после отказа и работу временной SQLite.
			ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
			defer cancel()
			// fixture предоставляет медиадопуск и долговечную квитанцию для проверки записи.
			fixture := newChatManagerTestFixture(t, ctx)
			// original и renewed позволяют проверить оба запроса одной попытки обнаружения.
			original, renewed := vkManagerIssue("vk-throttled-old"), vkManagerIssue("vk-throttled-new")
			// initial содержит действующий по времени токен, который площадка отклонит досрочно.
			initial := vkManagerSeedAccount(t, ctx, fixture, original)
			vkManagerBindSource(t, ctx, fixture, 0, initial.Account.ID)
			// retryAfter задаёт нижнюю границу площадки после повторного отказа.
			const retryAfter = 300 * time.Millisecond
			// limits допускает следующее обнаружение без ожидания лимита окна.
			limits := core.DefaultChatLimits()
			limits.HTTPRequests = 1
			limits.ReconnectAttempts = 2
			limits.ReconnectMinBackoff, limits.ReconnectMaxBackoff = time.Millisecond, time.Millisecond
			// discovers передаёт моменты запросов одной горутины обнаружения.
			discovers := make(chan time.Time, 3)
			// readers сообщает, что после задержки обнаружение действительно завершилось.
			readers := make(chan vkManagerReader, 1)
			// calls считает обращения к каналу внутри одной горутины. issues считает выпуски токена.
			calls := 0
			var issues atomic.Int32
			// adapter моделирует два отказа в одном цикле и успешную следующую попытку.
			adapter := vkManagerAdapter{
				// Проверка возвращает заданный исход перевыпуска по прежней паре реквизитов.
				verify: func(_ context.Context, clientID, clientSecret string) (core.VKVideoLiveChatCredentials, error) {
					issues.Add(1)
					if clientID != original.ClientID || clientSecret != original.ClientSecret {
						return core.VKVideoLiveChatCredentials{}, core.ErrInvalid
					}
					if testCase.failedRefresh {
						return core.VKVideoLiveChatCredentials{}, core.ErrNotReady
					}
					return renewed, nil
				},
				// Поиск отмечает каждый запрос и возвращает задержку с отклонённым Bearer.
				discover: func(_ context.Context, _ core.ChatSource, credentials core.ChatCredentials) (core.ChatDiscovery, error) {
					calls++
					discovers <- time.Now()
					if calls == 1 {
						if credentials.Bundle.VKVideoLive.AccessToken != original.AccessToken {
							t.Error("Первое обнаружение должно использовать исходный токен")
						}
						if testCase.failedRefresh {
							return core.ChatDiscovery{}, errors.Join(core.ErrChatAppTokenRejected, &core.ChatReadRetry{Reason: core.ChatReasonNetwork, RetryAfter: retryAfter})
						}
						return core.ChatDiscovery{}, core.ErrChatAppTokenRejected
					}
					if testCase.failedRefresh {
						if credentials.Bundle.VKVideoLive.AccessToken != original.AccessToken {
							t.Error("После временного отказа выпуска должен сохраниться исходный токен")
						}
						return vkManagerDiscovered(), nil
					}
					if credentials.Bundle.VKVideoLive.AccessToken != renewed.AccessToken {
						t.Error("Повтор должен использовать подтверждённый новый токен")
					}
					if calls == 2 {
						return core.ChatDiscovery{}, errors.Join(core.ErrChatAppTokenRejected, &core.ChatReadRetry{Reason: core.ChatReasonNetwork, RetryAfter: retryAfter})
					}
					return vkManagerDiscovered(), nil
				},
				// Читатель удерживает первую успешную подписку до закрытия Manager.
				read: func(ctx context.Context, request core.ChatReadRequest, sink core.ChatAdapterSink) error {
					return vkManagerKeepReader(ctx, request, sink, readers)
				},
			}
			startChatLifecycleManager(t, ctx, fixture, fixture.store, limits, adapter)
			chatTestOK(t, fixture.outputs[0].Observe(ctx, core.OutputObservation{State: core.OutputPublishing}))
			// rejected хранит момент запроса, для которого площадка указала задержку.
			rejected := chatTestReceive(t, ctx, discovers)
			if !testCase.failedRefresh {
				rejected = chatTestReceive(t, ctx, discovers)
			}
			// next хранит начало следующей плановой попытки.
			next := chatTestReceive(t, ctx, discovers)
			if next.Sub(rejected) < retryAfter {
				t.Fatalf("Обнаружение повторилось через %s при минимальной задержке %s", next.Sub(rejected), retryAfter)
			}
			chatTestReceive(t, ctx, readers)
			if testCase.failedRefresh {
				// stored содержит текущую квитанцию. err сообщает об отказе чтения после временного сбоя.
				stored, err := fixture.service.GetChatCredentialResult(ctx, initial.Account.ID)
				chatTestOK(t, err)
				if !stored.Matches(initial) {
					t.Fatal("Временный отказ выпуска изменил исходную квитанцию")
				}
			} else {
				vkManagerSavedToken(t, ctx, fixture, initial, renewed)
			}
			if issues.Load() != 1 {
				t.Fatalf("Ожидался один вызов восстановления после первого отказа токена, получено %d", issues.Load())
			}
		})
	}
}

// vkManagerRefreshResult содержит ответ восстановления и позволяет дождаться выхода горутины.
type vkManagerRefreshResult struct {
	// credentials содержит подтверждённую серверную квитанцию.
	credentials core.ChatCredentials
	// err сообщает отказ исходной попытки или ошибку выпуска.
	err error
}

// TestVKVideoLiveManagerCoalescesRejectedReceiptsAndFencesReplacement проверяет
// две реакции на одну квитанцию и запоздалую реакцию после подтверждённой ручной замены.
func TestVKVideoLiveManagerCoalescesRejectedReceiptsAndFencesReplacement(t *testing.T) {
	// ctx и cancel ограничивают конкурентные вызовы и их очистку.
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	// fixture использует приватную SQLite с настоящим credential CAS.
	fixture := newChatManagerTestFixture(t, ctx)
	// original, renewed и replacement различают исходный выпуск, восстановление и ручную замену.
	original := vkManagerIssue("vk-shared-old")
	renewed := vkManagerIssue("vk-shared-renewed")
	replacement := vkManagerCredentials("vk-replacement-client", "vk-replacement-secret", "vk-replacement-token", time.Now().Add(time.Hour))
	// initial связывает исходные реквизиты с конфигурационной и credential-ревизиями.
	initial := vkManagerSeedAccount(t, ctx, fixture, original)
	vkManagerBindSource(t, ctx, fixture, 0, initial.Account.ID)
	// neighborAccount и neighborSource используют тот же фактический канал с собственными реквизитами.
	neighborAccount := vkManagerSeedAccount(t, ctx, fixture, vkManagerIssue("vk-neighbor-token"))
	neighborSource := vkManagerBindSource(t, ctx, fixture, 1, neighborAccount.Account.ID)
	// issued считает выпуски старой пары. entered и release перекрывают две реакции на один 401.
	var issued atomic.Int32
	entered, release := make(chan struct{}, 2), make(chan struct{})
	// readers предоставляет точные попытки до и после ручной замены.
	readers := make(chan vkManagerReader, 4)
	// adapter различает реактивный выпуск и верификацию новой пары приложения.
	adapter := vkManagerAdapter{
		// Проверка блокирует только повторный выпуск прежней пары до готовности обеих реакций.
		verify: func(ctx context.Context, clientID, clientSecret string) (core.VKVideoLiveChatCredentials, error) {
			switch {
			case clientID == original.ClientID && clientSecret == original.ClientSecret:
				issued.Add(1)
				entered <- struct{}{}
				select {
				case <-release:
					return renewed, nil
				case <-ctx.Done():
					return core.VKVideoLiveChatCredentials{}, ctx.Err()
				}
			case clientID == replacement.ClientID && clientSecret == replacement.ClientSecret:
				return replacement, nil
			default:
				return core.VKVideoLiveChatCredentials{}, core.ErrInvalid
			}
		},
		// Читатель позволяет вызывать восстановление с точной исходной квитанцией.
		read: func(ctx context.Context, request core.ChatReadRequest, sink core.ChatAdapterSink) error {
			return vkManagerKeepReader(ctx, request, sink, readers)
		},
	}
	// manager подключает живой выход к единственному читателю.
	manager := newVKManagerTest(t, ctx, fixture, adapter)
	chatTestOK(t, fixture.outputs[0].Observe(ctx, core.OutputObservation{State: core.OutputPublishing}))
	// old удерживает первую подписку на протяжении двух одновременных реакций.
	old := chatTestReceive(t, ctx, readers)
	if old.request.RefreshRejectedCredentials == nil || old.request.Credentials.CredentialWriteID != initial.Credentials.CredentialWriteID {
		t.Fatal("Попытка чтения не получила восстановление исходного набора")
	}
	chatTestOK(t, fixture.outputs[2].Observe(ctx, core.OutputObservation{State: core.OutputPublishing}))
	// neighbor остаётся отдельной попыткой того же канала благодаря собственному аккаунту.
	neighbor := chatTestReceive(t, ctx, readers)
	if neighbor.request.Key.AccountID != neighborAccount.Account.ID || neighbor.request.Key.ChannelID != old.request.Key.ChannelID {
		t.Fatal("Соседний аккаунт не получил независимое чтение того же чата")
	}
	chatTestOK(t, neighbor.sink.Status(ctx, neighbor.request.Token, core.ChatConnected, core.ChatReasonNone))
	// ready сигнализирует о начале каждой реакции, results передаёт результаты обоих вызовов.
	ready, results := make(chan struct{}, 2), make(chan vkManagerRefreshResult, 2)
	for range 2 {
		// Горутина использует одну и ту же исходную квитанцию, как повторный отказ Bearer.
		go func() {
			ready <- struct{}{}
			// credentials и err сообщают какой набор получили сейчас под общим допуском аккаунта.
			credentials, err := old.request.RefreshRejectedCredentials(ctx)
			results <- vkManagerRefreshResult{credentials: credentials, err: err}
		}()
	}
	chatTestReceive(t, ctx, ready)
	chatTestReceive(t, ctx, ready)
	chatTestReceive(t, ctx, entered)
	close(release)
	// first и second содержат результаты для одного и того же исходного отказа.
	first, second := chatTestReceive(t, ctx, results), chatTestReceive(t, ctx, results)
	chatTestOK(t, first.err)
	chatTestOK(t, second.err)
	// stored — фактическая единственная запись реактивного перевыпуска в SQLite.
	stored := vkManagerSavedToken(t, ctx, fixture, initial, renewed)
	if issued.Load() != 1 || first.credentials.CredentialWriteID != stored.CredentialWriteID ||
		second.credentials.CredentialWriteID != stored.CredentialWriteID ||
		first.credentials.CredentialRevision != stored.CredentialRevision || second.credentials.CredentialRevision != stored.CredentialRevision {
		t.Fatal("Реакции на исходную квитанцию не сошлись к одному подтверждённому перевыпуску")
	}

	// account и reason описывают подтверждённую ручную замену, err сообщает об отказе.
	account, reason, err := manager.ConnectVKVideoLive(ctx, VKVideoLiveConnectRequest{
		AccountID: initial.Account.ID, ExpectedRevision: initial.Account.Revision,
		ClientID: replacement.ClientID, ClientSecret: replacement.ClientSecret,
	})
	chatTestOK(t, err)
	if account.ID != initial.Account.ID || account.Revision != initial.Account.Revision+1 || reason != core.ChatReasonNone {
		t.Fatal("Ручная замена не вернула следующую конфигурационную ревизию аккаунта")
	}
	chatTestReceive(t, ctx, old.stopped)
	// late содержит результат запоздалого чтения. err должен подтвердить отзыв попытки.
	late, err := old.request.RefreshRejectedCredentials(ctx)
	if err == nil || late.AccountID != "" {
		t.Fatal("Отозванная попытка получила полномочия после ручной замены")
	}
	// current получает новый читатель после завершения старого.
	current := chatTestReceive(t, ctx, readers)
	// final содержит последнюю запись. err сообщает об отказе чтения SQLite после ручной замены.
	final, err := fixture.service.GetChatCredentialResult(ctx, account.ID)
	chatTestOK(t, err)
	if issued.Load() != 1 || final.Account != account || final.Credentials.CredentialRevision != stored.CredentialRevision+1 ||
		final.Credentials.Bundle.VKVideoLive == nil || *final.Credentials.Bundle.VKVideoLive != replacement ||
		current.request.Credentials.CredentialWriteID != final.Credentials.CredentialWriteID ||
		current.request.Token == old.request.Token {
		t.Fatal("После ручной замены ожидались новые полномочия и попытка без повторного выпуска старой пары")
	}
	chatTestOK(t, neighbor.publish(ctx, "neighbor-during-replacement"))
	vkManagerMessage(t, ctx, manager, fixture.stream.ID, neighborSource.ID, "neighbor-during-replacement")
	// neighborStored содержит полную квитанцию отдельного аккаунта. err сообщает об отказе её чтения.
	neighborStored, err := fixture.service.GetChatCredentialResult(ctx, neighborAccount.Account.ID)
	chatTestOK(t, err)
	if !neighborStored.Matches(neighborAccount) {
		t.Fatal("Замена одного аккаунта изменила квитанцию соседнего аккаунта")
	}
}

// TestVKVideoLiveManagerFailedReplacementKeepsSharedReader проверяет, что внешний отказ,
// отмена и конфликт CAS сохраняют доступ прежнего читателя и обеих его привязок.
func TestVKVideoLiveManagerFailedReplacementKeepsSharedReader(t *testing.T) {
	// ctx и cancel ограничивают сценарий двух источников одной беседы.
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	// fixture предоставляет два назначения с раздельными наблюдателями.
	fixture := newChatManagerTestFixture(t, ctx)
	// initial содержит подтверждённый аккаунт с пригодным Bearer для обоих назначений.
	initial := vkManagerSeedAccount(t, ctx, fixture, vkManagerIssue("vk-shared-valid"))
	// first и neighbor используют тот же аккаунт и один фактический канал.
	first, neighbor := vkManagerBindSource(t, ctx, fixture, 0, initial.Account.ID), vkManagerBindSource(t, ctx, fixture, 1, initial.Account.ID)
	// readers получает каждую физическую попытку. verifyStarted и allowConflictCheck управляют длительными проверками.
	readers, verifyStarted, allowConflictCheck := make(chan vkManagerReader, 4), make(chan struct{}, 1), make(chan struct{})
	// adapter позволяет отличить подтверждённое отклонение пары от отменённой проверки.
	adapter := vkManagerAdapter{
		// Проверка моделирует отказ, отмену и успешное завершение после параллельного переименования.
		verify: func(ctx context.Context, clientID, clientSecret string) (core.VKVideoLiveChatCredentials, error) {
			switch clientID {
			case "vk-cancelled-client":
				verifyStarted <- struct{}{}
				<-ctx.Done()
				return core.VKVideoLiveChatCredentials{}, ctx.Err()
			case "vk-conflict-client":
				verifyStarted <- struct{}{}
				select {
				case <-allowConflictCheck:
					return vkManagerCredentials(clientID, clientSecret, "vk-conflict-token", time.Now().Add(time.Hour)), nil
				case <-ctx.Done():
					return core.VKVideoLiveChatCredentials{}, ctx.Err()
				}
			default:
				return core.VKVideoLiveChatCredentials{}, core.ErrInvalid
			}
		},
		// Читатель сохраняет общую подписку до отзыва последнего участника.
		read: func(ctx context.Context, request core.ChatReadRequest, sink core.ChatAdapterSink) error {
			return vkManagerKeepReader(ctx, request, sink, readers)
		},
	}
	// manager управляет допуском источников независимо от результата запрошенной замены.
	manager := newVKManagerTest(t, ctx, fixture, adapter)
	chatTestOK(t, fixture.outputs[0].Observe(ctx, core.OutputObservation{State: core.OutputPublishing}))
	chatTestOK(t, fixture.outputs[2].Observe(ctx, core.OutputObservation{State: core.OutputPublishing}))
	// reader содержит единственный токен физической попытки для двух источников.
	reader := chatTestReceive(t, ctx, readers)
	chatTestOK(t, reader.sink.Status(ctx, reader.request.Token, core.ChatConnected, core.ChatReasonNone))
	waitChatTestSnapshot(t, ctx, manager, fixture.stream.ID, func(image core.ChatSnapshot) bool {
		// Предикат ждёт присоединения обоих участников к подтверждённой подписке.
		return chatTestHasState(image, first.ID, core.ChatConnected) && chatTestHasState(image, neighbor.ID, core.ChatConnected)
	})
	// request указывает существующий аккаунт для двух неудачных операций.
	request := VKVideoLiveConnectRequest{AccountID: initial.Account.ID, ExpectedRevision: initial.Account.Revision, ClientID: "vk-invalid-client", ClientSecret: "vk-invalid-secret"}
	// denied, reason и err отделяют ожидаемый отказ пары от успешной записи.
	denied, reason, err := manager.ConnectVKVideoLive(ctx, request)
	if err == nil || denied.ID != "" || reason != core.ChatReasonCredentials {
		t.Fatalf("Отклонённая пара не дала ожидаемый публичный отказ: %s, %v", reason, err)
	}
	request.ClientID = "vk-cancelled-client"
	// pending и abort ограничивают отмену одной проверки, пока публикация продолжается.
	pending, abort := context.WithCancel(ctx)
	defer abort()
	// cancelled передаёт завершение попытки без изменения исходного аккаунта.
	cancelled := make(chan vkManagerConnectResult, 1)
	// Горутина оставляет проверку пары открытой, пока тест подтверждает поступление сообщения.
	go func() {
		// account, reason и err хранят публичный результат отменяемой операции.
		account, reason, err := manager.ConnectVKVideoLive(pending, request)
		cancelled <- vkManagerConnectResult{account: account, reason: reason, err: err}
	}()
	chatTestReceive(t, ctx, verifyStarted)
	chatTestOK(t, reader.publish(ctx, "during-replacement-verification"))
	vkManagerMessage(t, ctx, manager, fixture.stream.ID, first.ID, "during-replacement-verification")
	abort()
	// result фиксирует фактическое завершение отменённой операции.
	result := chatTestReceive(t, ctx, cancelled)
	if !errors.Is(result.err, context.Canceled) || result.account.ID != "" {
		t.Fatalf("Отмена проверки не вернула ожидаемый отказ: %v", result.err)
	}
	// after содержит обе ревизии и квитанцию после отмены. err сообщает об отказе их чтения.
	after, err := fixture.service.GetChatCredentialResult(ctx, initial.Account.ID)
	chatTestOK(t, err)
	if !after.Matches(initial) {
		t.Fatal("Неудачная замена изменила конфигурацию или полномочия аккаунта")
	}

	request.ClientID = "vk-conflict-client"
	// conflicted возвращает результат замены, пока параллельный CAS меняет config revision.
	conflicted := make(chan vkManagerConnectResult, 1)
	// Горутина передаёт проверенную пару после переименования аккаунта по исходной ревизии.
	go func() {
		// account, reason и err хранят результат применения проверки к устаревшей ревизии.
		account, reason, err := manager.ConnectVKVideoLive(ctx, request)
		conflicted <- vkManagerConnectResult{account: account, reason: reason, err: err}
	}()
	chatTestReceive(t, ctx, verifyStarted)
	// rename копирует лишь метаданные, чтобы независимый CAS не изменил серверную квитанцию.
	rename := initial.Account
	rename.Name = "Переименованный VK"
	rename, err = fixture.service.UpdateChatAccount(ctx, rename, rename.Revision)
	chatTestOK(t, err)
	close(allowConflictCheck)
	result = chatTestReceive(t, ctx, conflicted)
	if !errors.Is(result.err, core.ErrConflict) || result.account.ID != "" {
		t.Fatalf("Замена по устаревшей ревизии не вернула конфликт: %v", result.err)
	}
	after, err = fixture.service.GetChatCredentialResult(ctx, initial.Account.ID)
	chatTestOK(t, err)
	if !after.Matches(core.ChatCredentialResult{Account: rename, Credentials: initial.Credentials}) {
		t.Fatal("Конфликт замены потерял новое имя или изменил исходные полномочия")
	}

	first.Enabled = false
	// Функция меняет только первый источник через штатную конфигурационную транзакцию.
	_, err = manager.MutateSource(ctx, first.ID, func(ctx context.Context) (core.ChatSource, error) {
		return fixture.service.UpdateChatSource(ctx, first, first.Revision)
	})
	chatTestOK(t, err)
	chatTestOK(t, reader.publish(ctx, "after-first-source-revoked"))
	// image содержит сообщения двух фаз и состояние оставшегося участника.
	image := vkManagerMessage(t, ctx, manager, fixture.stream.ID, neighbor.ID, "after-first-source-revoked")
	if len(image.Messages) != 2 || !chatTestHasState(image, neighbor.ID, core.ChatConnected) {
		t.Fatal("Отзыв одного источника нарушил общую историю или состояние оставшегося участника")
	}
	select {
	case <-readers:
		t.Fatal("Обе привязки должны использовать одну физическую попытку")
	case <-reader.stopped:
		t.Fatal("Первый отзыв остановил общего читателя при действующем соседе")
	default:
	}
	// Команда прекращает единственный подходящий выход последнего участника.
	_, err = fixture.service.StopOutput(ctx, core.OutputCommand{RequestID: "vk-test-stop-neighbor", OutputID: fixture.outputs[2].ExecutionFence().OutputID})
	chatTestOK(t, err)
	chatTestReceive(t, ctx, reader.stopped)
	if err = reader.publish(ctx, "late-vk-message"); !errors.Is(err, core.ErrStale) {
		t.Fatalf("Позднее сообщение после освобождения читателя получило %v", err)
	}
	// snapshot содержит текущую ленту после позднего вызова. err сообщает об отказе чтения.
	snapshot, err := manager.Snapshot(ctx, fixture.stream.ID)
	chatTestOK(t, err)
	if slices.ContainsFunc(snapshot.Messages, func(message core.ChatMessage) bool {
		// Предикат ищет сообщение, отправленное уже после освобождения последней привязки.
		return message.ProviderMessageID == "late-vk-message"
	}) {
		t.Fatal("В ленте появилось сообщение от отозванного читателя")
	}
}
