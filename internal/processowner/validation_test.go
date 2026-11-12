package processowner

import (
	"context"
	"errors"
	"os"
	"strings"
	"testing"
)

// Проверяет ограничения бюджетов и команд, аргументы вспомогательной программы, допустимые файловые
// пути и отказ при отсутствующем доказательстве, неверной странице и слишком большой записи.
func TestBudgetsAndInvalidInputs(t *testing.T) {
	// l задаёт исходные корректные пределы журнала и команд.
	l := testLimits()
	// err содержит результат проверки исходных корректных Limits.
	if err := l.Validate(); err != nil {
		t.Fatal(err)
	}
	// change вносит очередное недопустимое значение в отдельный бюджет.
	for _, change := range []func(*Limits){func(l *Limits) {
		// Задаёт нулевой бюджет операций для проверки недопустимых Limits.
		l.MaxOperations = 0
	}, func(l *Limits) {
		// Задаёт нулевой размер записи для проверки недопустимых Limits.
		l.MaxRecordBytes = 0
	}, func(l *Limits) {
		// Задаёт нулевое число аргументов для проверки недопустимых Limits.
		l.MaxCommandArgs = 0
	}, func(l *Limits) {
		// Задаёт нулевой бюджет байтов команды для проверки недопустимых Limits.
		l.MaxCommandBytes = 0
	}, func(l *Limits) {
		// Задаёт отрицательный предел дополнительных дескрипторов для проверки недопустимых Limits.
		l.MaxExtraFiles = -1
	}, func(l *Limits) {
		// Задаёт нулевой размер страницы для проверки недопустимых Limits.
		l.PageSize = 0
	}} {
		// bad содержит копию пределов с выбранным нарушением.
		bad := l
		change(&bad)
		// err содержит ожидаемый ErrInvalid при проверке изменённого бюджета.
		if err := bad.Validate(); !errors.Is(err, ErrInvalid) {
			t.Fatalf("limits=%+v %v", bad, err)
		}
	}
	// command содержит очередную команду с пустым путём, неверной кодировкой, нулевым байтом,
	// превышением бюджета либо nil-дескриптором.
	for _, command := range []Command{{}, {Binary: "invalid\x00"}, {Binary: "invalid\xff"}, {Binary: "binary", Args: []string{"invalid\x00"}}, {Binary: "binary", Args: []string{"invalid\xff"}}, {Binary: "binary", Args: []string{strings.Repeat("x", l.MaxCommandBytes)}}, {Binary: "binary", ExtraFiles: []*os.File{nil}}} {
		// err содержит ожидаемый отказ проверки недопустимой команды.
		if err := validateCommand(command, l); err == nil {
			t.Fatalf("invalid command accepted: %+v", command)
		}
	}
	// args содержит очередной неполный либо некорректный набор аргументов helper.
	for _, args := range [][]string{nil, {"0"}, {"1", "/tmp", "bad-scope", "00000000"}, {"1", "/tmp", strings.Repeat("a", 32), "invalid!"}} {
		// err содержит ожидаемый ErrInvalid при проверке аргументов helper до запуска процессов.
		if err := RunHelper(args); !errors.Is(err, ErrInvalid) {
			t.Fatalf("helper args=%v %v", args, err)
		}
	}
	// name содержит очередной запрещённый управляемый путь, включая служебный каталог доказательств.
	for _, name := range []string{"", ".", "../escape", "/absolute", "parent/../escape", "bad\x00", "bad\\name", evidenceDirectory, evidenceDirectory + "/inside"} {
		// id содержит файловую идентичность с проверяемым недопустимым путём.
		id := testFileID(name, RegularFile)
		// err содержит ожидаемый ErrInvalid проверки файловой идентичности.
		if err := validateIdentity(id); !errors.Is(err, ErrInvalid) {
			t.Fatalf("path %q accepted: %v", name, err)
		}
	}
	// j содержит изолированный файловый журнал для проверки отсутствующего доказательства и неверных
	// страниц.
	j := testJournal(t, t.TempDir())
	// id обозначает отсутствующий файловый объект без доказательства владения.
	id := testFileID("missing", RegularFile)
	// err содержит ожидаемый ErrUnknown при Verify отсутствующего доказательства.
	if err := j.Verify(context.Background(), id); !errors.Is(err, ErrUnknown) {
		t.Fatalf("missing evidence=%v", err)
	}
	// err содержит ожидаемый ErrUnknown при Remove объекта без доказательства.
	if err := j.Remove(context.Background(), id); !errors.Is(err, ErrUnknown) {
		t.Fatalf("legacy cleanup=%v", err)
	}
	// err содержит ожидаемый ErrInvalid для нечислового курсора подтверждений.
	if _, err := j.RetryAcknowledgements(context.Background(), "invalid", 1); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	// err содержит ожидаемый ErrInvalid для нулевого размера страницы подтверждений.
	if _, err := j.RetryAcknowledgements(context.Background(), "", 0); !errors.Is(err, ErrInvalid) {
		t.Fatal(err)
	}
	// err содержит ожидаемый ErrLimit записи JSON, превышающей заданные 1024 байта.
	if err := writeRecord(j.root, "oversized", strings.Repeat("x", 2048), 1024); !errors.Is(err, ErrLimit) {
		t.Fatal(err)
	}
}
