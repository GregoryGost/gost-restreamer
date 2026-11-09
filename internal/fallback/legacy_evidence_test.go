package fallback

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/processowner"
)

func TestRemoveOwnedKeepsSameContentLegacyReplacementWithoutEvidence(t *testing.T) {
	ctx := context.Background()
	directory := t.TempDir()
	files, err := OpenFileStore(directory)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := files.Close(); err != nil {
			t.Error(err)
		}
	})

	const managedPath = "fallback/generation/original"
	if err := os.MkdirAll(filepath.Join(directory, filepath.Dir(managedPath)), 0700); err != nil {
		t.Fatal(err)
	}
	contents := []byte("same bytes as the legacy ledger entry")
	if err := os.WriteFile(filepath.Join(directory, managedPath), contents, 0600); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(contents)
	owned := core.OwnedFile{
		ID:           "legacy-file-id",
		AssetID:      "asset",
		GenerationID: "generation",
		Kind:         core.OwnedOriginal,
		ManagedPath:  managedPath,
		SizeBytes:    uint64(len(contents)),
		ContentHash:  hex.EncodeToString(digest[:]),
	}

	if exists, err := files.FileJournal().Exists(ctx, fileIdentity(owned)); exists || err != nil {
		t.Fatalf("test setup unexpectedly has journal evidence: exists=%t err=%v", exists, err)
	}
	if err := files.RemoveOwnedContext(ctx, owned); !errors.Is(err, ErrCleanupPending) || !errors.Is(err, processowner.ErrUnknown) {
		t.Fatalf("legacy cleanup without physical evidence must remain pending/unknown: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(directory, managedPath))
	if err != nil || string(got) != string(contents) {
		t.Fatalf("replacement object was removed or changed: %q, %v", got, err)
	}
}
