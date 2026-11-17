package integration

import (
	"bytes"
	"context"
	"encoding/binary"
	"fmt"
	"io"
	"math"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/fallback"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/GregoryGost/gost-restreamer/internal/media/encoding"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
	"github.com/GregoryGost/gost-restreamer/internal/media/group"
)

// expansionTempFile создаёт принадлежащий t файл с данными и закрывает его при очистке.
// name различает назначение файла; data задаёт исходные байты без последующих изменений.
func expansionTempFile(t *testing.T, name string, data []byte) *os.File {
	t.Helper()
	// file содержит временный файл; err сообщает об отказе создания или записи.
	file, err := os.CreateTemp(t.TempDir(), name)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { /* Закрывает дескриптор до удаления временного каталога. */
		// err сообщает об отказе закрытия принадлежащего тесту дескриптора.
		if err := file.Close(); err != nil {
			t.Error(err)
		}
	})
	if _, err = file.Write(data); err != nil {
		t.Fatal(err)
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	return file
}

// expansionPrepared готовит заглушку и отдельно проверяет нормативные заголовки собственного AAC 7.1.
// Остальные раскладки используют общий проверенный путь подготовки.
// t сообщает об ошибках подготовки; executable задаёт локальный FFmpeg, profile — полный выходной профиль.
func expansionPrepared(t *testing.T, executable string, profile core.GroupProfile) []byte {
	t.Helper()
	if profile.Format.Audio.ChannelLayout != "7.1" {
		return liveAudioPrepared(t, executable, profile, "image")
	}
	// sourceBytes и kind задают независимое PNG-изображение для реального генератора.
	sourceBytes, kind := liveAudioSourceAsset(t, executable, "image")
	// prober и runner представляют рабочие средства проверки и выполнения с конечными бюджетами.
	prober, runner := liveAudioProber(t, executable)
	// source принадлежит текущему тесту; observed содержит независимые измерения исходного PNG,
	// err сохраняет отказы проверки, генерации, нормализации и временных файлов.
	source := expansionTempFile(t, "source", sourceBytes)
	observed, err := prober.Source(context.Background(), source, kind)
	if err != nil {
		t.Fatal(err)
	}
	// period задаёт точный общий период A/V; command содержит рабочий рецепт нового профиля.
	period, err := fallback.EffectiveLoopDuration(observed.Duration, profile, prober.Limits.MaxDuration)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = source.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	command, err := encodingPipelineCPUBuilder(executable)(fallback.GenerateRequest{Source: observed, Profile: profile, LoopDuration: period})
	if err != nil {
		t.Fatal(err)
	}
	// encoded удерживает конечный свежий результат native AAC, result сообщает диагностику FFmpeg.
	encoded := boundedMediaBuffer{max: 8 << 20}
	result, err := runner.Run(context.Background(), command, source, &encoded)
	if err != nil {
		t.Fatalf("Генератор7.1: %v %s", err, result.Diagnostics)
	}
	// file передаёт только свежий собственный результат в явную generated-границу.
	file := expansionTempFile(t, "encoded", encoded.Bytes())
	// legacyOutput проверяет, что прежний API без профиля не исправляет произвольные заголовки.
	legacyOutput := boundedMediaBuffer{max: 8 << 20}
	// legacyPackets при успешной прежней нормализации сохраняет независимый эталон AU и времени.
	var legacyPackets map[media.Track][]media.Packet
	if err = prober.NormalizePrepared(context.Background(), file, &legacyOutput); err == nil {
		// legacyFormat описывает наблюдаемый config7; packets проверяет отсутствие подстановки 7.1,
		// parseErr сообщает об отказе чтения результата прежнего нормализатора.
		legacyFormat := profile.Format
		legacyFormat.Audio.ChannelLayout = "7.1(wide-back)"
		packets, parseErr := encodingPreparedPackets(legacyOutput.Bytes(), legacyFormat)
		if parseErr != nil || len(packets[media.TrackAudio]) == 0 || !bytes.Equal(packets[media.TrackAudio][0].Config().Headers(), []byte{0x11, 0xb8, 0x56, 0xe5, 0}) {
			t.Fatal("Прежний normalizer исправил untrusted ASC", parseErr)
		}
		legacyPackets = packets
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		t.Fatal(err)
	}
	// prepared получает новую нормативную сигнализацию до строгой проверки контейнера.
	prepared := boundedMediaBuffer{max: 8 << 20}
	if err = prober.NormalizePreparedForProfile(context.Background(), file, &prepared, profile); err != nil {
		t.Fatal("Generated normalizer7.1", err)
	}
	// correctedPackets проверяет фактическую нормативную сигнализацию и неизменность AU/времени.
	correctedPackets, err := encodingPreparedPackets(prepared.Bytes(), profile.Format)
	if err != nil {
		t.Fatal(err)
	}
	if len(correctedPackets[media.TrackAudio]) == 0 || !bytes.Equal(correctedPackets[media.TrackAudio][0].Config().Headers(), []byte{0x11, 0xe0, 0x56, 0xe5, 0}) {
		t.Fatal("Generated-граница не дала нормативный ASC12")
	}
	// track и previous выбирают независимо нормализованную дорожку без коррекции ASC.
	for track, previous := range legacyPackets {
		if len(previous) != len(correctedPackets[track]) {
			t.Fatal("Коррекция изменила число AU")
		}
		// index сравнивает точный исходный AU и все его временные поля до/после коррекции.
		for index := range previous {
			if !bytes.Equal(previous[index].Payload(), correctedPackets[track][index].Payload()) || previous[index].Info() != correctedPackets[track][index].Info() || previous[index].Config().Info().TimeBase != correctedPackets[track][index].Config().Info().TimeBase {
				t.Fatal("Коррекция изменила AU или временную шкалу")
			}
		}
	}
	// verified независимо проверяет полный период нового нормализованного файла.
	verified := expansionTempFile(t, "prepared", prepared.Bytes())
	if err = prober.VerifyLoopDuration(context.Background(), verified, period); err != nil {
		t.Fatal(err)
	}
	return bytes.Clone(prepared.Bytes())
}

// expansionAssertWireTime проверяет непрерывное время независимого захвата RTMP-сообщений:
// DTS каждой дорожки продвигается, разрывы и конечное рассогласование A/V не превышают 100 мс.
func expansionAssertWireTime(t *testing.T, messages []publisherWireMessage) {
	t.Helper()
	// last содержит последнее время A/V; seen отмечает начало наблюдения каждой дорожки.
	var last [2]uint32
	var seen [2]bool
	// m выбирает очередное фактически принятое сообщение RTMP.
	for _, m := range messages {
		// kind обозначает AAC AU; index выбирает видео=0 или аудио=1.
		kind, _ := expansionAudioMessage(m)
		index := 0
		if m.typ == 8 && kind == 1 {
			index = 1
		} else if m.typ != 9 || len(m.body) < 5 || m.body[0]&15 != 1 {
			continue
		}
		if seen[index] && (m.timestamp <= last[index] || m.timestamp-last[index] > 100) {
			t.Fatalf("Разрыв/сброс wire DTS track%d: %d→%d", index, last[index], m.timestamp)
		}
		last[index], seen[index] = m.timestamp, true
	}
	if !seen[0] || !seen[1] || math.Abs(float64(last[0])-float64(last[1])) > 100 {
		t.Fatalf("A/V-skew: %v", last)
	}
}

// expansionAudioMessage читает AAC из legacy либо Enhanced RTMP FourCC mp4a.
// m содержит независимое захваченное сообщение; kind различает sequence-start=0 и coded-frames=1.
// payload возвращает исходные байты без повторной сериализации ASC.
func expansionAudioMessage(m publisherWireMessage) (kind byte, payload []byte) {
	if m.typ != 8 || len(m.body) < 3 {
		return 255, nil
	}
	if m.body[0]>>4 == 10 {
		return m.body[1], m.body[2:]
	}
	if m.body[0]>>4 == 9 && len(m.body) > 5 && string(m.body[1:5]) == "mp4a" {
		return m.body[0] & 15, m.body[5:]
	}
	return 255, nil
}

// expansionDecodeFLV передаёт FFmpeg 7.1 AAC из Enhanced RTMP в знакомом ему legacy
// FLV-обрамлении. ASC, AU и временные метки сохраняются побайтно; этот адаптер не доказывает
// поддержку Enhanced Audio самим FFmpeg. messages содержит независимый захват RTMP-сообщений.
func expansionDecodeFLV(messages []publisherWireMessage) []byte {
	// adapted содержит самостоятельный список сообщений только для независимого декодера.
	adapted := append([]publisherWireMessage(nil), messages...)
	// i адресует сообщение, m содержит его исходное обрамление.
	for i, m := range adapted {
		// kind и payload восстанавливают точные медиабайты для нового FLV-префикса.
		kind, payload := expansionAudioMessage(m)
		if kind == 0 || kind == 1 {
			adapted[i].body = append([]byte{0xaf, kind}, payload...)
		}
	}
	return publisherFLV(adapted)
}

// expansionProfile создаёт явный CRF35 AV1-профиль с нужной канонической AAC-раскладкой.
// layout задаёт каноническую раскладку AAC, channels — согласованное с ней число каналов.
func expansionProfile(layout string, channels uint16) core.GroupProfile {
	// p содержит независимый профиль с точным запросом качества и аудиоформата.
	p := fallbackPipelineProfile(core.CodecAV1)
	p.LiveAudioMode = core.LiveAudioCopy
	p.Encoding = core.DefaultEncoding(p.Mode, core.VideoEncoderSVTAV1, core.RateControlCRF)
	p.Encoding.CRF = core.OptionalCRF{Set: true, ValueHundredths: 3500}
	p.Encoding.VideoCustom = "lp=1"
	p.Encoding.SVTAV1.Preset = 12
	p.VideoEncoding.TargetBitrateBPS, p.VideoEncoding.MaxBitrateBPS, p.VideoEncoding.BufferBits = 0, 0, 0
	p.Format.Audio.ChannelLayout, p.Format.Audio.Channels = layout, channels
	p.AudioEncoding.BitrateBPS = uint64(channels) * 64000
	return p
}

// expansionTones задаёт различимые частоты в порядке каналов PCM; LFE получает 60 Гц.
func expansionTones(layout string, channels uint16) []int {
	// frequencies содержит по одной независимой частоте на каждый канал.
	frequencies := make([]int, channels)
	// i выбирает очередной канал в стандартном порядке FFmpeg.
	for i := range frequencies {
		frequencies[i] = 300 + i*100
	}
	if layout == "2.1" {
		frequencies[2] = 60
	}
	if layout == "5.1" {
		frequencies[3] = 60
	}
	if layout == "7.1" {
		frequencies[3] = 60
	}
	return frequencies
}

// expansionWave записывает в отдельный WAV те же математические сигналы, что задаёт
// aevalsrc, с частотой 48 кГц, длительностью 1,6 с и точным порядком динамиков layout.
// t владеет файлом до конца теста; frequencies задаёт частоты поканальных синусоид.
func expansionWave(t *testing.T, layout string, frequencies []int) string {
	t.Helper()
	// masks задаёт стандартные WAVEFORMATEXTENSIBLE маски в порядке каналов PCM.
	masks := map[string]uint32{"2.1": 0xb, "3.0": 0x7, "4.0": 0x107, "5.0": 0x37, "5.1": 0x3f, "7.1": 0x63f}
	// channels задаёт число каналов, count — число отсчётов одного канала за 1,6 с.
	channels, count := len(frequencies), 76800
	// data содержит RIFF/WAVE, fmt с IEEE_FLOAT64 и сами чередующиеся отсчёты.
	data := make([]byte, 68+count*channels*8)
	copy(data, "RIFF")
	binary.LittleEndian.PutUint32(data[4:], uint32(len(data)-8))
	copy(data[8:], "WAVEfmt ")
	binary.LittleEndian.PutUint32(data[16:], 40)
	binary.LittleEndian.PutUint16(data[20:], 0xfffe)
	binary.LittleEndian.PutUint16(data[22:], uint16(channels))
	binary.LittleEndian.PutUint32(data[24:], 48000)
	binary.LittleEndian.PutUint32(data[28:], uint32(48000*channels*8))
	binary.LittleEndian.PutUint16(data[32:], uint16(channels*8))
	binary.LittleEndian.PutUint16(data[34:], 64)
	binary.LittleEndian.PutUint16(data[36:], 22)
	binary.LittleEndian.PutUint16(data[38:], 64)
	binary.LittleEndian.PutUint32(data[40:], masks[layout])
	copy(data[44:], []byte{3, 0, 0, 0, 0, 0, 0x10, 0, 0x80, 0, 0, 0xaa, 0, 0x38, 0x9b, 0x71})
	copy(data[60:], "data")
	binary.LittleEndian.PutUint32(data[64:], uint32(count*channels*8))
	// sample выбирает момент сигнала; channel и frequency задают динамик и его исходный тон.
	for sample := range count {
		for channel, frequency := range frequencies {
			binary.LittleEndian.PutUint64(data[68+(sample*channels+channel)*8:], math.Float64bits(0.1*math.Sin(2*math.Pi*float64(frequency)*float64(sample)/48000)))
		}
	}
	// path адресует только файл, принадлежащий текущему тесту; err сообщает об отказе записи.
	path := filepath.Join(t.TempDir(), "channels.wav")
	if err := os.WriteFile(path, data, 0600); err != nil {
		t.Fatal(err)
	}
	return path
}

// expansionNormative71Fixture заменяет только известный ASC собственного тестового
// native AAC результата на независимо заданный нормативный config12. Эта фикстура
// не вызывает production-коррекцию; строгий декодер и DFT затем проверяют её карту.
func expansionNormative71Fixture(t *testing.T, data []byte) []byte {
	t.Helper()
	// descriptor содержит полный ESDS DecoderSpecificInfo с известным ASC FFmpeg 7.1.5.
	descriptor := []byte{5, 0x80, 0x80, 0x80, 5, 0x11, 0xb8, 0x56, 0xe5, 0}
	if bytes.Count(data, descriptor) != 1 {
		t.Fatal("Ожидался единственный точный ESDS AAC-конфигурации")
	}
	// normalized получает самостоятельную копию фикстуры; offset адресует её исходный ASC.
	normalized := bytes.Clone(data)
	offset := bytes.Index(normalized, descriptor) + 5
	copy(normalized[offset:offset+5], []byte{0x11, 0xe0, 0x56, 0xe5, 0})
	return normalized
}

// expansionSource создаёт независимый LIVE с различимыми каналами, затем разбирает его
// рабочим разрешителем конфигурации fMP4 и Inspector; желаемый аудиоформат не подставляется в измеренный формат.
func expansionSource(t *testing.T, executable string, p core.GroupProfile) ([]byte, []media.Packet, fmp4.Init) {
	t.Helper()
	// tones задаёт PCM-частоты; wave хранит независимый детерминированный источник WAV.
	tones := expansionTones(p.Format.Audio.ChannelLayout, p.Format.Audio.Channels)
	wave := expansionWave(t, p.Format.Audio.ChannelLayout, tones)
	// args создаёт короткий источник AV1/AAC независимо от проверяемого компилятора.
	args := []string{"-v", "error", "-xerror", "-f", "lavfi", "-i", "testsrc2=size=128x128:rate=30:duration=1.6", "-f", "wav", "-i", wave, "-map", "0:v", "-map", "1:a", "-c:v", "libaom-av1", "-threads:v", "1", "-cpu-used", "8", "-lag-in-frames", "0", "-g:v", "3", "-b:v", "250000", "-vf", "setparams=color_primaries=bt709:color_trc=bt709:colorspace=bt709", "-color_range:v", "tv", "-color_primaries:v", "bt709", "-color_trc:v", "bt709", "-colorspace:v", "bt709", "-c:a", "aac", "-b:a", fmt.Sprint(p.AudioEncoding.BitrateBPS), "-f", "mp4", "-movflags", "delay_moov+default_base_moof+frag_every_frame", "-min_frag_duration", "1", "-use_editlist", "1", "-movie_timescale", "48000", "-avoid_negative_ts", "disabled", "pipe:1"}
	// data содержит настоящий источник; d читает его без подстановки желаемого аудиоформата; err сообщает об отказе разбора.
	data := mediaCommand(t, executable, nil, args...)
	if p.Format.Audio.ChannelLayout == "7.1" {
		data = expansionNormative71Fixture(t, data)
	}
	d, err := fmp4.NewDemuxer(bytes.NewReader(data), fmp4.DemuxOptions{Limits: integrationLimits(), SourceEpoch: 1, VideoRevision: 1, AudioRevision: 1, ResolveConfig: func(desc fmp4.TrackDescription) (media.CodecConfig, error) {
		// AAC восстанавливается исключительно из ASC; AV1 получает известную частоту кадров источника.
		if desc.Track == media.TrackAudio {
			return fmp4.ResolveCodecConfig(desc, 1, 1)
		}
		return media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: 1, Revision: 1, TimeBase: desc.TimeBase, Video: p.Format.Video}, desc.Headers)
	}})
	if err != nil {
		t.Fatal(err)
	}
	// packets содержит неизменяемые рабочие AU, init — фактические исходные конфигурации.
	packets := readMediaPackets(t, d)
	init := d.Init()
	if init.Audio.Info().Audio != p.Format.Audio {
		t.Fatalf("ASC изменил speakers: %+v != %+v", init.Audio.Info().Audio, p.Format.Audio)
	}
	// inspector проверяет каждую аудиоединицу через рабочий парсер приложения.
	var inspector codec.Inspector
	// packet содержит очередную полную единицу доступа.
	for _, packet := range packets {
		if packet.Config().Info().Track == media.TrackAudio {
			if _, err = inspector.Inspect(packet); err != nil {
				t.Fatal(err)
			}
		}
	}
	return data, packets, init
}

// expansionAssertTones отличает перестановку и потерю каналов по частотам исходного WAV.
// pcm содержит чередующиеся поканальные float32 после декодирования AAC; frequencies задаёт независимый порядок каналов.
func expansionAssertTones(t *testing.T, pcm []byte, frequencies []int) {
	t.Helper()
	// channels задаёт шаг чередования каналов PCM; begin пропускает задержку декодера; count задаёт окно DFT.
	channels, begin, count := len(frequencies), 4096, 8192
	if len(pcm) < (begin+count)*channels*4 {
		t.Fatal("Недостаточно PCM для проверки порядка")
	}
	// channel и wanted выбирают проверяемый динамик и его исходную частоту.
	for channel, wanted := range frequencies {
		// power вычисляет амплитуду выбранной frequency в конкретном выходном канале.
		power := func(frequency int) float64 {
			// real и imaginary накапливают проекцию PCM на точную синусоиду.
			real, imaginary := 0.0, 0.0
			// i выбирает отсчёт внутри проверяемого PCM-окна.
			for i := range count {
				// sample восстанавливает аудиоамплитуду; angle задаёт фазу выбранной частоты.
				sample := float64(math.Float32frombits(binary.LittleEndian.Uint32(pcm[((begin+i)*channels+channel)*4:])))
				angle := 2 * math.Pi * float64(frequency*i) / 48000
				real += sample * math.Cos(angle)
				imaginary += sample * math.Sin(angle)
			}
			return math.Hypot(real, imaginary) / float64(count)
		}
		// expected содержит амплитуду исходного тона этого динамика.
		expected := power(wanted)
		if expected < 0.015 {
			t.Fatalf("Потерян канал %d %dHz: %g", channel, wanted, expected)
		}
		// other задаёт тон другого динамика, который не должен преобладать в этом канале.
		for _, other := range frequencies {
			if other != wanted && power(other) > expected/4 {
				t.Fatalf("Перепутаны speakers: канал%d %dHz/%dHz", channel, wanted, other)
			}
		}
	}
}

// TestMediaExpansionSVTRealQualityPreparation проверяет реальные SVT CQP/CRF границы
// и tune2/4/5 через рабочий компилятор, подготовку, разбор приложением и декодирование.
// t сообщает об ошибках проверяемого сценария.
func TestMediaExpansionSVTRealQualityPreparation(t *testing.T) {
	// executable разрешает явный локальный FFmpeg; обычный CI не требует медиабинарников.
	executable := os.Getenv("GOST_MEDIA_FFMPEG")
	if executable == "" {
		t.Skip("CPU-проверка требует GOST_MEDIA_FFMPEG")
	}
	// tc задаёт независимые разрешённые сочетания качества и tuning и ожидаемый внешний tune.
	for _, tc := range []struct {
		// rc и tuning задают публичные настройки, tune — внешний номер SVT.
		rc, tuning, tune string
		// quality задаёт допустимую границу или представительное значение качества.
		quality int
	}{{"cqp", "psnr", "1", 1}, {"cqp", "psnr", "1", 63}, {"crf", "psnr", "1", 1}, {"crf", "psnr", "1", 63}, {"crf", "ssim", "2", 35}, {"crf", "ms_ssim", "4", 35}, {"crf", "vmaf", "5", 35}} {
		// t сообщает об ошибках проверяемого сценария.
		t.Run(fmt.Sprintf("%s/%s/%d", tc.rc, tc.tuning, tc.quality), func(t *testing.T) {
			// Проверяет указанное сочетание без замены выбранного libsvtav1.
			// p задаёт полный новый профиль качества.
			p := expansionProfile("stereo", 2)
			p.Encoding = core.DefaultEncoding(p.Mode, core.VideoEncoderSVTAV1, tc.rc)
			p.Encoding.VideoCustom = "lp=1"
			p.Encoding.SVTAV1.Preset = 12
			p.Encoding.SVTAV1.Tuning = tc.tuning
			if tc.rc == "cqp" {
				p.Encoding.QP = core.OptionalQP{Set: true, Value: tc.quality}
			} else {
				p.Encoding.CRF = core.OptionalCRF{Set: true, ValueHundredths: tc.quality * 100}
			}
			// plan содержит реальные параметры; err сообщает об отказе компилятора.
			plan, err := encoding.Compile(p, encoding.Prepared)
			if err != nil {
				t.Fatal(err)
			}
			// command хранит представление аргументов для независимой проверки отображения на внешние параметры.
			command := " " + strings.Join(plan.Video, " ") + " "
			if !strings.Contains(command, "tune="+tc.tune) || strings.Contains(command, " -b:v ") || strings.Contains(command, " -maxrate:v ") || strings.Contains(command, " -bufsize:v ") {
				t.Fatal("Потерян quality recipe", command)
			}
			// prepared создан рабочими генератором, проверкой и нормализатором, не одной CLI-пробой.
			prepared := liveAudioPrepared(t, executable, p, "image")
			// packets содержит разобранные приложением пакеты fMP4.
			packets, err := encodingPreparedPackets(prepared, p.Format)
			if err != nil {
				t.Fatal(err)
			}
			// inspector проверяет реальную независимость и формат каждого AU.
			var inspector codec.Inspector
			// packet выбирает видеопакет подготовленной заглушки.
			for _, packet := range packets[media.TrackVideo] {
				if _, err = inspector.Inspect(packet); err != nil {
					t.Fatal(err)
				}
			}
			// decoded содержит независимые контрольные суммы после реального декодирования AV1.
			decoded := mediaCommand(t, executable, prepared, "-v", "error", "-xerror", "-strict", "2", "-i", "pipe:0", "-map", "0:v", "-f", "framemd5", "pipe:1")
			if strings.Count(string(decoded), "\n0,") != 48 {
				t.Fatal("Не декодированы все 48 кадров")
			}
		})
	}
}

// TestMediaExpansionAACRealCopyTransport проверяет шесть раскладок через настоящий AAC,
// fMP4/Inspector приложения, приём и отправку RTMP, петли заглушки и переходы LIVE↔FALLBACK.
func TestMediaExpansionAACRealCopyTransport(t *testing.T) {
	// executable включает явно разрешённую CPU-интеграцию.
	executable := os.Getenv("GOST_MEDIA_FFMPEG")
	if executable == "" {
		t.Skip("CPU-проверка требует GOST_MEDIA_FFMPEG")
	}
	// layout и channels задают проверяемые канонические пары раскладки и числа каналов.
	for layout, channels := range map[string]uint16{"2.1": 3, "3.0": 3, "4.0": 4, "5.0": 5, "5.1": 6, "7.1": 8} {
		t.Run(layout, func(t *testing.T) {
			// Проверяет точную карту каналов и побайтное копирование всего локального медиатракта.
			// p содержит полный новый профиль; prepared — нормализованную тишину рабочего генератора.
			p := expansionProfile(layout, channels)
			prepared := expansionPrepared(t, executable, p)
			// quietPackets и quiet сохраняют точные AAC AU подготовленной заглушки
			// для проверки каждого выходного AU, а не только нескольких совпавших LIVE-пакетов.
			// err сохраняет отказы разбора заглушки и последующих операций медиатракта внутри этого подтеста.
			quietPackets, err := encodingPreparedPackets(prepared, p.Format)
			if err != nil {
				t.Fatal(err)
			}
			quiet := map[string]bool{}
			// packet выбирает каждый известный AU тишины одной точной петли.
			for _, packet := range quietPackets[media.TrackAudio] {
				quiet[string(packet.Payload())] = true
			}
			// pcm независимо декодирует подготовленную тишину во всех каналах.
			pcm := mediaCommand(t, executable, prepared, "-v", "error", "-xerror", "-strict", "2", "-i", "pipe:0", "-map", "0:a", "-f", "f32le", "pipe:1")
			if len(pcm) == 0 || liveAudioNonzeroSamples(pcm) != 0 {
				t.Fatal("Подготовка не дала цифровую тишину")
			}
			// sourceBytes, source и init задают независимо закодированный LIVE с различимыми каналами.
			sourceBytes, source, init := expansionSource(t, executable, p)
			expansionAssertTones(t, mediaCommand(t, executable, sourceBytes, "-v", "error", "-xerror", "-strict", "2", "-i", "pipe:0", "-map", "0:a", "-f", "f32le", "pipe:1"), expansionTones(layout, channels))
			// known содержит точные AAC AU источника для проверки сохранности копирования.
			known := map[string]bool{}
			// i задаёт пакет source; packet — исходный AU перед добавлением заголовка последовательности к ключевому кадру AV1.
			for i, packet := range source {
				if packet.Config().Info().Track == media.TrackAudio {
					known[string(packet.Payload())] = true
					continue
				}
				if packet.Info().Keyframe {
					// rebuilt содержит независимый AV1 AU; err сообщает об ошибке создания неизменяемого пакета.
					rebuilt, err := media.NewPacket(packet.Info(), packet.Config(), append(init.Video.Headers()[4:], packet.Payload()...))
					if err != nil {
						t.Fatal(err)
					}
					source[i] = rebuilt
				}
			}
			// receiver принимает настоящий RTMP; f владеет изолированными сессией и маршрутизатором.
			receiver, _ := newPublisherReceiver(t, false)
			f := newPublisherPipeline(t, []core.GroupProfile{p}, []string{"rtmp://" + receiver.listener.Addr().String() + "/multichannel"}, []string{"synthetic-key"}, []int{0})
			// filePath хранит нормализованную заглушку для реального файлового проигрывателя.
			filePath := filepath.Join(t.TempDir(), "prepared.mp4")
			// err сообщает об отказе записи подготовленного файла текущего теста.
			if err := os.WriteFile(filePath, prepared, 0600); err != nil {
				t.Fatal(err)
			}
			f.config.Binary = executable
			f.config.OpenFile = func(context.Context, core.ArtifactManifest) (*os.File, error) { /* Открывает принадлежащую тесту подготовленную заглушку. */
				return os.Open(filePath)
			}
			// registry создаёт настоящий файловый проигрыватель с UDS и группу и владеет их завершением;
			// err сохраняет отказы создания реестра, группы и последующего освобождения ресурсов.
			registry, err := group.NewRegistry(context.Background(), group.RegistryConfig{Core: f.service, Parent: f.parent, Observer: f.observer, Runtime: f.config, MaxGroups: 2, MaxReferences: 2})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { /* Останавливает группу и дочерние FFmpeg до удаления файлов. */
				// err сообщает об отказе завершения реестра и его медиапроцессов.
				if err := registry.Close(context.Background()); err != nil {
					t.Error(err)
				}
			})
			// ref удерживает группу; pub передаёт её выход через единственное локальное соединение RTMP.
			ref, err := registry.Acquire(context.Background(), f.outputs[0].Snapshot, f.live)
			if err != nil {
				t.Fatal(err)
			}
			pub := publisherActivate(t, f, f.outputs[0], ref.Router(), nil)
			receiver.wait(t, func(messages []publisherWireMessage) bool { /* Ожидает больше двух полных периодов заглушки. */
				// units считает полные RTMP-сообщения A/V, включая Enhanced Audio.
				units := 0
				// m содержит независимое принятое сообщение.
				for _, m := range messages {
					if m.typ == 8 || m.typ == 9 {
						units++
					}
				}
				return units > 260
			})
			// loopPCM содержит фактически доставленную тишину более двух петель,
			// поэтому проверяет также файловый проигрыватель с UDS и переключатель, а не только исходный файл.
			loopPCM := mediaCommand(t, executable, expansionDecodeFLV(receiver.snapshot()), "-v", "error", "-xerror", "-strict", "2", "-i", "pipe:0", "-map", "0:a", "-f", "f32le", "pipe:1")
			if len(loopPCM) < 2*int(channels)*48000*4 || liveAudioNonzeroSamples(loopPCM) != 0 {
				t.Fatal("Петли RTMP-заглушки не сохранили цифровую тишину")
			}
			// stop и done управляют конечной реальной RTMP-публикацией LIVE.
			_, stop, done := publisherStartIngest(t, f, source, init)
			defer stop()
			publisherWait(t, func() bool { /* Ожидает подтверждённый LIVE вместо принятия одного факта записи. */
				return ref.Group().Status().Available && ref.Group().Status().Source == group.Live
			})
			receiver.wait(t, func(messages []publisherWireMessage) bool {
				// Ожидает фактической доставки более 25 исходных AAC AU на локальное назначение.
				// matched считает реально принятые неизменённые AAC AU.
				matched := 0
				// m представляет очередное принятое сообщение.
				for _, m := range messages {
					// kind и payload восстанавливают настоящую AAC-нагрузку обоих сетевых представлений.
					kind, payload := expansionAudioMessage(m)
					if kind == 1 && known[string(payload)] {
						matched++
					}
				}
				return matched > 25
			})
			stop()
			<-done
			publisherWait(t, func() bool { /* Ожидает возврат к исправной заглушке после EOF LIVE. */
				return ref.Group().Status().Available && ref.Group().Status().Source == group.Fallback
			})
			if layout == "7.1" {
				// wideInfo и wide задают нормативный config7 с иной реальной картой динамиков;
				// обычный ingest/copy не имеет provenance собственного кодирования и не должен его чинить.
				// err сохраняет отказы создания конфигураций и пакетов несовместимого входа.
				wideInfo := init.Audio.Info()
				wideInfo.Audio.ChannelLayout = "7.1(wide-back)"
				wide, err := media.NewCodecConfig(wideInfo, []byte{0x11, 0xb8, 0x56, 0xe5, 0})
				if err != nil {
					t.Fatal(err)
				}
				// incompatible содержит те же AU/время, но с честной несовместимой конфигурацией AAC.
				incompatible := append([]media.Packet(nil), source...)
				// index и packet выбирают исходные аудиопакеты для новой immutable конфигурации.
				for index, packet := range incompatible {
					if packet.Config().Info().Track == media.TrackAudio {
						incompatible[index], err = media.NewPacket(packet.Info(), wide, packet.Payload())
						if err != nil {
							t.Fatal(err)
						}
					}
				}
				// incompatibleInit передаёт исходный config7 без изменения заголовка при публикации.
				incompatibleInit := init
				incompatibleInit.Audio = wide
				// ingress получает фактически принятые входные AU до групповой проверки совместимости.
				ingress, err := f.live.Subscribe(publisherQueue())
				if err != nil {
					t.Fatal(err)
				}
				defer func() { /* Закрывает независимую подписку несовместимого входа. */
					_ = ingress.Close()
				}()
				// stopBad и badDone завершают реальную публикацию с исходным wide-back ASC.
				_, stopBad, badDone := publisherStartIngest(t, f, incompatible, incompatibleInit)
				defer stopBad()
				// ctx и cancel ограничивают ожидание доказанного входного config7 тремя секундами.
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				// observedAudio считает реальные входные AAC AU за пределами окна восстановления группы.
				observedAudio := 0
				for observedAudio < 12 {
					// delivery и readErr содержат фактическую текущую входную единицу доступа.
					delivery, readErr := ingress.Next(ctx)
					if readErr != nil {
						t.Fatal("Не получен фактический wide-back вход", readErr)
					}
					if !delivery.IsCurrent() || delivery.Packet.Config().Info().Track != media.TrackAudio {
						continue
					}
					if delivery.Packet.Config().Info().Audio.ChannelLayout != "7.1(wide-back)" || !bytes.Equal(delivery.Packet.Config().Headers(), []byte{0x11, 0xb8, 0x56, 0xe5, 0}) {
						t.Fatal("Ingest исправил untrusted config7")
					}
					observedAudio++
				}
				if !ref.Group().Status().Available || ref.Group().Status().Source != group.Fallback {
					t.Fatal("Несовместимый config7 вытеснил исправный fallback")
				}
				stopBad()
				<-badDone
			}
			if err = pub.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			receiver.waitClosed(t)
			if receiver.accepted.Load() != 1 {
				t.Fatal("Переход пересоздал исправное RTMP-соединение")
			}
			// captured содержит полный смешанный поток; live содержит только LIVE AAC и его ASC.
			captured := receiver.snapshot()
			expansionAssertWireTime(t, captured)
			var live []publisherWireMessage
			// rawASCSeen подтверждает исходный ASC среди выходных заголовков последовательности.
			rawASCSeen := false
			// m выбирает заголовки аудиопоследовательности и известные исходные AU.
			for _, m := range captured {
				// kind и payload извлекают AAC без преобразования конфигурации.
				kind, payload := expansionAudioMessage(m)
				if kind == 1 && !known[string(payload)] && !quiet[string(payload)] {
					t.Fatal("Копирование изменило AAC AU")
				}
				if kind == 0 && bytes.Equal(payload, init.Audio.Headers()) {
					rawASCSeen = true
				}
				if kind == 0 || kind == 1 && known[string(payload)] {
					live = append(live, m)
				}
			}
			if !rawASCSeen {
				t.Fatal("RTMP переписал raw ASC")
			}
			expansionAssertTones(t, mediaCommand(t, executable, expansionDecodeFLV(live), "-v", "error", "-xerror", "-strict", "2", "-i", "pipe:0", "-map", "0:a", "-f", "f32le", "pipe:1"), expansionTones(layout, channels))
			// decoded проверяет реальное непрерывное декодирование обеих дорожек после переключений.
			decoded := mediaCommand(t, executable, expansionDecodeFLV(captured), "-v", "error", "-xerror", "-strict", "2", "-i", "pipe:0", "-map", "0:v", "-map", "0:a", "-f", "framemd5", "pipe:1")
			if strings.Count(string(decoded), "\n0,") < 60 || strings.Count(string(decoded), "\n1,") < 120 {
				t.Fatal("Не доказано декодирование смешанного потока")
			}
		})
	}
}

// TestMediaExpansionAACRealTranscodeSpeakers проверяет явное LIVE AAC transcode для
// шести раскладок через рабочий компилятор, медиапроцесс и UDS; только NVIDIA-видео заменяется CPU.
// t сообщает об ошибках проверяемого сценария.
func TestMediaExpansionAACRealTranscodeSpeakers(t *testing.T) {
	// executable включает явно выбранный локальный медианабор.
	executable := os.Getenv("GOST_MEDIA_FFMPEG")
	if executable == "" {
		t.Skip("CPU-проверка требует GOST_MEDIA_FFMPEG")
	}
	// layout и channels задают проверяемые канонические пары раскладки и числа каналов.
	for layout, channels := range map[string]uint16{"2.1": 3, "3.0": 3, "4.0": 4, "5.0": 5, "5.1": 6, "7.1": 8} {
		// t сообщает об ошибках проверяемого сценария.
		t.Run(layout, func(t *testing.T) {
			// Проверяет реальные медиапроцесс и кодировщик AAC без имитации звукового преобразования.
			// input задаёт независимый источник AV1; p — выход H264 с явным перекодированием AAC.
			input := expansionProfile(layout, channels)
			p := fallbackPipelineProfile(core.CodecH264)
			p.LiveAudioMode = core.LiveAudioTranscode
			p.Encoding = core.DefaultEncoding(p.Mode, core.VideoEncoderNVENC, core.RateControlVBR)
			p.Encoding.AudioCustom = "aac_coder=fast"
			p.Format.Audio = input.Format.Audio
			p.AudioEncoding.BitrateBPS = uint64(channels) * 48000
			// prepared создан рабочим AAC-планом с CPU-подстановкой только H264-видео.
			prepared := expansionPrepared(t, executable, p)
			// source и init задают реальные входные AU с независимыми частотами каналов.
			_, source, init := expansionSource(t, executable, input)
			// originalAudio содержит входные AAC AU до обязательного явного перекодирования.
			originalAudio := map[string]bool{}
			// packet выбирает исходные аудиоединицы для отрицательной проверки ошибочного copy.
			for _, packet := range source {
				if packet.Config().Info().Track == media.TrackAudio {
					originalAudio[string(packet.Payload())] = true
				}
			}
			// i адресует source; packet задаёт AV1 AU, которому требуется заголовок последовательности в независимой точке.
			for i, packet := range source {
				if packet.Config().Info().Track == media.TrackVideo && packet.Info().Keyframe {
					// rebuilt сохраняет формат и время; err сообщает об ошибке создания неизменяемого пакета.
					rebuilt, err := media.NewPacket(packet.Info(), packet.Config(), append(init.Video.Headers()[4:], packet.Payload()...))
					if err != nil {
						t.Fatal(err)
					}
					source[i] = rebuilt
				}
			}
			// receiver принимает RTMP; f владеет изолированными сервисом и сессией.
			receiver, _ := newPublisherReceiver(t, false)
			f := newPublisherPipeline(t, []core.GroupProfile{p}, []string{"rtmp://" + receiver.listener.Addr().String() + "/transcode"}, []string{"synthetic-key"}, []int{0})
			// filePath адресует только подготовленный файл этого подтеста.
			filePath := filepath.Join(t.TempDir(), "prepared.mp4")
			// err сообщает об отказе записи подготовленной заглушки текущего теста.
			if err := os.WriteFile(filePath, prepared, 0600); err != nil {
				t.Fatal(err)
			}
			f.config.Binary = executable
			f.config.OpenFile = func(context.Context, core.ArtifactManifest) (*os.File, error) { /* Открывает принадлежащий этому тесту файл заглушки. */
				return os.Open(filePath)
			}
			// c содержит настройки штатного построения команды, a — фактическую попытку с её транспортом.
			f.config.Transcode = func(_ context.Context, c ffmpeg.NVIDIAConfig, a ffmpeg.Attempt) (ffmpeg.Command, error) { /* Сохраняет рабочий AAC-план, заменяя только недоступный NVIDIA-видеокодировщик. */
				return liveAudioCPUCommand(c, a)
			}
			// registry владеет медиапроцессом и файловым проигрывателем до завершения теста;
			// err сохраняет отказы создания реестра, группы и остановки ресурсов.
			registry, err := group.NewRegistry(context.Background(), group.RegistryConfig{Core: f.service, Parent: f.parent, Observer: f.observer, Runtime: f.config, MaxGroups: 2, MaxReferences: 2})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { /* Дожидается завершения медиапроцессов и групп. */
				// err сообщает об отказе освобождения ресурсов реестра.
				if err := registry.Close(context.Background()); err != nil {
					t.Error(err)
				}
			})
			// ref удерживает рабочую группу; pub отправляет её A/V на локальный RTMP.
			ref, err := registry.Acquire(context.Background(), f.outputs[0].Snapshot, f.live)
			if err != nil {
				t.Fatal(err)
			}
			pub := publisherActivate(t, f, f.outputs[0], ref.Router(), nil)
			publisherWait(t, func() bool { /* Ожидает готовность начальной заглушки. */
				return ref.Group().Status().Available
			})
			// stop и done ограниченно останавливают и присоединяют производителя настоящей входной публикации.
			_, stop, done := publisherStartIngest(t, f, source, init)
			defer stop()
			publisherWait(t, func() bool { /* Ожидает фактический выход медиапроцесса на LIVE. */
				return ref.Group().Status().Available && ref.Group().Status().Source == group.Live
			})
			// begin отделяет начальную тишину от последующих AAC AU уже работающего transcode.
			begin := len(receiver.snapshot())
			// messages содержит текущий независимый захват RTMP для проверки достаточной длины LIVE.
			receiver.wait(t, func(messages []publisherWireMessage) bool { /* Ожидает достаточно декодируемого LIVE для независимого DFT. */
				return len(messages) > begin+50
			})
			// live содержит самостоятельный захват текущего участка LIVE.
			live := receiver.snapshot()
			stop()
			<-done
			if err = pub.Stop(context.Background()); err != nil {
				t.Fatal(err)
			}
			receiver.waitClosed(t)
			// selected сохраняет заголовки последовательности и медиаданные после подтверждённого начала transcode.
			var selected []publisherWireMessage
			// transformed считает новые LIVE AAC AU, которые не могли появиться простым копированием.
			transformed := 0
			// i выбирает сообщение; m содержит независимые байты RTMP.
			for i, m := range live {
				// kind отличает ASC от кодированного аудио; payload содержит исходные байты AAC сообщения.
				kind, payload := expansionAudioMessage(m)
				if kind == 0 || kind == 1 && i >= begin {
					selected = append(selected, m)
					if kind == 1 && !originalAudio[string(payload)] {
						transformed++
					}
				}
			}
			if transformed < 15 {
				t.Fatal("Явный transcode сохранил исходные AAC AU")
			}
			expansionAssertTones(t, mediaCommand(t, executable, expansionDecodeFLV(selected), "-v", "error", "-xerror", "-strict", "2", "-i", "pipe:0", "-map", "0:a", "-f", "f32le", "pipe:1"), expansionTones(layout, channels))
		})
	}
}
