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

// Независимое чтение SQL проверяет точного владельца удержаний.
func sessionAcceptanceAssertHolds(t *testing.T, f *sessionAcceptanceFixture, sessionID core.ID, retained bool) {
	t.Helper()
	// dsn задаёт независимое подключение только для чтения к SQLite фикстуры с ожиданием блокировки
	// до 1000 мс.
	dsn := url.URL{Scheme: "file", Path: filepath.Join(f.directory, "state.sqlite"), RawQuery: "mode=ro&_busy_timeout=1000"}
	// db предоставляет отдельное SQL-подключение для проверки удержаний, а err сообщает об ошибке
	// его открытия.
	db, err := sql.Open("sqlite3", dsn.String())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// Закрывает независимое соединение SQLite после проверки удержаний.

		// err сообщает об ошибке закрытия независимого SQL-подключения проверки удержаний.
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	db.SetMaxOpenConns(1)
	// ctx ограничивает чтение удержаний двумя секундами, а cancel освобождает его таймер.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// tx объединяет чтение родительских и дочерних удержаний в одну транзакцию только для чтения.
	tx, err := db.BeginTx(ctx, &sql.TxOptions{ReadOnly: true})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// Откатывает ещё открытую транзакцию чтения при любом раннем выходе.
		_ = tx.Rollback()
	}()
	// parent принимает сохранённое удержание потока, заглушки и поколения выбранной сессии.
	var parent core.ReferenceHold
	parent.SessionID = sessionID
	err = tx.QueryRowContext(ctx, "SELECT stream_id, asset_id, generation_id FROM session_holds WHERE session_id = ?", sessionID).Scan(&parent.StreamID, &parent.AssetID, &parent.GenerationID)
	if retained {
		// expected задаёт точного ожидаемого владельца родительского удержания сессии.
		expected := core.ReferenceHold{SessionID: sessionID, StreamID: f.stream.ID, AssetID: f.asset.ID, GenerationID: f.generation.ID}
		if err != nil || parent != expected {
			t.Fatalf("точный parent hold: получено %+v, ожидалось %+v: %v", parent, expected, err)
		}
	} else if !errors.Is(err, sql.ErrNoRows) {
		t.Fatalf("parent hold не освобождён: %+v %v", parent, err)
	}
	// rows читает удержания выходов выбранной сессии для независимой проверки всех связанных
	// ресурсов.
	rows, err := tx.QueryContext(ctx, "SELECT output_id, stream_id, group_id, profile_id, destination_id, asset_id, variant_id, generation_id FROM output_holds WHERE session_id = ?", sessionID)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// Закрывает строки запроса удержаний при раннем выходе проверки.

		// err сообщает об ошибке закрытия строк независимого запроса удержаний.
		if err := rows.Close(); err != nil {
			t.Error(err)
		}
	}()
	// holds собирает точные удержания выходов, прочитанные в одной транзакции.
	var holds []core.ReferenceHold
	for rows.Next() {
		// hold принимает одну строку удержания выхода с идентификаторами всех связанных
		// ресурсов.
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
	// outputs содержит сохранённые выходы сессии для сравнения с независимым SQL-удержанием.
	outputs := f.outputs(t, sessionID)
	if len(outputs) != 1 || len(holds) != 1 {
		t.Fatalf("ожидалось одно точное удержание Output: outputs=%d holds=%+v", len(outputs), holds)
	}
	// expected задаёт полный ожидаемый набор идентификаторов удержания единственного выхода.
	expected := core.ReferenceHold{SessionID: sessionID, OutputID: outputs[0].ID, StreamID: f.stream.ID, GroupID: f.groups[0].ID, ProfileID: f.groups[0].ProfileID, DestinationID: f.destinations[0].ID, AssetID: f.asset.ID, GenerationID: f.generation.ID, VariantID: f.generation.RequiredProfiles[0].VariantID}
	if holds[0] != expected {
		t.Fatalf("точный Output hold: получено %+v, ожидалось %+v", holds[0], expected)
	}
}

// Проверяет новый ручной запуск до возврата задержанного построителя прежней остановленной сессии.
// Поздняя очистка снимает только старые удержания и сохраняет новый Feed, телеметрию и
// медиасоединение.
func TestSessionManagerOldStartupCleanupDoesNotBlockNewExecution(t *testing.T) {
	// receiver принимает публикацию нового выполнения для проверки сохранения соединения при поздней
	// очистке старого.
	receiver, _ := newPublisherReceiver(t, false)
	// f предоставляет менеджер и файловые ресурсы для задержанного конструктора прежней сессии.
	f := newSessionAcceptanceFixture(t, sessionAcceptanceBuildHelper(t), []string{"rtmp://" + receiver.listener.Addr().String() + "/live"}, []int{0})
	// entered сообщает о входе в первый конструктор проигрывателя, а release разрешает ему вернуться
	// после проверки нового запуска.
	entered, release := make(chan struct{}), make(chan struct{})
	// releaseOnce защищает канал release от повторного закрытия явным продолжением и очисткой теста.
	var releaseOnce sync.Once
	// attempts считает вызовы построителя, чтобы задержать только первый запуск проигрывателя.
	var attempts atomic.Int32
	f.config.Group.Player = func(_ context.Context, _ string, _ *os.File, a ffmpeg.Attempt) (ffmpeg.Command, error) {
		// Задерживает только первый конструктор проигрывателя, позволяя следующей Session запуститься
		// независимо.

		if attempts.Add(1) == 1 {
			close(entered)
			<-release
		}
		return publisherControlledCommand(publisherProfile(core.CodecAV1, 640, 360), a, 10, "")
	}
	f.launch(t)
	t.Cleanup(func() {
		// Снимает задержку прежнего конструктора при освобождении теста.

		releaseOnce.Do(func() {
			// Однократно разрешает возврат прежнего конструктора при освобождении теста.
			close(release)
		})
	})
	// old содержит квитанцию прежнего запуска, чей конструктор ещё не вернулся.
	old := f.start(t, "old-delayed-start")
	select {
	case <-entered:
	case <-time.After(5 * time.Second):
		t.Fatal("старый builder не начался")
	}
	// stop содержит принятую остановку прежней сессии, а err сообщает об отказе команды.
	stop, err := f.manager.StopSession(context.Background(), core.SessionCommand{RequestID: "stop-old-delayed-start", SessionID: old.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	sessionAcceptanceReceipt(t, f.service, stop.ID, core.CommandSucceeded)
	// previous содержит состояние прежней сессии при сохранённом обязательстве очистки.
	previous, err := f.service.GetSession(context.Background(), old.SessionID)
	if err != nil || previous.State != core.SessionStopped || !previous.CleanupPending || !previous.Issued {
		t.Fatalf("старое liability скрыто: %+v %v", previous, err)
	}
	sessionAcceptanceAssertHolds(t, f, old.SessionID, true)
	// fresh содержит квитанцию нового ручного запуска до возврата прежнего конструктора.
	fresh := f.start(t, "new-start-before-old-builder-join")
	// current содержит новое работающее выполнение сессии, независимое от прежней очистки.
	current := f.running(t, fresh)
	// feed сохраняет медиавход нового выполнения для проверки его неизменности после поздней
	// очистки.
	feed, err := f.manager.Feed(current)
	if err != nil {
		t.Fatal(err)
	}
	receiver.wait(t, func(m []publisherWireMessage) bool {
		// Ожидает начальную передачу нового исполнения при незавершённом прежнем конструкторе.
		return publisherMediaCount(m) >= 8
	})
	// oldPending содержит прежнюю сессию после нового запуска для проверки сохранения её удержаний.
	oldPending, err := f.service.GetSession(context.Background(), old.SessionID)
	if err != nil || !oldPending.CleanupPending || oldPending.Incarnation != current.Incarnation {
		t.Fatalf("новый start преждевременно снял старые holds: %+v %v", oldPending, err)
	}
	sessionAcceptanceAssertHolds(t, f, old.SessionID, true)
	sessionAcceptanceAssertHolds(t, f, current.ID, true)
	// before фиксирует число медиасообщений нового выполнения до возврата прежнего конструктора.
	before := publisherMediaCount(receiver.snapshot())
	releaseOnce.Do(func() {
		// Разрешает возврат прежнего конструктора после проверки сохранённых удержаний.
		close(release)
	})
	sessionAcceptanceWait(t, func() bool {
		// Ожидает подтверждение очистки прежней Session после позднего возврата конструктора.

		previous, err = f.service.GetSession(context.Background(), old.SessionID)
		return err == nil && !previous.CleanupPending && !previous.Issued
	})
	receiver.wait(t, func(m []publisherWireMessage) bool {
		// Проверяет продолжение нового исполнения после поздней очистки прежнего.
		return publisherMediaCount(m) >= before+8
	})
	// after содержит состояние нового выполнения после завершения прежней очистки.
	after, err := f.service.GetSession(context.Background(), current.ID)
	if err != nil || after.State != core.SessionRunning || !after.Issued {
		t.Fatalf("поздняя очистка отозвала новый запуск: %+v %v", after, err)
	}
	// now содержит текущий медиавход нового выполнения для сравнения с feed, а err сообщает об
	// ошибке его получения.
	if now, err := f.manager.Feed(after); err != nil || now != feed || receiver.accepted.Load() != 1 {
		t.Fatal("поздняя очистка заменила Feed/здоровое соединение", err)
	}
	sessionAcceptanceAssertHolds(t, f, old.SessionID, false)
	sessionAcceptanceAssertHolds(t, f, current.ID, true)
	if _, err = f.service.RuntimeSnapshot(context.Background(), core.RuntimeSessionFence{SessionID: old.SessionID, Incarnation: old.Incarnation, Lifecycle: old.Lifecycle}); !errors.Is(err, core.ErrNotReady) {
		t.Fatalf("старая телеметрия сохранена: %v", err)
	}
}

// Резервирует и выдаёт попытку подготовки для текущего Asset, затем возвращает поколение, точный fence
// и её файловые записи.
func sessionAcceptanceReservePreparation(t *testing.T, f *sessionAcceptanceFixture) (core.FallbackGeneration, core.PreparationFence, []core.OwnedFile) {
	t.Helper()
	// ctx используется для резервирования и выдачи попытки подготовки текущей заглушки.
	ctx := context.Background()
	// asset содержит актуальную заглушку с ревизией для резервирования подготовки, а err сообщает об
	// ошибке её чтения.
	asset, err := f.service.GetAsset(ctx, f.asset.ID)
	if err != nil {
		t.Fatal(err)
	}
	// g содержит новое зарезервированное поколение подготовки текущей заглушки.
	g, err := f.service.ReservePreparation(ctx, core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage, GeneratorVersion: "controlled-generator"})
	if err != nil {
		t.Fatal(err)
	}
	// fence содержит точное разрешение попытки подготовки, которая отмечается как выданная.
	fence, err := f.service.ClaimPreparation(ctx, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.service.MarkPreparationIssued(ctx, fence); err != nil {
		t.Fatal(err)
	}
	// files содержит файловые записи выданного поколения для проверок владения и очистки.
	files, err := f.service.ListGenerationFiles(ctx, core.PageRequest{Query: core.ListQuery{Limit: 32, GenerationID: g.ID}})
	if err != nil {
		t.Fatal(err)
	}
	return g, fence, files.Items
}

// Проверяет, что активная подготовка отклоняет запуск до принятия команды, а её терминальное состояние
// допускает запуск с прежней рабочей заглушкой при сохранённом обязательстве очистки. Позднее
// подтверждение подготовки не меняет новую сессию.
func TestSessionManagerTerminalPreparationRetainsLiabilityWithoutStartGate(t *testing.T) {
	// receiver принимает новый запуск при терминальной подготовке с неподтверждённой очисткой.
	receiver, _ := newPublisherReceiver(t, false)
	// f объединяет рабочую заглушку и менеджер для проверки исключения активной подготовки и допуска
	// после её отмены.
	f := newSessionAcceptanceFixture(t, sessionAcceptanceBuildHelper(t), []string{"rtmp://" + receiver.listener.Addr().String() + "/live"}, []int{0})
	// g содержит выданное поколение подготовки, а fence — точное разрешение этой попытки.
	g, fence, _ := sessionAcceptanceReservePreparation(t, f)
	// owner регистрирует активное право действий подготовки, а err сообщает об ошибке его привязки.
	owner, err := f.service.BindPreparationOwner(context.Background(), fence)
	if err != nil {
		t.Fatal(err)
	}
	// finish завершает зарегистрированную операцию подготовки, без чего подтверждение очистки должно
	// отклоняться.
	finish, err := owner.Register(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(finish)
	f.launch(t)
	// ctx используется для проверок допуска запуска при активной подготовке.
	ctx := context.Background()
	// active фиксирует выданное поколение текущего владельца до отклонённого запуска.
	active, err := f.service.GetGeneration(ctx, g.ID)
	if err != nil || active.State != core.PreparationIssued || !active.Issued || core.PreparationFenceFor(active) != fence || active.Incarnation != f.store.Incarnation() || !owner.Valid() {
		t.Fatalf("не подтверждена активная подготовка текущего владельца: %+v %v", active, err)
	}
	// assetBefore фиксирует прежнее рабочее и новое ожидающее поколения заглушки перед проверкой
	// допуска.
	assetBefore, err := f.service.GetAsset(ctx, f.asset.ID)
	if err != nil || assetBefore.CurrentGenerationID != f.generation.ID || assetBefore.PendingGenerationID != g.ID {
		t.Fatalf("прежняя рабочая заглушка либо новая подготовка недоступна: %+v %v", assetBefore, err)
	}
	// bindings содержит готовые привязки прежнего рабочего поколения, доступные независимо от
	// активной новой подготовки.
	bindings, err := f.service.SelectStartBindings(ctx, f.stream.ID, nil)
	if err != nil || len(bindings) != 1 || bindings[0].GenerationID != f.generation.ID || bindings[0].GeneratorVersion != f.config.GeneratorVersion {
		t.Fatalf("не подтверждена готовая конфигурация запуска: %+v %v", bindings, err)
	}
	// variant содержит точные свойства рабочего варианта для проверки его физической доступности.
	variant, err := f.service.GetVariant(ctx, bindings[0].VariantID)
	if err != nil {
		t.Fatal(err)
	}
	// file открывает рабочий вариант с проверкой размера и хеша перед проверкой отказа запуска.
	file, err := f.files.OpenExact(ctx, variant.ManagedPath, variant.SizeBytes, variant.ContentHash)
	if err != nil {
		t.Fatal("рабочий fallback физически недоступен:", err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	// assertNoStart проверяет отсутствие сессий, квитанций, выходов, областей процессов и соединений
	// после отклонённого запроса.
	assertNoStart := func() {
		// Проверяет отсутствие Session, квитанций, выходов, процессов и соединений после отклонённого запуска.

		t.Helper()
		// sessions содержит найденные сессии потока, а sessionErr сообщает об ошибке их чтения
		// при проверке отсутствия запуска.
		sessions, sessionErr := f.service.ListSessions(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8, StreamID: f.stream.ID}})
		// receipts содержит все квитанции фикстуры, а receiptErr сообщает об ошибке их чтения
		// при проверке раннего отказа.
		receipts, receiptErr := f.service.ListReceipts(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8}})
		// outputs содержит все сохранённые выходы, а outputErr сообщает об ошибке их чтения при
		// проверке отсутствия запуска.
		outputs, outputErr := f.service.ListOutputs(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8}})
		// scopes содержит записи областей процессов, а scopeErr сообщает об ошибке чтения их
		// каталога.
		scopes, scopeErr := os.ReadDir(filepath.Join(f.directory, "p", "scopes"))
		if sessionErr != nil || receiptErr != nil || outputErr != nil || scopeErr != nil || len(sessions.Items) != 0 || sessions.NextCursor != "" || len(receipts.Items) != 0 || receipts.NextCursor != "" || len(outputs.Items) != 0 || outputs.NextCursor != "" || len(scopes) != 0 || f.starts.Load() != 0 || receiver.accepted.Load() != 0 {
			t.Fatalf("отклонённый start оставил выполнение: sessions=%+v (%v), receipts=%+v (%v), outputs=%+v (%v), scopes=%v (%v), builders=%d, connections=%d", sessions, sessionErr, receipts, receiptErr, outputs, outputErr, scopes, scopeErr, f.starts.Load(), receiver.accepted.Load())
		}
	}
	assertNoStart()
	// rejectedRequestID задаёт идентификатор отклоняемого запроса для проверки отсутствия
	// сохранённой квитанции.
	const rejectedRequestID = "active-preparation-must-exclude"
	// rejected содержит результат запуска, который должен быть пустым при активной подготовке.
	rejected, err := f.manager.StartSession(ctx, core.StartSessionRequest{RequestID: rejectedRequestID, StreamID: f.stream.ID})
	if !errors.Is(err, core.ErrNotReady) || rejected.ID != "" || rejected.SessionID != "" {
		t.Fatalf("активная подготовка не исключена до принятия start: receipt=%+v err=%v", rejected, err)
	}
	// receipt содержит возможную квитанцию отклонённого запроса, а err должен подтверждать её
	// отсутствие.
	if receipt, err := f.service.FindReceipt(ctx, rejectedRequestID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("отклонённый RequestID получил квитанцию: %+v %v", receipt, err)
	}
	assertNoStart()
	// after содержит поколение после отклонённого запуска, а err сообщает об ошибке чтения при
	// проверке сохранения его состояния.
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
	// r содержит квитанцию нового запуска после отмены подготовки при ещё незавершённой внешней
	// операции.
	r := f.start(t, "start-with-terminal-preparation")
	// current содержит работающее выполнение сессии на прежнем рабочем поколении заглушки.
	current := f.running(t, r)
	receiver.wait(t, func(m []publisherWireMessage) bool {
		// Ожидает медиа новой Session при терминальной подготовке с ещё не подтверждённой очисткой.
		return publisherMediaCount(m) >= 8
	})
	// pending содержит отменённое поколение подготовки, которое должно сохранять выданную попытку и
	// обязательство очистки.
	pending, err := f.service.GetGeneration(context.Background(), g.ID)
	if err != nil || !pending.Issued || !pending.CleanupPending || pending.State != core.PreparationFailed {
		t.Fatalf("новый start удалил старое liability: %+v %v", pending, err)
	}
	// asset содержит текущую заглушку для проверки запрета новой подготовки во время работающей
	// сессии.
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
	// after содержит состояние новой сессии после позднего подтверждения очистки подготовки.
	after, err := f.service.GetSession(context.Background(), current.ID)
	if err != nil || after.State != core.SessionRunning || !after.Issued {
		t.Fatalf("late preparation ack затронул Session: %+v %v", after, err)
	}
	if _, err = f.manager.Feed(after); err != nil {
		t.Fatal(err)
	}
	sessionAcceptanceAssertHolds(t, f, current.ID, true)
}

// Проверяет новый ручной запуск после повторного открытия БД без повторения старой выданной команды.
// Неизвестное прежнее исполнение и подготовка сохраняют собственные удержания и обязательства очистки.
func TestSessionManagerReopenedDatabaseAdmitsNewExecutionWithUnknownOldLiabilities(t *testing.T) {
	// helper содержит путь настоящего вспомогательного процесса владения для конфигурации до и после
	// открытия SQLite заново.
	helper := sessionAcceptanceBuildHelper(t)
	// receiver принимает только новый ручной запуск для обнаружения нежелательного повтора прежней
	// команды.
	receiver, _ := newPublisherReceiver(t, false)
	// f предоставляет файловые ресурсы и хранилище для проверки неизвестных обязательств прежнего
	// владельца.
	f := newSessionAcceptanceFixture(t, helper, []string{"rtmp://" + receiver.listener.Addr().String() + "/live"}, []int{0})
	// g содержит прежнее поколение подготовки, а preparation — разрешение его выданной попытки перед
	// отменой.
	g, preparation, _ := sessionAcceptanceReservePreparation(t, f)
	// err сообщает об отказе отмены подготовки, обязательство очистки которой сохраняется после
	// открытия SQLite заново.
	if err := f.service.CancelPreparation(context.Background(), preparation, "Отозванная подготовка прежнего владельца"); err != nil {
		t.Fatal(err)
	}
	// bindings содержит готовые привязки рабочего поколения, а err сообщает об ошибке их выбора для
	// прежнего запуска.
	bindings, err := f.service.SelectStartBindings(context.Background(), f.stream.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	// Старому владельцу выдана попытка, но внешний эффект неизвестен; готовность нового проверяет Manager.
	// old содержит квитанцию прежнего запуска, чья выданная внешняя попытка останется неизвестной
	// после открытия SQLite заново.
	old, err := f.service.StartSession(context.Background(), core.StartSessionRequest{RequestID: "old-owner-issued-start", StreamID: f.stream.ID, Bindings: bindings})
	if err != nil {
		t.Fatal(err)
	}
	// fence содержит разрешение прежней команды, которое отмечается как выданное без подтверждения
	// завершения.
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
	// previous содержит восстановленное состояние прежней сессии с сохранёнными удержаниями и
	// очисткой.
	previous, err := f.service.GetSession(context.Background(), old.SessionID)
	if err != nil || previous.State != core.SessionInterrupted || !previous.CleanupPending || !previous.Issued {
		t.Fatalf("recovery выдумал старое завершение: %+v %v", previous, err)
	}
	sessionAcceptanceAssertHolds(t, f, old.SessionID, true)
	if _, err = f.service.RuntimeSnapshot(context.Background(), core.RuntimeSessionFence{SessionID: old.SessionID, Incarnation: old.Incarnation, Lifecycle: old.Lifecycle}); !errors.Is(err, core.ErrNotReady) {
		t.Fatalf("recovery воскресил телеметрию: %v", err)
	}
	// r содержит квитанцию нового ручного запуска в новой инкарнации хранилища.
	r := f.start(t, "manual-start-new-incarnation")
	// current содержит новое работающее выполнение после открытия SQLite заново.
	current := f.running(t, r)
	receiver.wait(t, func(m []publisherWireMessage) bool {
		// Ожидает медиа нового исполнения после повторного открытия БД при сохранённых обязательствах прежнего
		// владельца.
		return publisherMediaCount(m) >= 8
	})
	previous, err = f.service.GetSession(context.Background(), old.SessionID)
	if err != nil || !previous.CleanupPending || !previous.Issued || previous.Incarnation == current.Incarnation {
		t.Fatalf("новый допуск снял старые holds: %+v %v", previous, err)
	}
	// oldPreparation содержит прежнее терминальное поколение с неизвестной выданной попыткой и
	// неподтверждённой очисткой.
	oldPreparation, err := f.service.GetGeneration(context.Background(), g.ID)
	if err != nil || !oldPreparation.Issued || !oldPreparation.CleanupPending || !oldPreparation.State.Terminal() {
		t.Fatalf("UNKNOWN preparation потеряна: %+v %v", oldPreparation, err)
	}
	// oldReceipt содержит прерванную квитанцию прежнего запуска для проверки отсутствия
	// автоматического повтора.
	oldReceipt, err := f.service.GetReceipt(context.Background(), old.ID)
	if err != nil || oldReceipt.Status != core.CommandInterrupted || receiver.accepted.Load() != 1 {
		t.Fatalf("старый start повторён: %+v %v", oldReceipt, err)
	}
	sessionAcceptanceAssertHolds(t, f, old.SessionID, true)
	sessionAcceptanceAssertHolds(t, f, current.ID, true)
}

// Проверяет сохранение файлового доказательства переименования после подтверждения области процессов и
// повторного открытия FileStore. Принадлежащий файл удаляется, подменённый объект и его каталог
// сохраняются с неподтверждённой очисткой.
func TestFileCleanupPreservesReplacementAndRenameProofAfterRuntimeAcknowledgement(t *testing.T) {
	// helper содержит путь вспомогательного процесса владения для проверки файловых доказательств
	// после подтверждения области.
	helper := sessionAcceptanceBuildHelper(t)
	// replacement выбирает сохранение принадлежащего файла либо его физическую подмену после
	// открытия FileStore заново.
	for _, replacement := range []bool{false, true} {
		t.Run(map[bool]string{false: "owned-file", true: "replaced-file"}[replacement], func(t *testing.T) {
			// Проверяет удаление принадлежащего файла либо сохранение подменённого файла после подтверждения
			// очистки выполнения и повторного открытия.

			// f предоставляет отдельные файловые и процессные журналы для текущего варианта
			// проверки очистки.
			f := newSessionAcceptanceFixture(t, helper, nil, nil)
			// g содержит поколение подготовки, fence — разрешение его выданной попытки, а
			// ledger — точные файловые записи поколения.
			g, fence, ledger := sessionAcceptanceReservePreparation(t, f)
			// directory сохраняет запись каталога поколения, staging — запись временного
			// исходника, а original — запись опубликованного исходника после переименования.
			var directory, staging, original core.OwnedFile
			// file обозначает очередную файловую запись поколения при выборе каталога и
			// обеих сторон переименования.
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
			// err сообщает об отказе создания принадлежащего поколения с файловым
			// доказательством каталога.
			if err := f.files.CreateOwnedGeneration(directory); err != nil {
				t.Fatal(err)
			}
			// writer открывает принадлежащий временный файл подготовки, а err сообщает об
			// ошибке его создания.
			writer, err := f.files.CreateOwned(staging)
			if err != nil {
				t.Fatal(err)
			}
			// writeErr сообщает об ошибке записи синтетического содержимого до синхронизации
			// и закрытия временного файла.
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
			// scope содержит отдельную область процессов, подтверждение которой не должно
			// удалять файловое доказательство переименования.
			scope, err := f.processes.Create(context.Background(), "completed-file-writer")
			if err != nil {
				t.Fatal(err)
			}
			// result содержит итог короткого процесса /usr/bin/true для проверки завершения
			// области процессов.
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
			// page содержит намерения очистки после отмены подготовки для поиска обеих
			// сторон переименования.
			page, err := f.service.ListCleanups(context.Background(), core.PageRequest{Query: core.ListQuery{Limit: 64}})
			if err != nil {
				t.Fatal(err)
			}
			// sourceIntent сохраняет очистку прежнего временного пути, а targetIntent —
			// очистку опубликованного пути после переименования.
			var sourceIntent, targetIntent core.CleanupIntent
			// intent обозначает очередную запись очистки при поиске намерений временного и
			// опубликованного файлов.
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
			// intent обозначает очередную сторону переименования, очищаемую через рабочий
			// менеджер подготовки.
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
			// stored содержит намерение очистки опубликованного файла для проверки
			// подтверждения только принадлежащего объекта.
			stored, err := f.service.GetCleanup(context.Background(), targetIntent.ID)
			if err != nil || stored.Acknowledged == replacement {
				t.Fatalf("FILE acknowledgement=%v replacement=%v %v", stored.Acknowledged, replacement, err)
			}
			if replacement {
				// bytes содержит прочитанное содержимое подменённого файла, а err
				// сообщает об ошибке чтения после отказа очистки.
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
