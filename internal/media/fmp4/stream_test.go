package fmp4

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
)

// Возвращает явные бюджеты тестового fMP4-транспорта с максимумом 100 описаний единиц доступа.
func testLimits() Limits {
	return Limits{MaxBoxBytes: 1 << 20, MaxInitBytes: 1 << 20, MaxMoofBytes: 1 << 16, MaxMdatBytes: 1 << 20, MaxSampleBytes: 1 << 18, MaxExtradataBytes: 1 << 12, MaxSamples: 100}
}

// Создаёт тестовую пару H.264 128×128/30 и AAC-LC с двумя каналами и частотой 48 кГц с общим сдвигом
// начала на три секунды.
func testInit(t testing.TB) Init {
	t.Helper()
	// Заголовки libx264 описывают High/yuv420p, 128×128 при 30 FPS.
	// headers содержит эталон avcC H.264 High 128×128/30; err сообщает об ошибке декодирования
	// записи.
	headers, err := hex.DecodeString("0164000bffe100186764000bacd942046c0440000003004000000f03c50a658001000668ebe3cb22c0fdf8f800")
	if err != nil {
		t.Fatal(err)
	}
	// v содержит видеоконфигурацию с эпохой 11, ревизией 2 и шкалой 1/30.
	v, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: 11, Revision: 2, TimeBase: media.TimeBase{Numerator: 1, Denominator: 30}, Video: core.VideoFormat{Codec: core.CodecH264, Width: 128, Height: 128, FrameRate: core.FrameRate{Numerator: 30, Denominator: 1}, CodecProfile: "high", PixelFormat: "yuv420p", BitDepth: 8, Color: core.ColorInfo{Range: "limited", Primaries: "unspecified", Transfer: "unspecified", Matrix: "unspecified"}}}, headers)
	if err != nil {
		t.Fatal(err)
	}
	// a содержит конфигурацию AAC-LC, 48 кГц и двух каналов с ревизией 7.
	a, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackAudio, SourceEpoch: 11, Revision: 7, TimeBase: media.TimeBase{Numerator: 1, Denominator: 48000}, Audio: core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", SampleRateHz: 48000, Channels: 2, ChannelLayout: "stereo"}}, []byte{0x11, 0x90})
	if err != nil {
		t.Fatal(err)
	}
	return Init{Video: v, Audio: a, Origin: OriginMapping{OffsetTicks: 3, TimeBase: media.TimeBase{Numerator: 1, Denominator: 1}}}
}

// Создаёт тестовый пакет с заданными конфигурацией, метками и нагрузкой; ошибка завершает тест.
func packet(t testing.TB, c media.CodecConfig, info media.PacketInfo, payload []byte) media.Packet {
	t.Helper()
	// p содержит созданный тестовый пакет; err сообщает об отказе проверки его данных.
	p, err := media.NewPacket(info, c, payload)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// shortWriter ограничивает отдельную запись для проверки продолжения частичного вывода.
type shortWriter struct {
	// w задаёт вложенный приёмник байтов контейнера.
	w io.Writer
	// n задаёт положительный предел байтов одной записи в тесте.
	n int
}

// Ограничивает одну запись числом w.n байтов, чтобы проверить продолжение частичного ввода-вывода.
func (w shortWriter) Write(b []byte) (int, error) {
	if len(b) > w.n {
		b = b[:w.n]
	}
	return w.w.Write(b)
}

// shortReader ограничивает отдельное чтение для проверки сборки контейнера из частичных данных.
type shortReader struct {
	// r задаёт вложенный источник байтов контейнера.
	r io.Reader
	// n задаёт положительный предел байтов одного чтения в тесте.
	n int
}

// Ограничивает одно чтение числом r.n байтов, чтобы проверить сборку заголовков и данных из
// фрагментов.
func (r shortReader) Read(b []byte) (int, error) {
	if len(b) > r.n {
		b = b[:r.n]
	}
	return r.r.Read(b)
}

// Проверяет точное сохранение знаковых PTS/DTS, длительностей, конфигураций и нагрузки при частичных
// чтениях и записях и EOF после последнего пакета.
func TestStreamingRoundTripPreservesSignedTimestampsAndConfig(t *testing.T) {
	// init содержит конфигурации дорожек и общий сдвиг начала для проверки знаковых меток.
	init := testInit(t)
	// buffer накапливает контейнер, записанный порциями не более трёх байтов.
	var buffer bytes.Buffer
	// m записывает контейнер с частичным выводом; err сообщает об отказе инициализации и последующих
	// операций.
	m, err := NewMuxer(shortWriter{&buffer, 3}, init, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	// packets задаёт эталонные пакеты видео и аудио с отрицательными и переставленными метками.
	packets := []media.Packet{packet(t, init.Video, media.PacketInfo{DTS: -60, PTS: -57, DurationTicks: 1, Keyframe: true}, []byte{1, 2, 3}), packet(t, init.Audio, media.PacketInfo{DTS: -95000, PTS: -95001, DurationTicks: 1024}, []byte{4, 5}), packet(t, init.Video, media.PacketInfo{DTS: -59, PTS: -61, DurationTicks: 1}, []byte{6, 7})}
	// p содержит очередной пакет, передаваемый мультиплексору.
	for _, p := range packets {
		if err = m.WritePacket(p); err != nil {
			t.Fatal(err)
		}
	}
	// d читает контейнер порциями не более двух байтов.
	d, err := NewDemuxer(shortReader{bytes.NewReader(buffer.Bytes()), 2}, DemuxOptions{Limits: testLimits()})
	if err != nil {
		t.Fatal(err)
	}
	// index задаёт номер пакета, want — исходный эталон для сравнения после чтения.
	for index, want := range packets {
		// got содержит прочитанный пакет; err сообщает об отказе его чтения.
		got, err := d.Next()
		if err != nil {
			t.Fatal(err)
		}
		if got.Info() != want.Info() || !sameConfig(got.Config(), want.Config()) || !bytes.Equal(got.Payload(), want.Payload()) {
			t.Fatalf("пакет %d изменился: %+v != %+v", index, got.Info(), want.Info())
		}
	}
	if _, err = d.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("ожидался EOF: %v", err)
	}
}

// Проверяет отказ непредставимого времени до записи и отдельные ограничения FFmpeg на PTS<DTS и
// повторный DTS.
func TestPacketRejectionPrecedesIO(t *testing.T) {
	// init содержит конфигурации дорожек для проверки отказов до ввода-вывода.
	init := testInit(t)
	// output накапливает контейнер для контроля отсутствия записи некорректных пакетов.
	var output bytes.Buffer
	// m записывает контейнер; err принимает ошибки инициализации и последующих проверяемых операций.
	m, err := NewMuxer(&output, init, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	// before фиксирует размер транспорта до отклонения непредставимых меток.
	before := output.Len()
	// info содержит очередной набор меток либо длительности, не представимый в контейнере.
	for _, info := range []media.PacketInfo{{DTS: math.MinInt64, PTS: 0}, {DTS: 0, PTS: math.MaxInt64}, {DTS: 0, PTS: 0, DurationTicks: math.MaxInt64}} {
		if err = m.WritePacket(packet(t, init.Video, info, []byte{1})); !errors.Is(err, ErrUnsupportedTiming) {
			t.Fatalf("не отвергнуто время: %v", err)
		}
		if output.Len() != before {
			t.Fatal("ошибочный пакет попал в транспорт")
		}
	}
	// validator отдельно проверяет ограничения временных меток на границе FFmpeg.
	validator := NewFFmpegTimingValidator()
	// p содержит пакет, сначала с PTS меньше DTS, затем с допустимой парой для проверки повторного
	// DTS.
	p := packet(t, init.Video, media.PacketInfo{PTS: 0, DTS: 1}, []byte{1})
	if err = validator.Validate(p); !errors.Is(err, ErrUnsupportedTiming) {
		t.Fatal(err)
	}
	p = packet(t, init.Video, media.PacketInfo{PTS: 2, DTS: 1}, []byte{1})
	if err = validator.Validate(p); err != nil {
		t.Fatal(err)
	}
	if err = validator.Validate(p); !errors.Is(err, ErrUnsupportedTiming) {
		t.Fatal(err)
	}
}

// Проверяет отказ по заявленному размеру ftyp или mdat до чтения отсутствующего тела.
func TestDeclaredLimitsPrecedeBodyReads(t *testing.T) {
	// l задаёт ограничения размеров тестовых блоков и нагрузки.
	l := testLimits()
	// data содержит сначала чрезмерный заголовок ftyp, затем копию контейнера для повреждения mdat.
	data := join(u32(uint32(l.MaxBoxBytes+1)), []byte("ftyp"))
	// err принимает результат чтения чрезмерного ftyp и последующих операций сценария.
	_, err := NewDemuxer(bytes.NewReader(data), DemuxOptions{Limits: l})
	if !errors.Is(err, ErrLimit) {
		t.Fatalf("размер должен быть отклонён до EOF: %v", err)
	}
	// init содержит конфигурации дорожек для построения исходного корректного контейнера.
	init := testInit(t)
	// output накапливает исходную инициализацию и пакет перед повреждением размера mdat.
	var output bytes.Buffer
	// m записывает исходный контейнер с выбранными ограничениями.
	m, err := NewMuxer(&output, init, l)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.WritePacket(packet(t, init.Video, media.PacketInfo{PTS: 0, DTS: 0}, []byte{1, 2})); err != nil {
		t.Fatal(err)
	}
	data = bytes.Clone(output.Bytes())
	// top содержит верхнеуровневые блоки для поиска заголовка mdat.
	top, err := children(data)
	if err != nil {
		t.Fatal(err)
	}
	// position задаёт абсолютное смещение текущего верхнеуровневого блока.
	position := 0
	// b содержит очередной блок для поиска и повреждения размера mdat.
	for _, b := range top {
		if b.kind == "mdat" {
			be.PutUint32(data[position:position+4], uint32(l.MaxMdatBytes+1))
			data = data[:position+8]
			break
		}
		position += int(b.size)
	}
	// d читает контейнер с чрезмерным mdat без его тела.
	d, err := NewDemuxer(bytes.NewReader(data), DemuxOptions{Limits: l})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Next(); !errors.Is(err, ErrLimit) {
		t.Fatalf("mdat должен быть отклонён до чтения: %v", err)
	}
}

// Проверяет UnexpectedEOF при усечённой нагрузке и сохранение ошибки записи Muxer даже после замены
// Writer.
func TestDisconnectAndStickyWriteFailure(t *testing.T) {
	// init содержит конфигурации дорожек для проверок обрыва и ошибки записи.
	init := testInit(t)
	// output накапливает исходный контейнер и служит новым приёмником после имитации отказа.
	var output bytes.Buffer
	// m записывает контейнер; err принимает результаты инициализации, обрыва чтения и проверок
	// записи.
	m, err := NewMuxer(&output, init, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	if err = m.WritePacket(packet(t, init.Video, media.PacketInfo{}, []byte{1, 2, 3})); err != nil {
		t.Fatal(err)
	}
	// data ссылается на сериализованный контейнер перед исключением последнего байта нагрузки.
	data := output.Bytes()
	// d читает источник с усечённой нагрузкой последнего пакета.
	d, err := NewDemuxer(bytes.NewReader(data[:len(data)-1]), DemuxOptions{Limits: testLimits()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Next(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal(err)
	}
	m.w = failingWriter{}
	if err = m.WritePacket(packet(t, init.Audio, media.PacketInfo{}, []byte{1})); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
	m.w = &output
	if err = m.WritePacket(packet(t, init.Audio, media.PacketInfo{}, []byte{1})); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal("повторная запись после частичной ошибки разрешена")
	}
}

// failingWriter имитирует закрытый транспорт, отклоняя каждую запись с io.ErrClosedPipe.
type failingWriter struct{}

// Всегда возвращает io.ErrClosedPipe без записанных байтов для проверки непригодности транспорта после
// ошибки.
func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }

// Проверяет отказ при непредставимой общей шкале и неточном общем сдвиге начала до записи
// инициализации.
func TestExactOriginAndTimescale(t *testing.T) {
	// err содержит результат поиска общей шкалы, превышающей представимый диапазон.
	if _, err := MovieTimeScale(media.TimeBase{Numerator: 1, Denominator: math.MaxUint32}, media.TimeBase{Numerator: 1, Denominator: math.MaxUint32 - 1}); !errors.Is(err, ErrUnsupportedTiming) {
		t.Fatal(err)
	}
	// init содержит пару дорожек с общим сдвигом, который тест заменяет на непредставимую долю
	// секунды.
	init := testInit(t)
	init.Origin = OriginMapping{OffsetTicks: 1, TimeBase: media.TimeBase{Numerator: 1, Denominator: 7}}
	// b учитывает записанные байты для проверки отказа до инициализации транспорта.
	var b bytes.Buffer
	// err содержит результат создания мультиплексора с неточно представимым сдвигом начала.
	if _, err := NewMuxer(&b, init, testLimits()); !errors.Is(err, ErrUnsupportedTiming) || b.Len() != 0 {
		t.Fatalf("непредставимый общий origin: %v", err)
	}
}

// Проверяет отказ переполнения PTS после сдвига начала и приём следующего корректного пакета без
// продвижения состояния ошибочным пакетом.
func TestFFmpegMappedRangeRejectsOverflowBeforeAdvancing(t *testing.T) {
	// init содержит видеоконфигурацию, в шкале которой задан проверяемый сдвиг.
	init := testInit(t)
	// v проверяет представимость меток после прибавления одного такта начала.
	v := NewFFmpegTimingValidatorForOrigin(OriginMapping{OffsetTicks: 1, TimeBase: init.Video.Info().TimeBase})
	// bad содержит пакет, время показа которого переполняется после сдвига.
	bad := packet(t, init.Video, media.PacketInfo{DTS: math.MaxInt64 - 1, PTS: math.MaxInt64}, []byte{1})
	// err содержит результат отклонения пакета с переполнением отображённого времени.
	if err := v.Validate(bad); !errors.Is(err, ErrUnsupportedTiming) {
		t.Fatal(err)
	}
	// err сообщает об отказе следующего допустимого пакета после проверки переполнения.
	if err := v.Validate(packet(t, init.Video, media.PacketInfo{DTS: 0, PTS: 1}, []byte{1})); err != nil {
		t.Fatal(err)
	}
}

// Проверяет отказ AV1 с PTS≠DTS до записи и отсутствие поля композиционного смещения в trun принятого
// пакета.
func TestAV1OmitsCompositionOffsets(t *testing.T) {
	// init содержит исходные дорожки перед заменой видеокодека на AV1.
	init := testInit(t)
	// info содержит изменяемое описание видео для выбора AV1.
	info := init.Video.Info()
	info.Video.Codec = core.CodecAV1
	// err принимает ошибку создания конфигурации AV1 и последующих операций контейнера.
	var err error
	init.Video, err = media.NewCodecConfig(info, []byte{0x81, 0, 0, 0})
	if err != nil {
		t.Fatal(err)
	}
	// out накапливает контейнер для контроля отказа до записи и просмотра trun.
	var out bytes.Buffer
	// m записывает тестовый контейнер AV1.
	m, err := NewMuxer(&out, init, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	// before фиксирует размер транспорта перед отклонением AV1 с несовпадающими PTS и DTS.
	before := out.Len()
	if err = m.WritePacket(packet(t, init.Video, media.PacketInfo{PTS: 1, DTS: 0}, []byte{1})); !errors.Is(err, ErrUnsupportedTiming) || out.Len() != before {
		t.Fatal(err)
	}
	if err = m.WritePacket(packet(t, init.Video, media.PacketInfo{Keyframe: true}, []byte{1})); err != nil {
		t.Fatal(err)
	}
	// moof содержит найденный блок описания принятого фрагмента.
	moof, err := find(out.Bytes(), "moof")
	if err != nil {
		t.Fatal(err)
	}
	// traf содержит описание дорожки внутри найденного moof.
	traf, err := find(moof.data, "traf")
	if err != nil {
		t.Fatal(err)
	}
	// trun содержит описание принятого пакета для проверки отсутствия композиционного смещения.
	trun, err := find(traf.data, "trun")
	if err != nil {
		t.Fatal(err)
	}
	// f содержит 24-битные флаги trun.
	f, _ := flags(trun.data)
	if f&0x800 != 0 {
		t.Fatal("AV1 trun содержит composition offset")
	}
}

// Проверяет отказ чрезмерного числа или размера единиц доступа и недопустимого смещения до чтения
// отсутствующей нагрузки.
func TestSampleDeclarationsAndOffsetsFailBeforePayloadRead(t *testing.T) {
	// kind выбирает повреждение числа пакетов, размера пакета либо смещения нагрузки.
	for _, kind := range []string{"sample-count", "sample-size", "data-offset"} {
		t.Run(kind, func(t *testing.T) {
			// Повреждает выбранное число, размер либо смещение единиц доступа и проверяет категорию отказа без
			// доступного тела mdat.

			// init содержит конфигурации дорожек для исходного контейнера.
			init := testInit(t)
			// wire накапливает корректный контейнер перед повреждением полей trun.
			var wire bytes.Buffer
			// m записывает исходный контейнер; err принимает результаты инициализации,
			// записи и разбора.
			m, err := NewMuxer(&wire, init, testLimits())
			if err != nil {
				t.Fatal(err)
			}
			if err = m.WritePacket(packet(t, init.Video, media.PacketInfo{}, []byte{1, 2, 3})); err != nil {
				t.Fatal(err)
			}
			// data содержит изменяемую копию контейнера, из которой затем исключается тело
			// mdat.
			data := bytes.Clone(wire.Bytes())
			// moof содержит найденное описание фрагмента в изменяемом буфере.
			moof, err := find(data, "moof")
			if err != nil {
				t.Fatal(err)
			}
			// traf содержит описание дорожки в найденном moof.
			traf, err := find(moof.data, "traf")
			if err != nil {
				t.Fatal(err)
			}
			// trun содержит изменяемые поля числа, размера и смещения пакетов.
			trun, err := find(traf.data, "trun")
			if err != nil {
				t.Fatal(err)
			}
			// expected задаёт ожидаемую категорию отказа для выбранного повреждения.
			expected := ErrLimit
			switch kind {
			case "sample-count":
				be.PutUint32(trun.data[4:8], math.MaxUint32)
			case "sample-size":
				be.PutUint32(trun.data[16:20], math.MaxUint32)
			case "data-offset":
				be.PutUint32(trun.data[8:12], math.MaxUint32)
				expected = ErrInvalid
			}
			// Заголовок mdat присутствует, но само тело намеренно не передаётся.
			data = data[:len(data)-3]
			// d читает повреждённое описание пакетов без доступной нагрузки mdat.
			d, err := NewDemuxer(bytes.NewReader(data), DemuxOptions{Limits: testLimits()})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = d.Next(); !errors.Is(err, expected) {
				t.Fatalf("нарушение %s дошло до payload: %v", kind, err)
			}
		})
	}
}

// Проверяет требование точного сохранения байтов строк формата через собственные метаданные, включая
// строку с некорректным UTF-8.
func TestConfigMetadataPreservesContractualStringBytes(t *testing.T) {
	// init содержит исходные дорожки для проверки точных байтов строк метаданных.
	init := testInit(t)
	// info содержит описание видео с намеренно некорректным UTF-8 в CodecProfile.
	info := init.Video.Info()
	info.Video.CodecProfile = "high\xff"
	// err принимает результаты создания конфигурации и последующих операций контейнера.
	var err error
	init.Video, err = media.NewCodecConfig(info, init.Video.Headers())
	if err != nil {
		t.Fatal(err)
	}
	// wire накапливает инициализацию с контрактными байтами строки профиля.
	var wire bytes.Buffer
	if _, err = NewMuxer(&wire, init, testLimits()); err != nil {
		t.Fatal(err)
	}
	// d восстанавливает конфигурации из собственных метаданных контейнера.
	d, err := NewDemuxer(&wire, DemuxOptions{Limits: testLimits()})
	if err != nil {
		t.Fatal(err)
	}
	if d.Init().Video.Info() != init.Video.Info() {
		t.Fatal("JSON изменил байты контрактных строк")
	}
}

// Проверяет отказ противоречащей SPS ширины или высоты до записи инициализации.
func TestH264ContradictoryDimensionsFailBeforeInitWrite(t *testing.T) {
	// field выбирает противоречащий SPS размер: ширину либо высоту.
	for _, field := range []string{"width", "height"} {
		t.Run(field, func(t *testing.T) {
			// Меняет выбранный размер видео вопреки SPS и проверяет отказ мультиплексора до записи инициализации.

			// init содержит исходную конфигурацию с согласованными размерами SPS.
			init := testInit(t)
			// info содержит описание видео с выбранным намеренно ложным размером.
			info := init.Video.Info()
			if field == "width" {
				info.Video.Width = 144
			} else {
				info.Video.Height = 144
			}
			// err принимает ошибку создания конфигурации с противоречащим SPS размером.
			var err error
			init.Video, err = media.NewCodecConfig(info, init.Video.Headers())
			if err != nil {
				t.Fatal(err)
			}
			// wire учитывает байты для проверки отказа мультиплексора до записи
			// инициализации.
			var wire bytes.Buffer
			// err содержит результат инициализации мультиплексора с противоречащим SPS
			// описанием.
			if _, err := NewMuxer(&wire, init, testLimits()); !errors.Is(err, ErrInvalid) || wire.Len() != 0 {
				t.Fatalf("описание вопреки SPS попало в транспорт: %v, bytes=%d", err, wire.Len())
			}
		})
	}
}

// Проверяет, что собственные метаданные, пользовательский преобразователь и стандартное восстановление
// не позволяют объявить размеры вопреки SPS.
func TestDemuxH264SPSDimensionsConstrainEveryMetadataSource(t *testing.T) {
	// source выбирает собственные метаданные, пользовательский преобразователь либо стандартное
	// восстановление.
	for _, source := range []string{"uuid", "resolver", "default"} {
		t.Run(source, func(t *testing.T) {
			// Для выбранного источника метаданных проверяет, что ширина записи контейнера и описания не может
			// противоречить SPS.

			// init содержит исходную согласованную конфигурацию видео.
			init := testInit(t)
			// wire накапливает корректную инициализацию перед подменой заявленной ширины.
			var wire bytes.Buffer
			// err сообщает об отказе записи исходной согласованной инициализации.
			if _, err := NewMuxer(&wire, init, testLimits()); err != nil {
				t.Fatal(err)
			}
			// data содержит изменяемую копию контейнера для подмены ширины и выбора
			// источника метаданных.
			data := bytes.Clone(wire.Bytes())
			// entry задаёт положение сигнатуры avc1 для проверки и изменения ширины записи.
			entry := bytes.Index(data, []byte("avc1"))
			if entry < 4 || be.Uint16(data[entry+28:entry+30]) != 128 {
				t.Fatal("нет ожидаемой video sample entry")
			}
			be.PutUint16(data[entry+28:entry+30], 144)
			// options задаёт ограничения, идентичность дорожек и при необходимости
			// пользовательский преобразователь.
			options := DemuxOptions{Limits: testLimits(), SourceEpoch: 1, VideoRevision: 1, AudioRevision: 1}
			if source == "uuid" {
				if bytes.Count(data, []byte(`"Width":128`)) != 1 {
					t.Fatal("нет ожидаемого размера в UUID")
				}
				data = bytes.Replace(data, []byte(`"Width":128`), []byte(`"Width":144`), 1)
			} else {
				// uuid задаёт положение собственных метаданных, скрываемых заменой типа
				// блока на free.
				uuid := bytes.Index(data, []byte("uuid"))
				if uuid < 4 {
					t.Fatal("нет ожидаемого UUID")
				}
				copy(data[uuid:uuid+4], "free")
				if source == "resolver" {
					options.ResolveConfig = func(desc TrackDescription) (media.CodecConfig, error) {
						// Возвращает обычную конфигурацию аудио, а для видео подставляет противоречащую SPS ширину из записи
						// контейнера.

						if desc.Track == media.TrackAudio {
							return ResolveCodecConfig(desc, 1, 1)
						}
						// info содержит исходное описание видео, которому
						// преобразователь присваивает ложную ширину записи MP4.
						info := init.Video.Info()
						info.Video.Width = desc.Width
						return media.NewCodecConfig(info, desc.Headers)
					}
				}
			}
			// err содержит результат чтения противоречивой инициализации с выбранным
			// источником метаданных.
			if _, err := NewDemuxer(bytes.NewReader(data), options); !errors.Is(err, ErrInvalid) {
				t.Fatalf("%s обошёл проверку SPS: %v", source, err)
			}
		})
	}
}

// FuzzBoundedDemuxer проверяет завершение разбора входов до 8192 байтов не позднее ста вызовов Next,
// начиная с корректного и пустого контейнеров.
func FuzzBoundedDemuxer(f *testing.F) {
	// init содержит конфигурации дорожек для корректного начального примера фаззинга.
	init := testInit(f)
	// wire накапливает корректную инициализацию и пакет для начального корпуса фаззинга.
	var wire bytes.Buffer
	// m записывает начальный контейнер; err принимает ошибки его инициализации и записи пакета.
	m, err := NewMuxer(&wire, init, testLimits())
	if err != nil {
		f.Fatal(err)
	}
	if err = m.WritePacket(packet(f, init.Video, media.PacketInfo{Keyframe: true}, []byte{1, 2, 3})); err != nil {
		f.Fatal(err)
	}
	f.Add(wire.Bytes())
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		// Пробует разобрать произвольный ввод до 8192 байтов и требует завершения чтения не позднее ста
		// вызовов Next.

		if len(data) > 8192 {
			t.Skip()
		}
		// d читает текущий произвольный ввод; err принимает ошибку инициализации и последующих
		// вызовов Next.
		d, err := NewDemuxer(bytes.NewReader(data), DemuxOptions{Limits: testLimits(), SourceEpoch: 1, VideoRevision: 1, AudioRevision: 1})
		if err != nil {
			return
		}
		for range 100 {
			if _, err = d.Next(); err != nil {
				return
			}
		}
		t.Fatal("ограниченный ввод не завершился")
	})
}
