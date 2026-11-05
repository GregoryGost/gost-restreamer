package fallback

import (
	"context"
	"errors"
	"io"
	"strings"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

func selfConfig() Config {
	return Config{ReconcileLimit: 10, GeneratorVersion: "generator", FFmpeg: "ffmpeg", FFprobe: "ffprobe", MaxUploadBytes: 1 << 20, MaxOutputBytes: 8 << 20, PrepareTimeout: time.Minute, ShutdownTimeout: time.Second, QueueInterval: time.Millisecond, Runner: selfRunner().Limits, Probe: selfProbeLimits()}
}

func TestDefaultManagerValidatesLimitsAndOwnsItsClosedLifecycle(t *testing.T) {
	files, _ := selfFileStore(t)
	c := selfConfig()
	if _, err := NewManager(&core.Service{}, files, c); !errors.Is(err, ErrInvalid) {
		t.Fatal("рабочий Manager без ProcessRegistry принят", err)
	}
	runner := Runner{Limits: c.Runner}
	probe := Prober{Binary: c.FFprobe, Runner: runner, Limits: c.Probe}
	m, err := NewManagerWithTools(&core.Service{}, files, c, Tools{Builder: ProductionBuilder(c.FFmpeg), Inspector: probe, Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	command, err := m.tools.Builder(GenerateRequest{Source: Source{Kind: core.AssetImage, Duration: c.Probe.ImageDuration, Demuxer: "png_pipe"}, Profile: selfProfile(core.CodecH264), LoopDuration: c.Probe.ImageDuration})
	if err != nil || !strings.Contains(strings.Join(command.Args, " "), "h264_nvenc") {
		t.Fatal(command, err)
	}
	if err = m.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	if _, err = m.Upload(context.Background(), core.PreparationRequest{}, io.NopCloser(strings.NewReader("unread"))); !errors.Is(err, core.ErrClosed) {
		t.Fatal(err)
	}
	if _, err = m.Rebuild(context.Background(), core.PreparationRequest{}); !errors.Is(err, core.ErrClosed) {
		t.Fatal(err)
	}
	if _, err = m.Upload(context.Background(), core.PreparationRequest{}, nil); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err = m.Upload(context.Background(), core.PreparationRequest{Rebuild: true}, io.NopCloser(strings.NewReader("unread"))); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err = NewManager(nil, files, c); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	if _, err = NewManager(&core.Service{}, nil, c); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	c.MaxOutputBytes = 0
	if _, err = NewManager(&core.Service{}, files, c); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
}

func TestJobSnapshotsDoNotExposeMutableRequiredProfiles(t *testing.T) {
	g := core.FallbackGeneration{RequiredProfiles: []core.RequiredProfile{{CanonicalProfile: []byte("immutable")}}}
	j := &Job{generation: g, done: make(chan struct{}), result: Result{Generation: g, Clean: true}}
	close(j.done)
	snapshot := j.Generation()
	snapshot.RequiredProfiles[0].CanonicalProfile[0] = 'x'
	if string(j.Generation().RequiredProfiles[0].CanonicalProfile) != "immutable" {
		t.Fatal("снимок изменил принятое поколение")
	}
	r, err := j.Wait(context.Background())
	if err != nil || !r.Clean {
		t.Fatal(r, err)
	}
	r.Generation.RequiredProfiles[0].CanonicalProfile[0] = 'x'
	again, err := j.Wait(context.Background())
	if err != nil || string(again.Generation.RequiredProfiles[0].CanonicalProfile) != "immutable" {
		t.Fatal(again, err)
	}
	wrapped := &PreparationError{GenerationID: "known", Err: ErrCleanupPending}
	if !errors.Is(wrapped, ErrCleanupPending) || !strings.Contains(wrapped.Error(), "known") {
		t.Fatal(wrapped)
	}
}
