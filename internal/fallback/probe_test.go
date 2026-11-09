package fallback

import (
	"bytes"
	"context"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

type probeJSONRunner struct {
	data []byte
	err  error
}

func TestSourceAndVerificationRejectUnmeasuredOrExcessiveMetadata(t *testing.T) {
	for _, scenario := range []struct{ name, old, new string }{
		{"duration-unknown", "1.600000", "N/A"},
		{"duration-negative", "1.600000", "-1"},
		{"duration-limit", "1.600000", "61"},
		{"width-limit", "\"width\":128", "\"width\":5000"},
		{"height-empty", "\"height\":128", "\"height\":0"},
		{"fps-unknown", "30/1", "0/0"},
		{"unsupported-pixel", "yuv420p", "yuv420p10le"},
		{"audio-rate-invalid", "\"48000\"", "\"invalid\""},
		{"audio-codec-invalid", "\"aac\"", "\"mp3\""},
		{"video-codec-invalid", "\"h264\"", "\"vp9\""},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			p := Prober{Binary: "probe", Runner: probeJSONRunner{data: []byte(strings.ReplaceAll(selfProbeJSON, scenario.old, scenario.new))}, Limits: selfProbeLimits()}
			f := selfFragmented(t, "valid")
			if _, err := p.Source(context.Background(), f, core.AssetVideo); err == nil {
				t.Fatal("непригодный источник принят")
			}
			if _, err := p.Verify(context.Background(), f, selfProfile(core.CodecH264)); err == nil {
				t.Fatal("непригодный результат принят")
			}
		})
	}
	p := Prober{Binary: "probe", Runner: probeJSONRunner{data: []byte(selfProbeJSON)}, Limits: selfProbeLimits()}
	p.Limits.MaxPackets = 1
	profile := selfProfile(core.CodecH264)
	profile.Format.Video.Color = core.ColorInfo{Range: "limited", Primaries: "unspecified", Transfer: "unspecified", Matrix: "unspecified"}
	if _, err := p.Verify(context.Background(), selfFragmented(t, "valid"), profile); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
}

func TestSourceRequiresOneVideoAndAtMostOneAudioInSupportedContainer(t *testing.T) {
	video := `{"codec_type":"video","start_time":"0.000000","codec_name":"h264","profile":"High","width":128,"height":128,"avg_frame_rate":"30/1","pix_fmt":"yuv420p","level":11}`
	audio := `{"codec_type":"audio","start_time":"0.000000","codec_name":"aac","profile":"LC","sample_rate":"48000","channels":2,"channel_layout":"stereo"}`
	valid := []byte(`{"streams":[` + video + `,` + audio + `],"format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2","duration":"1.6"}}`)
	p := Prober{Binary: "probe", Runner: probeJSONRunner{data: valid}, Limits: selfProbeLimits()}
	s, err := p.Source(context.Background(), selfFragmented(t, "valid"), core.AssetVideo)
	if err != nil || s.Demuxer != "mov" || s.Format.Video == nil || s.Format.Audio == nil {
		t.Fatalf("допустимые video+audio с нулевым началом отклонены: %+v, %v", s, err)
	}
	for _, scenario := range []struct{ name, streams, container string }{
		{"no-video", audio, "mov,mp4,m4a,3gp,3g2,mj2"},
		{"duplicate-video", video + "," + video, "mov,mp4,m4a,3gp,3g2,mj2"},
		{"too-many-tracks", video + "," + audio + "," + audio, "mov,mp4,m4a,3gp,3g2,mj2"},
		{"unknown-track", video + `,{"codec_type":"subtitle","start_time":"0.000000"}`, "mov,mp4,m4a,3gp,3g2,mj2"},
		{"external-playlist", video, "hls"},
		{"empty", "", "mov,mp4,m4a,3gp,3g2,mj2"},
	} {
		t.Run(scenario.name, func(t *testing.T) {
			data := []byte(`{"streams":[` + scenario.streams + `],"format":{"format_name":"` + scenario.container + `","duration":"1.6"}}`)
			p := Prober{Binary: "probe", Runner: probeJSONRunner{data: data}, Limits: selfProbeLimits()}
			if _, err := p.Source(context.Background(), selfFragmented(t, "valid"), core.AssetVideo); err == nil {
				t.Fatal("непригодный контейнер принят")
			}
		})
	}
	matroska := Prober{Binary: "probe", Runner: probeJSONRunner{data: []byte(strings.Replace(selfProbeJSON, "mov,mp4,m4a,3gp,3g2,mj2", "matroska,webm", 1))}, Limits: selfProbeLimits()}
	if s, err := matroska.Source(context.Background(), selfFragmented(t, "valid"), core.AssetVideo); err != nil || s.Demuxer != "matroska" {
		t.Fatal(s, err)
	}
}

func (r probeJSONRunner) Run(_ context.Context, c Command, input *os.File, out io.Writer) (RunResult, error) {
	if input == nil || c.Binary != "probe" {
		return RunResult{}, ErrInvalid
	}
	if r.err != nil {
		return RunResult{}, r.err
	}
	_, err := out.Write(r.data)
	return RunResult{}, err
}

const selfProbeJSON = `{"streams":[{"codec_type":"video","start_time":"0.000000","codec_name":"h264","profile":"High","width":128,"height":128,"avg_frame_rate":"30/1","pix_fmt":"yuv420p","level":11,"color_range":"tv","color_primaries":"unspecified","color_transfer":"unspecified","color_space":"unspecified"},{"codec_type":"audio","start_time":"0.000000","codec_name":"aac","profile":"LC","sample_rate":"48000","channels":2,"channel_layout":"stereo"}],"format":{"format_name":"mov,mp4,m4a,3gp,3g2,mj2","duration":"1.600000"}}`

func selfFragmented(t *testing.T, scenario string) *os.File {
	t.Helper()
	headers, err := hex.DecodeString("0164000bffe100186764000bacd942046c0440000003004000000f03c50a658001000668ebe3cb22c0fdf8f800")
	if err != nil {
		t.Fatal(err)
	}
	p := selfProfile(core.CodecH264)
	p.Format.Video.Color = core.ColorInfo{Range: "limited", Primaries: "unspecified", Transfer: "unspecified", Matrix: "unspecified"}
	v, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: 1, Revision: 1, TimeBase: media.TimeBase{Numerator: 1, Denominator: 30}, Video: p.Format.Video}, headers)
	if err != nil {
		t.Fatal(err)
	}
	a, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackAudio, SourceEpoch: 1, Revision: 1, TimeBase: media.TimeBase{Numerator: 1, Denominator: 48000}, Audio: p.Format.Audio}, []byte{0x11, 0x90})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	mux, err := fmp4.NewMuxer(&out, fmp4.Init{Video: v, Audio: a}, selfProbeLimits().Container)
	if err != nil {
		t.Fatal(err)
	}
	for i := range 3 {
		payload := []byte{0, 0, 0, 3, 0x65, 0xb8, 0x40}
		if scenario == "false-key" {
			payload[4] = 0x41
		}
		if scenario == "bad-nal" {
			payload = []byte{0, 0, 0, 99, 0x65}
		}
		duration := int64(1)
		if scenario == "video-duration" {
			duration = 2
		}
		packet, err := media.NewPacket(media.PacketInfo{PTS: int64(i), DTS: int64(i), DurationTicks: duration, Keyframe: true}, v, payload)
		if err != nil {
			t.Fatal(err)
		}
		if err = mux.WritePacket(packet); err != nil {
			t.Fatal(err)
		}
		if scenario == "no-audio" {
			continue
		}
		duration = 1024
		if scenario == "audio-duration" {
			duration = 1000
		}
		au := []byte{0x21, 0x10, 0x04, 0x60, 0x8c, 0x1c}
		if scenario == "adts" {
			au = []byte{0xff, 0xf1, 0, 0}
		}
		packet, err = media.NewPacket(media.PacketInfo{PTS: int64(i) * 1024, DTS: int64(i) * 1024, DurationTicks: duration}, a, au)
		if err != nil {
			t.Fatal(err)
		}
		if err = mux.WritePacket(packet); err != nil {
			t.Fatal(err)
		}
	}
	f, err := os.CreateTemp(t.TempDir(), "variant")
	if err != nil {
		t.Fatal(err)
	}
	selfClose(t, f)
	if _, err = f.Write(out.Bytes()); err != nil {
		t.Fatal(err)
	}
	return f
}
func TestProbeRequiresActualIndependentAUsAndCompleteDurations(t *testing.T) {
	for _, scenario := range []string{"valid", "false-key", "bad-nal", "video-duration", "audio-duration", "no-audio", "adts"} {
		t.Run(scenario, func(t *testing.T) {
			f := selfFragmented(t, scenario)
			p := selfProfile(core.CodecH264)
			p.Format.Video.Color = core.ColorInfo{Range: "limited", Primaries: "unspecified", Transfer: "unspecified", Matrix: "unspecified"}
			probe := Prober{Binary: "probe", Runner: probeJSONRunner{data: []byte(selfProbeJSON)}, Limits: selfProbeLimits()}
			actual, err := probe.Verify(context.Background(), f, p)
			if scenario == "valid" {
				if err != nil || actual.Video.Width != 128 {
					t.Fatal(actual, err)
				}
			} else if err == nil {
				t.Fatalf("некорректный файл принят: %s", scenario)
			}
		})
	}
}
func TestProbeSourceJSONAndLimitsFailClosed(t *testing.T) {
	f := selfFragmented(t, "valid")
	p := Prober{Binary: "probe", Runner: probeJSONRunner{data: []byte(selfProbeJSON)}, Limits: selfProbeLimits()}
	s, err := p.Source(context.Background(), f, core.AssetVideo)
	if err != nil || s.Duration <= 0 || s.Demuxer != "mov" || s.Format.Audio == nil {
		t.Fatal(s, err)
	}
	p.Limits.MaxJSONBytes = 2
	if _, err = p.Source(context.Background(), f, core.AssetVideo); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
	p.Limits = selfProbeLimits()
	p.Runner = probeJSONRunner{data: []byte("{")}
	if _, err = p.Source(context.Background(), f, core.AssetVideo); err == nil {
		t.Fatal("повреждённый JSON принят")
	}
	p.Runner = probeJSONRunner{err: io.ErrUnexpectedEOF}
	if _, err = p.Source(context.Background(), f, core.AssetVideo); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal(err)
	}
	for _, rate := range []string{"", "0/1", "1/0", "1/2/3", "x/2", "2/x"} {
		if _, err = parseRate(rate); err == nil {
			t.Fatal(rate)
		}
	}
}
