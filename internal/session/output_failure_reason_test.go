package session

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"reflect"
	"strings"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
)

// outputReasonStore скрывает только подтверждение уже выполненной попытки Complete.
// Все изменения и откаты выполняются настоящим Store; поля используются последовательно.
type outputReasonStore struct {
	// Store сохраняет транзакции и общую RuntimeAuthority исходной файловой базы.
	*storage.Store
	// hideReads запрещает чтение после отказа записи или потерянного подтверждения commit.
	hideReads bool
	// observed отмечает наступление проверяемой границы, оставляя начальное чтение доступным.
	observed bool
	// ambiguousCommit возвращает ошибку после настоящего успешного commit ровно один раз.
	ambiguousCommit bool
}

// Write вызывает callback на настоящих репозиториях и отмечает границу потери подтверждения.
// ctx ограничивает запись; callback сохраняет обычную атомарность Store.
func (s *outputReasonStore) Write(ctx context.Context, callback func(core.Repositories) error) error {
	// err содержит реальный отказ, включая поздний откат события SQL-триггером.
	if err := s.Store.Write(ctx, callback); err != nil {
		s.observed = true
		return err
	}
	if s.ambiguousCommit {
		s.ambiguousCommit = false
		s.observed = true
		return core.ErrBusy
	}
	return nil
}

// View вызывает callback через согласованное чтение SQLite до проверяемой границы записи.
// После неё hideReads изображает неизвестный исход, не меняя настоящий результат транзакции.
func (s *outputReasonStore) View(ctx context.Context, callback func(core.Repositories) error) error {
	if s.hideReads && s.observed {
		return core.ErrBusy
	}
	return s.Store.View(ctx, callback)
}

// outputReasonAttempt создаёт RUNNING-родителя и принятую попытку AddOutput без медиазапуска.
// t получает ошибки фикстуры; issued выбирает CLAIMED либо ISSUED перед проверкой отказа,
// restart заменяет первоначальный AddOutput на реальный StartOutput после остановки и очистки.
// Возвращённый Manager не запускает диспетчер, а SQLite остаётся под владельцем фикстуры.
func outputReasonAttempt(t *testing.T, issued, restart bool) (managerFixture, core.CommandReceipt, core.ExecutionFence, *Manager) {
	t.Helper()
	// fixture предоставляет настоящую SQLite с Goose и опубликованным вариантом заглушки.
	fixture := newManagerFixture(t, false, nil)
	// err подтверждает присоединение исходного диспетчера до ручных операций исполнения.
	if err := fixture.manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	// ctx задаёт последовательные операции изолированной базы без внешнего исполнения.
	ctx := context.Background()
	// parent и err содержат принятую команду запуска с пустым набором выходов.
	parent, err := fixture.service.StartSession(ctx, core.StartSessionRequest{RequestID: "output-reason-parent", StreamID: fixture.stream.ID})
	if err != nil {
		t.Fatal(err)
	}
	// parentFence адресует единственную настоящую попытку запуска родителя.
	parentFence, err := fixture.service.Claim(ctx, parent.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if err = fixture.service.MarkIssued(ctx, parentFence); err != nil {
		t.Fatal(err)
	}
	if err = fixture.service.Complete(ctx, parentFence, core.Completion{Success: true}); err != nil {
		t.Fatal(err)
	}
	// session проверяет обязательное состояние родителя до принятия отдельного выхода.
	session, err := fixture.service.GetSession(ctx, parent.SessionID)
	if err != nil || session.State != core.SessionRunning {
		t.Fatal("родитель не перешёл в RUNNING", session, err)
	}
	// accepted содержит реальную принятую команду независимого выхода со снимком назначения.
	accepted, err := fixture.service.AddOutput(ctx, core.AddOutputRequest{RequestID: "output-reason-add", SessionID: parent.SessionID, DestinationID: fixture.destination.ID, Binding: fixture.binding})
	if err != nil {
		t.Fatal(err)
	}
	if restart {
		if _, err = fixture.service.StopOutput(ctx, core.OutputCommand{RequestID: "output-reason-initial-stop", OutputID: accepted.OutputID}); err != nil {
			t.Fatal(err)
		}
		// cleanups содержит единственный предварительный ресурс, который здесь физически не создавался;
		// readErr сообщает об отказе чтения обязательств очистки.
		cleanups, readErr := fixture.service.ListCleanups(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8, SessionID: accepted.SessionID, PendingOnly: true}})
		if readErr != nil || len(cleanups.Items) != 1 || cleanups.Items[0].OutputID != accepted.OutputID {
			t.Fatal("не найдено обязательство очистки перед повторным запуском", cleanups, readErr)
		}
		if err = fixture.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(cleanups.Items[0])); err != nil {
			t.Fatal(err)
		}
		accepted, err = fixture.service.StartOutput(ctx, core.OutputCommand{RequestID: "output-reason-restart", OutputID: accepted.OutputID})
		if err != nil {
			t.Fatal(err)
		}
	}
	// fence идентифицирует фактическую попытку нового выхода, без создания Publisher.
	fence, err := fixture.service.Claim(ctx, accepted.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if issued {
		if err = fixture.service.MarkIssued(ctx, fence); err != nil {
			t.Fatal(err)
		}
	}
	// manager использует прежние сроки и logger, но не создаёт работников или медиаресурсы.
	manager := &Manager{config: fixture.config, sessions: make(map[core.ID]*sessionNode), wake: make(chan struct{}, 1)}
	return fixture, accepted, fence, manager
}

// outputReasonSQL открывает отдельное соединение только к принадлежащей тесту SQLite.
// t закрывает пул перед владельцем фикстуры; path задаёт уже созданный файл без миграций.
func outputReasonSQL(t *testing.T, path string) *sql.DB {
	t.Helper()
	// dsn разрешает лишь существующую локальную базу и сохраняет включённые внешние ключи.
	dsn := url.URL{Scheme: "file", Path: path, RawQuery: "mode=rw&_foreign_keys=on&_busy_timeout=1000"}
	// database и err содержат отдельный пул для инъекции SQL-триггера во временную базу.
	database, err := sql.Open("sqlite3", dsn.String())
	if err != nil {
		t.Fatal(err)
	}
	database.SetMaxOpenConns(1)
	// Завершитель закрывает только дополнительный пул этого теста; err сохраняет отказ закрытия.
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Error(err)
		}
	})
	return database
}

// TestOutputFailureReasonSurvivesLateCompletionRollback проверяет сохранение исходной причины
// после позднего отката SQLite и неизвестного результата commit. Обработчик каждого подтеста создаёт
// отдельный RUNNING-запуск и выход; продолжение выполняется существующим retryRetirements.
func TestOutputFailureReasonSurvivesLateCompletionRollback(t *testing.T) {
	// scenario задаёт фазу исходной попытки и доступность подтверждения результата.
	for _, scenario := range []struct {
		// name называет независимый класс отказа; issued выбирает выданную либо захваченную попытку.
		name   string
		issued bool
		// unreadable скрывает результат после записи; committed сохраняет его до искусственной ошибки.
		unreadable bool
		committed  bool
		// restart проверяет ту же гарантию для StartOutput по сохранённому снимку.
		restart bool
	}{
		{"claimed rollback", false, false, false, false},
		{"issued rollback", true, false, false, false},
		{"unknown rollback", true, true, false, false},
		{"confirmed commit", true, false, true, false},
		{"unknown commit", true, true, true, false},
		{"restart claimed rollback", false, false, false, true},
		{"restart issued rollback", true, false, false, true},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			// fixture, accepted, fence и manager задают RUNNING-родителя и отдельную принятую попытку.
			fixture, accepted, fence, manager := outputReasonAttempt(t, scenario.issued, scenario.restart)
			// ctx задаёт последовательные операции SQLite без внешнего запуска.
			ctx := context.Background()
			// database позволяет отказать после обновления Session/Output/Receipt на последнем событии.
			database := outputReasonSQL(t, fixture.databasePath)
			// before фиксирует все затрагиваемые строки управления, удержаний, событий и очистки.
			before := outputReasonRows(t, database)
			// originalOutput содержит самостоятельный снимок до попытки записи результата;
			// err сообщает об отказе исходного чтения.
			originalOutput, err := fixture.service.GetOutput(ctx, accepted.OutputID)
			if err != nil {
				t.Fatal(err)
			}
			// fault сохраняет реальный Store, скрывая только исход после выполнения транзакции.
			fault := &outputReasonStore{Store: fixture.store, hideReads: scenario.unreadable, ambiguousCommit: scenario.committed}
			// service и err содержат сервис той же incarnation через управляемую доступность чтения.
			service, err := core.NewService(fault, fixture.store.Incarnation())
			if err != nil {
				t.Fatal(err)
			}
			manager.config.Core = service
			// reason и cause содержат доказанный шаг запуска и полную синтетическую диагностику.
			reason := core.FailureReason{Code: core.FailureOutputStartFailed, Context: core.FailureContext{SessionID: accepted.SessionID, OutputID: accepted.OutputID, CommandID: accepted.ID}}
			cause := core.WithFailureReason(errors.New("synthetic-output-prepare-diagnostic"), reason)
			// node хранит исходную причину и пустой callback cancel: медиаресурсы не создавались.
			node := &outputNode{startReceipt: accepted, fence: fence, failure: cause, cancel: func() {}, done: make(chan struct{})}
			if !scenario.committed {
				// Триггер отвергает событие после FAILED того же выхода, допуская ошибочную
				// компенсацию с INTERRUPTED, чтобы такая потеря причины была видна проверке.
				if _, err = database.ExecContext(ctx, `CREATE TRIGGER reject_output_completion BEFORE INSERT ON events
					WHEN EXISTS (SELECT 1 FROM command_receipts WHERE output_id = NEW.output_id AND state = 'FAILED')
					BEGIN SELECT RAISE(ABORT, 'synthetic late output completion rollback'); END`); err != nil {
					t.Fatal(err)
				}
			}
			err = node.cleanup(ctx, manager)
			if scenario.committed && !scenario.unreadable {
				if err != nil || !node.joined {
					t.Fatal("чтение не подтвердило настоящий commit", err, node.joined)
				}
			} else if err == nil || node.joined || node.failure != cause {
				t.Fatal("неподтверждённый результат потерял продолжение", err, node.joined)
			}
			if !scenario.committed {
				if !strings.Contains(err.Error(), "synthetic late output completion rollback") || !reflect.DeepEqual(before, outputReasonRows(t, database)) {
					t.Fatal("поздний откат оставил изменения либо не достиг события", err)
				}
				if _, err = database.ExecContext(ctx, "DROP TRIGGER reject_output_completion"); err != nil {
					t.Fatal(err)
				}
			}
			fault.hideReads = false
			close(node.done)
			// parent удерживает вернувшийся выход для существующего прохода повторной очистки.
			parent := &sessionNode{outputs: map[core.ID]*outputNode{accepted.OutputID: node}}
			manager.sessions[accepted.SessionID] = parent
			manager.retryRetirements(ctx)
			// completed проверяет исходную причину и самостоятельную идентичность принятой команды.
			completed, err := fixture.service.GetReceipt(ctx, accepted.ID)
			if err != nil || completed.Status != core.CommandFailed || completed.Failure != cause.Error() || !reflect.DeepEqual(completed.FailureReason, &reason) || !sameCommand(completed, accepted) || !reflect.DeepEqual(completed.Identity, accepted.Identity) || completed.Fingerprint != accepted.Fingerprint || completed.Attempt != fence.Attempt || !node.joined {
				t.Fatal("повтор очистки потерял первоначальный отказ выхода", completed, err, node.joined)
			}
			// settled проверяет итог отзыва и сохранение самостоятельного снимка настроек выхода.
			settled, err := fixture.service.GetOutput(ctx, accepted.OutputID)
			if err != nil || settled.DesiredRunning || settled.Generation <= fence.OutputGeneration || !settled.CleanupPending || !reflect.DeepEqual(settled.Snapshot, originalOutput.Snapshot) {
				t.Fatal("отказ выхода не сохранил отзыв и обязательство очистки", settled, err)
			}
			// stable фиксирует завершённый результат перед идемпотентным повтором cleanup.
			stable := outputReasonRows(t, database)
			manager.retryRetirements(ctx)
			if !reflect.DeepEqual(stable, outputReasonRows(t, database)) {
				t.Fatal("завершённый повтор изменил результат или создал новую команду")
			}
		})
	}
}

// outputReasonRows читает полные payload и составные удержания из всех затрагиваемых таблиц.
// t сообщает ошибки SQL; database принадлежит только текущему последовательному тесту.
// Отсортированные строки позволяют заметить частичную запись, лишнюю команду, очистку или событие.
func outputReasonRows(t *testing.T, database *sql.DB) []string {
	t.Helper()
	// result содержит независимое представление долговечного контроля без локального состояния узла.
	var result []string
	// rows и err содержат полный набор строк, который закрывается до следующей операции записи.
	rows, err := database.Query(`SELECT 'session:' || id || ':' || hex(payload) FROM sessions
		UNION ALL SELECT 'output:' || id || ':' || hex(payload) FROM outputs
		UNION ALL SELECT 'receipt:' || id || ':' || hex(payload) FROM command_receipts
		UNION ALL SELECT 'cleanup:' || id || ':' || hex(payload) FROM cleanup_intents
		UNION ALL SELECT 'event:' || id || ':' || hex(payload) FROM events
		UNION ALL SELECT 'session_hold:' || session_id || ':' || stream_id || ':' || asset_id || ':' || generation_id FROM session_holds
		UNION ALL SELECT 'output_hold:' || output_id || ':' || session_id || ':' || stream_id || ':' || group_id || ':' || profile_id || ':' || destination_id || ':' || asset_id || ':' || variant_id || ':' || generation_id FROM output_holds
		ORDER BY 1`)
	if err != nil {
		t.Fatal(err)
	}
	// Отложенный обработчик закрывает строки при раннем выходе; err сообщает об отказе закрытия.
	defer func() {
		if err := rows.Close(); err != nil {
			t.Error(err)
		}
	}()
	for rows.Next() {
		// row сохраняет очередную полную строку, включая идентичность таблицы и записи.
		var row string
		if err = rows.Scan(&row); err != nil {
			t.Fatal(err)
		}
		result = append(result, row)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err = rows.Close(); err != nil {
		t.Fatal(err)
	}
	return result
}

// TestOutputFailureReasonPreservesAcceptedControl проверяет приоритет принятого успеха,
// пользовательской остановки и новой выданной попытки над поздним отказом прежнего работника.
// Обработчик каждого подтеста выполняет управление на отдельной настоящей SQLite без Publisher.
func TestOutputFailureReasonPreservesAcceptedControl(t *testing.T) {
	// scenario выбирает результат или управление, сохранённое до позднего callback старого выхода.
	for _, scenario := range []string{"success", "output stop", "session stop", "new output generation", "different attempt"} {
		t.Run(scenario, func(t *testing.T) {
			// fixture, accepted, fence и manager задают RUNNING Session и реально выданный AddOutput.
			fixture, accepted, fence, manager := outputReasonAttempt(t, true, false)
			// ctx задаёт последовательные записи приоритетного управления.
			ctx := context.Background()
			// database позволяет доказать отсутствие любых изменений после позднего callback.
			database := outputReasonSQL(t, fixture.databasePath)
			// err сохраняет результаты управления и последующей проверки точной попытки.
			var err error
			switch scenario {
			case "success":
				err = fixture.service.Complete(ctx, fence, core.Completion{Success: true})
			case "output stop", "new output generation":
				_, err = fixture.service.StopOutput(ctx, core.OutputCommand{RequestID: "output-reason-user-stop", OutputID: accepted.OutputID})
			case "session stop":
				_, err = fixture.service.StopSession(ctx, core.SessionCommand{RequestID: "output-reason-parent-stop", SessionID: accepted.SessionID})
			}
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "new output generation" {
				// cleanups содержит обязательства выданной попытки без физических ресурсов;
				// readErr сообщает об отказе чтения обязательств очистки.
				cleanups, readErr := fixture.service.ListCleanups(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8, SessionID: accepted.SessionID, PendingOnly: true}})
				if readErr != nil || len(cleanups.Items) != 1 || cleanups.Items[0].OutputID != accepted.OutputID {
					t.Fatal("не найдено точное обязательство очистки прежнего выхода", cleanups, readErr)
				}
				if err = fixture.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(cleanups.Items[0])); err != nil {
					t.Fatal(err)
				}
				// next содержит принятую команду повторного запуска прежнего снимка;
				// startErr сообщает об отказе принятия этого запуска.
				next, startErr := fixture.service.StartOutput(ctx, core.OutputCommand{RequestID: "output-reason-new-start", OutputID: accepted.OutputID})
				if startErr != nil {
					t.Fatal(startErr)
				}
				// nextFence фиксирует следующую попытку, которую нельзя отменять;
				// claimErr сообщает об отказе захвата нового запуска.
				nextFence, claimErr := fixture.service.Claim(ctx, next.RequestID)
				if claimErr != nil || nextFence.OutputGeneration <= fence.OutputGeneration || nextFence.Attempt <= fence.Attempt {
					t.Fatal("новое выполнение не получило отдельную точную попытку", nextFence, claimErr)
				}
				if err = fixture.service.MarkIssued(ctx, nextFence); err != nil {
					t.Fatal(err)
				}
			}
			// before и previous фиксируют весь контроль и исходную квитанцию после принятого управления.
			before := outputReasonRows(t, database)
			previous, err := fixture.service.GetReceipt(ctx, accepted.ID)
			if err != nil {
				t.Fatal(err)
			}
			if scenario == "different attempt" {
				fence.Attempt++
			}
			// cause содержит поздний отказ, который не должен заменить уже сохранённый результат.
			cause := core.WithFailureReason(errors.New("synthetic-late-output-diagnostic"), core.FailureReason{Code: core.FailureOutputStartFailed})
			// node не содержит физических ресурсов; пустой callback cancel лишь позволяет пройти cleanup.
			node := &outputNode{startReceipt: accepted, fence: fence, failure: cause, cancel: func() {}}
			err = node.cleanup(ctx, manager)
			if scenario == "different attempt" {
				if !errors.Is(err, core.ErrStale) || node.joined {
					t.Fatal("чужая попытка получила подтверждение очистки", err, node.joined)
				}
			} else if err != nil || !node.joined {
				t.Fatal("подтверждённое управление не позволило завершить старый узел", err, node.joined)
			}
			// preserved проверяет неизменность исходного результата независимо от новой команды компенсации.
			preserved, err := fixture.service.GetReceipt(ctx, accepted.ID)
			if err != nil || !reflect.DeepEqual(previous, preserved) {
				t.Fatal("поздний callback переписал исходную квитанцию", preserved, err)
			}
			if scenario == "success" {
				// output подтверждает отдельную компенсацию позднего отказа после сохранённого успеха;
				// readErr сообщает об отказе чтения компенсированного выхода.
				output, readErr := fixture.service.GetOutput(ctx, accepted.OutputID)
				if readErr != nil || output.DesiredRunning || !output.CleanupPending || preserved.Status != core.CommandSucceeded {
					t.Fatal("поздний отказ не компенсирован отдельно от успеха", output, preserved, readErr)
				}
			} else if !reflect.DeepEqual(before, outputReasonRows(t, database)) {
				t.Fatal("поздний callback изменил принятое управление или новое выполнение")
			}
		})
	}
}
