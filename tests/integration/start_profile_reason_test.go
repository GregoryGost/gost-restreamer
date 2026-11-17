package integration

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/api"
	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
)

// startProfileReasonRequest отправляет body через зарегистрированный handler и сохраняет точные
// метод, путь, JSON, статус и ответ в журнале t. status задаёт ожидаемый HTTP-код;
// результат декодируется в T только после проверки кода.
func startProfileReasonRequest[T any](t *testing.T, handler http.Handler, method, path string, body any, status int) T {
	t.Helper()
	// encoded содержит точные байты JSON-запроса; err переносит ошибки его кодирования и чтения ответа.
	encoded, err := json.Marshal(body)
	if err != nil {
		t.Fatal(err)
	}
	// request передаёт JSON в рабочий HTTP-маршрутизатор без открытия сетевого порта.
	request := httptest.NewRequest(method, api.Prefix+path, bytes.NewReader(encoded))
	request.Header.Set("Content-Type", "application/json")
	// response сохраняет фактический ответ зарегистрированного обработчика.
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	t.Logf("HTTP request: %s %s body=%s", method, api.Prefix+path, encoded)
	t.Logf("HTTP response: status=%d content_type=%q body=%s", response.Code, response.Header().Get("Content-Type"), response.Body.String())
	if response.Code != status {
		t.Fatalf("статус %d вместо %d", response.Code, status)
	}
	// result принимает публичное представление фактического HTTP-ответа.
	var result T
	if err = json.Unmarshal(response.Body.Bytes(), &result); err != nil {
		t.Fatal(err)
	}
	return result
}

// startProfileReasonSelection проверяет допуск точного профиля перед HTTP-запуском.
// ready требует единственную привязку; обратное значение требует именно ErrNotReady.
func startProfileReasonSelection(t *testing.T, fixture *sessionAcceptanceFixture, ready bool) {
	t.Helper()
	// bindings содержит выбранные варианты; err содержит исходную доменную причину отказа.
	bindings, err := fixture.service.SelectStartBindings(context.Background(), fixture.stream.ID, nil)
	t.Logf("Core.SelectStartBindings: count=%d error=%v is_ErrNotReady=%v", len(bindings), err, errors.Is(err, core.ErrNotReady))
	if ready {
		if err != nil || len(bindings) != 1 {
			t.Fatalf("рабочий профиль не выбран: %+v %v", bindings, err)
		}
	} else if !errors.Is(err, core.ErrNotReady) {
		t.Fatalf("ожидалась предметная неготовность, получено %v", err)
	}
}

// startProfileReasonStartStop проверяет принятие и завершение настоящего запуска и остановки.
// handler обслуживает HTTP, fixture даёт доступ к подтверждению очистки, label различает
// намерения команд; медиа передаются только локальному структурному приёмнику фикстуры.
func startProfileReasonStartStop(t *testing.T, handler http.Handler, fixture *sessionAcceptanceFixture, label string) {
	t.Helper()
	// started содержит принятую квитанцию запуска для последующего ожидания успеха.
	started := startProfileReasonRequest[api.ReceiptDTO](t, handler, "POST", "/streams/"+string(fixture.stream.ID)+"/start", map[string]any{"request_id": label + "-start", "destination_ids": []string{}}, 202)
	started = applicationFlowReceipt(t, handler, started)
	// current содержит наблюдаемое через HTTP сохранённое состояние запущенной сессии.
	current := applicationFlowRequest[api.SessionDTO](t, handler, "GET", "/sessions/"+started.SessionID, nil, 200)
	if current.State != "RUNNING" || len(current.Groups) != 1 {
		t.Fatalf("неполный положительный контроль: %+v", current)
	}
	t.Logf("положительный контроль: session=%s state=%s groups=%d", current.ID, current.State, len(current.Groups))
	// stopped содержит принятую квитанцию остановки того же запуска.
	stopped := startProfileReasonRequest[api.ReceiptDTO](t, handler, "POST", "/sessions/"+started.SessionID+"/stop", map[string]any{"request_id": label + "-stop"}, 202)
	applicationFlowReceipt(t, handler, stopped)
	sessionAcceptanceWait(t, func() bool {
		// Ожидает терминальное состояние и подтверждённую очистку до смены профиля.
		// saved содержит текущее состояние сессии; err содержит отказ его чтения.
		saved, err := fixture.service.GetSession(context.Background(), core.ID(started.SessionID))
		return err == nil && saved.State.Terminal() && !saved.CleanupPending && !saved.Issued
	})
}

// TestStartProfileMismatchExplainsExactRecipeWithoutChangingAdmission воспроизводит запрос запуска после замены полного профиля
// 12 Мбит/p4 на 10 Мбит/p5 при неизменном MediaFormat и прежнем готовом поколении.
// Положительные контроли до изменения и после возврата отделяют несовпадение рецепта
// от общей неготовности, занятой сессии и отсутствующего файла. Кодирование заменено
// структурной генерацией fMP4 существующей фикстуры; NVENC здесь не исполняется.
func TestStartProfileMismatchExplainsExactRecipeWithoutChangingAdmission(t *testing.T) {
	// fixture предоставляет настоящий Store/Goose, Core, FallbackManager и SessionManager.
	fixture := newApplicationFlowFixture(t, sessionAcceptanceBuildHelper(t))
	// handler предоставляет настоящую регистрацию HTTP API над компонентами фикстуры.
	handler := integrationAPI(t, fixture)
	// asset содержит созданную через API заглушку, пока ещё без подготовленного поколения.
	asset := startProfileReasonRequest[api.AssetDTO](t, handler, "POST", "/fallback-assets", map[string]any{"name": "Заглушка"}, 201)
	// policy задаёт выбранную заглушку и те же сроки watchdog, что использует фикстура.
	policy := map[string]any{"asset_id": asset.ID, "watchdog": map[string]any{"video_timeout_ns": 300000000, "audio_timeout_ns": 300000000, "recovery_window_ns": 70000000, "keyframe_timeout_ns": 1000000000}}
	// stream содержит созданный поток с синтетическим ключом публикации.
	stream := startProfileReasonRequest[api.StreamDTO](t, handler, "POST", "/streams", map[string]any{"name": "Поток", "publication_key": "synthetic-profile-key", "fallback": policy}, 201)
	// err переносит ошибки чтения сохранённых объектов и проверки профилей.
	var err error
	fixture.stream, err = fixture.service.GetStream(context.Background(), core.ID(stream.ID))
	if err != nil {
		t.Fatal(err)
	}
	// original задаёт H.264 1920×1080/60, CBR 12 Мбит/с и NVENC p4 с явным аудиоселектором.
	original := publisherProfile(core.CodecH264, 1920, 1080)
	original.Encoding = core.DefaultEncoding(original.Mode, core.VideoEncoderNVENC, core.RateControlCBR)
	original.Encoding.NVENC.Preset = "p4"
	original.VideoEncoding.TargetBitrateBPS = 12000000
	original.VideoEncoding.MaxBitrateBPS = 12000000
	original.VideoEncoding.BufferBits = 24000000
	// saved содержит HTTP-представление полного сохранённого профиля и его CAS-ревизию.
	saved := startProfileReasonRequest[api.ProfileDTO](t, handler, "POST", "/transcode-profiles", map[string]any{"name": "Профиль", "profile": applicationFlowProfileJSON(original)}, 201)
	// configuredGroup связывает именованный профиль с единственной группой потока.
	configuredGroup := startProfileReasonRequest[api.GroupDTO](t, handler, "POST", "/transcode-groups", map[string]any{"name": "Группа", "stream_id": stream.ID, "profile_id": saved.ID}, 201)
	// receiver принимает только локальные структурные пакеты управляемого проигрывателя.
	receiver, _ := newPublisherReceiver(t, false)
	startProfileReasonRequest[api.DestinationDTO](t, handler, "POST", "/destinations", map[string]any{"name": "Локальное назначение", "stream_id": stream.ID, "group_id": configuredGroup.ID, "platform": "custom", "endpoint": "rtmp://" + receiver.listener.Addr().String() + "/live", "stream_key": "synthetic-output-key", "enabled": true}, 201)
	// upload содержит принятое поколение PNG; подготовку и проверку fMP4 выполняет рабочий менеджер.
	upload := applicationFlowUpload(t, handler, asset, applicationFlowPNG(t))
	applicationFlowGeneration(t, handler, upload, "ready")
	// generation содержит точный набор профилей реально опубликованного поколения.
	generation, err := fixture.service.GetGeneration(context.Background(), core.ID(upload.GenerationID))
	if err != nil || len(generation.RequiredProfiles) != 1 || !generation.RequiredProfiles[0].Profile.Equal(original) {
		t.Fatalf("неверное начальное поколение: %+v %v", generation, err)
	}
	t.Logf("готовое поколение: id=%s variant=%s bitrate=%d preset=%s", generation.ID, generation.RequiredProfiles[0].VariantID, generation.RequiredProfiles[0].Profile.VideoEncoding.TargetBitrateBPS, generation.RequiredProfiles[0].Profile.Encoding.NVENC.Preset)
	startProfileReasonSelection(t, fixture, true)
	startProfileReasonStartStop(t, handler, fixture, "original")
	// cases отделяет влияние битрейта, preset и их совместной замены без изменения формата.
	cases := []struct {
		// name задаёт независимый RequestID отрицательного контроля.
		name string
		// bitrate задаёт целевой и максимальный битрейт CBR в битах в секунду.
		bitrate uint64
		// preset выбирает NVENC preset для текущего отрицательного контроля.
		preset string
	}{{"bitrate-only", 10000000, "p4"}, {"preset-only", 12000000, "p5"}, {"combined", 10000000, "p5"}}
	// scenario содержит очередную независимую замену полного рецепта с возвратом к исходному.
	for _, scenario := range cases {
		// changed сохраняет MediaFormat, меняя только указанные параметры полного рецепта.
		changed := original
		changed.VideoEncoding.TargetBitrateBPS = scenario.bitrate
		changed.VideoEncoding.MaxBitrateBPS = scenario.bitrate
		changed.VideoEncoding.BufferBits = 2 * scenario.bitrate
		changed.Encoding.NVENC.Preset = scenario.preset
		if changed.Format != original.Format {
			t.Fatal("контрольный MediaFormat изменился")
		}
		saved = startProfileReasonRequest[api.ProfileDTO](t, handler, "PATCH", "/transcode-profiles/"+saved.ID, map[string]any{"expected_revision": saved.Revision, "profile": applicationFlowProfileJSON(changed)}, 200)
		startProfileReasonSelection(t, fixture, false)
		// problem содержит отказ до принятия команды с независимыми причиной и следующим действием.
		problem := startProfileReasonRequest[map[string]any](t, handler, "POST", "/streams/"+stream.ID+"/start", map[string]any{"request_id": scenario.name + "-start", "destination_ids": []string{}}, 409)
		// detail объясняет отсутствие полного рецепта; action содержит отдельный шаг подготовки.
		detail, _ := problem["detail"].(string)
		action, _ := problem["action"].(string)
		if problem["code"] != "fallback_profile_not_prepared" || !strings.Contains(strings.ToLower(detail), "профил") || !strings.Contains(strings.ToLower(action), "подгот") || detail == action {
			t.Fatalf("отказ не объясняет полный профиль и отдельное действие: %+v", problem)
		}
		// known содержит только фактически прочитанные идентификаторы группы и профиля.
		known, _ := problem["context"].(map[string]any)
		if known["group_id"] != configuredGroup.ID || known["profile_id"] != saved.ID || known["stream_id"] != stream.ID {
			t.Fatalf("отказ потерял известные объекты: %+v", known)
		}
		// prohibited запрещает приписывать отказ формату LIVE, отсутствующему в HTTP-запуске.
		for _, prohibited := range []string{"synthetic-profile-key", "synthetic-output-key", "expected", "actual", "OBS", scenario.name + "-start"} {
			// public содержит полный публичный отказ; encodeErr сообщает об ошибке его кодирования.
			public, encodeErr := json.Marshal(problem)
			if encodeErr != nil {
				t.Fatal(encodeErr)
			}
			if strings.Contains(string(public), prohibited) {
				t.Fatalf("отказ публикует непроверенные или закрытые сведения %q: %s", prohibited, public)
			}
		}
		// rejectedReceipt подтверждает отсутствие принятой команды; rejectionError содержит причину поиска.
		rejectedReceipt, rejectionError := fixture.service.FindReceipt(context.Background(), scenario.name+"-start")
		if !errors.Is(rejectionError, core.ErrNotFound) {
			t.Fatalf("отказ создал квитанцию: %+v %v", rejectedReceipt, rejectionError)
		}
		// unchanged содержит поколение после отказа; err сообщает об ошибке проверки сохранности заглушки.
		unchanged, err := fixture.service.GetGeneration(context.Background(), generation.ID)
		if err != nil || len(unchanged.RequiredProfiles) != 1 || !unchanged.RequiredProfiles[0].Profile.Equal(original) {
			t.Fatalf("профиль готовой заглушки изменился: %+v %v", unchanged, err)
		}
		t.Logf("после PATCH: scenario=%s generation=%s fallback=%d/%s desired=%d/%s MediaFormat_equal=%v; отказ не создал квитанцию", scenario.name, unchanged.ID, original.VideoEncoding.TargetBitrateBPS, original.Encoding.NVENC.Preset, changed.VideoEncoding.TargetBitrateBPS, changed.Encoding.NVENC.Preset, changed.Format == original.Format)
		saved = startProfileReasonRequest[api.ProfileDTO](t, handler, "PATCH", "/transcode-profiles/"+saved.ID, map[string]any{"expected_revision": saved.Revision, "profile": applicationFlowProfileJSON(original)}, 200)
		startProfileReasonSelection(t, fixture, true)
	}
	startProfileReasonStartStop(t, handler, fixture, "restored")
	t.Logf("проверено: 12 Мбит/p4 → 10 Мбит/p5 → 12 Мбит/p4; профиль=%s поколение=%s", saved.ID, generation.ID)
}

// TestAcceptedStartFailureExplainsItsResultThroughSuccessfulReads проверяет различие принятия 202
// и последующего отказа команды: причина шага сохраняется в Core и выдаётся в читаемых ресурсах,
// а произвольная нативная диагностика не становится публичной причиной либо догадкой о GPU.
func TestAcceptedStartFailureExplainsItsResultThroughSuccessfulReads(t *testing.T) {
	// fixture создаёт настоящие менеджер, временные файлы, SQLite и опубликованное поколение.
	fixture := newSessionAcceptanceFixture(t, sessionAcceptanceBuildHelper(t), []string{"rtmp://127.0.0.1:1/live"}, []int{0})
	fixture.config.Group.Player = func(ctx context.Context, binary string, file *os.File, attempt ffmpeg.Attempt) (ffmpeg.Command, error) {
		// Моделирует отказ известного шага после принятия команды, не запуская медиапроцесс.
		return ffmpeg.Command{}, errors.New("synthetic-native-secret /private/internal/file rtmps://secret@localhost")
	}
	fixture.launch(t)
	// handler содержит зарегистрированный API поверх настоящего SessionManager.
	handler := integrationAPI(t, fixture)
	// accepted подтверждает принятие; итог выполнения проверяется отдельным GET со статусом 200.
	accepted := startProfileReasonRequest[api.ReceiptDTO](t, handler, "POST", "/streams/"+string(fixture.stream.ID)+"/start", map[string]any{"request_id": "accepted-start-failure", "destination_ids": []string{}}, 202)
	// terminal содержит доказанный неуспешный результат рабочей границы исполнения.
	terminal := sessionAcceptanceReceipt(t, fixture.service, core.ID(accepted.ID), core.CommandFailed)
	if terminal.FailureReason == nil || terminal.FailureReason.Code != core.FailureSessionStartFailed {
		t.Fatalf("менеджер потерял причину шага: %+v", terminal)
	}
	// receipt возвращает успешное чтение ресурса с неуспешным результатом команды.
	receipt := startProfileReasonRequest[map[string]any](t, handler, "GET", "/commands/"+accepted.ID, nil, 200)
	// failure содержит подготовленные публичные поля результата, без HTTP-статуса отказа принятия.
	failure, _ := receipt["failure"].(map[string]any)
	if failure["code"] != "session_start_failed" || failure["message"] == "" || failure["action"] == nil || failure["message"] == failure["action"] || receipt["status"] != "FAILED" {
		t.Fatalf("чтение квитанции потеряло отдельную причину: %+v", receipt)
	}
	// known содержит только идентификаторы принятой команды и сохранённой Session.
	known, _ := failure["context"].(map[string]any)
	if known["session_id"] != accepted.SessionID || known["command_id"] != accepted.ID {
		t.Fatalf("потерян известный контекст результата: %+v", known)
	}
	// session сохраняет тот же отказ запуска в самостоятельной истории.
	session := startProfileReasonRequest[map[string]any](t, handler, "GET", "/sessions/"+accepted.SessionID, nil, 200)
	// sessionFailure содержит объяснение результата всей Session.
	sessionFailure, _ := session["failure"].(map[string]any)
	if sessionFailure["code"] != "session_start_failed" || session["state"] != "FAILED" {
		t.Fatalf("история Session потеряла причину запуска: %+v", session)
	}
	// public содержит JSON публичной ошибки без полей исторической конфигурации;
	// err сообщает об отказе сериализации.
	public, err := json.Marshal(failure)
	if err != nil {
		t.Fatal(err)
	}
	// secret обозначает каждую диагностическую подробность, запрещённую в публичном объяснении.
	for _, secret := range []string{"synthetic-native-secret", "/private/internal/file", "rtmps://", "GPU", "NVENC"} {
		if strings.Contains(string(public), secret) {
			t.Fatalf("раскрыта либо придумана причина %q: %s", secret, public)
		}
	}
	// replay возвращает тот же долговечный неуспех, не исполняя запуск повторно.
	replay := startProfileReasonRequest[map[string]any](t, handler, "POST", "/streams/"+string(fixture.stream.ID)+"/start", map[string]any{"request_id": "accepted-start-failure", "destination_ids": []string{}}, 202)
	if replay["id"] != receipt["id"] || replay["status"] != "FAILED" {
		t.Fatal("повтор создал новый запуск", replay)
	}
}
