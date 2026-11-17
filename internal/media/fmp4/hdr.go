package fmp4

import (
	"math"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
)

// ParseHDRBoxes читает полный поток дочерних блоков видеозаписи без фиксированных 78 байтов.
// vp9 выбирает SmDm/CoLL FullBox; остальные поддержанные кодеки используют mdcv/clli Box.
// Привязка mdcv опирается на конкретный FFmpeg 7.1.5; универсальная ISO-конформность не заявляется.
func ParseHDRBoxes(data []byte, vp9 bool) (media.HDRMetadata, error) {
	return parseHDRBoxes(data, vp9, false)
}

// parseHDRBoxes допускает FFmpeg mdcv/clli у VP9 только для доказанного свежего результата собственного кодировщика.
// Каждый тип сохраняет собственные единицы и порядок полей; происхождение не означает переименование исходного блока.
func parseHDRBoxes(data []byte, vp9, fresh bool) (media.HDRMetadata, error) {
	// boxes содержит ограниченный набор записей контейнера; err сообщает о нарушении обрамления.
	boxes, err := children(data)
	if err != nil {
		return media.HDRMetadata{}, err
	}
	// nativeVP9 разрешает исключение FFmpeg только при доказанном происхождении результата.
	nativeVP9 := fresh && vp9
	// result хранит отдельно присутствующие блоки стандартного контейнера.
	var result media.HDRMetadata
	// item обозначает очередной дочерний блок записи контейнера.
	for _, item := range boxes {
		switch item.kind {
		case "mdcv", "SmDm":
			if result.MDCV.Present || (item.kind == "SmDm") != vp9 && !nativeVP9 {
				return result, ErrInvalid
			}
			// body содержит полезные поля без версии и флагов FullBox VP9.
			body := item.data
			if item.kind == "SmDm" {
				if len(body) != 28 || be.Uint32(body) != 0 {
					return result, ErrInvalid
				}
				body = body[4:]
			}
			result.MDCV, err = codec.DecodeHDRMastering(body, item.kind == "SmDm", media.HDRSourceContainer)
			if err != nil {
				return result, translateCodecError(err)
			}
		case "clli", "CoLL":
			if result.CLL.Present || (item.kind == "CoLL") != vp9 && !nativeVP9 {
				return result, ErrInvalid
			}
			// body содержит два uint16 после снятия VP9 FullBox.
			body := item.data
			if item.kind == "CoLL" {
				if len(body) != 8 || be.Uint32(body) != 0 {
					return result, ErrInvalid
				}
				body = body[4:]
			}
			result.CLL, err = codec.DecodeHDRLight(body, media.HDRSourceContainer)
			if err != nil {
				return result, translateCodecError(err)
			}
		}
	}
	return result, nil
}

// projectHDRComponent сохраняет неизвестный компонент только в идентичной исходной сетке.
// Известный компонент направленно округляется и должен помещаться в соответствующее поле блока.
func projectHDRComponent(value media.HDRValue, target media.HDRGrid, maximum uint64) (uint64, error) {
	if !value.Known {
		if value.Grid != target || value.Raw > maximum {
			return 0, media.ErrHDRUnavailable
		}
		return value.Raw, nil
	}
	// projected содержит единственное округление в сетку конкретного выходного блока.
	// err сообщает о невозможном округлении в сетку целевого поля.
	projected, err := value.Project(target)
	if err != nil || projected > maximum {
		return 0, media.ErrHDRUnavailable
	}
	return projected, nil
}

// marshalHDRBoxes строит направленные проекции только присутствующих блоков.
// Неизвестные компоненты не заменяются нулями и не отбрасываются при неполном представлении.
func marshalHDRBoxes(metadata media.HDRMetadata, vp9 bool) ([]byte, error) {
	// err сообщает о некорректном снимке до построения контейнерных блоков.
	if err := metadata.Validate(); err != nil {
		return nil, err
	}
	// result накапливает независимые блоки без изменения авторитетного источника runtime сведений.
	var result []byte
	if metadata.MDCV.Present {
		// body содержит 24 байта поля дисплея, values — копию координат RGB, белой точки и границ яркости.
		body := make([]byte, 24)
		values := metadata.MDCV.Values()
		// index выбирает позицию поля, value содержит исходный компонент неизменяемого снимка.
		for index, value := range values {
			// grid и maximum задают допустимую сетку и целочисленную ёмкость конкретного поля.
			grid, maximum := media.HDRGridSEIChromaticity, uint64(math.MaxUint16)
			if index >= 8 {
				grid, maximum = media.HDRGridSEILuminance, math.MaxUint32
			}
			if vp9 {
				switch {
				case index < 8:
					grid = media.HDRGridBinaryChromaticity
				case index == 8:
					grid = media.HDRGridBinaryMaxLuminance
				case index == 9:
					grid = media.HDRGridBinaryMinLuminance
				}
			}
			// projected содержит числовое значение целевого поля; err сообщает о невозможной проекции.
			projected, err := projectHDRComponent(value, grid, maximum)
			if err != nil {
				return nil, err
			}
			// position преобразует внутренний RGB в FFmpeg mdcv GBR, сохраняя SmDm RGB.
			position := index
			if !vp9 && index < 6 {
				position = [6]int{4, 5, 0, 1, 2, 3}[index]
			}
			if index < 8 {
				be.PutUint16(body[position*2:], uint16(projected))
			} else {
				be.PutUint32(body[16+(index-8)*4:], uint32(projected))
			}
		}
		if vp9 {
			result = append(result, full("SmDm", 0, 0, body)...)
		} else {
			result = append(result, box("mdcv", body)...)
		}
	}
	if metadata.CLL.Present {
		// body хранит две целые границы CLL выбранного выходного блока.
		body := make([]byte, 4)
		// index выбирает позицию поля 0/1, value содержит очередной компонент MaxCLL либо MaxFALL.
		for index, value := range []media.HDRValue{metadata.CLL.MaxCLL, metadata.CLL.MaxFALL} {
			// projected содержит проверенное 16-битное значение без подмены неопределённого нуля.
			// err сообщает о непредставимой границе CLL в uint16 без подстановки значения.
			projected, err := projectHDRComponent(value, media.HDRGridInteger, math.MaxUint16)
			if err != nil {
				return nil, err
			}
			be.PutUint16(body[2*index:], uint16(projected))
		}
		if vp9 {
			result = append(result, full("CoLL", 0, 0, body)...)
		} else {
			result = append(result, box("clli", body)...)
		}
	}
	// decoded проверяет известность после реального кодирования, включая неопределённые диапазоны SEI.
	// err сообщает об изменении синтаксиса либо известности после сериализации.
	decoded, err := ParseHDRBoxes(result, vp9)
	if err != nil {
		return nil, err
	}
	if err = codec.MatchHDRProjection(metadata, decoded); err != nil {
		return nil, media.ErrHDRUnavailable
	}
	return result, nil
}

// reconstructHDR восстанавливает сведения кодека и контейнера во всех ветвях, включая собственный UUID.
// AV1 OBU задают авторитетный источник блока: противоречивый сторонний дескриптор не исправляется.
func reconstructHDR(config media.CodecConfig, description TrackDescription) (media.CodecConfig, error) {
	// err сохраняет отказ внешнего nclx до выбора источника статических HDR-блоков.
	var err error
	config, err = reconstructColor(config, description)
	if err != nil {
		return config, err
	}
	// info содержит копию конфигурации времени выполнения без добавления HDR в сохраняемый UUID.
	info := config.Info()
	if info.Track != media.TrackVideo {
		return config, nil
	}
	if info.Video.Codec != core.CodecHEVC && info.Video.Color.Transfer != "smpte2084" && info.Video.Color.Transfer != "arib-std-b67" && description.HDR == (media.HDRMetadata{}) {
		return config, nil
	}
	// inband содержит статические сведения av1C/hvcC; err сообщает о повреждении сигнализации кодека.
	inband, err := codec.HDRFromConfig(info.Video.Codec, description.Headers)
	if err != nil {
		return config, translateCodecError(err)
	}

	if inband.MDCV.Present && description.HDR.MDCV.Present || inband.CLL.Present && description.HDR.CLL.Present {
		// overlap выбирает только одновременно присутствующие внешние блоки для направленного сравнения.
		overlap := description.HDR
		if !inband.MDCV.Present {
			overlap.MDCV = media.HDRMasteringDisplay{}
		}
		if !inband.CLL.Present {
			overlap.CLL = media.HDRContentLight{}
		}
		if err = codec.MatchHDRProjection(inband, overlap); err != nil {
			return config, err
		}
	}
	if !inband.MDCV.Present {
		inband.MDCV = description.HDR.MDCV
	}
	if !inband.CLL.Present {
		inband.CLL = description.HDR.CLL
	}
	inband.ColorConfig = info.HDR.ColorConfig
	info.HDR = inband
	return media.NewCodecConfig(info, description.Headers)
}

// normalizeFreshHDRDescription исправляет только дескрипторы метаданных доказанного свежего Encoding5.
// Проекции source проверяются до замены; исходный AU и заголовок кодека остаются неизменными.
func normalizeFreshHDRDescription(description *TrackDescription, source media.HDRMetadata) error {
	// inband содержит только статические сведения конфигурации кодека, err сохраняет синтаксический отказ.
	inband, err := codec.HDRFromConfig(description.Codec, description.Headers)
	if err != nil {
		return err
	}
	// effective содержит авторитетные сведения потока либо подтверждённый общий снимок source.
	effective, err := codec.ReconcileFreshHDR(source, description.HDR, inband)
	if err != nil {
		return err
	}
	// encoded строит дескриптор как проекцию authority в нужную контейнерную сетку.
	encoded, err := marshalHDRBoxes(effective, description.Codec == core.CodecVP9)
	if err != nil {
		return err
	}
	description.HDR, err = ParseHDRBoxes(encoded, description.Codec == core.CodecVP9)
	description.HDR.ColorConfig = effective.ColorConfig
	return err
}
