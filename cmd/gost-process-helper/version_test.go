package main

import (
	"bytes"
	"errors"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/buildinfo"
	"github.com/GregoryGost/gost-restreamer/internal/processowner"
)

func TestVersionPrecedesHelperProtocol(t *testing.T) {
	var stdout bytes.Buffer
	if err := run([]string{"--version"}, &stdout); err != nil || stdout.String() != buildinfo.Current().String()+"\n" {
		t.Fatalf("версия helper: stdout=%q err=%v", stdout.String(), err)
	}
	stdout.Reset()
	if err := run([]string{"--version", "extra"}, &stdout); !errors.Is(err, processowner.ErrInvalid) || stdout.Len() != 0 {
		t.Fatalf("неверные аргументы обошли протокол: stdout=%q err=%v", stdout.String(), err)
	}
}

type failedVersionWriter struct{}

func (failedVersionWriter) Write([]byte) (int, error) {
	return 0, errors.New("ошибка вывода")
}

func TestVersionReportsOutputFailure(t *testing.T) {
	if err := run([]string{"--version"}, failedVersionWriter{}); err == nil {
		t.Fatal("ошибка вывода версии потеряна")
	}
}
