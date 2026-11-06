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

func apiAcceptanceFFmpegAttempt(t *testing.T) (*apiAcceptanceFixture, core.ID, *core.FFmpegAttemptObserver) {
	t.Helper()
	f, preparation, _, release := newAPIAcceptanceUpload(t, time.Second)
	t.Cleanup(release)
	ctx := context.Background()
	asset, err := f.service.CreateAsset(ctx, "Заглушка наблюдаемого запуска")
	if err != nil {
		t.Fatal(err)
	}
	stream, err := f.service.CreateStream(ctx, core.Stream{Name: "Наблюдаемый запуск", PublicationKey: "synthetic-publication-secret", Fallback: core.FallbackPolicy{AssetID: asset.ID, Watchdog: core.WatchdogPolicy{VideoTimeout: time.Second, AudioTimeout: time.Second, RecoveryWindow: time.Second, KeyframeTimeout: time.Second}}})
	if err != nil {
		t.Fatal(err)
	}
	body := &apiAcceptanceUploadBody{reader: bytes.NewReader(apiAcceptancePNG(t)), closed: make(chan struct{})}
	apiAcceptanceUploadRequest(t, f, ctx, string(asset.ID), asset.Revision, body, 202)
	wait, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if _, err = (<-preparation.jobs).Wait(wait); err != nil {
		t.Fatal(err)
	}
	// Для RAM-наблюдений не требуется генерировать медиафайл профиля.
	profile, err := f.service.CreateProfile(ctx, core.TranscodeProfile{Name: "Профиль наблюдений", Profile: apiAcceptanceProfile()})
	if err != nil {
		t.Fatal(err)
	}
	group, err := f.service.CreateGroup(ctx, core.TranscodeGroup{Name: "Наблюдаемая группа", StreamID: stream.ID, ProfileID: profile.ID})
	if err != nil {
		t.Fatal(err)
	}
	const requestID = "observed-session-start"
	receipt, err := f.service.StartSession(ctx, core.StartSessionRequest{RequestID: requestID, StreamID: stream.ID})
	if err != nil {
		t.Fatal(err)
	}
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
	observer, err := f.service.OpenSessionObserver(ctx, fence)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := observer.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	groupObserver, err := observer.OpenGroup(ctx, group.ID, profile.ID)
	if err != nil {
		t.Fatal(err)
	}
	worker, err := groupObserver.OpenFFmpegRole(ctx, core.FFmpegLiveTranscode)
	if err != nil {
		t.Fatal(err)
	}
	attempt, err := worker.BeginAttempt(ctx, 1)
	if err != nil {
		t.Fatal(err)
	}
	return f, receipt.SessionID, attempt
}

func apiAcceptanceFFmpegWireMetrics(t *testing.T, snapshot map[string]any) map[string]any {
	t.Helper()
	workers, ok := snapshot["ffmpeg"].([]any)
	if !ok || len(workers) != 1 {
		t.Fatalf("снимок не содержит точного FFmpeg worker: %#v", snapshot)
	}
	worker := workers[0].(map[string]any)
	if worker["role"] != "live_transcode" {
		t.Fatalf("утрачена роль FFmpeg: %#v", worker)
	}
	metrics, ok := worker["metrics"].(map[string]any)
	if !ok {
		t.Fatalf("утрачены метаданные измерения FFmpeg: %#v", worker)
	}
	return metrics
}

func apiAcceptanceCheckFFmpegWire(t *testing.T, metrics map[string]any, status, reason string, sampled, progressed time.Time, wantProgress map[string]any) {
	t.Helper()
	if metrics["status"] != status || metrics["reason"] != reason || metrics["progress_losses"] != "9007199254740993" || metrics["diagnostic_losses"] != "18446744073709551615" {
		t.Errorf("утрачены доступность или точные счётчики потерь: %#v", metrics)
	}
	for key, want := range map[string]time.Time{"sampled_at": sampled, "last_progress_at": progressed} {
		got, ok := metrics[key].(string)
		parsed, err := time.Parse(time.RFC3339Nano, got)
		if !ok || err != nil || !parsed.Equal(want) || !strings.HasSuffix(got, "Z") {
			t.Errorf("время %s потеряло точность или UTC: got=%#v want=%s", key, metrics[key], want.Format(time.RFC3339Nano))
		}
	}
	progress, present := metrics["progress"]
	if !present || (wantProgress == nil && progress != nil) || (wantProgress != nil && !reflect.DeepEqual(progress, wantProgress)) {
		t.Errorf("неверный progress при status=%s: got=%#v want=%#v", status, progress, wantProgress)
	}
}

func TestAcceptanceFFmpegProgressAvailabilityAcrossRuntimeHTTP(t *testing.T) {
	zero := core.FFmpegProgress{Frame: core.OptionalUint64{Present: true}, TotalSizeBytes: core.OptionalUint64{Present: true}, DupFrames: core.OptionalUint64{Present: true}, DropFrames: core.OptionalUint64{Present: true}, FPS: core.OptionalFloat64{Present: true}, BitrateBitsPerSecond: core.OptionalFloat64{Present: true}, Speed: core.OptionalFloat64{Present: true}, OutTimeUS: core.OptionalInt64{Present: true}}
	zeroWire := map[string]any{"frame": "0", "total_size_bytes": "0", "dup_frames": "0", "drop_frames": "0", "fps": float64(0), "bitrate_bps": float64(0), "speed": float64(0), "out_time_us": "0", "end": false}
	partial := zero
	partial.TotalSizeBytes = core.OptionalUint64{Value: 91}
	partial.DupFrames = core.OptionalUint64{Value: 92}
	partial.DropFrames = core.OptionalUint64{Value: 93}
	partial.BitrateBitsPerSecond = core.OptionalFloat64{Value: 94}
	partial.Speed = core.OptionalFloat64{Value: 95}
	partialWire := map[string]any{"frame": "0", "total_size_bytes": nil, "dup_frames": nil, "drop_frames": nil, "fps": float64(0), "bitrate_bps": nil, "speed": nil, "out_time_us": "0", "end": false}
	old := zero
	old.Frame.Value, old.TotalSizeBytes.Value, old.FPS.Value, old.End = 123, 456, 12.5, true
	for _, tc := range []struct {
		name, status, reason string
		progress             core.FFmpegProgress
		age                  time.Duration
		freshnessOnly        bool
		want                 map[string]any
	}{
		{name: "sampler_stale", status: "stale", reason: "stale", progress: old, age: 2 * time.Minute},
		{name: "snapshot_freshness_stale", status: "available", progress: old, age: 2 * time.Minute, freshnessOnly: true},
		{name: "available_measured_zero", status: "available", progress: zero, want: zeroWire},
		{name: "available_with_NA", status: "available", progress: partial, want: partialWire},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f, sessionID, attempt := apiAcceptanceFFmpegAttempt(t)
			anchor := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "GET", "/events/snapshot", "", 200))
			sampled := time.Now().UTC()
			progressed := sampled.Add(-tc.age)
			measurement := core.FFmpegMetrics{Status: core.MetricStatus(tc.status), Reason: core.MetricReason(tc.reason), HasProgress: true, SampledAt: sampled, LastProgressAt: progressed, Progress: tc.progress, ProgressLosses: 9007199254740993, DiagnosticLosses: 18446744073709551615}
			if err := attempt.ObserveMetrics(context.Background(), measurement); err != nil {
				t.Fatal(err)
			}
			status, reason := tc.status, tc.reason
			if tc.freshnessOnly {
				status, reason = "stale", "stale"
			}
			t.Run("session_metrics", func(t *testing.T) {
				response := apiAcceptanceRequest(t, f.api, "GET", "/sessions/"+string(sessionID)+"/metrics", "", 200)
				v := apiAcceptanceJSON(t, response)
				if v["availability"] != "available" {
					t.Fatal("недоступность FFmpeg скрыла текущую Session", v)
				}
				apiAcceptanceCheckFFmpegWire(t, apiAcceptanceFFmpegWireMetrics(t, v["snapshot"].(map[string]any)), status, reason, sampled, progressed, tc.want)
				apiAcceptanceNoSecrets(t, response.Body.String(), f.store.Incarnation())
			})
			t.Run("atomic_anchor", func(t *testing.T) {
				response := apiAcceptanceRequest(t, f.api, "GET", "/events/snapshot", "", 200)
				v := apiAcceptanceJSON(t, response)
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
				stream := newAPIAcceptanceStreamWriter()
				cancel, done := apiAcceptanceStartStream(t, f, "/events?cursor="+url.QueryEscape(anchor["cursor"].(string)), stream)
				apiAcceptanceAwaitFrame(t, stream, "event: metrics\n")
				cancel()
				apiAcceptanceAwait(t, done)
				body, statusCode, unsafe := stream.snapshot()
				if statusCode != 200 || unsafe {
					t.Fatalf("некорректная SSE-граница: status=%d unsafe=%v", statusCode, unsafe)
				}
				count := 0
				for _, frame := range strings.Split(body, "\n\n") {
					if !strings.Contains(frame, "event: metrics\n") {
						continue
					}
					for _, line := range strings.Split(frame, "\n") {
						if !strings.HasPrefix(line, "data: ") {
							continue
						}
						var event map[string]any
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
