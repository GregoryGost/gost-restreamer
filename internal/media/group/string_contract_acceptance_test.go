package group

import (
	"context"
	"errors"
	"os"
	"sync/atomic"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
)

// TestRegistryRejectsInvalidatedArtifactBeforeIO проверяет новый и повторный Acquire
// аннулированного снимка. t контролирует файловый порт и сохранность пригодной группы.
func TestRegistryRejectsInvalidatedArtifactBeforeIO(t *testing.T) {
	// fixture предоставляет настоящий сохранённый снимок без запуска внешних процессов.
	fixture := runtimeUnitFixture(t)
	// opens считает обращения к файловому порту; отказ исключает запуск проигрывателя.
	var opens atomic.Uint64
	fixture.config.Runtime.OpenFile = func(context.Context, core.ArtifactManifest) (*os.File, error) {
		// Обработчик учитывает открытие, возвращая отсутствие файла до создания процесса.
		opens.Add(1)
		return nil, os.ErrNotExist
	}
	// ctx ограничивает операции реестра текущим сценарием.
	ctx := context.Background()
	// registry создаёт настоящий реестр; err сохраняет ошибки его операций.
	registry, err := NewRegistry(ctx, fixture.config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Обработчик закрывает созданные группы; closeErr сообщает об ошибке очистки.
		if closeErr := registry.Close(ctx); closeErr != nil {
			t.Error(closeErr)
		}
	})
	// live создаёт изолированный входной Router без сетевого транспорта.
	live, err := media.NewRouter(fixture.config.Runtime.Switcher.Router)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Обработчик освобождает изолированный Router; closeErr сообщает об ошибке очистки.
		if closeErr := live.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	// invalid сохраняет структуру снимка, но неподдержанный генератор запрещает его исполнение.
	invalid := fixture.snapshot
	invalid.Artifact.GeneratorVersion = "unsupported-generator"
	if err = invalid.Validate(); err != nil {
		t.Fatal("Структура снимка должна оставаться корректной", err)
	}
	// reference не должен появиться для нового непригодного снимка;
	// err должен сообщить о неготовности до обращения к файловому порту.
	if reference, err := registry.Acquire(ctx, invalid, live); !errors.Is(err, core.ErrNotReady) || reference != nil {
		t.Fatal("Аннулированный файл получил группу", reference, err)
	}
	if opens.Load() != 0 || len(registry.groups) != 0 || registry.references != 0 {
		t.Fatal("Отказ создал группу, ссылку или открыл файл")
	}
	// healthy проверяет сохранение пригодности неизменённого снимка.
	healthy, err := registry.Acquire(ctx, fixture.snapshot, live)
	if err != nil {
		t.Fatal("Неизменённый снимок отклонён", err)
	}
	// reference не должен переиспользовать уже зарегистрированную пригодную группу;
	// err должен сохранить отказ аннулированного снимка при повторном обращении.
	if reference, err := registry.Acquire(ctx, invalid, live); !errors.Is(err, core.ErrNotReady) || reference != nil {
		t.Fatal("Повторный Acquire обошёл аннулирование", reference, err)
	}
	if registry.references != 1 || len(registry.groups) != 1 {
		t.Fatal("Отказ изменил число пригодных групп или ссылок")
	}
	// repeated обязан получить ту же пригодную группу после отклонённого снимка.
	repeated, err := registry.Acquire(ctx, fixture.snapshot, live)
	if err != nil || repeated.Group() != healthy.Group() {
		t.Fatal("Отказ повредил пригодную группу", err)
	}
	if err = repeated.Close(); err != nil {
		t.Fatal(err)
	}
	if err = healthy.Close(); err != nil {
		t.Fatal(err)
	}
}
