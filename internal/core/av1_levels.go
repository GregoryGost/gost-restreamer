package core

// av1HighLimits содержит применимые пределы уровней AV1 из таблицы Annex A.
// Проверка описывает выбранный однослойный рецепт, не полный анализ каждого будущего битстрима.
type av1HighLimits struct {
	// level содержит точное публичное обозначение уровня.
	level string
	// picture, width и height ограничивают число отсчётов яркости и стороны кадра.
	picture, width, height uint64
	// display и decode ограничивают показанные и декодированные отсчёты яркости в секунду.
	display, decode uint64
	// bitrate содержит HighMbps, переведённый в биты в секунду.
	bitrate uint64
	// tiles и columns ограничивают число тайлов в кадре и столбцов.
	tiles, columns uint64
}

// highAV1Level возвращает независимые табличные пределы для явного level с категорией high.
// Значения соответствуют AV1 Version 1.0.0 Errata 1 и таблице FFmpeg n7.1.5 av1_levels.c.
func highAV1Level(level string) (av1HighLimits, bool) {
	// limits содержит очередной известный уровень и его нормативные границы.
	for _, limits := range []av1HighLimits{
		{"4.0", 2359296, 6144, 3456, 70778880, 77856768, 30000000, 32, 8},
		{"4.1", 2359296, 6144, 3456, 141557760, 155713536, 50000000, 32, 8},
		{"5.0", 8912896, 8192, 4352, 267386880, 273715200, 100000000, 64, 8},
		{"5.1", 8912896, 8192, 4352, 534773760, 547430400, 160000000, 64, 8},
		{"5.2", 8912896, 8192, 4352, 1069547520, 1094860800, 240000000, 64, 8},
		{"5.3", 8912896, 8192, 4352, 1069547520, 1176502272, 240000000, 64, 8},
		{"6.0", 35651584, 16384, 8704, 1069547520, 1176502272, 240000000, 128, 16},
		{"6.1", 35651584, 16384, 8704, 2139095040, 2189721600, 480000000, 128, 16},
		{"6.2", 35651584, 16384, 8704, 4278190080, 4379443200, 800000000, 128, 16},
		{"6.3", 35651584, 16384, 8704, 4278190080, 4706009088, 800000000, 128, 16},
	} {
		if limits.level == level {
			return limits, true
		}
	}
	return av1HighLimits{}, false
}

// validateSVTHighTier проверяет явный level, CBR, геометрию и частоту p с единой нативной сеткой тайлов.
// Один декодируемый/показанный кадр и один заголовок на FPS соответствуют рецепту SVT с одним слоем и без дополнительных кадров.
func (p GroupProfile) validateSVTHighTier() error {
	// v и ve задают формат и режим управления битрейтом; limits и ok содержат найденные пределы уровня и факт их наличия.
	v, ve := p.Format.Video, p.VideoEncoding
	limits, ok := highAV1Level(v.Level)
	if !ok || p.Encoding.RateControl != RateControlCBR {
		return invalidEncoding("format.video.tier", "SVT high требует CBR и явный уровень 4.0, 4.1, 5.0..5.3 либо 6.0..6.3.")
	}
	if ve.TargetBitrateBPS > min(uint64(100000000), limits.bitrate) || ve.BufferBits > ve.TargetBitrateBPS {
		return invalidEncoding("video_encoding", "SVT high ограничивает target таблицей уровня и 100 Мбит/с, а точный буфер — одной секундой.")
	}
	// picture содержит число отсчётов яркости в кадре; numerator и denominator сохраняют точный FPS.
	picture := uint64(v.Width) * uint64(v.Height)
	numerator, denominator := uint64(v.FrameRate.Numerator), uint64(v.FrameRate.Denominator)
	// divisor и remainder сокращают исходную дробь перед произведениями с большими пределами decode.
	divisor, remainder := numerator, denominator
	for remainder != 0 {
		divisor, remainder = remainder, divisor%remainder
	}
	numerator, denominator = numerator/divisor, denominator/divisor
	if picture > limits.picture || uint64(v.Width) > limits.width || uint64(v.Height) > limits.height || picture*numerator > limits.display*denominator || picture*numerator > limits.decode*denominator || numerator > 300*denominator {
		return invalidEncoding("format.video", "Геометрия, частота показа, декодирования или заголовков превышают выбранный уровень AV1 high.")
	}
	// err сообщает об отсутствии представимой сетки тайлов того же нативного рецепта.
	_, _, err := AV1TileLayout(p)
	return err
}

// AV1TileLayout выбирает показатели степени числа столбцов и строк для однослойного SVT high в p.
// Сетка соответствует синтаксису равномерной сетки тайлов §5.9.15 AV1; оценка по суперблокам 128×128 консервативна
// и для 64×64. Выбирается наименьшая сумма показателей, затем наименьший показатель столбцов.
// Проверяются ширина/площадь тайла, декодируемые отсчёты на тайл и число тайлов по Annex A.3.
// Функция не вызывает Validate, чтобы основной preflight мог использовать её без рекурсии.
// columnsLog2 и rowsLog2 возвращают показатели степени числа столбцов и строк выбранной сетки.
// err сообщает о неподдержанном запросе либо отсутствии сетки в пределах выбранного уровня.
func AV1TileLayout(p GroupProfile) (columnsLog2, rowsLog2 int, err error) {
	// v содержит геометрию/FPS; limits и ok выбирают применимые пределы явного high-уровня.
	v := p.Format.Video
	limits, ok := highAV1Level(v.Level)
	if p.Encoding.VideoEncoder != VideoEncoderSVTAV1 || p.Encoding.RateControl != RateControlCBR || v.Codec != CodecAV1 || v.Tier != "high" || !ok || v.Width < 64 || v.Width > 16384 || v.Height < 64 || v.Height > 8704 || v.Width%2 != 0 || v.Height%2 != 0 || v.FrameRate.Validate() != nil || uint64(v.FrameRate.Numerator) > 240*uint64(v.FrameRate.Denominator) {
		return 0, 0, invalidEncoding("format.video", "Сетка тайлов требует поддержанный однослойный SVT high CBR с корректными размерами и FPS.")
	}
	// sbColumns и sbRows округляют кадр к худшему поддержанному суперблоку 128×128.
	sbColumns, sbRows := (uint64(v.Width)+127)/128, (uint64(v.Height)+127)/128
	// total перебирает сумму степеней, colLog — показатель столбцов, rowLog — показатель строк.
	for total := 0; total <= 12; total++ {
		for colLog := 0; colLog <= 6; colLog++ {
			rowLog := total - colLog
			if rowLog < 0 || rowLog > 6 || colLog > 0 && (uint64(1)<<(colLog-1)) >= sbColumns || rowLog > 0 && (uint64(1)<<(rowLog-1)) >= sbRows {
				continue
			}
			// tileColumns и tileRows задают запрошенные степени сетки; widthSB и heightSB — округлённый размер её самого большого тайла.
			tileColumns, tileRows := uint64(1)<<colLog, uint64(1)<<rowLog
			widthSB, heightSB := (sbColumns+tileColumns-1)/tileColumns, (sbRows+tileRows-1)/tileRows
			// width и height содержат консервативные размеры тайла, area — число отсчётов яркости.
			width, height := widthSB*128, heightSB*128
			area := width * height
			// Запрошенные степени ограничивают число тайлов сверху и при выборе суперблоков 64×64.
			if width > 4096 || area > 4096*2304 || area*uint64(v.FrameRate.Numerator) > 588251136*uint64(v.FrameRate.Denominator) || tileColumns > limits.columns || tileColumns*tileRows > limits.tiles || tileColumns*tileRows*uint64(v.FrameRate.Numerator) > limits.tiles*120*uint64(v.FrameRate.Denominator) {
				continue
			}
			// validCrop проверяет обрезанные крайние тайлы для обоих нативных размеров суперблоков.
			validCrop := true
			// blockSize выбирает реальный размер суперблока, не меняя выбранные показатели сетки.
			for _, blockSize := range []uint64{64, 128} {
				// blockColumns и blockRows округляют исходные стороны к выбранным суперблокам.
				blockColumns, blockRows := (uint64(v.Width)+blockSize-1)/blockSize, (uint64(v.Height)+blockSize-1)/blockSize
				// tileWidth и tileHeight задают тайл равномерной сетки; columns и rows считают непустые тайлы.
				tileWidth, tileHeight := (blockColumns+tileColumns-1)/tileColumns*blockSize, (blockRows+tileRows-1)/tileRows*blockSize
				columns, rows := (uint64(v.Width)+tileWidth-1)/tileWidth, (uint64(v.Height)+tileHeight-1)/tileHeight
				if uint64(v.Width)-(columns-1)*tileWidth < 8 || uint64(v.Height)-(rows-1)*tileHeight < 8 {
					validCrop = false
					break
				}
			}
			if !validCrop {
				continue
			}
			return colLog, rowLog, nil
		}
	}
	return 0, 0, invalidEncoding("format.video", "Не найдена сетка тайлов в пределах выбранного уровня AV1 high.")
}
