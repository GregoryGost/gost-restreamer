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

// testConfig задаёт конечную очередь и включает DEBUG для проверки всех диагностических полей.
func testConfig() Config {
	return Config{MaxRecords: 8, MaxBytes: 16384, MaxRecordBytes: 4096, ShutdownTimeout: time.Second, Level: slog.LevelDebug}
}

// Проверяет сохранение полных синтетических значений, структуры argv и обратимых бинарных данных после
// изменения исходного буфера.
func TestJSONPreservesFullValuesAndImmutableBinary(t *testing.T) {
	// out сохраняет настоящий JSON-вывод для проверки полных значений и корреляции.
	var out bytes.Buffer
	// s содержит журнал с DEBUG-порогом; err содержит отказ создания тестового Sink.
	s, err := New(testConfig(), &out)
	if err != nil {
		t.Fatal(err)
	}
	// payload содержит бинарные байты, которые изменяются после допуска для проверки собственной
	// копии записи.
	payload := []byte{0, 0xff, '\n', 'x', 0x80}
	s.Logger().With("component", "ffmpeg", "stream_id", "stream-1", "session_id", "session-1").WithGroup("diagnostic").Info("worker_diagnostic", "operation", "stderr", "token", "synthetic-token+/=full", "header", "Bearer synthetic-header:full", "payload", "synthetic-payload\nwith\u0000tail", "argv", []string{"ffmpeg", "rtmps://synthetic.invalid/live?key=full"}, Binary("raw", payload))
	payload[0] = 7
	// err содержит результат полного закрытия журнала перед чтением JSON.
	if err := s.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	// record принимает поля выведенной JSON-записи с вложенной диагностикой.
	var record map[string]any
	// err содержит отказ разбора JSON настоящего вывода журнала.
	if err := json.Unmarshal(out.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record["msg"] != "worker_diagnostic" || record["component"] != "ffmpeg" || record["stream_id"] != "stream-1" || record["session_id"] != "session-1" || record["level"] != "INFO" {
		t.Fatal("утрачены поля корреляции")
	}
	// d содержит вложенные поля diagnostic для проверки полных синтетических значений.
	d := record["diagnostic"].(map[string]any)
	if d["token"] != "synthetic-token+/=full" || d["header"] != "Bearer synthetic-header:full" || d["payload"] != "synthetic-payload\nwith\u0000tail" || d["operation"] != "stderr" {
		t.Fatal("значения журнала изменены")
	}
	// argv содержит выведенный JSON-массив аргументов для проверки структуры и точных строк.
	argv := d["argv"].([]any)
	if len(argv) != 2 || argv[1] != "rtmps://synthetic.invalid/live?key=full" {
		t.Fatal("аргументы не сохранены массивом")
	}
	// raw содержит описание обратимого кодирования бинарной диагностики.
	raw := d["raw"].(map[string]any)
	// decoded содержит восстановленные байты диагностики для сравнения с исходным содержимым до
	// мутации.
	decoded, err := base64.StdEncoding.DecodeString(raw["bytes"].(string))
	if err != nil || raw["encoding"] != "base64" || !bytes.Equal(decoded, []byte{0, 0xff, '\n', 'x', 0x80}) {
		t.Fatal("бинарная диагностика необратима")
	}
	// stats содержит итоговые счётчики единственной записи после полного закрытия.
	if stats := s.Stats(); stats.Accepted != 1 || stats.Written != 1 || stats.RetainedBytes != 0 || stats.RetainedRecords != 0 {
		t.Fatalf("неверные счётчики: %+v", stats)
	}
}

// blockedWriter удерживает физический Write до разрешения теста, сохраняя принятые байты после
// освобождения.
type blockedWriter struct {
	// entered сообщает о начале физической записи без блокировки писателя на уведомлении.
	entered chan struct{}
	// release разрешает заблокированной физической записи продолжиться.
	release chan struct{}
	// out накапливает байты, реально переданные после снятия блокировки.
	out bytes.Buffer
}

// Write удерживает физическую запись до явного разрешения теста и сообщает о её начале.
func (w *blockedWriter) Write(p []byte) (int, error) {
	select {
	case w.entered <- struct{}{}:
	default:
	}
	<-w.release
	return w.out.Write(p)
}

// Проверяет общий бюджет ожидающих и выполняемой записей, целостность потерь и сохранение
// незавершённого владельца до освобождения получателя.
func TestBlockedSinkBoundsWholeRecordsAndKeepsPendingOwner(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Проверяет предел целых записей и сохранение незавершённого владельца при заблокированном физическом
		// получателе.

		// c задаёт очередь из двух записей, включая находящуюся у физического получателя.
		c := testConfig()
		c.MaxRecords = 2
		// w создаёт управляемого медленного получателя для проверки общих бюджетов.
		w := &blockedWriter{entered: make(chan struct{}, 1), release: make(chan struct{})}
		// s содержит журнал с заблокированным получателем; err содержит отказ создания.
		s, err := New(c, w)
		if err != nil {
			t.Fatal(err)
		}
		s.Logger().Info("first", "payload", "complete-one")
		<-w.entered
		s.Logger().Info("second", "payload", "complete-two")
		s.Logger().Info("third")
		s.Logger().Info("oversized", "payload", strings.Repeat("x", int(c.MaxRecordBytes)))
		// stats содержит счётчики после двух допусков, одного переполнения и одной слишком
		// большой записи.
		stats := s.Stats()
		if stats.RetainedRecords != 2 || stats.RetainedBytes > c.MaxBytes || stats.Overflow != 1 || stats.Oversized != 1 || stats.Accepted != 2 {
			t.Fatalf("бюджет записей нарушен: %+v", stats)
		}
		// err содержит результат закрытия при удерживаемом Write, ожидаются ErrPending и
		// истечение срока.
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
		// err содержит итог повторного закрытия после освобождения физического получателя.
		if err := s.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		// lines содержит фактически записанные JSON-строки для проверки порядка и целостности
		// двух допущенных сообщений.
		lines := bytes.Split(bytes.TrimSpace(w.out.Bytes()), []byte{'\n'})
		if len(lines) != 2 || !bytes.Contains(lines[0], []byte("complete-one")) || !bytes.Contains(lines[1], []byte("complete-two")) {
			t.Fatal("записи потеряли атомарность или порядок")
		}
	})
}

// failureWriter имитирует отказ получателя или короткую запись без явной ошибки.
type failureWriter struct { /* short выбирает короткую запись вместо io.ErrClosedPipe. */
	short bool
}

// Write имитирует короткую запись без ошибки либо отказ закрытого канала для проверки физического
// получателя.
func (w failureWriter) Write(p []byte) (int, error) {
	if w.short {
		return len(p) - 1, nil
	}
	return 0, io.ErrClosedPipe
}

// Проверяет сохранение ошибок получателя, освобождение бюджета и отсутствие повторной записи при
// повторном Close.
func TestWriterFailuresAndRepeatedLifecycle(t *testing.T) {
	// short задаёт очередной вид физического отказа для повторных жизненных циклов журнала.
	for _, short := range []bool{false, true} {
		for range 3 {
			// s содержит очередной журнал с неисправным получателем; err содержит отказ
			// создания.
			s, err := New(testConfig(), failureWriter{short: short})
			if err != nil {
				t.Fatal(err)
			}
			s.Logger().Error("failure")
			// want задаёт ожидаемую ошибку получателя либо обнаруженной короткой записи.
			want := io.ErrClosedPipe
			if short {
				want = io.ErrShortWrite
			}
			// err содержит результат первого Close для проверки сохранения ошибки физической
			// записи.
			if err := s.Close(context.Background()); !errors.Is(err, want) {
				t.Fatalf("ошибка io.Writer потеряна: %v", err)
			}
			if s.Stats().WriterErrors != 1 || s.Stats().RetainedRecords != 0 {
				t.Fatal("ошибка не освобождает бюджет")
			}
			// err содержит результат повторного Close, который должен сохранить ту же ошибку
			// без новой записи.
			if err := s.Close(context.Background()); !errors.Is(err, want) {
				t.Fatal(err)
			}
		}
	}
}

// Проверяет учёт выполняемой записи в байтовом бюджете и возврат ErrPending при отменённом ожидании.
func TestByteBudgetAndCancellation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Проверяет байтовый бюджет с учётом выполняющейся записи и отмену ожидания закрытия журнала.

		// c задаёт байтовый бюджет, в который помещается одна удерживаемая полная запись.
		c := testConfig()
		c.MaxBytes = 512
		c.MaxRecordBytes = 512
		// w удерживает первую физическую запись, чтобы её байты оставались занятыми.
		w := &blockedWriter{entered: make(chan struct{}, 1), release: make(chan struct{})}
		// s содержит журнал для проверки байтового переполнения; err содержит отказ создания.
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
		// ctx задаёт немедленно отменённое ожидание Close; cancel отменяет его перед вызовом.
		ctx, cancel := context.WithCancel(context.Background())
		cancel()
		// err содержит отказ отменённого ожидания с ErrPending без подтверждения завершения
		// писателя.
		if err := s.Close(ctx); !errors.Is(err, context.Canceled) || !errors.Is(err, ErrPending) {
			t.Fatal(err)
		}
		close(w.release)
		// err содержит итог закрытия после освобождения первой физической записи.
		if err := s.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	})
}

// Проверяет недопустимые пределы и отсутствие получателя, а также подавление INFO настроенным порогом
// WARN.
func TestConfigAndLevel(t *testing.T) {
	// c задаёт очередную недопустимую конфигурацию очереди журнала.
	for _, c := range []Config{{}, {MaxRecords: 1, MaxBytes: 1, MaxRecordBytes: 2, ShutdownTimeout: time.Second}} {
		// err содержит отказ создания журнала с недопустимыми бюджетами.
		if _, err := New(c, io.Discard); !errors.Is(err, ErrInvalid) {
			t.Fatal(err)
		}
	}
	// err содержит отказ создания журнала без физического получателя.
	if _, err := New(testConfig(), nil); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	// out сохраняет возможный вывод INFO, который должен быть подавлен порогом WARN.
	var out bytes.Buffer
	// c задаёт допустимые бюджеты и порог WARN для проверки фильтра уровня.
	c := testConfig()
	c.Level = slog.LevelWarn
	// s содержит журнал с WARN-порогом; err содержит отказ его создания.
	s, err := New(c, &out)
	if err != nil {
		t.Fatal(err)
	}
	s.Logger().Info("ignored")
	// err содержит итог закрытия журнала, который не должен принять или вывести INFO.
	if err := s.Close(context.Background()); err != nil || out.Len() != 0 || s.Stats().Accepted != 0 {
		t.Fatal("уровень журнала не соблюдён")
	}
}

// Проверяет запрет второго владельца stderr до завершения первого и повторное получение владения после
// подтверждённого завершения.
func TestStderrHasOneOwnerUntilJoin(t *testing.T) {
	// err содержит отказ попытки занять stderr с недопустимой конфигурацией.
	if _, err := NewStderr(Config{}); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	for range 2 {
		// s содержит единственного текущего владельца stderr; err содержит отказ его создания.
		s, err := NewStderr(testConfig())
		if err != nil {
			t.Fatal(err)
		}
		// err содержит отказ второго владельца stderr, ожидается ErrBusy.
		if _, err := NewStderr(testConfig()); !errors.Is(err, ErrBusy) {
			t.Fatal("создан второй владелец stderr")
		}
		// err содержит итог завершения владельца stderr до следующего цикла получения.
		if err := s.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
}
