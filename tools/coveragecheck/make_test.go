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

func TestMakeCoverageRequiresSuccessfulFreshRunAndCleansArtifacts(t *testing.T) {
	_, sourcePath, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("не удалось определить путь к тесту")
	}
	root := filepath.Clean(filepath.Join(filepath.Dir(sourcePath), "../.."))
	makefile, err := os.ReadFile(filepath.Join(root, "Makefile"))
	if err != nil {
		t.Fatal(err)
	}
	module, err := os.ReadFile(filepath.Join(root, "go.mod"))
	if err != nil {
		t.Fatal(err)
	}
	makePath, err := exec.LookPath("make")
	if err != nil {
		t.Fatalf("объявленный проектом make недоступен: %v", err)
	}
	for _, tc := range []struct {
		name, scenario string
		pass           bool
		wantReport     bool
		wantChecker    bool
	}{
		{"successful_gate", "success", true, true, true},
		{"failed_tests_leave_profile", "test_failure", false, false, false},
		{"missing_profile", "missing_profile", false, false, false},
		{"empty_profile", "empty_profile", false, false, false},
		{"report_failure", "report_failure", false, true, false},
		{"checker_build_failure", "build_failure", false, true, false},
		{"below_threshold", "checker_failure", false, true, true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			workdir := t.TempDir()
			artifactRoot := filepath.Join(workdir, "temporary files")
			if err := os.Mkdir(artifactRoot, 0o700); err != nil {
				t.Fatal(err)
			}
			writeTestFile(t, filepath.Join(workdir, "Makefile"), string(makefile), 0o600)
			writeTestFile(t, filepath.Join(workdir, "go.mod"), string(module), 0o600)
			writeTestFile(t, filepath.Join(artifactRoot, "coverage.out"), "старый профиль принадлежит другому запуску", 0o600)
			fakeGo := filepath.Join(workdir, "fake-go")
			writeTestFile(t, fakeGo, fakeCoverageGo, 0o700)
			logPath := filepath.Join(workdir, "commands.log")
			profileRecord := filepath.Join(workdir, "profiles.log")
			var previousProfile string
			for attempt := 0; attempt < 2; attempt++ {
				writeTestFile(t, logPath, "", 0o600)
				ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
				cmd := exec.CommandContext(ctx, makePath, "--no-print-directory", "coverage", "GO="+fakeGo)
				cmd.Dir = workdir
				cmd.Env = coverageMakeEnv(artifactRoot, logPath, profileRecord, tc.scenario)
				output, runErr := cmd.CombinedOutput()
				cancel()
				if ctx.Err() == context.DeadlineExceeded {
					t.Fatalf("make превысил ограничение времени: %s", output)
				}
				if (runErr == nil) != tc.pass {
					t.Fatalf("успешность make = %t, ожидается %t: %v\n%s", runErr == nil, tc.pass, runErr, output)
				}
				log := readTestFile(t, logPath)
				if strings.Contains(log, "REPORT\n") != tc.wantReport || strings.Contains(log, "CHECKER\n") != tc.wantChecker {
					t.Errorf("неверная последовательность отчёта/проверки: %q; вывод: %s", log, output)
				}
				if !strings.Contains(log, "TEST\n") {
					t.Fatalf("полный тестовый запуск не выполнен: %q\n%s", log, output)
				}
				profiles := strings.Split(strings.TrimSpace(readTestFile(t, profileRecord)), "\n")
				if len(profiles) != attempt+1 {
					t.Fatalf("ожидается один новый профиль на попытку: %q", profiles)
				}
				profile := profiles[attempt]
				if profile == previousProfile || !strings.HasPrefix(profile, artifactRoot+string(os.PathSeparator)) {
					t.Errorf("профиль не принадлежит отдельному новому каталогу: %q", profile)
				}
				previousProfile = profile
				if _, err := os.Stat(filepath.Dir(profile)); !os.IsNotExist(err) {
					t.Errorf("временный каталог не удалён: %q, ошибка %v", profile, err)
				}
				if stale := readTestFile(t, filepath.Join(artifactRoot, "coverage.out")); stale != "старый профиль принадлежит другому запуску" {
					t.Errorf("изменён посторонний профиль: %q", stale)
				}
			}
		})
	}
}

func coverageMakeEnv(artifactRoot, logPath, profileRecord, scenario string) []string {
	controlled := map[string]string{
		"TMPDIR":                  artifactRoot,
		"COVERAGE_TEST_LOG":       logPath,
		"COVERAGE_TEST_PROFILES":  profileRecord,
		"COVERAGE_TEST_SCENARIO":  scenario,
		"MAKEFLAGS":               "",
		"MFLAGS":                  "",
		"GOFLAGS":                 "-mod=readonly",
		"GOST_MEDIA_FFMPEG":       "/не-запускать/ffmpeg",
		"GOST_FMP4_FFMPEG":        "/не-запускать/ffmpeg",
		"GOST_MEDIA_VIDEOTOOLBOX": "1",
	}
	var env []string
	for _, entry := range os.Environ() {
		key, _, _ := strings.Cut(entry, "=")
		if _, found := controlled[key]; !found {
			env = append(env, entry)
		}
	}
	for key, value := range controlled {
		env = append(env, fmt.Sprintf("%s=%s", key, value))
	}
	return env
}

func readTestFile(t *testing.T, path string) string {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return string(data)
}

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
  printf '%s\n' "$profile" >> "$COVERAGE_TEST_PROFILES"
  printf 'TEST\n' >> "$COVERAGE_TEST_LOG"
  case "$COVERAGE_TEST_SCENARIO" in
    missing_profile) exit 0 ;;
    empty_profile) : > "$profile" ;;
    *) printf 'mode: atomic\na.go:1.1,2.1 1 1\n' > "$profile" ;;
  esac
  if test "$COVERAGE_TEST_SCENARIO" = test_failure; then exit 23; fi
  ;;
tool)
  test "$2" = cover
  test "$#" = 3
  profile=${3#-func=}
  test "$profile" != "$3"
  test -s "$profile"
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
