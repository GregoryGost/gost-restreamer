package ffmpeg

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

// BuildFileCommand воспроизводит готовый MP4 с поддержкой поиска по файлу без кодирования.
// После возврата построителя команды Supervisor владеет file до полного завершения попытки.
func BuildFileCommand(binary string, file *os.File, a Attempt) (Command, error) {
	if file == nil || !validCommand(binary, nil, 4096) || !filepath.IsAbs(a.OutputPath) || strings.ContainsRune(a.OutputPath, 0) {
		return Command{}, ErrInvalidSupervisor
	}
	info, err := file.Stat()
	if err != nil {
		return Command{}, err
	}
	if !info.Mode().IsRegular() {
		return Command{}, fmt.Errorf("%w: нужен обычный файл", ErrInvalidSupervisor)
	}
	if _, err = file.Seek(0, 0); err != nil {
		return Command{}, err
	}
	return Command{Binary: binary, ExtraFiles: []*os.File{file}, Args: []string{"-hide_banner", "-nostdin", "-loglevel", "warning", "-xerror", "-stream_loop", "-1", "-re", "-protocol_whitelist", "fd,unix", "-format_whitelist", "mov,mp4,m4a,3gp,3g2,mj2", "-f", "mp4", "-fd", "3", "-i", "fd:", "-map", "0:v:0", "-map", "0:a:0", "-c", "copy", "-f", "mp4", "-movflags", "delay_moov+default_base_moof+frag_every_frame", "-min_frag_duration", "1", "-use_editlist", "1", "-avoid_negative_ts", "disabled", "unix:" + a.OutputPath}}, nil
}
