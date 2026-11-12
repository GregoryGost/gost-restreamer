package api

import (
	"reflect"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/danielgtaylor/huma/v2"
)

// GroupProfileInput принимает полный профиль группы с независимым присутствием селектора LIVE-аудио и
// блока encoding.
type GroupProfileInput struct {
	// Mode задаёт обработку видео copy или transcode.
	Mode          string        `json:"mode" enum:"copy,transcode"`
	LiveAudioMode Field[string] `json:"live_audio_mode,omitempty" enum:"copy,transcode" doc:"Обработка LIVE-аудио независимо от видео. При создании по умолчанию copy. Прежний профиль без поля сохраняет свою обработку; полная замена уже явного профиля требует это поле. null запрещён."`
	// Format содержит полный ожидаемый формат видео и аудио.
	Format FormatDTO `json:"format"`
	// VideoEncoding содержит общие целевые параметры видеобитрейта и GOP.
	VideoEncoding VideoEncodingDTO `json:"video_encoding"`
	// AudioEncoding содержит общий целевой аудиобитрейт.
	AudioEncoding AudioEncodingDTO `json:"audio_encoding"`
	Encoding      NullableEncoding `json:"encoding,omitempty" doc:"Расширенные настройки кодирования версии 2. Отсутствие или null сохраняет прежний видеорецепт; удаление уже сохранённого блока версии 2 запрещено. Выбор LIVE-аудио задаётся отдельно."`
}

// EncodingInput принимает явный рецепт кодировщика версии 2 и отличает пропущенные необязательные
// настройки от null.
type EncodingInput struct {
	// Version задаёт формат блока encoding; поддерживается только версия 2.
	Version      int           `json:"version" enum:"2"`
	VideoEncoder string        `json:"video_encoder" enum:"h264_nvenc,libsvtav1" doc:"Серверный кодировщик: h264_nvenc для transcode/H264, libsvtav1 для подготовки copy/AV1. Основной copy-поток сервер не перекодирует."`
	AudioEncoder Field[string] `json:"audio_encoder,omitempty" enum:"aac" doc:"Кодировщик тишины заглушки и явно перекодируемого LIVE-аудио; по умолчанию aac. Копируемое LIVE-аудио не изменяется."`
	RateControl  string        `json:"rate_control" enum:"cbr,vbr" doc:"Явный режим управления битрейтом; равенство целевого и максимального битрейтов само по себе не выбирает CBR."`
	// RescaleOutput задаёт запрос масштабирования выходного видео отдельно от частоты кадров;
	// отсутствие материализуется по режиму группы.
	RescaleOutput Field[bool] `json:"rescale_output,omitempty" doc:"По умолчанию true для transcode, false для copy. false требует совпадения размеров входа и выхода. FPS задаётся профилем независимо."`
	// VideoCustom содержит строку дополнительных разрешённых параметров видеокодировщика,
	// нормализуемую перед сохранением.
	VideoCustom Field[string] `json:"video_custom,omitempty" maxLength:"4096" doc:"Пары name=value через ASCII-пробел. Каталог: NVENC nonref_p/strict_gop=0|1; SVT lp=0..6. Конфликты с обычными полями запрещены."`
	// AudioCustom содержит строку дополнительных разрешённых параметров AAC, нормализуемую перед
	// сохранением.
	AudioCustom Field[string]      `json:"audio_custom,omitempty" maxLength:"4096" doc:"Пары name=value через ASCII-пробел. Для AAC разрешён aac_coder=twoloop|fast."`
	NVENC       Field[NVENCInput]  `json:"nvenc,omitempty" doc:"Настройки только h264_nvenc; для другого кодировщика блок запрещён даже пустым."`
	SVTAV1      Field[SVTAV1Input] `json:"svt_av1,omitempty" doc:"Настройки только libsvtav1; для другого кодировщика блок запрещён даже пустым."`
}

// NVENCInput принимает присутствующие изменения настроек h264_nvenc поверх доменных значений по
// умолчанию.
type NVENCInput struct {
	// Preset выбирает предустановку NVENC p1..p7; отсутствие сохраняет доменное значение по
	// умолчанию.
	Preset Field[string] `json:"preset,omitempty" enum:"p1,p2,p3,p4,p5,p6,p7"`
	// Tuning выбирает назначение NVENC hq, ll или ull.
	Tuning Field[string] `json:"tuning,omitempty" enum:"hq,ll,ull"`
	// Multipass выбирает режим многопроходного NVENC disabled, qres или fullres.
	Multipass Field[string] `json:"multipass,omitempty" enum:"disabled,qres,fullres"`
	// LookaheadFrames задаёт запрошенное число кадров предварительного анализа NVENC; явный ноль
	// сохраняется.
	LookaheadFrames Field[int] `json:"lookahead_frames,omitempty" minimum:"0" maximum:"31" doc:"0 оставляет выбор кодировщику и preset, не гарантирует отключение. Для положительного L требуется L+B<=31."`
	// EnableSpatialAQ задаёт запрос дополнительного пространственного адаптивного квантования NVENC;
	// отсутствие и явное false различаются при применении.
	EnableSpatialAQ Field[bool] `json:"enable_spatial_aq,omitempty" doc:"false не включает дополнительный AQ поверх preset и не гарантирует очистку его флагов кодирования."`
	// EnableTemporalAQ задаёт запрос дополнительного временного адаптивного квантования NVENC;
	// отсутствие и явное false различаются при применении.
	EnableTemporalAQ Field[bool] `json:"enable_temporal_aq,omitempty" doc:"false не включает дополнительный AQ поверх preset и не гарантирует очистку его флагов кодирования."`
	// AQStrength задаёт интенсивность пространственного адаптивного квантования NVENC от 1 до 15.
	AQStrength Field[int] `json:"aq_strength,omitempty" minimum:"1" maximum:"15" doc:"Без пространственного AQ допускается только значение 8."`
}

// SVTAV1Input принимает присутствующие настройки подготовки AV1 через libsvtav1.
type SVTAV1Input struct {
	// Preset выбирает предустановку скорости SVT-AV1 от 0 до 13, сохраняя явный ноль.
	Preset Field[int] `json:"preset,omitempty" minimum:"0" maximum:"13"`
	// Tuning выбирает назначение настройки SVT-AV1 psnr или vq.
	Tuning Field[string] `json:"tuning,omitempty" enum:"psnr,vq" doc:"CBR допускает только psnr."`
	// AQMode задаёт поддержанный режим адаптивного квантования SVT-AV1, равный 2.
	AQMode          Field[int] `json:"aq_mode,omitempty" enum:"2"`
	LookaheadFrames Field[int] `json:"lookahead_frames,omitempty" minimum:"-1" maximum:"120" doc:"CBR: только 0, по умолчанию 0. VBR: -1 для автоматического выбора кодировщика либо 6..120, по умолчанию -1. Это запрос дистанции анализа, не измеренное эффективное значение."`
}

// TransformSchema публикует значения по умолчанию audio_encoder, video_custom и audio_custom без
// автоматического применения их к прежним профилям.
func (EncodingInput) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	setSchemaDefaults(s, map[string]any{"audio_encoder": "aac", "video_custom": "", "audio_custom": ""})
	return s
}

// TransformSchema задаёт документированные значения по умолчанию полей NVENC в схеме запроса.
func (NVENCInput) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	setSchemaDefaults(s, map[string]any{"preset": "p4", "tuning": "hq", "multipass": "disabled", "lookahead_frames": 0, "enable_spatial_aq": false, "enable_temporal_aq": false, "aq_strength": 8})
	return s
}

// TransformSchema задаёт общие значения по умолчанию SVT-AV1, оставляя зависимое от CBR/VBR поле
// lookahead_frames доменной обработке.
func (SVTAV1Input) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	setSchemaDefaults(s, map[string]any{"preset": 8, "tuning": "psnr", "aq_mode": 2})
	return s
}

// setSchemaDefaults добавляет значения по умолчанию уже существующим свойствам схемы.
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

// Schema описывает encoding как объект настроек либо явный null для прежнего видеорецепта.
func (NullableEncoding) Schema(r huma.Registry) *huma.Schema {
	return &huma.Schema{AnyOf: []*huma.Schema{r.Schema(reflect.TypeFor[EncodingInput](), true, ""), {Type: "null"}}}
}

// domain материализует настройки encoding версии 2 с учётом режима группы и явно переданных полей.
// Отсутствие и null сохраняют прежний рецепт; неприменимые блоки, null внутри настроек и недопустимые
// пары дополнительных параметров отклоняются.
func (e NullableEncoding) domain(mode core.GroupMode) (core.EncodingSettings, error) {
	if !e.Set || e.Null {
		return core.EncodingSettings{}, nil
	}
	// v содержит присутствующий входной рецепт до проверки версии, применимости блоков и
	// материализации.
	v := e.Value
	if v.Version != core.EncodingVersion {
		return core.EncodingSettings{}, profileInputError{"profile.encoding.version", "Объект encoding требует version=2."}
	}
	if v.NVENC.Set && v.VideoEncoder != core.VideoEncoderNVENC {
		return core.EncodingSettings{}, profileInputError{"profile.encoding.nvenc", "Блок nvenc применим только к h264_nvenc, включая пустой объект."}
	}
	if v.SVTAV1.Set && v.VideoEncoder != core.VideoEncoderSVTAV1 {
		return core.EncodingSettings{}, profileInputError{"profile.encoding.svt_av1", "Блок svt_av1 применим только к libsvtav1, включая пустой объект."}
	}
	// settings содержит доменные значения по умолчанию выбранного режима, которые затем изменяются
	// явно заданными допустимыми полями.
	settings := core.DefaultEncoding(mode, v.VideoEncoder, v.RateControl)
	// apply содержит очередное применение общего необязательного поля encoding с запретом null.
	for _, apply := range []func() error{
		func() error {
			// Применяет явно переданный AudioEncoder поверх значений по умолчанию и отклоняет null.
			return v.AudioEncoder.apply(&settings.AudioEncoder)
		},
		func() error {
			// Применяет явно переданный RescaleOutput, сохраняя различие отсутствующего поля и false.
			return v.RescaleOutput.apply(&settings.RescaleOutput)
		},
		func() error {
			// Применяет переданную строку VideoCustom и отклоняет null.
			return v.VideoCustom.apply(&settings.VideoCustom)
		},
		func() error {
			// Применяет переданную строку AudioCustom и отклоняет null.
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

// EncodingDTO возвращает материализованный рецепт версии 2 только с настройками применимого
// видеокодировщика.
type EncodingDTO struct {
	// Version содержит сохранённую версию рецепта encoding, равную 2.
	Version int `json:"version" enum:"2"`
	// VideoEncoder содержит выбранный серверный h264_nvenc или libsvtav1.
	VideoEncoder string `json:"video_encoder" enum:"h264_nvenc,libsvtav1"`
	// AudioEncoder содержит материализованный аудиокодировщик aac.
	AudioEncoder string `json:"audio_encoder" enum:"aac"`
	// RateControl содержит явный режим управления видеобитрейтом cbr или vbr.
	RateControl string `json:"rate_control" enum:"cbr,vbr"`
	// RescaleOutput содержит материализованный выбор масштабирования выходного видео.
	RescaleOutput bool `json:"rescale_output"`
	// VideoCustom содержит нормализованные дополнительные параметры видеокодировщика.
	VideoCustom string `json:"video_custom"`
	// AudioCustom содержит нормализованные дополнительные параметры AAC.
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
	// Tuning содержит выбранное назначение NVENC hq, ll или ull.
	Tuning string `json:"tuning"`
	// Multipass содержит выбранный режим многопроходного NVENC.
	Multipass string `json:"multipass"`
	// LookaheadFrames содержит запрошенное число кадров предварительного анализа NVENC, включая ноль
	// для выбора кодировщиком.
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
	// Tuning содержит выбранное назначение SVT-AV1 psnr или vq.
	Tuning string `json:"tuning"`
	// AQMode содержит выбранный режим адаптивного квантования SVT-AV1.
	AQMode int `json:"aq_mode"`
	// LookaheadFrames содержит запрошенную дистанцию анализа SVT-AV1, включая -1 для автоматического
	// выбора в VBR.
	LookaheadFrames int `json:"lookahead_frames"`
}

// TransformSchema сохраняет явную допустимость encoding:null в схеме ответа для прежних профилей.
func (GroupProfileDTO) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	s.Properties["encoding"] = &huma.Schema{AnyOf: []*huma.Schema{s.Properties["encoding"], {Type: "null"}}}
	return s
}

// encodingDTO возвращает null для прежнего рецепта либо полный материализованный блок только с
// применимыми настройками кодировщика.
func encodingDTO(e core.EncodingSettings) *EncodingDTO {
	if e == (core.EncodingSettings{}) {
		return nil
	}
	// v собирает публичный материализованный рецепт и добавляет только применимый вложенный блок.
	v := &EncodingDTO{Version: e.Version, VideoEncoder: e.VideoEncoder, AudioEncoder: e.AudioEncoder, RateControl: e.RateControl, RescaleOutput: e.RescaleOutput, VideoCustom: e.VideoCustom, AudioCustom: e.AudioCustom}
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
