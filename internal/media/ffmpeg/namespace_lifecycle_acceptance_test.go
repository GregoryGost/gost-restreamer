package ffmpeg_test

import (
	"context"
	"errors"
	"fmt"
	"net"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
)

// Создаёт короткий временный каталог для пространств UDS и регистрирует удаление после теста.
func namespaceLifecycleBase(t *testing.T) string {
	t.Helper()
	// parent задаёт короткий временный корень UDS для текущей ОС.
	parent := os.TempDir()
	if runtime.GOOS == "darwin" {
		parent = "/tmp"
	}
	// base содержит отдельный корень проверки Namespace; err сообщает об отказе его создания.
	base, err := os.MkdirTemp(parent, "gnl-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Удаляет принадлежащий тесту временный корень и сообщает ошибку через t.Error.

		// err сообщает об отказе удаления временного корня после теста.
		if err := os.RemoveAll(base); err != nil {
			t.Error(err)
		}
	})
	return base
}

// Создаёт пространство Session под заданным основанием и регистрирует повторно безопасное закрытие
// после теста.
func namespaceLifecycleNew(t *testing.T, base string) *ffmpeg.Namespace {
	t.Helper()
	// n содержит созданный Namespace Session; err сообщает об отказе его создания.
	n, err := ffmpeg.NewNamespace(base, "session")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает созданный Namespace после теста, игнорируя ошибку резервного закрытия.
		_ = n.Close()
	})
	return n
}

// Проверяет отсутствие оставшихся объектов в каталоге после завершения владельцев.
func namespaceLifecycleEmpty(t *testing.T, base string) {
	t.Helper()
	// entries содержит оставшиеся объекты каталога; err сообщает об отказе проверки его пустоты.
	entries, err := os.ReadDir(base)
	if err != nil || len(entries) != 0 {
		t.Fatalf("после последнего Close остались ресурсы: %v, %v", entries, err)
	}
}

// Проверяет наличие обоих путей действующего Namespace и их тип Unix-сокета.
func namespaceLifecycleSockets(t *testing.T, n *ffmpeg.Namespace) {
	t.Helper()
	// path содержит очередной входной или выходной путь действующего Namespace.
	for _, path := range []string{n.InputPath, n.OutputPath} {
		// info содержит метаданные проверяемого endpoint; err сообщает об отказе чтения его
		// типа.
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSocket == 0 {
			t.Fatalf("активный Namespace потерял endpoint %s: %v", path, err)
		}
	}
}

// Проверяет общий каталог Session до последнего Close независимо от порядка создателя и последующее
// повторное создание без прежних удержаний.
func TestNamespaceSharedParentLivesUntilLastClose(t *testing.T) {
	// creatorFirst выбирает закрытие создателя общего каталога до либо после второго пользователя.
	for _, creatorFirst := range []bool{true, false} {
		t.Run(map[bool]string{true: "creator_first", false: "creator_last"}[creatorFirst], func(t *testing.T) {
			// Проверяет сохранение общего родителя до последнего Close при выбранном порядке завершения создателя
			// и второго пользователя.

			// base задаёт общий короткий корень двух Namespace.
			base := namespaceLifecycleBase(t)
			// a содержит Namespace, первоначально создающий каталог Session.
			a := namespaceLifecycleNew(t, base)
			// b содержит второй Namespace, удерживающий тот же каталог Session.
			b := namespaceLifecycleNew(t, base)
			// parent задаёт путь общего каталога Session.
			parent := filepath.Dir(filepath.Dir(a.InputPath))
			// original сохраняет исходную идентичность общего каталога; err сохраняет
			// результаты её чтения и последующей проверки.
			original, err := os.Lstat(parent)
			if err != nil {
				t.Fatal(err)
			}
			// first задаёт Namespace, закрываемый первым; last задаёт оставшегося
			// пользователя общего каталога.
			first, last := a, b
			if !creatorFirst {
				first, last = b, a
			}
			// err сообщает об отказе первого Close выбранного Namespace.
			if err := first.Close(); err != nil {
				t.Fatal(err)
			}
			// err содержит результат повторного Close первого Namespace для проверки
			// неизменности исхода.
			if err := first.Close(); err != nil {
				t.Fatal("повторный Close изменил исход", err)
			}
			// current содержит идентичность общего каталога после первого Close.
			current, err := os.Lstat(parent)
			if err != nil || !os.SameFile(original, current) {
				t.Fatal("первый Close удалил или заменил общий parent", err)
			}
			namespaceLifecycleSockets(t, last)
			// c содержит нового пользователя общего каталога, созданного до закрытия
			// последнего из первоначальной пары.
			c := namespaceLifecycleNew(t, base)
			// err сообщает об отказе закрытия второго первоначального пользователя.
			if err := last.Close(); err != nil {
				t.Fatal(err)
			}
			namespaceLifecycleSockets(t, c)
			// err сообщает об отказе закрытия последнего текущего пользователя общего
			// каталога.
			if err := c.Close(); err != nil {
				t.Fatal(err)
			}
			namespaceLifecycleEmpty(t, base)
			// err содержит результат повторного Close уже закрытого первоначального
			// пользователя.
			if err := last.Close(); err != nil {
				t.Fatal(err)
			}
			// d содержит Namespace, созданный после освобождения всех прежних удержаний.
			d := namespaceLifecycleNew(t, base)
			// err сообщает об отказе закрытия повторно созданного Namespace.
			if err := d.Close(); err != nil {
				t.Fatal("повторное создание после последнего Close", err)
			}
			namespaceLifecycleEmpty(t, base)
		})
	}
}

// Проверяет общий срок жизни физического родителя при обращении через исходный базовый путь и его
// символический алиас.
func TestNamespaceCanonicalBaseSharesParentLifetime(t *testing.T) {
	// base задаёт исходный корень Namespace; aliasRoot содержит каталог символического алиаса.
	base, aliasRoot := namespaceLifecycleBase(t), namespaceLifecycleBase(t)
	// alias задаёт путь символической ссылки на исходный корень.
	alias := filepath.Join(aliasRoot, "alias")
	// err сообщает об отказе создания алиаса базового каталога.
	if err := os.Symlink(base, alias); err != nil {
		t.Fatal(err)
	}
	// a содержит Namespace, созданный через исходный базовый путь.
	a := namespaceLifecycleNew(t, base)
	// b содержит Namespace, созданный через символический алиас того же основания.
	b := namespaceLifecycleNew(t, alias)
	// err сообщает об отказе закрытия пользователя исходного пути.
	if err := a.Close(); err != nil {
		t.Fatal(err)
	}
	namespaceLifecycleSockets(t, b)
	// err сообщает об отказе закрытия последнего пользователя канонического каталога.
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	namespaceLifecycleEmpty(t, base)
}

// Проверяет удержания физического родителя для путей разного регистра как при совпадении объектов, так
// и на регистрозависимой файловой системе.
func TestNamespaceCaseAliasesRespectPhysicalParentLifetime(t *testing.T) {
	// aliasKind выбирает различие регистра в имени Session либо базового каталога.
	for _, aliasKind := range []string{"session_case", "base_case"} {
		// creatorFirst выбирает порядок закрытия пользователей путей разного регистра.
		for _, creatorFirst := range []bool{true, false} {
			// order содержит название выбранного порядка для имени подтеста.
			order := map[bool]string{true: "creator_first", false: "creator_last"}[creatorFirst]
			t.Run(aliasKind+"/"+order, func(t *testing.T) {
				// Проверяет физическую идентичность родителя при выбранном различии регистра путей и порядке закрытия,
				// учитывая поведение текущей файловой системы.

				// root задаёт изолированный корень проверки регистрозависимых путей.
				root := namespaceLifecycleBase(t)
				// baseA задаёт основание первого Namespace; baseB задаёт основание
				// второго с проверяемым различием регистра.
				baseA, baseB := root, root
				// sessionA задаёт имя первой Session; sessionB задаёт имя второй с
				// проверяемым различием регистра.
				sessionA, sessionB := "Session", "session"
				if aliasKind == "base_case" {
					baseA, baseB = filepath.Join(root, "Base"), filepath.Join(root, "base")
					sessionA, sessionB = "session", "session"
				}
				// create создаёт Namespace с указанными основанием и Session и
				// регистрирует его резервное закрытие.
				create := func(base, session string) *ffmpeg.Namespace {
					// Создаёт Namespace с заданными base и session и регистрирует его резервное закрытие.

					t.Helper()
					// n содержит Namespace выбранного регистра пути; err сообщает об
					// отказе его создания.
					n, err := ffmpeg.NewNamespace(base, session)
					if err != nil {
						t.Fatal(err)
					}
					t.Cleanup(func() {
						// Закрывает Namespace, созданный для проверки регистра путей, игнорируя ошибку.
						_ = n.Close()
					})
					return n
				}
				// a содержит первый Namespace; b содержит второй Namespace с выбранным
				// различием регистра.
				a, b := create(baseA, sessionA), create(baseB, sessionB)
				// parentA задаёт каталог Session первого Namespace; parentB задаёт
				// каталог Session второго Namespace.
				parentA, parentB := filepath.Dir(filepath.Dir(a.InputPath)), filepath.Dir(filepath.Dir(b.InputPath))
				// infoA сохраняет идентичность первого родителя; err сохраняет
				// результаты чтения метаданных и проверок сохранности.
				infoA, err := os.Lstat(parentA)
				if err != nil {
					t.Fatal(err)
				}
				// infoB сохраняет идентичность второго родителя для определения общего
				// физического объекта.
				infoB, err := os.Lstat(parentB)
				if err != nil {
					t.Fatal(err)
				}
				// shared показывает, обозначают ли пути разного регистра один физический
				// каталог на текущей файловой системе.
				shared := os.SameFile(infoA, infoB)
				if shared {
					t.Log("разные регистры пути обозначают один физический parent")
				} else {
					t.Log("файловая система различает регистр: проверяются два независимых parent")
				}
				// first задаёт закрываемый первым Namespace; last задаёт оставшийся
				// Namespace; firstParent и lastParent задают их каталоги; lastInfo
				// сохраняет исходную идентичность живого каталога.
				first, last, firstParent, lastParent, lastInfo := a, b, parentA, parentB, infoB
				if !creatorFirst {
					first, last, firstParent, lastParent, lastInfo = b, a, parentB, parentA, infoA
				}
				// err сообщает об отказе закрытия первого пользователя путей разного
				// регистра.
				if err := first.Close(); err != nil {
					t.Fatal(err)
				}
				// current содержит идентичность оставшегося родителя после закрытия
				// первого пользователя.
				current, err := os.Lstat(lastParent)
				if err != nil || !os.SameFile(lastInfo, current) {
					t.Fatal("Close первого пользователя изменил живой parent", err)
				}
				if !shared {
					// err содержит ожидаемый os.ErrNotExist для независимого
					// каталога после закрытия его последнего пользователя.
					if _, err := os.Lstat(firstParent); !errors.Is(err, os.ErrNotExist) {
						t.Fatal("последний пользователь независимого parent не очистил его", err)
					}
				}
				namespaceLifecycleSockets(t, last)
				// err сообщает об отказе закрытия последнего пользователя проверяемых
				// путей.
				if err := last.Close(); err != nil {
					t.Fatal(err)
				}
				namespaceLifecycleEmpty(t, baseA)
				namespaceLifecycleEmpty(t, baseB)
			})
		}
	}
}

// Проверяет сохранение заранее существующего каталога Session и его чужого содержимого после закрытия
// всех пространств.
func TestNamespacePreservesPreexistingParent(t *testing.T) {
	// foreignFile выбирает заранее существующий пустой каталог либо каталог с чужим файлом.
	for _, foreignFile := range []bool{false, true} {
		t.Run(map[bool]string{false: "empty", true: "foreign_child"}[foreignFile], func(t *testing.T) {
			// Проверяет сохранность заранее существовавшего родительского каталога, в том числе с чужим вложенным
			// файлом.

			// base задаёт изолированный корень проверки заранее существовавшего Session.
			base := namespaceLifecycleBase(t)
			// parent задаёт каталог Session, созданный до любых Namespace.
			parent := filepath.Join(base, "session")
			// err сообщает об отказе предварительного создания чужого родительского
			// каталога.
			if err := os.Mkdir(parent, 0700); err != nil {
				t.Fatal(err)
			}
			// original сохраняет исходную идентичность заранее существующего каталога; err
			// сохраняет результаты чтения метаданных и перечисления содержимого.
			original, err := os.Lstat(parent)
			if err != nil {
				t.Fatal(err)
			}
			// marker задаёт путь необязательного чужого файла в заранее существующем
			// каталоге.
			marker := filepath.Join(parent, "foreign")
			if foreignFile {
				// err сообщает об отказе записи чужого содержимого до создания
				// Namespace.
				if err := os.WriteFile(marker, []byte("чужое содержимое"), 0600); err != nil {
					t.Fatal(err)
				}
			}
			// a содержит первый Namespace в чужом родителе; b содержит второго пользователя
			// того же каталога.
			a, b := namespaceLifecycleNew(t, base), namespaceLifecycleNew(t, base)
			// n содержит очередной Namespace, закрываемый перед проверкой сохранности чужого
			// родителя.
			for _, n := range []*ffmpeg.Namespace{a, b} {
				// err сообщает об отказе закрытия пользователя заранее существовавшего
				// каталога.
				if err := n.Close(); err != nil {
					t.Fatal(err)
				}
			}
			// current содержит идентичность чужого родителя после закрытия обоих Namespace.
			current, err := os.Lstat(parent)
			if err != nil || !os.SameFile(original, current) {
				t.Fatal("чужой parent удалён или заменён", err)
			}
			// entries содержит сохранившиеся объекты чужого родителя для проверки
			// неизменности их числа.
			entries, err := os.ReadDir(parent)
			if err != nil || len(entries) != map[bool]int{false: 0, true: 1}[foreignFile] {
				t.Fatal("в чужом parent изменён набор ресурсов", entries, err)
			}
			if foreignFile {
				// data содержит байты чужого файла; err сообщает об отказе чтения при
				// проверке сохранности содержимого.
				data, err := os.ReadFile(marker)
				if err != nil || string(data) != "чужое содержимое" {
					t.Fatal("изменён чужой файл", err)
				}
			}
		})
	}
}

// Проверяет отказ создания и очистки при подмене живого родителя, стабильную ошибку повторного Close и
// освобождение старых удержаний без удаления замены.
func TestNamespaceRejectsReplacedParentAndReleasesFailedOwners(t *testing.T) {
	// base задаёт корень проверки подмены живого родителя.
	base := namespaceLifecycleBase(t)
	// a содержит первого пользователя живого родителя; b содержит второго пользователя того же
	// каталога.
	a, b := namespaceLifecycleNew(t, base), namespaceLifecycleNew(t, base)
	// parent задаёт путь живого каталога Session, который тест подменяет.
	parent := filepath.Join(base, "session")
	// err сообщает об отказе переименования исходного живого каталога перед подменой.
	if err := os.Rename(parent, filepath.Join(base, "original")); err != nil {
		t.Fatal(err)
	}
	// err сообщает об отказе создания замены по прежнему пути Session.
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	// marker задаёт файл нового владельца в заменённом каталоге.
	marker := filepath.Join(parent, "foreign")
	// err сообщает об отказе записи содержимого нового владельца каталога.
	if err := os.WriteFile(marker, []byte("новый владелец"), 0600); err != nil {
		t.Fatal(err)
	}
	// replacement сохраняет идентичность заменённого каталога; err сохраняет результаты её чтения и
	// проверки сохранности.
	replacement, err := os.Lstat(parent)
	if err != nil {
		t.Fatal(err)
	}
	// unexpected содержит недопустимо созданный Namespace в подменённом живом родителе; err содержит
	// ожидаемый ErrResourceOwnership.
	if unexpected, err := ffmpeg.NewNamespace(base, "session"); !errors.Is(err, ffmpeg.ErrResourceOwnership) {
		if unexpected != nil {
			_ = unexpected.Close()
		}
		t.Fatal("подменённый живой parent принят", err)
	}
	// n содержит очередного прежнего пользователя подменённого родителя.
	for _, n := range []*ffmpeg.Namespace{a, b} {
		// first сохраняет первоначальный отказ Close после подмены родителя.
		first := n.Close()
		if !errors.Is(first, ffmpeg.ErrResourceOwnership) {
			t.Fatal("Close не обнаружил подмену parent", first)
		}
		// again содержит повторный результат Close для проверки сохранения той же причины
		// отказа.
		if again := n.Close(); again == nil || again.Error() != first.Error() || !errors.Is(again, ffmpeg.ErrResourceOwnership) {
			t.Fatal("повторный Close изменил сохранённый исход", again, first)
		}
	}
	// После завершения прежних владельцев заменённый каталог становится обычным чужим родителем.
	// c содержит нового пользователя заменённого каталога после освобождения всех прежних удержаний.
	c := namespaceLifecycleNew(t, base)
	// err сообщает об отказе Close нового пользователя заменённого каталога.
	if err := c.Close(); err != nil {
		t.Fatal("ошибочный Close сохранил прежнее удержание", err)
	}
	// current содержит идентичность замены после завершения нового пользователя.
	current, err := os.Lstat(parent)
	if err != nil || !os.SameFile(replacement, current) {
		t.Fatal("Close изменил чужой parent", err)
	}
	// data содержит сохранённые байты нового владельца для проверки отсутствия изменений.
	data, err := os.ReadFile(marker)
	if err != nil || string(data) != "новый владелец" {
		t.Fatal("Close изменил чужое содержимое", err)
	}
}

// Проверяет сохранение сокетов соседнего пространства и чужого файла после отказа очистки подменённого
// сокета.
func TestNamespaceForeignEndpointDoesNotReleaseSibling(t *testing.T) {
	// base задаёт общий корень проверки подмены endpoint одного из соседних Namespace.
	base := namespaceLifecycleBase(t)
	// a содержит Namespace с подменяемым сокетом; b содержит здоровый соседний Namespace.
	a, b := namespaceLifecycleNew(t, base), namespaceLifecycleNew(t, base)
	// err сообщает об отказе Shutdown перед подменой входного endpoint.
	if err := a.Shutdown(); err != nil {
		t.Fatal(err)
	}
	// err сообщает об отказе удаления имени исходного входного сокета.
	if err := os.Remove(a.InputPath); err != nil {
		t.Fatal(err)
	}
	// err сообщает об отказе создания чужого файла вместо входного endpoint.
	if err := os.WriteFile(a.InputPath, []byte("чужой endpoint"), 0600); err != nil {
		t.Fatal(err)
	}
	// first сохраняет отказ очистки Namespace с подменённым сокетом.
	first := a.Close()
	if !errors.Is(first, ffmpeg.ErrResourceOwnership) {
		t.Fatal("подмена endpoint не обнаружена", first)
	}
	// again содержит повторный результат Close для проверки неизменности ошибки подмены.
	if again := a.Close(); again == nil || again.Error() != first.Error() || !errors.Is(again, ffmpeg.ErrResourceOwnership) {
		t.Fatal("повторный Close изменил сохранённый исход", again, first)
	}
	namespaceLifecycleSockets(t, b)
	// err сообщает об отказе Close здорового соседнего Namespace.
	if err := b.Close(); err != nil {
		t.Fatal(err)
	}
	// c содержит новый Namespace после освобождения прежних пользователей общего родителя.
	c := namespaceLifecycleNew(t, base)
	// err сообщает об отказе закрытия вновь созданного Namespace.
	if err := c.Close(); err != nil {
		t.Fatal(err)
	}
	// data содержит байты чужого endpoint после очистки соседей; err сообщает об отказе чтения.
	data, err := os.ReadFile(a.InputPath)
	if err != nil || string(data) != "чужой endpoint" {
		t.Fatal("cleanup удалил или изменил чужой endpoint", err)
	}
}

// Проверяет освобождение созданного родителя при отказе длины UDS и сохранение заранее существующего
// родителя после такого же частичного отказа.
func TestNamespacePartialStartupReleasesParent(t *testing.T) {
	// base намеренно удлиняет корень, чтобы создание Namespace отказало после выделения каталогов.
	base := filepath.Join(namespaceLifecycleBase(t), strings.Repeat("long", 32))
	// n содержит неожиданный частичный результат создания; err содержит ожидаемый отказ длины UDS.
	if n, err := ffmpeg.NewNamespace(base, "session"); !errors.Is(err, ffmpeg.ErrInvalidSupervisor) {
		if n != nil {
			_ = n.Close()
		}
		t.Fatal("ожидался отказ длины UDS после создания каталогов", err)
	}
	namespaceLifecycleEmpty(t, base)
	// parent задаёт каталог Session, создаваемый заранее перед повторной проверкой длинного пути.
	parent := filepath.Join(base, "session")
	// err сообщает об отказе создания заранее существующего родительского каталога.
	if err := os.Mkdir(parent, 0700); err != nil {
		t.Fatal(err)
	}
	// original сохраняет идентичность заранее существующего родителя; err сохраняет результаты
	// чтения и последующей проверки.
	original, err := os.Lstat(parent)
	if err != nil {
		t.Fatal(err)
	}
	// n содержит неожиданный результат повторного создания; err содержит ожидаемый отказ длины без
	// ложной ошибки владения.
	if n, err := ffmpeg.NewNamespace(base, "session"); !errors.Is(err, ffmpeg.ErrInvalidSupervisor) || errors.Is(err, ffmpeg.ErrResourceOwnership) {
		if n != nil {
			_ = n.Close()
		}
		t.Fatal("неудачный startup удержал прежнюю identity", err)
	}
	// current содержит идентичность чужого родителя после частичного отказа создания.
	current, err := os.Lstat(parent)
	if err != nil || !os.SameFile(original, current) {
		t.Fatal("неудачный startup удалил чужой parent", err)
	}
	namespaceLifecycleEmpty(t, parent)
}

// Проверяет конкурентное создание и последний Close без потери нового родителя и сокетов, ожидая
// завершения обеих операций до итоговой очистки.
func TestNamespaceConcurrentCreateAndLastClose(t *testing.T) {
	// base задаёт общий корень конкурентного последнего Close и нового создания Namespace.
	base := namespaceLifecycleBase(t)
	// a содержит единственный исходный Namespace, закрываемый конкурентно с новым созданием.
	a := namespaceLifecycleNew(t, base)
	// start одновременно разрешает обе конкурентные операции.
	start := make(chan struct{})
	// closed передаёт исход конкурентного Close исходного Namespace.
	closed := make(chan error, 1)
	// result объединяет Namespace и ошибку конкурентного создания.
	type result struct {
		// n содержит созданный Namespace либо nil при отказе.
		n *ffmpeg.Namespace
		// err содержит отказ конкурентного создания Namespace.
		err error
	}
	// created передаёт полный результат конкурентного создания.
	created := make(chan result, 1)
	// completed передаёт отдельный сигнал после возврата каждой конкурентной операции и отправки её
	// результата.
	completed := make(chan struct{}, 2)
	go func() {
		// По общему сигналу закрывает последний исходный Namespace и сообщает результат и завершение горутины.

		defer func() {
			// Сообщает через completed о завершении конкурентного Close.
			completed <- struct{}{}
		}()
		<-start
		closed <- a.Close()
	}()
	go func() {
		// По общему сигналу создаёт Namespace того же Session, передаёт его результат и сообщает о завершении
		// горутины.

		defer func() {
			// Сообщает через completed о завершении конкурентного создания Namespace.
			completed <- struct{}{}
		}()
		<-start
		// n содержит конкурентно созданный Namespace; err сообщает об отказе его создания.
		n, err := ffmpeg.NewNamespace(base, "session")
		created <- result{n, err}
	}()
	close(start)
	// joined отмечает, дождался ли тест завершения обеих конкурентных операций.
	joined := false
	// join ожидает два сигнала completed в общем конечном бюджете и сообщает тайм-аут тесту.
	join := func() bool {
		// Ждёт обе конкурентные операции в общем трёхсекундном бюджете; тайм-аут сообщает через t.Error и
		// возвращает false.

		// timer ограничивает присоединение обеих конкурентных операций тремя секундами.
		timer := time.NewTimer(3 * time.Second)
		defer timer.Stop()
		for range 2 {
			select {
			case <-completed:
			case <-timer.C:
				t.Error("конкурентные операции не присоединились")
				return false
			}
		}
		return true
	}
	t.Cleanup(func() {
		// Дожидается конкурентных операций при необходимости и закрывает оставшийся результат создания,
		// игнорируя ошибку Close.

		if !joined && !join() {
			return
		}
		select {
		// r содержит непрочитанный результат создания, который резервная очистка должна закрыть.
		case r := <-created:
			if r.n != nil {
				_ = r.n.Close()
			}
		default:
		}
	})
	select {
	// err содержит исход завершившегося конкурентного Close.
	case err := <-closed:
		if err != nil {
			t.Fatal(err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("последний Close не завершился")
	}
	// r хранит полученный результат конкурентного создания для проверки живых сокетов и последующего
	// закрытия.
	var r result
	select {
	case r = <-created:
	case <-time.After(3 * time.Second):
		t.Fatal("конкурентное создание не завершилось")
	}
	if r.err != nil {
		t.Fatal("создание потеряло parent при последнем Close", r.err)
	}
	t.Cleanup(func() {
		// Закрывает успешно созданный конкурентный Namespace после теста, игнорируя ошибку повторного
		// закрытия.
		_ = r.n.Close()
	})
	namespaceLifecycleSockets(t, r.n)
	// err сообщает об отказе закрытия конкурентно созданного Namespace.
	if err := r.n.Close(); err != nil {
		t.Fatal(err)
	}
	joined = join()
	if joined {
		namespaceLifecycleEmpty(t, base)
	}
}

// Проверяет общий родитель двух Supervisor при последовательных реальных попытках, отсутствие пропуска
// или повтора команд и полную очистку после последнего работника.
func TestSupervisorsShareSessionParentAcrossRealAttempts(t *testing.T) {
	// a содержит первого Supervisor с повторной попыткой; base задаёт общий корень обоих работников.
	a, base := acceptanceSupervisor(t, func(c *ffmpeg.Config) {
		// Задаёт одну повторную попытку и пятисекундный срок запуска первого Supervisor.

		c.MaxRestarts = 1
		c.StartupTimeout = 5 * time.Second
	})
	// b содержит второго Supervisor, использующего тот же BaseDir и бюджет повторов.
	b, _ := acceptanceSupervisor(t, func(c *ffmpeg.Config) {
		// Задаёт второму Supervisor тот же BaseDir, одну повторную попытку и пятисекундный срок запуска.

		c.BaseDir = base
		c.MaxRestarts = 1
		c.StartupTimeout = 5 * time.Second
	})
	// ctx ограничивает общий сценарий десятью секундами; cancel отменяет обоих работников при
	// очистке.
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	// marker задаёт файл, куда настоящие дочерние команды записывают метки выполненных попыток.
	marker := filepath.Join(t.TempDir(), "executed")
	// started передаёт метку работника, достигшего барьера первой команды.
	started := make(chan string, 2)
	// releases содержит отдельный сигнал разрешения первой команды каждого Supervisor.
	releases := [2]chan struct{}{make(chan struct{}), make(chan struct{})}
	// releaseOnce исключает повторное закрытие каждого сигнала разрешения.
	var releaseOnce [2]sync.Once
	// release единожды разрешает выбранному построителю продолжить выполнение.
	release := func(i int) {
		// Освобождает выбранный построитель команды ровно одним закрытием его канала.

		releaseOnce[i].Do(func() {
			// Закрывает канал разрешения выбранного построителя при первом вызове.
			close(releases[i])
		})
	}
	t.Cleanup(func() {
		// Освобождает оба построителя, отменяет общий контекст и ждёт остановки обоих Supervisor с отдельными
		// сроками, сообщая ошибки.

		release(0)
		release(1)
		cancel()
		// supervisor содержит очередного работника, останавливаемого при резервной очистке.
		for _, supervisor := range []*ffmpeg.Supervisor{a, b} {
			// stopCtx ограничивает остановку отдельного Supervisor тремя секундами; stop
			// освобождает таймер ожидания.
			stopCtx, stop := context.WithTimeout(context.Background(), 3*time.Second)
			// err сообщает об отказе резервной остановки одного из Supervisor.
			if err := supervisor.Stop(stopCtx); err != nil {
				t.Error("Supervisor не завершился при очистке", err)
			}
			stop()
		}
	})
	// start регистрирует работника с указанной меткой и ожидает барьера его первой команды.
	start := func(s *ffmpeg.Supervisor, index int, label string) {
		// Запускает выбранный Supervisor и ждёт барьера его первой команды перед продолжением сценария общего
		// родителя.

		t.Helper()
		// worker содержит настройки общей Session и команды, записывающей метку каждой
		// физической попытки.
		worker := ffmpeg.Worker{SessionID: "shared-session", GroupID: label, Owner: newAcceptanceOwner(), Input: acceptanceBlockInput,
			Output: func(context.Context, net.Conn, ffmpeg.Progress) error {
				// Завершает выходной обработчик без чтения: сценарий проверяет жизненный цикл каталогов и реальных
				// команд.
				return nil
			},
			Command: func(commandCtx context.Context, attempt ffmpeg.Attempt) (ffmpeg.Command, error) {
				// Задерживает первую команду выбранного работника до разрешения или отмены; каждая реальная попытка
				// добавляет свою метку в файл и выходит с кодом 23.

				if attempt.ID == 1 {
					started <- label
					select {
					case <-releases[index]:
					case <-commandCtx.Done():
						return ffmpeg.Command{}, commandCtx.Err()
					}
				}
				return ffmpeg.Command{Binary: "/bin/sh", Args: []string{"-c", "printf '%s\\n' \"$1\" >> \"$2\"; exit 23", "namespace-child", fmt.Sprintf("%s/%d", label, attempt.ID), marker}}, nil
			}}
		// err сообщает об отказе регистрации выбранного работника в общем сценарии.
		if err := s.Start(ctx, worker); err != nil {
			t.Fatal(err)
		}
		select {
		// got содержит метку работника, фактически достигшего барьера первой команды.
		case got := <-started:
			if got != label {
				t.Fatal("неверный владелец барьера", got)
			}
		case <-ctx.Done():
			t.Fatal("Supervisor не достиг первой команды", ctx.Err())
		}
	}
	start(a, 0, "creator")
	start(b, 1, "sibling")
	release(0)
	// result содержит итог первого Supervisor для проверки обеих реальных попыток.
	if result := acceptanceWait(t, a); result.Attempts != 2 || !errors.Is(result.Err, ffmpeg.ErrPrematureExit) {
		t.Fatal("создатель не завершил обе реальные попытки", result)
	}
	release(1)
	// result содержит итог второго Supervisor для проверки обеих реальных попыток.
	if result := acceptanceWait(t, b); result.Attempts != 2 || !errors.Is(result.Err, ffmpeg.ErrPrematureExit) {
		t.Fatal("второй Supervisor не завершил обе реальные попытки", result)
	}
	// data содержит метки фактически выполненных команд; err сообщает об отказе чтения контрольного
	// файла.
	data, err := os.ReadFile(marker)
	if err != nil {
		t.Fatal(err)
	}
	// want задаёт точный ожидаемый порядок четырёх физических попыток двух работников.
	want := []string{"creator/1", "creator/2", "sibling/1", "sibling/2"}
	// got содержит фактический список меток из контрольного файла для проверки пропусков и повторов.
	if got := strings.Fields(string(data)); !reflect.DeepEqual(got, want) {
		t.Fatal("реальные команды пропущены или повторены", got, want)
	}
	namespaceLifecycleEmpty(t, base)
}
