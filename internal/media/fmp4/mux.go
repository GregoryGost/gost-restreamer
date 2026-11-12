package fmp4

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
)

// metadataID отличает собственный блок uuid с полным JSON-описанием конфигураций от посторонних блоков
// uuid.
var metadataID = [16]byte{0xf5, 0x8b, 0x13, 0xca, 0x41, 0x4c, 0x4f, 0x16, 0x99, 0x87, 0x77, 0x03, 0x4b, 0x2b, 0x5c, 0x91}

// configMetadata сохраняет версию собственного описания, исходную идентичность двух дорожек и полный
// медиаформат для точного восстановления.
type configMetadata struct {
	// Version задаёт версию собственного JSON-представления; текущий читатель принимает только 1.
	Version uint32
	// Video сохраняет идентичность и шкалу видеодорожки, Audio — аудиодорожки, независимо от целых
	// шкал MP4.
	Video, Audio trackMetadata
	// Format содержит полные параметры видео и аудио, дополняющие сведения записей формата MP4.
	Format core.MediaFormat
}

// trackMetadata сохраняет исходную эпоху, ревизию и рациональную шкалу одной дорожки в собственных
// метаданных.
type trackMetadata struct {
	// SourceEpoch связывает дорожку с исходной эпохой A/V; Revision различает конфигурации этой
	// дорожки.
	SourceEpoch, Revision uint64
	// TimeBase сохраняет исходную длительность отсчёта конфигурации для восстановления временных
	// меток после контейнерной шкалы.
	TimeBase media.TimeBase
}

// Собирает описание выбранной дорожки из общих метаданных формата и её отдельной идентичности.
func (m configMetadata) info(track media.Track) media.CodecConfigInfo {
	// identity выбирает отдельные эпоху, ревизию и шкалу запрошенной аудио- либо видеодорожки.
	identity := m.Video
	if track == media.TrackAudio {
		identity = m.Audio
	}
	// info накапливает полное описание выбранной дорожки с её идентичностью и соответствующей частью
	// общего формата.
	info := media.CodecConfigInfo{Track: track, SourceEpoch: identity.SourceEpoch, Revision: identity.Revision, TimeBase: identity.TimeBase}
	if track == media.TrackVideo {
		info.Video = m.Format.Video
	} else {
		info.Audio = m.Format.Audio
	}
	return info
}

// Muxer пишет init сразу, затем отдельный moof/mdat для каждого пакета.
// При ошибке ввода-вывода транспорт становится непригодным для дальнейших записей.
type Muxer struct {
	// w принимает сериализованные байты транспорта; закрытием Writer и ограничением ожидания владеет
	// вызывающий.
	w io.Writer
	// init фиксирует пару конфигураций и общий сдвиг на весь срок текущего транспорта.
	init Init
	// limits хранит проверенные пределы блоков, заголовков и нагрузки, применяемые до записи.
	limits Limits
	// sequence считает полностью записанные фрагменты; следующий mfhd получает номер sequence+1.
	sequence uint32
	// last хранит DTS последнего полностью записанного пакета каждой дорожки в её целой контейнерной
	// шкале.
	last map[media.Track]int64
	// err сохраняет отказ записи, после которого продолжение этого транспорта запрещено.
	err error
}

// NewMuxer проверяет конфигурации и бюджеты, сериализует и сразу записывает инициализацию в переданный
// Writer; закрытием Writer владеет вызывающий.
func NewMuxer(w io.Writer, init Init, limits Limits) (*Muxer, error) {
	if w == nil {
		return nil, ErrInvalid
	}
	// err сообщает о недопустимых либо несогласованных бюджетах контейнера.
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	// err сообщает о недопустимой паре конфигураций или непредставимой общей шкале A/V.
	if err := init.Validate(); err != nil {
		return nil, err
	}
	// data содержит готовую инициализацию ftyp/moov; err сообщает об отказе её построения до первой
	// записи.
	data, err := marshalInit(init, limits)
	if err != nil {
		return nil, err
	}
	if err = writeAll(w, data); err != nil {
		return nil, err
	}
	return &Muxer{w: w, init: init, limits: limits, last: make(map[media.Track]int64)}, nil
}

// Сравнивает точные описания конфигураций и байты заголовков.
func sameConfig(a, b media.CodecConfig) bool {
	return a.Info() == b.Info() && bytes.Equal(a.Headers(), b.Headers())
}

// Сравнивает длину и байты заголовков конфигурации с переданным буфером.
func sameHeaders(c media.CodecConfig, headers []byte) bool {
	return c.StoredBytes() == uint64(len(headers)) && bytes.Equal(c.Headers(), headers)
}

// WritePacket записывает один пакет отдельной парой moof/mdat при неизменной конфигурации и
// неубывающем DTS дорожки. Проверяет точный пересчёт, сдвиг и бюджеты до записи; ошибка ввода-вывода
// сохраняется и запрещает дальнейшие записи.
func (m *Muxer) WritePacket(p media.Packet) error {
	if m.err != nil {
		return m.err
	}
	// err сообщает о некорректной структуре входной единицы доступа.
	if err := p.Validate(); err != nil {
		return err
	}
	// c содержит неизменяемую конфигурацию входного пакета для сверки с инициализацией.
	c := p.Config()
	// info содержит роль дорожки, формат и исходную временную шкалу пакета.
	info := c.Info()
	if c.StoredBytes() > m.limits.MaxExtradataBytes {
		return ErrLimit
	}
	// expected выбирает конфигурацию соответствующей дорожки из Init, id — её фиксированный
	// идентификатор MP4: 1 для видео и 2 для аудио.
	expected, id := m.init.Video, uint32(1)
	if info.Track == media.TrackAudio {
		expected, id = m.init.Audio, 2
	}
	if !sameConfig(c, expected) {
		return ErrConfigChanged
	}
	// size содержит размер сжатой нагрузки без заголовков конфигурации и обрамления MP4.
	size := p.StoredBytes() - c.StoredBytes()
	if size > m.limits.MaxSampleBytes || size+8 > m.limits.MaxMdatBytes || size+8 > m.limits.MaxBoxBytes {
		return ErrLimit
	}
	if m.sequence == math.MaxUint32 {
		return fmt.Errorf("%w: sequence number", ErrLimit)
	}
	// scale содержит точную целую шкалу текущей дорожки; err сообщает о невозможности её
	// представить.
	scale, err := trackScale(info.TimeBase)
	if err != nil {
		return err
	}
	// base задаёт длительность одного отсчёта временных полей MP4 текущей дорожки.
	base := media.TimeBase{Numerator: 1, Denominator: scale}
	// pi содержит временные метки, длительность и заявленный признак независимого кадра входного
	// пакета.
	pi := p.Info()
	// dts содержит точно переведённое в контейнерную шкалу время декодирования до общего сдвига.
	dts, err := rescale(pi.DTS, info.TimeBase, base)
	if err != nil {
		return err
	}
	// pts содержит точно переведённое в контейнерную шкалу время показа до общего сдвига.
	pts, err := rescale(pi.PTS, info.TimeBase, base)
	if err != nil {
		return err
	}
	// duration содержит длительность единицы доступа в контейнерных отсчётах для поля trun.
	duration, err := rescale(pi.DurationTicks, info.TimeBase, base)
	if err != nil {
		return err
	}
	// shift задаёт точный общий сдвиг начала в контейнерной шкале текущей дорожки.
	shift, err := m.init.Origin.ticks(scale)
	if err != nil {
		return err
	}
	// storedDTS содержит неотрицательное сдвинутое время, записываемое в tfdt.
	storedDTS, err := add(dts, shift)
	if err != nil || storedDTS < 0 {
		return ErrUnsupportedTiming
	}
	// cts хранит разность PTS и DTS произвольной точности до проверки знакового 32-битного поля
	// trun.
	cts := newDifference(pts, dts)
	if !cts.IsInt64() || cts.Int64() < math.MinInt32 || cts.Int64() > math.MaxInt32 || duration < 0 || duration > math.MaxUint32 {
		return ErrUnsupportedTiming
	}
	if info.Track == media.TrackVideo && info.Video.Codec == core.CodecAV1 && pts != dts {
		return fmt.Errorf("%w: AV1SampleEntry требует PTS = DTS", ErrUnsupportedTiming)
	}
	// previous содержит последний записанный DTS этой дорожки; ok определяет, требуется ли сравнение
	// с прежним пакетом.
	if previous, ok := m.last[info.Track]; ok && dts < previous {
		return ErrUnsupportedTiming
	}
	// sampleFlags кодирует заявленную зависимость единицы доступа; несинхронное видео отмечается по
	// pi.Keyframe.
	var sampleFlags uint32 = 0x02000000
	if info.Track == media.TrackVideo && !pi.Keyframe {
		sampleFlags = 0x01010000
	}
	// trunFlags задаёт присутствующие поля записи trun; композиционное смещение добавляется только
	// для поддерживающих его записей.
	trunFlags := uint32(0x701)
	// fields содержит число единиц доступа, заполняемое позже смещение данных, длительность, размер,
	// флаги и при необходимости композиционное смещение.
	fields := [][]byte{u32(1), u32(0), u32(uint32(duration)), u32(uint32(size)), u32(sampleFlags)}
	if info.Track != media.TrackVideo || info.Video.Codec != core.CodecAV1 {
		trunFlags |= 0x800
		fields = append(fields, u32(uint32(int32(cts.Int64()))))
	}
	// trun содержит сериализованную таблицу одной единицы доступа с ещё не заполненным смещением
	// нагрузки.
	trun := full("trun", 1, trunFlags, fields...)
	// moof содержит заголовок фрагмента, идентификатор дорожки, время декодирования и trun перед
	// заполнением data_offset.
	moof := box("moof", full("mfhd", 0, 0, u32(m.sequence+1)), box("traf", full("tfhd", 0, 0x020000, u32(id)), full("tfdt", 1, 0, u64(uint64(storedDTS))), trun))
	if uint64(len(moof)) > m.limits.MaxMoofBytes {
		return ErrLimit
	}
	// Смещение trun всегда относится к началу единственного moof.
	// offsetPos указывает на поле data_offset внутри moof; нагрузка начинается после moof и
	// восьмибайтового заголовка mdat.
	offsetPos := len(moof) - len(trun) + 16
	be.PutUint32(moof[offsetPos:offsetPos+4], uint32(len(moof)+8))
	// b обозначает очередную часть записи: moof, заголовок mdat либо собственную копию нагрузки
	// пакета.
	for _, b := range [][]byte{moof, join(u32(uint32(size+8)), []byte("mdat")), p.Payload()} {
		// err сообщает об отказе записи очередной части; он сохраняется в Muxer для запрета
		// дальнейших записей.
		if err := writeAll(m.w, b); err != nil {
			m.err = err
			return err
		}
	}
	m.sequence++
	m.last[info.Track] = dts
	return nil
}

// Собирает ftyp/moov с двумя дорожками и собственными JSON-метаданными, предварительно ограничивая
// заголовки и строки; общий и поблочные бюджеты проверяются до возврата.
func marshalInit(init Init, l Limits) ([]byte, error) {
	// v содержит формат и идентичность видео, a — аудио, используемые при построении обеих дорожек и
	// собственных метаданных.
	v, a := init.Video.Info(), init.Audio.Info()
	// c обозначает очередную конфигурацию, чей объём заголовков проверяется до сериализации
	// инициализации.
	for _, c := range []media.CodecConfig{init.Video, init.Audio} {
		if c.StoredBytes() > l.MaxExtradataBytes {
			return nil, ErrLimit
		}
	}
	// Ограничение строк предшествует JSON-копии полного описания.
	// textBytes суммирует длины строк полного формата для предварительной верхней оценки размера их
	// JSON-представления.
	textBytes := len(v.Video.CodecProfile) + len(v.Video.Level) + len(v.Video.Tier) + len(v.Video.PixelFormat) + len(v.Video.Color.Range) + len(v.Video.Color.Primaries) + len(v.Video.Color.Transfer) + len(v.Video.Color.Matrix) + len(a.Audio.CodecProfile) + len(a.Audio.ChannelLayout)
	// initLimit выбирает более строгий предел общей инициализации и отдельного блока для
	// предварительной оценки метаданных.
	initLimit := min(l.MaxInitBytes, l.MaxBoxBytes)
	if uint64(textBytes)*6+4096+2*(init.Video.StoredBytes()+init.Audio.StoredBytes()) > initLimit {
		return nil, ErrLimit
	}
	// meta содержит собственное полное JSON-описание обеих дорожек; err сообщает об отказе его
	// сериализации.
	meta, err := json.Marshal(configMetadata{Version: 1, Video: trackMetadata{v.SourceEpoch, v.Revision, v.TimeBase}, Audio: trackMetadata{a.SourceEpoch, a.Revision, a.TimeBase}, Format: core.MediaFormat{Video: v.Video, Audio: a.Audio}})
	if err != nil {
		return nil, err
	}
	// scale содержит точную общую шкалу фильма, представляющую обе временные шкалы дорожек.
	scale, err := MovieTimeScale(v.TimeBase, a.TimeBase)
	if err != nil {
		return nil, err
	}
	// mvhd содержит обнулённое тело заголовка фильма до заполнения шкалы, скорости, громкости,
	// матрицы и следующего идентификатора дорожки.
	mvhd := make([]byte, 96)
	be.PutUint32(mvhd[8:12], scale)
	be.PutUint32(mvhd[16:20], 0x10000)
	be.PutUint16(mvhd[20:22], 0x100)
	putMatrix(mvhd[32:68])
	be.PutUint32(mvhd[92:96], 3)
	// video содержит сериализованный trak видеодорожки с идентификатором 1.
	video, err := marshalTrack(1, init.Video, init.Origin, l)
	if err != nil {
		return nil, err
	}
	// audio содержит сериализованный trak аудиодорожки с идентификатором 2.
	audio, err := marshalTrack(2, init.Audio, init.Origin, l)
	if err != nil {
		return nil, err
	}
	// data объединяет ftyp и moov с описаниями двух дорожек, trex и собственными метаданными.
	data := join(box("ftyp", []byte("isom"), u32(0x200), []byte("isomiso6mp41")), box("moov", full("mvhd", 0, 0, mvhd), video, audio, box("mvex", full("trex", 0, 0, u32(1), u32(1), make([]byte, 12)), full("trex", 0, 0, u32(2), u32(1), make([]byte, 12))), box("uuid", metadataID[:], meta)))
	if uint64(len(data)) > l.MaxInitBytes {
		return nil, ErrLimit
	}
	// top содержит верхние блоки готовой инициализации для проверки индивидуального бюджета каждого.
	top, err := children(data)
	if err != nil {
		return nil, err
	}
	// b обозначает очередной верхний блок ftyp либо moov, размер которого проверяется перед
	// возвратом инициализации.
	for _, b := range top {
		if b.size > l.MaxBoxBytes {
			return nil, ErrLimit
		}
	}
	return data, nil
}

// Записывает диагональ единичной матрицы преобразования MP4 в заранее обнулённый буфер из 36 байтов.
func putMatrix(b []byte) {
	be.PutUint32(b[0:4], 0x10000)
	be.PutUint32(b[16:20], 0x10000)
	be.PutUint32(b[32:36], 0x40000000)
}

// Собирает описание одной дорожки с точной шкалой и, при ненулевом общем сдвиге, elst для
// восстановления исходного начала.
func marshalTrack(id uint32, c media.CodecConfig, origin OriginMapping, l Limits) ([]byte, error) {
	// info содержит формат, роль и исходную шкалу сериализуемой дорожки.
	info := c.Info()
	// scale содержит целую шкалу дорожки; err сообщает об ошибке её точного построения.
	scale, err := trackScale(info.TimeBase)
	if err != nil {
		return nil, err
	}
	// shift содержит общий сдвиг в отсчётах дорожки для представления его списком редактирования
	// elst.
	shift, err := origin.ticks(scale)
	if err != nil {
		return nil, err
	}
	// tkhd содержит тело заголовка дорожки с идентификатором, матрицей и размерами либо громкостью.
	tkhd := make([]byte, 80)
	be.PutUint32(tkhd[8:12], id)
	putMatrix(tkhd[36:72])
	if info.Track == media.TrackVideo {
		if info.Video.Width > 65535 || info.Video.Height > 65535 {
			return nil, ErrInvalid
		}
		be.PutUint32(tkhd[72:76], info.Video.Width<<16)
		be.PutUint32(tkhd[76:80], info.Video.Height<<16)
	} else {
		be.PutUint16(tkhd[32:34], 0x100)
	}
	// mdhd содержит тело медиазаголовка с целой шкалой дорожки и языковым кодом.
	mdhd := make([]byte, 20)
	be.PutUint32(mdhd[8:12], scale)
	be.PutUint16(mdhd[16:18], 0x55c4)
	// handler задаёт роль hdlr: vide для видео либо soun для аудио.
	handler := "vide"
	// mediaHeader содержит специфичный заголовок vmhd либо smhd для выбранной роли дорожки.
	mediaHeader := full("vmhd", 0, 1, make([]byte, 8))
	if info.Track == media.TrackAudio {
		handler = "soun"
		mediaHeader = full("smhd", 0, 0, make([]byte, 4))
	}
	// entry содержит единственное описание кодека с его исходными заголовками для таблицы stsd.
	entry, err := marshalEntry(c, l)
	if err != nil {
		return nil, err
	}
	// stbl объединяет описание кодека и пустые нефрагментированные таблицы, поскольку единицы
	// доступа будут описаны в moof.
	stbl := box("stbl", full("stsd", 0, 0, u32(1), entry), full("stts", 0, 0, u32(0)), full("stsc", 0, 0, u32(0)), full("stsz", 0, 0, u32(0), u32(0)), full("stco", 0, 0, u32(0)))
	// dinf задаёт ссылку на данные текущего файла без внешнего адреса.
	dinf := box("dinf", full("dref", 0, 0, u32(1), full("url ", 0, 1)))
	// mdia объединяет шкалу, роль обработчика, медиазаголовок, ссылку на данные и таблицы текущей
	// дорожки.
	mdia := box("mdia", full("mdhd", 0, 0, mdhd), full("hdlr", 0, 0, u32(0), []byte(handler), make([]byte, 12), []byte{0}), box("minf", mediaHeader, dinf, stbl))
	// edits содержит необязательный edts/elst, снимающий положительный сдвиг начала при чтении.
	edits := []byte(nil)
	if shift != 0 {
		edits = box("edts", full("elst", 1, 0, u32(1), u64(0), u64(uint64(shift)), u32(0x10000)))
	}
	return box("trak", full("tkhd", 0, 7, tkhd), edits, mdia), nil
}

// Оборачивает данные в MPEG-4-дескриптор с тегом и переменной длиной до четырёх байтов; размер заранее
// ограничивает вызывающий.
func descriptor(tag byte, data []byte) []byte {
	// n содержит ещё не закодированную часть длины дескриптора и сдвигается по семь бит за шаг.
	n := len(data)
	// length хранит до четырёх байтов длины MPEG-4-дескриптора с битами продолжения.
	var length [4]byte
	// pos указывает на первый используемый байт длины внутри четырёхбайтового буфера.
	pos := 3
	length[pos] = byte(n & 0x7f)
	for n >>= 7; n > 0; n >>= 7 {
		pos--
		length[pos] = byte(n&0x7f) | 0x80
	}
	return join([]byte{tag}, length[pos:], data)
}

// Собирает avc1, av01 либо mp4a с исходными заголовками; для H.264 сверяет SPS и размеры, для AAC —
// ASC, частоту и каналы.
func marshalEntry(c media.CodecConfig, l Limits) ([]byte, error) {
	// info содержит формат и роль дорожки для выбора записи avc1, av01 либо mp4a.
	info := c.Info()
	// headers содержит отдельную копию avcC, av1C либо ASC, которая проверяется и включается в
	// запись формата.
	headers := c.Headers()
	if uint64(len(headers)) > l.MaxExtradataBytes {
		return nil, ErrLimit
	}
	if info.Track == media.TrackVideo {
		if info.Video.Codec == core.CodecH264 {
			// err сообщает о некорректном SPS либо несовпадении размеров H.264 с заданным
			// форматом.
			if _, err := spsFromAVCC(TrackDescription{Width: info.Video.Width, Height: info.Video.Height, Headers: headers}); err != nil {
				return nil, err
			}
		}
		// entry содержит фиксированную часть видеозаписи без вложенного блока конфигурации
		// кодека.
		entry := make([]byte, 78)
		be.PutUint16(entry[6:8], 1)
		be.PutUint16(entry[24:26], uint16(info.Video.Width))
		be.PutUint16(entry[26:28], uint16(info.Video.Height))
		be.PutUint32(entry[28:32], 0x480000)
		be.PutUint32(entry[32:36], 0x480000)
		be.PutUint16(entry[40:42], 1)
		be.PutUint16(entry[74:76], 0x18)
		be.PutUint16(entry[76:78], 0xffff)
		// kind выбирает тип видеозаписи avc1 либо av01, config — соответствующий вложенный блок
		// avcC либо av1C.
		kind, config := "avc1", "avcC"
		if info.Video.Codec == core.CodecAV1 {
			kind, config = "av01", "av1C"
		}
		if len(headers) < 4 {
			return nil, ErrInvalid
		}
		return box(kind, entry, box(config, headers)), nil
	}
	if info.Audio.SampleRateHz > 65535 {
		return nil, ErrInvalid
	}
	// actual содержит аудиоформат из ASC для сверки частоты и каналов; err сообщает об отказе
	// восстановления.
	actual, err := audioFromASC(headers)
	if err != nil {
		return nil, err
	}
	if actual.SampleRateHz != info.Audio.SampleRateHz || actual.Channels != info.Audio.Channels {
		return nil, fmt.Errorf("%w: описание audio противоречит ASC", ErrInvalid)
	}
	if len(headers) > 0x0fffffff-32 {
		return nil, ErrLimit
	}
	// entry содержит фиксированную часть mp4a с каналами, разрядностью и частотой аудио.
	entry := make([]byte, 28)
	be.PutUint16(entry[6:8], 1)
	be.PutUint16(entry[16:18], info.Audio.Channels)
	be.PutUint16(entry[18:20], 16)
	be.PutUint32(entry[24:28], info.Audio.SampleRateHz<<16)
	// es содержит иерархию MPEG-4-дескрипторов с фактическим ASC для блока esds.
	es := descriptor(3, join([]byte{0, 2, 0}, descriptor(4, join([]byte{0x40, 0x15, 0, 0, 0}, make([]byte, 8), descriptor(5, headers))), descriptor(6, []byte{2})))
	return box("mp4a", entry, full("esds", 0, 0, es)), nil
}
