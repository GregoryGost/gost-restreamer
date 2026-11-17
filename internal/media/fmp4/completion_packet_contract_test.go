package fmp4

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
)

// vp9PacketFixture возвращает две действительные AU и init с измеренными 64×64/30.
// t проверяет буквальные записи; время пакетов в последующих проверках задаётся отдельно от Muxer.
func vp9PacketFixture(t *testing.T) (Init, []byte, []byte) {
	t.Helper()
	// record задаёт VPCodecConfigurationRecord без FullBox; key/inter — реальные сжатые кадры.
	record := []byte{0, 10, 0x82, 2, 2, 1, 0, 0}
	key, _ := hex.DecodeString("824983424003f003f60038241c184a00003060000078b9ffff57b0ffffff7c17fffffffc78c1cfd2e880")
	inter, _ := hex.DecodeString("860040929c00500000036000005470")
	// actual и err подтверждают корректность сжатой фикстуры до проверки контейнерного времени.
	actual, _, err := codec.InspectVP9AU(record, key, core.FrameRate{Numerator: 30, Denominator: 1})
	if err != nil || actual.Width != 64 || actual.Height != 64 {
		t.Fatal("Некорректная VP9-фикстура", actual, err)
	}
	// init сохраняет отдельную корректную аудиодорожку и нулевой сдвиг начала.
	init := testInit(t)
	init.Origin = OriginMapping{}
	init.Video, err = media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: 11, Revision: 1, TimeBase: media.TimeBase{Numerator: 1, Denominator: 30}, Video: actual}, record)
	if err != nil {
		t.Fatal(err)
	}
	return init, key, inter
}

// vp9RawFragment формирует один moof/mdat с буквальным trun v1 и signed CTS.
// sequence задаёт номер фрагмента, dts — такт 1/30, cts — PTS−DTS; payload остаётся неизменным.
// key выбирает sample flags независимого старта. Muxer.WritePacket здесь не участвует.
func vp9RawFragment(sequence uint32, dts uint64, cts int32, key bool, payload []byte) []byte {
	// sampleFlags задаёт нормативное различие sync/inter sample в контейнере.
	sampleFlags := uint32(0x01010000)
	if key {
		sampleFlags = 0x02000000
	}
	// trun содержит одну запись duration=1, size, flags, signed composition offset и место data_offset.
	trun := full("trun", 1, 0xf01, u32(1), u32(0), u32(1), u32(uint32(len(payload))), u32(sampleFlags), u32(uint32(cts)))
	// moof задаёт video track 1 и явную moof-relative базу; offset указывает на поле data_offset.
	moof := box("moof", full("mfhd", 0, 0, u32(sequence)), box("traf", full("tfhd", 0, 0x020000, u32(1)), full("tfdt", 1, 0, u64(dts)), trun))
	offset := len(moof) - len(trun) + 16
	be.PutUint32(moof[offset:offset+4], uint32(len(moof)+8))
	return append(moof, box("mdat", payload)...)
}

// vp9RawStream пишет только init штатным Muxer и добавляет независимо построенные два фрагмента.
// t проверяет init, uuid оставляет собственный полный формат, firstCTS/secondCTS меняют лишь время AU.
func vp9RawStream(t *testing.T, uuid bool, firstCTS, secondCTS int32) ([]byte, Init, []byte, []byte) {
	t.Helper()
	// init/key/inter содержат валидную конфигурацию и неизменные AU.
	init, key, inter := vp9PacketFixture(t)
	// output получает только согласованные заголовки до ручного построения trun.
	var output bytes.Buffer
	// err сообщает об отказе самой фикстуры, а не проверяемых композиционных смещений.
	if _, err := NewMuxer(&output, init, testLimits()); err != nil {
		t.Fatal(err)
	}
	// raw отделяет тестовый транспорт от буфера init; удаление UUID включает actual completion.
	raw := bytes.Clone(output.Bytes())
	if !uuid {
		raw = withoutConfigUUID(t, raw)
	}
	raw = append(raw, vp9RawFragment(1, 0, firstCTS, true, key)...)
	raw = append(raw, vp9RawFragment(2, 1, secondCTS, false, inter)...)
	return raw, init, key, inter
}

// TestVP9DemuxRequiresEqualTimesOnEveryPacket проверяет complete UUID, полный resolver и completion.
// t требует отказа signed CTS±1 как на первом, так и на следующем AU; нулевой CTS сохраняет AU/время.
func TestVP9DemuxRequiresEqualTimesOnEveryPacket(t *testing.T) {
	// path выбирает три способа получить фактический формат без изменения сжатых данных.
	for _, path := range []string{"uuid", "resolver", "incomplete"} {
		// scenario задаёт независимые положительный контроль и четыре нарушения времени пакетов.
		for _, scenario := range []struct {
			// name различает границу; first/second задают CTS первого и последующего AU.
			name          string
			first, second int32
		}{
			{"zero", 0, 0}, {"first-positive", 1, 0}, {"first-negative", -1, 0},
			{"next-positive", 0, 1}, {"next-negative", 0, -1},
		} {
			// Подтест через t проверяет конкретный путь и единственную контролируемую мутацию CTS.
			t.Run(path+"/"+scenario.name, func(t *testing.T) {
				// raw/init/key/inter формируют действительный VP9 с явно заданными временными метками trun.
				raw, init, key, inter := vp9RawStream(t, path == "uuid", scenario.first, scenario.second)
				// options задаёт ту же идентичность и при необходимости полный пользовательский resolver.
				options := DemuxOptions{Limits: testLimits(), SourceEpoch: 11, VideoRevision: 1, AudioRevision: 7}
				if path == "resolver" {
					// Функция принимает desc как фактическое описание; видео уже независимо проверено фикстурой.
					options.ResolveConfig = func(desc TrackDescription) (media.CodecConfig, error) {
						if desc.Track == media.TrackVideo {
							return init.Video, nil
						}
						return ResolveCodecConfig(desc, 11, 7)
					}
				}
				// demux и err позволяют отклонить нарушение при ограниченном уточнении описания либо до выдачи Next.
				demux, err := NewDemuxer(bytes.NewReader(raw), options)
				// rejected определяет отрицательную ветвь из исходного контрактного ввода.
				rejected := scenario.first != 0 || scenario.second != 0
				if err != nil {
					if !rejected || !errors.Is(err, ErrUnsupportedTiming) {
						t.Fatalf("Неверный отказ инициализации: %v", err)
					}
					return
				}
				// index и expected проверяют оба AU; ошибочный AU нельзя выдать даже перед последующим отказом.
				for index, expected := range [][]byte{key, inter} {
					// got/readErr содержат первый фактический результат выдачи каждого AU.
					got, readErr := demux.Next()
					if readErr != nil {
						if rejected && errors.Is(readErr, ErrUnsupportedTiming) {
							return
						}
						t.Fatalf("Неожиданный отказ AU%d: %v", index, readErr)
					}
					if got.Info().PTS != got.Info().DTS {
						t.Errorf("Выдан VP9 AU%d с PTS=%d DTS=%d", index, got.Info().PTS, got.Info().DTS)
					}
					if !bytes.Equal(got.Payload(), expected) || got.Info().DTS != int64(index) || got.Info().DurationTicks != 1 || got.Info().Keyframe != (index == 0) {
						t.Fatalf("Изменены AU%d, независимость либо время: %+v", index, got.Info())
					}
				}
				if rejected {
					t.Fatal("Контейнер с ненулевым VP9 CTS выдан без ErrUnsupportedTiming")
				}
				if _, err = demux.Next(); !errors.Is(err, io.EOF) {
					t.Fatal(err)
				}
			})
		}
	}
}

// TestCompletionResolverCannotRetainInternalBuffers проверяет копии на обоих вызовах resolver.
// t меняет буферы, удержанные функцией после конструктора; Headers/Color/FirstSample не должны менять описания, конфигурацию и AU.
func TestCompletionResolverCannotRetainInternalBuffers(t *testing.T) {
	// raw/init/key содержат внешний VP9 без UUID, который требует измерить первый AU.
	raw, init, key, _ := vp9RawStream(t, false, 0, 0)
	// retained сохраняет переданные функции описания для изменения после завершения NewDemuxer.
	var retained []TrackDescription
	// options использует штатный resolver, сохраняя только копии его фактических аргументов.
	options := DemuxOptions{Limits: testLimits(), SourceEpoch: 11, VideoRevision: 1, AudioRevision: 7}
	// Функция удерживает desc каждого вызова для видео; первый возвращает MetadataUnavailable.
	options.ResolveConfig = func(desc TrackDescription) (media.CodecConfig, error) {
		if desc.Track == media.TrackVideo {
			retained = append(retained, desc)
		}
		return ResolveCodecConfig(desc, 11, 1)
	}
	// demux и err подтверждают успешное уточнение формата без изменения аргументов во время вызова функции.
	demux, err := NewDemuxer(bytes.NewReader(raw), options)
	if err != nil || len(retained) != 2 {
		t.Fatalf("Ожидались initial и completion вызовы: %d, %v", len(retained), err)
	}
	// before хранит публичные отдельные копии до изменения удержанных буферов.
	before := demux.Descriptions()
	// description обозначает один из двух вызовов; data — каждый изменяемый срез этого вызова.
	for _, description := range retained {
		for _, data := range [][]byte{description.Headers, description.Color, description.FirstSample} {
			// index выбирает байт, который вызывающая сторона вправе менять в собственной копии.
			for index := range data {
				data[index] ^= 0xff
			}
		}
	}
	// after содержит повторно прочитанные описания; index выбирает дорожку, description — её новое описание.
	after := demux.Descriptions()
	for index, description := range after {
		if !bytes.Equal(description.Headers, before[index].Headers) {
			t.Errorf("Resolver изменил внутренние Headers дорожки %d: %x", description.ID, description.Headers)
		}
		if !bytes.Equal(description.Color, before[index].Color) {
			t.Errorf("Resolver изменил внутренние Color дорожки %d: %x", description.ID, description.Color)
		}
	}
	if !bytes.Equal(demux.Init().Video.Headers(), init.Video.Headers()) {
		t.Error("Изменилась неизменяемая конфигурация видео")
	}
	// first содержит удержанный AU, readErr сообщает об отказе его чтения после изменения FirstSample.
	first, readErr := demux.Next()
	if readErr != nil || !bytes.Equal(first.Payload(), key) || first.Info() != (media.PacketInfo{DTS: 0, PTS: 0, DurationTicks: 1, Keyframe: true}) {
		t.Fatalf("Изменён удержанный независимый AU: %+v, %v", first.Info(), readErr)
	}
}
