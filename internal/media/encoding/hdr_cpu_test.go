package encoding

import (
	"bytes"
	"context"
	"encoding/binary"
	"os"
	"os/exec"
	"testing"
	"time"
)

// TestCPUHDRConverterPreservesTenBitSamples проверяет реальные PQ/HLG пиксели обоих диапазонов.
// Каждое десятибитное кодовое значение Y проходит рабочий граф без смены размера, глубины или передаточной функции;
// побайтное равенство исключает неявное преобразование яркости, ограничение диапазона и восьмибитный промежуточный формат.
func TestCPUHDRConverterPreservesTenBitSamples(t *testing.T) {
	// executable включает только явно разрешённый локальный FFmpeg.
	executable := os.Getenv("GOST_MEDIA_FFMPEG")
	if executable == "" {
		t.Skip("CPU-проверка требует GOST_MEDIA_FFMPEG")
	}
	// frame содержит Y 32×32 и нейтральные U/V 16×16 в десятибитных, с младшим байтом первым, плоскостях.
	frame := make([]byte, 32*32*3)
	// index задаёт один исходный отсчёт яркости либо цветности.
	for index := 0; index < len(frame)/2; index++ {
		// value перечисляет все 1024 кодовых значения Y, оставляя U/V нейтральными.
		value := uint16(512)
		if index < 1024 {
			value = uint16(index)
		}
		binary.LittleEndian.PutUint16(frame[index*2:], value)
	}
	// transfer и rangeName выбирают каждую разрешённую HDR функцию и диапазон без их преобразования.
	for _, transfer := range []string{"smpte2084", "arib-std-b67"} {
		for _, rangeName := range []string{"limited", "full"} {
			t.Run(transfer+"/"+rangeName, func(t *testing.T) {
				// Пропускает один сырой 10-битный кадр через точный фильтр приложения и сравнивает все отсчёты.
				// format задаёт фактические исходные и выходные HDR-параметры без участия кодировщика или контейнерных тегов.
				format := compilerProfile("h264").Format.Video
				format.Width, format.Height, format.BitDepth, format.PixelFormat = 32, 32, 10, "yuv420p10le"
				format.Color.Primaries, format.Color.Transfer, format.Color.Matrix, format.Color.Range = "bt2020", transfer, "bt2020nc", rangeName
				// filter и err содержат реальный граф сохранения исходного сигнала либо отказ проверки конечных форматов.
				filter, err := SoftwareVideoFilter(format, format, false)
				if err != nil {
					t.Fatal(err)
				}
				// ctx/cancel ограничивают единственный дочерний процесс и освобождают таймер.
				ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
				defer cancel()
				// command передаёт несжатый кадр через stdin и записывает ровно один выходной несжатый кадр.
				command := exec.CommandContext(ctx, executable, "-v", "error", "-nostdin", "-xerror", "-f", "rawvideo", "-pixel_format", "yuv420p10le", "-video_size", "32x32", "-framerate", "30", "-i", "pipe:0", "-vf", filter, "-frames:v", "1", "-pix_fmt", "yuv420p10le", "-f", "rawvideo", "pipe:1")
				// diagnostics сохраняет конечный stderr для разбора исходного отказа FFmpeg.
				var diagnostics bytes.Buffer
				command.Stdin, command.Stderr = bytes.NewReader(frame), &diagnostics
				// output содержит действительные десятибитные пиксели после программного графа.
				output, err := command.Output()
				if err != nil || !bytes.Equal(output, frame) {
					t.Fatalf("десятибитные HDR-отсчёты изменены: байтов=%d, ошибка=%v, %s", len(output), err, diagnostics.String())
				}
			})
		}
	}
}
