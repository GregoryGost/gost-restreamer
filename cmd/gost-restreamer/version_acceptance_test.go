package main

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/buildinfo"
	"github.com/GregoryGost/gost-restreamer/internal/config"
)

func TestAcceptanceVersionExitsBeforeConfigurationAndStartup(t *testing.T) {
	for _, args := range [][]string{{"--version"}, {"--config", "/missing/config.yaml", "--version"}, {"--version", "--config", "/missing/config.yaml"}} {
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		var stdout, stderr bytes.Buffer
		deps := commandDependencies{
			load: func(string) (config.Config, config.Selection, error) {
				t.Error("--version загрузил конфигурацию")
				return config.Config{}, config.Selection{}, errors.New("загрузка запрещена")
			},
			start: func(context.Context, config.Config, config.Selection) (service, error) {
				t.Error("--version запустил приложение")
				return nil, errors.New("запуск запрещён")
			},
		}
		status := runWithOutput(ctx, args, &stdout, &stderr, deps)
		if status != 0 || stdout.String() != buildinfo.Current().String()+"\n" || stderr.Len() != 0 {
			t.Fatalf("аргументы %q: status=%d, stdout=%q, stderr=%q", args, status, stdout.String(), stderr.String())
		}
	}
}
