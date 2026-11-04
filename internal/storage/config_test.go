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
		t.Fatal(err)
	}
	ctx := context.Background()
	a, err := svc.ReserveAsset(ctx, core.FallbackAsset{Name: "Оригинал", Kind: core.AssetImage, OriginalPath: "originals/source.png"})
	if err != nil {
		t.Fatal(err)
	}
	a, err = svc.PublishAsset(ctx, a.ID, a.Revision, a.AttemptToken, "synthetic-source-hash", 1024, core.SourceFormat{ImageFormat: "png"})
	if err != nil {
		t.Fatal(err)
	}
	stream, err := svc.CreateStream(ctx, core.Stream{Name: "Поток", PublicationKey: "synthetic-publication-key", Fallback: core.FallbackPolicy{AssetID: a.ID, Watchdog: core.WatchdogPolicy{VideoTimeout: time.Second, AudioTimeout: time.Second, RecoveryWindow: time.Second, KeyframeTimeout: time.Second}}})
	if err != nil {
		t.Fatal(err)
	}
	p, err := svc.CreateProfile(ctx, core.TranscodeProfile{Name: "Профиль", Profile: storageProfile()})
	if err != nil {
		t.Fatal(err)
	}
	g, err := svc.CreateGroup(ctx, core.TranscodeGroup{StreamID: stream.ID, ProfileID: p.ID, Name: "Группа"})
	if err != nil {
		t.Fatal(err)
	}
	v, err := svc.ReserveVariant(ctx, core.FallbackVariant{AssetID: a.ID, SourceHash: a.SourceHash, Profile: p.Profile, GeneratorVersion: "generator-v1", ManagedPath: "variants/generation-1.nut"})
	if err != nil {
		t.Fatal(err)
	}
	v, err = svc.PublishVariant(ctx, v.ID, v.Revision, v.AttemptToken, "synthetic-content-hash", 2048)
	if err != nil {
		t.Fatal(err)
	}
	f := domainFixture{store: s, service: svc, asset: a, stream: stream, profile: p, group: g, variant: v}
	for i := 0; i < outputs; i++ {
		d, err := svc.CreateDestination(ctx, core.Destination{StreamID: stream.ID, GroupID: g.ID, Name: "Назначение", Platform: "custom", Endpoint: "rtmps://example.invalid/live", StreamKey: "synthetic-destination-key", Enabled: true})
		if err != nil {
			t.Fatal(err)
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
