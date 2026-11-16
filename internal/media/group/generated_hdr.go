package group

import (
	"context"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/GregoryGost/gost-restreamer/internal/media/encoding"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

// completeGeneratedHDR согласует первый независимый кадр собственного HDR-кодирования до публикации.
// Прочитанный префикс ограничен прежними бюджетами числа и размера пакетов и возвращается
// в исходном порядке. SDR, copy и готовые файлы этот дополнительный этап не используют.
// ctx ограничивает чтение из demux, profile выбирает рецепт, source закрепляет исходные HDR-сведения,
// limits задаёт бюджеты удерживаемых пакетов; результат сохраняет их исходные метки и содержимое.
func completeGeneratedHDR(ctx context.Context, demux *fmp4.Demuxer, profile core.GroupProfile, source media.HDRMetadata, limits fmp4.Limits) (fmp4.Init, []media.Packet, media.HDRMetadata, error) {
	// init содержит фактические конфигурации дорожек; container хранит HDR-описание контейнера
	// до восстановления сведений из source.
	init := demux.Init()
	var container media.HDRMetadata
	// description выбирает видеозапись из уже разобранных заголовков, без нового чтения файла.
	for _, description := range demux.Descriptions() {
		if description.Track == media.TrackVideo {
			container = description.HDR
		}
	}
	// prefix сохраняет все прочитанные AU, retained считает их логический вес в текущем бюджете.
	var prefix []media.Packet
	retained := uint64(0)
	// inspector проверяет первый фактический кадр и отделяет его метаданные кодека от Info.HDR,
	// заполненного сведениями из источника.
	var inspector codec.Inspector
	for {
		// err сообщает об отмене подготовки до очередного чтения в пределах бюджета.
		if err := ctx.Err(); err != nil {
			return init, nil, container, err
		}
		if uint64(len(prefix)) >= uint64(limits.MaxSamples) {
			return init, nil, container, fmp4.ErrLimit
		}
		// packet и err содержат следующий исходный AU и причинную ошибку контейнерного чтения.
		packet, err := demux.Next()
		if err != nil {
			return init, nil, container, err
		}
		if packet.StoredBytes() > limits.MaxMdatBytes-retained {
			return init, nil, container, fmp4.ErrLimit
		}
		retained += packet.StoredBytes()
		prefix = append(prefix, packet)
		// independent и inband получены только из текущих заголовков и кадра, без подстановки Info.HDR.
		independent, inband, err := inspector.InspectMetadata(packet)
		if err != nil {
			return init, nil, container, err
		}
		if packet.Config().Info().Track != media.TrackVideo {
			continue
		}
		if !independent {
			return init, nil, container, codec.ErrUnready
		}
		// effective выбирает подтверждённые метаданные сжатого кадра и проверяет согласованность
		// контейнерного и внутрипотокового описаний с source.
		effective, err := codec.ReconcileFreshHDR(source, container, inband)
		if err != nil {
			return init, nil, container, err
		}
		// info сохраняет фактический формат, идентичность и единицу времени дорожки,
		// меняя только подтверждённый снимок HDR-сведений.
		info := init.Video.Info()
		info.HDR = effective
		init.Video, err = media.NewCodecConfig(info, init.Video.Headers())
		if err != nil {
			return init, nil, container, err
		}
		init.Video, err = encoding.RestoreSourceHDR(profile, init.Video, source)
		return init, prefix, container, err
	}
}

// generatedPacketConfig переносит исходный AU на согласованную пару immutable конфигураций.
// Сжатые байты, PTS/DTS/duration и порядок выдачи не изменяются.
func generatedPacketConfig(packet media.Packet, init fmp4.Init) (media.Packet, error) {
	// config выбирает согласованную дорожку с исходной identity/timebase данного результата.
	config := init.Video
	if packet.Config().Info().Track == media.TrackAudio {
		config = init.Audio
	}
	return media.NewPacket(packet.Info(), config, packet.Payload())
}
