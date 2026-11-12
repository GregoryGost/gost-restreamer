package core_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// frozenLiveAudioProfile сохраняет эталонные байты и ключи прежнего профиля и вложенных снимков.
type frozenLiveAudioProfile struct {
	// Name различает закреплённые эталоны AV1 и H.264.
	Name string
	// Payload содержит точные сохранённые байты прежнего профиля. Canonical содержит эталонные
	// канонические байты прежнего профиля.
	Payload, Canonical []byte
	// ProfileKey содержит эталонный ключ полного профиля. VariantKey содержит эталонный ключ
	// варианта фиксированного источника и генератора.
	ProfileKey, VariantKey string
	// Session содержит эталонные байты снимка запуска с вложенным профилем. Output содержит
	// эталонные байты снимка выхода и manifest. Required содержит эталонные байты требуемого профиля
	// поколения.
	Session, Output, Required []byte
}

// Читает два закреплённых эталона прежних AV1/H.264-профилей и вложенных снимков для проверки
// совместимости.
func frozenLiveAudioProfiles(t *testing.T) []frozenLiveAudioProfile {
	t.Helper()
	// Эталоны получены исходным кодом a1c25b61fc981bdba42391ab7c51f61665bb3d97 в отдельном временном модуле.
	// data содержит байты закреплённых эталонов прежних профилей. err сообщает об ошибке чтения
	// закреплённого эталона с диска.
	data, err := os.ReadFile("testdata/live_audio_legacy_v2/profiles.json")
	if err != nil {
		t.Fatal(err)
	}
	// profiles принимает два закреплённых эталона прежних профилей.
	var profiles []frozenLiveAudioProfile
	if err = json.Unmarshal(data, &profiles); err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 2 {
		t.Fatalf("Нужны прежние AV1 и H.264: %d", len(profiles))
	}
	return profiles
}

// Проверяет прежнюю семантику LIVE-аудио без материализации селектора и точные байты и ключи профиля,
// снимков и RequiredProfile.
func TestLiveAudioLegacyProfilesAndNestedBytesRemainExact(t *testing.T) {
	// fixture содержит очередной эталон прежнего профиля и вложенных снимков.
	for _, fixture := range frozenLiveAudioProfiles(t) {
		t.Run(fixture.Name, func(t *testing.T) {
			// Проверяет для прежнего AV1 либо H.264 неизменность байтов, ключей и вложенных снимков при вычислении
			// эффективного LIVE-аудиорежима без материализации селектора.

			// p содержит профиль, для которого проверяется обработка LIVE-аудиоселектора.
			// err сообщает об ошибке чтения и проверки сохранённого профиля.
			p, err := core.DecodeProfile(fixture.Payload)
			if err != nil {
				t.Fatal(err)
			}
			// wantMode задаёт прежний эффективный LIVE-аудиорежим выбранного эталона.
			wantMode := core.LiveAudioCopy
			if fixture.Name == "h264" {
				wantMode = core.LiveAudioTranscode
			}
			if p.LiveAudioMode != "" || p.EffectiveLiveAudioMode() != wantMode {
				t.Fatalf("Изменена прежняя семантика: %+v", p)
			}
			if err = p.Validate(); err != nil || p.LiveAudioMode != "" {
				t.Fatalf("Validate материализовал селектор: %v", err)
			}
			// got содержит фактически закодированные байты для точного сравнения с эталоном.
			got, err := core.EncodeProfile(p)
			if err != nil || !bytes.Equal(got, fixture.Payload) {
				t.Fatalf("Изменены исходные байты v2: %v", err)
			}
			// key содержит вычисленный ключ полного профиля. canonical содержит вычисленные
			// канонические байты полного профиля.
			key, canonical, err := core.ProfileIdentity(p)
			if err != nil || key != fixture.ProfileKey || !bytes.Equal(canonical, fixture.Canonical) {
				t.Fatalf("Изменена прежняя идентичность: %s %v", key, err)
			}
			// variant содержит вычисленный ключ варианта фиксированного источника и
			// генератора.
			variant, err := core.VariantIdentity("frozen-source", p, "generator-v1")
			if err != nil || variant != fixture.VariantKey {
				t.Fatalf("Изменён ключ варианта: %s %v", variant, err)
			}
			// session содержит восстановленный начальный снимок запуска с прежним вложенным
			// профилем.
			session, err := core.DecodeSessionSnapshot(fixture.Session)
			if err != nil {
				t.Fatal(err)
			}
			got, err = core.EncodeSessionSnapshot(session)
			if err != nil || !bytes.Equal(got, fixture.Session) || session.Groups[0].Profile.Profile != p {
				t.Fatalf("Изменён вложенный Session: %v", err)
			}
			// output содержит восстановленный снимок выхода и manifest с прежним профилем.
			output, err := core.DecodeOutputSnapshot(fixture.Output)
			if err != nil {
				t.Fatal(err)
			}
			got, err = core.EncodeOutputSnapshot(output)
			if err != nil || !bytes.Equal(got, fixture.Output) || output.Artifact.Profile != p || output.Profile.Profile != p {
				t.Fatalf("Изменён Output/manifest: %v", err)
			}
			// required принимает восстановленный RequiredProfile с прежним вложенным
			// профилем.
			var required core.RequiredProfile
			if err = core.DecodeMetadata(fixture.Required, &required); err != nil {
				t.Fatal(err)
			}
			got, err = core.EncodeMetadata(required)
			if err != nil || !bytes.Equal(got, fixture.Required) || required.Profile != p {
				t.Fatalf("Изменён RequiredProfile: %v", err)
			}
		})
	}
}

// Проверяет самостоятельную версию профиля v3, сохранение явного режима LIVE-аудио и различие его
// идентичности при прежнем EncodingVersion.
func TestLiveAudioExplicitSelectionChangesIdentityAndUsesSeparateWireVersion(t *testing.T) {
	// fixture содержит очередной эталон прежнего профиля и вложенных снимков.
	for _, fixture := range frozenLiveAudioProfiles(t) {
		// legacy содержит прежний профиль с незаданным селектором LIVE-аудио. err сообщает об
		// ошибке чтения и проверки сохранённого профиля.
		legacy, err := core.DecodeProfile(fixture.Payload)
		if err != nil {
			t.Fatal(err)
		}
		// mode задаёт очередной явно выбранный LIVE-аудиорежим.
		for _, mode := range []core.LiveAudioMode{core.LiveAudioCopy, core.LiveAudioTranscode} {
			if legacy.Mode == core.ModeCopy && mode == core.LiveAudioTranscode {
				continue
			}
			// p содержит профиль, для которого проверяется обработка LIVE-аудиоселектора.
			p := legacy
			p.LiveAudioMode = mode
			// data содержит проверяемое сохраняемое представление профиля. err сообщает об
			// ошибке кодирования проверенного профиля.
			data, err := core.EncodeProfile(p)
			if err != nil {
				t.Fatal(err)
			}
			// root предоставляет поля JSON-профиля для проверки обязательности и версии.
			var root map[string]json.RawMessage
			if err = json.Unmarshal(data, &root); err != nil {
				t.Fatal(err)
			}
			if string(root["Version"]) != "3" || string(root["LiveAudioMode"]) != `"`+string(mode)+`"` || p.Encoding.Version != 2 || core.MetadataVersion != 1 {
				t.Fatalf("Смешаны независимые версии: %s", data)
			}
			// decoded содержит профиль после полного цикла сохранения и чтения.
			decoded, err := core.DecodeProfile(data)
			if err != nil || decoded != p {
				t.Fatalf("Селектор не пережил хранение: %+v %v", decoded, err)
			}
			// key содержит вычисленный ключ полного профиля.
			key, _, err := core.ProfileIdentity(p)
			if err != nil || key == fixture.ProfileKey || p.Equal(legacy) {
				t.Fatalf("Явный селектор не влияет на идентичность: %v", err)
			}
			// other содержит копию профиля с другим LIVE-аудиорежимом для сравнения
			// идентичности.
			other := p
			if mode == core.LiveAudioCopy && p.Mode == core.ModeTranscode {
				other.LiveAudioMode = core.LiveAudioTranscode
				// otherKey содержит ключ профиля с противоположным LIVE-аудиорежимом. e
				// сообщает об ошибке вычисления канонической идентичности профиля.
				otherKey, _, e := core.ProfileIdentity(other)
				if e != nil || otherKey == key || other.Equal(p) {
					t.Fatalf("copy/transcode получили общий ключ: %v", e)
				}
			}
		}
	}
}

// Проверяет строгие обязательные поля v3 и допустимость Encoding со значением null, отклоняя
// неизвестный селектор и режим LiveAudioTranscode при ModeCopy без частичной замены профиля.
func TestLiveAudioStoredExplicitProfileRejectsMissingNullAndUnknownFields(t *testing.T) {
	// p содержит профиль, для которого проверяется обработка LIVE-аудиоселектора.
	p := validProfile()
	p.LiveAudioMode = core.LiveAudioCopy
	// data содержит проверяемое сохраняемое представление профиля. err сообщает об ошибке
	// кодирования проверенного профиля.
	data, err := core.EncodeProfile(p)
	if err != nil {
		t.Fatal(err)
	}
	// decoded содержит профиль после полного цикла сохранения и чтения.
	decoded, err := core.DecodeProfile(data)
	if err != nil || decoded != p {
		t.Fatalf("Нулевой Encoding в v3 должен сохраняться: %v", err)
	}
	// field выбирает обязательное поле сохраняемого профиля для повреждения.
	for _, field := range []string{"Version", "Mode", "LiveAudioMode", "Format", "VideoEncoding", "AudioEncoding", "Encoding"} {
		// mutation выбирает удаление поля или замену его значения на null.
		for _, mutation := range []string{"missing", "null"} {
			if field == "Encoding" && mutation == "null" {
				continue
			}
			t.Run(field+"/"+mutation, func(t *testing.T) {
				// Удаляет выбранное поле v3 либо задаёт null и проверяет ErrInvalidMetadata без изменения приёмника;
				// допустимый null Encoding исключён из таблицы.

				// root предоставляет поля JSON-профиля для проверки обязательности и
				// версии.
				var root map[string]json.RawMessage
				// err сообщает об ошибке разбора JSON проверяемого представления.
				if err := json.Unmarshal(data, &root); err != nil {
					t.Fatal(err)
				}
				if mutation == "missing" {
					delete(root, field)
				} else {
					root[field] = json.RawMessage("null")
				}
				// bad содержит JSON-профиль с выбранным нарушением строгого
				// представления. err сообщает об ошибке кодирования проверяемого
				// JSON-представления.
				bad, err := json.Marshal(root)
				if err != nil {
					t.Fatal(err)
				}
				// receiver содержит исходный профиль для проверки отсутствия частичного
				// изменения при ошибке.
				receiver := p
				if err = receiver.UnmarshalJSON(bad); !errors.Is(err, core.ErrInvalidMetadata) || receiver != p {
					t.Fatalf("Частичное/нулевое поле принято или изменило приёмник: %v", err)
				}
			})
		}
	}
	// selector содержит очередное недопустимое JSON-значение LIVE-аудиоселектора.
	for _, selector := range []string{`""`, `"automatic"`, `true`, `2`} {
		// root предоставляет поля JSON-профиля для проверки обязательности и версии.
		var root map[string]json.RawMessage
		if err = json.Unmarshal(data, &root); err != nil {
			t.Fatal(err)
		}
		root["LiveAudioMode"] = json.RawMessage(selector)
		// bad содержит JSON-профиль с выбранным нарушением строгого представления. err сообщает
		// об ошибке кодирования проверяемого JSON-представления.
		bad, err := json.Marshal(root)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = core.DecodeProfile(bad); !errors.Is(err, core.ErrInvalidMetadata) {
			t.Fatalf("Недопустимый селектор %s принят: %v", selector, err)
		}
	}
	// unknown предоставляет JSON-поля профиля для добавления неизвестного поля.
	var unknown map[string]json.RawMessage
	if err = json.Unmarshal(data, &unknown); err != nil {
		t.Fatal(err)
	}
	unknown["Unexpected"] = json.RawMessage("true")
	// bad содержит JSON-профиль с выбранным нарушением строгого представления.
	bad, err := json.Marshal(unknown)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = core.DecodeProfile(bad); !errors.Is(err, core.ErrInvalidMetadata) {
		t.Fatalf("Неизвестное поле сохранённого v3 принято: %v", err)
	}
	p.LiveAudioMode = core.LiveAudioTranscode
	if err = p.Validate(); !errors.Is(err, core.ErrInvalidProfile) {
		t.Fatalf("Видео copy приняло audio transcode: %v", err)
	}
}
