package core_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// TestMediaExpansionAbsentCRFNegativeZeroHasOneIdentity связывает JSON +0/-0
// с одинаковыми доменными значениями, каноническими байтами и ключами профиля и варианта.
// t сообщает об ошибках проверяемого сценария.
func TestMediaExpansionAbsentCRFNegativeZeroHasOneIdentity(t *testing.T) {
	// positive содержит допустимый CQP с отсутствующим CRF перед чтением JSON с -0.
	positive := mediaQualityProfile()
	positive.Encoding = core.DefaultEncoding(positive.Mode, core.VideoEncoderSVTAV1, core.RateControlCQP)
	positive.Encoding.QP = core.OptionalQP{Set: true, Value: 35}
	// positiveKey и positiveCanonical описывают положительный ноль; err сообщает об отказе кодирования.
	positiveKey, positiveCanonical, err := core.ProfileIdentity(positive)
	if err != nil {
		t.Fatal(err)
	}
	// signedInput содержит старую внешнюю форму с отрицательным нулём, который домен хранит одним целым нулём.
	signedInput := bytes.Replace(positiveCanonical, []byte(`"CRF":{"Set":false,"ValueHundredths":0}`), []byte(`"CRF":{"Set":false,"ValueHundredths":-0}`), 1)
	// negative содержит профиль после обязательной проверки JSON.
	negative, err := core.DecodeProfile(signedInput)
	if err != nil || positive != negative || !positive.Equal(negative) {
		t.Fatal("Равные JSON-профили не равны", err)
	}
	// negativeKey и negativeCanonical должны задавать ту же полную идентичность.
	negativeKey, negativeCanonical, err := core.ProfileIdentity(negative)
	if err != nil || negativeKey != positiveKey || !bytes.Equal(negativeCanonical, positiveCanonical) {
		t.Fatal("Знак отсутствующего CRF изменил canonical/key", err)
	}
	// positiveVariant и negativeVariant задают варианты одного источника при равных профилях.
	positiveVariant, err := core.VariantIdentity("synthetic-source", positive, core.CurrentFallbackGeneratorVersion)
	if err != nil {
		t.Fatal(err)
	}
	negativeVariant, err := core.VariantIdentity("synthetic-source", negative, core.CurrentFallbackGeneratorVersion)
	if err != nil || positiveVariant != negativeVariant {
		t.Fatal("Знак нуля изменил VariantIdentity", err)
	}
}

// TestMediaExpansionQualityNestedCopies проверяет качество, его присутствие и раскладку во всех
// сохраняемых вложенных профилях и независимость снимков от дальнейших изменений конфигурации.
func TestMediaExpansionQualityNestedCopies(t *testing.T) {
	// p содержит полный новый профиль и явно отличает 2.1 от трёх фронтальных каналов.
	p := mediaQualityProfile()
	p.Format.Audio.Channels, p.Format.Audio.ChannelLayout = 3, "2.1"
	// key и canonical связывают один точный профиль с манифестом и подготовкой; err сообщает об отказе.
	key, canonical, err := core.ProfileIdentity(p)
	if err != nil {
		t.Fatal(err)
	}
	// session и output содержат согласованные исходные контейнеры снимков.
	session, output := contractSnapshots(t)
	session.Groups[0].Profile.Profile = p
	output.Profile.Profile = p
	output.Artifact.Profile, output.Artifact.ProfileKey, output.Artifact.CanonicalProfile, output.Artifact.VerifiedFormat = p, key, canonical, p.Format
	// generation содержит отдельную сохраняемую копию требуемого профиля варианта.
	generation := validGeneration(t)
	generation.RequiredProfiles[0].Profile, generation.RequiredProfiles[0].ProfileKey, generation.RequiredProfiles[0].CanonicalProfile = p, key, canonical
	// sessionBytes и outputBytes содержат реальные сохраняемые оболочки снимков.
	sessionBytes, err := core.EncodeSessionSnapshot(session)
	if err != nil {
		t.Fatal(err)
	}
	outputBytes, err := core.EncodeOutputSnapshot(output)
	if err != nil {
		t.Fatal(err)
	}
	// decodedSession и decodedOutput получают самостоятельные профили при чтении метаданных.
	decodedSession, err := core.DecodeSessionSnapshot(sessionBytes)
	if err != nil || !reflect.DeepEqual(decodedSession, session) {
		t.Fatal("Session потеряла поля качества", err)
	}
	decodedOutput, err := core.DecodeOutputSnapshot(outputBytes)
	if err != nil || !reflect.DeepEqual(decodedOutput, output) {
		t.Fatal("Output/manifest потеряли поля качества", err)
	}
	// generationBytes содержит сохранённое поколение; decodedGeneration восстанавливает требования подготовки.
	generationBytes, err := core.EncodeMetadata(generation)
	if err != nil {
		t.Fatal(err)
	}
	var decodedGeneration core.FallbackGeneration
	if err = core.DecodeMetadata(generationBytes, &decodedGeneration); err != nil || !reflect.DeepEqual(decodedGeneration, generation) {
		t.Fatal("RequiredProfile потерял качество", err)
	}
	p.Encoding.CRF.ValueHundredths = 3600
	if decodedSession.Groups[0].Profile.Profile.Encoding.CRF.ValueHundredths != 3500 || decodedOutput.Profile.Profile.Encoding.CRF.ValueHundredths != 3500 || decodedOutput.Artifact.Profile.Encoding.CRF.ValueHundredths != 3500 || decodedGeneration.RequiredProfiles[0].Profile.Encoding.CRF.ValueHundredths != 3500 {
		t.Fatal("Копии удерживают изменяемые настройки")
	}
}

// mediaQualityProfile строит полный AV1 CRF-профиль с отдельным присутствием качества.
func mediaQualityProfile() core.GroupProfile {
	// p содержит независимый допустимый профиль с чистым режимом качества.
	p := encodedProfile(core.VideoEncoderSVTAV1, "vbr")
	p.LiveAudioMode = core.LiveAudioCopy
	p.Encoding = core.DefaultEncoding(p.Mode, core.VideoEncoderSVTAV1, core.RateControlCRF)
	p.Encoding.CRF = core.OptionalCRF{Set: true, ValueHundredths: 3500}
	p.VideoEncoding.TargetBitrateBPS = 0
	return p
}

// TestMediaExpansionQualityStoredIdentity проверяет полный профиль, точный цикл записи и чтения,
// самостоятельность копий и различимость качеств в canonical/key/VariantIdentity.
// t сообщает об ошибках проверяемого сценария.
func TestMediaExpansionQualityStoredIdentity(t *testing.T) {
	// p содержит полный профиль; before сохраняет значение для проверки неизменности.
	p := mediaQualityProfile()
	before := p
	// raw содержит сохраняемый JSON; err сообщает о невозможности кодирования.
	raw, err := core.EncodeProfile(p)
	if err != nil {
		t.Fatal(err)
	}
	// fields принимает поля внешнего профиля для независимой проверки версий.
	var fields map[string]json.RawMessage
	if err = json.Unmarshal(raw, &fields); err != nil {
		t.Fatal(err)
	}
	if len(fields["Version"]) != 0 || core.MetadataVersion != 1 {
		t.Fatal("Смешаны оси версий", string(raw))
	}
	// decoded содержит точную копию сохраняемого значения.
	decoded, err := core.DecodeProfile(raw)
	if err != nil || decoded != p || p != before {
		t.Fatal("Roundtrip изменил профиль", err)
	}
	// key и canonical задают исходную идентичность профиля; variant — идентичность варианта.
	key, canonical, err := core.ProfileIdentity(p)
	if err != nil {
		t.Fatal(err)
	}
	variant, err := core.VariantIdentity("synthetic-source", p, core.CurrentFallbackGeneratorVersion)
	if err != nil {
		t.Fatal(err)
	}
	decoded.Encoding.CRF.ValueHundredths = 3600
	// otherKey и otherCanonical описывают профиль с другим качеством.
	otherKey, otherCanonical, err := core.ProfileIdentity(decoded)
	if err != nil || otherKey == key || bytes.Equal(otherCanonical, canonical) || decoded.Equal(p) || p != before {
		t.Fatal("Качество не изменило идентичность", err)
	}
	// otherVariant проверяет необходимость отдельной подготовки изменённого качества.
	otherVariant, err := core.VariantIdentity("synthetic-source", decoded, core.CurrentFallbackGeneratorVersion)
	if err != nil || otherVariant == variant {
		t.Fatal("Качество не изменило вариант", err)
	}
}

// TestMediaExpansionQualityRejectsInconsistentValues проверяет отрицательные
// доменные сценарии без зависимости от HTTP-парсера и выбранной команды FFmpeg.
// t сообщает об ошибках проверяемого сценария.
func TestMediaExpansionQualityRejectsInconsistentValues(t *testing.T) {
	// change задаёт одно нарушение независимого контракта качества или обязательности селектора.
	// Параметр p каждого обработчика change указывает на отдельную изменяемую копию проверяемого профиля.
	for _, change := range []func(*core.GroupProfile){
		func(p *core.GroupProfile) { /* Проверяет отсутствие обязательного CRF. */
			p.Encoding.CRF.Set = false
			p.Encoding.CRF.ValueHundredths = 0
		},
		func(p *core.GroupProfile) { /* Проверяет ненулевое отсутствующее качество. */
			p.Encoding.QP.Value = 1
		},
		func(p *core.GroupProfile) { /* Проверяет одновременное присутствие двух качеств. */
			p.Encoding.QP = core.OptionalQP{Set: true, Value: 35}
		},
		func(p *core.GroupProfile) { /* Проверяет нулевое качество. */
			p.Encoding.CRF.ValueHundredths = 0
		},
		func(p *core.GroupProfile) { /* Проверяет верхнюю границу. */
			p.Encoding.CRF.ValueHundredths = 6400
		},
		func(p *core.GroupProfile) { /* Проверяет математически дробное качество. */
			p.Encoding.CRF.ValueHundredths = 3550
		},
		func(p *core.GroupProfile) { /* Проверяет несовместимое AQ. */
			p.Encoding.SVTAV1.AQMode = 0
		},
		func(p *core.GroupProfile) { /* Проверяет запрещённый верхний предел битрейта. */
			p.VideoEncoding.MaxBitrateBPS = 1
		},
		func(p *core.GroupProfile) { /* Проверяет запрещённый VBV. */
			p.VideoEncoding.BufferBits = 1
		},
		func(p *core.GroupProfile) { /* Проверяет запрещённый целевой битрейт. */
			p.VideoEncoding.TargetBitrateBPS = 1
		},
		func(p *core.GroupProfile) { /* Проверяет недопустимый короткий lookahead. */
			p.Encoding.SVTAV1.LookaheadFrames = 5
		},
		func(p *core.GroupProfile) { /* Проверяет отсутствие материализованного режима LIVE-аудио. */
			p.LiveAudioMode = ""
		},
	} {
		// p содержит самостоятельный испорченный профиль; err — отказ доменной проверки.
		p := mediaQualityProfile()
		change(&p)
		if err := p.Validate(); !errors.Is(err, core.ErrInvalidProfile) {
			t.Fatalf("Принято несовместимое качество: %+v, %v", p, err)
		}
	}
}

// TestMediaExpansionStoredPresenceIsComplete требует признака присутствия и значения качества
// и запрещает null в сохранённом полном профиле, не изменяя получателя при отказе.
// t сообщает об ошибках проверяемого сценария.
func TestMediaExpansionStoredPresenceIsComplete(t *testing.T) {
	// raw содержит исходный полный профиль; err сообщает об ошибке его кодирования.
	raw, err := core.EncodeProfile(mediaQualityProfile())
	if err != nil {
		t.Fatal(err)
	}
	// field выбирает новое обязательное поле Encoding.
	for _, field := range []string{"QP", "CRF"} {
		// mutation выбирает отсутствие всего поля, null либо неполный объект присутствия.
		for _, mutation := range []string{"absent", "null", "set", "value"} {
			// root и encoding принимают самостоятельные JSON-объекты для повреждения.
			var root, encoding map[string]json.RawMessage
			if err = json.Unmarshal(raw, &root); err != nil {
				t.Fatal(err)
			}
			if err = json.Unmarshal(root["Encoding"], &encoding); err != nil {
				t.Fatal(err)
			}
			switch mutation {
			case "absent":
				delete(encoding, field)
			case "null":
				encoding[field] = json.RawMessage("null")
			case "set":
				encoding[field] = json.RawMessage(`{"Set":true}`)
			case "value":
				encoding[field] = json.RawMessage(`{"Value":35}`)
			}
			root["Encoding"], err = json.Marshal(encoding)
			if err != nil {
				t.Fatal(err)
			}
			// damaged содержит повреждённые байты; receiver и before проверяют атомарность чтения.
			// err сообщает об отказе сериализации повреждённой тестовой формы.
			damaged, err := json.Marshal(root)
			if err != nil {
				t.Fatal(err)
			}
			receiver := encodedProfile(core.VideoEncoderNVENC, "vbr")
			before := receiver
			if err = receiver.UnmarshalJSON(damaged); !errors.Is(err, core.ErrInvalidMetadata) || receiver != before {
				t.Fatalf("Неполное %s/%s принято: %v", field, mutation, err)
			}
		}
	}
}
