package api

import (
	"bytes"
	"encoding/json"
	"reflect"
	"strconv"
	"strings"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/danielgtaylor/huma/v2"
)

// GroupProfileInput принимает полный профиль группы с независимым присутствием селектора LIVE-аудио и
// блока encoding.
type GroupProfileInput struct {
	// Mode задаёт обработку видео copy или transcode.
	Mode string `json:"mode" enum:"copy,transcode"`
	// LiveAudioMode сохраняет присутствие независимого селектора обработки LIVE-аудио.
	LiveAudioMode Field[string] `json:"live_audio_mode,omitempty" enum:"copy,transcode" doc:"Обработка LIVE-аудио независимо от видео. При создании по умолчанию copy; полная замена требует явного выбора. null запрещён."`
	// Format содержит полный ожидаемый формат видео и аудио.
	Format FormatDTO `json:"format" doc:"Полный желаемый формат. Применимость профиля, пикселей, цвета, качества и аудиопривязки проверяется по выбранным кодировщикам. UNKNOWN и unspecified запрещены."`
	// VideoEncoding содержит общие целевые параметры видеобитрейта и GOP.
	VideoEncoding VideoEncodingDTO `json:"video_encoding"`
	// AudioEncoding содержит общий целевой аудиобитрейт.
	AudioEncoding AudioEncodingDTO `json:"audio_encoding"`
	// Encoding выбирает явные настройки либо текущие значения по умолчанию.
	Encoding NullableEncoding `json:"encoding,omitempty" doc:"Текущие настройки кодировщика. Отсутствие или null материализует H264/transcode в NVENC/VBR либо AV1/copy в SVT/CBR; другие сочетания требуют явного выбора."`
}

// EncodingInput принимает текущие настройки кодировщика и отличает пропущенные поля от null.
type EncodingInput struct {
	// VideoEncoder выбирает серверный кодировщик, согласованный с режимом и кодеком.
	VideoEncoder string `json:"video_encoder" enum:"h264_nvenc,libsvtav1,libx264,libaom-av1,libx265,hevc_nvenc,libvpx-vp9" doc:"Серверный кодировщик, соответствующий кодеку. Основное видео copy не перекодируется; выбранный кодировщик готовит заглушку."`
	// AudioEncoder выбирает аудиорецепт; отсутствие оставляет AAC независимо от выходного кодека.
	AudioEncoder Field[string] `json:"audio_encoder,omitempty" enum:"aac,libopus,flac,libmp3lame,ac3" doc:"Кодировщик заглушки и явно перекодируемого LIVE-аудио. Отсутствие выбирает aac; другие привязки требуют явного выбора и полного согласованного формата."`
	// RateControl выбирает режим битрейта либо качества с независимой проверкой применимости.
	RateControl string `json:"rate_control" enum:"cbr,vbr,cqp,crf" doc:"CBR/VBR запрещают qp/crf. CQP/CRF требуют нулевых target/max/buffer и явно заданного соответствующего качества. Допустимые режимы определяются кодировщиком."`
	// QP содержит явно переданный целый квантователь; null запрещён.
	QP Field[int] `json:"qp,omitempty" minimum:"0" maximum:"63" doc:"Обязателен только при CQP. SVT: 1..63; x264/NVENC/x265/HEVC NVENC: 0..51. x264 QP=0 требует high444. VP9/AOM не поддерживают CQP."`
	// CRF хранит качество в сотых долях без округления исходного JSON-числа.
	CRF Field[CRFNumber] `json:"crf,omitempty" doc:"Точный JSON number с шагом 0.01, только при CRF. x264/x265: 0..51; SVT: целое 1..63; AOM/VP9: целое 0..63. x264 CRF=0 требует high444. null запрещён."`
	// RescaleOutput задаёт запрос масштабирования выходного видео отдельно от частоты кадров;
	// отсутствие материализуется по режиму группы.
	RescaleOutput Field[bool] `json:"rescale_output,omitempty" doc:"По умолчанию true для transcode, false для copy. false требует совпадения размеров входа и выхода. FPS задаётся профилем независимо."`
	// VideoCustom содержит строку дополнительных разрешённых параметров видеокодировщика,
	// нормализуемую перед сохранением.
	VideoCustom Field[string] `json:"video_custom,omitempty" maxLength:"4096" doc:"Пары name=value через ASCII-пробел. Каталог: NVENC nonref_p/strict_gop=0|1; SVT lp=0..6. Для x264, AOM, новых HEVC/VP9-кодировщиков и H264 NVENC CQP требуется пустая строка. Конфликты с обычными полями запрещены."`
	// AudioCustom содержит строку дополнительных разрешённых параметров AAC, нормализуемую перед
	// сохранением.
	AudioCustom Field[string] `json:"audio_custom,omitempty" maxLength:"4096" doc:"Для AAC разрешён aac_coder=twoloop|fast; другие аудиокодировщики требуют пустую строку."`
	// NVENC содержит применимые необязательные параметры NVIDIA.
	NVENC Field[NVENCInput] `json:"nvenc,omitempty" doc:"Настройки только h264_nvenc; для другого кодировщика блок запрещён даже пустым."`
	// SVTAV1 содержит применимые необязательные параметры SVT-AV1.
	SVTAV1 Field[SVTAV1Input] `json:"svt_av1,omitempty" doc:"Настройки только libsvtav1; для другого кодировщика блок запрещён даже пустым."`
}

// NVENCInput принимает присутствующие изменения настроек h264_nvenc поверх доменных значений по
// умолчанию.
type NVENCInput struct {
	// Preset выбирает предустановку NVENC p1..p7; отсутствие сохраняет доменное значение по
	// умолчанию.
	Preset Field[string] `json:"preset,omitempty" enum:"p1,p2,p3,p4,p5,p6,p7"`
	// Tuning выбирает назначение NVENC hq, ll, ull или lossless.
	Tuning Field[string] `json:"tuning,omitempty" enum:"hq,ll,ull,lossless" doc:"lossless требует high444/yuv444p/8-bit, CQP=0, p4, B=0, без AQ, lookahead, multipass и video_custom. При CQP допустимы hq или lossless."`
	// Multipass выбирает режим многопроходного NVENC disabled, qres или fullres.
	Multipass Field[string] `json:"multipass,omitempty" enum:"disabled,qres,fullres"`
	// LookaheadFrames задаёт запрошенное число кадров предварительного анализа NVENC; явный ноль
	// сохраняется.
	LookaheadFrames Field[int] `json:"lookahead_frames,omitempty" minimum:"0" maximum:"31" doc:"При H264 CQP требуется 0 с явным отключением анализа. В битрейтных режимах 0 оставляет выбор кодировщику и preset; положительный L требует L+B<=31."`
	// EnableSpatialAQ задаёт запрос дополнительного пространственного адаптивного квантования NVENC;
	// отсутствие и явное false различаются при применении.
	EnableSpatialAQ Field[bool] `json:"enable_spatial_aq,omitempty" doc:"При H264 CQP требуется false с явным отключением AQ. В битрейтных режимах false не включает дополнительный AQ поверх preset."`
	// EnableTemporalAQ задаёт запрос дополнительного временного адаптивного квантования NVENC;
	// отсутствие и явное false различаются при применении.
	EnableTemporalAQ Field[bool] `json:"enable_temporal_aq,omitempty" doc:"При H264 CQP требуется false с явным отключением AQ. В битрейтных режимах false не включает дополнительный AQ поверх preset."`
	// AQStrength задаёт интенсивность пространственного адаптивного квантования NVENC от 1 до 15.
	AQStrength Field[int] `json:"aq_strength,omitempty" minimum:"1" maximum:"15" doc:"Без пространственного AQ допускается только значение 8."`
}

// SVTAV1Input принимает присутствующие настройки подготовки AV1 через libsvtav1.
type SVTAV1Input struct {
	// Preset выбирает предустановку скорости SVT-AV1 от 0 до 13, сохраняя явный ноль.
	Preset Field[int] `json:"preset,omitempty" minimum:"0" maximum:"13"`
	// Tuning выбирает назначение настройки SVT-AV1 с учётом управления качеством.
	Tuning Field[string] `json:"tuning,omitempty" enum:"psnr,vq,ssim,ms_ssim,vmaf" doc:"CBR/CQP: только psnr; VBR: psnr/vq; CRF также допускает ssim/ms_ssim/vmaf."`
	// AQMode задаёт адаптивное квантование SVT: CQP требует 0, остальные режимы — 2.
	AQMode Field[int] `json:"aq_mode,omitempty" enum:"0,2" doc:"CQP требует и по умолчанию получает 0; CBR/VBR/CRF требуют и получают 2."`
	// LookaheadFrames задаёт глубину анализа со значением по умолчанию по режиму RC.
	LookaheadFrames Field[int] `json:"lookahead_frames,omitempty" minimum:"-1" maximum:"120" doc:"CBR: только 0, по умолчанию 0. VBR/CQP/CRF: -1 для автоматического выбора кодировщика либо 6..120, по умолчанию -1. Это запрос дистанции анализа, не измеренное эффективное значение."`
}

// TransformSchema публикует значения по умолчанию audio_encoder, video_custom и audio_custom для
// текущего входного объекта.
// s содержит схему входных настроек, в которую записываются значения по умолчанию.
func (EncodingInput) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	setSchemaDefaults(s, map[string]any{"audio_encoder": "aac", "video_custom": "", "audio_custom": ""})
	return s
}

// TransformSchema задаёт документированные значения по умолчанию полей NVENC в схеме запроса.
// s содержит схему запроса NVENC, в которую записываются значения по умолчанию.
func (NVENCInput) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	setSchemaDefaults(s, map[string]any{"preset": "p4", "tuning": "hq", "multipass": "disabled", "lookahead_frames": 0, "enable_spatial_aq": false, "enable_temporal_aq": false, "aq_strength": 8})
	return s
}

// TransformSchema задаёт в s значения по умолчанию SVT-AV1; AQ и lookahead зависят от RC.
func (SVTAV1Input) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	setSchemaDefaults(s, map[string]any{"preset": 8, "tuning": "psnr"})
	return s
}

// setSchemaDefaults добавляет defaults к уже существующим свойствам схемы s.
func setSchemaDefaults(s *huma.Schema, defaults map[string]any) {
	// name указывает существующее свойство JSON-схемы; value содержит публикуемое значение по
	// умолчанию.
	for name, value := range defaults {
		s.Properties[name].Default = value
	}
}

// NullableEncoding сохраняет различие отсутствующего блока encoding, явного null и переданного рецепта.
type NullableEncoding Field[EncodingInput]

// UnmarshalJSON отдельно сохраняет отсутствие, null и значение блока encoding через Field.
func (e *NullableEncoding) UnmarshalJSON(b []byte) error {
	return (*Field[EncodingInput])(e).UnmarshalJSON(b)
}

// Schema описывает объект настроек либо null как сокращённый запрос текущих значений по умолчанию.
// r предоставляет реестр Huma для схемы EncodingInput внутри допустимого объекта или null.
func (NullableEncoding) Schema(r huma.Registry) *huma.Schema {
	return &huma.Schema{AnyOf: []*huma.Schema{r.Schema(reflect.TypeFor[EncodingInput](), true, ""), {Type: "null"}}}
}

// domain материализует e для видеорежима mode и кодека codec. Отсутствие или null разрешены
// только для H264/transcode и AV1/copy; прочие сочетания требуют явных настроек.
// Неприменимые блоки, null внутри настроек и неверные дополнительные параметры отклоняются.
func (e NullableEncoding) domain(mode core.GroupMode, codec core.Codec) (core.EncodingSettings, error) {
	if !e.Set || e.Null {
		switch {
		case mode == core.ModeTranscode && codec == core.CodecH264:
			return core.DefaultEncoding(mode, core.VideoEncoderNVENC, core.RateControlVBR), nil
		case mode == core.ModeCopy && codec == core.CodecAV1:
			return core.DefaultEncoding(mode, core.VideoEncoderSVTAV1, core.RateControlCBR), nil
		default:
			return core.EncodingSettings{}, profileInputError{"profile.encoding", "Для этого сочетания видео требуется явный кодировщик и режим управления качеством или битрейтом."}
		}
	}
	// v содержит явно переданные настройки до проверки применимости вложенных блоков.
	v := e.Value
	if v.NVENC.Set && v.VideoEncoder != core.VideoEncoderNVENC {
		return core.EncodingSettings{}, profileInputError{"profile.encoding.nvenc", "Блок nvenc применим только к h264_nvenc, включая пустой объект."}
	}
	if v.SVTAV1.Set && v.VideoEncoder != core.VideoEncoderSVTAV1 {
		return core.EncodingSettings{}, profileInputError{"profile.encoding.svt_av1", "Блок svt_av1 применим только к libsvtav1, включая пустой объект."}
	}
	// settings содержит доменные значения по умолчанию выбранного режима, которые затем изменяются
	// явно заданными допустимыми полями.
	settings := core.DefaultEncoding(mode, v.VideoEncoder, v.RateControl)
	if v.QP.Set {
		if v.QP.Null {
			return core.EncodingSettings{}, profileInputError{"profile.encoding.qp", "null запрещён."}
		}
		settings.QP = core.OptionalQP{Set: true, Value: v.QP.Value}
	}
	if v.CRF.Set {
		if v.CRF.Null {
			return core.EncodingSettings{}, profileInputError{"profile.encoding.crf", "null запрещён."}
		}
		settings.CRF = core.OptionalCRF{Set: true, ValueHundredths: int(v.CRF.Value)}
	}
	// apply содержит очередное применение общего необязательного поля encoding с запретом null.
	for _, apply := range []func() error{
		// Применяет явно переданный AudioEncoder поверх значений по умолчанию и отклоняет null.
		func() error {
			return v.AudioEncoder.apply(&settings.AudioEncoder)
		},
		// Применяет явно переданный RescaleOutput, сохраняя различие отсутствующего поля и false.
		func() error {
			return v.RescaleOutput.apply(&settings.RescaleOutput)
		},
		// Применяет переданную строку VideoCustom и отклоняет null.
		func() error {
			return v.VideoCustom.apply(&settings.VideoCustom)
		},
		// Применяет переданную строку AudioCustom и отклоняет null.
		func() error {
			return v.AudioCustom.apply(&settings.AudioCustom)
		},
	} {
		// err содержит отказ применения явно переданного общего поля encoding.
		if err := apply(); err != nil {
			return core.EncodingSettings{}, err
		}
	}
	if v.NVENC.Set {
		if v.NVENC.Null {
			return core.EncodingSettings{}, core.ErrInvalid
		}
		// err содержит отказ применения вложенных настроек NVENC, включая явный null поля.
		if err := v.NVENC.Value.apply(&settings.NVENC); err != nil {
			return core.EncodingSettings{}, err
		}
	}
	if v.SVTAV1.Set {
		if v.SVTAV1.Null {
			return core.EncodingSettings{}, core.ErrInvalid
		}
		// err содержит отказ применения вложенных настроек SVT-AV1, включая явный null поля.
		if err := v.SVTAV1.Value.apply(&settings.SVTAV1); err != nil {
			return core.EncodingSettings{}, err
		}
	}
	// err принимает отказы нормализации дополнительных параметров видео и аудио.
	var err error
	if settings.VideoCustom, err = core.NormalizeCustom(settings.VideoEncoder, settings.VideoCustom); err != nil {
		return core.EncodingSettings{}, err
	}
	if settings.AudioCustom, err = core.NormalizeCustom(settings.AudioEncoder, settings.AudioCustom); err != nil {
		return core.EncodingSettings{}, err
	}
	return settings, nil
}

// profileInputError сохраняет проверенный публичный путь и подготовленное объяснение ошибочного поля
// профиля.
type profileInputError struct { /* field содержит публичный путь ошибочной настройки; detail содержит безопасное объяснение без отклонённого значения. */
	field, detail string
}

// Error возвращает подготовленное объяснение ошибки входного профиля.
func (e profileInputError) Error() string { return e.detail }

// Unwrap связывает ошибку входного профиля с доменным ErrInvalidProfile для HTTP-классификации.
func (profileInputError) Unwrap() error { return core.ErrInvalidProfile }

// apply переносит только присутствующие поля NVENC в материализованные настройки и отклоняет явные
// null.
// to указывает на материализованные настройки NVENC, изменяемые только присутствующими полями.
func (v NVENCInput) apply(to *core.NVENCSettings) error {
	// apply содержит очередной перенос присутствующей настройки NVENC с сохранением отсутствующего
	// поля.
	for _, apply := range []func() error{
		func() error {
			// Применяет переданный Preset NVENC, сохраняя прежнее значение при отсутствии поля.
			return v.Preset.apply(&to.Preset)
		},
		func() error {
			// Применяет переданный Tuning NVENC и отклоняет null.
			return v.Tuning.apply(&to.Tuning)
		},
		func() error {
			// Применяет переданный Multipass NVENC и отклоняет null.
			return v.Multipass.apply(&to.Multipass)
		},
		func() error {
			// Применяет переданный LookaheadFrames NVENC, включая явный ноль.
			return v.LookaheadFrames.apply(&to.LookaheadFrames)
		},
		func() error {
			// Применяет переданный EnableSpatialAQ, включая явное false.
			return v.EnableSpatialAQ.apply(&to.EnableSpatialAQ)
		},
		func() error {
			// Применяет переданный EnableTemporalAQ, включая явное false.
			return v.EnableTemporalAQ.apply(&to.EnableTemporalAQ)
		},
		func() error {
			// Применяет переданный AQStrength NVENC и отклоняет null.
			return v.AQStrength.apply(&to.AQStrength)
		},
	} {
		// err содержит отказ применения текущего поля NVENC, в частности переданного null.
		if err := apply(); err != nil {
			return err
		}
	}
	return nil
}

// apply переносит только присутствующие поля SVT-AV1 в материализованные настройки и отклоняет явные
// null.
// to указывает на материализованные настройки SVT-AV1, изменяемые только присутствующими полями.
func (v SVTAV1Input) apply(to *core.SVTAV1Settings) error {
	// apply содержит очередной перенос присутствующей настройки SVT-AV1 с сохранением отсутствующего
	// поля.
	for _, apply := range []func() error{
		func() error {
			// Применяет переданный Preset SVT-AV1, включая явный ноль.
			return v.Preset.apply(&to.Preset)
		},
		func() error {
			// Применяет переданный Tuning SVT-AV1 и отклоняет null.
			return v.Tuning.apply(&to.Tuning)
		},
		func() error {
			// Применяет переданный AQMode SVT-AV1 и отклоняет null.
			return v.AQMode.apply(&to.AQMode)
		},
		func() error {
			// Применяет переданный LookaheadFrames SVT-AV1, сохраняя прежнее значение при отсутствии поля.
			return v.LookaheadFrames.apply(&to.LookaheadFrames)
		},
	} {
		// err содержит отказ применения текущего поля SVT-AV1, в частности переданного null.
		if err := apply(); err != nil {
			return err
		}
	}
	return nil
}

// EncodingDTO возвращает материализованные настройки выбранного видеокодировщика.
type EncodingDTO struct {
	// VideoEncoder содержит выбранный серверный кодировщик H264, AV1, HEVC или VP9.
	VideoEncoder string `json:"video_encoder" enum:"h264_nvenc,libsvtav1,libx264,libaom-av1,libx265,hevc_nvenc,libvpx-vp9"`
	// AudioEncoder содержит сохранённый кодировщик выбранной аудиопривязки.
	AudioEncoder string `json:"audio_encoder" enum:"aac,libopus,flac,libmp3lame,ac3"`
	// RateControl содержит явный режим битрейта либо качества: cbr/vbr/cqp/crf.
	RateControl string `json:"rate_control" enum:"cbr,vbr,cqp,crf"`
	// QP содержит качество только при явно выбранном CQP.
	QP *int `json:"qp,omitempty" minimum:"0" maximum:"63"`
	// CRF содержит точное числовое качество с шагом 0.01 только при явно выбранном CRF.
	CRF *CRFNumber `json:"crf,omitempty"`
	// RescaleOutput содержит материализованный выбор масштабирования выходного видео.
	RescaleOutput bool `json:"rescale_output"`
	// VideoCustom содержит нормализованные дополнительные параметры видеокодировщика.
	VideoCustom string `json:"video_custom"`
	// AudioCustom содержит нормализованные параметры AAC; новые аудиорецепты требуют пустую строку.
	AudioCustom string `json:"audio_custom"`
	// NVENC содержит материализованные настройки только при h264_nvenc.
	NVENC *NVENCDTO `json:"nvenc,omitempty"`
	// SVTAV1 содержит материализованные настройки только при libsvtav1.
	SVTAV1 *SVTAV1DTO `json:"svt_av1,omitempty"`
}

// NVENCDTO возвращает полный сохранённый набор настроек h264_nvenc.
type NVENCDTO struct {
	// Preset содержит выбранную предустановку NVENC p1..p7.
	Preset string `json:"preset"`
	// Tuning содержит выбранное назначение NVENC hq, ll, ull или lossless.
	Tuning string `json:"tuning"`
	// Multipass содержит выбранный режим многопроходного NVENC.
	Multipass string `json:"multipass"`
	// LookaheadFrames содержит запрошенное число кадров предварительного анализа NVENC, включая ноль
	// для выбора кодировщиком в битрейтных режимах; CQP явно отключает анализ.
	LookaheadFrames int `json:"lookahead_frames"`
	// EnableSpatialAQ содержит запрос дополнительного пространственного адаптивного квантования.
	EnableSpatialAQ bool `json:"enable_spatial_aq"`
	// EnableTemporalAQ содержит запрос дополнительного временного адаптивного квантования.
	EnableTemporalAQ bool `json:"enable_temporal_aq"`
	// AQStrength содержит интенсивность пространственного адаптивного квантования NVENC.
	AQStrength int `json:"aq_strength"`
}

// SVTAV1DTO возвращает полный сохранённый набор настроек libsvtav1.
type SVTAV1DTO struct {
	// Preset содержит выбранную предустановку скорости SVT-AV1.
	Preset int `json:"preset"`
	// Tuning содержит выбранное назначение SVT-AV1 psnr/vq либо ssim/ms_ssim/vmaf для CRF.
	Tuning string `json:"tuning"`
	// AQMode содержит выбранный режим адаптивного квантования SVT-AV1.
	AQMode int `json:"aq_mode"`
	// LookaheadFrames содержит запрошенную дистанцию анализа SVT-AV1, включая -1 для автоматического
	// выбора в VBR/CQP/CRF.
	LookaheadFrames int `json:"lookahead_frames"`
}

// encodingDTO возвращает полный блок e только с применимыми настройками кодировщика;
// чтение не назначает значения по умолчанию.
func encodingDTO(e core.EncodingSettings) EncodingDTO {
	// v собирает публичный материализованный рецепт и добавляет только применимый вложенный блок.
	v := EncodingDTO{VideoEncoder: e.VideoEncoder, AudioEncoder: e.AudioEncoder, RateControl: e.RateControl, RescaleOutput: e.RescaleOutput, VideoCustom: e.VideoCustom, AudioCustom: e.AudioCustom}
	if e.QP.Set {
		v.QP = &e.QP.Value
	}
	if e.CRF.Set {
		// quality содержит точные сотые доли для числовой сериализации без float64.
		quality := CRFNumber(e.CRF.ValueHundredths)
		v.CRF = &quality
	}
	if e.VideoEncoder == core.VideoEncoderNVENC {
		// n содержит сохранённые настройки NVENC для преобразования всех полей в DTO.
		n := e.NVENC
		v.NVENC = &NVENCDTO{n.Preset, n.Tuning, n.Multipass, n.LookaheadFrames, n.EnableSpatialAQ, n.EnableTemporalAQ, n.AQStrength}
	}
	if e.VideoEncoder == core.VideoEncoderSVTAV1 {
		// s содержит сохранённые настройки SVT-AV1 для преобразования всех полей в DTO.
		s := e.SVTAV1
		v.SVTAV1 = &SVTAV1DTO{s.Preset, s.Tuning, s.AQMode, s.LookaheadFrames}
	}
	return v
}

// CRFNumber хранит публичное качество CRF целым числом сотых долей.
// JSON остаётся числом: исходная десятичная лексема проверяется до любых округлений.
type CRFNumber int

// UnmarshalJSON принимает data как JSON-число 0..63 с точным шагом 0.01.
// Масштабирование степени использует ограниченный разбор целых; память зависит от длины
// запроса, а не величины экспоненты. Получатель n меняется только после полной проверки.
func (n *CRFNumber) UnmarshalJSON(data []byte) error {
	// token сохраняет исходную лексему; coefficient и exponent задают эквивалентное число сотых.
	token := string(bytes.TrimSpace(data))
	if token == "" || (token[0] != '-' && (token[0] < '0' || token[0] > '9')) || !json.Valid([]byte(token)) {
		return profileInputError{"profile.encoding.crf", "Требуется JSON-число от 0 до 63 с шагом 0.01."}
	}
	// coefficient содержит мантиссу без степени; exponent включает масштаб сотых долей.
	coefficient, exponent := token, 2
	// index отмечает разделитель исходной десятичной степени, если она присутствует.
	if index := strings.IndexAny(token, "eE"); index >= 0 {
		coefficient = token[:index]
		exponent += boundedExponent(token[index+1:], len(token)+21)
	}
	// integer содержит точную запись сотых; whole подтверждает отсутствие долей меньше 0.01.
	integer, whole := exactIntegerToken(coefficient + "e" + strconv.Itoa(exponent))
	if !whole {
		return profileInputError{"profile.encoding.crf", "Требуется точный шаг CRF 0.01 без округления."}
	}
	// value содержит сотые доли CRF; err обозначает переполнение либо неверный диапазон.
	value, err := strconv.ParseInt(integer, 10, 16)
	if err != nil || value < 0 || value > 6300 {
		return profileInputError{"profile.encoding.crf", "Требуется CRF от 0 до 63."}
	}
	*n = CRFNumber(value)
	return nil
}

// MarshalJSON записывает n в исходных единицах CRF без промежуточного float64.
// Целые значения сохраняют прежний вид, сотые дополняются нулём слева и сокращаются справа.
func (n CRFNumber) MarshalJSON() ([]byte, error) {
	if n < 0 || n > 6300 {
		return nil, core.ErrInvalid
	}
	// integer содержит целую часть; fraction — две цифры сотых при непустой дробной части.
	integer := strconv.Itoa(int(n) / 100)
	fraction := int(n) % 100
	if fraction == 0 {
		return []byte(integer), nil
	}
	return []byte(integer + "." + strings.TrimRight(strconv.Itoa(100 + fraction)[1:], "0")), nil
}

// Schema публикует CRF как число 0..63 с шагом 0.01; кодировщик сужает этот набор.
func (CRFNumber) Schema(_ huma.Registry) *huma.Schema {
	return &huma.Schema{Type: huma.TypeNumber, Minimum: ptrFloat(0), Maximum: ptrFloat(63), Extensions: map[string]any{"multipleOf": json.Number("0.01")}}
}

// ptrFloat возвращает адрес отдельной числовой границы JSON-схемы.
func ptrFloat(value float64) *float64 { return &value }
