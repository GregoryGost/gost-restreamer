package core_test

import (
	"errors"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

func validGeneration(t *testing.T) core.FallbackGeneration {
	t.Helper()
	profile := validProfile()
	key, b, err := core.ProfileIdentity(profile)
	if err != nil {
		t.Fatal(err)
	}
	return core.FallbackGeneration{Record: contractRecord("generation"), AssetID: "asset", Kind: core.AssetImage, OriginalPath: "fallback/generation/original", StagingPath: "fallback/generation/original.stage", SourceHash: "source", SizeBytes: 12, SourceFormat: core.SourceFormat{ImageFormat: "png"}, GeneratorVersion: "generator", Incarnation: "owner", AttemptToken: "token", Attempt: 1, State: core.PreparationPublished, RequiredProfiles: []core.RequiredProfile{{Profile: profile, ProfileKey: key, CanonicalProfile: b, VariantID: "variant", ManagedPath: "fallback/generation/variant.mp4", StagingPath: "fallback/generation/variant.stage"}}}
}
func TestGenerationValidationPreservesSourceAndExactOwnedProfileSet(t *testing.T) {
	for _, tc := range []struct {
		name string
		edit func(*core.FallbackGeneration)
	}{
		{"asset", func(g *core.FallbackGeneration) { g.AssetID = "" }},
		{"kind", func(g *core.FallbackGeneration) { g.Kind = "other" }},
		{"state", func(g *core.FallbackGeneration) { g.State = "other" }},
		{"owner", func(g *core.FallbackGeneration) { g.Incarnation = "" }},
		{"attempt", func(g *core.FallbackGeneration) { g.AttemptToken = "" }},
		{"generator", func(g *core.FallbackGeneration) { g.GeneratorVersion = "" }},
		{"source_path", func(g *core.FallbackGeneration) { g.OriginalPath = "../outside" }},
		{"foreign_path", func(g *core.FallbackGeneration) { g.OriginalPath = "fallback/foreign/original" }},
		{"profile", func(g *core.FallbackGeneration) { g.RequiredProfiles[0].Profile.Mode = "other" }},
		{"digest", func(g *core.FallbackGeneration) { g.RequiredProfiles[0].ProfileKey = "other" }},
		{"canonical", func(g *core.FallbackGeneration) { g.RequiredProfiles[0].CanonicalProfile = []byte("other") }},
		{"duplicate_profile", func(g *core.FallbackGeneration) {
			g.RequiredProfiles = append(g.RequiredProfiles, g.RequiredProfiles[0])
		}},
		{"missing_variant", func(g *core.FallbackGeneration) { g.RequiredProfiles[0].VariantID = "" }},
		{"variant_path", func(g *core.FallbackGeneration) { g.RequiredProfiles[0].ManagedPath = "../outside" }},
		{"foreign_variant_path", func(g *core.FallbackGeneration) { g.RequiredProfiles[0].ManagedPath = "fallback/foreign/variant.mp4" }},
		{"invalid_source", func(g *core.FallbackGeneration) { g.SourceFormat.ImageFormat = "\x00" }},
		{"source_size", func(g *core.FallbackGeneration) { g.SizeBytes = 0 }},
		{"source_hash", func(g *core.FallbackGeneration) { g.SourceHash = "" }},
		{"image_type", func(g *core.FallbackGeneration) { g.SourceFormat.ImageFormat = "gif" }},
		{"issued_attempt", func(g *core.FallbackGeneration) { g.Issued = true; g.Attempt = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			g := validGeneration(t)
			if err := g.Validate(); err != nil {
				t.Fatal(err)
			}
			tc.edit(&g)
			if err := g.Validate(); err == nil {
				t.Fatal("неполное поколение принято")
			}
		})
	}
	g := validGeneration(t)
	g.State = core.PreparationQueued
	g.SourceHash = ""
	g.SizeBytes = 0
	g.SourceFormat = core.SourceFormat{}
	g.Attempt = 0
	if err := g.Validate(); err != nil {
		t.Fatal("резервирование требует ещё не проверенные медиаданные", err)
	}
}
func TestOwnedFileValidationRejectsForeignNamesAndIncompleteIntegrity(t *testing.T) {
	base := core.OwnedFile{Record: contractRecord("file"), AssetID: "asset", GenerationID: "generation", ManagedPath: "fallback/generation/original", Kind: core.OwnedOriginal, SizeBytes: 5, ContentHash: "hash"}
	for _, edit := range []func(*core.OwnedFile){
		func(f *core.OwnedFile) { f.AssetID = "" },
		func(f *core.OwnedFile) { f.GenerationID = "" },
		func(f *core.OwnedFile) { f.ManagedPath = "../other" },
		func(f *core.OwnedFile) { f.ManagedPath = "fallback/foreign/original" },
		func(f *core.OwnedFile) { f.Kind = "other" },
		func(f *core.OwnedFile) { f.Kind = core.OwnedDirectory },
		func(f *core.OwnedFile) { f.ContentHash = "" },
		func(f *core.OwnedFile) { f.SizeBytes = 0 },
	} {
		f := base
		edit(&f)
		if err := f.Validate(); !errors.Is(err, core.ErrInvalid) {
			t.Fatalf("некорректный принадлежащий файл принят: %v", err)
		}
	}
	for _, kind := range []core.OwnedFileKind{core.OwnedDirectory, core.OwnedStaging, core.OwnedOriginal, core.OwnedVariant} {
		f := base
		f.Kind = kind
		f.ContentHash = ""
		f.SizeBytes = 0
		if err := f.Validate(); err != nil {
			t.Fatal("резервирование отклонено", err)
		}
	}
}
func TestPreparationLimitsRequireExplicitFinitePositiveBudgets(t *testing.T) {
	for _, l := range []core.PreparationLimits{{}, {MaxQueued: 1}, {MaxProfiles: 1}, {MaxQueued: 1, MaxProfiles: -1}, {MaxQueued: core.MaxListLimit + 1, MaxProfiles: 1}, {MaxQueued: 1, MaxProfiles: core.MaxListLimit + 1}} {
		if err := l.Validate(); !errors.Is(err, core.ErrInvalid) {
			t.Fatal("неверный бюджет принят", l, err)
		}
	}
	if err := (core.PreparationLimits{MaxQueued: 1, MaxProfiles: 1}).Validate(); err != nil {
		t.Fatal(err)
	}
}
