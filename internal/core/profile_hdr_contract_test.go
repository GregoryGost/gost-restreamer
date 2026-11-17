package core_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// storedHEVCHDRProfile задаёт полную сохраняемую форму текущего профиля независимо от текущих значений по умолчанию.
// CRF23.51 хранится целыми сотыми; пустые level/tier и нулевые неприменимые блоки материализованы.
const storedHEVCHDRProfile = `{"Mode":"transcode","LiveAudioMode":"copy","Format":{"Version":1,"Video":{"Codec":"hevc","Width":320,"Height":180,"FrameRate":{"Numerator":24,"Denominator":1},"CodecProfile":"bWFpbjEw","Level":"","Tier":"","PixelFormat":"eXV2NDIwcDEwbGU=","BitDepth":10,"Range":"bGltaXRlZA==","Primaries":"YnQyMDIw","Transfer":"c21wdGUyMDg0","Matrix":"YnQyMDIwbmM="},"Audio":{"Codec":"aac","CodecProfile":"TEM=","SampleRateHz":48000,"Channels":2,"ChannelLayout":"c3RlcmVv"}},"VideoEncoding":{"TargetBitrateBPS":0,"MaxBitrateBPS":0,"BufferBits":0,"GOPFrames":48,"MaxBFrames":0,"ClosedGOP":true},"AudioEncoding":{"BitrateBPS":128000},"Encoding":{"QP":{"Set":false,"Value":0},"CRF":{"Set":true,"ValueHundredths":2351},"VideoEncoder":"libx265","AudioEncoder":"aac","RateControl":"crf","RescaleOutput":false,"VideoCustom":"","AudioCustom":"","NVENC":{"Preset":"","Tuning":"","Multipass":"","LookaheadFrames":0,"EnableSpatialAQ":false,"EnableTemporalAQ":false,"AQStrength":0},"SVTAV1":{"Preset":0,"Tuning":"","AQMode":0,"LookaheadFrames":0}}}`

// literalHEVCHDRProfile описывает тот же пользовательский контракт в доменных значениях.
// Функция не вызывает DefaultEncoding: изменение значений по умолчанию не должно менять смысл сохранённой формы.
func literalHEVCHDRProfile() core.GroupProfile {
	return core.GroupProfile{
		Mode: core.ModeTranscode, LiveAudioMode: core.LiveAudioCopy,
		Format: core.MediaFormat{
			Video: core.VideoFormat{Codec: core.CodecHEVC, Width: 320, Height: 180, FrameRate: core.FrameRate{Numerator: 24, Denominator: 1}, CodecProfile: "main10", PixelFormat: "yuv420p10le", BitDepth: 10, Color: core.ColorInfo{Range: "limited", Primaries: "bt2020", Transfer: "smpte2084", Matrix: "bt2020nc"}},
			Audio: core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", SampleRateHz: 48000, Channels: 2, ChannelLayout: "stereo"},
		},
		VideoEncoding: core.VideoEncoding{GOPFrames: 48, ClosedGOP: true},
		AudioEncoding: core.AudioEncoding{BitrateBPS: 128000},
		Encoding:      core.EncodingSettings{CRF: core.OptionalCRF{Set: true, ValueHundredths: 2351}, VideoEncoder: "libx265", AudioEncoder: "aac", RateControl: "crf"},
	}
}

// TestProfileHDRLiteralBytesAndIdentity проверяет точный текущий профиль и сохранение отдельного LIVE-аудио.
// Одна сотая CRF, режим аудио и transfer обязаны различать полную идентичность; эквивалентный FPS — нет.
// t сообщает об ошибках проверяемого сценария.
func TestProfileHDRLiteralBytesAndIdentity(t *testing.T) {
	// expected задаёт полный независимый доменный эталон; decoded и err проверяют буквальную форму хранения.
	expected := literalHEVCHDRProfile()
	decoded, err := core.DecodeProfile([]byte(storedHEVCHDRProfile))
	if err != nil || decoded != expected || decoded.EffectiveLiveAudioMode() != core.LiveAudioCopy {
		t.Fatalf("Смысл сохранённого профиля изменился: %+v, %v", decoded, err)
	}
	// raw проверяет не только round-trip структуры, но и точные единицы, состав и порядок полей.
	raw, err := core.EncodeProfile(expected)
	if err != nil || string(raw) != storedHEVCHDRProfile {
		t.Fatalf("Байты профиля изменились: %s, %v", raw, err)
	}
	// key и canonical задают идентичность полного первоначального профиля.
	key, canonical, err := core.ProfileIdentity(expected)
	if err != nil || string(canonical) != storedHEVCHDRProfile {
		t.Fatalf("Неверная каноническая форма: %s, %v", canonical, err)
	}
	// equivalent изменяет только представление частоты, сохраняя математическое значение 24 FPS.
	equivalent := expected
	equivalent.Format.Video.FrameRate = core.FrameRate{Numerator: 48, Denominator: 2}
	// equivalentKey обязан совпасть без изменения несокращённого значения в самом профиле.
	equivalentKey, _, err := core.ProfileIdentity(equivalent)
	if err != nil || equivalentKey != key || equivalent.Format.Video.FrameRate.Numerator != 48 {
		t.Fatalf("Эквивалентный FPS изменил идентичность либо исходник: %s, %v", equivalentKey, err)
	}
	// variants содержат три самостоятельные допустимые мутации: сотая CRF, режим аудио и HLG вместо PQ.
	variants := []core.GroupProfile{expected, expected, expected}
	variants[0].Encoding.CRF.ValueHundredths = 2352
	variants[1].LiveAudioMode = core.LiveAudioTranscode
	variants[2].Format.Video.Color.Transfer = "arib-std-b67"
	// changed содержит очередной профиль с отличием ровно в одном существенном поле.
	for _, changed := range variants {
		// changedKey различает полный рецепт и исходный артефакт; err сообщает об отказе канонизации.
		changedKey, _, err := core.ProfileIdentity(changed)
		if err != nil || changedKey == key {
			t.Fatalf("Различные полные профили получили одну идентичность: %+v, %v", changed, err)
		}
	}
}

// damagedStoredField меняет одно поле буквального профиля, не используя таблицы рабочего читателя.
// path выбирает существующий объектный путь; remove удаляет ключ, иначе подставляется replacement.
// t прерывает проверку при ошибке самой тестовой фикстуры.
func damagedStoredField(t *testing.T, path string, remove bool, replacement any) []byte {
	t.Helper()
	// root содержит самостоятельное дерево фикстуры; err сообщает о неверной буквальной форме теста.
	var root map[string]any
	if err := json.Unmarshal([]byte(storedHEVCHDRProfile), &root); err != nil {
		t.Fatal(err)
	}
	// parts задаёт путь до проверяемого поля, object проходит только промежуточные объекты.
	parts := strings.Split(path, ".")
	object := root
	// part обозначает следующее имя промежуточного JSON-объекта фикстуры.
	for _, part := range parts[:len(parts)-1] {
		object = object[part].(map[string]any)
	}
	if remove {
		delete(object, parts[len(parts)-1])
	} else {
		object[parts[len(parts)-1]] = replacement
	}
	// raw и err содержат повреждённую форму и ошибку сериализации тестовой мутации.
	raw, err := json.Marshal(root)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// TestProfileHDRRequiresEveryStoredField проверяет полноту профиля и атомарность отказа.
// Включены false, нули, пустые строки и неприменимые блоки: отсутствующее или null не заменяется значением по умолчанию.
// t сообщает об ошибках проверяемого сценария.
func TestProfileHDRRequiresEveryStoredField(t *testing.T) {
	// path перечисляет обязательный состав контракта независимо от requiredProfileObject реализации.
	for _, path := range []string{
		"Mode", "LiveAudioMode", "Format", "VideoEncoding", "AudioEncoding", "Encoding",
		"Format.Version", "Format.Video", "Format.Audio",
		"Format.Video.Codec", "Format.Video.Width", "Format.Video.Height", "Format.Video.FrameRate", "Format.Video.FrameRate.Numerator", "Format.Video.FrameRate.Denominator",
		"Format.Video.CodecProfile", "Format.Video.Level", "Format.Video.Tier", "Format.Video.PixelFormat", "Format.Video.BitDepth", "Format.Video.Range", "Format.Video.Primaries", "Format.Video.Transfer", "Format.Video.Matrix",
		"Format.Audio.Codec", "Format.Audio.CodecProfile", "Format.Audio.SampleRateHz", "Format.Audio.Channels", "Format.Audio.ChannelLayout",
		"VideoEncoding.TargetBitrateBPS", "VideoEncoding.MaxBitrateBPS", "VideoEncoding.BufferBits", "VideoEncoding.GOPFrames", "VideoEncoding.MaxBFrames", "VideoEncoding.ClosedGOP", "AudioEncoding.BitrateBPS",
		"Encoding.QP", "Encoding.QP.Set", "Encoding.QP.Value", "Encoding.CRF", "Encoding.CRF.Set", "Encoding.CRF.ValueHundredths",
		"Encoding.VideoEncoder", "Encoding.AudioEncoder", "Encoding.RateControl", "Encoding.RescaleOutput", "Encoding.VideoCustom", "Encoding.AudioCustom", "Encoding.NVENC", "Encoding.SVTAV1",
		"Encoding.NVENC.Preset", "Encoding.NVENC.Tuning", "Encoding.NVENC.Multipass", "Encoding.NVENC.LookaheadFrames", "Encoding.NVENC.EnableSpatialAQ", "Encoding.NVENC.EnableTemporalAQ", "Encoding.NVENC.AQStrength",
		"Encoding.SVTAV1.Preset", "Encoding.SVTAV1.Tuning", "Encoding.SVTAV1.AQMode", "Encoding.SVTAV1.LookaheadFrames",
	} {
		// remove различает два самостоятельных нарушения: отсутствие ключа и явный null.
		for _, remove := range []bool{true, false} {
			// input содержит единственную повреждённую границу; receiver и before защищают прежнее значение.
			input := damagedStoredField(t, path, remove, nil)
			receiver := literalHEVCHDRProfile()
			receiver.Encoding.CRF.ValueHundredths = 1700
			before := receiver
			// err обязан сообщить отказ метаданных до изменения получателя.
			if err := receiver.UnmarshalJSON(input); !errors.Is(err, core.ErrInvalidMetadata) || receiver != before {
				t.Fatalf("Поле %s remove=%v принято либо изменило получателя: %v", path, remove, err)
			}
		}
	}
}

// TestProfileHDRRejectsForeignFieldsAndUnits отклоняет лишние Version-поля
// и чужие единицы качества: сотые CRF не интерпретируются как дробное значение.
// t сообщает об ошибках проверяемого сценария.
func TestProfileHDRRejectsForeignFieldsAndUnits(t *testing.T) {
	// replacement задаёт неправильную числовую форму либо неизвестное поле HDR в desired Format.
	for _, replacement := range []struct {
		// path выбирает нарушенный контракт; value сохраняет конкретный недопустимый JSON-узел.
		path  string
		value any
	}{
		{"Version", 5}, {"Encoding.Version", 4}, {"Format.Version", 2},
		{"Encoding.CRF", map[string]any{"Set": true, "Value": 23.51}},
		{"Encoding.CRF.ValueHundredths", 2351.1}, {"Encoding.CRF.ValueHundredths", "2351"},
		{"Format.Video.HDR", map[string]any{}}, {"Encoding.SVTAV1.extra", 0},
	} {
		// receiver и before сохраняют корректный объект; input нарушает ровно указанный узел.
		receiver := literalHEVCHDRProfile()
		before := receiver
		input := damagedStoredField(t, replacement.path, false, replacement.value)
		// err сообщает об отказе чтения формы с чужим полем или единицами качества.
		if err := receiver.UnmarshalJSON(input); !errors.Is(err, core.ErrInvalidMetadata) || receiver != before {
			t.Fatalf("Неверная форма %s принята: %s, %v", replacement.path, input, err)
		}
	}
	// raw и err удостоверяют, что отрицательные сценарии не изменили общую буквальную фикстуру.
	raw, err := core.EncodeProfile(literalHEVCHDRProfile())
	if err != nil || !bytes.Equal(raw, []byte(storedHEVCHDRProfile)) {
		t.Fatalf("Фикстура изменилась: %s, %v", raw, err)
	}
}
