package storage

import (
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

type historyContractPage struct {
	ids    []core.ID
	values []any
	cursor string
}

type historyContractFilter struct {
	query core.ListQuery
	ids   []core.ID
}

type historyContractKind struct {
	table   string
	prefix  string
	values  map[core.ID]any
	get     func(core.ID) (any, error)
	list    func(core.PageRequest) (historyContractPage, error)
	filters []historyContractFilter
}

func contractHistoryKind[T any](table string, values []T, id func(T) core.ID, get func(context.Context, core.ID) (T, error), list func(context.Context, core.PageRequest) (core.Page[T], error)) historyContractKind {
	kind := historyContractKind{table: table, values: make(map[core.ID]any)}
	for _, value := range values {
		kind.values[id(value)] = value
	}
	kind.get = func(key core.ID) (any, error) { return get(context.Background(), key) }
	kind.list = func(request core.PageRequest) (historyContractPage, error) {
		page, err := list(context.Background(), request)
		result := historyContractPage{cursor: page.NextCursor}
		for _, value := range page.Items {
			result.ids = append(result.ids, id(value))
			result.values = append(result.values, value)
		}
		return result, err
	}
	return kind
}

func TestHistoryServiceListsAndGetsRespectTypedFiltersAndExactRows(t *testing.T) {
	f := newDomainFixture(t, 1)
	ctx := context.Background()
	start := startDomainSession(t, f)
	session := readSession(t, f.store, start.SessionID)
	output := readOutputs(t, f.store, start.SessionID)[0]
	outputReceipt := outputStartReceipt(t, f, output.ID)
	base := time.Date(2020, 2, 3, 4, 5, 6, 0, time.UTC)
	end := base.Add(2 * time.Microsecond)
	var streams []core.Stream
	var profiles []core.TranscodeProfile
	var groups []core.TranscodeGroup
	var destinations []core.Destination
	var assets []core.FallbackAsset
	var variants []core.FallbackVariant
	var sessions []core.Session
	var outputs []core.SessionOutput
	var receipts []core.CommandReceipt
	var cleanups []core.CleanupIntent
	var events []core.Event
	err := f.store.Write(ctx, func(r core.Repositories) error {
		for _, suffix := range []string{"b", "c", "a"} {
			created := base
			if suffix == "c" {
				created = created.Add(time.Microsecond)
			}
			record := core.Record{ID: core.ID(suffix), Revision: 1, CreatedAt: created, UpdatedAt: created}
			s := f.stream
			s.Record, s.PublicationKey = record, "history-key-"+suffix
			if err := r.Config.InsertStream(ctx, s); err != nil {
				return err
			}
			streams = append(streams, s)
			p := f.profile
			p.Record = record
			if err := r.Config.InsertProfile(ctx, p); err != nil {
				return err
			}
			profiles = append(profiles, p)
			g := f.group
			g.Record, g.StreamID = record, s.ID
			if err := r.Config.InsertGroup(ctx, g); err != nil {
				return err
			}
			groups = append(groups, g)
			d := f.destinations[0]
			d.Record, d.StreamID, d.GroupID = record, s.ID, g.ID
			if err := r.Config.InsertDestination(ctx, d); err != nil {
				return err
			}
			destinations = append(destinations, d)
			a := f.asset
			a.Record, a.OriginalPath, a.Incarnation = record, "originals/history-"+suffix+".png", "history-owner"
			a.ID = core.ID("asset-" + suffix)
			if suffix == "b" {
				a.State, a.Incarnation = core.AssetPreparing, f.store.Incarnation()
				a.SourceHash, a.SizeBytes, a.SourceFormat = "", 0, core.SourceFormat{}
			}
			if suffix == "c" {
				a.State = core.AssetFailed
				a.SourceHash, a.SizeBytes, a.SourceFormat = "", 0, core.SourceFormat{}
			}
			if err := r.Artifacts.InsertAsset(ctx, a); err != nil {
				return err
			}
			assets = append(assets, a)
			v := f.variant
			v.Record, v.ManagedPath, v.Incarnation = record, "variants/history-"+suffix+".mp4", a.Incarnation
			v.ID = core.ID("variant-" + suffix)
			v.State = core.VariantState(a.State)
			if suffix != "a" {
				v.ContentHash, v.SizeBytes = "", 0
			}
			if err := r.Artifacts.InsertVariant(ctx, v); err != nil {
				return err
			}
			variants = append(variants, v)
			sess := session
			sess.Record, sess.State, sess.Incarnation = record, core.SessionStopped, a.Incarnation
			sess.StreamID = s.ID
			sess.Snapshot = core.SessionSnapshot{Stream: s, Groups: []core.GroupSnapshot{{Group: g, Profile: f.profile}}}
			sess.EndedAt = &created
			sess.Lifecycle, sess.CleanupPending = 2, suffix != "c"
			if suffix == "c" {
				sess.State = core.SessionFailed
				sess.Failure = "Не удалось завершить остановку сессии"
			}
			if err := r.Runtime.InsertSession(ctx, sess); err != nil {
				return err
			}
			sessions = append(sessions, sess)
			o := output
			o.Record, o.SessionID = record, sess.ID
			o.Snapshot.Destination, o.Snapshot.Group = d, g
			o.DesiredRunning, o.Generation, o.Attempt = false, 2, 1
			o.CleanupPending, o.Issued = sess.CleanupPending, sess.CleanupPending
			if err := r.Runtime.InsertOutput(ctx, o); err != nil {
				return err
			}
			outputs = append(outputs, o)
			if o.CleanupPending {
				if err := r.Runtime.InsertHold(ctx, core.ReferenceHold{SessionID: sess.ID, StreamID: sess.StreamID, AssetID: f.asset.ID}); err != nil {
					return err
				}
				if err := r.Runtime.InsertHold(ctx, core.ReferenceHold{SessionID: sess.ID, OutputID: o.ID, StreamID: sess.StreamID, GroupID: o.Snapshot.Group.ID, ProfileID: o.Snapshot.Profile.ID, DestinationID: o.Snapshot.Destination.ID, AssetID: o.Snapshot.Artifact.AssetID, VariantID: o.Snapshot.Artifact.VariantID}); err != nil {
					return err
				}
			}
			c := outputReceipt
			c.Record, c.RequestID, c.Incarnation, c.Status = record, "history-request-"+suffix, a.Incarnation, core.CommandPending
			// Stop после завершения Session ожидает оставшуюся очистку выхода.
			c.Kind, c.TargetID, c.SessionID = core.CommandStopOutput, o.ID, sess.ID
			c.Lifecycle, c.OutputID, c.OutputGeneration = sess.Lifecycle, o.ID, o.Generation
			identity, err := json.Marshal(struct {
				Version        int
				Kind           core.CommandKind
				TargetID       core.ID
				DestinationIDs []core.ID
			}{core.MetadataVersion, c.Kind, c.TargetID, nil})
			if err != nil {
				return err
			}
			c.Identity = identity
			digest := sha256.Sum256(c.Identity)
			c.Fingerprint = hex.EncodeToString(digest[:])
			if suffix == "b" {
				c.Status, c.Attempt = core.CommandClaimed, 1
			}
			if suffix == "c" {
				c.Status, c.Attempt = core.CommandSucceeded, 1
			}
			if err := r.Runtime.InsertReceipt(ctx, c); err != nil {
				return err
			}
			receipts = append(receipts, c)
			cleanup := core.CleanupIntent{Record: record, Kind: core.CleanupRuntime, StreamID: sess.StreamID, SessionID: sess.ID, OutputID: o.ID, Incarnation: sess.Incarnation, Lifecycle: sess.Lifecycle - 1, Generation: o.Generation - 1, Attempt: o.Attempt, Acknowledged: suffix == "c"}
			if err := r.Artifacts.InsertCleanup(ctx, cleanup); err != nil {
				return err
			}
			cleanups = append(cleanups, cleanup)
			event := core.Event{ID: record.ID, CreatedAt: created, SessionID: sess.ID, OutputID: o.ID, Kind: "command_accepted", Message: "Принята команда управления"}
			if c.Status.Terminal() {
				event.Kind, event.Message = "command_completed", "Получен результат исполнения команды"
			}
			if err := r.Runtime.InsertEvent(ctx, event); err != nil {
				return err
			}
			events = append(events, event)
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	streamKind := contractHistoryKind("streams", streams, func(v core.Stream) core.ID { return v.ID }, f.service.GetStream, f.service.ListStreams)
	profileKind := contractHistoryKind("profiles", profiles, func(v core.TranscodeProfile) core.ID { return v.ID }, f.service.GetProfile, f.service.ListProfiles)
	groupKind := contractHistoryKind("groups", groups, func(v core.TranscodeGroup) core.ID { return v.ID }, f.service.GetGroup, f.service.ListGroups)
	destinationKind := contractHistoryKind("destinations", destinations, func(v core.Destination) core.ID { return v.ID }, f.service.GetDestination, f.service.ListDestinations)
	assetKind := contractHistoryKind("fallback_assets", assets, func(v core.FallbackAsset) core.ID { return v.ID }, f.service.GetAsset, f.service.ListAssets)
	variantKind := contractHistoryKind("fallback_variants", variants, func(v core.FallbackVariant) core.ID { return v.ID }, f.service.GetVariant, f.service.ListVariants)
	assetKind.prefix, variantKind.prefix = "asset-", "variant-"
	sessionKind := contractHistoryKind("sessions", sessions, func(v core.Session) core.ID { return v.ID }, f.service.GetSession, f.service.ListSessions)
	outputKind := contractHistoryKind("outputs", outputs, func(v core.SessionOutput) core.ID { return v.ID }, f.service.GetOutput, f.service.ListOutputs)
	receiptKind := contractHistoryKind("command_receipts", receipts, func(v core.CommandReceipt) core.ID { return v.ID }, f.service.GetReceipt, f.service.ListReceipts)
	cleanupKind := contractHistoryKind("cleanup_intents", cleanups, func(v core.CleanupIntent) core.ID { return v.ID }, f.service.GetCleanup, f.service.ListCleanups)
	eventKind := contractHistoryKind("events", events, func(v core.Event) core.ID { return v.ID }, func(_ context.Context, _ core.ID) (core.Event, error) { return core.Event{}, core.ErrNotFound }, f.service.ListEvents)
	eventKind.get = nil
	all := []core.ID{"a", "b", "c"}
	streamKind.filters = []historyContractFilter{{core.ListQuery{AssetID: f.asset.ID}, all}}
	groupKind.filters = []historyContractFilter{{core.ListQuery{StreamID: "a", ProfileID: f.profile.ID}, []core.ID{"a"}}, {core.ListQuery{ProfileID: f.profile.ID}, all}}
	destinationKind.filters = []historyContractFilter{{core.ListQuery{StreamID: "a", GroupID: "a"}, []core.ID{"a"}}, {core.ListQuery{StreamID: "b"}, []core.ID{"b"}}}
	assetKind.filters = []historyContractFilter{{core.ListQuery{State: "READY"}, []core.ID{"a"}}, {core.ListQuery{Incarnation: "history-owner"}, []core.ID{"a", "c"}}, {core.ListQuery{ExcludeIncarnation: "history-owner"}, []core.ID{"b"}}}
	variantKind.filters = []historyContractFilter{{core.ListQuery{AssetID: f.asset.ID}, all}, {core.ListQuery{State: "PREPARING", Incarnation: f.store.Incarnation()}, []core.ID{"b"}}, {core.ListQuery{ExcludeIncarnation: f.store.Incarnation()}, []core.ID{"a", "c"}}}
	sessionKind.filters = []historyContractFilter{{core.ListQuery{StreamID: "a", State: "STOPPED"}, []core.ID{"a"}}, {core.ListQuery{State: "STOPPED"}, []core.ID{"a", "b"}}, {core.ListQuery{StreamID: "c", State: "FAILED"}, []core.ID{"c"}}, {core.ListQuery{PendingOnly: true}, []core.ID{"a", "b"}}, {core.ListQuery{Incarnation: "history-owner"}, []core.ID{"a", "c"}}, {core.ListQuery{ExcludeIncarnation: "history-owner"}, []core.ID{"b"}}}
	outputKind.filters = []historyContractFilter{{core.ListQuery{SessionID: "a"}, []core.ID{"a"}}, {core.ListQuery{PendingOnly: true}, []core.ID{"a", "b"}}}
	receiptKind.filters = []historyContractFilter{{core.ListQuery{SessionID: "a", OutputID: "a"}, []core.ID{"a"}}, {core.ListQuery{PendingOnly: true}, []core.ID{"a", "b"}}, {core.ListQuery{State: "SUCCEEDED", Incarnation: "history-owner"}, []core.ID{"c"}}, {core.ListQuery{ExcludeIncarnation: "history-owner"}, []core.ID{"b"}}}
	cleanupKind.filters = []historyContractFilter{{core.ListQuery{StreamID: "a", SessionID: "a", OutputID: "a"}, []core.ID{"a"}}, {core.ListQuery{PendingOnly: true}, []core.ID{"a", "b"}}, {core.ListQuery{Incarnation: "history-owner"}, []core.ID{"a", "c"}}, {core.ListQuery{ExcludeIncarnation: "history-owner"}, []core.ID{"b"}}}
	eventKind.filters = []historyContractFilter{{core.ListQuery{SessionID: "a", OutputID: "a"}, []core.ID{"a"}}}
	for _, kind := range []historyContractKind{streamKind, profileKind, groupKind, destinationKind, assetKind, variantKind, sessionKind, outputKind, receiptKind, cleanupKind, eventKind} {
		t.Run(kind.table, func(t *testing.T) {
			ids := func(suffixes []core.ID) []core.ID {
				result := make([]core.ID, len(suffixes))
				for i, suffix := range suffixes {
					result[i] = core.ID(kind.prefix + string(suffix))
				}
				return result
			}
			request := core.PageRequest{Query: core.ListQuery{Limit: 1, From: &base, To: &end}}
			first, err := kind.list(request)
			if err != nil || !reflect.DeepEqual(first.ids, ids([]core.ID{"a"})) || first.cursor == "" {
				t.Fatalf("первая страница нарушила порядок по ID: %+v, %v", first, err)
			}
			request.Cursor, request.Query.Limit = first.cursor, 2
			second, err := kind.list(request)
			if err != nil || !reflect.DeepEqual(second.ids, ids([]core.ID{"b", "c"})) || second.cursor != "" {
				t.Fatalf("продолжение потеряло строки с одинаковым временем: %+v, %v", second, err)
			}
			for _, page := range []historyContractPage{first, second} {
				for i, id := range page.ids {
					if !reflect.DeepEqual(page.values[i], kind.values[id]) {
						t.Fatalf("список изменил содержимое строки %s: %+v", id, page.values[i])
					}
				}
			}
			if kind.get != nil {
				for _, id := range ids(all) {
					got, err := kind.get(id)
					if err != nil || !reflect.DeepEqual(got, kind.values[id]) {
						t.Fatalf("Get(%s) изменил сохранённое значение: %+v, %v", id, got, err)
					}
				}
				_, err := kind.get("missing-history-row")
				requireDomainError(t, err, core.ErrNotFound)
			}
			for _, filter := range kind.filters {
				q := filter.query
				q.Limit, q.From, q.To = 10, &base, &end
				page, err := kind.list(core.PageRequest{Query: q})
				if err != nil || !reflect.DeepEqual(page.ids, ids(filter.ids)) || page.cursor != "" {
					t.Fatalf("фильтр %+v: %+v, %v; нужны %v", q, page, err, filter.ids)
				}
				var misses []core.ListQuery
				addMiss := func(present bool, change func(*core.ListQuery)) {
					if !present {
						return
					}
					copyQuery := q
					change(&copyQuery)
					misses = append(misses, copyQuery)
				}
				addMiss(q.StreamID != "", func(q *core.ListQuery) { q.StreamID = "missing-filter-value" })
				addMiss(q.SessionID != "", func(q *core.ListQuery) { q.SessionID = "missing-filter-value" })
				addMiss(q.OutputID != "", func(q *core.ListQuery) { q.OutputID = "missing-filter-value" })
				addMiss(q.AssetID != "", func(q *core.ListQuery) { q.AssetID = "missing-filter-value" })
				addMiss(q.GroupID != "", func(q *core.ListQuery) { q.GroupID = "missing-filter-value" })
				addMiss(q.ProfileID != "", func(q *core.ListQuery) { q.ProfileID = "missing-filter-value" })
				addMiss(q.Incarnation != "", func(q *core.ListQuery) { q.Incarnation = "missing-filter-value" })
				for _, miss := range misses {
					page, err := kind.list(core.PageRequest{Query: miss})
					if err != nil || len(page.ids) != 0 || page.cursor != "" {
						t.Fatalf("фильтр отсутствующего значения был проигнорирован: %+v: %+v, %v", miss, page, err)
					}
				}
			}
			unsupported := core.ListQuery{Limit: 10, ProfileID: "unsupported"}
			if kind.table == "groups" {
				unsupported = core.ListQuery{Limit: 10, OutputID: "unsupported"}
			}
			_, err = kind.list(core.PageRequest{Query: unsupported})
			requireDomainError(t, err, core.ErrInvalid)
			changedEnd := end.Add(time.Microsecond)
			changed := request
			changed.Query.To = &changedEnd
			_, err = kind.list(changed)
			requireDomainError(t, err, core.ErrInvalidCursor)
			other := profileKind
			if kind.table == "profiles" {
				other = streamKind
			}
			_, err = other.list(request)
			requireDomainError(t, err, core.ErrInvalidCursor)
			for _, boundary := range []core.ListBoundary{{ID: "", CreatedAt: base}, {ID: "a", CreatedAt: base.Add(time.Nanosecond)}} {
				data, err := base64.RawURLEncoding.DecodeString(first.cursor)
				if err != nil {
					t.Fatal(err)
				}
				var cursor map[string]json.RawMessage
				if err := json.Unmarshal(data, &cursor); err != nil {
					t.Fatal(err)
				}
				cursor["Boundary"], err = json.Marshal(boundary)
				if err != nil {
					t.Fatal(err)
				}
				data, err = json.Marshal(cursor)
				if err != nil {
					t.Fatal(err)
				}
				invalid := request
				invalid.Cursor = base64.RawURLEncoding.EncodeToString(data)
				_, err = kind.list(invalid)
				requireDomainError(t, err, core.ErrInvalidCursor)
			}
			if _, err := f.store.sqlDB.ExecContext(ctx, "UPDATE "+kind.table+" SET payload = ? WHERE id = ?", []byte("corrupt metadata"), kind.prefix+"c"); err != nil {
				t.Fatal(err)
			}
			if kind.get != nil {
				_, err := kind.get(core.ID(kind.prefix + "c"))
				requireDomainError(t, err, core.ErrCorrupt)
			}
			request.Cursor, request.Query.Limit = "", 10
			partial, err := kind.list(request)
			requireDomainError(t, err, core.ErrCorrupt)
			if len(partial.ids) != 0 || partial.cursor != "" {
				t.Fatalf("повреждённый список вернул частичные данные: %+v", partial)
			}
		})
	}
	if inUse := f.store.sqlDB.Stats().InUse; inUse != 0 {
		t.Fatalf("чтение истории оставило занятые соединения: %d", inUse)
	}
}
