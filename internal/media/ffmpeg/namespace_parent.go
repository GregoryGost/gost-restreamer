package ffmpeg

import (
	"errors"
	"fmt"
	"math"
	"os"
	"sync"
	"syscall"
)

// namespaceParent учитывает файловую идентичность и удержания общего каталога Session у неуправляемых
// Namespace.
type namespaceParent struct {
	// path хранит путь, по которому зарегистрирован родительский каталог.
	path string
	// info сохраняет файловую идентичность каталога для защиты от подмены.
	info os.FileInfo
	// users считает активные Namespace, удерживающие этот каталог.
	users uint64
	// created показывает, создан ли каталог здесь и допустима ли попытка удалить его при последнем
	// освобождении.
	created bool
}

// Учёт охватывает только активные Namespace без Scope в этом процессе.
// namespaceParents хранит общий для процесса учёт активных родительских каталогов Namespace без Scope.
var namespaceParents struct {
	// Mutex защищает поиск, создание и освобождение общих удержаний родительских каталогов.
	sync.Mutex
	// active связывает зарегистрированные пути с файловой идентичностью и числом удержаний
	// каталогов.
	active map[string]*namespaceParent
}

// Удерживает проверенную файловую идентичность каталога Session для пространств без Scope;
// существующий каталог проверяется, а алиасы одного объекта разделяют счётчик владельцев.
func acquireNamespaceParent(path string) (*namespaceParent, error) {
	namespaceParents.Lock()
	defer namespaceParents.Unlock()
	// parent содержит уже зарегистрированное удержание по запрошенному пути.
	if parent := namespaceParents.active[path]; parent != nil {
		// info содержит текущую файловую идентичность каталога; err сообщает об отказе её
		// проверки.
		info, err := verifyDirectory(path)
		if err != nil {
			return nil, err
		}
		if !os.SameFile(parent.info, info) {
			return nil, fmt.Errorf("%w: %s", ErrResourceOwnership, path)
		}
		return retainNamespaceParent(parent)
	}
	// created отмечает создание нового каталога этим вызовом для последующего безопасного
	// освобождения.
	created := false
	// err содержит результат создания родительского каталога с ограниченными правами.
	if err := os.Mkdir(path, 0700); err == nil {
		created = true
	} else if !errors.Is(err, os.ErrExist) {
		return nil, err
	}
	// info содержит проверенную идентичность запрошенного каталога; err сообщает об отказе проверки
	// после создания или обнаружения существующего каталога.
	info, err := verifyDirectory(path)
	if err != nil {
		return nil, err
	}
	// parent содержит очередной зарегистрированный каталог при поиске алиаса того же файлового
	// объекта.
	for _, parent := range namespaceParents.active {
		if os.SameFile(parent.info, info) {
			// err сообщает о подмене или недоступности ранее зарегистрированного пути
			// каталога.
			if err := sameDirectory(parent.path, parent.info); err != nil {
				return nil, err
			}
			return retainNamespaceParent(parent)
		}
	}
	// parent содержит новое удержание каталога с первоначальным пользователем.
	parent := &namespaceParent{path: path, info: info, users: 1, created: created}
	if namespaceParents.active == nil {
		namespaceParents.active = make(map[string]*namespaceParent)
	}
	namespaceParents.active[path] = parent
	return parent, nil
}

// Удержание добавляется только под блокировкой namespaceParents.
func retainNamespaceParent(parent *namespaceParent) (*namespaceParent, error) {
	if parent.users == math.MaxUint64 {
		return nil, fmt.Errorf("%w: превышен предел владельцев каталога", ErrInvalidSupervisor)
	}
	parent.users++
	return parent, nil
}

// Освобождает удержание родителя; последний владелец удаляет только созданный здесь пустой каталог,
// сохраняя чужие дочерние ресурсы.
func releaseNamespaceParent(parent *namespaceParent) error {
	if parent == nil {
		return nil
	}
	namespaceParents.Lock()
	defer namespaceParents.Unlock()
	parent.users--
	if parent.users != 0 {
		return nil
	}
	delete(namespaceParents.active, parent.path)
	if len(namespaceParents.active) == 0 {
		namespaceParents.active = nil
	}
	if parent.created {
		// Чужие дочерние ресурсы сохраняются вместе с непустым родителем.
		// err сообщает об отказе удаления созданного здесь родительского каталога; непустой
		// каталог сохраняется.
		if err := removeOwned(parent.path, parent.info); err != nil && !errors.Is(err, syscall.ENOTEMPTY) && !errors.Is(err, syscall.EEXIST) {
			return err
		}
	}
	return nil
}
