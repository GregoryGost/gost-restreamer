package integration

import (
	"context"
	"crypto/sha256"
	"fmt"
	"log/slog"
	"math/big"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"sync"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
	"github.com/GregoryGost/gost-restreamer/internal/media/group"
	"github.com/GregoryGost/gost-restreamer/internal/media/publisher"
)

func TestLiveAudioPreparedAssetsDecodeExactSilenceAcrossPeriods(t *testing.T) {
	executable := os.Getenv("GOST_MEDIA_FFMPEG")
	if executable == "" {
		t.Skip("Локальная CPU-проверка требует GOST_MEDIA_FFMPEG")
	}
	p := fallbackPipelineProfile(core.CodecH264)
	p.LiveAudioMode = core.LiveAudioCopy
	for _, kind := range []string{"image", "video-no-audio", "video-tone", "video-tone-bframes"} {
		t.Run(kind, func(t *testing.T) {
			profile := p
			sourceKind := kind
			if kind == "video-tone-bframes" {
				profile.VideoEncoding.MaxBFrames = 2
				sourceKind = "video-tone"
			}
			accessUnits := 75
			if sourceKind == "video-tone" {
				// Контейнер с задержкой AAC длится 1,621333 с; следующий общий период при 30 FPS и 48 kHz равен 3,2 с.
				accessUnits = 150
			}
			prepared := liveAudioPrepared(t, executable, profile, sourceKind)
			pcm := mediaCommand(t, executable, prepared, "-v", "error", "-xerror", "-copyts", "-flags2:a", "+skip_manual", "-i", "pipe:0", "-map", "0:a:0", "-f", "f32le", "pipe:1")
			if len(pcm) != accessUnits*1024*2*4 || liveAudioNonzeroSamples(pcm) != 0 {
				t.Fatalf("Подготовленный период содержит звук или неполные AU: bytes=%d nonzero=%d", len(pcm), liveAudioNonzeroSamples(pcm))
			}
			packets, err := encodingPreparedPackets(prepared, profile.Format)
			if err != nil {
				t.Fatal(err)
			}
			if len(packets[media.TrackAudio]) != accessUnits || len(packets[media.TrackVideo]) != accessUnits*1024*30/48000 {
				t.Fatalf("Неверное число полных AU: video=%d audio=%d", len(packets[media.TrackVideo]), len(packets[media.TrackAudio]))
			}
			var presentation []*big.Rat
			for i, packet := range packets[media.TrackVideo] {
				stamp, base := packet.Info(), packet.Config().Info().TimeBase
				if fallbackActualTime(stamp.DurationTicks, base).Cmp(big.NewRat(1, 30)) != 0 {
					t.Fatal("Изменена длительность видеокадра")
				}
				wantDTS := big.NewRat(int64(i), 30)
				if profile.VideoEncoding.MaxBFrames == 2 {
					wantDTS.Sub(wantDTS, big.NewRat(2, 30))
				}
				if fallbackActualTime(stamp.DTS, base).Cmp(wantDTS) != 0 {
					t.Fatalf("Изменён знаковый DTS видео[%d]: %+v != %s", i, stamp, wantDTS)
				}
				presentation = append(presentation, fallbackActualTime(stamp.PTS, base))
			}
			slices.SortFunc(presentation, func(a, b *big.Rat) int { return a.Cmp(b) })
			for i, pts := range presentation {
				if pts.Cmp(big.NewRat(int64(i), 30)) != 0 {
					t.Fatalf("Изменён порядок/состав представления видео: PTS[%d]=%s", i, pts)
				}
			}
			for i, packet := range packets[media.TrackAudio] {
				stamp, base := packet.Info(), packet.Config().Info().TimeBase
				if stamp.PTS != stamp.DTS || fallbackActualTime(stamp.DTS, base).Cmp(big.NewRat(int64(i*1024), 48000)) != 0 || fallbackActualTime(stamp.DurationTicks, base).Cmp(big.NewRat(1024, 48000)) != 0 {
					t.Fatalf("Нарушен общий восстановленный origin AAC[%d]: %+v", i, stamp)
				}
			}
			var repeated []byte
			for range 3 {
				for _, packet := range packets[media.TrackAudio] {
					repeated = append(repeated, liveAudioADTS(t, packet.Config().Headers(), packet.Payload())...)
				}
			}
			pcm = mediaCommand(t, executable, repeated, "-v", "error", "-xerror", "-flags2:a", "+skip_manual", "-f", "aac", "-i", "pipe:0", "-map", "0:a:0", "-f", "f32le", "pipe:1")
			if len(pcm) != 3*accessUnits*1024*2*4 || liveAudioNonzeroSamples(pcm) != 0 {
				t.Fatalf("Граница петли содержит звук: bytes=%d nonzero=%d", len(pcm), liveAudioNonzeroSamples(pcm))
			}
		})
	}
}

type liveAudioCapture struct {
	mu       sync.Mutex
	packets  []media.Packet
	overflow bool
}

func (c *liveAudioCapture) snapshot() []media.Packet {
	c.mu.Lock()
	defer c.mu.Unlock()
	return slices.Clone(c.packets)
}

func liveAudioStartCapture(t *testing.T, router *media.Router) (*liveAudioCapture, func()) {
	t.Helper()
	capture := &liveAudioCapture{}
	sub, err := router.Subscribe(publisherQueue())
	if err != nil {
		t.Fatal(err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			delivery, err := sub.Next(ctx)
			if err != nil {
				return
			}
			capture.mu.Lock()
			if len(capture.packets) >= 8192 {
				capture.overflow = true
				capture.mu.Unlock()
				return
			}
			capture.packets = append(capture.packets, delivery.Packet)
			capture.mu.Unlock()
		}
	}()
	stop := func() { cancel(); _ = sub.Close(); <-done }
	t.Cleanup(func() {
		stop()
		capture.mu.Lock()
		defer capture.mu.Unlock()
		if capture.overflow {
			t.Error("Превышен бюджет захвата AU")
		}
	})
	return capture, stop
}

func liveAudioKnown(packets []media.Packet) map[[32]byte]bool {
	known := map[[32]byte]bool{}
	for _, p := range packets {
		if p.Config().Info().Track == media.TrackAudio {
			known[sha256.Sum256(p.Payload())] = true
		}
	}
	return known
}

func liveAudioMatched(messages []publisherWireMessage, known, quiet map[[32]byte]bool) int {
	n := 0
	for _, m := range messages {
		if m.typ == 8 && len(m.body) > 2 && m.body[1] == 1 {
			hash := sha256.Sum256(m.body[2:])
			if known[hash] && !quiet[hash] {
				n++
			}
		}
	}
	return n
}

func liveAudioAssertGroupTiming(t *testing.T, packets []media.Packet, profile core.GroupProfile, source, reference []media.Packet, quiet map[[32]byte]bool) {
	t.Helper()
	type segment struct{ video, audio *big.Rat }
	segments := map[uint64]*segment{}
	last := map[media.Track]*big.Rat{}
	sourceTimes := map[[32]byte][]*big.Rat{}
	sourceDurations := map[[32]byte][]*big.Rat{}
	previousIngress := map[uint64]media.Packet{}
	for _, packet := range source {
		if packet.Config().Info().Track != media.TrackAudio {
			continue
		}
		hash := sha256.Sum256(packet.Payload())
		config := packet.Config().Info()
		if previous, exists := previousIngress[config.SourceEpoch]; exists {
			previousHash := sha256.Sum256(previous.Payload())
			duration := new(big.Rat).Sub(fallbackActualTime(packet.Info().DTS, config.TimeBase), fallbackActualTime(previous.Info().DTS, previous.Config().Info().TimeBase))
			sourceDurations[previousHash] = append(sourceDurations[previousHash], duration)
		}
		previousIngress[config.SourceEpoch] = packet
		if quiet[hash] {
			continue
		}
		sourceTimes[hash] = append(sourceTimes[hash], fallbackActualTime(packet.Info().DTS, config.TimeBase))
	}
	type referencePosition struct {
		owner  uint64
		index  int
		offset *big.Rat
	}
	referenceStreams := map[uint64][]media.Packet{}
	for _, packet := range reference {
		if packet.Config().Info().Track == media.TrackAudio {
			owner := packet.Config().Info().SourceEpoch
			referenceStreams[owner] = append(referenceStreams[owner], packet)
		}
	}
	nativePositions := map[uint64][]referencePosition{}
	lastAudio := map[uint64]int{}
	liveEpochs := map[uint64]bool{}
	for i, packet := range packets {
		if packet.Config().Info().Track == media.TrackAudio {
			lastAudio[packet.Config().Info().SourceEpoch] = i
			hash := sha256.Sum256(packet.Payload())
			if profile.EffectiveLiveAudioMode() == core.LiveAudioCopy && len(sourceTimes[hash]) > 0 || profile.EffectiveLiveAudioMode() == core.LiveAudioTranscode && !quiet[hash] {
				liveEpochs[packet.Config().Info().SourceEpoch] = true
			}
		}
	}
	shifts := map[uint64][]*big.Rat{}
	for index, packet := range packets {
		config, stamp := packet.Config().Info(), packet.Info()
		dts := fallbackActualTime(stamp.DTS, config.TimeBase)
		if previous := last[config.Track]; previous != nil && dts.Cmp(previous) <= 0 {
			t.Fatalf("Переход сбросил DTS %s: %s -> %s", config.Track, previous, dts)
		}
		last[config.Track] = dts
		wantDuration := big.NewRat(int64(profile.Format.Video.FrameRate.Denominator), int64(profile.Format.Video.FrameRate.Numerator))
		if config.Track == media.TrackAudio {
			wantDuration = big.NewRat(1024, int64(profile.Format.Audio.SampleRateHz))
		}
		duration := fallbackActualTime(stamp.DurationTicks, config.TimeBase)
		validDuration := duration.Cmp(wantDuration) == 0
		if config.Track == media.TrackAudio && liveEpochs[config.SourceEpoch] && profile.EffectiveLiveAudioMode() == core.LiveAudioCopy {
			// Ожидание берётся из соседних принятых DTS; последний AU может сохранить исходную длительность из ASC.
			validDuration = profile.EffectiveLiveAudioMode() == core.LiveAudioCopy && index == lastAudio[config.SourceEpoch] && validDuration
			for _, accepted := range sourceDurations[sha256.Sum256(packet.Payload())] {
				validDuration = validDuration || duration.Cmp(accepted) == 0
			}
		}
		if config.Track == media.TrackAudio && liveEpochs[config.SourceEpoch] && profile.EffectiveLiveAudioMode() == core.LiveAudioTranscode {
			var remaining []referencePosition
			match := func(owner uint64, index int, offset *big.Rat) {
				stream := referenceStreams[owner]
				if index >= len(stream) {
					return
				}
				expected := stream[index]
				base := expected.Config().Info().TimeBase
				if duration.Cmp(fallbackActualTime(expected.Info().DurationTicks, base)) != 0 {
					return
				}
				shift := new(big.Rat).Sub(dts, fallbackActualTime(expected.Info().DTS, base))
				if offset == nil || shift.Cmp(offset) == 0 {
					remaining = append(remaining, referencePosition{owner, index, shift})
				}
			}
			if prior, exists := nativePositions[config.SourceEpoch]; exists {
				for _, position := range prior {
					match(position.owner, position.index+1, position.offset)
				}
			} else {
				for owner, stream := range referenceStreams {
					for index := range stream {
						match(owner, index, nil)
					}
				}
			}
			validDuration = len(remaining) > 0
			nativePositions[config.SourceEpoch] = remaining
		}
		if stamp.PTS != stamp.DTS || !validDuration {
			t.Fatalf("Изменена независимая ожидаемая временная структура %s: %+v epoch=%d live=%v", config.Track, stamp, config.SourceEpoch, liveEpochs[config.SourceEpoch])
		}
		current := segments[config.SourceEpoch]
		if current == nil {
			current = &segment{}
			segments[config.SourceEpoch] = current
		}
		if config.Track == media.TrackVideo && current.video == nil {
			current.video = dts
		}
		if config.Track == media.TrackAudio && current.audio == nil {
			current.audio = dts
		}
		if config.Track != media.TrackAudio || profile.EffectiveLiveAudioMode() != core.LiveAudioCopy {
			continue
		}
		times := sourceTimes[sha256.Sum256(packet.Payload())]
		if len(times) == 0 {
			continue
		}
		var candidates []*big.Rat
		for _, original := range times {
			shift := new(big.Rat).Sub(dts, original)
			previous, exists := shifts[config.SourceEpoch]
			if !exists {
				candidates = append(candidates, shift)
				continue
			}
			for _, allowed := range previous {
				if shift.Cmp(allowed) == 0 {
					candidates = append(candidates, shift)
					break
				}
			}
		}
		if len(candidates) == 0 {
			t.Fatalf("LIVE AAC потерял единый сдвиг независимой ожидаемой шкалы: mode=%s epoch=%d", profile.EffectiveLiveAudioMode(), config.SourceEpoch)
		}
		shifts[config.SourceEpoch] = candidates
	}
	for epoch, current := range segments {
		if current.video != nil && current.audio != nil && new(big.Rat).Abs(new(big.Rat).Sub(current.video, current.audio)).Cmp(big.NewRat(1, 10)) > 0 {
			t.Fatalf("Начало сегмента рассогласовало A/V: epoch=%d video=%s audio=%s", epoch, current.video, current.audio)
		}
	}
}

func liveAudioAssertMixedSilence(t *testing.T, executable string, messages []publisherWireMessage, quiet map[[32]byte]bool, channels, sampleRate int) {
	t.Helper()
	pcm, payloads := liveAudioWirePCM(t, executable, messages, channels)
	liveAudioAssertTone(t, pcm, payloads, quiet, channels, sampleRate)
	var asc []byte
	for _, m := range messages {
		if m.typ == 8 && len(m.body) > 2 && m.body[1] == 0 {
			asc = m.body[2:]
			break
		}
	}
	blockBytes := 1024 * channels * 4
	longRuns, liveBlocks := 0, 0
	for start := 0; start < len(payloads); {
		if !quiet[sha256.Sum256(payloads[start])] {
			liveBlocks++
			start++
			continue
		}
		end := start
		for end < len(payloads) && quiet[sha256.Sum256(payloads[end])] {
			end++
		}
		if end-start < 8 {
			start = end
			continue
		}
		longRuns++
		actual := pcm[start*blockBytes : end*blockBytes]
		if start == 0 {
			if liveAudioNonzeroSamples(actual) != 0 {
				t.Fatal("Начальный fallback содержит ненулевой PCM")
			}
		} else {
			// Длительность остатка измеряется на той же фикстуре и декодере; универсальный предел не задаётся.
			prefix := max(0, start-2)
			var control []byte
			for _, payload := range payloads[prefix:end] {
				control = append(control, liveAudioADTS(t, asc, payload)...)
			}
			decoded := mediaCommand(t, executable, control, "-v", "error", "-xerror", "-flags2:a", "+skip_manual", "-f", "aac", "-i", "pipe:0", "-map", "0:a:0", "-f", "f32le", "pipe:1")
			decoded = decoded[(start-prefix)*blockBytes:]
			lastControl := -1
			for i := 0; i < len(decoded); i += 4 {
				if liveAudioNonzeroSamples(decoded[i:i+4]) != 0 {
					lastControl = i / 4
				}
			}
			lastActual := -1
			for i := 0; i < len(actual); i += 4 {
				if liveAudioNonzeroSamples(actual[i:i+4]) != 0 {
					lastActual = i / 4
				}
			}
			if lastControl >= 2*1024*channels || lastActual > lastControl+1024*channels {
				t.Fatalf("PCM-звук продолжается после измеренного остатка декодера: actual=%d control=%d run=%d AU", lastActual, lastControl, end-start)
			}
			quietFrom := (max(lastControl, lastActual) + 1) * 4
			if len(actual)-quietFrom < 4*blockBytes || liveAudioNonzeroSamples(actual[quietFrom:]) != 0 {
				t.Fatal("Нет устойчивой цифровой тишины после перехода")
			}
			t.Logf("Измеренный остаток LIVE: actual<=%g ms, control<=%g ms, тихий участок=%d AU", float64(lastActual+1)*1000/float64(channels*sampleRate), float64(lastControl+1)*1000/float64(channels*sampleRate), end-start)
		}
		start = end
	}
	if longRuns < 2 || liveBlocks < 12 {
		t.Fatalf("Не проверены LIVE и длинные тихие участки: fallback=%d live=%d", longRuns, liveBlocks)
	}
}

func TestLiveAudioCPUSharedGroupsPublisherTransitionsAndMismatch(t *testing.T) {
	executable := os.Getenv("GOST_MEDIA_FFMPEG")
	if executable == "" {
		t.Skip("Локальная CPU-проверка требует GOST_MEDIA_FFMPEG")
	}
	base := fallbackPipelineProfile(core.CodecH264)
	base.LiveAudioMode = core.LiveAudioCopy
	base.VideoEncoding.GOPFrames = 6
	base.Encoding = core.DefaultEncoding(base.Mode, "h264_nvenc", "vbr")
	base.Encoding.AudioCustom = "aac_coder=fast"
	base.AudioEncoding.BitrateBPS = 128000
	smaller := base
	smaller.Format.Video.Width, smaller.Format.Video.Height = 64, 64
	smaller.Format.Video.FrameRate = core.FrameRate{Numerator: 15, Denominator: 1}
	smaller.AudioEncoding.BitrateBPS = 96000
	transcode := base
	transcode.LiveAudioMode = core.LiveAudioTranscode
	transcode.Format.Audio.SampleRateHz, transcode.Format.Audio.Channels, transcode.Format.Audio.ChannelLayout = 24000, 1, "mono"
	transcode.AudioEncoding.BitrateBPS = 32000
	mismatch := base
	mismatch.Format.Audio.SampleRateHz = 32000
	profiles := []core.GroupProfile{base, smaller, transcode, mismatch}
	var prepared [][]byte
	var quiet []map[[32]byte]bool
	for _, p := range profiles {
		data := liveAudioPrepared(t, executable, p, "image")
		prepared = append(prepared, data)
		packets, err := encodingPreparedPackets(data, p.Format)
		if err != nil {
			t.Fatal(err)
		}
		quiet = append(quiet, liveAudioKnown(packets[media.TrackAudio]))
	}
	var receivers []*publisherReceiver
	var endpoints []string
	for range 5 {
		r, _ := newPublisherReceiver(t, false)
		receivers = append(receivers, r)
		endpoints = append(endpoints, "rtmp://"+r.listener.Addr().String()+"/audio")
	}
	groupIndex := []int{0, 0, 1, 2, 3}
	f := newPublisherPipeline(t, profiles, endpoints, []string{"shared-a", "shared-b", "smaller", "transcode", "mismatch"}, groupIndex)
	f.config.Binary = executable
	diagnostic := boundedMediaBuffer{max: 4 << 20}
	f.config.Logger = slog.New(slog.NewJSONHandler(&diagnostic, nil))
	paths := make([]string, len(profiles))
	for i, data := range prepared {
		paths[i] = filepath.Join(t.TempDir(), "prepared.mp4")
		if err := os.WriteFile(paths[i], data, 0600); err != nil {
			t.Fatal(err)
		}
	}
	f.config.OpenFile = func(_ context.Context, m core.ArtifactManifest) (*os.File, error) {
		for i, p := range profiles {
			if p.Equal(m.Profile) {
				return os.Open(paths[i])
			}
		}
		return nil, core.ErrCorrupt
	}
	var workerMu sync.Mutex
	workers := map[int]int{}
	var commands [][]string
	f.config.Transcode = func(_ context.Context, c ffmpeg.NVIDIAConfig, a ffmpeg.Attempt) (ffmpeg.Command, error) {
		command, err := liveAudioCPUCommand(c, a)
		workerMu.Lock()
		defer workerMu.Unlock()
		for i, p := range profiles {
			if p.Equal(c.Profile) {
				workers[i]++
			}
		}
		commands = append(commands, slices.Clone(command.Args))
		return command, err
	}
	registry, err := group.NewRegistry(context.Background(), group.RegistryConfig{Core: f.service, Parent: f.parent, Observer: f.observer, Runtime: f.config, MaxGroups: 4, MaxReferences: 8})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := registry.Close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	refs := make([]*group.Reference, 5)
	pubs := make([]*publisher.Publisher, 5)
	captures := map[*group.Group]*liveAudioCapture{}
	var tapStops []func()
	for _, output := range f.outputs {
		i := slices.Index(endpoints, output.Snapshot.Destination.Endpoint)
		if i < 0 {
			t.Fatal("Неизвестное тестовое назначение")
		}
		ref, err := registry.Acquire(context.Background(), output.Snapshot, f.live)
		if err != nil {
			t.Fatal(err)
		}
		refs[i] = ref
		if captures[ref.Group()] == nil {
			capture := &liveAudioCapture{}
			captures[ref.Group()] = capture
			sub, err := ref.Router().Subscribe(publisherQueue())
			if err != nil {
				t.Fatal(err)
			}
			ctx, cancel := context.WithCancel(context.Background())
			done := make(chan struct{})
			go func() {
				defer close(done)
				for {
					d, err := sub.Next(ctx)
					if err != nil {
						return
					}
					capture.mu.Lock()
					if len(capture.packets) >= 8192 {
						capture.overflow = true
						capture.mu.Unlock()
						return
					}
					capture.packets = append(capture.packets, d.Packet)
					capture.mu.Unlock()
				}
			}()
			stopTap := func() {
				cancel()
				_ = sub.Close()
				<-done
			}
			tapStops = append(tapStops, stopTap)
			t.Cleanup(func() {
				stopTap()
				capture.mu.Lock()
				defer capture.mu.Unlock()
				if capture.overflow {
					t.Error("Превышен бюджет захвата AU")
				}
			})
		}
		pubs[i] = publisherActivate(t, f, output, ref.Router(), nil)
	}
	if len(captures) != 4 || refs[0].Group() != refs[1].Group() {
		t.Fatal("Назначения не разделяют одну обработку группы")
	}
	t.Cleanup(func() {
		if !t.Failed() {
			return
		}
		for i, ref := range refs {
			t.Logf("Группа/назначение %d перед остановкой: status=%+v publisher=%+v", i, ref.Group().Status(), pubs[i].Snapshot())
		}
		if err := registry.Close(context.Background()); err != nil {
			t.Logf("Завершение диагностируемых групп: %v", err)
		}
		log := diagnostic.String()
		if len(log) > 32000 {
			log = log[len(log)-32000:]
		}
		t.Logf("Диагностика медиатракта: %s", log)
		workerMu.Lock()
		defer workerMu.Unlock()
		t.Logf("Число workers: %+v; команды: %q", workers, commands)
	})
	waitSource := func(indices []int, want group.Feed) {
		t.Helper()
		publisherWait(t, func() bool {
			for _, i := range indices {
				s := refs[i].Group().Status()
				if !s.Available || s.Source != want {
					return false
				}
			}
			return true
		})
	}
	waitSource([]int{0, 1, 2, 3, 4}, group.Fallback)
	for _, r := range receivers {
		r.wait(t, func(messages []publisherWireMessage) bool {
			n := 0
			for _, m := range messages {
				if m.typ == 8 && len(m.body) > 2 && m.body[1] == 1 {
					n++
				}
			}
			return n >= 155
		})
	}
	inputProfile := publisherProfile(core.CodecAV1, 128, 128)
	_, source, inputInit := publisherRealFixture(t, executable, inputProfile, "4")
	known := liveAudioKnown(source)
	for i, packet := range source {
		if packet.Config().Info().Track == media.TrackVideo && packet.Info().Keyframe {
			rebuilt, e := media.NewPacket(packet.Info(), packet.Config(), append(inputInit.Video.Headers()[4:], packet.Payload()...))
			if e != nil {
				t.Fatal(e)
			}
			source[i] = rebuilt
		}
	}
	ingress, stopIngress := liveAudioStartCapture(t, f.live)
	_, stop, done := publisherStartIngest(t, f, source, inputInit)
	defer stop()
	waitSource([]int{0, 1, 2, 3}, group.Live)
	for i := range 3 {
		receivers[i].wait(t, func(messages []publisherWireMessage) bool {
			return liveAudioMatched(messages, known, quiet[groupIndex[i]]) >= 20
		})
	}
	workerMu.Lock()
	if workers[0] != 1 || workers[1] != 1 || workers[2] != 1 || workers[3] != 0 {
		t.Errorf("Неверное число общих workers/допуск несовместимого AAC: %+v", workers)
	}
	workerMu.Unlock()
	if status := refs[4].Group().Status(); !status.Available || status.Source != group.Fallback {
		t.Fatalf("Несовместимый LIVE вытеснил пригодный fallback: %+v", status)
	}
	if _, err = f.service.PauseSession(context.Background(), core.SessionCommand{RequestID: "audio-pause", SessionID: f.parent.SessionID}); err != nil {
		t.Fatal(err)
	}
	waitSource([]int{0, 1, 2, 3}, group.Fallback)
	for i := range 4 {
		before := publisherMediaCount(receivers[i].snapshot())
		receivers[i].wait(t, func(m []publisherWireMessage) bool { return publisherMediaCount(m) > before+18 })
	}
	if _, err = f.service.ResumeSession(context.Background(), core.SessionCommand{RequestID: "audio-resume", SessionID: f.parent.SessionID}); err != nil {
		t.Fatal(err)
	}
	waitSource([]int{0, 1, 2, 3}, group.Live)
	previous := refs[0].Group().Status().Epoch
	_, overrideStop, overrideDone := publisherStartIngest(t, f, source, inputInit)
	defer overrideStop()
	publisherWait(t, func() bool {
		s := refs[0].Group().Status()
		return s.Available && s.Source == group.Live && s.Epoch > previous
	})
	stop()
	<-done
	waitSource([]int{0, 1, 2, 3}, group.Live)
	overrideStop()
	<-overrideDone
	waitSource([]int{0, 1, 2, 3, 4}, group.Fallback)
	for _, r := range receivers {
		before := publisherMediaCount(r.snapshot())
		r.wait(t, func(m []publisherWireMessage) bool { return publisherMediaCount(m) > before+30 })
	}
	for _, p := range pubs {
		if err = p.Stop(context.Background()); err != nil {
			t.Fatal(err)
		}
	}
	for _, r := range receivers {
		r.waitClosed(t)
		if r.accepted.Load() != 1 {
			t.Fatalf("Переходы переподключили исправный выход: %d", r.accepted.Load())
		}
	}
	// Лог читается после остановки групп, чтобы не конкурировать с записью обработчика.
	if err = registry.Close(context.Background()); err != nil {
		t.Fatal(err)
	}
	for _, stopTap := range tapStops {
		stopTap()
	}
	stopIngress()
	ingressPackets := ingress.snapshot()
	if len(ingressPackets) == 0 {
		t.Fatal("Не захвачен фактически принятый ingress")
	}
	liveAudioDumpPackets(t, "accepted-ingress", ingressPackets)
	liveAudioDumpPackets(t, "explicit-transcode-group", captures[refs[3].Group()].snapshot())
	reference := liveAudioTranscodeReference(t, executable, ingressPackets, transcode)
	if !strings.Contains(diagnostic.String(), "LIVE AAC copy") {
		t.Fatal("Нет понятной диагностики несовместимого LIVE при исправном fallback")
	}
	workerMu.Lock()
	savedCommands := slices.Clone(commands)
	workerMu.Unlock()
	explicitSettings := false
	for _, args := range savedCommands {
		joined := strings.Join(args, " ")
		if strings.Contains(joined, "-ar:a 24000") {
			explicitSettings = strings.Contains(joined, "-b:a 32000") && strings.Contains(joined, "-ac:a 1") && strings.Contains(joined, "-aac_coder fast")
		}
	}
	if !explicitSettings {
		t.Fatal("Явные настройки AAC не достигли рабочего процесса")
	}
	for i, r := range receivers {
		messages := r.snapshot()
		groupPackets := captures[refs[i].Group()].snapshot()
		publisherAssertMappedTimeline(t, messages, groupPackets)
		p := profiles[groupIndex[i]]
		liveAudioAssertGroupTiming(t, groupPackets, p, ingressPackets, reference, quiet[groupIndex[i]])
		probe := probeMedia(t, filepath.Join(filepath.Dir(executable), "ffprobe"), publisherFLV(messages))
		seenVideo, seenAudio := false, false
		for _, s := range probe.Streams {
			if s.CodecType == "video" {
				seenVideo = true
				if s.CodecName != "h264" || s.Width != p.Format.Video.Width || s.Height != p.Format.Video.Height {
					t.Fatalf("Неверное преобразование видео: %+v", s)
				}
			}
			if s.CodecType == "audio" {
				seenAudio = true
				if s.SampleRate != fmt.Sprint(p.Format.Audio.SampleRateHz) || s.Channels != p.Format.Audio.Channels {
					t.Fatalf("Неверный формат AAC: %+v", s)
				}
			}
		}
		if !seenVideo || !seenAudio {
			t.Fatal("Получатель не увидел обе дорожки")
		}
		if i < 3 {
			for _, m := range messages {
				if m.typ == 8 && len(m.body) > 2 && m.body[1] == 1 {
					hash := sha256.Sum256(m.body[2:])
					if !quiet[groupIndex[i]][hash] && !known[hash] {
						t.Fatalf("Publisher изменил AAC payload: %x", hash)
					}
				}
			}
			liveAudioAssertMixedSilence(t, executable, messages, quiet[groupIndex[i]], 2, int(p.Format.Audio.SampleRateHz))
		} else if i == 3 {
			liveAudioAssertMixedSilence(t, executable, messages, quiet[groupIndex[i]], 1, int(p.Format.Audio.SampleRateHz))
			if liveAudioMatched(messages, known, quiet[groupIndex[i]]) > 0 {
				t.Fatal("Явный transcode сохранил входные AAC AU вместо преобразования формата")
			}
		} else {
			pcm, _ := liveAudioWirePCM(t, executable, messages, 2)
			if liveAudioNonzeroSamples(pcm) != 0 {
				t.Fatal("Несовместимая группа передала звук вместо тишины fallback")
			}
		}
	}
}
