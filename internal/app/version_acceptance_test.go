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

func TestAcceptanceStartupBuildMetadataAndLogThreshold(t *testing.T) {
	helper := acceptanceHelper(t)
	for _, level := range []slog.Level{slog.LevelInfo, slog.LevelError} {
		t.Run(level.String(), func(t *testing.T) {
			c := acceptanceConfig(t, helper)
			c.Logging.Level = level
			output := &acceptanceLog{}
			a, err := start(context.Background(), c, acceptanceDependencies(output))
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { acceptanceStop(t, a) })
			acceptanceStop(t, a)
			count := 0
			scanner := bufio.NewScanner(strings.NewReader(output.text()))
			for scanner.Scan() {
				var event map[string]any
				if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
					t.Fatal(err)
				}
				if event["msg"] != "application_started" {
					continue
				}
				count++
				want := buildinfo.Current()
				if event["level"] != "INFO" || event["version"] != want.Version || event["commit"] != want.Commit || event["dirty"] != want.Dirty || event["go_version"] != want.GoVersion {
					t.Fatalf("стартовый журнал потерял метаданные: %v; ожидалось %+v", event, want)
				}
			}
			if err := scanner.Err(); err != nil {
				t.Fatal(err)
			}
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
