package core

import (
	"bytes"
	"encoding/json"
	"fmt"
)

func (p *GroupProfile) unmarshalProfileV2(data []byte) error {
	fail := func(err error) error { return &MetadataError{Kind: "profile", Version: EncodingVersion, Cause: err} }
	var d profileMetadataV2
	if err := strictJSON(data, &d); err != nil {
		return fail(err)
	}
	if d.Version != EncodingVersion || d.Format.Version != MetadataVersion {
		return fail(ErrInvalidMetadata)
	}
	if err := completeProfileV2(data); err != nil {
		return fail(err)
	}
	result := GroupProfile{Mode: d.Mode, Format: d.Format.value(), VideoEncoding: d.VideoEncoding, AudioEncoding: d.AudioEncoding, Encoding: d.Encoding}
	if result.Encoding.Version != EncodingVersion {
		return fail(ErrInvalidMetadata)
	}
	if err := result.Validate(); err != nil {
		return fail(err)
	}
	*p = result
	return nil
}

// completeProfileV2 требует присутствия сохранённых полей, включая явные false, 0 и пустые строки.
func completeProfileV2(data []byte) error {
	root, err := requiredProfileObject(data, true, "Version", "Mode", "Format", "VideoEncoding", "AudioEncoding", "Encoding")
	if err != nil {
		return err
	}
	if _, err = requiredProfileObject(root["VideoEncoding"], true, "TargetBitrateBPS", "MaxBitrateBPS", "BufferBits", "GOPFrames", "MaxBFrames", "ClosedGOP"); err != nil {
		return err
	}
	if _, err = requiredProfileObject(root["AudioEncoding"], true, "BitrateBPS"); err != nil {
		return err
	}
	format, err := requiredProfileObject(root["Format"], true, "Version", "Video", "Audio")
	if err != nil {
		return err
	}
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

func requiredProfileObject(data []byte, nonNull bool, fields ...string) (map[string]json.RawMessage, error) {
	var values map[string]json.RawMessage
	if err := json.Unmarshal(data, &values); err != nil {
		return nil, err
	}
	for _, field := range fields {
		value, ok := values[field]
		if !ok || nonNull && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, fmt.Errorf("%w: отсутствует материализованное поле %s", ErrInvalidMetadata, field)
		}
	}
	return values, nil
}
