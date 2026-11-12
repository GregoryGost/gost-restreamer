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

// mappingSQLContextKey отмечает только SQL проверяемой операции для захвата callback GORM.
type mappingSQLContextKey struct{}

// mappingSQLStatement сохраняет SQL, упорядоченные параметры и точную транзакцию выполненного запроса.
type mappingSQLStatement struct {
	// sql содержит фактический SQL выполненной записи метаданных.
	sql string
	// vars сохраняет независимую копию упорядоченных параметров SQL.
	vars []any
	// pool идентифицирует точный ConnPool выполненной записи.
	pool gorm.ConnPool
	// compiled показывает, разобрана ли GORM-схема модели перед записью.
	compiled bool
}

// Проверяет одинаковые SQL и упорядоченные параметры записей метаданных при разобранной модели GORM,
// сохранение точной транзакции и отсутствие RETURNING.
func TestMetadataWritesPreserveMapSQLAndTransaction(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 1)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// start содержит принятую квитанцию первоначального запуска сессии.
	start := startDomainSession(t, f)
	// session содержит сохраняемый запуск для проверки состояния и владения.
	session := readSession(t, f.store, start.SessionID)
	// output содержит сохраняемый выход с самостоятельным снимком и намерением.
	output := readOutputs(t, f.store, start.SessionID)[0]
	// receipt содержит сохраняемую квитанцию выбранной команды.
	receipt := outputStartReceipt(t, f, output.ID)
	// generation содержит опубликованное поколение тестовой заглушки. err сообщает об ошибке чтения
	// полного поколения подготовки.
	generation, err := f.service.GetGeneration(ctx, f.variant.GenerationID)
	if err != nil {
		t.Fatal(err)
	}
	// files принимает записи владения файлами опубликованного поколения.
	var files []core.OwnedFile
	// events принимает служебные события тестовой сессии.
	var events []core.Event
	// err сообщает об ошибке чтения принадлежащих поколению файлов и событий запуска для проверки
	// SQL.
	if err := f.store.View(ctx, func(r core.Repositories) error {
		// Читает принадлежащие поколению файлы и служебные события Session для проверки SQL всех сохраняемых
		// моделей.

		// err передаёт результат чтения журнала файлов и служебных событий тестовой сессии.
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
	// cleanup задаёт сохраняемое намерение очистки для проверки SQL этой модели.
	cleanup := core.CleanupIntent{
		ID: "mapping-cleanup", Revision: 1, CreatedAt: session.CreatedAt, UpdatedAt: session.CreatedAt,
		Kind: core.CleanupRuntime, StreamID: f.stream.ID, SessionID: session.ID,
		Incarnation: session.Incarnation, Lifecycle: session.Lifecycle,
	}
	// err сообщает об ошибке добавления намерения очистки для проверки отображения CleanupIntent в
	// SQL.
	if err := f.store.Write(ctx, func(r core.Repositories) error {
		// Сохраняет намерение очистки исполнения для включения CleanupIntent в проверку SQL.
		return r.Artifacts.InsertCleanup(ctx, cleanup)
	}); err != nil {
		t.Fatal(err)
	}

	// captured собирает только фактически выполненные помеченные запросы записи.
	var captured []mappingSQLStatement
	// capture фиксирует SQL и точную транзакцию только запросов с тестовой меткой контекста.
	capture := func(db *gorm.DB) {
		// Сохраняет SQL, упорядоченные параметры и точный пул только запросов с тестовым значением контекста,
		// проверяя наличие разобранной GORM-модели.

		if db.Statement.Context.Value(mappingSQLContextKey{}) != true {
			return
		}
		captured = append(captured, mappingSQLStatement{
			sql: db.Statement.SQL.String(), vars: append([]any(nil), db.Statement.Vars...),
			pool: db.Statement.ConnPool, compiled: db.Statement.Schema != nil,
		})
	}
	// err сообщает об ошибке установки callback захвата INSERT метаданных.
	if err := f.store.db.Callback().Create().After("gorm:create").Register("test:capture_metadata_create", capture); err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке установки callback захвата UPDATE метаданных.
	if err := f.store.db.Callback().Update().After("gorm:update").Register("test:capture_metadata_update", capture); err != nil {
		t.Fatal(err)
	}
	// actualContext отмечает проверяемые запросы записи для callback захвата.
	actualContext := context.WithValue(ctx, mappingSQLContextKey{}, true)
	// rollback задаёт контрольный отказ для отката после захвата SQL обновления.
	rollback := errors.New("откат проверки SQL")
	// tc содержит очередную таблицу и исходное доменное значение проверяемой модели.
	for _, tc := range []struct {
		// table задаёт внутреннюю SQL-таблицу проверяемой доменной модели.
		table string
		// value содержит исходное доменное значение проверяемой модели.
		value any
	}{
		{"streams", f.stream}, {"profiles", f.profile}, {"groups", f.group},
		{"destinations", f.destinations[0]}, {"fallback_assets", f.asset},
		{"fallback_generations", generation}, {"owned_paths", files[0]},
		{"fallback_variants", f.variant}, {"sessions", session}, {"outputs", output},
		{"command_receipts", receipt}, {"cleanup_intents", cleanup}, {"events", events[0]},
	} {
		t.Run(tc.table, func(t *testing.T) {
			// Сравнивает SQL вставки и обновления выбранной модели с запросом через map и проверяет использование
			// точной транзакции без RETURNING.

			// inserted содержит копию вставляемой модели с граничными uint64 и нулевыми
			// признаками.
			inserted := mappingInsertBoundaryValue(tc.value)
			// normalized содержит проверенную модель с нормализованными временами хранения.
			// err сообщает об ошибке нормализации проверяемой модели.
			normalized, err := normalize(inserted)
			if err != nil {
				t.Fatal(err)
			}
			// values содержит точные SQL-поля проверяемой модели.
			values, err := fields(normalized)
			if err != nil {
				t.Fatal(err)
			}
			// Прежний запрос с map задаёт отношение эквивалентности SQL и параметров.
			// want содержит эталонный запрос GORM через map для сравнения SQL и порядка
			// параметров.
			want := f.store.db.Session(&gorm.Session{DryRun: true}).Table(tc.table).Create(values)
			if want.Error != nil {
				t.Fatal(want.Error)
			}
			captured = nil
			// expectedPool сохраняет точную SQL-транзакцию callback для сравнения с
			// выполненным запросом.
			var expectedPool gorm.ConnPool
			err = f.store.Write(actualContext, func(r core.Repositories) error {
				// Пробует вставить существующий ID выбранной модели в текущей транзакции, ожидая конфликт и фиксируя
				// ConnPool.

				// repo предоставляет реальные репозитории текущей транзакции для вызова
				// проверяемой записи.
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

			// old сохраняет исходную Record перед проверяемым условным обновлением.
			old := recordOf(tc.value)
			// updated содержит отражённую копию доменной модели с увеличенной ревизией и
			// временем.
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
				// Обновляет выбранную модель в текущей транзакции и возвращает контрольную ошибку для отката после
				// фиксации SQL.

				// repo предоставляет реальные репозитории текущей транзакции для вызова
				// проверяемой записи.
				repo := r.Config.(*repositories)
				expectedPool = repo.db.Statement.ConnPool
				// err сообщает об ошибке условного обновления модели в точной
				// транзакции.
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

// Подготавливает граничные uint64 и нулевые признаки разных сущностей для сравнения SQL вставки без
// изменения исходного значения.
func mappingInsertBoundaryValue(value any) any {
	// v содержит копию конкретной доменной модели для назначения граничных полей.
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

// Проверяет единственный записывающий SQL, разбор модели, точный sql.Tx и совпадение параметров с
// эталонным запросом.
func mappingRequireStatement(t *testing.T, got []mappingSQLStatement, want *gorm.DB, pool gorm.ConnPool) {
	t.Helper()
	if len(got) != 1 {
		t.Fatalf("выполнено записывающих SQL-запросов: %d, нужен один", len(got))
	}
	if !got[0].compiled {
		t.Fatal("GORM не разобрал модель перед записью метаданных")
	}
	// ok показывает, является ли ожидаемый ConnPool настоящей SQL-транзакцией.
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
