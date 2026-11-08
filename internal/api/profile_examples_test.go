package api_test

import (
	"encoding/json"
	"os"
	"strings"
	"testing"
)

func TestDocumentedProfileExamplesUseRegisteredRequestContract(t *testing.T) {
	data, err := os.ReadFile("../../docs/obs-client-guide.md")
	if err != nil {
		t.Fatal(err)
	}
	f := newAPIAcceptanceFixture(t, nil)
	count := 0
	for _, section := range strings.Split(string(data), "```json\n")[1:] {
		body, _, ok := strings.Cut(section, "\n```")
		if !ok {
			t.Fatal("Пример JSON не завершён")
		}
		var request map[string]any
		if err = json.Unmarshal([]byte(body), &request); err != nil {
			t.Fatal(err)
		}
		if request["profile"] == nil {
			continue
		}
		count++
		created := apiAcceptanceJSON(t, apiAcceptanceRequest(t, f.api, "POST", "/transcode-profiles", body, 201))
		if created["profile"].(map[string]any)["encoding"] == nil {
			t.Fatal("Пример не описывает новые настройки:", request["name"])
		}
	}
	if count == 0 {
		t.Fatal("Отсутствуют документированные примеры создания профилей")
	}
}
