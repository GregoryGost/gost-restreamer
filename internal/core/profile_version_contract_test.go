package core_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"reflect"
	"strings"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// contractExpandedProfile задаёт полный x264-рецепт без обращения к изменяемым значениям по умолчанию.
// Качество 23.45 и high444/10-bit позволяют отдельно проверять единицы и нулевое качество.
func contractExpandedProfile() core.GroupProfile {
	return core.GroupProfile{
		Mode: core.ModeTranscode, LiveAudioMode: core.LiveAudioCopy,
		Format: core.MediaFormat{
			Video: core.VideoFormat{Codec: core.CodecH264, Width: 640, Height: 360, FrameRate: core.FrameRate{Numerator: 60, Denominator: 2}, CodecProfile: "high444", PixelFormat: "yuv444p10le", BitDepth: 10, Color: core.ColorInfo{Range: "limited", Primaries: "film", Transfer: "gamma22", Matrix: "fcc"}},
			Audio: core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", SampleRateHz: 48000, Channels: 6, ChannelLayout: "5.1"},
		},
		VideoEncoding: core.VideoEncoding{GOPFrames: 60, MaxBFrames: 2, ClosedGOP: true},
		AudioEncoding: core.AudioEncoding{BitrateBPS: 128000},
		Encoding:      core.EncodingSettings{VideoEncoder: "libx264", AudioEncoder: "aac", RateControl: "crf", CRF: core.OptionalCRF{Set: true, ValueHundredths: 2345}},
	}
}

// TestProfileNestedCopiesUseStrictReader сохраняет полный текущий профиль в каждом самостоятельном контейнере.
// Повреждённый CRF должен отклоняться и во вложенной копии, а не только при прямом DecodeProfile.
// t сообщает о потерянных полях и обходе строгого читателя.
func TestProfileNestedCopiesUseStrictReader(t *testing.T) {
	// profile задаёт новый рецепт; key, canonical и err связывают его с готовым манифестом и требованиями.
	profile := contractExpandedProfile()
	key, canonical, err := core.ProfileIdentity(profile)
	if err != nil {
		t.Fatal(err)
	}
	// session и output предоставляют корректные ссылки и самостоятельные снимки с новым полным профилем.
	session, output := contractSnapshots(t)
	session.Groups[0].Profile.Profile, output.Profile.Profile = profile, profile
	output.Artifact.Profile, output.Artifact.ProfileKey, output.Artifact.CanonicalProfile, output.Artifact.VerifiedFormat = profile, key, canonical, profile.Format
	// generation хранит отдельный RequiredProfile нового рецепта и точное владение путями.
	generation := validGeneration(t)
	generation.RequiredProfiles[0].Profile, generation.RequiredProfiles[0].ProfileKey, generation.RequiredProfiles[0].CanonicalProfile = profile, key, canonical
	// variant представляет готовый файл; его фактический формат соответствует именно расширенному профилю.
	variant := core.FallbackVariant{Record: contractRecord("variant"), GenerationID: "generation", AssetID: "asset", Incarnation: "owner", AttemptToken: "attempt", SourceHash: "source", Profile: profile, ProfileKey: key, CanonicalProfile: canonical, GeneratorVersion: core.CurrentFallbackGeneratorVersion, ManagedPath: "fallback/generation/variant.mp4", SizeBytes: 512, ContentHash: "content", State: core.VariantReady, VerifiedFormat: profile.Format}
	// scenario связывает конкретный контейнер с его типизированным получателем.
	for _, scenario := range []struct {
		// name различает сохраняемые границы; value задаёт исходный контейнер; receiver получает отдельную копию.
		name            string
		value, receiver any
	}{
		{"profile", core.TranscodeProfile{Record: contractRecord("profile"), Name: "Расширенный профиль", Profile: profile}, new(core.TranscodeProfile)},
		{"required", generation, new(core.FallbackGeneration)}, {"variant", variant, new(core.FallbackVariant)},
		{"session", session, new(core.SessionSnapshot)}, {"output", output, new(core.OutputSnapshot)}, {"manifest", output.Artifact, new(core.ArtifactManifest)},
	} {
		// Подтест сверяет контейнер целиком и затем проверяет отказ испорченной вложенной формы; t получает ошибки.
		t.Run(scenario.name, func(t *testing.T) {
			// raw содержит настоящую оболочку хранения; err сообщает об ошибке сериализации контейнера.
			raw, err := core.EncodeMetadata(scenario.value)
			if err != nil || !bytes.Contains(raw, []byte(`"CRF":{"Set":true,"ValueHundredths":2345}`)) {
				t.Fatalf("Не сохранён точный профиль: %s, %v", raw, err)
			}
			if err = core.DecodeMetadata(raw, scenario.receiver); err != nil || !reflect.DeepEqual(reflect.ValueOf(scenario.receiver).Elem().Interface(), scenario.value) {
				t.Fatalf("Самостоятельная копия изменилась: %v", err)
			}
			// damaged сохраняет весь контейнер, заменяя лишь поле CRF на форму предыдущей версии.
			damaged := bytes.Replace(raw, []byte(`"CRF":{"Set":true,"ValueHundredths":2345}`), []byte(`"CRF":{"Set":true,"Value":23.45}`), 1)
			if err = core.DecodeMetadata(damaged, scenario.receiver); !errors.Is(err, core.ErrInvalidMetadata) {
				t.Fatalf("Вложенный профиль обошёл строгий читатель: %v", err)
			}
		})
	}
}

// TestProfileStrictFailurePreservesReceiver проверяет отсутствие материализованных полей,
// неизвестные поля и неверные качества; получатель GroupProfile остаётся прежним при любом отказе.
// t фиксирует ошибочное принятие либо частичное присваивание получателю.
func TestProfileStrictFailurePreservesReceiver(t *testing.T) {
	// profile и raw задают полный допустимый исходник для прицельного удаления полей; err сообщает отказ записи.
	profile := contractExpandedProfile()
	raw, err := core.EncodeProfile(profile)
	if err != nil {
		t.Fatal(err)
	}
	// path перечисляет обязательные поля независимо от перечня, который использует сам читатель.
	for _, path := range []string{"LiveAudioMode", "Encoding.QP.Set", "Encoding.QP.Value", "Encoding.CRF.Set", "Encoding.CRF.ValueHundredths", "Encoding.NVENC.AQStrength", "Encoding.SVTAV1.Preset", "VideoEncoding.ClosedGOP", "Format.Video.Level", "Format.Video.Tier", "Format.Video.FrameRate.Denominator", "Format.Audio.ChannelLayout"} {
		// root хранит независимое JSON-дерево для удаления одного обязательного поля.
		var root map[string]any
		if err = json.Unmarshal(raw, &root); err != nil {
			t.Fatal(err)
		}
		// parts задаёт путь удаления; object проходит только известные объектные узлы фикстуры.
		parts := strings.Split(path, ".")
		object := root
		// part выбирает промежуточные объекты, оставляя последнюю часть для удаления поля.
		for _, part := range parts[:len(parts)-1] {
			object = object[part].(map[string]any)
		}
		delete(object, parts[len(parts)-1])
		// damaged содержит неполный профиль, err сообщает об ошибке JSON; receiver и before защищают атомарность чтения.
		damaged, err := json.Marshal(root)
		if err != nil {
			t.Fatal(err)
		}
		receiver := profile
		receiver.Encoding.CRF.ValueHundredths = 1750
		before := receiver
		if err = receiver.UnmarshalJSON(damaged); !errors.Is(err, core.ErrInvalidMetadata) || receiver != before {
			t.Fatalf("Пропуск %s принят или изменил получателя: %v", path, err)
		}
	}
	// replacement содержит только неверные формы качества: отсутствие, null, чужие единицы и лишние поля.
	for _, replacement := range []string{`null`, `{}`, `{"Set":true,"ValueHundredths":null}`, `{"Set":false,"ValueHundredths":2345}`, `{"Set":true,"ValueHundredths":-1}`, `{"Set":true,"ValueHundredths":2345.1}`, `{"Set":true,"ValueHundredths":2345,"extra":0}`, `{"Set":true,"Value":23.45}`} {
		// damaged меняет только объект CRF; receiver и before сохраняют контрольное значение.
		damaged := bytes.Replace(raw, []byte(`{"Set":true,"ValueHundredths":2345}`), []byte(replacement), 1)
		receiver := profile
		before := receiver
		if err = receiver.UnmarshalJSON(damaged); !errors.Is(err, core.ErrInvalidMetadata) || receiver != before {
			t.Fatalf("Повреждённое качество принято: %s, %v", replacement, err)
		}
	}
}
