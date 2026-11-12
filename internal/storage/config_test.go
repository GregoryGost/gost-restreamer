package storage

import (
	"context"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// Создаёт допустимый прежний профиль AV1/AAC без Encoding для тестов постоянного хранения.
func storageProfile() core.GroupProfile {
	return core.GroupProfile{
		Mode: core.ModeCopy,
		Format: core.MediaFormat{
			Video: core.VideoFormat{Codec: core.CodecAV1, Width: 2560, Height: 1440, FrameRate: core.FrameRate{Numerator: 60, Denominator: 1}, CodecProfile: "main", PixelFormat: "yuv420p", BitDepth: 8, Color: core.ColorInfo{Range: "limited", Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"}},
			Audio: core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", SampleRateHz: 48000, Channels: 2, ChannelLayout: "stereo"},
		},
		VideoEncoding: core.VideoEncoding{TargetBitrateBPS: 6000000, MaxBitrateBPS: 8000000, BufferBits: 12000000, GOPFrames: 120, ClosedGOP: true},
		AudioEncoding: core.AudioEncoding{BitrateBPS: 192000},
	}
}

// domainFixture объединяет изолированное хранилище и согласованные текущие настройки с опубликованной
// заглушкой.
type domainFixture struct {
	// store владеет временной файловой SQLite с рабочей схемой Goose.
	store *Store
	// service предоставляет доменные операции текущего владельца временного Store.
	service *core.Service
	// asset содержит стабильную заглушку и указатель опубликованного поколения.
	asset core.FallbackAsset
	// stream содержит конфигурацию потока с постоянным ключом и выбранной заглушкой.
	stream core.Stream
	// profile содержит полный именованный профиль тестовой группы.
	profile core.TranscodeProfile
	// group содержит конфигурацию группы тестового потока.
	group core.TranscodeGroup
	// variant содержит точный опубликованный вариант тестового поколения.
	variant core.FallbackVariant
	// destinations содержит включённые назначения тестовой группы.
	destinations []core.Destination
}

// Создаёт временное хранилище с Service, Stream, профилем, группой, опубликованным поколением заглушки
// и заданным числом включённых назначений.
func newDomainFixture(t *testing.T, outputs int) domainFixture {
	t.Helper()
	// s владеет временным Store проверяемого сценария.
	s := openTestStore(t)
	// svc предоставляет доменные операции текущего владельца временного Store. err сообщает об
	// ошибке создания доменного Service для текущего владельца Store.
	svc, err := core.NewService(s, s.Incarnation())
	if err != nil {
		t.Fatalf("создать сервис домена: %v", err)
	}
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// a содержит стабильную заглушку проверяемого поколения.
	a, err := svc.ReserveAsset(ctx, core.FallbackAsset{Name: "Оригинал", Kind: core.AssetImage, OriginalPath: "originals/source.png"})
	if err != nil {
		t.Fatalf("зарезервировать исходный ресурс: %v", err)
	}
	// stream содержит конфигурацию потока с постоянным ключом и выбранной заглушкой.
	stream, err := svc.CreateStream(ctx, core.Stream{Name: "Поток", PublicationKey: "synthetic-publication-key", Fallback: core.FallbackPolicy{AssetID: a.ID, Watchdog: core.WatchdogPolicy{VideoTimeout: time.Second, AudioTimeout: time.Second, RecoveryWindow: time.Second, KeyframeTimeout: time.Second}}})
	if err != nil {
		t.Fatalf("создать поток: %v", err)
	}
	// p содержит полный именованный профиль для проверяемого изменения.
	p, err := svc.CreateProfile(ctx, core.TranscodeProfile{Name: "Профиль", Profile: storageProfile()})
	if err != nil {
		t.Fatalf("создать профиль транскодирования: %v", err)
	}
	// g содержит конфигурацию группы тестового потока.
	g, err := svc.CreateGroup(ctx, core.TranscodeGroup{StreamID: stream.ID, ProfileID: p.ID, Name: "Группа"})
	if err != nil {
		t.Fatalf("создать группу транскодирования: %v", err)
	}
	// v содержит опубликованный вариант точного поколения.
	a, v := publishFixtureGeneration(t, svc, a, "synthetic-source-hash", "synthetic-content-hash")
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := domainFixture{store: s, service: svc, asset: a, stream: stream, profile: p, group: g, variant: v}
	// i указывает очередное создаваемое включённое назначение.
	for i := range outputs {
		// d содержит настройки проверяемого назначения. err сообщает об ошибке сохранения
		// назначения выбранной группы.
		d, err := svc.CreateDestination(ctx, core.Destination{StreamID: stream.ID, GroupID: g.ID, Name: "Назначение", Platform: "custom", Endpoint: "rtmps://example.invalid/live", StreamKey: "synthetic-destination-key", Enabled: true})
		if err != nil {
			t.Fatalf("создать назначение %d: %v", i+1, err)
		}
		f.destinations = append(f.destinations, d)
	}
	return f
}

// Проверяет ожидаемую доменную категорию через errors.Is.
func requireDomainError(t *testing.T, err, expected error) {
	t.Helper()
	if !errors.Is(err, expected) {
		t.Fatalf("ошибка = %v, нужна категория %v", err, expected)
	}
}

// Проверяет увеличение ревизии, сохранение false и пустого ключа назначения, отказ устаревшей записи и
// неизменность ключа Stream и используемого профиля.
func TestConfigurationCASZeroValuesAndPublicationKey(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 1)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// d содержит настройки проверяемого назначения.
	d := f.destinations[0]
	d.Enabled, d.StreamKey = false, ""
	// updated содержит результат условного сохранения изменённой конфигурации. err сообщает об
	// ошибке условного изменения назначения.
	updated, err := f.service.UpdateDestination(ctx, d, d.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != d.Revision+1 {
		t.Fatal("CAS не увеличил версию")
	}
	_, err = f.service.UpdateDestination(ctx, f.destinations[0], d.Revision)
	requireDomainError(t, err, core.ErrConflict)
	// err сообщает об ошибке чтения сохранённых настроек назначения.
	if err := f.store.View(ctx, func(r core.Repositories) error {
		// Проверяет чтение явно сохранённых false и пустого StreamKey вместе с увеличенной Revision
		// назначения.

		// got содержит прочитанное сохраняемое значение для проверки результата операции. err
		// сообщает об ошибке чтения сохранённых настроек назначения.
		got, err := r.Config.Destination(ctx, d.ID)
		if err != nil {
			return err
		}
		if got.Enabled || got.StreamKey != "" || got.Revision != updated.Revision {
			t.Fatalf("нулевые значения потеряны: %+v", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	// s содержит копию потока с подменённым постоянным ключом публикации.
	s := f.stream
	s.PublicationKey = "replacement-synthetic-key"
	_, err = f.service.UpdateStream(ctx, s, s.Revision)
	requireDomainError(t, err, core.ErrConflict)
	// err сообщает об ошибке удаления профиля при сохраняемых ссылках.
	if err := f.service.DeleteProfile(ctx, f.profile.ID, f.profile.Revision); !errors.Is(err, core.ErrInUse) {
		t.Fatalf("сохранённая ссылка не защищает профиль: %v", err)
	}
}

// Проверяет реальные уникальность, составные внешние связи и ограничения uint64 SQLite, а также откат
// доменной операции без утечки соединений.
func TestConfigurationConstraintsAndRollbackAreRealSQLite(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 1)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// err сообщает об ошибке сохранения конфигурации потока.
	_, err := f.service.CreateStream(ctx, core.Stream{Name: "Дубликат", PublicationKey: f.stream.PublicationKey})
	requireDomainError(t, err, core.ErrConflict)
	// other содержит конфигурацию другого потока для проверки границ принадлежности.
	other, err := f.service.CreateStream(ctx, core.Stream{Name: "Другой поток", PublicationKey: "other-synthetic-key"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.service.CreateDestination(ctx, core.Destination{StreamID: other.ID, GroupID: f.group.ID, Name: "Чужая группа", Platform: "custom", Endpoint: "rtmp://example.invalid/live"})
	requireDomainError(t, err, core.ErrInUse)
	err = f.store.Write(ctx, func(r core.Repositories) error {
		// Вставляет Stream и возвращает ErrInvalid, чтобы проверить откат реальных изменений SQLite.

		// value задаёт синтетическую конфигурацию потока, сохраняемую внутри проверяемой
		// транзакции.
		value := core.Stream{ID: "rolled-back-stream", Revision: 1, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC(), Name: "Откат", PublicationKey: "rolled-back-key"}
		// err сообщает об ошибке вставки конфигурации потока.
		if err := r.Config.InsertStream(ctx, value); err != nil {
			return err
		}
		return core.ErrInvalid
	})
	requireDomainError(t, err, core.ErrInvalid)
	err = f.store.View(ctx, func(r core.Repositories) error {
		// Ищет Stream отменённой транзакции для проверки ErrNotFound после отката.
		// err сообщает об ошибке чтения конфигурации потока.
		_, err := r.Config.Stream(ctx, "rolled-back-stream")
		return err
	})
	requireDomainError(t, err, core.ErrNotFound)
	// invalid содержит очередную недопустимую десятичную запись uint64.
	for _, invalid := range []string{"-0000000000000000001", "18446744073709551616", "0000000000000000000x", "1"} {
		// err сообщает об ошибке прямой записи недопустимого десятичного revision в SQLite.
		_, err := f.store.sqlDB.ExecContext(ctx, "UPDATE profiles SET revision = ? WHERE id = ?", invalid, f.profile.ID)
		if err == nil {
			t.Fatalf("SQLite CHECK принял некорректный uint64 %q", invalid)
		}
		requireDomainError(t, translate(err), core.ErrInvalid)
	}
	// got содержит число занятых соединений после отката и проверки ограничений.
	if got := f.store.sqlDB.Stats().InUse; got != 0 {
		t.Fatalf("после отката остались занятые соединения: %d", got)
	}
}

// Проверяет точное сохранение прежнего профиля в SQLite, включая исходную дробь FPS, строковые байты и
// MaxUint64.
func TestStoredProfilePreservesBytesRationalAndMaxUint64(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 0)
	// p содержит полный именованный профиль для проверяемого изменения.
	p := f.profile
	p.Profile.Format.Video.FrameRate = core.FrameRate{Numerator: 60000, Denominator: 2002}
	p.Profile.VideoEncoding.TargetBitrateBPS, p.Profile.VideoEncoding.MaxBitrateBPS = math.MaxUint64, math.MaxUint64
	p.Profile.VideoEncoding.BufferBits, p.Profile.AudioEncoding.BitrateBPS = math.MaxUint64, math.MaxUint64
	p.Profile.Format.Video.CodecProfile = string([]byte{0xff, 0, 'x'})
	// updated содержит результат условного сохранения изменённой конфигурации. err сообщает об
	// ошибке условного изменения полного профиля.
	updated, err := f.service.UpdateProfile(context.Background(), p, p.Revision)
	if err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке чтения сохранённого полного профиля.
	if err := f.store.View(context.Background(), func(r core.Repositories) error {
		// Сверяет прочитанный профиль с сохранённым, включая точные строковые байты, несокращённый FPS и
		// math.MaxUint64.

		// got содержит прочитанное сохраняемое значение для проверки результата операции. err
		// сообщает об ошибке чтения сохранённого полного профиля.
		got, err := r.Config.Profile(context.Background(), p.ID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(updated, got) {
			t.Fatalf("SQLite изменил сохранённый профиль: %+v", got)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Проверяет, что неуспешное удаление используемой заглушки сохраняет вариант и не оставляет намерений
// очистки.
func TestAssetDeletionFailureRollsBackVariantAndCleanup(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 0)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	requireDomainError(t, f.service.DeleteAsset(ctx, f.asset.ID, f.asset.Revision), core.ErrInUse)
	// err сообщает об ошибке проверки отсутствия частичной очистки после отката удаления заглушки.
	if err := f.store.View(ctx, func(r core.Repositories) error {
		// После запрещённого удаления Asset проверяет сохранность варианта и отсутствие оставшихся намерений
		// очистки.

		// got содержит прочитанное сохраняемое значение для проверки результата операции. err
		// сообщает об ошибке чтения точного варианта заглушки.
		got, err := r.Artifacts.Variant(ctx, f.variant.ID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(got, f.variant) {
			t.Fatal("неудачное удаление изменило вариант")
		}
		// cleanups содержит сохраняемые намерения физической очистки после проверяемой операции.
		cleanups, err := r.Artifacts.Cleanups(ctx, core.ListQuery{Limit: core.MaxListLimit})
		if err != nil {
			return err
		}
		if len(cleanups) != 0 {
			t.Fatalf("откаченная операция оставила %d намерений очистки", len(cleanups))
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
}

// Проводит тестовое поколение через резервирование, захват, выдачу, публикацию и подтверждение
// очистки, затем читает Asset и первый вариант; реальные медиафайлы не создаются.
func publishFixtureGeneration(t *testing.T, svc *core.Service, a core.FallbackAsset, sourceHash, contentHash string) (core.FallbackAsset, core.FallbackVariant) {
	t.Helper()
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// g содержит новое поколение с зарезервированным полным набором профилей и путей. err сообщает
	// об ошибке резервирования полного поколения подготовки.
	g, err := svc.ReservePreparation(ctx, core.PreparationRequest{AssetID: a.ID, ExpectedRevision: a.Revision, Kind: core.AssetImage, GeneratorVersion: "generator-v1"})
	if err != nil {
		t.Fatalf("зарезервировать подготовку поколения: %v", err)
	}
	// fence идентифицирует точную захваченную попытку подготовки поколения.
	fence, err := svc.ClaimPreparation(ctx, g.ID)
	if err != nil {
		t.Fatalf("получить право на подготовку поколения: %v", err)
	}
	if err = svc.MarkPreparationIssued(ctx, fence); err != nil {
		t.Fatalf("отметить выдачу подготовки поколения: %v", err)
	}
	// publication содержит синтетические метаданные оригинала и накапливает все требуемые варианты
	// для атомарной публикации.
	publication := core.GenerationPublication{SourceHash: sourceHash, SizeBytes: 1024, SourceFormat: core.SourceFormat{ImageFormat: "png"}}
	// p содержит очередной требуемый профиль и его зарезервированный путь варианта.
	for _, p := range g.RequiredProfiles {
		publication.Variants = append(publication.Variants, core.FallbackVariant{ID: p.VariantID, GenerationID: g.ID, AssetID: a.ID, SourceHash: sourceHash, Profile: p.Profile, GeneratorVersion: g.GeneratorVersion, ManagedPath: p.ManagedPath, SizeBytes: 2048, ContentHash: contentHash, VerifiedFormat: p.Profile.Format})
	}
	if _, err = svc.PublishPreparation(ctx, fence, publication); err != nil {
		t.Fatalf("опубликовать подготовленное поколение: %v", err)
	}
	if err = svc.AcknowledgePreparationCleanup(ctx, fence); err != nil {
		t.Fatalf("подтвердить очистку подготовки поколения: %v", err)
	}
	a, err = svc.GetAsset(ctx, a.ID)
	if err != nil {
		t.Fatalf("прочитать исходный ресурс после публикации: %v", err)
	}
	// v содержит опубликованный вариант точного поколения.
	var v core.FallbackVariant
	if len(g.RequiredProfiles) > 0 {
		v, err = svc.GetVariant(ctx, g.RequiredProfiles[0].VariantID)
		if err != nil {
			t.Fatalf("прочитать опубликованный вариант: %v", err)
		}
	}
	return a, v
}

// Создаёт согласованную тестовую публикацию PNG-оригинала и всех требуемых вариантов поколения с
// заданными хешами.
func fixturePublication(g core.FallbackGeneration, sourceHash, contentHash string) core.GenerationPublication {
	// p накапливает публикацию оригинала и всех требуемых вариантов поколения.
	p := core.GenerationPublication{SourceHash: sourceHash, SizeBytes: 1024, SourceFormat: core.SourceFormat{ImageFormat: "png"}}
	// r содержит очередной требуемый профиль поколения при построении тестовой публикации.
	for _, r := range g.RequiredProfiles {
		p.Variants = append(p.Variants, core.FallbackVariant{ID: r.VariantID, GenerationID: g.ID, AssetID: g.AssetID, SourceHash: sourceHash, Profile: r.Profile, GeneratorVersion: g.GeneratorVersion, ManagedPath: r.ManagedPath, SizeBytes: 2048, ContentHash: contentHash, VerifiedFormat: r.Profile.Format})
	}
	return p
}

// Проверяет через SQLite наличие либо отсутствие удержаний конкретного варианта выходами.
func requireOutputVariantHolds(t *testing.T, s *Store, id core.ID, want bool) {
	t.Helper()
	// count принимает число сохраняемых удержаний проверяемого варианта.
	var count int
	// err сообщает об ошибке SQL-проверки числа удержаний варианта.
	if err := s.sqlDB.QueryRowContext(context.Background(), "SELECT count(*) FROM output_holds WHERE variant_id = ?", id).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if (count != 0) != want {
		t.Fatalf("удержания output для %s: %d, ожидается наличие %t", id, count, want)
	}
}
