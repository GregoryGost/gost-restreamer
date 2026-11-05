package integration

import (
	"context"
	"database/sql"
	"errors"
	"net/url"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/fallback"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
	"github.com/GregoryGost/gost-restreamer/internal/processowner"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
)

// Независимое чтение SQL проверяет точного владельца holds, а не только управляющие флаги.
func sessionAcceptanceAssertHolds(t *testing.T, f *sessionAcceptanceFixture, sessionID core.ID, retained bool) {
	t.Helper()
	dsn := url.URL{Scheme: "file", Path: filepath.Join(f.directory, "state.sqlite"), RawQuery: "mode=ro&_busy_timeout=1000"}
	db, err := sql.Open("sqlite3", dsn.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	db.SetMaxOpenConns(1)
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = tx.Rollback() }()
	var parent core.ReferenceHold
	parent.SessionID = sessionID
	err = tx.QueryRowContext(ctx, "SELECT stream_id, asset_id, generation_id FROM session_holds WHERE session_id = ?", sessionID).Scan(&parent.StreamID, &parent.AssetID, &parent.GenerationID)
	if retained {
		expected := core.ReferenceHold{SessionID: sessionID, StreamID: f.stream.ID, AssetID: f.asset.ID, GenerationID: f.generation.ID}
		if err != nil || parent != expected {
			t.Fatalf("точный parent hold: получено %+v, ожидалось %+v: %v", parent, expected, err)
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("parent hold не освобождён: %+v %v", parent, err)
	}
	rows, err := tx.QueryContext(ctx, "SELECT output_id, stream_id, group_id, profile_id, destination_id, asset_id, variant_id, generation_id FROM output_holds WHERE session_id = ?", sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Error(err)
		}
	}()
	var holds []core.ReferenceHold
	for rows.Next() {
		hold := core.ReferenceHold{SessionID: sessionID}
		if err = rows.Scan(&hold.OutputID, &hold.StreamID, &hold.GroupID, &hold.ProfileID, &hold.DestinationID, &hold.AssetID, &hold.VariantID, &hold.GenerationID); err != nil {
			t.Fatal(err)
		}
		holds = append(holds, hold)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	if err = rows.Close(); err != nil {
		t.Fatal(err)
	}
	if err = tx.Commit(); err != nil {
		t.Fatal(err)
	}
	if !retained {
		if len(holds) != 0 {
			t.Fatalf("прежние output holds не освобождены: %+v", holds)
		}
		return
	}
	outputs := f.outputs(t, sessionID)
	if len(outputs) != 1 || len(holds) != 1 {
		t.Fatalf("ожидалось одно точное удержание Output: outputs=%d holds=%+v", len(outputs), holds)
	}
	expected := core.ReferenceHold{SessionID: sessionID, OutputID: outputs[0].ID, StreamID: f.stream.ID, GroupID: f.groups[0].ID, ProfileID: f.groups[0].ProfileID, DestinationID: f.destinations[0].ID, AssetID: f.asset.ID, GenerationID: f.generation.ID, VariantID: f.generation.RequiredProfiles[0].VariantID}
	if holds[0] != expected {
		t.Fatalf("точный Output hold: получено %+v, ожидалось %+v", holds[0], expected)
	}
}

func TestSessionManagerOldStartupCleanupDoesNotBlockNewExecution(t *testing.T) {
	receiver, _ := newPublisherReceiver(t, false)
	f := newSessionAcceptanceFixture(t, sessionAcceptanceBuildHelper(t), []string{"rtmp://" + receiver.listener.Addr().String() + "/live"}, []int{0})
	entered, release := make(chan struct{}), make(chan struct{})
	var releaseOnce sync.Once
	var attempts atomic.Int32
	f.config.Group.Player = func(_ context.Context, _ string, _ *os.File, a ffmpeg.Attempt) (ffmpeg.Command, error) {
		if attempts.Add(1) == 1 {
			close(entered)
			<-release
		}
		return publisherControlledCommand(publisherProfile(core.CodecAV1, 640, 360), a, 10, "")
	}
	f.launch(t)
	t.Cleanup(func() { releaseOnce.Do(func() { close(release) }) })
	old := f.start(t, "old-delayed-start")
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("старый builder не начался")
	}
	stop, err := f.manager.StopSession(context.Background(), core.SessionCommand{RequestID: "stop-old-delayed-start", SessionID: old.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	sessionAcceptanceReceipt(t, f.service, stop.ID, core.CommandSucceeded)
	previous, err := f.service.GetSession(context.Background(), old.SessionID)
	if err != nil || previous.State != core.SessionStopped || !previous.CleanupPending || !previous.Issued {
		t.Fatalf("старое liability скрыто: %+v %v", previous, err)
	}
	sessionAcceptanceAssertHolds(t, f, old.SessionID, true)
	fresh := f.start(t, "new-start-before-old-builder-join")
	current := f.running(t, fresh)
	feed, err := f.manager.Feed(current)
	if err != nil {
		t.Fatal(err)
	}
	receiver.wait(t, func(m []publisherWireMessage) bool { return publisherMediaCount(m) >= 8 })
	oldPending, err := f.service.GetSession(context.Background(), old.SessionID)
	if err != nil || !oldPending.CleanupPending || oldPending.Incarnation != current.Incarnation {
		t.Fatalf("новый start преждевременно снял старые holds: %+v %v", oldPending, err)
	}
	sessionAcceptanceAssertHolds(t, f, old.SessionID, true)
	sessionAcceptanceAssertHolds(t, f, current.ID, true)
	before := publisherMediaCount(receiver.snapshot())
	releaseOnce.Do(func() { close(release) })
	sessionAcceptanceWait(t, func() bool {
		previous, err = f.service.GetSession(context.Background(), old.SessionID)
		return err == nil && !previous.CleanupPending && !previous.Issued
	})
	receiver.wait(t, func(m []publisherWireMessage) bool { return publisherMediaCount(m) >= before+8 })
	after, err := f.service.GetSession(context.Background(), current.ID)
	if err != nil || after.State != core.SessionRunning || !after.Issued {
		t.Fatalf("поздняя очистка отозвала новый запуск: %+v %v", after, err)
	}
	if now, err := f.manager.Feed(after); err != nil || now != feed || receiver.accepted.Load() != 1 {
		t.Fatal("поздняя очистка заменила Feed/здоровое соединение", err)
	}
	sessionAcceptanceAssertHolds(t, f, old.SessionID, false)
	sessionAcceptanceAssertHolds(t, f, current.ID, true)
	if _, err = f.service.RuntimeSnapshot(context.Background(), core.RuntimeSessionFence{SessionID: old.SessionID, Incarnation: old.Incarnation, Lifecycle: old.Lifecycle}); !errors.Is(err, core.ErrNotReady) {
		t.Fatalf("старая телеметрия сохранена: %v", err)
	}
}

func sessionAcceptanceReservePreparation(t *testing.T, f *sessionAcceptanceFixture) (core.FallbackGeneration, core.PreparationFence, []core.OwnedFile) {
	t.Helper()
	ctx := context.Background()
	asset, err := f.service.GetAsset(ctx, f.asset.ID)
	if err != nil {
		t.Fatal(err)
	}
	g, err := f.service.ReservePreparation(ctx, core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage, GeneratorVersion: "controlled-generator"})
	if err != nil {
		t.Fatal(err)
	}
	fence, err := f.service.ClaimPreparation(ctx, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.service.MarkPreparationIssued(ctx, fence); err != nil {
		t.Fatal(err)
	}
	files, err := f.service.ListGenerationFiles(ctx, core.PageRequest{Query: core.ListQuery{Limit: 32, GenerationID: g.ID}})
	if err != nil {
		t.Fatal(err)
	}
	return g, fence, files.Items
}

func TestSessionManagerTerminalPreparationRetainsLiabilityWithoutStartGate(t *testing.T) {
	receiver, _ := newPublisherReceiver(t, false)
	f := newSessionAcceptanceFixture(t, sessionAcceptanceBuildHelper(t), []string{"rtmp://" + receiver.listener.Addr().String() + "/live"}, []int{0})
	g, fence, _ := sessionAcceptanceReservePreparation(t, f)
	owner, err := f.service.BindPreparationOwner(context.Background(), fence)
	if err != nil {
		t.Fatal(err)
	}
	finish, err := owner.Register(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(finish)
	f.launch(t)
	ctx := context.Background()
	active, err := f.service.GetGeneration(ctx, g.ID)
	if err != nil || active.State != core.PreparationIssued || !active.Issued || core.PreparationFenceFor(active) != fence || active.Incarnation != f.store.Incarnation() || !owner.Valid() {
		t.Fatalf("не подтверждена активная подготовка текущего владельца: %+v %v", active, err)
	}
	assetBefore, err := f.service.GetAsset(ctx, f.asset.ID)
	if err != nil || assetBefore.CurrentGenerationID != f.generation.ID || assetBefore.PendingGenerationID != g.ID {
		t.Fatalf("прежняя рабочая заглушка либо новая подготовка недоступна: %+v %v", assetBefore, err)
	}
	bindings, err := f.service.SelectStartBindings(ctx, f.stream.ID, nil)
	if err != nil || len(bindings) != 1 || bindings[0].GenerationID != f.generation.ID || bindings[0].GeneratorVersion != f.config.GeneratorVersion {
		t.Fatalf("не подтверждена готовая конфигурация запуска: %+v %v", bindings, err)
	}
	variant, err := f.service.GetVariant(ctx, bindings[0].VariantID)
	if err != nil {
		t.Fatal(err)
	}
	file, err := f.files.OpenExact(ctx, variant.ManagedPath, variant.SizeBytes, variant.ContentHash)
	if err != nil {
		t.Fatal("рабочий fallback физически недоступен:", err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	assertNoStart := func() {
		t.Helper()
		sessions, sessionErr := f.service.ListSessions(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8, StreamID: f.stream.ID}})
		receipts, receiptErr := f.service.ListReceipts(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8}})
		outputs, outputErr := f.service.ListOutputs(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8}})
		scopes, scopeErr := os.ReadDir(filepath.Join(f.directory, "p", "scopes"))
		if sessionErr != nil || receiptErr != nil || outputErr != nil || scopeErr != nil || len(sessions.Items) != 0 || sessions.NextCursor != "" || len(receipts.Items) != 0 || receipts.NextCursor != "" || len(outputs.Items) != 0 || outputs.NextCursor != "" || len(scopes) != 0 || f.starts.Load() != 0 || receiver.accepted.Load() != 0 {
			t.Fatalf("отклонённый start оставил выполнение: sessions=%+v (%v), receipts=%+v (%v), outputs=%+v (%v), scopes=%v (%v), builders=%d, connections=%d", sessions, sessionErr, receipts, receiptErr, outputs, outputErr, scopes, scopeErr, f.starts.Load(), receiver.accepted.Load())
		}
	}
	assertNoStart()
	const rejectedRequestID = "active-preparation-must-exclude"
	rejected, err := f.manager.StartSession(ctx, core.StartSessionRequest{RequestID: rejectedRequestID, StreamID: f.stream.ID})
	if !errors.Is(err, core.ErrNotReady) || rejected.ID != "" || rejected.SessionID != "" {
		t.Fatalf("активная подготовка не исключена до принятия start: receipt=%+v err=%v", rejected, err)
	}
	if receipt, err := f.service.FindReceipt(ctx, rejectedRequestID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("отклонённый RequestID получил квитанцию: %+v %v", receipt, err)
	}
	assertNoStart()
	if after, err := f.service.GetGeneration(ctx, g.ID); err != nil || after.State != active.State || !after.Issued || core.PreparationFenceFor(after) != fence || !owner.Valid() {
		t.Fatalf("отклонённый start изменил активную подготовку: %+v %v", after, err)
	}
	if err = f.service.CancelPreparation(context.Background(), fence, "Отменена владельцем до завершения внешней работы"); err != nil {
		t.Fatal(err)
	}
	if owner.Valid() {
		t.Fatal("terminal preparation сохранила право новых действий")
	}
	if err = f.service.AcknowledgePreparationCleanup(context.Background(), fence); !errors.Is(err, core.ErrInUse) {
		t.Fatalf("незавершённая операция выдана за cleanup: %v", err)
	}
	r := f.start(t, "start-with-terminal-preparation")
	current := f.running(t, r)
	receiver.wait(t, func(m []publisherWireMessage) bool { return publisherMediaCount(m) >= 8 })
	pending, err := f.service.GetGeneration(context.Background(), g.ID)
	if err != nil || !pending.Issued || !pending.CleanupPending || pending.State != core.PreparationFailed {
		t.Fatalf("новый start удалил старое liability: %+v %v", pending, err)
	}
	asset, err := f.service.GetAsset(context.Background(), f.asset.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.service.ReservePreparation(context.Background(), core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage, GeneratorVersion: "controlled-generator"}); !errors.Is(err, core.ErrInUse) {
		t.Fatalf("обратное исключение потеряно: %v", err)
	}
	finish()
	if err = f.service.AcknowledgePreparationCleanup(context.Background(), fence); err != nil {
		t.Fatal(err)
	}
	after, err := f.service.GetSession(context.Background(), current.ID)
	if err != nil || after.State != core.SessionRunning || !after.Issued {
		t.Fatalf("late preparation ack затронул Session: %+v %v", after, err)
	}
	if _, err = f.manager.Feed(after); err != nil {
		t.Fatal(err)
	}
	sessionAcceptanceAssertHolds(t, f, current.ID, true)
}

func TestSessionManagerReopenedDatabaseAdmitsNewExecutionWithUnknownOldLiabilities(t *testing.T) {
	helper := sessionAcceptanceBuildHelper(t)
	receiver, _ := newPublisherReceiver(t, false)
	f := newSessionAcceptanceFixture(t, helper, []string{"rtmp://" + receiver.listener.Addr().String() + "/live"}, []int{0})
	g, preparation, _ := sessionAcceptanceReservePreparation(t, f)
	if err := f.service.CancelPreparation(context.Background(), preparation, "Отозванная подготовка прежнего владельца"); err != nil {
		t.Fatal(err)
	}
	bindings, err := f.service.SelectStartBindings(context.Background(), f.stream.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Старому владельцу выдана попытка, но внешний эффект неизвестен; готовность нового проверяет Manager.
	old, err := f.service.StartSession(context.Background(), core.StartSessionRequest{RequestID: "old-owner-issued-start", StreamID: f.stream.ID, Bindings: bindings})
	if err != nil {
		t.Fatal(err)
	}
	fence, err := f.service.Claim(context.Background(), old.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.service.MarkIssued(context.Background(), fence); err != nil {
		t.Fatal(err)
	}
	if err = f.store.Close(); err != nil {
		t.Fatal(err)
	}
	f.store, err = storage.Open(context.Background(), filepath.Join(f.directory, "state.sqlite"), sessionAcceptanceOptions())
	if err != nil {
		t.Fatal(err)
	}
	f.service, err = core.NewService(f.store, f.store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	f.configureOwners(t, helper)
	f.launch(t)
	previous, err := f.service.GetSession(context.Background(), old.SessionID)
	if err != nil || previous.State != core.SessionInterrupted || !previous.CleanupPending || !previous.Issued {
		t.Fatalf("recovery выдумал старое завершение: %+v %v", previous, err)
	}
	sessionAcceptanceAssertHolds(t, f, old.SessionID, true)
	if _, err = f.service.RuntimeSnapshot(context.Background(), core.RuntimeSessionFence{SessionID: old.SessionID, Incarnation: old.Incarnation, Lifecycle: old.Lifecycle}); !errors.Is(err, core.ErrNotReady) {
		t.Fatalf("recovery воскресил телеметрию: %v", err)
	}
	r := f.start(t, "manual-start-new-incarnation")
	current := f.running(t, r)
	receiver.wait(t, func(m []publisherWireMessage) bool { return publisherMediaCount(m) >= 8 })
	previous, err = f.service.GetSession(context.Background(), old.SessionID)
	if err != nil || !previous.CleanupPending || !previous.Issued || previous.Incarnation == current.Incarnation {
		t.Fatalf("новый допуск снял старые holds: %+v %v", previous, err)
	}
	oldPreparation, err := f.service.GetGeneration(context.Background(), g.ID)
	if err != nil || !oldPreparation.Issued || !oldPreparation.CleanupPending || !oldPreparation.State.Terminal() {
		t.Fatalf("UNKNOWN preparation потеряна: %+v %v", oldPreparation, err)
	}
	oldReceipt, err := f.service.GetReceipt(context.Background(), old.ID)
	if err != nil || oldReceipt.Status != core.CommandInterrupted || receiver.accepted.Load() != 1 {
		t.Fatalf("старый start повторён: %+v %v", oldReceipt, err)
	}
	sessionAcceptanceAssertHolds(t, f, old.SessionID, true)
	sessionAcceptanceAssertHolds(t, f, current.ID, true)
}

func TestFileCleanupPreservesReplacementAndRenameProofAfterRuntimeAcknowledgement(t *testing.T) {
	helper := sessionAcceptanceBuildHelper(t)
	for _, replacement := range []bool{false, true} {
		t.Run(map[bool]string{false: "owned-file", true: "replaced-file"}[replacement], func(t *testing.T) {
			f := newSessionAcceptanceFixture(t, helper, nil, nil)
			g, fence, ledger := sessionAcceptanceReservePreparation(t, f)
			var directory, staging, original core.OwnedFile
			for _, file := range ledger {
				switch file.ManagedPath {
				case core.GenerationDirectory(g.ID):
					directory = file
				case g.StagingPath:
					staging = file
				case g.OriginalPath:
					original = file
				}
			}
			if err := f.files.CreateOwnedGeneration(directory); err != nil {
				t.Fatal(err)
			}
			writer, err := f.files.CreateOwned(staging)
			if err != nil {
				t.Fatal(err)
			}
			_, writeErr := writer.WriteString("hashless staged media")
			if err = errors.Join(writeErr, writer.Sync(), writer.Close()); err != nil {
				t.Fatal(err)
			}
			if err = f.files.PublishOwnedRename(staging, original); err != nil {
				t.Fatal(err)
			}
			if err = f.service.CancelPreparation(context.Background(), fence, "Отмена после rename"); err != nil {
				t.Fatal(err)
			}
			if err = f.service.AcknowledgePreparationCleanup(context.Background(), fence); err != nil {
				t.Fatal(err)
			}
			scope, err := f.processes.Create(context.Background(), "completed-file-writer")
			if err != nil {
				t.Fatal(err)
			}
			result, err := scope.Run(context.Background(), processowner.Command{Binary: "/usr/bin/true"})
			if err != nil || !result.Joined {
				t.Fatalf("native writer: %+v %v", result, err)
			}
			if err = scope.Close(); err != nil {
				t.Fatal(err)
			}
			if err = f.processes.AcknowledgeScope(context.Background(), scope.ID()); err != nil {
				t.Fatal(err)
			}
			if err = f.files.Close(); err != nil {
				t.Fatal(err)
			}
			f.files, err = fallback.OpenFileStore(f.directory)
			if err != nil {
				t.Fatal(err)
			}
			f.configureOwners(t, helper)
			if replacement {
				if err = os.Rename(filepath.Join(f.directory, original.ManagedPath), filepath.Join(f.directory, original.ManagedPath)+".displaced"); err != nil {
					t.Fatal(err)
				}
				if err = os.WriteFile(filepath.Join(f.directory, original.ManagedPath), []byte("unrelated replacement"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			page, err := f.service.ListCleanups(context.Background(), core.PageRequest{Query: core.ListQuery{Limit: 64}})
			if err != nil {
				t.Fatal(err)
			}
			var sourceIntent, targetIntent core.CleanupIntent
			for _, intent := range page.Items {
				if intent.FileID == staging.ID {
					sourceIntent = intent
				}
				if intent.FileID == original.ID {
					targetIntent = intent
				}
			}
			if sourceIntent.ID == "" || targetIntent.ID == "" {
				t.Fatal("отмена не сохранила оба FILE намерения rename")
			}
			for _, intent := range []core.CleanupIntent{sourceIntent, targetIntent} {
				err = f.config.Preparation.(*fallback.Manager).CleanupFile(context.Background(), intent)
				if replacement && intent.ID == targetIntent.ID {
					if err == nil {
						t.Fatal("cleanup удалил replacement по старому FileID")
					}
					break
				}
				if err != nil {
					t.Fatal("runtime ack/reopen потеряли FILE proof:", err)
				}
			}
			stored, err := f.service.GetCleanup(context.Background(), targetIntent.ID)
			if err != nil || stored.Acknowledged == replacement {
				t.Fatalf("FILE acknowledgement=%v replacement=%v %v", stored.Acknowledged, replacement, err)
			}
			if replacement {
				bytes, err := os.ReadFile(filepath.Join(f.directory, original.ManagedPath))
				if err != nil || string(bytes) != "unrelated replacement" {
					t.Fatalf("replacement изменён: %q %v", bytes, err)
				}
				if err = f.files.RemoveOwned(directory); err == nil {
					t.Fatal("неизвестное дерево удалено рекурсивно")
				}
			} else if _, err = os.Stat(filepath.Join(f.directory, original.ManagedPath)); !errors.Is(err, os.ErrNotExist) {
				t.Fatal("принадлежащий файл не удалён:", err)
			}
		})
	}
}
