package codec

import (
	"bytes"
	"errors"
	"fmt"
	"math"
	"strconv"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/abema/go-mp4"
	"github.com/bluenviron/mediacommon/v2/pkg/codecs/av1"
)

// OBU представляет элемент AV1 с исходной записью и её полезной нагрузкой без копирования входного
// буфера.
type OBU struct {
	// typ содержит четырёхбитный тип элемента OBU.
	typ byte
	// payload ссылается на тело OBU без заголовка и длины. raw ссылается на всю исходную запись
	// этого OBU.
	payload, raw []byte
}

// SplitOBUs разбивает поток на OBU с обязательным полем длины и без расширенного заголовка.
// Возвращаемые срезы ссылаются на raw; число OBU ограничено 4096.
func SplitOBUs(raw []byte) ([]OBU, error) {
	// out накапливает выделенные OBU в порядке входного потока.
	var out []OBU
	for len(raw) > 0 {
		if len(out) >= 4096 {
			return nil, ErrLimit
		}
		// h содержит первый байт заголовка очередного OBU.
		h := raw[0]
		if h&0x85 != 0 || h&2 == 0 {
			return nil, fmt.Errorf("%w: OBU header", ErrCodec)
		}
		// size накапливает длину тела OBU из переменного целочисленного поля.
		var size uint64
		// n обозначает позицию очередного байта длины, затем начало тела OBU.
		n := 1
		for {
			if n >= len(raw) || n > 8 {
				return nil, ErrCodec
			}
			// v содержит очередной байт переменного поля длины OBU.
			v := raw[n]
			size |= uint64(v&127) << uint(7*(n-1))
			n++
			if v&128 == 0 {
				break
			}
		}
		if size > uint64(len(raw)-n) {
			return nil, ErrCodec
		}
		// end задаёт исключающую правую границу OBU во входном буфере.
		end := n + int(size)
		out = append(out, OBU{typ: (h >> 3) & 15, payload: raw[n:end], raw: raw[:end]})
		raw = raw[end:]
	}
	return out, nil
}

// AV1Description сохраняет разобранную последовательность и сведения о формате, подтверждённые
// заголовками кадров.
type AV1Description struct {
	// seq содержит проверенный заголовок единственной последовательности AV1.
	seq av1.SequenceHeader
	// seqBytes хранит собственную копию тела заголовка последовательности для сравнения последующих
	// TU.
	seqBytes []byte
	// width хранит подтверждённую заголовком независимого кадра ширину в пикселях. height хранит
	// подтверждённую высоту в пикселях.
	width, height uint32
	// fps хранит частоту кадров из timing_info либо проверенных сведений вызывающего.
	fps core.FrameRate
	// provenance сохраняет обозначение источника подтверждённой частоты кадров.
	provenance string
}

// ParseAV1Config сверяет av1C с единственным заголовком последовательности AV1
// и сохраняет его копию. Частота берётся из корректного timing_info с равными интервалами, если он
// присутствует.
func ParseAV1Config(raw []byte) (AV1Description, error) {
	if len(raw) < 4 {
		return AV1Description{}, ErrCodec
	}
	// c принимает поля записи av1C для сверки с заголовком последовательности.
	var c mp4.Av1C
	// err сообщает об отказе разбора записи av1C.
	if _, err := mp4.Unmarshal(bytes.NewReader(raw), uint64(len(raw)), &c, mp4.Context{}); err != nil {
		return AV1Description{}, fmt.Errorf("%w: av1C: %v", ErrCodec, err)
	}
	if c.Marker != 1 || c.Version != 1 || c.Reserved != 0 || c.InitialPresentationDelayPresent != 0 || c.InitialPresentationDelayMinusOne != 0 {
		return AV1Description{}, ErrCodec
	}
	// obus содержит элементы конфигурации av1C. err сообщает об отказе разбиения их потока.
	obus, err := SplitOBUs(c.ConfigOBUs)
	if err != nil {
		return AV1Description{}, err
	}
	if len(obus) == 0 || obus[0].typ != 1 {
		return AV1Description{}, ErrCodec
	}
	// d накапливает проверенное описание AV1 из записи конфигурации.
	var d AV1Description
	// found отмечает, что единственный заголовок последовательности уже встречен.
	found := false
	// o обозначает очередной элемент OBU из конфигурации av1C.
	for _, o := range obus {
		if o.typ != 1 {
			if o.typ != 5 && o.typ != 15 {
				return d, ErrCodec
			}
			continue
		}
		if found {
			return d, ErrCodec
		}
		found = true
		if d.seq, err = parseSequence(o.payload); err != nil {
			return d, fmt.Errorf("%w: sequence: %v", ErrCodec, err)
		}
		d.seqBytes = bytes.Clone(o.payload)
	}
	if !found {
		return d, ErrCodec
	}
	if d.seq.SeqLevelIdx[0] > 23 && d.seq.SeqLevelIdx[0] != 31 {
		return d, fmt.Errorf("%w: зарезервированный level", ErrCodec)
	}
	// s указывает на разобранный заголовок последовательности для проверки ограничений av1C.
	s := &d.seq
	// cc содержит цветовые параметры последовательности для проверки поддержанного SDR и
	// согласованности av1C.
	cc := s.ColorConfig
	if s.SeqProfile > 2 || s.ReducedStillPictureHeader || s.StillPicture || s.OperatingPointsCntMinus1 != 0 || s.OperatingPointIdc[0] != 0 || s.FrameIDNumbersPresentFlag || s.DecoderModelInfoPresentFlag || s.EnableSuperRes {
		return d, fmt.Errorf("%w: требуется однослойный поток без superres, frame IDs и film grain", ErrCodec)
	}

	if c.SeqProfile != s.SeqProfile || c.SeqLevelIdx0 != s.SeqLevelIdx[0] || c.SeqTier0 != boolByte(s.SeqTier[0]) || c.HighBitdepth != boolByte(cc.HighBitDepth) || c.TwelveBit != boolByte(cc.TwelveBit) || c.Monochrome != boolByte(cc.MonoChrome) || c.ChromaSubsamplingX != boolByte(cc.SubsamplingX) || c.ChromaSubsamplingY != boolByte(cc.SubsamplingY) || c.ChromaSamplePosition != uint8(cc.ChromaSamplePosition) {
		return d, fmt.Errorf("%w: av1C расходится с sequence", ErrCodec)
	}
	// t содержит необязательные временные параметры последовательности.
	if t := s.TimingInfo; t != nil && t.EqualPictureInterval {
		// den содержит знаменатель частоты кадров с учётом числа тактов на картинку.
		den := uint64(t.NumUnitsInDisplayTick) * (uint64(t.NumTicksPerPictureMinus1) + 1)
		// num содержит числитель частоты кадров из TimeScale.
		num := uint64(t.TimeScale)
		if num == 0 || den == 0 {
			return d, ErrCodec
		}
		// g содержит общий делитель для точного сокращения частоты кадров.
		g := gcd(num, den)
		num /= g
		den /= g
		if num > math.MaxUint32 || den > math.MaxUint32 {
			return d, ErrCodec
		}
		d.fps = core.FrameRate{Numerator: uint32(num), Denominator: uint32(den)}
		d.provenance = "av1.timing_info"
	}
	return d, nil
}

// Представляет логическое поле av1C байтом 0 или 1.
func boolByte(v bool) uint8 {
	if v {
		return 1
	}
	return 0
}

// Вычисляет наибольший общий делитель для сокращения рациональной частоты кадров.
func gcd(a, b uint64) uint64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// bitReader последовательно читает биты из буфера и сохраняет первую ошибку чтения.
type bitReader struct {
	// b содержит буфер читаемого битового потока.
	b []byte
	// p задаёт позицию следующего читаемого бита от начала буфера.
	p int
	// err сохраняет ошибку чтения; после её появления последующие чтения возвращают ноль.
	err error
}

// Читает до 32 битов от старшего к младшему; усечение или недопустимая длина сохраняют ErrCodec, после
// чего чтения возвращают ноль.
func (r *bitReader) read(n int) uint32 {
	if r.err != nil {
		return 0
	}
	if n < 0 || n > 32 || len(r.b)*8-r.p < n {
		r.err = ErrCodec
		return 0
	}
	// v накапливает читаемое беззнаковое поле от старших битов к младшим.
	var v uint32
	for range n {
		v = v<<1 | uint32(r.b[r.p/8]>>uint(7-r.p%8)&1)
		r.p++
	}
	return v
}

// frame проверяет часть uncompressed_header, определяющую независимость и размеры.
// frame_size_override и EnableSuperRes не получают фиктивные размеры из максимальных размеров последовательности.
func (d *AV1Description) frame(payload []byte) (key, shown bool, err error) {
	// r читает заголовок текущего кадра AV1 по битам.
	r := bitReader{b: payload}
	// s указывает на параметры последовательности, определяющие состав заголовка кадра.
	s := &d.seq
	if r.read(1) != 0 {
		r.read(3)
		return false, true, r.err
	}
	// typ содержит двухбитный тип кадра AV1.
	typ := r.read(2)
	shown = r.read(1) != 0
	if !shown {
		r.read(1)
	}
	if typ != 3 && (typ != 0 || !shown) {
		r.read(1)
	}
	r.read(1)
	// allow содержит признак разрешения инструментов экранного содержимого для этого кадра.
	allow := uint32(s.SeqForceScreenContentTools)
	if allow == 2 {
		allow = r.read(1)
	}
	if allow != 0 && s.SeqForceIntegerMv == 2 {
		r.read(1)
	}
	// override отмечает явное переопределение размеров кадра, которое этот порт отклоняет.
	override := typ == 3
	if typ != 3 {
		override = r.read(1) != 0
	}
	if override {
		return false, shown, fmt.Errorf("%w: frame_size_override", ErrCodec)
	}
	if s.EnableOrderHint {
		r.read(int(s.OrderHintBitsMinus1) + 1)
	}
	if r.err != nil {
		return false, shown, r.err
	}
	if typ == 0 && shown {
		// Поля primary_ref_frame/refresh_frame_flags отсутствуют для показанного KEY_FRAME.
		if r.read(1) != 0 {
			return false, shown, fmt.Errorf("%w: render_size", ErrCodec)
		}
		if r.err != nil {
			return false, shown, r.err
		}
		d.width = s.MaxFrameWidthMinus1 + 1
		d.height = s.MaxFrameHeightMinus1 + 1
		return true, true, nil
	}
	return false, shown, nil
}

// InspectAV1TU разбирает один TU в поддержанном AV1. fps применяется только
// как проверенные сведения вызывающего, если частота не определена в timing_info;
// при наличии обеих частот они должны совпадать.
// Независимый старт требует заголовка последовательности в этом же TU
// и показанного KEY_FRAME первым кадром TU.
func InspectAV1TU(config, tu []byte, fps core.FrameRate) (core.VideoFormat, bool, error) {
	// d содержит описание AV1 из конфигурации. err сообщает об отказе его разбора.
	d, err := ParseAV1Config(config)
	if err != nil {
		return core.VideoFormat{}, false, err
	}
	return d.Inspect(tu, fps)
}

// Inspect проверяет TU против сохранённой последовательности и обновляет известные размеры и частоту.
// Несогласованный FPS отклоняется, нехватка размеров или FPS возвращает ErrUnready; независимый старт
// требует последовательность перед кадрами и показанный KEY_FRAME первым кадром TU.
func (d *AV1Description) Inspect(tu []byte, fps core.FrameRate) (core.VideoFormat, bool, error) {
	if d.fps.Validate() == nil && fps.Validate() == nil && !d.fps.Equal(fps) {
		return core.VideoFormat{}, false, fmt.Errorf("%w: timing_info расходится с metadata", ErrCodec)
	}
	// list содержит OBU проверяемого TU. err сообщает о повреждении структуры OBU.
	list, err := SplitOBUs(tu)
	if err != nil {
		return core.VideoFormat{}, false, err
	}
	// sequence отмечает заголовок последовательности перед кадрами этого TU. firstFrameKey сохраняет
	// независимость первого кадра. frame отмечает наличие хотя бы одного заголовка кадра. shown
	// отмечает наличие показанного кадра.
	sequence, firstFrameKey, frame, shown := false, false, false, false
	// o обозначает очередной OBU проверяемого TU.
	for _, o := range list {
		switch o.typ {
		case 1:
			if frame || !bytes.Equal(o.payload, d.seqBytes) {
				return core.VideoFormat{}, false, ErrCodec
			}
			sequence = true
		case 3, 6:
			if shown {
				return core.VideoFormat{}, false, fmt.Errorf("%w: несколько показанных кадров в TU", ErrCodec)
			}
			// k показывает независимость разобранного кадра. show показывает, предназначен
			// ли кадр для показа. e сообщает об отказе разбора заголовка кадра.
			k, show, e := d.frame(o.payload)
			if e != nil {
				return core.VideoFormat{}, false, e
			}
			if !frame {
				firstFrameKey = k
			}
			frame = true
			shown = shown || show
		case 2:
			if frame || len(o.payload) != 0 {
				return core.VideoFormat{}, false, ErrCodec
			}
		case 4, 5, 15:
		default:
			return core.VideoFormat{}, false, ErrCodec
		}
	}
	if !frame || !shown {
		return core.VideoFormat{}, false, ErrCodec
	}
	if d.fps.Validate() != nil {
		d.fps = fps
		if fps.Validate() == nil {
			d.provenance = "caller.verified_framerate"
		}
	}
	if d.width == 0 || d.height == 0 || d.fps.Validate() != nil {
		return core.VideoFormat{}, false, ErrUnready
	}
	return d.videoFormat(), sequence && firstFrameKey, nil
}

// videoFormat собирает наблюдаемые поля сохранённой последовательности и ранее проверенного кадра.
// Вызывающий устанавливает width, height и fps только из подтверждённых сведений.
func (d *AV1Description) videoFormat() core.VideoFormat {
	// s указывает на сохранённый заголовок последовательности при построении наблюдаемого формата.
	s := &d.seq
	// cc содержит цветовые параметры сохранённой последовательности.
	cc := s.ColorConfig
	// color содержит диапазон и именованные характеристики цвета наблюдаемого видео.
	color := core.ColorInfo{Range: "limited", Primaries: primaryName(uint8(cc.ColorPrimaries), "cicp:"), Transfer: cicpTransferName(uint8(cc.TransferCharacteristics), "cicp:"), Matrix: cicpMatrixName(uint8(cc.MatrixCoefficients), "cicp:")}
	if cc.ColorRange {
		color.Range = "full"
	}
	// level содержит обозначение уровня AV1, полученное из индекса последовательности.
	level := strconv.Itoa(2+int(s.SeqLevelIdx[0])/4) + "." + strconv.Itoa(int(s.SeqLevelIdx[0])%4)
	if s.SeqLevelIdx[0] == 31 {
		level = "unspecified"
	}
	// tier содержит обозначение main или high по признаку уровня последовательности.
	tier := "main"
	if s.SeqTier[0] {
		tier = "high"
	}
	// f содержит наблюдаемый формат видео, подтверждённый последовательностью и кадрами TU.
	f := core.VideoFormat{Codec: core.CodecAV1, Width: d.width, Height: d.height, FrameRate: d.fps, CodecProfile: []string{"main", "high", "professional"}[s.SeqProfile], Level: level, Tier: tier, PixelFormat: av1Pixel(cc), BitDepth: uint8(cc.BitDepth), Color: color}
	return f
}

// ValidateAV1Config сверяет declared с полями исходного av1C и последовательности raw.
// Размеры сравниваются с максимальным кадром: поддержанный порт запрещает frame_size_override,
// а фактический независимый старт дополнительно подтверждается при Inspect. Частота declared
// используется только при отсутствии равномерного timing_info, без подмены остальных полей.
func ValidateAV1Config(raw []byte, declared core.VideoFormat) error {
	return validateAV1Config(raw, declared, media.ColorConfigProvenance{})
}

// validateAV1Config проверяет исходные заголовки против declared с отдельным доказательством внешних CICP.
// provenance разрешает только уточнение действительно unspecified осей; остальные ограничения неизменны.
func validateAV1Config(raw []byte, declared core.VideoFormat, provenance media.ColorConfigProvenance) error {
	// d содержит разобранную последовательность; err сообщает о повреждённых заголовках.
	d, err := ParseAV1Config(raw)
	if err != nil {
		return err
	}
	d.width, d.height = d.seq.MaxFrameWidthMinus1+1, d.seq.MaxFrameHeightMinus1+1
	if d.fps.Validate() != nil {
		d.fps = declared.FrameRate
	}
	return CompatibleVideoConstraints(d.videoFormat(), declared, provenance)
}

// av1Pixel выводит имя плоскостного формата из фактической цветовой конфигурации c.
// Монохромность проверяется раньше субдискретизации, поскольку её служебные X/Y также равны единице.
func av1Pixel(c av1.SequenceHeader_ColorConfig) string {
	// pixel содержит базовое имя восьмибитного формата до добавления глубины.
	pixel := "yuv444p"
	if c.MonoChrome {
		pixel = "gray"
	} else if c.SubsamplingX {
		pixel = "yuv422p"
		if c.SubsamplingY {
			pixel = "yuv420p"
		}
	}
	if c.BitDepth > 8 {
		pixel += strconv.Itoa(c.BitDepth) + "le"
	}
	return pixel
}

var (
	// ErrCodec обозначает повреждённый либо неподдержанный сжатый поток.
	ErrCodec = errors.New("неподдерживаемый сжатый поток")
	// ErrLimit обозначает превышение установленного ограничения числа элементов кодека.
	ErrLimit = errors.New("превышен бюджет кодека")
	// ErrUnready обозначает отсутствие достаточных подтверждённых сведений для определения формата.
	ErrUnready = errors.New("формат источника ещё не определён")
)

// ErrUnsupported обозначает синтаксически допустимую форму вне конечного медиапорта.
var ErrUnsupported = errors.New("форма кодека не поддержана медиапортом")

// ErrTiming обозначает временное значение, которое невозможно точно представить требуемой шкалой.
var ErrTiming = errors.New("время не представимо точно")

// AV1ConfigFromTU строит av1C из исходного заголовка последовательности первого независимого TU.
// Префикс шаблона сверяется с фактической последовательностью; параметры профиля не подставляются.
func AV1ConfigFromTU(template, tu []byte, verifiedFPS core.FrameRate) ([]byte, core.VideoFormat, error) {
	// err сообщает о неподдержанной или повреждённой конфигурации шаблона AV1.
	if _, err := ParseAV1Config(template); err != nil {
		return nil, core.VideoFormat{}, err
	}
	// obus содержит элементы первого TU. err сообщает о повреждении их структуры.
	obus, err := SplitOBUs(tu)
	if err != nil {
		return nil, core.VideoFormat{}, err
	}
	// sequence ссылается на единственную исходную запись заголовка последовательности из TU.
	var sequence []byte
	// o обозначает очередной OBU первого TU при поиске заголовка последовательности.
	for _, o := range obus {
		if o.typ == 1 {
			if sequence != nil {
				return nil, core.VideoFormat{}, ErrCodec
			}
			sequence = o.raw
		}
	}
	if sequence == nil {
		return nil, core.VideoFormat{}, ErrUnready
	}
	// headers содержит новую запись av1C с копией префикса шаблона и фактической последовательностью
	// первого TU.
	headers := append(bytes.Clone(template[:4]), sequence...)
	// actual содержит фактически подтверждённый формат первого TU. independent показывает, разрешает
	// ли он независимый запуск.
	actual, independent, err := InspectAV1TU(headers, tu, verifiedFPS)
	if err != nil {
		return nil, core.VideoFormat{}, err
	}
	if !independent {
		return nil, core.VideoFormat{}, fmt.Errorf("%w: первый TU не независим", ErrCodec)
	}
	return headers, actual, nil
}
