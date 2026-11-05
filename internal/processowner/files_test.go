package processowner

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
)

func testFile(t *testing.T, root, name, content string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}
func testFileID(name, kind string) FileIdentity {
	return FileIdentity{FileID: "file:" + name, GenerationID: "generation", ManagedPath: name, Kind: kind, Owner: "owner"}
}
func testJournal(t *testing.T, root string) *FileJournal {
	t.Helper()
	j, err := NewFileJournal(root, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := j.Close(); err != nil {
			t.Error(err)
		}
	})
	return j
}

func TestFileJournalIdentity(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	id := testFileID("file", RegularFile)
	testFile(t, root, "file", "first inode")
	j := testJournal(t, root)
	if err := j.Register(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := j.Verify(ctx, id); err != nil {
		t.Fatal(err)
	}
	reopened := testJournal(t, root)
	if err := reopened.Verify(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(filepath.Join(root, "file"), filepath.Join(root, "original")); err != nil {
		t.Fatal(err)
	}
	testFile(t, root, "file", "replacement")
	if err := reopened.Verify(ctx, id); !errors.Is(err, ErrUnknown) {
		t.Fatalf("replacement verified: %v", err)
	}
	if err := reopened.Remove(ctx, id); !errors.Is(err, ErrUnknown) {
		t.Fatalf("replacement removed: %v", err)
	}
	b, err := os.ReadFile(filepath.Join(root, "file"))
	if err != nil || string(b) != "replacement" {
		t.Fatalf("replacement changed: %q %v", b, err)
	}
	if err = os.Remove(filepath.Join(root, "file")); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(filepath.Join(root, "original"), filepath.Join(root, "file")); err != nil {
		t.Fatal(err)
	}
	if err = reopened.Remove(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(root, evidenceDirectory, "anchors", fileKey(id))); err != nil {
		t.Fatal("anchor lost before FILE ack", err)
	}
	if err = reopened.Acknowledge(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err = reopened.Acknowledge(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(root, evidenceDirectory, "anchors", fileKey(id))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("anchor retained after FILE ack", err)
	}
	testFile(t, root, "file", "reused path")
	if err = reopened.Register(ctx, id); !errors.Is(err, ErrClosed) {
		t.Fatalf("resurrected acknowledged FileID: %v", err)
	}
}

func TestFileJournalRenameRecovery(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	j := testJournal(t, root)
	source := testFileID("staging", RegularFile)
	target := testFileID("final", RegularFile)
	testFile(t, root, "staging", "media payload")
	if err := j.Register(ctx, source); err != nil {
		t.Fatal(err)
	}
	if err := j.Rename(ctx, source, target); err != nil {
		t.Fatal(err)
	}
	j = testJournal(t, root)
	if err := j.Verify(ctx, target); err != nil {
		t.Fatal(err)
	}
	if err := j.Rename(ctx, source, target); err != nil {
		t.Fatal(err)
	}
	if err := j.Remove(ctx, source); err != nil {
		t.Fatal(err)
	}
	if err := j.Acknowledge(ctx, source); err != nil {
		t.Fatal(err)
	}
	if err := j.Verify(ctx, target); err != nil {
		t.Fatal("source FILE ack lost final anchor", err)
	}
	if err := j.Remove(ctx, target); err != nil {
		t.Fatal(err)
	}
	if err := j.Acknowledge(ctx, target); err != nil {
		t.Fatal(err)
	}
}

func TestFileJournalDirectoryAndCorruption(t *testing.T) {
	t.Run("reserved-object-never-created", func(t *testing.T) {
		j := testJournal(t, t.TempDir())
		id := testFileID("absent", RegularFile)
		ctx := context.Background()
		if err := j.Reserve(ctx, id); err != nil {
			t.Fatal(err)
		}
		if err := j.Reserve(ctx, id); err != nil {
			t.Fatal(err)
		}
		if err := j.Remove(ctx, id); err != nil {
			t.Fatal(err)
		}
		if _, err := j.RetryAcknowledgements(ctx, "", 16); err != nil {
			t.Fatal(err)
		}
		r, err := j.read(id)
		if err != nil {
			t.Fatal(err)
		}
		if r.Phase != "REMOVED" {
			t.Fatal("SQL ack was inferred from physical removal")
		}
		r.Phase = "ACKING"
		if err = j.write(r); err != nil {
			t.Fatal(err)
		}
		if _, err = j.RetryAcknowledgements(ctx, "", 16); err != nil {
			t.Fatal(err)
		}
		if err = j.Register(ctx, id); !errors.Is(err, ErrClosed) {
			t.Fatalf("late register=%v", err)
		}
		if err = j.Reserve(ctx, id); !errors.Is(err, ErrClosed) {
			t.Fatalf("late reserve=%v", err)
		}
	})
	t.Run("directory-removes-only-known-empty-namespace", func(t *testing.T) {
		root := t.TempDir()
		j := testJournal(t, root)
		id := testFileID("directory", Directory)
		if err := os.Mkdir(filepath.Join(root, id.ManagedPath), 0700); err != nil {
			t.Fatal(err)
		}
		if err := j.Register(context.Background(), id); err != nil {
			t.Fatal(err)
		}
		testFile(t, root, "directory/unknown", "unowned")
		if err := j.Remove(context.Background(), id); !errors.Is(err, ErrPending) {
			t.Fatalf("nonempty directory: %v", err)
		}
		if err := os.Remove(filepath.Join(root, "directory/unknown")); err != nil {
			t.Fatal(err)
		}
		if err := j.Remove(context.Background(), id); err != nil {
			t.Fatal(err)
		}
		if err := j.Acknowledge(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("marker-rmdir-crash-is-unknown", func(t *testing.T) {
		root := t.TempDir()
		j := testJournal(t, root)
		id := testFileID("directory", Directory)
		if err := os.Mkdir(filepath.Join(root, id.ManagedPath), 0700); err != nil {
			t.Fatal(err)
		}
		if err := j.Register(context.Background(), id); err != nil {
			t.Fatal(err)
		}
		r, err := j.read(id)
		if err != nil {
			t.Fatal(err)
		}
		r.Phase = "DIR_REMOVING"
		if err = j.write(r); err != nil {
			t.Fatal(err)
		}
		if err = j.root.Remove(r.Marker); err != nil {
			t.Fatal(err)
		}
		if err = j.Remove(context.Background(), id); !errors.Is(err, ErrUnknown) {
			t.Fatalf("ambiguous directory falsely removed: %v", err)
		}
	})
	t.Run("missing-anchor-is-not-legacy", func(t *testing.T) {
		root := t.TempDir()
		j := testJournal(t, root)
		id := testFileID("file", RegularFile)
		exists, err := j.Exists(context.Background(), id)
		if err != nil || exists {
			t.Fatalf("legacy=%v %v", exists, err)
		}
		testFile(t, root, "file", "data")
		if err = j.Register(context.Background(), id); err != nil {
			t.Fatal(err)
		}
		if err = j.root.Remove(evidenceDirectory + "/anchors/" + fileKey(id)); err != nil {
			t.Fatal(err)
		}
		exists, err = j.Exists(context.Background(), id)
		if !exists || !errors.Is(err, ErrUnknown) {
			t.Fatalf("lost anchor treated as legacy: %v %v", exists, err)
		}
	})
	t.Run("symlink-rejected", func(t *testing.T) {
		root := t.TempDir()
		j := testJournal(t, root)
		testFile(t, root, "real", "data")
		if err := os.Symlink("real", filepath.Join(root, "link")); err != nil {
			t.Fatal(err)
		}
		if err := j.Register(context.Background(), testFileID("link", RegularFile)); !errors.Is(err, ErrUnknown) {
			t.Fatalf("symlink registered: %v", err)
		}
	})
}

func TestSocketAnchors(t *testing.T) {
	root, err := os.MkdirTemp("/tmp", "processowner-socket-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.RemoveAll(root) })
	j := testJournal(t, root)
	id := testFileID("socket", Socket)
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(root, "socket"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	t.Cleanup(func() { _ = listener.Close() })
	if err = j.Register(context.Background(), id); err != nil {
		t.Fatal("socket anchor unsupported by test FS", err)
	}
	if err = j.Verify(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err = j.Remove(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if err = j.Acknowledge(context.Background(), id); err != nil {
		t.Fatal(err)
	}
}
