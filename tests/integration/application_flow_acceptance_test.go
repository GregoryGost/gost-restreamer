package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/api"
	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
	"github.com/bluenviron/gortmplib"
	"github.com/bluenviron/gortmplib/pkg/amf0"
	"github.com/bluenviron/gortmplib/pkg/message"
)

// Выполняет JSON-запрос через настоящий API, проверяет ожидаемый статус и переводит ответ в заданный
// тип T. Ошибка кодирования или разбора ответа завершает тест.
func applicationFlowRequest[T any](t *testing.T, a http.Handler, method, path string, body any, status int) T {
	t.Helper()
	// encoded содержит JSON входного значения, затем JSON полученного объекта для преобразования в
	// целевой T.
	var encoded []byte
	// err переносит отказы сериализации запроса и преобразования ответа в целевой тип.
	var err error
	if body != nil {
		encoded, err = json.Marshal(body)
		if err != nil {
			t.Fatal(err)
		}
	}
	// result содержит объект реального HTTP-ответа после проверки ожидаемого статуса.
	result := integrationAPIRequest(t, a, method, path, string(encoded), status)
	encoded, err = json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	// value принимает типизированный ответ T после повторного JSON-декодирования.
	var value T
	if err = json.Unmarshal(encoded, &value); err != nil {
		t.Fatal(err)
	}
	return value
}

// Передаёт байты изображения через HTTP-загрузку с текущей ревизией Asset и проверяет ответ 202,
// идентичность поколения и заголовок Location.
func applicationFlowUpload(t *testing.T, a http.Handler, asset api.AssetDTO, data []byte) api.PreparationDTO {
	t.Helper()
	// r содержит PUT изображения с точной текущей ревизией Asset.
	r := httptest.NewRequest("PUT", fmt.Sprintf("%s/fallback-assets/%s/content?kind=image&expected_revision=%d", api.Prefix, asset.ID, asset.Revision), bytes.NewReader(data))
	r.Header.Set("Content-Type", "application/octet-stream")
	// w сохраняет реальный ответ загрузки для проверки 202 и Location.
	w := httptest.NewRecorder()
	a.ServeHTTP(w, r)
	if w.Code != 202 {
		t.Fatalf("загрузка: status=%d body=%s", w.Code, w.Body)
	}
	// v принимает идентификаторы принятого поколения из ответа загрузки.
	var v api.PreparationDTO
	// err содержит отказ разбора ответа загрузки в PreparationDTO.
	if err := json.Unmarshal(w.Body.Bytes(), &v); err != nil {
		t.Fatal(err)
	}
	if v.AssetID != asset.ID || v.GenerationID == "" || w.Header().Get("Location") != api.Prefix+"/fallback-assets/"+asset.ID+"/generations/"+v.GenerationID {
		t.Fatal("неверная квитанция загрузки", v, w.Header())
	}
	return v
}

// Ожидает через HTTP заданное состояние поколения. Неожиданное состояние error завершает тест вместо
// ожидания успешной подготовки.
func applicationFlowGeneration(t *testing.T, a http.Handler, upload api.PreparationDTO, status string) api.GenerationDTO {
	t.Helper()
	// v сохраняет последний публичный результат поколения во время ожидания заданной стадии.
	var v api.GenerationDTO
	sessionAcceptanceWait(t, func() bool {
		// Ожидает выбранный результат поколения через HTTP, завершая тест при неожиданной ошибке подготовки.

		v = applicationFlowRequest[api.GenerationDTO](t, a, "GET", "/fallback-assets/"+upload.AssetID+"/generations/"+upload.GenerationID, nil, 200)
		if v.Status == "error" && status != "error" {
			t.Fatalf("подготовка завершилась ошибкой: %+v", v)
		}
		return v.Status == status
	})
	return v
}

// Ожидает успешного завершения HTTP-команды по её квитанции. При неуспешном или прерванном завершении
// сразу останавливает тест с диагностикой.
func applicationFlowReceipt(t *testing.T, a http.Handler, r api.ReceiptDTO) api.ReceiptDTO {
	t.Helper()
	// v сохраняет последнюю долговечную квитанцию HTTP-команды при ожидании успеха.
	var v api.ReceiptDTO
	sessionAcceptanceWait(t, func() bool {
		// Ожидает успешную долговечную квитанцию команды и сообщает её терминальную ошибку.

		v = applicationFlowRequest[api.ReceiptDTO](t, a, "GET", "/commands/"+r.ID, nil, 200)
		if v.Status == string(core.CommandFailed) || v.Status == string(core.CommandInterrupted) {
			if v.Failure != nil {
				t.Fatalf("команда завершилась ошибкой: %+v; failure=%+v", v, *v.Failure)
			}
			t.Fatalf("команда завершилась ошибкой: %+v", v)
		}
		return v.Status == string(core.CommandSucceeded)
	})
	return v
}

// Ожидает публичный снимок с тремя готовыми группами, у каждой из которых выбран заданный источник.
func applicationFlowSource(t *testing.T, a http.Handler, sessionID, source string) {
	t.Helper()
	// snapshot сохраняет последний оперативный снимок Session для проверки трёх готовых групп и
	// диагностики.
	var snapshot api.SessionMetricsDTO
	sessionAcceptanceWait(t, func() bool {
		// Проверяет, что все три группы готовы и используют ожидаемый источник.

		snapshot = applicationFlowRequest[api.SessionMetricsDTO](t, a, "GET", "/sessions/"+sessionID+"/metrics", nil, 200)
		if snapshot.Snapshot == nil || len(snapshot.Snapshot.Groups) != 3 {
			return false
		}
		// g содержит очередную текущую группу при проверке выбранного источника и READY.
		for _, g := range snapshot.Snapshot.Groups {
			if g.Source != source || g.State != "READY" {
				return false
			}
		}
		return true
	}, func() {
		// Записывает последний снимок групп в диагностику при неудачном ожидании источника.

		t.Logf("последний снимок при ожидании %s: %+v", source, snapshot.Snapshot)
	})
}

// Запускает управляемую RTMP-публикацию синтетических AV1/AAC-пакетов с исходным темпом и возвращает
// функцию остановки. Остановка отменяет отправку, закрывает соединение и ограниченно ожидает горутину;
// она также зарегистрирована в t.Cleanup.
func applicationFlowPublish(t *testing.T, rawURL, key string) func() {
	t.Helper()
	// packets содержит синтетические AV1/AAC-пакеты для публикации; init содержит их исходные
	// конфигурации дорожек.
	packets, init := publisherStructuralInput(t, 1800)
	// u содержит разобранный адрес входного RTMP; err содержит отказ URL-разбора до добавления
	// ключа.
	u, err := url.Parse(rawURL)
	if err != nil {
		t.Fatal(err)
	}
	u.Fragment = key
	// ctx управляет отправкой OBS; cancel прекращает её при остановке или ошибке начального
	// согласования.
	ctx, cancel := context.WithCancel(context.Background())
	// client публикует синтетические AV1/AAC через настоящий RTMP-вход.
	client := &gortmplib.Client{URL: u, Publish: true}
	if err = client.Initialize(ctx); err != nil {
		cancel()
		t.Fatal(err)
	}
	if err = client.Write(&message.SetChunkSize{Value: 128}); err != nil {
		client.Close()
		cancel()
		t.Fatal(err)
	}
	// metadata сообщает исходную частоту видео 60 FPS перед заголовками дорожек.
	metadata := &message.DataAMF0{ChunkStreamID: 5, MessageStreamID: 1, Payload: amf0.Data{"onMetaData", amf0.Object{{Key: "framerate", Value: float64(60)}}}}
	if err = client.Write(metadata); err != nil {
		client.Close()
		cancel()
		t.Fatal(err)
	}
	// header содержит очередной заголовок видеокодека или AAC для начальной RTMP-публикации.
	for _, header := range []struct {
		// typ задаёт RTMP-тип начального сообщения: 9 для видео или 8 для аудио.
		typ byte
		// body содержит RTMP-префикс и точные байты конфигурации соответствующей дорожки.
		body []byte
	}{{9, append([]byte{0x90, 'a', 'v', '0', '1'}, init.Video.Headers()...)}, {8, append([]byte{0xaf, 0}, init.Audio.Headers()...)}} {
		if err = publisherWriteMessage(client.NetConn(), header.typ, 1, 0, header.body); err != nil {
			client.Close()
			cancel()
			t.Fatal(err)
		}
	}
	// done подтверждает завершение горутины публикации и закрытие RTMP-клиента.
	done := make(chan struct{})
	go func() {
		// Передаёт пакеты OBS по их времени, прерывает ожидание при отмене и закрывает RTMP-клиент после
		// завершения.

		defer close(done)
		defer client.Close()
		// start задаёт начальный момент темповой отправки синтетического источника.
		start := time.Now()
		// p содержит очередной пакет исходной последовательности для отправки по его DTS.
		for _, p := range packets {
			// at содержит время DTS пакета в миллисекундах относительно start.
			at := publisherNearestMS(p.Info().DTS, p.Config().Info().TimeBase)
			// timer ожидает фактический срок текущего пакета и останавливается при отмене.
			timer := time.NewTimer(max(time.Until(start.Add(time.Duration(at)*time.Millisecond)), 0))
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			}
			// typ выбирает RTMP-тип текущего медиапакета: аудио либо видео.
			typ := byte(8)
			// body собирает RTMP-префикс и полезные байты текущего AU.
			body := append([]byte{0xaf, 1}, p.Payload()...)
			if p.Config().Info().Track == media.TrackVideo {
				typ = 9
				body = append([]byte{0x91, 'a', 'v', '0', '1'}, p.Payload()...)
			}
			// err содержит отказ передачи очередного медиасообщения OBS.
			if err := publisherWriteMessage(client.NetConn(), typ, 1, uint32(at+200), body); err != nil {
				return
			}
		}
		<-ctx.Done()
	}()
	// stop отменяет публикацию, закрывает соединение и ограниченно присоединяет отправляющую
	// горутину.
	stop := func() {
		// Отменяет публикацию OBS, закрывает соединение и ограниченно ожидает выхода отправителя.

		cancel()
		client.Close()
		select {
		case <-done:
		case <-time.After(3 * time.Second):
			t.Error("источник OBS не завершился")
		}
	}
	t.Cleanup(stop)
	return stop
}

// Проверяет через HTTP подготовку трёх профилей, переходы источников и управление сессией и выходами.
// Ошибка замены сохраняет рабочую заглушку, повторы команд не создают исполнение заново, а история и
// квитанции сохраняются после повторного открытия БД.
func TestApplicationHTTPFlowPreparesThreeProfilesAndPreservesHistory(t *testing.T) {
	// f содержит полную фикстуру подготовки, менеджера и RTMP с управляемым кодированием.
	f := newApplicationFlowFixture(t, sessionAcceptanceBuildHelper(t))
	// a содержит настоящий API поверх компонентов полного сценария.
	a := integrationAPI(t, f)
	// asset содержит созданную через HTTP заглушку трёх профилей.
	asset := applicationFlowRequest[api.AssetDTO](t, a, "POST", "/fallback-assets", map[string]any{"name": "Заглушка полного потока"}, 201)
	// policy задаёт индивидуальные сроки watchdog и выбранную заглушку потока.
	policy := map[string]any{"asset_id": asset.ID, "watchdog": map[string]any{"video_timeout_ns": "300000000", "audio_timeout_ns": "300000000", "recovery_window_ns": "70000000", "keyframe_timeout_ns": "1000000000"}}
	// stream содержит созданный HTTP-поток с постоянным ключом и политикой.
	stream := applicationFlowRequest[api.StreamDTO](t, a, "POST", "/streams", map[string]any{"name": "Полный поток", "publication_key": "synthetic-session-key", "fallback": policy}, 201)
	// err переносит результаты доменного чтения потока, дальнейших файловых операций и операций
	// жизненного цикла.
	var err error
	f.stream, err = f.service.GetStream(context.Background(), core.ID(stream.ID))
	if err != nil {
		t.Fatal(err)
	}
	// profiles задаёт AV1/H.264 1440p и H.264 1080p для подготовки и независимых выходов.
	profiles := applicationFlowProfiles()
	// receivers собирает три независимых локальных RTMP-назначения.
	var receivers []*publisherReceiver
	// destinations сохраняет созданные назначения для проверки выбранных запусков и идемпотентности.
	var destinations []api.DestinationDTO
	// groups сохраняет созданные конфигурационные группы для дальнейшего добавления выхода в общую
	// группу.
	var groups []api.GroupDTO
	// i задаёт номер профиля; p содержит полный профиль соответствующих группы и назначения.
	for i, p := range profiles {
		// profile содержит созданный через HTTP именованный профиль текущего формата.
		profile := applicationFlowRequest[api.ProfileDTO](t, a, "POST", "/transcode-profiles", map[string]any{"name": fmt.Sprintf("Профиль %d", i), "profile": applicationFlowProfileJSON(p)}, 201)
		// g содержит созданную группу текущего потока и профиля.
		g := applicationFlowRequest[api.GroupDTO](t, a, "POST", "/transcode-groups", map[string]any{"name": fmt.Sprintf("Группа %d", i), "stream_id": stream.ID, "profile_id": profile.ID}, 201)
		groups = append(groups, g)
		// r содержит отдельный локальный приёмник текущего назначения.
		r, _ := newPublisherReceiver(t, false)
		receivers = append(receivers, r)
		// d содержит созданное включённое назначение текущей группы.
		d := applicationFlowRequest[api.DestinationDTO](t, a, "POST", "/destinations", map[string]any{"name": fmt.Sprintf("Назначение %d", i), "stream_id": stream.ID, "group_id": g.ID, "platform": "custom", "endpoint": "rtmp://" + r.listener.Addr().String() + "/live", "stream_key": fmt.Sprintf("synthetic-key-%d", i), "enabled": true}, 201)
		destinations = append(destinations, d)
	}
	// startPath адресует асинхронный запуск основного Stream.
	startPath := "/streams/" + stream.ID + "/start"
	// startBody задаёт один смысловой RequestID и пустой селектор назначений для повторяемого
	// запуска.
	startBody := map[string]any{"request_id": "start-full-flow", "destination_ids": []string{}}
	applicationFlowRequest[map[string]any](t, a, "POST", startPath, startBody, 409)
	// upload содержит идентификатор первого поколения, принятого из корректного PNG.
	upload := applicationFlowUpload(t, a, asset, applicationFlowPNG(t))
	applicationFlowGeneration(t, a, upload, "ready")
	// variants содержит три готовых варианта первого поколения для проверки фактических форматов.
	variants := applicationFlowRequest[api.PageDTO[api.VariantDTO]](t, a, "GET", "/fallback-assets/"+asset.ID+"/variants", nil, 200)
	if len(variants.Items) != 3 {
		t.Fatalf("подготовлено %d вариантов вместо трёх", len(variants.Items))
	}
	// p содержит очередной требуемый профиль при поиске соответствующего подготовленного варианта.
	for _, p := range profiles {
		// found отмечает наличие готового варианта с ожидаемыми кодеком, размерами, FPS и аудио.
		found := false
		// v содержит очередной подготовленный вариант для сравнения с текущим профилем.
		for _, v := range variants.Items {
			if v.Format.Video.Codec == string(p.Format.Video.Codec) && v.Format.Video.Width == p.Format.Video.Width && v.Format.Video.Height == p.Format.Video.Height && v.Format.Video.FrameRate.Numerator == 60 && v.Format.Audio.SampleRateHz == 48000 && v.State == "READY" {
				found = true
			}
		}
		if !found {
			t.Fatalf("не подготовлен профиль %+v", p.Format)
		}
	}
	asset = applicationFlowRequest[api.AssetDTO](t, a, "GET", "/fallback-assets/"+asset.ID, nil, 200)
	// badUpload содержит принятое поколение неверного изображения, которое должно завершиться error.
	badUpload := applicationFlowUpload(t, a, asset, []byte("invalid image"))
	applicationFlowGeneration(t, a, badUpload, "error")
	asset = applicationFlowRequest[api.AssetDTO](t, a, "GET", "/fallback-assets/"+asset.ID, nil, 200)
	if asset.CurrentGenerationID != upload.GenerationID || asset.State != "READY" {
		t.Fatalf("ошибка замены уничтожила current: %+v", asset)
	}
	// readyGeneration содержит первое рабочее поколение с точными путями обязательных вариантов.
	readyGeneration, err := f.service.GetGeneration(context.Background(), core.ID(upload.GenerationID))
	if err != nil {
		t.Fatal(err)
	}
	// missing указывает вариант, временно переименовываемый для проверки отказа до принятия запуска.
	missing := filepath.Join(f.directory, readyGeneration.RequiredProfiles[0].ManagedPath)
	if err = os.Rename(missing, missing+".held"); err != nil {
		t.Fatal(err)
	}
	// Контракт требует отказа до принятия команды; файловый ввод-вывод не обещает определённый код ответа HTTP.
	// startJSON содержит JSON исходного смыслового запроса для ручной проверки безопасного отказа
	// отсутствующему файлу.
	startJSON, err := json.Marshal(startBody)
	if err != nil {
		t.Fatal(err)
	}
	// request содержит настоящий POST запуска при временно отсутствующем варианте.
	request := httptest.NewRequest("POST", api.Prefix+startPath, bytes.NewReader(startJSON))
	request.Header.Set("Content-Type", "application/json")
	// rejected сохраняет фактический ошибочный статус и безопасный Problem Details без требования
	// одного конкретного файлового HTTP-кода.
	rejected := httptest.NewRecorder()
	a.ServeHTTP(rejected, request)
	if rejected.Code < 400 || rejected.Code >= 600 || rejected.Header().Get("Content-Type") != "application/problem+json" {
		t.Fatalf("отсутствующий вариант не отклонён безопасно: %d %s", rejected.Code, rejected.Body)
	}
	// sessions содержит историю потока после отказа, в которой не должна появиться новая Session.
	sessions := applicationFlowRequest[api.PageDTO[api.SessionDTO]](t, a, "GET", "/streams/"+stream.ID+"/sessions", nil, 200)
	if len(sessions.Items) != 0 || f.starts.Load() != 0 {
		t.Fatal("отсутствующий файл создал Session либо worker", sessions)
	}
	if err = os.Rename(missing+".held", missing); err != nil {
		t.Fatal(err)
	}
	// start содержит принятую и затем успешную квитанцию полноценного запуска трёх групп.
	start := applicationFlowRequest[api.ReceiptDTO](t, a, "POST", startPath, startBody, 202)
	start = applicationFlowReceipt(t, a, start)
	// current содержит публичную RUNNING Session с тремя самостоятельными снимками групп.
	current := applicationFlowRequest[api.SessionDTO](t, a, "GET", "/sessions/"+start.SessionID, nil, 200)
	if current.State != "RUNNING" || len(current.Groups) != 3 || f.starts.Load() != 3 {
		t.Fatalf("неполный запуск: %+v players=%d", current, f.starts.Load())
	}
	applicationFlowSource(t, a, current.ID, "FALLBACK")
	// r содержит очередной приёмник при ожидании начальной передачи заглушки.
	for _, r := range receivers {
		r.wait(t, func(m []publisherWireMessage) bool {
			// Ожидает начальные медиапакеты на каждом из трёх назначений.
			return publisherMediaCount(m) >= 8
		})
	}
	// duplicate содержит повторную квитанцию того же запроса без дополнительного запуска
	// проигрывателей.
	duplicate := applicationFlowRequest[api.ReceiptDTO](t, a, "POST", startPath, startBody, 202)
	if duplicate.ID != start.ID || f.starts.Load() != 3 {
		t.Fatal("повтор RequestID запустил новый runtime")
	}
	applicationFlowRequest[map[string]any](t, a, "POST", startPath, map[string]any{"request_id": "start-full-flow", "destination_ids": []string{destinations[0].ID}}, 409)
	// inputURL содержит адрес настоящего ingest-сервера основного Stream.
	inputURL := applicationFlowServeIngest(t, f)
	// stopInput завершает управляемую OBS-публикацию для проверки возврата на заглушку.
	stopInput := applicationFlowPublish(t, inputURL, "synthetic-session-key")
	applicationFlowSource(t, a, current.ID, "LIVE")
	// workers содержит текущие роли FFmpeg после перехода всех групп на LIVE.
	workers := applicationFlowRequest[api.SessionMetricsDTO](t, a, "GET", "/sessions/"+current.ID+"/metrics", nil, 200)
	// roles подсчитывает наблюдаемые роли для проверки двух LIVE-транскодировщиков и трёх
	// fallback-проигрывателей.
	roles := map[string]int{}
	// worker содержит очередного текущего работника FFmpeg при подсчёте ролей.
	for _, worker := range workers.Snapshot.FFmpeg {
		roles[worker.Role]++
	}
	if roles[string(core.FFmpegLiveTranscode)] != 2 || roles[string(core.FFmpegFallbackPlayer)] != 3 {
		t.Fatal("для трёх профилей нужны два транскодировщика и три проигрывателя заглушки", roles)
	}
	// controls сохраняет успешные управляющие квитанции для проверки после повторного открытия БД.
	var controls []api.ReceiptDTO
	// command отправляет HTTP-команду, ждёт её успеха и сохраняет квитанцию в controls.
	command := func(path, id string) api.ReceiptDTO {
		// Посылает команду через HTTP, ожидает её результат и сохраняет квитанцию для проверки после рестарта.

		t.Helper()
		// r содержит квитанцию текущей управляющей команды до и после ожидания её успеха.
		r := applicationFlowRequest[api.ReceiptDTO](t, a, "POST", path, map[string]any{"request_id": id}, 202)
		r = applicationFlowReceipt(t, a, r)
		controls = append(controls, r)
		return r
	}
	command("/sessions/"+current.ID+"/pause", "pause-full-flow")
	applicationFlowSource(t, a, current.ID, "FALLBACK")
	applicationFlowRequest[map[string]any](t, a, "POST", "/fallback-assets/"+asset.ID+"/rebuild", map[string]any{"expected_revision": fmt.Sprint(asset.Revision)}, 409)
	command("/sessions/"+current.ID+"/resume", "resume-full-flow")
	applicationFlowSource(t, a, current.ID, "LIVE")
	// healthy проверяет сохранность единственного соединения каждого исходного исправного
	// назначения.
	healthy := func() {
		// Проверяет, что исправные назначения сохранили единственное соединение.

		t.Helper()
		// i задаёт номер исходного приёмника; r содержит его счётчик принятых соединений для
		// проверки отсутствия пересоздания.
		for i, r := range receivers {
			if r.accepted.Load() != 1 {
				t.Fatalf("здоровый выход %d пересоздан: %d", i, r.accepted.Load())
			}
		}
	}
	healthy()
	// i выбирает ожидаемый профиль текущего выхода; r содержит приёмник для проверки маркера LIVE и
	// структуры сетевого представления.
	for i, r := range receivers {
		r.wait(t, func(m []publisherWireMessage) bool {
			// Ожидает маркер LIVE после переключения источника группы.
			return applicationFlowHasMarker(m, 20)
		})
		publisherAssertStructuralWire(t, r.snapshot(), profiles[i])
	}
	// additionalReceiver содержит новый приёмник выхода, добавляемого к уже работающей группе.
	additionalReceiver, _ := newPublisherReceiver(t, false)
	// additional содержит созданное выключенное в конфигурации назначение для явного динамического
	// добавления.
	additional := applicationFlowRequest[api.DestinationDTO](t, a, "POST", "/destinations", map[string]any{"name": "Дополнительный выход", "stream_id": stream.ID, "group_id": groups[1].ID, "platform": "custom", "endpoint": "rtmp://" + additionalReceiver.listener.Addr().String() + "/live", "stream_key": "additional-key", "enabled": false}, 201)
	// add содержит успешную квитанцию добавления самостоятельного выхода в работающую Session.
	add := applicationFlowRequest[api.ReceiptDTO](t, a, "POST", "/sessions/"+current.ID+"/outputs", map[string]any{"request_id": "add-full-flow", "destination_id": additional.ID}, 202)
	add = applicationFlowReceipt(t, a, add)
	additionalReceiver.wait(t, func(m []publisherWireMessage) bool {
		// Ожидает медиа на добавленном выходе, использующем уже работающую группу.
		return publisherMediaCount(m) >= 8
	})
	if f.starts.Load() != 3 {
		t.Fatal("добавление в общую группу запустило новый player")
	}
	command("/sessions/"+current.ID+"/outputs/"+add.OutputID+"/detach", "detach-full-flow")
	additionalReceiver.waitClosed(t)
	applicationFlowRequest[map[string]any](t, a, "POST", "/sessions/"+current.ID+"/outputs/"+add.OutputID+"/start", map[string]any{"request_id": "restart-detached-output"}, 409)
	healthy()
	// before сохраняет число медиасообщений первого здорового выхода перед разрывом соседнего
	// назначения.
	before := publisherMediaCount(receivers[0].snapshot())
	receivers[1].disconnect()
	receivers[1].wait(t, func(m []publisherWireMessage) bool {
		// Ожидает аудиопакет на новом соединении после разрыва одного назначения.

		// v содержит очередное сообщение переподключённого назначения при поиске AAC на новом
		// соединении.
		for _, v := range m {
			if v.connection >= 2 && v.typ == 8 && len(v.body) > 1 && v.body[1] == 1 {
				return true
			}
		}
		return false
	})
	receivers[0].wait(t, func(m []publisherWireMessage) bool {
		// Проверяет продолжение передачи соседнего выхода во время переподключения.
		return publisherMediaCount(m) > before+4
	})
	if receivers[0].accepted.Load() != 1 || receivers[2].accepted.Load() != 1 {
		t.Fatal("reconnect изменил соседние выходы")
	}
	stopInput()
	applicationFlowSource(t, a, current.ID, "FALLBACK")
	command("/sessions/"+current.ID+"/stop", "stop-full-flow")
	// r содержит очередной исходный приёмник при ожидании закрытия после остановки Session.
	for _, r := range receivers {
		r.waitClosed(t)
	}
	// ended содержит сохранённую STOPPED Session с временем завершения и исходными группами.
	ended := applicationFlowRequest[api.SessionDTO](t, a, "GET", "/sessions/"+current.ID, nil, 200)
	if ended.State != "STOPPED" || ended.EndedAt == nil || len(ended.Groups) != 3 {
		t.Fatalf("неполная история завершения: %+v", ended)
	}
	sessionAcceptanceWait(t, func() bool {
		// Ожидает сохранённое подтверждение очистки Session после остановки.

		// s содержит текущее состояние Session; err содержит отказ чтения при ожидании снятия
		// удержаний очистки.
		s, err := f.service.GetSession(context.Background(), core.ID(current.ID))
		return err == nil && !s.CleanupPending && !s.Issued
	})
	// outputs содержит долговечную историю всех четырёх выходов, включая отсоединённый
	// дополнительный.
	outputs := applicationFlowRequest[api.PageDTO[api.OutputDTO]](t, a, "GET", "/sessions/"+current.ID+"/outputs", nil, 200)
	if len(outputs.Items) != 4 {
		t.Fatalf("история выходов потеряна: %+v", outputs)
	}
	// metrics содержит ответ терминальной Session, в котором текущий runtime должен отсутствовать.
	metrics := applicationFlowRequest[api.SessionMetricsDTO](t, a, "GET", "/sessions/"+current.ID+"/metrics", nil, 200)
	if metrics.Availability != "unavailable" || metrics.Snapshot != nil {
		t.Fatalf("терминальная Session сохранила runtime: %+v", metrics)
	}
	// ctx ограничивает закрытие и повторное чтение истории восемью секундами; cancel освобождает
	// таймер.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	if err = a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err = f.manager.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if err = f.store.Close(); err != nil {
		t.Fatal(err)
	}
	// reopened содержит новый Store той же БД после завершения менеджера и API.
	reopened, err := storage.Open(ctx, filepath.Join(f.directory, "state.sqlite"), sessionAcceptanceOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает повторно открытое хранилище после проверки истории команд и выходов.

		// err содержит результат закрытия повторно открытого Store при очистке.
		if err := reopened.Close(); err != nil {
			t.Error(err)
		}
	})
	// service содержит доменный сервис нового incarnation для проверки долговечных результатов.
	service, err := core.NewService(reopened, reopened.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	// receipt содержит очередную исходную успешную квитанцию из controls, start или add.
	for _, receipt := range append(controls, start, add) {
		// stored содержит квитанцию после повторного открытия; err содержит отказ чтения.
		stored, err := service.GetReceipt(ctx, core.ID(receipt.ID))
		if err != nil || string(stored.Status) != receipt.Status || stored.RequestID != receipt.RequestID {
			t.Fatalf("квитанция после рестарта: %+v %v", stored, err)
		}
	}
	// history содержит прежнюю завершённую Session после нового открытия БД.
	history, err := service.GetSession(ctx, core.ID(current.ID))
	if err != nil || history.State != core.SessionStopped || len(history.Snapshot.Groups) != 3 || history.CleanupPending || history.Issued {
		t.Fatalf("история после рестарта: %+v %v", history, err)
	}
	// storedOutputs содержит все прежние выходы после повторного открытия.
	storedOutputs, err := service.ListOutputs(ctx, core.PageRequest{Query: core.ListQuery{Limit: 32, SessionID: history.ID}})
	if err != nil || len(storedOutputs.Items) != 4 {
		t.Fatalf("выходы после рестарта: %+v %v", storedOutputs, err)
	}
	// output содержит очередной сохранённый выход для проверки снятых Issued, CleanupPending и
	// DesiredRunning.
	for _, output := range storedOutputs.Items {
		if output.CleanupPending || output.Issued || output.DesiredRunning {
			t.Fatal("очистка выхода не сохранилась после рестарта", output)
		}
	}
	if err = service.ConfigureTelemetry(ctx, f.config.Telemetry); err != nil {
		t.Fatal(err)
	}
	// anchor содержит новый пустой оперативный снимок после настройки телеметрии нового владельца.
	anchor, err := service.RuntimeAnchor(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if len(anchor.Sessions) != 0 {
		t.Fatal("рестарт восстановил старый runtime", anchor)
	}
}

// Ищет видеосообщение RTMP, последний байт которого совпадает с заданным тестовым маркером источника.
func applicationFlowHasMarker(messages []publisherWireMessage, marker byte) bool {
	// m содержит очередное RTMP-сообщение при поиске видеомаркера источника.
	for _, m := range messages {
		if m.typ == 9 && len(m.body) > 5 && m.body[len(m.body)-1] == marker {
			return true
		}
	}
	return false
}
