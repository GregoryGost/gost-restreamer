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
	"github.com/GregoryGost/gost-restreamer/internal/buildinfo"
	"github.com/GregoryGost/gost-restreamer/internal/config"
)

// service задаёт операции ожидания, остановки и журналирования приложения, необходимые CLI.
type service interface {

	// Wait ждёт отмены рабочего контекста либо отказа обслуживания, не освобождая общие ресурсы сервиса.
	Wait(context.Context) error

	// Shutdown запрашивает остановку и ждёт освобождения принадлежащих сервису ресурсов в переданном
	// контексте; CLI дополнительно ограничивает сам вызов внешним сроком.
	Shutdown(context.Context) error

	// LogError передаёт ошибку единственному журналу сервиса без создания второго получателя stderr.
	LogError(error)
}

// commandDependencies позволяет CLI подставлять загрузку конфигурации и запуск приложения при проверках.
type commandDependencies struct {
	// load читает выбранную конфигурацию и возвращает сведения об использованном файле вместе с
	// ошибкой загрузки.
	load func(string) (config.Config, config.Selection, error)
	// start создаёт сервис по конфигурации; ненулевой частичный сервис сохраняется для последующей
	// остановки даже при ошибке.
	start func(context.Context, config.Config, config.Selection) (service, error)
}

// main завершает процесс кодом, полученным после обработки CLI и остановки приложения.
func main() { os.Exit(mainCode()) }

// mainCode связывает SIGINT и SIGTERM с контекстом CLI и освобождает обработчики сигналов после его
// завершения.
func mainCode() int {
	// ctx отменяется при SIGINT или SIGTERM; stop освобождает регистрацию сигналов после завершения
	// CLI.
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	return run(ctx, os.Args[1:], os.Stderr)
}

// run обрабатывает CLI со штатными загрузчиком конфигурации и сборкой приложения, направляя
// диагностику в переданный поток вывода.
func run(ctx context.Context, args []string, stderr io.Writer) int {
	return runWith(ctx, args, stderr, commandDependencies{})
}

// runWith запускает CLI с подставляемыми зависимостями и настоящим stdout.
func runWith(ctx context.Context, args []string, stderr io.Writer, d commandDependencies) int {
	return runWithOutput(ctx, args, os.Stdout, stderr, d)
}

// runWithOutput разбирает аргументы до открытия приложения и обрабатывает ранний вывод версии. Запуск
// и завершение ограничены раздельными сроками; ненулевой частичный сервис сохраняется для очистки даже
// при ошибке запуска.
func runWithOutput(ctx context.Context, args []string, stdout, stderr io.Writer, d commandDependencies) int {
	// diagnostics накапливает сообщения разбора аргументов для передачи в выбранный stderr.
	var diagnostics bytes.Buffer
	// flags разбирает допустимые параметры CLI, возвращая ошибки вместо завершения процесса.
	flags := flag.NewFlagSet("gost-restreamer", flag.ContinueOnError)
	flags.SetOutput(&diagnostics)
	// path сохраняет точный явно переданный путь --config; пустая строка оставляет выбор файла
	// загрузчику.
	path := ""
	// showVersion указывает значение флага раннего вывода версии до открытия приложения.
	showVersion := flags.Bool("version", false, "Вывести версию сборки и завершить работу.")
	flags.Func("config", "Путь к единственному файлу YAML.", func(value string) error {
		// Сохраняет явно переданный путь YAML и отклоняет пустое значение --config.

		if value == "" {
			return errors.New("путь конфигурации пуст")
		}
		path = value
		return nil
	})
	// err содержит ошибку разбора параметров CLI, включая отдельный штатный запрос справки.
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
	if *showVersion {
		// err содержит отказ записи версии в stdout, при котором CLI завершается с кодом 1.
		if _, err := fmt.Fprintln(stdout, buildinfo.Current()); err != nil {
			_, _ = fmt.Fprintln(stderr, err)
			return 1
		}
		return 0
	}
	if d.load == nil {
		d.load = config.LoadSelected
	}
	if d.start == nil {
		d.start = func(ctx context.Context, c config.Config, s config.Selection) (service, error) {
			// Запускает Application с выбранной конфигурацией и сохраняет nil при отсутствии возвращённого
			// владельца.

			// a содержит созданное приложение либо его частичного владельца; err содержит
			// результат запуска приложения.
			a, err := app.StartSelected(ctx, c, s)
			if a == nil {
				return nil, err
			}
			return a, err
		}
	}
	// c содержит загруженные настройки сервера; selected описывает выбранный файл; err содержит
	// отказ их загрузки.
	c, selected, err := d.load(path)
	if err != nil {
		_, _ = fmt.Fprintln(stderr, err)
		return 1
	}
	// startup ограничивает создание приложения настроенным сроком; cancel освобождает его таймер и
	// отменяет незавершённый запуск.
	startup, cancel := context.WithTimeout(ctx, c.Lifecycle.StartupTimeout)
	// started переносит сервис и ошибку запуска из горутины конструктора в управляющий цикл CLI.
	type started struct {
		// app хранит возвращённого владельца сервиса, которого CLI обязан остановить, в том
		// числе после частичного запуска.
		app service
		// err хранит ошибку запуска, включая присоединённую причину истечения внешнего срока.
		err error
	}
	// result передаёт единственный итог конструктора без блокировки отправителя после выхода CLI по
	// сроку.
	result := make(chan started, 1)
	go func() {
		// Передаёт результат запуска через буферизованный канал, чтобы внешний срок CLI не зависел от возврата
		// конструктора.
		// a содержит результат создания сервиса; e содержит ошибку того же запуска, передаваемую
		// вместе с владельцем.
		a, e := d.start(startup, c, selected)
		result <- started{a, e}
	}()
	// current сохраняет полученного владельца и итог запуска для ожидания и обязательной остановки.
	var current started
	// shutdown хранит отдельный ограниченный контекст очистки, независимый от отменённого рабочего
	// контекста.
	var shutdown context.Context
	// stop освобождает таймер контекста очистки после окончания CLI.
	var stop context.CancelFunc
	select {
	case current = <-result:
		cancel()
	// trigger сохраняет причину отмены запуска до освобождения его контекста.
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
	// failed учитывает ошибки запуска, обслуживания и остановки при выборе итогового кода CLI.
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
	// closed передаёт итог остановки, пока CLI отдельно ограничивает ожидание внешним сроком.
	closed := make(chan error, 1)
	go func() {
		// Передаёт результат остановки в буферизованный канал, пока CLI отдельно следит за внешним сроком
		// завершения.
		closed <- current.app.Shutdown(shutdown)
	}()
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
