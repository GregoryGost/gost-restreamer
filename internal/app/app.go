// Package app собирает сервис и владеет временем жизни общих ресурсов.
package app

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"sync"

	"github.com/GregoryGost/gost-restreamer/internal/api"
	"github.com/GregoryGost/gost-restreamer/internal/buildinfo"
	"github.com/GregoryGost/gost-restreamer/internal/config"
	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/fallback"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/rtmp"
	"github.com/GregoryGost/gost-restreamer/internal/monitoring"
	"github.com/GregoryGost/gost-restreamer/internal/monitoring/gpu"
	"github.com/GregoryGost/gost-restreamer/internal/monitoring/logging"
	"github.com/GregoryGost/gost-restreamer/internal/processowner"
	"github.com/GregoryGost/gost-restreamer/internal/session"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
)

// dependencies подставляет границы приобретения ресурсов для проверок частичного запуска приложения.
type dependencies struct {
	// listen открывает слушатель HTTP или ingest в ограниченном контексте запуска.
	listen func(context.Context, string, string) (net.Listener, error)
	// logger создаёт единственный журнал приложения с переданными бюджетами.
	logger func(logging.Config) (*logging.Sink, error)
	// openStore открывает общее хранилище по пути и явным настройкам SQLite.
	openStore func(context.Context, string, storage.Options) (*storage.Store, error)
	// afterAcquire выполняет необязательную тестовую проверку сразу после приобретения указанного
	// ресурса.
	afterAcquire func(string, *Application) error
	// warning переносит сведения о двух стандартных конфигурационных файлах в обязательное стартовое
	// предупреждение.
	warning *config.Warning
}

// Application остаётся владельцем ресурсов, пока Done не подтвердит полную остановку.
type Application struct {
	// config сохраняет проверенный неизменяемый снимок настроек на срок жизни приложения.
	config config.Config
	// sink принимает структурированную диагностику всех компонентов и закрывается последним.
	sink *logging.Sink
	// store владеет общими SQLite, миграциями и RuntimeAuthority до завершения зависимых
	// пользователей.
	store *storage.Store
	// core предоставляет доменные операции поверх единственного общего store.
	core *core.Service
	// files обслуживает оригиналы и варианты заглушек и закрывается после подготовки и
	// медиакомпонентов.
	files *fallback.FileStore
	// processes учитывает физическое владение процессами и закрывается после завершения их
	// пользователей.
	processes *processowner.Registry
	// preparation владеет загрузкой и фоновой подготовкой поколений заглушек.
	preparation *fallback.Manager
	// manager владеет жизненным циклом сессий и связывает их входы, группы и отправителей.
	manager *session.Manager
	// ingest принимает публикации OBS и связывает их с готовыми сессиями менеджера.
	ingest *rtmp.Server
	// gpu связывает нативный сборщик GPU с общей системой наблюдений.
	gpu *monitoring.GPUCollector
	// api обслуживает REST и SSE и подтверждает завершение принятых обработчиков при остановке.
	api *api.API
	// http владеет HTTP-транспортом и закрытием соединений независимо от присоединения
	// API-обработчиков.
	http *http.Server
	// httpListener содержит приобретённый HTTP-слушатель; ingestListener содержит слушатель OBS,
	// передаваемый во владение RTMP-транспорту при запуске Serve.
	httpListener, ingestListener net.Listener
	// serveCancel отменяет отдельный срок жизни сетевого обслуживания при остановке приложения.
	serveCancel context.CancelFunc
	// httpServe наблюдает результат HTTP Serve; ingestServe наблюдает результат RTMP Serve для Wait
	// и остановки.
	httpServe, ingestServe *operation
	// owners сохраняет обязанности освобождения каждого приобретённого ресурса, включая частичный
	// запуск.
	owners shutdownOwners
	// gate последовательно допускает вызовы Shutdown к изменению владельцев и накопленных ошибок.
	gate chan struct{}
	// done подтверждает окончательное освобождение всех приобретённых ресурсов приложения.
	done chan struct{}
	// doneOnce обеспечивает однократное закрытие done после полной остановки.
	doneOnce sync.Once
	// errors сохраняет ошибки завершённых операций очистки для последующих вызовов Shutdown.
	errors []error
}

// shutdownOwners хранит отдельных владельцев остановки в порядке зависимостей общего приложения.
type shutdownOwners struct {
	// api присоединяет обработчики; transport останавливает HTTP-соединения; manager завершает
	// сессии; ingest останавливает вход OBS; preparation завершает задания; gpu завершает нативный
	// сборщик.
	api, transport, manager, ingest, preparation, gpu *stopOwner
	// httpListener закрывает ещё не переданный HTTP-транспорту слушатель; ingestListener закрывает
	// ещё не переданный RTMP-транспорту слушатель.
	httpListener, ingestListener *stopOwner
	// registry освобождает учёт процессов; files закрывает файловое хранилище; store закрывает
	// SQLite; logger завершает последний получатель диагностики.
	registry, files, store, logger *stopOwner
}

// Start собирает и запускает сервис с проверенной конфигурацией. Ошибка после начала приобретения
// ресурсов возвращается вместе с ненулевым Application, даже если ограниченная очистка ещё не
// завершена. Вызывающий код сохраняет этот результат до закрытия Done и при необходимости повторяет
// Shutdown с независимым ограниченным контекстом.
func Start(ctx context.Context, c config.Config) (*Application, error) {
	return start(ctx, c, dependencies{})
}

// StartSelected передаёт предупреждение выбора файла единственному JSON-журналу до привязки слушателей.
// Частичный Application при ошибке обрабатывается по контракту Start.
func StartSelected(ctx context.Context, c config.Config, s config.Selection) (*Application, error) {
	return start(ctx, c, dependencies{warning: s.Warning})
}

// start проверяет настройки и gost-process-helper до приобретения общих ресурсов, затем ограничивает
// срок сборки. При отказе пытается освободить приобретённые ресурсы и возвращает частичный Application
// вместе с ошибкой запуска и очистки.
func start(ctx context.Context, c config.Config, d dependencies) (*Application, error) {
	// err содержит отказ полной проверки настроек до приобретения ресурсов приложения.
	if err := c.Validate(); err != nil {
		return nil, err
	}
	// helper содержит проверенный абсолютный путь gost-process-helper; err содержит отказ его
	// разрешения.
	helper, err := resolveHelper(c.Executables.Helper)
	if err != nil {
		return nil, err
	}
	if ctx == nil {
		return nil, core.ErrInvalid
	}
	// startup ограничивает сборку приложения общим сроком; cancel освобождает таймер после
	// завершения запуска.
	startup, cancel := context.WithTimeout(ctx, c.Lifecycle.StartupTimeout)
	defer cancel()
	if err = startup.Err(); err != nil {
		return nil, err
	}
	if d.listen == nil {
		d.listen = (&net.ListenConfig{}).Listen
	}
	if d.logger == nil {
		d.logger = logging.NewStderr
	}
	if d.openStore == nil {
		d.openStore = storage.Open
	}
	// a сохраняет владельцев приобретаемых ресурсов и возвращается даже при незавершённой частичной
	// очистке.
	a := &Application{config: c, gate: make(chan struct{}, 1), done: make(chan struct{})}
	a.gate <- struct{}{}
	err = a.acquire(startup, helper, d)
	if err != nil {
		a.LogError(err)
		// shutdown задаёт независимый срок частичной очистки; stop освобождает его таймер после
		// попытки остановки.
		shutdown, stop := context.WithTimeout(context.Background(), c.Lifecycle.ShutdownTimeout)
		// cleanup содержит итог освобождения уже приобретённого подграфа после отказа запуска.
		cleanup := a.Shutdown(shutdown)
		stop()
		return a, errors.Join(err, cleanup)
	}
	return a, nil
}

// resolveHelper находит gost-process-helper через PATH либо заданный путь и возвращает абсолютный путь
// к обычному исполняемому файлу. Ошибка проверки останавливает запуск до открытия ресурсов приложения.
func resolveHelper(name string) (string, error) {
	// resolved содержит найденный путь helper; err содержит отказ поиска исполняемого файла.
	resolved, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("разрешить helper: %w", err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	// info содержит сведения о найденном helper для проверки обычного исполняемого файла.
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("helper: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return "", fmt.Errorf("helper должен быть обычным исполняемым файлом")
	}
	return resolved, nil
}

// acquire последовательно приобретает ресурсы и сразу закрепляет их владельцев для очистки при
// последующем отказе. HTTP и приём OBS начинают обслуживание после связывания всех компонентов; с
// этого момента слушатели принадлежат транспортам.
func (a *Application) acquire(ctx context.Context, helper string, d dependencies) error {
	// checkpoint проверяет отмену и вызывает необязательную тестовую границу после приобретения
	// каждого ресурса.
	checkpoint := func(stage string) error {
		// Проверяет отмену запуска и вызывает контрольную точку после приобретения очередного ресурса.

		// err содержит причину отмены сборки, проверяемую до вызова очередной контрольной точки.
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.afterAcquire != nil {
			return d.afterAcquire(stage, a)
		}
		return nil
	}
	// err переносит результат последовательного приобретения и настройки общих компонентов.
	var err error
	a.sink, err = d.logger(logging.Config(a.config.Logging))
	if a.sink != nil {
		a.owners.logger = &stopOwner{name: "logger", close: func(ctx context.Context) stopResult {
			// Закрывает журнал и отдельно проверяет Done, поскольку ошибка Close сама по себе не подтверждает
			// завершение.

			// err содержит результат Close журнала, отдельно от подтверждения его Done.
			err := a.sink.Close(ctx)
			return stopResult{err, isClosed(a.sink.Done())}
		}}
	}
	if err != nil {
		return err
	}
	if err = checkpoint("logger"); err != nil {
		return err
	}
	// w содержит сведения о конфликте конфигурационных файлов для обязательного предупреждения до
	// bind.
	if w := d.warning; w != nil {
		a.sink.LogConfigFilePriorityWarning(w.YAMLPath, w.YMLPath, w.Selected)
	}
	a.httpListener, err = d.listen(ctx, "tcp", a.config.HTTP.Address)
	if a.httpListener != nil {
		a.owners.httpListener = listenerOwner("http_listener", a.httpListener)
	}
	if err != nil {
		return fmt.Errorf("HTTP bind: %w", err)
	}
	if err = checkpoint("http_listener"); err != nil {
		return err
	}
	a.ingestListener, err = d.listen(ctx, "tcp", a.config.IngestAddress)
	if a.ingestListener != nil {
		a.owners.ingestListener = listenerOwner("ingest_listener", a.ingestListener)
	}
	if err != nil {
		return fmt.Errorf("ingest bind: %w", err)
	}
	if err = checkpoint("ingest_listener"); err != nil {
		return err
	}
	a.store, err = d.openStore(ctx, a.config.DatabasePath(), a.config.StorageOptions())
	if a.store != nil {
		a.owners.store = sharedOwner("store", func(context.Context) error {
			// Закрывает общее хранилище после завершения его пользователей.
			return a.store.Close()
		})
	}
	if err != nil {
		return err
	}
	if err = checkpoint("store"); err != nil {
		return err
	}
	a.core, err = core.NewService(a.store, a.store.Incarnation())
	if err != nil {
		return err
	}
	if err = a.core.ConfigureTelemetry(ctx, a.config.TelemetryLimits()); err != nil {
		return err
	}
	a.files, err = fallback.OpenFileStoreWithLimits(a.config.Paths.Data, processowner.Limits(a.config.Process.Limits))
	if a.files != nil {
		a.owners.files = sharedOwner("files", func(context.Context) error {
			// Освобождает файловое хранилище после завершения подготовки и медиакомпонентов.
			return a.files.Close()
		})
	}
	if err != nil {
		return err
	}
	if err = checkpoint("files"); err != nil {
		return err
	}
	// registry содержит созданный реестр процессов, сохраняемый в Application до запуска его
	// пользователей.
	registry, err := processowner.New(a.config.ProcessConfig(helper))
	a.processes = registry
	if registry != nil {
		a.owners.registry = sharedOwner("registry", func(context.Context) error {
			// Закрывает реестр владения процессами после завершения зависимых компонентов.
			return registry.Close()
		})
	}
	if err != nil {
		return err
	}
	if err = checkpoint("processes"); err != nil {
		return err
	}
	// fc дополняет настройки подготовки общим реестром физических процессов.
	fc := a.config.FallbackConfig()
	fc.ProcessRegistry = registry
	a.preparation, err = fallback.NewManager(a.core, a.files, fc)
	if a.preparation != nil {
		a.owners.preparation = newStopOwner("preparation", a.preparation.Close)
	}
	if err != nil {
		return err
	}
	if err = checkpoint("preparation"); err != nil {
		return err
	}
	// Порт допуска связывается с Manager до публикации слушателя и затем остаётся неизменным.
	// manager связывает допуск входного RTMP с фактически созданным менеджером до публикации
	// слушателя.
	var manager *session.Manager
	// admissionTimeout содержит заранее проверенную сумму существующих сроков допуска OBS.
	admissionTimeout, err := a.config.OBSAdmissionTimeout()
	if err != nil {
		return err
	}
	a.ingest, err = rtmp.NewServer(rtmp.Options{Core: a.core, Route: rtmp.ConventionalRoute, Admit: func(ctx context.Context, streamID core.ID, key string) (core.Session, *media.Router, error) {
		// Передаёт ключ и срок публикации Manager после завершения сборки приложения.
		return manager.AdmitOBS(ctx, streamID, key)
	}, AdmissionTimeout: admissionTimeout, Limits: rtmp.Limits(a.config.Ingress), Logger: a.sink.Logger()})
	if a.ingest != nil {
		a.owners.ingest = newStopOwner("ingest", a.ingest.Shutdown)
	}
	if err != nil {
		return err
	}
	if err = checkpoint("ingest"); err != nil {
		return err
	}
	// sc дополняет настройки менеджера рабочими зависимостями, единым журналом и открытием
	// проверенных файлов.
	sc := a.config.SessionConfig()
	sc.Core = a.core
	sc.Files = a.files
	sc.Processes = registry
	sc.Ingress = a.ingest
	sc.Preparation = a.preparation
	sc.Logger = a.sink.Logger()
	sc.Group.Logger = sc.Logger
	sc.Group.OpenFile = a.files.OpenVerified
	sc.Publisher.Logger = sc.Logger
	a.manager, err = session.New(ctx, sc)
	manager = a.manager
	if a.manager != nil {
		a.owners.manager = newStopOwner("manager", a.manager.Close)
	}
	if err != nil {
		return err
	}
	if err = checkpoint("manager"); err != nil {
		return err
	}
	a.gpu, err = monitoring.StartGPU(ctx, a.core, gpu.Config(a.config.GPU), gpu.NativeProvider())
	if a.gpu != nil {
		a.owners.gpu = &stopOwner{name: "gpu", close: func(ctx context.Context) stopResult {
			// Останавливает сборщик GPU и проверяет его Done независимо от возвращённой ошибки.

			// e содержит результат остановки сборщика GPU, проверяемый отдельно от закрытия
			// его Done.
			e := a.gpu.Close(ctx)
			return stopResult{e, isClosed(a.gpu.Done())}
		}}
	}
	if err != nil {
		return err
	}
	if err = checkpoint("gpu"); err != nil {
		return err
	}
	a.api, err = api.New(api.Config{Core: a.core, Commands: a.manager, Fallback: a.preparation, Capabilities: capabilities{api.ExecutableCapabilities{FFmpeg: a.config.Executables.FFmpeg, FFprobe: a.config.Executables.FFprobe, Helper: helper}}, Logger: a.sink.Logger(), Limits: api.Limits(a.config.API)})
	if a.api != nil {
		a.owners.api = newStopOwner("api", a.api.Close)
	}
	if err != nil {
		return err
	}
	if err = checkpoint("api"); err != nil {
		return err
	}
	a.http = &http.Server{Handler: a.api.Handler(), ReadHeaderTimeout: a.config.HTTP.ReadHeaderTimeout, IdleTimeout: a.config.HTTP.IdleTimeout, MaxHeaderBytes: a.config.HTTP.MaxHeaderBytes, ErrorLog: slog.NewLogLogger(a.sink.Logger().Handler(), slog.LevelError)}
	a.owners.transport = &stopOwner{name: "http_transport", close: func(ctx context.Context) stopResult {
		// Пробует штатную остановку HTTP, а при ошибке закрывает транспорт принудительно и возвращает обе
		// ошибки.

		// e содержит результат штатной остановки HTTP-транспорта до возможного принудительного
		// Close.
		e := a.http.Shutdown(ctx)
		if e == nil {
			return stopResult{nil, true}
		}
		// forced содержит результат принудительного закрытия HTTP при отказе штатного Shutdown.
		forced := a.http.Close()
		return stopResult{errors.Join(e, forced), forced == nil}
	}}
	if err = checkpoint("http"); err != nil {
		return err
	}
	// lifetime задаёт отдельный срок жизни RTMP Serve; stop отменяет его при остановке приложения.
	lifetime, stop := context.WithCancel(context.Background())
	a.serveCancel = stop
	// После запуска Serve слушатель закрывает только его транспортный владелец.
	a.owners.httpListener = nil
	a.owners.ingestListener = nil
	a.httpServe = startOperation(func() stopResult {
		// Запускает HTTP Serve как наблюдаемую операцию с результатом завершения.
		// e содержит результат HTTP Serve, публикуемый наблюдаемой операцией обслуживания.
		e := a.http.Serve(a.httpListener)
		return stopResult{e, true}
	})
	a.ingestServe = startOperation(func() stopResult {
		// Запускает приём RTMP с отдельным контекстом времени жизни и наблюдаемым завершением.
		// e содержит результат RTMP Serve, публикуемый наблюдаемой операцией обслуживания.
		e := a.ingest.Serve(lifetime, a.ingestListener)
		return stopResult{e, true}
	})
	// build содержит единые сведения текущего бинарника для стартового события application_started.
	build := buildinfo.Current()
	a.sink.Logger().Info("application_started", "http", a.HTTPAddress(), "ingest", a.IngestAddress(),
		"version", build.Version, "commit", build.Commit, "dirty", build.Dirty, "go_version", build.GoVersion)
	return nil
}

// capabilities дополняет проверку исполняемых файлов явной недоступностью NVIDIA вне Linux.
type capabilities struct { /* ExecutableCapabilities проверяет настроенные исполняемые файлы без запуска медиатестов. */
	api.ExecutableCapabilities
}

// Snapshot получает текущую доступность исполняемых файлов без их запуска. Вне Linux он отдельно
// сообщает недоступность NVIDIA на платформе, сохраняя ошибку исходного снимка.
func (c capabilities) Snapshot(ctx context.Context) (api.CapabilitiesDTO, error) {
	// v содержит текущий снимок возможностей; err содержит отказ проверки исходных исполняемых
	// файлов.
	v, err := c.ExecutableCapabilities.Snapshot(ctx)
	if runtime.GOOS != "linux" {
		// now сохраняет время исходной проверки helper для платформенной отметки недоступности
		// NVIDIA.
		now := v.Helper.CheckedAt
		v.NVIDIA = api.CapabilityDTO{Status: "unavailable", Reason: "unsupported_platform", CheckedAt: now}
	}
	return v, err
}

// HTTPAddress возвращает фактический адрес приобретённого HTTP-слушателя либо пустую строку, если
// слушатель ещё не создан.
func (a *Application) HTTPAddress() string {
	if a.httpListener == nil {
		return ""
	}
	return a.httpListener.Addr().String()
}

// IngestAddress возвращает фактический адрес приобретённого слушателя OBS либо пустую строку, если
// слушатель ещё не создан.
func (a *Application) IngestAddress() string {
	if a.ingestListener == nil {
		return ""
	}
	return a.ingestListener.Addr().String()
}

// Done закрывается после подтверждённой остановки всех принадлежащих приложению ресурсов. Историческая
// ошибка завершённой работы может сохраняться и после закрытия канала.
func (a *Application) Done() <-chan struct{} { return a.done }

// LogError передаёт ненулевую ошибку в имеющийся асинхронный журнал приложения. Отсутствие журнала и
// nil не создают дополнительного вывода.
func (a *Application) LogError(err error) {
	if err != nil && a.sink != nil {
		a.sink.Logger().Error("application_error", "error", err)
	}
}

// Wait ожидает отмену ненулевого ctx либо завершение HTTP- или RTMP-обслуживания. До запуска обеих
// операций возвращает core.ErrNotReady, при отмене — ошибку контекста, при возврате Serve — его ошибку с
// указанием транспорта; неожиданный возврат без ошибки также считается отказом обслуживания.
// Общие ресурсы сохраняются у приложения: владелец вызывает Shutdown с независимым сроком.
func (a *Application) Wait(ctx context.Context) error {
	if a.httpServe == nil || a.ingestServe == nil {
		return core.ErrNotReady
	}
	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-a.httpServe.done:
		return fmt.Errorf("HTTP Serve: %w", serveError(a.httpServe.result.err))
	case <-a.ingestServe.done:
		return fmt.Errorf("ingest Serve: %w", serveError(a.ingestServe.result.err))
	}
}

// serveError сохраняет ошибку Serve, а его неожиданный возврат без ошибки преобразует в явный отказ
// обслуживания.
func serveError(err error) error {
	if err == nil {
		return errors.New("обработчик Serve неожиданно завершился")
	}
	return err
}
