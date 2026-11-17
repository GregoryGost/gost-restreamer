package encoding

import (
	"fmt"
	"strings"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
)

// ValidateProfileVideoHeaders проверяет ограничения профиля p, отсутствующие в MediaFormat.
// video содержит фактическую конфигурацию выхода. Для H.264 level6 SPS должен
// подтверждать ограничение числа опорных кадров фиксированного рецепта; остальные заголовки проверяет Inspector.
func ValidateProfileVideoHeaders(p core.GroupProfile, video media.CodecConfig) error {
	if p.Format.Video.Codec != core.CodecH264 || !strings.HasPrefix(p.Format.Video.Level, "6.") {
		return nil
	}
	// actual содержит подтверждённые SPS размеры и число опорных кадров; err сообщает о повреждении.
	actual, err := codec.H264SPS(codec.VideoDescription{Headers: video.Headers(), Width: video.Info().Video.Width, Height: video.Info().Video.Height})
	if err != nil {
		return err
	}
	if actual.MaxNumRefFrames > 1 {
		return fmt.Errorf("%w: H.264 level6 требует не более одного опорного кадра", core.ErrInvalidFormat)
	}
	return nil
}
