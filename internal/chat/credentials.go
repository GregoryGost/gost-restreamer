package chat

import (
	"context"
	"crypto/rand"
	"errors"
	"slices"
	"sync"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

var (
	// ErrCredentialCommitUnknown сообщает, что согласованное чтение пока не позволяет подтвердить результат записи.
	ErrCredentialCommitUnknown = errors.New("результат записи полномочий не подтверждён")
	// ErrCredentialRefreshConsumed сообщает, что прежний одноразовый токен мог быть использован и требуется новый набор.
	ErrCredentialRefreshConsumed = errors.New("требуются новые полномочия чата")
	// ErrCredentialExchangeFailed обозначает отказ обмена, для которого адаптер не вернул известную безопасную причину.
	ErrCredentialExchangeFailed = errors.New("не удалось обновить полномочия чата")
)

// CredentialStore предоставляет согласованное чтение обеих областей аккаунта и две существующие операции CAS.
// Если ошибка записи допускает состоявшийся COMMIT, методы возвращают ожидаемый полный результат.
type CredentialStore interface {
	// GetChatCredentialResult читает конфигурацию и полномочия одним снимком без изменения данных.
	GetChatCredentialResult(context.Context, core.ID) (core.ChatCredentialResult, error)
	// RefreshChatCredentials заменяет полномочия при ожидаемой ревизии полномочий и сохраняет конфигурацию.
	RefreshChatCredentials(context.Context, core.ID, core.ChatProvider, core.ChatCredentialBundle, core.ID, uint64) (core.ChatCredentialResult, error)
	// ReplaceChatCredentials принимает подтверждённый вручную набор и увеличивает обе ревизии при успешном CAS.
	ReplaceChatCredentials(context.Context, core.ID, core.ChatProvider, core.ChatCredentialBundle, core.ID, uint64) (core.ChatCredentialResult, error)
}

// credentialAccountMutator координирует ручную замену с публикацией событий затронутых обработчиков.
type credentialAccountMutator interface {
	// mutateAccount ограничивает допуск затронутых обработчиков перед однократным синхронным вызовом записи.
	// Успех подтверждает применение результата, возвращённого этим вызовом. При ошибке Manager сверяет допуск.
	mutateAccount(context.Context, core.ID, func(context.Context) (credentialReplacement, error)) (core.ChatApplyResult, error)
}

// credentialReplacement подтверждает ручную замену после успешной записи или сверки полной квитанции.
type credentialReplacement struct {
	// account содержит конфигурацию уже подтверждённой записи.
	account core.ChatAccount
	// recovered разрешает применить уже сохранённую ревизию при точном совпадении прежней квитанции.
	recovered bool
}

// CredentialRefreshResult содержит полный результат обмена и сообщает, допустимо ли снова использовать прежний набор.
type CredentialRefreshResult struct {
	// Bundle содержит полный новый набор, включая следующий токен при одноразовом обмене.
	// nil без ошибки допускается, когда обмен не нужен.
	Bundle *core.ChatCredentialBundle
	// PreviousMayBeConsumed отмечает возможное использование одноразового токена, в том числе при ошибке обмена.
	PreviousMayBeConsumed bool
}

// CredentialRefreshFunc проверяет, нужен ли обмен текущих полномочий, и при необходимости выполняет его вне транзакции.
// Функция завершает принадлежащий ей обмен до возврата, соблюдает отмену ctx и не обращается повторно к CredentialOwner.
// Для разных аккаунтов вызовы могут выполняться одновременно.
// Если одноразовый токен мог быть использован, PreviousMayBeConsumed нужен даже при потере ответа или отмене.
type CredentialRefreshFunc func(ctx context.Context, current core.ChatCredentials) (CredentialRefreshResult, error)

// CredentialOwner сериализует чтение, обмен и принятие проверенной замены отдельно для каждого аккаунта.
// Слот освобождается после последней операции, если сверка завершена и нет отметки об использовании одноразового токена.
type CredentialOwner struct {
	// store предоставляет доменное чтение и CAS без сетевых операций.
	store CredentialStore
	// refresh при необходимости выполняет один внешний обмен по набору, прочитанному под блокировкой аккаунта.
	refresh CredentialRefreshFunc
	// refreshVK перевыпускает app-токен после проверки срока или квитанции отклонённого набора.
	// Manager задаёт функцию до передачи владельца читателям.
	refreshVK CredentialRefreshFunc
	// maxAccounts ограничивает все слоты, включая ожидающие сверки или подтверждённой замены.
	maxAccounts int
	// maxOperations ограничивает все выполняемые и ожидающие вызовы по наибольшему применимому бюджету чатов.
	maxOperations int
	// shutdownTimeout ограничивает немедленную послезаписную сверку и ожидание завершения в Close.
	shutdownTimeout time.Duration
	// lifetime отменяет операции, которые ожидали аккаунт или выполнялись при закрытии.
	lifetime context.Context
	// cancel прекращает срок жизни владельца при первом вызове Close.
	cancel context.CancelFunc
	// done закрывается после завершения всех допущенных операций.
	done chan struct{}
	// mu защищает lanes, inFlight и регистрацию операций при закрытии.
	mu sync.Mutex
	// lanes индексирует занятые слоты по AccountID. После завершения владельца поле равно nil.
	lanes map[core.ID]*credentialLane
	// inFlight считает допущенные вызовы, включая ожидание аккаунта и синхронное применение замены.
	inFlight int
}

// credentialLane позволяет выполнять операции одного аккаунта по очереди и хранит незавершённую сверку.
type credentialLane struct {
	// gate допускает одного пользователя слота и защищает pending и spent.
	gate chan struct{}
	// users считает допущенные вызовы данного аккаунта под CredentialOwner.mu.
	users int
	// retired отмечает доказанное удаление аккаунта под CredentialOwner.mu.
	retired bool
	// pending хранит ожидаемый полный результат записи, исход которой пока нельзя проверить.
	pending *pendingCredentialWrite
	// spent запоминает сохранённый набор, токен которого мог быть потрачен на одноразовый обмен.
	spent credentialReceipt
}

// credentialReceipt идентифицирует прежний набор независимо от изменения имени аккаунта.
type credentialReceipt struct {
	// revision указывает ревизию полномочий, использованных при обмене. Ноль означает отсутствие отметки.
	revision uint64
	// writeID связывает отметку с точной квитанцией прочитанного набора.
	writeID core.ID
}

// pendingCredentialWrite хранит ожидаемую квитанцию и вид операции, начатой до ошибки записи.
type pendingCredentialWrite struct {
	// result содержит обе ревизии, ожидаемые полномочия и служебные поля записи для полной сверки.
	result core.ChatCredentialResult
	// replacement отмечает ручную замену, принятие которой требует подтверждения Manager.
	replacement bool
}

// NewCredentialOwner принимает существующий сервис хранения, обязательные пределы и функцию обмена.
// При nil в refresh получение полномочий только читает согласованный серверный снимок.
func NewCredentialOwner(store CredentialStore, limits core.ChatLimits, refresh CredentialRefreshFunc) (*CredentialOwner, error) {
	if store == nil || limits.Validate() != nil {
		return nil, core.ErrInvalid
	}
	// lifetime задаёт срок жизни владельца, cancel завершает его при остановке.
	lifetime, cancel := context.WithCancel(context.Background())
	return &CredentialOwner{
		store: store, refresh: refresh, maxAccounts: limits.AccountOwners,
		maxOperations:   max(limits.AccountOwners, limits.Sources, limits.Readers, limits.Authorizations),
		shutdownTimeout: limits.ShutdownTimeout,
		lifetime:        lifetime, cancel: cancel, done: make(chan struct{}), lanes: make(map[core.ID]*credentialLane),
	}, nil
}

// Credentials читает текущий набор аккаунта и при необходимости сохраняет результат одного обмена.
// Операция ждёт предыдущую работу того же аккаунта и сверяет неопределённую запись до следующего обмена.
// Набор с возможно использованным одноразовым токеном остаётся недоступен до появления другого набора.
func (o *CredentialOwner) Credentials(ctx context.Context, id core.ID) (core.ChatCredentials, error) {
	return o.credentials(ctx, id, false, nil)
}

// credentials читает текущий набор под допуском аккаунта и сверяет незавершённую запись до обмена.
// checkRecovered позволяет Twitch проверить уже новую подтверждённую пару. rejected указывает исходный
// набор VK Видео Live из запроса, на который пришёл HTTP 401, и разрешает перевыпуск до истечения срока.
func (o *CredentialOwner) credentials(ctx context.Context, id core.ID, checkRecovered bool, rejected *core.ChatCredentials) (core.ChatCredentials, error) {
	// ctx учитывает остановку владельца, lane задаёт слот аккаунта, finish освобождает допуск, err сообщает причину отказа.
	ctx, lane, finish, err := o.begin(ctx, id)
	if err != nil {
		return core.ChatCredentials{}, err
	}
	defer finish()
	if err = lane.enter(ctx); err != nil {
		return core.ChatCredentials{}, err
	}
	defer lane.leave()

	// current содержит подтверждённый набор, recovered отмечает сверку ранее недоступной записи.
	current, recovered, err := o.currentCredentials(ctx, id, lane)
	if err != nil {
		return core.ChatCredentials{}, err
	}
	if lane.spent.revision != 0 {
		return core.ChatCredentials{}, ErrCredentialRefreshConsumed
	}
	if current.Credentials.Provider == core.ChatVKVideoLive && o.refreshVK != nil {
		return o.currentVKVideoLiveCredentials(ctx, lane, current.Credentials, rejected)
	}
	if rejected != nil {
		return core.ChatCredentials{}, core.ErrNotReady
	}
	if recovered && !checkRecovered || o.refresh == nil {
		return current.Credentials, nil
	}
	return o.refreshCredentials(ctx, lane, current.Credentials, o.refresh)
}

// currentVKVideoLiveCredentials проверяет срок после сверки. Более свежий пригодный набор
// используется без обмена, а совпавшая отклонённая квитанция допускает один принудительный CAS.
// Если новый токен истёк во время сверки записи CAS, следующая попытка обработает его обычным путём.
func (o *CredentialOwner) currentVKVideoLiveCredentials(ctx context.Context, lane *credentialLane, current core.ChatCredentials, rejected *core.ChatCredentials) (core.ChatCredentials, error) {
	if ctx.Err() != nil {
		return core.ChatCredentials{}, ctx.Err()
	}
	if rejected != nil && (current.AccountID != rejected.AccountID || current.Provider != rejected.Provider) {
		return core.ChatCredentials{}, core.ErrStale
	}
	// sameReceipt означает, что отказ относится к прочитанной под допуском ревизии и квитанции записи.
	sameReceipt := rejected != nil && current.CredentialRevision == rejected.CredentialRevision && current.CredentialWriteID == rejected.CredentialWriteID
	if !sameReceipt && current.Bundle.VKVideoLive.ExpiresAt.After(time.Now()) {
		return current, nil
	}
	// updated содержит подтверждённый результат единственного обмена и CAS. err сообщает причину отказа.
	updated, err := o.refreshCredentials(ctx, lane, current, o.refreshVK)
	if err != nil {
		return core.ChatCredentials{}, err
	}
	if ctx.Err() != nil {
		return core.ChatCredentials{}, ctx.Err()
	}
	if !updated.Bundle.VKVideoLive.ExpiresAt.After(time.Now()) {
		return core.ChatCredentials{}, core.ErrNotReady
	}
	return updated, nil
}

// Replace принимает ранее проверенный полный набор с заданными writeID и config revision.
// Manager ограничивает допуск обработчиков до записи, а после подтверждения применяет новую конфигурацию.
// Блокировка аккаунта удерживается только во время серверного чтения и CAS внутри вызова Manager.
func (o *CredentialOwner) Replace(ctx context.Context, manager credentialAccountMutator, id core.ID, provider core.ChatProvider, bundle core.ChatCredentialBundle, writeID core.ID, expected uint64) (core.ChatApplyResult, error) {
	// result и err сообщают вызывающему, была ли подтверждена и применена замена.
	result, _, err := o.replace(ctx, manager, id, provider, bundle, writeID, expected)
	return result, err
}

// replace применяет проверенный набор и возвращает метаданные именно подтверждённой записи.
// Эти метаданные получены при CAS или его сверке и не зависят от более позднего чтения аккаунта.
func (o *CredentialOwner) replace(ctx context.Context, manager credentialAccountMutator, id core.ID, provider core.ChatProvider, bundle core.ChatCredentialBundle, writeID core.ID, expected uint64) (core.ChatApplyResult, core.ChatAccount, error) {
	if manager == nil || expected == 0 || writeID.Validate() != nil || len(writeID) > core.ChatIDBytes || bundle.Validate(provider) != nil {
		return core.ChatApplyResult{}, core.ChatAccount{}, core.ErrInvalid
	}
	// ctx учитывает остановку владельца, lane сохраняет слот до окончания применения, finish освобождает его, err сообщает отказ в допуске.
	ctx, lane, finish, err := o.begin(ctx, id)
	if err != nil {
		return core.ChatApplyResult{}, core.ChatAccount{}, err
	}
	defer finish()
	bundle = copyCredentialBundle(bundle)

	// confirmed сохраняет публичные метаданные результата синхронной операции.
	var confirmed core.ChatAccount
	// applied и err передают результат применения. Callback под mutationCtx сверяет предыдущую
	// запись и сохраняет новые полномочия под последовательным допуском данного аккаунта.
	applied, err := manager.mutateAccount(ctx, id, func(mutationCtx context.Context) (credentialReplacement, error) {
		// err сообщает об отмене во время ожидания исключительного допуска.
		if err := lane.enter(mutationCtx); err != nil {
			return credentialReplacement{}, err
		}
		defer lane.leave()
		// recovered содержит результат сверенной ручной замены при повторном обращении с той же квитанцией.
		// err останавливает запись при недоступной сверке или конфликте параметров повторного обращения.
		recovered, err := o.recoveredReplacement(mutationCtx, id, lane, provider, bundle, writeID, expected)
		if err != nil || recovered.ID != "" {
			confirmed = recovered
			return credentialReplacement{account: recovered, recovered: err == nil}, err
		}
		// result содержит ожидаемую запись, writeErr сообщает итог config CAS, включая возможный COMMIT.
		result, writeErr := o.store.ReplaceChatCredentials(mutationCtx, id, provider, bundle, writeID, expected)
		result, writeErr = o.confirmCredentialWrite(mutationCtx, id, lane, pendingCredentialWrite{result: result, replacement: true}, writeErr)
		confirmed = result.Account
		return credentialReplacement{account: result.Account}, writeErr
	})
	if err != nil {
		return core.ChatApplyResult{}, core.ChatAccount{}, err
	}
	return applied, confirmed, nil
}

// Close прекращает допуск, отменяет работу владельца и ждёт её завершения в пределах заданного срока.
// После ошибки ожидания Done остаётся доступен до фактического завершения всех операций.
func (o *CredentialOwner) Close(ctx context.Context) error {
	if ctx == nil {
		return core.ErrInvalid
	}
	o.mu.Lock()
	o.cancel()
	o.completeIfIdle()
	o.mu.Unlock()

	// waitCtx объединяет срок вызывающего с локальным пределом остановки, cancel освобождает таймер.
	waitCtx, cancel := context.WithTimeout(ctx, o.shutdownTimeout)
	defer cancel()
	select {
	case <-o.done:
		return nil
	default:
	}
	select {
	case <-o.done:
		return nil
	case <-waitCtx.Done():
		return waitCtx.Err()
	}
}

// Done возвращает сигнал завершения всех допущенных операций. Принадлежащее вызывающему хранилище закрывается после него.
func (o *CredentialOwner) Done() <-chan struct{} {
	return o.done
}

// begin резервирует слот аккаунта и связывает срок действия запроса со временем жизни владельца.
// Возвращённый finish вызывается один раз после освобождения gate и завершения всей операции.
func (o *CredentialOwner) begin(ctx context.Context, id core.ID) (context.Context, *credentialLane, func(), error) {
	if ctx == nil || id.Validate() != nil || len(id) > core.ChatIDBytes {
		return nil, nil, nil, core.ErrInvalid
	}
	// err сообщает, что запрос отменён ещё до регистрации.
	if err := ctx.Err(); err != nil {
		return nil, nil, nil, err
	}
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.lifetime.Err() != nil {
		return nil, nil, nil, core.ErrClosed
	}
	if o.inFlight == o.maxOperations {
		return nil, nil, nil, core.ErrBusy
	}

	// lane обозначает существующий или только что созданный слот аккаунта.
	lane := o.lanes[id]
	if lane == nil {
		if len(o.lanes) == o.maxAccounts {
			return nil, nil, nil, core.ErrBusy
		}
		lane = &credentialLane{gate: make(chan struct{}, 1)}
		o.lanes[id] = lane
	}
	lane.users++
	o.inFlight++

	// operationCtx учитывает обе отмены, cancel завершает запрос, stop снимает подписку на закрытие владельца.
	operationCtx, cancel := context.WithCancel(ctx)
	stop := context.AfterFunc(o.lifetime, cancel)
	// Возвращённый вызов отменяет подписку и освобождает слот после полного завершения запроса.
	return operationCtx, lane, func() {
		stop()
		cancel()
		o.release(id, lane)
	}, nil
}

// enter ждёт исключительного допуска к аккаунту и учитывает отмену, совпавшую с освобождением слота.
func (l *credentialLane) enter(ctx context.Context) error {
	select {
	case <-ctx.Done():
		return ctx.Err()
	case l.gate <- struct{}{}:
	}
	// err проверяет отмену даже при одновременно доступных gate и ctx.Done.
	if err := ctx.Err(); err != nil {
		l.leave()
		return err
	}
	return nil
}

// leave передаёт доступ следующему запросу. Вызывается только после успешного enter.
func (l *credentialLane) leave() {
	<-l.gate
}

// release снимает допуск вызова. При отсутствии пользователей никто больше не меняет pending и spent.
func (o *CredentialOwner) release(id core.ID, lane *credentialLane) {
	o.mu.Lock()
	defer o.mu.Unlock()
	lane.users--
	o.inFlight--
	if lane.users == 0 && (lane.retired || lane.pending == nil && lane.spent.revision == 0) {
		delete(o.lanes, id)
	}
	o.completeIfIdle()
}

// retire освобождает существующий слот доказанно удалённого аккаунта. Слот с пользователями
// остаётся единственным до последнего release, даже если в нём удерживается квитанция или
// отметка об израсходованном токене. Операция не требует свободного места в реестре.
func (o *CredentialOwner) retire(id core.ID) {
	o.mu.Lock()
	defer o.mu.Unlock()
	// lane указывает только на уже существующий слот точного аккаунта.
	lane := o.lanes[id]
	if lane == nil {
		return
	}
	if lane.users == 0 {
		delete(o.lanes, id)
		return
	}
	lane.retired = true
}

// completeIfIdle освобождает оставшиеся квитанции и один раз закрывает done под CredentialOwner.mu.
func (o *CredentialOwner) completeIfIdle() {
	if o.lifetime.Err() != nil && o.inFlight == 0 && o.lanes != nil {
		o.lanes = nil
		close(o.done)
	}
}

// copyCredentialBundle отделяет изменяемые поля перед передачей набора другому участнику обмена.
func copyCredentialBundle(bundle core.ChatCredentialBundle) core.ChatCredentialBundle {
	if bundle.YouTube != nil {
		// value отделяет структуру ключа от исходного набора.
		value := *bundle.YouTube
		bundle.YouTube = &value
	}
	if bundle.Twitch != nil {
		// value отделяет структуру пары токенов и её список разрешений.
		value := *bundle.Twitch
		value.Scopes = slices.Clone(value.Scopes)
		bundle.Twitch = &value
	}
	if bundle.VKVideoLive != nil {
		// value отделяет структуру реквизитов приложения и токена.
		value := *bundle.VKVideoLive
		bundle.VKVideoLive = &value
	}
	return bundle
}

// refreshCredentials вызывает exchange под допуском аккаунта и выполняет не более одного credential CAS.
// Возможное потребление прежнего токена отмечается до проверки результата и записи.
func (o *CredentialOwner) refreshCredentials(ctx context.Context, lane *credentialLane, current core.ChatCredentials, exchange CredentialRefreshFunc) (core.ChatCredentials, error) {
	// err сообщает об отмене, которая успела наступить после получения серверного снимка.
	if err := ctx.Err(); err != nil {
		return core.ChatCredentials{}, err
	}
	// input отделяет текущий набор от изменяемых данных, доступных функции обмена.
	input := current
	input.Bundle = copyCredentialBundle(current.Bundle)
	// refresh описывает новый набор и судьбу старого токена, err сообщает результат обмена.
	refresh, err := exchange(ctx, input)
	if refresh.PreviousMayBeConsumed {
		lane.spent = credentialReceipt{revision: current.CredentialRevision, writeID: current.CredentialWriteID}
	}
	if err != nil {
		return core.ChatCredentials{}, safeCredentialExchangeError(err)
	}
	if err = ctx.Err(); err != nil {
		return core.ChatCredentials{}, err
	}
	if refresh.Bundle == nil {
		if lane.spent.revision != 0 {
			return core.ChatCredentials{}, ErrCredentialRefreshConsumed
		}
		return current, nil
	}
	if refresh.Bundle.Validate(current.Provider) != nil {
		return core.ChatCredentials{}, core.ErrInvalid
	}

	// writeID связывает единственную запись результата обмена с её полной послезаписной квитанцией.
	writeID := core.ID(rand.Text())
	// result содержит ожидаемую запись, writeErr сообщает итог credential CAS, включая возможный COMMIT.
	result, writeErr := o.store.RefreshChatCredentials(ctx, current.AccountID, current.Provider, copyCredentialBundle(*refresh.Bundle), writeID, current.CredentialRevision)
	result, writeErr = o.confirmCredentialWrite(ctx, current.AccountID, lane, pendingCredentialWrite{result: result}, writeErr)
	return result.Credentials, writeErr
}

// safeCredentialExchangeError оставляет вызывающему только известные причины без исходных данных платформы.
func safeCredentialExchangeError(err error) error {
	// retry передаёт только допустимый срок и безопасную причину повтора без данных внешнего обмена.
	var retry *core.ChatReadRetry
	if errors.As(err, &retry) && retry != nil {
		// reason сохраняет известное уточнение и заменяет неизвестную причину временным отказом сети.
		reason := retry.Reason
		switch reason {
		case core.ChatReasonNone, core.ChatReasonNetwork, core.ChatReasonQuota:
		default:
			reason = core.ChatReasonNetwork
		}
		return errors.Join(core.ErrNotReady, &core.ChatReadRetry{RetryAfter: max(0, retry.RetryAfter), Reason: reason})
	}
	// reason содержит очередную безопасную причину транспортного, контекстного или доменного отказа.
	for _, reason := range []error{context.Canceled, context.DeadlineExceeded, core.ErrBusy, core.ErrNotReady, core.ErrInvalid, ErrCredentialRefreshConsumed} {
		if errors.Is(err, reason) {
			return reason
		}
	}
	return ErrCredentialExchangeFailed
}
