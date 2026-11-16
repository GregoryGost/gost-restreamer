package rtmp

import (
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/bluenviron/gortmplib/pkg/amf0"
)

// outboundColorInfo создаёт полный объект Metadata до первой записи затронутого AU.
// Частично известная секция остаётся только в доказанном in-band канале; контейнерные сведения
// без такого канала не теряются молча и возвращают ErrHDRUnavailable до сетевой записи.
func outboundColorInfo(packet media.Packet, independent bool) (amf0.Data, error) {
	// info содержит immutable snapshot выбранного источника; result собирает заменяющий объект.
	info := packet.Config().Info()
	result := amf0.Object{}
	if hdrVideo(info.Video) || info.HDR.ColorConfig != (media.ColorConfigProvenance{}) {
		// config содержит реальные depth/CICP; tables задаёт независимые таблицы трёх индексов.
		config := amf0.Object{{Key: "bitDepth", Value: float64(info.Video.BitDepth)}}
		tables := colorTables()
		// index и field связывают цветовой индекс с фактическим именем в формате.
		for index, field := range []struct {
			// key задаёт сетевое имя, value — фактическое значение coded характеристики.
			key, value string
		}{{"colorPrimaries", info.Video.Color.Primaries}, {"transferCharacteristics", info.Video.Color.Transfer}, {"matrixCoefficients", info.Video.Color.Matrix}} {
			// code и err проверяют представимость actual CICP без desired defaults.
			code, err := colorCode(field.value, tables[index])
			if err != nil {
				return nil, err
			}
			config = append(config, amf0.ObjectEntry{Key: field.key, Value: float64(code)})
		}
		result = append(result, amf0.ObjectEntry{Key: "colorConfig", Value: config})
	}
	// inband и state используются только при необходимости доказать перенос неполной секции.
	var inband media.HDRMetadata
	var state codec.HDRState
	if info.HDR.MDCV.Present && !info.HDR.MDCV.FullyKnown() || info.HDR.CLL.Present && !info.HDR.CLL.FullyKnown() {
		// err сохраняет результат проверки именно текущего независимого AU и config headers.
		var err error
		inband, err = state.Observe(info.Video.Codec, packet.Config().Headers(), packet.Payload(), independent)
		if err != nil {
			return nil, err
		}
	}
	if info.HDR.MDCV.Present {
		if info.HDR.MDCV.FullyKnown() {
			// block и err содержат полный projected MDCV либо отказ представимости порта.
			block, err := outboundMastering(info.HDR.MDCV)
			if err != nil {
				return nil, err
			}
			result = append(result, amf0.ObjectEntry{Key: "hdrMdcv", Value: block})
		} else {
			// known отмечает хотя бы один известный компонент, который нельзя потерять.
			known := false
			// value перебирает все десять компонентов исходного блока дисплея.
			for _, value := range info.HDR.MDCV.Values() {
				known = known || value.Known
			}
			if known {
				if !inband.MDCV.Present || codec.MatchHDRProjection(media.HDRMetadata{MDCV: info.HDR.MDCV}, media.HDRMetadata{MDCV: inband.MDCV}) != nil {
					return nil, media.ErrHDRUnavailable
				}
			}
		}
	}
	if info.HDR.CLL.Present {
		if info.HDR.CLL.FullyKnown() {
			// maximum и err проверяют MaxCLL, average проверяет MaxFALL в диапазоне ERTMP.
			maximum, err := projectAMFValue(info.HDR.CLL.MaxCLL, 0.0001, 10000, false)
			if err != nil {
				return nil, err
			}
			average, err := projectAMFValue(info.HDR.CLL.MaxFALL, 0.0001, 10000, false)
			if err != nil {
				return nil, err
			}
			result = append(result, amf0.ObjectEntry{Key: "hdrCll", Value: amf0.Object{{Key: "maxCLL", Value: maximum}, {Key: "maxFall", Value: average}}})
		} else if info.HDR.CLL.MaxCLL.Known || info.HDR.CLL.MaxFALL.Known {
			if !inband.CLL.Present || codec.MatchHDRProjection(media.HDRMetadata{CLL: info.HDR.CLL}, media.HDRMetadata{CLL: inband.CLL}) != nil {
				return nil, media.ErrHDRUnavailable
			}
		}
	}
	if len(result) == 0 {
		return amf0.Data{"colorInfo", amf0.Undefined{}}, nil
	}
	return amf0.Data{"colorInfo", result}, nil
}

// outboundMastering проецирует все поля MDCV непосредственно из выбранной authority в AMF.
// Несуществующие, неизвестные или выходящие за границы ERTMP компоненты не заменяются defaults.
func outboundMastering(mastering media.HDRMasteringDisplay) (amf0.Object, error) {
	// values содержит исходные RGBxy/white/max/min; result собирает полный AMF-объект.
	values := mastering.Values()
	result := amf0.Object{}
	// index и name связывают исходный компонент с его нормативным сетевым полем.
	for index, name := range []string{"redX", "redY", "greenX", "greenY", "blueX", "blueY", "whitePointX", "whitePointY", "maxLuminance", "minLuminance"} {
		// minimum и maximum задают физические пределы координат либо яркости.
		minimum, maximum := 0.0001, 0.7400
		if index < 8 && index%2 == 1 {
			maximum = 0.8400
		} else if index == 8 {
			minimum, maximum = 5, 10000
		} else if index == 9 {
			maximum = 5
		}
		// number и err содержат прямую проекцию исходного компонента в сетку выбранного порта.
		number, err := projectAMFValue(values[index], minimum, maximum, index < 8)
		if err != nil {
			return nil, err
		}
		result = append(result, amf0.ObjectEntry{Key: name, Value: number})
	}
	return result, nil
}

// writeColorInfoLocked записывает уже проверенный Metadata под текущим сериализатором.
// Полное тело готовится до любой записи, а транспортные ошибки завершают соединение у вызывающего.
func (c *OutboundConn) writeColorInfoLocked(timestamp uint32, body []byte) error {
	// fourCC выбирает кодек выбранной конфигурации, prefix задаёт Metadata packet type4.
	fourCC := outboundFourCC(c.video.Info().Video)
	prefix := []byte{0x84, fourCC[0], fourCC[1], fourCC[2], fourCC[3]}
	return c.writeFramed(6, 9, c.streamID.Load(), timestamp, append(prefix, body...))
}
