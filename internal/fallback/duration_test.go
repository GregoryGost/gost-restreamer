package fallback

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

func TestDerivedLoopDurationUsesFullFramesAACAndExactCLITime(t *testing.T) {
	for _, fps := range []uint32{30, 60} {
		p := selfProfile(core.CodecH264)
		p.Format.Video.FrameRate.Numerator = fps
		for _, source := range []time.Duration{time.Second, 1133333 * time.Microsecond, 1600 * time.Millisecond} {
			got, err := EffectiveLoopDuration(source, p, 2*time.Second)
			if err != nil || got != 1600*time.Millisecond {
				t.Fatal(fps, source, got, err)
			}
			if uint64(got)*uint64(fps)%uint64(time.Second) != 0 || uint64(got)*48000%(1024*uint64(time.Second)) != 0 {
				t.Fatal("нецелое число кадров A/V")
			}
		}
	}
	p := selfProfile(core.CodecH264)
	if _, err := EffectiveLoopDuration(time.Second, p, time.Second); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
	p.Format.Video.FrameRate = core.FrameRate{Numerator: 30000, Denominator: 1001}
	if _, err := EffectiveLoopDuration(time.Second, p, time.Minute); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
	if got, err := EffectiveLoopDuration(time.Second, p, 2*time.Minute); err != nil || got != 64064*time.Millisecond {
		t.Fatal(got, err)
	}
	p.Format.Video.FrameRate = core.FrameRate{Numerator: math.MaxUint32, Denominator: math.MaxUint32 - 1}
	if _, err := EffectiveLoopDuration(time.Second, p, time.Minute); err == nil {
		t.Fatal("непредставимая короткая сетка принята")
	}
}
func TestProductionPaddingIsExplicitAndSourceDurationRemainsOriginal(t *testing.T) {
	source := Source{Kind: core.AssetVideo, Duration: time.Second, Demuxer: "mov"}
	p := selfProfile(core.CodecH264)
	cmd, err := ProductionBuilder("ffmpeg")(GenerateRequest{Source: source, Profile: p, LoopDuration: 1600 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	args := strings.Join(cmd.Args, " ")
	for _, value := range []string{"tpad=stop_mode=clone:stop=-1", "apad=pad_dur=1.600000", "-t 1.600000"} {
		if !strings.Contains(args, value) {
			t.Fatal(args)
		}
	}
	if source.Duration != time.Second {
		t.Fatal("исходная длина изменилась")
	}
}
