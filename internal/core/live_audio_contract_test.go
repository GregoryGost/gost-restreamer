package core_test

import (
	"encoding/json"
	"errors"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// Проверяет все обязательные поля полного профиля, включая ненулевой Encoding, отклоняя
// неизвестный селектор и режим LiveAudioTranscode при ModeCopy без частичной замены профиля.
// t сообщает об ошибках проверяемого сценария.
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
		t.Fatalf("Полный Encoding должен сохраняться: %v", err)
	}
	// field выбирает обязательное поле сохраняемого профиля для повреждения.
	for _, field := range []string{"Mode", "LiveAudioMode", "Format", "VideoEncoding", "AudioEncoding", "Encoding"} {
		// mutation выбирает удаление поля или замену его значения на null.
		for _, mutation := range []string{"missing", "null"} {
			// t сообщает об ошибках проверяемого сценария.
			t.Run(field+"/"+mutation, func(t *testing.T) {
				// Удаляет выбранное поле профиля либо задаёт null и проверяет ErrInvalidMetadata без изменения приёмника;
				// каждый null, включая Encoding, запрещён.

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
		t.Fatalf("Неизвестное поле сохранённого профиля принято: %v", err)
	}
	p.LiveAudioMode = core.LiveAudioTranscode
	if err = p.Validate(); !errors.Is(err, core.ErrInvalidProfile) {
		t.Fatalf("Видео copy приняло audio transcode: %v", err)
	}
}
