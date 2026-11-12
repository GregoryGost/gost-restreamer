package storage

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/mattn/go-sqlite3"
)

// Проверяет рост WAL при удерживаемом читающем снимке, неполную контрольную точку до его завершения и
// успешный TRUNCATE с сохранением данных после освобождения.
func TestWALGrowthWithHeldReaderAndCheckpointAfterRelease(t *testing.T) {
	// s содержит временное файловое хранилище проверки WAL и ёмкости. dbPath задаёт путь временной
	// базы для наблюдения её WAL.
	s, dbPath := openDeliveryStore(t, testOptions())
	// ctx ограничивает все операции проверки десятисекундным сроком. cancel отменяет операции и
	// освобождает таймер проверки.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// initial сохраняет исходный поток перед проверяемыми записями.
	initial := deliveryStream("initial-stream", "Исходный поток")
	// err сообщает об ошибке вставки конфигурации потока.
	if err := s.Write(ctx, func(r core.Repositories) error {
		// Сохраняет исходный Stream перед удержанием читателя и проверкой роста WAL.
		return r.Config.InsertStream(ctx, initial)
	}); err != nil {
		t.Fatal(err)
	}
	// got содержит результат контрольной точки для проверки полного TRUNCATE.
	if got := deliveryCheckpoint(t, ctx, s, "TRUNCATE"); got != (deliveryCheckpointResult{}) {
		t.Fatalf("начальный checkpoint не завершён: %+v", got)
	}
	// before сохраняет начальный размер WAL в байтах.
	before := deliveryWALSize(t, dbPath)
	if before != 0 {
		t.Fatalf("WAL после начального TRUNCATE: %d байт", before)
	}

	// ready передаёт результат открытия удерживаемого читающего снимка.
	ready := make(chan error, 1)
	// release разрешает удерживаемому читателю завершить View.
	release := make(chan struct{})
	// done сигнализирует завершение горутины удерживаемого читателя.
	done := make(chan struct{})
	// releaseOnce обеспечивает однократное освобождение удерживаемого читателя.
	var releaseOnce sync.Once
	// readerErr сохраняет итог удерживаемой читающей транзакции.
	var readerErr error
	go func() {
		// В отдельной горутине удерживает согласованное чтение и передаёт результат после освобождения либо
		// отмены.

		defer close(done)
		readerErr = s.View(ctx, func(r core.Repositories) error {
			// Сверяет неизменность списка Streams внутри одного View несмотря на параллельные записи и попытки
			// контрольной точки WAL.

			// before сохраняет потоки удерживаемого читающего снимка до параллельных
			// записей. err сообщает об ошибке чтения упорядоченных конфигураций потоков.
			before, err := r.Config.Streams(ctx, core.ListQuery{Limit: core.MaxListLimit})
			ready <- err
			if err != nil {
				return err
			}
			if !reflect.DeepEqual(before, []core.Stream{initial}) {
				return fmt.Errorf("исходный снимок читателя изменён: %+v", before)
			}
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			// after содержит потоки того же читающего снимка после параллельных записей.
			after, err := r.Config.Streams(ctx, core.ListQuery{Limit: core.MaxListLimit})
			if err == nil && !reflect.DeepEqual(after, before) {
				return fmt.Errorf("checkpoint или запись изменили удерживаемый снимок: %+v", after)
			}
			return err
		})
	}()
	t.Cleanup(func() {
		// Освобождает читателя, отменяет контекст и ограниченно ждёт его завершения при любом выходе из теста.

		releaseOnce.Do(func() {
			// Однократно разрешает удерживаемому читателю завершиться при завершении теста.
			close(release)
		})
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("читающая транзакция не завершилась после отмены")
		}
	})
	select {
	// err сообщает об ошибке открытия удерживаемого читающего снимка.
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	// expected собирает все потоки, чьи записи должны сохраниться.
	expected := []core.Stream{initial}
	// firstSize сохраняет размер WAL в байтах после первой параллельной записи.
	var firstSize int64
	// i указывает номер потока, добавляемого при открытом старом читателе.
	for i := range 24 {
		// stream содержит очередной поток с большим именем для роста WAL.
		stream := deliveryStream(core.ID(fmt.Sprintf("committed-stream-%02d", i)), strings.Repeat("Данные потока ", 256))
		stream.CreatedAt = stream.CreatedAt.Add(time.Duration(i+1) * time.Second)
		stream.UpdatedAt = stream.CreatedAt
		// err сообщает об ошибке вставки конфигурации потока.
		if err := s.Write(ctx, func(r core.Repositories) error {
			// Сохраняет очередной Stream при открытом старом читателе для наблюдаемого роста WAL.
			return r.Config.InsertStream(ctx, stream)
		}); err != nil {
			t.Fatalf("запись %d при открытом читателе: %v", i, err)
		}
		expected = append(expected, stream)
		if i == 0 {
			firstSize = deliveryWALSize(t, dbPath)
		}
	}
	// grownSize сохраняет размер WAL в байтах после всей серии записей.
	grownSize := deliveryWALSize(t, dbPath)
	if firstSize <= before || grownSize <= firstSize {
		t.Fatalf("реальные записи не увеличили WAL: %d → %d → %d байт", before, firstSize, grownSize)
	}
	// passive содержит неполный результат PASSIVE при удерживаемом старом снимке.
	passive := deliveryCheckpoint(t, ctx, s, "PASSIVE")
	if passive.busy != 0 || passive.frames <= 0 || passive.checkpointed < 0 || passive.checkpointed >= passive.frames {
		t.Fatalf("PASSIVE завершил checkpoint поверх старого читателя: %+v", passive)
	}
	// blocked содержит результат заблокированного TRUNCATE при удерживаемом читателе.
	blocked := deliveryCheckpoint(t, ctx, s, "TRUNCATE")
	if blocked.busy != 1 || blocked.frames <= blocked.checkpointed || blocked.checkpointed < 0 {
		t.Fatalf("TRUNCATE не сообщил блокировку старым читателем: %+v", blocked)
	}
	// size содержит наблюдаемый размер WAL в байтах после контрольной точки.
	if size := deliveryWALSize(t, dbPath); size != grownSize {
		t.Fatalf("незавершённый checkpoint изменил размер WAL: %d → %d", grownSize, size)
	}
	deliveryRequireStreams(t, ctx, s, expected)

	releaseOnce.Do(func() {
		// Однократно освобождает старого читателя перед успешным TRUNCATE WAL.
		close(release)
	})
	select {
	case <-done:
		if readerErr != nil {
			t.Fatal(readerErr)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// got содержит результат контрольной точки для проверки полного TRUNCATE.
	if got := deliveryCheckpoint(t, ctx, s, "TRUNCATE"); got != (deliveryCheckpointResult{}) {
		t.Fatalf("checkpoint после завершения читателя: %+v", got)
	}
	// size содержит наблюдаемый размер WAL в байтах после контрольной точки.
	if size := deliveryWALSize(t, dbPath); size != 0 {
		t.Fatalf("успешный TRUNCATE оставил %d байт WAL", size)
	}
	deliveryRequireStreams(t, ctx, s, expected)
	// inUse содержит число занятых соединений после завершения проверяемой операции.
	if inUse := s.sqlDB.Stats().InUse; inUse != 0 {
		t.Fatalf("после checkpoint занято соединений: %d", inUse)
	}
	t.Logf("WAL: %d → %d → %d → 0 байт; PASSIVE %+v; занятый TRUNCATE %+v; сохранено потоков: %d", before, firstSize, grownSize, passive, blocked, len(expected))
}

// Проверяет настоящий SQLITE_FULL через лимит страниц временной БД, откат уже выполненной первой
// записи без автоматического повтора и успех явного повтора после снятия лимита.
func TestSQLiteCapacityFailureRollsBackWholeWriteAndAllowsRetryAfterLimitLift(t *testing.T) {
	// options ограничивает пул одним долгоживущим соединением для стабильного max_page_count.
	options := testOptions()
	// Одно долговечное соединение удерживает тестовый max_page_count для всех записей.
	options.MaxOpenConns, options.MaxIdleConns = 1, 1
	options.ConnMaxLifetime, options.ConnMaxIdleTime = time.Hour, time.Hour
	// s содержит временное файловое хранилище проверки WAL и ёмкости.
	s, _ := openDeliveryStore(t, options)
	// ctx ограничивает все операции проверки десятисекундным сроком. cancel отменяет операции и
	// освобождает таймер проверки.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// initial сохраняет исходный поток перед проверяемыми записями.
	initial := deliveryStream("existing-stream", "Исходное имя")
	// err сообщает об ошибке вставки конфигурации потока.
	if err := s.Write(ctx, func(r core.Repositories) error {
		// Сохраняет исходный Stream до установки тестового ограничения числа страниц SQLite.
		return r.Config.InsertStream(ctx, initial)
	}); err != nil {
		t.Fatal(err)
	}
	// pages сохраняет число страниц базы до установки ограничения ёмкости. originalLimit сохраняет
	// исходный max_page_count для снятия искусственного ограничения. appliedLimit принимает
	// фактически установленный предел страниц SQLite.
	var pages, originalLimit, appliedLimit int64
	// err сообщает об ошибке чтения или установки проверяемого параметра SQLite.
	if err := s.sqlDB.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages); err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке чтения или установки проверяемого параметра SQLite.
	if err := s.sqlDB.QueryRowContext(ctx, "PRAGMA max_page_count").Scan(&originalLimit); err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке чтения или установки проверяемого параметра SQLite.
	if err := s.sqlDB.QueryRowContext(ctx, fmt.Sprintf("PRAGMA max_page_count=%d", pages+4)).Scan(&appliedLimit); err != nil || appliedLimit != pages+4 {
		t.Fatalf("установить ограничение ёмкости: %d, %v", appliedLimit, err)
	}
	// updated содержит изменение исходного потока, которое должно откатиться при SQLITE_FULL.
	updated := initial
	updated.Revision++
	updated.Name = "Изменённое имя"
	// large содержит поток с большим именем для превышения лимита страниц.
	large := deliveryStream("large-stream", strings.Repeat("Данные ёмкости ", 1<<15))
	// attempts считает фактические исполнения функции записи. prefixWrites считает успешные первые
	// UPDATE внутри функции записи.
	attempts, prefixWrites := 0, 0
	// write обновляет исходный поток и вставляет большой в одной проверяемой транзакции.
	write := func(r core.Repositories) error {
		// Обновляет существующий Stream и вставляет большой Stream в одной транзакции для проверки полного
		// отката SQLITE_FULL и явного повтора после снятия лимита.

		attempts++
		// err сообщает об ошибке условного изменения конфигурации потока.
		if err := r.Config.UpdateStream(ctx, updated, initial.Revision); err != nil {
			return err
		}
		prefixWrites++
		return r.Config.InsertStream(ctx, large)
	}
	// err сообщает об ошибке общей транзакции UPDATE и большой INSERT при лимите страниц SQLite.
	err := s.Write(ctx, write)
	// sqliteErr принимает конкретную ошибку драйвера для проверки SQLITE_FULL.
	var sqliteErr sqlite3.Error
	if !errors.As(err, &sqliteErr) || sqliteErr.Code != sqlite3.ErrFull {
		t.Fatalf("нужен реальный SQLITE_FULL из записи: %v", err)
	}
	if attempts != 1 || prefixWrites != 1 {
		t.Fatalf("ошибка ёмкости повторена или получена до успешной первой записи: попыток %d, первых записей %d", attempts, prefixWrites)
	}
	deliveryRequireStreams(t, ctx, s, []core.Stream{initial})
	// inUse содержит число занятых соединений после завершения проверяемой операции.
	if inUse := s.sqlDB.Stats().InUse; inUse != 0 {
		t.Fatalf("отказ ёмкости оставил занятое соединение: %d", inUse)
	}
	// err сообщает об ошибке чтения или установки проверяемого параметра SQLite.
	if err := s.sqlDB.QueryRowContext(ctx, fmt.Sprintf("PRAGMA max_page_count=%d", originalLimit)).Scan(&appliedLimit); err != nil || appliedLimit != originalLimit {
		t.Fatalf("снять ограничение ёмкости: %d, %v", appliedLimit, err)
	}
	// err сообщает об ошибке явного повтора той же транзакции после снятия лимита страниц.
	if err := s.Write(ctx, write); err != nil {
		t.Fatalf("та же операция после снятия ограничения: %v", err)
	}
	if attempts != 2 || prefixWrites != 2 {
		t.Fatalf("число выполнений после явного повтора: %d/%d", attempts, prefixWrites)
	}
	deliveryRequireStreams(t, ctx, s, []core.Stream{updated, large})
	// SQLITE_FULL здесь вызван лимитом страниц временной БД, а не заполнением диска ОС.
	t.Logf("max_page_count=%d при page_count=%d: SQLite %d/%d, errno=%d; откат и запись после снятия лимита проверены", pages+4, pages, sqliteErr.Code, sqliteErr.ExtendedCode, sqliteErr.SystemErrno)
}

// Открывает временную файловую SQLite с указанными пределами и регистрирует закрытие Store в очистке
// теста.
func openDeliveryStore(t *testing.T, options Options) (*Store, string) {
	t.Helper()
	// dbPath задаёт путь временной базы для наблюдения её WAL.
	dbPath := filepath.Join(t.TempDir(), "state.sqlite")
	// s содержит временное файловое хранилище проверки WAL и ёмкости. err сообщает об ошибке
	// открытия временного файлового SQLite.
	s, err := Open(context.Background(), dbPath, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает временный Store проверки WAL и ёмкости после завершения теста.

		// err сообщает об ошибке закрытия временного хранилища.
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s, dbPath
}

// Создаёт тестовый Stream с постоянным временем UTC и отдельным ключом публикации на основе ID.
func deliveryStream(id core.ID, name string) core.Stream {
	// now задаёт постоянное время записей тестового потока.
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return core.Stream{ID: id, Revision: 1, CreatedAt: now, UpdatedAt: now, Name: name, PublicationKey: string(id) + "-synthetic-key"}
}

// deliveryCheckpointResult сохраняет признаки занятости и числа кадров результата WAL checkpoint.
type deliveryCheckpointResult struct { /* busy содержит признак незавершённой из-за занятости контрольной точки. frames содержит число кадров WAL в результате контрольной точки. checkpointed содержит число кадров, перенесённых контрольной точкой в основную базу. */
	busy, frames, checkpointed int
}

// Выполняет контрольную точку WAL указанного тестом режима и читает признаки занятости и числа кадров.
func deliveryCheckpoint(t *testing.T, ctx context.Context, s *Store, mode string) deliveryCheckpointResult {
	t.Helper()
	// result принимает три показателя выполненной контрольной точки WAL.
	var result deliveryCheckpointResult
	// err сообщает об ошибке чтения или установки проверяемого параметра SQLite.
	if err := s.sqlDB.QueryRowContext(ctx, "PRAGMA wal_checkpoint("+mode+")").Scan(&result.busy, &result.frames, &result.checkpointed); err != nil {
		t.Fatalf("checkpoint %s: %v", mode, err)
	}
	return result
}

// Читает фактический размер WAL-файла временной тестовой БД.
func deliveryWALSize(t *testing.T, dbPath string) int64 {
	t.Helper()
	// info содержит сведения WAL-файла для чтения его размера. err сообщает об ошибке чтения размера
	// файла WAL.
	info, err := os.Stat(dbPath + "-wal")
	if err != nil {
		t.Fatalf("проверить файл WAL: %v", err)
	}
	return info.Size()
}

// Проверяет точное совпадение сохраняемых Stream с ожидаемым упорядоченным набором в согласованной
// транзакции.
func deliveryRequireStreams(t *testing.T, ctx context.Context, s *Store, expected []core.Stream) {
	t.Helper()
	// err сообщает об ошибке чтения и точного сравнения всех сохранённых потоков.
	if err := s.View(ctx, func(r core.Repositories) error {
		// Читает Streams и сравнивает весь материализованный список с ожидаемыми значениями после контрольной
		// точки либо отказа записи.

		// got содержит весь упорядоченный набор сохранённых потоков для сравнения с ожидаемым.
		// err сообщает об ошибке чтения упорядоченных конфигураций потоков.
		got, err := r.Config.Streams(ctx, core.ListQuery{Limit: core.MaxListLimit})
		if err == nil && !reflect.DeepEqual(got, expected) {
			return fmt.Errorf("сохраняемые потоки отличаются: получено %d, ожидалось %d", len(got), len(expected))
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
