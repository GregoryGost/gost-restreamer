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

func fallbackProcessRegistry(t *testing.T) *processowner.Registry {
	t.Helper()
	helper := filepath.Join(t.TempDir(), "gost-process-helper")
	args := []string{"build", "-o", helper}
	if directory := os.Getenv("GOST_TEST_PROCESS_HELPER_COVERDIR"); directory != "" {
		t.Setenv("GOCOVERDIR", directory)
		args = append(args, "-cover", "-covermode=atomic", "-coverpkg=github.com/GregoryGost/gost-restreamer/...")
	}
	build := exec.Command("go", append(args, "../../cmd/gost-process-helper")...)
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("сборка helper: %v: %s", err, output)
	}
	registry, err := processowner.New(processowner.Config{Root: t.TempDir(), HelperPath: helper, GracePeriod: 20 * time.Millisecond,
		Limits: processowner.Limits{MaxOperations: 100, MaxRecordBytes: 262144, MaxCommandArgs: 256, MaxCommandBytes: 65536, MaxExtraFiles: 1, PageSize: 10}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := registry.Close(); err != nil {
			t.Error(err)
		}
	})
	return registry
}

func TestManagedRunnerNativeFDAndCancellationJoin(t *testing.T) {
	registry := fallbackProcessRegistry(t)
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	for _, mode := range []string{"copy", "overflow", "diagnostic", "wait"} {
		t.Run(mode, func(t *testing.T) {
			ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
			defer cancel()
			scope, err := registry.Create(ctx, "exact-preparation-"+mode)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = scope.Close() })
			input, err := os.CreateTemp(t.TempDir(), "input")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = input.Close() }()
			if _, err = input.WriteString("owned seekable descriptor"); err != nil {
				t.Fatal(err)
			}
			if _, err = input.Seek(0, io.SeekStart); err != nil {
				t.Fatal(err)
			}
			runner := Runner{Limits: selfRunner().Limits, Scope: scope}
			runner.Limits.MaxDiagnosticBytes = 32
			if mode == "wait" {
				runner.Limits.Timeout = 100 * time.Millisecond
			}
			var output bytes.Buffer
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
			proof, err := scope.Resolve(ctx)
			if err != nil || !proof.Joined {
				t.Fatalf("возврат Runner до физического присоединения: %+v %v", proof, err)
			}
		})
	}
}

func TestManagedFileAnchorsSurviveReopenAndRejectReplacement(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	files, err := OpenFileStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	ownedDir := core.OwnedFile{Record: core.Record{ID: "directory-id"}, AssetID: "asset", GenerationID: "generation", Kind: core.OwnedDirectory, ManagedPath: "fallback/generation"}
	owned := core.OwnedFile{Record: core.Record{ID: "staging-id"}, AssetID: "asset", GenerationID: "generation", Kind: core.OwnedStaging, ManagedPath: "fallback/generation/partial"}
	if err = files.CreateOwnedGeneration(ownedDir); err != nil {
		t.Fatal(err)
	}
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
	completed := core.OwnedFile{Record: core.Record{ID: "completed-id"}, AssetID: "asset", GenerationID: "generation", Kind: core.OwnedOriginal, ManagedPath: "fallback/generation/original"}
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
		if err := files.Close(); err != nil {
			t.Error(err)
		}
	})
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
	if got, err := os.ReadFile(filepath.Join(directory, owned.ManagedPath)); err != nil || string(got) != "replacement" {
		t.Fatal(string(got), err)
	}
}

func TestManagedManagerOwnsPreparationScopeAndAdmission(t *testing.T) {
	registry := fallbackProcessRegistry(t)
	svc, _, asset, _ := acceptanceManagerService(t)
	files, _ := acceptanceFileStore(t)
	config := acceptanceManagerConfig()
	config.ProcessRegistry = registry
	manager, err := NewManager(svc, files, config)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	job, err := manager.Upload(ctx, core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage}, io.NopCloser(bytes.NewBufferString("invalid image")))
	if err != nil {
		t.Fatal(err)
	}
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

func TestReconcileProvesTerminalPreparationAndCollectsFileAnchors(t *testing.T) {
	registry := fallbackProcessRegistry(t)
	svc, _, asset, _ := acceptanceManagerService(t)
	files, directory := acceptanceFileStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	generation, err := svc.ReservePreparation(ctx, core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage, GeneratorVersion: "generator"})
	if err != nil {
		t.Fatal(err)
	}
	fence, err := svc.ClaimPreparation(ctx, generation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.MarkPreparationIssued(ctx, fence); err != nil {
		t.Fatal(err)
	}
	key, err := json.Marshal(preparationScopeOwner{1, "preparation", fence})
	if err != nil {
		t.Fatal(err)
	}
	scope, err := registry.Create(ctx, string(key))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scope.Close() })
	ticket, err := scope.Register(ctx, "preparation")
	if err != nil {
		t.Fatal(err)
	}
	config := acceptanceManagerConfig()
	config.ProcessRegistry = registry
	config.ReconcileLimit = 2
	manager, err := NewManager(svc, files, config)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = manager.Close(context.Background()) })
	if err = manager.reserveGenerationFiles(ctx, generation.ID); err != nil {
		t.Fatal(err)
	}
	ownedDir, err := manager.ownedFile(ctx, generation.ID, core.GenerationDirectory(generation.ID))
	if err != nil {
		t.Fatal(err)
	}
	if err = files.CreateOwnedGenerationContext(ctx, ownedDir); err != nil {
		t.Fatal(err)
	}
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
	current, err := svc.GetGeneration(ctx, generation.ID)
	if err != nil || !current.Issued || !current.CleanupPending {
		t.Fatalf("неприсоединённый scope подтверждён: %+v %v", current, err)
	}
	manager.retiredScopes[scope.ID()] = retiredPreparationScope{scope: scope, ticket: ticket, generationID: generation.ID}
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
	intents, err := svc.ListCleanups(ctx, core.PageRequest{Query: core.ListQuery{Limit: 100, GenerationID: generation.ID}})
	if err != nil {
		t.Fatal(err)
	}
	for _, intent := range intents.Items {
		if intent.ManagedPath != generation.StagingPath {
			continue
		}
		cleanupFence := core.CleanupFenceFor(intent)
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
		// Имитируется остановка между SQL ack и освобождением служебной ссылки.
	}
	manager.CloseAdmission()
	for range 20 {
		_ = manager.Reconcile(ctx)
	}
	current, err = svc.GetGeneration(ctx, generation.ID)
	if err != nil || current.Issued || current.CleanupPending {
		t.Fatalf("доказанный scope не подтверждён: %+v %v", current, err)
	}
	cleanups, err := svc.ListCleanups(ctx, core.PageRequest{Query: core.ListQuery{Limit: 100, GenerationID: generation.ID, PendingOnly: true}})
	if err != nil || len(cleanups.Items) != 0 {
		t.Fatalf("не завершена постраничная очистка: %+v %v", cleanups, err)
	}
	anchors, err := os.ReadDir(filepath.Join(directory, ".processowner-evidence", "anchors"))
	if err != nil || len(anchors) != 0 {
		t.Fatalf("FILE ack сохранил ссылки: %v %v", anchors, err)
	}
}
