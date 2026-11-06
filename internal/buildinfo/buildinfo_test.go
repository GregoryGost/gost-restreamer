package buildinfo

import (
	"runtime"
	"runtime/debug"
	"testing"
)

func TestResolveBuildMetadata(t *testing.T) {
	for _, tc := range []struct {
		name     string
		embedded string
		build    *debug.BuildInfo
		want     Info
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
			if got := resolve(tc.embedded, tc.build, "go-test"); got != tc.want {
				t.Fatalf("метаданные: получено %+v, ожидалось %+v", got, tc.want)
			}
		})
	}
	if got := resolve("", nil, "").GoVersion; got != "unknown" {
		t.Fatalf("отсутствующая версия Go: %q", got)
	}
}

func TestCurrentAndString(t *testing.T) {
	current := Current()
	if current.Version == "" || current.Commit == "" || current.Dirty == "" || current.GoVersion != runtime.Version() {
		t.Fatalf("неполные метаданные исполняемого файла: %+v", current)
	}
	info := Info{"v0.1.0", "abcdef0123456789", "false", "go1.27.3"}
	if got, want := info.String(), "v0.1.0 commit=abcdef0123456789 dirty=false go=go1.27.3"; got != want {
		t.Fatalf("формат версии: %q, ожидалось %q", got, want)
	}
}
