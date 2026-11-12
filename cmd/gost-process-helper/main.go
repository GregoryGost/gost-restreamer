package main

import (
	"fmt"
	"io"
	"os"

	"github.com/GregoryGost/gost-restreamer/internal/buildinfo"
	"github.com/GregoryGost/gost-restreamer/internal/processowner"
)

// main запускает протокол gost-process-helper или вывод версии; при ошибке пишет её в stderr и
// завершает процесс с кодом 1.
func main() {
	// err содержит отказ вывода версии или выполнения протокола helper, определяющий ненулевой код
	// процесса.
	if err := run(os.Args[1:], os.Stdout); err != nil {
		_, _ = fmt.Fprintln(os.Stderr, err)
		os.Exit(1)
	}
}

// run выводит версию только для единственного аргумента --version; остальные аргументы передаёт
// протоколу processowner без изменения.
func run(args []string, stdout io.Writer) error {
	if len(args) == 1 && args[0] == "--version" {
		// err возвращает отказ записи строки версии в выбранный stdout.
		_, err := fmt.Fprintln(stdout, buildinfo.Current())
		return err
	}
	return processowner.RunHelper(args)
}
