package main

import (
	"bytes"
	"io"
	"os/exec"
	"strings"
	"testing"
)

const testCommit = "0123456789abcdef0123456789abcdef01234567"

func TestReleaseSequence(t *testing.T) {
	short := testCommit[:12]
	tests := []struct {
		name, branch, previous, want, increment string
	}{
		{"первый цикл разработки", "develop", "", "v0.1.0-dev.1+" + short, ""},
		{"первый стабильный выпуск", "main", "", "v0.1.0", ""},
		{"счётчик разработки", "develop", "v0.1.0-dev.9+abcdef012345", "v0.1.0-dev.10+" + short, ""},
		{"стабилизация версии", "main", "v0.1.0-dev.9+abcdef012345", "v0.1.0", "minor"},
		{"следующий цикл разработки", "develop", "v0.1.0", "v0.2.0-dev.1+" + short, "minor"},
		{"следующий стабильный выпуск", "main", "v0.1.0", "v0.2.0", "minor"},
		{"следующая стабилизация", "main", "v0.2.0-dev.2+abcdef012345", "v0.2.0", "minor"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			var tags []release
			if tt.previous != "" {
				repo, _ := newRepo(t)
				fixtureGit(t, repo, "tag", tt.previous)
				var err error
				tags, err = readTags(repo)
				if err != nil {
					t.Fatal(err)
				}
			}
			p, err := makePlan(tt.branch, testCommit, short, tags)
			if err != nil || p.version != tt.want || p.releaseType != tt.increment || p.reuse {
				t.Fatalf("план %+v, ошибка %v; ожидались версия %s и повышение %q", p, err, tt.want, tt.increment)
			}
			if (tt.branch == "develop") != (p.postfix == "dev" && p.metadata == short) {
				t.Fatalf("неожиданные параметры предварительного выпуска: %+v", p)
			}
		})
	}
}

func TestPlanUsesHighestSemanticVersion(t *testing.T) {
	tags := []release{
		{minor: 2, dev: 9, name: "v0.2.0-dev.9+abcdef0"},
		{minor: 1, name: "v0.1.0"},
		{minor: 2, dev: 10, name: "v0.2.0-dev.10+abcdef0"},
		{minor: 2, dev: 1, name: "v0.2.0-dev.1+abcdef0"},
	}
	p, err := makePlan("develop", testCommit, testCommit[:12], tags)
	if err != nil || p.version != "v0.2.0-dev.11+"+testCommit[:12] {
		t.Fatalf("план %+v, ошибка %v", p, err)
	}
	tags = append(tags, release{minor: 2, name: "v0.2.0"})
	p, err = makePlan("develop", testCommit, testCommit[:12], tags)
	if err != nil || p.version != "v0.3.0-dev.1+"+testCommit[:12] {
		t.Fatalf("план %+v, ошибка %v", p, err)
	}
}

func TestRerunReusesBranchTagOnExactCommit(t *testing.T) {
	repo, commit := newRepo(t)
	dev := "v0.1.0-dev.1+" + commit[:12]
	fixtureGit(t, repo, "tag", "-a", dev, "-m", dev)
	fixtureGit(t, repo, "tag", "v0.1.0")
	fixtureGit(t, repo, "commit", "--allow-empty", "-m", "next")
	fixtureGit(t, repo, "tag", "v0.2.0")
	fixtureGit(t, repo, "checkout", "--detach", commit)
	for branch, want := range map[string]string{"main": "v0.1.0", "develop": dev} {
		var out bytes.Buffer
		if err := run([]string{"-repo", repo, "-branch", branch, "-commit", commit}, &out); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "version="+want+"\n") || !strings.Contains(out.String(), "reuse=true\n") {
			t.Fatalf("неожиданный результат: %s", out.String())
		}
	}
}

func TestTagValidation(t *testing.T) {
	for _, name := range []string{"v0.1.0-rc.1", "v0.1.0-dev.0+abcdef0", "v01.0.0", "v0.1.0+abcdef0", "v0.1.0-dev.1+fake", "v9007199254740992.0.0"} {
		t.Run(name, func(t *testing.T) {
			repo, _ := newRepo(t)
			fixtureGit(t, repo, "tag", name)
			if _, err := readTags(repo); err == nil {
				t.Fatal("принят неподдерживаемый тег")
			}
		})
	}
	t.Run("метаданные не различают версии", func(t *testing.T) {
		repo, _ := newRepo(t)
		fixtureGit(t, repo, "tag", "v0.1.0-dev.1+abcdef0")
		fixtureGit(t, repo, "tag", "v0.1.0-dev.1+abcdef1")
		if _, err := readTags(repo); err == nil {
			t.Fatal("принята повторная семантическая версия")
		}
	})
	t.Run("посторонние теги", func(t *testing.T) {
		repo, _ := newRepo(t)
		fixtureGit(t, repo, "tag", "backup")
		tags, err := readTags(repo)
		if err != nil || len(tags) != 0 {
			t.Fatalf("теги %+v, ошибка %v", tags, err)
		}
	})
}

func TestInvalidRerunAndOverflow(t *testing.T) {
	for _, tags := range [][]release{
		{{minor: 1, dev: 1, name: "v0.1.0-dev.1+abcdef0", commit: testCommit, metadata: "abcdef0"}},
		{{minor: 1, name: "v0.1.0", commit: testCommit}, {minor: 2, name: "v0.2.0", commit: testCommit}},
		{{minor: maxSafeInteger - 1, name: "v0.9007199254740990.0"}},
		{{minor: 1, dev: maxSafeInteger - 1, name: "v0.1.0-dev.9007199254740990+abcdef0"}},
	} {
		branch := "develop"
		if len(tags) == 2 {
			branch = "main"
		}
		if p, err := makePlan(branch, testCommit, testCommit[:12], tags); err == nil {
			t.Fatalf("принят некорректный план: %+v", p)
		}
	}
}

func TestInvalidCommandInput(t *testing.T) {
	repo, commit := newRepo(t)
	for _, args := range [][]string{
		{}, {"-unknown"}, {"-branch", "feature", "-commit", commit},
		{"-branch", "main", "-commit", commit, "extra"},
		{"-branch", "main", "-commit", "short"},
		{"-repo", repo, "-branch", "main", "-commit", testCommit},
		{"-repo", t.TempDir(), "-branch", "main", "-commit", commit},
	} {
		if err := run(args, io.Discard); err == nil {
			t.Fatalf("приняты некорректные аргументы команды: %q", args)
		}
	}
}

func newRepo(t *testing.T) (string, string) {
	t.Helper()
	repo := t.TempDir()
	fixtureGit(t, repo, "init", "--quiet")
	fixtureGit(t, repo, "commit", "--allow-empty", "-m", "initial")
	return repo, fixtureGit(t, repo, "rev-parse", "HEAD")
}

func fixtureGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/local/bin:/opt/homebrew/bin", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=Release Test", "GIT_AUTHOR_EMAIL=release@example.invalid", "GIT_COMMITTER_NAME=Release Test", "GIT_COMMITTER_EMAIL=release@example.invalid"}
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v: %s: %s", args, err, output, stderr.String())
	}
	return strings.TrimSpace(string(output))
}
