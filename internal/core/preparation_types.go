package core

import (
	"bytes"
	"fmt"
	"strings"
)

// PreparationLimits задаёт неизменяемый общий бюджет очереди и числа профилей.
type PreparationLimits struct {
	// MaxQueued ограничивает число принятых незавершённых заданий подготовки.
	MaxQueued int
	// MaxProfiles ограничивает число полных профилей в одном комплекте подготовки.
	MaxProfiles int
}

// Validate требует явные положительные бюджеты очереди и профилей, не превышающие MaxListLimit.
func (l PreparationLimits) Validate() error {
	if l.MaxQueued < 1 || l.MaxQueued > MaxListLimit || l.MaxProfiles < 1 || l.MaxProfiles > MaxListLimit {
		return fmt.Errorf("%w: бюджеты подготовки", ErrInvalid)
	}
	return nil
}

// PreparationLimitsProvider предоставляет общие неизменяемые бюджеты подготовки хранилища.
type PreparationLimitsProvider interface {
	// PreparationLimits возвращает неизменяемые явные бюджеты очереди подготовки и числа требуемых
	// профилей.
	PreparationLimits() PreparationLimits
}

// PreparationState описывает долговечный этап подготовки поколения заглушки.
type PreparationState string

const (
	// PreparationQueued обозначает принятое задание, ожидающее захвата исполнителем.
	PreparationQueued PreparationState = "QUEUED"
	// PreparationClaimed обозначает захваченное задание до выдачи внешней попытки.
	PreparationClaimed PreparationState = "CLAIMED"
	// PreparationIssued обозначает задание с выданной внешней попыткой подготовки.
	PreparationIssued PreparationState = "ISSUED"
	// PreparationPublished обозначает успешно опубликованное поколение заглушки.
	PreparationPublished PreparationState = "PUBLISHED"
	// PreparationFailed обозначает завершение подготовки с отказом.
	PreparationFailed PreparationState = "FAILED"
	// PreparationInterrupted обозначает прерывание подготовки прежнего владельца.
	PreparationInterrupted PreparationState = "INTERRUPTED"
)

// Active распознаёт состояния QUEUED, CLAIMED и ISSUED, в которых подготовка ещё занимает активный
// слот.
func (s PreparationState) Active() bool {
	return s == PreparationQueued || s == PreparationClaimed || s == PreparationIssued
}

// Terminal распознаёт завершение подготовки публикацией, отказом или прерыванием.
func (s PreparationState) Terminal() bool {
	return s == PreparationPublished || s == PreparationFailed || s == PreparationInterrupted
}

// RequiredProfile фиксирует полный профиль и принадлежащие поколению пути требуемого варианта.
type RequiredProfile struct {
	// Profile содержит полный выходной профиль требуемого варианта.
	Profile GroupProfile
	// ProfileKey содержит ключ канонической идентичности требуемого профиля.
	ProfileKey string
	// CanonicalProfile сохраняет точные канонические байты профиля для проверки совпадения.
	CanonicalProfile []byte
	// VariantID заранее назначает идентификатор создаваемого варианта.
	VariantID ID
	// ManagedPath задаёт окончательный относительный путь варианта внутри поколения.
	ManagedPath string
	// StagingPath задаёт относительный путь промежуточного файла варианта.
	StagingPath string
}

// FallbackGeneration сохраняет оригинал, требуемые варианты и точное владение одной подготовкой
// заглушки.
type FallbackGeneration struct {
	// Record содержит идентификатор, ревизию и временные метки поколения.
	Record
	// AssetID связывает поколение со стабильной заглушкой.
	AssetID ID
	// Kind различает изображение и видео исходного материала.
	Kind AssetKind
	// OriginalPath задаёт относительный окончательный путь оригинала поколения.
	OriginalPath string
	// StagingPath задаёт относительный путь промежуточного оригинала.
	StagingPath string
	// SourceHash содержит хеш проверенного оригинала.
	SourceHash string
	// SizeBytes содержит размер оригинала в байтах.
	SizeBytes uint64
	// SourceFormat описывает проверенный формат исходного материала.
	SourceFormat SourceFormat
	// GeneratorVersion фиксирует версию генератора требуемого комплекта вариантов.
	GeneratorVersion string
	// RequiredProfiles сохраняет полный набор профилей и заранее выделенных путей вариантов.
	RequiredProfiles []RequiredProfile
	// Incarnation идентифицирует открытие хранилища, которому принадлежит подготовка.
	Incarnation string
	// AttemptToken отличает конкретное владение подготовкой внутри поколения.
	AttemptToken string
	// Attempt содержит номер выданной попытки подготовки.
	Attempt uint64
	// State сохраняет текущий долговечный этап подготовки.
	State PreparationState
	// Issued отмечает выдачу внешней попытки, завершение которой требует подтверждения.
	Issued bool
	// CleanupPending отмечает необходимость подтвердить очистку ресурсов выданной подготовки.
	CleanupPending bool
	// RebuildFromGenerationID указывает поколение, из оригинала которого выполняется rebuild.
	RebuildFromGenerationID ID
	// Failure сохраняет причину неуспешного завершения подготовки.
	Failure string
}

// Validate проверяет владельца и состояние подготовки, уникальные полные профили и принадлежность всех
// путей каталогу поколения. PUBLISHED требует непустого источника, а Issued — положительной попытки.
func (g FallbackGeneration) Validate() error {
	// err содержит причину отказа проверки идентификатора заглушки.
	if err := g.AssetID.Validate(); err != nil {
		return err
	}
	if g.Kind != AssetImage && g.Kind != AssetVideo {
		return ErrInvalid
	}
	if !g.State.Active() && !g.State.Terminal() {
		return ErrInvalid
	}
	// v содержит очередное обязательное текстовое значение владельца или генератора.
	for _, v := range []string{g.Incarnation, g.AttemptToken, g.GeneratorVersion} {
		// err содержит причину отказа проверки обязательного текста поколения.
		if err := validText("generation", v); err != nil {
			return err
		}
	}
	// p содержит проверяемый путь оригинала или его промежуточного файла.
	for _, p := range []string{g.OriginalPath, g.StagingPath} {
		// err содержит причину отказа проверки относительного пути оригинала.
		if err := ValidateManagedPath(p); err != nil {
			return err
		}
		if !strings.HasPrefix(p, GenerationDirectory(g.ID)+"/") {
			return ErrInvalid
		}
	}
	// seen отмечает уже встреченные канонические профили для запрета дублей.
	seen := map[string]bool{}
	// p содержит очередной требуемый профиль и пути его варианта.
	for _, p := range g.RequiredProfiles {
		// k содержит ключ профиля, b — его канонические байты, а e — причину отказа вычисления
		// идентичности.
		k, b, e := ProfileIdentity(p.Profile)
		if e != nil {
			return e
		}
		if k != p.ProfileKey || !bytes.Equal(b, p.CanonicalProfile) || seen[string(b)] || p.VariantID == "" {
			return ErrInvalid
		}
		seen[string(b)] = true
		// path содержит очередной окончательный или промежуточный путь варианта.
		for _, path := range []string{p.ManagedPath, p.StagingPath} {
			// err содержит причину отказа проверки пути варианта.
			if err := ValidateManagedPath(path); err != nil {
				return err
			}
			if !strings.HasPrefix(path, GenerationDirectory(g.ID)+"/") {
				return ErrInvalid
			}
		}
	}
	// err содержит причину отказа проверки исходного формата поколения.
	if err := g.SourceFormat.Validate(); err != nil {
		return err
	}
	if g.State == PreparationPublished {
		if g.SizeBytes == 0 || g.SourceHash == "" {
			return ErrInvalid
		}
		if g.Kind == AssetImage && g.SourceFormat.ImageFormat != "png" && g.SourceFormat.ImageFormat != "jpeg" {
			return ErrInvalid
		}
	}
	if g.Issued && g.Attempt == 0 {
		return ErrInvalid
	}
	return nil
}

// GenerationDirectory строит относительный каталог fallback для указанного поколения без проверки ID.
func GenerationDirectory(id ID) string { return "fallback/" + string(id) }

// PreparationFence идентифицирует точное поколение и выданную попытку подготовки для защиты от поздних
// действий.
type PreparationFence struct {
	// GenerationID идентифицирует подготовляемое поколение.
	GenerationID ID
	// AssetID идентифицирует заглушку, которой принадлежит поколение.
	AssetID ID
	// Incarnation идентифицирует открытие хранилища, которому принадлежит попытка.
	Incarnation string
	// AttemptToken различает конкретное владение подготовкой.
	AttemptToken string
	// Attempt содержит номер внешней попытки подготовки.
	Attempt uint64
}

// PreparationFenceFor извлекает точную идентичность поколения, владельца и попытки подготовки.
func PreparationFenceFor(g FallbackGeneration) PreparationFence {
	return PreparationFence{g.ID, g.AssetID, g.Incarnation, g.AttemptToken, g.Attempt}
}

// PreparationRequest задаёт заглушку и условия резервирования нового поколения.
type PreparationRequest struct {
	// AssetID указывает заглушку для новой загрузки или rebuild.
	AssetID ID
	// ExpectedRevision задаёт ожидаемую ревизию заглушки для условного изменения.
	ExpectedRevision uint64
	// Kind задаёт тип нового исходного материала.
	Kind AssetKind
	// GeneratorVersion задаёт версию генератора для нового комплекта.
	GeneratorVersion string
	// Rebuild выбирает подготовку из сохранённого оригинала текущего поколения.
	Rebuild bool
}

// GenerationPublication передаёт проверенные результаты подготовки для атомарной публикации поколения.
type GenerationPublication struct {
	// SourceHash содержит хеш подготовленного оригинала.
	SourceHash string
	// SizeBytes содержит размер оригинала в байтах.
	SizeBytes uint64
	// SourceFormat содержит проверенный формат оригинала.
	SourceFormat SourceFormat
	// Variants содержит подготовленные варианты полного требуемого комплекта.
	Variants []FallbackVariant
}

// OwnedFileKind различает принадлежащие подготовке каталоги, оригиналы, варианты и промежуточные файлы.
type OwnedFileKind string

const (
	// OwnedDirectory обозначает каталог, принадлежащий поколению.
	OwnedDirectory OwnedFileKind = "directory"
	// OwnedOriginal обозначает окончательный файл оригинала.
	OwnedOriginal OwnedFileKind = "original"
	// OwnedVariant обозначает окончательный файл подготовленного варианта.
	OwnedVariant OwnedFileKind = "variant"
	// OwnedStaging обозначает промежуточный файл подготовки.
	OwnedStaging OwnedFileKind = "staging"
)

// OwnedFile сохраняет происхождение пути даже после удаления метаданных поколения.
type OwnedFile struct {
	// Record содержит идентификатор, ревизию и временные метки записи владения путём.
	Record
	// ContentHash содержит хеш завершённого оригинала или варианта; до завершения пуст.
	ContentHash string
	// SizeBytes содержит размер завершённого файла в байтах; задаётся вместе с хешем.
	SizeBytes uint64
	// AssetID идентифицирует заглушку-владельца пути.
	AssetID ID
	// GenerationID идентифицирует поколение-владельца пути.
	GenerationID ID
	// ManagedPath сохраняет относительный принадлежащий поколению путь для последующей очистки.
	ManagedPath string
	// Kind указывает назначение принадлежащего поколению пути.
	Kind OwnedFileKind
}

// Validate проверяет вид файла и путь внутри каталога поколения; хеш и ненулевой размер задаются
// вместе и разрешены только для OwnedOriginal и OwnedVariant.
func (f OwnedFile) Validate() error {
	if (f.ContentHash == "") != (f.SizeBytes == 0) {
		return ErrInvalid
	}
	if f.ContentHash != "" && f.Kind != OwnedOriginal && f.Kind != OwnedVariant {
		return ErrInvalid
	}
	// err содержит причину отказа проверки идентификатора заглушки-владельца.
	if err := f.AssetID.Validate(); err != nil {
		return err
	}
	// err содержит причину отказа проверки идентификатора поколения-владельца.
	if err := f.GenerationID.Validate(); err != nil {
		return err
	}
	// err содержит причину отказа проверки принадлежащего поколению пути.
	if err := ValidateManagedPath(f.ManagedPath); err != nil {
		return err
	}
	if f.ManagedPath != GenerationDirectory(f.GenerationID) && !strings.HasPrefix(f.ManagedPath, GenerationDirectory(f.GenerationID)+"/") {
		return ErrInvalid
	}
	switch f.Kind {
	case OwnedDirectory, OwnedOriginal, OwnedVariant, OwnedStaging:
		return nil
	default:
		return ErrInvalid
	}
}
