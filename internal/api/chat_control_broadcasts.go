package api

import (
	"context"
	"net/http"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// ChatBroadcastChoicesInput адресует актуальный набор эфиров источника в выбранном запуске.
type ChatBroadcastChoicesInput struct {
	// IDInput адресует источник, ожидающий выбор видео.
	IDInput
	// SessionID относится к текущему присланному клиенту запуску потока.
	SessionID core.ID `query:"session_id" required:"true" minLength:"1" maxLength:"256"`
}

// ChatBroadcastDTO содержит открытые данные одного доступного для выбора эфира.
type ChatBroadcastDTO struct {
	// VideoID содержит идентификатор видео из текущего набора обнаружения.
	VideoID string `json:"video_id"`
	// Title содержит опубликованное название эфира.
	Title string `json:"title"`
	// StartedAt содержит известное время начала в UTC или отсутствует.
	StartedAt *time.Time `json:"started_at,omitempty"`
}

// ChatBroadcastChoicesDTO связывает варианты выбора с текущим запуском и поколением обнаружения.
type ChatBroadcastChoicesDTO struct {
	// SessionID адресует запуск без оперативной идентичности его владельца.
	SessionID core.ID `json:"session_id"`
	// SourceID адресует точный источник, для которого получены варианты.
	SourceID core.ID `json:"source_id"`
	// SourceRevision передаёт ревизию источника, проверяемую при выборе.
	SourceRevision Decimal `json:"source_revision" minimum:"1"`
	// DiscoveryGeneration различает последовательные наборы в рамках текущего запуска.
	DiscoveryGeneration Decimal `json:"discovery_generation" minimum:"1"`
	// Broadcasts перечисляет ограниченный список кандидатов без внутренних идентификаторов чата.
	Broadcasts []ChatBroadcastDTO `json:"broadcasts" nullable:"false"`
}

// ChatBroadcastSelectInput содержит выбор, относящийся к источнику в пути запроса.
type ChatBroadcastSelectInput struct {
	// IDInput адресует источник, для которого принимается выбор.
	IDInput
	// Body содержит полученные клиентом удостоверения набора и выбранный VideoID.
	Body ChatBroadcastSelectBody
}

// ChatBroadcastSelectBody описывает условия принятия одного видео для текущего запуска.
type ChatBroadcastSelectBody struct {
	// SessionID задаёт ранее прочитанный запуск потока.
	SessionID core.ID `json:"session_id" minLength:"1" maxLength:"256"`
	// ExpectedRevision должна совпадать с конфигурационной ревизией источника из того же набора.
	ExpectedRevision Decimal `json:"expected_revision" minimum:"1"`
	// DiscoveryGeneration должна совпадать с поколением доступного набора.
	DiscoveryGeneration Decimal `json:"discovery_generation" minimum:"1"`
	// VideoID содержит один из предоставленных набором идентификаторов видео.
	VideoID string `json:"video_id" minLength:"1" maxLength:"256"`
}

// registerChatBroadcasts публикует чтение актуального обнаружения и выбор одного эфира.
func (a *API) registerChatBroadcasts() {
	// route связывает чтение и выбор с одним источником.
	const route = "/chat-sources/{id}/broadcast-selection"
	register(a, operation(http.MethodGet, route, "list-chat-broadcasts", "Получить актуальные варианты эфира чата."), a.chatBroadcastChoices)
	// selectBroadcast описывает выбор с пустым успешным ответом и лимитом тела чатов.
	selectBroadcast := a.chatControlOperation(http.MethodPost, route, "select-chat-broadcast", "Выбрать эфир чата для текущего запуска.")
	selectBroadcast.DefaultStatus = http.StatusNoContent
	register(a, selectBroadcast, a.selectChatBroadcast)
}

// chatBroadcastChoices читает данные текущего запуска и выделяет только публичные атрибуты видео.
func (a *API) chatBroadcastChoices(ctx context.Context, input *ChatBroadcastChoicesInput) (*Result[ChatBroadcastChoicesDTO], error) {
	// choices содержит согласованный набор, err сообщает об отсутствии источника или устаревшем запуске.
	choices, err := a.config.Chat.BroadcastChoicesForSession(ctx, input.SessionID, core.ID(input.ID))
	if err != nil {
		return nil, err
	}
	// result собирает публичное описание набора без внутреннего Session fence.
	result := ChatBroadcastChoicesDTO{
		SessionID: choices.Session.SessionID, SourceID: choices.SourceID,
		SourceRevision: Decimal(choices.SourceRevision), DiscoveryGeneration: Decimal(choices.DiscoveryGeneration),
		Broadcasts: make([]ChatBroadcastDTO, len(choices.Broadcasts)),
	}
	// index задаёт позицию в исходном порядке, broadcast содержит выбранную для проекции запись.
	for index, broadcast := range choices.Broadcasts {
		result.Broadcasts[index] = ChatBroadcastDTO{VideoID: broadcast.VideoID, Title: broadcast.Title, StartedAt: utcPointer(broadcast.StartedAt)}
	}
	return &Result[ChatBroadcastChoicesDTO]{Body: result}, nil
}

// selectChatBroadcast передаёт точные удостоверения Manager, который проверяет и применяет выбор.
func (a *API) selectChatBroadcast(ctx context.Context, input *ChatBroadcastSelectInput) (*struct{}, error) {
	// body содержит параметры того набора, из которого клиент выбрал видео.
	body := input.Body
	if body.ExpectedRevision == 0 {
		return nil, reasonError(core.ErrInvalid, "invalid_field", "expected_revision")
	}
	if body.DiscoveryGeneration == 0 {
		return nil, reasonError(core.ErrInvalid, "invalid_field", "discovery_generation")
	}
	return nil, a.config.Chat.SelectBroadcastForSession(ctx, body.SessionID, core.ID(input.ID), uint64(body.ExpectedRevision), uint64(body.DiscoveryGeneration), body.VideoID)
}
