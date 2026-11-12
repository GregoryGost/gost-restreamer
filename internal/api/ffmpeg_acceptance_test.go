package api_test

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// apiAcceptanceFFmpegAttempt создаёт настоящий доменный запуск и точного наблюдателя попытки FFmpeg
// без необходимости генерировать медиафайл профиля.
func apiAcceptanceFFmpegAttempt(t *testing.T) (*apiAcceptanceFixture, core.ID, *core.FFmpegAttemptObserver) {
	t.Helper()
	// f содержит API и SQLite; preparation передаёт принятые задания; release освобождает
	// управляемые ресурсы подготовки после сценария.
	f, preparation, _, release := newAPIAcceptanceUpload(t, time.Second)
	t.Cleanup(release)
	// ctx задаёт общий контекст создания доменных объектов и точного наблюдателя FFmpeg.
	ctx := context.Background()
	// asset содержит заглушку наблюдаемого запуска; err содержит отказ её создания.
	asset, err := f.service.CreateAsset(ctx, "Заглушка наблюдаемого запуска")
	if err != nil {
		t.Fatal(err)
	}
	// stream содержит поток с политикой заглушки для настоящего доменного запуска.
	stream, err := f.service.CreateStream(ctx, core.Stream{Name: "Наблюдаемый запуск", PublicationKey: "synthetic-publication-secret", Fallback: core.FallbackPolicy{AssetID: asset.ID, Watchdog: core.WatchdogPolicy{VideoTimeout: time.Second, AudioTimeout: time.Second, RecoveryWindow: time.Second, KeyframeTimeout: time.Second}}})
	if err != nil {
		t.Fatal(err)
	}
	// body передаёт PNG-оригинал для готового поколения без генерации медиафайла профиля.
	body := &apiAcceptanceUploadBody{reader: bytes.NewReader(apiAcceptancePNG(t)), closed: make(chan struct{})}
	apiAcceptanceUploadRequest(t, f, ctx, string(asset.ID), asset.Revision, body, 202)
	// wait ограничивает ожидание подготовки тремя секундами; cancel освобождает таймер.
	wait, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if _, err = (<-preparation.jobs).Wait(wait); err != nil {
		t.Fatal(err)
	}
	// Для RAM-наблюдений не требуется генерировать медиафайл профиля.
	// profile содержит профиль для конфигурационной связи наблюдаемой группы.
	profile, err := f.service.CreateProfile(ctx, core.TranscodeProfile{Name: "Профиль наблюдений", Profile: apiAcceptanceProfile()})
	if err != nil {
		t.Fatal(err)
	}
	// group содержит группу выбранного потока и профиля для точного наблюдателя.
	group, err := f.service.CreateGroup(ctx, core.TranscodeGroup{Name: "Наблюдаемая группа", StreamID: stream.ID, ProfileID: profile.ID})
	if err != nil {
		t.Fatal(err)
	}
	// requestID задаёт постоянный смысловой идентификатор запуска, используемый также при Claim.
	const requestID = "observed-session-start"
	// receipt содержит принятую команду запуска и идентификатор наблюдаемой Session.
	receipt, err := f.service.StartSession(ctx, core.StartSessionRequest{RequestID: requestID, StreamID: stream.ID})
	if err != nil {
		t.Fatal(err)
	}
	// fence содержит точную захваченную команду запуска для MarkIssued, Complete и наблюдателя
	// Session.
	fence, err := f.service.Claim(ctx, requestID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.service.MarkIssued(ctx, fence); err != nil {
		t.Fatal(err)
	}
	if err = f.service.Complete(ctx, fence, core.Completion{Success: true}); err != nil {
		t.Fatal(err)
	}
	// observer содержит точного владельца наблюдений Session текущего запуска.
	observer, err := f.service.OpenSessionObserver(ctx, fence)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает наблюдателя тестовой Session после проверки метрик FFmpeg.

		// err содержит результат закрытия тестового наблюдателя Session при очистке.
		if err := observer.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	// groupObserver содержит точного наблюдателя созданной группы.
	groupObserver, err := observer.OpenGroup(ctx, group.ID, profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	// worker содержит наблюдателя роли FFmpegLiveTranscode в этой группе.
	worker, err := groupObserver.OpenFFmpegRole(ctx, core.FFmpegLiveTranscode)
	if err != nil {
		t.Fatal(err)
	}
	// attempt содержит наблюдателя первой точной попытки работника FFmpeg.
	attempt, err := worker.BeginAttempt(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	return f, receipt.SessionID, attempt
}

// apiAcceptanceFFmpegWireMetrics извлекает единственного работника live_transcode и его метрики из
// публичного снимка.
func apiAcceptanceFFmpegWireMetrics(t *testing.T, snapshot map[string]any) map[string]any {
	t.Helper()
	// workers содержит публичный массив FFmpeg; ok подтверждает его тип перед требованием
	// единственного работника.
	workers, ok := snapshot["ffmpeg"].([]any)
	if !ok || len(workers) != 1 {
		t.Fatalf("снимок не содержит точного FFmpeg worker: %#v", snapshot)
	}
	// worker содержит единственного публичного работника для проверки роли live_transcode.
	worker := workers[0].(map[string]any)
	if worker["role"] != "live_transcode" {
		t.Fatalf("утрачена роль FFmpeg: %#v", worker)
	}
	// metrics содержит публичное измерение FFmpeg текущего работника.
	metrics, ok := worker["metrics"].(map[string]any)
	if !ok {
		t.Fatalf("утрачены метаданные измерения FFmpeg: %#v", worker)
	}
	return metrics
}

// apiAcceptanceCheckFFmpegWire сверяет доступность, точные счётчики потерь, UTC-времена и полное либо
// отсутствующее представление progress.
func apiAcceptanceCheckFFmpegWire(t *testing.T, metrics map[string]any, status, reason string, sampled, progressed time.Time, wantProgress map[string]any) {
	t.Helper()
	if metrics["status"] != status || metrics["reason"] != reason || metrics["progress_losses"] != "9007199254740993" || metrics["diagnostic_losses"] != "18446744073709551615" {
		t.Errorf("утрачены доступность или точные счётчики потерь: %#v", metrics)
	}
	// key задаёт проверяемое временное поле; want содержит точное ожидаемое время измерения или
	// progress.
	for key, want := range map[string]time.Time{"sampled_at": sampled, "last_progress_at": progressed} {
		// got содержит строку публичного времени; ok подтверждает строковый тип перед разбором.
		got, ok := metrics[key].(string)
		// parsed содержит разобранное RFC3339Nano-время; err содержит отказ его чтения.
		parsed, err := time.Parse(time.RFC3339Nano, got)
		if !ok || err != nil || !parsed.Equal(want) || !strings.HasSuffix(got, "Z") {
			t.Errorf("время %s потеряло точность или UTC: got=%#v want=%s", key, metrics[key], want.Format(time.RFC3339Nano))
		}
	}
	// progress содержит публичное значение отчёта либо null; present подтверждает обязательное
	// присутствие поля.
	progress, present := metrics["progress"]
	if !present || (wantProgress == nil && progress != nil) || (wantProgress != nil && !reflect.DeepEqual(progress, wantProgress)) {
		t.Errorf("неверный progress при status=%s: got=%#v want=%#v", status, progress, wantProgress)
	}
}

// Проверяет REST и SSE для устаревшего, доступного нулевого и частично доступного FFmpeg progress;
// самостоятельная проверка свежести снимка не переписывает прежнее событие.
func TestAcceptanceFFmpegProgressAvailabilityAcrossRuntimeHTTP(t *testing.T) {
	// zero задаёт присутствующие нулевые поля progress, отличные от отсутствующих измерений.
	zero := core.FFmpegProgress{Frame: core.OptionalUint64{Present: true}, TotalSizeBytes: core.OptionalUint64{Present: true}, DupFrames: core.OptionalUint64{Present: true}, DropFrames: core.OptionalUint64{Present: true}, FPS: core.OptionalFloat64{Present: true}, BitrateBitsPerSecond: core.OptionalFloat64{Present: true}, Speed: core.OptionalFloat64{Present: true}, OutTimeUS: core.OptionalInt64{Present: true}}
	// zeroWire задаёт точный публичный отчёт со всеми измеренными нулями.
	zeroWire := map[string]any{"frame": "0", "total_size_bytes": "0", "dup_frames": "0", "drop_frames": "0", "fps": float64(0), "bitrate_bps": float64(0), "speed": float64(0), "out_time_us": "0", "end": false}
	// partial сохраняет часть присутствующих нулей и задаёт значения неприсутствующих полей для
	// проверки их null.
	partial := zero
	partial.TotalSizeBytes = core.OptionalUint64{Value: 91}
	partial.DupFrames = core.OptionalUint64{Value: 92}
	partial.DropFrames = core.OptionalUint64{Value: 93}
	partial.BitrateBitsPerSecond = core.OptionalFloat64{Value: 94}
	partial.Speed = core.OptionalFloat64{Value: 95}
	// partialWire задаёт ожидаемый публичный progress с нулями и отсутствующими полями.
	partialWire := map[string]any{"frame": "0", "total_size_bytes": nil, "dup_frames": nil, "drop_frames": nil, "fps": float64(0), "bitrate_bps": nil, "speed": nil, "out_time_us": "0", "end": false}
	// old задаёт ненулевой прежний отчёт для сценариев устаревания.
	old := zero
	old.Frame.Value, old.TotalSizeBytes.Value, old.FPS.Value, old.End = 123, 456, 12.5, true
	// tc задаёт исходную доступность, возраст progress и ожидаемое публичное представление.
	for _, tc := range []struct {
		// name задаёт название сценария; status задаёт исходную доступность; reason задаёт
		// исходную причину FFmpeg-измерения.
		name, status, reason string
		// progress содержит исходный нулевой, частичный или прежний отчёт FFmpeg.
		progress core.FFmpegProgress
		// age задаёт возраст последнего progress относительно времени снимка.
		age time.Duration
		// freshnessOnly выбирает устаревание только при чтении снимка без переписывания прежнего
		// события.
		freshnessOnly bool
		// want задаёт ожидаемый объект progress либо nil для недоступного отчёта.
		want map[string]any
	}{
		{name: "sampler_stale", status: "stale", reason: "stale", progress: old, age: 2 * time.Minute},
		{name: "snapshot_freshness_stale", status: "available", progress: old, age: 2 * time.Minute, freshnessOnly: true},
		{name: "available_measured_zero", status: "available", progress: zero, want: zeroWire},
		{name: "available_with_NA", status: "available", progress: partial, want: partialWire},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Публикует выбранное измерение FFmpeg и проверяет его доступность через HTTP и SSE.

			// f содержит API; sessionID идентифицирует наблюдаемый запуск; attempt публикует
			// измерение точной попытки FFmpeg.
			f, sessionID, attempt := apiAcceptanceFFmpegAttempt(t)
			// anchor сохраняет курсор до публикации измерения для последующей подписки SSE.
			anchor := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", "/events/snapshot", "", 200))
			// sampled задаёт текущее UTC-время снимка FFmpeg.
			sampled := time.Now().UTC()
			// progressed задаёт время последнего progress с выбранным возрастом.
			progressed := sampled.Add(-tc.age)
			// measurement содержит исходные доступность, отчёт, времена и точные большие
			// счётчики потерь.
			measurement := core.FFmpegMetrics{Status: core.MetricStatus(tc.status), Reason: core.MetricReason(tc.reason), HasProgress: true, SampledAt: sampled, LastProgressAt: progressed, Progress: tc.progress, ProgressLosses: 9007199254740993, DiagnosticLosses: 18446744073709551615}
			// err содержит отказ публикации измерения точной попыткой FFmpeg.
			if err := attempt.ObserveMetrics(context.Background(), measurement); err != nil {
				t.Fatal(err)
			}
			// status задаёт ожидаемую доступность HTTP-снимка; reason задаёт её причину с
			// учётом самостоятельной проверки свежести.
			status, reason := tc.status, tc.reason
			if tc.freshnessOnly {
				status, reason = "stale", "stale"
			}
			t.Run("session_metrics", func(t *testing.T) {
				// Проверяет свежесть, точность счётчиков и отсутствие секретов в метриках отдельной Session.

				// response сохраняет реальный HTTP-ответ метрик Session для проверки
				// полей и отсутствия внутренних данных.
				response := apiAcceptanceRequest(t, f.api, "GET", "/sessions/"+string(sessionID)+"/metrics", "", 200)
				// v содержит разобранный ответ метрик Session с текущим снимком.
				v := apiAcceptanceJSON(t, response)
				if v["availability"] != "available" {
					t.Fatal("недоступность FFmpeg скрыла текущую Session", v)
				}
				apiAcceptanceCheckFFmpegWire(t, apiAcceptanceFFmpegWireMetrics(t, v["snapshot"].(map[string]any)), status, reason, sampled, progressed, tc.want)
				apiAcceptanceNoSecrets(t, response.Body.String(), f.store.Incarnation())
			})
			t.Run("atomic_anchor", func(t *testing.T) {
				// Проверяет сохранение измерения FFmpeg в атомарном снимке всех наблюдений.

				// response сохраняет атомарный HTTP-снимок всех наблюдений.
				response := apiAcceptanceRequest(t, f.api, "GET", "/events/snapshot", "", 200)
				// v содержит разобранный общий снимок для поиска проверяемой Session.
				v := apiAcceptanceJSON(t, response)
				// sessions содержит массив текущих Session; ok подтверждает тип перед
				// проверкой единственного запуска.
				sessions, ok := v["sessions"].([]any)
				if !ok || len(sessions) != 1 || sessions[0].(map[string]any)["session_id"] != string(sessionID) {
					t.Fatal("anchor потерял текущую Session", v)
				}
				apiAcceptanceCheckFFmpegWire(t, apiAcceptanceFFmpegWireMetrics(t, sessions[0].(map[string]any)), status, reason, sampled, progressed, tc.want)
				apiAcceptanceNoSecrets(t, response.Body.String(), f.store.Incarnation())
			})
			// Проверка свежести снимка не переписывает ранее записанное событие.
			if tc.freshnessOnly {
				return
			}
			t.Run("SSE", func(t *testing.T) {
				// Проверяет одно событие FFmpeg после курсора снимка, включая отсутствие секретов и точность
				// измерения.

				// stream содержит управляемый HTTP-получатель SSE для чтения события
				// FFmpeg.
				stream := newAPIAcceptanceStreamWriter()
				// cancel прекращает подписку после нужного кадра; done подтверждает
				// возврат её обработчика.
				cancel, done := apiAcceptanceStartStream(t, f, "/events?cursor="+url.QueryEscape(anchor["cursor"].(string)), stream)
				apiAcceptanceAwaitFrame(t, stream, "event: metrics\n")
				cancel()
				apiAcceptanceAwait(t, done)
				// body содержит переданные SSE-кадры; statusCode содержит HTTP-статус;
				// unsafe отмечает небезопасное использование тестового транспорта.
				body, statusCode, unsafe := stream.snapshot()
				if statusCode != 200 || unsafe {
					t.Fatalf("некорректная SSE-граница: status=%d unsafe=%v", statusCode, unsafe)
				}
				// count считает события FFmpeg metrics после сохранённого anchor.
				count := 0
				// frame содержит очередной целый SSE-кадр при поиске измерения.
				for frame := range strings.SplitSeq(body, "\n\n") {
					if !strings.Contains(frame, "event: metrics\n") {
						continue
					}
					// line содержит строку выбранного кадра для извлечения JSON
					// data.
					for line := range strings.SplitSeq(frame, "\n") {
						if !strings.HasPrefix(line, "data: ") {
							continue
						}
						// event принимает JSON события для проверки Session,
						// владельца и точного измерения FFmpeg.
						var event map[string]any
						// err содержит отказ разбора JSON-данных SSE-события
						// FFmpeg.
						if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
							t.Fatal(err)
						}
						if event["session_id"] != string(sessionID) || event["owner"] != "ffmpeg" {
							t.Fatal("событие потеряло владельца", event)
						}
						apiAcceptanceCheckFFmpegWire(t, event["ffmpeg"].(map[string]any), tc.status, tc.reason, sampled, progressed, tc.want)
						count++
					}
				}
				if count != 1 {
					t.Fatalf("после anchor ожидалось одно FFmpeg-событие, получено %d: %s", count, body)
				}
				apiAcceptanceNoSecrets(t, body, f.store.Incarnation())
			})
		})
	}
}
