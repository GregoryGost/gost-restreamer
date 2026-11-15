package api_test

import (
	"context"
	"math"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// numericAwaitEvent разбирает настоящие SSE-кадры и возвращает data события kind
// с заданным owner. Ожидание ограничено; числа остаются json.Number.
func numericAwaitEvent(t *testing.T, stream *apiAcceptanceStreamWriter, kind, owner string) map[string]any {
	t.Helper()
	// timer ограничивает ожидание кадра тремя секундами.
	timer := time.NewTimer(3 * time.Second)
	defer timer.Stop()
	for {
		// body содержит согласованные байты транспорта; status и unsafe проверяют HTTP-границу.
		body, status, unsafe := stream.snapshot()
		if unsafe || status != 0 && status != 200 {
			t.Fatalf("ошибка SSE-транспорта: status=%d unsafe=%v body=%s", status, unsafe, body)
		}
		// frames содержит также незавершённый хвост, который не участвует в разборе.
		frames := strings.Split(body, "\n\n")
		// frame содержит один завершённый кадр SSE.
		for _, frame := range frames[:len(frames)-1] {
			// eventName и id сохраняют текстовые служебные поля; data хранит исходный JSON.
			var eventName, id, data string
			// line задаёт одну строку кадра для строгого выделения event/id/data.
			for line := range strings.SplitSeq(frame, "\n") {
				switch {
				case strings.HasPrefix(line, "event: "):
					eventName = strings.TrimPrefix(line, "event: ")
				case strings.HasPrefix(line, "id: "):
					id = strings.TrimPrefix(line, "id: ")
				case strings.HasPrefix(line, "data: "):
					data = strings.TrimPrefix(line, "data: ")
				}
			}
			if eventName != kind {
				continue
			}
			// event содержит JSON-данные реального кадра с сохранёнными числовыми токенами.
			event := numericJSON(t, []byte(data))
			if owner != "" && event["owner"] != owner {
				continue
			}
			if eventName != "resync_required" && (id == "" || event["cursor"] != id || event["kind"] != kind) {
				t.Fatal("служебные строки SSE не согласованы с payload", frame, event)
			}
			return event
		}
		select {
		case <-stream.changed:
		case <-timer.C:
			t.Fatalf("не получено событие kind=%s owner=%s: %s", kind, owner, body)
		}
	}
}

// TestNumericSSEFFmpegSignedBounds сравнивает REST и реальные JSON-данные SSE
// для полного int64, отсутствия и нуля; uint64-счётчики не проходят через float64.
func TestNumericSSEFFmpegSignedBounds(t *testing.T) {
	// tc задаёт независимое состояние знакового микросекундного поля FFmpeg.
	for _, tc := range []struct {
		// name задаёт сценарий; value содержит исходное знаковое значение; present отличает null от нуля.
		name    string
		value   int64
		present bool
		// want задаёт точный числовой токен либо пустоту для null.
		want string
	}{
		{"absent", 91, false, ""}, {"zero", 0, true, "0"}, {"negative", -9007199254740993, true, "-9007199254740993"},
		{"minimum", math.MinInt64, true, "-9223372036854775808"}, {"maximum", math.MaxInt64, true, "9223372036854775807"},
	} {
		// Функция t.Run проверяет одну знаковую границу на собственной временной SQLite.
		t.Run(tc.name, func(t *testing.T) {
			// f содержит API; sessionID адресует метрики запуска; attempt публикует синтетические наблюдения.
			f, sessionID, attempt := apiAcceptanceFFmpegAttempt(t)
			// anchor фиксирует cursor до публикации нового отчёта FFmpeg.
			anchor := numericJSON(t, apiAcceptanceRequest(t, f.api, "GET", "/events/snapshot", "", 200).Body.Bytes())
			// sampled сохраняет свежесть отчёта без реального FFmpeg-процесса.
			sampled := time.Now().UTC()
			// measurement задаёт точные uint64, дробные метрики и необязательное знаковое время.
			measurement := core.FFmpegMetrics{Status: core.MetricAvailable, HasProgress: true, SampledAt: sampled, LastProgressAt: sampled, ProgressLosses: math.MaxUint64, DiagnosticLosses: 9007199254740993, Progress: core.FFmpegProgress{
				Frame: core.OptionalUint64{Present: true, Value: math.MaxUint64}, TotalSizeBytes: core.OptionalUint64{Present: true, Value: 9007199254740993},
				DupFrames: core.OptionalUint64{Present: true}, DropFrames: core.OptionalUint64{Value: 55},
				FPS: core.OptionalFloat64{Present: true, Value: 1.25}, BitrateBitsPerSecond: core.OptionalFloat64{Present: true},
				Speed: core.OptionalFloat64{Present: true, Value: math.Copysign(0, -1)}, OutTimeUS: core.OptionalInt64{Present: tc.present, Value: tc.value},
			}}
			// err сообщает отказ публикации отчёта в настоящую RuntimeAuthority.
			if err := attempt.ObserveMetrics(context.Background(), measurement); err != nil {
				t.Fatal(err)
			}
			// rest содержит реальный GET метрик Session с текущим снимком.
			rest := numericJSON(t, apiAcceptanceRequest(t, f.api, "GET", "/sessions/"+string(sessionID)+"/metrics", "", 200).Body.Bytes())
			// metrics извлекает единственного работника FFmpeg из согласованного REST-снимка.
			metrics := apiAcceptanceFFmpegWireMetrics(t, rest["snapshot"].(map[string]any))
			numericWant(t, metrics, "progress_losses", "18446744073709551615")
			numericWant(t, metrics, "diagnostic_losses", "9007199254740993")
			numericWant(t, metrics, "progress.frame", "18446744073709551615")
			numericWant(t, metrics, "progress.total_size_bytes", "9007199254740993")
			numericWant(t, metrics, "progress.dup_frames", "0")
			numericWant(t, metrics, "progress.fps", "1.25")
			numericWant(t, metrics, "progress.bitrate_bps", "0")
			numericWant(t, metrics, "progress.speed", "-0")
			if tc.present {
				numericWant(t, metrics, "progress.out_time_us", tc.want)
			} else if numericAt(t, metrics, "progress.out_time_us") != nil {
				t.Fatal("отсутствующее время стало числом", metrics)
			}
			if numericAt(t, metrics, "progress.drop_frames") != nil {
				t.Fatal("отсутствующий счётчик стал нулём", metrics)
			}
			// stream получает событие после сохранённой позиции; cancel/done закрывают подписку.
			stream := newAPIAcceptanceStreamWriter()
			cancel, done := apiAcceptanceStartStream(t, f, "/events?cursor="+url.QueryEscape(anchor["cursor"].(string)), stream)
			// event содержит фактические data кадра metrics нужного владельца.
			event := numericAwaitEvent(t, stream, "metrics", "ffmpeg")
			cancel()
			apiAcceptanceAwait(t, done)
			if !reflect.DeepEqual(event["ffmpeg"], metrics) {
				t.Fatal("REST и SSE расходятся по числам/null", metrics, event["ffmpeg"])
			}
		})
	}
}

// TestNumericSSEGPUFullUint64 отделяет проценты от байтов памяти, проверяет
// точность периода выборки и согласованность REST/SSE с null неподдержанных метрик.
func TestNumericSSEGPUFullUint64(t *testing.T) {
	// f содержит API и настоящую RuntimeAuthority временной SQLite.
	f := newAPIAcceptanceFixture(t, nil)
	// observer получает право публикации GPU без NVML и аппаратуры.
	observer := apiAcceptanceSystem(t, f)
	// anchor фиксирует границу подписки перед большим измерением.
	anchor := numericJSON(t, apiAcceptanceRequest(t, f.api, "GET", "/events/snapshot", "", 200).Body.Bytes())
	// snapshot задаёт физически допустимое число байтов uint64 и недоступные независимые показатели.
	snapshot := apiAcceptanceGPU(math.MaxUint64, time.Now().UTC())
	snapshot.StaleAfter = time.Duration(math.MaxInt64)
	snapshot.Devices[0].Utilization.Value = 100
	snapshot.Devices[0].Utilization.SamplingPeriodUS = math.MaxUint64
	// err сообщает отказ допустимой полной границы измерения GPU.
	if err := observer.ObserveGPU(context.Background(), snapshot); err != nil {
		t.Fatal(err)
	}
	// rest содержит атомарный публичный снимок с числовыми значениями.
	rest := numericJSON(t, apiAcceptanceRequest(t, f.api, "GET", "/events/snapshot", "", 200).Body.Bytes())
	// gpu содержит системную часть REST; device содержит единственное устройство.
	gpu := rest["gpu"].(map[string]any)
	device := gpu["devices"].([]any)[0].(map[string]any)
	numericWant(t, gpu, "stale_after_ns", "9223372036854775807")
	numericWant(t, device, "utilization.value", "100")
	numericWant(t, device, "utilization.sampling_period_us", "18446744073709551615")
	numericWant(t, device, "memory_utilization.value", "0")
	// field задаёт каждую байтовую метрику, которая допускает полный uint64.
	for _, field := range []string{"memory_used_bytes", "memory_total_bytes", "memory_free_bytes"} {
		numericWant(t, device, field+".value", "18446744073709551615")
	}
	if numericAt(t, device, "encoder_utilization.value") != nil {
		t.Fatal("неподдержанная метрика стала нулём", device)
	}
	// stream принимает реальный кадр GPU, cancel/done завершают его обработчик.
	stream := newAPIAcceptanceStreamWriter()
	cancel, done := apiAcceptanceStartStream(t, f, "/events?cursor="+url.QueryEscape(anchor["cursor"].(string)), stream)
	// event содержит данные кадра, полностью сопоставимые с REST-проекцией GPU.
	event := numericAwaitEvent(t, stream, "gpu", "")
	cancel()
	apiAcceptanceAwait(t, done)
	if !reflect.DeepEqual(event["gpu"], gpu) {
		t.Fatal("GPU REST/SSE расходятся", gpu, event["gpu"])
	}
	snapshot.Devices[0].Utilization.Value = 101
	if err := observer.ObserveGPU(context.Background(), snapshot); err == nil {
		// err должен подтвердить прежнее доменное ограничение процентов.
		t.Fatal("приняты 101 процент GPU")
	}
}
