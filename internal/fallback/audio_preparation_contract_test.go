package fallback

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

// audioPreparationFixture связывает синтетический fMP4 нативного кодировщика с независимо измеренными
// ffprobe 7.1.5 числом AU и периодом. Обычные тесты читают записанные байты без внешних процессов.
type audioPreparationFixture struct {
	// kind выбирает конечную аудиопривязку, name — имя файла в testdata/audio_bindings.
	kind core.Codec
	name string
	// digest фиксирует SHA256 исходного контейнера, чтобы замена эталона не меняла ожидания молча.
	digest string
	// period задаёт полный период обеих дорожек; quantum — число отсчётов 48 kHz в одной полной AU.
	period  time.Duration
	quantum int64
	// videos и audios задают измеренное число сохраняемых видеокадров и полных аудиоединиц.
	videos int
	audios int
	// headers содержит измеренные дополнительные заголовки ffprobe; MP3 и AC3 оставляют их пустыми.
	headers string
	// payloadDigest фиксирует все сохраняемые AU отдельно декодированного эталона с точным PCM-нолём.
	payloadDigest string
}

// audioPreparationFixtures возвращает четыре коротких результата кодирования black/anullsrc
// через FFmpeg 7.1.5: x264 High/30fps/128×128 и заданные аудиорецепты без GPU и пользовательских файлов.
// Периоды 0.1/2.4/0.6/0.8 s измерены по реальным AU, а не вычислены рабочим округлителем.
func audioPreparationFixtures() []audioPreparationFixture {
	return []audioPreparationFixture{
		{core.CodecOpus, "opus-celt-silent.mp4", "74608e9900647f67ae07122c0e9d2f5807ddfdb559da0d602cbdfe5c660fa395", 100 * time.Millisecond, 120, 3, 40, "4f707573486561640102780080bb0000000000", "8d8cf8016b3023b94aa7e278ab4887dda51d741bb45609ba8043d43d8189ab09"},
		{core.CodecFLAC, "flac-s16-silent.mp4", "90c5f7ff176c32ba06d4666cbd4cb07f4cf5fd1e810f869d7f8667c0155158c7", 2400 * time.Millisecond, 4608, 72, 25, "12001200000000004a580bb802f00000000000000000000000000000000000000000", "7bdf81f814cf44d8f7c1c08fcfbfbd6baf8f1e2ea6a940751726b191b374e38f"},
		{core.CodecMP3, "mp3-mpeg1-silent.mp4", "7c64faba6559ca7b2abcaae99a2d4691fb5da49d2e8f0c04763a962354f3771e", 600 * time.Millisecond, 1152, 18, 25, "", "c2d93be37a5268af3afcf27ea6783d38c4f2718a222d6f7aaa93d3eff70558ed"},
		{core.CodecAC3, "ac3-stereo-silent.mp4", "cf55203965e7309ca8dabd487bb68531a276aa45be92a38d164b4708510f9e47", 800 * time.Millisecond, 1536, 24, 25, "", "1ba6557283cb8ac131bb08ecec691b78d4a06c4f159d276ed781e44b82932e17"},
	}
}

// audioPreparationBytes читает принадлежащий тесту t эталон fixture и сверяет его происхождение
// по точному SHA256; отсутствие или замена синтетического файла немедленно завершает тест.
func audioPreparationBytes(t *testing.T, fixture audioPreparationFixture) []byte {
	t.Helper()
	// data сохраняет исходные байты; err сообщает о недоступном эталоне.
	data, err := os.ReadFile(filepath.Join("testdata", "audio_bindings", fixture.name))
	if err != nil {
		t.Fatal(err)
	}
	if fmt.Sprintf("%x", sha256.Sum256(data)) != fixture.digest {
		t.Fatalf("изменился эталон %s: SHA256=%x", fixture.name, sha256.Sum256(data))
	}
	return data
}

// audioPreparationFile создаёт для t отдельный изменяемый файл с data; исходная фикстура
// остаётся неизменной, а закрытие и удаление дескриптора выполняют тестовые владельцы.
func audioPreparationFile(t *testing.T, data []byte) *os.File {
	t.Helper()
	// file принадлежит текущему сценарию; err сообщает об отказе создания либо записи.
	file, err := os.CreateTemp(t.TempDir(), "audio.mp4")
	if err != nil {
		t.Fatal(err)
	}
	selfClose(t, file)
	if _, err = file.Write(data); err != nil {
		t.Fatal(err)
	}
	return file
}

// audioPreparationDump представляет raw в формате hex-колонки ffprobe без изменения байтов.
// t сообщает об ошибке записанного шестнадцатеричного эталона.
func audioPreparationDump(t *testing.T, raw string) (string, uint32) {
	t.Helper()
	// data содержит исходную extradata; err сообщает о повреждении её записи.
	data, err := hex.DecodeString(raw)
	if err != nil {
		t.Fatal(err)
	}
	// dump накапливает строки с непрерывными смещениями и отдельной ASCII-колонкой.
	var dump bytes.Buffer
	// offset выбирает начало очередных не более 16 байтов заголовка.
	for offset := 0; offset < len(data); offset += 16 {
		fmt.Fprintf(&dump, "%08x: %x  .\n", offset, data[offset:min(offset+16, len(data))])
	}
	return dump.String(), uint32(len(data))
}

// audioPreparationDocument задаёт ответ ffprobe для fixture; t проверяет запись измеренных заголовков.
// Параметры дорожек измерены независимо, а Format.Duration синтетически равна целевому полному периоду:
// это положительное значение для общей проверки бюджета, а не измеренная длительность исходного файла.
// Точный период проверяется отдельно по настоящим AU; внешний процесс здесь подменён.
func audioPreparationDocument(t *testing.T, fixture audioPreparationFixture) probeDocument {
	t.Helper()
	// video описывает измеренный SPS с High/level1.1 и явным SDR BT.709.
	video := probeStream{Type: "video", Codec: "h264", Profile: "High", Width: 128, Height: 128, Rate: "30/1", Pixel: "yuv420p", Level: 11, ColorRange: "tv", Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"}
	video.ExtraData, video.ExtraDataSize = audioPreparationDump(t, "0164000bffe1001c6764000bacb20408d80b50101014000003000400000300f03c50a92001000568ebccb22cfdf8f800")
	// audio содержит измеренную привязку 48 kHz/stereo без выдуманной extradata MP3/AC3.
	audio := probeStream{Type: "audio", Codec: string(fixture.kind), SampleRate: "48000", Channels: 2, Layout: "stereo"}
	if fixture.headers != "" {
		audio.ExtraData, audio.ExtraDataSize = audioPreparationDump(t, fixture.headers)
	}
	// document сохраняет лишь поля запроса -show_streams/-show_format, используемые Prober.
	document := probeDocument{Streams: []probeStream{video, audio}}
	document.Format.Name = "mov,mp4,m4a,3gp,3g2,mj2"
	// Duration подставляется только для проверки положительности и MaxDuration; число полных AU
	// и их временная сетка проверяются независимо от этого поля.
	document.Format.Duration = fmt.Sprintf("%.6f", fixture.period.Seconds())
	return document
}

// audioPreparationProber создаёт для t Prober с подставленным ответом document, настоящими
// демультиплексором и Inspector с конечными бюджетами. Измерены параметры дорожек, а Duration
// синтетическая; ни FFmpeg, ни ffprobe во время теста не запускаются.
func audioPreparationProber(t *testing.T, document probeDocument) Prober {
	t.Helper()
	// data содержит JSON подставленного ответа; err сообщает об ошибке сериализации тестового снимка.
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	return Prober{Binary: "probe", Runner: probeJSONRunner{data: data}, Limits: selfProbeLimits()}
}

// audioPreparationPackets читает data настоящим демультиплексором и возвращает пару конфигураций
// и AU выбранной track. fresh разрешает только чтение исходного нативного хвоста собственной тишины;
// t получает ошибку контейнера, а не заполненную по желаемому формату длительность.
func audioPreparationPackets(t *testing.T, data []byte, track media.Track, fresh bool) (fmp4.Init, []media.Packet) {
	t.Helper()
	// demux восстанавливает фактические заголовки и потребляет целую начальную Opus AU; err сообщает об отказе.
	demux, err := fmp4.NewDemuxer(bytes.NewReader(data), fmp4.DemuxOptions{Limits: selfProbeLimits().Container, SourceEpoch: 1, VideoRevision: 1, AudioRevision: 1, FreshSilentOpus: fresh, FreshSilentMP3: fresh})
	if err != nil {
		t.Fatal(err)
	}
	// packets сохраняет исходный порядок выбранной дорожки.
	var packets []media.Packet
	for {
		// packet содержит очередную настоящую AU; readErr различает EOF и повреждение файла.
		packet, readErr := demux.Next()
		if errors.Is(readErr, io.EOF) {
			return demux.Init(), packets
		}
		if readErr != nil {
			t.Fatal(readErr)
		}
		if packet.Config().Info().Track == track {
			packets = append(packets, packet)
		}
	}
}

// TestPreparedAudioFixturesPreserveVideoAndProveCompleteAudioPeriods проверяет через t
// Normalize→Verify→VerifyLoopDuration→reopen для четырёх настоящих файлов нативного кодировщика без внешних процессов.
// Измеренные AU/периоды выявляют лишнее удаление AU, неверную сетку, потерю видео и повторное доверие FLACOwned из файла.
func TestPreparedAudioFixturesPreserveVideoAndProveCompleteAudioPeriods(t *testing.T) {
	// fixture выбирает одну конечную привязку и её независимо измеренный период.
	for _, fixture := range audioPreparationFixtures() {
		// Подтест t проверяет выбранную fixture целиком, включая каждый сохранённый видео- и аудиопакет.
		t.Run(string(fixture.kind), func(t *testing.T) {
			// raw сохраняет контрольные байты; input даёт нормализатору отдельный дескриптор.
			raw := audioPreparationBytes(t, fixture)
			input := audioPreparationFile(t, raw)
			// profile задаёт настоящий Encoding6, prober подменяет только процесс ffprobe.
			profile := audioBindingProfile(t, fixture.kind)
			prober := audioPreparationProber(t, audioPreparationDocument(t, fixture))
			// prepared получает байты промежуточного файла и остаётся изолированным от исходного raw.
			prepared := audioPreparationFile(t, nil)
			if fixture.kind == core.CodecOpus || fixture.kind == core.CodecMP3 {
				// err проверяет обычную границу prepared без исключений для свежих длительностей и исправления Head120.
				if _, err := prober.Verify(context.Background(), input, profile); err == nil {
					t.Fatal("raw native файл принят обычной prepared границей")
				}
			}
			// err сохраняет отказ нормализации или последующей независимой проверки файла.
			if err := prober.NormalizePreparedForSource(context.Background(), input, prepared, profile, Source{}); err != nil {
				t.Fatal("normalize", err)
			}
			// actual выводится из заголовков файла и измеренных параметров дорожек; err сообщает об отказе Verify.
			actual, err := prober.Verify(context.Background(), prepared, profile)
			if err != nil || actual.Audio != profile.Format.Audio || actual.Video.FrameRate != (core.FrameRate{Numerator: 30, Denominator: 1}) {
				t.Fatal("verify", actual, err)
			}
			if err = prober.VerifyLoopDuration(context.Background(), prepared, fixture.period); err != nil {
				t.Fatal("period", err)
			}
			if err = prober.VerifyLoopDuration(context.Background(), prepared, fixture.period+time.Millisecond); !errors.Is(err, core.ErrNotReady) {
				t.Fatal("неверный ожидаемый период принят", err)
			}
			if fixture.kind == core.CodecOpus || fixture.kind == core.CodecMP3 || fixture.kind == core.CodecAC3 {
				if err = prober.NormalizePreparedForProfile(context.Background(), prepared, io.Discard, profile); err == nil {
					t.Fatal("готовый origin0/Head0 файл повторно получил fresh correction")
				}
			}
			// normalized содержит итоговый самостоятельный fMP4, а не сохранённую конфигурацию нормализатора.
			normalized, err := os.ReadFile(prepared.Name())
			if err != nil {
				t.Fatal(err)
			}
			// init и audio восстановлены повторным открытием; rawAudio сохраняет нативную форму до нормализации.
			init, audio := audioPreparationPackets(t, normalized, media.TrackAudio, false)
			_, rawAudio := audioPreparationPackets(t, raw, media.TrackAudio, true)
			if init.Audio.Info().FLACOwned || init.Audio.Info().TimeBase != (media.TimeBase{Numerator: 1, Denominator: 48000}) || len(audio) != fixture.audios {
				t.Fatal("reopen/period", init.Audio.Info(), len(audio), fixture.audios)
			}
			// skip отражает только одну полную начальную MP3 AU; Opus уже потреблён демультиплексором.
			skip := 0
			if fixture.kind == core.CodecMP3 {
				skip = 1
				if len(rawAudio) != 26 || rawAudio[0].Info().DTS != -1105 || rawAudio[25].Info().DurationTicks != 1105 {
					t.Fatal("утрачена измеренная native N+1 форма MP3")
				}
			}
			if len(rawAudio) != len(audio)+skip || !bytes.Equal(init.Audio.Headers(), rawAudio[0].Config().Headers()) {
				t.Fatal("изменились headers либо число сохраняемых AU")
			}
			// digest собирает payload всех AU для сопоставления с отдельно декодированным эталоном.
			digest := sha256.New()
			// index задаёт номер полной выходной AU; packet содержит её сжатые данные и точные метки.
			for index, packet := range audio {
				if packet.Info() != (media.PacketInfo{PTS: int64(index) * fixture.quantum, DTS: int64(index) * fixture.quantum, DurationTicks: fixture.quantum}) {
					t.Fatal("неполная сетка аудио", index, packet.Info())
				}
				if err = verifyPreparedAudio(packet, uint64(index)); err != nil {
					t.Fatal("actual AU/counter/CRC", index, err)
				}
				// samples подтверждает размер из сжатой AU; sampleErr сохраняет отказ разборщика кодека.
				samples, sampleErr := codec.AudioSamples(fixture.kind, packet.Config().Headers(), packet.Payload())
				if sampleErr != nil || int64(samples) != fixture.quantum {
					t.Fatal("неполная actual AU", samples, sampleErr)
				}
				if fixture.kind != core.CodecAC3 && !bytes.Equal(packet.Payload(), rawAudio[index+skip].Payload()) {
					t.Fatal("изменены сохраняемые сжатые данные", index)
				}
				_, _ = digest.Write(packet.Payload())
			}
			if fixture.kind == core.CodecOpus && !bytes.Equal(init.Audio.Headers(), []byte("OpusHead\x01\x02\x00\x00\x80\xbb\x00\x00\x00\x00\x00")) {
				t.Fatalf("Head0 не подтверждён: %x", init.Audio.Headers())
			}
			if fixture.kind == core.CodecMP3 && !bytes.Equal(init.Audio.Headers(), []byte{0xff, 0xfb, 0x94, 0x64}) || fixture.kind == core.CodecAC3 && !bytes.Equal(init.Audio.Headers(), []byte{0x10, 0x11, 0x40}) {
				t.Fatalf("actual MPEG-1 descriptor/dac3 не совпал с измеренным: %x", init.Audio.Headers())
			}
			// originalVideo и video представляют один и тот же ряд реальных AU до и после нормализации.
			_, originalVideo := audioPreparationPackets(t, raw, media.TrackVideo, true)
			_, video := audioPreparationPackets(t, normalized, media.TrackVideo, false)
			if len(video) != fixture.videos || len(originalVideo) != len(video) {
				t.Fatal("потеря видео", len(originalVideo), len(video))
			}
			// index сопоставляет каждую видеоединицу, packet сохраняет исходные байты и время без аудиопоправок.
			for index, packet := range originalVideo {
				if packet.Info() != video[index].Info() || packet.Config().Info().TimeBase != video[index].Config().Info().TimeBase || !bytes.Equal(packet.Payload(), video[index].Payload()) {
					t.Fatal("изменено независимое видео", index)
				}
			}
			// unchanged проверяет исходный файл после всех преобразований; readErr сообщает об отказе чтения.
			unchanged, readErr := os.ReadFile(input.Name())
			if readErr != nil || !bytes.Equal(unchanged, raw) {
				t.Fatal("изменён исходный контейнер", readErr)
			}
			if fmt.Sprintf("%x", digest.Sum(nil)) != fixture.payloadDigest {
				t.Fatalf("изменены AU эталона с независимо декодированным цифровым нулём: %x", digest.Sum(nil))
			}
		})
	}
}

// TestPreparedAudioPeriodRejectsWholeMissingAndExtraUnits проверяет через t независимый
// периодический контракт: корректные заголовки, AU и полное видео не скрывают одну потерянную или лишнюю Opus AU.
func TestPreparedAudioPeriodRejectsWholeMissingAndExtraUnits(t *testing.T) {
	// fixture задаёт измеренный период 0.1 s с тремя видеокадрами и сорока Opus AU120.
	fixture := audioPreparationFixtures()[0]
	// profile задаёт конечный рецепт, prober подставляет параметры дорожек и контрольную Duration.
	profile := audioBindingProfile(t, fixture.kind)
	prober := audioPreparationProber(t, audioPreparationDocument(t, fixture))
	// normalized принимает полную собственную нормализацию эталона до внесения дефекта.
	var normalized bytes.Buffer
	// err сообщает об отказе исходно корректной подготовки.
	if err := prober.NormalizePreparedForProfile(context.Background(), audioPreparationFile(t, audioPreparationBytes(t, fixture)), &normalized, profile); err != nil {
		t.Fatal(err)
	}
	// init содержит восстановленные конфигурации, audio и video — действительные AU готового файла.
	init, audio := audioPreparationPackets(t, normalized.Bytes(), media.TrackAudio, false)
	_, video := audioPreparationPackets(t, normalized.Bytes(), media.TrackVideo, false)
	// scenario выбирает ровно одну целую недостающую либо лишнюю AU, оставляя видео полным.
	for _, scenario := range []string{"missing-unit", "extra-unit"} {
		// Подтест t проверяет полный синтаксически корректный контейнер выбранного scenario с неверным периодом.
		t.Run(scenario, func(t *testing.T) {
			// changed содержит изменённый самостоятельный контейнер, mux сохраняет реальные конфигурации/AU.
			var changed bytes.Buffer
			// err сообщает об отказе создания или записи корректного обрамления контейнера.
			mux, err := fmp4.NewMuxer(&changed, init, selfProbeLimits().Container)
			if err != nil {
				t.Fatal(err)
			}
			// packet сохраняет каждый видеокадр полного периода без изменения сжатых байтов и времени.
			for _, packet := range video {
				if err = mux.WritePacket(packet); err != nil {
					t.Fatal(err)
				}
			}
			// count задаёт число полных аудиоединиц в контролируемом неправильном периоде.
			count := 39
			if scenario == "extra-unit" {
				count = 41
			}
			// index задаёт следующую AU на непрерывной сетке; последняя может повторить законную сжатую тишину.
			for index := range count {
				// packet использует только настоящую CELT120 AU, а не произвольные байты с нужной duration.
				packet := audioPreparationPacket(t, init.Audio, media.PacketInfo{PTS: int64(index) * 120, DTS: int64(index) * 120, DurationTicks: 120}, audio[min(index, len(audio)-1)].Payload())
				if err = mux.WritePacket(packet); err != nil {
					t.Fatal(err)
				}
			}
			// file представляет новый контейнер с целыми AU и отличающимся только числом аудиоединиц.
			file := audioPreparationFile(t, changed.Bytes())
			if _, err = prober.Verify(context.Background(), file, profile); err != nil {
				t.Fatal("контрольная синтаксическая проверка AU", err)
			}
			if err = prober.VerifyLoopDuration(context.Background(), file, fixture.period); !errors.Is(err, core.ErrNotReady) {
				t.Fatal("неполный либо лишний период принят", err)
			}
		})
	}
	// info намеренно ложно объявляет зависимый P-frame независимым контейнерным флагом.
	info := video[1].Info()
	info.Keyframe = true
	// err проверяет фактическую независимость свежего видео по AU, независимо от аудио и флага контейнера.
	if _, _, err := freshPreparedVideo(profile, init.Video, audioPreparationPacket(t, init.Video, info, video[1].Payload()), core.MediaFormat{Video: init.Video.Info().Video, Audio: init.Audio.Info().Audio}, nil); !errors.Is(err, core.ErrNotReady) {
		t.Fatal("зависимое первое видео принято по ложному keyframe", err)
	}
}

// TestPreparedAudioHeadersAndCountersRejectUnfinishedNormalization проверяет через t
// исходный Head120, несогласованные STREAMINFO и размер/счётчик подготовленного FLAC.
// Прежний AAC по-прежнему сохраняет свой отдельный допуск неизвестной duration0.
func TestPreparedAudioHeadersAndCountersRejectUnfinishedNormalization(t *testing.T) {
	// fixture выбирает измеренные заголовки и настоящие AU конкретного аудиокодека.
	for _, fixture := range audioPreparationFixtures() {
		// document задаёт подставленный ответ ffprobe; audio содержит измеренное описание аудиодорожки.
		document := audioPreparationDocument(t, fixture)
		audio := document.Streams[1]
		// actual/err показывают восстановленную привязку без подстановки желаемого профиля.
		actual, err := audio.audio()
		if err != nil || actual != audioBindingProfile(t, fixture.kind).Format.Audio {
			t.Fatal("actual probe binding", actual, err)
		}
		// scenario определяет неверное обязательное поле либо неподтверждённую extradata.
		for _, scenario := range []string{"rate", "channels", "layout", "profile", "headers"} {
			// Подтест t меняет выбранный scenario только в аудиометаданных fixture и требует отказа.
			t.Run(string(fixture.kind)+"/"+scenario, func(t *testing.T) {
				// bad сохраняет отдельную копию измеренных метаданных для проверки отказа.
				bad := audio
				switch scenario {
				case "rate":
					bad.SampleRate = "44100"
				case "channels":
					bad.Channels = 1
				case "layout":
					bad.Layout = "mono"
				case "profile":
					bad.Profile = "unsupported"
				case "headers":
					bad.ExtraData, bad.ExtraDataSize = audioPreparationDump(t, "1190")
				}
				// parseErr сообщает о несогласованных измеренных полях либо заголовках, без подстановки желаемого формата.
				if _, parseErr := bad.audio(); parseErr == nil {
					t.Fatal("неподтверждённые metadata приняты")
				}
			})
		}
	}
	// opus содержит исходную нативную Head120; decodeErr сообщает о неверной записи эталона.
	opus, decodeErr := hex.DecodeString(audioPreparationFixtures()[0].headers)
	if decodeErr != nil {
		t.Fatal(decodeErr)
	}
	// expected сохраняет исходные байты для проверки отсутствия скрытого исправления Head120.
	expected := bytes.Clone(opus)
	// err фиксирует отказ проверки подготовленного файла до пропуска целой начальной AU; заголовок не меняется.
	if _, err := preparedHeaders(fmp4.TrackDescription{Track: media.TrackAudio, Codec: core.CodecOpus, Headers: opus}); !errors.Is(err, core.ErrNotReady) || !bytes.Equal(opus, expected) {
		t.Fatal("raw Head120 принят либо изменён", err)
	}
	// flac выбирает полные AU4608 с фиксированными блоками исходного самостоятельного файла.
	flac := audioPreparationFixtures()[1]
	// packets содержит нативные кадры frame0/frame1 с реальными CRC и фиксированными номерами.
	_, packets := audioPreparationPackets(t, audioPreparationBytes(t, flac), media.TrackAudio, false)
	// err сообщает о невозможности подтвердить действительно полную frame0.
	if err := verifyPreparedAudio(packets[0], 0); err != nil {
		t.Fatal("полная frame0 отвергнута", err)
	}
	// err сообщает, что реальный frame1 нельзя объявить начальным frame0 готового файла.
	if err := verifyPreparedAudio(packets[1], 0); !errors.Is(err, core.ErrCorrupt) {
		t.Fatal("непоследовательный FLAC counter принят", err)
	}
	// shortInfo меняет только контейнерную длительность, оставляя полную фактическую AU4608.
	shortInfo := packets[0].Info()
	shortInfo.DurationTicks = 48
	// err сохраняет отказ несогласованности контейнера и AU, несмотря на разрешённые обрабатываемые блоки из 48 отсчётов.
	if err := verifyPreparedAudio(audioPreparationPacket(t, packets[0].Config(), shortInfo, packets[0].Payload()), 0); err == nil {
		t.Fatal("partial prepared FLAC duration принята")
	}
	// shortAU содержит нативную FLAC AU из 48 нулевых отсчётов после отдельного кодирования -t0.001 FFmpeg7.1.5,
	// без изменений CRC, подкадров и номера frame0; decodeErr сообщает об ошибке записи байтов.
	shortAU, decodeErr := hex.DecodeString("fff86a18002f4800000000000044aa")
	if decodeErr != nil {
		t.Fatal(decodeErr)
	}
	// samples/parseErr доказывают, что это разрешённая обрабатываемая AU48, а не просто битое аудио.
	samples, parseErr := codec.AudioSamples(core.CodecFLAC, packets[0].Config().Headers(), shortAU)
	if parseErr != nil || samples != 48 {
		t.Fatal("native processed48 не подтверждена", samples, parseErr)
	}
	// err различает допустимую обрабатываемую AU48 и обязательную файловую AU4608.
	if err := verifyPreparedAudio(audioPreparationPacket(t, packets[0].Config(), shortInfo, shortAU), 0); !errors.Is(err, core.ErrCorrupt) {
		t.Fatal("short actual prepared FLAC AU принята", err)
	}
	// state проверяет ту же фактическую AU48 на входе нормализатора свежего результата до вывода готового файла.
	state := freshSilentAudio{config: packets[0].Config()}
	// err сообщает об отказе короткой собственной FLAC AU, а не о запрете обрабатываемого блока из 48 отсчётов в целом.
	if _, err := state.packet(audioPreparationPacket(t, packets[0].Config(), shortInfo, shortAU)); !errors.Is(err, core.ErrCorrupt) {
		t.Fatal("fresh short FLAC AU нормализована", err)
	}
	// aac сохраняет старую привязку AAC LC; err сообщает об отказе создания её конфигурации.
	aac, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackAudio, SourceEpoch: 1, Revision: 1, TimeBase: media.TimeBase{Numerator: 1, Denominator: 48000}, Audio: selfProfile(core.CodecH264).Format.Audio}, []byte{0x11, 0x90})
	if err != nil {
		t.Fatal(err)
	}
	if err = verifyPreparedAudio(audioPreparationPacket(t, aac, media.PacketInfo{}, []byte{0x21, 0x10, 4, 0x60, 0x8c, 0x1c}), 0); err != nil {
		t.Fatal("изменён прежний AAC duration0 допуск", err)
	}
}

// audioPreparationBox задаёт границы одного полного стандартного блока ISO BMFF в тестовом буфере.
type audioPreparationBox struct {
	// kind содержит FourCC; begin и end ограничивают блок вместе с его восьмибайтовым заголовком.
	kind  string
	begin int
	end   int
}

// audioPreparationBoxes разбирает полный список обычных блоков с 32-битными размерами в data.
// t отклоняет иной формат фикстуры до изменения байтов; результат не ищет FourCC внутри сжатого содержимого.
func audioPreparationBoxes(t *testing.T, data []byte) []audioPreparationBox {
	t.Helper()
	// boxes сохраняет реальные границы блоков, offset — начало очередного блока.
	var boxes []audioPreparationBox
	for offset := 0; offset < len(data); {
		if len(data)-offset < 8 {
			t.Fatal("усечённый заголовок fixture box")
		}
		// size задаёт полный объявленный размер в байтах после проверки доступного заголовка.
		size := int(binary.BigEndian.Uint32(data[offset : offset+4]))
		if size < 8 || size > len(data)-offset {
			t.Fatal("неподдержанный размер fixture box", size)
		}
		boxes = append(boxes, audioPreparationBox{string(data[offset+4 : offset+8]), offset, offset + size})
		offset += size
	}
	return boxes
}

// audioPreparationWithoutFinalAudioFragment удаляет из data последнюю целую пару moof/mdat с аудиодорожкой track2,
// сохраняя остальные стандартные блоки. Этот moof может также содержать видеокадр; t проверяет структуру frag_every_frame перед внесением дефекта.
// Получается целый контейнер с потерянным фрагментом, а не случайно усечённый служебный индекс.
func audioPreparationWithoutFinalAudioFragment(t *testing.T, data []byte) []byte {
	t.Helper()
	// boxes ограничивает фрагменты верхнего уровня; begin/end запоминают последнюю пару с аудиодорожкой.
	boxes := audioPreparationBoxes(t, data)
	begin, end := -1, -1
	// index выбирает блок верхнего уровня, box — его проверенные границы.
	for index, box := range boxes {
		if box.kind != "moof" {
			continue
		}
		// moof содержит дочерние блоки только текущего фрагмента.
		moof := data[box.begin+8 : box.end]
		// child выбирает настоящий дочерний traf, не совпадение байтов в сжатом кадре.
		for _, child := range audioPreparationBoxes(t, moof) {
			if child.kind != "traf" {
				continue
			}
			// traf содержит FullBox заголовки ровно этой дорожки.
			traf := moof[child.begin+8 : child.end]
			// header перебирает реальные дочерние блоки traf для чтения track_ID из tfhd.
			for _, header := range audioPreparationBoxes(t, traf) {
				if header.kind != "tfhd" {
					continue
				}
				if header.end-header.begin < 16 {
					t.Fatal("усечённый fixture tfhd")
				}
				if binary.BigEndian.Uint32(traf[header.begin+12:header.begin+16]) == 2 {
					if index+1 >= len(boxes) || boxes[index+1].kind != "mdat" {
						t.Fatal("fixture не имеет отдельного mdat после audio moof")
					}
					begin, end = box.begin, boxes[index+1].end
				}
			}
		}
	}
	if begin < 0 {
		t.Fatal("fixture не содержит audio track2")
	}
	return append(bytes.Clone(data[:begin]), data[end:]...)
}

// audioPreparationFailedOutput воспроизводит причинный отказ получателя байтов промежуточного файла.
type audioPreparationFailedOutput struct{}

// Write отклоняет data с io.ErrClosedPipe до записи; содержимое не сохраняется.
func (audioPreparationFailedOutput) Write(data []byte) (int, error) {
	return 0, io.ErrClosedPipe
}

// TestPreparedAudioNormalizationRejectsIncompleteFilesAndFailures проверяет через t,
// что ни потерянный медиафрагмент, ни усечение, отмена, несогласованный профиль и бюджет
// не превращаются в успешный подготовленный период. Проверяется причинная ошибка вывода.
func TestPreparedAudioNormalizationRejectsIncompleteFilesAndFailures(t *testing.T) {
	// fixture выбирает конечную привязку с настоящими заголовками и AU.
	for _, fixture := range audioPreparationFixtures() {
		// scenario выбирает доказуемый дефект периода, контейнера, метаданных или внешнего получателя.
		for _, scenario := range []string{"missing-av-fragment", "truncated-container", "wrong-profile", "wrong-probe-profile", "packet-budget", "output-budget", "output-failure", "cancel"} {
			// Подтест t применяет выбранный scenario к fixture и требует отказа до успешной подготовки.
			t.Run(string(fixture.kind)+"/"+scenario, func(t *testing.T) {
				// data сохраняет изменяемую копию исходного нативного контейнера.
				data := audioPreparationBytes(t, fixture)
				if scenario == "missing-av-fragment" {
					data = audioPreparationWithoutFinalAudioFragment(t, data)
				} else if scenario == "truncated-container" {
					// box выбирает первую настоящую медианагрузку; индекс mfra после AU здесь не затрагивается.
					for _, box := range audioPreparationBoxes(t, data) {
						if box.kind == "mdat" {
							data = data[:box.end-1]
							break
						}
					}
				}
				// document содержит подставленный ответ ffprobe, profile — ожидаемый действительный рецепт.
				document := audioPreparationDocument(t, fixture)
				profile := audioBindingProfile(t, fixture.kind)
				if scenario == "wrong-probe-profile" {
					document.Streams[1].Profile = "unknown"
				}
				if scenario == "wrong-profile" {
					profile = audioBindingProfile(t, core.CodecOpus)
					if fixture.kind == core.CodecOpus {
						profile = audioBindingProfile(t, core.CodecMP3)
					}
				}
				// prober проверяет настоящий контейнер с подставленным ответом внешнего ffprobe.
				prober := audioPreparationProber(t, document)
				if scenario == "packet-budget" {
					prober.Limits.MaxPackets = 1
				}
				// output принимает байты без публикации; бюджет и причинный отказ меняют лишь внешний получатель.
				output := io.Discard
				switch scenario {
				case "output-budget":
					output = &boundedWriter{w: io.Discard, remaining: 8}
				case "output-failure":
					output = audioPreparationFailedOutput{}
				}
				// ctx/cancel ограничивают текущую попытку, включая отмену до первого чтения.
				ctx, cancel := context.WithCancel(context.Background())
				defer cancel()
				if scenario == "cancel" {
					cancel()
				}
				// err сохраняет фактический отказ компонента на испорченных входах.
				err := prober.NormalizePreparedForProfile(ctx, audioPreparationFile(t, data), output, profile)
				if err == nil {
					t.Fatal("неполный либо неподтверждённый файл принят")
				}
				if scenario == "output-failure" && !errors.Is(err, io.ErrClosedPipe) || scenario == "cancel" && !errors.Is(err, context.Canceled) || (scenario == "packet-budget" || scenario == "output-budget") && !errors.Is(err, ErrLimit) {
					t.Fatal("утрачена причинная ошибка", err)
				}
			})
		}
	}
}

// audioPreparationPacket создаёт для t пакет с конфигурацией config, заданными метками info и сжатыми байтами payload.
// Отдельный неизменяемый пакет позволяет испортить время либо AU без изменения общей фикстуры.
func audioPreparationPacket(t *testing.T, config media.CodecConfig, info media.PacketInfo, payload []byte) media.Packet {
	t.Helper()
	// packet сохраняет независимые данные; err сообщает об ошибке самого тестового входа.
	packet, err := media.NewPacket(info, config, payload)
	if err != nil {
		t.Fatal(err)
	}
	return packet
}

// TestFreshSilentAudioRejectsNonterminalPartialUnitsAndUnprovenClock независимо проверяет
// через t временной контракт фактической AU: допустимый частичный хвост нативного контейнера не разрешает следующую AU,
// а неверное начало, duration, PTS/DTS, разрыв и повреждение битстрима не нормализуются успешно.
func TestFreshSilentAudioRejectsNonterminalPartialUnitsAndUnprovenClock(t *testing.T) {
	// fixture определяет полный размер AU и настоящие сжатые байты каждого отдельного сценария.
	for _, fixture := range audioPreparationFixtures() {
		// scenario выбирает конкретный контрпример временного контракта либо требований к сжатым байтам.
		for _, scenario := range []string{"positive-start", "oversize-duration", "pts-differs", "clock-gap", "damaged-au", "partial-nonterminal"} {
			// Подтест t проверяет выбранный scenario с реальной AU fixture и новым состоянием нормализатора.
			t.Run(string(fixture.kind)+"/"+scenario, func(t *testing.T) {
				// init/packets восстанавливают исходные AU нативного кодировщика с разрешёнными только здесь свежими хвостами.
				init, packets := audioPreparationPackets(t, audioPreparationBytes(t, fixture), media.TrackAudio, true)
				// state принадлежит одному синтетическому запуску кодировщика и не переносится между сценариями.
				state := freshSilentAudio{config: init.Audio}
				// first сохраняет фактические начальные метки, payload — реальную первую AU без мутации файла.
				first := packets[0].Info()
				payload := packets[0].Payload()
				switch scenario {
				case "positive-start":
					first.DTS, first.PTS = 1, 1
				case "oversize-duration":
					first.DurationTicks = fixture.quantum + 1
				case "pts-differs":
					first.PTS++
				case "damaged-au":
					switch fixture.kind {
					case core.CodecOpus:
						payload[0] |= 1 // Код упаковки 1 означает два кадра вместо одной CELT120 AU.
					case core.CodecMP3:
						payload = payload[:len(payload)-1]
					default:
						payload[len(payload)-1] ^= 1 // Сжатые данные больше не согласованы с CRC.
					}
				case "partial-nonterminal":
					first.DurationTicks = 1
				}
				// result/err наблюдают первый переход, включая единственное допустимое MP3 nil при удалении начальной AU.
				result, err := state.packet(audioPreparationPacket(t, packets[0].Config(), first, payload))
				if scenario == "clock-gap" || scenario == "partial-nonterminal" && (fixture.kind == core.CodecOpus || fixture.kind == core.CodecMP3) {
					if err != nil || (fixture.kind == core.CodecMP3) != (result == nil) {
						t.Fatal("неверный начальный переход", result, err)
					}
					// second продолжает нативную последовательность; разрыв вводится ровно на один отсчёт декодера.
					second := packets[1].Info()
					if scenario == "clock-gap" {
						second.DTS++
						second.PTS++
					}
					_, err = state.packet(audioPreparationPacket(t, packets[1].Config(), second, packets[1].Payload()))
				}
				if err == nil {
					t.Fatal("неподтверждённая AU/time принята")
				}
			})
		}
	}
}

// TestFreshAC3SilenceRejectsActualTone проверяет через t требование фактического нулевого сигнала свежей AC3-тишины.
// Эталон получен FFmpeg 7.1.5 из sine997/48k/stereo с ac3/192k/coupling0/rematrix0/-t0.032;
// его корректные BSI/обе CRC проходят обычную проверку обрабатываемого потока, но коэффициенты тона запрещают исправление.
func TestFreshAC3SilenceRejectsActualTone(t *testing.T) {
	// tone содержит полную нативную AU1536 со слышимым сигналом; err сообщает о повреждении записи.
	tone, err := hex.DecodeString("0b771449144043e106a1f86127bdd87d5df577cfabbe7cf9fbe7cf9f127bdd87d5df577cfabbe7cf9fbe7cf9f197f68a141ecc26f943533d7329359776eb0ebbea73f5d80ec3a1cb1cfe7fa19d2b9aa2540ca6fb56b6ae5463ba833774567cb6914164c7a63252d3b52845dcc04076c7923466d1a02b5ad9ac2450e0b51e48e5bd1940e9c32aef6c824db8268798b968462deb5052aef60427f1683a4f574322f77c2a0f98433cf3142bafb24135fb30133ff380300147fad014ff2c0557eaa075fea80967e270b6fe260d73da40d77c8163c81617a417a417a417a41ecc26f943533d7329359776eb0ebbea73f5d80ec3a1cb1cfe7fa19d2b9aa2540ca6fb56b6ae5463ba833774567cb6914164c7a63252d3b52845dcc04076c7923466d1a02b5ad9ac2450e0b51e48e5bd1940e9c32aef6c824db8268798b968462deb5052aef60427f1683a4f574322f77c2a0f98433cf3142bafb24135fb30133ff380300147fad014ff2c0557eaa075fea80967e270b6fe260d73da40d77c8163c81617a417a417a417a40c2e187f30065663f9221437afb54286a6956aef92cea709d3da50ab53830dcd7530a95286f9f3da519edb7ef92c37ca600d749a12a749a8bf1ce9f29aeba6a77b5dc9fcc019598fe48850debed50a1a9a55abbe4b3a9c274f6942ad4e0c3735d4c2a54a1be7cf69467b6dfbe4b0df298035d2684a9d26a2fc73a7ca6bae9a9ded77200321f180bdbea6f53db445cfda9637df64c3d2de1233234ed05f180bdbea6f53db445cfda9637df64c3d2c91991a76980001e304027fd67c7fe96d4ffa5a1b0092f002e3bc6b5d018e00304027fd67c7fe96d4ffa5a1b0092f002e3b5ae80c70300003c835fc09dabdab46d728caeac91fbda4f34378da398d6b80835fc09dabdab46d728caeac91fbda4f3436d1cc6b5c6000078f9801f0aeabb96c32b1a7ba6d6e383505b4f2c78aeada80f9801f0aeabb96c32b1a7ba6d6e383505b563c5756d4c0002f13d013f2b175c70ae2f466c72128166416e1e3d596c5ae053d013f2b175c70ae2f466c72128166416e1eacb62d7000474e")
	if err != nil {
		t.Fatal(err)
	}
	// fixture выбирает измеренную привязку AC3; init восстанавливает фактическую конфигурацию собственной тишины.
	fixture := audioPreparationFixtures()[3]
	init, _ := audioPreparationPackets(t, audioPreparationBytes(t, fixture), media.TrackAudio, false)
	// samples/parseErr подтверждают законную AU с обеими CRC до проверки спектрального нуля.
	samples, parseErr := codec.AudioSamples(core.CodecAC3, init.Audio.Headers(), tone)
	if parseErr != nil || samples != 1536 {
		t.Fatal("тон не является полным processed AC3", samples, parseErr)
	}
	// original сохраняет байты слышимого аудио, которое нормализатор не вправе изменить.
	original := bytes.Clone(tone)
	// state представляет ровно одну собственную свежую попытку и не переносится к LIVE/copy.
	state := freshSilentAudio{config: init.Audio}
	// err сообщает об отказе ненулевых коэффициентов после успешной проверки BSI/CRC.
	if _, err := state.packet(audioPreparationPacket(t, init.Audio, media.PacketInfo{PTS: -256, DTS: -256, DurationTicks: 1536}, tone)); err == nil {
		t.Fatal("слышимый тон нормализован как тишина")
	}
	if !bytes.Equal(tone, original) {
		t.Fatal("изменены входные AU тона")
	}
}
