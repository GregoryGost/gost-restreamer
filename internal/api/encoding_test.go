package api

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"math"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// Проверяет точную сериализацию Decimal и произвольных байтов ProfileText, отклоняя недопустимые числа
// и представления base64.
func TestDecimalAndProfileTextPreserveExactValues(t *testing.T) {
	// v задаёт очередное точное uint64, включая значения за пределами точности JavaScript.
	for _, v := range []uint64{0, 9007199254740993, math.MaxUint64} {
		// b содержит JSON-строку Decimal; e содержит отказ её сериализации.
		b, e := json.Marshal(Decimal(v))
		if e != nil {
			t.Fatal(e)
		}
		// decoded принимает Decimal после обратного JSON-чтения для сравнения точного uint64.
		var decoded Decimal
		if e = json.Unmarshal(b, &decoded); e != nil || uint64(decoded) != v {
			t.Fatal(string(b), decoded, e)
		}
	}
	// s задаёт обычную UTF-8-строку, пустоту или произвольные невалидные UTF-8 байты профиля.
	for _, s := range []string{"", "Обычная строка", string([]byte{0xff, 0xfe, 'a'})} {
		// b содержит выбранное JSON-представление ProfileText; e содержит отказ сериализации.
		b, e := json.Marshal(ProfileText(s))
		if e != nil {
			t.Fatal(e)
		}
		// decoded принимает ProfileText после обратного чтения для проверки неизменных байтов.
		var decoded ProfileText
		if e = json.Unmarshal(b, &decoded); e != nil || string(decoded) != s {
			t.Fatal(string(b), e)
		}
	}
	// b задаёт очередное недопустимое JSON-представление ProfileText.
	for _, b := range []string{`null`, `1`, `{"encoding":"hex","value":"ff"}`, `{"encoding":"base64","value":"_bad"}`, `{"encoding":"base64","value":"YQ==","extra":true}`} {
		// decoded принимает возможное ошибочно допущенное значение неверного ProfileText.
		var decoded ProfileText
		if json.Unmarshal([]byte(b), &decoded) == nil {
			t.Fatalf("Недопустимое представление принято: %s", b)
		}
	}
	// s задаёт недопустимую десятичную запись: пустую, с ведущим нулём, знаком или переполнением.
	for _, s := range []string{"", "01", "-1", "18446744073709551616"} {
		// e содержит ожидаемый отказ разбора текущей недопустимой Decimal-строки.
		if _, e := parseDecimal(s); e == nil {
			t.Fatal(s)
		}
	}
}

// Проверяет восстановление cursor без раскрытия incarnation и отклонение неполной, чужой версии или
// лишней структуры.
func TestCursorRejectsUntrustedStructureWithoutInternalIdentity(t *testing.T) {
	// current задаёт внутренний incarnation, публичную эпоху и максимальный точный номер события.
	current := core.RuntimeCursor{Incarnation: "private-process-identity", Epoch: "public-epoch", Sequence: math.MaxUint64}
	// encoded содержит публичный курсор без внутреннего incarnation.
	encoded := encodeCursor(current)
	// raw содержит JSON-байты курсора; e содержит отказ обратного base64url-декодирования.
	raw, e := base64.RawURLEncoding.DecodeString(encoded)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(raw), current.Incarnation) {
		t.Fatal("Курсор раскрывает внутреннего владельца")
	}
	// got содержит восстановленный курсор с отдельной текущей внутренней идентичностью.
	got, e := decodeCursor(encoded, current)
	if e != nil || got != current {
		t.Fatal(got, e)
	}
	// raw задаёт очередной неполный, чужой версии или содержащий лишние данные JSON-курсор.
	for _, raw := range []string{`{}`, `{"v":1,"epoch":"e"}`, `{"v":2,"epoch":"e","sequence":"1"}`, `{"v":1,"epoch":"e","sequence":"1","extra":true}`, `{"v":1,"epoch":"e","sequence":"1"} {}`} {
		// e содержит ожидаемый отказ разбора неподдержанной структуры курсора.
		if _, e := decodeCursor(base64.RawURLEncoding.EncodeToString([]byte(raw)), current); e == nil {
			t.Fatalf("Некорректный курсор принят: %s", raw)
		}
	}
}

// Проверяет ограничение SSE-кадров и JSON-экранирование данных без внедрения новых заголовков событий.
func TestFramesAreBoundedAndEncodeDataWithoutFrameInjection(t *testing.T) {
	// frame содержит SSE-кадр с экранированными переводами строк данных; e содержит отказ его
	// формирования.
	frame, e := makeFrame("state", "cursor", map[string]string{"value": "line\nevent: injected\r\ndata: secret"}, 1024)
	if e != nil || strings.Count(string(frame), "event:") != 2 || strings.Contains(string(frame), "\nevent: injected") {
		t.Fatal(string(frame), e)
	}
	// part содержит недопустимое имя события с переводом строки или NUL.
	for _, part := range []string{"bad\nevent", "bad\rvalue", "bad\x00value"} {
		// e содержит отказ формирования кадра с недопустимым заголовком события.
		if _, e := makeFrame(part, "", nil, 1024); e == nil {
			t.Fatal("Недопустимый заголовок кадра принят")
		}
	}
	// e содержит отказ формирования кадра сверх байтового бюджета.
	if _, e := makeFrame("metrics", "", strings.Repeat("x", 1024), 256); !errors.Is(e, core.ErrObservationLimit) {
		t.Fatal(e)
	}
	// e содержит отказ JSON-сериализации NaN до записи SSE-кадра.
	if _, e := makeFrame("metrics", "", math.NaN(), 1024); e == nil {
		t.Fatal("Недопустимый JSON принят")
	}
}

// Проверяет различие измеренного нуля и недоступности в DTO и сохранение точных байтов медиаформата.
func TestMetricProjectionSeparatesUnavailableAndMeasuredZero(t *testing.T) {
	// zero содержит публичную измеренную нулевую скорость с доступным значением.
	zero := rateDTO(core.Rate{Status: core.MetricAvailable, Value: 0, Duration: time.Nanosecond})
	// absent содержит публичную недоступность без численного значения.
	absent := rateDTO(core.Rate{})
	if zero.Value == nil || *zero.Value != 0 || absent.Value != nil || absent.Status != "unavailable" {
		t.Fatal(zero, absent)
	}
	// gpu содержит проекцию устаревшего GPU-поля, численное значение которого должно быть null.
	gpu := gpuMetricDTO(core.GPUMetric{Status: core.MetricStale, Value: 42})
	if gpu.Value != nil {
		t.Fatal("Устаревшее значение выдано как измеренное")
	}
	// before задаёт доменный медиаформат с невалидными UTF-8 байтами строковых полей.
	before := core.MediaFormat{Video: core.VideoFormat{CodecProfile: string([]byte{0xff}), Color: core.ColorInfo{Range: string([]byte{0x80})}}, Audio: core.AudioFormat{ChannelLayout: string([]byte{0xfe})}}
	// after содержит медиаформат после полного преобразования в DTO и обратно.
	after := formatDTO(before).domain()
	if !reflect.DeepEqual(before, after) {
		t.Fatal("Преобразование изменило байты профиля")
	}
}

// Проверяет установленный приоритет HTTP-классификации объединённых ошибок домена и явных транспортных
// ошибок.
func TestErrorClassificationPreservesIntentionalJoinedErrorPrecedence(t *testing.T) {
	// tc задаёт исходную либо объединённую ошибку и ожидаемый приоритет HTTP-статуса.
	for _, tc := range []struct {
		// err содержит проверяемую доменную или транспортную ошибку, в том числе объединённые
		// причины.
		err error
		// status задаёт ожидаемый HTTP-статус для текущей цепочки ошибок.
		status int
	}{{core.ErrNotFound, 404}, {core.ErrConflict, 409}, {core.ErrInvalid, 422}, {errors.Join(core.ErrNotFound, core.ErrClosed), 404}, {errors.Join(core.ErrConflict, core.ErrInvalid), 409}, {errors.New("private-error"), 500}, {statusError(408), 408}} {
		// actual содержит результат установленного порядка классификации текущей ошибки.
		if actual := classify(tc.err); actual != tc.status {
			t.Fatal(tc.err, actual, tc.status)
		}
	}
}

// closedUnitOfWork имитирует закрытое хранилище для проверки транспортного поведения без операций БД.
type closedUnitOfWork struct{}

// View имитирует закрытое хранилище, не вызывая переданную функцию чтения.
func (closedUnitOfWork) View(context.Context, func(core.Repositories) error) error {
	return core.ErrClosed
}

// Write имитирует закрытое хранилище, не вызывая переданную функцию записи.
func (closedUnitOfWork) Write(context.Context, func(core.Repositories) error) error {
	return core.ErrClosed
}

// invalidTimestampCapabilities возвращает непредставимое JSON-время для проверки безопасного отказа до
// заголовков.
type invalidTimestampCapabilities struct{}

// Snapshot возвращает время за пределами представимости JSON для проверки отказа до записи
// HTTP-заголовков.
func (invalidTimestampCapabilities) Snapshot(context.Context) (CapabilitiesDTO, error) {
	// at задаёт год 10000, который нельзя сериализовать стандартным time.Time JSON.
	at := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	return CapabilitiesDTO{FFmpeg: CapabilityDTO{Status: "unavailable", Reason: "executable_unavailable", CheckedAt: &at}}, nil
}

// Проверяет безопасный статус 500 для непредставимого ответа до публикации заголовков и без исходного
// ошибочного значения.
func TestUnrepresentableResponseReturnsSafeProblemBeforeHeaders(t *testing.T) {
	// service содержит доменный сервис закрытого хранилища; e содержит отказ его создания.
	service, e := core.NewService(closedUnitOfWork{}, "local-owner")
	if e != nil {
		t.Fatal(e)
	}
	// a содержит настоящий API с поставщиком непредставимого времени для проверки ответа 500.
	a, e := New(Config{Core: service, Capabilities: invalidTimestampCapabilities{}, Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), Limits: Limits{JSONBytes: 4096, UploadBytes: 4096, UploadReadTimeout: time.Second, OperationTimeout: time.Second, ShutdownTimeout: time.Second, SSEClients: 1, SSEPageSize: 1, SSEFrameBytes: 1024, SSEWriteTimeout: time.Second, SSEHeartbeat: time.Second}})
	if e != nil {
		t.Fatal(e)
	}
	defer func() {
		// Закрывает тестовый API после проверки безопасного ответа на несериализуемое время.

		// e содержит результат закрытия API после проверки безопасной ошибки.
		if e := a.Close(context.Background()); e != nil {
			t.Error(e)
		}
	}()
	// response записывает статус, заголовки и тело настоящего маршрута capabilities.
	response := httptest.NewRecorder()
	a.ServeHTTP(response, httptest.NewRequest("GET", Prefix+"/system/capabilities", nil))
	if response.Code != 500 || !strings.HasPrefix(response.Header().Get("Content-Type"), "application/problem+json") || strings.Contains(response.Body.String(), "10000") {
		t.Fatal(response.Code, response.Body.String())
	}
}
