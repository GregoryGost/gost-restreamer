package media

import (
	"errors"
	"math"
	"math/big"
)

// ErrHDRUnavailable означает неизвестное или непредставимое на выбранном порте HDR-значение.
var ErrHDRUnavailable = errors.New("HDR-значение не представимо")

// HDRGrid обозначает исходную числовую сетку, а не единицу хранения общего формата.
type HDRGrid uint8

const (
	// HDRGridNone обозначает отсутствие числового представления.
	HDRGridNone HDRGrid = iota
	// HDRGridSEIChromaticity задаёт шаг координаты 1/50000.
	HDRGridSEIChromaticity
	// HDRGridSEILuminance задаёт шаг яркости 1/10000 кд/м².
	HDRGridSEILuminance
	// HDRGridBinaryChromaticity задаёт UQ0.16 координаты AV1 и VP9.
	HDRGridBinaryChromaticity
	// HDRGridBinaryMaxLuminance задаёт UQ24.8 максимальной яркости.
	HDRGridBinaryMaxLuminance
	// HDRGridBinaryMinLuminance задаёт UQ18.14 минимальной яркости.
	HDRGridBinaryMinLuminance
	// HDRGridInteger задаёт целое физическое значение CLL.
	HDRGridInteger
	// HDRGridAMFBinary64 хранит точные IEEE 754 биты входного AMF Number.
	HDRGridAMFBinary64
)

// HDRSource различает происхождение блока до выбора авторитетной сигнализации.
type HDRSource uint8

const (
	// HDRSourceNone обозначает отсутствие блока.
	HDRSourceNone HDRSource = iota
	// HDRSourceH264SEI обозначает H.264 SEI.
	HDRSourceH264SEI
	// HDRSourceHEVCSEI обозначает H.265 SEI.
	HDRSourceHEVCSEI
	// HDRSourceAV1OBU обозначает AV1 metadata OBU.
	HDRSourceAV1OBU
	// HDRSourceContainer обозначает стандартный дескриптор MP4.
	HDRSourceContainer
	// HDRSourceERTMP обозначает полное переопределение colorInfo.
	HDRSourceERTMP
)

// HDRValue сохраняет исходное число и его известность без округления при копировании Info.
type HDRValue struct {
	// Raw содержит кодовое слово либо IEEE 754 биты при Grid=HDRGridAMFBinary64.
	Raw uint64
	// Grid определяет единственную интерпретацию Raw.
	Grid HDRGrid
	// Known отличает известный физический компонент от неопределённого кодового слова, включая CLL=0.
	// Нулевое значение целиком означает отсутствие компонента; Grid!=0 сохраняет присутствующий неизвестный компонент.
	Known bool
}

// HDRChromaticity содержит две независимо известные координаты цветности.
type HDRChromaticity struct {
	// X и Y сохраняют исходные сетки соответствующих координат.
	X, Y HDRValue
}

// HDRMasteringDisplay сохраняет исходное описание дисплея; Primaries всегда упорядочены RGB.
type HDRMasteringDisplay struct {
	// Present различает отсутствие блока и присутствующий блок с неизвестными компонентами.
	Present bool
	// Source указывает источник этого блока до выбора авторитетного источника на конкретной границе.
	Source HDRSource
	// Primaries содержат красную, зелёную и синюю координаты в этом порядке.
	Primaries [3]HDRChromaticity
	// WhitePoint содержит координаты белой точки.
	WhitePoint HDRChromaticity
	// MaxLuminance и MinLuminance содержат яркости в собственных исходных сетках.
	MaxLuminance, MinLuminance HDRValue
}

// HDRContentLight сохраняет две независимо известные верхние границы яркости.
type HDRContentLight struct {
	// Present сохраняет наличие блока даже при двух unspecified значениях.
	Present bool
	// Source обозначает происхождение блока CLL.
	Source HDRSource
	// MaxCLL и MaxFALL содержат максимальную яркость компоненты и среднюю яркость кадра.
	MaxCLL, MaxFALL HDRValue
}

// ColorConfigProvenance отмечает реально прочитанные внешние CICP без дублирования Video.Color.
// Только Container и ERTMP могут уточнить unspecified из заголовков кодека; отсутствие отдельного поля
// не разрешает использовать желаемый цвет. Глубина и диапазон остаются ограничениями кодека.
type ColorConfigProvenance struct {
	// Source обозначает проверенный nclx либо colorConfig текущей полной конфигурации.
	Source HDRSource
	// Primaries, Transfer и Matrix отмечают присутствие соответствующего внешнего поля.
	Primaries, Transfer, Matrix bool
}

// Validate требует внешний источник и хотя бы одно явно присутствующее CICP либо нулевое значение.
func (p ColorConfigProvenance) Validate() error {
	if p == (ColorConfigProvenance{}) {
		return nil
	}
	if (p.Source != HDRSourceContainer && p.Source != HDRSourceERTMP) || (!p.Primaries && !p.Transfer && !p.Matrix) {
		return ErrHDRUnavailable
	}
	return nil
}

// HDRMetadata сравним по значению и не содержит ссылок на изменяемую память.
// Нулевое значение означает отсутствие блоков и внешнего colorConfig, а не известную нулевую яркость.
type HDRMetadata struct {
	// ColorConfig сохраняет происхождение внешних CICP, включая colorInfo без MDCV/CLL.
	ColorConfig ColorConfigProvenance
	// MDCV содержит исходные сведения о дисплее.
	MDCV HDRMasteringDisplay
	// CLL содержит исходные сведения о яркости контента.
	CLL HDRContentLight
}

// NewHDRAMFValue сохраняет конечный неотрицательный AMF Number без потери точности.
// Физические диапазоны конкретного поля проверяет адаптер colorInfo.
func NewHDRAMFValue(value float64) (HDRValue, error) {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
		return HDRValue{}, ErrHDRUnavailable
	}
	return HDRValue{Raw: math.Float64bits(value), Grid: HDRGridAMFBinary64, Known: true}, nil
}

// denominator возвращает число шагов данной фиксированной сетки в физической единице.
func (g HDRGrid) denominator() uint64 {
	switch g {
	case HDRGridSEIChromaticity:
		return 50000
	case HDRGridSEILuminance:
		return 10000
	case HDRGridBinaryChromaticity:
		return 65536
	case HDRGridBinaryMaxLuminance:
		return 256
	case HDRGridBinaryMinLuminance:
		return 16384
	case HDRGridInteger:
		return 1
	}
	return 0
}

// Rational возвращает отдельное точное рациональное значение известного компонента.
// Временная big.Rat не входит в immutable представление и может изменяться вызывающим.
func (v HDRValue) Rational() (*big.Rat, error) {
	if !v.Known {
		return nil, ErrHDRUnavailable
	}
	if v.Grid == HDRGridAMFBinary64 {
		// value восстанавливает исходное двоичное число без десятичного промежуточного округления.
		value := math.Float64frombits(v.Raw)
		if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 {
			return nil, ErrHDRUnavailable
		}
		return new(big.Rat).SetFloat64(value), nil
	}
	// denominator задаёт положительный знаменатель фиксированной сетки исходного значения.
	denominator := v.Grid.denominator()
	if denominator == 0 {
		return nil, ErrHDRUnavailable
	}
	return new(big.Rat).SetFrac(new(big.Int).SetUint64(v.Raw), new(big.Int).SetUint64(denominator)), nil
}

// Project округляет известное неотрицательное значение в target: ближайшее, половина вверх.
// Переполнение и неизвестность возвращают ErrHDRUnavailable; ограничение диапазоном и значения по умолчанию не применяются.
func (v HDRValue) Project(target HDRGrid) (uint64, error) {
	// value содержит точную физическую величину; err сообщает о неизвестном исходном компоненте.
	value, err := v.Rational()
	if err != nil {
		return 0, err
	}
	// denominator задаёт масштаб целевой сетки; AMF не является целочисленной сеткой проекции.
	denominator := target.denominator()
	if denominator == 0 {
		return 0, ErrHDRUnavailable
	}
	value.Mul(value, new(big.Rat).SetInt(new(big.Int).SetUint64(denominator)))
	// quotient и remainder сохраняют целую часть и остаток для точного правила половины.
	quotient, remainder := new(big.Int), new(big.Int)
	quotient.QuoRem(value.Num(), value.Denom(), remainder)
	if new(big.Int).Lsh(remainder, 1).Cmp(value.Denom()) >= 0 {
		quotient.Add(quotient, big.NewInt(1))
	}
	if !quotient.IsUint64() {
		return 0, ErrHDRUnavailable
	}
	return quotient.Uint64(), nil
}

// Float64 возвращает ближайшее binary64 представление известного значения для AMF-порта.
// Исходный Raw остаётся неизменным; диапазоны и десятичная сетка порта проверяются адаптером.
func (v HDRValue) Float64() (float64, error) {
	// value содержит точное значение; err сообщает о недоступной величине.
	value, err := v.Rational()
	if err != nil {
		return 0, err
	}
	// result содержит округлённое binary64 представление физической величины.
	result, _ := value.Float64()
	if math.IsInf(result, 0) {
		return 0, ErrHDRUnavailable
	}
	return result, nil
}

// Values возвращает копию десяти компонентов MDCV в порядке RGBxy, white xy, max и min.
func (m HDRMasteringDisplay) Values() [10]HDRValue {
	return [10]HDRValue{m.Primaries[0].X, m.Primaries[0].Y, m.Primaries[1].X, m.Primaries[1].Y, m.Primaries[2].X, m.Primaries[2].Y, m.WhitePoint.X, m.WhitePoint.Y, m.MaxLuminance, m.MinLuminance}
}

// FullyKnown сообщает, можно ли синтезировать полный внешний блок MDCV без выдуманных значений.
func (m HDRMasteringDisplay) FullyKnown() bool {
	if !m.Present {
		return false
	}
	// value обозначает очередной обязательный компонент блока.
	for _, value := range m.Values() {
		if !value.Known {
			return false
		}
	}
	return true
}

// FullyKnown сообщает, известны ли обе границы CLL присутствующего блока.
func (c HDRContentLight) FullyKnown() bool { return c.Present && c.MaxCLL.Known && c.MaxFALL.Known }

// Validate проверяет каноническое отсутствие блоков, происхождение и числовые представления.
// Неопределённые кодовые слова сохраняются; их физический смысл определяет исходный стандарт.
func (h HDRMetadata) Validate() error {
	// err сообщает о неподтверждённом источнике внешних CICP.
	if err := h.ColorConfig.Validate(); err != nil {
		return err
	}
	if !h.MDCV.Present && h.MDCV != (HDRMasteringDisplay{}) || !h.CLL.Present && h.CLL != (HDRContentLight{}) {
		return ErrHDRUnavailable
	}
	if h.MDCV.Present && (h.MDCV.Source == HDRSourceNone || h.MDCV.Source > HDRSourceERTMP) || h.CLL.Present && (h.CLL.Source == HDRSourceNone || h.CLL.Source > HDRSourceERTMP) {
		return ErrHDRUnavailable
	}
	// values собирает компоненты присутствующих блоков для проверки их единственного представления.
	values := make([]HDRValue, 0, 12)
	if h.MDCV.Present { /* components содержит отдельную копию компонентов дисплея. */
		components := h.MDCV.Values()
		values = append(values, components[:]...)
	}
	if h.CLL.Present {
		values = append(values, h.CLL.MaxCLL, h.CLL.MaxFALL)
	}
	// value обозначает очередной компонент с исходной числовой сеткой.
	for _, value := range values {
		if value == (HDRValue{}) {
			continue
		}
		if value.Grid == HDRGridNone || value.Grid > HDRGridAMFBinary64 {
			return ErrHDRUnavailable
		}
		switch value.Grid {
		case HDRGridSEIChromaticity, HDRGridBinaryChromaticity, HDRGridInteger:
			if value.Raw > math.MaxUint16 {
				return ErrHDRUnavailable
			}
		case HDRGridSEILuminance, HDRGridBinaryMaxLuminance, HDRGridBinaryMinLuminance:
			if value.Raw > math.MaxUint32 {
				return ErrHDRUnavailable
			}
		}
		if value.Grid == HDRGridAMFBinary64 {
			// number нужен для отбрасывания NaN, бесконечности и отрицательного AMF даже у неизвестного компонента.
			number := math.Float64frombits(value.Raw)
			if math.IsNaN(number) || math.IsInf(number, 0) || number < 0 {
				return ErrHDRUnavailable
			}
		}
	}
	return nil
}
