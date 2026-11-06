package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

func acceptanceWriteConfig(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

func TestAcceptanceConfigFileSelection(t *testing.T) {
	for _, tc := range []struct {
		name                   string
		yaml, yml, explicit    bool
		wantSuffix             string
		wantWarning, wantError bool
	}{
		{name: "нет файлов", wantError: true},
		{name: "только yaml", yaml: true, wantSuffix: ".yaml"},
		{name: "только yml", yml: true, wantSuffix: ".yml"},
		{name: "оба файла", yaml: true, yml: true, wantSuffix: ".yaml", wantWarning: true},
		{name: "явный yml", yaml: true, yml: true, explicit: true, wantSuffix: ".yml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			yamlPath := filepath.Join(dir, "config.yaml")
			ymlPath := filepath.Join(dir, "config.yml")
			if tc.yaml {
				acceptanceWriteConfig(t, yamlPath, "{}\n")
			}
			if tc.yml {
				acceptanceWriteConfig(t, ymlPath, "{}\n")
			}
			explicit := ""
			if tc.explicit {
				explicit = ymlPath
			}
			selected, err := SelectPaths(explicit, yamlPath, ymlPath)
			if tc.wantError {
				if err == nil {
					t.Fatal("отсутствующая конфигурация принята")
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			wantPath := filepath.Join(dir, "config"+tc.wantSuffix)
			if selected.Path != wantPath || (selected.Warning != nil) != tc.wantWarning {
				t.Fatalf("выбор файла: %+v; ожидался %s, предупреждение=%v", selected, wantPath, tc.wantWarning)
			}
			if w := selected.Warning; w != nil && (w.YAMLPath != yamlPath || w.YMLPath != ymlPath || w.Selected != yamlPath) {
				t.Fatalf("предупреждение потеряло пути: %+v", w)
			}
		})
	}
}

func TestAcceptanceSelectedFileNeverFallsBack(t *testing.T) {
	for _, kind := range []string{"повреждённый yaml", "каталог", "висячая ссылка", "явно отсутствующий"} {
		t.Run(kind, func(t *testing.T) {
			dir := t.TempDir()
			yamlPath, ymlPath := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "config.yml")
			acceptanceWriteConfig(t, ymlPath, "{}\n")
			explicit := ""
			switch kind {
			case "повреждённый yaml":
				acceptanceWriteConfig(t, yamlPath, "[broken")
			case "каталог":
				if err := os.Mkdir(yamlPath, 0700); err != nil {
					t.Fatal(err)
				}
			case "висячая ссылка":
				if err := os.Symlink(filepath.Join(dir, "missing"), yamlPath); err != nil {
					t.Fatal(err)
				}
			case "явно отсутствующий":
				explicit = yamlPath
			}
			selected, err := SelectPaths(explicit, yamlPath, ymlPath)
			if err == nil {
				if selected.Path != yamlPath {
					t.Fatalf("ошибочный выбранный файл заменён: %+v", selected)
				}
				_, err = Load(selected.Path)
			}
			if err == nil {
				t.Fatal("ошибка выбранного файла не остановила загрузку")
			}
		})
	}
}

func TestAcceptanceYAMLDocumentsAndDefaults(t *testing.T) {
	defaults := Default()
	if err := defaults.Validate(); err != nil {
		t.Fatalf("встроенные значения недопустимы: %v", err)
	}
	if defaults.HTTP.Address != ":8080" || defaults.IngestAddress != ":1935" || defaults.Paths.Data != "/var/lib/gost-restreamer" || defaults.Paths.Runtime != "/run/gost-restreamer" {
		t.Fatalf("начальные адреса и пути не соответствуют контракту: %+v", defaults)
	}
	if defaults.Lifecycle.StartupTimeout != 30*time.Second || defaults.Lifecycle.ShutdownTimeout != 30*time.Second || defaults.Storage.MaxOpenConns != 4 || defaults.Storage.MaxIdleConns != 4 || defaults.Storage.BusyTimeout != time.Second {
		t.Fatal("начальные сроки или бюджет SQLite не соответствуют контракту")
	}
	if defaults.FFmpeg.StartupTimeout != 15*time.Second || defaults.FFmpeg.VideoTimeout != 3*time.Second || defaults.FFmpeg.AudioTimeout != 3*time.Second || defaults.FFmpeg.MaxRestarts != 3 || defaults.Monitoring.MaxSources != 64 || defaults.GPU.MaxDevices != 8 {
		t.Fatal("начальные медиабюджеты не соответствуют контракту")
	}
	for _, suffix := range []string{".yaml", ".yml"} {
		path := filepath.Join(t.TempDir(), "config"+suffix)
		acceptanceWriteConfig(t, path, "{}\n")
		got, err := Load(path)
		if err != nil || !reflect.DeepEqual(got, defaults) {
			t.Fatalf("%s изменил встроенные значения: %v; %+v", suffix, err, got)
		}
	}
	for _, tc := range []struct{ name, content, field string }{
		{"пустой", "", ""},
		{"только комментарий", "# Нет настроек.\n", ""},
		{"null", "null\n", ""},
		{"последовательность", "[]\n", ""},
		{"скаляр", "plain\n", ""},
		{"второй документ", "{}\n---\n{}\n", ""},
		{"повтор ключа", "unknown_setting: 1\nunknown_setting: 2\n", "unknown_setting"},
		{"неизвестный ключ", "unknown_setting: 1\n", "unknown_setting"},
		{"предел размера", "#" + strings.Repeat("x", 1<<20) + "\n{}\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			acceptanceWriteConfig(t, path, tc.content)
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), path) || (tc.field != "" && !strings.Contains(err.Error(), tc.field)) {
				t.Fatalf("ожидалась ошибка файла и поля %q: %v", tc.field, err)
			}
		})
	}
}

func TestAcceptanceNestedOverlayAndExplicitZero(t *testing.T) {
	path := filepath.Join(t.TempDir(), "config.yml")
	acceptanceWriteConfig(t, path, "http:\n  address: '127.0.0.1:8181'\n  idle_timeout: 17s\nstorage:\n  max_idle_conns: 0\nffmpeg:\n  max_restarts: 0\n")
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	want := Default()
	want.HTTP.Address = "127.0.0.1:8181"
	want.HTTP.IdleTimeout = 17 * time.Second
	want.Storage.MaxIdleConns = 0
	want.FFmpeg.MaxRestarts = 0
	if !reflect.DeepEqual(got, want) {
		t.Fatalf("частичное переопределение изменило пропущенные поля или потеряло 0:\nполучено: %+v\nожидалось: %+v", got, want)
	}
	acceptanceWriteConfig(t, path, "http:\n  address: '127.0.0.1:8282'\n")
	if got.HTTP.Address != "127.0.0.1:8181" {
		t.Fatal("уже загруженная конфигурация изменилась вместе с файлом")
	}
	reloaded, err := Load(path)
	if err != nil || reloaded.HTTP.Address != "127.0.0.1:8282" || reloaded.Storage.MaxIdleConns != Default().Storage.MaxIdleConns {
		t.Fatalf("новая загрузка не применила изменённый файл поверх defaults: %+v; %v", reloaded, err)
	}
}

func TestAcceptanceInvalidNestedSettings(t *testing.T) {
	for _, tc := range []struct{ name, content, field string }{
		{"неизвестное вложенное поле", "http:\n  unknown_nested: 1\n", "unknown_nested"},
		{"неизвестное глубокое поле", "publisher:\n  transport:\n    unknown_transport: 1\n", "unknown_transport"},
		{"повтор известного поля", "http:\n  address: ':8080'\n  address: ':8181'\n", "address"},
		{"нулевой обязательный бюджет", "storage:\n  max_open_conns: 0\n", ""},
		{"отрицательный беззнаковый бюджет", "router:\n  max_consumers: -1\n", ""},
		{"переполнение числа", "router:\n  max_consumers: 18446744073709551616\n", ""},
		{"численная длительность", "http:\n  idle_timeout: 100\n", ""},
		{"длительность без единицы", "http:\n  idle_timeout: '100'\n", ""},
		{"неверный тип длительности", "http:\n  idle_timeout: false\n", ""},
		{"null раздела", "http: null\n", ""},
		{"null поля", "http:\n  address: null\n", ""},
		{"нечисленный порт", "http:\n  address: 'localhost:service'\n", ""},
		{"нулевой порт", "ingest_address: ':0'\n", ""},
		{"порт за диапазоном", "http:\n  address: ':65536'\n", ""},
		{"относительный путь", "paths:\n  data: './data'\n", ""},
		{"неочищенный путь", "paths:\n  runtime: '/run/gost/../runtime'\n", ""},
		{"несогласованный пул", "storage:\n  max_open_conns: 1\n  max_idle_conns: 2\n", ""},
		{"недостаточный бюджет источников", "monitoring:\n  max_sources: 1\n", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			path := filepath.Join(t.TempDir(), "config.yaml")
			acceptanceWriteConfig(t, path, tc.content)
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), path) || tc.field != "" && !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("ожидалась ошибка выбранного файла и поля %q: %v", tc.field, err)
			}
		})
	}
}
