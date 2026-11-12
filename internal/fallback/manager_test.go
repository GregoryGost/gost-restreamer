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

// Создаёт конечную конфигурацию тестового менеджера с явными пределами Runner, Prober и очереди.
func selfConfig() Config {
	return Config{ReconcileLimit: 10, GeneratorVersion: "generator", FFmpeg: "ffmpeg", FFprobe: "ffprobe", MaxUploadBytes: 1 << 20, MaxOutputBytes: 8 << 20, PrepareTimeout: time.Minute, ShutdownTimeout: time.Second, QueueInterval: time.Millisecond, Runner: selfRunner().Limits, Probe: selfProbeLimits()}
}

// Проверяет обязательность реестра у рабочего менеджера, валидацию зависимостей и бюджетов и отказ
// Upload/Rebuild после закрытия.
func TestDefaultManagerValidatesLimitsAndOwnsItsClosedLifecycle(t *testing.T) {
	// files содержит временное файловое хранилище для проверки зависимостей менеджера.
	files, _ := selfFileStore(t)
	// c содержит допустимую конфигурацию без обязательного рабочего реестра процессов.
	c := selfConfig()
	// err содержит результат создания рабочего менеджера без ProcessRegistry.
	if _, err := NewManager(&core.Service{}, files, c); !errors.Is(err, ErrInvalid) {
		t.Fatal("рабочий Manager без ProcessRegistry принят", err)
	}
	// runner содержит исполнитель с конечными тестовыми ограничениями.
	runner := Runner{Limits: c.Runner}
	// probe содержит проверяющий порт ffprobe с тем же исполнителем.
	probe := Prober{Binary: c.FFprobe, Runner: runner, Limits: c.Probe}
	// m содержит менеджер с явно переданными портами. err сообщает об отказе его создания.
	m, err := NewManagerWithTools(&core.Service{}, files, c, Tools{Builder: ProductionBuilder(c.FFmpeg), Inspector: probe, Runner: runner})
	if err != nil {
		t.Fatal(err)
	}
	// command содержит рабочую команду подготовки H.264 для проверки сохранения h264_nvenc.
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

// Проверяет независимость канонических профилей в снимках Generation/Wait и раскрытие причины
// PreparationError.
func TestJobSnapshotsDoNotExposeMutableRequiredProfiles(t *testing.T) {
	// g содержит поколение с изменяемым каноническим буфером профиля.
	g := core.FallbackGeneration{RequiredProfiles: []core.RequiredProfile{{CanonicalProfile: []byte("immutable")}}}
	// j содержит уже завершённое задание для проверки независимости возвращаемых снимков.
	j := &Job{generation: g, done: make(chan struct{}), result: Result{Generation: g, Clean: true}}
	close(j.done)
	// snapshot содержит копию исходного поколения, изменение которой не должно менять Job.
	snapshot := j.Generation()
	snapshot.RequiredProfiles[0].CanonicalProfile[0] = 'x'
	if string(j.Generation().RequiredProfiles[0].CanonicalProfile) != "immutable" {
		t.Fatal("снимок изменил принятое поколение")
	}
	// r содержит первый итог задания. err сообщает об отказе ожидания.
	r, err := j.Wait(context.Background())
	if err != nil || !r.Clean {
		t.Fatal(r, err)
	}
	r.Generation.RequiredProfiles[0].CanonicalProfile[0] = 'x'
	// again содержит повторно полученный итог после изменения предыдущей копии.
	again, err := j.Wait(context.Background())
	if err != nil || string(again.Generation.RequiredProfiles[0].CanonicalProfile) != "immutable" {
		t.Fatal(again, err)
	}
	// wrapped содержит ошибку неоднозначного принятия с известным идентификатором поколения.
	wrapped := &PreparationError{GenerationID: "known", Err: ErrCleanupPending}
	if !errors.Is(wrapped, ErrCleanupPending) || !strings.Contains(wrapped.Error(), "known") {
		t.Fatal(wrapped)
	}
}
