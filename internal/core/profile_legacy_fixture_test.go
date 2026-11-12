package core_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"maps"
	"math"
	"os"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// frozenProfileCase хранит независимые эталонные байты и идентичности прежнего профиля.
type frozenProfileCase struct {
	// Name различает закреплённые эталоны прежних профилей.
	Name string
	// Payload содержит эталонные сохраняемые байты прежнего профиля. Canonical содержит эталонные
	// канонические байты прежнего профиля.
	Payload, Canonical []byte
	// ProfileKey содержит эталонный ключ полного профиля. VariantKey содержит эталонный ключ
	// варианта фиксированного источника и генератора.
	ProfileKey, VariantKey string
}

// Читает три закреплённых эталона прежних профилей из testdata и завершает тест при ошибке или
// неполном наборе.
func frozenProfileCases(t *testing.T) []frozenProfileCase {
	t.Helper()
	// b содержит байты закреплённых эталонов прежних профилей. err сообщает об ошибке чтения
	// закреплённого эталона с диска.
	b, err := os.ReadFile("testdata/encoding_legacy_v1/profiles.json")
	if err != nil {
		t.Fatal(err)
	}
	// cases принимает три независимых эталона прежнего представления профилей.
	var cases []frozenProfileCase
	if err = json.Unmarshal(b, &cases); err != nil {
		t.Fatal(err)
	}
	if len(cases) != 3 {
		t.Fatalf("Нужно три независимых эталона, получено %d", len(cases))
	}
	return cases
}

// Проверяет точные прежние байты, ключи профилей и вариантов по закреплённым эталонам, включая
// недопустимые для новых кодировщиков прежние значения.
func TestFrozenLegacyProfilesPreserveBytesIdentityAndAcceptance(t *testing.T) {
	// fixture содержит очередной закреплённый эталон прежнего профиля.
	for _, fixture := range frozenProfileCases(t) {
		t.Run(fixture.Name, func(t *testing.T) {
			// Сверяет выбранный прежний профиль с независимым эталоном байтов, ключей и канонического содержания,
			// включая несокращённый FPS и точные строковые байты.

			// p содержит отдельный полный профиль текущего сценария проверки. err сообщает
			// об ошибке чтения и проверки сохранённого профиля.
			p, err := core.DecodeProfile(fixture.Payload)
			if err != nil {
				t.Fatal(err)
			}
			if p.Encoding != (core.EncodingSettings{}) {
				t.Fatalf("Старому профилю добавлены настройки: %+v", p.Encoding)
			}
			if err = p.Validate(); err != nil {
				t.Fatal(err)
			}
			if p.Format.Video.FrameRate != (core.FrameRate{Numerator: 60000, Denominator: 2002}) {
				t.Fatal("Сохранённая дробь FPS изменилась")
			}
			// got содержит повторно закодированные байты для точного сравнения с эталоном.
			got, err := core.EncodeProfile(p)
			if err != nil || !bytes.Equal(got, fixture.Payload) {
				t.Fatalf("Изменились байты v1: %s, %v", got, err)
			}
			// key содержит вычисленный ключ полного профиля. canonical содержит канонические
			// байты полного профиля.
			key, canonical, err := core.ProfileIdentity(p)
			if err != nil || key != fixture.ProfileKey || !bytes.Equal(canonical, fixture.Canonical) {
				t.Fatalf("Изменилась прежняя идентичность: %q, %v", key, err)
			}
			// variant содержит идентичность варианта исходного профиля и фиксированного
			// генератора.
			variant, err := core.VariantIdentity("frozen-source", p, "generator-v1")
			if err != nil || variant != fixture.VariantKey {
				t.Fatalf("Изменилась идентичность варианта: %q, %v", variant, err)
			}
			// reduced содержит копию профиля с сокращённой эквивалентной частотой кадров.
			reduced := p
			reduced.Format.Video.FrameRate = core.FrameRate{Numerator: 30000, Denominator: 1001}
			if !p.Equal(reduced) {
				t.Fatal("Математически равные FPS перестали совпадать")
			}
			// reducedKey содержит ключ прежнего профиля с сокращённой эквивалентной
			// частотой.
			reducedKey, _, err := core.ProfileIdentity(reduced)
			if err != nil || reducedKey != fixture.ProfileKey {
				t.Fatal("Канонизация FPS изменилась")
			}
			if fixture.Name == "exact-bytes" {
				// raw содержит эталонную строку с пробелами, некорректным UTF-8 и
				// нулевым байтом.
				raw := string([]byte{' ', 0xff, 0, 'x', ' '})
				// s содержит очередной строковый параметр прежнего профиля для точной
				// проверки байтов.
				for _, s := range []string{p.Format.Video.CodecProfile, p.Format.Video.Level, p.Format.Video.Tier, p.Format.Video.PixelFormat, p.Format.Video.Color.Range, p.Format.Video.Color.Primaries, p.Format.Video.Color.Transfer, p.Format.Video.Color.Matrix, p.Format.Audio.CodecProfile, p.Format.Audio.ChannelLayout} {
					if s != raw {
						t.Fatalf("Исходные байты строки потеряны: %x", s)
					}
				}
				if p.VideoEncoding.TargetBitrateBPS != math.MaxUint64-1 || p.VideoEncoding.MaxBitrateBPS != math.MaxUint64 || p.VideoEncoding.BufferBits != math.MaxUint64 || p.AudioEncoding.BitrateBPS != math.MaxUint64 {
					t.Fatal("Полный диапазон uint64 потерян")
				}
				if p.Format.Video.Width != 65535 || p.Format.Video.Height != 1 {
					t.Fatal("Прежние допустимые размеры изменены")
				}
			}
		})
	}
}

// Строит проверенный профиль с явным Encoding из закреплённого эталона H.264 либо AV1, согласуя
// ограничения VBR.
func explicitFixtureProfile(t *testing.T, encoder string) core.GroupProfile {
	t.Helper()
	// f содержит очередной закреплённый эталон для выбора исходного кодека.
	for _, f := range frozenProfileCases(t) {
		if f.Name == "h264" && encoder == "h264_nvenc" || f.Name == "av1" && encoder == "libsvtav1" {
			// p содержит отдельный полный профиль текущего сценария проверки. err сообщает
			// об ошибке чтения и проверки сохранённого профиля.
			p, err := core.DecodeProfile(f.Payload)
			if err != nil {
				t.Fatal(err)
			}
			p.Encoding = core.DefaultEncoding(p.Mode, encoder, "vbr")
			if encoder == "libsvtav1" {
				p.VideoEncoding.MaxBitrateBPS = 0
				p.VideoEncoding.BufferBits = 0
			} else {
				p.VideoEncoding.MaxBitrateBPS = p.VideoEncoding.TargetBitrateBPS
				p.Encoding.NVENC.EnableSpatialAQ = true
			}
			if err = p.Validate(); err != nil {
				t.Fatal(err)
			}
			return p
		}
	}
	t.Fatal("Не найден исходный профиль")
	return core.GroupProfile{}
}

// Проверяет по эталонным профилям влияние явных настроек кодирования на равенство, оба ключа и точное
// сохранение значений.
func TestExplicitEncodingFieldsChangeFullIdentity(t *testing.T) {
	// encoder выбирает проверяемый видеокодировщик.
	for _, encoder := range []string{"h264_nvenc", "libsvtav1"} {
		t.Run(encoder, func(t *testing.T) {
			// Проверяет для выбранного кодировщика участие применимых явных настроек в равенстве, идентичностях и
			// точном сохранении профиля.

			// p содержит отдельный полный профиль текущего сценария проверки.
			p := explicitFixtureProfile(t, encoder)
			// changes перечисляет отдельные изменения полей для проверки их участия в
			// идентичности.
			changes := []struct {
				// name задаёт имя текущего сценария проверки поля профиля.
				name string
				// apply вносит выбранное изменение поля в копию исходного профиля.
				apply func(*core.GroupProfile)
			}{
				{"audio_custom", func(p *core.GroupProfile) {
					// Задаёт AudioCustom aac_coder=fast для проверки участия аудиопараметров в идентичности.
					p.Encoding.AudioCustom = "aac_coder=fast"
				}},
			}
			if encoder == "h264_nvenc" {
				changes = append(changes, []struct {
					// name задаёт имя текущего сценария проверки поля профиля.
					name string
					// apply вносит выбранное изменение поля в копию исходного
					// профиля.
					apply func(*core.GroupProfile)
				}{
					{"rate_control", func(p *core.GroupProfile) {
						// Меняет RateControl на cbr при совместимых битрейтах.
						p.Encoding.RateControl = "cbr"
					}},
					{"rescale", func(p *core.GroupProfile) {
						// Отключает RescaleOutput для проверки участия масштабирования в идентичности.
						p.Encoding.RescaleOutput = false
					}},
					{"video_custom", func(p *core.GroupProfile) {
						// Задаёт VideoCustom nonref_p=1 для проверки участия дополнительных параметров в идентичности.
						p.Encoding.VideoCustom = "nonref_p=1"
					}},
					{"preset", func(p *core.GroupProfile) {
						// Меняет пресет NVENC на p7.
						p.Encoding.NVENC.Preset = "p7"
					}},
					{"tuning", func(p *core.GroupProfile) {
						// Меняет Tuning NVENC на ll.
						p.Encoding.NVENC.Tuning = "ll"
					}},
					{"multipass", func(p *core.GroupProfile) {
						// Меняет Multipass NVENC на fullres.
						p.Encoding.NVENC.Multipass = "fullres"
					}},
					{"lookahead", func(p *core.GroupProfile) {
						// Меняет LookaheadFrames NVENC на 29.
						p.Encoding.NVENC.LookaheadFrames = 29
					}},
					{"spatial_aq", func(p *core.GroupProfile) {
						// Отключает пространственный AQ NVENC.
						p.Encoding.NVENC.EnableSpatialAQ = false
					}},
					{"temporal_aq", func(p *core.GroupProfile) {
						// Включает временной AQ NVENC.
						p.Encoding.NVENC.EnableTemporalAQ = true
					}},
					{"aq_strength", func(p *core.GroupProfile) {
						// Меняет AQStrength NVENC на 15.
						p.Encoding.NVENC.AQStrength = 15
					}},
				}...)
			} else {
				changes = append(changes, []struct {
					// name задаёт имя текущего сценария проверки поля профиля.
					name string
					// apply вносит выбранное изменение поля в копию исходного
					// профиля.
					apply func(*core.GroupProfile)
				}{
					{"video_custom", func(p *core.GroupProfile) {
						// Задаёт VideoCustom lp=0 для SVTAV1.
						p.Encoding.VideoCustom = "lp=0"
					}},
					{"preset_zero", func(p *core.GroupProfile) {
						// Задаёт допустимый нулевой пресет SVTAV1.
						p.Encoding.SVTAV1.Preset = 0
					}},
					{"tuning", func(p *core.GroupProfile) {
						// Меняет Tuning SVTAV1 на vq.
						p.Encoding.SVTAV1.Tuning = "vq"
					}},
					{"lookahead", func(p *core.GroupProfile) {
						// Меняет LookaheadFrames SVTAV1 на нижнюю границу 6.
						p.Encoding.SVTAV1.LookaheadFrames = 6
					}},
				}...)
			}
			// key содержит вычисленный ключ полного профиля. canonical содержит канонические
			// байты полного профиля. err сообщает об ошибке вычисления канонической
			// идентичности профиля.
			key, canonical, err := core.ProfileIdentity(p)
			if err != nil {
				t.Fatal(err)
			}
			// vk содержит ключ варианта исходного явного профиля.
			vk, err := core.VariantIdentity("frozen-source", p, "generator-v1")
			if err != nil {
				t.Fatal(err)
			}
			// change вносит выбранное изменение параметра в копию исходного профиля.
			for _, change := range changes {
				t.Run(change.name, func(t *testing.T) {
					// Проверяет, что выбранное допустимое изменение настройки различается в Equal и ключах профиля и
					// варианта и точно сохраняется через JSON.

					// other содержит копию профиля с выбранным отличающимся
					// параметром.
					other := p
					change.apply(&other)
					// err сообщает об ошибке проверки допустимости профиля.
					if err := other.Validate(); err != nil {
						t.Fatalf("Допустимый вариант отвергнут: %v", err)
					}
					if p.Equal(other) {
						t.Fatal("Полное равенство проигнорировало настройку")
					}
					// otherKey содержит ключ изменённого полного профиля.
					// otherCanonical содержит канонические байты изменённого полного
					// профиля. err сообщает об ошибке вычисления канонической
					// идентичности профиля.
					otherKey, otherCanonical, err := core.ProfileIdentity(other)
					if err != nil || otherKey == key || bytes.Equal(otherCanonical, canonical) {
						t.Fatalf("Настройка не участвует в canonical identity: %v", err)
					}
					// otherVK содержит ключ варианта изменённого явного профиля.
					otherVK, err := core.VariantIdentity("frozen-source", other, "generator-v1")
					if err != nil || otherVK == vk {
						t.Fatalf("Настройка не участвует в variant identity: %v", err)
					}
					// payload содержит сохраняемые байты профиля с явными
					// настройками кодировщика.
					payload, err := core.EncodeProfile(other)
					if err != nil {
						t.Fatal(err)
					}
					// decoded содержит профиль после чтения проверяемых сохраняемых
					// байтов.
					decoded, err := core.DecodeProfile(payload)
					if err != nil || decoded != other {
						t.Fatalf("Материализованные значения изменились при чтении: %+v, %v", decoded, err)
					}
				})
			}
		})
	}
}

// Проверяет отказ при удалении любого обязательного поля сохранённого Encoding v2 вместо подстановки
// исходных настроек.
func TestExplicitStoredEncodingRejectsIncompleteValuesWithoutDefaults(t *testing.T) {
	// p содержит отдельный полный профиль текущего сценария проверки.
	p := explicitFixtureProfile(t, "h264_nvenc")
	// payload содержит сохраняемые байты профиля с явными настройками кодировщика. err сообщает об
	// ошибке кодирования проверенного профиля.
	payload, err := core.EncodeProfile(p)
	if err != nil {
		t.Fatal(err)
	}
	// fields предоставляет поля сохранённого JSON-профиля для выбранного изменения.
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(payload, &fields); err != nil {
		t.Fatal(err)
	}
	if string(fields["Version"]) != "2" {
		t.Fatalf("Нужен отдельный профильный discriminator v2: %s", payload)
	}
	// encoding предоставляет поля сохраняемого блока Encoding для проверки полноты.
	var encoding map[string]json.RawMessage
	if err = json.Unmarshal(fields["Encoding"], &encoding); err != nil {
		t.Fatal(err)
	}
	// field выбирает обязательное поле сохраняемого представления для проверки.
	for _, field := range []string{"Version", "VideoEncoder", "AudioEncoder", "RateControl", "RescaleOutput", "VideoCustom", "AudioCustom", "NVENC", "SVTAV1"} {
		t.Run(field, func(t *testing.T) {
			// Удаляет выбранное обязательное поле Encoding и проверяет ErrInvalidMetadata вместо подстановки
			// значений по умолчанию.

			// mutated содержит копию полей Encoding без выбранного обязательного поля.
			var mutated map[string]json.RawMessage
			// err сообщает об ошибке разбора JSON проверяемого представления.
			if err := json.Unmarshal(fields["Encoding"], &mutated); err != nil {
				t.Fatal(err)
			}
			delete(mutated, field)
			// b содержит JSON-блок Encoding после удаления обязательного поля. err сообщает
			// об ошибке кодирования проверяемого JSON-представления.
			b, err := json.Marshal(mutated)
			if err != nil {
				t.Fatal(err)
			}
			// envelope содержит отдельную корневую оболочку профиля с повреждённым Encoding.
			envelope := map[string]json.RawMessage{}
			maps.Copy(envelope, fields)
			envelope["Encoding"] = b
			b, err = json.Marshal(envelope)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = core.DecodeProfile(b); !errors.Is(err, core.ErrInvalidMetadata) {
				t.Fatalf("Частичный stored v2 получил defaults вместо отказа: %v", err)
			}
		})
	}
}

// Проверяет принятие точных допустимых границ кодировщиков и типизированный отказ за ними, включая
// расчёты буфера и неприменимые настройки.
func TestExplicitEncodingCheckedBoundaries(t *testing.T) {
	// tests перечисляет допустимые границы и нарушения конкретных кодировщиков.
	tests := []struct {
		// name задаёт имя текущего сценария проверки поля профиля. encoder выбирает проверяемый
		// видеокодировщик.
		name, encoder string
		// change вносит выбранное изменение параметра в копию исходного профиля.
		change func(*core.GroupProfile)
		// valid задаёт ожидаемую допустимость граничного сочетания параметров.
		valid bool
	}{
		{"nvenc_min_dimensions", "h264_nvenc", func(p *core.GroupProfile) {
			// Задаёт минимальные допустимые размеры NVENC 2 на 2.
			p.Format.Video.Width = 2
			p.Format.Video.Height = 2
		}, true},
		{"nvenc_max_dimensions", "h264_nvenc", func(p *core.GroupProfile) {
			// Задаёт максимальные допустимые размеры NVENC 65534 на 65534.
			p.Format.Video.Width = 65534
			p.Format.Video.Height = 65534
		}, true},
		{"nvenc_container_overflow", "h264_nvenc", func(p *core.GroupProfile) {
			// Задаёт ширину NVENC 65536, выходящую за границу контейнера.
			p.Format.Video.Width = 65536
		}, false},
		{"nvenc_b_lookahead_limit", "h264_nvenc", func(p *core.GroupProfile) {
			// Задаёт допустимую сумму четырёх B-кадров и LookaheadFrames 27.
			p.VideoEncoding.MaxBFrames = 4
			p.Encoding.NVENC.LookaheadFrames = 27
		}, true},
		{"nvenc_b_lookahead_overflow", "h264_nvenc", func(p *core.GroupProfile) {
			// Задаёт недопустимую сумму четырёх B-кадров и LookaheadFrames 28.
			p.VideoEncoding.MaxBFrames = 4
			p.Encoding.NVENC.LookaheadFrames = 28
		}, false},
		{"nvenc_baseline_b", "h264_nvenc", func(p *core.GroupProfile) {
			// Меняет видеопрофиль на baseline при сохранённых B-кадрах.
			p.Format.Video.CodecProfile = "baseline"
		}, false},
		{"nvenc_max_bitrate", "h264_nvenc", func(p *core.GroupProfile) {
			// Задаёт граничные допустимые битрейты math.MaxUint32 и буфер math.MaxInt32 для NVENC.

			p.VideoEncoding.TargetBitrateBPS = math.MaxUint32
			p.VideoEncoding.MaxBitrateBPS = math.MaxUint32
			p.VideoEncoding.BufferBits = math.MaxInt32
		}, true},
		{"nvenc_bitrate_overflow", "h264_nvenc", func(p *core.GroupProfile) {
			// Задаёт максимальный битрейт на единицу выше math.MaxUint32.
			p.VideoEncoding.MaxBitrateBPS = math.MaxUint32 + 1
		}, false},
		{"nvenc_cbr_mismatch", "h264_nvenc", func(p *core.GroupProfile) {
			// Включает CBR с несовпадающими целевым и максимальным битрейтами.
			p.Encoding.RateControl = "cbr"
			p.VideoEncoding.MaxBitrateBPS++
		}, false},
		{"svt_min_dimensions", "libsvtav1", func(p *core.GroupProfile) {
			// Задаёт минимальные допустимые размеры SVTAV1 64 на 64.
			p.Format.Video.Width = 64
			p.Format.Video.Height = 64
		}, true},
		{"svt_odd_dimensions", "libsvtav1", func(p *core.GroupProfile) {
			// Задаёт недопустимую нечётную ширину SVTAV1 65.
			p.Format.Video.Width = 65
		}, false},
		{"svt_max_dimensions", "libsvtav1", func(p *core.GroupProfile) {
			// Задаёт максимальные допустимые размеры SVTAV1 16384 на 8704.
			p.Format.Video.Width = 16384
			p.Format.Video.Height = 8704
		}, true},
		{"svt_dimensions_overflow", "libsvtav1", func(p *core.GroupProfile) {
			// Задаёт высоту SVTAV1 8706 за допустимой границей.
			p.Format.Video.Height = 8706
		}, false},
		{"svt_vbr_no_caps", "libsvtav1", func(p *core.GroupProfile) {
			// Задаёт запрещённый ненулевой максимальный битрейт VBR SVTAV1.
			p.VideoEncoding.MaxBitrateBPS = 1
		}, false},
		{"svt_vbr_no_buffer", "libsvtav1", func(p *core.GroupProfile) {
			// Задаёт запрещённый ненулевой буфер VBR SVTAV1.
			p.VideoEncoding.BufferBits = 1
		}, false},
		{"svt_lookahead_min", "libsvtav1", func(p *core.GroupProfile) {
			// Задаёт допустимую нижнюю границу LookaheadFrames SVTAV1 6.
			p.Encoding.SVTAV1.LookaheadFrames = 6
		}, true},
		{"svt_lookahead_max", "libsvtav1", func(p *core.GroupProfile) {
			// Задаёт допустимую верхнюю границу LookaheadFrames SVTAV1 120.
			p.Encoding.SVTAV1.LookaheadFrames = 120
		}, true},
		{"svt_lookahead_below", "libsvtav1", func(p *core.GroupProfile) {
			// Задаёт LookaheadFrames SVTAV1 5 ниже допустимой границы.
			p.Encoding.SVTAV1.LookaheadFrames = 5
		}, false},
		{"svt_lookahead_zero", "libsvtav1", func(p *core.GroupProfile) {
			// Задаёт недопустимый нулевой LookaheadFrames SVTAV1.
			p.Encoding.SVTAV1.LookaheadFrames = 0
		}, false},
		{"svt_lookahead_above", "libsvtav1", func(p *core.GroupProfile) {
			// Задаёт LookaheadFrames SVTAV1 121 выше допустимой границы.
			p.Encoding.SVTAV1.LookaheadFrames = 121
		}, false},
		{"svt_cbr_20ms", "libsvtav1", func(p *core.GroupProfile) {
			// Создаёт допустимый буфер CBR SVTAV1 длительностью 20 мс при битрейте 6000000.

			p.Encoding = core.DefaultEncoding(p.Mode, "libsvtav1", "cbr")
			p.VideoEncoding.MaxBitrateBPS = 6000000
			p.VideoEncoding.BufferBits = 120000
		}, true},
		{"svt_cbr_19ms", "libsvtav1", func(p *core.GroupProfile) {
			// Создаёт недопустимый буфер CBR SVTAV1 длительностью 19 мс при битрейте 6000000.

			p.Encoding = core.DefaultEncoding(p.Mode, "libsvtav1", "cbr")
			p.VideoEncoding.MaxBitrateBPS = 6000000
			p.VideoEncoding.BufferBits = 114000
		}, false},
		{"svt_cbr_10000ms", "libsvtav1", func(p *core.GroupProfile) {
			// Создаёт допустимый буфер CBR SVTAV1 длительностью 10000 мс.

			p.Encoding = core.DefaultEncoding(p.Mode, "libsvtav1", "cbr")
			p.VideoEncoding.MaxBitrateBPS = 6000000
			p.VideoEncoding.BufferBits = 60000000
		}, true},
		{"svt_cbr_10001ms", "libsvtav1", func(p *core.GroupProfile) {
			// Создаёт недопустимый буфер CBR SVTAV1 длительностью 10001 мс.

			p.Encoding = core.DefaultEncoding(p.Mode, "libsvtav1", "cbr")
			p.VideoEncoding.MaxBitrateBPS = 6000000
			p.VideoEncoding.BufferBits = 60006000
		}, false},
		{"svt_cbr_fractional_ms", "libsvtav1", func(p *core.GroupProfile) {
			// Создаёт буфер CBR SVTAV1 120001 бит с нецелой длительностью в миллисекундах.

			p.Encoding = core.DefaultEncoding(p.Mode, "libsvtav1", "cbr")
			p.VideoEncoding.MaxBitrateBPS = 6000000
			p.VideoEncoding.BufferBits = 120001
		}, false},
		{"svt_cbr_buffer_uint64", "libsvtav1", func(p *core.GroupProfile) {
			// Создаёт буфер CBR SVTAV1 math.MaxUint64 для проверки переполнения.

			p.Encoding = core.DefaultEncoding(p.Mode, "libsvtav1", "cbr")
			p.VideoEncoding.MaxBitrateBPS = 6000000
			p.VideoEncoding.BufferBits = math.MaxUint64
		}, false},
		{"aac_max_bitrate", "h264_nvenc", func(p *core.GroupProfile) {
			// Задаёт допустимый максимальный аудиобитрейт AAC 576000.
			p.AudioEncoding.BitrateBPS = 576000
		}, true},
		{"aac_bitrate_clamp", "h264_nvenc", func(p *core.GroupProfile) {
			// Поднимает аудиобитрейт AAC на единицу выше допустимой границы.
			p.AudioEncoding.BitrateBPS = 576001
		}, false},
		{"aac_unrepresentable_sample_entry", "h264_nvenc", func(p *core.GroupProfile) {
			// Задаёт частоту AAC 96000 Гц, непредставимую выбранным форматом записи.
			p.Format.Audio.SampleRateHz = 96000
		}, false},
		{"copy_cannot_rescale", "libsvtav1", func(p *core.GroupProfile) {
			// Включает запрещённое масштабирование в режиме копирования.
			p.Encoding.RescaleOutput = true
		}, false},
		{"inapplicable_svt", "h264_nvenc", func(p *core.GroupProfile) {
			// Добавляет настройки SVTAV1 к профилю NVENC.
			p.Encoding.SVTAV1.Preset = 8
		}, false},
		{"partial_legacy_block", "h264_nvenc", func(p *core.GroupProfile) {
			// Создаёт неполный блок EncodingSettings с одним RateControl без версии.
			p.Encoding = core.EncodingSettings{RateControl: "vbr"}
		}, false},
	}
	// tt содержит очередной граничный сценарий кодировщика и ожидаемую допустимость.
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			// Проверяет принятие допустимой границы либо содержательный типизированный ErrInvalidProfile для
			// выбранного нарушения.

			// p содержит отдельный полный профиль текущего сценария проверки.
			p := explicitFixtureProfile(t, tt.encoder)
			tt.change(&p)
			// err сообщает об ошибке проверки допустимости профиля.
			err := p.Validate()
			if tt.valid {
				if err != nil {
					t.Fatalf("Допустимая граница отвергнута: %v", err)
				}
			} else {
				// pe принимает типизированный отказ профиля для проверки доверенной
				// диагностики.
				var pe *core.ProfileError
				if !errors.Is(err, core.ErrInvalidProfile) || !errors.As(err, &pe) || pe.Code() == "" || pe.Field() == "" || pe.Detail() == "" {
					t.Fatalf("Нужен доверенный отказ профиля: %v", err)
				}
			}
		})
	}
}

// Проверяет одинаковую идентичность нормализованных пользовательских параметров и точные категории
// конфликтов, повторов и неподдерживаемых значений.
func TestCustomEncodingCanonicalizationAndConflictBoundary(t *testing.T) {
	// raw содержит очередную эквивалентную запись дополнительных параметров NVENC.
	for _, raw := range []string{"strict_gop=+01 nonref_p=000", " nonref_p=0  strict_gop=1 "} {
		// normalized содержит каноническую строку пользовательских параметров. err сообщает об
		// ошибке нормализации пользовательских параметров кодировщика.
		normalized, err := core.NormalizeCustom("h264_nvenc", raw)
		if err != nil || normalized != "nonref_p=0 strict_gop=1" {
			t.Fatalf("Канонизация %q: %q, %v", raw, normalized, err)
		}
	}
	// base задаёт профиль с канонической строкой дополнительных параметров.
	base := explicitFixtureProfile(t, "h264_nvenc")
	base.Encoding.VideoCustom = "nonref_p=0 strict_gop=1"
	// key содержит вычисленный ключ полного профиля. err сообщает об ошибке вычисления канонической
	// идентичности профиля.
	key, _, err := core.ProfileIdentity(base)
	if err != nil {
		t.Fatal(err)
	}
	// same содержит копию профиля с эквивалентно нормализованными параметрами.
	same := base
	// canonical содержит нормализованную эквивалентную строку дополнительных параметров.
	canonical, err := core.NormalizeCustom("h264_nvenc", "strict_gop=01 nonref_p=+0")
	if err != nil {
		t.Fatal(err)
	}
	same.Encoding.VideoCustom = canonical
	// sameKey содержит ключ профиля с эквивалентной записью дополнительных параметров.
	sameKey, _, err := core.ProfileIdentity(same)
	if err != nil || sameKey != key || !same.Equal(base) {
		t.Fatal("Порядок custom изменил полную идентичность")
	}
	// input содержит очередной проверяемый вход выбранного сценария.
	for _, input := range []struct { /* encoder выбирает проверяемый кодировщик видео или аудио. value содержит строку дополнительных параметров с проверяемым нарушением. code задаёт ожидаемую категорию отказа NormalizeCustom. */
		encoder, value, code string
	}{
		{"h264_nvenc", "rc=vbr", "custom_conflict"}, {"h264_nvenc", "b:v=6000000", "custom_conflict"}, {"h264_nvenc", "preset=p4", "custom_conflict"},
		{"libsvtav1", "pred-struct=1", "custom_conflict"}, {"libsvtav1", "lookahead=6", "custom_conflict"}, {"aac", "ar=48000", "custom_conflict"},
		{"h264_nvenc", "nonref_p=0 nonref_p=1", "duplicate_custom"}, {"h264_nvenc", "lp=1", "unknown_custom"}, {"libsvtav1", "nonref_p=1", "unknown_custom"},
		{"aac", "aac_coder=fast\n", "invalid_custom"}, {"h264_nvenc", "nonref_p='1'", "invalid_custom"}, {"h264_nvenc", "nonref_p=1=1", "invalid_custom"},
		{"h264_nvenc", "nonref_p=2", "invalid_custom"}, {"libsvtav1", "lp=7", "invalid_custom"},
	} {
		t.Run(input.encoder+"/"+input.value, func(t *testing.T) {
			// Проверяет точный код отказа NormalizeCustom для конфликтующих, повторных, чужих или синтаксически
			// недопустимых параметров.

			// err сообщает об ошибке нормализации пользовательских параметров кодировщика.
			_, err := core.NormalizeCustom(input.encoder, input.value)
			// pe принимает типизированный отказ профиля для проверки доверенной диагностики.
			var pe *core.ProfileError
			if !errors.As(err, &pe) || pe.Code() != input.code || pe.Field() == "" || pe.Detail() == "" {
				t.Fatalf("Неверная категория отказа: %v", err)
			}
		})
	}
}
