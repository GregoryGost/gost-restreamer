package ffmpeg

import (
	"strings"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// TestSoftwareLiveCommandKeepsSelectedEncoderAndTime проверяет программное декодирование и фильтрацию
// для x264 и NVENC: выбранный кодировщик, исходный CICP, полный AAC и прежние временные флаги.
// t проверяет буквальные параметры программного фильтра и выбранного кодировщика.
func TestSoftwareLiveCommandKeepsSelectedEncoderAndTime(t *testing.T) {
	// encoder выбирает программный либо аппаратный H.264 без автоматической замены.
	for _, encoder := range []string{core.VideoEncoderX264, core.VideoEncoderNVENC} {
		// c задаёт старый контрольный AV1-вход и явно новый выходной рецепт.
		c := nvidiaCommandConfig()
		c.Profile.Encoding = core.DefaultEncoding(c.Profile.Mode, encoder, core.RateControlVBR)
		c.Profile.Format.Video.CodecProfile, c.Profile.Format.Video.PixelFormat = "high444", "yuv444p"
		c.Profile.Format.Video.Color = core.ColorInfo{Range: "full", Primaries: "bt2020", Transfer: "gamma22", Matrix: "bt2020nc"}
		// before удерживает исходный профиль; command содержит новый argv, err сообщает об отказе.
		before := c
		command, err := BuildNVIDIACommand(c, Attempt{InputPath: "/tmp/in socket", OutputPath: "/tmp/out socket"})
		if err != nil {
			t.Fatal(err)
		}
		// args содержит внешний контракт команды, независимый от внутренней структуры helpers.
		args := strings.Join(command.Args, " ")
		// want задаёт обязательный параметр кодировщика, конверсию или сохранённую временную семантику.
		for _, want := range []string{"-c:v " + encoder, "primariesin=1:transferin=1:matrixin=1:rangein=limited:primaries=9:transfer=4:matrix=9:range=full", "format=yuv444p", "setparams=range=full", "-flags2:a +skip_manual", "-copyts", "-enc_time_base:v filter", "-avoid_negative_ts disabled", "-use_editlist 1", "async:unix:/tmp/in socket"} {
			if !strings.Contains(args, want) {
				t.Fatal(want, args)
			}
		}
		// forbidden перечисляет неявное аппаратное декодирование и второй регулятор времени.
		for _, forbidden := range []string{"-hwaccel", "av1_cuvid", "scale_cuda", "setpts", "-start_at_zero", "-shortest"} {
			if strings.Contains(args, forbidden) {
				t.Fatal(forbidden, args)
			}
		}
		if c != before || command.Args[len(command.Args)-1] != "unix:/tmp/out socket" {
			t.Fatal("построитель изменил вход или транспорт")
		}
		c.Profile.Encoding.RescaleOutput = false
		c.Profile.Format.Video.Width -= 2
		if _, err = BuildNVIDIACommand(c, Attempt{InputPath: "/in", OutputPath: "/out"}); err == nil {
			t.Fatal("rescale_output=false разрешил изменение размеров")
		}
		c = before
		c.Input.Video.Color.Primaries = "unspecified"
		if _, err = BuildNVIDIACommand(c, Attempt{InputPath: "/in", OutputPath: "/out"}); err == nil {
			t.Fatal("неизвестный sourcecolor заменён целевым")
		}
	}
}
