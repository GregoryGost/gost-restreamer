package fallback

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/processowner"
)

// Собирает настоящую вспомогательную программу во временный каталог и открывает изолированный реестр с
// закрытием после теста; при заданном каталоге включает покрытие этой программы.
func fallbackProcessRegistry(t *testing.T) *processowner.Registry {
	t.Helper()
	// helper содержит путь временного исполняемого файла вспомогательной программы.
	helper := filepath.Join(t.TempDir(), "gost-process-helper")
	// args накапливает аргументы сборки помощника, включая покрытие при явном запросе.
	args := []string{"build", "-o", helper}
	// directory содержит явно заданный каталог данных покрытия дочерних процессов.
	if directory := os.Getenv("GOST_TEST_PROCESS_HELPER_COVERDIR"); directory != "" {
		t.Setenv("GOCOVERDIR", directory)
		args = append(args, "-cover", "-covermode=atomic", "-coverpkg=github.com/GregoryGost/gost-restreamer/...")
	}
	// build содержит команду сборки настоящего помощника во временный каталог.
	build := exec.Command("go", append(args, "../../cmd/gost-process-helper")...)
	// output содержит объединённый вывод сборки. err сообщает об отказе сборки.
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("сборка helper: %v: %s", err, output)
	}
	// registry содержит изолированный реестр с настоящим помощником. err сообщает об отказе его
	// создания.
	registry, err := processowner.New(processowner.Config{Root: t.TempDir(), HelperPath: helper, GracePeriod: 20 * time.Millisecond,
		Limits: processowner.Limits{MaxOperations: 100, MaxRecordBytes: 262144, MaxCommandArgs: 256, MaxCommandBytes: 65536, MaxExtraFiles: 1, PageSize: 10}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает тестовый реестр процессов и сообщает ошибку через t.Error.

		// err сообщает об отказе закрытия тестового реестра.
		if err := registry.Close(); err != nil {
			t.Error(err)
		}
	})
	return registry
}

// Проверяет FD 3, бюджеты вывода и диагностики и остановку по сроку через настоящую вспомогательную
// программу, требуя Joined после возврата Runner.
func TestManagedRunnerNativeFDAndCancellationJoin(t *testing.T) {
	// registry содержит реестр для запуска проверяемых дочерних процессов.
	registry := fallbackProcessRegistry(t)
	// binary содержит путь текущего тестового исполняемого файла. err сообщает об отказе его
	// определения.
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	// mode выбирает копирование, переполнение вывода, переполнение диагностики либо ожидание.
	for _, mode := range []string{"copy", "overflow", "diagnostic", "wait"} {
		t.Run(mode, func(t *testing.T) {
			// Проверяет выбранный режим Runner через настоящий вспомогательный процесс и требует доказанного
			// физического завершения области после результата или ошибки.

			// ctx ограничивает текущий сценарий десятью секундами. cancel освобождает этот
			// срок.
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			// scope содержит область процессов текущего режима. err сообщает об отказе её
			// создания.
			scope, err := registry.Create(ctx, "exact-preparation-"+mode)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				// Закрывает область процессов при завершении сценария, игнорируя ошибку резервного закрытия.
				_ = scope.Close()
			})
			// input содержит временный файл для передачи через FD 3.
			input, err := os.CreateTemp(t.TempDir(), "input")
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				// Закрывает входной файловый дескриптор сценария, игнорируя ошибку.
				_ = input.Close()
			}()
			if _, err = input.WriteString("owned seekable descriptor"); err != nil {
				t.Fatal(err)
			}
			if _, err = input.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			// runner запускает команду через текущую область с малыми бюджетами диагностики.
			runner := Runner{Limits: selfRunner().Limits, Scope: scope}
			runner.Limits.MaxDiagnosticBytes = 32
			if mode == "wait" {
				runner.Limits.Timeout = 100 * time.Millisecond
			}
			// output принимает ограниченный стандартный вывод дочернего процесса.
			var output bytes.Buffer
			// runErr содержит итог запуска, вывода и подтверждения завершения.
			_, runErr := runner.Run(ctx, Command{Binary: binary, Args: []string{"-test.run=^TestRunnerChild$", "fallback-child", mode}}, input, &boundedWriter{w: &output, remaining: 32})
			switch mode {
			case "copy":
				if runErr != nil || output.String() != "owned seekable descriptor" {
					t.Fatal(output.String(), runErr)
				}
			case "wait":
				if !errors.Is(runErr, context.DeadlineExceeded) {
					t.Fatal(runErr)
				}
			default:
				if !errors.Is(runErr, ErrLimit) {
					t.Fatal(runErr)
				}
			}
			if err = scope.Seal(ctx); err != nil {
				t.Fatal(err)
			}
			if err = scope.Close(); err != nil {
				t.Fatal(err)
			}
			// proof содержит сохранённое доказательство завершения области после Run.
			proof, err := scope.Resolve(ctx)
			if err != nil || !proof.Joined {
				t.Fatalf("возврат Runner до физического присоединения: %+v %v", proof, err)
			}
		})
	}
}

// Проверяет сохранение доказательств после открытия нового FileStore и отказ удаления при утрате
// ссылки, подмене файла либо неизвестном содержимом каталога.
func TestManagedFileAnchorsSurviveReopenAndRejectReplacement(t *testing.T) {
	// ctx задаёт контекст проверки долговечных файловых доказательств.
	ctx := context.Background()
	// directory содержит временный корень для повторного открытия хранилища.
	directory := t.TempDir()
	// files содержит текущий экземпляр файлового хранилища. err сообщает об отказе открытия.
	files, err := OpenFileStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	// ownedDir содержит точную запись каталога поколения с FileID.
	ownedDir := core.OwnedFile{ID: "directory-id", AssetID: "asset", GenerationID: "generation", Kind: core.OwnedDirectory, ManagedPath: "fallback/generation"}
	// owned содержит точную запись частичного промежуточного файла.
	owned := core.OwnedFile{ID: "staging-id", AssetID: "asset", GenerationID: "generation", Kind: core.OwnedStaging, ManagedPath: "fallback/generation/partial"}
	if err = files.CreateOwnedGeneration(ownedDir); err != nil {
		t.Fatal(err)
	}
	// f содержит созданный промежуточный файл с доказательством владения.
	f, err := files.CreateOwned(owned)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString("partial media"); err != nil {
		t.Fatal(err)
	}
	if err = f.Close(); err != nil {
		t.Fatal(err)
	}
	// completed содержит запись завершённого исходника с измеряемыми размером и SHA256.
	completed := core.OwnedFile{ID: "completed-id", AssetID: "asset", GenerationID: "generation", Kind: core.OwnedOriginal, ManagedPath: "fallback/generation/original"}
	// original содержит дескриптор завершённого исходника для записи и измерения.
	original, err := files.CreateOwned(completed)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = original.WriteString("completed media"); err != nil {
		t.Fatal(err)
	}
	completed.SizeBytes, completed.ContentHash, err = InspectFile(ctx, original, 1024)
	if err != nil {
		t.Fatal(err)
	}
	if err = original.Close(); err != nil {
		t.Fatal(err)
	}
	if err = files.Close(); err != nil {
		t.Fatal(err)
	}
	files, err = OpenFileStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает повторно открытый FileStore после проверки служебных ссылок и сообщает ошибку через
		// t.Error.

		// err сообщает об отказе закрытия повторно открытого хранилища.
		if err := files.Close(); err != nil {
			t.Error(err)
		}
	})
	// anchor содержит путь служебной ссылки завершённого исходника, намеренно удаляемой для проверки
	// недоказанности владения.
	anchor := filepath.Join(directory, ".processowner-evidence", "anchors", acceptanceDigest([]byte(completed.ID)))
	if err = os.Remove(anchor); err != nil {
		t.Fatal(err)
	}
	if err = files.RemoveOwnedContext(ctx, completed); err == nil {
		t.Fatal("утрата служебной ссылки скрыта совпадением хеша")
	}
	if _, err = os.Lstat(filepath.Join(directory, completed.ManagedPath)); err != nil {
		t.Fatal("завершённый файл потерян", err)
	}
	if err = files.RemoveOwnedContext(ctx, owned); err != nil {
		t.Fatal("потеряно доказательство после повторного открытия", err)
	}
	if err = files.RemoveOwnedContext(ctx, owned); err != nil {
		t.Fatal("повтор удаления", err)
	}
	if err = os.WriteFile(filepath.Join(directory, owned.ManagedPath), []byte("replacement"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = files.RemoveOwnedContext(ctx, owned); err == nil {
		t.Fatal("подмена удалена")
	}
	if err = files.RemoveOwnedContext(ctx, ownedDir); err == nil {
		t.Fatal("неизвестный вложенный объект удалён")
	}
	// got содержит байты заменяющего объекта после отклонённой очистки. err сообщает об отказе
	// чтения.
	if got, err := os.ReadFile(filepath.Join(directory, owned.ManagedPath)); err != nil || string(got) != "replacement" {
		t.Fatal(string(got), err)
	}
}

// Проверяет завершённую очистку области после ошибки исходного изображения и запрет новых работ и
// сверки после закрытия менеджера.
func TestManagedManagerOwnsPreparationScopeAndAdmission(t *testing.T) {
	// registry содержит настоящий изолированный реестр процессов подготовки.
	registry := fallbackProcessRegistry(t)
	// svc содержит тестовый доменный сервис. asset содержит Asset, для которого загружается
	// некорректное изображение.
	svc, _, asset, _ := acceptanceManagerService(t)
	// files содержит изолированное файловое хранилище менеджера.
	files, _ := acceptanceFileStore(t)
	// config содержит конфигурацию с настоящим реестром процессов.
	config := acceptanceManagerConfig()
	config.ProcessRegistry = registry
	// manager содержит рабочий менеджер с управляемыми процессами. err сообщает об отказе его
	// создания.
	manager, err := NewManager(svc, files, config)
	if err != nil {
		t.Fatal(err)
	}
	// ctx ограничивает загрузку и ожидание пятью секундами. cancel освобождает срок.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// job содержит принятое задание с некорректным изображением.
	job, err := manager.Upload(ctx, core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage}, io.NopCloser(bytes.NewBufferString("invalid image")))
	if err != nil {
		t.Fatal(err)
	}
	// result содержит итог ошибочной подготовки для проверки подтверждённой очистки области.
	result, err := job.Wait(ctx)
	if err == nil || !result.Clean {
		t.Fatalf("ошибка исходного файла не завершила scope: %+v %v", result, err)
	}
	manager.CloseAdmission()
	if _, err = manager.Rebuild(ctx, core.PreparationRequest{}); !errors.Is(err, core.ErrClosed) {
		t.Fatal(err)
	}
	if err = manager.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err = manager.Reconcile(ctx); !errors.Is(err, core.ErrClosed) {
		t.Fatal(err)
	}
}

// Проверяет запрет подтверждения незавершённой области, повтор отложенного завершения и постраничное
// освобождение файловых ссылок после подтверждения SQL.
func TestReconcileProvesTerminalPreparationAndCollectsFileAnchors(t *testing.T) {
	// registry содержит настоящий реестр для восстановления областей и очистки ссылок.
	registry := fallbackProcessRegistry(t)
	// svc содержит тестовый доменный сервис. asset содержит Asset проверяемой подготовки.
	svc, _, asset, _ := acceptanceManagerService(t)
	// files содержит файловое хранилище. directory содержит его корень для проверки оставшихся
	// ссылок.
	files, directory := acceptanceFileStore(t)
	// ctx ограничивает сценарий сверки десятью секундами. cancel освобождает этот срок.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// generation содержит зарезервированное поколение. err сообщает об отказе резервирования.
	generation, err := svc.ReservePreparation(ctx, core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage, GeneratorVersion: "generator"})
	if err != nil {
		t.Fatal(err)
	}
	// fence содержит выданный точный допуск подготовки.
	fence, err := svc.ClaimPreparation(ctx, generation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.MarkPreparationIssued(ctx, fence); err != nil {
		t.Fatal(err)
	}
	// key содержит JSON идентичности владельца области текущего допуска.
	key, err := json.Marshal(preparationScopeOwner{1, "preparation", fence})
	if err != nil {
		t.Fatal(err)
	}
	// scope содержит созданную область подготовки, завершение которой проверяется отдельно.
	scope, err := registry.Create(ctx, string(key))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает тестовую область подготовки после сценария, игнорируя ошибку.
		_ = scope.Close()
	})
	// ticket содержит незавершённую операцию подготовки для проверки запрета преждевременного
	// подтверждения.
	ticket, err := scope.Register(ctx, "preparation")
	if err != nil {
		t.Fatal(err)
	}
	// config содержит рабочую конфигурацию со страницами сверки по две записи.
	config := acceptanceManagerConfig()
	config.ProcessRegistry = registry
	config.ReconcileLimit = 2
	// manager содержит менеджер, выполняющий проверяемую сверку.
	manager, err := NewManager(svc, files, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает Manager с фоновым контекстом после сценария сверки, игнорируя ошибку.
		_ = manager.Close(context.Background())
	})
	if err = manager.reserveGenerationFiles(ctx, generation.ID); err != nil {
		t.Fatal(err)
	}
	// ownedDir содержит точную запись каталога поколения из реестра файлов.
	ownedDir, err := manager.ownedFile(ctx, generation.ID, core.GenerationDirectory(generation.ID))
	if err != nil {
		t.Fatal(err)
	}
	if err = files.CreateOwnedGenerationContext(ctx, ownedDir); err != nil {
		t.Fatal(err)
	}
	// file содержит управляемый промежуточный файл с частичными данными.
	file, err := manager.createOwned(ctx, generation.ID, generation.StagingPath)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = file.WriteString("partial"); err != nil {
		t.Fatal(err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	if err = svc.CancelPreparation(ctx, fence, "отозванная подготовка"); err != nil {
		t.Fatal(err)
	}
	_ = manager.Reconcile(ctx)
	// current содержит перечитываемое поколение для проверки Issued и CleanupPending.
	current, err := svc.GetGeneration(ctx, generation.ID)
	if err != nil || !current.Issued || !current.CleanupPending {
		t.Fatalf("неприсоединённый scope подтверждён: %+v %v", current, err)
	}
	manager.retiredScopes[scope.ID()] = retiredPreparationScope{scope: scope, ticket: ticket, generationID: generation.ID}
	// canceled содержит заранее отменённый контекст повторной очистки области. stop отменяет его до
	// попытки.
	canceled, stop := context.WithCancel(ctx)
	stop()
	if err = manager.retryRetiredScopes(canceled); !errors.Is(err, ErrCleanupPending) {
		t.Fatal(err)
	}
	if len(manager.retiredScopes) != 1 {
		t.Fatal("потерян дескриптор незавершённой записи")
	}
	if err = manager.retryRetiredScopes(ctx); err != nil {
		t.Fatal(err)
	}
	if err = svc.AcknowledgePreparationCleanup(ctx, fence); err != nil {
		t.Fatal(err)
	}
	// intents содержит намерения очистки файлов текущего поколения.
	intents, err := svc.ListCleanups(ctx, core.PageRequest{Query: core.ListQuery{Limit: 100, GenerationID: generation.ID}})
	if err != nil {
		t.Fatal(err)
	}
	// intent содержит очередное намерение для выбора промежуточного файла.
	for _, intent := range intents.Items {
		if intent.ManagedPath != generation.StagingPath {
			continue
		}
		// cleanupFence содержит точную идентичность выбранного намерения очистки.
		cleanupFence := core.CleanupFenceFor(intent)
		// owned содержит запись, удаление которой разрешено намерением. err сообщает об отказе
		// допуска очистки.
		owned, err := svc.AuthorizeFileCleanup(ctx, cleanupFence)
		if err != nil {
			t.Fatal(err)
		}
		if err = files.RemoveOwnedContext(ctx, owned); err != nil {
			t.Fatal(err)
		}
		if err = svc.AcknowledgeCleanup(ctx, cleanupFence); err != nil {
			t.Fatal(err)
		}
		// Имитируется остановка между подтверждением в SQL и освобождением служебной ссылки.
	}
	manager.CloseAdmission()
	for range 20 {
		_ = manager.Reconcile(ctx)
	}
	current, err = svc.GetGeneration(ctx, generation.ID)
	if err != nil || current.Issued || current.CleanupPending {
		t.Fatalf("доказанный scope не подтверждён: %+v %v", current, err)
	}
	// cleanups содержит оставшиеся неподтверждённые намерения после повторных страниц сверки.
	cleanups, err := svc.ListCleanups(ctx, core.PageRequest{Query: core.ListQuery{Limit: 100, GenerationID: generation.ID, PendingOnly: true}})
	if err != nil || len(cleanups.Items) != 0 {
		t.Fatalf("не завершена постраничная очистка: %+v %v", cleanups, err)
	}
	// anchors содержит оставшиеся служебные ссылки файлового журнала после подтверждения FILE.
	anchors, err := os.ReadDir(filepath.Join(directory, ".processowner-evidence", "anchors"))
	if err != nil || len(anchors) != 0 {
		t.Fatalf("FILE ack сохранил ссылки: %v %v", anchors, err)
	}
}
