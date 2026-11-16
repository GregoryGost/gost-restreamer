package core

// CurrentFallbackGeneratorVersion обозначает действующий формат генератора с беззвучной аудиодорожкой.
// Кодек тишины определяется выбранной аудиопривязкой полного профиля.
const CurrentFallbackGeneratorVersion = "fallback-fmp4-silent-v2"

// ValidateUsable требует пригодности m для нового использования без открытия файла.
// Несовместимый проверенный формат и другой генератор не дают нового допуска к файлу.
func (m ArtifactManifest) ValidateUsable() error {
	// err сохраняет повреждение исторического контракта отдельно от предметной неготовности.
	if err := m.Validate(); err != nil {
		return err
	}
	if m.GeneratorVersion != CurrentFallbackGeneratorVersion || CompatibleProfileFormat(m.VerifiedFormat, m.Profile) != nil {
		return ErrNotReady
	}
	return nil
}

// ValidateUsable проверяет связи снимка s и пригодность его точного файла до нового запуска.
// Текущий профиль или поздний rebuild не подменяют сохранённую привязку.
func (s OutputSnapshot) ValidateUsable() error {
	// err содержит нарушение сохранённого контракта снимка.
	if err := s.Validate(); err != nil {
		return err
	}
	return s.Artifact.ValidateUsable()
}
