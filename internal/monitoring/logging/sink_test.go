package logging

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"strings"
	"testing"
	"testing/synctest"
	"time"
)

func testConfig() Config {
	return Config{MaxRecords: 8, MaxBytes: 16384, MaxRecordBytes: 4096, ShutdownTimeout: time.Second, Level: slog.LevelDebug}
}

func TestJSONPreservesFullValuesAndImmutableBinary(t *testing.T) {
	var out bytes.Buffer
	s, err := New(testConfig(), &out)
	if err != nil {
		t.Fatal(err)
	}
	payload := []byte{0, 0xff, '\n', 'x', 0x80}
	s.Logger().With("component", "ffmpeg", "stream_id", "stream-1", "session_id", "session-1").WithGroup("diagnostic").Info("worker_diagnostic", "operation", "stderr", "token", "synthetic-token+/=full", "header", "Bearer synthetic-header:full", "payload", "synthetic-payload\nwith\u0000tail", "argv", []string{"ffmpeg", "rtmps://synthetic.invalid/live?key=full"}, Binary("raw", payload))
	payload[0] = 7
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	var record map[string]any
	if err := json.Unmarshal(out.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record["msg"] != "worker_diagnostic" || record["component"] != "ffmpeg" || record["stream_id"] != "stream-1" || record["session_id"] != "session-1" || record["level"] != "INFO" {
		t.Fatal("утрачены поля корреляции")
	}
	d := record["diagnostic"].(map[string]any)
	if d["token"] != "synthetic-token+/=full" || d["header"] != "Bearer synthetic-header:full" || d["payload"] != "synthetic-payload\nwith\u0000tail" || d["operation"] != "stderr" {
		t.Fatal("значения журнала изменены")
	}
	argv := d["argv"].([]any)
	if len(argv) != 2 || argv[1] != "rtmps://synthetic.invalid/live?key=full" {
		t.Fatal("аргументы не сохранены массивом")
	}
	raw := d["raw"].(map[string]any)
	decoded, err := base64.StdEncoding.DecodeString(raw["bytes"].(string))
	if err != nil || raw["encoding"] != "base64" || !bytes.Equal(decoded, []byte{0, 0xff, '\n', 'x', 0x80}) {
		t.Fatal("бинарная диагностика необратима")
	}
	if stats := s.Stats(); stats.Accepted != 1 || stats.Written != 1 || stats.RetainedBytes != 0 || stats.RetainedRecords != 0 {
		t.Fatalf("неверные счётчики: %+v", stats)
	}
}

type blockedWriter struct {
	entered chan struct{}
	release chan struct{}
	out     bytes.Buffer
}

func (w *blockedWriter) Write(p []byte) (int, error) {
	select {
	case w.entered <- struct{}{}:
	default:
	}
	<-w.release
	return w.out.Write(p)
}

func TestBlockedSinkBoundsWholeRecordsAndKeepsPendingOwner(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := testConfig()
		c.MaxRecords = 2
		w := &blockedWriter{entered: make(chan struct{}, 1), release: make(chan struct{})}
		s, err := New(c, w)
		if err != nil {
			t.Fatal(err)
		}
		s.Logger().Info("first", "payload", "complete-one")
		<-w.entered
		s.Logger().Info("second", "payload", "complete-two")
		s.Logger().Info("third")
		s.Logger().Info("oversized", "payload", strings.Repeat("x", int(c.MaxRecordBytes)))
		stats := s.Stats()
		if stats.RetainedRecords != 2 || stats.RetainedBytes > c.MaxBytes || stats.Overflow != 1 || stats.Oversized != 1 || stats.Accepted != 2 {
			t.Fatalf("бюджет записей нарушен: %+v", stats)
		}
		if err := s.Close(context.Background()); !errors.Is(err, ErrPending) || !errors.Is(err, context.DeadlineExceeded) {
			t.Fatalf("зависший Sink не отражён: %v", err)
		}
		select {
		case <-s.Done():
			t.Fatal("ложное подтверждение физического завершения")
		default:
		}
		s.Logger().Info("closed")
		if s.Stats().Closed != 1 {
			t.Fatal("закрытый допуск не учитывает потерю")
		}
		close(w.release)
		if err := s.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		lines := bytes.Split(bytes.TrimSpace(w.out.Bytes()), []byte{'\n'})
		if len(lines) != 2 || !bytes.Contains(lines[0], []byte("complete-one")) || !bytes.Contains(lines[1], []byte("complete-two")) {
			t.Fatal("записи потеряли атомарность или порядок")
		}
	})
}

type failureWriter struct{ short bool }

func (w failureWriter) Write(p []byte) (int, error) {
	if w.short {
		return len(p) - 1, nil
	}
	return 0, io.ErrClosedPipe
}

func TestWriterFailuresAndRepeatedLifecycle(t *testing.T) {
	for _, short := range []bool{false, true} {
		for i := 0; i < 3; i++ {
			s, err := New(testConfig(), failureWriter{short: short})
			if err != nil {
				t.Fatal(err)
			}
			s.Logger().Error("failure")
			want := io.ErrClosedPipe
			if short {
				want = io.ErrShortWrite
			}
			if err := s.Close(context.Background()); !errors.Is(err, want) {
				t.Fatalf("ошибка io.Writer потеряна: %v", err)
			}
			if s.Stats().WriterErrors != 1 || s.Stats().RetainedRecords != 0 {
				t.Fatal("ошибка не освобождает бюджет")
			}
			if err := s.Close(context.Background()); !errors.Is(err, want) {
				t.Fatal(err)
			}
		}
	}
}

func TestByteBudgetAndCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		c := testConfig()
		c.MaxBytes = 512
		c.MaxRecordBytes = 512
		w := &blockedWriter{entered: make(chan struct{}, 1), release: make(chan struct{})}
		s, err := New(c, w)
		if err != nil {
			t.Fatal(err)
		}
		s.Logger().Info("first", "payload", strings.Repeat("x", 250))
		<-w.entered
		s.Logger().Info("second", "payload", strings.Repeat("x", 250))
		if s.Stats().Overflow != 1 || s.Stats().RetainedRecords != 1 {
			t.Fatal("байтовый лимит не учитывает выполняющуюся запись")
		}
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		if err := s.Close(ctx); !errors.Is(err, context.Canceled) || !errors.Is(err, ErrPending) {
			t.Fatal(err)
		}
		close(w.release)
		if err := s.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}

func TestConfigAndLevel(t *testing.T) {
	for _, c := range []Config{{}, {MaxRecords: 1, MaxBytes: 1, MaxRecordBytes: 2, ShutdownTimeout: time.Second}} {
		if _, err := New(c, io.Discard); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	}
	if _, err := New(testConfig(), nil); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	var out bytes.Buffer
	c := testConfig()
	c.Level = slog.LevelWarn
	s, err := New(c, &out)
	if err != nil {
		t.Fatal(err)
	}
	s.Logger().Info("ignored")
	if err := s.Close(context.Background()); err != nil || out.Len() != 0 || s.Stats().Accepted != 0 {
		t.Fatal("уровень журнала не соблюдён")
	}
}

func TestStderrHasOneOwnerUntilJoin(t *testing.T) {
	if _, err := NewStderr(Config{}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		s, err := NewStderr(testConfig())
		if err != nil {
			t.Fatal(err)
		}
		if _, err := NewStderr(testConfig()); !errors.Is(err, ErrBusy) {
			t.Fatal("создан второй владелец stderr")
		}
		if err := s.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}
