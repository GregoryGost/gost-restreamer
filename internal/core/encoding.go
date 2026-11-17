package core

import (
	"fmt"
	"math"
	"math/big"
)

const (
	// VideoEncoderX265 выбирает программный кодировщик HEVC с фиксированным рецептом.
	VideoEncoderX265 = "libx265"
	// VideoEncoderHEVCNVENC выбирает NVIDIA-кодировщик HEVC с фиксированным рецептом.
	VideoEncoderHEVCNVENC = "hevc_nvenc"
	// VideoEncoderVP9 выбирает программный кодировщик VP9.
	VideoEncoderVP9 = "libvpx-vp9"
	// VideoEncoderNVENC обозначает NVIDIA-кодировщик H.264.
	VideoEncoderNVENC = "h264_nvenc"
	// VideoEncoderSVTAV1 обозначает программный кодировщик SVT-AV1.
	VideoEncoderSVTAV1 = "libsvtav1"
	// VideoEncoderX264 обозначает программный кодировщик H.264.
	VideoEncoderX264 = "libx264"
	// VideoEncoderAOMAV1 обозначает программный кодировщик AOM AV1.
	VideoEncoderAOMAV1 = "libaom-av1"
	// AudioEncoderAAC обозначает поддерживаемый кодировщик AAC.
	AudioEncoderAAC = "aac"
	// AudioEncoderOpus выбирает фиксированный рецепт CELT.
	AudioEncoderOpus = "libopus"
	// AudioEncoderFLAC выбирает нативное кодирование PCM16 без целевого битрейта.
	AudioEncoderFLAC = "flac"
	// AudioEncoderMP3 выбирает MPEG1 Layer III без резервуара.
	AudioEncoderMP3 = "libmp3lame"
	// AudioEncoderAC3 выбирает обычный AC-3 stereo.
	AudioEncoderAC3 = "ac3"
	// RateControlCBR выбирает режим постоянного видеобитрейта.
	RateControlCBR = "cbr"
	// RateControlVBR выбирает режим переменного видеобитрейта.
	RateControlVBR = "vbr"
	// RateControlCQP выбирает постоянный квантователь поддерживающего его кодировщика.
	RateControlCQP = "cqp"
	// RateControlCRF выбирает постоянное качество поддерживающего его кодировщика.
	RateControlCRF = "crf"
)

// OptionalQP сохраняет присутствие и целый квантователь независимо от нулевого значения.
type OptionalQP struct {
	// Set отмечает явно выбранный квантователь.
	Set bool
	// Value содержит квантователь; при Set=false требуется ноль.
	Value int
}

// OptionalCRF сохраняет присутствие и числовое качество независимо от нулевого значения.
type OptionalCRF struct {
	// Set отмечает явно выбранное качество.
	Set bool
	// ValueHundredths содержит качество в сотых долях; при Set=false требуется ноль.
	ValueHundredths int
}

// EncodingSettings хранит полный материализованный запрос кодирования; нулевой блок недопустим.
type EncodingSettings struct {
	// QP содержит явно заданный квантователь CQP.
	QP OptionalQP
	// CRF содержит явно заданное качество CRF.
	CRF OptionalCRF
	// VideoEncoder выбирает конкретный кодировщик видео для подготовки профиля.
	VideoEncoder string
	// AudioEncoder выбирает кодировщик аудиодорожки.
	AudioEncoder string
	// RateControl выбирает управление видеобитрейтом CBR/VBR либо качеством CQP/CRF.
	RateControl string
	// RescaleOutput разрешает масштабирование LIVE-видео в режиме transcode.
	RescaleOutput bool
	// VideoCustom сохраняет нормализованные дополнительные параметры видеокодировщика.
	VideoCustom string
	// AudioCustom сохраняет нормализованные дополнительные параметры аудиокодировщика.
	AudioCustom string
	// NVENC содержит параметры NVIDIA H.264; для остальных кодировщиков блок должен быть нулевым.
	NVENC NVENCSettings
	// SVTAV1 содержит параметры SVT-AV1; для остальных кодировщиков блок должен быть нулевым.
	SVTAV1 SVTAV1Settings
}

// NVENCSettings описывает запрошенные параметры NVIDIA, а не измеренное состояние устройства.
type NVENCSettings struct {
	// Preset выбирает предустановку выбранного кодировщика.
	Preset string
	// Tuning выбирает настройку качества или задержки выбранного кодировщика.
	Tuning string
	// Multipass выбирает режим многопроходного анализа NVENC.
	Multipass string
	// LookaheadFrames задаёт глубину предварительного анализа в кадрах.
	LookaheadFrames int
	// EnableSpatialAQ включает пространственное адаптивное квантование NVENC.
	EnableSpatialAQ bool
	// EnableTemporalAQ включает временное адаптивное квантование NVENC.
	EnableTemporalAQ bool
	// AQStrength задаёт силу пространственного адаптивного квантования NVENC.
	AQStrength int
}

// SVTAV1Settings описывает применимые параметры подготовки AV1.
type SVTAV1Settings struct {
	// Preset выбирает предустановку выбранного кодировщика.
	Preset int
	// Tuning выбирает настройку качества или задержки выбранного кодировщика.
	Tuning string
	// AQMode выбирает режим адаптивного квантования SVT-AV1.
	AQMode int
	// LookaheadFrames задаёт глубину анализа SVT-AV1 в кадрах; -1 выбирает автоматическую глубину
	// для VBR.
	LookaheadFrames int
}

// DefaultEncoding материализует применимые настройки нового запроса mode/videoEncoder/rateControl.
// AAC выбран первоначально; QP и CRF требуют явного присутствия. Сохранённые профили
// не проходят через этот конструктор; результат требует Validate полного профиля.
func DefaultEncoding(mode GroupMode, videoEncoder, rateControl string) EncodingSettings {
	// e накапливает настройки нового запроса с начальными значениями выбранного кодировщика.
	e := EncodingSettings{VideoEncoder: videoEncoder, AudioEncoder: AudioEncoderAAC, RateControl: rateControl, RescaleOutput: mode == ModeTranscode}
	switch videoEncoder {
	case VideoEncoderNVENC:
		e.NVENC = NVENCSettings{Preset: "p4", Tuning: "hq", Multipass: "disabled", AQStrength: 8}
	case VideoEncoderSVTAV1:
		e.SVTAV1 = SVTAV1Settings{Preset: 8, Tuning: "psnr", AQMode: 2}
		if rateControl == RateControlVBR || rateControl == RateControlCQP || rateControl == RateControlCRF {
			e.SVTAV1.LookaheadFrames = -1
		}
		if rateControl == RateControlCQP {
			e.SVTAV1.AQMode = 0
		}
	}
	return e
}

// ProfileError содержит только доверенные объяснения и публичные пути полей.
type ProfileError struct {
	// code содержит машинный код отказа проверки профиля.
	code string
	// field указывает публичный путь некорректного поля профиля.
	field string
	// detail содержит доверенное объяснение отказа без значения пользовательского запроса.
	detail string
}

// Error соединяет публичный путь поля с объяснением ошибки профиля.
func (e *ProfileError) Error() string { return fmt.Sprintf("%s: %s", e.field, e.detail) }

// Unwrap связывает ошибку поля с ErrInvalidProfile для errors.Is.
func (e *ProfileError) Unwrap() error { return ErrInvalidProfile }

// Code возвращает машинный код ошибки профиля.
func (e *ProfileError) Code() string { return e.code }

// Field возвращает публичный путь некорректного поля.
func (e *ProfileError) Field() string { return e.field }

// Detail возвращает подготовленное объяснение ошибки без добавления значения запроса.
func (e *ProfileError) Detail() string { return e.detail }

// invalidEncoding создаёт ошибку invalid_profile и добавляет к пути поля префикс «profile.».
func invalidEncoding(field, detail string) error {
	return &ProfileError{code: "invalid_profile", field: "profile." + field, detail: detail}
}

// validateEncodingProfile проверяет полный p без назначения значений по умолчанию.
// Геометрия, точные временные шкалы и GOP общие для всех кодировщиков; видео и аудио
// затем проверяются независимо, без подстановки другого формата дорожки.
func (p GroupProfile) validateEncodingProfile() error {
	// e содержит кодировщики, v и a — реальные желаемые дорожки, ve — битрейты и GOP.
	e, v, a, ve := p.Encoding, p.Format.Video, p.Format.Audio, p.VideoEncoding
	if p.Format.Validate() != nil || v.Width%2 != 0 || v.Height%2 != 0 || v.Width > 65534 || v.Height > 65534 {
		return invalidEncoding("format", "Требуются обе дорожки и положительные чётные размеры не более 65534.")
	}
	if !validEncodingFrameRate(v.FrameRate, a.SampleRateHz) {
		return invalidEncoding("format.video.frame_rate", "Частота и временные шкалы должны точно представляться FFmpeg.")
	}
	if !ve.ClosedGOP || ve.GOPFrames == 0 || ve.GOPFrames > math.MaxInt32 {
		return invalidEncoding("video_encoding.gop_frames", "Требуется закрытый GOP длиной от 1 до 2147483647 кадров.")
	}
	if p.Mode == ModeCopy && e.RescaleOutput {
		return invalidEncoding("encoding.rescale_output", "В режиме copy масштабирование LIVE недоступно.")
	}
	// err сообщает отказ конкретной аудиопривязки, включая применимость LIVE-режима FLAC.
	if err := p.validateAudioEncoding(); err != nil {
		return err
	}
	switch v.Codec {
	case CodecAV1, CodecH264:
		return p.validateAV1H264Encoding()
	case CodecHEVC, CodecVP9:
		return p.validateHEVCVP9Encoding()
	default:
		return invalidEncoding("format.video.codec", "Видеокодек отсутствует в поддерживаемом наборе настроек.")
	}
}

// validAV1Level разрешает пустой уровень, auto либо обозначение уровня от 2.0 до 7.3 с последней
// цифрой 0..3.
func validAV1Level(level string) bool {
	if level == "" || level == "auto" {
		return true
	}
	return len(level) == 3 && level[0] >= '2' && level[0] <= '7' && level[1] == '.' && level[2] >= '0' && level[2] <= '3'
}

// validEncodingFrameRate проверяет положительную частоту и представимость шкал видео и аудио в int32,
// а затем точность восстановления исходной дроби при преобразовании FPS для FFmpeg.
func validEncodingFrameRate(rate FrameRate, sampleRate uint32) bool {
	if rate.Validate() != nil || sampleRate == 0 {
		return false
	}
	// gcd вычисляет НОД частей частоты и временных шкал для проверки их представимости.
	gcd := func(a, b uint32) uint32 {
		// Вычисляет НОД без изменения исходного FrameRate для сокращения дроби и проверки общих временных
		// шкал.

		for b != 0 {
			a, b = b, a%b
		}
		return a
	}
	// g содержит общий делитель частей исходной частоты кадров.
	g := gcd(rate.Numerator, rate.Denominator)
	// n содержит сокращённый числитель частоты кадров, а d — сокращённый знаменатель.
	n, d := rate.Numerator/g, rate.Denominator/g
	if n > math.MaxInt32 || d > math.MaxInt32 || uint64(n/gcd(n, sampleRate))*uint64(sampleRate) > math.MaxInt32 {
		return false
	}
	// При n < 1000 подготовка использует НОК шкал видео и аудио; длительность пакета FFmpeg ограничена int32.
	if n < 1000 && uint64(d)*uint64(sampleRate/gcd(n, sampleRate)) > math.MaxInt32 {
		return false
	}
	// Проверяется точность av_d2q для FPS-фильтра FFmpeg, без округления исходного профиля.
	// x содержит частоту кадров в представлении float64 для проверки точности преобразования FFmpeg.
	x := float64(n) / float64(d)
	// exponent содержит двоичный порядок частоты для построения приближённой дроби.
	_, exponent := math.Frexp(x)
	exponent = max(exponent-1, 0)
	// denominator содержит знаменатель двоичного приближения частоты кадров.
	denominator := int64(1) << (62 - exponent)
	// numerator содержит округлённый числитель двоичного приближения частоты кадров.
	numerator := int64(math.Floor(x*float64(denominator) + 0.5))
	// difference накапливает точную разницу исходной и приближённой дробей без переполнения
	// машинного целого.
	difference := new(big.Int).Mul(big.NewInt(numerator), big.NewInt(int64(d)))
	difference.Sub(difference, new(big.Int).Mul(big.NewInt(int64(n)), big.NewInt(denominator)))
	difference.Abs(difference)
	difference.Mul(difference, big.NewInt(2*math.MaxInt32))
	return difference.Cmp(big.NewInt(denominator)) < 0
}
