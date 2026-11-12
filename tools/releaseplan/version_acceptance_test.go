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

// acceptanceGit выполняет ограниченную по времени команду в локальной Git-фикстуре без
// пользовательских обработчиков Git и подписания.
func acceptanceGit(t *testing.T, repo string, args ...string) string {
	t.Helper()
	// ctx ограничивает отдельную Git-команду фикстуры 10 секундами; cancel освобождает таймер после
	// её завершения.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// flags задаёт репозиторий и локальные настройки отключения обработчиков Git и подписания с
	// синтетическим автором.
	flags := []string{"-C", repo, "-c", "core.hooksPath=/dev/null", "-c", "commit.gpgsign=false", "-c", "tag.gpgsign=false", "-c", "user.name=Version Test", "-c", "user.email=version-test@example.invalid"}
	// command выполняет текущую Git-операцию изолированной фикстуры с конечным сроком.
	command := exec.CommandContext(ctx, "git", append(flags, args...)...)
	command.Env = append(os.Environ(), "GIT_CONFIG_GLOBAL=/dev/null", "GIT_CONFIG_NOSYSTEM=1", "GIT_TERMINAL_PROMPT=0")
	// output содержит объединённый вывод Git-фикстуры; err содержит отказ выполнения команды.
	output, err := command.CombinedOutput()
	if err != nil {
		t.Fatalf("локальная Git-фикстура %q: %v\n%s", args, err, output)
	}
	return strings.TrimSpace(string(output))
}

// acceptanceReleaseRepo создаёт временную ветку develop и начальный коммит для проверки
// последовательности выпусков.
func acceptanceReleaseRepo(t *testing.T) string {
	t.Helper()
	// repo выделяет временный репозиторий с веткой develop и начальным пустым коммитом.
	repo := t.TempDir()
	acceptanceGit(t, repo, "init", "--initial-branch=develop")
	acceptanceGit(t, repo, "commit", "--allow-empty", "-m", "Начало разработки")
	return repo
}

// acceptanceReleasePlan сверяет версию и признак повторного использования, проверяя неизменность
// ссылок Git после планирования.
func acceptanceReleasePlan(t *testing.T, repo, branch, commit, wantVersion string, wantReuse bool) {
	t.Helper()
	// before сохраняет ссылки и HEAD до планирования для проверки отсутствия Git-мутаций.
	before := acceptanceGit(t, repo, "show-ref", "--head")
	// output сохраняет поля плана версии для независимого разбора и проверки.
	var output bytes.Buffer
	// err содержит отказ планировщика для ожидаемо допустимого коммита.
	if err := run([]string{"-repo", repo, "-branch", branch, "-commit", commit}, &output); err != nil {
		t.Fatal(err)
	}
	// fields собирает выведенные пары параметров плана с запретом повторных ключей.
	fields := make(map[string]string)
	// line содержит очередную строку результата планирования версии.
	for line := range strings.SplitSeq(strings.TrimSpace(output.String()), "\n") {
		// key содержит имя поля плана; value содержит его строковое значение; ok подтверждает
		// наличие разделителя равенства.
		key, value, ok := strings.Cut(line, "=")
		if !ok {
			t.Fatalf("некорректный результат планирования: %q", output.String())
		}
		// exists отмечает уже встреченный ключ результата для отклонения повторного поля.
		if _, exists := fields[key]; exists {
			t.Fatalf("дублированное поле результата: %q", key)
		}
		fields[key] = value
	}
	// wantReuseText содержит ожидаемое строковое значение булева поля reuse.
	wantReuseText := "false"
	if wantReuse {
		wantReuseText = "true"
	}
	if fields["version"] != wantVersion || fields["reuse"] != wantReuseText {
		t.Fatalf("ветка %s, коммит %s: поля %v; ожидалось version=%s reuse=%s", branch, commit, fields, wantVersion, wantReuseText)
	}
	// after содержит ссылки и HEAD после планирования для сравнения с before.
	if after := acceptanceGit(t, repo, "show-ref", "--head"); after != before {
		t.Fatal("планирование изменило Git refs")
	}
}

// Проверяет последовательность двух выпусков разработки, стабильной версии и следующего минорного
// цикла, включая повторное использование точных тегов.
func TestAcceptanceReleaseProgressionAndExactCommitReuse(t *testing.T) {
	// repo указывает изолированный репозиторий полного цикла версий разработки и стабильного
	// выпуска.
	repo := acceptanceReleaseRepo(t)
	// commit содержит текущий проверяемый HEAD и обновляется после каждого нового тестового коммита.
	commit := acceptanceGit(t, repo, "rev-parse", "HEAD")
	// short содержит сокращённый SHA текущего HEAD для метаданных dev-выпуска.
	short := acceptanceGit(t, repo, "rev-parse", "--short=12", "HEAD")
	// first задаёт ожидаемый первый dev-тег текущего цикла.
	first := "v0.1.0-dev.1+" + short
	acceptanceReleasePlan(t, repo, "develop", commit, first, false)
	acceptanceGit(t, repo, "tag", "-a", first, "-m", "Первый тестовый выпуск")
	acceptanceReleasePlan(t, repo, "develop", commit, first, true)

	acceptanceGit(t, repo, "commit", "--allow-empty", "-m", "Следующий merge-коммит")
	commit = acceptanceGit(t, repo, "rev-parse", "HEAD")
	short = acceptanceGit(t, repo, "rev-parse", "--short=12", "HEAD")
	// second задаёт ожидаемый второй dev-тег после следующего коммита.
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
	// next задаёт ожидаемый первый dev-тег нового минорного цикла после стабильной версии.
	next := "v0.2.0-dev.1+" + short
	acceptanceReleasePlan(t, repo, "develop", commit, next, false)
	acceptanceGit(t, repo, "tag", next)
	acceptanceReleasePlan(t, repo, "develop", commit, next, true)
	acceptanceReleasePlan(t, repo, "main", commit, "v0.2.0", false)
}

// Проверяет первый стабильный выпуск v0.1.0 и следующий минорный выпуск после нового коммита.
func TestAcceptanceFirstStableReleaseAndSubsequentMinor(t *testing.T) {
	// repo указывает репозиторий сценария первого стабильного и следующего минорного выпусков.
	repo := acceptanceReleaseRepo(t)
	// commit содержит HEAD, для которого по очереди планируются первый и следующий стабильные теги.
	commit := acceptanceGit(t, repo, "rev-parse", "HEAD")
	acceptanceReleasePlan(t, repo, "main", commit, "v0.1.0", false)
	acceptanceGit(t, repo, "tag", "v0.1.0")
	acceptanceGit(t, repo, "commit", "--allow-empty", "-m", "Следующий стабильный выпуск")
	commit = acceptanceGit(t, repo, "rev-parse", "HEAD")
	acceptanceReleasePlan(t, repo, "main", commit, "v0.2.0", false)
}

// Проверяет отказ без вывода плана при несовпадающем HEAD, чужих метаданных и двух тегах одного канала
// на коммите.
func TestAcceptanceReleasePlanRejectsAmbiguousOrWrongCommit(t *testing.T) {
	// scenario выбирает несовпадение HEAD, чужие метаданные или неоднозначность тегов одного канала.
	for _, scenario := range []string{"HEAD не совпадает", "чужие метаданные", "два dev-тега", "два стабильных тега"} {
		t.Run(scenario, func(t *testing.T) {
			// Проверяет отказ планирования выбранному неоднозначному набору тегов либо несовпадению коммита без
			// выдачи частичного результата.

			// repo указывает отдельный репозиторий текущего недопустимого сценария.
			repo := acceptanceReleaseRepo(t)
			// commit сохраняет исходный HEAD, с которым проверяется допустимость
			// планирования.
			commit := acceptanceGit(t, repo, "rev-parse", "HEAD")
			// short содержит правильные метаданные исходного коммита для сценариев двух
			// dev-тегов.
			short := acceptanceGit(t, repo, "rev-parse", "--short=12", "HEAD")
			// branch выбирает канал develop либо main согласно проверяемой неоднозначности.
			branch := "develop"
			switch scenario {
			case "HEAD не совпадает":
				acceptanceGit(t, repo, "commit", "--allow-empty", "-m", "Другой коммит")
			case "чужие метаданные":
				// wrong задаёт метаданные, заведомо не совпадающие с началом commit.
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
			// output перехватывает возможный частичный план, который при отказе должен
			// отсутствовать.
			var output bytes.Buffer
			// err содержит ожидаемый отказ планирования несовпадающего или неоднозначного
			// коммита.
			if err := run([]string{"-repo", repo, "-branch", branch, "-commit", commit}, &output); err == nil || output.Len() != 0 {
				t.Fatalf("небезопасное планирование прошло: err=%v, output=%q", err, output.String())
			}
		})
	}
}
