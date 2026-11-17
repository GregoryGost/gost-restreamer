package fmp4

import (
	"strconv"
	"strings"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
)

// marshalColor кодирует цвет c в nclx с тремя независимыми таблицами CICP.
// Неизвестные числовые коды сохраняются точно; текстовые имена без кода отклоняются.
func marshalColor(c core.ColorInfo) ([]byte, error) {
	// fields сопоставляет каждую характеристику с её собственной таблицей, исключая совпадение
	// одинаковых числовых кодов из разных разделов CICP.
	fields := []struct {
		// value содержит каноническое имя либо сохранённый числовой код.
		value string
		// names содержит известные только для этой характеристики коды и имена.
		names map[string]uint16
	}{
		{c.Primaries, map[string]uint16{"bt709": 1, "unspecified": 2, "bt470m": 4, "bt470bg": 5, "smpte170m": 6, "smpte240m": 7, "film": 8, "bt2020": 9}},
		{c.Transfer, map[string]uint16{"bt709": 1, "unspecified": 2, "gamma22": 4, "gamma28": 5, "bt470m": 4, "bt470bg": 5, "smpte170m": 6, "smpte240m": 7, "linear": 8, "iec61966-2-1": 13, "bt2020-10": 14, "bt2020-12": 15, "smpte2084": 16, "arib-std-b67": 18}},
		{c.Matrix, map[string]uint16{"gbr": 0, "bt709": 1, "unspecified": 2, "fcc": 4, "bt470bg": 5, "smpte170m": 6, "smpte240m": 7, "bt2020nc": 9, "bt2020c": 10}},
	}
	// out содержит четыре байта типа nclx, три uint16 и бит полного диапазона.
	out := make([]byte, 11)
	copy(out, "nclx")
	// i выбирает поле CICP в выходе; field содержит значение и применимую таблицу.
	for i, field := range fields {
		// code содержит числовое значение, known отмечает наличие канонического имени в таблице.
		code, known := field.names[field.value]
		if !known {
			// raw хранит числовую часть известной формы iso- либо cicp: без изменения исходной строки.
			raw := strings.TrimPrefix(strings.TrimPrefix(field.value, "iso-"), "cicp:")
			// parsed содержит восьмибитный код заголовка кодека; err сообщает о неверной записи.
			parsed, err := strconv.ParseUint(raw, 10, 8)
			if raw == field.value || err != nil || strconv.FormatUint(parsed, 10) != raw {
				return nil, ErrInvalid
			}
			code = uint16(parsed)
		}
		be.PutUint16(out[4+2*i:], code)
	}
	switch c.Range {
	case "full", "pc":
		out[10] = 0x80
	case "limited", "tv":
	default:
		return nil, ErrInvalid
	}
	return out, nil
}

// readColor находит необязательный единственный colr/nclx среди childrenData видеозаписи.
// Возвращает отдельную копию его тела; неиспользуемые биты и неподдержанный тип отвергаются.
func readColor(childrenData []byte) ([]byte, error) {
	// boxes содержит проверенные дочерние блоки; err сообщает о нарушении их обрамления.
	boxes, err := children(childrenData)
	if err != nil {
		return nil, err
	}
	// color хранит единственное найденное тело nclx, nil отмечает отсутствие дескриптора.
	var color []byte
	// b содержит очередной дочерний блок видеозаписи.
	for _, b := range boxes {
		if b.kind == "colr" {
			if color != nil || len(b.data) != 11 || string(b.data[:4]) != "nclx" || b.data[10]&0x7f != 0 {
				return nil, ErrInvalid
			}
			color = append([]byte(nil), b.data...)
		}
	}
	return color, nil
}

// reconstructColor восстанавливает проверенное nclx в Info времени выполнения, в том числе после чтения UUID.
// Прежний SDR AV1/H.264 остаётся на строгом пути. Внешний диапазон не уточняет кодек:
// он обязан совпадать, как и все известные CICP. При отсутствии nclx прежний признак происхождения не наследуется.
func reconstructColor(config media.CodecConfig, description TrackDescription) (media.CodecConfig, error) {
	// info содержит копию runtime описания; color сохраняет фактическое тело внешнего nclx.
	info, color := config.Info(), description.Color
	if info.Track != media.TrackVideo {
		return config, nil
	}
	info.HDR.ColorConfig = media.ColorConfigProvenance{}
	if color == nil {
		return media.NewCodecConfig(info, config.Headers())
	}
	if len(color) != 11 || string(color[:4]) != "nclx" || color[10]&0x7f != 0 || be.Uint16(color[4:]) > 255 || be.Uint16(color[6:]) > 255 || be.Uint16(color[8:]) > 255 {
		return media.CodecConfig{}, ErrInvalid
	}
	// external берёт CICP и диапазон исключительно из прочитанного дескриптора.
	external := info.Video
	external.Color = codec.CICPColor(uint8(be.Uint16(color[4:])), uint8(be.Uint16(color[6:])), uint8(be.Uint16(color[8:])), color[10]&128 != 0)
	if !codec.UsesExternalColor(info.Video) && !codec.UsesExternalColor(external) {
		return media.NewCodecConfig(info, config.Headers())
	}
	// provenance подтверждает каждую из трёх обязательных осей nclx без копии самих значений.
	provenance := media.ColorConfigProvenance{Source: media.HDRSourceContainer, Primaries: true, Transfer: true, Matrix: true}
	// err отклоняет противоречие кэшированному actual/UUID до замены его неизвестных осей.
	if err := codec.CompatibleVideoConstraints(info.Video, external, provenance); err != nil {
		return media.CodecConfig{}, translateCodecError(err)
	}
	info.Video, info.HDR.ColorConfig = external, provenance
	// result сохраняет исходные headers; err отделяет структурный отказ от противоречия ограничений заголовков кодека.
	result, err := media.NewCodecConfig(info, config.Headers())
	if err != nil {
		return result, err
	}
	if err = codec.ValidateVideoConfig(result); err != nil {
		return media.CodecConfig{}, translateCodecError(err)
	}
	return result, nil
}
