package group

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
)

func TestFallbackReadinessTracksAttemptAndBothTracks(t *testing.T) {
	s, v, a, now := switcherFixture(t)
	if !errors.Is(s.FallbackReady(1, now), core.ErrNotReady) {
		t.Fatal("пустая группа объявлена готовой")
	}
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, v, v, a, 0, 0, 1600, true, now))
	if s.FallbackReady(1, now) == nil {
		t.Fatal("одна дорожка прошла барьер")
	}
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, a, v, a, 0, 0, 1024, false, now))
	if err := s.FallbackReady(1, now); err != nil {
		t.Fatal(err)
	}
	if err := s.BeginAttempt(Fallback, 2); err != nil {
		t.Fatal(err)
	}
	if s.FallbackReady(2, now) == nil || s.FallbackReady(1, now) == nil {
		t.Fatal("новая попытка сохранила прежний барьер")
	}
	allowUnavailable(t, pushPacket(t, s, Fallback, 2, v, v, a, 0, 0, 1600, true, now))
	allowUnavailable(t, pushPacket(t, s, Fallback, 2, a, v, a, 0, 0, 1024, false, now))
	if err := s.FallbackReady(2, now); err != nil {
		t.Fatal(err)
	}
	_ = s.Fail(Fallback, 2, ErrFeedEnded, now)
	if s.FallbackReady(2, now) == nil {
		t.Fatal("завершённая попытка осталась готовой")
	}
}
func TestFallbackReadinessWhileLiveSelectedValidatesTiming(t *testing.T) {
	for _, skew := range []int64{480, 48000} {
		t.Run(time.Duration(skew).String(), func(t *testing.T) {
			s, v, a, now := switcherFixture(t)
			s.config.MaxAVSkew = 20 * time.Millisecond
			// LIVE становится выбранным до появления fallback.
			allowUnavailable(t, pushPacket(t, s, Live, 1, v, v, a, 0, 0, 1600, true, now))
			allowUnavailable(t, pushPacket(t, s, Live, 1, a, v, a, 0, 0, 1024, false, now))
			later := now.Add(120 * time.Millisecond)
			allowUnavailable(t, pushPacket(t, s, Live, 1, v, v, a, 1600, 1600, 1600, true, later))
			allowUnavailable(t, pushPacket(t, s, Live, 1, a, v, a, 1600, 1600, 1024, false, later))
			if s.Status().Source != Live {
				t.Fatal(s.Status())
			}
			allowUnavailable(t, pushPacket(t, s, Fallback, 1, v, v, a, 0, 0, 1600, true, later))
			allowUnavailable(t, pushPacket(t, s, Fallback, 1, a, v, a, skew, skew, 1024, false, later))
			err := s.FallbackReady(1, later)
			if (err == nil) != (skew == 480) {
				t.Fatal("барьер не проверил A/V timing", err)
			}
			if s.Status().Source != Live || !s.Status().Available {
				t.Fatal("проверка fallback изменила здоровый LIVE")
			}
		})
	}
}
func TestFallbackReadinessExpiresAndRejectsUnusableDuration(t *testing.T) {
	s, v, a, now := switcherFixture(t)
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, v, v, a, 0, 0, 0, true, now))
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, a, v, a, 0, 0, 0, false, now))
	if s.FallbackReady(1, now) == nil {
		t.Fatal("неизвестная длительность прошла барьер")
	}
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, v, v, a, 1600, 1600, 1600, false, now))
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, a, v, a, 1024, 1024, 1024, false, now))
	if err := s.FallbackReady(1, now); err != nil {
		t.Fatal(err)
	}
	if s.FallbackReady(1, now.Add(2*time.Second)) == nil {
		t.Fatal("просроченная готовность сохранена")
	}
	_ = s.Close()
	if !errors.Is(s.FallbackReady(1, now), media.ErrRouterClosed) {
		t.Fatal("закрытая группа готова")
	}
}
func TestGroupControlIgnoresOlderRevision(t *testing.T) {
	s, _, _, _ := switcherFixture(t)
	g := &Group{switcher: s}
	if err := g.applyControl(20, true); err != nil {
		t.Fatal(err)
	}
	if err := g.applyControl(19, false); err != nil {
		t.Fatal(err)
	}
	if !s.Status().Paused {
		t.Fatal("старый polling откатил желание")
	}
}
func TestWaitReadyHonorsFinalFailureAndCancellation(t *testing.T) {
	f := runtimeUnitFixture(t)
	owner, err := f.config.Core.BindExecutionOwner(context.Background(), f.config.Parent)
	if err != nil {
		t.Fatal(err)
	}
	s, _, _, _ := switcherFixture(t)
	g := &Group{config: f.config.Runtime, owner: owner, switcher: s, done: make(chan struct{})}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if !errors.Is(g.WaitReady(ctx), context.Canceled) {
		t.Fatal("ожидание не отменилось")
	}
	g.fail(Fallback, ErrFeedEnded)
	if !errors.Is(g.WaitReady(context.Background()), ErrFeedEnded) {
		t.Fatal("окончательный отказ скрыт")
	}
}

func TestFallbackReadinessDoesNotReuseDrainedQueueAfterLiveSwitch(t *testing.T) {
	s, v, a, now := switcherFixture(t)
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, v, v, a, 0, 0, 1600, true, now))
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, a, v, a, 0, 0, 1024, false, now))
	if err := s.FallbackReady(1, now); err != nil {
		t.Fatal(err)
	}
	allowUnavailable(t, pushPacket(t, s, Live, 1, v, v, a, 0, 0, 1600, true, now))
	allowUnavailable(t, pushPacket(t, s, Live, 1, a, v, a, 0, 0, 1024, false, now))
	later := now.Add(120 * time.Millisecond)
	allowUnavailable(t, pushPacket(t, s, Live, 1, v, v, a, 1600, 1600, 1600, true, later))
	allowUnavailable(t, pushPacket(t, s, Live, 1, a, v, a, 1600, 1600, 1024, false, later))
	if s.Status().Source != Live {
		t.Fatal(s.Status())
	}
	if s.FallbackReady(1, later) == nil {
		t.Fatal("исторический барьер заменил новую точку перехода")
	}
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, v, v, a, 1600, 1600, 1600, true, later))
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, a, v, a, 1600, 1600, 1024, false, later))
	if err := s.FallbackReady(1, later); err != nil {
		t.Fatal(err)
	}
}
