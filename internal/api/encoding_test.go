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

func TestDecimalAndProfileTextPreserveExactValues(t *testing.T) {
	for _, v := range []uint64{0, 9007199254740993, math.MaxUint64} {
		b, e := json.Marshal(Decimal(v))
		if e != nil {
			t.Fatal(e)
		}
		var decoded Decimal
		if e = json.Unmarshal(b, &decoded); e != nil || uint64(decoded) != v {
			t.Fatal(string(b), decoded, e)
		}
	}
	for _, s := range []string{"", "Обычная строка", string([]byte{0xff, 0xfe, 'a'})} {
		b, e := json.Marshal(ProfileText(s))
		if e != nil {
			t.Fatal(e)
		}
		var decoded ProfileText
		if e = json.Unmarshal(b, &decoded); e != nil || string(decoded) != s {
			t.Fatal(string(b), e)
		}
	}
	for _, b := range []string{`null`, `1`, `{"encoding":"hex","value":"ff"}`, `{"encoding":"base64","value":"_bad"}`, `{"encoding":"base64","value":"YQ==","extra":true}`} {
		var decoded ProfileText
		if json.Unmarshal([]byte(b), &decoded) == nil {
			t.Fatalf("Недопустимое представление принято: %s", b)
		}
	}
	for _, s := range []string{"", "01", "-1", "18446744073709551616"} {
		if _, e := parseDecimal(s); e == nil {
			t.Fatal(s)
		}
	}
}
func TestCursorRejectsUntrustedStructureWithoutInternalIdentity(t *testing.T) {
	current := core.RuntimeCursor{Incarnation: "private-process-identity", Epoch: "public-epoch", Sequence: math.MaxUint64}
	encoded := encodeCursor(current)
	raw, e := base64.RawURLEncoding.DecodeString(encoded)
	if e != nil {
		t.Fatal(e)
	}
	if strings.Contains(string(raw), current.Incarnation) {
		t.Fatal("Курсор раскрывает внутреннего владельца")
	}
	got, e := decodeCursor(encoded, current)
	if e != nil || got != current {
		t.Fatal(got, e)
	}
	for _, raw := range []string{`{}`, `{"v":1,"epoch":"e"}`, `{"v":2,"epoch":"e","sequence":"1"}`, `{"v":1,"epoch":"e","sequence":"1","extra":true}`, `{"v":1,"epoch":"e","sequence":"1"} {}`} {
		if _, e := decodeCursor(base64.RawURLEncoding.EncodeToString([]byte(raw)), current); e == nil {
			t.Fatalf("Некорректный курсор принят: %s", raw)
		}
	}
}
func TestFramesAreBoundedAndEncodeDataWithoutFrameInjection(t *testing.T) {
	frame, e := makeFrame("state", "cursor", map[string]string{"value": "line\nevent: injected\r\ndata: secret"}, 1024)
	if e != nil || strings.Count(string(frame), "event:") != 2 || strings.Contains(string(frame), "\nevent: injected") {
		t.Fatal(string(frame), e)
	}
	for _, part := range []string{"bad\nevent", "bad\rvalue", "bad\x00value"} {
		if _, e := makeFrame(part, "", nil, 1024); e == nil {
			t.Fatal("Недопустимый заголовок кадра принят")
		}
	}
	if _, e := makeFrame("metrics", "", strings.Repeat("x", 1024), 256); !errors.Is(e, core.ErrObservationLimit) {
		t.Fatal(e)
	}
	if _, e := makeFrame("metrics", "", math.NaN(), 1024); e == nil {
		t.Fatal("Недопустимый JSON принят")
	}
}
func TestMetricProjectionSeparatesUnavailableAndMeasuredZero(t *testing.T) {
	zero := rateDTO(core.Rate{Status: core.MetricAvailable, Value: 0, Duration: time.Nanosecond})
	absent := rateDTO(core.Rate{})
	if zero.Value == nil || *zero.Value != 0 || absent.Value != nil || absent.Status != "unavailable" {
		t.Fatal(zero, absent)
	}
	gpu := gpuMetricDTO(core.GPUMetric{Status: core.MetricStale, Value: 42})
	if gpu.Value != nil {
		t.Fatal("Устаревшее значение выдано как измеренное")
	}
	before := core.MediaFormat{Video: core.VideoFormat{CodecProfile: string([]byte{0xff}), Color: core.ColorInfo{Range: string([]byte{0x80})}}, Audio: core.AudioFormat{ChannelLayout: string([]byte{0xfe})}}
	after := formatDTO(before).domain()
	if !reflect.DeepEqual(before, after) {
		t.Fatal("Преобразование изменило байты профиля")
	}
}
func TestErrorClassificationPreservesIntentionalJoinedErrorPrecedence(t *testing.T) {
	for _, tc := range []struct {
		err    error
		status int
	}{{core.ErrNotFound, 404}, {core.ErrConflict, 409}, {core.ErrInvalid, 422}, {errors.Join(core.ErrNotFound, core.ErrClosed), 404}, {errors.Join(core.ErrConflict, core.ErrInvalid), 409}, {errors.New("private-error"), 500}, {statusError(408), 408}} {
		if actual := classify(tc.err); actual != tc.status {
			t.Fatal(tc.err, actual, tc.status)
		}
	}
}

type closedUnitOfWork struct{}

func (closedUnitOfWork) View(context.Context, func(core.Repositories) error) error {
	return core.ErrClosed
}
func (closedUnitOfWork) Write(context.Context, func(core.Repositories) error) error {
	return core.ErrClosed
}

type invalidTimestampCapabilities struct{}

func (invalidTimestampCapabilities) Snapshot(context.Context) (CapabilitiesDTO, error) {
	at := time.Date(10000, 1, 1, 0, 0, 0, 0, time.UTC)
	return CapabilitiesDTO{FFmpeg: CapabilityDTO{Status: "unavailable", Reason: "executable_unavailable", CheckedAt: &at}}, nil
}
func TestUnrepresentableResponseReturnsSafeProblemBeforeHeaders(t *testing.T) {
	service, e := core.NewService(closedUnitOfWork{}, "local-owner")
	if e != nil {
		t.Fatal(e)
	}
	a, e := New(Config{Core: service, Capabilities: invalidTimestampCapabilities{}, Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), Limits: Limits{JSONBytes: 4096, UploadBytes: 4096, UploadReadTimeout: time.Second, OperationTimeout: time.Second, ShutdownTimeout: time.Second, SSEClients: 1, SSEPageSize: 1, SSEFrameBytes: 1024, SSEWriteTimeout: time.Second, SSEHeartbeat: time.Second}})
	if e != nil {
		t.Fatal(e)
	}
	defer func() {
		if e := a.Close(context.Background()); e != nil {
			t.Error(e)
		}
	}()
	response := httptest.NewRecorder()
	a.ServeHTTP(response, httptest.NewRequest("GET", Prefix+"/system/capabilities", nil))
	if response.Code != 500 || !strings.HasPrefix(response.Header().Get("Content-Type"), "application/problem+json") || strings.Contains(response.Body.String(), "10000") {
		t.Fatal(response.Code, response.Body.String())
	}
}
