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

// acceptanceGatedWriter удерживает физический вывод журнала, позволяя проверить переполнение и мутацию
// исходных данных.
type acceptanceGatedWriter struct {
	// entered сообщает о начале Write, чтобы заполнить оставшуюся очередь в контролируемом
	// состоянии.
	entered chan struct{}
	// release разрешает физическому Write сохранить байты и завершиться.
	release chan struct{}
	// output накапливает фактически записанные целые сообщения после освобождения.
	output bytes.Buffer
}

// Write сообщает о начале физической записи и удерживает её до разрешения теста, затем сохраняет
// исходные байты.
func (w *acceptanceGatedWriter) Write(data []byte) (int, error) {
	select {
	case w.entered <- struct{}{}:
	default:
	}
	<-w.release
	return w.output.Write(data)
}

// Проверяет полные значения и корреляцию настоящих JSON-записей при переполнении, изменение исходных
// argv/байтов после допуска и восстановление записи после снятия нагрузки.
func TestAcceptanceJSONFullValuesSurvivePressureAndCallerMutation(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Проверяет целостность и копирование полных JSON-записей при переполнении очереди и изменении
		// исходных данных вызывающим кодом.

		// w содержит управляемого получателя с заблокированным первым Write.
		w := &acceptanceGatedWriter{entered: make(chan struct{}, 1), release: make(chan struct{})}
		// s содержит журнал с бюджетом двух удерживаемых записей; err содержит отказ его
		// создания.
		s, err := New(Config{MaxRecords: 2, MaxBytes: 65536, MaxRecordBytes: 4096, ShutdownTimeout: time.Second}, w)
		if err != nil {
			t.Fatal(err)
		}
		defer func() {
			// Освобождает заблокированного получателя журнала и завершает запись принятых сообщений после теста.

			select {
			case <-w.release:
			default:
				close(w.release)
			}
			_ = s.Close(context.Background())
		}()
		// logger добавляет одинаковую корреляцию всем событиям проверяемого диагностического
		// потока.
		logger := s.Logger().With("component", "ffmpeg", "stream_id", "stream", "session_id", "session", "group_id", "group", "destination_id", "destination")
		logger.Info("writer_started")
		<-w.entered
		// payload содержит полный диапазон 256 байтов для проверки обратимого JSON-кодирования.
		payload := make([]byte, 256)
		// i указывает позицию payload, одновременно задающую проверяемое значение байта.
		for i := range payload {
			payload[i] = byte(i)
		}
		// original сохраняет независимые исходные байты до изменения буфера вызывающим кодом.
		original := bytes.Clone(payload)
		// argv содержит синтетические аргументы с URL, переводами строк и кавычками, изменяемые
		// после допуска.
		argv := []string{"ffmpeg", "rtmps://synthetic.invalid/live?key=synthetic%2Bfull", "строка\n\"кавычки\""}
		// originalArgs сохраняет ожидаемые аргументы до мутации исходного среза.
		originalArgs := append([]string(nil), argv...)
		// token содержит полное синтетическое значение с экранируемыми символами для проверки
		// режима full.
		token := "synthetic-token:/+\"\\\nполное"
		// header содержит полное синтетическое значение заголовка для проверки отсутствия
		// усечения.
		header := "Bearer synthetic-header+/= with tail"
		logger.WithGroup("diagnostic").LogAttrs(context.Background(), slog.LevelError, "worker_diagnostic",
			slog.String("operation", "stderr"), slog.String("token", token), slog.String("header", header),
			slog.Any("argv", argv), Binary("payload", payload))
		clear(payload)
		argv[1] = "caller-changed"
		logger.Info("overflow_must_disappear")
		logger.Info("oversized_must_disappear", "payload", strings.Repeat("x", 4096))
		// stats содержит состояние бюджета после двух принятых записей и двух целых потерь.
		if stats := s.Stats(); stats.Accepted != 2 || stats.Overflow != 1 || stats.Oversized != 1 || stats.RetainedRecords != 2 || stats.RetainedBytes > 65536 {
			t.Fatal("допуск не ограничил полные записи", stats)
		}
		close(w.release)
		synctest.Wait()
		logger.Info("after_pressure", "payload", "complete-after-pressure")
		if err = s.Close(context.Background()); err != nil {
			t.Fatal(err)
		}
		// decoder читает поток целых JSON-записей после освобождения получателя и закрытия
		// журнала.
		decoder := json.NewDecoder(bytes.NewReader(w.output.Bytes()))
		// records накапливает выведенные события для проверки порядка и восстановления допуска.
		var records []map[string]any
		for decoder.More() {
			// record принимает очередную полную запись JSON из реального вывода.
			var record map[string]any
			if err = decoder.Decode(&record); err != nil {
				t.Fatal("выход содержит частичную JSON-запись", err)
			}
			records = append(records, record)
		}
		if len(records) != 3 || records[0]["msg"] != "writer_started" || records[1]["msg"] != "worker_diagnostic" || records[2]["msg"] != "after_pressure" || records[2]["payload"] != "complete-after-pressure" {
			t.Fatal("перегрузка нарушила целостность, порядок или восстановление допуска", records)
		}
		// r содержит диагностическое событие между начальным сообщением и восстановленной
		// записью.
		r := records[1]
		// key задаёт проверяемое поле корреляции; want содержит его точное ожидаемое значение.
		for key, want := range map[string]string{"component": "ffmpeg", "stream_id": "stream", "session_id": "session", "group_id": "group", "destination_id": "destination", "level": "ERROR"} {
			if r[key] != want {
				t.Fatal("потеря корреляции", key)
			}
		}
		// diagnostic содержит вложенные полные значения токена, заголовка, аргументов и бинарных
		// данных.
		diagnostic := r["diagnostic"].(map[string]any)
		if diagnostic["token"] != token || diagnostic["header"] != header || diagnostic["operation"] != "stderr" {
			t.Fatal("изменены полные синтетические поля")
		}
		// gotArgs собирает выведенные строковые аргументы для сравнения с originalArgs.
		var gotArgs []string
		// arg содержит очередной аргумент, прочитанный из JSON-массива диагностической записи.
		for _, arg := range diagnostic["argv"].([]any) {
			gotArgs = append(gotArgs, arg.(string))
		}
		if !reflect.DeepEqual(gotArgs, originalArgs) {
			t.Fatal("argv изменился после допуска записи", gotArgs)
		}
		// raw содержит обозначение base64 и строку исходных бинарных данных.
		raw := diagnostic["payload"].(map[string]any)
		// decoded содержит восстановленный полный диапазон байтов, ожидаемый до мутации payload.
		decoded, err := base64.StdEncoding.DecodeString(raw["bytes"].(string))
		if err != nil || raw["encoding"] != "base64" || !bytes.Equal(decoded, original) {
			t.Fatal("полный диапазон бинарных значений необратим", err)
		}
		// stats содержит итоговые счётчики трёх записей и полностью освобождённого бюджета.
		if stats := s.Stats(); stats.Written != 3 || stats.Accepted != 3 || stats.RetainedRecords != 0 || stats.RetainedBytes != 0 {
			t.Fatal("физическая запись не освободила бюджет", stats)
		}
	})
}
