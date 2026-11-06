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

type dependencies struct {
	listen       func(context.Context, string, string) (net.Listener, error)
	logger       func(logging.Config) (*logging.Sink, error)
	openStore    func(context.Context, string, storage.Options) (*storage.Store, error)
	afterAcquire func(string, *Application) error
	warning      *config.Warning
}

// Application остаётся владельцем ресурсов, пока Done не подтвердит полную остановку.
type Application struct {
	config                       config.Config
	sink                         *logging.Sink
	store                        *storage.Store
	core                         *core.Service
	files                        *fallback.FileStore
	processes                    *processowner.Registry
	preparation                  *fallback.Manager
	manager                      *session.Manager
	ingest                       *rtmp.Server
	gpu                          *monitoring.GPUCollector
	api                          *api.API
	http                         *http.Server
	httpListener, ingestListener net.Listener
	serveCancel                  context.CancelFunc
	httpServe, ingestServe       *operation
	owners                       shutdownOwners
	gate                         chan struct{}
	done                         chan struct{}
	doneOnce                     sync.Once
	errors                       []error
}

type shutdownOwners struct {
	api, transport, manager, ingest, preparation, gpu *stopOwner
	httpListener, ingestListener                      *stopOwner
	registry, files, store, logger                    *stopOwner
}

func Start(ctx context.Context, c config.Config) (*Application, error) {
	return start(ctx, c, dependencies{})
}

// StartSelected передаёт предупреждение выбора файла единственному JSON-журналу до привязки слушателей.
func StartSelected(ctx context.Context, c config.Config, s config.Selection) (*Application, error) {
	return start(ctx, c, dependencies{warning: s.Warning})
}

func start(ctx context.Context, c config.Config, d dependencies) (*Application, error) {
	if err := c.Validate(); err != nil {
		return nil, err
	}
	helper, err := resolveHelper(c.Executables.Helper)
	if err != nil {
		return nil, err
	}
	if ctx == nil {
		return nil, core.ErrInvalid
	}
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
	a := &Application{config: c, gate: make(chan struct{}, 1), done: make(chan struct{})}
	a.gate <- struct{}{}
	err = a.acquire(startup, helper, d)
	if err != nil {
		a.LogError(err)
		shutdown, stop := context.WithTimeout(context.Background(), c.Lifecycle.ShutdownTimeout)
		cleanup := a.Shutdown(shutdown)
		stop()
		return a, errors.Join(err, cleanup)
	}
	return a, nil
}
func resolveHelper(name string) (string, error) {
	resolved, err := exec.LookPath(name)
	if err != nil {
		return "", fmt.Errorf("разрешить helper: %w", err)
	}
	resolved, err = filepath.Abs(resolved)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(resolved)
	if err != nil {
		return "", fmt.Errorf("helper: %w", err)
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return "", fmt.Errorf("helper должен быть обычным исполняемым файлом")
	}
	return resolved, nil
}
func (a *Application) acquire(ctx context.Context, helper string, d dependencies) error {
	checkpoint := func(stage string) error {
		if err := ctx.Err(); err != nil {
			return err
		}
		if d.afterAcquire != nil {
			return d.afterAcquire(stage, a)
		}
		return nil
	}
	var err error
	a.sink, err = d.logger(logging.Config(a.config.Logging))
	if a.sink != nil {
		a.owners.logger = &stopOwner{name: "logger", close: func(ctx context.Context) stopResult {
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
		a.owners.store = sharedOwner("store", func(context.Context) error { return a.store.Close() })
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
		a.owners.files = sharedOwner("files", func(context.Context) error { return a.files.Close() })
	}
	if err != nil {
		return err
	}
	if err = checkpoint("files"); err != nil {
		return err
	}
	registry, err := processowner.New(a.config.ProcessConfig(helper))
	a.processes = registry
	if registry != nil {
		a.owners.registry = sharedOwner("registry", func(context.Context) error { return registry.Close() })
	}
	if err != nil {
		return err
	}
	if err = checkpoint("processes"); err != nil {
		return err
	}
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
	// Feed записывается до публикации слушателя и затем остаётся неизменным.
	var manager *session.Manager
	a.ingest, err = rtmp.NewServer(rtmp.Options{Core: a.core, Route: rtmp.ConventionalRoute, Feed: func(s core.Session) (*media.Router, error) { return manager.Feed(s) }, Limits: rtmp.Limits(a.config.Ingress), Logger: a.sink.Logger()})
	if a.ingest != nil {
		a.owners.ingest = newStopOwner("ingest", a.ingest.Shutdown)
	}
	if err != nil {
		return err
	}
	if err = checkpoint("ingest"); err != nil {
		return err
	}
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
		e := a.http.Shutdown(ctx)
		if e == nil {
			return stopResult{nil, true}
		}
		forced := a.http.Close()
		return stopResult{errors.Join(e, forced), forced == nil}
	}}
	if err = checkpoint("http"); err != nil {
		return err
	}
	lifetime, stop := context.WithCancel(context.Background())
	a.serveCancel = stop
	// После запуска Serve слушатель закрывает только его транспортный владелец.
	a.owners.httpListener = nil
	a.owners.ingestListener = nil
	a.httpServe = startOperation(func() stopResult { e := a.http.Serve(a.httpListener); return stopResult{e, true} })
	a.ingestServe = startOperation(func() stopResult { e := a.ingest.Serve(lifetime, a.ingestListener); return stopResult{e, true} })
	build := buildinfo.Current()
	a.sink.Logger().Info("application_started", "http", a.HTTPAddress(), "ingest", a.IngestAddress(),
		"version", build.Version, "commit", build.Commit, "dirty", build.Dirty, "go_version", build.GoVersion)
	return nil
}

type capabilities struct{ api.ExecutableCapabilities }

func (c capabilities) Snapshot(ctx context.Context) (api.CapabilitiesDTO, error) {
	v, err := c.ExecutableCapabilities.Snapshot(ctx)
	if runtime.GOOS != "linux" {
		now := v.Helper.CheckedAt
		v.NVIDIA = api.CapabilityDTO{Status: "unavailable", Reason: "unsupported_platform", CheckedAt: now}
	}
	return v, err
}
func (a *Application) HTTPAddress() string {
	if a.httpListener == nil {
		return ""
	}
	return a.httpListener.Addr().String()
}
func (a *Application) IngestAddress() string {
	if a.ingestListener == nil {
		return ""
	}
	return a.ingestListener.Addr().String()
}
func (a *Application) Done() <-chan struct{} { return a.done }
func (a *Application) LogError(err error) {
	if err != nil && a.sink != nil {
		a.sink.Logger().Error("application_error", "error", err)
	}
}

// Wait не освобождает общие ресурсы; их владелец вызывает Shutdown с независимым сроком.
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
func serveError(err error) error {
	if err == nil {
		return errors.New("обработчик Serve неожиданно завершился")
	}
	return err
}
