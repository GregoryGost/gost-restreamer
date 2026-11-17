package ffmpeg

import "os"

// BuildFLACFileCommand строит файловую попытку для уже проверенного FLAC-варианта file.
// binary и a задают прежний supervised FFmpeg/UDS; video копируется, audio декодируется и
// непрерывно кодируется native FLAC/s16 через все петли. Вызывающий подтверждает выбранный
// verified artifact; сама команда не назначает FLACOwned. Supervisor закрывает file как раньше.
func BuildFLACFileCommand(binary string, file *os.File, a Attempt) (Command, error) {
	// command сохраняет прежние FD, loop, pacing и transport; err сообщает об отказе их проверки.
	command, err := BuildFileCommand(binary, file, a)
	if err != nil {
		return Command{}, err
	}
	// index находит единственный прежний выбор копирования обеих дорожек до выходного контейнера.
	for index := 0; index+1 < len(command.Args); index++ {
		if command.Args[index] == "-c" && command.Args[index+1] == "copy" {
			// args заменяет выбор кодеков, сохраняя порядок прочих аргументов и единственный аудиокодировщик.
			args := append([]string(nil), command.Args[:index]...)
			args = append(args, "-c:v", "copy", "-c:a", "flac", "-sample_fmt:a", "s16", "-ar:a", "48000", "-ac:a", "2", "-channel_layout:a", "stereo", "-strict", "-2")
			command.Args = append(args, command.Args[index+2:]...)
			return command, nil
		}
	}
	return Command{}, ErrInvalidSupervisor
}
