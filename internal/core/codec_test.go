package core_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

func TestProfileMetadataPreservesExactAcceptedValues(t *testing.T) {
	p := validProfile()
	p.Format.Video.FrameRate = core.FrameRate{Numerator: 60000, Denominator: 2002}
	p.VideoEncoding.TargetBitrateBPS = math.MaxUint64 - 1
	p.VideoEncoding.MaxBitrateBPS = math.MaxUint64
	p.VideoEncoding.BufferBits = math.MaxUint64
	p.AudioEncoding.BitrateBPS = math.MaxUint64
	raw := string([]byte{' ', 0xff, 0, 'x', ' '})
	p.Format.Video.CodecProfile, p.Format.Video.Level, p.Format.Video.Tier = raw, raw, raw
	p.Format.Video.PixelFormat = raw
	p.Format.Video.Color = core.ColorInfo{Range: raw, Primaries: raw, Transfer: raw, Matrix: raw}
	p.Format.Audio.CodecProfile, p.Format.Audio.ChannelLayout = raw, raw
	if err := p.Validate(); err != nil {
		t.Fatalf("исходный принятый профиль недопустим: %v", err)
	}
	data, err := core.EncodeProfile(p)
	if err != nil {
		t.Fatal(err)
	}
	got, err := core.DecodeProfile(data)
	if err != nil || got != p {
		t.Fatalf("профиль после хранения изменён: ошибка %v, получен %#v", err, got)
	}
	formatData, err := core.EncodeMediaFormat(p.Format)
	if err != nil {
		t.Fatal(err)
	}
	format, err := core.DecodeMediaFormat(formatData)
	if err != nil || format != p.Format {
		t.Fatalf("формат после хранения изменён: ошибка %v, получен %#v", err, format)
	}
	now := time.Date(2026, 1, 2, 3, 4, 5, 0, time.UTC)
	record := func(id core.ID) core.Record {
		return core.Record{ID: id, Revision: math.MaxUint64, CreatedAt: now, UpdatedAt: now}
	}
	snapshot := core.SessionSnapshot{
		Stream: core.Stream{Record: record("stream"), Name: "Поток", PublicationKey: "synthetic-key"},
		Groups: []core.GroupSnapshot{{
			Group:   core.TranscodeGroup{Record: record("group"), StreamID: "stream", ProfileID: "profile", Name: "Группа"},
			Profile: core.TranscodeProfile{Record: record("profile"), Name: "Профиль", Profile: p},
		}},
	}
	snapshotData, err := core.EncodeSessionSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	restored, err := core.DecodeSessionSnapshot(snapshotData)
	if err != nil || !reflect.DeepEqual(snapshot, restored) {
		t.Fatalf("снимок изменён после хранения: ошибка %v", err)
	}
}

func TestProfileIdentityUsesRationalFPSAndEveryProfileField(t *testing.T) {
	p := validProfile()
	key, content, err := core.ProfileIdentity(p)
	if err != nil || len(key) != 64 || len(content) == 0 {
		t.Fatalf("идентичность профиля: %q, %v", key, err)
	}
	equivalent := p
	equivalent.Format.Video.FrameRate = core.FrameRate{Numerator: 120, Denominator: 2}
	equivalentKey, equivalentContent, err := core.ProfileIdentity(equivalent)
	if err != nil || equivalentKey != key || !bytes.Equal(content, equivalentContent) {
		t.Fatalf("равные дроби получили разные идентичности: %v", err)
	}
	if equivalent.Format.Video.FrameRate.Denominator != 2 {
		t.Fatal("канонизация изменила исходный профиль")
	}
	changes := []struct {
		name  string
		apply func(*core.GroupProfile)
	}{
		{"mode", func(p *core.GroupProfile) { p.Mode = core.ModeTranscode }},
		{"video_codec", func(p *core.GroupProfile) { p.Format.Video.Codec = core.CodecH264 }},
		{"width", func(p *core.GroupProfile) { p.Format.Video.Width++ }},
		{"height", func(p *core.GroupProfile) { p.Format.Video.Height++ }},
		{"fps", func(p *core.GroupProfile) { p.Format.Video.FrameRate.Numerator++ }},
		{"video_profile", func(p *core.GroupProfile) { p.Format.Video.CodecProfile = " main " }},
		{"level", func(p *core.GroupProfile) { p.Format.Video.Level = "4.1" }},
		{"tier", func(p *core.GroupProfile) { p.Format.Video.Tier = "main" }},
		{"pixel_format", func(p *core.GroupProfile) { p.Format.Video.PixelFormat = "yuv420p10le" }},
		{"bit_depth", func(p *core.GroupProfile) { p.Format.Video.BitDepth++ }},
		{"range", func(p *core.GroupProfile) { p.Format.Video.Color.Range = "full" }},
		{"primaries", func(p *core.GroupProfile) { p.Format.Video.Color.Primaries = "bt2020" }},
		{"transfer", func(p *core.GroupProfile) { p.Format.Video.Color.Transfer = "smpte2084" }},
		{"matrix", func(p *core.GroupProfile) { p.Format.Video.Color.Matrix = "bt2020nc" }},
		{"audio_profile", func(p *core.GroupProfile) { p.Format.Audio.CodecProfile = "lc" }},
		{"sample_rate", func(p *core.GroupProfile) { p.Format.Audio.SampleRateHz++ }},
		{"channels", func(p *core.GroupProfile) { p.Format.Audio.Channels++ }},
		{"channel_layout", func(p *core.GroupProfile) { p.Format.Audio.ChannelLayout = "mono" }},
		{"target", func(p *core.GroupProfile) { p.VideoEncoding.TargetBitrateBPS++ }},
		{"maximum", func(p *core.GroupProfile) { p.VideoEncoding.MaxBitrateBPS++ }},
		{"buffer", func(p *core.GroupProfile) { p.VideoEncoding.BufferBits++ }},
		{"gop", func(p *core.GroupProfile) { p.VideoEncoding.GOPFrames++ }},
		{"b_frames", func(p *core.GroupProfile) { p.VideoEncoding.MaxBFrames++ }},
		{"closed_gop", func(p *core.GroupProfile) { p.VideoEncoding.ClosedGOP = false }},
		{"audio_bitrate", func(p *core.GroupProfile) { p.AudioEncoding.BitrateBPS++ }},
	}
	for _, change := range changes {
		t.Run(change.name, func(t *testing.T) {
			other := p
			change.apply(&other)
			otherKey, otherContent, err := core.ProfileIdentity(other)
			if err != nil || otherKey == key || bytes.Equal(otherContent, content) {
				t.Fatalf("изменение поля не различено: ключ %q, ошибка %v", otherKey, err)
			}
		})
	}
	variant, err := core.VariantIdentity("source", p, "generator")
	if err != nil {
		t.Fatal(err)
	}
	for _, input := range []struct{ source, generator string }{{"other-source", "generator"}, {"source", "other-generator"}, {"sourcegenerator", ""}, {"source", "generator "}} {
		other, err := core.VariantIdentity(input.source, p, input.generator)
		if err != nil || other == variant {
			t.Fatalf("источник и генератор не различены: %v", err)
		}
	}
}

func TestProfileMetadataRejectsUnknownCorruptAndInvalidValues(t *testing.T) {
	valid, err := core.EncodeProfile(validProfile())
	if err != nil {
		t.Fatal(err)
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(valid, &fields); err != nil {
		t.Fatal(err)
	}
	fields["Version"] = json.RawMessage("999")
	unknown, err := json.Marshal(fields)
	if err != nil {
		t.Fatal(err)
	}
	inputs := [][]byte{nil, []byte("null"), []byte("{}"), []byte("{"), append(append([]byte{}, valid...), []byte(" {}")...), unknown, bytes.Replace(valid, []byte(`"Numerator":60`), []byte(`"Numerator":0`), 1), bytes.Replace(valid, []byte(`"BitrateBPS":192000`), []byte(`"BitrateBPS":18446744073709551616`), 1)}
	for _, input := range inputs {
		before := validProfile()
		receiver := before
		err := json.Unmarshal(input, &receiver)
		if err == nil {
			t.Fatalf("некорректные метаданные приняты: %s", input)
		}
		// Синтаксические ошибки JSON может вернуть внешний json.Unmarshal.
		_, decodeErr := core.DecodeProfile(input)
		var metadata *core.MetadataError
		if !errors.Is(decodeErr, core.ErrInvalidMetadata) || !errors.As(decodeErr, &metadata) {
			t.Fatalf("DecodeProfile потерял тип ошибки для %s: %v", input, decodeErr)
		}
		if receiver != before {
			t.Fatalf("ошибка частично изменила приёмник для %s", input)
		}
	}
	for _, decode := range []func([]byte) error{
		func(b []byte) error { _, e := core.DecodeMediaFormat(b); return e },
		func(b []byte) error { _, e := core.DecodeSessionSnapshot(b); return e },
		func(b []byte) error { _, e := core.DecodeOutputSnapshot(b); return e },
		func(b []byte) error { var v core.Record; return core.DecodeMetadata(b, &v) },
	} {
		if err := decode([]byte(`{"Version":999}`)); !errors.Is(err, core.ErrInvalidMetadata) {
			t.Fatalf("неизвестная версия метаданных принята: %v", err)
		}
	}
}
