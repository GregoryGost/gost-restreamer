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

func TestRunStatementWeightedThreshold(t *testing.T) {
	cases := []struct {
		name, rows, covered, total string
		pass                       bool
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
	for _, mode := range []string{"set", "count", "atomic"} {
		for _, tc := range cases {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				code, output := runProfile(t, "mode: "+mode+"\n"+tc.rows)
				assertCoverageResult(t, code, output, tc.pass, tc.covered, tc.total)
			})
		}
	}
}

func TestRunNormalizesRepeatedBlocks(t *testing.T) {
	cases := []struct {
		name, rows, covered, total string
		pass                       bool
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
	for _, mode := range []string{"set", "count", "atomic"} {
		for _, tc := range cases {
			t.Run(mode+"/"+tc.name, func(t *testing.T) {
				code, output := runProfile(t, "mode: "+mode+"\n"+tc.rows)
				assertCoverageResult(t, code, output, tc.pass, tc.covered, tc.total)
			})
		}
	}
}

func TestRunRejectsUnusableProfiles(t *testing.T) {
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
	for name, profile := range cases {
		t.Run(name, func(t *testing.T) {
			code, output := runProfile(t, profile)
			assertNonpassing(t, code, output)
		})
	}
	for _, mode := range []string{"set", "count", "atomic"} {
		t.Run(mode+"/inconsistent_statement_count", func(t *testing.T) {
			code, output := runProfile(t, "mode: "+mode+"\na.go:1.1,2.1 85 1\na.go:3.1,4.1 15 0\na.go:1.1,2.1 86 1\n")
			assertNonpassing(t, code, output)
		})
	}
}

func TestRunUsesExactDecimalMinimum(t *testing.T) {
	profile := "mode: atomic\na.go:1.1,2.1 8496 1\na.go:3.1,4.1 1504 0\n"
	for _, tc := range []struct {
		minimum string
		pass    bool
	}{
		{"84.95", true},
		{"84.96", true},
		{"84.960000000000000000000000000001", false},
		{"85.0", false},
		{"0", true},
		{"100", false},
	} {
		t.Run(tc.minimum, func(t *testing.T) {
			code, output := runProfile(t, profile, "-min", tc.minimum)
			assertCoverageResult(t, code, output, tc.pass, "8496", "10000")
		})
	}
	code, output := runProfile(t, "mode: set\na.go:1.1,2.1 1 1\n", "-min", "100")
	assertCoverageResult(t, code, output, true, "1", "1")
	code, output = runProfile(t, "mode: set\na.go:1.1,2.1 1 0\n", "-min", "0")
	assertCoverageResult(t, code, output, true, "0", "1")
}

func TestRunRejectsInvalidArgumentsAndMissingProfiles(t *testing.T) {
	for _, minimum := range []string{"", "-1", "100.01", "NaN", "Inf", "1/2", "8.5e1", "85%", "oops"} {
		t.Run("minimum/"+minimum, func(t *testing.T) {
			code, output := runProfile(t, "mode: atomic\na.go:1.1,2.1 1 1\n", "-min", minimum)
			assertNonpassing(t, code, output)
		})
	}
	for _, args := range [][]string{
		nil,
		{"-profile"},
		{"-min"},
		{"-unknown"},
		{"-profile", filepath.Join(t.TempDir(), "missing.out")},
		{"-profile", t.TempDir()},
	} {
		var stdout, stderr bytes.Buffer
		code := run(args, &stdout, &stderr)
		assertNonpassing(t, code, stdout.String()+stderr.String())
	}
	code, output := runProfile(t, "mode: atomic\na.go:1.1,2.1 1 1\n", "unexpected-positional-argument")
	assertNonpassing(t, code, output)
}

func TestRunCannotReusePreviousSuccessfulProfile(t *testing.T) {
	profilePath := filepath.Join(t.TempDir(), "coverage.out")
	writeTestFile(t, profilePath, "mode: atomic\na.go:1.1,2.1 1 1\n", 0o600)
	var stdout, stderr bytes.Buffer
	code := run([]string{"-profile", profilePath}, &stdout, &stderr)
	assertCoverageResult(t, code, stdout.String()+stderr.String(), true, "1", "1")
	for _, replacement := range []string{"mode: atomic\na.go:1.1,2.1 1 0\n", "invalid"} {
		writeTestFile(t, profilePath, replacement, 0o600)
		stdout.Reset()
		stderr.Reset()
		code = run([]string{"-profile", profilePath}, &stdout, &stderr)
		assertNonpassing(t, code, stdout.String()+stderr.String())
	}
	if err := os.Remove(profilePath); err != nil {
		t.Fatal(err)
	}
	stdout.Reset()
	stderr.Reset()
	code = run([]string{"-profile", profilePath}, &stdout, &stderr)
	assertNonpassing(t, code, stdout.String()+stderr.String())
}

func TestRunCannotReportSuccessWhenOutputFails(t *testing.T) {
	profilePath := filepath.Join(t.TempDir(), "coverage.out")
	writeTestFile(t, profilePath, "mode: atomic\na.go:1.1,2.1 1 1\n", 0o600)
	var stderr bytes.Buffer
	code := run([]string{"-profile=" + profilePath, "-min=85"}, failingCoverageWriter{}, &stderr)
	assertNonpassing(t, code, stderr.String())
}

type failingCoverageWriter struct{}

func (failingCoverageWriter) Write([]byte) (int, error) {
	return 0, io.ErrClosedPipe
}

func runProfile(t *testing.T, profile string, args ...string) (int, string) {
	t.Helper()
	profilePath := filepath.Join(t.TempDir(), "coverage.out")
	writeTestFile(t, profilePath, profile, 0o600)
	var stdout, stderr bytes.Buffer
	code := run(append([]string{"-profile", profilePath}, args...), &stdout, &stderr)
	return code, stdout.String() + stderr.String()
}

func writeTestFile(t *testing.T, path, contents string, mode os.FileMode) {
	t.Helper()
	if err := os.WriteFile(path, []byte(contents), mode); err != nil {
		t.Fatal(err)
	}
}

func assertCoverageResult(t *testing.T, code int, output string, pass bool, covered, total string) {
	t.Helper()
	if (code == 0) != pass {
		t.Fatalf("код = %d, ожидается успешность %t; вывод: %s", code, pass, output)
	}
	ratio := regexp.MustCompile(fmt.Sprintf(`\b%s\s*/\s*%s\b`, covered, total))
	if !ratio.MatchString(output) {
		t.Errorf("в отчёте нет ожидаемого отношения %s/%s: %s", covered, total, output)
	}
	verdict := "FAIL"
	if pass {
		verdict = "PASS"
	}
	if !regexp.MustCompile(`\b` + verdict + `\b`).MatchString(strings.ToUpper(output)) {
		t.Errorf("в отчёте нет результата %s: %s", verdict, output)
	}
}

func assertNonpassing(t *testing.T, code int, output string) {
	t.Helper()
	if code == 0 {
		t.Errorf("некорректный ввод дал успешный код: %s", output)
	}
	if regexp.MustCompile(`\bPASS\b`).MatchString(strings.ToUpper(output)) {
		t.Errorf("некорректный ввод дал PASS: %s", output)
	}
}
