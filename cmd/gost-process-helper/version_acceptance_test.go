package main

import (
	"bytes"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/buildinfo"
)

func TestAcceptanceVersionBypassesHelperLifeline(t *testing.T) {
	var stdout bytes.Buffer
	if err := run([]string{"--version"}, &stdout); err != nil {
		t.Fatal("--version потребовал параметры или lifeline helper", err)
	}
	if got, want := stdout.String(), buildinfo.Current().String()+"\n"; got != want {
		t.Fatalf("stdout=%q, ожидалось %q", got, want)
	}
}
