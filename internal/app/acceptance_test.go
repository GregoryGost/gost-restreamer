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

// acceptanceLog сохраняет настоящий журнал приложения для согласованного чтения тестом.
type acceptanceLog struct {
	// mu защищает буфер журнала при записи фоновых компонентов и чтении проверок.
	mu sync.Mutex
	// buffer накапливает полные байты JSON-записей приложения.
	buffer bytes.Buffer
}

// Write сохраняет тестовый журнал под блокировкой, позволяя приложению писать из фоновых горутин.
func (b *acceptanceLog) Write(p []byte) (int, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.Write(p)
}

// text возвращает согласованную копию накопленного журнала.
func (b *acceptanceLog) text() string {
	b.mu.Lock()
	defer b.mu.Unlock()
	return b.buffer.String()
}

// acceptanceHelper собирает настоящий gost-process-helper во временный каталог без загрузки модулей.
// При включённом сборе покрытия добавляет инструментацию и передаёт каталог дочерних профилей.
func acceptanceHelper(t *testing.T) string {
	t.Helper()
	// path указывает отдельный бинарник настоящего helper внутри временного каталога теста.
	path := filepath.Join(t.TempDir(), "gost-process-helper")
	// ctx ограничивает сборку helper 90 секундами; cancel освобождает таймер после её завершения.
	ctx, cancel := context.WithTimeout(context.Background(), 90*time.Second)
	defer cancel()
	// args задаёт сборку helper и дополняется инструментацией при включённом покрытии дочерних
	// процессов.
	args := []string{"build", "-o", path}
	// directory содержит переданный каталог покрытия helper; непустое значение включает
	// инструментацию и GOCOVERDIR.
	if directory := os.Getenv("GOST_TEST_PROCESS_HELPER_COVERDIR"); directory != "" {
		t.Setenv("GOCOVERDIR", directory)
		args = append(args, "-cover", "-covermode=atomic", "-coverpkg=github.com/GregoryGost/gost-restreamer/...")
	}
	// build запускает локальную сборку настоящего helper с запрещённой загрузкой модулей.
	build := exec.CommandContext(ctx, "go", append(args, "../../cmd/gost-process-helper")...)
	build.Env = append(os.Environ(), "GOTOOLCHAIN=local", "GOWORK=off", "GOFLAGS=-mod=readonly", "GOPROXY=off")
	// output сохраняет диагностику сборки helper; err содержит результат go build.
	if output, err := build.CombinedOutput(); err != nil {
		t.Fatalf("сборка настоящего helper: %v\n%s", err, output)
	}
	return path
}

// acceptanceConfig создаёт отдельные каталоги данных и объектов выполнения, сохраняя настоящий
// gost-process-helper и заведомо отсутствующие FFmpeg/ffprobe.
func acceptanceConfig(t *testing.T, helper string) config.Config {
	t.Helper()
	// root выделяет короткий изолированный рабочий корень приложения; err содержит отказ создания
	// каталога.
	root, err := os.MkdirTemp("/tmp", "gost-app-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Удаляет частный рабочий каталог теста после освобождения его ресурсов.

		// err содержит отказ удаления принадлежащего тесту рабочего корня при очистке.
		if err := os.RemoveAll(root); err != nil {
			t.Error(err)
		}
	})
	// c содержит настройки по умолчанию, дополняемые тестовыми путями и исполняемыми файлами.
	c := config.Default()
	c.Paths.Data, c.Paths.Runtime = filepath.Join(root, "data"), filepath.Join(root, "run")
	c.Executables.Helper = helper
	c.Executables.FFmpeg, c.Executables.FFprobe = filepath.Join(root, "missing-ffmpeg"), filepath.Join(root, "missing-ffprobe")
	return c
}

// acceptanceDependencies привязывает слушатели к свободным локальным портам и направляет настоящий
// JSON-журнал в тестовый буфер.
func acceptanceDependencies(log *acceptanceLog) dependencies {
	return dependencies{
		listen: func(ctx context.Context, network, _ string) (net.Listener, error) {
			// Привязывает тестовые слушатели к свободным портам локального интерфейса.

			return (&net.ListenConfig{}).Listen(ctx, network, "127.0.0.1:0")
		},
		logger: func(c logging.Config) (*logging.Sink, error) {
			// Создаёт настоящий журнал с захватом вывода в синхронизированный тестовый буфер.
			return logging.New(c, log)
		},
	}
}

// acceptanceStop ограниченно останавливает приложение и требует подтверждения полной остановки через
// Done.
func acceptanceStop(t *testing.T, a *Application) {
	t.Helper()
	// ctx ограничивает полную остановку приложения пятью секундами; cancel освобождает таймер.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// err содержит результат Shutdown, который должен подтвердить полное освобождение ресурсов.
	if err := a.Shutdown(ctx); err != nil {
		t.Errorf("остановка приложения: %v", err)
	}
	select {
	case <-a.Done():
	default:
		t.Error("успешная остановка не подтвердила Done")
	}
}

// acceptanceHTTP отправляет запрос настоящему приложению, проверяет статус и возвращает JSON-объект
// после закрытия тела ответа.
func acceptanceHTTP(t *testing.T, a *Application, method, path, body string, status int) map[string]any {
	t.Helper()
	// ctx ограничивает HTTP-запрос тремя секундами; cancel освобождает срок после чтения ответа.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// req содержит запрос к настоящему API; err содержит отказ формирования запроса.
	req, err := http.NewRequestWithContext(ctx, method, "http://"+a.HTTPAddress()+"/api/v1"+path, strings.NewReader(body))
	if err != nil {
		t.Fatal(err)
	}
	if body != "" {
		req.Header.Set("Content-Type", "application/json")
	}
	// client выполняет запрос без сохранения keep-alive-соединений между вызовами тестового
	// помощника.
	client := &http.Client{Transport: &http.Transport{DisableKeepAlives: true}}
	// resp содержит ответ настоящего HTTP API для проверки статуса и чтения JSON.
	resp, err := client.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	// data содержит прочитанные байты тела ответа; readErr содержит отказ чтения до закрытия тела.
	data, readErr := io.ReadAll(resp.Body)
	// err объединяет отказы чтения и закрытия тела HTTP-ответа.
	if err := errors.Join(readErr, resp.Body.Close()); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != status {
		t.Fatalf("%s %s: получено %d, ожидалось %d: %s", method, path, resp.StatusCode, status, data)
	}
	// result принимает JSON-объект ответа, возвращаемый тестовому сценарию.
	var result map[string]any
	// err содержит отказ разбора тела HTTP-ответа как JSON-объекта.
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}

// acceptanceConnection открывает локальное TCP-соединение с конечным сроком и регистрирует его
// закрытие при очистке теста.
func acceptanceConnection(t *testing.T, address string) net.Conn {
	t.Helper()
	// conn содержит локальное тестовое TCP-соединение; err содержит отказ подключения в пределах
	// трёх секунд.
	conn, err := net.DialTimeout("tcp", address, 3*time.Second)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает тестовое TCP-соединение после проверки транспорта.
		_ = conn.Close()
	})
	// err содержит отказ установки общего срока операций тестового TCP-соединения.
	if err := conn.SetDeadline(time.Now().Add(3 * time.Second)); err != nil {
		t.Fatal(err)
	}
	return conn
}

// acceptanceIdleHTTP создаёт исправное keep-alive соединение после полного чтения ответа health/live
// для последующей проверки остановки транспорта.
func acceptanceIdleHTTP(t *testing.T, a *Application) (net.Conn, *bufio.Reader) {
	t.Helper()
	// conn содержит HTTP-соединение, которое переводится в исправное idle keep-alive-состояние.
	conn := acceptanceConnection(t, a.HTTPAddress())
	// err содержит отказ отправки health/live через сырой HTTP-транспорт.
	if _, err := fmt.Fprintf(conn, "GET /api/v1/health/live HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
		t.Fatal(err)
	}
	// reader сохраняет буферизованное чтение HTTP-соединения для ответа и последующей проверки EOF.
	reader := bufio.NewReader(conn)
	// resp содержит ответ health/live; err содержит отказ разбора HTTP-заголовков.
	resp, err := http.ReadResponse(reader, &http.Request{Method: http.MethodGet})
	if err != nil {
		t.Fatal(err)
	}
	// readErr содержит отказ полного дочитывания тела, необходимого перед проверкой idle-соединения.
	_, readErr := io.Copy(io.Discard, resp.Body)
	// err объединяет отказы дочитывания и закрытия тела health/live.
	if err := errors.Join(readErr, resp.Body.Close()); err != nil {
		t.Fatal(err)
	}
	if resp.StatusCode != http.StatusOK || resp.Close {
		t.Fatalf("не создано исправное keep-alive соединение: %+v", resp)
	}
	return conn, reader
}

// Проверяет сохранение конфигурации через два запуска, готовность без медиазависимостей и закрытие
// HTTP, SSE, входа OBS и Store после остановки.
func TestAcceptanceApplicationBootPersistenceAndTransportStop(t *testing.T) {
	// c задаёт одни постоянные тестовые пути для двух последовательных запусков приложения.
	c := acceptanceConfig(t, acceptanceHelper(t))
	// streamID сохраняет созданный в первом цикле Stream; previousIncarnation сохраняет владельца
	// прежнего открытия БД для проверки нового запуска.
	var streamID, previousIncarnation string
	// cycle выбирает первый или второй запуск с общей долговечной конфигурацией.
	for cycle := range 2 {
		// log перехватывает настоящий журнал текущего цикла запуска.
		log := &acceptanceLog{}
		// d подставляет локальные слушатели и захват журнала, сохраняя настоящее открытие
		// SQLite.
		d := acceptanceDependencies(log)
		// opens считает обращения к storage.Open внутри одного цикла сборки.
		opens := 0
		d.openStore = func(ctx context.Context, path string, opts storage.Options) (*storage.Store, error) {
			// Считает открытия настоящего хранилища, проверяя единственного владельца на цикл запуска.

			opens++
			return storage.Open(ctx, path, opts)
		}
		// a содержит приложение текущего цикла; err содержит отказ его сборки.
		a, err := start(context.Background(), c, d)
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			// Останавливает приложение текущего цикла и проверяет подтверждение Done.
			acceptanceStop(t, a)
		})
		if opens != 1 || a.store == nil || a.store.Incarnation() == previousIncarnation {
			t.Fatalf("нарушен единственный новый владелец БД: opens=%d", opens)
		}
		previousIncarnation = a.store.Incarnation()
		// other содержит возможный второй Store для очистки при ошибке проверки; err должен
		// сообщать core.ErrInUse при живом владельце.
		if other, err := storage.Open(context.Background(), c.DatabasePath(), c.StorageOptions()); !errors.Is(err, core.ErrInUse) {
			if other != nil {
				_ = other.Close()
			}
			t.Fatalf("второй opener допущен к живому приложению: %v", err)
		}
		// got содержит реальный ответ readiness при отсутствующих FFmpeg и ffprobe.
		if got := acceptanceHTTP(t, a, "GET", "/health/ready", "", 200); got["status"] != "ready" {
			t.Fatal("управление не готово без медиазависимостей", got)
		}
		// caps содержит снимок возможностей для проверки явной недоступности отсутствующих
		// медиазависимостей.
		caps := acceptanceHTTP(t, a, "GET", "/system/capabilities", "", 200)
		if caps["ffmpeg"].(map[string]any)["status"] != "unavailable" || caps["ffprobe"].(map[string]any)["status"] != "unavailable" {
			t.Fatal("отсутствующие исполняемые файлы объявлены доступными", caps)
		}
		if runtime.GOOS != "linux" && caps["nvidia"].(map[string]any)["status"] != "unavailable" {
			t.Fatal("платформа без NVIDIA объявлена пригодной", caps)
		}
		// anchor содержит стартовый RAM-снимок, в котором не должны появиться наблюдения
		// прежнего запуска.
		anchor := acceptanceHTTP(t, a, "GET", "/events/snapshot", "", 200)
		if len(anchor["sessions"].([]any)) != 0 {
			t.Fatal("после запуска восстановлен старый RAM-снимок", anchor)
		}
		if cycle == 0 {
			// created содержит ответ создания Stream в первом цикле для сохранения его
			// идентификатора.
			created := acceptanceHTTP(t, a, "POST", "/streams", `{"name":"Сохранённый поток","publication_key":"synthetic-bootstrap-key"}`, 201)
			streamID = created["id"].(string)
		}
		// stored содержит Stream после текущего запуска для проверки постоянства имени и
		// publication_key.
		stored := acceptanceHTTP(t, a, "GET", "/streams/"+streamID, "", 200)
		if stored["name"] != "Сохранённый поток" || stored["publication_key"] != "synthetic-bootstrap-key" {
			t.Fatal("имя или ключ публикации не совпадают с сохранёнными значениями", stored)
		}
		// idle содержит исправное keep-alive-соединение; idleReader позволяет проверить его
		// закрытие после Shutdown.
		idle, idleReader := acceptanceIdleHTTP(t, a)
		// sse содержит отдельное TCP-соединение для проверки завершения активной подписки.
		sse := acceptanceConnection(t, a.HTTPAddress())
		// err содержит отказ отправки HTTP-запроса подписки SSE.
		if _, err := fmt.Fprint(sse, "GET /api/v1/events HTTP/1.1\r\nHost: localhost\r\n\r\n"); err != nil {
			t.Fatal(err)
		}
		// sseResp содержит ответ SSE для проверки статуса, типа и последующего чтения до EOF.
		sseResp, err := http.ReadResponse(bufio.NewReader(sse), &http.Request{Method: http.MethodGet})
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			// Закрывает тело тестового ответа SSE после проверки остановки.
			_ = sseResp.Body.Close()
		})
		if sseResp.StatusCode != 200 || sseResp.Header.Get("Content-Type") != "text/event-stream" {
			t.Fatal("поток SSE не открыт", sseResp.Status)
		}
		// sseReader читает начальный кадр и завершение потокового тела SSE.
		sseReader := bufio.NewReader(sseResp.Body)
		// line содержит начальную строку SSE; err содержит отказ её чтения до проверки
		// подтверждения подключения.
		if line, err := sseReader.ReadString('\n'); err != nil || line != ": connected\n" {
			t.Fatalf("нет начального подтверждения SSE: %q %v", line, err)
		}
		// ingest содержит принятое TCP-соединение OBS, которое остановка должна физически
		// закрыть.
		ingest := acceptanceConnection(t, a.IngestAddress())
		acceptanceStop(t, a)
		// err содержит результат чтения idle HTTP после Shutdown, ожидается EOF.
		if _, err := idleReader.ReadByte(); !errors.Is(err, io.EOF) {
			t.Fatalf("idle HTTP transport остался открыт после Shutdown: %v", err)
		}
		// err содержит отказ дочитывания SSE после остановки приложения.
		if _, err := io.ReadAll(sseReader); err != nil {
			t.Fatalf("SSE не завершился штатно: %v", err)
		}
		// err содержит результат чтения ingest после остановки, которое должно сообщить о
		// закрытии.
		if _, err := ingest.Read(make([]byte, 1)); err == nil {
			t.Fatal("ingest оставил соединение после остановки")
		} else /* timeout содержит транспортную ошибку ingest; ok показывает возможность проверить, что соединение закрылось, а не осталось до срока. */ if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
			t.Fatalf("ingest не закрыл соединение: %v", err)
		}
		_ = idle.Close()
		// err содержит отказ диагностики уже закрытого Store, ожидается core.ErrClosed.
		if _, err := a.store.Diagnostics(context.Background()); !errors.Is(err, core.ErrClosed) {
			t.Fatalf("БД не закрыта после завершения пользователей: %v", err)
		}
	}
}

// Проверяет, что недопустимые настройки и отсутствующий gost-process-helper отклоняются до открытия
// слушателей, журнала и хранилища.
func TestAcceptanceInvalidConfigAndHelperPrecedeAcquisitions(t *testing.T) {
	// kind выбирает ранний отказ конфигурации или отсутствующего helper.
	for _, kind := range []string{"конфигурация", "helper"} {
		t.Run(kind, func(t *testing.T) {
			// Проверяет отказ до приобретения ресурсов при выбранной ошибке конфигурации или пути вспомогательной
			// программы.

			// c задаёт изолированные пути с отсутствующим helper и при необходимости
			// неверным пулом SQLite.
			c := acceptanceConfig(t, filepath.Join(t.TempDir(), "missing-helper"))
			if kind == "конфигурация" {
				c.Storage.MaxOpenConns = 0
			}
			// acquired отмечает любое запрещённое получение ресурса после раннего отказа
			// проверки.
			acquired := false
			// d обнаруживает открытие слушателя, журнала или Store при недопустимой
			// конфигурации либо helper.
			d := dependencies{
				listen: func(context.Context, string, string) (net.Listener, error) {
					// Фиксирует запрещённое приобретение слушателя после ранней ошибки запуска.

					acquired = true
					return nil, errors.New("не должен вызываться")
				},
				logger: func(logging.Config) (*logging.Sink, error) {
					// Фиксирует запрещённое создание журнала после ранней ошибки запуска.

					acquired = true
					return nil, errors.New("не должен вызываться")
				},
				openStore: func(context.Context, string, storage.Options) (*storage.Store, error) {
					// Фиксирует запрещённое открытие БД после ранней ошибки запуска.

					acquired = true
					return nil, errors.New("не должен вызываться")
				},
			}
			// a содержит возможного ошибочного владельца; err содержит ожидаемый ранний
			// отказ без приобретения ресурсов.
			a, err := start(context.Background(), c, d)
			if err == nil || a != nil || acquired {
				t.Fatalf("ошибка до ресурсов не соблюдена: app=%p, acquired=%v, err=%v", a, acquired, err)
			}
			// err содержит результат проверки каталога данных, который ранний отказ не
			// должен создавать.
			if _, err := os.Stat(c.Paths.Data); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("ошибочная конфигурация создала рабочие файлы: %v", err)
			}
		})
	}
}

// Проверяет освобождение каждого приобретённого владельца при отказе на любой из двенадцати границ
// частичной сборки.
func TestAcceptancePartialStartupReleasesEveryAcquiredOwner(t *testing.T) {
	// helper содержит настоящий бинарник для всех границ частичной сборки.
	helper := acceptanceHelper(t)
	// stage задаёт одну из двенадцати точек отказа после приобретения очередного ресурса.
	for _, stage := range []string{"logger", "http_listener", "ingest_listener", "store", "files", "processes", "preparation", "ingest", "manager", "gpu", "api", "http"} {
		t.Run(stage, func(t *testing.T) {
			// Проверяет освобождение всех уже приобретённых ресурсов при отказе выбранной стадии сборки
			// приложения.

			// c задаёт отдельные каталоги приложения текущей границы отказа.
			c := acceptanceConfig(t, helper)
			// injected задаёт узнаваемую ошибку контрольной точки приобретения.
			injected := errors.New("контролируемый отказ приобретения")
			// d дополняет реальные тестовые зависимости отказом после выбранной стадии.
			d := acceptanceDependencies(&acceptanceLog{})
			d.afterAcquire = func(current string, _ *Application) error {
				// Вводит ошибку сразу после приобретения ресурса выбранной стадии.

				if current == stage {
					return injected
				}
				return nil
			}
			// a содержит частично собранное приложение; err должен сохранить введённый отказ
			// после очистки.
			a, err := start(context.Background(), c, d)
			if !errors.Is(err, injected) || a == nil {
				t.Fatalf("потеряны ошибка или частичный владелец: app=%p, err=%v", a, err)
			}
			select {
			case <-a.Done():
			default:
				t.Fatal("очистка частичного startup не завершена")
			}
			// listener содержит очередной приобретённый HTTP- или ingest-слушатель для
			// проверки закрытия.
			for _, listener := range []net.Listener{a.httpListener, a.ingestListener} {
				if listener == nil {
					continue
				}
				// conn содержит возможное ошибочно принятое соединение; err должен
				// подтвердить закрытие слушателя после отказа сборки.
				conn, err := net.DialTimeout("tcp", listener.Addr().String(), time.Second)
				if err == nil {
					_ = conn.Close()
					t.Fatal("частичный startup оставил listener")
				}
			}
			if a.store != nil {
				// err содержит результат диагностики Store после частичной очистки,
				// ожидается core.ErrClosed.
				if _, err := a.store.Diagnostics(context.Background()); !errors.Is(err, core.ErrClosed) {
					t.Fatalf("частичный startup оставил Store: %v", err)
				}
				// reopened содержит новое открытие БД после очистки; err содержит отказ,
				// выявляющий неосвобождённую блокировку.
				reopened, err := storage.Open(context.Background(), c.DatabasePath(), c.StorageOptions())
				if err != nil {
					t.Fatal("не освобождена блокировка Store", err)
				}
				// err содержит отказ закрытия повторно открытого тестового Store.
				if err := reopened.Close(); err != nil {
					t.Fatal(err)
				}
			}
		})
	}
}

// Проверяет настоящий вход OBS до и после запуска SessionManager: публикация требует RUNNING Session и
// правильный постоянный ключ, а остановка закрывает соединение.
func TestAcceptanceIngestUsesRunningManagerAndPersistentStreamKey(t *testing.T) {
	// c задаёт изолированное приложение с настоящими helper и входным RTMP.
	c := acceptanceConfig(t, acceptanceHelper(t))
	// a содержит приложение для проверки связи ingest с менеджером; err содержит отказ запуска.
	a, err := start(context.Background(), c, acceptanceDependencies(&acceptanceLog{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Останавливает приложение после проверки допуска публикации RTMP.
		acceptanceStop(t, a)
	})
	// ctx ограничивает настройку и сценарий публикации восемью секундами; cancel освобождает его
	// таймер.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	// asset содержит заглушку без требуемых выходных вариантов для подготовки минимального доменного
	// запуска.
	asset, err := a.core.CreateAsset(ctx, "Метаданные без назначений")
	if err != nil {
		t.Fatal(err)
	}
	// stream содержит поток с постоянным синтетическим ключом и политикой заглушки для проверки
	// допуска RTMP.
	stream, err := a.core.CreateStream(ctx, core.Stream{Name: "Вход без выходов", PublicationKey: "synthetic-ingest-key", Fallback: core.FallbackPolicy{AssetID: asset.ID, Watchdog: core.WatchdogPolicy{VideoTimeout: time.Second, AudioTimeout: time.Second, RecoveryWindow: time.Second, KeyframeTimeout: time.Second}}})
	if err != nil {
		t.Fatal(err)
	}
	// connect открывает публикацию с указанным ключом и закрывает клиента при отказе согласования.
	connect := func(key string) (*gortmplib.Client, error) {
		// Подключает RTMP-публикацию с указанным ключом и закрывает клиента при ошибке инициализации.

		// client задаёт RTMP-публикацию текущего Stream с переданным ключом.
		client := &gortmplib.Client{URL: &url.URL{Scheme: "rtmp", Host: a.IngestAddress(), Path: "/" + string(stream.ID), Fragment: key}, Publish: true}
		// err содержит отказ инициализации RTMP-клиента для текущего ключа.
		err := client.Initialize(ctx)
		if err != nil {
			client.Close()
		}
		return client, err
	}
	// client содержит попытку публикации до RUNNING Session; err должен подтвердить отказ допуска.
	if client, err := connect(stream.PublicationKey); err == nil {
		client.Close()
		t.Fatal("публикация допущена без RUNNING Session")
	}
	// Пустой комплект метаданных проходит настоящий домен; медиабайты и работники здесь не создаются.
	// generation содержит зарезервированное поколение минимальной заглушки без медиаработников.
	generation, err := a.core.ReservePreparation(ctx, core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage, GeneratorVersion: config.GeneratorVersion})
	if err != nil {
		t.Fatal(err)
	}
	// fence содержит точную захваченную попытку подготовки для последовательной публикации
	// метаданных.
	fence, err := a.core.ClaimPreparation(ctx, generation.ID)
	if err != nil {
		t.Fatal(err)
	}
	// err содержит отказ фиксации выдачи попытки подготовки.
	if err := a.core.MarkPreparationIssued(ctx, fence); err != nil {
		t.Fatal(err)
	}
	// err содержит отказ публикации минимальных метаданных поколения заглушки.
	if _, err := a.core.PublishPreparation(ctx, fence, core.GenerationPublication{SourceHash: "synthetic-source", SizeBytes: 1, SourceFormat: core.SourceFormat{ImageFormat: "png"}}); err != nil {
		t.Fatal(err)
	}
	// err содержит отказ подтверждения очистки подготовленного поколения перед запуском Session.
	if err := a.core.AcknowledgePreparationCleanup(ctx, fence); err != nil {
		t.Fatal(err)
	}
	// receipt содержит квитанцию настоящего менеджера на запуск Session проверяемого Stream.
	receipt, err := a.manager.StartSession(ctx, core.StartSessionRequest{RequestID: "application-ingest-start", StreamID: stream.ID})
	if err != nil {
		t.Fatal(err)
	}
	// ticker задаёт короткий период чтения квитанции до готовности запуска.
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		// stored содержит текущее состояние квитанции запуска; err содержит отказ её чтения.
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
	// client содержит попытку публикации с неверным постоянным ключом; err должен подтвердить отказ.
	if client, err := connect("wrong-synthetic-key"); err == nil {
		client.Close()
		t.Fatal("публикация допущена с чужим ключом")
	}
	// client содержит успешно принятую публикацию с правильным ключом после готовности Session.
	client, err := connect(stream.PublicationKey)
	if err != nil {
		t.Fatal("реальный ingest не связан с RUNNING Manager.Feed", err)
	}
	t.Cleanup(client.Close)
	acceptanceStop(t, a)
	// err содержит отказ установки срока чтения при проверке закрытия публикации остановленным
	// приложением.
	if err := client.NetConn().SetReadDeadline(time.Now().Add(time.Second)); err == nil {
		_, err = client.NetConn().Read(make([]byte, 1))
		if err == nil {
			t.Fatal("успешная остановка оставила публикацию открытой")
		} else /* timeout содержит сетевой отказ чтения; ok позволяет отличить фактическое закрытие публикации от простого истечения срока. */ if timeout, ok := err.(net.Error); ok && timeout.Timeout() {
			t.Fatal("успешная остановка не закрыла публикацию", err)
		}
	}
}

// Проверяет, что занятый порт HTTP или входа OBS останавливает сборку до открытия БД и освобождает уже
// созданный слушатель.
func TestAcceptanceOccupiedPortsDoNotOpenStorage(t *testing.T) {
	// helper содержит настоящий helper для сценариев занятых портов.
	helper := acceptanceHelper(t)
	// blocked выбирает HTTP или ingest, чей адрес намеренно занят сторонним слушателем.
	for _, blocked := range []string{"http", "ingest"} {
		t.Run(blocked, func(t *testing.T) {
			// Проверяет ранний отказ на занятом порту выбранного транспорта без открытия хранилища.

			// occupied удерживает свободно выделенный локальный порт; err содержит отказ
			// создания блокирующего слушателя.
			occupied, err := net.Listen("tcp", "127.0.0.1:0")
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				// Освобождает слушатель, удерживающий занятый тестовый порт.
				_ = occupied.Close()
			}()
			// c задаёт изолированные настройки для текущего сценария занятого порта.
			c := acceptanceConfig(t, helper)
			// d подставляет занятый адрес выбранному транспорту и отслеживает попытки
			// открыть БД.
			d := acceptanceDependencies(&acceptanceLog{})
			// opens считает запрещённые обращения к хранилищу после отказа bind.
			opens := 0
			d.openStore = func(ctx context.Context, path string, opts storage.Options) (*storage.Store, error) {
				// Считает обращения к настоящему хранилищу, которые должны отсутствовать после ошибки привязки.

				opens++
				return storage.Open(ctx, path, opts)
			}
			d.listen = func(ctx context.Context, network, address string) (net.Listener, error) {
				// Подставляет занятый адрес только выбранному транспорту, оставляя второму свободный локальный порт.

				if blocked == "http" && address == c.HTTP.Address || blocked == "ingest" && address == c.IngestAddress {
					address = occupied.Addr().String()
				} else {
					address = "127.0.0.1:0"
				}
				return (&net.ListenConfig{}).Listen(ctx, network, address)
			}
			// a содержит частичного владельца при ошибке bind для проверки полной очистки.
			a, err := start(context.Background(), c, d)
			if err == nil || opens != 0 || a == nil || !isClosed(a.Done()) {
				t.Fatalf("занятый порт: opens=%d, app=%p, err=%v", opens, a, err)
			}
			// err содержит результат проверки отсутствия каталога данных после раннего
			// отказа bind.
			if _, err := os.Stat(c.Paths.Data); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("ошибка bind создала хранилище", err)
			}
			if blocked == "ingest" {
				// conn содержит возможное ошибочное соединение HTTP; err должен
				// подтвердить закрытие ранее приобретённого слушателя после отказа
				// ingest.
				conn, err := net.DialTimeout("tcp", a.HTTPAddress(), time.Second)
				if err == nil {
					_ = conn.Close()
					t.Fatal("ошибка ingest bind оставила HTTP listener")
				}
			}
		})
	}
}

// Проверяет единственную запись WARN о приоритете yaml с обоими точными путями и выбранным файлом.
func TestAcceptanceFilePriorityIsLoggedOnceAtWARN(t *testing.T) {
	// dir выделяет каталог двух стандартных файлов конфигурации для предупреждения о приоритете.
	dir := t.TempDir()
	// yamlPath задаёт приоритетный YAML; ymlPath задаёт существующий второй стандартный YML.
	yamlPath, ymlPath := filepath.Join(dir, "config.yaml"), filepath.Join(dir, "config.yml")
	// path содержит очередной стандартный файл для записи пустого отображения.
	for _, path := range []string{yamlPath, ymlPath} {
		// err содержит отказ записи стандартного YAML-кандидата.
		if err := os.WriteFile(path, []byte("{}\n"), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// selected содержит выбранный файл и предупреждение; err содержит отказ проверки двух
	// стандартных путей.
	selected, err := config.SelectPaths("", yamlPath, ymlPath)
	if err != nil {
		t.Fatal(err)
	}
	// log сохраняет настоящий журнал для подсчёта единственного предупреждения запуска.
	log := &acceptanceLog{}
	// d передаёт предупреждение выбора файла в настоящую сборку с тестовым журналом.
	d := acceptanceDependencies(log)
	d.warning = selected.Warning
	// a содержит запущенное приложение, журнал которого проверяется после полного закрытия.
	a, err := start(context.Background(), acceptanceConfig(t, acceptanceHelper(t)), d)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Останавливает приложение перед проверкой единственного предупреждения выбора файла.
		acceptanceStop(t, a)
	})
	acceptanceStop(t, a)
	// warnings считает события config_file_priority в итоговом журнале.
	warnings := 0
	// scanner читает итоговый журнал по JSON-строкам.
	scanner := bufio.NewScanner(strings.NewReader(log.text()))
	for scanner.Scan() {
		// record принимает поля текущего события для проверки уровня WARN и точных путей.
		var record map[string]any
		// err содержит отказ разбора очередной JSON-записи журнала.
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
	// err содержит отказ построчного чтения итогового журнала.
	if err := scanner.Err(); err != nil {
		t.Fatal(err)
	}
	if warnings != 1 {
		t.Fatalf("ожидалось одно предупреждение при запуске, получено %d", warnings)
	}
}

// acceptanceHeldReader удерживает чтение и закрытие загрузки до явного освобождения тестом.
type acceptanceHeldReader struct {
	// reading сообщает о начале Read; closing сообщает о начале Close; release разрешает обеим
	// операциям завершиться.
	reading, closing, release chan struct{}
	// readOnce обеспечивает однократный сигнал reading; closeOnce обеспечивает однократный сигнал
	// closing.
	readOnce, closeOnce sync.Once
}

// Read сообщает о начале чтения и удерживает его до явного снятия блокировки, затем возвращает
// управляемую ошибку передачи.
func (r *acceptanceHeldReader) Read([]byte) (int, error) {
	r.readOnce.Do(func() {
		// Однократно сообщает о начале заблокированного чтения файла заглушки.
		close(r.reading)
	})
	<-r.release
	return 0, errors.New("контролируемая ошибка передачи")
}

// Close сообщает о начале закрытия источника загрузки и удерживает завершение до снятия той же
// блокировки.
func (r *acceptanceHeldReader) Close() error {
	r.closeOnce.Do(func() {
		// Однократно сообщает о попытке закрыть ещё удерживаемый источник загрузки.
		close(r.closing)
	})
	<-r.release
	return nil
}

// Проверяет, что зависшая подготовка сохраняет Store, журнал и блокировку БД до позднего завершения,
// после которого повторный Shutdown освобождает зависимости.
func TestAcceptancePendingPreparationRetainsSharedStoreUntilLateJoin(t *testing.T) {
	// c задаёт изолированные ресурсы приложения со специально удерживаемым источником подготовки.
	c := acceptanceConfig(t, acceptanceHelper(t))
	// a содержит приложение, которое обязано сохранить общие зависимости до позднего завершения
	// подготовки; err содержит отказ запуска.
	a, err := start(context.Background(), c, acceptanceDependencies(&acceptanceLog{}))
	if err != nil {
		t.Fatal(err)
	}
	// r представляет источник загрузки с управляемой блокировкой Read и Close.
	r := &acceptanceHeldReader{reading: make(chan struct{}), closing: make(chan struct{}), release: make(chan struct{})}
	// releaseOnce обеспечивает однократное освобождение источника при явном вызове и очистке теста.
	var releaseOnce sync.Once
	// release снимает блокировку чтения и закрытия источника безопасно для повторных вызовов.
	release := func() {
		// Освобождает удерживаемые чтение и закрытие только при первом вызове.

		releaseOnce.Do(func() {
			// Закрывает канал, разрешающий завершение тестового источника.
			close(r.release)
		})
	}
	t.Cleanup(func() {
		// Освобождает источник и завершает остановку приложения после проверки позднего возврата подготовки.

		release()
		// ctx ограничивает завершающую очистку теста пятью секундами; cancel освобождает таймер
		// после позднего Shutdown.
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		_ = a.Shutdown(ctx)
		if !isClosed(a.Done()) {
			t.Error("поздняя подготовка не присоединена при очистке теста")
		}
	})
	// asset содержит заглушку, получающую намеренно незавершённую передачу файла.
	asset, err := a.core.CreateAsset(context.Background(), "Незавершённая передача")
	if err != nil {
		t.Fatal(err)
	}
	// job содержит принятое настоящее задание подготовки с удерживаемым источником загрузки.
	job, err := a.preparation.Upload(context.Background(), core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage}, r)
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-r.reading:
	case <-time.After(5 * time.Second):
		t.Fatal("настоящая подготовка не начала чтение")
	}
	// ctx ограничивает первую остановку 60 миллисекундами; cancel освобождает таймер после проверки
	// pending.
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
	// err содержит результат диагностики Store, который должен оставаться открытым при удерживаемой
	// подготовке.
	if _, err := a.store.Diagnostics(context.Background()); err != nil {
		t.Fatal("Store закрыт до завершения его пользователей", err)
	}
	if isClosed(a.sink.Done()) {
		t.Fatal("журнал закрыт до завершения пользователей")
	}
	// other содержит возможный ошибочный второй Store; err должен подтвердить сохранение
	// lifetime-блокировки исходного приложения.
	if other, err := storage.Open(context.Background(), c.DatabasePath(), c.StorageOptions()); !errors.Is(err, core.ErrInUse) {
		if other != nil {
			_ = other.Close()
		}
		t.Fatalf("pending handle потерял lifetime lock: %v", err)
	}
	release()
	// joined задаёт отдельный срок позднего завершения; stop освобождает его таймер после проверки
	// освобождения.
	joined, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	// result содержит итог задания и подтверждение Clean; err содержит ожидаемую рабочую ошибку
	// передачи после освобождения источника.
	if result, err := job.Wait(joined); err == nil || !result.Clean {
		t.Fatalf("задание должно завершиться с рабочей ошибкой и доказанной очисткой: %+v; %v", result, err)
	}
	_ = a.Shutdown(joined)
	if !isClosed(a.Done()) {
		t.Fatal("повторный Shutdown не присоединил позднее завершение")
	}
	// err содержит результат диагностики после позднего Shutdown, ожидается core.ErrClosed.
	if _, err := a.store.Diagnostics(context.Background()); !errors.Is(err, core.ErrClosed) {
		t.Fatalf("Store не закрыт после позднего join: %v", err)
	}
	// reopened содержит новое открытие БД после позднего освобождения её блокировки.
	reopened, err := storage.Open(joined, c.DatabasePath(), c.StorageOptions())
	if err != nil {
		t.Fatal("поздний join не освободил lifetime lock", err)
	}
	// err содержит отказ закрытия повторно открытого Store.
	if err := reopened.Close(); err != nil {
		t.Fatal(err)
	}
}

// Проверяет раздельный учёт исторической ошибки и завершения владельца, а также сохранение общих
// ресурсов при синхронно зависшем Close.
func TestAcceptanceShutdownRetriesHistoricalErrorAndRetainsSynchronousPending(t *testing.T) {
	t.Run("завершённая рабочая ошибка", func(t *testing.T) {
		// Проверяет повтор остановки после рабочей ошибки с сохранением этой ошибки и однократным закрытием
		// общего ресурса.

		// a моделирует приложение с ошибкой первой остановки завершённого менеджера.
		a := &Application{gate: make(chan struct{}, 1), done: make(chan struct{})}
		a.gate <- struct{}{}
		// failure задаёт историческую ошибку, сохраняемую после успешного повторного закрытия.
		failure := errors.New("ошибка завершённой работы")
		// calls считает попытки закрытия менеджера; shared считает закрытия общего Store для
		// проверки однократного освобождения.
		calls, shared := 0, 0
		a.owners.manager = newStopOwner("manager", func(context.Context) error {
			// Возвращает ошибку первого вызова остановки менеджера и подтверждает завершение вторым.

			calls++
			if calls == 1 {
				return failure
			}
			return nil
		})
		a.owners.store = sharedOwner("store", func(context.Context) error {
			// Считает закрытия общего хранилища для проверки однократного освобождения.
			shared++
			return nil
		})
		// ctx ограничивает остановку тестового приложения секундой; cancel освобождает таймер.
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		// err содержит итог первой остановки с исторической ошибкой и подтверждённым
		// завершением.
		err := a.Shutdown(ctx)
		if !errors.Is(err, failure) || errors.Is(err, ErrPending) || calls != 2 || shared != 1 || !isClosed(a.Done()) {
			t.Fatalf("рабочая ошибка смешана с join: calls=%d, shared=%d, err=%v", calls, shared, err)
		}
		// err содержит итог повторного Shutdown после закрытия Done для проверки сохранения
		// ошибки без новых закрытий.
		if err := a.Shutdown(ctx); !errors.Is(err, failure) || calls != 2 || shared != 1 {
			t.Fatalf("повторный Shutdown повторяет закрытия или теряет ошибку: %v", err)
		}
	})
	t.Run("синхронный Close", func(t *testing.T) {
		// Проверяет ограничение ожидания синхронного Close и сохранение общих ресурсов до его возврата.

		// a моделирует приложение с синхронно зависшим закрытием API и независимым ingest.
		a := &Application{gate: make(chan struct{}, 1), done: make(chan struct{})}
		a.gate <- struct{}{}
		// entered подтверждает начало Close API; otherStopped подтверждает независимую остановку
		// ingest; release разрешает завершиться Close API.
		entered, otherStopped, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
		// once обеспечивает однократное закрытие release при явном снятии блокировки и очистке.
		var once sync.Once
		// unblock освобождает синхронный Close API с защитой повторного вызова.
		unblock := func() {
			// Снимает блокировку синхронного Close только один раз.

			once.Do(func() {
				// Закрывает канал, разрешающий тестовому Close завершиться.
				close(release)
			})
		}
		t.Cleanup(func() {
			// Разблокирует владельца и ограниченно завершает остановку приложения после подтеста.

			unblock()
			// ctx ограничивает окончательную очистку подслучая секундой; cancel освобождает
			// таймер.
			ctx, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			_ = a.Shutdown(ctx)
			if !isClosed(a.Done()) {
				t.Error("синхронный тестовый владелец не присоединён")
			}
		})
		a.owners.api = newStopOwner("api", func(context.Context) error {
			// Сообщает о начале остановки API и блокирует её до разрешения теста.
			close(entered)
			<-release
			return nil
		})
		a.owners.ingest = newStopOwner("ingest", func(context.Context) error {
			// Подтверждает независимое начало остановки приёмника при заблокированном API.
			close(otherStopped)
			return nil
		})
		// shared считает закрытия Store, которые запрещены до возврата зависшего API.
		shared := 0
		a.owners.store = sharedOwner("store", func(context.Context) error {
			// Считает закрытия общего хранилища после подтверждения завершения независимых владельцев.
			shared++
			return nil
		})
		// ctx ограничивает первую попытку ожидания Close 60 миллисекундами; cancel освобождает
		// таймер.
		ctx, cancel := context.WithTimeout(context.Background(), 60*time.Millisecond)
		// err содержит результат остановки при удерживаемом API, ожидается ErrPending без
		// освобождения Store.
		err := a.Shutdown(ctx)
		cancel()
		if !errors.Is(err, ErrPending) || shared != 0 || isClosed(a.Done()) {
			t.Fatalf("неподтверждённый Close освободил shared: count=%d, err=%v", shared, err)
		}
		// name обозначает независимого владельца; event содержит его сигнал начала остановки для
		// проверки обоих владельцев.
		for name, event := range map[string]<-chan struct{}{"api": entered, "ingest": otherStopped} {
			select {
			case <-event:
			default:
				t.Fatalf("независимый stop %s не инициирован", name)
			}
		}
		unblock()
		// joined задаёт независимый срок позднего повторного Shutdown; stop освобождает его
		// таймер.
		joined, stop := context.WithTimeout(context.Background(), time.Second)
		defer stop()
		// err содержит итог остановки после освобождения синхронного Close API.
		if err := a.Shutdown(joined); err != nil || shared != 1 || !isClosed(a.Done()) {
			t.Fatalf("поздний синхронный Close не присоединён: shared=%d, err=%v", shared, err)
		}
	})
}

// Проверяет штатную остановку входа OBS, если запланированный Serve начинает выполняться уже после
// сигнала завершения.
func TestAcceptanceStopBeforeIngestServeStarts(t *testing.T) {
	// c задаёт временные данные и пределы RTMP для сценария остановки до начала Serve.
	c := acceptanceConfig(t, "unused-helper")
	// store содержит временное общее хранилище сценария; err содержит отказ его открытия.
	store, err := storage.Open(context.Background(), c.DatabasePath(), c.StorageOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает временное хранилище проверки позднего запуска Serve.
		_ = store.Close()
	})
	// service содержит доменный сервис того же Store для создания настоящего RTMP-сервера.
	service, err := core.NewService(store, store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	// ingest содержит настоящий приёмник с недоступным медиавходом, поскольку проверяется только
	// поздний Serve.
	ingest, err := rtmp.NewServer(rtmp.Options{Core: service, Route: rtmp.ConventionalRoute, Limits: rtmp.Limits(c.Ingress), Feed: func(core.Session) (*media.Router, error) {
		// Возвращает ErrNotReady вместо маршрутизатора, поскольку проверка касается остановки транспорта без
		// публикации медиа.
		return nil, core.ErrNotReady
	}})
	if err != nil {
		t.Fatal(err)
	}
	// listener содержит локальный TCP-слушатель, передаваемый отложенному RTMP Serve.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Освобождает слушатель, переданный отложенному запуску приёмника.
		_ = listener.Close()
	})
	// a моделирует приложение с ещё не начавшим обслуживаться ingest-слушателем.
	a := &Application{gate: make(chan struct{}, 1), done: make(chan struct{}), ingest: ingest, ingestListener: listener}
	a.gate <- struct{}{}
	// release удерживает запуск Serve до сигнала остановки приложения.
	release := make(chan struct{})
	// once обеспечивает однократный запуск отложенного Serve через serveCancel.
	var once sync.Once
	a.serveCancel = func() {
		// Однократно разрешает отложенному Serve продолжить работу при остановке приложения.

		once.Do(func() {
			// Закрывает канал, удерживающий запуск Serve до сигнала остановки.
			close(release)
		})
	}
	a.owners.ingest = newStopOwner("ingest", ingest.Shutdown)
	a.owners.ingestListener = listenerOwner("ingest_listener", listener)
	a.owners.store = sharedOwner("store", func(context.Context) error {
		// Закрывает хранилище после подтверждения остановки приёмника.
		return store.Close()
	})
	a.ingestServe = startOperation(func() stopResult {
		// Ожидает разрешение запуска Serve и сохраняет его результат после предварительной остановки
		// приёмника.

		<-release
		return stopResult{ingest.Serve(context.Background(), listener), true}
	})
	t.Cleanup(func() {
		// Разрешает отложенный Serve и завершает остановку приложения с отдельным сроком.

		a.serveCancel()
		// ctx ограничивает очистку теста секундой; cancel освобождает таймер после повторного
		// Shutdown.
		ctx, cancel := context.WithTimeout(context.Background(), time.Second)
		defer cancel()
		_ = a.Shutdown(ctx)
	})
	// ctx ограничивает основную остановку до позднего Serve секундой; cancel освобождает таймер.
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	// err содержит итог остановки, который должен принять штатный поздний возврат Serve.
	if err := a.Shutdown(ctx); err != nil || !isClosed(a.Done()) {
		t.Fatalf("запланированный после stop Serve превратил штатную остановку в отказ: %v", err)
	}
}

// Проверяет, что начальная сборка не переигрывает прежнюю Session и не откатывает сохранённый
// результат восстановления даже при последующем отказе сборки.
func TestAcceptancePopulatedRecoveryIsNotReplayedOrRolledBackByLateStartupFailure(t *testing.T) {
	// helper содержит настоящий helper для обоих вариантов проверки долговечного recovery.
	helper := acceptanceHelper(t)
	// lateFailure выбирает нормальную сборку либо отказ после создания API, когда recovery уже
	// завершён.
	for _, lateFailure := range []bool{false, true} {
		t.Run(fmt.Sprintf("late_failure=%v", lateFailure), func(t *testing.T) {
			// Проверяет сохранение прерывания прежней Session после нормального запуска либо поздней ошибки сборки
			// приложения.

			// c задаёт постоянные пути текущего сценария с первоначальным и последующим
			// открытием БД.
			c := acceptanceConfig(t, helper)
			// ctx ограничивает подготовку прежнего состояния и повторный запуск восемью
			// секундами; cancel освобождает таймер.
			ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
			defer cancel()
			// previous содержит первый Store с незавершённым запуском; err содержит отказ
			// его открытия.
			previous, err := storage.Open(ctx, c.DatabasePath(), c.StorageOptions())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				// Закрывает первоначальное хранилище с незавершённой командой запуска.
				_ = previous.Close()
			})
			// service содержит доменный сервис первого владельца для создания состояния
			// перед перезапуском.
			service, err := core.NewService(previous, previous.Incarnation())
			if err != nil {
				t.Fatal(err)
			}
			// asset содержит минимальную заглушку прежнего запуска без исходящих назначений.
			asset, err := service.CreateAsset(ctx, "Сохранённое поколение без выходов")
			if err != nil {
				t.Fatal(err)
			}
			// stream содержит поток, чья прежняя Session должна стать INTERRUPTED после
			// повторного открытия.
			stream, err := service.CreateStream(ctx, core.Stream{Name: "Прерванный запуск", PublicationKey: "synthetic-recovery-key", Fallback: core.FallbackPolicy{AssetID: asset.ID, Watchdog: core.WatchdogPolicy{VideoTimeout: time.Second, AudioTimeout: time.Second, RecoveryWindow: time.Second, KeyframeTimeout: time.Second}}})
			if err != nil {
				t.Fatal(err)
			}
			// generation содержит поколение минимальной заглушки для допуска прежнего
			// запуска.
			generation, err := service.ReservePreparation(ctx, core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage, GeneratorVersion: config.GeneratorVersion})
			if err != nil {
				t.Fatal(err)
			}
			// fence содержит точную попытку подготовки поколения прежним владельцем.
			fence, err := service.ClaimPreparation(ctx, generation.ID)
			if err != nil {
				t.Fatal(err)
			}
			// err содержит отказ фиксации выданной попытки прежнего поколения.
			if err := service.MarkPreparationIssued(ctx, fence); err != nil {
				t.Fatal(err)
			}
			// err содержит отказ публикации минимальных метаданных прежнего поколения.
			if _, err := service.PublishPreparation(ctx, fence, core.GenerationPublication{SourceHash: "synthetic-recovery-source", SizeBytes: 1, SourceFormat: core.SourceFormat{ImageFormat: "png"}}); err != nil {
				t.Fatal(err)
			}
			// err содержит отказ подтверждения очистки прежней подготовки до запуска
			// Session.
			if err := service.AcknowledgePreparationCleanup(ctx, fence); err != nil {
				t.Fatal(err)
			}
			// receipt содержит незавершённую команду запуска, которая должна прерваться при
			// новом открытии Store.
			receipt, err := service.StartSession(ctx, core.StartSessionRequest{RequestID: "pending-before-restart", StreamID: stream.ID})
			if err != nil {
				t.Fatal(err)
			}
			// err содержит отказ закрытия первоначального Store перед восстановлением.
			if err := previous.Close(); err != nil {
				t.Fatal(err)
			}
			// d задаёт настоящую последующую сборку и при необходимости точку позднего
			// отказа.
			d := acceptanceDependencies(&acceptanceLog{})
			// failure задаёт узнаваемый поздний отказ после уже сохранённого recovery.
			failure := errors.New("поздний отказ после committed recovery")
			if lateFailure {
				d.afterAcquire = func(stage string, _ *Application) error {
					// Имитирует позднюю ошибку после создания API, когда восстановление хранилища уже завершено.

					if stage == "api" {
						return failure
					}
					return nil
				}
			}
			// a содержит результат последующей сборки, включая частичного владельца при
			// lateFailure.
			a, err := start(ctx, c, d)
			if lateFailure {
				if !errors.Is(err, failure) || a == nil || !isClosed(a.Done()) {
					t.Fatalf("поздняя ошибка потеряна или не завершила startup: %v", err)
				}
			} else {
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() {
					// Останавливает успешно запущенное приложение перед повторным открытием БД.
					acceptanceStop(t, a)
				})
				// stored содержит прежнюю Session после запуска приложения; err содержит
				// отказ её чтения для проверки отсутствия replay.
				stored, err := a.core.GetSession(ctx, receipt.SessionID)
				if err != nil || stored.State != core.SessionInterrupted {
					t.Fatalf("bootstrap replay старой Session: %+v; %v", stored, err)
				}
				// anchor содержит текущий RAM-снимок, который должен оставаться без
				// наблюдений прежней Session.
				anchor := acceptanceHTTP(t, a, "GET", "/events/snapshot", "", 200)
				if len(anchor["sessions"].([]any)) != 0 {
					t.Fatal("старый запуск восстановлен как текущий RAM runtime", anchor)
				}
				acceptanceStop(t, a)
			}
			// reopened содержит Store после завершения проверяемого запуска для чтения
			// окончательных долговечных результатов.
			reopened, err := storage.Open(ctx, c.DatabasePath(), c.StorageOptions())
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				// Закрывает повторно открытое хранилище после проверки долговечных результатов восстановления.
				_ = reopened.Close()
			}()
			// read содержит новый доменный сервис для проверки сохранности recovery после
			// закрытия приложения.
			read, err := core.NewService(reopened, reopened.Incarnation())
			if err != nil {
				t.Fatal(err)
			}
			// stored содержит прежнюю Session после повторного открытия для проверки
			// INTERRUPTED и исходного incarnation.
			stored, err := read.GetSession(ctx, receipt.SessionID)
			if err != nil || stored.State != core.SessionInterrupted || stored.Incarnation != previous.Incarnation() {
				t.Fatalf("committed recovery потерян: %+v; %v", stored, err)
			}
			// command содержит прежнюю квитанцию запуска, которая должна сохранять статус
			// INTERRUPTED.
			command, err := read.GetReceipt(ctx, receipt.ID)
			if err != nil || command.Status != core.CommandInterrupted {
				t.Fatalf("старая pending команда повторно принята: %+v; %v", command, err)
			}
		})
	}
}
