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

// Проверяет происхождение версии при локальной, архивной и явно версионированной сборке, включая
// неизвестное состояние дерева.
func TestAcceptanceVersionProvenance(t *testing.T) {
	// revision задаёт фиксированный полный Git-коммит для сценариев происхождения версии.
	const revision = "0123456789abcdef0123456789abcdef01234567"
	// tc задаёт независимые источники версии и ожидаемое происхождение каждого поля результата.
	for _, tc := range []struct {
		// name задаёт название сценария происхождения; embedded содержит внедрённую версию либо
		// пустую строку для выбора из метаданных.
		name, embedded string
		// metadata содержит подставленные метаданные Go либо nil для имитации сборки без них.
		metadata *debug.BuildInfo
		// fallbackGo задаёт запасную версию Go, используемую при отсутствии её в metadata.
		fallbackGo string
		// want содержит ожидаемые итоговые сведения для текущего набора источников.
		want Info
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
			// Проверяет точное происхождение версии, коммита, состояния дерева и версии Go для выбранного случая.

			// got содержит фактическое объединение внедрённой версии, метаданных и запасной
			// версии Go.
			if got := resolve(tc.embedded, tc.metadata, tc.fallbackGo); got != tc.want {
				t.Fatalf("сведения о сборке: получено %+v, ожидалось %+v", got, tc.want)
			}
		})
	}
	// info задаёт фиксированные сведения для проверки точных разделителей строки версии.
	info := Info{"v0.1.0", revision, "false", "go1.27.3"}
	// got содержит сформированную строку версии; want задаёт её точное ожидаемое представление с
	// revision.
	if got, want := info.String(), "v0.1.0 commit="+revision+" dirty=false go=go1.27.3"; got != want {
		t.Fatalf("строка версии: %q, ожидалось %q", got, want)
	}
}

// Собирает оба настоящих бинарника с явной версией и без неё и сверяет их вывод со встроенными
// метаданными и Git HEAD.
func TestAcceptanceNativeBinariesMatchEmbeddedProvenance(t *testing.T) {
	// ctx ограничивает весь сценарий нативных сборок 90 секундами; cancel освобождает таймер после
	// теста.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	// gitHead читает текущий HEAD для сравнения с VCS-метаданными собранных бинарников.
	gitHead := exec.CommandContext(ctx, "git", "rev-parse", "HEAD")
	// revision содержит stdout Git с текущим HEAD; gitErr содержит отказ его чтения, при котором
	// сравнение коммита пропускается.
	revision, gitErr := gitHead.Output()
	// embedded задаёт очередной вариант сборки: автоматическое происхождение или явно внедрённую
	// версию.
	for _, embedded := range []string{"", "v0.2.0-dev.4+0123456"} {
		// first сохраняет вывод первого бинарника для сравнения единства сведений обоих CLI.
		var first string
		// name выбирает очередной настоящий бинарник сервера или helper для сборки и запуска
		// --version.
		for _, name := range []string{"gost-restreamer", "gost-process-helper"} {
			// binary указывает изолированный путь выходного бинарника текущего варианта.
			binary := filepath.Join(t.TempDir(), name)
			// args содержит аргументы Go-сборки, дополняемые -ldflags при явной версии.
			args := []string{"build", "-trimpath", "-o", binary}
			if embedded != "" {
				args = append(args, "-ldflags", "-X github.com/GregoryGost/gost-restreamer/internal/buildinfo.version="+embedded)
			}
			// build запускает нативную сборку текущего cmd-пакета с закреплённым локальным
			// окружением.
			build := exec.CommandContext(ctx, "go", append(args, "../../cmd/"+name)...)
			build.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOWORK=off", "GOFLAGS=-mod=readonly", "GOPROXY=off")
			// output сохраняет диагностику сборки для сообщения об отказе; err содержит
			// результат выполнения go build.
			if output, err := build.CombinedOutput(); err != nil {
				t.Fatalf("нативная сборка %s: %v\n%s", name, err, output)
			}
			// metadata содержит прочитанные из готового бинарника сведения Go; err содержит
			// отказ их чтения.
			metadata, err := executableinfo.ReadFile(binary)
			if err != nil {
				t.Fatal(err)
			}
			// settings индексирует настройки готового бинарника по ключу для независимого
			// вычисления ожидаемого вывода.
			settings := make(map[string]string)
			// setting содержит очередную пару метаданных, помещаемую в индекс settings.
			for _, setting := range metadata.Settings {
				settings[setting.Key] = setting.Value
			}
			if gitErr == nil && settings["vcs.revision"] != strings.TrimSpace(string(revision)) {
				t.Fatalf("нативная сборка потеряла фактический Git-коммит: %v", settings)
			}
			// wantCommit задаёт ожидаемый коммит либо unknown; wantDirty задаёт допустимый
			// признак изменённого дерева либо unknown.
			wantCommit, wantDirty := "unknown", "unknown"
			// value содержит vcs.revision готового бинарника для замены ожидаемого unknown.
			if value := settings["vcs.revision"]; value != "" {
				wantCommit = value
			}
			// value содержит vcs.modified готового бинарника для принятия только true или
			// false.
			if value := settings["vcs.modified"]; value == "true" || value == "false" {
				wantDirty = value
			}
			// wantVersion вычисляет ожидаемую версию по внедрённому значению, версии модуля
			// и правилу devel.
			wantVersion := embedded
			if wantVersion == "" {
				wantVersion = metadata.Main.Version
				if wantVersion == "" || wantVersion == "(devel)" {
					wantVersion = "devel"
				}
			}
			// want содержит полную ожидаемую строку --version с переводом строки для
			// собранного бинарника.
			want := fmt.Sprintf("%s commit=%s dirty=%s go=%s\n", wantVersion, wantCommit, wantDirty, metadata.GoVersion)
			// command запускает собранный бинарник с ранним аргументом --version.
			command := exec.CommandContext(ctx, binary, "--version")
			// stdout сохраняет строку версии собранного бинарника; stderr перехватывает
			// нежелательную диагностику.
			var stdout, stderr bytes.Buffer
			command.Stdout, command.Stderr = &stdout, &stderr
			// err содержит отказ запуска собранного бинарника для вывода версии.
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

// Собирает оба бинарника без VCS-метаданных и проверяет ранний вывод явной версии из отдельного
// каталога без создания рабочих файлов.
func TestAcceptanceBothBinariesVersionWithoutVCS(t *testing.T) {
	// embedded задаёт явную версию для сборок, в которых VCS-метаданные намеренно отключены.
	const embedded = "v0.2.0-dev.3+0123456"
	// want задаёт ожидаемую строку версии с unknown для коммита и состояния дерева.
	want := embedded + " commit=unknown dirty=unknown go=" + runtime.Version() + "\n"
	// name выбирает сервер или helper для проверки --version без VCS и рабочих файлов.
	for _, name := range []string{"gost-restreamer", "gost-process-helper"} {
		t.Run(name, func(t *testing.T) {
			// Собирает выбранный бинарник без VCS-метаданных и проверяет точный вывод --version без создания
			// рабочих файлов.

			// dir выделяет каталог единственного бинарника, где после --version не должны
			// появиться рабочие данные.
			dir := t.TempDir()
			// binary указывает путь текущего бинарника внутри проверяемого каталога.
			binary := filepath.Join(dir, name)
			// ctx ограничивает сборку и запуск выбранного бинарника 90 секундами; cancel
			// освобождает таймер.
			ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
			defer cancel()
			// build собирает текущий cmd-пакет с явной версией и выключенными
			// VCS-метаданными.
			build := exec.CommandContext(ctx, "go", "build", "-buildvcs=false", "-trimpath", "-ldflags", "-X github.com/GregoryGost/gost-restreamer/internal/buildinfo.version="+embedded, "-o", binary, "../../cmd/"+name)
			build.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOWORK=off", "GOFLAGS=-mod=readonly", "GOPROXY=off")
			// output сохраняет диагностику сборки без VCS; err содержит результат выполнения
			// go build.
			if output, err := build.CombinedOutput(); err != nil {
				t.Fatalf("сборка %s: %v\n%s", name, err, output)
			}
			// command запускает --version с рабочим каталогом, в котором проверяется
			// отсутствие побочных файлов.
			command := exec.CommandContext(ctx, binary, "--version")
			command.Dir = dir
			// stdout сохраняет строку версии; stderr перехватывает нежелательную диагностику
			// раннего выхода.
			var stdout, stderr bytes.Buffer
			command.Stdout, command.Stderr = &stdout, &stderr
			// err содержит результат запуска --version у бинарника без VCS-метаданных.
			if err := command.Run(); err != nil || stdout.String() != want || stderr.Len() != 0 {
				t.Fatalf("--version: err=%v, stdout=%q, stderr=%q; ожидалось %q", err, stdout.String(), stderr.String(), want)
			}
			// entries содержит объекты каталога после --version; err содержит отказ чтения
			// для проверки отсутствия созданных рабочих файлов.
			entries, err := os.ReadDir(dir)
			if err != nil || len(entries) != 1 || entries[0].Name() != name {
				t.Fatalf("--version создал рабочие файлы: entries=%v, err=%v", entries, err)
			}
		})
	}
}
