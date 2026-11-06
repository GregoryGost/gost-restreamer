package main

import (
	"bytes"
	"context"
	"os"
	"os/exec"
	"strings"
	"testing"
	"time"
)

func acceptanceGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	flags := []string{"-C", repo, "-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false", "-c", "user.name=Version Test", "-c", "user.email=version-test@example.invalid"}
	command := exec.CommandContext(ctx, "git", append(flags, args...)...)
	command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("локальная Git-фикстура %q: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

func acceptanceReleaseRepo(t *testing.T) string {
	t.Helper()
	repo := t.TempDir()
	acceptanceGit(t, repo, "init", "--initial-branch=develop")
	acceptanceGit(t, repo, "commit", "--allow-empty", "-m", "Начало разработки")
	return repo
}

func acceptanceReleasePlan(t *testing.T, repo, branch, commit, wantVersion string, wantReuse bool) {
	t.Helper()
	before := acceptanceGit(t, repo, "show-ref", "--head")
	var output bytes.Buffer
	if err := run([]string{"-repo", repo, "-branch", branch, "-commit", commit}, &output); err != nil {
		t.Fatal(err)
	}
	fields := make(map[string]string)
	for line := range strings.SplitSeq(strings.TrimSpace(output.String()), "\n") {
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("некорректный результат планирования: %q", output.String())
		}
		if _, exists := fields[key]; exists {
			t.Fatalf("дублированное поле результата: %q", key)
		}
		fields[key] = value
	}
	wantReuseText := "false"
	if wantReuse {
		wantReuseText = "true"
	}
	if fields["version"] != wantVersion || fields["reuse"] != wantReuseText {
		t.Fatalf("ветка %s, коммит %s: поля %v; ожидалось version=%s reuse=%s", branch, commit, fields, wantVersion, wantReuseText)
	}
	if after := acceptanceGit(t, repo, "show-ref", "--head"); after != before {
		t.Fatal("планирование изменило Git refs")
	}
}

func TestAcceptanceReleaseProgressionAndExactCommitReuse(t *testing.T) {
	repo := acceptanceReleaseRepo(t)
	commit := acceptanceGit(t, repo, "rev-parse", "HEAD")
	short := acceptanceGit(t, repo, "rev-parse", "--short=12", "HEAD")
	first := "v0.1.0-dev.1+" + short
	acceptanceReleasePlan(t, repo, "develop", commit, first, false)
	acceptanceGit(t, repo, "tag", "-a", first, "-m", "Первый тестовый выпуск")
	acceptanceReleasePlan(t, repo, "develop", commit, first, true)

	acceptanceGit(t, repo, "commit", "--allow-empty", "-m", "Следующий merge-коммит")
	commit = acceptanceGit(t, repo, "rev-parse", "HEAD")
	short = acceptanceGit(t, repo, "rev-parse", "--short=12", "HEAD")
	second := "v0.1.0-dev.2+" + short
	acceptanceReleasePlan(t, repo, "develop", commit, second, false)
	acceptanceGit(t, repo, "tag", second)
	acceptanceReleasePlan(t, repo, "develop", commit, second, true)

	acceptanceGit(t, repo, "commit", "--allow-empty", "-m", "Стабильный merge-коммит")
	commit = acceptanceGit(t, repo, "rev-parse", "HEAD")
	acceptanceReleasePlan(t, repo, "main", commit, "v0.1.0", false)
	acceptanceGit(t, repo, "tag", "v0.1.0")
	acceptanceReleasePlan(t, repo, "main", commit, "v0.1.0", true)

	acceptanceGit(t, repo, "commit", "--allow-empty", "-m", "Начало следующего выпуска")
	commit = acceptanceGit(t, repo, "rev-parse", "HEAD")
	short = acceptanceGit(t, repo, "rev-parse", "--short=12", "HEAD")
	next := "v0.2.0-dev.1+" + short
	acceptanceReleasePlan(t, repo, "develop", commit, next, false)
	acceptanceGit(t, repo, "tag", next)
	acceptanceReleasePlan(t, repo, "develop", commit, next, true)
	acceptanceReleasePlan(t, repo, "main", commit, "v0.2.0", false)
}

func TestAcceptanceFirstStableReleaseAndSubsequentMinor(t *testing.T) {
	repo := acceptanceReleaseRepo(t)
	commit := acceptanceGit(t, repo, "rev-parse", "HEAD")
	acceptanceReleasePlan(t, repo, "main", commit, "v0.1.0", false)
	acceptanceGit(t, repo, "tag", "v0.1.0")
	acceptanceGit(t, repo, "commit", "--allow-empty", "-m", "Следующий стабильный выпуск")
	commit = acceptanceGit(t, repo, "rev-parse", "HEAD")
	acceptanceReleasePlan(t, repo, "main", commit, "v0.2.0", false)
}

func TestAcceptanceReleasePlanRejectsAmbiguousOrWrongCommit(t *testing.T) {
	for _, scenario := range []string{"HEAD не совпадает", "чужие метаданные", "два dev-тега", "два стабильных тега"} {
		t.Run(scenario, func(t *testing.T) {
			repo := acceptanceReleaseRepo(t)
			commit := acceptanceGit(t, repo, "rev-parse", "HEAD")
			short := acceptanceGit(t, repo, "rev-parse", "--short=12", "HEAD")
			branch := "develop"
			switch scenario {
			case "HEAD не совпадает":
				acceptanceGit(t, repo, "commit", "--allow-empty", "-m", "Другой коммит")
			case "чужие метаданные":
				wrong := "000000000000"
				if strings.HasPrefix(commit, wrong) {
					wrong = "ffffffffffff"
				}
				acceptanceGit(t, repo, "tag", "v0.1.0-dev.1+"+wrong)
			case "два dev-тега":
				acceptanceGit(t, repo, "tag", "v0.1.0-dev.1+"+short)
				acceptanceGit(t, repo, "tag", "v0.1.0-dev.2+"+short)
			case "два стабильных тега":
				branch = "main"
				acceptanceGit(t, repo, "tag", "v0.1.0")
				acceptanceGit(t, repo, "tag", "v0.2.0")
			}
			var output bytes.Buffer
			if err := run([]string{"-repo", repo, "-branch", branch, "-commit", commit}, &output); err == nil || output.Len() != 0 {
				t.Fatalf("небезопасное планирование прошло: err=%v, output=%q", err, output.String())
			}
		})
	}
}
