package ffmpeg_test

import (
	"bytes"
	"context"
	"errors"
	"io"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"sync"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
)

// acceptanceOwner моделирует отзываемый допуск с общей блокировкой действий и каналом уведомления.
type acceptanceOwner struct {
	// mu защищает действительность допуска и выполнение действий владельца.
	mu sync.Mutex
	// done закрывается единожды при отзыве допуска.
	done chan struct{}
	// valid показывает, разрешены ли новые действия данного владельца.
	valid bool
}

// Создаёт действующего управляемого владельца с отдельным каналом отзыва.
func newAcceptanceOwner() *acceptanceOwner {
	return &acceptanceOwner{done: make(chan struct{}), valid: true}
}

// Do выполняет действие под блокировкой владельца после проверки контекста и действительности допуска.
func (o *acceptanceOwner) Do(ctx context.Context, fn func() error) error {
	o.mu.Lock()
	defer o.mu.Unlock()
	// err содержит причину отмены контекста до выполнения действия под допуском.
	if err := ctx.Err(); err != nil {
		return err
	}
	if !o.valid {
		return ffmpeg.ErrOwnerRevoked
	}
	return fn()
}

// Valid возвращает состояние допуска под блокировкой.
func (o *acceptanceOwner) Valid() bool { o.mu.Lock(); defer o.mu.Unlock(); return o.valid }

// Done возвращает канал отзыва владельца.
func (o *acceptanceOwner) Done() <-chan struct{} { return o.done }

// Отзывает допуск и единожды закрывает канал под общей блокировкой действий.
func (o *acceptanceOwner) revoke() {
	o.mu.Lock()
	defer o.mu.Unlock()
	if o.valid {
		o.valid = false
		close(o.done)
	}
}

// Создаёт Supervisor с конечными бюджетами в коротком временном каталоге, применяет явные настройки
// теста и регистрирует остановку и удаление ресурсов.
func acceptanceSupervisor(t *testing.T, change func(*ffmpeg.Config)) (*ffmpeg.Supervisor, string) {
	t.Helper()
	// parent задаёт короткий временный корень с учётом длины UDS на Darwin.
	parent := os.TempDir()
	if runtime.GOOS == "darwin" {
		// Системный TMPDIR macOS слишком длинный для вложенных путей UDS.
		parent = "/tmp"
	}
	// base содержит корень ресурсов проверяемого супервизора; err сохраняет результаты создания
	// каталога и Supervisor.
	base, err := os.MkdirTemp(parent, "gs-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Удаляет временный корень сценария и сообщает ошибку через t.Error.

		// err сообщает об отказе удаления временного корня после сценария.
		if err := os.RemoveAll(base); err != nil {
			t.Error(err)
		}
	})
	// cfg задаёт конечные сроки и бюджеты приемочного сценария перед его явной настройкой.
	cfg := ffmpeg.Config{BaseDir: base, StartupTimeout: time.Second, VideoTimeout: 150 * time.Millisecond, AudioTimeout: 150 * time.Millisecond, WatchdogInterval: 10 * time.Millisecond, MaxRestarts: 0, RestartBackoff: 10 * time.Millisecond, GracePeriod: 30 * time.Millisecond, KillTimeout: time.Second, ShutdownTimeout: 2 * time.Second, DiagnosticEvents: 1024, DiagnosticBytes: 1 << 20, DiagnosticChunkBytes: 64, MaxCommandBytes: 4096, MaxCommandArgs: 32}
	if change != nil {
		change(&cfg)
	}
	// s содержит созданный Supervisor для выбранного приемочного сценария.
	s, err := ffmpeg.New(cfg)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Останавливает Supervisor с отдельным трёхсекундным сроком и сообщает ошибку через t.Error.

		// ctx ограничивает резервный Stop тремя секундами; cancel освобождает таймер.
		ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
		defer cancel()
		// err сообщает об отказе резервной остановки Supervisor при очистке теста.
		if err := s.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	return s, base
}

// Строит команду управляемого дочернего теста и при наличии канала сообщает точные пути и номер
// попытки.
func acceptanceChildCommand(t *testing.T, mode string, seen chan<- ffmpeg.Attempt) ffmpeg.CommandBuilder {
	t.Helper()
	// binary задаёт текущий тестовый бинарник для запуска дочернего режима; err сообщает об отказе
	// определения пути.
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	return func(_ context.Context, a ffmpeg.Attempt) (ffmpeg.Command, error) {
		// Строит команду выбранного режима тестового дочернего процесса и при необходимости сообщает пути
		// попытки через seen.

		if seen != nil {
			seen <- a
		}
		return ffmpeg.Command{Binary: binary, Args: []string{"-test.run=^TestAcceptanceSupervisorChild$", "--", "media-helper", mode, a.InputPath, a.OutputPath}}, nil
	}
}

// Ожидает отмены контекста входного обработчика и возвращает её причину.
func acceptanceBlockInput(ctx context.Context, _ net.Conn, _ ffmpeg.Progress) error {
	<-ctx.Done()
	return ctx.Err()
}

// Создаёт обработчик контрольной пары VA, сообщает первые DTS обеих дорожек и при наличии канала
// фиксирует принятие повторных меток.
func acceptanceOutput(ready chan<- struct{}, repeated chan<- bool) ffmpeg.IOHandler {
	return func(_ context.Context, c net.Conn, progress ffmpeg.Progress) error {
		// Проверяет VA, отмечает первый прогресс обеих дорожек и далее сообщает результаты повторных DTS, пока
		// чтение не завершится ошибкой.

		// marker содержит первые два выходных байта для проверки контрольной пары VA.
		var marker [2]byte
		// err сообщает об отказе чтения начальных контрольных маркеров из UDS.
		if _, err := io.ReadFull(c, marker[:]); err != nil {
			return err
		}
		if marker != [2]byte{'V', 'A'} {
			return errors.New("неверный синтетический пакет")
		}
		if !progress(ffmpeg.Video, -10) || !progress(ffmpeg.Audio, -20) {
			return errors.New("первый валидный пакет отвергнут")
		}
		if ready != nil {
			close(ready)
		}
		for {
			// one принимает очередной байт, по которому тест повторно сообщает неизменные
			// DTS.
			var one [1]byte
			// err содержит результат чтения следующего контрольного байта до завершения
			// соединения.
			if _, err := c.Read(one[:]); err != nil {
				return err
			}
			if repeated != nil {
				repeated <- progress(ffmpeg.Video, -10) || progress(ffmpeg.Audio, -20)
			}
		}
	}
}

// Ждёт Supervisor в конечный срок и требует подтверждённую очистку без CleanupErr.
func acceptanceWait(t *testing.T, s *ffmpeg.Supervisor) ffmpeg.Result {
	t.Helper()
	// ctx ограничивает ожидание окончательного результата четырьмя секундами; cancel освобождает
	// таймер.
	ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
	defer cancel()
	// r содержит итог работника и очистки; err сообщает об отказе ожидания Supervisor.
	r, err := s.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if !r.Clean || r.CleanupErr != nil {
		t.Fatalf("нет доказательства очистки: %+v", r)
	}
	return r
}

// При специальном аргументе работает дочерним процессом: управляет ранним выходом, бинарной
// диагностикой, разрывом UDS или повторными метками.
func TestAcceptanceSupervisorChild(t *testing.T) {
	// pos хранит позицию маркера media-helper в argv; -1 означает обычный запуск теста.
	pos := -1
	// i обозначает позицию аргумента; arg содержит проверяемую строку при поиске дочернего режима.
	for i, arg := range os.Args {
		if arg == "media-helper" {
			pos = i
			break
		}
	}
	if pos < 0 {
		return
	}
	if len(os.Args) < pos+4 {
		os.Exit(91)
	}
	// mode выбирает поведение дочернего процесса; input задаёт входной UDS; output задаёт выходной
	// UDS.
	mode, input, output := os.Args[pos+1], os.Args[pos+2], os.Args[pos+3]
	if mode == "early" {
		os.Exit(23)
	}
	if mode == "diagnostics" {
		// payload содержит полный контрольный бинарный stderr, способный заполнить очередь
		// диагностики.
		payload := bytes.Repeat([]byte{0xff, 0, 0xc3, 0xa9, '\n'}, 8192)
		// err сообщает об отказе записи контрольной бинарной диагностики.
		if _, err := os.Stderr.Write(payload); err != nil {
			os.Exit(92)
		}
	}
	// i содержит входное UDS-соединение; err сохраняет ошибки подключения и передачи выходных
	// маркеров.
	i, err := net.DialTimeout("unix", input, time.Second)
	if err != nil {
		os.Exit(93)
	}
	defer func() {
		// Закрывает входное соединение тестового дочернего процесса при возврате, игнорируя ошибку.
		_ = i.Close()
	}()
	// o содержит выходное UDS-соединение для контрольных маркеров и повторных пакетов.
	o, err := net.DialTimeout("unix", output, time.Second)
	if err != nil {
		os.Exit(94)
	}
	defer func() {
		// Закрывает выходное соединение тестового дочернего процесса при возврате, игнорируя ошибку.
		_ = o.Close()
	}()
	if _, err = o.Write([]byte("VA")); err != nil {
		os.Exit(95)
	}
	if mode == "disconnect" {
		_ = o.Close()
		_, _ = io.Copy(io.Discard, i)
		return
	}
	if mode == "duplicates" {
		// ticker задаёт период передачи байтов без продвижения DTS в сценарии duplicates.
		ticker := time.NewTicker(10 * time.Millisecond)
		defer ticker.Stop()
		for range ticker.C {
			// err содержит результат записи очередного байта дубликата в выходное
			// соединение.
			if _, err := o.Write([]byte{'D'}); err != nil {
				return
			}
		}
	}
	_, _ = io.Copy(io.Discard, i)
}

// Проверяет конечный предел повторов, новые пути каждой попытки и отсутствие оставшихся пространств
// UDS после ранних выходов.
func TestAcceptanceSupervisorEarlyExitRestartsAndCleansEveryNamespace(t *testing.T) {
	// s выполняет три попытки с ранним завершением; base задаёт корень проверки удалённых Namespace.
	s, base := acceptanceSupervisor(t, func(c *ffmpeg.Config) {
		// Разрешает две повторные попытки для проверки раннего выхода дочернего процесса.
		c.MaxRestarts = 2
	})
	// seen передаёт идентичности всех трёх попыток для проверки новых путей.
	seen := make(chan ffmpeg.Attempt, 3)
	// err сообщает об отказе регистрации сценария ранних выходов.
	if err := s.Start(context.Background(), ffmpeg.Worker{SessionID: "session", GroupID: "group", Owner: newAcceptanceOwner(), Command: acceptanceChildCommand(t, "early", seen), Input: acceptanceBlockInput, Output: acceptanceOutput(nil, nil)}); err != nil {
		t.Fatal(err)
	}
	// r содержит итог ограниченного числа повторов после ранних выходов.
	r := acceptanceWait(t, s)
	if r.Attempts != 3 || r.Err == nil {
		t.Fatalf("нарушен предел рестартов: %+v", r)
	}
	close(seen)
	// paths отмечает уже встреченные входные UDS для обнаружения повторного использования
	// пространства имён.
	paths := make(map[string]bool)
	// a содержит идентичность очередной фактической попытки из построителя команды.
	for a := range seen {
		if paths[a.InputPath] {
			t.Fatal("рестарт переиспользовал namespace")
		}
		paths[a.InputPath] = true
		// err содержит результат проверки удаления каталога конкретной завершённой попытки.
		if _, err := os.Stat(filepath.Dir(a.InputPath)); !errors.Is(err, os.ErrNotExist) {
			t.Fatalf("namespace попытки %d остался: %v", a.ID, err)
		}
	}
	// entries содержит остатки корня после всех попыток; err сообщает об отказе перечисления
	// каталога.
	entries, err := os.ReadDir(base)
	if err != nil || len(entries) != 0 {
		t.Fatalf("остатки после join: %v, %v", entries, err)
	}
}

// Проверяет, что отзыв владельца и Stop сохраняют ErrCleanupPending до возврата уже
// зарегистрированного построителя, затем очищают UDS с исходной причиной отзыва.
func TestAcceptanceSupervisorStopWaitsForRegisteredBuilder(t *testing.T) {
	// s выполняет работника с задержанным построителем; base задаёт корень проверки окончательной
	// очистки.
	s, base := acceptanceSupervisor(t, nil)
	// entered сообщает о входе в построитель; release разрешает ему вернуться после проверки Stop.
	entered, release := make(chan struct{}), make(chan struct{})
	// once исключает повторное закрытие release при обычном и резервном освобождении построителя.
	var once sync.Once
	t.Cleanup(func() {
		// Освобождает задержанный построитель при завершении теста через sync.Once.

		once.Do(func() {
			// Закрывает release при первом резервном освобождении построителя.
			close(release)
		})
	})
	// owner предоставляет отзываемый допуск задержанного работника.
	owner := newAcceptanceOwner()
	// err сообщает об отказе регистрации работника с задержанным построителем.
	if err := s.Start(context.Background(), ffmpeg.Worker{SessionID: "session", GroupID: "group", Owner: owner, Command: func(context.Context, ffmpeg.Attempt) (ffmpeg.Command, error) {
		// Сообщает о входе и ждёт release, позволяя проверить, что Stop не подтверждает завершение
		// зарегистрированного построителя раньше его возврата.

		close(entered)
		<-release
		return ffmpeg.Command{Binary: "/does-not-exist"}, nil
	}, Input: acceptanceBlockInput, Output: acceptanceOutput(nil, nil)}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-entered:
	case <-time.After(time.Second):
		t.Fatal("builder не начался")
	}
	owner.revoke()
	// ctx передаёт уже отменённый контекст в Stop; cancel отменяет его до вызова.
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	// err содержит ожидаемый ErrCleanupPending при незавершённом построителе команды.
	if err := s.Stop(ctx); !errors.Is(err, ffmpeg.ErrCleanupPending) {
		t.Fatal("заблокированный зарегистрированный запуск объявлен очищенным:", err)
	}
	select {
	case <-s.Done():
		t.Fatal("Done закрылся до завершения builder")
	default:
	}
	once.Do(func() {
		// Освобождает построитель после проверки неподтверждённой очистки.
		close(release)
	})
	// r содержит окончательный итог после возврата построителя для проверки причины отзыва.
	r := acceptanceWait(t, s)
	if !errors.Is(r.Err, ffmpeg.ErrOwnerRevoked) {
		t.Fatal("потеряна причина отзыва:", r.Err)
	}
	// entries содержит оставшиеся пути после завершения задержанного построителя; err сообщает об
	// отказе чтения корня.
	entries, err := os.ReadDir(base)
	if err != nil || len(entries) != 0 {
		t.Fatalf("UDS не очищены: %v %v", entries, err)
	}
}

// Проверяет истечение срока прогресса при повторных DTS даже при продолжающемся поступлении
// контрольных байтов.
func TestAcceptanceSupervisorDuplicatePacketsCannotFeedWatchdog(t *testing.T) {
	// s проверяет контроль продвижения при продолжающейся передаче повторных DTS.
	s, _ := acceptanceSupervisor(t, nil)
	// repeated сохраняет результаты повторных отчётов DTS для проверки их отклонения.
	repeated := make(chan bool, 64)
	// err сообщает об отказе регистрации работника, передающего только дубликаты после первых
	// пакетов.
	if err := s.Start(context.Background(), ffmpeg.Worker{SessionID: "session", GroupID: "group", Owner: newAcceptanceOwner(), Command: acceptanceChildCommand(t, "duplicates", nil), Input: acceptanceBlockInput, Output: acceptanceOutput(nil, repeated)}); err != nil {
		t.Fatal(err)
	}
	// r содержит итог для проверки ErrProgressTimeout при неизменных DTS.
	r := acceptanceWait(t, s)
	if !errors.Is(r.Err, ffmpeg.ErrProgressTimeout) {
		t.Fatalf("дубликаты скрыли отсутствие продвижения: %+v", r)
	}
	close(repeated)
	if len(repeated) == 0 {
		t.Fatal("проверка не получила дубликаты")
	}
	// accepted содержит результат очередного повторного отчёта DTS, который должен быть false.
	for accepted := range repeated {
		if accepted {
			t.Fatal("дубликат принят как продвижение")
		}
	}
}

// Проверяет передачу первых медиаданных при переполненной непрочитанной диагностике и учёт потерь
// после остановки.
func TestAcceptanceSupervisorUnreadDiagnosticsCannotBlockMedia(t *testing.T) {
	// s выполняет медиатракт с намеренно малой непрочитанной очередью диагностики.
	s, _ := acceptanceSupervisor(t, func(c *ffmpeg.Config) {
		// Ограничивает очередь диагностики двумя событиями и 8192 байтами для проверки независимости медиа от
		// её чтения.
		c.DiagnosticEvents = 2
		c.DiagnosticBytes = 8192
	})
	// ready сообщает о получении первых проверенных пакетов вопреки переполнению диагностики.
	ready := make(chan struct{})
	// err сообщает об отказе регистрации работника с большим бинарным stderr.
	if err := s.Start(context.Background(), ffmpeg.Worker{SessionID: "session", GroupID: "group", Owner: newAcceptanceOwner(), Command: acceptanceChildCommand(t, "diagnostics", nil), Input: acceptanceBlockInput, Output: acceptanceOutput(ready, nil)}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("нечитанная диагностика заблокировала медиа")
	}
	// ctx ограничивает остановку после получения медиа двумя секундами; cancel освобождает таймер.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// err сообщает об отказе остановки работника с переполненной диагностикой.
	if err := s.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	acceptanceWait(t, s)
	if s.DroppedDiagnostics() == 0 {
		t.Fatal("переполнение очереди не отражено счётчиком")
	}
}

// Проверяет, что исчезновение выходного UDS после первых дорожек завершается ошибкой работника при
// подтверждённой очистке.
func TestAcceptanceSupervisorDisconnectIsFailureWithCleanJoin(t *testing.T) {
	// s выполняет сценарий закрытия выходного UDS после первых дорожек.
	s, _ := acceptanceSupervisor(t, nil)
	// err сообщает об отказе регистрации сценария разрыва UDS.
	if err := s.Start(context.Background(), ffmpeg.Worker{SessionID: "session", GroupID: "group", Owner: newAcceptanceOwner(), Command: acceptanceChildCommand(t, "disconnect", nil), Input: acceptanceBlockInput, Output: acceptanceOutput(nil, nil)}); err != nil {
		t.Fatal(err)
	}
	// r содержит окончательный исход для проверки ошибочного завершения при разрыве UDS.
	r := acceptanceWait(t, s)
	if r.Err == nil {
		t.Fatal("исчезновение UDS объявлено успешным worker")
	}
}

// Проверяет полное сохранение бинарного stderr, порядка фрагментов и буквальных отдельных argv без
// потерь диагностики.
func TestAcceptanceSupervisorDiagnosticsPreserveBytesAndLiteralArgs(t *testing.T) {
	// s выполняет сценарий сохранения полных бинарных данных и буквальных аргументов.
	s, _ := acceptanceSupervisor(t, nil)
	// ready сообщает о получении контрольных дорожек после записи stderr.
	ready := make(chan struct{})
	// build сохраняет построитель контрольного дочернего процесса с бинарной диагностикой.
	build := acceptanceChildCommand(t, "diagnostics", nil)
	// wantArgs сохраняет полный ожидаемый argv, включая исполняемый путь и буквальный синтетический
	// токен.
	var wantArgs []string
	// command дополняет построенную команду буквальным аргументом и сохраняет точное ожидание
	// диагностики.
	command := func(ctx context.Context, a ffmpeg.Attempt) (ffmpeg.Command, error) {
		// Добавляет буквальный синтетический секрет с метасимволами и переводом строки к аргументам и
		// сохраняет точное ожидание диагностики.

		// cmd содержит исходную контрольную команду; err сообщает об отказе её построения до
		// добавления буквального аргумента.
		cmd, err := build(ctx, a)
		cmd.Args = append(cmd.Args, "synthetic-token=секрет; $(literal) `literal`\nnext")
		wantArgs = append([]string{cmd.Binary}, cmd.Args...)
		return cmd, err
	}
	// err сообщает об отказе регистрации сценария полной диагностики.
	if err := s.Start(context.Background(), ffmpeg.Worker{SessionID: "session", GroupID: "group", Owner: newAcceptanceOwner(), Command: command, Input: acceptanceBlockInput, Output: acceptanceOutput(ready, nil)}); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ready:
	case <-time.After(2 * time.Second):
		t.Fatal("процесс не передал проверенный пакет")
	}
	// ctx ограничивает Stop и чтение диагностики двумя секундами; cancel освобождает таймер.
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	// err сообщает об отказе остановки перед проверкой сохранённых диагностических событий.
	if err := s.Stop(ctx); err != nil {
		t.Fatal(err)
	}
	acceptanceWait(t, s)
	// stderr накапливает исходные байты стандартного потока ошибок из прочитанных событий.
	var stderr []byte
	// sequence содержит ожидаемый номер очередного фрагмента stderr.
	var sequence uint64
	// commandSeen отмечает получение события с полной командой запуска.
	commandSeen := false
	for {
		// d содержит очередное событие диагностики; err сообщает об отказе чтения или EOF.
		d, err := s.NextDiagnostic(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if d.Attempt != 1 {
			t.Fatal("диагностика потеряла идентичность попытки")
		}
		switch d.Kind {
		case "command":
			commandSeen = true
			if !reflect.DeepEqual(d.Args, wantArgs) {
				t.Fatalf("аргументы изменены: %q", d.Args)
			}
		case "stderr":
			sequence++
			if d.Sequence != sequence || len(d.Data) > 64 {
				t.Fatalf("нарушена последовательность ограниченных фрагментов: %+v", d)
			}
			stderr = append(stderr, d.Data...)
		}
	}
	// want содержит полную ожидаемую последовательность бинарных байтов stderr.
	want := bytes.Repeat([]byte{0xff, 0, 0xc3, 0xa9, '\n'}, 8192)
	if !commandSeen || !bytes.Equal(stderr, want) || s.DroppedDiagnostics() != 0 {
		t.Fatalf("полные значения потеряны: command=%v bytes=%d want=%d dropped=%d", commandSeen, len(stderr), len(want), s.DroppedDiagnostics())
	}
}

// Проверяет отказ небезопасных сегментов пути и сохранение чужого файла, заменившего сокет, при ошибке
// очистки.
func TestAcceptanceNamespaceRejectsEscapeAndPreservesReplacedEndpoint(t *testing.T) {
	// parent задаёт короткий временный корень для проверки безопасных путей Namespace.
	parent := os.TempDir()
	if runtime.GOOS == "darwin" {
		parent = "/tmp"
	}
	// base содержит корень проверки выхода за путь и подмены сокета; err сохраняет результаты
	// файловых операций.
	base, err := os.MkdirTemp(parent, "gn-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Удаляет временный корень проверки Namespace и сообщает ошибку через t.Error.

		// err сообщает об отказе удаления временного корня проверки Namespace.
		if err := os.RemoveAll(base); err != nil {
			t.Error(err)
		}
	})
	// id содержит очередной небезопасный сегмент пути, который должен быть отвергнут.
	for _, id := range []string{"../escape", "a/b", ".", "..", ""} {
		// n содержит неожиданный Namespace при проверке небезопасного сегмента; err содержит
		// ожидаемый отказ создания.
		if n, err := ffmpeg.NewNamespace(base, id); err == nil {
			_ = n.Close()
			t.Errorf("небезопасный сегмент принят: %q", id)
		}
	}
	// n содержит допустимый Namespace, входной сокет которого будет подменён чужим файлом.
	n, err := ffmpeg.NewNamespace(base, "session")
	if err != nil {
		t.Fatal(err)
	}
	// err сообщает об отказе закрытия дескрипторов перед подменой входного сокета.
	if err := n.Shutdown(); err != nil {
		t.Fatal(err)
	}
	// err сообщает об отказе удаления исходного имени входного сокета.
	if err := os.Remove(n.InputPath); err != nil {
		t.Fatal(err)
	}
	// foreign содержит контрольные байты чужого файла, которые очистка должна сохранить.
	foreign := []byte("чужой ресурс")
	// err сообщает об отказе создания чужого файла вместо входного сокета.
	if err := os.WriteFile(n.InputPath, foreign, 0600); err != nil {
		t.Fatal(err)
	}
	// err содержит ожидаемый ErrResourceOwnership при попытке очистки подменённого сокета.
	if err := n.Close(); !errors.Is(err, ffmpeg.ErrResourceOwnership) {
		t.Fatal("подмена endpoint не обнаружена:", err)
	}
	// got содержит байты чужого файла после очистки для проверки отсутствия удаления или изменений.
	got, err := os.ReadFile(n.InputPath)
	if err != nil || !bytes.Equal(got, foreign) {
		t.Fatal("cleanup удалил или изменил чужой ресурс:", err)
	}
}
