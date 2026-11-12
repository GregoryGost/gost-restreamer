package processowner

import (
	"context"
	"errors"
	"net"
	"os"
	"path/filepath"
	"testing"
)

// Записывает файл с заданным содержимым и правами 0600; ошибка записи завершает тест.
func testFile(t *testing.T, root, name, content string) {
	t.Helper()
	// err сообщает об отказе записи контрольного файла с правами 0600.
	if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0600); err != nil {
		t.Fatal(err)
	}
}

// Создаёт файловую идентичность с заданными путём и видом объекта и фиксированными тестовыми
// владельцем и поколением.
func testFileID(name, kind string) FileIdentity {
	return FileIdentity{FileID: "file:" + name, GenerationID: "generation", ManagedPath: name, Kind: kind, Owner: "owner"}
}

// Открывает файловый журнал с тестовыми ограничениями и регистрирует его закрытие в t.Cleanup.
func testJournal(t *testing.T, root string) *FileJournal {
	t.Helper()
	// j содержит открытый тестовый файловый журнал; err сообщает об отказе его создания.
	j, err := NewFileJournal(root, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает созданный файловый журнал после проверки и сообщает тесту об ошибке.

		// err сообщает об отказе закрытия файлового журнала при очистке теста.
		if err := j.Close(); err != nil {
			t.Error(err)
		}
	})
	return j
}

// Проверяет сохранение идентичности при повторном открытии, отклонение подменённого объекта и удаление
// служебной ссылки только после подтверждения владельца. Подтверждённый FileID повторно не
// используется.
func TestFileJournalIdentity(t *testing.T) {
	// ctx задаёт общий контекст проверки идентичности файлового объекта.
	ctx := context.Background()
	// root задаёт изолированный каталог объекта и доказательств.
	root := t.TempDir()
	// id содержит точную идентичность первоначального обычного файла.
	id := testFileID("file", RegularFile)
	testFile(t, root, "file", "first inode")
	// j содержит первоначально открытый журнал для регистрации исходного объекта.
	j := testJournal(t, root)
	// err сообщает об отказе регистрации исходного обычного файла.
	if err := j.Register(ctx, id); err != nil {
		t.Fatal(err)
	}
	// err сообщает об отказе проверки исходного файла после регистрации.
	if err := j.Verify(ctx, id); err != nil {
		t.Fatal(err)
	}
	// reopened содержит повторно открытый журнал тех же доказательств.
	reopened := testJournal(t, root)
	// err сообщает об отказе проверки исходного объекта после повторного открытия.
	if err := reopened.Verify(ctx, id); err != nil {
		t.Fatal(err)
	}
	// err сообщает об отказе перемещения исходного файла перед подменой его пути.
	if err := os.Rename(filepath.Join(root, "file"), filepath.Join(root, "original")); err != nil {
		t.Fatal(err)
	}
	testFile(t, root, "file", "replacement")
	// err содержит ожидаемый ErrUnknown при проверке заменившего файл объекта.
	if err := reopened.Verify(ctx, id); !errors.Is(err, ErrUnknown) {
		t.Fatalf("replacement verified: %v", err)
	}
	// err содержит ожидаемый ErrUnknown при попытке удаления чужой замены.
	if err := reopened.Remove(ctx, id); !errors.Is(err, ErrUnknown) {
		t.Fatalf("replacement removed: %v", err)
	}
	// b содержит сохранённые байты замены; err сохраняет результаты чтения, восстановления исходника
	// и дальнейшего подтверждения очистки.
	b, err := os.ReadFile(filepath.Join(root, "file"))
	if err != nil || string(b) != "replacement" {
		t.Fatalf("replacement changed: %q %v", b, err)
	}
	if err = os.Remove(filepath.Join(root, "file")); err != nil {
		t.Fatal(err)
	}
	if err = os.Rename(filepath.Join(root, "original"), filepath.Join(root, "file")); err != nil {
		t.Fatal(err)
	}
	if err = reopened.Remove(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(root, evidenceDirectory, "anchors", fileKey(id))); err != nil {
		t.Fatal("anchor lost before FILE ack", err)
	}
	if err = reopened.Acknowledge(ctx, id); err != nil {
		t.Fatal(err)
	}
	if err = reopened.Acknowledge(ctx, id); err != nil {
		t.Fatal(err)
	}
	if _, err = os.Stat(filepath.Join(root, evidenceDirectory, "anchors", fileKey(id))); !errors.Is(err, os.ErrNotExist) {
		t.Fatal("anchor retained after FILE ack", err)
	}
	testFile(t, root, "file", "reused path")
	if err = reopened.Register(ctx, id); !errors.Is(err, ErrClosed) {
		t.Fatalf("resurrected acknowledged FileID: %v", err)
	}
}

// Проверяет повторное переименование после открытия нового журнала и независимость служебной ссылки
// конечного файла от подтверждения очистки исходника.
func TestFileJournalRenameRecovery(t *testing.T) {
	// ctx задаёт общий контекст проверки повторяемого переименования.
	ctx := context.Background()
	// root задаёт изолированный каталог исходного и целевого файлов.
	root := t.TempDir()
	// j содержит журнал, повторно открываемый после первого Rename.
	j := testJournal(t, root)
	// source обозначает исходный staging-файл с самостоятельным доказательством.
	source := testFileID("staging", RegularFile)
	// target обозначает конечный final-файл с независимым доказательством.
	target := testFileID("final", RegularFile)
	testFile(t, root, "staging", "media payload")
	// err сообщает об отказе регистрации staging-файла.
	if err := j.Register(ctx, source); err != nil {
		t.Fatal(err)
	}
	// err сообщает об отказе первоначального переименования в final.
	if err := j.Rename(ctx, source, target); err != nil {
		t.Fatal(err)
	}
	j = testJournal(t, root)
	// err сообщает об отказе проверки конечного файла после повторного открытия.
	if err := j.Verify(ctx, target); err != nil {
		t.Fatal(err)
	}
	// err сообщает об отказе повторного Rename уже перемещённого объекта.
	if err := j.Rename(ctx, source, target); err != nil {
		t.Fatal(err)
	}
	// err сообщает об отказе подтверждённого удаления отсутствующего исходного пути.
	if err := j.Remove(ctx, source); err != nil {
		t.Fatal(err)
	}
	// err сообщает об отказе освобождения служебного anchor исходной идентичности.
	if err := j.Acknowledge(ctx, source); err != nil {
		t.Fatal(err)
	}
	// err сообщает об отказе проверки независимого anchor конечного файла после подтверждения
	// исходника.
	if err := j.Verify(ctx, target); err != nil {
		t.Fatal("source FILE ack lost final anchor", err)
	}
	// err сообщает об отказе удаления конечного файла.
	if err := j.Remove(ctx, target); err != nil {
		t.Fatal(err)
	}
	// err сообщает об отказе подтверждения очистки конечного файла.
	if err := j.Acknowledge(ctx, target); err != nil {
		t.Fatal(err)
	}
}

// Проверяет удаление незаполненной резервации и известного пустого каталога, отказ при неоднозначном
// удалении маркера, потере служебной ссылки и символической ссылке.
func TestFileJournalDirectoryAndCorruption(t *testing.T) {
	t.Run("reserved-object-never-created", func(t *testing.T) {
		// Проверяет очистку резервирования без созданного объекта, отсутствие выведенного из удаления
		// подтверждения и запрет поздней регистрации.

		// j содержит журнал резервации объекта, который так и не был создан.
		j := testJournal(t, t.TempDir())
		// id обозначает не созданный обычный файл.
		id := testFileID("absent", RegularFile)
		// ctx задаёт общий контекст резервации, удаления и повторных подтверждений.
		ctx := context.Background()
		// err сообщает об отказе первоначальной записи RESERVED.
		if err := j.Reserve(ctx, id); err != nil {
			t.Fatal(err)
		}
		// err сообщает об отказе безопасного повтора той же резервации.
		if err := j.Reserve(ctx, id); err != nil {
			t.Fatal(err)
		}
		// err сообщает об отказе очистки резервации без созданного объекта.
		if err := j.Remove(ctx, id); err != nil {
			t.Fatal(err)
		}
		// err сообщает об отказе прохода повторных подтверждений до разрешённого ACKING.
		if _, err := j.RetryAcknowledgements(ctx, "", 16); err != nil {
			t.Fatal(err)
		}
		// r содержит запись удалённого объекта; err сохраняет результаты её чтения, изменения
		// стадии и повторных операций.
		r, err := j.read(id)
		if err != nil {
			t.Fatal(err)
		}
		if r.Phase != "REMOVED" {
			t.Fatal("SQL ack was inferred from physical removal")
		}
		r.Phase = "ACKING"
		if err = j.write(r); err != nil {
			t.Fatal(err)
		}
		if _, err = j.RetryAcknowledgements(ctx, "", 16); err != nil {
			t.Fatal(err)
		}
		if err = j.Register(ctx, id); !errors.Is(err, ErrClosed) {
			t.Fatalf("late register=%v", err)
		}
		if err = j.Reserve(ctx, id); !errors.Is(err, ErrClosed) {
			t.Fatalf("late reserve=%v", err)
		}
	})
	t.Run("directory-removes-only-known-empty-namespace", func(t *testing.T) {
		// Проверяет отказ удаления каталога с неизвестным содержимым и успешную очистку после удаления
		// неизвестного дочернего файла.

		// root задаёт изолированный каталог проверки неизвестного дочернего содержимого.
		root := t.TempDir()
		// j содержит журнал доказательства удаляемого каталога.
		j := testJournal(t, root)
		// id обозначает каталог, чьё удаление разрешено только без чужих дочерних объектов.
		id := testFileID("directory", Directory)
		// err сообщает об отказе создания контрольного каталога.
		if err := os.Mkdir(filepath.Join(root, id.ManagedPath), 0700); err != nil {
			t.Fatal(err)
		}
		// err сообщает об отказе регистрации каталога и его маркера.
		if err := j.Register(context.Background(), id); err != nil {
			t.Fatal(err)
		}
		testFile(t, root, "directory/unknown", "unowned")
		// err содержит ожидаемый ErrPending при чужом файле внутри каталога.
		if err := j.Remove(context.Background(), id); !errors.Is(err, ErrPending) {
			t.Fatalf("nonempty directory: %v", err)
		}
		// err сообщает об отказе явного удаления чужого контрольного дочернего файла самим
		// тестом.
		if err := os.Remove(filepath.Join(root, "directory/unknown")); err != nil {
			t.Fatal(err)
		}
		// err сообщает об отказе удаления теперь известного пустого каталога.
		if err := j.Remove(context.Background(), id); err != nil {
			t.Fatal(err)
		}
		// err сообщает об отказе подтверждения удаления каталога.
		if err := j.Acknowledge(context.Background(), id); err != nil {
			t.Fatal(err)
		}
	})
	t.Run("marker-rmdir-crash-is-unknown", func(t *testing.T) {
		// Моделирует остановку между удалением маркера и каталога и проверяет UNKNOWN вместо недоказанного
		// удаления.

		// root задаёт изолированный каталог моделируемого окна между удалением маркера и rmdir.
		root := t.TempDir()
		// j содержит журнал неоднозначно удаляемого каталога.
		j := testJournal(t, root)
		// id обозначает каталог с намеренно прерванной стадией DIR_REMOVING.
		id := testFileID("directory", Directory)
		// err сообщает об отказе создания каталога перед моделированием частичного удаления.
		if err := os.Mkdir(filepath.Join(root, id.ManagedPath), 0700); err != nil {
			t.Fatal(err)
		}
		// err сообщает об отказе регистрации каталога перед удалением маркера.
		if err := j.Register(context.Background(), id); err != nil {
			t.Fatal(err)
		}
		// r содержит запись каталога; err сохраняет результаты изменения стадии, удаления
		// маркера и проверки Unknown.
		r, err := j.read(id)
		if err != nil {
			t.Fatal(err)
		}
		r.Phase = "DIR_REMOVING"
		if err = j.write(r); err != nil {
			t.Fatal(err)
		}
		if err = j.root.Remove(r.Marker); err != nil {
			t.Fatal(err)
		}
		if err = j.Remove(context.Background(), id); !errors.Is(err, ErrUnknown) {
			t.Fatalf("ambiguous directory falsely removed: %v", err)
		}
	})
	t.Run("missing-anchor-is-not-legacy", func(t *testing.T) {
		// Проверяет, что исчезновение служебной ссылки оставляет повреждённое доказательство и не превращает
		// его в отсутствие прежней записи.

		// root задаёт изолированный каталог проверки исчезнувшего anchor.
		root := t.TempDir()
		// j содержит журнал, в котором исходное доказательство будет повреждено удалением
		// ссылки.
		j := testJournal(t, root)
		// id обозначает обычный файл с проверяемым служебным anchor.
		id := testFileID("file", RegularFile)
		// exists различает отсутствие прежнего доказательства и найденную повреждённую запись;
		// err сохраняет результаты файловых операций и проверок.
		exists, err := j.Exists(context.Background(), id)
		if err != nil || exists {
			t.Fatalf("legacy=%v %v", exists, err)
		}
		testFile(t, root, "file", "data")
		if err = j.Register(context.Background(), id); err != nil {
			t.Fatal(err)
		}
		if err = j.root.Remove(evidenceDirectory + "/anchors/" + fileKey(id)); err != nil {
			t.Fatal(err)
		}
		exists, err = j.Exists(context.Background(), id)
		if !exists || !errors.Is(err, ErrUnknown) {
			t.Fatalf("lost anchor treated as legacy: %v %v", exists, err)
		}
	})
	t.Run("symlink-rejected", func(t *testing.T) {
		// Проверяет отказ регистрации символической ссылки вместо принадлежащего обычного файла.

		// root задаёт изолированный каталог проверки символической ссылки вместо файла.
		root := t.TempDir()
		// j содержит журнал отказа регистрации символической ссылки.
		j := testJournal(t, root)
		testFile(t, root, "real", "data")
		// err сообщает об отказе создания контрольной ссылки на существующий файл.
		if err := os.Symlink("real", filepath.Join(root, "link")); err != nil {
			t.Fatal(err)
		}
		// err содержит ожидаемый ErrUnknown при регистрации ссылки как обычного файла.
		if err := j.Register(context.Background(), testFileID("link", RegularFile)); !errors.Is(err, ErrUnknown) {
			t.Fatalf("symlink registered: %v", err)
		}
	})
}

// Проверяет регистрацию Unix-сокета, его служебную ссылку и удаление после явного закрытия слушателя
// без автоматического удаления пути.
func TestSocketAnchors(t *testing.T) {
	// root содержит короткий каталог Unix-сокета; err сохраняет результаты создания, регистрации и
	// очистки.
	root, err := os.MkdirTemp("/tmp", "processowner-socket-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Удаляет принадлежащий проверке короткий каталог Unix-сокета при завершении проверки.
		_ = os.RemoveAll(root)
	})
	// j содержит журнал доказательства контрольного Unix-сокета.
	j := testJournal(t, root)
	// id обозначает точную идентичность сокета с самостоятельным anchor.
	id := testFileID("socket", Socket)
	// listener содержит Unix-слушатель без автоматического удаления имени при закрытии.
	listener, err := net.ListenUnix("unix", &net.UnixAddr{Name: filepath.Join(root, "socket"), Net: "unix"})
	if err != nil {
		t.Fatal(err)
	}
	listener.SetUnlinkOnClose(false)
	t.Cleanup(func() {
		// Закрывает проверочный Unix-слушатель, сохраняя путь сокета для явной очистки журналом.
		_ = listener.Close()
	})
	if err = j.Register(context.Background(), id); err != nil {
		t.Fatal("socket anchor unsupported by test FS", err)
	}
	if err = j.Verify(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if err = listener.Close(); err != nil {
		t.Fatal(err)
	}
	if err = j.Remove(context.Background(), id); err != nil {
		t.Fatal(err)
	}
	if err = j.Acknowledge(context.Background(), id); err != nil {
		t.Fatal(err)
	}
}
