package media

import (
	"math"
	"time"
)

// SubscriptionDrops считает только пакеты, ещё не переданные через Next.
// Каждая единица получает ровно одну причину у текущего владельца.
type SubscriptionDrops struct {
	// Overflow считает пакеты, удалённые при сбросе из-за переполнения очереди.
	Overflow uint64
	// Oversized считает пакеты, не помещающиеся в пустую очередь после сброса.
	Oversized uint64
	// ConfigRejected считает пакеты, отвергнутые из-за непомещающихся заголовков дорожек.
	ConfigRejected uint64
	// AwaitingKeyframe считает видеопакеты без заявленного Keyframe, отброшенные до выбора
	// стартового видеопакета.
	AwaitingKeyframe uint64
	// AudioBeforeCut считает аудиоединицы, начинающиеся раньше PTS выбранного стартового
	// видеопакета.
	AudioBeforeCut uint64
	// SourceReset считает пакеты, удалённые при смене или отзыве источника.
	SourceReset uint64
	// ConfigReset считает пакеты, удалённые после смены конфигураций дорожек.
	ConfigReset uint64
	// Closed считает пакеты, удалённые при завершении подписки.
	Closed uint64
}

// SubscriptionSnapshot согласованно показывает логическое удержание и накопленные потери.
// PacketBytes включает очередь и ожидающее аудио; размер кучи и объём резидентной памяти сюда не входят.
type SubscriptionSnapshot struct {
	// QueuedPackets показывает число пакетов, ожидающих выдачи через Next.
	QueuedPackets uint64
	// PendingPackets показывает текущее число аудиоединиц, удерживаемых до выбора видеопакета с
	// заявленным Keyframe.
	PendingPackets uint64
	PacketBytes    uint64
	// ConfigBytes показывает текущий логический вес пары заголовков, принятой в бюджет подписки; при
	// отсутствии подходящей пары или завершении подписки равен нулю.
	ConfigBytes uint64
	// Generation показывает текущее поколение сброса, используемое для отзыва доставок.
	Generation uint64
	// Resync показывает, ожидает ли подписка согласованного запуска видео и аудио.
	Resync bool
	// Drops содержит накопленные потери пакетов по причинам отбрасывания.
	Drops SubscriptionDrops
	// Saturated отмечает переполнение хотя бы одного счётчика потерь.
	Saturated bool
}

// subscriptionDropReason выбирает единственный счётчик причины для потерянной единицы доступа.
type subscriptionDropReason uint8

const (
	// dropOverflow обозначает очистку пакетов при переполнении очереди.
	dropOverflow subscriptionDropReason = iota
	// dropOversized обозначает пакет, не помещающийся даже после очистки очереди.
	dropOversized
	// dropConfigRejected обозначает отказ из-за превышения бюджета конфигурациями дорожек.
	dropConfigRejected
	// dropAwaitingKeyframe обозначает пропуск видеопакета без заявленного Keyframe до выбора
	// стартового видео.
	dropAwaitingKeyframe
	// dropAudioBeforeCut обозначает аудио, начинающееся раньше PTS выбранного стартового
	// видеопакета.
	dropAudioBeforeCut
	// dropSourceReset обозначает очистку при смене или отзыве источника.
	dropSourceReset
	// dropConfigReset обозначает очистку при смене конфигураций дорожек.
	dropConfigReset
	// dropClosed обозначает удаление пакетов при завершении подписки.
	dropClosed
)

// Учитывает count потерянных AU по одной известной причине с насыщением на MaxUint64; требует
// блокировку Router.
func (s *Subscription) dropLocked(reason subscriptionDropReason, count uint64) {
	// counter указывает на счётчик потерь, соответствующий переданной причине.
	var counter *uint64
	switch reason {
	case dropOverflow:
		counter = &s.drops.Overflow
	case dropOversized:
		counter = &s.drops.Oversized
	case dropConfigRejected:
		counter = &s.drops.ConfigRejected
	case dropAwaitingKeyframe:
		counter = &s.drops.AwaitingKeyframe
	case dropAudioBeforeCut:
		counter = &s.drops.AudioBeforeCut
	case dropSourceReset:
		counter = &s.drops.SourceReset
	case dropConfigReset:
		counter = &s.drops.ConfigReset
	case dropClosed:
		counter = &s.drops.Closed
	}
	if count > math.MaxUint64-*counter {
		*counter = math.MaxUint64
		s.saturated = true
	} else {
		*counter += count
	}
}

// Snapshot возвращает согласованную копию очереди, поколения, логических байтов и потерь под
// блокировкой Router; перед чтением проверяет истечение срока синхронизации.
func (s *Subscription) Snapshot() SubscriptionSnapshot {
	s.router.mu.Lock()
	defer s.router.mu.Unlock()
	s.expireLocked(time.Now())
	return SubscriptionSnapshot{QueuedPackets: uint64(len(s.queue)), PendingPackets: uint64(len(s.pendingAudio)), PacketBytes: s.packetBytes, ConfigBytes: s.configBytes, Generation: s.generation, Resync: s.resync, Drops: s.drops, Saturated: s.saturated}
}
