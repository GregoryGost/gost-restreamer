package fallback

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// TestExpandedPreparationRequiresKnownSourceColor проверяет через t отказ AOM для изображений
// без подтверждённого цвета и допуск ограниченного SVT-рендеринга. Подготовка масштабирует source,
// поскольку rescale_output управляет только LIVE, включая обязательное false у AV1 copy.
func TestExpandedPreparationRequiresKnownSourceColor(t *testing.T) {
	// p задаёт новый AOM-профиль; imageRequest содержит PNG без фактического цветового описания.
	p := selfProfile(core.CodecAV1)
	p.LiveAudioMode = core.LiveAudioCopy
	p.Encoding = core.DefaultEncoding(p.Mode, core.VideoEncoderAOMAV1, core.RateControlVBR)
	p.VideoEncoding.MaxBitrateBPS, p.VideoEncoding.BufferBits = 0, 0
	imageRequest := GenerateRequest{Source: Source{Kind: core.AssetImage, Duration: time.Second, Demuxer: "png_pipe"}, Profile: p, LoopDuration: 1600 * time.Millisecond}
	// err сохраняет отказ подготовки изображения с неподтверждённым цветом через AOM.
	if _, err := ProductionBuilder("ffmpeg")(imageRequest); err == nil {
		t.Fatal("новый image recipe выдумал source color")
	}
	imageRequest.Profile = selfProfile(core.CodecAV1)
	if _, err := ProductionBuilder("ffmpeg")(imageRequest); err != nil {
		// err сообщает об отказе построения допустимого ограниченного SVT-рендерера.
		t.Fatal("изменён прежний image recipe", err)
	}
	// source задаёт известное видео другого размера без подстановки desired в фактический цвет.
	source := selfProfile(core.CodecAV1).Format.Video
	source.Width = 256
	// request содержит новый видеоисточник; command — построенную команду подготовки;
	// err сообщает об отказе её построения.
	request := GenerateRequest{Source: Source{Kind: core.AssetVideo, Duration: time.Second, Demuxer: "mov", Format: core.SourceFormat{Video: &source}}, Profile: p, LoopDuration: 1600 * time.Millisecond}
	command, err := ProductionBuilder("ffmpeg")(request)
	if err != nil || !strings.Contains(strings.Join(command.Args, " "), "zscale=w=128:h=128") {
		t.Fatal(command, err)
	}
	source.Color.Transfer = "unspecified"
	if _, err = ProductionBuilder("ffmpeg")(request); err == nil {
		t.Fatal("неизвестный transfer принят")
	}
}

// TestExpandedProbeRequiresHeadersAndCanonicalFullRange проверяет строгий допуск extradata
// у новых профилей и точное отображение восьмибитных yuvj-плоскостей с отдельным full range.
// t сообщает об ошибках проверяемого сценария.
func TestExpandedProbeRequiresHeadersAndCanonicalFullRange(t *testing.T) {
	// prober и file задают контрольный файл с намеренно неполным описанием codec extradata.
	prober := Prober{Binary: "probe", Runner: probeJSONRunner{data: []byte(strings.ReplaceAll(selfProbeJSON, `"extradata_size":47`, `"extradata_size":0`))}, Limits: selfProbeLimits()}
	file := selfFragmented(t, "valid")
	// err сообщает о принятии новым путём неподтверждённых видеозаголовков.
	if _, err := prober.observedFormat(context.Background(), file); err == nil {
		t.Fatal("отсутствующие headers приняты новым профилем")
	}
	// err подтверждает, что повторное чтение не ослабляет обязательность заголовков.
	if _, err := prober.observedFormat(context.Background(), file); err == nil {
		t.Fatal("Повторное измерение пропустило отсутствующие headers")
	}
	// pixel и canonical сопоставляют только реальные одинаковые плоскости, сохраняя диапазон отдельно.
	for pixel, canonical := range map[string]string{"yuvj420p": "yuv420p", "yuvj422p": "yuv422p", "yuvj444p": "yuv444p"} {
		// stream задаёт фактическое описание ffprobe; actual — канонические плоскости с full range;
		// err сообщает об отказе отображения наблюдаемого формата.
		stream := probeStream{Codec: "h264", Profile: "High 4:4:4 Predictive", Width: 128, Height: 128, Rate: "30/1", Pixel: pixel, ColorRange: "pc", Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"}
		actual, err := stream.video()
		if err != nil || actual.PixelFormat != canonical || actual.Color.Range != "full" || actual.BitDepth != 8 {
			t.Fatal(actual, err)
		}
		stream.ColorRange = "tv"
		if _, err = stream.video(); err == nil {
			t.Fatal("yuvj без full range принят")
		}
	}
}
