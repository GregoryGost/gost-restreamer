package storage

import (
	"context"
	"reflect"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// Проверяет выбор готовых привязок только выбранных групп и возврат прежней квитанции до повторной
// оценки изменившейся готовности.
func TestStartBindingSelectionUsesSelectedGroupsAndExactCurrentProfile(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 2)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// p содержит полный именованный профиль для проверяемого изменения.
	p := f.profile
	p.ID, p.Name = "", "Неподготовленный профиль"
	p.Profile.VideoEncoding.GOPFrames++
	// err сообщает об ошибке сохранения полного именованного профиля.
	p, err := f.service.CreateProfile(ctx, p)
	if err != nil {
		t.Fatal(err)
	}
	// g содержит конфигурацию группы тестового потока.
	g, err := f.service.CreateGroup(ctx, core.TranscodeGroup{StreamID: f.stream.ID, ProfileID: p.ID, Name: "Невыбранная группа"})
	if err != nil {
		t.Fatal(err)
	}
	// d содержит настройки проверяемого назначения.
	d, err := f.service.CreateDestination(ctx, core.Destination{StreamID: f.stream.ID, GroupID: g.ID, Name: "Отключённое назначение", Platform: "custom", Endpoint: "rtmp://example.invalid/disabled"})
	if err != nil {
		t.Fatal(err)
	}
	// bindings содержит точные варианты уникальных групп выбранного начального набора назначений.
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
	// request сохраняет первоначальный выбор назначений для проверки тождественного повтора.
	request := f.startRequest("accepted-selection")
	request.Bindings = bindings
	// receipt содержит сохраняемую квитанцию выбранной команды.
	receipt, err := f.service.StartSession(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	// changed содержит копию текущего профиля с изменённым GOP для проверки устаревшей готовности.
	changed := f.profile
	changed.Profile.VideoEncoding.GOPFrames++
	// err сообщает об ошибке условного изменения полного профиля.
	if _, err := f.service.UpdateProfile(ctx, changed, changed.Revision); err != nil {
		t.Fatal(err)
	}
	_, err = f.service.SelectStartBindings(ctx, f.stream.ID, request.DestinationIDs)
	requireDomainError(t, err, core.ErrNotReady)
	request.Bindings = nil
	request.DestinationIDs[0], request.DestinationIDs[1] = request.DestinationIDs[1], request.DestinationIDs[0]
	// prior содержит прежнюю квитанцию тождественного RequestID. found показывает наличие прежней
	// квитанции тождественного запроса.
	prior, found, err := f.service.PriorStartSession(ctx, request)
	if err != nil || !found || !reflect.DeepEqual(prior, receipt) {
		t.Fatalf("чтение RequestID переоценило текущую готовность: %+v, %t, %v", prior, found, err)
	}
	request.DestinationIDs = request.DestinationIDs[:1]
	_, _, err = f.service.PriorStartSession(ctx, request)
	requireDomainError(t, err, core.ErrConflict)
	request.RequestID = "unaccepted-selection"
	// found показывает наличие прежней квитанции тождественного запроса. err сообщает об ошибке
	// поиска тождественной прежней квитанции запуска.
	if _, found, err := f.service.PriorStartSession(ctx, request); err != nil || found {
		t.Fatalf("несуществующий RequestID найден: %t, %v", found, err)
	}
}

// Проверяет выбор привязки по политике Session и независимый от текущего профиля повтор принятого
// AddOutput при точной идентичности запроса.
func TestAddBindingSelectionUsesSavedPolicyAndPriorRequestIdentity(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 1)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// start содержит принятую квитанцию первоначального запуска сессии.
	start := startDomainSession(t, f)
	executeCommand(t, f.service, start.RequestID)
	// a содержит стабильную заглушку проверяемого поколения. err сообщает об ошибке создания
	// стабильной заглушки.
	a, err := f.service.CreateAsset(ctx, "Будущая заглушка")
	if err != nil {
		t.Fatal(err)
	}
	// stream содержит конфигурацию потока с постоянным ключом и выбранной заглушкой.
	stream := f.stream
	stream.Fallback.AssetID = a.ID
	// err сообщает об ошибке условного изменения конфигурации потока.
	if _, err := f.service.UpdateStream(ctx, stream, stream.Revision); err != nil {
		t.Fatal(err)
	}
	// d содержит настройки проверяемого назначения.
	d, err := f.service.CreateDestination(ctx, core.Destination{StreamID: f.stream.ID, GroupID: f.group.ID, Name: "Новый выход", Platform: "custom", Endpoint: "rtmp://example.invalid/new"})
	if err != nil {
		t.Fatal(err)
	}
	// binding содержит точный вариант группы добавляемого назначения по политике Session.
	binding, err := f.service.SelectAddBinding(ctx, start.SessionID, d.ID)
	if err != nil || binding != f.startRequest("").Bindings[0] {
		t.Fatalf("Add выбрал будущую policy вместо сохранённой: %+v, %v", binding, err)
	}
	// request задаёт добавление выбранного назначения с точной привязкой варианта.
	request := core.AddOutputRequest{RequestID: "add-selected-binding", SessionID: start.SessionID, DestinationID: d.ID, Binding: binding}
	// receipt содержит сохраняемую квитанцию выбранной команды.
	receipt, err := f.service.AddOutput(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	// changed содержит копию текущего профиля с изменённым GOP для проверки устаревшей готовности.
	changed := f.profile
	changed.Profile.VideoEncoding.GOPFrames++
	// err сообщает об ошибке условного изменения полного профиля.
	if _, err := f.service.UpdateProfile(ctx, changed, changed.Revision); err != nil {
		t.Fatal(err)
	}
	_, err = f.service.SelectAddBinding(ctx, start.SessionID, d.ID)
	requireDomainError(t, err, core.ErrNotReady)
	request.Binding = core.PreparedBinding{}
	// prior содержит прежнюю квитанцию тождественного RequestID. found показывает наличие прежней
	// квитанции тождественного запроса.
	prior, found, err := f.service.PriorAddOutput(ctx, request)
	if err != nil || !found || !reflect.DeepEqual(prior, receipt) {
		t.Fatalf("повтор Add переоценил bindings: %+v, %t, %v", prior, found, err)
	}
	request.DestinationID = f.destinations[0].ID
	_, _, err = f.service.PriorAddOutput(ctx, request)
	requireDomainError(t, err, core.ErrConflict)
}
