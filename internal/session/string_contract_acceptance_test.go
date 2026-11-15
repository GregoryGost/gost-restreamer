package session

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// membershipContractOutput создаёт согласованный снимок без БД, файлов и работников.
// t сообщает об ошибке канонизации профиля из существующей медиаконфигурации.
func membershipContractOutput(t *testing.T) core.SessionOutput {
	t.Helper()
	// profile содержит действующий AV1/AAC-профиль; key и canonical задают его точную идентичность,
	// а err сообщает об ошибке её вычисления.
	profile, _, _ := fixtureProfile(t)
	key, canonical, err := core.ProfileIdentity(profile)
	if err != nil {
		t.Fatal(err)
	}
	// now фиксирует общее корректное время записей снимка.
	now := time.Date(2026, 1, 1, 0, 0, 0, 0, time.UTC)
	// record создаёт запись id с одинаковыми временами и начальной ревизией.
	record := func(id core.ID) core.Record {
		return core.Record{ID: id, Revision: 1, CreatedAt: now, UpdatedAt: now}
	}
	// snapshot связывает неизменяемый профиль, группу, назначение и точный проверенный файл.
	snapshot := core.OutputSnapshot{
		Destination: core.Destination{Record: record("destination"), StreamID: "stream", GroupID: "group", Name: "Выход", Platform: "custom", Endpoint: "rtmp://example.invalid/live"},
		Group:       core.TranscodeGroup{Record: record("group"), StreamID: "stream", ProfileID: "profile", Name: "Группа"},
		Profile:     core.TranscodeProfile{Record: record("profile"), Name: "Профиль", Profile: profile},
		Fallback:    core.FallbackPolicy{AssetID: "asset", Watchdog: core.WatchdogPolicy{VideoTimeout: time.Second, AudioTimeout: time.Second, RecoveryWindow: time.Second, KeyframeTimeout: time.Second}},
		Artifact:    core.ArtifactManifest{GenerationID: "generation", VariantID: "variant", AssetID: "asset", SourceHash: "source", Profile: profile, ProfileKey: key, CanonicalProfile: canonical, GeneratorVersion: core.CurrentFallbackGeneratorVersion, ManagedPath: "fallback/variant.mp4", SizeBytes: 1, ContentHash: "content", VerifiedFormat: profile.Format},
	}
	if err = snapshot.ValidateUsable(); err != nil {
		t.Fatal(err)
	}
	return core.SessionOutput{Record: record("output"), Generation: 1, Snapshot: snapshot}
}

// TestMembershipRejectsInvalidatedSnapshotBeforeRegistrationOrReuse проверяет ранний отказ
// RequiresRebuild до Scope.Register и возврата прежнего членства. t сверяет сохранность пригодного соседа.
func TestMembershipRejectsInvalidatedSnapshotBeforeRegistrationOrReuse(t *testing.T) {
	// healthy содержит самостоятельный пригодный снимок без внешних ресурсов.
	healthy := membershipContractOutput(t)
	// member моделирует уже зарегистрированное членство того же поколения выхода.
	member := &membership{output: healthy}
	// node содержит прежнее членство; отсутствие scope исключает неучтённую регистрацию работника.
	node := &sessionNode{manager: &Manager{config: Config{GeneratorVersion: core.CurrentFallbackGeneratorVersion, MaxOutputs: 2}}, members: map[core.ID]*membership{healthy.ID: member}, excluded: map[core.ID]uint64{}}
	// invalid отличается только аннулированием доказательства пригодности файла.
	invalid := healthy
	invalid.Snapshot.Artifact.RequiresRebuild = true
	// before сохраняет точный снимок для проверки отсутствия автоматической перепривязки.
	before := invalid
	// id выбирает прежнее членство и новый выход, ещё не зарегистрированный в scope.
	for _, id := range []core.ID{healthy.ID, "new-output"} {
		invalid.ID = id
		// result и err фиксируют отказ до использования прежнего членства либо регистрации нового.
		result, err := node.acquire(context.Background(), invalid)
		if !errors.Is(err, core.ErrNotReady) || result != nil || len(node.members) != 1 || node.members[healthy.ID] != member {
			t.Fatal("Аннулированный снимок зарегистрирован или переиспользован", result, err)
		}
	}
	invalid.ID = before.ID
	if !reflect.DeepEqual(invalid, before) {
		t.Fatal("Отказ подменил точный исторический снимок")
	}
	// result и err подтверждают пригодность прежнего неизменённого членства после обоих отказов.
	result, err := node.acquire(context.Background(), healthy)
	if err != nil || result != member {
		t.Fatal("Неизменённый снимок потерял прежнее членство", result, err)
	}
}
