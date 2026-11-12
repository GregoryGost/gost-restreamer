package api_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

// Проверяет документированные JSON-примеры полного профиля через настоящий маршрут создания и требует
// материализованный encoding в результате.
func TestDocumentedProfileExamplesUseRegisteredRequestContract(t *testing.T) {
	// data содержит документированное руководство клиента, из которого извлекаются JSON-примеры
	// профиля; err содержит отказ чтения.
	data, err := os.ReadFile("../../docs/obs-client-guide.md")
	if err != nil {
		t.Fatal(err)
	}
	// f содержит настоящий API, через который проверяются документированные запросы.
	f := newAPIAcceptanceFixture(t, nil)
	// count считает найденные примеры полного профиля, чтобы отсутствие примеров не давало ложного
	// успеха.
	count := 0
	// section содержит текст после очередного начала JSON-блока документации.
	for _, section := range strings.Split(string(data), "```json\n")[1:] {
		// body содержит содержимое текущего JSON-блока; ok подтверждает закрывающий маркер.
		body, _, ok := strings.Cut(section, "\n```")
		if !ok {
			t.Fatal("Пример JSON не завершён")
		}
		// request принимает объект примера для отбора запросов с profile.
		var request map[string]any
		if err = json.Unmarshal([]byte(body), &request); err != nil {
			t.Fatal(err)
		}
		if request["profile"] == nil {
			continue
		}
		count++
		// created содержит результат создания профиля по примеру для проверки материализованного
		// encoding.
		created := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", body, 201))
		if created["profile"].(map[string]any)["encoding"] == nil {
			t.Fatal("Пример не описывает новые настройки:", request["name"])
		}
	}
	if count == 0 {
		t.Fatal("Отсутствуют документированные примеры создания профилей")
	}
}
