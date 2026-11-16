package encoding

import (
	"fmt"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
)

// RestoreSourceHDR переносит исходные авторские сведения source только в свежий собственный
// результат output проверенного profile. Вызывающий подтверждает, что результат создан его текущей
// попыткой кодирования. Присутствующая выходная сигнализация должна совпадать с направленной проекцией source;
// конфигурация сохраняет фактический формат и исходные AU, отсутствующие исходные блоки не создаются.
func RestoreSourceHDR(profile core.GroupProfile, output media.CodecConfig, source media.HDRMetadata) (media.CodecConfig, error) {
	if profile.Validate() != nil || output.Validate() != nil || output.Info().Track != media.TrackVideo {
		return media.CodecConfig{}, core.ErrInvalid
	}
	// err сообщает об отсутствующем числовом представлении или некорректном происхождении source.
	if err := source.Validate(); err != nil {
		return media.CodecConfig{}, err
	}
	// info копирует проверенный фактический формат без изменения исходной конфигурации.
	info := output.Info()
	if !hdrTransfer(info.Video.Color.Transfer) {
		if source != (media.HDRMetadata{}) {
			return media.CodecConfig{}, fmt.Errorf("%w: HDR-сведения при SDR-выходе", core.ErrInvalidFormat)
		}
		return output, nil
	}
	// restored дополняет только отсутствующие блоки; err сообщает о конфликтующей фактической
	// сигнализации, которую нельзя исправлять ретегированием.
	restored, err := codec.RestoreSourceHDR(info.HDR, source)
	if err != nil {
		return media.CodecConfig{}, fmt.Errorf("исходные HDR-сведения противоречат результату: %w", err)
	}
	info.HDR = restored
	return media.NewCodecConfig(info, output.Headers())
}
