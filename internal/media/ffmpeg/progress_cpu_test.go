package ffmpeg

import (
	"bytes"
	"context"
	"io"
	"net"
	"os"
	"os/exec"
	"path/filepath"
	"slices"
	"strings"
	"testing"
	"time"
)

func TestFileCommandCPUProgress(t *testing.T) {
	binary := os.Getenv("GOST_MEDIA_FFMPEG")
	if binary == "" {
		t.Skip("короткая CPU-проверка требует явного GOST_MEDIA_FFMPEG")
	}
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	version, err := exec.CommandContext(ctx, binary, "-version").Output()
	if err != nil || !strings.HasPrefix(string(version), "ffmpeg version 7.1.5") {
		t.Fatalf("требуется FFmpeg 7.1.5: %v %s", err, version)
	}
	path := filepath.Join(t.TempDir(), "cpu.mp4")
	generate := exec.CommandContext(ctx, binary, "-hide_banner", "-nostdin", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=64x64:rate=10", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
		"-t", "0.5", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac", "-movflags", "+faststart", path)
	if output, err := generate.CombinedOutput(); err != nil {
		t.Fatalf("создание изолированного CPU-файла: %v %s", err, output)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	outputPath := filepath.Join(shortBase(t), "cpu.sock")
	listener, err := net.Listen("unix", outputPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = listener.Close() })
	stopListener := context.AfterFunc(ctx, func() { _ = listener.Close() })
	defer stopListener()
	drained := make(chan int64, 1)
	go func() {
		conn, err := listener.Accept()
		if err != nil {
			drained <- 0
			return
		}
		defer func() { _ = conn.Close() }()
		stopConn := context.AfterFunc(ctx, func() { _ = conn.Close() })
		defer stopConn()
		n, _ := io.Copy(io.Discard, conn)
		drained <- n
	}()
	command, err := BuildFileCommand(binary, file, Attempt{ID: 1, OutputPath: outputPath})
	if err != nil {
		t.Fatal(err)
	}
	// Ограничение относится только к синтетическому тесту: штатный player повторяет файл.
	index := slices.Index(command.Args, "-stream_loop")
	command.Args[index+1] = "0"
	store := &progressStore{}
	store.begin(1)
	parser := newProgressParser(parserConfig(), store, 1)
	queue := newDiagnosticQueue(256, 1<<20)
	stdout := &diagnosticWriter{queue: queue, attempt: 1, kind: "stdout", chunk: 256, progress: parser}
	cmd := exec.CommandContext(ctx, command.Binary, command.Args...)
	cmd.ExtraFiles, cmd.Stdout = command.ExtraFiles, stdout
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err = cmd.Run(); err != nil {
		t.Fatalf("штатный FD/UDS player с progress: %v %s", err, stderr.Bytes())
	}
	parser.finish()
	select {
	case n := <-drained:
		if n == 0 {
			t.Fatal("UDS не передал медиабайты")
		}
	case <-ctx.Done():
		t.Fatal("UDS не завершил чтение", ctx.Err())
	}
	s := store.snapshot()
	if !s.HasReport || !s.Report.End || !s.Report.HasFrame || s.Report.Frame != 5 || !s.Report.HasOutTime || s.DroppedReports != 0 || queue.losses() != 0 {
		t.Fatalf("неверный CLI progress для пяти кадров: %+v; %s", s, stderr.Bytes())
	}
}
