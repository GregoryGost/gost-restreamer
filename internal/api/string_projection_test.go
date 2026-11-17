package api

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"math"
	"net/http/httptest"
	"reflect"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// TestMeasuredFormatPreservesProducerLevels проверяет каждый uint8-уровень H264 вне желаемых настроек. Числа сохраняют физические границы и исходную дробь FPS.
func TestMeasuredFormatPreservesProducerLevels(t *testing.T) {
	// before содержит измерение с максимальными физическими числами и H264 без желаемого профиля.
	before := core.MediaFormat{Video: core.VideoFormat{Codec: core.CodecH264, Width: math.MaxUint32, Height: math.MaxUint32, FrameRate: core.FrameRate{Numerator: math.MaxUint32, Denominator: math.MaxUint32 - 1}, BitDepth: math.MaxUint8}, Audio: core.AudioFormat{Codec: core.CodecAAC, SampleRateHz: math.MaxUint32, Channels: math.MaxUint16}}
	// number задаёт каждый исходный байт H264 level_idc.
	for number := 0; number <= 255; number++ {
		before.Video.Level = strconv.Itoa(number/10) + "." + strconv.Itoa(number%10)
		// after содержит публичную проекцию текущего уровня.
		after := measuredFormatDTO(before)
		if after.Video.Level != before.Video.Level || after.Video.Width != before.Video.Width || after.Video.Height != before.Video.Height || after.Video.BitDepth != before.Video.BitDepth || after.Audio.Channels != before.Audio.Channels || after.Audio.SampleRateHz != before.Audio.SampleRateHz || after.Video.FrameRate != (FrameRateDTO{math.MaxUint32, math.MaxUint32 - 1}) {
			t.Fatal("Измерение изменено", before, after)
		}
	}
	// value представляет пустое измерение, неизвестный текст либо неверный UTF-8.
	for _, value := range []string{"", "unknown-producer-value", string([]byte{0xff})} {
		// want сохраняет пустоту отдельно от неизвестного непустого измерения.
		want := "UNKNOWN"
		if value == "" {
			want = ""
		}
		if knownObserved(value, observedValues("video.codec")) != want {
			t.Fatal(value, want)
		}
	}
}

// TestVariantProjectionKeepsRawMeasurement проверяет ненулевой format во всех состояниях варианта
// и отсутствие изменений исходного raw-измерения при замене неизвестного текста на UNKNOWN.
func TestVariantProjectionKeepsRawMeasurement(t *testing.T) {
	// state задаёт все долговечные состояния варианта, включая пустое и старое измерения FAILED.
	for _, state := range []core.VariantState{core.VariantPreparing, core.VariantReady, core.VariantFailed} {
		// variant содержит raw-измерение неизвестного производителя с неверными UTF-8 байтами.
		variant := core.FallbackVariant{State: state, VerifiedFormat: core.MediaFormat{Video: core.VideoFormat{Codec: core.CodecH264, CodecProfile: "h264-profile-idc-255", Level: "25.5", Color: core.ColorInfo{Primaries: string([]byte{0xff})}}}}
		// before сохраняет исходные данные; result содержит публичный DTO без изменения raw.
		before := variant
		result := variantDTO(variant)
		// encoded и err содержат сериализованный результат и возможный отказ JSON.
		encoded, err := json.Marshal(result)
		if err != nil || !strings.Contains(string(encoded), `"format":{`) || result.Format.Video.CodecProfile != "UNKNOWN" || result.Format.Video.Level != "25.5" || result.Format.Video.Color.Primaries != "UNKNOWN" || !reflect.DeepEqual(before, variant) {
			t.Fatal(result, err)
		}
	}
	// zero содержит вариант до появления какого-либо измерения.
	zero := variantDTO(core.FallbackVariant{})
	if zero.Format.Video.Codec != "" || zero.Format.Audio.Codec != "" {
		t.Fatal(zero)
	}
}

// untrustedCapabilities передаёт произвольные строки внешней реализации поставщика возможностей.
type untrustedCapabilities struct {
	// value задаёт снимок, возвращаемый без доменной проверки поставщиком.
	value CapabilitiesDTO
}

// Snapshot возвращает подготовленный снимок p без побочных действий; контекст не используется.
func (p untrustedCapabilities) Snapshot(context.Context) (CapabilitiesDTO, error) {
	return p.value, nil
}

// TestCapabilitiesRejectsUnknownStringsAtHTTPBoundary проверяет safe500 до публикации произвольных
// значений поставщика, а корректные finite-значения проходят настоящий HTTP-маршрут.
func TestCapabilitiesRejectsUnknownStringsAtHTTPBoundary(t *testing.T) {
	// service использует закрытое хранилище, не нужное системному маршруту; err сообщает отказ создания.
	service, err := core.NewService(closedUnitOfWork{}, "local-owner")
	if err != nil {
		t.Fatal(err)
	}
	// invalid задаёт неизвестный статус, причину, неверный регистр и неверный UTF-8.
	for _, invalid := range []string{"available", "future-capability", "AVAILABLE", string([]byte{0xff})} {
		// capability задаёт корректную основу снимка с допустимой причиной.
		capability := CapabilityDTO{Status: "available", Reason: "executable_resolved"}
		// value содержит четыре независимых компонента поставщика.
		value := CapabilitiesDTO{capability, capability, capability, capability}
		value.Helper.Status = invalid
		if invalid == "future-capability" {
			value.Helper.Status = "available"
			value.Helper.Reason = invalid
		}
		// handler представляет настоящий API с внешней реализацией поставщика;
		// err сообщает об ошибке регистрации его компонентов.
		handler, err := New(Config{Core: service, Capabilities: untrustedCapabilities{value}, Logger: slog.New(slog.NewJSONHandler(io.Discard, nil)), Limits: Limits{JSONBytes: 4096, UploadBytes: 4096, UploadReadTimeout: time.Second, OperationTimeout: time.Second, ShutdownTimeout: time.Second, SSEClients: 1, SSEPageSize: 1, SSEFrameBytes: 1024, SSEWriteTimeout: time.Second, SSEHeartbeat: time.Second}})
		if err != nil {
			t.Fatal(err)
		}
		// response сохраняет реальный ответ системного маршрута для проверки безопасности.
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest("GET", Prefix+"/system/capabilities", nil))
		// want соответствует корректному снимку либо безопасному отказу неизвестного текста.
		want := 500
		if invalid == "available" {
			want = 200
		}
		if response.Code != want || want == 500 && (strings.Contains(response.Body.String(), invalid) || !strings.HasPrefix(response.Header().Get("Content-Type"), "application/problem+json")) {
			t.Fatal(response.Code, response.Body.String())
		}
		if err = handler.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}
