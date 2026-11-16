package fmp4

import (
	"bytes"
	"encoding/hex"
	"errors"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
)

// TestVP9ActualCompletionAndPrefix подтверждает фактический формат без UUID и желаемых настроек, сохранность префикса, аудио и времени.
func TestVP9ActualCompletionAndPrefix(t *testing.T) {
	// headers и first/second — буквальная запись и два реальных ключевой/межкадровый AU FFmpeg7.1.5, 64×64/30.
	headers, _ := hex.DecodeString("000a820202010000")
	first, _ := hex.DecodeString("824983424003f003f60038241c184a00003060000078b9ffff57b0ffffff7c17fffffffc78c1cfd2e880")
	second, _ := hex.DecodeString("860040929c00500000036000005470")
	// actual содержит сведения настоящего ключевого заголовка без подстановки размеров желаемого профиля.
	// err проверяет разбор настоящего VP9 key header до создания контейнера.
	actual, _, err := codec.InspectVP9AU(headers, first, core.FrameRate{Numerator: 30, Denominator: 1})
	if err != nil {
		t.Fatal(err)
	}
	// init сохраняет прежний AAC и задаёт одну исходную эпоху обеим дорожкам.
	init := testInit(t)
	// video содержит запись и фактический видеоформат в точной шкале 30 Гц.
	// err сообщает об отказе создания конфигурации видео.
	video, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: init.Audio.Info().SourceEpoch, Revision: 1, TimeBase: media.TimeBase{Numerator: 1, Denominator: 30}, Video: actual}, headers)
	if err != nil {
		t.Fatal(err)
	}
	init.Video = video
	init.Origin = OriginMapping{}
	// wire принимает ограниченную инициализацию и три единицы нагрузки без промежуточного файла.
	var wire bytes.Buffer
	// mux проверяет новый vpcC FullBox binding до публикации init.
	// err проверяет сериализацию нового vpcC до записи пакетов.
	mux, err := NewMuxer(&wire, init, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	// audio/video0/video1 сохраняют физический порядок audio→key→inter и исходные длительности.
	audio := packet(t, init.Audio, media.PacketInfo{DTS: 0, PTS: 0, DurationTicks: 1024}, []byte{0x21, 0x10})
	video0 := packet(t, video, media.PacketInfo{DTS: 0, PTS: 0, DurationTicks: 1, Keyframe: true}, first)
	video1 := packet(t, video, media.PacketInfo{DTS: 1, PTS: 1, DurationTicks: 1}, second)
	// original обозначает очередной пакет последовательности.
	for _, original := range []media.Packet{audio, video0, video1} {
		if err = mux.WritePacket(original); err != nil {
			t.Fatal(err)
		}
	}
	// raw удаляет только собственный UUID, имитируя внешний fMP4 без утверждаемого формата.
	raw := withoutConfigUUID(t, wire.Bytes())
	// options задаёт явную идентичность времени выполнения; первоначальный восстановитель VP9 требует измерения первой единицы доступа.
	options := DemuxOptions{Limits: testLimits(), SourceEpoch: init.Audio.Info().SourceEpoch, VideoRevision: 1, AudioRevision: init.Audio.Info().Revision}
	// demux восстанавливает оба фактических формата, удерживая префикс аудио и ключевого кадра в исходных бюджетах.
	// err проверяет восстановление фактического формата через ограниченное предварительное чтение.
	demux, err := NewDemuxer(bytes.NewReader(raw), options)
	if err != nil {
		t.Fatal(err)
	}
	if demux.Init().Video.Info().Video != actual {
		t.Fatal(demux.Init().Video.Info())
	}
	// expected проверяет тот же порядок байтов и временных меток после дополнения фактического формата.
	for _, expected := range []media.Packet{audio, video0, video1} {
		// got содержит очередной возвращённый префикс либо последующий межкадровый кадр.
		// readErr сохраняет причинный отказ чтения, отдельно от сравнения payload/time.
		got, readErr := demux.Next()
		if readErr != nil || !bytes.Equal(got.Payload(), expected.Payload()) || got.Info() != expected.Info() {
			t.Fatal(got.Info(), readErr)
		}
	}
	// calls считает ровно initial и повторный custom resolver video вызовы.
	calls := 0
	options.ResolveConfig = func(description TrackDescription) (media.CodecConfig, error) {
		// Обработчик принимает только измеренные фактические сведения без подстановки желаемых значений.
		if description.Codec == core.CodecVP9 {
			calls++
			if description.ActualVideo.Validate() != nil {
				return media.CodecConfig{}, ErrMetadataUnavailable
			}
			if !bytes.Equal(description.FirstSample, first) {
				t.Fatal("Потерян original independent prefix")
			}
		}
		return ResolveCodecConfig(description, options.SourceEpoch, 1)
	}
	if _, err = NewDemuxer(bytes.NewReader(raw), options); err != nil || calls != 2 {
		t.Fatal(calls, err)
	}
}

// withoutConfigUUID удаляет собственный UUID из ограниченной тестовой инициализации и пересчитывает moov size.
func withoutConfigUUID(t *testing.T, data []byte) []byte {
	t.Helper()
	// top содержит init/fragments без изменения moof-relative offsets.
	// err проверяет корректность обрамления исходных test boxes.
	top, err := children(data)
	if err != nil {
		t.Fatal(err)
	}
	// result сохраняет исходные верхние boxes кроме пересобранного moov.
	var result []byte
	// item обозначает очередной верхний box.
	for _, item := range top {
		if item.kind != "moov" {
			result = append(result, box(item.kind, item.data)...)
			continue
		}
		// inside содержит дочерние boxes moov для удаления одного test UUID.
		// parseErr сообщает о повреждении исходного test moov.
		inside, parseErr := children(item.data)
		if parseErr != nil {
			t.Fatal(parseErr)
		}
		// body накапливает исходные дочерние boxes с сохранением порядка.
		var body []byte
		// child обозначает очередной box moov.
		for _, child := range inside {
			if child.kind != "uuid" {
				body = append(body, box(child.kind, child.data)...)
			}
		}
		result = append(result, box("moov", body)...)
	}
	return result
}

// TestHDRBoxGridsAndUnknown проверяет RGB/GBR, обрамление FullBox и сохранение неизвестного CLL.
func TestHDRBoxGridsAndUnknown(t *testing.T) {
	// literal содержит mdcv в сетке SEI и CLL с исходной неизвестной MaxCLL=0.
	literal := box("mdcv", []byte{0x33, 0xc2, 0x86, 0xc4, 0x1d, 0x4c, 0x0b, 0xb8, 0x84, 0xd0, 0x3e, 0x80, 0x3d, 0x13, 0x40, 0x42, 0, 0x98, 0x96, 0x80, 0, 0, 0, 1})
	literal = append(literal, box("clli", []byte{0, 0, 1, 144})...)
	// metadata подтверждает GBR→RGB и собственную сетку каждой яркости.
	// err проверяет разбор буквальных блоков HDR.
	metadata, err := ParseHDRBoxes(literal, false)
	if err != nil {
		t.Fatal(err)
	}
	if metadata.MDCV.Primaries[0].X.Raw != 34000 || metadata.MDCV.MaxLuminance.Raw != 10000000 || metadata.CLL.MaxCLL.Known {
		t.Fatal(metadata)
	}
	// exact сохраняет неизвестное кодовое слово при тождественной сетке.
	// err проверяет сохранение исходного неизвестного кодового слова без подмены.
	exact, err := marshalHDRBoxes(metadata, false)
	if err != nil || !bytes.Equal(exact, literal) {
		t.Fatal(err)
	}
	// vp9 имеет отдельные RGB UQ и FullBox version0/flags0.
	vp9, err := marshalHDRBoxes(metadata, true)
	if err != nil {
		t.Fatal(err)
	}
	// projected восстанавливает physical projection в иной сетке без raw integer equality.
	projected, err := ParseHDRBoxes(vp9, true)
	if err != nil {
		t.Fatal(err)
	}
	if err = codec.MatchHDRProjection(metadata, projected); err != nil {
		t.Fatal(err)
	}
	if _, err = ParseHDRBoxes(vp9, false); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	// corrupt меняет flags реального SmDm FullBox.
	corrupt := bytes.Clone(vp9)
	corrupt[11] = 1
	if _, err = ParseHDRBoxes(corrupt, true); err == nil {
		t.Fatal("Приняты reserved flags")
	}
}

// TestHEVCIncompleteHeadersPreserved подтверждает выведенный контекст без изменения исходного hvcC.
func TestHEVCIncompleteHeadersPreserved(t *testing.T) {
	// full/first/second сохраняют полные заголовки и два буквальных AU собственной FFmpeg пробы.
	full, _ := hex.DecodeString("0101600000009000000000001ef000fcfdf8f800000f03200001001840010c01ffff01600000030090000003000003001eba0240210001002b42010101600000030090000003000003001ea020810596ea4932bc05a810100820000003002000000303c122000100074401c172b42240")
	first, _ := hex.DecodeString("0000001840010c01ffff01600000030090000003000003001eba02400000002b42010101600000030090000003000003001ea020810596ea4932bc05a810100820000003002000000303c1000000074401c172b42240000000142801af0860f9eead5ffea9b7f2614b9762feb980")
	second, _ := hex.DecodeString("0000000b0201d0097e10c610c0fb8c")
	// actual подтверждён полным SPS и VUI до теста неполного контейнера.
	// err проверяет фактическую геометрию и временные параметры полного HEVC SPS.
	actual, err := codec.HEVCFormat(codec.VideoDescription{Headers: full})
	if err != nil {
		t.Fatal(err)
	}
	// headers сохраняет исходный hvcC без массивов, предоставленных в первом AU.
	headers := bytes.Clone(full[:23])
	headers[22] = 0
	// init использует прежнюю проверенную AAC дорожку и новую HEVC конфигурацию.
	init := testInit(t)
	init.Origin = OriginMapping{}
	// video хранит исходную неполную запись с ранее доказанным фактическим описанием.
	// err проверяет создание конфигурации времени выполнения с оригинальным неполным hvcC.
	video, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: init.Audio.Info().SourceEpoch, Revision: 1, TimeBase: media.TimeBase{Numerator: 1, Denominator: 30}, Video: actual}, headers)
	if err != nil {
		t.Fatal(err)
	}
	init.Video = video
	// wire принимает новую инициализацию hev1 и исходные ключевую/межкадровую единицы доступа.
	var wire bytes.Buffer
	// mux сохраняет исходный hvcC; он не записывает выведенные из AU arrays.
	// err проверяет запись исходного hvcC в новый hev1 init.
	mux, err := NewMuxer(&wire, init, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	// index и payload выбирают два последовательных compressed AU.
	for index, payload := range [][]byte{first, second} {
		if err = mux.WritePacket(packet(t, video, media.PacketInfo{DTS: int64(index), PTS: int64(index), DurationTicks: 1, Keyframe: index == 0}, payload)); err != nil {
			t.Fatal(err)
		}
	}
	// demux дополняет фактические сведения без UUID с идентичностью этого тестового источника.
	demux, err := NewDemuxer(bytes.NewReader(withoutConfigUUID(t, wire.Bytes())), DemuxOptions{Limits: testLimits(), SourceEpoch: init.Audio.Info().SourceEpoch, VideoRevision: 1, AudioRevision: 1})
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(demux.Init().Video.Headers(), headers) {
		t.Fatal("Изменён original hvcC")
	}
	// description проверяет оригинальную wire запись в публичном описании.
	for _, description := range demux.Descriptions() {
		if description.Track == media.TrackVideo && !bytes.Equal(description.Headers, headers) {
			t.Fatal("Подменён description")
		}
	}
	// expected подтверждает неизменность байтов и конфигурации после восстановления состояния разборщика.
	for _, expected := range [][]byte{first, second} {
		// got содержит исходный AU вместе с исходной записью без подстановки выведенной конфигурации.
		// readErr проверяет выдачу key/inter AU с неизменными headers.
		got, readErr := demux.Next()
		if readErr != nil || !bytes.Equal(got.Payload(), expected) || !bytes.Equal(got.Config().Headers(), headers) {
			t.Fatal(readErr)
		}
	}
}

// TestFreshHDRIndependentRounding проверяет разрешённую коррекцию из двух проекций общего источника.
func TestFreshHDRIndependentRounding(t *testing.T) {
	// payload содержит SEI-grid дисплей с максимальной яркостью 1000.0001 кд/м².
	payload := []byte{0x33, 0xc2, 0x86, 0xc4, 0x1d, 0x4c, 0x0b, 0xb8, 0x84, 0xd0, 0x3e, 0x80, 0x3d, 0x13, 0x40, 0x42, 0, 0x98, 0x96, 0x81, 0, 0, 0, 1}
	// source сохраняет точные исходные кодовые слова без промежуточного AV1 округления.
	source := media.HDRMetadata{}
	source.MDCV, _ = codec.DecodeHDRMastering(payload, false, media.HDRSourceH264SEI)
	// binary содержит направленную проекцию source в сетки UQ AV1/SmDm.
	// err проверяет направленную проекцию исходного SEI в двоичную сетку.
	binary, err := marshalHDRBoxes(source, true)
	if err != nil {
		t.Fatal(err)
	}
	// inband использует фактическую проекцию AV1, максимум которой уже округлён до 1000 кд/м².
	inband, err := ParseHDRBoxes(binary, true)
	if err != nil {
		t.Fatal(err)
	}
	inband.MDCV.Source = media.HDRSourceAV1OBU
	if err = codec.MatchHDRProjection(inband, source); err == nil {
		t.Fatal("Скрыто различие independent rounding")
	}
	// effective принимает обе доказанные проекции источника до выбора фактических сведений сжатого потока.
	effective, err := codec.ReconcileFreshHDR(source, source, inband)
	if err != nil {
		t.Fatal(err)
	}
	// corrected содержит новую направленную проекцию авторитетных сведений AV1 без сохранения старой сетки источника.
	corrected, err := marshalHDRBoxes(effective, false)
	if err != nil {
		t.Fatal(err)
	}
	// result подтверждает максимальную яркость после разрешённой коррекции свежего результата.
	result, err := ParseHDRBoxes(corrected, false)
	if err != nil || result.MDCV.MaxLuminance.Raw != 10000000 {
		t.Fatal(result, err)
	}
	// native разрешает mdcv кодировщика VP9 только при явно подтверждённом происхождении свежего результата.
	native := box("mdcv", payload)
	if _, err = ParseHDRBoxes(native, true); err == nil {
		t.Fatal("Расширен arbitrary VP9 ingest")
	}
	if _, err = parseHDRBoxes(native, true, true); err != nil {
		t.Fatal(err)
	}
	// absent содержит неизвестный компонент, который нельзя выдать за известный SEI ноль.
	absent := source
	absent.MDCV.MinLuminance = media.HDRValue{Raw: 1, Grid: media.HDRGridBinaryMinLuminance, Known: true}
	// tiny округляется в SEI в известное положительное значение; ноль ниже минимальной сетки отклоняется.
	tiny, _ := media.NewHDRAMFValue(0.000001)
	absent.MDCV.MinLuminance = tiny
	if _, err = marshalHDRBoxes(absent, false); err == nil {
		t.Fatal("Известный компонент стал unknown нулём")
	}
}
