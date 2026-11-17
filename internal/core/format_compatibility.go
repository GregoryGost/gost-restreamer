package core

// CompatibleProfileFormat сверяет actual с полным проверенным desired.
// Фактический формат не ограничивается каталогом желаемых настроек.
func CompatibleProfileFormat(actual MediaFormat, desired GroupProfile) error {
	// err сохраняет типизированную причину недопустимого полного профиля.
	if err := desired.Validate(); err != nil {
		return err
	}
	return CompatibleActualFormat(actual, desired.Format)
}

// CompatibleActualFormat сверяет структурные actual и declared без каталога настроек кодировщика.
// Равные дроби FPS и псевдонимы диапазона/transfer сравнимы; пустые level/tier в declared
// не накладывают ограничения. Ни профиль, ни глубина, ни схема AAC не подменяются родственными.
func CompatibleActualFormat(actual, declared MediaFormat) error {
	if actual.Validate() != nil || declared.Validate() != nil {
		return ErrInvalidFormat
	}
	// a и d содержат независимые видеодорожки для нормализации представления без изменения входа.
	a, d := actual.Video, declared.Video
	if !a.FrameRate.Equal(d.FrameRate) {
		return ErrInvalidFormat
	}
	a.FrameRate = d.FrameRate
	a.Color, d.Color = comparableActualColor(a.Color), comparableActualColor(d.Color)
	if d.Level == "" || d.Level == "auto" {
		a.Level = d.Level
	}
	if d.Tier == "" {
		a.Tier = ""
	}
	if a != d || actual.Audio != declared.Audio {
		return ErrInvalidFormat
	}
	return nil
}

// comparableActualColor возвращает копию c с нормативными псевдонимами FFprobe.
// bt470m/bt470bg нормализуются только в transfer; таблицы primaries и matrix независимы.
func comparableActualColor(c ColorInfo) ColorInfo {
	switch c.Range {
	case "tv":
		c.Range = "limited"
	case "pc":
		c.Range = "full"
	}
	switch c.Transfer {
	case "bt470m":
		c.Transfer = "gamma22"
	case "bt470bg":
		c.Transfer = "gamma28"
	}
	return c
}
