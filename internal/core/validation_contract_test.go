package core_test

import (
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"math"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// Создаёт тестовую запись ревизии 1 с постоянным временем UTC до микросекунд.
func contractRecord(id core.ID) core.Record {
	// now задаёт постоянное UTC-время тестовой Record с точностью до микросекунды.
	now := time.Date(2026, 1, 2, 3, 4, 5, 123456000, time.UTC)
	return core.Record{ID: id, Revision: 1, CreatedAt: now, UpdatedAt: now}
}

// Создаёт согласованные снимки Session и Output с общими ссылками, точным полным профилем и манифестом
// готового файла.
func contractSnapshots(t *testing.T) (core.SessionSnapshot, core.OutputSnapshot) {
	t.Helper()
	// p задаёт корректный полный профиль обеих дорожек.
	p := validProfile()
	// key содержит канонический ключ профиля тестового manifest. canonical содержит точные
	// канонические байты профиля тестового manifest. err сообщает об ошибке вычисления канонической
	// идентичности профиля.
	key, canonical, err := core.ProfileIdentity(p)
	if err != nil {
		t.Fatal(err)
	}
	// policy задаёт выбранную заглушку и положительные интервалы контроля источника.
	policy := core.FallbackPolicy{AssetID: "asset", Watchdog: core.WatchdogPolicy{VideoTimeout: time.Second, AudioTimeout: time.Second, RecoveryWindow: time.Second, KeyframeTimeout: time.Second}}
	// stream задаёт корректный поток с постоянным ключом и политикой заглушки.
	stream := core.Stream{Record: contractRecord("stream"), Name: "Поток", PublicationKey: "synthetic-key", Fallback: policy}
	// group задаёт группу того же потока со ссылкой на именованный профиль.
	group := core.TranscodeGroup{Record: contractRecord("group"), StreamID: stream.ID, ProfileID: "profile", Name: "Группа"}
	// profile задаёт именованный полный профиль тестовой группы.
	profile := core.TranscodeProfile{Record: contractRecord(group.ProfileID), Name: "Профиль", Profile: p}
	// manifest задаёт точный готовый файл заглушки с согласованным профилем.
	manifest := core.ArtifactManifest{GenerationID: "generation", VerifiedFormat: p.Format, VariantID: "variant", AssetID: policy.AssetID, SourceHash: "synthetic-source-hash", Profile: p, ProfileKey: key, CanonicalProfile: canonical, GeneratorVersion: "generator-1", ManagedPath: "variants/generation.mp4", SizeBytes: 512, ContentHash: "synthetic-content-hash"}
	// output задаёт согласованный самостоятельный снимок выхода.
	output := core.OutputSnapshot{
		Destination: core.Destination{Record: contractRecord("destination"), StreamID: stream.ID, GroupID: group.ID, Name: "Назначение", Platform: "custom", Endpoint: "rtmps://example.invalid/live", StreamKey: "synthetic-output-key", Enabled: true},
		Group:       group, Profile: profile, Fallback: policy, Artifact: manifest,
	}
	return core.SessionSnapshot{Stream: stream, Groups: []core.GroupSnapshot{{Group: group, Profile: profile}}}, output
}

// Проверяет ID, ненулевую ревизию, порядок времён и точную микросекундную представимость записи.
func TestRecordRejectsInvalidIdentityRevisionAndChronology(t *testing.T) {
	// base задаёт исходное корректное доменное значение перед внесением нарушения.
	base := contractRecord("record")
	// revision задаёт допустимую граничную ревизию Record.
	for _, revision := range []uint64{1, math.MaxUint64} {
		// value содержит отдельную копию Record для проверки выбранного условия.
		value := base
		value.Revision = revision
		// err сообщает об ошибке проверки Record.
		if err := value.Validate(); err != nil {
			t.Fatalf("допустимая версия %d: %v", revision, err)
		}
	}
	// tc содержит выбранное изменение или допустимое сочетание полей проверяемой сущности.
	for _, tc := range []struct {
		// name задаёт имя проверяемого нарушения доменного ограничения.
		name string
		// edit вносит выбранное нарушение в отдельную копию корректного доменного значения.
		edit func(*core.Record)
	}{
		{"missing_id", func(v *core.Record) {
			// Убирает обязательный ID записи.
			v.ID = ""
		}},
		{"zero_revision", func(v *core.Record) {
			// Обнуляет обязательную Revision записи.
			v.Revision = 0
		}},
		{"missing_creation", func(v *core.Record) {
			// Убирает время создания записи.
			v.CreatedAt = time.Time{}
		}},
		{"missing_update", func(v *core.Record) {
			// Убирает время изменения записи.
			v.UpdatedAt = time.Time{}
		}},
		{"update_before_creation", func(v *core.Record) {
			// Сдвигает UpdatedAt на микросекунду раньше CreatedAt.
			v.UpdatedAt = v.CreatedAt.Add(-time.Microsecond)
		}},
		{"submicrosecond_creation", func(v *core.Record) {
			// Добавляет наносекунду к CreatedAt, нарушая микросекундную точность хранения.
			v.CreatedAt = v.CreatedAt.Add(time.Nanosecond)
		}},
		{"submicrosecond_update", func(v *core.Record) {
			// Добавляет наносекунду к UpdatedAt, нарушая микросекундную точность хранения.
			v.UpdatedAt = v.UpdatedAt.Add(time.Nanosecond)
		}},
		{"unrepresentable_creation", func(v *core.Record) {
			// Задаёт непредставимое время создания через time.Unix(math.MaxInt64, 0).
			v.CreatedAt = time.Unix(math.MaxInt64, 0)
		}},
		{"unrepresentable_update", func(v *core.Record) {
			// Задаёт непредставимое время изменения через time.Unix(math.MaxInt64, 0).
			v.UpdatedAt = time.Unix(math.MaxInt64, 0)
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет ErrInvalid для выбранного нарушения идентичности, версии или времени Record.

			// value содержит отдельную копию Record для проверки выбранного условия.
			value := base
			tc.edit(&value)
			// err сообщает об ошибке проверки Record.
			if err := value.Validate(); !errors.Is(err, core.ErrInvalid) {
				t.Fatalf("некорректная запись принята: %+v, %v", value, err)
			}
		})
	}
}

// Проверяет отказ начального снимка с некорректным Stream, чужими ссылками, повтором группы или
// недопустимым профилем.
func TestSessionSnapshotRejectsForeignAndDuplicateGroups(t *testing.T) {
	// tc содержит выбранное изменение или допустимое сочетание полей проверяемой сущности.
	for _, tc := range []struct {
		// name задаёт имя проверяемого нарушения доменного ограничения.
		name string
		// edit вносит выбранное нарушение в отдельную копию корректного доменного значения.
		edit func(*core.SessionSnapshot)
	}{
		{"invalid_stream", func(v *core.SessionSnapshot) {
			// Убирает имя Stream в начальном снимке.
			v.Stream.Name = ""
		}},
		{"missing_stream_id", func(v *core.SessionSnapshot) {
			// Убирает ID Stream в начальном снимке.
			v.Stream.ID = ""
		}},
		{"foreign_stream", func(v *core.SessionSnapshot) {
			// Привязывает группу начального снимка к другому Stream.
			v.Groups[0].Group.StreamID = "other"
		}},
		{"foreign_profile", func(v *core.SessionSnapshot) {
			// Привязывает группу начального снимка к другому профилю.
			v.Groups[0].Group.ProfileID = "other"
		}},
		{"duplicate_group", func(v *core.SessionSnapshot) {
			// Дублирует группу в начальном снимке Session.
			v.Groups = append(v.Groups, v.Groups[0])
		}},
		{"invalid_group", func(v *core.SessionSnapshot) {
			// Убирает имя группы начального снимка.
			v.Groups[0].Group.Name = ""
		}},
		{"invalid_profile", func(v *core.SessionSnapshot) {
			// Убирает имя профиля начального снимка.
			v.Groups[0].Profile.Name = ""
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет ErrInvalid для выбранного нарушения согласованности начального снимка Session.

			// value содержит отдельную копию начального снимка Session для проверки
			// выбранного условия.
			value, _ := contractSnapshots(t)
			// err сообщает об ошибке проверки начального снимка Session.
			if err := value.Validate(); err != nil {
				t.Fatal(err)
			}
			tc.edit(&value)
			// err сообщает об ошибке проверки начального снимка Session.
			if err := value.Validate(); !errors.Is(err, core.ErrInvalid) {
				t.Fatalf("несогласованный начальный снимок принят: %v", err)
			}
		})
	}
}

// Проверяет согласованность ссылок и полного профиля всех частей снимка выхода.
func TestOutputSnapshotRequiresOneConsistentFallbackProfile(t *testing.T) {
	// tc содержит выбранное изменение или допустимое сочетание полей проверяемой сущности.
	for _, tc := range []struct {
		// name задаёт имя проверяемого нарушения доменного ограничения.
		name string
		// edit вносит выбранное нарушение в отдельную копию корректного доменного значения.
		edit func(*core.OutputSnapshot)
	}{
		{"invalid_destination", func(v *core.OutputSnapshot) {
			// Убирает имя назначения снимка выхода.
			v.Destination.Name = ""
		}},
		{"invalid_group", func(v *core.OutputSnapshot) {
			// Убирает имя группы снимка выхода.
			v.Group.Name = ""
		}},
		{"invalid_profile", func(v *core.OutputSnapshot) {
			// Убирает имя профиля снимка выхода.
			v.Profile.Name = ""
		}},
		{"invalid_fallback", func(v *core.OutputSnapshot) {
			// Убирает AssetID политики заглушки снимка выхода.
			v.Fallback.AssetID = ""
		}},
		{"invalid_manifest", func(v *core.OutputSnapshot) {
			// Обнуляет размер файла манифеста снимка выхода.
			v.Artifact.SizeBytes = 0
		}},
		{"foreign_group", func(v *core.OutputSnapshot) {
			// Подменяет GroupID назначения на постороннюю группу.
			v.Destination.GroupID = "other"
		}},
		{"foreign_stream", func(v *core.OutputSnapshot) {
			// Подменяет StreamID назначения на посторонний поток.
			v.Destination.StreamID = "other"
		}},
		{"foreign_profile", func(v *core.OutputSnapshot) {
			// Подменяет ProfileID группы на посторонний профиль.
			v.Group.ProfileID = "other"
		}},
		{"foreign_asset", func(v *core.OutputSnapshot) {
			// Подменяет AssetID политики заглушки на посторонний объект.
			v.Fallback.AssetID = "other"
		}},
		{"different_full_profile", func(v *core.OutputSnapshot) {
			// Меняет ширину профиля снимка без изменения профиля манифеста.
			v.Profile.Profile.Format.Video.Width++
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет ErrInvalid для выбранного нарушения связей назначения, группы, профиля, политики и
			// манифеста выхода.

			// value содержит отдельную копию самостоятельного снимка Output для проверки
			// выбранного условия.
			_, value := contractSnapshots(t)
			// err сообщает об ошибке проверки самостоятельного снимка Output.
			if err := value.Validate(); err != nil {
				t.Fatal(err)
			}
			tc.edit(&value)
			// err сообщает об ошибке проверки самостоятельного снимка Output.
			if err := value.Validate(); !errors.Is(err, core.ErrInvalid) {
				t.Fatalf("несогласованный снимок выхода принят: %v", err)
			}
		})
	}
}

// Проверяет обязательные сведения готового файла и точное совпадение ключа и канонических байтов
// полного профиля.
func TestArtifactManifestRequiresExactIdentityAndOwnedFile(t *testing.T) {
	// tc содержит выбранное изменение или допустимое сочетание полей проверяемой сущности.
	for _, tc := range []struct {
		// name задаёт имя проверяемого нарушения доменного ограничения.
		name string
		// edit вносит выбранное нарушение в отдельную копию корректного доменного значения.
		edit func(*core.ArtifactManifest)
	}{
		{"variant", func(v *core.ArtifactManifest) {
			// Убирает VariantID манифеста.
			v.VariantID = ""
		}},
		{"asset", func(v *core.ArtifactManifest) {
			// Убирает AssetID манифеста.
			v.AssetID = ""
		}},
		{"profile", func(v *core.ArtifactManifest) {
			// Подставляет неизвестный режим профиля манифеста.
			v.Profile.Mode = "unknown"
		}},
		{"profile_digest", func(v *core.ArtifactManifest) {
			// Подменяет ProfileKey манифеста значением other.
			v.ProfileKey = "other"
		}},
		{"canonical_content", func(v *core.ArtifactManifest) {
			// Подменяет канонические байты профиля манифеста.
			v.CanonicalProfile = []byte("other")
		}},
		{"size", func(v *core.ArtifactManifest) {
			// Обнуляет размер файла манифеста.
			v.SizeBytes = 0
		}},
		{"source_hash", func(v *core.ArtifactManifest) {
			// Убирает хеш исходника манифеста.
			v.SourceHash = ""
		}},
		{"content_hash", func(v *core.ArtifactManifest) {
			// Убирает хеш содержимого файла манифеста.
			v.ContentHash = ""
		}},
		{"generator", func(v *core.ArtifactManifest) {
			// Убирает версию генератора манифеста.
			v.GeneratorVersion = ""
		}},
		{"managed_path", func(v *core.ArtifactManifest) {
			// Задаёт путь манифеста с выходом в родительский каталог.
			v.ManagedPath = "../generation.mp4"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет отклонение выбранного нарушения манифеста, различая ErrInvalidProfile для профиля и
			// ErrInvalid для остальных полей.

			// snapshot предоставляет согласованный снимок для построения проверяемого
			// значения.
			_, snapshot := contractSnapshots(t)
			// value содержит отдельную копию manifest готового файла для проверки выбранного
			// условия.
			value := snapshot.Artifact
			// err сообщает об ошибке проверки manifest готового файла.
			if err := value.Validate(); err != nil {
				t.Fatal(err)
			}
			tc.edit(&value)
			// category задаёт ожидаемую категорию отказа выбранного поля manifest.
			category := core.ErrInvalid
			if tc.name == "profile" {
				category = core.ErrInvalidProfile
			}
			// err сообщает об ошибке проверки manifest готового файла.
			if err := value.Validate(); !errors.Is(err, category) {
				t.Fatalf("неполный manifest принят: %v", err)
			}
		})
	}
}

// Создаёт PENDING-квитанцию запуска сессии с согласованными точными байтами Identity и SHA-256.
func contractReceipt() core.CommandReceipt {
	// identity задаёт точные канонические байты проверяемой команды.
	identity := []byte(`{"Version":1,"Kind":"start_session","TargetID":"stream","DestinationIDs":null}`)
	// digest содержит SHA-256 точных байтов команды для согласованного Fingerprint.
	digest := sha256.Sum256(identity)
	return core.CommandReceipt{Record: contractRecord("command"), RequestID: "request", Kind: core.CommandStartSession, TargetID: "stream", Identity: identity, Fingerprint: hex.EncodeToString(digest[:]), SessionID: "session", Incarnation: "owner", Lifecycle: 1, Status: core.CommandPending}
}

// Проверяет допустимые виды и состояния команд, полноту владельца и целостность идентичности, включая
// полный диапазон поколения выхода.
func TestCommandReceiptRequiresIdentityAndCurrentOwnership(t *testing.T) {
	// tc содержит выбранное изменение или допустимое сочетание полей проверяемой сущности.
	for _, tc := range []struct {
		// kind задаёт допустимый вид проверяемой команды.
		kind core.CommandKind
		// target задаёт соответствующую виду команды цель.
		target core.ID
		// output задаёт выход для команд выхода либо пустое значение для команд сессии.
		output core.ID
		// identity задаёт точные канонические байты проверяемой команды.
		identity string
	}{
		{core.CommandStartSession, "stream", "", `{"Version":1,"Kind":"start_session","TargetID":"stream","DestinationIDs":null}`},
		{core.CommandStopSession, "session", "", `{"Version":1,"Kind":"stop_session","TargetID":"session","DestinationIDs":null}`},
		{core.CommandPauseSession, "session", "", `{"Version":1,"Kind":"pause_session","TargetID":"session","DestinationIDs":null}`},
		{core.CommandResumeSession, "session", "", `{"Version":1,"Kind":"resume_session","TargetID":"session","DestinationIDs":null}`},
		{core.CommandAddOutput, "session", "output", `{"Version":1,"Kind":"add_output","TargetID":"session","DestinationIDs":["destination"]}`},
		{core.CommandStartOutput, "output", "output", `{"Version":1,"Kind":"start_output","TargetID":"output","DestinationIDs":null}`},
		{core.CommandStopOutput, "output", "output", `{"Version":1,"Kind":"stop_output","TargetID":"output","DestinationIDs":null}`},
		{core.CommandDetachOutput, "output", "output", `{"Version":1,"Kind":"detach_output","TargetID":"output","DestinationIDs":null}`},
	} {
		t.Run(string(tc.kind), func(t *testing.T) {
			// Проверяет согласованность выбранного вида команды с целью, выходом, поколением и канонической
			// идентичностью запроса.

			// value содержит отдельную копию квитанции команды для проверки выбранного
			// условия.
			value := contractReceipt()
			value.Kind, value.TargetID, value.OutputID = tc.kind, tc.target, tc.output
			if tc.output != "" {
				value.OutputGeneration = 1
			}
			value.Identity = []byte(tc.identity)
			// digest содержит SHA-256 точных байтов команды для согласованного Fingerprint.
			digest := sha256.Sum256(value.Identity)
			value.Fingerprint = hex.EncodeToString(digest[:])
			// err сообщает об ошибке проверки квитанции команды.
			if err := value.Validate(); err != nil {
				t.Fatalf("вид команды %s отклонён: %v", tc.kind, err)
			}
		})
	}
	// tc содержит выбранное изменение или допустимое сочетание полей проверяемой сущности.
	for _, tc := range []struct {
		// status задаёт допустимый статус квитанции.
		status core.CommandStatus
		// attempt задаёт номер попытки, соответствующий проверяемому статусу.
		attempt uint64
		// failure задаёт итоговое объяснение отказа или прерывания команды.
		failure string
	}{
		{core.CommandPending, 0, ""},
		{core.CommandClaimed, 1, ""},
		{core.CommandIssued, 1, ""},
		{core.CommandSucceeded, 1, ""},
		{core.CommandFailed, 1, "Не удалось запустить сессию"},
		{core.CommandInterrupted, 1, "Потерян владелец исполнения"},
	} {
		t.Run(string(tc.status), func(t *testing.T) {
			// Проверяет допустимость выбранного статуса квитанции вместе с соответствующими Attempt и Failure.

			// value содержит отдельную копию квитанции команды для проверки выбранного
			// условия.
			value := contractReceipt()
			value.Status, value.Attempt, value.Failure = tc.status, tc.attempt, tc.failure
			// err сообщает об ошибке проверки квитанции команды.
			if err := value.Validate(); err != nil {
				t.Fatalf("состояние команды %s отклонено: %v", tc.status, err)
			}
		})
	}
	// tc содержит выбранное изменение или допустимое сочетание полей проверяемой сущности.
	for _, tc := range []struct {
		// name задаёт имя проверяемого нарушения доменного ограничения.
		name string
		// edit вносит выбранное нарушение в отдельную копию корректного доменного значения.
		edit func(*core.CommandReceipt)
	}{
		{"record", func(v *core.CommandReceipt) {
			// Обнуляет Revision записи квитанции.
			v.Revision = 0
		}},
		{"request", func(v *core.CommandReceipt) {
			// Убирает RequestID квитанции.
			v.RequestID = ""
		}},
		{"target", func(v *core.CommandReceipt) {
			// Убирает TargetID квитанции.
			v.TargetID = ""
		}},
		{"session", func(v *core.CommandReceipt) {
			// Убирает SessionID квитанции.
			v.SessionID = ""
		}},
		{"incarnation", func(v *core.CommandReceipt) {
			// Убирает Incarnation квитанции.
			v.Incarnation = ""
		}},
		{"lifecycle", func(v *core.CommandReceipt) {
			// Обнуляет Lifecycle квитанции.
			v.Lifecycle = 0
		}},
		{"identity", func(v *core.CommandReceipt) {
			// Убирает каноническую Identity квитанции.
			v.Identity = nil
		}},
		{"kind", func(v *core.CommandReceipt) {
			// Подставляет неизвестный вид команды.
			v.Kind = "unknown"
		}},
		{"status", func(v *core.CommandReceipt) {
			// Подставляет неизвестный статус команды.
			v.Status = "unknown"
		}},
		{"fingerprint", func(v *core.CommandReceipt) {
			// Подменяет Fingerprint квитанции без изменения Identity.
			v.Fingerprint = "different"
		}},
		{"changed_request", func(v *core.CommandReceipt) {
			// Подменяет Identity без пересчёта Fingerprint.
			v.Identity = []byte("other request")
		}},
		{"invalid_output", func(v *core.CommandReceipt) {
			// Задаёт недопустимый OutputID с нулевым байтом при положительном поколении.
			v.OutputID = "\x00"
			v.OutputGeneration = 1
		}},
		{"missing_output_generation", func(v *core.CommandReceipt) {
			// Задаёт OutputID без обязательного OutputGeneration.
			v.OutputID = "output"
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет ErrInvalid для выбранного нарушения содержимого или владения квитанцией.

			// value содержит отдельную копию квитанции команды для проверки выбранного
			// условия.
			value := contractReceipt()
			tc.edit(&value)
			// err сообщает об ошибке проверки квитанции команды.
			if err := value.Validate(); !errors.Is(err, core.ErrInvalid) {
				t.Fatalf("неполная квитанция принята: %v", err)
			}
		})
	}
	// value содержит отдельную копию квитанции команды для проверки выбранного условия.
	value := contractReceipt()
	value.Kind, value.TargetID = core.CommandStartOutput, "output"
	value.OutputID, value.OutputGeneration = "output", math.MaxUint64
	value.Identity = []byte(`{"Version":1,"Kind":"start_output","TargetID":"output","DestinationIDs":null}`)
	// digest содержит SHA-256 точных байтов команды для согласованного Fingerprint.
	digest := sha256.Sum256(value.Identity)
	value.Fingerprint = hex.EncodeToString(digest[:])
	// err сообщает об ошибке проверки квитанции команды.
	if err := value.Validate(); err != nil {
		t.Fatalf("квитанция выхода отклонена: %v", err)
	}
}

// Проверяет сохраняемое намерение Session/Output, запрет ошибки у активной Session и запрет запуска
// отсоединённого выхода.
func TestSessionAndOutputRequireValidSavedIntent(t *testing.T) {
	// tc содержит выбранное изменение или допустимое сочетание полей проверяемой сущности.
	for _, tc := range []struct {
		// name задаёт имя проверяемого нарушения доменного ограничения.
		name string
		// edit вносит выбранное нарушение в отдельную копию корректного доменного значения.
		edit func(*core.Session)
	}{
		{"active_failure", func(v *core.Session) {
			// Задаёт Failure у нетерминальной SessionRunning.
			v.Failure = "ошибка"
		}},
		{"record", func(v *core.Session) {
			// Убирает ID Session.
			v.ID = ""
		}},
		{"stream", func(v *core.Session) {
			// Убирает StreamID Session.
			v.StreamID = ""
		}},
		{"owner", func(v *core.Session) {
			// Убирает Incarnation Session.
			v.Incarnation = ""
		}},
		{"state", func(v *core.Session) {
			// Подставляет неизвестное состояние Session.
			v.State = "unknown"
		}},
		{"lifecycle", func(v *core.Session) {
			// Обнуляет Lifecycle Session.
			v.Lifecycle = 0
		}},
		{"snapshot_stream", func(v *core.Session) {
			// Подменяет StreamID Session, сохраняя прежний снимок Stream.
			v.StreamID = "foreign"
		}},
		{"end_time_overflow", func(v *core.Session) {
			// Задаёт непредставимое EndedAt через time.Unix(math.MaxInt64, 0).
			// value задаёт непредставимое время завершения для проверки отказа.
			value := time.Unix(math.MaxInt64, 0)
			v.EndedAt = &value
		}},
		{"snapshot", func(v *core.Session) {
			// Убирает имя Stream в сохранённом снимке Session.
			v.Snapshot.Stream.Name = ""
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет ErrInvalid для выбранного нарушения состояния, владения, времени или снимка Session.

			// snapshot предоставляет согласованный снимок для построения проверяемого
			// значения.
			snapshot, _ := contractSnapshots(t)
			// value содержит отдельную копию сохраняемого управления Session для проверки
			// выбранного условия.
			value := core.Session{Record: contractRecord("session"), StreamID: snapshot.Stream.ID, Incarnation: "owner", State: core.SessionRunning, Lifecycle: 1, Snapshot: snapshot}
			// err сообщает об ошибке проверки сохраняемого управления Session.
			if err := value.Validate(); err != nil {
				t.Fatal(err)
			}
			tc.edit(&value)
			// err сообщает об ошибке проверки сохраняемого управления Session.
			if err := value.Validate(); !errors.Is(err, core.ErrInvalid) {
				t.Fatalf("некорректная Session принята: %v", err)
			}
		})
	}
	// snapshot предоставляет согласованный снимок для построения проверяемого значения. output
	// задаёт согласованный самостоятельный снимок выхода.
	snapshot, output := contractSnapshots(t)
	// ended задаёт время завершения корректной терминальной сессии.
	ended := contractRecord("session").UpdatedAt.Add(time.Second)
	// terminal задаёт завершённую с отказом сессию с допустимыми Failure и EndedAt.
	terminal := core.Session{Record: contractRecord("session"), StreamID: snapshot.Stream.ID, Incarnation: "owner", State: core.SessionFailed, Lifecycle: 1, Snapshot: snapshot, Failure: "итоговый отказ", EndedAt: &ended}
	// err сообщает об ошибке проверки сохраняемого управления Session или Output.
	if err := terminal.Validate(); err != nil {
		t.Fatalf("итоговый отказ отклонён: %v", err)
	}
	// base задаёт исходное корректное доменное значение перед внесением нарушения.
	base := core.SessionOutput{Record: contractRecord("output"), SessionID: "session", Snapshot: output, Attached: true, DesiredRunning: true, Generation: 1}
	// err сообщает об ошибке проверки сохраняемого управления Session или Output.
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	// edit вносит выбранное нарушение в отдельную копию корректного доменного значения.
	for _, edit := range []func(*core.SessionOutput){
		func(v *core.SessionOutput) {
			// Убирает ID сохранённого выхода.
			v.ID = ""
		},
		func(v *core.SessionOutput) {
			// Убирает SessionID сохранённого выхода.
			v.SessionID = ""
		},
		func(v *core.SessionOutput) {
			// Обнуляет поколение сохранённого выхода.
			v.Generation = 0
		},
		func(v *core.SessionOutput) {
			// Отсоединяет выход, сохраняя DesiredRunning, для проверки недопустимого намерения.
			v.Attached = false
		},
		func(v *core.SessionOutput) {
			// Убирает имя назначения в сохранённом снимке выхода.
			v.Snapshot.Destination.Name = ""
		},
	} {
		// value содержит отдельную копию сохраняемого управления Session или Output для проверки
		// выбранного условия.
		value := base
		edit(&value)
		// err сообщает об ошибке проверки сохраняемого управления Session или Output.
		if err := value.Validate(); !errors.Is(err, core.ErrInvalid) {
			t.Fatalf("некорректное намерение выхода принято: %v", err)
		}
	}
	base.Attached, base.DesiredRunning = false, false
	// err сообщает об ошибке проверки сохраняемого управления Session или Output.
	if err := base.Validate(); err != nil {
		t.Fatalf("отсоединённый остановленный выход отклонён: %v", err)
	}
}

// Проверяет точные зависимости намерений файловой очистки и очистки ресурсов исполнения и удержаний
// Session/Output.
func TestCleanupAndReferenceHoldsRequireOwnedDependencies(t *testing.T) {
	// file задаёт корректное намерение очистки принадлежащего поколению пути.
	file := core.CleanupIntent{Record: contractRecord("cleanup"), Kind: core.CleanupFile, ArtifactGenerationID: "generation", FileID: "file", FileKind: core.OwnedVariant, ManagedPath: "fallback/generation/retired.mp4"}
	// runtime задаёт корректное намерение очистки точного исполнения сессии.
	runtime := core.CleanupIntent{Record: contractRecord("runtime-cleanup"), Kind: core.CleanupRuntime, StreamID: "stream", SessionID: "session", Incarnation: "owner", Lifecycle: 1}
	// value содержит отдельную копию очистки или удержания зависимостей для проверки выбранного
	// условия.
	for _, value := range []core.CleanupIntent{file, runtime} {
		// err сообщает об ошибке проверки очистки или удержания зависимостей.
		if err := value.Validate(); err != nil {
			t.Fatalf("допустимое намерение очистки отклонено: %v", err)
		}
	}
	// edit вносит выбранное нарушение в отдельную копию корректного доменного значения.
	for _, edit := range []func(*core.CleanupIntent){
		func(v *core.CleanupIntent) {
			// Убирает ID намерения очистки исполнения.
			v.ID = ""
		},
		func(v *core.CleanupIntent) {
			// Подставляет неизвестный вид очистки.
			v.Kind = "unknown"
		},
		func(v *core.CleanupIntent) {
			// Убирает StreamID намерения очистки исполнения.
			v.StreamID = ""
		},
		func(v *core.CleanupIntent) {
			// Убирает SessionID намерения очистки исполнения.
			v.SessionID = ""
		},
		func(v *core.CleanupIntent) {
			// Убирает Incarnation намерения очистки исполнения.
			v.Incarnation = ""
		},
		func(v *core.CleanupIntent) {
			// Обнуляет Lifecycle намерения очистки исполнения.
			v.Lifecycle = 0
		},
		func(v *core.CleanupIntent) {
			// Задаёт OutputID очистки без обязательного поколения выхода.
			v.OutputID = "output"
		},
	} {
		// value содержит отдельную копию очистки или удержания зависимостей для проверки
		// выбранного условия.
		value := runtime
		edit(&value)
		// err сообщает об ошибке проверки очистки или удержания зависимостей.
		if err := value.Validate(); !errors.Is(err, core.ErrInvalid) {
			t.Fatalf("неполное владение очисткой принято: %v", err)
		}
	}
	file.ManagedPath = "../unowned.mp4"
	// err сообщает об ошибке проверки очистки или удержания зависимостей.
	if err := file.Validate(); !errors.Is(err, core.ErrInvalid) {
		t.Fatalf("очистка чужого файла принята: %v", err)
	}
	runtime.OutputID, runtime.Generation = "output", 1
	// err сообщает об ошибке проверки очистки или удержания зависимостей.
	if err := runtime.Validate(); err != nil {
		t.Fatal(err)
	}
	// hold задаёт полные удерживаемые зависимости сессии либо выхода.
	hold := core.ReferenceHold{GenerationID: "generation", SessionID: "session", StreamID: "stream", AssetID: "asset"}
	// err сообщает об ошибке проверки очистки или удержания зависимостей.
	if err := hold.Validate(); err != nil {
		t.Fatalf("удержание исходника Session отклонено: %v", err)
	}
	hold.OutputID, hold.GroupID, hold.ProfileID, hold.DestinationID, hold.VariantID = "output", "group", "profile", "destination", "variant"
	// err сообщает об ошибке проверки очистки или удержания зависимостей.
	if err := hold.Validate(); err != nil {
		t.Fatal(err)
	}
	// edit вносит выбранное нарушение в отдельную копию корректного доменного значения.
	for _, edit := range []func(*core.ReferenceHold){
		func(v *core.ReferenceHold) {
			// Убирает SessionID удержания зависимостей выхода.
			v.SessionID = ""
		}, func(v *core.ReferenceHold) {
			// Убирает StreamID удержания зависимостей выхода.
			v.StreamID = ""
		},
		func(v *core.ReferenceHold) {
			// Убирает AssetID удержания зависимостей выхода.
			v.AssetID = ""
		}, func(v *core.ReferenceHold) {
			// Задаёт OutputID удержания с недопустимым нулевым байтом.
			v.OutputID = "\x00"
		},
		func(v *core.ReferenceHold) {
			// Убирает GroupID удержания зависимостей выхода.
			v.GroupID = ""
		}, func(v *core.ReferenceHold) {
			// Убирает ProfileID удержания зависимостей выхода.
			v.ProfileID = ""
		},
		func(v *core.ReferenceHold) {
			// Убирает DestinationID удержания зависимостей выхода.
			v.DestinationID = ""
		}, func(v *core.ReferenceHold) {
			// Убирает VariantID удержания зависимостей выхода.
			v.VariantID = ""
		},
	} {
		// value содержит отдельную копию очистки или удержания зависимостей для проверки
		// выбранного условия.
		value := hold
		edit(&value)
		// err сообщает об ошибке проверки очистки или удержания зависимостей.
		if err := value.Validate(); !errors.Is(err, core.ErrInvalid) {
			t.Fatalf("неполное удержание принято: %v", err)
		}
	}
}

// Проверяет обязательную идентичность исполнения и согласованное наличие OutputID и OutputGeneration.
func TestExecutionFenceRequiresExactSessionAndOutputGeneration(t *testing.T) {
	// base задаёт корректную идентичность точного исполнения сессии.
	base := core.ExecutionFence{CommandID: "command", SessionID: "session", Incarnation: "owner", Lifecycle: 1, Attempt: 1}
	// err сообщает об ошибке проверки точного ExecutionFence.
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	// edit вносит выбранное нарушение в отдельную копию корректного доменного значения.
	for _, edit := range []func(*core.ExecutionFence){
		func(v *core.ExecutionFence) {
			// Убирает CommandID точного ExecutionFence.
			v.CommandID = ""
		}, func(v *core.ExecutionFence) {
			// Убирает SessionID точного ExecutionFence.
			v.SessionID = ""
		},
		func(v *core.ExecutionFence) {
			// Убирает Incarnation точного ExecutionFence.
			v.Incarnation = ""
		}, func(v *core.ExecutionFence) {
			// Обнуляет Lifecycle точного ExecutionFence.
			v.Lifecycle = 0
		},
		func(v *core.ExecutionFence) {
			// Обнуляет Attempt точного ExecutionFence.
			v.Attempt = 0
		}, func(v *core.ExecutionFence) {
			// Задаёт OutputGeneration без OutputID.
			v.OutputGeneration = 1
		},
		func(v *core.ExecutionFence) {
			// Задаёт OutputID без OutputGeneration.
			v.OutputID = "output"
		},
		func(v *core.ExecutionFence) {
			// Задаёт OutputID с нулевым байтом при положительном поколении.
			v.OutputID = "\x00"
			v.OutputGeneration = 1
		},
	} {
		// value содержит отдельную копию точного ExecutionFence для проверки выбранного условия.
		value := base
		edit(&value)
		// err сообщает об ошибке проверки точного ExecutionFence.
		if err := value.Validate(); !errors.Is(err, core.ErrInvalid) {
			t.Fatalf("неполный fence принят: %v", err)
		}
	}
	base.OutputID, base.OutputGeneration = "output", math.MaxUint64
	// err сообщает об ошибке проверки точного ExecutionFence.
	if err := base.Validate(); err != nil {
		t.Fatalf("точный fence выхода отклонён: %v", err)
	}
}

// Проверяет допустимость пустого стабильного Asset и отказ для некорректных имени, состояния и
// указателей поколений.
func TestFallbackAssetSeparatesStableIdentityFromGenerationMetadata(t *testing.T) {
	// base задаёт стабильную заглушку без выбранного поколения и содержимого.
	base := core.FallbackAsset{Record: contractRecord("asset"), Name: "Заглушка", State: core.AssetPreparing}
	// err сообщает об ошибке проверки стабильной заглушки.
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	// edit вносит выбранное нарушение в отдельную копию корректного доменного значения.
	for _, edit := range []func(*core.FallbackAsset){func(v *core.FallbackAsset) {
		// Убирает имя стабильного FallbackAsset.
		v.Name = ""
	}, func(v *core.FallbackAsset) {
		// Подставляет неизвестное состояние FallbackAsset.
		v.State = "unknown"
	}, func(v *core.FallbackAsset) {
		// Задаёт CurrentGenerationID с недопустимым нулевым байтом.
		v.CurrentGenerationID = "\x00"
	}, func(v *core.FallbackAsset) {
		// Приравнивает PendingGenerationID к CurrentGenerationID.
		v.CurrentGenerationID = "same"
		v.PendingGenerationID = "same"
	}} {
		// v содержит копию стабильной заглушки с выбранным нарушением.
		v := base
		edit(&v)
		// err сообщает об ошибке проверки стабильной заглушки.
		if err := v.Validate(); !errors.Is(err, core.ErrInvalid) {
			t.Fatalf("невалидный стабильный объект принят: %v", err)
		}
	}
}

// Проверяет точный профиль варианта во всех состояниях и обязательность сведений готового файла только
// для READY.
func TestFallbackVariantRequiresExactProfileForEveryPreparationState(t *testing.T) {
	// output задаёт согласованный самостоятельный снимок выхода.
	_, output := contractSnapshots(t)
	// m содержит manifest точного готового варианта для построения проверяемых метаданных.
	m := output.Artifact
	// base задаёт готовый вариант с точным профилем и проверенными метаданными файла.
	base := core.FallbackVariant{GenerationID: m.GenerationID, VerifiedFormat: m.VerifiedFormat, Record: contractRecord(m.VariantID), Incarnation: "owner", AssetID: m.AssetID, SourceHash: m.SourceHash, Profile: m.Profile, ProfileKey: m.ProfileKey, CanonicalProfile: m.CanonicalProfile, GeneratorVersion: m.GeneratorVersion, ManagedPath: m.ManagedPath, SizeBytes: m.SizeBytes, ContentHash: m.ContentHash, State: core.VariantReady, AttemptToken: "attempt"}
	// err сообщает об ошибке проверки варианта заглушки.
	if err := base.Validate(); err != nil {
		t.Fatal(err)
	}
	// state выбирает неготовое состояние варианта без обязательных размера и хеша готового файла.
	for _, state := range []core.VariantState{core.VariantPreparing, core.VariantFailed} {
		// value содержит отдельную копию варианта заглушки для проверки выбранного условия.
		value := base
		value.State, value.SizeBytes, value.ContentHash = state, 0, ""
		// err сообщает об ошибке проверки варианта заглушки.
		if err := value.Validate(); err != nil {
			t.Fatalf("неготовое поколение требует готовый файл: %v", err)
		}
		value.CanonicalProfile = []byte("different-profile")
		// err сообщает об ошибке проверки варианта заглушки.
		if err := value.Validate(); !errors.Is(err, core.ErrInvalid) {
			t.Fatalf("неготовое поколение потеряло точную идентичность: %v", err)
		}
	}
	// tc содержит выбранное изменение или допустимое сочетание полей проверяемой сущности.
	for _, tc := range []struct {
		// name задаёт имя проверяемого нарушения доменного ограничения.
		name string
		// edit вносит выбранное нарушение в отдельную копию корректного доменного значения.
		edit func(*core.FallbackVariant)
	}{
		{"owner", func(v *core.FallbackVariant) {
			// Убирает Incarnation варианта заглушки.
			v.Incarnation = ""
		}},
		{"asset", func(v *core.FallbackVariant) {
			// Убирает AssetID варианта заглушки.
			v.AssetID = ""
		}},
		{"source_hash", func(v *core.FallbackVariant) {
			// Убирает SourceHash варианта заглушки.
			v.SourceHash = ""
		}},
		{"generator", func(v *core.FallbackVariant) {
			// Убирает GeneratorVersion варианта заглушки.
			v.GeneratorVersion = ""
		}},
		{"path", func(v *core.FallbackVariant) {
			// Задаёт путь варианта заглушки с выходом в родительский каталог.
			v.ManagedPath = "../other"
		}},
		{"attempt", func(v *core.FallbackVariant) {
			// Убирает AttemptToken варианта заглушки.
			v.AttemptToken = ""
		}},
		{"state", func(v *core.FallbackVariant) {
			// Подставляет неизвестное состояние варианта заглушки.
			v.State = "unknown"
		}},
		{"identity", func(v *core.FallbackVariant) {
			// Подменяет ProfileKey варианта без изменения профиля.
			v.ProfileKey = "different"
		}},
		{"size", func(v *core.FallbackVariant) {
			// Обнуляет размер готового варианта заглушки.
			v.SizeBytes = 0
		}},
		{"content_hash", func(v *core.FallbackVariant) {
			// Убирает хеш содержимого готового варианта заглушки.
			v.ContentHash = ""
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет ErrInvalid для выбранного нарушения владения, идентичности или готового файла
			// FallbackVariant.

			// value содержит отдельную копию варианта заглушки для проверки выбранного
			// условия.
			value := base
			tc.edit(&value)
			// err сообщает об ошибке проверки варианта заглушки.
			if err := value.Validate(); !errors.Is(err, core.ErrInvalid) {
				t.Fatalf("некорректное поколение принято: %v", err)
			}
		})
	}
}
