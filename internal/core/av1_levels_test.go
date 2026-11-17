package core_test

import (
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// TestExpandedH264LevelAdmission проверяет заявленные границы Level 6 и точность рационального MBPS.
func TestExpandedH264LevelAdmission(t *testing.T) {
	// p содержит ограниченный x264 VBR с одним reference и B0.
	p := expandedProfile(core.VideoEncoderX264, core.RateControlVBR)
	p.Format.Video.Width, p.Format.Video.Height, p.Format.Video.Level = 8192, 4352, "6.0"
	p.Format.Video.FrameRate = core.FrameRate{Numerator: 30, Denominator: 1}
	p.VideoEncoding.MaxBitrateBPS, p.VideoEncoding.BufferBits = 300000000, 300000000
	if err := p.Validate(); err != nil { // err сообщает об отказе точных границ FS/MBPS/High VCL.
		t.Fatal(err)
	}
	// change вводит одно превышение подтверждённого предела, сохраняя остальные параметры.
	for _, change := range []func(*core.GroupProfile){
		func(p *core.GroupProfile) { /* Добавляет макроблок к предельной площади кадра. */
			p.Format.Video.Width += 2
		},
		func(p *core.GroupProfile) { /* Превышает MBPS без округления дроби. */
			p.Format.Video.FrameRate = core.FrameRate{Numerator: 30001, Denominator: 1000}
		},
		func(p *core.GroupProfile) { /* Превышает предел VCL-битрейта профиля High. */
			p.VideoEncoding.MaxBitrateBPS += 1000
		},
		func(p *core.GroupProfile) { /* Превышает предел CPB профиля High. */
			p.VideoEncoding.BufferBits += 1000
		},
		func(p *core.GroupProfile) { /* Нарушает фиксированный B0 нового Level 6. */
			p.VideoEncoding.MaxBFrames = 1
		},
	} {
		// invalid содержит самостоятельный недопустимый запрос.
		invalid := p
		change(&invalid)
		if invalid.Validate() == nil {
			t.Fatal("Превышение Level 6 принято", invalid)
		}
	}
	p.Format.Video.Width, p.Format.Video.Height = 20000, 64
	if p.Validate() == nil {
		t.Fatal("Проверка площади пропустила слишком длинную сторону кадра")
	}
	// level и fps связывают уровни 6.1/6.2 с точными граничными скоростями макроблоков.
	for level, fps := range map[string]uint32{"6.1": 60, "6.2": 120} {
		p.Format.Video.Width, p.Format.Video.Height, p.Format.Video.Level = 8192, 4352, level
		p.Format.Video.FrameRate = core.FrameRate{Numerator: fps, Denominator: 1}
		if err := p.Validate(); err != nil { // err сохраняет отказ следующего допустимого уровня.
			t.Fatal(level, err)
		}
	}
}

// TestExpandedSVTHighTierAndTiles проверяет явные high-уровни, CBR-буфер и общую нативную сетку тайлов.
// t сообщает об ошибках проверяемого сценария.
func TestExpandedSVTHighTierAndTiles(t *testing.T) {
	// p задаёт точный high4.0 1080p30 CBR с предельными 30 Мбит/с и секундным буфером.
	p := expandedProfile(core.VideoEncoderSVTAV1, core.RateControlCBR)
	p.Format.Video.Width, p.Format.Video.Height, p.Format.Video.Level, p.Format.Video.Tier = 1920, 1080, "4.0", "high"
	p.Format.Video.FrameRate = core.FrameRate{Numerator: 30, Denominator: 1}
	p.VideoEncoding.TargetBitrateBPS, p.VideoEncoding.MaxBitrateBPS, p.VideoEncoding.BufferBits = 30000000, 30000000, 30000000
	if err := p.Validate(); err != nil { // err сохраняет отказ допустимой строки high4.0.
		t.Fatal(err)
	}
	// change создаёт одно нарушение категории, геометрии или RC выбранного уровня.
	// Параметр p каждого обработчика change указывает на отдельную изменяемую копию проверяемого профиля.
	for _, change := range []func(*core.GroupProfile){
		func(p *core.GroupProfile) { /* Убирает обязательный явный уровень high. */
			p.Format.Video.Level = "auto"
		},
		func(p *core.GroupProfile) { /* Выбирает уровень без HighMbps. */
			p.Format.Video.Level = "3.1"
		},
		func(p *core.GroupProfile) { /* Превышает MaxPicSize. */ p.Format.Video.Width = 2560 },
		func(p *core.GroupProfile) { /* Превышает MaxDisplayRate при допустимой площади. */
			p.Format.Video.FrameRate.Numerator = 35
		},
		func(p *core.GroupProfile) { /* Превышает HighMbps, сохраняя точную секунду буфера. */
			p.VideoEncoding.TargetBitrateBPS++
			p.VideoEncoding.MaxBitrateBPS++
			p.VideoEncoding.BufferBits++
		},
		func(p *core.GroupProfile) { /* Увеличивает секундный буфер до точных 1001 мс. */
			p.VideoEncoding.BufferBits += 30000
		},
		func(p *core.GroupProfile) { /* Выбирает неподдержанный high VBR. */
			p.Encoding = core.DefaultEncoding(p.Mode, core.VideoEncoderSVTAV1, core.RateControlVBR)
			p.VideoEncoding.MaxBitrateBPS, p.VideoEncoding.BufferBits = 0, 0
		},
	} {
		// invalid сохраняет отдельную копию запроса для одного отказа.
		invalid := p
		change(&invalid)
		if invalid.Validate() == nil {
			t.Fatal("Недопустимый AV1 high принят", invalid)
		}
	}
	p.Format.Video.Width, p.Format.Video.Height, p.Format.Video.Level = 8192, 4320, "6.2"
	p.Format.Video.FrameRate = core.FrameRate{Numerator: 120, Denominator: 1}
	if err := p.Validate(); err != nil { // err сообщает об отказе высокоскоростного формата 8K.
		t.Fatal(err)
	}
	// columns и rows содержат показатели сетки, err сообщает об отсутствии допустимой сетки тайлов.
	columns, rows, err := core.AV1TileLayout(p)
	if err != nil || columns == 0 || rows == 0 {
		t.Fatalf("8K120 не получил нужную ширину и параллелизм тайлов: %d/%d %v", columns, rows, err)
	}
	// sbColumns, sbRows, width и height независимо восстанавливают худшую площадь тайла из результата.
	sbColumns, sbRows := (uint64(p.Format.Video.Width)+127)/128, (uint64(p.Format.Video.Height)+127)/128
	width, height := ((sbColumns+(1<<columns)-1)>>columns)*128, ((sbRows+(1<<rows)-1)>>rows)*128
	if width > 4096 || width*height > 4096*2304 || width*height*120 > 588251136 {
		t.Fatal("Сетка тайлов нарушает нормативные пределы ширины/площади/параллелизма")
	}
	p.Format.Video.FrameRate = core.FrameRate{Numerator: 120000, Denominator: 1000}
	// equivalentColumns и equivalentRows проверяют детерминированность одинаковых рациональных частот.
	equivalentColumns, equivalentRows, err := core.AV1TileLayout(p)
	if err != nil || equivalentColumns != columns || equivalentRows != rows {
		t.Fatal("Эквивалентная частота изменила нативную сетку", err)
	}
}

// TestExpandedRateControlAndColorRejections проверяет границы нативной представимости и несовместимые SDR tuples.
func TestExpandedRateControlAndColorRejections(t *testing.T) {
	// p содержит допустимый x264 VBR перед нарушением представимости в кбит/с.
	p := expandedProfile(core.VideoEncoderX264, core.RateControlVBR)
	p.VideoEncoding.TargetBitrateBPS++
	if p.Validate() == nil {
		t.Fatal("x264 принял непредставимый целый битрейт")
	}
	p = expandedProfile(core.VideoEncoderAOMAV1, core.RateControlCBR)
	p.VideoEncoding.BufferBits = 1
	if p.Validate() == nil {
		t.Fatal("AOM принял нулевой эффективный буфер в миллисекундах")
	}
	p.VideoEncoding.BufferBits = 120000
	if err := p.Validate(); err != nil { // err сообщает отказ точно представимых 20 мс AOM.
		t.Fatal(err)
	}
	p = expandedProfile(core.VideoEncoderNVENC, core.RateControlCQP)
	p.Format.Video.CodecProfile, p.Format.Video.PixelFormat = "high444", "yuv444p"
	p.Encoding.QP.Value, p.Encoding.NVENC.Tuning = 0, "lossless"
	if err := p.Validate(); err != nil { // err сообщает об отказе узкого lossless-рецепта.
		t.Fatal(err)
	}
	p.Encoding.NVENC.EnableTemporalAQ = true
	if p.Validate() == nil {
		t.Fatal("NVENC lossless принял AQ")
	}
	p = expandedProfile(core.VideoEncoderAOMAV1, core.RateControlCRF)
	p.Format.Video.Color = core.ColorInfo{Range: "full", Primaries: "film", Transfer: "bt2020-12", Matrix: "bt2020c"}
	if err := p.Validate(); err != nil { // err сообщает об отказе совместимых раздельных CICP-каталогов.
		t.Fatal(err)
	}
	p.Format.Video.PixelFormat = "gray"
	if p.Validate() == nil {
		t.Fatal("Gray принят с несовместимой матрицей bt2020c")
	}
	p.Format.Video.PixelFormat, p.Format.Video.Color.Transfer = "yuv420p", "gamma22"
	if p.Validate() == nil {
		t.Fatal("bt2020c принят с несовместимым transfer")
	}
	p.Format.Video.Color.Matrix, p.Format.Video.Color.Transfer = "fcc", "film"
	if p.Validate() == nil {
		t.Fatal("Primaries film ошибочно принято как transfer")
	}
}
