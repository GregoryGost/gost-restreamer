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

func TestAcceptanceFilePriorityWarningSurvivesErrorThreshold(t *testing.T) {
	dir := t.TempDir()
	yamlPath, ymlPath := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "config.yml")
	for _, path := range []string{yamlPath, ymlPath} {
		if err := os.WriteFile(path, []byte("logging:\n  level: ERROR\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	selected, err := config.SelectPaths("", yamlPath, ymlPath)
	if err != nil {
		t.Fatal(err)
	}
	if selected.Path != yamlPath {
		t.Fatalf("выбран %q вместо %q", selected.Path, yamlPath)
	}
	loaded, err := config.Load(selected.Path)
	if err != nil {
		t.Fatal(err)
	}
	c := acceptanceConfig(t, acceptanceHelper(t))
	c.Logging = loaded.Logging
	log := &acceptanceLog{}
	d := acceptanceDependencies(log)
	d.warning = selected.Warning
	d.afterAcquire = func(stage string, a *Application) error {
		if stage == "logger" {
			a.sink.Logger().Info("ordinary_info")
			a.sink.Logger().Warn("ordinary_warning")
			a.sink.Logger().Error("ordinary_error")
		}
		return nil
	}
	bindFailure := errors.New("контролируемый отказ до привязки слушателя")
	d.listen = func(context.Context, string, string) (net.Listener, error) {
		return nil, bindFailure
	}
	a, err := start(context.Background(), c, d)
	if !errors.Is(err, bindFailure) || a == nil {
		t.Fatalf("не достигнута граница после журнала: app=%p, err=%v", a, err)
	}
	t.Cleanup(func() { acceptanceStop(t, a) })
	if !isClosed(a.Done()) {
		t.Fatal("отказ запуска не завершил запись журнала")
	}
	if c.Logging.Level != slog.LevelError {
		t.Fatalf("тест требует ERROR, получено %v", c.Logging.Level)
	}
	warnings, ordinaryErrors := 0, 0
	scanner := bufio.NewScanner(strings.NewReader(log.text()))
	for scanner.Scan() {
		var record map[string]any
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
