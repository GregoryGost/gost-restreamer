package ffmpeg

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

// ownedPermit моделирует тестовый допуск с атомарным чтением действительности и блокировкой действий и
// отзыва.
type ownedPermit struct {
	// mu упорядочивает выполнение действий и отзыв тестового допуска.
	mu sync.Mutex
	// valid хранит атомарный признак действительности допуска.
	valid atomic.Bool
	// done закрывается единожды при отзыве владельца.
	done chan struct{}
}

// Создаёт действующий тестовый допуск владельца с каналом отзыва.
func newOwnedPermit() *ownedPermit {
	// p содержит создаваемый тестовый допуск с ещё не закрытым каналом отзыва.
	p := &ownedPermit{done: make(chan struct{})}
	p.valid.Store(true)
	return p
}

// Do выполняет действие под общей блокировкой только при действующем допуске и неотменённом контексте.
func (p *ownedPermit) Do(ctx context.Context, fn func() error) error {
	p.mu.Lock()
	defer p.mu.Unlock()
	// err содержит причину отмены контекста до выполнения разрешённого действия.
	if err := ctx.Err(); err != nil {
		return err
	}
	if !p.valid.Load() {
		return ErrOwnerRevoked
	}
	return fn()
}

// Valid возвращает атомарный признак действующего тестового допуска.
func (p *ownedPermit) Valid() bool { return p.valid.Load() }

// Done возвращает канал отзыва тестового владельца.
func (p *ownedPermit) Done() <-chan struct{} { return p.done }

// Единожды отзывает допуск под общей блокировкой и закрывает канал владельца.
func (p *ownedPermit) revoke() {
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.valid.Swap(false) {
		close(p.done)
	}
}

// Создаёт короткий временный каталог для UDS, используя /tmp на macOS, и регистрирует удаление после
// теста.
func shortBase(t *testing.T) string {
	t.Helper()
	// parent задаёт короткий временный корень UDS с учётом ограничений Darwin.
	parent := os.TempDir()
	if runtime.GOOS == "darwin" {
		// Системный TMPDIR macOS слишком длинный для вложенных путей UDS.
		parent = "/tmp"
	}
	// base содержит отдельный временный корень UDS; err сообщает об отказе его создания.
	base, err := os.MkdirTemp(parent, "gfr-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Удаляет принадлежащий тесту короткий каталог UDS и сообщает ошибку через t.Error.

		// err сообщает об отказе удаления тестового корня UDS при очистке.
		if err := os.RemoveAll(base); err != nil {
			t.Error(err)
		}
	})
	return base
}

// Создаёт конечные сроки и бюджеты Supervisor с отдельным коротким каталогом UDS для теста.
func supervisorConfig(t *testing.T) Config {
	t.Helper()
	return Config{BaseDir: shortBase(t), StartupTimeout: time.Second, VideoTimeout: time.Second, AudioTimeout: time.Second, WatchdogInterval: 10 * time.Millisecond, MaxRestarts: 0, RestartBackoff: 10 * time.Millisecond, GracePeriod: 50 * time.Millisecond, KillTimeout: time.Second, ShutdownTimeout: 2 * time.Second, DiagnosticEvents: 8, DiagnosticBytes: 16384, DiagnosticChunkBytes: 256, MaxCommandBytes: 4096, MaxCommandArgs: 32}
}

// Создаёт Supervisor и регистрирует его ограниченную остановку с проверкой ошибки после теста.
func newTestSupervisor(t *testing.T, c Config) *Supervisor {
	t.Helper()
	// s содержит тестовый Supervisor; err сообщает об отказе его создания.
	s, err := New(c)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Останавливает Supervisor с отдельным трёхсекундным сроком и сообщает ошибку через t.Error.

		// ctx ограничивает резервную остановку тремя секундами; cancel освобождает таймер.
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		// err сообщает об отказе резервного Stop при очистке теста.
		if err := s.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	return s
}

// Создаёт построитель команды повторного запуска тестового бинарника в заданном режиме с путями UDS
// текущей попытки.
func childCommand(mode string) CommandBuilder {
	return func(_ context.Context, a Attempt) (Command, error) {
		// Строит команду текущего тестового бинарника с выбранным режимом дочернего процесса и путями UDS
		// попытки.

		// binary задаёт текущий тестовый бинарник для дочернего режима; err сообщает об отказе
		// определения его пути.
		binary, err := os.Executable()
		return Command{Binary: binary, Args: []string{"-test.run=^TestSupervisorChild$", "--", "owned-child", mode, a.InputPath, a.OutputPath}}, err
	}
}

// Ожидает отмены контекста без чтения соединения и возвращает причину отмены.
func inertHandler(ctx context.Context, _ net.Conn, _ Progress) error { <-ctx.Done(); return ctx.Err() }

// Создаёт тестового работника с отдельным допуском и управляемым дочерним процессом для заданного
// режима.
func childWorker(mode string) Worker {
	return Worker{SessionID: "session/unsafe", GroupID: "group", Owner: newOwnedPermit(), Command: childCommand(mode), Input: inertHandler, Output: inertHandler}
}

// Ждёт результат Supervisor в пределах пяти секунд либо завершает тест ошибкой ожидания.
func waitResult(t *testing.T, s *Supervisor) Result {
	t.Helper()
	// ctx ограничивает ожидание результата пятью секундами; cancel освобождает таймер.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// r содержит итог работника; err сообщает об отказе ожидания Supervisor.
	r, err := s.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	return r
}

// TestSupervisorChild выполняется только отдельным управляемым дочерним процессом.
func TestSupervisorChild(t *testing.T) {
	// index хранит позицию маркера owned-child в argv; -1 означает обычный тестовый запуск.
	index := -1
	// i обозначает позицию аргумента; arg содержит строку при поиске режима дочернего процесса.
	for i, arg := range os.Args {
		if arg == "owned-child" {
			index = i
			break
		}
	}
	if index < 0 {
		return
	}
	// mode выбирает ранний выход, зависание, большой вывод или обмен по UDS.
	mode := os.Args[index+1]
	switch mode {
	case "exit":
		os.Exit(3)
	case "hang":
		signal.Ignore(os.Interrupt)
		for {
			time.Sleep(time.Hour)
		}
	case "flood":
		// data содержит большой бинарный вывод для проверки независимости процесса от
		// потребителя диагностики.
		data := bytes.Repeat([]byte{0xff, '\n'}, 1<<18)
		// err сообщает об отказе записи большого контрольного stdout.
		if _, err := os.Stdout.Write(data); err != nil {
			os.Exit(2)
		}
		// err сообщает об отказе записи большого контрольного stderr.
		if _, err := os.Stderr.Write(data); err != nil {
			os.Exit(2)
		}
		os.Exit(0)
	}
	// Сначала подключается выход: последовательный Accept двух направлений создал бы взаимную блокировку.
	// out содержит выходное UDS-соединение; err сохраняет результаты подключения и обмена
	// контрольными данными.
	out, err := net.Dial("unix", os.Args[index+3])
	if err != nil {
		os.Exit(2)
	}
	if _, err = out.Write([]byte("v")); err != nil {
		os.Exit(2)
	}
	// in содержит входное UDS-соединение для дочитывания родительских данных.
	in, err := net.Dial("unix", os.Args[index+2])
	if err != nil {
		os.Exit(2)
	}
	if _, err = io.Copy(io.Discard, in); err != nil {
		os.Exit(2)
	}
	if _, err = out.Write([]byte("a")); err != nil {
		os.Exit(2)
	}
	if err = in.Close(); err != nil {
		os.Exit(2)
	}
	if mode == "connected-hang" {
		signal.Ignore(os.Interrupt)
		for {
			time.Sleep(time.Hour)
		}
	}
	if err = out.Close(); err != nil {
		os.Exit(2)
	}
	os.Exit(0)
}

// Проверяет параллельный приём обоих направлений и дочитывание проверяемого выхода после завершения
// процесса с очисткой всех путей.
func TestSupervisorDuplexCompletion(t *testing.T) {
	// c увеличивает сроки продвижения дорожек для нормального завершения настоящего процесса.
	c := supervisorConfig(t)
	c.VideoTimeout, c.AudioTimeout = 3*time.Second, 3*time.Second
	// s выполняет дуплексного работника с задержанным разбором выхода.
	s := newTestSupervisor(t, c)
	// w содержит работника с контрольными входными байтами и разбором видеомаркера и аудиомаркера.
	w := childWorker("duplex")
	w.Input = func(_ context.Context, c net.Conn, _ Progress) error {
		// Передаёт условные входные байты дочернему процессу и возвращает ошибку записи.
		// err содержит результат записи условного входа дочернему процессу.
		_, err := c.Write([]byte("input"))
		return err
	}
	w.Output = func(_ context.Context, c net.Conn, report Progress) error {
		// С задержкой разбирает маркеры выходных дорожек и сообщает первый прогресс каждой, проверяя
		// дочитывание после завершения процесса.

		// Задержка проверки пакетов позволяет процессу завершиться раньше разбора выходных данных.
		time.Sleep(30 * time.Millisecond)
		// data принимает очередной однобайтовый маркер выходной дорожки.
		data := make([]byte, 1)
		for {
			// err сообщает о завершении или отказе чтения очередного выходного маркера.
			_, err := io.ReadFull(c, data)
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			// track выбирает видео либо аудио по прочитанному контрольному маркеру.
			track := Video
			if data[0] == 'a' {
				track = Audio
			}
			if !report(track, 1) {
				return errors.New("не принят первый пакет")
			}
		}
	}
	// err сообщает об отказе регистрации дуплексного работника.
	if err := s.Start(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	// r содержит итог успешного дуплексного выполнения и очистки.
	r := waitResult(t, s)
	if r.Err != nil || !r.Clean || r.Attempts != 1 {
		t.Fatalf("исход: %+v", r)
	}
	// entries содержит остатки корня UDS после выполнения; err сообщает об отказе перечисления
	// каталога.
	entries, err := os.ReadDir(s.config.BaseDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(entries) != 0 {
		t.Fatalf("остались ресурсы: %v", entries)
	}
}

// Проверяет ровно одну начальную и две повторные попытки при раннем выходе и запрет повторного Start
// завершённого Supervisor.
func TestSupervisorEarlyExitHasFiniteRestartBudget(t *testing.T) {
	// c разрешает ровно две повторные попытки после первоначального раннего выхода.
	c := supervisorConfig(t)
	c.MaxRestarts = 2
	// s выполняет проверку конечного бюджета повторов.
	s := newTestSupervisor(t, c)
	// err сообщает об отказе первого Start работника с ранним выходом.
	if err := s.Start(context.Background(), childWorker("exit")); err != nil {
		t.Fatal(err)
	}
	// r содержит итог для проверки трёх попыток и ErrPrematureExit.
	r := waitResult(t, s)
	if r.Attempts != 3 || !r.Clean || !errors.Is(r.Err, ErrPrematureExit) {
		t.Fatalf("исход: %+v", r)
	}
	// err содержит ожидаемый ErrStopped при повторном Start завершённого Supervisor.
	if err := s.Start(context.Background(), childWorker("exit")); !errors.Is(err, ErrStopped) {
		t.Fatalf("повторный Start: %v", err)
	}
}

// Проверяет конечное завершение процесса, игнорирующего Interrupt и не подключающего UDS, по сроку
// готовности с подтверждённой очисткой.
func TestSupervisorStopsHangingProcessAndAccepts(t *testing.T) {
	// c сокращает срок готовности для процесса, не подключающего UDS.
	c := supervisorConfig(t)
	c.StartupTimeout = 150 * time.Millisecond
	// s выполняет проверку остановки зависшего процесса и ожидающих Accept.
	s := newTestSupervisor(t, c)
	// err сообщает об отказе запуска зависшего тестового процесса.
	if err := s.Start(context.Background(), childWorker("hang")); err != nil {
		t.Fatal(err)
	}
	// r содержит итог для проверки ErrStartupTimeout и подтверждённой очистки.
	r := waitResult(t, s)
	if !r.Clean || !errors.Is(r.Err, ErrStartupTimeout) {
		t.Fatalf("исход: %+v", r)
	}
}

// Проверяет ErrCleanupPending до возврата задержанного построителя и запрет запуска его поздней
// команды после Stop.
func TestSupervisorDelayedBuilderCannotEscapeStop(t *testing.T) {
	// c сокращает срок ожидания Stop для проверки задержанного построителя.
	c := supervisorConfig(t)
	c.ShutdownTimeout = 30 * time.Millisecond
	// s выполняет попытку, чей построитель возвращает команду после Stop.
	s := newTestSupervisor(t, c)
	// entered сообщает о входе в построитель; release разрешает возврат команды после проверки
	// неподтверждённой очистки.
	entered, release := make(chan struct{}), make(chan struct{})
	// w содержит работника с задерживаемым построителем команды.
	w := childWorker("exit")
	w.Command = func(context.Context, Attempt) (Command, error) {
		// Задерживает возврат команды до release, создавая незавершённый построитель во время Stop.

		close(entered)
		<-release
		return Command{Binary: "/usr/bin/false"}, nil
	}
	// err сообщает об отказе регистрации работника с задержанным построителем.
	if err := s.Start(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-s.Done():
		t.Fatalf("startup завершился: %+v", waitResult(t, s))
	case <-time.After(3 * time.Second):
		t.Fatal("builder не вызван")
	}
	// err содержит ожидаемый ErrCleanupPending при Stop до возврата построителя.
	if err := s.Stop(context.Background()); !errors.Is(err, ErrCleanupPending) {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-s.Done():
		t.Fatal("ложное подтверждение cleanup")
	default:
	}
	close(release)
	// r содержит окончательный итог после освобождения построителя для проверки отмены и очистки.
	r := waitResult(t, s)
	if !r.Clean || !errors.Is(r.Err, context.Canceled) {
		t.Fatalf("исход: %+v", r)
	}
	for {
		// d содержит очередное диагностическое событие; err сообщает о завершении либо отказе
		// чтения при проверке отсутствия позднего запуска.
		d, err := s.NextDiagnostic(context.Background())
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if d.Kind == "command" {
			t.Fatal("процесс запущен после Stop")
		}
	}
}

// Проверяет, что возврат Stop по сроку не закрывает Done до фактического завершения заблокированного
// обработчика.
func TestSupervisorUnjoinedHandlerKeepsCleanupPending(t *testing.T) {
	// c сокращает ожидание Stop для проверки ещё работающего UDS-обработчика.
	c := supervisorConfig(t)
	c.ShutdownTimeout = 30 * time.Millisecond
	// s выполняет работника с выходным обработчиком, задержанным после Stop.
	s := newTestSupervisor(t, c)
	// entered сообщает о входе в выходной обработчик; release разрешает его фактическое завершение.
	entered, release := make(chan struct{}), make(chan struct{})
	// w содержит работника с немедленно завершаемым входом и задержанным выходом.
	w := childWorker("connected-hang")
	w.Input = func(context.Context, net.Conn, Progress) error {
		// Сразу завершает входной обработчик, чтобы тест задерживал только выходного пользователя Namespace.
		return nil
	}
	w.Output = func(context.Context, net.Conn, Progress) error {
		// Сообщает о входе и ждёт release, моделируя обработчик, который ещё не завершился после Stop.
		close(entered)
		<-release
		return nil
	}
	// err сообщает об отказе регистрации сценария задержанного выходного обработчика.
	if err := s.Start(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(3 * time.Second):
		t.Fatal("handler не вызван")
	}
	// err содержит ожидаемый ErrCleanupPending до фактического завершения обработчика.
	if err := s.Stop(context.Background()); !errors.Is(err, ErrCleanupPending) {
		t.Fatalf("Stop: %v", err)
	}
	select {
	case <-s.Done():
		t.Fatal("callback не присоединён")
	default:
	}
	close(release)
	// r содержит окончательный результат после разрешения выходному обработчику завершиться.
	if r := waitResult(t, s); !r.Clean {
		t.Fatalf("исход: %+v", r)
	}
}

// Проверяет отмену контекста выполняющегося построителя при отзыве Owner и сохранение причины
// ErrOwnerRevoked с очисткой ресурсов.
func TestSupervisorRevokedOwnerCancelsBuilder(t *testing.T) {
	// s выполняет проверку отмены построителя при отзыве Owner.
	s := newTestSupervisor(t, supervisorConfig(t))
	// w содержит работника с построителем, ожидающим отмены контекста.
	w := childWorker("exit")
	// p предоставляет конкретный тестовый допуск для явного отзыва во время построения команды.
	p := w.Owner.(*ownedPermit)
	// entered сообщает о начале построителя до отзыва владельца.
	entered := make(chan struct{})
	w.Command = func(ctx context.Context, _ Attempt) (Command, error) {
		// Сообщает о входе и ждёт отмены контекста построителя, затем возвращает её ошибку без команды.

		close(entered)
		<-ctx.Done()
		return Command{}, ctx.Err()
	}
	// err сообщает об отказе регистрации работника перед отзывом владельца.
	if err := s.Start(context.Background(), w); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-s.Done():
		t.Fatalf("startup завершился: %+v", waitResult(t, s))
	case <-time.After(3 * time.Second):
		t.Fatal("builder не вызван")
	}
	p.revoke()
	// r содержит окончательный результат для проверки ErrOwnerRevoked и очистки.
	r := waitResult(t, s)
	if !r.Clean || !errors.Is(r.Err, ErrOwnerRevoked) {
		t.Fatalf("исход: %+v", r)
	}
}

// Проверяет завершение дочернего процесса при большом непрочитанном выводе с учётом потерь
// ограниченной очереди.
func TestSupervisorBlockedDiagnosticConsumerDoesNotBlockChild(t *testing.T) {
	// s выполняет процесс, создающий большой вывод при непрочитанной диагностике.
	s := newTestSupervisor(t, supervisorConfig(t))
	// err сообщает об отказе запуска сценария переполнения диагностики.
	if err := s.Start(context.Background(), childWorker("flood")); err != nil {
		t.Fatal(err)
	}
	// r содержит результат завершения процесса для проверки очистки и ненулевого числа потерь.
	r := waitResult(t, s)
	if !r.Clean || s.DroppedDiagnostics() == 0 {
		t.Fatalf("исход: %+v, dropped=%d", r, s.DroppedDiagnostics())
	}
}

// Проверяет отказ равных и убывающих DTS без продления времени здоровья дорожки.
func TestProgressDuplicatesCannotRestoreHealth(t *testing.T) {
	// p хранит контрольные DTS и времена продвижения для проверки отклонения дубликатов.
	p := &packetProgress{}
	if !p.report(Video, -10) || !p.report(Audio, -20) {
		t.Fatal("первые пакеты отклонены")
	}
	p.mu.Lock()
	p.at[0] = time.Now().Add(-time.Hour)
	p.mu.Unlock()
	if p.report(Video, -10) || p.report(Video, -11) {
		t.Fatal("дубликат обновил прогресс")
	}
	// err содержит ожидаемый ErrProgressTimeout после равных и убывающих DTS.
	if err := p.health(time.Now(), time.Now(), Config{VideoTimeout: time.Second, AudioTimeout: time.Second}); !errors.Is(err, ErrProgressTimeout) {
		t.Fatal(err)
	}
}

// Проверяет единственный Accept направления, права сокетов 0600 и отказ очистки подменённого пути с
// сохранением чужого файла.
func TestNamespaceOwnershipAndSingleAccept(t *testing.T) {
	// base задаёт изолированный короткий корень проверки приёма и владения сокетами.
	base := shortBase(t)
	// n содержит проверяемый Namespace; err сохраняет результаты создания, подключения и очистки
	// путей.
	n, err := NewNamespace(base, "session")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает Namespace после сценария владения путями, игнорируя ошибку резервного закрытия.
		_ = n.Close()
	})
	// accepted передаёт исход единственного приёма входного соединения.
	accepted := make(chan error, 1)
	go func() {
		// Принимает входное соединение Namespace в отдельной горутине и передаёт ошибку через accepted.
		// err содержит результат первого AcceptInput, передаваемый основному тесту.
		_, err := n.AcceptInput()
		accepted <- err
	}()
	// c содержит клиентское подключение к входному UDS.
	c, err := net.Dial("unix", n.InputPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает клиентское соединение UDS после теста, игнорируя ошибку.
		_ = c.Close()
	})
	if err = <-accepted; err != nil {
		t.Fatal(err)
	}
	if _, err = n.AcceptInput(); !errors.Is(err, ErrAlreadyAccepted) {
		t.Fatal(err)
	}
	// path содержит очередной путь сокета для проверки прав 0600.
	for _, path := range []string{n.InputPath, n.OutputPath} {
		// info содержит метаданные проверяемого сокета; err сообщает об отказе их чтения.
		info, err := os.Lstat(path)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm() != 0600 {
			t.Fatal(info.Mode())
		}
	}
	if err = os.Remove(n.OutputPath); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(n.OutputPath, []byte("чужой файл"), 0600); err != nil {
		t.Fatal(err)
	}
	if err = n.Close(); !errors.Is(err, ErrResourceOwnership) {
		t.Fatal(err)
	}
	// data содержит байты чужого файла на месте выходного сокета после отказа очистки.
	data, err := os.ReadFile(n.OutputPath)
	if err != nil || string(data) != "чужой файл" {
		t.Fatalf("чужой путь изменён: %q %v", data, err)
	}
}

// Проверяет отказ каталога Session, подменённого символической ссылкой, без создания ресурсов во
// внешнем каталоге.
func TestNamespaceRejectsSymlinkSession(t *testing.T) {
	// base задаёт корень Namespace; foreign задаёт внешний каталог, куда указывает подменённый
	// Session.
	base, foreign := shortBase(t), shortBase(t)
	// err сообщает об отказе создания символической ссылки вместо каталога Session.
	if err := os.Symlink(foreign, filepath.Join(base, "session")); err != nil {
		t.Fatal(err)
	}
	// err содержит ожидаемый ErrResourceOwnership при создании Namespace через подменённый Session.
	if _, err := NewNamespace(base, "session"); !errors.Is(err, ErrResourceOwnership) {
		t.Fatal(err)
	}
	// entries содержит содержимое внешнего каталога; err сообщает об отказе чтения при проверке
	// отсутствия ресурсов.
	entries, err := os.ReadDir(foreign)
	if err != nil || len(entries) != 0 {
		t.Fatalf("symlink escape: %v %v", entries, err)
	}
}

// Проверяет освобождение ожидающего AcceptOutput после Shutdown и последующую очистку через Close.
func TestNamespaceShutdownUnblocksAccept(t *testing.T) {
	// n содержит Namespace с ожидающим AcceptOutput; err сохраняет результаты создания, Shutdown и
	// Close.
	n, err := NewNamespace(shortBase(t), "session")
	if err != nil {
		t.Fatal(err)
	}
	// done передаёт результат AcceptOutput после закрытия слушателя.
	done := make(chan error, 1)
	go func() {
		// Ждёт выходное соединение и передаёт ошибку через done, позволяя проверить прерывание Accept при
		// Shutdown.
		// err содержит ожидаемый net.ErrClosed от выходного Accept после Shutdown.
		_, err := n.AcceptOutput()
		done <- err
	}()
	if err = n.Shutdown(); err != nil {
		t.Fatal(err)
	}
	select {
	case err = <-done:
		if !errors.Is(err, net.ErrClosed) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("Accept не завершился")
	}
	if err = n.Close(); err != nil {
		t.Fatal(err)
	}
}
