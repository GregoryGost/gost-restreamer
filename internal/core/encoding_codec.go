package core

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// unmarshalProfileV2 требует полного сохранённого профиля v2 и присваивает его получателю только после
// проверки версий, полей и доменных ограничений.
func (p *GroupProfile) unmarshalProfileV2(data []byte) error {
	// fail оборачивает отказ чтения профиля v2 с указанием вида метаданных и версии.
	fail := func(err error) error {
		// Оборачивает причину отказа чтения профиля v2 в MetadataError, сохраняя Kind profile и
		// EncodingVersion.
		return &MetadataError{Kind: "profile", Version: EncodingVersion, Cause: err}
	}
	// d принимает сохранённое представление профиля v2 до проверки и присваивания получателю.
	var d profileMetadataV2
	// err содержит причину отказа строгого разбора профиля v2.
	if err := strictJSON(data, &d); err != nil {
		return fail(err)
	}
	if d.Version != EncodingVersion || d.Format.Version != MetadataVersion {
		return fail(ErrInvalidMetadata)
	}
	// err содержит причину отсутствия обязательных материализованных полей профиля v2.
	if err := completeProfileV2(data); err != nil {
		return fail(err)
	}
	// result хранит восстановленный профиль до успешной доменной проверки.
	result := GroupProfile{Mode: d.Mode, Format: d.Format.value(), VideoEncoding: d.VideoEncoding, AudioEncoding: d.AudioEncoding, Encoding: d.Encoding}
	if result.Encoding.Version != EncodingVersion {
		return fail(ErrInvalidMetadata)
	}
	// err содержит причину отказа доменной проверки восстановленного профиля.
	if err := result.Validate(); err != nil {
		return fail(err)
	}
	*p = result
	return nil
}

// completeProfileV2 требует присутствия сохранённых полей, включая явные false, 0 и пустые строки.
func completeProfileV2(data []byte) error {
	// root содержит обязательные поля корневого объекта профиля v2, а err — причину отказа проверки
	// их присутствия.
	root, err := requiredProfileObject(data, true, "Version", "Mode", "Format", "VideoEncoding", "AudioEncoding", "Encoding")
	if err != nil {
		return err
	}
	return completeProfileFields(root, false)
}

// unmarshalProfileV3 читает профиль с явным LiveAudioMode; Encoding может быть null. Получатель
// меняется только после полной проверки представления и профиля.
func (p *GroupProfile) unmarshalProfileV3(data []byte) error {
	// fail оборачивает отказ чтения профиля с явным LIVE-аудиорежимом в ошибку метаданных v3.
	fail := func(err error) error {
		// Оборачивает причину отказа чтения профиля v3 в MetadataError с точной версией profileVersionV3.
		return &MetadataError{Kind: "profile", Version: profileVersionV3, Cause: err}
	}
	// d принимает сохранённое представление профиля v3 до проверки и присваивания получателю.
	var d profileMetadataV3
	// err содержит причину отказа строгого разбора профиля v3.
	if err := strictJSON(data, &d); err != nil {
		return fail(err)
	}
	// root содержит обязательные поля профиля v3, а err — причину отказа проверки их присутствия.
	root, err := requiredProfileObject(data, true, "Version", "Mode", "LiveAudioMode", "Format", "VideoEncoding", "AudioEncoding")
	if err != nil {
		return fail(err)
	}
	// ok показывает, присутствует ли поле Encoding, включая допустимый явный null.
	if _, ok := root["Encoding"]; !ok {
		return fail(ErrInvalidMetadata)
	}
	if d.Version != profileVersionV3 || d.Format.Version != MetadataVersion || d.LiveAudioMode == "" {
		return fail(ErrInvalidMetadata)
	}
	// err содержит причину неполноты вложенных полей профиля v3.
	if err := completeProfileFields(root, true); err != nil {
		return fail(err)
	}
	// result хранит восстановленный профиль с LIVE-аудиорежимом до окончательной проверки.
	result := GroupProfile{Mode: d.Mode, LiveAudioMode: d.LiveAudioMode, Format: d.Format.value(), VideoEncoding: d.VideoEncoding, AudioEncoding: d.AudioEncoding}
	if d.Encoding != nil {
		if d.Encoding.Version != EncodingVersion {
			return fail(ErrInvalidMetadata)
		}
		result.Encoding = *d.Encoding
	}
	// err содержит причину отказа доменной проверки восстановленного профиля v3.
	if err := result.Validate(); err != nil {
		return fail(err)
	}
	*p = result
	return nil
}

// completeProfileFields проверяет наличие материализованных полей дорожек и кодировщиков; при
// nullableEncoding явный null заменяет весь блок Encoding.
func completeProfileFields(root map[string]json.RawMessage, nullableEncoding bool) error {
	// err передаёт отказ проверки очередного обязательного вложенного объекта профиля.
	var err error
	if _, err = requiredProfileObject(root["VideoEncoding"], true, "TargetBitrateBPS", "MaxBitrateBPS", "BufferBits", "GOPFrames", "MaxBFrames", "ClosedGOP"); err != nil {
		return err
	}
	if _, err = requiredProfileObject(root["AudioEncoding"], true, "BitrateBPS"); err != nil {
		return err
	}
	// format содержит обязательные поля сохранённого медиаформата.
	format, err := requiredProfileObject(root["Format"], true, "Version", "Video", "Audio")
	if err != nil {
		return err
	}
	// video содержит обязательные поля сохранённой видеодорожки.
	video, err := requiredProfileObject(format["Video"], false, "Codec", "Width", "Height", "FrameRate", "CodecProfile", "Level", "Tier", "PixelFormat", "BitDepth", "Range", "Primaries", "Transfer", "Matrix")
	if err != nil {
		return err
	}
	if _, err = requiredProfileObject(video["FrameRate"], true, "Numerator", "Denominator"); err != nil {
		return err
	}
	if _, err = requiredProfileObject(format["Audio"], false, "Codec", "CodecProfile", "SampleRateHz", "Channels", "ChannelLayout"); err != nil {
		return err
	}
	if nullableEncoding && bytes.Equal(bytes.TrimSpace(root["Encoding"]), []byte("null")) {
		return nil
	}
	// encoding содержит обязательные поля блока настроек кодировщиков.
	encoding, err := requiredProfileObject(root["Encoding"], true, "Version", "VideoEncoder", "AudioEncoder", "RateControl", "RescaleOutput", "VideoCustom", "AudioCustom", "NVENC", "SVTAV1")
	if err != nil {
		return err
	}
	if _, err = requiredProfileObject(encoding["NVENC"], true, "Preset", "Tuning", "Multipass", "LookaheadFrames", "EnableSpatialAQ", "EnableTemporalAQ", "AQStrength"); err != nil {
		return err
	}
	_, err = requiredProfileObject(encoding["SVTAV1"], true, "Preset", "Tuning", "AQMode", "LookaheadFrames")
	return err
}

// requiredProfileObject разбирает объект и требует перечисленные поля; nonNull дополнительно запрещает
// им значение null.
func requiredProfileObject(data []byte, nonNull bool, fields ...string) (map[string]json.RawMessage, error) {
	// values сопоставляет имена полей объекта их исходным JSON-значениям.
	var values map[string]json.RawMessage
	// err содержит причину отказа разбора JSON-объекта профиля.
	if err := json.Unmarshal(data, &values); err != nil {
		return nil, err
	}
	// field содержит очередное обязательное имя поля для проверки присутствия.
	for _, field := range fields {
		// value содержит JSON-значение обязательного поля, а ok показывает, найдено ли это поле
		// в объекте.
		value, ok := values[field]
		if !ok || nonNull && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, fmt.Errorf("%w: отсутствует материализованное поле %s", ErrInvalidMetadata, field)
		}
	}
	return values, nil
}
