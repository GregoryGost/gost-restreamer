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

type startupHelperBarrier struct {
	root, marker, wrapper string
	release               *os.File
	once                  sync.Once
}

func newStartupHelperBarrier(t *testing.T) *startupHelperBarrier {
	t.Helper()
	directory := t.TempDir()
	helper := filepath.Join(directory, "gost-process-helper")
	args := []string{"build", "-o", helper}
	if directory := os.Getenv("GOST_TEST_PROCESS_HELPER_COVERDIR"); directory != "" {
		t.Setenv("GOCOVERDIR", directory)
		args = append(args, "-cover", "-covermode=atomic", "-coverpkg=github.com/GregoryGost/gost-restreamer/...")
	}
	command := exec.Command("go", append(args, "../../cmd/gost-process-helper")...)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("helper: %v %s", err, output)
	}
	fifo := filepath.Join(directory, "release")
	if err := syscall.Mkfifo(fifo, 0600); err != nil {
		t.Fatal(err)
	}
	release, err := os.OpenFile(fifo, os.O_RDWR, 0600)
	if err != nil {
		t.Fatal(err)
	}
	b := &startupHelperBarrier{root: filepath.Join(directory, "processes"), marker: filepath.Join(directory, "entered"), wrapper: filepath.Join(directory, "helper-barrier"), release: release}
	quote := func(s string) string { return "'" + strings.ReplaceAll(s, "'", "'\"'\"'") + "'" }
	// Оболочка сохраняет lifeline и exec передаёт владение настоящему helper.
	script := "#!/bin/sh\nprintf '%s %s\\n' \"$3\" \"$4\" >" + quote(b.marker) + "\nIFS= read -r release <" + quote(fifo) + "\nexec " + quote(helper) + " \"$@\"\n"
	if err = os.WriteFile(b.wrapper, []byte(script), 0700); err != nil {
		_ = release.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		b.unblock(t)
		if err := release.Close(); err != nil {
			t.Error(err)
		}
	})
	return b
}

func (b *startupHelperBarrier) unblock(t *testing.T) {
	t.Helper()
	b.once.Do(func() {
		if _, err := b.release.WriteString("release\n"); err != nil {
			t.Error(err)
		}
	})
}

func (b *startupHelperBarrier) configure(t *testing.T, c *Config) {
	t.Helper()
	registry, err := processowner.New(processowner.Config{Root: b.root, RuntimeRoot: c.Group.FFmpeg.BaseDir, HelperPath: b.wrapper, Limits: processowner.Limits{MaxOperations: 512, MaxRecordBytes: 1 << 20, MaxCommandArgs: 128, MaxCommandBytes: 16384, MaxExtraFiles: 8, PageSize: 32}, GracePeriod: 20 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	c.Processes = registry
	t.Cleanup(func() {
		if err := registry.Close(); err != nil {
			t.Error(err)
		}
	})
}

func waitStartupCondition(t *testing.T, ctx context.Context, description string, condition func() bool) {
	t.Helper()
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

func (b *startupHelperBarrier) entered(t *testing.T, ctx context.Context) (string, string) {
	t.Helper()
	var fields []string
	waitStartupCondition(t, ctx, "helper не достиг барьера", func() bool {
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

type startupOperation struct {
	ScopeID, ID, Kind, State string
}

func (b *startupHelperBarrier) operation(t *testing.T, scope, operation string) startupOperation {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(b.root, "scopes", scope, "operations", operation+".json"))
	if err != nil {
		t.Fatal(err)
	}
	var record startupOperation
	if err = json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if record.ScopeID != scope || record.ID != operation || record.Kind != "native" {
		t.Fatal("барьер не соответствует точной native-операции", record)
	}
	return record
}

func startupParent(t *testing.T, ctx context.Context, f managerFixture) (core.CommandReceipt, core.Session, *media.Router, *sessionNode) {
	t.Helper()
	start, err := f.manager.StartSession(ctx, core.StartSessionRequest{RequestID: "barrier-parent", StreamID: f.stream.ID})
	if err != nil {
		t.Fatal(err)
	}
	if current := waitReceipt(t, f, start); current.Status != core.CommandSucceeded {
		t.Fatal(current)
	}
	session, err := f.service.GetSession(ctx, start.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var feed *media.Router
	waitStartupCondition(t, ctx, "нет Feed родителя", func() bool {
		feed, err = f.manager.Feed(session)
		return err == nil
	})
	f.manager.mu.Lock()
	node := f.manager.sessions[start.SessionID]
	f.manager.mu.Unlock()
	return start, session, feed, node
}

func startupReceipt(t *testing.T, ctx context.Context, f managerFixture, receipt core.CommandReceipt) core.CommandReceipt {
	t.Helper()
	var current core.CommandReceipt
	waitStartupCondition(t, ctx, "команда не ограничена сроком запуска", func() bool {
		var err error
		current, err = f.service.GetReceipt(ctx, receipt.ID)
		if err != nil {
			t.Fatal(err)
		}
		return current.Status.Terminal()
	})
	return current
}

func TestManagerStartupHelperReleasedBeforeDeadlineBecomesReady(t *testing.T) {
	barrier := newStartupHelperBarrier(t)
	f := newManagerFixture(t, false, func(c *Config) {
		c.Group.FFmpeg.StartupTimeout = c.StartupTimeout
		barrier.configure(t, c)
	})
	defer barrier.unblock(t)
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	parent, _, _, node := startupParent(t, ctx, f)
	added, err := f.manager.AddOutput(ctx, core.AddOutputRequest{RequestID: "released-add", SessionID: parent.SessionID, DestinationID: f.destination.ID})
	if err != nil {
		t.Fatal(err)
	}
	scope, operation := barrier.entered(t, ctx)
	if scope != node.scope.ID() || barrier.operation(t, scope, operation).State != "RESERVED" {
		t.Fatal("helper не остановлен до native spawn")
	}
	barrier.unblock(t)
	deadline, stop := context.WithTimeout(ctx, f.config.StartupTimeout)
	defer stop()
	if current := startupReceipt(t, deadline, f, added); current.Status != core.CommandSucceeded {
		t.Fatal("освобождённый helper не дал готовность", current)
	}
	node.mu.Lock()
	member := node.members[added.OutputID]
	node.mu.Unlock()
	if member == nil {
		t.Fatal("нет ссылки готового выхода")
	}
	member.mu.Lock()
	ref := member.reference
	member.mu.Unlock()
	if ref == nil || ref.Group().Ready() != nil {
		t.Fatal("Add завершён до фактической готовности fallback")
	}
	sub, err := ref.Router().Subscribe(f.config.Publisher.Queue)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = sub.Close() }()
	seen := map[media.Track]bool{}
	for len(seen) < 2 {
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

func openStartupHoldObserver(t *testing.T, ctx context.Context, path string) *sql.DB {
	t.Helper()
	dsn := url.URL{Scheme: "file", Path: path, RawQuery: url.Values{"mode": {"ro"}, "_query_only": {"on"}, "_busy_timeout": {"1000"}}.Encode()}
	db, err := sql.Open("sqlite3", dsn.String())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(1)
	t.Cleanup(func() {
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	})
	var queryOnly int
	if err = db.QueryRowContext(ctx, "PRAGMA query_only").Scan(&queryOnly); err != nil || queryOnly != 1 {
		t.Fatal("наблюдатель SQLite допускает запись", queryOnly, err)
	}
	return db
}

func startupHolds(t *testing.T, ctx context.Context, db *sql.DB, session core.ID) []core.ReferenceHold {
	t.Helper()
	rows, err := db.QueryContext(ctx, `SELECT session_id, '', stream_id, '', '', '', asset_id, '', generation_id FROM session_holds WHERE session_id = ?
UNION ALL SELECT session_id, output_id, stream_id, group_id, profile_id, destination_id, asset_id, variant_id, generation_id FROM output_holds WHERE session_id = ? ORDER BY 2`, session, session)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = rows.Close() }()
	var result []core.ReferenceHold
	for rows.Next() {
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

func TestManagerStartupDeadlineRetainsLateHelperUntilPhysicalJoin(t *testing.T) {
	barrier := newStartupHelperBarrier(t)
	causes := make(chan error, 1)
	f := newManagerFixture(t, false, func(c *Config) {
		barrier.configure(t, c)
		original := c.Group.Player
		c.Group.Player = func(ctx context.Context, binary string, file *os.File, attempt ffmpeg.Attempt) (ffmpeg.Command, error) {
			context.AfterFunc(ctx, func() { causes <- context.Cause(ctx) })
			return original(ctx, binary, file, attempt)
		}
	})
	defer barrier.unblock(t)
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	parent, session, feed, node := startupParent(t, ctx, f)
	observer := openStartupHoldObserver(t, ctx, f.databasePath)
	added, err := f.manager.AddOutput(ctx, core.AddOutputRequest{RequestID: "held-add", SessionID: parent.SessionID, DestinationID: f.destination.ID})
	if err != nil {
		t.Fatal(err)
	}
	scope, operation := barrier.entered(t, ctx)
	if scope != node.scope.ID() {
		t.Fatal("helper принадлежит другому Scope", scope)
	}
	deadline, stop := context.WithTimeout(ctx, f.config.StartupTimeout)
	defer stop()
	select {
	case cause := <-causes:
		if !errors.Is(cause, ffmpeg.ErrStartupTimeout) {
			t.Fatal("не наблюдалась локальная граница готовности", cause)
		}
	case <-deadline.Done():
		t.Fatal("локальный startup не ограничен сроком", deadline.Err())
	}
	current := startupReceipt(t, deadline, f, added)
	if current.Status != core.CommandFailed || !strings.Contains(current.Failure, context.DeadlineExceeded.Error()) {
		t.Fatal("Add не завершён общим сроком запуска", current)
	}
	output, err := f.service.GetOutput(ctx, added.OutputID)
	if err != nil || !output.CleanupPending || !output.Issued || output.DesiredRunning {
		t.Fatal("задержанное выполнение потеряло обязательство очистки", output, err)
	}
	wantHolds := []core.ReferenceHold{
		{SessionID: session.ID, StreamID: f.stream.ID, AssetID: f.stream.Fallback.AssetID, GenerationID: f.binding.GenerationID},
		{SessionID: session.ID, OutputID: added.OutputID, StreamID: f.stream.ID, GroupID: f.binding.GroupID, ProfileID: output.Snapshot.Profile.ID, DestinationID: f.destination.ID, AssetID: f.stream.Fallback.AssetID, VariantID: f.binding.VariantID, GenerationID: f.binding.GenerationID},
	}
	if actual := startupHolds(t, ctx, observer, parent.SessionID); !reflect.DeepEqual(actual, wantHolds) {
		t.Fatal("утрачены точные удержания незавершённого helper", actual, wantHolds)
	}
	if record := barrier.operation(t, scope, operation); record.State != "RESERVED" {
		t.Fatal("задержанный helper ложно объявлен завершённым", record)
	}
	stillRunning, err := f.service.GetSession(ctx, session.ID)
	if err != nil || stillRunning.State != core.SessionRunning {
		t.Fatal("отказ выхода остановил parent", stillRunning, err)
	}
	if actual, err := f.manager.Feed(stillRunning); err != nil || actual != feed {
		t.Fatal("отказ выхода изменил Feed", err)
	}
	if _, err = f.manager.StopSession(ctx, core.SessionCommand{RequestID: "held-parent-stop", SessionID: parent.SessionID}); err != nil {
		t.Fatal(err)
	}
	waitStartupCondition(t, ctx, "старый parent не завершён логически", func() bool {
		old, err := f.service.GetSession(ctx, parent.SessionID)
		if err != nil {
			t.Fatal(err)
		}
		return old.State.Terminal()
	})
	next, err := f.manager.StartSession(ctx, core.StartSessionRequest{RequestID: "after-held-helper", StreamID: f.stream.ID})
	if err != nil {
		t.Fatal("старый helper запретил новый ручной start", err)
	}
	if current := waitReceipt(t, f, next); current.Status != core.CommandSucceeded {
		t.Fatal(current)
	}
	fresh, err := f.service.GetSession(ctx, next.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	var freshFeed *media.Router
	waitStartupCondition(t, ctx, "нет нового Feed", func() bool {
		freshFeed, err = f.manager.Feed(fresh)
		return err == nil
	})
	if fresh.Incarnation != session.Incarnation || freshFeed == feed {
		t.Fatal("новый start не изолирован в той же incarnation")
	}
	f.manager.mu.Lock()
	freshNode := f.manager.sessions[fresh.ID]
	f.manager.mu.Unlock()
	if freshNode == nil || freshNode.scope.ID() == scope {
		t.Fatal("новый start переиспользовал старый Scope")
	}
	freshHolds := startupHolds(t, ctx, observer, fresh.ID)
	if len(freshHolds) != 1 || freshHolds[0] != (core.ReferenceHold{SessionID: fresh.ID, StreamID: f.stream.ID, AssetID: f.stream.Fallback.AssetID, GenerationID: f.binding.GenerationID}) {
		t.Fatal("новый start не сохранил своё точное удержание", freshHolds)
	}
	if actual := startupHolds(t, ctx, observer, session.ID); !reflect.DeepEqual(actual, wantHolds) {
		t.Fatal("новый start освободил старые удержания", actual)
	}
	cleanups, err := f.service.ListCleanups(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8, SessionID: parent.SessionID}})
	if err != nil || len(cleanups.Items) != 2 {
		t.Fatal("не сохранены оба обязательства очистки", cleanups, err)
	}
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
	joined := node.joined
	node.mu.Unlock()
	if joined || barrier.operation(t, scope, operation).State != "RESERVED" {
		t.Fatal("физическое завершение подтверждено до освобождения барьера")
	}
	barrier.unblock(t)
	waitStartupCondition(t, ctx, "нет физического результата позднего helper", func() bool {
		state := barrier.operation(t, scope, operation).State
		return state == "JOINED_RESULT" || state == "CLOSED"
	})
	waitStartupCondition(t, ctx, "старые удержания не освобождены после join", func() bool {
		return len(startupHolds(t, ctx, observer, session.ID)) == 0
	})
	for _, cleanup := range cleanups.Items {
		actual, err := f.service.GetCleanup(ctx, cleanup.ID)
		if err != nil || !actual.Acknowledged || core.CleanupFenceFor(actual) != core.CleanupFenceFor(cleanup) {
			t.Fatal("нет точного подтверждения после join", actual, err)
		}
	}
	waitStartupCondition(t, ctx, "Manager не подтвердил старый Scope", func() bool {
		data, err := os.ReadFile(filepath.Join(barrier.root, "scopes", scope, "header.json"))
		if err != nil {
			t.Fatal(err)
		}
		var header struct{ Sealed, Acknowledged bool }
		if err = json.Unmarshal(data, &header); err != nil {
			t.Fatal(err)
		}
		return header.Sealed && header.Acknowledged
	})
	waitStartupCondition(t, ctx, "нет полного доказательства завершения старого Scope", func() bool {
		proof, err := node.scope.Resolve(ctx)
		if err != nil || proof.Unknown {
			t.Fatal("не удалось сверить завершённый Scope", proof, err)
		}
		return proof.Joined && proof.Sealed && proof.Acknowledged && proof.PendingOperations == 0
	})
	if actual := startupHolds(t, ctx, observer, fresh.ID); !reflect.DeepEqual(actual, freshHolds) {
		t.Fatal("поздняя очистка изменила новые удержания", actual)
	}
	freshAfter, err := f.service.GetSession(ctx, fresh.ID)
	if err != nil || freshAfter.State != core.SessionRunning {
		t.Fatal("поздний helper изменил новую Session", freshAfter, err)
	}
	if actual, err := f.manager.Feed(freshAfter); err != nil || actual != freshFeed {
		t.Fatal("поздний helper отозвал новый Feed", err)
	}
	t.Logf("поздний helper: scope=%s operation=%s; общий срок=%s, локальный=%s", scope, operation, f.config.StartupTimeout, f.config.Group.FFmpeg.StartupTimeout)
}
