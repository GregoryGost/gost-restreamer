// Package encoding преобразует проверенный профиль в параметры кодировщиков FFmpeg.
package encoding

import (
	"strings"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// Target определяет, строятся ли аргументы для текущего эфира или для файловой подготовки заглушки.
type Target uint8

const (
	// Live выбирает кодирование текущего эфира с учётом режима LIVE-аудио.
	Live Target = iota + 1
	// Prepared выбирает файловую подготовку заглушки с собственной тишиной выбранного аудиокодека.
	Prepared
)

// Plan содержит только параметры кодеков; транспортом и фильтрами владеет вызывающий.
type Plan struct { /* Video содержит аргументы видеокодировщика FFmpeg. Audio содержит аргументы аудиокодировщика либо копирования LIVE-аудио. */
	Video, Audio []string
}

// Compile проверяет полный профиль p и строит параметры кодеков для target без изменения p.
// Live допустим только для transcode и учитывает LiveAudioMode; Prepared создаёт собственное аудио.
// Фильтры, транспорт и физический формат кадров остаются у вызывающего построителя.
func Compile(p core.GroupProfile, target Target) (Plan, error) {
	// err сохраняет отказ полной валидации без материализации отсутствующих настроек.
	if err := p.Validate(); err != nil {
		return Plan{}, err
	}
	if target != Live && target != Prepared || target == Live && p.Mode != core.ModeTranscode {
		return Plan{}, core.ErrInvalid
	}
	// video содержит выбранный видеорецепт; err сообщает о недопустимом кодировщике для target.
	video, err := compileVideo(p, target)
	if err != nil {
		return Plan{}, err
	}
	// audio содержит копирование LIVE либо конечный рецепт выбранной аудиопривязки.
	audio, err := compileAudio(p, target)
	if err != nil {
		return Plan{}, err
	}
	return Plan{Video: video, Audio: audio}, nil
}

// UsesNativeAAC71 сообщает, создаёт ли проверенный профиль собственный AAC LC 7.1 в данном
// режиме. Само совпадение профиля не разрешает менять входные заголовки: вызывающий применяет
// результат только к свежему выходу своего кодировщика. LIVE copy никогда не получает такой допуск.
// p задаёт проверяемый рецепт, target определяет подготовку либо обработку эфира.
func UsesNativeAAC71(p core.GroupProfile, target Target) bool {
	return p.Validate() == nil &&
		p.Encoding.AudioEncoder == core.AudioEncoderAAC && p.Format.Audio.Codec == core.CodecAAC &&
		p.Format.Audio.CodecProfile == "LC" && p.Format.Audio.ChannelLayout == "7.1" && p.Format.Audio.Channels == 8 &&
		(target == Prepared || target == Live && p.Mode == core.ModeTranscode && p.EffectiveLiveAudioMode() == core.LiveAudioTranscode)
}

// Преобразует уже проверенные пары name=value, разделённые пробелами, в отдельные аргументы -name и
// value без запуска оболочки.
func appendCustom(args []string, custom string) []string {
	// pair содержит очередную проверенную пару пользовательского параметра name=value.
	for pair := range strings.FieldsSeq(custom) {
		// name содержит имя пользовательского параметра FFmpeg без дефиса. value содержит его
		// значение.
		name, value, _ := strings.Cut(pair, "=")
		args = append(args, "-"+name, value)
	}
	return args
}
