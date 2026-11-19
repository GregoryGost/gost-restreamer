package core

import (
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"strings"
	"testing"
	"time"
)

// TestChatOptionsAcceptOnlyClosedProviderVariant проверяет явную версию и закрытые варианты.
// t получает отказ, если неверный JSON меняет ранее допустимое значение или получает defaults.
func TestChatOptionsAcceptOnlyClosedProviderVariant(t *testing.T) {
	// provider обозначает каждую из трёх платформ, доступных без сетевого адаптера.
	for _, provider := range []ChatProvider{ChatYouTube, ChatTwitch, ChatVKVideoLive} {
		// options содержит явный вариант; err сообщает отказ его создания.
		options, err := NewChatOptions(provider)
		if err != nil || options.Validate(provider) != nil || options.Version != 1 {
			t.Fatalf("Допустимый вариант %q отклонён: %+v, %v", provider, options, err)
		}
		// data сохраняет вариант для проверки полного обратного преобразования.
		data, err := json.Marshal(options)
		if err != nil {
			t.Fatal(err)
		}
		// restored принимает вариант без скрытого выбора платформы.
		var restored ChatOptions
		if err = json.Unmarshal(data, &restored); err != nil || !reflect.DeepEqual(options, restored) {
			t.Fatalf("Изменились настройки %q: %v", provider, err)
		}
	}
	// invalid задаёт неизвестные, отсутствующие, неоднозначные и устаревшие формы.
	for _, invalid := range []string{
		`null`, `{}`, `{"version":1}`, `{"version":0,"youtube":{}}`,
		`{"version":2,"youtube":{}}`, `{"version":1,"youtube":null}`,
		`{"version":1,"youtube":{},"twitch":{}}`,
		`{"version":1,"youtube":{},"twitch":null}`,
		`{"version":1,"youtube":{"video_id":"temporary-video"}}`,
		`{"version":1,"youtube":{},"extra":true}`, `{"version":1,"other":{}}`,
		`{"version":1,"youtube":{}} {}`, `{"version":"1","youtube":{}}`,
	} {
		// original и decoded позволяют проверить неизменность получателя при отказе.
		original := ChatOptions{Version: 1, Twitch: &TwitchChatOptions{}}
		decoded := original
		// err содержит ожидаемый отказ строгого разбора.
		if err := json.Unmarshal([]byte(invalid), &decoded); err == nil || !reflect.DeepEqual(original, decoded) {
			t.Fatalf("Неверная форма принята или изменила получателя: %s, %v", invalid, err)
		}
	}
	// err подтверждает отказ неизвестного провайдера без назначения варианта по умолчанию.
	if _, err := NewChatOptions("other"); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Неизвестный провайдер получил defaults: %v", err)
	}
	if (ChatOptions{Version: 1, Twitch: &TwitchChatOptions{}}).Validate(ChatYouTube) == nil {
		t.Fatal("Вариант другой платформы принят")
	}
}

// TestChatSourceChannelFormsAndUTF8Budgets проверяет форму постоянного канала и байтовые пределы.
// t получает ошибки валидации; данные не должны скрыто обрезаться или нормализоваться.
func TestChatSourceChannelFormsAndUTF8Budgets(t *testing.T) {
	// provider выбирает платформу; hints задаёт допустимые постоянные ссылки без сетевой проверки.
	for provider, hints := range map[ChatProvider][]string{
		ChatYouTube:     {"UCchannel", "UC" + strings.Repeat("я", 127)},
		ChatTwitch:      {"", "000123", strings.Repeat("9", 256)},
		ChatVKVideoLive: {"канал", "channel-with.dots"},
	} {
		// options содержит явный вариант для выбранной платформы; err сообщает отказ конструктора.
		options, err := NewChatOptions(provider)
		if err != nil {
			t.Fatal(err)
		}
		// hint содержит допустимый канал, source — проверяемую конфигурацию без назначенной Record.
		for _, hint := range hints {
			source := ChatSource{DestinationID: "destination", AccountID: "account", Provider: provider, ChannelHint: hint, Options: options, Name: strings.Repeat("я", 256)}
			if err = source.Validate(); err != nil {
				t.Fatalf("Допустимый канал %q/%q отклонён: %v", provider, hint, err)
			}
			// original сохраняет все байты, включая отсутствие Enabled.
			original := source
			if err = source.Validate(); err != nil || !reflect.DeepEqual(original, source) {
				t.Fatalf("Validate изменил конфигурацию: %v", err)
			}
		}
	}
	// source задаёт допустимую базу для независимых нарушений полей.
	source := ChatSource{DestinationID: "destination", AccountID: "account", Provider: ChatYouTube, ChannelHint: "UCchannel", Options: ChatOptions{Version: 1, YouTube: &YouTubeChatOptions{}}}
	// change вносит одно нарушение; каждый callback меняет переданную ему локальную копию s.
	for _, change := range []func(*ChatSource){
		func(s *ChatSource) { s.DestinationID = "" },
		func(s *ChatSource) { s.AccountID = "\x00account" },
		func(s *ChatSource) { s.AccountID = ID(strings.Repeat("я", 129)) },
		func(s *ChatSource) { s.Name = strings.Repeat("я", 257) },
		func(s *ChatSource) { s.Name = string([]byte{0xff}) },
		func(s *ChatSource) { s.ChannelHint = "UC" },
		func(s *ChatSource) { s.ChannelHint = "video-id" },
		func(s *ChatSource) { s.ChannelHint = "UC" + strings.Repeat("x", 255) },
		func(s *ChatSource) { s.Options.Version = 2 },
		func(s *ChatSource) { s.Options.Twitch = &TwitchChatOptions{} },
	} {
		// invalid получает ровно одно нарушение относительно независимой допустимой базы.
		invalid := source
		change(&invalid)
		// err должен сохранить доменную категорию недопустимого ввода.
		if err := invalid.Validate(); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Неверный Source принят: %+v, %v", invalid, err)
		}
	}
	// provider задаёт другую платформу, hints — набор строк, которые нарушают её форму.
	for provider, hints := range map[ChatProvider][]string{ChatTwitch: {"-1", "12a", "１２"}, ChatVKVideoLive: {"", "https://live.vkvideo.ru/channel", "path/channel", "channel?x=1"}} {
		// options и err задают согласованный вариант для отрицательных проверок канала.
		options, err := NewChatOptions(provider)
		if err != nil {
			t.Fatal(err)
		}
		// hint содержит очередную строку, не представляющую постоянный канал платформы.
		for _, hint := range hints {
			source.Provider, source.Options, source.ChannelHint = provider, options, hint
			if err = source.Validate(); !errors.Is(err, ErrInvalid) {
				t.Fatalf("Неверный канал %q/%q принят: %v", provider, hint, err)
			}
		}
	}
}

// acceptanceTwitchBundle создаёт полный синтетический набор с уже истёкшим сроком.
// access и refresh сохраняются без нормализации; истечение само по себе не повреждает форму.
func acceptanceTwitchBundle(access, refresh string) ChatCredentialBundle {
	return ChatCredentialBundle{Twitch: &TwitchChatCredentials{ClientID: "application", UserID: "123", AccessToken: access, RefreshToken: refresh, Scopes: []string{"user:read:chat", "extra:scope"}, ExpiresAt: time.Date(2000, 1, 1, 0, 0, 0, 123000, time.UTC)}}
}

// TestChatCredentialsKeepCompletePairsAndExactValues проверяет обязательные поля и полный бюджет.
// t различает допустимые истёкшие данные, неполный обмен и некорректные времена или scopes.
func TestChatCredentialsKeepCompletePairsAndExactValues(t *testing.T) {
	// bundle сохраняет пробелы токенов и порядок scopes; original нужен для проверки чистоты Validate.
	bundle := acceptanceTwitchBundle(" access ", " refresh ")
	original := acceptanceTwitchBundle(" access ", " refresh ")
	if bundle.Validate(ChatTwitch) != nil || !reflect.DeepEqual(bundle, original) {
		t.Fatal("Допустимые полномочия изменены или истечение ошибочно признано повреждением")
	}
	// change вносит одно нарушение полного набора; каждый callback меняет переданную ему копию b.
	for _, change := range []func(*ChatCredentialBundle){
		func(b *ChatCredentialBundle) { b.Twitch.AccessToken = "" },
		func(b *ChatCredentialBundle) { b.Twitch.RefreshToken = "" },
		func(b *ChatCredentialBundle) { b.Twitch.ClientID = "" },
		func(b *ChatCredentialBundle) { b.Twitch.UserID = "12x" },
		func(b *ChatCredentialBundle) { b.Twitch.Scopes = []string{"user:write:chat"} },
		func(b *ChatCredentialBundle) { b.Twitch.Scopes = []string{"user:read:chat", "\x00"} },
		func(b *ChatCredentialBundle) { b.Twitch.ExpiresAt = time.Time{} },
		func(b *ChatCredentialBundle) { b.Twitch.ExpiresAt = b.Twitch.ExpiresAt.Add(time.Nanosecond) },
		func(b *ChatCredentialBundle) {
			b.Twitch.ExpiresAt = b.Twitch.ExpiresAt.In(time.FixedZone("offset", 3600))
		},
		func(b *ChatCredentialBundle) { b.YouTube = &YouTubeChatCredentials{APIKey: "key"} },
		func(b *ChatCredentialBundle) { b.Twitch.AccessToken = string([]byte{0xff}) },
		func(b *ChatCredentialBundle) { b.Twitch.AccessToken = strings.Repeat("<", 12000) },
	} {
		// invalid содержит отдельный набор, чтобы мутации указателя не загрязняли другие сценарии.
		invalid := acceptanceTwitchBundle("access", "refresh")
		change(&invalid)
		// err подтверждает структурный отказ независимо от текущих часов.
		if err := invalid.Validate(ChatTwitch); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Неполные или непредставимые полномочия приняты: %v", err)
		}
	}
	// provider выбирает второй тип полномочий; valid содержит полный допустимый набор.
	for provider, valid := range map[ChatProvider]ChatCredentialBundle{
		ChatYouTube:     {YouTube: &YouTubeChatCredentials{APIKey: " key "}},
		ChatVKVideoLive: {VKVideoLive: &VKVideoLiveChatCredentials{ClientID: "application", ClientSecret: " secret ", AccessToken: "token", ExpiresAt: time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)}},
	} {
		if valid.Validate(provider) != nil || valid.Validate(ChatTwitch) == nil {
			t.Fatalf("Нарушено соответствие полного набора платформе %q", provider)
		}
	}
}

// TestChatQueriesValidateBoundsWithoutBroadeningFilters проверяет конечные страницы и ссылки.
// t получает ошибки для нулевого размера, неверного провайдера и повреждённой границы.
func TestChatQueriesValidateBoundsWithoutBroadeningFilters(t *testing.T) {
	// limit проверяет оба допустимых края страницы и ближайшие недопустимые значения.
	for _, limit := range []int{-1, 0, 1, 500, 501} {
		// valid задаётся контрактным диапазоном страницы, а не реализацией запроса.
		valid := limit == 1 || limit == 500
		if ((ChatAccountQuery{Limit: limit}).Validate() == nil) != valid || ((ChatSourceQuery{Limit: limit}).Validate() == nil) != valid {
			t.Fatalf("Нарушен диапазон страницы: %d", limit)
		}
	}
	// query содержит каждую независимо неверную границу или фильтр источников.
	for _, query := range []ChatSourceQuery{
		{Limit: 1, Provider: "other"}, {Limit: 1, AccountID: "\x00"},
		{Limit: 1, DestinationID: ID(strings.Repeat("x", 257))}, {Limit: 1, StreamID: "\x00"},
		{Limit: 1, After: &ListBoundary{ID: "", CreatedAt: time.Now().UTC()}},
	} {
		if query.Validate() == nil {
			t.Fatalf("Неверный запрос принят: %+v", query)
		}
	}
}

// acceptanceIncomingChatMessage создаёт публичное сообщение с Unicode и безопасными HTTPS-ресурсами.
// Текст сохраняет HTML-подобные байты; интерпретация и окончательное JSON-ограничение принадлежат UI/Hub.
func acceptanceIncomingChatMessage() ChatIncomingMessage {
	return ChatIncomingMessage{Provider: ChatYouTube, ChannelID: "actual-chat", ProviderMessageID: "message", Kind: ChatMessageText, Text: "Привет 🙂 <script>alert(1)</script>", Author: ChatAuthor{ID: "author", DisplayName: "Имя", AvatarURL: "https://example.invalid/avatar"}, Parts: []ChatPart{{Kind: ChatPartText, Text: "Привет 🙂", URL: "https://example.invalid/link"}, {Kind: ChatPartEmote, Text: ":emote:", EmoteID: "emote", ImageURL: "https://example.invalid/emote"}}, Badges: []ChatBadge{{ID: "badge", Name: "Модератор", ImageURL: "https://example.invalid/badge"}}}
}

// TestChatMessageStructureRejectsUnsafeLinksAndUnboundedFields проверяет нормализованную форму.
// t получает отрицательные проверки без требования уже работающего Hub или браузера.
func TestChatMessageStructureRejectsUnsafeLinksAndUnboundedFields(t *testing.T) {
	// incoming содержит допустимые публичные данные; text фиксирует исходные байты до Validate.
	incoming := acceptanceIncomingChatMessage()
	text := incoming.Text
	if incoming.Validate() != nil || incoming.Text != text {
		t.Fatal("Допустимое сообщение отклонено или нормализовано")
	}
	// change получает новую независимую модель m и вносит одно нарушение контракта.
	for _, change := range []func(*ChatIncomingMessage){
		func(m *ChatIncomingMessage) { m.Provider = "unknown" },
		func(m *ChatIncomingMessage) { m.ChannelID = "" },
		func(m *ChatIncomingMessage) { m.Kind = "html" },
		func(m *ChatIncomingMessage) { m.Text = string([]byte{0xff}) },
		func(m *ChatIncomingMessage) { m.Author.AvatarURL = "http://example.invalid/avatar" },
		func(m *ChatIncomingMessage) { m.Author.AvatarURL = "https:///missing-host" },
		func(m *ChatIncomingMessage) { m.Parts[0].URL = "javascript:alert(1)" },
		func(m *ChatIncomingMessage) { m.Parts[1].ImageURL = "data:image/png;base64,AQ==" },
		func(m *ChatIncomingMessage) { m.Parts[0].Kind = "raw_html" },
		func(m *ChatIncomingMessage) { m.Parts[0].ImageURL = "https://example.invalid/image" },
		func(m *ChatIncomingMessage) { m.Badges[0].URL = "file:///local" },
		func(m *ChatIncomingMessage) { m.Parts = make([]ChatPart, 257) },
		func(m *ChatIncomingMessage) { m.Badges = make([]ChatBadge, 65) },
		func(m *ChatIncomingMessage) { m.Text = strings.Repeat("x", 65536) },
		func(m *ChatIncomingMessage) { m.Author.DisplayName = strings.Repeat("я", 257) },
	} {
		// invalid содержит собственные part/badge срезы, чтобы сценарии не влияли друг на друга.
		invalid := acceptanceIncomingChatMessage()
		change(&invalid)
		// err должен сохранить доменную категорию повреждённого нормализованного ввода.
		if err := invalid.Validate(); !errors.Is(err, ErrInvalid) {
			t.Fatalf("Неверное сообщение принято: %v", err)
		}
	}
	// message содержит доверенные поля Hub с точным seq выше 2^53.
	message := ChatMessage{ChatIncomingMessage: incoming, SessionID: "session", ChatSourceID: "source", Seq: 9007199254740993, ReceivedAt: time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)}
	if message.Validate() != nil {
		t.Fatal("Допустимое серверное сообщение отклонено")
	}
	message.Seq = 0
	if message.Validate() == nil {
		t.Fatal("Сообщение без серверной последовательности принято")
	}
}

// TestChatLogicalWeightsGrowWithUTF8AndDetectOverflow проверяет независимые свойства учёта памяти.
// t использует договорённые единицы, проверяет Unicode в байтах и запрет переполнения.
func TestChatLogicalWeightsGrowWithUTF8AndDetectOverflow(t *testing.T) {
	// message задаёт минимальный набор удерживаемых строк, base и err — его исходный вес.
	message := ChatIncomingMessage{Provider: ChatYouTube, ChannelID: "chat", Kind: ChatMessageText}
	base, err := message.LogicalBytes()
	if err != nil || base < 256 {
		t.Fatalf("Не учтён базовый вес записи: %d, %v", base, err)
	}
	message.Text = "я🙂"
	// withText обязан увеличиться ровно на шесть UTF-8 байтов, а не на два символа.
	withText, err := message.LogicalBytes()
	if err != nil || withText-base != 6 {
		t.Fatalf("Вес текста считался символами: %d, %v", withText-base, err)
	}
	message.Parts = []ChatPart{{Kind: ChatPartText, Text: "hello"}}
	// withPart проверяет накладные 64 байта part и его девять удерживаемых байтов.
	withPart, err := message.LogicalBytes()
	if err != nil || withPart-withText != 73 {
		t.Fatalf("Не учтены поля фрагмента: %d, %v", withPart-withText, err)
	}
	// sum допускает точный MaxUint64; err должен отклонить только следующее сложение.
	sum, err := CheckedChatBytes(math.MaxUint64-1, 1)
	if err != nil || sum != math.MaxUint64 {
		t.Fatalf("Допустимая граница суммы отклонена: %d, %v", sum, err)
	}
	if _, err = CheckedChatBytes(math.MaxUint64, 1); !errors.Is(err, ErrInvalid) {
		t.Fatalf("Переполнение веса не отклонено: %v", err)
	}
}

// TestChatLimitsAreExplicitFiniteAndRejectContradictions проверяет согласованные исходные бюджеты.
// t проверяет отсутствие defaults для неполной конфигурации и основные отношения ресурсов.
func TestChatLimitsAreExplicitFiniteAndRejectContradictions(t *testing.T) {
	// limits содержит полный набор исходных инженерных пределов из утверждённого контракта.
	limits := DefaultChatLimits()
	if limits.Validate() != nil || limits.Sessions != 4 || limits.Sources != 64 || limits.SourcesPerSession != 32 || limits.Readers != 32 || limits.Connections != 64 || limits.History != (ChatBudget{Count: 1000, Bytes: 16 << 20}) || limits.Replay != (ChatBudget{Count: 2048, Bytes: 16 << 20}) || limits.MessageBytes != 64<<10 || limits.CredentialBytes != 64<<10 || limits.SnapshotBytes != 20<<20 || limits.Snapshots != 4 || limits.FlushInterval != 50*time.Millisecond || limits.ReconnectAttempts != 8 || limits.OverlayMessages != 50 || limits.DashboardMessages != 200 {
		t.Fatal("Явные исходные пределы не совпадают с контрактом")
	}
	// index адресует каждое обязательное поле верхнего уровня; reflection нужен для полноты zero-проверки.
	for index := 0; index < reflect.TypeFor[ChatLimits]().NumField(); index++ {
		// invalid получает ровно одно обнулённое поле, field позволяет задать его без перечисления формул.
		invalid := limits
		field := reflect.ValueOf(&invalid).Elem().Field(index)
		field.SetZero()
		if invalid.Validate() == nil {
			t.Fatalf("Неполный бюджет принят: %s", reflect.TypeFor[ChatLimits]().Field(index).Name)
		}
	}
	// change меняет переданную копию l: создаёт противоречие пределов либо отрицательный срок.
	for _, change := range []func(*ChatLimits){
		func(l *ChatLimits) { l.SourcesPerSession = 65 },
		func(l *ChatLimits) { l.Connections = 31 },
		func(l *ChatLimits) { l.SSEClientsPerStream = 33 },
		func(l *ChatLimits) { l.SnapshotMetadataBytes = 21 << 20 },
		func(l *ChatLimits) { l.ReconnectMinBackoff = 2 * time.Minute },
		func(l *ChatLimits) { l.ReplacementsPerAccount = 2 },
		func(l *ChatLimits) { l.ReconnectConcurrent = 2 },
		func(l *ChatLimits) { l.HTTPTimeout = -time.Second },
		func(l *ChatLimits) { l.History.Count = -1 },
		func(l *ChatLimits) { l.InputQueue.Bytes = 0 },
	} {
		// invalid содержит одно нарушение без изменения общего эталона limits.
		invalid := limits
		change(&invalid)
		if invalid.Validate() == nil {
			t.Fatal("Противоречивые пределы приняты")
		}
	}
}

// TestChatStatusAndEventVariantsKeepMessagesSeparate проверяет закрытые безопасные причины и union.
// t различает состояние источника, сообщение и сигнал потери без требования работающей SSE-выдачи.
func TestChatStatusAndEventVariantsKeepMessagesSeparate(t *testing.T) {
	// now задаёт точную UTC-метку; status представляет отдельное состояние ожидающего источника.
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	status := ChatSourceStatus{SourceID: "source", State: ChatWaitingOutput, ChangedAt: now}
	// event передаёт статус отдельно от ленты, затем используется для отрицательных union-проверок.
	event := ChatEvent{Kind: ChatEventSourceStatus, Cursor: "opaque", Status: &status}
	if event.Validate() != nil {
		t.Fatal("Отдельный статус до Session отклонён")
	}
	status.Reason = "raw upstream token failure"
	if event.Validate() == nil {
		t.Fatal("Произвольная диагностическая причина принята как публичный код")
	}
	status.Reason = ChatReasonNone
	event.Kind = ChatEventMessage
	if event.Validate() == nil {
		t.Fatal("Состояние источника принято как сообщение аудитории")
	}
	event = ChatEvent{Kind: ChatEventGap, Cursor: "opaque", SessionID: "session", Lost: math.MaxUint64, Reason: ChatReasonUpstreamGap}
	if event.Validate() != nil {
		t.Fatal("Явная потеря либо точный uint64 отклонены")
	}
	event.Status = &status
	if event.Validate() == nil {
		t.Fatal("Неоднозначный вариант gap/status принят")
	}
	event = ChatEvent{Kind: ChatEventResync, Cursor: ChatCursor(strings.Repeat("x", 4097))}
	if event.Validate() == nil {
		t.Fatal("Cursor превысил конечный предел")
	}
}
