package fallback

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

type transferAcceptanceReader struct {
	io.Reader
	closed    chan struct{}
	closeGate <-chan struct{}
	closeErr  error
	reads     atomic.Int32
	once      sync.Once
}

func (r *transferAcceptanceReader) Read(p []byte) (int, error) {
	r.reads.Add(1)
	return r.Reader.Read(p)
}

func (r *transferAcceptanceReader) Close() error {
	r.once.Do(func() { close(r.closed) })
	if r.closeGate != nil {
		<-r.closeGate
	}
	return r.closeErr
}

type transferAcceptanceInspection struct {
	entered chan struct{}
	release <-chan struct{}
	err     error
	once    sync.Once
}

func (i *transferAcceptanceInspection) Source(ctx context.Context, f *os.File, kind core.AssetKind) (Source, error) {
	i.once.Do(func() { close(i.entered) })
	select {
	case <-i.release:
	case <-ctx.Done():
		return Source{}, ctx.Err()
	}
	if i.err != nil {
		return Source{}, i.err
	}
	return (&acceptanceInspection{}).Source(ctx, f, kind)
}

func (*transferAcceptanceInspection) Verify(_ context.Context, _ *os.File, p core.GroupProfile) (core.MediaFormat, error) {
	return p.Format, nil
}

func transferAcceptanceManager(t *testing.T, svc *core.Service, files *FileStore, inspect Inspection, config Config) *Manager {
	t.Helper()
	m, err := NewManagerWithTools(svc, files, config, Tools{Builder: func(GenerateRequest) (Command, error) { return Command{Binary: "controlled"}, nil }, Inspector: inspect, Runner: acceptanceRunFunc(func(_ context.Context, _ Command, _ *os.File, out io.Writer) (RunResult, error) {
		_, err := out.Write([]byte("verified-variant"))
		return RunResult{}, err
	})})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = m.Close(ctx)
	})
	return m
}

func transferAcceptanceWait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("не получено подтверждение этапа передачи")
	}
}

func transferAcceptanceGate(t *testing.T) (<-chan struct{}, func()) {
	t.Helper()
	gate := make(chan struct{})
	var once sync.Once
	release := func() { once.Do(func() { close(gate) }) }
	t.Cleanup(release)
	return gate, release
}

func TestAcceptanceUploadCompletesBeforeInspectionAndSurvivesRequestCancellation(t *testing.T) {
	svc, _, asset, _ := acceptanceManagerService(t)
	files, _ := acceptanceFileStore(t)
	release, finish := transferAcceptanceGate(t)
	inspect := &transferAcceptanceInspection{entered: make(chan struct{}), release: release}
	m := transferAcceptanceManager(t, svc, files, inspect, acceptanceManagerConfig())
	reader := &transferAcceptanceReader{Reader: bytes.NewReader(acceptancePNG(t)), closed: make(chan struct{})}
	job, err := m.Upload(context.Background(), core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage}, reader)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	if err = job.WaitUpload(ctx); err != nil {
		t.Fatal(err)
	}
	transferAcceptanceWait(t, reader.closed)
	transferAcceptanceWait(t, inspect.entered)
	select {
	case <-job.Done():
		t.Fatal("передача ошибочно означает завершение подготовки")
	default:
	}
	cancel()
	if err = job.WaitUpload(ctx); err != nil {
		t.Fatalf("поздняя отмена изменила подтверждённую передачу: %v", err)
	}
	finish()
	wait, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	result, err := job.Wait(wait)
	if err != nil || !result.Clean || result.Generation.State != core.PreparationPublished {
		t.Fatalf("фоновая подготовка не опубликована: %+v %v", result, err)
	}
	current, err := svc.GetAsset(wait, asset.ID)
	if err != nil || current.CurrentGenerationID != result.Generation.ID {
		t.Fatalf("публикация не стала current: %+v %v", current, err)
	}
}

func TestAcceptanceUploadWaitsForReaderCloseAndPreservesSuccessfulTransferOnPreparationFailure(t *testing.T) {
	svc, _, asset, _ := acceptanceManagerService(t)
	files, _ := acceptanceFileStore(t)
	closeGate, finishClose := transferAcceptanceGate(t)
	release, finish := transferAcceptanceGate(t)
	inspect := &transferAcceptanceInspection{entered: make(chan struct{}), release: release, err: core.ErrCorrupt}
	m := transferAcceptanceManager(t, svc, files, inspect, acceptanceManagerConfig())
	reader := &transferAcceptanceReader{Reader: bytes.NewReader(acceptancePNG(t)), closed: make(chan struct{}), closeGate: closeGate}
	job, err := m.Upload(context.Background(), core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage}, reader)
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	transferAcceptanceWait(t, reader.closed)
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	completed := make(chan error, 1)
	go func() { completed <- job.WaitUpload(ctx) }()
	select {
	case err := <-completed:
		t.Fatalf("успех до завершения Close: %v", err)
	default:
	}
	finishClose()
	if err = <-completed; err != nil {
		t.Fatal(err)
	}
	transferAcceptanceWait(t, inspect.entered)
	// Ошибка инспекции относится к подготовке, уже сохранённый файл передан успешно.
	job.Cancel()
	if _, err = job.Wait(ctx); err == nil {
		t.Fatal("ошибка подготовки потеряна")
	}
	if err = job.WaitUpload(ctx); err != nil {
		t.Fatalf("ошибка подготовки переписала результат передачи: %v", err)
	}
}

func TestAcceptanceQueuedUploadCancellationClosesUnreadReaderAndLeavesNoPending(t *testing.T) {
	svc, _, asset, _ := acceptanceManagerService(t)
	files, _ := acceptanceFileStore(t)
	release, finish := transferAcceptanceGate(t)
	defer finish()
	inspect := &transferAcceptanceInspection{entered: make(chan struct{}), release: release}
	m := transferAcceptanceManager(t, svc, files, inspect, acceptanceManagerConfig())
	first, err := m.Upload(context.Background(), core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage}, io.NopCloser(bytes.NewReader(acceptancePNG(t))))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Cancel()
	transferAcceptanceWait(t, inspect.entered)
	queuedAsset, err := svc.CreateAsset(context.Background(), "Ожидающий файл")
	if err != nil {
		t.Fatal(err)
	}
	reader := &transferAcceptanceReader{Reader: bytes.NewReader(acceptancePNG(t)), closed: make(chan struct{})}
	queued, err := m.Upload(context.Background(), core.PreparationRequest{AssetID: queuedAsset.ID, ExpectedRevision: queuedAsset.Revision, Kind: core.AssetImage}, reader)
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = queued.WaitUpload(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("отмена очереди не сохранена: %v", err)
	}
	transferAcceptanceWait(t, reader.closed)
	if reader.reads.Load() != 0 {
		t.Fatal("ожидающий reader прочитан до claim")
	}
	wait, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	result, err := queued.Wait(wait)
	if !errors.Is(err, context.Canceled) || !result.Clean {
		t.Fatalf("очередь не завершила отмену: %+v %v", result, err)
	}
	stored, err := svc.GetAsset(wait, queuedAsset.ID)
	if err != nil || stored.PendingGenerationID != "" || stored.CurrentGenerationID != "" {
		t.Fatalf("отмена оставила pending/current: %+v %v", stored, err)
	}
}

func TestAcceptanceUploadTransferRejectsEmptyOversizedAndCloseFailure(t *testing.T) {
	for _, tc := range []struct {
		name     string
		data     []byte
		closeErr error
	}{{"empty", nil, nil}, {"oversized", bytes.Repeat([]byte("x"), 17), nil}, {"close-error", []byte("payload"), io.ErrClosedPipe}} {
		t.Run(tc.name, func(t *testing.T) {
			svc, _, asset, _ := acceptanceManagerService(t)
			files, _ := acceptanceFileStore(t)
			config := acceptanceManagerConfig()
			config.MaxUploadBytes = 16
			m := transferAcceptanceManager(t, svc, files, &acceptanceInspection{}, config)
			reader := &transferAcceptanceReader{Reader: bytes.NewReader(tc.data), closed: make(chan struct{}), closeErr: tc.closeErr}
			job, err := m.Upload(context.Background(), core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage}, reader)
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err = job.WaitUpload(ctx); err == nil {
				t.Fatal("некорректная передача подтверждена")
			}
			transferAcceptanceWait(t, reader.closed)
			if _, err = job.Wait(ctx); err == nil {
				t.Fatal("некорректный файл опубликован")
			}
			stored, err := svc.GetAsset(ctx, asset.ID)
			if err != nil || stored.CurrentGenerationID != "" || stored.PendingGenerationID != "" {
				t.Fatalf("неудача изменила current или оставила pending: %+v %v", stored, err)
			}
		})
	}
}
