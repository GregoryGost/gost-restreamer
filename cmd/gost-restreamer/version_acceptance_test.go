package main

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/buildinfo"
	"github.com/GregoryGost/gost-restreamer/internal/config"
)

// Проверяет вывод версии с отменённым контекстом и разным порядком --config без загрузки файла и
// запуска приложения.
func TestAcceptanceVersionExitsBeforeConfigurationAndStartup(t *testing.T) {
	// args задаёт очередное допустимое расположение --version относительно --config.
	for _, args := range [][]string{{"--version"}, {"--config", "/missing/config.yaml", "--version"}, {"--version", "--config", "/missing/config.yaml"}} {
		// ctx представляет уже отменённый рабочий контекст; cancel отменяет его до запроса
		// версии.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		// stdout перехватывает строку версии; stderr накапливает нежелательную диагностику
		// раннего выхода.
		var stdout, stderr bytes.Buffer
		// deps фиксирует любое запрещённое чтение конфигурации или создание сервиса при
		// --version.
		deps := commandDependencies{
			load: func(string) (config.Config, config.Selection, error) {
				// Фиксирует запрещённое чтение конфигурации при запросе --version.

				t.Error("--version загрузил конфигурацию")
				return config.Config{}, config.Selection{}, errors.New("загрузка запрещена")
			},
			start: func(context.Context, config.Config, config.Selection) (service, error) {
				// Фиксирует запрещённый запуск приложения при запросе --version.

				t.Error("--version запустил приложение")
				return nil, errors.New("запуск запрещён")
			},
		}
		// status содержит код раннего завершения CLI для текущего набора аргументов.
		status := runWithOutput(ctx, args, &stdout, &stderr, deps)
		if status != 0 || stdout.String() != buildinfo.Current().String()+"\n" || stderr.Len() != 0 {
			t.Fatalf("аргументы %q: status=%d, stdout=%q, stderr=%q", args, status, stdout.String(), stderr.String())
		}
	}
}
