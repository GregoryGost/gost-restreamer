package fmp4

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"math/big"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
)

type probeStream struct {
	Index          int
	CodecName      string `json:"codec_name"`
	CodecType      string `json:"codec_type"`
	TimeBase       string `json:"time_base"`
	Width, Height  uint32
	Profile        string
	PixelFormat    string `json:"pix_fmt"`
	FrameRate      string `json:"avg_frame_rate"`
	SampleRate     string `json:"sample_rate"`
	Channels       uint16
	ChannelLayout  string `json:"channel_layout"`
	ColorRange     string `json:"color_range"`
	ColorPrimaries string `json:"color_primaries"`
	ColorTransfer  string `json:"color_transfer"`
	ColorSpace     string `json:"color_space"`
}
type probePacket struct {
	StreamIndex int `json:"stream_index"`
	PTS, DTS    int64
	Flags       string
}
type probeResult struct {
	Streams []probeStream
	Packets []probePacket
}
type cappedBuffer struct {
	bytes.Buffer
	limit int
}

func (b *cappedBuffer) Write(p []byte) (int, error) {
	if len(p) > b.limit-b.Len() {
		return 0, ErrLimit
	}
	return b.Buffer.Write(p)
}
func runMediaCommand(t *testing.T, path string, input []byte, args ...string) []byte {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 20*time.Second)
	defer cancel()
	cmd := exec.CommandContext(ctx, path, args...)
	cmd.Stdin = bytes.NewReader(input)
	out := &cappedBuffer{limit: 16 << 20}
	stderr := &cappedBuffer{limit: 1 << 20}
	cmd.Stdout = out
	cmd.Stderr = stderr
	if err := cmd.Run(); err != nil {
		t.Fatalf("%s: %v: %s", filepath.Base(path), err, stderr.String())
	}
	if strings.Contains(stderr.String(), "Invalid DTS") || strings.Contains(stderr.String(), "Non-monotonic") {
		t.Fatalf("переписаны временные метки: %s", stderr.String())
	}
	return out.Bytes()
}
func probeMedia(t *testing.T, path string, input []byte) probeResult {
	t.Helper()
	data := runMediaCommand(t, path, input, "-v", "error", "-show_streams", "-show_packets", "-of", "json", "pipe:0")
	var result probeResult
	if err := json.Unmarshal(data, &result); err != nil {
		t.Fatal(err)
	}
	return result
}
func rationalBase(t *testing.T, value string) media.TimeBase {
	t.Helper()
	parts := strings.Split(value, "/")
	if len(parts) != 2 {
		t.Fatal(value)
	}
	n, err := strconv.ParseUint(parts[0], 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	d, err := strconv.ParseUint(parts[1], 10, 32)
	if err != nil {
		t.Fatal(err)
	}
	return media.TimeBase{Numerator: uint32(n), Denominator: uint32(d)}
}
func observedConfig(t *testing.T, streams []probeStream, desc TrackDescription) (media.CodecConfig, error) {
	t.Helper()
	for _, s := range streams {
		if s.CodecType != string(desc.Track) {
			continue
		}
		info := media.CodecConfigInfo{Track: desc.Track, SourceEpoch: 41, Revision: 1, TimeBase: desc.TimeBase}
		if desc.Track == media.TrackVideo {
			fps := rationalBase(t, s.FrameRate)
			color := func(s string) string {
				if s == "" {
					return "unspecified"
				}
				return s
			}
			info.Video = core.VideoFormat{Codec: desc.Codec, Width: s.Width, Height: s.Height, FrameRate: core.FrameRate{Numerator: fps.Numerator, Denominator: fps.Denominator}, CodecProfile: s.Profile, PixelFormat: s.PixelFormat, BitDepth: 8, Color: core.ColorInfo{Range: color(s.ColorRange), Primaries: color(s.ColorPrimaries), Transfer: color(s.ColorTransfer), Matrix: color(s.ColorSpace)}}
		} else {
			rate, err := strconv.ParseUint(s.SampleRate, 10, 32)
			if err != nil {
				return media.CodecConfig{}, err
			}
			info.Audio = core.AudioFormat{Codec: core.CodecAAC, CodecProfile: s.Profile, SampleRateHz: uint32(rate), Channels: s.Channels, ChannelLayout: s.ChannelLayout}
		}
		return media.NewCodecConfig(info, desc.Headers)
	}
	return media.CodecConfig{}, ErrMetadataUnavailable
}
func timingValue(v int64, b media.TimeBase) *big.Rat {
	return new(big.Rat).Mul(new(big.Rat).SetInt64(v), new(big.Rat).SetFrac64(int64(b.Numerator), int64(b.Denominator)))
}

// Проверка запускается только при явно заданном пути к разрешённому FFmpeg.
func TestFFmpegCPUFragmentedRoundTrip(t *testing.T) {
	ffmpeg := os.Getenv("GOST_FMP4_FFMPEG")
	if ffmpeg == "" {
		t.Skip("локальная CPU-интеграция не включена")
	}
	ffprobe := filepath.Join(filepath.Dir(ffmpeg), "ffprobe")
	for _, scenario := range []string{"h264", "av1", "h264-negative", "av1-negative"} {
		t.Run(scenario, func(t *testing.T) {
			codec := strings.Split(scenario, "-")[0]
			sign := "+"
			if strings.HasSuffix(scenario, "-negative") {
				sign = "-"
			}
			encoder := []string{"-c:v", "libx264", "-threads:v", "1", "-bf", "2", "-g", "15"}
			if codec == "av1" {
				encoder = []string{"-c:v", "libaom-av1", "-threads:v", "1", "-cpu-used", "8", "-lag-in-frames", "0"}
			}
			args := []string{"-hide_banner", "-loglevel", "warning", "-xerror", "-copyts", "-f", "lavfi", "-i", "testsrc2=size=128x128:rate=30:duration=0.4", "-f", "lavfi", "-i", "sine=frequency=1000:sample_rate=48000:duration=0.4", "-map", "0:v", "-map", "1:a", "-vf", "setpts=PTS" + sign + "15", "-af", "asetpts=PTS" + sign + "6000", "-fps_mode", "passthrough"}
			args = append(args, encoder...)
			args = append(args, "-c:a", "aac", "-f", "mp4", "-movflags", "delay_moov+default_base_moof+frag_every_frame", "-min_frag_duration", "1", "-use_editlist", "1", "-movie_timescale", "48000", "-avoid_negative_ts", "disabled", "pipe:1")
			source := runMediaCommand(t, ffmpeg, nil, args...)
			oracle := probeMedia(t, ffprobe, source)
			if sign == "-" {
				negative := false
				for _, p := range oracle.Packets {
					negative = negative || p.DTS < 0
				}
				if !negative {
					t.Fatal("FFmpeg не сохранил отрицательное начало теста")
				}
			}
			if codec == "h264" {
				production, err := NewDemuxer(bytes.NewReader(source), DemuxOptions{Limits: testLimits(), SourceEpoch: 51, VideoRevision: 2, AudioRevision: 3})
				if err != nil {
					t.Fatal(err)
				}
				if production.Init().Video.Info().Video.FrameRate != (core.FrameRate{Numerator: 30, Denominator: 1}) || production.Init().Audio.Info().Audio.SampleRateHz != 48000 {
					t.Fatal("VUI/ASC не восстановлены")
				}
			}
			d, err := NewDemuxer(shortReader{bytes.NewReader(source), 5}, DemuxOptions{Limits: testLimits(), ResolveConfig: func(desc TrackDescription) (media.CodecConfig, error) { return observedConfig(t, oracle.Streams, desc) }})
			if err != nil {
				t.Fatal(err)
			}
			var packets []media.Packet
			for {
				p, err := d.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				packets = append(packets, p)
			}
			comparePackets(t, oracle, packets)
			init := d.Init()
			init.Origin = OriginMapping{OffsetTicks: 3, TimeBase: media.TimeBase{Numerator: 1, Denominator: 1}}
			var output bytes.Buffer
			m, err := NewMuxer(shortWriter{&output, 7}, init, testLimits())
			if err != nil {
				t.Fatal(err)
			}
			v := NewFFmpegTimingValidatorForOrigin(init.Origin)
			for _, p := range packets {
				if err = v.Validate(p); err != nil {
					t.Fatal(err)
				}
				if err = m.WritePacket(p); err != nil {
					t.Fatal(err)
				}
			}
			remuxed := probeMedia(t, ffprobe, output.Bytes())
			comparePackets(t, remuxed, packets)
			movieScale, err := MovieTimeScale(init.Video.Info().TimeBase, init.Audio.Info().TimeBase)
			if err != nil {
				t.Fatal(err)
			}
			transported := runMediaCommand(t, ffmpeg, output.Bytes(), "-hide_banner", "-loglevel", "warning", "-xerror", "-copyts", "-i", "pipe:0", "-map", "0:v", "-map", "0:a", "-c", "copy", "-f", "mp4", "-movflags", "delay_moov+default_base_moof+frag_every_frame", "-min_frag_duration", "1", "-use_editlist", "1", "-movie_timescale", strconv.FormatUint(uint64(movieScale), 10), "-avoid_negative_ts", "disabled", "pipe:1")
			comparePackets(t, probeMedia(t, ffprobe, transported), packets)
			returned, err := NewDemuxer(bytes.NewReader(transported), DemuxOptions{Limits: testLimits(), ResolveConfig: func(desc TrackDescription) (media.CodecConfig, error) { return observedConfig(t, oracle.Streams, desc) }})
			if err != nil {
				t.Fatal(err)
			}
			initialDescriptions, returnedDescriptions := d.Descriptions(), returned.Descriptions()
			for index, desc := range initialDescriptions {
				if !bytes.Equal(desc.Headers, returnedDescriptions[index].Headers) {
					t.Fatalf("FFmpeg изменил extradata %s", desc.Track)
				}
			}
			var returnedPackets []media.Packet
			for {
				p, err := returned.Next()
				if err == io.EOF {
					break
				}
				if err != nil {
					t.Fatal(err)
				}
				returnedPackets = append(returnedPackets, p)
			}
			comparePackets(t, oracle, returnedPackets)
			decoded := runMediaCommand(t, ffmpeg, transported, "-hide_banner", "-loglevel", "warning", "-xerror", "-i", "pipe:0", "-map", "0:v", "-map", "0:a", "-f", "framemd5", "pipe:1")
			if bytes.Count(decoded, []byte("\n")) < 20 {
				t.Fatal("недостаточно декодированных кадров")
			}
		})
	}
}
func comparePackets(t *testing.T, oracle probeResult, packets []media.Packet) {
	t.Helper()
	byTrack := map[media.Track][]probePacket{}
	bases := map[media.Track]media.TimeBase{}
	indices := map[int]media.Track{}
	for _, s := range oracle.Streams {
		track := media.Track(s.CodecType)
		indices[s.Index] = track
		bases[track] = rationalBase(t, s.TimeBase)
	}
	for _, p := range oracle.Packets {
		track := indices[p.StreamIndex]
		byTrack[track] = append(byTrack[track], p)
	}
	seen := map[media.Track]int{}
	for _, p := range packets {
		track := p.Config().Info().Track
		index := seen[track]
		seen[track]++
		if index >= len(byTrack[track]) {
			t.Fatal("лишний пакет")
		}
		expected := byTrack[track][index]
		base := p.Config().Info().TimeBase
		info := p.Info()
		if timingValue(info.PTS, base).Cmp(timingValue(expected.PTS, bases[track])) != 0 || timingValue(info.DTS, base).Cmp(timingValue(expected.DTS, bases[track])) != 0 {
			t.Fatalf("%s[%d]: Go PTS/DTS=%s/%s, ffprobe=%s/%s", track, index, timingValue(info.PTS, base), timingValue(info.DTS, base), timingValue(expected.PTS, bases[track]), timingValue(expected.DTS, bases[track]))
		}
	}
	for track, expected := range byTrack {
		if seen[track] != len(expected) {
			t.Fatalf("число %s: %d != %d", track, seen[track], len(expected))
		}
	}
}
