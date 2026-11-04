package main

import (
	"bufio"
	"bytes"
	"fmt"
	"io"
	"math/big"
	"os"
	"regexp"
	"strconv"
	"strings"
)

var decimalPercent = regexp.MustCompile(`^(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)$`)

type blockLocation struct {
	file        string
	coordinates [4]uint64
}

type blockSample struct {
	statements *big.Int
	count      *big.Int
}

func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

func run(args []string, stdout, stderr io.Writer) int {
	profile, minimum, err := parseArgs(args)
	if err != nil {
		return notVerified(stderr, err)
	}
	data, err := os.ReadFile(profile)
	if err != nil {
		return notVerified(stderr, fmt.Errorf("не удалось прочитать профиль %q: %w", profile, err))
	}
	covered, total, err := statementCounts(data)
	if err != nil {
		return notVerified(stderr, err)
	}
	percent := new(big.Rat).SetFrac(new(big.Int).Mul(covered, big.NewInt(100)), total)
	verdict, code := "PASS", 0
	if percent.Cmp(minimum) < 0 {
		verdict, code = "FAIL", 1
	}
	if _, err := fmt.Fprintf(stdout, "%s: покрыто %s/%s инструкций; покрытие %s%% (округлено); порог %s%%.\n",
		verdict, covered, total, percent.FloatString(6), minimum.RatString()); err != nil {
		return notVerified(stderr, fmt.Errorf("не удалось вывести результат: %w", err))
	}
	return code
}

func notVerified(stderr io.Writer, err error) int {
	_, _ = fmt.Fprintf(stderr, "NOT VERIFIED: %v\n", err)
	return 2
}

func parseArgs(args []string) (string, *big.Rat, error) {
	profile, minimumText := "", "85"
	for i := 0; i < len(args); i++ {
		name, value, hasValue := strings.Cut(args[i], "=")
		if name != "-profile" && name != "-min" {
			return "", nil, fmt.Errorf("неизвестный аргумент %q; использование: coveragecheck -profile <путь> [-min <процент>]", args[i])
		}
		if !hasValue {
			i++
			if i == len(args) {
				return "", nil, fmt.Errorf("для %s требуется значение", name)
			}
			value = args[i]
		}
		if name == "-profile" {
			profile = value
		} else {
			minimumText = value
		}
	}
	if profile == "" {
		return "", nil, fmt.Errorf("нужен непустой путь -profile")
	}
	if !decimalPercent.MatchString(minimumText) {
		return "", nil, fmt.Errorf("порог %q должен быть десятичным числом от 0 до 100", minimumText)
	}
	minimum, ok := new(big.Rat).SetString(minimumText)
	if !ok || minimum.Cmp(big.NewRat(100, 1)) > 0 {
		return "", nil, fmt.Errorf("порог %q должен быть десятичным числом от 0 до 100", minimumText)
	}
	return profile, minimum, nil
}

func statementCounts(data []byte) (*big.Int, *big.Int, error) {
	scanner := bufio.NewScanner(bytes.NewReader(data))
	if !scanner.Scan() {
		if err := scanner.Err(); err != nil {
			return nil, nil, fmt.Errorf("не удалось прочитать заголовок: %w", err)
		}
		return nil, nil, fmt.Errorf("профиль пуст")
	}
	mode := strings.TrimPrefix(scanner.Text(), "mode: ")
	if scanner.Text() != "mode: "+mode || (mode != "set" && mode != "count" && mode != "atomic") {
		return nil, nil, fmt.Errorf("некорректный режим профиля %q", scanner.Text())
	}
	blocks := make(map[blockLocation]blockSample)
	lineNumber := 1
	for scanner.Scan() {
		lineNumber++
		location, sample, err := parseBlock(scanner.Text())
		if err != nil {
			return nil, nil, fmt.Errorf("строка %d: %w", lineNumber, err)
		}
		if previous, found := blocks[location]; found {
			if previous.statements.Cmp(sample.statements) != 0 {
				return nil, nil, fmt.Errorf("строка %d: повторный блок содержит другое число инструкций", lineNumber)
			}
			if mode == "set" {
				previous.count.Or(previous.count, sample.count)
			} else {
				previous.count.Add(previous.count, sample.count)
			}
		} else {
			blocks[location] = sample
		}
	}
	if err := scanner.Err(); err != nil {
		return nil, nil, fmt.Errorf("не удалось прочитать профиль: %w", err)
	}
	covered, total := new(big.Int), new(big.Int)
	for _, sample := range blocks {
		total.Add(total, sample.statements)
		if sample.count.Sign() > 0 {
			covered.Add(covered, sample.statements)
		}
	}
	if total.Sign() == 0 {
		return nil, nil, fmt.Errorf("в профиле нет исполняемых инструкций")
	}
	return covered, total, nil
}

func parseBlock(line string) (blockLocation, blockSample, error) {
	var location blockLocation
	var sample blockSample
	prefix, count, err := takeField(line, ' ')
	if err != nil {
		return location, sample, err
	}
	prefix, statements, err := takeField(prefix, ' ')
	if err != nil {
		return location, sample, err
	}
	sample.count, err = naturalNumber(count)
	if err != nil {
		return location, sample, fmt.Errorf("некорректный счётчик: %w", err)
	}
	sample.statements, err = naturalNumber(statements)
	if err != nil {
		return location, sample, fmt.Errorf("некорректное число инструкций: %w", err)
	}
	separators := [4]byte{':', '.', ',', '.'}
	for i := len(separators) - 1; i >= 0; i-- {
		var coordinate string
		prefix, coordinate, err = takeField(prefix, separators[i])
		if err != nil {
			return location, sample, err
		}
		location.coordinates[i], err = strconv.ParseUint(coordinate, 10, 64)
		if err != nil {
			return location, sample, fmt.Errorf("некорректная координата %q", coordinate)
		}
	}
	if prefix == "" {
		return location, sample, fmt.Errorf("имя исходного файла пусто")
	}
	location.file = prefix
	return location, sample, nil
}

func takeField(text string, separator byte) (string, string, error) {
	i := strings.LastIndexByte(text, separator)
	if i < 0 {
		return "", "", fmt.Errorf("некорректный формат блока: нет разделителя %q", separator)
	}
	return text[:i], text[i+1:], nil
}

func naturalNumber(text string) (*big.Int, error) {
	if text == "" {
		return nil, fmt.Errorf("нужно неотрицательное целое число")
	}
	for _, digit := range text {
		if digit < '0' || digit > '9' {
			return nil, fmt.Errorf("%q не является неотрицательным целым числом", text)
		}
	}
	value, ok := new(big.Int).SetString(text, 10)
	if !ok {
		return nil, fmt.Errorf("не удалось прочитать целое число %q", text)
	}
	return value, nil
}
