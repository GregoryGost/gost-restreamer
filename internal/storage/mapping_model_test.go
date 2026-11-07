package storage

import (
	"context"
	"database/sql"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"gorm.io/gorm"
)

type mappingSQLContextKey struct{}

type mappingSQLStatement struct {
	sql      string
	vars     []any
	pool     gorm.ConnPool
	compiled bool
}

func TestMetadataWritesPreserveMapSQLAndTransaction(t *testing.T) {
	f := newDomainFixture(t, 1)
	ctx := context.Background()
	start := startDomainSession(t, f)
	session := readSession(t, f.store, start.SessionID)
	output := readOutputs(t, f.store, start.SessionID)[0]
	receipt := outputStartReceipt(t, f, output.ID)
	generation, err := f.service.GetGeneration(ctx, f.variant.GenerationID)
	if err != nil {
		t.Fatal(err)
	}
	var files []core.OwnedFile
	var events []core.Event
	if err := f.store.View(ctx, func(r core.Repositories) error {
		var err error
		files, err = r.Artifacts.OwnedFiles(ctx, core.ListQuery{Limit: core.MaxListLimit, GenerationID: generation.ID})
		if err != nil {
			return err
		}
		events, err = r.Runtime.Events(ctx, core.ListQuery{Limit: core.MaxListLimit, SessionID: session.ID})
		return err
	}); err != nil {
		t.Fatal(err)
	}
	if len(files) == 0 || len(events) == 0 {
		t.Fatal("фикстура не создала принадлежащие поколению файлы и события управления")
	}
	cleanup := core.CleanupIntent{
		Record: core.Record{ID: "mapping-cleanup", Revision: 1, CreatedAt: session.CreatedAt, UpdatedAt: session.CreatedAt},
		Kind:   core.CleanupRuntime, StreamID: f.stream.ID, SessionID: session.ID,
		Incarnation: session.Incarnation, Lifecycle: session.Lifecycle,
	}
	if err := f.store.Write(ctx, func(r core.Repositories) error { return r.Artifacts.InsertCleanup(ctx, cleanup) }); err != nil {
		t.Fatal(err)
	}

	var captured []mappingSQLStatement
	capture := func(db *gorm.DB) {
		if db.Statement.Context.Value(mappingSQLContextKey{}) != true {
			return
		}
		captured = append(captured, mappingSQLStatement{
			sql: db.Statement.SQL.String(), vars: append([]any(nil), db.Statement.Vars...),
			pool: db.Statement.ConnPool, compiled: db.Statement.Schema != nil,
		})
	}
	if err := f.store.db.Callback().Create().After("gorm:create").Register("test:capture_metadata_create", capture); err != nil {
		t.Fatal(err)
	}
	if err := f.store.db.Callback().Update().After("gorm:update").Register("test:capture_metadata_update", capture); err != nil {
		t.Fatal(err)
	}
	actualContext := context.WithValue(ctx, mappingSQLContextKey{}, true)
	rollback := errors.New("откат проверки SQL")
	for _, tc := range []struct {
		table string
		value any
	}{
		{"streams", f.stream}, {"profiles", f.profile}, {"groups", f.group},
		{"destinations", f.destinations[0]}, {"fallback_assets", f.asset},
		{"fallback_generations", generation}, {"owned_paths", files[0]},
		{"fallback_variants", f.variant}, {"sessions", session}, {"outputs", output},
		{"command_receipts", receipt}, {"cleanup_intents", cleanup}, {"events", events[0]},
	} {
		t.Run(tc.table, func(t *testing.T) {
			inserted := mappingInsertBoundaryValue(tc.value)
			normalized, err := normalize(inserted)
			if err != nil {
				t.Fatal(err)
			}
			values, err := fields(normalized)
			if err != nil {
				t.Fatal(err)
			}
			// Прежний запрос с map задаёт отношение эквивалентности SQL и параметров.
			want := f.store.db.Session(&gorm.Session{DryRun: true}).Table(tc.table).Create(values)
			if want.Error != nil {
				t.Fatal(want.Error)
			}
			captured = nil
			var expectedPool gorm.ConnPool
			err = f.store.Write(actualContext, func(r core.Repositories) error {
				repo := r.Config.(*repositories)
				expectedPool = repo.db.Statement.ConnPool
				return insertValue(actualContext, repo, tc.table, inserted)
			})
			requireDomainError(t, err, core.ErrConflict)
			mappingRequireStatement(t, captured, want, expectedPool)
			if tc.table == "events" {
				if strings.Contains(captured[0].sql, "revision") || strings.Contains(captured[0].sql, "updated_at") {
					t.Fatal("вставка события добавила отсутствующие в его схеме метаданные")
				}
				return
			}

			old := recordOf(tc.value)
			updated := reflect.New(reflect.TypeOf(tc.value)).Elem()
			updated.Set(reflect.ValueOf(tc.value))
			updated.FieldByName("Revision").SetUint(old.Revision + 1)
			updated.FieldByName("UpdatedAt").Set(reflect.ValueOf(old.UpdatedAt.Add(123456 * time.Microsecond)))
			normalized, err = normalize(updated.Interface())
			if err != nil {
				t.Fatal(err)
			}
			values, err = fields(normalized)
			if err != nil {
				t.Fatal(err)
			}
			delete(values, "id")
			delete(values, "created_at")
			want = f.store.db.Session(&gorm.Session{DryRun: true}).Table(tc.table).
				Where("id = ? AND revision = ?", old.ID, decimal(old.Revision)).Updates(values)
			if want.Error != nil {
				t.Fatal(want.Error)
			}
			captured = nil
			err = f.store.Write(actualContext, func(r core.Repositories) error {
				repo := r.Config.(*repositories)
				expectedPool = repo.db.Statement.ConnPool
				if err := updateValue(actualContext, repo, tc.table, updated.Interface(), old.Revision); err != nil {
					return err
				}
				return rollback
			})
			if !errors.Is(err, rollback) {
				t.Fatalf("обновление не дошло до проверяемого отката: %v", err)
			}
			mappingRequireStatement(t, captured, want, expectedPool)
		})
	}
}

func mappingInsertBoundaryValue(value any) any {
	switch v := value.(type) {
	case core.Stream:
		v.Fallback = core.FallbackPolicy{}
		v.Revision = math.MaxUint64
		return v
	case core.FallbackVariant:
		v.SizeBytes = math.MaxUint64
		return v
	case core.Session:
		v.Lifecycle = math.MaxUint64
		v.Issued, v.CleanupPending = false, false
		return v
	case core.SessionOutput:
		v.Generation, v.Attempt = math.MaxUint64, math.MaxUint64
		v.Issued, v.CleanupPending = false, false
		return v
	case core.CommandReceipt:
		v.Lifecycle, v.OutputGeneration, v.Attempt = math.MaxUint64, math.MaxUint64, math.MaxUint64
		return v
	default:
		return value
	}
}

func mappingRequireStatement(t *testing.T, got []mappingSQLStatement, want *gorm.DB, pool gorm.ConnPool) {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("выполнено записывающих SQL-запросов: %d, нужен один", len(got))
	}
	if !got[0].compiled {
		t.Fatal("GORM не разобрал модель перед записью метаданных")
	}
	if _, ok := pool.(*sql.Tx); !ok || got[0].pool != pool {
		t.Fatal("запись не использует точную транзакцию UnitOfWork")
	}
	if got[0].sql != want.Statement.SQL.String() || !reflect.DeepEqual(got[0].vars, want.Statement.Vars) {
		t.Fatalf("изменились SQL или упорядоченные параметры: SQL %q, ожидается %q; параметры совпадают: %t", got[0].sql, want.Statement.SQL.String(), reflect.DeepEqual(got[0].vars, want.Statement.Vars))
	}
	if strings.Contains(strings.ToUpper(got[0].sql), "RETURNING") {
		t.Fatal("запись метаданных добавила RETURNING")
	}
}
