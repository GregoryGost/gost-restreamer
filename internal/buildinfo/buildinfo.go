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
	// Version содержит внедрённую версию, версию модуля либо devel при отсутствии обоих источников.
	Version string
	// Commit содержит vcs.revision из метаданных бинарника либо unknown при отсутствии значения.
	Commit string
	Dirty  string
	// GoVersion содержит версию компилятора из метаданных, запасного runtime.Version либо unknown.
	GoVersion string
}

// Current возвращает сведения только из бинарника, без обращения к окружению и Git.
func Current() Info {
	// b содержит доступные метаданные текущего бинарника; отсутствие сведений обрабатывается
	// resolve.
	b, _ := debug.ReadBuildInfo()
	return resolve(version, b, runtime.Version())
}

// resolve объединяет внедрённую версию с метаданными Go, отдавая явной версии приоритет. При
// отсутствии версии возвращает devel, а неизвестные сведения о происхождении, состоянии дерева и
// версии Go обозначает unknown.
func resolve(embedded string, b *debug.BuildInfo, goVersion string) Info {
	// info накапливает сведения о сборке, начиная с явных значений для неизвестного происхождения.
	info := Info{Version: "devel", Commit: "unknown", Dirty: "unknown", GoVersion: goVersion}
	if b != nil {
		if b.Main.Version != "" && b.Main.Version != "(devel)" {
			info.Version = b.Main.Version
		}
		if b.GoVersion != "" {
			info.GoVersion = b.GoVersion
		}
		// setting содержит очередную настройку сборки для извлечения коммита и признака
		// изменённого дерева.
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

// String формирует единую строку версии, коммита, состояния дерева и версии Go для обоих CLI.
func (i Info) String() string {
	return fmt.Sprintf("%s commit=%s dirty=%s go=%s", i.Version, i.Commit, i.Dirty, i.GoVersion)
}
