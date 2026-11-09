package fallback

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

type normalizationInspection struct {
	acceptanceInspection
	verify func(context.Context, *os.File, core.GroupProfile) (core.MediaFormat, error)
}

func (i normalizationInspection) Verify(ctx context.Context, f *os.File, p core.GroupProfile) (core.MediaFormat, error) {
	return i.verify(ctx, f, p)
}

func normalizedPayload(index int) []byte {
	return fmt.Appendf(nil, "normalized-complete-media-for-profile-%d", index)
}

func normalizationFileBytes(f *os.File, expected []byte) error {
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	got, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	if !bytes.Equal(got, expected) {
		return fmt.Errorf("получены байты %q вместо %q", got, expected)
	}
	return nil
}

func TestManagerPublishesOnlyCompleteNormalizedBundle(t *testing.T) {
	for _, failure := range []string{"none", "normalize", "normalized-limit", "verify", "loop-duration"} {
		t.Run(failure, func(t *testing.T) {
			svc, _, asset, _ := acceptanceManagerService(t)
			files, directory := acceptanceFileStore(t)
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			raw := []byte("raw-generator-bytes-with-unadjusted-timing")
			runner := acceptanceRunFunc(func(_ context.Context, _ Command, _ *os.File, out io.Writer) (RunResult, error) {
				_, err := out.Write(raw)
				return RunResult{}, err
			})
			baseline := acceptanceNewManager(t, svc, files, acceptanceManagerConfig(), &acceptanceInspection{}, runner)
			original := acceptancePNG(t)
			old := acceptanceUpload(t, baseline, svc, asset.ID, original).Generation
			other, err := svc.CreateAsset(ctx, "Сохранённая отдельная заглушка")
			if err != nil {
				t.Fatal(err)
			}
			otherGeneration := acceptanceUpload(t, baseline, svc, other.ID, original).Generation
			otherBefore, err := svc.GetAsset(ctx, other.ID)
			if err != nil {
				t.Fatal(err)
			}
			injected := errors.New("контролируемый отказ завершающей подготовки")
			normalized, verified, loops := 0, 0, 0
			config := acceptanceManagerConfig()
			// Контролируемые порты проверяют файловый протокол Manager, а не декодирование кодеков.
			pending := func(ctx context.Context) (core.FallbackGeneration, error) {
				a, err := svc.GetAsset(ctx, asset.ID)
				if err != nil {
					return core.FallbackGeneration{}, err
				}
				if a.CurrentGenerationID != old.ID || a.PendingGenerationID == "" {
					return core.FallbackGeneration{}, errors.New("current изменён до завершения проверки комплекта")
				}
				return svc.GetGeneration(ctx, a.PendingGenerationID)
			}
			tools := Tools{
				Builder: func(GenerateRequest) (Command, error) { return Command{Binary: "controlled"}, nil },
				Runner:  runner,
				Normalize: func(ctx context.Context, input *os.File, out io.Writer) error {
					g, err := pending(ctx)
					if err != nil {
						return err
					}
					if normalized >= len(g.RequiredProfiles) {
						return errors.New("лишняя нормализация профиля")
					}
					p := g.RequiredProfiles[normalized]
					ledger, err := svc.ListGenerationFiles(ctx, core.PageRequest{Query: core.ListQuery{Limit: 100, GenerationID: g.ID}})
					if err != nil {
						return err
					}
					registered := false
					for _, file := range ledger.Items {
						if file.ManagedPath == p.StagingPath+".normalized" && file.Kind == core.OwnedStaging && file.AssetID == asset.ID {
							registered = true
						}
					}
					if !registered {
						return errors.New("нормализация начата без принадлежащей поколению записи staging")
					}
					if position, err := input.Seek(0, io.SeekCurrent); err != nil || position != 0 {
						return errors.New("raw-дескриптор передан не с начала")
					}
					if err = normalizationFileBytes(input, raw); err != nil {
						return err
					}
					data := normalizedPayload(normalized)
					normalized++
					if normalized == 2 && failure == "normalized-limit" {
						data = bytes.Repeat([]byte{0x5a}, int(config.MaxOutputBytes)+1)
					}
					if _, err = out.Write(data); err != nil {
						return err
					}
					if normalized == 2 && failure == "normalize" {
						return injected
					}
					return nil
				},
				Inspector: &normalizationInspection{verify: func(ctx context.Context, input *os.File, p core.GroupProfile) (core.MediaFormat, error) {
					if _, err := pending(ctx); err != nil {
						return core.MediaFormat{}, err
					}
					if normalized != verified+1 {
						return core.MediaFormat{}, errors.New("Verify начат до нормализации своего профиля")
					}
					if err := normalizationFileBytes(input, normalizedPayload(verified)); err != nil {
						return core.MediaFormat{}, err
					}
					verified++
					if verified == 2 && failure == "verify" {
						return core.MediaFormat{}, injected
					}
					return p.Format, nil
				}},
				VerifyLoopDuration: func(ctx context.Context, input *os.File, duration time.Duration) error {
					if _, err := pending(ctx); err != nil {
						return err
					}
					// 1,6 с содержат ровно 48 кадров 30 FPS и 75 пакетов AAC по 1024/48000 с.
					if duration != 1600*time.Millisecond || verified != loops+1 {
						return fmt.Errorf("неверная длина или порядок финальной проверки: %s", duration)
					}
					if err := normalizationFileBytes(input, normalizedPayload(loops)); err != nil {
						return err
					}
					loops++
					if loops == 2 && failure == "loop-duration" {
						return injected
					}
					return nil
				},
			}
			manager, err := NewManagerWithTools(svc, files, config, tools)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				closeCtx, stop := context.WithTimeout(context.Background(), 2*time.Second)
				defer stop()
				if err := manager.Close(closeCtx); err != nil {
					t.Error(err)
				}
			})
			a, err := svc.GetAsset(ctx, asset.ID)
			if err != nil {
				t.Fatal(err)
			}
			job, err := manager.Upload(ctx, core.PreparationRequest{AssetID: a.ID, ExpectedRevision: a.Revision, Kind: core.AssetImage}, io.NopCloser(bytes.NewReader(original)))
			if err != nil {
				t.Fatal(err)
			}
			result, waitErr := job.Wait(ctx)
			if !result.Clean || normalized != 2 {
				t.Fatalf("подготовка не завершила два профиля и физическую очистку: %+v normalized=%d error=%v", result, normalized, waitErr)
			}
			generation, err := svc.GetGeneration(ctx, job.Generation().ID)
			if err != nil {
				t.Fatal(err)
			}
			current, err := svc.GetAsset(ctx, asset.ID)
			if err != nil || current.PendingGenerationID != "" || current.State != core.AssetReady || generation.Issued || generation.CleanupPending {
				t.Fatalf("неверное завершение подготовки: asset=%+v generation=%+v error=%v", current, generation, err)
			}
			if failure == "none" {
				if waitErr != nil || verified != 2 || loops != 2 || generation.State != core.PreparationPublished || current.CurrentGenerationID != generation.ID || generation.ID == old.ID {
					t.Fatalf("не опубликован полный новый комплект: %+v verified=%d loops=%d error=%v", result, verified, loops, waitErr)
				}
				for index, required := range generation.RequiredProfiles {
					variant, err := svc.GetVariant(ctx, required.VariantID)
					if err != nil {
						t.Fatal(err)
					}
					data := normalizedPayload(index)
					if variant.GenerationID != generation.ID || variant.State != core.VariantReady || variant.SizeBytes != uint64(len(data)) || variant.ContentHash != acceptanceDigest(data) || variant.ContentHash == acceptanceDigest(raw) || variant.ManagedPath != required.ManagedPath {
						t.Fatalf("метаданные не соответствуют нормализованному файлу: %+v", variant)
					}
					manifest := core.ArtifactManifest{AssetID: asset.ID, GenerationID: generation.ID, VariantID: variant.ID, Profile: variant.Profile, ProfileKey: variant.ProfileKey, CanonicalProfile: variant.CanonicalProfile, GeneratorVersion: variant.GeneratorVersion, SourceHash: generation.SourceHash, ManagedPath: variant.ManagedPath, SizeBytes: variant.SizeBytes, ContentHash: variant.ContentHash, VerifiedFormat: variant.VerifiedFormat}
					if err = manifest.Validate(); err != nil {
						t.Fatal(err)
					}
					file, err := files.OpenVerified(ctx, manifest)
					if err != nil {
						t.Fatal(err)
					}
					readErr := normalizationFileBytes(file, data)
					acceptanceClose(t, file)
					if readErr != nil {
						t.Fatal(readErr)
					}
					for _, path := range []string{required.StagingPath, required.StagingPath + ".normalized"} {
						if _, err = os.Stat(filepath.Join(directory, path)); !errors.Is(err, os.ErrNotExist) {
							t.Fatalf("после публикации сохранился staging %s: %v", path, err)
						}
					}
				}
			} else {
				wantErr := injected
				if failure == "normalized-limit" {
					wantErr = ErrLimit
				}
				if !errors.Is(waitErr, wantErr) || generation.State != core.PreparationFailed || current.CurrentGenerationID != old.ID {
					t.Fatalf("ошибка заменила current или потеряла причину: %+v current=%s error=%v", generation, current.CurrentGenerationID, waitErr)
				}
				variants, err := svc.ListVariants(ctx, core.PageRequest{Query: core.ListQuery{Limit: 100, GenerationID: generation.ID}})
				if err != nil || len(variants.Items) != 0 {
					t.Fatalf("частичный комплект получил опубликованные variants: %+v, %v", variants, err)
				}
				normalizationCleanup(t, ctx, manager, svc, generation, directory)
			}
			for _, g := range []core.FallbackGeneration{old, otherGeneration} {
				file, err := files.OpenExact(ctx, g.OriginalPath, g.SizeBytes, g.SourceHash)
				if err != nil {
					t.Fatal(err)
				}
				readErr := normalizationFileBytes(file, original)
				acceptanceClose(t, file)
				if readErr != nil {
					t.Fatal(readErr)
				}
				for _, required := range g.RequiredProfiles {
					variant, err := svc.GetVariant(ctx, required.VariantID)
					if err != nil || variant.ContentHash != acceptanceDigest(raw) {
						t.Fatalf("старый вариант изменён: %+v, %v", variant, err)
					}
					file, err := files.OpenExact(ctx, variant.ManagedPath, variant.SizeBytes, variant.ContentHash)
					if err != nil {
						t.Fatal(err)
					}
					acceptanceClose(t, file)
				}
			}
			otherAfter, err := svc.GetAsset(ctx, other.ID)
			if err != nil || !reflect.DeepEqual(otherBefore, otherAfter) {
				t.Fatalf("замена изменила другой asset: before=%+v after=%+v error=%v", otherBefore, otherAfter, err)
			}
		})
	}
}

func normalizationCleanup(t *testing.T, ctx context.Context, manager *Manager, svc *core.Service, g core.FallbackGeneration, directory string) {
	t.Helper()
	ledger, err := svc.ListGenerationFiles(ctx, core.PageRequest{Query: core.ListQuery{Limit: 100, GenerationID: g.ID}})
	if err != nil {
		t.Fatal(err)
	}
	intents, err := svc.ListCleanups(ctx, core.PageRequest{Query: core.ListQuery{Limit: 100, GenerationID: g.ID, PendingOnly: true}})
	if err != nil || len(intents.Items) != len(ledger.Items) {
		t.Fatalf("cleanup не покрывает весь ledger: intents=%d files=%d error=%v", len(intents.Items), len(ledger.Items), err)
	}
	registered := map[string]core.OwnedFile{}
	for _, file := range ledger.Items {
		registered[file.ManagedPath] = file
	}
	for _, profile := range g.RequiredProfiles {
		if registered[profile.StagingPath+".normalized"].Kind != core.OwnedStaging {
			t.Fatal("не сохранена запись нормализованного staging после отказа")
		}
	}
	var directoryIntent core.CleanupIntent
	for _, intent := range intents.Items {
		file, ok := registered[intent.ManagedPath]
		if !ok || file.ID != intent.FileID || file.Kind != intent.FileKind || intent.Kind != core.CleanupFile || intent.ArtifactGenerationID != g.ID {
			t.Fatalf("cleanup не соответствует точному ledger: %+v", intent)
		}
		delete(registered, intent.ManagedPath)
		if intent.FileKind == core.OwnedDirectory {
			directoryIntent = intent
		} else if err := manager.CleanupFile(ctx, intent); err != nil {
			t.Fatalf("очистка %s: %v", intent.ManagedPath, err)
		}
	}
	if len(registered) != 0 || directoryIntent.ID == "" {
		t.Fatal("cleanup пропустил принадлежащие поколению пути")
	}
	if err := manager.CleanupFile(ctx, directoryIntent); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Stat(filepath.Join(directory, core.GenerationDirectory(g.ID))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("после точного cleanup остался каталог поколения: %v", err)
	}
}
