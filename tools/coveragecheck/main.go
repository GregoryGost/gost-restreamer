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

// decimalPercent допускает только беззнаковую десятичную запись порога без экспоненты и дробной черты.
var decimalPercent = regexp.MustCompile(`^(?:[0-9]+(?:\.[0-9]*)?|\.[0-9]+)$`)

// blockLocation определяет идентичность блока покрытия по исходному файлу и всем координатам.
type blockLocation struct {
	// file сохраняет точное имя исходного файла, включая пробелы и двоеточия.
	file string
	// coordinates хранит начальные строку и столбец, затем конечные строку и столбец блока.
	coordinates [4]uint64
}

// blockSample сохраняет число инструкций и объединяемый счётчик выполнения блока без ограничения int64.
type blockSample struct {
	// statements содержит вес блока в инструкциях для суммарного покрытия.
	statements *big.Int
	// count содержит число выполнений либо объединённый признак выполнения в режиме set.
	count *big.Int
}

// main возвращает процессу код проверки покрытия, направляя результат в stdout, а ошибки проверки — в
// stderr.
func main() {
	os.Exit(run(os.Args[1:], os.Stdout, os.Stderr))
}

// run читает указанный профиль и сравнивает взвешенное покрытие с точным рациональным порогом без
// округления. Коды 0, 1 и 2 различают достижение порога, недостаточное покрытие и невозможность
// проверки.
func run(args []string, stdout, stderr io.Writer) int {
	// profile указывает выбранный профиль; minimum содержит точный рациональный порог; err содержит
	// отказ разбора параметров.
	profile, minimum, err := parseArgs(args)
	if err != nil {
		return notVerified(stderr, err)
	}
	// data содержит текущее содержимое выбранного профиля покрытия.
	data, err := os.ReadFile(profile)
	if err != nil {
		return notVerified(stderr, fmt.Errorf("не удалось прочитать профиль %q: %w", profile, err))
	}
	// covered содержит число покрытых инструкций; total содержит общее число инструкций после
	// объединения повторных блоков.
	covered, total, err := statementCounts(data)
	if err != nil {
		return notVerified(stderr, err)
	}
	// percent содержит точное рациональное покрытие в процентах для сравнения без округления.
	percent := new(big.Rat).SetFrac(new(big.Int).Mul(covered, big.NewInt(100)), total)
	// verdict задаёт PASS или FAIL для отчёта; code задаёт соответствующий код процесса 0 или 1.
	verdict, code := "PASS", 0
	if percent.Cmp(minimum) < 0 {
		verdict, code = "FAIL", 1
	}
	// err содержит отказ записи итогового отчёта, при котором успешная проверка не подтверждается.
	if _, err := fmt.Fprintf(stdout, "%s: покрыто %s/%s инструкций; покрытие %s%% (округлено); порог %s%%.\n",
		verdict, covered, total, percent.FloatString(6), minimum.RatString()); err != nil {
		return notVerified(stderr, fmt.Errorf("не удалось вывести результат: %w", err))
	}
	return code
}

// notVerified выводит причину невозможности проверки в stderr и возвращает код 2.
func notVerified(stderr io.Writer, err error) int {
	_, _ = fmt.Fprintf(stderr, "NOT VERIFIED: %v\n", err)
	return 2
}

// parseArgs разбирает путь профиля и десятичный порог от 0 до 100, используя 85 по умолчанию.
// Неизвестные параметры и отсутствующие значения отклоняются.
func parseArgs(args []string) (string, *big.Rat, error) {
	// profile накапливает путь из -profile; minimumText хранит десятичный порог из -min либо
	// исходные 85.
	profile, minimumText := "", "85"
	// i указывает текущий аргумент CLI и пропускает отдельное значение параметра при необходимости.
	for i := 0; i < len(args); i++ {
		// name содержит имя параметра; value содержит его значение; hasValue отличает форму с
		// равенством от отдельного следующего аргумента.
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
	// minimum содержит точный рациональный порог; ok подтверждает успешное преобразование десятичной
	// строки.
	minimum, ok := new(big.Rat).SetString(minimumText)
	if !ok || minimum.Cmp(big.NewRat(100, 1)) > 0 {
		return "", nil, fmt.Errorf("порог %q должен быть десятичным числом от 0 до 100", minimumText)
	}
	return profile, minimum, nil
}

// statementCounts проверяет режим и все блоки профиля, объединяет повторные координаты по правилам set
// либо count/atomic и считает покрытые инструкции без двойного учёта. Пустой или повреждённый профиль
// и отсутствие исполняемых инструкций возвращают ошибку.
func statementCounts(data []byte) (*big.Int, *big.Int, error) {
	// scanner читает профиль по строкам для проверки заголовка и всех блоков.
	scanner := bufio.NewScanner(bytes.NewReader(data))
	if !scanner.Scan() {
		// err содержит отказ чтения заголовка, отличаемый от пустого профиля.
		if err := scanner.Err(); err != nil {
			return nil, nil, fmt.Errorf("не удалось прочитать заголовок: %w", err)
		}
		return nil, nil, fmt.Errorf("профиль пуст")
	}
	// mode содержит режим set, count или atomic, определяющий объединение счётчиков повторного
	// блока.
	mode := strings.TrimPrefix(scanner.Text(), "mode: ")
	if scanner.Text() != "mode: "+mode || (mode != "set" && mode != "count" && mode != "atomic") {
		return nil, nil, fmt.Errorf("некорректный режим профиля %q", scanner.Text())
	}
	// blocks хранит единственную запись каждого полного положения блока, исключая повторное
	// взвешивание.
	blocks := make(map[blockLocation]blockSample)
	// lineNumber отслеживает строку профиля для точной диагностики ошибочного блока.
	lineNumber := 1
	for scanner.Scan() {
		lineNumber++
		// location содержит полную идентичность прочитанного блока; sample содержит его вес и
		// счётчик; err содержит отказ разбора строки.
		location, sample, err := parseBlock(scanner.Text())
		if err != nil {
			return nil, nil, fmt.Errorf("строка %d: %w", lineNumber, err)
		}
		// previous содержит прежний отсчёт тех же координат; found отмечает повторный блок для
		// проверки веса и объединения счётчиков.
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
	// err содержит отказ построчного чтения оставшейся части профиля.
	if err := scanner.Err(); err != nil {
		return nil, nil, fmt.Errorf("не удалось прочитать профиль: %w", err)
	}
	// covered накапливает вес выполненных блоков; total накапливает вес всех уникальных блоков без
	// переполнения.
	covered, total := new(big.Int), new(big.Int)
	// sample содержит очередной уникальный блок для взвешенного суммирования инструкций.
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

// parseBlock разбирает счётчик, число инструкций и четыре координаты с конца строки, сохраняя пробелы
// и двоеточия в имени файла.
func parseBlock(line string) (blockLocation, blockSample, error) {
	// location собирает имя файла и четыре координаты разбираемого блока.
	var location blockLocation
	// sample собирает число инструкций и счётчик выполнения разбираемого блока.
	var sample blockSample
	// prefix содержит ещё не разобранную часть строки; count содержит последнее поле счётчика; err
	// содержит отказ отделения поля.
	prefix, count, err := takeField(line, ' ')
	if err != nil {
		return location, sample, err
	}
	// statements содержит текст веса блока, отделённый перед полем счётчика.
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
	// separators задаёт разделители четырёх координат в исходном порядке для разбора строки с конца.
	separators := [4]byte{':', '.', ',', '.'}
	// i указывает разбираемую координату от конечного столбца к начальной строке.
	for i := len(separators) - 1; i >= 0; i-- {
		// coordinate принимает текст очередной координаты перед проверкой беззнакового числа.
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

// takeField отделяет последнее поле по заданному разделителю; отсутствие разделителя считается ошибкой
// формата.
func takeField(text string, separator byte) (string, string, error) {
	// i содержит позицию последнего разделителя для отделения конечного поля без изменения префикса.
	i := strings.LastIndexByte(text, separator)
	if i < 0 {
		return "", "", fmt.Errorf("некорректный формат блока: нет разделителя %q", separator)
	}
	return text[:i], text[i+1:], nil
}

// naturalNumber разбирает непустую строку десятичных цифр в целое произвольной точности, отклоняя знак
// и прочие символы.
func naturalNumber(text string) (*big.Int, error) {
	if text == "" {
		return nil, fmt.Errorf("нужно неотрицательное целое число")
	}
	// digit содержит очередной символ числа, который должен быть десятичной цифрой.
	for _, digit := range text {
		if digit < '0' || digit > '9' {
			return nil, fmt.Errorf("%q не является неотрицательным целым числом", text)
		}
	}
	// value содержит целое произвольной точности; ok подтверждает успешный разбор проверенной строки
	// цифр.
	value, ok := new(big.Int).SetString(text, 10)
	if !ok {
		return nil, fmt.Errorf("не удалось прочитать целое число %q", text)
	}
	return value, nil
}
