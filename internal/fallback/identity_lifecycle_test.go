package fallback

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/processowner"
)

// Создаёт тестовую запись файла либо каталога с точными путём поколения и FileID.
func identityOwnedFile(generation, name string, kind core.OwnedFileKind) core.OwnedFile {
	// managedPath содержит канонический путь поколения с необязательным именем дочернего файла.
	managedPath := core.GenerationDirectory(core.ID(generation))
	if name != "" {
		managedPath += "/" + name
	}
	return core.OwnedFile{ID: core.ID(generation + "-" + name), AssetID: "asset", GenerationID: core.ID(generation), Kind: kind, ManagedPath: managedPath}
}

// Создаёт управляемый файл, записывает данные, вычисляет размер и SHA256, синхронизирует и закрывает
// дескриптор.
func identityWriteOwned(t *testing.T, files *FileStore, owned core.OwnedFile) (uint64, string) {
	t.Helper()
	// f содержит созданный файл с журнальным доказательством владения. err сообщает об отказе
	// создания.
	f, err := files.CreateOwned(owned)
	if err != nil {
		t.Fatal(err)
	}
	// writeErr содержит результат записи тестового содержимого.
	_, writeErr := f.WriteString("prepared media bytes")
	// size содержит измеренный размер файла. digest содержит SHA256 его содержимого. inspectErr
	// сообщает об отказе проверки.
	size, digest, inspectErr := InspectFile(context.Background(), f, 1024)
	if err = errors.Join(writeErr, inspectErr, f.Sync(), f.Close()); err != nil {
		t.Fatal(err)
	}
	return size, digest
}

// Читает число запомненных файловых идентичностей под блокировкой FileStore.
func identityCount(files *FileStore) int {
	files.mu.Lock()
	defer files.mu.Unlock()
	return len(files.identities)
}

// Вычисляет путь служебной ссылки файлового журнала по SHA256 идентификатора файла.
func identityAnchor(directory string, file core.OwnedFile) string {
	return filepath.Join(directory, ".processowner-evidence", "anchors", fmt.Sprintf("%x", sha256.Sum256([]byte(file.ID))))
}

// Проверяет в повторных циклах освобождение наблюдений после переименования и удаления и сохранение
// служебных ссылок до подтверждения FILE, не затрагивая чужую запись.
func TestManagedGenerationCleanupReleasesRememberedIdentities(t *testing.T) {
	// ctx задаёт контекст файлового жизненного цикла без дополнительного срока.
	ctx := context.Background()
	// files содержит тестовое файловое хранилище. directory содержит его временный корень для
	// проверки служебных ссылок.
	files, directory := selfFileStore(t)
	// keep содержит файл, не относящийся к очищаемым поколениям. err сообщает об отказе его
	// создания.
	keep, err := files.Create("preserved")
	if err != nil {
		t.Fatal(err)
	}
	if err = keep.Close(); err != nil {
		t.Fatal(err)
	}
	// baseline сохраняет исходное число локальных наблюдений, которое должно восстановиться после
	// каждого цикла.
	baseline := identityCount(files)
	// preserved сохраняет наблюдение постороннего файла для проверки его неизменности.
	preserved := files.remembered("preserved")
	// cycle задаёт номер одного из 24 повторных жизненных циклов поколения.
	for cycle := range 24 {
		t.Run(fmt.Sprintf("generation-%d", cycle), func(t *testing.T) {
			// Проводит полный цикл поколения с двумя файлами: переименование, точное чтение, удаление и
			// освобождение ссылок после подтверждения, сохраняя чужую RAM-запись.

			// generation содержит отдельный идентификатор поколения текущего цикла.
			generation := fmt.Sprintf("generation-%d", cycle)
			// dir содержит точную запись управляемого каталога поколения.
			dir := identityOwnedFile(generation, "", core.OwnedDirectory)
			// err сообщает об отказе создания каталога с доказательством владения.
			if err := files.CreateOwnedGeneration(dir); err != nil {
				t.Fatal(err)
			}
			// retired накапливает записи промежуточных и конечных файлов, затем каталога для
			// последовательной очистки.
			var retired []core.OwnedFile
			// name выбирает исходник либо вариант текущего поколения.
			for _, name := range []string{"original", "variant"} {
				// staging содержит запись промежуточного файла перед публикацией
				// переименования.
				staging := identityOwnedFile(generation, name+".stage", core.OwnedStaging)
				// kind выбирает доменный вид конечного исходника или варианта.
				kind := core.OwnedOriginal
				if name == "variant" {
					kind = core.OwnedVariant
				}
				// final содержит точную запись конечного пути с соответствующим видом
				// файла.
				final := identityOwnedFile(generation, name, kind)
				// err сообщает об отказе предварительного резервирования конечной
				// идентичности.
				if err := files.ReserveOwnedContext(ctx, final); err != nil {
					t.Fatal(err)
				}
				final.SizeBytes, final.ContentHash = identityWriteOwned(t, files, staging)
				// err сообщает об отказе журналируемого переименования промежуточного
				// файла.
				if err := files.PublishOwnedRename(staging, final); err != nil {
					t.Fatal(err)
				}
				if files.remembered(staging.ManagedPath) != nil {
					t.Fatal("успешный rename сохранил RAM-запись staging")
				}
				// verified содержит независимо открытый конечный файл с проверенным
				// размером и SHA256. err сообщает об отказе проверки.
				verified, err := files.OpenExact(ctx, final.ManagedPath, final.SizeBytes, final.ContentHash)
				if err != nil {
					t.Fatal(err)
				}
				if err = verified.Close(); err != nil {
					t.Fatal(err)
				}
				retired = append(retired, staging, final)
			}
			retired = append(retired, dir)
			// file содержит очередную запись для удаления и подтверждения очистки.
			for _, file := range retired {
				// err сообщает об отказе точного удаления текущего объекта.
				if err := files.RemoveOwnedContext(ctx, file); err != nil {
					t.Fatal(file.ManagedPath, err)
				}
				if files.remembered(file.ManagedPath) != nil {
					t.Fatal("успешное удаление сохранило RAM-запись", file.ManagedPath)
				}
				// err содержит результат проверки сохранности служебной ссылки до
				// подтверждения FILE.
				if _, err := os.Lstat(identityAnchor(directory, file)); err != nil {
					t.Fatal("служебная ссылка потеряна до FILE ack", err)
				}
				// Вызывающая сторона подтверждает FILE в SQL до освобождения ссылки.
				// err сообщает об отказе освобождения доказательства после подтверждения
				// FILE.
				if err := files.AcknowledgeOwnedContext(ctx, file); err != nil {
					t.Fatal(err)
				}
				// err содержит результат проверки отсутствия служебной ссылки после
				// подтверждения.
				if _, err := os.Lstat(identityAnchor(directory, file)); !errors.Is(err, os.ErrNotExist) {
					t.Fatal("служебная ссылка осталась после FILE ack", err)
				}
			}
			// got содержит число локальных наблюдений после полного цикла очистки.
			if got := identityCount(files); got != baseline || files.remembered("preserved") != preserved {
				t.Fatalf("жизненный цикл сохранил чужие или завершённые записи: %d, исходно %d", got, baseline)
			}
		})
	}
}

// Проверяет удаление и подтверждение управляемых объектов после повторного открытия FileStore с
// освобождением новых наблюдений в памяти.
func TestManagedIdentityCleanupAfterReopen(t *testing.T) {
	// ctx задаёт контекст проверки после повторного открытия хранилища.
	ctx := context.Background()
	// directory содержит временный корень повторно открываемого хранилища.
	directory := t.TempDir()
	// files содержит текущий экземпляр хранилища. err сообщает об отказе открытия.
	files, err := OpenFileStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	// closed показывает, нужно ли резервное закрытие текущего экземпляра после теста.
	closed := false
	t.Cleanup(func() {
		// Закрывает текущий FileStore, если явное закрытие ещё не выполнено, и сообщает ошибку через t.Error.

		if !closed {
			// err сообщает об отказе резервного закрытия текущего хранилища.
			if err := files.Close(); err != nil {
				t.Error(err)
			}
		}
	})
	// dir содержит запись каталога поколения, создаваемого до повторного открытия.
	dir := identityOwnedFile("reopened", "", core.OwnedDirectory)
	// owned содержит запись исходника с доказательством, размером и SHA256.
	owned := identityOwnedFile("reopened", "original", core.OwnedOriginal)
	if err = files.CreateOwnedGeneration(dir); err != nil {
		t.Fatal(err)
	}
	owned.SizeBytes, owned.ContentHash = identityWriteOwned(t, files, owned)
	if err = files.Close(); err != nil {
		t.Fatal(err)
	}
	closed = true
	files, err = OpenFileStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	closed = false
	// verified содержит проверенный исходник, открытый новым экземпляром хранилища.
	verified, err := files.OpenExact(ctx, owned.ManagedPath, owned.SizeBytes, owned.ContentHash)
	if err != nil {
		t.Fatal(err)
	}
	if err = verified.Close(); err != nil {
		t.Fatal(err)
	}
	if identityCount(files) != 1 {
		t.Fatal("проверенное чтение не учтено")
	}
	// file содержит исходник либо каталог для точной очистки после открытия.
	for _, file := range []core.OwnedFile{owned, dir} {
		if err = files.RemoveOwned(file); err != nil {
			t.Fatal(err)
		}
		if err = files.AcknowledgeOwned(file); err != nil {
			t.Fatal(err)
		}
	}
	if identityCount(files) != 0 {
		t.Fatal("очистка после повторного открытия сохранила RAM-запись")
	}
}

// Проверяет, что отмена, подмена, конфликт переименования и преждевременное подтверждение сохраняют
// наблюдение и ссылку для явной последующей очистки.
func TestManagedIdentityFailuresPreserveExplicitCleanup(t *testing.T) {
	// failure выбирает отмену, подмену, конфликт переименования либо преждевременное подтверждение.
	for _, failure := range []string{"remove-canceled", "remove-replacement", "rename-conflict", "ack-pending"} {
		t.Run(failure, func(t *testing.T) {
			// Проверяет сохранность RAM-доказательства и служебной ссылки после выбранного отказа удаления,
			// переименования или подтверждения и возможность последующей явной очистки.

			// files содержит хранилище текущего сценария. directory содержит его корень для
			// контролируемой подмены.
			files, directory := selfFileStore(t)
			// dir содержит запись каталога проверяемого поколения.
			dir := identityOwnedFile("pending", "", core.OwnedDirectory)
			// staging содержит запись исходного промежуточного файла с доказательством
			// владения.
			staging := identityOwnedFile("pending", "source.stage", core.OwnedStaging)
			// final содержит конечную идентичность для проверки конфликта переименования.
			final := identityOwnedFile("pending", "original", core.OwnedOriginal)
			// err сообщает об отказе создания каталога проверяемого поколения.
			if err := files.CreateOwnedGeneration(dir); err != nil {
				t.Fatal(err)
			}
			identityWriteOwned(t, files, staging)
			// known сохраняет исходное локальное наблюдение, которое должен сохранить
			// неуспешный путь.
			known := files.remembered(staging.ManagedPath)
			// err принимает результат выбранного отказа и последующей явной очистки.
			var err error
			switch failure {
			case "remove-canceled":
				// ctx содержит заранее отменяемый контекст удаления. cancel отменяет его
				// до файлового действия.
				ctx, cancel := context.WithCancel(context.Background())
				cancel()
				err = files.RemoveOwnedContext(ctx, staging)
				if !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			case "remove-replacement":
				// name содержит полный путь промежуточного файла для временной подмены
				// объекта.
				name := filepath.Join(directory, staging.ManagedPath)
				if err = os.Rename(name, name+".old"); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(name, []byte("replacement"), 0600); err != nil {
					t.Fatal(err)
				}
				if err = files.RemoveOwned(staging); !errors.Is(err, processowner.ErrUnknown) {
					t.Fatal(err)
				}
				if err = os.Remove(name); err != nil {
					t.Fatal(err)
				}
				if err = os.Rename(name+".old", name); err != nil {
					t.Fatal(err)
				}
			case "rename-conflict":
				if err = os.WriteFile(filepath.Join(directory, final.ManagedPath), []byte("replacement"), 0600); err != nil {
					t.Fatal(err)
				}
				err = files.PublishOwnedRename(staging, final)
				if !errors.Is(err, processowner.ErrUnknown) {
					t.Fatal(err)
				}
			case "ack-pending":
				err = files.AcknowledgeOwned(staging)
				if !errors.Is(err, processowner.ErrPending) {
					t.Fatal(err)
				}
			}
			if files.remembered(staging.ManagedPath) != known {
				t.Fatal("неуспешное действие потеряло RAM-доказательство")
			}
			if _, err = os.Lstat(identityAnchor(directory, staging)); err != nil {
				t.Fatal("неуспешное действие потеряло служебную ссылку", err)
			}
			// Явный прежний порт без FileID по-прежнему использует точную RAM-запись.
			staging.ID = ""
			if err = files.RemoveOwned(staging); err != nil {
				t.Fatal("утрачена возможность явной очистки", err)
			}
		})
	}
}

// Проверяет освобождение старого наблюдения после удаления через общий журнал и сохранение нового
// объекта по тому же пути при повторном подтверждении.
func TestManagedAcknowledgementReleasesRememberedIdentity(t *testing.T) {
	// ctx задаёт контекст проверки освобождения доказательств.
	ctx := context.Background()
	// files содержит файловое хранилище. directory содержит его корень для проверки служебных
	// ссылок.
	files, directory := selfFileStore(t)
	// dir содержит запись каталога поколения для последующей очистки.
	dir := identityOwnedFile("acknowledged", "", core.OwnedDirectory)
	// owned содержит запись промежуточного файла, удаляемого напрямую общим журналом.
	owned := identityOwnedFile("acknowledged", "source.stage", core.OwnedStaging)
	// err сообщает об отказе создания каталога поколения.
	if err := files.CreateOwnedGeneration(dir); err != nil {
		t.Fatal(err)
	}
	identityWriteOwned(t, files, owned)
	// Общий журнал завершил физическую очистку; FileStore ещё хранит прежнее наблюдение.
	// err сообщает об отказе прямого удаления через общий файловый журнал.
	if err := files.FileJournal().Remove(ctx, fileIdentity(owned)); err != nil {
		t.Fatal(err)
	}
	if files.remembered(owned.ManagedPath) == nil {
		t.Fatal("исходное RAM-наблюдение отсутствует")
	}
	// err содержит результат проверки сохранности служебной ссылки до подтверждения.
	if _, err := os.Lstat(identityAnchor(directory, owned)); err != nil {
		t.Fatal("служебная ссылка потеряна до FILE ack", err)
	}
	// err сообщает об отказе освобождения доказательства после физического удаления.
	if err := files.AcknowledgeOwned(owned); err != nil {
		t.Fatal(err)
	}
	if files.remembered(owned.ManagedPath) != nil {
		t.Fatal("FILE ack сохранил завершённое RAM-наблюдение")
	}
	// replacement содержит новый объект по прежнему пути. err сообщает об отказе его создания.
	replacement, err := files.Create(owned.ManagedPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = replacement.Close(); err != nil {
		t.Fatal(err)
	}
	// current сохраняет наблюдение нового объекта, которое должно пережить повторное подтверждение
	// старого.
	current := files.remembered(owned.ManagedPath)
	if err = files.AcknowledgeOwned(owned); err != nil {
		t.Fatal(err)
	}
	if files.remembered(owned.ManagedPath) != current {
		t.Fatal("повтор FILE ack удалил наблюдение нового объекта")
	}
	owned.ID = ""
	if err = files.RemoveOwned(owned); err != nil {
		t.Fatal("новый объект потерял доказательство явного владения", err)
	}
	// err сообщает об отказе удаления каталога после очистки его файлов.
	if err := files.RemoveOwned(dir); err != nil {
		t.Fatal(err)
	}
	// err сообщает об отказе освобождения доказательства каталога.
	if err := files.AcknowledgeOwned(dir); err != nil {
		t.Fatal(err)
	}
}

// Проверяет, что позднее забывание прежнего объекта не удаляет наблюдение его замены и возможность её
// явной очистки.
func TestRememberedIdentityCleanupKeepsReplacementObservation(t *testing.T) {
	// files содержит файловое хранилище. directory содержит его корень для временного перемещения
	// прежнего объекта.
	files, directory := selfFileStore(t)
	// err сообщает об отказе создания каталога поколения.
	if err := files.CreateGeneration("replacement"); err != nil {
		t.Fatal(err)
	}
	// owned содержит запись пути без FileID для проверки точного локального наблюдения.
	owned := identityOwnedFile("replacement", "source", core.OwnedStaging)
	owned.ID = ""
	// original содержит первоначальный объект этого пути. err сообщает об отказе его создания.
	original, err := files.Create(owned.ManagedPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = original.Close(); err != nil {
		t.Fatal(err)
	}
	// previous сохраняет наблюдение первоначального объекта до его перемещения.
	previous := files.remembered(owned.ManagedPath)
	if err = os.Rename(filepath.Join(directory, owned.ManagedPath), filepath.Join(directory, owned.ManagedPath+".old")); err != nil {
		t.Fatal(err)
	}
	// replacement содержит новый объект, созданный по прежнему пути.
	replacement, err := files.Create(owned.ManagedPath)
	if err != nil {
		t.Fatal(err)
	}
	if err = replacement.Close(); err != nil {
		t.Fatal(err)
	}
	// current сохраняет наблюдение замены, которое позднее забывание previous не должно удалить.
	current := files.remembered(owned.ManagedPath)
	// Завершение прежнего действия не удаляет запись более нового объекта по тому же пути.
	files.forget(owned.ManagedPath, previous)
	if current == previous || files.remembered(owned.ManagedPath) != current {
		t.Fatal("завершение прежнего действия удалило новое наблюдение")
	}
	if err = files.RemoveOwned(owned); err != nil {
		t.Fatal("новый объект потерял доказательство явного владения", err)
	}
}
