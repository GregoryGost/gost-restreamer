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
	return completeProfileFields(root, false)
}

func (p *GroupProfile) unmarshalProfileV3(data []byte) error {
	fail := func(err error) error { return &MetadataError{Kind: "profile", Version: profileVersionV3, Cause: err} }
	var d profileMetadataV3
	if err := strictJSON(data, &d); err != nil {
		return fail(err)
	}
	root, err := requiredProfileObject(data, true, "Version", "Mode", "LiveAudioMode", "Format", "VideoEncoding", "AudioEncoding")
	if err != nil {
		return fail(err)
	}
	if _, ok := root["Encoding"]; !ok {
		return fail(ErrInvalidMetadata)
	}
	if d.Version != profileVersionV3 || d.Format.Version != MetadataVersion || d.LiveAudioMode == "" {
		return fail(ErrInvalidMetadata)
	}
	if err := completeProfileFields(root, true); err != nil {
		return fail(err)
	}
	result := GroupProfile{Mode: d.Mode, LiveAudioMode: d.LiveAudioMode, Format: d.Format.value(), VideoEncoding: d.VideoEncoding, AudioEncoding: d.AudioEncoding}
	if d.Encoding != nil {
		if d.Encoding.Version != EncodingVersion {
			return fail(ErrInvalidMetadata)
		}
		result.Encoding = *d.Encoding
	}
	if err := result.Validate(); err != nil {
		return fail(err)
	}
	*p = result
	return nil
}

func completeProfileFields(root map[string]json.RawMessage, nullableEncoding bool) error {
	var err error
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
	if nullableEncoding && bytes.Equal(bytes.TrimSpace(root["Encoding"]), []byte("null")) {
		return nil
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
