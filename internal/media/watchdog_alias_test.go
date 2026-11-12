package media_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
)

// Проверяет взаимное присваивание media/core WatchdogPolicy и сохранение идентичности
// ErrInvalidWatchdogPolicy через обёрнутую ошибку.
func TestWatchdogAliasPreservesAssignmentAndErrorIdentity(t *testing.T) {
	// toDomain проверяет присваивание политики media доменному типу без преобразования.
	toDomain := func(p media.WatchdogPolicy) core.WatchdogPolicy {
		// Возвращает политику media как core.WatchdogPolicy, проверяя совместимость присваивания на уровне
		// типов.
		return p
	}
	// toMedia проверяет обратное присваивание доменной политики типу media без преобразования.
	toMedia := func(p core.WatchdogPolicy) media.WatchdogPolicy {
		// Возвращает политику core как media.WatchdogPolicy, проверяя совместимость присваивания на уровне
		// типов.
		return p
	}
	// domain содержит политику после присваивания из media в core.
	domain := toDomain(media.WatchdogPolicy{VideoTimeout: 1, AudioTimeout: 2, RecoveryWindow: 3, KeyframeTimeout: 4})
	// compatible содержит политику после обратного присваивания из core в media.
	compatible := toMedia(domain)
	if compatible != domain || compatible.Validate() != nil {
		t.Fatal("совместимое присваивание изменило политику")
	}
	if media.ErrInvalidWatchdogPolicy != core.ErrInvalidWatchdogPolicy {
		t.Fatal("изменена идентичность ошибки WatchdogPolicy")
	}
	// category обозначает одну из двух ссылок на общую категорию ошибки политики контроля источника.
	for _, category := range []error{media.ErrInvalidWatchdogPolicy, core.ErrInvalidWatchdogPolicy} {
		if !errors.Is(fmt.Errorf("контекст: %w", (media.WatchdogPolicy{}).Validate()), category) {
			t.Fatalf("обёрнутая ошибка потеряла категорию %v", category)
		}
	}
}
