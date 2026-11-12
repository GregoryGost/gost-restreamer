package fallback

import (
	"errors"
	"math"
	"strings"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// Проверяет округление до общей сетки видео, AAC и микросекунд CLI, включая дробную частоту и
// превышение предельной длины.
func TestDerivedLoopDurationUsesFullFramesAACAndExactCLITime(t *testing.T) {
	// fps выбирает проверяемую целую частоту видео 30 или 60 кадров в секунду.
	for _, fps := range []uint32{30, 60} {
		// p содержит профиль H.264 с выбранной частотой кадров.
		p := selfProfile(core.CodecH264)
		p.Format.Video.FrameRate.Numerator = fps
		// source содержит исходную длительность, округляемую до общей полной сетки A/V.
		for _, source := range []time.Duration{time.Second, 1133333 * time.Microsecond, 1600 * time.Millisecond} {
			// got содержит вычисленную длину петли. err сообщает о невозможности
			// выравнивания в заданный предел.
			got, err := EffectiveLoopDuration(source, p, 2*time.Second)
			if err != nil || got != 1600*time.Millisecond {
				t.Fatal(fps, source, got, err)
			}
			if uint64(got)*uint64(fps)%uint64(time.Second) != 0 || uint64(got)*48000%(1024*uint64(time.Second)) != 0 {
				t.Fatal("нецелое число кадров A/V")
			}
		}
	}
	// p содержит профиль для проверки предельных и дробных частот кадров.
	p := selfProfile(core.CodecH264)
	// err содержит результат выравнивания при слишком малой максимальной длительности.
	if _, err := EffectiveLoopDuration(time.Second, p, time.Second); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
	p.Format.Video.FrameRate = core.FrameRate{Numerator: 30000, Denominator: 1001}
	// err содержит результат выравнивания дробного FPS в пределах одной минуты.
	if _, err := EffectiveLoopDuration(time.Second, p, time.Minute); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
	// got содержит точную длину петли для частоты 30000/1001. err сообщает об отказе вычисления.
	if got, err := EffectiveLoopDuration(time.Second, p, 2*time.Minute); err != nil || got != 64064*time.Millisecond {
		t.Fatal(got, err)
	}
	p.Format.Video.FrameRate = core.FrameRate{Numerator: math.MaxUint32, Denominator: math.MaxUint32 - 1}
	// err содержит результат выравнивания при почти предельной дроби FPS.
	if _, err := EffectiveLoopDuration(time.Second, p, time.Minute); err == nil {
		t.Fatal("непредставимая короткая сетка принята")
	}
}

// Проверяет явное дополнение видео и аудио до длины петли без изменения измеренной длительности
// источника.
func TestProductionPaddingIsExplicitAndSourceDurationRemainsOriginal(t *testing.T) {
	// source содержит проверенный секундный видеоисточник для явного дополнения до петли.
	source := Source{Kind: core.AssetVideo, Duration: time.Second, Demuxer: "mov"}
	// p содержит целевой профиль H.264 для подготовки.
	p := selfProfile(core.CodecH264)
	// cmd содержит построенную команду подготовки петли 1,6 с. err сообщает об отказе построения.
	cmd, err := ProductionBuilder("ffmpeg")(GenerateRequest{Source: source, Profile: p, LoopDuration: 1600 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	// args содержит объединённые аргументы для проверки дополнения видео и аудио.
	args := strings.Join(cmd.Args, " ")
	// value содержит очередной обязательный фрагмент аргументов дополнения до длины петли.
	for _, value := range []string{"tpad=stop_mode=clone:stop=-1", "apad=pad_dur=1.600000", "-t 1.600000"} {
		if !strings.Contains(args, value) {
			t.Fatal(args)
		}
	}
	if source.Duration != time.Second {
		t.Fatal("исходная длина изменилась")
	}
}
