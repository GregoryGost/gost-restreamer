package fmp4_test

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

// Возвращает ограничения независимых проверок fMP4 с максимумом 32 описаний единиц доступа.
func acceptanceLimits() fmp4.Limits {
	return fmp4.Limits{MaxBoxBytes: 1 << 20, MaxInitBytes: 1 << 20, MaxMoofBytes: 4096, MaxMdatBytes: 65536, MaxSampleBytes: 32768, MaxExtradataBytes: 4096, MaxSamples: 32}
}

// Создаёт эталонную пару H.264/AAC с разными шкалами и общим сдвигом начала на четыре секунды.
func acceptanceInit(t *testing.T) fmp4.Init {
	t.Helper()
	// Синтетический источник получен через libx264; ffprobe подтвердил 128×128, 30 FPS, High и yuv420p.
	// headers содержит эталон avcC H.264 High 128×128/30; err сообщает об ошибке декодирования его
	// записи.
	headers, err := hex.DecodeString("0164000bffe100186764000bacd942046c0440000003004000000f03c50a658001000668ebe3cb22c0fdf8f800")
	if err != nil {
		t.Fatal(err)
	}
	// v содержит видеоконфигурацию с временной шкалой 1/30000 и ревизией 3.
	v, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: 9, Revision: 3, TimeBase: media.TimeBase{Numerator: 1, Denominator: 30000}, Video: core.VideoFormat{Codec: core.CodecH264, Width: 128, Height: 128, FrameRate: core.FrameRate{Numerator: 30, Denominator: 1}, CodecProfile: "high", PixelFormat: "yuv420p", BitDepth: 8, Color: core.ColorInfo{Range: "limited", Primaries: "unspecified", Transfer: "unspecified", Matrix: "unspecified"}}}, headers)
	if err != nil {
		t.Fatal(err)
	}
	// a содержит конфигурацию AAC-LC, 48 кГц и двух каналов с ревизией 8.
	a, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackAudio, SourceEpoch: 9, Revision: 8, TimeBase: media.TimeBase{Numerator: 1, Denominator: 48000}, Audio: core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", SampleRateHz: 48000, Channels: 2, ChannelLayout: "stereo"}}, []byte{0x11, 0x90})
	if err != nil {
		t.Fatal(err)
	}
	return fmp4.Init{Video: v, Audio: a, Origin: fmp4.OriginMapping{OffsetTicks: 4, TimeBase: media.TimeBase{Numerator: 1, Denominator: 1}}}
}

// Создаёт пакет независимой проверки с заданными метками и данными, завершая тест при ошибке.
func acceptancePacket(t *testing.T, c media.CodecConfig, info media.PacketInfo, payload []byte) media.Packet {
	t.Helper()
	// p содержит созданный тестовый пакет; err сообщает об отказе проверки его данных.
	p, err := media.NewPacket(info, c, payload)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

// acceptanceFragments ограничивает размер отдельного чтения и записи для проверки частичного
// ввода-вывода.
type acceptanceFragments struct {
	// r задаёт источник для чтений не более одного байта.
	r io.Reader
	// w задаёт приёмник для записей не более трёх байтов.
	w io.Writer
}

// Передаёт не более одного байта за чтение для проверки фрагментированного ввода.
func (f acceptanceFragments) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return f.r.Read(p)
}

// Передаёт не более трёх байтов за запись для проверки фрагментированного вывода.
func (f acceptanceFragments) Write(p []byte) (int, error) {
	if len(p) > 3 {
		p = p[:3]
	}
	return f.w.Write(p)
}

// Проверяет точное сохранение дробной шкалы, переставленных PTS и нагрузки при частичном вводе-выводе
// с явной заменой SourceEpoch и ревизий при чтении.
func TestAcceptanceFractionalTimeBaseAndReorderedPresentation(t *testing.T) {
	// init содержит исходную пару дорожек и общий сдвиг перед изменением шкалы видео.
	init := acceptanceInit(t)
	// info содержит изменяемое описание видео для задания дробной временной шкалы.
	info := init.Video.Info()
	info.TimeBase.Numerator = 1001
	// err принимает ошибку пересоздания видеоконфигурации с дробной шкалой.
	var err error
	init.Video, err = media.NewCodecConfig(info, init.Video.Headers())
	if err != nil {
		t.Fatal(err)
	}
	// wire накапливает сериализованный контейнер для последующего чтения малыми порциями.
	var wire bytes.Buffer
	// m записывает пакеты через обёртку с частичными записями.
	m, err := fmp4.NewMuxer(acceptanceFragments{w: &wire}, init, acceptanceLimits())
	if err != nil {
		t.Fatal(err)
	}
	// Метки заданы независимо: порядок декодирования возрастает, порядок показа меняется.
	// packets задаёт эталонный порядок пакетов с отрицательными DTS и переставленными PTS.
	packets := []media.Packet{
		acceptancePacket(t, init.Video, media.PacketInfo{DTS: -30, PTS: -28, DurationTicks: 1, Keyframe: true}, []byte{1, 2}),
		acceptancePacket(t, init.Audio, media.PacketInfo{DTS: -24000, PTS: -24000, DurationTicks: 1024}, []byte{3}),
		acceptancePacket(t, init.Video, media.PacketInfo{DTS: -29, PTS: -30, DurationTicks: 1}, []byte{4, 5}),
		acceptancePacket(t, init.Video, media.PacketInfo{DTS: -28, PTS: -29, DurationTicks: 1}, []byte{6}),
		acceptancePacket(t, init.Audio, media.PacketInfo{DTS: -22976, PTS: -22976, DurationTicks: 1024}, []byte{7}),
	}
	// p содержит очередной эталонный пакет, передаваемый мультиплексору.
	for _, p := range packets {
		// err сообщает об отказе записи эталонного пакета.
		if err := m.WritePacket(p); err != nil {
			t.Fatal(err)
		}
	}
	// d читает контейнер по одному байту с явной заменой эпохи и ревизий.
	d, err := fmp4.NewDemuxer(acceptanceFragments{r: bytes.NewReader(wire.Bytes())}, fmp4.DemuxOptions{Limits: acceptanceLimits(), SourceEpoch: 31, VideoRevision: 17, AudioRevision: 18})
	if err != nil {
		t.Fatal(err)
	}
	// i задаёт номер пакета, want — исходный эталон для точного сравнения после чтения.
	for i, want := range packets {
		// got содержит прочитанный пакет; err сообщает об отказе его чтения.
		got, err := d.Next()
		if err != nil {
			t.Fatalf("пакет %d: %v", i, err)
		}
		// info содержит восстановленное описание дорожки прочитанного пакета.
		info := got.Config().Info()
		// revision задаёт ожидаемую заменённую ревизию видео либо аудио.
		revision := uint64(17)
		if info.Track == media.TrackAudio {
			revision = 18
		}
		if got.Info() != want.Info() || !bytes.Equal(got.Payload(), want.Payload()) || !bytes.Equal(got.Config().Headers(), want.Config().Headers()) || info.SourceEpoch != 31 || info.Revision != revision || info.TimeBase != want.Config().Info().TimeBase {
			t.Fatalf("изменились время, данные или идентичность пакета %d: %+v, %+v", i, got.Info(), info)
		}
	}
	// err содержит результат чтения после последнего пакета для проверки EOF.
	if _, err := d.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("после всех пакетов ожидался EOF: %v", err)
	}
}

// acceptanceBox читает структуру синтетических данных независимо от рабочего декодера.
func acceptanceBox(t *testing.T, data []byte, path ...string) []byte {
	t.Helper()
	for len(data) >= 8 {
		// n содержит полный размер текущего блока для проверки границ и поиска по пути.
		n := int(binary.BigEndian.Uint32(data[:4]))
		if n < 8 || n > len(data) {
			t.Fatal("некорректная структура fixture")
		}
		if string(data[4:8]) == path[0] {
			if len(path) == 1 {
				return data[8:n]
			}
			return acceptanceBox(t, data[8:n], path[1:]...)
		}
		data = data[n:]
	}
	t.Fatalf("в fixture нет box %v", path)
	return nil
}

// Проверяет отказ инициализации, когда число каналов или частота в mp4a противоречат ASC и полному
// описанию.
func TestAcceptanceInitRejectsContradictoryAudioDescription(t *testing.T) {
	// field выбирает противоречащее ASC поле mp4a: число каналов либо частоту.
	for _, field := range []string{"channels", "sample-rate"} {
		t.Run(field, func(t *testing.T) {
			// Изменяет выбранное поле mp4a вопреки ASC и сохранённому описанию, проверяя отказ при разборе
			// инициализации.

			// wire накапливает корректную инициализацию перед повреждением описания аудио.
			var wire bytes.Buffer
			// err сообщает об отказе записи исходной корректной инициализации.
			if _, err := fmp4.NewMuxer(&wire, acceptanceInit(t), acceptanceLimits()); err != nil {
				t.Fatal(err)
			}
			// data содержит отдельную изменяемую копию инициализации для создания
			// противоречия.
			data := bytes.Clone(wire.Bytes())
			// mp4a имеет уникальную сигнатуру в синтетических данных; длина блока проверяется отдельно.
			// pos задаёт положение сигнатуры mp4a, после которой расположено тело записи
			// аудио.
			pos := bytes.Index(data, []byte("mp4a"))
			if pos < 4 || int(binary.BigEndian.Uint32(data[pos-4:pos])) < 36 {
				t.Fatal("в fixture нет корректного mp4a")
			}
			// entry ссылается на тело mp4a в изменяемом буфере.
			entry := data[pos+4:]
			if field == "channels" {
				binary.BigEndian.PutUint16(entry[16:18], 1)
			} else {
				binary.BigEndian.PutUint32(entry[24:28], 44100<<16)
			}
			// err содержит результат открытия инициализации с противоречащим ASC описанием.
			if _, err := fmp4.NewDemuxer(bytes.NewReader(data), fmp4.DemuxOptions{Limits: acceptanceLimits()}); err == nil {
				t.Fatal("init принят: sample entry противоречит AAC ASC и полному описанию")
			}
		})
	}
}

// Проверяет отказ ResolveCodecConfig при несовпадении частоты либо числа каналов записи MP4 с ASC.
func TestAcceptanceAudioResolverRejectsASCContradiction(t *testing.T) {
	// desc содержит описание AAC с частотой либо числом каналов, противоречащими заголовкам ASC.
	for _, desc := range []fmp4.TrackDescription{
		{Track: media.TrackAudio, Codec: core.CodecAAC, TimeBase: media.TimeBase{Numerator: 1, Denominator: 48000}, SampleRateHz: 44100, Channels: 2, Headers: []byte{0x11, 0x90}},
		{Track: media.TrackAudio, Codec: core.CodecAAC, TimeBase: media.TimeBase{Numerator: 1, Denominator: 48000}, SampleRateHz: 48000, Channels: 1, Headers: []byte{0x11, 0x90}},
	} {
		// err содержит результат восстановления конфигурации из противоречивого описания аудио.
		if _, err := fmp4.ResolveCodecConfig(desc, 1, 1); err == nil {
			t.Errorf("sample entry противоречит ASC: sample_rate=%d channels=%d", desc.SampleRateHz, desc.Channels)
		}
	}
}

// Проверяет непосредственно trun: H.264 сохраняет отрицательное смещение в шкале дорожки, AV1 не
// содержит поля композиционного смещения.
func TestAcceptanceMuxWireRetainsSignedCTSAndAV1OmitsCTS(t *testing.T) {
	// codec выбирает H.264 либо AV1 для проверки представления композиционного смещения.
	for _, codec := range []core.Codec{core.CodecH264, core.CodecAV1} {
		t.Run(string(codec), func(t *testing.T) {
			// Проверяет отрицательное знаковое смещение показа H.264 в trun либо отсутствие поля смещения у AV1
			// при дробной временной шкале.

			// init содержит исходную пару дорожек перед заменой видеокодека и шкалы.
			init := acceptanceInit(t)
			// info содержит изменяемое описание выбранного видеокодека с дробной шкалой.
			info := init.Video.Info()
			info.Video.Codec = codec
			info.TimeBase.Numerator = 1001
			// headers содержит заголовки H.264 либо синтетический av1C для выбранного
			// сценария.
			headers := init.Video.Headers()
			if codec == core.CodecAV1 {
				info.Video.CodecProfile = "Main"
				headers = []byte{0x81, 0, 0, 0}
			}
			// err принимает ошибку создания конфигурации выбранного видеокодека.
			var err error
			init.Video, err = media.NewCodecConfig(info, headers)
			if err != nil {
				t.Fatal(err)
			}
			// wire накапливает инициализацию и пакет для независимого просмотра trun.
			var wire bytes.Buffer
			// m сериализует пакет выбранного кодека в тестовый контейнер.
			m, err := fmp4.NewMuxer(&wire, init, acceptanceLimits())
			if err != nil {
				t.Fatal(err)
			}
			// pts задаёт отрицательное время показа H.264 либо совпадающее с DTS время AV1.
			pts := int64(-2)
			if codec == core.CodecAV1 {
				pts = 0
			}
			// err сообщает об отказе записи пакета для проверки поля композиционного
			// смещения.
			if err := m.WritePacket(acceptancePacket(t, init.Video, media.PacketInfo{DTS: 0, PTS: pts, DurationTicks: 1, Keyframe: true}, []byte{9})); err != nil {
				t.Fatal(err)
			}
			// trun ссылается на тело сериализованного блока с описанием пакета.
			trun := acceptanceBox(t, wire.Bytes(), "moof", "traf", "trun")
			// flags содержит 24-битные флаги trun для проверки наличия поля композиционного
			// смещения.
			flags := binary.BigEndian.Uint32(trun[:4]) & 0xffffff
			if codec == core.CodecAV1 {
				if flags&0x800 != 0 {
					t.Fatal("AV1 содержит запрещённое поле composition offset")
				}
			} else if trun[0] != 1 || flags&0x800 == 0 || int32(binary.BigEndian.Uint32(trun[len(trun)-4:])) != -2002 {
				t.Fatal("H.264 потерял отрицательное смещение -2002 в шкале 1/30000")
			}
		})
	}
}

// Проверяет, что отказ непредставимого времени не записывает байты и не мешает следующему корректному
// пакету.
func TestAcceptanceRejectedTimingDoesNotPoisonFollowingPacket(t *testing.T) {
	// init содержит корректную пару дорожек для проверки восстановления после отклонённого времени.
	init := acceptanceInit(t)
	// wire накапливает контейнер для контроля отсутствия записи отклонённого пакета.
	var wire bytes.Buffer
	// m записывает контейнер; err сообщает об отказе его инициализации.
	m, err := fmp4.NewMuxer(&wire, init, acceptanceLimits())
	if err != nil {
		t.Fatal(err)
	}
	// before фиксирует размер транспорта до попытки записи непредставимого времени.
	before := wire.Len()
	// bad содержит пакет с предельными метками, не представимыми в контейнерной шкале со сдвигом.
	bad := acceptancePacket(t, init.Video, media.PacketInfo{DTS: math.MaxInt64, PTS: math.MaxInt64}, []byte{1})
	// err содержит результат отклонённой записи для проверки категории ошибки и неизменности
	// транспорта.
	if err := m.WritePacket(bad); !errors.Is(err, fmp4.ErrUnsupportedTiming) || wire.Len() != before {
		t.Fatalf("непредставимый пакет изменил транспорт: %v", err)
	}
	// good содержит допустимый пакет для проверки следующей записи после отказа.
	good := acceptancePacket(t, init.Video, media.PacketInfo{DTS: 0, PTS: 1}, []byte{2})
	// err сообщает об отказе записи допустимого пакета после предыдущего отклонения.
	if err := m.WritePacket(good); err != nil {
		t.Fatal("ошибка до записи испортила исправный транспорт:", err)
	}
}

// acceptanceCountingReader учитывает фактически прочитанный объём для проверок отказа до нагрузки.
type acceptanceCountingReader struct {
	// r задаёт вложенный источник байтов контейнера.
	r io.Reader
	// bytes накапливает число фактически прочитанных байтов.
	bytes int
}

// Передаёт чтение вложенному Reader и считает фактически прочитанные байты для проверки раннего
// отказа.
func (r *acceptanceCountingReader) Read(p []byte) (int, error) {
	// n содержит объём текущего чтения; err сохраняет ошибку вложенного источника.
	n, err := r.r.Read(p)
	r.bytes += n
	return n, err
}

// Проверяет категорию ошибки чрезмерного числа единиц доступа или неверного смещения и отсутствие
// чтения их нагрузки.
func TestAcceptanceDemuxRejectsSampleCountAndOffsetBeforePayload(t *testing.T) {
	// violation выбирает превышение числа пакетов либо некорректное смещение нагрузки.
	for _, violation := range []string{"sample-count", "sample-offset"} {
		t.Run(violation, func(t *testing.T) {
			// Завышает число единиц доступа либо смещение данных и проверяет ожидаемый отказ до чтения полезной
			// нагрузки.

			// wire накапливает корректный контейнер перед изменением trun.
			var wire bytes.Buffer
			// init содержит конфигурации дорожек для пакета с трёхбайтовой нагрузкой.
			init := acceptanceInit(t)
			// m записывает исходный контейнер; err сообщает об отказе его инициализации.
			m, err := fmp4.NewMuxer(&wire, init, acceptanceLimits())
			if err != nil {
				t.Fatal(err)
			}
			// err сообщает об отказе записи исходного пакета перед повреждением контейнера.
			if err := m.WritePacket(acceptancePacket(t, init.Video, media.PacketInfo{DTS: 0, PTS: 0, DurationTicks: 1, Keyframe: true}, []byte{1, 2, 3})); err != nil {
				t.Fatal(err)
			}
			// data содержит независимую изменяемую копию сериализованного контейнера.
			data := bytes.Clone(wire.Bytes())
			// trun ссылается на изменяемое описание числа пакетов и смещения нагрузки.
			trun := acceptanceBox(t, data, "moof", "traf", "trun")
			// wantErr задаёт ожидаемую категорию отказа для выбранного нарушения.
			wantErr := fmp4.ErrLimit
			if violation == "sample-count" {
				binary.BigEndian.PutUint32(trun[4:8], acceptanceLimits().MaxSamples+1)
			} else {
				binary.BigEndian.PutUint32(trun[8:12], math.MaxInt32)
				wantErr = fmp4.ErrInvalid
			}
			// reader учитывает чтения повреждённого контейнера для проверки раннего отказа.
			reader := &acceptanceCountingReader{r: bytes.NewReader(data)}
			// d читает контейнер с повреждённым trun для проверки отказа до нагрузки.
			d, err := fmp4.NewDemuxer(reader, fmp4.DemuxOptions{Limits: acceptanceLimits()})
			if err != nil {
				t.Fatal(err)
			}
			// err содержит результат Next для сравнения с ожидаемой категорией нарушения.
			if _, err := d.Next(); !errors.Is(err, wantErr) {
				t.Fatalf("недопустимый sample принят: %v", err)
			}
			if reader.bytes > len(data)-3 {
				t.Fatal("payload прочитан до проверки count/offset")
			}
		})
	}
}

// Возвращает инициализацию с эталонными SPS и описанием H.264 High 128×128/30.
func acceptanceH264Init(t *testing.T) fmp4.Init {
	t.Helper()
	// init содержит исходную пару дорожек перед явным закреплением параметров H.264.
	init := acceptanceInit(t)
	// info содержит изменяемое описание видео для согласования с эталонным SPS.
	info := init.Video.Info()
	info.Video.Width, info.Video.Height = 128, 128
	info.Video.FrameRate = core.FrameRate{Numerator: 30, Denominator: 1}
	// Синтетический источник получен через libx264; ffprobe подтвердил 128×128, 30 FPS, High и yuv420p.
	// headers содержит эталон avcC H.264 High 128×128/30; err сообщает об ошибке декодирования его
	// записи.
	headers, err := hex.DecodeString("0164000bffe100186764000bacd942046c0440000003004000000f03c50a658001000668ebe3cb22c0fdf8f800")
	if err != nil {
		t.Fatal(err)
	}
	init.Video, err = media.NewCodecConfig(info, headers)
	if err != nil {
		t.Fatal(err)
	}
	return init
}

// Проверяет отказ при противоречии кодека или ширины между записью MP4, собственными метаданными и
// пользовательским преобразователем.
func TestAcceptanceVideoDescriptionCannotContradictSampleEntry(t *testing.T) {
	// violation выбирает противоречие ширины либо кодека в контейнере или пользовательском
	// преобразователе.
	for _, violation := range []string{"width", "codec", "resolver-width", "resolver-codec"} {
		t.Run(violation, func(t *testing.T) {
			// Создаёт выбранное противоречие кодека либо ширины между описанием видео и записью контейнера и
			// проверяет отказ демультиплексора.

			// init содержит согласованную конфигурацию H.264 перед созданием противоречия.
			init := acceptanceH264Init(t)
			// wire накапливает исходную инициализацию с собственными метаданными.
			var wire bytes.Buffer
			// err сообщает об отказе записи согласованной инициализации.
			if _, err := fmp4.NewMuxer(&wire, init, acceptanceLimits()); err != nil {
				t.Fatal(err)
			}
			// data содержит изменяемую копию инициализации для повреждения записи либо
			// скрытия UUID.
			data := bytes.Clone(wire.Bytes())
			// options задаёт ограничения чтения и при необходимости пользовательское
			// восстановление конфигурации.
			options := fmp4.DemuxOptions{Limits: acceptanceLimits()}
			switch violation {
			case "width":
				// pos задаёт положение avc1 для изменения заявленной ширины.
				pos := bytes.Index(data, []byte("avc1"))
				if pos < 4 {
					t.Fatal("нет avc1")
				}
				binary.BigEndian.PutUint16(data[pos+4+24:pos+4+26], 144)
			case "codec":
				// pos задаёт положение avc1, затем avcC для подмены обозначений
				// видеокодека.
				pos := bytes.Index(data, []byte("avc1"))
				if pos < 4 {
					t.Fatal("нет avc1")
				}
				copy(data[pos:pos+4], "av01")
				pos = bytes.Index(data, []byte("avcC"))
				if pos < 4 {
					t.Fatal("нет avcC")
				}
				copy(data[pos:pos+4], "av1C")
			default:
				// pos задаёт положение UUID для замены его типом free перед вызовом
				// пользовательского преобразователя.
				pos := bytes.Index(data, []byte("uuid"))
				if pos < 4 {
					t.Fatal("нет metadata uuid")
				}
				copy(data[pos:pos+4], "free")
				options.ResolveConfig = func(desc fmp4.TrackDescription) (media.CodecConfig, error) {
					// Для аудио использует обычное восстановление, а для видео возвращает конфигурацию с выбранным
					// противоречием ширины либо кодека.

					if desc.Track == media.TrackAudio {
						return fmp4.ResolveCodecConfig(desc, 9, 1)
					}
					// info содержит копию исходного описания видео, в которую
					// преобразователь вносит выбранное противоречие.
					info := init.Video.Info()
					if violation == "resolver-width" {
						info.Video.Width = 144
					} else {
						info.Video.Codec = core.CodecAV1
					}
					return media.NewCodecConfig(info, desc.Headers)
				}
			}
			// err содержит результат открытия инициализации с противоречивым описанием
			// видео.
			if _, err := fmp4.NewDemuxer(bytes.NewReader(data), options); err == nil {
				t.Fatal("противоречие video metadata и sample entry принято")
			}
		})
	}
}

// Проверяет, что согласованное ложное описание в UUID и записи MP4 не обходит реальные размеры SPS;
// ранний отказ Muxer также допустим.
func TestAcceptanceRetainedMetadataCannotOverrideH264SPSDimensions(t *testing.T) {
	// init содержит исходную конфигурацию с SPS шириной 128 перед подменой заявленного размера.
	init := acceptanceH264Init(t)
	// info содержит описание видео с намеренно ложной шириной 144.
	info := init.Video.Info()
	info.Video.Width = 144
	// err принимает ошибку создания конфигурации с ложным заявленным размером.
	var err error
	init.Video, err = media.NewCodecConfig(info, init.Video.Headers())
	if err != nil {
		t.Fatal(err)
	}
	// wire накапливает инициализацию с согласованными между собой, но противоречащими SPS
	// метаданными и записью MP4.
	var wire bytes.Buffer
	// err содержит результат ранней проверки ложного размера мультиплексором; отказ удовлетворяет
	// сценарию.
	if _, err := fmp4.NewMuxer(&wire, init, acceptanceLimits()); err != nil {
		return
	}
	// err содержит результат проверки ложного размера демультиплексором, если мультиплексор его
	// пропустил.
	if _, err := fmp4.NewDemuxer(bytes.NewReader(wire.Bytes()), fmp4.DemuxOptions{Limits: acceptanceLimits()}); err == nil {
		t.Fatal("UUID и sample entry объявили 144×128 вопреки реальному SPS 128×128")
	}
}

// Проверяет приём истинных размеров SPS стандартным преобразователем и отказ после изменения только
// заявленной ширины.
func TestAcceptanceDefaultVideoResolverChecksActualSPSDimensions(t *testing.T) {
	// init содержит конфигурацию H.264 с эталонным SPS для проверки стандартного преобразователя.
	init := acceptanceH264Init(t)
	// desc содержит описание видеодорожки, сначала согласованное с SPS, затем с изменённой шириной.
	desc := fmp4.TrackDescription{Track: media.TrackVideo, Codec: core.CodecH264, TimeBase: init.Video.Info().TimeBase, Width: 128, Height: 128, Headers: init.Video.Headers()}
	// err содержит результат восстановления конфигурации из согласованного описания и SPS.
	if _, err := fmp4.ResolveCodecConfig(desc, 1, 1); err != nil {
		t.Fatal("валидный фактический SPS отвергнут:", err)
	}
	desc.Width = 144
	// err содержит результат восстановления конфигурации после подмены заявленной ширины.
	if _, err := fmp4.ResolveCodecConfig(desc, 1, 1); err == nil {
		t.Fatal("SPS не ограничил заявленные размеры sample entry")
	}
}
