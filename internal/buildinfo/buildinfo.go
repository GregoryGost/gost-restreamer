// Package buildinfo читает неизменяемые сведения, встроенные при сборке бинарника.
package buildinfo

import (
	"fmt"
	"runtime"
	"runtime/debug"
)

// version задаётся через -ldflags -X при сборке обоих исполняемых файлов.
var version string

// Info содержит версию и происхождение исполняемого файла.
// Dirty принимает значения true, false или unknown, если данных о состоянии дерева нет.
type Info struct {
	Version   string
	Commit    string
	Dirty     string
	GoVersion string
}

// Current возвращает сведения только из бинарника, без обращения к окружению и Git.
func Current() Info {
	b, _ := debug.ReadBuildInfo()
	return resolve(version, b, runtime.Version())
}

func resolve(embedded string, b *debug.BuildInfo, goVersion string) Info {
	info := Info{Version: "devel", Commit: "unknown", Dirty: "unknown", GoVersion: goVersion}
	if b != nil {
		if b.Main.Version != "" && b.Main.Version != "(devel)" {
			info.Version = b.Main.Version
		}
		if b.GoVersion != "" {
			info.GoVersion = b.GoVersion
		}
		for _, setting := range b.Settings {
			switch setting.Key {
			case "vcs.revision":
				if setting.Value != "" {
					info.Commit = setting.Value
				}
			case "vcs.modified":
				if setting.Value == "true" || setting.Value == "false" {
					info.Dirty = setting.Value
				}
			}
		}
	}
	if embedded != "" {
		info.Version = embedded
	}
	if info.GoVersion == "" {
		info.GoVersion = "unknown"
	}
	return info
}

func (i Info) String() string {
	return fmt.Sprintf("%s commit=%s dirty=%s go=%s", i.Version, i.Commit, i.Dirty, i.GoVersion)
}
