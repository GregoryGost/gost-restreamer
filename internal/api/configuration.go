package api

import (
	"context"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// Result оборачивает типизированное JSON-тело обычного ответа Huma.
type Result[T any] struct { /* Body содержит публичное тело результата операции. */
	Body T
}

// Created передаёт JSON созданного или принятого ресурса вместе с заголовком Location.
type Created[T any] struct {
	// Location указывает публичный адрес созданного ресурса либо квитанции асинхронной операции.
	Location string `header:"Location"`
	// Body содержит публичное представление созданного ресурса или принятой команды.
	Body T
}

// IDInput адресует ресурс по обязательному непустому идентификатору пути.
type IDInput struct {
	// ID содержит идентификатор ресурса из сегмента id.
	ID string `path:"id" minLength:"1"`
}

// DeleteInput дополняет идентификатор ресурса обязательной ожидаемой ревизией для удаления.
type DeleteInput struct {
	// IDInput задаёт удаляемый ресурс в пути.
	IDInput
	// ExpectedRevision содержит обязательную каноническую положительную ревизию в query.
	ExpectedRevision string `query:"expected_revision" required:"true"`
}

// PageParams задаёт ограничение страницы, курсор и необязательные временные границы списка.
type PageParams struct {
	// Limit задаёт число запрашиваемых записей от 1 до 499, по умолчанию 100.
	Limit int `query:"limit" minimum:"1" maximum:"499" default:"100"`
	// Cursor содержит непрозрачную позицию продолжения списка.
	Cursor string `query:"cursor" maxLength:"32768"`
	// From задаёт необязательную нижнюю временную границу в RFC3339Nano.
	From string `query:"from"`
	// To задаёт необязательную верхнюю временную границу в RFC3339Nano.
	To string `query:"to"`
}

// request преобразует параметры страницы в доменный запрос и разбирает границы времени RFC3339Nano.
// Ошибка времени даёт некорректный запрос без подстановки другой границы.
func (p PageParams) request() (core.PageRequest, error) {
	// q собирает доменные параметры страницы с числом записей и разобранными временными границами.
	q := core.ListQuery{Limit: p.Limit}
	// i различает нижнюю и верхнюю границу; s содержит очередную необязательную строку времени.
	for i, s := range []string{p.From, p.To} {
		if s != "" {
			// t содержит разобранную временную границу; err содержит отказ RFC3339Nano,
			// отображаемый в плохой запрос.
			t, err := time.Parse(time.RFC3339Nano, s)
			if err != nil {
				return core.PageRequest{}, badRequest()
			}
			if i == 0 {
				q.From = &t
			} else {
				q.To = &t
			}
		}
	}
	return core.PageRequest{Query: q, Cursor: p.Cursor}, nil
}

// StreamListInput задаёт страницу потоков с фильтром выбранной заглушки.
type StreamListInput struct {
	// PageParams содержит параметры страницы, продолжения и времени списка потоков.
	PageParams
	// AssetID ограничивает потоки использующейся заглушкой, если задан.
	AssetID string `query:"asset_id"`
}

// DestinationListInput задаёт страницу назначений с фильтрами потока и группы.
type DestinationListInput struct {
	// PageParams содержит общие параметры страницы назначений.
	PageParams
	// StreamID ограничивает назначения выбранным потоком.
	StreamID string `query:"stream_id"`
	// GroupID ограничивает назначения выбранной медиагруппой.
	GroupID string `query:"group_id"`
}

// GroupListInput задаёт страницу групп с фильтрами потока и профиля.
type GroupListInput struct {
	// PageParams содержит общие параметры страницы групп.
	PageParams
	// StreamID ограничивает группы выбранным потоком.
	StreamID string `query:"stream_id"`
	// ProfileID ограничивает группы выбранным профилем.
	ProfileID string `query:"profile_id"`
}

// SessionListInput задаёт страницу долговечной истории Session с фильтрами потока и состояния.
type SessionListInput struct {
	// PageParams содержит параметры страницы истории и временного диапазона.
	PageParams
	// StreamID ограничивает историю выбранным потоком.
	StreamID string `query:"stream_id"`
	// State ограничивает историю выбранным долговечным состоянием Session.
	State string `query:"state" enum:"STARTING,RUNNING,STOPPING,STOPPED,FAILED,INTERRUPTED"`
}

// StreamSessionListInput адресует историю конкретного потока с необязательным состоянием.
type StreamSessionListInput struct {
	// IDInput задаёт родительский Stream, существование которого проверяется перед списком.
	IDInput
	// PageParams содержит ограничение страницы, курсор и временные границы истории потока.
	PageParams
	// State ограничивает запуски потока выбранным состоянием Session.
	State string `query:"state" enum:"STARTING,RUNNING,STOPPING,STOPPED,FAILED,INTERRUPTED"`
}

// NestedListInput сочетает идентификатор родительского ресурса и параметры вложенного списка.
type NestedListInput struct {
	// IDInput задаёт родителя, существование которого проверяет соответствующий обработчик.
	IDInput
	// PageParams содержит параметры ограниченного вложенного списка.
	PageParams
}

// StreamCreateBody задаёт имя, неизменяемый постоянный ключ и необязательную политику нового Stream.
type StreamCreateBody struct {
	// Name содержит непустое имя нового потока.
	Name string `json:"name" minLength:"1"`
	// PublicationKey содержит непустой постоянный ключ публикации OBS нового потока.
	PublicationKey string `json:"publication_key" minLength:"1"`
	// Fallback содержит политику заглушки нового потока либо nil при её отсутствии.
	Fallback *PolicyDTO `json:"fallback,omitempty"`
}

// StreamCreate передаёт Huma JSON-тело создания потока.
type StreamCreate struct { /* Body содержит проверяемые параметры нового Stream. */
	Body StreamCreateBody
}

// StreamPatchBody задаёт ожидаемую ревизию и частичные изменения потока без изменения publication_key.
type StreamPatchBody struct {
	// ExpectedRevision содержит ненулевую ревизию для проверки конкурирующего изменения Stream.
	ExpectedRevision Decimal `json:"expected_revision"`
	// Name различает отсутствие изменения имени и явно переданную строку; null отклоняется.
	Name Field[string] `json:"name,omitempty"`
	// Fallback различает сохранение прежней политики, её очистку через null и замену.
	Fallback NullablePolicy `json:"fallback,omitempty"`
}

// StreamPatch адресует поток и передаёт его частичное изменение по ревизии.
type StreamPatch struct {
	// IDInput задаёт изменяемый Stream.
	IDInput
	// Body содержит ожидаемую ревизию и только передаваемые изменения Stream.
	Body StreamPatchBody
}

// DestinationCreateBody задаёт связи и полные параметры нового исходящего назначения.
type DestinationCreateBody struct {
	// StreamID указывает поток нового назначения.
	StreamID string `json:"stream_id" minLength:"1"`
	// GroupID указывает группу, чей медиавыход получит назначение.
	GroupID string `json:"group_id" minLength:"1"`
	// Name содержит непустое имя назначения.
	Name string `json:"name" minLength:"1"`
	// Platform содержит непустое обозначение площадки назначения.
	Platform string `json:"platform" minLength:"1"`
	// Endpoint содержит полный непустой адрес назначения.
	Endpoint string `json:"endpoint" minLength:"1"`
	// StreamKey содержит ключ назначения, включая допустимую пустую строку.
	StreamKey string `json:"stream_key"`
	// Enabled задаёт первоначальный признак включения назначения.
	Enabled bool `json:"enabled"`
}

// DestinationCreate передаёт JSON создания назначения с самостоятельными настройками.
type DestinationCreate struct { /* Body содержит все параметры нового назначения. */
	Body DestinationCreateBody
}

// DestinationPatchBody различает отсутствующие и явно заданные изменения назначения по ожидаемой
// ревизии.
type DestinationPatchBody struct {
	// ExpectedRevision содержит ненулевую ревизию для проверки конкурирующего изменения назначения.
	ExpectedRevision Decimal `json:"expected_revision"`
	// StreamID позволяет изменить связь потока; отсутствие сохраняет прежнюю связь, null
	// отклоняется.
	StreamID Field[string] `json:"stream_id,omitempty"`
	// GroupID позволяет изменить связь группы; отсутствие сохраняет прежнюю связь, null отклоняется.
	GroupID Field[string] `json:"group_id,omitempty"`
	// Name передаёт новое имя либо сохраняет прежнее при отсутствии поля.
	Name Field[string] `json:"name,omitempty"`
	// Platform передаёт новое обозначение площадки либо сохраняет прежнее.
	Platform Field[string] `json:"platform,omitempty"`
	// Endpoint передаёт новый полный адрес назначения либо сохраняет прежний.
	Endpoint Field[string] `json:"endpoint,omitempty"`
	// StreamKey позволяет явно очистить ключ пустой строкой; отсутствие сохраняет прежний ключ, null
	// отклоняется.
	StreamKey Field[string] `json:"stream_key,omitempty"`
	// Enabled позволяет явно передать false; отсутствие сохраняет прежний признак, null отклоняется.
	Enabled Field[bool] `json:"enabled,omitempty"`
}

// DestinationPatch адресует назначение и передаёт частичные изменения по ревизии.
type DestinationPatch struct {
	// IDInput задаёт изменяемое назначение.
	IDInput
	// Body содержит ожидаемую ревизию и присутствующие изменения назначения.
	Body DestinationPatchBody
}

// ProfileCreateBody задаёт имя и полный входной профиль обработки новой конфигурации.
type ProfileCreateBody struct {
	// Name содержит непустое имя нового профиля.
	Name string `json:"name" minLength:"1"`
	// Profile содержит полный профиль видео и самостоятельного LIVE-аудио с входными настройками
	// кодировщика.
	Profile GroupProfileInput `json:"profile"`
}

// ProfileCreate передаёт JSON создания именованного профиля.
type ProfileCreate struct { /* Body содержит имя и полный набор параметров нового профиля. */
	Body ProfileCreateBody
}

// ProfilePatchBody задаёт частичное изменение имени и полную замену параметров профиля по ревизии.
type ProfilePatchBody struct {
	// ExpectedRevision содержит ненулевую ревизию изменяемого профиля.
	ExpectedRevision Decimal `json:"expected_revision"`
	// Name сохраняет прежнее имя при отсутствии поля и отклоняет null.
	Name    Field[string]            `json:"name,omitempty"`
	Profile Field[GroupProfileInput] `json:"profile,omitempty" doc:"Полная замена профиля. Отсутствующее поле сохраняет прежний профиль; новые настройки действуют со следующего запуска."`
}

// ProfilePatch адресует профиль и передаёт изменения его имени или полного содержимого.
type ProfilePatch struct {
	// IDInput задаёт изменяемый профиль.
	IDInput
	// Body содержит ожидаемую ревизию, необязательное имя и необязательную полную замену профиля.
	Body ProfilePatchBody
}

// GroupCreateBody задаёт имя и связи новой медиагруппы.
type GroupCreateBody struct {
	// StreamID указывает родительский поток новой группы.
	StreamID string `json:"stream_id" minLength:"1"`
	// ProfileID указывает профиль обработки новой группы.
	ProfileID string `json:"profile_id" minLength:"1"`
	// Name содержит непустое имя новой группы.
	Name string `json:"name" minLength:"1"`
}

// GroupCreate передаёт JSON создания группы с указанными связями.
type GroupCreate struct { /* Body содержит имя, StreamID и ProfileID новой группы. */
	Body GroupCreateBody
}

// GroupPatchBody задаёт частичное изменение имени и связей группы по ревизии.
type GroupPatchBody struct {
	// ExpectedRevision содержит ненулевую ревизию изменяемой группы.
	ExpectedRevision Decimal `json:"expected_revision"`
	// StreamID передаёт новую связь с потоком либо сохраняет прежнюю при отсутствии.
	StreamID Field[string] `json:"stream_id,omitempty"`
	// ProfileID передаёт новую связь с профилем либо сохраняет прежнюю при отсутствии.
	ProfileID Field[string] `json:"profile_id,omitempty"`
	// Name передаёт новое имя либо сохраняет прежнее при отсутствии.
	Name Field[string] `json:"name,omitempty"`
}

// GroupPatch адресует группу и передаёт частичное изменение её настроек.
type GroupPatch struct {
	// IDInput задаёт изменяемую группу.
	IDInput
	// Body содержит ожидаемую ревизию и присутствующие изменения группы.
	Body GroupPatchBody
}

// PolicyResult возвращает сохранённую политику заглушки вместе с ревизией Stream.
type PolicyResult struct {
	// Revision содержит точную текущую ревизию потока для последующего изменения политики.
	Revision Decimal `json:"revision"`
	// Fallback содержит текущую политику либо null, если заглушка не выбрана.
	Fallback *PolicyDTO `json:"fallback"`
}

// PolicyPut адресует поток и требует полную замену либо очистку его политики заглушки.
type PolicyPut struct {
	// IDInput задаёт поток, чья политика заменяется.
	IDInput
	// Body содержит ожидаемую ревизию и обязательное присутствие Fallback.
	Body struct {
		// ExpectedRevision содержит ненулевую ревизию Stream для проверки изменения политики.
		ExpectedRevision Decimal `json:"expected_revision"`
		// Fallback содержит новую политику либо явный null для очистки; отсутствие отклоняется.
		Fallback NullablePolicy `json:"fallback"`
	}
}

// registerConfiguration публикует типизированные операции конфигурации и связанных списков. Изменения
// сохраняют отсутствующие поля, применяют допустимые null/false/пустые строки и передают ожидаемую
// ревизию домену.
func (a *API) registerConfiguration() {
	// c предоставляет доменные операции сохранённой конфигурации всем зарегистрированным
	// обработчикам.
	c := a.config.Core
	register(a, operation("GET", "/streams", "list-streams", "Список потоков."), func(ctx context.Context, i *StreamListInput) (*Result[PageDTO[StreamDTO]], error) {
		// Возвращает страницу потоков с переданным фильтром по заглушке и параметрами продолжения списка.

		// q содержит проверенную страницу потоков; e содержит отказ параметров списка или
		// курсора.
		q, e := i.request()
		if e != nil {
			return nil, e
		}
		q.Query.AssetID = core.ID(i.AssetID)
		// p содержит страницу потоков с фильтром AssetID.
		p, e := c.ListStreams(ctx, q)
		return &Result[PageDTO[StreamDTO]]{pageDTO(p, streamDTO)}, e
	})
	// op задаёт регистрацию создаваемого ресурса со статусом 201 и переиспользуется для других
	// операций создания.
	op := operation("POST", "/streams", "create-stream", "Создать поток.")
	op.DefaultStatus = 201
	register(a, op, func(ctx context.Context, i *StreamCreate) (*Created[StreamDTO], error) {
		// Преобразует необязательную политику заглушки и создаёт поток со ссылкой на его ресурс.

		// v собирает новый Stream из имени, постоянного ключа и необязательной политики.
		v := core.Stream{Name: i.Body.Name, PublicationKey: i.Body.PublicationKey}
		if i.Body.Fallback != nil {
			// p содержит проверенную доменную политику нового потока; e содержит отказ её
			// преобразования.
			p, e := i.Body.Fallback.domain()
			if e != nil {
				return nil, e
			}
			v.Fallback = p
		}
		// s содержит созданный Stream; e содержит отказ доменного создания.
		s, e := c.CreateStream(ctx, v)
		return &Created[StreamDTO]{Prefix + "/streams/" + string(s.ID), streamDTO(s)}, e
	})
	register(a, operation("GET", "/streams/{id}", "get-stream", "Получить поток."), func(ctx context.Context, i *IDInput) (*Result[StreamDTO], error) {
		// Возвращает публичное представление потока по идентификатору.

		// s содержит сохранённый Stream; e содержит отказ чтения для GET.
		s, e := c.GetStream(ctx, core.ID(i.ID))
		return &Result[StreamDTO]{streamDTO(s)}, e
	})
	register(a, operation("PATCH", "/streams/{id}", "update-stream", "Изменить поток по ревизии."), func(ctx context.Context, i *StreamPatch) (*Result[StreamDTO], error) {
		// Применяет переданные поля потока, включая очистку политики через null, и сохраняет их с ожидаемой
		// ревизией.

		if i.Body.ExpectedRevision == 0 {
			return nil, core.ErrInvalid
		}
		// s содержит текущий Stream для наложения PATCH; e содержит отказ его чтения.
		s, e := c.GetStream(ctx, core.ID(i.ID))
		if e != nil {
			return nil, e
		}
		if e = i.Body.Name.apply(&s.Name); e != nil {
			return nil, e
		}
		if i.Body.Fallback.Set {
			s.Fallback = core.FallbackPolicy{}
			if !i.Body.Fallback.Null {
				s.Fallback, e = i.Body.Fallback.Value.domain()
				if e != nil {
					return nil, e
				}
			}
		}
		s, e = c.UpdateStream(ctx, s, uint64(i.Body.ExpectedRevision))
		return &Result[StreamDTO]{streamDTO(s)}, e
	})
	registerDelete(a, "/streams/{id}", "delete-stream", c.DeleteStream)
	register(a, operation("GET", "/streams/{id}/destinations", "list-stream-destinations", "Назначения потока."), func(ctx context.Context, i *NestedListInput) (*Result[PageDTO[DestinationDTO]], error) {
		// Проверяет поток и возвращает страницу его назначений.

		// e содержит отказ проверки родительского Stream перед вложенным списком назначений.
		if _, e := c.GetStream(ctx, core.ID(i.ID)); e != nil {
			return nil, e
		}
		// q содержит проверенную страницу назначений потока; e содержит отказ параметров списка.
		q, e := i.request()
		if e != nil {
			return nil, e
		}
		q.Query.StreamID = core.ID(i.ID)
		// p содержит страницу назначений, ограниченную StreamID из пути.
		p, e := c.ListDestinations(ctx, q)
		return &Result[PageDTO[DestinationDTO]]{pageDTO(p, destinationDTO)}, e
	})
	register(a, operation("GET", "/streams/{id}/sessions", "list-stream-sessions", "История запусков потока."), func(ctx context.Context, i *StreamSessionListInput) (*Result[PageDTO[SessionDTO]], error) {
		// Проверяет поток и возвращает страницу его истории запусков с фильтром состояния.

		// e содержит отказ проверки родительского Stream перед чтением истории.
		if _, e := c.GetStream(ctx, core.ID(i.ID)); e != nil {
			return nil, e
		}
		// q содержит проверенные параметры страницы истории потока; e содержит отказ курсора или
		// временных границ.
		q, e := i.request()
		if e != nil {
			return nil, e
		}
		q.Query.StreamID = core.ID(i.ID)
		q.Query.State = i.State
		// p содержит страницу Session текущего потока с фильтром состояния.
		p, e := c.ListSessions(ctx, q)
		return &Result[PageDTO[SessionDTO]]{pageDTO(p, sessionDTO)}, e
	})
	register(a, operation("GET", "/destinations", "list-destinations", "Список назначений."), func(ctx context.Context, i *DestinationListInput) (*Result[PageDTO[DestinationDTO]], error) {
		// Возвращает страницу назначений с фильтрами потока и группы.

		// q содержит проверенные параметры страницы назначений; e содержит отказ их
		// преобразования.
		q, e := i.request()
		if e != nil {
			return nil, e
		}
		q.Query.StreamID = core.ID(i.StreamID)
		q.Query.GroupID = core.ID(i.GroupID)
		// p содержит страницу назначений с выбранными StreamID и GroupID.
		p, e := c.ListDestinations(ctx, q)
		return &Result[PageDTO[DestinationDTO]]{pageDTO(p, destinationDTO)}, e
	})
	op = operation("POST", "/destinations", "create-destination", "Создать назначение.")
	op.DefaultStatus = 201
	register(a, op, func(ctx context.Context, i *DestinationCreate) (*Created[DestinationDTO], error) {
		// Создаёт назначение из переданных настроек и возвращает адрес созданного ресурса.

		// b содержит входные параметры создаваемого назначения.
		b := i.Body
		// d содержит созданное назначение; e содержит отказ доменного создания.
		d, e := c.CreateDestination(ctx, core.Destination{StreamID: core.ID(b.StreamID), GroupID: core.ID(b.GroupID), Name: b.Name, Platform: b.Platform, Endpoint: b.Endpoint, StreamKey: b.StreamKey, Enabled: b.Enabled})
		return &Created[DestinationDTO]{Prefix + "/destinations/" + string(d.ID), destinationDTO(d)}, e
	})
	register(a, operation("GET", "/destinations/{id}", "get-destination", "Получить назначение."), func(ctx context.Context, i *IDInput) (*Result[DestinationDTO], error) {
		// Возвращает сохранённые настройки назначения в публичном представлении.

		// v содержит сохранённое назначение; e содержит отказ его чтения для GET.
		v, e := c.GetDestination(ctx, core.ID(i.ID))
		return &Result[DestinationDTO]{destinationDTO(v)}, e
	})
	register(a, operation("PATCH", "/destinations/{id}", "update-destination", "Изменить назначение по ревизии."), func(ctx context.Context, i *DestinationPatch) (*Result[DestinationDTO], error) {
		// Применяет только переданные поля назначения и сохраняет результат с ожидаемой ревизией.

		// b содержит ожидаемую ревизию и присутствующие поля PATCH назначения.
		b := i.Body
		if b.ExpectedRevision == 0 {
			return nil, core.ErrInvalid
		}
		// d содержит текущие настройки назначения для наложения частичного изменения; e содержит
		// отказ чтения.
		d, e := c.GetDestination(ctx, core.ID(i.ID))
		if e != nil {
			return nil, e
		}
		// stream хранит изменяемую строковую связь StreamID; group хранит изменяемую строковую
		// связь GroupID перед восстановлением доменных ID.
		stream, group := string(d.StreamID), string(d.GroupID)
		// apply содержит очередное частичное изменение назначения, сохраняющее отсутствующее
		// поле и отклоняющее null.
		for _, apply := range []func() error{func() error {
			// Переносит переданный StreamID во временное строковое значение и отклоняет null.
			return b.StreamID.apply(&stream)
		}, func() error {
			// Переносит переданный GroupID во временное строковое значение и отклоняет null.
			return b.GroupID.apply(&group)
		}, func() error {
			// Применяет переданное имя назначения, сохраняя прежнее при отсутствии поля.
			return b.Name.apply(&d.Name)
		}, func() error {
			// Применяет переданную платформу назначения и отклоняет null.
			return b.Platform.apply(&d.Platform)
		}, func() error {
			// Применяет переданный адрес назначения и отклоняет null.
			return b.Endpoint.apply(&d.Endpoint)
		}, func() error {
			// Применяет переданный ключ назначения, включая допустимую пустую строку, и отклоняет null.
			return b.StreamKey.apply(&d.StreamKey)
		}, func() error {
			// Применяет переданный признак Enabled, включая явное false, и отклоняет null.
			return b.Enabled.apply(&d.Enabled)
		}} {
			if e = apply(); e != nil {
				return nil, e
			}
		}
		d.StreamID = core.ID(stream)
		d.GroupID = core.ID(group)
		d, e = c.UpdateDestination(ctx, d, uint64(b.ExpectedRevision))
		return &Result[DestinationDTO]{destinationDTO(d)}, e
	})
	registerDelete(a, "/destinations/{id}", "delete-destination", c.DeleteDestination)
	register(a, operation("GET", "/transcode-profiles", "list-profiles", "Список профилей."), func(ctx context.Context, i *PageParams) (*Result[PageDTO[ProfileDTO]], error) {
		// Возвращает страницу сохранённых профилей с параметрами продолжения списка.

		// q содержит проверенные параметры страницы профилей; e содержит отказ курсора или
		// временного диапазона.
		q, e := i.request()
		if e != nil {
			return nil, e
		}
		// p содержит страницу сохранённых профилей.
		p, e := c.ListProfiles(ctx, q)
		return &Result[PageDTO[ProfileDTO]]{pageDTO(p, profileDTO)}, e
	})
	op = operation("POST", "/transcode-profiles", "create-profile", "Создать профиль.")
	op.DefaultStatus = 201
	register(a, op, func(ctx context.Context, i *ProfileCreate) (*Created[ProfileDTO], error) {
		// Преобразует входной профиль, создаёт его и возвращает адрес нового ресурса.

		// profile содержит проверенный полный доменный профиль; e содержит отказ преобразования
		// входных настроек.
		profile, e := i.Body.Profile.domain()
		if e != nil {
			return nil, e
		}
		// p содержит созданный именованный профиль.
		p, e := c.CreateProfile(ctx, core.TranscodeProfile{Name: i.Body.Name, Profile: profile})
		return &Created[ProfileDTO]{Prefix + "/transcode-profiles/" + string(p.ID), profileDTO(p)}, e
	})
	register(a, operation("GET", "/transcode-profiles/{id}", "get-profile", "Получить профиль."), func(ctx context.Context, i *IDInput) (*Result[ProfileDTO], error) {
		// Возвращает сохранённый профиль в публичном представлении.

		// p содержит сохранённый профиль; e содержит отказ чтения для GET.
		p, e := c.GetProfile(ctx, core.ID(i.ID))
		return &Result[ProfileDTO]{profileDTO(p)}, e
	})
	register(a, operation("PATCH", "/transcode-profiles/{id}", "update-profile", "Заменить переданные поля профиля по ревизии."), func(ctx context.Context, i *ProfilePatch) (*Result[ProfileDTO], error) {
		// Сохраняет переданное имя и полную замену профиля с ожидаемой ревизией, отклоняя null для профиля.

		// b содержит ожидаемую ревизию и присутствующие изменения имени или полного профиля.
		b := i.Body
		if b.ExpectedRevision == 0 {
			return nil, core.ErrInvalid
		}
		// p содержит текущий профиль для наложения PATCH; e содержит отказ его чтения.
		p, e := c.GetProfile(ctx, core.ID(i.ID))
		if e != nil {
			return nil, e
		}
		if e = b.Name.apply(&p.Name); e != nil {
			return nil, e
		}
		if b.Profile.Set {
			if b.Profile.Null {
				return nil, core.ErrInvalid
			}
			p.Profile, e = b.Profile.Value.domain()
			if e != nil {
				return nil, e
			}
		}
		p, e = c.UpdateProfile(ctx, p, uint64(b.ExpectedRevision))
		return &Result[ProfileDTO]{profileDTO(p)}, e
	})
	registerDelete(a, "/transcode-profiles/{id}", "delete-profile", c.DeleteProfile)
	register(a, operation("GET", "/transcode-groups", "list-groups", "Список групп."), func(ctx context.Context, i *GroupListInput) (*Result[PageDTO[GroupDTO]], error) {
		// Возвращает страницу групп с фильтрами потока и профиля.

		// q содержит проверенные параметры страницы групп; e содержит отказ списка или курсора.
		q, e := i.request()
		if e != nil {
			return nil, e
		}
		q.Query.StreamID = core.ID(i.StreamID)
		q.Query.ProfileID = core.ID(i.ProfileID)
		// p содержит страницу групп с фильтрами потока и профиля.
		p, e := c.ListGroups(ctx, q)
		return &Result[PageDTO[GroupDTO]]{pageDTO(p, groupDTO)}, e
	})
	op = operation("POST", "/transcode-groups", "create-group", "Создать группу.")
	op.DefaultStatus = 201
	register(a, op, func(ctx context.Context, i *GroupCreate) (*Created[GroupDTO], error) {
		// Создаёт группу с указанными связями потока и профиля и возвращает адрес ресурса.

		// g содержит созданную группу; e содержит отказ доменного создания с указанными связями.
		g, e := c.CreateGroup(ctx, core.TranscodeGroup{Name: i.Body.Name, StreamID: core.ID(i.Body.StreamID), ProfileID: core.ID(i.Body.ProfileID)})
		return &Created[GroupDTO]{Prefix + "/transcode-groups/" + string(g.ID), groupDTO(g)}, e
	})
	register(a, operation("GET", "/transcode-groups/{id}", "get-group", "Получить группу."), func(ctx context.Context, i *IDInput) (*Result[GroupDTO], error) {
		// Возвращает настройки группы по её идентификатору.

		// g содержит сохранённую группу; e содержит отказ чтения для GET.
		g, e := c.GetGroup(ctx, core.ID(i.ID))
		return &Result[GroupDTO]{groupDTO(g)}, e
	})
	register(a, operation("PATCH", "/transcode-groups/{id}", "update-group", "Изменить группу по ревизии."), func(ctx context.Context, i *GroupPatch) (*Result[GroupDTO], error) {
		// Применяет переданные имя и связи группы и сохраняет результат с ожидаемой ревизией.

		// b содержит ожидаемую ревизию и присутствующие изменения группы.
		b := i.Body
		if b.ExpectedRevision == 0 {
			return nil, core.ErrInvalid
		}
		// g содержит текущую группу для наложения PATCH; e содержит отказ её чтения.
		g, e := c.GetGroup(ctx, core.ID(i.ID))
		if e != nil {
			return nil, e
		}
		// stream хранит изменяемую строковую связь потока; profile хранит изменяемую строковую
		// связь профиля до восстановления доменных ID.
		stream, profile := string(g.StreamID), string(g.ProfileID)
		if e = b.Name.apply(&g.Name); e != nil {
			return nil, e
		}
		if e = b.StreamID.apply(&stream); e != nil {
			return nil, e
		}
		if e = b.ProfileID.apply(&profile); e != nil {
			return nil, e
		}
		g.StreamID = core.ID(stream)
		g.ProfileID = core.ID(profile)
		g, e = c.UpdateGroup(ctx, g, uint64(b.ExpectedRevision))
		return &Result[GroupDTO]{groupDTO(g)}, e
	})
	registerDelete(a, "/transcode-groups/{id}", "delete-group", c.DeleteGroup)
	register(a, operation("GET", "/streams/{id}/fallback-policy", "get-fallback-policy", "Получить политику заглушки."), func(ctx context.Context, i *IDInput) (*Result[PolicyResult], error) {
		// Возвращает политику заглушки вместе с текущей ревизией потока.

		// s содержит Stream для возврата его текущей политики и ревизии; e содержит отказ
		// чтения.
		s, e := c.GetStream(ctx, core.ID(i.ID))
		return &Result[PolicyResult]{PolicyResult{Decimal(s.Revision), policyDTO(s.Fallback)}}, e
	})
	register(a, operation("PUT", "/streams/{id}/fallback-policy", "set-fallback-policy", "Заменить или очистить политику заглушки."), func(ctx context.Context, i *PolicyPut) (*Result[PolicyResult], error) {
		// Полностью заменяет либо очищает политику заглушки с обязательным указанием ожидаемой ревизии.

		// b содержит ожидаемую ревизию и обязательную замену либо очистку политики.
		b := i.Body
		if b.ExpectedRevision == 0 || !b.Fallback.Set {
			return nil, core.ErrInvalid
		}
		// s содержит текущий Stream для изменения политики; e содержит отказ его чтения.
		s, e := c.GetStream(ctx, core.ID(i.ID))
		if e != nil {
			return nil, e
		}
		s.Fallback = core.FallbackPolicy{}
		if !b.Fallback.Null {
			s.Fallback, e = b.Fallback.Value.domain()
			if e != nil {
				return nil, e
			}
		}
		s, e = c.UpdateStream(ctx, s, uint64(b.ExpectedRevision))
		return &Result[PolicyResult]{PolicyResult{Decimal(s.Revision), policyDTO(s.Fallback)}}, e
	})
}

// registerDelete добавляет удаление по обязательной положительной десятичной ревизии и возвращает 204
// только после успешной доменной операции.
func registerDelete(a *API, path, id string, remove func(context.Context, core.ID, uint64) error) {
	// op задаёт удаление выбранного ресурса с успешным статусом 204 без тела.
	op := operation("DELETE", path, id, "Удалить объект по ревизии.")
	op.DefaultStatus = 204
	register(a, op, func(ctx context.Context, i *DeleteInput) (*struct{}, error) {
		// Разбирает ожидаемую ревизию и вызывает переданную операцию удаления без тела успешного ответа.

		// v содержит проверенную положительную ревизию удаления; e содержит отказ её
		// канонической записи.
		v, e := revision(i.ExpectedRevision)
		if e != nil {
			return nil, e
		}
		e = remove(ctx, core.ID(i.ID), v)
		return nil, e
	})
}
