package api

import (
	"context"
	"net/http"

	"github.com/GregoryGost/gost-restreamer/internal/chatwire"
	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/danielgtaylor/huma/v2"
)

// ChatPageInput ограничивает список чата и задаёт непрозрачное продолжение чтения.
type ChatPageInput struct {
	// Limit задаёт размер страницы без проверочной строки следующей страницы.
	Limit int `query:"limit" minimum:"1" maximum:"499" default:"100"`
	// Cursor сохраняет выданную сервером границу списка.
	Cursor string `query:"cursor" maxLength:"32768"`
}

// ChatDestinationInput адресует настройки источников одного назначения.
type ChatDestinationInput struct {
	// DestinationID содержит обязательный идентификатор неизменного родителя.
	DestinationID core.ID `path:"destinationId" minLength:"1" maxLength:"256"`
}

// ChatSourceListInput выбирает ограниченную страницу источников одного назначения.
type ChatSourceListInput struct {
	// ChatDestinationInput задаёт родителя источников.
	ChatDestinationInput
	// ChatPageInput ограничивает страницу текущего назначения.
	ChatPageInput
}

// ChatSourceCreateInput содержит параметры нового источника и его родителя из пути.
type ChatSourceCreateInput struct {
	// ChatDestinationInput связывает создаваемый источник с родителем.
	ChatDestinationInput
	// Body содержит отдельные настройки одного источника без серверной ревизии.
	Body ChatSourceCreateBody
}

// ChatSourceCreateBody описывает поля создания с обязательным явным Enabled.
type ChatSourceCreateBody struct {
	// Provider выбирает протокол чтения чата независимо от медийного назначения.
	Provider core.ChatProvider `json:"provider" enum:"youtube,twitch,vk_video_live"`
	// AccountID адресует сохранённый аккаунт той же площадки.
	AccountID core.ID `json:"account_id" minLength:"1" maxLength:"256"`
	// Name задаёт необязательное имя источника, по умолчанию пустое.
	Name Field[string] `json:"name,omitempty" maxLength:"512"`
	// Enabled явно задаёт, разрешено ли чтение при публикации назначения.
	Enabled bool `json:"enabled"`
	// ChannelHint содержит постоянный идентификатор канала. Пустота допускается у Twitch.
	ChannelHint string `json:"channel_hint" maxLength:"256"`
	// Options содержит закрытую версию настроек выбранной площадки.
	Options chatOptionsBody `json:"options"`
}

// ChatSourcePatchInput адресует источник и содержит его частичные изменения.
type ChatSourcePatchInput struct {
	// IDInput адресует изменяемый источник.
	IDInput
	// Body несёт ревизию и только те поля, которые клиент изменяет.
	Body ChatSourcePatchBody
}

// ChatSourcePatchBody описывает поля, заменяемые в одной операции по ExpectedRevision.
type ChatSourcePatchBody struct {
	// ExpectedRevision содержит положительную конфигурационную ревизию сохранённого источника.
	ExpectedRevision Decimal `json:"expected_revision" minimum:"1"`
	// Provider при передаче заменяет платформу. Домен проверяет согласованность связанных настроек целиком.
	Provider Field[core.ChatProvider] `json:"provider,omitempty" enum:"youtube,twitch,vk_video_live"`
	// AccountID при передаче заменяет привязанный аккаунт.
	AccountID Field[core.ID] `json:"account_id,omitempty" minLength:"1" maxLength:"256"`
	// Name при передаче заменяет имя источника и допускает явную пустую строку.
	Name Field[string] `json:"name,omitempty" maxLength:"512"`
	// Enabled при передаче включает или отключает чтение.
	Enabled Field[bool] `json:"enabled,omitempty"`
	// ChannelHint при передаче задаёт постоянный канал. Twitch допускает пустое значение.
	ChannelHint Field[string] `json:"channel_hint,omitempty" maxLength:"256"`
	// Options при передаче заменяет весь объект настроек.
	Options Field[chatOptionsBody] `json:"options,omitempty"`
}

// apply накладывает переданные поля на текущий источник и сохраняет родителя.
// Явный null отклоняется, а совместимость итоговой настройки проверяет домен перед CAS.
func (b ChatSourcePatchBody) apply(source *core.ChatSource) error {
	// options принимает замену настроек, сохраняя прежнее значение при отсутствии поля.
	options := chatOptionsBody(source.Options)
	// changes связывает каждую операцию наложения с известным публичным полем для безопасной ошибки.
	changes := []struct {
		// field содержит имя на границе JSON.
		field string
		// err содержит результат проверки присутствия и применения поля.
		err error
	}{
		{"provider", b.Provider.apply(&source.Provider)},
		{"account_id", b.AccountID.apply(&source.AccountID)},
		{"name", b.Name.apply(&source.Name)},
		{"enabled", b.Enabled.apply(&source.Enabled)},
		{"channel_hint", b.ChannelHint.apply(&source.ChannelHint)},
		{"options", b.Options.apply(&options)},
	}
	// change содержит результат одного поля в порядке проверки.
	for _, change := range changes {
		if change.err != nil {
			return reasonError(change.err, "invalid_field", change.field)
		}
	}
	source.Options = core.ChatOptions(options)
	return nil
}

// chatOptionsBody делегирует строгий JSON-разбор доменному каталогу вариантов.
type chatOptionsBody core.ChatOptions

// UnmarshalJSON проверяет версию, точные ключи, пустой объект варианта и отсутствие повторов.
func (o *chatOptionsBody) UnmarshalJSON(data []byte) error {
	return (*core.ChatOptions)(o).UnmarshalJSON(data)
}

// Schema описывает закрытое объединение версий и вариантов. Совпадение с Provider проверяется доменом.
func (chatOptionsBody) Schema(huma.Registry) *huma.Schema {
	// variants содержит независимые схемы для каждой площадки, чтобы исключить сочетания вариантов.
	variants := make([]*huma.Schema, 0, 3)
	// provider задаёт единственное допустимое поле платформы данной ветви.
	for _, provider := range []string{"youtube", "twitch", "vk_video_live"} {
		variants = append(variants, &huma.Schema{
			Type:                 huma.TypeObject,
			AdditionalProperties: false,
			Required:             []string{"version", provider},
			Properties: map[string]*huma.Schema{
				"version": {Type: huma.TypeInteger, Enum: []any{1}},
				provider:  {Type: huma.TypeObject, AdditionalProperties: false},
			},
		})
	}
	return &huma.Schema{Type: huma.TypeObject, OneOf: variants}
}

// registerChatControl публикует операции конфигурации, авторизации и выбора эфира до построения OpenAPI.
func (a *API) registerChatControl() {
	a.registerChatSources()
	a.registerChatAccounts()
	a.registerChatBroadcasts()
}

// chatControlOperation применяет независимый проверенный бюджет управляющего тела чата.
func (a *API) chatControlOperation(method, route, id, summary string) huma.Operation {
	// op содержит стандартную операцию и заменяет только предел её тела.
	op := operation(method, route, id, summary)
	op.MaxBodyBytes = int64(a.config.ChatLimits.RESTBodyBytes)
	return op
}

// registerChatSources публикует конфигурационные операции источника с проверкой родителя и барьерами Manager.
func (a *API) registerChatSources() {
	register(a, operation(http.MethodGet, "/destinations/{destinationId}/chat-sources", "list-chat-sources", "Получить страницу источников чата назначения."), a.listChatSources)
	// create задаёт POST с отдельным бюджетом и статусом сохранённого ресурса.
	create := a.chatControlOperation(http.MethodPost, "/destinations/{destinationId}/chat-sources", "create-chat-source", "Создать источник чата назначения.")
	create.DefaultStatus = http.StatusCreated
	register(a, create, a.createChatSource)
	register(a, operation(http.MethodGet, "/chat-sources/{id}", "get-chat-source", "Получить источник чата."), a.getChatSource)
	register(a, a.chatControlOperation(http.MethodPatch, "/chat-sources/{id}", "update-chat-source", "Изменить источник чата по ревизии."), a.updateChatSource)
	registerDelete(a, "/chat-sources/{id}", "delete-chat-source", a.config.Chat.DeleteSource)
}

// listChatSources возвращает страницу назначения с публичной проекцией источников.
func (a *API) listChatSources(ctx context.Context, input *ChatSourceListInput) (*Result[PageDTO[chatwire.SourceDTO]], error) {
	// page содержит источники одного назначения, а err сообщает об отказе чтения родителя или списка.
	page, err := a.config.Core.PageDestinationChatSources(ctx, input.DestinationID, input.Limit, input.Cursor)
	return &Result[PageDTO[chatwire.SourceDTO]]{Body: pageDTO(page, chatwire.Source)}, err
}

// createChatSource сохраняет настройку с проверкой родителя, затем уведомляет Manager о подтверждённой записи.
// После успешной вставки клиент получает ссылку на источник даже при временном отказе сверки Manager.
func (a *API) createChatSource(ctx context.Context, input *ChatSourceCreateInput) (*Created[chatwire.SourceDTO], error) {
	// body содержит принятые настройки, а name принимает необязательное имя, отличая null от отсутствия.
	body := input.Body
	var name string
	// err сообщает о явном null для имени.
	if err := body.Name.apply(&name); err != nil {
		return nil, reasonError(err, "invalid_field", "name")
	}
	// source содержит подтверждённую настройку с назначенной идентичностью, а err относится к вставке.
	source, err := a.config.Core.CreateChatSource(ctx, core.ChatSource{
		DestinationID: input.DestinationID, Provider: body.Provider, AccountID: body.AccountID,
		Name: name, Enabled: body.Enabled, ChannelHint: body.ChannelHint, Options: core.ChatOptions(body.Options),
	})
	if err != nil {
		return nil, err
	}
	// apply и cancel связывают ограниченную сверку зафиксированной настройки с остановкой API.
	apply, cancel := a.operation(a.ctx)
	defer cancel()
	// err сохраняет исходную причину, если Manager не смог подтвердить применение сохранённой настройки.
	if _, err := a.config.Chat.ApplySource(apply, source.ID, source.Revision); err != nil {
		a.config.Logger.Error("chat_source_apply", "component", "api", "source_id", source.ID, "error", err)
	}
	return &Created[chatwire.SourceDTO]{Location: Prefix + "/chat-sources/" + string(source.ID), Body: chatwire.Source(source)}, nil
}

// getChatSource возвращает сохранённую конфигурацию без чтения полномочий аккаунта.
func (a *API) getChatSource(ctx context.Context, input *IDInput) (*Result[chatwire.SourceDTO], error) {
	// source содержит настройки, err сообщает об отказе согласованного чтения.
	source, err := a.config.Core.GetChatSource(ctx, core.ID(input.ID))
	return &Result[chatwire.SourceDTO]{Body: chatwire.Source(source)}, err
}

// updateChatSource выполняет CAS под барьером Manager и возвращает подтверждённую конфигурацию.
func (a *API) updateChatSource(ctx context.Context, input *ChatSourcePatchInput) (*Result[chatwire.SourceDTO], error) {
	// expected содержит положительную конфигурационную ревизию точного запроса.
	expected := uint64(input.Body.ExpectedRevision)
	if expected == 0 {
		return nil, reasonError(core.ErrInvalid, "invalid_field", "expected_revision")
	}
	// id адресует точный источник, к которому относится барьер.
	id := core.ID(input.ID)
	// updated сохраняет ожидаемый результат CAS для выдачи после подтверждения Manager.
	var updated core.ChatSource
	// err относится к применённому изменению, включая проверку его результата Manager.
	_, err := a.config.Chat.MutateSource(ctx, id, func(operation context.Context) (core.ChatSource, error) {
		// Читает текущее значение под барьером, накладывает переданные поля и сохраняет по CAS.
		// current содержит конфигурацию до изменений, err — отказ чтения, наложения или записи.
		current, err := a.config.Core.GetChatSource(operation, id)
		if err != nil {
			return core.ChatSource{}, err
		}
		if current.Revision != expected {
			return core.ChatSource{}, reasonError(core.ErrConflict, "revision_conflict", "expected_revision")
		}
		if err = input.Body.apply(&current); err != nil {
			return core.ChatSource{}, err
		}
		updated, err = a.config.Core.UpdateChatSource(operation, current, expected)
		return updated, err
	})
	return &Result[chatwire.SourceDTO]{Body: chatwire.Source(updated)}, err
}
