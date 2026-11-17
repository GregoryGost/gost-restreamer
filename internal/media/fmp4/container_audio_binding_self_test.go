package fmp4

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"os"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
)

// audioBindingFixture возвращает буквальные headers и AU нативного FFmpeg 7.1.5 для kind.
// t останавливает тест при повреждении встроенного hex; samples задаёт фактический период AU.
func audioBindingFixture(t *testing.T, kind core.Codec) ([]byte, []byte, uint32) {
	t.Helper()
	// headers и payload задают нормативную конфигурацию и одну нативную AU; samples — её число отсчётов при 48 кГц.
	var headers, payload string
	var samples uint32
	switch kind {
	case core.CodecOpus:
		headers, payload, samples = "4f707573486561640102000080bb0000000000", "e4fffe", 120
	case core.CodecFLAC:
		headers, payload, samples = "12001200000000004a580bb802f00000000000000000000000000000000000000000", "fff85a1800d60000000000000666", 4608
	case core.CodecMP3:
		headers, payload, samples = "fffb9464", "fffb9464000ff00000690000000800000d20000001000001a400000020000034800000044c414d45342e300000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000000", 1152
	case core.CodecAC3:
		headers, payload, samples = "101140", "0b771afb144043e106a0b861ff3abe7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7c7fceaf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f3e7cf9f197f872640000000078db6db6db6db78f1e3bbbbbbbbbbbbbbbbbbbbbbbbbbbbc78f1b6db7cf9ad6b5ad6b5ad6b5ad6b5ad6b5ad6b5ad6b4000000078db6db6db6db78f1e3bbbbbbbbbbbbbbbbbbbbbbbbbbbbc78f1b6db7cf9ad6b5ad6b5ad6b5ad6b5ad6b5ad6b5ad6b4c0000000000f1b6db6db6db6f1e3c777777777777777777777777777778f1e36db6f9f35ad6b5ad6b5ad6b5ad6b5ad6b5ad6b5ad680000000f1b6db6db6db6f1e3c777777777777777777777777777778f1e36db6f9f35ad6b5ad6b5ad6b5ad6b5ad6b5ad6b5ad6980000000001e36db6db6db6de3c78eeeeeeeeeeeeeeeeeeeeeeeeeeeef1e3c6db6df3e6b5ad6b5ad6b5ad6b5ad6b5ad6b5ad6b5ad00000001e36db6db6db6de3c78eeeeeeeeeeeeeeeeeeeeeeeeeeeef1e3c6db6df3e6b5ad6b5ad6b5ad6b5ad6b5ad6b5ad6b5ad300000000003c6db6db6db6dbc78f1dddddddddddddddddddddddddddde3c78db6dbe7cd6b5ad6b5ad6b5ad6b5ad6b5ad6b5ad6b5a00000003c6db6db6db6dbc78f1dddddddddddddddddddddddddddde3c78db6dbe7cd6b5ad6b5ad6b5ad6b5ad6b5ad6b5ad6b5a6000000000078db6db6db6db78f1e3bbbbbbbbbbbbbbbbbbbbbbbbbbbbc78f1b6db7cf9ad6b5ad6b5ad6b5ad6b5ad6b5ad6b5ad6b4000000078db6db6db6db78f1e3bbbbbbbbbbbbbbbbbbbbbbbbbbbbc78f1b6db7cf9ad6b5ad6b5ad6b5ad6b5ad6b5ad6b5ad6b4c0000000000f1b6db6db6db6f1e3c777777777777777777777777777778f1e36db6f9f35ad6b5ad6b5ad6b5ad6b5ad6b5ad6b5ad680000000f1b6db6db6db6f1e3c777777777777777777777777777778f1e36db6f9f35ad6b5ad6b5ad6b5ad6b5ad6b5ad6b5ad680c3c0", 1536
	default:
		t.Fatal("Неизвестная тестовая привязка", kind)
	}
	// h и err проверяют правильность буквальной записи до выполнения тестируемого разбора.
	h, err := hex.DecodeString(headers)
	if err != nil {
		t.Fatal(err)
	}
	// p содержит отдельные буквальные байты нативной AU.
	p, err := hex.DecodeString(payload)
	if err != nil {
		t.Fatal(err)
	}
	return h, p, samples
}

// audioBindingInit заменяет только аудио testInit на фактический kind с переданными headers.
// t проверяет структурный формат, не выводя желаемые параметры из тестируемого контейнера.
func audioBindingInit(t *testing.T, kind core.Codec, headers []byte) Init {
	t.Helper()
	// init сохраняет прежнее H264 и общую эпоху; новый звук использует шкалу декодера 48 кГц.
	init := testInit(t)
	init.Origin = OriginMapping{}
	// actual и err проверяют запись кодека до построения контейнерной записи.
	actual, err := codec.AudioFormatHeaders(kind, headers)
	if err != nil {
		t.Fatal(err)
	}
	init.Audio, err = media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackAudio, SourceEpoch: 11, Revision: 7, TimeBase: media.TimeBase{Numerator: 1, Denominator: 48000}, Audio: actual}, headers)
	if err != nil {
		t.Fatal(err)
	}
	return init
}

// rawAudioBindingFragment строит независимый аудиофрагмент trun с явной duration, dts и payload.
// sequence задаёт mfhd; Muxer.WritePacket не участвует, поэтому тест проверяет контейнерное время независимо от проверки записи.
func rawAudioBindingFragment(sequence uint32, dts uint64, duration uint32, payload []byte) []byte {
	// trun задаёт единственную AU дорожки2 и оставляет data_offset для последующего заполнения.
	trun := full("trun", 0, 0x701, u32(1), u32(0), u32(duration), u32(uint32(len(payload))), u32(0x02000000))
	// moof содержит буквальные tfhd/tfdt и эту единственную запись trun.
	moof := box("moof", full("mfhd", 0, 0, u32(sequence)), box("traf", full("tfhd", 0, 0x020000, u32(2)), full("tfdt", 1, 0, u64(dts)), trun))
	// offset указывает на data_offset, который всегда относится к началу moof.
	offset := len(moof) - len(trun) + 16
	be.PutUint32(moof[offset:offset+4], uint32(len(moof)+8))
	return join(moof, box("mdat", payload))
}

// TestAudioBindingsStandardEntriesRoundTrip проверяет четыре записи, фактическое уточнение конфигурации и сохранение AU и времени.
// t сравнивает контейнер с UUID и без него: стандартные заголовки и фактическая AU одинаково обязательны.
func TestAudioBindingsStandardEntriesRoundTrip(t *testing.T) {
	// kind выбирает отдельную начальную привязку, без изменения AAC или рецептов видео.
	for _, kind := range []core.Codec{core.CodecOpus, core.CodecFLAC, core.CodecMP3, core.CodecAC3} {
		// Подтест с t проверяет буквальную AU и стандартную запись выбранного kind.
		t.Run(string(kind), func(t *testing.T) {
			// headers,payload,samples задают независимые буквальные данные кодека и ожидаемый период.
			headers, payload, samples := audioBindingFixture(t, kind)
			// init задаёт фактическое аудио; output накапливает самостоятельный FLAC с постоянным размером блока.
			init := audioBindingInit(t, kind, headers)
			var output bytes.Buffer
			// mux и err проверяют сериализацию стандартной записи формата.
			mux, err := NewMuxer(&output, init, testLimits())
			if err != nil {
				t.Fatal(err)
			}
			// original сохраняет положительное начало 10 с и полную длительность, определяемую кодеком.
			original := packet(t, init.Audio, media.PacketInfo{PTS: 480000, DTS: 480000, DurationTicks: int64(samples)}, payload)
			if err = mux.WritePacket(original); err != nil {
				t.Fatal(err)
			}
			// uuid выбирает наличие необязательной дополнительной идентичности, которая не заменяет заголовки кодека.
			for _, uuid := range []bool{true, false} {
				// raw содержит отдельный проверяемый контейнер; UUID удаляется без изменения сжатых данных.
				raw := bytes.Clone(output.Bytes())
				if !uuid {
					raw = withoutConfigUUID(t, raw)
				}
				// demux и readErr проверяют ограниченное уточнение фактического аудио по первой AU.
				demux, readErr := NewDemuxer(bytes.NewReader(raw), DemuxOptions{Limits: testLimits(), SourceEpoch: 11, VideoRevision: 2, AudioRevision: 7})
				if readErr != nil {
					t.Fatal(kind, uuid, readErr)
				}
				// got и nextErr дают тот же удержанный AU без потерь или назначения признака ключевого кадра аудио.
				got, nextErr := demux.Next()
				if nextErr != nil || got.Info() != original.Info() || !bytes.Equal(got.Payload(), payload) || !sameConfig(got.Config(), init.Audio) {
					t.Fatal(kind, uuid, got.Info(), nextErr)
				}
				if _, nextErr = demux.Next(); !errors.Is(nextErr, io.EOF) {
					t.Fatal(nextErr)
				}
				if demux.Init().Audio.Info().FLACOwned {
					t.Fatal("Внешний контейнер назначил право FLAC rewrite")
				}
			}
		})
	}
}

// TestAudioBindingDurationRejectionPrecedesIO проверяет, что нулевая или неполная длительность не записывается для новых кодеков.
// t отдельно включает FreshSilentOpus/FreshSilentMP3 и подтверждает, что прочие кодеки сохраняют строгий период.
func TestAudioBindingDurationRejectionPrecedesIO(t *testing.T) {
	// kind выбирает привязку; duration ниже задаётся независимо от фактической AU.
	for _, kind := range []core.Codec{core.CodecOpus, core.CodecFLAC, core.CodecMP3, core.CodecAC3} {
		// headers,payload,samples задают фактическую форму, init — исходную неизменяемую конфигурацию.
		headers, payload, samples := audioBindingFixture(t, kind)
		init := audioBindingInit(t, kind, headers)
		// output хранит init, mux проверяет ошибочные пакеты до I/O, err — фактический отказ.
		var output bytes.Buffer
		mux, err := NewMuxer(&output, init, testLimits())
		if err != nil {
			t.Fatal(err)
		}
		// before отмечает длину неизменённого init перед каждым отклонённым пакетом.
		before := output.Len()
		// durations задаёт нулевую, неполную и чрезмерную длительность полной AU, включая нативный MP3-хвост 1105.
		durations := []uint32{0, samples - 1, samples + 1}
		if kind == core.CodecMP3 {
			durations = append(durations, 1105)
		}
		// duration перебирает независимо заданные контейнерные длительности.
		for _, duration := range durations {
			if err = mux.WritePacket(packet(t, init.Audio, media.PacketInfo{DurationTicks: int64(duration)}, payload)); !errors.Is(err, ErrUnsupportedTiming) {
				t.Fatal(kind, duration, err)
			}
			if output.Len() != before {
				t.Fatal("Неверная duration выполнила запись")
			}
			// raw обходит только проверку записи Muxer.WritePacket, чтобы независимо проверить чтение контейнерной длительности.
			raw := join(output.Bytes(), rawAudioBindingFragment(1, 480000, duration, payload))
			// fresh включает узкие допуски свежей собственной Opus/MP3-тишины; FLAC и AC3 остаются строгими.
			for _, fresh := range []bool{false, true} {
				// demux и readErr дают результат фактического уточнения по первой AU.
				demux, readErr := NewDemuxer(bytes.NewReader(raw), DemuxOptions{Limits: testLimits(), FreshSilentOpus: fresh, FreshSilentMP3: fresh})
				if fresh && (kind == core.CodecOpus && duration <= 120 || kind == core.CodecMP3 && duration > 0 && duration <= 1152) {
					if readErr != nil {
						t.Fatal(readErr)
					}
					// got сохраняет исходную длительность без заполнения по TOC; nextErr сообщает об отказе чтения пакета.
					got, nextErr := demux.Next()
					if nextErr != nil || got.Info().DurationTicks != int64(duration) {
						t.Fatal(got.Info(), nextErr)
					}
				} else if !errors.Is(readErr, ErrUnsupportedTiming) {
					t.Fatal(kind, duration, fresh, readErr)
				}
			}
		}
	}
}

// TestOpusWholeInitialAUOwnerPreservesRetainedOrigin проверяет удаление ровно одной целой AU при начале +10 с.
// t меняет только настоящее исключение dOps; удержанная AU и её время остаются прежними при новом demux.
func TestOpusWholeInitialAUOwnerPreservesRetainedOrigin(t *testing.T) {
	// headers,payload,samples задают нормализованный Head0 и буквальную CELT120.
	headers, payload, samples := audioBindingFixture(t, core.CodecOpus)
	// init и output содержат настоящую запись формата для последующей независимой мутации dOps.
	init := audioBindingInit(t, core.CodecOpus, headers)
	var output bytes.Buffer
	// err сообщает об отказе записи контейнерной инициализации перед проверкой её содержимого.
	if _, err := NewMuxer(&output, init, testLimits()); err != nil {
		t.Fatal(err)
	}
	// raw клонирует init; position указывает на dOps FourCC, перед его телом нет префикса FullBox.
	raw := bytes.Clone(output.Bytes())
	position := bytes.Index(raw, []byte("dOps"))
	if position < 0 {
		t.Fatal("Отсутствует dOps")
	}
	be.PutUint16(raw[position+6:position+8], 120)
	raw = append(raw, rawAudioBindingFragment(1, 480000, samples, payload)...)
	raw = append(raw, rawAudioBindingFragment(2, 480120, samples, payload)...)
	// seen отмечает, что NormalizeHeaders видит настоящее начальное состояние декодера, до удаления AU.
	seen := false
	// options сохраняет исходный Head через функцию, принимающую desc; она не исправляет preSkip за владельца уточнения.
	options := DemuxOptions{Limits: testLimits(), NormalizeHeaders: func(desc TrackDescription) ([]byte, error) {
		if desc.Codec == core.CodecOpus {
			// head и err подтверждают исходные120; пользовательская функция читает отдельную копию заголовков.
			head, err := codec.ParseOpusHead(desc.Headers)
			if err != nil {
				return nil, err
			}
			seen = head.PreSkip == 120
		}
		return desc.Headers, nil
	}}
	// demux и err нормализуют начальное состояние источника один раз независимо от положительного начала времени.
	demux, err := NewDemuxer(bytes.NewReader(raw), options)
	if err != nil {
		t.Fatal(err)
	}
	// head подтверждает нулевое начальное исключение удержанного сегмента; got содержит только вторую исходную AU.
	head, err := codec.ParseOpusHead(demux.Init().Audio.Headers())
	if err != nil || head.PreSkip != 0 || !seen {
		t.Fatal(head, seen, err)
	}
	got, err := demux.Next()
	if err != nil || got.Info().DTS != 480120 || got.Info().PTS != 480120 || got.Info().DurationTicks != 120 || !bytes.Equal(got.Payload(), payload) {
		t.Fatal(got.Info(), err)
	}
	if _, err = demux.Next(); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
}

// TestFLACPrivateInputRebasesClonesAndCommitsAfterWrite проверяет отдельный счётчик потока и неизменность источника.
// t требует STREAMINFO неизвестной длины, переменные номера 0/4608 и отсутствие изменения счётчика при переполнении или отказе записи.
func TestFLACPrivateInputRebasesClonesAndCommitsAfterWrite(t *testing.T) {
	// headers,payload,samples задают внешний кадр 0 с постоянным размером блока без FLACOwned.
	headers, payload, samples := audioBindingFixture(t, core.CodecFLAC)
	// init и source сохраняют исходные конфигурацию и байты для проверки после приватной перенумерации.
	init := audioBindingInit(t, core.CodecFLAC, headers)
	source := packet(t, init.Audio, media.PacketInfo{DurationTicks: int64(samples)}, payload)
	// output принимает приватный поток, mux владеет только его собственным счётчиком, err — отказ.
	var output bytes.Buffer
	mux, err := NewAudioTranscodeInputMuxer(&output, init, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	if err = mux.WritePacket(source); err != nil {
		t.Fatal(err)
	}
	if err = mux.WritePacket(packet(t, init.Audio, media.PacketInfo{DTS: 4608, PTS: 4608, DurationTicks: 4608}, payload)); err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(source.Payload(), payload) || !bytes.Equal(source.Config().Headers(), headers) || source.Config().Info().FLACOwned {
		t.Fatal("Изменён source")
	}
	// demux восстанавливает новое приватное STREAMINFO исключительно по стандартным байтам и AU.
	demux, err := NewDemuxer(bytes.NewReader(output.Bytes()), DemuxOptions{Limits: testLimits()})
	if err != nil {
		t.Fatal(err)
	}
	// info и parseErr подтверждают неизвестную длину и консервативные границы размера блока.
	info, parseErr := codec.ParseFLACStreamInfo(demux.Init().Audio.Headers())
	if parseErr != nil || info.MinBlockSize != 48 || info.MaxBlockSize != 4608 || info.TotalSamples != 0 {
		t.Fatal(info, parseErr)
	}
	// number задаёт ожидаемый счётчик отсчётов каждого физически записанного AU.
	for _, number := range []uint64{0, 4608} {
		// got и readErr содержат новую AU, frame и frameErr проверяют стратегию, счётчик и обе контрольные суммы кадра.
		got, readErr := demux.Next()
		if readErr != nil {
			t.Fatal(readErr)
		}
		frame, frameErr := codec.FLACFrameInfo(got.Config().Headers(), got.Payload())
		if frameErr != nil || !frame.Variable || frame.Number != number || frame.Samples != 4608 {
			t.Fatal(frame, frameErr)
		}
	}
	// before фиксирует байты перед переполнением; oldCounter сохраняет число полностью записанных отсчётов.
	before := output.Len()
	oldCounter := mux.flacCounter
	mux.flacCounter = (1 << 36) - 10
	if err = mux.WritePacket(source); err == nil || output.Len() != before || mux.flacCounter != (1<<36)-10 {
		t.Fatal("Переполнение не отвергнуто до I/O", err)
	}
	mux.flacCounter = oldCounter
	// next сохраняет строго продвинувшийся DTS, чтобы I/O отказ проверял запись, а не старое время.
	next := packet(t, init.Audio, media.PacketInfo{DTS: 9216, PTS: 9216, DurationTicks: 4608}, payload)
	mux.w = failingWriter{}
	if err = mux.WritePacket(next); !errors.Is(err, io.ErrClosedPipe) || mux.flacCounter != oldCounter {
		t.Fatal("Partial I/O изменил counter", err)
	}
	mux.w = &output
	if err = mux.WritePacket(next); !errors.Is(err, io.ErrClosedPipe) || output.Len() != before {
		t.Fatal("Sticky I/O не сохранён", err)
	}
}

// TestAudioBindingDurationDefaultsRespectExplicitZero проверяет порядок trun>tfhd>trex.
// t требует положительную фактическую длительность, даже если явный ноль перекрывает корректную длительность по умолчанию.
func TestAudioBindingDurationDefaultsRespectExplicitZero(t *testing.T) {
	// headers и payload задают полный кадр FLAC; samples — его размер, init — конфигурации обеих дорожек.
	headers, payload, samples := audioBindingFixture(t, core.CodecFLAC)
	init := audioBindingInit(t, core.CodecFLAC, headers)
	// output хранит init; trex затем получает независимо заданную duration4608.
	var output bytes.Buffer
	// err сообщает об отказе записи контейнерной инициализации перед проверкой её содержимого.
	if _, err := NewMuxer(&output, init, testLimits()); err != nil {
		t.Fatal(err)
	}
	// raw содержит отдельный init; position проходит его байты для поиска буквального trex дорожки2.
	raw := bytes.Clone(output.Bytes())
	for position := 0; position+28 <= len(raw); position++ {
		if string(raw[position:position+4]) == "trex" && be.Uint32(raw[position+8:position+12]) == 2 {
			be.PutUint32(raw[position+16:position+20], samples)
		}
	}
	// scenario задаёт наличие и значения двух уровней переопределения поверх trex4608.
	for _, scenario := range []struct {
		// name именует сочетание переопределений; tfhd и trun задают наличие полей, tfhdDuration и trunDuration — их значения.
		name                       string
		tfhd, trun                 bool
		tfhdDuration, trunDuration uint32
		// valid требует полного фактического периода после применения именно последнего присутствующего поля.
		valid bool
	}{
		{name: "trex", valid: true},
		{name: "tfhd-zero", tfhd: true, valid: false},
		{name: "tfhd-full", tfhd: true, tfhdDuration: 4608, valid: true},
		{name: "trun-zero", tfhd: true, tfhdDuration: 4608, trun: true, valid: false},
		{name: "trun-full", tfhd: true, trun: true, trunDuration: 4608, valid: true},
		{name: "trun-partial", trun: true, trunDuration: 4607, valid: false},
	} {
		// Подтест через t независимо строит поля duration только для выбранного scenario.
		t.Run(scenario.name, func(t *testing.T) {
			// tfhdFlags/tfhdFields задают наличие необязательной длительности по умолчанию для одной AU.
			tfhdFlags, tfhdFields := uint32(0x020000), u32(2)
			if scenario.tfhd {
				tfhdFlags |= 8
				tfhdFields = append(tfhdFields, u32(scenario.tfhdDuration)...)
			}
			// trunFlags/trunFields задают размер, флаги и необязательную длительность конкретной AU после data_offset.
			trunFlags, trunFields := uint32(0x601), join(u32(1), u32(0))
			if scenario.trun {
				trunFlags |= 0x100
				trunFields = append(trunFields, u32(scenario.trunDuration)...)
			}
			trunFields = append(trunFields, join(u32(uint32(len(payload))), u32(0x02000000))...)
			// trun и moof задают ровно одну аудио AU без использования Muxer.WritePacket.
			trun := full("trun", 0, trunFlags, trunFields)
			moof := box("moof", full("mfhd", 0, 0, u32(1)), box("traf", full("tfhd", 0, tfhdFlags, tfhdFields), full("tfdt", 1, 0, u64(0)), trun))
			be.PutUint32(moof[len(moof)-len(trun)+16:len(moof)-len(trun)+20], uint32(len(moof)+8))
			// demux и err проверяют фактический период кодека по окончательной контейнерной длительности.
			demux, err := NewDemuxer(bytes.NewReader(join(raw, moof, box("mdat", payload))), DemuxOptions{Limits: testLimits()})
			if !scenario.valid {
				if !errors.Is(err, ErrUnsupportedTiming) {
					t.Fatal(err)
				}
				return
			}
			if err != nil {
				t.Fatal(err)
			}
			// got сохраняет ожидаемые4608 именно после стандартного наследования duration.
			got, err := demux.Next()
			if err != nil || got.Info().DurationTicks != 4608 {
				t.Fatal(got.Info(), err)
			}
		})
	}
}

// TestNativeAudioBindingsDecode проверяет чтение четырёх новых записей реальным FFmpeg.
// t включает нативную проверку только через существующий GOST_FMP4_FFMPEG; обычный набор тестов остаётся изолированным.
func TestNativeAudioBindingsDecode(t *testing.T) {
	// ffmpeg задаёт явно выбранный существующий нативный декодер; пустое значение отключает эту проверку.
	ffmpeg := os.Getenv("GOST_FMP4_FFMPEG")
	if ffmpeg == "" {
		t.Skip("локальная CPU-интеграция не включена")
	}
	// kind выбирает самостоятельную запись формата; FLAC дополнительно проходит перенумерацию приватного входа.
	for _, kind := range []core.Codec{core.CodecOpus, core.CodecFLAC, core.CodecMP3, core.CodecAC3} {
		// Подтест с t требует полное число отсчётов декодера для буквальной AU данного kind.
		t.Run(string(kind), func(t *testing.T) {
			// headers,payload,samples задают фактическую буквальную AU кодека и полный период декодера.
			headers, payload, samples := audioBindingFixture(t, kind)
			// init и output задают fMP4 поток; mux использует приватную политику лишь для FLAC.
			init := audioBindingInit(t, kind, headers)
			var output bytes.Buffer
			// mux и err содержат новый физический транспорт и фактический результат его создания.
			mux, err := NewAudioTranscodeInputMuxer(&output, init, testLimits())
			if err != nil {
				t.Fatal(err)
			}
			// index задаёт три непрерывных AU, чтобы проверить также рост номера FLAC и независимое декодирование.
			for index := range 3 {
				if err = mux.WritePacket(packet(t, init.Audio, media.PacketInfo{DTS: int64(index) * int64(samples), PTS: int64(index) * int64(samples), DurationTicks: int64(samples)}, payload)); err != nil {
					t.Fatal(err)
				}
			}
			// decoded содержит фактический PCM16/stereo без предполагаемой длительности контейнерного описания.
			decoded := runMediaCommand(t, ffmpeg, output.Bytes(), "-hide_banner", "-loglevel", "error", "-xerror", "-i", "pipe:0", "-map", "0:a", "-c:a", "pcm_s16le", "-f", "s16le", "pipe:1")
			if len(decoded) != 3*int(samples)*2*2 {
				t.Fatal(kind, len(decoded), "ожидалось", 3*int(samples)*4)
			}
			if kind == core.CodecFLAC && !bytes.Equal(decoded, make([]byte, len(decoded))) {
				t.Fatal("Приватная FLAC перенумерация изменила нулевые samples")
			}
		})
	}
}

// TestAudioCompletionRetainsVideoPrefixAndSharesBudget проверяет совместное уточнение VP9 и MP3.
// t требует исходный порядок аудио→видео и общий предел прочитанных AU без безусловного предварительного чтения AAC.
func TestAudioCompletionRetainsVideoPrefixAndSharesBudget(t *testing.T) {
	// init и key дают независимую VP9-фикстуру; межкадровый AU здесь не требуется для уточнения фактического формата.
	init, key, _ := vp9PacketFixture(t)
	// headers,payload,samples задают MP3 без контейнерного DSI, поэтому аудио тоже требует первую AU.
	headers, payload, samples := audioBindingFixture(t, core.CodecMP3)
	// audioInit содержит ту же эпоху и полный фактический MP3-дескриптор времени выполнения.
	audioInit := audioBindingInit(t, core.CodecMP3, headers)
	init.Audio = audioInit.Audio
	// output хранит исходный fMP4, mux и err проверяют его независимую корректность.
	var output bytes.Buffer
	mux, err := NewMuxer(&output, init, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	// expected задаёт физический порядок двух разных дорожек с положительным началом.
	expected := []media.Packet{
		packet(t, init.Audio, media.PacketInfo{DTS: 480000, PTS: 480000, DurationTicks: int64(samples)}, payload),
		packet(t, init.Video, media.PacketInfo{DTS: 300, PTS: 300, DurationTicks: 1, Keyframe: true}, key),
	}
	// original обозначает пакет исходного порядка перед ограниченным уточнением фактического формата.
	for _, original := range expected {
		if err = mux.WritePacket(original); err != nil {
			t.Fatal(err)
		}
	}
	// raw удаляет UUID: обеим дорожкам теперь нужны фактические первые AU.
	raw := withoutConfigUUID(t, output.Bytes())
	// options задаёт только идентичность, не подставляя желаемое описание.
	options := DemuxOptions{Limits: testLimits(), SourceEpoch: 11, VideoRevision: 1, AudioRevision: 7}
	// demux сохраняет оба элемента префикса, err проверяет совместную инициализацию.
	demux, err := NewDemuxer(bytes.NewReader(raw), options)
	if err != nil {
		t.Fatal(err)
	}
	// original проверяет, что уточнение по первой AU не потерял, не переставил и не переписал другую дорожку.
	for _, original := range expected {
		// got и readErr содержат исходную AU и её фактическое время после завершения обеих дорожек.
		got, readErr := demux.Next()
		if readErr != nil || got.Info() != original.Info() || !bytes.Equal(got.Payload(), original.Payload()) {
			t.Fatal(got.Info(), readErr)
		}
	}
	options.Limits.MaxSamples = 1
	if _, err = NewDemuxer(bytes.NewReader(raw), options); !errors.Is(err, ErrLimit) {
		t.Fatal("Прочитанный префикс превысил общий MaxSamples", err)
	}
}

// TestAudioCompletionRequiresAUAndNeverSerializesFLACOwnership проверяет происхождение сведений времени выполнения.
// t не позволяет UUID заменить первую AU и не переносит FLACOwned через стандартный файл.
func TestAudioCompletionRequiresAUAndNeverSerializesFLACOwnership(t *testing.T) {
	// kind выбирает кодек, которому стандартная запись без фактической AU ещё не даёт полной проверки.
	for _, kind := range []core.Codec{core.CodecOpus, core.CodecFLAC, core.CodecMP3, core.CodecAC3} {
		// headers,payload,samples дают полные данные; init сохраняет разрешённый локальный формат.
		headers, payload, samples := audioBindingFixture(t, kind)
		init := audioBindingInit(t, kind, headers)
		if kind == core.CodecFLAC {
			// info отмечает доверенный источник только в RAM до записи самостоятельного файла.
			info := init.Audio.Info()
			info.FLACOwned = true
			// err сообщает об отказе создания доверенной конфигурации времени выполнения.
			var err error
			init.Audio, err = media.NewCodecConfig(info, headers)
			if err != nil {
				t.Fatal(err)
			}
		}
		// output хранит init; mux и err проверяют сериализацию описания времени выполнения без FLACOwned.
		var output bytes.Buffer
		mux, err := NewMuxer(&output, init, testLimits())
		if err != nil {
			t.Fatal(err)
		}
		if bytes.Contains(output.Bytes(), []byte("FLACOwned")) {
			t.Fatal("Runtime право сериализовано")
		}
		if _, err = NewDemuxer(bytes.NewReader(output.Bytes()), DemuxOptions{Limits: testLimits()}); !errors.Is(err, io.EOF) {
			t.Fatal("UUID заменил фактическую AU", kind, err)
		}
		if err = mux.WritePacket(packet(t, init.Audio, media.PacketInfo{DurationTicks: int64(samples)}, payload)); err != nil {
			t.Fatal(err)
		}
		// demux восстанавливает файл заново и не получает локального права исходного владельца кодировщика или артефакта.
		demux, err := NewDemuxer(bytes.NewReader(output.Bytes()), DemuxOptions{Limits: testLimits()})
		if err != nil || demux.Init().Audio.Info().FLACOwned {
			t.Fatal(kind, err)
		}
	}
}

// TestFLACPrivateHeaderGrowthRespectsBudget проверяет увеличение номера до первого лишнего байта I/O.
// t ограничивает нагрузку исходными 14 байтами: второй переменный номер длиннее и должен быть отклонён.
func TestFLACPrivateHeaderGrowthRespectsBudget(t *testing.T) {
	// headers,payload,samples задают полную14-байтовую FLAC AU; init сохраняет её внешнюю конфигурацию.
	headers, payload, samples := audioBindingFixture(t, core.CodecFLAC)
	init := audioBindingInit(t, core.CodecFLAC, headers)
	// limits ограничивает окончательную нагрузку, включая возможный рост поля номера.
	limits := testLimits()
	limits.MaxSampleBytes = uint64(len(payload))
	// output хранит поток; mux и err проверяют приватную перенумерацию при этом бюджете.
	var output bytes.Buffer
	mux, err := NewAudioTranscodeInputMuxer(&output, init, limits)
	if err != nil {
		t.Fatal(err)
	}
	if err = mux.WritePacket(packet(t, init.Audio, media.PacketInfo{DurationTicks: int64(samples)}, payload)); err != nil {
		t.Fatal(err)
	}
	// before отмечает границу полной первой записи перед более длинным номером4608.
	before := output.Len()
	if err = mux.WritePacket(packet(t, init.Audio, media.PacketInfo{DTS: 4608, PTS: 4608, DurationTicks: 4608}, payload)); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
	if output.Len() != before || mux.flacCounter != 4608 {
		t.Fatal("Отказ бюджета изменил байты или счётчик")
	}
}
