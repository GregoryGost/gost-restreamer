package processowner

import (
	"context"
	"errors"
	"path"
)

// RuntimeJournal принадлежит Registry и использует RuntimeRoot как корень путей.
func (s *Scope) RuntimeJournal() *FileJournal { return s.registry.runtime }

// RuntimeIdentity связывает относительный рабочий путь с точными идентификаторами области и её
// сохранённым владельцем. Значение «.» обозначает корень области; недопустимый путь, вид объекта или
// заголовок возвращает ошибку.
func (s *Scope) RuntimeIdentity(relativePath, kind string) (FileIdentity, error) {
	if relativePath != "." && !validManagedPath(relativePath) {
		return FileIdentity{}, ErrInvalid
	}
	// owner содержит сохранённого владельца области; err сохраняет результаты чтения владельца и
	// проверки файловой идентичности.
	owner, err := s.Owner()
	if err != nil {
		return FileIdentity{}, err
	}
	// id содержит точную файловую идентичность объекта внутри пространства данной области.
	id := FileIdentity{FileID: s.id + ":" + relativePath, GenerationID: s.id, ManagedPath: path.Join(s.id, relativePath), Kind: kind, Owner: owner}
	if err = validateIdentity(id); err != nil {
		return FileIdentity{}, err
	}
	return id, nil
}

// RegisterRuntime учитывает созданный объект до передачи его для записи.
// Создание самого объекта должно находиться внутри ранее зарегистрированной операции основного процесса.
func (s *Scope) RegisterRuntime(ctx context.Context, relativePath, kind string) error {
	// id содержит идентичность регистрируемого рабочего объекта; err сохраняет результаты её
	// построения и регистрации операции.
	id, err := s.RuntimeIdentity(relativePath, kind)
	if err != nil {
		return err
	}
	// ticket учитывает регистрацию доказательства временного ресурса до её завершения.
	ticket, err := s.register(ctx, "runtime-resource", nil, &id)
	if err != nil {
		return err
	}
	return errors.Join(s.registry.runtime.Register(ctx, id), ticket.Complete(context.Background()))
}
