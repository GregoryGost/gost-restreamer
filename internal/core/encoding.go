package core

import (
	"fmt"
	"math"
	"math/big"
	"slices"
)

const (
	// EncodingVersion задаёт версию полного блока настроек кодировщиков.
	EncodingVersion = 2
	// VideoEncoderNVENC обозначает NVIDIA-кодировщик H.264.
	VideoEncoderNVENC = "h264_nvenc"
	// VideoEncoderSVTAV1 обозначает программный кодировщик SVT-AV1.
	VideoEncoderSVTAV1 = "libsvtav1"
	// AudioEncoderAAC обозначает поддерживаемый кодировщик AAC.
	AudioEncoderAAC = "aac"
	// RateControlCBR выбирает режим постоянного видеобитрейта.
	RateControlCBR = "cbr"
	// RateControlVBR выбирает режим переменного видеобитрейта.
	RateControlVBR = "vbr"
)

// EncodingSettings хранит полный запрос кодирования. Нулевое значение обозначает прежний профиль.
type EncodingSettings struct {
	// Version фиксирует версию полного блока настроек кодировщиков.
	Version int
	// VideoEncoder выбирает конкретный кодировщик видео для подготовки профиля.
	VideoEncoder string
	// AudioEncoder выбирает кодировщик аудиодорожки.
	AudioEncoder string
	// RateControl выбирает управление видеобитрейтом CBR или VBR.
	RateControl string
	// RescaleOutput разрешает масштабирование LIVE-видео в режиме transcode.
	RescaleOutput bool
	// VideoCustom сохраняет нормализованные дополнительные параметры видеокодировщика.
	VideoCustom string
	// AudioCustom сохраняет нормализованные дополнительные параметры аудиокодировщика.
	AudioCustom string
	// NVENC содержит параметры NVIDIA H.264; для SVT-AV1 блок должен быть нулевым.
	NVENC NVENCSettings
	// SVTAV1 содержит параметры SVT-AV1; для NVIDIA блок должен быть нулевым.
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

// DefaultEncoding применяется только к новому запросу; результат требует Validate полного профиля.
func DefaultEncoding(mode GroupMode, videoEncoder, rateControl string) EncodingSettings {
	// e накапливает настройки нового запроса с начальными значениями выбранного кодировщика.
	e := EncodingSettings{Version: EncodingVersion, VideoEncoder: videoEncoder, AudioEncoder: AudioEncoderAAC, RateControl: rateControl, RescaleOutput: mode == ModeTranscode}
	switch videoEncoder {
	case VideoEncoderNVENC:
		e.NVENC = NVENCSettings{Preset: "p4", Tuning: "hq", Multipass: "disabled", AQStrength: 8}
	case VideoEncoderSVTAV1:
		e.SVTAV1 = SVTAV1Settings{Preset: 8, Tuning: "psnr", AQMode: 2}
		if rateControl == RateControlVBR {
			e.SVTAV1.LookaheadFrames = -1
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

// validateEncodingProfile проверяет полный профиль EncodingVersion, ограничения выбранного кодировщика
// и нормализованные пользовательские параметры; значения по умолчанию не назначаются.
func (p GroupProfile) validateEncodingProfile() error {
	// e содержит настройки кодировщиков, v — выходной видеоформат, a — выходной аудиоформат, а ve —
	// параметры видеобитрейта и GOP.
	e, v, a, ve := p.Encoding, p.Format.Video, p.Format.Audio, p.VideoEncoding
	if e.Version != EncodingVersion {
		return invalidEncoding("encoding.version", "Объект encoding требует version=2; прежний профиль должен иметь полностью нулевой блок настроек.")
	}
	if p.Mode != ModeCopy && p.Mode != ModeTranscode {
		return invalidEncoding("mode", "Поддерживаются режимы copy и transcode.")
	}
	if e.RateControl != RateControlCBR && e.RateControl != RateControlVBR {
		return invalidEncoding("encoding.rate_control", "Требуется явный режим cbr или vbr.")
	}
	if e.AudioEncoder != AudioEncoderAAC {
		return invalidEncoding("encoding.audio_encoder", "Поддерживается только кодировщик aac.")
	}
	if p.Mode == ModeTranscode && (e.VideoEncoder != VideoEncoderNVENC || v.Codec != CodecH264) ||
		p.Mode == ModeCopy && (e.VideoEncoder != VideoEncoderSVTAV1 || v.Codec != CodecAV1) {
		return invalidEncoding("encoding.video_encoder", "Для transcode/H.264 требуется h264_nvenc, для copy/AV1 — libsvtav1.")
	}
	if p.Mode == ModeCopy && e.RescaleOutput {
		return invalidEncoding("encoding.rescale_output", "В режиме copy размеры описывают ожидаемый поток OBS; масштабирование LIVE недоступно.")
	}
	if v.Width == 0 || v.Height == 0 || v.Width%2 != 0 || v.Height%2 != 0 {
		return invalidEncoding("format.video", "Требуются положительные чётные ширина и высота.")
	}
	if v.PixelFormat != "yuv420p" || v.BitDepth != 8 || v.Color.Primaries != "bt709" || v.Color.Transfer != "bt709" || v.Color.Matrix != "bt709" || !slices.Contains([]string{"limited", "full", "tv", "pc"}, v.Color.Range) {
		return invalidEncoding("format.video", "Поддерживается yuv420p, 8 бит, SDR bt709 и диапазон limited/full/tv/pc.")
	}
	if !validEncodingFrameRate(v.FrameRate, a.SampleRateHz) {
		return invalidEncoding("format.video.frame_rate", "Частота должна быть положительной точной дробью с представимыми шкалами времени FFmpeg.")
	}
	if !ve.ClosedGOP || ve.GOPFrames == 0 || ve.GOPFrames > math.MaxInt32 {
		return invalidEncoding("video_encoding.gop_frames", "Требуется закрытый GOP длиной от 1 до 2147483647 кадров.")
	}
	if e.VideoEncoder == VideoEncoderNVENC {
		// err содержит причину отказа проверки настроек NVENC.
		if err := p.validateNVENC(); err != nil {
			return err
		}
	} else {
		// err содержит причину отказа проверки настроек SVT-AV1.
		if err := p.validateSVTAV1(); err != nil {
			return err
		}
	}
	// validLayout показывает согласованность числа каналов со схемой mono или stereo.
	validLayout := a.Channels == 1 && a.ChannelLayout == "mono" || a.Channels == 2 && a.ChannelLayout == "stereo"
	if a.Codec != CodecAAC || a.CodecProfile != "LC" || !validLayout || !slices.Contains([]uint32{7350, 8000, 11025, 12000, 16000, 22050, 24000, 32000, 44100, 48000, 64000}, a.SampleRateHz) {
		return invalidEncoding("format.audio", "Требуется AAC LC mono/stereo с частотой 7350..64000 Гц из поддерживаемого набора.")
	}
	if p.AudioEncoding.BitrateBPS == 0 || p.AudioEncoding.BitrateBPS > 6*uint64(a.SampleRateHz)*uint64(a.Channels) {
		return invalidEncoding("audio_encoding.bitrate_bps", "Битрейт AAC должен быть положительным и не превышать 6 × частота дискретизации × число каналов.")
	}
	// custom содержит очередной кодировщик, его дополнительные параметры и поле для диагностики.
	for _, custom := range []struct { /* encoder указывает проверяемый кодировщик, value содержит его дополнительные параметры, а field указывает поле профиля для диагностики. */
		encoder, value, field string
	}{{e.VideoEncoder, e.VideoCustom, "video_custom"}, {e.AudioEncoder, e.AudioCustom, "audio_custom"}} {
		// normalized содержит каноническую строку параметров кодировщика, а err — причину отказа
		// нормализации.
		normalized, err := NormalizeCustom(custom.encoder, custom.value)
		if err != nil {
			return err
		}
		if normalized != custom.value {
			return invalidEncoding("encoding."+custom.field, "Параметры должны храниться в нормализованном виде.")
		}
	}
	return nil
}

// validateNVENC проверяет параметры H.264/NVENC, ограничения битрейта, GOP, буфера и AQ и отклоняет
// неприменимый блок SVTAV1.
func (p GroupProfile) validateNVENC() error {
	// v содержит выходной видеоформат, ve — настройки видеобитрейта и GOP, а e — запрос кодировщиков
	// для проверки NVENC.
	v, ve, e := p.Format.Video, p.VideoEncoding, p.Encoding
	if e.SVTAV1 != (SVTAV1Settings{}) {
		return invalidEncoding("encoding.svt_av1", "Блок svt_av1 неприменим к h264_nvenc.")
	}
	if v.Width > 65534 || v.Height > 65534 {
		return invalidEncoding("format.video", "Размеры H.264 не должны превышать 65534.")
	}
	if !slices.Contains([]string{"baseline", "main", "high"}, v.CodecProfile) || v.Tier != "" || !slices.Contains([]string{"", "auto", "1", "1.0", "1b", "1.0b", "1.1", "1.2", "1.3", "2", "2.0", "2.1", "2.2", "3", "3.0", "3.1", "3.2", "4", "4.0", "4.1", "4.2", "5", "5.0", "5.1", "5.2"}, v.Level) {
		return invalidEncoding("format.video", "Для H.264 поддержаны baseline/main/high, известный Level и пустой Tier.")
	}
	if ve.MaxBFrames > 4 || ve.MaxBFrames >= ve.GOPFrames || v.CodecProfile == "baseline" && ve.MaxBFrames != 0 {
		return invalidEncoding("video_encoding.max_b_frames", "Допустимы 0..4 B-кадра, меньше длины GOP; baseline требует 0.")
	}
	if ve.TargetBitrateBPS == 0 || ve.TargetBitrateBPS > ve.MaxBitrateBPS || ve.MaxBitrateBPS > math.MaxUint32 {
		return invalidEncoding("video_encoding.max_bitrate_bps", "H.264 требует 1 ≤ target ≤ max ≤ 4294967295 бит/с.")
	}
	if ve.BufferBits == 0 || ve.BufferBits > math.MaxInt32 {
		return invalidEncoding("video_encoding.buffer_bits", "Буфер H.264 должен быть от 1 до 2147483647 бит.")
	}
	if e.RateControl == RateControlCBR && ve.TargetBitrateBPS != ve.MaxBitrateBPS {
		return invalidEncoding("video_encoding.max_bitrate_bps", "Для CBR максимальный битрейт должен совпадать с целевым.")
	}
	// n содержит параметры NVENC, проверяемые по поддерживаемому каталогу.
	n := e.NVENC
	if !slices.Contains([]string{"p1", "p2", "p3", "p4", "p5", "p6", "p7"}, n.Preset) {
		return invalidEncoding("encoding.nvenc.preset", "Допустимы preset p1..p7.")
	}
	if !slices.Contains([]string{"hq", "ll", "ull"}, n.Tuning) {
		return invalidEncoding("encoding.nvenc.tuning", "Допустимы tuning hq, ll и ull.")
	}
	if !slices.Contains([]string{"disabled", "qres", "fullres"}, n.Multipass) {
		return invalidEncoding("encoding.nvenc.multipass", "Допустимы multipass disabled, qres и fullres.")
	}
	if n.LookaheadFrames < 0 || n.LookaheadFrames > 31 || n.LookaheadFrames > 0 && uint64(n.LookaheadFrames)+uint64(ve.MaxBFrames) > 31 {
		return invalidEncoding("encoding.nvenc.lookahead_frames", "Допустимы 0..31; положительный lookahead вместе с B-кадрами не должен превышать 31.")
	}
	if n.AQStrength < 1 || n.AQStrength > 15 || !n.EnableSpatialAQ && n.AQStrength != 8 {
		return invalidEncoding("encoding.nvenc.aq_strength", "Допустимы 1..15; без пространственного AQ требуется значение 8.")
	}
	return nil
}

// validateSVTAV1 проверяет размеры, частоту кадров и настройки AV1, включая различные ограничения
// CBR/VBR; блок NVENC должен быть нулевым.
func (p GroupProfile) validateSVTAV1() error {
	// v содержит выходной видеоформат, ve — настройки видеобитрейта и GOP, а e — запрос кодировщиков
	// для проверки SVT-AV1.
	v, ve, e := p.Format.Video, p.VideoEncoding, p.Encoding
	if e.NVENC != (NVENCSettings{}) {
		return invalidEncoding("encoding.nvenc", "Блок nvenc неприменим к libsvtav1.")
	}
	if v.Width < 64 || v.Width > 16384 || v.Height < 64 || v.Height > 8704 || uint64(v.FrameRate.Numerator) > 240*uint64(v.FrameRate.Denominator) {
		return invalidEncoding("format.video", "SVT-AV1 требует ширину 64..16384, высоту 64..8704 и FPS не выше 240.")
	}
	if v.CodecProfile != "main" || v.Tier != "" && v.Tier != "main" || !validAV1Level(v.Level) {
		return invalidEncoding("format.video", "AV1 требует main, Tier пустой/main и Level auto либо 2.0..7.3 из поддерживаемого набора.")
	}
	if ve.GOPFrames < 2 || ve.MaxBFrames != 0 {
		return invalidEncoding("video_encoding.gop_frames", "SVT-AV1 требует GOP не меньше 2 и нулевое число B-кадров.")
	}
	if ve.TargetBitrateBPS == 0 || ve.TargetBitrateBPS > 100000000 {
		return invalidEncoding("video_encoding.target_bitrate_bps", "Битрейт AV1 должен быть от 1 до 100000000 бит/с.")
	}
	if e.RateControl == RateControlVBR {
		if ve.MaxBitrateBPS != 0 || ve.BufferBits != 0 {
			return invalidEncoding("video_encoding", "SVT-AV1 VBR не поддерживает заданный hard cap/VBV; max_bitrate_bps и buffer_bits должны быть 0.")
		}
	} else {
		// Предварительный предел делает последующее умножение на 1000 безопасным.
		if ve.MaxBitrateBPS != ve.TargetBitrateBPS || ve.BufferBits == 0 || ve.BufferBits > ve.TargetBitrateBPS*10 || ve.BufferBits*1000%ve.TargetBitrateBPS != 0 || ve.BufferBits*1000/ve.TargetBitrateBPS < 20 {
			return invalidEncoding("video_encoding", "AV1 CBR требует max=target и точный целый буфер 20..10000 мс.")
		}
	}
	// s содержит параметры SVT-AV1, проверяемые с учётом режима битрейта.
	s := e.SVTAV1
	if s.Preset < 0 || s.Preset > 13 {
		return invalidEncoding("encoding.svt_av1.preset", "Допустимы preset 0..13.")
	}
	if s.Tuning != "psnr" && s.Tuning != "vq" || e.RateControl == RateControlCBR && s.Tuning != "psnr" {
		return invalidEncoding("encoding.svt_av1.tuning", "Допустимы psnr/vq; CBR требует psnr.")
	}
	if s.AQMode != 2 {
		return invalidEncoding("encoding.svt_av1.aq_mode", "Поддерживается только aq_mode=2.")
	}
	if e.RateControl == RateControlCBR && s.LookaheadFrames != 0 || e.RateControl == RateControlVBR && s.LookaheadFrames != -1 && (s.LookaheadFrames < 6 || s.LookaheadFrames > 120) {
		return invalidEncoding("encoding.svt_av1.lookahead_frames", "CBR требует lookahead=0; VBR допускает auto (-1) либо 6..120.")
	}
	return nil
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
