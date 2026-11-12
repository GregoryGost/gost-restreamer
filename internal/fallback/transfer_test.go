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

// transferReader управляет чтением и закрытием тела загрузки, считая обращения и выдавая контрольные
// сигналы.
type transferReader struct {
	// read задаёт управляемое чтение тестового тела.
	read func([]byte) (int, error)
	// close при наличии задаёт задержку или ошибку закрытия.
	close func() error
	// reads атомарно считает обращения к Read.
	reads atomic.Int32
	// closes атомарно считает обращения к Close.
	closes atomic.Int32
	// closed закрывается после завершения управляемого Close.
	closed chan struct{}
	// closeStarted при наличии закрывается в начале Close до управляемой задержки.
	closeStarted chan struct{}
}

// Read считает обращения к телу и передаёт чтение заданной тестовой функции.
func (r *transferReader) Read(p []byte) (int, error) {
	r.reads.Add(1)
	return r.read(p)
}

// Close считает закрытия, сообщает об их начале, выполняет управляемую задержку или ошибку и закрывает
// канал завершения.
func (r *transferReader) Close() error {
	r.closes.Add(1)
	if r.closeStarted != nil {
		close(r.closeStarted)
	}
	// err сохраняет результат управляемого закрытия тела.
	var err error
	if r.close != nil {
		err = r.close()
	}
	close(r.closed)
	return err
}

// transferInspection подставляет управляемую проверку источника, сохраняя проверку варианта
// acceptanceInspection.
type transferInspection struct {
	// acceptanceInspection предоставляет тестовую проверку готового варианта.
	acceptanceInspection
	// source задаёт управляемую проверку исходного файла.
	source func(context.Context, *os.File, core.AssetKind) (Source, error)
}

// Source передаёт проверку источника управляемой функции теста.
func (i transferInspection) Source(ctx context.Context, f *os.File, kind core.AssetKind) (Source, error) {
	return i.source(ctx, f, kind)
}

// Создаёт менеджер с управляемым генератором и инспекцией, регистрируя ограниченное закрытие с
// проверкой ошибки.
func transferManager(t *testing.T, svc *core.Service, files *FileStore, c Config, inspect Inspection) *Manager {
	t.Helper()
	// m содержит менеджер с управляемыми портами передачи. err сообщает об отказе его создания.
	m, err := NewManagerWithTools(svc, files, c, Tools{
		Builder: func(GenerateRequest) (Command, error) {
			// Возвращает условную команду controlled для изолированной проверки передачи.
			return Command{Binary: "controlled"}, nil
		},
		Inspector: inspect,
		Runner: acceptanceRunFunc(func(_ context.Context, _ Command, _ *os.File, out io.Writer) (RunResult, error) {
			// Записывает условные байты проверенного варианта и возвращает ошибку выхода.

			// err сообщает об отказе записи условных байтов варианта.
			_, err := out.Write([]byte("controlled-verified-variant"))
			return RunResult{}, err
		}),
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает Manager с отдельным двухсекундным сроком и сообщает ошибку через t.Error.

		// ctx ограничивает закрытие менеджера двумя секундами. cancel освобождает срок.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		// err сообщает об отказе закрытия тестового менеджера.
		if err := m.Close(ctx); err != nil {
			t.Error(err)
		}
	})
	return m
}

// Ожидает контрольный сигнал до отмены контекста и завершает тест при истечении ожидания.
func transferAwait(t *testing.T, ctx context.Context, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
}

// Проверяет сохранение исходного файла и завершение Close до подтверждения передачи и инспекции, затем
// независимое завершение подготовки после отмены запроса.
func TestWaitUploadClosesReaderBeforeInspectionAndPreservesBackground(t *testing.T) {
	// svc содержит тестовый доменный сервис. asset содержит Asset проверяемой загрузки.
	svc, _, asset, _ := acceptanceManagerService(t)
	// files содержит файловое хранилище. directory содержит его корень для проверки сохранённого
	// исходника.
	files, directory := acceptanceFileStore(t)
	// ctx ограничивает сценарий пятью секундами. cancel освобождает этот срок.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// data содержит допустимый PNG, передаваемый менеджеру.
	data := acceptancePNG(t)
	// closeRelease разрешает закончить Close тела. inspectEntered сообщает о входе в инспекцию.
	// inspectRelease разрешает продолжить инспекцию.
	closeRelease, inspectEntered, inspectRelease := make(chan struct{}), make(chan struct{}), make(chan struct{})
	// releaseClose однократно открывает барьер Close. releaseInspect однократно открывает барьер
	// инспекции.
	var releaseClose, releaseInspect sync.Once
	defer releaseClose.Do(func() {
		// Освобождает задержанное закрытие источника при завершении теста.
		close(closeRelease)
	})
	defer releaseInspect.Do(func() {
		// Освобождает задержанную проверку источника при завершении теста.
		close(inspectRelease)
	})
	// r содержит тело PNG с управляемой задержкой закрытия.
	r := &transferReader{read: bytes.NewReader(data).Read, closed: make(chan struct{}), closeStarted: make(chan struct{}), close: func() error {
		// Задерживает Close источника до сигнала closeRelease, затем возвращает успех.
		<-closeRelease
		return nil
	}}
	// inspect проверяет, что тело уже закрыто, и задерживает дальнейшую подготовку до разрешения.
	inspect := transferInspection{source: func(work context.Context, f *os.File, kind core.AssetKind) (Source, error) {
		// Требует завершённого Close до проверки источника и ждёт inspectRelease либо отмены рабочего
		// контекста.

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
	// m содержит менеджер с задерживаемой инспекцией.
	m := transferManager(t, svc, files, acceptanceManagerConfig(), &inspect)
	// j содержит принятое задание загрузки. err сообщает об отказе принятия.
	j, err := m.Upload(ctx, core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage}, r)
	if err != nil {
		t.Fatal(err)
	}
	// request задаёт отдельно отменяемое ожидание передачи. cancelRequest отменяет его после
	// успешной передачи.
	request, cancelRequest := context.WithCancel(ctx)
	defer cancelRequest()
	// waited передаёт результат WaitUpload из ожидающей горутины.
	waited := make(chan error, 1)
	go func() {
		// Передаёт результат WaitUpload в waited, оставляя основной тест свободным для управления закрытием и
		// проверкой.
		waited <- j.WaitUpload(request)
	}()
	transferAwait(t, ctx, r.closeStarted)
	select {
	// err содержит преждевременно возвращённый результат передачи, который нарушил бы ожидание
	// Close.
	case err := <-waited:
		t.Fatalf("передача подтверждена до завершения Close: %v", err)
	case <-inspectEntered:
		t.Fatal("проверка началась во время Close")
	default:
	}
	// final содержит конечный путь исходника принятого поколения.
	final := j.Generation().OriginalPath
	// got содержит сохранённые байты исходника до освобождения Close.
	got, err := os.ReadFile(filepath.Join(directory, final))
	if err != nil || !bytes.Equal(got, data) {
		t.Fatalf("исходный файл не сохранён до закрытия reader: %x, %v", got, err)
	}
	if _, err = os.Stat(filepath.Join(directory, j.Generation().StagingPath)); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("исходный staging не переименован: %v", err)
	}
	releaseClose.Do(func() {
		// Разрешает завершить Close источника после проверки сохранённого исходного файла.
		close(closeRelease)
	})
	select {
	// err содержит результат передачи после завершения Close.
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
	releaseInspect.Do(func() {
		// Разрешает продолжить проверку источника после подтверждённой передачи и отмены запроса.
		close(inspectRelease)
	})
	// result содержит итог фоновой подготовки после поздней отмены запроса.
	result, err := j.Wait(ctx)
	if err != nil || !result.Clean || result.Generation.State != core.PreparationPublished || r.closes.Load() != 1 {
		t.Fatalf("фон после отмены запроса: %+v, закрытий=%d, ошибка=%v", result, r.closes.Load(), err)
	}
}

// Проверяет отмену очереди запросом, заданием, сроком или менеджером: тело закрывается без чтения, а
// поколение не остаётся выданным или ожидающим очистки.
func TestWaitUploadCancellationBeforeClaimClosesWithoutReading(t *testing.T) {
	// mode выбирает способ отмены ожидающего задания до захвата слота.
	for _, mode := range []string{"request-cancel", "request-deadline", "job-cancel", "job-deadline", "manager-close"} {
		t.Run(mode, func(t *testing.T) {
			// Проверяет выбранный способ отмены до Claim: источник не читается, закрывается один раз, а задание
			// сохраняет соответствующую причину и завершает очистку.

			// svc содержит доменный сервис. blockerAsset содержит Asset подготовки,
			// занимающей слот.
			svc, _, blockerAsset, _ := acceptanceManagerService(t)
			// files содержит изолированное файловое хранилище менеджера.
			files, _ := acceptanceFileStore(t)
			// ctx ограничивает сценарий пятью секундами. cancel освобождает этот срок.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			// blocker содержит зарезервированное поколение, которым занят слот. err сообщает
			// об отказе резервирования.
			blocker, err := svc.ReservePreparation(ctx, core.PreparationRequest{AssetID: blockerAsset.ID, ExpectedRevision: blockerAsset.Revision, Kind: core.AssetImage, GeneratorVersion: "controlled"})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = svc.ClaimPreparation(ctx, blocker.ID); err != nil {
				t.Fatal(err)
			}
			// a содержит отдельный Asset для ожидающей загрузки.
			a, err := svc.CreateAsset(ctx, "Ожидающий файл")
			if err != nil {
				t.Fatal(err)
			}
			// c содержит конфигурацию с длинным интервалом очереди и при необходимости
			// коротким сроком задания.
			c := acceptanceManagerConfig()
			c.QueueInterval = time.Hour
			if mode == "job-deadline" {
				c.PrepareTimeout = 50 * time.Millisecond
			}
			// m содержит менеджер, чья загрузка должна ждать занятого слота.
			m := transferManager(t, svc, files, c, &acceptanceInspection{})
			// r содержит непрочитанное тело загрузки с учётом чтений и закрытий.
			r := &transferReader{read: bytes.NewReader(acceptancePNG(t)).Read, closed: make(chan struct{})}
			// j содержит ожидающее задание загрузки.
			j, err := m.Upload(ctx, core.PreparationRequest{AssetID: a.ID, ExpectedRevision: a.Revision, Kind: core.AssetImage}, r)
			if err != nil {
				t.Fatal(err)
			}
			// waitCtx задаёт отменяемое ожидание передачи. stop отменяет его в сценарии
			// отмены запроса.
			waitCtx, stop := context.WithCancel(ctx)
			defer stop()
			// want содержит ожидаемую причину завершения передачи.
			want := context.Canceled
			switch mode {
			case "request-cancel":
				stop()
			case "request-deadline":
				// deadlineCancel освобождает отдельный короткий срок ожидания передачи.
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
			// result содержит итог отменённого задания и признак очистки.
			result, err := j.Wait(ctx)
			// wantJob содержит ожидаемую причину завершения задания, которая может
			// отличаться от срока запроса передачи.
			wantJob := want
			if mode == "request-deadline" {
				// Срок запроса завершает передачу с DeadlineExceeded, а задание получает Cancel.
				wantJob = context.Canceled
			}
			if !errors.Is(err, wantJob) || !result.Clean || r.reads.Load() != 0 || r.closes.Load() != 1 {
				t.Fatalf("отмена до claim: %+v, чтений=%d, закрытий=%d, ошибка=%v", result, r.reads.Load(), r.closes.Load(), err)
			}
			// stored содержит сохранённое поколение после отмены для проверки снятия выдачи
			// и очистки.
			stored, err := svc.GetGeneration(ctx, j.Generation().ID)
			if err != nil || stored.State != core.PreparationFailed || stored.Issued || stored.CleanupPending {
				t.Fatalf("очередь после отмены: %+v, %v", stored, err)
			}
		})
	}
}

// Проверяет, что отмена ожидания передачи закрывает тело загрузки и освобождает заблокированное
// копирование с завершённой очисткой задания.
func TestWaitUploadCancellationDuringCopyUnblocksReader(t *testing.T) {
	// svc содержит тестовый доменный сервис. asset содержит Asset проверяемой загрузки.
	svc, _, asset, _ := acceptanceManagerService(t)
	// files содержит изолированное файловое хранилище менеджера.
	files, _ := acceptanceFileStore(t)
	// ctx ограничивает сценарий пятью секундами. cancel освобождает этот срок.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// entered сообщает о начале блокирующего чтения. closed освобождает это чтение после Close.
	entered, closed := make(chan struct{}), make(chan struct{})
	// r моделирует тело, чтение которого возвращается только после закрытия.
	r := &transferReader{closed: closed, read: func([]byte) (int, error) {
		// Сообщает о начале чтения, ждёт закрытия источника и возвращает ErrClosedPipe, моделируя блокирующее
		// чтение при отмене.

		close(entered)
		<-closed
		return 0, io.ErrClosedPipe
	}}
	// m содержит менеджер для отмены передачи во время копирования.
	m := transferManager(t, svc, files, acceptanceManagerConfig(), &acceptanceInspection{})
	// j содержит принятое задание с заблокированным чтением. err сообщает об отказе принятия.
	j, err := m.Upload(ctx, core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage}, r)
	if err != nil {
		t.Fatal(err)
	}
	transferAwait(t, ctx, entered)
	// request задаёт контекст ожидания передачи. stop отменяет его после входа в чтение.
	request, stop := context.WithCancel(ctx)
	stop()
	if err = j.WaitUpload(request); !errors.Is(err, context.Canceled) {
		t.Fatal(err)
	}
	transferAwait(t, ctx, closed)
	// result содержит итог задания после прерывания копирования и закрытия тела.
	result, err := j.Wait(ctx)
	if err == nil || !result.Clean || r.closes.Load() != 1 {
		t.Fatalf("незавершённое копирование: %+v, закрытий=%d, ошибка=%v", result, r.closes.Load(), err)
	}
}

// Проверяет, что принятая во время Close отмена сохраняется в результате передачи, а ожидание не
// возвращается до единственного закрытия тела загрузки.
func TestWaitUploadCancellationWinsBeforeReaderCloseCompletes(t *testing.T) {
	// ctx ограничивает проверку гонки завершения передачи пятью секундами. stop освобождает срок.
	ctx, stop := context.WithTimeout(context.Background(), 5*time.Second)
	defer stop()
	// work представляет отменяемую подготовку. cancel должен сработать при принятии отмены передачи.
	work, cancel := context.WithCancel(context.Background())
	defer cancel()
	// release разрешает закончить задержанный Close тела.
	release := make(chan struct{})
	// releaseOnce защищает однократное освобождение Close в сценарии и очистке.
	var releaseOnce sync.Once
	defer releaseOnce.Do(func() {
		// Освобождает задержанный Close при завершении теста.
		close(release)
	})
	// r содержит тело с задерживаемым Close для воспроизводимого окна принятия отмены.
	r := &transferReader{closed: make(chan struct{}), closeStarted: make(chan struct{}), close: func() error {
		// Задерживает Close до разрешения release, создавая окно для принятия отмены передачи.
		<-release
		return nil
	}}
	// u содержит отдельный протокол передачи с единственным закрытием тела.
	u := &uploadTransfer{reader: &ownedReader{ReadCloser: r}, done: make(chan struct{})}
	// j связывает отмену задания с проверяемым состоянием передачи.
	j := &Job{cancel: cancel, upload: u}
	// completed передаёт результат начавшегося успешного завершения передачи.
	completed := make(chan error, 1)
	go func() {
		// Начинает успешное завершение передачи, блокируясь на Close источника, и передаёт результат в
		// completed.
		completed <- u.complete(work, nil)
	}()
	transferAwait(t, ctx, r.closeStarted)
	// request задаёт заранее отменяемое ожидание передачи. cancelRequest отменяет его до вызова
	// WaitUpload.
	request, cancelRequest := context.WithCancel(ctx)
	cancelRequest()
	// waited передаёт результат ожидания с принятым запросом отмены.
	waited := make(chan error, 1)
	go func() {
		// Ожидает передачу с уже отменённым запросом и сообщает результат после завершения Close.
		waited <- j.WaitUpload(request)
	}()
	transferAwait(t, ctx, work.Done())
	select {
	// err содержит преждевременно возвращённый результат отмены до окончания Close.
	case err := <-waited:
		t.Fatalf("отмена вернулась до закрытия reader: %v", err)
	default:
	}
	releaseOnce.Do(func() {
		// Разрешает завершить Close после подтверждения отмены рабочего контекста.
		close(release)
	})
	// result выбирает канал завершения передачи или канал ожидания для проверки общей причины.
	for _, result := range []<-chan error{completed, waited} {
		select {
		// err содержит итог одного из конкурирующих путей завершения после Close.
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

// Проверяет ошибки пустого тела, переполнения, чтения, закрытия и каталога до инспекции и неизменность
// сохранённого результата передачи.
func TestWaitUploadRejectsEarlyFailuresAndKeepsItsResult(t *testing.T) {
	// mode выбирает ранний отказ пустого тела, размера, чтения, закрытия или каталога.
	for _, mode := range []string{"empty", "overflow", "read", "close", "directory"} {
		t.Run(mode, func(t *testing.T) {
			// Проверяет выбранную раннюю ошибку передачи, единственное закрытие и сохранность результата при
			// повторном WaitUpload без запуска проверки содержимого.

			// svc содержит тестовый доменный сервис. asset содержит Asset проверяемой
			// загрузки.
			svc, _, asset, _ := acceptanceManagerService(t)
			// files содержит изолированное файловое хранилище менеджера.
			files, _ := acceptanceFileStore(t)
			// ctx ограничивает сценарий пятью секундами. cancel освобождает этот срок.
			ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
			defer cancel()
			// c содержит конфигурацию, чей бюджет загрузки меняется в сценарии переполнения.
			c := acceptanceManagerConfig()
			// data содержит допустимое исходное PNG для базового сценария.
			data := acceptancePNG(t)
			// injected содержит управляемую ошибку чтения или закрытия тела.
			injected := errors.New("контролируемая ошибка передачи")
			// want содержит ожидаемую категорию раннего отказа передачи.
			want := injected
			// r содержит тело загрузки, поведение которого изменяется выбранным сценарием.
			r := &transferReader{closed: make(chan struct{}), read: bytes.NewReader(data).Read}
			switch mode {
			case "empty":
				r.read = bytes.NewReader(nil).Read
				want = core.ErrCorrupt
			case "overflow":
				c.MaxUploadBytes = uint64(len(data) - 1)
				want = ErrLimit
			case "read":
				r.read = func([]byte) (int, error) {
					// Возвращает injected при первом чтении, моделируя ошибку тела загрузки.
					return 0, injected
				}
			case "close":
				r.close = func() error {
					// Возвращает injected из Close, моделируя ошибку закрытия тела загрузки.
					return injected
				}
			case "directory":
				// err сообщает об отказе удаления каталога fallback при подготовке
				// сценария отсутствующего родителя.
				if err := files.root.Remove("fallback"); err != nil {
					t.Fatal(err)
				}
				want = os.ErrNotExist
			}
			// inspected атомарно считает недопустимые попытки инспекции после раннего отказа
			// передачи.
			var inspected atomic.Int32
			// inspect обнаруживает запуск инспекции после ошибки передачи и возвращает
			// контрольный отказ.
			inspect := transferInspection{source: func(context.Context, *os.File, core.AssetKind) (Source, error) {
				// Считает попытку проверки после ошибки передачи и возвращает ошибку, чтобы обнаружить недопустимое
				// продолжение подготовки.

				inspected.Add(1)
				return Source{}, errors.New("проверка после ошибки передачи")
			}}
			// m содержит менеджер текущего сценария раннего отказа.
			m := transferManager(t, svc, files, c, &inspect)
			// j содержит принятое задание. err сообщает об отказе его принятия.
			j, err := m.Upload(ctx, core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage}, r)
			if err != nil {
				t.Fatal(err)
			}
			// first сохраняет первоначальный результат WaitUpload для проверки идентичности
			// повторного результата.
			first := j.WaitUpload(ctx)
			if !errors.Is(first, want) {
				t.Fatalf("причина ошибки передачи потеряна: %v", first)
			}
			// result содержит итог задания после раннего отказа передачи.
			result, err := j.Wait(ctx)
			if err == nil || !result.Clean || inspected.Load() != 0 || r.closes.Load() != 1 {
				t.Fatalf("ошибка передачи не завершила работу: %+v, проверок=%d, закрытий=%d, ошибка=%v", result, inspected.Load(), r.closes.Load(), err)
			}
			// repeated содержит повторно прочитанный результат завершённой передачи.
			if repeated := j.WaitUpload(ctx); repeated != first {
				t.Fatalf("завершённая передача изменила результат: %v -> %v", first, repeated)
			}
		})
	}
}

// Проверяет, что отказ инспекции не переписывает успешную передачу и прежнее READY-поколение; задание
// без загрузки отвергает WaitUpload.
func TestWaitUploadSuccessSurvivesPreparationFailure(t *testing.T) {
	// svc содержит тестовый доменный сервис. asset содержит Asset проверяемой загрузки.
	svc, _, asset, _ := acceptanceManagerService(t)
	// files содержит изолированное файловое хранилище менеджера.
	files, _ := acceptanceFileStore(t)
	// ctx ограничивает сценарий пятью секундами. cancel освобождает этот срок.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// m содержит менеджер для проверки независимости успеха передачи от дальнейшего отказа
	// инспекции.
	m := transferManager(t, svc, files, acceptanceManagerConfig(), &acceptanceInspection{})
	// old содержит прежнее опубликованное поколение, которое должно сохраниться при отказе новой
	// подготовки.
	old := acceptanceUpload(t, m, svc, asset.ID, acceptancePNG(t))
	// a содержит текущее состояние Asset для нового запроса с верной ревизией. err сообщает об
	// отказе чтения.
	a, err := svc.GetAsset(ctx, asset.ID)
	if err != nil {
		t.Fatal(err)
	}
	// j содержит задание передачи некорректного изображения.
	j, err := m.Upload(ctx, core.PreparationRequest{AssetID: a.ID, ExpectedRevision: a.Revision, Kind: core.AssetImage}, io.NopCloser(bytes.NewReader([]byte("invalid-image"))))
	if err != nil {
		t.Fatal(err)
	}
	if err = j.WaitUpload(ctx); err != nil {
		t.Fatal(err)
	}
	// result содержит отказ подготовки после успешно сохранённой передачи.
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
