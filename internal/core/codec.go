package core

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
)

// MetadataVersion задаёт версию общего представления доменных метаданных.
const MetadataVersion = 1

// profileVersionV3 задаёт версию профиля с явным режимом обработки LIVE-аудио.
const profileVersionV3 = 3

// MetadataError описывает отказ чтения или проверки конкретной версии метаданных.
type MetadataError struct {
	// Kind указывает вид метаданных, чтение или проверка которых завершились отказом.
	Kind string
	// Version содержит версию сохраняемого представления.
	Version int
	// Cause сохраняет исходную причину ошибки метаданных.
	Cause error
}

// Error указывает вид метаданных, их версию и причину отказа.
func (e *MetadataError) Error() string {
	return fmt.Sprintf("метаданные %s, версия %d: %v", e.Kind, e.Version, e.Cause)
}

// Unwrap возвращает исходную причину ошибки метаданных.
func (e *MetadataError) Unwrap() error { return e.Cause }

// Is позволяет распознавать любую MetadataError как ErrInvalidMetadata.
func (e *MetadataError) Is(target error) bool { return target == ErrInvalidMetadata }

// videoMetadata сохраняет параметры видеодорожки, представляя строковые параметры точными байтами.
type videoMetadata struct {
	// Codec указывает кодек сжатой дорожки.
	Codec Codec
	// Width содержит ширину видеокадра в пикселях.
	Width uint32
	// Height содержит высоту видеокадра в пикселях.
	Height uint32
	// FrameRate задаёт частоту видеокадров точной положительной дробью.
	FrameRate FrameRate
	// CodecProfile сохраняет точные байты параметра CodecProfile без нормализации текста.
	CodecProfile []byte
	// Level сохраняет точные байты параметра Level без нормализации текста.
	Level []byte
	// Tier сохраняет точные байты параметра Tier без нормализации текста.
	Tier []byte
	// PixelFormat сохраняет точные байты параметра PixelFormat без нормализации текста.
	PixelFormat []byte
	// BitDepth содержит глубину компонента пикселя в битах.
	BitDepth uint8
	// Range сохраняет точные байты параметра Range без нормализации текста.
	Range []byte
	// Primaries сохраняет точные байты параметра Primaries без нормализации текста.
	Primaries []byte
	// Transfer сохраняет точные байты параметра Transfer без нормализации текста.
	Transfer []byte
	// Matrix сохраняет точные байты параметра Matrix без нормализации текста.
	Matrix []byte
}

// audioMetadata сохраняет параметры аудиодорожки с точными байтами строковых параметров.
type audioMetadata struct {
	// Codec указывает кодек сжатой дорожки.
	Codec Codec
	// CodecProfile сохраняет точные байты параметра CodecProfile без нормализации текста.
	CodecProfile []byte
	// SampleRateHz содержит частоту дискретизации аудио в герцах.
	SampleRateHz uint32
	// Channels содержит число аудиоканалов.
	Channels uint16
	// ChannelLayout сохраняет точные байты параметра ChannelLayout без нормализации текста.
	ChannelLayout []byte
}

// formatMetadata объединяет версию формата и сохраняемые параметры обеих дорожек.
type formatMetadata struct {
	// Version содержит версию сохраняемого представления.
	Version int
	// Video содержит описание обязательной видеодорожки.
	Video videoMetadata
	// Audio содержит описание обязательной аудиодорожки.
	Audio audioMetadata
}

// profileMetadataV1 сохраняет точный порядок и типы полей прежнего формата хранения.
type profileMetadataV1 struct {
	// Version содержит версию сохраняемого представления.
	Version int
	// Mode сохраняет режим обработки LIVE-видео группы.
	Mode GroupMode
	// Format содержит версионированное описание обеих дорожек.
	Format formatMetadata
	// VideoEncoding сохраняет параметры видеокодирования.
	VideoEncoding videoEncodingV1
	// AudioEncoding сохраняет параметры аудиокодирования.
	AudioEncoding audioEncodingV1
}

// videoEncodingV1 сохраняет прежний порядок и типы полей видеокодирования профиля v1.
type videoEncodingV1 struct {
	// TargetBitrateBPS содержит целевой видеобитрейт в битах в секунду.
	TargetBitrateBPS uint64
	// MaxBitrateBPS содержит верхний предел видеобитрейта в битах в секунду.
	MaxBitrateBPS uint64
	// BufferBits содержит размер буфера управления битрейтом в битах.
	BufferBits uint64
	// GOPFrames содержит длину GOP в кадрах.
	GOPFrames uint32
	// MaxBFrames содержит максимальное число B-кадров.
	MaxBFrames uint32
	// ClosedGOP указывает требование закрытого GOP.
	ClosedGOP bool
}

// audioEncodingV1 сохраняет прежнее представление аудиобитрейта профиля v1.
type audioEncodingV1 struct { /* BitrateBPS содержит аудиобитрейт в битах в секунду. */
	BitrateBPS uint64
}

// profileMetadataV2 сохраняет полный профиль v2 с явными настройками кодировщиков.
type profileMetadataV2 struct {
	// Version содержит версию сохраняемого представления.
	Version int
	// Mode сохраняет режим обработки LIVE-видео группы.
	Mode GroupMode
	// Format содержит версионированное описание обеих дорожек.
	Format formatMetadata
	// VideoEncoding сохраняет параметры видеокодирования.
	VideoEncoding VideoEncoding
	// AudioEncoding сохраняет параметры аудиокодирования.
	AudioEncoding AudioEncoding
	// Encoding сохраняет настройки конкретных кодировщиков.
	Encoding EncodingSettings
}

// profileMetadataV3 сохраняет профиль v3 с явным режимом LIVE-аудио и допускающим null блоком
// кодировщиков.
type profileMetadataV3 struct {
	// Version содержит версию сохраняемого представления.
	Version int
	// Mode сохраняет режим обработки LIVE-видео группы.
	Mode GroupMode
	// LiveAudioMode сохраняет явно выбранный режим обработки LIVE-аудио.
	LiveAudioMode LiveAudioMode
	// Format содержит версионированное описание обеих дорожек.
	Format formatMetadata
	// VideoEncoding сохраняет параметры видеокодирования.
	VideoEncoding VideoEncoding
	// AudioEncoding сохраняет параметры аудиокодирования.
	AudioEncoding AudioEncoding
	// Encoding сохраняет настройки кодировщиков либо явный null для прежнего профиля.
	Encoding *EncodingSettings
}

// formatDTO переводит формат в версионированное представление, сохраняя строковые параметры как точные
// последовательности байтов.
func formatDTO(f MediaFormat) formatMetadata {
	// v содержит видеодорожку исходного формата, а a — его аудиодорожку для перевода в сохраняемое
	// представление.
	v, a := f.Video, f.Audio
	return formatMetadata{Version: MetadataVersion, Video: videoMetadata{Codec: v.Codec, Width: v.Width, Height: v.Height, FrameRate: v.FrameRate, CodecProfile: []byte(v.CodecProfile), Level: []byte(v.Level), Tier: []byte(v.Tier), PixelFormat: []byte(v.PixelFormat), BitDepth: v.BitDepth, Range: []byte(v.Color.Range), Primaries: []byte(v.Color.Primaries), Transfer: []byte(v.Color.Transfer), Matrix: []byte(v.Color.Matrix)}, Audio: audioMetadata{Codec: a.Codec, CodecProfile: []byte(a.CodecProfile), SampleRateHz: a.SampleRateHz, Channels: a.Channels, ChannelLayout: []byte(a.ChannelLayout)}}
}

// value восстанавливает доменный формат из сохранённых байтов без проверки версии и допустимости
// параметров.
func (d formatMetadata) value() MediaFormat {
	// v содержит сохранённые параметры видео, а a — сохранённые параметры аудио для восстановления
	// доменного формата.
	v, a := d.Video, d.Audio
	return MediaFormat{Video: VideoFormat{Codec: v.Codec, Width: v.Width, Height: v.Height, FrameRate: v.FrameRate, CodecProfile: string(v.CodecProfile), Level: string(v.Level), Tier: string(v.Tier), PixelFormat: string(v.PixelFormat), BitDepth: v.BitDepth, Color: ColorInfo{Range: string(v.Range), Primaries: string(v.Primaries), Transfer: string(v.Transfer), Matrix: string(v.Matrix)}}, Audio: AudioFormat{Codec: a.Codec, CodecProfile: string(a.CodecProfile), SampleRateHz: a.SampleRateHz, Channels: a.Channels, ChannelLayout: string(a.ChannelLayout)}}
}

// strictJSON декодирует ровно одно значение JSON и отклоняет неизвестные поля и последующие значения.
func strictJSON(data []byte, value any) error {
	// d декодирует исходные байты с запретом неизвестных полей и лишних JSON-значений.
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	// err содержит причину отказа чтения основного JSON-значения.
	if err := d.Decode(value); err != nil {
		return err
	}
	// err позволяет отличить конец входа от ошибки или лишнего JSON-значения.
	if err := d.Decode(new(any)); err != io.EOF {
		if err != nil {
			return err
		}
		return fmt.Errorf("лишнее значение JSON")
	}
	return nil
}

// EncodeProfile кодирует проверенный профиль в соответствующее его полям представление v1, v2 или v3.
func EncodeProfile(p GroupProfile) ([]byte, error) { return p.MarshalJSON() }

// DecodeProfile восстанавливает и проверяет профиль из поддерживаемого версионированного
// представления.
func DecodeProfile(data []byte) (GroupProfile, error) {
	// p принимает восстановленный профиль, возвращаемый вызывающему коду.
	var p GroupProfile
	// err содержит причину отказа чтения или проверки профиля.
	err := p.UnmarshalJSON(data)
	return p, err
}

// MarshalJSON проверяет профиль и выбирает v3 при явном LiveAudioMode, v2 при ненулевом Encoding,
// иначе сохраняет прежнее представление v1.
func (p GroupProfile) MarshalJSON() ([]byte, error) {
	// version выбирает версию сохраняемого профиля по наличию настроек кодировщиков и
	// LIVE-аудиорежима.
	version := MetadataVersion
	if p.Encoding != (EncodingSettings{}) {
		version = EncodingVersion
	}
	if p.LiveAudioMode != "" {
		version = profileVersionV3
	}
	// err содержит причину отказа проверки профиля перед сохранением.
	if err := p.Validate(); err != nil {
		return nil, &MetadataError{Kind: "profile", Version: version, Cause: err}
	}
	if p.LiveAudioMode != "" {
		// encoding указывает сохраняемый блок кодировщиков либо nil для прежнего профиля с явным
		// LIVE-аудиорежимом.
		var encoding *EncodingSettings
		if p.Encoding != (EncodingSettings{}) {
			encoding = &p.Encoding
		}
		return json.Marshal(profileMetadataV3{Version: profileVersionV3, Mode: p.Mode, LiveAudioMode: p.LiveAudioMode, Format: formatDTO(p.Format), VideoEncoding: p.VideoEncoding, AudioEncoding: p.AudioEncoding, Encoding: encoding})
	}
	if p.Encoding == (EncodingSettings{}) {
		return json.Marshal(profileMetadataV1{Version: MetadataVersion, Mode: p.Mode, Format: formatDTO(p.Format), VideoEncoding: videoEncodingV1(p.VideoEncoding), AudioEncoding: audioEncodingV1(p.AudioEncoding)})
	}
	return json.Marshal(profileMetadataV2{Version: EncodingVersion, Mode: p.Mode, Format: formatDTO(p.Format), VideoEncoding: p.VideoEncoding, AudioEncoding: p.AudioEncoding, Encoding: p.Encoding})
}

// UnmarshalJSON выбирает декодер по версии профиля и изменяет получатель только после успешной
// проверки; неизвестные поля отклоняются.
func (p *GroupProfile) UnmarshalJSON(data []byte) error {
	// header содержит прочитанную версию для выбора декодера профиля.
	var header struct { /* Version содержит версию сохраняемого представления. */
		Version int
	}
	// err содержит причину отказа чтения версии профиля.
	if err := json.Unmarshal(data, &header); err != nil {
		return &MetadataError{Kind: "profile", Cause: err}
	}
	if header.Version == EncodingVersion {
		return p.unmarshalProfileV2(data)
	}
	if header.Version == profileVersionV3 {
		return p.unmarshalProfileV3(data)
	}
	// d принимает прежнее сохраняемое представление профиля v1.
	var d profileMetadataV1
	// err содержит причину отказа строгого чтения профиля v1.
	if err := strictJSON(data, &d); err != nil {
		return &MetadataError{Kind: "profile", Cause: err}
	}
	if d.Version != MetadataVersion || d.Format.Version != MetadataVersion {
		return &MetadataError{Kind: "profile", Version: d.Version, Cause: ErrInvalidMetadata}
	}
	// result хранит восстановленный профиль v1 до успешной проверки.
	result := GroupProfile{Mode: d.Mode, Format: d.Format.value(), VideoEncoding: VideoEncoding(d.VideoEncoding), AudioEncoding: AudioEncoding(d.AudioEncoding)}
	// err содержит причину отказа проверки восстановленного профиля v1.
	if err := result.Validate(); err != nil {
		return &MetadataError{Kind: "profile", Version: d.Version, Cause: err}
	}
	*p = result
	return nil
}

// EncodeMediaFormat проверяет обе дорожки и кодирует формат с MetadataVersion.
func EncodeMediaFormat(f MediaFormat) ([]byte, error) { return f.MarshalJSON() }

// DecodeMediaFormat читает версионированный формат и проверяет обе дорожки.
func DecodeMediaFormat(data []byte) (MediaFormat, error) {
	// f принимает восстановленный медиаформат для возврата вызывающему коду.
	var f MediaFormat
	// err содержит причину отказа чтения или проверки медиаформата.
	err := f.UnmarshalJSON(data)
	return f, err
}

// MarshalJSON проверяет формат и сохраняет его строковые параметры через байтовые поля
// MetadataVersion.
func (f MediaFormat) MarshalJSON() ([]byte, error) {
	// err содержит причину отказа проверки медиаформата перед сохранением.
	if err := f.Validate(); err != nil {
		return nil, &MetadataError{Kind: "format", Version: MetadataVersion, Cause: err}
	}
	return json.Marshal(formatDTO(f))
}

// UnmarshalJSON требует поддерживаемую версию и допустимый формат; при ошибке получатель сохраняется.
func (f *MediaFormat) UnmarshalJSON(data []byte) error {
	// d принимает версионированное представление медиаформата.
	var d formatMetadata
	// err содержит причину отказа строгого чтения медиаформата.
	if err := strictJSON(data, &d); err != nil {
		return &MetadataError{Kind: "format", Cause: err}
	}
	if d.Version != MetadataVersion {
		return &MetadataError{Kind: "format", Version: d.Version, Cause: ErrInvalidMetadata}
	}
	// result хранит восстановленный медиаформат до успешной проверки.
	result := d.value()
	// err содержит причину отказа проверки восстановленного медиаформата.
	if err := result.Validate(); err != nil {
		return &MetadataError{Kind: "format", Version: d.Version, Cause: err}
	}
	*f = result
	return nil
}

// CanonicalProfile проверяет профиль и сокращает дробь частоты кадров перед кодированием; исходное
// значение не изменяется.
func CanonicalProfile(p GroupProfile) ([]byte, error) {
	// err содержит причину отказа проверки профиля перед канонизацией.
	if err := p.Validate(); err != nil {
		return nil, err
	}
	// a и b содержат остатки алгоритма Евклида; итоговый a сокращает дробь частоты кадров.
	a, b := p.Format.Video.FrameRate.Numerator, p.Format.Video.FrameRate.Denominator
	for b != 0 {
		a, b = b, a%b
	}
	p.Format.Video.FrameRate.Numerator /= a
	p.Format.Video.FrameRate.Denominator /= a
	return EncodeProfile(p)
}

// ProfileIdentity возвращает SHA-256 в шестнадцатеричном виде и канонические байты полного профиля.
func ProfileIdentity(p GroupProfile) (string, []byte, error) {
	// data содержит канонические байты профиля, а err — причину отказа канонизации.
	data, err := CanonicalProfile(p)
	if err != nil {
		return "", nil, err
	}
	// sum содержит SHA-256 канонических байтов профиля перед шестнадцатеричной записью.
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), data, nil
}

// VariantIdentity вычисляет SHA-256 версионированного сочетания хеша источника, канонического профиля
// и версии генератора.
func VariantIdentity(sourceHash string, p GroupProfile, generatorVersion string) (string, error) {
	// data содержит канонические байты профиля варианта, а err — причину отказа их получения.
	data, err := CanonicalProfile(p)
	if err != nil {
		return "", err
	}
	// encoded содержит версионированное сочетание источника, профиля и генератора для хеширования.
	encoded, err := json.Marshal(struct {
		// Version содержит версию сохраняемого представления.
		Version int
		// SourceHash содержит хеш исходного материала для идентичности варианта.
		SourceHash string
		// Profile содержит канонические байты полного профиля.
		Profile []byte
		// GeneratorVersion содержит версию генератора для идентичности варианта.
		GeneratorVersion string
	}{MetadataVersion, sourceHash, data, generatorVersion})
	if err != nil {
		return "", err
	}
	// sum содержит SHA-256 полной идентичности варианта.
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

// sessionMetadata связывает сохраняемый снимок запуска с версией его представления.
type sessionMetadata struct {
	// Version содержит версию сохраняемого представления.
	Version int
	// Snapshot содержит самостоятельный сохраняемый снимок.
	Snapshot SessionSnapshot
}

// outputMetadata связывает сохраняемый снимок выхода с версией его представления.
type outputMetadata struct {
	// Version содержит версию сохраняемого представления.
	Version int
	// Snapshot содержит самостоятельный сохраняемый снимок.
	Snapshot OutputSnapshot
}

// EncodeSessionSnapshot проверяет снимок запуска и кодирует его в оболочке MetadataVersion.
func EncodeSessionSnapshot(s SessionSnapshot) ([]byte, error) {
	// err содержит причину отказа проверки снимка запуска перед сохранением.
	if err := s.Validate(); err != nil {
		return nil, &MetadataError{Kind: "session", Version: MetadataVersion, Cause: err}
	}
	return json.Marshal(sessionMetadata{MetadataVersion, s})
}

// DecodeSessionSnapshot проверяет оболочку и снимок запуска; ошибка проверки снимка возвращается
// вместе с прочитанным значением.
func DecodeSessionSnapshot(data []byte) (SessionSnapshot, error) {
	// d принимает сохраняемую оболочку снимка запуска.
	var d sessionMetadata
	// err содержит причину отказа строгого чтения снимка запуска.
	if err := strictJSON(data, &d); err != nil {
		return SessionSnapshot{}, &MetadataError{Kind: "session", Cause: err}
	}
	if d.Version != MetadataVersion {
		return SessionSnapshot{}, &MetadataError{Kind: "session", Version: d.Version, Cause: ErrInvalidMetadata}
	}
	// err содержит причину отказа доменной проверки прочитанного снимка запуска.
	if err := d.Snapshot.Validate(); err != nil {
		return d.Snapshot, &MetadataError{Kind: "snapshot", Version: d.Version, Cause: err}
	}
	return d.Snapshot, nil
}

// EncodeOutputSnapshot проверяет снимок выхода и кодирует его в оболочке MetadataVersion.
func EncodeOutputSnapshot(s OutputSnapshot) ([]byte, error) {
	// err содержит причину отказа проверки снимка выхода перед сохранением.
	if err := s.Validate(); err != nil {
		return nil, &MetadataError{Kind: "output", Version: MetadataVersion, Cause: err}
	}
	return json.Marshal(outputMetadata{MetadataVersion, s})
}

// DecodeOutputSnapshot проверяет оболочку и снимок выхода; ошибка проверки снимка возвращается вместе
// с прочитанным значением.
func DecodeOutputSnapshot(data []byte) (OutputSnapshot, error) {
	// d принимает сохраняемую оболочку снимка выхода.
	var d outputMetadata
	// err содержит причину отказа строгого чтения снимка выхода.
	if err := strictJSON(data, &d); err != nil {
		return OutputSnapshot{}, &MetadataError{Kind: "output", Cause: err}
	}
	if d.Version != MetadataVersion {
		return OutputSnapshot{}, &MetadataError{Kind: "output", Version: d.Version, Cause: ErrInvalidMetadata}
	}
	// err содержит причину отказа доменной проверки прочитанного снимка выхода.
	if err := d.Snapshot.Validate(); err != nil {
		return d.Snapshot, &MetadataError{Kind: "snapshot", Version: d.Version, Cause: err}
	}
	return d.Snapshot, nil
}

// EncodeMetadata оборачивает типизированное значение версией схемы. Вложенные
// профили и форматы используют собственные точные представления байтов строк.
func EncodeMetadata(value any) ([]byte, error) {
	// payload содержит JSON доменного значения, а err — причину отказа его кодирования.
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, &MetadataError{Kind: "domain", Version: MetadataVersion, Cause: err}
	}
	return json.Marshal(struct {
		// Version содержит версию сохраняемого представления.
		Version int
		// Payload содержит JSON-представление вложенного доменного значения.
		Payload json.RawMessage
	}{MetadataVersion, payload})
}

// DecodeMetadata читает строгое версионированное представление и вызывает Validate у получателя, если
// тот поддерживает проверку; ошибочная нагрузка не откатывает уже выполненное декодирование.
func DecodeMetadata(data []byte, value any) error {
	// envelope принимает версию и JSON-нагрузку сохраняемого доменного значения.
	var envelope struct {
		// Version содержит версию сохраняемого представления.
		Version int
		// Payload содержит JSON-представление вложенного доменного значения.
		Payload json.RawMessage
	}
	// err содержит причину отказа строгого чтения оболочки метаданных.
	if err := strictJSON(data, &envelope); err != nil {
		return &MetadataError{Kind: "domain", Cause: err}
	}
	if envelope.Version != MetadataVersion || len(envelope.Payload) == 0 || bytes.Equal(envelope.Payload, []byte("null")) {
		return &MetadataError{Kind: "domain", Version: envelope.Version, Cause: ErrInvalidMetadata}
	}
	// err содержит причину отказа строгого чтения доменного значения из нагрузки.
	if err := strictJSON(envelope.Payload, value); err != nil {
		return &MetadataError{Kind: "domain", Version: envelope.Version, Cause: err}
	}
	// validatable предоставляет доменную проверку декодированного получателя, а ok показывает,
	// поддерживает ли он её.
	if validatable, ok := value.(interface {
		// Validate проверяет уже декодированное доменное значение и возвращает причину недопустимых
		// метаданных.
		Validate() error
	}); ok {
		// err содержит причину отказа проверки декодированного доменного значения.
		if err := validatable.Validate(); err != nil {
			return &MetadataError{Kind: "domain", Version: envelope.Version, Cause: err}
		}
	}
	return nil
}

// sourceMetadata сохраняет формат исходного материала с необязательными аудио- и видеодорожками.
type sourceMetadata struct {
	// Version содержит версию сохраняемого представления.
	Version int
	// Video содержит сохраняемые параметры присутствующей видеодорожки.
	Video *videoMetadata
	// Audio содержит сохраняемые параметры присутствующей аудиодорожки.
	Audio *audioMetadata
	// ImageFormat сохраняет обозначение формата неподвижного изображения.
	ImageFormat string
}

// MarshalJSON проверяет исходный формат и сохраняет только присутствующие дорожки, не подставляя
// выходной профиль.
func (f SourceFormat) MarshalJSON() ([]byte, error) {
	// err содержит причину отказа проверки исходного формата перед сохранением.
	if err := f.Validate(); err != nil {
		return nil, &MetadataError{Kind: "source", Version: MetadataVersion, Cause: err}
	}
	// d накапливает сохраняемый формат исходника с явно присутствующими дорожками.
	d := sourceMetadata{Version: MetadataVersion, ImageFormat: f.ImageFormat}
	if f.Video != nil {
		// v содержит сохраняемые параметры присутствующей видеодорожки исходника.
		v := formatDTO(MediaFormat{Video: *f.Video}).Video
		d.Video = &v
	}
	if f.Audio != nil {
		// a содержит сохраняемые параметры присутствующей аудиодорожки исходника.
		a := formatDTO(MediaFormat{Audio: *f.Audio}).Audio
		d.Audio = &a
	}
	return json.Marshal(d)
}

// UnmarshalJSON восстанавливает необязательные дорожки исходника и присваивает результат только после
// проверки версии и формата.
func (f *SourceFormat) UnmarshalJSON(data []byte) error {
	// d принимает версионированный формат исходного материала.
	var d sourceMetadata
	// err содержит причину отказа строгого чтения формата исходника.
	if err := strictJSON(data, &d); err != nil {
		return &MetadataError{Kind: "source", Cause: err}
	}
	if d.Version != MetadataVersion {
		return &MetadataError{Kind: "source", Version: d.Version, Cause: ErrInvalidMetadata}
	}
	// result накапливает восстановленные параметры исходника до окончательной проверки.
	result := SourceFormat{ImageFormat: d.ImageFormat}
	if d.Video != nil {
		// v содержит восстановленную видеодорожку, на которую ссылается результат.
		v := (formatMetadata{Video: *d.Video}).value().Video
		result.Video = &v
	}
	if d.Audio != nil {
		// a содержит восстановленную аудиодорожку, на которую ссылается результат.
		a := (formatMetadata{Audio: *d.Audio}).value().Audio
		result.Audio = &a
	}
	// err содержит причину отказа проверки восстановленного формата исходника.
	if err := result.Validate(); err != nil {
		return &MetadataError{Kind: "source", Version: d.Version, Cause: err}
	}
	*f = result
	return nil
}
