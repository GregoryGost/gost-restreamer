package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

func TestLiveAudioLegacyStoredProfileReopenDoesNotDefaultOrRewrite(t *testing.T) {
	data, err := os.ReadFile("../core/testdata/live_audio_legacy_v2/profiles.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixtures []struct {
		Name                   string
		Payload, ProfileRecord []byte
	}
	if err = json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	if len(fixtures) != 2 {
		t.Fatal("Нужны оба прежних режима")
	}
	for _, fixture := range fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			ctx := context.Background()
			path := filepath.Join(t.TempDir(), "compatibility.sqlite")
			s, err := Open(ctx, path, testOptions())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := s.Close(); err != nil {
					t.Error(err)
				}
			})
			var original core.TranscodeProfile
			if err = core.DecodeMetadata(fixture.ProfileRecord, &original); err != nil {
				t.Fatal(err)
			}
			if err = s.Write(ctx, func(r core.Repositories) error { return r.Config.InsertProfile(ctx, original) }); err != nil {
				t.Fatal(err)
			}
			for cycle := range 2 {
				var row []byte
				if err = s.sqlDB.QueryRowContext(ctx, "SELECT payload FROM profiles WHERE id = ?", original.ID).Scan(&row); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(row, fixture.ProfileRecord) {
					t.Fatal("Чтение/открытие переписало прежние байты v2")
				}
				svc, err := core.NewService(s, s.Incarnation())
				if err != nil {
					t.Fatal(err)
				}
				stored, err := svc.GetProfile(ctx, original.ID)
				if err != nil || stored != original || stored.Profile.LiveAudioMode != "" {
					t.Fatalf("Применены значения по умолчанию: %+v %v", stored, err)
				}
				payload, err := core.EncodeProfile(stored.Profile)
				if err != nil || !bytes.Equal(payload, fixture.Payload) {
					t.Fatalf("Изменён профиль после чтения: %v", err)
				}
				if cycle == 0 {
					if err = s.Close(); err != nil {
						t.Fatal(err)
					}
					s, err = Open(ctx, path, testOptions())
					if err != nil {
						t.Fatal(err)
					}
				}
			}
		})
	}
}

func TestLiveAudioServiceCreateOnlyDefaultAndStrictCASReplacement(t *testing.T) {
	s := openTestStore(t)
	svc, err := core.NewService(s, s.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	for _, mode := range []core.GroupMode{core.ModeCopy, core.ModeTranscode} {
		p := storageProfile()
		p.Mode = mode
		created, err := svc.CreateProfile(ctx, core.TranscodeProfile{Name: "Новый профиль", Profile: p})
		if err != nil || created.Profile.LiveAudioMode != core.LiveAudioCopy || created.Profile.Encoding != (core.EncodingSettings{}) {
			t.Fatalf("Неверное значение по умолчанию при создании: %+v %v", created, err)
		}
		missing := created
		missing.Profile.LiveAudioMode = ""
		if _, err = svc.UpdateProfile(ctx, missing, created.Revision-1); !errors.Is(err, core.ErrConflict) {
			t.Fatalf("Проверка селектора обошла CAS: %v", err)
		}
		if _, err = svc.UpdateProfile(ctx, missing, created.Revision); !errors.Is(err, core.ErrInvalidProfile) {
			t.Fatalf("Удалён явный селектор: %v", err)
		}
		stored, err := svc.GetProfile(ctx, created.ID)
		if err != nil || stored != created {
			t.Fatalf("Отказ изменил строку: %+v %v", stored, err)
		}
		stored.Name = "Новое имя"
		changed, err := svc.UpdateProfile(ctx, stored, stored.Revision)
		if err != nil || changed.Profile != created.Profile {
			t.Fatalf("Имя изменило профиль: %+v %v", changed, err)
		}
	}
}

func TestLiveAudioExplicitEditLeavesHistoricalSnapshotsAndLegacyReceiptsExact(t *testing.T) {
	s, svc, state, _ := openFrozenEncodingState(t)
	assertFrozenEncodingRows(t, s, state)
	assertFrozenEncodingSnapshots(t, svc, state)
	p, err := svc.GetProfile(context.Background(), state.ProfileID)
	if err != nil {
		t.Fatal(err)
	}
	p.Profile.LiveAudioMode = core.LiveAudioCopy
	if _, err = svc.UpdateProfile(context.Background(), p, p.Revision); err != nil {
		t.Fatal(err)
	}
	assertFrozenEncodingSnapshots(t, svc, state)
	replayed, err := svc.StartSession(context.Background(), state.StartRequest)
	if err != nil || replayed.RequestID != state.Receipt.RequestID || replayed.Fingerprint != state.Receipt.Fingerprint || !bytes.Equal(replayed.Identity, state.Receipt.Identity) {
		t.Fatalf("Явная правка изменила прежнюю квитанцию: %+v %v", replayed, err)
	}
}
