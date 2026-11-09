package core_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

type frozenLiveAudioProfile struct {
	Name                      string
	Payload, Canonical        []byte
	ProfileKey, VariantKey    string
	Session, Output, Required []byte
}

func frozenLiveAudioProfiles(t *testing.T) []frozenLiveAudioProfile {
	t.Helper()
	// Эталоны получены исходным кодом a1c25b61fc981bdba42391ab7c51f61665bb3d97 в отдельном временном модуле.
	data, err := os.ReadFile("testdata/live_audio_legacy_v2/profiles.json")
	if err != nil {
		t.Fatal(err)
	}
	var profiles []frozenLiveAudioProfile
	if err = json.Unmarshal(data, &profiles); err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 2 {
		t.Fatalf("Нужны прежние AV1 и H.264: %d", len(profiles))
	}
	return profiles
}

func TestLiveAudioLegacyProfilesAndNestedBytesRemainExact(t *testing.T) {
	for _, fixture := range frozenLiveAudioProfiles(t) {
		t.Run(fixture.Name, func(t *testing.T) {
			p, err := core.DecodeProfile(fixture.Payload)
			if err != nil {
				t.Fatal(err)
			}
			wantMode := core.LiveAudioCopy
			if fixture.Name == "h264" {
				wantMode = core.LiveAudioTranscode
			}
			if p.LiveAudioMode != "" || p.EffectiveLiveAudioMode() != wantMode {
				t.Fatalf("Изменена прежняя семантика: %+v", p)
			}
			if err = p.Validate(); err != nil || p.LiveAudioMode != "" {
				t.Fatalf("Validate материализовал селектор: %v", err)
			}
			got, err := core.EncodeProfile(p)
			if err != nil || !bytes.Equal(got, fixture.Payload) {
				t.Fatalf("Изменены исходные байты v2: %v", err)
			}
			key, canonical, err := core.ProfileIdentity(p)
			if err != nil || key != fixture.ProfileKey || !bytes.Equal(canonical, fixture.Canonical) {
				t.Fatalf("Изменена прежняя идентичность: %s %v", key, err)
			}
			variant, err := core.VariantIdentity("frozen-source", p, "generator-v1")
			if err != nil || variant != fixture.VariantKey {
				t.Fatalf("Изменён ключ варианта: %s %v", variant, err)
			}
			session, err := core.DecodeSessionSnapshot(fixture.Session)
			if err != nil {
				t.Fatal(err)
			}
			got, err = core.EncodeSessionSnapshot(session)
			if err != nil || !bytes.Equal(got, fixture.Session) || session.Groups[0].Profile.Profile != p {
				t.Fatalf("Изменён вложенный Session: %v", err)
			}
			output, err := core.DecodeOutputSnapshot(fixture.Output)
			if err != nil {
				t.Fatal(err)
			}
			got, err = core.EncodeOutputSnapshot(output)
			if err != nil || !bytes.Equal(got, fixture.Output) || output.Artifact.Profile != p || output.Profile.Profile != p {
				t.Fatalf("Изменён Output/manifest: %v", err)
			}
			var required core.RequiredProfile
			if err = core.DecodeMetadata(fixture.Required, &required); err != nil {
				t.Fatal(err)
			}
			got, err = core.EncodeMetadata(required)
			if err != nil || !bytes.Equal(got, fixture.Required) || required.Profile != p {
				t.Fatalf("Изменён RequiredProfile: %v", err)
			}
		})
	}
}

func TestLiveAudioExplicitSelectionChangesIdentityAndUsesSeparateWireVersion(t *testing.T) {
	for _, fixture := range frozenLiveAudioProfiles(t) {
		legacy, err := core.DecodeProfile(fixture.Payload)
		if err != nil {
			t.Fatal(err)
		}
		for _, mode := range []core.LiveAudioMode{core.LiveAudioCopy, core.LiveAudioTranscode} {
			if legacy.Mode == core.ModeCopy && mode == core.LiveAudioTranscode {
				continue
			}
			p := legacy
			p.LiveAudioMode = mode
			data, err := core.EncodeProfile(p)
			if err != nil {
				t.Fatal(err)
			}
			var root map[string]json.RawMessage
			if err = json.Unmarshal(data, &root); err != nil {
				t.Fatal(err)
			}
			if string(root["Version"]) != "3" || string(root["LiveAudioMode"]) != `"`+string(mode)+`"` || p.Encoding.Version != 2 || core.MetadataVersion != 1 {
				t.Fatalf("Смешаны независимые версии: %s", data)
			}
			decoded, err := core.DecodeProfile(data)
			if err != nil || decoded != p {
				t.Fatalf("Селектор не пережил хранение: %+v %v", decoded, err)
			}
			key, _, err := core.ProfileIdentity(p)
			if err != nil || key == fixture.ProfileKey || p.Equal(legacy) {
				t.Fatalf("Явный селектор не влияет на идентичность: %v", err)
			}
			other := p
			if mode == core.LiveAudioCopy && p.Mode == core.ModeTranscode {
				other.LiveAudioMode = core.LiveAudioTranscode
				otherKey, _, e := core.ProfileIdentity(other)
				if e != nil || otherKey == key || other.Equal(p) {
					t.Fatalf("copy/transcode получили общий ключ: %v", e)
				}
			}
		}
	}
}

func TestLiveAudioStoredExplicitProfileRejectsMissingNullAndUnknownFields(t *testing.T) {
	p := validProfile()
	p.LiveAudioMode = core.LiveAudioCopy
	data, err := core.EncodeProfile(p)
	if err != nil {
		t.Fatal(err)
	}
	decoded, err := core.DecodeProfile(data)
	if err != nil || decoded != p {
		t.Fatalf("Нулевой Encoding в v3 должен сохраняться: %v", err)
	}
	for _, field := range []string{"Version", "Mode", "LiveAudioMode", "Format", "VideoEncoding", "AudioEncoding", "Encoding"} {
		for _, mutation := range []string{"missing", "null"} {
			if field == "Encoding" && mutation == "null" {
				continue
			}
			t.Run(field+"/"+mutation, func(t *testing.T) {
				var root map[string]json.RawMessage
				if err := json.Unmarshal(data, &root); err != nil {
					t.Fatal(err)
				}
				if mutation == "missing" {
					delete(root, field)
				} else {
					root[field] = json.RawMessage("null")
				}
				bad, err := json.Marshal(root)
				if err != nil {
					t.Fatal(err)
				}
				receiver := p
				if err = receiver.UnmarshalJSON(bad); !errors.Is(err, core.ErrInvalidMetadata) || receiver != p {
					t.Fatalf("Частичное/нулевое поле принято или изменило приёмник: %v", err)
				}
			})
		}
	}
	for _, selector := range []string{`""`, `"automatic"`, `true`, `2`} {
		var root map[string]json.RawMessage
		if err = json.Unmarshal(data, &root); err != nil {
			t.Fatal(err)
		}
		root["LiveAudioMode"] = json.RawMessage(selector)
		bad, err := json.Marshal(root)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = core.DecodeProfile(bad); !errors.Is(err, core.ErrInvalidMetadata) {
			t.Fatalf("Недопустимый селектор %s принят: %v", selector, err)
		}
	}
	var unknown map[string]json.RawMessage
	if err = json.Unmarshal(data, &unknown); err != nil {
		t.Fatal(err)
	}
	unknown["Unexpected"] = json.RawMessage("true")
	bad, err := json.Marshal(unknown)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = core.DecodeProfile(bad); !errors.Is(err, core.ErrInvalidMetadata) {
		t.Fatalf("Неизвестное поле сохранённого v3 принято: %v", err)
	}
	p.LiveAudioMode = core.LiveAudioTranscode
	if err = p.Validate(); !errors.Is(err, core.ErrInvalidProfile) {
		t.Fatalf("Видео copy приняло audio transcode: %v", err)
	}
}
