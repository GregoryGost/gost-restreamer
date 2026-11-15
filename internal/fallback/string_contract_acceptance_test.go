package fallback

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"io"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// TestInvalidatedManifestNeverReopensButHealthySiblingDoes проверяет каждое
// повторное OpenVerified на синтетическом файле без FFmpeg. История аннулированного
// manifest остаётся читаемой, но не даёт файлового дескриптора даже при совпадении
// нового desired с прежними байтами. t сообщает о нарушении границы использования.
func TestInvalidatedManifestNeverReopensButHealthySiblingDoes(t *testing.T) {
	// files содержит изолированное файловое дерево; profile задаёт пригодный прежний рецепт.
	files, _ := selfFileStore(t)
	profile := selfProfile(core.CodecH264)
	// key и canonical удостоверяют полный desired без имитации медиапарсера; err сообщает об отказе.
	key, canonical, err := core.ProfileIdentity(profile)
	if err != nil {
		t.Fatal(err)
	}
	// content содержит синтетические байты: OpenVerified проверяет целостность, а не контейнер.
	content := []byte("synthetic verified artifact bytes")
	// digest задаёт независимый SHA256 фиксированного содержимого.
	digest := sha256.Sum256(content)
	// file создаёт принадлежащий тесту файл для положительного контроля чтения.
	file, err := files.Create("healthy.mp4")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.Write(content); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	// healthy содержит полный пригодный manifest с проверенным содержимым и текущим генератором.
	healthy := core.ArtifactManifest{GenerationID: "generation", VariantID: "healthy", AssetID: "asset", SourceHash: "source", Profile: profile, ProfileKey: key, CanonicalProfile: canonical,
		VerifiedFormat: profile.Format, GeneratorVersion: core.CurrentFallbackGeneratorVersion, ManagedPath: "healthy.mp4", SizeBytes: uint64(len(content)), ContentHash: hex.EncodeToString(digest[:])}
	// changed задаёт миграционный факт аннулирования даже при совпадении измерения с новым desired.
	changed := healthy
	changed.RequiresRebuild = true
	if err = changed.Validate(); err != nil {
		t.Fatal("История аннулированного manifest потеряна", err)
	}
	// iteration повторяет реальные открытия, чтобы первый успешный вызов не кэшировал допуск.
	for iteration := range 3 {
		// opened должен отсутствовать у аннулированного manifest; denied сохраняет предметную неготовность.
		opened, denied := files.OpenVerified(context.Background(), changed)
		if opened != nil || !errors.Is(denied, core.ErrNotReady) {
			t.Fatalf("Открыт аннулированный файл на попытке %d: %v", iteration, denied)
		}
		// sibling проверяет пригодный manifest после отказа; readErr сохраняет отказ открытия и чтения.
		sibling, readErr := files.OpenVerified(context.Background(), healthy)
		if readErr != nil {
			t.Fatal("Аннулирование заблокировало пригодного соседа", readErr)
		}
		// actual содержит все байты нового дескриптора с исходной позиции 0.
		actual, readErr := io.ReadAll(sibling)
		// closeErr сохраняет отказ освобождения независимо открытого дескриптора.
		if closeErr := sibling.Close(); closeErr != nil {
			t.Fatal(closeErr)
		}
		if readErr != nil || string(actual) != string(content) {
			t.Fatal("Повторное открытие изменило пригодный файл", string(actual), readErr)
		}
	}
	changed.ManagedPath = "absent.mp4"
	if _, err = files.OpenVerified(context.Background(), changed); !errors.Is(err, core.ErrNotReady) {
		t.Fatal("Аннулированный manifest дошёл до файлового ввода-вывода", err)
	}
	changed.ManagedPath = healthy.ManagedPath
	changed.VerifiedFormat.Video.CodecProfile = "high10"
	if err = changed.Validate(); err != nil {
		t.Fatal("История не допускает структурно корректное старое измерение", err)
	}
	if _, err = files.OpenVerified(context.Background(), changed); !errors.Is(err, core.ErrNotReady) {
		t.Fatal("Старое измерение использовано как новый desired", err)
	}
	changed.VerifiedFormat.Video.Width = 0
	if err = changed.Validate(); err == nil {
		t.Fatal("RequiresRebuild скрыл структурное повреждение raw-измерения")
	}
	healthy.GeneratorVersion = "generator-v1"
	if _, err = files.OpenVerified(context.Background(), healthy); !errors.Is(err, core.ErrNotReady) {
		t.Fatal("Файл прежнего генератора принят для нового использования", err)
	}
}
