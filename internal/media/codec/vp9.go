package codec

import (
	"fmt"
	"strconv"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
)

// VP9Description хранит проверенную запись и геометрию опорных картинок одной последовательности.
// Экземпляр используется последовательно; запись сама не содержит FPS и размеров.
type VP9Description struct {
	// Profile,Level,BitDepth и ChromaSubsampling сохраняют исходные поля записи.
	Profile, Level, BitDepth, ChromaSubsampling uint8
	// Color содержит внешние CICP и полный либо ограниченный диапазон сигнала.
	Color core.ColorInfo
	// refs сохраняет размеры восьми декодированных опорных картинок.
	refs [8][2]uint32
	// width/height сохраняют постоянную подтверждённую геометрию реализованного пути обработки.
	width, height uint32
}

// ParseVP9Config проверяет VPCodecConfigurationRecord без версии и флагов FullBox.
// VP9 не имеет данных инициализации; профили 1/3 и 12 бит вне конечного пути.
func ParseVP9Config(raw []byte) (VP9Description, error) {
	// result сохраняет фактические поля без желаемых значений по умолчанию.
	var result VP9Description
	if len(raw) != 8 || be.Uint16(raw[6:8]) != 0 {
		return result, ErrCodec
	}
	result.Profile, result.Level, result.BitDepth, result.ChromaSubsampling = raw[0], raw[1], raw[2]>>4, (raw[2]>>1)&7
	if result.Profile > 3 || result.ChromaSubsampling > 3 {
		return result, ErrCodec
	}
	if result.Profile != 0 && result.Profile != 2 || result.BitDepth != 8 && result.BitDepth != 10 {
		return result, ErrUnsupported
	}
	if result.Profile == 0 && result.BitDepth != 8 || result.Profile == 2 && result.BitDepth != 10 || result.ChromaSubsampling > 1 {
		return result, ErrCodec
	}
	switch result.Level {
	case 0, 10, 11, 20, 21, 30, 31, 40, 41, 50, 51, 52, 60, 61, 62:
	default:
		return result, ErrCodec
	}
	result.Color = core.ColorInfo{Primaries: primaryName(raw[3], "iso-"), Transfer: transferName(raw[4]), Matrix: matrixName(raw[5]), Range: "limited"}
	if raw[2]&1 != 0 {
		result.Color.Range = "full"
	}
	return result, nil
}

// VP9Format извлекает фактический формат из записи и независимого ключевого кадра; FPS должен быть измерен вызывающим.
func VP9Format(headers, payload []byte, fps core.FrameRate) (core.VideoFormat, error) {
	// actual содержит проверенный формат; err сообщает о повреждении заголовка или записи либо отсутствии FPS.
	actual, _, err := InspectVP9AU(headers, payload, fps)
	return actual, err
}

// InspectVP9AU проверяет один AU с новым состоянием; межкадровый кадр без истории опорных картинок возвращает ErrUnready.
// Для последовательности вызывающий сохраняет ParseVP9Config результат и вызывает его Inspect.
func InspectVP9AU(headers, payload []byte, fps core.FrameRate) (core.VideoFormat, bool, error) {
	// description содержит проверенную запись; err сохраняет отказ его разбора.
	description, err := ParseVP9Config(headers)
	if err != nil {
		return core.VideoFormat{}, false, err
	}
	return description.Inspect(payload, fps)
}

// vp9Size читает ширину/высоту frame_size и не подменяет их размерами записи контейнера.
func vp9Size(r *bitReader) [2]uint32 { return [2]uint32{r.read(16) + 1, r.read(16) + 1} }

// vp9Color читает color_config сжатого кадра и сверяет обобщённое цветовое пространство с внешней записью.
// Раздельные характеристики цвета не выводятся из одного обобщённого кода.
func (d VP9Description) vp9Color(r *bitReader) error {
	// depth содержит фактическую глубину из заголовка, space — обобщённое цветовое пространство.
	depth := uint8(8)
	if d.Profile >= 2 {
		depth = 10
		if r.read(1) != 0 {
			depth = 12
		}
	}
	// space сохраняет трёхбитный обобщённый код цветового пространства кадра.
	space := r.read(3)
	if space == 6 {
		return ErrCodec
	}
	if space == 7 {
		return ErrUnsupported
	}
	// full содержит диапазон сигнала, ограничение профилей 0/2 задаёт 4:2:0 без дополнительного синтаксиса.
	full := r.read(1) != 0
	if depth != d.BitDepth || full != (d.Color.Range == "full") {
		return ErrCodec
	}
	switch space {
	case 1, 3:
		if d.Color.Matrix != "smpte170m" && d.Color.Matrix != "bt470bg" && d.Color.Matrix != "unspecified" {
			return ErrCodec
		}
	case 2:
		if d.Color.Primaries != "unspecified" && d.Color.Primaries != "bt709" || d.Color.Matrix != "unspecified" && d.Color.Matrix != "bt709" {
			return ErrCodec
		}
	case 4:
		if d.Color.Primaries != "unspecified" && d.Color.Primaries != "smpte240m" || d.Color.Matrix != "unspecified" && d.Color.Matrix != "smpte240m" {
			return ErrCodec
		}
	case 5:
		if d.Color.Primaries != "unspecified" && d.Color.Primaries != "bt2020" || d.Color.Matrix != "unspecified" && d.Color.Matrix != "bt2020nc" && d.Color.Matrix != "bt2020c" {
			return ErrCodec
		}
	}
	return r.err
}

// Inspect проверяет один показанный кадр, запись, геометрию кадра и опорных картинок, полный несжатый заголовок.
// Формы superframe/altref/show_existing_frame вне реализованной обработки. Состояние опорных картинок
// обновляется только после успешного синтаксического разбора и проверки постоянной геометрии.
func (d *VP9Description) Inspect(payload []byte, fps core.FrameRate) (core.VideoFormat, bool, error) {
	if len(payload) == 0 {
		return core.VideoFormat{}, false, ErrCodec
	}
	if payload[len(payload)-1]&0xe0 == 0xc0 {
		return core.VideoFormat{}, false, fmt.Errorf("%w: VP9 superframe", ErrUnsupported)
	}
	// r читает несжатый заголовок текущего кадра от старших битов к младшим.
	r := bitReader{b: payload}
	if r.read(2) != 2 {
		return core.VideoFormat{}, false, ErrCodec
	}
	// profile собирает младший и старший биты профиля в нормативном порядке.
	profile := r.read(1) | r.read(1)<<1
	if profile == 3 && r.read(1) != 0 {
		return core.VideoFormat{}, false, ErrCodec
	}
	if uint8(profile) != d.Profile {
		return core.VideoFormat{}, false, ErrCodec
	}
	if r.read(1) != 0 {
		r.read(3)
		if r.err != nil {
			return core.VideoFormat{}, false, r.err
		}
		return core.VideoFormat{}, false, ErrUnsupported
	}
	// key,shown и resilient определяют ветвь заголовка кадра.
	key, shown, resilient := r.read(1) == 0, r.read(1) != 0, r.read(1) != 0
	if !shown {
		return core.VideoFormat{}, false, ErrUnsupported
	}
	// size содержит фактическую геометрию, refresh — обновляемые ячейки опорных картинок.
	size := [2]uint32{}
	refresh := uint32(255)
	if key {
		if r.read(24) != 0x498342 {
			return core.VideoFormat{}, false, ErrCodec
		}
		// err сообщает о расхождении coded color_config с внешней записью.
		if err := d.vp9Color(&r); err != nil {
			return core.VideoFormat{}, false, err
		}
		size = vp9Size(&r)
	} else {
		// У показанного межкадрового кадра intra_only отсутствует; reset_frame_context присутствует без устойчивости к ошибкам.
		if !resilient {
			r.read(2)
		}
		refresh = r.read(8)
		// references сохраняет три индекса опорных картинок межкадрового кадра для frame_size_with_refs.
		var references [3]uint32
		// i выбирает одну из трёх опорных картинок; знак направления ссылки не меняет геометрию.
		for i := range references {
			references[i] = r.read(3)
			r.read(1)
		}
		// found отмечает первый размер, выбранный из опорной картинки.
		found := false
		// index выбирает опорную картинку, размер которой может наследоваться текущим кадром.
		for _, index := range references {
			if r.read(1) != 0 {
				size = d.refs[index]
				found = true
				break
			}
		}
		if !found {
			size = vp9Size(&r)
		}
		if size[0] == 0 || size[1] == 0 {
			return core.VideoFormat{}, false, ErrUnready
		}
	}
	if r.read(1) != 0 { /* render содержит отдельную display geometry, которую finite path не масштабирует. */
		render := vp9Size(&r)
		if render != size {
			return core.VideoFormat{}, false, ErrUnsupported
		}
	}
	if !key {
		r.read(1)
		if r.read(1) == 0 {
			r.read(2)
		}
	}
	if !resilient {
		r.read(2)
	}
	r.read(2)
	// err сообщает об усечении полного заголовка квантования, сегментации и плиток.
	if err := vp9HeaderTail(&r, size[0]); err != nil {
		return core.VideoFormat{}, false, err
	}
	if d.width != 0 && (d.width != size[0] || d.height != size[1]) {
		return core.VideoFormat{}, false, ErrUnsupported
	}
	if fps.Validate() != nil {
		return core.VideoFormat{}, false, ErrUnready
	}
	d.width, d.height = size[0], size[1]
	// index выбирает один из восьми опорных ячеек с установленным битом refresh.
	for index := range d.refs {
		if refresh&(1<<index) != 0 {
			d.refs[index] = size
		}
	}
	return d.videoFormat(fps), key, nil
}

// videoFormat собирает только подтверждённые поля записи и заголовка и измеренную частоту кадров.
func (d VP9Description) videoFormat(fps core.FrameRate) core.VideoFormat {
	// pixel и level сохраняют фактическую глубину и уровень записи, включая unspecified=0.
	pixel, level := "yuv420p", "unspecified"
	if d.BitDepth == 10 {
		pixel = "yuv420p10le"
	}
	if d.Level != 0 {
		level = fmt.Sprintf("%d.%d", d.Level/10, d.Level%10)
	}
	return core.VideoFormat{Codec: core.CodecVP9, CodecProfile: "profile" + strconv.Itoa(int(d.Profile)), Width: d.width, Height: d.height, FrameRate: fps, Level: level, PixelFormat: pixel, BitDepth: d.BitDepth, Color: d.Color}
}

// vp9HeaderTail проверяет петлевой фильтр, квантование, сегментацию, плитки и длину сжатого заголовка.
// Размер сжатого заголовка должен помещаться в оставшуюся AU; нагрузка плиток остаётся непрозрачным.
func vp9HeaderTail(r *bitReader, width uint32) error {
	r.read(6)
	r.read(3)
	// deltaEnabled управляет наличием loop_filter_delta_update, второй бит условный.
	deltaEnabled := r.read(1) != 0
	if deltaEnabled && r.read(1) != 0 {
		for range 6 {
			if r.read(1) != 0 {
				r.read(6)
				r.read(1)
			}
		}
	}
	r.read(8)
	for range 3 {
		if r.read(1) != 0 {
			r.read(4)
			r.read(1)
		}
	}
	if r.read(1) != 0 {
		if r.read(1) != 0 {
			for range 7 {
				if r.read(1) != 0 {
					r.read(8)
				}
			}
			if r.read(1) != 0 {
				for range 3 {
					if r.read(1) != 0 {
						r.read(8)
					}
				}
			}
		}
		if r.read(1) != 0 {
			r.read(1)
			for range 8 { /* feature выбирает ALT_Q, ALT_LF, REF_FRAME либо SKIP. */
				for feature := 0; feature < 4; feature++ {
					if r.read(1) != 0 {
						r.read([4]int{8, 6, 2, 0}[feature])
						if feature < 2 {
							r.read(1)
						}
					}
				}
			}
		}
	}
	// superblocks задаёт число 64px блоков строки, minimum/maximum — допустимые логарифмы числа столбцов плиток.
	superblocks := (width + 63) / 64
	minimum, maximum := 0, 0
	for uint32(64<<minimum) < superblocks {
		minimum++
	}
	for uint32(superblocks>>maximum) >= 4 {
		maximum++
	}
	if maximum > 0 {
		maximum--
	}
	// columns читает ограниченное унарное число дополнительных столбцов плиток.
	columns := minimum
	for columns < maximum && r.read(1) != 0 {
		columns++
	}
	if r.read(1) != 0 {
		r.read(1)
	}
	// compressed задаёт длину сжатого заголовка; выравнивание заполняет байтовую границу нулями.
	compressed := r.read(16)
	for r.p%8 != 0 {
		if r.read(1) != 0 {
			return ErrCodec
		}
	}
	if r.err != nil || compressed == 0 || uint64(compressed) >= uint64(len(r.b)-r.p/8) {
		return ErrCodec
	}
	return nil
}

// ValidateVP9Config сравнивает полный формат declared с записью; геометрия и FPS проверяются при разборе AU.
func ValidateVP9Config(headers []byte, declared core.VideoFormat) error {
	return validateVP9Config(headers, declared, media.ColorConfigProvenance{})
}

// validateVP9Config проверяет исходные заголовки против declared с отдельным доказательством внешних CICP.
// provenance разрешает только уточнение действительно unspecified осей; остальные ограничения неизменны.
func validateVP9Config(headers []byte, declared core.VideoFormat, provenance media.ColorConfigProvenance) error {
	// description содержит проверенную запись; err сообщает о повреждении или неподдержанном профиле.
	description, err := ParseVP9Config(headers)
	if err != nil {
		return err
	}
	description.width, description.height = declared.Width, declared.Height
	return CompatibleVideoConstraints(description.videoFormat(declared.FrameRate), declared, provenance)
}
