package buildinfo

import (
	"bytes"
	"context"
	executableinfo "debug/buildinfo"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"runtime/debug"
	"strings"
	"testing"
	"time"
)

func TestAcceptanceVersionProvenance(t *testing.T) {
	const revision = "0123456789abcdef0123456789abcdef01234567"
	for _, tc := range []struct {
		name, embedded string
		metadata       *debug.BuildInfo
		fallbackGo     string
		want           Info
	}{
		{name: "без метаданных", fallbackGo: "go1.27.3", want: Info{"devel", "unknown", "unknown", "go1.27.3"}},
		{name: "без toolchain", want: Info{"devel", "unknown", "unknown", "unknown"}},
		{name: "архив с явной версией", embedded: "v0.1.0", fallbackGo: "go1.27.3", want: Info{"v0.1.0", "unknown", "unknown", "go1.27.3"}},
		{name: "версия модуля", metadata: &debug.BuildInfo{GoVersion: "go1.27.3", Main: debug.Module{Version: "v0.2.0"}}, want: Info{"v0.2.0", "unknown", "unknown", "go1.27.3"}},
		{name: "нативная разработка", metadata: &debug.BuildInfo{Main: debug.Module{Version: "(devel)"}, Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: revision}, {Key: "vcs.modified", Value: "true"}}}, fallbackGo: "go1.27.3", want: Info{"devel", revision, "true", "go1.27.3"}},
		{name: "явная версия имеет приоритет", embedded: "v0.2.0-dev.4+0123456", metadata: &debug.BuildInfo{GoVersion: "go1.27.3", Main: debug.Module{Version: "v0.1.0"}, Settings: []debug.BuildSetting{{Key: "vcs.revision", Value: revision}, {Key: "vcs.modified", Value: "false"}}}, want: Info{"v0.2.0-dev.4+0123456", revision, "false", "go1.27.3"}},
		{name: "неизвестное состояние дерева", metadata: &debug.BuildInfo{Settings: []debug.BuildSetting{{Key: "vcs.modified", Value: "invalid"}, {Key: "vcs.revision", Value: ""}}}, fallbackGo: "go1.27.3", want: Info{"devel", "unknown", "unknown", "go1.27.3"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := resolve(tc.embedded, tc.metadata, tc.fallbackGo); got != tc.want {
				t.Fatalf("сведения о сборке: получено %+v, ожидалось %+v", got, tc.want)
			}
		})
	}
	info := Info{"v0.1.0", revision, "false", "go1.27.3"}
	if got, want := info.String(), "v0.1.0 commit="+revision+" dirty=false go=go1.27.3"; got != want {
		t.Fatalf("строка версии: %q, ожидалось %q", got, want)
	}
}

func TestAcceptanceNativeBinariesMatchEmbeddedProvenance(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	gitHead := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	revision, gitErr := gitHead.Output()
	for _, embedded := range []string{"", "v0.2.0-dev.4+0123456"} {
		var first string
		for _, name := range []string{"gost-restreamer", "gost-process-helper"} {
			binary := filepath.Join(t.TempDir(), name)
			args := []string{"build", "-trimpath", "-o", binary}
			if embedded != "" {
				args = append(args, "-ldflags", "-X github.com/GregoryGost/gost-restreamer/internal/buildinfo.version="+embedded)
			}
			build := exec.CommandContext(ctx, "go", append(args, "../../cmd/"+name)...)
			build.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOWORK=off", "GOFLAGS=-mod=readonly", "GOPROXY=off")
			if output, err := build.CombinedOutput(); err != nil {
				t.Fatalf("нативная сборка %s: %v\n%s", name, err, output)
			}
			metadata, err := executableinfo.ReadFile(binary)
			if err != nil {
				t.Fatal(err)
			}
			settings := make(map[string]string)
			for _, setting := range metadata.Settings {
				settings[setting.Key] = setting.Value
			}
			if gitErr == nil && settings["vcs.revision"] != strings.TrimSpace(string(revision)) {
				t.Fatalf("нативная сборка потеряла фактический Git-коммит: %v", settings)
			}
			wantCommit, wantDirty := "unknown", "unknown"
			if value := settings["vcs.revision"]; value != "" {
				wantCommit = value
			}
			if value := settings["vcs.modified"]; value == "true" || value == "false" {
				wantDirty = value
			}
			wantVersion := embedded
			if wantVersion == "" {
				wantVersion = metadata.Main.Version
				if wantVersion == "" || wantVersion == "(devel)" {
					wantVersion = "devel"
				}
			}
			want := fmt.Sprintf("%s commit=%s dirty=%s go=%s\n", wantVersion, wantCommit, wantDirty, metadata.GoVersion)
			command := exec.CommandContext(ctx, binary, "--version")
			var stdout, stderr bytes.Buffer
			command.Stdout, command.Stderr = &stdout, &stderr
			if err := command.Run(); err != nil || stdout.String() != want || stderr.Len() != 0 {
				t.Fatalf("нативный %s, version=%q: err=%v, stdout=%q, stderr=%q, ожидалось %q", name, embedded, err, stdout.String(), stderr.String(), want)
			}
			if first != "" && stdout.String() != first {
				t.Fatalf("бинарники сообщают разные сведения: %q и %q", first, stdout.String())
			}
			first = stdout.String()
		}
	}
}

func TestAcceptanceBothBinariesVersionWithoutVCS(t *testing.T) {
	const embedded = "v0.2.0-dev.3+0123456"
	want := embedded + " commit=unknown dirty=unknown go=" + runtime.Version() + "\n"
	for _, name := range []string{"gost-restreamer", "gost-process-helper"} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			binary := filepath.Join(dir, name)
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			build := exec.CommandContext(ctx, "go", "build", "-buildvcs=false", "-trimpath", "-ldflags", "-X github.com/GregoryGost/gost-restreamer/internal/buildinfo.version="+embedded, "-o", binary, "../../cmd/"+name)
			build.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOWORK=off", "GOFLAGS=-mod=readonly", "GOPROXY=off")
			if output, err := build.CombinedOutput(); err != nil {
				t.Fatalf("сборка %s: %v\n%s", name, err, output)
			}
			command := exec.CommandContext(ctx, binary, "--version")
			command.Dir = dir
			var stdout, stderr bytes.Buffer
			command.Stdout, command.Stderr = &stdout, &stderr
			if err := command.Run(); err != nil || stdout.String() != want || stderr.Len() != 0 {
				t.Fatalf("--version: err=%v, stdout=%q, stderr=%q; ожидалось %q", err, stdout.String(), stderr.String(), want)
			}
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 || entries[0].Name() != name {
				t.Fatalf("--version создал рабочие файлы: entries=%v, err=%v", entries, err)
			}
		})
	}
}
