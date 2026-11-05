package processowner

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
)

func TestRuntimeResourcesAreExhaustiveAndOwned(t *testing.T) {
	r := testRegistry(t, buildHelper(t))
	s := testScope(t, r)
	ctx := testContext(t)
	if r.PageLimit() != testLimits().PageSize {
		t.Fatal("page limit mismatch")
	}
	owner, err := s.Owner()
	if err != nil || owner == "" {
		t.Fatalf("owner=%q %v", owner, err)
	}
	if _, err = s.RuntimeIdentity("../escape", RegularFile); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	ticket, err := s.Register(ctx, "runtime-builder")
	if err != nil {
		t.Fatal(err)
	}
	if err = os.Mkdir(filepath.Join(s.Namespace(), "directory"), 0700); err != nil {
		t.Fatal(err)
	}
	if err = s.RegisterRuntime(ctx, "directory", Directory); err != nil {
		t.Fatal(err)
	}
	testFile(t, s.Namespace(), "directory/file", "runtime data")
	if err = s.RegisterRuntime(ctx, "directory/file", RegularFile); err != nil {
		t.Fatal(err)
	}
	id, err := s.RuntimeIdentity("directory/file", RegularFile)
	if err != nil {
		t.Fatal(err)
	}
	if err = s.RuntimeJournal().Verify(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err = s.Seal(ctx); err != nil {
		t.Fatal(err)
	}
	proof, err := s.Resolve(ctx)
	if err != nil || proof.Joined || proof.PendingOperations == 0 {
		t.Fatalf("builder not joined: %+v %v", proof, err)
	}
	if _, err = os.Stat(filepath.Join(s.Namespace(), "directory/file")); err != nil {
		t.Fatal("reaper raced live backend writer", err)
	}
	if err = ticket.Complete(ctx); err != nil {
		t.Fatal(err)
	}
	proof, err = s.Resolve(ctx)
	if err != nil || !proof.Joined {
		t.Fatalf("proof=%+v %v", proof, err)
	}
	if err = r.AcknowledgeScope(ctx, s.ID()); err != nil {
		t.Fatal(err)
	}
	if err = r.AcknowledgeScope(ctx, s.ID()); err != nil {
		t.Fatal(err)
	}
	if err = s.RegisterRuntime(ctx, "late", RegularFile); !errors.Is(err, ErrClosed) {
		t.Fatalf("late runtime registration=%v", err)
	}
	if _, _, err = r.RecoverPage(ctx, "invalid", 1); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, _, err = r.RecoverPage(ctx, "", 0); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err = r.OpenScope(ctx, "bad"); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err = r.Create(ctx, ""); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if _, err = r.Create(cancelled, "owner"); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
}

func TestCanceledCompletionRemainsRetryableWithoutInventingJoin(t *testing.T) {
	r := testRegistry(t, buildHelper(t))
	s := testScope(t, r)
	ctx := testContext(t)
	returned, err := s.Register(ctx, "returned-builder")
	if err != nil {
		t.Fatal(err)
	}
	unfinished, err := s.Register(ctx, "unfinished-builder")
	if err != nil {
		t.Fatal(err)
	}
	cancelled, cancel := context.WithCancel(ctx)
	cancel()
	if err = returned.Complete(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("first completion=%v", err)
	}
	if err = s.CloseContext(cancelled); !errors.Is(err, context.Canceled) {
		t.Fatalf("bounded close=%v", err)
	}
	if _, err = s.Register(ctx, "late-builder"); !errors.Is(err, ErrClosed) {
		t.Fatalf("registration remained open: %v", err)
	}
	if err = s.RetryCompleted(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.CloseContext(ctx); !errors.Is(err, ErrPending) {
		t.Fatalf("unfinished operation falsely joined: %v", err)
	}
	proof, err := s.Resolve(ctx)
	if err != nil || proof.Joined || proof.PendingOperations != 1 {
		t.Fatalf("proof=%+v %v", proof, err)
	}
	if err = unfinished.Complete(ctx); err != nil {
		t.Fatal(err)
	}
	if err = s.CloseContext(ctx); err != nil {
		t.Fatal(err)
	}
	proof, err = s.Resolve(ctx)
	if err != nil || !proof.Joined {
		t.Fatalf("proof after retry=%+v %v", proof, err)
	}
}
