package integration

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"image"
	"image/color"
	"image/png"
	"io"
	"math"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/fallback"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

func liveAudioProber(t *testing.T, executable string) (fallback.Prober, fallback.Runner) {
	t.Helper()
	runner := fallback.Runner{Limits: fallback.RunnerLimits{Timeout: 10 * time.Second, MaxArgs: 256, MaxCommandBytes: 1 << 16, MaxDiagnosticBytes: 1 << 20}}
	return fallback.Prober{Binary: filepath.Join(filepath.Dir(executable), "ffprobe"), Runner: fallbackPipelineRunner{t: t, runner: runner}, Limits: fallback.ProbeLimits{MaxJSONBytes: 1 << 20, MaxWidth: 1024, MaxHeight: 1024, MaxPixels: 1 << 20, MaxDuration: 10 * time.Second, ImageDuration: 1600 * time.Millisecond, MaxPackets: 4096, Container: integrationLimits()}}, runner
}

func liveAudioSourceAsset(t *testing.T, executable, kind string) ([]byte, core.AssetKind) {
	t.Helper()
	if kind == "image" {
		img := image.NewRGBA(image.Rect(0, 0, 128, 128))
		for y := range 128 {
			for x := range 128 {
				img.SetRGBA(x, y, color.RGBA{R: uint8(x), G: uint8(y), B: 40, A: 255})
			}
		}
		var data bytes.Buffer
		if err := png.Encode(&data, img); err != nil {
			t.Fatal(err)
		}
		return data.Bytes(), core.AssetImage
	}
	args := []string{"-hide_banner", "-loglevel", "error", "-xerror", "-f", "lavfi", "-i", "testsrc2=size=128x128:rate=30:duration=1.6"}
	if kind == "video-tone" {
		args = append(args, "-f", "lavfi", "-i", "sine=frequency=997:sample_rate=48000:duration=1.6")
	}
	args = append(args, "-map", "0:v:0", "-c:v", "libx264", "-threads:v", "1", "-pix_fmt", "yuv420p", "-g:v", "15", "-bf:v", "0")
	if kind == "video-tone" {
		args = append(args, "-map", "1:a:0", "-c:a", "aac", "-b:a", "96000", "-ar:a", "48000", "-ac:a", "2")
	}
	args = append(args, "-f", "mp4", "-movflags", "delay_moov+default_base_moof+frag_every_frame", "pipe:1")
	data := mediaCommand(t, executable, nil, args...)
	if kind == "video-tone" {
		pcm := mediaCommand(t, executable, data, "-v", "error", "-i", "pipe:0", "-map", "0:a:0", "-f", "f32le", "pipe:1")
		peak := float64(0)
		for i := 0; i+4 <= len(pcm); i += 4 {
			peak = max(peak, math.Abs(float64(math.Float32frombits(binary.LittleEndian.Uint32(pcm[i:])))))
		}
		if peak < 0.02 {
			t.Fatal("Исходное видео не содержит заведомо слышимого тона")
		}
	}
	return data, core.AssetVideo
}

func liveAudioPrepared(t *testing.T, executable string, profile core.GroupProfile, kind string) []byte {
	t.Helper()
	data, assetKind := liveAudioSourceAsset(t, executable, kind)
	prober, runner := liveAudioProber(t, executable)
	source, err := os.CreateTemp(t.TempDir(), "source")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := source.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err = source.Write(data); err != nil {
		t.Fatal(err)
	}
	observed, err := prober.Source(context.Background(), source, assetKind)
	if err != nil {
		t.Fatalf("Проверка исходника: %v", err)
	}
	period, err := fallback.EffectiveLoopDuration(observed.Duration, profile, prober.Limits.MaxDuration)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = source.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	command, err := encodingPipelineCPUBuilder(executable)(fallback.GenerateRequest{Source: observed, Profile: profile, LoopDuration: period})
	if err != nil {
		t.Fatal(err)
	}
	var encoded boundedMediaBuffer
	encoded.max = 8 << 20
	result, err := runner.Run(context.Background(), command, source, &encoded)
	if err != nil {
		t.Fatalf("Генерация: %v %s", err, result.Diagnostics)
	}
	file, err := os.CreateTemp(t.TempDir(), "encoded")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err = file.Write(encoded.Bytes()); err != nil {
		t.Fatal(err)
	}
	var prepared boundedMediaBuffer
	prepared.max = 8 << 20
	if err = prober.NormalizePrepared(context.Background(), file, &prepared); err != nil {
		t.Fatalf("Нормализация исходника %s duration=%s period=%s: %v", kind, observed.Duration, period, err)
	}
	verified, err := os.CreateTemp(t.TempDir(), "prepared")
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		if err := verified.Close(); err != nil {
			t.Error(err)
		}
	}()
	if _, err = verified.Write(prepared.Bytes()); err != nil {
		t.Fatal(err)
	}
	if err = prober.VerifyLoopDuration(context.Background(), verified, period); err != nil {
		t.Fatalf("Проверка периода исходника %s duration=%s period=%s: %v", kind, observed.Duration, period, err)
	}
	return bytes.Clone(prepared.Bytes())
}

func liveAudioNonzeroSamples(pcm []byte) int {
	n := 0
	for i := 0; i+4 <= len(pcm); i += 4 {
		if math.Float32frombits(binary.LittleEndian.Uint32(pcm[i:])) != 0 {
			n++
		}
	}
	return n
}

func liveAudioADTS(t *testing.T, headers, payload []byte) []byte {
	t.Helper()
	if len(headers) < 2 {
		t.Fatal("Нет ASC")
	}
	object := headers[0] >> 3
	rate := (headers[0]&7)<<1 | headers[1]>>7
	channels := headers[1] >> 3 & 15
	n := len(payload) + 7
	if object != 2 || rate > 12 || channels == 0 || channels > 7 || n > 8191 {
		t.Fatal("Фикстура не представима как AAC LC ADTS")
	}
	return append([]byte{0xff, 0xf1, (object-1)<<6 | rate<<2 | channels>>2, (channels&3)<<6 | byte(n>>11), byte(n >> 3), byte(n&7)<<5 | 31, 0xfc}, payload...)
}

// Подстановка меняет аппаратные видеопараметры; аудиоплан остаётся результатом рабочего компилятора.
func liveAudioCPUCommand(c ffmpeg.NVIDIAConfig, a ffmpeg.Attempt) (ffmpeg.Command, error) {
	command, err := ffmpeg.BuildNVIDIACommand(c, a)
	if err != nil {
		return command, err
	}
	var args []string
	beforeInput := true
	for i := 0; i < len(command.Args); i++ {
		arg := command.Args[i]
		if arg == "-i" {
			beforeInput = false
		}
		if arg == "-hwaccel" || arg == "-hwaccel_output_format" || arg == "-pix_fmt" || arg == "-no-scenecut" || arg == "-forced-idr" || arg == "-rc" || arg == "-preset" || arg == "-tune" || arg == "-multipass" || arg == "-rc-lookahead" || arg == "-spatial-aq" || arg == "-temporal-aq" || arg == "-aq-strength" {
			i++
			continue
		}
		if beforeInput && arg == "-c:v" {
			i++
			continue
		}
		if arg == "h264_nvenc" {
			arg = "libx264"
		}
		if strings.Contains(arg, "scale_cuda=") {
			arg = strings.ReplaceAll(arg, "scale_cuda=", "scale=")
			arg = strings.ReplaceAll(arg, ":format=yuv420p", ",format=yuv420p")
		}
		args = append(args, arg)
	}
	if len(args) == 0 {
		return command, fmt.Errorf("Пустая команда CPU")
	}
	output := args[len(args)-1]
	args = append(args[:len(args)-1], "-threads:v", "1", "-sc_threshold", "0", "-preset", "veryfast", output)
	command.Args = args
	return command, nil
}

func liveAudioWirePCM(t *testing.T, executable string, messages []publisherWireMessage, channels int) ([]byte, [][]byte) {
	t.Helper()
	var asc, adts []byte
	var payloads [][]byte
	for _, message := range messages {
		if message.typ != 8 || len(message.body) < 2 {
			continue
		}
		if message.body[1] == 0 {
			asc = bytes.Clone(message.body[2:])
		}
		if message.body[1] == 1 {
			adts = append(adts, liveAudioADTS(t, asc, message.body[2:])...)
			payloads = append(payloads, bytes.Clone(message.body[2:]))
		}
	}
	pcm := mediaCommand(t, executable, adts, "-v", "error", "-xerror", "-flags2:a", "+skip_manual", "-f", "aac", "-i", "pipe:0", "-map", "0:a:0", "-f", "f32le", "pipe:1")
	if len(pcm) != len(payloads)*1024*channels*4 {
		t.Fatalf("Неполные AU после непрерывного декодирования: %d/%d", len(pcm), len(payloads))
	}
	return pcm, payloads
}

func liveAudioDumpPackets(t *testing.T, name string, packets []media.Packet) {
	t.Helper()
	directory := os.Getenv("GOST_LIVE_AUDIO_DIAGNOSTICS_DIR")
	if directory == "" {
		return
	}
	type record struct {
		Info             media.PacketInfo
		Config           media.CodecConfigInfo
		Headers, Payload []byte
	}
	var records []record
	for _, packet := range packets {
		records = append(records, record{packet.Info(), packet.Config().Info(), packet.Config().Headers(), packet.Payload()})
	}
	data, err := json.Marshal(records)
	if err != nil || len(data) > 16<<20 {
		t.Fatalf("Сериализация ограниченной диагностики: bytes=%d err=%v", len(data), err)
	}
	if err = os.MkdirAll(directory, 0700); err != nil {
		t.Fatal(err)
	}
	if err = os.WriteFile(filepath.Join(directory, name+".json"), data, 0600); err != nil {
		t.Fatal(err)
	}
}

// Независимая конечная команда получает те же принятые PTS/DTS, без Group, Supervisor и рабочего компилятора.
func liveAudioTranscodeReference(t *testing.T, executable string, source []media.Packet, profile core.GroupProfile) []media.Packet {
	t.Helper()
	epochs := map[uint64][]media.Packet{}
	var order []uint64
	for _, packet := range source {
		epoch := packet.Config().Info().SourceEpoch
		if _, exists := epochs[epoch]; !exists {
			order = append(order, epoch)
		}
		epochs[epoch] = append(epochs[epoch], packet)
	}
	var references []media.Packet
	referenceID := uint64(0)
	for _, epoch := range order {
		publication := epochs[epoch]
		for start, independent := range publication {
			if independent.Config().Info().Track != media.TrackVideo || !independent.Info().Keyframe {
				continue
			}
			cut := fallbackActualTime(independent.Info().PTS, independent.Config().Info().TimeBase)
			var packets []media.Packet
			for _, packet := range publication[start:] {
				if packet.Config().Info().Track == media.TrackAudio && fallbackActualTime(packet.Info().PTS, packet.Config().Info().TimeBase).Cmp(cut) < 0 {
					continue
				}
				packets = append(packets, packet)
			}
			referenceID++
			if referenceID > 128 {
				t.Fatal("Превышен конечный бюджет независимых точек допуска")
			}
			var init fmp4.Init
			for _, packet := range packets {
				if packet.Config().Info().Track == media.TrackVideo && init.Video.Validate() != nil {
					init.Video = packet.Config()
				}
				if packet.Config().Info().Track == media.TrackAudio && init.Audio.Validate() != nil {
					init.Audio = packet.Config()
				}
			}
			if init.Audio.Validate() != nil {
				continue
			}
			var input boundedMediaBuffer
			input.max = 16 << 20
			mux, err := fmp4.NewMuxer(&input, init, integrationLimits())
			if err != nil {
				t.Fatal(err)
			}
			for _, packet := range packets {
				if err = mux.WritePacket(packet); err != nil {
					t.Fatal(err)
				}
			}
			v, a := profile.Format.Video, profile.Format.Audio
			n := func(value uint64) string { return strconv.FormatUint(value, 10) }
			filter := fmt.Sprintf("fps=fps=%d/%d:round=near:eof_action=round,scale=w=%d:h=%d,format=yuv420p", v.FrameRate.Numerator, v.FrameRate.Denominator, v.Width, v.Height)
			wire := mediaCommand(t, executable, input.Bytes(), "-hide_banner", "-loglevel", "warning", "-xerror", "-copyts", "-flags2:a", "+skip_manual", "-f", "mp4", "-fflags", "+ignidx", "-i", "pipe:0", "-map", "0:v:0", "-map", "0:a:0", "-vf", filter, "-fps_mode:v", "passthrough", "-enc_time_base:v", "filter", "-c:v", "libx264", "-threads:v", "1", "-preset", "veryfast", "-profile:v", "high", "-g:v", "6", "-bf:v", "0", "-sc_threshold", "0", "-c:a", "aac", "-profile:a", "aac_low", "-b:a", n(profile.AudioEncoding.BitrateBPS), "-ar:a", n(uint64(a.SampleRateHz)), "-ac:a", n(uint64(a.Channels)), "-channel_layout:a", a.ChannelLayout, "-aac_coder", "fast", "-f", "mp4", "-movflags", "delay_moov+default_base_moof+frag_every_frame", "-min_frag_duration", "1", "-use_editlist", "1", "-movie_timescale", "48000", "-video_track_timescale", n(uint64(v.FrameRate.Numerator)), "-avoid_negative_ts", "disabled", "pipe:1")
			demux, err := fmp4.NewDemuxer(bytes.NewReader(wire), fmp4.DemuxOptions{Limits: integrationLimits(), SourceEpoch: referenceID, VideoRevision: 1, AudioRevision: 1})
			if err != nil {
				t.Fatal(err)
			}
			decoded := readMediaPackets(t, demux)
			liveAudioDumpPackets(t, fmt.Sprintf("reference-cut-%d", referenceID), decoded)
			audio := 0
			durations := map[int64]int{}
			for _, packet := range decoded {
				if packet.Config().Info().Track == media.TrackAudio {
					audio++
					durations[packet.Info().DurationTicks]++
				}
			}
			pcm := mediaCommand(t, executable, wire, "-v", "error", "-xerror", "-copyts", "-flags2:a", "+skip_manual", "-i", "pipe:0", "-map", "0:a:0", "-f", "f32le", "pipe:1")
			if len(pcm) != audio*1024*int(a.Channels)*4 {
				t.Fatalf("Эталон не сохранил 1024 декодированных сэмпла на AU: %d bytes/%d AU", len(pcm), audio)
			}
			if referenceID == 1 {
				t.Logf("Независимый эталон FFmpeg: AAC=%d PCM на канал=%d ASC=%x длительности=%v", audio, len(pcm)/(int(a.Channels)*4), demux.Init().Audio.Headers(), durations)
			}
			for _, packet := range decoded {
				if packet.Config().Info().Track == media.TrackAudio {
					references = append(references, packet)
				}
			}
		}
	}
	t.Logf("Независимые временные эталоны: %d допустимых ключевых точек, %d AAC AU", referenceID, len(references))
	return references
}

func liveAudioAssertTone(t *testing.T, pcm []byte, payloads [][]byte, quiet map[[32]byte]bool, channels, sampleRate int) {
	t.Helper()
	var wanted, adjacent float64
	for block, payload := range payloads {
		if quiet[sha256.Sum256(payload)] {
			continue
		}
		for _, frequency := range []int{750, 500, 1000} {
			var sine, cosine float64
			for n := range 1024 {
				value := float64(math.Float32frombits(binary.LittleEndian.Uint32(pcm[(block*1024+n)*channels*4:])))
				angle := 2 * math.Pi * float64(frequency*n) / float64(sampleRate)
				sine += value * math.Sin(angle)
				cosine += value * math.Cos(angle)
			}
			energy := sine*sine + cosine*cosine
			if frequency == 750 {
				wanted += energy
			} else {
				adjacent += energy
			}
		}
	}
	if wanted == 0 || wanted <= adjacent {
		t.Fatalf("LIVE потерял независимый тон 750 Hz: энергия=%g, соседние частоты=%g", wanted, adjacent)
	}
}
