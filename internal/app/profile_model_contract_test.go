package app

import (
	"context"
	"errors"
	"io/fs"
	"os"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/config"
	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/processowner"
)

// profileModelPreservedEntry сохраняет тип, права и байты одного объекта прежней установки.
// Полный набор ключей снимка дополнительно обнаруживает удаление и появление путей.
type profileModelPreservedEntry struct {
	// mode отличает каталог от файла и фиксирует исходные права доступа.
	mode fs.FileMode
	// data содержит полные байты обычного файла; для каталога остаётся nil.
	data []byte
}

// profileModelTreeSnapshot читает весь собственный каталог root без изменения его объектов.
// t сообщает ошибку чтения; обработчик WalkDir сохраняет относительный путь, тип, права и байты.
func profileModelTreeSnapshot(t *testing.T, root string) map[string]profileModelPreservedEntry {
	t.Helper()
	// snapshot содержит каждый путь, включая каталоги, чтобы сравнение не пропустило пустой объект.
	snapshot := map[string]profileModelPreservedEntry{}
	// err сохраняет отказ полного обхода; path и entry описывают текущий объект, walkErr — отказ обхода.
	err := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		// relative адресует объект внутри проверяемого корня; err передаёт ошибку вычисления пути.
		relative, err := filepath.Rel(root, path)
		if err != nil {
			return err
		}
		// info содержит фактические права и тип объекта до чтения его содержимого.
		info, err := entry.Info()
		if err != nil {
			return err
		}
		// saved сохраняет тип и права; данные добавляются только для обычного файла.
		saved := profileModelPreservedEntry{mode: info.Mode()}
		if !entry.IsDir() {
			if !info.Mode().IsRegular() {
				return core.ErrInvalid
			}
			saved.data, err = os.ReadFile(path)
			if err != nil {
				return err
			}
		}
		snapshot[relative] = saved
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshot
}

// profileModelPreviousInstallation создаёт только принадлежащий t старый комплект c.
// helper — настоящий тестовый бинарник; архивная SQLite и sidecar-файлы только копируются.
// Возвращённый ID обозначает закрытую, но ещё не очищенную область с реальным runtime-файлом:
// восстановление её прежнего реестра могло бы удалить этот файл и переписать журнал.
func profileModelPreviousInstallation(t *testing.T, c config.Config, helper string) string {
	t.Helper()
	// registry открывает прежние корни; err сообщает об отказе создания настоящего журнала процессов.
	registry, err := processowner.New(c.ProcessConfig(helper))
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// err сообщает о закрытии прежнего реестра до снятия исходного снимка файлов.
		if err := registry.Close(); err != nil {
			t.Error(err)
		}
	}()
	// scope создаёт достоверную область прежнего владельца без запуска внешнего процесса.
	scope, err := registry.Create(context.Background(), "previous-installation-owner")
	if scope != nil {
		defer func() {
			// err сообщает о снятии backend-владения, включая частичный отказ Create.
			// CloseContext сохраняет журнал для дальнейшей сверки.
			if err := scope.CloseContext(context.Background()); err != nil {
				t.Error(err)
			}
		}()
	}
	if err != nil {
		t.Fatal(err)
	}
	// file и data задают самостоятельные сигнальные объекты прежнего комплекта.
	for file, data := range map[string][]byte{
		filepath.Join(c.Paths.Data, "original.bin"):     {0x00, 0x7f, 0x80, 0xff},
		filepath.Join(c.Paths.Data, "prepared.mp4"):     []byte("previous-prepared-media"),
		filepath.Join(c.Paths.Data, "diagnostics.log"):  []byte("previous-installation-log\n"),
		filepath.Join(scope.Namespace(), "owned.media"): []byte("previous-owned-runtime-media"),
	} {
		// err сообщает о записи только собственного файла, сохранность которого будет проверена.
		if err := os.WriteFile(file, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	// err сообщает об учёте физического файла, который старая область ещё имеет право очистить.
	if err := scope.RegisterRuntime(context.Background(), "owned.media", processowner.RegularFile); err != nil {
		t.Fatal(err)
	}
	// suffix выбирает весь архивный комплект SQLite, не открываемый на запись самим тестом.
	for _, suffix := range []string{"", "-wal", "-shm"} {
		// data содержит точные архивные байты; err сообщает об отказе чтения только исходной фикстуры.
		data, err := os.ReadFile(filepath.Join("..", "storage", "testdata", "encoding_legacy_v1", "state.sqlite"+suffix))
		if err != nil {
			t.Fatal(err)
		}
		if err = os.WriteFile(c.DatabasePath()+suffix, data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	return scope.ID()
}

// TestProfileModelFreshInstallationPreservesPreviousRoots проверяет исходное требование сохранности файлов.
// t запускает и повторно открывает настоящее приложение с новыми Data/Runtime, проверяет пустую новую БД,
// собственную очистку нового runtime и полную неизменность прежних БД, медиа и журналов на обоих циклах.
// Обработчики очистки останавливают приложение до удаления принадлежащих тесту каталогов.
func TestProfileModelFreshInstallationPreservesPreviousRoots(t *testing.T) {
	// helper собирается один раз; previous и fresh имеют разные изолированные корни через существующие пути.
	helper := acceptanceHelper(t)
	previous, fresh := acceptanceConfig(t, helper), acceptanceConfig(t, helper)
	// previousScopeID адресует настоящую прежнюю runtime-область, не связанную с новым приложением.
	previousScopeID := profileModelPreviousInstallation(t, previous, helper)
	// dataBefore и runtimeBefore сохраняют все имена, права и байты после закрытия прежних владельцев.
	dataBefore := profileModelTreeSnapshot(t, previous.Paths.Data)
	runtimeBefore := profileModelTreeSnapshot(t, previous.Paths.Runtime)
	// streamID фиксирует только новую конфигурацию, которая должна пережить повторное открытие новой БД.
	var streamID string
	// cycle отделяет первичное создание текущей схемы от повторного bootstrap/Recover.
	for cycle := range 2 {
		// application и err описывают настоящий запуск с временными loopback-слушателями и реальными владельцами.
		application, err := start(context.Background(), fresh, acceptanceDependencies(&acceptanceLog{}))
		if application != nil {
			t.Cleanup(func() {
				// Останавливает application даже при досрочном отказе следующей проверки.
				acceptanceStop(t, application)
			})
		}
		if err != nil {
			t.Fatal("Новая установка не запустилась независимо от прежней БД", err)
		}
		if acceptanceHTTP(t, application, "GET", "/health/ready", "", 200)["status"] != "ready" {
			t.Fatal("Управляющая часть новой установки не готова")
		}
		// assets и err проверяют, что старые медиафайлы не стали новым Asset автоматически.
		assets, err := application.core.ListAssets(context.Background(), core.PageRequest{Query: core.ListQuery{Limit: 10}})
		if err != nil || len(assets.Items) != 0 {
			t.Fatal("Прежний файловый комплект усвоен новой БД", assets, err)
		}
		if cycle == 0 {
			// streams проверяет начальную пустоту нового хранилища до создания собственной конфигурации.
			// err сообщает об отказе чтения списка потоков новой установки.
			streams, err := application.core.ListStreams(context.Background(), core.PageRequest{Query: core.ListQuery{Limit: 10}})
			if err != nil || len(streams.Items) != 0 {
				t.Fatal("Прежняя конфигурация импортирована", streams, err)
			}
			streamID = acceptanceHTTP(t, application, "POST", "/streams", `{"name":"Независимая установка","publication_key":"fresh-installation-key"}`, 201)["id"].(string)
		}
		if acceptanceHTTP(t, application, "GET", "/streams/"+streamID, "", 200)["name"] != "Независимая установка" {
			t.Fatal("Повторное открытие потеряло собственную конфигурацию")
		}
		// err подтверждает, что новый реестр не видит прежний журнал области даже по её точному ID.
		if _, err := application.processes.OpenScope(context.Background(), previousScopeID); !errors.Is(err, processowner.ErrUnknown) {
			t.Fatal("Новый реестр усвоил прежнюю область", err)
		}
		// current создаёт собственную новую область, чтобы проверить реальный RuntimeRoot и физическую очистку.
		current, err := application.processes.Create(context.Background(), "fresh-installation-owner")
		if current != nil {
			t.Cleanup(func() {
				// err сообщает о снятии backend-владения даже при отказе проверки пути новой области.
				if err := current.CloseContext(context.Background()); err != nil {
					t.Error(err)
				}
			})
		}
		if err != nil {
			t.Fatal(err)
		}
		if filepath.Dir(current.Namespace()) != fresh.Paths.Runtime {
			t.Fatal("Новая область размещена за пределами нового Runtime", current.Namespace())
		}
		if err = current.CloseContext(context.Background()); err != nil {
			t.Fatal(err)
		}
		// ctx ограничивает всю сверку штатным сроком операции; cancel освобождает её таймер.
		ctx, cancel := context.WithTimeout(context.Background(), fresh.Session.OperationTimeout)
		t.Cleanup(cancel)
		// ticker задаёт период повторного чтения при временно занятой блокировке операции.
		ticker := time.NewTicker(5 * time.Millisecond)
		t.Cleanup(ticker.Stop)
		for {
			// results содержит только текущую полную страницу; next и err выявляют неполную либо неудачную сверку.
			results, next, err := application.processes.RecoverPage(ctx, "", application.processes.PageLimit())
			if err != nil || next != "" || len(results) != cycle+1 {
				t.Fatal("Новый реестр содержит посторонние области либо не завершил сверку", results, next, err)
			}
			// allJoined требует завершённости всех областей в одном текущем проходе, без накопления прежних доказательств.
			allJoined := true
			// result проверяет принадлежность и достоверность каждой области до решения о повторе.
			for _, result := range results {
				if result.ID == previousScopeID || result.Unknown {
					t.Fatal("Нарушена независимость восстановления runtime", result)
				}
				allJoined = allJoined && result.Joined
			}
			if allJoined {
				break
			}
			select {
			case <-ctx.Done():
				t.Fatal("Не подтверждена завершённость новых областей runtime", results, ctx.Err())
			case <-ticker.C:
			}
		}
		ticker.Stop()
		cancel()
		if _, err = os.Stat(current.Namespace()); !errors.Is(err, os.ErrNotExist) {
			t.Fatal("Не подтверждена физическая очистка новой области", err)
		}
		acceptanceStop(t, application)
		if !reflect.DeepEqual(dataBefore, profileModelTreeSnapshot(t, previous.Paths.Data)) || !reflect.DeepEqual(runtimeBefore, profileModelTreeSnapshot(t, previous.Paths.Runtime)) {
			t.Fatal("Новый bootstrap/recovery изменил прежние БД, медиа, runtime либо журналы")
		}
	}
}
