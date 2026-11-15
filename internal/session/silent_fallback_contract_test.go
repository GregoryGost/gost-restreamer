package session

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"io"
	"net"
	"os"
	"reflect"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
)

// silentContractGenerator задаёт версию беззвучной заглушки, требуемую менеджером в контрактных
// проверках.
const silentContractGenerator = "fallback-fmp4-silent-v2"

// silentContractLegacyVariant воспроизводит историю прежнего генератора в изолированной SQLite f.
// Обычные репозитории не предоставляют изменения происхождения готового файла, поэтому тест пишет
// согласованные payload и SQL-проекцию напрямую; работающий Manager ещё не получил команд.
func silentContractLegacyVariant(t *testing.T, f *managerFixture) {
	t.Helper()
	// ctx ограничивает чтение исходных записей временем жизни теста.
	ctx := context.Background()
	// generation содержит историческое поколение; err сообщает отказ чтения и сохранения тестовой истории.
	generation, err := f.service.GetGeneration(ctx, f.binding.GenerationID)
	if err != nil {
		t.Fatal(err)
	}
	// variant содержит точный вариант сохраняемого поколения.
	variant, err := f.service.GetVariant(ctx, f.binding.VariantID)
	if err != nil {
		t.Fatal(err)
	}
	generation.GeneratorVersion, variant.GeneratorVersion = "legacy-generator", "legacy-generator"
	f.binding.GeneratorVersion = variant.GeneratorVersion
	// db открывает только файл временной базы f для построения прежнего состояния.
	db, err := sql.Open("sqlite3", f.databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// err сообщает отказ закрытия вспомогательного подключения теста.
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	// generationData содержит строгие исторические метаданные поколения.
	generationData, err := core.EncodeMetadata(generation)
	if err != nil {
		t.Fatal(err)
	}
	// variantData содержит строгие исторические метаданные варианта.
	variantData, err := core.EncodeMetadata(variant)
	if err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, "UPDATE fallback_generations SET payload=? WHERE id=?", generationData, generation.ID); err != nil {
		t.Fatal(err)
	}
	if _, err = db.ExecContext(ctx, "UPDATE fallback_variants SET generator_version=?, payload=? WHERE id=?", variant.GeneratorVersion, variantData, variant.ID); err != nil {
		t.Fatal(err)
	}
}

// silentContractLegacyOutputs воспроизводит принятые прежним бинарником снимки выходов Session.
// f владеет временной SQLite; session выбирает записи. Вызывающий удерживает manager.mu до завершения,
// чтобы диспетчер не начал команду между созданием снимка и записью исторического payload.
func silentContractLegacyOutputs(t *testing.T, f managerFixture, session core.ID) {
	t.Helper()
	// ctx задаёт контекст создания контролируемой истории.
	ctx := context.Background()
	// outputs содержит ещё не исполняемые выходы выбранного запуска; err сообщает отказ операций.
	outputs, err := f.service.ListOutputs(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8, SessionID: session}})
	if err != nil {
		t.Fatal(err)
	}
	// db открывает только изолированный файл тестовой базы для записи исторического снимка.
	db, err := sql.Open("sqlite3", f.databasePath)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// err сообщает отказ закрытия дополнительного подключения теста.
		if err := db.Close(); err != nil {
			t.Error(err)
		}
	}()
	// output содержит очередной неизменяемый снимок, созданный до обновления генератора.
	for _, output := range outputs.Items {
		output.Snapshot.Artifact.GeneratorVersion = "legacy-generator"
		// payload содержит полную историческую запись с неизменными командами, ID и остальными полями;
		// err сообщает об ошибке кодирования этой самостоятельной записи.
		payload, err := core.EncodeMetadata(output)
		if err != nil {
			t.Fatal(err)
		}
		if _, err = db.ExecContext(ctx, "UPDATE outputs SET payload=? WHERE id=?", payload, output.ID); err != nil {
			t.Fatal(err)
		}
	}
}

// silentContractNoNetwork заменяет сетевое подключение счётчиком попыток и отказом, чтобы тест
// обнаруживал любой выход к сети.
func silentContractNoNetwork(c *Config, attempts *atomic.Uint64) {
	c.Publisher.Transport.DialContext = func(context.Context, string, string) (net.Conn, error) {
		// Учитывает неожиданное сетевое подключение и возвращает отказ без создания соединения.

		attempts.Add(1)
		return nil, errors.New("неожиданная сетевая попытка")
	}
}

// TestSilentFallbackRejectsLegacyBindingsWithoutAdmission проверяет отказ старой версии генератора до
// квитанции, выхода и внешнего исполнения при сохранении читаемой истории варианта и поколения.
func TestSilentFallbackRejectsLegacyBindingsWithoutAdmission(t *testing.T) {
	// network считает попытки сетевого подключения; players считает попытки построения проигрывателя
	// при отклоняемом старом варианте.
	var network, players atomic.Uint64
	// f предоставляет старое поколение заглушки при требовании текущего беззвучного генератора.
	f := newManagerFixture(t, false, func(c *Config) {
		// Задаёт текущую версию беззвучного генератора и счётчики запрещённых внешних действий для проверки
		// старых привязок.

		c.GeneratorVersion = silentContractGenerator
		silentContractNoNetwork(c, &network)
		c.Group.Player = func(context.Context, string, *os.File, ffmpeg.Attempt) (ffmpeg.Command, error) {
			// Учитывает попытку построения проигрывателя и возвращает ErrInvalid, чтобы она не создала процесс.

			players.Add(1)
			return ffmpeg.Command{}, core.ErrInvalid
		}
	})
	silentContractLegacyVariant(t, &f)
	// ctx задаёт контекст проверки допуска старых привязок.
	ctx := context.Background()
	// variant содержит исходный исторический вариант; err сообщает отказ его чтения до отклонения
	// запуска.
	variant, err := f.service.GetVariant(ctx, f.binding.VariantID)
	if err != nil {
		t.Fatal(err)
	}
	// generation содержит исходное историческое поколение для проверки неизменности после отказов
	// допуска.
	generation, err := f.service.GetGeneration(ctx, f.binding.GenerationID)
	if err != nil {
		t.Fatal(err)
	}
	// request задаёт новый запуск с явно выбранным назначением старого поколения.
	request := core.StartSessionRequest{RequestID: "legacy-binding-start", StreamID: f.stream.ID, DestinationIDs: []core.ID{f.destination.ID}}
	// receipt содержит ответ отклоняемого запуска; err проверяется на непригодность версии
	// генератора.
	if receipt, err := f.manager.StartSession(ctx, request); !errors.Is(err, core.ErrNotReady) || receipt.ID != "" {
		t.Fatal("старый генератор допущен в новую Session", receipt, err)
	}
	// found отмечает наличие квитанции отклонённого запуска; err сообщает отказ проверки прежнего
	// запроса.
	if _, found, err := f.service.PriorStartSession(ctx, request); err != nil || found {
		t.Fatal("отказ оставил квитанцию запуска", found, err)
	}
	// parent содержит успешный запуск без назначений для проверки отдельного добавления старой
	// заглушки.
	parent := startAdmissionParent(t, f)
	// add задаёт добавление назначения с непригодной версией генератора к работающему родителю.
	add := core.AddOutputRequest{RequestID: "legacy-binding-add", SessionID: parent.SessionID, DestinationID: f.destination.ID}
	// receipt содержит ответ отклоняемого добавления; err проверяется на непригодность версии
	// генератора.
	if receipt, err := f.manager.AddOutput(ctx, add); !errors.Is(err, core.ErrNotReady) || receipt.ID != "" {
		t.Fatal("старый генератор допущен в новый выход", receipt, err)
	}
	// found отмечает наличие квитанции отклонённого добавления; err сообщает отказ поиска прежней
	// команды.
	if _, found, err := f.service.PriorAddOutput(ctx, add); err != nil || found {
		t.Fatal("отказ оставил квитанцию выхода", found, err)
	}
	// outputs содержит назначения родителя после отклонения старого варианта.
	outputs, err := f.service.ListOutputs(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8, SessionID: parent.SessionID}})
	if err != nil || len(outputs.Items) != 0 {
		t.Fatal("отказ создал выход", outputs, err)
	}
	// afterVariant содержит исторический вариант после отказов для сравнения с исходным состоянием.
	afterVariant, err := f.service.GetVariant(ctx, variant.ID)
	if err != nil || !reflect.DeepEqual(afterVariant, variant) || afterVariant.State != core.VariantReady {
		t.Fatal("отказ изменил исторический вариант", afterVariant, err)
	}
	// afterGeneration содержит историческое поколение после отказов для проверки его неизменности.
	afterGeneration, err := f.service.GetGeneration(ctx, generation.ID)
	if err != nil || !reflect.DeepEqual(afterGeneration, generation) {
		t.Fatal("отказ изменил историческое поколение", afterGeneration, err)
	}
	if network.Load() != 0 || players.Load() != 0 {
		t.Fatal("отказ допуска начал внешнее выполнение", network.Load(), players.Load())
	}
}

// silentContractSnapshotBytes кодирует снимок выхода в сохраняемое представление для побайтовой
// проверки его неизменности.
func silentContractSnapshotBytes(t *testing.T, snapshot core.OutputSnapshot) []byte {
	t.Helper()
	// encoded содержит сохраняемые байты снимка назначения; err сообщает отказ кодирования снимка.
	encoded, err := core.EncodeOutputSnapshot(snapshot)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

// TestSilentFallbackAcceptedLegacySessionFailsBeforeFeed проверяет отказ ранее принятого запуска со
// старой заглушкой до Feed и внешнего исполнения с сохранением квитанции при повторе.
func TestSilentFallbackAcceptedLegacySessionFailsBeforeFeed(t *testing.T) {
	// network считает сетевые подключения; players считает построения проигрывателя ранее принятого
	// запуска со старой заглушкой.
	var network, players atomic.Uint64
	// f предоставляет включённое старое назначение при требовании текущего генератора менеджером.
	f := newManagerFixture(t, true, func(c *Config) {
		// Задаёт текущую версию генератора и считает обращения к проигрывателю и сети для ранее принятой
		// сессии.

		c.GeneratorVersion = silentContractGenerator
		silentContractNoNetwork(c, &network)
		// original сохраняет исходный построитель проигрывателя под счётчиком вызовов.
		original := c.Group.Player
		c.Group.Player = func(ctx context.Context, binary string, file *os.File, attempt ffmpeg.Attempt) (ffmpeg.Command, error) {
			// Учитывает вызов проигрывателя и делегирует построение исходной команде, если проверка версии его всё
			// же допустила.

			players.Add(1)
			return original(ctx, binary, file, attempt)
		}
	})
	// ctx задаёт контекст сохранения и проверки ранее принятого старого запуска.
	ctx := context.Background()
	// request изображает запуск, принятый сервисом до проверки новой версии генератора менеджером.
	request := core.StartSessionRequest{RequestID: "accepted-legacy-session", StreamID: f.stream.ID, Bindings: []core.PreparedBinding{f.binding}}
	// accepted содержит сохранённую команду старого запуска; err сообщает отказ её приёма сервисом.
	f.manager.mu.Lock()
	accepted, err := f.service.StartSession(ctx, request)
	if err == nil {
		silentContractLegacyOutputs(t, f, accepted.SessionID)
	}
	f.manager.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	// failed содержит терминальный отказ исполнения сохранённого запуска со старой заглушкой.
	failed := waitReceipt(t, f, accepted)
	if failed.Status != core.CommandFailed || !strings.Contains(failed.Failure, core.ErrNotReady.Error()) {
		t.Fatal("прежняя команда запустила Session со старой заглушкой", failed)
	}
	// session содержит отказавшую сессию для проверки читаемости исторического снимка.
	session, err := f.service.GetSession(ctx, accepted.SessionID)
	if err != nil || session.State != core.SessionFailed || session.Snapshot.Validate() != nil {
		t.Fatal("история отказавшей Session не читается", session, err)
	}
	// feed содержит ответ доступа к медиавходу отказавшей сессии; err сообщает причину отказа Feed.
	if feed, err := f.manager.Feed(session); err == nil || feed != nil {
		t.Fatal("старый fallback допустил Feed", feed, err)
	}
	// duplicate содержит ответ повторного запроса старого запуска для проверки сохранности прежнего
	// отказа.
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

// TestSilentFallbackAcceptedLegacyOutputCannotStartOrRestart проверяет отказ принятого выхода со
// старой заглушкой и его перезапуска без сети и подмены снимка; исторический файл остаётся читаемым.
func TestSilentFallbackAcceptedLegacyOutputCannotStartOrRestart(t *testing.T) {
	// network считает запрещённые сетевые попытки старого назначения и его перезапуска.
	var network atomic.Uint64
	// f предоставляет работающего родителя при несовпадении старой заглушки с требуемым генератором.
	f := newManagerFixture(t, false, func(c *Config) {
		// Задаёт текущую версию генератора и запрещает сеть для проверки старого снимка выхода и его
		// перезапуска.

		c.GeneratorVersion = silentContractGenerator
		silentContractNoNetwork(c, &network)
	})
	// ctx задаёт контекст проверки старого снимка назначения.
	ctx := context.Background()
	// parent содержит успешный запуск родительской сессии без назначений.
	parent := startAdmissionParent(t, f)
	// request изображает добавление со старой привязкой, принятое до смены версии генератора.
	request := core.AddOutputRequest{RequestID: "accepted-legacy-output", SessionID: parent.SessionID, DestinationID: f.destination.ID, Binding: f.binding}
	// Сохранённая команда могла быть принята до смены серверной версии генератора.
	// accepted содержит сохранённую команду добавления старого назначения; err сообщает отказ её
	// приёма сервисом.
	f.manager.mu.Lock()
	accepted, err := f.service.AddOutput(ctx, request)
	if err == nil {
		silentContractLegacyOutputs(t, f, accepted.SessionID)
	}
	f.manager.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	// before содержит исходный снимок и поколение принятого старого назначения.
	before, err := f.service.GetOutput(ctx, accepted.OutputID)
	if err != nil {
		t.Fatal(err)
	}
	// original сохраняет байты исходного снимка для проверки неизменности после отказа и
	// перезапуска.
	original := silentContractSnapshotBytes(t, before.Snapshot)
	// failed содержит отказ исполнения добавления с непригодной версией заглушки.
	failed := waitReceipt(t, f, accepted)
	if failed.Status != core.CommandFailed || !strings.Contains(failed.Failure, core.ErrNotReady.Error()) {
		t.Fatal("сохранённая команда использовала старую заглушку", failed)
	}
	// duplicate содержит повторный ответ добавления после проверки несовместимого генератора.
	duplicate, err := f.manager.AddOutput(ctx, request)
	if err != nil || !reflect.DeepEqual(duplicate, failed) {
		t.Fatal("версия генератора переписала прежнюю квитанцию", duplicate, failed, err)
	}
	// joined ограничивает ожидание очистки отказавшего назначения восемью секундами; stop
	// освобождает этот срок.
	joined, stop := context.WithTimeout(ctx, 8*time.Second)
	defer stop()
	// ticker задаёт период проверки завершения очистки отказавшего назначения.
	ticker := time.NewTicker(5 * time.Millisecond)
	defer ticker.Stop()
	for {
		// output содержит текущее состояние очистки; err сообщает отказ чтения назначения.
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
	// beforeRestart фиксирует поколение после штатной компенсации отказа до отклоняемого перезапуска.
	beforeRestart, err := f.service.GetOutput(ctx, accepted.OutputID)
	if err != nil {
		t.Fatal(err)
	}
	// restart содержит ответ отказа до приёма новой команды по старому снимку.
	restart, err := f.manager.StartOutput(ctx, core.OutputCommand{RequestID: "restart-legacy-output", OutputID: accepted.OutputID})
	if !errors.Is(err, core.ErrNotReady) || restart.ID != "" {
		t.Fatal("перезапуск принял старую заглушку", restart, err)
	}
	// after содержит снимок и новое поколение назначения после отказавшего повторного запуска.
	after, err := f.service.GetOutput(ctx, accepted.OutputID)
	if err != nil || !bytes.Equal(original, silentContractSnapshotBytes(t, after.Snapshot)) || after.Generation != beforeRestart.Generation {
		t.Fatal("перезапуск подменил закреплённый снимок", after, err)
	}
	// file предоставляет чтение исторического файла по сохранённому манифесту после отказа
	// перезапуска.
	file, err := f.files.OpenExact(ctx, after.Snapshot.Artifact.ManagedPath, after.Snapshot.Artifact.SizeBytes, after.Snapshot.Artifact.ContentHash)
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

// silentContractPublishCurrent публикует новое поколение проверочного файла с текущей версией
// беззвучного генератора и подтверждает завершение его подготовки.
func silentContractPublishCurrent(t *testing.T, c *Config) {
	t.Helper()
	// ctx задаёт контекст явной повторной подготовки текущего поколения.
	ctx := context.Background()
	// assets содержит объекты заглушки фикстуры; err сообщает отказ их чтения перед повторной
	// подготовкой.
	assets, err := c.Core.ListAssets(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8}})
	if err != nil || len(assets.Items) != 1 {
		t.Fatal(assets, err)
	}
	// asset содержит единственный объект заглушки, для которого создаётся текущее поколение.
	asset := assets.Items[0]
	// generation содержит явно повторно подготовляемое поколение с текущей версией беззвучного
	// генератора.
	generation, err := c.Core.ReservePreparation(ctx, core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage, GeneratorVersion: silentContractGenerator, Rebuild: true})
	if err != nil {
		t.Fatal(err)
	}
	// fence удостоверяет захваченную попытку подготовки текущего поколения.
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
	// required задаёт вариант первого требуемого профиля нового поколения.
	required := generation.RequiredProfiles[0]
	// content задаёт байты нового проверяемого файла заглушки.
	content := []byte("проверенный файл нового поколения")
	// file предоставляет запись нового варианта в его управляемый путь.
	file, err := c.Files.Create(required.ManagedPath)
	if err != nil {
		t.Fatal(err)
	}
	// writeErr сообщает отказ записи байтов нового файла до объединения с результатом закрытия.
	_, writeErr := file.Write(content)
	if err = errors.Join(writeErr, file.Close()); err != nil {
		t.Fatal(err)
	}
	// digest содержит SHA-256 байтов публикуемого текущего варианта.
	digest := sha256.Sum256(content)
	_, err = c.Core.PublishPreparation(ctx, fence, core.GenerationPublication{SourceHash: asset.SourceHash, SizeBytes: asset.SizeBytes, SourceFormat: asset.SourceFormat, Variants: []core.FallbackVariant{{ID: required.VariantID, GenerationID: generation.ID, AssetID: asset.ID, SourceHash: asset.SourceHash, Profile: required.Profile, VerifiedFormat: required.Profile.Format, GeneratorVersion: silentContractGenerator, ManagedPath: required.ManagedPath, SizeBytes: uint64(len(content)), ContentHash: hex.EncodeToString(digest[:])}}})
	if err != nil {
		t.Fatal(err)
	}
	if err = c.Core.AcknowledgePreparationCleanup(ctx, fence); err != nil {
		t.Fatal(err)
	}
}

// TestSilentFallbackChecksGeneratorBeforeMembershipReuse проверяет версию генератора до возврата
// готового членства и повторное открытие точного файла с проверкой размера и хеша.
func TestSilentFallbackChecksGeneratorBeforeMembershipReuse(t *testing.T) {
	// f предоставляет готовое текущее поколение рядом с сохранённым историческим вариантом.
	f := newManagerFixture(t, true, func(c *Config) {
		// Публикует текущее поколение заглушки и задаёт соответствующую версию генератора Manager.

		silentContractPublishCurrent(t, c)
		c.GeneratorVersion = silentContractGenerator
	})
	silentContractLegacyVariant(t, &f)
	// ctx задаёт контекст проверки повторного использования членства и открытия файлов.
	ctx := context.Background()
	// parent содержит успешный запуск с текущей версией заглушки.
	parent := startAdmissionParent(t, f)
	// outputs содержит начальное назначение текущего поколения; err сообщает отказ чтения выходов.
	outputs, err := f.service.ListOutputs(ctx, core.PageRequest{Query: core.ListQuery{Limit: 8, SessionID: parent.SessionID}})
	if err != nil || len(outputs.Items) != 1 {
		t.Fatal(outputs, err)
	}
	// output содержит сохранённый снимок текущего назначения.
	output := outputs.Items[0]
	f.manager.mu.Lock()
	// node указывает владельца сессии, у которого проверяется повторное получение членства.
	node := f.manager.sessions[parent.SessionID]
	f.manager.mu.Unlock()
	// member содержит исходное членство текущего назначения для сравнения повторных получений.
	member, err := node.acquire(ctx, output)
	if err != nil || member == nil {
		t.Fatal("текущее беззвучное поколение не получило membership", err)
	}
	for range 2 {
		// file предоставляет очередное открытие точного текущего манифеста; err сообщает отказ
		// проверки и открытия файла.
		file, err := f.manager.openPreparedFile(ctx, output.Snapshot.Artifact)
		if err != nil {
			t.Fatal("текущий manifest не открылся повторно", err)
		}
		// content содержит прочитанные байты очередного открытия; readErr сообщает отказ чтения
		// содержимого.
		content, readErr := io.ReadAll(file)
		if err = errors.Join(readErr, file.Close()); err != nil {
			t.Fatal(err)
		}
		// digest содержит контрольную сумму повторно прочитанного файла для сравнения с
		// манифестом.
		digest := sha256.Sum256(content)
		if uint64(len(content)) != output.Snapshot.Artifact.SizeBytes || hex.EncodeToString(digest[:]) != output.Snapshot.Artifact.ContentHash {
			t.Fatal("открыт другой файл или сохранено смещение предыдущей попытки")
		}
	}
	// corrupt задаёт текущий манифест с намеренно неверной контрольной суммой.
	corrupt := output.Snapshot.Artifact
	corrupt.ContentHash = strings.Repeat("0", sha256.Size*2)
	// file содержит ответ открытия повреждённого манифеста; err проверяется на ошибку содержимого.
	if file, err := f.manager.openPreparedFile(ctx, corrupt); !errors.Is(err, core.ErrCorrupt) || file != nil {
		t.Fatal("совпадение версии обошло проверку содержимого", file, err)
	}
	// oldVariant содержит сохранённый исторический вариант для построения валидного контрпримера
	// старой версии.
	oldVariant, err := f.service.GetVariant(ctx, f.binding.VariantID)
	if err != nil {
		t.Fatal("прежний вариант перестал читаться после явной подготовки нового", err)
	}
	// legacy содержит копию текущего выхода с историческим манифестом заглушки.
	legacy := output
	legacy.Snapshot.Artifact = core.ArtifactManifest{GenerationID: oldVariant.GenerationID, VariantID: oldVariant.ID, AssetID: oldVariant.AssetID, SourceHash: oldVariant.SourceHash, Profile: oldVariant.Profile, ProfileKey: oldVariant.ProfileKey, CanonicalProfile: bytes.Clone(oldVariant.CanonicalProfile), GeneratorVersion: oldVariant.GeneratorVersion, ManagedPath: oldVariant.ManagedPath, SizeBytes: oldVariant.SizeBytes, ContentHash: oldVariant.ContentHash, VerifiedFormat: oldVariant.VerifiedFormat}
	if err = legacy.Validate(); err != nil {
		t.Fatal("контрпример должен сохранять валидность снимка", err)
	}
	// legacyBytes сохраняет исходные байты старого снимка перед отклоняемым получением членства.
	legacyBytes := silentContractSnapshotBytes(t, legacy.Snapshot)
	// reused содержит ответ получения членства со старым манифестом; err проверяется на отказ версии
	// до повторного использования.
	if reused, err := node.acquire(ctx, legacy); !errors.Is(err, core.ErrNotReady) || reused != nil {
		t.Fatal("старый генератор обошёл проверку через готовый membership", reused, err)
	}
	if !bytes.Equal(legacyBytes, silentContractSnapshotBytes(t, legacy.Snapshot)) {
		t.Fatal("старый снимок подменён новым вариантом")
	}
	// reused содержит повторно полученное текущее членство; err сообщает отказ проверки его
	// сохранности.
	if reused, err := node.acquire(ctx, output); err != nil || reused != member {
		t.Fatal("отказ старому снимку повредил текущий membership", err)
	}
}

// TestSilentFallbackSessionOpenerRejectsEveryLegacyPlayerAttempt проверяет отказ старому манифесту
// на прямом входе реестра и при каждом повторном открытии до обращения к диску и построения процесса.
func TestSilentFallbackSessionOpenerRejectsEveryLegacyPlayerAttempt(t *testing.T) {
	// players считает построения проигрывателя; network считает сетевые подключения.
	var players, network atomic.Uint64
	// f предоставляет текущую заглушку и порты, обнаруживающие внешнее выполнение.
	f := newManagerFixture(t, false, func(c *Config) {
		// Подменяет запуск проигрывателя и сеть счётчиками для проверки раннего отказа.
		c.Group.Player = func(context.Context, string, *os.File, ffmpeg.Attempt) (ffmpeg.Command, error) {
			// Учитывает недопустимое построение процесса без запуска программы.
			players.Add(1)
			return ffmpeg.Command{}, core.ErrInvalid
		}
		silentContractNoNetwork(c, &network)
	})
	// ctx задаёт контекст проверки синтетической истории.
	ctx := context.Background()
	// parent содержит работающего родителя без начального назначения.
	parent := startAdmissionParent(t, f)
	f.manager.mu.Lock()
	// accepted сохраняет добавление до преобразования снимка в исторический; err сообщает отказ.
	accepted, err := f.service.AddOutput(ctx, core.AddOutputRequest{RequestID: "legacy-player-attempts", SessionID: parent.SessionID, DestinationID: f.destination.ID, Binding: f.binding})
	if err == nil {
		f.manager.commands[accepted.ID] = true
		silentContractLegacyOutputs(t, f, parent.SessionID)
	}
	// node предоставляет реестр родителя для обхода более ранней проверки членства.
	node := f.manager.sessions[parent.SessionID]
	f.manager.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	// output содержит настоящий сохранённый снимок с прежним генератором.
	output, err := f.service.GetOutput(ctx, accepted.OutputID)
	if err != nil {
		t.Fatal(err)
	}
	// manifest сохраняет историческую привязку для повторных проверок открытия.
	manifest := output.Snapshot.Artifact
	if err = manifest.Validate(); err != nil {
		t.Fatal("исторический контракт повреждён", err)
	}
	// ref обязан отсутствовать: старый файл отвергается до регистрации группы;
	// err должен обозначать неготовность исторической версии генератора.
	if ref, err := node.registry.Acquire(ctx, output.Snapshot, node.router); !errors.Is(err, core.ErrNotReady) || ref != nil {
		t.Fatal("реестр принял старую заглушку", ref, err)
	}
	for range 2 {
		// file обязан отсутствовать при каждом повторном открытии старого manifest;
		// err должен сообщить о неготовности до работы с файловой системой.
		if file, err := f.manager.openPreparedFile(ctx, manifest); !errors.Is(err, core.ErrNotReady) || file != nil {
			t.Fatal("повтор открытия разрешил исторический manifest", file, err)
		}
		// file обязан отсутствовать и при прямом обращении к FileStore без менеджера;
		// err должен сохранить отказ версии генератора на этой самостоятельной границе.
		if file, err := f.files.OpenVerified(ctx, manifest); !errors.Is(err, core.ErrNotReady) || file != nil {
			t.Fatal("FileStore открыл старый manifest", file, err)
		}
	}
	// missing содержит старый manifest с отсутствующим путём: версия проверяется до файловой системы.
	missing := manifest
	missing.ManagedPath += ".missing"
	if _, err = f.manager.openPreparedFile(ctx, missing); !errors.Is(err, core.ErrNotReady) {
		t.Fatal("файловая проверка опередила проверку версии", err)
	}
	if players.Load() != 0 || network.Load() != 0 {
		t.Fatal("старый manifest начал внешнее выполнение", players.Load(), network.Load())
	}
}

// TestOutputCleanupJoinsProvisionalMembershipBeforeReleasingOwnership проверяет завершение
// предварительной группы и её операции области, даже если отменённый запуск выхода не сохранил
// членство в своём узле.
func TestOutputCleanupJoinsProvisionalMembershipBeforeReleasingOwnership(t *testing.T) {
	// f предоставляет сессию, чей проигрыватель отказывает в готовности предварительной группы.
	f := newManagerFixture(t, false, func(c *Config) {
		// Задаёт проигрыватель с отказом готовности для проверки очистки предварительного членства.

		c.Group.Player = func(context.Context, string, *os.File, ffmpeg.Attempt) (ffmpeg.Command, error) {
			// Возвращает ErrNotReady вместо команды проигрывателя, оставляя созданную группу непригодной для
			// запуска выхода.

			return ffmpeg.Command{}, core.ErrNotReady
		}
	})
	// ctx ограничивает очистку предварительного членства десятью секундами; cancel освобождает этот
	// срок.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// parent содержит успешный запуск сессии без начальных назначений.
	parent := startAdmissionParent(t, f)
	f.manager.mu.Lock()
	// accepted содержит добавление, временно скрытое от диспетчера; err сообщает отказ его
	// сохранения.
	accepted, err := f.service.AddOutput(ctx, core.AddOutputRequest{RequestID: "provisional-membership-cleanup", SessionID: parent.SessionID, DestinationID: f.destination.ID, Binding: f.binding})
	if err == nil {
		f.manager.commands[accepted.ID] = true
	}
	// node указывает владельца предварительного членства проверяемого выхода.
	node := f.manager.sessions[parent.SessionID]
	f.manager.mu.Unlock()
	if err != nil {
		t.Fatal(err)
	}
	// output содержит снимок выхода для получения предварительной группы.
	output, err := f.service.GetOutput(ctx, accepted.OutputID)
	if err != nil {
		t.Fatal(err)
	}
	// member содержит предварительное членство, которое не будет сохранено в искусственно отменённом
	// узле выхода.
	member, err := node.acquire(ctx, output)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает предварительное членство после проверки, сохраняя возможность завершения ресурсов и при
		// раннем отказе теста.

		// err сообщает отказ закрытия предварительного членства при очистке теста.
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
	// ref содержит созданную ссылку предварительной группы; readinessErr сообщает отказ её начальной
	// готовности.
	ref, readinessErr := member.reference, member.err
	member.mu.Unlock()
	if ref == nil || !errors.Is(readinessErr, core.ErrNotReady) {
		t.Fatal("не создано предварительное владение группой", ref, readinessErr)
	}
	// fence удостоверяет выданную попытку добавления перед моделированием отменённого узла.
	fence, err := f.manager.issue(ctx, accepted)
	if err != nil {
		t.Fatal(err)
	}
	// stop отменяет искусственное исполнение выхода до получения им ссылки членства.
	_, stop := context.WithCancel(node.ctx)
	stop()
	// done изображает уже завершившийся обработчик отменённого выхода.
	done := make(chan struct{})
	close(done)
	// Отмена запуска до получения членства в группе оставляет предварительную ссылку у Session.
	// failed изображает отменённый выход без собственной ссылки на предварительное членство.
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
	// proof содержит результат проверки области после очистки выхода для выявления оставшихся
	// операций.
	proof, err := node.scope.Resolve(ctx)
	if err != nil || proof.Unknown || proof.PendingOperations != 0 {
		t.Fatal("очистка потеряла незавершённую операцию Scope предварительной группы", proof, err)
	}
}
