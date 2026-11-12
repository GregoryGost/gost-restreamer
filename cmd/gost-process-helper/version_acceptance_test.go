package main

import (
	"bytes"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/buildinfo"
)

// Проверяет, что --version выводит сведения о сборке без параметров и канала связи протокола
// gost-process-helper.
func TestAcceptanceVersionBypassesHelperLifeline(t *testing.T) {
	// stdout перехватывает строку версии, чтобы проверить ранний выход без протокола helper.
	var stdout bytes.Buffer
	// err содержит результат выполнения helper с единственным аргументом --version.
	if err := run([]string{"--version"}, &stdout); err != nil {
		t.Fatal("--version потребовал параметры или lifeline helper", err)
	}
	// got содержит фактический stdout helper; want задаёт строку текущей версии с завершающим
	// переводом строки.
	if got, want := stdout.String(), buildinfo.Current().String()+"\n"; got != want {
		t.Fatalf("stdout=%q, ожидалось %q", got, want)
	}
}
