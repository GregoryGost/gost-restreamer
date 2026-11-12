package main

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"
)

// Проверяет настоящий рецепт make coverage с управляемым Go: свежий полный запуск, последовательность
// преобразования и проверки, остановку при каждой ошибке и удаление только собственных артефактов.
func TestMakeCoverageRequiresSuccessfulFreshRunAndCleansArtifacts(t *testing.T) {
	// sourcePath содержит путь текущего тестового файла; ok подтверждает его получение для поиска
	// корня проекта.
	_, sourcePath, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("не удалось определить путь к тесту")
	}
	// root указывает корень проекта, из которого читается настоящий рецепт покрытия.
	root := filepath.Clean(filepath.Join(filepath.Dir(sourcePath), "../.."))
	// makefile содержит действующий Makefile; err содержит отказ чтения рецепта для изолированной
	// проверки.
	makefile, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	// module содержит действующий go.mod, необходимый проверке версии Go в копии рецепта.
	module, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	// makePath содержит найденный исполняемый make для запуска настоящей цели coverage.
	makePath, err := exec.LookPath("make")
	if err != nil {
		t.Fatalf("объявленный проектом make недоступен: %v", err)
	}
	// tc задаёт точку отказа рецепта и ожидаемые выполненные стадии проверки покрытия.
	for _, tc := range []struct {
		// name задаёт название сценария; scenario выбирает поведение подставленного Go.
		name, scenario string
		// pass задаёт ожидаемый успешный или ошибочный код make coverage.
		pass bool
		// wantConversion показывает, должен ли рецепт дойти до преобразования покрытия helper.
		wantConversion bool
		// wantReport показывает, должен ли рецепт дойти до отчёта go tool cover.
		wantReport bool
		// wantChecker показывает, должен ли рецепт запустить точную проверку порога.
		wantChecker bool
	}{
		{"successful_gate", "success", true, true, true, true},
		{"failed_tests_leave_profile", "test_failure", false, false, false, false},
		{"missing_profile", "missing_profile", false, false, false, false},
		{"empty_profile", "empty_profile", false, false, false, false},
		{"header_only_profile", "header_only_profile", false, false, false, false},
		{"bad_parent_header", "bad_parent_header", false, false, false, false},
		{"conversion_failure", "conversion_failure", false, true, false, false},
		{"missing_helper_profile", "missing_helper_profile", false, true, false, false},
		{"empty_helper_profile", "empty_helper_profile", false, true, false, false},
		{"header_only_helper_profile", "header_only_helper_profile", false, true, false, false},
		{"bad_helper_header", "bad_helper_header", false, true, false, false},
		{"report_failure", "report_failure", false, true, true, false},
		{"checker_build_failure", "build_failure", false, true, true, false},
		{"below_threshold", "checker_failure", false, true, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Дважды проверяет выбранный исход make coverage, свежесть профиля, порядок этапов и удаление только
			// собственных артефактов.

			// workdir выделяет изолированный проект с копиями настоящих Makefile и go.mod.
			workdir := t.TempDir()
			// artifactRoot содержит отдельный временный корень с пробелом в пути для
			// проверки размещения и очистки артефактов.
			artifactRoot := filepath.Join(workdir, "temporary files")
			// err содержит отказ создания изолированного корня временных профилей.
			if err := os.Mkdir(artifactRoot, 0o700); err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, filepath.Join(workdir, "Makefile"), string(makefile), 0o600)
			writeTestFile(t, filepath.Join(workdir, "go.mod"), string(module), 0o600)
			writeTestFile(t, filepath.Join(artifactRoot, "coverage.out"), "старый профиль принадлежит другому запуску", 0o600)
			// fakeGo указывает управляемую замену Go, проверяющую аргументы и задающую
			// выбранный отказ.
			fakeGo := filepath.Join(workdir, "fake-go")
			writeTestFile(t, fakeGo, fakeCoverageGo, 0o700)
			// logPath указывает журнал реально выполненных стадий рецепта.
			logPath := filepath.Join(workdir, "commands.log")
			// profileRecord указывает журнал путей профилей для проверки нового каталога
			// каждого запуска.
			profileRecord := filepath.Join(workdir, "profiles.log")
			// previousProfile сохраняет путь предыдущего запуска, который следующий запуск
			// не должен переиспользовать.
			var previousProfile string
			// attempt указывает первый или второй запуск одного сценария для проверки
			// свежести артефактов.
			for attempt := range 2 {
				writeTestFile(t, logPath, "", 0o600)
				// ctx ограничивает отдельный make-запуск 20 секундами; cancel
				// освобождает срок сразу после его возврата.
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				// cmd запускает настоящий рецепт coverage с подставленным Go в
				// изолированной копии проекта.
				cmd := exec.CommandContext(ctx, makePath, "--no-print-directory", "coverage", "GO="+fakeGo)
				cmd.Dir = workdir
				cmd.Env = coverageMakeEnv(artifactRoot, logPath, profileRecord, tc.scenario)
				// output содержит объединённую диагностику make; runErr содержит
				// результат выполнения рецепта.
				output, runErr := cmd.CombinedOutput()
				cancel()
				if ctx.Err() == context.DeadlineExceeded {
					t.Fatalf("make превысил ограничение времени: %s", output)
				}
				if (runErr == nil) != tc.pass {
					t.Fatalf("успешность make = %t, ожидается %t: %v\n%s", runErr == nil, tc.pass, runErr, output)
				}
				// log содержит наблюдённую последовательность стадий из управляемого Go.
				log := readTestFile(t, logPath)
				// wantLog строит точную ожидаемую последовательность до выбранной точки
				// отказа.
				wantLog := "TEST\n"
				if tc.wantConversion {
					wantLog += "CONVERT\n"
				}
				if tc.wantReport {
					wantLog += "REPORT\n"
				}
				if tc.wantChecker {
					wantLog += "CHECKER\n"
				}
				if log != wantLog {
					t.Errorf("неверная последовательность конвертации/отчёта/проверки: %q; вывод: %s", log, output)
				}
				if !strings.Contains(log, "TEST\n") {
					t.Fatalf("полный тестовый запуск не выполнен: %q\n%s", log, output)
				}
				// profiles содержит все записанные пути профилей для проверки одного
				// нового пути на попытку.
				profiles := strings.Split(strings.TrimSpace(readTestFile(t, profileRecord)), "\n")
				if len(profiles) != attempt+1 {
					t.Fatalf("ожидается один новый профиль на попытку: %q", profiles)
				}
				// profile содержит путь профиля текущей попытки для проверки
				// принадлежности и удаления его каталога.
				profile := profiles[attempt]
				if profile == previousProfile || !strings.HasPrefix(profile, artifactRoot+string(os.PathSeparator)) {
					t.Errorf("профиль не принадлежит отдельному новому каталогу: %q", profile)
				}
				previousProfile = profile
				// err содержит результат проверки существования каталога текущего
				// профиля после очистки рецептом.
				if _, err := os.Stat(filepath.Dir(profile)); !os.IsNotExist(err) {
					t.Errorf("временный каталог не удалён: %q, ошибка %v", profile, err)
				}
				// stale содержит прежний чужой профиль, который очистка текущего запуска
				// обязана сохранить.
				if stale := readTestFile(t, filepath.Join(artifactRoot, "coverage.out")); stale != "старый профиль принадлежит другому запуску" {
					t.Errorf("изменён посторонний профиль: %q", stale)
				}
			}
		})
	}
}

// coverageMakeEnv изолирует переменные проверки покрытия и намеренно задаёт прежние медианастройки,
// чтобы проверить их очистку рецептом обязательного запуска.
func coverageMakeEnv(artifactRoot, logPath, profileRecord, scenario string) []string {
	// controlled задаёт изолированные переменные make и покрытия, включая намеренно прежние
	// медианастройки.
	controlled := map[string]string{
		"TMPDIR":                            artifactRoot,
		"COVERAGE_TEST_LOG":                 logPath,
		"COVERAGE_TEST_PROFILES":            profileRecord,
		"COVERAGE_TEST_SCENARIO":            scenario,
		"MAKEFLAGS":                         "",
		"MFLAGS":                            "",
		"GOFLAGS":                           "-mod=readonly",
		"GOST_MEDIA_FFMPEG":                 "/не-запускать/ffmpeg",
		"GOST_FMP4_FFMPEG":                  "/не-запускать/ffmpeg",
		"GOST_MEDIA_VIDEOTOOLBOX":           "1",
		"GOST_TEST_PROCESS_HELPER_COVERDIR": "/не-использовать/старое-покрытие-helper",
	}
	// env собирает окружение дочернего make без дублирования управляемых переменных.
	var env []string
	// entry содержит очередную запись исходного окружения, проверяемую перед копированием.
	for _, entry := range os.Environ() {
		// key содержит имя исходной переменной окружения без её значения.
		key, _, _ := strings.Cut(entry, "=")
		// found отмечает управляемую переменную, которую следует заменить вместо копирования.
		if _, found := controlled[key]; !found {
			env = append(env, entry)
		}
	}
	// key задаёт имя управляемой переменной; value содержит её изолированное значение для дочернего
	// make.
	for key, value := range controlled {
		env = append(env, fmt.Sprintf("%s=%s", key, value))
	}
	return env
}

// readTestFile читает текст тестового артефакта и завершает сценарий при ошибке.
func readTestFile(t *testing.T, path string) string {
	t.Helper()
	// data содержит текст тестового артефакта; err содержит отказ чтения для проверки результата
	// рецепта.
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

// fakeCoverageGo имитирует закреплённые команды Go, проверяет полный запуск покрытия и порядок
// объединения helper-профиля, создавая выбранные ошибки и журнал стадий.
const fakeCoverageGo = `#!/bin/sh
set -eu
test "$GOTOOLCHAIN" = local
test "$GOWORK" = off
test "$CGO_ENABLED" = 1
test "$GOFLAGS" = -mod=readonly
case "$1" in
version) printf 'go version go%s\n' "$(sed -n 's/^go //p' go.mod)" ;;
env) test "$2" = GOVERSION; printf 'go%s\n' "$(sed -n 's/^go //p' go.mod)" ;;
list) test "$2" = ./...; printf 'example.test/module\n' ;;
test)
  test -z "${GOST_MEDIA_FFMPEG:-}"
  test -z "${GOST_FMP4_FFMPEG:-}"
  test -z "${GOST_MEDIA_VIDEOTOOLBOX:-}"
  shift
  count=no; mode=no; packages=no; scope=no; profile=
  for argument do
    case "$argument" in
      -count=1) count=yes ;;
      -covermode=atomic) mode=yes ;;
      -coverpkg=./...) packages=yes ;;
      -coverprofile=*) profile=${argument#-coverprofile=} ;;
      ./...) scope=yes ;;
      *) printf 'Неожиданный аргумент тестов: %s\n' "$argument" >&2; exit 91 ;;
    esac
  done
  test "$count:$mode:$packages:$scope" = yes:yes:yes:yes
  test -n "$profile"
  test ! -e "$profile"
  test "$GOST_TEST_PROCESS_HELPER_COVERDIR" = "$(dirname "$profile")/helpers"
  test -d "$GOST_TEST_PROCESS_HELPER_COVERDIR"
  for entry in "$GOST_TEST_PROCESS_HELPER_COVERDIR"/*; do
    test ! -e "$entry"
  done
  : > "$GOST_TEST_PROCESS_HELPER_COVERDIR/covmeta.synthetic"
  : > "$GOST_TEST_PROCESS_HELPER_COVERDIR/covcounters.synthetic"
  printf '%s\n' "$profile" >> "$COVERAGE_TEST_PROFILES"
  printf 'TEST\n' >> "$COVERAGE_TEST_LOG"
  case "$COVERAGE_TEST_SCENARIO" in
    missing_profile) exit 0 ;;
    empty_profile) : > "$profile" ;;
    header_only_profile) printf 'mode: atomic\n' > "$profile" ;;
    bad_parent_header) printf 'mode: count\na.go:1.1,2.1 1 0\n' > "$profile" ;;
    *) printf 'mode: atomic\na.go:1.1,2.1 1 0\n' > "$profile" ;;
  esac
  if test "$COVERAGE_TEST_SCENARIO" = test_failure; then exit 23; fi
  ;;
tool)
  if test "$2" = covdata; then
    test "$#" = 6
    test "$3" = textfmt
    test "$4" = -hw
    helper_dir=${5#-i=}
    helper_profile=${6#-o=}
    test "$helper_dir" != "$5"
    test "$helper_profile" != "$6"
    test "$helper_profile" = "$(dirname "$helper_dir")/helpers.out"
    test ! -e "$helper_profile"
    test -f "$helper_dir/covmeta.synthetic"
    test -f "$helper_dir/covcounters.synthetic"
    printf 'CONVERT\n' >> "$COVERAGE_TEST_LOG"
    case "$COVERAGE_TEST_SCENARIO" in
      conversion_failure) exit 26 ;;
      missing_helper_profile) exit 0 ;;
      empty_helper_profile) : > "$helper_profile" ;;
      header_only_helper_profile) printf 'mode: atomic\n' > "$helper_profile" ;;
      bad_helper_header) printf 'mode: count\na.go:1.1,2.1 1 1\n' > "$helper_profile" ;;
      *) printf 'mode: atomic\na.go:1.1,2.1 1 1\n' > "$helper_profile" ;;
    esac
    exit 0
  fi
  test "$2" = cover
  test "$#" = 3
  profile=${3#-func=}
  test "$profile" != "$3"
  test -s "$profile"
  test "$(cat "$profile")" = "$(printf 'mode: atomic\na.go:1.1,2.1 1 0\na.go:1.1,2.1 1 1\n')"
  printf 'REPORT\n' >> "$COVERAGE_TEST_LOG"
  if test "$COVERAGE_TEST_SCENARIO" = report_failure; then exit 24; fi
  ;;
build)
  test "$2" = -o
  test "$4" = ./tools/coveragecheck
  test "$#" = 4
  if test "$COVERAGE_TEST_SCENARIO" = build_failure; then exit 25; fi
  cat > "$3" <<'CHECKER'
#!/bin/sh
set -eu
test "$#" = 4
test "$1" = -profile
test -s "$2"
test "$3" = -min
test "$4" = 85
printf 'CHECKER\n' >> "$COVERAGE_TEST_LOG"
if test "$COVERAGE_TEST_SCENARIO" = checker_failure; then exit 1; fi
CHECKER
  chmod 700 "$3"
  ;;
*) printf 'Неожиданная команда: %s\n' "$1" >&2; exit 92 ;;
esac
`
