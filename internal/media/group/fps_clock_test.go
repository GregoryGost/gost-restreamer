package group

import (
	"bytes"
	"errors"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

func TestSwitcherClockReducesEquivalentFrameRates(t *testing.T) {
	fixture, video, audio, _ := switcherFixture(t)
	config := fixture.config

	var outputs [][]media.Packet
	for _, rate := range []core.FrameRate{
		{Numerator: 30, Denominator: 1},
		{Numerator: 30000030, Denominator: 1000001},
	} {
		t.Run(rateName(rate), func(t *testing.T) {
			c := config
			c.Profile.Format.Video.FrameRate = rate
			s, err := NewSwitcher(c)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := s.Close(); err != nil {
					t.Error(err)
				}
			}()
			if s.base != (media.TimeBase{Numerator: 1, Denominator: 48000}) {
				t.Fatalf("получена шкала %v", s.base)
			}
			if s.config.Profile.Format.Video.FrameRate != rate {
				t.Fatalf("профиль изменён: %+v", s.config.Profile.Format.Video.FrameRate)
			}

			info := video.Info()
			info.Video.FrameRate = rate
			v, err := media.NewCodecConfig(info, video.Headers())
			if err != nil {
				t.Fatal(err)
			}
			sub, err := s.Router().Subscribe(c.Queue)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				if err := sub.Close(); err != nil {
					t.Error(err)
				}
			}()

			now := time.Unix(100, 0)
			allowUnavailable(t, pushPacket(t, s, Fallback, 1, v, v, audio, 0, 0, 1600, true, now))
			allowUnavailable(t, pushPacket(t, s, Fallback, 1, audio, v, audio, 0, 0, 1024, false, now))
			outputs = append(outputs, []media.Packet{next(t, sub), next(t, sub)})
		})
	}

	if len(outputs) != 2 || len(outputs[0]) != len(outputs[1]) {
		t.Fatalf("ожидались две одинаковые пары пакетов: %d", len(outputs))
	}
	for i := range outputs[0] {
		left, right := outputs[0][i], outputs[1][i]
		if left.Info() != right.Info() || left.Config().Info().TimeBase != right.Config().Info().TimeBase || !bytes.Equal(left.Payload(), right.Payload()) {
			t.Fatalf("эквивалентные FPS дали разные выходные пакеты: %+v / %+v", left.Info(), right.Info())
		}
	}
}

func TestSwitcherRejectsUnrepresentableFrameRateClock(t *testing.T) {
	fixture, _, _, _ := switcherFixture(t)
	c := fixture.config
	c.Profile.Format.Video.FrameRate = core.FrameRate{Numerator: 4294967291, Denominator: 1}
	if _, err := NewSwitcher(c); !errors.Is(err, fmp4.ErrUnsupportedTiming) {
		t.Fatalf("ожидалась ошибка непредставимой шкалы, получено %v", err)
	}
}

func rateName(rate core.FrameRate) string {
	if rate.Numerator == 30 && rate.Denominator == 1 {
		return "30/1"
	}
	return "30000030/1000001"
}
