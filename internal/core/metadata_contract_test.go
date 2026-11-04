package core_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

func requireMetadataError(t *testing.T, err error) {
	t.Helper()
	var metadata *core.MetadataError
	if !errors.Is(err, core.ErrInvalidMetadata) || !errors.As(err, &metadata) || metadata.Cause == nil || metadata.Error() == "" {
		t.Fatalf("потеря категории или причины ошибки метаданных: %v", err)
	}
	if !errors.Is(err, metadata.Cause) {
		t.Fatalf("причина ошибки не доступна через errors.Is: %v", err)
	}
}

func TestOutputSnapshotMetadataPreservesHistoricalConfiguration(t *testing.T) {
	_, expected := contractSnapshots(t)
	expected.Destination.Revision = math.MaxUint64
	expected.Artifact.SizeBytes = math.MaxUint64
	expected.Profile.Profile.Format.Video.FrameRate = core.FrameRate{Numerator: 120, Denominator: 2}
	expected.Artifact.Profile = expected.Profile.Profile
	key, canonical, err := core.ProfileIdentity(expected.Artifact.Profile)
	if err != nil {
		t.Fatal(err)
	}
	expected.Artifact.ProfileKey, expected.Artifact.CanonicalProfile = key, canonical
	originalCanonical := bytes.Clone(canonical)
	data, err := core.EncodeOutputSnapshot(expected)
	if err != nil {
		t.Fatal(err)
	}
	actual, err := core.DecodeOutputSnapshot(data)
	if err != nil || !reflect.DeepEqual(actual, expected) {
		t.Fatalf("исторический снимок изменён после хранения: %+v, %v", actual, err)
	}
	actual.Artifact.CanonicalProfile[0] ^= 1
	if !bytes.Equal(expected.Artifact.CanonicalProfile, originalCanonical) {
		t.Fatal("восстановленный снимок изменил исходные байты manifest")
	}
}

func TestSourceMetadataPreservesObservedOriginalInsteadOfOutputProfile(t *testing.T) {
	raw := string([]byte{' ', 0xff, 0, 'x', ' '})
	video := core.VideoFormat{Codec: "source-video-codec", Width: 73, Height: 41, FrameRate: core.FrameRate{Numerator: 120, Denominator: 2}, CodecProfile: raw, Level: raw, Tier: raw, PixelFormat: raw, BitDepth: 12, Color: core.ColorInfo{Range: raw, Primaries: raw, Transfer: raw, Matrix: raw}}
	audio := core.AudioFormat{Codec: "source-audio-codec", CodecProfile: raw, SampleRateHz: 44100, Channels: 1, ChannelLayout: raw}
	for _, expected := range []core.SourceFormat{
		{}, {ImageFormat: "png"}, {Video: &video}, {Audio: &audio}, {Video: &video, Audio: &audio},
	} {
		data, err := json.Marshal(expected)
		if err != nil {
			t.Fatal(err)
		}
		var actual core.SourceFormat
		if err := json.Unmarshal(data, &actual); err != nil || !reflect.DeepEqual(actual, expected) {
			t.Fatalf("описание исходника изменено: %+v, %v", actual, err)
		}
	}
}

func TestTypedMediaMetadataRejectsCorruption(t *testing.T) {
	format := validProfile().Format
	formatData, err := core.EncodeMediaFormat(format)
	if err != nil {
		t.Fatal(err)
	}
	source := core.SourceFormat{Video: &format.Video, Audio: &format.Audio, ImageFormat: "png"}
	sourceData, err := json.Marshal(source)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		data []byte
		run  func([]byte) error
	}{
		{"format", formatData, func(data []byte) error {
			actual := format
			return actual.UnmarshalJSON(data)
		}},
		{"source", sourceData, func(data []byte) error {
			receiverFormat := format
			actual := core.SourceFormat{Video: &receiverFormat.Video, Audio: &receiverFormat.Audio, ImageFormat: "png"}
			return actual.UnmarshalJSON(data)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, input := range [][]byte{
				nil, []byte("null"), []byte("{"), []byte(`{"Version":1,"Unknown":true}`),
				bytes.Replace(tc.data, []byte(`"Version":1`), []byte(`"Version":2`), 1),
				bytes.Replace(tc.data, []byte(`"Video":{`), []byte(`"Video":{"Unknown":true,`), 1),
				bytes.Replace(tc.data, []byte(`"Codec":"av1"`), []byte(`"Codec":""`), 1),
				append(bytes.Clone(tc.data), []byte(" true")...),
				append(bytes.Clone(tc.data), []byte(" {")...),
			} {
				requireMetadataError(t, tc.run(input))
			}
		})
	}
	invalidFormat := format
	invalidFormat.Video.Width = 0
	_, err = core.EncodeMediaFormat(invalidFormat)
	requireMetadataError(t, err)
	invalidSource := source
	invalidSource.ImageFormat = "\x00"
	_, err = json.Marshal(invalidSource)
	requireMetadataError(t, err)
}

func TestSnapshotMetadataRejectsCorruptAndInconsistentSnapshots(t *testing.T) {
	session, output := contractSnapshots(t)
	sessionData, err := core.EncodeSessionSnapshot(session)
	if err != nil {
		t.Fatal(err)
	}
	outputData, err := core.EncodeOutputSnapshot(output)
	if err != nil {
		t.Fatal(err)
	}
	for _, tc := range []struct {
		name string
		data []byte
		run  func([]byte) error
	}{
		{"session", sessionData, func(data []byte) error { _, err := core.DecodeSessionSnapshot(data); return err }},
		{"output", outputData, func(data []byte) error { _, err := core.DecodeOutputSnapshot(data); return err }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			for _, input := range [][]byte{
				[]byte("{"), []byte("null"), []byte(`{"Version":1,"Snapshot":{},"Unknown":1}`),
				bytes.Replace(tc.data, []byte(`"Version":1`), []byte(`"Version":2`), 1),
				bytes.Replace(tc.data, []byte(`"StreamID":"stream"`), []byte(`"StreamID":"foreign"`), 1),
				append(bytes.Clone(tc.data), []byte(" {}")...),
				append(bytes.Clone(tc.data), []byte(" !")...),
			} {
				requireMetadataError(t, tc.run(input))
			}
		})
	}
	session.Stream.Name = ""
	_, err = core.EncodeSessionSnapshot(session)
	requireMetadataError(t, err)
	output.Artifact.ContentHash = ""
	_, err = core.EncodeOutputSnapshot(output)
	requireMetadataError(t, err)
}

func TestDomainMetadataPreservesNumbersAndRejectsUnsupportedEnvelope(t *testing.T) {
	expected := contractReceipt()
	expected.Lifecycle, expected.Attempt = math.MaxUint64, math.MaxUint64
	expected.Status = core.CommandClaimed
	data, err := core.EncodeMetadata(expected)
	if err != nil {
		t.Fatal(err)
	}
	var actual core.CommandReceipt
	if err := core.DecodeMetadata(data, &actual); err != nil || !reflect.DeepEqual(actual, expected) {
		t.Fatalf("квитанция изменилась после хранения: %+v, %v", actual, err)
	}
	for _, input := range [][]byte{
		[]byte("{"), []byte(`{"Version":999,"Payload":{}}`), []byte(`{"Version":1}`),
		[]byte(`{"Version":1,"Payload":null}`), []byte(`{"Version":1,"Payload":{},"Unknown":1}`),
		[]byte(`{"Version":1,"Payload":{"Unknown":1}}`), []byte(`{"Version":1,"Payload":{}}`),
		append(bytes.Clone(data), []byte(" {}")...),
		bytes.Replace(data, []byte(`"Lifecycle":18446744073709551615`), []byte(`"Lifecycle":18446744073709551616`), 1),
	} {
		var receiver core.CommandReceipt
		requireMetadataError(t, core.DecodeMetadata(input, &receiver))
	}
	_, err = core.EncodeMetadata(make(chan int))
	requireMetadataError(t, err)
	plain := struct{ Name string }{Name: "Синтетические данные"}
	data, err = core.EncodeMetadata(plain)
	if err != nil {
		t.Fatal(err)
	}
	var decoded struct{ Name string }
	if err := core.DecodeMetadata(data, &decoded); err != nil || decoded != plain {
		t.Fatalf("обычное типизированное значение изменено: %+v, %v", decoded, err)
	}
}

func TestMetadataErrorsExposeKindVersionAndCause(t *testing.T) {
	cause := fmt.Errorf("причина: %w", core.ErrInvalid)
	err := &core.MetadataError{Kind: "format", Version: 99, Cause: cause}
	if !errors.Is(err, core.ErrInvalid) || !errors.Is(err, core.ErrInvalidMetadata) || errors.Is(err, core.ErrNotFound) {
		t.Fatalf("неверная цепочка ошибок: %v", err)
	}
	for _, value := range []string{"format", "99", cause.Error()} {
		if !strings.Contains(err.Error(), value) {
			t.Fatalf("диагностика не содержит %q: %v", value, err)
		}
	}
}
