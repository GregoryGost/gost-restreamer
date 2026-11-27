package chat

import (
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"math"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// chatBufferItem хранит значение и его вес в бюджете очереди.
type chatBufferItem[T any] struct {
	// value содержит данные одной записи.
	value T
	// size содержит логический либо закодированный размер по правилам владельца.
	size uint64
}

// chatBuffer удерживает FIFO в пределах заданного владельцем бюджета.
// Удаление сразу обнуляет ячейку, чтобы хвост массива не сохранял ссылки на данные.
type chatBuffer[T any] struct {
	// items содержит записи в порядке поступления и уже обнулённый префикс.
	items []chatBufferItem[T]
	// first указывает начало живой части items.
	first int
	// bytes содержит сумму размеров живых записей.
	bytes uint64
}

// len возвращает число живых записей.
func (b *chatBuffer[T]) len() int { return len(b.items) - b.first }

// at возвращает запись по смещению от головы в пределах живой части.
func (b *chatBuffer[T]) at(index int) T { return b.items[b.first+index].value }

// canPush проверяет оба предела с корректным сравнением даже при максимальном uint64.
func (b *chatBuffer[T]) canPush(size uint64, budget core.ChatBudget) bool {
	return b.len() < budget.Count && size <= budget.Bytes-b.bytes
}

// push добавляет запись после проверки бюджета вызывающим кодом.
func (b *chatBuffer[T]) push(value T, size uint64) {
	if b.first >= len(b.items)/2 && len(b.items) == cap(b.items) && b.first != 0 {
		// n содержит живую длину после переноса к началу массива.
		n := copy(b.items, b.items[b.first:])
		clear(b.items[n:])
		b.items = b.items[:n]
		b.first = 0
	}
	b.items = append(b.items, chatBufferItem[T]{value: value, size: size})
	b.bytes += size
}

// pop удаляет голову непустой очереди и возвращает принадлежавшее ей значение.
func (b *chatBuffer[T]) pop() T {
	// item удерживает удаляемое значение и размер до обнуления ячейки.
	item := b.items[b.first]
	b.items[b.first] = chatBufferItem[T]{}
	b.first++
	b.bytes -= item.size
	if b.first == len(b.items) {
		b.items = b.items[:0]
		b.first = 0
	}
	return item.value
}

// retain добавляет запись, выталкивая старейшие записи, либо очищает окно, если запись в него не помещается.
// Очистка не позволяет представить разрыв журнала как сплошную последовательность.
func (b *chatBuffer[T]) retain(value T, size uint64, budget core.ChatBudget) {
	if size > budget.Bytes {
		*b = chatBuffer[T]{}
		return
	}
	for !b.canPush(size, budget) {
		b.pop()
	}
	b.push(value, size)
}

// chatAddLoss складывает счётчики без перехода через ноль после исчерпания точности uint64.
func chatAddLoss(current, additional uint64) uint64 {
	if additional > math.MaxUint64-current {
		return math.MaxUint64
	}
	return current + additional
}

const (
	// chatCursorTagBytes задаёт длину усечённой подписи непрозрачной позиции.
	chatCursorTagBytes = 16
	// chatCursorPositionBytes задаёт размер полного двоичного uint64.
	chatCursorPositionBytes = 8
)

// chatCursorKey содержит самостоятельный секрет одной эпохи Hub.
// Ключ никогда не покидает этот объект, а размер выдаваемого cursor не зависит от ID.
type chatCursorKey [32]byte

// newChatCursorKey создаёт независимую привязку позиции к точному владельцу ленты.
func newChatCursorKey() (chatCursorKey, error) {
	// key получает байты из системного источника случайности.
	var key chatCursorKey
	// err сообщает о недоступности источника случайности.
	_, err := rand.Read(key[:])
	return key, err
}

// cursor подписывает позицию с контекстом scope, который различает потоки и назначение токена.
func (k chatCursorKey) cursor(scope string, position uint64) core.ChatCursor {
	// raw содержит точный uint64 и укороченную подпись.
	var raw [chatCursorPositionBytes + chatCursorTagBytes]byte
	binary.BigEndian.PutUint64(raw[:chatCursorPositionBytes], position)
	// mac связывает позицию с ключом эпохи и идентичностью владельца.
	mac := hmac.New(sha256.New, k[:])
	_, _ = mac.Write(raw[:chatCursorPositionBytes])
	_, _ = mac.Write([]byte(scope))
	copy(raw[chatCursorPositionBytes:], mac.Sum(nil))
	return core.ChatCursor(base64.RawURLEncoding.EncodeToString(raw[:]))
}

// position проверяет размер, каноническую форму и подпись курсора перед возвратом позиции.
func (k chatCursorKey) position(scope string, cursor core.ChatCursor, limit uint64) (uint64, bool) {
	if uint64(len(cursor)) > limit || len(cursor) != base64.RawURLEncoding.EncodedLen(chatCursorPositionBytes+chatCursorTagBytes) {
		return 0, false
	}
	// raw содержит двоичную позицию, err сообщает о повреждении base64.
	raw, err := base64.RawURLEncoding.Strict().DecodeString(string(cursor))
	if err != nil || len(raw) != chatCursorPositionBytes+chatCursorTagBytes {
		return 0, false
	}
	// position сохраняет целый uint64 для проверки подписи и дальнейшего поиска в replay.
	position := binary.BigEndian.Uint64(raw[:chatCursorPositionBytes])
	return position, hmac.Equal([]byte(k.cursor(scope, position)), []byte(cursor))
}
