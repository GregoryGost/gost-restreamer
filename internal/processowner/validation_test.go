package processowner

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

func TestBudgetsAndInvalidInputs(t *testing.T) {
	l := testLimits()
	if err := l.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, change := range []func(*Limits){func(l *Limits) { l.MaxOperations = 0 }, func(l *Limits) { l.MaxRecordBytes = 0 }, func(l *Limits) { l.MaxCommandArgs = 0 }, func(l *Limits) { l.MaxCommandBytes = 0 }, func(l *Limits) { l.MaxExtraFiles = -1 }, func(l *Limits) { l.PageSize = 0 }} {
		bad := l
		change(&bad)
		if err := bad.Validate(); !errors.Is(err, ErrInvalid) {
			t.Fatalf("limits=%+v %v", bad, err)
		}
	}
	for _, command := range []Command{{}, {Binary: "invalid\x00"}, {Binary: "invalid\xff"}, {Binary: "binary", Args: []string{"invalid\x00"}}, {Binary: "binary", Args: []string{"invalid\xff"}}, {Binary: "binary", Args: []string{strings.Repeat("x", l.MaxCommandBytes)}}, {Binary: "binary", ExtraFiles: []*os.File{nil}}} {
		if err := validateCommand(command, l); err == nil {
			t.Fatalf("invalid command accepted: %+v", command)
		}
	}
	for _, args := range [][]string{nil, {"0"}, {"1", "/tmp", "bad-scope", "00000000"}, {"1", "/tmp", strings.Repeat("a", 32), "invalid!"}} {
		if err := RunHelper(args); !errors.Is(err, ErrInvalid) {
			t.Fatalf("helper args=%v %v", args, err)
		}
	}
	for _, name := range []string{"", ".", "../escape", "/absolute", "parent/../escape", "bad\x00", "bad\\name", evidenceDirectory, evidenceDirectory + "/inside"} {
		id := testFileID(name, RegularFile)
		if err := validateIdentity(id); !errors.Is(err, ErrInvalid) {
			t.Fatalf("path %q accepted: %v", name, err)
		}
	}
	j := testJournal(t, t.TempDir())
	id := testFileID("missing", RegularFile)
	if err := j.Verify(context.Background(), id); !errors.Is(err, ErrUnknown) {
		t.Fatalf("missing evidence=%v", err)
	}
	if err := j.Remove(context.Background(), id); !errors.Is(err, ErrUnknown) {
		t.Fatalf("legacy cleanup=%v", err)
	}
	if _, err := j.RetryAcknowledgements(context.Background(), "invalid", 1); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err := j.RetryAcknowledgements(context.Background(), "", 0); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if err := writeRecord(j.root, "oversized", strings.Repeat("x", 2048), 1024); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
}
