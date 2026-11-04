package fallback

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"os/exec"
	"strings"
	"time"
)

// Command задаёт отдельные аргументы и заранее открытый входной дескриптор FD 3.
type Command struct {
	Binary string
	Args   []string
}
type RunnerLimits struct {
	Timeout                             time.Duration
	MaxArgs                             int
	MaxCommandBytes, MaxDiagnosticBytes uint64
}

func (l RunnerLimits) Validate() error {
	if l.Timeout <= 0 || l.MaxArgs <= 0 || l.MaxCommandBytes == 0 || l.MaxDiagnosticBytes == 0 {
		return ErrInvalid
	}
	return nil
}

// Runner выполняет конечный процесс; возврат подтверждает единственный Wait и закрытие копировщиков.
type Runner struct{ Limits RunnerLimits }
type RunResult struct{ Diagnostics []byte }

func (r Runner) Run(ctx context.Context, command Command, input *os.File, output io.Writer) (RunResult, error) {
	if err := r.Limits.Validate(); err != nil {
		return RunResult{}, err
	}
	if command.Binary == "" || strings.ContainsRune(command.Binary, 0) || len(command.Args) > r.Limits.MaxArgs {
		return RunResult{}, ErrInvalid
	}
	n := uint64(len(command.Binary))
	if n > r.Limits.MaxCommandBytes {
		return RunResult{}, ErrLimit
	}
	for _, arg := range command.Args {
		if strings.ContainsRune(arg, 0) || uint64(len(arg)) > r.Limits.MaxCommandBytes-n {
			return RunResult{}, ErrLimit
		}
		n += uint64(len(arg))
	}
	if n > r.Limits.MaxCommandBytes {
		return RunResult{}, ErrLimit
	}
	ctx, cancel := context.WithTimeout(ctx, r.Limits.Timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, command.Binary, command.Args...)
	if input != nil {
		cmd.ExtraFiles = []*os.File{input}
	}
	var diagnostics bytes.Buffer
	diag := &cancelWriter{writer: boundedWriter{w: &diagnostics, remaining: r.Limits.MaxDiagnosticBytes}, cancel: cancel}
	cmd.Stderr = diag
	var stdout *cancelOutput
	if output != nil {
		stdout = &cancelOutput{w: output, cancel: cancel}
		cmd.Stdout = stdout
	}
	err := cmd.Run()
	if stdout != nil && stdout.err != nil {
		err = errors.Join(stdout.err, err)
	}
	if diag.err != nil {
		return RunResult{}, errors.Join(diag.err, err)
	}
	if ctx.Err() != nil {
		err = errors.Join(ctx.Err(), err)
	}
	return RunResult{Diagnostics: bytes.Clone(diagnostics.Bytes())}, err
}

type cancelWriter struct {
	writer boundedWriter
	cancel context.CancelFunc
	err    error
}

func (w *cancelWriter) Write(p []byte) (int, error) {
	n, e := w.writer.Write(p)
	if e != nil {
		w.err = e
		w.cancel()
	}
	return n, e
}

type cancelOutput struct {
	w      io.Writer
	cancel context.CancelFunc
	err    error
}

func (w *cancelOutput) Write(p []byte) (int, error) {
	n, e := w.w.Write(p)
	if e != nil {
		w.err = e
		w.cancel()
	}
	return n, e
}
