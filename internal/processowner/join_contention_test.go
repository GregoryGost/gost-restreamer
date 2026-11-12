package processowner

import "testing"

// Проверяет, что занятая блокировка временно снимает Joined даже у подтверждённой области, сохраняя
// Acknowledged. После освобождения возвращается прежнее доказательство.
func TestAcknowledgedScopeProofWaitsForContendedOperation(t *testing.T) {
	// r содержит изолированный реестр проверки временно занятой операции подтверждённой области.
	r := testRegistry(t, boundaryHelperPath(t))
	// s содержит область, полностью закрываемую и подтверждаемую до проверки конкуренции.
	s := testScope(t, r)
	// ctx ограничивает операции проверки тестовым сроком.
	ctx := testContext(t)
	// err сообщает об отказе первоначального закрытия области.
	if err := s.CloseContext(ctx); err != nil {
		t.Fatal(err)
	}
	// err сообщает об отказе первоначального подтверждения очистки области.
	if err := r.AcknowledgeScope(ctx, s.ID()); err != nil {
		t.Fatal(err)
	}
	// before сохраняет исходное доказательство Joined и Acknowledged; err сохраняет результаты
	// последующей сверки.
	before, err := s.Resolve(ctx)
	if err != nil || !before.Joined || !before.Sealed || !before.Acknowledged || before.Unknown {
		t.Fatal("нет исходного доказательства завершения", before, err)
	}
	// lock намеренно удерживает блокировку уже завершённой операции для временного
	// PendingOperations.
	lock, err := lockExisting(ctx, r.root, operationPath(s.ID(), "00000000")+".lock", true)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// Освобождает удержанную проверкой блокировку операции, если ранний выход ещё не позволил сделать это
		// явно.

		if lock != nil {
			// err сообщает об отказе резервного освобождения удержанной блокировки.
			if err := unlock(lock); err != nil {
				t.Error(err)
			}
		}
	}()
	// Проверяющий уже завершённой записи тоже временно занимает её блокировку.
	// during содержит доказательство во время удержания блокировки для проверки сохранения
	// Acknowledged без Joined.
	during, err := s.Resolve(ctx)
	if err != nil || during.Joined || !during.Sealed || !during.Acknowledged || during.Unknown || during.PendingOperations != 1 {
		t.Fatal("занятая блокировка не отделена от долговечного подтверждения", during, err)
	}
	// err сообщает об отказе явного освобождения контрольной блокировки.
	if err := unlock(lock); err != nil {
		t.Fatal(err)
	}
	lock = nil
	// after содержит доказательство после освобождения для сравнения с исходным before.
	after, err := s.Resolve(ctx)
	if err != nil || !after.Joined || after.PendingOperations != 0 || after != before {
		t.Fatal("освобождение блокировки не восстановило то же доказательство", before, after, err)
	}
}
