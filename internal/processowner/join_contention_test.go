package processowner

import "testing"

func TestAcknowledgedScopeProofWaitsForContendedOperation(t *testing.T) {
	r := testRegistry(t, boundaryHelperPath(t))
	s := testScope(t, r)
	ctx := testContext(t)
	if err := s.CloseContext(ctx); err != nil {
		t.Fatal(err)
	}
	if err := r.AcknowledgeScope(ctx, s.ID()); err != nil {
		t.Fatal(err)
	}
	before, err := s.Resolve(ctx)
	if err != nil || !before.Joined || !before.Sealed || !before.Acknowledged || before.Unknown {
		t.Fatal("нет исходного доказательства завершения", before, err)
	}
	lock, err := lockExisting(ctx, r.root, operationPath(s.ID(), "00000000")+".lock", true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if lock != nil {
			if err := unlock(lock); err != nil {
				t.Error(err)
			}
		}
	}()
	// Проверяющий уже завершённой записи тоже временно занимает её блокировку.
	during, err := s.Resolve(ctx)
	if err != nil || during.Joined || !during.Sealed || !during.Acknowledged || during.Unknown || during.PendingOperations != 1 {
		t.Fatal("занятая блокировка не отделена от долговечного подтверждения", during, err)
	}
	if err := unlock(lock); err != nil {
		t.Fatal(err)
	}
	lock = nil
	after, err := s.Resolve(ctx)
	if err != nil || !after.Joined || after.PendingOperations != 0 || after != before {
		t.Fatal("освобождение блокировки не восстановило то же доказательство", before, after, err)
	}
}
