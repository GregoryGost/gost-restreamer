package core_test

import (
	"errors"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// Создаёт опубликованное тестовое поколение изображения с точным профилем, вариантом и принадлежащими
// ему путями.
func validGeneration(t *testing.T) core.FallbackGeneration {
	t.Helper()
	// profile задаёт корректный полный профиль требуемого варианта.
	profile := validProfile()
	// key содержит вычисленный ключ канонического профиля. b содержит точные канонические байты
	// профиля. err сообщает об ошибке вычисления канонической идентичности профиля.
	key, b, err := core.ProfileIdentity(profile)
	if err != nil {
		t.Fatal(err)
	}
	return core.FallbackGeneration{Record: contractRecord("generation"), AssetID: "asset", Kind: core.AssetImage, OriginalPath: "fallback/generation/original", StagingPath: "fallback/generation/original.stage", SourceHash: "source", SizeBytes: 12, SourceFormat: core.SourceFormat{ImageFormat: "png"}, GeneratorVersion: "generator", Incarnation: "owner", AttemptToken: "token", Attempt: 1, State: core.PreparationPublished, RequiredProfiles: []core.RequiredProfile{{Profile: profile, ProfileKey: key, CanonicalProfile: b, VariantID: "variant", ManagedPath: "fallback/generation/variant.mp4", StagingPath: "fallback/generation/variant.stage"}}}
}

// Проверяет отказ для неполного или несогласованного опубликованного поколения и допустимость QUEUED
// без ещё не полученных медиаданных.
func TestGenerationValidationPreservesSourceAndExactOwnedProfileSet(t *testing.T) {
	// tc содержит очередное нарушение, вносимое в исходно корректное поколение.
	for _, tc := range []struct {
		// name задаёт имя сценария проверки нарушенного ограничения.
		name string
		// edit вносит выбранное нарушение владения или целостности в копию корректных данных.
		edit func(*core.FallbackGeneration)
	}{
		{"asset", func(g *core.FallbackGeneration) {
			// Убирает AssetID поколения заглушки.
			g.AssetID = ""
		}},
		{"kind", func(g *core.FallbackGeneration) {
			// Подставляет неизвестный вид исходного материала.
			g.Kind = "other"
		}},
		{"state", func(g *core.FallbackGeneration) {
			// Подставляет неизвестное состояние подготовки.
			g.State = "other"
		}},
		{"owner", func(g *core.FallbackGeneration) {
			// Убирает Incarnation владельца поколения.
			g.Incarnation = ""
		}},
		{"attempt", func(g *core.FallbackGeneration) {
			// Убирает AttemptToken подготовки.
			g.AttemptToken = ""
		}},
		{"generator", func(g *core.FallbackGeneration) {
			// Убирает версию генератора поколения.
			g.GeneratorVersion = ""
		}},
		{"source_path", func(g *core.FallbackGeneration) {
			// Задаёт путь оригинала с выходом в родительский каталог.
			g.OriginalPath = "../outside"
		}},
		{"foreign_path", func(g *core.FallbackGeneration) {
			// Задаёт путь оригинала внутри чужого поколения.
			g.OriginalPath = "fallback/foreign/original"
		}},
		{"profile", func(g *core.FallbackGeneration) {
			// Подставляет неизвестный режим требуемого профиля.
			g.RequiredProfiles[0].Profile.Mode = "other"
		}},
		{"digest", func(g *core.FallbackGeneration) {
			// Подменяет ключ требуемого профиля без изменения его содержимого.
			g.RequiredProfiles[0].ProfileKey = "other"
		}},
		{"canonical", func(g *core.FallbackGeneration) {
			// Подменяет канонические байты требуемого профиля.
			g.RequiredProfiles[0].CanonicalProfile = []byte("other")
		}},
		{"duplicate_profile", func(g *core.FallbackGeneration) {
			// Дублирует требуемый профиль поколения.

			g.RequiredProfiles = append(g.RequiredProfiles, g.RequiredProfiles[0])
		}},
		{"missing_variant", func(g *core.FallbackGeneration) {
			// Убирает VariantID требуемого профиля.
			g.RequiredProfiles[0].VariantID = ""
		}},
		{"variant_path", func(g *core.FallbackGeneration) {
			// Задаёт путь варианта с выходом в родительский каталог.
			g.RequiredProfiles[0].ManagedPath = "../outside"
		}},
		{"foreign_variant_path", func(g *core.FallbackGeneration) {
			// Задаёт путь варианта внутри чужого поколения.
			g.RequiredProfiles[0].ManagedPath = "fallback/foreign/variant.mp4"
		}},
		{"invalid_source", func(g *core.FallbackGeneration) {
			// Добавляет недопустимый нулевой байт в формат изображения.
			g.SourceFormat.ImageFormat = "\x00"
		}},
		{"source_size", func(g *core.FallbackGeneration) {
			// Обнуляет размер исходника опубликованного поколения.
			g.SizeBytes = 0
		}},
		{"source_hash", func(g *core.FallbackGeneration) {
			// Убирает хеш исходника опубликованного поколения.
			g.SourceHash = ""
		}},
		{"image_type", func(g *core.FallbackGeneration) {
			// Задаёт неподдерживаемый формат изображения gif.
			g.SourceFormat.ImageFormat = "gif"
		}},
		{"issued_attempt", func(g *core.FallbackGeneration) {
			// Отмечает исполнение выданным при нулевой Attempt.
			g.Issued = true
			g.Attempt = 0
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет отклонение выбранного нарушения владения, исходника или точного набора профилей поколения.

			// g содержит копию корректного поколения для изменения проверяемого условия.
			g := validGeneration(t)
			// err сообщает об ошибке проверки допустимости изменённого поколения.
			if err := g.Validate(); err != nil {
				t.Fatal(err)
			}
			tc.edit(&g)
			// err сообщает об ошибке проверки допустимости изменённого поколения.
			if err := g.Validate(); err == nil {
				t.Fatal("неполное поколение принято")
			}
		})
	}
	// g содержит копию корректного поколения для изменения проверяемого условия.
	g := validGeneration(t)
	g.State = core.PreparationQueued
	g.SourceHash = ""
	g.SizeBytes = 0
	g.SourceFormat = core.SourceFormat{}
	g.Attempt = 0
	// err сообщает об ошибке проверки допустимости изменённого поколения.
	if err := g.Validate(); err != nil {
		t.Fatal("резервирование требует ещё не проверенные медиаданные", err)
	}
}

// Проверяет принадлежность пути поколению, согласованность хеша и размера и допустимость пустых данных
// целостности при резервировании.
func TestOwnedFileValidationRejectsForeignNamesAndIncompleteIntegrity(t *testing.T) {
	// base задаёт корректную запись владения файлом с согласованными размером и хешем.
	base := core.OwnedFile{Record: contractRecord("file"), AssetID: "asset", GenerationID: "generation", ManagedPath: "fallback/generation/original", Kind: core.OwnedOriginal, SizeBytes: 5, ContentHash: "hash"}
	// edit вносит выбранное нарушение владения или целостности в копию корректных данных.
	for _, edit := range []func(*core.OwnedFile){
		func(f *core.OwnedFile) {
			// Убирает AssetID принадлежащего файла.
			f.AssetID = ""
		},
		func(f *core.OwnedFile) {
			// Убирает GenerationID принадлежащего файла.
			f.GenerationID = ""
		},
		func(f *core.OwnedFile) {
			// Задаёт путь файла с выходом в родительский каталог.
			f.ManagedPath = "../other"
		},
		func(f *core.OwnedFile) {
			// Задаёт путь файла внутри чужого поколения.
			f.ManagedPath = "fallback/foreign/original"
		},
		func(f *core.OwnedFile) {
			// Подставляет неизвестный вид принадлежащего файла.
			f.Kind = "other"
		},
		func(f *core.OwnedFile) {
			// Меняет вид файла на каталог, сохраняя файловые размер и хеш.
			f.Kind = core.OwnedDirectory
		},
		func(f *core.OwnedFile) {
			// Убирает хеш файла при сохранённом положительном размере.
			f.ContentHash = ""
		},
		func(f *core.OwnedFile) {
			// Обнуляет размер файла при сохранённом хеше.
			f.SizeBytes = 0
		},
	} {
		// f содержит отдельную копию записи владения файлом для текущей проверки.
		f := base
		edit(&f)
		// err сообщает об ошибке проверки принадлежности и целостности записи файла.
		if err := f.Validate(); !errors.Is(err, core.ErrInvalid) {
			t.Fatalf("некорректный принадлежащий файл принят: %v", err)
		}
	}
	// kind задаёт очередной разрешённый вид пути при резервировании.
	for _, kind := range []core.OwnedFileKind{core.OwnedDirectory, core.OwnedStaging, core.OwnedOriginal, core.OwnedVariant} {
		// f содержит отдельную копию записи владения файлом для текущей проверки.
		f := base
		f.Kind = kind
		f.ContentHash = ""
		f.SizeBytes = 0
		// err сообщает об ошибке проверки принадлежности и целостности записи файла.
		if err := f.Validate(); err != nil {
			t.Fatal("резервирование отклонено", err)
		}
	}
}

// Проверяет обязательность положительных ограниченных бюджетов очереди и профилей подготовки.
func TestPreparationLimitsRequireExplicitFinitePositiveBudgets(t *testing.T) {
	// l содержит очередной неполный, неположительный или превышающий предел бюджет подготовки.
	for _, l := range []core.PreparationLimits{{}, {MaxQueued: 1}, {MaxProfiles: 1}, {MaxQueued: 1, MaxProfiles: -1}, {MaxQueued: core.MaxListLimit + 1, MaxProfiles: 1}, {MaxQueued: 1, MaxProfiles: core.MaxListLimit + 1}} {
		// err сообщает об ошибке проверки явных бюджетов подготовки.
		if err := l.Validate(); !errors.Is(err, core.ErrInvalid) {
			t.Fatal("неверный бюджет принят", l, err)
		}
	}
	// err сообщает об ошибке проверки явных бюджетов подготовки.
	if err := (core.PreparationLimits{MaxQueued: 1, MaxProfiles: 1}).Validate(); err != nil {
		t.Fatal(err)
	}
}
