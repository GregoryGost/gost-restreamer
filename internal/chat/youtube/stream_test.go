package youtube

import (
	"context"
	"crypto/ed25519"
	"crypto/rand"
	"crypto/tls"
	"crypto/x509"
	"errors"
	"fmt"
	"math/big"
	"net"
	"slices"
	"strings"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"google.golang.org/genproto/googleapis/rpc/errdetails"
	"google.golang.org/grpc"
	"google.golang.org/grpc/codes"
	"google.golang.org/grpc/credentials"
	"google.golang.org/grpc/metadata"
	"google.golang.org/grpc/status"
	"google.golang.org/protobuf/encoding/protowire"
	"google.golang.org/protobuf/types/known/durationpb"
)

// fixtureStreamMethod берёт путь RPC из package/service/method опубликованного Google stream_list.proto.
const fixtureStreamMethod = "/youtube.api.v3.V3DataLiveChatMessageService/StreamList"

// fixtureWireCodec передаёт байты protobuf через gRPC. Дескриптор адаптера здесь не используется.
type fixtureWireCodec struct{}

// Name возвращает gRPC content subtype protobuf для обмена с клиентом.
func (fixtureWireCodec) Name() string { return "proto" }

// Marshal передаёт готовые байты одного ответа v. Ошибка обнаруживает несогласованную тестовую фикстуру.
func (fixtureWireCodec) Marshal(v any) ([]byte, error) {
	// payload и ok проверяют форму аргумента SendMsg.
	payload, ok := v.([]byte)
	if !ok {
		return nil, fmt.Errorf("неподдерживаемый ответ фикстуры %T", v)
	}
	return payload, nil
}

// Unmarshal копирует data запроса в принадлежащий обработчику v буфер.
func (fixtureWireCodec) Unmarshal(data []byte, v any) error {
	// target и ok проверяют форму аргумента RecvMsg.
	target, ok := v.(*[]byte)
	if !ok {
		return fmt.Errorf("неподдерживаемый запрос фикстуры %T", v)
	}
	*target = slices.Clone(data)
	return nil
}

// fixtureGRPCTLS создаёт временный сертификат для локального сервера и доверие клиента только к нему.
func fixtureGRPCTLS(t *testing.T) (credentials.TransportCredentials, credentials.TransportCredentials) {
	t.Helper()
	// public, private — пара ключей одного теста, err сообщает об отказе криптографического генератора.
	public, private, err := ed25519.GenerateKey(rand.Reader)
	if err != nil {
		t.Fatal(err)
	}
	// now задаёт окно действия сертификата относительно локальных часов.
	now := time.Now()
	// identity подтверждает локальное имя и назначение тестового TLS-сервера.
	identity := &x509.Certificate{
		SerialNumber: big.NewInt(1), DNSNames: []string{"youtube.fixture.invalid"},
		NotBefore: now.Add(-time.Hour), NotAfter: now.Add(time.Hour),
		KeyUsage: x509.KeyUsageDigitalSignature, ExtKeyUsage: []x509.ExtKeyUsage{x509.ExtKeyUsageServerAuth},
	}
	// certBytes и leaf содержат сертификат для цепочки TLS и записи в клиентском хранилище доверия.
	certBytes, err := x509.CreateCertificate(rand.Reader, identity, identity, public, private)
	if err != nil {
		t.Fatal(err)
	}
	leaf, err := x509.ParseCertificate(certBytes)
	if err != nil {
		t.Fatal(err)
	}
	// trust ограничивает локальный тест единственным созданным сертификатом.
	trust := x509.NewCertPool()
	trust.AddCert(leaf)
	return credentials.NewTLS(&tls.Config{
			MinVersion: tls.VersionTLS12, Certificates: []tls.Certificate{{Certificate: [][]byte{certBytes}, PrivateKey: private, Leaf: leaf}},
		}), credentials.NewTLS(&tls.Config{
			MinVersion: tls.VersionTLS12, RootCAs: trust, ServerName: "youtube.fixture.invalid",
		})
}

// useGRPCFixture направляет a к реальному TLS/HTTP2 серверу на выделенном loopback-порту.
// Обработчик handler получает ровно одну структуру запроса как необработанные байты protobuf.
func useGRPCFixture(t *testing.T, a *Adapter, handler func(grpc.ServerStream, []byte) error) {
	t.Helper()
	// listener и err подтверждают владение локальным временным портом.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// serverTLS удостоверяет сервер, clientTLS проверяет его сертификат.
	serverTLS, clientTLS := fixtureGRPCTLS(t)
	// server обрабатывает один путь gRPC, определённый независимо от сгенерированной схемы.
	server := grpc.NewServer(grpc.Creds(serverTLS), grpc.ForceServerCodec(fixtureWireCodec{}),
		grpc.UnknownServiceHandler(func(_ any, stream grpc.ServerStream) error {
			// Обработчик отвергает неверный путь и затем передаёт запрос локальной модели Google.
			// method и ok подтверждают полный RPC path, включая имя пакета.
			method, ok := grpc.MethodFromServerStream(stream)
			if !ok || method != fixtureStreamMethod {
				t.Errorf("Вызван неверный RPC: %q", method)
				return status.Error(codes.Unimplemented, "неверный RPC фикстуры")
			}
			// request получает protobuf-байты клиента без разбора дескриптором адаптера.
			var request []byte
			// err сообщает об отказе транспорта до передачи запроса.
			if err := stream.RecvMsg(&request); err != nil {
				return err
			}
			return handler(stream, request)
		}))
	// done подтверждает выход исключительно тестового gRPC-сервера после Stop.
	done := make(chan error, 1)
	go func() {
		// Горутина владеет циклом приёма TLS-подключений.
		done <- server.Serve(listener)
	}()
	t.Cleanup(func() {
		// Остановка сервера закрывает listener и активные локальные RPC перед выходом теста.
		server.Stop()
		// serveErr сообщает о неожиданном завершении Serve.
		if serveErr := <-done; serveErr != nil && !errors.Is(serveErr, grpc.ErrServerStopped) {
			t.Error(serveErr)
		}
	})
	a.grpcTarget, a.grpcCredentials = "dns:///"+listener.Addr().String(), clientTLS
}

// fixtureWireString добавляет строковое поле number и text к protobuf-буферу dst.
func fixtureWireString(dst []byte, number protowire.Number, text string) []byte {
	return protowire.AppendString(protowire.AppendTag(dst, number, protowire.BytesType), text)
}

// fixtureWireMessage добавляет вложенное сообщение data в поле number буфера dst.
func fixtureWireMessage(dst []byte, number protowire.Number, data []byte) []byte {
	return protowire.AppendBytes(protowire.AppendTag(dst, number, protowire.BytesType), data)
}

// fixtureWireVarint добавляет целочисленное поле number со значением value к буферу dst.
func fixtureWireVarint(dst []byte, number protowire.Number, value uint64) []byte {
	return protowire.AppendVarint(protowire.AppendTag(dst, number, protowire.VarintType), value)
}

// checkFixtureStreamRequest сверяет raw с официальными номерами полей и TLS metadata stream.
// Ожидаемый resume передаётся дословно, включая символы, требующие экранирования в HTTP.
func checkFixtureStreamRequest(t *testing.T, stream grpc.ServerStream, raw []byte, resume string) {
	t.Helper()
	// md содержит входные metadata, ok отмечает, что граничный сервер их действительно получил.
	md, ok := metadata.FromIncomingContext(stream.Context())
	if !ok || !slices.Equal(md.Get("x-goog-api-key"), []string{fixtureAPIKey}) || len(md.Get("authorization")) != 0 {
		t.Error("В gRPC metadata были переданы неверные полномочия")
	}
	// fields собирает строковые поля запроса по их номерам в официальном proto2 Google.
	fields := make(map[protowire.Number][]string)
	for len(raw) > 0 {
		// number и kind задают номер и тип текущего wire-поля, header — длину тега.
		number, kind, header := protowire.ConsumeTag(raw)
		if header < 0 {
			t.Error("Некорректный тег protobuf-запроса")
			return
		}
		raw = raw[header:]
		// consumed задаёт длину значения любого protobuf-поля.
		consumed := protowire.ConsumeFieldValue(number, kind, raw)
		if consumed < 0 {
			t.Error("Некорректное значение protobuf-запроса")
			return
		}
		if kind == protowire.BytesType {
			// text принимает строковое значение без нормализации.
			text, _ := protowire.ConsumeString(raw)
			fields[number] = append(fields[number], text)
		}
		raw = raw[consumed:]
	}
	slices.Sort(fields[100])
	if !slices.Equal(fields[1], []string{fixtureChatID}) || !slices.Equal(fields[100], []string{"authorDetails", "id", "snippet"}) {
		t.Errorf("Неверные protobuf-поля запроса: %v", fields)
	}
	if resume == "" {
		if len(fields[99]) != 0 && !slices.Equal(fields[99], []string{""}) {
			t.Error("Новая попытка неожиданно получила предыдущую позицию")
		}
	} else if !slices.Equal(fields[99], []string{resume}) {
		t.Errorf("Позиция возобновления изменилась: %v", fields[99])
	}
}

// fixtureTextWire строит LiveChatMessage в байтах по схеме Google от 2026-11-12.
// id задаёт идентификатор сообщения, chatID — принадлежность беседе, text — исходный текст.
func fixtureTextWire(id, chatID, text string) []byte {
	// snippet содержит текстовое событие с живыми автором и чатом.
	snippet := fixtureWireVarint(nil, 1, 1)
	snippet = fixtureWireString(snippet, 201, chatID)
	snippet = fixtureWireString(snippet, 301, "UCfixture-author")
	snippet = fixtureWireString(snippet, 4, fixtureStarted)
	snippet = fixtureWireVarint(snippet, 17, 1)
	snippet = fixtureWireString(snippet, 16, "Другой резервный текст")
	snippet = fixtureWireMessage(snippet, 19, fixtureWireString(nil, 1, text))
	// author несёт имя автора и две независимые роли.
	author := fixtureWireString(nil, 10101, "UCfixture-author")
	author = fixtureWireString(author, 103, "Тестовый автор")
	author = fixtureWireString(author, 104, "https://example.invalid/avatar.png")
	author = fixtureWireVarint(author, 4, 1)
	author = fixtureWireVarint(author, 7, 1)
	// message объединяет верхнеуровневый ID, snippet и author_details.
	message := fixtureWireString(nil, 101, id)
	message = fixtureWireMessage(message, 2, snippet)
	return fixtureWireMessage(message, 3, author)
}

// fixtureBatchWire строит LiveChatMessageListResponse с токеном next и упорядоченными messages.
func fixtureBatchWire(next string, messages ...[]byte) []byte {
	// batch начинается с необязательного следующего токена.
	var batch []byte
	if next != "" {
		batch = fixtureWireString(batch, 100602, next)
	}
	// message содержит следующее исходное protobuf-сообщение пакета.
	for _, message := range messages {
		batch = fixtureWireMessage(batch, 1007, message)
	}
	return batch
}

// fixtureDisplayEventWire строит событие указанного eventType с текстом text и признаком отображения show.
// id передаётся без преобразования, неизменный chat ID связывает событие с проверяемым стримом.
func fixtureDisplayEventWire(id string, eventType uint64, text string, show bool) []byte {
	// snippet хранит поля одного события из LiveChatMessageSnippet.
	snippet := fixtureWireVarint(nil, 1, eventType)
	snippet = fixtureWireString(snippet, 201, fixtureChatID)
	snippet = fixtureWireString(snippet, 16, text)
	// display содержит числовое значение логического поля has_display_content в protobuf.
	var display uint64
	if show {
		display = 1
	}
	snippet = fixtureWireVarint(snippet, 17, display)
	return fixtureWireMessage(fixtureWireString(nil, 101, id), 2, snippet)
}

// fixtureReadRequest выдаёт полный принадлежащий одному читателю запрос с исходной позицией resume.
func fixtureReadRequest(resume string) core.ChatReadRequest {
	// credentials содержит синтетический ключ, который должна получить TLS-фикстура.
	_, credentials := fixtureYouTubeAccess(fixtureChannelID, fixtureAPIKey)
	return core.ChatReadRequest{
		Token: core.ChatWorkerToken{19, 23}, ResumeToken: resume,
		Key:         core.ChatWorkerKey{Provider: core.ChatYouTube, AccountID: credentials.AccountID, ChannelID: fixtureChatID},
		Credentials: credentials,
	}
}

// fixtureStreamStatus хранит одну безопасную пару состояния и причины от адаптера.
type fixtureStreamStatus struct {
	// state отражает фазу протокола.
	state core.ChatSourceState
	// reason содержит код причины без upstream-строки.
	reason core.ChatReason
}

// fixtureStreamSink записывает наблюдаемые вызовы адаптера. Читатель наполняет его последовательно.
type fixtureStreamSink struct {
	// token сверяет принадлежность callbacks одной физической попытке.
	token core.ChatWorkerToken
	// messages и statuses сохраняют полученные данные и фазу чтения.
	messages []core.ChatIncomingMessage
	statuses []fixtureStreamStatus
	// gaps сохраняет безопасные признаки возможного пропуска.
	gaps []core.ChatReason
	// connected подаёт сигнал пустого или непустого первого подтверждённого ответа.
	connected chan struct{}
	// staleID моделирует отзыв попытки перед приёмом сообщения с этим исходным ID.
	staleID string
}

// Message добавляет message точной попытки token либо воспроизводит отказ Hub после отзыва.
// Переданный контекст используется адаптером для отмены физического чтения.
func (s *fixtureStreamSink) Message(_ context.Context, token core.ChatWorkerToken, message core.ChatIncomingMessage) error {
	if token != s.token || s.staleID != "" && message.ProviderMessageID == s.staleID {
		return core.ErrStale
	}
	s.messages = append(s.messages, message)
	return nil
}

// Status фиксирует безопасные state и reason точного token. При первом CONNECTED подаёт сигнал тесту.
func (s *fixtureStreamSink) Status(_ context.Context, token core.ChatWorkerToken, state core.ChatSourceState, reason core.ChatReason) error {
	if token != s.token {
		return core.ErrStale
	}
	s.statuses = append(s.statuses, fixtureStreamStatus{state, reason})
	if state == core.ChatConnected && s.connected != nil {
		close(s.connected)
		s.connected = nil
	}
	return reason.Validate()
}

// Gap фиксирует причину reason при сбросе позиции точного token.
func (s *fixtureStreamSink) Gap(_ context.Context, token core.ChatWorkerToken, reason core.ChatReason) error {
	if token != s.token {
		return core.ErrStale
	}
	s.gaps = append(s.gaps, reason)
	return reason.Validate()
}

// TestStreamListWireAndResume проверяет путь RPC и независимые protobuf-поля, пустое подтверждение,
// нормализацию текста и метаданных, отбрасывание чужого чата и возврат последнего токена на разрыве.
func TestStreamListWireAndResume(t *testing.T) {
	// adapter создаёт отдельные физические попытки; request приходит с предыдущей позицией.
	adapter := newYouTubeFixture(t, core.DefaultChatLimits())
	request := fixtureReadRequest("initial+/=")
	// attempts считает фактически достигшие upstream RPC без чтения состояния клиента.
	attempts := make(chan struct{}, 8)
	useGRPCFixture(t, adapter, func(stream grpc.ServerStream, raw []byte) error {
		// Обработчик подтверждает подписку пустым ответом, а затем присылает историю и разрывает RPC.
		attempts <- struct{}{}
		checkFixtureStreamRequest(t, stream, raw, request.ResumeToken)
		// err сообщает о неудачной отправке пустого подтверждения.
		if err := stream.SendMsg(fixtureBatchWire("first")); err != nil {
			return err
		}
		// batch содержит обычный текст, объявление Super Chat, скрытое событие и ID чужого чата.
		batch := fixtureBatchWire("resume+/=", fixtureTextWire("message-a", fixtureChatID, "Привет <live> 🙂"),
			fixtureTextWire("foreign-message", "other-conversation", "Чужой чат"),
			fixtureDisplayEventWire("donation-event", 15, "Спасибо за поддержку", true),
			fixtureDisplayEventWire("invisible-event", 15, "Скрытый текст", false))
		// err сообщает об ошибке транслирования тестовой истории.
		if err := stream.SendMsg(batch); err != nil {
			return err
		}
		return status.Error(codes.Unavailable, fixtureAPIKey)
	})
	// ctx и cancel ограничивают физическую попытку, sink получает внешние события адаптера.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	sink := &fixtureStreamSink{token: request.Token}
	// err содержит только безопасный контракт возобновления, retry извлекает последнюю позицию.
	err := adapter.Read(ctx, request, sink)
	var retry *core.ChatReadRetry
	if !errors.As(err, &retry) || retry.ResumeToken != "resume+/=" || strings.Contains(err.Error(), fixtureAPIKey) {
		t.Fatalf("Разрыв не вернул безопасную последнюю позицию: %T, состояние=%+v", err, sink.statuses)
	}
	if len(attempts) != 1 || !slices.Contains(sink.statuses, fixtureStreamStatus{core.ChatConnected, core.ChatReasonNone}) ||
		!slices.Contains(sink.statuses, fixtureStreamStatus{core.ChatRetrying, core.ChatReasonNetwork}) {
		t.Fatalf("Сбой статусов/числа RPC: %+v, %d", sink.statuses, len(attempts))
	}
	if len(sink.messages) != 2 {
		t.Fatalf("Число отображаемых сообщений из своего чата = %d, ожидалось два", len(sink.messages))
	}
	// message содержит нормализованное проверяемое сообщение с ожидаемым текстом и ролями.
	message := sink.messages[0]
	// roles сопоставляет публичные идентификаторы меток с независимо заданными полями автора.
	roles := make(map[string]bool)
	// badge представляет одну публичную роль автора.
	for _, badge := range message.Badges {
		roles[badge.ID] = true
	}
	if message.ChannelID != fixtureChatID || message.Provider != core.ChatYouTube || message.ProviderMessageID != "message-a" ||
		message.Text != "Привет <live> 🙂" || message.Kind != core.ChatMessageText ||
		message.Author != (core.ChatAuthor{ID: "UCfixture-author", DisplayName: "Тестовый автор", AvatarURL: "https://example.invalid/avatar.png"}) ||
		message.ProviderTimestamp == nil || message.ProviderTimestamp.Format(time.RFC3339) != fixtureStarted || !roles["verified"] || !roles["moderator"] ||
		message.Validate() != nil {
		t.Fatalf("Неверная нормализация Google: %+v", message)
	}
	// announcement содержит отображаемое объявление платформы и отличается от обычного текста.
	announcement := sink.messages[1]
	if announcement.Kind != core.ChatMessageSystem || announcement.ProviderMessageID != "donation-event" ||
		announcement.Text != "Спасибо за поддержку" || announcement.ChannelID != fixtureChatID {
		t.Fatalf("Объявление Super Chat не нормализовалось: %+v", announcement)
	}
}

// TestStreamListStopsAtRejectedMessage проверяет, что отзыв Hub посреди пакета не продвигает
// позицию за необработанное сообщение и не передаёт следующие сообщения устаревшей попытки.
func TestStreamListStopsAtRejectedMessage(t *testing.T) {
	// adapter читает один пакет; request задаёт последнюю позицию предыдущей попытки.
	adapter := newYouTubeFixture(t, core.DefaultChatLimits())
	request := fixtureReadRequest("previous-checkpoint")
	useGRPCFixture(t, adapter, func(stream grpc.ServerStream, raw []byte) error {
		// Обработчик сначала передаёт целый пакет, затем пакет с отозванным сообщением.
		checkFixtureStreamRequest(t, stream, raw, request.ResumeToken)
		// err сообщает, что сервер не передал полностью подтверждаемую позицию первого пакета.
		if err := stream.SendMsg(fixtureBatchWire("first-whole-checkpoint", fixtureTextWire("first", fixtureChatID, "Из первого пакета"))); err != nil {
			return err
		}
		return stream.SendMsg(fixtureBatchWire("unprocessed-checkpoint",
			fixtureTextWire("accepted", fixtureChatID, "Первое"),
			fixtureTextWire("revoked", fixtureChatID, "Второе"),
			fixtureTextWire("after", fixtureChatID, "Третье")))
	})
	// sink отзывает допуск ровно на втором сообщении полученного пакета.
	sink := &fixtureStreamSink{token: request.Token, staleID: "revoked"}
	// ctx и cancel ограничивают ожидание завершения физического чтения.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	// err и retry проверяют, что новая позиция не дошла до Manager после отзыва.
	err := adapter.Read(ctx, request, sink)
	var retry *core.ChatReadRetry
	if !errors.As(err, &retry) || retry.ResumeToken != "first-whole-checkpoint" {
		t.Fatalf("Отзыв потерял позицию полностью принятого пакета: %T", err)
	}
	if len(sink.messages) != 2 || sink.messages[0].ProviderMessageID != "first" || sink.messages[1].ProviderMessageID != "accepted" {
		t.Fatalf("Отзыв не остановил передачу пакета: %d сообщений", len(sink.messages))
	}
}

// TestStreamListCancellationClosesConfirmedRPC подтверждает CONNECTED пустым пакетом и проверяет,
// что отмену наблюдает и клиент, и рантайм сервера, причём RPC стартует только один раз.
func TestStreamListCancellationClosesConfirmedRPC(t *testing.T) {
	// limits задаёт короткий срок первого ответа, который уже подтверждённый RPC переживает.
	limits := core.DefaultChatLimits()
	limits.ConnectTimeout = 400 * time.Millisecond
	// adapter обслуживает физическую попытку; stopped закрывается только после отмены на сервере.
	adapter := newYouTubeFixture(t, limits)
	stopped := make(chan struct{})
	// request стартует без курсора, confirmed подаёт сигнал из Status CONNECTED.
	request := fixtureReadRequest("")
	confirmed := make(chan struct{})
	useGRPCFixture(t, adapter, func(stream grpc.ServerStream, raw []byte) error {
		// Сервер отправляет валидный ответ без сообщений и остаётся активен до физической отмены.
		checkFixtureStreamRequest(t, stream, raw, "")
		// err сообщает, что транспорт отказался доставить пустой ответ.
		if err := stream.SendMsg([]byte{}); err != nil {
			return err
		}
		<-stream.Context().Done()
		close(stopped)
		return stream.Context().Err()
	})
	// deadline и finish ограничивают всю фикстуру, reading и cancel отзывают только этот RPC.
	deadline, finish := context.WithTimeout(t.Context(), 5*time.Second)
	defer finish()
	reading, cancel := context.WithCancel(deadline)
	defer cancel()
	// sink сохраняет результаты одного чтения до его физического возврата.
	sink := &fixtureStreamSink{token: request.Token, connected: confirmed}
	// result сообщает о завершении и причине остановки адаптера.
	result := make(chan error, 1)
	go func() {
		// Горутина даёт тесту дождаться подтверждения перед отменой транспортного контекста.
		result <- adapter.Read(reading, request, sink)
	}()
	awaitYouTubeFixture(t, deadline, confirmed)
	// initialWindow проверяет, что решение об отмене принадлежит Manager после пустого подтверждения.
	initialWindow := time.NewTimer(limits.ConnectTimeout)
	defer initialWindow.Stop()
	select {
	// readErr показывает преждевременное завершение подтверждённого RPC.
	case readErr := <-result:
		t.Fatalf("Пустой ответ не снял срок подключения: %v", readErr)
	case <-initialWindow.C:
	case <-deadline.Done():
		t.Fatal(deadline.Err())
	}
	cancel()
	awaitYouTubeFixture(t, deadline, stopped)
	if !errors.Is(awaitYouTubeFixture(t, deadline, result), context.Canceled) || len(sink.messages) != 0 {
		t.Fatal("Пустой ответ не подтвердил отменяемый RPC либо был опубликован как сообщение")
	}
}

// TestStreamListBoundsHandshakeAndPerResponseSize проверяет два независимых ограничения
// физического чтения. Ожидание первого ответа ограничено по времени, каждый пакет — по размеру.
func TestStreamListBoundsHandshakeAndPerResponseSize(t *testing.T) {
	// cases связывает тип ограничения с наблюдаемым сценарием локального сервера.
	cases := []struct {
		// name отличает ожидание ответа от проверяемого лимита байтов.
		name string
		// oversized выбирает немедленный чрезмерный ответ вместо зависшего первого ответа.
		oversized bool
	}{
		{"initial response timeout", false},
		{"oversized response", true},
	}
	// tc задаёт один тип отказа до публикации каких-либо сообщений.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Каждый подтест открывает отдельное TLS-соединение и проверяет его освобождение.
			// limits ограничивает ожидание ответа и размер одного пакета независимо от ctx теста.
			limits := core.DefaultChatLimits()
			limits.ConnectTimeout = 400 * time.Millisecond
			limits.UpstreamMessageBytes = 512
			// adapter и request задают попытку с уже подтверждённой позицией.
			adapter := newYouTubeFixture(t, limits)
			request := fixtureReadRequest("bounded-resume")
			// stopped закрывается при физическом прекращении обработки запроса сервером.
			stopped := make(chan struct{})
			useGRPCFixture(t, adapter, func(stream grpc.ServerStream, raw []byte) error {
				// Обработчик держит RPC открытым, пока клиент не сработает по нужному пределу.
				defer close(stopped)
				checkFixtureStreamRequest(t, stream, raw, request.ResumeToken)
				if tc.oversized {
					// large содержит валидное protobuf-сообщение больше сетевого ограничения.
					large := fixtureBatchWire("oversized-progress", fixtureTextWire("oversized", fixtureChatID, strings.Repeat("a", 1024)))
					// err сообщает о транспортном отказе отправки большого пакета.
					if err := stream.SendMsg(large); err != nil {
						return err
					}
				}
				<-stream.Context().Done()
				return stream.Context().Err()
			})
			// ctx и cancel обнаруживают зависшее чтение независимо от внутреннего таймера.
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			// sink фиксирует, что непроверенный первый ответ не подтвердил подключение.
			sink := &fixtureStreamSink{token: request.Token}
			// err и retry показывают, что нарушенная граница оставляет предыдущую позицию.
			err := adapter.Read(ctx, request, sink)
			var retry *core.ChatReadRetry
			if !errors.As(err, &retry) || retry.ResumeToken != request.ResumeToken || len(sink.messages) != 0 ||
				slices.Contains(sink.statuses, fixtureStreamStatus{core.ChatConnected, core.ChatReasonNone}) {
				t.Fatalf("Необработанный пакет подтвердил подписку либо изменил позицию: %T, %+v", err, sink.statuses)
			}
			awaitYouTubeFixture(t, ctx, stopped)
		})
	}
}

// TestStreamListRespectsProviderRetryInfo проверяет, что рекомендованная задержка
// возвращается Manager вместе с последней подтверждённой позицией, без ожидания внутри RPC.
func TestStreamListRespectsProviderRetryInfo(t *testing.T) {
	// adapter и request задают одну попытку чтения после успешного пакета предыдущего подключения.
	adapter := newYouTubeFixture(t, core.DefaultChatLimits())
	request := fixtureReadRequest("before-quota")
	// delay превышает весь срок этой проверки, так что физический Read должен вернуться сразу.
	const delay = 47 * time.Second
	useGRPCFixture(t, adapter, func(stream grpc.ServerStream, raw []byte) error {
		// Обработчик передаёт типизированные детали о допустимой паузе со стороны Google.
		checkFixtureStreamRequest(t, stream, raw, request.ResumeToken)
		// upstream и err содержат результат упаковки стандартного сообщения RetryInfo.
		upstream, err := status.New(codes.ResourceExhausted, fixtureAPIKey).
			WithDetails(&errdetails.RetryInfo{RetryDelay: durationpb.New(delay)})
		if err != nil {
			t.Error(err)
			return err
		}
		return upstream.Err()
	})
	// ctx и cancel ограничивают допустимое ожидание возврата, sink принимает безопасный результат.
	ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
	defer cancel()
	sink := &fixtureStreamSink{token: request.Token}
	// err и retry фиксируют одновременно паузу, причину и непрерывную позицию читателя.
	err := adapter.Read(ctx, request, sink)
	var retry *core.ChatReadRetry
	if !errors.As(err, &retry) || retry.RetryAfter != delay || retry.ResumeToken != request.ResumeToken ||
		!slices.Contains(sink.statuses, fixtureStreamStatus{core.ChatRetrying, core.ChatReasonQuota}) {
		t.Fatalf("RPC потерял безопасную паузу квоты: %T, %+v", err, sink.statuses)
	}
}

// TestStreamListSafeTerminationDifferentiatesCauses проверяет безопасные причины отдельно
// для end, disabled, неоднозначного precondition, ограничений доступа, квоты и сети.
func TestStreamListSafeTerminationDifferentiatesCauses(t *testing.T) {
	// cases связывает наблюдаемый wire-код с безопасным результатом по спецификации Google.
	cases := []struct {
		// name различает сценарий в выводе теста.
		name string
		// code задаёт gRPC status; details при наличии содержит подтверждённое имя причины Google.
		code    codes.Code
		details string
		// offlineAt задаёт подтверждённое завершение в ответе вместо gRPC ошибки.
		offlineAt bool
		// expected отражает последний безопасный статус конкретной физической попытки.
		expected fixtureStreamStatus
		// wantGap требует явного сообщения о сбросе подтверждённо недействительной позиции.
		wantGap bool
	}{
		{"ended by response", codes.OK, "", true, fixtureStreamStatus{core.ChatFailed, core.ChatReasonEnded}, false},
		{"ended by detail", codes.FailedPrecondition, "LIVE_CHAT_ENDED", false, fixtureStreamStatus{core.ChatFailed, core.ChatReasonEnded}, false},
		{"disabled", codes.FailedPrecondition, "LIVE_CHAT_DISABLED", false, fixtureStreamStatus{core.ChatFailed, core.ChatReasonDisabled}, false},
		{"ambiguous", codes.FailedPrecondition, "", false, fixtureStreamStatus{core.ChatFailed, core.ChatReasonUnavailable}, false},
		{"private chat", codes.PermissionDenied, "", false, fixtureStreamStatus{core.ChatFailed, core.ChatReasonUnavailable}, false},
		{"absent chat", codes.NotFound, "", false, fixtureStreamStatus{core.ChatFailed, core.ChatReasonUnavailable}, false},
		{"invalid credentials", codes.Unauthenticated, "", false, fixtureStreamStatus{core.ChatAuthRequired, core.ChatReasonCredentials}, false},
		{"provider quota", codes.ResourceExhausted, "", false, fixtureStreamStatus{core.ChatRetrying, core.ChatReasonQuota}, false},
		{"network", codes.Unavailable, "", false, fixtureStreamStatus{core.ChatRetrying, core.ChatReasonNetwork}, false},
		{"invalid resume", codes.InvalidArgument, "PAGE_TOKEN_INVALID", false, fixtureStreamStatus{core.ChatRetrying, core.ChatReasonUpstreamGap}, true},
	}
	// tc содержит один независимый сценарий протокольного завершения.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Подтест поднимает собственный TLS RPC для одного кода и проверяет все наружные сигналы.
			// adapter читает только эту фикстуру, request имеет предыдущую позицию.
			adapter := newYouTubeFixture(t, core.DefaultChatLimits())
			request := fixtureReadRequest("some-cursor")
			useGRPCFixture(t, adapter, func(stream grpc.ServerStream, raw []byte) error {
				// Обработчик возвращает только ожидаемый ответ без доступа к дескриптору адаптера.
				checkFixtureStreamRequest(t, stream, raw, request.ResumeToken)
				if tc.offlineAt {
					return stream.SendMsg(fixtureWireString(nil, 2, fixtureStarted))
				}
				// upstream содержит искусственно секретную строку, которую нельзя публиковать.
				upstream := status.New(tc.code, fixtureAPIKey)
				if tc.details != "" {
					// detailed добавляет подтверждённую причину Google, err выявляет ошибку упаковки метаданных.
					detailed, err := upstream.WithDetails(&errdetails.ErrorInfo{Domain: "youtube.googleapis.com", Reason: tc.details})
					if err != nil {
						t.Error(err)
						return upstream.Err()
					}
					upstream = detailed
				}
				return upstream.Err()
			})
			// ctx и cancel ограничивают время, sink принимает публичные результаты.
			ctx, cancel := context.WithTimeout(t.Context(), 5*time.Second)
			defer cancel()
			sink := &fixtureStreamSink{token: request.Token}
			// err проверяется на утечку секретного текста и на полноту типизированного повторения.
			err := adapter.Read(ctx, request, sink)
			if err != nil && strings.Contains(err.Error(), fixtureAPIKey) {
				t.Error("Обработанная ошибка RPC раскрыла текст площадки")
			}
			if len(sink.statuses) == 0 || sink.statuses[len(sink.statuses)-1] != tc.expected {
				t.Fatalf("Публичные статусы = %+v, ожидался последний %+v", sink.statuses, tc.expected)
			}
			if len(sink.messages) != 0 {
				t.Error("Служебная ошибка опубликована как сообщение чата")
			}
			if tc.wantGap {
				// retry должен вернуть пустую позицию для новой попытки вместе с сигналом возможного пропуска.
				var retry *core.ChatReadRetry
				if !slices.Equal(sink.gaps, []core.ChatReason{core.ChatReasonUpstreamGap}) || !errors.As(err, &retry) || retry.ResumeToken != "" {
					t.Fatalf("Недействительная позиция не сброшена с сигналом пробела: %v, %T", sink.gaps, err)
				}
			} else if len(sink.gaps) != 0 {
				t.Fatalf("Неожиданный сброс позиции: %v", sink.gaps)
			}
		})
	}
}
