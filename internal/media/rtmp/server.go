package rtmp

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"net"
	"net/url"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/bluenviron/gortmplib"
	"github.com/bluenviron/gortmplib/pkg/amf0"
	"github.com/bluenviron/gortmplib/pkg/message"
)

// RouteResolver явно сопоставляет URL публикации со Stream и постоянным ключом.
type RouteResolver func(*url.URL) (core.ID, string, error)

// FeedResolver возвращает фактический Router владельца Session. Функция выполняется
// вне RuntimeAuthority и не должна создавать Session или заменять её состояние в core.
type FeedResolver func(core.Session) (*media.Router, error)

// Options задаёт ограничения входящих соединений и зависимости проверки, маршрутизации и
// журналирования публикаций.
type Options struct {
	// Limits задаёт бюджеты и сроки входящих соединений.
	Limits Limits
	// Core проверяет Stream, ключ публикации, активную Session и точный RuntimeLease.
	Core *core.Service
	// Route сопоставляет URL входящей публикации с Stream и постоянным ключом.
	Route RouteResolver
	// Feed возвращает маршрутизатор действующей сессии для допуска публикации.
	Feed FeedResolver
	// Logger при наличии записывает исходы соединений и сведения активированной публикации.
	Logger *slog.Logger
}

// Result сообщает фактический исход соединения без блокировки его завершения.
type Result struct { /* Err содержит фактическую причину завершения обслуженного соединения либо nil. */
	Err error
}

// Server владеет принятыми соединениями и присоединяет их при Shutdown.
// Канал Results ограничен числом соединений; потери видны через DroppedResults.
type Server struct {
	// opts хранит проверенные зависимости и ограничения сервера.
	opts Options
	// mu защищает активные соединения, слушатели и начало остановки.
	mu sync.Mutex
	// active содержит принятые во владение соединения до завершения их обработки.
	active map[net.Conn]struct{}
	// listeners содержит слушатели, закрываемые сервером при остановке.
	listeners map[net.Listener]struct{}
	// stopping запрещает регистрацию новых слушателей и соединений после начала Shutdown.
	stopping bool
	// changed закрывается при завершении соединения или слушателя для пробуждения ожиданий.
	changed chan struct{}
	// results содержит ограниченную очередь исходов обслуженных соединений.
	results chan Result
	// dropped атомарно считает результаты, не попавшие в заполненный канал results.
	dropped atomic.Uint64
}

// NewServer проверяет бюджеты до первого сетевого выделения ресурсов.
func NewServer(opts Options) (*Server, error) {
	// err сообщает о некорректных бюджетах входящего сервера до выделения ресурсов.
	if err := opts.Limits.ValidateIngress(); err != nil {
		return nil, err
	}
	if opts.Core == nil || opts.Route == nil || opts.Feed == nil {
		return nil, core.ErrInvalid
	}
	return &Server{opts: opts, active: make(map[net.Conn]struct{}), listeners: make(map[net.Listener]struct{}), changed: make(chan struct{}), results: make(chan Result, opts.Limits.MaxConnections)}, nil
}

// Results возвращает ограниченный канал исходов соединений; заполнение канала учитывается через
// DroppedResults.
func (s *Server) Results() <-chan Result { return s.results }

// DroppedResults возвращает число исходов соединений, пропущенных из-за заполненного Results.
func (s *Server) DroppedResults() uint64 { return s.dropped.Load() }

// ConventionalRoute является явно выбираемым соглашением /<streamID>/<key>.
// Схема не назначает маршрут реального развёртывания приложения.
func ConventionalRoute(u *url.URL) (core.ID, string, error) {
	if u == nil || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", "", core.ErrInvalid
	}
	// parts содержит две части условного маршрута: идентификатор Stream и постоянный ключ.
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || parts[0] == "." || parts[0] == ".." {
		return "", "", core.ErrInvalid
	}
	return core.ID(parts[0]), parts[1], nil
}

// Пробуждает ожидания завершения соединений и создаёт следующий канал уведомления при удерживаемой
// s.mu.
func (s *Server) signalLocked() { close(s.changed); s.changed = make(chan struct{}) }

// Serve принимает только локально переданный слушатель и владеет его закрытием.
// Отмена ctx инициирует Shutdown; ошибка слушателя также присоединяет соединения.
func (s *Server) Serve(ctx context.Context, l net.Listener) error {
	if l == nil {
		return core.ErrInvalid
	}
	s.mu.Lock()
	if s.stopping {
		s.mu.Unlock()
		_ = l.Close()
		return core.ErrClosed
	}
	s.listeners[l] = struct{}{}
	s.mu.Unlock()
	// watchDone останавливает наблюдение контекста при выходе Serve.
	watchDone := make(chan struct{})
	// watchExit закрывается после выхода наблюдателя контекста Serve.
	watchExit := make(chan struct{})
	go func() {
		// Закрывает слушатель при отмене контекста Serve и закрывает watchExit при выходе.

		defer close(watchExit)
		select {
		case <-ctx.Done():
			_ = l.Close()
		case <-watchDone:
		}
	}()
	defer func() {
		// Останавливает наблюдение контекста, ждёт его выхода, закрывает слушатель и удаляет его из учёта
		// сервера.

		close(watchDone)
		<-watchExit
		_ = l.Close()
		s.mu.Lock()
		delete(s.listeners, l)
		s.signalLocked()
		s.mu.Unlock()
	}()
	for {
		// conn содержит очередное принятое соединение. err сообщает об отказе Accept.
		conn, err := l.Accept()
		if err != nil {
			// shutdownCtx ограничивает остановку сервера после отказа слушателя. cancel
			// освобождает срок.
			shutdownCtx, cancel := context.WithTimeout(context.Background(), s.opts.Limits.ShutdownTimeout)
			// stopErr содержит результат присоединения соединений после отказа слушателя.
			stopErr := s.Shutdown(shutdownCtx)
			cancel()
			if ctx.Err() != nil {
				return errors.Join(ctx.Err(), stopErr)
			}
			return errors.Join(err, stopErr)
		}
		if err = s.register(conn); err != nil {
			_ = conn.Close()
			s.report(err)
			continue
		}
		go func() {
			// Обслуживает зарегистрированное соединение; закрытие, учёт и сообщение результата выполняет handle.
			_ = s.handle(ctx, conn)
		}()
	}
}

// Публикует исход соединения без ожидания читателя; при заполненном канале увеличивает счётчик
// пропусков.
func (s *Server) report(err error) {
	select {
	case s.results <- Result{Err: err}:
	default:
		s.dropped.Add(1)
	}
}

// Под блокировкой проверяет остановку, предел соединений и отсутствие повторной регистрации, затем
// принимает соединение во владение Server.
func (s *Server) register(conn net.Conn) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping {
		return core.ErrClosed
	}
	if uint64(len(s.active)) >= uint64(s.opts.Limits.MaxConnections) {
		return ErrLimit
	}
	// exists показывает, было ли соединение уже зарегистрировано этим сервером.
	if _, exists := s.active[conn]; exists {
		return core.ErrInvalid
	}
	s.active[conn] = struct{}{}
	return nil
}

// Handle синхронно обслуживает и закрывает одно соединение. Предел соединений
// общий с Serve; вызывающий владеет горутиной, если запускает Handle асинхронно.
func (s *Server) Handle(ctx context.Context, conn net.Conn) error {
	if conn == nil {
		return core.ErrInvalid
	}
	// err сообщает об остановке сервера, переполнении или повторной регистрации соединения.
	if err := s.register(conn); err != nil {
		_ = conn.Close()
		return err
	}
	return s.handle(ctx, conn)
}

// Обслуживает зарегистрированное соединение: ограничивает запуск, проверяет публикацию через core и
// передаёт сообщения только внутри действующего RuntimeLease. Отмена или отзыв закрывают сокет; при
// выходе ждёт наблюдателей и пытается отозвать допуск с отдельным ограниченным контекстом, включая
// ошибку отзыва в результат.
func (s *Server) handle(ctx context.Context, conn net.Conn) (result error) {
	// closed останавливает наблюдателя контекста после завершения обработки соединения.
	closed := make(chan struct{})
	// watchExit закрывается после выхода наблюдателя контекста соединения.
	watchExit := make(chan struct{})
	go func() {
		// Закрывает соединение при отмене контекста обработки и сообщает о своём выходе через watchExit.

		defer close(watchExit)
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-closed:
		}
	}()
	defer func() {
		// Закрывает соединение, дожидается наблюдателя контекста, публикует результат и удаляет соединение из
		// активного учёта.

		_ = conn.Close()
		close(closed)
		<-watchExit
		s.report(result)
		if result != nil && s.opts.Logger != nil {
			s.opts.Logger.Error("obs_connection", "component", "rtmp", "operation", "handle", "error", result)
		}
		s.mu.Lock()
		delete(s.active, conn)
		s.signalLocked()
		s.mu.Unlock()
	}()
	// wire ограничивает поток байтов перед передачей RTMP-библиотеке. err сообщает о некорректном
	// резерве соединения.
	wire, err := newWireConn(conn, s.opts.Limits)
	if err != nil {
		return err
	}
	wire.startupDeadline = time.Now().Add(s.opts.Limits.HandshakeTimeout)
	if err = conn.SetDeadline(wire.startupDeadline); err != nil {
		return err
	}
	// rc выполняет серверное согласование и типизированный разбор сообщений поверх ограничителя.
	rc := gortmplib.ServerConn{RW: wire}
	if err = rc.Initialize(); err != nil {
		return fmt.Errorf("RTMP Initialize: %w", err)
	}
	if err = rc.AcceptConn(); err != nil {
		return fmt.Errorf("RTMP AcceptConn: %w", err)
	}
	if !rc.Publish {
		return core.ErrNotReady
	}
	// streamID содержит выбранный маршрутом Stream. key содержит отдельный постоянный ключ
	// публикации.
	streamID, key, err := s.opts.Route(rc.URL)
	if err != nil {
		return err
	}
	// session содержит действующую Session, разрешённую для Stream и ключа.
	session, err := s.opts.Core.AuthorizeOBS(ctx, streamID, key)
	if err != nil {
		_ = rc.RejectAction()
		return err
	}
	// router содержит маршрутизатор, возвращённый владельцем разрешённой сессии.
	router, err := s.opts.Feed(session)
	if err != nil {
		return err
	}
	if router == nil {
		return core.ErrNotReady
	}
	if err = rc.AcceptAction(); err != nil {
		return fmt.Errorf("RTMP AcceptAction: %w", err)
	}
	// adapter хранит состояние кодеков и шкалы текущего допуска входящей публикации.
	adapter := publication{router: router, wire: wire}
	// fence сохраняет точную идентичность разрешённой Session для повторной проверки при активации.
	fence := core.RuntimeSessionFence{SessionID: session.ID, Incarnation: session.Incarnation, Lifecycle: session.Lifecycle}
	// lease содержит допуск OBS, ограничивающий обработку сообщений и отзыв источника.
	lease, err := s.opts.Core.ActivateOBSFor(ctx, fence, streamID, key, func(lease *core.RuntimeLease) (func(), error) {
		// Создаёт источник маршрутизатора для выданного допуска OBS, связывает наблюдения с ID допуска и
		// возвращает действие отзыва.

		// err принимает результат создания нового источника маршрутизатора при установке
		// допуска.
		var err error
		adapter.source, err = router.BeginSource()
		if err != nil {
			return nil, err
		}
		adapter.publicationID = lease.ID()
		router.BeginPublication(lease.ID())
		return func() {
			// Завершает наблюдения публикации данного допуска и отключает текущий источник adapter, включая
			// источник после смены конфигурации.
			router.EndPublication(lease.ID())
			_ = adapter.source.Disconnect()
		}, nil
	})
	if err != nil {
		return err
	}
	if s.opts.Logger != nil {
		s.opts.Logger.Info("obs_publication", "component", "rtmp", "operation", "activate", "stream_id", streamID, "session_id", session.ID, "lease_id", lease.ID(), "url", rc.URL.String(), "publication_key", key)
	}
	// leaseWatchDone останавливает наблюдение отзыва допуска при завершении обработки.
	leaseWatchDone := make(chan struct{})
	// leaseWatchExit закрывается после выхода наблюдателя отзыва допуска.
	leaseWatchExit := make(chan struct{})
	go func() {
		// Закрывает соединение после отзыва допуска OBS и закрывает leaseWatchExit при выходе.

		defer close(leaseWatchExit)
		select {
		case <-lease.Done():
			_ = conn.Close()
		case <-leaseWatchDone:
		}
	}()
	defer func() {
		// Отзывает допуск с отдельным ограниченным контекстом, присоединяет ошибку к результату и дожидается
		// выхода наблюдателя допуска.

		// cleanupCtx ограничивает независимую попытку отзыва допуска. cancel освобождает срок.
		cleanupCtx, cancel := context.WithTimeout(context.Background(), s.opts.Limits.ShutdownTimeout)
		// err содержит результат отзыва допуска, присоединяемый к исходу соединения.
		err := lease.Revoke(cleanupCtx)
		cancel()
		result = errors.Join(result, err)
		close(leaseWatchDone)
		<-leaseWatchExit
	}()
	if err = conn.SetDeadline(time.Time{}); err != nil {
		return err
	}
	wire.startupDeadline = time.Time{}
	for {
		// msg содержит очередное типизированное сообщение RTMP. err сообщает об отказе чтения
		// или разбора.
		msg, err := rc.Read()
		if err != nil {
			return err
		}
		adapter.receivedAt = time.Now()
		if err = lease.Do(ctx, func() error {
			// Обрабатывает принятое сообщение через adapter в пределах проверки действующего допуска OBS.
			return adapter.message(msg)
		}); err != nil {
			return err
		}
	}
}

// Shutdown запрещает новые соединения, закрывает собственные сокеты и ждёт их
// завершения. Истечение ctx оставляет незавершённую очистку наблюдаемой повторным вызовом.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.stopping = true
	// conns сохраняет снимок активных соединений для закрытия вне блокировки.
	conns := make([]net.Conn, 0, len(s.active))
	// c обозначает соединение при создании снимка остановки.
	for c := range s.active {
		conns = append(conns, c)
	}
	// listeners сохраняет снимок принадлежащих серверу слушателей для закрытия вне блокировки.
	listeners := make([]net.Listener, 0, len(s.listeners))
	// l обозначает слушатель при создании снимка остановки.
	for l := range s.listeners {
		listeners = append(listeners, l)
	}
	s.mu.Unlock()
	// l обозначает слушатель, закрываемый при Shutdown.
	for _, l := range listeners {
		_ = l.Close()
	}
	// c обозначает активное соединение, закрываемое при Shutdown.
	for _, c := range conns {
		_ = c.Close()
	}
	for {
		s.mu.Lock()
		// empty показывает, завершилась ли обработка всех зарегистрированных соединений.
		empty := len(s.active) == 0
		// changed сохраняет текущий канал уведомления до снятия блокировки и ожидания.
		changed := s.changed
		s.mu.Unlock()
		if empty {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-changed:
		}
	}
}

// publication переводит AV1/AAC текущего допуска OBS в конфигурации и пакеты маршрутизатора.
type publication struct {
	// publicationID связывает входные измерения с точным RuntimeLease публикации.
	publicationID core.ID
	// receivedAt сохраняет время получения текущего сообщения для учёта подтверждённого видео.
	receivedAt time.Time
	// router содержит маршрутизатор разрешённой сессии.
	router *media.Router
	// source содержит текущий допуск источника, заменяемый при границе конфигурации.
	source *media.Source
	// wire предоставляет исходные байты последней конфигурации до преобразований библиотеки.
	wire *wireConn
	// av1c хранит копию исходных заголовков AV1. asc хранит копию исходной AudioSpecificConfig.
	av1c, asc []byte
	// description сохраняет проверенную последовательность AV1 и подтверждённые между TU сведения.
	description av1Description
	// video содержит действующую видеоконфигурацию. audio содержит действующую аудиоконфигурацию.
	video, audio media.CodecConfig
	// videoRevision считает принятые конфигурации AV1. audioRevision считает принятые конфигурации
	// AAC.
	videoRevision, audioRevision uint64
	// fps сохраняет проверенную частоту кадров из метаданных.
	fps core.FrameRate
	// ready показывает наличие согласованной пары конфигураций после независимого видеостарта.
	ready bool
	// clock задаёт общую точную частоту отсчётов в секунду для обеих дорожек.
	clock uint32
	// videoDuration содержит длительность кадра в отсчётах clock. audioDuration содержит
	// длительность 1024 аудиосэмплов в тех же отсчётах.
	videoDuration, audioDuration int64
	// videoTime сохраняет последнюю входную миллисекундную метку видео. audioTime сохраняет
	// последнюю метку аудио.
	videoTime, audioTime time.Duration
	// hasVideoTime отмечает установленную предыдущую видеометку. hasAudioTime отмечает установленную
	// предыдущую аудиометку.
	hasVideoTime, hasAudioTime bool
}

// Разбирает сообщение текущей публикации AV1/AAC: обновляет конфигурации, метаданные или передаёт AU;
// завершение последовательности и неподдержанные формы возвращают ошибку.
func (p *publication) message(msg message.Message) error {
	// m содержит текущее RTMP-сообщение в распознанной конкретной форме.
	switch m := msg.(type) {
	case *message.DataAMF0:
		return p.metadata(m.Payload)
	case *message.VideoExMetadata:
		if m.FourCC != message.FourCCAV1 {
			return ErrCodec
		}
		return p.metadata(m.Payload)
	case *message.VideoExSequenceStart:
		if m.FourCC != message.FourCCAV1 || m.AV1Config == nil {
			return ErrCodec
		}
		// raw ссылается на исходное полное сообщение конфигурации AV1 из ограничителя.
		raw := p.wire.lastConfig
		if len(raw) < 5 {
			return ErrCodec
		}
		// d содержит разобранное описание AV1. err сообщает о повреждении или неподдержанной
		// конфигурации.
		d, err := parseAV1Config(raw[5:])
		if err != nil {
			return err
		}
		if err = p.invalidate(); err != nil {
			return err
		}
		p.av1c = append([]byte(nil), raw[5:]...)
		p.description = d
		p.videoRevision++
		p.ready = false
	case *message.Audio:
		if m.Codec != message.CodecMPEG4Audio {
			return ErrCodec
		}
		if m.AACType == message.AudioAACTypeConfig {
			return p.audioConfig(2)
		}
		return p.audioPacket(m.DTS, m.AU)
	case *message.AudioExSequenceStart:
		if m.FourCC != message.FourCCMP4A {
			return ErrCodec
		}
		return p.audioConfig(5)
	case *message.AudioExCodedFrames:
		if m.FourCC != message.FourCCMP4A {
			return ErrCodec
		}
		return p.audioPacket(m.DTS, m.Payload)
	case *message.VideoExCodedFrames:
		if m.FourCC != message.FourCCAV1 {
			return ErrCodec
		}
		return p.videoPacket(m.DTS, m.Payload)
	case *message.VideoExFramesX:
		// FramesX не является принятой здесь нормативной AV1 CodedFrames формой.
		return fmt.Errorf("%w: требуется AV1 CodedFrames", ErrCodec)
	case *message.VideoExSequenceEnd, *message.AudioExSequenceEnd:
		return ioEndSequence()
	case *message.Video, *message.AudioExMultitrack, *message.VideoExMultitrack:
		return ErrCodec
	}
	return nil
}

// Возвращает ErrUnready с причиной завершения последовательности для прекращения текущего приёма.
func ioEndSequence() error {
	return fmt.Errorf("%w: завершение последовательности", ErrUnready)
}

// Извлекает ASC после заданного префикса, проверяет AAC-LC и отзывает готовый источник перед новой
// ревизией; повторный старт ждёт согласованного видео.
func (p *publication) audioConfig(offset int) error {
	// raw ссылается на исходное сообщение аудиоконфигурации до удаления префикса.
	raw := p.wire.lastConfig
	if len(raw) <= offset {
		return ErrCodec
	}
	// f содержит фактический AAC-формат ASC. err сообщает об отказе разбора.
	f, err := AudioFormatASC(raw[offset:])
	if err != nil {
		return err
	}
	if err = p.invalidate(); err != nil {
		return err
	}
	p.audioRevision++
	p.audio, err = media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackAudio, SourceEpoch: p.source.Epoch(), Revision: p.audioRevision, TimeBase: media.TimeBase{Numerator: 1, Denominator: 1000}, Audio: f}, raw[offset:])
	if err != nil {
		return err
	}
	p.asc = append([]byte(nil), raw[offset:]...)
	p.ready = false
	return nil
}

// Проверяет метку и TU, учитывает подтверждённый вход и ждёт полной пары конфигураций с независимым
// стартом. После готовности публикует исходный TU с точной общей шкалой и PTS=DTS.
func (p *publication) videoPacket(t time.Duration, tu []byte) error {
	// err сообщает о некорректной или уменьшившейся входной метке видео.
	if err := p.timestamp(t, true); err != nil {
		return err
	}
	if len(p.av1c) == 0 {
		return nil
	}
	// f содержит подтверждённый формат TU либо пока неполное описание. key показывает независимость
	// по сжатым данным. err сообщает о повреждении или нехватке сведений.
	f, key, err := p.description.inspect(tu, p.fps)
	if err == nil || errors.Is(err, ErrUnready) {
		if f.Codec == "" {
			f.Codec = core.CodecAV1
		}
		// receivedAt выбирает сохранённое время получения сообщения либо текущее время при его
		// отсутствии.
		receivedAt := p.receivedAt
		if receivedAt.IsZero() {
			receivedAt = time.Now()
		}
		p.router.ObserveIngressAt(p.publicationID, media.TrackVideo, uint64(len(tu)), f, core.AudioFormat{}, receivedAt)
	}
	if errors.Is(err, ErrUnready) {
		return nil
	}
	if err != nil {
		return err
	}
	if len(p.asc) == 0 {
		return nil
	}
	if p.videoRevision == 0 {
		return ErrCodec
	}
	// config содержит видеоконфигурацию для фактического формата до назначения общей точной шкалы.
	config, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: p.source.Epoch(), Revision: p.videoRevision, TimeBase: media.TimeBase{Numerator: 1, Denominator: 1000}, Video: f}, p.av1c)
	if err != nil {
		return err
	}
	if !p.ready {
		if !key {
			return nil
		}
		// scale содержит общую частоту отсчётов. videoDuration содержит точную длительность
		// видеокадра. audioDuration содержит точную длительность AAC-единицы. err сообщает о
		// непредставимой общей шкале.
		scale, videoDuration, audioDuration, err := ingestClock(f.FrameRate, p.audio.Info().Audio.SampleRateHz)
		if err != nil {
			return err
		}
		// vi содержит описание видеоконфигурации для замены миллисекундной шкалы на точную
		// общую.
		vi := config.Info()
		vi.TimeBase = media.TimeBase{Numerator: 1, Denominator: scale}
		config, err = media.NewCodecConfig(vi, p.av1c)
		if err != nil {
			return err
		}
		// ai содержит описание аудиоконфигурации для назначения той же общей шкалы.
		ai := p.audio.Info()
		ai.TimeBase = vi.TimeBase
		p.audio, err = media.NewCodecConfig(ai, p.asc)
		if err != nil {
			return err
		}
		if err = p.source.UpdateConfigs(config, p.audio); err != nil {
			return err
		}
		p.video = config
		p.clock = scale
		p.videoDuration = videoDuration
		p.audioDuration = audioDuration
		p.ready = true
	}
	// AV1 CodedFrames несёт один TU и единственную метку T. Локальный порт
	// сохраняет порядок TU и представляет расписание T точно в обоих полях;
	// это не восстановление отсутствующего DTS кодировщика.
	// ticks содержит исходную RTMP-метку видео, точно выраженную в отсчётах общей шкалы.
	ticks := int64(t/time.Millisecond) * int64(p.clock/1000)
	// packet содержит исходный AV1 TU с общей шкалой, равными PTS/DTS и проверенной независимостью.
	packet, err := media.NewPacket(media.PacketInfo{PTS: ticks, DTS: ticks, DurationTicks: p.videoDuration, Keyframe: key}, p.video, tu)
	if err != nil {
		return err
	}
	return p.source.Publish(packet)
}

// Проверяет метку и учитывает вход при пригодном AAC-префиксе; после готовности публикует исходную AU
// в общей шкале с длительностью 1024 сэмпла.
func (p *publication) audioPacket(t time.Duration, au []byte) error {
	// err сообщает о некорректной или уменьшившейся входной метке аудио.
	if err := p.timestamp(t, false); err != nil {
		return err
	}
	if len(p.asc) > 0 && codec.ValidateAACAU(p.asc, au) == nil {
		p.router.ObserveIngress(p.publicationID, media.TrackAudio, uint64(len(au)), core.VideoFormat{}, p.audio.Info().Audio)
	}
	if !p.ready {
		return nil
	}
	// ticks содержит исходную RTMP-метку аудио в отсчётах общей шкалы.
	ticks := int64(t/time.Millisecond) * int64(p.clock/1000)
	// packet содержит исходную AAC AU с равными PTS/DTS и длительностью 1024 сэмпла. err сообщает об
	// отказе создания.
	packet, err := media.NewPacket(media.PacketInfo{PTS: ticks, DTS: ticks, DurationTicks: p.audioDuration}, p.audio, au)
	if err != nil {
		return err
	}
	return p.source.Publish(packet)
}

// Проверяет целую неотрицательную миллисекундную метку в диапазоне uint32 и неубывание отдельно для
// каждой дорожки; нарушение возвращает ErrDTSRegression.
func (p *publication) timestamp(t time.Duration, video bool) error {
	// last указывает на последнюю метку выбранной дорожки. has указывает на признак её наличия.
	last, has := &p.audioTime, &p.hasAudioTime
	if video {
		last, has = &p.videoTime, &p.hasVideoTime
	}
	if t < 0 || t > time.Duration(^uint32(0))*time.Millisecond || t%time.Millisecond != 0 || (*has && t < *last) {
		return media.ErrDTSRegression
	}
	*last = t
	*has = true
	return nil
}

// invalidate создаёт явную границу конфигурации до приёма новых пакетов.
// Вызывается только под lease.Do; функция отзыва всегда видит последний Source этого допуска.
func (p *publication) invalidate() error {
	if !p.ready {
		return nil
	}
	// source содержит новый сегмент маршрутизатора на границе конфигурации. err сообщает об отказе
	// его создания.
	source, err := p.router.BeginSource()
	if err != nil {
		return err
	}
	p.source = source
	p.ready = false
	p.hasAudioTime = false
	p.hasVideoTime = false
	if len(p.asc) > 0 {
		// info содержит прежнее описание аудио для переноса на новый SourceEpoch.
		info := p.audio.Info()
		info.SourceEpoch = source.Epoch()
		p.audio, err = media.NewCodecConfig(info, p.asc)
		if err != nil {
			return err
		}
	}
	return nil
}

// Принимает framerate из onMetaData в Object либо ECMAArray, включая @setDataFrame; некорректный тип
// или изменение установленной частоты возвращают ErrCodec.
func (p *publication) metadata(data amf0.Data) error {
	if len(data) == 0 {
		return nil
	}
	// start задаёт позицию имени onMetaData после необязательного @setDataFrame.
	start := 0
	// v содержит возможное строковое имя обёртки. ok подтверждает строковый тип.
	if v, ok := data[0].(string); ok && v == "@setDataFrame" {
		start = 1
	}
	if len(data) <= start+1 {
		return nil
	}
	// name содержит имя сообщения метаданных. ok подтверждает строковый тип.
	name, ok := data[start].(string)
	if !ok || name != "onMetaData" {
		return nil
	}
	// object принимает AMF Object либо ECMAArray с метаданными.
	var object amf0.Object
	// m содержит распознанную форму объекта метаданных.
	switch m := data[start+1].(type) {
	case amf0.Object:
		object = m
	case amf0.ECMAArray:
		object = amf0.Object(m)
	default:
		return ErrCodec
	}
	// v содержит значение поля framerate. ok показывает присутствие поля.
	if v, ok := object.Get("framerate"); ok {
		// n содержит числовой FPS. ok подтверждает числовой тип значения.
		n, ok := v.(float64)
		if !ok {
			return ErrCodec
		}
		// rate содержит проверенное рациональное представление FPS. err сообщает о
		// неподдержанном или непредставимом значении.
		rate, err := metadataFrameRate(n)
		if err != nil {
			return err
		}
		if p.fps.Validate() == nil && !p.fps.Equal(rate) {
			return fmt.Errorf("%w: framerate изменился", ErrCodec)
		}
		p.fps = rate
	}
	return nil
}
