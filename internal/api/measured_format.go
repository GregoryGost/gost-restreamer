package api

import (
	"strconv"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/danielgtaylor/huma/v2"
)

// MeasuredColorDTO сохраняет распознаваемые обозначения цветовых характеристик видео.
// Неизвестный непустой текст каждого поля передаётся как UNKNOWN, отсутствие — пустой строкой.
type MeasuredColorDTO struct {
	// Range содержит обозначение диапазона уровней цвета.
	Range string `json:"range"`
	// Primaries содержит обозначение цветовых первичных координат.
	Primaries string `json:"primaries"`
	// Transfer содержит обозначение передаточной характеристики цвета.
	Transfer string `json:"transfer"`
	// Matrix содержит обозначение матрицы преобразования цветовых компонентов.
	Matrix string `json:"matrix"`
}

// MeasuredVideoDTO передаёт полное измеренное описание видео. Строки содержат распознанное
// обозначение, UNKNOWN для неизвестного непустого текста либо пустоту до измерения.
type MeasuredVideoDTO struct {
	// Codec содержит идентификатор видеокодека.
	Codec string `json:"codec"`
	// Width содержит ширину видеокадра в пикселях.
	Width uint32 `json:"width"`
	// Height содержит высоту видеокадра в пикселях.
	Height uint32 `json:"height"`
	// FrameRate содержит несокращённую рациональную частоту кадров.
	FrameRate FrameRateDTO `json:"frame_rate"`
	// CodecProfile содержит точное обозначение профиля видеокодека.
	CodecProfile string `json:"codec_profile"`
	// Level содержит точное обозначение уровня видеокодека.
	Level string `json:"level"`
	// Tier содержит точное обозначение уровня производительности видеокодека, если оно задано.
	Tier string `json:"tier"`
	// PixelFormat содержит обозначение пиксельного формата видео.
	PixelFormat string `json:"pixel_format"`
	// BitDepth содержит глубину цветового компонента в битах.
	BitDepth uint8 `json:"bit_depth"`
	// Color содержит точные цветовые характеристики видео.
	Color MeasuredColorDTO `json:"color"`
}

// MeasuredAudioDTO передаёт полное измеренное описание аудио. Строки содержат распознанное
// обозначение, UNKNOWN для неизвестного непустого текста либо пустоту до измерения.
type MeasuredAudioDTO struct {
	// Codec содержит идентификатор аудиокодека.
	Codec string `json:"codec"`
	// CodecProfile содержит обозначение профиля аудиокодека.
	CodecProfile string `json:"codec_profile"`
	// SampleRateHz содержит частоту дискретизации аудио в герцах.
	SampleRateHz uint32 `json:"sample_rate_hz"`
	// Channels содержит число аудиоканалов.
	Channels uint16 `json:"channels"`
	// ChannelLayout содержит точное обозначение расположения каналов.
	ChannelLayout string `json:"channel_layout"`
}

// MeasuredFormatDTO объединяет полные описания видео- и аудиодорожек измеренного медиаформата.
type MeasuredFormatDTO struct {
	// Video содержит параметры видеодорожки.
	Video MeasuredVideoDTO `json:"video"`
	// Audio содержит параметры аудиодорожки.
	Audio MeasuredAudioDTO `json:"audio"`
}

// observedValues возвращает конечный каталог распознаваемых измерений поля field.
// Пустота и UNKNOWN добавляются схемой отдельно от распознаваемых значений.
func observedValues(field string) []string {
	switch field {
	case "video.codec":
		return []string{"av1", "h264"}
	case "audio.codec":
		return []string{"aac"}
	case "video.codec_profile":
		return []string{"baseline", "constrained-baseline", "main", "extended", "high", "high10", "high422", "high444"}
	case "video.level.h264":
		// values содержит все уровни, выдаваемые uint8-парсером, и прежние известные алиасы.
		values := []string{"1", "1b", "1.0b", "2", "3", "4", "5"}
		// number перечисляет все возможные исходные значения байта level_idc.
		for number := 0; number <= 255; number++ {
			values = append(values, strconv.Itoa(number/10)+"."+strconv.Itoa(number%10))
		}
		return values
	case "video.level.av1":
		// values содержит уровни AV1 и явное отсутствие ограничения производителя.
		values := []string{"unspecified"}
		// major и minor задают целую и дробную части уровня AV1.
		for major := 2; major <= 7; major++ {
			for minor := 0; minor <= 3; minor++ {
				values = append(values, strconv.Itoa(major)+"."+strconv.Itoa(minor))
			}
		}
		return values
	case "video.tier":
		return []string{"main", "high"}
	case "video.pixel_format":
		// values собирает базовые форматы и их известные варианты глубины компонента.
		values := []string{}
		// base обозначает семейство размещения пикселей; depth — глубину компонента в битах.
		for _, base := range []string{"gray", "yuv420p", "yuv422p", "yuv444p"} {
			values = append(values, base)
			for depth := 9; depth <= 16; depth++ {
				values = append(values, base+strconv.Itoa(depth)+"le")
			}
		}
		return values
	case "video.color.range":
		return []string{"limited", "full", "tv", "pc", "unspecified"}
	case "video.color.primaries":
		return []string{"bt709", "unspecified", "bt470bg", "smpte170m", "bt2020"}
	case "video.color.transfer":
		return []string{"bt709", "unspecified", "smpte170m", "linear", "iec61966-2-1", "bt2020-10", "bt2020-12", "smpte2084", "arib-std-b67"}
	case "video.color.matrix":
		return []string{"gbr", "bt709", "unspecified", "bt470bg", "smpte170m", "bt2020nc", "bt2020c"}
	case "audio.codec_profile":
		return []string{"LC"}
	case "audio.channel_layout":
		return []string{"mono", "stereo"}
	}
	return nil
}

// measuredEnums заполняет конечные схемы fields в schema с пустотой и UNKNOWN.
func measuredEnums(schema *huma.Schema, fields map[string]string) *huma.Schema {
	// name адресует JSON-свойство; field определяет каталог наблюдаемого значения.
	for name, field := range fields {
		stringEnum(schema, name, append([]string{"", "UNKNOWN"}, observedValues(field)...))
	}
	return schema
}

// TransformSchema задаёт конечные измеренные цветовые параметры в схеме schema.
func (MeasuredColorDTO) TransformSchema(_ huma.Registry, schema *huma.Schema) *huma.Schema {
	return measuredEnums(schema, map[string]string{"range": "video.color.range", "primaries": "video.color.primaries", "transfer": "video.color.transfer", "matrix": "video.color.matrix"})
}

// TransformSchema задаёт конечные измеренные видеопараметры в схеме schema, объединяя уровни кодеков.
func (MeasuredVideoDTO) TransformSchema(_ huma.Registry, schema *huma.Schema) *huma.Schema {
	measuredEnums(schema, map[string]string{"codec": "video.codec", "codec_profile": "video.codec_profile", "tier": "video.tier", "pixel_format": "video.pixel_format"})
	stringEnum(schema, "level", append(append([]string{"", "UNKNOWN"}, observedValues("video.level.h264")...), observedValues("video.level.av1")...))
	return schema
}

// TransformSchema задаёт конечные измеренные аудиопараметры в схеме schema.
func (MeasuredAudioDTO) TransformSchema(_ huma.Registry, schema *huma.Schema) *huma.Schema {
	return measuredEnums(schema, map[string]string{"codec": "audio.codec", "codec_profile": "audio.codec_profile", "channel_layout": "audio.channel_layout"})
}

// measuredFormatDTO проецирует строки исходного измерения format через каталоги известных производителей.
// Числа и дробь FPS переносятся без изменения; исходный доменный объект не меняется.
func measuredFormatDTO(format core.MediaFormat) MeasuredFormatDTO {
	// video и audio содержат исходные дорожки до публичной проекции строк.
	video, audio := format.Video, format.Audio
	// profiles, levels и tiers определяются известным кодеком видео.
	var profiles, levels, tiers []string
	switch video.Codec {
	case core.CodecH264:
		profiles = observedValues("video.codec_profile")
		levels = observedValues("video.level.h264")
	case core.CodecAV1:
		profiles = []string{"main"}
		levels = observedValues("video.level.av1")
		tiers = observedValues("video.tier")
	}
	return MeasuredFormatDTO{MeasuredVideoDTO{knownObserved(string(video.Codec), observedValues("video.codec")), video.Width, video.Height, FrameRateDTO{video.FrameRate.Numerator, video.FrameRate.Denominator}, knownObserved(video.CodecProfile, profiles), knownObserved(video.Level, levels), knownObserved(video.Tier, tiers), knownObserved(video.PixelFormat, observedValues("video.pixel_format")), video.BitDepth, MeasuredColorDTO{knownObserved(video.Color.Range, observedValues("video.color.range")), knownObserved(video.Color.Primaries, observedValues("video.color.primaries")), knownObserved(video.Color.Transfer, observedValues("video.color.transfer")), knownObserved(video.Color.Matrix, observedValues("video.color.matrix"))}}, MeasuredAudioDTO{knownObserved(string(audio.Codec), observedValues("audio.codec")), knownObserved(audio.CodecProfile, observedValues("audio.codec_profile")), audio.SampleRateHz, audio.Channels, knownObserved(audio.ChannelLayout, observedValues("audio.channel_layout"))}}
}
