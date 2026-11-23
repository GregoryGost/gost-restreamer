package core

import (
	"context"
	"sync/atomic"
)

// chatPublicationState различает последовательные стадии допуска чата одного наблюдателя.
type chatPublicationState uint32

const (
	// chatPublicationWaiting — начальное состояние до первого принятого PUBLISHING.
	chatPublicationWaiting chatPublicationState = iota
	// chatPublicationLive означает, что точный наблюдатель уже подтвердил передачу медиа.
	chatPublicationLive
	// chatPublicationRevoked запрещает дальнейшее использование этого наблюдателя для чата.
	chatPublicationRevoked
)

// ChatOutputPublication позволяет проверить первую публикацию точного наблюдателя выхода.
// При повторном CONNECTING или RETRYING признак сохраняется до отзыва наблюдателя или остановки
// исполнения. Ссылка выдаётся в оперативном снимке и может пережить отзыв, например в очереди чата.
type ChatOutputPublication struct {
	// authority связывает свидетельство с реестром, который его создал и отзывает.
	authority *RuntimeAuthority
	// session ограничивает проверку точным родительским выполнением этого выхода.
	session RuntimeSessionFence
	// state меняется под допуском RuntimeAuthority и допускает предварительное чтение без блокировки.
	state atomic.Uint32
}

// Published сообщает, подтвердил ли наблюдатель публикацию и действует ли он на момент чтения.
// Перед выдачей сообщения Hub выполняет окончательную проверку через CheckChatPublications.
func (p *ChatOutputPublication) Published() bool {
	return p != nil && p.state.Load() == uint32(chatPublicationLive)
}

// revoke закрывает свидетельство под RuntimeAuthority. Результат показывает, изменилось ли состояние.
func (p *ChatOutputPublication) revoke() bool {
	return p != nil && p.state.Swap(uint32(chatPublicationRevoked)) != uint32(chatPublicationRevoked)
}

// ChatSessionOwner различает наблюдателей родительской сессии, включая замену с тем же SessionFence.
// Владельца ленты можно проверить независимо от числа наблюдаемых выходов.
type ChatSessionOwner struct {
	// authority проверяет, что снимок и текущий Service принадлежат одному открытию хранилища.
	authority *RuntimeAuthority
	// session фиксирует родительское выполнение для проверки связанных выходов.
	session RuntimeSessionFence
	// active доступен для предварительного чтения и сбрасывается при удалении наблюдателя.
	active atomic.Bool
}

// Active сообщает, зарегистрирован ли ещё этот наблюдатель на момент чтения.
func (p *ChatSessionOwner) Active() bool {
	return p != nil && p.active.Load()
}

// notifyChatLocked закрывает прежний сигнал и создаёт новый под RuntimeAuthority. Сравнение двух
// каналов обнаруживает изменение даже при совпадении пробуждения со страницей событий измерений.
func (a *RuntimeAuthority) notifyChatLocked() {
	close(a.chatChanged)
	a.chatChanged = make(chan struct{})
}

// NotifyChatRunning будит подписчиков после подтверждённого сохранения RUNNING. Session Manager
// вызывает метод после успешной команды либо точной сверки квитанции. Метод проверяет живого
// наблюдателя под RuntimeAuthority, включая запуск без выходов и без входа OBS.
func (o *SessionObserver) NotifyChatRunning() {
	if o == nil || o.node == nil {
		return
	}
	// a сериализует пробуждение с отзывом именно этого наблюдателя.
	a := o.node.authority
	<-a.gate
	defer a.release()
	if !a.closed && o.Valid() {
		a.notifyChatLocked()
	}
}

// CheckChatPublications в одной короткой проверке подтверждает точного родителя и заполняет live
// для соответствующих proofs. Родитель из прежнего открытия или отозванный наблюдатель даёт ErrStale.
// Пустые срезы позволяют проверить только родителя. При ошибке вызывающий игнорирует live.
// Никакие операции чатов, SQLite и сети под RuntimeAuthority здесь не выполняются.
func (s *Service) CheckChatPublications(ctx context.Context, owner *ChatSessionOwner, proofs []*ChatOutputPublication, live []bool) error {
	if owner == nil || len(proofs) != len(live) {
		return ErrInvalid
	}
	// a содержит реестр данного Service, err сообщает о недоступности либо отмене проверки.
	a, err := s.runtimeAuthority()
	if err != nil {
		return err
	}
	if err = a.acquire(ctx); err != nil {
		return err
	}
	defer a.release()
	if owner.authority != a || !owner.Active() {
		return ErrStale
	}
	// index связывает результат с исходным элементом, proof — свидетельство проверяемого выхода.
	for index, proof := range proofs {
		live[index] = proof != nil && proof.authority == a && proof.session == owner.session && proof.Published()
	}
	return nil
}
