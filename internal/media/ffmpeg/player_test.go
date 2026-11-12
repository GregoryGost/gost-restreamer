package ffmpeg

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

// Проверяет воспроизведение с позиции 0 через переданный FD 3, бесконечную петлю и копирование двух
// дорожек в один UDS без переоткрытия пути или кодирования.
func TestFileCommandUsesSeekableDescriptorCopyAndOneWayOutput(t *testing.T) {
	// path задаёт путь временного контрольного MP4 для проверки чтения через дескриптор.
	path := filepath.Join(t.TempDir(), "variant.mp4")
	// err сообщает об отказе записи контрольного содержимого входного файла.
	if err := os.WriteFile(path, []byte("verified fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	// file содержит открытый входной дескриптор; err сохраняет ошибки его открытия, перемещения
	// позиции и построения команды.
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// Закрывает тестовый файл после проверки построенной команды, игнорируя ошибку.
		_ = file.Close()
	}()
	if _, err = file.Seek(5, 0); err != nil {
		t.Fatal(err)
	}
	// command содержит команду бесконечного копирования файловых дорожек в выходной UDS.
	command, err := BuildFileCommand("/usr/bin/ffmpeg", file, Attempt{ID: 1, OutputPath: "/tmp/output.sock"})
	if err != nil {
		t.Fatal(err)
	}
	// offset содержит текущую позицию дескриптора для проверки возврата к началу файла.
	offset, err := file.Seek(0, 1)
	if err != nil || offset != 0 {
		t.Fatalf("у дочернего процесса не начало файла: %d %v", offset, err)
	}
	if len(command.ExtraFiles) != 1 || command.ExtraFiles[0] != file {
		t.Fatal("потерян descriptor ownership")
	}
	// pair ищет точный флаг и соседнее значение в построенной файловой команде.
	pair := func(name, value string) bool {
		// Ищет точную соседнюю пару аргументов команды для проверки FD, повторения и копирования дорожек.

		// i обозначает позицию аргумента; v содержит его строку при поиске ожидаемой пары.
		for i, v := range command.Args {
			if v == name && i+1 < len(command.Args) && command.Args[i+1] == value {
				return true
			}
		}
		return false
	}
	// p содержит очередную ожидаемую пару параметров файлового воспроизведения.
	for _, p := range [][2]string{{"-stream_loop", "-1"}, {"-c", "copy"}, {"-fd", "3"}, {"-i", "fd:"}, {"-protocol_whitelist", "fd,unix"}, {"-map", "0:v:0"}, {"-map", "0:a:0"}} {
		if !pair(p[0], p[1]) {
			t.Fatal(command.Args)
		}
	}
	if command.Args[len(command.Args)-1] != "unix:/tmp/output.sock" {
		t.Fatal(command.Args)
	}
	// v содержит очередной аргумент для проверки отсутствия исходного пути и отдельного
	// видеокодировщика.
	for _, v := range command.Args {
		if v == path || v == "-c:v" {
			t.Fatal("путь переоткрывается либо запускается encoder")
		}
	}
	if !reflect.DeepEqual(command.ExtraFiles, []*os.File{file}) {
		t.Fatal(command.ExtraFiles)
	}
}

// Проверяет отказ для nil, каталога и уже закрытого входного дескриптора.
func TestFileCommandRejectsNonRegularAndInvalidArguments(t *testing.T) {
	// err содержит ожидаемый отказ команды с отсутствующим входным дескриптором.
	if _, err := BuildFileCommand("ffmpeg", nil, Attempt{}); err == nil {
		t.Fatal("принят nil descriptor")
	}
	// dir содержит дескриптор каталога для проверки запрета нештатного входа; err сообщает об отказе
	// его открытия или закрытия.
	dir, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// Закрывает дескриптор каталога после проверки отказа, игнорируя в том числе повторное закрытие.
		_ = dir.Close()
	}()
	// err содержит ожидаемый отказ принять каталог за входной MP4.
	if _, err := BuildFileCommand("ffmpeg", dir, Attempt{OutputPath: "/tmp/out"}); err == nil {
		t.Fatal("каталог принят за файл")
	}
	if err = dir.Close(); err != nil {
		t.Fatal(err)
	}
	// err содержит ожидаемый отказ принять уже закрытый входной дескриптор.
	if _, err := BuildFileCommand("ffmpeg", dir, Attempt{OutputPath: "/tmp/out"}); err == nil {
		t.Fatal("закрытый descriptor принят")
	}
}

// Проверяет закрытие переданных ExtraFiles при отказе построителя, бюджета или топологии и возможность
// FileInput без входного обработчика.
func TestFileSupervisorOwnsRejectedDescriptorsAndNeedsNoInputHandler(t *testing.T) {
	// mode задаёт проверяемое нарушение файловых дескрипторов, бюджета или результата построителя.
	for _, mode := range []string{"missing", "nil", "directory", "budget", "builder-error", "duplex-file"} {
		t.Run(mode, func(t *testing.T) {
			// Проверяет выбранный отказ передачи ExtraFiles или построителя и требует закрытия принадлежащего
			// попытке дескриптора.

			// supervisor выполняет выбранный сценарий отказа передачи ExtraFiles.
			supervisor := newTestSupervisor(t, supervisorConfig(t))
			// owned содержит дескриптор, который попытка должна закрыть даже при отказе
			// построения или проверки команды.
			var owned *os.File
			switch mode {
			case "directory":
				// err сообщает об отказе открытия каталога, передаваемого как
				// неподходящий ExtraFiles.
				var err error
				owned, err = os.Open(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
			case "missing", "nil":
			default:
				// err сообщает об отказе создания временного файла, передаваемого во
				// владение попытки.
				var err error
				owned, err = os.CreateTemp(t.TempDir(), "owned")
				if err != nil {
					t.Fatal(err)
				}
			}
			// worker задаёт файловую топологию с проверяемыми ExtraFiles и инертным выходным
			// обработчиком.
			worker := Worker{SessionID: "session", GroupID: "group", Owner: newOwnedPermit(), Topology: FileInput, Output: inertHandler}
			// failure задаёт контрольный отказ построителя после открытия входного файла.
			failure := errors.New("построитель отказал после открытия файла")
			worker.Command = func(context.Context, Attempt) (Command, error) {
				// Возвращает команду с выбранным нарушением ExtraFiles или бюджета; в сценарии builder-error сохраняет
				// дескриптор вместе с ошибкой построителя.

				// command содержит намеренно неполную или некорректную команду для
				// выбранного сценария отказа.
				command := Command{Binary: "/usr/bin/false"}
				if mode != "missing" {
					command.ExtraFiles = []*os.File{owned}
				}
				if mode == "budget" {
					command.Args = make([]string, 100)
				}
				if mode == "builder-error" {
					return command, failure
				}
				return command, nil
			}
			if mode == "duplex-file" {
				worker.Topology = Duplex
				worker.Input = inertHandler
			}
			// err сообщает об отказе регистрации тестового работника в супервизоре.
			if err := supervisor.Start(context.Background(), worker); err != nil {
				t.Fatal(err)
			}
			// result содержит окончательный отказ и признак очистки после отклонённой
			// файловой команды.
			result := waitResult(t, supervisor)
			if result.Err == nil || !result.Clean {
				t.Fatalf("неверный отказ/cleanup: %+v", result)
			}
			if mode == "builder-error" && !errors.Is(result.Err, failure) {
				t.Fatal("потеряна ошибка построителя", result.Err)
			}
			if owned != nil {
				// err содержит ожидаемый os.ErrClosed при проверке освобождённого
				// дескриптора.
				if _, err := owned.Stat(); !errors.Is(err, os.ErrClosed) {
					t.Fatalf("descriptor не закрыт после отказа: %v", err)
				}
			}
		})
	}
}
