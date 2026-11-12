package storage

import (
	"context"
	"errors"
	"reflect"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// Проверяет точную попытку подготовки, сохранение текущего комплекта при ошибке и отказ обходных
// операций публикации отдельных Asset/Variant.
func TestPreparationRequiresExactAttemptAndPreservesCurrentOnFailure(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 1)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// g содержит новое зарезервированное поколение для проверки точной попытки. err сообщает об
	// ошибке резервирования полного поколения подготовки.
	g, err := f.service.ReservePreparation(ctx, core.PreparationRequest{AssetID: f.asset.ID, ExpectedRevision: f.asset.Revision, Kind: core.AssetImage, GeneratorVersion: "generator-v2"})
	if err != nil {
		t.Fatal(err)
	}
	// fence идентифицирует точную захваченную попытку проверяемой команды.
	fence, err := f.service.ClaimPreparation(ctx, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = f.service.MarkPreparationIssued(ctx, fence); err != nil {
		t.Fatal(err)
	}
	// wrong содержит копию точной идентичности с намеренно подменённым полем.
	wrong := fence
	wrong.AttemptToken = "old-attempt"
	_, err = f.service.PublishPreparation(ctx, wrong, core.GenerationPublication{SourceHash: "hash", SizeBytes: 10, SourceFormat: core.SourceFormat{ImageFormat: "png"}})
	requireDomainError(t, err, core.ErrStale)
	// p содержит публикацию с намеренно нулевым размером оригинала.
	p := fixturePublication(g, "hash", "result")
	p.SizeBytes = 0
	_, err = f.service.PublishPreparation(ctx, fence, p)
	requireDomainError(t, err, core.ErrInvalid)
	// current содержит стабильную заглушку после отклонённой публикации для проверки сохранности
	// current.
	current, err := f.service.GetAsset(ctx, f.asset.ID)
	if err != nil || current.CurrentGenerationID != f.asset.CurrentGenerationID || current.SourceHash != f.asset.SourceHash {
		t.Fatalf("ошибка публикации изменила current: %+v, %v", current, err)
	}
	if err = f.service.CancelPreparation(ctx, fence, "Подготовка отменена"); err != nil {
		t.Fatal(err)
	}
	_, err = f.service.PublishPreparation(ctx, fence, fixturePublication(g, "late", "late"))
	requireDomainError(t, err, core.ErrStale)
	if err = f.service.AcknowledgePreparationCleanup(ctx, fence); err != nil {
		t.Fatal(err)
	}
	_, err = f.service.PublishAsset(ctx, f.asset.ID, f.asset.Revision, f.asset.AttemptToken, "bypass", 10, core.SourceFormat{ImageFormat: "png"})
	requireDomainError(t, err, core.ErrInvalid)
	_, err = f.service.ReserveVariant(ctx, f.variant)
	requireDomainError(t, err, core.ErrInvalid)
	_, err = f.service.PublishVariant(ctx, f.variant.ID, f.variant.Revision, f.variant.AttemptToken, "bypass", 10)
	requireDomainError(t, err, core.ErrInvalid)
	requireDomainError(t, f.service.FailAsset(ctx, f.asset.ID, f.asset.Revision, f.asset.AttemptToken), core.ErrInvalid)
	requireDomainError(t, f.service.FailVariant(ctx, f.variant.ID, f.variant.Revision, f.variant.AttemptToken), core.ErrInvalid)
}

// Проверяет отказ несовпадающих генератора, профиля, поколения и варианта без частичной Session и
// квитанции.
func TestStartRejectsEveryNonmatchingPreparedBindingAtomically(t *testing.T) {
	// mismatch выбирает несовпадение генератора, профиля, поколения или варианта.
	for _, mismatch := range []string{"generator", "profile", "generation", "variant"} {
		t.Run(mismatch, func(t *testing.T) {
			// Проверяет атомарный отказ запуска с чужой версией генератора, изменённым профилем, прежним
			// поколением либо отсутствующим вариантом.

			// f объединяет временный Store, Service, поток и опубликованную заглушку
			// проверяемого сценария.
			f := newDomainFixture(t, 1)
			// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
			ctx := context.Background()
			// request задаёт запуск с точной привязкой подготовленного варианта.
			request := f.startRequest("start-with-binding")
			switch mismatch {
			case "generator":
				request.Bindings[0].GeneratorVersion = "other-generator"
			case "generation":
				request.Bindings[0].GenerationID = "other-generation"
			case "variant":
				request.Bindings[0].VariantID = "missing-variant"
			case "profile":
				// p содержит полный именованный профиль для проверяемого изменения.
				p := f.profile
				p.Profile.Format.Video.Width++
				// err сообщает об ошибке условного изменения полного профиля.
				if _, err := f.service.UpdateProfile(ctx, p, p.Revision); err != nil {
					t.Fatal(err)
				}
			}
			// err сообщает об ошибке принятия ручного запуска потока.
			_, err := f.service.StartSession(ctx, request)
			if mismatch == "variant" {
				requireDomainError(t, err, core.ErrNotFound)
			} else {
				requireDomainError(t, err, core.ErrNotReady)
			}
			if err = f.store.View(ctx, func(r core.Repositories) error {
				// Проверяет отсутствие частичной Session и квитанции после отклонения привязки подготовленного файла.

				// ss содержит сохраняемые сессии после отказа точной привязки. e
				// сообщает об ошибке чтения сохраняемых запусков потока.
				ss, e := r.Runtime.Sessions(ctx, core.ListQuery{Limit: 10, StreamID: f.stream.ID})
				if e != nil {
					return e
				}
				if len(ss) != 0 {
					t.Fatal("отклонённый binding оставил частичную Session")
				}
				_, e = r.Runtime.Receipt(ctx, request.RequestID)
				requireDomainError(t, e, core.ErrNotFound)
				return nil
			}); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Проверяет ограничение SQLite, запрещающее расходящийся хеш источника варианта и его поколения.
func TestVariantSourceIdentityCannotDivergeFromGeneration(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 0)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// err сообщает об ошибке попытки нарушить совпадение source_hash варианта и поколения прямым
	// SQL.
	_, err := f.store.sqlDB.ExecContext(ctx, "UPDATE fallback_variants SET source_hash = ? WHERE id = ?", "wrong-source-hash", f.variant.ID)
	requireDomainError(t, translate(err), core.ErrInUse)
	// got содержит прочитанное сохраняемое значение для проверки результата операции.
	got, err := f.service.GetVariant(ctx, f.variant.ID)
	if err != nil || !reflect.DeepEqual(got, f.variant) {
		t.Fatalf("смена источника изменила вариант: %+v, %v", got, err)
	}
}

// Проверяет отказ устаревшей привязки без неявного выбора нового поколения и сохранение точно
// запрошенного текущего варианта.
func TestFreshStartRejectsRetiredGenerationWithoutSelectingNewest(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 1)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// old сохраняет запрос с прежней точной привязкой до публикации нового поколения.
	old := f.startRequest("old-binding")
	// a содержит стабильную заглушку проверяемого поколения. v содержит опубликованный вариант
	// точного поколения.
	a, v := publishFixtureGeneration(t, f.service, f.asset, f.asset.SourceHash, "new-content")
	// err сообщает об ошибке принятия ручного запуска потока.
	_, err := f.service.StartSession(ctx, old)
	requireDomainError(t, err, core.ErrNotReady)
	f.asset, f.variant = a, v
	// start содержит принятую квитанцию первоначального запуска сессии.
	start := startDomainSession(t, f)
	// output содержит сохраняемый выход с самостоятельным снимком и намерением.
	output := readOutputs(t, f.store, start.SessionID)[0]
	if output.Snapshot.Artifact.GenerationID != v.GenerationID || output.Snapshot.Artifact.VariantID != v.ID {
		t.Fatal("start не сохранил точный запрошенный комплект")
	}
}

// Проверяет сохранение происхождения и очистки удалённого поколения, отказ чужого CleanupFence и
// запрет повторного использования управляемого пути.
func TestDeletedGenerationPathCannotBeReusedAndCleanupIsImmutable(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 0)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// old содержит прежнее опубликованное поколение перед его разрешённым удалением. err сообщает об
	// ошибке чтения полного поколения подготовки.
	old, err := f.service.GetGeneration(ctx, f.variant.GenerationID)
	if err != nil {
		t.Fatal(err)
	}
	// a содержит стабильную заглушку проверяемого поколения.
	a, _ := publishFixtureGeneration(t, f.service, f.asset, "new-source", "new-content")
	if a.CurrentGenerationID == old.ID {
		t.Fatal("замена переиспользовала поколение")
	}
	if err = f.service.DeleteGeneration(ctx, old.ID, old.Revision); err != nil {
		t.Fatal(err)
	}
	// cleanups содержит сохраняемые намерения физической очистки после проверяемой операции.
	cleanups, err := f.service.ListCleanups(ctx, core.PageRequest{Query: core.ListQuery{Limit: 20, PendingOnly: true}})
	if err != nil || len(cleanups.Items) != 5 {
		t.Fatalf("намерения файловой очистки: %+v, %v", cleanups, err)
	}
	// intent содержит выбранное намерение физической очистки удалённого поколения.
	intent := cleanups.Items[0]
	// wrong содержит копию точной идентичности с намеренно подменённым полем.
	wrong := core.CleanupFenceFor(intent)
	wrong.ManagedPath = "variants/foreign.mp4"
	requireDomainError(t, f.service.AcknowledgeCleanup(ctx, wrong), core.ErrStale)
	if err = f.service.AcknowledgeCleanup(ctx, core.CleanupFenceFor(intent)); err != nil {
		t.Fatal(err)
	}
	// files содержит сохраняемый журнал файлов удалённого поколения.
	files, err := f.service.ListGenerationFiles(ctx, core.PageRequest{Query: core.ListQuery{Limit: 20, GenerationID: old.ID}})
	if err != nil {
		t.Fatal(err)
	}
	// duplicate содержит запись с новым ID, пытающуюся занять уже принадлежавший поколению путь.
	duplicate := files.Items[0]
	duplicate.ID = "new-file-id"
	err = f.store.Write(ctx, func(r core.Repositories) error {
		// Пробует повторно занять путь удалённого поколения другой записью OwnedFile, ожидая ErrConflict.
		return r.Artifacts.InsertOwnedFile(ctx, duplicate)
	})
	requireDomainError(t, err, core.ErrConflict)
	if _, err = f.service.GetVariant(ctx, f.variant.ID); !errors.Is(err, core.ErrNotFound) {
		t.Fatalf("удалённые метаданные сохранились: %v", err)
	}
}

// Проверяет ErrCorrupt вместо ErrNotFound при повреждённой нагрузке и освобождение соединений после
// ошибок чтения.
func TestCorruptMetadataIsDistinctFromMissingAndDoesNotLeakRows(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 0)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// err сообщает об ошибке подмены нагрузки профиля неподдерживаемой версией для проверки
	// ErrCorrupt.
	if _, err := f.store.sqlDB.ExecContext(ctx, "UPDATE profiles SET payload = ? WHERE id = ?", []byte(`{"Version":999,"Payload":{}}`), f.profile.ID); err != nil {
		t.Fatal(err)
	}
	for range 3 {
		// err сообщает об ошибке чтения полного сохраняемого профиля.
		_, err := f.service.GetProfile(ctx, f.profile.ID)
		requireDomainError(t, err, core.ErrCorrupt)
		if errors.Is(err, core.ErrNotFound) {
			t.Fatal("повреждение подменено отсутствием")
		}
		if f.store.sqlDB.Stats().InUse != 0 {
			t.Fatal("ошибка декодирования удерживает соединение")
		}
	}
}

// Проверяет перевод сохранённых времён в UTC до микросекунд и отказ для непредставимого диапазона.
func TestStoredTimestampsNormalizeUTCAndRejectUnrepresentableRange(t *testing.T) {
	// s владеет временным Store проверяемого сценария.
	s := openTestStore(t)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// input задаёт время с часовым поясом и наносекундами для проверки нормализации хранения.
	input := time.Date(2026, 1, 2, 3, 4, 5, 123456789, time.FixedZone("offset", 3*60*60))
	// value задаёт поток с ненормализованными временными метками для проверки сохранения UTC.
	value := core.Stream{ID: "timestamp-stream", Revision: 1, CreatedAt: input, UpdatedAt: input, Name: "Поток", PublicationKey: "synthetic-timestamp-key"}
	// err сообщает об ошибке вставки конфигурации потока.
	if err := s.Write(ctx, func(r core.Repositories) error {
		// Вставляет Stream с часовым поясом и наносекундами для проверки нормализации времени хранения.
		return r.Config.InsertStream(ctx, value)
	}); err != nil {
		t.Fatal(err)
	}
	// err сообщает об ошибке чтения конфигурации потока.
	if err := s.View(ctx, func(r core.Repositories) error {
		// Проверяет чтение UTC-времени с усечением до микросекунд.

		// got содержит прочитанное сохраняемое значение для проверки результата операции. err
		// сообщает об ошибке чтения конфигурации потока.
		got, err := r.Config.Stream(ctx, value.ID)
		if err != nil {
			return err
		}
		if got.CreatedAt.Location() != time.UTC || !got.CreatedAt.Equal(input.Truncate(time.Microsecond)) {
			t.Fatalf("неверное UTC-время: %v", got.CreatedAt)
		}
		return nil
	}); err != nil {
		t.Fatal(err)
	}
	value.ID = "overflow-time-stream"
	value.CreatedAt = time.Date(400000, 1, 1, 0, 0, 0, 0, time.UTC)
	value.UpdatedAt = value.CreatedAt
	requireDomainError(t, s.Write(ctx, func(r core.Repositories) error {
		// Пробует сохранить Stream с непредставимым временем, ожидая ErrInvalid.
		return r.Config.InsertStream(ctx, value)
	}), core.ErrInvalid)
}
