package core

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
)

const MetadataVersion = 1

type MetadataError struct {
	Kind    string
	Version int
	Cause   error
}

func (e *MetadataError) Error() string {
	return fmt.Sprintf("метаданные %s, версия %d: %v", e.Kind, e.Version, e.Cause)
}
func (e *MetadataError) Unwrap() error        { return e.Cause }
func (e *MetadataError) Is(target error) bool { return target == ErrInvalidMetadata }

type videoMetadata struct {
	Codec        Codec
	Width        uint32
	Height       uint32
	FrameRate    FrameRate
	CodecProfile []byte
	Level        []byte
	Tier         []byte
	PixelFormat  []byte
	BitDepth     uint8
	Range        []byte
	Primaries    []byte
	Transfer     []byte
	Matrix       []byte
}
type audioMetadata struct {
	Codec         Codec
	CodecProfile  []byte
	SampleRateHz  uint32
	Channels      uint16
	ChannelLayout []byte
}
type formatMetadata struct {
	Version int
	Video   videoMetadata
	Audio   audioMetadata
}

// profileMetadataV1 сохраняет точный порядок и типы полей прежнего формата хранения.
type profileMetadataV1 struct {
	Version       int
	Mode          GroupMode
	Format        formatMetadata
	VideoEncoding videoEncodingV1
	AudioEncoding audioEncodingV1
}

type videoEncodingV1 struct {
	TargetBitrateBPS uint64
	MaxBitrateBPS    uint64
	BufferBits       uint64
	GOPFrames        uint32
	MaxBFrames       uint32
	ClosedGOP        bool
}
type audioEncodingV1 struct{ BitrateBPS uint64 }

type profileMetadataV2 struct {
	Version       int
	Mode          GroupMode
	Format        formatMetadata
	VideoEncoding VideoEncoding
	AudioEncoding AudioEncoding
	Encoding      EncodingSettings
}

func formatDTO(f MediaFormat) formatMetadata {
	v, a := f.Video, f.Audio
	return formatMetadata{Version: MetadataVersion, Video: videoMetadata{Codec: v.Codec, Width: v.Width, Height: v.Height, FrameRate: v.FrameRate, CodecProfile: []byte(v.CodecProfile), Level: []byte(v.Level), Tier: []byte(v.Tier), PixelFormat: []byte(v.PixelFormat), BitDepth: v.BitDepth, Range: []byte(v.Color.Range), Primaries: []byte(v.Color.Primaries), Transfer: []byte(v.Color.Transfer), Matrix: []byte(v.Color.Matrix)}, Audio: audioMetadata{Codec: a.Codec, CodecProfile: []byte(a.CodecProfile), SampleRateHz: a.SampleRateHz, Channels: a.Channels, ChannelLayout: []byte(a.ChannelLayout)}}
}
func (d formatMetadata) value() MediaFormat {
	v, a := d.Video, d.Audio
	return MediaFormat{Video: VideoFormat{Codec: v.Codec, Width: v.Width, Height: v.Height, FrameRate: v.FrameRate, CodecProfile: string(v.CodecProfile), Level: string(v.Level), Tier: string(v.Tier), PixelFormat: string(v.PixelFormat), BitDepth: v.BitDepth, Color: ColorInfo{Range: string(v.Range), Primaries: string(v.Primaries), Transfer: string(v.Transfer), Matrix: string(v.Matrix)}}, Audio: AudioFormat{Codec: a.Codec, CodecProfile: string(a.CodecProfile), SampleRateHz: a.SampleRateHz, Channels: a.Channels, ChannelLayout: string(a.ChannelLayout)}}
}
func strictJSON(data []byte, value any) error {
	d := json.NewDecoder(bytes.NewReader(data))
	d.DisallowUnknownFields()
	if err := d.Decode(value); err != nil {
		return err
	}
	if err := d.Decode(new(any)); err != io.EOF {
		if err != nil {
			return err
		}
		return fmt.Errorf("лишнее значение JSON")
	}
	return nil
}
func EncodeProfile(p GroupProfile) ([]byte, error) { return p.MarshalJSON() }
func DecodeProfile(data []byte) (GroupProfile, error) {
	var p GroupProfile
	err := p.UnmarshalJSON(data)
	return p, err
}
func (p GroupProfile) MarshalJSON() ([]byte, error) {
	version := MetadataVersion
	if p.Encoding != (EncodingSettings{}) {
		version = EncodingVersion
	}
	if err := p.Validate(); err != nil {
		return nil, &MetadataError{Kind: "profile", Version: version, Cause: err}
	}
	if p.Encoding == (EncodingSettings{}) {
		return json.Marshal(profileMetadataV1{Version: MetadataVersion, Mode: p.Mode, Format: formatDTO(p.Format), VideoEncoding: videoEncodingV1(p.VideoEncoding), AudioEncoding: audioEncodingV1(p.AudioEncoding)})
	}
	return json.Marshal(profileMetadataV2{Version: EncodingVersion, Mode: p.Mode, Format: formatDTO(p.Format), VideoEncoding: p.VideoEncoding, AudioEncoding: p.AudioEncoding, Encoding: p.Encoding})
}
func (p *GroupProfile) UnmarshalJSON(data []byte) error {
	var header struct{ Version int }
	if err := json.Unmarshal(data, &header); err != nil {
		return &MetadataError{Kind: "profile", Cause: err}
	}
	if header.Version == EncodingVersion {
		return p.unmarshalProfileV2(data)
	}
	var d profileMetadataV1
	if err := strictJSON(data, &d); err != nil {
		return &MetadataError{Kind: "profile", Cause: err}
	}
	if d.Version != MetadataVersion || d.Format.Version != MetadataVersion {
		return &MetadataError{Kind: "profile", Version: d.Version, Cause: ErrInvalidMetadata}
	}
	result := GroupProfile{Mode: d.Mode, Format: d.Format.value(), VideoEncoding: VideoEncoding(d.VideoEncoding), AudioEncoding: AudioEncoding(d.AudioEncoding)}
	if err := result.Validate(); err != nil {
		return &MetadataError{Kind: "profile", Version: d.Version, Cause: err}
	}
	*p = result
	return nil
}
func EncodeMediaFormat(f MediaFormat) ([]byte, error) { return f.MarshalJSON() }
func DecodeMediaFormat(data []byte) (MediaFormat, error) {
	var f MediaFormat
	err := f.UnmarshalJSON(data)
	return f, err
}
func (f MediaFormat) MarshalJSON() ([]byte, error) {
	if err := f.Validate(); err != nil {
		return nil, &MetadataError{Kind: "format", Version: MetadataVersion, Cause: err}
	}
	return json.Marshal(formatDTO(f))
}
func (f *MediaFormat) UnmarshalJSON(data []byte) error {
	var d formatMetadata
	if err := strictJSON(data, &d); err != nil {
		return &MetadataError{Kind: "format", Cause: err}
	}
	if d.Version != MetadataVersion {
		return &MetadataError{Kind: "format", Version: d.Version, Cause: ErrInvalidMetadata}
	}
	result := d.value()
	if err := result.Validate(); err != nil {
		return &MetadataError{Kind: "format", Version: d.Version, Cause: err}
	}
	*f = result
	return nil
}

func CanonicalProfile(p GroupProfile) ([]byte, error) {
	if err := p.Validate(); err != nil {
		return nil, err
	}
	a, b := p.Format.Video.FrameRate.Numerator, p.Format.Video.FrameRate.Denominator
	for b != 0 {
		a, b = b, a%b
	}
	p.Format.Video.FrameRate.Numerator /= a
	p.Format.Video.FrameRate.Denominator /= a
	return EncodeProfile(p)
}
func ProfileIdentity(p GroupProfile) (string, []byte, error) {
	data, err := CanonicalProfile(p)
	if err != nil {
		return "", nil, err
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), data, nil
}
func VariantIdentity(sourceHash string, p GroupProfile, generatorVersion string) (string, error) {
	data, err := CanonicalProfile(p)
	if err != nil {
		return "", err
	}
	encoded, err := json.Marshal(struct {
		Version          int
		SourceHash       string
		Profile          []byte
		GeneratorVersion string
	}{MetadataVersion, sourceHash, data, generatorVersion})
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(encoded)
	return hex.EncodeToString(sum[:]), nil
}

type sessionMetadata struct {
	Version  int
	Snapshot SessionSnapshot
}
type outputMetadata struct {
	Version  int
	Snapshot OutputSnapshot
}

func EncodeSessionSnapshot(s SessionSnapshot) ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, &MetadataError{Kind: "session", Version: MetadataVersion, Cause: err}
	}
	return json.Marshal(sessionMetadata{MetadataVersion, s})
}
func DecodeSessionSnapshot(data []byte) (SessionSnapshot, error) {
	var d sessionMetadata
	if err := strictJSON(data, &d); err != nil {
		return SessionSnapshot{}, &MetadataError{Kind: "session", Cause: err}
	}
	if d.Version != MetadataVersion {
		return SessionSnapshot{}, &MetadataError{Kind: "session", Version: d.Version, Cause: ErrInvalidMetadata}
	}
	if err := d.Snapshot.Validate(); err != nil {
		return d.Snapshot, &MetadataError{Kind: "snapshot", Version: d.Version, Cause: err}
	}
	return d.Snapshot, nil
}
func EncodeOutputSnapshot(s OutputSnapshot) ([]byte, error) {
	if err := s.Validate(); err != nil {
		return nil, &MetadataError{Kind: "output", Version: MetadataVersion, Cause: err}
	}
	return json.Marshal(outputMetadata{MetadataVersion, s})
}
func DecodeOutputSnapshot(data []byte) (OutputSnapshot, error) {
	var d outputMetadata
	if err := strictJSON(data, &d); err != nil {
		return OutputSnapshot{}, &MetadataError{Kind: "output", Cause: err}
	}
	if d.Version != MetadataVersion {
		return OutputSnapshot{}, &MetadataError{Kind: "output", Version: d.Version, Cause: ErrInvalidMetadata}
	}
	if err := d.Snapshot.Validate(); err != nil {
		return d.Snapshot, &MetadataError{Kind: "snapshot", Version: d.Version, Cause: err}
	}
	return d.Snapshot, nil
}

// EncodeMetadata оборачивает типизированное значение версией схемы. Вложенные
// профили и форматы используют собственные точные представления байтов строк.
func EncodeMetadata(value any) ([]byte, error) {
	payload, err := json.Marshal(value)
	if err != nil {
		return nil, &MetadataError{Kind: "domain", Version: MetadataVersion, Cause: err}
	}
	return json.Marshal(struct {
		Version int
		Payload json.RawMessage
	}{MetadataVersion, payload})
}
func DecodeMetadata(data []byte, value any) error {
	var envelope struct {
		Version int
		Payload json.RawMessage
	}
	if err := strictJSON(data, &envelope); err != nil {
		return &MetadataError{Kind: "domain", Cause: err}
	}
	if envelope.Version != MetadataVersion || len(envelope.Payload) == 0 || bytes.Equal(envelope.Payload, []byte("null")) {
		return &MetadataError{Kind: "domain", Version: envelope.Version, Cause: ErrInvalidMetadata}
	}
	if err := strictJSON(envelope.Payload, value); err != nil {
		return &MetadataError{Kind: "domain", Version: envelope.Version, Cause: err}
	}
	if validatable, ok := value.(interface{ Validate() error }); ok {
		if err := validatable.Validate(); err != nil {
			return &MetadataError{Kind: "domain", Version: envelope.Version, Cause: err}
		}
	}
	return nil
}

type sourceMetadata struct {
	Version     int
	Video       *videoMetadata
	Audio       *audioMetadata
	ImageFormat string
}

func (f SourceFormat) MarshalJSON() ([]byte, error) {
	if err := f.Validate(); err != nil {
		return nil, &MetadataError{Kind: "source", Version: MetadataVersion, Cause: err}
	}
	d := sourceMetadata{Version: MetadataVersion, ImageFormat: f.ImageFormat}
	if f.Video != nil {
		v := formatDTO(MediaFormat{Video: *f.Video}).Video
		d.Video = &v
	}
	if f.Audio != nil {
		a := formatDTO(MediaFormat{Audio: *f.Audio}).Audio
		d.Audio = &a
	}
	return json.Marshal(d)
}
func (f *SourceFormat) UnmarshalJSON(data []byte) error {
	var d sourceMetadata
	if err := strictJSON(data, &d); err != nil {
		return &MetadataError{Kind: "source", Cause: err}
	}
	if d.Version != MetadataVersion {
		return &MetadataError{Kind: "source", Version: d.Version, Cause: ErrInvalidMetadata}
	}
	result := SourceFormat{ImageFormat: d.ImageFormat}
	if d.Video != nil {
		v := (formatMetadata{Video: *d.Video}).value().Video
		result.Video = &v
	}
	if d.Audio != nil {
		a := (formatMetadata{Audio: *d.Audio}).value().Audio
		result.Audio = &a
	}
	if err := result.Validate(); err != nil {
		return &MetadataError{Kind: "source", Version: d.Version, Cause: err}
	}
	*f = result
	return nil
}
