package group

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
)

// Проверяет готовность только текущей попытки с обеими дорожками и её отзыв до новой попытки и после
// Fail.
func TestFallbackReadinessTracksAttemptAndBothTracks(t *testing.T) {
	// s содержит переключатель; v задаёт AV1-конфигурацию; a задаёт AAC-конфигурацию; now задаёт
	// время проверки готовности текущей попытки.
	s, v, a, now := switcherFixture(t)
	if !errors.Is(s.FallbackReady(1, now), core.ErrNotReady) {
		t.Fatal("пустая группа объявлена готовой")
	}
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, v, v, a, 0, 0, 1600, true, now))
	if s.FallbackReady(1, now) == nil {
		t.Fatal("одна дорожка прошла барьер")
	}
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, a, v, a, 0, 0, 1024, false, now))
	// err содержит результат готовности первой попытки после получения обеих дорожек.
	if err := s.FallbackReady(1, now); err != nil {
		t.Fatal(err)
	}
	// err сообщает об отказе начала второй попытки, отзывающей прежнюю готовность.
	if err := s.BeginAttempt(Fallback, 2); err != nil {
		t.Fatal(err)
	}
	if s.FallbackReady(2, now) == nil || s.FallbackReady(1, now) == nil {
		t.Fatal("новая попытка сохранила прежний барьер")
	}
	allowUnavailable(t, pushPacket(t, s, Fallback, 2, v, v, a, 0, 0, 1600, true, now))
	allowUnavailable(t, pushPacket(t, s, Fallback, 2, a, v, a, 0, 0, 1024, false, now))
	// err содержит результат готовности второй попытки после новой согласованной пары.
	if err := s.FallbackReady(2, now); err != nil {
		t.Fatal(err)
	}
	_ = s.Fail(Fallback, 2, ErrFeedEnded, now)
	if s.FallbackReady(2, now) == nil {
		t.Fatal("завершённая попытка осталась готовой")
	}
}

// Проверяет A/V-условия заглушки при выбранном LIVE, не меняя исправный выбранный источник при
// проверке.
func TestFallbackReadinessWhileLiveSelectedValidatesTiming(t *testing.T) {
	// skew задаёт смещение стартового AAC относительно видео в единицах шкалы 1/48000.
	for _, skew := range []int64{480, 48000} {
		t.Run(time.Duration(skew).String(), func(t *testing.T) {
			// Проверяет фактическую готовность fallback при уже выбранном LIVE: сдвиг AAC 480 единиц допустим,
			// 48000 нарушает бюджет 20 мс, здоровый LIVE сохраняется.

			// s содержит переключатель; v и a задают контрольные конфигурации; now задаёт
			// начало восстановления LIVE перед проверкой заглушки.
			s, v, a, now := switcherFixture(t)
			s.config.MaxAVSkew = 20 * time.Millisecond
			// LIVE становится выбранным до появления fallback.
			allowUnavailable(t, pushPacket(t, s, Live, 1, v, v, a, 0, 0, 1600, true, now))
			allowUnavailable(t, pushPacket(t, s, Live, 1, a, v, a, 0, 0, 1024, false, now))
			// later задаёт время после окна восстановления LIVE и поступления проверяемого
			// fallback.
			later := now.Add(120 * time.Millisecond)
			allowUnavailable(t, pushPacket(t, s, Live, 1, v, v, a, 1600, 1600, 1600, true, later))
			allowUnavailable(t, pushPacket(t, s, Live, 1, a, v, a, 1600, 1600, 1024, false, later))
			if s.Status().Source != Live {
				t.Fatal(s.Status())
			}
			allowUnavailable(t, pushPacket(t, s, Fallback, 1, v, v, a, 0, 0, 1600, true, later))
			allowUnavailable(t, pushPacket(t, s, Fallback, 1, a, v, a, skew, skew, 1024, false, later))
			// err содержит результат проверки фактической A/V-готовности заглушки при
			// выбранном LIVE.
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

// Проверяет отсутствие готовности при неизвестных длительностях, восстановление через следующий пакет
// и отказ после истечения сроков или Close.
func TestFallbackReadinessExpiresAndRejectsUnusableDuration(t *testing.T) {
	// s содержит переключатель; v и a задают контрольные дорожки; now задаёт время проверки
	// неизвестных длительностей и устаревания.
	s, v, a, now := switcherFixture(t)
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, v, v, a, 0, 0, 0, true, now))
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, a, v, a, 0, 0, 0, false, now))
	if s.FallbackReady(1, now) == nil {
		t.Fatal("неизвестная длительность прошла барьер")
	}
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, v, v, a, 1600, 1600, 1600, false, now))
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, a, v, a, 1024, 1024, 1024, false, now))
	// err содержит результат готовности после определения длительностей по следующим DTS.
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

// Проверяет, что более старая ревизия управления не снимает уже применённую паузу.
func TestGroupControlIgnoresOlderRevision(t *testing.T) {
	// s содержит переключатель для проверки порядка ревизий ручной паузы.
	s, _, _, _ := switcherFixture(t)
	// g связывает проверяемый Switcher с применением ревизий управления группы.
	g := &Group{switcher: s}
	// err содержит результат применения новой ревизии 20 с включённой паузой.
	if err := g.applyControl(20, true); err != nil {
		t.Fatal(err)
	}
	// err содержит результат пропуска старой ревизии 19, пытающейся снять паузу.
	if err := g.applyControl(19, false); err != nil {
		t.Fatal(err)
	}
	if !s.Status().Paused {
		t.Fatal("старый polling откатил желание")
	}
}

// Проверяет немедленный возврат отмены ожидания и окончательного отказа заглушки вместо бесконечного
// ожидания готовности.
func TestWaitReadyHonorsFinalFailureAndCancellation(t *testing.T) {
	// f предоставляет доменный сервис и точного владельца для проверки WaitReady.
	f := runtimeUnitFixture(t)
	// owner содержит допуск текущего выполнения; err сообщает об отказе его привязки.
	owner, err := f.config.Core.BindExecutionOwner(context.Background(), f.config.Parent)
	if err != nil {
		t.Fatal(err)
	}
	// s содержит переключатель без готовой заглушки для проверки ожидания.
	s, _, _, _ := switcherFixture(t)
	// g содержит группу с действующим владельцем и отдельно задаваемым окончательным отказом
	// заглушки.
	g := &Group{config: f.config.Runtime, owner: owner, switcher: s, done: make(chan struct{})}
	// ctx передаёт уже отменённый контекст в WaitReady; cancel инициирует отмену до вызова.
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

// Проверяет, что историческая готовность заглушки после перехода на LIVE требует новой независимой
// точки старта с аудио.
func TestFallbackReadinessDoesNotReuseDrainedQueueAfterLiveSwitch(t *testing.T) {
	// s содержит переключатель; v и a задают контрольные дорожки; now задаёт первоначальную
	// готовность заглушки перед переходом на LIVE.
	s, v, a, now := switcherFixture(t)
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, v, v, a, 0, 0, 1600, true, now))
	allowUnavailable(t, pushPacket(t, s, Fallback, 1, a, v, a, 0, 0, 1024, false, now))
	// err содержит результат первоначального барьера готовности заглушки.
	if err := s.FallbackReady(1, now); err != nil {
		t.Fatal(err)
	}
	allowUnavailable(t, pushPacket(t, s, Live, 1, v, v, a, 0, 0, 1600, true, now))
	allowUnavailable(t, pushPacket(t, s, Live, 1, a, v, a, 0, 0, 1024, false, now))
	// later задаёт время после восстановления LIVE, когда прежняя очередь заглушки уже дочитана.
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
	// err содержит результат готовности после поступления новой независимой пары заглушки.
	if err := s.FallbackReady(1, later); err != nil {
		t.Fatal(err)
	}
}
