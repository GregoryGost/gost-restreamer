package storage

import (
	"context"
	"database/sql"
	"errors"
	"reflect"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// preservedSQLRows фиксирует все столбцы строк для проверки атомарности и ресурсов.
type preservedSQLRows map[string][][]any

// captureSQLRows читает полный состав фиксированных таблиц db в стабильном порядке.
// t завершает тест при любой ошибке чтения или закрытия курсора.
func captureSQLRows(t *testing.T, db *sql.DB) preservedSQLRows {
	t.Helper()
	// snapshot хранит полные строки, включая SQL-проекции и точные BLOB.
	snapshot := preservedSQLRows{}
	// table выбирается только из фиксированного перечня таблиц приложения.
	for _, table := range []string{"storage_format", "goose_db_version", "streams", "profiles", "groups", "destinations", "fallback_assets", "fallback_generations", "fallback_variants", "sessions", "outputs", "session_holds", "output_holds", "command_receipts", "events", "owned_paths", "cleanup_intents"} {
		// rows предоставляет все столбцы таблицы без потери двоичных данных; err сообщает об ошибке SQL.
		rows, err := db.Query("SELECT * FROM " + table + " ORDER BY 1")
		if err != nil {
			t.Fatal(err)
		}
		// columns задаёт размер списка Scan, не вводя предположений о числе столбцов.
		columns, err := rows.Columns()
		if err != nil {
			t.Fatal(errors.Join(err, rows.Close()))
		}
		for rows.Next() {
			// row хранит одну полную строку; targets предоставляет адреса ячеек для database/sql.
			row := make([]any, len(columns))
			targets := make([]any, len(columns))
			// index адресует каждый столбец текущей строки.
			for index := range row {
				targets[index] = &row[index]
			}
			if err = rows.Scan(targets...); err != nil {
				t.Fatal(errors.Join(err, rows.Close()))
			}
			snapshot[table] = append(snapshot[table], row)
		}
		if err = rows.Err(); err != nil {
			t.Fatal(errors.Join(err, rows.Close()))
		}
		if err = rows.Close(); err != nil {
			t.Fatal(err)
		}
	}
	return snapshot
}

// selectionFaultWork сохраняет настоящие транзакции и подменяет только отказ чтения варианта.
type selectionFaultWork struct {
	// UnitOfWork предоставляет исходное изолированное хранилище и неизменённую Write.
	core.UnitOfWork
	// failure задаёт точный возвращаемый отказ репозитория.
	failure error
}

// View оборачивает только Artifacts в согласованной транзакции ctx и вызывает fn без иных изменений.
func (w selectionFaultWork) View(ctx context.Context, fn func(core.Repositories) error) error {
	return w.UnitOfWork.View(ctx, func(repositories core.Repositories) error {
		// repositories сохраняет настоящие Config/Runtime; callback заменяет единственную границу отказа.
		repositories.Artifacts = selectionFaultArtifacts{repositories.Artifacts, w.failure}
		return fn(repositories)
	})
}

// selectionFaultArtifacts сохраняет настоящий каталог поколений, изменяя только чтение Variant.
type selectionFaultArtifacts struct {
	// ArtifactRepository обслуживает все операции, кроме выбранного чтения файла.
	core.ArtifactRepository
	// failure содержит ошибку, которую нельзя потерять при переборе canonical-совпадений.
	failure error
}

// Variant возвращает заданный отказ без файлового ввода-вывода; ctx и id определяют обычное чтение.
func (a selectionFaultArtifacts) Variant(ctx context.Context, id core.ID) (core.FallbackVariant, error) {
	return core.FallbackVariant{}, a.failure
}

// TestBindingSelectionPreservesJoinedStorageFailures проверяет, что предметная неготовность
// не скрывает одновременно возвращённую отмену или повреждение. t использует настоящие остальные репозитории.
func TestBindingSelectionPreservesJoinedStorageFailures(t *testing.T) {
	// fixture содержит один пригодный вариант, позволяющий дойти до проверяемого чтения.
	fixture := newDomainFixture(t, 1)
	// serious перечисляет независимые причины, которые обязаны дойти до вызывающего кода.
	for _, serious := range []error{context.Canceled, core.ErrCorrupt} {
		// callback проверяет одну составную ошибку на неизменённой временной базе.
		t.Run(serious.Error(), func(t *testing.T) {
			// failure сохраняет оба признака отказа, не меняя доменные значения фикстуры.
			failure := errors.Join(core.ErrNotReady, serious)
			// service использует настоящую конфигурацию и контролируемое чтение Variant; err сообщает отказ создания.
			service, err := core.NewService(selectionFaultWork{fixture.store, failure}, fixture.store.Incarnation())
			if err != nil {
				t.Fatal(err)
			}
			// bindings и err фиксируют результат публичной операции выбора привязок.
			bindings, err := service.SelectStartBindings(context.Background(), fixture.stream.ID, nil)
			if !errors.Is(err, serious) || len(bindings) != 0 {
				t.Fatalf("Перебор потерял серьёзную причину %v: bindings=%v, error=%v", serious, bindings, err)
			}
		})
	}
}

// TestOutputRestartRejectsInvalidatedSnapshotAtomically проверяет первый и последующий запуск
// прикреплённого выхода после остановки. t сверяет полный откат отказа и пригодность неизменённого файла.
func TestOutputRestartRejectsInvalidatedSnapshotAtomically(t *testing.T) {
	// repeated выбирает снимок до первого внешнего запуска либо после принятого повторного запуска.
	for _, repeated := range []bool{false, true} {
		// callback выполняет сценарий на собственной временной SQLite без физических работников.
		// t сообщает об ошибках проверяемого сценария.
		t.Run(map[bool]string{false: "first", true: "repeated"}[repeated], func(t *testing.T) {
			// fixture предоставляет прикреплённый выход и действующую родительскую Session.
			fixture := newDomainFixture(t, 1)
			// ctx ограничивает доменные операции текущим сценарием.
			ctx := context.Background()
			// start содержит принятую первоначальную команду Session.
			start := startDomainSession(t, fixture)
			executeCommand(t, fixture.service, start.RequestID)
			// output содержит ещё не выданный внешнему работнику снимок назначения.
			output := readOutputs(t, fixture.store, start.SessionID)[0]
			// err сообщает об ошибке остановки первоначального намерения без внешнего процесса.
			if _, err := fixture.service.StopOutput(ctx, core.OutputCommand{RequestID: "stop-initial-intent", OutputID: output.ID}); err != nil {
				t.Fatal(err)
			}
			// cleanup подтверждает предварительные ресурсы выданной Session до нового запуска выхода.
			for _, cleanup := range pendingCleanups(t, fixture.store, start.SessionID) {
				// err сообщает об отказе подтверждения завершённой предварительной очистки.
				if err := fixture.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(cleanup)); err != nil {
					t.Fatal(err)
				}
			}
			if repeated {
				// err сообщает об ошибке допуска пригодного выхода до моделирования прежнего снимка.
				if _, err := fixture.service.StartOutput(ctx, core.OutputCommand{RequestID: "healthy-prior-restart", OutputID: output.ID}); err != nil {
					t.Fatal(err)
				}
				// err сообщает об ошибке остановки этого предварительного повторного запуска.
				if _, err := fixture.service.StopOutput(ctx, core.OutputCommand{RequestID: "stop-prior-restart", OutputID: output.ID}); err != nil {
					t.Fatal(err)
				}
				// cleanup закрывает обязательство предварительной группы повторного намерения.
				for _, cleanup := range pendingCleanups(t, fixture.store, start.SessionID) {
					// err сообщает об отказе подтверждения очистки повторного намерения.
					if err := fixture.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(cleanup)); err != nil {
						t.Fatal(err)
					}
				}
			}
			output = readOutputs(t, fixture.store, start.SessionID)[0]
			// healthyPayload позволяет вернуть неизменённый снимок для положительного контроля;
			// err сообщает об ошибке кодирования его метаданных.
			healthyPayload, err := core.EncodeMetadata(output)
			if err != nil {
				t.Fatal(err)
			}
			output.Snapshot.Artifact.GeneratorVersion = "unsupported-generator"
			// invalidPayload моделирует самостоятельный исторический снимок с несовместимым генератором.
			invalidPayload, err := core.EncodeMetadata(output)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = fixture.store.sqlDB.Exec("UPDATE outputs SET payload=? WHERE id=?", invalidPayload, output.ID); err != nil {
				t.Fatal(err)
			}
			// before фиксирует все таблицы перед отклоняемой командой.
			before := captureSQLRows(t, fixture.store.sqlDB)
			// rejected и err должны подтвердить отказ до квитанции и изменения Generation.
			rejected, err := fixture.service.StartOutput(ctx, core.OutputCommand{RequestID: "invalidated-restart", OutputID: output.ID})
			if !errors.Is(err, core.ErrNotReady) || rejected.ID != "" || !reflect.DeepEqual(before, captureSQLRows(t, fixture.store.sqlDB)) {
				t.Fatal("Непригодный снимок запущен либо отказ оставил изменения", rejected, err)
			}
			if _, err = fixture.store.sqlDB.Exec("UPDATE outputs SET payload=? WHERE id=?", healthyPayload, output.ID); err != nil {
				t.Fatal(err)
			}
			// accepted подтверждает новый допуск только прежнего неизменённого снимка.
			accepted, err := fixture.service.StartOutput(ctx, core.OutputCommand{RequestID: "unchanged-restart", OutputID: output.ID})
			if err != nil || accepted.OutputID != output.ID {
				t.Fatal("Неизменённый снимок потерял пригодность", accepted, err)
			}
		})
	}
}
