package main

import (
	"bytes"
	"errors"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/buildinfo"
	"github.com/GregoryGost/gost-restreamer/internal/processowner"
)

// Проверяет ранний вывод версии и передачу сочетания --version с лишним аргументом обычной проверке
// протокола.
func TestVersionPrecedesHelperProtocol(t *testing.T) {
	// stdout сохраняет вывод helper для сравнения версии и проверки отсутствия вывода при неверных
	// аргументах.
	var stdout bytes.Buffer
	// err содержит отказ выполнения раннего запроса версии helper.
	if err := run([]string{"--version"}, &stdout); err != nil || stdout.String() != buildinfo.Current().String()+"\n" {
		t.Fatalf("версия helper: stdout=%q err=%v", stdout.String(), err)
	}
	stdout.Reset()
	// err содержит результат проверки лишнего аргумента штатным протоколом helper.
	if err := run([]string{"--version", "extra"}, &stdout); !errors.Is(err, processowner.ErrInvalid) || stdout.Len() != 0 {
		t.Fatalf("неверные аргументы обошли протокол: stdout=%q err=%v", stdout.String(), err)
	}
}

// failedVersionWriter имитирует недоступный поток вывода версии: любая запись завершается ошибкой.
type failedVersionWriter struct{}

// Write имитирует отказ stdout, не принимая переданные байты.
func (failedVersionWriter) Write([]byte) (int, error) {
	return 0, errors.New("ошибка вывода")
}

// Проверяет, что ошибка записи версии возвращается вызывающему коду.
func TestVersionReportsOutputFailure(t *testing.T) {
	// err содержит отказ записи версии через намеренно неисправный stdout.
	if err := run([]string{"--version"}, failedVersionWriter{}); err == nil {
		t.Fatal("ошибка вывода версии потеряна")
	}
}
