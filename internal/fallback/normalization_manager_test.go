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

// normalizationInspection подставляет проверку нормализованного варианта, сохраняя настоящую проверку
// изображения.
type normalizationInspection struct {
	// acceptanceInspection предоставляет проверку исходного изображения.
	acceptanceInspection
	// verify задаёт управляемую проверку нормализованного файла и профиля.
	verify func(context.Context, *os.File, core.GroupProfile) (core.MediaFormat, error)
}

// Verify передаёт проверку файла и профиля управляемой функции теста.
func (i normalizationInspection) Verify(ctx context.Context, f *os.File, p core.GroupProfile) (core.MediaFormat, error) {
	return i.verify(ctx, f, p)
}

// Возвращает различимое содержимое нормализованного варианта для заданного номера профиля.
func normalizedPayload(index int) []byte {
	return fmt.Appendf(nil, "normalized-complete-media-for-profile-%d", index)
}

// Читает файл с начала и сравнивает его байты с ожидаемыми, возвращая ошибку чтения либо несовпадения.
func normalizationFileBytes(f *os.File, expected []byte) error {
	// err сообщает об отказе перемотки проверяемого файла в начало.
	if _, err := f.Seek(0, io.SeekStart); err != nil {
		return err
	}
	// got содержит прочитанные байты файла. err сообщает об отказе чтения.
	got, err := io.ReadAll(f)
	if err != nil {
		return err
	}
	if !bytes.Equal(got, expected) {
		return fmt.Errorf("получены байты %q вместо %q", got, expected)
	}
	return nil
}

// Проверяет публикацию только полного проверенного нормализованного комплекта; отказ на втором профиле
// сохраняет прежнее поколение и другой Asset и оставляет точные намерения очистки.
func TestManagerPublishesOnlyCompleteNormalizedBundle(t *testing.T) {
	// failure выбирает успешный комплект либо отказ второго профиля на одном из завершающих этапов.
	for _, failure := range []string{"none", "normalize", "normalized-limit", "verify", "loop-duration"} {
		t.Run(failure, func(t *testing.T) {
			// Проверяет публикацию полного нормализованного комплекта либо сохранение прежнего поколения при
			// выбранном отказе второго профиля.

			// svc содержит тестовый доменный сервис. asset содержит Asset проверяемой
			// подготовки.
			svc, _, asset, _ := acceptanceManagerService(t)
			// files содержит файловое хранилище. directory содержит его корень для проверки
			// промежуточных путей.
			files, directory := acceptanceFileStore(t)
			// ctx ограничивает сценарий десятью секундами. cancel освобождает срок.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			// raw содержит различимые ненормализованные байты подставленного генератора.
			raw := []byte("raw-generator-bytes-with-unadjusted-timing")
			// runner записывает исходные ненормализованные байты каждого варианта.
			runner := acceptanceRunFunc(func(_ context.Context, _ Command, _ *os.File, out io.Writer) (RunResult, error) {
				// Записывает условный исходный результат генератора в ограниченный выход и возвращает ошибку записи.

				// err сообщает об отказе записи ненормализованного вывода.
				_, err := out.Write(raw)
				return RunResult{}, err
			})
			// baseline содержит менеджер для создания прежних рабочих поколений.
			baseline := acceptanceNewManager(t, svc, files, acceptanceManagerConfig(), &acceptanceInspection{}, runner)
			// original содержит исходные PNG-байты, сохраняемые во всех поколениях.
			original := acceptancePNG(t)
			// old содержит прежнее выбранное поколение проверяемого Asset.
			old := acceptanceUpload(t, baseline, svc, asset.ID, original).Generation
			// other содержит отдельный Asset для проверки изоляции замены. err сообщает об
			// отказе его создания.
			other, err := svc.CreateAsset(ctx, "Сохранённая отдельная заглушка")
			if err != nil {
				t.Fatal(err)
			}
			// otherGeneration содержит опубликованное поколение отдельного Asset.
			otherGeneration := acceptanceUpload(t, baseline, svc, other.ID, original).Generation
			// otherBefore сохраняет состояние отдельного Asset до проверяемой замены.
			otherBefore, err := svc.GetAsset(ctx, other.ID)
			if err != nil {
				t.Fatal(err)
			}
			// injected содержит ожидаемую управляемую ошибку завершающего этапа второго
			// профиля.
			injected := errors.New("контролируемый отказ завершающей подготовки")
			// normalized считает нормализованные профили. verified считает проверенные
			// форматы. loops считает проверенные длительности петель.
			normalized, verified, loops := 0, 0, 0
			// config содержит конечные бюджеты проверяемого менеджера.
			config := acceptanceManagerConfig()
			// Контролируемые порты проверяют файловый протокол Manager, а не декодирование кодеков.
			// pending проверяет сохранение прежнего current и возвращает ещё не
			// опубликованное поколение.
			pending := func(ctx context.Context) (core.FallbackGeneration, error) {
				// Проверяет сохранение прежнего опубликованного поколения во время подготовки и возвращает поколение,
				// ожидающее публикации.

				// a содержит состояние Asset во время подготовки. err сообщает об отказе
				// чтения.
				a, err := svc.GetAsset(ctx, asset.ID)
				if err != nil {
					return core.FallbackGeneration{}, err
				}
				if a.CurrentGenerationID != old.ID || a.PendingGenerationID == "" {
					return core.FallbackGeneration{}, errors.New("current изменён до завершения проверки комплекта")
				}
				return svc.GetGeneration(ctx, a.PendingGenerationID)
			}
			// tools содержит управляемые порты нормализации, проверки формата и
			// длительности.
			tools := Tools{
				Builder: func(GenerateRequest) (Command, error) {
					// Возвращает условную команду controlled для проверки файлового протокола без запуска кодировщика.
					return Command{Binary: "controlled"}, nil
				},
				Runner: runner,
				Normalize: func(ctx context.Context, input *os.File, out io.Writer) error {
					// Проверяет предварительную регистрацию нормализованного файла и исходные байты, затем записывает
					// результат профиля либо вводит выбранный отказ второго варианта.

					// g содержит ожидающее поколение текущей нормализации. err
					// сообщает о преждевременной смене current или отказе чтения.
					g, err := pending(ctx)
					if err != nil {
						return err
					}
					if normalized >= len(g.RequiredProfiles) {
						return errors.New("лишняя нормализация профиля")
					}
					// p содержит требуемый профиль, соответствующий номеру текущей
					// нормализации.
					p := g.RequiredProfiles[normalized]
					// ledger содержит реестр файлов поколения для проверки
					// предварительной регистрации normalized.
					ledger, err := svc.ListGenerationFiles(ctx, core.PageRequest{Query: core.ListQuery{Limit: 100, GenerationID: g.ID}})
					if err != nil {
						return err
					}
					// registered отмечает наличие точной записи нормализованного
					// промежуточного файла до начала записи.
					registered := false
					// file обозначает очередную запись реестра при поиске
					// нормализованного пути.
					for _, file := range ledger.Items {
						if file.ManagedPath == p.StagingPath+".normalized" && file.Kind == core.OwnedStaging && file.AssetID == asset.ID {
							registered = true
						}
					}
					if !registered {
						return errors.New("нормализация начата без принадлежащей поколению записи staging")
					}
					// position содержит текущую позицию ненормализованного входного
					// дескриптора. err сообщает об отказе её проверки.
					if position, err := input.Seek(0, io.SeekCurrent); err != nil || position != 0 {
						return errors.New("raw-дескриптор передан не с начала")
					}
					if err = normalizationFileBytes(input, raw); err != nil {
						return err
					}
					// data содержит различимые нормализованные байты профиля либо
					// блок сверх бюджета.
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
					// Проверяет последовательность нормализации и верификации и точные нормализованные байты; при
					// выбранном отказе второго профиля возвращает injected.

					// err сообщает об отказе проверки ожидающего поколения перед
					// верификацией.
					if _, err := pending(ctx); err != nil {
						return core.MediaFormat{}, err
					}
					if normalized != verified+1 {
						return core.MediaFormat{}, errors.New("Verify начат до нормализации своего профиля")
					}
					// err сообщает об отказе чтения либо несовпадении
					// нормализованных байтов перед проверкой формата.
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
					// Проверяет петлю 1,6 с после верификации соответствующего профиля и его байты; при выбранном отказе
					// второй проверки возвращает injected.

					// err сообщает об отказе проверки ожидающего поколения перед
					// сверкой периода.
					if _, err := pending(ctx); err != nil {
						return err
					}
					// 1,6 с содержат ровно 48 кадров 30 FPS и 75 пакетов AAC по 1024/48000 с.
					if duration != 1600*time.Millisecond || verified != loops+1 {
						return fmt.Errorf("неверная длина или порядок финальной проверки: %s", duration)
					}
					// err сообщает об отказе чтения либо несовпадении байтов перед
					// проверкой длительности петли.
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
			// manager содержит менеджер с проверяемыми завершающими портами.
			manager, err := NewManagerWithTools(svc, files, config, tools)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				// Закрывает Manager с отдельным двухсекундным сроком и сообщает ошибку через t.Error.

				// closeCtx ограничивает закрытие менеджера двумя секундами. stop
				// освобождает срок.
				closeCtx, stop := context.WithTimeout(context.Background(), 2*time.Second)
				defer stop()
				// err сообщает об отказе закрытия менеджера.
				if err := manager.Close(closeCtx); err != nil {
					t.Error(err)
				}
			})
			// a содержит актуальный Asset перед новой загрузкой.
			a, err := svc.GetAsset(ctx, asset.ID)
			if err != nil {
				t.Fatal(err)
			}
			// job содержит принятое задание замены полного комплекта.
			job, err := manager.Upload(ctx, core.PreparationRequest{AssetID: a.ID, ExpectedRevision: a.Revision, Kind: core.AssetImage}, io.NopCloser(bytes.NewReader(original)))
			if err != nil {
				t.Fatal(err)
			}
			// result содержит завершённую подготовку и очистку. waitErr содержит отказ
			// выбранного завершающего этапа.
			result, waitErr := job.Wait(ctx)
			if !result.Clean || normalized != 2 {
				t.Fatalf("подготовка не завершила два профиля и физическую очистку: %+v normalized=%d error=%v", result, normalized, waitErr)
			}
			// generation содержит сохранённое поколение после завершения задания.
			generation, err := svc.GetGeneration(ctx, job.Generation().ID)
			if err != nil {
				t.Fatal(err)
			}
			// current содержит состояние Asset после успеха либо отказа замены.
			current, err := svc.GetAsset(ctx, asset.ID)
			if err != nil || current.PendingGenerationID != "" || current.State != core.AssetReady || generation.Issued || generation.CleanupPending {
				t.Fatalf("неверное завершение подготовки: asset=%+v generation=%+v error=%v", current, generation, err)
			}
			if failure == "none" {
				if waitErr != nil || verified != 2 || loops != 2 || generation.State != core.PreparationPublished || current.CurrentGenerationID != generation.ID || generation.ID == old.ID {
					t.Fatalf("не опубликован полный новый комплект: %+v verified=%d loops=%d error=%v", result, verified, loops, waitErr)
				}
				// index задаёт номер профиля в комплекте. required содержит
				// соответствующий требуемый вариант.
				for index, required := range generation.RequiredProfiles {
					// variant содержит опубликованный вариант текущего профиля. err
					// сообщает об отказе чтения.
					variant, err := svc.GetVariant(ctx, required.VariantID)
					if err != nil {
						t.Fatal(err)
					}
					// data содержит ожидаемые нормализованные байты текущего
					// профиля.
					data := normalizedPayload(index)
					if variant.GenerationID != generation.ID || variant.State != core.VariantReady || variant.SizeBytes != uint64(len(data)) || variant.ContentHash != acceptanceDigest(data) || variant.ContentHash == acceptanceDigest(raw) || variant.ManagedPath != required.ManagedPath {
						t.Fatalf("метаданные не соответствуют нормализованному файлу: %+v", variant)
					}
					// manifest объединяет сохранённые сведения опубликованного
					// варианта для проверки файла.
					manifest := core.ArtifactManifest{AssetID: asset.ID, GenerationID: generation.ID, VariantID: variant.ID, Profile: variant.Profile, ProfileKey: variant.ProfileKey, CanonicalProfile: variant.CanonicalProfile, GeneratorVersion: variant.GeneratorVersion, SourceHash: generation.SourceHash, ManagedPath: variant.ManagedPath, SizeBytes: variant.SizeBytes, ContentHash: variant.ContentHash, VerifiedFormat: variant.VerifiedFormat}
					if err = manifest.Validate(); err != nil {
						t.Fatal(err)
					}
					// file содержит проверенный дескриптор опубликованного
					// нормализованного варианта.
					file, err := files.OpenVerified(ctx, manifest)
					if err != nil {
						t.Fatal(err)
					}
					// readErr сообщает об отказе чтения или несовпадении
					// опубликованных байтов.
					readErr := normalizationFileBytes(file, data)
					acceptanceClose(t, file)
					if readErr != nil {
						t.Fatal(readErr)
					}
					// path содержит исходный или нормализованный промежуточный путь,
					// который должен отсутствовать после публикации.
					for _, path := range []string{required.StagingPath, required.StagingPath + ".normalized"} {
						if _, err = os.Stat(filepath.Join(directory, path)); !errors.Is(err, os.ErrNotExist) {
							t.Fatalf("после публикации сохранился staging %s: %v", path, err)
						}
					}
				}
			} else {
				// wantErr содержит ожидаемую управляемую ошибку либо ErrLimit при
				// превышении нормализованного вывода.
				wantErr := injected
				if failure == "normalized-limit" {
					wantErr = ErrLimit
				}
				if !errors.Is(waitErr, wantErr) || generation.State != core.PreparationFailed || current.CurrentGenerationID != old.ID {
					t.Fatalf("ошибка заменила current или потеряла причину: %+v current=%s error=%v", generation, current.CurrentGenerationID, waitErr)
				}
				// variants содержит опубликованные варианты отказавшего поколения. err
				// сообщает об отказе чтения списка.
				variants, err := svc.ListVariants(ctx, core.PageRequest{Query: core.ListQuery{Limit: 100, GenerationID: generation.ID}})
				if err != nil || len(variants.Items) != 0 {
					t.Fatalf("частичный комплект получил опубликованные variants: %+v, %v", variants, err)
				}
				normalizationCleanup(t, ctx, manager, svc, generation, directory)
			}
			// g содержит прежнее поколение проверяемого либо отдельного Asset для проверки
			// сохранности.
			for _, g := range []core.FallbackGeneration{old, otherGeneration} {
				// file содержит проверенный исходник сохраняемого поколения. err
				// сообщает об отказе открытия.
				file, err := files.OpenExact(ctx, g.OriginalPath, g.SizeBytes, g.SourceHash)
				if err != nil {
					t.Fatal(err)
				}
				// readErr сообщает об отказе чтения либо изменении исходных PNG-байтов.
				readErr := normalizationFileBytes(file, original)
				acceptanceClose(t, file)
				if readErr != nil {
					t.Fatal(readErr)
				}
				// required обозначает очередной профиль сохраняемого поколения.
				for _, required := range g.RequiredProfiles {
					// variant содержит прежний вариант для проверки неизменности
					// SHA256. err сообщает об отказе чтения.
					variant, err := svc.GetVariant(ctx, required.VariantID)
					if err != nil || variant.ContentHash != acceptanceDigest(raw) {
						t.Fatalf("старый вариант изменён: %+v, %v", variant, err)
					}
					// file содержит проверенный дескриптор прежнего варианта.
					file, err := files.OpenExact(ctx, variant.ManagedPath, variant.SizeBytes, variant.ContentHash)
					if err != nil {
						t.Fatal(err)
					}
					acceptanceClose(t, file)
				}
			}
			// otherAfter содержит состояние отдельного Asset после замены для сравнения с
			// исходным снимком.
			otherAfter, err := svc.GetAsset(ctx, other.ID)
			if err != nil || !reflect.DeepEqual(otherBefore, otherAfter) {
				t.Fatalf("замена изменила другой asset: before=%+v after=%+v error=%v", otherBefore, otherAfter, err)
			}
		})
	}
}

// Сверяет намерения очистки с полным реестром файлов отказавшего поколения, удаляет файлы перед
// каталогом и проверяет отсутствие каталога.
func normalizationCleanup(t *testing.T, ctx context.Context, manager *Manager, svc *core.Service, g core.FallbackGeneration, directory string) {
	t.Helper()
	// ledger содержит полный реестр файлов отказавшего поколения. err сообщает об отказе его чтения.
	ledger, err := svc.ListGenerationFiles(ctx, core.PageRequest{Query: core.ListQuery{Limit: 100, GenerationID: g.ID}})
	if err != nil {
		t.Fatal(err)
	}
	// intents содержит неподтверждённые намерения очистки того же поколения.
	intents, err := svc.ListCleanups(ctx, core.PageRequest{Query: core.ListQuery{Limit: 100, GenerationID: g.ID, PendingOnly: true}})
	if err != nil || len(intents.Items) != len(ledger.Items) {
		t.Fatalf("cleanup не покрывает весь ledger: intents=%d files=%d error=%v", len(intents.Items), len(ledger.Items), err)
	}
	// registered связывает управляемые пути с записями реестра для проверки полного покрытия
	// очисткой.
	registered := map[string]core.OwnedFile{}
	// file содержит очередную запись реестра, добавляемую в проверочную таблицу.
	for _, file := range ledger.Items {
		registered[file.ManagedPath] = file
	}
	// profile содержит очередной требуемый профиль для проверки регистрации normalized после отказа.
	for _, profile := range g.RequiredProfiles {
		if registered[profile.StagingPath+".normalized"].Kind != core.OwnedStaging {
			t.Fatal("не сохранена запись нормализованного staging после отказа")
		}
	}
	// directoryIntent сохраняет намерение каталога, выполняемое только после файлов.
	var directoryIntent core.CleanupIntent
	// intent содержит очередное намерение очистки для сверки с реестром.
	for _, intent := range intents.Items {
		// file содержит запись реестра по пути намерения. ok подтверждает её наличие.
		file, ok := registered[intent.ManagedPath]
		if !ok || file.ID != intent.FileID || file.Kind != intent.FileKind || intent.Kind != core.CleanupFile || intent.ArtifactGenerationID != g.ID {
			t.Fatalf("cleanup не соответствует точному ledger: %+v", intent)
		}
		delete(registered, intent.ManagedPath)
		if intent.FileKind == core.OwnedDirectory {
			directoryIntent = intent
		} else /* err сообщает об отказе очистки очередного файла до удаления каталога. */ if err := manager.CleanupFile(ctx, intent); err != nil {
			t.Fatalf("очистка %s: %v", intent.ManagedPath, err)
		}
	}
	if len(registered) != 0 || directoryIntent.ID == "" {
		t.Fatal("cleanup пропустил принадлежащие поколению пути")
	}
	// err сообщает об отказе очистки каталога после всех принадлежащих файлов.
	if err := manager.CleanupFile(ctx, directoryIntent); err != nil {
		t.Fatal(err)
	}
	// err содержит результат проверки отсутствия каталога поколения после точной очистки.
	if _, err := os.Stat(filepath.Join(directory, core.GenerationDirectory(g.ID))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("после точного cleanup остался каталог поколения: %v", err)
	}
}
