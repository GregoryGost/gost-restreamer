package fallback

import (
	"bytes"
	"context"
	"errors"
	"io"
	"math"
	"os"
	"sync"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// Config задаёт конечные бюджеты операций; очередь ограничивает общий Store.
type Config struct {
	GeneratorVersion                               string
	FFmpeg, FFprobe                                string
	MaxUploadBytes, MaxOutputBytes                 uint64
	PrepareTimeout, ShutdownTimeout, QueueInterval time.Duration
	Runner                                         RunnerLimits
	Probe                                          ProbeLimits
}

func (c Config) Validate() error {
	if c.GeneratorVersion == "" || c.FFmpeg == "" || c.FFprobe == "" || c.MaxUploadBytes == 0 || c.MaxOutputBytes == 0 || c.MaxUploadBytes >= math.MaxInt64 || c.MaxOutputBytes >= math.MaxInt64 || c.PrepareTimeout <= 0 || c.ShutdownTimeout <= 0 || c.QueueInterval <= 0 {
		return ErrInvalid
	}
	return errors.Join(c.Runner.Validate(), c.Probe.Validate())
}

type Inspection interface {
	Source(context.Context, *os.File, core.AssetKind) (Source, error)
	Verify(context.Context, *os.File, core.GroupProfile) (core.MediaFormat, error)
}
type ProcessRunner interface {
	Run(context.Context, Command, *os.File, io.Writer) (RunResult, error)
}

// Tools позволяет явно подставить ограниченные тестовые порты без автоматического выбора CPU.
type Tools struct {
	Builder            CommandBuilder
	Inspector          Inspection
	Runner             ProcessRunner
	Normalize          func(context.Context, *os.File, io.Writer) error
	VerifyLoopDuration func(context.Context, *os.File, time.Duration) error
}
type Manager struct {
	service *core.Service
	files   *FileStore
	config  Config
	tools   Tools
	mu      sync.Mutex
	closed  bool
	jobs    map[core.ID]*Job
}

func NewManager(service *core.Service, files *FileStore, config Config) (*Manager, error) {
	runner := Runner{Limits: config.Runner}
	probe := Prober{Binary: config.FFprobe, Runner: runner, Limits: config.Probe}
	return NewManagerWithTools(service, files, config, Tools{Builder: ProductionBuilder(config.FFmpeg), Inspector: probe, Runner: runner, Normalize: probe.NormalizePrepared, VerifyLoopDuration: probe.VerifyLoopDuration})
}
func NewManagerWithTools(service *core.Service, files *FileStore, config Config, tools Tools) (*Manager, error) {
	if service == nil || files == nil || tools.Builder == nil || tools.Inspector == nil || tools.Runner == nil {
		return nil, ErrInvalid
	}
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &Manager{service: service, files: files, config: config, tools: tools, jobs: map[core.ID]*Job{}}, nil
}

type Result struct {
	Generation core.FallbackGeneration
	Err        error
	Clean      bool
}

// PreparationError сохраняет идентичность неоднозначного принятия без повтора внешней работы.
type PreparationError struct {
	GenerationID core.ID
	Err          error
}

func (e *PreparationError) Error() string {
	return "не подтверждено принятие подготовки " + string(e.GenerationID) + ": " + e.Err.Error()
}
func (e *PreparationError) Unwrap() error { return e.Err }

type Job struct {
	generation core.FallbackGeneration
	cancel     context.CancelFunc
	done       chan struct{}
	result     Result
	mu         sync.Mutex
	owner      *core.PreparationOwner
	shutdown   time.Duration
}

func (j *Job) Generation() core.FallbackGeneration { return cloneGeneration(j.generation) }
func (j *Job) Done() <-chan struct{}               { return j.done }
func (j *Job) Cancel() {
	j.mu.Lock()
	owner := j.owner
	j.mu.Unlock()
	if owner != nil {
		ctx, cancel := context.WithTimeout(context.Background(), j.shutdown)
		_ = owner.Revoke(ctx)
		cancel()
	}
	j.cancel()
}
func (j *Job) Wait(ctx context.Context) (Result, error) {
	select {
	case <-j.done:
		return j.completed(), j.result.Err
	default:
	}
	select {
	case <-j.done:
		return j.completed(), j.result.Err
	case <-ctx.Done():
		return Result{}, errors.Join(ErrCleanupPending, ctx.Err())
	}
}
func (j *Job) completed() Result {
	r := j.result
	r.Generation = cloneGeneration(r.Generation)
	return r
}
func cloneGeneration(g core.FallbackGeneration) core.FallbackGeneration {
	g.RequiredProfiles = append([]core.RequiredProfile(nil), g.RequiredProfiles...)
	for i := range g.RequiredProfiles {
		g.RequiredProfiles[i].CanonicalProfile = bytes.Clone(g.RequiredProfiles[i].CanonicalProfile)
	}
	if g.SourceFormat.Video != nil {
		v := *g.SourceFormat.Video
		g.SourceFormat.Video = &v
	}
	if g.SourceFormat.Audio != nil {
		a := *g.SourceFormat.Audio
		g.SourceFormat.Audio = &a
	}
	return g
}

// Upload принимает очередь до первого чтения; после успеха Manager владеет reader до закрытия.
func (m *Manager) Upload(ctx context.Context, request core.PreparationRequest, reader io.ReadCloser) (*Job, error) {
	if reader == nil || request.Rebuild {
		return nil, ErrInvalid
	}
	return m.enqueue(ctx, request, reader)
}
func (m *Manager) Rebuild(ctx context.Context, request core.PreparationRequest) (*Job, error) {
	request.Rebuild = true
	return m.enqueue(ctx, request, nil)
}
func (m *Manager) enqueue(ctx context.Context, request core.PreparationRequest, reader io.ReadCloser) (*Job, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	if m.closed {
		return nil, core.ErrClosed
	}
	if request.GeneratorVersion != "" && request.GeneratorVersion != m.config.GeneratorVersion {
		return nil, ErrInvalid
	}
	request.GeneratorVersion = m.config.GeneratorVersion
	g, err := m.service.ReservePreparation(ctx, request)
	if err != nil {
		if g.ID != "" {
			check, stop := context.WithTimeout(context.Background(), m.config.ShutdownTimeout)
			defer stop()
			stored, e := m.service.GetGeneration(check, g.ID)
			if e == nil && stored.AttemptToken == g.AttemptToken && stored.Incarnation == g.Incarnation && stored.State == core.PreparationQueued && !stored.Issued {
				err = errors.Join(err, m.service.CancelPreparation(check, core.PreparationFenceFor(stored), err.Error()))
			} else if !errors.Is(e, core.ErrNotFound) {
				err = errors.Join(err, e, ErrCleanupPending)
			}
			return nil, &PreparationError{GenerationID: g.ID, Err: err}
		}
		return nil, err
	}
	if reader != nil {
		reader = &ownedReader{ReadCloser: reader}
	}
	work, cancel := context.WithTimeout(context.Background(), m.config.PrepareTimeout)
	j := &Job{generation: g, cancel: cancel, done: make(chan struct{}), shutdown: m.config.ShutdownTimeout}
	m.jobs[g.ID] = j
	go func() {
		j.result = m.execute(work, g, reader, j)
		cancel()
		m.mu.Lock()
		delete(m.jobs, g.ID)
		m.mu.Unlock()
		close(j.done)
	}()
	return j, nil
}

type ownedReader struct {
	io.ReadCloser
	once sync.Once
	err  error
}

func (r *ownedReader) Close() error { r.once.Do(func() { r.err = r.ReadCloser.Close() }); return r.err }

func (m *Manager) execute(ctx context.Context, g core.FallbackGeneration, reader io.ReadCloser, j *Job) (result Result) {
	result.Generation = g
	fence := core.PreparationFenceFor(g)
	var owner *core.PreparationOwner
	var finish func()
	defer func() {
		cleanup, cancel := context.WithTimeout(context.Background(), m.config.ShutdownTimeout)
		defer cancel()
		stored, identityErr := m.service.GetGeneration(cleanup, g.ID)
		if identityErr == nil && stored.AttemptToken == g.AttemptToken && stored.Incarnation == g.Incarnation {
			fence = core.PreparationFenceFor(stored)
		}
		if reader != nil {
			result.Err = errors.Join(result.Err, reader.Close())
		}
		if owner != nil {
			result.Err = errors.Join(result.Err, owner.Revoke(cleanup))
		}
		if finish != nil {
			finish()
		}
		if result.Err != nil {
			current, err := m.service.GetGeneration(cleanup, g.ID)
			if err == nil && current.State == core.PreparationPublished {
				result.Generation = current
			} else {
				result.Err = errors.Join(result.Err, err, m.service.CancelPreparation(cleanup, fence, result.Err.Error()))
			}
		}
		ack := m.service.AcknowledgePreparationCleanup(cleanup, fence)
		result.Clean = ack == nil
		result.Err = errors.Join(result.Err, ack)
	}()
	for {
		var err error
		claimed, claimErr := m.service.ClaimPreparation(ctx, g.ID)
		err = claimErr
		if claimed.GenerationID != "" {
			fence = claimed
		}
		if err == nil {
			break
		}
		if !errors.Is(err, core.ErrBusy) {
			result.Err = err
			return result
		}
		timer := time.NewTimer(m.config.QueueInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			result.Err = ctx.Err()
			fence = core.PreparationFenceFor(g)
			return result
		case <-timer.C:
		}
	}
	if err := m.service.MarkPreparationIssued(ctx, fence); err != nil {
		// Неоднозначная запись проверяется по идентичности до любого внешнего действия.
		check, stop := context.WithTimeout(context.Background(), m.config.ShutdownTimeout)
		current, e := m.service.GetGeneration(check, g.ID)
		stop()
		if e != nil || core.PreparationFenceFor(current) != fence || !current.Issued || current.State != core.PreparationIssued {
			result.Err = errors.Join(err, e)
			return result
		}
	}
	var err error
	owner, err = m.service.BindPreparationOwner(ctx, fence)
	if err != nil {
		result.Err = err
		return result
	}
	j.mu.Lock()
	j.owner = owner
	j.mu.Unlock()
	finish, err = owner.Register(ctx)
	if err != nil {
		result.Err = err
		return result
	}
	work, cancel := context.WithCancel(ctx)
	watchDone := make(chan struct{})
	go func() {
		defer close(watchDone)
		select {
		case <-owner.Done():
			cancel()
		case <-work.Done():
			cleanup, stop := context.WithTimeout(context.Background(), m.config.ShutdownTimeout)
			_ = owner.Revoke(cleanup)
			stop()
		}
	}()
	defer func() { cancel(); <-watchDone }()
	g.Attempt = fence.Attempt
	publication, err := m.prepare(work, g, reader)
	if err != nil {
		result.Err = err
		return result
	}
	published, err := m.service.PublishPreparation(work, fence, publication)
	result.Err = err
	if err == nil {
		result.Generation = published
	}
	return result
}

func (m *Manager) prepare(ctx context.Context, g core.FallbackGeneration, reader io.ReadCloser) (publication core.GenerationPublication, retErr error) {
	if err := ctx.Err(); err != nil {
		return core.GenerationPublication{}, err
	}
	if err := m.files.CreateGeneration(g.ID); err != nil {
		return core.GenerationPublication{}, err
	}
	var source io.Reader = reader
	var opened *os.File
	if g.RebuildFromGenerationID != "" {
		old, err := m.service.GetGeneration(ctx, g.RebuildFromGenerationID)
		if err != nil {
			return core.GenerationPublication{}, err
		}
		opened, err = m.files.OpenExact(ctx, old.OriginalPath, old.SizeBytes, old.SourceHash)
		if err != nil {
			return core.GenerationPublication{}, err
		}
		defer func() { retErr = errors.Join(retErr, opened.Close()) }()
		source = opened
	}
	if source == nil {
		return core.GenerationPublication{}, ErrInvalid
	}
	file, err := m.files.Create(g.StagingPath)
	if err != nil {
		return core.GenerationPublication{}, err
	}
	var copyErr error
	if reader != nil {
		// Close выполняется при отмене, а возврат ожидает завершения самого Close.
		closed := make(chan struct{})
		stop := context.AfterFunc(ctx, func() { defer close(closed); _ = reader.Close() })
		_, copyErr = CopyBounded(ctx, file, source, m.config.MaxUploadBytes)
		if !stop() {
			<-closed
		}
	} else {
		_, copyErr = CopyBounded(ctx, file, source, m.config.MaxUploadBytes)
	}
	if copyErr != nil {
		return core.GenerationPublication{}, errors.Join(copyErr, file.Close())
	}
	size, hash, err := InspectFile(ctx, file, m.config.MaxUploadBytes)
	err = errors.Join(err, file.Sync(), file.Close())
	if err != nil {
		return core.GenerationPublication{}, err
	}
	if err = m.files.PublishRename(g.StagingPath, g.OriginalPath); err != nil {
		return core.GenerationPublication{}, err
	}
	input, err := m.files.OpenExact(ctx, g.OriginalPath, size, hash)
	if err != nil {
		return core.GenerationPublication{}, err
	}
	observed, err := m.tools.Inspector.Source(ctx, input, g.Kind)
	err = errors.Join(err, input.Close())
	if err != nil {
		return core.GenerationPublication{}, err
	}
	publication = core.GenerationPublication{SourceHash: hash, SizeBytes: size, SourceFormat: observed.Format}
	for _, required := range g.RequiredProfiles {
		v := required.Profile.Format.Video
		if v.Width > m.config.Probe.MaxWidth || v.Height > m.config.Probe.MaxHeight || uint64(v.Width)*uint64(v.Height) > m.config.Probe.MaxPixels {
			return publication, ErrLimit
		}
		variant, err := m.variant(ctx, g, required, observed, size, hash)
		if err != nil {
			return publication, err
		}
		publication.Variants = append(publication.Variants, variant)
	}
	return publication, nil
}

func (m *Manager) variant(ctx context.Context, g core.FallbackGeneration, p core.RequiredProfile, source Source, size uint64, hash string) (core.FallbackVariant, error) {
	loop, err := EffectiveLoopDuration(source.Duration, p.Profile, m.config.Probe.MaxDuration)
	if err != nil {
		return core.FallbackVariant{}, err
	}
	command, err := m.tools.Builder(GenerateRequest{Source: source, Profile: p.Profile, LoopDuration: loop})
	if err != nil {
		return core.FallbackVariant{}, err
	}
	if err = ctx.Err(); err != nil {
		return core.FallbackVariant{}, err
	}
	input, err := m.files.OpenExact(ctx, g.OriginalPath, size, hash)
	if err != nil {
		return core.FallbackVariant{}, err
	}
	output, err := m.files.Create(p.StagingPath)
	if err != nil {
		return core.FallbackVariant{}, errors.Join(err, input.Close())
	}
	_, err = m.tools.Runner.Run(ctx, command, input, &boundedWriter{w: output, remaining: m.config.MaxOutputBytes})
	err = errors.Join(err, input.Close())
	if err != nil {
		return core.FallbackVariant{}, errors.Join(err, output.Close())
	}
	variantSize, digest, err := InspectFile(ctx, output, m.config.MaxOutputBytes)
	err = errors.Join(err, output.Sync(), output.Close())
	if err != nil {
		return core.FallbackVariant{}, err
	}
	staging := p.StagingPath
	normalize := m.tools.Normalize
	if normalize != nil {
		staging = p.StagingPath + ".normalized"
		_, err = m.service.RegisterPreparationFile(ctx, core.PreparationFenceFor(g), core.OwnedFile{ManagedPath: staging, Kind: core.OwnedStaging})
		if err != nil {
			return core.FallbackVariant{}, err
		}
		raw, e := m.files.OpenExact(ctx, p.StagingPath, variantSize, digest)
		if e != nil {
			return core.FallbackVariant{}, e
		}
		normalized, e := m.files.Create(staging)
		if e != nil {
			return core.FallbackVariant{}, errors.Join(e, raw.Close())
		}
		err = normalize(ctx, raw, &boundedWriter{w: normalized, remaining: m.config.MaxOutputBytes})
		err = errors.Join(err, raw.Close())
		if err == nil {
			variantSize, digest, err = InspectFile(ctx, normalized, m.config.MaxOutputBytes)
		}
		err = errors.Join(err, normalized.Sync(), normalized.Close())
		if err != nil {
			return core.FallbackVariant{}, err
		}
	}
	verify, err := m.files.OpenExact(ctx, staging, variantSize, digest)
	if err != nil {
		return core.FallbackVariant{}, err
	}
	actual, err := m.tools.Inspector.Verify(ctx, verify, p.Profile)
	if err == nil && m.tools.VerifyLoopDuration != nil {
		err = m.tools.VerifyLoopDuration(ctx, verify, loop)
	}
	err = errors.Join(err, verify.Close())
	if err != nil {
		return core.FallbackVariant{}, err
	}
	if staging != p.StagingPath {
		if err = m.files.RemoveOwnedContext(ctx, core.OwnedFile{AssetID: g.AssetID, GenerationID: g.ID, ManagedPath: p.StagingPath, Kind: core.OwnedStaging}); err != nil {
			return core.FallbackVariant{}, err
		}
	}
	if err = m.files.PublishRename(staging, p.ManagedPath); err != nil {
		return core.FallbackVariant{}, err
	}
	return core.FallbackVariant{Record: core.Record{ID: p.VariantID}, AssetID: g.AssetID, GenerationID: g.ID, Profile: p.Profile, ProfileKey: p.ProfileKey, CanonicalProfile: p.CanonicalProfile, ManagedPath: p.ManagedPath, GeneratorVersion: g.GeneratorVersion, SourceHash: hash, SizeBytes: variantSize, ContentHash: digest, VerifiedFormat: actual}, nil
}

// Close отзывает все задания; истечение срока не подтверждает физическое завершение.
func (m *Manager) Close(ctx context.Context) error {
	m.mu.Lock()
	m.closed = true
	jobs := make([]*Job, 0, len(m.jobs))
	for _, j := range m.jobs {
		jobs = append(jobs, j)
	}
	m.mu.Unlock()
	for _, j := range jobs {
		j.Cancel()
	}
	var err error
	for _, j := range jobs {
		_, e := j.Wait(ctx)
		if errors.Is(e, ErrCleanupPending) {
			return errors.Join(err, e)
		}
		err = errors.Join(err, e)
	}
	return err
}

// CleanupFile сверяет намерение с реестром файлов и подтверждает долговечность удаления.
func (m *Manager) CleanupFile(ctx context.Context, intent core.CleanupIntent) error {
	if intent.Kind != core.CleanupFile {
		return ErrInvalid
	}
	stored, err := m.service.GetCleanup(ctx, intent.ID)
	if err != nil {
		return err
	}
	if stored.Kind != core.CleanupFile || core.CleanupFenceFor(stored) != core.CleanupFenceFor(intent) {
		return core.ErrStale
	}
	if stored.Acknowledged {
		return nil
	}
	fence := core.CleanupFenceFor(stored)
	file, err := m.service.AuthorizeFileCleanup(ctx, fence)
	if err != nil {
		return err
	}
	if err = m.files.RemoveOwnedContext(ctx, file); err != nil {
		return err
	}
	return m.service.AcknowledgeCleanup(ctx, fence)
}
