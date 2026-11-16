package storage

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// TestHDRProfileEditCASAndReopen проверяет изменение полного профиля,
// сохранность исходных снимков и квитанций, приоритет CAS и точное чтение после переименования и открытия.
// t сообщает об ошибках проверяемого сценария.
func TestHDRProfileEditCASAndReopen(t *testing.T) {
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
	requested.Profile.Encoding = core.DefaultEncoding(requested.Profile.Mode, core.VideoEncoderX265, core.RateControlCRF)
	requested.Profile.Encoding.CRF = core.OptionalCRF{Set: true, ValueHundredths: 2351}
	requested.Profile.Format.Video.PixelFormat, requested.Profile.Format.Video.BitDepth = "yuv420p10le", 10
	requested.Profile.Format.Video.Codec, requested.Profile.Format.Video.CodecProfile = core.CodecHEVC, "main10"
	requested.Profile.Format.Video.Level, requested.Profile.Format.Video.Tier = "auto", "main"
	requested.Profile.Format.Video.Color = core.ColorInfo{Range: "limited", Primaries: "bt2020", Transfer: "smpte2084", Matrix: "bt2020nc"}
	requested.Profile.VideoEncoding.TargetBitrateBPS, requested.Profile.VideoEncoding.MaxBitrateBPS, requested.Profile.VideoEncoding.BufferBits = 0, 0, 0
	// updated содержит условно сохранённый HDR-профиль с неизменным явным LIVE-аудиорежимом.
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
	if err = s.sqlDB.QueryRowContext(ctx, "SELECT payload FROM profiles WHERE id=?", renamed.ID).Scan(&payload); err != nil || !bytes.Contains(payload, []byte(`"ValueHundredths":2351`)) {
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

// TestHDRRequiredSetRejectsOldIdentityAndChangedQuality проверяет существующий протокол подготовки.
// Старый файл не подходит новому рецепту, а изменение качества CRF после резервирования откатывает всю публикацию.
// t сверяет полный граф после отказов и положительный допуск после публикации точного набора.
func TestHDRRequiredSetRejectsOldIdentityAndChangedQuality(t *testing.T) {
	// fixture содержит текущую заглушку и один настроенный выход без запущенных процессов.
	fixture := newDomainFixture(t, 1)
	// ctx ограничивает только изолированные операции SQLite.
	ctx := context.Background()
	// requested явно заменяет текущий профиль на HEVC HDR CRF с точной глубиной 10 бит.
	requested := fixture.profile
	requested.Profile.Encoding = core.EncodingSettings{VideoEncoder: "libx265", AudioEncoder: "aac", RateControl: "crf", CRF: core.OptionalCRF{Set: true, ValueHundredths: 2351}}
	requested.Profile.Format.Video.PixelFormat, requested.Profile.Format.Video.BitDepth = "yuv420p10le", 10
	requested.Profile.Format.Video.Codec, requested.Profile.Format.Video.CodecProfile = core.CodecHEVC, "main10"
	requested.Profile.Format.Video.Level = "auto"
	requested.Profile.Format.Video.Color = core.ColorInfo{Range: "limited", Primaries: "bt2020", Transfer: "smpte2084", Matrix: "bt2020nc"}
	requested.Profile.VideoEncoding.TargetBitrateBPS, requested.Profile.VideoEncoding.MaxBitrateBPS, requested.Profile.VideoEncoding.BufferBits = 0, 0, 0
	// updated и err фиксируют успешный явный CAS до проверки непригодности прежнего файла.
	updated, err := fixture.service.UpdateProfile(ctx, requested, fixture.profile.Revision)
	if err != nil {
		t.Fatal(err)
	}
	// beforeStart фиксирует отсутствие допустимой привязки без новой подготовки.
	beforeStart := captureSQLRows(t, fixture.store.sqlDB)
	if _, err = fixture.service.SelectStartBindings(ctx, fixture.stream.ID, nil); !errors.Is(err, core.ErrNotReady) || !reflect.DeepEqual(beforeStart, captureSQLRows(t, fixture.store.sqlDB)) {
		t.Fatal("Старый вариант использован под новой идентичностью", err)
	}
	// generation и fence сохраняют точный требуемый профиль перед поздней сменой его качества.
	generation, fence := issuedFixturePreparation(t, fixture)
	if len(generation.RequiredProfiles) != 1 || generation.RequiredProfiles[0].Profile != updated.Profile {
		t.Fatal("Резервирование потеряло полный новый профиль")
	}
	// changed меняет только качество, не геометрию и не метаданные фактического формата.
	changed := updated
	changed.Profile.Encoding.CRF.ValueHundredths = 3600
	changed, err = fixture.service.UpdateProfile(ctx, changed, updated.Revision)
	if err != nil {
		t.Fatal(err)
	}
	// beforePublish фиксирует весь граф непосредственно перед отклоняемой публикацией старого набора.
	beforePublish := captureSQLRows(t, fixture.store.sqlDB)
	if _, err = fixture.service.PublishPreparation(ctx, fence, fixturePublication(generation, "new-source", "new-content")); !errors.Is(err, core.ErrConflict) || !reflect.DeepEqual(beforePublish, captureSQLRows(t, fixture.store.sqlDB)) {
		t.Fatal("Смена качества допустила частичную публикацию прежнего набора", err)
	}
	// restored возвращает то же полное значение с новой Record: публикация зависит от профиля, а не его ревизии.
	restored := changed
	restored.Profile = updated.Profile
	if _, err = fixture.service.UpdateProfile(ctx, restored, changed.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err = fixture.service.PublishPreparation(ctx, fence, fixturePublication(generation, "new-source", "new-content")); err != nil {
		t.Fatal("Точный восстановленный набор перестал публиковаться", err)
	}
	if err = fixture.service.AcknowledgePreparationCleanup(ctx, fence); err != nil {
		t.Fatal(err)
	}
	// bindings подтверждают положительный контроль после полной публикации именно нового поколения.
	bindings, err := fixture.service.SelectStartBindings(ctx, fixture.stream.ID, nil)
	if err != nil || len(bindings) != 1 || bindings[0].GenerationID != generation.ID {
		t.Fatal("Новый полный комплект не получил допуск", bindings, err)
	}
}

// TestHDREditPreservesExplicitTranscodedAudio проверяет обновление полного H264 transcode
// в текущий Encoding с явным селектором: эффективное LIVE-аудио остаётся transcode.
// t сообщает об ошибках проверяемого сценария.
func TestHDREditPreservesExplicitTranscodedAudio(t *testing.T) {
	// store и service обслуживают текущую временную SQLite; state содержит исходные снимки обеих групп.
	store, service, state, _ := openCurrentEncodingState(t)
	// ctx используется только для локальной условной замены сохранённого профиля.
	ctx := context.Background()
	// original и err читают полный H264-профиль с явным LIVE-аудиорежимом.
	original, err := service.GetProfile(ctx, state.TranscodeProfileID)
	if err != nil || original.Profile.Mode != core.ModeTranscode || original.Profile.LiveAudioMode != core.LiveAudioTranscode {
		t.Fatal("Неподходящий исходный профиль", err)
	}
	// requested выбирает новый HEVC-рецепт, сохраняя явный селектор при CAS.
	requested := original
	requested.Profile.Encoding = core.DefaultEncoding(core.ModeTranscode, core.VideoEncoderX265, core.RateControlCRF)
	requested.Profile.Encoding.CRF = core.OptionalCRF{Set: true, ValueHundredths: 2351}
	requested.Profile.Format.Video.Codec, requested.Profile.Format.Video.CodecProfile = core.CodecHEVC, "main"
	requested.Profile.Format.Video.Level, requested.Profile.Format.Video.Tier = "auto", ""
	requested.Profile.VideoEncoding.TargetBitrateBPS, requested.Profile.VideoEncoding.MaxBitrateBPS, requested.Profile.VideoEncoding.BufferBits = 0, 0, 0
	requested.Profile.VideoEncoding.MaxBFrames = 0
	// updated сохраняет явно выбранный режим исходного профиля.
	updated, err := service.UpdateProfile(ctx, requested, original.Revision)
	if err != nil || updated.Profile.LiveAudioMode != core.LiveAudioTranscode {
		t.Fatal("Обновление изменило обработку LIVE-аудио", err)
	}
	assertCurrentEncodingSnapshots(t, service, state)
	// before фиксирует полный граф перед ошибочной потерей уже явного аудиорежима.
	before := captureSQLRows(t, store.sqlDB)
	requested = updated
	requested.Profile.LiveAudioMode = ""
	if _, err = service.UpdateProfile(ctx, requested, updated.Revision); !errors.Is(err, core.ErrInvalidProfile) || !reflect.DeepEqual(before, captureSQLRows(t, store.sqlDB)) {
		t.Fatal("Потеря явного режима изменила данные", err)
	}
}
