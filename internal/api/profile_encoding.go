package api

import (
	"reflect"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/danielgtaylor/huma/v2"
)

type GroupProfileInput struct {
	Mode          string           `json:"mode" enum:"copy,transcode"`
	LiveAudioMode Field[string]    `json:"live_audio_mode,omitempty" enum:"copy,transcode" doc:"Обработка LIVE-аудио независимо от видео. При создании по умолчанию copy. Прежний профиль без поля сохраняет свою обработку; полная замена уже явного профиля требует это поле. null запрещён."`
	Format        FormatDTO        `json:"format"`
	VideoEncoding VideoEncodingDTO `json:"video_encoding"`
	AudioEncoding AudioEncodingDTO `json:"audio_encoding"`
	Encoding      NullableEncoding `json:"encoding,omitempty" doc:"Расширенные настройки кодирования версии 2. Отсутствие или null сохраняет прежний видеорецепт; удаление уже сохранённого блока версии 2 запрещено. Выбор LIVE-аудио задаётся отдельно."`
}

type EncodingInput struct {
	Version       int                `json:"version" enum:"2"`
	VideoEncoder  string             `json:"video_encoder" enum:"h264_nvenc,libsvtav1" doc:"Серверный кодировщик: h264_nvenc для transcode/H264, libsvtav1 для подготовки copy/AV1. Основной copy-поток сервер не перекодирует."`
	AudioEncoder  Field[string]      `json:"audio_encoder,omitempty" enum:"aac" doc:"Кодировщик тишины заглушки и явно перекодируемого LIVE-аудио; по умолчанию aac. Копируемое LIVE-аудио не изменяется."`
	RateControl   string             `json:"rate_control" enum:"cbr,vbr" doc:"Явный режим управления битрейтом; равенство target и max само по себе не выбирает CBR."`
	RescaleOutput Field[bool]        `json:"rescale_output,omitempty" doc:"По умолчанию true для transcode, false для copy. false требует совпадения размеров входа и выхода. FPS задаётся профилем независимо."`
	VideoCustom   Field[string]      `json:"video_custom,omitempty" maxLength:"4096" doc:"Пары name=value через ASCII-пробел. Каталог: NVENC nonref_p/strict_gop=0|1; SVT lp=0..6. Конфликты с обычными полями запрещены."`
	AudioCustom   Field[string]      `json:"audio_custom,omitempty" maxLength:"4096" doc:"Пары name=value через ASCII-пробел. Для AAC разрешён aac_coder=twoloop|fast."`
	NVENC         Field[NVENCInput]  `json:"nvenc,omitempty" doc:"Настройки только h264_nvenc; для другого кодировщика блок запрещён даже пустым."`
	SVTAV1        Field[SVTAV1Input] `json:"svt_av1,omitempty" doc:"Настройки только libsvtav1; для другого кодировщика блок запрещён даже пустым."`
}

type NVENCInput struct {
	Preset           Field[string] `json:"preset,omitempty" enum:"p1,p2,p3,p4,p5,p6,p7"`
	Tuning           Field[string] `json:"tuning,omitempty" enum:"hq,ll,ull"`
	Multipass        Field[string] `json:"multipass,omitempty" enum:"disabled,qres,fullres"`
	LookaheadFrames  Field[int]    `json:"lookahead_frames,omitempty" minimum:"0" maximum:"31" doc:"0 оставляет выбор кодировщику и preset, не гарантирует отключение. Для положительного L требуется L+B<=31."`
	EnableSpatialAQ  Field[bool]   `json:"enable_spatial_aq,omitempty" doc:"false не включает дополнительный AQ поверх preset и не гарантирует очистку его флагов кодирования."`
	EnableTemporalAQ Field[bool]   `json:"enable_temporal_aq,omitempty" doc:"false не включает дополнительный AQ поверх preset и не гарантирует очистку его флагов кодирования."`
	AQStrength       Field[int]    `json:"aq_strength,omitempty" minimum:"1" maximum:"15" doc:"Без пространственного AQ допускается только значение 8."`
}

type SVTAV1Input struct {
	Preset          Field[int]    `json:"preset,omitempty" minimum:"0" maximum:"13"`
	Tuning          Field[string] `json:"tuning,omitempty" enum:"psnr,vq" doc:"CBR допускает только psnr."`
	AQMode          Field[int]    `json:"aq_mode,omitempty" enum:"2"`
	LookaheadFrames Field[int]    `json:"lookahead_frames,omitempty" minimum:"-1" maximum:"120" doc:"CBR: только 0, по умолчанию 0. VBR: -1 для автоматического выбора кодировщика либо 6..120, по умолчанию -1. Это запрос дистанции анализа, не измеренное эффективное значение."`
}

func (EncodingInput) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	setSchemaDefaults(s, map[string]any{"audio_encoder": "aac", "video_custom": "", "audio_custom": ""})
	return s
}
func (NVENCInput) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	setSchemaDefaults(s, map[string]any{"preset": "p4", "tuning": "hq", "multipass": "disabled", "lookahead_frames": 0, "enable_spatial_aq": false, "enable_temporal_aq": false, "aq_strength": 8})
	return s
}
func (SVTAV1Input) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	setSchemaDefaults(s, map[string]any{"preset": 8, "tuning": "psnr", "aq_mode": 2})
	return s
}
func setSchemaDefaults(s *huma.Schema, defaults map[string]any) {
	for name, value := range defaults {
		s.Properties[name].Default = value
	}
}

type NullableEncoding Field[EncodingInput]

func (e *NullableEncoding) UnmarshalJSON(b []byte) error {
	return (*Field[EncodingInput])(e).UnmarshalJSON(b)
}
func (NullableEncoding) Schema(r huma.Registry) *huma.Schema {
	return &huma.Schema{AnyOf: []*huma.Schema{r.Schema(reflect.TypeFor[EncodingInput](), true, ""), {Type: "null"}}}
}
func (e NullableEncoding) domain(mode core.GroupMode) (core.EncodingSettings, error) {
	if !e.Set || e.Null {
		return core.EncodingSettings{}, nil
	}
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
	settings := core.DefaultEncoding(mode, v.VideoEncoder, v.RateControl)
	for _, apply := range []func() error{
		func() error { return v.AudioEncoder.apply(&settings.AudioEncoder) },
		func() error { return v.RescaleOutput.apply(&settings.RescaleOutput) },
		func() error { return v.VideoCustom.apply(&settings.VideoCustom) },
		func() error { return v.AudioCustom.apply(&settings.AudioCustom) },
	} {
		if err := apply(); err != nil {
			return core.EncodingSettings{}, err
		}
	}
	if v.NVENC.Set {
		if v.NVENC.Null {
			return core.EncodingSettings{}, core.ErrInvalid
		}
		if err := v.NVENC.Value.apply(&settings.NVENC); err != nil {
			return core.EncodingSettings{}, err
		}
	}
	if v.SVTAV1.Set {
		if v.SVTAV1.Null {
			return core.EncodingSettings{}, core.ErrInvalid
		}
		if err := v.SVTAV1.Value.apply(&settings.SVTAV1); err != nil {
			return core.EncodingSettings{}, err
		}
	}
	var err error
	if settings.VideoCustom, err = core.NormalizeCustom(settings.VideoEncoder, settings.VideoCustom); err != nil {
		return core.EncodingSettings{}, err
	}
	if settings.AudioCustom, err = core.NormalizeCustom(settings.AudioEncoder, settings.AudioCustom); err != nil {
		return core.EncodingSettings{}, err
	}
	return settings, nil
}

type profileInputError struct{ field, detail string }

func (e profileInputError) Error() string { return e.detail }
func (profileInputError) Unwrap() error   { return core.ErrInvalidProfile }
func (v NVENCInput) apply(to *core.NVENCSettings) error {
	for _, apply := range []func() error{
		func() error { return v.Preset.apply(&to.Preset) },
		func() error { return v.Tuning.apply(&to.Tuning) },
		func() error { return v.Multipass.apply(&to.Multipass) },
		func() error { return v.LookaheadFrames.apply(&to.LookaheadFrames) },
		func() error { return v.EnableSpatialAQ.apply(&to.EnableSpatialAQ) },
		func() error { return v.EnableTemporalAQ.apply(&to.EnableTemporalAQ) },
		func() error { return v.AQStrength.apply(&to.AQStrength) },
	} {
		if err := apply(); err != nil {
			return err
		}
	}
	return nil
}
func (v SVTAV1Input) apply(to *core.SVTAV1Settings) error {
	for _, apply := range []func() error{
		func() error { return v.Preset.apply(&to.Preset) },
		func() error { return v.Tuning.apply(&to.Tuning) },
		func() error { return v.AQMode.apply(&to.AQMode) },
		func() error { return v.LookaheadFrames.apply(&to.LookaheadFrames) },
	} {
		if err := apply(); err != nil {
			return err
		}
	}
	return nil
}

type EncodingDTO struct {
	Version       int        `json:"version" enum:"2"`
	VideoEncoder  string     `json:"video_encoder" enum:"h264_nvenc,libsvtav1"`
	AudioEncoder  string     `json:"audio_encoder" enum:"aac"`
	RateControl   string     `json:"rate_control" enum:"cbr,vbr"`
	RescaleOutput bool       `json:"rescale_output"`
	VideoCustom   string     `json:"video_custom"`
	AudioCustom   string     `json:"audio_custom"`
	NVENC         *NVENCDTO  `json:"nvenc,omitempty"`
	SVTAV1        *SVTAV1DTO `json:"svt_av1,omitempty"`
}

type NVENCDTO struct {
	Preset           string `json:"preset"`
	Tuning           string `json:"tuning"`
	Multipass        string `json:"multipass"`
	LookaheadFrames  int    `json:"lookahead_frames"`
	EnableSpatialAQ  bool   `json:"enable_spatial_aq"`
	EnableTemporalAQ bool   `json:"enable_temporal_aq"`
	AQStrength       int    `json:"aq_strength"`
}
type SVTAV1DTO struct {
	Preset          int    `json:"preset"`
	Tuning          string `json:"tuning"`
	AQMode          int    `json:"aq_mode"`
	LookaheadFrames int    `json:"lookahead_frames"`
}

func (GroupProfileDTO) TransformSchema(_ huma.Registry, s *huma.Schema) *huma.Schema {
	s.Properties["encoding"] = &huma.Schema{AnyOf: []*huma.Schema{s.Properties["encoding"], {Type: "null"}}}
	return s
}
func encodingDTO(e core.EncodingSettings) *EncodingDTO {
	if e == (core.EncodingSettings{}) {
		return nil
	}
	v := &EncodingDTO{Version: e.Version, VideoEncoder: e.VideoEncoder, AudioEncoder: e.AudioEncoder, RateControl: e.RateControl, RescaleOutput: e.RescaleOutput, VideoCustom: e.VideoCustom, AudioCustom: e.AudioCustom}
	if e.VideoEncoder == core.VideoEncoderNVENC {
		n := e.NVENC
		v.NVENC = &NVENCDTO{n.Preset, n.Tuning, n.Multipass, n.LookaheadFrames, n.EnableSpatialAQ, n.EnableTemporalAQ, n.AQStrength}
	}
	if e.VideoEncoder == core.VideoEncoderSVTAV1 {
		s := e.SVTAV1
		v.SVTAV1 = &SVTAV1DTO{s.Preset, s.Tuning, s.AQMode, s.LookaheadFrames}
	}
	return v
}
