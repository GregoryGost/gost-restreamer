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

// historyContractPage приводит страницу любого вида истории к идентификаторам, полным значениям и
// курсору.
type historyContractPage struct {
	// ids содержит идентификаторы строк в порядке страницы.
	ids []core.ID
	// values сохраняет полные значения строк для точного сравнения.
	values []any
	// cursor содержит курсор продолжения выбранного вида истории.
	cursor string
}

// historyContractFilter связывает типизированный фильтр истории с точным ожидаемым набором ID.
type historyContractFilter struct {
	// query задаёт проверяемые типизированные условия выборки.
	query core.ListQuery
	// ids содержит ожидаемые ID строк для данного фильтра.
	ids []core.ID
}

// historyContractKind объединяет чтение одного вида истории, эталоны строк и поддерживаемые фильтры.
type historyContractKind struct {
	// table называет SQL-таблицу выбранного вида истории.
	table string
	// prefix задаёт префикс ID заглушек или вариантов при общем наборе суффиксов.
	prefix string
	// values сопоставляет идентификаторам полные эталонные строки выбранного вида истории.
	values map[core.ID]any
	// get читает полное значение выбранного вида истории по ID; nil исключает события из
	// Get-проверки.
	get func(core.ID) (any, error)
	// list читает страницу выбранного вида истории в общем тестовом представлении.
	list func(core.PageRequest) (historyContractPage, error)
	// filters содержит поддерживаемые условия выборки и ожидаемые ID.
	filters []historyContractFilter
}

// Приводит типизированные Get/List и эталонные значения к общему тестовому представлению для проверки
// точных строк и курсоров.
func contractHistoryKind[T any](table string, values []T, id func(T) core.ID, get func(context.Context, core.ID) (T, error), list func(context.Context, core.PageRequest) (core.Page[T], error)) historyContractKind {
	// kind содержит адаптер и эталоны проверяемого вида истории.
	kind := historyContractKind{table: table, values: make(map[core.ID]any)}
	// value содержит очередную полную запись при приведении страницы к общему представлению.
	for _, value := range values {
		kind.values[id(value)] = value
	}
	kind.get = func(key core.ID) (any, error) {
		// Приводит типизированный Get к общему результату any, сохраняя его ошибку.
		return get(context.Background(), key)
	}
	kind.list = func(request core.PageRequest) (historyContractPage, error) {
		// Приводит типизированную страницу к общим ID и значениям для проверки точных строк и курсора.

		// page содержит выбранную страницу для проверки порядка и точных значений. err сообщает
		// об ошибке чтения типизированной страницы проверяемого вида истории.
		page, err := list(context.Background(), request)
		// result собирает общее представление типизированной страницы.
		result := historyContractPage{cursor: page.NextCursor}
		// value содержит очередную полную запись при приведении страницы к общему представлению.
		for _, value := range page.Items {
			result.ids = append(result.ids, id(value))
			result.values = append(result.values, value)
		}
		return result, err
	}
	return kind
}

// Проверяет Get/List всех поддерживаемых видов истории, точность строк, фильтры и курсоры;
// повреждённая нагрузка отклоняет всю страницу без частичных данных.
func TestHistoryServiceListsAndGetsRespectTypedFiltersAndExactRows(t *testing.T) {
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
	// outputReceipt сохраняет исходную квитанцию выхода для создания эталонных исторических команд.
	outputReceipt := outputStartReceipt(t, f, output.ID)
	// base задаёт общее время первых эталонных записей.
	base := time.Date(2020, 2, 3, 4, 5, 6, 0, time.UTC)
	// end задаёт исключительную верхнюю границу диапазона всех трёх эталонов.
	end := base.Add(2 * time.Microsecond)
	// streams собирает эталонные конфигурации потоков.
	var streams []core.Stream
	// profiles собирает эталонные полные именованные профили.
	var profiles []core.TranscodeProfile
	// groups собирает эталонные группы потоков.
	var groups []core.TranscodeGroup
	// destinations собирает эталонные назначения групп.
	var destinations []core.Destination
	// assets собирает эталонные заглушки разных состояний.
	var assets []core.FallbackAsset
	// variants собирает эталонные варианты с различающейся полной идентичностью профиля.
	var variants []core.FallbackVariant
	// sessions собирает эталонные терминальные запуски с разной остаточной очисткой.
	var sessions []core.Session
	// outputs собирает эталонные выходы исторических запусков.
	var outputs []core.SessionOutput
	// receipts собирает эталонные квитанции разных стадий.
	var receipts []core.CommandReceipt
	// cleanups собирает эталонные подтверждённые и ожидающие намерения очистки.
	var cleanups []core.CleanupIntent
	// events собирает эталонные события принятия и завершения команд.
	var events []core.Event
	// err сообщает об ошибке атомарного создания трёх комплектов исторических строк и удержаний.
	err := f.store.Write(ctx, func(r core.Repositories) error {
		// Создаёт три согласованных комплекта конфигурации, истории, квитанций, удержаний, очистки и событий с
		// заданными временами и состояниями для проверки фильтров.

		// suffix различает три эталонные строки с выбранными временами и состояниями.
		for _, suffix := range []string{"b", "c", "a"} {
			// created задаёт время очередного эталона для проверки порядка по времени и ID.
			created := base
			if suffix == "c" {
				created = created.Add(time.Microsecond)
			}
			// record задаёт идентичность, ревизию и время очередного исторического
			// комплекта.
			record := core.Record{ID: core.ID(suffix), Revision: 1, CreatedAt: created, UpdatedAt: created}
			// s содержит эталонный поток с отдельным ключом публикации.
			s := f.stream
			s.Record, s.PublicationKey = record, "history-key-"+suffix
			// err сообщает об ошибке вставки конфигурации потока.
			if err := r.Config.InsertStream(ctx, s); err != nil {
				return err
			}
			streams = append(streams, s)
			// p содержит эталонный полный профиль.
			p := f.profile
			p.Record = record
			// err сообщает об ошибке вставки исторического профиля.
			if err := r.Config.InsertProfile(ctx, p); err != nil {
				return err
			}
			profiles = append(profiles, p)
			// g содержит эталонную группу соответствующего исторического потока.
			g := f.group
			g.Record, g.StreamID = record, s.ID
			// err сообщает об ошибке вставки исторической группы.
			if err := r.Config.InsertGroup(ctx, g); err != nil {
				return err
			}
			groups = append(groups, g)
			// d содержит эталонное назначение исторической группы.
			d := f.destinations[0]
			d.Record, d.StreamID, d.GroupID = record, s.ID, g.ID
			// err сообщает об ошибке вставки исторического назначения.
			if err := r.Config.InsertDestination(ctx, d); err != nil {
				return err
			}
			destinations = append(destinations, d)
			// a содержит эталонную заглушку выбранного состояния и владельца.
			a := f.asset
			a.Record, a.OriginalPath, a.Incarnation = record, "originals/history-"+suffix+".png", "history-owner"
			a.ID = core.ID("asset-" + suffix)
			a.CurrentGenerationID = ""
			a.PendingGenerationID = ""
			if suffix == "b" {
				a.State, a.Incarnation = core.AssetPreparing, f.store.Incarnation()
				a.SourceHash, a.SizeBytes, a.SourceFormat = "", 0, core.SourceFormat{}
			}
			if suffix == "c" {
				a.State = core.AssetFailed
				a.SourceHash, a.SizeBytes, a.SourceFormat = "", 0, core.SourceFormat{}
			}
			// err сообщает об ошибке вставки исторической заглушки.
			if err := r.Artifacts.InsertAsset(ctx, a); err != nil {
				return err
			}
			assets = append(assets, a)
			// v содержит эталонный вариант с отдельным путём и полным профилем.
			v := f.variant
			v.Record, v.ManagedPath, v.Incarnation = record, core.GenerationDirectory(f.variant.GenerationID)+"/history-"+suffix+".mp4", a.Incarnation
			v.ID = core.ID("variant-" + suffix)
			v.State = core.VariantState(a.State)
			if suffix != "a" {
				v.ContentHash, v.SizeBytes = "", 0
			}
			v.Profile.VideoEncoding.GOPFrames += uint32(suffix[0])
			// key содержит ключ полной идентичности эталонного профиля варианта. canonical
			// содержит канонические байты полного эталонного профиля. e сообщает об ошибке
			// вычисления полной идентичности исторического профиля.
			key, canonical, e := core.ProfileIdentity(v.Profile)
			if e != nil {
				return e
			}
			v.ProfileKey = key
			v.CanonicalProfile = canonical
			// fileRecord задаёт отдельную идентичность принадлежащего варианта файла.
			fileRecord := record
			fileRecord.ID = core.ID("file-" + suffix)
			// err сообщает об ошибке вставки исторического принадлежащего файла.
			if err := r.Artifacts.InsertOwnedFile(ctx, core.OwnedFile{Record: fileRecord, AssetID: v.AssetID, GenerationID: v.GenerationID, ManagedPath: v.ManagedPath, Kind: core.OwnedVariant}); err != nil {
				return err
			}
			// err сообщает об ошибке вставки исторического варианта.
			if err := r.Artifacts.InsertVariant(ctx, v); err != nil {
				return err
			}
			variants = append(variants, v)
			// sess содержит эталонный терминальный запуск со снимком конфигурации.
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
			// err сообщает об ошибке вставки исторического запуска.
			if err := r.Runtime.InsertSession(ctx, sess); err != nil {
				return err
			}
			sessions = append(sessions, sess)
			// o содержит эталонный выход с заданными поколением и остаточной очисткой.
			o := output
			o.Record, o.SessionID = record, sess.ID
			o.Snapshot.Destination, o.Snapshot.Group = d, g
			o.DesiredRunning, o.Generation, o.Attempt = false, 2, 1
			o.CleanupPending, o.Issued = sess.CleanupPending, sess.CleanupPending
			// err сообщает об ошибке вставки исторического выхода.
			if err := r.Runtime.InsertOutput(ctx, o); err != nil {
				return err
			}
			outputs = append(outputs, o)
			if o.CleanupPending {
				// err сообщает об ошибке вставки удержания исторического исполнения.
				if err := r.Runtime.InsertHold(ctx, core.ReferenceHold{SessionID: sess.ID, StreamID: sess.StreamID, AssetID: f.asset.ID, GenerationID: f.variant.GenerationID}); err != nil {
					return err
				}
				// err сообщает об ошибке вставки удержания исторического исполнения.
				if err := r.Runtime.InsertHold(ctx, core.ReferenceHold{SessionID: sess.ID, OutputID: o.ID, StreamID: sess.StreamID, GroupID: o.Snapshot.Group.ID, ProfileID: o.Snapshot.Profile.ID, DestinationID: o.Snapshot.Destination.ID, AssetID: o.Snapshot.Artifact.AssetID, VariantID: o.Snapshot.Artifact.VariantID, GenerationID: o.Snapshot.Artifact.GenerationID}); err != nil {
					return err
				}
			}
			// c содержит эталонную квитанцию остановки исторического выхода.
			c := outputReceipt
			c.Record, c.RequestID, c.Incarnation, c.Status = record, "history-request-"+suffix, a.Incarnation, core.CommandPending
			// Stop после завершения Session ожидает оставшуюся очистку выхода.
			c.Kind, c.TargetID, c.SessionID = core.CommandStopOutput, o.ID, sess.ID
			c.Lifecycle, c.OutputID, c.OutputGeneration = sess.Lifecycle, o.ID, o.Generation
			// identity содержит JSON идентичности эталонной команды остановки выхода. err
			// сообщает об ошибке кодирования идентичности команды истории.
			identity, err := json.Marshal(struct {
				// Version задаёт версию формата идентичности команды.
				Version int
				// Kind задаёт вид команды в её идентичности.
				Kind core.CommandKind
				// TargetID указывает исторический выход, на который направлена команда.
				TargetID core.ID
				// DestinationIDs сохраняет выбор назначений в идентичности команды; для
				// StopOutput равен nil.
				DestinationIDs []core.ID
			}{core.MetadataVersion, c.Kind, c.TargetID, nil})
			if err != nil {
				return err
			}
			c.Identity = identity
			// digest содержит SHA-256 идентичности для fingerprint эталонной квитанции.
			digest := sha256.Sum256(c.Identity)
			c.Fingerprint = hex.EncodeToString(digest[:])
			if suffix == "b" {
				c.Status, c.Attempt = core.CommandClaimed, 1
			}
			if suffix == "c" {
				c.Status, c.Attempt = core.CommandSucceeded, 1
			}
			// err сообщает об ошибке вставки исторической квитанции.
			if err := r.Runtime.InsertReceipt(ctx, c); err != nil {
				return err
			}
			receipts = append(receipts, c)
			// cleanup содержит эталонное обязательство очистки точной прежней попытки.
			cleanup := core.CleanupIntent{Record: record, Kind: core.CleanupRuntime, StreamID: sess.StreamID, SessionID: sess.ID, OutputID: o.ID, Incarnation: sess.Incarnation, Lifecycle: sess.Lifecycle - 1, Generation: o.Generation - 1, Attempt: o.Attempt, Acknowledged: suffix == "c"}
			// err сообщает об ошибке вставки исторического намерения очистки.
			if err := r.Artifacts.InsertCleanup(ctx, cleanup); err != nil {
				return err
			}
			cleanups = append(cleanups, cleanup)
			// event содержит эталонное событие принятия либо завершения команды.
			event := core.Event{ID: record.ID, CreatedAt: created, SessionID: sess.ID, OutputID: o.ID, Kind: "command_accepted", Message: "Принята команда управления"}
			if c.Status.Terminal() {
				event.Kind, event.Message = "command_completed", "Получен результат исполнения команды"
			}
			// err сообщает об ошибке вставки исторического события.
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
	// streamKind содержит адаптер Get/List и эталоны потоков.
	streamKind := contractHistoryKind("streams", streams, func(v core.Stream) core.ID {
		// Возвращает ID Stream для сопоставления списка и точного чтения.
		return v.ID
	}, f.service.GetStream, f.service.ListStreams)
	// profileKind содержит адаптер Get/List и эталоны профилей.
	profileKind := contractHistoryKind("profiles", profiles, func(v core.TranscodeProfile) core.ID {
		// Возвращает ID TranscodeProfile для сопоставления списка и точного чтения.
		return v.ID
	}, f.service.GetProfile, f.service.ListProfiles)
	// groupKind содержит адаптер Get/List и эталоны групп.
	groupKind := contractHistoryKind("groups", groups, func(v core.TranscodeGroup) core.ID {
		// Возвращает ID TranscodeGroup для сопоставления списка и точного чтения.
		return v.ID
	}, f.service.GetGroup, f.service.ListGroups)
	// destinationKind содержит адаптер Get/List и эталоны назначений.
	destinationKind := contractHistoryKind("destinations", destinations, func(v core.Destination) core.ID {
		// Возвращает ID Destination для сопоставления списка и точного чтения.
		return v.ID
	}, f.service.GetDestination, f.service.ListDestinations)
	// assetKind содержит адаптер Get/List и эталоны заглушек.
	assetKind := contractHistoryKind("fallback_assets", assets, func(v core.FallbackAsset) core.ID {
		// Возвращает ID FallbackAsset для сопоставления списка и точного чтения.
		return v.ID
	}, f.service.GetAsset, f.service.ListAssets)
	// variantKind содержит адаптер Get/List и эталоны вариантов.
	variantKind := contractHistoryKind("fallback_variants", variants, func(v core.FallbackVariant) core.ID {
		// Возвращает ID FallbackVariant для сопоставления списка и точного чтения.
		return v.ID
	}, f.service.GetVariant, f.service.ListVariants)
	assetKind.prefix, variantKind.prefix = "asset-", "variant-"
	// sessionKind содержит адаптер Get/List и эталоны запусков.
	sessionKind := contractHistoryKind("sessions", sessions, func(v core.Session) core.ID {
		// Возвращает ID Session для сопоставления списка и точного чтения.
		return v.ID
	}, f.service.GetSession, f.service.ListSessions)
	// outputKind содержит адаптер Get/List и эталоны выходов.
	outputKind := contractHistoryKind("outputs", outputs, func(v core.SessionOutput) core.ID {
		// Возвращает ID SessionOutput для сопоставления списка и точного чтения.
		return v.ID
	}, f.service.GetOutput, f.service.ListOutputs)
	// receiptKind содержит адаптер Get/List и эталоны квитанций.
	receiptKind := contractHistoryKind("command_receipts", receipts, func(v core.CommandReceipt) core.ID {
		// Возвращает ID CommandReceipt для сопоставления списка и точного чтения.
		return v.ID
	}, f.service.GetReceipt, f.service.ListReceipts)
	// cleanupKind содержит адаптер Get/List и эталоны очистки.
	cleanupKind := contractHistoryKind("cleanup_intents", cleanups, func(v core.CleanupIntent) core.ID {
		// Возвращает ID CleanupIntent для сопоставления списка и точного чтения.
		return v.ID
	}, f.service.GetCleanup, f.service.ListCleanups)
	// eventKind содержит адаптер списка и эталоны событий без операции Get.
	eventKind := contractHistoryKind("events", events, func(v core.Event) core.ID {
		// Возвращает ID Event для проверки точных строк списка.
		return v.ID
	}, func(_ context.Context, _ core.ID) (core.Event, error) {
		// Возвращает ErrNotFound как формальную подстановку Get событий, который затем исключён из общей
		// проверки.
		return core.Event{}, core.ErrNotFound
	}, f.service.ListEvents)
	eventKind.get = nil
	// all задаёт упорядоченные суффиксы всех трёх эталонных строк.
	all := []core.ID{"a", "b", "c"}
	streamKind.filters = []historyContractFilter{{core.ListQuery{AssetID: f.asset.ID}, all}}
	groupKind.filters = []historyContractFilter{{core.ListQuery{StreamID: "a", ProfileID: f.profile.ID}, []core.ID{"a"}}, {core.ListQuery{ProfileID: f.profile.ID}, all}}
	destinationKind.filters = []historyContractFilter{{core.ListQuery{StreamID: "a", GroupID: "a"}, []core.ID{"a"}}, {core.ListQuery{StreamID: "b"}, []core.ID{"b"}}}
	assetKind.filters = []historyContractFilter{{core.ListQuery{State: "READY"}, []core.ID{"a"}}, {core.ListQuery{Incarnation: "history-owner"}, []core.ID{"a", "c"}}, {core.ListQuery{ExcludeIncarnation: "history-owner"}, []core.ID{"b"}}}
	variantKind.filters = []historyContractFilter{{core.ListQuery{AssetID: f.asset.ID, GenerationID: f.variant.GenerationID}, all}, {core.ListQuery{State: "PREPARING", Incarnation: f.store.Incarnation()}, []core.ID{"b"}}, {core.ListQuery{ExcludeIncarnation: f.store.Incarnation()}, []core.ID{"a", "c"}}}
	sessionKind.filters = []historyContractFilter{{core.ListQuery{StreamID: "a", State: "STOPPED"}, []core.ID{"a"}}, {core.ListQuery{State: "STOPPED"}, []core.ID{"a", "b"}}, {core.ListQuery{StreamID: "c", State: "FAILED"}, []core.ID{"c"}}, {core.ListQuery{PendingOnly: true}, []core.ID{"a", "b"}}, {core.ListQuery{Incarnation: "history-owner"}, []core.ID{"a", "c"}}, {core.ListQuery{ExcludeIncarnation: "history-owner"}, []core.ID{"b"}}}
	outputKind.filters = []historyContractFilter{{core.ListQuery{SessionID: "a"}, []core.ID{"a"}}, {core.ListQuery{PendingOnly: true}, []core.ID{"a", "b"}}}
	receiptKind.filters = []historyContractFilter{{core.ListQuery{SessionID: "a", OutputID: "a"}, []core.ID{"a"}}, {core.ListQuery{PendingOnly: true}, []core.ID{"a", "b"}}, {core.ListQuery{State: "SUCCEEDED", Incarnation: "history-owner"}, []core.ID{"c"}}, {core.ListQuery{ExcludeIncarnation: "history-owner"}, []core.ID{"b"}}}
	cleanupKind.filters = []historyContractFilter{{core.ListQuery{StreamID: "a", SessionID: "a", OutputID: "a"}, []core.ID{"a"}}, {core.ListQuery{PendingOnly: true}, []core.ID{"a", "b"}}, {core.ListQuery{Incarnation: "history-owner"}, []core.ID{"a", "c"}}, {core.ListQuery{ExcludeIncarnation: "history-owner"}, []core.ID{"b"}}}
	eventKind.filters = []historyContractFilter{{core.ListQuery{SessionID: "a", OutputID: "a"}, []core.ID{"a"}}}
	// kind содержит адаптер и эталоны проверяемого вида истории.
	for _, kind := range []historyContractKind{streamKind, profileKind, groupKind, destinationKind, assetKind, variantKind, sessionKind, outputKind, receiptKind, cleanupKind, eventKind} {
		t.Run(kind.table, func(t *testing.T) {
			// Проверяет выбранный вид истории: точные значения, порядок и фильтры, привязку курсора, ошибки
			// неподдерживаемых условий и отказ без частичных данных при повреждении.

			// ids добавляет префикс вида истории к ожидаемым суффиксам ID.
			ids := func(suffixes []core.ID) []core.ID {
				// Добавляет префикс выбранной таблицы к ожидаемым ID, сохраняя их порядок.

				// result собирает ожидаемые ID с префиксом выбранного вида истории.
				result := make([]core.ID, len(suffixes))
				// i указывает позицию идентификатора и полного значения в проверяемой
				// странице. suffix различает три эталонные строки с выбранными временами
				// и состояниями.
				for i, suffix := range suffixes {
					result[i] = core.ID(kind.prefix + string(suffix))
				}
				return result
			}
			// request задаёт ограниченную страницу выбранной истории и её курсор.
			request := core.PageRequest{Query: core.ListQuery{Limit: 1, From: &base, To: &end}}
			// first содержит первую строку страницы и курсор продолжения. err сообщает об
			// ошибке чтения типизированной страницы проверяемого вида истории.
			first, err := kind.list(request)
			if err != nil || !reflect.DeepEqual(first.ids, ids([]core.ID{"a"})) || first.cursor == "" {
				t.Fatalf("первая страница нарушила порядок по ID: %+v, %v", first, err)
			}
			request.Cursor, request.Query.Limit = first.cursor, 2
			// second содержит оставшиеся две строки страницы.
			second, err := kind.list(request)
			if err != nil || !reflect.DeepEqual(second.ids, ids([]core.ID{"b", "c"})) || second.cursor != "" {
				t.Fatalf("продолжение потеряло строки с одинаковым временем: %+v, %v", second, err)
			}
			// page содержит выбранную страницу для проверки порядка и точных значений.
			for _, page := range []historyContractPage{first, second} {
				// i указывает позицию идентификатора и полного значения в проверяемой
				// странице. id указывает точную историческую строку для сопоставления с
				// эталоном.
				for i, id := range page.ids {
					if !reflect.DeepEqual(page.values[i], kind.values[id]) {
						t.Fatalf("список изменил содержимое строки %s: %+v", id, page.values[i])
					}
				}
			}
			if kind.get != nil {
				// id указывает точную историческую строку для сопоставления с эталоном.
				for _, id := range ids(all) {
					// got содержит полную историческую запись точного Get. err
					// сообщает об ошибке точного чтения выбранного вида истории.
					got, err := kind.get(id)
					if err != nil || !reflect.DeepEqual(got, kind.values[id]) {
						t.Fatalf("Get(%s) изменил сохранённое значение: %+v, %v", id, got, err)
					}
				}
				// err сообщает об ошибке точного чтения выбранного вида истории.
				_, err := kind.get("missing-history-row")
				requireDomainError(t, err, core.ErrNotFound)
			}
			// filter содержит очередной поддерживаемый фильтр и ожидаемые ID.
			for _, filter := range kind.filters {
				// q содержит фильтр с общими границами времени и конечным лимитом.
				q := filter.query
				q.Limit, q.From, q.To = 10, &base, &end
				// page содержит выбранную страницу для проверки порядка и точных
				// значений. err сообщает об ошибке чтения типизированной страницы
				// проверяемого вида истории.
				page, err := kind.list(core.PageRequest{Query: q})
				if err != nil || !reflect.DeepEqual(page.ids, ids(filter.ids)) || page.cursor != "" {
					t.Fatalf("фильтр %+v: %+v, %v; нужны %v", q, page, err, filter.ids)
				}
				// misses собирает запросы с отсутствующим значением одного
				// поддерживаемого фильтра.
				var misses []core.ListQuery
				// addMiss добавляет запрос с подменой присутствующего фильтра для
				// проверки пустой страницы.
				addMiss := func(present bool, change func(*core.ListQuery)) {
					// Для присутствующего фильтра добавляет копию запроса с подменённым значением для проверки пустого
					// результата.

					if !present {
						return
					}
					// copyQuery содержит независимую копию фильтра перед подменой
					// одного значения.
					copyQuery := q
					change(&copyQuery)
					misses = append(misses, copyQuery)
				}
				addMiss(q.StreamID != "", func(q *core.ListQuery) {
					// Подменяет StreamID фильтра отсутствующим значением.
					q.StreamID = "missing-filter-value"
				})
				addMiss(q.SessionID != "", func(q *core.ListQuery) {
					// Подменяет SessionID фильтра отсутствующим значением.
					q.SessionID = "missing-filter-value"
				})
				addMiss(q.OutputID != "", func(q *core.ListQuery) {
					// Подменяет OutputID фильтра отсутствующим значением.
					q.OutputID = "missing-filter-value"
				})
				addMiss(q.AssetID != "", func(q *core.ListQuery) {
					// Подменяет AssetID фильтра отсутствующим значением.
					q.AssetID = "missing-filter-value"
				})
				addMiss(q.GroupID != "", func(q *core.ListQuery) {
					// Подменяет GroupID фильтра отсутствующим значением.
					q.GroupID = "missing-filter-value"
				})
				addMiss(q.ProfileID != "", func(q *core.ListQuery) {
					// Подменяет ProfileID фильтра отсутствующим значением.
					q.ProfileID = "missing-filter-value"
				})
				addMiss(q.Incarnation != "", func(q *core.ListQuery) {
					// Подменяет Incarnation фильтра отсутствующим значением.
					q.Incarnation = "missing-filter-value"
				})
				// miss содержит запрос с отсутствующим значением поддерживаемого
				// фильтра.
				for _, miss := range misses {
					// page содержит выбранную страницу для проверки порядка и точных
					// значений. err сообщает об ошибке чтения типизированной
					// страницы проверяемого вида истории.
					page, err := kind.list(core.PageRequest{Query: miss})
					if err != nil || len(page.ids) != 0 || page.cursor != "" {
						t.Fatalf("фильтр отсутствующего значения был проигнорирован: %+v: %+v, %v", miss, page, err)
					}
				}
			}
			// unsupported задаёт фильтр, недопустимый для выбранного вида истории.
			unsupported := core.ListQuery{Limit: 10, ProfileID: "unsupported"}
			if kind.table == "groups" {
				unsupported = core.ListQuery{Limit: 10, OutputID: "unsupported"}
			}
			_, err = kind.list(core.PageRequest{Query: unsupported})
			requireDomainError(t, err, core.ErrInvalid)
			// changedEnd задаёт изменённую верхнюю границу для проверки привязки курсора.
			changedEnd := end.Add(time.Microsecond)
			// changed содержит прежний курсор при изменённой верхней границе.
			changed := request
			changed.Query.To = &changedEnd
			_, err = kind.list(changed)
			requireDomainError(t, err, core.ErrInvalidCursor)
			// other выбирает другой вид истории для проверки привязки курсора.
			other := profileKind
			if kind.table == "profiles" {
				other = streamKind
			}
			_, err = other.list(request)
			requireDomainError(t, err, core.ErrInvalidCursor)
			// boundary задаёт повреждённую границу курсора с пустым ID либо долей
			// микросекунды.
			for _, boundary := range []core.ListBoundary{{ID: "", CreatedAt: base}, {ID: "a", CreatedAt: base.Add(time.Nanosecond)}} {
				// data содержит JSON курсора для подмены границы. err сообщает об ошибке
				// раскодирования курсора для подмены границы.
				data, err := base64.RawURLEncoding.DecodeString(first.cursor)
				if err != nil {
					t.Fatal(err)
				}
				// cursor содержит курсор продолжения выбранного вида истории.
				var cursor map[string]json.RawMessage
				// err сообщает об ошибке разбора JSON курсора.
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
				// invalid содержит запрос с искусственно повреждённой границей курсора.
				invalid := request
				invalid.Cursor = base64.RawURLEncoding.EncodeToString(data)
				_, err = kind.list(invalid)
				requireDomainError(t, err, core.ErrInvalidCursor)
			}
			// err сообщает об ошибке повреждения payload выбранной строки истории.
			if _, err := f.store.sqlDB.ExecContext(ctx, "UPDATE "+kind.table+" SET payload = ? WHERE id = ?", []byte("corrupt metadata"), kind.prefix+"c"); err != nil {
				t.Fatal(err)
			}
			if kind.get != nil {
				// err сообщает об ошибке точного чтения выбранного вида истории.
				_, err := kind.get(core.ID(kind.prefix + "c"))
				requireDomainError(t, err, core.ErrCorrupt)
			}
			request.Cursor, request.Query.Limit = "", 10
			// partial содержит результат страницы с повреждённой строкой, который должен
			// быть пустым.
			partial, err := kind.list(request)
			requireDomainError(t, err, core.ErrCorrupt)
			if len(partial.ids) != 0 || partial.cursor != "" {
				t.Fatalf("повреждённый список вернул частичные данные: %+v", partial)
			}
		})
	}
	// inUse содержит число занятых соединений после проверок истории.
	if inUse := f.store.sqlDB.Stats().InUse; inUse != 0 {
		t.Fatalf("чтение истории оставило занятые соединения: %d", inUse)
	}
}
