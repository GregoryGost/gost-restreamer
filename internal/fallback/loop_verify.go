package fallback

import (
	"context"
	"errors"
	"fmt"
	"io"
	"math/big"
	"os"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

// VerifyLoopDuration сверяет обе фактические дорожки с явно вычисленной длиной петли.
func (p Prober) VerifyLoopDuration(ctx context.Context, file *os.File, expected time.Duration) error {
	if expected <= 0 {
		return ErrInvalid
	}
	// actual содержит фактический формат проверяемой петли. err сообщает об отказе определения
	// формата.
	actual, err := p.observedFormat(ctx, file)
	if err != nil {
		return err
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return err
	}
	// d читает обе дорожки fMP4 с конфигурациями на основе фактического формата.
	d, err := fmp4.NewDemuxer(contextReader{ctx, file}, fmp4.DemuxOptions{Limits: p.Limits.Container, SourceEpoch: 1, VideoRevision: 1, AudioRevision: 1, ResolveConfig: func(desc fmp4.TrackDescription) (media.CodecConfig, error) {
		// Соединяет фактический формат ffprobe с заголовками и шкалой дорожки fMP4, назначая SourceEpoch и
		// Revision равными 1 для проверки файла.

		// info объединяет описание дорожки контейнера с фактическим форматом для проверки файла.
		info := media.CodecConfigInfo{Track: desc.Track, SourceEpoch: 1, Revision: 1, TimeBase: desc.TimeBase}
		if desc.Track == media.TrackVideo {
			info.Video = actual.Video
		} else {
			info.Audio = actual.Audio
		}
		return media.NewCodecConfig(info, desc.Headers)
	}})
	if err != nil {
		return err
	}
	// period накапливает непрерывные временные границы дорожек и полный ряд PTS видео.
	period := newPreparedPeriod(actual)
	// n считает прочитанные пакеты для проверки общего ограничения.
	for n := uint64(0); ; n++ {
		// packet содержит очередной пакет петли. e сообщает об окончании файла или отказе
		// чтения.
		packet, e := d.Next()
		if errors.Is(e, io.EOF) {
			break
		}
		if e != nil {
			return e
		}
		if n >= p.Limits.MaxPackets {
			return ErrLimit
		}
		// track обозначает дорожку текущего пакета. base задаёт его единицу времени. info
		// содержит временные метки и длительность.
		track, base, info := packet.Config().Info().Track, packet.Config().Info().TimeBase, packet.Info()
		if e = period.add(track, base, info.PTS, info.DTS, info.DurationTicks); e != nil {
			return e
		}
	}
	// want содержит ожидаемую длительность петли в точных рациональных секундах.
	want := new(big.Rat).SetFrac64(int64(expected), int64(time.Second))
	// got содержит общий фактический период обеих дорожек в секундах.
	got, err := period.duration()
	if err != nil {
		return err
	}
	if got.Cmp(want) != 0 {
		return fmt.Errorf("%w: продолжительность не соответствует вычисленной петле %s", core.ErrNotReady, expected)
	}
	return nil
}
