package storage

import (
	"bytes"
	"encoding/json"
	"math"
	"reflect"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// TestLegacyDesiredReplacementPreservesNumbers проверяет точные разрешённые замены прежнего
// профиля, сохранение независимых чисел и исходных канонических байтов; t создаёт изолированные случаи.
func TestLegacyDesiredReplacementPreservesNumbers(t *testing.T) {
	// base содержит корректный прежний профиль с числами, которые нельзя ограничить матрицей v2.
	base := storageProfile()
	base.VideoEncoding.TargetBitrateBPS = math.MaxUint64 - 1
	base.VideoEncoding.MaxBitrateBPS = math.MaxUint64
	base.VideoEncoding.BufferBits = math.MaxUint64
	base.AudioEncoding.BitrateBPS = math.MaxUint64
	// encoded сохраняет известную корректную оболочку v1, err сообщает отказ кодирования.
	encoded, err := core.EncodeProfile(base)
	if err != nil {
		t.Fatal(err)
	}
	// scenario определяет исходные старые строки и точный независимый ожидаемый профиль.
	for _, scenario := range []struct {
		// name обозначает проверяемую пару либо прежнее открытое поле.
		name string
		// edit меняет старую wire-копию, допуская только структурно допустимые прежние значения.
		edit func(*migrationProfileMetadataV1)
		// expected задаёт отличие результата от base; nil сохраняет base целиком.
		expected func(*core.GroupProfile)
	}{
		{"unchanged", func(p *migrationProfileMetadataV1) { /* Сохраняет корректную исходную оболочку без изменений. */
		}, nil},
		{"codec_pair", func(p *migrationProfileMetadataV1) { /* Меняет ранее допустимую пару copy/H264. */
			p.Format.Video.Codec = core.CodecH264
		}, nil},
		{"unknown_video_profile", func(p *migrationProfileMetadataV1) { /* Записывает точные прежние произвольные байты профиля кодека. */
			p.Format.Video.CodecProfile = []byte{0xff, 0, 'x'}
		}, nil},
		{"unknown_level_tier", func(p *migrationProfileMetadataV1) { /* Записывает ранее открытые обозначения уровня и категории. */
			p.Format.Video.Level = []byte("outside")
			p.Format.Video.Tier = []byte("high")
		}, nil},
		{"pixel_depth", func(p *migrationProfileMetadataV1) { /* Моделирует ранее допустимый структурный формат 10-bit. */
			p.Format.Video.PixelFormat = []byte("yuv420p10le")
			p.Format.Video.BitDepth = 10
		}, nil},
		{"colors", func(p *migrationProfileMetadataV1) { /* Заменяет четыре прежних открытых обозначения цвета. */
			p.Format.Video.Range = []byte("outside")
			p.Format.Video.Primaries = []byte("outside")
			p.Format.Video.Transfer = []byte("outside")
			p.Format.Video.Matrix = []byte("outside")
		}, nil},
		{"audio_profile_layout", func(p *migrationProfileMetadataV1) { /* Меняет открытые строки AAC без изменения числа каналов. */
			p.Format.Audio.CodecProfile = []byte("outside")
			p.Format.Audio.ChannelLayout = []byte("outside")
		}, nil},
		{"positive_multichannel", func(p *migrationProfileMetadataV1) { /* Моделирует прежнее положительное число каналов за пределами mono/stereo. */
			p.Format.Audio.Channels = 6
			p.Format.Audio.ChannelLayout = []byte("5.1")
		}, nil},
		{"mono_companion", func(p *migrationProfileMetadataV1) { /* Сохраняет положительный единственный канал с неизвестной схемой. */
			p.Format.Audio.Channels = 1
			p.Format.Audio.ChannelLayout = []byte("outside")
		}, func(p *core.GroupProfile) { /* Сохраняет исходное число каналов и назначает прямую схему mono. */
			p.Format.Audio.Channels = 1
			p.Format.Audio.ChannelLayout = "mono"
		}},
		{"baseline_bframes", func(p *migrationProfileMetadataV1) { /* Сохраняет ранее независимые B-кадры при H264 baseline. */
			p.Mode = core.ModeTranscode
			p.Format.Video.Codec = core.CodecH264
			p.Format.Video.CodecProfile = []byte("baseline")
			p.VideoEncoding.MaxBFrames = 2
		}, func(p *core.GroupProfile) { /* Заменяет только профиль кодека; количество B-кадров сохраняется. */
			p.Mode = core.ModeTranscode
			p.Format.Video.Codec = core.CodecH264
			p.Format.Video.CodecProfile = "high"
			p.VideoEncoding.MaxBFrames = 2
		}},
		{"av1_legacy_bframes", func(p *migrationProfileMetadataV1) { /* Проверяет отсутствие переноса запрета B-кадров из SVTAV1 v2. */
			p.VideoEncoding.MaxBFrames = 17
		}, func(p *core.GroupProfile) { /* Независимое прежнее число B-кадров остаётся допустимым. */
			p.VideoEncoding.MaxBFrames = 17
		}},
		{"known_aliases", func(p *migrationProfileMetadataV1) { /* Сохраняет распознаваемые legacy-алиасы побайтно. */
			p.Mode = core.ModeTranscode
			p.Format.Video.Codec = core.CodecH264
			p.Format.Video.CodecProfile = []byte("high")
			p.Format.Video.Level = []byte("1.0b")
			p.Format.Video.Range = []byte("tv")
		}, func(p *core.GroupProfile) { /* Формирует независимый эталон тех же точных алиасов. */
			p.Mode = core.ModeTranscode
			p.Format.Video.Codec = core.CodecH264
			p.Format.Video.CodecProfile = "high"
			p.Format.Video.Level = "1.0b"
			p.Format.Video.Color.Range = "tv"
		}},
	} {
		// callback сравнивает полный результат каждой замены с ожидаемым профилем и прежней идентичностью.
		t.Run(scenario.name, func(t *testing.T) {
			// wire читает только известную старую структуру до внесения прежних допустимых строк.
			var wire migrationProfileMetadataV1
			// err сообщает об ошибке чтения исходного JSON и его типов.
			if err := json.Unmarshal(encoded, &wire); err != nil {
				t.Fatal(err)
			}
			scenario.edit(&wire)
			// input содержит согласованные исходные байты старого профиля, err сообщает отказ JSON.
			input, err := json.Marshal(wire)
			if err != nil {
				t.Fatal(err)
			}
			// result содержит новые байты, canonical — исходную идентичность, changed — факт замены, err — отказ проверки.
			result, canonical, changed, err := migrationProfile(input)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(canonical, input) {
				t.Fatal("Изменена исходная идентичность до проверки")
			}
			// actual принимает строго допустимый новый профиль.
			actual, err := core.DecodeProfile(result)
			if err != nil {
				t.Fatal(err)
			}
			// expected сохраняет все числа base и применяет только независимый ожидаемый результат.
			expected := base
			if scenario.expected != nil {
				scenario.expected(&expected)
			}
			if !reflect.DeepEqual(actual, expected) {
				t.Fatalf("Нарушена точная замена: %+v", actual)
			}
			if (scenario.name == "unchanged" || scenario.name == "known_aliases" || scenario.name == "av1_legacy_bframes") && (changed || !bytes.Equal(input, result)) {
				t.Fatal("Переписан корректный legacy payload")
			}
		})
	}
}

// TestLegacyDesiredReplacementRejectsEarlierCorruption запрещает расширять миграцию на старые
// ошибки контракта. t проверяет версии, обязательные поля, прежние codec/mode и независимые числа.
func TestLegacyDesiredReplacementRejectsEarlierCorruption(t *testing.T) {
	// encoded содержит корректный исходный профиль, err сообщает отказ кодирования.
	encoded, err := core.EncodeProfile(storageProfile())
	if err != nil {
		t.Fatal(err)
	}
	// scenario выбирает поле и недопустимую замену в прежнем JSON.
	for _, scenario := range []struct { /* before задаёт исходную лексему, after — прежнее повреждение. */
		before, after string
	}{
		{`"Version":1`, `"Version":99`}, {`"Mode":"copy"`, `"Mode":"other"`}, {`"Codec":"av1"`, `"Codec":"hevc"`},
		{`"Channels":2`, `"Channels":0`}, {`"BitDepth":8`, `"BitDepth":0`}, {`"GOPFrames":120`, `"GOPFrames":0`},
		{`"Width":2560`, `"Width":0`}, {`"SampleRateHz":48000`, `"SampleRateHz":0`},
	} {
		// input вносит ровно одну прежнюю ошибку, не меняя остальные поля.
		input := bytes.Replace(encoded, []byte(scenario.before), []byte(scenario.after), 1)
		// err должен сохранить отказ прежнего повреждения, а не результат автоматической замены.
		if _, _, _, err := migrationProfile(input); err == nil {
			t.Fatalf("Исправлено ранее запрещённое повреждение %s", scenario.after)
		}
	}
}
