package fallback

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
)

func acceptanceDigest(data []byte) string { h := sha256.Sum256(data); return hex.EncodeToString(h[:]) }

func acceptanceFileStore(t *testing.T) (*FileStore, string) {
	t.Helper()
	dir := t.TempDir()
	s, err := OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s, dir
}

type acceptanceInspection struct{ verifyError error }

func (i *acceptanceInspection) Source(ctx context.Context, f *os.File, kind core.AssetKind) (Source, error) {
	return (Prober{Limits: acceptanceManagerConfig().Probe}).Source(ctx, f, kind)
}
func (i *acceptanceInspection) Verify(_ context.Context, _ *os.File, p core.GroupProfile) (core.MediaFormat, error) {
	return p.Format, i.verifyError
}

type acceptanceRunFunc func(context.Context, Command, *os.File, io.Writer) (RunResult, error)

func (f acceptanceRunFunc) Run(ctx context.Context, c Command, input *os.File, out io.Writer) (RunResult, error) {
	return f(ctx, c, input, out)
}

func acceptanceManagerConfig() Config {
	return Config{GeneratorVersion: "acceptance-generator", FFmpeg: "controlled-ffmpeg", FFprobe: "controlled-ffprobe", MaxUploadBytes: 1 << 16, MaxOutputBytes: 1 << 16, PrepareTimeout: 5 * time.Second, ShutdownTimeout: time.Second, QueueInterval: time.Millisecond, Runner: RunnerLimits{Timeout: time.Second, MaxArgs: 256, MaxCommandBytes: 1 << 16, MaxDiagnosticBytes: 1 << 16}, Probe: ProbeLimits{MaxJSONBytes: 1 << 16, MaxWidth: 4096, MaxHeight: 4096, MaxPixels: 4096 * 4096, MaxDuration: time.Minute, ImageDuration: time.Second, MaxPackets: 1000, Container: fmp4.Limits{MaxBoxBytes: 1 << 20, MaxInitBytes: 1 << 20, MaxMoofBytes: 1 << 16, MaxMdatBytes: 1 << 20, MaxSampleBytes: 1 << 18, MaxExtradataBytes: 1 << 12, MaxSamples: 100}}}
}

func acceptanceManagerService(t *testing.T) (*core.Service, *storage.Store, core.FallbackAsset, core.Stream) {
	t.Helper()
	ctx := context.Background()
	options := storage.Options{PreparationLimits: core.PreparationLimits{MaxQueued: 3, MaxProfiles: 8}, ObservationLimits: core.ObservationLimits{MaxSessions: 8, MaxGroups: 32, MaxOutputs: 64, MaxFFmpegWorkers: 32, MaxIdentifierBytes: 1024, MaxErrorBytes: 65536, MaxBytes: 4 << 20}, MaxOpenConns: 4, MaxIdleConns: 4, ConnMaxLifetime: time.Minute, ConnMaxIdleTime: time.Minute, BusyTimeout: 10 * time.Millisecond, RetryAttempts: 3, RetryTimeout: time.Second, RetryDelay: time.Millisecond}
	s, err := storage.Open(ctx, filepath.Join(t.TempDir(), "manager.sqlite"), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	svc, err := core.NewService(s, s.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	a, err := svc.CreateAsset(ctx, "Выбранная заглушка")
	if err != nil {
		t.Fatal(err)
	}
	stream, err := svc.CreateStream(ctx, core.Stream{Name: "Поток", PublicationKey: "manager-publication", Fallback: core.FallbackPolicy{AssetID: a.ID, Watchdog: core.WatchdogPolicy{VideoTimeout: time.Second, AudioTimeout: time.Second, RecoveryWindow: time.Second, KeyframeTimeout: time.Second}}})
	if err != nil {
		t.Fatal(err)
	}
	profile := core.GroupProfile{Mode: core.ModeCopy, Format: core.MediaFormat{Video: core.VideoFormat{Codec: core.CodecAV1, Width: 640, Height: 360, FrameRate: core.FrameRate{Numerator: 30, Denominator: 1}, CodecProfile: "main", PixelFormat: "yuv420p", BitDepth: 8, Color: core.ColorInfo{Range: "limited", Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"}}, Audio: core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", SampleRateHz: 48000, Channels: 2, ChannelLayout: "stereo"}}, VideoEncoding: core.VideoEncoding{TargetBitrateBPS: 300000, MaxBitrateBPS: 400000, BufferBits: 600000, GOPFrames: 30, ClosedGOP: true}, AudioEncoding: core.AudioEncoding{BitrateBPS: 64000}}
	for i := 0; i < 3; i++ {
		p := profile
		if i == 2 {
			p.Mode = core.ModeTranscode
			p.Format.Video.Codec = core.CodecH264
			p.Format.Video.CodecProfile = "high"
		}
		saved, err := svc.CreateProfile(ctx, core.TranscodeProfile{Name: "Профиль", Profile: p})
		if err != nil {
			t.Fatal(err)
		}
		if _, err = svc.CreateGroup(ctx, core.TranscodeGroup{Name: "Группа", StreamID: stream.ID, ProfileID: saved.ID}); err != nil {
			t.Fatal(err)
		}
	}
	return svc, s, a, stream
}

func acceptancePNG(t *testing.T) []byte {
	t.Helper()
	var data bytes.Buffer
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 16, 16))); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

func acceptanceNewManager(t *testing.T, svc *core.Service, files *FileStore, c Config, inspect *acceptanceInspection, run ProcessRunner) *Manager {
	t.Helper()
	m, err := NewManagerWithTools(svc, files, c, Tools{Builder: func(GenerateRequest) (Command, error) { return Command{Binary: "controlled"}, nil }, Inspector: inspect, Runner: run})
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

func acceptanceUpload(t *testing.T, m *Manager, svc *core.Service, id core.ID, data []byte) Result {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	a, err := svc.GetAsset(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	j, err := m.Upload(ctx, core.PreparationRequest{AssetID: id, ExpectedRevision: a.Revision, Kind: core.AssetImage}, io.NopCloser(bytes.NewReader(data)))
	if err != nil {
		t.Fatal(err)
	}
	r, err := j.Wait(ctx)
	if err != nil || !r.Clean {
		t.Fatalf("подготовка: %+v, %v", r, err)
	}
	return r
}

func TestManagerRegeneratesEqualUploadsAndRebuildWithoutSharingOldFiles(t *testing.T) {
	svc, _, asset, _ := acceptanceManagerService(t)
	files, _ := acceptanceFileStore(t)
	inspect := &acceptanceInspection{}
	var calls atomic.Int32
	run := acceptanceRunFunc(func(_ context.Context, _ Command, input *os.File, out io.Writer) (RunResult, error) {
		position, err := input.Seek(0, io.SeekCurrent)
		if err != nil || position != 0 {
			return RunResult{}, errors.New("Дескриптор генератора не в начале")
		}
		calls.Add(1)
		_, err = out.Write([]byte("verified-controlled-variant"))
		return RunResult{}, err
	})
	m := acceptanceNewManager(t, svc, files, acceptanceManagerConfig(), inspect, run)
	data := acceptancePNG(t)
	first := acceptanceUpload(t, m, svc, asset.ID, data)
	second := acceptanceUpload(t, m, svc, asset.ID, data)
	if calls.Load() != 4 || len(first.Generation.RequiredProfiles) != 2 || len(second.Generation.RequiredProfiles) != 2 {
		t.Fatalf("одинаковые профили не разделяют один variant внутри поколения, либо equal upload не регенерирован: calls=%d", calls.Load())
	}
	if first.Generation.ID == second.Generation.ID || first.Generation.OriginalPath == second.Generation.OriginalPath {
		t.Fatal("повторная загрузка переиспользовала старый original")
	}
	for _, a := range first.Generation.RequiredProfiles {
		for _, b := range second.Generation.RequiredProfiles {
			if a.ManagedPath == b.ManagedPath || a.VariantID == b.VariantID {
				t.Fatal("новое поколение переиспользовало прежний variant")
			}
		}
	}
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	current, err := svc.GetAsset(ctx, asset.ID)
	if err != nil {
		t.Fatal(err)
	}
	j, err := m.Rebuild(ctx, core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: current.Revision})
	if err != nil {
		t.Fatal(err)
	}
	rebuilt, err := j.Wait(ctx)
	if err != nil || !rebuilt.Clean || calls.Load() != 6 {
		t.Fatalf("rebuild не выполнил новый комплект: %+v calls=%d error=%v", rebuilt, calls.Load(), err)
	}
	if rebuilt.Generation.RebuildFromGenerationID != second.Generation.ID || rebuilt.Generation.OriginalPath == second.Generation.OriginalPath || rebuilt.Generation.SourceHash != second.Generation.SourceHash {
		t.Fatalf("rebuild не скопировал exact original в собственный путь: %+v", rebuilt.Generation)
	}
	for _, g := range []core.FallbackGeneration{first.Generation, second.Generation, rebuilt.Generation} {
		f, err := files.OpenExact(ctx, g.OriginalPath, g.SizeBytes, g.SourceHash)
		if err != nil {
			t.Fatal(err)
		}
		got, err := io.ReadAll(f)
		acceptanceClose(t, f)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("original изменён: %x, %v", got, err)
		}
	}
}

func TestManagerFailedOutputAndVerificationPreserveCurrent(t *testing.T) {
	for _, failure := range []string{"output-limit", "verification", "runner", "invalid-source"} {
		t.Run(failure, func(t *testing.T) {
			svc, _, asset, _ := acceptanceManagerService(t)
			files, _ := acceptanceFileStore(t)
			inspect := &acceptanceInspection{}
			fail := false
			run := acceptanceRunFunc(func(_ context.Context, _ Command, _ *os.File, out io.Writer) (RunResult, error) {
				if fail && failure == "runner" {
					return RunResult{}, io.ErrClosedPipe
				}
				data := []byte("verified-variant")
				if fail && failure == "output-limit" {
					data = bytes.Repeat([]byte{1}, int(acceptanceManagerConfig().MaxOutputBytes)+1)
				}
				_, err := out.Write(data)
				return RunResult{}, err
			})
			m := acceptanceNewManager(t, svc, files, acceptanceManagerConfig(), inspect, run)
			data := acceptancePNG(t)
			old := acceptanceUpload(t, m, svc, asset.ID, data)
			fail = true
			if failure == "verification" {
				inspect.verifyError = core.ErrCorrupt
			}
			if failure == "invalid-source" {
				data = []byte("not a PNG")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			a, err := svc.GetAsset(ctx, asset.ID)
			if err != nil {
				t.Fatal(err)
			}
			j, err := m.Upload(ctx, core.PreparationRequest{AssetID: a.ID, ExpectedRevision: a.Revision, Kind: core.AssetImage}, io.NopCloser(bytes.NewReader(data)))
			if err != nil {
				t.Fatal(err)
			}
			result, err := j.Wait(ctx)
			if err == nil || !result.Clean {
				t.Fatalf("ошибка подготовки не завершена безопасно: %+v error=%v", result, err)
			}
			a, err = svc.GetAsset(ctx, asset.ID)
			if err != nil || a.CurrentGenerationID != old.Generation.ID || a.State != core.AssetReady || a.PendingGenerationID != "" {
				t.Fatalf("неудачная замена изменила current: %+v error=%v", a, err)
			}
			if failure == "output-limit" && !errors.Is(result.Err, ErrLimit) {
				t.Fatalf("превышение выходного бюджета не классифицировано: %v", result.Err)
			}
		})
	}
}

type acceptanceCountReader struct {
	io.ReadCloser
	reads atomic.Int32
}

func (r *acceptanceCountReader) Read(p []byte) (int, error) {
	r.reads.Add(1)
	return r.ReadCloser.Read(p)
}

func TestManagersShareExecutionSlotAndCancellationWaitsForPhysicalRunner(t *testing.T) {
	svc, store, asset, stream := acceptanceManagerService(t)
	files, _ := acceptanceFileStore(t)
	entered, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	var calls atomic.Int32
	run := acceptanceRunFunc(func(ctx context.Context, _ Command, _ *os.File, out io.Writer) (RunResult, error) {
		if calls.Add(1) == 1 {
			close(entered)
			<-ctx.Done()
			close(cancelled)
			<-release
			return RunResult{}, ctx.Err()
		}
		_, err := out.Write([]byte("controlled-result"))
		return RunResult{}, err
	})
	inspect := &acceptanceInspection{}
	m := acceptanceNewManager(t, svc, files, acceptanceManagerConfig(), inspect, run)
	otherSvc, err := core.NewService(store, store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	other := acceptanceNewManager(t, otherSvc, files, acceptanceManagerConfig(), inspect, run)
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	data := acceptancePNG(t)
	job, err := m.Upload(ctx, core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage}, io.NopCloser(bytes.NewReader(data)))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	released := false
	defer func() {
		if !released {
			close(release)
		}
	}()
	queuedAsset, err := svc.CreateAsset(ctx, "Ожидающая заглушка")
	if err != nil {
		t.Fatal(err)
	}
	reader := &acceptanceCountReader{ReadCloser: io.NopCloser(bytes.NewReader(data))}
	queued, err := other.Upload(ctx, core.PreparationRequest{AssetID: queuedAsset.ID, ExpectedRevision: queuedAsset.Revision, Kind: core.AssetImage}, reader)
	if err != nil {
		t.Fatal(err)
	}
	if reader.reads.Load() != 0 || calls.Load() != 1 {
		t.Fatalf("второй Manager начал IO до execution slot: reads=%d calls=%d", reader.reads.Load(), calls.Load())
	}
	job.Cancel()
	select {
	case <-cancelled:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	short, cancelShort := context.WithCancel(ctx)
	cancelShort()
	if _, err = job.Wait(short); !errors.Is(err, ErrCleanupPending) {
		t.Fatalf("Cancel выдал physical completion до runner join: %v", err)
	}
	if _, err = svc.StartSession(ctx, core.StartSessionRequest{RequestID: "while-cancelled-runner", StreamID: stream.ID}); !errors.Is(err, core.ErrNotReady) {
		t.Fatalf("отмена разрешила start до физического завершения: %v", err)
	}
	if reader.reads.Load() != 0 || calls.Load() != 1 {
		t.Fatal("queued работа началась до физического завершения отменённой")
	}
	close(release)
	released = true
	result, err := job.Wait(ctx)
	if !errors.Is(err, context.Canceled) || !result.Clean {
		t.Fatalf("runner join не завершил отмену: %+v, %v", result, err)
	}
	result, err = queued.Wait(ctx)
	if err != nil || !result.Clean || reader.reads.Load() == 0 {
		t.Fatalf("следующий Manager не получил освобождённый слот: %+v reads=%d error=%v", result, reader.reads.Load(), err)
	}
}

func TestVerifiedReadersHaveIndependentPositionsAndRejectCorruption(t *testing.T) {
	s, dir := acceptanceFileStore(t)
	ctx := context.Background()
	data := []byte("independent seekable media descriptors")
	if err := os.WriteFile(filepath.Join(dir, "media.mp4"), data, 0600); err != nil {
		t.Fatal(err)
	}
	m := core.ArtifactManifest{ManagedPath: "media.mp4", SizeBytes: uint64(len(data)), ContentHash: acceptanceDigest(data)}
	a, err := s.OpenVerified(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	defer acceptanceClose(t, a)
	b, err := s.OpenVerified(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	defer acceptanceClose(t, b)
	first := make([]byte, 7)
	if _, err = io.ReadFull(a, first); err != nil {
		t.Fatal(err)
	}
	got, err := io.ReadAll(b)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("второй читатель унаследовал позицию: %q, %v", got, err)
	}
	rest, err := io.ReadAll(a)
	if err != nil || !bytes.Equal(rest, data[7:]) {
		t.Fatalf("чтение второго дескриптора изменило первый: %q, %v", rest, err)
	}
	c, err := s.OpenVerified(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	defer acceptanceClose(t, c)
	got, err = io.ReadAll(c)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("повторная попытка не началась с нуля: %q, %v", got, err)
	}
	data[0] ^= 1
	if err = os.WriteFile(filepath.Join(dir, "media.mp4"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if f, err := s.OpenVerified(ctx, m); err == nil {
		acceptanceClose(t, f)
		t.Fatal("равный размер скрыл повреждение hash")
	}
	if err = s.RemoveExact(ctx, m.ManagedPath, m.SizeBytes, m.ContentHash); err == nil {
		t.Fatal("cleanup удалил изменённый файл")
	}
	if _, err = os.Stat(filepath.Join(dir, m.ManagedPath)); err != nil {
		t.Fatalf("ошибка cleanup уничтожила чужое содержимое: %v", err)
	}
}

func TestManagedFilesConfineLinksAndNeverOverwriteFinal(t *testing.T) {
	s, dir := acceptanceFileStore(t)
	outside := t.TempDir()
	data := []byte("outside-owned-data")
	if err := os.WriteFile(filepath.Join(outside, "secret"), data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(dir, "link.mp4")); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"../secret", "/secret", "escape/secret", "link.mp4"} {
		t.Run(name, func(t *testing.T) {
			if f, err := s.OpenExact(context.Background(), name, uint64(len(data)), acceptanceDigest(data)); err == nil {
				acceptanceClose(t, f)
				t.Fatal("managed path открыл файл вне владения")
			}
		})
	}
	for _, name := range []string{"first.stage", "final.mp4"} {
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	if err := s.PublishRename("first.stage", "final.mp4"); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("существующий final не защищён: %v", err)
	}
	got, err := os.ReadFile(filepath.Join(dir, "final.mp4"))
	if err != nil || string(got) != "final.mp4" {
		t.Fatalf("final перезаписан: %q, %v", got, err)
	}
	got, err = os.ReadFile(filepath.Join(dir, "first.stage"))
	if err != nil || string(got) != "first.stage" {
		t.Fatalf("неудачный rename потерял staging: %q, %v", got, err)
	}
	if err = s.PublishRename("first.stage", "new.mp4"); err != nil {
		t.Fatalf("rename и барьер каталога: %v", err)
	}
	if _, err = os.Stat(filepath.Join(dir, "first.stage")); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("успешный rename сохранил staging: %v", err)
	}
}

func TestBoundedCopyEnforcesBytesAndCancellationBeforeWrites(t *testing.T) {
	for _, size := range []int{7, 8, 9, 32768} {
		t.Run(string(rune('a'+size%26)), func(t *testing.T) {
			var dst bytes.Buffer
			src := bytes.Repeat([]byte{0x5a}, size)
			n, err := CopyBounded(context.Background(), &dst, bytes.NewReader(src), 8)
			if size <= 8 {
				if err != nil || n != uint64(size) || !bytes.Equal(dst.Bytes(), src) {
					t.Fatalf("допустимая граница %d: n=%d data=%x error=%v", size, n, dst.Bytes(), err)
				}
			} else if !errors.Is(err, ErrLimit) || dst.Len() > 8 || n > 8 {
				t.Fatalf("переполнение %d: n=%d bytes=%d error=%v", size, n, dst.Len(), err)
			}
		})
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	var dst bytes.Buffer
	n, err := CopyBounded(ctx, &dst, bytes.NewReader([]byte("payload")), 32)
	if !errors.Is(err, context.Canceled) || n != 0 || dst.Len() != 0 {
		t.Fatalf("отменённая запись изменила файл: n=%d bytes=%d error=%v", n, dst.Len(), err)
	}
}

func TestExactCleanupIsIdempotentAndDoesNotFollowReplacementLinks(t *testing.T) {
	s, dir := acceptanceFileStore(t)
	ctx := context.Background()
	data := []byte("owned-media")
	if err := os.WriteFile(filepath.Join(dir, "owned.mp4"), data, 0600); err != nil {
		t.Fatal(err)
	}
	for i := 0; i < 2; i++ {
		if err := s.RemoveExact(ctx, "owned.mp4", uint64(len(data)), acceptanceDigest(data)); err != nil {
			t.Fatalf("идемпотентная очистка %d: %v", i, err)
		}
	}
	outside := filepath.Join(t.TempDir(), "keep.mp4")
	if err := os.WriteFile(outside, data, 0600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(dir, "owned.mp4")); err != nil {
		t.Fatal(err)
	}
	if err := s.RemoveExact(ctx, "owned.mp4", uint64(len(data)), acceptanceDigest(data)); err == nil {
		t.Fatal("cleanup последовал замещающей ссылке")
	}
	got, err := os.ReadFile(outside)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("cleanup изменил внешний файл: %q, %v", got, err)
	}
}

func acceptanceClose(t *testing.T, c io.Closer) {
	t.Helper()
	if err := c.Close(); err != nil {
		t.Error(err)
	}
}

func TestCleanupUsesDurableIntentAndPreservesUnknownObjectsAndCurrent(t *testing.T) {
	svc, _, asset, _ := acceptanceManagerService(t)
	files, dir := acceptanceFileStore(t)
	ctx := context.Background()
	run := acceptanceRunFunc(func(_ context.Context, _ Command, _ *os.File, w io.Writer) (RunResult, error) {
		_, err := w.Write([]byte("controlled-verified-media"))
		return RunResult{}, err
	})
	m := acceptanceNewManager(t, svc, files, acceptanceManagerConfig(), &acceptanceInspection{}, run)
	old := acceptanceUpload(t, m, svc, asset.ID, acceptancePNG(t))
	current := acceptanceUpload(t, m, svc, asset.ID, acceptancePNG(t))
	g, err := svc.GetGeneration(ctx, old.Generation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.DeleteGeneration(ctx, g.ID, g.Revision); err != nil {
		t.Fatal(err)
	}
	page, err := svc.ListCleanups(ctx, core.PageRequest{Query: core.ListQuery{Limit: 100, PendingOnly: true}})
	if err != nil {
		t.Fatal(err)
	}
	var intents []core.CleanupIntent
	var directory core.CleanupIntent
	for _, intent := range page.Items {
		if intent.ArtifactGenerationID == g.ID {
			if intent.FileKind == core.OwnedDirectory {
				directory = intent
			} else {
				intents = append(intents, intent)
			}
		}
	}
	if directory.ID == "" || len(intents) == 0 {
		t.Fatal("retirement не сохранил полный ledger cleanup")
	}
	forged := intents[0]
	forged.ManagedPath = current.Generation.OriginalPath
	if err = m.CleanupFile(ctx, forged); !errors.Is(err, core.ErrStale) {
		t.Fatalf("подмена intent не отвергнута до IO: %v", err)
	}
	restarted, err := OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := restarted.Close(); err != nil {
			t.Error(err)
		}
	})
	cleanup := acceptanceNewManager(t, svc, restarted, acceptanceManagerConfig(), &acceptanceInspection{}, run)
	for _, intent := range intents {
		if err = cleanup.CleanupFile(ctx, intent); err != nil {
			t.Fatalf("durable file proof после нового FileStore: %s: %v", intent.ManagedPath, err)
		}
		if err = cleanup.CleanupFile(ctx, intent); err != nil {
			t.Fatalf("повтор exact intent: %v", err)
		}
	}
	unknown := filepath.Join(dir, core.GenerationDirectory(g.ID), "foreign.keep")
	if err = os.WriteFile(unknown, []byte("unknown"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = m.CleanupFile(ctx, directory); err == nil {
		t.Fatal("cleanup рекурсивно удалил неизвестный объект")
	}
	if got, err := os.ReadFile(unknown); err != nil || string(got) != "unknown" {
		t.Fatalf("неизвестный файл изменён: %q, %v", got, err)
	}
	stored, err := svc.GetCleanup(ctx, directory.ID)
	if err != nil || stored.Acknowledged {
		t.Fatalf("неуспешный directory cleanup подтверждён: %+v, %v", stored, err)
	}
	if err = os.Remove(unknown); err != nil {
		t.Fatal(err)
	}
	if err = m.CleanupFile(ctx, directory); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(dir, core.GenerationDirectory(g.ID))); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("пустой принадлежащий каталог не удалён: %v", err)
	}
	f, err := files.OpenExact(ctx, current.Generation.OriginalPath, current.Generation.SizeBytes, current.Generation.SourceHash)
	if err != nil {
		t.Fatalf("очистка прежнего поколения затронула current: %v", err)
	}
	acceptanceClose(t, f)
}
