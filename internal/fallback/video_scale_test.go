package fallback

import (
	"math"
	"slices"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

func TestPreparedVideoScalePreservesRangeAndChecksFrameStep(t *testing.T) {
	for _, tc := range []struct {
		name              string
		n, d, movie, want uint32
	}{
		{"low-rational", 75, 2, 48000, 48000},
		{"normal-rational", 30000, 1001, 240000, 30000},
		{"large-denominator", 32749, 65536, 1571952000, 32749},
		{"larger-denominator", 32749, 131072, 1571952000, 32749},
		{"low-step-last-valid", 1, 44739, 48000, 48000},
		{"low-step-first-invalid", 1, 44740, 48000, 0},
		{"exact-int32-step", 1000, math.MaxInt32, 48000, 1000},
		{"step-over-int32", 1000, math.MaxInt32 + 1, 48000, 0},
		{"not-divisible", 75, 2, 48001, 0},
		{"invalid-fps", 0, 1, 48000, 0},
		{"zero-movie", 30, 1, 0, 0},
		{"overflow-movie", 30, 1, math.MaxUint32, 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := preparedVideoScale(core.FrameRate{Numerator: tc.n, Denominator: tc.d}, tc.movie)
			if got != tc.want || (err != nil) != (tc.want == 0) {
				t.Fatal(got, err)
			}
		})
	}
}

func TestPreparedArgumentsUseExactVideoClockWithoutExpandingHighScale(t *testing.T) {
	for _, rate := range []core.FrameRate{{Numerator: 75, Denominator: 2}, {Numerator: 32749, Denominator: 65536}, {Numerator: 32749, Denominator: 131072}} {
		profile := selfProfile(core.CodecH264)
		profile.Format.Video.FrameRate = rate
		loop, err := EffectiveLoopDuration(time.Second, profile, 30*24*time.Hour)
		if err != nil {
			t.Fatal(err)
		}
		command, err := ProductionBuilder("ffmpeg")(GenerateRequest{Source: Source{Kind: core.AssetImage, Duration: time.Second, Demuxer: "png_pipe"}, Profile: profile, LoopDuration: loop})
		if err != nil {
			t.Fatal(err)
		}
		want := "32749"
		if rate.Numerator == 75 {
			want = "48000"
		}
		at := slices.Index(command.Args, "-video_track_timescale")
		if at < 0 || command.Args[at+1] != want {
			t.Fatal(command.Args)
		}
	}
}
