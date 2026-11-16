package group

import (
	"context"
	"net"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/GregoryGost/gost-restreamer/internal/media/encoding"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

// readGeneratedLiveOutput читает только свежий выход собственного LIVE-кодировщика.
// Для FFmpeg7.1.5 исправление собственного AAC7.1 ASC разрешается только при LIVE transcode.
// Единственный source при HDR закрепляет снимок этой попытки и проверяется
// по первой независимой AU до публикации. Скопированное аудио сохраняет исходные заголовки.
// ctx ограничивает чтение соединения c группы g; attempt и binding связывают физическую попытку,
// progress сообщает продвижение дорожек, profile задаёт проверенный собственный рецепт.
func (g *Group) readGeneratedLiveOutput(ctx context.Context, c net.Conn, attempt uint64, progress ffmpeg.Progress, binding *attemptBinding, profile core.GroupProfile, source ...media.HDRMetadata) error {
	if (profile.Format.Video.Color.Transfer == "smpte2084" || profile.Format.Video.Color.Transfer == "arib-std-b67") && len(source) == 1 {
		return g.readOutputOwned(ctx, c, Live, attempt, core.MediaFormat{}, progress, binding, generatedLiveHeaders(profile), profile.Format.Audio.Codec == core.CodecFLAC && profile.Mode == core.ModeTranscode && profile.EffectiveLiveAudioMode() == core.LiveAudioTranscode, source[0])
	}
	return g.readOutputOwned(ctx, c, Live, attempt, core.MediaFormat{}, progress, binding, generatedLiveHeaders(profile), profile.Format.Audio.Codec == core.CodecFLAC && profile.Mode == core.ModeTranscode && profile.EffectiveLiveAudioMode() == core.LiveAudioTranscode)
}

// generatedLiveHeaders возвращает обработчик только для известного собственного AAC-рецепта.
// Он сохраняет видео, проверяет исправленный ASC общим разборщиком и сверяет фактическую частоту
// и карту каналов с profile; другая сигнализация возвращает ошибку до проверки контейнера.
// Возвращаемая функция принимает описание desc и сохраняет видеодорожку побайтно.
func generatedLiveHeaders(profile core.GroupProfile) func(fmp4.TrackDescription) ([]byte, error) {
	if !encoding.UsesNativeAAC71(profile, encoding.Live) {
		return nil
	}
	return func(desc fmp4.TrackDescription) ([]byte, error) {
		if desc.Track != media.TrackAudio {
			return desc.Headers, nil
		}
		// headers содержит проверенный собственный ASC; err сообщает об отказе узкой нормализации.
		headers, err := codec.NormalizeNativeAAC71ASC(desc.Headers)
		if err != nil {
			return nil, err
		}
		// actual выводится из исправленных байтов, поэтому желаемый формат не может скрыть несовпадение
		// частоты или расположения каналов.
		actual, err := codec.AudioFormatASC(headers)
		if err != nil {
			return nil, err
		}
		if actual != profile.Format.Audio {
			return nil, codec.ErrCodec
		}
		return headers, nil
	}
}
