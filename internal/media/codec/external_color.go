package codec

import (
	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
)

// UsesExternalColor ограничивает уточнение CICP новыми кодеками и HDR.
// Прежние SDR AV1/H.264 сохраняют строгое сравнение исходных цветовых полей.
func UsesExternalColor(video core.VideoFormat) bool {
	return video.Codec == core.CodecHEVC || video.Codec == core.CodecVP9 || video.Color.Transfer == "smpte2084" || video.Color.Transfer == "arib-std-b67"
}

// CICPColor восстанавливает три независимых восьмибитных индекса и диапазон внешнего дескриптора.
// Зарезервированные коды сохраняются численно и никогда не превращаются в unspecified.
func CICPColor(primaries, transfer, matrix uint8, full bool) core.ColorInfo {
	// color содержит именованные индексы; диапазон берётся из явного бита дескриптора.
	color := core.ColorInfo{Primaries: primaryName(primaries, "iso-"), Transfer: transferName(transfer), Matrix: matrixName(matrix), Range: "limited"}
	if full {
		color.Range = "full"
	}
	return color
}

// CompatibleVideoConstraints сверяет coded с фактическим declared, уточняя только значение unspecified
// на тех осях CICP, которые присутствовали во внешнем дескрипторе. provenance не меняет
// профиль, глубину, субдискретизацию, диапазон, геометрию и FPS. Зарезервированные коды, UNKNOWN
// и прежний SDR сохраняют строгое сравнение.
func CompatibleVideoConstraints(coded, declared core.VideoFormat, provenance media.ColorConfigProvenance) error {
	// err отклоняет выдуманное происхождение до сравнения любых значений.
	if err := provenance.Validate(); err != nil {
		return err
	}
	if provenance != (media.ColorConfigProvenance{}) && UsesExternalColor(declared) {
		if provenance.Primaries && coded.Color.Primaries == "unspecified" {
			coded.Color.Primaries = declared.Color.Primaries
		}
		if provenance.Transfer && coded.Color.Transfer == "unspecified" {
			coded.Color.Transfer = declared.Color.Transfer
		}
		if provenance.Matrix && coded.Color.Matrix == "unspecified" {
			coded.Color.Matrix = declared.Color.Matrix
		}
	}
	return CompatibleVideoActual(coded, declared)
}

// ValidateVideoConfig проверяет исходные заголовки против Info времени выполнения с его доказательством внешнего цвета.
// Старые Validate*Config без Info остаются строгими; желаемый профиль не создаёт provenance.
func ValidateVideoConfig(config media.CodecConfig) error {
	// err сообщает о структурно неверном Info до разбора кодека.
	if err := config.Validate(); err != nil {
		return err
	}
	// info и headers содержат независимые копии описания и исходной записи.
	info, headers := config.Info(), config.Headers()
	if info.Track != media.TrackVideo {
		return ErrCodec
	}
	return ValidateVideoHeaders(headers, info.Video, info.HDR.ColorConfig)
}

// ValidateVideoHeaders сверяет исходную запись headers с declared и отдельно проверенным provenance.
// Восстановитель файла может передать nclx без выдуманной идентичности дорожки времени выполнения.
func ValidateVideoHeaders(headers []byte, declared core.VideoFormat, provenance media.ColorConfigProvenance) error {
	// err отклоняет недопустимый признак происхождения до ветви неполного hvcC.
	if err := provenance.Validate(); err != nil {
		return err
	}
	switch declared.Codec {
	case core.CodecAV1:
		return validateAV1Config(headers, declared, provenance)
	case core.CodecHEVC:
		return validateHEVCConfig(headers, declared, provenance)
	case core.CodecVP9:
		return validateVP9Config(headers, declared, provenance)
	case core.CodecH264:
		// coded содержит только факты SPS, включая реальные временные параметры и диапазон; err сохраняет отказ разбора.
		coded, err := H264Format(VideoDescription{Headers: headers, Width: declared.Width, Height: declared.Height})
		if err != nil {
			return err
		}
		return CompatibleVideoConstraints(coded, declared, provenance)
	}
	return ErrCodec
}
