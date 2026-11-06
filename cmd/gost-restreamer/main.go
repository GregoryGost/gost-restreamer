package main

import (
	"bytes"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"os/signal"
	"syscall"

	"github.com/GregoryGost/gost-restreamer/internal/app"
	"github.com/GregoryGost/gost-restreamer/internal/config"
)

type service interface {
	Wait(context.Context) error
	Shutdown(context.Context) error
	LogError(error)
}
type commandDependencies struct {
	load  func(string) (config.Config, config.Selection, error)
	start func(context.Context, config.Config, config.Selection) (service, error)
}

func main() { os.Exit(mainCode()) }
func mainCode() int {
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return run(ctx, os.Args[1:], os.Stderr)
}
func run(ctx context.Context, args []string, stderr io.Writer) int {
	return runWith(ctx, args, stderr, commandDependencies{})
}
func runWith(ctx context.Context, args []string, stderr io.Writer, d commandDependencies) int {
	var diagnostics bytes.Buffer
	flags := flag.NewFlagSet("gost-restreamer", flag.ContinueOnError)
	flags.SetOutput(&diagnostics)
	path := ""
	flags.Func("config", "Путь к единственному файлу YAML.", func(value string) error {
		if value == "" {
			return errors.New("путь конфигурации пуст")
		}
		path = value
		return nil
	})
	if err := flags.Parse(args); err != nil {
		_, _ = stderr.Write(diagnostics.Bytes())
		if errors.Is(err, flag.ErrHelp) {
			return 0
		}
		return 2
	}
	if flags.NArg() != 0 {
		_, _ = fmt.Fprintln(stderr, "позиционные аргументы запрещены")
		return 2
	}
	if d.load == nil {
		d.load = config.LoadSelected
	}
	if d.start == nil {
		d.start = func(ctx context.Context, c config.Config, s config.Selection) (service, error) {
			a, err := app.StartSelected(ctx, c, s)
			if a == nil {
				return nil, err
			}
			return a, err
		}
	}
	c, selected, err := d.load(path)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	startup, cancel := context.WithTimeout(ctx, c.Lifecycle.StartupTimeout)
	type started struct {
		app service
		err error
	}
	result := make(chan started, 1)
	go func() { a, e := d.start(startup, c, selected); result <- started{a, e} }()
	var current started
	var shutdown context.Context
	var stop context.CancelFunc
	select {
	case current = <-result:
		cancel()
	case <-startup.Done():
		trigger := startup.Err()
		cancel()
		shutdown, stop = context.WithTimeout(context.Background(), c.Lifecycle.ShutdownTimeout)
		defer stop()
		// Отмена запуска начинает отдельное ограниченное ожидание частичной очистки.
		select {
		case current = <-result:
			current.err = errors.Join(current.err, trigger)
		case <-shutdown.Done():
			return 1
		}
	}
	if current.app == nil {
		if current.err != nil && shutdown == nil {
			_, _ = fmt.Fprintln(stderr, current.err)
		}
		return 1
	}
	failed := current.err != nil
	if failed {
		current.app.LogError(current.err)
	} else {
		err = current.app.Wait(ctx)
		if err != nil && !errors.Is(err, context.Canceled) {
			current.app.LogError(err)
			failed = true
		}
	}
	// Внешний срок ограничивает даже синхронный Close, не завершающийся по ctx.
	if shutdown == nil {
		shutdown, stop = context.WithTimeout(context.Background(), c.Lifecycle.ShutdownTimeout)
		defer stop()
	}
	closed := make(chan error, 1)
	go func() { closed <- current.app.Shutdown(shutdown) }()
	select {
	case err = <-closed:
		if err != nil {
			failed = true
		}
	case <-shutdown.Done():
		return 1
	}
	if failed {
		return 1
	}
	return 0
}
