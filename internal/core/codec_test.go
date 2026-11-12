package core_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// Проверяет сохранение точных строковых байтов, полного uint64, исходной дроби FPS и вложенного снимка
// после кодирования и чтения.
func TestProfileMetadataPreservesExactAcceptedValues(t *testing.T) {
	// p содержит отдельный полный профиль текущего сценария проверки.
	p := validProfile()
	p.Format.Video.FrameRate = core.FrameRate{Numerator: 60000, Denominator: 2002}
	p.VideoEncoding.TargetBitrateBPS = math.MaxUint64 - 1
	p.VideoEncoding.MaxBitrateBPS = math.MaxUint64
	p.VideoEncoding.BufferBits = math.MaxUint64
	p.AudioEncoding.BitrateBPS = math.MaxUint64
	// raw содержит пробелы, некорректный UTF-8 и нулевой байт для проверки точного хранения строк.
	raw := string([]byte{' ', 0xff, 0, 'x', ' '})
	p.Format.Video.CodecProfile, p.Format.Video.Level, p.Format.Video.Tier = raw, raw, raw
	p.Format.Video.PixelFormat = raw
	p.Format.Video.Color = core.ColorInfo{Range: raw, Primaries: raw, Transfer: raw, Matrix: raw}
	p.Format.Audio.CodecProfile, p.Format.Audio.ChannelLayout = raw, raw
	// err сообщает об ошибке проверки допустимости профиля.
	if err := p.Validate(); err != nil {
		t.Fatalf("исходный принятый профиль недопустим: %v", err)
	}
	// data содержит байты JSON-профиля для чтения и сравнения. err сообщает об ошибке кодирования
	// проверенного профиля.
	data, err := core.EncodeProfile(p)
	if err != nil {
		t.Fatal(err)
	}
	// got содержит профиль, восстановленный из проверяемого представления.
	got, err := core.DecodeProfile(data)
	if err != nil || got != p {
		t.Fatalf("профиль после хранения изменён: ошибка %v, получен %#v", err, got)
	}
	// formatData содержит сохранённые байты медиаформата.
	formatData, err := core.EncodeMediaFormat(p.Format)
	if err != nil {
		t.Fatal(err)
	}
	// format содержит восстановленный медиаформат для точного сравнения.
	format, err := core.DecodeMediaFormat(formatData)
	if err != nil || format != p.Format {
		t.Fatalf("формат после хранения изменён: ошибка %v, получен %#v", err, format)
	}
	// now задаёт общее время записей исторического снимка.
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	// record создаёт записи снимка с максимальной ревизией и общим фиксированным временем.
	record := func(id core.ID) core.Record {
		// Создаёт запись с заданным ID, максимальной Revision и общим временем для проверки точного сохранения
		// снимка.

		return core.Record{ID: id, Revision: math.MaxUint64, CreatedAt: now, UpdatedAt: now}
	}
	// snapshot задаёт самостоятельный снимок запуска с проверяемым вложенным профилем.
	snapshot := core.SessionSnapshot{
		Stream: core.Stream{Record: record("stream"), Name: "Поток", PublicationKey: "synthetic-key"},
		Groups: []core.GroupSnapshot{{
			Group:   core.TranscodeGroup{Record: record("group"), StreamID: "stream", ProfileID: "profile", Name: "Группа"},
			Profile: core.TranscodeProfile{Record: record("profile"), Name: "Профиль", Profile: p},
		}},
	}
	// snapshotData содержит сохранённые байты начального снимка запуска.
	snapshotData, err := core.EncodeSessionSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	// restored содержит восстановленный снимок для проверки точного сохранения.
	restored, err := core.DecodeSessionSnapshot(snapshotData)
	if err != nil || !reflect.DeepEqual(snapshot, restored) {
		t.Fatalf("снимок изменён после хранения: ошибка %v", err)
	}
}

// Проверяет одинаковую идентичность эквивалентных FPS и различие при изменении любого прежнего поля,
// источника или генератора.
func TestProfileIdentityUsesRationalFPSAndEveryProfileField(t *testing.T) {
	// p содержит отдельный полный профиль текущего сценария проверки.
	p := validProfile()
	// key содержит вычисленный ключ полного профиля. content содержит канонические байты полного
	// профиля. err сообщает об ошибке вычисления канонической идентичности профиля.
	key, content, err := core.ProfileIdentity(p)
	if err != nil || len(key) != 64 || len(content) == 0 {
		t.Fatalf("идентичность профиля: %q, %v", key, err)
	}
	// equivalent содержит копию профиля с математически равной несокращённой частотой кадров.
	equivalent := p
	equivalent.Format.Video.FrameRate = core.FrameRate{Numerator: 120, Denominator: 2}
	// equivalentKey содержит ключ профиля с эквивалентной частотой кадров. equivalentContent
	// содержит канонические байты профиля с эквивалентной частотой кадров.
	equivalentKey, equivalentContent, err := core.ProfileIdentity(equivalent)
	if err != nil || equivalentKey != key || !bytes.Equal(content, equivalentContent) {
		t.Fatalf("равные дроби получили разные идентичности: %v", err)
	}
	if equivalent.Format.Video.FrameRate.Denominator != 2 {
		t.Fatal("канонизация изменила исходный профиль")
	}
	// changes перечисляет отдельные изменения полей для проверки их участия в идентичности.
	changes := []struct {
		// name задаёт имя текущего сценария проверки поля профиля.
		name string
		// apply вносит выбранное изменение поля в копию исходного профиля.
		apply func(*core.GroupProfile)
	}{
		{"mode", func(p *core.GroupProfile) {
			// Меняет режим на ModeTranscode для проверки его участия в идентичности профиля.
			p.Mode = core.ModeTranscode
		}},
		{"video_codec", func(p *core.GroupProfile) {
			// Меняет видеокодек на CodecH264 для проверки его участия в идентичности профиля.
			p.Format.Video.Codec = core.CodecH264
		}},
		{"width", func(p *core.GroupProfile) {
			// Увеличивает ширину кадра для проверки различимости профилей.
			p.Format.Video.Width++
		}},
		{"height", func(p *core.GroupProfile) {
			// Увеличивает высоту кадра для проверки различимости профилей.
			p.Format.Video.Height++
		}},
		{"fps", func(p *core.GroupProfile) {
			// Увеличивает числитель FPS для проверки различимости профилей.
			p.Format.Video.FrameRate.Numerator++
		}},
		{"video_profile", func(p *core.GroupProfile) {
			// Задаёт CodecProfile с пробелами, которые должны участвовать в идентичности без обрезания.
			p.Format.Video.CodecProfile = " main "
		}},
		{"level", func(p *core.GroupProfile) {
			// Меняет Level на 4.1 для проверки различимости профилей.
			p.Format.Video.Level = "4.1"
		}},
		{"tier", func(p *core.GroupProfile) {
			// Меняет Tier на main для проверки различимости профилей.
			p.Format.Video.Tier = "main"
		}},
		{"pixel_format", func(p *core.GroupProfile) {
			// Меняет PixelFormat на yuv420p10le для проверки различимости профилей.
			p.Format.Video.PixelFormat = "yuv420p10le"
		}},
		{"bit_depth", func(p *core.GroupProfile) {
			// Увеличивает BitDepth для проверки различимости профилей.
			p.Format.Video.BitDepth++
		}},
		{"range", func(p *core.GroupProfile) {
			// Меняет диапазон цвета на full для проверки различимости профилей.
			p.Format.Video.Color.Range = "full"
		}},
		{"primaries", func(p *core.GroupProfile) {
			// Меняет цветовые первичные координаты на bt2020 для проверки различимости профилей.
			p.Format.Video.Color.Primaries = "bt2020"
		}},
		{"transfer", func(p *core.GroupProfile) {
			// Меняет функцию переноса на smpte2084 для проверки различимости профилей.
			p.Format.Video.Color.Transfer = "smpte2084"
		}},
		{"matrix", func(p *core.GroupProfile) {
			// Меняет цветовую матрицу на bt2020nc для проверки различимости профилей.
			p.Format.Video.Color.Matrix = "bt2020nc"
		}},
		{"audio_profile", func(p *core.GroupProfile) {
			// Меняет аудиопрофиль на lc для проверки различимости профилей.
			p.Format.Audio.CodecProfile = "lc"
		}},
		{"sample_rate", func(p *core.GroupProfile) {
			// Увеличивает частоту дискретизации для проверки различимости профилей.
			p.Format.Audio.SampleRateHz++
		}},
		{"channels", func(p *core.GroupProfile) {
			// Увеличивает число аудиоканалов для проверки различимости профилей.
			p.Format.Audio.Channels++
		}},
		{"channel_layout", func(p *core.GroupProfile) {
			// Меняет раскладку каналов на mono для проверки различимости профилей.
			p.Format.Audio.ChannelLayout = "mono"
		}},
		{"target", func(p *core.GroupProfile) {
			// Увеличивает целевой видеобитрейт для проверки различимости профилей.
			p.VideoEncoding.TargetBitrateBPS++
		}},
		{"maximum", func(p *core.GroupProfile) {
			// Увеличивает максимальный видеобитрейт для проверки различимости профилей.
			p.VideoEncoding.MaxBitrateBPS++
		}},
		{"buffer", func(p *core.GroupProfile) {
			// Увеличивает размер видеобуфера для проверки различимости профилей.
			p.VideoEncoding.BufferBits++
		}},
		{"gop", func(p *core.GroupProfile) {
			// Увеличивает длину GOP для проверки различимости профилей.
			p.VideoEncoding.GOPFrames++
		}},
		{"b_frames", func(p *core.GroupProfile) {
			// Увеличивает допустимое число B-кадров для проверки различимости профилей.
			p.VideoEncoding.MaxBFrames++
		}},
		{"closed_gop", func(p *core.GroupProfile) {
			// Отключает закрытый GOP для проверки различимости профилей.
			p.VideoEncoding.ClosedGOP = false
		}},
		{"audio_bitrate", func(p *core.GroupProfile) {
			// Увеличивает аудиобитрейт для проверки различимости профилей.
			p.AudioEncoding.BitrateBPS++
		}},
	}
	// change вносит выбранное изменение параметра в копию исходного профиля.
	for _, change := range changes {
		t.Run(change.name, func(t *testing.T) {
			// Проверяет, что выбранное изменение поля меняет и ключ, и канонические байты, сохраняя исходный
			// профиль.

			// other содержит копию профиля с выбранным отличающимся параметром.
			other := p
			change.apply(&other)
			// otherKey содержит ключ изменённого полного профиля. otherContent содержит
			// канонические байты изменённого полного профиля. err сообщает об ошибке
			// вычисления канонической идентичности профиля.
			otherKey, otherContent, err := core.ProfileIdentity(other)
			if err != nil || otherKey == key || bytes.Equal(otherContent, content) {
				t.Fatalf("изменение поля не различено: ключ %q, ошибка %v", otherKey, err)
			}
		})
	}
	// variant содержит идентичность варианта исходного профиля и фиксированного генератора.
	variant, err := core.VariantIdentity("source", p, "generator")
	if err != nil {
		t.Fatal(err)
	}
	// input задаёт изменённый источник или генератор для сравнения ключа варианта.
	for _, input := range []struct { /* source задаёт хеш исходника для проверки его участия в идентичности варианта. generator задаёт версию генератора для проверки её участия в идентичности варианта. */
		source, generator string
	}{{"other-source", "generator"}, {"source", "other-generator"}, {"sourcegenerator", ""}, {"source", "generator "}} {
		// other содержит ключ варианта после изменения источника или генератора. err сообщает об
		// ошибке вычисления идентичности варианта.
		other, err := core.VariantIdentity(input.source, p, input.generator)
		if err != nil || other == variant {
			t.Fatalf("источник и генератор не различены: %v", err)
		}
	}
}

// Проверяет типизированный отказ чтения повреждённых, неизвестных и недопустимых метаданных без
// частичного изменения получателя профиля.
func TestProfileMetadataRejectsUnknownCorruptAndInvalidValues(t *testing.T) {
	// valid содержит корректные байты профиля до внесения повреждений. err сообщает об ошибке
	// кодирования проверенного профиля.
	valid, err := core.EncodeProfile(validProfile())
	if err != nil {
		t.Fatal(err)
	}
	// fields предоставляет поля сохранённого JSON-профиля для выбранного изменения.
	var fields map[string]json.RawMessage
	// err сообщает об ошибке разбора JSON проверяемого представления.
	if err := json.Unmarshal(valid, &fields); err != nil {
		t.Fatal(err)
	}
	fields["Version"] = json.RawMessage("999")
	// unknown содержит JSON-профиль с неподдерживаемой версией.
	unknown, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	// inputs перечисляет повреждённые или недопустимые входные представления.
	inputs := [][]byte{nil, []byte("null"), []byte("{}"), []byte("{"), append(append([]byte{}, valid...), []byte(" {}")...), unknown, bytes.Replace(valid, []byte(`"Numerator":60`), []byte(`"Numerator":0`), 1), bytes.Replace(valid, []byte(`"BitrateBPS":192000`), []byte(`"BitrateBPS":18446744073709551616`), 1)}
	// input содержит очередной проверяемый вход выбранного сценария.
	for _, input := range inputs {
		// before сохраняет исходный профиль для проверки отсутствия изменения.
		before := validProfile()
		// receiver содержит исходное значение получателя для проверки отсутствия частичного
		// изменения.
		receiver := before
		// err сообщает об ошибке разбора JSON проверяемого представления.
		err := json.Unmarshal(input, &receiver)
		if err == nil {
			t.Fatalf("некорректные метаданные приняты: %s", input)
		}
		// Синтаксические ошибки JSON может вернуть внешний json.Unmarshal.
		// decodeErr содержит типизированный отказ доменного декодера профиля.
		_, decodeErr := core.DecodeProfile(input)
		// metadata принимает ошибку метаданных для проверки категории и типа.
		var metadata *core.MetadataError
		if !errors.Is(decodeErr, core.ErrInvalidMetadata) || !errors.As(decodeErr, &metadata) {
			t.Fatalf("DecodeProfile потерял тип ошибки для %s: %v", input, decodeErr)
		}
		if receiver != before {
			t.Fatalf("ошибка частично изменила приёмник для %s", input)
		}
	}
	// decode выбирает очередной декодер версионированных доменных метаданных.
	for _, decode := range []func([]byte) error{
		func(b []byte) error {
			// Возвращает ошибку DecodeMediaFormat для общей проверки отклонения неизвестной версии метаданных.
			// e сообщает об ошибке чтения и проверки медиаформата.
			_, e := core.DecodeMediaFormat(b)
			return e
		},
		func(b []byte) error {
			// Возвращает ошибку DecodeSessionSnapshot для общей проверки отклонения неизвестной версии метаданных.
			// e сообщает об ошибке чтения и проверки снимка запуска.
			_, e := core.DecodeSessionSnapshot(b)
			return e
		},
		func(b []byte) error {
			// Возвращает ошибку DecodeOutputSnapshot для общей проверки отклонения неизвестной версии метаданных.
			// e сообщает об ошибке чтения и проверки снимка выхода.
			_, e := core.DecodeOutputSnapshot(b)
			return e
		},
		func(b []byte) error {
			// Декодирует Record для общей проверки отклонения неизвестной версии метаданных.
			// v принимает доменную Record при проверке неизвестной версии.
			var v core.Record
			return core.DecodeMetadata(b, &v)
		},
	} {
		// err сообщает об ошибке отклонения неизвестной версии выбранным декодером.
		if err := decode([]byte(`{"Version":999}`)); !errors.Is(err, core.ErrInvalidMetadata) {
			t.Fatalf("неизвестная версия метаданных принята: %v", err)
		}
	}
}

// Проверяет представление v3 с явным режимом LIVE-аудио, допустимый null Encoding и отказ для неполных
// полей без изменения получателя.
func TestExplicitLiveAudioProfileWireAndStrictReplacement(t *testing.T) {
	// withEncoding выбирает наличие полного блока Encoding в профиле с явным LIVE-аудиорежимом.
	for _, withEncoding := range []bool{false, true} {
		// legacy задаёт исходный профиль без явного LIVE-аудиоселектора.
		legacy := encodedProfile(core.VideoEncoderNVENC, core.RateControlVBR)
		if !withEncoding {
			legacy.Encoding = core.EncodingSettings{}
		}
		// mode задаёт очередной явный LIVE-аудиорежим.
		for _, mode := range []core.LiveAudioMode{core.LiveAudioCopy, core.LiveAudioTranscode} {
			// p содержит отдельный полный профиль текущего сценария проверки.
			p := legacy
			p.LiveAudioMode = mode
			// before сохраняет исходный профиль для проверки отсутствия изменения.
			before := p
			// raw содержит сохраняемое представление проверяемого профиля. err сообщает об
			// ошибке кодирования проверенного профиля.
			raw, err := core.EncodeProfile(p)
			if err != nil {
				t.Fatal(err)
			}
			// got содержит профиль, восстановленный из проверяемого представления.
			got, err := core.DecodeProfile(raw)
			if err != nil || got != p || p != before || got.EffectiveLiveAudioMode() != mode {
				t.Fatalf("v3 изменил явный профиль: %v", err)
			}
			// fields предоставляет поля сохранённого JSON-профиля для выбранного изменения.
			var fields map[string]json.RawMessage
			// err сообщает об ошибке разбора JSON проверяемого представления.
			if err := json.Unmarshal(raw, &fields); err != nil {
				t.Fatal(err)
			}
			if string(fields["Version"]) != "3" || (string(fields["Encoding"]) == "null") == withEncoding || core.MetadataVersion != 1 || core.EncodingVersion != 2 {
				t.Fatalf("Версии или null Encoding изменены: %s", raw)
			}
			// key содержит вычисленный ключ полного профиля.
			key, _, err := core.ProfileIdentity(p)
			if err != nil {
				t.Fatal(err)
			}
			// oldKey содержит ключ исходного профиля без LIVE-аудиоселектора.
			oldKey, _, err := core.ProfileIdentity(legacy)
			if err != nil || key == oldKey || p.Equal(legacy) || legacy.Equal(p) {
				t.Fatal("Явный селектор не входит в идентичность", err)
			}
			// field выбирает обязательное поле сохраняемого представления для проверки.
			for _, field := range []string{"LiveAudioMode", "Encoding", "VideoEncoding", "AudioEncoding", "Format"} {
				// value задаёт отсутствие поля либо явный null для проверки строгого
				// чтения.
				for _, value := range []json.RawMessage{nil, json.RawMessage("null")} {
					// copyFields содержит отдельную копию полей профиля для внесения
					// повреждения.
					copyFields := make(map[string]json.RawMessage, len(fields))
					maps.Copy(copyFields, fields)
					if value == nil {
						delete(copyFields, field)
					} else {
						copyFields[field] = value
					}
					// corrupt содержит профиль с выбранным удалённым или обнулённым
					// полем. err сообщает об ошибке кодирования проверяемого
					// JSON-представления.
					corrupt, err := json.Marshal(copyFields)
					if err != nil {
						t.Fatal(err)
					}
					// receiver содержит исходное значение получателя для проверки
					// отсутствия частичного изменения.
					receiver := p
					err = receiver.UnmarshalJSON(corrupt)
					if field == "Encoding" && value != nil {
						if err != nil || receiver.Encoding != (core.EncodingSettings{}) {
							t.Fatal("Явный null Encoding не прочитан", err)
						}
					} else if !errors.Is(err, core.ErrInvalidMetadata) || receiver != p {
						t.Fatalf("Некорректное поле %s изменило профиль: %v", field, err)
					}
				}
			}
		}
		// before сохраняет исходный профиль для проверки отсутствия изменения.
		before := legacy
		// err сообщает об ошибке проверки допустимости профиля.
		if err := legacy.Validate(); err != nil || legacy.EffectiveLiveAudioMode() != core.LiveAudioTranscode || legacy != before {
			t.Fatal("Чтение прежнего режима изменило профиль", err)
		}
	}
}
