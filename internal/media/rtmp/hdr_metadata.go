package rtmp

import (
	"fmt"
	"math"
	"math/big"
	"strconv"
	"strings"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/bluenviron/gortmplib/pkg/amf0"
)

// colorOverride хранит один полный colorInfo отдельно от устойчивой сигнализации кодека.
// Наличие нового объекта заменяет всё значение; нулевое значение возвращает сведения кодека.
type colorOverride struct {
	// active отличает действующий объект от Undefined или пустого объекта.
	active bool
	// hdr содержит только блоки, явно переданные в текущем полном объекте.
	hdr media.HDRMetadata
	// config сохраняет bitDepth, primaries, transfer и matrix; known различает отсутствие и код 0.
	config [4]uint8
	known  [4]bool
}

// amfObject принимает обе распространённые объектные формы AMF и отклоняет повторяющиеся ключи.
// Проверка дубликатов не позволяет разным потребителям выбрать разные значения одного поля.
func amfObject(value any) (amf0.Object, error) {
	// object получает исходные пары, без копирования числовых значений через другой формат.
	var object amf0.Object
	// typed выбирает конкретное объектное представление значения.
	switch typed := value.(type) {
	case amf0.Object:
		object = typed
	case amf0.ECMAArray:
		object = amf0.Object(typed)
	default:
		return nil, fmt.Errorf("%w: требуется AMF Object", ErrCodec)
	}
	// seen хранит только имена этого уже ограниченного AMF-объекта. Линейная проверка
	// не превращает допустимое число уникальных полей в квадратичное число сравнений.
	seen := make(map[string]struct{}, len(object))
	// entry выбирает пару для проверки отсутствия повторного ключа.
	for _, entry := range object {
		// duplicate отмечает уже встреченное имя текущего полного объекта.
		if _, duplicate := seen[entry.Key]; duplicate {
			return nil, fmt.Errorf("%w: повторное поле colorInfo", ErrCodec)
		}
		seen[entry.Key] = struct{}{}
	}
	return object, nil
}

// amfHDRNumber проверяет обязательное конечное число поля и диапазон выбранного AMF-порта.
// Для координат допускается только binary64 представление числа с четырьмя десятичными знаками.
func amfHDRNumber(object amf0.Object, name string, minimum, maximum float64, coordinate bool) (media.HDRValue, error) {
	// raw и present сохраняют фактическое поле, number и numeric проверяют AMF Number.
	raw, present := object.Get(name)
	number, numeric := raw.(float64)
	if !present || !numeric || math.IsNaN(number) || math.IsInf(number, 0) || number < minimum || number > maximum {
		return media.HDRValue{}, fmt.Errorf("%w: colorInfo.%s", ErrCodec, name)
	}
	if coordinate && number != math.Round(number*10000)/10000 {
		return media.HDRValue{}, fmt.Errorf("%w: точность colorInfo.%s", ErrCodec, name)
	}
	return media.NewHDRAMFValue(number)
}

// parseColorOverride разбирает полный colorInfo без подмешивания старых значений.
// Неполный MDCV/CLL не дополняется нулями; неподдержанная объектная форма явно отклоняется.
func parseColorOverride(value any) (colorOverride, error) {
	// result собирает проверенное значение до присваивания состоянию публикации.
	var result colorOverride
	// reset распознаёт нормативный Undefined без подстановки отсутствующих чисел.
	if _, reset := value.(amf0.Undefined); reset {
		return result, nil
	}
	// object и err содержат полный объект и отказ проверки его формы.
	object, err := amfObject(value)
	if err != nil || len(object) == 0 {
		return result, err
	}
	result.active = true
	// raw и present выбирают необязательный раздел цвета текущего объекта.
	if raw, present := object.Get("colorConfig"); present {
		// config содержит пары цветовых индексов без подстановки желаемого формата;
		// err сообщает о неподдержанной форме или повторных полях этого раздела.
		config, err := amfObject(raw)
		if err != nil {
			return colorOverride{}, err
		}
		// index и name сопоставляют поля со сравнимыми фиксированными ячейками override.
		for index, name := range []string{"bitDepth", "colorPrimaries", "transferCharacteristics", "matrixCoefficients"} {
			// value и exists отличают отсутствующий индекс от известного нуля.
			if value, exists := config.Get(name); exists {
				// number и numeric проверяют конечное целое числовое значение кода.
				number, numeric := value.(float64)
				if !numeric || math.IsNaN(number) || math.IsInf(number, 0) || number < 0 || number > 255 || math.Trunc(number) != number || index == 0 && number != 8 && number != 10 && number != 12 {
					return colorOverride{}, fmt.Errorf("%w: colorConfig.%s", ErrCodec, name)
				}
				result.config[index], result.known[index] = uint8(number), true
			}
		}
	}
	// raw и present выбирают новый CLL, не наследуя отсутствие секции от прежнего объекта.
	if raw, present := object.Get("hdrCll"); present {
		// block содержит полный набор CLL; err сообщает о неподдержанной форме или повторных полях.
		// maxCLL и maxFALL сохраняют точные числа AMF после проверки их диапазона.
		block, err := amfObject(raw)
		if err != nil {
			return colorOverride{}, err
		}
		maxCLL, err := amfHDRNumber(block, "maxCLL", 0.0001, 10000, false)
		if err != nil {
			return colorOverride{}, err
		}
		maxFALL, err := amfHDRNumber(block, "maxFall", 0.0001, 10000, false)
		if err != nil {
			return colorOverride{}, err
		}
		result.hdr.CLL = media.HDRContentLight{Present: true, Source: media.HDRSourceERTMP, MaxCLL: maxCLL, MaxFALL: maxFALL}
	}
	// raw и present выбирают новый дисплей; все его компоненты проверяются до применения объекта.
	if raw, present := object.Get("hdrMdcv"); present {
		// block содержит полный набор дисплея; err сообщает о неподдержанной форме или повторных полях.
		// values собирает координаты RGB, белой точки и границы яркости без округления.
		block, err := amfObject(raw)
		if err != nil {
			return colorOverride{}, err
		}
		var values [10]media.HDRValue
		// index и name задают поле и его проверяемый физический диапазон.
		for index, name := range []string{"redX", "redY", "greenX", "greenY", "blueX", "blueY", "whitePointX", "whitePointY", "maxLuminance", "minLuminance"} {
			// minimum и maximum задают диапазон координаты либо яркости данного поля.
			minimum, maximum := 0.0001, 0.7400
			if index < 8 && index%2 == 1 {
				maximum = 0.8400
			} else if index == 8 {
				minimum, maximum = 5, 10000
			} else if index == 9 {
				maximum = 5
			}
			values[index], err = amfHDRNumber(block, name, minimum, maximum, index < 8)
			if err != nil {
				return colorOverride{}, err
			}
		}
		result.hdr.MDCV = media.HDRMasteringDisplay{Present: true, Source: media.HDRSourceERTMP, Primaries: [3]media.HDRChromaticity{{X: values[0], Y: values[1]}, {X: values[2], Y: values[3]}, {X: values[4], Y: values[5]}}, WhitePoint: media.HDRChromaticity{X: values[6], Y: values[7]}, MaxLuminance: values[8], MinLuminance: values[9]}
	}
	return result, result.hdr.Validate()
}

// colorCode переводит проверенное имя CICP в сетевой индекс, сохраняя неизвестные числовые коды.
func colorCode(value string, names map[uint8]string) (uint8, error) {
	// code и name перебирают точную таблицу данной цветовой характеристики.
	for code, name := range names {
		if value == name {
			return code, nil
		}
	}
	// prefix выбирает действующее codec-specific обозначение неизвестного индекса.
	for _, prefix := range []string{"cicp:", "iso-"} {
		if strings.HasPrefix(value, prefix) {
			// number и err проверяют каноническую десятичную форму неизвестного CICP.
			number, err := strconv.ParseUint(strings.TrimPrefix(value, prefix), 10, 8)
			if err == nil && value == prefix+strconv.FormatUint(number, 10) {
				return uint8(number), nil
			}
		}
	}
	return 0, media.ErrHDRUnavailable
}

// colorTables возвращает три независимые таблицы CICP, применяемые на AMF-границе.
func colorTables() [3]map[uint8]string {
	return [3]map[uint8]string{
		{1: "bt709", 2: "unspecified", 4: "bt470m", 5: "bt470bg", 6: "smpte170m", 7: "smpte240m", 8: "film", 9: "bt2020"},
		{1: "bt709", 2: "unspecified", 4: "gamma22", 5: "gamma28", 6: "smpte170m", 7: "smpte240m", 8: "linear", 13: "iec61966-2-1", 14: "bt2020-10", 15: "bt2020-12", 16: "smpte2084", 18: "arib-std-b67"},
		{0: "gbr", 1: "bt709", 2: "unspecified", 4: "fcc", 5: "bt470bg", 6: "smpte170m", 7: "smpte240m", 9: "bt2020nc", 10: "bt2020c"},
	}
}

// apply проверяет внешний цвет против известных ограничений кодека и выбирает полный snapshot.
// Только unspecified можно уточнить; зарезервированные коды/UNKNOWN и известные характеристики остаются ограничениями.
func (o colorOverride) apply(video core.VideoFormat, coded media.HDRMetadata) (core.VideoFormat, media.HDRMetadata, error) {
	if !o.active {
		return video, coded, nil
	}
	if o.known[0] && uint8(video.BitDepth) != o.config[0] {
		return video, coded, fmt.Errorf("%w: colorInfo bitDepth противоречит потоку", ErrCodec)
	}
	// fields указывает на три цветовых имени в собственной копии формата, tables задаёт их CICP.
	fields := [3]*string{&video.Color.Primaries, &video.Color.Transfer, &video.Color.Matrix}
	tables := colorTables()
	// index и field выбирают известную характеристику и соответствующий внешний индекс.
	for index, field := range fields {
		if !o.known[index+1] {
			continue
		}
		// code и err сохраняют фактическое coded значение для проверки противоречия.
		code, err := colorCode(*field, tables[index])
		if err != nil || code != 2 && code != o.config[index+1] {
			return video, coded, fmt.Errorf("%w: colorInfo CICP противоречит потоку", ErrCodec)
		}
		// name содержит известное имя; known отличает его от канонического неизвестного индекса.
		name, known := tables[index][o.config[index+1]]
		if !known {
			name = "cicp:" + strconv.Itoa(int(o.config[index+1]))
		}
		*field = name
	}
	// metadata содержит новый полный объект без происхождения, оставшегося от прежнего override.
	metadata := o.hdr
	if codec.UsesExternalColor(video) && (o.known[1] || o.known[2] || o.known[3]) {
		metadata.ColorConfig = media.ColorConfigProvenance{Source: media.HDRSourceERTMP, Primaries: o.known[1], Transfer: o.known[2], Matrix: o.known[3]}
	}
	return video, metadata, nil
}

// projectAMFValue проверяет физический диапазон до и после прямой проекции координат на 1/10000.
// Яркости переводятся в binary64 непосредственно из исходной рациональной величины.
func projectAMFValue(value media.HDRValue, minimum, maximum float64, coordinate bool) (float64, error) {
	// number содержит исходную физическую величину без промежуточной целой сетки кодека.
	// err сообщает об неизвестном либо непредставимом исходном компоненте.
	number, err := value.Float64()
	if err != nil || number < minimum || number > maximum {
		return 0, media.ErrHDRUnavailable
	}
	if coordinate {
		// rational содержит точное исходное число; err сообщает о невозможности восстановить его дробь.
		// quotient и remainder хранят частное и остаток для округления к ближайшему с половиной от нуля.
		rational, err := value.Rational()
		if err != nil {
			return 0, err
		}
		rational.Mul(rational, big.NewRat(10000, 1))
		quotient, remainder := new(big.Int), new(big.Int)
		quotient.QuoRem(rational.Num(), rational.Denom(), remainder)
		if new(big.Int).Lsh(remainder, 1).Cmp(rational.Denom()) >= 0 {
			quotient.Add(quotient, big.NewInt(1))
		}
		if !quotient.IsUint64() {
			return 0, media.ErrHDRUnavailable
		}
		number = float64(quotient.Uint64()) / 10000
	}
	if number < minimum || number > maximum {
		return 0, media.ErrHDRUnavailable
	}
	return number, nil
}
