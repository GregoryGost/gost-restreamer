package app

import (
	"context"
	"errors"
	"math"
	"net"
	"net/url"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/config"
	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/monitoring/logging"
	"github.com/bluenviron/gortmplib"
)

// obsApplicationStream создаёт подготовленные доменные метаданные потока без назначений.
// Нулевой комплект не требует FFmpeg и позволяет проверить настоящий app→RTMP→Manager→SQLite.
func obsApplicationStream(t *testing.T, ctx context.Context, a *Application) core.Stream {
	t.Helper()
	// asset содержит заглушку без требуемых медиавариантов; err сообщает отказ её создания.
	asset, err := a.core.CreateAsset(ctx, "Минимальная заглушка")
	if err != nil {
		t.Fatal(err)
	}
	// stream содержит поток с точным постоянным ключом и политикой fallback.
	stream, err := a.core.CreateStream(ctx, core.Stream{Name: "Автозапуск OBS", PublicationKey: "application-obs-key", Fallback: core.FallbackPolicy{AssetID: asset.ID, Watchdog: core.WatchdogPolicy{VideoTimeout: time.Second, AudioTimeout: time.Second, RecoveryWindow: time.Second, KeyframeTimeout: time.Second}}})
	if err != nil {
		t.Fatal(err)
	}
	// generation содержит требуемое опубликованное поколение с пустым набором профилей.
	generation, err := a.core.ReservePreparation(ctx, core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage, GeneratorVersion: config.GeneratorVersion})
	if err != nil {
		t.Fatal(err)
	}
	// fence фиксирует владельца попытки подготовки для настоящего протокола публикации.
	fence, err := a.core.ClaimPreparation(ctx, generation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = a.core.MarkPreparationIssued(ctx, fence); err != nil {
		t.Fatal(err)
	}
	if _, err = a.core.PublishPreparation(ctx, fence, core.GenerationPublication{SourceHash: "synthetic-empty-profile-source", SizeBytes: 1, SourceFormat: core.SourceFormat{ImageFormat: "png"}}); err != nil {
		t.Fatal(err)
	}
	if err = a.core.AcknowledgePreparationCleanup(ctx, fence); err != nil {
		t.Fatal(err)
	}
	return stream
}

// obsApplicationConnect выполняет настоящую публикацию RTMP с указанным ключом и закрывает клиент
// при отказе. Успешным клиентом до Close владеет вызывающий тест.
func obsApplicationConnect(ctx context.Context, address string, stream core.Stream, key string) (*gortmplib.Client, error) {
	// client публикует в реальный app ingest через локальное TCP-соединение.
	client := &gortmplib.Client{URL: &url.URL{Scheme: "rtmp", Host: address, Path: "/" + string(stream.ID), Fragment: key}, Publish: true}
	// err содержит исход сетевого согласования и Publish.Start.
	err := client.Initialize(ctx)
	if err != nil {
		client.Close()
		return nil, err
	}
	return client, nil
}

// TestOBSApplicationColdPublishDisconnectAndReconnect проверяет рабочую сборку приложения:
// неверный ключ не создаёт эфир, правильный publish запускает его без ручной команды,
// а отключение и повторное подключение сохраняют тот же RUNNING и ручную паузу.
func TestOBSApplicationColdPublishDisconnectAndReconnect(t *testing.T) {
	// c предоставляет изолированные каталоги, настоящий helper и отсутствующий FFmpeg.
	c := acceptanceConfig(t, acceptanceHelper(t))
	// a содержит рабочее приложение; err сообщает отказ сборки.
	a, err := start(context.Background(), c, acceptanceDependencies(&acceptanceLog{}))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Присоединяет настоящее приложение и освобождает его SQLite и слушатели.
		acceptanceStop(t, a)
	})
	// ctx ограничивает полный сетевой сценарий; cancel освобождает срок.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	// stream содержит готовый поток без назначений и медиапроцессов.
	stream := obsApplicationStream(t, ctx, a)
	if _, err = obsApplicationConnect(ctx, a.IngestAddress(), stream, "wrong-key"); err == nil {
		t.Fatal("неверный ключ запустил публикацию")
	}
	select {
	// rejected подтверждает завершение именно первой публикации с ошибочным ключом.
	case rejected := <-a.ingest.Results():
		if !errors.Is(rejected.Err, core.ErrNotReady) {
			t.Fatal("неверный отказ ключа", rejected.Err)
		}
	case <-ctx.Done():
		t.Fatal("нет исхода ошибочного ключа", ctx.Err())
	}
	// before подтверждает пустую историю после ошибочного ключа.
	before, err := a.core.ListSessions(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8, StreamID: stream.ID}})
	if err != nil || len(before.Items) != 0 {
		t.Fatal("неверный ключ создал Session", before, err)
	}
	// client содержит первую настоящую холодную публикацию.
	client, err := obsApplicationConnect(ctx, a.IngestAddress(), stream, stream.PublicationKey)
	if err != nil {
		t.Fatal("холодная публикация не связана с Manager.AdmitOBS", err)
	}
	t.Cleanup(client.Close)
	// selected содержит запущенную публикацией Session; router подтверждает локальную готовность.
	selected, router, err := a.manager.AdmitOBS(ctx, stream.ID, stream.PublicationKey)
	if err != nil || selected.State != core.SessionRunning || router == nil {
		t.Fatal("publish принят без готового Manager", selected, err)
	}
	client.Close()
	select {
	case <-a.ingest.Results():
	case <-ctx.Done():
		t.Fatal("обрыв публикации не присоединён", ctx.Err())
	}
	// current читает долговечный эфир после фактического завершения сетевого обработчика.
	current, err := a.core.GetSession(ctx, selected.ID)
	if err != nil || current.State != core.SessionRunning {
		t.Fatal("отключение OBS завершило эфир", current, err)
	}
	// pause фиксирует ручное намерение до переподключения через тот же реальный RTMP.
	pause, err := a.manager.PauseSession(ctx, core.SessionCommand{RequestID: "app-obs-pause", SessionID: selected.ID})
	if err != nil {
		t.Fatal(err)
	}
	// reconnected содержит новый сетевой publish, которому нельзя сбрасывать паузу или создавать Session.
	reconnected, err := obsApplicationConnect(ctx, a.IngestAddress(), stream, stream.PublicationKey)
	if err != nil {
		t.Fatal("переподключение не использует текущий эфир", err)
	}
	t.Cleanup(reconnected.Close)
	// sessions подтверждает единственную Session и сохранённое намерение ручной паузы.
	sessions, err := a.core.ListSessions(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8, StreamID: stream.ID}})
	if err != nil || len(sessions.Items) != 1 || sessions.Items[0].ID != selected.ID || !sessions.Items[0].DesiredPaused {
		t.Fatal("переподключение изменило Session или паузу", sessions, err)
	}
	// receipts содержит только исходный автозапуск и явно отправленную паузу.
	receipts, err := a.core.ListReceipts(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8, SessionID: selected.ID}})
	if err != nil || len(receipts.Items) != 2 {
		t.Fatal("переподключение создало дополнительные команды", receipts, err)
	}
	if pause.Kind != core.CommandPauseSession {
		t.Fatal("неверный вид явной команды", pause)
	}
}

// TestOBSOverflowPrecedesApplicationResources проверяет, что переполнение общего срока
// отклоняется до поиска helper, журнала, открытия слушателя TCP и создания SQLite.
func TestOBSOverflowPrecedesApplicationResources(t *testing.T) {
	// c содержит корректные настройки с переполненной суммой существующих сроков допуска.
	c := config.Default()
	c.Session.OperationTimeout = time.Duration(math.MaxInt64)
	// acquired отмечает запрещённое приобретение первой зависимости после ранней валидации.
	acquired := false
	// d останавливает приобретение на первой зависимости, если проверка ошибочно пропустит переполнение.
	d := dependencies{logger: func(logging.Config) (*logging.Sink, error) {
		// Фиксирует попытку создать журнал, не создавая сокетов, файлов или БД.
		acquired = true
		return nil, core.ErrClosed
	}, listen: func(context.Context, string, string) (net.Listener, error) {
		// Запрещает открытие слушателя даже при ошибочной перестановке этапов сборки.
		acquired = true
		return nil, core.ErrClosed
	}}
	// a должен отсутствовать; err должен сохранить конкретную ошибку недопустимого срока.
	a, err := start(context.Background(), c, d)
	if a != nil || !errors.Is(err, core.ErrInvalid) || acquired {
		t.Fatal("переполнение пропущено до ресурсов приложения", a, err, acquired)
	}
}
