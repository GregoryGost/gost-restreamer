package fmp4_test

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

// contractWords сериализует переданные 32-битные слова от старшего байта к младшему в новый буфер.
// Вспомогательные построители этого файла создают контейнеры ISOBMFF независимо от рабочего Muxer;
// структура elst/tfdt/trun сопоставлена с FFmpeg n7.1.5, libavformat/mov.c.
func contractWords(values ...uint32) []byte {
	// b хранит последовательность 32-битных слов в порядке от старшего байта к младшему.
	b := make([]byte, 4*len(values))
	// i задаёт индекс слова, а v — записываемое значение из values.
	for i, v := range values {
		binary.BigEndian.PutUint32(b[4*i:], v)
	}
	return b
}

// Собирает тестовый блок MP4 независимо от рабочего сериализатора, записывая полный 32-битный размер.
func contractBox(kind string, parts ...[]byte) []byte {
	// b накапливает заголовок и дочерние части тестового блока MP4.
	b := make([]byte, 8)
	copy(b[4:], kind)
	// part содержит очередную часть тела собираемого блока.
	for _, part := range parts {
		b = append(b, part...)
	}
	binary.BigEndian.PutUint32(b, uint32(len(b)))
	return b
}

// Добавляет версию и 24-битные флаги к независимому тестовому блоку MP4.
func contractFull(kind string, version byte, flags uint32, parts ...[]byte) []byte {
	return contractBox(kind, append([]byte{version}, contractWords(flags)[1:]...), bytes.Join(parts, nil))
}

// Декодирует фиксированный avcC с H.264 High 128×128/30; неверный эталон завершает тест.
func contractHeaders(t *testing.T) []byte {
	t.Helper()
	// b содержит декодированный эталон avcC; err сообщает об ошибке его шестнадцатеричной записи.
	b, err := hex.DecodeString("0164000bffe100186764000bacd942046c0440000003004000000f03c50a658001000668ebe3cb22c0fdf8f800")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

// Строит независимую дорожку H.264 либо AAC с заданной версией заголовков и переданным elst, без
// собственных метаданных приложения.
func contractTrack(t *testing.T, audio bool, version byte, edits []byte) []byte {
	t.Helper()
	// id задаёт идентификатор дорожки, scale — число тактов в секунду, handler — код обработчика
	// видео либо аудио.
	id, scale, handler := uint32(9), uint32(30000), "vide"
	// entry содержит поля записи описания видео либо аудио перед добавлением блока кодека.
	entry := make([]byte, 78)
	binary.BigEndian.PutUint16(entry[6:], 1)
	binary.BigEndian.PutUint16(entry[24:], 128)
	binary.BigEndian.PutUint16(entry[26:], 128)
	// entryBox содержит запись avc1 либо mp4a вместе с заголовками соответствующего кодека.
	entryBox := contractBox("avc1", entry, contractBox("avcC", contractHeaders(t)))
	// mediaHeader содержит vmhd для видео либо smhd для аудио.
	mediaHeader := contractFull("vmhd", 0, 1, make([]byte, 8))
	if audio {
		id, scale, handler = 2, 48000, "soun"
		entry = make([]byte, 28)
		binary.BigEndian.PutUint16(entry[6:], 1)
		binary.BigEndian.PutUint16(entry[16:], 2)
		binary.BigEndian.PutUint16(entry[18:], 16)
		binary.BigEndian.PutUint32(entry[24:], 48000<<16)
		// ES_Descriptor содержит DecoderConfigDescriptor с DecoderSpecificInfo для AAC-LC, 48 кГц и двух каналов.
		// es содержит вложенные дескрипторы AAC-LC, 48 кГц и двух каналов для esds.
		es := []byte{3, 25, 0, 2, 0, 4, 17, 0x40, 0x15, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 5, 2, 0x11, 0x90, 6, 1, 2}
		entryBox = contractBox("mp4a", entry, contractFull("esds", 0, 0, es))
		mediaHeader = contractFull("smhd", 0, 0, make([]byte, 4))
	}
	// tkhd содержит поля заголовка дорожки, а mdhd — поля заголовка её временной шкалы.
	tkhd, mdhd := make([]byte, 80), make([]byte, 20)
	// idPos задаёт смещение идентификатора в tkhd, scalePos — смещение временной шкалы в mdhd для
	// выбранной версии.
	idPos, scalePos := 8, 8
	if version == 1 {
		tkhd, mdhd = make([]byte, 92), make([]byte, 32)
		idPos, scalePos = 16, 16
	}
	binary.BigEndian.PutUint32(tkhd[idPos:], id)
	binary.BigEndian.PutUint32(mdhd[scalePos:], scale)
	// matrixPos задаёт начало матрицы преобразования в tkhd выбранной версии.
	matrixPos := 36
	if version == 1 {
		matrixPos = 48
	}
	binary.BigEndian.PutUint32(tkhd[matrixPos:], 0x10000)
	binary.BigEndian.PutUint32(tkhd[matrixPos+16:], 0x10000)
	binary.BigEndian.PutUint32(tkhd[matrixPos+32:], 0x40000000)
	if audio {
		binary.BigEndian.PutUint16(tkhd[matrixPos-4:], 0x100)
	} else {
		binary.BigEndian.PutUint32(tkhd[matrixPos+36:], 128<<16)
		binary.BigEndian.PutUint32(tkhd[matrixPos+40:], 128<<16)
	}
	binary.BigEndian.PutUint16(mdhd[len(mdhd)-4:], 0x55c4)
	// dinf описывает ссылку на данные внутри того же файла.
	dinf := contractBox("dinf", contractFull("dref", 0, 0, contractWords(1), contractFull("url ", 0, 1)))
	// stbl объединяет описание кодека и пустые таблицы нефрагментированных пакетов.
	stbl := contractBox("stbl", contractFull("stsd", 0, 0, contractWords(1), entryBox), contractFull("stts", 0, 0, contractWords(0)), contractFull("stsc", 0, 0, contractWords(0)), contractFull("stsz", 0, 0, contractWords(0, 0)), contractFull("stco", 0, 0, contractWords(0)))
	// mdia объединяет временную шкалу, обработчик и таблицы медиадорожки.
	mdia := contractBox("mdia", contractFull("mdhd", version, 0, mdhd), contractFull("hdlr", 0, 0, contractWords(0), []byte(handler), make([]byte, 13)), contractBox("minf", mediaHeader, dinf, stbl))
	return contractBox("trak", contractFull("tkhd", version, 7, tkhd), edits, mdia)
}

// Строит независимый ftyp/moov с видеодорожкой ID=9, аудиодорожкой ID=2 и значениями trex для проверки
// стандартного разбора.
func contractInit(t *testing.T, version byte, edits []byte) []byte {
	t.Helper()
	// mvhd содержит поля заголовка фильма выбранной версии.
	mvhd := make([]byte, 96)
	// pos задаёт смещение временной шкалы в теле mvhd выбранной версии.
	pos := 8
	if version == 1 {
		mvhd, pos = make([]byte, 108), 16
	}
	binary.BigEndian.PutUint32(mvhd[pos:], 1000)
	// ratePos задаёт смещение скорости воспроизведения после полей времени и длительности.
	ratePos := pos + 8
	if version == 1 {
		ratePos += 4
	}
	binary.BigEndian.PutUint32(mvhd[ratePos:], 0x10000)
	binary.BigEndian.PutUint16(mvhd[ratePos+4:], 0x100)
	binary.BigEndian.PutUint32(mvhd[ratePos+16:], 0x10000)
	binary.BigEndian.PutUint32(mvhd[ratePos+32:], 0x10000)
	binary.BigEndian.PutUint32(mvhd[ratePos+48:], 0x40000000)
	binary.BigEndian.PutUint32(mvhd[len(mvhd)-4:], 10)
	return append(contractBox("ftyp", []byte("iso6"), contractWords(0), []byte("iso6isom")), contractBox("moov", contractFull("mvhd", version, 0, mvhd), contractTrack(t, false, version, edits), contractTrack(t, true, version, nil), contractBox("mvex", contractFull("trex", 0, 0, contractWords(9, 1, 1000, 2, 0x10000)), contractFull("trex", 0, 0, contractWords(2, 1, 1024, 2, 0))))...)
}

// Задаёт бюджеты и явные SourceEpoch=47 и ревизии 3/5 для чтения независимых контейнеров.
func contractOptions() fmp4.DemuxOptions {
	return fmp4.DemuxOptions{Limits: acceptanceLimits(), SourceEpoch: 47, VideoRevision: 3, AudioRevision: 5}
}

// Перестройка меняет только первый блок по указанному пути, пересчитывая размеры предков.
func contractRewrite(t *testing.T, data []byte, path []string, change func([]byte) []byte) []byte {
	t.Helper()
	// pos задаёт смещение очередного дочернего блока в перестраиваемом буфере.
	for pos := 0; pos+8 <= len(data); {
		// n содержит полный размер текущего блока для проверки границ и перехода к следующему.
		n := int(binary.BigEndian.Uint32(data[pos:]))
		if n < 8 || n > len(data)-pos {
			t.Fatal("повреждена структура тестового контейнера")
		}
		if string(data[pos+4:pos+8]) == path[0] {
			// body содержит независимую копию тела найденного блока для выбранного
			// преобразования.
			body := bytes.Clone(data[pos+8 : pos+n])
			if len(path) == 1 {
				body = change(body)
			} else {
				body = contractRewrite(t, body, path[1:], change)
			}
			// result накапливает исходный префикс и перестроенный блок перед добавлением
			// неизменённого хвоста.
			result := append([]byte(nil), data[:pos]...)
			result = append(result, contractBox(path[0], body)...)
			return append(result, data[pos+n:]...)
		}
		pos += n
	}
	t.Fatalf("нет блока %v в тестовом контейнере", path)
	return nil
}

// Сериализует тестовый elst заданной версии из длительности, media_time и скорости каждой записи.
func contractEdit(version byte, entries ...[3]int64) []byte {
	// b накапливает число монтажных записей и их сериализованные поля.
	b := contractWords(uint32(len(entries)))
	// entry содержит длительность, media_time и скорость очередной монтажной записи.
	for _, entry := range entries {
		if version == 1 {
			// part хранит 64-битные длительность и media_time записи версии 1.
			part := make([]byte, 16)
			binary.BigEndian.PutUint64(part, uint64(entry[0]))
			binary.BigEndian.PutUint64(part[8:], uint64(entry[1]))
			b = append(b, part...)
		} else {
			b = append(b, contractWords(uint32(entry[0]), uint32(entry[1]))...)
		}
		b = append(b, contractWords(uint32(entry[2]))...)
	}
	return contractBox("edts", contractFull("elst", version, 0, b))
}

// Строит независимый moof видеодорожки с выбираемыми tfhd и trun; при отсутствии trun создаёт одну
// запись с data_offset.
func contractFragment(decode, dataOffset uint32, tfhdFlags uint32, tfhdFields []byte, truns ...[]byte) []byte {
	// parts содержит tfhd, tfdt и добавляемые trun тестового фрагмента.
	parts := [][]byte{contractFull("tfhd", 0, tfhdFlags, contractWords(9), tfhdFields), contractFull("tfdt", 0, 0, contractWords(decode))}
	parts = append(parts, truns...)
	if len(truns) == 0 {
		parts = append(parts, contractFull("trun", 0, 1, contractWords(1, dataOffset)))
	}
	return contractBox("moof", contractFull("mfhd", 0, 0, contractWords(1)), contractBox("traf", parts...))
}

// Создаёт независимый фрагмент с двумя байтами нагрузки и точным смещением mdat относительно moof.
func contractOneFragment(decode uint32) []byte {
	// m содержит moof, сначала для определения его размера, затем с точным смещением нагрузки.
	m := contractFragment(decode, 0, 0x20000, nil)
	m = contractFragment(decode, uint32(len(m)+8), 0x20000, nil)
	return append(m, contractBox("mdat", []byte{0x61, 0x62})...)
}

// Проверяет версии заголовков 0/1, сортировку Descriptions по ID, изоляцию возвращённых заголовков и
// точное чтение независимого пакета.
func TestContractIndependentInitDescriptionsAreSortedAndIsolated(t *testing.T) {
	// version выбирает вариант заголовков MP4 0 либо 1.
	for _, version := range []byte{0, 1} {
		t.Run(string(rune('0'+version)), func(t *testing.T) {
			// Для выбранной версии заголовков проверяет сортировку и независимость описаний дорожек, а также
			// точные метки и байты первого пакета.

			// wire содержит независимую инициализацию и один фрагмент для проверки описаний
			// и пакета.
			wire := append(contractInit(t, version, nil), contractOneFragment(3000)...)
			// d читает тестовый контейнер; err сообщает об отказе его инициализации.
			d, err := fmp4.NewDemuxer(bytes.NewReader(wire), contractOptions())
			if err != nil {
				t.Fatal(err)
			}
			// desc содержит первую копию описаний дорожек, которую тест намеренно изменяет.
			desc := d.Descriptions()
			if len(desc) != 2 || desc[0].ID != 2 || desc[1].ID != 9 || desc[0].Track != media.TrackAudio || desc[1].Width != 128 || desc[1].Height != 128 || desc[1].TimeBase != (media.TimeBase{Numerator: 1, Denominator: 30000}) || !bytes.Equal(desc[1].Headers, contractHeaders(t)) {
				t.Fatalf("неверные сведения: %+v", desc)
			}
			desc[0].Headers[0], desc[1].Headers[0], desc[1].ID = 0, 0, 0
			// again содержит повторно полученные описания для проверки изоляции от изменений
			// desc.
			again := d.Descriptions()
			if again[1].ID != 9 || again[0].Headers[0] != 0x11 || again[1].Headers[0] != 1 {
				t.Fatal("Descriptions передаёт внутренние данные во владение вызывающего")
			}
			// p содержит первый пакет для проверки меток, нагрузки и конфигурации.
			p, err := d.Next()
			if err != nil || p.Info() != (media.PacketInfo{DTS: 3000, PTS: 3000, DurationTicks: 1000}) || !bytes.Equal(p.Payload(), []byte("ab")) || p.Config().Info().SourceEpoch != 47 || p.Config().Headers()[0] != 1 {
				t.Fatalf("изменились пакет или конфигурация: %+v, %v", p.Info(), err)
			}
		})
	}
}

// Проверяет отказ для нулевых шкал, неверных версий, отсутствующих или повторных заголовков,
// неоднозначных дорожек и неверных trex.
func TestContractInitRejectsAmbiguousAndMalformedTracks(t *testing.T) {
	// set32 создаёт преобразования тестовых блоков с записью 32-битного значения по смещению.
	set32 := func(offset int, value uint32) func([]byte) []byte {
		// Создаёт преобразование тела блока, записывающее заданное 32-битное значение по указанному смещению.

		return func(b []byte) []byte {
			// Записывает захваченное 32-битное значение по заданному смещению в порядке от старшего байта к
			// младшему.
			binary.BigEndian.PutUint32(b[offset:], value)
			return b
		}
	}
	// cases перечисляет повреждения инициализации и ожидаемые категории отказа.
	cases := []struct {
		// name задаёт имя сценария повреждения инициализации.
		name string
		// path задаёт путь дочерних блоков до изменяемого тела.
		path []string
		// change преобразует тело выбранного блока для создания повреждения.
		change func([]byte) []byte
		// want задаёт ожидаемую категорию ошибки инициализации.
		want error
	}{
		{"movie-timescale-zero", []string{"moov", "mvhd"}, set32(12, 0), fmp4.ErrInvalid},
		{"movie-version", []string{"moov", "mvhd"}, func(b []byte) []byte {
			// Задаёт неподдержанную версию mvhd.
			b[0] = 2
			return b
		}, fmp4.ErrInvalid},
		{"track-zero-id", []string{"moov", "trak", "tkhd"}, set32(12, 0), fmp4.ErrInvalid},
		{"track-duplicate-id", []string{"moov", "trak", "tkhd"}, set32(12, 2), fmp4.ErrInvalid},
		{"track-version", []string{"moov", "trak", "tkhd"}, func(b []byte) []byte {
			// Задаёт неподдержанную версию tkhd.
			b[0] = 2
			return b
		}, fmp4.ErrInvalid},
		{"media-timescale-zero", []string{"moov", "trak", "mdia", "mdhd"}, set32(12, 0), fmp4.ErrInvalid},
		{"media-version", []string{"moov", "trak", "mdia", "mdhd"}, func(b []byte) []byte {
			// Задаёт неподдержанную версию mdhd.
			b[0] = 2
			return b
		}, fmp4.ErrInvalid},
		{"handler-contradiction", []string{"moov", "trak", "mdia", "hdlr"}, func(b []byte) []byte {
			// Меняет обработчик видеодорожки на soun, создавая противоречие её описанию.
			copy(b[8:12], "soun")
			return b
		}, fmp4.ErrInvalid},
		{"missing-movie-header", []string{"moov"}, func(b []byte) []byte {
			// Переименовывает mvhd в free, оставляя контейнер без заголовка фильма.
			copy(b[4:8], "free")
			return b
		}, fmp4.ErrInvalid},
		{"duplicate-movie-header", []string{"moov"}, func(b []byte) []byte {
			// Добавляет вторую копию первого дочернего блока moov, создавая повтор mvhd.
			// n содержит размер первого дочернего блока для добавления второй копии mvhd.
			n := binary.BigEndian.Uint32(b)
			return append(b, b[:n]...)
		}, fmp4.ErrInvalid},
		{"missing-track-header", []string{"moov", "trak"}, func(b []byte) []byte {
			// Переименовывает tkhd в free, оставляя дорожку без её заголовка.
			copy(b[4:8], "free")
			return b
		}, fmp4.ErrInvalid},
		{"duplicate-track-header", []string{"moov", "trak"}, func(b []byte) []byte {
			// Добавляет вторую копию первого дочернего блока trak, создавая повтор tkhd.
			// n содержит размер первого дочернего блока для добавления второй копии tkhd.
			n := binary.BigEndian.Uint32(b)
			return append(b, b[:n]...)
		}, fmp4.ErrInvalid},
		{"missing-audio", []string{"moov"}, func(b []byte) []byte {
			// Оставляет в moov только mvhd и первую дорожку, удаляя аудиодорожку и последующие блоки.

			return b[:int(binary.BigEndian.Uint32(b))+int(binary.BigEndian.Uint32(b[binary.BigEndian.Uint32(b):]))]
		}, fmp4.ErrInvalid},
		{"third-track", []string{"moov"}, func(b []byte) []byte {
			// Добавляет в moov третью дорожку с описанием аудио.
			return append(b, contractTrack(t, true, 0, nil)...)
		}, fmp4.ErrLimit},
		{"unknown-default-track", []string{"moov", "mvex", "trex"}, set32(4, 99), fmp4.ErrInvalid},
		{"unknown-description-index", []string{"moov", "mvex", "trex"}, set32(8, 2), fmp4.ErrInvalid},
		{"truncated-defaults", []string{"moov", "mvex", "trex"}, func(b []byte) []byte {
			// Обрезает trex до 20 байтов, удаляя часть обязательных значений по умолчанию.
			return b[:20]
		}, fmp4.ErrInvalid},
		{"trailing-partial-child", []string{"moov"}, func(b []byte) []byte {
			// Добавляет одиночный байт после дочерних блоков moov, создавая незавершённый заголовок.
			return append(b, 0)
		}, fmp4.ErrInvalid},
	}
	// tc содержит очередное повреждение и ожидаемую ошибку из cases.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Применяет выбранное повреждение инициализации и проверяет ожидаемую категорию отказа
			// демультиплексора.

			// wire содержит инициализацию с применённым повреждением выбранного блока.
			wire := contractRewrite(t, contractInit(t, 0, nil), tc.path, tc.change)
			// err содержит результат попытки открыть повреждённую инициализацию.
			if _, err := fmp4.NewDemuxer(bytes.NewReader(wire), contractOptions()); !errors.Is(err, tc.want) {
				t.Fatalf("ожидалась %v, получена %v", tc.want, err)
			}
		})
	}
}

// contractReadCounter оборачивает источник и учитывает объём и число чтений для проверок раннего отказа.
type contractReadCounter struct {
	// r задаёт источник байтов, чтения которого учитывает обёртка.
	r io.Reader
	// bytes накапливает число фактически прочитанных байтов, calls — число вызовов Read.
	bytes, calls int
}

// Считает вызовы Reader и фактически прочитанные байты для проверки отказа до тела и отсутствия
// повторного ввода-вывода.
func (r *contractReadCounter) Read(p []byte) (int, error) {
	r.calls++
	// n содержит число байтов текущего чтения; err сохраняет ошибку исходного Reader.
	n, err := r.r.Read(p)
	r.bytes += n
	return n, err
}

// Проверяет бюджеты moof/free/mdat на заголовке, общий бюджет инициализации и сохранение первой ошибки
// без дальнейшего чтения.
func TestContractFramingBudgetsRejectBeforeBodyAndRemainSticky(t *testing.T) {
	// kind выбирает тип блока, чей бюджет проверяется до чтения тела.
	for _, kind := range []string{"moof", "free", "mdat"} {
		t.Run(kind, func(t *testing.T) {
			// Проверяет отказ по размеру выбранного блока до чтения тела и сохранение той же ошибки без
			// последующих чтений.

			// o содержит ограничения демультиплексора для текущего типа блока.
			o := contractOptions()
			// wire накапливает корректную инициализацию и заголовок блока с превышенным
			// размером.
			wire := contractInit(t, 0, nil)
			if kind == "mdat" {
				// m содержит moof с описанием пакета перед заголовком чрезмерного mdat.
				m := contractFragment(0, 0, 0x20000, nil)
				m = contractFragment(0, uint32(len(m)+8), 0x20000, nil)
				wire = append(wire, m...)
			}
			// limit содержит бюджет размера выбранного moof, free либо mdat.
			limit := o.Limits.MaxMoofBytes
			if kind == "free" {
				limit = o.Limits.MaxBoxBytes
			}
			if kind == "mdat" {
				limit = o.Limits.MaxMdatBytes
			}
			wire = append(wire, contractWords(uint32(limit+1))...)
			wire = append(wire, kind...)
			// r считает чтения контейнера для проверки отказа на заголовке.
			r := &contractReadCounter{r: bytes.NewReader(wire)}
			// d читает контейнер с чрезмерным блоком; err сообщает об отказе начальной
			// инициализации.
			d, err := fmp4.NewDemuxer(r, o)
			if err != nil {
				t.Fatal(err)
			}
			// first сохраняет первую ошибку Next для проверки категории и повторного
			// возврата того же значения.
			_, first := d.Next()
			if !errors.Is(first, fmp4.ErrLimit) || r.bytes != len(wire) {
				t.Fatalf("бюджет не проверен на заголовке: %v, прочитано %d/%d", first, r.bytes, len(wire))
			}
			// calls фиксирует число чтений после первой ошибки для проверки отсутствия
			// дальнейшего ввода-вывода.
			calls := r.calls
			if _, err = d.Next(); err != first || r.calls != calls {
				t.Fatalf("повторное чтение после ошибки: %v, calls=%d/%d", err, r.calls, calls)
			}
		})
	}
	// extended выбирает обычный либо расширенный заголовок moov.
	for _, extended := range []bool{false, true} {
		// o задаёт ограничения с уменьшенным общим бюджетом инициализации.
		o := contractOptions()
		// prefix содержит корректный ftyp, уже занимающий часть общего бюджета инициализации.
		prefix := contractBox("ftyp", []byte("iso6"), contractWords(0))
		o.Limits.MaxInitBytes = 4096
		// header содержит обычный либо расширенный заголовок moov с объявленным размером 4096
		// байтов.
		header := append(contractWords(4096), []byte("moov")...)
		if extended {
			header = append(contractWords(1), []byte("moov")...)
			header = append(header, []byte{0, 0, 0, 0, 0, 0, 16, 0}...)
		}
		// wire объединяет ftyp и заголовок moov без тела для проверки отказа до его чтения.
		wire := append(prefix, header...)
		// r учитывает байты чтения при проверке общего бюджета инициализации.
		r := &contractReadCounter{r: bytes.NewReader(wire)}
		// err содержит результат открытия контейнера с превышенным общим бюджетом.
		if _, err := fmp4.NewDemuxer(r, o); !errors.Is(err, fmp4.ErrLimit) || r.bytes != len(wire) {
			t.Fatalf("общий init-бюджет не проверен до тела: %v", err)
		}
	}
}

// Проверяет точный общий сдвиг elst по шкалам фильма и дорожки в версиях 0/1 и отказ для округления,
// неверной скорости и неподдержанных последовательностей записей.
func TestContractEditOriginsUseMovieAndTrackTimeScalesExactly(t *testing.T) {
	// version выбирает версию заголовков и монтажного списка для проверки точных сдвигов.
	for _, version := range []byte{0, 1} {
		// tc содержит монтажные записи и ожидаемые метки очередного допустимого сценария.
		for _, tc := range []struct {
			// name задаёт имя допустимого сценария монтажных правок.
			name string
			// entries содержит монтажные записи в виде длительности, media_time и скорости.
			entries [][3]int64
			// want задаёт ожидаемые PTS и DTS в тактах видеодорожки.
			want int64
		}{
			{"media-origin", [][3]int64{{0, 9000, 65536}}, -6000},
			// Пауза 250/1000 s и media_time=1500/30000 s дают начало 0.3 s.
			{"positive-gap", [][3]int64{{250, -1, 65536}, {0, 1500, 65536}}, 9000},
		} {
			t.Run(tc.name+string(rune('0'+version)), func(t *testing.T) {
				// Проверяет точные PTS и DTS после выбранного списка монтажных правок для версии 0 либо 1.

				// wire содержит контейнер с выбранным монтажным списком и пакетом с
				// исходной меткой 3000.
				wire := append(contractInit(t, version, contractEdit(version, tc.entries...)), contractOneFragment(3000)...)
				// d читает контейнер с монтажными правками; err сообщает об отказе его
				// инициализации.
				d, err := fmp4.NewDemuxer(bytes.NewReader(wire), contractOptions())
				if err != nil {
					t.Fatal(err)
				}
				// p содержит пакет после применения сдвига монтажного списка.
				p, err := d.Next()
				if err != nil || p.Info().PTS != tc.want || p.Info().DTS != tc.want || p.Info().DurationTicks != 1000 {
					t.Fatalf("ожидалось DTS=PTS=%d, получено %+v, %v", tc.want, p.Info(), err)
				}
			})
		}
	}
	// tc содержит неподдержанный монтажный список и ожидаемую категорию ошибки.
	for _, tc := range []struct {
		// name задаёт имя сценария отказа монтажного списка.
		name string
		// version задаёт версию elst, включая неподдержанное значение.
		version byte
		// entries содержит последовательность монтажных записей, вызывающую проверяемый отказ.
		entries [][3]int64
		// want задаёт ожидаемую категорию ошибки разбора монтажного списка.
		want error
	}{
		{"unsupported-version", 2, [][3]int64{{0, 0, 65536}}, fmp4.ErrInvalid},
		{"zero-edits", 0, nil, fmp4.ErrUnsupportedTiming},
		{"non-unit-rate", 0, [][3]int64{{0, 0, 32768}}, fmp4.ErrUnsupportedTiming},
		{"gap-after-media", 0, [][3]int64{{0, 0, 65536}, {1, -1, 65536}}, fmp4.ErrUnsupportedTiming},
		{"negative-media-time", 0, [][3]int64{{0, -2, 65536}}, fmp4.ErrUnsupportedTiming},
		{"two-media-origins", 0, [][3]int64{{0, 0, 65536}, {0, 1, 65536}}, fmp4.ErrUnsupportedTiming},
		{"gap-only", 0, [][3]int64{{10, -1, 65536}}, fmp4.ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет отказ с выбранной категорией для неподдержанного либо некорректного списка монтажных
			// правок.

			// err содержит результат открытия контейнера с неподдержанными монтажными
			// правками.
			_, err := fmp4.NewDemuxer(bytes.NewReader(contractInit(t, 0, contractEdit(tc.version, tc.entries...))), contractOptions())
			if !errors.Is(err, tc.want) {
				t.Fatalf("ожидалась %v, получена %v", tc.want, err)
			}
		})
	}
	// wire содержит монтажный список, пауза которого после смены шкалы фильма не выражается целым
	// числом тактов дорожки.
	wire := contractInit(t, 0, contractEdit(0, [3]int64{1, -1, 65536}, [3]int64{0, 0, 65536}))
	wire = contractRewrite(t, wire, []string{"moov", "mvhd"}, func(b []byte) []byte {
		// Задаёт шкалу фильма 1001, при которой пауза списка правок не выражается целым числом тактов дорожки.
		binary.BigEndian.PutUint32(b[12:], 1001)
		return b
	})
	// err содержит результат проверки нецелого преобразования длительности монтажной паузы.
	if _, err := fmp4.NewDemuxer(bytes.NewReader(wire), contractOptions()); !errors.Is(err, fmp4.ErrUnsupportedTiming) {
		t.Fatalf("нецелая пауза была округлена: %v", err)
	}
}

// Проверяет переопределение trex полями tfhd, продолжение нескольких trun и пропуск промежутков
// нагрузки и служебных блоков.
func TestContractFragmentDefaultsRunsAndPayloadGaps(t *testing.T) {
	// tfhd переопределяет trex: 40 тактов, 2 байта; первый пакет имеет Keyframe=true, второй — Keyframe=false.
	// build создаёт moof с двумя trun и выбранным смещением первой нагрузки.
	build := func(offset uint32) []byte {
		// Создаёт два trun с заданным смещением данных первого пакета и переопределёнными в tfhd
		// длительностью, размером и флагами.

		return contractFragment(3000, 0, 0x2003a, contractWords(1, 40, 2, 0x10000), contractFull("trun", 0, 5, contractWords(1, offset, 0)), contractFull("trun", 0, 0, contractWords(1)))
	}
	// m содержит moof до и после уточнения смещения с учётом трёх байтов промежутка.
	m := build(0)
	m = build(uint32(len(m) + 8 + 3))
	// wire накапливает контейнер со служебными блоками, промежутками нагрузки и двумя фрагментами.
	wire := contractInit(t, 0, nil)
	wire = append(wire, contractBox("styp", []byte("iso6"), contractWords(0), []byte("iso6"))...)
	wire = append(wire, contractBox("free", []byte{0, 0, 0, 0})...)
	wire = append(wire, contractFull("sidx", 0, 0, contractWords(9, 30000, 0, 0, 0))...)
	wire = append(wire, m...)
	wire = append(wire, contractBox("mdat", []byte{9, 9, 9, 'a', 'b', 'c', 'd', 8, 8})...)
	wire = append(wire, contractOneFragment(4000)...)
	wire = append(wire, contractBox("mfra", contractFull("mfro", 0, 0, contractWords(24)))...)
	// d читает контейнер с несколькими trun; err сообщает об отказе начальной инициализации.
	d, err := fmp4.NewDemuxer(bytes.NewReader(wire), contractOptions())
	if err != nil {
		t.Fatal(err)
	}
	// i задаёт номер ожидаемого пакета, want — его точные метки, признаки и байты нагрузки.
	for i, want := range []struct {
		// info задаёт ожидаемые метки, длительность и объявленный признак ключевого пакета.
		info media.PacketInfo
		// payload задаёт ожидаемые байты нагрузки текущего пакета.
		payload string
	}{{media.PacketInfo{DTS: 3000, PTS: 3000, DurationTicks: 40, Keyframe: true}, "ab"}, {media.PacketInfo{DTS: 3040, PTS: 3040, DurationTicks: 40}, "cd"}, {media.PacketInfo{DTS: 4000, PTS: 4000, DurationTicks: 1000}, "ab"}} {
		// p содержит очередной прочитанный пакет; err сообщает об отказе чтения.
		p, err := d.Next()
		if err != nil || p.Info() != want.info || string(p.Payload()) != want.payload {
			t.Fatalf("пакет %d: %+v, %q, %v", i, p.Info(), p.Payload(), err)
		}
	}
	// err содержит результат чтения после всех ожидаемых пакетов для проверки EOF.
	if _, err := d.Next(); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
}

// Проверяет отказ некорректного tfhd/tfdt/trun до чтения нагрузки и сохранение той же ошибки при
// повторном Next.
func TestContractMalformedFragmentFailsWithoutReadingPayload(t *testing.T) {
	// tc содержит очередное повреждение структуры фрагмента.
	for _, tc := range []struct {
		// name задаёт имя сценария повреждения фрагмента.
		name string
		// path задаёт путь к изменяемому tfhd, tfdt либо trun.
		path []string
		// change преобразует тело блока в некорректное представление.
		change func([]byte) []byte
	}{
		{"missing-base-offset", []string{"moof", "traf", "tfhd"}, func(b []byte) []byte {
			// Удаляет флаги tfhd, оставляя фрагмент без поддержанной базы смещений.
			b[1], b[2], b[3] = 0, 0, 0
			return b
		}},
		{"unknown-tfhd-flags", []string{"moof", "traf", "tfhd"}, func(b []byte) []byte {
			// Добавляет неизвестный флаг tfhd.
			b[3] |= 0x40
			return b
		}},
		{"unknown-track", []string{"moof", "traf", "tfhd"}, func(b []byte) []byte {
			// Задаёт в tfhd идентификатор отсутствующей дорожки 99.
			binary.BigEndian.PutUint32(b[4:], 99)
			return b
		}},
		{"tfdt-version", []string{"moof", "traf", "tfdt"}, func(b []byte) []byte {
			// Задаёт неподдержанную версию tfdt.
			b[0] = 2
			return b
		}},
		{"tfdt-length", []string{"moof", "traf", "tfdt"}, func(b []byte) []byte {
			// Добавляет лишний байт к tfdt.
			return append(b, 0)
		}},
		{"trun-version", []string{"moof", "traf", "trun"}, func(b []byte) []byte {
			// Задаёт неподдержанную версию trun.
			b[0] = 2
			return b
		}},
		{"unknown-trun-flags", []string{"moof", "traf", "trun"}, func(b []byte) []byte {
			// Добавляет неизвестный флаг trun.
			b[3] |= 0x10
			return b
		}},
		{"missing-run-offset", []string{"moof", "traf", "trun"}, func(b []byte) []byte {
			// Удаляет флаг и поле смещения данных первого trun.
			b[3] = 0
			return b[:8]
		}},
		{"trailing-run-fields", []string{"moof", "traf", "trun"}, func(b []byte) []byte {
			// Добавляет четыре лишних байта после полей trun.
			return append(b, 0, 0, 0, 0)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет отказ повреждённого фрагмента до чтения полезной нагрузки и неизменность ошибки при
			// повторном Next.

			// fragment содержит один фрагмент с применённым повреждением служебного блока.
			fragment := contractRewrite(t, contractOneFragment(0), tc.path, tc.change)
			// r учитывает чтения для проверки отказа до полезной нагрузки.
			r := &contractReadCounter{r: bytes.NewReader(append(contractInit(t, 0, nil), fragment...))}
			// d читает контейнер с повреждённым фрагментом; err сообщает об отказе начальной
			// инициализации.
			d, err := fmp4.NewDemuxer(r, contractOptions())
			if err != nil {
				t.Fatal(err)
			}
			// first сохраняет первоначальную ошибку разбора фрагмента.
			_, first := d.Next()
			if !errors.Is(first, fmp4.ErrInvalid) {
				t.Fatalf("ожидалась ErrInvalid, получена %v", first)
			}
			if r.bytes > len(contractInit(t, 0, nil))+len(fragment)-2 {
				t.Fatal("payload прочитан до проверки структуры фрагмента")
			}
			// calls фиксирует число чтений после отказа разбора фрагмента.
			calls := r.calls
			// err содержит повторный результат Next для проверки сохранения ошибки без новых
			// чтений.
			if _, err := d.Next(); err != first || r.calls != calls {
				t.Fatalf("ошибка demux не сохранена: %v", err)
			}
		})
	}
}

// Проверяет выдачу аудио перед видео по физическим смещениям mdat, хотя описания traf расположены в
// обратном порядке.
func TestContractInterleavedTrackPayloadUsesOffsetsInsteadOfDeclarationOrder(t *testing.T) {
	// build создаёт moof, где описания видео и аудио расположены в порядке, обратном размещению
	// нагрузки.
	build := func(offset uint32) []byte {
		// Размещает описание видео перед аудио, задавая смещения данных в обратном порядке для проверки чтения
		// по смещениям.

		// video содержит traf видео со смещением на два байта после начала аудионагрузки.
		video := contractBox("traf", contractFull("tfhd", 0, 0x20000, contractWords(9)), contractFull("tfdt", 0, 0, contractWords(3000)), contractFull("trun", 0, 1, contractWords(1, offset+2)))
		// audio содержит traf аудио со смещением на начало mdat.
		audio := contractBox("traf", contractFull("tfhd", 0, 0x20000, contractWords(2)), contractFull("tfdt", 0, 0, contractWords(4800)), contractFull("trun", 0, 1, contractWords(1, offset)))
		return contractBox("moof", contractFull("mfhd", 0, 0, contractWords(1)), video, audio)
	}
	// m содержит moof до и после вычисления точного начала нагрузки.
	m := build(0)
	m = build(uint32(len(m) + 8))
	// wire объединяет инициализацию, фрагмент и нагрузку с аудио перед видео.
	wire := append(contractInit(t, 0, nil), m...)
	wire = append(wire, contractBox("mdat", []byte("CDab"))...)
	// d читает контейнер с перемежением дорожек; err сообщает об отказе начальной инициализации.
	d, err := fmp4.NewDemuxer(bytes.NewReader(wire), contractOptions())
	if err != nil {
		t.Fatal(err)
	}
	// i задаёт номер пакета в физическом порядке, want — ожидаемые дорожку, метки и нагрузку.
	for i, want := range []struct {
		// track задаёт ожидаемую дорожку пакета в порядке физических смещений.
		track media.Track
		// info задаёт ожидаемые метки и длительность пакета своей дорожки.
		info media.PacketInfo
		// payload задаёт ожидаемые байты пакета в mdat.
		payload string
	}{
		{media.TrackAudio, media.PacketInfo{DTS: 4800, PTS: 4800, DurationTicks: 1024}, "CD"},
		{media.TrackVideo, media.PacketInfo{DTS: 3000, PTS: 3000, DurationTicks: 1000}, "ab"},
	} {
		// p содержит пакет по следующему физическому смещению; err сообщает об отказе чтения.
		p, err := d.Next()
		if err != nil || p.Config().Info().Track != want.track || p.Info() != want.info || string(p.Payload()) != want.payload {
			t.Fatalf("пакет %d прочитан с чужого смещения: %+v, %q, %v", i, p.Info(), p.Payload(), err)
		}
	}
}

// Проверяет абсолютную базу tfhd и отрицательное data_offset=-10 при чтении пакета из правильного
// места mdat.
func TestContractAbsoluteBaseAndSignedRunOffset(t *testing.T) {
	// init содержит инициализацию, длина которой входит в абсолютное смещение данных.
	init := contractInit(t, 0, nil)
	// build создаёт фрагмент с абсолютной базой tfhd и отрицательным смещением trun.
	build := func(base uint32) []byte {
		// Создаёт фрагмент с заданной абсолютной базой и отрицательным смещением trun на десять байтов.

		return contractFragment(3000, 0, 0x3b, contractWords(0, base, 1, 40, 2, 0), contractFull("trun", 0, 1, contractWords(1, 0xfffffff6)))
	}
	// m содержит фрагмент до и после вычисления абсолютной базы с поправкой на data_offset=-10.
	m := build(0)
	// data_offset=-10 возвращает абсолютную базу к началу payload.
	m = build(uint32(len(init) + len(m) + 8 + 10))
	// wire объединяет инициализацию, фрагмент с абсолютной базой и двухбайтовую нагрузку.
	wire := append(init, m...)
	wire = append(wire, contractBox("mdat", []byte("ab"))...)
	// d читает контейнер с абсолютной базой; err сообщает об отказе начальной инициализации.
	d, err := fmp4.NewDemuxer(bytes.NewReader(wire), contractOptions())
	if err != nil {
		t.Fatal(err)
	}
	// p содержит пакет для проверки знакового смещения, меток и байтов нагрузки.
	p, err := d.Next()
	if err != nil || p.Info() != (media.PacketInfo{DTS: 3000, PTS: 3000, DurationTicks: 40, Keyframe: true}) || string(p.Payload()) != "ab" {
		t.Fatalf("абсолютная база или signed offset искажены: %+v, %q, %v", p.Info(), p.Payload(), err)
	}
}

// Проверяет отказ нулевых, коротких и некорректных расширенных заголовков, неверного порядка
// инициализации и превышения бюджета заголовков кодека.
func TestContractTopLevelFramingRejectsInvalidHeaders(t *testing.T) {
	// tc содержит некорректное верхнеуровневое обрамление и ожидаемую категорию отказа.
	for _, tc := range []struct {
		// name задаёт имя сценария повреждения верхнеуровневых блоков.
		name string
		// wire содержит байты некорректного заголовка либо порядка инициализации.
		wire []byte
		// want задаёт ожидаемую категорию ошибки чтения заголовков.
		want error
	}{
		{"zero-size", append(contractWords(0), []byte("ftyp")...), fmp4.ErrInvalid},
		{"size-below-header", append(contractWords(7), []byte("ftyp")...), fmp4.ErrInvalid},
		{"short-extended-size", append(contractWords(1), []byte("ftyp\x00")...), io.ErrUnexpectedEOF},
		{"extended-below-header", append(append(contractWords(1), []byte("ftyp")...), contractWords(0, 15)...), fmp4.ErrInvalid},
		{"movie-before-file-type", contractInit(t, 0, nil)[24:], fmp4.ErrInvalid},
		{"duplicate-file-type", append(contractBox("ftyp", []byte("iso6"), contractWords(0)), contractInit(t, 0, nil)...), fmp4.ErrInvalid},
		{"unexpected-init-box", contractBox("mdat"), fmp4.ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет ожидаемую ошибку для выбранного некорректного заголовка либо порядка начальных блоков.

			// err содержит результат открытия контейнера с выбранным дефектом обрамления.
			if _, err := fmp4.NewDemuxer(bytes.NewReader(tc.wire), contractOptions()); !errors.Is(err, tc.want) {
				t.Fatalf("ожидалась %v, получена %v", tc.want, err)
			}
		})
	}
	// options задаёт уменьшенный бюджет заголовков кодека для отдельной проверки ограничения
	// extradata.
	options := contractOptions()
	options.Limits.MaxExtradataBytes = 4
	// err содержит результат открытия корректной инициализации при слишком малом бюджете extradata.
	if _, err := fmp4.NewDemuxer(bytes.NewReader(contractInit(t, 0, nil)), options); !errors.Is(err, fmp4.ErrLimit) {
		t.Fatalf("ограничение extradata проигнорировано: %v", err)
	}
}

// Проверяет отказ без публикации частичной нагрузки и отсутствие новых чтений после UnexpectedEOF.
func TestContractDemuxDisconnectDoesNotResumeReads(t *testing.T) {
	// wire содержит корректный контейнер перед удалением последнего байта нагрузки.
	wire := append(contractInit(t, 0, nil), contractOneFragment(3000)...)
	// r учитывает чтения источника с оборванной нагрузкой последнего пакета.
	r := &contractReadCounter{r: bytes.NewReader(wire[:len(wire)-1])}
	// d читает оборванный контейнер; err сообщает об отказе начальной инициализации.
	d, err := fmp4.NewDemuxer(r, contractOptions())
	if err != nil {
		t.Fatal(err)
	}
	// p содержит результат чтения неполной нагрузки, который должен остаться невалидным; first
	// сохраняет исходную ошибку чтения.
	p, first := d.Next()
	if !errors.Is(first, io.ErrUnexpectedEOF) || p.Validate() == nil {
		t.Fatalf("частичный payload опубликован: %+v, %v", p.Info(), first)
	}
	// calls фиксирует число чтений после обрыва нагрузки.
	calls := r.calls
	// err содержит повторный результат Next для проверки сохранения ошибки обрыва без новых чтений.
	if _, err := d.Next(); err != first || r.calls != calls {
		t.Fatalf("чтение возобновилось после разрыва: %v, calls=%d/%d", err, r.calls, calls)
	}
}
