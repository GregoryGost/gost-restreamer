package chat

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// twitchReaderFixture выдаёт синтетическую подписку и отличает одну ротацию от проверок новой пары.
type twitchReaderFixture struct {
	// chatTestAdapter передаёт тесту точный допуск чтения и сигнал физического завершения.
	chatTestAdapter
	// exchanges считает обмен исходного набора полномочий.
	exchanges atomic.Int32
	// checks считает сетевые проверки, которые Manager передаёт владельцу.
	checks atomic.Int32
}

// Provider выбирает зарегистрированный маршрут Twitch в Manager.
func (*twitchReaderFixture) Provider() core.ChatProvider { return core.ChatTwitch }

// Discover находит один чат собственного канала по ID из сохранённых credentials.
func (*twitchReaderFixture) Discover(_ context.Context, source core.ChatSource, credentials core.ChatCredentials) (core.ChatDiscovery, error) {
	if source.ChannelHint != "" || credentials.Bundle.Twitch == nil {
		return core.ChatDiscovery{}, core.ErrInvalid
	}
	return core.ChatDiscovery{ChannelID: credentials.Bundle.Twitch.UserID, State: core.ChatConnecting}, nil
}

// Refresh меняет только первую ревизию current, чтобы повторные обращения проверяли сохранённую пару.
func (a *twitchReaderFixture) Refresh(_ context.Context, current core.ChatCredentials) (*core.ChatCredentialBundle, bool, error) {
	a.checks.Add(1)
	if current.CredentialRevision != 1 {
		return nil, false, nil
	}
	a.exchanges.Add(1)
	// next представляет проверенный комплект, возвращённый единственным синтетическим обменом.
	next := credentialTestBundle("twitch-reader-rotated")
	return &next, true, nil
}

// TestTwitchReadUsesBoundOutputAndAccountOwner проверяет серверный путь от публикации привязанного
// выхода до подписки, повторной проверки полномочий и освобождения общих сетевых ресурсов.
func TestTwitchReadUsesBoundOutputAndAccountOwner(t *testing.T) {
	// ctx ограничивает сценарий на временной SQLite. cancel останавливает его оставшиеся операции.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// fixture содержит точные наблюдатели двух назначений, настроенных на одном Media Stream.
	fixture := newChatManagerTestFixture(t, ctx)
	// account хранит тестовую пару Twitch, err сообщает о результатах подготовки реальных записей.
	account, err := fixture.service.CreateChatAccount(ctx, core.ChatAccount{Provider: core.ChatTwitch}, credentialTestBundle("twitch-reader-original"), "initial-twitch-reader")
	chatTestOK(t, err)
	// options соответствует Twitch и выбирает чтение собственного канала с пустым ChannelHint.
	options, err := core.NewChatOptions(core.ChatTwitch)
	chatTestOK(t, err)
	// source привязывает Twitch к первому назначению. Второе остаётся независимым.
	source, err := fixture.service.CreateChatSource(ctx, core.ChatSource{
		DestinationID: fixture.sources[0].DestinationID, AccountID: account.Account.ID,
		Provider: core.ChatTwitch, Enabled: true, Options: options,
	})
	chatTestOK(t, err)
	// adapter сообщает тесту о подписке и фиксирует операции по её аккаунту.
	adapter := &twitchReaderFixture{chatTestAdapter: chatTestAdapter{readers: make(chan chatTestReader, 1)}}
	// limits допускает один основной сокет и ровно один дополнительный во время перехода.
	limits := core.DefaultChatLimits()
	limits.Readers = 1
	limits.Connections = 2
	// manager использует настоящий CredentialOwner, который создаётся для зарегистрированного адаптера.
	manager, err := New(ctx, fixture.service, fixture.store, limits, []core.ChatAdapterPort{adapter})
	chatTestOK(t, err)
	t.Cleanup(func() {
		// Очистка дожидается Manager до закрытия SQLite. cleanup и stop ограничивают её срок.
		cleanup, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		chatTestOK(t, manager.Close(cleanup))
	})

	chatTestOK(t, fixture.outputs[2].Observe(ctx, core.OutputObservation{State: core.OutputPublishing}))
	waitChatTestSnapshot(t, ctx, manager, fixture.stream.ID, func(image core.ChatSnapshot) bool {
		// Предикат подтверждает, что активная сессия уже показывает ожидание нужного выхода.
		return image.Session != nil && chatTestHasState(image, source.ID, core.ChatWaitingOutput)
	})
	select {
	case <-adapter.readers:
		t.Fatal("Чат запустился по публикации соседнего выхода")
	default:
	}

	chatTestOK(t, fixture.outputs[0].Observe(ctx, core.OutputObservation{State: core.OutputPublishing}))
	// reader содержит подтверждённую подписку, созданную для канала сохранённого пользователя.
	reader := chatTestReceive(t, ctx, adapter.readers)
	if reader.request.Key.ChannelID != account.Credentials.Bundle.Twitch.UserID ||
		reader.request.Key.AccountID != account.Account.ID || reader.request.Credentials.CredentialRevision != 2 {
		t.Fatal("Читатель не получил идентичность канала и обновлённые полномочия аккаунта")
	}
	// renewed проверяет, что периодический путь читает тот же сохранённый набор и не обменивает старый токен.
	renewed, err := reader.request.RefreshCredentials(ctx)
	chatTestOK(t, err)
	if renewed.Bundle.Twitch.RefreshToken != reader.request.Credentials.Bundle.Twitch.RefreshToken ||
		adapter.exchanges.Load() != 1 || adapter.checks.Load() < 3 {
		t.Fatal("Повторная проверка обошла владельца аккаунта либо повторила ротацию")
	}
	// releaseOverlap занимает единственное свободное физическое соединение и освобождает его после проверки.
	releaseOverlap, err := reader.request.AcquireConnection(ctx)
	chatTestOK(t, err)
	// saturated и stopWait ограничивают ожидание лишнего сокета, пока заняты оба общих слота.
	saturated, stopWait := context.WithTimeout(ctx, 20*time.Millisecond)
	defer stopWait()
	// unexpectedRelease остаётся nil, пока общий лимит правильно учитывает основную подписку.
	unexpectedRelease, err := reader.request.AcquireConnection(saturated)
	if unexpectedRelease != nil {
		unexpectedRelease()
	}
	releaseOverlap()
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Превышение общего бюджета соединений вернуло %v", err)
	}
	assertTwitchControlsSurviveRollback(t, ctx, manager, reader, fixture.stream.ID, account.Account.ID)

	chatTestOK(t, fixture.outputs[0].ObserveTerminal(ctx, core.OutputObservation{State: core.OutputStopped}))
	chatTestReceive(t, ctx, reader.stopped)
	// err подтверждает, что просроченный читатель не может снова проверить или обновить токены.
	if _, err := reader.request.RefreshCredentials(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("Отозванная подписка сохранила доступ к владельцу полномочий: %v", err)
	}
}

// assertTwitchControlsSurviveRollback проверяет подтверждение подписки и событие Gap, которые
// пришли при временно закрытом допуске аккаунта. Снятие барьера оставляет тот же reader.
func assertTwitchControlsSurviveRollback(t *testing.T, ctx context.Context, manager *Manager, reader chatTestReader, stream, account core.ID) {
	t.Helper()
	// before содержит позицию ленты до локальной мутации. err сообщает об отказе чтения.
	before, err := manager.Snapshot(ctx, stream)
	chatTestOK(t, err)
	// unblock снимает тот же барьер, который Manager использует вокруг ручного CAS аккаунта.
	_, unblock, err := manager.beginMutation(ctx, "", account)
	chatTestOK(t, err)
	// Функция снимает барьер, даже если проверка обнаружит ошибку до обычного освобождения.
	defer func() {
		if unblock != nil {
			unblock()
		}
	}()
	// finished получает результаты двух событий управления после повторной проверки допуска.
	finished := make(chan error, 2)
	// Функция имитирует подтверждение подписки, полученное в момент попытки записи аккаунта.
	go func() {
		finished <- reader.sink.Status(ctx, reader.request.Token, core.ChatConnected, core.ChatReasonNone)
	}()
	// Функция имитирует отдельно подтверждённую потерю транспорта в том же окне ограничения.
	go func() { finished <- reader.sink.Gap(ctx, reader.request.Token, core.ChatReasonUpstreamGap) }()
	// held задаёт наблюдаемое окно временного запрета, пока запись ещё не принята.
	held := time.NewTimer(25 * time.Millisecond)
	defer held.Stop()
	select {
	// err сообщает, если callback преждевременно завершился, пока допуск был закрыт.
	case err := <-finished:
		t.Fatalf("Событие завершилось до разрешения текущей привязки: %v", err)
	case <-held.C:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// Снимаем ограничение без изменения сохранённой конфигурации, как при откате записи.
	unblock()
	unblock = nil
	chatTestOK(t, chatTestReceive(t, ctx, finished))
	chatTestOK(t, chatTestReceive(t, ctx, finished))
	// page получает публичные события после начала мутации, включая ровно один отложенный Gap.
	page, err := manager.Events(ctx, stream, before.Cursor, 16)
	chatTestOK(t, err)
	// gaps считает события подтверждённой потери за время проверки.
	gaps := 0
	// event относится к текущему поколению ленты после снятия ограничения.
	for _, event := range page.Events {
		if event.Kind == core.ChatEventGap && event.Reason == core.ChatReasonUpstreamGap {
			gaps++
		}
	}
	if gaps != 1 {
		t.Fatalf("Лента получила %d событий upstream_gap вместо одного", gaps)
	}

	// Открываем тот же барьер ещё раз, чтобы проверить прекращение ожидания без публикации.
	_, unblock, err = manager.beginMutation(ctx, "", account)
	chatTestOK(t, err)
	// blockedCtx и expire задают отдельный срок callback той же попытки, пока аккаунт заблокирован.
	blockedCtx, expire := context.WithTimeout(ctx, 15*time.Millisecond)
	defer expire()
	// err должен сохранить причину отмены и завершить ожидание до снятия барьера.
	if err := reader.sink.Gap(blockedCtx, reader.request.Token, core.ChatReasonUpstreamGap); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("Отмена заблокированного callback вернула %v", err)
	}
	unblock()
	unblock = nil
	// after проверяет, что отменённый callback не опубликовал позднее событие.
	after, err := manager.Events(ctx, stream, page.Cursor, 16)
	chatTestOK(t, err)
	// event содержит очередное событие после уже принятого upstream_gap.
	for _, event := range after.Events {
		if event.Kind == core.ChatEventGap {
			t.Fatal("Отменённый callback оставил поздний пробел")
		}
	}
}
