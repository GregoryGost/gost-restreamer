package config

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"
)

// acceptanceWriteConfig создаёт тестовый YAML с ограниченными правами и завершает сценарий при ошибке
// записи.
func acceptanceWriteConfig(t *testing.T, path, content string) {
	t.Helper()
	// err содержит отказ записи YAML-фикстуры с ограниченными правами.
	if err := os.WriteFile(path, []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

// Проверяет выбор единственного стандартного файла, приоритет yaml над yml и явный путь с точными
// данными предупреждения.
func TestAcceptanceConfigFileSelection(t *testing.T) {
	// tc задаёт комбинацию существующих файлов и явного пути вместе с ожидаемым результатом выбора.
	for _, tc := range []struct {
		// name задаёт название сценария выбора конфигурации.
		name string
		// yaml определяет создание стандартного YAML; yml определяет создание стандартного YML;
		// explicit включает явный выбор YML.
		yaml, yml, explicit bool
		// wantSuffix задаёт ожидаемое расширение выбранного стандартного файла.
		wantSuffix string
		// wantWarning ожидает предупреждение о двух файлах; wantError ожидает отказ при
		// отсутствии обоих файлов.
		wantWarning, wantError bool
	}{
		{name: "нет файлов", wantError: true},
		{name: "только yaml", yaml: true, wantSuffix: ".yaml"},
		{name: "только yml", yml: true, wantSuffix: ".yml"},
		{name: "оба файла", yaml: true, yml: true, wantSuffix: ".yaml", wantWarning: true},
		{name: "явный yml", yaml: true, yml: true, explicit: true, wantSuffix: ".yml"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет выбранную комбинацию стандартных и явного путей, включая приоритет YAML и предупреждение о
			// двух файлах.

			// dir выделяет изолированный каталог кандидатов конфигурации.
			dir := t.TempDir()
			// yamlPath указывает стандартный YAML-кандидат текущего сценария.
			yamlPath := filepath.Join(dir, "config.yaml")
			// ymlPath указывает стандартный YML-кандидат текущего сценария.
			ymlPath := filepath.Join(dir, "config.yml")
			if tc.yaml {
				acceptanceWriteConfig(t, yamlPath, "{}\n")
			}
			if tc.yml {
				acceptanceWriteConfig(t, ymlPath, "{}\n")
			}
			// explicit содержит точный явный путь либо пустую строку для стандартного
			// выбора.
			explicit := ""
			if tc.explicit {
				explicit = ymlPath
			}
			// selected содержит выбранный файл и предупреждение; err содержит отказ выбора
			// при текущем наборе файлов.
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
			// wantPath содержит ожидаемый полный путь выбранного файла.
			wantPath := filepath.Join(dir, "config"+tc.wantSuffix)
			if selected.Path != wantPath || (selected.Warning != nil) != tc.wantWarning {
				t.Fatalf("выбор файла: %+v; ожидался %s, предупреждение=%v", selected, wantPath, tc.wantWarning)
			}
			// w содержит предупреждение о двух файлах для проверки сохранности обоих путей и
			// выбранного YAML.
			if w := selected.Warning; w != nil && (w.YAMLPath != yamlPath || w.YMLPath != ymlPath || w.Selected != yamlPath) {
				t.Fatalf("предупреждение потеряло пути: %+v", w)
			}
		})
	}
}

// Проверяет, что повреждённый, нечитаемый или явно отсутствующий выбранный файл не заменяется вторым
// стандартным путём.
func TestAcceptanceSelectedFileNeverFallsBack(t *testing.T) {
	// kind выбирает способ отказа выбранного файла при наличии исправного YML.
	for _, kind := range []string{"повреждённый yaml", "каталог", "висячая ссылка", "явно отсутствующий"} {
		t.Run(kind, func(t *testing.T) {
			// Проверяет, что ошибка выбранного файла не заменяет его исправным файлом с другим расширением.

			// dir выделяет изолированный каталог исправного и неисправного кандидатов.
			dir := t.TempDir()
			// yamlPath задаёт путь неисправного выбранного кандидата; ymlPath задаёт
			// исправный запасной файл, переход к которому запрещён.
			yamlPath, ymlPath := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "config.yml")
			acceptanceWriteConfig(t, ymlPath, "{}\n")
			// explicit включает явный выбор отсутствующего YAML только в соответствующем
			// сценарии.
			explicit := ""
			switch kind {
			case "повреждённый yaml":
				acceptanceWriteConfig(t, yamlPath, "[broken")
			case "каталог":
				// err содержит отказ создания каталога вместо выбранного файла
				// конфигурации.
				if err := os.Mkdir(yamlPath, 0700); err != nil {
					t.Fatal(err)
				}
			case "висячая ссылка":
				// err содержит отказ создания висячей ссылки на месте выбранного YAML.
				if err := os.Symlink(filepath.Join(dir, "missing"), yamlPath); err != nil {
					t.Fatal(err)
				}
			case "явно отсутствующий":
				explicit = yamlPath
			}
			// selected содержит итог выбора неисправного кандидата; err сначала содержит
			// отказ выбора, затем возможный отказ его загрузки.
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

// Проверяет допустимость значений по умолчанию и одинаковую загрузку файлов yaml/yml, отклоняя пустой,
// лишний, повторный или слишком большой документ.
func TestAcceptanceYAMLDocumentsAndDefaults(t *testing.T) {
	// defaults содержит встроенные настройки для проверки допустимости и закреплённых начальных
	// значений.
	defaults := Default()
	// err содержит отказ проверки встроенных настроек сервера.
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
	// suffix задаёт очередное расширение файла, которое должно одинаково принимать пустое
	// отображение YAML.
	for _, suffix := range []string{".yaml", ".yml"} {
		// path указывает временный файл с текущим расширением для загрузки встроенных настроек.
		path := filepath.Join(t.TempDir(), "config"+suffix)
		acceptanceWriteConfig(t, path, "{}\n")
		// got содержит настройки после пустого отображения; err содержит отказ их загрузки.
		got, err := Load(path)
		if err != nil || !reflect.DeepEqual(got, defaults) {
			t.Fatalf("%s изменил встроенные значения: %v; %+v", suffix, err, got)
		}
	}
	// tc задаёт недопустимый документ и ожидаемое имя ошибочного поля.
	for _, tc := range []struct { /* name задаёт название недопустимого документа; content содержит его YAML; field задаёт имя поля, ожидаемое в диагностике, либо пустую строку. */
		name, content, field string
	}{
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
			// Проверяет отказ выбранному недопустимому документу YAML с указанием пути и известного ошибочного
			// поля.

			// path указывает изолированный файл текущего недопустимого документа.
			path := filepath.Join(t.TempDir(), "config.yaml")
			acceptanceWriteConfig(t, path, tc.content)
			// err содержит отказ загрузки документа, в котором проверяются путь файла и
			// доступное имя поля.
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), path) || (tc.field != "" && !strings.Contains(err.Error(), tc.field)) {
				t.Fatalf("ожидалась ошибка файла и поля %q: %v", tc.field, err)
			}
		})
	}
}

// Проверяет сохранение пропущенных полей и явных нулей при наложении YAML, а также независимость
// загруженной конфигурации от последующей записи файла.
func TestAcceptanceNestedOverlayAndExplicitZero(t *testing.T) {
	// path указывает файл для проверки частичного наложения и последующего повторного чтения.
	path := filepath.Join(t.TempDir(), "config.yml")
	acceptanceWriteConfig(t, path, "http:\n  address: '127.0.0.1:8181'\n  idle_timeout: 17s\nstorage:\n  max_idle_conns: 0\nffmpeg:\n  max_restarts: 0\n")
	// got содержит конфигурацию после первого наложения YAML; err содержит отказ первой загрузки.
	got, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	// want содержит ожидаемую копию defaults с изменениями только явно заданных полей, включая нули.
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
	// reloaded содержит настройки после повторного чтения изменённого файла.
	reloaded, err := Load(path)
	if err != nil || reloaded.HTTP.Address != "127.0.0.1:8282" || reloaded.Storage.MaxIdleConns != Default().Storage.MaxIdleConns {
		t.Fatalf("новая загрузка не применила изменённый файл поверх defaults: %+v; %v", reloaded, err)
	}
}

// Проверяет отклонение недопустимых вложенных настроек, типов, длительностей, адресов, путей и
// несогласованных бюджетов с указанием файла.
func TestAcceptanceInvalidNestedSettings(t *testing.T) {
	// tc задаёт ошибочную вложенную настройку и ожидаемую диагностику.
	for _, tc := range []struct { /* name задаёт название ошибки настройки; content содержит недопустимый YAML; field задаёт известное ошибочное поле для проверки сообщения. */
		name, content, field string
	}{
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
			// Проверяет отказ выбранной ошибке вложенной настройки без молчаливого принятия значения.

			// path указывает временный YAML текущего сценария недопустимой настройки.
			path := filepath.Join(t.TempDir(), "config.yaml")
			acceptanceWriteConfig(t, path, tc.content)
			// err содержит отказ загрузки с проверяемыми путём файла и именем ошибочного
			// поля.
			_, err := Load(path)
			if err == nil || !strings.Contains(err.Error(), path) || tc.field != "" && !strings.Contains(err.Error(), tc.field) {
				t.Fatalf("ожидалась ошибка выбранного файла и поля %q: %v", tc.field, err)
			}
		})
	}
}
