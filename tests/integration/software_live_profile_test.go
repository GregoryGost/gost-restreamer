package integration

import (
	"bytes"
	"context"
	"errors"
	"os"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
	"github.com/GregoryGost/gost-restreamer/internal/media/group"
)

// TestSoftwareLiveProfileCPU проверяет рабочий LIVE builder через Supervisor/UDS без
// тестовой подмены кодировщика или фильтра: AV1→x264 high44410 с преобразованием цвета/FPS/размеров.
// AAC copy должен сохранить каждый payload и его время; результат проходит Inspector,
// настоящее декодирование и переключения того же полного профиля.
// t сообщает об ошибках проверяемого сценария.
func TestSoftwareLiveProfileCPU(t *testing.T) {
	// binary разрешает локальную CPU-проверку; format задаёт независимый фактический AV1-вход.
	binary := os.Getenv("GOST_MEDIA_FFMPEG")
	if binary == "" {
		t.Skip("CPU-проверка требует GOST_MEDIA_FFMPEG")
	}
	format := fallbackPipelineProfile(core.CodecAV1).Format
	// wire содержит независимый синтетический источник с явно заданными свойствами кадров.
	wire := mediaCommand(t, binary, nil, "-v", "error", "-xerror", "-f", "lavfi", "-i", "testsrc2=s=128x128:r=30:d=1.6", "-f", "lavfi", "-i", "sine=frequency=750:sample_rate=48000:duration=1.6", "-map", "0:v", "-map", "1:a", "-vf", "setparams=range=limited:color_primaries=bt709:color_trc=bt709:colorspace=bt709", "-c:v", "libaom-av1", "-threads:v", "1", "-cpu-used", "8", "-lag-in-frames", "0", "-g:v", "6", "-crf", "45", "-c:a", "aac", "-ac:a", "2", "-b:a", "64000", "-f", "mp4", "-movflags", "delay_moov+default_base_moof+frag_every_frame", "-min_frag_duration", "1", "-use_editlist", "1", "pipe:1")
	// demux восстанавливает AAC по ASC и связывает AV1 с независимо известной частотой.
	// err сообщает об отказе разбора исходного контейнера.
	// desc содержит фактические заголовки и шкалу дорожки независимого источника fMP4.
	demux, err := fmp4.NewDemuxer(bytes.NewReader(wire), fmp4.DemuxOptions{Limits: integrationLimits(), SourceEpoch: 1, VideoRevision: 1, AudioRevision: 1, ResolveConfig: func(desc fmp4.TrackDescription) (media.CodecConfig, error) {
		// Разбирает фактический ASC и сохраняет неизменные заголовки AV1 независимого источника.
		if desc.Track == media.TrackAudio {
			return fmp4.ResolveCodecConfig(desc, 1, 1)
		}
		return media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: 1, Revision: 1, TimeBase: desc.TimeBase, Video: format.Video}, desc.Headers)
	}})
	if err != nil {
		t.Fatal(err)
	}
	// original хранит неизменные AU; scenario задаёт положительное общее начало 1 секунду.
	original := readMediaPackets(t, demux)
	scenario := conversionScenario{inputStep: 2000, videoOrigin: 60000, audioOrigin: 48000, outputRate: core.FrameRate{Numerator: 15, Denominator: 1}, firstVideoTick: 15, endVideoTick: 39}
	// input и init содержат AU с проверенными явными сетками, без codec payload изменений.
	input, init := scheduledConversionInput(t, original, demux.Init(), scenario)
	// p задаёт настоящий новый high44410 профиль со сменой цвета, глубины и разрешения.
	p := fallbackPipelineProfile(core.CodecH264)
	p.LiveAudioMode = core.LiveAudioCopy
	p.Encoding = core.DefaultEncoding(p.Mode, core.VideoEncoderX264, core.RateControlCRF)
	p.Encoding.CRF = core.OptionalCRF{Set: true, ValueHundredths: 2357}
	p.VideoEncoding.TargetBitrateBPS, p.VideoEncoding.MaxBitrateBPS, p.VideoEncoding.BufferBits = 0, 0, 0
	p.VideoEncoding.GOPFrames, p.VideoEncoding.MaxBFrames = 6, 0
	p.Format.Video.Width, p.Format.Video.Height = 64, 64
	p.Format.Video.FrameRate = scenario.outputRate
	p.Format.Video.CodecProfile, p.Format.Video.PixelFormat, p.Format.Video.BitDepth = "high444", "yuv444p10le", 10
	p.Format.Video.Color = core.ColorInfo{Range: "full", Primaries: "bt2020", Transfer: "gamma22", Matrix: "bt2020nc"}
	// builder вызывает production код с фактическими исходными конфигурациями обеих дорожек.
	// attempt задаёт транспорт и идентичность фактической попытки Supervisor.
	builder := func(_ context.Context, attempt ffmpeg.Attempt) (ffmpeg.Command, error) {
		return ffmpeg.BuildNVIDIACommand(ffmpeg.NVIDIAConfig{Binary: binary, Input: core.MediaFormat{Video: init.Video.Info().Video, Audio: init.Audio.Info().Audio}, Profile: p, VideoTimeBase: init.Video.Info().TimeBase, AudioTimeBase: init.Audio.Info().TimeBase}, attempt)
	}
	// output, actualInit и encoded принадлежат настоящей завершённой попытке Supervisor.
	output, actualInit, encoded := runConversionWorker(t, binary, input, init, scenario, builder)
	if err = core.CompatibleProfileFormat(core.MediaFormat{Video: actualInit.Video.Info().Video, Audio: actualInit.Audio.Info().Audio}, p); err != nil {
		t.Fatal("actual/desired", err)
	}
	// inspector проверяет каждый реальный AU; audio собирает исходные AAC для побайтовой сверки
	// и проверки времени.
	var inspector codec.Inspector
	var audio []media.Packet
	// packet обозначает очередную исходную AU, аудиопоследовательность остаётся неизменной.
	for _, packet := range input {
		if packet.Config().Info().Track == media.TrackAudio {
			audio = append(audio, packet)
		}
	}
	// audioIndex считает скопированные AU; videoCount подтверждает ровно 24 выходных кадра.
	audioIndex, videoCount := 0, 0
	// packet содержит очередной выходной AU для проверки Inspector и точного копирования AAC.
	for _, packet := range output {
		if _, err = inspector.Inspect(packet); err != nil {
			t.Fatal(err)
		}
		if packet.Config().Info().Track == media.TrackVideo {
			videoCount++
			continue
		}
		if audioIndex >= len(audio) || !bytes.Equal(packet.Payload(), audio[audioIndex].Payload()) || conversionTimeline(t, packet.Info().DTS, packet.Config().Info().TimeBase).Cmp(conversionTimeline(t, audio[audioIndex].Info().DTS, audio[audioIndex].Config().Info().TimeBase)) != 0 {
			t.Fatal("AAC copy изменил AU или DTS", audioIndex)
		}
		audioIndex++
	}
	if audioIndex != len(audio) || videoCount != 24 {
		t.Fatal("потеряны AU", audioIndex, len(audio), videoCount)
	}
	// decoded содержит все реальные декодированные видеокадры результата production-команды.
	decoded := mediaCommand(t, binary, encoded, "-v", "error", "-xerror", "-i", "pipe:0", "-map", "0:v:0", "-f", "framemd5", "pipe:1")
	if bytes.Count(decoded, []byte("\n0,")) != 24 {
		t.Fatal("не декодирован полный LIVE")
	}
	expandedSwitchProfile(t, p, output, actualInit)
}

// expandedSwitchProfile проверяет fallback→LIVE→fallback→LIVE на реальных AU нового профиля p.
// packets и init содержат фактический выход кодировщика; дополнительные циклы меняют только время.
// t сообщает об ошибках проверяемого сценария.
func expandedSwitchProfile(t *testing.T, p core.GroupProfile, packets []media.Packet, init fmp4.Init) {
	t.Helper()
	// switcher проверяет допуск по полному профилю с конечными бюджетами обеих дорожек.
	// err сообщает об отказе его создания или приёма AU.
	switcher, err := group.NewSwitcher(group.SwitcherConfig{Profile: p, Watchdog: media.WatchdogPolicy{VideoTimeout: time.Second, AudioTimeout: time.Second, RecoveryWindow: time.Millisecond, KeyframeTimeout: time.Second}, Queue: media.QueueLimits{MaxPackets: 512, MaxBytes: 8 << 20, MaxPacketBytes: 1 << 20, MaxConfigBytes: 1 << 16, ResyncTimeout: time.Second}, Router: media.RouterLimits{MaxConsumers: 2, MaxRetainedBytes: 16 << 20, MaxConfigBytes: 1 << 16}, MaxAVSkew: 100 * time.Millisecond, MaxTransitionGap: 100 * time.Millisecond})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { /* Закрывает принадлежащий тесту маршрутизатор группы. */
		_ = switcher.Close()
	})
	// now задаёт монотонные локальные приходы; push передаёт один полный цикл feed со сдвигом секунд.
	now := time.Unix(100, 0)
	// seconds задаёт общий сдвиг временных меток очередного цикла в целых секундах.
	push := func(feed group.Feed, seconds int64) {
		// packet содержит очередной реальный AU; stamp — его копию с общим сдвигом времени.
		for _, packet := range packets {
			// stamp сохраняет все поля AU, кроме нового положительного начала цикла.
			stamp := packet.Info()
			// base задаёт точную шкалу дорожки, shift — целый сдвиг в её единицах.
			base := packet.Config().Info().TimeBase
			shift := seconds * int64(base.Denominator) / int64(base.Numerator)
			stamp.PTS, stamp.DTS = stamp.PTS+shift, stamp.DTS+shift
			// repeated сохраняет исходные заголовки и данные при новом времени;
			// err сообщает об отказе создания или передачи полного пакета.
			repeated, err := media.NewPacket(stamp, packet.Config(), packet.Payload())
			if err != nil {
				t.Fatal(err)
			}
			now = now.Add(time.Millisecond)
			err = switcher.Push(feed, 1, repeated, init.Video, init.Audio, now)
			if err != nil && !errors.Is(err, group.ErrUnavailable) && !errors.Is(err, group.ErrFeed) {
				t.Fatal(err)
			}
		}
	}
	push(group.Fallback, 0)
	push(group.Live, 0)
	if switcher.Status().Source != group.Live || !switcher.Status().Available {
		t.Fatal("не выбран LIVE", switcher.Status())
	}
	push(group.Fallback, 2)
	if err = switcher.SetPaused(true, now.Add(time.Millisecond)); err != nil || switcher.Status().Source != group.Fallback {
		t.Fatal("не выбрана заглушка", err, switcher.Status())
	}
	now = now.Add(2 * time.Millisecond)
	push(group.Live, 2)
	if err = switcher.SetPaused(false, now.Add(time.Millisecond)); err != nil || switcher.Status().Source != group.Live {
		t.Fatal("не восстановлен LIVE", err, switcher.Status())
	}
}
