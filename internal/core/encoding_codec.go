package core

import (
	"bytes"
	"encoding/json"
	"fmt"
)

// completeProfile требует все поля полного профиля data, включая явные false, нули и пустые строки.
// Проверка присутствия независима от декодирования: null нельзя принять за нулевое значение Go.
func completeProfile(data []byte) error {
	// root содержит обязательные корневые поля, err сообщает об отсутствии или null.
	root, err := requiredProfileObject(data, "Mode", "LiveAudioMode", "Format", "VideoEncoding", "AudioEncoding", "Encoding")
	if err != nil {
		return err
	}
	// err сообщает о неполном блоке битрейтов и GOP.
	if _, err = requiredProfileObject(root["VideoEncoding"], "TargetBitrateBPS", "MaxBitrateBPS", "BufferBits", "GOPFrames", "MaxBFrames", "ClosedGOP"); err != nil {
		return err
	}
	if _, err = requiredProfileObject(root["AudioEncoding"], "BitrateBPS"); err != nil {
		return err
	}
	// format содержит обязательные дорожки и независимую версию их метаданных.
	format, err := requiredProfileObject(root["Format"], "Version", "Video", "Audio")
	if err != nil {
		return err
	}
	// video содержит все точные параметры видеодорожки.
	video, err := requiredProfileObject(format["Video"], "Codec", "Width", "Height", "FrameRate", "CodecProfile", "Level", "Tier", "PixelFormat", "BitDepth", "Range", "Primaries", "Transfer", "Matrix")
	if err != nil {
		return err
	}
	if _, err = requiredProfileObject(video["FrameRate"], "Numerator", "Denominator"); err != nil {
		return err
	}
	if _, err = requiredProfileObject(format["Audio"], "Codec", "CodecProfile", "SampleRateHz", "Channels", "ChannelLayout"); err != nil {
		return err
	}
	// encoding содержит все материализованные параметры и независимые объекты качества.
	encoding, err := requiredProfileObject(root["Encoding"], "QP", "CRF", "VideoEncoder", "AudioEncoder", "RateControl", "RescaleOutput", "VideoCustom", "AudioCustom", "NVENC", "SVTAV1")
	if err != nil {
		return err
	}
	if _, err = requiredProfileObject(encoding["QP"], "Set", "Value"); err != nil {
		return err
	}
	if _, err = requiredProfileObject(encoding["CRF"], "Set", "ValueHundredths"); err != nil {
		return err
	}
	if _, err = requiredProfileObject(encoding["NVENC"], "Preset", "Tuning", "Multipass", "LookaheadFrames", "EnableSpatialAQ", "EnableTemporalAQ", "AQStrength"); err != nil {
		return err
	}
	_, err = requiredProfileObject(encoding["SVTAV1"], "Preset", "Tuning", "AQMode", "LookaheadFrames")
	return err
}

// requiredProfileObject читает объект data и требует заданные fields без JSON null.
// Нулевые числа, false и пустые строки допустимы; только JSON null считается отсутствием значения.
func requiredProfileObject(data []byte, fields ...string) (map[string]json.RawMessage, error) {
	// values связывает точное имя поля с исходными JSON-байтами.
	var values map[string]json.RawMessage
	// err сообщает об ошибочном типе JSON-объекта.
	if err := json.Unmarshal(data, &values); err != nil {
		return nil, err
	}
	// field содержит очередное обязательное имя без преобразования регистра.
	for _, field := range fields {
		// value содержит исходное JSON-значение, ok подтверждает присутствие поля.
		value, ok := values[field]
		if !ok || bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, fmt.Errorf("%w: отсутствует материализованное поле %s", ErrInvalidMetadata, field)
		}
	}
	return values, nil
}
