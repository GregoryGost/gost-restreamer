package fallback

import (
	"fmt"
	"math/big"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// EffectiveLoopDuration округляет длину вверх до общей сетки полных кадров видео и AAC.
// Сетка также точно представима в микросекундах FFmpeg CLI; исходная длина не изменяется.
func EffectiveLoopDuration(source time.Duration, profile core.GroupProfile, maximum time.Duration) (time.Duration, error) {
	if source <= 0 || maximum <= 0 || source > maximum || profile.Validate() != nil {
		return 0, ErrInvalid
	}
	// fps содержит частоту кадров выходного профиля.
	fps := profile.Format.Video.FrameRate
	// vn содержит числитель длительности видеокадра в микросекундах до сокращения дроби.
	vn := uint64(time.Second/time.Microsecond) * uint64(fps.Denominator)
	// video задаёт минимальный целый интервал в микросекундах, содержащий полное число видеокадров.
	video := vn / gcd(vn, uint64(fps.Numerator))
	// an содержит числитель длительности AAC-единицы из 1024 сэмплов в микросекундах.
	an := uint64(1024) * uint64(time.Second/time.Microsecond)
	// audio задаёт минимальный целый интервал в микросекундах, содержащий полное число AAC-единиц.
	audio := an / gcd(an, uint64(profile.Format.Audio.SampleRateHz))
	// grid вычисляет общий период видео, AAC и микросекундной сетки, затем переводится в
	// наносекунды.
	grid := new(big.Int).SetUint64(video / gcd(video, audio))
	grid.Mul(grid, new(big.Int).SetUint64(audio))
	grid.Mul(grid, big.NewInt(int64(time.Microsecond)))
	if !grid.IsInt64() || grid.Int64() > int64(maximum) {
		return 0, ErrLimit
	}
	// steps содержит целое число общих периодов для округления исходной длительности вверх.
	steps := new(big.Int).Quo(big.NewInt(int64(source)-1), grid)
	steps.Add(steps, big.NewInt(1))
	// duration содержит выровненную длительность петли в наносекундах без промежуточного
	// переполнения.
	duration := steps.Mul(steps, grid)
	if !duration.IsInt64() || duration.Int64() > int64(maximum) {
		return 0, ErrLimit
	}
	return time.Duration(duration.Int64()), nil
}

// Представляет длительность в секундах с шестью десятичными знаками; доли меньше микросекунды
// отбрасываются.
func durationArgument(d time.Duration) string {
	return fmt.Sprintf("%d.%06d", d/time.Second, (d%time.Second)/time.Microsecond)
}
