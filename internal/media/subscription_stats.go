package media

import (
	"math"
	"time"
)

// SubscriptionDrops считает только пакеты, ещё не переданные через Next.
// Каждая единица получает ровно одну причину у текущего владельца.
type SubscriptionDrops struct {
	Overflow         uint64
	Oversized        uint64
	ConfigRejected   uint64
	AwaitingKeyframe uint64
	AudioBeforeCut   uint64
	SourceReset      uint64
	ConfigReset      uint64
	Closed           uint64
}

// SubscriptionSnapshot согласованно показывает логическое удержание и накопленные потери.
// PacketBytes включает очередь и ожидающее аудио; размер кучи и RSS сюда не входят.
type SubscriptionSnapshot struct {
	QueuedPackets  uint64
	PendingPackets uint64
	PacketBytes    uint64
	ConfigBytes    uint64
	Generation     uint64
	Resync         bool
	Drops          SubscriptionDrops
	Saturated      bool
}

type subscriptionDropReason uint8

const (
	dropOverflow subscriptionDropReason = iota
	dropOversized
	dropConfigRejected
	dropAwaitingKeyframe
	dropAudioBeforeCut
	dropSourceReset
	dropConfigReset
	dropClosed
)

func (s *Subscription) dropLocked(reason subscriptionDropReason, count uint64) {
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

func (s *Subscription) Snapshot() SubscriptionSnapshot {
	s.router.mu.Lock()
	defer s.router.mu.Unlock()
	s.expireLocked(time.Now())
	return SubscriptionSnapshot{QueuedPackets: uint64(len(s.queue)), PendingPackets: uint64(len(s.pendingAudio)), PacketBytes: s.packetBytes, ConfigBytes: s.configBytes, Generation: s.generation, Resync: s.resync, Drops: s.drops, Saturated: s.saturated}
}
