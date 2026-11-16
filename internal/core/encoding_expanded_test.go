package core_test

import (
	"bytes"
	"encoding/json"
	"errors"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// expandedProfile создаёт полный явный рецепт выбранного encoder/rc, сохраняя независимое LIVE copy.
// Качество материализуется отдельно от битрейтов; профиль остаётся пригодным для изменений в тестах.
func expandedProfile(encoder, rc string) core.GroupProfile {
	// p содержит самостоятельный формат до явного выбора кодировщика.
	p := validProfile()
	p.LiveAudioMode = core.LiveAudioCopy
	if encoder == core.VideoEncoderX264 || encoder == core.VideoEncoderNVENC {
		p.Mode, p.Format.Video.Codec, p.Format.Video.CodecProfile = core.ModeTranscode, core.CodecH264, "high"
	}
	p.Encoding = core.DefaultEncoding(p.Mode, encoder, rc)
	switch rc {
	case core.RateControlCQP, core.RateControlCRF:
		p.VideoEncoding.TargetBitrateBPS, p.VideoEncoding.MaxBitrateBPS, p.VideoEncoding.BufferBits = 0, 0, 0
		if rc == core.RateControlCQP {
			p.Encoding.QP = core.OptionalQP{Set: true, Value: 35}
		} else {
			p.Encoding.CRF = core.OptionalCRF{Set: true, ValueHundredths: 3500}
		}
	case core.RateControlCBR:
		p.VideoEncoding.MaxBitrateBPS = p.VideoEncoding.TargetBitrateBPS
	case core.RateControlVBR:
		if encoder == core.VideoEncoderSVTAV1 || encoder == core.VideoEncoderAOMAV1 {
			p.VideoEncoding.MaxBitrateBPS, p.VideoEncoding.BufferBits = 0, 0
		}
	}
	return p
}

// TestExpandedEncoderPixelBindings проверяет все поддержанные точные пары профиля, пикселей и глубины.
// Каждый допустимый рецепт проходит настоящий профильный кодек и сохраняет все значения.
func TestExpandedEncoderPixelBindings(t *testing.T) {
	// binding задаёт одну строку конечной матрицы выбранного кодировщика.
	for _, binding := range []struct {
		// encoder, profile и pixel выбирают точный рецепт; depth указывает число бит компонента.
		encoder, profile, pixel string
		depth                   uint8
	}{
		{core.VideoEncoderSVTAV1, "main", "yuv420p", 8}, {core.VideoEncoderSVTAV1, "main", "yuv420p10le", 10},
		{core.VideoEncoderAOMAV1, "main", "yuv420p", 8}, {core.VideoEncoderAOMAV1, "main", "yuv420p10le", 10}, {core.VideoEncoderAOMAV1, "main", "gray", 8},
		{core.VideoEncoderAOMAV1, "high", "yuv444p", 8}, {core.VideoEncoderAOMAV1, "high", "yuv444p10le", 10},
		{core.VideoEncoderAOMAV1, "professional", "yuv422p", 8}, {core.VideoEncoderAOMAV1, "professional", "yuv422p10le", 10},
		{core.VideoEncoderX264, "constrained-baseline", "yuv420p", 8}, {core.VideoEncoderX264, "main", "yuv420p", 8}, {core.VideoEncoderX264, "high", "yuv420p", 8},
		{core.VideoEncoderX264, "high10", "yuv420p10le", 10}, {core.VideoEncoderX264, "high422", "yuv422p", 8}, {core.VideoEncoderX264, "high422", "yuv422p10le", 10},
		{core.VideoEncoderX264, "high444", "yuv444p", 8}, {core.VideoEncoderX264, "high444", "yuv444p10le", 10},
		{core.VideoEncoderNVENC, "baseline", "yuv420p", 8}, {core.VideoEncoderNVENC, "main", "yuv420p", 8}, {core.VideoEncoderNVENC, "high", "yuv420p", 8}, {core.VideoEncoderNVENC, "high444", "yuv444p", 8},
	} {
		// p содержит допустимую строку матрицы; raw и err — сохраняемые байты и отказ проверки.
		p := expandedProfile(binding.encoder, core.RateControlVBR)
		p.Format.Video.CodecProfile, p.Format.Video.PixelFormat, p.Format.Video.BitDepth = binding.profile, binding.pixel, binding.depth
		raw, err := core.EncodeProfile(p)
		if err != nil {
			t.Fatalf("%+v: %v", binding, err)
		}
		// decoded восстанавливает все настройки без назначения значений по умолчанию.
		decoded, err := core.DecodeProfile(raw)
		if err != nil || decoded != p {
			t.Fatalf("Потеряна строка матрицы %+v: %v", binding, err)
		}
		if core.CompatibleProfileFormat(p.Format, p) != nil {
			t.Fatalf("Точный фактический формат несовместим с %+v", binding)
		}
		p.Format.Video.BitDepth++
		if p.Validate() == nil {
			t.Fatalf("Несоответствие глубины принято: %+v", binding)
		}
	}
}

// TestExpandedQualityRanges проверяет шаг качества и границы выбранных кодировщиков.
// Качество каждого кодировщика проверяется по собственному диапазону и шагу.
// t сообщает об ошибках проверяемого сценария.
func TestExpandedQualityRanges(t *testing.T) {
	// p проверяет дробный x264 CRF, который домен хранит точными целыми сотыми.
	p := expandedProfile(core.VideoEncoderX264, core.RateControlCRF)
	p.Encoding.CRF.ValueHundredths = 2345
	if err := p.Validate(); err != nil { // err сообщает об отказе допустимого дробного качества.
		t.Fatal(err)
	}
	// raw и err содержат новую сохраняемую форму, не использующую старое CRF.Value.
	raw, err := core.EncodeProfile(p)
	if err != nil || !bytes.Contains(raw, []byte(`"CRF":{"Set":true,"ValueHundredths":2345}`)) {
		t.Fatalf("Неверная форма точного качества: %s %v", raw, err)
	}
	// value перебирает нижнюю/верхнюю границы и ближайшие неподдержанные значения качества.
	for _, value := range []int{-1, 0, 1, 5099, 5100, 5101} {
		p.Encoding.CRF.ValueHundredths = value
		if (p.Validate() == nil) != (value > 0 && value <= 5100) {
			t.Fatalf("Неожиданная граница x264 CRF: %d", value)
		}
	}
	p.Format.Video.CodecProfile, p.Format.Video.PixelFormat, p.Format.Video.BitDepth = "high444", "yuv444p10le", 10
	p.Encoding.CRF.ValueHundredths = 0
	if err = p.Validate(); err != nil {
		t.Fatal("Явный x264 high444 CRF0 отклонён", err)
	}
	// encoder выбирает нативную целую шкалу CRF AV1, в которую добавляется ненулевая сотая.
	for _, encoder := range []string{core.VideoEncoderSVTAV1, core.VideoEncoderAOMAV1} {
		p = expandedProfile(encoder, core.RateControlCRF)
		p.Encoding.CRF.ValueHundredths = 3501
		if p.Validate() == nil {
			t.Fatal("AV1 принял дробный нативный CRF", encoder)
		}
	}

}

// TestExpandedProfileStrictPresence проверяет полноту текущего профиля и атомарность отказа читателя.
// t сообщает об ошибках проверяемого сценария.
func TestExpandedProfileStrictPresence(t *testing.T) {
	// p задаёт новый точный рецепт; raw и err содержат исходную полную форму и отказ записи.
	p := expandedProfile(core.VideoEncoderX264, core.RateControlCRF)
	p.Encoding.CRF.ValueHundredths = 2345
	raw, err := core.EncodeProfile(p)
	if err != nil {
		t.Fatal(err)
	}
	// replacement сохраняет несовместимую либо неполную форму CRF.
	for _, replacement := range []string{`null`, `{}`, `{"Set":true}`, `{"ValueHundredths":2345}`, `{"Set":null,"ValueHundredths":2345}`, `{"Set":true,"ValueHundredths":null}`, `{"Set":true,"Value":23.45}`, `{"Set":false,"ValueHundredths":2345}`, `{"Set":true,"ValueHundredths":2345.001}`, `{"Set":true,"ValueHundredths":9223372036854775808}`} {
		// damaged изменяет только полный объект качества; receiver и before контролируют атомарность.
		damaged := bytes.Replace(raw, []byte(`{"Set":true,"ValueHundredths":2345}`), []byte(replacement), 1)
		receiver := qualityProfile(core.RateControlCRF)
		before := receiver
		if err = receiver.UnmarshalJSON(damaged); !errors.Is(err, core.ErrInvalidMetadata) || receiver != before {
			t.Fatalf("Неверный профиль принят: %s, %v", replacement, err)
		}
	}
	// root хранит все материализованные поля, чтобы проверять удаление каждого из них.
	var root map[string]json.RawMessage
	if err = json.Unmarshal(raw, &root); err != nil {
		t.Fatal(err)
	}
	// field выбирает каждое обязательное поле корня, включая явно пустые значения.
	for field := range root {
		// missing является независимой копией JSON перед удалением одного поля.
		var missing map[string]json.RawMessage
		if err = json.Unmarshal(raw, &missing); err != nil {
			t.Fatal(err)
		}
		delete(missing, field)
		// damaged содержит неполную форму нового профиля.
		// err сообщает об отказе сериализации профиля с удалённым обязательным полем.
		damaged, err := json.Marshal(missing)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = core.DecodeProfile(damaged); err == nil {
			t.Fatal("Принят пропуск обязательного поля", field)
		}
	}
}

// TestExpandedCompatibilitySeparatesDesiredAndActual проверяет структурный actual-каталог отдельно от настроек.
// t сообщает об ошибках проверяемого сценария.
func TestExpandedCompatibilitySeparatesDesiredAndActual(t *testing.T) {
	// p выбирает 10-битный AV1, а actual сохраняет независимую измеренную копию.
	p := expandedProfile(core.VideoEncoderSVTAV1, core.RateControlVBR)
	p.Format.Video.PixelFormat, p.Format.Video.BitDepth = "yuv420p10le", 10
	p.Format.Video.Color = core.ColorInfo{Range: "limited", Primaries: "film", Transfer: "gamma22", Matrix: "fcc"}
	actual := p.Format
	actual.Video.FrameRate = core.FrameRate{Numerator: 120, Denominator: 2}
	actual.Video.Color.Range, actual.Video.Color.Transfer = "tv", "bt470m"
	if core.CompatibleProfileFormat(actual, p) != nil || core.CompatibleActualFormat(actual, p.Format) != nil {
		t.Fatal("Полный профиль и фактическое описание не совпали по разрешённым псевдонимам")
	}
	actual.Video.CodecProfile = "observed-profile"
	if core.CompatibleActualFormat(actual, actual) != nil || core.CompatibleProfileFormat(actual, p) == nil {
		t.Fatal("Структурный actual использует каталог кодировщика либо профиль подменён семейством")
	}
	actual = p.Format
	actual.Audio.ChannelLayout = "7.1"
	if core.CompatibleProfileFormat(actual, p) == nil {
		t.Fatal("Совместимость не проверила обязательную AAC-дорожку")
	}
}
