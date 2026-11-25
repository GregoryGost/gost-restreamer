package api_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// TestChatWebServesEmbeddedPagesAndAssets проверяет поставку обеих страниц, их пределы
// окна сообщений, публичные модули и заголовки браузерных ресурсов.
func TestChatWebServesEmbeddedPagesAndAssets(t *testing.T) {
	// limits задаёт разные значения для подтверждения связи разметки с серверной конфигурацией.
	limits := core.DefaultChatLimits()
	limits.OverlayMessages, limits.DashboardMessages = 37, 149
	// handler обслуживает зарегистрированные страницы и ресурсы из бинарника.
	_, handler, _ := newChatStreamSelfFixture(t, limits)
	// item связывает точный URL ресурса с MIME-типом и значимым содержимым.
	for _, item := range []struct {
		// path содержит не зависящий от префикса API адрес страницы или модуля.
		path string
		// mime задаёт формат, по которому браузер принимает ресурс.
		mime string
		// body содержит обязательный литерал текущей страницы или её ресурса.
		body string
	}{
		{path: "/chat/dashboard?stream_id=synthetic", mime: "text/html", body: `data-message-limit="149"`},
		{path: "/chat/overlay?stream_id=synthetic", mime: "text/html", body: `data-message-limit="37"`},
		{path: "/chat/assets/dashboard.js", mime: "text/javascript", body: "showDashboard()"},
		{path: "/chat/assets/overlay.js", mime: "text/javascript", body: "ChatStream"},
		{path: "/chat/assets/chat.css", mime: "text/css", body: ".overlay"},
	} {
		// request читает встроенный ресурс, response сохраняет его тело и заголовки.
		request := httptest.NewRequestWithContext(t.Context(), http.MethodGet, item.path, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusOK || !strings.HasPrefix(response.Header().Get("Content-Type"), item.mime) ||
			!strings.Contains(response.Body.String(), item.body) || response.Header().Get("X-Content-Type-Options") != "nosniff" {
			t.Fatalf("Неверный встроенный ресурс %s: статус=%d, заголовки=%v", item.path, response.Code, response.Header())
		}
	}
}

// selectableChatFeed подставляет несколько платформенных эфиров и использует тот же порт
// подтверждённого чтения, что и обычная проверка живого потока.
type selectableChatFeed struct {
	// acceptanceChatFeed сообщает о фактическом канале, переданном читателю после выбора.
	acceptanceChatFeed
}

// Discover возвращает два допустимых эфира. Их внутренние идентификаторы бесед нужны читателю.
func (*selectableChatFeed) Discover(context.Context, core.ChatSource, core.ChatCredentials) (core.ChatDiscovery, error) {
	return core.ChatDiscovery{Broadcasts: []core.ChatBroadcast{
		{VideoID: "first-video", ChannelID: "internal-first-chat", Title: "Первый эфир"},
		{VideoID: "selected-video", ChannelID: "internal-selected-chat", Title: "Второй эфир"},
	}}, nil
}

// TestChatBroadcastSelectionAcceptsCurrentPublicChoice проверяет чтение и принятие актуального
// набора через публичный API и привязку читателя к выбранному эфиру.
func TestChatBroadcastSelectionAcceptsCurrentPublicChoice(t *testing.T) {
	// ctx ограничивает подготовку, опрос GET и ожидание читателя. cancel освобождает таймер.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// fixture размещает тестовый поток и конфигурацию в изолированной SQLite.
	fixture := newAPIAcceptanceFixture(t, nil)
	// stream, profile, group и destination образуют реальные доказательства публикации для Manager.
	stream, profile, group, destination := apiAcceptanceSeedConfiguration(t, fixture)
	// account хранит подтверждённый синтетический ключ, err относится к записи аккаунта.
	account, err := fixture.service.CreateChatAccount(ctx, core.ChatAccount{Provider: core.ChatYouTube},
		core.ChatCredentialBundle{YouTube: &core.YouTubeChatCredentials{APIKey: "synthetic-selection-key"}}, "selection-initial")
	if err != nil {
		t.Fatal(err)
	}
	// source связывает локальное назначение с синтетическим обнаружением, err сообщает об отказе создания.
	source, err := fixture.service.CreateChatSource(ctx, core.ChatSource{
		DestinationID: destination.ID, AccountID: account.Account.ID, Enabled: true,
		Provider: core.ChatYouTube, ChannelHint: "UCsynthetic",
		Options: core.ChatOptions{Version: 1, YouTube: &core.YouTubeChatOptions{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// previousRevision сохраняет устаревшую ревизию для отдельной проверки CAS.
	previousRevision := source.Revision
	// Обновление источника до обнаружения разводит значения его ревизии и поколения набора.
	source.Name = "Источник с обновлённой ревизией"
	source, err = fixture.service.UpdateChatSource(ctx, source, previousRevision)
	if err != nil {
		t.Fatal(err)
	}
	// feed передаёт подтверждённое чтение после явного выбора одного видео.
	feed := &selectableChatFeed{acceptanceChatFeed{ready: make(chan acceptanceChatReader, 1)}}
	// handler принимает публичные запросы и разрешает их в точную текущую Session.
	handler, _ := newChatAcceptanceHandler(t, fixture, core.DefaultChatLimits(), []core.ChatAdapterPort{feed})
	// sessionID указывает на действующий запуск с фактическим событием PUBLISHING.
	sessionID, _ := seedChatStreamSelfSession(t, fixture, stream)
	acceptanceChatSeedOutput(t, fixture, sessionID, stream, profile, group, destination)
	// path адресует выбор одного источника, query указывает на текущую Session.
	path := "/chat-sources/" + string(source.ID) + "/broadcast-selection"
	query := path + "?session_id=" + string(sessionID)
	// tick ограничивает частоту ожидания публикации найденных вариантов.
	tick := time.NewTicker(10 * time.Millisecond)
	defer tick.Stop()
	// choices принимает публичный набор и его удостоверения после завершения обнаружения.
	var choices map[string]any
	for choices == nil {
		// request пробует прочитать текущий набор, response фиксирует состояние этого чтения.
		request := httptest.NewRequestWithContext(ctx, http.MethodGet, "/api/v1"+query, nil)
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		switch response.Code {
		case http.StatusOK:
			choices = numericJSON(t, response.Body.Bytes())
			// Значения пяти публичных полей проверяются ниже. Любые дополнительные поля запрещены.
			if len(choices) != 5 {
				t.Fatalf("Неверный состав полей набора эфиров: %v", choices)
			}
		case http.StatusConflict:
			select {
			case <-tick.C:
			case <-ctx.Done():
				t.Fatal("HTTP не дождался текущего набора эфиров", ctx.Err())
			}
		default:
			t.Fatalf("Чтение вариантов: статус=%d, тело=%s", response.Code, response.Body)
		}
	}
	// broadcasts содержит открытые элементы набора. ok подтверждает наличие массива.
	broadcasts, ok := choices["broadcasts"].([]any)
	if !ok || len(broadcasts) != 2 || choices["session_id"] != string(sessionID) || choices["source_id"] != string(source.ID) {
		t.Fatalf("Неверные открытые варианты эфира: %v", choices)
	}
	// expectedTitles перечисляет оба допустимых варианта. Каждый проверенный вариант удаляется из списка.
	expectedTitles := map[string]string{"first-video": "Первый эфир", "selected-video": "Второй эфир"}
	// videoID сохраняет полученный идентификатор эфира, который отправит клиент.
	var videoID string
	// item содержит отдельный вариант независимо от позиции в ответе.
	for _, item := range broadcasts {
		// candidate раскрывает поля варианта. ok подтверждает, что вариант является объектом.
		candidate, ok := item.(map[string]any)
		// В фикстуре время начала неизвестно. Публичный вариант содержит идентификатор и название.
		if !ok || len(candidate) != 2 {
			t.Fatalf("Неверный состав полей варианта эфира: %v", item)
		}
		// id содержит публичный идентификатор. ok подтверждает его строковый формат.
		id, ok := candidate["video_id"].(string)
		// title задаёт ожидаемое название и остаётся пустым для неизвестного или повторного идентификатора.
		title := expectedTitles[id]
		if !ok || title == "" || candidate["title"] != title {
			t.Fatalf("Неизвестный вариант эфира, повтор или неверное название: %v", candidate)
		}
		delete(expectedTitles, id)
		if id == "selected-video" {
			videoID = id
		}
	}
	// revision сохраняет исходную лексему ревизии. revisionIsNumber подтверждает число в JSON.
	revision, revisionIsNumber := choices["source_revision"].(json.Number)
	// generation сохраняет исходную лексему поколения. generationIsNumber подтверждает число в JSON.
	generation, generationIsNumber := choices["discovery_generation"].(json.Number)
	// generationValue проверяет диапазон поколения. err сообщает о недопустимой лексеме.
	generationValue, err := strconv.ParseUint(generation.String(), 10, 64)
	if !revisionIsNumber || revision.String() != strconv.FormatUint(source.Revision, 10) ||
		!generationIsNumber || err != nil || generationValue == 0 || generationValue == source.Revision {
		t.Fatalf("Неверная ревизия источника или поколение набора: %v", choices)
	}
	// selectionFormat сохраняет проверенные числовые лексемы при построении тела POST.
	const selectionFormat = `{"session_id":%q,"expected_revision":%s,"discovery_generation":%s,"video_id":%q}`
	apiAcceptanceRequest(t, handler, http.MethodPost, path,
		fmt.Sprintf(selectionFormat, sessionID, revision, generation, "missing-video"), http.StatusConflict)
	apiAcceptanceRequest(t, handler, http.MethodPost, path,
		fmt.Sprintf(selectionFormat, sessionID, strconv.FormatUint(previousRevision, 10), generation, videoID), http.StatusConflict)
	apiAcceptanceRequest(t, handler, http.MethodPost, path,
		fmt.Sprintf(selectionFormat, sessionID, revision, revision, videoID), http.StatusConflict)
	// selectedBody передаёт действительный выбор и позволяет повторить точно ту же команду.
	selectedBody := fmt.Sprintf(selectionFormat, sessionID, revision, generation, videoID)
	// accepted содержит ответ успешного выбора, который по контракту не имеет тела.
	accepted := apiAcceptanceRequest(t, handler, http.MethodPost, path, selectedBody, http.StatusNoContent)
	if accepted.Body.Len() != 0 {
		t.Fatalf("Успешный выбор вернул неожиданное тело: %s", accepted.Body)
	}
	select {
	case reader := <-feed.ready:
		// reader предоставляет порт принятого канала. message принимается только для выбранной беседы.
		message := core.ChatIncomingMessage{
			Provider: core.ChatYouTube, ChannelID: "internal-selected-chat", ProviderMessageID: "selected-message",
			Kind: core.ChatMessageText, Text: "Сообщение выбранного эфира", Author: core.ChatAuthor{DisplayName: "Зритель"},
		}
		// wrongChannel подставляет беседу первого эфира для проверки ограничения читателя.
		wrongChannel := message
		wrongChannel.ChannelID = "internal-first-chat"
		// err содержит результат попытки передать сообщение из другой беседы.
		if err := reader.sink.Message(ctx, reader.token, wrongChannel); !errors.Is(err, core.ErrInvalid) {
			t.Fatalf("Читатель должен отклонять сообщения другого эфира: %v", err)
		}
		// err сообщает об отказе приёма сообщения после выбора.
		if err := reader.sink.Message(ctx, reader.token, message); err != nil {
			t.Fatalf("Читатель не принимает сообщения выбранного видео: %v", err)
		}
	case <-ctx.Done():
		t.Fatal("После выбора не открылся подтверждённый читатель", ctx.Err())
	}
	apiAcceptanceRequest(t, handler, http.MethodPost, path, selectedBody, http.StatusConflict)
}
