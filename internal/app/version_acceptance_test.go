package app

import (
	"bufio"
	"context"
	"encoding/json"
	"log/slog"
	"strings"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/buildinfo"
)

// Проверяет точные метаданные buildinfo в настоящем стартовом JSON-событии и подавление INFO при
// настроенном уровне ERROR.
func TestAcceptanceStartupBuildMetadataAndLogThreshold(t *testing.T) {
	// helper содержит собранный настоящий helper для проверки полного запуска приложения.
	helper := acceptanceHelper(t)
	// level выбирает INFO или ERROR для проверки допустимости стартового события.
	for _, level := range []slog.Level{slog.LevelInfo, slog.LevelError} {
		t.Run(level.String(), func(t *testing.T) {
			// Проверяет метаданные application_started и фильтрацию стартовой записи при выбранном уровне журнала.

			// c содержит изолированные настройки приложения с текущим уровнем журнала.
			c := acceptanceConfig(t, helper)
			c.Logging.Level = level
			// output сохраняет согласованный JSON-журнал настоящего приложения.
			output := &acceptanceLog{}
			// a содержит запущенное приложение; err содержит отказ его общей сборки.
			a, err := start(context.Background(), c, acceptanceDependencies(output))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				// Останавливает приложение после проверки стартового журнала.
				acceptanceStop(t, a)
			})
			acceptanceStop(t, a)
			// count считает только события application_started после полной остановки
			// журнала.
			count := 0
			// scanner читает захваченный журнал по целым JSON-записям.
			scanner := bufio.NewScanner(strings.NewReader(output.text()))
			for scanner.Scan() {
				// event принимает поля очередной JSON-записи для поиска стартового
				// события.
				var event map[string]any
				// err содержит отказ разбора JSON текущего события журнала.
				if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
					t.Fatal(err)
				}
				if event["msg"] != "application_started" {
					continue
				}
				count++
				// want содержит реальные buildinfo текущего бинарника для точного
				// сравнения стартовых полей.
				want := buildinfo.Current()
				if event["level"] != "INFO" || event["version"] != want.Version || event["commit"] != want.Commit || event["dirty"] != want.Dirty || event["go_version"] != want.GoVersion {
					t.Fatalf("стартовый журнал потерял метаданные: %v; ожидалось %+v", event, want)
				}
			}
			// err содержит отказ построчного чтения захваченного журнала.
			if err := scanner.Err(); err != nil {
				t.Fatal(err)
			}
			// wantCount задаёт одно стартовое событие при INFO и отсутствие события при
			// ERROR.
			wantCount := 1
			if level == slog.LevelError {
				wantCount = 0
			}
			if count != wantCount {
				t.Fatalf("при уровне %s стартовых записей %d, ожидалось %d", level, count, wantCount)
			}
		})
	}
}
