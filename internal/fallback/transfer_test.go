package fallback

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

type transferReader struct {
	read         func([]byte) (int, error)
	close        func() error
	reads        atomic.Int32
	closes       atomic.Int32
	closed       chan struct{}
	closeStarted chan struct{}
}

func (r *transferReader) Read(p []byte) (int, error) {
	r.reads.Add(1)
	return r.read(p)
}
func (r *transferReader) Close() error {
	r.closes.Add(1)
	if r.closeStarted != nil {
		close(r.closeStarted)
	}
	var err error
	if r.close != nil {
		err = r.close()
	}
	close(r.closed)
	return err
}

type transferInspection struct {
	acceptanceInspection
	source func(context.Context, *os.File, core.AssetKind) (Source, error)
}

func (i transferInspection) Source(ctx context.Context, f *os.File, kind core.AssetKind) (Source, error) {
	return i.source(ctx, f, kind)
}

func transferManager(t *testing.T, svc *core.Service, files *FileStore, c Config, inspect Inspection) *Manager {
	t.Helper()
	m, err := NewManagerWithTools(svc, files, c, Tools{
		Builder:   func(GenerateRequest) (Command, error) { return Command{Binary: "controlled"}, nil },
		Inspector: inspect,
		Runner: acceptanceRunFunc(func(_ context.Context, _ Command, _ *os.File, out io.Writer) (RunResult, error) {
			_, err := out.Write([]byte("controlled-verified-variant"))
			return RunResult{}, err
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		if err := m.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return m
}

func transferAwait(t *testing.T, ctx context.Context, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

func TestWaitUploadClosesReaderBeforeInspectionAndPreservesBackground(t *testing.T) {
	svc, _, asset, _ := acceptanceManagerService(t)
	files, directory := acceptanceFileStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	data := acceptancePNG(t)
	closeRelease, inspectEntered, inspectRelease := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var releaseClose, releaseInspect sync.Once
	defer releaseClose.Do(func() { close(closeRelease) })
	defer releaseInspect.Do(func() { close(inspectRelease) })
	r := &transferReader{read: bytes.NewReader(data).Read, closed: make(chan struct{}), closeStarted: make(chan struct{}), close: func() error { <-closeRelease; return nil }}
	inspect := transferInspection{source: func(work context.Context, f *os.File, kind core.AssetKind) (Source, error) {
		select {
		case <-r.closed:
		default:
			return Source{}, errors.New("проверка началась до закрытия тела загрузки")
		}
		close(inspectEntered)
		select {
		case <-inspectRelease:
			return (&acceptanceInspection{}).Source(work, f, kind)
		case <-work.Done():
			return Source{}, work.Err()
		}
	}}
	m := transferManager(t, svc, files, acceptanceManagerConfig(), &inspect)
	j, err := m.Upload(ctx, core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage}, r)
	if err != nil {
		t.Fatal(err)
	}
	request, cancelRequest := context.WithCancel(ctx)
	defer cancelRequest()
	waited := make(chan error, 1)
	go func() { waited <- j.WaitUpload(request) }()
	transferAwait(t, ctx, r.closeStarted)
	select {
	case err := <-waited:
		t.Fatalf("передача подтверждена до завершения Close: %v", err)
	case <-inspectEntered:
		t.Fatal("проверка началась во время Close")
	default:
	}
	final := j.Generation().OriginalPath
	got, err := os.ReadFile(filepath.Join(directory, final))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("исходный файл не сохранён до закрытия reader: %x, %v", got, err)
	}
	if _, err = os.Stat(filepath.Join(directory, j.Generation().StagingPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("исходный staging не переименован: %v", err)
	}
	releaseClose.Do(func() { close(closeRelease) })
	select {
	case err := <-waited:
		if err != nil {
			t.Fatal(err)
		}
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	transferAwait(t, ctx, inspectEntered)
	select {
	case <-j.Done():
		t.Fatal("WaitUpload дождался полной подготовки")
	default:
	}
	cancelRequest()
	if err = j.WaitUpload(request); err != nil {
		t.Fatalf("подтверждённая передача отменена запросом: %v", err)
	}
	releaseInspect.Do(func() { close(inspectRelease) })
	result, err := j.Wait(ctx)
	if err != nil || !result.Clean || result.Generation.State != core.PreparationPublished || r.closes.Load() != 1 {
		t.Fatalf("фон после отмены запроса: %+v, закрытий=%d, ошибка=%v", result, r.closes.Load(), err)
	}
}

func TestWaitUploadCancellationBeforeClaimClosesWithoutReading(t *testing.T) {
	for _, mode := range []string{"request-cancel", "request-deadline", "job-cancel", "job-deadline", "manager-close"} {
		t.Run(mode, func(t *testing.T) {
			svc, _, blockerAsset, _ := acceptanceManagerService(t)
			files, _ := acceptanceFileStore(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			blocker, err := svc.ReservePreparation(ctx, core.PreparationRequest{AssetID: blockerAsset.ID, ExpectedRevision: blockerAsset.Revision, Kind: core.AssetImage, GeneratorVersion: "controlled"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = svc.ClaimPreparation(ctx, blocker.ID); err != nil {
				t.Fatal(err)
			}
			a, err := svc.CreateAsset(ctx, "Ожидающий файл")
			if err != nil {
				t.Fatal(err)
			}
			c := acceptanceManagerConfig()
			c.QueueInterval = time.Hour
			if mode == "job-deadline" {
				c.PrepareTimeout = 50 * time.Millisecond
			}
			m := transferManager(t, svc, files, c, &acceptanceInspection{})
			r := &transferReader{read: bytes.NewReader(acceptancePNG(t)).Read, closed: make(chan struct{})}
			j, err := m.Upload(ctx, core.PreparationRequest{AssetID: a.ID, ExpectedRevision: a.Revision, Kind: core.AssetImage}, r)
			if err != nil {
				t.Fatal(err)
			}
			waitCtx, stop := context.WithCancel(ctx)
			defer stop()
			want := context.Canceled
			switch mode {
			case "request-cancel":
				stop()
			case "request-deadline":
				var deadlineCancel context.CancelFunc
				waitCtx, deadlineCancel = context.WithTimeout(ctx, 50*time.Millisecond)
				defer deadlineCancel()
				want = context.DeadlineExceeded
			case "job-cancel":
				j.Cancel()
			case "job-deadline":
				want = context.DeadlineExceeded
			case "manager-close":
				if err = m.Close(ctx); err != nil && !errors.Is(err, context.Canceled) {
					t.Fatal(err)
				}
			}
			if err = j.WaitUpload(waitCtx); !errors.Is(err, want) {
				t.Fatalf("причина отмены передачи: %v", err)
			}
			transferAwait(t, ctx, r.closed)
			result, err := j.Wait(ctx)
			wantJob := want
			if mode == "request-deadline" {
				// Срок запроса завершает передачу с DeadlineExceeded, а задание получает Cancel.
				wantJob = context.Canceled
			}
			if !errors.Is(err, wantJob) || !result.Clean || r.reads.Load() != 0 || r.closes.Load() != 1 {
				t.Fatalf("отмена до claim: %+v, чтений=%d, закрытий=%d, ошибка=%v", result, r.reads.Load(), r.closes.Load(), err)
			}
			stored, err := svc.GetGeneration(ctx, j.Generation().ID)
			if err != nil || stored.State != core.PreparationFailed || stored.Issued || stored.CleanupPending {
				t.Fatalf("очередь после отмены: %+v, %v", stored, err)
			}
		})
	}
}

func TestWaitUploadCancellationDuringCopyUnblocksReader(t *testing.T) {
	svc, _, asset, _ := acceptanceManagerService(t)
	files, _ := acceptanceFileStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	entered, closed := make(chan struct{}), make(chan struct{})
	r := &transferReader{closed: closed, read: func([]byte) (int, error) {
		close(entered)
		<-closed
		return 0, io.ErrClosedPipe
	}}
	m := transferManager(t, svc, files, acceptanceManagerConfig(), &acceptanceInspection{})
	j, err := m.Upload(ctx, core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage}, r)
	if err != nil {
		t.Fatal(err)
	}
	transferAwait(t, ctx, entered)
	request, stop := context.WithCancel(ctx)
	stop()
	if err = j.WaitUpload(request); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	transferAwait(t, ctx, closed)
	result, err := j.Wait(ctx)
	if err == nil || !result.Clean || r.closes.Load() != 1 {
		t.Fatalf("незавершённое копирование: %+v, закрытий=%d, ошибка=%v", result, r.closes.Load(), err)
	}
}

func TestWaitUploadCancellationWinsBeforeReaderCloseCompletes(t *testing.T) {
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	work, cancel := context.WithCancel(context.Background())
	defer cancel()
	release := make(chan struct{})
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() { close(release) })
	r := &transferReader{closed: make(chan struct{}), closeStarted: make(chan struct{}), close: func() error { <-release; return nil }}
	u := &uploadTransfer{reader: &ownedReader{ReadCloser: r}, done: make(chan struct{})}
	j := &Job{cancel: cancel, upload: u}
	completed := make(chan error, 1)
	go func() { completed <- u.complete(work, nil) }()
	transferAwait(t, ctx, r.closeStarted)
	request, cancelRequest := context.WithCancel(ctx)
	cancelRequest()
	waited := make(chan error, 1)
	go func() { waited <- j.WaitUpload(request) }()
	transferAwait(t, ctx, work.Done())
	select {
	case err := <-waited:
		t.Fatalf("отмена вернулась до закрытия reader: %v", err)
	default:
	}
	releaseOnce.Do(func() { close(release) })
	for _, result := range []<-chan error{completed, waited} {
		select {
		case err := <-result:
			if !errors.Is(err, context.Canceled) {
				t.Fatalf("успешное завершение обогнало принятую отмену: %v", err)
			}
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		}
	}
	if r.closes.Load() != 1 {
		t.Fatalf("reader закрыт повторно: %d", r.closes.Load())
	}
}

func TestWaitUploadRejectsEarlyFailuresAndKeepsItsResult(t *testing.T) {
	for _, mode := range []string{"empty", "overflow", "read", "close", "directory"} {
		t.Run(mode, func(t *testing.T) {
			svc, _, asset, _ := acceptanceManagerService(t)
			files, _ := acceptanceFileStore(t)
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			c := acceptanceManagerConfig()
			data := acceptancePNG(t)
			injected := errors.New("контролируемая ошибка передачи")
			want := injected
			r := &transferReader{closed: make(chan struct{}), read: bytes.NewReader(data).Read}
			switch mode {
			case "empty":
				r.read = bytes.NewReader(nil).Read
				want = core.ErrCorrupt
			case "overflow":
				c.MaxUploadBytes = uint64(len(data) - 1)
				want = ErrLimit
			case "read":
				r.read = func([]byte) (int, error) { return 0, injected }
			case "close":
				r.close = func() error { return injected }
			case "directory":
				if err := files.root.Remove("fallback"); err != nil {
					t.Fatal(err)
				}
				want = os.ErrNotExist
			}
			var inspected atomic.Int32
			inspect := transferInspection{source: func(context.Context, *os.File, core.AssetKind) (Source, error) {
				inspected.Add(1)
				return Source{}, errors.New("проверка после ошибки передачи")
			}}
			m := transferManager(t, svc, files, c, &inspect)
			j, err := m.Upload(ctx, core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage}, r)
			if err != nil {
				t.Fatal(err)
			}
			first := j.WaitUpload(ctx)
			if !errors.Is(first, want) {
				t.Fatalf("причина ошибки передачи потеряна: %v", first)
			}
			result, err := j.Wait(ctx)
			if err == nil || !result.Clean || inspected.Load() != 0 || r.closes.Load() != 1 {
				t.Fatalf("ошибка передачи не завершила работу: %+v, проверок=%d, закрытий=%d, ошибка=%v", result, inspected.Load(), r.closes.Load(), err)
			}
			if repeated := j.WaitUpload(ctx); repeated != first {
				t.Fatalf("завершённая передача изменила результат: %v -> %v", first, repeated)
			}
		})
	}
}

func TestWaitUploadSuccessSurvivesPreparationFailure(t *testing.T) {
	svc, _, asset, _ := acceptanceManagerService(t)
	files, _ := acceptanceFileStore(t)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	m := transferManager(t, svc, files, acceptanceManagerConfig(), &acceptanceInspection{})
	old := acceptanceUpload(t, m, svc, asset.ID, acceptancePNG(t))
	a, err := svc.GetAsset(ctx, asset.ID)
	if err != nil {
		t.Fatal(err)
	}
	j, err := m.Upload(ctx, core.PreparationRequest{AssetID: a.ID, ExpectedRevision: a.Revision, Kind: core.AssetImage}, io.NopCloser(bytes.NewReader([]byte("invalid-image"))))
	if err != nil {
		t.Fatal(err)
	}
	if err = j.WaitUpload(ctx); err != nil {
		t.Fatal(err)
	}
	result, err := j.Wait(ctx)
	if err == nil || !result.Clean {
		t.Fatalf("неверное содержимое принято подготовкой: %+v, %v", result, err)
	}
	if err = j.WaitUpload(ctx); err != nil {
		t.Fatalf("ошибка подготовки переписала успешную передачу: %v", err)
	}
	a, err = svc.GetAsset(ctx, a.ID)
	if err != nil || a.CurrentGenerationID != old.Generation.ID || a.PendingGenerationID != "" || a.State != core.AssetReady {
		t.Fatalf("отказ подготовки заменил current: %+v, %v", a, err)
	}
	if err = (&Job{}).WaitUpload(ctx); !errors.Is(err, ErrInvalid) {
		t.Fatalf("задание без загрузки приняло ожидание передачи: %v", err)
	}
}
