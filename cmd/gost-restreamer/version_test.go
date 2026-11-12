package main

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/buildinfo"
	"github.com/GregoryGost/gost-restreamer/internal/config"
)

// Проверяет, что --version возвращает сведения о сборке раньше чтения конфигурации и создания
// приложения.
func TestVersionPrecedesConfigurationAndApplication(t *testing.T) {
	// stdout сохраняет строку версии; stderr перехватывает диагностику, которая при успешном раннем
	// выходе должна отсутствовать.
	var stdout, stderr bytes.Buffer
	// deps обнаруживает ошибочное обращение к загрузчику или конструктору при выводе версии.
	deps := commandDependencies{
		load: func(string) (config.Config, config.Selection, error) {
			// Фиксирует ошибочное обращение к конфигурации при выводе версии.

			t.Error("версия вызвала загрузку конфигурации")
			return config.Config{}, config.Selection{}, errors.New("неожиданная загрузка")
		},
		start: func(context.Context, config.Config, config.Selection) (service, error) {
			// Фиксирует ошибочный запуск сервиса при выводе версии.

			t.Error("версия запустила приложение")
			return nil, errors.New("неожиданный запуск")
		},
	}
	// status содержит код CLI после запроса версии с отсутствующим файлом конфигурации.
	status := runWithOutput(context.Background(), []string{"--version", "--config", "/missing/config.yaml"}, &stdout, &stderr, deps)
	if status != 0 || stderr.Len() != 0 || stdout.String() != buildinfo.Current().String()+"\n" {
		t.Fatalf("вывод версии: status=%d stdout=%q stderr=%q", status, stdout.String(), stderr.String())
	}
}

// failedVersionWriter имитирует отказ stdout при любой попытке вывести версию.
type failedVersionWriter struct{}

// Write имитирует отказ stdout без принятия байтов.
func (failedVersionWriter) Write([]byte) (int, error) {
	return 0, errors.New("ошибка вывода")
}

// Проверяет ненулевой код CLI и диагностику при ошибке вывода версии.
func TestVersionReportsOutputFailure(t *testing.T) {
	// stderr сохраняет диагностику отказа записи версии.
	var stderr bytes.Buffer
	// status содержит код CLI после отказа stdout; проверка требует код 1 и сообщение в stderr.
	if status := runWithOutput(context.Background(), []string{"--version"}, failedVersionWriter{}, &stderr, commandDependencies{}); status != 1 || stderr.Len() == 0 {
		t.Fatalf("ошибка вывода: status=%d stderr=%q", status, stderr.String())
	}
}
