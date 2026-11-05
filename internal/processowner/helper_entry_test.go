package processowner

import (
	"io"
	"os"
	"os/exec"
	"os/signal"
	"testing"
	"time"
)

func TestGuardianNativeResultsAndEscalation(t *testing.T) {
	r := testRegistry(t, buildHelper(t))
	s := testScope(t, r)
	ctx := testContext(t)
	trueBinary, err := exec.LookPath("true")
	if err != nil {
		t.Fatal(err)
	}
	falseBinary, err := exec.LookPath("false")
	if err != nil {
		t.Fatal(err)
	}
	for _, binary := range []string{trueBinary, falseBinary, "/nonexistent/processowner-binary"} {
		result, err := s.Run(ctx, Command{Binary: binary})
		if !result.Joined || (binary == trueBinary && err != nil) || (binary != trueBinary && err == nil) {
			t.Fatalf("%s: %+v %v", binary, result, err)
		}
	}
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = reader.Close() }()
	p, err := s.Start(ctx, Command{Binary: os.Args[0], Args: []string{"-test.run=^TestInterruptIgnoringNative$", "--", "--interrupt-ignoring-native"}, ExtraFiles: []*os.File{writer}})
	if err != nil {
		t.Fatal(err)
	}
	_ = writer.Close()
	var ready [1]byte
	if _, err = io.ReadFull(reader, ready[:]); err != nil {
		t.Fatal(err)
	}
	p.Cancel()
	result, err := p.Wait(ctx)
	if err != nil || !result.Joined || result.Err == nil || result.ExitCode != -1 {
		t.Fatalf("escalation=%+v %v", result, err)
	}
}

func TestInterruptIgnoringNative(t *testing.T) {
	found := false
	for _, arg := range os.Args {
		if arg == "--interrupt-ignoring-native" {
			found = true
		}
	}
	if !found {
		return
	}
	signal.Ignore(os.Interrupt)
	ready := os.NewFile(3, "native-ready")
	if ready == nil {
		os.Exit(97)
	}
	_, _ = ready.Write([]byte{1})
	_ = ready.Close()
	timer := time.NewTimer(time.Hour)
	<-timer.C
}
