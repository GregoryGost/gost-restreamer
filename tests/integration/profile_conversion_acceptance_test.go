package integration

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"net"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

type conversionScenario struct {
	name                                string
	inputStep, videoOrigin, audioOrigin int64
	outputRate                          core.FrameRate
	firstVideoTick, endVideoTick        int64
	originShiftSeconds                  int64
	hardware                            bool
}

func conversionFixture(t *testing.T, binary string) ([]media.Packet, fmp4.Init) {
	t.Helper()
	source := mediaCommand(t, binary, nil,
		"-hide_banner", "-loglevel", "warning", "-xerror",
		"-f", "lavfi", "-i", "testsrc2=size=2560x1440:rate=60:duration=0.4",
		"-f", "lavfi", "-i", "sine=frequency=750:sample_rate=48000:duration=0.32",
		"-map", "0:v", "-map", "1:a", "-c:v", "libaom-av1", "-threads:v", "2", "-cpu-used", "8", "-usage", "realtime", "-lag-in-frames", "0", "-g:v", "12", "-crf", "50",
		"-c:a", "aac", "-b:a", "128000", "-ac:a", "2", "-ar:a", "48000",
		"-f", "mp4", "-movflags", "delay_moov+default_base_moof+frag_every_frame", "-min_frag_duration", "1", "-use_editlist", "1", "-movie_timescale", "240000", "-avoid_negative_ts", "disabled", "pipe:1")
	resolver := func(d fmp4.TrackDescription) (media.CodecConfig, error) {
		if d.Track == media.TrackAudio {
			return fmp4.ResolveCodecConfig(d, 51, 1)
		}
		if d.Codec != core.CodecAV1 || d.Width != 2560 || d.Height != 1440 {
			return media.CodecConfig{}, errors.New("фикстура потеряла AV1 1440p")
		}
		return media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: 51, Revision: 1, TimeBase: d.TimeBase,
			Video: core.VideoFormat{Codec: core.CodecAV1, Width: 2560, Height: 1440, FrameRate: core.FrameRate{Numerator: 60, Denominator: 1}, CodecProfile: "main", PixelFormat: "yuv420p", BitDepth: 8, Color: core.ColorInfo{Range: "limited", Primaries: "unspecified", Transfer: "unspecified", Matrix: "unspecified"}}}, d.Headers)
	}
	d, err := fmp4.NewDemuxer(bytes.NewReader(source), fmp4.DemuxOptions{Limits: integrationLimits(), SourceEpoch: 51, VideoRevision: 1, AudioRevision: 1, ResolveConfig: resolver})
	if err != nil {
		t.Fatal(err)
	}
	packets := readMediaPackets(t, d)
	video, audio := 0, 0
	for _, p := range packets {
		if p.Config().Info().Track == media.TrackVideo {
			video++
		} else {
			audio++
		}
	}
	if video != 24 || audio != 16 {
		t.Fatalf("фикстура должна содержать 24 AV1 кадра и 16 AAC AU: %d/%d", video, audio)
	}
	return packets, d.Init()
}

func scheduledConversionInput(t *testing.T, source []media.Packet, original fmp4.Init, scenario conversionScenario) ([]media.Packet, fmp4.Init) {
	t.Helper()
	init := original
	info := init.Video.Info()
	info.TimeBase = media.TimeBase{Numerator: 1, Denominator: 60000}
	info.Video.FrameRate = core.FrameRate{Numerator: 60000, Denominator: uint32(scenario.inputStep)}
	var err error
	init.Video, err = media.NewCodecConfig(info, init.Video.Headers())
	if err != nil {
		t.Fatal(err)
	}
	info = init.Audio.Info()
	info.TimeBase = media.TimeBase{Numerator: 1, Denominator: 48000}
	init.Audio, err = media.NewCodecConfig(info, init.Audio.Headers())
	if err != nil {
		t.Fatal(err)
	}
	init.Origin = fmp4.OriginMapping{OffsetTicks: scenario.originShiftSeconds, TimeBase: media.TimeBase{Numerator: 1, Denominator: 1}}
	video, audio := int64(0), int64(0)
	packets := make([]media.Packet, 0, len(source))
	for _, p := range source {
		config, stamp := init.Video, p.Info()
		if p.Config().Info().Track == media.TrackVideo {
			stamp.PTS, stamp.DTS, stamp.DurationTicks = scenario.videoOrigin+video*scenario.inputStep, scenario.videoOrigin+video*scenario.inputStep, scenario.inputStep
			video++
		} else {
			config = init.Audio
			stamp.PTS, stamp.DTS, stamp.DurationTicks = scenario.audioOrigin+audio*1024, scenario.audioOrigin+audio*1024, 1024
			audio++
		}
		packet, err := media.NewPacket(stamp, config, p.Payload())
		if err != nil {
			t.Fatal(err)
		}
		packets = append(packets, packet)
	}
	return packets, init
}

func conversionTimeline(t *testing.T, ticks int64, base media.TimeBase) *big.Rat {
	t.Helper()
	return new(big.Rat).Mul(new(big.Rat).SetInt64(ticks), new(big.Rat).SetFrac64(int64(base.Numerator), int64(base.Denominator)))
}

func assertConversionTimeline(t *testing.T, packets []media.Packet, init fmp4.Init, scenario conversionScenario) {
	t.Helper()
	video := init.Video.Info().Video
	if video.Codec != core.CodecH264 || video.Width != 1920 || video.Height != 1080 || !video.FrameRate.Equal(scenario.outputRate) {
		t.Fatalf("не тот выходной формат: %+v", video)
	}
	if audio := init.Audio.Info().Audio; audio.Codec != core.CodecAAC || audio.SampleRateHz != 48000 || audio.Channels != 2 {
		t.Fatalf("не тот аудиопрофиль: %+v", audio)
	}
	var videos, audios []media.Packet
	for _, p := range packets {
		if p.Config().Info().Track == media.TrackVideo {
			videos = append(videos, p)
		} else {
			audios = append(audios, p)
		}
	}
	wantVideoCount := int(scenario.endVideoTick - scenario.firstVideoTick)
	if len(videos) != wantVideoCount || len(audios) != 17 {
		t.Fatalf("число пакетов не совпало с заданным входом: video=%d/%d audio=%d/17", len(videos), wantVideoCount, len(audios))
	}
	frameBase := media.TimeBase{Numerator: scenario.outputRate.Denominator, Denominator: scenario.outputRate.Numerator}
	var presentation []*big.Rat
	bframes := false
	var lastDTS *big.Rat
	for _, p := range videos {
		stamp, base := p.Info(), p.Config().Info().TimeBase
		pts, dts := conversionTimeline(t, stamp.PTS, base), conversionTimeline(t, stamp.DTS, base)
		if lastDTS != nil && dts.Cmp(lastDTS) <= 0 || pts.Cmp(dts) < 0 {
			t.Fatal("видеодорожка потеряла монотонный DTS/допустимый CTS", stamp)
		}
		lastDTS, bframes = dts, bframes || pts.Cmp(dts) != 0
		if got, want := conversionTimeline(t, stamp.DurationTicks, base), conversionTimeline(t, 1, frameBase); got.Cmp(want) != 0 {
			t.Fatalf("длительность кадра %s, требуется %s", got, want)
		}
		presentation = append(presentation, pts)
	}
	if !bframes && !scenario.hardware {
		t.Fatal("фикстура не проверила новые DTS B-кадров")
	}
	slices.SortFunc(presentation, func(a, b *big.Rat) int { return a.Cmp(b) })
	for i, pts := range presentation {
		want := conversionTimeline(t, scenario.firstVideoTick+int64(i), frameBase)
		if pts.Cmp(want) != 0 {
			t.Fatalf("PTS кадра %d = %s, требуется %s", i, pts, want)
		}
	}
	// AAC n7.1.5 добавляет initial_padding=1024. Входные 16 AU декодируются
	// в 16384 сэмпла: получаются 17 выходных AU без ресемплинга и без усечения.
	for i, p := range audios {
		stamp, base := p.Info(), p.Config().Info().TimeBase
		want := conversionTimeline(t, scenario.audioOrigin-1024+int64(i)*1024, media.TimeBase{Numerator: 1, Denominator: 48000})
		if conversionTimeline(t, stamp.PTS, base).Cmp(want) != 0 || conversionTimeline(t, stamp.DTS, base).Cmp(want) != 0 || conversionTimeline(t, stamp.DurationTicks, base).Cmp(big.NewRat(1024, 48000)) != 0 {
			t.Fatalf("AAC[%d] не сохранил заданную сетку сэмплов и начальную задержку: %+v, требуется %s", i, stamp, want)
		}
	}
	firstAudio := conversionTimeline(t, audios[0].Info().PTS, audios[0].Config().Info().TimeBase)
	effectiveAudioStart := new(big.Rat).Add(firstAudio, big.NewRat(1024, 48000))
	wantAudioStart := big.NewRat(scenario.audioOrigin, 48000)
	if effectiveAudioStart.Cmp(wantAudioStart) != 0 {
		t.Fatal("начальная задержка AAC изменила эффективное начало дорожки")
	}
	firstVideo := presentation[0]
	quantization := new(big.Rat).Sub(firstVideo, big.NewRat(scenario.videoOrigin, 60000))
	if new(big.Rat).Abs(quantization).Cmp(new(big.Rat).Quo(conversionTimeline(t, 1, frameBase), big.NewRat(2, 1))) > 0 {
		t.Fatal("начало видеодорожки ушло более чем на половину кадра")
	}
	wantAVStart := new(big.Rat).Sub(conversionTimeline(t, scenario.firstVideoTick, frameBase), wantAudioStart)
	if new(big.Rat).Sub(firstVideo, effectiveAudioStart).Cmp(wantAVStart) != 0 {
		t.Fatal("раздельное обнуление дорожек изменило A/V origin")
	}
	lastAudio := audios[len(audios)-1]
	audioEnd := conversionTimeline(t, lastAudio.Info().PTS+lastAudio.Info().DurationTicks, lastAudio.Config().Info().TimeBase)
	wantAudioEnd := big.NewRat(scenario.audioOrigin+16384, 48000)
	if audioEnd.Cmp(wantAudioEnd) != 0 {
		t.Fatalf("конец AAC=%s, требуется %s", audioEnd, wantAudioEnd)
	}
	wantVideoEnd := conversionTimeline(t, scenario.endVideoTick, frameBase)
	actualVideoEnd := new(big.Rat).Add(presentation[len(presentation)-1], conversionTimeline(t, 1, frameBase))
	if actualVideoEnd.Cmp(wantVideoEnd) != 0 {
		t.Fatal("неверное округлённое окончание видеодорожки")
	}
	if new(big.Rat).Sub(actualVideoEnd, audioEnd).Cmp(new(big.Rat).Sub(wantVideoEnd, wantAudioEnd)) != 0 {
		t.Fatal("потеряно заданное соотношение концов A/V-дорожек")
	}
}

func assertDecodedConversion(t *testing.T, binary string, wire []byte, scenario conversionScenario) {
	t.Helper()
	// Проверяем полные AU со знаковыми метками: обычный декодер MOV обрезает начало по шкале воспроизведения.
	decoded := mediaCommand(t, binary, wire, "-hide_banner", "-loglevel", "warning", "-xerror", "-copyts", "-flags2:a", "+skip_manual", "-i", "pipe:0", "-map", "0:v", "-map", "0:a", "-fps_mode", "passthrough", "-f", "framemd5", "pipe:1")
	counts := [2]int{}
	samples := int64(0)
	bases := map[int]string{}
	for _, line := range strings.Split(string(decoded), "\n") {
		if strings.HasPrefix(line, "#tb ") {
			var index int
			var base string
			if _, err := fmt.Sscanf(line, "#tb %d: %s", &index, &base); err != nil {
				t.Fatal(err)
			}
			bases[index] = base
		}
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		fields := strings.Split(line, ",")
		if len(fields) < 6 {
			t.Fatal("неполный framemd5", line)
		}
		index, err := strconv.Atoi(strings.TrimSpace(fields[0]))
		if err != nil || index < 0 || index > 1 {
			t.Fatal("неизвестная дорожка", line)
		}
		pts, err := strconv.ParseInt(strings.TrimSpace(fields[2]), 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		duration, err := strconv.ParseInt(strings.TrimSpace(fields[3]), 10, 64)
		if err != nil {
			t.Fatal(err)
		}
		if index == 0 {
			want := big.NewRat((scenario.firstVideoTick+int64(counts[0]))*int64(scenario.outputRate.Denominator), int64(scenario.outputRate.Numerator))
			if seconds(t, pts, bases[index]).Cmp(want) != 0 {
				t.Fatal("декодирование изменило заданные метки представления видео", line, want)
			}
		} else {
			want := big.NewRat(scenario.audioOrigin-1024+samples, 48000)
			if seconds(t, pts, bases[index]).Cmp(want) != 0 {
				t.Fatal("декодирование изменило начало AAC или начальную задержку", line, want)
			}
			samples += duration
		}
		counts[index]++
	}
	if counts[0] != int(scenario.endVideoTick-scenario.firstVideoTick) || counts[1] != 17 || samples != 17408 {
		t.Fatalf("число декодированных кадров и сэмплов: %v, сэмплов=%d", counts, samples)
	}
}

func runConversionWorker(t *testing.T, binary string, packets []media.Packet, init fmp4.Init, scenario conversionScenario) ([]media.Packet, fmp4.Init, []byte) {
	t.Helper()
	base, err := os.MkdirTemp("/tmp", "gpc-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(base); err != nil {
			t.Error(err)
		}
	})
	s, err := ffmpeg.New(ffmpeg.Config{BaseDir: base, StartupTimeout: 10 * time.Second, VideoTimeout: 5 * time.Second, AudioTimeout: 5 * time.Second, WatchdogInterval: 50 * time.Millisecond, MaxRestarts: 0, RestartBackoff: 10 * time.Millisecond, GracePeriod: 100 * time.Millisecond, KillTimeout: time.Second, ShutdownTimeout: 3 * time.Second, DiagnosticEvents: 1024, DiagnosticBytes: 1 << 20, DiagnosticChunkBytes: 256, MaxCommandBytes: 8192, MaxCommandArgs: 128})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		ctx, cancel := context.WithTimeout(context.Background(), 4*time.Second)
		defer cancel()
		if err := s.Stop(ctx); err != nil {
			t.Error(err)
		}
	})
	var returned []media.Packet
	var resultInit fmp4.Init
	out := &boundedMediaBuffer{max: 16 << 20}
	inputWire := &boundedMediaBuffer{max: 16 << 20}
	input := func(ctx context.Context, c net.Conn, _ ffmpeg.Progress) error {
		mux, err := fmp4.NewMuxer(io.MultiWriter(c, inputWire), init, integrationLimits())
		if err != nil {
			return err
		}
		validator := fmp4.NewFFmpegTimingValidatorForOrigin(init.Origin)
		for _, packet := range packets {
			if err := validator.Validate(packet); err != nil {
				return err
			}
			if err := mux.WritePacket(packet); err != nil {
				return err
			}
		}
		u, ok := c.(*net.UnixConn)
		if !ok {
			return errors.New("ожидался UnixConn")
		}
		if err := u.CloseWrite(); err != nil {
			return err
		}
		<-ctx.Done()
		return ctx.Err()
	}
	output := func(_ context.Context, c net.Conn, progress ffmpeg.Progress) error {
		d, err := fmp4.NewDemuxer(io.TeeReader(c, out), fmp4.DemuxOptions{Limits: integrationLimits(), SourceEpoch: 52, VideoRevision: 1, AudioRevision: 1})
		if err != nil {
			return err
		}
		resultInit = d.Init()
		for {
			p, err := d.Next()
			if errors.Is(err, io.EOF) {
				return nil
			}
			if err != nil {
				return err
			}
			if len(returned) >= 128 {
				return errors.New("превышен бюджет выходных пакетов")
			}
			returned = append(returned, p)
			track := ffmpeg.Video
			if p.Config().Info().Track == media.TrackAudio {
				track = ffmpeg.Audio
			}
			progress(track, p.Info().DTS)
		}
	}
	command := func(_ context.Context, a ffmpeg.Attempt) (ffmpeg.Command, error) {
		filter := fmt.Sprintf("fps=fps=%d/%d:round=near:eof_action=round,scale=w=1920:h=1080:flags=bilinear,format=yuv420p", scenario.outputRate.Numerator, scenario.outputRate.Denominator)
		args := []string{"-hide_banner", "-nostdin", "-loglevel", "warning", "-xerror", "-copyts", "-flags2:a", "+skip_manual", "-f", "mp4", "-fflags", "+ignidx", "-i", "async:unix:" + a.InputPath, "-map", "0:v", "-map", "0:a", "-vf", filter}
		if scenario.hardware {
			// VideoToolbox не сообщает VUI timing; фиксируем профиль в SPS,
			// а фактическую сетку независимо проверяем по всем PTS и длительностям.
			vui := fmt.Sprintf("h264_metadata=tick_rate=%d/%d", 2*scenario.outputRate.Numerator, scenario.outputRate.Denominator)
			args = append(args, "-c:v", "h264_videotoolbox", "-allow_sw", "0", "-b:v", "3000000", "-g:v", "12", "-bf:v", "0", "-bsf:v", vui)
		} else {
			args = append(args, "-c:v", "libx264", "-threads:v", "1", "-preset", "ultrafast", "-g:v", "12", "-bf:v", "2")
		}
		args = append(args, "-fps_mode:v", "passthrough", "-enc_time_base:v", "filter", "-c:a", "aac", "-b:a", "64000", "-ar:a", "48000", "-ac:a", "2", "-f", "mp4", "-movflags", "delay_moov+default_base_moof+frag_every_frame", "-min_frag_duration", "1", "-use_editlist", "1", "-movie_timescale", "240000", "-video_track_timescale", strconv.FormatUint(uint64(scenario.outputRate.Numerator), 10), "-avoid_negative_ts", "disabled", "unix:"+a.OutputPath)
		return ffmpeg.Command{Binary: binary, Args: args}, nil
	}
	ctx, cancel := context.WithTimeout(context.Background(), 25*time.Second)
	defer cancel()
	if err := s.Start(ctx, ffmpeg.Worker{SessionID: "conversion-session", GroupID: "conversion-group", Owner: integrationOwner{make(chan struct{})}, Command: command, Input: input, Output: output}); err != nil {
		t.Fatal(err)
	}
	result, err := s.Wait(ctx)
	if err != nil {
		t.Fatal(err)
	}
	var diagnostics []byte
	for {
		event, err := s.NextDiagnostic(ctx)
		if errors.Is(err, io.EOF) {
			break
		}
		if err != nil {
			t.Fatal(err)
		}
		if event.Kind == "stderr" {
			diagnostics = append(diagnostics, event.Data...)
		}
	}
	if result.Err != nil || result.CleanupErr != nil || !result.Clean || result.Attempts != 1 {
		probe := mediaCommand(t, filepath.Join(filepath.Dir(binary), "ffprobe"), inputWire.Bytes(), "-v", "error", "-select_streams", "a", "-show_packets", "-show_entries", "packet=pts,dts,duration,side_data_list", "-of", "json", "pipe:0")
		t.Logf("AAC на входе FFmpeg: %s", probe)
		t.Fatalf("преобразование через Supervisor/UDS: %+v: %s", result, diagnostics)
	}
	initialWarningCount, err := conversionTimingDiagnostics(diagnostics, scenario)
	if err != nil {
		t.Fatal(err)
	}
	if initialWarningCount == 1 {
		t.Logf("x264 b35605a сравнил первый отрицательный PTS с начальным i_largest_pts=-1; полная сетка проверяется отдельно: %s", diagnostics)
	}
	entries, err := os.ReadDir(base)
	if err != nil || len(entries) != 0 {
		t.Fatal("UDS не удалены после join", entries, err)
	}
	return returned, resultInit, bytes.Clone(out.Bytes())
}

var initialX264PTSWarning = regexp.MustCompile(`^\[libx264 @ 0x[0-9a-fA-F]+\] non-strictly-monotonic PTS$`)

func conversionTimingDiagnostics(diagnostics []byte, scenario conversionScenario) (int, error) {
	warnings := 0
	for _, line := range strings.Split(string(diagnostics), "\n") {
		lower := strings.ToLower(line)
		if !strings.Contains(lower, "non-monotonic") && !strings.Contains(lower, "non-strictly-monotonic") && !strings.Contains(lower, "invalid dts") {
			continue
		}
		// В x264 b35605a i_largest_pts начинается с -1, затем безусловно принимает PTS.
		// Только при первом отрицательном PTS на CPU допускается эта точная строка один раз.
		if !scenario.hardware && scenario.firstVideoTick < 0 && initialX264PTSWarning.MatchString(line) && warnings == 0 {
			warnings++
			continue
		}
		return warnings, fmt.Errorf("FFmpeg сообщил недопустимую диагностику времени: %s", diagnostics)
	}
	return warnings, nil
}

func TestConversionTimingDiagnosticsRejectsTimestampRepairs(t *testing.T) {
	warning := "[libx264 @ 0x123ab] non-strictly-monotonic PTS\n"
	negative := conversionScenario{firstVideoTick: -4}
	for _, tc := range []struct {
		name        string
		diagnostics string
		scenario    conversionScenario
		wantCount   int
		wantError   bool
	}{
		{name: "clean_negative", scenario: negative},
		{name: "first_negative_x264", diagnostics: warning, scenario: negative, wantCount: 1},
		{name: "duplicate_x264", diagnostics: warning + warning, scenario: negative, wantCount: 1, wantError: true},
		{name: "positive_x264", diagnostics: warning, scenario: conversionScenario{firstVideoTick: 4}, wantError: true},
		{name: "hardware_x264", diagnostics: warning, scenario: conversionScenario{firstVideoTick: -4, hardware: true}, wantError: true},
		{name: "other_encoder", diagnostics: "[h264_videotoolbox @ 0x123ab] non-strictly-monotonic PTS\n", scenario: negative, wantError: true},
		{name: "different_message", diagnostics: "[libx264 @ 0x123ab] non-strictly-monotonic PTS repaired\n", scenario: negative, wantError: true},
		{name: "invalid_dts", diagnostics: warning + "[mp4 @ 0x123ab] Invalid DTS: 1 PTS: 0\n", scenario: negative, wantCount: 1, wantError: true},
		{name: "nonmonotonic_dts", diagnostics: warning + "[mp4 @ 0x123ab] Non-monotonic DTS; changing to 1\n", scenario: negative, wantCount: 1, wantError: true},
		{name: "lowercase_dts", diagnostics: "[mp4 @ 0x123ab] non-monotonic dts\n", scenario: negative, wantError: true},
	} {
		t.Run(tc.name, func(t *testing.T) {
			count, err := conversionTimingDiagnostics([]byte(tc.diagnostics), tc.scenario)
			if (err != nil) != tc.wantError || count != tc.wantCount {
				t.Fatalf("классификация диагностики: count=%d err=%v, требуется count=%d error=%v", count, err, tc.wantCount, tc.wantError)
			}
		})
	}
}

func TestAcceptanceProfileConversionCPUAndOptionalVideoToolbox(t *testing.T) {
	binary := os.Getenv("GOST_MEDIA_FFMPEG")
	if binary == "" {
		t.Skip("локальное преобразование требует явно заданного GOST_MEDIA_FFMPEG")
	}
	version := mediaCommand(t, binary, nil, "-version")
	if !strings.HasPrefix(string(version), "ffmpeg version 7.1.5 ") {
		t.Fatalf("временные эталоны проверены для FFmpeg 7.1.5: %s", strings.SplitN(string(version), "\n", 2)[0])
	}
	packets, init := conversionFixture(t, binary)
	scenarios := []conversionScenario{
		{name: "cpu_60_to_30_positive", inputStep: 1000, videoOrigin: 7000, audioOrigin: 6000, outputRate: core.FrameRate{Numerator: 30, Denominator: 1}, firstVideoTick: 4, endVideoTick: 16},
		{name: "cpu_60_to_30_positive_shifted", inputStep: 1000, videoOrigin: 7000, audioOrigin: 6000, outputRate: core.FrameRate{Numerator: 30, Denominator: 1}, firstVideoTick: 4, endVideoTick: 16, originShiftSeconds: 1},
		{name: "cpu_60_to_30_negative", inputStep: 1000, videoOrigin: -7000, audioOrigin: -6000, outputRate: core.FrameRate{Numerator: 30, Denominator: 1}, firstVideoTick: -4, endVideoTick: 9, originShiftSeconds: 1},
		{name: "cpu_rational", inputStep: 1001, videoOrigin: 7007, audioOrigin: 6000, outputRate: core.FrameRate{Numerator: 30000, Denominator: 1001}, firstVideoTick: 4, endVideoTick: 16},
	}
	if os.Getenv("GOST_MEDIA_VIDEOTOOLBOX") == "1" {
		scenarios = append(scenarios, conversionScenario{name: "videotoolbox_60_to_30", inputStep: 1000, videoOrigin: 7000, audioOrigin: 6000, outputRate: core.FrameRate{Numerator: 30, Denominator: 1}, firstVideoTick: 4, endVideoTick: 16, hardware: true})
	}
	for _, scenario := range scenarios {
		t.Run(scenario.name, func(t *testing.T) {
			input, inputInit := scheduledConversionInput(t, packets, init, scenario)
			returned, outputInit, wire := runConversionWorker(t, binary, input, inputInit, scenario)
			assertConversionTimeline(t, returned, outputInit, scenario)
			assertDecodedConversion(t, binary, wire, scenario)
			assertProbePackets(t, probeMedia(t, filepath.Join(filepath.Dir(binary), "ffprobe"), wire), returned)
		})
	}
}
