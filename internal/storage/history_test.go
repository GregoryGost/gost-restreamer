package storage

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

func historyFixture(t *testing.T) (*Store, *core.Service, time.Time) {
	t.Helper()
	s := openTestStore(t)
	svc, err := core.NewService(s, s.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	base := time.Date(2026, 3, 2, 12, 0, 0, 0, time.UTC)
	err = s.Write(context.Background(), func(r core.Repositories) error {
		for i, id := range []core.ID{"a", "b", "c", "d", "e"} {
			created := base
			if i > 2 {
				created = base.Add(time.Duration(i-2) * time.Microsecond)
			}
			value := core.Stream{Record: core.Record{ID: id, Revision: 1, CreatedAt: created, UpdatedAt: created}, Name: "Поток", PublicationKey: "synthetic-key-" + string(id)}
			if err := r.Config.InsertStream(context.Background(), value); err != nil {
				return err
			}
		}
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	return s, svc, base
}

func TestCursorUsesStableTieBreakAndExactKindAndFilters(t *testing.T) {
	_, svc, base := historyFixture(t)
	ctx := context.Background()
	to := base.Add(2 * time.Microsecond)
	request := core.PageRequest{Query: core.ListQuery{Limit: 2, From: &base, To: &to}}
	first, err := svc.ListStreams(ctx, request)
	if err != nil || len(first.Items) != 2 || first.Items[0].ID != "a" || first.Items[1].ID != "b" || first.NextCursor == "" {
		t.Fatalf("первая страница: %+v, %v", first, err)
	}
	request.Cursor = first.NextCursor
	second, err := svc.ListStreams(ctx, request)
	if err != nil || len(second.Items) != 2 || second.Items[0].ID != "c" || second.Items[1].ID != "d" || second.NextCursor != "" {
		t.Fatalf("вторая страница: %+v, %v", second, err)
	}
	_, err = svc.ListProfiles(ctx, request)
	requireDomainError(t, err, core.ErrInvalidCursor)
	changed := request
	changed.Query.To = nil
	_, err = svc.ListStreams(ctx, changed)
	requireDomainError(t, err, core.ErrInvalidCursor)
	zone := time.FixedZone("offset", 3*60*60)
	fromZone, toZone := base.In(zone), to.In(zone)
	request.Query.From, request.Query.To = &fromZone, &toZone
	repeated, err := svc.ListStreams(ctx, request)
	if err != nil || !reflect.DeepEqual(second, repeated) {
		t.Fatalf("тот же диапазон в другой зоне изменил страницу: %+v, %v", repeated, err)
	}
	for _, invalid := range []string{"%", "e30", first.NextCursor + "!"} {
		request.Cursor = invalid
		_, err = svc.ListStreams(ctx, request)
		requireDomainError(t, err, core.ErrInvalidCursor)
	}
	data, err := base64.RawURLEncoding.DecodeString(first.NextCursor)
	if err != nil {
		t.Fatal(err)
	}
	var cursor map[string]json.RawMessage
	if err := json.Unmarshal(data, &cursor); err != nil {
		t.Fatal(err)
	}
	cursor["Version"] = json.RawMessage("999")
	data, err = json.Marshal(cursor)
	if err != nil {
		t.Fatal(err)
	}
	request.Cursor = base64.RawURLEncoding.EncodeToString(data)
	_, err = svc.ListStreams(ctx, request)
	requireDomainError(t, err, core.ErrInvalidCursor)
}

func TestDateRangesPreserveSubmicrosecondExclusiveBoundaries(t *testing.T) {
	s, svc, base := historyFixture(t)
	ctx := context.Background()
	from := base.Add(time.Nanosecond)
	to := base.Add(time.Microsecond + time.Nanosecond)
	for _, boundary := range []struct {
		name     string
		from, to *time.Time
		ids      []core.ID
	}{
		{"from", &from, nil, []core.ID{"d", "e"}},
		{"to", nil, &to, []core.ID{"a", "b", "c", "d"}},
		{"both", &from, &to, []core.ID{"d"}},
	} {
		t.Run(boundary.name, func(t *testing.T) {
			query := core.ListQuery{Limit: 10, From: boundary.from, To: boundary.to}
			var values []core.Stream
			if err := s.View(ctx, func(r core.Repositories) error { var err error; values, err = r.Config.Streams(ctx, query); return err }); err != nil {
				t.Fatal(err)
			}
			ids := make([]core.ID, 0, len(values))
			for _, value := range values {
				ids = append(ids, value.ID)
			}
			if !reflect.DeepEqual(ids, boundary.ids) {
				t.Fatalf("repository [from,to) = %v, нужно %v", ids, boundary.ids)
			}
			page, err := svc.ListStreams(ctx, core.PageRequest{Query: query})
			if err != nil {
				t.Fatal(err)
			}
			ids = ids[:0]
			for _, value := range page.Items {
				ids = append(ids, value.ID)
			}
			if !reflect.DeepEqual(ids, boundary.ids) {
				t.Fatalf("service [from,to) = %v, нужно %v", ids, boundary.ids)
			}
		})
	}
}

func TestListRejectsUnboundedAndInvalidQueries(t *testing.T) {
	_, svc, base := historyFixture(t)
	for _, query := range []core.ListQuery{{Limit: 0}, {Limit: core.MaxListLimit + 1}, {Limit: 1, From: &base, To: &base}, {Limit: 1, After: &core.ListBoundary{CreatedAt: base, ID: "a"}}, {Limit: 1, StreamID: "unsupported-filter"}} {
		_, err := svc.ListStreams(context.Background(), core.PageRequest{Query: query})
		if !errors.Is(err, core.ErrInvalid) {
			t.Fatalf("неограниченный или недопустимый фильтр принят: %+v, %v", query, err)
		}
	}
}

func TestAuthorizeOBSUsesActiveSessionAndStopsAdmission(t *testing.T) {
	f := newDomainFixture(t, 0)
	ctx := context.Background()
	start := startDomainSession(t, f)
	if _, err := f.service.AuthorizeOBS(ctx, f.stream.ID, f.stream.PublicationKey); err == nil {
		t.Fatal("OBS принят до RUNNING")
	}
	executeCommand(t, f.service, start.RequestID)
	if _, err := f.service.AuthorizeOBS(ctx, f.stream.ID, "wrong-key"); err == nil {
		t.Fatal("OBS принят с чужим ключом")
	}
	if _, err := f.service.PauseSession(ctx, core.SessionCommand{RequestID: "pause-for-admission", SessionID: start.SessionID}); err != nil {
		t.Fatal(err)
	}
	authorized, err := f.service.AuthorizeOBS(ctx, f.stream.ID, f.stream.PublicationKey)
	if err != nil || authorized.ID != start.SessionID {
		t.Fatalf("пауза отозвала допустимую публикацию: %+v, %v", authorized, err)
	}
	if _, err := f.service.StopSession(ctx, core.SessionCommand{RequestID: "stop-admission", SessionID: start.SessionID}); err != nil {
		t.Fatal(err)
	}
	if _, err := f.service.AuthorizeOBS(ctx, f.stream.ID, f.stream.PublicationKey); err == nil {
		t.Fatal("принятый stop не отозвал OBS admission")
	}
}
