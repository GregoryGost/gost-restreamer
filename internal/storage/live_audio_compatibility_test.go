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

// Проверяет точные прежние байты профилей v2 после хранения и повторного открытия без материализации
// LiveAudioMode.
func TestLiveAudioLegacyStoredProfileReopenDoesNotDefaultOrRewrite(t *testing.T) {
	// data содержит независимые JSON-эталоны прежних профилей v2. err сообщает об ошибке чтения
	// независимых эталонов прежних профилей.
	data, err := os.ReadFile("../core/testdata/live_audio_legacy_v2/profiles.json")
	if err != nil {
		t.Fatal(err)
	}
	// fixtures содержит оба прежних режима с исходными байтами профиля и записи.
	var fixtures []struct {
		// Name задаёт имя сценария прежнего режима профиля.
		Name string
		// Payload хранит исходные байты прежнего Profile v2. ProfileRecord хранит исходные байты
		// прежней записи TranscodeProfile.
		Payload, ProfileRecord []byte
	}
	if err = json.Unmarshal(data, &fixtures); err != nil {
		t.Fatal(err)
	}
	if len(fixtures) != 2 {
		t.Fatal("Нужны оба прежних режима")
	}
	// fixture содержит эталон прежнего режима для двух циклов открытия.
	for _, fixture := range fixtures {
		t.Run(fixture.Name, func(t *testing.T) {
			// Проверяет два цикла чтения и открытия прежнего профиля без материализации LiveAudioMode и изменения
			// исходных байтов записи и профиля.

			// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
			ctx := context.Background()
			// path задаёт путь отдельной временной базы проверки совместимости.
			path := filepath.Join(t.TempDir(), "compatibility.sqlite")
			// s содержит текущее открытие временной базы с прежними профилями. err сообщает
			// об ошибке открытия временного файлового SQLite.
			s, err := Open(ctx, path, testOptions())
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				// Закрывает текущее значение s после проверки, включая пул, назначенный при повторном открытии.

				// err сообщает об ошибке закрытия временного хранилища.
				if err := s.Close(); err != nil {
					t.Error(err)
				}
			})
			// original принимает прежний TranscodeProfile без назначения LiveAudioMode.
			var original core.TranscodeProfile
			if err = core.DecodeMetadata(fixture.ProfileRecord, &original); err != nil {
				t.Fatal(err)
			}
			if err = s.Write(ctx, func(r core.Repositories) error {
				// Вставляет независимый эталон прежнего TranscodeProfile без применения значений по умолчанию
				// Service.CreateProfile.
				return r.Config.InsertProfile(ctx, original)
			}); err != nil {
				t.Fatal(err)
			}
			// cycle выбирает чтение до либо после повторного открытия базы.
			for cycle := range 2 {
				// row принимает точные байты payload сохранённого профиля.
				var row []byte
				if err = s.sqlDB.QueryRowContext(ctx, "SELECT payload FROM profiles WHERE id = ?", original.ID).Scan(&row); err != nil {
					t.Fatal(err)
				}
				if !bytes.Equal(row, fixture.ProfileRecord) {
					t.Fatal("Чтение/открытие переписало прежние байты v2")
				}
				// svc предоставляет доменные операции текущего владельца временного
				// Store. err сообщает об ошибке создания доменного Service для текущего
				// владельца Store.
				svc, err := core.NewService(s, s.Incarnation())
				if err != nil {
					t.Fatal(err)
				}
				// stored содержит прочитанный профиль для проверки неизменности и
				// отсутствия назначения значений по умолчанию.
				stored, err := svc.GetProfile(ctx, original.ID)
				if err != nil || stored != original || stored.Profile.LiveAudioMode != "" {
					t.Fatalf("Применены значения по умолчанию: %+v %v", stored, err)
				}
				// payload содержит повторно закодированный профиль для побайтовой
				// сверки.
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

// Проверяет copy только при создании профиля, приоритет CAS, отказ удаления явного селектора и
// неизменность профиля при переименовании.
func TestLiveAudioServiceCreateOnlyDefaultAndStrictCASReplacement(t *testing.T) {
	// s содержит текущее открытие временной базы с прежними профилями.
	s := openTestStore(t)
	// svc предоставляет доменные операции текущего владельца временного Store. err сообщает об
	// ошибке создания доменного Service для текущего владельца Store.
	svc, err := core.NewService(s, s.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// mode выбирает прежний режим видео copy либо transcode.
	for _, mode := range []core.GroupMode{core.ModeCopy, core.ModeTranscode} {
		// p содержит полный профиль, проверяемый на сохранение LIVE-аудиорежима.
		p := storageProfile()
		p.Mode = mode
		// created содержит новый профиль с назначенным при создании LiveAudioCopy. err сообщает
		// об ошибке сохранения полного именованного профиля.
		created, err := svc.CreateProfile(ctx, core.TranscodeProfile{Name: "Новый профиль", Profile: p})
		if err != nil || created.Profile.LiveAudioMode != core.LiveAudioCopy || created.Profile.Encoding != (core.EncodingSettings{}) {
			t.Fatalf("Неверное значение по умолчанию при создании: %+v %v", created, err)
		}
		// missing содержит замену с удалённым явным LiveAudioMode.
		missing := created
		missing.Profile.LiveAudioMode = ""
		if _, err = svc.UpdateProfile(ctx, missing, created.Revision-1); !errors.Is(err, core.ErrConflict) {
			t.Fatalf("Проверка селектора обошла CAS: %v", err)
		}
		if _, err = svc.UpdateProfile(ctx, missing, created.Revision); !errors.Is(err, core.ErrInvalidProfile) {
			t.Fatalf("Удалён явный селектор: %v", err)
		}
		// stored содержит прочитанный профиль для проверки неизменности и отсутствия назначения
		// значений по умолчанию.
		stored, err := svc.GetProfile(ctx, created.ID)
		if err != nil || stored != created {
			t.Fatalf("Отказ изменил строку: %+v %v", stored, err)
		}
		stored.Name = "Новое имя"
		// changed содержит результат переименования при неизменных параметрах профиля.
		changed, err := svc.UpdateProfile(ctx, stored, stored.Revision)
		if err != nil || changed.Profile != created.Profile {
			t.Fatalf("Имя изменило профиль: %+v %v", changed, err)
		}
	}
}

// Проверяет сохранение прежних исторических снимков и идентичности квитанции после явного изменения
// режима LIVE-аудио.
func TestLiveAudioExplicitEditLeavesHistoricalSnapshotsAndLegacyReceiptsExact(t *testing.T) {
	// s содержит текущее открытие временной базы с прежними профилями. svc предоставляет доменные
	// операции текущего владельца временного Store. state содержит замороженные конфигурации,
	// исторические снимки и исходную квитанцию.
	s, svc, state, _ := openFrozenEncodingState(t)
	assertFrozenEncodingRows(t, s, state)
	assertFrozenEncodingSnapshots(t, svc, state)
	// p содержит полный профиль, проверяемый на сохранение LIVE-аудиорежима. err сообщает об ошибке
	// чтения полного сохраняемого профиля.
	p, err := svc.GetProfile(context.Background(), state.ProfileID)
	if err != nil {
		t.Fatal(err)
	}
	p.Profile.LiveAudioMode = core.LiveAudioCopy
	if _, err = svc.UpdateProfile(context.Background(), p, p.Revision); err != nil {
		t.Fatal(err)
	}
	assertFrozenEncodingSnapshots(t, svc, state)
	// replayed содержит квитанцию тождественного повторного запроса.
	replayed, err := svc.StartSession(context.Background(), state.StartRequest)
	if err != nil || replayed.RequestID != state.Receipt.RequestID || replayed.Fingerprint != state.Receipt.Fingerprint || !bytes.Equal(replayed.Identity, state.Receipt.Identity) {
		t.Fatalf("Явная правка изменила прежнюю квитанцию: %+v %v", replayed, err)
	}
}
