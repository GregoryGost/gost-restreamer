package fallback

import (
	"math"
	"math/big"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
)

func TestPreparedPeriodPreservesReorderedPresentationAndDistinctDecodeEnds(t *testing.T) {
	for _, scenario := range []string{"valid", "signed-cts", "duplicate", "hole", "fraction", "decode-gap", "positive-origin", "audio-offset", "unequal", "av1-reorder"} {
		t.Run(scenario, func(t *testing.T) {
			format := selfProfile(core.CodecH264).Format
			format.Video.FrameRate = core.FrameRate{Numerator: 25, Denominator: 2}
			if scenario == "av1-reorder" {
				format.Video.Codec = core.CodecAV1
			}
			period := newPreparedPeriod(format)
			// Четыре кадра по 2/25 с и 15 AAC AU по 1024/48000 с имеют период 0,32 с.
			pts := []int64{0, 6, 2, 4}
			if scenario == "signed-cts" {
				pts = []int64{0, 6, 4, 2}
			}
			if scenario == "duplicate" {
				pts[2] = 6
			}
			if scenario == "hole" {
				pts[2] = 8
			}
			if scenario == "fraction" {
				pts[2] = 3
			}
			var err error
			for i, value := range pts {
				dts := int64(i*2 - 4)
				if scenario == "signed-cts" {
					dts += 2
				}
				if scenario == "decode-gap" && i == 2 {
					dts++
				}
				if scenario == "positive-origin" {
					dts += 6
				}
				if e := period.add(media.TrackVideo, media.TimeBase{Numerator: 1, Denominator: 25}, value, dts, 2); e != nil {
					err = e
					break
				}
			}
			for i := 0; err == nil && i < 15; i++ {
				if scenario == "unequal" && i == 14 {
					break
				}
				stamp := int64(i * 1024)
				if scenario == "audio-offset" {
					stamp += 1024
				}
				err = period.add(media.TrackAudio, media.TimeBase{Numerator: 1, Denominator: 48000}, stamp, stamp, 1024)
			}
			if err == nil {
				var duration *big.Rat
				duration, err = period.duration()
				if err == nil && duration.Cmp(big.NewRat(8, 25)) != 0 {
					t.Fatal(duration)
				}
			}
			if (err == nil) != (scenario == "valid" || scenario == "signed-cts") {
				t.Fatalf("ошибка периода: %v", err)
			}
		})
	}
}

func TestPreparedOriginUsesCommonGridAndCheckedCeiling(t *testing.T) {
	v, a := media.TimeBase{Numerator: 1, Denominator: 30000}, media.TimeBase{Numerator: 1, Denominator: 48000}
	origin, err := preparedOrigin(-2002, v, v, a)
	if err != nil || origin.OffsetTicks != 401 || origin.TimeBase != (media.TimeBase{Numerator: 1, Denominator: 6000}) {
		t.Fatal(origin, err)
	}
	if ticks(origin.OffsetTicks, origin.TimeBase).Cmp(big.NewRat(2002, 30000)) <= 0 {
		t.Fatal("дробный lead ошибочно принят за общую сетку")
	}
	if _, err = preparedOrigin(math.MinInt64, media.TimeBase{Numerator: math.MaxUint32, Denominator: 1}, v, a); err == nil {
		t.Fatal("переполнение origin принято")
	}
	if _, err = preparedOrigin(-1, v, media.TimeBase{}, a); err == nil {
		t.Fatal("некорректная шкала принята")
	}
}
