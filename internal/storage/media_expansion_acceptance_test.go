package storage

import (
	"bytes"
	"context"
	"encoding/json"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// TestMediaExpansionQualityHistoryAndReopen проверяет текущую заполненную SQLite:
// качество CRF, многоканальный AAC, неизменность снимков и точный повтор квитанции после открытия.
// t сообщает об ошибках проверяемого сценария.
func TestMediaExpansionQualityHistoryAndReopen(t *testing.T) {
	// s и svc обслуживают текущую временную БД; state сохраняет исходные конфигурации, снимки
	// и квитанцию, а path позволяет повторно открыть только эту БД.
	s, svc, state, path := openCurrentEncodingState(t)
	assertCurrentEncodingRows(t, s, state)
	assertCurrentEncodingSnapshots(t, svc, state)
	// ctx используется только для изолированной БД; p содержит новый чистый рецепт CRF AV1.
	ctx := context.Background()
	p := storageProfile()
	p.LiveAudioMode = core.LiveAudioCopy
	p.Encoding = core.DefaultEncoding(p.Mode, core.VideoEncoderSVTAV1, core.RateControlCRF)
	p.Encoding.CRF = core.OptionalCRF{Set: true, ValueHundredths: 3500}
	p.VideoEncoding.TargetBitrateBPS, p.VideoEncoding.MaxBitrateBPS, p.VideoEncoding.BufferBits = 0, 0, 0
	p.Format.Audio.Channels, p.Format.Audio.ChannelLayout = 6, "5.1"
	// created содержит новый профиль, сосуществующий с исходными неизменяемыми снимками;
	// err сообщает об отказе записи профиля или последующих операций временного хранилища.
	created, err := svc.CreateProfile(ctx, core.TranscodeProfile{Name: "Многоканальный профиль качества", Profile: p})
	if err != nil {
		t.Fatal(err)
	}
	// original сохраняет точные байты новой строки для сравнения после повторного открытия.
	var original []byte
	if err = s.sqlDB.QueryRowContext(ctx, "SELECT payload FROM profiles WHERE id = ?", created.ID).Scan(&original); err != nil {
		t.Fatal(err)
	}
	// version получает фактический независимый формат хранения новой схемы.
	var version int
	if err = s.sqlDB.QueryRowContext(ctx, "SELECT version FROM storage_format").Scan(&version); err != nil || version != 4 {
		t.Fatalf("storage_format: %d %v", version, err)
	}
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { /* Закрывает повторное открытие до удаления временной базы. */
		// err сообщает об отказе закрытия принадлежащего тесту пула.
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	svc, err = core.NewService(s, s.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	assertCurrentEncodingRows(t, s, state)
	assertCurrentEncodingSnapshots(t, svc, state)
	// stored содержит самостоятельно восстановленный новый профиль.
	stored, err := svc.GetProfile(ctx, created.ID)
	if err != nil || stored != created {
		t.Fatalf("Новый профиль изменился при reopen: %+v %v", stored, err)
	}
	// reopened принимает байты новой строки для проверки отсутствия перезаписи при запуске.
	var reopened []byte
	if err = s.sqlDB.QueryRowContext(ctx, "SELECT payload FROM profiles WHERE id = ?", created.ID).Scan(&reopened); err != nil || !bytes.Equal(original, reopened) {
		t.Fatal("Reopen изменил payload", err)
	}
	// profileJSON позволяет проверить единственную сохраняемую форму без версии возможностей.
	profileJSON, err := core.EncodeProfile(stored.Profile)
	if err != nil {
		t.Fatal(err)
	}
	// root содержит только проверяемую оболочку текущего профиля.
	var root map[string]json.RawMessage
	if err = json.Unmarshal(profileJSON, &root); err != nil || root["Version"] != nil || root["Encoding"] == nil {
		t.Fatal("Не сохранён полный текущий профиль", err)
	}
	// replayed проверяет точную первоначальную идентичность команды после открытия и новых записей.
	replayed, err := svc.StartSession(ctx, state.StartRequest)
	if err != nil || replayed.RequestID != state.Receipt.RequestID || replayed.Fingerprint != state.Receipt.Fingerprint || !bytes.Equal(replayed.Identity, state.Receipt.Identity) {
		t.Fatal("Изменилась исходная идентичность команды", err)
	}
}
