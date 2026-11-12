// Package encoding преобразует проверенный профиль в параметры кодировщиков FFmpeg.
package encoding

import (
	"fmt"
	"strconv"
	"strings"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// Target определяет, строятся ли аргументы для текущего эфира или для файловой подготовки заглушки.
type Target uint8

const (
	// Live выбирает кодирование текущего эфира с учётом режима LIVE-аудио.
	Live Target = iota + 1
	// Prepared выбирает файловую подготовку заглушки с кодированием AAC-тишины.
	Prepared
)

// Plan содержит только параметры кодеков; транспортом и фильтрами владеет вызывающий.
type Plan struct { /* Video содержит аргументы видеокодировщика FFmpeg. Audio содержит аргументы аудиокодировщика либо копирования LIVE-аудио. */
	Video, Audio []string
}

// Compile проверяет профиль и строит отдельные аргументы видео и аудио FFmpeg в Plan. Live разрешён
// только для transcode/H264; Prepared также поддерживает подготовку AV1. Неподдерживаемый target или
// профиль возвращает ошибку. Прежний профиль без Encoding сохраняет прежние параметры кодировщика.
// В Live режим копирования аудио даёт -c:a copy; для подготовки и явно перекодируемого LIVE-аудио
// формируются параметры AAC. Транспорт и фильтры добавляет вызывающий код.
func Compile(p core.GroupProfile, target Target) (Plan, error) {
	// err сообщает о некорректном профиле группы.
	if err := p.Validate(); err != nil {
		return Plan{}, err
	}
	if target != Live && target != Prepared || target == Live && p.Mode != core.ModeTranscode {
		return Plan{}, core.ErrInvalid
	}
	// v содержит выходной формат видео. a содержит выходной формат аудио. e содержит ограничения
	// битрейта и GOP. settings содержит явные параметры кодировщиков.
	v, a, e, settings := p.Format.Video, p.Format.Audio, p.VideoEncoding, p.Encoding
	// legacy отмечает профиль без версионированных параметров кодировщиков, для которого сохраняется
	// прежний рецепт.
	legacy := settings.Version == 0
	// n преобразует числовой параметр кодировщика в десятичный аргумент FFmpeg.
	n := func(v uint64) string {
		// Преобразует беззнаковое значение параметра кодировщика в десятичную строку.
		return strconv.FormatUint(v, 10)
	}
	// encoder содержит имя видеокодировщика из профиля либо прежнего рецепта.
	encoder := settings.VideoEncoder
	if legacy {
		encoder = "h264_nvenc"
		if v.Codec == core.CodecAV1 {
			encoder = "libsvtav1"
		}
	}
	// video накапливает аргументы видеокодировщика и описания цвета.
	video := []string{"-c:v", encoder, "-b:v", n(e.TargetBitrateBPS)}
	if v.Codec != core.CodecAV1 || legacy || settings.RateControl == "cbr" {
		video = append(video, "-maxrate:v", n(e.MaxBitrateBPS), "-bufsize:v", n(e.BufferBits))
	}
	video = append(video, "-g:v", n(uint64(e.GOPFrames)), "-flags:v", "+cgop")
	switch v.Codec {
	case core.CodecH264:
		video = append(video, "-profile:v", v.CodecProfile, "-bf:v", n(uint64(e.MaxBFrames)), "-no-scenecut", "1", "-forced-idr", "1")
		if !legacy || target == Live {
			// rc содержит выбранный режим регулирования битрейта NVENC с учётом прежнего
			// профиля.
			rc := settings.RateControl
			if legacy {
				rc = "vbr"
			}
			video = append(video, "-rc", rc)
		}
		if !legacy {
			// nv содержит явные настройки NVENC проверенного профиля.
			nv := settings.NVENC
			// flag преобразует логический параметр NVENC в допустимый строковый аргумент 0
			// или 1.
			flag := func(v bool) string {
				// Преобразует логический параметр NVENC в строковое значение 1 или 0.

				if v {
					return "1"
				}
				return "0"
			}
			video = append(video, "-preset", nv.Preset, "-tune", nv.Tuning, "-multipass", nv.Multipass,
				"-rc-lookahead", strconv.Itoa(nv.LookaheadFrames), "-spatial-aq", flag(nv.EnableSpatialAQ),
				"-temporal-aq", flag(nv.EnableTemporalAQ), "-aq-strength", strconv.Itoa(nv.AQStrength))
			video = appendCustom(video, settings.VideoCustom)
		}
	case core.CodecAV1:
		if target != Prepared {
			return Plan{}, core.ErrInvalid
		}
		// preset содержит числовой пресет SVT-AV1. params накапливает строку параметров SVT-AV1,
		// разделённых двоеточиями.
		preset, params := "8", "rc=2:mbr=0:pred-struct=1:scd=0:enable-overlays=0:enable-dg=0:hierarchical-levels=0:enable-tf=0"
		if !legacy {
			// svt содержит явные настройки кодировщика SVT-AV1.
			svt := settings.SVTAV1
			preset = strconv.Itoa(svt.Preset)
			// rc содержит числовой режим битрейта SVT-AV1. prediction содержит
			// соответствующую структуру предсказания кадров.
			rc, prediction := "2", "1"
			if settings.RateControl == "vbr" {
				rc, prediction = "1", "2"
			}
			// tuning содержит числовое значение режима настройки SVT-AV1.
			tuning := 1
			if svt.Tuning == "vq" {
				tuning = 0
			}
			params = fmt.Sprintf("rc=%s:mbr=0:pred-struct=%s:scd=0:enable-overlays=0:enable-dg=0:hierarchical-levels=0:enable-tf=0:aq-mode=%d:tune=%d", rc, prediction, svt.AQMode, tuning)
			if svt.LookaheadFrames >= 0 {
				params += ":lookahead=" + strconv.Itoa(svt.LookaheadFrames)
			}
			if settings.VideoCustom != "" {
				params += ":" + strings.ReplaceAll(settings.VideoCustom, " ", ":")
			}
		}
		video = append(video, "-svtav1-params", params, "-preset", preset)
	default:
		return Plan{}, core.ErrInvalid
	}
	if v.Level != "" && (target == Live || v.Level != "auto") {
		video = append(video, "-level:v", v.Level)
	}
	// rangeName содержит обозначение цветового диапазона FFmpeg после преобразования limited/full в
	// tv/pc.
	rangeName := v.Color.Range
	if rangeName == "limited" {
		rangeName = "tv"
	}
	if rangeName == "full" {
		rangeName = "pc"
	}
	video = append(video, "-color_range:v", rangeName, "-color_primaries:v", v.Color.Primaries, "-color_trc:v", v.Color.Transfer, "-colorspace:v", v.Color.Matrix)
	if target == Live && p.EffectiveLiveAudioMode() == core.LiveAudioCopy {
		return Plan{Video: video, Audio: []string{"-c:a", "copy"}}, nil
	}
	// audio содержит аргументы кодирования AAC с заданными битрейтом, частотой и каналами.
	audio := []string{"-c:a", "aac", "-profile:a", "aac_low", "-b:a", n(p.AudioEncoding.BitrateBPS), "-ar:a", n(uint64(a.SampleRateHz)), "-ac:a", n(uint64(a.Channels)), "-channel_layout:a", a.ChannelLayout}
	if !legacy {
		audio = appendCustom(audio, settings.AudioCustom)
	}
	return Plan{Video: video, Audio: audio}, nil
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
