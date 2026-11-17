package storage

import (
	"bytes"
	"context"
	"errors"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// TestExpandedProfileEditCASAndReopen проверяет изменение полного профиля,
// сохранность исходных снимков и квитанций, приоритет CAS и точное чтение после переименования и открытия.
// t сообщает об ошибках проверяемого сценария.
func TestExpandedProfileEditCASAndReopen(t *testing.T) {
	// s и svc владеют временной копией текущей БД; state фиксирует независимые исторические данные;
	// path указывает исключительно на принадлежащую тесту копию.
	s, svc, state, path := openCurrentEncodingState(t)
	// ctx ограничивает только изолированные операции временной SQLite.
	ctx := context.Background()
	// original содержит исходный полный профиль с явным селектором; err сообщает отказ БД.
	original, err := svc.GetProfile(ctx, state.ProfileID)
	if err != nil {
		t.Fatal(err)
	}
	// requested сохраняет прежнюю идентичность записи и явно выбирает новый рецепт с глубиной 10 бит.
	requested := original
	requested.Profile.Encoding = core.DefaultEncoding(requested.Profile.Mode, core.VideoEncoderSVTAV1, core.RateControlCRF)
	requested.Profile.Encoding.CRF = core.OptionalCRF{Set: true, ValueHundredths: 3500}
	requested.Profile.Format.Video.PixelFormat, requested.Profile.Format.Video.BitDepth = "yuv420p10le", 10
	requested.Profile.VideoEncoding.TargetBitrateBPS, requested.Profile.VideoEncoding.MaxBitrateBPS, requested.Profile.VideoEncoding.BufferBits = 0, 0, 0
	// updated содержит условно сохранённый профиль с неизменным явным LIVE-аудиорежимом.
	updated, err := svc.UpdateProfile(ctx, requested, original.Revision)
	if err != nil || updated.Profile.LiveAudioMode != original.Profile.EffectiveLiveAudioMode() || updated.Revision != original.Revision+1 {
		t.Fatal("Замена изменила явный LIVE-аудиорежим", err)
	}
	if _, err = svc.UpdateProfile(ctx, requested, original.Revision); !errors.Is(err, core.ErrConflict) {
		t.Fatal("Устаревшая запись обошла CAS", err)
	}
	// stored проверяет отсутствие частичной записи после отклонённой устаревшей замены.
	stored, err := svc.GetProfile(ctx, updated.ID)
	if err != nil || stored != updated {
		t.Fatal("Отклонённое изменение переписало профиль", err)
	}
	stored.Name = "Переименованный расширенный профиль"
	// renamed фиксирует изменение одного имени при прежней идентичности полного рецепта.
	renamed, err := svc.UpdateProfile(ctx, stored, stored.Revision)
	if err != nil || renamed.Profile != updated.Profile {
		t.Fatal("Rename изменил новый рецепт", err)
	}
	// payload сохраняет точные байты текущего профиля перед повторным открытием.
	var payload []byte
	if err = s.sqlDB.QueryRowContext(ctx, "SELECT payload FROM profiles WHERE id=?", renamed.ID).Scan(&payload); err != nil || !bytes.Contains(payload, []byte(`"ValueHundredths":3500`)) {
		t.Fatal("Не сохранена явная форма текущего профиля", err)
	}
	assertCurrentEncodingSnapshots(t, svc, state)
	if err = s.Close(); err != nil {
		t.Fatal(err)
	}
	s, err = Open(ctx, path, testOptions())
	if err != nil {
		t.Fatal(err)
	}
	// Закрывает повторный пул до удаления принадлежащей тесту SQLite.
	t.Cleanup(func() {
		// err содержит отказ закрытия повторного открытия хранилища.
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	svc, err = core.NewService(s, s.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	// reopened содержит точное прежнее значение после Recover без молчаливого преобразования профиля.
	reopened, err := svc.GetProfile(ctx, renamed.ID)
	if err != nil || reopened != renamed {
		t.Fatal("Повторное открытие изменило профиль", err)
	}
	// after содержит исходный payload после штатного открытия для побайтового сравнения.
	var after []byte
	if err = s.sqlDB.QueryRowContext(ctx, "SELECT payload FROM profiles WHERE id=?", renamed.ID).Scan(&after); err != nil || !bytes.Equal(after, payload) {
		t.Fatal("Reopen переписал сохраняемый рецепт", err)
	}
	assertCurrentEncodingSnapshots(t, svc, state)
	// receipt подтверждает прежнюю команду с исходной идентичностью после изменения текущего профиля.
	receipt, err := svc.StartSession(ctx, state.StartRequest)
	if err != nil || receipt.Fingerprint != state.Receipt.Fingerprint || !bytes.Equal(receipt.Identity, state.Receipt.Identity) {
		t.Fatal("Новый рецепт изменил историческую идентичность команды", err)
	}
}
