package session

import (
	"context"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
)

// TestManagerMonitoredSourcesPublishAndSamplerJoins проверяет публикацию счётчиков входа, группы и
// выхода через реальные сборщики и их полное завершение с освобождением источников при Close.
func TestManagerMonitoredSourcesPublishAndSamplerJoins(t *testing.T) {
	// f предоставляет включённое назначение с настоящими сборщиками метрик сессии.
	f := newManagerFixture(t, true, nil)
	// ctx ограничивает ожидание публикации метрик восемью секундами; cancel освобождает этот срок.
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	// receipt содержит принятую команду наблюдаемой сессии; err сообщает отказ приёма запуска.
	receipt, err := f.manager.StartSession(ctx, core.StartSessionRequest{RequestID: "monitored-session", StreamID: f.stream.ID})
	if err != nil {
		t.Fatal(err)
	}
	// result содержит успешный результат запуска перед проверкой источников метрик.
	if result := waitReceipt(t, f, receipt); result.Status != core.CommandSucceeded {
		t.Fatal(result)
	}
	// session содержит сохранённое исполнение для получения его медиавхода.
	session, err := f.service.GetSession(ctx, receipt.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	// router содержит медиамаршрутизатор сессии, в который добавляются известные входные объёмы.
	router, err := f.manager.Feed(session)
	if err != nil {
		t.Fatal(err)
	}
	// lease содержит точный допуск OBS, ограничивающий тестовую публикацию и изменение входных
	// счётчиков.
	lease, err := f.service.ActivateOBS(ctx, f.stream.ID, f.stream.PublicationKey, func(l *core.RuntimeLease) (func(), error) {
		// Начинает источник и публикацию Router под точным допуском OBS, возвращая действие завершения этой
		// публикации.

		// err сообщает отказ начала новой эпохи источника медиамаршрутизатора.
		if _, err := router.BeginSource(); err != nil {
			return nil, err
		}
		router.BeginPublication(l.ID())
		return func() {
			// Завершает публикацию Router только с идентификатором выданного допуска OBS.
			router.EndPublication(l.ID())
		}, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	// profile задаёт проверенные форматы видео и аудио для синтетических наблюдений входа.
	profile, _, _ := fixtureProfile(t)
	if err = lease.Do(ctx, func() error {
		// Добавляет известные объёмы видео и аудио к счётчикам входа, пока точный допуск OBS действителен.

		for range 10 {
			router.ObserveIngress(lease.ID(), media.TrackVideo, 100, profile.Format.Video, core.AudioFormat{})
		}
		router.ObserveIngress(lease.ID(), media.TrackAudio, 20, core.VideoFormat{}, profile.Format.Audio)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.manager.mu.Lock()
	// node указывает локального владельца наблюдаемой сессии.
	node := f.manager.sessions[session.ID]
	f.manager.mu.Unlock()
	node.mu.Lock()
	// sampler содержит сборщик сессии для проверки завершения и освобождения источников при Close.
	sampler := node.sampler
	node.mu.Unlock()
	// ticker задаёт период ожидания опубликованных входных, групповых и выходных метрик.
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		// snapshot содержит текущие наблюдения точного исполнения OBS; err сообщает отказ чтения
		// оперативного снимка.
		snapshot, err := f.service.RuntimeSnapshot(ctx, lease.SessionFence())
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.Input != nil && snapshot.Input.Metrics != nil && snapshot.Input.Metrics.Rates.PayloadBitrate.Status == core.MetricAvailable && len(snapshot.Groups) > 0 && snapshot.Groups[0].Metrics != nil && len(snapshot.Outputs) > 0 && snapshot.Outputs[0].Metrics != nil {
			// input содержит опубликованные метрики входа для проверки известных
			// накопительных объёмов и пустого окна скорости.
			input := snapshot.Input.Metrics
			if input.VideoFrames != 10 || input.PayloadBytes != 1020 || input.Rates.PayloadBitrate.Value != 0 || input.Rates.FPS.Value != 0 {
				t.Fatalf("накопительные значения или пустое окно изменены: %+v", input)
			}
			// group содержит опубликованные метрики первой группы для проверки заглушки,
			// выхода и отсутствующего LIVE.
			group := snapshot.Groups[0].Metrics
			if group.FallbackInput.VideoFrames == 0 || group.Output.VideoFrames == 0 || group.LiveInput.Rates.FPS.Status != core.MetricUnavailable {
				t.Fatalf("не подключены уровни группы: %+v", group)
			}
			break
		}
		select {
		case <-ctx.Done():
			t.Fatal("метрики не опубликованы", ctx.Err())
		case <-ticker.C:
		}
	}
	if err = f.manager.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case <-sampler.Done():
	default:
		t.Fatal("Manager завершился до sampler")
	}
	if sampler.Stats().Sources != 0 {
		t.Fatal("сборщик сохранил функции сбора после завершения")
	}
}
