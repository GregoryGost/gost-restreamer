package storage

import (
	"context"
	"reflect"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

func TestStartBindingSelectionUsesSelectedGroupsAndExactCurrentProfile(t *testing.T) {
	f := newDomainFixture(t, 2)
	ctx := context.Background()
	p := f.profile
	p.ID, p.Name = "", "Неподготовленный профиль"
	p.Profile.VideoEncoding.GOPFrames++
	p, err := f.service.CreateProfile(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	g, err := f.service.CreateGroup(ctx, core.TranscodeGroup{StreamID: f.stream.ID, ProfileID: p.ID, Name: "Невыбранная группа"})
	if err != nil {
		t.Fatal(err)
	}
	d, err := f.service.CreateDestination(ctx, core.Destination{StreamID: f.stream.ID, GroupID: g.ID, Name: "Отключённое назначение", Platform: "custom", Endpoint: "rtmp://example.invalid/disabled"})
	if err != nil {
		t.Fatal(err)
	}
	bindings, err := f.service.SelectStartBindings(ctx, f.stream.ID, nil)
	if err != nil || len(bindings) != 1 || bindings[0] != f.startRequest("").Bindings[0] {
		t.Fatalf("неверный выбор enabled групп: %+v, %v", bindings, err)
	}
	_, err = f.service.SelectStartBindings(ctx, f.stream.ID, []core.ID{d.ID})
	requireDomainError(t, err, core.ErrNotReady)
	_, err = f.service.SelectStartBindings(ctx, f.stream.ID, []core.ID{"missing-destination"})
	requireDomainError(t, err, core.ErrNotFound)
	_, err = f.service.SelectStartBindings(ctx, f.stream.ID, []core.ID{f.destinations[0].ID, f.destinations[0].ID})
	requireDomainError(t, err, core.ErrInvalid)
	request := f.startRequest("accepted-selection")
	request.Bindings = bindings
	receipt, err := f.service.StartSession(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	changed := f.profile
	changed.Profile.VideoEncoding.GOPFrames++
	if _, err := f.service.UpdateProfile(ctx, changed, changed.Revision); err != nil {
		t.Fatal(err)
	}
	_, err = f.service.SelectStartBindings(ctx, f.stream.ID, request.DestinationIDs)
	requireDomainError(t, err, core.ErrNotReady)
	request.Bindings = nil
	request.DestinationIDs[0], request.DestinationIDs[1] = request.DestinationIDs[1], request.DestinationIDs[0]
	prior, found, err := f.service.PriorStartSession(ctx, request)
	if err != nil || !found || !reflect.DeepEqual(prior, receipt) {
		t.Fatalf("чтение RequestID переоценило текущую готовность: %+v, %t, %v", prior, found, err)
	}
	request.DestinationIDs = request.DestinationIDs[:1]
	_, _, err = f.service.PriorStartSession(ctx, request)
	requireDomainError(t, err, core.ErrConflict)
	request.RequestID = "unaccepted-selection"
	if _, found, err := f.service.PriorStartSession(ctx, request); err != nil || found {
		t.Fatalf("несуществующий RequestID найден: %t, %v", found, err)
	}
}

func TestAddBindingSelectionUsesSavedPolicyAndPriorRequestIdentity(t *testing.T) {
	f := newDomainFixture(t, 1)
	ctx := context.Background()
	start := startDomainSession(t, f)
	executeCommand(t, f.service, start.RequestID)
	a, err := f.service.CreateAsset(ctx, "Будущая заглушка")
	if err != nil {
		t.Fatal(err)
	}
	stream := f.stream
	stream.Fallback.AssetID = a.ID
	if _, err := f.service.UpdateStream(ctx, stream, stream.Revision); err != nil {
		t.Fatal(err)
	}
	d, err := f.service.CreateDestination(ctx, core.Destination{StreamID: f.stream.ID, GroupID: f.group.ID, Name: "Новый выход", Platform: "custom", Endpoint: "rtmp://example.invalid/new"})
	if err != nil {
		t.Fatal(err)
	}
	binding, err := f.service.SelectAddBinding(ctx, start.SessionID, d.ID)
	if err != nil || binding != f.startRequest("").Bindings[0] {
		t.Fatalf("Add выбрал будущую policy вместо сохранённой: %+v, %v", binding, err)
	}
	request := core.AddOutputRequest{RequestID: "add-selected-binding", SessionID: start.SessionID, DestinationID: d.ID, Binding: binding}
	receipt, err := f.service.AddOutput(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	changed := f.profile
	changed.Profile.VideoEncoding.GOPFrames++
	if _, err := f.service.UpdateProfile(ctx, changed, changed.Revision); err != nil {
		t.Fatal(err)
	}
	_, err = f.service.SelectAddBinding(ctx, start.SessionID, d.ID)
	requireDomainError(t, err, core.ErrNotReady)
	request.Binding = core.PreparedBinding{}
	prior, found, err := f.service.PriorAddOutput(ctx, request)
	if err != nil || !found || !reflect.DeepEqual(prior, receipt) {
		t.Fatalf("повтор Add переоценил bindings: %+v, %t, %v", prior, found, err)
	}
	request.DestinationID = f.destinations[0].ID
	_, _, err = f.service.PriorAddOutput(ctx, request)
	requireDomainError(t, err, core.ErrConflict)
}
