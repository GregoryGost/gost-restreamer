package chat

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"slices"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
)

// chatTestRecord создаёт начальную ревизию синтетической записи id для подготовки тестовых снимков.
func chatTestRecord(id core.ID) core.Record {
	// at задаёт микросекундно представимое UTC-время синтетической записи.
	at := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	return core.Record{ID: id, Revision: 1, CreatedAt: at, UpdatedAt: at}
}

// chatManagerTestFixture предоставляет родителя и три выходных наблюдателя для проверки
// нескольких выходов одного назначения и соседнего источника общей платформенной беседы.
type chatManagerTestFixture struct {
	// store хранит конфигурацию и управление в тестовой SQLite.
	store *storage.Store
	// service предоставляет чтение и изменение текущих настроек.
	service *core.Service
	// stream определяет поток, чей снимок проверяется.
	stream core.Stream
	// session и parent определяют сохраняемый запуск и его оперативного родителя.
	session core.Session
	parent  *core.SessionObserver
	// outputs содержит два выхода первого назначения и один выход соседнего назначения.
	outputs []*core.OutputObserver
	// sources содержит по одному источнику на назначение в порядке возрастания ID.
	sources []core.ChatSource
}

// newChatManagerTestFixture проверяет Manager на настоящей SQLite без запуска медиапроцессов.
// t владеет очисткой, ctx ограничивает создание. Выходы имеют успешные квитанции запуска,
// но наблюдатели ещё не подтвердили фактическую передачу.
func newChatManagerTestFixture(t *testing.T, ctx context.Context) chatManagerTestFixture {
	t.Helper()
	// store и service задают изолированные зависимости фикстуры.
	store, service := openChatTestStore(t, ctx)
	// asset задаёт допустимую ссылку политики заглушки. err передаёт ошибки подготовки записей.
	asset, err := service.CreateAsset(ctx, "Тестовая заглушка")
	chatTestOK(t, err)
	// policy задаёт простую политику тестового потока без запуска заглушки.
	policy := core.FallbackPolicy{AssetID: asset.ID, Watchdog: core.WatchdogPolicy{VideoTimeout: time.Second, AudioTimeout: time.Second, RecoveryWindow: time.Second, KeyframeTimeout: time.Second}}
	// stream содержит конфигурацию потока.
	stream, err := service.CreateStream(ctx, core.Stream{Name: "Проверка чатов", PublicationKey: "synthetic-test-key", Fallback: policy})
	chatTestOK(t, err)
	// format задаёт допустимые форматы снимков выхода без фактических медиаданных.
	format := core.MediaFormat{
		Video: core.VideoFormat{Codec: core.CodecAV1, Width: 2560, Height: 1440, FrameRate: core.FrameRate{Numerator: 60, Denominator: 1}, CodecProfile: "main", PixelFormat: "yuv420p", BitDepth: 8, Color: core.ColorInfo{Range: "limited", Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"}},
		Audio: core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", SampleRateHz: 48000, Channels: 2, ChannelLayout: "stereo"},
	}
	// profile содержит сохранённый профиль тестовой группы.
	profile, err := service.CreateProfile(ctx, core.TranscodeProfile{Name: "Тестовый профиль", Profile: core.GroupProfile{
		Mode: core.ModeCopy, LiveAudioMode: core.LiveAudioCopy, Format: format,
		VideoEncoding: core.VideoEncoding{TargetBitrateBPS: 6_000_000, MaxBitrateBPS: 6_000_000, BufferBits: 12_000_000, GOPFrames: 120, ClosedGOP: true},
		AudioEncoding: core.AudioEncoding{BitrateBPS: 192_000},
		Encoding:      core.DefaultEncoding(core.ModeCopy, core.VideoEncoderSVTAV1, core.RateControlCBR),
	}})
	chatTestOK(t, err)
	// group связана с профилем и потоком, общими для трёх медиавыходов.
	group, err := service.CreateGroup(ctx, core.TranscodeGroup{Name: "Тестовая группа", StreamID: stream.ID, ProfileID: profile.ID})
	chatTestOK(t, err)
	// account хранит синтетические полномочия, общие для обоих источников.
	account, err := service.CreateChatAccount(ctx, core.ChatAccount{Provider: core.ChatYouTube}, core.ChatCredentialBundle{YouTube: &core.YouTubeChatCredentials{APIKey: "synthetic-api-key"}}, "initial-write")
	chatTestOK(t, err)
	// options содержит допустимые настройки постоянного канала YouTube.
	options, err := core.NewChatOptions(core.ChatYouTube)
	chatTestOK(t, err)
	// destinations и sources накапливают две пары назначение и источник.
	var destinations []core.Destination
	var sources []core.ChatSource
	// name задаёт имя одного из двух назначений в пределах общей группы.
	for _, name := range []string{"Первое назначение", "Соседнее назначение"} {
		// destination сохраняется с синтетическим адресом, который тест не открывает.
		destination, err := service.CreateDestination(ctx, core.Destination{Name: name, StreamID: stream.ID, GroupID: group.ID, Platform: "custom", Endpoint: "rtmps://example.invalid/live", StreamKey: "synthetic-output-key", Enabled: true})
		chatTestOK(t, err)
		// source связывает назначение с общим аккаунтом и постоянным каналом.
		source, err := service.CreateChatSource(ctx, core.ChatSource{DestinationID: destination.ID, AccountID: account.Account.ID, Provider: core.ChatYouTube, ChannelHint: "UCsynthetic", Enabled: true, Options: options})
		chatTestOK(t, err)
		destinations = append(destinations, destination)
		sources = append(sources, source)
	}
	if sources[1].ID < sources[0].ID {
		sources[0], sources[1] = sources[1], sources[0]
		destinations[0], destinations[1] = destinations[1], destinations[0]
	}

	// session содержит выданный RUNNING-запуск, результат работы сессии на границе этого теста.
	session := core.Session{Record: chatTestRecord("chat-session"), StreamID: stream.ID, Incarnation: store.Incarnation(), Lifecycle: 1, State: core.SessionRunning, Issued: true,
		Snapshot: core.SessionSnapshot{Stream: stream, Groups: []core.GroupSnapshot{{Group: group, Profile: profile}}}}
	// key и canonical дают согласованную идентичность готовой заглушки в обязательном снимке выхода.
	key, canonical, err := core.ProfileIdentity(profile.Profile)
	chatTestOK(t, err)
	// manifest описывает синтетический подготовленный файл, который тест не открывает.
	manifest := core.ArtifactManifest{GenerationID: "generation", VerifiedFormat: format, VariantID: "variant", AssetID: asset.ID, SourceHash: "synthetic-source-hash", Profile: profile.Profile, ProfileKey: key, CanonicalProfile: canonical, GeneratorVersion: core.CurrentFallbackGeneratorVersion, ManagedPath: "variants/generation.mp4", SizeBytes: 512, ContentHash: "synthetic-content-hash"}
	// parentFence и fences адресуют успешные команды Session и трёх выходов.
	var parentFence core.ExecutionFence
	var fences []core.ExecutionFence
	err = store.Write(ctx, func(r core.Repositories) error {
		// r добавляет выданный запуск и успешные квитанции в одну изолированную транзакцию.
		// err сообщает об отказе конкретной вставки.
		if err := r.Runtime.InsertSession(ctx, session); err != nil {
			return err
		}
		// parent и err задают точную команду запуска родителя и результат её сохранения.
		parent, err := insertChatTestReceipt(ctx, r, session, "")
		if err != nil {
			return err
		}
		parentFence = parent
		// index различает выходы, destination задаёт один из двух сохраняемых снимков назначения.
		for index, destination := range []core.Destination{destinations[0], destinations[0], destinations[1]} {
			// outputID адресует текущий выход независимо от его сохраняемого Destination.
			outputID := core.ID(fmt.Sprintf("chat-output-%d", index))
			// snapshot копирует конфигурацию выбранного назначения и общий готовый профиль.
			snapshot := core.OutputSnapshot{Destination: destination, Group: group, Profile: profile, Fallback: policy, Artifact: manifest}
			// output задаёт прикреплённый желаемый выход с выданной успешной попыткой.
			output := core.SessionOutput{Record: chatTestRecord(outputID), SessionID: session.ID, Snapshot: snapshot, Attached: true, DesiredRunning: true, Generation: 1, Attempt: 1, Issued: true}
			// err сообщает об отказе записи текущего выхода.
			if err := r.Runtime.InsertOutput(ctx, output); err != nil {
				return err
			}
			// fence связывает текущий выход с его точной успешной квитанцией.
			fence, err := insertChatTestReceipt(ctx, r, session, output.ID)
			if err != nil {
				return err
			}
			fences = append(fences, fence)
		}
		return nil
	})
	chatTestOK(t, err)
	// parent содержит точного оперативного родителя.
	parent, err := service.OpenSessionObserver(ctx, parentFence)
	chatTestOK(t, err)
	parent.NotifyChatRunning()
	// outputs накапливает живых наблюдателей из ранее сохранённых попыток.
	var outputs []*core.OutputObserver
	// fence адресует очередной актуальный выход.
	for _, fence := range fences {
		// observer представляет текущий выход до первой публикации.
		observer, err := service.OpenOutputObserver(ctx, fence)
		chatTestOK(t, err)
		outputs = append(outputs, observer)
	}
	return chatManagerTestFixture{store: store, service: service, stream: stream, session: session, parent: parent, outputs: outputs, sources: sources}
}

// insertChatTestReceipt записывает через репозитории r успешную квитанцию Media для session.
// ctx ограничивает запись. Пустой outputID обозначает запуск сессии, непустой — её выхода.
func insertChatTestReceipt(ctx context.Context, r core.Repositories, session core.Session, outputID core.ID) (core.ExecutionFence, error) {
	// id и kind задают идентичность, target — объект команды, generation — номер поколения выхода.
	id, kind, target, generation := core.ID("chat-session-start"), core.CommandStartSession, session.StreamID, uint64(0)
	if outputID != "" {
		id, kind, target, generation = outputID+"-start", core.CommandStartOutput, outputID, 1
	}
	// digest обеспечивает согласованный Fingerprint синтетического запроса.
	digest := sha256.Sum256([]byte(id))
	// receipt представляет подтверждённый запуск, а fence фиксирует его точное выполнение.
	receipt := core.CommandReceipt{Record: chatTestRecord(id), RequestID: string(id), Kind: kind, TargetID: target, Identity: []byte(id), Fingerprint: hex.EncodeToString(digest[:]), SessionID: session.ID, OutputID: outputID, Incarnation: session.Incarnation, Lifecycle: session.Lifecycle, OutputGeneration: generation, Attempt: 1, Status: core.CommandSucceeded}
	fence := core.ExecutionFence{CommandID: id, SessionID: session.ID, Incarnation: session.Incarnation, Lifecycle: session.Lifecycle, OutputID: outputID, OutputGeneration: generation, Attempt: 1}
	return fence, r.Runtime.InsertReceipt(ctx, receipt)
}

// chatTestReader сохраняет подтверждённую подписку и позволяет тесту вызвать позднюю доставку
// даже после отмены её платформенного контекста.
type chatTestReader struct {
	// request содержит токен и фактический ID общей беседы.
	request core.ChatReadRequest
	// sink принимает сообщения от проверяемого точного reader.
	sink core.ChatAdapterSink
	// stopped закрывается после выхода из Read.
	stopped <-chan struct{}
}

// chatTestAdapter моделирует платформу без любых сетевых вызовов.
type chatTestAdapter struct {
	// readers получает каждую физическую подписку после подтверждения.
	readers chan chatTestReader
	// broadcasts задаёт необязательный набор обнаруженных эфиров YouTube.
	broadcasts []core.ChatBroadcast
}

// Provider сообщает платформу тестового адаптера.
func (*chatTestAdapter) Provider() core.ChatProvider { return core.ChatYouTube }

// Discover возвращает одну фактическую беседу для обоих постоянных источников.
// Параметры конфигурации и полномочий предоставлены Manager, но фиктивная беседа от них не зависит.
func (a *chatTestAdapter) Discover(context.Context, core.ChatSource, core.ChatCredentials) (core.ChatDiscovery, error) {
	if len(a.broadcasts) != 0 {
		return core.ChatDiscovery{Broadcasts: a.broadcasts, State: core.ChatWaitingChat}, nil
	}
	return core.ChatDiscovery{ChannelID: "actual-chat", State: core.ChatConnected}, nil
}

// Read сообщает через sink об установлении подписки request, выдаёт тесту будущие callbacks,
// затем удерживает подписку до отмены ctx.
func (a *chatTestAdapter) Read(ctx context.Context, request core.ChatReadRequest, sink core.ChatAdapterSink) error {
	// stopped свидетельствует о физическом завершении чтения.
	stopped := make(chan struct{})
	defer close(stopped)
	// err сообщает об отказе принятия подтверждённой подписки.
	if err := sink.Status(ctx, request.Token, core.ChatConnected, core.ChatReasonNone); err != nil {
		return err
	}
	select {
	case a.readers <- chatTestReader{request: request, sink: sink, stopped: stopped}:
	case <-ctx.Done():
		return ctx.Err()
	}
	<-ctx.Done()
	return ctx.Err()
}

// publish передаёт синтетическое сообщение id в ту же беседу через токен подписки.
// ctx задаёт контекст вызова независимо от контекста первоначального Read.
func (r chatTestReader) publish(ctx context.Context, id string) error {
	// message использует фактический канал выданной reader подписки.
	message := chatTestIncoming(id)
	message.ChannelID = r.request.Key.ChannelID
	return r.sink.Message(ctx, r.request.Token, message)
}

// waitChatTestSnapshot читает у Manager снимки потока stream до выполнения предиката accept.
// ctx ограничивает ожидание изменений, t сообщает об ошибке чтения или невыполненном предикате.
func waitChatTestSnapshot(t *testing.T, ctx context.Context, manager *Manager, stream core.ID, accept func(core.ChatSnapshot) bool) core.ChatSnapshot {
	t.Helper()
	// timer задаёт период опроса между изменениями разных компонентов на реальной SQLite.
	timer := time.NewTicker(5 * time.Millisecond)
	defer timer.Stop()
	for {
		// image получает согласованный снимок одной попытки, err сообщает об отказе чтения.
		image, err := manager.Snapshot(ctx, stream)
		chatTestOK(t, err)
		if accept(image) {
			return image
		}
		select {
		case <-timer.C:
		case <-ctx.Done():
			t.Fatalf("Ожидаемое состояние не достигнуто: сессия %v, статусы %+v, сообщений %d: %v", image.Session, image.Statuses, len(image.Messages), ctx.Err())
		}
	}
}

// chatTestHasState проверяет публичное состояние state источника source в снимке image.
func chatTestHasState(image core.ChatSnapshot, source core.ID, state core.ChatSourceState) bool {
	// status содержит очередное публичное состояние источника.
	for _, status := range image.Statuses {
		if status.SourceID == source && status.State == state {
			return true
		}
	}
	return false
}

// TestChatManagerPublicationAndSharedReaderLifecycle проверяет таблицу медиасостояний,
// мультиплексирование чтения, независимый отзыв источников и завершение ленты.
// t сообщает о расхождении SQLite, медианаблюдений и публичного снимка.
func TestChatManagerPublicationAndSharedReaderLifecycle(t *testing.T) {
	// ctx и cancel ограничивают ожидания в сценарии и останавливают оставшиеся операции.
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	// fixture предоставляет два источника общей беседы и три независимо отзываемых выхода.
	fixture := newChatManagerTestFixture(t, ctx)
	chatTestOK(t, fixture.outputs[0].Observe(ctx, core.OutputObservation{State: core.OutputConnecting}))
	chatTestOK(t, fixture.outputs[0].Observe(ctx, core.OutputObservation{State: core.OutputRetrying}))
	// adapter предоставляет управляемые подтверждения подписок без сети.
	adapter := &chatTestAdapter{readers: make(chan chatTestReader, 8)}
	// limits выпускает каждый пакет немедленно по достижении одного сообщения.
	limits := core.DefaultChatLimits()
	limits.FlushMessages = 1
	// manager содержит проверяемый runtime. err сообщает об ошибке операции сценария.
	manager, err := New(ctx, fixture.service, fixture.store, limits, []core.ChatAdapterPort{adapter})
	chatTestOK(t, err)
	t.Cleanup(func() {
		// Закрывает Manager до закрытия SQLite даже при раннем отказе сценария.
		// cleanup и stop ограничивают отдельно физическое завершение всех его работ.
		cleanup, stop := context.WithTimeout(context.Background(), 3*time.Second)
		defer stop()
		chatTestOK(t, manager.Close(cleanup))
	})
	// first и neighbor задают меньший и больший ID источников общей беседы.
	first, neighbor := fixture.sources[0].ID, fixture.sources[1].ID
	// stream адресует общую ленту текущего запуска.
	stream := fixture.stream.ID
	// image проверяет в публичном снимке наличие ожидания фактической публикации.
	image := waitChatTestSnapshot(t, ctx, manager, stream, func(image core.ChatSnapshot) bool {
		// Предикат ждёт появления сессии и двух источников, ожидающих передачу медиа.
		return image.Session != nil && chatTestHasState(image, first, core.ChatWaitingOutput) && chatTestHasState(image, neighbor, core.ChatWaitingOutput)
	})
	select {
	case <-adapter.readers:
		t.Fatal("Подписка запустилась до первой публикации")
	default:
	}
	if len(image.Messages) != 0 {
		t.Fatal("Лента до публикации содержит сообщения")
	}

	// observer соответствует очередному публикующему выходу. Первые два имеют один Destination.
	for _, observer := range fixture.outputs {
		chatTestOK(t, observer.Observe(ctx, core.OutputObservation{State: core.OutputPublishing}))
	}
	// reader — физическая подписка, которую делят оба допущенных источника.
	reader := chatTestReceive(t, ctx, adapter.readers)
	waitChatTestSnapshot(t, ctx, manager, stream, func(image core.ChatSnapshot) bool {
		// Предикат ждёт подтверждения обоих членов общей подписки.
		return chatTestHasState(image, first, core.ChatConnected) && chatTestHasState(image, neighbor, core.ChatConnected)
	})
	select {
	case <-adapter.readers:
		t.Fatal("Общий канал получил лишнюю физическую подписку")
	default:
	}
	chatTestOK(t, reader.publish(ctx, "first"))
	chatTestOK(t, reader.publish(ctx, "first"))
	chatTestOK(t, reader.publish(ctx, "barrier"))
	image = waitChatTestSnapshot(t, ctx, manager, stream, func(image core.ChatSnapshot) bool {
		// Предикат ждёт сообщения после дубликата, чтобы проверить итоговый порядок обработки.
		return slices.ContainsFunc(image.Messages, func(m core.ChatMessage) bool { return m.ProviderMessageID == "barrier" })
	})
	if len(image.Messages) != 2 || image.Messages[0].ChatSourceID != first || image.Messages[1].ChatSourceID != first ||
		image.Messages[0].ProviderMessageID != "first" || image.Messages[0].Seq >= image.Messages[1].Seq {
		t.Fatalf("Неверная дедупликация, порядок или атрибуция общей подписки: %+v", image.Messages)
	}
	chatTestOK(t, fixture.outputs[0].ObserveTerminal(ctx, core.OutputObservation{State: core.OutputStopped}))
	// group представляет наблюдателя общего медиатракта, переводимого на заглушку.
	group, err := fixture.parent.OpenGroup(ctx, fixture.session.Snapshot.Groups[0].Group.ID, fixture.session.Snapshot.Groups[0].Profile.ID)
	chatTestOK(t, err)
	chatTestOK(t, group.Observe(ctx, core.GroupObservation{SourceRevision: 1, State: core.ObservationReady, Source: core.ObservationSourceFallback}))
	// pause фиксирует штатную команду ручной паузы в живой сессии.
	pause, err := fixture.service.PauseSession(ctx, core.SessionCommand{RequestID: "chat-pause", SessionID: fixture.session.ID})
	chatTestOK(t, err)
	if pause.Kind != core.CommandPauseSession {
		t.Fatalf("Ручная пауза не принята для текущей сессии: %+v", pause)
	}
	chatTestOK(t, fixture.outputs[1].Observe(ctx, core.OutputObservation{State: core.OutputConnecting}))
	chatTestOK(t, fixture.outputs[1].Observe(ctx, core.OutputObservation{State: core.OutputRetrying}))
	chatTestOK(t, reader.publish(ctx, "retrying"))
	image = waitChatTestSnapshot(t, ctx, manager, stream, func(image core.ChatSnapshot) bool {
		// Предикат ждёт сообщения второго выхода пока он повторяет соединение после публикации.
		return len(image.Messages) >= 3
	})
	if image.Messages[2].ChatSourceID != first || image.Messages[2].ProviderMessageID != "retrying" {
		t.Fatalf("Повторное соединение одного из двух выходов отозвало ещё действующий источник: %+v", image.Messages[2])
	}

	// detach фиксирует принятую команду отсоединения последнего выхода первого назначения.
	detach, err := fixture.service.DetachOutput(ctx, core.OutputCommand{
		RequestID: "chat-detach-first", OutputID: fixture.outputs[1].ExecutionFence().OutputID,
	})
	chatTestOK(t, err)
	if detach.Kind != core.CommandDetachOutput || detach.SessionID != fixture.session.ID {
		t.Fatalf("Команда detach не принята для текущего выхода: %+v", detach)
	}
	chatTestOK(t, reader.publish(ctx, "neighbor"))
	image = waitChatTestSnapshot(t, ctx, manager, stream, func(image core.ChatSnapshot) bool {
		// Предикат ждёт сообщение живого соседа после отзыва последнего подходящего выхода первого источника.
		return len(image.Messages) >= 4
	})
	if image.Messages[0].ChatSourceID != first || image.Messages[3].ChatSourceID != neighbor || image.Messages[3].ProviderMessageID != "neighbor" {
		t.Fatalf("Отзыв одного источника изменил атрибуцию истории либо соседа: %+v", image.Messages)
	}
	// stop фиксирует принятое намерение остановки последнего подходящего выхода.
	stop, err := fixture.service.StopOutput(ctx, core.OutputCommand{
		RequestID: "chat-stop-neighbor", OutputID: fixture.outputs[2].ExecutionFence().OutputID,
	})
	chatTestOK(t, err)
	if stop.Kind != core.CommandStopOutput || stop.SessionID != fixture.session.ID {
		t.Fatalf("Команда stop не принята для текущего выхода: %+v", stop)
	}
	chatTestReceive(t, ctx, reader.stopped)
	if err = reader.publish(ctx, "late"); !errors.Is(err, core.ErrStale) {
		t.Fatalf("Поздняя доставка от отозванного reader принята: %v", err)
	}
	image, err = manager.Snapshot(ctx, stream)
	chatTestOK(t, err)
	if image.Session == nil || len(image.Messages) != 4 {
		t.Fatalf("Остановка последнего выхода закрыла ленту родителя: %+v", image)
	}
	// cursor запоминает позицию живой ленты перед прекращением точного родителя.
	cursor := image.Cursor
	chatTestOK(t, fixture.parent.Close(ctx))
	waitChatTestSnapshot(t, ctx, manager, stream, func(image core.ChatSnapshot) bool {
		// Предикат ждёт пустой ленты без оперативного владельца при сохранённом RUNNING.
		return image.Session == nil && len(image.Messages) == 0
	})
	// stored и err проверяют отсутствие перехода сохраняемой сессии в терминальное состояние.
	stored, err := fixture.service.GetSession(ctx, fixture.session.ID)
	chatTestOK(t, err)
	if stored.State != core.SessionRunning {
		t.Fatalf("Граница сценария требует сохранённый RUNNING, получено %s", stored.State)
	}
	// page получает продолжение прежнего курсора после отзыва точной сессии.
	page, err := manager.Events(ctx, stream, cursor, 32)
	chatTestOK(t, err)
	if !page.ResyncRequired && !slices.ContainsFunc(page.Events, func(event core.ChatEvent) bool {
		// event должен завершить именно закрытую тестовую ленту.
		return event.Kind == core.ChatEventSessionClosed && event.SessionID == fixture.session.ID
	}) {
		t.Fatalf("Прежний курсор не сообщил о закрытии ленты или смене эпохи: %+v", page)
	}
	chatTestOK(t, manager.Close(ctx))
	chatTestReceive(t, ctx, manager.Done())
}

// TestChatManagerQueuedMessageCannotBorrowNewMembership проверяет, что сообщение,
// принятое от первого источника, нельзя приписать позже присоединившемуся соседу.
// t меняет конфигурацию под штатным барьером Manager до запуска обработки пакета.
func TestChatManagerQueuedMessageCannotBorrowNewMembership(t *testing.T) {
	// ctx и cancel ограничивают ожидания переходов и освобождают ресурсы адаптера.
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	// fixture предоставляет два источника, которые лишь позднее разделят одну подписку.
	fixture := newChatManagerTestFixture(t, ctx)
	// adapter сообщает о физическом чтении единственной беседы.
	adapter := &chatTestAdapter{readers: make(chan chatTestReader, 8)}
	// limits не выпускает одиночную запись за время теста. Второе сообщение запускает flush.
	limits := core.DefaultChatLimits()
	limits.FlushInterval, limits.FlushMessages = time.Minute, 2
	// manager наблюдает за настройками и точными медиавыходами. err сообщает об отказе операции.
	manager, err := New(ctx, fixture.service, fixture.store, limits, []core.ChatAdapterPort{adapter})
	chatTestOK(t, err)
	t.Cleanup(func() {
		// Закрывает Manager и фоновые работы до освобождения SQLite.
		chatTestOK(t, manager.Close(context.Background()))
	})
	// first и neighbor адресуют начального и позднего членов подписки. stream адресует их ленту.
	first, neighbor := fixture.sources[0], fixture.sources[1]
	stream := fixture.stream.ID
	chatTestOK(t, fixture.outputs[0].Observe(ctx, core.OutputObservation{State: core.OutputPublishing}))
	// reader принимает первое сообщение при единственном действующем источнике.
	reader := chatTestReceive(t, ctx, adapter.readers)
	chatTestOK(t, reader.publish(ctx, "queued-before-new-member"))

	chatTestOK(t, fixture.outputs[2].Observe(ctx, core.OutputObservation{State: core.OutputPublishing}))
	waitChatTestSnapshot(t, ctx, manager, stream, func(image core.ChatSnapshot) bool {
		// Предикат ждёт допуска нового участника общей беседы.
		return chatTestHasState(image, neighbor.ID, core.ChatConnected)
	})
	first.Enabled = false
	// applied подтверждает сохранение и применение новой ревизии под единым барьером.
	applied, err := manager.MutateSource(ctx, first.ID, func(ctx context.Context) (core.ChatSource, error) {
		// Callback выполняет CAS изменения первого источника с сохранением соседней настройки.
		return fixture.service.UpdateChatSource(ctx, first, first.Revision)
	})
	if err != nil || applied.Revision != first.Revision+1 {
		t.Fatalf("Отключение первого источника не применилось: %+v, %v", applied, err)
	}
	chatTestOK(t, reader.publish(ctx, "accepted-after-new-member"))
	// image появляется после flush обеих записей. Старая запись не должна взять новый допуск.
	image := waitChatTestSnapshot(t, ctx, manager, stream, func(image core.ChatSnapshot) bool {
		// Предикат ждёт сообщение, принятое уже после отзыва первого источника.
		return slices.ContainsFunc(image.Messages, func(message core.ChatMessage) bool { return message.ProviderMessageID == "accepted-after-new-member" })
	})
	if len(image.Messages) != 1 || image.Messages[0].Seq != 1 || image.Messages[0].ChatSourceID != neighbor.ID {
		t.Fatalf("Очередь заимствовала новый допуск либо потеряла соседа: %+v", image.Messages)
	}

	first.Enabled = true
	// applied после CAS повторно включает источник при уже публикующем медиавыходе.
	applied, err = manager.MutateSource(ctx, first.ID, func(ctx context.Context) (core.ChatSource, error) {
		// Callback применяет включение с ревизией, полученной при отключении.
		return fixture.service.UpdateChatSource(ctx, first, first.Revision+1)
	})
	if err != nil || applied.Revision != first.Revision+2 {
		t.Fatalf("Включение источника при готовом выходе не применилось: %+v, %v", applied, err)
	}
	waitChatTestSnapshot(t, ctx, manager, stream, func(image core.ChatSnapshot) bool {
		// Предикат ждёт возврата источника к действующей физической подписке.
		return chatTestHasState(image, first.ID, core.ChatConnected)
	})
	select {
	case <-adapter.readers:
		t.Fatal("Повторное включение создало дублирующее физическое чтение")
	default:
	}
	chatTestOK(t, reader.publish(ctx, "queued-with-both-members"))
	first.Enabled = false
	// applied теперь отзывает первого участника, когда сосед уже входил в исходный допуск.
	applied, err = manager.MutateSource(ctx, first.ID, func(ctx context.Context) (core.ChatSource, error) {
		// Callback применяет повторное отключение к последней подтверждённой ревизии.
		return fixture.service.UpdateChatSource(ctx, first, first.Revision+2)
	})
	if err != nil || applied.Revision != first.Revision+3 {
		t.Fatalf("Повторное отключение источника не применилось: %+v, %v", applied, err)
	}
	chatTestOK(t, reader.publish(ctx, "accepted-with-one-original-member"))
	image = waitChatTestSnapshot(t, ctx, manager, stream, func(image core.ChatSnapshot) bool {
		// Предикат ждёт конца следующего пакета после удаления одного исходного участника.
		return slices.ContainsFunc(image.Messages, func(message core.ChatMessage) bool {
			// message относится к последней принятой в сценарии записи.
			return message.ProviderMessageID == "accepted-with-one-original-member"
		})
	})
	if len(image.Messages) != 3 || image.Messages[1].ProviderMessageID != "queued-with-both-members" || image.Messages[1].ChatSourceID != neighbor.ID {
		t.Fatalf("Отзыв одного участника потерял ранее допущенного соседа: %+v", image.Messages)
	}
}

// TestChatManagerBroadcastSelectionRequiresCurrentDiscovery проверяет выбор из текущих данных
// YouTube. t подставляет несколько эфиров и просроченные удостоверения выбора.
func TestChatManagerBroadcastSelectionRequiresCurrentDiscovery(t *testing.T) {
	// ctx и cancel ограничивают ожидания и отменяют работу изолированного адаптера.
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	// fixture предоставляет одного допускаемого источника и независимого невещающего соседа.
	fixture := newChatManagerTestFixture(t, ctx)
	// adapter обнаруживает два действительных видео с разными фактическими каналами.
	adapter := &chatTestAdapter{readers: make(chan chatTestReader, 8), broadcasts: []core.ChatBroadcast{
		{VideoID: "example-video-a", ChannelID: "example-chat-a", Title: "Первый эфир"},
		{VideoID: "example-video-b", ChannelID: "example-chat-b", Title: "Второй эфир"},
	}}
	// manager предоставляет оперативный выбор эфира, err сообщает ошибки сценария.
	manager, err := New(ctx, fixture.service, fixture.store, core.DefaultChatLimits(), []core.ChatAdapterPort{adapter})
	chatTestOK(t, err)
	t.Cleanup(func() {
		// Останавливает тестовые работы прежде, чем закроется их SQLite.
		chatTestOK(t, manager.Close(context.Background()))
	})
	// source и stream задают границы допустимого ручного выбора.
	source := fixture.sources[0]
	stream := fixture.stream.ID
	chatTestOK(t, fixture.outputs[0].Observe(ctx, core.OutputObservation{State: core.OutputPublishing}))
	// waiting фиксирует конкретную Session, в которой адаптер уже опубликовал неоднозначный набор.
	waiting := waitChatTestSnapshot(t, ctx, manager, stream, func(image core.ChatSnapshot) bool {
		// Предикат ждёт публичного статуса отсутствия автоматического выбора.
		return image.Session != nil && chatTestHasState(image, source.ID, core.ChatWaitingChat)
	})
	// choices содержит целый согласованный набор для выбора.
	choices, err := manager.BroadcastChoices(ctx, *waiting.Session, source.ID)
	chatTestOK(t, err)
	if len(choices.Broadcasts) != 2 || choices.SourceRevision != source.Revision || choices.DiscoveryGeneration == 0 {
		t.Fatalf("Не возвращён текущий набор из двух результатов: %+v", choices)
	}
	// selection выбирает второе видео из текущего набора.
	selection := core.ChatBroadcastSelection{Session: *waiting.Session, SourceID: source.ID, SourceRevision: source.Revision,
		DiscoveryGeneration: choices.DiscoveryGeneration, VideoID: "example-video-b"}
	// wrongSession, wrongRevision, wrongGeneration и wrongVideo различают независимые нарушения допуска.
	wrongSession, wrongRevision, wrongGeneration, wrongVideo := selection, selection, selection, selection
	wrongSession.Session.Lifecycle++
	wrongRevision.SourceRevision++
	wrongGeneration.DiscoveryGeneration++
	wrongVideo.VideoID = "video-outside-current-list"
	// stale содержит очередной неподтверждённый выбор.
	for _, stale := range []core.ChatBroadcastSelection{wrongSession, wrongRevision, wrongGeneration, wrongVideo} {
		if err = manager.SelectBroadcast(ctx, stale); !errors.Is(err, core.ErrStale) {
			t.Fatalf("Выбор с неподтверждённым допуском принят: %+v, %v", stale, err)
		}
	}
	select {
	case <-adapter.readers:
		t.Fatal("Чтение началось до выбора из текущего набора")
	default:
	}
	chatTestOK(t, manager.SelectBroadcast(ctx, selection))
	// reader подтверждает выбор именно фактического чата указанного видео.
	reader := chatTestReceive(t, ctx, adapter.readers)
	if reader.request.Key.ChannelID != "example-chat-b" || reader.request.Key.Session != *waiting.Session {
		t.Fatalf("Чтение направлено в другой чат или Session: %+v", reader.request.Key)
	}
	if err = manager.SelectBroadcast(ctx, selection); !errors.Is(err, core.ErrStale) {
		t.Fatalf("Израсходованный выбор вновь принят: %v", err)
	}
}

// chatTopologyTestStore позволяет провести медиасобытия между чтением RAM-снимка Manager
// и его согласованным чтением SQLite без вмешательства в семантику транзакции.
type chatTopologyTestStore struct {
	// UnitOfWork обслуживает настоящую файловую SQLite.
	core.UnitOfWork
	// blocked отмечает единственное ожидание после первого RAM-снимка.
	blocked atomic.Bool
	// entered отмечает начало чтения конфигурации, proceed снимает тестовый барьер.
	entered chan struct{}
	proceed chan struct{}
}

// View при первом вызове в контексте ctx ждёт тестового сигнала и делегирует read рабочей SQLite.
func (s *chatTopologyTestStore) View(ctx context.Context, read func(core.Repositories) error) error {
	if s.blocked.CompareAndSwap(false, true) {
		close(s.entered)
		select {
		case <-s.proceed:
		case <-ctx.Done():
			return ctx.Err()
		}
	}
	return s.UnitOfWork.View(ctx, read)
}

// TestChatManagerReconcilesRuntimeGapAfterFirstPublication проверяет повторную согласованную
// сверку, когда первое PUBLISHING выпало из журнала. t ограничивает ожидание живого reader
// и проверяет его привязку при текущем RETRYING без новой публикации.
func TestChatManagerReconcilesRuntimeGapAfterFirstPublication(t *testing.T) {
	// ctx и cancel ограничивают тест и освобождают барьер при ошибке.
	ctx, cancel := context.WithTimeout(t.Context(), 8*time.Second)
	defer cancel()
	// fixture предоставляет действующий родитель и выходы, ещё не публиковавшие медиа.
	fixture := newChatManagerTestFixture(t, ctx)
	// before содержит курсор до событий, которые вытеснят успешную публикацию.
	before, err := fixture.service.RuntimeAnchor(ctx)
	chatTestOK(t, err)
	// gated задерживает единственное первоначальное чтение SQLite.
	gated := &chatTopologyTestStore{UnitOfWork: fixture.store, entered: make(chan struct{}), proceed: make(chan struct{})}
	// adapter подтверждает, что допуск чата был восстановлен из актуального RAM.
	adapter := &chatTestAdapter{readers: make(chan chatTestReader, 8)}
	// manager начинает сверку с наблюдениями до первой публикации.
	manager, err := New(ctx, fixture.service, gated, core.DefaultChatLimits(), []core.ChatAdapterPort{adapter})
	chatTestOK(t, err)
	t.Cleanup(func() {
		// Закрывает Manager после отмены барьера и до закрытия временной SQLite.
		chatTestOK(t, manager.Close(context.Background()))
	})
	chatTestReceive(t, ctx, gated.entered)
	chatTestOK(t, fixture.outputs[0].Observe(ctx, core.OutputObservation{State: core.OutputPublishing}))
	for range 64 {
		chatTestOK(t, fixture.outputs[0].Observe(ctx, core.OutputObservation{State: core.OutputConnecting}))
		chatTestOK(t, fixture.outputs[0].Observe(ctx, core.OutputObservation{State: core.OutputRetrying}))
	}
	// page подтверждает разрыв журнала и позволяет проверить отсутствие первого PUBLISHING.
	page, err := fixture.service.RuntimeEvents(ctx, before.Cursor, 64)
	if err != nil || !page.Gap {
		t.Fatalf("Фикстура не вытеснила первое PUBLISHING из медиажурнала: %+v, %v", page, err)
	}
	// event содержит очередное удерживаемое медианаблюдение.
	for _, event := range page.Events {
		if event.OutputState == core.OutputPublishing {
			t.Fatal("В журнале остался первый PUBLISHING")
		}
	}
	close(gated.proceed)
	// reader появился без новой публикации после переподключения.
	reader := chatTestReceive(t, ctx, adapter.readers)
	chatTestOK(t, reader.publish(ctx, "after-runtime-gap"))
	waitChatTestSnapshot(t, ctx, manager, fixture.stream.ID, func(image core.ChatSnapshot) bool {
		// Предикат проверяет принятие сообщения живого источника при продолжающемся RETRYING.
		return len(image.Messages) == 1 && image.Messages[0].ChatSourceID == fixture.sources[0].ID
	})
}
