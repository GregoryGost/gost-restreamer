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

// generationAcceptanceFixture объединяет временное хранилище и связанную конфигурацию проверки поколений
// заглушки.
type generationAcceptanceFixture struct {
	// store владеет временной файловой SQLite с рабочей схемой Goose.
	store *Store
	// svc предоставляет доменные операции текущего владельца временного Store.
	svc *core.Service
	// asset содержит стабильную заглушку и указатель опубликованного поколения.
	asset core.FallbackAsset
	// stream содержит конфигурацию потока с постоянным ключом и выбранной заглушкой.
	stream core.Stream
	// profile содержит полный именованный профиль тестовой группы.
	profile core.TranscodeProfile
	// group содержит конфигурацию группы тестового потока.
	group core.TranscodeGroup
	// destination содержит назначение единственной группы тестового потока.
	destination core.Destination
}

// Создаёт временную SQLite с общей очередью подготовки на три элемента и связанной конфигурацией одной
// заглушки, группы и назначения.
func newGenerationAcceptanceFixture(t *testing.T) generationAcceptanceFixture {
	t.Helper()
	// o задаёт параметры временного Store с общей очередью из трёх подготовок.
	o := testOptions()
	o.PreparationLimits = core.PreparationLimits{MaxQueued: 3, MaxProfiles: 8}
	// s содержит временное хранилище проверки поколений. err сообщает об ошибке открытия временного
	// хранилища поколений.
	s, err := Open(context.Background(), filepath.Join(t.TempDir(), "generation.sqlite"), o)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает временный Store проверки поколений после завершения теста.

		// err сообщает об ошибке закрытия временного хранилища.
		if err := s.Close(); err != nil {
			t.Error(err)
		}
	})
	// svc предоставляет доменные операции текущего владельца временного Store.
	svc, err := core.NewService(s, s.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	// f объединяет временный Store, Service и связанную конфигурацию проверки поколений заглушки.
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

// Читает текущую ревизию Asset и резервирует новое тестовое поколение с заданным генератором.
func reserveGenerationAcceptance(t *testing.T, svc *core.Service, assetID core.ID) core.FallbackGeneration {
	t.Helper()
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// a содержит стабильную заглушку проверяемого поколения. err сообщает об ошибке чтения заглушки
	// и указателей поколений.
	a, err := svc.GetAsset(ctx, assetID)
	if err != nil {
		t.Fatal(err)
	}
	// g содержит зарезервированное или прочитанное поколение подготовки.
	g, err := svc.ReservePreparation(ctx, core.PreparationRequest{AssetID: a.ID, ExpectedRevision: a.Revision, Kind: core.AssetImage, GeneratorVersion: "acceptance-generator"})
	if err != nil {
		t.Fatal(err)
	}
	return g
}

// Захватывает тестовое поколение и сохраняет выдачу его точной попытки перед моделированием
// подготовки.
func issueGenerationAcceptance(t *testing.T, svc *core.Service, g core.FallbackGeneration) core.PreparationFence {
	t.Helper()
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// f содержит точный допуск захваченной подготовки. err сообщает об ошибке захвата точной
	// подготовки.
	f, err := svc.ClaimPreparation(ctx, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.MarkPreparationIssued(ctx, f); err != nil {
		t.Fatal(err)
	}
	return f
}

// Создаёт согласованный комплект метаданных PNG и всех требуемых вариантов с одинаковыми тестовыми
// хешами для разных поколений.
func generationAcceptancePublication(g core.FallbackGeneration) core.GenerationPublication {
	// p содержит комплект метаданных оригинала и всех требуемых вариантов.
	p := core.GenerationPublication{SourceHash: "same-source-bytes", SizeBytes: 37, SourceFormat: core.SourceFormat{ImageFormat: "png"}}
	// r содержит очередной требуемый профиль поколения при построении тестовой публикации.
	for _, r := range g.RequiredProfiles {
		p.Variants = append(p.Variants, core.FallbackVariant{ID: r.VariantID, AssetID: g.AssetID, GenerationID: g.ID, SourceHash: p.SourceHash, Profile: r.Profile, GeneratorVersion: g.GeneratorVersion, ManagedPath: r.ManagedPath, SizeBytes: 1234, ContentHash: "same-result-bytes", VerifiedFormat: r.Profile.Format})
	}
	return p
}

// Резервирует, выдаёт и публикует полный тестовый комплект, затем подтверждает завершение подготовки
// без создания реальных медиафайлов.
func publishGenerationAcceptance(t *testing.T, svc *core.Service, assetID core.ID) (core.FallbackGeneration, core.GenerationPublication) {
	t.Helper()
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// g содержит зарезервированное или прочитанное поколение подготовки.
	g := reserveGenerationAcceptance(t, svc, assetID)
	// f содержит точный допуск выданной подготовки.
	f := issueGenerationAcceptance(t, svc, g)
	// p содержит комплект метаданных оригинала и всех требуемых вариантов.
	p := generationAcceptancePublication(g)
	// published содержит атомарно опубликованное поколение. err сообщает об ошибке атомарной
	// публикации полного поколения.
	published, err := svc.PublishPreparation(ctx, f, p)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.AcknowledgePreparationCleanup(ctx, f); err != nil {
		t.Fatal(err)
	}
	return published, p
}

// Строит запрос запуска с точной привязкой первого требуемого варианта указанного поколения.
func (f generationAcceptanceFixture) start(id string, g core.FallbackGeneration) core.StartSessionRequest {
	return core.StartSessionRequest{RequestID: id, StreamID: f.stream.ID, Bindings: []core.PreparedBinding{{GroupID: f.group.ID, GenerationID: g.ID, VariantID: g.RequiredProfiles[0].VariantID, GeneratorVersion: g.GeneratorVersion}}}
}

// Проверяет сохранение прежнего комплекта при ошибке, новые идентичности даже для одинакового
// содержимого и изоляцию другой заглушки при замене.
func TestGenerationReplacementIsAtomicFreshAndAssetScoped(t *testing.T) {
	// f объединяет временный Store, Service и связанную конфигурацию проверки поколений заглушки.
	f := newGenerationAcceptanceFixture(t)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// first сохраняет первое опубликованное поколение для проверки смены идентичности.
	first, _ := publishGenerationAcceptance(t, f.svc, f.asset.ID)
	// other содержит другую заглушку, которую замена выбранной не должна затронуть. err сообщает об
	// ошибке создания стабильной заглушки.
	other, err := f.svc.CreateAsset(ctx, "Другая заглушка")
	if err != nil {
		t.Fatal(err)
	}
	publishGenerationAcceptance(t, f.svc, other.ID)
	other, err = f.svc.GetAsset(ctx, other.ID)
	if err != nil {
		t.Fatal(err)
	}
	// g содержит зарезервированное или прочитанное поколение подготовки.
	g := reserveGenerationAcceptance(t, f.svc, f.asset.ID)
	// fence идентифицирует точную захваченную попытку проверяемой команды.
	fence := issueGenerationAcceptance(t, f.svc, g)
	// bad содержит неполный комплект без требуемых вариантов.
	bad := generationAcceptancePublication(g)
	bad.Variants = nil
	_, err = f.svc.PublishPreparation(ctx, fence, bad)
	if !errors.Is(err, core.ErrConflict) {
		t.Fatalf("неполный комплект: %v", err)
	}
	// a содержит стабильную заглушку проверяемого поколения.
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
	// second содержит следующее опубликованное поколение при тех же хешах содержимого.
	second, _ := publishGenerationAcceptance(t, f.svc, f.asset.ID)
	if second.ID == first.ID || second.OriginalPath == first.OriginalPath || second.RequiredProfiles[0].VariantID == first.RequiredProfiles[0].VariantID || second.RequiredProfiles[0].ManagedPath == first.RequiredProfiles[0].ManagedPath {
		t.Fatal("равное содержимое переиспользовало поколение или файл")
	}
	a, err = f.svc.GetAsset(ctx, f.asset.ID)
	if err != nil || a.CurrentGenerationID != second.ID || a.PendingGenerationID != "" {
		t.Fatalf("новый комплект не стал current: %+v, %v", a, err)
	}
	// got содержит прочитанное сохраняемое значение для проверки результата операции.
	got, err := f.svc.GetAsset(ctx, other.ID)
	if err != nil || !reflect.DeepEqual(got, other) {
		t.Fatalf("замена изменила другую заглушку: %+v, %v", got, err)
	}
	// old сохраняет прежнее опубликованное поколение.
	old, err := f.svc.GetGeneration(ctx, first.ID)
	if err != nil || old.State != core.PreparationPublished || old.SourceHash != second.SourceHash {
		t.Fatalf("прежний комплект потерян: %+v, %v", old, err)
	}
}

// Проверяет конфликт изменения полного требуемого профиля во время подготовки и допустимость
// переименования или изменения несвязанного профиля.
func TestGenerationPublicationDetectsProfileChangesButNotNames(t *testing.T) {
	// change выбирает переименование, изменение полного профиля либо несвязанный профиль.
	for _, change := range []string{"name", "profile", "unrelated"} {
		t.Run(change, func(t *testing.T) {
			// Проверяет отказ публикации при изменении полного требуемого профиля и допустимость публикации после
			// переименования либо создания несвязанного профиля.

			// f объединяет временный Store, Service и связанную конфигурацию проверки
			// поколений заглушки.
			f := newGenerationAcceptanceFixture(t)
			// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
			ctx := context.Background()
			// old сохраняет прежнее опубликованное поколение.
			old, _ := publishGenerationAcceptance(t, f.svc, f.asset.ID)
			// g содержит зарезервированное или прочитанное поколение подготовки.
			g := reserveGenerationAcceptance(t, f.svc, f.asset.ID)
			// fence идентифицирует точную захваченную попытку проверяемой команды.
			fence := issueGenerationAcceptance(t, f.svc, g)
			// p содержит изменяемый полный профиль либо новое имя требуемого профиля.
			p := f.profile
			if change == "profile" {
				p.Profile.VideoEncoding.GOPFrames++
			} else {
				p.Name = "Другое имя"
			}
			if change == "unrelated" {
				// err сообщает об ошибке сохранения полного именованного профиля.
				_, err := f.svc.CreateProfile(ctx, core.TranscodeProfile{Name: "Несвязанный", Profile: storageProfile()})
				if err != nil {
					t.Fatal(err)
				}
			} else /* err сообщает об ошибке условного изменения полного профиля. */ if _, err := f.svc.UpdateProfile(ctx, p, p.Revision); err != nil {
				t.Fatal(err)
			}
			// err сообщает об ошибке атомарной публикации полного поколения.
			_, err := f.svc.PublishPreparation(ctx, fence, generationAcceptancePublication(g))
			if change == "profile" {
				if !errors.Is(err, core.ErrConflict) {
					t.Fatalf("изменение полного профиля: %v", err)
				}
				// a содержит стабильную заглушку проверяемого поколения. e сообщает об
				// ошибке чтения заглушки и указателей поколений.
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

// Проверяет общий запрет запуска активной подготовкой и сохранение очистки до окончания
// зарегистрированной операции, разрешая ручной старт после терминального отзыва.
func TestPreparationAdmissionIsSharedAndPhysicalCompletionIsRequired(t *testing.T) {
	// f объединяет временный Store, Service и связанную конфигурацию проверки поколений заглушки.
	f := newGenerationAcceptanceFixture(t)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// ready содержит рабочее опубликованное поколение для ручного запуска.
	ready, _ := publishGenerationAcceptance(t, f.svc, f.asset.ID)
	// otherService содержит второй Service с общими очередью и допуском Store. err сообщает об
	// ошибке создания доменного Service для текущего владельца Store.
	otherService, err := core.NewService(f.store, f.store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	// g содержит зарезервированное или прочитанное поколение подготовки.
	g := reserveGenerationAcceptance(t, f.svc, f.asset.ID)
	if _, err = otherService.StartSession(ctx, f.start("blocked-by-accepted", ready)); !errors.Is(err, core.ErrNotReady) {
		t.Fatalf("очередь не исключила start другого Service: %v", err)
	}
	// fence идентифицирует точную захваченную попытку проверяемой команды.
	fence := issueGenerationAcceptance(t, otherService, g)
	// owner содержит владельца выданной подготовки с незавершённой операцией.
	owner, err := otherService.BindPreparationOwner(ctx, fence)
	if err != nil {
		t.Fatal(err)
	}
	// finish завершает регистрацию операции подготовки для разрешения подтверждения очистки.
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
	// start содержит принятую квитанцию первоначального запуска сессии.
	start, err := f.svc.StartSession(ctx, f.start("manual-before-preparation-join", ready))
	if err != nil {
		t.Fatalf("терминальная подготовка заблокировала ручной start: %v", err)
	}
	executeGenerationCommand(t, f.svc, start.RequestID)
	// before сохраняет прерванную подготовку после нового запуска для проверки её обязательств.
	before, err := f.svc.GetGeneration(ctx, g.ID)
	if err != nil || !before.Issued || !before.CleanupPending {
		t.Fatalf("новый start потерял прежние обязательства: %+v, %v", before, err)
	}
	finish()
	if err = f.svc.AcknowledgePreparationCleanup(ctx, fence); err != nil {
		t.Fatal(err)
	}
	// current содержит новый запуск после подтверждения очистки старой подготовки. err сообщает об
	// ошибке чтения нового ручного запуска.
	if current, err := f.svc.GetSession(ctx, start.SessionID); err != nil || current.State != core.SessionRunning {
		t.Fatalf("старый cleanup затронул новый запуск: %+v, %v", current, err)
	}
}

// Проверяет единственного победителя гонки резервирования подготовки и запуска Session через разные
// Service.
func TestPreparationAndSessionAdmissionCannotBothWin(t *testing.T) {
	// f объединяет временный Store, Service и связанную конфигурацию проверки поколений заглушки.
	f := newGenerationAcceptanceFixture(t)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// ready содержит рабочее опубликованное поколение для ручного запуска.
	ready, _ := publishGenerationAcceptance(t, f.svc, f.asset.ID)
	// a содержит стабильную заглушку проверяемого поколения. err сообщает об ошибке чтения заглушки
	// и указателей поколений.
	a, err := f.svc.GetAsset(ctx, f.asset.ID)
	if err != nil {
		t.Fatal(err)
	}
	// other содержит второй Service, конкурирующий за общий допуск Store.
	other, err := core.NewService(f.store, f.store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	// begin одновременно разрешает резервирование подготовки и запуск Session.
	begin := make(chan struct{})
	// wg ожидает завершение обоих конкурирующих запросов.
	var wg sync.WaitGroup
	wg.Add(2)
	// prepErr содержит результат конкурирующего резервирования подготовки. startErr содержит
	// результат конкурирующего ручного запуска.
	var prepErr, startErr error
	go func() {
		// После общего сигнала резервирует подготовку, конкурируя с запуском Session другого Service.

		defer wg.Done()
		<-begin
		_, prepErr = f.svc.ReservePreparation(ctx, core.PreparationRequest{AssetID: a.ID, ExpectedRevision: a.Revision, Kind: core.AssetImage, GeneratorVersion: "concurrent-generator"})
	}()
	go func() {
		// После общего сигнала запускает Session через другой Service, конкурируя с резервированием
		// подготовки.

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

// Проверяет отказ нового запуска со старым готовым поколением и неизменность прежней квитанции после
// замены комплекта.
func TestFreshBindingRejectsOldGenerationAndReceiptSurvivesReplacement(t *testing.T) {
	// f объединяет временный Store, Service и связанную конфигурацию проверки поколений заглушки.
	f := newGenerationAcceptanceFixture(t)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// first сохраняет первое опубликованное поколение для проверки смены идентичности.
	first, _ := publishGenerationAcceptance(t, f.svc, f.asset.ID)
	// request сохраняет исходный запрос запуска для проверки повтора после замены комплекта.
	request := f.start("accepted-before-replacement", first)
	// receipt содержит сохраняемую квитанцию выбранной команды. err сообщает об ошибке принятия
	// ручного запуска потока.
	receipt, err := f.svc.StartSession(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	// fence идентифицирует точную захваченную попытку проверяемой команды.
	fence, err := f.svc.Claim(ctx, request.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.svc.Complete(ctx, fence, core.Completion{Failure: "Ресурсы не запускались"}); err != nil {
		t.Fatal(err)
	}
	// second содержит следующее опубликованное поколение при тех же хешах содержимого.
	second, _ := publishGenerationAcceptance(t, f.svc, f.asset.ID)
	if _, err = f.svc.StartSession(ctx, f.start("fresh-with-stale-binding", first)); !errors.Is(err, core.ErrNotReady) {
		t.Fatalf("новый start принял старое READY поколение: %v", err)
	}
	// stored сохраняет исходную квитанцию принятого запуска.
	stored, err := f.svc.GetReceipt(ctx, receipt.ID)
	if err != nil {
		t.Fatal(err)
	}
	request.Bindings = f.start("", second).Bindings
	// replay содержит повторно возвращённую квитанцию при изменённых Bindings.
	replay, err := f.svc.StartSession(ctx, request)
	if err != nil || !reflect.DeepEqual(replay, stored) {
		t.Fatalf("повтор принятого RequestID изменил receipt: %+v, %v", replay, err)
	}
	if _, err = f.svc.StartSession(ctx, f.start("fresh-with-current", second)); err != nil {
		t.Fatalf("точный current отклонён: %v", err)
	}
}

// Проводит тестовую команду через захват, выдачу и успешное завершение протокола управления.
func executeGenerationCommand(t *testing.T, svc *core.Service, id string) core.ExecutionFence {
	t.Helper()
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// f содержит точную захваченную попытку исполняемой команды. err сообщает об ошибке захвата
	// точной команды исполнения.
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

// Проверяет запрет подготовки при ручной паузе и до подтверждения обеих независимых очисток
// Session/Output в любом порядке.
func TestPreparationRemainsExcludedDuringPauseAndBothCleanupOrders(t *testing.T) {
	// parentFirst выбирает порядок подтверждения очистки сессии и выхода.
	for _, parentFirst := range []bool{true, false} {
		// name задаёт имя сценария выдачи или порядка очистки.
		name := "output-first"
		if parentFirst {
			name = "parent-first"
		}
		t.Run(name, func(t *testing.T) {
			// Проверяет запрет подготовки во время ручной паузы и до обоих подтверждений очистки родителя и выхода
			// при выбранном порядке.

			// f объединяет временный Store, Service и связанную конфигурацию проверки
			// поколений заглушки.
			f := newGenerationAcceptanceFixture(t)
			// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
			ctx := context.Background()
			// ready содержит рабочее опубликованное поколение для ручного запуска.
			ready, _ := publishGenerationAcceptance(t, f.svc, f.asset.ID)
			// start содержит принятую квитанцию первоначального запуска сессии. err сообщает
			// об ошибке принятия ручного запуска потока.
			start, err := f.svc.StartSession(ctx, f.start("issued-session", ready))
			if err != nil {
				t.Fatal(err)
			}
			executeGenerationCommand(t, f.svc, start.RequestID)
			// commands содержит квитанции сессии для исполнения команд запуска выходов.
			commands, err := f.svc.ListReceipts(ctx, core.PageRequest{Query: core.ListQuery{Limit: 50, SessionID: start.SessionID}})
			if err != nil {
				t.Fatal(err)
			}
			// c содержит сохраняемую квитанцию выбранной команды.
			for _, c := range commands.Items {
				if c.Kind == core.CommandStartOutput {
					executeGenerationCommand(t, f.svc, c.RequestID)
				}
			}
			// pause содержит принятую квитанцию включения ручной паузы.
			pause, err := f.svc.PauseSession(ctx, core.SessionCommand{RequestID: "pause-running-session", SessionID: start.SessionID})
			if err != nil {
				t.Fatal(err)
			}
			executeGenerationCommand(t, f.svc, pause.RequestID)
			// other содержит отдельную заглушку для проверки общего запрета подготовки.
			other, err := f.svc.CreateAsset(ctx, "Заглушка другого потока")
			if err != nil {
				t.Fatal(err)
			}
			// reserve пытается резервировать подготовку другой заглушки при паузе и
			// остаточной очистке.
			reserve := func() error {
				// Пробует зарезервировать подготовку другой заглушки для проверки общего запрета при эфире и
				// остаточной очистке.

				// e сообщает об ошибке резервирования полного поколения подготовки.
				_, e := f.svc.ReservePreparation(ctx, core.PreparationRequest{AssetID: other.ID, ExpectedRevision: other.Revision, Kind: core.AssetImage, GeneratorVersion: "new-generator"})
				return e
			}
			if err = reserve(); !errors.Is(err, core.ErrInUse) {
				t.Fatalf("пауза разрешила подготовку другого asset: %v", err)
			}
			// stop содержит принятую квитанцию остановки сессии.
			stop, err := f.svc.StopSession(ctx, core.SessionCommand{RequestID: "stop-issued-session", SessionID: start.SessionID})
			if err != nil {
				t.Fatal(err)
			}
			executeGenerationCommand(t, f.svc, stop.RequestID)
			// intents содержит независимые намерения очистки сессии и выхода.
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

// Проверяет сохранение текущего комплекта и обязательств выданной подготовки после смены incarnation,
// точный прежний PreparationFence очистки и изоляцию нового ручного запуска.
func TestPreparationRecoveryPreservesCurrentAndQuarantinesIssuedWork(t *testing.T) {
	// issued выбирает выдачу подготовки перед сменой открытия хранилища.
	for _, issued := range []bool{false, true} {
		// name задаёт имя сценария выдачи или порядка очистки.
		name := "unissued"
		if issued {
			name = "issued"
		}
		t.Run(name, func(t *testing.T) {
			// Проверяет восстановление невыданной либо выданной подготовки: прежнее текущее поколение сохраняется,
			// а выданные остатки требуют точного подтверждения без запрета нового ручного запуска.

			// f объединяет временный Store, Service и связанную конфигурацию проверки
			// поколений заглушки.
			f := newGenerationAcceptanceFixture(t)
			// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
			ctx := context.Background()
			// ready содержит рабочее опубликованное поколение для ручного запуска.
			ready, _ := publishGenerationAcceptance(t, f.svc, f.asset.ID)
			// pending сохраняет подготовку перед восстановлением при повторном открытии.
			pending := reserveGenerationAcceptance(t, f.svc, f.asset.ID)
			// fence идентифицирует точную захваченную попытку проверяемой команды.
			fence := core.PreparationFenceFor(pending)
			if issued {
				fence = issueGenerationAcceptance(t, f.svc, pending)
			}
			// path принимает путь файла SQLite для повторного открытия.
			var path string
			// err сообщает об ошибке чтения пути текущей временной базы.
			if err := f.store.sqlDB.QueryRowContext(ctx, "PRAGMA database_list").Scan(new(int), new(string), &path); err != nil {
				t.Fatal(err)
			}
			// options сохраняет параметры Store для повторного открытия той же базы.
			options := f.store.options
			// err сообщает об ошибке закрытия временного хранилища.
			if err := f.store.Close(); err != nil {
				t.Fatal(err)
			}
			// reopened содержит новое открытие временной базы с новой incarnation. err
			// сообщает об ошибке открытия временного хранилища поколений.
			reopened, err := Open(ctx, path, options)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				// Закрывает повторно открытый Store после проверки восстановления подготовки.

				// err сообщает об ошибке закрытия временного хранилища.
				if err := reopened.Close(); err != nil {
					t.Error(err)
				}
			})
			// svc предоставляет доменные операции текущего владельца временного Store.
			svc, err := core.NewService(reopened, reopened.Incarnation())
			if err != nil {
				t.Fatal(err)
			}
			// a содержит стабильную заглушку проверяемого поколения.
			a, err := svc.GetAsset(ctx, f.asset.ID)
			if err != nil || a.CurrentGenerationID != ready.ID || a.State != core.AssetReady {
				t.Fatalf("recovery потерял current: %+v, %v", a, err)
			}
			// g содержит зарезервированное или прочитанное поколение подготовки.
			g, err := svc.GetGeneration(ctx, pending.ID)
			if err != nil || g.State != core.PreparationInterrupted || g.Issued != issued || g.CleanupPending != issued {
				t.Fatalf("неверное восстановление подготовки: %+v, %v", g, err)
			}
			// start содержит принятую квитанцию первоначального запуска сессии.
			start, err := svc.StartSession(ctx, f.start("after-recovery", ready))
			if err != nil {
				t.Fatalf("терминальная подготовка прежнего владельца заблокировала start: %v", err)
			}
			executeGenerationCommand(t, svc, start.RequestID)
			if issued {
				// wrong содержит копию точной идентичности с намеренно подменённым
				// полем.
				wrong := fence
				wrong.Attempt++
				if err = svc.AcknowledgePreparationCleanup(ctx, wrong); !errors.Is(err, core.ErrStale) {
					t.Fatalf("неверный cleanup fence принят: %v", err)
				}
				if err = svc.AcknowledgePreparationCleanup(ctx, fence); err != nil {
					t.Fatal(err)
				}
			}
			// current содержит новый запуск после подтверждения очистки старой подготовки.
			// err сообщает об ошибке чтения нового ручного запуска.
			if current, err := svc.GetSession(ctx, start.SessionID); err != nil || current.State != core.SessionRunning {
				t.Fatalf("старый cleanup затронул новый запуск: %+v, %v", current, err)
			}
		})
	}
}

// Проверяет независимость копии PreparationLimits и общий бюджет очереди и единственного исполнения
// подготовки между двумя Service.
func TestStorePreparationLimitsRemainImmutableAndQueueIsGlobal(t *testing.T) {
	// f объединяет временный Store, Service и связанную конфигурацию проверки поколений заглушки.
	f := newGenerationAcceptanceFixture(t)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// other содержит второй Service с общей очередью и единственным местом исполнения подготовки.
	// err сообщает об ошибке создания доменного Service для текущего владельца Store.
	other, err := core.NewService(f.store, f.store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	// limits содержит независимую копию ограничений подготовки Store.
	limits := f.store.PreparationLimits()
	limits.MaxQueued = 400
	if f.store.PreparationLimits().MaxQueued != 3 {
		t.Fatal("выданное значение изменило политику Store")
	}
	// first сохраняет первый принятый элемент очереди для выдачи единственного места исполнения
	// подготовки.
	var first core.FallbackGeneration
	// i указывает номер принимаемого элемента общей очереди.
	for i := range 4 {
		// a содержит стабильную заглушку проверяемого поколения. err сообщает об ошибке создания
		// стабильной заглушки.
		a, err := f.svc.CreateAsset(ctx, "Элемент очереди")
		if err != nil {
			t.Fatal(err)
		}
		// svc предоставляет доменные операции текущего владельца временного Store.
		svc := f.svc
		if i%2 == 1 {
			svc = other
		}
		// g содержит зарезервированное или прочитанное поколение подготовки.
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
	// fence идентифицирует точную захваченную попытку проверяемой команды.
	fence, err := other.ClaimPreparation(ctx, first.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.svc.MarkPreparationIssued(ctx, fence); err != nil {
		t.Fatal(err)
	}
	// page содержит общую очередь подготовок для проверки единственного места исполнения подготовки.
	page, err := f.svc.ListGenerations(ctx, core.PageRequest{Query: core.ListQuery{Limit: 20, PendingOnly: true}})
	if err != nil {
		t.Fatal(err)
	}
	// g содержит зарезервированное или прочитанное поколение подготовки.
	for _, g := range page.Items {
		if g.ID != first.ID {
			if _, err = f.svc.ClaimPreparation(ctx, g.ID); !errors.Is(err, core.ErrBusy) {
				t.Fatalf("второй Service получил параллельный execution slot: %v", err)
			}
		}
	}
}
