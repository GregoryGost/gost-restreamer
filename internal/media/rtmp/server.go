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

// Options не задаёт скрытых URL и не создаёт общий SessionManager.
type Options struct {
	Limits Limits
	Core   *core.Service
	Route  RouteResolver
	Feed   FeedResolver
	Logger *slog.Logger
}

// Result сообщает фактический исход соединения без блокировки его завершения.
type Result struct{ Err error }

// Server владеет принятыми соединениями и присоединяет их при Shutdown.
// Канал Results ограничен числом соединений; потери видны через DroppedResults.
type Server struct {
	opts      Options
	mu        sync.Mutex
	active    map[net.Conn]struct{}
	listeners map[net.Listener]struct{}
	stopping  bool
	changed   chan struct{}
	results   chan Result
	dropped   atomic.Uint64
}

// NewServer проверяет бюджеты до первого сетевого выделения ресурсов.
func NewServer(opts Options) (*Server, error) {
	if err := opts.Limits.Validate(); err != nil {
		return nil, err
	}
	if opts.Core == nil || opts.Route == nil || opts.Feed == nil {
		return nil, core.ErrInvalid
	}
	base := opts.Limits.baseRetained()
	if base > opts.Limits.MaxRetainedBytes {
		return nil, ErrLimit
	}
	return &Server{opts: opts, active: make(map[net.Conn]struct{}), listeners: make(map[net.Listener]struct{}), changed: make(chan struct{}), results: make(chan Result, opts.Limits.MaxConnections)}, nil
}

func (s *Server) Results() <-chan Result { return s.results }
func (s *Server) DroppedResults() uint64 { return s.dropped.Load() }

// ConventionalRoute является явно выбираемым соглашением /<streamID>/<key>.
// Схема не назначает маршрут реального развёртывания приложения.
func ConventionalRoute(u *url.URL) (core.ID, string, error) {
	if u == nil || u.RawQuery != "" || u.Fragment != "" || u.User != nil {
		return "", "", core.ErrInvalid
	}
	parts := strings.Split(strings.TrimPrefix(u.Path, "/"), "/")
	if len(parts) != 2 || parts[0] == "" || parts[1] == "" || parts[0] == "." || parts[0] == ".." {
		return "", "", core.ErrInvalid
	}
	return core.ID(parts[0]), parts[1], nil
}

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
	watchDone := make(chan struct{})
	watchExit := make(chan struct{})
	go func() {
		defer close(watchExit)
		select {
		case <-ctx.Done():
			_ = l.Close()
		case <-watchDone:
		}
	}()
	defer func() {
		close(watchDone)
		<-watchExit
		_ = l.Close()
		s.mu.Lock()
		delete(s.listeners, l)
		s.signalLocked()
		s.mu.Unlock()
	}()
	for {
		conn, err := l.Accept()
		if err != nil {
			shutdownCtx, cancel := context.WithTimeout(context.Background(), s.opts.Limits.ShutdownTimeout)
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
		go func() { _ = s.handle(ctx, conn) }()
	}
}

func (s *Server) report(err error) {
	select {
	case s.results <- Result{Err: err}:
	default:
		s.dropped.Add(1)
	}
}

func (s *Server) register(conn net.Conn) error {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.stopping {
		return core.ErrClosed
	}
	if uint64(len(s.active)) >= uint64(s.opts.Limits.MaxConnections) {
		return ErrLimit
	}
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
	if err := s.register(conn); err != nil {
		_ = conn.Close()
		return err
	}
	return s.handle(ctx, conn)
}

func (s *Server) handle(ctx context.Context, conn net.Conn) (result error) {
	closed := make(chan struct{})
	watchExit := make(chan struct{})
	go func() {
		defer close(watchExit)
		select {
		case <-ctx.Done():
			_ = conn.Close()
		case <-closed:
		}
	}()
	defer func() {
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
	wire, err := newWireConn(conn, s.opts.Limits)
	if err != nil {
		return err
	}
	wire.startupDeadline = time.Now().Add(s.opts.Limits.HandshakeTimeout)
	if err = conn.SetDeadline(wire.startupDeadline); err != nil {
		return err
	}
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
	streamID, key, err := s.opts.Route(rc.URL)
	if err != nil {
		return err
	}
	session, err := s.opts.Core.AuthorizeOBS(ctx, streamID, key)
	if err != nil {
		_ = rc.RejectAction()
		return err
	}
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
	adapter := publication{router: router, wire: wire}
	fence := core.RuntimeSessionFence{SessionID: session.ID, Incarnation: session.Incarnation, Lifecycle: session.Lifecycle}
	lease, err := s.opts.Core.ActivateOBSFor(ctx, fence, streamID, key, func(lease *core.RuntimeLease) (func(), error) {
		var err error
		adapter.source, err = router.BeginSource()
		if err != nil {
			return nil, err
		}
		adapter.publicationID = lease.ID()
		router.BeginPublication(lease.ID())
		return func() { router.EndPublication(lease.ID()); _ = adapter.source.Disconnect() }, nil
	})
	if err != nil {
		return err
	}
	if s.opts.Logger != nil {
		s.opts.Logger.Info("obs_publication", "component", "rtmp", "operation", "activate", "stream_id", streamID, "session_id", session.ID, "lease_id", lease.ID(), "url", rc.URL.String(), "publication_key", key)
	}
	leaseWatchDone := make(chan struct{})
	leaseWatchExit := make(chan struct{})
	go func() {
		defer close(leaseWatchExit)
		select {
		case <-lease.Done():
			_ = conn.Close()
		case <-leaseWatchDone:
		}
	}()
	defer func() {
		cleanupCtx, cancel := context.WithTimeout(context.Background(), s.opts.Limits.ShutdownTimeout)
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
		msg, err := rc.Read()
		if err != nil {
			return err
		}
		adapter.receivedAt = time.Now()
		if err = lease.Do(ctx, func() error { return adapter.message(msg) }); err != nil {
			return err
		}
	}
}

// Shutdown запрещает новые соединения, закрывает собственные сокеты и ждёт их
// завершения. Истечение ctx оставляет незавершённую очистку наблюдаемой повторным вызовом.
func (s *Server) Shutdown(ctx context.Context) error {
	s.mu.Lock()
	s.stopping = true
	conns := make([]net.Conn, 0, len(s.active))
	for c := range s.active {
		conns = append(conns, c)
	}
	listeners := make([]net.Listener, 0, len(s.listeners))
	for l := range s.listeners {
		listeners = append(listeners, l)
	}
	s.mu.Unlock()
	for _, l := range listeners {
		_ = l.Close()
	}
	for _, c := range conns {
		_ = c.Close()
	}
	for {
		s.mu.Lock()
		empty := len(s.active) == 0
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

type publication struct {
	publicationID                core.ID
	receivedAt                   time.Time
	router                       *media.Router
	source                       *media.Source
	wire                         *wireConn
	av1c, asc                    []byte
	description                  av1Description
	video, audio                 media.CodecConfig
	videoRevision, audioRevision uint64
	fps                          core.FrameRate
	ready                        bool
	clock                        uint32
	videoDuration, audioDuration int64
	videoTime, audioTime         time.Duration
	hasVideoTime, hasAudioTime   bool
}

func (p *publication) message(msg message.Message) error {
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
		raw := p.wire.lastConfig
		if len(raw) < 5 {
			return ErrCodec
		}
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

func ioEndSequence() error {
	return fmt.Errorf("%w: завершение последовательности", ErrUnready)
}

func (p *publication) audioConfig(offset int) error {
	raw := p.wire.lastConfig
	if len(raw) <= offset {
		return ErrCodec
	}
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

func (p *publication) videoPacket(t time.Duration, tu []byte) error {
	if err := p.timestamp(t, true); err != nil {
		return err
	}
	if len(p.av1c) == 0 {
		return nil
	}
	f, key, err := p.description.inspect(tu, p.fps)
	if err == nil || errors.Is(err, ErrUnready) {
		if f.Codec == "" {
			f.Codec = core.CodecAV1
		}
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
	config, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: p.source.Epoch(), Revision: p.videoRevision, TimeBase: media.TimeBase{Numerator: 1, Denominator: 1000}, Video: f}, p.av1c)
	if err != nil {
		return err
	}
	if !p.ready {
		if !key {
			return nil
		}
		scale, videoDuration, audioDuration, err := ingestClock(f.FrameRate, p.audio.Info().Audio.SampleRateHz)
		if err != nil {
			return err
		}
		vi := config.Info()
		vi.TimeBase = media.TimeBase{Numerator: 1, Denominator: scale}
		config, err = media.NewCodecConfig(vi, p.av1c)
		if err != nil {
			return err
		}
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
	ticks := int64(t/time.Millisecond) * int64(p.clock/1000)
	packet, err := media.NewPacket(media.PacketInfo{PTS: ticks, DTS: ticks, DurationTicks: p.videoDuration, Keyframe: key}, p.video, tu)
	if err != nil {
		return err
	}
	return p.source.Publish(packet)
}

func (p *publication) audioPacket(t time.Duration, au []byte) error {
	if err := p.timestamp(t, false); err != nil {
		return err
	}
	if len(p.asc) > 0 && codec.ValidateAACAU(p.asc, au) == nil {
		p.router.ObserveIngress(p.publicationID, media.TrackAudio, uint64(len(au)), core.VideoFormat{}, p.audio.Info().Audio)
	}
	if !p.ready {
		return nil
	}
	ticks := int64(t/time.Millisecond) * int64(p.clock/1000)
	packet, err := media.NewPacket(media.PacketInfo{PTS: ticks, DTS: ticks, DurationTicks: p.audioDuration}, p.audio, au)
	if err != nil {
		return err
	}
	return p.source.Publish(packet)
}

func (p *publication) timestamp(t time.Duration, video bool) error {
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
	source, err := p.router.BeginSource()
	if err != nil {
		return err
	}
	p.source = source
	p.ready = false
	p.hasAudioTime = false
	p.hasVideoTime = false
	if len(p.asc) > 0 {
		info := p.audio.Info()
		info.SourceEpoch = source.Epoch()
		p.audio, err = media.NewCodecConfig(info, p.asc)
		if err != nil {
			return err
		}
	}
	return nil
}

func (p *publication) metadata(data amf0.Data) error {
	if len(data) == 0 {
		return nil
	}
	start := 0
	if v, ok := data[0].(string); ok && v == "@setDataFrame" {
		start = 1
	}
	if len(data) <= start+1 {
		return nil
	}
	name, ok := data[start].(string)
	if !ok || name != "onMetaData" {
		return nil
	}
	var object amf0.Object
	switch m := data[start+1].(type) {
	case amf0.Object:
		object = m
	case amf0.ECMAArray:
		object = amf0.Object(m)
	default:
		return ErrCodec
	}
	if v, ok := object.Get("framerate"); ok {
		n, ok := v.(float64)
		if !ok {
			return ErrCodec
		}
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
