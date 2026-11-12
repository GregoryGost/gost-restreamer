package main

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"testing"
)

// Проверяет взвешивание по числу инструкций и точное включение границы порога для всех режимов,
// включая значения за пределами int64.
func TestRunStatementWeightedThreshold(t *testing.T) {
	// cases задаёт профили с разными весами блоков и точные ожидаемые количества инструкций у
	// порога.
	cases := []struct {
		// name задаёт название сценария; rows содержит строки блоков; covered задаёт ожидаемый
		// числитель; total задаёт ожидаемый знаменатель покрытия.
		name, rows, covered, total string
		// pass задаёт ожидаемый результат точного сравнения с порогом 85%.
		pass bool
	}{
		{"below", "a.go:1.1,2.1 169 1\na.go:3.1,4.1 31 0\n", "169", "200", false},
		{"inclusive", "a.go:1.1,2.1 170 1\na.go:3.1,4.1 30 0\n", "170", "200", true},
		{"above", "a.go:1.1,2.1 171 1\na.go:3.1,4.1 29 0\n", "171", "200", true},
		{"rounded_display_is_insufficient", "a.go:1.1,2.1 8496 1\na.go:3.1,4.1 1504 0\n", "8496", "10000", false},
		{"weights_instead_of_package_mean", "large/a.go:1.1,2.1 90 1\nsmall/b.go:1.1,2.1 10 0\n", "90", "100", true},
		{"weights_instead_of_block_mean", "a.go:1.1,2.1 1 1\na.go:3.1,4.1 99 0\n", "1", "100", false},
		{"uncovered", "a.go:1.1,2.1 100 0\n", "0", "100", false},
		{"covered", "a.go:1.1,2.1 100 1\n", "100", "100", true},
		{"zero_statement_blocks", "a.go:1.1,2.1 85 1\na.go:3.1,4.1 15 0\na.go:5.1,6.1 0 1\n", "85", "100", true},
		{"zero_coordinates", "a.go:0.0,0.0 1 1\n", "1", "1", true},
		{"drive_colon_and_spaces", "C:/source tree/a.go:1.1,2.1 85 1\nC:/source tree/a.go:3.1,4.1 15 0\n", "85", "100", true},
		{"without_final_newline", "a.go:1.1,2.1 1 1", "1", "1", true},
		{"crlf", "a.go:1.1,2.1 1 1\r\n", "1", "1", true},
		{"large_exact_boundary", "a.go:1.1,2.1 850000000000000000 1\na.go:3.1,4.1 150000000000000000 0\n", "850000000000000000", "1000000000000000000", true},
		{"large_one_statement_below", "a.go:1.1,2.1 849999999999999999 1\na.go:3.1,4.1 150000000000000001 0\n", "849999999999999999", "1000000000000000000", false},
		{"aggregate_exceeds_int64", "a.go:1.1,2.1 9000000000000000000 1\na.go:3.1,4.1 9000000000000000000 1\na.go:5.1,6.1 1000000000000000000 0\n", "18000000000000000000", "19000000000000000000", true},
	}
	// mode выбирает режим профиля set, count или atomic для каждого набора весов.
	for _, mode := range []string{"set", "count", "atomic"} {
		// tc содержит текущий набор блоков и ожидаемое взвешенное покрытие.
		for _, tc := range cases {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				// Проверяет взвешенный по инструкциям порог покрытия для выбранных режима и набора блоков.

				// code содержит код проверки; output содержит объединённый отчёт по
				// текущему режиму и весам блоков.
				code, output := runProfile(t, "mode: "+mode+"\n"+tc.rows)
				assertCoverageResult(t, code, output, tc.pass, tc.covered, tc.total)
			})
		}
	}
}

// Проверяет объединение повторных блоков без повторного взвешивания, с учётом файла и всех координат и
// без переполнения счётчика.
func TestRunNormalizesRepeatedBlocks(t *testing.T) {
	// cases задаёт повторные блоки, чьи инструкции должны учитываться только один раз.
	cases := []struct {
		// name задаёт название повтора; rows содержит исходные блоки; covered задаёт ожидаемые
		// покрытые инструкции; total задаёт вес уникальных блоков.
		name, rows, covered, total string
		// pass задаёт ожидаемый результат после корректного объединения повторных блоков.
		pass bool
	}{
		{"mixed_samples_would_raw_sum_fail", "a.go:1.1,2.1 85 0\na.go:3.1,4.1 15 0\na.go:1.1,2.1 85 1\n", "85", "100", true},
		{"covered_duplicates_would_raw_sum_pass", "a.go:1.1,2.1 80 1\na.go:3.1,4.1 20 0\na.go:1.1,2.1 80 1\n", "80", "100", false},
		{"all_uncovered_duplicates", "a.go:1.1,2.1 85 0\na.go:3.1,4.1 15 1\na.go:1.1,2.1 85 0\n", "15", "100", false},
		{"complete_location_includes_end", "a.go:1.1,2.1 85 1\na.go:1.1,3.1 15 0\n", "85", "100", true},
		{"complete_location_includes_start_column", "a.go:1.1,2.1 85 1\na.go:1.2,2.1 15 0\n", "85", "100", true},
		{"complete_location_includes_end_column", "a.go:1.1,2.1 85 1\na.go:1.1,2.2 15 0\n", "85", "100", true},
		{"source_file_is_part_of_identity", "a.go:1.1,2.1 85 1\nb.go:1.1,2.1 15 0\n", "85", "100", true},
		{"nonadjacent_interleaved_duplicate", "a.go:1.1,2.1 85 0\na.go:1.1,3.1 15 0\nb.go:1.1,2.1 0 0\na.go:1.1,2.1 85 2\n", "85", "100", true},
		{"repeated_counts_do_not_overflow", "a.go:1.1,2.1 85 9223372036854775807\na.go:3.1,4.1 15 0\na.go:1.1,2.1 85 1\n", "85", "100", true},
	}
	// mode выбирает правило объединения счётчиков повторов для set, count или atomic.
	for _, mode := range []string{"set", "count", "atomic"} {
		// tc содержит текущий сценарий повторов с точным ожидаемым отношением инструкций.
		for _, tc := range cases {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				// Проверяет нормализацию повторных блоков без завышения общего числа инструкций и покрытия.

				// code содержит код проверки повторов; output содержит её отчёт для
				// сравнения с ожидаемым покрытием.
				code, output := runProfile(t, "mode: "+mode+"\n"+tc.rows)
				assertCoverageResult(t, code, output, tc.pass, tc.covered, tc.total)
			})
		}
	}
}

// Проверяет отказ для пустых, повреждённых, слишком длинных профилей и несовпадающего числа инструкций
// у повторного блока.
func TestRunRejectsUnusableProfiles(t *testing.T) {
	// cases связывает названия повреждений с непригодными байтами профиля покрытия.
	cases := map[string]string{
		"empty":                       "",
		"only_mode":                   "mode: atomic\n",
		"zero_statements":             "mode: atomic\na.go:1.1,2.1 0 1\n",
		"unknown_mode":                "mode: other\na.go:1.1,2.1 100 1\n",
		"missing_mode":                "a.go:1.1,2.1 100 1\n",
		"empty_mode":                  "mode: \na.go:1.1,2.1 100 1\n",
		"second_header":               "mode: atomic\nmode: atomic\na.go:1.1,2.1 100 1\n",
		"truncated_row":               "mode: atomic\na.go:1.1,2.1 100\n",
		"trailing_garbage":            "mode: atomic\na.go:1.1,2.1 100 1 garbage\n",
		"blank_filename":              "mode: atomic\n:1.1,2.1 100 1\n",
		"nonnumeric_coordinate":       "mode: atomic\na.go:x.1,2.1 100 1\n",
		"negative_start_line":         "mode: atomic\na.go:-1.1,2.1 100 1\n",
		"negative_start_column":       "mode: atomic\na.go:1.-1,2.1 100 1\n",
		"negative_end_line":           "mode: atomic\na.go:1.1,-2.1 100 1\n",
		"negative_end_column":         "mode: atomic\na.go:1.1,2.-1 100 1\n",
		"negative_statements":         "mode: atomic\na.go:1.1,2.1 -100 1\n",
		"negative_count":              "mode: atomic\na.go:1.1,2.1 100 -1\n",
		"fractional_count":            "mode: atomic\na.go:1.1,2.1 100 0.5\n",
		"valid_prefix_invalid_suffix": "mode: atomic\na.go:1.1,2.1 100 1\ninvalid\n",
		"blank_row":                   "mode: atomic\na.go:1.1,2.1 100 1\n\n",
		"oversized_mode":              strings.Repeat("x", 70000),
		"oversized_row":               "mode: atomic\n" + strings.Repeat("a", 70000) + ".go:1.1,2.1 100 1\n",
	}
	// name задаёт название повреждения; profile содержит непригодный профиль текущего подслучая.
	for name, profile := range cases {
		t.Run(name, func(t *testing.T) {
			// Проверяет отказ непригодному профилю без успешного кода или отметки PASS.

			// code содержит код отклонения профиля; output содержит сообщение, в котором не
			// должно быть PASS.
			code, output := runProfile(t, profile)
			assertNonpassing(t, code, output)
		})
	}
	// mode выбирает режим для проверки противоречивого веса повторного блока.
	for _, mode := range []string{"set", "count", "atomic"} {
		t.Run(mode+"/inconsistent_statement_count", func(t *testing.T) {
			// Проверяет отказ противоречивому числу инструкций одного блока в выбранном режиме покрытия.

			// code содержит код проверки противоречивого веса; output содержит диагностику
			// невозможности принять профиль.
			code, output := runProfile(t, "mode: "+mode+"\na.go:1.1,2.1 85 1\na.go:3.1,4.1 15 0\na.go:1.1,2.1 86 1\n")
			assertNonpassing(t, code, output)
		})
	}
}

// Проверяет точное сравнение десятичного порога, включая различия, меньшие точности отображаемого
// процента.
func TestRunUsesExactDecimalMinimum(t *testing.T) {
	// profile задаёт точное покрытие 8496/10000 для проверки близких десятичных порогов.
	profile := "mode: atomic\na.go:1.1,2.1 8496 1\na.go:3.1,4.1 1504 0\n"
	// tc задаёт очередной десятичный порог и ожидаемый исход без округления.
	for _, tc := range []struct {
		// minimum содержит десятичный порог, в том числе отличающийся за пределами точности
		// отображения.
		minimum string
		// pass указывает ожидаемое точное сравнение покрытия 84.96% с minimum.
		pass bool
	}{
		{"84.95", true},
		{"84.96", true},
		{"84.960000000000000000000000000001", false},
		{"85.0", false},
		{"0", true},
		{"100", false},
	} {
		t.Run(tc.minimum, func(t *testing.T) {
			// Проверяет точное сравнение покрытия с выбранным десятичным порогом без округления.

			// code содержит результат проверки текущего точного порога; output содержит
			// отчёт для проверки числителя и знаменателя.
			code, output := runProfile(t, profile, "-min", tc.minimum)
			assertCoverageResult(t, code, output, tc.pass, "8496", "10000")
		})
	}
	// code содержит результат проверки включённой границы 100%; output содержит соответствующий
	// отчёт и затем переиспользуется для границы 0%.
	code, output := runProfile(t, "mode: set\na.go:1.1,2.1 1 1\n", "-min", "100")
	assertCoverageResult(t, code, output, true, "1", "1")
	code, output = runProfile(t, "mode: set\na.go:1.1,2.1 1 0\n", "-min", "0")
	assertCoverageResult(t, code, output, true, "0", "1")
}

// Проверяет отклонение неверных порогов, параметров и недоступных путей профиля без успешного
// результата.
func TestRunRejectsInvalidArgumentsAndMissingProfiles(t *testing.T) {
	// minimum задаёт очередную недопустимую запись порога.
	for _, minimum := range []string{"", "-1", "100.01", "NaN", "Inf", "1/2", "8.5e1", "85%", "oops"} {
		t.Run("minimum/"+minimum, func(t *testing.T) {
			// Проверяет отказ недопустимому значению порога покрытия.

			// code содержит код отклонения порога; output содержит диагностику без успешного
			// PASS.
			code, output := runProfile(t, "mode: atomic\na.go:1.1,2.1 1 1\n", "-min", minimum)
			assertNonpassing(t, code, output)
		})
	}
	// args задаёт очередной неполный, неизвестный или указывающий недоступный профиль набор
	// аргументов.
	for _, args := range [][]string{
		nil,
		{"-profile"},
		{"-min"},
		{"-unknown"},
		{"-profile", filepath.Join(t.TempDir(), "missing.out")},
		{"-profile", t.TempDir()},
	} {
		// stdout сохраняет обычный отчёт проверки; stderr сохраняет диагностику невозможности
		// проверки.
		var stdout, stderr bytes.Buffer
		// code содержит итог CLI для текущих недопустимых аргументов.
		code := run(args, &stdout, &stderr)
		assertNonpassing(t, code, stdout.String()+stderr.String())
	}
	// code содержит код отказа лишнему позиционному аргументу; output содержит его диагностику.
	code, output := runProfile(t, "mode: atomic\na.go:1.1,2.1 1 1\n", "unexpected-positional-argument")
	assertNonpassing(t, code, output)
}

// Проверяет повторное чтение текущего содержимого файла и отказ после его замены или удаления вместо
// использования прежнего успешного профиля.
func TestRunCannotReusePreviousSuccessfulProfile(t *testing.T) {
	// profilePath указывает единственный файл, чьи байты затем заменяются или удаляются между
	// проверками.
	profilePath := filepath.Join(t.TempDir(), "coverage.out")
	writeTestFile(t, profilePath, "mode: atomic\na.go:1.1,2.1 1 1\n", 0o600)
	// stdout сохраняет отчёт текущего запуска; stderr сохраняет диагностику после замены либо
	// удаления профиля.
	var stdout, stderr bytes.Buffer
	// code содержит результат текущего чтения файла для выявления недопустимого повторного
	// использования прежнего успеха.
	code := run([]string{"-profile", profilePath}, &stdout, &stderr)
	assertCoverageResult(t, code, stdout.String()+stderr.String(), true, "1", "1")
	// replacement задаёт очередное содержимое, которое должно отменить прежний успешный результат.
	for _, replacement := range []string{"mode: atomic\na.go:1.1,2.1 1 0\n", "invalid"} {
		writeTestFile(t, profilePath, replacement, 0o600)
		stdout.Reset()
		stderr.Reset()
		code = run([]string{"-profile", profilePath}, &stdout, &stderr)
		assertNonpassing(t, code, stdout.String()+stderr.String())
	}
	// err содержит отказ удаления профиля перед проверкой отсутствующего файла.
	if err := os.Remove(profilePath); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	code = run([]string{"-profile", profilePath}, &stdout, &stderr)
	assertNonpassing(t, code, stdout.String()+stderr.String())
}

// Проверяет, что ошибка вывода результата не позволяет вернуть успешный код проверки покрытия.
func TestRunCannotReportSuccessWhenOutputFails(t *testing.T) {
	// profilePath указывает корректный полностью покрытый профиль для сценария отказа вывода.
	profilePath := filepath.Join(t.TempDir(), "coverage.out")
	writeTestFile(t, profilePath, "mode: atomic\na.go:1.1,2.1 1 1\n", 0o600)
	// stderr сохраняет диагностику невозможности вывести итоговый отчёт.
	var stderr bytes.Buffer
	// code содержит результат проверки с неисправным stdout, который должен быть ненулевым.
	code := run([]string{"-profile=" + profilePath, "-min=85"}, failingCoverageWriter{}, &stderr)
	assertNonpassing(t, code, stderr.String())
}

// failingCoverageWriter имитирует отказ выходного канала отчёта покрытия при любой записи.
type failingCoverageWriter struct{}

// Write имитирует отказ выходного канала, не принимая байты отчёта.
func (failingCoverageWriter) Write([]byte) (int, error) {
	return 0, io.ErrClosedPipe
}

// runProfile записывает отдельный профиль, вызывает проверку с переданными аргументами и возвращает
// код вместе с обоими потоками вывода.
func runProfile(t *testing.T, profile string, args ...string) (int, string) {
	t.Helper()
	// profilePath указывает отдельный файл профиля текущего тестового вызова.
	profilePath := filepath.Join(t.TempDir(), "coverage.out")
	writeTestFile(t, profilePath, profile, 0o600)
	// stdout собирает отчёт проверки; stderr собирает диагностику того же вызова для общего
	// возвращаемого текста.
	var stdout, stderr bytes.Buffer
	// code содержит код проверки только что записанного профиля с дополнительными аргументами теста.
	code := run(append([]string{"-profile", profilePath}, args...), &stdout, &stderr)
	return code, stdout.String() + stderr.String()
}

// writeTestFile создаёт тестовый файл с заданными байтами и правами, завершая сценарий при ошибке.
func writeTestFile(t *testing.T, path, contents string, mode os.FileMode) {
	t.Helper()
	// err содержит отказ записи профиля или другого тестового файла с заданными правами.
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
}

// assertCoverageResult проверяет код, точное отношение покрытых инструкций к общему числу и
// соответствующий PASS либо FAIL в отчёте.
func assertCoverageResult(t *testing.T, code int, output string, pass bool, covered, total string) {
	t.Helper()
	if (code == 0) != pass {
		t.Fatalf("код = %d, ожидается успешность %t; вывод: %s", code, pass, output)
	}
	// ratio ищет точное ожидаемое отношение covered/total независимо от промежутков в отчёте.
	ratio := regexp.MustCompile(fmt.Sprintf(`\b%s\s*/\s*%s\b`, covered, total))
	if !ratio.MatchString(output) {
		t.Errorf("в отчёте нет ожидаемого отношения %s/%s: %s", covered, total, output)
	}
	// verdict задаёт ожидаемое слово PASS либо FAIL согласно проверяемому исходу.
	verdict := "FAIL"
	if pass {
		verdict = "PASS"
	}
	if !regexp.MustCompile(`\b` + verdict + `\b`).MatchString(strings.ToUpper(output)) {
		t.Errorf("в отчёте нет результата %s: %s", verdict, output)
	}
}

// assertNonpassing требует ненулевой код и отсутствие PASS для недопустимого ввода или невозможности
// проверки.
func assertNonpassing(t *testing.T, code int, output string) {
	t.Helper()
	if code == 0 {
		t.Errorf("некорректный ввод дал успешный код: %s", output)
	}
	if regexp.MustCompile(`\bPASS\b`).MatchString(strings.ToUpper(output)) {
		t.Errorf("некорректный ввод дал PASS: %s", output)
	}
}
