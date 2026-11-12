package storage

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// Проверяет на SQLite сверку после отмены уже завершённой записи: посторонний COMMIT и откат сохраняют
// допуски, а смена ExecutionFence отзывает их без повторной записи и утечки соединений.
func TestReconcileSQLitePostCommitCancellationPreservesUnchangedExecution(t *testing.T) {
	// owned выбирает обычную запись либо запись от имени наблюдателя.
	for _, owned := range []bool{false, true} {
		// outcome выбирает посторонний COMMIT, смену fence либо подтверждённый откат.
		for _, outcome := range []string{"unrelated_commit", "changed_fence", "rollback"} {
			t.Run(map[bool]string{false: "write", true: "owned"}[owned]+"/"+outcome, func(t *testing.T) {
				// Проверяет реальный COMMIT либо откат с последующей отменой контекста для обычной и принадлежащей
				// наблюдателю записи, сохраняя только подтверждённые допуски.

				// f объединяет временный Store, Service, поток и опубликованную заглушку
				// проверяемого сценария.
				f := newDomainFixture(t, 1)
				// parent содержит точную попытку родительской сессии. outputs содержит
				// выданные попытки выходов исходного запуска.
				parent, outputs := startObservationOutputs(t, f)
				// owner содержит наблюдателя первого выхода для проверки допуска
				// принадлежащей записи.
				owner := openOutputObserver(t, f.service, outputs[0])
				// err сообщает об ошибке записи наблюдаемого состояния точного выхода.
				if err := owner.Observe(context.Background(), core.OutputObservation{State: core.OutputPublishing}); err != nil {
					t.Fatal(err)
				}
				// publication содержит действующий допуск OBS перед записью и отменой.
				publication := activateRuntimeSource(t, f)
				// before сохраняет строки управляющих таблиц перед записью.
				before := observationRows(t, f.store.sqlDB)
				// beforeSession сохраняет полную сессию перед записью.
				beforeSession := readSession(t, f.store, parent.SessionID)
				// beforeRAM сохраняет RAM-снимок действующего исполнения перед записью.
				// err сообщает об ошибке чтения текущего согласованного снимка RAM.
				beforeRAM, err := f.service.RuntimeSnapshot(context.Background(), observationFence(parent))
				if err != nil {
					t.Fatal(err)
				}
				// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
				// cancel отменяет вызывающий контекст только после завершения COMMIT
				// либо отката.
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				// failure задаёт контрольную ошибку подтверждённого отката.
				failure := errors.New("подтверждённый откат")
				// writeCalls считает вызовы функции записи, переданной GuardWrite.
				// callbackCalls считает исполнения тела транзакции.
				writeCalls, callbackCalls := 0, 0
				// committed отмечает успешный COMMIT до отмены вызывающего контекста.
				committed := false
				// write выполняет одну реальную запись и отменяет контекст после её
				// завершения.
				write := func() (bool, error) {
					// Выполняет одну реальную запись с бюджетом, фиксирует подтверждение COMMIT и лишь после завершения
					// транзакции отменяет вызывающий контекст.

					writeCalls++
					// uncertain показывает необходимость сверки RuntimeAuthority
					// после попытки транзакции. writeErr содержит результат
					// транзакционной записи до отмены контекста.
					uncertain, writeErr := f.store.writeWithBudget(ctx, func(r core.Repositories) error {
						// Учитывает вызов и изменяет Lifecycle Session либо конфигурацию профиля; для сценария отката
						// возвращает контрольную ошибку.

						callbackCalls++
						if outcome == "changed_fence" {
							// session содержит сессию, чей Lifecycle изменяется для
							// отзыва допусков. err сообщает об ошибке чтения
							// сохраняемой сессии.
							session, err := r.Runtime.Session(ctx, parent.SessionID)
							if err != nil {
								return err
							}
							// old сохраняет исходную ревизию изменяемой записи для
							// CAS.
							old := session.Revision
							session.Lifecycle++
							session.Revision++
							return r.Runtime.UpdateSession(ctx, session, old)
						}
						// profile содержит профиль, изменение которого не должно
						// отзывать активное исполнение. err сообщает об ошибке
						// чтения сохранённого полного профиля.
						profile, err := r.Config.Profile(ctx, f.profile.ID)
						if err != nil {
							return err
						}
						// old сохраняет исходную ревизию изменяемой записи для
						// CAS.
						old := profile.Revision
						profile.Name = "Изменённая конфигурация следующего запуска"
						profile.Revision++
						profile.UpdatedAt = time.Now().UTC().Truncate(time.Microsecond)
						// err сообщает об ошибке условного изменения полного
						// профиля.
						if err := r.Config.UpdateProfile(ctx, profile, old); err != nil {
							return err
						}
						if outcome == "rollback" {
							return failure
						}
						return nil
					})
					committed = uncertain && writeErr == nil
					// Отмена наступает только после завершения настоящего COMMIT или отката.
					cancel()
					return uncertain, writeErr
				}
				if owned {
					err = f.store.authority.GuardOwnedWrite(ctx, f.store, owner, f.store.options.RetryTimeout, write)
				} else {
					err = f.store.authority.GuardWrite(ctx, f.store, f.store.options.RetryTimeout, write)
				}
				if outcome == "rollback" {
					if !errors.Is(err, failure) || committed {
						t.Fatalf("rollback выдан за COMMIT: committed=%t err=%v", committed, err)
					}
				} else if err != nil || !committed {
					t.Fatalf("COMMIT не подтверждён до отмены: committed=%t err=%v", committed, err)
				}
				if writeCalls != 1 || callbackCalls != 1 || !errors.Is(ctx.Err(), context.Canceled) {
					t.Fatalf("сверка повторила запись либо потеряла отмену: write=%d callback=%d ctx=%v", writeCalls, callbackCalls, ctx.Err())
				}
				// after содержит строки управляющих таблиц после записи и сверки.
				after := observationRows(t, f.store.sqlDB)
				// table называет управляющую таблицу, которая не должна измениться при
				// сверке.
				for _, table := range []string{"command_receipts", "cleanup_intents", "session_holds", "output_holds", "outputs", "events"} {
					if !reflect.DeepEqual(before[table], after[table]) {
						t.Errorf("сверка изменила %s", table)
					}
				}
				if outcome == "changed_fence" {
					// got содержит сохраняемую сессию после записи и сверки.
					if got := readSession(t, f.store, parent.SessionID); got.Lifecycle != beforeSession.Lifecycle+1 {
						t.Fatal("смена fence не сохранена в SQLite")
					}
					requireRevokedLease(t, publication)
					if owner.Valid() {
						t.Fatal("смена долговечного fence сохранила наблюдателя")
					}
				} else {
					// got содержит сохраняемую сессию после записи и сверки.
					if got := readSession(t, f.store, parent.SessionID); !reflect.DeepEqual(beforeSession, got) {
						t.Fatal("посторонняя запись изменила действующую Session")
					}
					if !publication.Valid() || !owner.Valid() {
						t.Fatal("отмена завершённой записи отозвала неизменённое выполнение")
					}
					// afterRAM содержит RAM-снимок после сверки неизменённого
					// исполнения. err сообщает об ошибке чтения текущего
					// согласованного снимка RAM.
					afterRAM, err := f.service.RuntimeSnapshot(context.Background(), observationFence(parent))
					if err != nil || !reflect.DeepEqual(beforeRAM, afterRAM) {
						t.Fatalf("неизменённое выполнение потеряло RAM: %v", err)
					}
					// acted отмечает фактическое действие сохранившего допуск
					// источника OBS.
					acted := false
					// err сообщает об ошибке выполнения действия допуска после
					// сверки.
					if err := publication.Do(context.Background(), func() error {
						// Отмечает успешное действие неизменённой публикации после послезаписной сверки.
						acted = true
						return nil
					}); err != nil || !acted {
						t.Fatal("проверенный источник потерял допуск действий", err)
					}
					if outcome == "rollback" {
						if !reflect.DeepEqual(before, after) {
							t.Fatal("подтверждённый rollback изменил долговечные строки")
						}
					} else {
						if reflect.DeepEqual(before["profiles"], after["profiles"]) {
							t.Fatal("посторонняя запись не сохранена")
						}
						// err сообщает об ошибке чтения сохранённого полного
						// профиля.
						if err := f.store.View(context.Background(), func(r core.Repositories) error {
							// Проверяет сохранённые имя и Revision профиля после посторонней записи, не меняющей активную Session.

							// profile содержит профиль, изменение которого не должно
							// отзывать активное исполнение. err сообщает об ошибке
							// чтения сохранённого полного профиля.
							profile, err := r.Config.Profile(context.Background(), f.profile.ID)
							if err == nil && (profile.Name != "Изменённая конфигурация следующего запуска" || profile.Revision != f.profile.Revision+1) {
								t.Error("SQLite не сохранила точную конфигурацию и CAS")
							}
							return err
						}); err != nil {
							t.Fatal(err)
						}
					}
				}
				// inUse содержит число занятых соединений после записи и сверки.
				if inUse := f.store.sqlDB.Stats().InUse; inUse != 0 {
					t.Fatalf("запись или сверка удерживает соединения: %d", inUse)
				}
				// err сообщает об ошибке закрытия временного хранилища.
				if err := f.store.Close(); err != nil {
					t.Fatal(err)
				}
				if publication.Valid() || owner.Valid() {
					t.Fatal("Close сохранил допуск после сверки")
				}
			})
		}
	}
}
