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

type domainFixture struct {
	store        *Store
	service      *core.Service
	asset        core.FallbackAsset
	stream       core.Stream
	profile      core.TranscodeProfile
	group        core.TranscodeGroup
	variant      core.FallbackVariant
	destinations []core.Destination
}

func newDomainFixture(t *testing.T, outputs int) domainFixture {
	t.Helper()
	s := openTestStore(t)
	svc, err := core.NewService(s, s.Incarnation())
	if err != nil {
		t.Fatalf("создать сервис домена: %v", err)
	}
	ctx := context.Background()
	a, err := svc.ReserveAsset(ctx, core.FallbackAsset{Name: "Оригинал", Kind: core.AssetImage, OriginalPath: "originals/source.png"})
	if err != nil {
		t.Fatalf("зарезервировать исходный ресурс: %v", err)
	}
	stream, err := svc.CreateStream(ctx, core.Stream{Name: "Поток", PublicationKey: "synthetic-publication-key", Fallback: core.FallbackPolicy{AssetID: a.ID, Watchdog: core.WatchdogPolicy{VideoTimeout: time.Second, AudioTimeout: time.Second, RecoveryWindow: time.Second, KeyframeTimeout: time.Second}}})
	if err != nil {
		t.Fatalf("создать поток: %v", err)
	}
	p, err := svc.CreateProfile(ctx, core.TranscodeProfile{Name: "Профиль", Profile: storageProfile()})
	if err != nil {
		t.Fatalf("создать профиль транскодирования: %v", err)
	}
	g, err := svc.CreateGroup(ctx, core.TranscodeGroup{StreamID: stream.ID, ProfileID: p.ID, Name: "Группа"})
	if err != nil {
		t.Fatalf("создать группу транскодирования: %v", err)
	}
	a, v := publishFixtureGeneration(t, svc, a, "synthetic-source-hash", "synthetic-content-hash")
	f := domainFixture{store: s, service: svc, asset: a, stream: stream, profile: p, group: g, variant: v}
	for i := 0; i < outputs; i++ {
		d, err := svc.CreateDestination(ctx, core.Destination{StreamID: stream.ID, GroupID: g.ID, Name: "Назначение", Platform: "custom", Endpoint: "rtmps://example.invalid/live", StreamKey: "synthetic-destination-key", Enabled: true})
		if err != nil {
			t.Fatalf("создать назначение %d: %v", i+1, err)
		}
		f.destinations = append(f.destinations, d)
	}
	return f
}

func requireDomainError(t *testing.T, err, expected error) {
	t.Helper()
	if !errors.Is(err, expected) {
		t.Fatalf("ошибка = %v, нужна категория %v", err, expected)
	}
}

func TestConfigurationCASZeroValuesAndPublicationKey(t *testing.T) {
	f := newDomainFixture(t, 1)
	ctx := context.Background()
	d := f.destinations[0]
	d.Enabled, d.StreamKey = false, ""
	updated, err := f.service.UpdateDestination(ctx, d, d.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != d.Revision+1 {
		t.Fatal("CAS не увеличил версию")
	}
	_, err = f.service.UpdateDestination(ctx, f.destinations[0], d.Revision)
	requireDomainError(t, err, core.ErrConflict)
	if err := f.store.View(ctx, func(r core.Repositories) error {
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
	s := f.stream
	s.PublicationKey = "replacement-synthetic-key"
	_, err = f.service.UpdateStream(ctx, s, s.Revision)
	requireDomainError(t, err, core.ErrConflict)
	if err := f.service.DeleteProfile(ctx, f.profile.ID, f.profile.Revision); !errors.Is(err, core.ErrInUse) {
		t.Fatalf("сохранённая ссылка не защищает профиль: %v", err)
	}
}

func TestConfigurationConstraintsAndRollbackAreRealSQLite(t *testing.T) {
	f := newDomainFixture(t, 1)
	ctx := context.Background()
	_, err := f.service.CreateStream(ctx, core.Stream{Name: "Дубликат", PublicationKey: f.stream.PublicationKey})
	requireDomainError(t, err, core.ErrConflict)
	other, err := f.service.CreateStream(ctx, core.Stream{Name: "Другой поток", PublicationKey: "other-synthetic-key"})
	if err != nil {
		t.Fatal(err)
	}
	_, err = f.service.CreateDestination(ctx, core.Destination{StreamID: other.ID, GroupID: f.group.ID, Name: "Чужая группа", Platform: "custom", Endpoint: "rtmp://example.invalid/live"})
	requireDomainError(t, err, core.ErrInUse)
	err = f.store.Write(ctx, func(r core.Repositories) error {
		value := core.Stream{Record: core.Record{ID: "rolled-back-stream", Revision: 1, CreatedAt: time.Now().UTC(), UpdatedAt: time.Now().UTC()}, Name: "Откат", PublicationKey: "rolled-back-key"}
		if err := r.Config.InsertStream(ctx, value); err != nil {
			return err
		}
		return core.ErrInvalid
	})
	requireDomainError(t, err, core.ErrInvalid)
	err = f.store.View(ctx, func(r core.Repositories) error { _, err := r.Config.Stream(ctx, "rolled-back-stream"); return err })
	requireDomainError(t, err, core.ErrNotFound)
	for _, invalid := range []string{"-0000000000000000001", "18446744073709551616", "0000000000000000000x", "1"} {
		_, err := f.store.sqlDB.ExecContext(ctx, "UPDATE profiles SET revision = ? WHERE id = ?", invalid, f.profile.ID)
		if err == nil {
			t.Fatalf("SQLite CHECK принял некорректный uint64 %q", invalid)
		}
		requireDomainError(t, translate(err), core.ErrInvalid)
	}
	if got := f.store.sqlDB.Stats().InUse; got != 0 {
		t.Fatalf("после отката остались занятые соединения: %d", got)
	}
}

func TestStoredProfilePreservesBytesRationalAndMaxUint64(t *testing.T) {
	f := newDomainFixture(t, 0)
	p := f.profile
	p.Profile.Format.Video.FrameRate = core.FrameRate{Numerator: 60000, Denominator: 2002}
	p.Profile.VideoEncoding.TargetBitrateBPS, p.Profile.VideoEncoding.MaxBitrateBPS = math.MaxUint64, math.MaxUint64
	p.Profile.VideoEncoding.BufferBits, p.Profile.AudioEncoding.BitrateBPS = math.MaxUint64, math.MaxUint64
	p.Profile.Format.Video.CodecProfile = string([]byte{0xff, 0, 'x'})
	updated, err := f.service.UpdateProfile(context.Background(), p, p.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.store.View(context.Background(), func(r core.Repositories) error {
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

func TestAssetDeletionFailureRollsBackVariantAndCleanup(t *testing.T) {
	f := newDomainFixture(t, 0)
	ctx := context.Background()
	requireDomainError(t, f.service.DeleteAsset(ctx, f.asset.ID, f.asset.Revision), core.ErrInUse)
	if err := f.store.View(ctx, func(r core.Repositories) error {
		got, err := r.Artifacts.Variant(ctx, f.variant.ID)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(got, f.variant) {
			t.Fatal("неудачное удаление изменило вариант")
		}
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

func publishFixtureGeneration(t *testing.T, svc *core.Service, a core.FallbackAsset, sourceHash, contentHash string) (core.FallbackAsset, core.FallbackVariant) {
	t.Helper()
	ctx := context.Background()
	g, err := svc.ReservePreparation(ctx, core.PreparationRequest{AssetID: a.ID, ExpectedRevision: a.Revision, Kind: core.AssetImage, GeneratorVersion: "generator-v1"})
	if err != nil {
		t.Fatalf("зарезервировать подготовку поколения: %v", err)
	}
	fence, err := svc.ClaimPreparation(ctx, g.ID)
	if err != nil {
		t.Fatalf("получить право на подготовку поколения: %v", err)
	}
	if err = svc.MarkPreparationIssued(ctx, fence); err != nil {
		t.Fatalf("отметить выдачу подготовки поколения: %v", err)
	}
	publication := core.GenerationPublication{SourceHash: sourceHash, SizeBytes: 1024, SourceFormat: core.SourceFormat{ImageFormat: "png"}}
	for _, p := range g.RequiredProfiles {
		publication.Variants = append(publication.Variants, core.FallbackVariant{Record: core.Record{ID: p.VariantID}, GenerationID: g.ID, AssetID: a.ID, SourceHash: sourceHash, Profile: p.Profile, GeneratorVersion: g.GeneratorVersion, ManagedPath: p.ManagedPath, SizeBytes: 2048, ContentHash: contentHash, VerifiedFormat: p.Profile.Format})
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
	var v core.FallbackVariant
	if len(g.RequiredProfiles) > 0 {
		v, err = svc.GetVariant(ctx, g.RequiredProfiles[0].VariantID)
		if err != nil {
			t.Fatalf("прочитать опубликованный вариант: %v", err)
		}
	}
	return a, v
}

func fixturePublication(g core.FallbackGeneration, sourceHash, contentHash string) core.GenerationPublication {
	p := core.GenerationPublication{SourceHash: sourceHash, SizeBytes: 1024, SourceFormat: core.SourceFormat{ImageFormat: "png"}}
	for _, r := range g.RequiredProfiles {
		p.Variants = append(p.Variants, core.FallbackVariant{Record: core.Record{ID: r.VariantID}, GenerationID: g.ID, AssetID: g.AssetID, SourceHash: sourceHash, Profile: r.Profile, GeneratorVersion: g.GeneratorVersion, ManagedPath: r.ManagedPath, SizeBytes: 2048, ContentHash: contentHash, VerifiedFormat: r.Profile.Format})
	}
	return p
}
func requireOutputVariantHolds(t *testing.T, s *Store, id core.ID, want bool) {
	t.Helper()
	var count int
	if err := s.sqlDB.QueryRowContext(context.Background(), "SELECT count(*) FROM output_holds WHERE variant_id = ?", id).Scan(&count); err != nil {
		t.Fatal(err)
	}
	if (count != 0) != want {
		t.Fatalf("удержания output для %s: %d, ожидается наличие %t", id, count, want)
	}
}
