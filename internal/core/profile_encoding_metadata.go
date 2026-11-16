package core

// frameRateMetadata сохраняет исходную целую дробь отдельно от сокращения при канонизации.
type frameRateMetadata struct {
	// Numerator содержит исходный числитель частоты кадров.
	Numerator uint32
	// Denominator содержит исходный положительный знаменатель.
	Denominator uint32
}
