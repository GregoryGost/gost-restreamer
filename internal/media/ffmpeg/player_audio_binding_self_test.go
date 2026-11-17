package ffmpeg

import (
	"os"
	"strings"
	"testing"
)

// TestFLACFilePlayerKeepsFDAndCopiesVideo проверяет один непрерывный аудиокодировщик через петли,
// прежнее владение FD и отсутствие изменений старого copy-рецепта. t владеет временным файлом.
func TestFLACFilePlayerKeepsFDAndCopiesVideo(t *testing.T) {
	// file содержит собственный seekable дескриптор; err сообщает об отказе создания или команды.
	file, err := os.CreateTemp(t.TempDir(), "verified.mp4")
	if err != nil {
		t.Fatal(err)
	}
	// Закрытие тестового file проверяется после всех построителей, не передавая его Supervisor.
	t.Cleanup(func() {
		// closeErr сообщает об отказе освобождения тестового дескриптора.
		if closeErr := file.Close(); closeErr != nil {
			t.Error(closeErr)
		}
	})
	// attempt задаёт тот же выходной UDS для нового и прежнего файлового рецепта.
	attempt := Attempt{OutputPath: "/tmp/audio-player.sock"}
	// command содержит новый FLAC-рецепт с прежним FD; args упрощает проверку независимых флагов.
	command, err := BuildFLACFileCommand("ffmpeg", file, attempt)
	args := strings.Join(command.Args, " ")
	if err != nil || len(command.ExtraFiles) != 1 || command.ExtraFiles[0] != file || strings.Count(args, "-c:a flac") != 1 {
		t.Fatal(command, err)
	}
	// want перечисляет сохранённые параметры попытки и явный encoder без второго codec override.
	for _, want := range []string{"-stream_loop -1", "-re", "-fd 3 -i fd:", "-c:v copy", "-c:a flac", "-sample_fmt:a s16", "-strict -2", "unix:/tmp/audio-player.sock"} {
		if !strings.Contains(args, want) {
			t.Fatal(want, args)
		}
	}
	// legacy должен сохранить единственный прежний выбор копирования обеих дорожек.
	legacy, err := BuildFileCommand("ffmpeg", file, attempt)
	if err != nil || !strings.Contains(strings.Join(legacy.Args, " "), "-c copy") || strings.Contains(strings.Join(legacy.Args, " "), "-c:a") {
		t.Fatal(legacy, err)
	}
}
