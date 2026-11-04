package storage

import (
	"context"
	"reflect"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

func TestResumePreservesSessionSnapshotAndHealthyOutputAttempt(t *testing.T) {
	f := newDomainFixture(t, 2)
	ctx := context.Background()
	start := startDomainSession(t, f)
	executeCommand(t, f.service, start.RequestID)
	before := readSession(t, f.store, start.SessionID)
	outputs := readOutputs(t, f.store, start.SessionID)
	fences := make([]core.ExecutionFence, len(outputs))
	for i, output := range outputs {
		command := outputStartReceipt(t, f, output.ID)
		var err error
		fences[i], err = f.service.Claim(ctx, command.RequestID)
		if err != nil {
			t.Fatal(err)
		}
		if err := f.service.MarkIssued(ctx, fences[i]); err != nil {
			t.Fatal(err)
		}
	}
	issuedOutputs := readOutputs(t, f.store, start.SessionID)
	pause, err := f.service.PauseSession(ctx, core.SessionCommand{RequestID: "pause-fallback", SessionID: start.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	pauseFence, err := f.service.Claim(ctx, pause.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if err := f.service.MarkIssued(ctx, pauseFence); err != nil {
		t.Fatal(err)
	}
	if session := readSession(t, f.store, start.SessionID); !session.DesiredPaused {
		t.Fatal("принятая пауза не включила желаемый fallback")
	}
	request := core.SessionCommand{RequestID: "resume-live", SessionID: start.SessionID}
	resume, err := f.service.ResumeSession(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	after := readSession(t, f.store, start.SessionID)
	if after.DesiredPaused || after.State != core.SessionRunning || after.Lifecycle != before.Lifecycle || !reflect.DeepEqual(after.Snapshot, before.Snapshot) {
		t.Fatalf("возобновление изменило жизненный цикл или снимок: %+v", after)
	}
	if !reflect.DeepEqual(issuedOutputs, readOutputs(t, f.store, start.SessionID)) {
		t.Fatal("пауза или возобновление изменили выходы")
	}
	if _, err := f.service.AuthorizeOBS(ctx, f.stream.ID, f.stream.PublicationKey); err != nil {
		t.Fatalf("возобновление отозвало OBS: %v", err)
	}
	requireDomainError(t, f.service.Complete(ctx, pauseFence, core.Completion{Success: true}), core.ErrStale)
	if receipt := readReceipt(t, f.store, pause.RequestID); receipt.Status != core.CommandInterrupted {
		t.Fatalf("отменённая пауза осталась исполнимой: %+v", receipt)
	}
	for _, fence := range fences {
		if err := f.service.Complete(ctx, fence, core.Completion{Success: true}); err != nil {
			t.Fatalf("возобновление аннулировало здоровый callback: %v", err)
		}
	}
	executeCommand(t, f.service, resume.RequestID)
	completed := readReceipt(t, f.store, resume.RequestID)
	stable := readSession(t, f.store, start.SessionID)
	replayed, err := f.service.ResumeSession(ctx, request)
	if err != nil || !reflect.DeepEqual(completed, replayed) || !reflect.DeepEqual(stable, readSession(t, f.store, start.SessionID)) {
		t.Fatalf("повтор возобновления изменил результат или сессию: %+v, %v", replayed, err)
	}
	_, err = f.service.PauseSession(ctx, request)
	requireDomainError(t, err, core.ErrConflict)
}

func TestRenameAssetChangesOnlyNameAndRevisionWithoutReplacingLiveGeneration(t *testing.T) {
	f := newDomainFixture(t, 1)
	ctx := context.Background()
	start := startDomainSession(t, f)
	output := readOutputs(t, f.store, start.SessionID)[0]
	for _, name := range []string{"", " \t", "имя\x00", string([]byte{0xff})} {
		_, err := f.service.RenameAsset(ctx, f.asset.ID, f.asset.Revision, name)
		requireDomainError(t, err, core.ErrInvalid)
		got, err := f.service.GetAsset(ctx, f.asset.ID)
		if err != nil || !reflect.DeepEqual(got, f.asset) {
			t.Fatalf("ошибка имени изменила оригинал: %+v, %v", got, err)
		}
	}
	renamed, err := f.service.RenameAsset(ctx, f.asset.ID, f.asset.Revision, "Новое имя оригинала")
	if err != nil {
		t.Fatal(err)
	}
	expected := f.asset
	expected.Name, expected.Revision, expected.UpdatedAt = "Новое имя оригинала", f.asset.Revision+1, renamed.UpdatedAt
	if renamed.UpdatedAt.Before(f.asset.UpdatedAt) || !reflect.DeepEqual(renamed, expected) {
		t.Fatalf("переименование изменило содержимое или владение: %+v", renamed)
	}
	_, err = f.service.RenameAsset(ctx, f.asset.ID, f.asset.Revision, "Устаревшая правка")
	requireDomainError(t, err, core.ErrConflict)
	_, err = f.service.RenameAsset(ctx, "missing-asset", 1, "Отсутствующий оригинал")
	requireDomainError(t, err, core.ErrNotFound)
	stored, err := f.service.GetAsset(ctx, f.asset.ID)
	if err != nil || !reflect.DeepEqual(stored, renamed) {
		t.Fatalf("ошибка CAS изменила оригинал: %+v, %v", stored, err)
	}
	variant, err := f.service.GetVariant(ctx, f.variant.ID)
	if err != nil || !reflect.DeepEqual(variant, f.variant) || !reflect.DeepEqual(output, readOutputs(t, f.store, start.SessionID)[0]) {
		t.Fatalf("переименование заменило поколение или снимок: %+v, %v", variant, err)
	}
	requireDomainError(t, f.service.DeleteVariant(ctx, f.variant.ID, f.variant.Revision), core.ErrInUse)
}
