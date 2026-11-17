package encoding

import (
	"bytes"
	"context"
	"math"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// TestCPUGrayPlanesAndTransfer использует рабочий converter с синтетической Y-плоскостью.
// Побайтный проход туда и обратно защищает limited/full; независимая формула linear→gamma22
// отличает реальное преобразование передаточной функции от одной смены тегов.
func TestCPUGrayPlanesAndTransfer(t *testing.T) {
	// binary разрешает только явно выбранный локальный FFmpeg.
	binary := os.Getenv("GOST_MEDIA_FFMPEG")
	if binary == "" {
		t.Skip("CPU-проверка требует GOST_MEDIA_FFMPEG")
	}
	// source задаёт известный gray8 BT709; ramp покрывает каждое из 256 значений Y.
	source := compilerProfile(core.CodecAV1).Format.Video
	source.Width, source.Height, source.PixelFormat = 16, 16, "gray"
	ramp := make([]byte, 256)
	// i задаёт исходный код яркости каждой позиции плоскости.
	for i := range ramp {
		ramp[i] = byte(i)
	}
	// rangeName выбирает оба разрешённых диапазона без неявного расширения gray.
	for _, rangeName := range []string{"limited", "full"} {
		source.Color.Range = rangeName
		// filter содержит реальный граф приложения; err сообщает об отказе его сборки.
		filter, err := SoftwareVideoFilter(source, source, false)
		if err != nil {
			t.Fatal(err)
		}
		// got содержит необжатую Y после lift/zscale/extract.
		got := runRawConverter(t, binary, filter, "gray", ramp)
		if !bytes.Equal(got, ramp) {
			t.Fatalf("изменена %s Y", rangeName)
		}
		// color задаёт те же свойства с тремя выходными плоскостями без смены передаточной функции.
		color := source
		color.PixelFormat = "yuv444p"
		filter, err = SoftwareVideoFilter(source, color, false)
		if err != nil {
			t.Fatal(err)
		}
		// lifted содержит Y/U/V после рабочего подъёма gray; U и V должны содержать только 128.
		lifted := runRawConverter(t, binary, filter, "yuv444p", ramp)
		if !bytes.Equal(lifted[:256], ramp) || !bytes.Equal(lifted[256:], bytes.Repeat([]byte{128}, 512)) {
			t.Fatalf("изменены Y или нейтральные U/V для %s", rangeName)
		}
	}
	source.Color.Range = "limited"
	source.Color.Transfer = "linear"
	// target выбирает другую transfer и full range без смены количества плоскостей.
	target := source
	target.Color.Transfer, target.Color.Range = "gamma22", "full"
	// filter содержит граф изменения transfer; err сообщает об отказе сборки.
	filter, err := SoftwareVideoFilter(source, target, false)
	if err != nil {
		t.Fatal(err)
	}
	// got содержит реально конвертированные значения; code выбирает чёрный, полутон и белый.
	got := runRawConverter(t, binary, filter, "gray", ramp)
	for _, code := range []int{16, 80, 126, 180, 235} {
		// linear нормирует Y в ограниченном диапазоне источника с линейной передаточной функцией.
		linear := float64(code-16) / 219
		// want задаёт независимый gamma22 full-range результат с округлением до кода gray8.
		want := math.Round(math.Pow(linear, 1/2.2) * 255)
		if math.Abs(float64(got[code])-want) > 2 {
			t.Fatalf("Y%d: %d, формула %.0f", code, got[code], want)
		}
	}
}

// runRawConverter запускает один кадр gray через filter приложения с конечным сроком. t владеет сроком,
// binary задаёт разрешённый FFmpeg, input — плоскость 16×16, pixel — gray либо yuv444p выхода.
// Результат содержит ровно одну либо три плоскости по 256 байтов без изменения их записи.
func runRawConverter(t *testing.T, binary, filter, pixel string, input []byte) []byte {
	t.Helper()
	// ctx ограничивает дочерний процесс; cancel освобождает его таймер по завершении вызова.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// command передаёт argv напрямую без оболочки; stderr сохраняет конечную диагностику кадра.
	command := exec.CommandContext(ctx, binary, "-v", "error", "-nostdin", "-xerror", "-f", "rawvideo", "-pixel_format", "gray", "-video_size", "16x16", "-framerate", "30", "-i", "pipe:0", "-vf", filter, "-frames:v", "1", "-pix_fmt", pixel, "-f", "rawvideo", "pipe:1")
	var stderr bytes.Buffer
	command.Stdin, command.Stderr = bytes.NewReader(input), &stderr
	// output содержит единственный необжатый кадр; err сообщает об ошибке графа или декодера.
	output, err := command.Output()
	// size задаёт ровно один кадр выбранного тестом плоскостного формата.
	size := 256
	if pixel == "yuv444p" {
		size *= 3
	}
	if err != nil || len(output) != size {
		t.Fatalf("converter: %v, bytes=%d, %s", err, len(output), stderr.String())
	}
	return output
}
