package storage

import (
	"bytes"
	"context"
	"errors"
	"reflect"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// TestAudioPublicationRechecksCodecAndReopensManifest проверяет полную
// идентичность при смене только аудиокодека с прежним битрейтом и видеорецептом.
// t требует атомарный отказ прежнего набора профилей и точное чтение полного профиля во всех
// копиях опубликованного поколения, варианта и снимков нового запуска после Recover.
func TestAudioPublicationRechecksCodecAndReopensManifest(t *testing.T) {
	// fixture предоставляет отдельную SQLite с прежним опубликованным AAC-вариантом.
	fixture := newDomainFixture(t, 1)
	// ctx ограничивает операции собственными ресурсами SQLite без медиапроцессов.
	ctx := context.Background()
	// requested задаёт полный новый AV1/Opus-рецепт с независимой идентичностью подготовки.
	requested := fixture.profile
	requested.Profile.Encoding = core.DefaultEncoding(core.ModeCopy, core.VideoEncoderSVTAV1, core.RateControlVBR)
	requested.Profile.Encoding.AudioEncoder = "libopus"
	requested.Profile.LiveAudioMode = core.LiveAudioCopy
	requested.Profile.VideoEncoding.MaxBitrateBPS, requested.Profile.VideoEncoding.BufferBits = 0, 0
	requested.Profile.Format.Audio = core.AudioFormat{Codec: core.CodecOpus, CodecProfile: "celt", SampleRateHz: 48000, Channels: 2, ChannelLayout: "stereo"}
	requested.Profile.AudioEncoding.BitrateBPS = 128000
	// updated и err сохраняют допустимый явный запрос без изменения старого варианта.
	updated, err := fixture.service.UpdateProfile(ctx, requested, requested.Revision)
	if err != nil {
		t.Fatal(err)
	}
	// beforeStart фиксирует данные перед отказом применить прежний AAC-файл к новому рецепту.
	beforeStart := captureSQLRows(t, fixture.store.sqlDB)
	if _, err = fixture.service.SelectStartBindings(ctx, fixture.stream.ID, nil); !errors.Is(err, core.ErrNotReady) || !reflect.DeepEqual(beforeStart, captureSQLRows(t, fixture.store.sqlDB)) {
		t.Fatal("Прежний вариант допущен к новому аудиорецепту либо чтение изменило БД", err)
	}
	// generation и fence резервируют полный новый рецепт до поздней смены только аудио.
	generation, fence := issuedFixturePreparation(t, fixture)
	if len(generation.RequiredProfiles) != 1 || generation.RequiredProfiles[0].Profile != updated.Profile || generation.RequiredProfiles[0].ProfileKey == fixture.variant.ProfileKey || bytes.Equal(generation.RequiredProfiles[0].CanonicalProfile, fixture.variant.CanonicalProfile) {
		t.Fatal("Подготовка потеряла полную новую аудиоидентичность")
	}
	// changed сохраняет битрейт 128000, меняя только аудиопривязку Opus на MP3.
	changed := updated
	changed.Profile.Format.Audio.Codec, changed.Profile.Format.Audio.CodecProfile, changed.Profile.Encoding.AudioEncoder = core.CodecMP3, "mpeg1-layer3", "libmp3lame"
	changed, err = fixture.service.UpdateProfile(ctx, changed, updated.Revision)
	if err != nil {
		t.Fatal(err)
	}
	// beforePublish фиксирует строки и журнал файлов перед конфликтом требуемого набора профилей.
	beforePublish := captureSQLRows(t, fixture.store.sqlDB)
	if _, err = fixture.service.PublishPreparation(ctx, fence, fixturePublication(generation, "audio-source", "audio-content")); !errors.Is(err, core.ErrConflict) || !reflect.DeepEqual(beforePublish, captureSQLRows(t, fixture.store.sqlDB)) {
		t.Fatal("Смена аудиокодека допустила публикацию прежнего набора либо частичную запись", err)
	}
	// restored возвращает точный полный рецепт с новой Record; версия записи не определяет идентичность файла.
	restored := changed
	restored.Profile = updated.Profile
	if _, err = fixture.service.UpdateProfile(ctx, restored, changed.Revision); err != nil {
		t.Fatal(err)
	}
	// published подтверждает атомарную положительную публикацию того же fence после восстановления рецепта.
	published, err := fixture.service.PublishPreparation(ctx, fence, fixturePublication(generation, "audio-source", "audio-content"))
	if err != nil {
		t.Fatal(err)
	}
	if err = fixture.service.AcknowledgePreparationCleanup(ctx, fence); err != nil {
		t.Fatal(err)
	}
	fixture = reopenFixture(t, fixture)
	// reopened и variant сверяют все сохраняемые копии текущего профиля после штатного открытия.
	reopened, err := fixture.service.GetGeneration(ctx, generation.ID)
	if err != nil || !reflect.DeepEqual(reopened.RequiredProfiles, published.RequiredProfiles) {
		t.Fatal("Recover изменил новый required set", err)
	}
	variant, err := fixture.service.GetVariant(ctx, generation.RequiredProfiles[0].VariantID)
	if err != nil || variant.Profile != updated.Profile || variant.ProfileKey != generation.RequiredProfiles[0].ProfileKey || !bytes.Equal(variant.CanonicalProfile, generation.RequiredProfiles[0].CanonicalProfile) {
		t.Fatal("Recover изменил новую identity варианта", err)
	}
	// bindings подтверждают пригодность только точного нового поколения после повторного открытия.
	bindings, err := fixture.service.SelectStartBindings(ctx, fixture.stream.ID, nil)
	if err != nil || len(bindings) != 1 || bindings[0].GenerationID != generation.ID {
		t.Fatal("Новая публикация не получила допуска", bindings, err)
	}
	// receipt создаёт сохраняемые снимки с точными bindings без выдачи внешней работы.
	receipt, err := fixture.service.StartSession(ctx, core.StartSessionRequest{RequestID: "audio-profile-start", StreamID: fixture.stream.ID, Bindings: bindings})
	if err != nil {
		t.Fatal(err)
	}
	// session и outputs читают самостоятельные снимки полного профиля и манифеста.
	session := readSession(t, fixture.store, receipt.SessionID)
	outputs := readOutputs(t, fixture.store, receipt.SessionID)
	if len(session.Snapshot.Groups) != 1 || session.Snapshot.Groups[0].Profile.Profile != updated.Profile || len(outputs) != 1 || outputs[0].Snapshot.Profile.Profile != updated.Profile || outputs[0].Snapshot.Artifact.Profile != updated.Profile || outputs[0].Snapshot.Artifact.ProfileKey != variant.ProfileKey || !bytes.Equal(outputs[0].Snapshot.Artifact.CanonicalProfile, variant.CanonicalProfile) {
		t.Fatal("Новый запуск потерял самостоятельный полный аудиопрофиль или manifest")
	}
	fixture = reopenFixture(t, fixture)
	if !reflect.DeepEqual(readSession(t, fixture.store, receipt.SessionID).Snapshot, session.Snapshot) || !reflect.DeepEqual(readOutputs(t, fixture.store, receipt.SessionID)[0].Snapshot, outputs[0].Snapshot) {
		t.Fatal("Recover переписал самостоятельные снимки нового запуска или manifest")
	}
}
