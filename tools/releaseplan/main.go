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

const maxSafeInteger = 9007199254740991

var (
	releasePattern = regexp.MustCompile(`^v(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)\.(0|[1-9][0-9]*)(?:-dev\.([1-9][0-9]*)\+([0-9a-f]{7,40}))?$`)
	versionLike    = regexp.MustCompile(`^v[0-9]+\.[0-9]+\.[0-9]+`)
	commitPattern  = regexp.MustCompile(`^[0-9a-f]{40}$`)
)

type release struct {
	major, minor, patch, dev uint64
	name, commit, metadata   string
}

type plan struct {
	version, releaseType, postfix, metadata string
	useLastTag, reuse                       bool
}

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

func run(args []string, out io.Writer) error {
	flags := flag.NewFlagSet("releaseplan", flag.ContinueOnError)
	flags.SetOutput(io.Discard)
	branch := flags.String("branch", "", "Ветка выпуска: main или develop.")
	commit := flags.String("commit", "", "Полный SHA merge-коммита.")
	repo := flags.String("repo", ".", "Каталог Git-репозитория.")
	if err := flags.Parse(args); err != nil {
		return err
	}
	if flags.NArg() != 0 || (*branch != "main" && *branch != "develop") || !commitPattern.MatchString(*commit) {
		return fmt.Errorf("нужны -branch main|develop и -commit с полным SHA")
	}
	head, err := git(*repo, "rev-parse", "HEAD")
	if err != nil {
		return err
	}
	if head != *commit {
		return fmt.Errorf("HEAD %s не совпадает с merge-коммитом %s", head, *commit)
	}
	short, err := git(*repo, "rev-parse", "--short=12", *commit)
	if err != nil {
		return err
	}
	tags, err := readTags(*repo)
	if err != nil {
		return err
	}
	p, err := makePlan(*branch, *commit, short, tags)
	if err != nil {
		return err
	}
	_, err = fmt.Fprintf(out, "version=%s\nrelease_type=%s\npostfix=%s\nmetadata=%s\nuse_last_tag=%t\nreuse=%t\n",
		p.version, p.releaseType, p.postfix, p.metadata, p.useLastTag, p.reuse)
	return err
}

func git(repo string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", repo}, args...)...)
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	output, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(stderr.String()))
	}
	return strings.TrimSpace(string(output)), nil
}

func readTags(repo string) ([]release, error) {
	names, err := git(repo, "for-each-ref", "--format=%(refname:strip=2)", "refs/tags")
	if err != nil {
		return nil, err
	}
	var tags []release
	semanticNames := make(map[string]string)
	for name := range strings.SplitSeq(names, "\n") {
		fields := releasePattern.FindStringSubmatch(name)
		if fields == nil {
			if versionLike.MatchString(name) {
				return nil, fmt.Errorf("неподдерживаемый тег выпуска %q: нужны vX.Y.Z или vX.Y.Z-dev.N+commit", name)
			}
			continue
		}
		numbers := [4]uint64{}
		for i, field := range fields[1:5] {
			if field == "" {
				continue
			}
			n, parseErr := strconv.ParseUint(field, 10, 64)
			if parseErr != nil || n >= maxSafeInteger {
				return nil, fmt.Errorf("число вне поддерживаемого диапазона в теге %q", name)
			}
			numbers[i] = n
		}
		semantic, _, _ := strings.Cut(name, "+")
		if previous, ok := semanticNames[semantic]; ok {
			return nil, fmt.Errorf("теги %q и %q имеют одинаковую семантическую версию", previous, name)
		}
		semanticNames[semantic] = name
		commit, resolveErr := git(repo, "rev-parse", "refs/tags/"+name+"^{commit}")
		if resolveErr != nil {
			return nil, resolveErr
		}
		tags = append(tags, release{numbers[0], numbers[1], numbers[2], numbers[3], name, commit, fields[5]})
	}
	return tags, nil
}

func makePlan(branch, commit, short string, tags []release) (plan, error) {
	p := plan{useLastTag: len(tags) > 0}
	var latest release
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

func compare(a, b release) int {
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
