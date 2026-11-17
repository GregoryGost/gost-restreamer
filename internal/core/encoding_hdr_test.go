package core_test

import (
	"bytes"
	"errors"
	"math"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// hdrRecipe создаёт самостоятельный явный профиль encoder/rc с известным SDR либо HDR-цветом.
// mode определяет обработку LIVE; новые кодировщики не получают пользовательских блоков.
// hdr включает десятибитный формат с цветом BT.2020/PQ вместо SDR.
func hdrRecipe(encoder, rc string, mode core.GroupMode, hdr bool) core.GroupProfile {
	// p сохраняет проверенную геометрию, AAC и GOP общей фикстуры.
	p := validProfile()
	p.Mode, p.LiveAudioMode = mode, core.LiveAudioCopy
	p.Format.Video.Level, p.Format.Video.Tier = "auto", ""
	switch encoder {
	case core.VideoEncoderX265, core.VideoEncoderHEVCNVENC:
		p.Format.Video.Codec = core.CodecHEVC
	case core.VideoEncoderVP9:
		p.Format.Video.Codec, p.Format.Video.CodecProfile = core.CodecVP9, "profile0"
	case core.VideoEncoderX264:
		p.Format.Video.Codec, p.Format.Video.CodecProfile = core.CodecH264, "high"
	}
	p.Encoding = core.DefaultEncoding(mode, encoder, rc)
	if hdr {
		p.Format.Video.PixelFormat, p.Format.Video.BitDepth = "yuv420p10le", 10
		p.Format.Video.Color = core.ColorInfo{Range: "limited", Primaries: "bt2020", Transfer: "smpte2084", Matrix: "bt2020nc"}
		switch p.Format.Video.Codec {
		case core.CodecHEVC:
			p.Format.Video.CodecProfile = "main10"
		case core.CodecVP9:
			p.Format.Video.CodecProfile = "profile2"
		case core.CodecH264:
			p.Format.Video.CodecProfile = "high10"
		}
	}
	if rc == core.RateControlCRF || rc == core.RateControlCQP {
		p.VideoEncoding.TargetBitrateBPS, p.VideoEncoding.MaxBitrateBPS, p.VideoEncoding.BufferBits = 0, 0, 0
		if rc == core.RateControlCRF {
			p.Encoding.CRF = core.OptionalCRF{Set: true, ValueHundredths: 2300}
		} else {
			p.Encoding.QP = core.OptionalQP{Set: true, Value: 23}
		}
	} else if rc == core.RateControlCBR {
		p.VideoEncoding.MaxBitrateBPS = p.VideoEncoding.TargetBitrateBPS
	} else if encoder == core.VideoEncoderVP9 || encoder == core.VideoEncoderSVTAV1 || encoder == core.VideoEncoderAOMAV1 {
		p.VideoEncoding.MaxBitrateBPS, p.VideoEncoding.BufferBits = 0, 0
	}
	return p
}

// TestHDRRecipesRoundTrip проверяет конечную SDR/PQ/HLG-матрицу новых кодировщиков,
// полную форму текущего профиля без подстановки параметров при чтении.
// t сообщает об ошибках проверяемого сценария.
func TestHDRRecipesRoundTrip(t *testing.T) {
	// encoder перечисляет новые конкретные реализации кодирования видео.
	for _, encoder := range []string{core.VideoEncoderX265, core.VideoEncoderHEVCNVENC, core.VideoEncoderVP9} {
		// mode проверяет оба разрешённых способа обработки LIVE-видео.
		for _, mode := range []core.GroupMode{core.ModeCopy, core.ModeTranscode} {
			// transfer выбирает SDR либо каждую из двух поддержанных передаточных характеристик HDR.
			for _, transfer := range []string{"bt709", "smpte2084", "arib-std-b67"} {
				// p содержит независимую разрешённую строку матрицы.
				p := hdrRecipe(encoder, core.RateControlVBR, mode, transfer != "bt709")
				p.Format.Video.Color.Transfer = transfer
				// raw и err содержат сохраняемый профиль либо причину его отказа.
				raw, err := core.EncodeProfile(p)
				if err != nil || !bytes.HasPrefix(raw, []byte(`{"Mode":`)) {
					t.Fatalf("%s/%s/%s: %s %v", encoder, mode, transfer, raw, err)
				}
				// decoded восстанавливает полный профиль без изменения присутствия и defaults.
				decoded, err := core.DecodeProfile(raw)
				if err != nil || decoded != p || core.CompatibleProfileFormat(p.Format, p) != nil {
					t.Fatalf("Потеряна строка матрицы: %+v %v", p, err)
				}

			}
		}
	}
}

// TestHDRTuplesRetainExistingEncoderLimits проверяет точные HDR-пары кодировщиков.
// Произвольная маркировка цвета не заменяет реальную глубину пикселей.
// t сообщает об ошибках проверяемого сценария.
func TestHDRTuplesRetainExistingEncoderLimits(t *testing.T) {
	// binding задаёт разрешённый кодировщик и точный профиль/пиксели HDR.
	for _, binding := range []struct {
		// encoder выбирает рецепт; profile и pixel задают точное сочетание 10-битного видео.
		encoder, profile, pixel string
	}{
		{core.VideoEncoderSVTAV1, "main", "yuv420p10le"},
		{core.VideoEncoderAOMAV1, "main", "yuv420p10le"},
		{core.VideoEncoderAOMAV1, "high", "yuv444p10le"},
		{core.VideoEncoderAOMAV1, "professional", "yuv422p10le"},
		{core.VideoEncoderX264, "high10", "yuv420p10le"},
		{core.VideoEncoderX264, "high422", "yuv422p10le"},
		{core.VideoEncoderX264, "high444", "yuv444p10le"},
	} {
		// mode сохраняет AV1 copy и H264 transcode, не вводя новых комбинаций режима.
		mode := core.ModeCopy
		if binding.encoder == core.VideoEncoderX264 {
			mode = core.ModeTranscode
		}
		// p содержит допустимый HDR с конкретной парой профиля и пикселей.
		p := hdrRecipe(binding.encoder, core.RateControlCRF, mode, true)
		p.Format.Video.CodecProfile, p.Format.Video.PixelFormat = binding.profile, binding.pixel
		if err := p.Validate(); err != nil { // err сообщает об отказе применимой HDR-пары.
			t.Fatal(binding, err)
		}
		p.Format.Video.BitDepth = 8
		if p.Validate() == nil {
			t.Fatal("HDR без 10-битных пикселей принят", binding)
		}
	}
}

// TestNativeHEVCVP9QualityBounds проверяет независимые нативные шкалы и отсутствие скрытого VBV.
// t сообщает об ошибках проверяемого сценария.
func TestNativeHEVCVP9QualityBounds(t *testing.T) {
	// encoder выбирает новые кодировщики с различными возможностями качества.
	for _, encoder := range []string{core.VideoEncoderX265, core.VideoEncoderHEVCNVENC, core.VideoEncoderVP9} {
		// value содержит сотые CRF, включая границы и дробь, запрещённую VP9.
		for _, value := range []int{-1, 0, 1, 5100, 5101, 6300, 6301} {
			// p содержит чистый CRF; allowed задаёт независимо вычисленную нативную допустимость.
			p := hdrRecipe(encoder, core.RateControlCRF, core.ModeTranscode, true)
			p.Encoding.CRF.ValueHundredths = value
			allowed := encoder == core.VideoEncoderX265 && value >= 0 && value <= 5100 || encoder == core.VideoEncoderVP9 && value >= 0 && value <= 6300 && value%100 == 0
			if (p.Validate() == nil) != allowed {
				t.Fatal("Неверная граница CRF", encoder, value, p.Validate())
			}
		}
		// value содержит крайние целые QP для HEVC и запрещённого VP9 CQP.
		for _, value := range []int{-1, 0, 51, 52} {
			// p содержит единственное поле CQP; allowed отражает конечный диапазон HEVC.
			p := hdrRecipe(encoder, core.RateControlCQP, core.ModeCopy, false)
			p.Encoding.QP.Value = value
			allowed := encoder != core.VideoEncoderVP9 && value >= 0 && value <= 51
			if (p.Validate() == nil) != allowed {
				t.Fatal("Неверная граница QP", encoder, value)
			}
		}
	}
	// p проверяет точное представление в нативных кбит/с и кбит, затем независимые ограничения VP9 VBR и CBR.
	p := hdrRecipe(core.VideoEncoderX265, core.RateControlVBR, core.ModeCopy, false)
	p.VideoEncoding.TargetBitrateBPS++
	if p.Validate() == nil {
		t.Fatal("x265 принял округляемый битрейт")
	}
	p = hdrRecipe(core.VideoEncoderHEVCNVENC, core.RateControlVBR, core.ModeCopy, false)
	p.VideoEncoding.TargetBitrateBPS++
	p.VideoEncoding.MaxBitrateBPS++
	if err := p.Validate(); err != nil { // err сохраняет допустимость битового нативного NVENC значения.
		t.Fatal(err)
	}
	p = hdrRecipe(core.VideoEncoderVP9, core.RateControlVBR, core.ModeCopy, false)
	p.VideoEncoding.MaxBitrateBPS = 1
	if p.Validate() == nil {
		t.Fatal("VP9 принял несуществующий VBR hardmax")
	}
	p = hdrRecipe(core.VideoEncoderVP9, core.RateControlCBR, core.ModeTranscode, false)
	p.VideoEncoding.BufferBits++
	if p.Validate() == nil {
		t.Fatal("VP9 принял дробные нативные миллисекунды")
	}
	p.VideoEncoding.TargetBitrateBPS, p.VideoEncoding.MaxBitrateBPS, p.VideoEncoding.BufferBits = 1000, 1000, math.MaxInt32
	if err := p.Validate(); err != nil { // err проверяет верхнюю битовую границу без переполнения умножения.
		t.Fatal(err)
	}
	p.VideoEncoding.BufferBits++
	if p.Validate() == nil {
		t.Fatal("VP9 принял переполнение буфера")
	}
}

// TestStoredHDRProfileStrictAtomicRead проверяет полноту профиля, независимую версию Format,
// новый CRF в сотых долях и сохранность получателя при каждом отказе.
// t сообщает об ошибках проверяемого сценария.
func TestStoredHDRProfileStrictAtomicRead(t *testing.T) {
	// p содержит x265 CRF с точными сотыми; raw и err фиксируют допустимый эталон.
	p := hdrRecipe(core.VideoEncoderX265, core.RateControlCRF, core.ModeCopy, true)
	p.Encoding.CRF.ValueHundredths = 2351
	raw, err := core.EncodeProfile(p)
	if err != nil {
		t.Fatal(err)
	}
	// replacement заменяет одно обязательное поле, тип либо вложенную версию эталона.
	for _, replacement := range []struct {
		// from задаёт проверенную исходную лексему; to создаёт единственное нарушение хранения.
		from, to string
	}{
		{`"Mode":`, `"Version":6,"Mode":`}, {`"Encoding":{"QP"`, `"Encoding":{"Version":5,"QP"`},
		{`"Version":1,"Video"`, `"Version":2,"Video"`},
		{`"Set":true,"ValueHundredths":2351`, `"Set":true,"Value":23.51`},
		{`"Set":true,"ValueHundredths":2351`, `"Set":true,"ValueHundredths":null`},
		{`"ClosedGOP":true`, `"ClosedGOP":null`}, {`"ClosedGOP":true`, `"Other":true`},
		{`"LookaheadFrames":0,`, ``}, {`"Level":"YXV0bw=="`, `"Level":null`},
		{`"QP":{"Set":false,"Value":0}`, `"QP":null`},
		{`"CRF":{"Set":true,"ValueHundredths":2351}`, `"CRF":{}`},
	} {
		// damaged меняет единственную лексему; receiver и before проверяют атомарность отказа.
		damaged := bytes.Replace(raw, []byte(replacement.from), []byte(replacement.to), 1)
		if bytes.Equal(damaged, raw) {
			t.Fatal("Фикстура не изменилась", replacement)
		}
		receiver := validProfile()
		before := receiver
		if err = receiver.UnmarshalJSON(damaged); !errors.Is(err, core.ErrInvalidMetadata) || receiver != before {
			t.Fatal("Недопустимая форма принята либо изменён получатель", replacement, err)
		}
	}
}
