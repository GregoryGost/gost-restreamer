package api

import (
	"context"
	"errors"
	"net/http"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/chat"
	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/danielgtaylor/huma/v2"
)

// ChatAccountDTO содержит сохранённую конфигурацию аккаунта и факт её наличия.
type ChatAccountDTO struct {
	// RecordDTO передаёт конфигурационную ревизию и времена записи.
	RecordDTO
	// Provider указывает неизменную площадку аккаунта.
	Provider core.ChatProvider `json:"provider" enum:"youtube,twitch,vk_video_live"`
	// Name содержит необязательное отображаемое имя, включая пустую строку.
	Name string `json:"name"`
	// AuthorizationState сообщает о сохранённом аккаунте независимо от доступности конкретного чата.
	AuthorizationState string `json:"authorization_state" enum:"CONFIGURED"`
}

// chatAccountDTO преобразует метаданные, полученные без чтения полномочий площадки.
func chatAccountDTO(account core.ChatAccount) ChatAccountDTO {
	return ChatAccountDTO{RecordDTO: recordDTO(account.Record), Provider: account.Provider, Name: account.Name, AuthorizationState: "CONFIGURED"}
}

// ChatAccountPatchInput адресует аккаунт и изменение его имени.
type ChatAccountPatchInput struct {
	// IDInput содержит путь к изменяемому аккаунту.
	IDInput
	// Body содержит новую конфигурацию имени и ожидаемую ревизию.
	Body ChatAccountPatchBody
}

// ChatAccountPatchBody позволяет переименовать аккаунт по его конфигурационной ревизии.
type ChatAccountPatchBody struct {
	// ExpectedRevision содержит положительную ревизию аккаунта.
	ExpectedRevision Decimal `json:"expected_revision" minimum:"1"`
	// Name содержит новое имя, в том числе допустимую пустую строку.
	Name string `json:"name" maxLength:"512"`
}

// ChatConnectInput содержит начальные параметры ограниченной RAM-попытки подключения.
type ChatConnectInput struct {
	// Body содержит реквизиты только одной площадки и условные параметры замены.
	Body ChatConnectBody
}

// ChatConnectBody разделяет реквизиты площадок и необязательную пару для замены аккаунта.
type ChatConnectBody struct {
	// Provider выбирает площадку авторизации до допуска запроса.
	Provider core.ChatProvider `json:"provider" enum:"youtube,twitch,vk_video_live"`
	// Name задаёт имя для нового аккаунта.
	Name Field[string] `json:"name,omitempty" maxLength:"512" doc:"Необязательное имя нового аккаунта."`
	// AccountID адресует существующий аккаунт только при замене.
	AccountID Field[core.ID] `json:"account_id,omitempty" minLength:"1" maxLength:"256" dependentRequired:"expected_revision" doc:"Для замены передаётся вместе с expected_revision."`
	// ExpectedRevision проверяет версию существующего аккаунта при замене.
	ExpectedRevision Field[Decimal] `json:"expected_revision,omitempty" minimum:"1" dependentRequired:"account_id" doc:"Для замены передаётся вместе с account_id."`
	// APIKey передаёт исходный Google API Key только для YouTube.
	APIKey Field[string] `json:"api_key,omitempty" minLength:"1" maxLength:"65536" writeOnly:"true" doc:"Обязательный ключ для YouTube."`
	// ClientID задаёт Public-приложение Twitch или приложение DevAPI.
	ClientID Field[string] `json:"client_id,omitempty" minLength:"1" maxLength:"256" writeOnly:"true" doc:"Обязательный идентификатор для Twitch и VK Видео Live."`
	// ClientSecret передаёт закрытый ключ приложения только для VK Видео Live.
	ClientSecret Field[string] `json:"client_secret,omitempty" minLength:"1" maxLength:"65536" writeOnly:"true" doc:"Обязательный секрет для VK Видео Live."`
}

// TransformSchema выражает допустимые сочетания реквизитов и запрет имени при замене аккаунта.
func (ChatConnectBody) TransformSchema(_ huma.Registry, schema *huma.Schema) *huma.Schema {
	// forbidden отклоняет наличие поля с любым значением.
	forbidden := &huma.Schema{Not: &huma.Schema{}}
	schema.OneOf = []*huma.Schema{
		{
			Type: huma.TypeObject, Required: []string{"provider", "api_key"},
			Properties: map[string]*huma.Schema{
				"provider":  {Type: huma.TypeString, Const: string(core.ChatYouTube)},
				"api_key":   {},
				"client_id": forbidden, "client_secret": forbidden,
			},
		},
		{
			Type: huma.TypeObject, Required: []string{"provider", "client_id"},
			Properties: map[string]*huma.Schema{
				"provider":  {Type: huma.TypeString, Const: string(core.ChatTwitch)},
				"client_id": {},
				"api_key":   forbidden, "client_secret": forbidden,
			},
		},
		{
			Type: huma.TypeObject, Required: []string{"provider", "client_id", "client_secret"},
			Properties: map[string]*huma.Schema{
				"provider":  {Type: huma.TypeString, Const: string(core.ChatVKVideoLive)},
				"client_id": {}, "client_secret": {},
				"api_key": forbidden,
			},
		},
	}
	schema.Not = &huma.Schema{
		Type: huma.TypeObject, Required: []string{"account_id", "name"},
		Properties: map[string]*huma.Schema{"account_id": {}, "name": {}},
	}
	return schema
}

// request разбирает условную пару замены и ровно тот набор реквизитов, который принимает площадка.
// Существование аккаунта и его ревизию Manager проверяет перед допуском.
func (b ChatConnectBody) request() (core.ChatAuthorizationRequest, error) {
	if b.AccountID.Set && (b.AccountID.Null || b.AccountID.Value == "" || !b.ExpectedRevision.Set) {
		return core.ChatAuthorizationRequest{}, reasonError(core.ErrInvalid, "invalid_field", "account_id")
	}
	if b.ExpectedRevision.Set && (b.ExpectedRevision.Null || b.ExpectedRevision.Value == 0 || !b.AccountID.Set) {
		return core.ChatAuthorizationRequest{}, reasonError(core.ErrInvalid, "invalid_field", "expected_revision")
	}
	if b.Name.Set && (b.Name.Null || b.AccountID.Set) {
		return core.ChatAuthorizationRequest{}, reasonError(core.ErrInvalid, "invalid_field", "name")
	}
	// request передаёт Manager метаданные и только реквизиты проверенного варианта запроса.
	request := core.ChatAuthorizationRequest{
		Provider: b.Provider, AccountID: b.AccountID.Value, ExpectedRevision: uint64(b.ExpectedRevision.Value), Name: b.Name.Value,
	}
	if (b.Provider == core.ChatYouTube) != b.APIKey.Set || b.APIKey.Null {
		return core.ChatAuthorizationRequest{}, reasonError(core.ErrInvalid, "invalid_field", "api_key")
	}
	if (b.Provider == core.ChatTwitch || b.Provider == core.ChatVKVideoLive) != b.ClientID.Set || b.ClientID.Null {
		return core.ChatAuthorizationRequest{}, reasonError(core.ErrInvalid, "invalid_field", "client_id")
	}
	if (b.Provider == core.ChatVKVideoLive) != b.ClientSecret.Set || b.ClientSecret.Null {
		return core.ChatAuthorizationRequest{}, reasonError(core.ErrInvalid, "invalid_field", "client_secret")
	}
	switch b.Provider {
	case core.ChatYouTube:
		request.Bundle.YouTube = &core.YouTubeChatCredentials{APIKey: b.APIKey.Value}
	case core.ChatTwitch:
		request.ClientID = b.ClientID.Value
	case core.ChatVKVideoLive:
		request.ClientID, request.ClientSecret = b.ClientID.Value, b.ClientSecret.Value
	default:
		return core.ChatAuthorizationRequest{}, reasonError(core.ErrInvalid, "invalid_field", "provider")
	}
	return request, nil
}

// ChatAuthorizationInput адресует ровно одну оперативную попытку подключения.
type ChatAuthorizationInput struct {
	// AttemptID берётся из Location принятого подключения.
	AttemptID core.ID `path:"attemptId" minLength:"1" maxLength:"256"`
}

// ChatAuthorizationDTO содержит результат публичного чтения ограниченной RAM-попытки.
type ChatAuthorizationDTO struct {
	// AttemptID адресует попытку в рамках жизни процесса.
	AttemptID core.ID `json:"attempt_id"`
	// Provider определяет тип проверки и применимость параметров подтверждения.
	Provider core.ChatProvider `json:"provider" enum:"youtube,twitch,vk_video_live"`
	// State содержит текущее состояние авторизации.
	State core.ChatAuthorizationState `json:"state" enum:"PENDING,SUCCEEDED,FAILED,CANCELLED,EXPIRED"`
	// ExpiresAt задаёт срок действия текущей попытки в UTC.
	ExpiresAt time.Time `json:"expires_at"`
	// Cancelable сообщает, успеет ли клиент отменить попытку до допуска записи.
	Cancelable bool `json:"cancelable"`
	// AccountID присутствует для замены либо успешного создания аккаунта.
	AccountID core.ID `json:"account_id,omitempty"`
	// Code сообщает безопасную причину текущего состояния, если она известна.
	Code string `json:"code,omitempty" enum:"no_broadcast,selection_required,chat_unavailable,chat_ended,chat_disabled,credentials_required,network,quota,configuration,limit,upstream_gap,credential_commit_unknown"`
	// UserCode показывает короткий публичный код подтверждения Twitch до допуска записи.
	UserCode string `json:"user_code,omitempty"`
	// VerificationURI сообщает проверенный публичный адрес подтверждения Twitch.
	VerificationURI string `json:"verification_uri,omitempty"`
}

// chatAuthorizationDTO копирует только публичные поля. Неопределённый COMMIT остаётся ожидающим
// без возможности отмены и без параметров пользовательского подтверждения.
func chatAuthorizationDTO(view core.ChatAuthorization, unknownCommit bool) ChatAuthorizationDTO {
	// result сохраняет общие поля принятой попытки.
	result := ChatAuthorizationDTO{
		AttemptID: view.AttemptID, Provider: view.Provider, State: view.State, ExpiresAt: view.ExpiresAt.UTC(),
		Cancelable: view.Cancelable, AccountID: view.AccountID,
	}
	if unknownCommit {
		result.State = core.ChatAuthorizationPending
		result.Cancelable = false
		result.Code = "credential_commit_unknown"
		return result
	}
	if view.Reason.Validate() == nil {
		result.Code = string(view.Reason)
	}
	if view.Provider == core.ChatTwitch && view.State == core.ChatAuthorizationPending && view.Cancelable {
		result.UserCode, result.VerificationURI = view.UserCode, view.VerificationURI
	}
	return result
}

// registerChatAccounts публикует управление метаданными и единый порядок авторизации для всех площадок.
func (a *API) registerChatAccounts() {
	register(a, operation(http.MethodGet, "/chat/accounts", "list-chat-accounts", "Получить страницу аккаунтов чатов."), a.listChatAccounts)
	register(a, operation(http.MethodGet, "/chat/accounts/{id}", "get-chat-account", "Получить метаданные аккаунта чата."), a.getChatAccount)
	register(a, a.chatControlOperation(http.MethodPatch, "/chat/accounts/{id}", "update-chat-account", "Переименовать аккаунт чата по ревизии."), a.updateChatAccount)
	registerDelete(a, "/chat/accounts/{id}", "delete-chat-account", a.deleteChatAccount)
	// connect принимает реквизиты только одной площадки и возвращает адрес попытки.
	connect := a.chatControlOperation(http.MethodPost, "/chat/accounts/connect", "connect-chat-account", "Начать подключение аккаунта чата.")
	connect.DefaultStatus = http.StatusAccepted
	register(a, connect, a.connectChatAccount)
	register(a, operation(http.MethodGet, "/chat/accounts/authorizations/{attemptId}", "get-chat-authorization", "Получить состояние подключения аккаунта чата."), a.getChatAuthorization)
	// cancel возвращает 204 для подтверждённой отмены без тела.
	cancel := operation(http.MethodDelete, "/chat/accounts/authorizations/{attemptId}", "cancel-chat-authorization", "Отменить подключение аккаунта чата.")
	cancel.DefaultStatus = http.StatusNoContent
	register(a, cancel, a.cancelChatAuthorization)
}

// listChatAccounts возвращает ограниченную страницу сохранённых конфигурационных метаданных.
func (a *API) listChatAccounts(ctx context.Context, input *ChatPageInput) (*Result[PageDTO[ChatAccountDTO]], error) {
	// page содержит текущую страницу, err сообщает об отказе запроса или чтения.
	page, err := a.config.Core.PageChatAccounts(ctx, input.Limit, input.Cursor)
	return &Result[PageDTO[ChatAccountDTO]]{Body: pageDTO(page, chatAccountDTO)}, err
}

// getChatAccount читает только конфигурационные метаданные одного аккаунта.
func (a *API) getChatAccount(ctx context.Context, input *IDInput) (*Result[ChatAccountDTO], error) {
	// account содержит запись, err сообщает об отказе чтения.
	account, err := a.config.Core.GetChatAccount(ctx, core.ID(input.ID))
	return &Result[ChatAccountDTO]{Body: chatAccountDTO(account)}, err
}

// updateChatAccount меняет только имя по конфигурационной ревизии.
func (a *API) updateChatAccount(ctx context.Context, input *ChatAccountPatchInput) (*Result[ChatAccountDTO], error) {
	// expected содержит положительную ревизию изменения имени.
	expected := uint64(input.Body.ExpectedRevision)
	if expected == 0 {
		return nil, reasonError(core.ErrInvalid, "invalid_field", "expected_revision")
	}
	// account содержит конфигурацию с неизменной площадкой, а err сообщает об отказе чтения или CAS.
	account, err := a.config.Core.GetChatAccount(ctx, core.ID(input.ID))
	if err != nil {
		return nil, err
	}
	if account.Revision != expected {
		return nil, reasonError(core.ErrConflict, "revision_conflict", "expected_revision")
	}
	account.Name = input.Body.Name
	account, err = a.config.Core.UpdateChatAccount(ctx, account, expected)
	return &Result[ChatAccountDTO]{Body: chatAccountDTO(account)}, err
}

// deleteChatAccount передаёт удаление Manager для CAS и освобождения слота полномочий после подтверждения.
func (a *API) deleteChatAccount(ctx context.Context, id core.ID, expected uint64) error {
	// err содержит результат CAS и проверки зависимых источников.
	err := a.config.Chat.DeleteAccount(ctx, id, expected)
	if errors.Is(err, core.ErrInUse) {
		return reasonError(err, "resource_in_use", "")
	}
	return err
}

// connectChatAccount создаёт попытку через Manager и указывает клиенту адрес результата.
func (a *API) connectChatAccount(ctx context.Context, input *ChatConnectInput) (*Created[ChatAuthorizationDTO], error) {
	// request передаёт реквизиты и метаданные Manager, err сообщает о недопустимой структуре запроса.
	request, err := input.Body.request()
	if err != nil {
		return nil, err
	}
	// view содержит публичное состояние принятой попытки, err сообщает об отказе допуска.
	view, err := a.config.Chat.Connect(ctx, request)
	if errors.Is(err, core.ErrNotReady) {
		return nil, reasonError(errors.Join(unavailable(), err), "runtime_unavailable", "")
	}
	if err != nil {
		return nil, err
	}
	return &Created[ChatAuthorizationDTO]{Location: Prefix + "/chat/accounts/authorizations/" + string(view.AttemptID), Body: chatAuthorizationDTO(view, false)}, nil
}

// getChatAuthorization выдаёт текущее состояние, сохраняя неопределённый исход как ожидающую попытку.
func (a *API) getChatAuthorization(ctx context.Context, input *ChatAuthorizationInput) (*Result[ChatAuthorizationDTO], error) {
	// view содержит безопасную проекцию попытки, err различает отсутствие и неопределённый COMMIT.
	view, err := a.config.Chat.Authorization(ctx, input.AttemptID)
	// unknownCommit требует успешного чтения публичной неопределённости.
	unknownCommit := errors.Is(err, chat.ErrCredentialCommitUnknown)
	if err != nil && !unknownCommit {
		return nil, err
	}
	return &Result[ChatAuthorizationDTO]{Body: chatAuthorizationDTO(view, unknownCommit)}, nil
}

// cancelChatAuthorization останавливает проверку до принятия записи либо сообщает о конфликте.
func (a *API) cancelChatAuthorization(ctx context.Context, input *ChatAuthorizationInput) (*struct{}, error) {
	return nil, a.config.Chat.CancelAuthorization(ctx, input.AttemptID)
}
