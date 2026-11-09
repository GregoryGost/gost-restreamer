package app

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/config"
	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/rtmp"
	"github.com/GregoryGost/gost-restreamer/internal/monitoring/logging"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
	"github.com/bluenviron/gortmplib"
)

type acceptanceLog struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (b *acceptanceLog) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

func (b *acceptanceLog) text() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

func acceptanceHelper(t *testing.T) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "gost-process-helper")
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	args := []string{"build", "-o", path}
	if directory := os.Getenv("GOST_TEST_PROCESS_HELPER_COVERDIR"); directory != "" {
		t.Setenv("GOCOVERDIR", directory)
		args = append(args, "-cover", "-covermode=atomic", "-coverpkg=github.com/GregoryGost/gost-restreamer/...")
	}
	build := exec.CommandContext(ctx, "go", append(args, "../../cmd/gost-process-helper")...)
	build.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOWORK=off", "GOFLAGS=-mod=readonly", "GOPROXY=off")
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("сборка настоящего helper: %v\n%s", err, output)
	}
	return path
}

func acceptanceConfig(t *testing.T, helper string) config.Config {
	t.Helper()
	root, err := os.MkdirTemp("/tmp", "gost-app-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	c := config.Default()
	c.Paths.Data, c.Paths.Runtime = filepath.Join(root, "data"), filepath.Join(root, "run")
	c.Executables.Helper = helper
	c.Executables.FFmpeg, c.Executables.FFprobe = filepath.Join(root, "missing-ffmpeg"), filepath.Join(root, "missing-ffprobe")
	return c
}

func acceptanceDependencies(log *acceptanceLog) dependencies {
	return dependencies{
		listen: func(ctx context.Context, network, _ string) (net.Listener, error) {
			return (&net.ListenConfig{}).Listen(ctx, network, "127.0.0.1:0")
		},
		logger: func(c logging.Config) (*logging.Sink, error) { return logging.New(c, log) },
	}
}

func acceptanceStop(t *testing.T, a *Application) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := a.Shutdown(ctx); err != nil {
		t.Errorf("остановка приложения: %v", err)
	}
	select {
	case <-a.Done():
	default:
		t.Error("успешная остановка не подтвердила Done")
	}
}

func acceptanceHTTP(t *testing.T, a *Application, method, path, body string, status int) map[string]any {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	req, err := http.NewRequestWithContext(ctx, method, "http://"+a.HTTPAddress()+"/api/v1"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	data, readErr := io.ReadAll(resp.Body)
	if err := errors.Join(readErr, resp.Body.Close()); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != status {
		t.Fatalf("%s %s: получено %d, ожидалось %d: %s", method, path, resp.StatusCode, status, data)
	}
	var result map[string]any
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

func acceptanceConnection(t *testing.T, address string) net.Conn {
	t.Helper()
	conn, err := net.DialTimeout("tcp", address, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = conn.Close() })
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return conn
}

func acceptanceIdleHTTP(t *testing.T, a *Application) (net.Conn, *bufio.Reader) {
	t.Helper()
	conn := acceptanceConnection(t, a.HTTPAddress())
	if _, err := fmt.Fprintf(conn, "GET /api/v1/health/live HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	reader := bufio.NewReader(conn)
	resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatal(err)
	}
	_, readErr := io.Copy(io.Discard, resp.Body)
	if err := errors.Join(readErr, resp.Body.Close()); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || resp.Close {
		t.Fatalf("не создано исправное keep-alive соединение: %+v", resp)
	}
	return conn, reader
}

func TestAcceptanceApplicationBootPersistenceAndTransportStop(t *testing.T) {
	c := acceptanceConfig(t, acceptanceHelper(t))
	var streamID, previousIncarnation string
	for cycle := range 2 {
		log := &acceptanceLog{}
		d := acceptanceDependencies(log)
		opens := 0
		d.openStore = func(ctx context.Context, path string, opts storage.Options) (*storage.Store, error) {
			opens++
			return storage.Open(ctx, path, opts)
		}
		a, err := start(context.Background(), c, d)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { acceptanceStop(t, a) })
		if opens != 1 || a.store == nil || a.store.Incarnation() == previousIncarnation {
			t.Fatalf("нарушен единственный новый владелец БД: opens=%d", opens)
		}
		previousIncarnation = a.store.Incarnation()
		if other, err := storage.Open(context.Background(), c.DatabasePath(), c.StorageOptions()); !errors.Is(err, core.ErrInUse) {
			if other != nil {
				_ = other.Close()
			}
			t.Fatalf("второй opener допущен к живому приложению: %v", err)
		}
		if got := acceptanceHTTP(t, a, "GET", "/health/ready", "", 200); got["status"] != "ready" {
			t.Fatal("управление не готово без медиазависимостей", got)
		}
		caps := acceptanceHTTP(t, a, "GET", "/system/capabilities", "", 200)
		if caps["ffmpeg"].(map[string]any)["status"] != "unavailable" || caps["ffprobe"].(map[string]any)["status"] != "unavailable" {
			t.Fatal("отсутствующие исполняемые файлы объявлены доступными", caps)
		}
		if runtime.GOOS != "linux" && caps["nvidia"].(map[string]any)["status"] != "unavailable" {
			t.Fatal("платформа без NVIDIA объявлена пригодной", caps)
		}
		anchor := acceptanceHTTP(t, a, "GET", "/events/snapshot", "", 200)
		if len(anchor["sessions"].([]any)) != 0 {
			t.Fatal("после запуска восстановлен старый RAM-снимок", anchor)
		}
		if cycle == 0 {
			created := acceptanceHTTP(t, a, "POST", "/streams", `{"name":"Сохранённый поток","publication_key":"synthetic-bootstrap-key"}`, 201)
			streamID = created["id"].(string)
		}
		stored := acceptanceHTTP(t, a, "GET", "/streams/"+streamID, "", 200)
		if stored["name"] != "Сохранённый поток" || stored["publication_key"] != "synthetic-bootstrap-key" {
			t.Fatal("имя или ключ публикации не совпадают с сохранёнными значениями", stored)
		}
		idle, idleReader := acceptanceIdleHTTP(t, a)
		sse := acceptanceConnection(t, a.HTTPAddress())
		if _, err := fmt.Fprint(sse, "GET /api/v1/events HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		sseResp, err := http.ReadResponse(bufio.NewReader(sse), &http.Request{Method: http.MethodGet})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() { _ = sseResp.Body.Close() })
		if sseResp.StatusCode != 200 || sseResp.Header.Get("Content-Type") != "text/event-stream" {
			t.Fatal("поток SSE не открыт", sseResp.Status)
		}
		sseReader := bufio.NewReader(sseResp.Body)
		if line, err := sseReader.ReadString('\n'); err != nil || line != ": connected\n" {
			t.Fatalf("нет начального подтверждения SSE: %q %v", line, err)
		}
		ingest := acceptanceConnection(t, a.IngestAddress())
		acceptanceStop(t, a)
		if _, err := idleReader.ReadByte(); !errors.Is(err, io.EOF) {
			t.Fatalf("idle HTTP transport остался открыт после Shutdown: %v", err)
		}
		if _, err := io.ReadAll(sseReader); err != nil {
			t.Fatalf("SSE не завершился штатно: %v", err)
		}
		if _, err := ingest.Read(make([]byte, 1)); err == nil {
			t.Fatal("ingest оставил соединение после остановки")
		} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
			t.Fatalf("ingest не закрыл соединение: %v", err)
		}
		_ = idle.Close()
		if _, err := a.store.Diagnostics(context.Background()); !errors.Is(err, core.ErrClosed) {
			t.Fatalf("БД не закрыта после завершения пользователей: %v", err)
		}
	}
}

func TestAcceptanceInvalidConfigAndHelperPrecedeAcquisitions(t *testing.T) {
	for _, kind := range []string{"конфигурация", "helper"} {
		t.Run(kind, func(t *testing.T) {
			c := acceptanceConfig(t, filepath.Join(t.TempDir(), "missing-helper"))
			if kind == "конфигурация" {
				c.Storage.MaxOpenConns = 0
			}
			acquired := false
			d := dependencies{
				listen: func(context.Context, string, string) (net.Listener, error) {
					acquired = true
					return nil, errors.New("не должен вызываться")
				},
				logger: func(logging.Config) (*logging.Sink, error) {
					acquired = true
					return nil, errors.New("не должен вызываться")
				},
				openStore: func(context.Context, string, storage.Options) (*storage.Store, error) {
					acquired = true
					return nil, errors.New("не должен вызываться")
				},
			}
			a, err := start(context.Background(), c, d)
			if err == nil || a != nil || acquired {
				t.Fatalf("ошибка до ресурсов не соблюдена: app=%p, acquired=%v, err=%v", a, acquired, err)
			}
			if _, err := os.Stat(c.Paths.Data); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("ошибочная конфигурация создала рабочие файлы: %v", err)
			}
		})
	}
}

func TestAcceptancePartialStartupReleasesEveryAcquiredOwner(t *testing.T) {
	helper := acceptanceHelper(t)
	for _, stage := range []string{"logger", "http_listener", "ingest_listener", "store", "files", "processes", "preparation", "ingest", "manager", "gpu", "api", "http"} {
		t.Run(stage, func(t *testing.T) {
			c := acceptanceConfig(t, helper)
			injected := errors.New("контролируемый отказ приобретения")
			d := acceptanceDependencies(&acceptanceLog{})
			d.afterAcquire = func(current string, _ *Application) error {
				if current == stage {
					return injected
				}
				return nil
			}
			a, err := start(context.Background(), c, d)
			if !errors.Is(err, injected) || a == nil {
				t.Fatalf("потеряны ошибка или частичный владелец: app=%p, err=%v", a, err)
			}
			select {
			case <-a.Done():
			default:
				t.Fatal("очистка частичного startup не завершена")
			}
			for _, listener := range []net.Listener{a.httpListener, a.ingestListener} {
				if listener == nil {
					continue
				}
				conn, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
				if err == nil {
					_ = conn.Close()
					t.Fatal("частичный startup оставил listener")
				}
			}
			if a.store != nil {
				if _, err := a.store.Diagnostics(context.Background()); !errors.Is(err, core.ErrClosed) {
					t.Fatalf("частичный startup оставил Store: %v", err)
				}
				reopened, err := storage.Open(context.Background(), c.DatabasePath(), c.StorageOptions())
				if err != nil {
					t.Fatal("не освобождена блокировка Store", err)
				}
				if err := reopened.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

func TestAcceptanceIngestUsesRunningManagerAndPersistentStreamKey(t *testing.T) {
	c := acceptanceConfig(t, acceptanceHelper(t))
	a, err := start(context.Background(), c, acceptanceDependencies(&acceptanceLog{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { acceptanceStop(t, a) })
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	asset, err := a.core.CreateAsset(ctx, "Метаданные без назначений")
	if err != nil {
		t.Fatal(err)
	}
	stream, err := a.core.CreateStream(ctx, core.Stream{Name: "Вход без выходов", PublicationKey: "synthetic-ingest-key", Fallback: core.FallbackPolicy{AssetID: asset.ID, Watchdog: core.WatchdogPolicy{VideoTimeout: time.Second, AudioTimeout: time.Second, RecoveryWindow: time.Second, KeyframeTimeout: time.Second}}})
	if err != nil {
		t.Fatal(err)
	}
	connect := func(key string) (*gortmplib.Client, error) {
		client := &gortmplib.Client{URL: &url.URL{Scheme: "rtmp", Host: a.IngestAddress(), Path: "/" + string(stream.ID), Fragment: key}, Publish: true}
		err := client.Initialize(ctx)
		if err != nil {
			client.Close()
		}
		return client, err
	}
	if client, err := connect(stream.PublicationKey); err == nil {
		client.Close()
		t.Fatal("публикация допущена без RUNNING Session")
	}
	// Пустой комплект метаданных проходит настоящий домен; медиабайты и работники здесь не создаются.
	generation, err := a.core.ReservePreparation(ctx, core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage, GeneratorVersion: config.GeneratorVersion})
	if err != nil {
		t.Fatal(err)
	}
	fence, err := a.core.ClaimPreparation(ctx, generation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err := a.core.MarkPreparationIssued(ctx, fence); err != nil {
		t.Fatal(err)
	}
	if _, err := a.core.PublishPreparation(ctx, fence, core.GenerationPublication{SourceHash: "synthetic-source", SizeBytes: 1, SourceFormat: core.SourceFormat{ImageFormat: "png"}}); err != nil {
		t.Fatal(err)
	}
	if err := a.core.AcknowledgePreparationCleanup(ctx, fence); err != nil {
		t.Fatal(err)
	}
	receipt, err := a.manager.StartSession(ctx, core.StartSessionRequest{RequestID: "application-ingest-start", StreamID: stream.ID})
	if err != nil {
		t.Fatal(err)
	}
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		stored, err := a.core.GetReceipt(ctx, receipt.ID)
		if err != nil {
			t.Fatal(err)
		}
		if stored.Status == core.CommandSucceeded {
			break
		}
		if stored.Status.Terminal() {
			t.Fatal("настоящий SessionManager не завершил запуск", stored.Status)
		}
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-ticker.C:
		}
	}
	if client, err := connect("wrong-synthetic-key"); err == nil {
		client.Close()
		t.Fatal("публикация допущена с чужим ключом")
	}
	client, err := connect(stream.PublicationKey)
	if err != nil {
		t.Fatal("реальный ingest не связан с RUNNING Manager.Feed", err)
	}
	t.Cleanup(client.Close)
	acceptanceStop(t, a)
	if err := client.NetConn().SetReadDeadline(time.Now().Add(time.Second)); err == nil {
		_, err = client.NetConn().Read(make([]byte, 1))
		if err == nil {
			t.Fatal("успешная остановка оставила публикацию открытой")
		} else if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
			t.Fatal("успешная остановка не закрыла публикацию", err)
		}
	}
}

func TestAcceptanceOccupiedPortsDoNotOpenStorage(t *testing.T) {
	helper := acceptanceHelper(t)
	for _, blocked := range []string{"http", "ingest"} {
		t.Run(blocked, func(t *testing.T) {
			occupied, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = occupied.Close() }()
			c := acceptanceConfig(t, helper)
			d := acceptanceDependencies(&acceptanceLog{})
			opens := 0
			d.openStore = func(ctx context.Context, path string, opts storage.Options) (*storage.Store, error) {
				opens++
				return storage.Open(ctx, path, opts)
			}
			d.listen = func(ctx context.Context, network, address string) (net.Listener, error) {
				if blocked == "http" && address == c.HTTP.Address || blocked == "ingest" && address == c.IngestAddress {
					address = occupied.Addr().String()
				} else {
					address = "127.0.0.1:0"
				}
				return (&net.ListenConfig{}).Listen(ctx, network, address)
			}
			a, err := start(context.Background(), c, d)
			if err == nil || opens != 0 || a == nil || !isClosed(a.Done()) {
				t.Fatalf("занятый порт: opens=%d, app=%p, err=%v", opens, a, err)
			}
			if _, err := os.Stat(c.Paths.Data); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("ошибка bind создала хранилище", err)
			}
			if blocked == "ingest" {
				conn, err := net.DialTimeout("tcp", a.HTTPAddress(), time.Second)
				if err == nil {
					_ = conn.Close()
					t.Fatal("ошибка ingest bind оставила HTTP listener")
				}
			}
		})
	}
}

func TestAcceptanceFilePriorityIsLoggedOnceAtWARN(t *testing.T) {
	dir := t.TempDir()
	yamlPath, ymlPath := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "config.yml")
	for _, path := range []string{yamlPath, ymlPath} {
		if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	selected, err := config.SelectPaths("", yamlPath, ymlPath)
	if err != nil {
		t.Fatal(err)
	}
	log := &acceptanceLog{}
	d := acceptanceDependencies(log)
	d.warning = selected.Warning
	a, err := start(context.Background(), acceptanceConfig(t, acceptanceHelper(t)), d)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { acceptanceStop(t, a) })
	acceptanceStop(t, a)
	warnings := 0
	scanner := bufio.NewScanner(strings.NewReader(log.text()))
	for scanner.Scan() {
		var record map[string]any
		if err := json.Unmarshal(scanner.Bytes(), &record); err != nil {
			t.Fatal(err)
		}
		if record["msg"] == "config_file_priority" {
			warnings++
			if record["level"] != "WARN" || record["yaml_path"] != yamlPath || record["yml_path"] != ymlPath || record["selected"] != yamlPath {
				t.Fatal("предупреждение потеряло уровень или точные пути", record)
			}
		}
	}
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if warnings != 1 {
		t.Fatalf("ожидалось одно предупреждение при запуске, получено %d", warnings)
	}
}

type acceptanceHeldReader struct {
	reading, closing, release chan struct{}
	readOnce, closeOnce       sync.Once
}

func (r *acceptanceHeldReader) Read([]byte) (int, error) {
	r.readOnce.Do(func() { close(r.reading) })
	<-r.release
	return 0, errors.New("контролируемая ошибка передачи")
}

func (r *acceptanceHeldReader) Close() error {
	r.closeOnce.Do(func() { close(r.closing) })
	<-r.release
	return nil
}

func TestAcceptancePendingPreparationRetainsSharedStoreUntilLateJoin(t *testing.T) {
	c := acceptanceConfig(t, acceptanceHelper(t))
	a, err := start(context.Background(), c, acceptanceDependencies(&acceptanceLog{}))
	if err != nil {
		t.Fatal(err)
	}
	r := &acceptanceHeldReader{reading: make(chan struct{}), closing: make(chan struct{}), release: make(chan struct{})}
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(r.release) }) }
	t.Cleanup(func() {
		release()
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = a.Shutdown(ctx)
		if !isClosed(a.Done()) {
			t.Error("поздняя подготовка не присоединена при очистке теста")
		}
	})
	asset, err := a.core.CreateAsset(context.Background(), "Незавершённая передача")
	if err != nil {
		t.Fatal(err)
	}
	job, err := a.preparation.Upload(context.Background(), core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage}, r)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-r.reading:
	case <-time.After(5 * time.Second):
		t.Fatal("настоящая подготовка не начала чтение")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
	err = a.Shutdown(ctx)
	cancel()
	if !errors.Is(err, ErrPending) || isClosed(a.Done()) {
		t.Fatalf("неподтверждённая подготовка объявлена завершённой: %v", err)
	}
	select {
	case <-r.closing:
	default:
		t.Fatal("независимая stop-инициация не закрыла вход подготовки")
	}
	if _, err := a.store.Diagnostics(context.Background()); err != nil {
		t.Fatal("Store закрыт до завершения его пользователей", err)
	}
	if isClosed(a.sink.Done()) {
		t.Fatal("журнал закрыт до завершения пользователей")
	}
	if other, err := storage.Open(context.Background(), c.DatabasePath(), c.StorageOptions()); !errors.Is(err, core.ErrInUse) {
		if other != nil {
			_ = other.Close()
		}
		t.Fatalf("pending handle потерял lifetime lock: %v", err)
	}
	release()
	joined, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	if result, err := job.Wait(joined); err == nil || !result.Clean {
		t.Fatalf("задание должно завершиться с рабочей ошибкой и доказанной очисткой: %+v; %v", result, err)
	}
	_ = a.Shutdown(joined)
	if !isClosed(a.Done()) {
		t.Fatal("повторный Shutdown не присоединил позднее завершение")
	}
	if _, err := a.store.Diagnostics(context.Background()); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("Store не закрыт после позднего join: %v", err)
	}
	reopened, err := storage.Open(joined, c.DatabasePath(), c.StorageOptions())
	if err != nil {
		t.Fatal("поздний join не освободил lifetime lock", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestAcceptanceShutdownRetriesHistoricalErrorAndRetainsSynchronousPending(t *testing.T) {
	t.Run("завершённая рабочая ошибка", func(t *testing.T) {
		a := &Application{gate: make(chan struct{}, 1), done: make(chan struct{})}
		a.gate <- struct{}{}
		failure := errors.New("ошибка завершённой работы")
		calls, shared := 0, 0
		a.owners.manager = newStopOwner("manager", func(context.Context) error {
			calls++
			if calls == 1 {
				return failure
			}
			return nil
		})
		a.owners.store = sharedOwner("store", func(context.Context) error { shared++; return nil })
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		err := a.Shutdown(ctx)
		if !errors.Is(err, failure) || errors.Is(err, ErrPending) || calls != 2 || shared != 1 || !isClosed(a.Done()) {
			t.Fatalf("рабочая ошибка смешана с join: calls=%d, shared=%d, err=%v", calls, shared, err)
		}
		if err := a.Shutdown(ctx); !errors.Is(err, failure) || calls != 2 || shared != 1 {
			t.Fatalf("повторный Shutdown повторяет закрытия или теряет ошибку: %v", err)
		}
	})
	t.Run("синхронный Close", func(t *testing.T) {
		a := &Application{gate: make(chan struct{}, 1), done: make(chan struct{})}
		a.gate <- struct{}{}
		entered, otherStopped, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
		var once sync.Once
		unblock := func() { once.Do(func() { close(release) }) }
		t.Cleanup(func() {
			unblock()
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = a.Shutdown(ctx)
			if !isClosed(a.Done()) {
				t.Error("синхронный тестовый владелец не присоединён")
			}
		})
		a.owners.api = newStopOwner("api", func(context.Context) error { close(entered); <-release; return nil })
		a.owners.ingest = newStopOwner("ingest", func(context.Context) error { close(otherStopped); return nil })
		shared := 0
		a.owners.store = sharedOwner("store", func(context.Context) error { shared++; return nil })
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
		err := a.Shutdown(ctx)
		cancel()
		if !errors.Is(err, ErrPending) || shared != 0 || isClosed(a.Done()) {
			t.Fatalf("неподтверждённый Close освободил shared: count=%d, err=%v", shared, err)
		}
		for name, event := range map[string]<-chan struct{}{"api": entered, "ingest": otherStopped} {
			select {
			case <-event:
			default:
				t.Fatalf("независимый stop %s не инициирован", name)
			}
		}
		unblock()
		joined, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		if err := a.Shutdown(joined); err != nil || shared != 1 || !isClosed(a.Done()) {
			t.Fatalf("поздний синхронный Close не присоединён: shared=%d, err=%v", shared, err)
		}
	})
}

func TestAcceptanceStopBeforeIngestServeStarts(t *testing.T) {
	c := acceptanceConfig(t, "unused-helper")
	store, err := storage.Open(context.Background(), c.DatabasePath(), c.StorageOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	service, err := core.NewService(store, store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	ingest, err := rtmp.NewServer(rtmp.Options{Core: service, Route: rtmp.ConventionalRoute, Limits: rtmp.Limits(c.Ingress), Feed: func(core.Session) (*media.Router, error) { return nil, core.ErrNotReady }})
	if err != nil {
		t.Fatal(err)
	}
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	a := &Application{gate: make(chan struct{}, 1), done: make(chan struct{}), ingest: ingest, ingestListener: listener}
	a.gate <- struct{}{}
	release := make(chan struct{})
	var once sync.Once
	a.serveCancel = func() { once.Do(func() { close(release) }) }
	a.owners.ingest = newStopOwner("ingest", ingest.Shutdown)
	a.owners.ingestListener = listenerOwner("ingest_listener", listener)
	a.owners.store = sharedOwner("store", func(context.Context) error { return store.Close() })
	a.ingestServe = startOperation(func() stopResult {
		<-release
		return stopResult{ingest.Serve(context.Background(), listener), true}
	})
	t.Cleanup(func() {
		a.serveCancel()
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = a.Shutdown(ctx)
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if err := a.Shutdown(ctx); err != nil || !isClosed(a.Done()) {
		t.Fatalf("запланированный после stop Serve превратил штатную остановку в отказ: %v", err)
	}
}

func TestAcceptancePopulatedRecoveryIsNotReplayedOrRolledBackByLateStartupFailure(t *testing.T) {
	helper := acceptanceHelper(t)
	for _, lateFailure := range []bool{false, true} {
		t.Run(fmt.Sprintf("late_failure=%v", lateFailure), func(t *testing.T) {
			c := acceptanceConfig(t, helper)
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			previous, err := storage.Open(ctx, c.DatabasePath(), c.StorageOptions())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = previous.Close() })
			service, err := core.NewService(previous, previous.Incarnation())
			if err != nil {
				t.Fatal(err)
			}
			asset, err := service.CreateAsset(ctx, "Сохранённое поколение без выходов")
			if err != nil {
				t.Fatal(err)
			}
			stream, err := service.CreateStream(ctx, core.Stream{Name: "Прерванный запуск", PublicationKey: "synthetic-recovery-key", Fallback: core.FallbackPolicy{AssetID: asset.ID, Watchdog: core.WatchdogPolicy{VideoTimeout: time.Second, AudioTimeout: time.Second, RecoveryWindow: time.Second, KeyframeTimeout: time.Second}}})
			if err != nil {
				t.Fatal(err)
			}
			generation, err := service.ReservePreparation(ctx, core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage, GeneratorVersion: config.GeneratorVersion})
			if err != nil {
				t.Fatal(err)
			}
			fence, err := service.ClaimPreparation(ctx, generation.ID)
			if err != nil {
				t.Fatal(err)
			}
			if err := service.MarkPreparationIssued(ctx, fence); err != nil {
				t.Fatal(err)
			}
			if _, err := service.PublishPreparation(ctx, fence, core.GenerationPublication{SourceHash: "synthetic-recovery-source", SizeBytes: 1, SourceFormat: core.SourceFormat{ImageFormat: "png"}}); err != nil {
				t.Fatal(err)
			}
			if err := service.AcknowledgePreparationCleanup(ctx, fence); err != nil {
				t.Fatal(err)
			}
			receipt, err := service.StartSession(ctx, core.StartSessionRequest{RequestID: "pending-before-restart", StreamID: stream.ID})
			if err != nil {
				t.Fatal(err)
			}
			if err := previous.Close(); err != nil {
				t.Fatal(err)
			}
			d := acceptanceDependencies(&acceptanceLog{})
			failure := errors.New("поздний отказ после committed recovery")
			if lateFailure {
				d.afterAcquire = func(stage string, _ *Application) error {
					if stage == "api" {
						return failure
					}
					return nil
				}
			}
			a, err := start(ctx, c, d)
			if lateFailure {
				if !errors.Is(err, failure) || a == nil || !isClosed(a.Done()) {
					t.Fatalf("поздняя ошибка потеряна или не завершила startup: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { acceptanceStop(t, a) })
				stored, err := a.core.GetSession(ctx, receipt.SessionID)
				if err != nil || stored.State != core.SessionInterrupted {
					t.Fatalf("bootstrap replay старой Session: %+v; %v", stored, err)
				}
				anchor := acceptanceHTTP(t, a, "GET", "/events/snapshot", "", 200)
				if len(anchor["sessions"].([]any)) != 0 {
					t.Fatal("старый запуск восстановлен как текущий RAM runtime", anchor)
				}
				acceptanceStop(t, a)
			}
			reopened, err := storage.Open(ctx, c.DatabasePath(), c.StorageOptions())
			if err != nil {
				t.Fatal(err)
			}
			defer func() { _ = reopened.Close() }()
			read, err := core.NewService(reopened, reopened.Incarnation())
			if err != nil {
				t.Fatal(err)
			}
			stored, err := read.GetSession(ctx, receipt.SessionID)
			if err != nil || stored.State != core.SessionInterrupted || stored.Incarnation != previous.Incarnation() {
				t.Fatalf("committed recovery потерян: %+v; %v", stored, err)
			}
			command, err := read.GetReceipt(ctx, receipt.ID)
			if err != nil || command.Status != core.CommandInterrupted {
				t.Fatalf("старая pending команда повторно принята: %+v; %v", command, err)
			}
		})
	}
}
