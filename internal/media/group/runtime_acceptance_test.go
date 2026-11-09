package group

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
)

type acceptanceBits struct {
	data  []byte
	count uint
}

func (b *acceptanceBits) put(v uint64, n uint) {
	for i := n; i > 0; i-- {
		if b.count%8 == 0 {
			b.data = append(b.data, 0)
		}
		b.data[len(b.data)-1] |= byte(v>>(i-1)&1) << (7 - b.count%8)
		b.count++
	}
}
func (b *acceptanceBits) ue(v uint32) {
	n := uint64(v) + 1
	w := uint(0)
	for x := n; x != 0; x >>= 1 {
		w++
	}
	b.put(0, w-1)
	b.put(n, w)
}

// Заголовки записаны по синтаксису AV1 §5.5 и H.264 7.3.2.1.1/VUI.
// Срезы проверяют протокол и независимость; декодируемость устанавливает отдельная FFmpeg-проверка.
func acceptanceVideoHeaders(codec core.Codec, width, height, fps uint32) []byte {
	return acceptanceVideoHeadersAtRate(codec, width, height, core.FrameRate{Numerator: fps, Denominator: 1})
}
func acceptanceVideoHeadersAtRate(codec core.Codec, width, height uint32, rate core.FrameRate) []byte {
	b := &acceptanceBits{}
	if codec == core.CodecAV1 {
		b.put(0, 5)
		b.put(1, 1)
		b.put(uint64(rate.Denominator), 32)
		b.put(uint64(rate.Numerator), 32)
		b.put(1, 1)
		b.put(1, 1)
		b.put(0, 1)
		b.put(0, 1)
		b.put(0, 5)
		b.put(0, 12)
		b.put(8, 5)
		b.put(0, 1)
		b.put(11, 4)
		b.put(11, 4)
		b.put(uint64(width-1), 12)
		b.put(uint64(height-1), 12)
		b.put(0, 1)
		b.put(0, 3)
		b.put(0, 5)
		b.put(0, 1)
		b.put(0, 1)
		b.put(0, 3)
		b.put(0, 1)
		b.put(0, 1)
		b.put(1, 1)
		b.put(1, 8)
		b.put(1, 8)
		b.put(1, 8)
		b.put(0, 1)
		b.put(0, 2)
		b.put(0, 1)
		b.put(0, 1)
		b.put(1, 1)
		return append([]byte{0x81, 8, 0x0c, 0, 0x0a, byte(len(b.data))}, b.data...)
	}
	b.put(100, 8)
	b.put(0, 8)
	b.put(51, 8)
	b.ue(0)
	b.ue(1)
	b.ue(0)
	b.ue(0)
	b.put(0, 1)
	b.put(0, 1)
	b.ue(0)
	b.ue(0)
	b.ue(0)
	b.ue(1)
	b.put(0, 1)
	b.ue((width+15)/16 - 1)
	b.ue((height+15)/16 - 1)
	b.put(1, 1)
	b.put(1, 1)
	if height%16 != 0 {
		b.put(1, 1)
		b.ue(0)
		b.ue(0)
		b.ue(0)
		b.ue((16 - height%16) / 2)
	} else {
		b.put(0, 1)
	}
	b.put(1, 1)
	b.put(0, 1)
	b.put(0, 1)
	b.put(1, 1)
	b.put(5, 3)
	b.put(0, 1)
	b.put(1, 1)
	b.put(1, 8)
	b.put(1, 8)
	b.put(1, 8)
	b.put(0, 1)
	b.put(1, 1)
	b.put(uint64(rate.Denominator), 32)
	b.put(uint64(2*rate.Numerator), 32)
	b.put(1, 1)
	b.put(0, 4)
	b.put(1, 1)
	for b.count%8 != 0 {
		b.put(0, 1)
	}
	sps := []byte{0x67}
	zeros := 0
	for _, v := range b.data {
		if zeros == 2 && v <= 3 {
			sps = append(sps, 3)
			zeros = 0
		}
		sps = append(sps, v)
		if v == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	pps := []byte{0x68, 0xce, 0x3c, 0x80}
	result := []byte{1, 100, 0, 51, 0xff, 0xe1, byte(len(sps) >> 8), byte(len(sps))}
	result = append(result, sps...)
	result = append(result, 1, 0, byte(len(pps)))
	return append(result, pps...)
}

func acceptanceRuntimeConfigs(t testing.TB, p core.GroupProfile, epoch uint64) (media.CodecConfig, media.CodecConfig) {
	t.Helper()
	v, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: epoch, Revision: 1, TimeBase: media.TimeBase{Numerator: 1, Denominator: p.Format.Video.FrameRate.Numerator}, Video: p.Format.Video}, acceptanceVideoHeaders(p.Format.Video.Codec, p.Format.Video.Width, p.Format.Video.Height, p.Format.Video.FrameRate.Numerator))
	if err != nil {
		t.Fatal(err)
	}
	a, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackAudio, SourceEpoch: epoch, Revision: 1, TimeBase: media.TimeBase{Numerator: 1, Denominator: 48000}, Audio: p.Format.Audio}, []byte{0x11, 0x90})
	if err != nil {
		t.Fatal(err)
	}
	return v, a
}

func acceptanceRuntimeProfile(codec core.Codec, width, height uint32) core.GroupProfile {
	p := switchAcceptanceConfig().Profile
	p.Mode = core.ModeTranscode
	p.Format.Video.Codec = codec
	p.Format.Video.CodecProfile = "high"
	if codec == core.CodecAV1 {
		p.Mode = core.ModeCopy
		p.Format.Video.CodecProfile = "main"
	}
	p.Format.Video.Width = width
	p.Format.Video.Height = height
	p.Format.Video.FrameRate = core.FrameRate{Numerator: 60, Denominator: 1}
	p.Format.Video.Color = core.ColorInfo{Range: "limited", Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"}
	p.VideoEncoding.GOPFrames = 60
	return p
}

func acceptanceRuntimeLimits() fmp4.Limits {
	return fmp4.Limits{MaxBoxBytes: 1 << 20, MaxInitBytes: 1 << 20, MaxMoofBytes: 1 << 16, MaxMdatBytes: 1 << 20, MaxSampleBytes: 1 << 18, MaxExtradataBytes: 1 << 12, MaxSamples: 100}
}

func acceptanceHelperCommand(p core.GroupProfile, a ffmpeg.Attempt) (ffmpeg.Command, error) {
	binary, err := os.Executable()
	if err != nil {
		return ffmpeg.Command{}, err
	}
	return ffmpeg.Command{Binary: binary, Args: []string{"-test.run=^TestGroupControlledMediaProcess$", "--", a.OutputPath, string(p.Format.Video.Codec), fmt.Sprint(p.Format.Video.Width), fmt.Sprint(p.Format.Video.Height), a.InputPath}}, nil
}

func TestGroupControlledMediaProcess(t *testing.T) {
	position := -1
	for i, a := range os.Args {
		if a == "--" {
			position = i
			break
		}
	}
	if position < 0 {
		return
	}
	args := os.Args[position+1:]
	if len(args) != 5 {
		os.Exit(21)
	}
	w, e := strconv.ParseUint(args[2], 10, 32)
	if e != nil {
		os.Exit(22)
	}
	h, e := strconv.ParseUint(args[3], 10, 32)
	if e != nil {
		os.Exit(23)
	}
	output, e := net.Dial("unix", args[0])
	if e != nil {
		os.Exit(24)
	}
	defer func() { _ = output.Close() }()
	if args[4] != "" {
		input, e := net.Dial("unix", args[4])
		if e != nil {
			os.Exit(25)
		}
		defer func() { _ = input.Close() }()
		go func() { _, _ = io.Copy(io.Discard, input) }()
	}
	p := acceptanceRuntimeProfile(core.Codec(args[1]), uint32(w), uint32(h))
	v, a := acceptanceRuntimeConfigs(t, p, 1)
	mux, e := fmp4.NewMuxer(output, fmp4.Init{Video: v, Audio: a}, acceptanceRuntimeLimits())
	if e != nil {
		os.Exit(26)
	}
	ticker := time.NewTicker(time.Second / 60)
	defer ticker.Stop()
	var audio int64
	for video := int64(0); ; video++ {
		payload := []byte{0, 0, 0, 3, 0x65, 0xb8, 0x40}
		if p.Format.Video.Codec == core.CodecAV1 {
			payload = append(v.Headers()[4:], 0x1a, 1, 0x10)
		}
		packet, e := media.NewPacket(media.PacketInfo{PTS: video, DTS: video, DurationTicks: 1, Keyframe: true}, v, payload)
		if e != nil || mux.WritePacket(packet) != nil {
			return
		}
		for audio*60 < (video+1)*48000 {
			packet, e = media.NewPacket(media.PacketInfo{PTS: audio, DTS: audio, DurationTicks: 1024}, a, switchAcceptanceAACPayload())
			if e != nil || mux.WritePacket(packet) != nil {
				return
			}
			audio += 1024
		}
		<-ticker.C
	}
}

type acceptanceRuntimeFixture struct {
	service  *core.Service
	parent   core.ExecutionFence
	observer *core.SessionObserver
	outputs  []core.SessionOutput
	config   RuntimeConfig
}

func newRuntimeAcceptanceFixture(t *testing.T) acceptanceRuntimeFixture {
	t.Helper()
	ctx := context.Background()
	options := storage.Options{PreparationLimits: core.PreparationLimits{MaxQueued: 3, MaxProfiles: 8}, OutputExecutionLimits: core.OutputExecutionLimits{MaxReservations: 128, MaxIdentifierBytes: 1024, MaxBytes: 4 << 20}, ObservationLimits: core.ObservationLimits{MaxSessions: 8, MaxGroups: 32, MaxOutputs: 64, MaxFFmpegWorkers: 32, MaxIdentifierBytes: 1024, MaxErrorBytes: 65536, MaxBytes: 4 << 20}, MaxOpenConns: 4, MaxIdleConns: 4, ConnMaxLifetime: time.Minute, ConnMaxIdleTime: time.Minute, BusyTimeout: 10 * time.Millisecond, RetryAttempts: 3, RetryTimeout: time.Second, RetryDelay: time.Millisecond}
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "runtime.sqlite"), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	svc, err := core.NewService(store, store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	asset, err := svc.CreateAsset(ctx, "Заглушка")
	if err != nil {
		t.Fatal(err)
	}
	policy := core.FallbackPolicy{AssetID: asset.ID, Watchdog: switchAcceptanceConfig().Watchdog}
	stream, err := svc.CreateStream(ctx, core.Stream{Name: "Поток", PublicationKey: "runtime-key", Fallback: policy})
	if err != nil {
		t.Fatal(err)
	}
	var groups []core.TranscodeGroup
	for _, p := range []core.GroupProfile{acceptanceRuntimeProfile(core.CodecAV1, 2560, 1440), acceptanceRuntimeProfile(core.CodecH264, 2560, 1440), acceptanceRuntimeProfile(core.CodecH264, 1920, 1080)} {
		profile, err := svc.CreateProfile(ctx, core.TranscodeProfile{Name: "Профиль", Profile: p})
		if err != nil {
			t.Fatal(err)
		}
		g, err := svc.CreateGroup(ctx, core.TranscodeGroup{Name: "Группа", StreamID: stream.ID, ProfileID: profile.ID})
		if err != nil {
			t.Fatal(err)
		}
		groups = append(groups, g)
		count := 1
		if p.Format.Video.Codec == core.CodecH264 && p.Format.Video.Width == 2560 {
			count = 2
		}
		for i := 0; i < count; i++ {
			if _, err = svc.CreateDestination(ctx, core.Destination{Name: "Назначение", StreamID: stream.ID, GroupID: g.ID, Platform: "custom", Endpoint: "rtmp://example.invalid/live", Enabled: true}); err != nil {
				t.Fatal(err)
			}
		}
	}
	g, err := svc.ReservePreparation(ctx, core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage, GeneratorVersion: "runtime-generator"})
	if err != nil {
		t.Fatal(err)
	}
	pf, err := svc.ClaimPreparation(ctx, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.MarkPreparationIssued(ctx, pf); err != nil {
		t.Fatal(err)
	}
	publication := core.GenerationPublication{SourceHash: "controlled-source", SizeBytes: 1, SourceFormat: core.SourceFormat{ImageFormat: "png"}}
	for _, r := range g.RequiredProfiles {
		publication.Variants = append(publication.Variants, core.FallbackVariant{ID: r.VariantID, GenerationID: g.ID, AssetID: g.AssetID, SourceHash: publication.SourceHash, Profile: r.Profile, VerifiedFormat: r.Profile.Format, GeneratorVersion: g.GeneratorVersion, ManagedPath: r.ManagedPath, SizeBytes: 1, ContentHash: "controlled-result"})
	}
	if _, err = svc.PublishPreparation(ctx, pf, publication); err != nil {
		t.Fatal(err)
	}
	if err = svc.AcknowledgePreparationCleanup(ctx, pf); err != nil {
		t.Fatal(err)
	}
	request := core.StartSessionRequest{RequestID: "runtime-parent", StreamID: stream.ID}
	for _, group := range groups {
		profile, err := svc.GetProfile(ctx, group.ProfileID)
		if err != nil {
			t.Fatal(err)
		}
		for _, r := range g.RequiredProfiles {
			if r.Profile.Equal(profile.Profile) {
				request.Bindings = append(request.Bindings, core.PreparedBinding{GroupID: group.ID, GenerationID: g.ID, VariantID: r.VariantID, GeneratorVersion: g.GeneratorVersion})
			}
		}
	}
	receipt, err := svc.StartSession(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	parent, err := svc.Claim(ctx, receipt.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.MarkIssued(ctx, parent); err != nil {
		t.Fatal(err)
	}
	if err = svc.Complete(ctx, parent, core.Completion{Success: true}); err != nil {
		t.Fatal(err)
	}
	observer, err := svc.OpenSessionObserver(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = observer.Close(context.Background()) })
	outputs, err := svc.ListOutputs(ctx, core.PageRequest{Query: core.ListQuery{Limit: 20, SessionID: parent.SessionID}})
	if err != nil {
		t.Fatal(err)
	}
	parentDirectory := os.TempDir()
	if runtime.GOOS == "darwin" {
		parentDirectory = "/tmp"
	}
	base, err := os.MkdirTemp(parentDirectory, "group-acceptance-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := os.RemoveAll(base); err != nil {
			t.Error(err)
		}
	})
	c := RuntimeConfig{Switcher: switchAcceptanceConfig(), Binary: "controlled", Container: acceptanceRuntimeLimits(), ResubscribeBackoff: time.Millisecond, ControlInterval: 5 * time.Millisecond, ShutdownTimeout: 2 * time.Second, FFmpeg: ffmpeg.Config{BaseDir: base, StartupTimeout: time.Second, VideoTimeout: time.Second, AudioTimeout: time.Second, WatchdogInterval: 10 * time.Millisecond, RestartBackoff: time.Millisecond, GracePeriod: 50 * time.Millisecond, KillTimeout: time.Second, ShutdownTimeout: 2 * time.Second, DiagnosticEvents: 32, DiagnosticBytes: 1 << 16, DiagnosticChunkBytes: 1024, MaxCommandBytes: 8192, MaxCommandArgs: 64}}
	c.OpenFile = func(context.Context, core.ArtifactManifest) (*os.File, error) { return nil, os.ErrNotExist }

	return acceptanceRuntimeFixture{svc, parent, observer, outputs.Items, c}
}

func waitRuntimeAcceptance(t *testing.T, predicate func() bool) {
	t.Helper()
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for !predicate() {
		select {
		case <-ctx.Done():
			t.Fatal("условие runtime не выполнено до срока")
		case <-ticker.C:
		}
	}
}

func TestRegistrySharesGroupResultAndLastReferenceJoins(t *testing.T) {
	f := newRuntimeAcceptanceFixture(t)
	ctx := context.Background()
	live, err := media.NewRouter(switchAcceptanceConfig().Router)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = live.Close() }()
	var attempts atomic.Int32
	c := f.config
	c.OpenFile = func(context.Context, core.ArtifactManifest) (*os.File, error) {
		attempts.Add(1)
		return nil, os.ErrNotExist
	}
	r, err := NewRegistry(ctx, RegistryConfig{Core: f.service, Parent: f.parent, Observer: f.observer, Runtime: c, MaxGroups: 8, MaxReferences: 8})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	var same []core.SessionOutput
	for _, o := range f.outputs {
		if o.Snapshot.Profile.Profile.Format.Video.Codec == core.CodecH264 && o.Snapshot.Profile.Profile.Format.Video.Width == 2560 {
			same = append(same, o)
		}
	}
	if len(same) != 2 {
		t.Fatal("нет двух назначений общей группы")
	}
	a, err := r.Acquire(ctx, same[0].Snapshot, live)
	if err != nil {
		t.Fatal(err)
	}
	b, err := r.Acquire(ctx, same[1].Snapshot, live)
	if err != nil {
		t.Fatal(err)
	}
	if a.Group() != b.Group() || a.Router() != b.Router() {
		t.Fatal("два назначения получили разные общие результаты")
	}
	waitRuntimeAcceptance(t, func() bool { return attempts.Load() > 0 })
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-b.Group().Done():
		t.Fatal("первый ref остановил используемую группу")
	default:
	}
	if err = b.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-b.Group().Done():
	case <-time.After(3 * time.Second):
		t.Fatal("последний ref не остановил и не join группу")
	}
	if !f.observer.Valid() {
		t.Fatal("last ref отозвал parent Session")
	}
}

func TestGroupParallelProfilesKeepFallbackAndRoutersAcrossOverride(t *testing.T) {
	f := newRuntimeAcceptanceFixture(t)
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	live, err := media.NewRouter(media.RouterLimits{MaxConsumers: 8, MaxRetainedBytes: 12 << 20, MaxConfigBytes: 1 << 12})
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = live.Close() }()
	c := f.config
	c.Switcher.Queue.ResyncTimeout = 40 * time.Millisecond
	var files sync.Map
	var transcodes atomic.Int32
	var players atomic.Int32
	dir := t.TempDir()
	c.OpenFile = func(_ context.Context, m core.ArtifactManifest) (*os.File, error) {
		file, err := os.CreateTemp(dir, "held")
		if err == nil {
			files.Store(file, m.Profile)
		}
		return file, err
	}
	c.Player = func(_ context.Context, _ string, file *os.File, a ffmpeg.Attempt) (ffmpeg.Command, error) {
		p, ok := files.LoadAndDelete(file)
		if !ok {
			return ffmpeg.Command{}, core.ErrCorrupt
		}
		players.Add(1)
		command, err := acceptanceHelperCommand(p.(core.GroupProfile), a)
		command.ExtraFiles = []*os.File{file}
		return command, err
	}
	c.Transcode = func(_ context.Context, n ffmpeg.NVIDIAConfig, a ffmpeg.Attempt) (ffmpeg.Command, error) {
		if n.Profile.Format.Video.Codec != core.CodecH264 {
			return ffmpeg.Command{}, core.ErrInvalid
		}
		transcodes.Add(1)
		return acceptanceHelperCommand(n.Profile, a)
	}
	r, err := NewRegistry(ctx, RegistryConfig{Core: f.service, Parent: f.parent, Observer: f.observer, Runtime: c, MaxGroups: 8, MaxReferences: 8})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = r.Close(context.Background()) })
	var refs []*Reference
	for _, o := range f.outputs {
		ref, err := r.Acquire(ctx, o.Snapshot, live)
		if err != nil {
			t.Fatal(err)
		}
		refs = append(refs, ref)
	}
	groups := map[*Group]bool{}
	for _, ref := range refs {
		groups[ref.Group()] = true
	}
	if len(groups) != 3 {
		t.Fatalf("три полных профиля создали %d групп", len(groups))
	}
	t.Cleanup(func() {
		if t.Failed() {
			for g := range groups {
				g.switcher.mu.Lock()
				liveFeed := g.switcher.feeds[0]
				fallbackFeed := g.switcher.feeds[1]
				t.Logf("group=%s codec=%s width=%d status=%+v attempts=%d/%d live attempt=%d seen=%v dts=%v stable=%v rejected=%v failure=%v queue=%d fallback seen=%v failure=%v transcodes=%d players=%d", g.id, g.config.Switcher.Profile.Format.Video.Codec, g.config.Switcher.Profile.Format.Video.Width, g.switcher.status, g.attempts[0].Load(), g.attempts[1].Load(), liveFeed.attempt, liveFeed.seen, liveFeed.dts, liveFeed.stable, liveFeed.rejected, liveFeed.failure, len(liveFeed.queue), fallbackFeed.seen, fallbackFeed.failure, transcodes.Load(), players.Load())
				g.switcher.mu.Unlock()
			}
		}
	})
	waitRuntimeAcceptance(t, func() bool {
		for g := range groups {
			if !g.Status().Available || g.Status().Source != Fallback || g.attempts[0].Load() < 2 {
				return false
			}
		}
		return true
	})
	if transcodes.Load() != 0 || players.Load() != 3 {
		t.Fatalf("до OBS: transcode=%d players=%d", transcodes.Load(), players.Load())
	}
	var producerCancel context.CancelFunc
	var producerDone <-chan struct{}
	startProducer := func() {
		source, err := live.BeginSource()
		if err != nil {
			t.Fatal(err)
		}
		p := acceptanceRuntimeProfile(core.CodecAV1, 2560, 1440)
		v, a := acceptanceRuntimeConfigs(t, p, source.Epoch())
		if err = source.UpdateConfigs(v, a); err != nil {
			t.Fatal(err)
		}
		work, stop := context.WithCancel(ctx)
		producerCancel = stop
		done := make(chan struct{})
		producerDone = done
		go func() {
			defer close(done)
			ticker := time.NewTicker(time.Second / 60)
			defer ticker.Stop()
			var audio int64
			for video := int64(0); ; video++ {
				packet, e := media.NewPacket(media.PacketInfo{PTS: video, DTS: video, DurationTicks: 1, Keyframe: true}, v, append(v.Headers()[4:], 0x1a, 1, 0x10))
				if e != nil || source.Publish(packet) != nil {
					return
				}
				for audio*60 < (video+1)*48000 {
					packet, e = media.NewPacket(media.PacketInfo{PTS: audio, DTS: audio, DurationTicks: 1024}, a, switchAcceptanceAACPayload())
					if e != nil || source.Publish(packet) != nil {
						return
					}
					audio += 1024
				}
				select {
				case <-work.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
	startProducer()
	defer func() { producerCancel(); <-producerDone }()
	waitRuntimeAcceptance(t, func() bool {
		for g := range groups {
			if !g.Status().Available || g.Status().Source != Live {
				return false
			}
		}
		return true
	})
	if transcodes.Load() != 2 {
		t.Fatalf("ожидались два независимых H.264 encoder и ни одного copy encoder: %d", transcodes.Load())
	}
	var shared []*Reference
	var independent *Reference
	for _, ref := range refs {
		if ref.Group().config.Switcher.Profile.Format.Video.Codec == core.CodecH264 {
			if ref.Group().config.Switcher.Profile.Format.Video.Width == 2560 {
				shared = append(shared, ref)
			} else {
				independent = ref
			}
		}
	}
	if len(shared) != 2 || shared[0].Router() != shared[1].Router() {
		t.Fatal("общая H.264 группа не разделяет результат")
	}
	q := c.Switcher.Queue
	q.ResyncTimeout = time.Second
	a, err := shared[0].Router().Subscribe(q)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = a.Close() }()
	b, err := shared[1].Router().Subscribe(q)
	if err != nil {
		t.Fatal(err)
	}
	defer func() { _ = b.Close() }()
	seen := map[string]media.Packet{}
	for range 8 {
		d := switchAcceptanceNext(t, a)
		seen[fmt.Sprintf("%s/%d", d.Packet.Config().Info().Track, d.Packet.Info().DTS)] = d.Packet
	}
	matches := 0
	for range 8 {
		d := switchAcceptanceNext(t, b)
		if p, ok := seen[fmt.Sprintf("%s/%d", d.Packet.Config().Info().Track, d.Packet.Info().DTS)]; ok {
			if p.Info() != d.Packet.Info() || string(p.Payload()) != string(d.Packet.Payload()) {
				t.Fatal("consumers общей группы получили разные результаты")
			}
			matches++
		}
	}
	if matches == 0 {
		t.Fatal("не найден общий результат двух consumers")
	}
	stable := map[*Group]*media.Router{}
	for g := range groups {
		stable[g] = g.Router()
	}
	producerCancel()
	<-producerDone
	startProducer()
	waitRuntimeAcceptance(t, func() bool {
		for g := range groups {
			if !g.Status().Available || g.Status().Source != Live || g.Router() != stable[g] {
				return false
			}
		}
		return transcodes.Load() >= 4
	})
	if _, err = f.service.PauseSession(ctx, core.SessionCommand{RequestID: "runtime-manual-pause", SessionID: f.parent.SessionID}); err != nil {
		t.Fatal(err)
	}
	waitRuntimeAcceptance(t, func() bool {
		for g := range groups {
			if !g.Status().Paused || g.Status().Source != Fallback || !g.Status().Available {
				return false
			}
		}
		return true
	})
	if _, err = f.service.ResumeSession(ctx, core.SessionCommand{RequestID: "runtime-manual-resume", SessionID: f.parent.SessionID}); err != nil {
		t.Fatal(err)
	}
	waitRuntimeAcceptance(t, func() bool {
		for g := range groups {
			if g.Status().Source != Live || g.Status().Paused || !g.Status().Available {
				return false
			}
		}
		return true
	})
	if independent == nil {
		t.Fatal("нет отдельной 1080p группы")
	}
	if err = independent.Group().Stop(ctx); err != nil {
		t.Fatal(err)
	}
	for g := range groups {
		if g != independent.Group() {
			if g.Router() != stable[g] || !g.Status().Available {
				t.Fatal("отказ одной группы изменил здорового соседа")
			}
			select {
			case <-g.Done():
				t.Fatal("отказ одной группы остановил другую")
			default:
			}
		}
	}
	_ = switchAcceptanceNext(t, a)
}
