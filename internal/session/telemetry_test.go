package session

import (
	"context"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
)

func TestManagerMonitoredSourcesPublishAndSamplerJoins(t *testing.T) {
	f := newManagerFixture(t, true, nil)
	ctx, cancel := context.WithTimeout(context.Background(), 8*time.Second)
	defer cancel()
	receipt, err := f.manager.StartSession(ctx, core.StartSessionRequest{RequestID: "monitored-session", StreamID: f.stream.ID})
	if err != nil {
		t.Fatal(err)
	}
	if result := waitReceipt(t, f, receipt); result.Status != core.CommandSucceeded {
		t.Fatal(result)
	}
	session, err := f.service.GetSession(ctx, receipt.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	router, err := f.manager.Feed(session)
	if err != nil {
		t.Fatal(err)
	}
	lease, err := f.service.ActivateOBS(ctx, f.stream.ID, f.stream.PublicationKey, func(l *core.RuntimeLease) (func(), error) {
		if _, err := router.BeginSource(); err != nil {
			return nil, err
		}
		router.BeginPublication(l.ID())
		return func() { router.EndPublication(l.ID()) }, nil
	})
	if err != nil {
		t.Fatal(err)
	}
	profile, _, _ := fixtureProfile(t)
	if err = lease.Do(ctx, func() error {
		for range 10 {
			router.ObserveIngress(lease.ID(), media.TrackVideo, 100, profile.Format.Video, core.AudioFormat{})
		}
		router.ObserveIngress(lease.ID(), media.TrackAudio, 20, core.VideoFormat{}, profile.Format.Audio)
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	f.manager.mu.Lock()
	node := f.manager.sessions[session.ID]
	f.manager.mu.Unlock()
	node.mu.Lock()
	sampler := node.sampler
	node.mu.Unlock()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		snapshot, err := f.service.RuntimeSnapshot(ctx, lease.SessionFence())
		if err != nil {
			t.Fatal(err)
		}
		if snapshot.Input != nil && snapshot.Input.Metrics != nil && snapshot.Input.Metrics.Rates.PayloadBitrate.Status == core.MetricAvailable && len(snapshot.Groups) > 0 && snapshot.Groups[0].Metrics != nil && len(snapshot.Outputs) > 0 && snapshot.Outputs[0].Metrics != nil {
			input := snapshot.Input.Metrics
			if input.VideoFrames != 10 || input.PayloadBytes != 1020 || input.Rates.PayloadBitrate.Value != 0 || input.Rates.FPS.Value != 0 {
				t.Fatalf("накопительные значения или пустое окно изменены: %+v", input)
			}
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
