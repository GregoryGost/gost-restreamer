package storage

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// TestExplicitEncodingPersistsCASAndPreservesSnapshots проверяет CAS, историю и точное повторное чтение полного профиля.
// t сообщает об ошибках проверяемого сценария.
func TestExplicitEncodingPersistsCASAndPreservesSnapshots(t *testing.T) {
	// f объединяет временный Store, Service, поток и опубликованную заглушку проверяемого сценария.
	f := newDomainFixture(t, 1)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// started содержит квитанцию нового запуска при остатках прежних выполнений.
	started := startDomainSession(t, f)
	// sessionBefore сохраняет активную сессию до изменения текущего профиля.
	sessionBefore := readSession(t, f.store, started.SessionID)
	// outputBefore сохраняет активный выход до изменения текущего профиля.
	outputBefore := readOutputs(t, f.store, started.SessionID)[0]
	// p содержит полный профиль, проверяемый на сохранение идентичности и явных параметров.
	p := f.profile
	p.Profile.Encoding = core.DefaultEncoding(core.ModeCopy, "libsvtav1", "vbr")
	p.Profile.VideoEncoding.MaxBitrateBPS = 0
	p.Profile.VideoEncoding.BufferBits = 0
	p.Profile.Encoding.SVTAV1.Preset = 0
	p.Profile.Encoding.SVTAV1.LookaheadFrames = 6
	p.Profile.Encoding.VideoCustom = "lp=0"
	p.Profile.Encoding.AudioCustom = "aac_coder=fast"
	// updated содержит сохранённый профиль после условного изменения. err сообщает об ошибке
	// условного изменения полного профиля.
	updated, err := f.service.UpdateProfile(ctx, p, p.Revision)
	if err != nil {
		t.Fatal(err)
	}
	if updated.Revision != p.Revision+1 {
		t.Fatal("CAS не увеличил версию")
	}
	if _, err = f.service.UpdateProfile(ctx, p, p.Revision); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("Устаревшая CAS-правка принята: %v", err)
	}
	// incomplete убирает обязательный полный блок Encoding.
	incomplete := updated
	incomplete.Profile.Encoding = core.EncodingSettings{}
	incomplete.Profile.VideoEncoding = storageProfile().VideoEncoding
	if _, err = f.service.UpdateProfile(ctx, incomplete, updated.Revision); err == nil {
		t.Fatal("Сервис допустил пустой Encoding")
	}
	// stored содержит прочитанный профиль после отклонённых CAS и неполного Encoding.
	stored, err := f.service.GetProfile(ctx, p.ID)
	if err != nil || !reflect.DeepEqual(stored, updated) {
		t.Fatalf("CAS/неполный профиль повредили материализованный профиль: %+v, %v", stored, err)
	}
	// after содержит исторический запуск либо выход после изменения текущего профиля.
	if after := readSession(t, f.store, started.SessionID); !reflect.DeepEqual(after.Snapshot, sessionBefore.Snapshot) {
		t.Fatal("Изменение профиля переписало активный Session snapshot")
	}
	// after содержит исторический запуск либо выход после изменения текущего профиля.
	if after := readOutputs(t, f.store, started.SessionID)[0]; !reflect.DeepEqual(after.Snapshot, outputBefore.Snapshot) {
		t.Fatal("Изменение профиля переписало активный Output snapshot/manifest")
	}
	f = reopenFixture(t, f)
	stored, err = f.service.GetProfile(ctx, p.ID)
	if err != nil || !reflect.DeepEqual(stored, updated) {
		t.Fatalf("Reopen применил defaults к профилю: %+v, %v", stored, err)
	}
	// after содержит исторический запуск либо выход после изменения текущего профиля.
	if after := readSession(t, f.store, started.SessionID); !reflect.DeepEqual(after.Snapshot, sessionBefore.Snapshot) {
		t.Fatal("Recovery переписал прежний активный снимок")
	}
}

// Проверяет новую идентичность подготовки после явного Encoding, конфликт его изменения до публикации
// и допустимость переименования с сохранением точного профиля в варианте и снимке выхода.
func TestExplicitEncodingPreparationUsesNewIdentityAndPreservesCurrentOnConflict(t *testing.T) {
	// f объединяет временный Store, Service и связанную конфигурацию проверки поколений заглушки.
	f := newGenerationAcceptanceFixture(t)
	// ctx задаёт контекст доменных операций проверяемого сценария SQLite.
	ctx := context.Background()
	// old сохраняет опубликованное поколение с прежней идентичностью профиля.
	old, _ := publishGenerationAcceptance(t, f.svc, f.asset.ID)
	// p содержит полный профиль, проверяемый на сохранение идентичности и явных параметров.
	p := f.profile
	p.Profile.Encoding = core.DefaultEncoding(core.ModeCopy, "libsvtav1", "vbr")
	p.Profile.VideoEncoding.MaxBitrateBPS = 0
	p.Profile.VideoEncoding.BufferBits = 0
	// updated содержит сохранённый профиль после условного изменения. err сообщает об ошибке
	// условного изменения полного профиля.
	updated, err := f.svc.UpdateProfile(ctx, p, p.Revision)
	if err != nil {
		t.Fatal(err)
	}
	// pending содержит подготовку с новой полной идентичностью Encoding.
	pending := reserveGenerationAcceptance(t, f.svc, f.asset.ID)
	if len(pending.RequiredProfiles) != 1 || pending.RequiredProfiles[0].Profile.Encoding != p.Profile.Encoding || pending.RequiredProfiles[0].ProfileKey == old.RequiredProfiles[0].ProfileKey || bytes.Equal(pending.RequiredProfiles[0].CanonicalProfile, old.RequiredProfiles[0].CanonicalProfile) {
		t.Fatal("Подготовка переиспользовала прежнюю идентичность")
	}
	// fence идентифицирует точную захваченную попытку проверяемой команды.
	fence := issueGenerationAcceptance(t, f.svc, pending)
	// changed содержит профиль с изменённым Preset после резервирования подготовки.
	changed := updated
	changed.Profile.Encoding.SVTAV1.Preset = 0
	if _, err = f.svc.UpdateProfile(ctx, changed, updated.Revision); err != nil {
		t.Fatal(err)
	}
	if _, err = f.svc.PublishPreparation(ctx, fence, generationAcceptancePublication(pending)); !errors.Is(err, core.ErrConflict) {
		t.Fatalf("Публикация не заметила новое поле кодировщика: %v", err)
	}
	// current содержит заглушку для проверки сохранения прежнего поколения при конфликте публикации.
	current, err := f.svc.GetAsset(ctx, f.asset.ID)
	if err != nil || current.CurrentGenerationID != old.ID {
		t.Fatalf("Неудачная подготовка заменила current: %+v, %v", current, err)
	}
	if err = f.svc.CancelPreparation(ctx, fence, "Профиль изменился до публикации"); err != nil {
		t.Fatal(err)
	}
	if err = f.svc.AcknowledgePreparationCleanup(ctx, fence); err != nil {
		t.Fatal(err)
	}
	// next содержит следующую подготовку после отмены конфликтной.
	next := reserveGenerationAcceptance(t, f.svc, f.asset.ID)
	// nf содержит точный допуск следующей подготовки.
	nf := issueGenerationAcceptance(t, f.svc, next)
	// named содержит текущий профиль для переименования перед публикацией.
	named, err := f.svc.GetProfile(ctx, p.ID)
	if err != nil {
		t.Fatal(err)
	}
	named.Name = "Новое имя без изменения кодирования"
	if _, err = f.svc.UpdateProfile(ctx, named, named.Revision); err != nil {
		t.Fatal(err)
	}
	// published содержит опубликованное поколение после допустимого переименования.
	published, err := f.svc.PublishPreparation(ctx, nf, generationAcceptancePublication(next))
	if err != nil {
		t.Fatalf("Name-only заблокировал допустимую публикацию: %v", err)
	}
	if err = f.svc.AcknowledgePreparationCleanup(ctx, nf); err != nil {
		t.Fatal(err)
	}
	// variant содержит точный опубликованный вариант тестового поколения.
	variant, err := f.svc.GetVariant(ctx, published.RequiredProfiles[0].VariantID)
	if err != nil || variant.Profile.Encoding.SVTAV1.Preset != 0 || variant.Profile.Encoding.RateControl != "vbr" {
		t.Fatalf("Вариант потерял явные параметры: %+v, %v", variant, err)
	}
	// start содержит принятую квитанцию первоначального запуска сессии.
	start, err := f.svc.StartSession(ctx, f.start("explicit-profile-start", published))
	if err != nil {
		t.Fatal(err)
	}
	// outputs содержит все сохраняемые выходы проверяемой сессии.
	outputs := readOutputs(t, f.store, start.SessionID)
	if len(outputs) != 1 || outputs[0].Snapshot.Profile.Profile.Encoding != variant.Profile.Encoding || outputs[0].Snapshot.Artifact.ProfileKey != variant.ProfileKey || !bytes.Equal(outputs[0].Snapshot.Artifact.CanonicalProfile, variant.CanonicalProfile) {
		t.Fatalf("Output snapshot/manifest потерял полную идентичность: %+v", outputs)
	}
}
