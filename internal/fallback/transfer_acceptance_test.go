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

// transferAcceptanceReader считает чтения тела и позволяет задержать закрытие или вернуть ошибку.
type transferAcceptanceReader struct {
	// Reader содержит вложенный источник байтов загрузки.
	io.Reader
	// closed закрывается при первом входе в Close до возможного ожидания closeGate.
	closed chan struct{}
	// closeGate при наличии задерживает возврат Close до разрешения теста.
	closeGate <-chan struct{}
	// closeErr задаёт возвращаемую ошибку закрытия тела.
	closeErr error
	// reads атомарно считает вызовы чтения тела.
	reads atomic.Int32
	// once защищает единственный сигнал начала закрытия.
	once sync.Once
}

// Read учитывает обращения и читает вложенный источник данных.
func (r *transferAcceptanceReader) Read(p []byte) (int, error) {
	r.reads.Add(1)
	return r.Reader.Read(p)
}

// Close единожды сообщает о начале закрытия, ждёт управляемый барьер при его наличии и возвращает
// заданную ошибку.
func (r *transferAcceptanceReader) Close() error {
	r.once.Do(func() {
		// Единожды сообщает через closed о начале Close тестового источника.
		close(r.closed)
	})
	if r.closeGate != nil {
		<-r.closeGate
	}
	return r.closeErr
}

// transferAcceptanceInspection задерживает инспекцию исходника и позволяет подставить ошибку подготовки.
type transferAcceptanceInspection struct {
	// entered закрывается при первом входе в проверку источника.
	entered chan struct{}
	// release разрешает продолжить проверку источника.
	release <-chan struct{}
	// err при наличии задаёт отказ после снятия задержки.
	err error
	// once защищает единственный сигнал входа в инспекцию.
	once sync.Once
}

// Source сообщает о входе, ждёт разрешение либо отмену и затем возвращает управляемую ошибку или
// проверяет фактический источник.
func (i *transferAcceptanceInspection) Source(ctx context.Context, f *os.File, kind core.AssetKind) (Source, error) {
	i.once.Do(func() {
		// Единожды сообщает о входе в проверку источника перед ожиданием разрешения или отмены.
		close(i.entered)
	})
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

// Verify возвращает формат профиля без анализа файла для проверки протокола передачи.
func (*transferAcceptanceInspection) Verify(_ context.Context, _ *os.File, p core.GroupProfile) (core.MediaFormat, error) {
	return p.Format, nil
}

// Создаёт менеджер с управляемыми инструментами и регистрирует ограниченное закрытие после теста.
func transferAcceptanceManager(t *testing.T, svc *core.Service, files *FileStore, inspect Inspection, config Config) *Manager {
	t.Helper()
	// m содержит менеджер с управляемыми инструментами передачи. err сообщает об отказе его
	// создания.
	m, err := NewManagerWithTools(svc, files, config, Tools{Builder: func(GenerateRequest) (Command, error) {
		// Возвращает условную команду controlled для проверки передачи файла без кодировщика.
		return Command{Binary: "controlled"}, nil
	}, Inspector: inspect, Runner: acceptanceRunFunc(func(_ context.Context, _ Command, _ *os.File, out io.Writer) (RunResult, error) {
		// Записывает условный проверенный вариант в выход и возвращает ошибку записи.

		// err сообщает об отказе записи условных байтов варианта.
		_, err := out.Write([]byte("verified-variant"))
		return RunResult{}, err
	})})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает Manager с двухсекундным сроком после сценария, игнорируя ошибку.

		// ctx ограничивает закрытие менеджера двумя секундами. cancel освобождает срок.
		ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		_ = m.Close(ctx)
	})
	return m
}

// Ожидает контрольный канал не более трёх секунд и завершает тест при отсутствии сигнала.
func transferAcceptanceWait(t *testing.T, ch <-chan struct{}) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatal("не получено подтверждение этапа передачи")
	}
}

// Создаёт барьер с повторно безопасной функцией освобождения, вызываемой также при завершении теста.
func transferAcceptanceGate(t *testing.T) (<-chan struct{}, func()) {
	t.Helper()
	// gate содержит барьер продолжения, закрываемый однократным разрешением.
	gate := make(chan struct{})
	// once защищает gate от повторного закрытия.
	var once sync.Once
	// release однократно открывает барьер для ожидающих и регистрируется в очистке теста.
	release := func() {
		// Разрешает ожидающим продолжить работу, выполняя закрытие gate через sync.Once.

		once.Do(func() {
			// Закрывает gate при первом вызове разрешения.
			close(gate)
		})
	}
	t.Cleanup(release)
	return gate, release
}

// Проверяет подтверждение закрытой загрузки до полной подготовки и успешную публикацию после поздней
// отмены запроса.
func TestAcceptanceUploadCompletesBeforeInspectionAndSurvivesRequestCancellation(t *testing.T) {
	// svc содержит тестовый доменный сервис. asset содержит Asset проверяемой загрузки.
	svc, _, asset, _ := acceptanceManagerService(t)
	// files содержит изолированное файловое хранилище менеджера.
	files, _ := acceptanceFileStore(t)
	// release задерживает инспекцию. finish разрешает ей продолжить работу.
	release, finish := transferAcceptanceGate(t)
	// inspect содержит инспекцию с контролируемым входом и продолжением.
	inspect := &transferAcceptanceInspection{entered: make(chan struct{}), release: release}
	// m содержит менеджер для проверки независимости фоновой подготовки от запроса передачи.
	m := transferAcceptanceManager(t, svc, files, inspect, acceptanceManagerConfig())
	// reader содержит тело допустимого PNG с сигналом закрытия.
	reader := &transferAcceptanceReader{Reader: bytes.NewReader(acceptancePNG(t)), closed: make(chan struct{})}
	// job содержит принятое задание. err сообщает об отказе принятия.
	job, err := m.Upload(context.Background(), core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage}, reader)
	if err != nil {
		t.Fatal(err)
	}
	// ctx ограничивает ожидание передачи тремя секундами. cancel затем моделирует позднюю отмену
	// запроса.
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
	// wait ограничивает независимое ожидание фоновой подготовки тремя секундами. stop освобождает
	// срок.
	wait, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	// result содержит итог фоновой публикации после поздней отмены запроса.
	result, err := job.Wait(wait)
	if err != nil || !result.Clean || result.Generation.State != core.PreparationPublished {
		t.Fatalf("фоновая подготовка не опубликована: %+v %v", result, err)
	}
	// current содержит Asset после публикации для проверки выбранного поколения.
	current, err := svc.GetAsset(wait, asset.ID)
	if err != nil || current.CurrentGenerationID != result.Generation.ID {
		t.Fatalf("публикация не стала current: %+v %v", current, err)
	}
}

// Проверяет ожидание завершения Close и сохранение успеха передачи после отмены задания во время
// управляемой инспекции.
func TestAcceptanceUploadWaitsForReaderCloseAndPreservesSuccessfulTransferOnPreparationFailure(t *testing.T) {
	// svc содержит тестовый доменный сервис. asset содержит Asset проверяемой загрузки.
	svc, _, asset, _ := acceptanceManagerService(t)
	// files содержит изолированное файловое хранилище менеджера.
	files, _ := acceptanceFileStore(t)
	// closeGate задерживает завершение Close тела. finishClose разрешает его возврат.
	closeGate, finishClose := transferAcceptanceGate(t)
	// release задерживает инспекцию. finish освобождает её при завершении теста.
	release, finish := transferAcceptanceGate(t)
	// inspect содержит задерживаемую проверку с управляемой ошибкой содержимого.
	inspect := &transferAcceptanceInspection{entered: make(chan struct{}), release: release, err: core.ErrCorrupt}
	// m содержит менеджер для проверки ожидания Close и сохранности успеха передачи.
	m := transferAcceptanceManager(t, svc, files, inspect, acceptanceManagerConfig())
	// reader содержит PNG с управляемой задержкой Close.
	reader := &transferAcceptanceReader{Reader: bytes.NewReader(acceptancePNG(t)), closed: make(chan struct{}), closeGate: closeGate}
	// job содержит принятое задание. err сообщает об отказе принятия.
	job, err := m.Upload(context.Background(), core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage}, reader)
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	transferAcceptanceWait(t, reader.closed)
	// ctx ограничивает ожидание передачи и отмены задания тремя секундами. cancel освобождает срок.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// completed передаёт результат WaitUpload из отдельной горутины.
	completed := make(chan error, 1)
	go func() {
		// Ожидает результат передачи в отдельной горутине, позволяя проверить, что WaitUpload ждёт завершения
		// Close.
		completed <- job.WaitUpload(ctx)
	}()
	select {
	// err содержит преждевременно возвращённый результат WaitUpload до освобождения Close.
	case err := <-completed:
		t.Fatalf("успех до завершения Close: %v", err)
	default:
	}
	finishClose()
	if err = <-completed; err != nil {
		t.Fatal(err)
	}
	transferAcceptanceWait(t, inspect.entered)
	// Отмена подготовки не меняет уже подтверждённую передачу файла.
	job.Cancel()
	if _, err = job.Wait(ctx); err == nil {
		t.Fatal("ошибка подготовки потеряна")
	}
	if err = job.WaitUpload(ctx); err != nil {
		t.Fatalf("ошибка подготовки переписала результат передачи: %v", err)
	}
}

// Проверяет закрытие непрочитанного тела при отмене ожидающей загрузки и отсутствие ожидающего и
// текущего поколения у её Asset.
func TestAcceptanceQueuedUploadCancellationClosesUnreadReaderAndLeavesNoPending(t *testing.T) {
	// svc содержит тестовый доменный сервис. asset содержит Asset проверяемой загрузки.
	svc, _, asset, _ := acceptanceManagerService(t)
	// files содержит изолированное файловое хранилище менеджера.
	files, _ := acceptanceFileStore(t)
	// release задерживает инспекцию первой подготовки, занимающей слот. finish освобождает этот
	// барьер.
	release, finish := transferAcceptanceGate(t)
	defer finish()
	// inspect содержит инспекцию, удерживающую первую подготовку активной.
	inspect := &transferAcceptanceInspection{entered: make(chan struct{}), release: release}
	// m содержит менеджер с занятым слотом подготовки.
	m := transferAcceptanceManager(t, svc, files, inspect, acceptanceManagerConfig())
	// first содержит первое активное задание. err сообщает об отказе его принятия.
	first, err := m.Upload(context.Background(), core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage}, io.NopCloser(bytes.NewReader(acceptancePNG(t))))
	if err != nil {
		t.Fatal(err)
	}
	defer first.Cancel()
	transferAcceptanceWait(t, inspect.entered)
	// queuedAsset содержит отдельный Asset для ожидающей загрузки.
	queuedAsset, err := svc.CreateAsset(context.Background(), "Ожидающий файл")
	if err != nil {
		t.Fatal(err)
	}
	// reader содержит тело ожидающей загрузки, которое не должно быть прочитано до захвата слота.
	reader := &transferAcceptanceReader{Reader: bytes.NewReader(acceptancePNG(t)), closed: make(chan struct{})}
	// queued содержит задание в очереди за активной подготовкой.
	queued, err := m.Upload(context.Background(), core.PreparationRequest{AssetID: queuedAsset.ID, ExpectedRevision: queuedAsset.Revision, Kind: core.AssetImage}, reader)
	if err != nil {
		t.Fatal(err)
	}
	// ctx задаёт заранее отменяемое ожидание передачи. cancel отменяет его до WaitUpload.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if err = queued.WaitUpload(ctx); !errors.Is(err, context.Canceled) {
		t.Fatalf("отмена очереди не сохранена: %v", err)
	}
	transferAcceptanceWait(t, reader.closed)
	if reader.reads.Load() != 0 {
		t.Fatal("ожидающий reader прочитан до claim")
	}
	// wait ограничивает ожидание отменённого задания тремя секундами. stop освобождает срок.
	wait, stop := context.WithTimeout(context.Background(), 3*time.Second)
	defer stop()
	// result содержит итог отменённого задания с подтверждением очистки.
	result, err := queued.Wait(wait)
	if !errors.Is(err, context.Canceled) || !result.Clean {
		t.Fatalf("очередь не завершила отмену: %+v %v", result, err)
	}
	// stored содержит состояние Asset после отмены ожидающей загрузки.
	stored, err := svc.GetAsset(wait, queuedAsset.ID)
	if err != nil || stored.PendingGenerationID != "" || stored.CurrentGenerationID != "" {
		t.Fatalf("отмена оставила pending/current: %+v %v", stored, err)
	}
}

// Проверяет отказ передачи и публикации для пустого или чрезмерного тела и ошибки Close без
// оставшегося ожидающего поколения.
func TestAcceptanceUploadTransferRejectsEmptyOversizedAndCloseFailure(t *testing.T) {
	// tc содержит недопустимое тело или ошибку Close выбранного сценария.
	for _, tc := range []struct {
		// name обозначает ранний отказ в имени подтеста.
		name string
		// data содержит передаваемые байты тела, включая пустое или слишком большое.
		data []byte
		// closeErr задаёт управляемый отказ закрытия тела.
		closeErr error
	}{{"empty", nil, nil}, {"oversized", bytes.Repeat([]byte("x"), 17), nil}, {"close-error", []byte("payload"), io.ErrClosedPipe}} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет отказ передачи пустого или превышающего 16 байт тела либо ошибку Close, требуя отсутствия
			// current и pending после завершения.

			// svc содержит тестовый доменный сервис. asset содержит Asset проверяемой
			// загрузки.
			svc, _, asset, _ := acceptanceManagerService(t)
			// files содержит изолированное файловое хранилище менеджера.
			files, _ := acceptanceFileStore(t)
			// config содержит конфигурацию с шестнадцатибайтовым пределом загрузки.
			config := acceptanceManagerConfig()
			config.MaxUploadBytes = 16
			// m содержит менеджер для проверки раннего отказа передачи.
			m := transferAcceptanceManager(t, svc, files, &acceptanceInspection{}, config)
			// reader содержит выбранные байты и управляемую ошибку Close.
			reader := &transferAcceptanceReader{Reader: bytes.NewReader(tc.data), closed: make(chan struct{}), closeErr: tc.closeErr}
			// job содержит принятое задание. err сообщает об отказе принятия.
			job, err := m.Upload(context.Background(), core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage}, reader)
			if err != nil {
				t.Fatal(err)
			}
			// ctx ограничивает ожидание передачи и задания тремя секундами. cancel
			// освобождает срок.
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if err = job.WaitUpload(ctx); err == nil {
				t.Fatal("некорректная передача подтверждена")
			}
			transferAcceptanceWait(t, reader.closed)
			if _, err = job.Wait(ctx); err == nil {
				t.Fatal("некорректный файл опубликован")
			}
			// stored содержит Asset после неуспешной передачи для проверки отсутствия
			// current и pending.
			stored, err := svc.GetAsset(ctx, asset.ID)
			if err != nil || stored.CurrentGenerationID != "" || stored.PendingGenerationID != "" {
				t.Fatalf("неудача изменила current или оставила pending: %+v %v", stored, err)
			}
		})
	}
}
