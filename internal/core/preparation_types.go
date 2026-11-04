package core

import (
	"bytes"
	"fmt"
	"strings"
)

// PreparationLimits задаёт неизменяемый общий бюджет очереди и числа профилей.
type PreparationLimits struct {
	MaxQueued   int
	MaxProfiles int
}

func (l PreparationLimits) Validate() error {
	if l.MaxQueued < 1 || l.MaxQueued > MaxListLimit || l.MaxProfiles < 1 || l.MaxProfiles > MaxListLimit {
		return fmt.Errorf("%w: бюджеты подготовки", ErrInvalid)
	}
	return nil
}

type PreparationLimitsProvider interface{ PreparationLimits() PreparationLimits }

type PreparationState string

const (
	PreparationQueued      PreparationState = "QUEUED"
	PreparationClaimed     PreparationState = "CLAIMED"
	PreparationIssued      PreparationState = "ISSUED"
	PreparationPublished   PreparationState = "PUBLISHED"
	PreparationFailed      PreparationState = "FAILED"
	PreparationInterrupted PreparationState = "INTERRUPTED"
)

func (s PreparationState) Active() bool {
	return s == PreparationQueued || s == PreparationClaimed || s == PreparationIssued
}
func (s PreparationState) Terminal() bool {
	return s == PreparationPublished || s == PreparationFailed || s == PreparationInterrupted
}

type RequiredProfile struct {
	Profile          GroupProfile
	ProfileKey       string
	CanonicalProfile []byte
	VariantID        ID
	ManagedPath      string
	StagingPath      string
}

type FallbackGeneration struct {
	Record
	AssetID                 ID
	Kind                    AssetKind
	OriginalPath            string
	StagingPath             string
	SourceHash              string
	SizeBytes               uint64
	SourceFormat            SourceFormat
	GeneratorVersion        string
	RequiredProfiles        []RequiredProfile
	Incarnation             string
	AttemptToken            string
	Attempt                 uint64
	State                   PreparationState
	Issued                  bool
	CleanupPending          bool
	RebuildFromGenerationID ID
	Failure                 string
}

func (g FallbackGeneration) Validate() error {
	if err := g.AssetID.Validate(); err != nil {
		return err
	}
	if g.Kind != AssetImage && g.Kind != AssetVideo {
		return ErrInvalid
	}
	if !g.State.Active() && !g.State.Terminal() {
		return ErrInvalid
	}
	for _, v := range []string{g.Incarnation, g.AttemptToken, g.GeneratorVersion} {
		if err := validText("generation", v); err != nil {
			return err
		}
	}
	for _, p := range []string{g.OriginalPath, g.StagingPath} {
		if err := ValidateManagedPath(p); err != nil {
			return err
		}
		if !strings.HasPrefix(p, GenerationDirectory(g.ID)+"/") {
			return ErrInvalid
		}
	}
	seen := map[string]bool{}
	for _, p := range g.RequiredProfiles {
		k, b, e := ProfileIdentity(p.Profile)
		if e != nil {
			return e
		}
		if k != p.ProfileKey || !bytes.Equal(b, p.CanonicalProfile) || seen[string(b)] || p.VariantID == "" {
			return ErrInvalid
		}
		seen[string(b)] = true
		for _, path := range []string{p.ManagedPath, p.StagingPath} {
			if err := ValidateManagedPath(path); err != nil {
				return err
			}
			if !strings.HasPrefix(path, GenerationDirectory(g.ID)+"/") {
				return ErrInvalid
			}
		}
	}
	if err := g.SourceFormat.Validate(); err != nil {
		return err
	}
	if g.State == PreparationPublished {
		if g.SizeBytes == 0 || g.SourceHash == "" {
			return ErrInvalid
		}
		if g.Kind == AssetImage && g.SourceFormat.ImageFormat != "png" && g.SourceFormat.ImageFormat != "jpeg" {
			return ErrInvalid
		}
	}
	if g.Issued && g.Attempt == 0 {
		return ErrInvalid
	}
	return nil
}
func GenerationDirectory(id ID) string { return "fallback/" + string(id) }

type PreparationFence struct {
	GenerationID ID
	AssetID      ID
	Incarnation  string
	AttemptToken string
	Attempt      uint64
}

func PreparationFenceFor(g FallbackGeneration) PreparationFence {
	return PreparationFence{g.ID, g.AssetID, g.Incarnation, g.AttemptToken, g.Attempt}
}

type PreparationRequest struct {
	AssetID          ID
	ExpectedRevision uint64
	Kind             AssetKind
	GeneratorVersion string
	Rebuild          bool
}
type GenerationPublication struct {
	SourceHash   string
	SizeBytes    uint64
	SourceFormat SourceFormat
	Variants     []FallbackVariant
}

type OwnedFileKind string

const (
	OwnedDirectory OwnedFileKind = "directory"
	OwnedOriginal  OwnedFileKind = "original"
	OwnedVariant   OwnedFileKind = "variant"
	OwnedStaging   OwnedFileKind = "staging"
)

// OwnedFile сохраняет происхождение пути даже после удаления метаданных поколения.
type OwnedFile struct {
	Record
	ContentHash  string
	SizeBytes    uint64
	AssetID      ID
	GenerationID ID
	ManagedPath  string
	Kind         OwnedFileKind
}

func (f OwnedFile) Validate() error {
	if (f.ContentHash == "") != (f.SizeBytes == 0) {
		return ErrInvalid
	}
	if f.ContentHash != "" && f.Kind != OwnedOriginal && f.Kind != OwnedVariant {
		return ErrInvalid
	}
	if err := f.AssetID.Validate(); err != nil {
		return err
	}
	if err := f.GenerationID.Validate(); err != nil {
		return err
	}
	if err := ValidateManagedPath(f.ManagedPath); err != nil {
		return err
	}
	if f.ManagedPath != GenerationDirectory(f.GenerationID) && !strings.HasPrefix(f.ManagedPath, GenerationDirectory(f.GenerationID)+"/") {
		return ErrInvalid
	}
	switch f.Kind {
	case OwnedDirectory, OwnedOriginal, OwnedVariant, OwnedStaging:
		return nil
	default:
		return ErrInvalid
	}
}
