package fallback

import (
	"bytes"
	"context"
	"encoding/json"
	"image"
	"image/png"
	"io"
	"os"
	"path/filepath"
	"slices"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

// При включённом FFmpeg проверяет длительность кадров по самому fMP4 на контрольном файле с грубой
// шкалой, где синтетическое duration ffprobe отличается, и сохранение пакетов при нормализации.
func TestCPUPreparedDurationProofUsesNativeSamplesInsteadOfProbeFill(t *testing.T) {
	// binary содержит явно включённый путь FFmpeg; пустая строка пропускает CPU-проверку.
	binary := os.Getenv("GOST_MEDIA_FFMPEG")
	if binary == "" {
		t.Skip("локальная CPU-проверка требует GOST_MEDIA_FFMPEG")
	}
	// source содержит временный PNG для реального кодирования. err сообщает об отказе его создания.
	source, err := os.CreateTemp(t.TempDir(), "source.png")
	if err != nil {
		t.Fatal(err)
	}
	selfClose(t, source)
	if err = png.Encode(source, image.NewRGBA(image.Rect(0, 0, 128, 128))); err != nil {
		t.Fatal(err)
	}
	// profile содержит профиль H.264 с дробной частотой и двумя допустимыми B-кадрами.
	profile := selfProfile(core.CodecH264)
	profile.Format.Video.FrameRate = core.FrameRate{Numerator: 25, Denominator: 2}
	profile.VideoEncoding.MaxBFrames = 2
	// loop задаёт ожидаемую длину подготовленного периода 1,6 с.
	loop := 1600 * time.Millisecond
	// command содержит CPU-команду, чья видеошкала намеренно заменяется грубой контрольной шкалой.
	command, err := buildCommand(binary, GenerateRequest{Source: Source{Kind: core.AssetImage, Duration: loop, Demuxer: "png_pipe"}, Profile: profile, LoopDuration: loop}, "libx264")
	if err != nil {
		t.Fatal(err)
	}
	// Намеренно грубая шкала создаёт отдельную контрольную фикстуру FFmpeg n7.1.5.
	command.Args[slices.Index(command.Args, "-video_track_timescale")+1] = "25"
	if _, err = source.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	// raw содержит временный ненормализованный MP4.
	raw, err := os.CreateTemp(t.TempDir(), "raw.mp4")
	if err != nil {
		t.Fatal(err)
	}
	selfClose(t, raw)
	// runner выполняет реальные команды с конечными тестовыми бюджетами.
	runner := selfRunner()
	// result содержит диагностику кодирования контрольного файла. e сообщает об отказе команды.
	if result, e := runner.Run(context.Background(), command, source, &boundedWriter{w: raw, remaining: 8 << 20}); e != nil {
		t.Fatal(e, result.Diagnostics)
	}
	if _, err = raw.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	// demux читает фактические длительности пакетов из контрольного fMP4.
	demux, err := fmp4.NewDemuxer(raw, fmp4.DemuxOptions{Limits: selfProbeLimits().Container, SourceEpoch: 1, VideoRevision: 1, AudioRevision: 1})
	if err != nil {
		t.Fatal(err)
	}
	for {
		// packet содержит очередной пакет контрольного файла. e сообщает об отказе его чтения.
		packet, e := demux.Next()
		if e != nil {
			t.Fatal(e)
		}
		if packet.Config().Info().Track != media.TrackVideo {
			continue
		}
		if packet.Info().DurationTicks != 2 || packet.Config().Info().TimeBase != (media.TimeBase{Numerator: 1, Denominator: 25}) {
			t.Fatal(packet.Info(), packet.Config().Info())
		}
		break
	}
	if _, err = raw.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	// filled принимает обычный вывод ffprobe с синтетически заполненной длительностью.
	var filled bytes.Buffer
	// probeBinary содержит путь ffprobe рядом с выбранным FFmpeg.
	probeBinary := filepath.Join(filepath.Dir(binary), "ffprobe")
	// args содержит запрос длительности первого видеопакета без отключения синтетического
	// заполнения.
	args := []string{"-v", "error", "-fd", "3", "-i", "fd:", "-select_streams", "v:0", "-read_intervals", "%+#1", "-show_entries", "packet=duration", "-of", "json"}
	// result содержит диагностику контрольного ffprobe. e сообщает об отказе его запуска.
	if result, e := runner.Run(context.Background(), Command{Binary: probeBinary, Args: args}, raw, &filled); e != nil {
		t.Fatal(e, result.Diagnostics)
	}
	// doc принимает JSON контрольной длительности ffprobe.
	var doc struct {
		// Packets содержит сведения о выбранном первом видеопакете.
		Packets []struct {
			// Duration содержит сообщённую ffprobe длительность в отсчётах видеодорожки.
			Duration int64 `json:"duration"`
		} `json:"packets"`
	}
	if err = json.Unmarshal(filled.Bytes(), &doc); err != nil {
		t.Fatal(err)
	}
	if len(doc.Packets) != 1 || doc.Packets[0].Duration != 1 {
		t.Fatal("контроль синтетического duration ffprobe изменился", filled.String())
	}
	// output содержит временный нормализованный fMP4.
	output, err := os.CreateTemp(t.TempDir(), "normalized.mp4")
	if err != nil {
		t.Fatal(err)
	}
	selfClose(t, output)
	// probe выполняет нормализацию и сверку фактических длительностей через выбранный ffprobe.
	probe := Prober{Binary: probeBinary, Runner: runner, Limits: selfProbeLimits()}
	if err = probe.NormalizePrepared(context.Background(), raw, output); err != nil {
		t.Fatal(err)
	}
	selfComparePackets(t, probe, raw, output, true)
	if _, err = probe.Verify(context.Background(), output, profile); err != nil {
		t.Fatal(err)
	}
	if err = probe.VerifyLoopDuration(context.Background(), output, loop); err != nil {
		t.Fatal(err)
	}
}
