package media_test

import (
	"errors"
	"fmt"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
)

func TestWatchdogAliasPreservesAssignmentAndErrorIdentity(t *testing.T) {
	toDomain := func(p media.WatchdogPolicy) core.WatchdogPolicy { return p }
	toMedia := func(p core.WatchdogPolicy) media.WatchdogPolicy { return p }
	domain := toDomain(media.WatchdogPolicy{VideoTimeout: 1, AudioTimeout: 2, RecoveryWindow: 3, KeyframeTimeout: 4})
	compatible := toMedia(domain)
	if compatible != domain || compatible.Validate() != nil {
		t.Fatal("совместимое присваивание изменило политику")
	}
	if media.ErrInvalidWatchdogPolicy != core.ErrInvalidWatchdogPolicy {
		t.Fatal("изменена идентичность ошибки WatchdogPolicy")
	}
	for _, category := range []error{media.ErrInvalidWatchdogPolicy, core.ErrInvalidWatchdogPolicy} {
		if !errors.Is(fmt.Errorf("контекст: %w", (media.WatchdogPolicy{}).Validate()), category) {
			t.Fatalf("обёрнутая ошибка потеряла категорию %v", category)
		}
	}
}
