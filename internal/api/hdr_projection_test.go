package api

import (
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// TestMeasuredHEVCVP9CatalogsKeepAssignedLevels проверяет распознанные уровни отдельно от
// неизвестного текста и не подставляет желаемый auto либо уровень для отсутствующего измерения.
func TestMeasuredHEVCVP9CatalogsKeepAssignedLevels(t *testing.T) {
	// scenario задаёт известный codec/profile/level/tier, включая неподдержанные для настройки VP9-профили.
	for _, scenario := range []struct {
		// codec выбирает каталог; profile, level и tier содержат точные наблюдаемые обозначения.
		codec                core.Codec
		profile, level, tier string
	}{
		{core.CodecHEVC, "main", "3.1", "main"},
		{core.CodecHEVC, "main10", "8.5", "high"},
		{core.CodecVP9, "profile0", "1.0", ""},
		{core.CodecVP9, "profile2", "6.2", ""},
		{core.CodecVP9, "profile1", "unspecified", ""},
		{core.CodecVP9, "profile3", "", ""},
	} {
		// before содержит независимое сырое наблюдение; result не меняет его при публичной проекции.
		before := core.MediaFormat{Video: core.VideoFormat{Codec: scenario.codec, CodecProfile: scenario.profile, Level: scenario.level, Tier: scenario.tier}}
		result := measuredFormatDTO(before)
		if result.Video.Codec != string(scenario.codec) || result.Video.CodecProfile != scenario.profile || result.Video.Level != scenario.level || result.Video.Tier != scenario.tier {
			t.Fatal("Изменено известное наблюдение", scenario, result)
		}
		before.Video.Level = "2.2"
		result = measuredFormatDTO(before)
		if result.Video.Level != "UNKNOWN" || before.Video.Level != "2.2" {
			t.Fatal("Неназначенный уровень стал известным либо изменил исходное описание", scenario, result)
		}
		// metrics переносит тот же новый codec в общий формат REST/SSE-счётчиков.
		metrics := mediaDTO(core.MediaMetrics{Format: core.ObservedFormat{VideoCodec: scenario.codec}})
		if metrics.Format.VideoCodec != string(scenario.codec) {
			t.Fatal("REST/SSE потерял видеокодек", metrics)
		}
	}
}
