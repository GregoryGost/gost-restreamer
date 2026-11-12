package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"reflect"
	"strconv"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/api"
	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
)

// Проверяет, что разобранное JSON-значение является объектом, и возвращает его для дальнейшей проверки
// полей.
func publicTelemetryObject(t *testing.T, value any) map[string]any {
	t.Helper()
	// object содержит переданный JSON-объект, а ok подтверждает ожидаемый тип перед проверкой его
	// полей.
	object, ok := value.(map[string]any)
	if !ok {
		t.Fatalf("ожидался объект JSON: %#v", value)
	}
	return object
}

// Извлекает обязательный объект media из объекта метрик; неверная структура JSON завершает тест.
func publicTelemetryMedia(t *testing.T, value any) map[string]any {
	t.Helper()
	return publicTelemetryObject(t, publicTelemetryObject(t, value)["media"])
}

// Проверяет строковое десятичное представление счётчика и разбирает его в точный uint64 без
// промежуточного преобразования во float64.
func publicTelemetryUint(t *testing.T, value any) uint64 {
	t.Helper()
	// text содержит точную десятичную строку счётчика, а ok подтверждает строковое представление без
	// float64.
	text, ok := value.(string)
	if !ok {
		t.Fatalf("точный счётчик не передан строкой: %#v", value)
	}
	// n содержит точное значение счётчика uint64, а err сообщает об ошибке его десятичного разбора.
	n, err := strconv.ParseUint(text, 10, 64)
	if err != nil {
		t.Fatal(err)
	}
	return n
}

// По независимому RTMP-приёмнику считает полезные байты, единицы доступа и видеокадры. Каждая учтённая
// нагрузка должна совпадать с данными конечного источника фикстуры.
func publicTelemetryWireTotals(t *testing.T, receiver *publisherReceiver) (payload, units, videos uint64) {
	t.Helper()
	// video содержит ожидаемую AV1-конфигурацию конечного источника, а audio — его AAC-конфигурацию
	// для проверки нагрузки.
	video, audio := publisherConfigs(t, publisherProfile(core.CodecAV1, 640, 360), 1)
	// message обозначает очередное независимо принятое RTMP-сообщение для подсчёта полезной
	// нагрузки.
	for _, message := range receiver.snapshot() {
		// data содержит полезную нагрузку принятой единицы доступа, а expected — ожидаемые байты
		// соответствующей дорожки.
		var data, expected []byte
		if message.typ == 8 && len(message.body) >= 2 && message.body[1] == 1 {
			data, expected = message.body[2:], publisherPayload(audio, 0)
		}
		if message.typ == 9 && len(message.body) >= 5 && message.body[0]&0x80 != 0 && message.body[0]&15 == 1 {
			data, expected = message.body[5:], publisherPayload(video, 10)
			videos++
		}
		if data != nil {
			if !bytes.Equal(data, expected) {
				t.Fatalf("приёмник получил изменённую полезную нагрузку: %x", data)
			}
			units++
			payload += uint64(len(data))
		}
	}
	return
}

// Сверяет публичные метрики выхода с независимо принятыми RTMP-пакетами и числом прочитанных байтов.
// Требует достаточного количества видео и аудио, единственного соединения и отсутствия потерь
// отправки.
func publicTelemetryCheckOutput(t *testing.T, metrics map[string]any, receiver *publisherReceiver) {
	t.Helper()
	// payload содержит независимо принятые полезные байты, units — число единиц доступа, а videos —
	// число видеокадров приёмника.
	payload, units, videos := publicTelemetryWireTotals(t, receiver)
	// media содержит публичные медиасчётчики выхода для сравнения с RTMP-приёмником.
	media := publicTelemetryMedia(t, metrics)
	// counters содержит счётчики единиц доступа, попыток и соединений проверяемого выхода.
	counters := publicTelemetryObject(t, metrics["counters"])
	if videos < 8 || units <= videos {
		t.Fatalf("недостаточно реального видео и аудио для сверки: %d/%d", videos, units)
	}
	if publicTelemetryUint(t, media["payload_bytes"]) != payload || publicTelemetryUint(t, media["written_bytes"]) != receiver.bytes.Load() || publicTelemetryUint(t, media["video_frames"]) != videos || publicTelemetryUint(t, media["audio_units"]) != units-videos || publicTelemetryUint(t, counters["access_units"]) != units {
		t.Fatalf("REST/SSE не совпал с независимым RTMP-приёмником: media=%v counters=%v wire payload=%d units=%d video=%d bytes=%d", media, counters, payload, units, videos, receiver.bytes.Load())
	}
	if counters["attempts"] != "1" || counters["connections"] != "1" || counters["reconnects"] != "0" || receiver.accepted.Load() != 1 {
		t.Fatal("конечный источник вызвал лишнее подключение", counters)
	}
	// reason обозначает причину потери отправки, а count содержит её публичный счётчик, который
	// ожидается нулевым.
	for reason, count := range publicTelemetryObject(t, metrics["delivery_drops"]) {
		if count != "0" {
			t.Fatalf("неожиданная потеря отправки %s=%v", reason, count)
		}
	}
}

// Проверяет точные счётчики конечного источника заглушки: 60 видео-AU и 47 AAC-AU при нулевых
// счётчиках потреблённого LIVE.
func publicTelemetryCheckGroup(t *testing.T, metrics map[string]any) {
	t.Helper()
	// video содержит конфигурацию видеонагрузки конечного источника, а audio — конфигурацию его
	// AAC-нагрузки.
	video, audio := publisherConfigs(t, publisherProfile(core.CodecAV1, 640, 360), 1)
	// fallback содержит публичные счётчики фактически потреблённой заглушки группы.
	fallback := publicTelemetryObject(t, metrics["fallback_input"])
	// live содержит публичные счётчики фактически потреблённого LIVE, ожидаемые нулевыми в этом
	// сценарии.
	live := publicTelemetryObject(t, metrics["live_input"])
	// За одну секунду источник выдаёт 60 видео-AU и 47 AAC-AU по 1024 сэмпла при 48 kHz.
	// payload содержит ожидаемое строковое число байтов 60 видео-AU и 47 AAC-AU конечного источника.
	payload := strconv.Itoa(60*len(publisherPayload(video, 10)) + 47*len(publisherPayload(audio, 0)))
	if fallback["video_frames"] != "60" || fallback["audio_units"] != "47" || fallback["payload_bytes"] != payload || live["video_frames"] != "0" || live["audio_units"] != "0" || live["payload_bytes"] != "0" {
		t.Fatal("группа смешала фактические входы LIVE и FALLBACK", metrics)
	}
}

// Сверяет REST и SSE с известными входными AU и независимым RTMP-приёмником, разделяя полученные
// входные данные и потреблённый LIVE. Остановка удаляет текущие метрики, а повторное открытие SQLite
// сохраняет историю и квитанции без восстановления телеметрии в RAM.
func TestAcceptancePublicTelemetryMatchesKnownIngressAndNativeWrites(t *testing.T) {
	// ctx используется для настройки, остановки и повторного открытия тестовой сессии.
	ctx := context.Background()
	// receiver независимо принимает RTMP-выход для сравнения публичной телеметрии с реальными
	// сообщениями.
	receiver, _ := newPublisherReceiver(t, false)
	// f объединяет рабочий менеджер и хранилище для проверки телеметрии конечного источника.
	f := newSessionAcceptanceFixture(t, sessionAcceptanceBuildHelper(t), []string{"rtmp://" + receiver.listener.Addr().String() + "/live"}, []int{0})
	f.stream.Fallback.Watchdog.VideoTimeout = time.Minute
	f.stream.Fallback.Watchdog.AudioTimeout = time.Minute
	// err хранит результат обновления настроек потока и последующих операций сценария.
	var err error
	f.stream, err = f.service.UpdateStream(ctx, f.stream, f.stream.Revision)
	if err != nil {
		t.Fatal(err)
	}
	f.config.Group.FFmpeg.VideoTimeout, f.config.Group.FFmpeg.AudioTimeout = time.Minute, time.Minute
	// finished задаёт путь маркера окончания подачи конечного источника.
	finished := filepath.Join(f.directory, "finite-source-finished")
	f.config.Group.Player = func(_ context.Context, _ string, _ *os.File, attempt ffmpeg.Attempt) (ffmpeg.Command, error) {
		// Создаёт команду конечного тестового источника вместо проигрывателя для независимого подсчёта
		// фактически переданного медиа.

		// binary содержит путь текущего тестового бинарника для запуска конечного дочернего
		// источника, а err сообщает об ошибке определения пути.
		binary, err := os.Executable()
		return ffmpeg.Command{Binary: binary, Args: []string{"-test.run=^TestPublicTelemetryFiniteMediaChild$", "--", "--public-telemetry", attempt.OutputPath, finished}}, err
	}
	f.launch(t)
	// a предоставляет HTTP-команды и телеметрию рабочей фикстуры.
	a := integrationAPI(t, f)
	// server обслуживает локальную SSE-подписку на события проверяемой сессии.
	server := httptest.NewServer(a)
	t.Cleanup(server.Close)
	// start содержит HTTP-квитанцию запуска потока с единственным выбранным назначением.
	start := integrationAPIRequest(t, a, "POST", "/streams/"+string(f.stream.ID)+"/start", fmt.Sprintf(`{"request_id":"start-public-metrics","destination_ids":[%q]}`, f.destinations[0].ID), 202)
	// r содержит успешно завершённую квитанцию запуска сессии.
	r := sessionAcceptanceReceipt(t, f.service, core.ID(start["id"].(string)), core.CommandSucceeded)
	// current содержит сессию после перехода в рабочее состояние.
	current := f.running(t, r)
	// path задаёт HTTP-префикс маршрутов проверяемой сессии.
	path := "/sessions/" + string(current.ID)
	// anchor фиксирует исходный курсор снимка до проверки входных и выходных событий.
	anchor := integrationAPIRequest(t, a, "GET", "/events/snapshot", "", 200)
	// stream читает SSE после исходного снимка для независимого сравнения с REST.
	stream := publicTelemetrySubscribe(t, server, "/events?cursor="+url.QueryEscape(anchor["cursor"].(string)), "")
	// listener предоставляет локальный адрес RTMP-входа менеджера для синтетической публикации.
	listener := acceptanceMonitoringIngest(t, f)
	// videoConfig содержит AV1-конфигурацию входа, а audioConfig — AAC-конфигурацию для известных
	// нагрузок.
	videoConfig, audioConfig := publisherConfigs(t, publisherProfile(core.CodecAV1, 640, 360), 1)
	// video содержит структурный AV1-пакет входа, а audio — структурный AAC-пакет для трёх известных
	// A/V-пар.
	video, audio := publisherPayload(videoConfig, 20), publisherPayload(audioConfig, 0)
	// client публикует синтетический RTMP-вход, остающийся неготовым для потребления LIVE.
	client := acceptanceMonitoringClient(t, f, listener, videoConfig)
	acceptanceMonitoringBeforeReady(t, client, audioConfig, video, audio, 3)
	// wantInputBytes содержит строковое число полезных байтов трёх синтетических A/V-пар входа.
	wantInputBytes := strconv.Itoa(3 * (len(video) + len(audio)))
	sessionAcceptanceWait(t, func() bool {
		// Ожидает завершение передачи конечного источника по файловому маркеру.
		// err показывает, появился ли маркер окончания подачи конечного источника.
		_, err := os.Stat(finished)
		return err == nil
	})
	// snapshot сохраняет согласованный публичный снимок сессии, а outputMetrics — метрики
	// единственного выхода для последующих сравнений REST и SSE.
	var snapshot, outputMetrics map[string]any
	sessionAcceptanceWait(t, func() bool {
		// Ожидает согласование REST со счётчиками входа и независимого RTMP-приёмника, включая измеренное
		// нулевое окно FPS.

		// response содержит очередной HTTP-ответ метрик при ожидании согласования с независимым
		// приёмником.
		response := integrationAPIRequest(t, a, "GET", path+"/metrics", "", 200)
		if response["availability"] != "available" {
			return false
		}
		snapshot = publicTelemetryObject(t, response["snapshot"])
		// input содержит наблюдение входа текущего публичного снимка для ожидания известных
		// счётчиков.
		input := publicTelemetryObject(t, snapshot["input"])
		if input["metrics"] == nil || publicTelemetryMedia(t, input["metrics"])["payload_bytes"] != wantInputBytes {
			return false
		}
		// outputs содержит выходы публичного снимка для проверки единственного измеряемого
		// назначения.
		outputs := snapshot["outputs"].([]any)
		if len(outputs) != 1 || publicTelemetryObject(t, outputs[0])["metrics"] == nil {
			return false
		}
		outputMetrics = publicTelemetryObject(t, publicTelemetryObject(t, outputs[0])["metrics"])
		// media содержит медиасчётчики выхода при ожидании согласования байтов и нулевого окна
		// FPS.
		media := publicTelemetryMedia(t, outputMetrics)
		// units содержит число независимо принятых единиц доступа для проверки счётчика выхода.
		_, units, _ := publicTelemetryWireTotals(t, receiver)
		return units >= 16 && publicTelemetryUint(t, publicTelemetryObject(t, outputMetrics["counters"])["access_units"]) == units && publicTelemetryUint(t, media["written_bytes"]) == receiver.bytes.Load() && publicTelemetryObject(t, publicTelemetryObject(t, media["rates"])["fps"])["value"] == float64(0)
	})
	// input содержит согласованное публичное наблюдение входной публикации до готовности LIVE.
	input := publicTelemetryObject(t, snapshot["input"])
	// inputMedia содержит известные входные счётчики и формат из согласованного REST-снимка.
	inputMedia := publicTelemetryMedia(t, input["metrics"])
	// format содержит публичное описание кодеков и размеров входа для проверки AV1/AAC 640×360.
	format := publicTelemetryObject(t, inputMedia["format"])
	if input["connected"] != true || inputMedia["video_frames"] != "3" || inputMedia["audio_units"] != "3" || inputMedia["payload_bytes"] != wantInputBytes || format["video_codec"] != "av1" || format["audio_codec"] != "aac" || format["width"] != float64(640) || format["height"] != float64(360) {
		t.Fatal("REST потерял известный вход до готовности LIVE", input)
	}
	// groups содержит наблюдаемые группы согласованного снимка, где ожидается одна группа.
	groups := snapshot["groups"].([]any)
	if len(groups) != 1 {
		t.Fatal("неверное число наблюдаемых групп", groups)
	}
	// group содержит наблюдение единственной группы, продолжающей потреблять заглушку.
	group := publicTelemetryObject(t, groups[0])
	// live содержит счётчики потреблённого LIVE для проверки отсутствия смешения с принятыми
	// входными пакетами.
	live := publicTelemetryObject(t, publicTelemetryObject(t, group["metrics"])["live_input"])
	if group["source"] != "FALLBACK" || live["video_frames"] != "0" || live["payload_bytes"] != "0" {
		t.Fatal("полученный OBS ошибочно посчитан потреблённым LIVE", group)
	}
	publicTelemetryCheckGroup(t, publicTelemetryObject(t, group["metrics"]))
	publicTelemetryCheckOutput(t, outputMetrics, receiver)
	// fps содержит измеренное окно частоты кадров после окончания источника для проверки доступного
	// нулевого значения.
	fps := publicTelemetryObject(t, publicTelemetryObject(t, publicTelemetryMedia(t, outputMetrics)["rates"])["fps"])
	if fps["status"] != "available" || fps["value"] != float64(0) || publicTelemetryUint(t, fps["duration_ns"]) == 0 {
		t.Fatal("наблюдённый интервал без новых кадров не представлен измеренным нулём", fps)
	}
	// seenInput отмечает получение известных входных счётчиков по SSE, seenOutput — проверенных
	// выходных счётчиков, а seenGroup — точных счётчиков заглушки группы.
	seenInput, seenOutput, seenGroup := false, false, false
	for !seenInput || !seenOutput || !seenGroup {
		// frame содержит очередное событие SSE, а ok показывает, удалось ли прочитать событие
		// без закрытия подписки.
		frame, ok := stream.next(t)
		if !ok || frame.kind == "resync_required" {
			t.Fatal("непрерывная подписка потеряла измерения", frame)
		}
		if frame.data["session_id"] != string(current.ID) || frame.kind != "metrics" {
			continue
		}
		if frame.data["input"] != nil {
			// observed содержит входное наблюдение SSE для поиска трёх известных A/V-пар.
			observed := publicTelemetryObject(t, frame.data["input"])
			if observed["metrics"] != nil {
				// media содержит медиасчётчики входного SSE-наблюдения для сравнения с
				// известной нагрузкой.
				media := publicTelemetryMedia(t, observed["metrics"])
				if media["payload_bytes"] == wantInputBytes {
					if media["video_frames"] != "3" || media["audio_units"] != "3" || observed["connected"] != true {
						t.Fatal("SSE изменил известные входные AU", observed)
					}
					seenInput = true
				}
			}
		}
		if frame.data["group"] != nil {
			// metrics содержит измерения группы из SSE для проверки 60 кадров заглушки и
			// нулевого LIVE.
			metrics := publicTelemetryObject(t, frame.data["group"])
			if publicTelemetryObject(t, metrics["fallback_input"])["video_frames"] == "60" {
				publicTelemetryCheckGroup(t, metrics)
				seenGroup = true
			}
		}
		if frame.data["output"] != nil {
			// metrics содержит измерения выхода из SSE для сравнения с согласованным REST и
			// RTMP-приёмником.
			metrics := publicTelemetryObject(t, frame.data["output"])
			if publicTelemetryMedia(t, metrics)["written_bytes"] == publicTelemetryMedia(t, outputMetrics)["written_bytes"] {
				publicTelemetryCheckOutput(t, metrics, receiver)
				seenOutput = true
			}
		}
	}
	stream.close(t)
	client.Close()
	// stop содержит квитанцию HTTP-остановки сессии после сравнения метрик.
	stop := integrationAPIRequest(t, a, "POST", path+"/stop", `{"request_id":"stop-public-metrics"}`, 202)
	sessionAcceptanceReceipt(t, f.service, core.ID(stop["id"].(string)), core.CommandSucceeded)
	sessionAcceptanceWait(t, func() bool {
		// Ожидает терминальную Session с сохранённым подтверждением очистки после остановки через HTTP.

		// s содержит долговечное состояние остановленной сессии, а err сообщает об ошибке чтения
		// при ожидании очистки.
		s, err := f.service.GetSession(ctx, current.ID)
		return err == nil && s.State.Terminal() && !s.CleanupPending && !s.Issued
	})
	// terminal содержит эталонный ответ недоступных метрик завершённой сессии.
	terminal := integrationAPIRequest(t, a, "GET", path+"/metrics", "", 200)
	if terminal["availability"] != "unavailable" || terminal["reason"] != "session_terminal" || terminal["snapshot"] != nil {
		t.Fatal("терминальный запуск сохранил текущие метрики", terminal)
	}
	// cleaned содержит долговечное состояние сессии после подтверждённой очистки для сравнения после
	// открытия SQLite.
	cleaned, err := f.service.GetSession(ctx, current.ID)
	if err != nil {
		t.Fatal(err)
	}
	// cleanedOutputs содержит сохранённые выходы после остановки для проверки очистки и сохранности
	// истории.
	cleanedOutputs := f.outputs(t, current.ID)
	// output обозначает очередной выход, который должен подтвердить очистку до открытия хранилища
	// заново.
	for _, output := range cleanedOutputs {
		if output.CleanupPending || output.Issued {
			t.Fatal("выход не подтвердил очистку", output)
		}
	}
	// history содержит публичную историю остановленной сессии до закрытия хранилища.
	history := integrationAPIRequest(t, a, "GET", path, "", 200)
	// receipt содержит публичную квитанцию остановки до закрытия хранилища.
	receipt := integrationAPIRequest(t, a, "GET", "/commands/"+stop["id"].(string), "", 200)
	// err сообщает об ошибке закрытия менеджера перед повторным открытием SQLite.
	if err := f.manager.Close(ctx); err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке завершения исходного API перед открытием хранилища заново.
	if err := a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке закрытия исходного хранилища перед повторным открытием.
	if err := f.store.Close(); err != nil {
		t.Fatal(err)
	}
	// reopened содержит заново открытое файловое хранилище для сравнения истории без восстановления
	// RAM-метрик.
	reopened, err := storage.Open(ctx, filepath.Join(f.directory, "state.sqlite"), sessionAcceptanceOptions())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает повторно открытое хранилище после сравнения истории и отсутствия старой телеметрии.
		_ = reopened.Close()
	})
	// service предоставляет доменные операции нового экземпляра поверх повторно открытой SQLite.
	service, err := core.NewService(reopened, reopened.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	// err сообщает об отказе настройки телеметрии нового экземпляра сервиса.
	if err := service.ConfigureTelemetry(ctx, f.config.Telemetry); err != nil {
		t.Fatal(err)
	}
	// freshAPI предоставляет публичное чтение истории и оперативного состояния после открытия SQLite
	// заново.
	freshAPI := publicTelemetryAPI(t, service)
	// got содержит историю сессии после повторного открытия для сравнения с прежним
	// HTTP-представлением.
	if got := integrationAPIRequest(t, freshAPI, "GET", path, "", 200); !reflect.DeepEqual(got, history) {
		t.Fatal("история изменилась после открытия БД", got, history)
	}
	// got содержит квитанцию остановки после повторного открытия для сравнения с прежним
	// представлением.
	if got := integrationAPIRequest(t, freshAPI, "GET", "/commands/"+stop["id"].(string), "", 200); !reflect.DeepEqual(got, receipt) {
		t.Fatal("квитанция изменилась после открытия БД", got, receipt)
	}
	// after содержит долговечное состояние сессии после открытия SQLite заново для сравнения с
	// cleaned.
	after, err := service.GetSession(ctx, current.ID)
	if err != nil || !reflect.DeepEqual(after, cleaned) {
		t.Fatal("долговечный lifecycle/cleanup изменился", after, cleaned, err)
	}
	// before обозначает очередной сохранённый выход до закрытия хранилища.
	for _, before := range cleanedOutputs {
		// after содержит тот же выход после повторного открытия, а err сообщает об ошибке его
		// чтения.
		after, err := service.GetOutput(ctx, before.ID)
		if err != nil || !reflect.DeepEqual(after, before) {
			t.Fatal("снимок выхода/cleanup изменился", after, before, err)
		}
	}
	// got содержит ответ метрик после повторного открытия для сравнения с недоступным терминальным
	// ответом.
	if got := integrationAPIRequest(t, freshAPI, "GET", path+"/metrics", "", 200); !reflect.DeepEqual(got, terminal) {
		t.Fatal("рестарт восстановил старые метрики", got)
	}
	// fresh содержит новый снимок событий для проверки пустой RAM и сменившегося курсора.
	fresh := integrationAPIRequest(t, freshAPI, "GET", "/events/snapshot", "", 200)
	if len(fresh["sessions"].([]any)) != 0 || fresh["gpu"] != nil || fresh["cursor"] == anchor["cursor"] {
		t.Fatal("старый runtime восстановлен из истории", fresh)
	}
}

// Создаёт синтетическое наблюдение одного GPU: memory_used_bytes доступен с заданным значением,
// остальные показатели явно недоступны.
func publicTelemetryGPU(value uint64, at time.Time) core.GPUSnapshot {
	// metric задаёт доступное синтетическое измерение занятой памяти GPU в байтах с указанным
	// временем.
	metric := core.GPUMetric{Status: core.MetricAvailable, Value: value, SampledAt: at, LastSuccessAt: at}
	// absent задаёт явно недоступные показатели GPU с причиной отсутствия источника.
	absent := core.GPUMetric{Status: core.MetricUnavailable, Reason: core.ReasonSourceAbsent, SampledAt: at}
	return core.GPUSnapshot{Status: core.MetricAvailable, SampledAt: at, StaleAfter: time.Minute, Devices: []core.GPUDevice{{UUID: "synthetic-gpu", Utilization: absent, MemoryUtilization: absent, EncoderUtilization: absent, DecoderUtilization: absent, MemoryUsedBytes: metric, MemoryTotalBytes: absent, MemoryFreeBytes: absent}}}
}

// Извлекает значение memory_used_bytes единственного GPU из публичного JSON; отсутствие точного одного
// устройства завершает тест.
func publicTelemetryGPUValue(t *testing.T, value any) any {
	t.Helper()
	// gpu содержит публичный объект синтетического GPU-снимка для извлечения занятой памяти.
	gpu := publicTelemetryObject(t, value)
	// devices содержит устройства публичного GPU-снимка, где ожидается ровно одно.
	devices := gpu["devices"].([]any)
	if len(devices) != 1 {
		t.Fatal("потеряно точное системное наблюдение", gpu)
	}
	return publicTelemetryObject(t, publicTelemetryObject(t, devices[0])["memory_used_bytes"])["value"]
}

// Проверяет, что неподходящий cursor вызывает единственный resync_required с заданной причиной и
// адресом нового снимка, после чего поток закрывается.
func publicTelemetryResync(t *testing.T, server *httptest.Server, cursor, reason string) {
	t.Helper()
	// stream открывает подписку с неподходящим курсором для проверки единственного требования
	// повторного снимка.
	stream := publicTelemetrySubscribe(t, server, "/events?cursor="+url.QueryEscape(cursor), "")
	// frame содержит первый кадр ответа на неподходящий курсор, а ok подтверждает его получение.
	frame, ok := stream.next(t)
	if !ok || frame.kind != "resync_required" || frame.id != "" || frame.data["reason"] != reason || frame.data["snapshot_url"] != api.Prefix+"/events/snapshot" {
		t.Fatal("неполный диапазон не требует новый снимок", frame)
	}
	// frame содержит возможный лишний кадр после resync_required, а ok показывает нарушение
	// ожидаемого закрытия потока.
	if frame, ok := stream.next(t); ok {
		t.Fatal("после resync выдано событие старого диапазона", frame)
	}
	stream.close(t)
}

// Проверяет восстановление SSE после вытеснения событий через новый снимок и продолжение по
// Last-Event-ID без повтора последнего события. После повторного открытия хранилища старый cursor
// требует reset, а новая подписка получает только новое RAM-наблюдение GPU.
func TestAcceptancePublicTelemetryResyncSnapshotAndResubscribe(t *testing.T) {
	// ctx используется для настройки телеметрии, публикации GPU-наблюдений и повторного открытия
	// хранилища.
	ctx := context.Background()
	// path задаёт файл временной SQLite, повторно открываемый в сценарии смены эпохи событий.
	path := filepath.Join(t.TempDir(), "events.sqlite")
	// limits ограничивает журнал восемью событиями для управляемого вытеснения и задаёт бюджеты GPU.
	limits := core.TelemetryLimits{Events: core.EventLimits{MaxEvents: 8, MaxBytes: 1 << 20, MaxEventBytes: 65536, MaxPageSize: 8}, MediaStaleAfter: time.Minute, MaxGPUDevices: 2, MaxGPUUUIDBytes: 256, MaxGPUBytes: 16384}
	// open создаёт новый сервис, API и локальный HTTP-сервер поверх одной временной SQLite с
	// заданными пределами событий.
	open := func() (*storage.Store, *core.Service, *api.API, *httptest.Server) {
		// Открывает хранилище и новый HTTP-сервер с ограниченным журналом событий для проверки пропусков и
		// смены эпохи.

		t.Helper()
		// store содержит очередной экземпляр файлового хранилища, а err сообщает об ошибке его
		// открытия.
		store, err := storage.Open(ctx, path, sessionAcceptanceOptions())
		if err != nil {
			t.Fatal(err)
		}
		t.Cleanup(func() {
			// Закрывает очередное временное хранилище после проверки подписки.
			_ = store.Close()
		})
		// service предоставляет доменные операции очередного экземпляра хранилища и новый реестр
		// RAM-наблюдений.
		service, err := core.NewService(store, store.Incarnation())
		if err != nil {
			t.Fatal(err)
		}
		// err сообщает об отказе настройки ограниченного журнала событий текущего сервиса.
		if err := service.ConfigureTelemetry(ctx, limits); err != nil {
			t.Fatal(err)
		}
		// a предоставляет публичные маршруты телеметрии текущего экземпляра сервиса.
		a := publicTelemetryAPI(t, service)
		// server обслуживает локальные SSE-подписки текущего экземпляра API.
		server := httptest.NewServer(a)
		t.Cleanup(server.Close)
		return store, service, a, server
	}
	// store содержит исходное хранилище, service — исходный сервис телеметрии, a — его API, а server
	// — локальный HTTP-сервер исходной эпохи.
	store, service, a, server := open()
	// observer публикует синтетические системные наблюдения GPU, а err сообщает об ошибке его
	// регистрации.
	observer, err := service.OpenSystemObserver(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает системного наблюдателя GPU исходного процесса.
		_ = observer.Close(ctx)
	})
	// stale содержит исходный курсор, который станет недоступен после вытеснения событий.
	stale := integrationAPIRequest(t, a, "GET", "/events/snapshot", "", 200)
	// at задаёт начало возрастающих времён синтетических GPU-измерений.
	at := time.Now()
	// base задаёт счётчик байтов выше точного целочисленного диапазона float64 для проверки
	// строковой сериализации.
	const base uint64 = 9007199254740993
	// i задаёт номер одного из двенадцати GPU-наблюдений, переполняющих восьмисобытийный журнал.
	for i := range uint64(12) {
		// err сообщает об отказе публикации синтетического GPU-наблюдения при заполнении
		// журнала.
		if err := observer.ObserveGPU(ctx, publicTelemetryGPU(base+i, at.Add(time.Duration(i)*time.Nanosecond))); err != nil {
			t.Fatal(err)
		}
	}
	publicTelemetryResync(t, server, stale["cursor"].(string), "gap")
	// fresh содержит новый снимок после вытеснения с последним GPU-значением и актуальным курсором.
	fresh := integrationAPIRequest(t, a, "GET", "/events/snapshot", "", 200)
	if fresh["cursor"] == stale["cursor"] || publicTelemetryGPUValue(t, fresh["gpu"]) != strconv.FormatUint(base+11, 10) {
		t.Fatal("новый снимок не восстановил последнее состояние", fresh)
	}
	// stream читает события после нового снимка для проверки первого последующего GPU-измерения.
	stream := publicTelemetrySubscribe(t, server, "/events?cursor="+url.QueryEscape(fresh["cursor"].(string)), "")
	// err сообщает об отказе публикации первого GPU-измерения после нового снимка.
	if err := observer.ObserveGPU(ctx, publicTelemetryGPU(base+12, at.Add(12*time.Nanosecond))); err != nil {
		t.Fatal(err)
	}
	// frame содержит следующее событие GPU, а ok подтверждает его получение после снимка.
	frame, ok := stream.next(t)
	if !ok || frame.kind != "gpu" || publicTelemetryGPUValue(t, frame.data["gpu"]) != strconv.FormatUint(base+12, 10) || frame.id == fresh["cursor"] {
		t.Fatal("подписка после снимка потеряла следующее событие", frame)
	}
	// lastID сохраняет курсор полученного GPU-события для продолжения через Last-Event-ID.
	lastID := frame.id
	stream.close(t)
	// err сообщает об отказе публикации GPU-события между закрытием и возобновлением подписки.
	if err := observer.ObserveGPU(ctx, publicTelemetryGPU(base+13, at.Add(13*time.Nanosecond))); err != nil {
		t.Fatal(err)
	}
	// resumed читает события после lastID через HTTP-заголовок Last-Event-ID.
	resumed := publicTelemetrySubscribe(t, server, "/events", lastID)
	frame, ok = resumed.next(t)
	if !ok || frame.kind != "gpu" || publicTelemetryGPUValue(t, frame.data["gpu"]) != strconv.FormatUint(base+13, 10) || frame.id == lastID {
		t.Fatal("Last-Event-ID повторил прежнее либо пропустил следующее событие", frame)
	}
	resumed.close(t)
	// err сообщает об ошибке закрытия исходного API перед сменой эпохи хранилища.
	if err := a.Close(ctx); err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке закрытия исходного хранилища перед его повторным открытием.
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	// restarted предоставляет сервис новой эпохи, freshAPI — его публичные маршруты, а freshServer —
	// локальный сервер новых подписок.
	_, restarted, freshAPI, freshServer := open()
	publicTelemetryResync(t, freshServer, lastID, "reset")
	// reset содержит пустой оперативный снимок новой эпохи после повторного открытия SQLite.
	reset := integrationAPIRequest(t, freshAPI, "GET", "/events/snapshot", "", 200)
	if reset["gpu"] != nil || len(reset["sessions"].([]any)) != 0 {
		t.Fatal("новый процесс восстановил RAM-телеметрию", reset)
	}
	// newObserver публикует системные GPU-наблюдения новой эпохи хранилища.
	newObserver, err := restarted.OpenSystemObserver(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает системного наблюдателя GPU после повторного открытия хранилища.
		_ = newObserver.Close(ctx)
	})
	// newStream читает только события после пустого снимка новой эпохи.
	newStream := publicTelemetrySubscribe(t, freshServer, "/events?cursor="+url.QueryEscape(reset["cursor"].(string)), "")
	// err сообщает об отказе публикации нового GPU-наблюдения после смены эпохи.
	if err := newObserver.ObserveGPU(ctx, publicTelemetryGPU(base+14, time.Now())); err != nil {
		t.Fatal(err)
	}
	frame, ok = newStream.next(t)
	if !ok || frame.kind != "gpu" || publicTelemetryGPUValue(t, frame.data["gpu"]) != strconv.FormatUint(base+14, 10) {
		// encoded содержит JSON неудачно проверенного SSE-кадра для диагностики теста.
		encoded, _ := json.Marshal(frame.data)
		t.Fatalf("новая подписка после reset не получила новое состояние: %s", encoded)
	}
	newStream.close(t)
}
