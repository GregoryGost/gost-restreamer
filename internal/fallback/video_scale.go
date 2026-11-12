package fallback

import (
	"fmt"
	"math"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// preparedVideoScale сохраняет точную сетку кадров и исключает грубую шкалу,
// в которой FFmpeg n7.1.5 вычисляет duration как одну единицу шкалы вместо полного кадра.
func preparedVideoScale(fps core.FrameRate, movieScale uint32) (uint32, error) {
	if fps.Validate() != nil || fps.Numerator > math.MaxInt32 || movieScale == 0 || movieScale > math.MaxInt32 {
		return 0, ErrInvalid
	}
	// scale содержит выбираемую шкалу видеодорожки: числитель FPS либо более подробную шкалу
	// контейнера.
	scale := fps.Numerator
	if scale < 1000 {
		scale = movieScale
	}
	if scale%fps.Numerator != 0 {
		return 0, ErrInvalid
	}
	// step содержит точную длительность видеокадра в отсчётах выбранной шкалы.
	step := uint64(fps.Denominator) * uint64(scale/fps.Numerator)
	if step == 0 || step > math.MaxInt32 {
		return 0, fmt.Errorf("%w: длительность кадра превышает диапазон FFmpeg", ErrInvalid)
	}
	return scale, nil
}
