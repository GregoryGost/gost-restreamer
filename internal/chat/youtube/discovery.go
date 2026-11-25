package youtube

import (
	"context"
	"net/url"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

const (
	// searchPageLimit ограничивает число страниц одного полного обнаружения.
	searchPageLimit = 4
	// searchPageSize задаёт максимальное число видео на странице поиска.
	searchPageSize = 50
	// videoBatchSize ограничивает размер запроса videos.list по идентификаторам.
	videoBatchSize = 50
	// broadcastLimit соответствует числу вариантов, принимаемых Manager.
	broadcastLimit = 64
	// searchTokenBytes ограничивает непрозрачный токен страницы до следующего запроса.
	searchTokenBytes = 4 << 10
)

// searchItem содержит только идентификатор, который необходимо перепроверить через videos.list.
type searchItem struct {
	// ID содержит запрошенную ссылку на видео.
	ID struct {
		// VideoID содержит исходный идентификатор из поискового индекса.
		VideoID string `json:"videoId"`
	} `json:"id"`
}

// videoItem содержит сведения, необходимые для проверки доступности публичного эфира.
type videoItem struct {
	// ID связывает сведения videos.list с одним из запрошенных видео.
	ID string `json:"id"`
	// Snippet содержит публичные сведения о канале и названии видео.
	Snippet struct {
		// ChannelID задаёт канал, опубликовавший видео.
		ChannelID string `json:"channelId"`
		// Title содержит отображаемое название видео.
		Title string `json:"title"`
	} `json:"snippet"`
	// Status содержит сведения о публичности видео.
	Status struct {
		// PrivacyStatus отличает публичный эфир от закрытого либо доступного по ссылке.
		PrivacyStatus string `json:"privacyStatus"`
	} `json:"status"`
	// LiveStreamingDetails содержит факты начала, завершения и наличия активного чата.
	LiveStreamingDetails struct {
		// ActualStartTime появляется после фактического начала эфира.
		ActualStartTime string `json:"actualStartTime"`
		// ActualEndTime появляется после окончания эфира.
		ActualEndTime string `json:"actualEndTime"`
		// ActiveLiveChatID адресует доступный чат текущего эфира.
		ActiveLiveChatID string `json:"activeLiveChatId"`
	} `json:"liveStreamingDetails"`
}

// ValidateAPIKey проверяет пригодность key запросом videos.list по публичному рейтингу.
// Пустой массив считается успешной проверкой. Сетевой или платформенный отказ
// возвращается безопасной причиной, отмена ctx передаётся вызывающему.
func (a *Adapter) ValidateAPIKey(ctx context.Context, key string) (core.ChatReason, error) {
	if ctx == nil {
		return core.ChatReasonConfiguration, core.ErrInvalid
	}
	if ctx.Err() != nil {
		return core.ChatReasonNone, ctx.Err()
	}
	// bundle учитывает ключ вместе с оболочкой сохранённого набора.
	bundle := core.ChatCredentialBundle{YouTube: &core.YouTubeChatCredentials{APIKey: key}}
	if !validAPIKey(key) || bundle.ValidateLimit(core.ChatYouTube, a.limits.CredentialBytes) != nil {
		return core.ChatReasonCredentials, nil
	}
	// page содержит выдачу проверки, которой нужны только возвращённые ID.
	var page restPage[videoItem]
	// failure сообщает безопасную причину, err — отмену вызывающего контекста.
	failure, err := a.getREST(ctx, restKeyProbe, key, url.Values{
		"part": {"id"}, "chart": {"mostPopular"}, "maxResults": {"1"},
	}, &page)
	if err != nil || failure.reason != core.ChatReasonNone {
		return failure.reason, err
	}
	if page.Items == nil || len(page.Items) > 1 || len(page.Items) == 1 && !validVideoID(page.Items[0].ID) {
		return core.ChatReasonNetwork, nil
	}
	return core.ChatReasonNone, nil
}

// Discover сканирует поисковый индекс для настроенного source.ChannelHint и
// проверяет каждое найденное видео по текущим данным. Возвращает только полный
// набор кандидатов. Один эфир без чата проверяется Manager отдельно через DiscoverBroadcast.
func (a *Adapter) Discover(ctx context.Context, source core.ChatSource, credentials core.ChatCredentials) (core.ChatDiscovery, error) {
	// failure сообщает о несовместимой привязке или полномочиях, err — об отмене.
	failure, err := checkDiscoveryInput(ctx, source, credentials)
	if err != nil || failure.reason != core.ChatReasonNone {
		return failure.discovery(), err
	}
	// key берётся из уже проверенного набора для данного аккаунта.
	key := credentials.Bundle.YouTube.APIKey
	// ids содержит полный список уникальных ID поиска. failure и err отражают отказ всего прохода.
	ids, failure, err := a.searchVideos(ctx, key, source.ChannelHint)
	if err != nil {
		return core.ChatDiscovery{}, err
	}
	if failure.reason != core.ChatReasonNone {
		return a.incompleteSearch(failure), nil
	}

	// budget совпадает с наименьшим бюджетом вариантов у Manager.
	budget := min(a.limits.UpstreamHTTPBytes, a.limits.SnapshotMetadataBytes)
	// result хранит только перепроверенные публичные эфиры, size — их логический объём.
	var result []core.ChatBroadcast
	var size uint64
	// offset отмечает начало очередной группы идентификаторов videos.list.
	for offset := 0; offset < len(ids); offset += videoBatchSize {
		// batch содержит допустимое число идентификаторов одного запроса.
		batch := ids[offset:min(offset+videoBatchSize, len(ids))]
		// records содержит перепроверенные записи этой группы. failure и err прекращают весь проход.
		records, failure, err := a.loadVideos(ctx, key, batch)
		if err != nil {
			return core.ChatDiscovery{}, err
		}
		if failure.reason == core.ChatReasonNoBroadcast && len(batch) == 1 {
			continue
		}
		if failure.reason != core.ChatReasonNone {
			if failure.reason == core.ChatReasonNoBroadcast {
				failure.reason = core.ChatReasonNetwork
			}
			return a.incompleteSearch(failure), nil
		}
		// id сохраняет последовательность из поиска, независимо от порядка выдачи videos.list.
		for _, id := range batch {
			// record содержит текущие сведения о видео, exists подтверждает, что Google его вернул.
			record, exists := records[id]
			if !exists {
				continue
			}
			// broadcast содержит пригодный эфир, reason объясняет его отсутствие или отказ формы.
			broadcast, reason := record.broadcast(source.ChannelHint)
			if reason == core.ChatReasonNetwork {
				return a.incompleteSearch(restFailure{reason: reason}), nil
			}
			if broadcast.VideoID == "" {
				continue
			}
			size += broadcastBytes(broadcast)
			if len(result) == broadcastLimit || size > budget {
				return a.incompleteSearch(restFailure{reason: core.ChatReasonLimit}), nil
			}
			result = append(result, broadcast)
		}
	}

	if len(result) == 0 {
		return core.ChatDiscovery{State: core.ChatDiscovering, Reason: core.ChatReasonNoBroadcast, RetryAfter: a.quota.searchDelay(a.now())}, nil
	}
	return broadcastDiscovery(result), nil
}

// DiscoverBroadcast перепроверяет ровно один ранее определённый videoID для source.
// Подтверждённое отсутствие, изменение доступности или завершение возвращают
// пустой список и безопасную причину. Manager определяет дальнейший поиск.
func (a *Adapter) DiscoverBroadcast(ctx context.Context, source core.ChatSource, credentials core.ChatCredentials, videoID string) (core.ChatDiscovery, error) {
	// failure сообщает о несовместимой привязке или полномочиях, err — об отмене.
	failure, err := checkDiscoveryInput(ctx, source, credentials)
	if err != nil || failure.reason != core.ChatReasonNone {
		return failure.discovery(), err
	}
	if !validVideoID(videoID) {
		return restFailure{reason: core.ChatReasonConfiguration}.discovery(), nil
	}
	// records ограничен одним запрошенным видео, failure сообщает отказ, err — отмену контекста.
	records, failure, err := a.loadVideos(ctx, credentials.Bundle.YouTube.APIKey, []string{videoID})
	if err != nil || failure.reason != core.ChatReasonNone {
		return failure.discovery(), err
	}
	// record содержит актуальное видео, exists отличает отсутствие совпадения.
	record, exists := records[videoID]
	if !exists {
		return restFailure{reason: core.ChatReasonNoBroadcast}.discovery(), nil
	}
	// broadcast содержит пригодный эфир, reason объясняет отсутствие эфира или недоступность чата.
	broadcast, reason := record.broadcast(source.ChannelHint)
	if broadcast.VideoID == "" {
		return restFailure{reason: reason}.discovery(), nil
	}
	if broadcastBytes(broadcast) > min(a.limits.UpstreamHTTPBytes, a.limits.SnapshotMetadataBytes) {
		return restFailure{reason: core.ChatReasonLimit}.discovery(), nil
	}
	return broadcastDiscovery([]core.ChatBroadcast{broadcast}), nil
}

// checkDiscoveryInput проверяет относящиеся к YouTube поля привязки и набора ключа
// перед сетью. Manager отдельно проверяет метаданные хранения полномочий.
func checkDiscoveryInput(ctx context.Context, source core.ChatSource, credentials core.ChatCredentials) (restFailure, error) {
	if ctx == nil {
		return restFailure{}, core.ErrInvalid
	}
	if ctx.Err() != nil {
		return restFailure{}, ctx.Err()
	}
	if source.Provider != core.ChatYouTube || credentials.Provider != source.Provider ||
		source.AccountID == "" || credentials.AccountID != source.AccountID ||
		!strings.HasPrefix(source.ChannelHint, "UC") || len(source.ChannelHint) <= 2 || !validLiveChatID(source.ChannelHint) {
		return restFailure{reason: core.ChatReasonConfiguration}, nil
	}
	if credentials.Bundle.Validate(core.ChatYouTube) != nil || !validAPIKey(credentials.Bundle.YouTube.APIKey) {
		return restFailure{reason: core.ChatReasonCredentials}, nil
	}
	return restFailure{}, nil
}

// searchVideos читает полный результат поиска. Повтор токена, неизвестная форма
// страницы или превышение границы запрещают использовать уже полученные ID.
func (a *Adapter) searchVideos(ctx context.Context, key, channelID string) ([]string, restFailure, error) {
	// retry содержит срок до возможного допуска следующего полного прохода.
	if retry := a.quota.beginSearch(a.now(), a.limits.ReconnectMinBackoff); retry > 0 {
		return nil, restFailure{reason: core.ChatReasonQuota, retryAfter: retry}, nil
	}
	defer a.quota.finishSearch()
	// params фиксирует фильтры поиска и меняет только токен продолжения.
	params := url.Values{
		"part": {"snippet"}, "channelId": {channelID}, "eventType": {"live"}, "type": {"video"}, "maxResults": {"50"},
		"fields": {"items(id/videoId),nextPageToken"},
	}
	// ids хранит первый порядок уникальных видео, seenIDs исключает дубли на соседних страницах.
	var ids []string
	seenIDs := make(map[string]struct{}, searchPageSize)
	// seenTokens обнаруживает цикл прежде, чем будет сделан повторный запрос.
	seenTokens := make(map[string]struct{}, searchPageLimit)
	// operation отличает допуск первого запроса от последующих страниц.
	operation := restSearch
	// index ограничивает число фактически запрошенных страниц.
	for index := range searchPageLimit {
		// page содержит одну страницу поисковых ссылок и возможный токен продолжения.
		var page restPage[searchItem]
		// failure и err прерывают поиск при квотном, сетевом или другом отказе.
		failure, err := a.getREST(ctx, operation, key, params, &page)
		if err != nil || failure.reason != core.ChatReasonNone {
			return nil, failure, err
		}
		if page.Items == nil || len(page.Items) > searchPageSize {
			return nil, restFailure{reason: core.ChatReasonNetwork}, nil
		}
		// item даёт очередной ID, который ещё не подтверждает доступность видео.
		for _, item := range page.Items {
			// id используется только после проверки синтаксиса для фильтра videos.list.
			id := item.ID.VideoID
			if !validVideoID(id) {
				return nil, restFailure{reason: core.ChatReasonNetwork}, nil
			}
			// exists показывает, что такой ID уже добавлен в список той же попытки.
			if _, exists := seenIDs[id]; !exists {
				seenIDs[id] = struct{}{}
				ids = append(ids, id)
			}
		}
		// token указывает очередную страницу, пустое значение завершает полный проход.
		token := page.NextPageToken
		if token == "" {
			return ids, restFailure{}, nil
		}
		if index == searchPageLimit-1 || len(token) > searchTokenBytes {
			return nil, restFailure{reason: core.ChatReasonLimit}, nil
		}
		// repeated обнаруживает повтор любого токена текущего прохода.
		if _, repeated := seenTokens[token]; repeated {
			return nil, restFailure{reason: core.ChatReasonNetwork}, nil
		}
		seenTokens[token] = struct{}{}
		params.Set("pageToken", token)
		operation = restSearchNext
	}
	return nil, restFailure{reason: core.ChatReasonLimit}, nil
}

// loadVideos читает сведения о конкретной группе ids. Учитываются только ID
// из запроса. Дубликат или неожиданное продолжение не дают сделать уверенный вывод.
// Если вместо части запрошенных записей вернулись посторонние, отсутствие видео не подтверждено.
func (a *Adapter) loadVideos(ctx context.Context, key string, ids []string) (map[string]videoItem, restFailure, error) {
	// page содержит одну ограниченную выдачу сведений о видео.
	var page restPage[videoItem]
	// failure и err сохраняют результат квоты, сервиса или отмены вызывающего.
	failure, err := a.getREST(ctx, restVideos, key, url.Values{
		"part": {"snippet,status,liveStreamingDetails"}, "id": {strings.Join(ids, ",")},
		"fields": {"items(id,snippet(channelId,title),status/privacyStatus,liveStreamingDetails(actualStartTime,actualEndTime,activeLiveChatId)),nextPageToken"},
	}, &page)
	if err != nil || failure.reason != core.ChatReasonNone {
		return nil, failure, err
	}
	if page.Items == nil || len(page.Items) > videoBatchSize || page.NextPageToken != "" {
		return nil, restFailure{reason: core.ChatReasonNetwork}, nil
	}
	// records индексирует возвращённые Google записи только по запрошенным идентификаторам.
	records := make(map[string]videoItem, len(ids))
	// unexpected отмечает ответ с ID за пределами запрошенного набора.
	unexpected := false
	// item содержит очередную возвращённую запись для проверки связи с запросом.
	for _, item := range page.Items {
		if !validVideoID(item.ID) {
			return nil, restFailure{reason: core.ChatReasonNetwork}, nil
		}
		if !slices.Contains(ids, item.ID) {
			unexpected = true
			continue
		}
		// repeated обнаруживает неоднозначные записи одного ID в ответе.
		if _, repeated := records[item.ID]; repeated {
			return nil, restFailure{reason: core.ChatReasonNetwork}, nil
		}
		records[item.ID] = item
	}
	if unexpected && len(records) != len(ids) {
		return nil, restFailure{reason: core.ChatReasonNetwork}, nil
	}
	return records, restFailure{}, nil
}

// incompleteSearch сообщает о незавершённом поиске с учётом срока следующего
// допустимого прохода. Постоянный отказ всё равно останавливает этот источник.
func (a *Adapter) incompleteSearch(failure restFailure) core.ChatDiscovery {
	// result содержит классифицированный отказ и минимальную задержку провайдера.
	result := failure.discovery()
	if result.State == core.ChatRetrying {
		result.RetryAfter = max(result.RetryAfter, a.quota.searchDelay(a.now()))
	}
	return result
}

// broadcast проверяет принадлежность видео каналу, публичность и подтверждение
// фактического эфира. Пустой chat ID сохраняется у действующего видео для повторной проверки.
func (v videoItem) broadcast(channelID string) (core.ChatBroadcast, core.ChatReason) {
	if v.Snippet.ChannelID == "" || v.Status.PrivacyStatus == "" {
		return core.ChatBroadcast{}, core.ChatReasonNetwork
	}
	if v.Snippet.ChannelID != channelID || v.Status.PrivacyStatus != "public" {
		return core.ChatBroadcast{}, core.ChatReasonUnavailable
	}
	// live содержит исходные сведения о жизненном цикле эфира в YouTube.
	live := v.LiveStreamingDetails
	if live.ActualEndTime != "" {
		// ended содержит подтверждённое время окончания, err — некорректные метаданные.
		ended, err := time.Parse(time.RFC3339Nano, live.ActualEndTime)
		if err != nil || ended.IsZero() {
			return core.ChatBroadcast{}, core.ChatReasonNetwork
		}
		return core.ChatBroadcast{}, core.ChatReasonEnded
	}
	if live.ActualStartTime == "" {
		return core.ChatBroadcast{}, core.ChatReasonUnavailable
	}
	// actualStart содержит время начала с точностью YouTube, err сообщает отказ разбора.
	actualStart, err := time.Parse(time.RFC3339Nano, live.ActualStartTime)
	if err != nil {
		return core.ChatBroadcast{}, core.ChatReasonNetwork
	}
	// started переводит время в точность и часовой пояс публичного контракта.
	started := actualStart.UTC().Truncate(time.Microsecond)
	if started.IsZero() || started.Year() < 1 || started.Year() > 9999 || live.ActiveLiveChatID != "" && !validLiveChatID(live.ActiveLiveChatID) {
		return core.ChatBroadcast{}, core.ChatReasonNetwork
	}
	return core.ChatBroadcast{
		VideoID: v.ID, ChannelID: live.ActiveLiveChatID, Title: boundedTitle(v.Snippet.Title), StartedAt: &started,
	}, core.ChatReasonNone
}

// broadcastDiscovery задаёт ожидание чата либо пользовательского выбора для конечного набора.
func broadcastDiscovery(values []core.ChatBroadcast) core.ChatDiscovery {
	// result переносит готовый список кандидатов в Manager.
	result := core.ChatDiscovery{Broadcasts: values}
	if len(values) > 1 {
		result.State, result.Reason = core.ChatWaitingChat, core.ChatReasonSelectionRequired
	} else if values[0].ChannelID == "" {
		result.State, result.Reason = core.ChatWaitingChat, core.ChatReasonUnavailable
	}
	return result
}

// broadcastBytes считает логический объём варианта по правилу Manager.
func broadcastBytes(value core.ChatBroadcast) uint64 {
	return uint64(256 + len(value.VideoID) + len(value.Title) + len(value.ChannelID))
}

// validVideoID разрешает конечное значение из алфавита идентификаторов YouTube.
// Запятые исключены, поскольку videos.list использует их как разделители.
func validVideoID(value string) bool {
	if value == "" || len(value) > core.ChatIDBytes {
		return false
	}
	// index адресует очередной байт идентификатора.
	for index := range len(value) {
		// character сравнивается с разрешёнными символами идентификатора видео.
		character := value[index]
		switch {
		case character >= '0' && character <= '9',
			character >= 'a' && character <= 'z',
			character >= 'A' && character <= 'Z',
			character == '-', character == '_':
		default:
			return false
		}
	}
	return true
}

// validLiveChatID ограничивает непрозрачный ID из videos.list перед передачей в Manager и gRPC.
func validLiveChatID(value string) bool {
	return value != "" && len(value) <= core.ChatIDBytes && utf8.ValidString(value) && strings.TrimSpace(value) == value && !strings.ContainsRune(value, 0)
}

// boundedTitle оставляет публичный текст в бюджете Manager, сохраняя границы UTF-8.
func boundedTitle(value string) string {
	value = strings.ReplaceAll(value, "\x00", "")
	if len(value) <= core.ChatNameBytes {
		return value
	}
	// end указывает ближайшую границу символа в пределах допустимого числа байтов.
	end := core.ChatNameBytes
	for !utf8.RuneStart(value[end]) {
		end--
	}
	return strings.Clone(value[:end])
}
