package fallback

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"os"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
)

// externalSourceDump записывает data в ограниченную форму дампа ffprobe с точными смещениями.
// Это лишь представление переданных тестом байтов, без внешнего процесса или чтения пользовательских файлов.
func externalSourceDump(data []byte) string {
	// dump накапливает отдельные строки не более чем по 16 исходных байтов.
	var dump bytes.Buffer
	// offset задаёт начало очередной строки дампа.
	for offset := 0; offset < len(data); offset += 16 {
		// bytes.Buffer не возвращает ошибку записи; результат форматирования не влияет на размер исходных данных.
		_, _ = fmt.Fprintf(&dump, "%08x: %s  .\n", offset, hex.EncodeToString(data[offset:min(offset+16, len(data))]))
	}
	return dump.String()
}

// externalSourceMOV создаёт временный moov с одной AV1 записью headers и необязательным nclx color.
// t закрывает только свой файл; сжатые пакеты передаёт изолированный исполнитель команд теста SourceHDR.
func externalSourceMOV(t *testing.T, headers, color []byte) *os.File {
	t.Helper()
	// entry объединяет фиксированную видеозапись, исходный av1C и настоящий внешний nclx.
	entry := append(make([]byte, 78), sourceTestBox("av1C", headers)...)
	if color != nil {
		entry = append(entry, sourceTestBox("colr", color)...)
	}
	// body оборачивает одну запись в прежний фиксированный путь ограниченного читателя.
	body := sourceTestBox("stsd", append([]byte{0, 0, 0, 0, 0, 0, 0, 1}, sourceTestBox("av01", entry)...))
	// kind задаёт следующий внешний уровень контейнера, без таблиц сжатых пакетов.
	for _, kind := range []string{"stbl", "minf", "mdia", "trak", "moov"} {
		body = sourceTestBox(kind, body)
	}
	// file принимает байты собственного тестового MOV; err сохраняет отказ создания файла.
	file, err := os.CreateTemp(t.TempDir(), "external-source")
	if err != nil {
		t.Fatal(err)
	}
	selfClose(t, file)
	if _, err = file.Write(body); err != nil {
		t.Fatal(err)
	}
	return file
}

// TestSourceExternalCICPRequiresActualNCLX проходит Source и единственный проход SourceHDR по сжатым пакетам.
// t проверяет, что метки ffprobe либо признак происхождения ранее переданного source без nclx не заменяют дескриптор;
// кодированные диапазон/глубина/субдискретизация и известные CICP остаются обязательными.
func TestSourceExternalCICPRequiresActualNCLX(t *testing.T) {
	// headers содержит синтаксически полный main10 av1C с явными unspecified CICP; err сохраняет отказ hex.
	headers, err := hex.DecodeString("81004c000a160400000004000000f28000002627fb38001408080810")
	if err != nil {
		t.Fatal(err)
	}
	// payload содержит ту же последовательность и минимальный показанный KEY_FRAME для сжатого анализа.
	payload := append(bytes.Clone(headers[4:]), 0x1a, 1, 0x10)
	// coded подтверждает три unspecified оси, глубину и геометрию до проверки внешнего цвета; err сохраняет отказ.
	coded, _, err := codec.InspectAV1TU(headers, payload, core.FrameRate{Numerator: 30, Denominator: 1})
	if err != nil || coded.Color.Primaries != "unspecified" || coded.Color.Transfer != "unspecified" || coded.Color.Matrix != "unspecified" || coded.BitDepth != 10 {
		t.Fatal(coded, err)
	}
	// stream содержит фактические метки внешнего HDR, извлечённые выбранным ffprobe-портом.
	stream := probeStream{Type: "video", Codec: "av1", Profile: "Main", Width: coded.Width, Height: coded.Height, Rate: "30/1", Pixel: "yuv420p10le", Level: 0, ColorRange: "tv", Primaries: "bt2020", Transfer: "smpte2084", Matrix: "bt2020nc", StartTime: "0", ExtraData: externalSourceDump(headers), ExtraDataSize: uint32(len(headers))}
	if _, err = stream.video(); err == nil {
		t.Fatal("video() перестал строго проверять исходный av1C")
	}
	// color является реальным nclx с PQ/BT.2020 и обязательным limited range.
	color := []byte{'n', 'c', 'l', 'x', 0, 9, 0, 16, 0, 9, 0}
	// file содержит внешние байты, которыми подтверждается право уточнить unspecified CICP.
	file := externalSourceMOV(t, headers, color)
	// document предоставляет Source формат той же выбранной дорожки без желаемого профиля.
	document := probeDocument{Streams: []probeStream{stream}}
	document.Format.Name, document.Format.Duration = "mov,mp4,m4a,3gp,3g2,mj2", "1"
	// encoded передаёт runner точный JSON наблюдаемых полей; err проверяет сериализацию тестовых данных.
	encoded, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	// prober использует настоящий ограниченный читатель moov и управляемый исполнитель команд, не запуская FFmpeg.
	prober := Prober{Binary: "probe", Runner: probeJSONRunner{data: encoded}, Limits: selfProbeLimits()}
	// source должен принять реальные nclx сведения уже на раннем пути Source().
	source, err := prober.Source(context.Background(), file, core.AssetVideo)
	if err != nil || source.Format.Video.Color != codec.CICPColor(9, 16, 9, false) || source.HDR.ColorConfig.Source != media.HDRSourceContainer {
		t.Fatal(source, err)
	}
	// incomplete сохраняет реальный HDR transfer, но не сообщает primaries/matrix: их подтверждает только nclx.
	incomplete := stream
	incomplete.Primaries, incomplete.Matrix = "unspecified", ""
	// completed получает отсутствующие индексы из реального файла; completeErr сохраняет отказ этого уточнения.
	completed, _, completeErr := prober.sourceVideoFormat(context.Background(), file, incomplete)
	if completeErr != nil || completed.Color != source.Format.Video.Color {
		t.Fatal(completed, completeErr)
	}
	// scan описывает тот же выбранный stream и один синтаксически полный сжатый пакет.
	scan := map[string]any{
		"streams": []probeStream{stream},
		"packets": []map[string]any{{"data": externalSourceDump(payload), "size": fmt.Sprint(len(payload)), "flags": "K"}},
	}
	encoded, err = json.Marshal(scan)
	if err != nil {
		t.Fatal(err)
	}
	prober.Runner = probeJSONRunner{data: encoded}
	// verified должен сохранить только реальное происхождение CICP без выдуманных MDCV/CLL.
	verified, err := prober.SourceHDR(context.Background(), file, source)
	if err != nil || !verified.HDRVerified || verified.HDR.ColorConfig != source.HDR.ColorConfig || verified.HDR.MDCV.Present || verified.HDR.CLL.Present {
		t.Fatal(verified, err)
	}
	// missing сохраняет исходные заголовки при полном отсутствии внешнего nclx.
	missing := externalSourceMOV(t, headers, nil)
	if _, err = prober.SourceHDR(context.Background(), missing, source); err == nil {
		t.Fatal("Прежний признак происхождения или метки ffprobe заменили отсутствующий nclx")
	}
	// bad выбирает одно противоречие ограничениям заголовков кодека либо реальному внешнему дескриптору.
	for _, bad := range []struct {
		// name обозначает ось; обработчик change меняет только метки переданной копии s описания stream.
		name   string
		change func(*probeStream)
	}{
		{"range", func(s *probeStream) { s.ColorRange = "pc" }},
		{"depth", func(s *probeStream) { s.Pixel = "yuv420p" }},
		{"chroma", func(s *probeStream) { s.Pixel = "yuv444p10le" }},
		{"labels", func(s *probeStream) { s.Primaries = "bt709" }},
	} {
		// changed содержит одно независимое противоречие с теми же реальными байтами файла.
		changed := stream
		bad.change(&changed)
		if _, _, err = prober.sourceVideoFormat(context.Background(), file, changed); err == nil {
			t.Fatal("Принято противоречие", bad.name)
		}
	}
	// knownHeaders задаёт кодированный BT.709 вместо unspecified, сохраняя тот же nclx PQ/BT.2020.
	knownHeaders, err := hex.DecodeString("81004c000a160400000004000000f28000002627fb38001404040410")
	if err != nil {
		t.Fatal(err)
	}
	// knownFile и knownStream относятся к одним байтам, но конфликтуют по известным CICP заголовков кодека.
	knownFile := externalSourceMOV(t, knownHeaders, color)
	knownStream := stream
	knownStream.ExtraData = externalSourceDump(knownHeaders)
	if _, _, err = prober.sourceVideoFormat(context.Background(), knownFile, knownStream); err == nil {
		t.Fatal("nclx изменил известные кодированные CICP")
	}
}
