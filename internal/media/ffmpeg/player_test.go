package ffmpeg

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"
)

func TestFileCommandUsesSeekableDescriptorCopyAndOneWayOutput(t *testing.T) {
	path := filepath.Join(t.TempDir(), "variant.mp4")
	if err := os.WriteFile(path, []byte("verified fixture"), 0600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = file.Close() }()
	if _, err = file.Seek(5, 0); err != nil {
		t.Fatal(err)
	}
	command, err := BuildFileCommand("/usr/bin/ffmpeg", file, Attempt{ID: 1, OutputPath: "/tmp/output.sock"})
	if err != nil {
		t.Fatal(err)
	}
	offset, err := file.Seek(0, 1)
	if err != nil || offset != 0 {
		t.Fatalf("у дочернего процесса не начало файла: %d %v", offset, err)
	}
	if len(command.ExtraFiles) != 1 || command.ExtraFiles[0] != file {
		t.Fatal("потерян descriptor ownership")
	}
	pair := func(name, value string) bool {
		for i, v := range command.Args {
			if v == name && i+1 < len(command.Args) && command.Args[i+1] == value {
				return true
			}
		}
		return false
	}
	for _, p := range [][2]string{{"-stream_loop", "-1"}, {"-c", "copy"}, {"-fd", "3"}, {"-i", "fd:"}, {"-protocol_whitelist", "fd,unix"}, {"-map", "0:v:0"}, {"-map", "0:a:0"}} {
		if !pair(p[0], p[1]) {
			t.Fatal(command.Args)
		}
	}
	if command.Args[len(command.Args)-1] != "unix:/tmp/output.sock" {
		t.Fatal(command.Args)
	}
	for _, v := range command.Args {
		if v == path || v == "-c:v" {
			t.Fatal("путь переоткрывается либо запускается encoder")
		}
	}
	if !reflect.DeepEqual(command.ExtraFiles, []*os.File{file}) {
		t.Fatal(command.ExtraFiles)
	}
}
func TestFileCommandRejectsNonRegularAndInvalidArguments(t *testing.T) {
	if _, err := BuildFileCommand("ffmpeg", nil, Attempt{}); err == nil {
		t.Fatal("принят nil descriptor")
	}
	dir, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = dir.Close() }()
	if _, err := BuildFileCommand("ffmpeg", dir, Attempt{OutputPath: "/tmp/out"}); err == nil {
		t.Fatal("каталог принят за файл")
	}
	if err = dir.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := BuildFileCommand("ffmpeg", dir, Attempt{OutputPath: "/tmp/out"}); err == nil {
		t.Fatal("закрытый descriptor принят")
	}
}

func TestFileSupervisorOwnsRejectedDescriptorsAndNeedsNoInputHandler(t *testing.T) {
	for _, mode := range []string{"missing", "nil", "directory", "budget", "builder-error", "duplex-file"} {
		t.Run(mode, func(t *testing.T) {
			supervisor := newTestSupervisor(t, supervisorConfig(t))
			var owned *os.File
			switch mode {
			case "directory":
				var err error
				owned, err = os.Open(t.TempDir())
				if err != nil {
					t.Fatal(err)
				}
			case "missing", "nil":
			default:
				var err error
				owned, err = os.CreateTemp(t.TempDir(), "owned")
				if err != nil {
					t.Fatal(err)
				}
			}
			worker := Worker{SessionID: "session", GroupID: "group", Owner: newOwnedPermit(), Topology: FileInput, Output: inertHandler}
			failure := errors.New("построитель отказал после открытия файла")
			worker.Command = func(context.Context, Attempt) (Command, error) {
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
			if err := supervisor.Start(context.Background(), worker); err != nil {
				t.Fatal(err)
			}
			result := waitResult(t, supervisor)
			if result.Err == nil || !result.Clean {
				t.Fatalf("неверный отказ/cleanup: %+v", result)
			}
			if mode == "builder-error" && !errors.Is(result.Err, failure) {
				t.Fatal("потеряна ошибка построителя", result.Err)
			}
			if owned != nil {
				if _, err := owned.Stat(); !errors.Is(err, os.ErrClosed) {
					t.Fatalf("descriptor не закрыт после отказа: %v", err)
				}
			}
		})
	}
}
