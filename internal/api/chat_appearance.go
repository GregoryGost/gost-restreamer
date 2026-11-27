package api

import (
	"context"
	jsonv2 "encoding/json/v2"
	"net/http"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// chatAppearancePath адресует оформление выбранного потока в управляющем API чата.
const chatAppearancePath = "/chat/streams/{streamId}/appearance"

// ChatBackgroundAppearanceDTO описывает видимость, цвет и прозрачность одного фона.
type ChatBackgroundAppearanceDTO struct {
	// Enabled показывает, рисуется ли фон.
	Enabled bool `json:"enabled"`
	// Color содержит цвет фона в формате #RRGGBB.
	Color string `json:"color" pattern:"^#[0-9a-fA-F]{6}$"`
	// Transparency задаёт процент прозрачности фона.
	Transparency int `json:"transparency" minimum:"0" maximum:"100"`
}

// ChatTextOutlineAppearanceDTO описывает обводку сообщения и имени его автора.
type ChatTextOutlineAppearanceDTO struct {
	// Enabled показывает, рисуется ли обводка.
	Enabled bool `json:"enabled"`
	// Color содержит цвет обводки в формате #RRGGBB.
	Color string `json:"color" pattern:"^#[0-9a-fA-F]{6}$"`
	// WidthPx задаёт толщину обводки в пикселях CSS.
	WidthPx int `json:"width_px" minimum:"0" maximum:"8"`
	// Transparency задаёт процент прозрачности обводки.
	Transparency int `json:"transparency" minimum:"0" maximum:"100"`
}

// ChatMessageTextAppearanceDTO описывает заливку и размер текста сообщения.
type ChatMessageTextAppearanceDTO struct {
	// Color содержит цвет заливки в формате #RRGGBB.
	Color string `json:"color" pattern:"^#[0-9a-fA-F]{6}$"`
	// SizePx задаёт размер текста в пикселях CSS.
	SizePx int `json:"size_px" minimum:"8" maximum:"72"`
	// Transparency задаёт процент прозрачности заливки букв.
	Transparency int `json:"transparency" minimum:"0" maximum:"100"`
}

// ChatAppearanceDTO содержит все параметры оформления, включая настройки выключенных элементов.
type ChatAppearanceDTO struct {
	// DashboardBackground описывает фон области чата в Dashboard.
	DashboardBackground ChatBackgroundAppearanceDTO `json:"dashboard_background"`
	// OverlayBackground описывает фон Overlay.
	OverlayBackground ChatBackgroundAppearanceDTO `json:"overlay_background"`
	// OverlayTransparency задаёт общую прозрачность Overlay в процентах.
	OverlayTransparency int `json:"overlay_transparency" minimum:"0" maximum:"100"`
	// MessageBackground описывает фон отдельных сообщений.
	MessageBackground ChatBackgroundAppearanceDTO `json:"message_background"`
	// TextOutline содержит общую обводку текста и имён авторов.
	TextOutline ChatTextOutlineAppearanceDTO `json:"text_outline"`
	// MessageText описывает заливку и размер текста сообщения.
	MessageText ChatMessageTextAppearanceDTO `json:"message_text"`
}

// domain переводит полный набор параметров HTTP в настройки потока для проверки доменом.
func (a ChatAppearanceDTO) domain() core.ChatAppearance {
	return core.ChatAppearance{
		DashboardBackground: core.ChatBackgroundAppearance(a.DashboardBackground),
		OverlayBackground:   core.ChatBackgroundAppearance(a.OverlayBackground),
		OverlayTransparency: a.OverlayTransparency,
		MessageBackground:   core.ChatBackgroundAppearance(a.MessageBackground),
		TextOutline:         core.ChatTextOutlineAppearance(a.TextOutline),
		MessageText:         core.ChatMessageTextAppearance(a.MessageText),
	}
}

// ChatAppearanceInput адресует оформление конкретного потока по идентификатору в пути.
type ChatAppearanceInput struct {
	// StreamID определяет поток, оформление которого читает или заменяет клиент.
	StreamID core.ID `path:"streamId" minLength:"1" maxLength:"256"`
}

// ChatAppearanceUpdateInput объединяет путь и полную замену оформления.
type ChatAppearanceUpdateInput struct {
	// ChatAppearanceInput связывает замену с выбранным потоком.
	ChatAppearanceInput
	// Body содержит ожидаемую ревизию и все параметры оформления.
	Body ChatAppearanceUpdateBody
}

// ChatAppearanceUpdateBody задаёт условную замену полного набора настроек.
type ChatAppearanceUpdateBody struct {
	// ExpectedRevision содержит положительную точную ревизию конфигурации потока.
	ExpectedRevision Decimal `json:"expected_revision" minimum:"1"`
	// Appearance содержит все параметры, которые клиент сохраняет одной операцией.
	Appearance ChatAppearanceDTO `json:"appearance"`
}

// UnmarshalJSON декодирует полную замену с точными именами и отклоняет неизвестные или повторяющиеся свойства.
// data содержит исходное тело PUT; обязательность и null дополнительно проверяются схемой операции Huma.
func (b *ChatAppearanceUpdateBody) UnmarshalJSON(data []byte) error {
	// chatAppearanceUpdateBody обходит этот метод, сохраняя обычное представление DTO для jsonv2.
	type chatAppearanceUpdateBody ChatAppearanceUpdateBody
	return jsonv2.Unmarshal(data, (*chatAppearanceUpdateBody)(b), jsonv2.RejectUnknownMembers(true))
}

// ChatAppearanceRevisionDTO возвращает оформление и ревизию конфигурации потока для следующей записи.
type ChatAppearanceRevisionDTO struct {
	// Revision содержит точную текущую ревизию потока.
	Revision Decimal `json:"revision" minimum:"1"`
	// Appearance содержит готовые к отображению параметры, включая значения по умолчанию при первом чтении.
	Appearance ChatAppearanceDTO `json:"appearance"`
}

// chatAppearanceResponse проецирует только оформление и ревизию потока в ответ HTTP.
func chatAppearanceResponse(stream core.Stream) *Result[ChatAppearanceRevisionDTO] {
	// appearance содержит сохранённые настройки либо значения по умолчанию для этой проекции.
	appearance := stream.EffectiveChatAppearance()
	return &Result[ChatAppearanceRevisionDTO]{Body: ChatAppearanceRevisionDTO{
		Revision: Decimal(stream.Revision),
		Appearance: ChatAppearanceDTO{
			DashboardBackground: ChatBackgroundAppearanceDTO(appearance.DashboardBackground),
			OverlayBackground:   ChatBackgroundAppearanceDTO(appearance.OverlayBackground),
			OverlayTransparency: appearance.OverlayTransparency,
			MessageBackground:   ChatBackgroundAppearanceDTO(appearance.MessageBackground),
			TextOutline:         ChatTextOutlineAppearanceDTO(appearance.TextOutline),
			MessageText:         ChatMessageTextAppearanceDTO(appearance.MessageText),
		},
	}}
}

// registerChatAppearance публикует чтение и условную замену оформления по тому же пути.
func (a *API) registerChatAppearance() {
	register(a, operation(http.MethodGet, chatAppearancePath, "get-chat-appearance", "Получить оформление чата потока."), a.getChatAppearance)
	register(a, a.chatControlOperation(http.MethodPut, chatAppearancePath, "update-chat-appearance", "Сохранить оформление чата потока по ревизии."), a.updateChatAppearance)
}

// getChatAppearance читает через ctx настройки и ревизию одного потока для ответа. Ошибка чтения
// возвращается через общий преобразователь API.
func (a *API) getChatAppearance(ctx context.Context, input *ChatAppearanceInput) (*Result[ChatAppearanceRevisionDTO], error) {
	// err сообщает о недопустимом доменном идентификаторе выбранного потока.
	if err := input.StreamID.Validate(); err != nil {
		return nil, err
	}
	// stream содержит текущую конфигурацию, а err сообщает об отказе чтения, включая отсутствие потока.
	stream, err := a.config.Core.GetStream(ctx, input.StreamID)
	if err != nil {
		return nil, err
	}
	return chatAppearanceResponse(stream), nil
}

// updateChatAppearance проверяет положительную ревизию и сохраняет весь набор настроек через ctx.
// Ответ содержит оформление с новой ревизией после успешной фиксации.
func (a *API) updateChatAppearance(ctx context.Context, input *ChatAppearanceUpdateInput) (*Result[ChatAppearanceRevisionDTO], error) {
	// expected содержит ожидаемую ревизию, декодированную без округления.
	expected := uint64(input.Body.ExpectedRevision)
	if expected == 0 {
		return nil, reasonError(core.ErrInvalid, "invalid_field", "expected_revision")
	}
	// stream содержит результат условного изменения, а err сообщает об отказе проверки или сохранения.
	stream, err := a.config.Core.UpdateChatAppearance(ctx, input.StreamID, input.Body.Appearance.domain(), expected)
	if err != nil {
		return nil, err
	}
	return chatAppearanceResponse(stream), nil
}
