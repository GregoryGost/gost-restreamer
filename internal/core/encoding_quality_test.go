package core_test

import (
	"bytes"
	"encoding/json"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// qualityProfile создаёт полный SVT-профиль с выбранным качеством и явным LIVE copy.
// rc выбирает режим CQP либо CRF, для которого задаётся обязательное качество.
func qualityProfile(rc string) core.GroupProfile {
	// p содержит допустимый исходный AV1-профиль до переключения управления качеством.
	p := encodedProfile(core.VideoEncoderSVTAV1, core.RateControlVBR)
	p.Encoding = core.DefaultEncoding(p.Mode, core.VideoEncoderSVTAV1, rc)
	p.LiveAudioMode = core.LiveAudioCopy
	p.VideoEncoding.TargetBitrateBPS = 0
	if rc == core.RateControlCQP {
		p.Encoding.QP = core.OptionalQP{Set: true, Value: 35}
	} else {
		p.Encoding.CRF = core.OptionalCRF{Set: true, ValueHundredths: 3500}
	}
	return p
}

// TestQualityProfileRoundTripAndPresence проверяет полный профиль, идентичность и обязательные компоненты качества.
// t сообщает об ошибках проверяемого сценария.
func TestQualityProfileRoundTripAndPresence(t *testing.T) {
	// rc выбирает отдельный режим качества для полного цикла сохранения.
	for _, rc := range []string{core.RateControlCQP, core.RateControlCRF} {
		// p содержит полный профиль текущего режима; data и err — его сохранённые байты и отказ.
		p := qualityProfile(rc)
		data, err := core.EncodeProfile(p)
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(data, []byte(`"Encoding":{"Version":`)) {
			t.Fatal(string(data))
		}
		// decoded содержит восстановленное сравнимое значение.
		decoded, err := core.DecodeProfile(data)
		if err != nil || decoded != p {
			t.Fatal(decoded, err)
		}
		// before содержит исходную идентичность; changed — копию с другим качеством.
		before, _, err := core.ProfileIdentity(p)
		if err != nil {
			t.Fatal(err)
		}
		changed := p
		if rc == core.RateControlCQP {
			changed.Encoding.QP.Value++
		} else {
			changed.Encoding.CRF.ValueHundredths += 100
		}
		// after содержит идентичность изменённого рецепта, которая должна отличаться.
		after, _, err := core.ProfileIdentity(changed)
		if err != nil || after == before {
			t.Fatal(after, err)
		}
		if rc == core.RateControlCRF {
			// fractional заменяет сохраняемое целое дробью, которая округляется к нему в float64.
			fractional := bytes.Replace(data, []byte(`"ValueHundredths":3500`), []byte(`"ValueHundredths":3500.0000000000000001`), 1)
			if _, err = core.DecodeProfile(fractional); err == nil {
				t.Fatal("Сохранённая дробь округлена до целого CRF")
			}
		}

		// field выбирает обязательный объект качества новой сохранённой формы.
		for _, field := range []string{"QP", "CRF"} {
			// component выбирает присутствие либо значение внутри объекта.
			for _, component := range []string{"Set", "Value"} {
				if field == "CRF" && component == "Value" {
					component = "ValueHundredths"
				}
				// root хранит независимую изменяемую JSON-копию профиля.
				var root map[string]any
				if err = json.Unmarshal(data, &root); err != nil {
					t.Fatal(err)
				}
				delete(root["Encoding"].(map[string]any)[field].(map[string]any), component)
				// bad содержит неполную сохранённую форму; err сообщает об отказе её сериализации и проверки.
				bad, err := json.Marshal(root)
				if err != nil {
					t.Fatal(err)
				}
				if _, err = core.DecodeProfile(bad); err == nil {
					t.Fatal("Принят пропуск", field, component)
				}
			}
		}
	}
}

// TestQualityValidationRejectsInapplicableSettings проверяет границы и несовместимые режимы нового рецепта.
// t сообщает об ошибках проверяемого сценария.
func TestQualityValidationRejectsInapplicableSettings(t *testing.T) {
	// edit изменяет независимую копию допустимого CRF-профиля для проверки отказа.
	// Параметр p каждого обработчика edit указывает на отдельную изменяемую копию проверяемого профиля.
	for _, edit := range []func(*core.GroupProfile){
		func(p *core.GroupProfile) { /* Оставляет ненулевое число при отсутствующем качестве. */
			p.Encoding.CRF.Set = false
		},
		func(p *core.GroupProfile) { /* Задаёт запрещённый нулевой CRF. */
			p.Encoding.CRF.ValueHundredths = 0
		},
		func(p *core.GroupProfile) { /* Превышает верхнюю границу качества. */
			p.Encoding.CRF.ValueHundredths = 6400
		},
		func(p *core.GroupProfile) { /* Нарушает целочисленность SVT CRF. */
			p.Encoding.CRF.ValueHundredths = 150
		},
		func(p *core.GroupProfile) { /* Нарушает нулевое значение отсутствующего QP. */
			p.Encoding.QP.Value = 1
		},
		func(p *core.GroupProfile) { /* Добавляет несовместимый с CRF квантователь. */
			p.Encoding.QP = core.OptionalQP{Set: true, Value: 1}
		},
		func(p *core.GroupProfile) { /* Смешивает чистое качество с целевым битрейтом. */
			p.VideoEncoding.TargetBitrateBPS = 1
		},
		func(p *core.GroupProfile) { /* Отключает обязательный для CRF AQ2. */
			p.Encoding.SVTAV1.AQMode = 0
		},
		func(p *core.GroupProfile) { /* Задаёт запрещённый для CRF нулевой lookahead. */
			p.Encoding.SVTAV1.LookaheadFrames = 0
		},
		func(p *core.GroupProfile) { /* Убирает обязательный селектор LIVE-аудио. */
			p.LiveAudioMode = ""
		},
	} {
		// p содержит новый допустимый рецепт перед единственной ошибкой.
		p := qualityProfile(core.RateControlCRF)
		edit(&p)
		if p.Validate() == nil {
			t.Fatal("Недопустимое качество принято", p)
		}
	}
	// tuning выбирает все новые CRF tuning, которые запрещены для CQP.
	for _, tuning := range []string{"ssim", "ms_ssim", "vmaf"} {
		// p содержит CRF-профиль для проверки применимости tuning.
		p := qualityProfile(core.RateControlCRF)
		p.Encoding.SVTAV1.Tuning = tuning
		if err := p.Validate(); err != nil {
			t.Fatal(err)
		} // err сообщает отказ допустимого CRF tuning.
		p = qualityProfile(core.RateControlCQP)
		p.Encoding.SVTAV1.Tuning = tuning
		if p.Validate() == nil {
			t.Fatal("CQP принял", tuning)
		}
	}
}

// TestMultichannelAACRequiresMaterializedEncoding проверяет точные пары AAC в CRF/VBR и отказ нулевого Encoding.
// t сообщает об ошибках проверяемого сценария.
func TestMultichannelAACRequiresMaterializedEncoding(t *testing.T) {
	// layout обозначает каноническую схему; channels содержит её точное число каналов.
	for layout, channels := range map[string]uint16{"2.1": 3, "3.0": 3, "4.0": 4, "5.0": 5, "5.1": 6, "7.1": 8} {
		// p содержит полный новый рецепт с проверяемой схемой AAC.
		p := qualityProfile(core.RateControlCRF)
		p.Format.Audio.ChannelLayout, p.Format.Audio.Channels = layout, channels
		if err := p.Validate(); err != nil {
			t.Fatal(layout, err)
		} // err сообщает отказ канонической пары.
		p.Encoding = core.DefaultEncoding(p.Mode, core.VideoEncoderSVTAV1, core.RateControlVBR)
		p.VideoEncoding.TargetBitrateBPS = 4000000
		if err := p.Validate(); err != nil {
			t.Fatal("Текущий VBR отклонил AAC", layout, err)
		}
		p.Encoding = core.EncodingSettings{}
		p.VideoEncoding.MaxBitrateBPS, p.VideoEncoding.BufferBits = 4000000, 8000000
		if p.Validate() == nil {
			t.Fatal("Нулевой Encoding принят для", layout)
		}
	}
}

// TestAbsentCRFZeroHasSingleStoredIdentity проверяет единственную форму нуля отсутствующего CRF
// во всех применимых режимах текущего SVT/NVENC, включая повторное сохранение входного JSON с -0.
// t сообщает об ошибках проверяемого сценария.
func TestAbsentCRFZeroHasSingleStoredIdentity(t *testing.T) {
	// recipe задаёт кодировщик и управление качеством, для которых CRF обязан отсутствовать.
	for _, recipe := range []struct {
		// encoder выбирает проверяемый видеокодировщик, rc — его режим управления качеством.
		encoder, rc string
	}{
		{core.VideoEncoderNVENC, core.RateControlCBR},
		{core.VideoEncoderNVENC, core.RateControlVBR},
		{core.VideoEncoderSVTAV1, core.RateControlCBR},
		{core.VideoEncoderSVTAV1, core.RateControlVBR},
		{core.VideoEncoderSVTAV1, core.RateControlCQP},
	} {
		// Проверяет для текущего recipe точные байты, оба ключа и сохранность исходного знака нуля.
		// t сообщает об ошибках проверяемого сценария.
		t.Run(recipe.encoder+"/"+recipe.rc, func(t *testing.T) {
			// positive содержит допустимый профиль с положительным нулём отсутствующего CRF.
			positive := encodedProfile(recipe.encoder, recipe.rc)
			positive.LiveAudioMode = core.LiveAudioCopy
			if recipe.rc == core.RateControlCQP {
				positive = qualityProfile(recipe.rc)
			}
			// expected хранит точную положительную форму; err сообщает отказ кодирования или проверки.
			expected, err := core.EncodeProfile(positive)
			if err != nil || !bytes.Contains(expected, []byte(`"CRF":{"Set":false,"ValueHundredths":0}`)) {
				t.Fatalf("Отсутствует ожидаемый канонический ноль: %s %v", expected, err)
			}
			// signedInput сохраняет исходную отрицательную лексему нуля старого JSON.
			signedInput := bytes.Replace(expected, []byte(`"CRF":{"Set":false,"ValueHundredths":0}`), []byte(`"CRF":{"Set":false,"ValueHundredths":-0}`), 1)
			// sourceInput позволяет проверить, что читатель не переписывает исходные байты.
			sourceInput := bytes.Clone(signedInput)
			// negative содержит доменную копию входного JSON с нормализованным целым нулём.
			negative, err := core.DecodeProfile(signedInput)
			if err != nil || !positive.Equal(negative) || positive != negative {
				t.Fatal("Знак нуля изменил доменное равенство допустимых профилей", err)
			}
			// stored содержит повторную JSON-запись профиля с отрицательным нулём во входе.
			stored, err := json.Marshal(negative)
			if err != nil || !bytes.Equal(stored, expected) {
				t.Fatalf("Равные профили сохранены разными байтами: %s %v", stored, err)
			}
			// expectedKey и expectedCanonical фиксируют ключ и канонические байты положительного нуля.
			expectedKey, expectedCanonical, err := core.ProfileIdentity(positive)
			if err != nil || !bytes.Equal(expectedCanonical, expected) {
				t.Fatalf("Неожиданная исходная каноническая форма: %s %v", expectedCanonical, err)
			}
			// key и canonical содержат идентичность профиля с отрицательным нулём для точного сравнения.
			key, canonical, err := core.ProfileIdentity(negative)
			if err != nil || key != expectedKey || !bytes.Equal(canonical, expectedCanonical) {
				t.Fatalf("Знак отсутствующего CRF изменил ключ или байты: %s %v", key, err)
			}
			// expectedVariant фиксирует ключ варианта с теми же источником и генератором.
			expectedVariant, err := core.VariantIdentity("synthetic-source", positive, core.CurrentFallbackGeneratorVersion)
			if err != nil {
				t.Fatal(err)
			}
			// variant содержит ключ варианта профиля с отрицательным нулём.
			variant, err := core.VariantIdentity("synthetic-source", negative, core.CurrentFallbackGeneratorVersion)
			if err != nil || variant != expectedVariant {
				t.Fatalf("Знак отсутствующего CRF изменил ключ варианта: %s %v", variant, err)
			}
			// decoded принимает прочитанный профиль для проверки канонического повторного сохранения.
			decoded, err := core.DecodeProfile(signedInput)
			if err != nil || decoded != positive {
				t.Fatalf("Не прочитан равный профиль с -0: %v", err)
			}
			stored, err = core.EncodeProfile(decoded)
			if err != nil || !bytes.Equal(stored, expected) {
				t.Fatalf("Повторное сохранение оставило неканонический ноль: %s %v", stored, err)
			}
			decoded, err = core.DecodeProfile(stored)
			if err != nil || decoded != positive || decoded.Encoding.CRF.ValueHundredths != 0 {
				t.Fatalf("Каноническая форма не пережила чтение: %v", err)
			}
			if !bytes.Equal(signedInput, sourceInput) {
				t.Fatal("Чтение или кодирование изменило исходный JSON")
			}
		})
	}
}

// TestStoredQualityRejectsNonFiniteNumbers проверяет отказ NaN/Inf при чтении полного качества.
// Домен с целыми сотыми не способен содержать эти значения; отказ не изменяет существующий профиль.
// t сообщает об ошибках проверяемого сценария.
func TestStoredQualityRejectsNonFiniteNumbers(t *testing.T) {
	// source содержит полный текущий JSON; err сообщает отказ его кодирования.
	source, err := core.EncodeProfile(qualityProfile(core.RateControlCRF))
	if err != nil {
		t.Fatal(err)
	}
	// token задаёт нечисловую либо переполняющую JSON-лексему качества.
	for _, token := range []string{"NaN", "Inf", "-Inf", "1e999999999999", "\"NaN\"", "\"Infinity\""} {
		// damaged заменяет только значение качества; receiver и before проверяют атомарность отказа.
		damaged := bytes.Replace(source, []byte(`"CRF":{"Set":true,"ValueHundredths":3500}`), []byte(`"CRF":{"Set":true,"ValueHundredths":`+token+`}`), 1)
		receiver := encodedProfile(core.VideoEncoderNVENC, core.RateControlCBR)
		before := receiver
		if err = receiver.UnmarshalJSON(damaged); err == nil || receiver != before {
			t.Fatalf("Недопустимое качество %s принято либо изменило получателя: %v", token, err)
		}
	}
}
