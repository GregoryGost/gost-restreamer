package api

import (
	"context"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

type Result[T any] struct{ Body T }
type Created[T any] struct {
	Location string `header:"Location"`
	Body     T
}
type IDInput struct {
	ID string `path:"id" minLength:"1"`
}
type DeleteInput struct {
	IDInput
	ExpectedRevision string `query:"expected_revision" required:"true"`
}
type PageParams struct {
	Limit  int    `query:"limit" minimum:"1" maximum:"499" default:"100"`
	Cursor string `query:"cursor" maxLength:"32768"`
	From   string `query:"from"`
	To     string `query:"to"`
}

func (p PageParams) request() (core.PageRequest, error) {
	q := core.ListQuery{Limit: p.Limit}
	for i, s := range []string{p.From, p.To} {
		if s != "" {
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

type StreamListInput struct {
	PageParams
	AssetID string `query:"asset_id"`
}
type DestinationListInput struct {
	PageParams
	StreamID string `query:"stream_id"`
	GroupID  string `query:"group_id"`
}
type GroupListInput struct {
	PageParams
	StreamID  string `query:"stream_id"`
	ProfileID string `query:"profile_id"`
}
type SessionListInput struct {
	PageParams
	StreamID string `query:"stream_id"`
	State    string `query:"state" enum:"STARTING,RUNNING,STOPPING,STOPPED,FAILED,INTERRUPTED"`
}
type StreamSessionListInput struct {
	IDInput
	PageParams
	State string `query:"state" enum:"STARTING,RUNNING,STOPPING,STOPPED,FAILED,INTERRUPTED"`
}
type NestedListInput struct {
	IDInput
	PageParams
}

type StreamCreateBody struct {
	Name           string     `json:"name" minLength:"1"`
	PublicationKey string     `json:"publication_key" minLength:"1"`
	Fallback       *PolicyDTO `json:"fallback,omitempty"`
}
type StreamCreate struct{ Body StreamCreateBody }
type StreamPatchBody struct {
	ExpectedRevision Decimal        `json:"expected_revision"`
	Name             Field[string]  `json:"name,omitempty"`
	Fallback         NullablePolicy `json:"fallback,omitempty"`
}
type StreamPatch struct {
	IDInput
	Body StreamPatchBody
}
type DestinationCreateBody struct {
	StreamID  string `json:"stream_id" minLength:"1"`
	GroupID   string `json:"group_id" minLength:"1"`
	Name      string `json:"name" minLength:"1"`
	Platform  string `json:"platform" minLength:"1"`
	Endpoint  string `json:"endpoint" minLength:"1"`
	StreamKey string `json:"stream_key"`
	Enabled   bool   `json:"enabled"`
}
type DestinationCreate struct{ Body DestinationCreateBody }
type DestinationPatchBody struct {
	ExpectedRevision Decimal       `json:"expected_revision"`
	StreamID         Field[string] `json:"stream_id,omitempty"`
	GroupID          Field[string] `json:"group_id,omitempty"`
	Name             Field[string] `json:"name,omitempty"`
	Platform         Field[string] `json:"platform,omitempty"`
	Endpoint         Field[string] `json:"endpoint,omitempty"`
	StreamKey        Field[string] `json:"stream_key,omitempty"`
	Enabled          Field[bool]   `json:"enabled,omitempty"`
}
type DestinationPatch struct {
	IDInput
	Body DestinationPatchBody
}
type ProfileCreateBody struct {
	Name    string            `json:"name" minLength:"1"`
	Profile GroupProfileInput `json:"profile"`
}
type ProfileCreate struct{ Body ProfileCreateBody }
type ProfilePatchBody struct {
	ExpectedRevision Decimal                  `json:"expected_revision"`
	Name             Field[string]            `json:"name,omitempty"`
	Profile          Field[GroupProfileInput] `json:"profile,omitempty" doc:"Полная замена профиля. Отсутствующее поле сохраняет прежний профиль; новые настройки действуют со следующего запуска."`
}
type ProfilePatch struct {
	IDInput
	Body ProfilePatchBody
}
type GroupCreateBody struct {
	StreamID  string `json:"stream_id" minLength:"1"`
	ProfileID string `json:"profile_id" minLength:"1"`
	Name      string `json:"name" minLength:"1"`
}
type GroupCreate struct{ Body GroupCreateBody }
type GroupPatchBody struct {
	ExpectedRevision Decimal       `json:"expected_revision"`
	StreamID         Field[string] `json:"stream_id,omitempty"`
	ProfileID        Field[string] `json:"profile_id,omitempty"`
	Name             Field[string] `json:"name,omitempty"`
}
type GroupPatch struct {
	IDInput
	Body GroupPatchBody
}
type PolicyResult struct {
	Revision Decimal    `json:"revision"`
	Fallback *PolicyDTO `json:"fallback"`
}
type PolicyPut struct {
	IDInput
	Body struct {
		ExpectedRevision Decimal        `json:"expected_revision"`
		Fallback         NullablePolicy `json:"fallback"`
	}
}

func (a *API) registerConfiguration() {
	c := a.config.Core
	register(a, operation("GET", "/streams", "list-streams", "Список потоков."), func(ctx context.Context, i *StreamListInput) (*Result[PageDTO[StreamDTO]], error) {
		q, e := i.request()
		if e != nil {
			return nil, e
		}
		q.Query.AssetID = core.ID(i.AssetID)
		p, e := c.ListStreams(ctx, q)
		return &Result[PageDTO[StreamDTO]]{pageDTO(p, streamDTO)}, e
	})
	op := operation("POST", "/streams", "create-stream", "Создать поток.")
	op.DefaultStatus = 201
	register(a, op, func(ctx context.Context, i *StreamCreate) (*Created[StreamDTO], error) {
		v := core.Stream{Name: i.Body.Name, PublicationKey: i.Body.PublicationKey}
		if i.Body.Fallback != nil {
			p, e := i.Body.Fallback.domain()
			if e != nil {
				return nil, e
			}
			v.Fallback = p
		}
		s, e := c.CreateStream(ctx, v)
		return &Created[StreamDTO]{Prefix + "/streams/" + string(s.ID), streamDTO(s)}, e
	})
	register(a, operation("GET", "/streams/{id}", "get-stream", "Получить поток."), func(ctx context.Context, i *IDInput) (*Result[StreamDTO], error) {
		s, e := c.GetStream(ctx, core.ID(i.ID))
		return &Result[StreamDTO]{streamDTO(s)}, e
	})
	register(a, operation("PATCH", "/streams/{id}", "update-stream", "Изменить поток по ревизии."), func(ctx context.Context, i *StreamPatch) (*Result[StreamDTO], error) {
		if i.Body.ExpectedRevision == 0 {
			return nil, core.ErrInvalid
		}
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
		if _, e := c.GetStream(ctx, core.ID(i.ID)); e != nil {
			return nil, e
		}
		q, e := i.request()
		if e != nil {
			return nil, e
		}
		q.Query.StreamID = core.ID(i.ID)
		p, e := c.ListDestinations(ctx, q)
		return &Result[PageDTO[DestinationDTO]]{pageDTO(p, destinationDTO)}, e
	})
	register(a, operation("GET", "/streams/{id}/sessions", "list-stream-sessions", "История запусков потока."), func(ctx context.Context, i *StreamSessionListInput) (*Result[PageDTO[SessionDTO]], error) {
		if _, e := c.GetStream(ctx, core.ID(i.ID)); e != nil {
			return nil, e
		}
		q, e := i.request()
		if e != nil {
			return nil, e
		}
		q.Query.StreamID = core.ID(i.ID)
		q.Query.State = i.State
		p, e := c.ListSessions(ctx, q)
		return &Result[PageDTO[SessionDTO]]{pageDTO(p, sessionDTO)}, e
	})
	register(a, operation("GET", "/destinations", "list-destinations", "Список назначений."), func(ctx context.Context, i *DestinationListInput) (*Result[PageDTO[DestinationDTO]], error) {
		q, e := i.request()
		if e != nil {
			return nil, e
		}
		q.Query.StreamID = core.ID(i.StreamID)
		q.Query.GroupID = core.ID(i.GroupID)
		p, e := c.ListDestinations(ctx, q)
		return &Result[PageDTO[DestinationDTO]]{pageDTO(p, destinationDTO)}, e
	})
	op = operation("POST", "/destinations", "create-destination", "Создать назначение.")
	op.DefaultStatus = 201
	register(a, op, func(ctx context.Context, i *DestinationCreate) (*Created[DestinationDTO], error) {
		b := i.Body
		d, e := c.CreateDestination(ctx, core.Destination{StreamID: core.ID(b.StreamID), GroupID: core.ID(b.GroupID), Name: b.Name, Platform: b.Platform, Endpoint: b.Endpoint, StreamKey: b.StreamKey, Enabled: b.Enabled})
		return &Created[DestinationDTO]{Prefix + "/destinations/" + string(d.ID), destinationDTO(d)}, e
	})
	register(a, operation("GET", "/destinations/{id}", "get-destination", "Получить назначение."), func(ctx context.Context, i *IDInput) (*Result[DestinationDTO], error) {
		v, e := c.GetDestination(ctx, core.ID(i.ID))
		return &Result[DestinationDTO]{destinationDTO(v)}, e
	})
	register(a, operation("PATCH", "/destinations/{id}", "update-destination", "Изменить назначение по ревизии."), func(ctx context.Context, i *DestinationPatch) (*Result[DestinationDTO], error) {
		b := i.Body
		if b.ExpectedRevision == 0 {
			return nil, core.ErrInvalid
		}
		d, e := c.GetDestination(ctx, core.ID(i.ID))
		if e != nil {
			return nil, e
		}
		stream, group := string(d.StreamID), string(d.GroupID)
		for _, apply := range []func() error{func() error { return b.StreamID.apply(&stream) }, func() error { return b.GroupID.apply(&group) }, func() error { return b.Name.apply(&d.Name) }, func() error { return b.Platform.apply(&d.Platform) }, func() error { return b.Endpoint.apply(&d.Endpoint) }, func() error { return b.StreamKey.apply(&d.StreamKey) }, func() error { return b.Enabled.apply(&d.Enabled) }} {
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
		q, e := i.request()
		if e != nil {
			return nil, e
		}
		p, e := c.ListProfiles(ctx, q)
		return &Result[PageDTO[ProfileDTO]]{pageDTO(p, profileDTO)}, e
	})
	op = operation("POST", "/transcode-profiles", "create-profile", "Создать профиль.")
	op.DefaultStatus = 201
	register(a, op, func(ctx context.Context, i *ProfileCreate) (*Created[ProfileDTO], error) {
		profile, e := i.Body.Profile.domain()
		if e != nil {
			return nil, e
		}
		p, e := c.CreateProfile(ctx, core.TranscodeProfile{Name: i.Body.Name, Profile: profile})
		return &Created[ProfileDTO]{Prefix + "/transcode-profiles/" + string(p.ID), profileDTO(p)}, e
	})
	register(a, operation("GET", "/transcode-profiles/{id}", "get-profile", "Получить профиль."), func(ctx context.Context, i *IDInput) (*Result[ProfileDTO], error) {
		p, e := c.GetProfile(ctx, core.ID(i.ID))
		return &Result[ProfileDTO]{profileDTO(p)}, e
	})
	register(a, operation("PATCH", "/transcode-profiles/{id}", "update-profile", "Заменить переданные поля профиля по ревизии."), func(ctx context.Context, i *ProfilePatch) (*Result[ProfileDTO], error) {
		b := i.Body
		if b.ExpectedRevision == 0 {
			return nil, core.ErrInvalid
		}
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
		q, e := i.request()
		if e != nil {
			return nil, e
		}
		q.Query.StreamID = core.ID(i.StreamID)
		q.Query.ProfileID = core.ID(i.ProfileID)
		p, e := c.ListGroups(ctx, q)
		return &Result[PageDTO[GroupDTO]]{pageDTO(p, groupDTO)}, e
	})
	op = operation("POST", "/transcode-groups", "create-group", "Создать группу.")
	op.DefaultStatus = 201
	register(a, op, func(ctx context.Context, i *GroupCreate) (*Created[GroupDTO], error) {
		g, e := c.CreateGroup(ctx, core.TranscodeGroup{Name: i.Body.Name, StreamID: core.ID(i.Body.StreamID), ProfileID: core.ID(i.Body.ProfileID)})
		return &Created[GroupDTO]{Prefix + "/transcode-groups/" + string(g.ID), groupDTO(g)}, e
	})
	register(a, operation("GET", "/transcode-groups/{id}", "get-group", "Получить группу."), func(ctx context.Context, i *IDInput) (*Result[GroupDTO], error) {
		g, e := c.GetGroup(ctx, core.ID(i.ID))
		return &Result[GroupDTO]{groupDTO(g)}, e
	})
	register(a, operation("PATCH", "/transcode-groups/{id}", "update-group", "Изменить группу по ревизии."), func(ctx context.Context, i *GroupPatch) (*Result[GroupDTO], error) {
		b := i.Body
		if b.ExpectedRevision == 0 {
			return nil, core.ErrInvalid
		}
		g, e := c.GetGroup(ctx, core.ID(i.ID))
		if e != nil {
			return nil, e
		}
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
		s, e := c.GetStream(ctx, core.ID(i.ID))
		return &Result[PolicyResult]{PolicyResult{Decimal(s.Revision), policyDTO(s.Fallback)}}, e
	})
	register(a, operation("PUT", "/streams/{id}/fallback-policy", "set-fallback-policy", "Заменить или очистить политику заглушки."), func(ctx context.Context, i *PolicyPut) (*Result[PolicyResult], error) {
		b := i.Body
		if b.ExpectedRevision == 0 || !b.Fallback.Set {
			return nil, core.ErrInvalid
		}
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
func registerDelete(a *API, path, id string, remove func(context.Context, core.ID, uint64) error) {
	op := operation("DELETE", path, id, "Удалить объект по ревизии.")
	op.DefaultStatus = 204
	register(a, op, func(ctx context.Context, i *DeleteInput) (*struct{}, error) {
		v, e := revision(i.ExpectedRevision)
		if e != nil {
			return nil, e
		}
		e = remove(ctx, core.ID(i.ID), v)
		return nil, e
	})
}
