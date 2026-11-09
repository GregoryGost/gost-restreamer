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

func TestWALGrowthWithHeldReaderAndCheckpointAfterRelease(t *testing.T) {
	s, dbPath := openDeliveryStore(t, testOptions())
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	initial := deliveryStream("initial-stream", "Исходный поток")
	if err := s.Write(ctx, func(r core.Repositories) error { return r.Config.InsertStream(ctx, initial) }); err != nil {
		t.Fatal(err)
	}
	if got := deliveryCheckpoint(t, ctx, s, "TRUNCATE"); got != (deliveryCheckpointResult{}) {
		t.Fatalf("начальный checkpoint не завершён: %+v", got)
	}
	before := deliveryWALSize(t, dbPath)
	if before != 0 {
		t.Fatalf("WAL после начального TRUNCATE: %d байт", before)
	}

	ready := make(chan error, 1)
	release := make(chan struct{})
	done := make(chan struct{})
	var releaseOnce sync.Once
	var readerErr error
	go func() {
		defer close(done)
		readerErr = s.View(ctx, func(r core.Repositories) error {
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
			after, err := r.Config.Streams(ctx, core.ListQuery{Limit: core.MaxListLimit})
			if err == nil && !reflect.DeepEqual(after, before) {
				return fmt.Errorf("checkpoint или запись изменили удерживаемый снимок: %+v", after)
			}
			return err
		})
	}()
	t.Cleanup(func() {
		releaseOnce.Do(func() { close(release) })
		cancel()
		select {
		case <-done:
		case <-time.After(time.Second):
			t.Error("читающая транзакция не завершилась после отмены")
		}
	})
	select {
	case err := <-ready:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}

	expected := []core.Stream{initial}
	var firstSize int64
	for i := range 24 {
		stream := deliveryStream(core.ID(fmt.Sprintf("committed-stream-%02d", i)), strings.Repeat("Данные потока ", 256))
		stream.CreatedAt = stream.CreatedAt.Add(time.Duration(i+1) * time.Second)
		stream.UpdatedAt = stream.CreatedAt
		if err := s.Write(ctx, func(r core.Repositories) error { return r.Config.InsertStream(ctx, stream) }); err != nil {
			t.Fatalf("запись %d при открытом читателе: %v", i, err)
		}
		expected = append(expected, stream)
		if i == 0 {
			firstSize = deliveryWALSize(t, dbPath)
		}
	}
	grownSize := deliveryWALSize(t, dbPath)
	if firstSize <= before || grownSize <= firstSize {
		t.Fatalf("реальные записи не увеличили WAL: %d → %d → %d байт", before, firstSize, grownSize)
	}
	passive := deliveryCheckpoint(t, ctx, s, "PASSIVE")
	if passive.busy != 0 || passive.frames <= 0 || passive.checkpointed < 0 || passive.checkpointed >= passive.frames {
		t.Fatalf("PASSIVE завершил checkpoint поверх старого читателя: %+v", passive)
	}
	blocked := deliveryCheckpoint(t, ctx, s, "TRUNCATE")
	if blocked.busy != 1 || blocked.frames <= blocked.checkpointed || blocked.checkpointed < 0 {
		t.Fatalf("TRUNCATE не сообщил блокировку старым читателем: %+v", blocked)
	}
	if size := deliveryWALSize(t, dbPath); size != grownSize {
		t.Fatalf("незавершённый checkpoint изменил размер WAL: %d → %d", grownSize, size)
	}
	deliveryRequireStreams(t, ctx, s, expected)

	releaseOnce.Do(func() { close(release) })
	select {
	case <-done:
		if readerErr != nil {
			t.Fatal(readerErr)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	if got := deliveryCheckpoint(t, ctx, s, "TRUNCATE"); got != (deliveryCheckpointResult{}) {
		t.Fatalf("checkpoint после завершения читателя: %+v", got)
	}
	if size := deliveryWALSize(t, dbPath); size != 0 {
		t.Fatalf("успешный TRUNCATE оставил %d байт WAL", size)
	}
	deliveryRequireStreams(t, ctx, s, expected)
	if inUse := s.sqlDB.Stats().InUse; inUse != 0 {
		t.Fatalf("после checkpoint занято соединений: %d", inUse)
	}
	t.Logf("WAL: %d → %d → %d → 0 байт; PASSIVE %+v; занятый TRUNCATE %+v; сохранено потоков: %d", before, firstSize, grownSize, passive, blocked, len(expected))
}

func TestSQLiteCapacityFailureRollsBackWholeWriteAndAllowsRetryAfterLimitLift(t *testing.T) {
	options := testOptions()
	// Одно долговечное соединение удерживает тестовый max_page_count для всех записей.
	options.MaxOpenConns, options.MaxIdleConns = 1, 1
	options.ConnMaxLifetime, options.ConnMaxIdleTime = time.Hour, time.Hour
	s, _ := openDeliveryStore(t, options)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	initial := deliveryStream("existing-stream", "Исходное имя")
	if err := s.Write(ctx, func(r core.Repositories) error { return r.Config.InsertStream(ctx, initial) }); err != nil {
		t.Fatal(err)
	}
	var pages, originalLimit, appliedLimit int64
	if err := s.sqlDB.QueryRowContext(ctx, "PRAGMA page_count").Scan(&pages); err != nil {
		t.Fatal(err)
	}
	if err := s.sqlDB.QueryRowContext(ctx, "PRAGMA max_page_count").Scan(&originalLimit); err != nil {
		t.Fatal(err)
	}
	if err := s.sqlDB.QueryRowContext(ctx, fmt.Sprintf("PRAGMA max_page_count=%d", pages+4)).Scan(&appliedLimit); err != nil || appliedLimit != pages+4 {
		t.Fatalf("установить ограничение ёмкости: %d, %v", appliedLimit, err)
	}
	updated := initial
	updated.Revision++
	updated.Name = "Изменённое имя"
	large := deliveryStream("large-stream", strings.Repeat("Данные ёмкости ", 1<<15))
	attempts, prefixWrites := 0, 0
	write := func(r core.Repositories) error {
		attempts++
		if err := r.Config.UpdateStream(ctx, updated, initial.Revision); err != nil {
			return err
		}
		prefixWrites++
		return r.Config.InsertStream(ctx, large)
	}
	err := s.Write(ctx, write)
	var sqliteErr sqlite3.Error
	if !errors.As(err, &sqliteErr) || sqliteErr.Code != sqlite3.ErrFull {
		t.Fatalf("нужен реальный SQLITE_FULL из записи: %v", err)
	}
	if attempts != 1 || prefixWrites != 1 {
		t.Fatalf("ошибка ёмкости повторена или получена до успешной первой записи: попыток %d, первых записей %d", attempts, prefixWrites)
	}
	deliveryRequireStreams(t, ctx, s, []core.Stream{initial})
	if inUse := s.sqlDB.Stats().InUse; inUse != 0 {
		t.Fatalf("отказ ёмкости оставил занятое соединение: %d", inUse)
	}
	if err := s.sqlDB.QueryRowContext(ctx, fmt.Sprintf("PRAGMA max_page_count=%d", originalLimit)).Scan(&appliedLimit); err != nil || appliedLimit != originalLimit {
		t.Fatalf("снять ограничение ёмкости: %d, %v", appliedLimit, err)
	}
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

func openDeliveryStore(t *testing.T, options Options) (*Store, string) {
	t.Helper()
	dbPath := filepath.Join(t.TempDir(), "state.sqlite")
	s, err := Open(context.Background(), dbPath, options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s, dbPath
}

func deliveryStream(id core.ID, name string) core.Stream {
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	return core.Stream{ID: id, Revision: 1, CreatedAt: now, UpdatedAt: now, Name: name, PublicationKey: string(id) + "-synthetic-key"}
}

type deliveryCheckpointResult struct{ busy, frames, checkpointed int }

func deliveryCheckpoint(t *testing.T, ctx context.Context, s *Store, mode string) deliveryCheckpointResult {
	t.Helper()
	var result deliveryCheckpointResult
	if err := s.sqlDB.QueryRowContext(ctx, "PRAGMA wal_checkpoint("+mode+")").Scan(&result.busy, &result.frames, &result.checkpointed); err != nil {
		t.Fatalf("checkpoint %s: %v", mode, err)
	}
	return result
}

func deliveryWALSize(t *testing.T, dbPath string) int64 {
	t.Helper()
	info, err := os.Stat(dbPath + "-wal")
	if err != nil {
		t.Fatalf("проверить файл WAL: %v", err)
	}
	return info.Size()
}

func deliveryRequireStreams(t *testing.T, ctx context.Context, s *Store, expected []core.Stream) {
	t.Helper()
	if err := s.View(ctx, func(r core.Repositories) error {
		got, err := r.Config.Streams(ctx, core.ListQuery{Limit: core.MaxListLimit})
		if err == nil && !reflect.DeepEqual(got, expected) {
			return fmt.Errorf("сохраняемые потоки отличаются: получено %d, ожидалось %d", len(got), len(expected))
		}
		return err
	}); err != nil {
		t.Fatal(err)
	}
}
