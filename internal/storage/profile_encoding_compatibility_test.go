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

type frozenEncodingState struct {
	Baseline                                                                                           string
	StreamID, ProfileID, AssetID, VariantID, TerminalSessionID, ActiveSessionID, CancelledGenerationID core.ID
	StartRequest                                                                                       core.StartSessionRequest
	Receipt                                                                                            core.CommandReceipt
	Cursor                                                                                             string
	CursorIDs                                                                                          []core.ID
	Immutable                                                                                          map[string]map[string][]byte
	Snapshots                                                                                          map[string][]byte
	Holds                                                                                              map[string][][]string
}

func openFrozenEncodingState(t *testing.T) (*Store, *core.Service, frozenEncodingState, string) {
	t.Helper()
	root := "testdata/encoding_legacy_v1"
	b, err := os.ReadFile(filepath.Join(root, "state.json"))
	if err != nil {
		t.Fatal(err)
	}
	var state frozenEncodingState
	if err = json.Unmarshal(b, &state); err != nil {
		t.Fatal(err)
	}
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
	sum := sha256.Sum256(b)
	if state.Baseline != "9ebb0d276a50bd655e870a32c2074168377cb038" || provenance["baseline"] != state.Baseline || hex.EncodeToString(sum[:]) != provenance["database_sha256"] {
		t.Fatal("Происхождение замороженной БД не совпадает")
	}
	path := filepath.Join(t.TempDir(), "legacy.sqlite")
	if err = os.WriteFile(path, b, 0600); err != nil {
		t.Fatal(err)
	}
	s, err := Open(context.Background(), path, testOptions())
	if err != nil {
		t.Fatalf("Открыть заполненную БД прежней версией профилей: %v", err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	svc, err := core.NewService(s, s.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	return s, svc, state, path
}

func assertFrozenEncodingSnapshots(t *testing.T, svc *core.Service, state frozenEncodingState) {
	t.Helper()
	ctx := context.Background()
	for key, want := range state.Snapshots {
		var got []byte
		var err error
		if id, ok := strings.CutPrefix(key, "session:"); ok {
			var s core.Session
			s, err = svc.GetSession(ctx, core.ID(id))
			if err == nil {
				got, err = core.EncodeSessionSnapshot(s.Snapshot)
			}
		} else if id, ok := strings.CutPrefix(key, "output:"); ok {
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

func assertFrozenEncodingRows(t *testing.T, s *Store, state frozenEncodingState) {
	t.Helper()
	for _, table := range []string{"streams", "profiles", "groups", "destinations", "fallback_assets", "fallback_generations", "fallback_variants", "owned_paths", "events", "cleanup_intents", "command_receipts"} {
		if len(state.Immutable[table]) == 0 {
			t.Fatalf("В эталоне нет строк %s", table)
		}
		for id, want := range state.Immutable[table] {
			var got []byte
			if err := s.sqlDB.QueryRow("SELECT payload FROM "+table+" WHERE id = ?", id).Scan(&got); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(got, want) {
				t.Fatalf("Открытие переписало прежние байты %s/%s", table, id)
			}
		}
	}
	for _, table := range []string{"session_holds", "output_holds"} {
		rows, err := s.sqlDB.Query("SELECT * FROM " + table + " ORDER BY 1")
		if err != nil {
			t.Fatal(err)
		}
		columns, err := rows.Columns()
		if err != nil {
			_ = rows.Close()
			t.Fatal(err)
		}
		var got [][]string
		for rows.Next() {
			values := make([]string, len(columns))
			targets := make([]any, len(columns))
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

func TestFrozenLegacyDatabaseOpenRecoveryReplayAndCursor(t *testing.T) {
	s, svc, state, path := openFrozenEncodingState(t)
	ctx := context.Background()
	for cycle := 0; cycle < 2; cycle++ {
		assertFrozenEncodingRows(t, s, state)
		assertFrozenEncodingSnapshots(t, svc, state)
		terminal, err := svc.GetSession(ctx, state.TerminalSessionID)
		if err != nil || terminal.State != core.SessionStopped || !terminal.CleanupPending {
			t.Fatalf("Потеряна терминальная история: %+v, %v", terminal, err)
		}
		recovered, err := svc.GetSession(ctx, state.ActiveSessionID)
		if err != nil || recovered.State != core.SessionInterrupted || !recovered.CleanupPending || recovered.EndedAt == nil {
			t.Fatalf("Выданный запуск восстановлен неверно: %+v, %v", recovered, err)
		}
		variant, err := svc.GetVariant(ctx, state.VariantID)
		if err != nil || variant.State != core.VariantReady {
			t.Fatalf("Старый вариант перестал читаться: %+v, %v", variant, err)
		}
		gen, err := svc.GetGeneration(ctx, variant.GenerationID)
		if err != nil || gen.State != core.PreparationPublished || len(gen.RequiredProfiles) != 1 {
			t.Fatalf("Потеряно PUBLISHED поколение: %+v, %v", gen, err)
		}
		failed, err := svc.GetGeneration(ctx, state.CancelledGenerationID)
		if err != nil || !failed.State.Terminal() || failed.State == core.PreparationPublished {
			t.Fatalf("Потеряна история отказа подготовки: %+v, %v", failed, err)
		}
		asset, err := svc.GetAsset(ctx, state.AssetID)
		if err != nil || asset.CurrentGenerationID != gen.ID {
			t.Fatalf("Recovery подменил current: %+v, %v", asset, err)
		}
		replay, err := svc.StartSession(ctx, state.StartRequest)
		if err != nil || !reflect.DeepEqual(replay, state.Receipt) {
			t.Fatalf("Прежняя квитанция изменилась при replay: %+v, %v", replay, err)
		}
		conflict := state.StartRequest
		conflict.DestinationIDs = []core.ID{"other-destination"}
		if _, err = svc.StartSession(ctx, conflict); !errors.Is(err, core.ErrConflict) {
			t.Fatalf("Повтор RequestID принял другое назначение: %v", err)
		}
		page, err := svc.ListProfiles(ctx, core.PageRequest{Query: core.ListQuery{Limit: 100}, Cursor: state.Cursor})
		if err != nil {
			t.Fatalf("Старый cursor отвергнут: %v", err)
		}
		var ids []core.ID
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
	diagnostics, err := s.Diagnostics(ctx)
	if err != nil || diagnostics.SchemaVersion != 1 || diagnostics.SQLiteVersion == "" {
		t.Fatalf("Goose/schema: %+v, %v", diagnostics, err)
	}
	t.Logf("SQLite %s, Goose schema %d", diagnostics.SQLiteVersion, diagnostics.SchemaVersion)
}

func TestFrozenLegacyNameChangesPreserveIdentityAndHistory(t *testing.T) {
	_, svc, state, _ := openFrozenEncodingState(t)
	ctx := context.Background()
	profiles, err := svc.ListProfiles(ctx, core.PageRequest{Query: core.ListQuery{Limit: 100}})
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles.Items) != 3 {
		t.Fatal("Нужны обычные AV1/H264 и прежние крайние значения")
	}
	for _, p := range profiles.Items {
		key, canonical, err := core.ProfileIdentity(p.Profile)
		if err != nil {
			t.Fatal(err)
		}
		original := p.Profile
		p.Name = "Новое имя прежнего профиля"
		updated, err := svc.UpdateProfile(ctx, p, p.Revision)
		if err != nil {
			t.Fatalf("Name-only отверг прежнюю допустимость: %v", err)
		}
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

func TestFrozenLegacyCleanupFencesRetainNewExecution(t *testing.T) {
	s, svc, state, _ := openFrozenEncodingState(t)
	ctx := context.Background()
	request := state.StartRequest
	request.RequestID = "manual-after-legacy-recovery"
	started, err := svc.StartSession(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	for _, sessionID := range []core.ID{state.TerminalSessionID, state.ActiveSessionID} {
		page, err := svc.ListCleanups(ctx, core.PageRequest{Query: core.ListQuery{Limit: 100, SessionID: sessionID, PendingOnly: true}})
		if err != nil || len(page.Items) != 2 {
			t.Fatalf("Потеряны независимые cleanup intents: %+v, %v", page, err)
		}
		bad := core.CleanupFenceFor(page.Items[0])
		bad.Attempt++
		if err = svc.AcknowledgeCleanup(ctx, bad); !errors.Is(err, core.ErrStale) {
			t.Fatalf("Принят неточный cleanup fence: %v", err)
		}
		for _, intent := range page.Items {
			if err = svc.AcknowledgeCleanup(ctx, core.CleanupFenceFor(intent)); err != nil {
				t.Fatal(err)
			}
		}
	}
	current, err := svc.GetSession(ctx, started.SessionID)
	if err != nil || current.State != core.SessionStarting {
		t.Fatalf("Прежний cleanup затронул новый запуск: %+v, %v", current, err)
	}
	requireOutputVariantHolds(t, s, state.VariantID, true)
	variant, err := svc.GetVariant(ctx, state.VariantID)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.DeleteVariant(ctx, variant.ID, variant.Revision); !errors.Is(err, core.ErrInUse) {
		t.Fatalf("Cleanup освободил удерживаемый новым запуском вариант: %v", err)
	}
	assertFrozenEncodingSnapshots(t, svc, state)
}

func TestExplicitEncodingPersistsCASAndPreservesActiveLegacySnapshots(t *testing.T) {
	f := newDomainFixture(t, 1)
	ctx := context.Background()
	started := startDomainSession(t, f)
	sessionBefore := readSession(t, f.store, started.SessionID)
	outputBefore := readOutputs(t, f.store, started.SessionID)[0]
	p := f.profile
	p.Profile.Encoding = core.DefaultEncoding(core.ModeCopy, "libsvtav1", "vbr")
	p.Profile.VideoEncoding.MaxBitrateBPS = 0
	p.Profile.VideoEncoding.BufferBits = 0
	p.Profile.Encoding.SVTAV1.Preset = 0
	p.Profile.Encoding.SVTAV1.LookaheadFrames = 6
	p.Profile.Encoding.VideoCustom = "lp=0"
	p.Profile.Encoding.AudioCustom = "aac_coder=fast"
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
	legacy := updated
	legacy.Profile.Encoding = core.EncodingSettings{}
	legacy.Profile.VideoEncoding = storageProfile().VideoEncoding
	if _, err = f.service.UpdateProfile(ctx, legacy, updated.Revision); err == nil {
		t.Fatal("Сервис допустил downgrade v2 в legacy")
	}
	stored, err := f.service.GetProfile(ctx, p.ID)
	if err != nil || !reflect.DeepEqual(stored, updated) {
		t.Fatalf("CAS/downgrade повредили материализованный v2: %+v, %v", stored, err)
	}
	if after := readSession(t, f.store, started.SessionID); !reflect.DeepEqual(after.Snapshot, sessionBefore.Snapshot) {
		t.Fatal("Изменение v2 переписало активный Session snapshot")
	}
	if after := readOutputs(t, f.store, started.SessionID)[0]; !reflect.DeepEqual(after.Snapshot, outputBefore.Snapshot) {
		t.Fatal("Изменение v2 переписало активный Output snapshot/manifest")
	}
	f = reopenFixture(t, f)
	stored, err = f.service.GetProfile(ctx, p.ID)
	if err != nil || !reflect.DeepEqual(stored, updated) {
		t.Fatalf("Reopen применил defaults к v2: %+v, %v", stored, err)
	}
	if after := readSession(t, f.store, started.SessionID); !reflect.DeepEqual(after.Snapshot, sessionBefore.Snapshot) {
		t.Fatal("Recovery переписал прежний активный снимок")
	}
}

func TestExplicitEncodingPreparationUsesNewIdentityAndPreservesCurrentOnConflict(t *testing.T) {
	f := newGenerationAcceptanceFixture(t)
	ctx := context.Background()
	old, _ := publishGenerationAcceptance(t, f.svc, f.asset.ID)
	p := f.profile
	p.Profile.Encoding = core.DefaultEncoding(core.ModeCopy, "libsvtav1", "vbr")
	p.Profile.VideoEncoding.MaxBitrateBPS = 0
	p.Profile.VideoEncoding.BufferBits = 0
	updated, err := f.svc.UpdateProfile(ctx, p, p.Revision)
	if err != nil {
		t.Fatal(err)
	}
	pending := reserveGenerationAcceptance(t, f.svc, f.asset.ID)
	if len(pending.RequiredProfiles) != 1 || pending.RequiredProfiles[0].Profile.Encoding != p.Profile.Encoding || pending.RequiredProfiles[0].ProfileKey == old.RequiredProfiles[0].ProfileKey || bytes.Equal(pending.RequiredProfiles[0].CanonicalProfile, old.RequiredProfiles[0].CanonicalProfile) {
		t.Fatal("Подготовка переиспользовала прежнюю идентичность")
	}
	fence := issueGenerationAcceptance(t, f.svc, pending)
	changed := updated
	changed.Profile.Encoding.SVTAV1.Preset = 0
	if _, err = f.svc.UpdateProfile(ctx, changed, updated.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err = f.svc.PublishPreparation(ctx, fence, generationAcceptancePublication(pending)); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("Публикация не заметила новое поле кодировщика: %v", err)
	}
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
	next := reserveGenerationAcceptance(t, f.svc, f.asset.ID)
	nf := issueGenerationAcceptance(t, f.svc, next)
	named, err := f.svc.GetProfile(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	named.Name = "Новое имя без изменения кодирования"
	if _, err = f.svc.UpdateProfile(ctx, named, named.Revision); err != nil {
		t.Fatal(err)
	}
	published, err := f.svc.PublishPreparation(ctx, nf, generationAcceptancePublication(next))
	if err != nil {
		t.Fatalf("Name-only заблокировал допустимую публикацию: %v", err)
	}
	if err = f.svc.AcknowledgePreparationCleanup(ctx, nf); err != nil {
		t.Fatal(err)
	}
	variant, err := f.svc.GetVariant(ctx, published.RequiredProfiles[0].VariantID)
	if err != nil || variant.Profile.Encoding.SVTAV1.Preset != 0 || variant.Profile.Encoding.RateControl != "vbr" {
		t.Fatalf("Вариант потерял явные параметры: %+v, %v", variant, err)
	}
	start, err := f.svc.StartSession(ctx, f.start("explicit-profile-start", published))
	if err != nil {
		t.Fatal(err)
	}
	outputs := readOutputs(t, f.store, start.SessionID)
	if len(outputs) != 1 || outputs[0].Snapshot.Profile.Profile.Encoding != variant.Profile.Encoding || outputs[0].Snapshot.Artifact.ProfileKey != variant.ProfileKey || !bytes.Equal(outputs[0].Snapshot.Artifact.CanonicalProfile, variant.CanonicalProfile) {
		t.Fatalf("Output snapshot/manifest потерял полную идентичность: %+v", outputs)
	}
}
