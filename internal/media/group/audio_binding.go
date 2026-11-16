package group

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"fmt"
	"io"
	"os"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

// validateSourceAudio сверяет audio с реальными заголовками и аудиорежимом profile до запуска FFmpeg.
// Режим copy требует точного целевого формата; transcode сохраняет весь прежний допуск AAC LC.
// Нормализация настоящего начала Opus уже выполнена адаптером, поэтому ненулевое начальное исключение отклоняется.
func validateSourceAudio(profile core.GroupProfile, audio media.CodecConfig) error {
	if profile.Validate() != nil || audio.Validate() != nil || audio.Info().Track != media.TrackAudio {
		return codec.ErrCodec
	}
	// kind выбирает фактический разборщик; actual и err сохраняют формат заголовка и причину отказа.
	kind := audio.Info().Audio.Codec
	actual, err := codec.AudioFormatHeaders(kind, audio.Headers())
	if err != nil {
		return err
	}
	if actual != audio.Info().Audio {
		return codec.ErrCodec
	}
	if kind == core.CodecOpus {
		// head содержит обработанное адаптером начальное исключение; parseErr сообщает о повреждении.
		head, parseErr := codec.ParseOpusHead(audio.Headers())
		if parseErr != nil || head.PreSkip != 0 {
			return codec.ErrCodec
		}
	}
	if kind == core.CodecFLAC && (profile.Mode != core.ModeTranscode || profile.EffectiveLiveAudioMode() != core.LiveAudioTranscode) {
		return codec.ErrCodec
	}
	if profile.EffectiveLiveAudioMode() == core.LiveAudioCopy && actual != profile.Format.Audio {
		return codec.ErrCodec
	}
	return nil
}

// validateGroupAudio проверяет audio на границе выхода Switcher для profile.
// FLAC допускается только от доверенного кодировщика; Opus уже имеет нулевое начальное исключение.
func validateGroupAudio(profile core.GroupProfile, audio media.CodecConfig) error {
	// err сохраняет отказ заголовков или режима исходного аудио.
	if err := validateSourceAudio(profile, audio); err != nil {
		return err
	}
	if audio.Info().Audio.Codec == core.CodecFLAC && !audio.Info().FLACOwned {
		return codec.ErrCodec
	}
	return nil
}

// validateTranscodeSource проверяет source до пользовательского построителя и создания процесса.
// profile определяет целевую видеоматрицу; аудио source проверяется без подстановки желаемого формата.
// У первой аудиоединицы дополнительно проверяются фактические заголовки, содержимое и длительность.
func validateTranscodeSource(profile core.GroupProfile, source media.Delivery) error {
	// err сохраняет отказ аудиоконфигурации до рассмотрения видеокодека.
	if err := validateSourceAudio(profile, source.AudioConfig); err != nil {
		return err
	}
	// kind определяет фактический видеокодек разрешённого LIVE-входа.
	kind := source.VideoConfig.Info().Video.Codec
	if kind != core.CodecAV1 && kind != core.CodecHEVC && kind != core.CodecVP9 {
		return codec.ErrCodec
	}
	// Видеопакеты сохраняют прежнюю проверку на входном порте контейнера.
	if source.Packet.Config().Info().Track != media.TrackAudio {
		return nil
	}
	// inspector проверяет первую фактическую аудиоединицу без изменения пакета или временной шкалы.
	var inspector codec.Inspector
	// err сохраняет причину отказа фактической единицы доступа.
	_, err := inspector.Inspect(source.Packet)
	return err
}

// newTranscodeInputMuxer выбирает приватное физическое FLAC-представление только при явном
// перекодировании аудио в profile. writer принимает байты, init задаёт дорожки, limits — бюджеты.
func newTranscodeInputMuxer(writer io.Writer, init fmp4.Init, limits fmp4.Limits, profile core.GroupProfile) (*fmp4.Muxer, error) {
	if profile.EffectiveLiveAudioMode() == core.LiveAudioTranscode {
		return fmp4.NewAudioTranscodeInputMuxer(writer, init, limits)
	}
	return fmp4.NewMuxer(writer, init, limits)
}

// ownFLACOutput создаёт отдельную конфигурацию audio лишь после проверки фактического packet.
// trusted подтверждает собственный кодировщик данной попытки, profile — его полный целевой формат.
// Заголовки и AU внешнего источника сами по себе права FLACOwned не дают.
func ownFLACOutput(audio media.CodecConfig, packet media.Packet, profile core.GroupProfile, trusted bool) (media.CodecConfig, error) {
	if !trusted || audio.Info().Audio.Codec != core.CodecFLAC {
		return audio, nil
	}
	if profile.Validate() != nil || profile.Mode != core.ModeTranscode || profile.EffectiveLiveAudioMode() != core.LiveAudioTranscode || audio.Info().Audio != profile.Format.Audio {
		return media.CodecConfig{}, codec.ErrCodec
	}
	if packet.Config().Info().Track != media.TrackAudio || !sameConfig(packet.Config(), audio) {
		return media.CodecConfig{}, codec.ErrCodec
	}
	// inspector проверяет STREAMINFO, структуру кадра и длительность до назначения признака происхождения.
	var inspector codec.Inspector
	// err сообщает о повреждении или несогласованности фактической единицы доступа.
	if _, err := inspector.Inspect(packet); err != nil {
		return media.CodecConfig{}, err
	}
	// info переносит ту же доказанную подачу в отдельную неизменяемую конфигурацию.
	info := audio.Info()
	info.FLACOwned = true
	return media.NewCodecConfig(info, audio.Headers())
}

// buildVerifiedFLACPlayer подтверждает выбранный manifest относительно profile и открытого file.
// ctx ограничивает проверку SHA256; binary и attempt задают прежний проигрыватель FFmpeg с FD/UDS.
// limits ограничивает разбор заголовков и первой AU файла; file остаётся открытым в позиции0.
func buildVerifiedFLACPlayer(ctx context.Context, binary string, file *os.File, attempt ffmpeg.Attempt, manifest core.ArtifactManifest, profile core.GroupProfile, limits fmp4.Limits) (ffmpeg.Command, error) {
	if file == nil || manifest.ValidateUsable() != nil || manifest.VerifiedFormat.Audio.Codec != core.CodecFLAC {
		return ffmpeg.Command{}, core.ErrCorrupt
	}
	// canonical и err подтверждают полный выбранный профиль, включая режим и рецепт кодирования.
	canonical, err := core.CanonicalProfile(profile)
	if err != nil || !bytes.Equal(canonical, manifest.CanonicalProfile) {
		return ffmpeg.Command{}, core.ErrCorrupt
	}
	// info содержит размер и тип открытого дескриптора; statErr сообщает об отказе чтения свойств.
	info, statErr := file.Stat()
	if statErr != nil || !info.Mode().IsRegular() || info.Size() <= 0 || uint64(info.Size()) != manifest.SizeBytes {
		return ffmpeg.Command{}, core.ErrCorrupt
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return ffmpeg.Command{}, err
	}
	// digest накапливает SHA256; buffer ограничивает память проверки файла; count хранит прочитанные байты.
	digest := sha256.New()
	buffer := make([]byte, 32768)
	count := uint64(0)
	for {
		if err = ctx.Err(); err != nil {
			return ffmpeg.Command{}, err
		}
		// n и readErr описывают очередную порцию проверяемого содержимого.
		n, readErr := file.Read(buffer)
		count += uint64(n)
		if count > manifest.SizeBytes {
			return ffmpeg.Command{}, core.ErrCorrupt
		}
		_, _ = digest.Write(buffer[:n])
		if readErr == io.EOF {
			break
		}
		if readErr != nil {
			return ffmpeg.Command{}, readErr
		}
	}
	if count != manifest.SizeBytes || hex.EncodeToString(digest.Sum(nil)) != manifest.ContentHash {
		return ffmpeg.Command{}, core.ErrCorrupt
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return ffmpeg.Command{}, err
	}
	// demux уточняет аудиоформат по первой AU, без нормализации тишины или восстановления FLACOwned.
	demux, err := fmp4.NewDemuxer(file, fmp4.DemuxOptions{Limits: limits, SourceEpoch: 1, VideoRevision: 1, AudioRevision: 1})
	if err != nil {
		return ffmpeg.Command{}, fmt.Errorf("%w: %w", core.ErrCorrupt, err)
	}
	if demux.Init().Audio.Info().Audio != manifest.VerifiedFormat.Audio {
		return ffmpeg.Command{}, core.ErrCorrupt
	}
	if _, err = file.Seek(0, io.SeekStart); err != nil {
		return ffmpeg.Command{}, err
	}
	return ffmpeg.BuildFLACFileCommand(binary, file, attempt)
}
