package rtmp

import (
	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
)

// validateNewOutboundVideo ограничивает native HEVC/VP9 подтверждённой конечной матрицей.
// Уровень проверяется фактическим record parser, а его desired-ограничение — общей совместимостью.
func validateNewOutboundVideo(video core.VideoFormat) error {
	if video.Width%2 != 0 || video.Height%2 != 0 || video.PixelFormat != "yuv420p" && video.PixelFormat != "yuv420p10le" {
		return ErrCodec
	}
	if !hdrVideo(video) && (video.Color.Primaries != "bt709" || video.Color.Transfer != "bt709" || video.Color.Matrix != "bt709") {
		return ErrCodec
	}
	if video.Codec == core.CodecHEVC {
		if video.Tier != "" && video.Tier != "main" && video.Tier != "high" || video.BitDepth == 8 && video.CodecProfile != "main" || video.BitDepth == 10 && video.CodecProfile != "main10" {
			return ErrCodec
		}
		return nil
	}
	if video.Tier != "" || video.BitDepth == 8 && video.CodecProfile != "profile0" || video.BitDepth == 10 && video.CodecProfile != "profile2" {
		return ErrCodec
	}
	return nil
}

// outboundFourCC возвращает настоящий транспортный FourCC, сохраняя legacy SDR H.264 отдельно.
func outboundFourCC(video core.VideoFormat) string {
	switch video.Codec {
	case core.CodecHEVC:
		return "hvc1"
	case core.CodecVP9:
		return "vp09"
	case core.CodecAV1:
		return "av01"
	case core.CodecH264:
		return "avc1"
	}
	return ""
}

// enhancedVideo выбирает Enhanced AVC при HDR; старый SDR AVC сохраняет legacy wire.
func enhancedVideo(info media.CodecConfigInfo) bool {
	return info.Video.Codec != core.CodecH264 || hdrVideo(info.Video) || info.HDR != (media.HDRMetadata{})
}

// videoFramePrefix записывает нормативный префикс одного AU, не меняя payload.
// HEVC использует signed24 CTS и CodedFramesX только при нулевом CTS; VP9 всегда CodedFrames.
func videoFramePrefix(info media.CodecConfigInfo, independent bool, cts int32) []byte {
	// frame содержит фактический независимый либо зависимый признак кадра.
	frame := byte(2)
	if independent {
		frame = 1
	}
	// offset содержит младшие24 бита знакового CTS в сетевом представлении.
	offset := uint32(cts) & 0xffffff
	if !enhancedVideo(info) {
		return []byte{frame<<4 | 7, 1, byte(offset >> 16), byte(offset >> 8), byte(offset)}
	}
	// kind обозначает CodedFrames либо HEVC CodedFramesX при отсутствии смещения.
	kind := byte(1)
	if info.Video.Codec == core.CodecHEVC && cts == 0 {
		kind = 3
	}
	// fourCC и prefix формируют исходный пятибайтовый Enhanced Video префикс.
	fourCC := outboundFourCC(info.Video)
	prefix := []byte{0x80 | frame<<4 | kind, fourCC[0], fourCC[1], fourCC[2], fourCC[3]}
	if kind == 1 && (info.Video.Codec == core.CodecHEVC || info.Video.Codec == core.CodecH264) {
		prefix = append(prefix, byte(offset>>16), byte(offset>>8), byte(offset))
	}
	return prefix
}

// validateNewVideoHeaders сверяет новый record с заявленным actual до первой сетевой записи.
// Геометрия и FPS VP9 подтверждаются первым AU общим Inspector, поскольку record их не содержит.
func validateNewVideoHeaders(config media.CodecConfig) error {
	return codec.ValidateVideoConfig(config)
}
