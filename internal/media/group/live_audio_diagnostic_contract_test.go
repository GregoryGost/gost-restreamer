package group

import (
	"bytes"
	"context"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
)

type liveAudioDiagnosticLog struct {
	mu     sync.Mutex
	buffer bytes.Buffer
}

func (l *liveAudioDiagnosticLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.buffer.Write(p)
}

func (l *liveAudioDiagnosticLog) snapshot() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return bytes.Clone(l.buffer.Bytes())
}

func TestLiveAudioDirectCopyRejectsMismatchWithDiagnosticAndHealthyFallback(t *testing.T) {
	for _, scenario := range []string{"initial", "configuration_change", "override"} {
		t.Run(scenario, func(t *testing.T) {
			f := newRuntimeAcceptanceFixture(t)
			var snapshot core.OutputSnapshot
			for _, output := range f.outputs {
				if output.Snapshot.Profile.Profile.Mode == core.ModeCopy {
					snapshot = output.Snapshot
					break
				}
			}
			if snapshot.Profile.Profile.Format.Video.Codec != core.CodecAV1 || snapshot.Profile.Profile.Format.Audio.SampleRateHz != 48000 {
				t.Fatal("нет исходного профиля AV1 copy с AAC 48 kHz")
			}
			ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
			defer cancel()
			live, err := media.NewRouter(f.config.Switcher.Router)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = live.Close() })
			var logs liveAudioDiagnosticLog
			var transcodes atomic.Int32
			config := f.config
			config.Logger = slog.New(slog.NewJSONHandler(&logs, nil))
			directory := t.TempDir()
			config.OpenFile = func(context.Context, core.ArtifactManifest) (*os.File, error) {
				return os.CreateTemp(directory, "fallback-")
			}
			config.Player = func(_ context.Context, _ string, _ *os.File, attempt ffmpeg.Attempt) (ffmpeg.Command, error) {
				return acceptanceHelperCommand(snapshot.Profile.Profile, attempt)
			}
			config.Transcode = func(context.Context, ffmpeg.NVIDIAConfig, ffmpeg.Attempt) (ffmpeg.Command, error) {
				transcodes.Add(1)
				return ffmpeg.Command{}, errors.New("прямое копирование не должно запускать перекодирование")
			}
			registry, err := NewRegistry(ctx, RegistryConfig{Core: f.service, Parent: f.parent, Observer: f.observer, Runtime: config, MaxGroups: 1, MaxReferences: 1})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := registry.Close(context.Background()); err != nil {
					t.Error("остановка группы:", err)
				}
			})
			ref, err := registry.Acquire(ctx, snapshot, live)
			if err != nil {
				t.Fatal(err)
			}
			group := ref.Group()
			if err = group.WaitReady(ctx); err != nil {
				t.Fatal("заглушка не готова:", err)
			}
			if status := group.Status(); !status.Available || status.Source != Fallback {
				t.Fatal("до LIVE не выбрана исправная заглушка:", status)
			}
			output, err := ref.Router().Subscribe(config.Switcher.Queue)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = output.Close() })
			source, err := live.BeginSource()
			if err != nil {
				t.Fatal(err)
			}
			video, audio := acceptanceRuntimeConfigs(t, snapshot.Profile.Profile, source.Epoch())
			// Полный AAC-LC AU с FIL отличается от AU заглушки; его байты должны сохраниться.
			payload, err := hex.DecodeString("de02004c61766336312e31392e31303100422008c11838")
			if err != nil {
				t.Fatal(err)
			}
			if scenario != "initial" {
				if err = source.UpdateConfigs(video, audio); err != nil {
					t.Fatal(err)
				}
				stop := publishLiveAudioDiagnostic(t, ctx, source, video, audio, payload, 0)
				waitRuntimeAcceptance(t, func() bool {
					status := group.Status()
					return status.Available && status.Source == Live
				})
				liveAudioDiagnosticPayload(t, ctx, output, payload)
				stop()
			}
			start := int64(0)
			switch scenario {
			case "override":
				source, err = live.ReplaceSource()
				if err != nil {
					t.Fatal(err)
				}
				video, audio = acceptanceRuntimeConfigs(t, snapshot.Profile.Profile, source.Epoch())
			case "configuration_change":
				start = 60
			}
			audioInfo := audio.Info()
			audioInfo.Audio.SampleRateHz = 44100
			audioInfo.TimeBase = media.TimeBase{Numerator: 1, Denominator: 44100}
			if scenario == "configuration_change" {
				audioInfo.Revision++
			}
			// ASC 0x1210 задаёт AAC-LC, 44,1 kHz и два канала: метаданные соответствуют заголовку.
			audio, err = media.NewCodecConfig(audioInfo, []byte{0x12, 0x10})
			if err != nil {
				t.Fatal(err)
			}
			if err = source.UpdateConfigs(video, audio); err != nil {
				t.Fatal(err)
			}
			publishLiveAudioDiagnostic(t, ctx, source, video, audio, payload, start)
			waitRuntimeAcceptance(t, func() bool {
				status := group.Status()
				return status.Available && status.Source == Fallback
			})
			seenAudio, seenVideo := false, false
			for range 24 {
				delivery, err := output.Next(ctx)
				if err != nil {
					t.Fatal("выдача заглушки прервалась:", err)
				}
				if !delivery.IsCurrent() {
					continue
				}
				if delivery.Packet.Config().Info().Track == media.TrackAudio {
					seenAudio = true
					if delivery.AudioConfig.Info().Audio != snapshot.Profile.Profile.Format.Audio || !bytes.Equal(delivery.Packet.Payload(), switchAcceptanceAACPayload()) {
						t.Fatal("несовместимый LIVE заменил AAC исправной заглушки")
					}
				} else {
					seenVideo = true
				}
			}
			if !seenAudio || !seenVideo || group.Ready() != nil {
				t.Fatal("после отказа LIVE не продолжаются обе дорожки исправной заглушки")
			}
			if calls := transcodes.Load(); calls != 0 {
				t.Fatalf("AV1 copy вызвал построитель перекодирования %d раз", calls)
			}
			liveAudioDiagnosticReason(t, ctx, &logs, f.parent.SessionID, snapshot.Group.ID)
		})
	}
}

func publishLiveAudioDiagnostic(t *testing.T, parent context.Context, source *media.Source, video, audio media.CodecConfig, payload []byte, start int64) func() {
	t.Helper()
	ctx, cancel := context.WithCancel(parent)
	done := make(chan error, 1)
	go func() {
		ticker := time.NewTicker(time.Second / 60)
		defer ticker.Stop()
		audioTicks := start * int64(audio.Info().Audio.SampleRateHz) / 60
		for frame := start; ; frame++ {
			packet, err := media.NewPacket(media.PacketInfo{PTS: frame, DTS: frame, DurationTicks: 1, Keyframe: true}, video, append(video.Headers()[4:], 0x1a, 1, 0x10))
			if err == nil {
				err = source.Publish(packet)
			}
			if err != nil {
				done <- err
				return
			}
			for audioTicks*60 < (frame+1)*int64(audio.Info().Audio.SampleRateHz) {
				packet, err = media.NewPacket(media.PacketInfo{PTS: audioTicks, DTS: audioTicks, DurationTicks: 1024}, audio, payload)
				if err == nil {
					err = source.Publish(packet)
				}
				if err != nil {
					done <- err
					return
				}
				audioTicks += 1024
			}
			select {
			case <-ctx.Done():
				done <- nil
				return
			case <-ticker.C:
			}
		}
	}()
	var once sync.Once
	stop := func() {
		once.Do(func() {
			cancel()
			if err := <-done; err != nil {
				t.Error("публикация контролируемого LIVE:", err)
			}
		})
	}
	t.Cleanup(stop)
	return stop
}

func liveAudioDiagnosticPayload(t *testing.T, ctx context.Context, output *media.Subscription, want []byte) {
	t.Helper()
	for {
		delivery, err := output.Next(ctx)
		if err != nil {
			t.Fatal("совместимый LIVE не передал исходный AAC:", err)
		}
		if delivery.IsCurrent() && delivery.Packet.Config().Info().Track == media.TrackAudio && bytes.Equal(delivery.Packet.Payload(), want) {
			return
		}
	}
}

func liveAudioDiagnosticReason(t *testing.T, parent context.Context, logs *liveAudioDiagnosticLog, session, group core.ID) {
	t.Helper()
	ctx, cancel := context.WithTimeout(parent, time.Second)
	defer cancel()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for {
		decoder := json.NewDecoder(bytes.NewReader(logs.snapshot()))
		for {
			var record map[string]any
			if err := decoder.Decode(&record); errors.Is(err, io.EOF) {
				break
			} else if err != nil {
				t.Fatal("некорректный JSON журнала:", err)
			}
			reason, _ := record["error"].(string)
			actual, expected, describesExpected := strings.Cut(reason, "ожида")
			if describesExpected && strings.Contains(actual, "AAC") && strings.Contains(actual, "44100") && strings.Contains(expected, "48000") && !strings.Contains(expected, "44100") {
				if record["session_id"] != string(session) || record["group_id"] != string(group) || record["component"] != "group" || record["operation"] == nil || record["level"] != "ERROR" {
					t.Fatal("причина несовместимого AAC потеряла корреляцию или уровень:", record)
				}
				return
			}
		}
		select {
		case <-ctx.Done():
			t.Fatalf("при исправной заглушке нет коррелированной причины отказа LIVE AAC с фактической частотой 44100 и ожидаемой 48000; журнал:\n%s", logs.snapshot())
		case <-ticker.C:
		}
	}
}
