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

// При включённом FFmpeg 7.1.5 проверяет настоящий FD/UDS-проигрыватель на пяти кадрах с полным
// завершающим progress без потерь отчёта и диагностики.
func TestFileCommandCPUProgress(t *testing.T) {
	// binary содержит явно выбранный через GOST_MEDIA_FFMPEG исполняемый файл CPU-проверки.
	binary := os.Getenv("GOST_MEDIA_FFMPEG")
	if binary == "" {
		t.Skip("короткая CPU-проверка требует явного GOST_MEDIA_FFMPEG")
	}
	// ctx ограничивает создание файла и его воспроизведение пятнадцатью секундами; cancel
	// освобождает таймер.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// version содержит вывод версии FFmpeg; err сохраняет результаты запуска команд и файловых
	// операций сценария.
	version, err := exec.CommandContext(ctx, binary, "-version").Output()
	if err != nil || !strings.HasPrefix(string(version), "ffmpeg version 7.1.5") {
		t.Fatalf("требуется FFmpeg 7.1.5: %v %s", err, version)
	}
	// path задаёт временный MP4 с пятью кадрами для проверки настоящего CLI progress.
	path := filepath.Join(t.TempDir(), "cpu.mp4")
	// generate создаёт короткий синтетический H.264/AAC-файл на CPU.
	generate := exec.CommandContext(ctx, binary, "-hide_banner", "-nostdin", "-loglevel", "error",
		"-f", "lavfi", "-i", "testsrc2=size=64x64:rate=10", "-f", "lavfi", "-i", "sine=frequency=440:sample_rate=48000",
		"-t", "0.5", "-c:v", "libx264", "-preset", "ultrafast", "-pix_fmt", "yuv420p", "-c:a", "aac", "-movflags", "+faststart", path)
	// output содержит полную диагностику генерации файла; err сообщает об отказе её выполнения.
	if output, err := generate.CombinedOutput(); err != nil {
		t.Fatalf("создание изолированного CPU-файла: %v %s", err, output)
	}
	// file содержит открытый синтетический MP4 для передачи проигрывателю через FD 3.
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает входной файл CPU-сценария после теста, игнорируя ошибку.
		_ = file.Close()
	})
	// outputPath задаёт короткий путь UDS, принимающего медиавыход FFmpeg.
	outputPath := filepath.Join(shortBase(t), "cpu.sock")
	// listener принимает единственное выходное соединение проигрывателя.
	listener, err := net.Listen("unix", outputPath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает выходной слушатель UDS после теста, игнорируя ошибку.
		_ = listener.Close()
	})
	// stopListener снимает обработчик отмены, закрывающий UDS-слушатель при истечении ctx.
	stopListener := context.AfterFunc(ctx, func() {
		// Закрывает слушатель при отмене контекста, прерывая ожидание Accept; ошибка игнорируется.
		_ = listener.Close()
	})
	defer stopListener()
	// drained передаёт число прочитанных из выходного UDS медиабайтов.
	drained := make(chan int64, 1)
	go func() {
		// Принимает выход FFmpeg и считает прочитанные медиабайты; ошибка Accept даёт ноль, ошибка дочитывания
		// не проверяется.

		// conn содержит принятое выходное соединение; err сообщает об отказе его приёма.
		conn, err := listener.Accept()
		if err != nil {
			drained <- 0
			return
		}
		defer func() {
			// Закрывает принятое соединение после дочитывания, игнорируя ошибку.
			_ = conn.Close()
		}()
		// stopConn снимает обработчик отмены, закрывающий принятое соединение при истечении ctx.
		stopConn := context.AfterFunc(ctx, func() {
			// Закрывает принятое соединение при отмене контекста, прерывая чтение; ошибка игнорируется.
			_ = conn.Close()
		})
		defer stopConn()
		// n содержит число медиабайтов, дочитанных из выходного соединения.
		n, _ := io.Copy(io.Discard, conn)
		drained <- n
	}()
	// command содержит стандартную файловую команду, ограничиваемую в тесте одним проходом.
	command, err := BuildFileCommand(binary, file, Attempt{ID: 1, OutputPath: outputPath})
	if err != nil {
		t.Fatal(err)
	}
	// Ограничение относится только к синтетическому тесту: штатный player повторяет файл.
	// index обозначает позицию -stream_loop для отключения бесконечного повтора только в контрольном
	// запуске.
	index := slices.Index(command.Args, "-stream_loop")
	command.Args[index+1] = "0"
	// store хранит progress единственной CPU-попытки.
	store := &progressStore{}
	store.begin(1)
	// parser разбирает настоящий stdout FFmpeg с тестовыми пределами progress.
	parser := newProgressParser(parserConfig(), store, 1)
	// queue хранит полные фрагменты диагностики CPU-проигрывателя.
	queue := newDiagnosticQueue(256, 1<<20)
	// stdout одновременно сохраняет стандартный вывод и передаёт его парсеру progress.
	stdout := &diagnosticWriter{queue: queue, attempt: 1, kind: "stdout", chunk: 256, progress: parser}
	// cmd выполняет файловую команду с FD 3 и захватом обоих потоков вывода.
	cmd := exec.CommandContext(ctx, command.Binary, command.Args...)
	cmd.ExtraFiles, cmd.Stdout = command.ExtraFiles, stdout
	// stderr сохраняет диагностику ошибок FFmpeg для сообщения при отказе проверки.
	var stderr bytes.Buffer
	cmd.Stderr = &stderr
	if err = cmd.Run(); err != nil {
		t.Fatalf("штатный FD/UDS player с progress: %v %s", err, stderr.Bytes())
	}
	parser.finish()
	select {
	// n содержит итоговое число медиабайтов из завершившегося читателя UDS.
	case n := <-drained:
		if n == 0 {
			t.Fatal("UDS не передал медиабайты")
		}
	case <-ctx.Done():
		t.Fatal("UDS не завершил чтение", ctx.Err())
	}
	// s содержит окончательный снимок progress для проверки пяти кадров, времени и отсутствия
	// потерь.
	s := store.snapshot()
	if !s.HasReport || !s.Report.End || !s.Report.HasFrame || s.Report.Frame != 5 || !s.Report.HasOutTime || s.DroppedReports != 0 || queue.losses() != 0 {
		t.Fatalf("неверный CLI progress для пяти кадров: %+v; %s", s, stderr.Bytes())
	}
}
