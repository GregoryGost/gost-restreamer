package buildinfo

import (
	"runtime"
	"runtime/debug"
	"testing"
)

// Проверяет приоритет внедрённой версии, чтение VCS-метаданных и явные значения при отсутствии или
// недопустимости сведений.
func TestResolveBuildMetadata(t *testing.T) {
	// tc задаёт источники метаданных и ожидаемый результат их объединения.
	for _, tc := range []struct {
		// name задаёт название проверяемого сочетания сведений о сборке.
		name string
		// embedded задаёт явно внедрённую версию, которая должна иметь приоритет перед модулем.
		embedded string
		// build содержит необязательные метаданные Go, подставляемые вместо чтения бинарника.
		build *debug.BuildInfo
		// want задаёт ожидаемое происхождение версии, коммита, состояния дерева и версии Go.
		want Info
	}{
		{name: "без метаданных", want: Info{"devel", "unknown", "unknown", "go-test"}},
		{name: "версия модуля", build: &debug.BuildInfo{Main: debug.Module{Version: "v0.1.0"}}, want: Info{"v0.1.0", "unknown", "unknown", "go-test"}},
		{name: "локальная сборка", build: &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}}, want: Info{"devel", "unknown", "unknown", "go-test"}},
		{name: "чистое дерево", build: &debug.BuildInfo{GoVersion: "go1.27.3", Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: "abcdef0123456789"}, {Key: "vcs.modified", Value: "false"}}}, want: Info{"devel", "abcdef0123456789", "false", "go1.27.3"}},
		{name: "изменённое дерево", build: &debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.modified", Value: "true"}}}, want: Info{"devel", "unknown", "true", "go-test"}},
		{name: "неизвестное состояние", build: &debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.revision"}, {Key: "vcs.modified", Value: "invalid"}, {Key: "vcs", Value: "git"}}}, want: Info{"devel", "unknown", "unknown", "go-test"}},
		{name: "встроенная версия приоритетна", embedded: "v0.1.0-dev.2+abcdef0", build: &debug.BuildInfo{Main: debug.Module{Version: "v0.0.1"}}, want: Info{"v0.1.0-dev.2+abcdef0", "unknown", "unknown", "go-test"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет приоритет встроенной версии и заполнение отсутствующих метаданных для выбранного набора
			// сведений о сборке.

			// got содержит результат объединения источников для текущего сценария.
			if got := resolve(tc.embedded, tc.build, "go-test"); got != tc.want {
				t.Fatalf("метаданные: получено %+v, ожидалось %+v", got, tc.want)
			}
		})
	}
	// got содержит обозначение отсутствующей версии Go при пустых источниках.
	if got := resolve("", nil, "").GoVersion; got != "unknown" {
		t.Fatalf("отсутствующая версия Go: %q", got)
	}
}

// Проверяет полноту текущих сведений о сборке и точный формат строки версии.
func TestCurrentAndString(t *testing.T) {
	// current содержит реальные сведения текущего тестового бинарника для проверки заполненности.
	current := Current()
	if current.Version == "" || current.Commit == "" || current.Dirty == "" || current.GoVersion != runtime.Version() {
		t.Fatalf("неполные метаданные исполняемого файла: %+v", current)
	}
	// info задаёт фиксированные сведения для проверки единого формата CLI.
	info := Info{"v0.1.0", "abcdef0123456789", "false", "go1.27.3"}
	// got содержит сформированную строку сведений; want задаёт её точное ожидаемое представление.
	if got, want := info.String(), "v0.1.0 commit=abcdef0123456789 dirty=false go=go1.27.3"; got != want {
		t.Fatalf("формат версии: %q, ожидалось %q", got, want)
	}
}
