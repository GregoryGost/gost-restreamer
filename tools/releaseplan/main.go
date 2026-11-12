package main

import (
	"bytes"
	"flag"
	"fmt"
	"io"
	"os"
	"os/exec"
	"regexp"
	"strconv"
	"strings"
)

// maxSafeInteger задаёт исключённую верхнюю границу числовых частей тегов, принимаемых и создаваемых
// планировщиком.
const maxSafeInteger = 9007199254740991

var (
	// releasePattern распознаёт только поддержанные стабильные теги и dev-теги с числовыми частями и
	// Git-метаданными.
	releasePattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-dev\.([1-9][0-9]*)\+([0-9a-f]{7,40}))?$`)
	// versionLike отличает неподдержанный тег, похожий на версию, от постороннего тега, который
	// можно пропустить.
	versionLike = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+`)
	// commitPattern допускает только полный 40-символьный Git SHA из строчных шестнадцатеричных
	// цифр.
	commitPattern = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

// release хранит разобранные части поддержанного тега и точный коммит, до которого он разрешён.
type release struct {
	// major содержит старшую часть версии; minor содержит минорную часть; patch содержит
	// исправление; dev содержит счётчик разработки либо ноль для стабильного тега.
	major, minor, patch, dev uint64
	// name сохраняет точное имя тега; commit сохраняет его полный коммит; metadata сохраняет
	// сокращённый SHA из dev-тега.
	name, commit, metadata string
}

// plan задаёт вычисленную версию и параметры version-tagger либо повторного использования точного тега.
type plan struct {
	// version содержит итоговый тег; releaseType задаёт повышение minor; postfix задаёт dev для
	// разработки; metadata содержит сокращённый SHA нового dev-выпуска.
	version, releaseType, postfix, metadata string
	// useLastTag сообщает о наличии исходных тегов; reuse сообщает о повторном использовании
	// единственного подходящего тега текущего коммита.
	useLastTag, reuse bool
}

// main вычисляет план версии и выводит ошибку с ненулевым завершением процесса при невозможности
// безопасного планирования.
func main() {
	// err содержит отказ вычисления или вывода плана, определяющий ошибочное завершение процесса.
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run проверяет ветку и точный HEAD, читает теги и выводит параметры назначения либо повторного
// использования версии. Планирование не изменяет ссылки Git.
func run(args []string, out io.Writer) error {
	// flags разбирает параметры планировщика, возвращая ошибки без собственного вывода справки.
	flags := flag.NewFlagSet("releaseplan", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	// branch указывает выбранный канал main или develop для вычисления вида версии.
	branch := flags.String("branch", "", "Ветка выпуска: main или develop.")
	// commit указывает полный SHA ожидаемого коммита слияния, который должен совпасть с HEAD.
	commit := flags.String("commit", "", "Полный SHA merge-коммита.")
	// repo указывает каталог репозитория, чьи HEAD и теги читаются без изменения ссылок.
	repo := flags.String("repo", ".", "Каталог Git-репозитория.")
	// err содержит отказ разбора аргументов планировщика версии.
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || (*branch != "main" && *branch != "develop") || !commitPattern.MatchString(*commit) {
		return fmt.Errorf("нужны -branch main|develop и -commit с полным SHA")
	}
	// head содержит фактический HEAD выбранного репозитория; err содержит отказ его чтения.
	head, err := git(*repo, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if head != *commit {
		return fmt.Errorf("HEAD %s не совпадает с merge-коммитом %s", head, *commit)
	}
	// short содержит сокращённый SHA текущего коммита для метаданных нового dev-тега.
	short, err := git(*repo, "rev-parse", "--short=12", *commit)
	if err != nil {
		return err
	}
	// tags содержит проверенные поддержанные теги выбранного репозитория.
	tags, err := readTags(*repo)
	if err != nil {
		return err
	}
	// p содержит рассчитанный план выпуска или повторного использования версии.
	p, err := makePlan(*branch, *commit, short, tags)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "version=%s\nrelease_type=%s\npostfix=%s\nmetadata=%s\nuse_last_tag=%t\nreuse=%t\n",
		p.version, p.releaseType, p.postfix, p.metadata, p.useLastTag, p.reuse)
	return err
}

// git запускает команду Git в выбранном репозитории и возвращает очищенный вывод либо ошибку со
// stderr.
func git(repo string, args ...string) (string, error) {
	// cmd запускает указанную Git-команду в выбранном репозитории.
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	// stderr сохраняет диагностику Git, добавляемую к ошибке команды.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	// output содержит stdout Git; err содержит отказ выполнения выбранной команды.
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(output)), nil
}

// readTags читает поддерживаемые теги выпуска и разрешает их до коммитов. Неподдерживаемые теги,
// похожие на версии, слишком большие числа и повторные семантические версии отклоняются.
func readTags(repo string) ([]release, error) {
	// names содержит имена всех тегов репозитория; err содержит отказ их перечисления.
	names, err := git(repo, "for-each-ref", "--format=%(refname:strip=2)", "refs/tags")
	if err != nil {
		return nil, err
	}
	// tags накапливает поддержанные теги с числовыми частями и разрешёнными коммитами.
	var tags []release
	// semanticNames сопоставляет версию без метаданных с первым именем тега для отклонения дублей.
	semanticNames := make(map[string]string)
	// name содержит очередное имя тега из вывода Git.
	for name := range strings.SplitSeq(names, "\n") {
		// fields содержит распознанные числовые части и метаданные поддержанного тега либо nil.
		fields := releasePattern.FindStringSubmatch(name)
		if fields == nil {
			if versionLike.MatchString(name) {
				return nil, fmt.Errorf("неподдерживаемый тег выпуска %q: нужны vX.Y.Z или vX.Y.Z-dev.N+commit", name)
			}
			continue
		}
		// numbers сохраняет разобранные major, minor, patch и необязательный dev-счётчик.
		numbers := [4]uint64{}
		// i указывает числовую часть версии; field содержит её десятичную запись либо пустой dev
		// для стабильного тега.
		for i, field := range fields[1:5] {
			if field == "" {
				continue
			}
			// n содержит разобранную часть версии; parseErr содержит отказ её беззнакового
			// преобразования.
			n, parseErr := strconv.ParseUint(field, 10, 64)
			if parseErr != nil || n >= maxSafeInteger {
				return nil, fmt.Errorf("число вне поддерживаемого диапазона в теге %q", name)
			}
			numbers[i] = n
		}
		// semantic содержит версию тега без метаданных для проверки семантической уникальности.
		semantic, _, _ := strings.Cut(name, "+")
		// previous содержит первый тег той же семантической версии; ok отмечает запрещённый
		// дубль.
		if previous, ok := semanticNames[semantic]; ok {
			return nil, fmt.Errorf("теги %q и %q имеют одинаковую семантическую версию", previous, name)
		}
		semanticNames[semantic] = name
		// commit содержит полный коммит тега; resolveErr содержит отказ разрешения легковесного
		// или аннотированного тега до коммита.
		commit, resolveErr := git(repo, "rev-parse", "refs/tags/"+name+"^{commit}")
		if resolveErr != nil {
			return nil, resolveErr
		}
		tags = append(tags, release{numbers[0], numbers[1], numbers[2], numbers[3], name, commit, fields[5]})
	}
	return tags, nil
}

// makePlan повторно использует единственный подходящий тег точного коммита либо вычисляет следующую
// версию по максимальному тегу. Метаданные версии разработки должны соответствовать коммиту, а
// неоднозначность и исчерпание диапазона чисел останавливают планирование.
func makePlan(branch, commit, short string, tags []release) (plan, error) {
	// p накапливает параметры планируемой версии и обнаружение повторного подходящего тега.
	p := plan{useLastTag: len(tags) > 0}
	// latest сохраняет максимальную поддержанную семантическую версию независимо от порядка тегов.
	var latest release
	// tag содержит очередной проверенный тег при выборе максимума и точного совпадения текущего
	// коммита.
	for _, tag := range tags {
		if latest.name == "" || compare(tag, latest) > 0 {
			latest = tag
		}
		if tag.commit != commit || (tag.dev > 0) != (branch == "develop") {
			continue
		}
		if tag.dev > 0 && !strings.HasPrefix(commit, tag.metadata) {
			return plan{}, fmt.Errorf("метаданные тега %q не совпадают с его коммитом", tag.name)
		}
		if p.reuse {
			return plan{}, fmt.Errorf("несколько тегов выпуска для ветки %s и коммита %s", branch, commit)
		}
		p.version, p.reuse = tag.name, true
	}
	if p.reuse {
		return p, nil
	}
	// next содержит числовую основу следующей версии, изменяемую согласно ветке и предыдущему
	// выпуску.
	next := latest
	if latest.name == "" {
		next = release{minor: 1}
	} else if latest.dev == 0 || (branch == "main" && latest.patch != 0) {
		p.releaseType = "minor"
		next.minor++
		next.patch, next.dev = 0, 0
	} else if branch == "main" {
		p.releaseType = "minor"
		next.dev = 0
	}
	if branch == "develop" {
		next.dev++
		p.postfix, p.metadata = "dev", short
	}
	if next.minor >= maxSafeInteger || next.dev >= maxSafeInteger {
		return plan{}, fmt.Errorf("исчерпан диапазон чисел версии")
	}
	p.version = fmt.Sprintf("v%d.%d.%d", next.major, next.minor, next.patch)
	if next.dev > 0 {
		p.version += fmt.Sprintf("-dev.%d+%s", next.dev, short)
	}
	return p, nil
}

// compare сравнивает числовые части версии и dev-счётчик, считая стабильную версию выше её
// предварительных выпусков с той же базой.
func compare(a, b release) int {
	// pair содержит соответствующие major, minor или patch сравниваемых тегов для числового порядка.
	for _, pair := range [][2]uint64{{a.major, b.major}, {a.minor, b.minor}, {a.patch, b.patch}} {
		if pair[0] < pair[1] {
			return -1
		}
		if pair[0] > pair[1] {
			return 1
		}
	}
	if a.dev == b.dev {
		return 0
	}
	if a.dev == 0 || (b.dev != 0 && a.dev > b.dev) {
		return 1
	}
	return -1
}
