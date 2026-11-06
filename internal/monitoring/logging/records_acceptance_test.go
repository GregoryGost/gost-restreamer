package logging

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"log/slog"
	"reflect"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

type acceptanceGatedWriter struct {
	entered chan struct{}
	release chan struct{}
	output  bytes.Buffer
}

func (w *acceptanceGatedWriter) Write(data []byte) (int, error) {
	select {
	case w.entered <- struct{}{}:
	default:
	}
	<-w.release
	return w.output.Write(data)
}

func TestAcceptanceJSONFullValuesSurvivePressureAndCallerMutation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		w := &acceptanceGatedWriter{entered: make(chan struct{}, 1), release: make(chan struct{})}
		s, err := New(Config{MaxRecords: 2, MaxBytes: 65536, MaxRecordBytes: 4096, ShutdownTimeout: time.Second}, w)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			select {
			case <-w.release:
			default:
				close(w.release)
			}
			_ = s.Close(context.Background())
		}()
		logger := s.Logger().With("component", "ffmpeg", "stream_id", "stream", "session_id", "session", "group_id", "group", "destination_id", "destination")
		logger.Info("writer_started")
		<-w.entered
		payload := make([]byte, 256)
		for i := range payload {
			payload[i] = byte(i)
		}
		original := bytes.Clone(payload)
		argv := []string{"ffmpeg", "rtmps://synthetic.invalid/live?key=synthetic%2Bfull", "строка\n\"кавычки\""}
		originalArgs := append([]string(nil), argv...)
		token := "synthetic-token:/+\"\\\nполное"
		header := "Bearer synthetic-header+/= with tail"
		logger.WithGroup("diagnostic").LogAttrs(context.Background(), slog.LevelError, "worker_diagnostic",
			slog.String("operation", "stderr"), slog.String("token", token), slog.String("header", header),
			slog.Any("argv", argv), Binary("payload", payload))
		clear(payload)
		argv[1] = "caller-changed"
		logger.Info("overflow_must_disappear")
		logger.Info("oversized_must_disappear", "payload", strings.Repeat("x", 4096))
		if stats := s.Stats(); stats.Accepted != 2 || stats.Overflow != 1 || stats.Oversized != 1 || stats.RetainedRecords != 2 || stats.RetainedBytes > 65536 {
			t.Fatal("допуск не ограничил полные записи", stats)
		}
		close(w.release)
		synctest.Wait()
		logger.Info("after_pressure", "payload", "complete-after-pressure")
		if err = s.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		decoder := json.NewDecoder(bytes.NewReader(w.output.Bytes()))
		var records []map[string]any
		for decoder.More() {
			var record map[string]any
			if err = decoder.Decode(&record); err != nil {
				t.Fatal("выход содержит частичную JSON-запись", err)
			}
			records = append(records, record)
		}
		if len(records) != 3 || records[0]["msg"] != "writer_started" || records[1]["msg"] != "worker_diagnostic" || records[2]["msg"] != "after_pressure" || records[2]["payload"] != "complete-after-pressure" {
			t.Fatal("перегрузка нарушила целостность, порядок или восстановление допуска", records)
		}
		r := records[1]
		for key, want := range map[string]string{"component": "ffmpeg", "stream_id": "stream", "session_id": "session", "group_id": "group", "destination_id": "destination", "level": "ERROR"} {
			if r[key] != want {
				t.Fatal("потеря корреляции", key)
			}
		}
		diagnostic := r["diagnostic"].(map[string]any)
		if diagnostic["token"] != token || diagnostic["header"] != header || diagnostic["operation"] != "stderr" {
			t.Fatal("изменены полные синтетические поля")
		}
		var gotArgs []string
		for _, arg := range diagnostic["argv"].([]any) {
			gotArgs = append(gotArgs, arg.(string))
		}
		if !reflect.DeepEqual(gotArgs, originalArgs) {
			t.Fatal("argv изменился после допуска записи", gotArgs)
		}
		raw := diagnostic["payload"].(map[string]any)
		decoded, err := base64.StdEncoding.DecodeString(raw["bytes"].(string))
		if err != nil || raw["encoding"] != "base64" || !bytes.Equal(decoded, original) {
			t.Fatal("полный диапазон бинарных значений необратим", err)
		}
		if stats := s.Stats(); stats.Written != 3 || stats.Accepted != 3 || stats.RetainedRecords != 0 || stats.RetainedBytes != 0 {
			t.Fatal("физическая запись не освободила бюджет", stats)
		}
	})
}
