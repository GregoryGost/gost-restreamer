package chat

import (
	"context"
	"errors"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// currentCredentials читает обе области аккаунта, пока вызов удерживает допуск lane.
// Если была отложенная сверка, успех требует совпадения полного результата. При недоступном чтении
// квитанция остаётся в занятом слоте. Установленное отсутствие аккаунта освобождает отметки.
func (o *CredentialOwner) currentCredentials(ctx context.Context, id core.ID, lane *credentialLane) (core.ChatCredentialResult, bool, error) {
	// current принимает обе актуальные области, err сообщает об отказе чтения согласованного снимка.
	current, err := o.store.GetChatCredentialResult(ctx, id)
	if errors.Is(err, core.ErrNotFound) {
		lane.pending = nil
		lane.spent = credentialReceipt{}
		return core.ChatCredentialResult{}, false, err
	}
	if err == nil && (current.Validate() != nil || current.Account.ID != id) {
		err = core.ErrCorrupt
	}
	if err != nil {
		if lane.pending != nil {
			return core.ChatCredentialResult{}, false, errors.Join(ErrCredentialCommitUnknown, ctx.Err())
		}
		return core.ChatCredentialResult{}, false, err
	}

	lane.observe(current.Credentials)
	// pending содержит ожидаемый результат одной ранее завершившейся записи.
	pending := lane.pending
	lane.pending = nil
	if pending == nil {
		return current, false, nil
	}
	if !current.Matches(pending.result) {
		return core.ChatCredentialResult{}, false, core.ErrStale
	}
	return current, true, nil
}

// confirmCredentialWrite возвращает принятый результат успешной записи либо подтверждает его явным чтением.
// Сверка имеет собственный ограниченный контекст. Отмена запроса после возможного COMMIT не мешает
// прочитать квитанцию, а закрытие CredentialOwner отменяет и эту работу. Запись и обмен не повторяются.
func (o *CredentialOwner) confirmCredentialWrite(ctx context.Context, id core.ID, lane *credentialLane, expected pendingCredentialWrite, writeErr error) (core.ChatCredentialResult, error) {
	if expected.result.Validate() != nil || expected.result.Account.ID != id {
		if writeErr != nil {
			return core.ChatCredentialResult{}, writeErr
		}
		return core.ChatCredentialResult{}, core.ErrCorrupt
	}
	if writeErr == nil {
		lane.observe(expected.result.Credentials)
		return expected.result, nil
	}

	expected.result.Credentials.Bundle = copyCredentialBundle(expected.result.Credentials.Bundle)
	lane.pending = &expected
	// checkCtx задаёт отдельный срок чтения квитанции, cancel освобождает таймер; stop снимает подписку на Close.
	checkCtx, cancel := context.WithTimeout(context.WithoutCancel(ctx), o.shutdownTimeout)
	defer cancel()
	stop := context.AfterFunc(o.lifetime, cancel)
	defer stop()
	if o.lifetime.Err() != nil {
		cancel()
	}
	// current содержит результат сверки, err различает недоступность и установленное несовпадение.
	current, _, err := o.currentCredentials(checkCtx, id, lane)
	if err != nil {
		if errors.Is(err, ErrCredentialCommitUnknown) {
			return core.ChatCredentialResult{}, err
		}
		return core.ChatCredentialResult{}, errors.Join(err, writeErr)
	}
	return current, nil
}

// recoveredReplacement перед новой записью сверяет незавершённую операцию аккаунта.
// При повторном обращении с параметрами той же подтверждённой ручной замены возвращает
// её результат для синхронного применения Manager без второго CAS. Пустой Account допускает новую запись.
func (o *CredentialOwner) recoveredReplacement(ctx context.Context, id core.ID, lane *credentialLane, provider core.ChatProvider, bundle core.ChatCredentialBundle, writeID core.ID, revision uint64) (core.ChatAccount, error) {
	// pending указывает на ожидаемую квитанцию до того, как сверка освободит слот незавершённой записи.
	pending := lane.pending
	if pending == nil {
		return core.ChatAccount{}, nil
	}
	// current содержит подтверждённую запись, err сообщает о невозможности принять её результат.
	current, _, err := o.currentCredentials(ctx, id, lane)
	if err != nil {
		return core.ChatAccount{}, err
	}
	if current.Credentials.CredentialWriteID != writeID {
		return core.ChatAccount{}, nil
	}
	// requested подставляет набор повторного обращения для точного сравнения с сохранённым результатом.
	requested := current
	requested.Credentials.Bundle = bundle
	if !pending.replacement || provider != current.Account.Provider || revision != current.Account.Revision-1 || !current.Matches(requested) {
		return core.ChatAccount{}, core.ErrConflict
	}
	return current.Account, nil
}

// observe освобождает отметку об использованном токене, если теперь сохранён другой набор полномочий.
// Конфигурационная ревизия при этом не участвует в сравнении, поэтому переименование ничего не меняет.
func (l *credentialLane) observe(current core.ChatCredentials) {
	if l.spent.revision != current.CredentialRevision || l.spent.writeID != current.CredentialWriteID {
		l.spent = credentialReceipt{}
	}
}
