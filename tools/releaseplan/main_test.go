package main

import (
	"bytes"
	"io"
	"os/exec"
	"strings"
	"testing"
)

// testCommit задаёт фиксированный полный SHA для независимых сценариев вычисления плана.
const testCommit = "0123456789abcdef0123456789abcdef01234567"

// Проверяет первый выпуск, увеличение dev-счётчика, стабилизацию и следующий минорный цикл вместе с
// параметрами version-tagger.
func TestReleaseSequence(t *testing.T) {
	// short содержит 12-символьный SHA, ожидаемый в метаданных нового dev-выпуска.
	short := testCommit[:12]
	// tests задаёт последовательности предыдущего тега, ветки и ожидаемых параметров version-tagger.
	tests := []struct {
		// name задаёт название перехода; branch выбирает канал; previous задаёт исходный тег;
		// want задаёт итоговую версию; increment задаёт ожидаемое повышение.
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
	// tt содержит текущий сценарий перехода между версиями.
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Проверяет версию, вид повышения и параметры предварительного выпуска для выбранного состояния ветки
			// и тегов.

			// tags содержит поддержанные теги текущего сценария либо пустой список для
			// первого выпуска.
			var tags []release
			if tt.previous != "" {
				// repo указывает изолированный репозиторий для разбора предыдущего тега.
				repo, _ := newRepo(t)
				fixtureGit(t, repo, "tag", tt.previous)
				// err принимает отказ чтения тегов подготовленного тестового
				// репозитория.
				var err error
				tags, err = readTags(repo)
				if err != nil {
					t.Fatal(err)
				}
			}
			// p содержит вычисленный план перехода; err содержит отказ его построения.
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

// Проверяет выбор максимальной семантической версии независимо от порядка тегов и переход к следующему
// циклу после стабильного выпуска.
func TestPlanUsesHighestSemanticVersion(t *testing.T) {
	// tags задаёт намеренно неупорядоченные версии для проверки выбора семантического максимума.
	tags := []release{
		{minor: 2, dev: 9, name: "v0.2.0-dev.9+abcdef0"},
		{minor: 1, name: "v0.1.0"},
		{minor: 2, dev: 10, name: "v0.2.0-dev.10+abcdef0"},
		{minor: 2, dev: 1, name: "v0.2.0-dev.1+abcdef0"},
	}
	// p содержит план от максимального тега; err содержит отказ вычисления следующей версии.
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

// Проверяет повторное использование тегов разработки и стабильных тегов точного коммита после
// появления более новых тегов на других коммитах.
func TestRerunReusesBranchTagOnExactCommit(t *testing.T) {
	// repo содержит путь изолированного репозитория; commit содержит точный коммит, чьи теги должны
	// переиспользоваться.
	repo, commit := newRepo(t)
	// dev содержит dev-тег исходного коммита, сохраняемый при появлении новых тегов на других
	// коммитах.
	dev := "v0.1.0-dev.1+" + commit[:12]
	fixtureGit(t, repo, "tag", "-a", dev, "-m", dev)
	fixtureGit(t, repo, "tag", "v0.1.0")
	fixtureGit(t, repo, "commit", "--allow-empty", "-m", "next")
	fixtureGit(t, repo, "tag", "v0.2.0")
	fixtureGit(t, repo, "checkout", "--detach", commit)
	// branch задаёт проверяемый канал; want задаёт тег точного коммита, который должен быть
	// переиспользован.
	for branch, want := range map[string]string{"main": "v0.1.0", "develop": dev} {
		// out сохраняет поля вывода планировщика для проверки версии и reuse=true.
		var out bytes.Buffer
		// err содержит отказ планирования повторного запуска на точном коммите.
		if err := run([]string{"-repo", repo, "-branch", branch, "-commit", commit}, &out); err != nil {
			t.Fatal(err)
		}
		if !strings.Contains(out.String(), "version="+want+"\n") || !strings.Contains(out.String(), "reuse=true\n") {
			t.Fatalf("неожиданный результат: %s", out.String())
		}
	}
}

// Проверяет отклонение неподдерживаемых тегов и повторной семантической версии при разных метаданных,
// сохраняя допустимость посторонних неверсионных тегов.
func TestTagValidation(t *testing.T) {
	// name задаёт очередной неподдержанный тег, который обязан остановить чтение версий.
	for _, name := range []string{"v0.1.0-rc.1", "v0.1.0-dev.0+abcdef0", "v01.0.0", "v0.1.0+abcdef0", "v0.1.0-dev.1+fake", "v9007199254740992.0.0"} {
		t.Run(name, func(t *testing.T) {
			// Проверяет отказ выбранному неподдерживаемому тегу версии.

			// repo указывает изолированный репозиторий с одним неподдержанным тегом.
			repo, _ := newRepo(t)
			fixtureGit(t, repo, "tag", name)
			// err содержит результат чтения неподдержанного тега, который должен быть
			// ошибкой.
			if _, err := readTags(repo); err == nil {
				t.Fatal("принят неподдерживаемый тег")
			}
		})
	}
	t.Run("метаданные не различают версии", func(t *testing.T) {
		// Проверяет отклонение тегов с одинаковой семантической версией и разными метаданными.

		// repo указывает репозиторий с двумя тегами одной семантической версии и разными
		// метаданными.
		repo, _ := newRepo(t)
		fixtureGit(t, repo, "tag", "v0.1.0-dev.1+abcdef0")
		fixtureGit(t, repo, "tag", "v0.1.0-dev.1+abcdef1")
		// err содержит результат чтения семантических дублей, который должен быть ошибкой.
		if _, err := readTags(repo); err == nil {
			t.Fatal("принята повторная семантическая версия")
		}
	})
	t.Run("посторонние теги", func(t *testing.T) {
		// Проверяет игнорирование тега, не обозначающего версию выпуска.

		// repo указывает репозиторий с посторонним неверсионным тегом.
		repo, _ := newRepo(t)
		fixtureGit(t, repo, "tag", "backup")
		// tags содержит принятые версии, список должен быть пустым; err содержит отказ
		// перечисления тегов.
		tags, err := readTags(repo)
		if err != nil || len(tags) != 0 {
			t.Fatalf("теги %+v, ошибка %v", tags, err)
		}
	})
}

// Проверяет отказ при несовпадающих метаданных версии разработки, нескольких тегах одного канала и
// исчерпании диапазона версии.
func TestInvalidRerunAndOverflow(t *testing.T) {
	// tags задаёт текущий набор с неверными метаданными, неоднозначными тегами или исчерпанным
	// числовым диапазоном.
	for _, tags := range [][]release{
		{{minor: 1, dev: 1, name: "v0.1.0-dev.1+abcdef0", commit: testCommit, metadata: "abcdef0"}},
		{{minor: 1, name: "v0.1.0", commit: testCommit}, {minor: 2, name: "v0.2.0", commit: testCommit}},
		{{minor: maxSafeInteger - 1, name: "v0.9007199254740990.0"}},
		{{minor: 1, dev: maxSafeInteger - 1, name: "v0.1.0-dev.9007199254740990+abcdef0"}},
	} {
		// branch выбирает develop либо main для достижения проверяемого отказа текущего набора
		// тегов.
		branch := "develop"
		if len(tags) == 2 {
			branch = "main"
		}
		// p содержит ошибочно возможный план для диагностики; err содержит ожидаемый отказ
		// проверки тегов или диапазона.
		if p, err := makePlan(branch, testCommit, testCommit[:12], tags); err == nil {
			t.Fatalf("принят некорректный план: %+v", p)
		}
	}
}

// Проверяет недопустимые параметры, несовпадение HEAD и недоступный Git-репозиторий без успешного
// плана.
func TestInvalidCommandInput(t *testing.T) {
	// repo указывает изолированный репозиторий; commit содержит его действительный HEAD для
	// сценариев недопустимого CLI.
	repo, commit := newRepo(t)
	// args задаёт очередной неверный набор параметров, коммит или недоступный репозиторий.
	for _, args := range [][]string{
		{}, {"-unknown"}, {"-branch", "feature", "-commit", commit},
		{"-branch", "main", "-commit", commit, "extra"},
		{"-branch", "main", "-commit", "short"},
		{"-repo", repo, "-branch", "main", "-commit", testCommit},
		{"-repo", t.TempDir(), "-branch", "main", "-commit", commit},
	} {
		// err содержит результат CLI с недопустимыми входами, который должен быть ошибкой.
		if err := run(args, io.Discard); err == nil {
			t.Fatalf("приняты некорректные аргументы команды: %q", args)
		}
	}
}

// newRepo создаёт временный Git-репозиторий с начальным пустым коммитом и возвращает его путь и полный
// SHA.
func newRepo(t *testing.T) (string, string) {
	t.Helper()
	// repo выделяет временный Git-репозиторий с синтетическим начальным коммитом.
	repo := t.TempDir()
	fixtureGit(t, repo, "init", "--quiet")
	fixtureGit(t, repo, "commit", "--allow-empty", "-m", "initial")
	return repo, fixtureGit(t, repo, "rev-parse", "HEAD")
}

// fixtureGit запускает Git в тестовом репозитории с изолированными глобальными настройками и
// синтетическими автором и коммитером.
func fixtureGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	// cmd запускает Git-команду только в переданном тестовом репозитории.
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	cmd.Env = []string{"PATH=/usr/bin:/bin:/usr/local/bin:/opt/homebrew/bin", "GIT_CONFIG_NOSYSTEM=1", "GIT_CONFIG_GLOBAL=/dev/null",
		"GIT_AUTHOR_NAME=Release Test", "GIT_AUTHOR_EMAIL=release@example.invalid", "GIT_COMMITTER_NAME=Release Test", "GIT_COMMITTER_EMAIL=release@example.invalid"}
	// stderr перехватывает диагностику Git для объяснения отказа фикстуры.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	// output содержит stdout Git-фикстуры; err содержит отказ выполнения команды.
	output, err := cmd.Output()
	if err != nil {
		t.Fatalf("git %v: %v: %s: %s", args, err, output, stderr.String())
	}
	return strings.TrimSpace(string(output))
}
