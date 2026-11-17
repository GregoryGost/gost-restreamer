package storage

import (
	"bytes"
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// currentEncodingState сохраняет профили и снимки, созданные текущим рабочим кодом.
// Эти значения позволяют независимо сверять историю после изменения настроек и Recover.
type currentEncodingState struct {
	// ProfileID адресует полный AV1/copy-профиль тестового потока.
	ProfileID core.ID
	// TranscodeProfileID адресует отдельный H264/transcode-профиль с явным LIVE transcode.
	TranscodeProfileID core.ID
	// StartRequest содержит первоначальный запрос для проверки идемпотентного повтора.
	StartRequest core.StartSessionRequest
	// Receipt содержит исходную квитанцию без зависимости от дальнейшего состояния команды.
	Receipt core.CommandReceipt
	// Snapshots содержит точные байты самостоятельных Session и Output снимков.
	Snapshots map[string][]byte
	// Immutable хранит исходные payload неизменяемых конфигураций для проверки чтения и открытия.
	Immutable map[string]map[string][]byte
}

// openCurrentEncodingState создаёт в каталоге t новую БД и пригодное поколение полного профиля.
// Возвращает Store, Service, текущие эталоны истории и путь для повторного открытия.
// Зарегистрированная очистка закрывает исходный Store; повторный пул закрывает вызывающий тест.
func openCurrentEncodingState(t *testing.T) (*Store, *core.Service, currentEncodingState, string) {
	t.Helper()
	// fixture содержит согласованные текущие конфигурации и опубликованный AAC-вариант.
	fixture := newDomainFixture(t, 1)
	// ctx ограничивает операции изолированной базой.
	ctx := context.Background()
	// transcode задаёт самостоятельный полный H264-профиль с явно выбранной обработкой аудио.
	transcode := storageProfile()
	transcode.Mode, transcode.LiveAudioMode = core.ModeTranscode, core.LiveAudioTranscode
	transcode.Format.Video.Codec, transcode.Format.Video.CodecProfile = core.CodecH264, "high"
	transcode.Encoding = core.DefaultEncoding(core.ModeTranscode, core.VideoEncoderNVENC, core.RateControlVBR)
	// named и err фиксируют отдельный профиль, который можно менять без подмены снимков потока.
	named, err := fixture.service.CreateProfile(ctx, core.TranscodeProfile{Name: "Полный H264-профиль", Profile: transcode})
	if err != nil {
		t.Fatal(err)
	}
	// group связывает H264-профиль с тем же потоком для независимого снимка второго выхода.
	group, err := fixture.service.CreateGroup(ctx, core.TranscodeGroup{StreamID: fixture.stream.ID, ProfileID: named.ID, Name: "Группа H264"})
	if err != nil {
		t.Fatal(err)
	}
	// destination добавляет второй выход, чтобы исторические снимки включали оба полных профиля.
	destination, err := fixture.service.CreateDestination(ctx, core.Destination{StreamID: fixture.stream.ID, GroupID: group.ID, Name: "Выход H264", Platform: "custom", Endpoint: "rtmp://example.invalid/live", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	fixture.asset, _ = publishFixtureGeneration(t, fixture.service, fixture.asset, "current-source", "current-content")
	// bindings выбирает полный опубликованный комплект для обеих групп без внешней работы.
	bindings, err := fixture.service.SelectStartBindings(ctx, fixture.stream.ID, nil)
	if err != nil {
		t.Fatal(err)
	}
	// request содержит первоначальные назначения и точные привязки обоих опубликованных вариантов.
	request := core.StartSessionRequest{RequestID: "snapshot-profile-start", StreamID: fixture.stream.ID, DestinationIDs: []core.ID{fixture.destinations[0].ID, destination.ID}, Bindings: bindings}
	// receipt создаёт самостоятельные сохраняемые снимки без запуска внешних процессов.
	receipt, err := fixture.service.StartSession(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	// state собирает независимые байтовые эталоны до дальнейших изменений текущих настроек.
	state := currentEncodingState{ProfileID: fixture.profile.ID, TranscodeProfileID: named.ID, StartRequest: request, Receipt: receipt, Snapshots: map[string][]byte{}, Immutable: map[string]map[string][]byte{}}
	// session содержит снимок первоначального запуска, сохраняемый отдельно от конфигураций.
	session := readSession(t, fixture.store, receipt.SessionID)
	state.Snapshots["session:"+string(session.ID)], err = core.EncodeSessionSnapshot(session.Snapshot)
	if err != nil {
		t.Fatal(err)
	}
	// output содержит самостоятельный снимок назначения и его артефакта.
	for _, output := range readOutputs(t, fixture.store, receipt.SessionID) {
		state.Snapshots["output:"+string(output.ID)], err = core.EncodeOutputSnapshot(output.Snapshot)
		if err != nil {
			t.Fatal(err)
		}
	}
	// table и records перебирают только исходные конфигурации, не изменяемые Recover.
	for table, records := range captureSQLRows(t, fixture.store.sqlDB) {
		if table != "profiles" && table != "streams" && table != "groups" && table != "destinations" {
			continue
		}
		state.Immutable[table] = map[string][]byte{}
		// row содержит все колонки конфигурации; payload расположен после Record.
		for _, row := range records {
			state.Immutable[table][row[0].(string)] = bytes.Clone(row[4].([]byte))
		}
	}
	return fixture.store, fixture.service, state, filepath.Clean(strings.TrimSuffix(fixture.store.lock.Name(), ".lock"))
}

// assertCurrentEncodingSnapshots сверяет байты всех исходных снимков state через текущий Service svc.
// t получает ошибку чтения или изменение самостоятельного исторического значения.
func assertCurrentEncodingSnapshots(t *testing.T, svc *core.Service, state currentEncodingState) {
	t.Helper()
	// key обозначает вид снимка и ID, want содержит исходную полную форму.
	for key, want := range state.Snapshots {
		// got содержит повторно закодированный снимок; err сообщает отказ чтения или кодирования.
		var got []byte
		var err error
		// id выделяет владельца Session, ok отличает его от Output.
		if id, ok := strings.CutPrefix(key, "session:"); ok {
			// value содержит полную Session с неизменяемым снимком.
			var value core.Session
			value, err = svc.GetSession(context.Background(), core.ID(id))
			if err == nil {
				got, err = core.EncodeSessionSnapshot(value.Snapshot)
			}
		} else {
			// value содержит самостоятельный снимок выхода, чей префикс формирует только helper.
			var value core.SessionOutput
			value, err = svc.GetOutput(context.Background(), core.ID(strings.TrimPrefix(key, "output:")))
			if err == nil {
				got, err = core.EncodeOutputSnapshot(value.Snapshot)
			}
		}
		if err != nil || !bytes.Equal(got, want) {
			t.Fatalf("Снимок %s изменился: %v", key, err)
		}
	}
}

// assertCurrentEncodingRows требует точных payload исходных конфигураций state в пуле s.
// t завершает проверку при ошибке чтения или неявной записи во время открытия.
func assertCurrentEncodingRows(t *testing.T, s *Store, state currentEncodingState) {
	t.Helper()
	// table и values адресуют сохранённые конфигурации из фиксированного helper перечня.
	for table, values := range state.Immutable {
		// id адресует исходную строку; want содержит её полную оболочку метаданных.
		for id, want := range values {
			// got принимает точный payload без назначения значений по умолчанию.
			var got []byte
			// err сообщает об отказе чтения исходной строки.
			if err := s.sqlDB.QueryRow("SELECT payload FROM "+table+" WHERE id=?", id).Scan(&got); err != nil || !bytes.Equal(got, want) {
				t.Fatalf("Неявно изменена конфигурация %s/%s: %v", table, id, err)
			}
		}
	}
}
