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

// Вычисляет SHA256 тестовых байтов в шестнадцатеричном нижнем регистре.
func acceptanceDigest(data []byte) string { /* h содержит SHA256 переданных тестовых байтов до шестнадцатеричного представления. */
	h := sha256.Sum256(data)
	return hex.EncodeToString(h[:])
}

// Создаёт FileStore во временном каталоге и регистрирует закрытие после теста.
func acceptanceFileStore(t *testing.T) (*FileStore, string) {
	t.Helper()
	// dir содержит временный корень тестового файлового хранилища.
	dir := t.TempDir()
	// s содержит открытое тестовое хранилище. err сообщает об отказе открытия.
	s, err := OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает FileStore после сценария и сообщает ошибку через t.Error.

		// err сообщает об отказе закрытия тестового хранилища.
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	return s, dir
}

// acceptanceInspection проверяет изображение настоящим Prober и подставляет результат проверки варианта.
type acceptanceInspection struct { /* verifyError задаёт управляемый отказ проверки готового варианта. */
	verifyError error
}

// Source проверяет фактическое изображение через Prober с тестовыми пределами.
func (i *acceptanceInspection) Source(ctx context.Context, f *os.File, kind core.AssetKind) (Source, error) {
	return (Prober{Limits: acceptanceManagerConfig().Probe}).Source(ctx, f, kind)
}

// Verify возвращает формат заданного профиля и управляемую ошибку без анализа содержимого файла.
func (i *acceptanceInspection) Verify(_ context.Context, _ *os.File, p core.GroupProfile) (core.MediaFormat, error) {
	return p.Format, i.verifyError
}

// acceptanceRunFunc позволяет передать управляемую функцию вместо исполнителя внешних команд.
type acceptanceRunFunc func(context.Context, Command, *os.File, io.Writer) (RunResult, error)

// Run передаёт команду и дескрипторы заданной тестовой функции исполнения.
func (f acceptanceRunFunc) Run(ctx context.Context, c Command, input *os.File, out io.Writer) (RunResult, error) {
	return f(ctx, c, input, out)
}

// Возвращает конечные бюджеты менеджера и управляемые имена инструментов для приёмочных тестов.
func acceptanceManagerConfig() Config {
	return Config{ReconcileLimit: 10, GeneratorVersion: "acceptance-generator", FFmpeg: "controlled-ffmpeg", FFprobe: "controlled-ffprobe", MaxUploadBytes: 1 << 16, MaxOutputBytes: 1 << 16, PrepareTimeout: 5 * time.Second, ShutdownTimeout: time.Second, QueueInterval: time.Millisecond, Runner: RunnerLimits{Timeout: time.Second, MaxArgs: 256, MaxCommandBytes: 1 << 16, MaxDiagnosticBytes: 1 << 16}, Probe: ProbeLimits{MaxJSONBytes: 1 << 16, MaxWidth: 4096, MaxHeight: 4096, MaxPixels: 4096 * 4096, MaxDuration: time.Minute, ImageDuration: time.Second, MaxPackets: 1000, Container: fmp4.Limits{MaxBoxBytes: 1 << 20, MaxInitBytes: 1 << 20, MaxMoofBytes: 1 << 16, MaxMdatBytes: 1 << 20, MaxSampleBytes: 1 << 18, MaxExtradataBytes: 1 << 12, MaxSamples: 100}}}
}

// Открывает временную SQLite, создаёт Service, Asset и Stream с тремя группами на двух различных
// профилях; хранилище закрывается после теста.
func acceptanceManagerService(t *testing.T) (*core.Service, *storage.Store, core.FallbackAsset, core.Stream) {
	t.Helper()
	// ctx задаёт контекст создания изолированных доменных данных.
	ctx := context.Background()
	// options задаёт явные бюджеты подготовки, исполнения, наблюдений и соединений временной SQLite.
	options := storage.Options{PreparationLimits: core.PreparationLimits{MaxQueued: 3, MaxProfiles: 8}, OutputExecutionLimits: core.OutputExecutionLimits{MaxReservations: 128, MaxIdentifierBytes: 1024, MaxBytes: 4 << 20}, ObservationLimits: core.ObservationLimits{MaxSessions: 8, MaxGroups: 32, MaxOutputs: 64, MaxFFmpegWorkers: 32, MaxIdentifierBytes: 1024, MaxErrorBytes: 65536, MaxBytes: 4 << 20}, MaxOpenConns: 4, MaxIdleConns: 4, ConnMaxLifetime: time.Minute, ConnMaxIdleTime: time.Minute, BusyTimeout: 10 * time.Millisecond, RetryAttempts: 3, RetryTimeout: time.Second, RetryDelay: time.Millisecond}
	// s содержит временное SQLite-хранилище. err сообщает об отказе его открытия.
	s, err := storage.Open(ctx, filepath.Join(t.TempDir(), "manager.sqlite"), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает временное SQLite-хранилище после теста и сообщает ошибку через t.Error.

		// err сообщает об отказе закрытия временного хранилища.
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	// svc содержит доменный сервис текущего воплощения временного хранилища.
	svc, err := core.NewService(s, s.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	// a содержит созданный Asset, выбранный заглушкой тестового потока.
	a, err := svc.CreateAsset(ctx, "Выбранная заглушка")
	if err != nil {
		t.Fatal(err)
	}
	// stream содержит поток с постоянным ключом публикации и выбранным Asset.
	stream, err := svc.CreateStream(ctx, core.Stream{Name: "Поток", PublicationKey: "manager-publication", Fallback: core.FallbackPolicy{AssetID: a.ID, Watchdog: core.WatchdogPolicy{VideoTimeout: time.Second, AudioTimeout: time.Second, RecoveryWindow: time.Second, KeyframeTimeout: time.Second}}})
	if err != nil {
		t.Fatal(err)
	}
	// profile содержит исходный профиль AV1/AAC, от которого строятся три группы с двумя различными
	// форматами.
	profile := core.GroupProfile{Mode: core.ModeCopy, Format: core.MediaFormat{Video: core.VideoFormat{Codec: core.CodecAV1, Width: 640, Height: 360, FrameRate: core.FrameRate{Numerator: 30, Denominator: 1}, CodecProfile: "main", PixelFormat: "yuv420p", BitDepth: 8, Color: core.ColorInfo{Range: "limited", Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"}}, Audio: core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", SampleRateHz: 48000, Channels: 2, ChannelLayout: "stereo"}}, VideoEncoding: core.VideoEncoding{TargetBitrateBPS: 300000, MaxBitrateBPS: 400000, BufferBits: 600000, GOPFrames: 30, ClosedGOP: true}, AudioEncoding: core.AudioEncoding{BitrateBPS: 64000}}
	// i задаёт номер одной из трёх создаваемых групп.
	for i := range 3 {
		// p содержит копию профиля, заменяемую на H.264 transcode для третьей группы.
		p := profile
		if i == 2 {
			p.Mode = core.ModeTranscode
			p.Format.Video.Codec = core.CodecH264
			p.Format.Video.CodecProfile = "high"
		}
		// saved содержит сохранённый профиль текущей группы. err сообщает об отказе его
		// создания.
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

// Кодирует небольшое изображение PNG в байты для загрузок через менеджер.
func acceptancePNG(t *testing.T) []byte {
	t.Helper()
	// data накапливает байты небольшого PNG для загрузки.
	var data bytes.Buffer
	// err сообщает об отказе кодирования тестового PNG.
	if err := png.Encode(&data, image.NewRGBA(image.Rect(0, 0, 16, 16))); err != nil {
		t.Fatal(err)
	}
	return data.Bytes()
}

// Создаёт менеджер с управляемыми портами генерации и проверки и регистрирует ограниченное закрытие
// после теста.
func acceptanceNewManager(t *testing.T, svc *core.Service, files *FileStore, c Config, inspect *acceptanceInspection, run ProcessRunner) *Manager {
	t.Helper()
	// m содержит менеджер с явно управляемыми портами. err сообщает об отказе создания.
	m, err := NewManagerWithTools(svc, files, c, Tools{Builder: func(GenerateRequest) (Command, error) {
		// Возвращает условную команду controlled для проверки Manager с подставленными портами.
		return Command{Binary: "controlled"}, nil
	}, Inspector: inspect, Runner: run})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает Manager с двухсекундным сроком при завершении теста, игнорируя ошибку.

		// ctx ограничивает резервное закрытие менеджера двумя секундами. cancel освобождает
		// срок.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = m.Close(ctx)
	})
	return m
}

// Загружает изображение с текущей ревизией Asset и ждёт успешную подготовку с подтверждённой очисткой
// в конечный срок.
func acceptanceUpload(t *testing.T, m *Manager, svc *core.Service, id core.ID, data []byte) Result {
	t.Helper()
	// ctx ограничивает принятие и завершение тестовой загрузки пятью секундами. cancel освобождает
	// срок.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// a содержит текущий Asset для передачи ожидаемой ревизии. err сообщает об отказе чтения.
	a, err := svc.GetAsset(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	// j содержит принятое задание загрузки изображения.
	j, err := m.Upload(ctx, core.PreparationRequest{AssetID: id, ExpectedRevision: a.Revision, Kind: core.AssetImage}, io.NopCloser(bytes.NewReader(data)))
	if err != nil {
		t.Fatal(err)
	}
	// r содержит завершённую подготовку с подтверждением очистки.
	r, err := j.Wait(ctx)
	if err != nil || !r.Clean {
		t.Fatalf("подготовка: %+v, %v", r, err)
	}
	return r
}

// Проверяет новые поколения и отдельные файлы при равных загрузках и повторной подготовке, сохраняя
// исходные байты и разделяя одинаковые профили только внутри поколения.
func TestManagerRegeneratesEqualUploadsAndRebuildWithoutSharingOldFiles(t *testing.T) {
	// svc содержит тестовый доменный сервис. asset содержит Asset проверяемой подготовки.
	svc, _, asset, _ := acceptanceManagerService(t)
	// files содержит изолированное файловое хранилище менеджера.
	files, _ := acceptanceFileStore(t)
	// inspect содержит управляемую проверку вариантов без анализа сжатых данных.
	inspect := &acceptanceInspection{}
	// calls атомарно считает запуски генератора для проверки отдельных поколений и общих профилей.
	var calls atomic.Int32
	// run проверяет начальную позицию входного дескриптора и записывает условный вариант.
	run := acceptanceRunFunc(func(_ context.Context, _ Command, input *os.File, out io.Writer) (RunResult, error) {
		// Проверяет нулевую позицию независимо открытого исходника, считает генерации и записывает условный
		// проверенный вариант.

		// position содержит текущую позицию дескриптора исходника. err сообщает об отказе её
		// проверки.
		position, err := input.Seek(0, io.SeekCurrent)
		if err != nil || position != 0 {
			return RunResult{}, errors.New("Дескриптор генератора не в начале")
		}
		calls.Add(1)
		_, err = out.Write([]byte("verified-controlled-variant"))
		return RunResult{}, err
	})
	// m содержит менеджер повторных загрузок и повторной подготовки.
	m := acceptanceNewManager(t, svc, files, acceptanceManagerConfig(), inspect, run)
	// data содержит одинаковые исходные PNG-байты для двух загрузок.
	data := acceptancePNG(t)
	// first содержит итог первой публикации поколения.
	first := acceptanceUpload(t, m, svc, asset.ID, data)
	// second содержит итог второй публикации тех же исходных байтов.
	second := acceptanceUpload(t, m, svc, asset.ID, data)
	if calls.Load() != 4 || len(first.Generation.RequiredProfiles) != 2 || len(second.Generation.RequiredProfiles) != 2 {
		t.Fatalf("одинаковые профили не разделяют один variant внутри поколения, либо equal upload не регенерирован: calls=%d", calls.Load())
	}
	if first.Generation.ID == second.Generation.ID || first.Generation.OriginalPath == second.Generation.OriginalPath {
		t.Fatal("повторная загрузка переиспользовала старый original")
	}
	// a содержит требуемый профиль первого поколения для сравнения путей и идентичностей.
	for _, a := range first.Generation.RequiredProfiles {
		// b содержит требуемый профиль второго поколения для проверки отсутствия
		// переиспользования файлов.
		for _, b := range second.Generation.RequiredProfiles {
			if a.ManagedPath == b.ManagedPath || a.VariantID == b.VariantID {
				t.Fatal("новое поколение переиспользовало прежний variant")
			}
		}
	}
	// ctx ограничивает сценарий пятью секундами. cancel освобождает этот срок.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// current содержит Asset после второй публикации. err сообщает об отказе чтения.
	current, err := svc.GetAsset(ctx, asset.ID)
	if err != nil {
		t.Fatal(err)
	}
	// j содержит задание повторной подготовки текущего поколения.
	j, err := m.Rebuild(ctx, core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: current.Revision})
	if err != nil {
		t.Fatal(err)
	}
	// rebuilt содержит итог повторной подготовки с новым комплектом файлов.
	rebuilt, err := j.Wait(ctx)
	if err != nil || !rebuilt.Clean || calls.Load() != 6 {
		t.Fatalf("rebuild не выполнил новый комплект: %+v calls=%d error=%v", rebuilt, calls.Load(), err)
	}
	if rebuilt.Generation.RebuildFromGenerationID != second.Generation.ID || rebuilt.Generation.OriginalPath == second.Generation.OriginalPath || rebuilt.Generation.SourceHash != second.Generation.SourceHash {
		t.Fatalf("rebuild не скопировал exact original в собственный путь: %+v", rebuilt.Generation)
	}
	// g содержит одно из трёх поколений для проверки сохранности его исходника.
	for _, g := range []core.FallbackGeneration{first.Generation, second.Generation, rebuilt.Generation} {
		// f содержит отдельно открытый исходник с проверенными размером и SHA256. err сообщает
		// об отказе открытия.
		f, err := files.OpenExact(ctx, g.OriginalPath, g.SizeBytes, g.SourceHash)
		if err != nil {
			t.Fatal(err)
		}
		// got содержит прочитанные байты исходника выбранного поколения.
		got, err := io.ReadAll(f)
		acceptanceClose(t, f)
		if err != nil || !bytes.Equal(got, data) {
			t.Fatalf("original изменён: %x, %v", got, err)
		}
	}
}

// Проверяет сохранение прежнего READY-поколения при превышении вывода, ошибках проверки, генератора и
// исходного файла.
func TestManagerFailedOutputAndVerificationPreserveCurrent(t *testing.T) {
	// failure выбирает превышение вывода, отказ проверки, отказ генератора либо неверный исходник.
	for _, failure := range []string{"output-limit", "verification", "runner", "invalid-source"} {
		t.Run(failure, func(t *testing.T) {
			// Проверяет сохранение прежнего current и завершение очистки при выбранной ошибке генератора, проверки
			// или источника.

			// svc содержит тестовый доменный сервис. asset содержит Asset проверяемой
			// подготовки.
			svc, _, asset, _ := acceptanceManagerService(t)
			// files содержит изолированное файловое хранилище менеджера.
			files, _ := acceptanceFileStore(t)
			// inspect содержит проверяющий порт с возможным управляемым отказом.
			inspect := &acceptanceInspection{}
			// fail включает выбранный отказ только после создания прежнего рабочего
			// поколения.
			fail := false
			// run записывает условный вариант либо моделирует отказ или превышение бюджета.
			run := acceptanceRunFunc(func(_ context.Context, _ Command, _ *os.File, out io.Writer) (RunResult, error) {
				// Возвращает ErrClosedPipe при выбранном отказе Runner либо пишет результат, превышающий бюджет только
				// в сценарии output-limit.

				if fail && failure == "runner" {
					return RunResult{}, io.ErrClosedPipe
				}
				// data содержит байты генерируемого варианта, увеличиваемые сверх
				// бюджета в нужном сценарии.
				data := []byte("verified-variant")
				if fail && failure == "output-limit" {
					data = bytes.Repeat([]byte{1}, int(acceptanceManagerConfig().MaxOutputBytes)+1)
				}
				// err сообщает об отказе записи вывода генератора.
				_, err := out.Write(data)
				return RunResult{}, err
			})
			// m содержит менеджер для проверки сохранения прежнего поколения.
			m := acceptanceNewManager(t, svc, files, acceptanceManagerConfig(), inspect, run)
			// data содержит PNG либо подставленное некорректное изображение.
			data := acceptancePNG(t)
			// old содержит прежнее опубликованное поколение, которое должно сохраниться.
			old := acceptanceUpload(t, m, svc, asset.ID, data)
			fail = true
			if failure == "verification" {
				inspect.verifyError = core.ErrCorrupt
			}
			if failure == "invalid-source" {
				data = []byte("not a PNG")
			}
			// ctx ограничивает сценарий пятью секундами. cancel освобождает этот срок.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			// a содержит Asset с актуальной ревизией перед неудачной заменой. err сообщает
			// об отказе чтения.
			a, err := svc.GetAsset(ctx, asset.ID)
			if err != nil {
				t.Fatal(err)
			}
			// j содержит задание замены, которое должно завершиться выбранной ошибкой.
			j, err := m.Upload(ctx, core.PreparationRequest{AssetID: a.ID, ExpectedRevision: a.Revision, Kind: core.AssetImage}, io.NopCloser(bytes.NewReader(data)))
			if err != nil {
				t.Fatal(err)
			}
			// result содержит итог отказавшей подготовки и подтверждение очистки.
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

// acceptanceCountReader учитывает чтения тела загрузки при проверке ожидания общего слота.
type acceptanceCountReader struct {
	// ReadCloser содержит вложенное тело загрузки, закрываемое менеджером после принятия.
	io.ReadCloser
	// reads атомарно считает обращения к чтению тела.
	reads atomic.Int32
}

// Read считает обращения к телу загрузки перед передачей чтения вложенному источнику.
func (r *acceptanceCountReader) Read(p []byte) (int, error) {
	r.reads.Add(1)
	return r.ReadCloser.Read(p)
}

// Проверяет общий слот двух менеджеров: очередь не читает тело, а отмена не освобождает слот и запрет
// эфира до фактического возврата генератора.
func TestManagersShareExecutionSlotAndCancellationWaitsForPhysicalRunner(t *testing.T) {
	// svc содержит первый доменный сервис. store содержит общее хранилище. asset содержит Asset
	// активной подготовки. stream содержит поток для проверки запрета запуска до завершения.
	svc, store, asset, stream := acceptanceManagerService(t)
	// files содержит общее файловое хранилище двух менеджеров.
	files, _ := acceptanceFileStore(t)
	// entered сообщает о начале первого генератора. cancelled сообщает о получении им отмены.
	// release разрешает его физический возврат.
	entered, cancelled, release := make(chan struct{}), make(chan struct{}), make(chan struct{})
	// calls атомарно считает запуски генератора обоими менеджерами.
	var calls atomic.Int32
	// run задерживает первый запуск после отмены, а последующим записывает условный результат.
	run := acceptanceRunFunc(func(ctx context.Context, _ Command, _ *os.File, out io.Writer) (RunResult, error) {
		// Задерживает первую попытку Runner после отмены до release, чтобы проверить удержание общего слота;
		// последующие попытки записывают результат.

		if calls.Add(1) == 1 {
			close(entered)
			<-ctx.Done()
			close(cancelled)
			<-release
			return RunResult{}, ctx.Err()
		}
		// err сообщает об отказе записи результата последующих запусков.
		_, err := out.Write([]byte("controlled-result"))
		return RunResult{}, err
	})
	// inspect содержит общую управляемую проверку вариантов.
	inspect := &acceptanceInspection{}
	// m содержит первый менеджер, занимающий общий слот.
	m := acceptanceNewManager(t, svc, files, acceptanceManagerConfig(), inspect, run)
	// otherSvc содержит второй доменный сервис того же хранилища. err сообщает об отказе создания.
	otherSvc, err := core.NewService(store, store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	// other содержит второй менеджер, ожидающий общий слот подготовки.
	other := acceptanceNewManager(t, otherSvc, files, acceptanceManagerConfig(), inspect, run)
	// ctx ограничивает сценарий пятью секундами. cancel освобождает этот срок.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// data содержит PNG, используемый обеими загрузками.
	data := acceptancePNG(t)
	// job содержит первую активную подготовку.
	job, err := m.Upload(ctx, core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage}, io.NopCloser(bytes.NewReader(data)))
	if err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	// released показывает, открыт ли барьер задержанного генератора, чтобы очистка не закрыла канал
	// повторно.
	released := false
	defer func() {
		// Освобождает задержанный Runner при раннем завершении теста, если release ещё не закрыт.

		if !released {
			close(release)
		}
	}()
	// queuedAsset содержит отдельный Asset ожидающей подготовки.
	queuedAsset, err := svc.CreateAsset(ctx, "Ожидающая заглушка")
	if err != nil {
		t.Fatal(err)
	}
	// reader содержит тело второй загрузки со счётчиком чтений.
	reader := &acceptanceCountReader{ReadCloser: io.NopCloser(bytes.NewReader(data))}
	// queued содержит задание второго менеджера, ожидающее физического завершения первого.
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
	// short задаёт заранее отменённое ожидание первого задания. cancelShort отменяет его до Wait.
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
	// result содержит итог первой отменённой, затем второй успешной подготовки.
	result, err := job.Wait(ctx)
	if !errors.Is(err, context.Canceled) || !result.Clean {
		t.Fatalf("runner join не завершил отмену: %+v, %v", result, err)
	}
	result, err = queued.Wait(ctx)
	if err != nil || !result.Clean || reader.reads.Load() == 0 {
		t.Fatalf("следующий Manager не получил освобождённый слот: %+v reads=%d error=%v", result, reader.reads.Load(), err)
	}
}

// Проверяет независимые позиции открытых дескрипторов и отказ чтения и очистки при изменении
// содержимого без изменения размера.
func TestVerifiedReadersHaveIndependentPositionsAndRejectCorruption(t *testing.T) {
	// s содержит файловое хранилище. dir содержит его корень для изменения тестового файла.
	s, dir := acceptanceFileStore(t)
	// ctx задаёт контекст проверки независимых дескрипторов.
	ctx := context.Background()
	// data содержит исходное содержимое для проверки позиций и SHA256.
	data := []byte("independent seekable media descriptors")
	// err сообщает об отказе записи исходного тестового файла.
	if err := os.WriteFile(filepath.Join(dir, "media.mp4"), data, 0600); err != nil {
		t.Fatal(err)
	}
	// m содержит ожидаемые путь, размер и SHA256 проверяемого файла.
	m := core.ArtifactManifest{ManagedPath: "media.mp4", SizeBytes: uint64(len(data)), ContentHash: acceptanceDigest(data)}
	// a содержит первый независимо открытый дескриптор. err сообщает об отказе проверки файла.
	a, err := s.OpenVerified(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	defer acceptanceClose(t, a)
	// b содержит второй независимо открытый дескриптор с собственной позицией чтения.
	b, err := s.OpenVerified(ctx, m)
	if err != nil {
		t.Fatal(err)
	}
	defer acceptanceClose(t, b)
	// first принимает первые семь байтов из первого дескриптора.
	first := make([]byte, 7)
	if _, err = io.ReadFull(a, first); err != nil {
		t.Fatal(err)
	}
	// got содержит полное содержимое второго, затем третьего независимо открытого дескриптора.
	got, err := io.ReadAll(b)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("второй читатель унаследовал позицию: %q, %v", got, err)
	}
	// rest содержит оставшиеся байты первого дескриптора после чтения второго.
	rest, err := io.ReadAll(a)
	if err != nil || !bytes.Equal(rest, data[7:]) {
		t.Fatalf("чтение второго дескриптора изменило первый: %q, %v", rest, err)
	}
	// c содержит третий проверенный дескриптор для проверки повторного начала с нуля.
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
	// f содержит ошибочно принятый повреждённый файл при неожиданном успехе. err содержит результат
	// проверки изменённого содержимого.
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

// Проверяет отказ выхода через пути и ссылки, сохранение занятого конечного файла и успешное
// переименование по свободному пути.
func TestManagedFilesConfineLinksAndNeverOverwriteFinal(t *testing.T) {
	// s содержит файловое хранилище. dir содержит его временный корень.
	s, dir := acceptanceFileStore(t)
	// outside содержит отдельный каталог, за который запрещено выходить файловым операциям.
	outside := t.TempDir()
	// data содержит контрольные байты внешнего файла.
	data := []byte("outside-owned-data")
	// err сообщает об отказе создания внешнего контрольного файла.
	if err := os.WriteFile(filepath.Join(outside, "secret"), data, 0600); err != nil {
		t.Fatal(err)
	}
	// err сообщает об отказе создания ссылки на внешний каталог.
	if err := os.Symlink(outside, filepath.Join(dir, "escape")); err != nil {
		t.Fatal(err)
	}
	// err сообщает об отказе создания ссылки на внешний файл.
	if err := os.Symlink(filepath.Join(outside, "secret"), filepath.Join(dir, "link.mp4")); err != nil {
		t.Fatal(err)
	}
	// name содержит очередной выходящий из корня путь либо символьную ссылку.
	for _, name := range []string{"../secret", "/secret", "escape/secret", "link.mp4"} {
		t.Run(name, func(t *testing.T) {
			// Проверяет отказ точного открытия для выбранного выхода из корня или символической ссылки.

			// f содержит ошибочно открытый внешний файл при неожиданном успехе. err содержит
			// результат проверки запрещённого пути.
			if f, err := s.OpenExact(context.Background(), name, uint64(len(data)), acceptanceDigest(data)); err == nil {
				acceptanceClose(t, f)
				t.Fatal("managed path открыл файл вне владения")
			}
		})
	}
	// name выбирает исходный промежуточный либо уже занятый конечный путь.
	for _, name := range []string{"first.stage", "final.mp4"} {
		// err сообщает об отказе записи файлов для проверки конфликта переименования.
		if err := os.WriteFile(filepath.Join(dir, name), []byte(name), 0600); err != nil {
			t.Fatal(err)
		}
	}
	// err содержит результат переименования на уже занятый конечный путь.
	if err := s.PublishRename("first.stage", "final.mp4"); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("существующий final не защищён: %v", err)
	}
	// got содержит байты конечного, затем исходного файла после отклонённого переименования. err
	// сообщает об отказе чтения.
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

// Проверяет точную границу копирования, отсутствие записи сверх бюджета и отсутствие изменений при
// заранее отменённом контексте.
func TestBoundedCopyEnforcesBytesAndCancellationBeforeWrites(t *testing.T) {
	// size выбирает число исходных байтов вокруг восьмибайтового предела и значительно выше него.
	for _, size := range []int{7, 8, 9, 32768} {
		t.Run(string(rune('a'+size%26)), func(t *testing.T) {
			// Проверяет копирование выбранного размера при бюджете 8 байт: допустимые данные сохраняются точно,
			// превышение даёт ErrLimit без лишней записи.

			// dst принимает байты ограниченного копирования.
			var dst bytes.Buffer
			// src содержит повторяющиеся контрольные байты выбранного размера.
			src := bytes.Repeat([]byte{0x5a}, size)
			// n содержит число скопированных байтов. err содержит результат копирования с
			// бюджетом восемь байтов.
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
	// ctx задаёт заранее отменяемое копирование. cancel отменяет его до записи.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// dst принимает возможный вывод заранее отменённого копирования и должен остаться пустым.
	var dst bytes.Buffer
	// n содержит число скопированных байтов. err содержит результат копирования с заранее отменённым
	// контекстом.
	n, err := CopyBounded(ctx, &dst, bytes.NewReader([]byte("payload")), 32)
	if !errors.Is(err, context.Canceled) || n != 0 || dst.Len() != 0 {
		t.Fatalf("отменённая запись изменила файл: n=%d bytes=%d error=%v", n, dst.Len(), err)
	}
}

// Проверяет повторное удаление отсутствующего файла и сохранение внешнего файла при замене
// управляемого пути символической ссылкой.
func TestExactCleanupIsIdempotentAndDoesNotFollowReplacementLinks(t *testing.T) {
	// s содержит файловое хранилище. dir содержит его корень для удаления и подмены пути.
	s, dir := acceptanceFileStore(t)
	// ctx задаёт контекст повторной точной очистки.
	ctx := context.Background()
	// data содержит контрольные байты принадлежащего файла и защищаемого внешнего файла.
	data := []byte("owned-media")
	// err сообщает об отказе записи принадлежащего файла.
	if err := os.WriteFile(filepath.Join(dir, "owned.mp4"), data, 0600); err != nil {
		t.Fatal(err)
	}
	// i задаёт номер первой либо повторной очистки одного пути.
	for i := range 2 {
		// err содержит результат точного удаления, включая повтор после отсутствия файла.
		if err := s.RemoveExact(ctx, "owned.mp4", uint64(len(data)), acceptanceDigest(data)); err != nil {
			t.Fatalf("идемпотентная очистка %d: %v", i, err)
		}
	}
	// outside содержит путь внешнего файла, который должна сохранить подменяющая ссылка.
	outside := filepath.Join(t.TempDir(), "keep.mp4")
	// err сообщает об отказе записи внешнего контрольного файла.
	if err := os.WriteFile(outside, data, 0600); err != nil {
		t.Fatal(err)
	}
	// err сообщает об отказе создания подменяющей символьной ссылки.
	if err := os.Symlink(outside, filepath.Join(dir, "owned.mp4")); err != nil {
		t.Fatal(err)
	}
	// err содержит результат попытки очистки подменяющей ссылки.
	if err := s.RemoveExact(ctx, "owned.mp4", uint64(len(data)), acceptanceDigest(data)); err == nil {
		t.Fatal("cleanup последовал замещающей ссылке")
	}
	// got содержит байты внешнего файла после отклонённой очистки. err сообщает об отказе чтения.
	got, err := os.ReadFile(outside)
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("cleanup изменил внешний файл: %q, %v", got, err)
	}
}

// Закрывает переданный ресурс и отмечает ошибку в тесте.
func acceptanceClose(t *testing.T, c io.Closer) {
	t.Helper()
	// err сообщает об отказе закрытия переданного тестового ресурса.
	if err := c.Close(); err != nil {
		t.Error(err)
	}
}

// Проверяет точный сохранённый допуск очистки после нового FileStore, отказ подменённого намерения и
// непустого каталога и сохранение текущего поколения.
func TestCleanupUsesDurableIntentAndPreservesUnknownObjectsAndCurrent(t *testing.T) {
	// svc содержит тестовый доменный сервис. asset содержит Asset проверяемой подготовки.
	svc, _, asset, _ := acceptanceManagerService(t)
	// files содержит первоначальное файловое хранилище. dir содержит его корень для повторного
	// открытия.
	files, dir := acceptanceFileStore(t)
	// ctx задаёт контекст проверки долговечной очистки поколений.
	ctx := context.Background()
	// run записывает условный вариант для проверки файлового протокола очистки.
	run := acceptanceRunFunc(func(_ context.Context, _ Command, _ *os.File, w io.Writer) (RunResult, error) {
		// Записывает условные проверенные медиаданные для последующей проверки долговечной очистки поколений.

		// err сообщает об отказе записи условного варианта.
		_, err := w.Write([]byte("controlled-verified-media"))
		return RunResult{}, err
	})
	// m содержит менеджер, публикующий два поколения и инициирующий очистку прежнего.
	m := acceptanceNewManager(t, svc, files, acceptanceManagerConfig(), &acceptanceInspection{}, run)
	// old содержит прежнее поколение, которое будет удалено.
	old := acceptanceUpload(t, m, svc, asset.ID, acceptancePNG(t))
	// current содержит новое выбранное поколение, которое очистка должна сохранить.
	current := acceptanceUpload(t, m, svc, asset.ID, acceptancePNG(t))
	// g содержит текущее состояние прежнего поколения перед удалением. err сообщает об отказе
	// чтения.
	g, err := svc.GetGeneration(ctx, old.Generation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.DeleteGeneration(ctx, g.ID, g.Revision); err != nil {
		t.Fatal(err)
	}
	// page содержит неподтверждённые намерения очистки после удаления поколения.
	page, err := svc.ListCleanups(ctx, core.PageRequest{Query: core.ListQuery{Limit: 100, PendingOnly: true}})
	if err != nil {
		t.Fatal(err)
	}
	// intents собирает намерения очистки файлов удалённого поколения.
	var intents []core.CleanupIntent
	// directory сохраняет намерение очистки каталога, выполняемое после файлов.
	var directory core.CleanupIntent
	// intent обозначает очередное намерение при отборе записей удалённого поколения.
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
	// forged содержит намерение с подменённым путём текущего поколения для проверки отказа до
	// файловых действий.
	forged := intents[0]
	forged.ManagedPath = current.Generation.OriginalPath
	if err = m.CleanupFile(ctx, forged); !errors.Is(err, core.ErrStale) {
		t.Fatalf("подмена intent не отвергнута до IO: %v", err)
	}
	// restarted содержит повторно открытое файловое хранилище без прежних локальных наблюдений.
	restarted, err := OpenFileStore(dir)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает повторно открытый FileStore после проверки долговечных намерений и сообщает ошибку через
		// t.Error.

		// err сообщает об отказе закрытия повторно открытого хранилища.
		if err := restarted.Close(); err != nil {
			t.Error(err)
		}
	})
	// cleanup содержит менеджер очистки с новым экземпляром FileStore.
	cleanup := acceptanceNewManager(t, svc, restarted, acceptanceManagerConfig(), &acceptanceInspection{}, run)
	// intent содержит очередное точное намерение очистки прежнего файла.
	for _, intent := range intents {
		if err = cleanup.CleanupFile(ctx, intent); err != nil {
			t.Fatalf("durable file proof после нового FileStore: %s: %v", intent.ManagedPath, err)
		}
		if err = cleanup.CleanupFile(ctx, intent); err != nil {
			t.Fatalf("повтор exact intent: %v", err)
		}
	}
	// unknown содержит путь постороннего файла, который не должен удаляться вместе с каталогом.
	unknown := filepath.Join(dir, core.GenerationDirectory(g.ID), "foreign.keep")
	if err = os.WriteFile(unknown, []byte("unknown"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = m.CleanupFile(ctx, directory); err == nil {
		t.Fatal("cleanup рекурсивно удалил неизвестный объект")
	}
	// got содержит байты постороннего файла после отклонённой очистки каталога. err сообщает об
	// отказе чтения.
	if got, err := os.ReadFile(unknown); err != nil || string(got) != "unknown" {
		t.Fatalf("неизвестный файл изменён: %q, %v", got, err)
	}
	// stored содержит сохранённое намерение каталога для проверки отсутствия подтверждения после
	// отказа.
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
	// f содержит проверенный исходник текущего поколения после очистки прежнего.
	f, err := files.OpenExact(ctx, current.Generation.OriginalPath, current.Generation.SizeBytes, current.Generation.SourceHash)
	if err != nil {
		t.Fatalf("очистка прежнего поколения затронула current: %v", err)
	}
	acceptanceClose(t, f)
}
