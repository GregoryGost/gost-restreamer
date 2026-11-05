package storage

import (
	"context"
	"errors"
	"path/filepath"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

type generationAcceptanceFixture struct {
	store       *Store
	svc         *core.Service
	asset       core.FallbackAsset
	stream      core.Stream
	profile     core.TranscodeProfile
	group       core.TranscodeGroup
	destination core.Destination
}

func newGenerationAcceptanceFixture(t *testing.T) generationAcceptanceFixture {
	t.Helper()
	o := testOptions()
	o.PreparationLimits = core.PreparationLimits{MaxQueued: 3, MaxProfiles: 8}
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "generation.sqlite"), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	svc, err := core.NewService(s, s.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	f := generationAcceptanceFixture{store: s, svc: svc}
	f.asset, err = svc.CreateAsset(context.Background(), "Выбранная заглушка")
	if err != nil {
		t.Fatal(err)
	}
	f.stream, err = svc.CreateStream(context.Background(), core.Stream{Name: "Поток", PublicationKey: "generation-key", Fallback: core.FallbackPolicy{AssetID: f.asset.ID, Watchdog: core.WatchdogPolicy{VideoTimeout: time.Second, AudioTimeout: time.Second, RecoveryWindow: time.Second, KeyframeTimeout: time.Second}}})
	if err != nil {
		t.Fatal(err)
	}
	f.profile, err = svc.CreateProfile(context.Background(), core.TranscodeProfile{Name: "Профиль", Profile: storageProfile()})
	if err != nil {
		t.Fatal(err)
	}
	f.group, err = svc.CreateGroup(context.Background(), core.TranscodeGroup{Name: "Группа", StreamID: f.stream.ID, ProfileID: f.profile.ID})
	if err != nil {
		t.Fatal(err)
	}
	f.destination, err = svc.CreateDestination(context.Background(), core.Destination{Name: "Назначение", StreamID: f.stream.ID, GroupID: f.group.ID, Platform: "custom", Endpoint: "rtmp://example.invalid/live", Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	return f
}

func reserveGenerationAcceptance(t *testing.T, svc *core.Service, assetID core.ID) core.FallbackGeneration {
	t.Helper()
	ctx := context.Background()
	a, err := svc.GetAsset(ctx, assetID)
	if err != nil {
		t.Fatal(err)
	}
	g, err := svc.ReservePreparation(ctx, core.PreparationRequest{AssetID: a.ID, ExpectedRevision: a.Revision, Kind: core.AssetImage, GeneratorVersion: "acceptance-generator"})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

func issueGenerationAcceptance(t *testing.T, svc *core.Service, g core.FallbackGeneration) core.PreparationFence {
	t.Helper()
	ctx := context.Background()
	f, err := svc.ClaimPreparation(ctx, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.MarkPreparationIssued(ctx, f); err != nil {
		t.Fatal(err)
	}
	return f
}

func generationAcceptancePublication(g core.FallbackGeneration) core.GenerationPublication {
	p := core.GenerationPublication{SourceHash: "same-source-bytes", SizeBytes: 37, SourceFormat: core.SourceFormat{ImageFormat: "png"}}
	for _, r := range g.RequiredProfiles {
		p.Variants = append(p.Variants, core.FallbackVariant{Record: core.Record{ID: r.VariantID}, AssetID: g.AssetID, GenerationID: g.ID, SourceHash: p.SourceHash, Profile: r.Profile, GeneratorVersion: g.GeneratorVersion, ManagedPath: r.ManagedPath, SizeBytes: 1234, ContentHash: "same-result-bytes", VerifiedFormat: r.Profile.Format})
	}
	return p
}

func publishGenerationAcceptance(t *testing.T, svc *core.Service, assetID core.ID) (core.FallbackGeneration, core.GenerationPublication) {
	t.Helper()
	ctx := context.Background()
	g := reserveGenerationAcceptance(t, svc, assetID)
	f := issueGenerationAcceptance(t, svc, g)
	p := generationAcceptancePublication(g)
	published, err := svc.PublishPreparation(ctx, f, p)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.AcknowledgePreparationCleanup(ctx, f); err != nil {
		t.Fatal(err)
	}
	return published, p
}

func (f generationAcceptanceFixture) start(id string, g core.FallbackGeneration) core.StartSessionRequest {
	return core.StartSessionRequest{RequestID: id, StreamID: f.stream.ID, Bindings: []core.PreparedBinding{{GroupID: f.group.ID, GenerationID: g.ID, VariantID: g.RequiredProfiles[0].VariantID, GeneratorVersion: g.GeneratorVersion}}}
}

func TestGenerationReplacementIsAtomicFreshAndAssetScoped(t *testing.T) {
	f := newGenerationAcceptanceFixture(t)
	ctx := context.Background()
	first, _ := publishGenerationAcceptance(t, f.svc, f.asset.ID)
	other, err := f.svc.CreateAsset(ctx, "Другая заглушка")
	if err != nil {
		t.Fatal(err)
	}
	publishGenerationAcceptance(t, f.svc, other.ID)
	other, err = f.svc.GetAsset(ctx, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	g := reserveGenerationAcceptance(t, f.svc, f.asset.ID)
	fence := issueGenerationAcceptance(t, f.svc, g)
	bad := generationAcceptancePublication(g)
	bad.Variants = nil
	_, err = f.svc.PublishPreparation(ctx, fence, bad)
	if !errors.Is(err, core.ErrConflict) {
		t.Fatalf("неполный комплект: %v", err)
	}
	a, err := f.svc.GetAsset(ctx, f.asset.ID)
	if err != nil || a.CurrentGenerationID != first.ID || a.PendingGenerationID != g.ID || a.State != core.AssetReady {
		t.Fatalf("ошибка публикации повредила current: %+v, %v", a, err)
	}
	if err = f.svc.CancelPreparation(ctx, fence, "Отказ проверки"); err != nil {
		t.Fatal(err)
	}
	if err = f.svc.AcknowledgePreparationCleanup(ctx, fence); err != nil {
		t.Fatal(err)
	}
	second, _ := publishGenerationAcceptance(t, f.svc, f.asset.ID)
	if second.ID == first.ID || second.OriginalPath == first.OriginalPath || second.RequiredProfiles[0].VariantID == first.RequiredProfiles[0].VariantID || second.RequiredProfiles[0].ManagedPath == first.RequiredProfiles[0].ManagedPath {
		t.Fatal("равное содержимое переиспользовало поколение или файл")
	}
	a, err = f.svc.GetAsset(ctx, f.asset.ID)
	if err != nil || a.CurrentGenerationID != second.ID || a.PendingGenerationID != "" {
		t.Fatalf("новый комплект не стал current: %+v, %v", a, err)
	}
	got, err := f.svc.GetAsset(ctx, other.ID)
	if err != nil || !reflect.DeepEqual(got, other) {
		t.Fatalf("замена изменила другую заглушку: %+v, %v", got, err)
	}
	old, err := f.svc.GetGeneration(ctx, first.ID)
	if err != nil || old.State != core.PreparationPublished || old.SourceHash != second.SourceHash {
		t.Fatalf("прежний комплект потерян: %+v, %v", old, err)
	}
}

func TestGenerationPublicationDetectsProfileChangesButNotNames(t *testing.T) {
	for _, change := range []string{"name", "profile", "unrelated"} {
		t.Run(change, func(t *testing.T) {
			f := newGenerationAcceptanceFixture(t)
			ctx := context.Background()
			old, _ := publishGenerationAcceptance(t, f.svc, f.asset.ID)
			g := reserveGenerationAcceptance(t, f.svc, f.asset.ID)
			fence := issueGenerationAcceptance(t, f.svc, g)
			p := f.profile
			if change == "profile" {
				p.Profile.VideoEncoding.GOPFrames++
			} else {
				p.Name = "Другое имя"
			}
			if change == "unrelated" {
				_, err := f.svc.CreateProfile(ctx, core.TranscodeProfile{Name: "Несвязанный", Profile: storageProfile()})
				if err != nil {
					t.Fatal(err)
				}
			} else if _, err := f.svc.UpdateProfile(ctx, p, p.Revision); err != nil {
				t.Fatal(err)
			}
			_, err := f.svc.PublishPreparation(ctx, fence, generationAcceptancePublication(g))
			if change == "profile" {
				if !errors.Is(err, core.ErrConflict) {
					t.Fatalf("изменение полного профиля: %v", err)
				}
				a, e := f.svc.GetAsset(ctx, f.asset.ID)
				if e != nil || a.CurrentGenerationID != old.ID {
					t.Fatalf("конфликт изменил current: %+v, %v", a, e)
				}
			} else if err != nil {
				t.Fatalf("несущественная правка помешала публикации: %v", err)
			}
		})
	}
}

func TestPreparationAdmissionIsSharedAndPhysicalCompletionIsRequired(t *testing.T) {
	f := newGenerationAcceptanceFixture(t)
	ctx := context.Background()
	ready, _ := publishGenerationAcceptance(t, f.svc, f.asset.ID)
	otherService, err := core.NewService(f.store, f.store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	g := reserveGenerationAcceptance(t, f.svc, f.asset.ID)
	if _, err = otherService.StartSession(ctx, f.start("blocked-by-accepted", ready)); !errors.Is(err, core.ErrNotReady) {
		t.Fatalf("очередь не исключила start другого Service: %v", err)
	}
	fence := issueGenerationAcceptance(t, otherService, g)
	owner, err := otherService.BindPreparationOwner(ctx, fence)
	if err != nil {
		t.Fatal(err)
	}
	finish, err := owner.Register(ctx)
	if err != nil {
		t.Fatal(err)
	}
	defer finish()
	if err = otherService.CancelPreparation(ctx, fence, "Отмена"); err != nil {
		t.Fatal(err)
	}
	if owner.Valid() {
		t.Fatal("отмена оставила действующего владельца")
	}
	if _, err = owner.Register(ctx); !errors.Is(err, core.ErrStale) {
		t.Fatalf("отозванный владелец принял новую работу: %v", err)
	}
	if err = f.svc.AcknowledgePreparationCleanup(ctx, fence); !errors.Is(err, core.ErrInUse) {
		t.Fatalf("cleanup подтверждён до join: %v", err)
	}
	start, err := f.svc.StartSession(ctx, f.start("manual-before-preparation-join", ready))
	if err != nil {
		t.Fatalf("терминальная подготовка заблокировала ручной start: %v", err)
	}
	executeGenerationCommand(t, f.svc, start.RequestID)
	before, err := f.svc.GetGeneration(ctx, g.ID)
	if err != nil || !before.Issued || !before.CleanupPending {
		t.Fatalf("новый start потерял прежние обязательства: %+v, %v", before, err)
	}
	finish()
	if err = f.svc.AcknowledgePreparationCleanup(ctx, fence); err != nil {
		t.Fatal(err)
	}
	if current, err := f.svc.GetSession(ctx, start.SessionID); err != nil || current.State != core.SessionRunning {
		t.Fatalf("старый cleanup затронул новый запуск: %+v, %v", current, err)
	}
}

func TestPreparationAndSessionAdmissionCannotBothWin(t *testing.T) {
	f := newGenerationAcceptanceFixture(t)
	ctx := context.Background()
	ready, _ := publishGenerationAcceptance(t, f.svc, f.asset.ID)
	a, err := f.svc.GetAsset(ctx, f.asset.ID)
	if err != nil {
		t.Fatal(err)
	}
	other, err := core.NewService(f.store, f.store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	begin := make(chan struct{})
	var wg sync.WaitGroup
	wg.Add(2)
	var prepErr, startErr error
	go func() {
		defer wg.Done()
		<-begin
		_, prepErr = f.svc.ReservePreparation(ctx, core.PreparationRequest{AssetID: a.ID, ExpectedRevision: a.Revision, Kind: core.AssetImage, GeneratorVersion: "concurrent-generator"})
	}()
	go func() {
		defer wg.Done()
		<-begin
		_, startErr = other.StartSession(ctx, f.start("racing-start", ready))
	}()
	close(begin)
	wg.Wait()
	if (prepErr == nil) == (startErr == nil) {
		t.Fatalf("нужен один победитель: preparation=%v, start=%v", prepErr, startErr)
	}
	if prepErr != nil && !errors.Is(prepErr, core.ErrInUse) {
		t.Fatalf("неожиданный отказ подготовки: %v", prepErr)
	}
	if startErr != nil && !errors.Is(startErr, core.ErrNotReady) {
		t.Fatalf("неожиданный отказ start: %v", startErr)
	}
}

func TestFreshBindingRejectsOldGenerationAndReceiptSurvivesReplacement(t *testing.T) {
	f := newGenerationAcceptanceFixture(t)
	ctx := context.Background()
	first, _ := publishGenerationAcceptance(t, f.svc, f.asset.ID)
	request := f.start("accepted-before-replacement", first)
	receipt, err := f.svc.StartSession(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	fence, err := f.svc.Claim(ctx, request.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.svc.Complete(ctx, fence, core.Completion{Failure: "Ресурсы не запускались"}); err != nil {
		t.Fatal(err)
	}
	second, _ := publishGenerationAcceptance(t, f.svc, f.asset.ID)
	if _, err = f.svc.StartSession(ctx, f.start("fresh-with-stale-binding", first)); !errors.Is(err, core.ErrNotReady) {
		t.Fatalf("новый start принял старое READY поколение: %v", err)
	}
	stored, err := f.svc.GetReceipt(ctx, receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	request.Bindings = f.start("", second).Bindings
	replay, err := f.svc.StartSession(ctx, request)
	if err != nil || !reflect.DeepEqual(replay, stored) {
		t.Fatalf("повтор принятого RequestID изменил receipt: %+v, %v", replay, err)
	}
	if _, err = f.svc.StartSession(ctx, f.start("fresh-with-current", second)); err != nil {
		t.Fatalf("точный current отклонён: %v", err)
	}
}

func executeGenerationCommand(t *testing.T, svc *core.Service, id string) core.ExecutionFence {
	t.Helper()
	ctx := context.Background()
	f, err := svc.Claim(ctx, id)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.MarkIssued(ctx, f); err != nil {
		t.Fatal(err)
	}
	if err = svc.Complete(ctx, f, core.Completion{Success: true}); err != nil {
		t.Fatal(err)
	}
	return f
}

func TestPreparationRemainsExcludedDuringPauseAndBothCleanupOrders(t *testing.T) {
	for _, parentFirst := range []bool{true, false} {
		name := "output-first"
		if parentFirst {
			name = "parent-first"
		}
		t.Run(name, func(t *testing.T) {
			f := newGenerationAcceptanceFixture(t)
			ctx := context.Background()
			ready, _ := publishGenerationAcceptance(t, f.svc, f.asset.ID)
			start, err := f.svc.StartSession(ctx, f.start("issued-session", ready))
			if err != nil {
				t.Fatal(err)
			}
			executeGenerationCommand(t, f.svc, start.RequestID)
			commands, err := f.svc.ListReceipts(ctx, core.PageRequest{Query: core.ListQuery{Limit: 50, SessionID: start.SessionID}})
			if err != nil {
				t.Fatal(err)
			}
			for _, c := range commands.Items {
				if c.Kind == core.CommandStartOutput {
					executeGenerationCommand(t, f.svc, c.RequestID)
				}
			}
			pause, err := f.svc.PauseSession(ctx, core.SessionCommand{RequestID: "pause-running-session", SessionID: start.SessionID})
			if err != nil {
				t.Fatal(err)
			}
			executeGenerationCommand(t, f.svc, pause.RequestID)
			other, err := f.svc.CreateAsset(ctx, "Заглушка другого потока")
			if err != nil {
				t.Fatal(err)
			}
			reserve := func() error {
				_, e := f.svc.ReservePreparation(ctx, core.PreparationRequest{AssetID: other.ID, ExpectedRevision: other.Revision, Kind: core.AssetImage, GeneratorVersion: "new-generator"})
				return e
			}
			if err = reserve(); !errors.Is(err, core.ErrInUse) {
				t.Fatalf("пауза разрешила подготовку другого asset: %v", err)
			}
			stop, err := f.svc.StopSession(ctx, core.SessionCommand{RequestID: "stop-issued-session", SessionID: start.SessionID})
			if err != nil {
				t.Fatal(err)
			}
			executeGenerationCommand(t, f.svc, stop.RequestID)
			intents, err := f.svc.ListCleanups(ctx, core.PageRequest{Query: core.ListQuery{Limit: 50, SessionID: start.SessionID, PendingOnly: true}})
			if err != nil || len(intents.Items) != 2 {
				t.Fatalf("нужны независимые parent/output intents: %+v, %v", intents, err)
			}
			if (intents.Items[0].OutputID == "") != parentFirst {
				intents.Items[0], intents.Items[1] = intents.Items[1], intents.Items[0]
			}
			if err = reserve(); !errors.Is(err, core.ErrInUse) {
				t.Fatalf("terminal session с остаточными процессами разрешила prep: %v", err)
			}
			if err = f.svc.AcknowledgeCleanup(ctx, core.CleanupFenceFor(intents.Items[0])); err != nil {
				t.Fatal(err)
			}
			if err = reserve(); !errors.Is(err, core.ErrInUse) {
				t.Fatalf("один ack снял чужой запрет: %v", err)
			}
			if err = f.svc.AcknowledgeCleanup(ctx, core.CleanupFenceFor(intents.Items[1])); err != nil {
				t.Fatal(err)
			}
			if err = reserve(); err != nil {
				t.Fatalf("оба физических cleanup не освободили prep: %v", err)
			}
		})
	}
}

func TestPreparationRecoveryPreservesCurrentAndQuarantinesIssuedWork(t *testing.T) {
	for _, issued := range []bool{false, true} {
		name := "unissued"
		if issued {
			name = "issued"
		}
		t.Run(name, func(t *testing.T) {
			f := newGenerationAcceptanceFixture(t)
			ctx := context.Background()
			ready, _ := publishGenerationAcceptance(t, f.svc, f.asset.ID)
			pending := reserveGenerationAcceptance(t, f.svc, f.asset.ID)
			fence := core.PreparationFenceFor(pending)
			if issued {
				fence = issueGenerationAcceptance(t, f.svc, pending)
			}
			var path string
			if err := f.store.sqlDB.QueryRowContext(ctx, "PRAGMA database_list").Scan(new(int), new(string), &path); err != nil {
				t.Fatal(err)
			}
			options := f.store.options
			if err := f.store.Close(); err != nil {
				t.Fatal(err)
			}
			reopened, err := Open(ctx, path, options)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				if err := reopened.Close(); err != nil {
					t.Error(err)
				}
			})
			svc, err := core.NewService(reopened, reopened.Incarnation())
			if err != nil {
				t.Fatal(err)
			}
			a, err := svc.GetAsset(ctx, f.asset.ID)
			if err != nil || a.CurrentGenerationID != ready.ID || a.State != core.AssetReady {
				t.Fatalf("recovery потерял current: %+v, %v", a, err)
			}
			g, err := svc.GetGeneration(ctx, pending.ID)
			if err != nil || g.State != core.PreparationInterrupted || g.Issued != issued || g.CleanupPending != issued {
				t.Fatalf("неверное восстановление подготовки: %+v, %v", g, err)
			}
			start, err := svc.StartSession(ctx, f.start("after-recovery", ready))
			if err != nil {
				t.Fatalf("терминальная подготовка прежнего владельца заблокировала start: %v", err)
			}
			executeGenerationCommand(t, svc, start.RequestID)
			if issued {
				wrong := fence
				wrong.Attempt++
				if err = svc.AcknowledgePreparationCleanup(ctx, wrong); !errors.Is(err, core.ErrStale) {
					t.Fatalf("неверный cleanup fence принят: %v", err)
				}
				if err = svc.AcknowledgePreparationCleanup(ctx, fence); err != nil {
					t.Fatal(err)
				}
			}
			if current, err := svc.GetSession(ctx, start.SessionID); err != nil || current.State != core.SessionRunning {
				t.Fatalf("старый cleanup затронул новый запуск: %+v, %v", current, err)
			}
		})
	}
}

func TestStorePreparationLimitsRemainImmutableAndQueueIsGlobal(t *testing.T) {
	f := newGenerationAcceptanceFixture(t)
	ctx := context.Background()
	other, err := core.NewService(f.store, f.store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	limits := f.store.PreparationLimits()
	limits.MaxQueued = 400
	if f.store.PreparationLimits().MaxQueued != 3 {
		t.Fatal("выданное значение изменило политику Store")
	}
	var first core.FallbackGeneration
	for i := 0; i < 4; i++ {
		a, err := f.svc.CreateAsset(ctx, "Элемент очереди")
		if err != nil {
			t.Fatal(err)
		}
		svc := f.svc
		if i%2 == 1 {
			svc = other
		}
		g, err := svc.ReservePreparation(ctx, core.PreparationRequest{AssetID: a.ID, ExpectedRevision: a.Revision, Kind: core.AssetImage, GeneratorVersion: "queue-generator"})
		if i < 3 {
			if err != nil {
				t.Fatal(err)
			}
			if i == 0 {
				first = g
			}
		} else if !errors.Is(err, core.ErrBusy) {
			t.Fatalf("четвёртый accepted элемент обошёл общий лимит: %v", err)
		}
	}
	fence, err := other.ClaimPreparation(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.svc.MarkPreparationIssued(ctx, fence); err != nil {
		t.Fatal(err)
	}
	page, err := f.svc.ListGenerations(ctx, core.PageRequest{Query: core.ListQuery{Limit: 20, PendingOnly: true}})
	if err != nil {
		t.Fatal(err)
	}
	for _, g := range page.Items {
		if g.ID != first.ID {
			if _, err = f.svc.ClaimPreparation(ctx, g.ID); !errors.Is(err, core.ErrBusy) {
				t.Fatalf("второй Service получил параллельный execution slot: %v", err)
			}
		}
	}
}
