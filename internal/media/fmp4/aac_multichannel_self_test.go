package fmp4

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
)

// TestNativeAAC71NormalizationBeforeContainerValidation проверяет, что исправление заголовков
// выполняется до строгой сверки собственных метаданных; ложный stereo ASC этой сверкой отвергается.
func TestNativeAAC71NormalizationBeforeContainerValidation(t *testing.T) {
	// init содержит обычное тестовое видео и нормативную AAC 7.1 конфигурацию.
	init := testInit(t)
	// info задаёт точную 7.1 карту при сохранении исходной эпохи и временной шкалы.
	info := init.Audio.Info()
	info.Audio.Channels, info.Audio.ChannelLayout = 8, "7.1"
	// err сообщает об отказе создания нормативной конфигурации и последующей упаковки.
	var err error
	init.Audio, err = media.NewCodecConfig(info, []byte{0x11, 0xe0, 0x56, 0xe5, 0})
	if err != nil {
		t.Fatal(err)
	}
	// output содержит валидную инициализацию с собственными метаданными 7.1.
	var output bytes.Buffer
	if _, err = NewMuxer(&output, init, testLimits()); err != nil {
		t.Fatal(err)
	}
	// wire заменяет только ASC config12 на известный native config7, сохраняя собственные метаданные.
	wire := bytes.Replace(output.Bytes(), []byte{0x11, 0xe0, 0x56, 0xe5, 0}, []byte{0x11, 0xb8, 0x56, 0xe5, 0}, 1)
	if _, err = NewDemuxer(bytes.NewReader(wire), DemuxOptions{Limits: testLimits()}); !errors.Is(err, ErrInvalid) {
		t.Fatalf("принято противоречие ASC и метаданных: %v", err)
	}
	// options разрешает только явную коррекцию AAC заголовков перед общей проверкой.
	options := DemuxOptions{Limits: testLimits(), NormalizeHeaders: func(desc TrackDescription) ([]byte, error) {
		// Callback исправляет известный свежий AAC ASC; заголовки видео возвращает неизменными.
		if desc.Track == media.TrackAudio {
			return codec.NormalizeNativeAAC71ASC(desc.Headers)
		}
		return desc.Headers, nil
	}}
	// restored подтверждает исправленный формат только после полной строгой сверки контейнера.
	restored, err := NewDemuxer(bytes.NewReader(wire), options)
	if err != nil || restored.Init().Audio.Info().Audio.ChannelLayout != "7.1" || !bytes.Equal(restored.Init().Audio.Headers(), init.Audio.Headers()) {
		t.Fatal("нормализация не предшествует строгой проверке", err)
	}
	options.NormalizeHeaders = func(desc TrackDescription) ([]byte, error) {
		// Callback намеренно возвращает stereo ASC, противоречащий channels=8 в контейнере.
		if desc.Track == media.TrackAudio {
			return []byte{0x11, 0x90}, nil
		}
		return desc.Headers, nil
	}
	if _, err = NewDemuxer(bytes.NewReader(wire), options); !errors.Is(err, ErrInvalid) {
		t.Fatalf("hook ослабил строгую проверку: %v", err)
	}
}

// TestFFmpegAAC71HeaderCorrectionPreservesPackets проверяет реальный native AAC 7.1:
// обычное чтение оставляет config7/wide-back, явно разрешённая коррекция меняет только ASC,
// все AU/PTS/DTS/duration совпадают, а config12 после упаковки проходит строгий декодер.
func TestFFmpegAAC71HeaderCorrectionPreservesPackets(t *testing.T) {
	// ffmpeg задаёт явно разрешённый CPU бинарник; обычные unit tests не запускают процесс.
	ffmpeg := os.Getenv("GOST_FMP4_FFMPEG")
	if ffmpeg == "" {
		t.Skip("локальная CPU-интеграция не включена")
	}
	// source содержит настоящий результат native AAC с восемью различимыми тонами и H.264 видео.
	source := runMediaCommand(t, ffmpeg, nil, "-v", "error", "-f", "lavfi", "-i", "testsrc2=s=128x128:r=30:d=0.2", "-f", "lavfi", "-i", "aevalsrc=0.05*sin(2*PI*173*t)|0.05*sin(2*PI*300*t)|0.05*sin(2*PI*427*t)|0.05*sin(2*PI*554*t)|0.05*sin(2*PI*681*t)|0.05*sin(2*PI*808*t)|0.05*sin(2*PI*935*t)|0.05*sin(2*PI*1062*t):s=48000:d=0.2:c=7.1", "-map", "0:v", "-map", "1:a", "-c:v", "libx264", "-threads:v", "1", "-bf", "0", "-c:a", "aac", "-aac_pce", "0", "-f", "mp4", "-movflags", "empty_moov+default_base_moof+frag_every_frame", "pipe:1")
	// options выбирает штатное восстановление фактического формата, не подставляя desired.
	options := DemuxOptions{Limits: testLimits(), SourceEpoch: 1, VideoRevision: 1, AudioRevision: 1}
	// raw и err сохраняют обычный путь чтения config7 без коррекции.
	raw, err := NewDemuxer(bytes.NewReader(source), options)
	if err != nil || raw.Init().Audio.Info().Audio.ChannelLayout != "7.1(wide-back)" {
		t.Fatal("обычное чтение подменило config7", err)
	}
	options.NormalizeHeaders = func(desc TrackDescription) ([]byte, error) {
		// Callback применяется лишь к созданному выше native AAC результату этого теста.
		if desc.Track == media.TrackAudio {
			return codec.NormalizeNativeAAC71ASC(desc.Headers)
		}
		return desc.Headers, nil
	}
	// normalized восстанавливает формат после исправления собственного свежего заголовка.
	normalized, err := NewDemuxer(bytes.NewReader(source), options)
	if err != nil || normalized.Init().Audio.Info().Audio.ChannelLayout != "7.1" {
		t.Fatal("native AAC не нормализован", err)
	}
	// output и mux собирают нормативный config12 транспорт без изменения сжатых данных.
	var output bytes.Buffer
	mux, err := NewMuxer(&output, normalized.Init(), testLimits())
	if err != nil {
		t.Fatal(err)
	}
	for {
		// original и rawErr сохраняют AU и метки из обычного чтения исходного файла.
		original, rawErr := raw.Next()
		// actual и readErr содержат соответствующий пакет после явной коррекции ASC.
		actual, readErr := normalized.Next()
		if rawErr == io.EOF && readErr == io.EOF {
			break
		}
		if rawErr != nil || readErr != nil || original.Info() != actual.Info() || !bytes.Equal(original.Payload(), actual.Payload()) {
			t.Fatalf("изменены AU или время: %v, %v", rawErr, readErr)
		}
		if err = mux.WritePacket(actual); err != nil {
			t.Fatal(err)
		}
	}
	// pcm содержит реальные восемь каналов строгого декодера; их порядок проверяется отдельным oracle.
	pcm := runMediaCommand(t, ffmpeg, output.Bytes(), "-v", "error", "-xerror", "-strict", "2", "-i", "pipe:0", "-map", "0:a", "-f", "f32le", "pipe:1")
	if len(pcm) < 8*1024*4 {
		t.Fatal("строгий декодер не вернул восемь каналов")
	}
}

// TestFFmpegAACMultichannelCopy при явном GOST_FMP4_FFMPEG проводит различимые каналы
// через FFmpeg → штатный Demuxer → Inspector → Muxer → штатный Demuxer → строгий декодер.
// Сравнение исходного и возвращённого PCM проверяет сохранность всех сэмплов и порядка каналов.
func TestFFmpegAACMultichannelCopy(t *testing.T) {
	// ffmpeg задаёт разрешённый бинарник локальной CPU-проверки.
	ffmpeg := os.Getenv("GOST_FMP4_FFMPEG")
	if ffmpeg == "" {
		t.Skip("локальная CPU-интеграция не включена")
	}
	// tc задаёт независимо выбранные раскладку и число генерируемых каналов.
	for _, tc := range []struct {
		// layout задаёт схему anullsrc/aevalsrc, channels — число различных тонов.
		layout   string
		channels int
	}{{"2.1", 3}, {"3.0", 3}, {"4.0", 4}, {"5.0", 5}, {"5.1", 6}} {
		// Подтест проводит выбранную раскладку через настоящий кодировщик и штатный медиапорт.
		t.Run(tc.layout, func(t *testing.T) {
			// tones хранит отдельную частоту для каждого входного канала.
			tones := make([]string, tc.channels)
			// channel задаёт индекс канала и его уникальную частоту в исходном PCM.
			for channel := range tones {
				tones[channel] = fmt.Sprintf("0.05*sin(2*PI*%d*t)", 173+channel*127)
			}
			// source содержит настоящий AAC-LC и короткое H.264-видео с VUI timing.
			source := runMediaCommand(t, ffmpeg, nil, "-v", "error", "-f", "lavfi", "-i", "testsrc2=s=128x128:r=30:d=0.2", "-f", "lavfi", "-i", "aevalsrc="+strings.Join(tones, "|")+":s=48000:d=0.2:c="+tc.layout, "-map", "0:v", "-map", "1:a", "-c:v", "libx264", "-threads:v", "1", "-bf", "0", "-c:a", "aac", "-f", "mp4", "-movflags", "empty_moov+default_base_moof+frag_every_frame", "pipe:1")
			// demux использует штатное восстановление ASC, без desired и без подстановки ffprobe;
			// err сообщает об отказе создания и последующих операций контейнерного обмена в этом подтесте.
			demux, err := NewDemuxer(bytes.NewReader(source), DemuxOptions{Limits: testLimits(), SourceEpoch: 1, VideoRevision: 1, AudioRevision: 1})
			if err != nil {
				t.Fatal(err)
			}
			if demux.Init().Audio.Info().Audio.ChannelLayout != tc.layout || demux.Init().Audio.Info().Audio.Channels != uint16(tc.channels) {
				t.Fatal("ASC не восстановил фактическую раскладку", demux.Init().Audio.Info())
			}
			// output принимает повторно упакованный транспорт, mux пишет его без перекодирования.
			var output bytes.Buffer
			mux, err := NewMuxer(&output, demux.Init(), testLimits())
			if err != nil {
				t.Fatal(err)
			}
			// inspector проверяет штатный AAC-префикс каждого пакета перед его копированием.
			var inspector codec.Inspector
			// packets сохраняет исходные пакеты для точной сверки всех AU и временных меток.
			var packets []media.Packet
			for {
				// packet содержит очередной исходный AU; readErr отличает EOF от повреждения.
				packet, readErr := demux.Next()
				if readErr == io.EOF {
					break
				}
				if readErr != nil {
					t.Fatal(readErr)
				}
				if packet.Config().Info().Track == media.TrackAudio {
					if _, err = inspector.Inspect(packet); err != nil {
						t.Fatal(err)
					}
				}
				if err = mux.WritePacket(packet); err != nil {
					t.Fatal(err)
				}
				packets = append(packets, packet)
			}
			// returned восстанавливает собственные метаданные и проверяет их против сохранённого ASC.
			returned, err := NewDemuxer(bytes.NewReader(output.Bytes()), DemuxOptions{Limits: testLimits()})
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(returned.Init().Audio.Headers(), demux.Init().Audio.Headers()) {
				t.Fatal("изменены исходные ASC/PCE")
			}
			// expected содержит исходный пакет в том же порядке контейнерного транспорта.
			for _, expected := range packets {
				// actual содержит возвращённые байты AU и исходные рациональные метки; readErr сообщает об отказе чтения.
				actual, readErr := returned.Next()
				if readErr != nil || actual.Info() != expected.Info() || !bytes.Equal(actual.Payload(), expected.Payload()) {
					t.Fatalf("изменён AU или его время: %v", readErr)
				}
			}
			// originalPCM и copiedPCM содержат результат одного строгого декодера до и после медиапорта.
			originalPCM := runMediaCommand(t, ffmpeg, source, "-v", "error", "-xerror", "-strict", "2", "-i", "pipe:0", "-map", "0:a", "-f", "f32le", "pipe:1")
			copiedPCM := runMediaCommand(t, ffmpeg, output.Bytes(), "-v", "error", "-xerror", "-strict", "2", "-i", "pipe:0", "-map", "0:a", "-f", "f32le", "pipe:1")
			if len(originalPCM) < tc.channels*1024*4 || !bytes.Equal(originalPCM, copiedPCM) {
				t.Fatalf("изменены PCM-каналы: %d → %d байт", len(originalPCM), len(copiedPCM))
			}
		})
	}
}
