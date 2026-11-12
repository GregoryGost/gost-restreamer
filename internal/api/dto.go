package api

import (
	"bytes"
	"encoding/base64"
	"encoding/json"
	"reflect"
	"strconv"
	"time"
	"unicode/utf8"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/danielgtaylor/huma/v2"
)

// Decimal передаёт uint64 десятичной JSON-строкой для сохранения точности счётчиков и ревизий.
type Decimal uint64

// MarshalJSON передаёт uint64 десятичной JSON-строкой без преобразования в неточное число JavaScript.
func (d Decimal) MarshalJSON() ([]byte, error) {
	return json.Marshal(strconv.FormatUint(uint64(d), 10))
}

// UnmarshalJSON принимает только каноническую десятичную строку uint64 и изменяет Decimal после
// успешной проверки.
func (d *Decimal) UnmarshalJSON(b []byte) error {
	// s принимает строковое JSON-представление точного беззнакового числа.
	var s string
	// err содержит отказ чтения Decimal из JSON-строки.
	if err := json.Unmarshal(b, &s); err != nil {
		return core.ErrInvalid
	}
	// n содержит проверенное значение uint64; err содержит отказ канонической десятичной записи.
	n, err := parseDecimal(s)
	if err != nil {
		return err
	}
	*d = Decimal(n)
	return nil
}

// Schema описывает Decimal как десятичную строку; фактическую границу uint64 дополнительно проверяет
// UnmarshalJSON.
func (Decimal) Schema(huma.Registry) *huma.Schema {
	return &huma.Schema{Type: "string", Pattern: "^(0|[1-9][0-9]{0,19})$", Description: "Точное беззнаковое целое в десятичной записи."}
}

// parseDecimal разбирает uint64 из ASCII-цифр без знака и ведущих нулей; пустота и переполнение
// отклоняются.
func parseDecimal(s string) (uint64, error) {
	if s == "" || (len(s) > 1 && s[0] == '0') {
		return 0, core.ErrInvalid
	}
	// c содержит очередной символ числа, который должен быть ASCII-цифрой.
	for _, c := range s {
		if c < '0' || c > '9' {
			return 0, core.ErrInvalid
		}
	}
	// v содержит разобранное uint64; err содержит отказ числового преобразования или переполнение.
	v, err := strconv.ParseUint(s, 10, 64)
	if err != nil {
		return 0, core.ErrInvalid
	}
	return v, nil
}

// revision требует положительную каноническую десятичную ревизию, исключая ноль.
func revision(s string) (uint64, error) {
	// v содержит разобранную ревизию; err содержит отказ канонического числа до отдельного запрета
	// нуля.
	v, err := parseDecimal(s)
	if err != nil || v == 0 {
		return 0, core.ErrInvalid
	}
	return v, nil
}

// ProfileText сохраняет точные байты строковых параметров профиля, используя base64 для недопустимого
// UTF-8.
type ProfileText string

// MarshalJSON передаёт корректный UTF-8 строкой, а остальные байты ProfileText — объектом с base64 без
// изменения содержимого.
func (p ProfileText) MarshalJSON() ([]byte, error) {
	// s содержит точные байты ProfileText как строку для выбора UTF-8 или base64-представления.
	s := string(p)
	if utf8.ValidString(s) {
		return json.Marshal(s)
	}
	return json.Marshal(struct {
		// Encoding обозначает base64 в объектном представлении невалидных UTF-8 байтов профиля.
		Encoding string `json:"encoding"`
		// Value содержит base64-кодирование точных байтов ProfileText.
		Value string `json:"value"`
	}{"base64", base64.StdEncoding.EncodeToString([]byte(s))})
}

// UnmarshalJSON читает UTF-8 строку либо строгий объект base64 без неизвестных полей, сохраняя точные
// байты профиля.
func (p *ProfileText) UnmarshalJSON(b []byte) error {
	if len(b) > 0 && b[0] == '"' {
		// s принимает строковое UTF-8-представление профиля до изменения целевого значения.
		var s string
		if !utf8.Valid(b) || json.Unmarshal(b, &s) != nil {
			return core.ErrInvalid
		}
		*p = ProfileText(s)
		return nil
	}
	// v принимает объектное представление ProfileText с видом кодирования и закодированными байтами.
	var v struct {
		// Encoding должно содержать base64 для допуска объектного представления профиля.
		Encoding string `json:"encoding"`
		// Value содержит строку строгого base64, из которой восстанавливаются исходные байты.
		Value string `json:"value"`
	}
	// dec разбирает объект ProfileText с запрещёнными неизвестными полями.
	dec := json.NewDecoder(bytes.NewReader(b))
	dec.DisallowUnknownFields()
	if dec.Decode(&v) != nil || v.Encoding != "base64" {
		return core.ErrInvalid
	}
	// raw содержит восстановленные байты профиля; err содержит отказ строгого base64-декодирования.
	raw, err := base64.StdEncoding.Strict().DecodeString(v.Value)
	if err != nil {
		return core.ErrInvalid
	}
	*p = ProfileText(raw)
	return nil
}

// Schema описывает два допустимых представления ProfileText: строку и строгий объект base64.
func (ProfileText) Schema(huma.Registry) *huma.Schema {
	return &huma.Schema{OneOf: []*huma.Schema{{Type: "string"}, {Type: "object", Properties: map[string]*huma.Schema{"encoding": {Type: "string", Enum: []any{"base64"}}, "value": {Type: "string", ContentEncoding: "base64"}}, Required: []string{"encoding", "value"}, AdditionalProperties: false}}}
}

// Field различает отсутствующее поле частичного изменения, явный null и переданное значение целевого
// типа.
type Field[T any] struct {
	// Set отмечает присутствие JSON-поля; Null отдельно отмечает переданный null.
	Set, Null bool
	// Value хранит декодированное значение присутствующего ненулевого JSON-поля.
	Value T
}

// UnmarshalJSON отмечает наличие поля и отдельно фиксирует null; остальные значения декодирует в
// целевой тип.
func (f *Field[T]) UnmarshalJSON(b []byte) error {
	f.Set = true
	if bytes.Equal(bytes.TrimSpace(b), []byte("null")) {
		f.Null = true
		return nil
	}
	return json.Unmarshal(b, &f.Value)
}

// Schema описывает значение Field через схему его целевого типа, сохраняя отдельную обработку
// присутствия в Go.
func (Field[T]) Schema(r huma.Registry) *huma.Schema {
	// zero представляет нулевое значение целевого T для получения его типа при построении схемы
	// Field.
	var zero T
	return huma.SchemaFromType(r, reflect.TypeOf(zero))
}

// apply сохраняет целевое значение при отсутствии поля, отклоняет явный null и применяет переданное
// значение, включая нулевое.
func (f Field[T]) apply(to *T) error {
	if !f.Set {
		return nil
	}
	if f.Null {
		return core.ErrInvalid
	}
	*to = f.Value
	return nil
}

// RecordDTO передаёт общие метаданные сохранённой записи с точной ревизией и UTC-временами.
type RecordDTO struct {
	// ID содержит публичный идентификатор сохранённой записи.
	ID string `json:"id"`
	// Revision содержит точную ревизию для проверки конкурирующих изменений.
	Revision Decimal `json:"revision"`
	// CreatedAt содержит время создания записи в UTC.
	CreatedAt time.Time `json:"created_at"`
	// UpdatedAt содержит время последнего изменения записи в UTC.
	UpdatedAt time.Time `json:"updated_at"`
}

// recordDTO переводит идентификатор, точную ревизию и UTC-времена записи в публичное представление.
func recordDTO(r core.Record) RecordDTO {
	return RecordDTO{string(r.ID), Decimal(r.Revision), r.CreatedAt.UTC(), r.UpdatedAt.UTC()}
}

// FailureDTO сообщает безопасный код неуспешной операции без внутренней диагностики.
type FailureDTO struct {
	// Code содержит стабильное публичное обозначение причины неуспеха.
	Code string `json:"code"`
	// Message содержит безопасное пояснение с указанием журнала приложения для подробностей.
	Message string `json:"message"`
}

// failure создаёт безопасное описание неуспешной операции по коду; пустой код означает отсутствие
// ошибки.
func failure(code string) *FailureDTO {
	if code == "" {
		return nil
	}
	return &FailureDTO{code, "Операция не завершена успешно. Подробности доступны в журнале приложения."}
}

// WatchdogDTO передаёт сроки контроля источника точными десятичными наносекундами.
type WatchdogDTO struct {
	// VideoTimeoutNS задаёт допустимое отсутствие продвижения видео в наносекундах.
	VideoTimeoutNS Decimal `json:"video_timeout_ns"`
	// AudioTimeoutNS задаёт допустимое отсутствие продвижения аудио в наносекундах.
	AudioTimeoutNS Decimal `json:"audio_timeout_ns"`
	// RecoveryWindowNS задаёт окно подтверждения восстановления LIVE в наносекундах.
	RecoveryWindowNS Decimal `json:"recovery_window_ns"`
	// KeyframeTimeoutNS задаёт ожидание подходящего ключевого кадра в наносекундах.
	KeyframeTimeoutNS Decimal `json:"keyframe_timeout_ns"`
}

// domain проверяет положительные сроки контроля медиапотока и представимость каждого в time.Duration
// до преобразования.
func (w WatchdogDTO) domain() (core.WatchdogPolicy, error) {
	// n содержит очередной срок watchdog для проверки положительности и представимости в
	// time.Duration.
	for _, n := range []Decimal{w.VideoTimeoutNS, w.AudioTimeoutNS, w.RecoveryWindowNS, w.KeyframeTimeoutNS} {
		if n == 0 || uint64(n) > uint64(1<<63-1) {
			return core.WatchdogPolicy{}, core.ErrInvalid
		}
	}
	return core.WatchdogPolicy{VideoTimeout: time.Duration(w.VideoTimeoutNS), AudioTimeout: time.Duration(w.AudioTimeoutNS), RecoveryWindow: time.Duration(w.RecoveryWindowNS), KeyframeTimeout: time.Duration(w.KeyframeTimeoutNS)}, nil
}

// PolicyDTO связывает выбранную заглушку с индивидуальной политикой контроля LIVE потока.
type PolicyDTO struct {
	// AssetID указывает заглушку, используемую при недоступном LIVE.
	AssetID string `json:"asset_id"`
	// Watchdog содержит точные сроки обнаружения отказа и восстановления входа.
	Watchdog WatchdogDTO `json:"watchdog"`
}

// domain преобразует публичную политику заглушки и проверяет её сроки контроля медиапотока.
func (p PolicyDTO) domain() (core.FallbackPolicy, error) {
	// w содержит проверенную доменную политику watchdog; err содержит отказ преобразования её
	// сроков.
	w, err := p.Watchdog.domain()
	if err != nil {
		return core.FallbackPolicy{}, err
	}
	return core.FallbackPolicy{AssetID: core.ID(p.AssetID), Watchdog: w}, nil
}

// policyDTO возвращает политику с точными наносекундами либо null при отсутствии AssetID.
func policyDTO(p core.FallbackPolicy) *PolicyDTO {
	if p.AssetID == "" {
		return nil
	}
	return &PolicyDTO{string(p.AssetID), WatchdogDTO{Decimal(p.Watchdog.VideoTimeout), Decimal(p.Watchdog.AudioTimeout), Decimal(p.Watchdog.RecoveryWindow), Decimal(p.Watchdog.KeyframeTimeout)}}
}

// StreamDTO возвращает сохранённые настройки потока, постоянный ключ OBS и политику заглушки.
type StreamDTO struct {
	// RecordDTO содержит общие метаданные и ревизию Stream.
	RecordDTO
	// Name содержит сохранённое имя потока.
	Name           string `json:"name"`
	PublicationKey string `json:"publication_key" doc:"Сохранённый постоянный ключ публикации OBS. В Session возвращается ключ из снимка запуска."`
	// Fallback содержит выбранную политику заглушки либо null при отсутствии AssetID.
	Fallback *PolicyDTO `json:"fallback"`
}

// streamDTO возвращает сохранённый Stream вместе с полным ключом публикации и политикой заглушки.
func streamDTO(s core.Stream) StreamDTO {
	return StreamDTO{recordDTO(s.Record), s.Name, s.PublicationKey, policyDTO(s.Fallback)}
}

// DestinationDTO возвращает сохранённое назначение со связями, полными параметрами подключения и
// признаком включения.
type DestinationDTO struct {
	// RecordDTO содержит общие метаданные и ревизию назначения.
	RecordDTO
	// StreamID указывает поток, которому принадлежит назначение.
	StreamID string `json:"stream_id"`
	// GroupID указывает медиагруппу, выход которой получает назначение.
	GroupID string `json:"group_id"`
	// Name содержит сохранённое имя назначения.
	Name string `json:"name"`
	// Platform содержит сохранённое обозначение площадки назначения.
	Platform  string `json:"platform"`
	Endpoint  string `json:"endpoint" doc:"Полный сохранённый адрес назначения, включая учётные данные и параметры запроса. В Output возвращается адрес из снимка выхода."`
	StreamKey string `json:"stream_key" doc:"Сохранённый ключ назначения. Пустая строка допустима. В Output возвращается ключ из снимка выхода."`
	// Enabled содержит сохранённый признак включённого назначения.
	Enabled bool `json:"enabled"`
}

// destinationDTO возвращает полные сохранённые endpoint и stream_key, включая пустой ключ назначения.
func destinationDTO(d core.Destination) DestinationDTO {
	return DestinationDTO{recordDTO(d.Record), string(d.StreamID), string(d.GroupID), d.Name, d.Platform, d.Endpoint, d.StreamKey, d.Enabled}
}

// FrameRateDTO сохраняет исходную рациональную частоту кадров без сокращения дроби.
type FrameRateDTO struct {
	// Numerator содержит числитель частоты кадров в кадрах за секунду.
	Numerator uint32 `json:"numerator"`
	// Denominator содержит знаменатель исходной дроби частоты кадров.
	Denominator uint32 `json:"denominator"`
}

// ColorDTO сохраняет точные обозначения цветовых характеристик видео.
type ColorDTO struct {
	// Range содержит обозначение диапазона уровней цвета.
	Range ProfileText `json:"range"`
	// Primaries содержит обозначение цветовых первичных координат.
	Primaries ProfileText `json:"primaries"`
	// Transfer содержит обозначение передаточной характеристики цвета.
	Transfer ProfileText `json:"transfer"`
	// Matrix содержит обозначение матрицы преобразования цветовых компонентов.
	Matrix ProfileText `json:"matrix"`
}

// VideoFormatDTO передаёт полное описание сжатого видео с точными байтами строковых параметров.
type VideoFormatDTO struct {
	// Codec содержит идентификатор видеокодека.
	Codec string `json:"codec"`
	// Width содержит ширину видеокадра в пикселях.
	Width uint32 `json:"width"`
	// Height содержит высоту видеокадра в пикселях.
	Height uint32 `json:"height"`
	// FrameRate содержит несокращённую рациональную частоту кадров.
	FrameRate FrameRateDTO `json:"frame_rate"`
	// CodecProfile содержит точное обозначение профиля видеокодека.
	CodecProfile ProfileText `json:"codec_profile"`
	// Level содержит точное обозначение уровня видеокодека.
	Level ProfileText `json:"level"`
	// Tier содержит точное обозначение уровня производительности видеокодека, если оно задано.
	Tier ProfileText `json:"tier"`
	// PixelFormat содержит обозначение пиксельного формата видео.
	PixelFormat ProfileText `json:"pixel_format"`
	// BitDepth содержит глубину цветового компонента в битах.
	BitDepth uint8 `json:"bit_depth"`
	// Color содержит точные цветовые характеристики видео.
	Color ColorDTO `json:"color"`
}

// AudioFormatDTO передаёт полное описание сжатого аудио с точными байтами строковых параметров.
type AudioFormatDTO struct {
	// Codec содержит идентификатор аудиокодека.
	Codec string `json:"codec"`
	// CodecProfile содержит обозначение профиля аудиокодека.
	CodecProfile ProfileText `json:"codec_profile"`
	// SampleRateHz содержит частоту дискретизации аудио в герцах.
	SampleRateHz uint32 `json:"sample_rate_hz"`
	// Channels содержит число аудиоканалов.
	Channels uint16 `json:"channels"`
	// ChannelLayout содержит точное обозначение расположения каналов.
	ChannelLayout ProfileText `json:"channel_layout"`
}

// FormatDTO объединяет полные описания видео- и аудиодорожек публичного медиаформата.
type FormatDTO struct {
	// Video содержит параметры видеодорожки.
	Video VideoFormatDTO `json:"video"`
	// Audio содержит параметры аудиодорожки.
	Audio AudioFormatDTO `json:"audio"`
}

// formatDTO сохраняет точные численные параметры, дробь FPS и байты строковых полей медиаформата в
// HTTP DTO.
func formatDTO(f core.MediaFormat) FormatDTO {
	// v содержит исходные параметры видео; a содержит исходные параметры аудио для точного
	// преобразования в DTO.
	v, a := f.Video, f.Audio
	return FormatDTO{VideoFormatDTO{string(v.Codec), v.Width, v.Height, FrameRateDTO{v.FrameRate.Numerator, v.FrameRate.Denominator}, ProfileText(v.CodecProfile), ProfileText(v.Level), ProfileText(v.Tier), ProfileText(v.PixelFormat), v.BitDepth, ColorDTO{ProfileText(v.Color.Range), ProfileText(v.Color.Primaries), ProfileText(v.Color.Transfer), ProfileText(v.Color.Matrix)}}, AudioFormatDTO{string(a.Codec), ProfileText(a.CodecProfile), a.SampleRateHz, a.Channels, ProfileText(a.ChannelLayout)}}
}

// domain возвращает доменный медиаформат из DTO без нормализации дроби FPS и байтов ProfileText.
func (f FormatDTO) domain() core.MediaFormat {
	// v содержит публичные параметры видео; a содержит публичные параметры аудио для восстановления
	// доменного формата без нормализации.
	v, a := f.Video, f.Audio
	return core.MediaFormat{Video: core.VideoFormat{Codec: core.Codec(v.Codec), Width: v.Width, Height: v.Height, FrameRate: core.FrameRate{Numerator: v.FrameRate.Numerator, Denominator: v.FrameRate.Denominator}, CodecProfile: string(v.CodecProfile), Level: string(v.Level), Tier: string(v.Tier), PixelFormat: string(v.PixelFormat), BitDepth: v.BitDepth, Color: core.ColorInfo{Range: string(v.Color.Range), Primaries: string(v.Color.Primaries), Transfer: string(v.Color.Transfer), Matrix: string(v.Color.Matrix)}}, Audio: core.AudioFormat{Codec: core.Codec(a.Codec), CodecProfile: string(a.CodecProfile), SampleRateHz: a.SampleRateHz, Channels: a.Channels, ChannelLayout: string(a.ChannelLayout)}}
}

// VideoEncodingDTO передаёт общие параметры видеопотока отдельно от конкретного рецепта кодировщика.
type VideoEncodingDTO struct {
	// TargetBitrateBPS содержит целевой видеобитрейт в битах за секунду.
	TargetBitrateBPS Decimal `json:"target_bitrate_bps"`
	// MaxBitrateBPS содержит максимальный видеобитрейт в битах за секунду.
	MaxBitrateBPS Decimal `json:"max_bitrate_bps"`
	// BufferBits содержит размер буфера управления видеобитрейтом в битах.
	BufferBits Decimal `json:"buffer_bits"`
	// GOPFrames содержит длину группы кадров в видеокадрах.
	GOPFrames uint32 `json:"gop_frames"`
	// MaxBFrames содержит максимальное число B-кадров.
	MaxBFrames uint32 `json:"max_b_frames"`
	// ClosedGOP содержит признак закрытой группы кадров.
	ClosedGOP bool `json:"closed_gop"`
}

// AudioEncodingDTO передаёт общий аудиобитрейт независимо от конкретного рецепта.
type AudioEncodingDTO struct {
	// BitrateBPS содержит целевой аудиобитрейт в битах за секунду.
	BitrateBPS Decimal `json:"bitrate_bps"`
}

// GroupProfileDTO возвращает полный сохранённый профиль группы без назначения новых значений прежним
// снимкам.
type GroupProfileDTO struct {
	// Mode содержит режим видео copy или transcode.
	Mode string `json:"mode" enum:"copy,transcode"`
	// LiveAudioMode содержит самостоятельный режим LIVE-аудио; отсутствие значения сохраняет прежнюю
	// семантику профиля.
	LiveAudioMode string `json:"live_audio_mode,omitempty" enum:"copy,transcode"`
	// Format содержит полный ожидаемый медиаформат обеих дорожек.
	Format FormatDTO `json:"format"`
	// VideoEncoding содержит общие параметры видеобитрейта и GOP.
	VideoEncoding VideoEncodingDTO `json:"video_encoding"`
	// AudioEncoding содержит общий параметр аудиобитрейта.
	AudioEncoding AudioEncodingDTO `json:"audio_encoding"`
	Encoding      *EncodingDTO     `json:"encoding" doc:"Материализованные настройки кодировщика. null обозначает прежний видеорецепт; выбор LIVE-аудио задаётся отдельно."`
}

// domain проверяет явный селектор LIVE-аудио и преобразует настройки профиля, сохраняя отсутствие
// селектора для доменных правил совместимости.
func (p GroupProfileInput) domain() (core.GroupProfile, error) {
	// liveAudioMode сохраняет отсутствие селектора либо проверенный явный режим LIVE-аудио.
	var liveAudioMode core.LiveAudioMode
	if p.LiveAudioMode.Set {
		if p.LiveAudioMode.Null || p.LiveAudioMode.Value != string(core.LiveAudioCopy) && p.LiveAudioMode.Value != string(core.LiveAudioTranscode) {
			return core.GroupProfile{}, profileInputError{"profile.live_audio_mode", "Требуется явное значение copy или transcode."}
		}
		liveAudioMode = core.LiveAudioMode(p.LiveAudioMode.Value)
	}
	// encoding содержит проверенные настройки конкретного кодировщика; err содержит отказ их
	// преобразования для выбранного режима видео.
	encoding, err := p.Encoding.domain(core.GroupMode(p.Mode))
	if err != nil {
		return core.GroupProfile{}, err
	}
	return core.GroupProfile{Mode: core.GroupMode(p.Mode), LiveAudioMode: liveAudioMode, Format: p.Format.domain(), VideoEncoding: core.VideoEncoding{TargetBitrateBPS: uint64(p.VideoEncoding.TargetBitrateBPS), MaxBitrateBPS: uint64(p.VideoEncoding.MaxBitrateBPS), BufferBits: uint64(p.VideoEncoding.BufferBits), GOPFrames: p.VideoEncoding.GOPFrames, MaxBFrames: p.VideoEncoding.MaxBFrames, ClosedGOP: p.VideoEncoding.ClosedGOP}, AudioEncoding: core.AudioEncoding{BitrateBPS: uint64(p.AudioEncoding.BitrateBPS)}, Encoding: encoding}, nil
}

// groupProfileDTO переводит полный профиль в HTTP DTO без назначения новых настроек прежним
// сохранённым профилям.
func groupProfileDTO(p core.GroupProfile) GroupProfileDTO {
	// v содержит общие настройки видеокодирования для возврата точных значений DTO.
	v := p.VideoEncoding
	return GroupProfileDTO{string(p.Mode), string(p.LiveAudioMode), formatDTO(p.Format), VideoEncodingDTO{Decimal(v.TargetBitrateBPS), Decimal(v.MaxBitrateBPS), Decimal(v.BufferBits), v.GOPFrames, v.MaxBFrames, v.ClosedGOP}, AudioEncodingDTO{Decimal(p.AudioEncoding.BitrateBPS)}, encodingDTO(p.Encoding)}
}

// ProfileDTO возвращает имя, метаданные и полный сохранённый профиль кодирования.
type ProfileDTO struct {
	// RecordDTO содержит общие метаданные и ревизию профиля.
	RecordDTO
	// Name содержит сохранённое имя профиля.
	Name string `json:"name"`
	// Profile содержит полный сохранённый набор параметров обработки группы.
	Profile GroupProfileDTO `json:"profile"`
}

// profileDTO возвращает метаданные и полный сохранённый профиль кодирования.
func profileDTO(p core.TranscodeProfile) ProfileDTO {
	return ProfileDTO{recordDTO(p.Record), p.Name, groupProfileDTO(p.Profile)}
}

// GroupDTO возвращает именованную связь потока с профилем кодирования.
type GroupDTO struct {
	// RecordDTO содержит общие метаданные и ревизию группы.
	RecordDTO
	// StreamID указывает поток группы.
	StreamID string `json:"stream_id"`
	// ProfileID указывает профиль обработки группы.
	ProfileID string `json:"profile_id"`
	// Name содержит сохранённое имя группы.
	Name string `json:"name"`
}

// groupDTO возвращает публичные метаданные группы и её связи с потоком и профилем.
func groupDTO(g core.TranscodeGroup) GroupDTO {
	return GroupDTO{recordDTO(g.Record), string(g.StreamID), string(g.ProfileID), g.Name}
}

// GroupSnapshotDTO возвращает самостоятельные снимки группы и её профиля из запуска Session.
type GroupSnapshotDTO struct {
	// Group содержит сохранённые при запуске метаданные группы.
	Group GroupDTO `json:"group"`
	// Profile содержит сохранённый при запуске полный профиль этой группы.
	Profile ProfileDTO `json:"profile"`
}

// SessionDTO возвращает долговечное состояние Session и самостоятельные снимки настроек её запуска.
type SessionDTO struct {
	// RecordDTO содержит общие метаданные и ревизию Session.
	RecordDTO
	// StreamID указывает исходный Stream этой Session.
	StreamID string `json:"stream_id"`
	// State содержит долговечное состояние жизненного цикла Session.
	State string `json:"state"`
	// DesiredPaused содержит желаемую ручную паузу, отдельно от текущего наблюдения медиатракта.
	DesiredPaused bool `json:"desired_paused"`
	// Stream содержит снимок настроек Stream и постоянного ключа на момент запуска.
	Stream StreamDTO `json:"stream"`
	// Groups содержит снимки выбранных групп и профилей на момент запуска.
	Groups []GroupSnapshotDTO `json:"groups"`
	// EndedAt содержит UTC-время завершения Session либо null.
	EndedAt *time.Time `json:"ended_at"`
	// Failure содержит безопасную причину FAILED или INTERRUPTED либо null.
	Failure *FailureDTO `json:"failure"`
}

// sessionDTO возвращает сохранённую Session со снимками Stream и групп на момент запуска. Терминальные
// отказы заменяются безопасными кодами без исходной диагностики.
func sessionDTO(s core.Session) SessionDTO {
	// groups собирает публичные снимки групп в сохранённом порядке Session.
	groups := make([]GroupSnapshotDTO, 0, len(s.Snapshot.Groups))
	// g содержит очередную пару сохранённых группы и профиля запуска.
	for _, g := range s.Snapshot.Groups {
		groups = append(groups, GroupSnapshotDTO{groupDTO(g.Group), profileDTO(g.Profile)})
	}
	// f содержит безопасную терминальную причину Session либо nil при отсутствии отказа.
	var f *FailureDTO
	if s.State == core.SessionFailed {
		f = failure("session_failed")
	}
	if s.State == core.SessionInterrupted {
		f = failure("session_interrupted")
	}
	return SessionDTO{recordDTO(s.Record), string(s.StreamID), string(s.State), s.DesiredPaused, streamDTO(s.Snapshot.Stream), groups, utcPointer(s.EndedAt), f}
}

// OutputDTO возвращает самостоятельный сохранённый снимок выхода и его желаемое состояние.
type OutputDTO struct {
	// RecordDTO содержит общие метаданные и ревизию выхода.
	RecordDTO
	// SessionID указывает Session, к которой прикреплён выход.
	SessionID string `json:"session_id"`
	// Destination содержит самостоятельный снимок полного подключения назначения.
	Destination DestinationDTO `json:"destination"`
	// Group содержит снимок группы, выбранной для выхода.
	Group GroupDTO `json:"group"`
	// Profile содержит самостоятельный полный снимок профиля выхода.
	Profile ProfileDTO `json:"profile"`
	// Fallback содержит сохранённую политику заглушки выхода либо null.
	Fallback *PolicyDTO `json:"fallback"`
	// DesiredRunning содержит желаемое состояние работы выхода.
	DesiredRunning bool `json:"desired_running"`
	// Attached показывает сохранённое прикрепление выхода к Session.
	Attached bool `json:"attached"`
}

// outputDTO возвращает самостоятельный сохранённый снимок Output и его желаемое состояние, не подменяя
// настройки текущей конфигурацией.
func outputDTO(o core.SessionOutput) OutputDTO {
	return OutputDTO{recordDTO(o.Record), string(o.SessionID), destinationDTO(o.Snapshot.Destination), groupDTO(o.Snapshot.Group), profileDTO(o.Snapshot.Profile), policyDTO(o.Snapshot.Fallback), o.DesiredRunning, o.Attached}
}

// ReceiptDTO возвращает долговечный статус принятой команды с публичными идентификаторами и безопасным
// отказом.
type ReceiptDTO struct {
	// RecordDTO содержит общие метаданные и ревизию квитанции.
	RecordDTO
	// RequestID содержит идентификатор смыслового запроса для идемпотентного повторения команды.
	RequestID string `json:"request_id"`
	// Kind содержит вид принятой команды.
	Kind string `json:"kind"`
	// TargetID указывает целевой объект команды.
	TargetID string `json:"target_id"`
	// SessionID указывает Session принятой команды.
	SessionID string `json:"session_id"`
	// OutputID указывает выход команды, если она относится к выходу.
	OutputID string `json:"output_id"`
	// Status содержит долговечное состояние исполнения команды.
	Status string `json:"status"`
	// Failure содержит безопасное обозначение неуспеха команды либо null.
	Failure *FailureDTO `json:"failure"`
}

// receiptDTO возвращает долговечный статус команды и безопасную причину неуспеха без внутренних
// признаков владения и диагностики.
func receiptDTO(r core.CommandReceipt) ReceiptDTO {
	// f содержит безопасный отказ FAILED или INTERRUPTED без внутренней диагностики квитанции.
	var f *FailureDTO
	if r.Status == core.CommandFailed {
		f = failure("command_failed")
	}
	if r.Status == core.CommandInterrupted {
		f = failure("command_interrupted")
	}
	return ReceiptDTO{recordDTO(r.Record), r.RequestID, string(r.Kind), string(r.TargetID), string(r.SessionID), string(r.OutputID), string(r.Status), f}
}

// AssetDTO возвращает метаданные заглушки и раздельные ссылки текущего и ожидающего поколения.
type AssetDTO struct {
	// RecordDTO содержит общие метаданные и ревизию заглушки.
	RecordDTO
	// Name содержит сохранённое имя заглушки.
	Name string `json:"name"`
	// Kind содержит вид исходника заглушки.
	Kind string `json:"kind"`
	// State содержит сохранённое состояние заглушки.
	State string `json:"state"`
	// CurrentGenerationID указывает опубликованное текущее поколение либо пустую строку.
	CurrentGenerationID string `json:"current_generation_id"`
	// PendingGenerationID указывает ожидающее поколение новой подготовки либо пустую строку.
	PendingGenerationID string `json:"pending_generation_id"`
	// SizeBytes содержит точный размер сохранённого исходника в байтах.
	SizeBytes Decimal `json:"size_bytes"`
}

// assetDTO возвращает метаданные заглушки и раздельные идентификаторы текущего и ожидающего поколения
// без серверных путей.
func assetDTO(v core.FallbackAsset) AssetDTO {
	return AssetDTO{recordDTO(v.Record), v.Name, string(v.Kind), string(v.State), string(v.CurrentGenerationID), string(v.PendingGenerationID), Decimal(v.SizeBytes)}
}

// GenerationDTO возвращает точный результат подготовки поколения без серверных путей и внутренних
// полномочий.
type GenerationDTO struct {
	// RecordDTO содержит общие метаданные и ревизию поколения.
	RecordDTO
	// AssetID указывает родительскую заглушку поколения.
	AssetID string `json:"asset_id"`
	// Kind содержит вид исходника текущей подготовки.
	Kind string `json:"kind"`
	// Status содержит публичную стадию queued, preparing, ready или error.
	Status string `json:"status"`
	// SizeBytes содержит точный размер исходника поколения в байтах.
	SizeBytes Decimal `json:"size_bytes"`
	// Failure содержит безопасную причину неуспешной подготовки либо null.
	Failure *FailureDTO `json:"failure"`
}

// generationDTO переводит долговечные состояния подготовки в queued, preparing, ready или error,
// сохраняя безопасную причину неуспеха.
func generationDTO(v core.FallbackGeneration) GenerationDTO {
	// status накапливает публичную стадию из долговечного состояния подготовки, начиная с error.
	status := "error"
	// f содержит безопасный отказ failed или interrupted для публичного результата подготовки.
	var f *FailureDTO
	switch v.State {
	case core.PreparationQueued:
		status = "queued"
	case core.PreparationClaimed, core.PreparationIssued:
		status = "preparing"
	case core.PreparationPublished:
		status = "ready"
	case core.PreparationFailed:
		f = failure("preparation_failed")
	case core.PreparationInterrupted:
		f = failure("preparation_interrupted")
	}
	return GenerationDTO{recordDTO(v.Record), string(v.AssetID), string(v.Kind), status, Decimal(v.SizeBytes), f}
}

// VariantDTO возвращает полный профиль, проверенный формат и размер варианта заглушки.
type VariantDTO struct {
	// RecordDTO содержит общие метаданные и ревизию варианта.
	RecordDTO
	// AssetID указывает заглушку, которой принадлежит вариант.
	AssetID string `json:"asset_id"`
	// GenerationID указывает точное поколение варианта.
	GenerationID string `json:"generation_id"`
	// State содержит сохранённое состояние готовности варианта.
	State string `json:"state"`
	// Profile содержит полный профиль, по которому подготовлен вариант.
	Profile GroupProfileDTO `json:"profile"`
	// Format содержит фактически проверенный медиаформат файла варианта.
	Format FormatDTO `json:"format"`
	// SizeBytes содержит точный размер файла варианта в байтах.
	SizeBytes Decimal `json:"size_bytes"`
}

// variantDTO возвращает профиль, проверенный формат и точный размер варианта, исключая файловые пути и
// внутренние ключи артефакта.
func variantDTO(v core.FallbackVariant) VariantDTO {
	return VariantDTO{recordDTO(v.Record), string(v.AssetID), string(v.GenerationID), string(v.State), groupProfileDTO(v.Profile), formatDTO(v.VerifiedFormat), Decimal(v.SizeBytes)}
}

// PageDTO возвращает упорядоченные элементы ограниченной страницы и позицию следующего чтения.
type PageDTO[T any] struct {
	// Items содержит элементы страницы; пустой результат кодируется пустым массивом.
	Items []T `json:"items"`
	// NextCursor содержит непрозрачную позицию следующей страницы либо пустую строку.
	NextCursor string `json:"next_cursor"`
}

// pageDTO преобразует элементы страницы, сохраняя порядок и следующий cursor; пустой результат
// передаётся массивом.
func pageDTO[A, B any](p core.Page[A], convert func(A) B) PageDTO[B] {
	// items собирает преобразованные элементы в исходном порядке и сохраняет срез длины ноль вместо
	// nil для пустого JSON-массива.
	items := make([]B, 0, len(p.Items))
	// v содержит очередную исходную запись страницы для преобразования в публичный тип.
	for _, v := range p.Items {
		items = append(items, convert(v))
	}
	return PageDTO[B]{items, p.NextCursor}
}

// utcPointer возвращает отдельное UTC-значение времени либо nil, не изменяя переданный указатель.
func utcPointer(t *time.Time) *time.Time {
	if t == nil {
		return nil
	}
	// v содержит отдельное UTC-значение ненулевого указателя времени без изменения исходного
	// объекта.
	v := t.UTC()
	return &v
}

// timestamp возвращает UTC-указатель только для известного времени; нулевое время становится null.
func timestamp(t time.Time) *time.Time {
	if t.IsZero() {
		return nil
	}
	// v содержит отдельное UTC-представление известного ненулевого времени для JSON.
	v := t.UTC()
	return &v
}

// NullablePolicy различает отсутствующую политику, явный null для очистки и переданное значение
// политики.
type NullablePolicy Field[PolicyDTO]

// UnmarshalJSON сохраняет присутствие и null политики через общий механизм Field.
func (p *NullablePolicy) UnmarshalJSON(b []byte) error {
	return (*Field[PolicyDTO])(p).UnmarshalJSON(b)
}

// Schema явно допускает null для очистки политики заглушки.
func (NullablePolicy) Schema(r huma.Registry) *huma.Schema {
	return &huma.Schema{AnyOf: []*huma.Schema{r.Schema(reflect.TypeFor[PolicyDTO](), true, ""), {Type: "null"}}}
}
