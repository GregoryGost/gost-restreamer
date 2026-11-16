package storage

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// TestAudioProfileEditHistoryCASAndReopen проверяет замену AAC на FLAC через настоящий Goose/SQLite.
// t требует неизменных снимков и точных байтов профиля после переименования и Recover.
func TestAudioProfileEditHistoryCASAndReopen(t *testing.T) {
	// store и service обслуживают временную БД; state содержит исходные неизменяемые снимки; path принадлежит тесту.
	store, service, state, path := openCurrentEncodingState(t)
	// ctx используется только для операций с изолированной SQLite.
	ctx := context.Background()
	// original и err читают полный H264-профиль с явно выбранным LIVE transcode.
	original, err := service.GetProfile(ctx, state.TranscodeProfileID)
	if err != nil || original.Profile.LiveAudioMode != core.LiveAudioTranscode || original.Profile.Mode != core.ModeTranscode {
		t.Fatal("Неверный исходный текущий профиль", err)
	}
	// requested сохраняет прежние видеопараметры и задаёт полный явный рецепт NVENC/FLAC.
	requested := original
	requested.Profile.Encoding = core.DefaultEncoding(core.ModeTranscode, core.VideoEncoderNVENC, core.RateControlVBR)
	requested.Profile.Encoding.AudioEncoder = "flac"
	requested.Profile.Encoding.AudioCustom = ""
	requested.Profile.Format.Audio = core.AudioFormat{Codec: "flac", CodecProfile: "s16", SampleRateHz: 48000, Channels: 2, ChannelLayout: "stereo"}
	requested.Profile.AudioEncoding.BitrateBPS = 0
	// updated сохраняет явный LIVE transcode и увеличивает ревизию после замены аудиокодека.
	updated, err := service.UpdateProfile(ctx, requested, original.Revision)
	if err != nil || updated.Profile.LiveAudioMode != core.LiveAudioTranscode || updated.Revision != original.Revision+1 {
		t.Fatal("Замена потеряла явный LIVE transcode", updated, err)
	}
	// before фиксирует весь долговечный граф перед отказами устаревшей записи и сброса селектора.
	before := captureSQLRows(t, store.sqlDB)
	if _, err = service.UpdateProfile(ctx, requested, original.Revision); !errors.Is(err, core.ErrConflict) {
		t.Fatal("Нарушен CAS", err)
	}
	requested = updated
	requested.Profile.LiveAudioMode = ""
	if _, err = service.UpdateProfile(ctx, requested, updated.Revision); !errors.Is(err, core.ErrInvalidProfile) || !reflect.DeepEqual(before, captureSQLRows(t, store.sqlDB)) {
		t.Fatal("Отказ изменил БД либо потерян явный селектор", err)
	}
	updated.Name = "Переименованный FLAC-профиль"
	// renamed изменяет только имя, оставляя полные настройки и идентичность нового рецепта.
	renamed, err := service.UpdateProfile(ctx, updated, updated.Revision)
	if err != nil || renamed.Profile != updated.Profile {
		t.Fatal("Переименование изменило рецепт", err)
	}
	// payload сохраняет точные байты новой формы до штатного повторного открытия.
	var payload []byte
	if err = store.sqlDB.QueryRowContext(ctx, "SELECT payload FROM profiles WHERE id=?", renamed.ID).Scan(&payload); err != nil || !bytes.Contains(payload, []byte(`"BitrateBPS":0`)) {
		t.Fatal("Неверные сохраняемые байты текущего профиля", err)
	}
	assertCurrentEncodingSnapshots(t, service, state)
	if err = store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(ctx, path, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	// Завершает только повторно открытый тестовый пул; t получает ошибку закрытия.
	t.Cleanup(func() {
		// err содержит отказ закрытия принадлежащей тесту БД.
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	service, err = core.NewService(store, store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	// reopened содержит полное доменное значение после Recover.
	reopened, err := service.GetProfile(ctx, renamed.ID)
	if err != nil || reopened != renamed {
		t.Fatal("Reopen изменил профиль", reopened, err)
	}
	// after принимает payload для точного сравнения исходных байтов после Recover.
	var after []byte
	if err = store.sqlDB.QueryRowContext(ctx, "SELECT payload FROM profiles WHERE id=?", renamed.ID).Scan(&after); err != nil || !bytes.Equal(after, payload) {
		t.Fatal("Recover переписал новый профиль", err)
	}
	assertCurrentEncodingSnapshots(t, service, state)
}
