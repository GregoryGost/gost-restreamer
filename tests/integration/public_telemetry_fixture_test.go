package integration

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/api"
	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

// Создаёт API для чтения телеметрии общего сервиса с ограничениями JSON и SSE; ограниченное завершение
// зарегистрировано в t.Cleanup.
func publicTelemetryAPI(t *testing.T, service *core.Service) *api.API {
	t.Helper()
	// a предоставляет публичные маршруты телеметрии тестового сервиса, а err сообщает об ошибке
	// создания API.
	a, err := api.New(api.Config{Core: service, Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), Limits: api.Limits{JSONBytes: 16384, UploadBytes: 65536, UploadReadTimeout: time.Second, OperationTimeout: 2 * time.Second, ShutdownTimeout: 2 * time.Second, SSEClients: 2, SSEPageSize: 8, SSEFrameBytes: 128 << 10, SSEWriteTimeout: time.Second, SSEHeartbeat: time.Second}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Останавливает API публичной телеметрии с отдельным сроком.

		// ctx ограничивает завершение API тремя секундами, а cancel освобождает таймер очистки.
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		// err сообщает об ошибке завершения API телеметрии при очистке теста.
		if err := a.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return a
}

// publicTelemetryFrame хранит разобранный SSE-кадр либо ошибку его получения.
type publicTelemetryFrame struct {
	// kind содержит имя SSE-события, а id — его курсор для проверки связи с JSON-полезной нагрузкой.
	kind, id string
	// data содержит JSON-объект полезной нагрузки разобранного SSE-кадра.
	data map[string]any
	// err хранит ошибку чтения SSE либо разбора JSON текущего кадра.
	err error
}

// publicTelemetryStream связывает читателя SSE с каналами кадров, отменой и закрытием HTTP-тела.
type publicTelemetryStream struct {
	// frames предоставляет ограниченный поток разобранных кадров для проверок теста.
	frames <-chan publicTelemetryFrame
	// done закрывается после завершения горутины разбора SSE для отдельного ожидания очистки.
	done <-chan struct{}
	// cancel отменяет контекст HTTP-подписки и ожидания передачи кадров.
	cancel context.CancelFunc
	// body содержит тело SSE-ответа, закрываемое при завершении подписки.
	body io.ReadCloser
}

// Открывает SSE с заданным cursor либо Last-Event-ID и разбирает кадры в ограниченный канал. Подписка
// имеет общий срок 15 секунд; t.Cleanup отменяет чтение, закрывает тело ответа и ждёт завершения
// горутины.
func publicTelemetrySubscribe(t *testing.T, server *httptest.Server, path, lastID string) *publicTelemetryStream {
	t.Helper()
	// ctx ограничивает HTTP-подписку пятнадцатью секундами, а cancel прекращает её чтение и
	// освобождает таймер.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	// request содержит GET-запрос SSE с контекстом подписки, а err сообщает об ошибке его создания.
	request, err := http.NewRequestWithContext(ctx, "GET", server.URL+api.Prefix+path, nil)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if lastID != "" {
		request.Header.Set("Last-Event-ID", lastID)
	}
	// response содержит открытый HTTP-ответ для проверки статуса и чтения SSE.
	response, err := server.Client().Do(request)
	if err != nil {
		cancel()
		t.Fatal(err)
	}
	if response.StatusCode != 200 || !strings.HasPrefix(response.Header.Get("Content-Type"), "text/event-stream") {
		_ = response.Body.Close()
		cancel()
		t.Fatalf("SSE не открыт: %s %v", response.Status, response.Header)
	}
	// frames передаёт не более 256 разобранных кадров, а done подтверждает завершение горутины
	// читателя.
	frames, done := make(chan publicTelemetryFrame, 256), make(chan struct{})
	// s связывает открытый SSE-ответ с каналами и отменой для последующего чтения и очистки.
	s := &publicTelemetryStream{frames: frames, done: done, cancel: cancel, body: response.Body}
	go func() {
		// Разбирает SSE построчно, передаёт полные кадры в ограниченный канал и сообщает ошибки чтения до
		// подтверждения завершения.

		defer close(done)
		defer close(frames)
		// scanner разбирает тело SSE построчно с пределом строки 128 КиБ.
		scanner := bufio.NewScanner(response.Body)
		scanner.Buffer(make([]byte, 4096), 128<<10)
		// frame накапливает поля текущего SSE-кадра до пустой строки-разделителя.
		frame := publicTelemetryFrame{}
		// send передаёт полный кадр тесту и прекращает ожидание при отмене подписки.
		send := func(v publicTelemetryFrame) bool {
			// Передаёт кадр читателю теста либо прерывает ожидание по отмене подписки.

			select {
			case frames <- v:
				return true
			case <-ctx.Done():
				return false
			}
		}
		for scanner.Scan() {
			// line содержит очередную строку SSE для разбора event, id и JSON-поля data.
			line := scanner.Text()
			if line == "" {
				if frame.kind != "" && !send(frame) {
					return
				}
				frame = publicTelemetryFrame{}
				continue
			}
			switch {
			case strings.HasPrefix(line, "event: "):
				frame.kind = strings.TrimPrefix(line, "event: ")
			case strings.HasPrefix(line, "id: "):
				frame.id = strings.TrimPrefix(line, "id: ")
			case strings.HasPrefix(line, "data: "):
				frame.err = json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &frame.data)
			}
		}
		// err сообщает об ошибке сканирования SSE, возникшей до отмены подписки.
		if err := scanner.Err(); err != nil && ctx.Err() == nil {
			_ = send(publicTelemetryFrame{err: err})
		}
	}()
	t.Cleanup(func() {
		// Закрывает подписку и ограниченно ожидает завершения читателя SSE.
		s.close(t)
	})
	return s
}

// Отменяет подписку, закрывает тело HTTP-ответа и ждёт завершения читателя не более трёх секунд;
// превышение срока сообщает ошибкой теста.
func (s *publicTelemetryStream) close(t *testing.T) {
	t.Helper()
	s.cancel()
	_ = s.body.Close()
	select {
	case <-s.done:
	case <-time.After(3 * time.Second):
		t.Error("чтение SSE не присоединено")
	}
}

// Ожидает следующий SSE-кадр до десяти секунд и проверяет согласованность id, cursor и kind у обычных
// событий. Закрытый поток возвращает false, ошибка разбора завершает тест.
func (s *publicTelemetryStream) next(t *testing.T) (publicTelemetryFrame, bool) {
	t.Helper()
	select {
	// frame содержит очередной разобранный кадр подписки, а ok показывает, открыт ли ещё канал
	// кадров.
	case frame, ok := <-s.frames:
		if frame.err != nil {
			t.Fatal("разбор SSE:", frame.err)
		}
		if ok && frame.kind != "resync_required" && (frame.id == "" || frame.data["cursor"] != frame.id || frame.data["kind"] != frame.kind) {
			t.Fatalf("SSE потерял связь cursor и payload: %+v", frame)
		}
		return frame, ok
	case <-time.After(10 * time.Second):
		t.Fatal("следующий кадр SSE не получен до срока")
		return publicTelemetryFrame{}, false
	}
}

// Конечный источник оставляет UDS открытым: счётчики успевают попасть в RAM без нового медиа.
func TestPublicTelemetryFiniteMediaChild(t *testing.T) {
	// index хранит позицию служебного аргумента дочернего источника; значение -1 означает обычный
	// запуск теста.
	index := -1
	// i задаёт позицию аргумента процесса, а arg содержит его текст для поиска режима дочернего
	// источника.
	for i, arg := range os.Args {
		if arg == "--public-telemetry" {
			index = i
			break
		}
	}
	if index < 0 {
		return
	}
	// args содержит путь выходного UDS и путь маркера окончания, переданные дочернему источнику.
	args := os.Args[index+1:]
	if len(args) != 2 {
		t.Fatal("неверные аргументы конечного источника")
	}
	// connection связывает дочерний источник с выходным UDS, а err сообщает об ошибке подключения.
	connection, err := net.Dial("unix", args[0])
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// Закрывает UDS при выходе из функции конечного тестового источника.
		_ = connection.Close()
	}()
	// video содержит AV1-конфигурацию, а audio — AAC-конфигурацию структурных пакетов конечного
	// источника.
	video, audio := publisherConfigs(t, publisherProfile(core.CodecAV1, 640, 360), 1)
	// mux записывает синтетические видео- и аудиопакеты дочернего источника в fMP4 через UDS.
	mux, err := fmp4.NewMuxer(connection, fmp4.Init{Video: video, Audio: audio}, integrationLimits())
	if err != nil {
		t.Fatal(err)
	}
	// ticker задаёт частоту подачи шестидесяти видеоинтервалов в секунду.
	ticker := time.NewTicker(time.Second / 60)
	defer ticker.Stop()
	// audioTicks хранит следующую аудиометку в отсчётах 48000 Гц с шагом полного AAC-пакета 1024.
	var audioTicks int64
	// frame задаёт номер одного из шестидесяти видеоинтервалов конечного источника.
	for frame := range int64(60) {
		// packet содержит очередную структурную единицу доступа для записи в fMP4, а err
		// сообщает об ошибке её создания.
		packet, err := media.NewPacket(media.PacketInfo{PTS: frame, DTS: frame, DurationTicks: 1, Keyframe: true}, video, publisherPayload(video, 10))
		if err != nil || mux.WritePacket(packet) != nil {
			t.Fatal("передача конечного видео:", err)
		}
		for audioTicks*60 < (frame+1)*48000 {
			packet, err = media.NewPacket(media.PacketInfo{PTS: audioTicks, DTS: audioTicks, DurationTicks: 1024}, audio, publisherPayload(audio, 0))
			if err != nil || mux.WritePacket(packet) != nil {
				t.Fatal("передача конечного аудио:", err)
			}
			audioTicks += 1024
		}
		<-ticker.C
	}
	// err сообщает об ошибке записи маркера завершённой подачи медиапакетов.
	if err := os.WriteFile(args[1], []byte("complete"), 0600); err != nil {
		t.Fatal(err)
	}
	_, _ = io.Copy(io.Discard, connection)
}
