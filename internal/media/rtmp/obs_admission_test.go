package rtmp

import (
	"context"
	"errors"
	"net"
	"net/url"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/bluenviron/gortmplib"
)

// runOBSAdmissionServer запускает настоящий локальный RTMP с заданным портом допуска и его сроком.
// direct выбирает одиночный Handle вместо Serve; очистка отменяет владельца и присоединяет обработчик.
func runOBSAdmissionServer(t *testing.T, f ingestFixture, admit AdmissionResolver, budget time.Duration, direct bool) (*Server, string) {
	t.Helper()
	// server содержит транспорт с новым портом; err сообщает отказ его конфигурации.
	server, err := NewServer(Options{Limits: testLimits(), Core: f.svc, Route: ConventionalRoute, Admit: admit, AdmissionTimeout: budget})
	if err != nil {
		t.Fatal(err)
	}
	// listener выделяет отдельный локальный порт для единственного тестового сервера.
	listener, err := net.Listen("tcp", "127.0.0.1:0")
	if err != nil {
		t.Fatal(err)
	}
	// lifetime ограничивает жизнь Handle/Serve; cancel отзывает её при очистке.
	lifetime, cancel := context.WithCancel(context.Background())
	// done передаёт завершение выбранного обработчика владельцу очистки.
	done := make(chan error, 1)
	go func() {
		// Обслуживает единственное соединение через Handle либо весь локальный слушатель через Serve.
		if direct {
			// conn содержит принятое соединение; acceptErr сообщает отказ локального Accept.
			conn, acceptErr := listener.Accept()
			if acceptErr != nil {
				done <- acceptErr
				return
			}
			done <- server.Handle(lifetime, conn)
			return
		}
		done <- server.Serve(lifetime, listener)
	}()
	t.Cleanup(func() {
		// Закрывает принадлежащий проверке транспорт и ждёт возврата фонового Handle/Serve.
		cancel()
		_ = listener.Close()
		// ctx ограничивает присоединение сервера; stop освобождает срок.
		ctx, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		// err сообщает, удалось ли присоединить все обработчики сервера в срок очистки.
		if err := server.Shutdown(ctx); err != nil {
			t.Error(err)
		}
		select {
		// err содержит исход Handle/Serve; отмена владельца и закрытие слушателя ожидаемы при очистке.
		case err := <-done:
			if !errors.Is(err, context.Canceled) && !errors.Is(err, net.ErrClosed) {
				t.Error("неожиданный исход Handle/Serve", err)
			}
		case <-ctx.Done():
			t.Error("не завершён Handle/Serve", ctx.Err())
		}
	})
	return server, listener.Addr().String()
}

// TestOBSAdmissionShutdownCancelsWaitingHandleAndServe проверяет явную отмену допуска Shutdown,
// когда функция допуска не читает сокет и потому простое закрытие TCP не может закончить её работу.
func TestOBSAdmissionShutdownCancelsWaitingHandleAndServe(t *testing.T) {
	// direct выбирает самостоятельный Handle либо обработчик, зарегистрированный Serve.
	for _, direct := range []bool{true, false} {
		t.Run(map[bool]string{true: "Handle", false: "Serve"}[direct], func(t *testing.T) {
			// Проверяет отмену владельца ожидания без зависимости от чтения закрытого сокета.
			// f содержит изолированную SQLite и Router транспортной границы.
			f := newIngestFixture(t)
			// entered подтверждает достижение функции допуска после publish.
			entered := make(chan struct{})
			// canceled сохраняет фактическую причину отмены функции допуска.
			canceled := make(chan error, 1)
			// server владеет проверяемым обработчиком; address задаёт локальный адрес.
			server, address := runOBSAdmissionServer(t, f, func(ctx context.Context, _ core.ID, _ string) (core.Session, *media.Router, error) {
				// Ждёт исключительно отмены ctx; закрытие сокета само по себе функцию допуска не будит.
				close(entered)
				<-ctx.Done()
				canceled <- ctx.Err()
				return core.Session{}, nil, ctx.Err()
			}, 5*time.Second, direct)
			// clientDone передаёт завершение согласования клиента и позволяет присоединить его.
			clientDone := make(chan error, 1)
			go func() {
				// Пытается пройти Publish.Start, который должен ждать завершения допуска до Shutdown.
				// client содержит успешный клиент, если сервер ошибочно принял публикацию; err сообщает исход.
				client, err := connectClient(t, address, f.stream, f.stream.PublicationKey)
				if client != nil {
					client.Close()
				}
				clientDone <- err
			}()
			select {
			case <-entered:
			case <-time.After(2 * time.Second):
				t.Fatal("publish не дошёл до допуска")
			}
			// ctx ограничивает Shutdown; stop освобождает срок.
			ctx, stop := context.WithTimeout(context.Background(), time.Second)
			defer stop()
			// err сообщает, удалось ли Shutdown отменить и присоединить ожидающий обработчик.
			if err := server.Shutdown(ctx); err != nil {
				t.Fatal("Shutdown не отменил ожидающий обработчик допуска", err)
			}
			select {
			// err содержит причину, которую увидел обработчик допуска после явной остановки ingress.
			case err := <-canceled:
				if !errors.Is(err, context.Canceled) {
					t.Fatal("ожидание завершилось не отменой Shutdown", err)
				}
			case <-ctx.Done():
				t.Fatal(ctx.Err())
			}
			select {
			// err содержит отказ клиента, которому нельзя было отправлять успешный Publish.Start.
			case err := <-clientDone:
				if err == nil {
					t.Fatal("публикация принята до завершения допуска")
				}
			case <-ctx.Done():
				t.Fatal("клиент не завершён")
			}
		})
	}
}

// TestOBSAdmissionDeadlineDoesNotLimitActiveMedia проверяет, что после активации медиа продолжает
// использовать исходный срок Handle/Serve даже после истечения абсолютного срока допуска.
func TestOBSAdmissionDeadlineDoesNotLimitActiveMedia(t *testing.T) {
	// f предоставляет действующий точный fence и маршрутизатор.
	f := newIngestFixture(t)
	// deadline передаёт абсолютную границу допуска, наблюдённую в функции допуска.
	deadline := make(chan time.Time, 1)
	// server обслуживает новый порт; address задаёт локальный адрес публикации.
	server, address := runOBSAdmissionServer(t, f, func(ctx context.Context, stream core.ID, key string) (core.Session, *media.Router, error) {
		// Сохраняет deadline и возвращает реально авторизованную Session с её маршрутизатором.
		// limit содержит границу ctx; present подтверждает наличие срока.
		limit, present := ctx.Deadline()
		if !present {
			return core.Session{}, nil, core.ErrInvalid
		}
		deadline <- limit
		// session содержит реально допущенный запуск; err сообщает отказ ключа или состояния.
		session, err := f.svc.AuthorizeOBS(ctx, stream, key)
		return session, f.router, err
	}, 300*time.Millisecond, false)
	// subscription наблюдает фактические пакеты активированного источника.
	subscription := newSubscription(t, f.router)
	// client содержит принятую RTMP-публикацию; err сообщает отказ согласования.
	client, err := connectClient(t, address, f.stream, f.stream.PublicationKey)
	if err != nil {
		t.Fatal(err)
	}
	sendReady(t, client, 0)
	_ = delivery(t, subscription)
	_ = delivery(t, subscription)
	// limit сохраняет наблюдённую абсолютную границу допуска.
	limit := <-deadline
	// expired обозначает именно истечение исходного deadline; release освобождает таймер проверки.
	expired, release := context.WithDeadline(context.Background(), limit.Add(20*time.Millisecond))
	defer release()
	<-expired.Done()
	sendVideo(t, client, 33, testAV1TU())
	sendAudio(t, client, 33, []byte{4, 5, 6})
	// received содержит реальное видео, отправленное после срока допуска.
	received := delivery(t, subscription)
	if !acceptanceWireTime(received.Packet, 33) {
		t.Fatal("после срока допуска не доставлено новое видео", received.Packet.Info())
	}
	select {
	// result означал бы преждевременное завершение активной публикации по истечении срока допуска.
	case result := <-server.Results():
		t.Fatal("срок допуска завершил активный эфир", result.Err)
	default:
	}
}

// TestOBSAdmissionTimeoutRejectsBeforeAccept проверяет общий абсолютный срок функции допуска и транспорта,
// не позволяя успешному Publish.Start обойти ещё не завершившийся допуск.
func TestOBSAdmissionTimeoutRejectsBeforeAccept(t *testing.T) {
	// f содержит изолированные зависимости RTMP.
	f := newIngestFixture(t)
	// observed сохраняет фактический контекстный исход допуска.
	observed := make(chan error, 1)
	// server предоставляет результат ожидающего обработчика; address задаёт адрес клиента.
	server, address := runOBSAdmissionServer(t, f, func(ctx context.Context, _ core.ID, _ string) (core.Session, *media.Router, error) {
		// Дожидается срока допуска без сетевого чтения и возвращает наблюдённую ошибку ctx.
		<-ctx.Done()
		observed <- ctx.Err()
		return core.Session{}, nil, ctx.Err()
	}, 80*time.Millisecond, false)
	// client должен отсутствовать, пока функция допуска не вернула готовый вход; err содержит отказ запуска.
	client, err := connectClient(t, address, f.stream, f.stream.PublicationKey)
	if err == nil || client != nil {
		t.Fatal("публикация принята без готового допуска")
	}
	if err = serverResult(t, server); !errors.Is(err, context.DeadlineExceeded) {
		t.Fatal("общий deadline не дошёл до допуска", err)
	}
	if !errors.Is(<-observed, context.DeadlineExceeded) {
		t.Fatal("функция допуска завершилась до истечения срока")
	}
}

// TestOBSAdmissionFinalActivationKeepsExactSession проверяет гонку Stop/Start после выбора входа:
// окончательный ActivateOBSFor обязан отклонить старый fence, сохранив новый эфир.
func TestOBSAdmissionFinalActivationKeepsExactSession(t *testing.T) {
	// f содержит исходный работающий запуск и Router.
	f := newIngestFixture(t)
	// replacement сохраняет новый запуск, созданный между выбором и сетевым принятием.
	var replacement core.CommandReceipt
	// server выполняет финальную повторную проверку; address задаёт локальный адрес клиента.
	server, address := runOBSAdmissionServer(t, f, func(ctx context.Context, stream core.ID, key string) (core.Session, *media.Router, error) {
		// Выбирает старый запуск, затем явно завершает его и запускает новый до возврата старого fence.
		// original содержит первоначально разрешённую Session; err сообщает отказ авторизации.
		original, err := f.svc.AuthorizeOBS(ctx, stream, key)
		if err != nil {
			return core.Session{}, nil, err
		}
		// stop содержит принятую команду явного завершения старого запуска.
		stop, err := f.svc.StopSession(ctx, core.SessionCommand{RequestID: "rtmp-replace-stop", SessionID: original.ID})
		if err != nil {
			return core.Session{}, nil, err
		}
		complete(t, f.svc, stop)
		replacement, err = f.svc.StartSession(ctx, core.StartSessionRequest{RequestID: "rtmp-replace-start", StreamID: stream})
		if err != nil {
			return core.Session{}, nil, err
		}
		complete(t, f.svc, replacement)
		return original, f.router, nil
	}, time.Second, false)
	// client может получить wire-успех до окончательной активации; err здесь не определяет её результат.
	client, _ := connectClient(t, address, f.stream, f.stream.PublicationKey)
	if client != nil {
		defer client.Close()
	}
	// err содержит фактический отказ окончательной активации первоначального запуска.
	if err := serverResult(t, server); !errors.Is(err, core.ErrStale) {
		t.Fatal("старый fence перешёл к новому запуску", err)
	}
	// current проверяет сохранность нового эфира после отказа активации прежнего.
	current, err := f.svc.GetSession(context.Background(), replacement.SessionID)
	if err != nil || current.State != core.SessionRunning {
		t.Fatal("отказ прежнего допуска остановил новый эфир", current, err)
	}
}

// obsAcceptFailureConn сохраняет настоящий транспорт и отклоняет записи только после допуска.
type obsAcceptFailureConn struct {
	// Conn выполняет сетевое согласование и все операции, кроме управляемого отказа Write.
	net.Conn
	// fail включает отказ ответа Publish.Start после создания Session в функции допуска.
	fail atomic.Bool
	// failure сохраняет точную причину отказа транспортной записи для проверки AcceptAction.
	failure error
}

// Write выполняет настоящий сетевой обмен до допуска, затем возвращает назначенный отказ ответа.
func (c *obsAcceptFailureConn) Write(data []byte) (int, error) {
	if c.fail.Load() {
		return 0, c.failure
	}
	return c.Conn.Write(data)
}

// TestOBSAdmissionAcceptFailureKeepsAcceptedSession проверяет потерю Publish.Start после
// долговечного холодного запуска. Транспорт не вправе посылать компенсационный StopSession.
func TestOBSAdmissionAcceptFailureKeepsAcceptedSession(t *testing.T) {
	// f содержит реальные доменные метаданные и Router без внешних медиазависимостей.
	f := newIngestFixture(t)
	// ctx ограничивает весь обмен и присоединение тестового транспорта; cancel освобождает срок.
	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	// stop завершает исходную Session фикстуры, чтобы функция допуска действительно выполнила холодный запуск.
	stop, err := f.svc.StopSession(ctx, core.SessionCommand{RequestID: "accept-failure-old-stop", SessionID: f.session.SessionID})
	if err != nil {
		t.Fatal(err)
	}
	complete(t, f.svc, stop)
	// left и right образуют принадлежащий тесту транспорт без внешнего адреса.
	left, right := net.Pipe()
	defer func() {
		// Закрывает оба конца тестового транспорта, включая путь раннего отказа согласования.
		// err сообщает об отказе закрытия серверного конца net.Pipe.
		if err := left.Close(); err != nil {
			t.Error("не закрыт серверный конец тестового канала", err)
		}
		// err сообщает об отказе закрытия клиентского конца net.Pipe.
		if err := right.Close(); err != nil {
			t.Error("не закрыт клиентский конец тестового канала", err)
		}
	}()
	// writeFailure отличает контролируемый отказ AcceptAction от ошибок согласования.
	writeFailure := errors.New("контролируемый отказ Publish.Start")
	// wire сохраняет настоящий net.Pipe до выполнения функции допуска, затем включает ошибку ответа.
	wire := &obsAcceptFailureConn{Conn: left, failure: writeFailure}
	// accepted передаёт принятую Session после успешного доменного Complete.
	accepted := make(chan core.ID, 1)
	// server использует контролируемую потерю ответа после холодного запуска; err сообщает отказ настроек.
	server, err := NewServer(Options{Limits: testLimits(), Core: f.svc, Route: ConventionalRoute, AdmissionTimeout: time.Second, Admit: func(ctx context.Context, stream core.ID, key string) (core.Session, *media.Router, error) {
		// Принимает реальный новый запуск и включает ошибку Write только после его готовности.
		// receipt содержит единственную команду холодного запуска; err сообщает отказ её принятия.
		receipt, err := f.svc.StartSession(ctx, core.StartSessionRequest{RequestID: "accept-failure-cold-start", StreamID: stream})
		if err != nil {
			return core.Session{}, nil, err
		}
		complete(t, f.svc, receipt)
		// session содержит реально авторизованный RUNNING с точным fence нового запуска.
		session, err := f.svc.AuthorizeOBS(ctx, stream, key)
		if err != nil {
			return core.Session{}, nil, err
		}
		accepted <- session.ID
		wire.fail.Store(true)
		return session, f.router, nil
	}})
	if err != nil {
		t.Fatal(err)
	}
	// done подтверждает возврат настоящего Handle после отказа Publish.Start.
	done := make(chan error, 1)
	go func() {
		// Выполняет настоящий протокол RTMP поверх net.Pipe и передаёт причину завершения владельцу теста.
		done <- server.Handle(ctx, wire)
	}()
	// client публикует через правый конец net.Pipe, сохраняя библиотечное согласование и Publish.Start.
	client := &gortmplib.Client{URL: &url.URL{Scheme: "rtmp", Host: "localhost", Path: "/" + string(f.stream.ID), Fragment: f.stream.PublicationKey}, Publish: true, DialContext: func(context.Context, string, string) (net.Conn, error) {
		// Возвращает единственный принадлежащий проверке конец транспорта вместо сетевого соединения.
		return right, nil
	}}
	defer client.Close()
	if err = client.Initialize(ctx); err == nil {
		t.Fatal("отказ Publish.Start стал успехом клиента")
	}
	select {
	// result содержит фактический отказ Handle после созданной Session.
	case result := <-done:
		if !errors.Is(result, writeFailure) {
			t.Fatal("ошибка не достигла AcceptAction", result)
		}
	case <-ctx.Done():
		t.Fatal("Handle не завершён", ctx.Err())
	}
	// sessionID содержит запуск, уже принятый до потери ответа клиенту.
	sessionID := <-accepted
	// current подтверждает сохранность RUNNING после завершения отказавшего Handle.
	current, err := f.svc.GetSession(ctx, sessionID)
	if err != nil || current.State != core.SessionRunning {
		t.Fatal("отказ AcceptAction остановил принятый запуск", current, err)
	}
	// receipts обнаруживает дополнительную компенсационную остановку выбранного запуска.
	receipts, err := f.svc.ListReceipts(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8, SessionID: sessionID}})
	if err != nil || len(receipts.Items) != 1 || receipts.Items[0].Kind != core.CommandStartSession {
		t.Fatal("транспорт создал StopSession после ошибки ответа", receipts, err)
	}
}
