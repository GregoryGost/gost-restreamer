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

// Проверяет одинаковую точную выходную шкалу и байты пакетов для эквивалентных сокращённой и
// несокращённой частот без изменения профиля.
func TestSwitcherClockReducesEquivalentFrameRates(t *testing.T) {
	// fixture предоставляет контрольные настройки переключателя; video содержит исходную
	// AV1-конфигурацию; audio содержит согласованную AAC-конфигурацию.
	fixture, video, audio, _ := switcherFixture(t)
	// config сохраняет общие настройки для сравнения двух представлений одной частоты.
	config := fixture.config

	// outputs собирает выходные пары пакетов для побайтного сравнения эквивалентных FPS.
	var outputs [][]media.Packet
	// rate содержит очередное рациональное представление частоты 30 кадров в секунду.
	for _, rate := range []core.FrameRate{
		{Numerator: 30, Denominator: 1},
		{Numerator: 30000030, Denominator: 1000001},
	} {
		t.Run(rateName(rate), func(t *testing.T) {
			// Проверяет сокращение выбранной эквивалентной дроби FPS к шкале 1/48000 и сохраняет выходные пакеты
			// для побайтного сравнения.

			// c содержит отдельный профиль с выбранным представлением частоты.
			c := config
			c.Profile.Format.Video.FrameRate = rate
			// s содержит переключатель выбранной частоты; err сохраняет результаты его
			// создания, конфигурации и подписки.
			s, err := NewSwitcher(c)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				// Закрывает Switcher после сценария эквивалентной дроби и сообщает ошибку через t.Error.

				// err сообщает об отказе закрытия переключателя после сценария
				// эквивалентной дроби.
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

			// info содержит копию описания AV1 для назначения проверяемого FPS.
			info := video.Info()
			info.Video.FrameRate = rate
			// v содержит входную видеоконфигурацию с выбранной рациональной частотой.
			v, err := media.NewCodecConfig(info, video.Headers())
			if err != nil {
				t.Fatal(err)
			}
			// sub содержит подписку на выход переключателя для получения контрольной пары.
			sub, err := s.Router().Subscribe(c.Queue)
			if err != nil {
				t.Fatal(err)
			}
			defer func() {
				// Закрывает подписку выходного Router после сценария и сообщает ошибку через t.Error.

				// err сообщает об отказе закрытия выходной подписки выбранного сценария.
				if err := sub.Close(); err != nil {
					t.Error(err)
				}
			}()

			// now задаёт одинаковое контрольное время поступления видео и аудио.
			now := time.Unix(100, 0)
			allowUnavailable(t, pushPacket(t, s, Fallback, 1, v, v, audio, 0, 0, 1600, true, now))
			allowUnavailable(t, pushPacket(t, s, Fallback, 1, audio, v, audio, 0, 0, 1024, false, now))
			outputs = append(outputs, []media.Packet{next(t, sub), next(t, sub)})
		})
	}

	if len(outputs) != 2 || len(outputs[0]) != len(outputs[1]) {
		t.Fatalf("ожидались две одинаковые пары пакетов: %d", len(outputs))
	}
	// i обозначает позицию пакета в двух сравниваемых выходных парах.
	for i := range outputs[0] {
		// left содержит пакет сокращённого FPS; right содержит соответствующий пакет
		// эквивалентной несокращённой частоты.
		left, right := outputs[0][i], outputs[1][i]
		if left.Info() != right.Info() || left.Config().Info().TimeBase != right.Config().Info().TimeBase || !bytes.Equal(left.Payload(), right.Payload()) {
			t.Fatalf("эквивалентные FPS дали разные выходные пакеты: %+v / %+v", left.Info(), right.Info())
		}
	}
}

// Проверяет отказ создания переключателя при частоте, для которой общая шкала дорожек не представима.
func TestSwitcherRejectsUnrepresentableFrameRateClock(t *testing.T) {
	// fixture предоставляет контрольные настройки для проверки непредставимой выходной шкалы.
	fixture, _, _, _ := switcherFixture(t)
	// c содержит профиль с частотой, вызывающей непредставимую общую шкалу.
	c := fixture.config
	c.Profile.Format.Video.FrameRate = core.FrameRate{Numerator: 4294967291, Denominator: 1}
	// err содержит ожидаемый ErrUnsupportedTiming при создании переключателя.
	if _, err := NewSwitcher(c); !errors.Is(err, fmp4.ErrUnsupportedTiming) {
		t.Fatalf("ожидалась ошибка непредставимой шкалы, получено %v", err)
	}
}

// Возвращает различимые имена двух контрольных представлений одной частоты кадров.
func rateName(rate core.FrameRate) string {
	if rate.Numerator == 30 && rate.Denominator == 1 {
		return "30/1"
	}
	return "30000030/1000001"
}
