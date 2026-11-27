package storage

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// TestChatConfigurationReadbackMatchesStoredPayload отличает повреждение записи от потери полей при чтении.
// t проверяет, что созданные Account и Source сохраняют точную нагрузку и полностью читаются через сервис.
func TestChatConfigurationReadbackMatchesStoredPayload(t *testing.T) {
	// fixture создаёт собственную файловую SQLite и действительного родителя Source.
	fixture := newDomainFixture(t, 1)
	// ctx ограничивает обращения только к временной БД этого теста.
	ctx := context.Background()
	// created содержит раздельные конфигурацию и полномочия; err передаёт отказ создания.
	created, err := fixture.service.CreateChatAccount(ctx, core.ChatAccount{Provider: core.ChatYouTube, Name: "Аккаунт"}, core.ChatCredentialBundle{YouTube: &core.YouTubeChatCredentials{APIKey: "synthetic-readback-key"}}, "readback-write")
	if err != nil {
		t.Fatal(err)
	}
	// expected содержит точную конфигурационную оболочку без серверных полномочий.
	expected, err := core.EncodeMetadata(created.Account)
	if err != nil {
		t.Fatal(err)
	}
	// persisted читает нагрузку напрямую, чтобы отказ ORM нельзя было принять за ошибку записи.
	var persisted []byte
	if err = fixture.store.sqlDB.QueryRowContext(ctx, "SELECT payload FROM chat_accounts WHERE id=?", created.Account.ID).Scan(&persisted); err != nil || !bytes.Equal(persisted, expected) {
		t.Fatalf("Конфигурация аккаунта не сохранена точно: %v", err)
	}
	// account должен восстановить все поля из уже проверенной SQL-нагрузки.
	account, err := fixture.service.GetChatAccount(ctx, created.Account.ID)
	if err != nil || !reflect.DeepEqual(account, created.Account) {
		t.Fatalf("Чтение аккаунта потеряло сохранённые метаданные: %v", err)
	}
	// options задаёт допустимый вариант конфигурации платформы без дополнительных настроек.
	options, err := core.NewChatOptions(core.ChatYouTube)
	if err != nil {
		t.Fatal(err)
	}
	// createdSource связывает аккаунт с действительным назначением и сохраняет явный Enabled.
	createdSource, err := fixture.service.CreateChatSource(ctx, core.ChatSource{DestinationID: fixture.destinations[0].ID, AccountID: account.ID, Provider: account.Provider, Name: "Источник", ChannelHint: "UCreadback", Options: options, Enabled: true})
	if err != nil {
		t.Fatal(err)
	}
	expected, err = core.EncodeMetadata(createdSource)
	if err != nil {
		t.Fatal(err)
	}
	if err = fixture.store.sqlDB.QueryRowContext(ctx, "SELECT payload FROM chat_sources WHERE id=?", createdSource.ID).Scan(&persisted); err != nil || !bytes.Equal(persisted, expected) {
		t.Fatalf("Конфигурация источника не сохранена точно: %v", err)
	}
	// source должен сохранить Record, ссылки, options и Enabled после отдельного чтения.
	source, err := fixture.service.GetChatSource(ctx, createdSource.ID)
	if err != nil || !reflect.DeepEqual(source, createdSource) {
		t.Fatalf("Чтение источника потеряло сохранённые метаданные: %v", err)
	}
}

// TestPopulatedPreviousStorageFormatIsPreserved проверяет отказ настоящему заполненному старому формату.
// Архив только читается; SQLite открывает отдельную копию в t.TempDir.
func TestPopulatedPreviousStorageFormatIsPreserved(t *testing.T) {
	// original содержит завершённый baseline-образ с историей, err — результат чтения архива.
	original, err := os.ReadFile("testdata/storage_format4/state.sqlite")
	if err != nil {
		t.Fatal(err)
	}
	// digest подтверждает подлинный снимок до смены активной схемы.
	digest := sha256.Sum256(original)
	if hex.EncodeToString(digest[:]) != "d629b54a8589fb968b3a96d0188223b69ad39cfa98b426e2b28428276986cd07" {
		t.Fatal("Изменён архив заполненной БД")
	}
	// path адресует единственную изменяемую копию текущего теста.
	path := filepath.Join(t.TempDir(), "previous.sqlite")
	if err = os.WriteFile(path, original, 0600); err != nil {
		t.Fatal(err)
	}
	// db открывает копию только для снимков строк и схемы до и после отказа Open.
	db := preflightTestDB(t, path)
	// before и schema сохраняют прикладные данные, историю Goose и определения объектов.
	before, schema := captureSQLRows(t, db), preflightSchema(t, db)
	// rejected и err должны сообщить несовместимость до Goose и Recover.
	rejected, err := Open(context.Background(), path, testOptions())
	if rejected != nil || !errors.Is(err, ErrIncompatibleFormat) {
		t.Fatal("Старый формат принят", rejected, err)
	}
	if !reflect.DeepEqual(before, captureSQLRows(t, db)) || !reflect.DeepEqual(schema, preflightSchema(t, db)) {
		t.Fatal("Отказ изменил исходные строки, историю либо схему")
	}
	// after повторно читает неизменяемый архив и проверяет отсутствие случайной перезаписи.
	after, err := os.ReadFile("testdata/storage_format4/state.sqlite")
	if err != nil || !bytes.Equal(original, after) {
		t.Fatal("Изменён исходный архив", err)
	}
}
