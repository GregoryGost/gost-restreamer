package ffmpeg

import (
	"context"
	"crypto/sha256"
	"errors"
	"fmt"
	"io"
	"net"
	"os"
	"os/exec"
	"sync"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/processowner"
)

// Supervisor владеет одним логическим работником и всеми его последовательными попытками.
type Supervisor struct {
	// config хранит проверенные сроки и бюджеты логического работника.
	config Config
	// queue хранит ограниченную диагностику всех физических попыток.
	queue *diagnosticQueue
	// mu защищает запуск, остановку, функцию отмены и итог работника.
	mu sync.Mutex
	// started отмечает единожды принятый запуск либо остановку ещё не запущенного супервизора.
	started bool
	// stopping запрещает новые запуски и повторные попытки.
	stopping bool
	// cancel отменяет контекст логического работника после принятого Start.
	cancel context.CancelFunc
	// done закрывается после завершения работника и попытки освобождения его ресурсов.
	done chan struct{}
	// result хранит окончательный исход выполнения и очистки после закрытия done.
	result Result
	// progress хранит последнее наблюдение FFmpeg текущей физической попытки.
	progress progressStore
}

// New проверяет настройки и создаёт Supervisor с ограниченной очередью диагностики без запуска
// процесса.
func New(config Config) (*Supervisor, error) {
	// err сообщает об отказе проверки конфигурации до создания Supervisor.
	if err := config.Validate(); err != nil {
		return nil, err
	}
	return &Supervisor{config: config, queue: newDiagnosticQueue(config.DiagnosticEvents, config.DiagnosticBytes), done: make(chan struct{})}, nil
}

// Start регистрирует учитываемую работу под Owner.Do, затем запускает её вне общего допуска.
// ctx задаёт время жизни работника независимо от публикации OBS.
func (s *Supervisor) Start(ctx context.Context, w Worker) error {
	if w.Owner == nil || w.Command == nil || (w.Topology == Duplex && w.Input == nil) || w.Topology > FileInput || w.Output == nil || w.SessionID == "" || w.GroupID == "" || len(w.SessionID) > 256 || len(w.GroupID) > 256 {
		return fmt.Errorf("%w: неполный Worker", ErrInvalidSupervisor)
	}
	// runCtx содержит контекст зарегистрированного логического работника для запуска вне допуска
	// владельца.
	var runCtx context.Context
	// registered показывает, зарегистрирован ли запуск под допуском Owner и требуется ли запустить
	// run.
	registered := false
	// err содержит результат регистрации запуска под общим допуском владельца.
	err := w.Owner.Do(ctx, func() error {
		// Под допуском владельца единожды регистрирует запуск и создаёт контекст работника; запрет запуска,
		// повторный Start или отменённый ctx возвращают ошибку.

		s.mu.Lock()
		defer s.mu.Unlock()
		if s.stopping {
			return ErrStopped
		}
		if s.started {
			return ErrAlreadyStarted
		}
		// err содержит причину отмены входного контекста до регистрации запуска.
		if err := ctx.Err(); err != nil {
			return err
		}
		s.started = true
		s.queue.setOwner(w.SessionID, w.GroupID)
		runCtx, s.cancel = context.WithCancel(ctx)
		registered = true
		return nil
	})
	if registered {
		go s.run(runCtx, w)
	}
	return err
}

// Done закрывается после завершения операций супервизора и попытки очистки.
// Результат очистки передаётся через Result.Clean и Result.CleanupErr.
func (s *Supervisor) Done() <-chan struct{} { return s.done }

// NextDiagnostic возвращает следующее полное событие с ожиданием контекста; после закрытия очереди
// дочитывает накопленное до EOF.
func (s *Supervisor) NextDiagnostic(ctx context.Context) (Diagnostic, error) {
	return s.queue.next(ctx)
}

// DroppedDiagnostics возвращает число событий, отвергнутых очередью из-за закрытия или предела
// хранения.
func (s *Supervisor) DroppedDiagnostics() uint64 { return s.queue.losses() }

// Snapshot копирует последнее полное наблюдение текущей физической попытки.
func (s *Supervisor) Snapshot() ProgressSnapshot {
	// snapshot содержит копию отчёта прогресса, дополненную текущим числом потерь диагностики.
	snapshot := s.progress.snapshot()
	snapshot.DroppedDiagnostics = s.queue.losses()
	return snapshot
}

// Stop сначала запрещает новые попытки, затем ожидает физического завершения.
// По истечении срока Done остаётся наблюдаемым; повторный Stop может дождаться очистки.
func (s *Supervisor) Stop(ctx context.Context) error {
	s.mu.Lock()
	s.stopping = true
	if !s.started {
		s.started = true
		s.result.Clean = true
		s.queue.close()
		close(s.done)
	} else if s.cancel != nil {
		s.cancel()
	}
	s.mu.Unlock()
	// waitCtx ограничивает ожидание остановки сроком ShutdownTimeout; cancel освобождает связанный с
	// ожиданием таймер.
	waitCtx, cancel := context.WithTimeout(ctx, s.config.ShutdownTimeout)
	defer cancel()
	// result содержит итог остановленного работника; err сообщает об истечении или отмене ожидания
	// его завершения.
	result, err := s.Wait(waitCtx)
	if err != nil {
		return errors.Join(ErrCleanupPending, err)
	}
	return result.CleanupErr
}

// Wait возвращает исход работника отдельно от ошибок ожидания и физической очистки.
func (s *Supervisor) Wait(ctx context.Context) (Result, error) {
	select {
	case <-s.done:
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.result, nil
	default:
	}
	select {
	case <-ctx.Done():
		return Result{}, ctx.Err()
	case <-s.done:
		s.mu.Lock()
		defer s.mu.Unlock()
		return s.result, nil
	}
}

// Владеет последовательными попытками и наблюдением отзыва Owner, передаёт итог каждой попытки до
// решения о повторе и прекращает повторы при неполной очистке.
func (s *Supervisor) run(ctx context.Context, w Worker) {
	// Наблюдатель отзывает работника даже во время внешнего CommandBuilder/Start.
	// observerDone прекращает наблюдение за отзывом владельца после окончания цикла попыток.
	observerDone := make(chan struct{})
	// observerJoined подтверждает завершение горутины наблюдения за владельцем.
	observerJoined := make(chan struct{})
	go func() {
		// Наблюдает отзыв владельца и отменяет работника; при обычном завершении выходит по observerDone и
		// подтверждает выход через observerJoined.

		defer close(observerJoined)
		select {
		case <-w.Owner.Done():
			s.mu.Lock()
			s.cancel()
			s.mu.Unlock()
		case <-observerDone:
		}
	}()
	// result накапливает число попыток, итог выполнения и ошибки физической очистки работника.
	result := Result{}
	for {
		// err содержит причину отмены контекста логического работника перед следующей попыткой.
		if err := ctx.Err(); err != nil {
			result.Err = err
			break
		}
		// err содержит результат проверки допуска владельца и запрета повторов перед попыткой.
		err := w.Owner.Do(ctx, func() error {
			// Под допуском владельца проверяет запрет новых попыток и отмену контекста перед очередным запуском.

			s.mu.Lock()
			defer s.mu.Unlock()
			if s.stopping {
				return ErrStopped
			}
			return ctx.Err()
		})
		if err != nil {
			result.Err = err
			break
		}
		result.Attempts++
		// attemptErr содержит исход выполнения физической попытки; cleanupErr содержит отдельный
		// результат её очистки.
		attemptErr, cleanupErr := s.runAttempt(ctx, w, result.Attempts)
		result.Err, result.CleanupErr = attemptErr, errors.Join(result.CleanupErr, cleanupErr)
		if w.OnAttemptResult != nil {
			w.OnAttemptResult(AttemptResult{ID: result.Attempts, Err: attemptErr, CleanupErr: cleanupErr, Stopped: ctx.Err() != nil || !w.Owner.Valid()})
		}
		if cleanupErr != nil || attemptErr == nil || ctx.Err() != nil || !w.Owner.Valid() || result.Attempts > uint64(s.config.MaxRestarts) {
			break
		}
		// timer отсчитывает паузу до следующей разрешённой попытки.
		timer := time.NewTimer(s.config.RestartBackoff)
		select {
		case <-ctx.Done():
			timer.Stop()
			result.Err = ctx.Err()
		case <-timer.C:
			continue
		}
		break
	}
	close(observerDone)
	<-observerJoined
	if !w.Owner.Valid() {
		result.Err = errors.Join(result.Err, ErrOwnerRevoked)
	}
	result.Clean = result.CleanupErr == nil
	s.mu.Lock()
	s.stopping = true
	s.result = result
	s.cancel()
	s.queue.close()
	close(s.done)
	s.mu.Unlock()
}

// handlerResult передаёт направление и исход завершившегося UDS-обработчика владельцу попытки.
type handlerResult struct {
	// output отличает результат выходного обработчика от результата входного.
	output bool
	// err содержит ошибку приёма, выполнения или закрытия соединения данного направления.
	err error
}

// packetProgress хранит последние возрастающие DTS и время их получения отдельно для видео и аудио.
type packetProgress struct {
	// mu защищает проверку готовности и обновление продвижения обеих дорожек.
	mu sync.Mutex
	// seen отмечает получение первого проверенного пакета видео и аудио.
	seen [2]bool
	// dts хранит последний принятый DTS каждой дорожки в её неизменной шкале.
	dts [2]int64
	// at хранит время последнего принятого продвижения каждой дорожки.
	at [2]time.Time
}

// Принимает только строго возрастающий DTS допустимой дорожки и обновляет время её прогресса под
// блокировкой.
func (p *packetProgress) report(track Track, dts int64) bool {
	if track != Video && track != Audio {
		return false
	}
	// i обозначает позицию видео или аудио в массивах контроля прогресса.
	i := int(track - 1)
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.seen[i] && dts <= p.dts[i] {
		return false
	}
	p.seen[i], p.dts[i], p.at[i] = true, dts, time.Now()
	return true
}

// До первых пакетов обеих дорожек проверяет срок запуска, затем отдельные сроки продвижения видео и
// аудио.
func (p *packetProgress) health(now, start time.Time, c Config) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.seen[0] || !p.seen[1] {
		if now.Sub(start) >= c.StartupTimeout {
			return ErrStartupTimeout
		}
		return nil
	}
	if now.Sub(p.at[0]) >= c.VideoTimeout || now.Sub(p.at[1]) >= c.AudioTimeout {
		return ErrProgressTimeout
	}
	return nil
}

// Владеет пространством UDS, обработчиками и одним процессом до их завершения, включая поздний
// построитель команды. Ошибки физической очистки возвращаются отдельно, а неподтверждённое завершение
// сохраняет пути.
func (s *Supervisor) runAttempt(parent context.Context, w Worker, id uint64) (retErr, cleanupErr error) {
	s.progress.begin(id)
	defer func() {
		// Отмечает очистку как неподтверждённую при результате ErrCleanupPending, ErrPending или ErrUnknown.

		if errors.Is(retErr, ErrCleanupPending) || errors.Is(retErr, processowner.ErrPending) || errors.Is(retErr, processowner.ErrUnknown) {
			cleanupErr = errors.Join(cleanupErr, ErrCleanupPending)
		}
	}()
	// ctx ограничивает текущую попытку; cancelCause отменяет её с конкретной причиной отказа или
	// остановки.
	ctx, cancelCause := context.WithCancelCause(parent)
	// cancel отменяет текущую попытку с причиной context.Canceled при завершении её операций.
	cancel := func() {
		// Отменяет попытку с причиной context.Canceled.
		cancelCause(context.Canceled)
	}
	defer cancel()
	if w.ProcessScope != nil {
		// Ticket учитывает и задержавшийся построитель команды; Complete вызывается
		// после завершения операций попытки.
		// ticket учитывает операции попытки, включая построение команды; err сообщает об отказе
		// регистрации в ProcessScope.
		ticket, err := w.ProcessScope.Register(ctx, "ffmpeg-attempt")
		if err != nil {
			return err, nil
		}
		defer func() {
			// Завершает зарегистрированную операцию ffmpeg-attempt с отдельным сроком и присоединяет ошибку к
			// cleanupErr.

			// complete ограничивает запись завершения операции отдельным сроком; stop
			// освобождает таймер этого ожидания.
			complete, stop := context.WithTimeout(context.Background(), s.config.ShutdownTimeout)
			defer stop()
			cleanupErr = errors.Join(cleanupErr, ticket.Complete(complete))
		}()
	}
	// sessionHash содержит короткий шестнадцатеричный хеш SessionID для безопасного сегмента
	// неуправляемого пути.
	sessionHash := fmt.Sprintf("%x", sha256.Sum256([]byte(w.SessionID)))[:16]
	// n хранит пространство имён и UDS текущей физической попытки.
	var n *Namespace
	// err переносит ошибки создания пространства имён, построения команды, запуска процесса и
	// контроля попытки.
	var err error
	if w.ProcessScope != nil {
		n, err = newManagedNamespace(ctx, w.ProcessScope, w.Topology == FileInput)
	} else {
		n, err = newNamespace(s.config.BaseDir, sessionHash, w.Topology == FileInput)
	}
	if err != nil {
		// partial раскрывает отказ частичного создания Namespace; ok показывает наличие
		// отдельной ошибки очистки.
		if partial, ok := errors.AsType[*namespaceCreationError](err); ok {
			return err, partial.cleanup
		}
		return err, nil
	}
	// shutdownDone подтверждает завершение закрытия дескрипторов Namespace после отмены попытки.
	shutdownDone := make(chan struct{})
	// shutdownErr хранит результат асинхронного Shutdown до его присоединения к очистке.
	var shutdownErr error
	// stopShutdown отменяет ещё не начавшийся обработчик закрытия Namespace; запущенный обработчик
	// ожидается через shutdownDone.
	stopShutdown := context.AfterFunc(ctx, func() {
		// При отмене попытки закрывает дескрипторы Namespace, сохраняет ошибку и сообщает через shutdownDone о
		// завершении.
		shutdownErr = n.Shutdown()
		close(shutdownDone)
	})
	// ioWG учитывает выполнение входного и выходного обработчиков UDS.
	var ioWG sync.WaitGroup
	// ioResults принимает по одному результату каждого направления без ожидания читателя.
	ioResults := make(chan handlerResult, 2)
	// handlerErrors сохраняет ошибки обоих направлений для сбора после завершения обработчиков.
	var handlerErrors [2]error
	// progress хранит проверенные продвижения выходных DTS для готовности и контроля зависания.
	progress := &packetProgress{}
	// startupJoined подтверждает завершение обработчика срока запуска, если таймер уже сработал.
	startupJoined := make(chan struct{})
	// startup ограничивает ожидание первых пакетов обеих выходных дорожек и отменяет неготовую
	// попытку.
	startup := time.AfterFunc(s.config.StartupTimeout, func() {
		// По истечении StartupTimeout отменяет попытку, если прогресс обеих выходных дорожек ещё не получен.

		defer close(startupJoined)
		progress.mu.Lock()
		// ready показывает, получены ли первые проверенные пакеты обеих дорожек к срабатыванию
		// таймера запуска.
		ready := progress.seen[0] && progress.seen[1]
		progress.mu.Unlock()
		if !ready {
			cancelCause(ErrStartupTimeout)
		}
	})
	defer func() {
		// Останавливает таймер запуска или ждёт завершения уже выполняемого обработчика тайм-аута.

		if !startup.Stop() {
			<-startupJoined
		}
	}()
	// handler принимает одно UDS-соединение, выполняет переданный обработчик и сохраняет исход
	// закрытия направления.
	handler := func(accept func() (net.Conn, error), fn IOHandler, report Progress, output bool) {
		// Принимает одно направление UDS, выполняет его обработчик и закрывает соединение; сохраняет ошибки
		// направления и передаёт результат владельцу попытки.

		defer ioWG.Done()
		// c содержит соединение выбранного направления; acceptErr накапливает отказ приёма,
		// обработки и закрытия этого соединения.
		c, acceptErr := accept()
		if acceptErr == nil {
			acceptErr = fn(ctx, c, report)
			// closeErr содержит ошибку закрытия соединения после возврата обработчика.
			closeErr := c.Close()
			if closeErr != nil && !errors.Is(closeErr, net.ErrClosed) {
				acceptErr = errors.Join(acceptErr, closeErr)
			}
		}
		// index выбирает ячейку ошибки входного либо выходного направления.
		index := 0
		if output {
			index = 1
		}
		handlerErrors[index] = acceptErr
		ioResults <- handlerResult{output: output, err: acceptErr}
	}
	if w.Topology == Duplex {
		ioWG.Add(1)
		go handler(n.AcceptInput, w.Input, func(Track, int64) bool {
			// Отклоняет отчёты входного обработчика: готовность определяется только выходными дорожками.
			return false
		}, false)
	}
	ioWG.Add(1)
	go handler(n.AcceptOutput, w.Output, func(t Track, dts int64) bool {
		// Принимает продвижение DTS выходной дорожки, пока контекст попытки не отменён.

		if ctx.Err() != nil {
			return false
		}
		return progress.report(t, dts)
	}, true)
	defer func() {
		// Отменяет попытку, завершает Shutdown и обработчики UDS, собирает их ошибки и закрывает Namespace;
		// при неподтверждённой очистке сохраняет управляемые пути.

		cancel()
		if stopShutdown() {
			cleanupErr = errors.Join(cleanupErr, n.Shutdown())
		} else {
			<-shutdownDone
			cleanupErr = errors.Join(cleanupErr, shutdownErr)
		}
		ioWG.Wait()
		// handlerErr содержит очередную сохранённую ошибку направления для присоединения после
		// завершения UDS-обработчиков.
		for _, handlerErr := range handlerErrors {
			if handlerErr != nil && !errors.Is(handlerErr, io.EOF) && !errors.Is(handlerErr, context.Canceled) && !errors.Is(handlerErr, net.ErrClosed) {
				retErr = errors.Join(retErr, handlerErr)
			}
		}
		if errors.Is(retErr, ErrCleanupPending) || errors.Is(retErr, processowner.ErrPending) || errors.Is(retErr, processowner.ErrUnknown) {
			n.keepPaths = true
		}
		cleanupErr = errors.Join(cleanupErr, n.Close())
	}()
	if ctx.Err() != nil {
		return context.Cause(ctx), nil
	}
	// command содержит команду и дескрипторы, возвращённые построителем для текущей попытки.
	command, err := w.Command(ctx, Attempt{ID: id, InputPath: n.InputPath, OutputPath: n.OutputPath})
	defer func() {
		// Закрывает все ненулевые ExtraFiles, возвращённые построителем, в том числе вместе с его ошибкой, и
		// присоединяет ошибки к cleanupErr.

		// f содержит очередной дескриптор ExtraFiles, закрываемый после завершения операций
		// попытки.
		for _, f := range command.ExtraFiles {
			if f != nil {
				cleanupErr = errors.Join(cleanupErr, f.Close())
			}
		}
	}()
	if err != nil {
		return errors.Join(err, context.Cause(ctx)), nil
	}
	if ctx.Err() != nil {
		return context.Cause(ctx), nil
	}
	if (w.Topology == FileInput && len(command.ExtraFiles) != 1) || (w.Topology == Duplex && len(command.ExtraFiles) != 0) {
		return fmt.Errorf("%w: несогласованное владение ExtraFiles", ErrInvalidSupervisor), nil
	}
	if w.Topology == FileInput {
		// file содержит единственный входной дескриптор MP4 в режиме FileInput.
		file := command.ExtraFiles[0]
		if file == nil {
			return fmt.Errorf("%w: отсутствует дескриптор файла", ErrInvalidSupervisor), nil
		}
		// info содержит метаданные входного дескриптора для проверки обычного файла; err
		// сообщает об отказе их чтения.
		info, err := file.Stat()
		if err != nil {
			return err, nil
		}
		if !info.Mode().IsRegular() {
			return fmt.Errorf("%w: требуется обычный файл", ErrInvalidSupervisor), nil
		}
		if _, err = file.Seek(0, io.SeekStart); err != nil {
			return err, nil
		}
	}
	if len(command.Args) > s.config.MaxCommandArgs || !validCommand(command.Binary, command.Args, s.config.MaxCommandBytes) {
		return fmt.Errorf("%w: команда превышает бюджет", ErrInvalidSupervisor), nil
	}
	// argv копируются до передачи процессу и диагностики.
	command.Args = append([]string(nil), command.Args...)
	s.queue.push(Diagnostic{Time: time.Now(), Attempt: id, Kind: "command", Args: append([]string{command.Binary}, command.Args...)})
	// processDone передаёт исход единственного ожидания запущенного процесса.
	processDone, err := s.startProcess(ctx, w.ProcessScope, command, id)
	if err != nil {
		return err, nil
	}
	// ticker задаёт период проверки продвижения выходных дорожек.
	ticker := time.NewTicker(s.config.WatchdogInterval)
	defer ticker.Stop()
	// start фиксирует начало контроля процесса для проверки срока начальной готовности.
	start := time.Now()
	// reason сохраняет причину прекращения работающей попытки до присоединения результата процесса.
	var reason error
	// outputFinished отмечает завершение выходного обработчика для дочитывания хвоста после Wait.
	outputFinished := false
selectLoop:
	for {
		select {
		// drain ограничивает дочитывание выходного соединения после успешного завершения
		// процесса.
		// ready показывает наличие первых пакетов обеих дорожек после завершения процесса и
		// дочитывания выхода.
		case err = <-processDone:
			if ctx.Err() != nil {
				return errors.Join(context.Cause(ctx), err), nil
			}
			if err != nil {
				progress.mu.Lock()
				// ready показывает наличие первых пакетов обеих дорожек при ошибочном
				// завершении процесса.
				ready := progress.seen[0] && progress.seen[1]
				progress.mu.Unlock()
				if !ready {
					return errors.Join(ErrPrematureExit, err), nil
				}
				return err, nil
			}
			// Wait не доказывает, что получатель уже прочитал переданный сокету хвост.
			drain := time.NewTimer(s.config.ShutdownTimeout)
			for !outputFinished {
				select {
				// result содержит очередной исход UDS-обработчика во время дочитывания
				// выхода.
				case result := <-ioResults:
					if result.output {
						outputFinished = true
					}
					if result.err != nil && !errors.Is(result.err, io.EOF) && result.output {
						drain.Stop()
						return result.err, nil
					}
				case <-ctx.Done():
					drain.Stop()
					return context.Cause(ctx), nil
				case <-drain.C:
					return ErrOutputDrainTimeout, nil
				}
			}
			drain.Stop()
			progress.mu.Lock()
			ready := progress.seen[0] && progress.seen[1]
			progress.mu.Unlock()
			if !ready {
				return ErrPrematureExit, nil
			}
			return nil, nil
		// result содержит исход завершившегося направления UDS во время наблюдения за процессом.
		case result := <-ioResults:
			if result.output {
				outputFinished = true
			}
			err = result.err
			if err == nil || errors.Is(err, io.EOF) {
				continue
			}
			reason = err
			break selectLoop
		case <-ctx.Done():
			reason = context.Cause(ctx)
			break selectLoop
		// now содержит время очередной проверки сроков продвижения дорожек.
		case now := <-ticker.C:
			if err = progress.health(now, start, s.config); err != nil {
				reason = err
				break selectLoop
			}
		}
	}
	cancel()
	// При ProcessScope отмена закрывает lifeline; gost-process-helper выполняет Interrupt, Kill и Wait.
	// Прямой запуск без ProcessScope использует CommandContext и WaitDelay.
	// deadline отсчитывает GracePeriod и KillTimeout после отмены; его истечение сохраняет
	// дальнейшее ожидание единственного Wait.
	deadline := time.NewTimer(s.config.GracePeriod + s.config.KillTimeout)
	defer deadline.Stop()
	select {
	case err = <-processDone:
		return errors.Join(reason, err), nil
	case <-deadline.C:
		s.queue.push(Diagnostic{Time: time.Now(), Attempt: id, Kind: "cleanup_pending", Message: ErrCleanupPending.Error()})
		// Истечение срока не подтверждает очистку: единственный владелец продолжает ждать процесс через Wait.
		err = <-processDone
		return errors.Join(reason, err), nil
	}
}

// Запускает управляемый Scope либо явный прямой тестовый процесс и возвращает канал единственного
// ожидания; stdout одновременно передаётся в диагностику и ограниченный разбор отчётов прогресса.
func (s *Supervisor) startProcess(ctx context.Context, scope *processowner.Scope, command Command, id uint64) (<-chan error, error) {
	// parser разбирает отчёты progress из stdout только текущей физической попытки.
	parser := newProgressParser(s.config.Progress, &s.progress, id)
	// stdout передаёт стандартный вывод процесса в диагностику и разбор progress.
	stdout := &diagnosticWriter{queue: s.queue, attempt: id, kind: "stdout", chunk: s.config.DiagnosticChunkBytes, progress: parser}
	// stderr передаёт стандартный поток ошибок процесса в диагностику без разбора progress.
	stderr := &diagnosticWriter{queue: s.queue, attempt: id, kind: "stderr", chunk: s.config.DiagnosticChunkBytes}
	// done передаёт единственный результат ожидания процесса после завершения разбора progress.
	done := make(chan error, 1)
	if scope != nil {
		// process представляет процесс, запущенный через Scope; err сообщает об отказе его
		// запуска.
		process, err := scope.Start(ctx, processowner.Command{Binary: command.Binary, Args: command.Args, ExtraFiles: command.ExtraFiles, Stdout: stdout, Stderr: stderr})
		if err != nil {
			parser.finish()
			return nil, err
		}
		go func() {
			// Ждёт управляемый процесс независимо от отмены ожидания и завершает разбор прогресса; если
			// result.Joined ложен, добавляет ErrCleanupPending к ошибке результата.

			// result содержит исход и признак присоединения управляемого процесса; waitErr
			// содержит ошибку ожидания или неподтверждённого завершения.
			result, waitErr := process.Wait(context.Background())
			parser.finish()
			if !result.Joined {
				waitErr = errors.Join(waitErr, ErrCleanupPending)
			}
			done <- errors.Join(result.Err, waitErr)
		}()
		return done, nil
	}
	// Прямой запуск оставлен отдельным портом низкоуровневых тестов компонента.
	// cmd запускает прямой процесс в низкоуровневом тестовом режиме без ProcessScope.
	cmd := exec.CommandContext(ctx, command.Binary, command.Args...)
	cmd.ExtraFiles = append([]*os.File(nil), command.ExtraFiles...)
	cmd.Cancel = func() error {
		// Отправляет прямому дочернему процессу os.Interrupt и возвращает ошибку сигнала, сохраняя
		// os.ErrProcessDone.

		// err содержит результат отправки os.Interrupt прямому дочернему процессу.
		err := cmd.Process.Signal(os.Interrupt)
		if errors.Is(err, os.ErrProcessDone) {
			return os.ErrProcessDone
		}
		return err
	}
	cmd.WaitDelay = s.config.GracePeriod
	cmd.Stdout, cmd.Stderr = stdout, stderr
	// err сообщает об отказе запуска прямого тестового процесса.
	if err := cmd.Start(); err != nil {
		parser.finish()
		return nil, err
	}
	go func() {
		// Единожды ждёт прямой дочерний процесс, завершает разбор прогресса и передаёт ошибку через done.

		// err содержит исход единственного Wait прямого тестового процесса.
		err := cmd.Wait()
		parser.finish()
		done <- err
	}()
	return done, nil
}
