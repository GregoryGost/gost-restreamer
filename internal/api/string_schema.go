package api

import (
	"slices"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/danielgtaylor/huma/v2"
)

// stringEnum задаёт конечный набор values для свойства name схемы schema.
// Отдельный экземпляр свойства исключает изменение схем других DTO с вложенными полями.
func stringEnum(schema *huma.Schema, name string, values []string) {
	// property копирует описание поля перед заменой его перечисления.
	property := *schema.Properties[name]
	property.Enum = nil
	// value содержит очередное допустимое строковое значение.
	for _, value := range values {
		if !slices.Contains(property.Enum, any(value)) {
			property.Enum = append(property.Enum, value)
		}
	}
	schema.Properties[name] = &property
}

// desiredEnums переносит текущий каталог fields в свойства schema.
// Применимость значений проверяется по выбранным кодировщикам в домене.
// Ключ fields обозначает JSON-свойство, значение — поле строгого желаемого профиля.
func desiredEnums(schema *huma.Schema, fields map[string]string) *huma.Schema {
	// name адресует JSON-свойство; field выбирает соответствующий доменный каталог.
	for name, field := range fields {
		stringEnum(schema, name, core.DesiredStringValues(field))
	}
	return schema
}

// TransformSchema описывает конечные цветовые настройки схемы schema; registry не требуется.
func (ColorDTO) TransformSchema(_ huma.Registry, schema *huma.Schema) *huma.Schema {
	return desiredEnums(schema, map[string]string{"range": "video.color.range", "primaries": "video.color.primaries", "transfer": "video.color.transfer", "matrix": "video.color.matrix"})
}

// TransformSchema публикует желаемые видеопараметры schema и объединённые уровни поддержанных кодеков.
func (VideoFormatDTO) TransformSchema(_ huma.Registry, schema *huma.Schema) *huma.Schema {
	desiredEnums(schema, map[string]string{"codec": "video.codec", "codec_profile": "video.codec_profile", "tier": "video.tier", "pixel_format": "video.pixel_format"})
	stringEnum(schema, "level", append(core.DesiredStringValues("video.level.h264"), core.DesiredStringValues("video.level.av1")...))
	return schema
}

// TransformSchema публикует конечные желаемые аудиопараметры схемы schema.
func (AudioFormatDTO) TransformSchema(_ huma.Registry, schema *huma.Schema) *huma.Schema {
	return desiredEnums(schema, map[string]string{"codec": "audio.codec", "codec_profile": "audio.codec_profile", "channel_layout": "audio.channel_layout"})
}

// TransformSchema ограничивает причину успешной проверки здоровья в схеме schema пустой строкой.
func (HealthDTO) TransformSchema(_ huma.Registry, schema *huma.Schema) *huma.Schema {
	stringEnum(schema, "reason", []string{""})
	return schema
}

// TransformSchema уточняет схему schema наблюдаемой группы до её собственного набора состояний.
func (RuntimeGroupDTO) TransformSchema(_ huma.Registry, schema *huma.Schema) *huma.Schema {
	stringEnum(schema, "state", []string{"UNKNOWN", "STARTING", "READY", "RETRYING", "FAILED", "STOPPED"})
	return schema
}

// TransformSchema уточняет схему schema наблюдаемого выхода до состояний отправителя.
func (RuntimeOutputDTO) TransformSchema(_ huma.Registry, schema *huma.Schema) *huma.Schema {
	stringEnum(schema, "state", []string{"UNKNOWN", "CONNECTING", "PUBLISHING", "RETRYING", "FAILED", "STOPPED"})
	return schema
}

// TransformSchema уточняет схему schema работника FFmpeg до состояний медиапроизводителя.
func (RuntimeFFmpegDTO) TransformSchema(_ huma.Registry, schema *huma.Schema) *huma.Schema {
	stringEnum(schema, "state", []string{"UNKNOWN", "STARTING", "READY", "RETRYING", "FAILED", "STOPPED"})
	return schema
}

// knownObserved сохраняет value из конечного каталога values; неизвестный непустой текст
// становится UNKNOWN. Пустота означает отсутствие измерения и сохраняется отдельно.
func knownObserved(value string, values []string) string {
	if value == "" || slices.Contains(values, value) {
		return value
	}
	return "UNKNOWN"
}

// EventObservationDTO описывает наблюдение события: неприменимое состояние остаётся пустым.
type EventObservationDTO ObservationDTO

// TransformSchema задаёт в схеме schema объединённый набор состояний событий разных владельцев.
func (EventObservationDTO) TransformSchema(_ huma.Registry, schema *huma.Schema) *huma.Schema {
	stringEnum(schema, "state", []string{"", "UNKNOWN", "STARTING", "READY", "RETRYING", "FAILED", "STOPPED", "CONNECTING", "PUBLISHING"})
	return schema
}

// TransformSchema публикует конечные сохранённые настройки NVENC в schema.
func (NVENCDTO) TransformSchema(_ huma.Registry, schema *huma.Schema) *huma.Schema {
	return desiredEnums(schema, map[string]string{"preset": "encoding.nvenc.preset", "tuning": "encoding.nvenc.tuning", "multipass": "encoding.nvenc.multipass"})
}

// TransformSchema публикует конечный выбор tuning SVT-AV1 в schema.
func (SVTAV1DTO) TransformSchema(_ huma.Registry, schema *huma.Schema) *huma.Schema {
	return desiredEnums(schema, map[string]string{"tuning": "encoding.svt_av1.tuning"})
}
