package rtmp

import (
	"errors"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/bluenviron/gortmplib/pkg/amf0"
	"github.com/bluenviron/gortmplib/pkg/message"
)

// ingestVideoFourCC ограничивает вход конечными AV1/HEVC/VP9 транспортными формами.
func ingestVideoFourCC(fourCC message.FourCC) bool {
	return fourCC == message.FourCCAV1 || fourCC == message.FourCCHEVC || fourCC == message.FourCCVP9
}

// ingestCodec сопоставляет проверенный FourCC внутреннему кодеку без альтернативных aliases.
func ingestCodec(fourCC message.FourCC) core.Codec {
	switch fourCC {
	case message.FourCCHEVC:
		return core.CodecHEVC
	case message.FourCCVP9:
		return core.CodecVP9
	}
	return core.CodecAV1
}

// hdrVideo распознаёт действительный HDR transfer без назначения отсутствующих static metadata.
func hdrVideo(video core.VideoFormat) bool {
	return video.Color.Transfer == "smpte2084" || video.Color.Transfer == "arib-std-b67"
}

// sameIngestVideo сравнивает наблюдаемый формат, учитывая математическое равенство FPS.
func sameIngestVideo(left, right core.VideoFormat) bool {
	if !left.FrameRate.Equal(right.FrameRate) {
		return false
	}
	right.FrameRate = left.FrameRate
	return left == right
}

// videoConfig проверяет исходные headers выбранного кодека до замены действующего источника.
// Неполные FPS/geometry нового record устанавливаются по фактическому AU, а не желаемому профилю.
func (p *publication) videoConfig(fourCC message.FourCC) error {
	if !ingestVideoFourCC(fourCC) || p.wire == nil || len(p.wire.lastConfig) < 6 || p.videoRevision == ^uint64(0) {
		return ErrCodec
	}
	// headers содержит исходный record без транспортного префикса.
	headers := p.wire.lastConfig[5:]
	// selected выбирает семантику record; err хранит ошибку структурного разбора.
	selected := ingestCodec(fourCC)
	var err error
	switch selected {
	case core.CodecAV1:
		p.description, err = parseAV1Config(headers)
	case core.CodecHEVC:
		p.hevc, err = codec.ParseHEVCConfig(headers)
		p.hevcReady = err == nil
		if errors.Is(err, codec.ErrUnready) {
			err = nil
		}
	case core.CodecVP9:
		p.vp9, err = codec.ParseVP9Config(headers)
	}
	if err != nil {
		return err
	}
	if err = p.invalidate(); err != nil {
		return err
	}
	p.av1c = append([]byte(nil), headers...)
	p.videoCodec = selected
	p.hdrState = codec.HDRState{}
	p.videoRevision++
	p.ready = false
	return nil
}

// inspectVideo устанавливает actual формат и независимость сжатого AU выбранного кодека.
func (p *publication) inspectVideo(payload []byte) (core.VideoFormat, bool, error) {
	switch p.videoCodec {
	case core.CodecHEVC:
		if !p.hevcReady {
			// derived содержит временную полную запись кодека; err сохраняет отказ восстановления контекста разбора.
			// Публикуемые raw headers остаются p.av1c, включая их исходную неполноту.
			derived, err := codec.CompleteHEVCConfig(p.av1c, payload)
			if err != nil {
				return core.VideoFormat{}, false, err
			}
			p.hevc, err = codec.ParseHEVCConfig(derived)
			if err != nil {
				return core.VideoFormat{}, false, err
			}
			p.hevcReady = true
		}
		// description содержит только реальные headers; actual выводится из SPS и metadata FPS.
		description := codec.VideoDescription{Headers: p.av1c}
		// actual и err сохраняют фактический формат HEVC и возможную неполноту timing.
		actual, err := p.hevc.Format(description, p.fps)
		if err != nil && !errors.Is(err, codec.ErrUnready) {
			return actual, false, err
		}
		if actual.FrameRate.Validate() != nil && p.fps.Validate() == nil {
			actual.FrameRate = p.fps
		} else if p.fps.Validate() == nil && !actual.FrameRate.Equal(p.fps) {
			return actual, false, ErrCodec
		}
		// independent и inspectErr проверяют AU независимо от недостаточного FPS в SPS.
		independent, inspectErr := p.hevc.Inspect(payload)
		if inspectErr != nil {
			return actual, false, inspectErr
		}
		if actual.Validate() != nil {
			return actual, independent, codec.ErrUnready
		}
		return actual, independent, nil
	case core.CodecVP9:
		// description сохраняет опорные картинки, но внешний цвет применяется только к проверке этого AU.
		description := p.vp9
		// effective содержит проверенный внешний CICP, metadataErr — отказ сверки с исходной записью до проверки цвета самого кадра.
		effective, _, metadataErr := p.override.apply(core.VideoFormat{Codec: core.CodecVP9, BitDepth: description.BitDepth, Color: description.Color}, media.HDRMetadata{})
		if metadataErr != nil {
			return core.VideoFormat{}, false, metadataErr
		}
		description.Color = effective.Color
		// actual и independent содержат фактический кадр с внешним цветом; inspectErr сохраняет противоречие записи кодека и кадра.
		actual, independent, inspectErr := description.Inspect(payload, p.fps)
		// После сброса следующая AU снова проверяется с исходным цветом записи кодека, без прежнего переопределения.
		description.Color = p.vp9.Color
		if inspectErr == nil {
			p.vp9 = description
		}
		return actual, independent, inspectErr
	default:
		return p.description.inspect(payload, p.fps)
	}
}

// videoMetadata принимает нормативные пары Metadata и сохраняет старую форму onMetaData с FPS.
// Новый colorInfo заменяет весь override; invalidate отзывает старые доставки до следующих AU.
func (p *publication) videoMetadata(data amf0.Data) error {
	// first и named отличают нормативные пары Metadata от прежней обёртки onMetaData с FPS.
	var first string
	var named bool
	if len(data) > 0 {
		first, named = data[0].(string)
	}
	if named && first != "onMetaData" && first != "@setDataFrame" {
		if len(data)%2 != 0 {
			return ErrCodec
		}
		// index перебирает пары Metadata в их исходном порядке.
		for index := 0; index < len(data); index += 2 {
			// name и named проверяют строковое имя очередной пары.
			name, named := data[index].(string)
			if !named {
				return ErrCodec
			}
			if name != "colorInfo" {
				continue
			}
			// next и err сохраняют новый полный объект только после проверки всех полей.
			next, err := parseColorOverride(data[index+1])
			if err != nil {
				return err
			}
			if next == p.override {
				continue
			}
			if err = p.invalidate(); err != nil {
				return err
			}
			if p.videoRevision == ^uint64(0) {
				return ErrLimit
			}
			p.override = next
			p.videoRevision++
		}
		return nil
	}
	return p.metadata(data)
}
