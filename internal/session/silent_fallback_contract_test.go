package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net"
	"os"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
)

const silentContractGenerator = "fallback-fmp4-silent-v2"

func silentContractNoNetwork(c *Config, attempts *atomic.Uint64) {
	c.Publisher.Transport.DialContext = func(context.Context, string, string) (net.Conn, error) {
		attempts.Add(1)
		return nil, errors.New("неожиданная сетевая попытка")
	}
}

func TestSilentFallbackRejectsLegacyBindingsWithoutAdmission(t *testing.T) {
	var network, players atomic.Uint64
	f := newManagerFixture(t, false, func(c *Config) {
		c.GeneratorVersion = silentContractGenerator
		silentContractNoNetwork(c, &network)
		c.Group.Player = func(context.Context, string, *os.File, ffmpeg.Attempt) (ffmpeg.Command, error) {
			players.Add(1)
			return ffmpeg.Command{}, core.ErrInvalid
		}
	})
	ctx := context.Background()
	variant, err := f.service.GetVariant(ctx, f.binding.VariantID)
	if err != nil {
		t.Fatal(err)
	}
	generation, err := f.service.GetGeneration(ctx, f.binding.GenerationID)
	if err != nil {
		t.Fatal(err)
	}
	request := core.StartSessionRequest{RequestID: "legacy-binding-start", StreamID: f.stream.ID, DestinationIDs: []core.ID{f.destination.ID}}
	if receipt, err := f.manager.StartSession(ctx, request); !errors.Is(err, core.ErrNotReady) || receipt.ID != "" {
		t.Fatal("старый генератор допущен в новую Session", receipt, err)
	}
	if _, found, err := f.service.PriorStartSession(ctx, request); err != nil || found {
		t.Fatal("отказ оставил квитанцию запуска", found, err)
	}
	parent := startAdmissionParent(t, f)
	add := core.AddOutputRequest{RequestID: "legacy-binding-add", SessionID: parent.SessionID, DestinationID: f.destination.ID}
	if receipt, err := f.manager.AddOutput(ctx, add); !errors.Is(err, core.ErrNotReady) || receipt.ID != "" {
		t.Fatal("старый генератор допущен в новый выход", receipt, err)
	}
	if _, found, err := f.service.PriorAddOutput(ctx, add); err != nil || found {
		t.Fatal("отказ оставил квитанцию выхода", found, err)
	}
	outputs, err := f.service.ListOutputs(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8, SessionID: parent.SessionID}})
	if err != nil || len(outputs.Items) != 0 {
		t.Fatal("отказ создал выход", outputs, err)
	}
	afterVariant, err := f.service.GetVariant(ctx, variant.ID)
	if err != nil || !reflect.DeepEqual(afterVariant, variant) || afterVariant.State != core.VariantReady {
		t.Fatal("отказ изменил исторический вариант", afterVariant, err)
	}
	afterGeneration, err := f.service.GetGeneration(ctx, generation.ID)
	if err != nil || !reflect.DeepEqual(afterGeneration, generation) {
		t.Fatal("отказ изменил историческое поколение", afterGeneration, err)
	}
	if network.Load() != 0 || players.Load() != 0 {
		t.Fatal("отказ допуска начал внешнее выполнение", network.Load(), players.Load())
	}
}

func silentContractSnapshotBytes(t *testing.T, snapshot core.OutputSnapshot) []byte {
	t.Helper()
	encoded, err := core.EncodeOutputSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestSilentFallbackAcceptedLegacySessionFailsBeforeFeed(t *testing.T) {
	var network, players atomic.Uint64
	f := newManagerFixture(t, true, func(c *Config) {
		c.GeneratorVersion = silentContractGenerator
		silentContractNoNetwork(c, &network)
		original := c.Group.Player
		c.Group.Player = func(ctx context.Context, binary string, file *os.File, attempt ffmpeg.Attempt) (ffmpeg.Command, error) {
			players.Add(1)
			return original(ctx, binary, file, attempt)
		}
	})
	ctx := context.Background()
	request := core.StartSessionRequest{RequestID: "accepted-legacy-session", StreamID: f.stream.ID, Bindings: []core.PreparedBinding{f.binding}}
	accepted, err := f.service.StartSession(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	failed := waitReceipt(t, f, accepted)
	if failed.Status != core.CommandFailed || !strings.Contains(failed.Failure, core.ErrNotReady.Error()) {
		t.Fatal("прежняя команда запустила Session со старой заглушкой", failed)
	}
	session, err := f.service.GetSession(ctx, accepted.SessionID)
	if err != nil || session.State != core.SessionFailed || session.Snapshot.Validate() != nil {
		t.Fatal("история отказавшей Session не читается", session, err)
	}
	if feed, err := f.manager.Feed(session); err == nil || feed != nil {
		t.Fatal("старый fallback допустил Feed", feed, err)
	}
	duplicate, err := f.manager.StartSession(ctx, request)
	if err != nil || !reflect.DeepEqual(duplicate, failed) {
		t.Fatal("повтор изменил прежнюю квитанцию Session", duplicate, failed, err)
	}
	if err = f.manager.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if network.Load() != 0 || players.Load() != 0 {
		t.Fatal("отказ Session начал внешнее выполнение", network.Load(), players.Load())
	}
}

func TestSilentFallbackAcceptedLegacyOutputCannotStartOrRestart(t *testing.T) {
	var network atomic.Uint64
	f := newManagerFixture(t, false, func(c *Config) {
		c.GeneratorVersion = silentContractGenerator
		silentContractNoNetwork(c, &network)
	})
	ctx := context.Background()
	parent := startAdmissionParent(t, f)
	request := core.AddOutputRequest{RequestID: "accepted-legacy-output", SessionID: parent.SessionID, DestinationID: f.destination.ID, Binding: f.binding}
	// Сохранённая команда могла быть принята до смены серверной версии генератора.
	accepted, err := f.service.AddOutput(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	before, err := f.service.GetOutput(ctx, accepted.OutputID)
	if err != nil {
		t.Fatal(err)
	}
	original := silentContractSnapshotBytes(t, before.Snapshot)
	failed := waitReceipt(t, f, accepted)
	if failed.Status != core.CommandFailed || !strings.Contains(failed.Failure, core.ErrNotReady.Error()) {
		t.Fatal("сохранённая команда использовала старую заглушку", failed)
	}
	duplicate, err := f.manager.AddOutput(ctx, request)
	if err != nil || !reflect.DeepEqual(duplicate, failed) {
		t.Fatal("версия генератора переписала прежнюю квитанцию", duplicate, failed, err)
	}
	joined, stop := context.WithTimeout(ctx, 8*time.Second)
	defer stop()
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		output, err := f.service.GetOutput(joined, accepted.OutputID)
		if err != nil {
			t.Fatal(err)
		}
		if !output.DesiredRunning && !output.CleanupPending {
			break
		}
		select {
		case <-joined.Done():
			t.Fatal("не закончилась очистка отказавшего выхода", output, joined.Err())
		case <-ticker.C:
		}
	}
	restart, err := f.manager.StartOutput(ctx, core.OutputCommand{RequestID: "restart-legacy-output", OutputID: accepted.OutputID})
	if err != nil {
		t.Fatal(err)
	}
	if result := waitReceipt(t, f, restart); result.Status != core.CommandFailed || !strings.Contains(result.Failure, core.ErrNotReady.Error()) {
		t.Fatal("перезапуск использовал старую заглушку", result)
	}
	after, err := f.service.GetOutput(ctx, accepted.OutputID)
	if err != nil || !bytes.Equal(original, silentContractSnapshotBytes(t, after.Snapshot)) || after.Generation <= before.Generation {
		t.Fatal("перезапуск подменил закреплённый снимок", after, err)
	}
	file, err := f.files.OpenVerified(ctx, after.Snapshot.Artifact)
	if err != nil {
		t.Fatal("исторический файл перестал читаться", err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	if err = f.manager.Close(ctx); err != nil {
		t.Fatal(err)
	}
	if network.Load() != 0 {
		t.Fatal("непригодный fallback допустил Publisher к сети", network.Load())
	}
}

func silentContractPublishCurrent(t *testing.T, c *Config) {
	t.Helper()
	ctx := context.Background()
	assets, err := c.Core.ListAssets(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8}})
	if err != nil || len(assets.Items) != 1 {
		t.Fatal(assets, err)
	}
	asset := assets.Items[0]
	generation, err := c.Core.ReservePreparation(ctx, core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage, GeneratorVersion: silentContractGenerator, Rebuild: true})
	if err != nil {
		t.Fatal(err)
	}
	fence, err := c.Core.ClaimPreparation(ctx, generation.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Core.MarkPreparationIssued(ctx, fence); err != nil {
		t.Fatal(err)
	}
	if err = c.Files.CreateGeneration(generation.ID); err != nil {
		t.Fatal(err)
	}
	required := generation.RequiredProfiles[0]
	content := []byte("проверенный файл нового поколения")
	file, err := c.Files.Create(required.ManagedPath)
	if err != nil {
		t.Fatal(err)
	}
	_, writeErr := file.Write(content)
	if err = errors.Join(writeErr, file.Close()); err != nil {
		t.Fatal(err)
	}
	digest := sha256.Sum256(content)
	_, err = c.Core.PublishPreparation(ctx, fence, core.GenerationPublication{SourceHash: asset.SourceHash, SizeBytes: asset.SizeBytes, SourceFormat: asset.SourceFormat, Variants: []core.FallbackVariant{{ID: required.VariantID, GenerationID: generation.ID, AssetID: asset.ID, SourceHash: asset.SourceHash, Profile: required.Profile, VerifiedFormat: required.Profile.Format, GeneratorVersion: silentContractGenerator, ManagedPath: required.ManagedPath, SizeBytes: uint64(len(content)), ContentHash: hex.EncodeToString(digest[:])}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Core.AcknowledgePreparationCleanup(ctx, fence); err != nil {
		t.Fatal(err)
	}
}

func TestSilentFallbackChecksGeneratorBeforeMembershipReuse(t *testing.T) {
	f := newManagerFixture(t, true, func(c *Config) {
		silentContractPublishCurrent(t, c)
		c.GeneratorVersion = silentContractGenerator
	})
	ctx := context.Background()
	parent := startAdmissionParent(t, f)
	outputs, err := f.service.ListOutputs(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8, SessionID: parent.SessionID}})
	if err != nil || len(outputs.Items) != 1 {
		t.Fatal(outputs, err)
	}
	output := outputs.Items[0]
	f.manager.mu.Lock()
	node := f.manager.sessions[parent.SessionID]
	f.manager.mu.Unlock()
	member, err := node.acquire(ctx, output)
	if err != nil || member == nil {
		t.Fatal("текущее беззвучное поколение не получило membership", err)
	}
	for range 2 {
		file, err := f.manager.openPreparedFile(ctx, output.Snapshot.Artifact)
		if err != nil {
			t.Fatal("текущий manifest не открылся повторно", err)
		}
		content, readErr := io.ReadAll(file)
		if err = errors.Join(readErr, file.Close()); err != nil {
			t.Fatal(err)
		}
		digest := sha256.Sum256(content)
		if uint64(len(content)) != output.Snapshot.Artifact.SizeBytes || hex.EncodeToString(digest[:]) != output.Snapshot.Artifact.ContentHash {
			t.Fatal("открыт другой файл или сохранено смещение предыдущей попытки")
		}
	}
	corrupt := output.Snapshot.Artifact
	corrupt.ContentHash = strings.Repeat("0", sha256.Size*2)
	if file, err := f.manager.openPreparedFile(ctx, corrupt); !errors.Is(err, core.ErrCorrupt) || file != nil {
		t.Fatal("совпадение версии обошло проверку содержимого", file, err)
	}
	oldVariant, err := f.service.GetVariant(ctx, f.binding.VariantID)
	if err != nil {
		t.Fatal("прежний вариант перестал читаться после явной подготовки нового", err)
	}
	legacy := output
	legacy.Snapshot.Artifact = core.ArtifactManifest{GenerationID: oldVariant.GenerationID, VariantID: oldVariant.ID, AssetID: oldVariant.AssetID, SourceHash: oldVariant.SourceHash, Profile: oldVariant.Profile, ProfileKey: oldVariant.ProfileKey, CanonicalProfile: bytes.Clone(oldVariant.CanonicalProfile), GeneratorVersion: oldVariant.GeneratorVersion, ManagedPath: oldVariant.ManagedPath, SizeBytes: oldVariant.SizeBytes, ContentHash: oldVariant.ContentHash, VerifiedFormat: oldVariant.VerifiedFormat}
	if err = legacy.Validate(); err != nil {
		t.Fatal("контрпример должен сохранять валидность снимка", err)
	}
	legacyBytes := silentContractSnapshotBytes(t, legacy.Snapshot)
	if reused, err := node.acquire(ctx, legacy); !errors.Is(err, core.ErrNotReady) || reused != nil {
		t.Fatal("старый генератор обошёл проверку через готовый membership", reused, err)
	}
	if !bytes.Equal(legacyBytes, silentContractSnapshotBytes(t, legacy.Snapshot)) {
		t.Fatal("старый снимок подменён новым вариантом")
	}
	if reused, err := node.acquire(ctx, output); err != nil || reused != member {
		t.Fatal("отказ старому снимку повредил текущий membership", err)
	}
}

type silentContractLog struct {
	mu sync.Mutex
	bytes.Buffer
}

func (l *silentContractLog) Write(p []byte) (int, error) {
	l.mu.Lock()
	defer l.mu.Unlock()
	return l.Buffer.Write(p)
}

func (l *silentContractLog) records() []byte {
	l.mu.Lock()
	defer l.mu.Unlock()
	return bytes.Clone(l.Bytes())
}

func TestSilentFallbackSessionOpenerRejectsEveryLegacyPlayerAttempt(t *testing.T) {
	var templateOpens, players, network atomic.Uint64
	logs := &silentContractLog{}
	f := newManagerFixture(t, false, func(c *Config) {
		c.GeneratorVersion = silentContractGenerator
		c.Logger = slog.New(slog.NewJSONHandler(logs, nil))
		c.Group.FFmpeg.MaxRestarts = 1
		c.Group.OpenFile = func(ctx context.Context, manifest core.ArtifactManifest) (*os.File, error) {
			templateOpens.Add(1)
			return c.Files.OpenVerified(ctx, manifest)
		}
		original := c.Group.Player
		c.Group.Player = func(ctx context.Context, binary string, file *os.File, attempt ffmpeg.Attempt) (ffmpeg.Command, error) {
			players.Add(1)
			return original(ctx, binary, file, attempt)
		}
		silentContractNoNetwork(c, &network)
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	parent := startAdmissionParent(t, f)
	f.manager.mu.Lock()
	accepted, err := f.service.AddOutput(ctx, core.AddOutputRequest{RequestID: "legacy-player-attempts", SessionID: parent.SessionID, DestinationID: f.destination.ID, Binding: f.binding})
	if err == nil {
		f.manager.commands[accepted.ID] = true
	}
	node := f.manager.sessions[parent.SessionID]
	f.manager.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	output, err := f.service.GetOutput(ctx, accepted.OutputID)
	if err != nil {
		t.Fatal(err)
	}
	manifest := output.Snapshot.Artifact
	file, err := f.files.OpenVerified(ctx, manifest)
	if err != nil {
		t.Fatal("контрпример требует физически исправный старый файл", err)
	}
	if err = file.Close(); err != nil {
		t.Fatal(err)
	}
	// Registry получает настоящий сохранённый снимок в обход более раннего допуска членства в группе.
	ref, err := node.registry.Acquire(ctx, output.Snapshot, node.router)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := ref.CloseContext(context.Background()); err != nil {
			t.Error(err)
		}
	})
	if err = ref.Group().WaitReady(ctx); !errors.Is(err, core.ErrNotReady) {
		t.Fatal("принадлежащая Session функция открытия допустила старый файл", err)
	}
	results := map[uint64]bool{}
	decoder := json.NewDecoder(bytes.NewReader(logs.records()))
	for {
		var record struct {
			Operation string `json:"operation"`
			Attempt   uint64 `json:"attempt"`
			Error     string `json:"error"`
		}
		if err := decoder.Decode(&record); errors.Is(err, io.EOF) {
			break
		} else if err != nil {
			t.Fatal(err)
		}
		if record.Operation == "attempt_result" {
			results[record.Attempt] = strings.Contains(record.Error, core.ErrNotReady.Error())
		}
	}
	if len(results) != 2 || !results[1] || !results[2] {
		t.Fatalf("повтор player не подтвердил тот же отказ версии: %v; %s", results, logs.records())
	}
	if templateOpens.Load() != 0 || players.Load() != 0 || network.Load() != 0 {
		t.Fatal("проверка версии пропустила открытие или внешнее выполнение", templateOpens.Load(), players.Load(), network.Load())
	}
	for range 2 {
		if file, err := f.manager.openPreparedFile(ctx, manifest); !errors.Is(err, core.ErrNotReady) || file != nil {
			t.Fatal("повтор открытия разрешил исторический manifest", file, err)
		}
	}
	missing := manifest
	missing.ManagedPath += ".missing"
	if _, err := f.manager.openPreparedFile(ctx, missing); !errors.Is(err, core.ErrNotReady) {
		t.Fatal("файловая проверка опередила проверку версии", err)
	}
}

func TestOutputCleanupJoinsProvisionalMembershipBeforeReleasingOwnership(t *testing.T) {
	f := newManagerFixture(t, false, func(c *Config) {
		c.Group.Player = func(context.Context, string, *os.File, ffmpeg.Attempt) (ffmpeg.Command, error) {
			return ffmpeg.Command{}, core.ErrNotReady
		}
	})
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	parent := startAdmissionParent(t, f)
	f.manager.mu.Lock()
	accepted, err := f.service.AddOutput(ctx, core.AddOutputRequest{RequestID: "provisional-membership-cleanup", SessionID: parent.SessionID, DestinationID: f.destination.ID, Binding: f.binding})
	if err == nil {
		f.manager.commands[accepted.ID] = true
	}
	node := f.manager.sessions[parent.SessionID]
	f.manager.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	output, err := f.service.GetOutput(ctx, accepted.OutputID)
	if err != nil {
		t.Fatal(err)
	}
	member, err := node.acquire(ctx, output)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := member.close(context.Background()); err != nil {
			t.Error(err)
		}
	})
	select {
	case <-member.ready:
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	member.mu.Lock()
	ref, readinessErr := member.reference, member.err
	member.mu.Unlock()
	if ref == nil || !errors.Is(readinessErr, core.ErrNotReady) {
		t.Fatal("не создано предварительное владение группой", ref, readinessErr)
	}
	fence, err := f.manager.issue(ctx, accepted)
	if err != nil {
		t.Fatal(err)
	}
	_, stop := context.WithCancel(node.ctx)
	stop()
	done := make(chan struct{})
	close(done)
	// Отмена запуска до получения членства в группе оставляет предварительную ссылку у Session.
	failed := &outputNode{fence: fence, startReceipt: accepted, cancel: stop, done: done, failure: context.Canceled}
	node.mu.Lock()
	node.outputs[output.ID] = failed
	node.mu.Unlock()
	if err = node.waitOutputCleanup(ctx, output.ID, output.Generation); err != nil {
		t.Fatal("не завершилась очистка отменённого выхода", err)
	}
	select {
	case <-ref.Group().Done():
	default:
		t.Fatal("очистка потеряла владение ещё работающей предварительной группой")
	}
	proof, err := node.scope.Resolve(ctx)
	if err != nil || proof.Unknown || proof.PendingOperations != 0 {
		t.Fatal("очистка потеряла незавершённую операцию Scope предварительной группы", proof, err)
	}
}
