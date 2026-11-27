package api_test

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/api"
	"github.com/GregoryGost/gost-restreamer/internal/chatwire"
	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// heldChatSnapshotWriter задерживает Flush успешного снимка, сохраняя наблюдение за сроком записи.
type heldChatSnapshotWriter struct {
	// apiAcceptanceStreamWriter сохраняет HTTP-статус, тело и срок записи под блокировкой.
	*apiAcceptanceStreamWriter
	// resume разрешает физически завершить уже начатый Flush.
	resume <-chan struct{}
}

// Flush отмечает начало передачи снимка и ждёт, пока тест разрешит её завершить.
func (w *heldChatSnapshotWriter) Flush() {
	w.apiAcceptanceStreamWriter.Flush()
	<-w.resume
}

// TestChatSnapshotHoldsAdmissionUntilFlush проверяет отказ 503, пока единственный слот занят
// реальным HTTP-ответом, и успешное чтение после завершения передачи.
func TestChatSnapshotHoldsAdmissionUntilFlush(t *testing.T) {
	// limits позволяет выдать только один снимок за раз.
	limits := core.DefaultChatLimits()
	limits.Snapshots = 1
	// fixture содержит временную БД, handler обслуживает публичный маршрут снимка.
	fixture, handler, _ := newChatStreamSelfFixture(t, limits)
	// stream адресует существующий поток, err сообщает об отказе его создания.
	stream, err := fixture.service.CreateStream(t.Context(), core.Stream{Name: "Лента", PublicationKey: "local-snapshot-key"})
	if err != nil {
		t.Fatal(err)
	}
	// path выбирает тот же снимок для удерживаемого, отклонённого и повторного запроса.
	path := api.Prefix + "/chat/streams/" + string(stream.ID) + "/snapshot"
	// resume разблокирует передачу, release закрывает канал один раз до ожидания обработчиков при очистке.
	resume := make(chan struct{})
	release := sync.OnceFunc(func() {
		// Снимает удержание Flush, чтобы обработчик вернул свой слот до закрытия фикстуры.
		close(resume)
	})
	defer release()
	// slow удерживает первый успешный ответ, done подтверждает возврат HTTP-обработчика.
	slow := &heldChatSnapshotWriter{apiAcceptanceStreamWriter: newAPIAcceptanceStreamWriter(), resume: resume}
	done := make(chan struct{})
	go func() {
		// Обслуживает первую выдачу снимка вплоть до завершения удерживаемого транспорта.
		defer close(done)
		handler.ServeHTTP(slow, httptest.NewRequest(http.MethodGet, path, nil).WithContext(t.Context()))
	}()
	t.Cleanup(func() {
		// Дожидается первого обработчика после того, как отложенный вызов снимет удержание Flush.
		apiAcceptanceAwait(t, done)
	})
	apiAcceptanceAwait(t, slow.flushed)

	// busy принимает публичную ошибку второго клиента, busyDone подтверждает ответ без ожидания свободного слота.
	busy, _, busyDone := chatStreamSelfServe(t, handler, path)
	apiAcceptanceAwait(t, busyDone)
	assertChatAdmissionProblem(t, busy, http.StatusServiceUnavailable, "service_busy")

	release()
	apiAcceptanceAwait(t, done)
	assertChatAdmissionSnapshot(t, slow.apiAcceptanceStreamWriter, stream.ID)

	// recovered подтверждает, что после завершения Flush место доступно следующему HTTP-клиенту.
	recovered := newAPIAcceptanceStreamWriter()
	handler.ServeHTTP(recovered, httptest.NewRequest(http.MethodGet, path, nil))
	assertChatAdmissionSnapshot(t, recovered, stream.ID)
}

// TestChatReadRejectsUnknownStream проверяет, что у отсутствующего потока нет снимка или SSE
// и его запросы не занимают места, нужные существующему потоку.
func TestChatReadRejectsUnknownStream(t *testing.T) {
	// limits оставляет по одному месту для каждого проверяемого вида запроса.
	limits := core.DefaultChatLimits()
	limits.Snapshots, limits.SSEClients, limits.SSEClientsPerStream = 1, 1, 1
	// fixture создаёт настоящий поток в SQLite, handler проверяет его через публичные маршруты.
	fixture, handler, _ := newChatStreamSelfFixture(t, limits)
	// stream даёт контрольный существующий объект, err сообщает об отказе его создания.
	stream, err := fixture.service.CreateStream(t.Context(), core.Stream{Name: "Доступная лента", PublicationKey: "local-known-key"})
	if err != nil {
		t.Fatal(err)
	}
	// missing задаёт корректный ID, которого нет во временной базе.
	const missing = "/chat/streams/absent-stream"
	// resource выбирает один из двух маршрутов под общим префиксом потока.
	for _, resource := range []string{"/snapshot", "/events"} {
		// ctx ограничивает запрос, если неизвестный поток ошибочно принят для SSE.
		// cancel освобождает таймер после ответа.
		ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
		// response поддерживает транспортные сроки и принимает ответ об отсутствующем потоке.
		response := newAPIAcceptanceStreamWriter()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, api.Prefix+missing+resource, nil).WithContext(ctx))
		cancel()
		assertChatAdmissionProblem(t, response, http.StatusNotFound, "resource_not_found")
	}

	// path адресует существующий поток, snapshot принимает его обычный снимок после отказов.
	path := api.Prefix + "/chat/streams/" + string(stream.ID)
	snapshot := newAPIAcceptanceStreamWriter()
	handler.ServeHTTP(snapshot, httptest.NewRequest(http.MethodGet, path+"/snapshot", nil))
	assertChatAdmissionSnapshot(t, snapshot, stream.ID)
	assertChatAdmissionEvents(t, handler, path+"/events")
}

// TestChatSnapshotRecoversAfterMetadataLimit проверяет, что слишком большой снимок отклоняется
// целиком и возвращается после сокращения метаданных через штатный API.
func TestChatSnapshotRecoversAfterMetadataLimit(t *testing.T) {
	// limits оставляет достаточно места для компактного снимка с одним источником.
	limits := core.DefaultChatLimits()
	limits.Snapshots = 1
	limits.SnapshotMetadataBytes = 768
	// fixture предоставляет мигрированную SQLite, handler обслуживает чтение и обновление конфигурации.
	fixture, handler, _ := newChatStreamSelfFixture(t, limits)
	// stream и destination связывают источник с запрашиваемой лентой.
	stream, _, _, destination := apiAcceptanceSeedConfiguration(t, fixture)
	// account содержит синтетические серверные полномочия, err сообщает об отказе их записи.
	account, err := fixture.service.CreateChatAccount(t.Context(), core.ChatAccount{Provider: core.ChatYouTube},
		core.ChatCredentialBundle{YouTube: &core.YouTubeChatCredentials{APIKey: acceptancePrivatePrefix + "snapshot-key"}}, "snapshot-account")
	if err != nil {
		t.Fatal(err)
	}
	// source содержит допустимое имя, которое вместе с остальными полями исчерпывает бюджет снимка.
	source, err := fixture.service.CreateChatSource(t.Context(), core.ChatSource{
		DestinationID: destination.ID, AccountID: account.Account.ID, Provider: core.ChatYouTube,
		Name: strings.Repeat("я", 256), ChannelHint: "UCsnapshot", Options: core.ChatOptions{Version: 1, YouTube: &core.YouTubeChatOptions{}},
	})
	if err != nil {
		t.Fatal(err)
	}
	// path выбирает снимок с тем же источником до и после сокращения имени.
	path := api.Prefix + "/chat/streams/" + string(stream.ID) + "/snapshot"
	// oversized принимает Problem Details вместо частично записанного снимка.
	oversized := newAPIAcceptanceStreamWriter()
	handler.ServeHTTP(oversized, httptest.NewRequest(http.MethodGet, path, nil))
	assertChatAdmissionProblem(t, oversized, http.StatusServiceUnavailable, "service_unavailable")

	apiAcceptanceRequest(t, handler, http.MethodPatch, "/chat-sources/"+string(source.ID),
		fmt.Sprintf(`{"expected_revision":%d,"name":"Короткое имя"}`, source.Revision), http.StatusOK)
	// recovered принимает снимок после успешного обновления конфигурации и освобождения слота.
	recovered := newAPIAcceptanceStreamWriter()
	handler.ServeHTTP(recovered, httptest.NewRequest(http.MethodGet, path, nil))
	// snapshot возвращает источники из успешного HTTP-ответа, включая обновлённое имя.
	snapshot := assertChatAdmissionSnapshot(t, recovered, stream.ID)
	if len(snapshot.Sources) != 1 || snapshot.Sources[0].ID != source.ID || snapshot.Sources[0].Name != "Короткое имя" {
		t.Fatalf("После сокращения метаданных снимок вернул источники %v", snapshot.Sources)
	}
}

// chatWriterWithoutDeadline передаёт запись и Flush, оставляя установку срока недоступной.
type chatWriterWithoutDeadline struct {
	// ResponseWriter принимает статус, заголовки и тело.
	http.ResponseWriter
	// Flusher сохраняет поддержку Flush, чтобы отказ был вызван именно отсутствием срока записи.
	http.Flusher
}

// chatShortWriter сохраняет только часть переданного фрагмента, имитируя неполную запись транспорта.
type chatShortWriter struct {
	// apiAcceptanceStreamWriter позволяет проверить фактический срок и отсутствие Flush.
	*apiAcceptanceStreamWriter
}

// Write передаёт все байты p, кроме последнего. Возвращённое число позволяет
// обработчику обнаружить неполную запись даже при nil от транспорта.
func (w chatShortWriter) Write(p []byte) (int, error) {
	return w.apiAcceptanceStreamWriter.Write(p[:max(0, len(p)-1)])
}

// TestChatReadReleasesAdmissionAfterTransportFailure проверяет отказ до записи,
// прекращение неполной передачи и повторное использование места для обоих маршрутов чтения.
func TestChatReadReleasesAdmissionAfterTransportFailure(t *testing.T) {
	// limits оставляет по одному месту для снимка и подписки, чтобы обнаружить утечку после отказа.
	limits := core.DefaultChatLimits()
	limits.Snapshots, limits.SSEClients, limits.SSEClientsPerStream = 1, 1, 1
	// fixture создаёт временную ленту, handler проводит каждый запрос через публичный API.
	fixture, handler, _ := newChatStreamSelfFixture(t, limits)
	// stream адресует существующую ленту, err сообщает об отказе её создания.
	stream, err := fixture.service.CreateStream(t.Context(), core.Stream{Name: "Проверка транспорта", PublicationKey: "local-transport-key"})
	if err != nil {
		t.Fatal(err)
	}
	// base оставляет одинаковым поток для обоих способов чтения.
	base := api.Prefix + "/chat/streams/" + string(stream.ID)
	// scenarios связывает путь с публичным кодом отказа при отсутствии транспортного срока.
	scenarios := []struct {
		// resource выбирает снимок или поток событий.
		resource string
		// code различает отказ снимка и отдельную причину недоступности транспорта SSE.
		code string
	}{
		{"snapshot", "service_unavailable"},
		{"events", "sse_transport_unavailable"},
	}
	// scenario проверяет оба транспортных сбоя и следующее успешное чтение для одного маршрута.
	for _, scenario := range scenarios {
		// Подтест использует одну ленту для отказов и последующего запроса с исправным транспортом.
		t.Run(scenario.resource, func(t *testing.T) {
			// path направляет все три запроса в один HTTP-обработчик.
			path := base + "/" + scenario.resource
			// ctx ограничивает оба ошибочных запроса на случай, если обработчик продолжит SSE.
			// cancel освобождает таймер при выходе из подтеста.
			ctx, cancel := context.WithTimeout(t.Context(), 3*time.Second)
			defer cancel()
			// unavailable принимает Problem Details от транспорта, поддерживающего только Flush.
			unavailable := newAPIAcceptanceStreamWriter()
			handler.ServeHTTP(chatWriterWithoutDeadline{ResponseWriter: unavailable, Flusher: unavailable}, httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx))
			assertChatAdmissionProblem(t, unavailable, http.StatusServiceUnavailable, scenario.code)

			// short принимает неполный фрагмент первого ответа и регистрирует попытки его Flush.
			short := newAPIAcceptanceStreamWriter()
			handler.ServeHTTP(chatShortWriter{apiAcceptanceStreamWriter: short}, httptest.NewRequest(http.MethodGet, path, nil).WithContext(ctx))
			// body и status подтверждают начало успешного ответа, unsafe отмечает запись без установленного срока.
			body, status, unsafe := short.snapshot()
			if ctx.Err() != nil || status != http.StatusOK || body == "" || unsafe || !short.deadline.IsZero() {
				t.Fatalf("Не завершена неполная запись со снятием срока: статус %d, тело %q, срок %v", status, body, short.deadline)
			}
			select {
			case <-short.flushed:
				t.Fatal("Неполный фрагмент был передан через Flush")
			default:
			}

			if scenario.resource == "events" {
				assertChatAdmissionEvents(t, handler, path)
				return
			}
			// recovered принимает полный снимок после отказов в том же слоте.
			recovered := newAPIAcceptanceStreamWriter()
			handler.ServeHTTP(recovered, httptest.NewRequest(http.MethodGet, path, nil))
			assertChatAdmissionSnapshot(t, recovered, stream.ID)
		})
	}
}

// assertChatAdmissionEvents проверяет начало контрольной SSE-подписки и дожидается её завершения.
// handler обслуживает запрос по адресу path.
func assertChatAdmissionEvents(t *testing.T, handler *api.API, path string) {
	t.Helper()
	// events принимает начало SSE, cancel и done позволяют отменить запрос и дождаться возврата обработчика.
	events, cancel, done := chatStreamSelfServe(t, handler, path)
	apiAcceptanceAwait(t, events.flushed)
	// body содержит первый комментарий, status и unsafe подтверждают успешную запись со сроком.
	body, status, unsafe := events.snapshot()
	if status != http.StatusOK || unsafe || !strings.HasPrefix(body, ": connected\n\n") {
		t.Fatalf("Ожидалось начало SSE-подписки, получено %d %q", status, body)
	}
	cancel()
	apiAcceptanceAwait(t, done)
}

// assertChatAdmissionProblem проверяет HTTP-статус, MIME-тип и публичный код ожидаемого отказа.
// writer содержит ответ, wantStatus и wantCode задают ожидаемый статус и код.
func assertChatAdmissionProblem(t *testing.T, writer *apiAcceptanceStreamWriter, wantStatus int, wantCode string) {
	t.Helper()
	// body и status содержат результат завершившегося HTTP-запроса.
	body, status, _ := writer.snapshot()
	// problem отделяет поля публичной ошибки от остального ответа.
	var problem api.ProblemDTO
	// err сообщает, удалось ли разобрать тело целиком как объект Problem Details.
	err := json.Unmarshal([]byte(body), &problem)
	if err != nil || status != wantStatus || problem.Status != wantStatus || problem.Code != wantCode ||
		!strings.HasPrefix(writer.Header().Get("Content-Type"), "application/problem+json") || strings.Contains(body, acceptancePrivatePrefix) {
		t.Fatalf("Ожидалась ошибка %d %s, получено %d %s (разбор: %v)", wantStatus, wantCode, status, body, err)
	}
}

// assertChatAdmissionSnapshot проверяет, что ответ в writer содержит снимок потока streamID и непустой курсор.
func assertChatAdmissionSnapshot(t *testing.T, writer *apiAcceptanceStreamWriter, streamID core.ID) chatwire.SnapshotDTO {
	t.Helper()
	// body содержит полное тело, status подтверждает успех, unsafe отмечает запись без транспортного срока.
	body, status, unsafe := writer.snapshot()
	// snapshot принимает проверенное публичное представление для дальнейших утверждений теста.
	var snapshot chatwire.SnapshotDTO
	// err сообщает, удалось ли разобрать полный JSON-ответ.
	err := json.Unmarshal([]byte(body), &snapshot)
	if err != nil || status != http.StatusOK || unsafe || snapshot.StreamID != streamID || snapshot.Cursor == "" ||
		strings.Contains(body, acceptancePrivatePrefix) {
		t.Fatalf("Ожидался полный снимок потока %s, получено %d %s (разбор: %v)", streamID, status, body, err)
	}
	return snapshot
}
