package fallback

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/json"
	"fmt"
	"io"
	"math/big"
	"os"
	"strconv"
	"strings"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
)

// isHDRVideo различает PQ/HLG по фактической либо желаемой передаточной функции v.
func isHDRVideo(v core.VideoFormat) bool {
	return v.Color.Transfer == "smpte2084" || v.Color.Transfer == "arib-std-b67"
}

// needsHDRSource включает единственную проверку сжатых HDR-сведений лишь для нового HDR-варианта.
// profiles задаёт весь обязательный комплект поколения; SDR не требует этого прохода.
func needsHDRSource(profiles []core.RequiredProfile) bool {
	// required обозначает очередной обязательный вариант поколения.
	for _, required := range profiles {
		if isHDRVideo(required.Profile.Format.Video) {
			return true
		}
	}
	return false
}

// inspectHDRSource требует у inspector ограниченную проверку всего HDR-источника перед первым вариантом.
// file остаётся открытым у менеджера; отсутствие порта не подменяется непроверенным неизменным снимком.
func inspectHDRSource(ctx context.Context, inspector Inspection, file *os.File, source Source) (Source, error) {
	// hdrInspector получает необязательный порт, ok подтверждает его реализацию инспектором.
	hdrInspector, ok := inspector.(interface {
		SourceHDR(context.Context, *os.File, Source) (Source, error)
	})
	if !ok {
		return Source{}, fmt.Errorf("%w: недоступна проверка HDR-источника", core.ErrNotReady)
	}
	return hdrInspector.SourceHDR(ctx, file, source)
}

// sourceHDRSide принимает точные рациональные значения дополнительных сведений о дорожках и пакетах ffprobe.
// Поля вне MDCV/CLL не интерпретируются и не становятся желаемыми значениями по умолчанию.
type sourceHDRSide struct {
	// Type различает сведения о дисплее и яркости контента.
	Type string `json:"side_data_type"`
	// RedX/RedY, GreenX/GreenY и BlueX/BlueY содержат исходные координаты в порядке RGB.
	RedX   string `json:"red_x"`
	RedY   string `json:"red_y"`
	GreenX string `json:"green_x"`
	GreenY string `json:"green_y"`
	BlueX  string `json:"blue_x"`
	BlueY  string `json:"blue_y"`
	// WhiteX/WhiteY сохраняют координаты белой точки.
	WhiteX string `json:"white_point_x"`
	WhiteY string `json:"white_point_y"`
	// Minimum/Maximum содержат точную яркость дисплея в кд/м².
	Minimum string `json:"min_luminance"`
	Maximum string `json:"max_luminance"`
	// MaxCLL/MaxFALL сохраняют целые исходные границы без создания известных значений из отсутствия.
	MaxCLL  *uint64 `json:"max_content"`
	MaxFALL *uint64 `json:"max_average"`
}

// sourceHDRDocument содержит ограниченный дамп сжатых видеопакетов и исходных дескрипторов дорожки.
type sourceHDRDocument struct {
	// Streams содержит ровно одну выбранную видеодорожку с заголовками кодека и контейнерными метаданными.
	Streams []struct {
		// probeStream сохраняет фактические заголовки кодека и формат ffprobe.
		probeStream
		// Side содержит исходные стандартные дескрипторы контейнера.
		Side []sourceHDRSide `json:"side_data_list"`
	} `json:"streams"`
	// Packets содержит каждый сжатый AU без декодирования пикселей.
	Packets []struct {
		// Data сохраняет шестнадцатеричный дамп исходного AU, Size — его объявленный размер в байтах.
		Data string `json:"data"`
		Size string `json:"size"`
		// Flags содержит сообщённый демультиплексором признак независимого пакета.
		Flags string `json:"flags"`
		// Side содержит метаданные пакета, если контейнер передал их на этой границе.
		Side []sourceHDRSide `json:"side_data_list"`
	} `json:"packets"`
}

// SourceHDR проверяет единый неизменный снимок HDR источника source на всей сжатой видеодорожке MOV/MP4.
// ctx, MaxJSONBytes, MaxPackets и MaxSampleBytes ограничивают процесс и удержанные данные;
// ffprobe не декодирует все пиксели ради измерения максимальной яркости. Меняющиеся или поздние
// сведения не публикуются.
func (p Prober) SourceHDR(ctx context.Context, file *os.File, source Source) (Source, error) {
	if file == nil || source.Kind != core.AssetVideo || source.Demuxer != "mov" || source.Format.Video == nil || !isHDRVideo(*source.Format.Video) || p.Limits.Validate() != nil || p.Runner == nil || p.Binary == "" {
		return Source{}, fmt.Errorf("%w: HDR-подготовка требует MOV/MP4 видео", core.ErrNotReady)
	}
	// err сообщает об отказе перемотки исходного дескриптора до отдельного прохода сжатых данных.
	if _, err := file.Seek(0, io.SeekStart); err != nil {
		return Source{}, err
	}
	// output принимает ограниченный по объёму JSON с исходными байтами всех видеопакетов.
	var output bytes.Buffer
	// command выбирает только видеодорожку, сохраняя заголовки, содержимое пакетов и дескрипторы без декодирования.
	command := Command{Binary: p.Binary, Args: []string{"-v", "error", "-protocol_whitelist", "fd,pipe", "-format_whitelist", "mov", "-fd", "3", "-i", "fd:", "-select_streams", "v:0", "-show_streams", "-show_packets", "-show_data", "-of", "json"}}
	// err сохраняет исходный отказ процесса или превышение бюджета вывода.
	if _, err := p.Runner.Run(ctx, command, file, &boundedWriter{w: &output, remaining: p.Limits.MaxJSONBytes}); err != nil {
		return Source{}, err
	}
	// document содержит ограниченное число исходных AU выбранной видеодорожки.
	var document sourceHDRDocument
	// err сообщает об усечённом либо повреждённом JSON.
	if err := json.Unmarshal(output.Bytes(), &document); err != nil {
		return Source{}, err
	}
	if len(document.Streams) != 1 || len(document.Packets) == 0 || uint64(len(document.Packets)) > p.Limits.MaxPackets {
		return Source{}, ErrLimit
	}
	// stream сохраняет единственное описание видеодорожки с исходными заголовками кодека.
	stream := document.Streams[0]
	// measured повторно подтверждает данные ffprobe реальной записью кодека и nclx того же файла.
	// description сохраняет исходные байты и происхождение; err сообщает об отказе проверки.
	measured, description, err := p.sourceVideoFormat(ctx, file, stream.probeStream)
	if err != nil {
		return Source{}, err
	}
	if err = codec.CompatibleVideoActual(measured, *source.Format.Video); err != nil {
		return Source{}, err
	}
	// container содержит стандартные дескрипторы с точными исходными сетками и внешний цвет файла.
	container, err := sourceHDRMetadata(stream.Side, source.Format.Video.Codec)
	if err != nil {
		return Source{}, err
	}
	container.ColorConfig = description.provenance
	// config предоставляет Inspector подтверждённые параметры и исходные заголовки без желаемых значений.
	// Единица времени и идентификаторы служат локальному синтаксическому анализу; времена не измеряются здесь.
	config, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: 1, Revision: 1, TimeBase: media.TimeBase{Numerator: 1, Denominator: 1000}, Video: measured, HDR: media.HDRMetadata{ColorConfig: description.provenance}}, description.headers)
	if err != nil {
		return Source{}, err
	}
	// inspector сохраняет состояние разборщика и действие метаданных кодека между AU; snapshot фиксирует первый полный снимок.
	var inspector codec.Inspector
	var snapshot media.HDRMetadata
	// index и packet задают положение AU, необходимое для проверки неизменности сведений на всём источнике source.
	for index, packet := range document.Packets {
		if err = ctx.Err(); err != nil {
			return Source{}, err
		}
		// size ограничивает одну сжатую AU до восстановления байтов из дампа;
		// parseErr сообщает о некорректной записи размера.
		size, parseErr := strconv.ParseUint(packet.Size, 10, 32)
		if parseErr != nil || size == 0 || size > p.Limits.Container.MaxSampleBytes {
			return Source{}, ErrLimit
		}
		// payload сохраняет исходные сжатые байты; parseErr сообщает о неполном дампе.
		payload, parseErr := probeExtraData(packet.Data, uint32(size))
		if parseErr != nil {
			return Source{}, parseErr
		}
		// compressed оборачивает исходные байты только для проверки кодека; нулевая длительность не выдумывает CFR.
		compressed, parseErr := media.NewPacket(media.PacketInfo{}, config, payload)
		if parseErr != nil {
			return Source{}, parseErr
		}
		// current содержит только действительные внутрипоточные сведения, а не внешний Info.HDR.
		// parseErr сохраняет нарушение известных ограничений кодека, независимости или срока действия метаданных кодека.
		_, current, parseErr := inspector.InspectMetadata(compressed)
		if parseErr != nil {
			return Source{}, parseErr
		}
		// external содержит только дескрипторы этого пакета; отсутствие не сбрасывает метаданные дорожки.
		external, parseErr := sourceHDRMetadata(packet.Side, source.Format.Video.Codec)
		if parseErr != nil {
			return Source{}, parseErr
		}
		if external.MDCV.Present {
			container.MDCV = external.MDCV
		}
		if external.CLL.Present {
			container.CLL = external.CLL
		}
		// effective выбирает авторитетные внутрипоточные сведения каждого присутствующего блока, сохраняя сведения, присутствующие лишь в контейнере.
		effective, parseErr := combineSourceHDR(current, container)
		if parseErr != nil {
			return Source{}, parseErr
		}
		if index == 0 {
			snapshot = effective
		} else if effective != snapshot {
			return Source{}, fmt.Errorf("%w: HDR-сведения источника меняются после первого кадра", core.ErrNotReady)
		}
	}
	source.HDR, source.HDRVerified = snapshot, true
	return source, nil
}

// combineSourceHDR выбирает авторитетную сигнализацию кодека и проверяет её направленную проекцию в container.
// Отсутствующие внутрипоточные блоки сохраняют исходный стандартный дескриптор без выдуманных сведений.
func combineSourceHDR(inband, container media.HDRMetadata) (media.HDRMetadata, error) {
	// compared исключает из сравнения блоки, присутствующие лишь в контейнере: для них нет второго источника.
	compared := container
	if !inband.MDCV.Present {
		compared.MDCV = media.HDRMasteringDisplay{}
	}
	if !inband.CLL.Present {
		compared.CLL = media.HDRContentLight{}
	}
	// err сообщает о противоречивом внешнем дескрипторе.
	if err := codec.MatchHDRProjection(inband, compared); err != nil {
		return media.HDRMetadata{}, err
	}
	if !inband.MDCV.Present {
		inband.MDCV = container.MDCV
	}
	if !inband.CLL.Present {
		inband.CLL = container.CLL
	}
	inband.ColorConfig = container.ColorConfig
	return inband, inband.Validate()
}

// sourceHDRMetadata восстанавливает исходные числовые сетки дополнительных сведений ffprobe для kind.
// Рациональное число обязано точно попадать в стандартную сетку; округление не заменяет исходные сведения.
func sourceHDRMetadata(items []sourceHDRSide, kind core.Codec) (media.HDRMetadata, error) {
	// result содержит независимые MDCV и дескрипторы CLL; grid/maxGrid/minGrid задают сетки kind.
	var result media.HDRMetadata
	grid, maxGrid, minGrid := media.HDRGridSEIChromaticity, media.HDRGridSEILuminance, media.HDRGridSEILuminance
	if kind == core.CodecAV1 || kind == core.CodecVP9 {
		grid, maxGrid, minGrid = media.HDRGridBinaryChromaticity, media.HDRGridBinaryMaxLuminance, media.HDRGridBinaryMinLuminance
	}
	// item обозначает очередной дескриптор; посторонние типы дополнительных сведений не интерпретируются.
	for _, item := range items {
		switch item.Type {
		case "Mastering display metadata":
			// values содержит координаты RGB, координаты белой точки и яркости в каноническом порядке.
			var values [10]media.HDRValue
			// index и text обозначают очередное точное рациональное значение.
			for index, text := range []string{item.RedX, item.RedY, item.GreenX, item.GreenY, item.BlueX, item.BlueY, item.WhiteX, item.WhiteY, item.Maximum, item.Minimum} {
				// selected задаёт сетку конкретной координаты или яркости.
				selected := grid
				switch index {
				case 8:
					selected = maxGrid
				case 9:
					selected = minGrid
				}
				// err сообщает о числе, которое нельзя восстановить без потери исходной точности.
				var err error
				values[index], err = sourceHDRValue(text, selected)
				if err != nil {
					return result, err
				}
			}
			// mastering сохраняет нормативную известность компонентов через общий декодер кодека,
			// включая неизвестные кодовые слова; err сообщает о непредставимой числовой сетке.
			mastering, err := sourceHDRMastering(values)
			if err != nil {
				return result, err
			}
			result.MDCV = mastering
		case "Content light level metadata":
			if item.MaxCLL == nil || item.MaxFALL == nil || *item.MaxCLL > 65535 || *item.MaxFALL > 65535 {
				return result, media.ErrHDRUnavailable
			}
			result.CLL = media.HDRContentLight{Present: true, Source: media.HDRSourceContainer,
				MaxCLL:  media.HDRValue{Raw: *item.MaxCLL, Grid: media.HDRGridInteger, Known: *item.MaxCLL != 0},
				MaxFALL: media.HDRValue{Raw: *item.MaxFALL, Grid: media.HDRGridInteger, Known: *item.MaxFALL != 0}}
		}
	}
	return result, result.Validate()
}

// sourceHDRMastering передаёт точные кодовые слова общему декодеру MDCV без повторения правил известности.
// values имеют порядок RGBxy, координаты белой точки, max/min; смешанные исходные сетки не поддерживаются.
func sourceHDRMastering(values [10]media.HDRValue) (media.HDRMasteringDisplay, error) {
	// binaryGrid выбирает исходную двоичную сетку AV1/SmDm, а не тип выходного кодека.
	binaryGrid := values[0].Grid == media.HDRGridBinaryChromaticity
	// body сохраняет исходные кодовые слова в 24 байтах стандартного сообщения.
	var body [24]byte
	// index и value выбирают компонент дисплея и его неизменённое целое исходное значение.
	for index, value := range values {
		// expected задаёт единственную сетку соответствующего поля исходного блока.
		expected := media.HDRGridSEIChromaticity
		if index >= 8 {
			expected = media.HDRGridSEILuminance
		}
		if binaryGrid {
			expected = media.HDRGridBinaryChromaticity
			switch index {
			case 8:
				expected = media.HDRGridBinaryMaxLuminance
			case 9:
				expected = media.HDRGridBinaryMinLuminance
			}
		}
		if value.Grid != expected || index < 8 && value.Raw > 65535 || index >= 8 && value.Raw > 4294967295 {
			return media.HDRMasteringDisplay{}, media.ErrHDRUnavailable
		}
		if index < 8 {
			// position переводит RGB в исходный SEI GBR только для десятичной сетки.
			position := index
			if !binaryGrid && index < 6 {
				position = [6]int{4, 5, 0, 1, 2, 3}[index]
			}
			binary.BigEndian.PutUint16(body[position*2:], uint16(value.Raw))
		} else {
			binary.BigEndian.PutUint32(body[16+(index-8)*4:], uint32(value.Raw))
		}
	}
	return codec.DecodeHDRMastering(body[:], binaryGrid, media.HDRSourceContainer)
}

// sourceHDRValue восстанавливает точное кодовое слово text в grid, не округляя и не применяя значений по умолчанию.
func sourceHDRValue(text string, grid media.HDRGrid) (media.HDRValue, error) {
	// denominator сохраняет исходную запись сетки ffprobe до сокращения рационального числа;
	// hasFraction различает явный знаменатель и целое число.
	// MOV mdcv использует десятичную сетку даже при AV1; SmDm и OBU используют двоичную.
	_, denominator, hasFraction := strings.Cut(text, "/")
	if hasFraction {
		switch grid {
		case media.HDRGridSEIChromaticity, media.HDRGridBinaryChromaticity:
			switch denominator {
			case "50000":
				grid = media.HDRGridSEIChromaticity
			case "65536":
				grid = media.HDRGridBinaryChromaticity
			}
		case media.HDRGridSEILuminance, media.HDRGridBinaryMaxLuminance, media.HDRGridBinaryMinLuminance:
			switch denominator {
			case "10000":
				grid = media.HDRGridSEILuminance
			case "256":
				grid = media.HDRGridBinaryMaxLuminance
			case "16384":
				grid = media.HDRGridBinaryMinLuminance
			}
		}
	}
	// value содержит точное число ffprobe; ok подтверждает корректную рациональную запись.
	value, ok := new(big.Rat).SetString(text)
	if !ok || value.Sign() < 0 {
		return media.HDRValue{}, media.ErrHDRUnavailable
	}
	// unit задаёт величину одного шага выбранной сетки без дублирования таблицы её знаменателей.
	// err сообщает о неподдержанной или неопределённой исходной сетке HDR.
	unit, err := (media.HDRValue{Raw: 1, Grid: grid, Known: true}).Rational()
	if err != nil {
		return media.HDRValue{}, err
	}
	value.Quo(value, unit)
	if !value.IsInt() || !value.Num().IsUint64() {
		return media.HDRValue{}, media.ErrHDRUnavailable
	}
	return media.HDRValue{Raw: value.Num().Uint64(), Grid: grid, Known: true}, nil
}
