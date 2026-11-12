package session

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"syscall"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
	"github.com/GregoryGost/gost-restreamer/internal/processowner"
)

// startupHelperBarrier задерживает настоящий вспомогательный процесс перед exec и сохраняет идентичность
// его операции.
type startupHelperBarrier struct {
	// root указывает журнал процессов; marker указывает файл идентификаторов задержанного запуска;
	// wrapper указывает исполняемую оболочку барьера.
	root, marker, wrapper string
	// release владеет открытым FIFO для разрешения задержанного запуска.
	release *os.File
	// once обеспечивает однократную отправку разрешения через FIFO.
	once sync.Once
}

// newStartupHelperBarrier собирает настоящий gost-process-helper и обёртку, которая сохраняет
// переданные дескрипторы и ждёт разрешения через FIFO до exec.
func newStartupHelperBarrier(t *testing.T) *startupHelperBarrier {
	t.Helper()
	// directory изолирует вспомогательный бинарник, оболочку, FIFO и журнал процессов проверки.
	directory := t.TempDir()
	// helper указывает собранный настоящий gost-process-helper, запускаемый после барьера.
	helper := filepath.Join(directory, "gost-process-helper")
	// args задаёт сборку вспомогательного процесса с возможным включением покрытия.
	args := []string{"build", "-o", helper}
	// directory задаёт переданный окружением каталог покрытия вспомогательного процесса.
	if directory := os.Getenv("GOST_TEST_PROCESS_HELPER_COVERDIR"); directory != "" {
		t.Setenv("GOCOVERDIR", directory)
		args = append(args, "-cover", "-covermode=atomic", "-coverpkg=github.com/GregoryGost/gost-restreamer/...")
	}
	// command собирает настоящий вспомогательный процесс для управляемого запуска.
	command := exec.Command("go", append(args, "../../cmd/gost-process-helper")...)
	// output содержит диагностику сборки вспомогательного процесса; err сообщает отказ сборки.
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("helper: %v %s", err, output)
	}
	// fifo указывает именованный канал разрешения запуска вспомогательного процесса.
	fifo := filepath.Join(directory, "release")
	// err сообщает отказ создания именованного канала барьера.
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	// release содержит открытый дескриптор FIFO; err сообщает отказ его открытия.
	release, err := os.OpenFile(fifo, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	// b объединяет пути и дескриптор управляемого барьера запуска.
	b := &startupHelperBarrier{root: filepath.Join(directory, "processes"), marker: filepath.Join(directory, "entered"), wrapper: filepath.Join(directory, "helper-barrier"), release: release}
	// quote экранирует пути в отдельные аргументы командной оболочки создаваемой обёртки.
	quote := func(s string) string {
		// Экранирует строку как один аргумент командной оболочки для путей проверочной обёртки.
		return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'"
	}
	// Оболочка сохраняет lifeline и exec передаёт владение настоящему helper.
	// script сохраняет идентификаторы операции, ждёт FIFO и передаёт выполнение настоящему helper с
	// исходными аргументами.
	script := "#!/bin/sh\nprintf '%s %s\\n' \"$3\" \"$4\" >" + quote(b.marker) + "\nIFS= read -r release <" + quote(fifo) + "\nexec " + quote(helper) + " \"$@\"\n"
	if err = os.WriteFile(b.wrapper, []byte(script), 0700); err != nil {
		_ = release.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Разрешает задержанный запуск и закрывает принадлежащий проверке дескриптор FIFO.

		b.unblock(t)
		// err сообщает отказ закрытия дескриптора FIFO при очистке теста.
		if err := release.Close(); err != nil {
			t.Error(err)
		}
	})
	return b
}

// unblock однократно записывает разрешение в FIFO, позволяя задержанному запуску gost-process-helper
// продолжиться.
func (b *startupHelperBarrier) unblock(t *testing.T) {
	t.Helper()
	b.once.Do(func() {
		// Отправляет единственное разрешение продолжить запуск через FIFO и сообщает тесту об ошибке записи.

		// err сообщает отказ записи однократного разрешения в FIFO.
		if _, err := b.release.WriteString("release\n"); err != nil {
			t.Error(err)
		}
	})
}

// configure заменяет реестр процессов конфигурацией с задерживающей обёрткой и регистрирует его
// закрытие в тесте.
func (b *startupHelperBarrier) configure(t *testing.T, c *Config) {
	t.Helper()
	// registry содержит реестр с задерживающей оболочкой helper; err сообщает отказ создания этого
	// реестра.
	registry, err := processowner.New(processowner.Config{Root: b.root, RuntimeRoot: c.Group.FFmpeg.BaseDir, HelperPath: b.wrapper, Limits: processowner.Limits{MaxOperations: 512, MaxRecordBytes: 1 << 20, MaxCommandArgs: 128, MaxCommandBytes: 16384, MaxExtraFiles: 8, PageSize: 32}, GracePeriod: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	c.Processes = registry
	t.Cleanup(func() {
		// Закрывает реестр процессов с задерживающей обёрткой при завершении проверки.

		// err сообщает отказ закрытия реестра с задерживающей оболочкой.
		if err := registry.Close(); err != nil {
			t.Error(err)
		}
	})
}

// waitStartupCondition опрашивает условие с миллисекундным интервалом и завершает тест с заданным
// описанием при отмене контекста.
func waitStartupCondition(t *testing.T, ctx context.Context, description string, condition func() bool) {
	t.Helper()
	// ticker задаёт миллисекундный период проверки условия запуска или очистки.
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for !condition() {
		select {
		case <-ticker.C:
		case <-ctx.Done():
			t.Fatal(description, ctx.Err())
		}
	}
}

// entered ждёт маркер обёртки и возвращает точные идентификаторы области и операции задержанного
// запуска.
func (b *startupHelperBarrier) entered(t *testing.T, ctx context.Context) (string, string) {
	t.Helper()
	// fields сохраняет идентификаторы области и операции из маркера достигнутого барьера.
	var fields []string
	waitStartupCondition(t, ctx, "helper не достиг барьера", func() bool {
		// Читает маркер обёртки до появления двух идентификаторов, не принимая ещё не созданный файл за отказ
		// проверки.

		// data содержит байты маркера запуска; err сообщает отсутствие маркера либо отказ его
		// чтения.
		data, err := os.ReadFile(b.marker)
		if errors.Is(err, os.ErrNotExist) {
			return false
		}
		if err != nil {
			t.Fatal(err)
		}
		fields = strings.Fields(string(data))
		return len(fields) == 2
	})
	return fields[0], fields[1]
}

// startupOperation представляет поля журнальной записи для проверки точной native-операции и её
// состояния.
type startupOperation struct {
	// ScopeID задаёт область операции; ID задаёт идентификатор операции; Kind задаёт её вид; State
	// задаёт сохранённую стадию исполнения.
	ScopeID, ID, Kind, State string
}

// operation читает запись операции из журнала и проверяет, что она относится к заданной области и
// имеет вид native.
func (b *startupHelperBarrier) operation(t *testing.T, scope, operation string) startupOperation {
	t.Helper()
	// data содержит JSON точной журнальной операции; err сообщает отказ чтения её файла.
	data, err := os.ReadFile(filepath.Join(b.root, "scopes", scope, "operations", operation+".json"))
	if err != nil {
		t.Fatal(err)
	}
	// record содержит разобранную журнальную запись задержанного запуска.
	var record startupOperation
	if err = json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if record.ScopeID != scope || record.ID != operation || record.Kind != "native" {
		t.Fatal("барьер не соответствует точной native-операции", record)
	}
	return record
}

// startupParent запускает родительскую сессию, ждёт успешную квитанцию и доступный Feed, затем
// возвращает её локальный узел.
func startupParent(t *testing.T, ctx context.Context, f managerFixture) (core.CommandReceipt, core.Session, *media.Router, *sessionNode) {
	t.Helper()
	// start содержит принятую команду родительского запуска; err сообщает отказ её приёма.
	start, err := f.manager.StartSession(ctx, core.StartSessionRequest{RequestID: "barrier-parent", StreamID: f.stream.ID})
	if err != nil {
		t.Fatal(err)
	}
	// current содержит успешное завершение родительского запуска до выдачи его ресурсов тесту.
	if current := waitReceipt(t, f, start); current.Status != core.CommandSucceeded {
		t.Fatal(current)
	}
	// session содержит сохранённую родительскую сессию для получения Feed.
	session, err := f.service.GetSession(ctx, start.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	// feed сохраняет доступный медиамаршрутизатор родителя.
	var feed *media.Router
	waitStartupCondition(t, ctx, "нет Feed родителя", func() bool {
		// Проверяет доступность Feed родительской сессии и сохраняет его для проверки последующей изоляции.

		feed, err = f.manager.Feed(session)
		return err == nil
	})
	f.manager.mu.Lock()
	// node указывает локального владельца родительской сессии.
	node := f.manager.sessions[start.SessionID]
	f.manager.mu.Unlock()
	return start, session, feed, node
}

// startupReceipt ждёт терминальную сохранённую квитанцию в пределах переданного контекста.
func startupReceipt(t *testing.T, ctx context.Context, f managerFixture, receipt core.CommandReceipt) core.CommandReceipt {
	t.Helper()
	// current сохраняет последнюю прочитанную квитанцию до достижения терминального состояния.
	var current core.CommandReceipt
	waitStartupCondition(t, ctx, "команда не ограничена сроком запуска", func() bool {
		// Читает квитанцию команды до терминального статуса в пределах срока проверки.

		// err сообщает отказ очередного чтения ожидаемой квитанции.
		var err error
		current, err = f.service.GetReceipt(ctx, receipt.ID)
		if err != nil {
			t.Fatal(err)
		}
		return current.Status.Terminal()
	})
	return current
}

// TestManagerStartupHelperReleasedBeforeDeadlineBecomesReady проверяет, что разрешённый до срока
// gost-process-helper обеспечивает готовность выхода и выдачу обеих дорожек заглушки.
func TestManagerStartupHelperReleasedBeforeDeadlineBecomesReady(t *testing.T) {
	// barrier управляет задержкой настоящего helper перед созданием медиапроцесса.
	barrier := newStartupHelperBarrier(t)
	// f предоставляет родителя и реестр процессов с согласованными сроками запуска.
	f := newManagerFixture(t, false, func(c *Config) {
		// Согласует сроки запуска FFmpeg и сессии и устанавливает реестр с задерживаемым gost-process-helper.

		c.Group.FFmpeg.StartupTimeout = c.StartupTimeout
		barrier.configure(t, c)
	})
	defer barrier.unblock(t)
	// ctx ограничивает проверку готовности пятнадцатью секундами; cancel освобождает этот срок.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// parent содержит квитанцию родителя; node содержит локальные ресурсы родителя и область
	// запуска.
	parent, _, _, node := startupParent(t, ctx, f)
	// added содержит команду добавления с задержанным helper; err сообщает отказ её приёма.
	added, err := f.manager.AddOutput(ctx, core.AddOutputRequest{RequestID: "released-add", SessionID: parent.SessionID, DestinationID: f.destination.ID})
	if err != nil {
		t.Fatal(err)
	}
	// scope задаёт область задержанного запуска; operation задаёт его точную native-операцию.
	scope, operation := barrier.entered(t, ctx)
	if scope != node.scope.ID() || barrier.operation(t, scope, operation).State != "RESERVED" {
		t.Fatal("helper не остановлен до native spawn")
	}
	barrier.unblock(t)
	// deadline ограничивает ожидание готовности сроком запуска менеджера; stop освобождает этот
	// срок.
	deadline, stop := context.WithTimeout(ctx, f.config.StartupTimeout)
	defer stop()
	// current содержит результат добавления после разрешения helper до истечения срока.
	if current := startupReceipt(t, deadline, f, added); current.Status != core.CommandSucceeded {
		t.Fatal("освобождённый helper не дал готовность", current)
	}
	node.mu.Lock()
	// member содержит членство добавленного выхода после успешной начальной готовности.
	member := node.members[added.OutputID]
	node.mu.Unlock()
	if member == nil {
		t.Fatal("нет ссылки готового выхода")
	}
	member.mu.Lock()
	// ref содержит ссылку готовой группы добавленного выхода.
	ref := member.reference
	member.mu.Unlock()
	if ref == nil || ref.Group().Ready() != nil {
		t.Fatal("Add завершён до фактической готовности fallback")
	}
	// sub получает медиапакеты готовой заглушки для проверки наличия обеих дорожек.
	sub, err := ref.Router().Subscribe(f.config.Publisher.Queue)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// Освобождает подписку на готовую заглушку после проверки обеих дорожек.
		_ = sub.Close()
	}()
	// seen отмечает дорожки, уже полученные от готовой заглушки.
	seen := map[media.Track]bool{}
	for len(seen) < 2 {
		// delivery содержит очередной пакет заглушки; err сообщает отказ получения пакета в срок
		// готовности.
		delivery, err := sub.Next(deadline)
		if err != nil {
			t.Fatal("готовый fallback не выдаёт обе дорожки", err)
		}
		seen[delivery.Packet.Config().Info().Track] = true
	}
	if !seen[media.TrackVideo] || !seen[media.TrackAudio] {
		t.Fatal("неверные дорожки fallback", seen)
	}
}

// openStartupHoldObserver открывает отдельное соединение SQLite только для чтения и проверяет
// query_only до наблюдения удержаний.
func openStartupHoldObserver(t *testing.T, ctx context.Context, path string) *sql.DB {
	t.Helper()
	// dsn задаёт отдельное соединение SQLite в режиме чтения с включённым query_only.
	dsn := url.URL{Scheme: "file", Path: path, RawQuery: url.Values{"mode": {"ro"}, "_query_only": {"on"}, "_busy_timeout": {"1000"}}.Encode()}
	// db предоставляет наблюдение удержаний через отдельное соединение; err сообщает отказ его
	// создания.
	db, err := sql.Open("sqlite3", dsn.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		// Закрывает отдельное соединение SQLite для наблюдения удержаний.

		// err сообщает отказ закрытия соединения наблюдателя SQLite.
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	// queryOnly содержит фактическое значение PRAGMA, подтверждающее запрет записи наблюдателем.
	var queryOnly int
	if err = db.QueryRowContext(ctx, "PRAGMA query_only").Scan(&queryOnly); err != nil || queryOnly != 1 {
		t.Fatal("наблюдатель SQLite допускает запись", queryOnly, err)
	}
	return db
}

// startupHolds читает точные удержания сессии и её выходов из отдельного соединения SQLite в порядке
// идентификаторов выхода.
func startupHolds(t *testing.T, ctx context.Context, db *sql.DB, session core.ID) []core.ReferenceHold {
	t.Helper()
	// rows содержит удержания сессии и её выходов в порядке OutputID; err сообщает отказ SQL-запроса
	// удержаний.
	rows, err := db.QueryContext(ctx, `SELECT session_id, '', stream_id, '', '', '', asset_id, '', generation_id FROM session_holds WHERE session_id = ?
UNION ALL SELECT session_id, output_id, stream_id, group_id, profile_id, destination_id, asset_id, variant_id, generation_id FROM output_holds WHERE session_id = ? ORDER BY 2`, session, session)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// Закрывает строки запроса удержаний после чтения или отказа проверки.
		_ = rows.Close()
	}()
	// result накапливает прочитанные точные удержания сессии и назначений.
	var result []core.ReferenceHold
	for rows.Next() {
		// hold содержит очередное удержание, прочитанное из SQL-строки.
		var hold core.ReferenceHold
		if err = rows.Scan(&hold.SessionID, &hold.OutputID, &hold.StreamID, &hold.GroupID, &hold.ProfileID, &hold.DestinationID, &hold.AssetID, &hold.VariantID, &hold.GenerationID); err != nil {
			t.Fatal(err)
		}
		result = append(result, hold)
	}
	if err = rows.Err(); err != nil {
		t.Fatal(err)
	}
	return result
}

// TestManagerStartupDeadlineRetainsLateHelperUntilPhysicalJoin проверяет сохранение точных удержаний и
// обязательств очистки после срока запуска, изоляцию нового эфира и освобождение прежних ресурсов
// только после завершения задержанного gost-process-helper.
func TestManagerStartupDeadlineRetainsLateHelperUntilPhysicalJoin(t *testing.T) {
	// barrier задерживает helper за пределы исходного срока запуска для проверки сохранения
	// ресурсов.
	barrier := newStartupHelperBarrier(t)
	// causes передаёт причину отмены контекста попытки проигрывателя.
	causes := make(chan error, 1)
	// f предоставляет реестр с задержанным helper и наблюдением причины отмены попытки.
	f := newManagerFixture(t, false, func(c *Config) {
		// Устанавливает задерживаемый gost-process-helper и сохраняет причину отмены контекста построителя
		// проигрывателя.

		barrier.configure(t, c)
		// original сохраняет исходный построитель проигрывателя под наблюдением отмены его
		// контекста.
		original := c.Group.Player
		c.Group.Player = func(ctx context.Context, binary string, file *os.File, attempt ffmpeg.Attempt) (ffmpeg.Command, error) {
			// Регистрирует наблюдение причины отмены попытки и строит исходную команду проигрывателя.

			context.AfterFunc(ctx, func() {
				// Передаёт тесту причину отмены контекста попытки, чтобы отличить локальный срок FFmpeg от общего
				// срока запуска.
				causes <- context.Cause(ctx)
			})
			return original(ctx, binary, file, attempt)
		}
	})
	defer barrier.unblock(t)
	// ctx ограничивает сценарий позднего helper двадцатью секундами; cancel освобождает этот срок.
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	// parent содержит команду родителя; session содержит его сохранённое исполнение; feed содержит
	// исходный медиавход; node содержит локальные ресурсы родителя.
	parent, session, feed, node := startupParent(t, ctx, f)
	// observer читает удержания из отдельного соединения SQLite без записи.
	observer := openStartupHoldObserver(t, ctx, f.databasePath)
	// added содержит команду выхода с задержанным helper; err сообщает отказ её приёма.
	added, err := f.manager.AddOutput(ctx, core.AddOutputRequest{RequestID: "held-add", SessionID: parent.SessionID, DestinationID: f.destination.ID})
	if err != nil {
		t.Fatal(err)
	}
	// scope задаёт область задержанного helper; operation задаёт его точную native-операцию.
	scope, operation := barrier.entered(t, ctx)
	if scope != node.scope.ID() {
		t.Fatal("helper принадлежит другому Scope", scope)
	}
	// deadline ограничивает наблюдение локального отказа и квитанции сроком запуска менеджера; stop
	// освобождает этот срок.
	deadline, stop := context.WithTimeout(ctx, f.config.StartupTimeout)
	defer stop()
	select {
	// cause содержит наблюдённую причину отмены локальной попытки проигрывателя.
	case cause := <-causes:
		if !errors.Is(cause, ffmpeg.ErrStartupTimeout) {
			t.Fatal("не наблюдалась локальная граница готовности", cause)
		}
	case <-deadline.Done():
		t.Fatal("локальный startup не ограничен сроком", deadline.Err())
	}
	// current содержит терминальную квитанцию выхода после общего срока запуска.
	current := startupReceipt(t, deadline, f, added)
	if current.Status != core.CommandFailed || !strings.Contains(current.Failure, context.DeadlineExceeded.Error()) {
		t.Fatal("Add не завершён общим сроком запуска", current)
	}
	// output содержит отказавший выход для проверки сохранения выдачи и обязательства очистки.
	output, err := f.service.GetOutput(ctx, added.OutputID)
	if err != nil || !output.CleanupPending || !output.Issued || output.DesiredRunning {
		t.Fatal("задержанное выполнение потеряло обязательство очистки", output, err)
	}
	// wantHolds задаёт точные удержания родителя и выхода, сохраняемые до завершения позднего
	// helper.
	wantHolds := []core.ReferenceHold{
		{SessionID: session.ID, StreamID: f.stream.ID, AssetID: f.stream.Fallback.AssetID, GenerationID: f.binding.GenerationID},
		{SessionID: session.ID, OutputID: added.OutputID, StreamID: f.stream.ID, GroupID: f.binding.GroupID, ProfileID: output.Snapshot.Profile.ID, DestinationID: f.destination.ID, AssetID: f.stream.Fallback.AssetID, VariantID: f.binding.VariantID, GenerationID: f.binding.GenerationID},
	}
	// actual содержит удержания прежней сессии после срока запуска для сравнения с требуемыми.
	if actual := startupHolds(t, ctx, observer, parent.SessionID); !reflect.DeepEqual(actual, wantHolds) {
		t.Fatal("утрачены точные удержания незавершённого helper", actual, wantHolds)
	}
	// record содержит стадию задержанной операции до разрешения helper.
	if record := barrier.operation(t, scope, operation); record.State != "RESERVED" {
		t.Fatal("задержанный helper ложно объявлен завершённым", record)
	}
	// stillRunning содержит родителя после отказа отдельного выхода.
	stillRunning, err := f.service.GetSession(ctx, session.ID)
	if err != nil || stillRunning.State != core.SessionRunning {
		t.Fatal("отказ выхода остановил parent", stillRunning, err)
	}
	// actual содержит Feed родителя после отказа выхода; err сообщает отказ проверки его
	// сохранности.
	if actual, err := f.manager.Feed(stillRunning); err != nil || actual != feed {
		t.Fatal("отказ выхода изменил Feed", err)
	}
	if _, err = f.manager.StopSession(ctx, core.SessionCommand{RequestID: "held-parent-stop", SessionID: parent.SessionID}); err != nil {
		t.Fatal(err)
	}
	waitStartupCondition(t, ctx, "старый parent не завершён логически", func() bool {
		// Проверяет логическое завершение прежней сессии перед новым ручным запуском.

		// old содержит прежнюю сессию до нового ручного запуска; err сообщает отказ проверки её
		// логического завершения.
		old, err := f.service.GetSession(ctx, parent.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		return old.State.Terminal()
	})
	// next содержит новый ручной запуск при ещё задержанном прежнем helper.
	next, err := f.manager.StartSession(ctx, core.StartSessionRequest{RequestID: "after-held-helper", StreamID: f.stream.ID})
	if err != nil {
		t.Fatal("старый helper запретил новый ручной start", err)
	}
	// current содержит успешный результат нового ручного запуска.
	if current := waitReceipt(t, f, next); current.Status != core.CommandSucceeded {
		t.Fatal(current)
	}
	// fresh содержит новую сессию для проверки изоляции от прежних ресурсов.
	fresh, err := f.service.GetSession(ctx, next.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	// freshFeed сохраняет медиавход нового исполнения для сравнения после поздней очистки.
	var freshFeed *media.Router
	waitStartupCondition(t, ctx, "нет нового Feed", func() bool {
		// Проверяет доступность нового Feed и сохраняет его адрес для проверки независимости от прежнего
		// исполнения.

		freshFeed, err = f.manager.Feed(fresh)
		return err == nil
	})
	if fresh.Incarnation != session.Incarnation || freshFeed == feed {
		t.Fatal("новый start не изолирован в той же incarnation")
	}
	f.manager.mu.Lock()
	// freshNode указывает локальные ресурсы нового запуска для проверки отдельной области процессов.
	freshNode := f.manager.sessions[fresh.ID]
	f.manager.mu.Unlock()
	if freshNode == nil || freshNode.scope.ID() == scope {
		t.Fatal("новый start переиспользовал старый Scope")
	}
	// freshHolds сохраняет исходное удержание новой сессии для сравнения после поздней очистки.
	freshHolds := startupHolds(t, ctx, observer, fresh.ID)
	if len(freshHolds) != 1 || freshHolds[0] != (core.ReferenceHold{SessionID: fresh.ID, StreamID: f.stream.ID, AssetID: f.stream.Fallback.AssetID, GenerationID: f.binding.GenerationID}) {
		t.Fatal("новый start не сохранил своё точное удержание", freshHolds)
	}
	// actual содержит прежние удержания после нового запуска для проверки отсутствия
	// преждевременного освобождения.
	if actual := startupHolds(t, ctx, observer, session.ID); !reflect.DeepEqual(actual, wantHolds) {
		t.Fatal("новый start освободил старые удержания", actual)
	}
	// cleanups содержит обязательства очистки прежнего родителя и его выхода.
	cleanups, err := f.service.ListCleanups(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8, SessionID: parent.SessionID}})
	if err != nil || len(cleanups.Items) != 2 {
		t.Fatal("не сохранены оба обязательства очистки", cleanups, err)
	}
	// cleanup содержит очередное прежнее обязательство для проверки неподтверждённого точного
	// владельца.
	for _, cleanup := range cleanups.Items {
		if cleanup.Kind != core.CleanupRuntime || cleanup.Acknowledged || cleanup.Incarnation != session.Incarnation || cleanup.SessionID != session.ID || cleanup.Lifecycle != session.Lifecycle {
			t.Fatal("задержанный helper дал ложное подтверждение", cleanup)
		}
		if cleanup.OutputID == "" {
			if cleanup.Generation != 0 || cleanup.Attempt != 0 {
				t.Fatal("изменён fence очистки родителя", cleanup)
			}
		} else if cleanup.OutputID != added.OutputID || cleanup.Generation != added.OutputGeneration || cleanup.Attempt != current.Attempt {
			t.Fatal("изменён fence очистки выхода", cleanup)
		}
	}
	node.mu.Lock()
	// joined содержит признак завершения локальных ресурсов прежней сессии до освобождения барьера.
	joined := node.joined
	node.mu.Unlock()
	if joined || barrier.operation(t, scope, operation).State != "RESERVED" {
		t.Fatal("физическое завершение подтверждено до освобождения барьера")
	}
	barrier.unblock(t)
	waitStartupCondition(t, ctx, "нет физического результата позднего helper", func() bool {
		// Проверяет сохранённый физический результат задержанного gost-process-helper по записи JOINED_RESULT
		// либо CLOSED.

		// state содержит сохранённую стадию поздней native-операции после разрешения helper.
		state := barrier.operation(t, scope, operation).State
		return state == "JOINED_RESULT" || state == "CLOSED"
	})
	waitStartupCondition(t, ctx, "старые удержания не освобождены после join", func() bool {
		// Проверяет отсутствие прежних удержаний после завершения задержанного процесса.

		return len(startupHolds(t, ctx, observer, session.ID)) == 0
	})
	// cleanup содержит прежнее обязательство, чьё точное подтверждение проверяется после завершения
	// helper.
	for _, cleanup := range cleanups.Items {
		// actual содержит перечитанное обязательство очистки; err сообщает отказ чтения его
		// подтверждения.
		actual, err := f.service.GetCleanup(ctx, cleanup.ID)
		if err != nil || !actual.Acknowledged || core.CleanupFenceFor(actual) != core.CleanupFenceFor(cleanup) {
			t.Fatal("нет точного подтверждения после join", actual, err)
		}
	}
	waitStartupCondition(t, ctx, "Manager не подтвердил старый Scope", func() bool {
		// Читает заголовок прежней области и проверяет сохранение запрета регистраций и подтверждения её
		// очистки.

		// data содержит JSON заголовка прежней области; err сообщает отказ чтения файла
		// заголовка.
		data, err := os.ReadFile(filepath.Join(barrier.root, "scopes", scope, "header.json"))
		if err != nil {
			t.Fatal(err)
		}
		// header содержит признаки закрытия регистраций и подтверждения очистки прежней области.
		var header struct { /* Sealed отмечает запрет новых регистраций области; Acknowledged отмечает сохранённое подтверждение её очистки. */
			Sealed, Acknowledged bool
		}
		if err = json.Unmarshal(data, &header); err != nil {
			t.Fatal(err)
		}
		return header.Sealed && header.Acknowledged
	})
	waitStartupCondition(t, ctx, "нет полного доказательства завершения старого Scope", func() bool {
		// Проверяет полный результат Resolve прежней области без оставшихся операций и неизвестных
		// доказательств.

		// proof содержит итог проверки прежней области; err сообщает отказ разрешения её
		// состояния после завершения helper.
		proof, err := node.scope.Resolve(ctx)
		if err != nil || proof.Unknown {
			t.Fatal("не удалось сверить завершённый Scope", proof, err)
		}
		return proof.Joined && proof.Sealed && proof.Acknowledged && proof.PendingOperations == 0
	})
	// actual содержит новые удержания после поздней очистки прежней сессии.
	if actual := startupHolds(t, ctx, observer, fresh.ID); !reflect.DeepEqual(actual, freshHolds) {
		t.Fatal("поздняя очистка изменила новые удержания", actual)
	}
	// freshAfter содержит новую сессию после позднего завершения прежнего helper.
	freshAfter, err := f.service.GetSession(ctx, fresh.ID)
	if err != nil || freshAfter.State != core.SessionRunning {
		t.Fatal("поздний helper изменил новую Session", freshAfter, err)
	}
	// actual содержит Feed новой сессии после поздней очистки; err сообщает отказ проверки его
	// сохранности.
	if actual, err := f.manager.Feed(freshAfter); err != nil || actual != freshFeed {
		t.Fatal("поздний helper отозвал новый Feed", err)
	}
	t.Logf("поздний helper: scope=%s operation=%s; общий срок=%s, локальный=%s", scope, operation, f.config.StartupTimeout, f.config.Group.FFmpeg.StartupTimeout)
}
