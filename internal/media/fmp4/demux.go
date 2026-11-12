package fmp4

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"sort"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
)

// trackState связывает описание и конфигурацию дорожки с её сдвигом, значениями trex и последним
// разобранным временем декодирования.
type trackState struct {
	// description хранит идентификатор, шкалу и параметры дорожки, извлечённые из заголовков MP4.
	description TrackDescription
	// config содержит восстановленную полную конфигурацию, к которой относятся выдаваемые пакеты
	// дорожки.
	config media.CodecConfig
	// offset хранит знаковый сдвиг elst в отсчётах контейнерной шкалы этой дорожки.
	offset int64
	// duration задаёт длительность единицы доступа по умолчанию из trex; size задаёт её размер в
	// байтах, flags — флаги зависимости.
	duration, size, flags uint32
	// lastDTS хранит последний DTS, восстановленный из описаний единиц доступа дорожки в отсчётах
	// полной конфигурации, для запрета уменьшения времени.
	lastDTS int64
	// hasDTS отличает первую единицу доступа от дорожки с уже установленным lastDTS, включая нулевое
	// значение.
	hasDTS bool
}

// sample описывает ещё не прочитанную единицу доступа: её дорожку, диапазон в mdat и восстановленные
// временные метки.
type sample struct {
	// track связывает единицу доступа с полной конфигурацией её дорожки.
	track *trackState
	// offset содержит абсолютное смещение первого байта сжатой нагрузки от начала транспорта.
	offset uint64
	// size задаёт число байтов сжатой нагрузки без заголовков MP4.
	size uint32
	// info хранит восстановленные PTS, DTS и длительность в шкале конфигурации; признак кадра
	// берётся из флагов контейнера.
	info media.PacketInfo
}

// Demuxer читает только текущий ограниченный init/moof и одну единицу доступа.
type Demuxer struct {
	// r предоставляет входные байты; отменой чтения и закрытием Reader управляет вызывающий код.
	r io.Reader
	// options сохраняет пределы разбора, запрошенную идентичность конфигураций и необязательный
	// восстановитель формата.
	options DemuxOptions
	// init хранит восстановленную пару конфигураций аудио и видео, используемых при выдаче пакетов.
	init Init
	// tracks связывает идентификаторы MP4 с состоянием каждой из двух принятых дорожек.
	tracks map[uint32]*trackState
	// position учитывает завершённые чтения и пропуски в байтах от начала транспорта.
	position uint64
	// samples содержит оставшиеся описания единиц доступа текущего moof, отсортированные по
	// положению данных перед чтением.
	samples []sample
	// mdatEnd хранит абсолютное смещение первого байта после текущего mdat; ноль означает отсутствие
	// текущего блока данных.
	mdatEnd uint64
	// err сохраняет первую ошибку Next, включая EOF, для возврата того же результата последующим
	// вызовам.
	err error
}

// NewDemuxer читает ограниченную инициализацию от ftyp до moov и восстанавливает обе конфигурации;
// Reader остаётся во владении вызывающего.
func NewDemuxer(r io.Reader, options DemuxOptions) (*Demuxer, error) {
	if r == nil {
		return nil, ErrInvalid
	}
	// err сообщает о недопустимости заданных бюджетов чтения контейнера.
	if err := options.Limits.Validate(); err != nil {
		return nil, err
	}
	// d хранит создаваемый последовательный разборщик до завершения инициализации обеих дорожек.
	d := &Demuxer{r: r, options: options, tracks: make(map[uint32]*trackState)}
	// total накапливает объявленные размеры блоков инициализации для проверки общего бюджета до
	// чтения каждого тела.
	var total uint64
	// seenFtyp отмечает уже прочитанный ftyp, чтобы отклонить повтор и потребовать его наличие перед
	// moov.
	seenFtyp := false
	for {
		// b содержит заголовок очередного блока инициализации; err сообщает об отказе его чтения
		// или проверки размера.
		b, err := d.header()
		if err != nil {
			return nil, err
		}
		if b.size > options.Limits.MaxInitBytes-total {
			return nil, ErrLimit
		}
		total += b.size
		if b.kind != "ftyp" && b.kind != "moov" && b.kind != "free" && b.kind != "skip" {
			return nil, fmt.Errorf("%w: неожиданный init box %s", ErrInvalid, b.kind)
		}
		b, err = d.body(b, options.Limits.MaxBoxBytes)
		if err != nil {
			return nil, err
		}
		if b.kind == "ftyp" {
			if seenFtyp {
				return nil, ErrInvalid
			}
			seenFtyp = true
		}
		if b.kind == "moov" {
			if !seenFtyp {
				return nil, ErrInvalid
			}
			if err = d.parseInit(b.data); err != nil {
				return nil, err
			}
			return d, nil
		}
	}
}

// Init возвращает восстановленную пару неизменяемых конфигураций по значению.
func (d *Demuxer) Init() Init { return d.init }

// Descriptions возвращает отдельные копии сведений, извлечённых из init.
func (d *Demuxer) Descriptions() []TrackDescription {
	// result накапливает отдельные описания дорожек, которые будут выданы в порядке их
	// идентификаторов.
	result := make([]TrackDescription, 0, len(d.tracks))
	// t обозначает очередную восстановленную дорожку, чьё описание копируется для вызывающего.
	for _, t := range d.tracks {
		// desc содержит копию описания с отдельно клонируемыми заголовками, чтобы результат не
		// изменял внутреннюю дорожку.
		desc := t.description
		desc.Headers = bytes.Clone(desc.Headers)
		result = append(result, desc)
	}
	sort.Slice(result, func(i, j int) bool {
		// Сравнивает идентификаторы описаний для выдачи дорожек по возрастанию ID.
		return result[i].ID < result[j].ID
	})
	return result
}

// Читает заголовок блока и продвигает абсолютную позицию на его длину с проверкой переполнения.
func (d *Demuxer) header() (rawBox, error) {
	// b содержит прочитанный заголовок блока; err сообщает о его усечении либо недопустимом размере.
	b, err := readHeader(d.r)
	if err != nil {
		return b, err
	}
	if d.position > math.MaxUint64-b.header {
		return b, ErrLimit
	}
	d.position += b.header
	return b, nil
}

// Читает ограниченное тело блока, продвигая абсолютную позицию только после полного успешного чтения.
func (d *Demuxer) body(b rawBox, limit uint64) (rawBox, error) {
	if d.position > math.MaxUint64-(b.size-b.header) {
		return rawBox{}, ErrLimit
	}
	// err сообщает об отказе полного чтения тела; только успех позволяет продвинуть учётную позицию.
	b, err := readBody(d.r, b, limit)
	if err == nil {
		d.position += b.size - b.header
	}
	return b, err
}

// Пропускает ровно n байтов без сохранения данных и при успехе обновляет абсолютную позицию;
// непредставимая длина возвращает ErrLimit.
func (d *Demuxer) skip(n uint64) error {
	if n > math.MaxInt64 {
		return ErrLimit
	}
	if d.position > math.MaxUint64-n {
		return ErrLimit
	}
	// err сообщает о неполном пропуске или отказе Reader; учётная позиция продвигается только при
	// успехе.
	_, err := io.CopyN(io.Discard, d.r, int64(n))
	if err == nil {
		d.position += n
	}
	return err
}

// Next возвращает очередной пакет и сохраняет первую ошибку, включая EOF; последующие вызовы
// возвращают ту же ошибку.
func (d *Demuxer) Next() (media.Packet, error) {
	if d.err != nil {
		return media.Packet{}, d.err
	}
	// p содержит очередной восстановленный пакет; err сохраняется разборщиком для всех последующих
	// Next.
	p, err := d.next()
	if err != nil {
		d.err = err
	}
	return p, err
}

// Читает очередной ограниченный moof и проверяет все диапазоны единиц доступа внутри следующего mdat
// до чтения их нагрузки; данные выдаёт по физическим смещениям.
func (d *Demuxer) next() (media.Packet, error) {
	for len(d.samples) == 0 {
		if d.mdatEnd > d.position {
			// err сообщает об отказе пропуска непрочитанного хвоста предыдущего mdat.
			if err := d.skip(d.mdatEnd - d.position); err != nil {
				return media.Packet{}, err
			}
		}
		d.mdatEnd = 0
		// start фиксирует абсолютное начало очередного блока для разрешения смещений данных
		// относительно moof.
		start := d.position
		// b содержит очередной заголовок после предыдущего mdat; err сообщает об отказе его
		// чтения.
		b, err := d.header()
		if err != nil {
			return media.Packet{}, err
		}
		if b.kind == "mfra" || b.kind == "sidx" || b.kind == "styp" || b.kind == "free" {
			if b.size > d.options.Limits.MaxBoxBytes {
				return media.Packet{}, ErrLimit
			}
			if err = d.skip(b.size - b.header); err != nil {
				return media.Packet{}, err
			}
			continue
		}
		if b.kind != "moof" {
			return media.Packet{}, fmt.Errorf("%w: ожидался moof, получен %s", ErrInvalid, b.kind)
		}
		b, err = d.body(b, d.options.Limits.MaxMoofBytes)
		if err != nil {
			return media.Packet{}, err
		}
		if err = d.parseMoof(b.data, start); err != nil {
			return media.Packet{}, err
		}
		// mdat содержит заголовок блока данных, который должен непосредственно следовать
		// разобранному moof.
		mdat, err := d.header()
		if err != nil {
			return media.Packet{}, err
		}
		if mdat.kind != "mdat" {
			return media.Packet{}, ErrInvalid
		}
		if mdat.size > d.options.Limits.MaxMdatBytes || mdat.size > d.options.Limits.MaxBoxBytes {
			return media.Packet{}, ErrLimit
		}
		if d.position > math.MaxUint64-(mdat.size-mdat.header) {
			return media.Packet{}, ErrLimit
		}
		d.mdatEnd = d.position + mdat.size - mdat.header
		sort.Slice(d.samples, func(i, j int) bool {
			// Сравнивает абсолютные смещения единиц доступа, чтобы читать mdat в порядке расположения данных.
			return d.samples[i].offset < d.samples[j].offset
		})
		// end хранит правую границу уже проверенного диапазона нагрузки для обнаружения
		// перекрытий единиц доступа.
		end := d.position
		// s обозначает очередное описание единицы доступа, чей диапазон должен целиком
		// помещаться в mdat без перекрытий.
		for _, s := range d.samples {
			if s.offset < end || s.offset > d.mdatEnd || uint64(s.size) > d.mdatEnd-s.offset {
				return media.Packet{}, fmt.Errorf("%w: sample за пределами mdat", ErrInvalid)
			}
			end = s.offset + uint64(s.size)
		}
	}
	// s содержит следующее по физическому смещению описание единицы доступа, удаляемое из очереди
	// перед чтением.
	s := d.samples[0]
	d.samples = d.samples[1:]
	if s.offset > d.position {
		// err сообщает об отказе пропуска промежутка до нагрузки выбранной единицы доступа.
		if err := d.skip(s.offset - d.position); err != nil {
			return media.Packet{}, err
		}
	}
	// payload принимает ровно объявленное число байтов сжатой единицы доступа.
	payload := make([]byte, int(s.size))
	// err сообщает об усечении либо отказе чтения нагрузки выбранной единицы доступа.
	if _, err := io.ReadFull(d.r, payload); err != nil {
		return media.Packet{}, err
	}
	d.position += uint64(s.size)
	return media.NewPacket(s.info, s.track.config, payload)
}

// Разбирает обе дорожки и значения trex, восстанавливая конфигурации из собственных метаданных,
// ResolveConfig либо заголовков. Явные идентичности DemuxOptions применяются до итоговой проверки
// согласованности A/V.
func (d *Demuxer) parseInit(data []byte) error {
	// boxes содержит дочерние блоки moov; err сообщает о нарушении их обрамления.
	boxes, err := children(data)
	if err != nil {
		return err
	}
	// mvhd содержит общий заголовок фильма, из которого извлекается шкала времени для elst.
	mvhd, err := find(data, "mvhd")
	if err != nil {
		return err
	}
	// movieScale хранит ненулевое число отсчётов в секунду из поддержанной версии mvhd.
	var movieScale uint32
	if len(mvhd.data) < 20 {
		return ErrInvalid
	}
	switch mvhd.data[0] {
	case 0:
		movieScale = be.Uint32(mvhd.data[12:16])
	case 1:
		if len(mvhd.data) < 32 {
			return ErrInvalid
		}
		movieScale = be.Uint32(mvhd.data[20:24])
	default:
		return ErrInvalid
	}
	if movieScale == 0 {
		return ErrInvalid
	}
	// metadata хранит единственный собственный JSON-блок полного формата; nil оставляет
	// восстановление заголовкам или ResolveConfig.
	var metadata *configMetadata
	// b обозначает очередной дочерний блок moov, проверяемый как описание дорожки либо собственные
	// метаданные.
	for _, b := range boxes {
		switch b.kind {
		case "trak":
			if len(d.tracks) >= 2 {
				return ErrLimit
			}
			// t содержит разобранное состояние дорожки; err сообщает о неверных заголовках,
			// времени или превышении бюджета.
			t, err := d.parseTrack(b.data, movieScale)
			if err != nil {
				return err
			}
			// ok отмечает, что такой идентификатор дорожки уже занят и повтор должен быть
			// отклонён.
			if _, ok := d.tracks[t.description.ID]; ok {
				return ErrInvalid
			}
			d.tracks[t.description.ID] = t
		case "uuid":
			if len(b.data) >= 16 && bytes.Equal(b.data[:16], metadataID[:]) {
				if metadata != nil {
					return ErrInvalid
				}
				metadata = &configMetadata{}
				// decoder читает JSON собственных метаданных после UUID, запрещая
				// неизвестные поля и лишнее значение.
				decoder := json.NewDecoder(bytes.NewReader(b.data[16:]))
				decoder.DisallowUnknownFields()
				// err сообщает о недопустимом JSON собственных метаданных контейнера.
				if err := decoder.Decode(metadata); err != nil {
					return fmt.Errorf("%w: metadata: %v", ErrInvalid, err)
				}
				if metadata.Version != 1 {
					return ErrInvalid
				}
				// err отличает корректный конец JSON от лишнего значения или
				// синтаксического отказа после метаданных.
				if err := decoder.Decode(new(any)); err != io.EOF {
					return ErrInvalid
				}
			}
		}
	}
	if len(d.tracks) != 2 {
		return fmt.Errorf("%w: нужны обе дорожки", ErrInvalid)
	}
	// mvex содержит расширения фрагментированного фильма; err определяет, удалось ли найти этот
	// необязательный блок.
	if mvex, err := find(data, "mvex"); err == nil {
		// defaults содержит дочерние блоки расширений; err сообщает об ошибке их структуры до
		// чтения trex.
		defaults, err := children(mvex.data)
		if err != nil {
			return err
		}
		// b обозначает очередной дочерний блок mvex, из которого могут быть получены значения
		// trex.
		for _, b := range defaults {
			if b.kind == "trex" {
				if len(b.data) != 24 {
					return ErrInvalid
				}
				// t содержит дорожку с идентификатором из trex; nil означает ссылку на
				// неизвестную дорожку.
				t := d.tracks[be.Uint32(b.data[4:8])]
				if t == nil {
					return ErrInvalid
				}
				if be.Uint32(b.data[8:12]) != 1 {
					return ErrInvalid
				}
				t.duration = be.Uint32(b.data[12:16])
				t.size = be.Uint32(b.data[16:20])
				t.flags = be.Uint32(b.data[20:24])
			}
		}
	}
	// t обозначает очередную дорожку, для которой восстанавливается полная конфигурация кодека.
	for _, t := range d.tracks {
		// c накапливает восстановленную конфигурацию текущей дорожки до её проверки и установки
		// в Init.
		var c media.CodecConfig
		if metadata != nil {
			// info содержит формат и идентичность дорожки из собственных метаданных до
			// применения ненулевых переопределений.
			info := metadata.info(t.description.Track)
			applyIdentity(&info, d.options)
			c, err = media.NewCodecConfig(info, t.description.Headers)
		} else if d.options.ResolveConfig != nil {
			// desc содержит описание для пользовательского восстановителя с отдельной копией
			// фактических заголовков.
			desc := t.description
			desc.Headers = bytes.Clone(desc.Headers)
			c, err = d.options.ResolveConfig(desc)
			if err == nil {
				if err = validateWireConfig(c, t.description); err != nil {
					return err
				}
				// info содержит результат пользовательского восстановителя до применения
				// запрошенной эпохи и ревизии.
				info := c.Info()
				applyIdentity(&info, d.options)
				c, err = media.NewCodecConfig(info, t.description.Headers)
			}
		} else {
			// revision выбирает переданную ревизию именно текущей видео- или аудиодорожки
			// для штатного восстановителя.
			revision := d.options.VideoRevision
			if t.description.Track == media.TrackAudio {
				revision = d.options.AudioRevision
			}
			c, err = ResolveCodecConfig(t.description, d.options.SourceEpoch, revision)
		}
		if err != nil {
			return err
		}
		if err = validateWireConfig(c, t.description); err != nil {
			return err
		}
		t.config = c
		if t.description.Track == media.TrackVideo {
			if d.init.Video.Validate() == nil {
				return ErrInvalid
			}
			d.init.Video = c
		} else {
			if d.init.Audio.Validate() == nil {
				return ErrInvalid
			}
			d.init.Audio = c
		}
	}
	if d.init.Video.Info().SourceEpoch != d.init.Audio.Info().SourceEpoch {
		return ErrInvalid
	}
	return nil
}

// Заменяет SourceEpoch и ревизию выбранной дорожки только ненулевыми значениями DemuxOptions.
func applyIdentity(info *media.CodecConfigInfo, o DemuxOptions) {
	if o.SourceEpoch != 0 {
		info.SourceEpoch = o.SourceEpoch
	}
	if info.Track == media.TrackVideo && o.VideoRevision != 0 {
		info.Revision = o.VideoRevision
	}
	if info.Track == media.TrackAudio && o.AudioRevision != 0 {
		info.Revision = o.AudioRevision
	}
}

// Извлекает идентификатор, шкалу, формат и заголовки одной поддержанной дорожки; сверяет hdlr и
// преобразует поддержанный elst в знаковое смещение.
func (d *Demuxer) parseTrack(data []byte, movieScale uint32) (*trackState, error) {
	// tkhd содержит заголовок дорожки; err сообщает об отсутствии, повторе либо нарушенном
	// обрамлении блока.
	tkhd, err := find(data, "tkhd")
	if err != nil {
		return nil, err
	}
	if len(tkhd.data) < 24 {
		return nil, ErrInvalid
	}
	// idPos задаёт положение идентификатора дорожки с учётом версии tkhd.
	idPos := 12
	if tkhd.data[0] == 1 {
		idPos = 20
	} else if tkhd.data[0] != 0 {
		return nil, ErrInvalid
	}
	if len(tkhd.data) < idPos+4 {
		return nil, ErrInvalid
	}
	// id содержит ненулевой идентификатор дорожки, связывающий trak, trex и traf.
	id := be.Uint32(tkhd.data[idPos : idPos+4])
	if id == 0 {
		return nil, ErrInvalid
	}
	// mdia содержит блок медиаданных описания дорожки для извлечения шкалы и формата.
	mdia, err := find(data, "mdia")
	if err != nil {
		return nil, err
	}
	// mdhd содержит заголовок временной шкалы дорожки.
	mdhd, err := find(mdia.data, "mdhd")
	if err != nil {
		return nil, err
	}
	if len(mdhd.data) < 20 {
		return nil, ErrInvalid
	}
	// scalePos выбирает положение timescale по версии mdhd.
	scalePos := 12
	if mdhd.data[0] == 1 {
		scalePos = 20
	} else if mdhd.data[0] != 0 {
		return nil, ErrInvalid
	}
	if len(mdhd.data) < scalePos+4 {
		return nil, ErrInvalid
	}
	// scale содержит ненулевое число отсчётов в секунду временной шкалы дорожки.
	scale := be.Uint32(mdhd.data[scalePos : scalePos+4])
	if scale == 0 {
		return nil, ErrInvalid
	}
	// minf содержит описание медиадорожки, в котором ищется таблица сжатых данных.
	minf, err := find(mdia.data, "minf")
	if err != nil {
		return nil, err
	}
	// stbl содержит таблицы дорожки, включая единственное поддерживаемое описание формата.
	stbl, err := find(minf.data, "stbl")
	if err != nil {
		return nil, err
	}
	// stsd содержит таблицу описаний формата, в которой разрешена ровно одна запись.
	stsd, err := find(stbl.data, "stsd")
	if err != nil {
		return nil, err
	}
	if len(stsd.data) < 8 || be.Uint32(stsd.data[4:8]) != 1 {
		return nil, ErrInvalid
	}
	// entries содержит разобранные записи stsd; допустима только одна запись поддерживаемого кодека.
	entries, err := children(stsd.data[8:])
	if err != nil || len(entries) != 1 {
		return nil, ErrInvalid
	}
	// entry содержит единственную запись формата для разбора размеров, аудиопараметров и заголовков
	// кодека.
	entry := entries[0]
	// t накапливает описание и смещение создаваемой дорожки до проверки всей структуры trak.
	t := &trackState{description: TrackDescription{ID: id, TimeBase: media.TimeBase{Numerator: 1, Denominator: scale}}}
	// desc указывает на описание внутри создаваемого состояния дорожки для заполнения полей из MP4.
	desc := &t.description
	// extra принимает найденный блок avcC, av1C либо esds с заголовками кодека.
	var extra rawBox
	switch entry.kind {
	case "avc1", "av01":
		if len(entry.data) < 78 {
			return nil, ErrInvalid
		}
		desc.Track = media.TrackVideo
		desc.Codec = core.CodecH264
		// kind выбирает обязательный блок конфигурации видео: avcC для avc1 либо av1C для av01.
		kind := "avcC"
		if entry.kind == "av01" {
			desc.Codec = core.CodecAV1
			kind = "av1C"
		}
		desc.Width = uint32(be.Uint16(entry.data[24:26]))
		desc.Height = uint32(be.Uint16(entry.data[26:28]))
		extra, err = find(entry.data[78:], kind)
		if err != nil {
			return nil, err
		}
		if uint64(len(extra.data)) > d.options.Limits.MaxExtradataBytes {
			return nil, ErrLimit
		}
		desc.Headers = bytes.Clone(extra.data)
	case "mp4a":
		if len(entry.data) < 28 || be.Uint16(entry.data[8:10]) != 0 {
			return nil, ErrInvalid
		}
		desc.Track = media.TrackAudio
		desc.Codec = core.CodecAAC
		desc.Channels = be.Uint16(entry.data[16:18])
		desc.SampleRateHz = be.Uint32(entry.data[24:28]) >> 16
		extra, err = find(entry.data[28:], "esds")
		if err != nil {
			return nil, err
		}
		if len(extra.data) < 4 {
			return nil, ErrInvalid
		}
		desc.Headers, err = parseASC(extra.data[4:], d.options.Limits.MaxExtradataBytes)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("%w: codec %s", ErrInvalid, entry.kind)
	}
	if len(desc.Headers) == 0 {
		return nil, ErrInvalid
	}
	// hdlr содержит объявленную роль обработчика дорожки для сверки с фактической записью формата.
	hdlr, err := find(mdia.data, "hdlr")
	if err != nil {
		return nil, err
	}
	if len(hdlr.data) < 12 {
		return nil, ErrInvalid
	}
	// expectedHandler задаёт требуемый hdlr: vide для видео либо soun для аудио.
	expectedHandler := "vide"
	if desc.Track == media.TrackAudio {
		expectedHandler = "soun"
	}
	if string(hdlr.data[8:12]) != expectedHandler {
		return nil, fmt.Errorf("%w: hdlr противоречит sample entry", ErrInvalid)
	}
	// edts содержит необязательные правки временной шкалы; при наличии из него извлекается elst.
	edts, err := find(data, "edts")
	if err == nil {
		// elst содержит список правок дорожки; err сообщает о его отсутствии, повторе либо
		// нарушенной структуре.
		elst, err := find(edts.data, "elst")
		if err != nil {
			return nil, err
		}
		t.offset, err = parseEdit(elst.data, movieScale, scale)
		if err != nil {
			return nil, err
		}
	}
	return t, nil
}

// Ищет и копирует ограниченный ASC в поддержанной вложенности MPEG-4-дескрипторов; неверная длина или
// структура возвращает ошибку, отсутствие — ErrMetadataUnavailable.
func parseASC(data []byte, limit uint64) ([]byte, error) {
	// walk хранит рекурсивный обход поддержанных MPEG-4-дескрипторов с ограничением глубины и
	// отдельной копией найденного ASC.
	var walk func([]byte, int) ([]byte, error)
	walk = func(b []byte, depth int) ([]byte, error) {
		// Рекурсивно ищет DecoderSpecificInfo в ограниченно вложенных MPEG-4-дескрипторах, проверяя длины и
		// возвращая отдельную копию ASC в пределах limit.

		if depth > 3 {
			return nil, ErrInvalid
		}
		for len(b) > 0 {
			// tag содержит тип очередного MPEG-4-дескриптора, определяющий поиск вложенных
			// данных либо ASC.
			tag := b[0]
			b = b[1:]
			// n накапливает объявленную длину тела дескриптора из последовательности
			// семибитных групп.
			n := uint32(0)
			// ended отмечает завершающий байт длины, чтобы отклонить незаконченный
			// четырёхбайтовый префикс.
			ended := false
			for range 4 {
				if len(b) == 0 {
					return nil, ErrInvalid
				}
				// v содержит очередной байт длины дескриптора с битом продолжения и
				// семью битами значения.
				v := b[0]
				b = b[1:]
				n = n<<7 | uint32(v&0x7f)
				if v&0x80 == 0 {
					ended = true
					break
				}
			}
			if !ended || uint64(n) > uint64(len(b)) {
				return nil, ErrInvalid
			}
			// body ссылается на объявленное тело очередного дескриптора после проверки его
			// границ.
			body := b[:n]
			b = b[n:]
			if tag == 5 {
				if uint64(n) > limit {
					return nil, ErrLimit
				}
				return bytes.Clone(body), nil
			}
			// skip задаёт длину служебного префикса ES_Descriptor либо
			// DecoderConfigDescriptor перед вложенными дескрипторами.
			skip := 0
			switch tag {
			case 3:
				if len(body) < 3 || body[2] != 0 {
					return nil, ErrInvalid
				}
				skip = 3
			case 4:
				if len(body) < 13 || body[0] != 0x40 {
					return nil, ErrInvalid
				}
				skip = 13
			default:
				continue
			}
			// result содержит копию найденного ASC; err отличает отсутствие конфигурации от
			// неверной структуры вложенных данных.
			result, err := walk(body[skip:], depth+1)
			if err == nil {
				return result, nil
			}
			if err != ErrMetadataUnavailable {
				return nil, err
			}
		}
		return nil, ErrMetadataUnavailable
	}
	return walk(data, 0)
}

// Преобразует elst версии 0 или 1 с максимум двумя записями в точный сдвиг дорожки; разрешены
// начальный пустой интервал и единственная медиазапись с единичной скоростью.
func parseEdit(data []byte, movieScale, scale uint32) (int64, error) {
	if len(data) < 8 {
		return 0, ErrInvalid
	}
	// version выбирает 32- либо 64-битное представление полей поддержанной версии elst.
	version := data[0]
	if version > 1 {
		return 0, ErrInvalid
	}
	// count задаёт число правок; поддержаны одна медиазапись и необязательный начальный пустой
	// интервал.
	count := be.Uint32(data[4:8])
	if count == 0 || count > 2 {
		return 0, ErrUnsupportedTiming
	}
	// c последовательно читает записи elst, сохраняя ошибку усечения их полей.
	c := cursor{b: data[8:]}
	// offset накапливает точный знаковый сдвиг дорожки в отсчётах её контейнерной шкалы.
	offset := int64(0)
	// seenMedia отмечает уже прочитанную медиазапись для запрета второй и проверки её обязательного
	// наличия.
	seenMedia := false
	for range count {
		// duration содержит длительность очередной правки в шкале фильма; для пустого интервала
		// она добавляется к сдвигу.
		var duration uint64
		// mediaTime задаёт начало медиа в шкале дорожки; значение -1 обозначает пустой интервал.
		var mediaTime int64
		if version == 1 {
			duration = c.u64()
			mediaTime = int64(c.u64())
		} else {
			duration = uint64(c.u32())
			mediaTime = int64(int32(c.u32()))
		}
		// rate содержит закодированную скорость правки; поддерживается только значение 0x10000.
		rate := c.u32()
		if c.err != nil || rate != 0x10000 {
			return 0, ErrUnsupportedTiming
		}
		if mediaTime == -1 {
			if seenMedia || duration > math.MaxInt64 {
				return 0, ErrUnsupportedTiming
			}
			// gap содержит точную длительность пустого интервала в шкале дорожки; err
			// сообщает о непредставимом пересчёте.
			gap, err := rescale(int64(duration), media.TimeBase{Numerator: 1, Denominator: movieScale}, media.TimeBase{Numerator: 1, Denominator: scale})
			if err != nil {
				return 0, err
			}
			offset, err = add(offset, gap)
			if err != nil {
				return 0, err
			}
		} else {
			if seenMedia || mediaTime < 0 {
				return 0, ErrUnsupportedTiming
			}
			seenMedia = true
			// err сообщает о выходе итогового сдвига за int64 при вычитании начала медиа.
			var err error
			offset, err = add(offset, -mediaTime)
			if err != nil {
				return 0, err
			}
		}
	}
	if !seenMedia || len(c.b) != 0 {
		return 0, ErrInvalid
	}
	return offset, nil
}

// Собирает описания единиц доступа всех traf текущего moof; пустой результат отклоняет.
func (d *Demuxer) parseMoof(data []byte, start uint64) error {
	// boxes содержит дочерние блоки текущего moof; err сообщает о нарушении их обрамления.
	boxes, err := children(data)
	if err != nil {
		return err
	}
	d.samples = nil
	// b обозначает очередной дочерний блок moof, из которого выбираются описания дорожек traf.
	for _, b := range boxes {
		if b.kind != "traf" {
			continue
		}
		if err = d.parseTraf(b.data, start); err != nil {
			return err
		}
	}
	if len(d.samples) == 0 {
		return ErrInvalid
	}
	return nil
}

// Разбирает tfhd/tfdt/trun с явным базовым смещением данных, ограничивает число и размеры единиц
// доступа и точно восстанавливает знаковые PTS/DTS. Уменьшение DTS дорожки и наличие композиционного
// смещения у AV1 отклоняются.
func (d *Demuxer) parseTraf(data []byte, start uint64) error {
	// tfhd содержит заголовок текущего фрагмента дорожки; err сообщает об отсутствии, повторе либо
	// неверной структуре.
	tfhd, err := find(data, "tfhd")
	if err != nil {
		return err
	}
	// f хранит флаги разбираемого полного блока: сначала tfhd, затем каждого trun.
	f, err := flags(tfhd.data)
	if err != nil {
		return err
	}
	if f&^uint32(0x03003b) != 0 {
		return ErrInvalid
	}
	// c последовательно читает числовые поля tfhd и trun с сохранением ошибки границ.
	c := cursor{b: tfhd.data[4:]}
	// id содержит идентификатор дорожки из tfhd для связи с инициализацией.
	id := c.u32()
	// t содержит ранее инициализированную дорожку; nil означает неизвестный идентификатор traf.
	t := d.tracks[id]
	if t == nil {
		return ErrInvalid
	}
	// base задаёт абсолютную базу смещений данных из tfhd либо начало текущего moof при явном
	// соответствующем флаге.
	base := start
	if f&1 != 0 {
		base = c.u64()
	} else if f&0x020000 == 0 {
		return fmt.Errorf("%w: неявный base offset", ErrInvalid)
	}
	if f&2 != 0 && c.u32() != 1 {
		return ErrInvalid
	}
	// duration, size и sampleFlags задают длительность, размер нагрузки и флаги единицы доступа по
	// умолчанию; tfhd может заменить значения trex.
	duration, size, sampleFlags := t.duration, t.size, t.flags
	if f&8 != 0 {
		duration = c.u32()
	}
	if f&16 != 0 {
		size = c.u32()
	}
	if f&32 != 0 {
		sampleFlags = c.u32()
	}
	if c.err != nil || len(c.b) != 0 {
		return ErrInvalid
	}
	// tfdt содержит базовое время декодирования текущего фрагмента дорожки.
	tfdt, err := find(data, "tfdt")
	if err != nil {
		return err
	}
	if len(tfdt.data) < 8 {
		return ErrInvalid
	}
	// decode хранит неотрицательное базовое время из tfdt и продвигается на длительность каждой
	// единицы доступа.
	var decode uint64
	switch tfdt.data[0] {
	case 0:
		if len(tfdt.data) != 8 {
			return ErrInvalid
		}
		decode = uint64(be.Uint32(tfdt.data[4:]))
	case 1:
		if len(tfdt.data) != 12 {
			return ErrInvalid
		}
		decode = be.Uint64(tfdt.data[4:])
	default:
		return ErrInvalid
	}
	// boxes содержит дочерние блоки traf для последовательного разбора всех trun.
	boxes, err := children(data)
	if err != nil {
		return err
	}
	// nextOffset хранит абсолютное смещение следующей нагрузки и продолжается между trun одного
	// traf.
	var nextOffset uint64
	// haveOffset отмечает, что явный data_offset уже задал допустимое начало данных текущего traf.
	haveOffset := false
	// b обозначает очередной блок traf, из которого выбирается таблица trun.
	for _, b := range boxes {
		if b.kind != "trun" {
			continue
		}
		f, err = flags(b.data)
		if err != nil {
			return err
		}
		if f&^uint32(0xf05) != 0 || b.data[0] > 1 || f&4 != 0 && f&0x400 != 0 {
			return ErrInvalid
		}
		if t.description.Codec == core.CodecAV1 && f&0x800 != 0 {
			return fmt.Errorf("%w: composition offset в AV1SampleEntry", ErrUnsupportedTiming)
		}
		c = cursor{b: b.data[4:]}
		// count задаёт число единиц доступа trun, проверяемое вместе с уже накопленными
		// описаниями moof.
		count := c.u32()
		if count == 0 || uint64(count)+uint64(len(d.samples)) > uint64(d.options.Limits.MaxSamples) {
			return ErrLimit
		}
		if f&1 != 0 {
			// delta содержит знаковое смещение данных trun относительно выбранной базы.
			delta := int64(int32(c.u32()))
			if delta < 0 {
				// magnitude хранит модуль отрицательного data_offset для проверки
				// вычитания из беззнаковой базы.
				magnitude := uint64(-delta)
				if magnitude > base {
					return ErrInvalid
				}
				nextOffset = base - magnitude
			} else {
				if base > math.MaxUint64-uint64(delta) {
					return ErrInvalid
				}
				nextOffset = base + uint64(delta)
			}
			haveOffset = true
		}
		if !haveOffset {
			return ErrInvalid
		}
		// firstFlags задаёт флаги первой единицы доступа trun, если для неё объявлено отдельное
		// переопределение.
		firstFlags := sampleFlags
		if f&4 != 0 {
			firstFlags = c.u32()
		}
		// entryWords считает число четырёхбайтовых полей одной записи trun для точной проверки
		// длины таблицы.
		entryWords := 0
		// bit выбирает флаг присутствия одного из полей длительности, размера, флагов либо
		// композиционного смещения.
		for _, bit := range []uint32{0x100, 0x200, 0x400, 0x800} {
			if f&bit != 0 {
				entryWords++
			}
		}
		if c.err != nil || uint64(count)*uint64(entryWords)*4 != uint64(len(c.b)) {
			return ErrInvalid
		}
		// index задаёт позицию текущей единицы доступа внутри trun для применения
		// first_sample_flags только к первой.
		for index := range count {
			// dur хранит длительность текущей единицы доступа, n — размер её нагрузки в
			// байтах, sf — флаги зависимости; явные поля trun заменяют значения по
			// умолчанию.
			dur, n, sf := duration, size, sampleFlags
			if index == 0 {
				sf = firstFlags
			}
			if f&0x100 != 0 {
				dur = c.u32()
			}
			if f&0x200 != 0 {
				n = c.u32()
			}
			if f&0x400 != 0 {
				sf = c.u32()
			}
			// cts хранит композиционное смещение PTS относительно DTS в контейнерной шкале
			// дорожки.
			cts := int64(0)
			if f&0x800 != 0 {
				// value содержит сырые 32 бита композиционного смещения, которые версия
				// trun определяет как знаковое либо беззнаковое значение.
				value := c.u32()
				cts = int64(value)
				if b.data[0] == 1 {
					cts = int64(int32(value))
				}
			}
			if n == 0 || uint64(n) > d.options.Limits.MaxSampleBytes {
				return ErrLimit
			}
			if nextOffset > math.MaxUint64-uint64(n) || decode > math.MaxUint64-uint64(dur) {
				return ErrUnsupportedTiming
			}
			// dts накапливает время декодирования с учётом elst в произвольной точности до
			// проверки диапазона int64.
			dts := new(big.Int).SetUint64(decode)
			dts.Add(dts, big.NewInt(t.offset))
			// pts хранит время показа после добавления композиционного смещения к
			// восстановленному DTS.
			pts := new(big.Int).Add(new(big.Int).Set(dts), big.NewInt(cts))
			if !dts.IsInt64() || !pts.IsInt64() {
				return ErrUnsupportedTiming
			}
			// timeBase задаёт исходную шкалу временных полей MP4 для текущей дорожки.
			timeBase := t.description.TimeBase
			// target задаёт шкалу полной конфигурации, в которой должны выдаваться временные
			// метки Packet.
			target := t.config.Info().TimeBase
			// dt содержит точно пересчитанный DTS; err сообщает о непредставимости временной
			// метки в шкале конфигурации.
			dt, err := rescale(dts.Int64(), timeBase, target)
			if err != nil {
				return err
			}
			// pt содержит точно пересчитанный PTS в шкале конфигурации выходного пакета.
			pt, err := rescale(pts.Int64(), timeBase, target)
			if err != nil {
				return err
			}
			// du содержит точно пересчитанную длительность единицы доступа в шкале
			// конфигурации выходного пакета.
			du, err := rescale(int64(dur), timeBase, target)
			if err != nil {
				return err
			}
			if t.hasDTS && dt < t.lastDTS {
				return fmt.Errorf("%w: DTS дорожки сброшен", ErrUnsupportedTiming)
			}
			t.lastDTS, t.hasDTS = dt, true
			d.samples = append(d.samples, sample{track: t, offset: nextOffset, size: n, info: media.PacketInfo{PTS: pt, DTS: dt, DurationTicks: du, Keyframe: t.description.Track == media.TrackVideo && sf&0x10000 == 0}})
			nextOffset += uint64(n)
			decode += uint64(dur)
		}
		if c.err != nil {
			return c.err
		}
	}
	return nil
}
