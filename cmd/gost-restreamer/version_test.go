package main

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/buildinfo"
	"github.com/GregoryGost/gost-restreamer/internal/config"
)

func TestVersionPrecedesConfigurationAndApplication(t *testing.T) {
	var stdout, stderr bytes.Buffer
	deps := commandDependencies{
		load: func(string) (config.Config, config.Selection, error) {
			t.Error("версия вызвала загрузку конфигурации")
			return config.Config{}, config.Selection{}, errors.New("неожиданная загрузка")
		},
		start: func(context.Context, config.Config, config.Selection) (service, error) {
			t.Error("версия запустила приложение")
			return nil, errors.New("неожиданный запуск")
		},
	}
	status := runWithOutput(context.Background(), []string{"--version", "--config", "/missing/config.yaml"}, &stdout, &stderr, deps)
	if status != 0 || stderr.Len() != 0 || stdout.String() != buildinfo.Current().String()+"\n" {
		t.Fatalf("вывод версии: status=%d stdout=%q stderr=%q", status, stdout.String(), stderr.String())
	}
}

type failedVersionWriter struct{}

func (failedVersionWriter) Write([]byte) (int, error) {
	return 0, errors.New("ошибка вывода")
}

func TestVersionReportsOutputFailure(t *testing.T) {
	var stderr bytes.Buffer
	if status := runWithOutput(context.Background(), []string{"--version"}, failedVersionWriter{}, &stderr, commandDependencies{}); status != 1 || stderr.Len() == 0 {
		t.Fatalf("ошибка вывода: status=%d stderr=%q", status, stderr.String())
	}
}
