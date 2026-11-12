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

	"github.com/GregoryGost/gost-restreamer/internal/processowner"
)

// Command задаёт отдельные аргументы и заранее открытый входной дескриптор FD 3.
type Command struct {
	// Binary задаёт путь или имя запускаемой программы.
	Binary string
	// Args содержит отдельные аргументы запуска без интерпретации командной оболочкой.
	Args []string
}

// RunnerLimits ограничивает время процесса и объёмы его команды и диагностики.
type RunnerLimits struct {
	// Timeout задаёт максимальное время выполнения команды.
	Timeout time.Duration
	// MaxArgs ограничивает число аргументов команды.
	MaxArgs int
	// MaxCommandBytes ограничивает суммарную длину программы и аргументов в байтах.
	// MaxDiagnosticBytes ограничивает объём принимаемой диагностики в байтах.
	MaxCommandBytes, MaxDiagnosticBytes uint64
}

// Validate требует положительный срок процесса и ненулевые бюджеты аргументов, команды и диагностики.
func (l RunnerLimits) Validate() error {
	if l.Timeout <= 0 || l.MaxArgs <= 0 || l.MaxCommandBytes == 0 || l.MaxDiagnosticBytes == 0 {
		return ErrInvalid
	}
	return nil
}

// Runner выполняет команду с ограниченными сроком и диагностикой. При наличии Scope он ждёт
// вспомогательный процесс и его копировщиков; завершение целевого процесса должно быть подтверждено
// сохранённым результатом, иначе Run возвращает ErrCleanupPending.
type Runner struct {
	// Limits содержит конечные ограничения запуска и вывода команды.
	Limits RunnerLimits
	// Scope при наличии запускает команду через журналируемого владельца процессов и проверяет
	// подтверждение завершения.
	Scope *processowner.Scope
}

// RunResult возвращает собранную диагностику выполненной команды.
type RunResult struct { /* Diagnostics содержит отдельную копию ограниченного диагностического вывода процесса. */
	Diagnostics []byte
}

// Run проверяет команду до запуска, передаёт input как FD 3 и ограничивает срок и диагностику. При
// отмене или ошибке записи запрашивает остановку. Без Scope возврат дожидается cmd.Run; со Scope —
// завершения вспомогательного процесса и его копировщиков. Если результат не подтверждает завершение
// целевого процесса, возвращается ErrCleanupPending; записи владения сохраняются для дальнейшей сверки.
func (r Runner) Run(ctx context.Context, command Command, input *os.File, output io.Writer) (RunResult, error) {
	// err сообщает о некорректных ограничениях запуска.
	if err := r.Limits.Validate(); err != nil {
		return RunResult{}, err
	}
	if command.Binary == "" || strings.ContainsRune(command.Binary, 0) || len(command.Args) > r.Limits.MaxArgs {
		return RunResult{}, ErrInvalid
	}
	// n считает суммарное число байтов имени программы и проверенных аргументов.
	n := uint64(len(command.Binary))
	if n > r.Limits.MaxCommandBytes {
		return RunResult{}, ErrLimit
	}
	// arg содержит очередной аргумент команды для проверки нулевого байта и общего размера.
	for _, arg := range command.Args {
		if strings.ContainsRune(arg, 0) || uint64(len(arg)) > r.Limits.MaxCommandBytes-n {
			return RunResult{}, ErrLimit
		}
		n += uint64(len(arg))
	}
	if n > r.Limits.MaxCommandBytes {
		return RunResult{}, ErrLimit
	}
	// cancel отменяет ограниченный по времени запуск, в том числе при отказе записи вывода.
	ctx, cancel := context.WithTimeout(ctx, r.Limits.Timeout)
	defer cancel()
	// diagnostics накапливает диагностический вывод в пределах заданного бюджета.
	var diagnostics bytes.Buffer
	// diag ограничивает диагностический вывод и отменяет процесс при отказе его записи.
	diag := &cancelWriter{writer: boundedWriter{w: &diagnostics, remaining: r.Limits.MaxDiagnosticBytes}, cancel: cancel}
	// stdout при наличии получателя передаёт ему вывод и сохраняет ошибку записи.
	var stdout *cancelOutput
	if output != nil {
		stdout = &cancelOutput{w: output, cancel: cancel}
	}
	// err объединяет ошибки запуска, ожидания, вывода, отмены и неподтверждённого завершения.
	var err error
	if r.Scope != nil {
		// c содержит команду для запуска через Scope с заранее настроенной диагностикой.
		c := processowner.Command{Binary: command.Binary, Args: command.Args, Stderr: diag}
		if input != nil {
			c.ExtraFiles = []*os.File{input}
		}
		if stdout != nil {
			c.Stdout = stdout
		}
		// process представляет запущенную операцию владельца процессов. startErr сообщает об
		// отказе её запуска.
		process, startErr := r.Scope.Start(ctx, c)
		err = startErr
		if err == nil {
			// Дедлайн закрывает lifeline. Wait ждёт вспомогательный процесс, а Joined отдельно подтверждает
			// сохранённый результат завершения целевого процесса.
			// result содержит сохранённый результат завершения целевого процесса. waitErr
			// сообщает об отказе ожидания вспомогательного процесса.
			result, waitErr := process.Wait(context.Background())
			err = errors.Join(result.Err, waitErr)
			if !result.Joined {
				err = errors.Join(err, ErrCleanupPending)
			}
		}
	} else {
		// cmd содержит непосредственную команду с отменой по контексту при отсутствии Scope.
		cmd := exec.CommandContext(ctx, command.Binary, command.Args...)
		if input != nil {
			cmd.ExtraFiles = []*os.File{input}
		}
		cmd.Stderr = diag
		if stdout != nil {
			cmd.Stdout = stdout
		}
		err = cmd.Run()
	}
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

// cancelWriter ограничивает диагностический вывод и отменяет процесс при отказе записи.
type cancelWriter struct {
	// writer передаёт диагностику в ограниченный по байтам приёмник.
	writer boundedWriter
	// cancel запрашивает отмену запуска при отказе записи диагностики.
	cancel context.CancelFunc
	// err сохраняет отказ записи диагностики для итогового результата Run.
	err error
}

// Write ограничивает диагностику и отменяет процесс при ошибке её записи, сохраняя ошибку для
// результата Run.
func (w *cancelWriter) Write(p []byte) (int, error) {
	// n содержит число записанных байтов диагностики. e сообщает об отказе записи или превышении
	// бюджета.
	n, e := w.writer.Write(p)
	if e != nil {
		w.err = e
		w.cancel()
	}
	return n, e
}

// cancelOutput передаёт стандартный вывод и отменяет процесс при отказе получателя.
type cancelOutput struct {
	// w содержит получателя стандартного вывода процесса.
	w io.Writer
	// cancel запрашивает отмену запуска при отказе записи стандартного вывода.
	cancel context.CancelFunc
	// err сохраняет отказ получателя для итогового результата Run.
	err error
}

// Write передаёт вывод получателю и отменяет процесс при ошибке записи, сохраняя её для результата
// Run.
func (w *cancelOutput) Write(p []byte) (int, error) {
	// n содержит число переданных получателю байтов. e сообщает об отказе записи стандартного
	// вывода.
	n, e := w.w.Write(p)
	if e != nil {
		w.err = e
		w.cancel()
	}
	return n, e
}
