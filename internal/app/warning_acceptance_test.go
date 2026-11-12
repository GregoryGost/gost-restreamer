package app

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"log/slog"
	"net"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/config"
)

// Проверяет, что обязательный WARN о приоритете конфигурационных файлов проходит при пороге ERROR до
// первой привязки слушателя, сохраняя фильтрацию обычных записей.
func TestAcceptanceFilePriorityWarningSurvivesErrorThreshold(t *testing.T) {
	// dir выделяет изолированный каталог двух стандартных конфигурационных файлов.
	dir := t.TempDir()
	// yamlPath задаёт приоритетный файл YAML; ymlPath задаёт второй существующий файл YML.
	yamlPath, ymlPath := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "config.yml")
	// path содержит очередной стандартный файл для записи одинакового уровня ERROR.
	for _, path := range []string{yamlPath, ymlPath} {
		// err содержит отказ записи YAML-фикстуры с уровнем журнала ERROR.
		if err := os.WriteFile(path, []byte("logging:\n  level: ERROR\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// selected содержит выбранный YAML и предупреждение; err содержит отказ выбора двух файлов.
	selected, err := config.SelectPaths("", yamlPath, ymlPath)
	if err != nil {
		t.Fatal(err)
	}
	if selected.Path != yamlPath {
		t.Fatalf("выбран %q вместо %q", selected.Path, yamlPath)
	}
	// loaded содержит реальные настройки ERROR, прочитанные из выбранного YAML.
	loaded, err := config.Load(selected.Path)
	if err != nil {
		t.Fatal(err)
	}
	// c дополняет изолированные настройки приложения уровнем из выбранного файла.
	c := acceptanceConfig(t, acceptanceHelper(t))
	c.Logging = loaded.Logging
	// log сохраняет JSON-записи до раннего отказа привязки слушателя и завершения частичной сборки.
	log := &acceptanceLog{}
	// d подставляет журнал, обязательное предупреждение и контроль ранних записей перед отказом
	// слушателя.
	d := acceptanceDependencies(log)
	d.warning = selected.Warning
	d.afterAcquire = func(stage string, a *Application) error {
		// После открытия журнала создаёт обычные записи разных уровней для сравнения с обязательным
		// предупреждением выбора файла.

		if stage == "logger" {
			a.sink.Logger().Info("ordinary_info")
			a.sink.Logger().Warn("ordinary_warning")
			a.sink.Logger().Error("ordinary_error")
		}
		return nil
	}
	// bindFailure задаёт узнаваемый отказ до приобретения слушателя, после записи предупреждения.
	bindFailure := errors.New("контролируемый отказ до привязки слушателя")
	d.listen = func(context.Context, string, string) (net.Listener, error) {
		// Имитирует отказ привязки до появления слушателей, чтобы проверить сохранение предупреждения раннего
		// запуска.

		return nil, bindFailure
	}
	// a содержит частичного владельца запуска после отказа привязки слушателя.
	a, err := start(context.Background(), c, d)
	if !errors.Is(err, bindFailure) || a == nil {
		t.Fatalf("не достигнута граница после журнала: app=%p, err=%v", a, err)
	}
	t.Cleanup(func() {
		// Завершает освобождение частично созданного приложения после отказа привязки.
		acceptanceStop(t, a)
	})
	if !isClosed(a.Done()) {
		t.Fatal("отказ запуска не завершил запись журнала")
	}
	if c.Logging.Level != slog.LevelError {
		t.Fatalf("тест требует ERROR, получено %v", c.Logging.Level)
	}
	// warnings считает обязательные предупреждения выбора файла; ordinaryErrors считает контрольные
	// записи ERROR, проходящие обычный фильтр.
	warnings, ordinaryErrors := 0, 0
	// scanner читает итоговый журнал частичного запуска по целым JSON-записям.
	scanner := bufio.NewScanner(strings.NewReader(log.text()))
	for scanner.Scan() {
		// record принимает поля очередного события для проверки уровня, путей и обычной
		// фильтрации.
		var record map[string]any
		// err содержит отказ разбора очередной записи захваченного журнала.
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatal(err)
		}
		switch record["msg"] {
		case "config_file_priority":
			warnings++
			if record["level"] != "WARN" || record["yaml_path"] != yamlPath || record["yml_path"] != ymlPath || record["selected"] != yamlPath {
				t.Error("предупреждение потеряло уровень или точные пути", record)
			}
		case "ordinary_info", "ordinary_warning":
			t.Error("обычная запись обошла настроенный порог ERROR", record)
		case "ordinary_error":
			ordinaryErrors++
			if record["level"] != "ERROR" {
				t.Error("контрольная запись потеряла уровень ERROR", record)
			}
		}
	}
	// err содержит отказ построчного чтения итогового журнала.
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if ordinaryErrors != 1 {
		t.Errorf("ожидалась одна контрольная запись ERROR, получено %d", ordinaryErrors)
	}
	if warnings != 1 {
		t.Errorf("при пороге ERROR ожидалось одно обязательное предупреждение WARN, получено %d; журнал:\n%s", warnings, log.text())
	}
}
