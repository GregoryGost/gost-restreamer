package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// frozenEncodingState хранит независимые эталоны прежней заполненной базы, снимков и управления для
// проверки совместимости.
type frozenEncodingState struct {
	// Baseline указывает исходную ревизию, зафиксированную в происхождении эталонной базы.
	Baseline string
	// StreamID указывает эталонный поток. ProfileID указывает эталонный полный профиль. AssetID
	// указывает эталонную стабильную заглушку. VariantID указывает эталонный подготовленный вариант.
	// TerminalSessionID указывает прежний терминальный запуск с незавершённой очисткой.
	// ActiveSessionID указывает прежний активный запуск, подлежащий прерыванию при открытии.
	// CancelledGenerationID указывает эталонное терминальное поколение неуспешной подготовки.
	StreamID, ProfileID, AssetID, VariantID, TerminalSessionID, ActiveSessionID, CancelledGenerationID core.ID
	// StartRequest сохраняет исходный запрос для проверки повтора прежнего RequestID.
	StartRequest core.StartSessionRequest
	// Receipt сохраняет точную исходную квитанцию принятого запуска.
	Receipt core.CommandReceipt
	// Cursor сохраняет прежний курсор списка профилей.
	Cursor string
	// CursorIDs содержит ожидаемые ID продолжения прежнего курсора.
	CursorIDs []core.ID
	// Immutable сохраняет эталонные байты payload по таблицам и ID.
	Immutable map[string]map[string][]byte
	// Snapshots сохраняет точные байты исторических снимков по виду и ID владельца.
	Snapshots map[string][]byte
	// Holds сохраняет все колонки эталонных удержаний сессий и выходов.
	Holds map[string][][]string
}

// Проверяет происхождение и SHA-256 закреплённой заполненной БД, копирует её во временный каталог и
// открывает с рабочими миграциями и новым Service.
func openFrozenEncodingState(t *testing.T) (*Store, *core.Service, frozenEncodingState, string) {
	t.Helper()
	// root задаёт каталог независимой прежней заполненной базы и её происхождения.
	root := "testdata/encoding_legacy_v1"
	// b содержит прочитанные байты очередного эталонного файла. err сообщает об ошибке чтения
	// независимого эталона прежней заполненной базы.
	b, err := os.ReadFile(filepath.Join(root, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	// state содержит эталонные идентификаторы, снимки, квитанцию и удержания прежней базы.
	var state frozenEncodingState
	if err = json.Unmarshal(b, &state); err != nil {
		t.Fatal(err)
	}
	// provenance содержит исходную ревизию и ожидаемый SHA-256 файла эталонной базы.
	var provenance map[string]string
	b, err = os.ReadFile(filepath.Join(root, "provenance.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err = json.Unmarshal(b, &provenance); err != nil {
		t.Fatal(err)
	}
	b, err = os.ReadFile(filepath.Join(root, "state.sqlite"))
	if err != nil {
		t.Fatal(err)
	}
	// sum содержит SHA-256 прочитанных байтов эталонной базы.
	sum := sha256.Sum256(b)
	if state.Baseline != "9ebb0d276a50bd655e870a32c2074168377cb038" || provenance["baseline"] != state.Baseline || hex.EncodeToString(sum[:]) != provenance["database_sha256"] {
		t.Fatal("Происхождение замороженной БД не совпадает")
	}
	// path задаёт путь отдельной рабочей копии эталонной базы.
	path := filepath.Join(t.TempDir(), "legacy.sqlite")
	if err = os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	// s содержит открытый Store рабочей копии эталонной базы.
	s, err := Open(context.Background(), path, testOptions())
	if err != nil {
		t.Fatalf("Открыть заполненную БД прежней версией профилей: %v", err)
	}
	t.Cleanup(func() {
		// Закрывает первоначально открытый Store копии эталонной БД после завершения теста.

		// err сообщает об ошибке закрытия тестового пула SQLite.
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	// svc предоставляет доменные операции текущего владельца временного Store.
	svc, err := core.NewService(s, s.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	return s, svc, state, path
}

// Сверяет точные байты исторических снимков Session и Output с закреплёнными эталонами.
func assertFrozenEncodingSnapshots(t *testing.T, svc *core.Service, state frozenEncodingState) {
	t.Helper()
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// key указывает вид и ID эталонного исторического снимка. want содержит точные ожидаемые байты
	// исторической строки либо снимка.
	for key, want := range state.Snapshots {
		// got собирает фактически прочитанные байты либо колонки сохраняемой истории.
		var got []byte
		// err сообщает об ошибке чтения и кодирования исторического снимка.
		var err error
		// id указывает строку исторического снимка или неизменяемой таблицы. ok показывает
		// совпадение префикса вида исторического снимка.
		if id, ok := strings.CutPrefix(key, "session:"); ok {
			// s принимает историческую сессию для кодирования её снимка.
			var s core.Session
			s, err = svc.GetSession(ctx, core.ID(id))
			if err == nil {
				got, err = core.EncodeSessionSnapshot(s.Snapshot)
			}
		} else /* id указывает строку исторического снимка или неизменяемой таблицы. ok показывает совпадение префикса вида исторического снимка. */ if id, ok := strings.CutPrefix(key, "output:"); ok {
			// o принимает исторический выход для кодирования его снимка.
			var o core.SessionOutput
			o, err = svc.GetOutput(ctx, core.ID(id))
			if err == nil {
				got, err = core.EncodeOutputSnapshot(o.Snapshot)
			}
		} else {
			t.Fatalf("Неизвестный вид снимка %q", key)
		}
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("Изменён исходный снимок %s: %v", key, err)
		}
	}
}

// Сверяет точные нагрузки неизменяемых строк и оба набора удержаний с эталоном после восстановления.
func assertFrozenEncodingRows(t *testing.T, s *Store, state frozenEncodingState) {
	t.Helper()
	// table называет проверяемую таблицу исторических данных либо удержаний.
	for _, table := range []string{"streams", "profiles", "groups", "destinations", "fallback_assets", "fallback_generations", "fallback_variants", "owned_paths", "events", "cleanup_intents", "command_receipts"} {
		if len(state.Immutable[table]) == 0 {
			t.Fatalf("В эталоне нет строк %s", table)
		}
		// id указывает строку исторического снимка или неизменяемой таблицы. want содержит
		// точные ожидаемые байты исторической строки либо снимка.
		for id, want := range state.Immutable[table] {
			// got собирает фактически прочитанные байты либо колонки сохраняемой истории.
			var got []byte
			// err сообщает об ошибке чтения точных байтов исторической строки.
			if err := s.sqlDB.QueryRow("SELECT payload FROM "+table+" WHERE id = ?", id).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("Открытие переписало прежние байты %s/%s", table, id)
			}
		}
	}
	// table называет проверяемую таблицу исторических данных либо удержаний.
	for _, table := range []string{"session_holds", "output_holds"} {
		// rows предоставляет все строки проверяемой таблицы удержаний в заданном порядке. err
		// сообщает об ошибке чтения всех колонок сохраняемых удержаний.
		rows, err := s.sqlDB.Query("SELECT * FROM " + table + " ORDER BY 1")
		if err != nil {
			t.Fatal(err)
		}
		// columns содержит имена колонок таблицы удержаний для точного чтения каждой строки.
		columns, err := rows.Columns()
		if err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		// got собирает фактически прочитанные байты либо колонки сохраняемой истории.
		var got [][]string
		for rows.Next() {
			// values принимает все строковые значения очередного удержания.
			values := make([]string, len(columns))
			// targets содержит адреса колонок очередного удержания для SQL Scan.
			targets := make([]any, len(columns))
			// i указывает позицию колонки удержания в строке и массиве адресов.
			for i := range values {
				targets[i] = &values[i]
			}
			if err = rows.Scan(targets...); err != nil {
				_ = rows.Close()
				t.Fatal(err)
			}
			got = append(got, values)
		}
		if err = rows.Err(); err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		if err = rows.Close(); err != nil {
			t.Fatal(err)
		}
		if len(got) != 2 || !reflect.DeepEqual(got, state.Holds[table]) {
			t.Fatalf("Recovery изменил точные удержания %s: %+v", table, got)
		}
	}
}

// Проверяет повторное открытие заполненной прежней БД, восстановление управления без переписывания
// эталонных данных, повтор квитанции и совместимость сохранённого курсора.
func TestFrozenLegacyDatabaseOpenRecoveryReplayAndCursor(t *testing.T) {
	// s содержит открытый Store рабочей копии эталонной базы. svc предоставляет доменные операции
	// текущего владельца временного Store. state содержит эталонные идентификаторы, снимки,
	// квитанцию и удержания прежней базы. path задаёт путь отдельной рабочей копии эталонной базы.
	s, svc, state, path := openFrozenEncodingState(t)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// cycle выбирает проверку первоначального либо повторного открытия рабочей копии.
	for cycle := range 2 {
		assertFrozenEncodingRows(t, s, state)
		assertFrozenEncodingSnapshots(t, svc, state)
		// terminal содержит прежний остановленный запуск с сохраняемой очисткой. err сообщает об
		// ошибке чтения сохраняемого исторического запуска.
		terminal, err := svc.GetSession(ctx, state.TerminalSessionID)
		if err != nil || terminal.State != core.SessionStopped || !terminal.CleanupPending {
			t.Fatalf("Потеряна терминальная история: %+v, %v", terminal, err)
		}
		// recovered содержит прежний активный запуск после прерывания при открытии.
		recovered, err := svc.GetSession(ctx, state.ActiveSessionID)
		if err != nil || recovered.State != core.SessionInterrupted || !recovered.CleanupPending || recovered.EndedAt == nil {
			t.Fatalf("Выданный запуск восстановлен неверно: %+v, %v", recovered, err)
		}
		// variant содержит точный опубликованный вариант тестового поколения.
		variant, err := svc.GetVariant(ctx, state.VariantID)
		if err != nil || variant.State != core.VariantReady {
			t.Fatalf("Старый вариант перестал читаться: %+v, %v", variant, err)
		}
		// gen содержит опубликованное поколение эталонного варианта.
		gen, err := svc.GetGeneration(ctx, variant.GenerationID)
		if err != nil || gen.State != core.PreparationPublished || len(gen.RequiredProfiles) != 1 {
			t.Fatalf("Потеряно PUBLISHED поколение: %+v, %v", gen, err)
		}
		// failed содержит прежнее терминальное поколение неуспешной подготовки.
		failed, err := svc.GetGeneration(ctx, state.CancelledGenerationID)
		if err != nil || !failed.State.Terminal() || failed.State == core.PreparationPublished {
			t.Fatalf("Потеряна история отказа подготовки: %+v, %v", failed, err)
		}
		// asset содержит заглушку для проверки неизменности текущего поколения.
		asset, err := svc.GetAsset(ctx, state.AssetID)
		if err != nil || asset.CurrentGenerationID != gen.ID {
			t.Fatalf("Recovery подменил current: %+v, %v", asset, err)
		}
		// replay содержит повторно возвращённую прежнюю квитанцию запуска.
		replay, err := svc.StartSession(ctx, state.StartRequest)
		if err != nil || !reflect.DeepEqual(replay, state.Receipt) {
			t.Fatalf("Прежняя квитанция изменилась при replay: %+v, %v", replay, err)
		}
		// conflict содержит прежний RequestID с другим выбором назначений.
		conflict := state.StartRequest
		conflict.DestinationIDs = []core.ID{"other-destination"}
		if _, err = svc.StartSession(ctx, conflict); !errors.Is(err, core.ErrConflict) {
			t.Fatalf("Повтор RequestID принял другое назначение: %v", err)
		}
		// page содержит страницу профилей либо очистки для проверки прежнего контракта.
		page, err := svc.ListProfiles(ctx, core.PageRequest{Query: core.ListQuery{Limit: 100}, Cursor: state.Cursor})
		if err != nil {
			t.Fatalf("Старый cursor отвергнут: %v", err)
		}
		// ids собирает фактические ID продолжения прежнего курсора.
		var ids []core.ID
		// p содержит полный профиль, проверяемый на сохранение идентичности и явных параметров.
		for _, p := range page.Items {
			ids = append(ids, p.ID)
		}
		if !reflect.DeepEqual(ids, state.CursorIDs) || page.NextCursor != "" {
			t.Fatalf("Старый cursor потерял порядок: %v", ids)
		}
		if err = svc.Recover(ctx); err != nil {
			t.Fatal(err)
		}
		if cycle == 0 {
			if err = s.Close(); err != nil {
				t.Fatal(err)
			}
			s, err = Open(ctx, path, testOptions())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				// Закрывает повторно открытый Store после проверки сохранности прежних байтов, снимков, удержаний,
				// квитанций и курсора.

				// err сообщает об ошибке закрытия тестового пула SQLite.
				if err := s.Close(); err != nil {
					t.Error(err)
				}
			})
			svc, err = core.NewService(s, s.Incarnation())
			if err != nil {
				t.Fatal(err)
			}
		}
	}
	// diagnostics содержит фактически прочитанные версии SQLite и схемы Goose. err сообщает об
	// ошибке чтения версий SQLite и схемы Goose.
	diagnostics, err := s.Diagnostics(ctx)
	if err != nil || diagnostics.SchemaVersion != 1 || diagnostics.SQLiteVersion == "" {
		t.Fatalf("Goose/schema: %+v, %v", diagnostics, err)
	}
	t.Logf("SQLite %s, Goose schema %d", diagnostics.SQLiteVersion, diagnostics.SchemaVersion)
}

// Проверяет переименование прежних профилей без изменения допустимости, ключей и исторических снимков
// и с отказом устаревшей ревизии.
func TestFrozenLegacyNameChangesPreserveIdentityAndHistory(t *testing.T) {
	// svc предоставляет доменные операции текущего владельца временного Store. state содержит
	// эталонные идентификаторы, снимки, квитанцию и удержания прежней базы.
	_, svc, state, _ := openFrozenEncodingState(t)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// profiles содержит страницу трёх прежних профилей. err сообщает об ошибке чтения страницы
	// прежних профилей.
	profiles, err := svc.ListProfiles(ctx, core.PageRequest{Query: core.ListQuery{Limit: 100}})
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles.Items) != 3 {
		t.Fatal("Нужны обычные AV1/H264 и прежние крайние значения")
	}
	// p содержит полный профиль, проверяемый на сохранение идентичности и явных параметров.
	for _, p := range profiles.Items {
		// key содержит ключ полной идентичности профиля. canonical содержит канонические байты
		// профиля до переименования. err сообщает об ошибке вычисления полной идентичности
		// профиля.
		key, canonical, err := core.ProfileIdentity(p.Profile)
		if err != nil {
			t.Fatal(err)
		}
		// original сохраняет полный профиль до изменения имени.
		original := p.Profile
		p.Name = "Новое имя прежнего профиля"
		// updated содержит сохранённый профиль после условного изменения.
		updated, err := svc.UpdateProfile(ctx, p, p.Revision)
		if err != nil {
			t.Fatalf("Name-only отверг прежнюю допустимость: %v", err)
		}
		// gotKey содержит ключ полной идентичности после переименования. gotCanonical содержит
		// канонические байты профиля после переименования.
		gotKey, gotCanonical, err := core.ProfileIdentity(updated.Profile)
		if err != nil || updated.Profile != original || gotKey != key || !bytes.Equal(gotCanonical, canonical) {
			t.Fatalf("Name-only изменил профиль: %v", err)
		}
		if _, err = svc.UpdateProfile(ctx, p, p.Revision); !errors.Is(err, core.ErrConflict) {
			t.Fatalf("Устаревшая CAS-правка принята: %v", err)
		}
	}
	assertFrozenEncodingSnapshots(t, svc, state)
}

// Проверяет точные старые подтверждения очистки и сохранение нового ручного запуска и его удержаний
// варианта после восстановления прежней БД.
func TestFrozenLegacyCleanupFencesRetainNewExecution(t *testing.T) {
	// s содержит открытый Store рабочей копии эталонной базы. svc предоставляет доменные операции
	// текущего владельца временного Store. state содержит эталонные идентификаторы, снимки,
	// квитанцию и удержания прежней базы.
	s, svc, state, _ := openFrozenEncodingState(t)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// request содержит прежний точный запрос с новым RequestID для ручного запуска.
	request := state.StartRequest
	request.RequestID = "manual-after-legacy-recovery"
	// started содержит квитанцию нового запуска при остатках прежних выполнений. err сообщает об
	// ошибке принятия ручного запуска потока.
	started, err := svc.StartSession(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	// sessionID выбирает прежнюю терминальную либо прерванную сессию для очистки.
	for _, sessionID := range []core.ID{state.TerminalSessionID, state.ActiveSessionID} {
		// page содержит страницу профилей либо очистки для проверки прежнего контракта. err
		// сообщает об ошибке чтения страницы намерений очистки.
		page, err := svc.ListCleanups(ctx, core.PageRequest{Query: core.ListQuery{Limit: 100, SessionID: sessionID, PendingOnly: true}})
		if err != nil || len(page.Items) != 2 {
			t.Fatalf("Потеряны независимые cleanup intents: %+v, %v", page, err)
		}
		// bad содержит допуск старой очистки с намеренно увеличенной попыткой.
		bad := core.CleanupFenceFor(page.Items[0])
		bad.Attempt++
		if err = svc.AcknowledgeCleanup(ctx, bad); !errors.Is(err, core.ErrStale) {
			t.Fatalf("Принят неточный cleanup fence: %v", err)
		}
		// intent содержит точное прежнее обязательство очистки, подтверждаемое после нового
		// запуска.
		for _, intent := range page.Items {
			if err = svc.AcknowledgeCleanup(ctx, core.CleanupFenceFor(intent)); err != nil {
				t.Fatal(err)
			}
		}
	}
	// current содержит новый запуск либо заглушку после проверяемого управления.
	current, err := svc.GetSession(ctx, started.SessionID)
	if err != nil || current.State != core.SessionStarting {
		t.Fatalf("Прежний cleanup затронул новый запуск: %+v, %v", current, err)
	}
	requireOutputVariantHolds(t, s, state.VariantID, true)
	// variant содержит точный опубликованный вариант тестового поколения.
	variant, err := svc.GetVariant(ctx, state.VariantID)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.DeleteVariant(ctx, variant.ID, variant.Revision); !errors.Is(err, core.ErrInUse) {
		t.Fatalf("Cleanup освободил удерживаемый новым запуском вариант: %v", err)
	}
	assertFrozenEncodingSnapshots(t, svc, state)
}

// Проверяет сохранение полного Encoding с нулевыми значениями, CAS и запрет понижения версии без
// изменения прежних активных снимков при записи и повторном открытии.
func TestExplicitEncodingPersistsCASAndPreservesActiveLegacySnapshots(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 1)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// started содержит квитанцию нового запуска при остатках прежних выполнений.
	started := startDomainSession(t, f)
	// sessionBefore сохраняет активную сессию до изменения текущего профиля.
	sessionBefore := readSession(t, f.store, started.SessionID)
	// outputBefore сохраняет активный выход до изменения текущего профиля.
	outputBefore := readOutputs(t, f.store, started.SessionID)[0]
	// p содержит полный профиль, проверяемый на сохранение идентичности и явных параметров.
	p := f.profile
	p.Profile.Encoding = core.DefaultEncoding(core.ModeCopy, "libsvtav1", "vbr")
	p.Profile.VideoEncoding.MaxBitrateBPS = 0
	p.Profile.VideoEncoding.BufferBits = 0
	p.Profile.Encoding.SVTAV1.Preset = 0
	p.Profile.Encoding.SVTAV1.LookaheadFrames = 6
	p.Profile.Encoding.VideoCustom = "lp=0"
	p.Profile.Encoding.AudioCustom = "aac_coder=fast"
	// updated содержит сохранённый профиль после условного изменения. err сообщает об ошибке
	// условного изменения полного профиля.
	updated, err := f.service.UpdateProfile(ctx, p, p.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != p.Revision+1 {
		t.Fatal("CAS не увеличил версию")
	}
	if _, err = f.service.UpdateProfile(ctx, p, p.Revision); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("Устаревшая CAS-правка принята: %v", err)
	}
	// legacy содержит недопустимую замену явного Encoding прежним пустым представлением.
	legacy := updated
	legacy.Profile.Encoding = core.EncodingSettings{}
	legacy.Profile.VideoEncoding = storageProfile().VideoEncoding
	if _, err = f.service.UpdateProfile(ctx, legacy, updated.Revision); err == nil {
		t.Fatal("Сервис допустил downgrade v2 в legacy")
	}
	// stored содержит прочитанный профиль после отклонённых CAS и downgrade.
	stored, err := f.service.GetProfile(ctx, p.ID)
	if err != nil || !reflect.DeepEqual(stored, updated) {
		t.Fatalf("CAS/downgrade повредили материализованный v2: %+v, %v", stored, err)
	}
	// after содержит исторический запуск либо выход после изменения текущего профиля.
	if after := readSession(t, f.store, started.SessionID); !reflect.DeepEqual(after.Snapshot, sessionBefore.Snapshot) {
		t.Fatal("Изменение v2 переписало активный Session snapshot")
	}
	// after содержит исторический запуск либо выход после изменения текущего профиля.
	if after := readOutputs(t, f.store, started.SessionID)[0]; !reflect.DeepEqual(after.Snapshot, outputBefore.Snapshot) {
		t.Fatal("Изменение v2 переписало активный Output snapshot/manifest")
	}
	f = reopenFixture(t, f)
	stored, err = f.service.GetProfile(ctx, p.ID)
	if err != nil || !reflect.DeepEqual(stored, updated) {
		t.Fatalf("Reopen применил defaults к v2: %+v, %v", stored, err)
	}
	// after содержит исторический запуск либо выход после изменения текущего профиля.
	if after := readSession(t, f.store, started.SessionID); !reflect.DeepEqual(after.Snapshot, sessionBefore.Snapshot) {
		t.Fatal("Recovery переписал прежний активный снимок")
	}
}

// Проверяет новую идентичность подготовки после явного Encoding, конфликт его изменения до публикации
// и допустимость переименования с сохранением точного профиля в варианте и снимке выхода.
func TestExplicitEncodingPreparationUsesNewIdentityAndPreservesCurrentOnConflict(t *testing.T) {
	// f объединяет временный Store, Service и связанную конфигурацию проверки поколений заглушки.
	f := newGenerationAcceptanceFixture(t)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// old сохраняет опубликованное поколение с прежней идентичностью профиля.
	old, _ := publishGenerationAcceptance(t, f.svc, f.asset.ID)
	// p содержит полный профиль, проверяемый на сохранение идентичности и явных параметров.
	p := f.profile
	p.Profile.Encoding = core.DefaultEncoding(core.ModeCopy, "libsvtav1", "vbr")
	p.Profile.VideoEncoding.MaxBitrateBPS = 0
	p.Profile.VideoEncoding.BufferBits = 0
	// updated содержит сохранённый профиль после условного изменения. err сообщает об ошибке
	// условного изменения полного профиля.
	updated, err := f.svc.UpdateProfile(ctx, p, p.Revision)
	if err != nil {
		t.Fatal(err)
	}
	// pending содержит подготовку с новой полной идентичностью Encoding.
	pending := reserveGenerationAcceptance(t, f.svc, f.asset.ID)
	if len(pending.RequiredProfiles) != 1 || pending.RequiredProfiles[0].Profile.Encoding != p.Profile.Encoding || pending.RequiredProfiles[0].ProfileKey == old.RequiredProfiles[0].ProfileKey || bytes.Equal(pending.RequiredProfiles[0].CanonicalProfile, old.RequiredProfiles[0].CanonicalProfile) {
		t.Fatal("Подготовка переиспользовала прежнюю идентичность")
	}
	// fence идентифицирует точную захваченную попытку проверяемой команды.
	fence := issueGenerationAcceptance(t, f.svc, pending)
	// changed содержит профиль с изменённым Preset после резервирования подготовки.
	changed := updated
	changed.Profile.Encoding.SVTAV1.Preset = 0
	if _, err = f.svc.UpdateProfile(ctx, changed, updated.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err = f.svc.PublishPreparation(ctx, fence, generationAcceptancePublication(pending)); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("Публикация не заметила новое поле кодировщика: %v", err)
	}
	// current содержит заглушку для проверки сохранения прежнего поколения при конфликте публикации.
	current, err := f.svc.GetAsset(ctx, f.asset.ID)
	if err != nil || current.CurrentGenerationID != old.ID {
		t.Fatalf("Неудачная подготовка заменила current: %+v, %v", current, err)
	}
	if err = f.svc.CancelPreparation(ctx, fence, "Профиль изменился до публикации"); err != nil {
		t.Fatal(err)
	}
	if err = f.svc.AcknowledgePreparationCleanup(ctx, fence); err != nil {
		t.Fatal(err)
	}
	// next содержит следующую подготовку после отмены конфликтной.
	next := reserveGenerationAcceptance(t, f.svc, f.asset.ID)
	// nf содержит точный допуск следующей подготовки.
	nf := issueGenerationAcceptance(t, f.svc, next)
	// named содержит текущий профиль для переименования перед публикацией.
	named, err := f.svc.GetProfile(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	named.Name = "Новое имя без изменения кодирования"
	if _, err = f.svc.UpdateProfile(ctx, named, named.Revision); err != nil {
		t.Fatal(err)
	}
	// published содержит опубликованное поколение после допустимого переименования.
	published, err := f.svc.PublishPreparation(ctx, nf, generationAcceptancePublication(next))
	if err != nil {
		t.Fatalf("Name-only заблокировал допустимую публикацию: %v", err)
	}
	if err = f.svc.AcknowledgePreparationCleanup(ctx, nf); err != nil {
		t.Fatal(err)
	}
	// variant содержит точный опубликованный вариант тестового поколения.
	variant, err := f.svc.GetVariant(ctx, published.RequiredProfiles[0].VariantID)
	if err != nil || variant.Profile.Encoding.SVTAV1.Preset != 0 || variant.Profile.Encoding.RateControl != "vbr" {
		t.Fatalf("Вариант потерял явные параметры: %+v, %v", variant, err)
	}
	// start содержит принятую квитанцию первоначального запуска сессии.
	start, err := f.svc.StartSession(ctx, f.start("explicit-profile-start", published))
	if err != nil {
		t.Fatal(err)
	}
	// outputs содержит все сохраняемые выходы проверяемой сессии.
	outputs := readOutputs(t, f.store, start.SessionID)
	if len(outputs) != 1 || outputs[0].Snapshot.Profile.Profile.Encoding != variant.Profile.Encoding || outputs[0].Snapshot.Artifact.ProfileKey != variant.ProfileKey || !bytes.Equal(outputs[0].Snapshot.Artifact.CanonicalProfile, variant.CanonicalProfile) {
		t.Fatalf("Output snapshot/manifest потерял полную идентичность: %+v", outputs)
	}
}
