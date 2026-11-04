package core_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

func contractRecord(id core.ID) core.Record {
	now := time.Date(2026, 1, 2, 3, 4, 5, 123456000, time.UTC)
	return core.Record{ID: id, Revision: 1, CreatedAt: now, UpdatedAt: now}
}

func contractSnapshots(t *testing.T) (core.SessionSnapshot, core.OutputSnapshot) {
	t.Helper()
	p := validProfile()
	key, canonical, err := core.ProfileIdentity(p)
	if err != nil {
		t.Fatal(err)
	}
	policy := core.FallbackPolicy{AssetID: "asset", Watchdog: core.WatchdogPolicy{VideoTimeout: time.Second, AudioTimeout: time.Second, RecoveryWindow: time.Second, KeyframeTimeout: time.Second}}
	stream := core.Stream{Record: contractRecord("stream"), Name: "Поток", PublicationKey: "synthetic-key", Fallback: policy}
	group := core.TranscodeGroup{Record: contractRecord("group"), StreamID: stream.ID, ProfileID: "profile", Name: "Группа"}
	profile := core.TranscodeProfile{Record: contractRecord(group.ProfileID), Name: "Профиль", Profile: p}
	manifest := core.ArtifactManifest{VariantID: "variant", AssetID: policy.AssetID, SourceHash: "synthetic-source-hash", Profile: p, ProfileKey: key, CanonicalProfile: canonical, GeneratorVersion: "generator-1", ManagedPath: "variants/generation.mp4", SizeBytes: 512, ContentHash: "synthetic-content-hash"}
	output := core.OutputSnapshot{
		Destination: core.Destination{Record: contractRecord("destination"), StreamID: stream.ID, GroupID: group.ID, Name: "Назначение", Platform: "custom", Endpoint: "rtmps://example.invalid/live", StreamKey: "synthetic-output-key", Enabled: true},
		Group:       group, Profile: profile, Fallback: policy, Artifact: manifest,
	}
	return core.SessionSnapshot{Stream: stream, Groups: []core.GroupSnapshot{{Group: group, Profile: profile}}}, output
}

func TestRecordRejectsInvalidIdentityRevisionAndChronology(t *testing.T) {
	base := contractRecord("record")
	for _, revision := range []uint64{1, math.MaxUint64} {
		value := base
		value.Revision = revision
		if err := value.Validate(); err != nil {
			t.Fatalf("допустимая версия %d: %v", revision, err)
		}
	}
	for _, tc := range []struct {
		name string
		edit func(*core.Record)
	}{
		{"missing_id", func(v *core.Record) { v.ID = "" }},
		{"zero_revision", func(v *core.Record) { v.Revision = 0 }},
		{"missing_creation", func(v *core.Record) { v.CreatedAt = time.Time{} }},
		{"missing_update", func(v *core.Record) { v.UpdatedAt = time.Time{} }},
		{"update_before_creation", func(v *core.Record) { v.UpdatedAt = v.CreatedAt.Add(-time.Microsecond) }},
		{"submicrosecond_creation", func(v *core.Record) { v.CreatedAt = v.CreatedAt.Add(time.Nanosecond) }},
		{"submicrosecond_update", func(v *core.Record) { v.UpdatedAt = v.UpdatedAt.Add(time.Nanosecond) }},
		{"unrepresentable_creation", func(v *core.Record) { v.CreatedAt = time.Unix(math.MaxInt64, 0) }},
		{"unrepresentable_update", func(v *core.Record) { v.UpdatedAt = time.Unix(math.MaxInt64, 0) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := base
			tc.edit(&value)
			if err := value.Validate(); !errors.Is(err, core.ErrInvalid) {
				t.Fatalf("некорректная запись принята: %+v, %v", value, err)
			}
		})
	}
}

func TestSessionSnapshotRejectsForeignAndDuplicateGroups(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*core.SessionSnapshot)
	}{
		{"invalid_stream", func(v *core.SessionSnapshot) { v.Stream.Name = "" }},
		{"missing_stream_id", func(v *core.SessionSnapshot) { v.Stream.ID = "" }},
		{"foreign_stream", func(v *core.SessionSnapshot) { v.Groups[0].Group.StreamID = "other" }},
		{"foreign_profile", func(v *core.SessionSnapshot) { v.Groups[0].Group.ProfileID = "other" }},
		{"duplicate_group", func(v *core.SessionSnapshot) { v.Groups = append(v.Groups, v.Groups[0]) }},
		{"invalid_group", func(v *core.SessionSnapshot) { v.Groups[0].Group.Name = "" }},
		{"invalid_profile", func(v *core.SessionSnapshot) { v.Groups[0].Profile.Name = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value, _ := contractSnapshots(t)
			if err := value.Validate(); err != nil {
				t.Fatal(err)
			}
			tc.edit(&value)
			if err := value.Validate(); !errors.Is(err, core.ErrInvalid) {
				t.Fatalf("несогласованный начальный снимок принят: %v", err)
			}
		})
	}
}

func TestOutputSnapshotRequiresOneConsistentFallbackProfile(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*core.OutputSnapshot)
	}{
		{"invalid_destination", func(v *core.OutputSnapshot) { v.Destination.Name = "" }},
		{"invalid_group", func(v *core.OutputSnapshot) { v.Group.Name = "" }},
		{"invalid_profile", func(v *core.OutputSnapshot) { v.Profile.Name = "" }},
		{"invalid_fallback", func(v *core.OutputSnapshot) { v.Fallback.AssetID = "" }},
		{"invalid_manifest", func(v *core.OutputSnapshot) { v.Artifact.SizeBytes = 0 }},
		{"foreign_group", func(v *core.OutputSnapshot) { v.Destination.GroupID = "other" }},
		{"foreign_stream", func(v *core.OutputSnapshot) { v.Destination.StreamID = "other" }},
		{"foreign_profile", func(v *core.OutputSnapshot) { v.Group.ProfileID = "other" }},
		{"foreign_asset", func(v *core.OutputSnapshot) { v.Fallback.AssetID = "other" }},
		{"different_full_profile", func(v *core.OutputSnapshot) { v.Profile.Profile.Format.Video.Width++ }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, value := contractSnapshots(t)
			if err := value.Validate(); err != nil {
				t.Fatal(err)
			}
			tc.edit(&value)
			if err := value.Validate(); !errors.Is(err, core.ErrInvalid) {
				t.Fatalf("несогласованный снимок выхода принят: %v", err)
			}
		})
	}
}

func TestArtifactManifestRequiresExactIdentityAndOwnedFile(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*core.ArtifactManifest)
	}{
		{"variant", func(v *core.ArtifactManifest) { v.VariantID = "" }},
		{"asset", func(v *core.ArtifactManifest) { v.AssetID = "" }},
		{"profile", func(v *core.ArtifactManifest) { v.Profile.Mode = "unknown" }},
		{"profile_digest", func(v *core.ArtifactManifest) { v.ProfileKey = "other" }},
		{"canonical_content", func(v *core.ArtifactManifest) { v.CanonicalProfile = []byte("other") }},
		{"size", func(v *core.ArtifactManifest) { v.SizeBytes = 0 }},
		{"source_hash", func(v *core.ArtifactManifest) { v.SourceHash = "" }},
		{"content_hash", func(v *core.ArtifactManifest) { v.ContentHash = "" }},
		{"generator", func(v *core.ArtifactManifest) { v.GeneratorVersion = "" }},
		{"managed_path", func(v *core.ArtifactManifest) { v.ManagedPath = "../generation.mp4" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, snapshot := contractSnapshots(t)
			value := snapshot.Artifact
			if err := value.Validate(); err != nil {
				t.Fatal(err)
			}
			tc.edit(&value)
			category := core.ErrInvalid
			if tc.name == "profile" {
				category = core.ErrInvalidProfile
			}
			if err := value.Validate(); !errors.Is(err, category) {
				t.Fatalf("неполный manifest принят: %v", err)
			}
		})
	}
}

func contractReceipt() core.CommandReceipt {
	identity := []byte(`{"Version":1,"Kind":"start_session","TargetID":"stream","DestinationIDs":null}`)
	digest := sha256.Sum256(identity)
	return core.CommandReceipt{Record: contractRecord("command"), RequestID: "request", Kind: core.CommandStartSession, TargetID: "stream", Identity: identity, Fingerprint: hex.EncodeToString(digest[:]), SessionID: "session", Incarnation: "owner", Lifecycle: 1, Status: core.CommandPending}
}

func TestCommandReceiptRequiresIdentityAndCurrentOwnership(t *testing.T) {
	for _, tc := range []struct {
		kind     core.CommandKind
		target   core.ID
		output   core.ID
		identity string
	}{
		{core.CommandStartSession, "stream", "", `{"Version":1,"Kind":"start_session","TargetID":"stream","DestinationIDs":null}`},
		{core.CommandStopSession, "session", "", `{"Version":1,"Kind":"stop_session","TargetID":"session","DestinationIDs":null}`},
		{core.CommandPauseSession, "session", "", `{"Version":1,"Kind":"pause_session","TargetID":"session","DestinationIDs":null}`},
		{core.CommandResumeSession, "session", "", `{"Version":1,"Kind":"resume_session","TargetID":"session","DestinationIDs":null}`},
		{core.CommandAddOutput, "session", "output", `{"Version":1,"Kind":"add_output","TargetID":"session","DestinationIDs":["destination"]}`},
		{core.CommandStartOutput, "output", "output", `{"Version":1,"Kind":"start_output","TargetID":"output","DestinationIDs":null}`},
		{core.CommandStopOutput, "output", "output", `{"Version":1,"Kind":"stop_output","TargetID":"output","DestinationIDs":null}`},
		{core.CommandDetachOutput, "output", "output", `{"Version":1,"Kind":"detach_output","TargetID":"output","DestinationIDs":null}`},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			value := contractReceipt()
			value.Kind, value.TargetID, value.OutputID = tc.kind, tc.target, tc.output
			if tc.output != "" {
				value.OutputGeneration = 1
			}
			value.Identity = []byte(tc.identity)
			digest := sha256.Sum256(value.Identity)
			value.Fingerprint = hex.EncodeToString(digest[:])
			if err := value.Validate(); err != nil {
				t.Fatalf("вид команды %s отклонён: %v", tc.kind, err)
			}
		})
	}
	for _, tc := range []struct {
		status  core.CommandStatus
		attempt uint64
		failure string
	}{
		{core.CommandPending, 0, ""},
		{core.CommandClaimed, 1, ""},
		{core.CommandIssued, 1, ""},
		{core.CommandSucceeded, 1, ""},
		{core.CommandFailed, 1, "Не удалось запустить сессию"},
		{core.CommandInterrupted, 1, "Потерян владелец исполнения"},
	} {
		t.Run(string(tc.status), func(t *testing.T) {
			value := contractReceipt()
			value.Status, value.Attempt, value.Failure = tc.status, tc.attempt, tc.failure
			if err := value.Validate(); err != nil {
				t.Fatalf("состояние команды %s отклонено: %v", tc.status, err)
			}
		})
	}
	for _, tc := range []struct {
		name string
		edit func(*core.CommandReceipt)
	}{
		{"record", func(v *core.CommandReceipt) { v.Revision = 0 }},
		{"request", func(v *core.CommandReceipt) { v.RequestID = "" }},
		{"target", func(v *core.CommandReceipt) { v.TargetID = "" }},
		{"session", func(v *core.CommandReceipt) { v.SessionID = "" }},
		{"incarnation", func(v *core.CommandReceipt) { v.Incarnation = "" }},
		{"lifecycle", func(v *core.CommandReceipt) { v.Lifecycle = 0 }},
		{"identity", func(v *core.CommandReceipt) { v.Identity = nil }},
		{"kind", func(v *core.CommandReceipt) { v.Kind = "unknown" }},
		{"status", func(v *core.CommandReceipt) { v.Status = "unknown" }},
		{"fingerprint", func(v *core.CommandReceipt) { v.Fingerprint = "different" }},
		{"changed_request", func(v *core.CommandReceipt) { v.Identity = []byte("other request") }},
		{"invalid_output", func(v *core.CommandReceipt) { v.OutputID = "\x00"; v.OutputGeneration = 1 }},
		{"missing_output_generation", func(v *core.CommandReceipt) { v.OutputID = "output" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := contractReceipt()
			tc.edit(&value)
			if err := value.Validate(); !errors.Is(err, core.ErrInvalid) {
				t.Fatalf("неполная квитанция принята: %v", err)
			}
		})
	}
	value := contractReceipt()
	value.Kind, value.TargetID = core.CommandStartOutput, "output"
	value.OutputID, value.OutputGeneration = "output", math.MaxUint64
	value.Identity = []byte(`{"Version":1,"Kind":"start_output","TargetID":"output","DestinationIDs":null}`)
	digest := sha256.Sum256(value.Identity)
	value.Fingerprint = hex.EncodeToString(digest[:])
	if err := value.Validate(); err != nil {
		t.Fatalf("квитанция выхода отклонена: %v", err)
	}
}

func TestSessionAndOutputRequireValidSavedIntent(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*core.Session)
	}{
		{"active_failure", func(v *core.Session) { v.Failure = "ошибка" }},
		{"record", func(v *core.Session) { v.ID = "" }},
		{"stream", func(v *core.Session) { v.StreamID = "" }},
		{"owner", func(v *core.Session) { v.Incarnation = "" }},
		{"state", func(v *core.Session) { v.State = "unknown" }},
		{"lifecycle", func(v *core.Session) { v.Lifecycle = 0 }},
		{"snapshot_stream", func(v *core.Session) { v.StreamID = "foreign" }},
		{"end_time_overflow", func(v *core.Session) { value := time.Unix(math.MaxInt64, 0); v.EndedAt = &value }},
		{"snapshot", func(v *core.Session) { v.Snapshot.Stream.Name = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			snapshot, _ := contractSnapshots(t)
			value := core.Session{Record: contractRecord("session"), StreamID: snapshot.Stream.ID, Incarnation: "owner", State: core.SessionRunning, Lifecycle: 1, Snapshot: snapshot}
			if err := value.Validate(); err != nil {
				t.Fatal(err)
			}
			tc.edit(&value)
			if err := value.Validate(); !errors.Is(err, core.ErrInvalid) {
				t.Fatalf("некорректная Session принята: %v", err)
			}
		})
	}
	snapshot, output := contractSnapshots(t)
	ended := contractRecord("session").UpdatedAt.Add(time.Second)
	terminal := core.Session{Record: contractRecord("session"), StreamID: snapshot.Stream.ID, Incarnation: "owner", State: core.SessionFailed, Lifecycle: 1, Snapshot: snapshot, Failure: "итоговый отказ", EndedAt: &ended}
	if err := terminal.Validate(); err != nil {
		t.Fatalf("итоговый отказ отклонён: %v", err)
	}
	base := core.SessionOutput{Record: contractRecord("output"), SessionID: "session", Snapshot: output, Attached: true, DesiredRunning: true, Generation: 1}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, edit := range []func(*core.SessionOutput){
		func(v *core.SessionOutput) { v.ID = "" },
		func(v *core.SessionOutput) { v.SessionID = "" },
		func(v *core.SessionOutput) { v.Generation = 0 },
		func(v *core.SessionOutput) { v.Attached = false },
		func(v *core.SessionOutput) { v.Snapshot.Destination.Name = "" },
	} {
		value := base
		edit(&value)
		if err := value.Validate(); !errors.Is(err, core.ErrInvalid) {
			t.Fatalf("некорректное намерение выхода принято: %v", err)
		}
	}
	base.Attached, base.DesiredRunning = false, false
	if err := base.Validate(); err != nil {
		t.Fatalf("отсоединённый остановленный выход отклонён: %v", err)
	}
}

func TestCleanupAndReferenceHoldsRequireOwnedDependencies(t *testing.T) {
	file := core.CleanupIntent{Record: contractRecord("cleanup"), Kind: core.CleanupFile, ManagedPath: "variants/retired.mp4"}
	runtime := core.CleanupIntent{Record: contractRecord("runtime-cleanup"), Kind: core.CleanupRuntime, StreamID: "stream", SessionID: "session", Incarnation: "owner", Lifecycle: 1}
	for _, value := range []core.CleanupIntent{file, runtime} {
		if err := value.Validate(); err != nil {
			t.Fatalf("допустимое намерение очистки отклонено: %v", err)
		}
	}
	for _, edit := range []func(*core.CleanupIntent){
		func(v *core.CleanupIntent) { v.ID = "" },
		func(v *core.CleanupIntent) { v.Kind = "unknown" },
		func(v *core.CleanupIntent) { v.StreamID = "" },
		func(v *core.CleanupIntent) { v.SessionID = "" },
		func(v *core.CleanupIntent) { v.Incarnation = "" },
		func(v *core.CleanupIntent) { v.Lifecycle = 0 },
		func(v *core.CleanupIntent) { v.OutputID = "output" },
	} {
		value := runtime
		edit(&value)
		if err := value.Validate(); !errors.Is(err, core.ErrInvalid) {
			t.Fatalf("неполное владение очисткой принято: %v", err)
		}
	}
	file.ManagedPath = "../unowned.mp4"
	if err := file.Validate(); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("очистка чужого файла принята: %v", err)
	}
	runtime.OutputID, runtime.Generation = "output", 1
	if err := runtime.Validate(); err != nil {
		t.Fatal(err)
	}
	hold := core.ReferenceHold{SessionID: "session", StreamID: "stream", AssetID: "asset"}
	if err := hold.Validate(); err != nil {
		t.Fatalf("удержание исходника Session отклонено: %v", err)
	}
	hold.OutputID, hold.GroupID, hold.ProfileID, hold.DestinationID, hold.VariantID = "output", "group", "profile", "destination", "variant"
	if err := hold.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, edit := range []func(*core.ReferenceHold){
		func(v *core.ReferenceHold) { v.SessionID = "" }, func(v *core.ReferenceHold) { v.StreamID = "" },
		func(v *core.ReferenceHold) { v.AssetID = "" }, func(v *core.ReferenceHold) { v.OutputID = "\x00" },
		func(v *core.ReferenceHold) { v.GroupID = "" }, func(v *core.ReferenceHold) { v.ProfileID = "" },
		func(v *core.ReferenceHold) { v.DestinationID = "" }, func(v *core.ReferenceHold) { v.VariantID = "" },
	} {
		value := hold
		edit(&value)
		if err := value.Validate(); !errors.Is(err, core.ErrInvalid) {
			t.Fatalf("неполное удержание принято: %v", err)
		}
	}
}

func TestExecutionFenceRequiresExactSessionAndOutputGeneration(t *testing.T) {
	base := core.ExecutionFence{CommandID: "command", SessionID: "session", Incarnation: "owner", Lifecycle: 1, Attempt: 1}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, edit := range []func(*core.ExecutionFence){
		func(v *core.ExecutionFence) { v.CommandID = "" }, func(v *core.ExecutionFence) { v.SessionID = "" },
		func(v *core.ExecutionFence) { v.Incarnation = "" }, func(v *core.ExecutionFence) { v.Lifecycle = 0 },
		func(v *core.ExecutionFence) { v.Attempt = 0 }, func(v *core.ExecutionFence) { v.OutputGeneration = 1 },
		func(v *core.ExecutionFence) { v.OutputID = "output" },
		func(v *core.ExecutionFence) { v.OutputID = "\x00"; v.OutputGeneration = 1 },
	} {
		value := base
		edit(&value)
		if err := value.Validate(); !errors.Is(err, core.ErrInvalid) {
			t.Fatalf("неполный fence принят: %v", err)
		}
	}
	base.OutputID, base.OutputGeneration = "output", math.MaxUint64
	if err := base.Validate(); err != nil {
		t.Fatalf("точный fence выхода отклонён: %v", err)
	}
}

func TestFallbackAssetSeparatesPreparingAndReadyOriginalMetadata(t *testing.T) {
	base := core.FallbackAsset{Record: contractRecord("asset"), Incarnation: "owner", Name: "Исходник", Kind: core.AssetImage, State: core.AssetPreparing, AttemptToken: "attempt", OriginalPath: "originals/source.png"}
	for _, state := range []core.AssetState{core.AssetPreparing, core.AssetFailed} {
		value := base
		value.State = state
		if err := value.Validate(); err != nil {
			t.Fatalf("неготовый исходник обязан допускать отсутствие probe: %v", err)
		}
	}
	base.State, base.SourceHash, base.SizeBytes, base.SourceFormat = core.AssetReady, "synthetic-source-hash", 1, core.SourceFormat{ImageFormat: "png"}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	jpeg := base
	jpeg.SourceFormat.ImageFormat = "jpeg"
	if err := jpeg.Validate(); err != nil {
		t.Fatalf("JPEG отклонён: %v", err)
	}
	video := base
	video.Kind, video.OriginalPath = core.AssetVideo, "originals/source.webm"
	video.SourceFormat = core.SourceFormat{
		Video: &core.VideoFormat{Codec: "vp9", Width: 1280, Height: 720, FrameRate: core.FrameRate{Numerator: 30, Denominator: 1}, CodecProfile: "0", Level: "3.1", PixelFormat: "yuv420p", BitDepth: 8, Color: core.ColorInfo{Range: "tv", Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"}},
		Audio: &core.AudioFormat{Codec: "opus", SampleRateHz: 48000, Channels: 2, ChannelLayout: "stereo"},
	}
	if err := video.Validate(); err != nil {
		t.Fatalf("описание видео-оригинала подменено ограничениями выходного профиля: %v", err)
	}
	for _, tc := range []struct {
		name string
		edit func(*core.FallbackAsset)
	}{
		{"owner", func(v *core.FallbackAsset) { v.Incarnation = "" }},
		{"name", func(v *core.FallbackAsset) { v.Name = "" }},
		{"path", func(v *core.FallbackAsset) { v.OriginalPath = "../other" }},
		{"attempt", func(v *core.FallbackAsset) { v.AttemptToken = "" }},
		{"kind", func(v *core.FallbackAsset) { v.Kind = "unknown" }},
		{"state", func(v *core.FallbackAsset) { v.State = "unknown" }},
		{"hash", func(v *core.FallbackAsset) { v.SourceHash = "" }},
		{"size", func(v *core.FallbackAsset) { v.SizeBytes = 0 }},
		{"image_type", func(v *core.FallbackAsset) { v.SourceFormat.ImageFormat = "gif" }},
		{"image_description", func(v *core.FallbackAsset) { v.SourceFormat.ImageFormat = "\x00" }},
		{"video_description", func(v *core.FallbackAsset) { v.SourceFormat.Video = &core.VideoFormat{Codec: ""} }},
		{"audio_description", func(v *core.FallbackAsset) { v.SourceFormat.Audio = &core.AudioFormat{Codec: ""} }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := base
			tc.edit(&value)
			if err := value.Validate(); !errors.Is(err, core.ErrInvalid) {
				t.Fatalf("некорректный исходник принят: %v", err)
			}
		})
	}
}

func TestFallbackVariantRequiresExactProfileForEveryPreparationState(t *testing.T) {
	_, output := contractSnapshots(t)
	m := output.Artifact
	base := core.FallbackVariant{Record: contractRecord(m.VariantID), Incarnation: "owner", AssetID: m.AssetID, SourceHash: m.SourceHash, Profile: m.Profile, ProfileKey: m.ProfileKey, CanonicalProfile: m.CanonicalProfile, GeneratorVersion: m.GeneratorVersion, ManagedPath: m.ManagedPath, SizeBytes: m.SizeBytes, ContentHash: m.ContentHash, State: core.VariantReady, AttemptToken: "attempt"}
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	for _, state := range []core.VariantState{core.VariantPreparing, core.VariantFailed} {
		value := base
		value.State, value.SizeBytes, value.ContentHash = state, 0, ""
		if err := value.Validate(); err != nil {
			t.Fatalf("неготовое поколение требует готовый файл: %v", err)
		}
		value.CanonicalProfile = []byte("different-profile")
		if err := value.Validate(); !errors.Is(err, core.ErrInvalid) {
			t.Fatalf("неготовое поколение потеряло точную идентичность: %v", err)
		}
	}
	for _, tc := range []struct {
		name string
		edit func(*core.FallbackVariant)
	}{
		{"owner", func(v *core.FallbackVariant) { v.Incarnation = "" }},
		{"asset", func(v *core.FallbackVariant) { v.AssetID = "" }},
		{"source_hash", func(v *core.FallbackVariant) { v.SourceHash = "" }},
		{"generator", func(v *core.FallbackVariant) { v.GeneratorVersion = "" }},
		{"path", func(v *core.FallbackVariant) { v.ManagedPath = "../other" }},
		{"attempt", func(v *core.FallbackVariant) { v.AttemptToken = "" }},
		{"state", func(v *core.FallbackVariant) { v.State = "unknown" }},
		{"identity", func(v *core.FallbackVariant) { v.ProfileKey = "different" }},
		{"size", func(v *core.FallbackVariant) { v.SizeBytes = 0 }},
		{"content_hash", func(v *core.FallbackVariant) { v.ContentHash = "" }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := base
			tc.edit(&value)
			if err := value.Validate(); !errors.Is(err, core.ErrInvalid) {
				t.Fatalf("некорректное поколение принято: %v", err)
			}
		})
	}
}
