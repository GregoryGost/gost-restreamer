package config

import (
	"bytes"
	"errors"
	"fmt"
	"io"
	"log/slog"
	"os"
	"reflect"
	"strings"
	"syscall"
	"time"

	"go.yaml.in/yaml/v3"
)

// MaxFileBytes ограничивает размер единственного файла конфигурации одним MiB; чтение дополнительного
// байта выявляет превышение.
const MaxFileBytes = 1 << 20

// StandardYAMLPath задаёт первый стандартный путь, выбираемый без явного --config.
const StandardYAMLPath = "/etc/gost-restreamer/config.yaml"

// StandardYMLPath задаёт запасной стандартный путь, используемый только при отсутствии StandardYAMLPath.
const StandardYMLPath = "/etc/gost-restreamer/config.yml"

// Warning сохраняет сведения о двух существующих стандартных файлах для обязательного предупреждения о
// выборе.
type Warning struct { /* YAMLPath хранит путь стандартного YAML; YMLPath хранит путь стандартного YML; Selected указывает выбранный из них файл. */
	YAMLPath, YMLPath, Selected string
}

// Selection возвращает выбранный путь вместе с необязательными сведениями о конфликте стандартных
// файлов.
type Selection struct {
	// Path сохраняет точный выбранный путь; явный относительный путь не преобразуется в абсолютный.
	Path string
	// Warning содержит данные предупреждения только при одновременном наличии обоих стандартных
	// файлов.
	Warning *Warning
}

// Select выбирает явный путь либо один из стандартных файлов конфигурации без чтения их содержимого.
func Select(explicit string) (Selection, error) {
	return SelectPaths(explicit, StandardYAMLPath, StandardYMLPath)
}

// SelectPaths использует Lstat: висячая ссылка остаётся выбранным, но нечитаемым файлом.
func SelectPaths(explicit, yamlPath, ymlPath string) (Selection, error) {
	if explicit != "" {
		return Selection{Path: explicit}, nil
	}
	// exists проверяет сам путь через Lstat, сохраняя висячую ссылку среди существующих кандидатов.
	exists := func(path string) (bool, error) {
		// Проверяет существование самого пути через Lstat, чтобы висячая ссылка не приводила к выбору другого
		// файла.

		// err содержит результат Lstat для кандидата конфигурации; отсутствие пути
		// обрабатывается отдельно от остальных отказов.
		_, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return err == nil, err
	}
	// first отмечает наличие пути YAML; e содержит ошибку проверки его существования.
	first, e := exists(yamlPath)
	if e != nil {
		return Selection{}, fmt.Errorf("%s: %w", yamlPath, e)
	}
	// second отмечает наличие стандартного YML, чтобы выбрать его или сообщить о двух существующих
	// файлах.
	second, e := exists(ymlPath)
	if e != nil {
		return Selection{}, fmt.Errorf("%s: %w", ymlPath, e)
	}
	if first {
		// s содержит выбранный YAML и получает предупреждение, если также существует YML.
		s := Selection{Path: yamlPath}
		if second {
			s.Warning = &Warning{yamlPath, ymlPath, yamlPath}
		}
		return s, nil
	}
	if second {
		return Selection{Path: ymlPath}, nil
	}
	return Selection{}, fmt.Errorf("конфигурация отсутствует: %s, %s: %w", yamlPath, ymlPath, os.ErrNotExist)
}

// LoadSelected возвращает выбранный файл вместе с результатом загрузки и предупреждением о приоритете,
// если существуют оба стандартных пути.
func LoadSelected(explicit string) (Config, Selection, error) {
	// s содержит итог выбора файла; err содержит отказ выбора до чтения конфигурации.
	s, err := Select(explicit)
	if err != nil {
		return Config{}, s, err
	}
	// c содержит настройки, прочитанные из пути выбранного файла.
	c, err := Load(s.Path)
	return c, s, err
}

// Load накладывает единственный строгий документ на собственную копию значений по умолчанию.
func Load(path string) (Config, error) {
	// c содержит результат строгой загрузки; err содержит её отказ, к которому добавляется путь
	// файла.
	c, err := load(path)
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}

// load читает ограниченный обычный файл и проверяет единственный YAML-документ до наложения на
// значения по умолчанию. Некорректные типы, null, неизвестные поля и итоговая недопустимая
// конфигурация приводят к ошибке без частичного результата.
func load(path string) (Config, error) {
	// f содержит открытый без ожидания писателя файл конфигурации; err содержит отказ его открытия.
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return Config{}, err
	}
	// info описывает открытый объект для проверки обычного файла; statErr содержит отказ получения
	// этих сведений.
	info, statErr := f.Stat()
	if statErr != nil {
		return Config{}, errors.Join(statErr, f.Close())
	}
	if !info.Mode().IsRegular() {
		return Config{}, errors.Join(errors.New("конфигурация должна быть обычным файлом"), f.Close())
	}
	// data содержит не более MaxFileBytes+1 прочитанных байтов для обнаружения превышения; readErr
	// сохраняет ошибку чтения до закрытия файла.
	data, readErr := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err = errors.Join(readErr, f.Close()); err != nil {
		return Config{}, err
	}
	if len(data) > MaxFileBytes {
		return Config{}, fmt.Errorf("файл превышает %d байт", MaxFileBytes)
	}
	// decoder разбирает исходные байты в YAML-узлы для проверки структуры до декодирования настроек.
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	// document содержит первый YAML-документ, который должен оказаться единственным отображением.
	var document yaml.Node
	if err = decoder.Decode(&document); err != nil {
		return Config{}, fmt.Errorf("YAML: %w", err)
	}
	if document.Kind != yaml.DocumentNode || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return Config{}, errors.New("нужен один YAML mapping-документ")
	}
	// extra принимает возможный второй YAML-документ, наличие которого запрещено.
	var extra yaml.Node
	if err = decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			err = errors.New("второй YAML-документ запрещён")
		}
		return Config{}, err
	}
	if err = checkNode(document.Content[0], reflect.TypeFor[Config](), "", map[nodeType]bool{}, 0); err != nil {
		return Config{}, err
	}
	// c начинает с собственной копии встроенных настроек и получает только явно заданные поля YAML.
	c := Default()
	// strict декодирует проверенный документ в настройки с запрещёнными неизвестными полями.
	strict := yaml.NewDecoder(bytes.NewReader(data))
	strict.KnownFields(true)
	if err = strict.Decode(&c); err != nil {
		return Config{}, err
	}
	if err = c.Validate(); err != nil {
		return Config{}, err
	}
	return c, nil
}

// durationType отличает time.Duration от обычных целых чисел при проверке YAML-скаляров.
var durationType = reflect.TypeFor[time.Duration]()

// nodeType связывает YAML-узел с целевым Go-типом для учёта текущей и завершённой проверки в active.
type nodeType struct {
	// node — конкретный узел документа, включая цель алиаса при его разворачивании.
	node *yaml.Node
	// typ — тип значения конфигурации, с которым сопоставляется этот узел.
	typ reflect.Type
}

// checkNode проверяет YAML-узел по целевому типу до декодирования, запрещая неявное преобразование
// скаляров, null, неизвестные и повторные поля. Учёт пар узла и типа отклоняет циклы и позволяет не
// проверять алиасы повторно.
func checkNode(node *yaml.Node, typ reflect.Type, path string, active map[nodeType]bool, depth int) (retErr error) {
	// fail добавляет к причине отказа путь поля и строку проверяемого YAML-узла.
	fail := func(reason string) error {
		// Добавляет к ошибке проверки YAML путь поля и номер строки текущего узла.

		return fmt.Errorf("поле %s (строка %d): %s", path, node.Line, reason)
	}
	// key обозначает пару узла и целевого типа для обнаружения циклов и повторного использования
	// успешной проверки.
	key := nodeType{node, typ}
	// state показывает незавершённую проверку пары узла и типа; checked отмечает наличие этой пары в
	// журнале обхода.
	if state, checked := active[key]; checked {
		if state {
			return fail("циклическая структура")
		}
		return nil
	}
	if depth > 64 {
		return fail("слишком глубокая или циклическая структура")
	}
	active[key] = true
	defer func() {
		// Запоминает успешную проверку пары узел–тип либо удаляет незавершённую запись после ошибки.

		if retErr == nil {
			active[key] = false
		} else {
			delete(active, key)
		}
	}()
	if node.Kind == yaml.AliasNode {
		return checkNode(node.Alias, typ, path, active, depth+1)
	}
	if node.Tag == "!!null" {
		return fail("null запрещён")
	}
	if typ == reflect.TypeFor[slog.Level]() {
		if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
			return fail("нужна строка уровня журнала")
		}
		// level принимает разобранный уровень журнала, чтобы проверить допустимость его
		// строкового представления.
		var level slog.Level
		// err содержит отказ разбора строкового уровня slog из YAML.
		if err := level.UnmarshalText([]byte(node.Value)); err != nil {
			return fail(err.Error())
		}
		return nil
	}
	if typ == durationType {
		if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
			return fail("нужна строка длительности с единицей")
		}
		// err содержит отказ разбора длительности YAML с единицей времени.
		if _, err := time.ParseDuration(node.Value); err != nil {
			return fail("некорректная длительность: " + err.Error())
		}
		if !strings.ContainsAny(node.Value, "nmuµμsh") {
			return fail("длительность требует единицу")
		}
		return nil
	}
	switch typ.Kind() {
	case reflect.Struct:
		if node.Kind != yaml.MappingNode || node.Tag != "!!map" {
			return fail("нужен mapping")
		}
		// fields сопоставляет допустимые YAML-имена полей структуры их Go-типам.
		fields := map[string]reflect.Type{}
		// f описывает очередное поле целевой Go-структуры, чьи YAML-тег и тип добавляются в
		// fields.
		for f := range typ.Fields() {
			fields[f.Tag.Get("yaml")] = f.Type
		}
		// seen учитывает имена явных ключей текущего отображения для отклонения повторов.
		seen := map[string]bool{}
		// i указывает позицию ключа в последовательности пар key/value текущего
		// YAML-отображения.
		for i := 0; i < len(node.Content); i += 2 {
			// key содержит YAML-узел имени поля; value содержит связанный с ним узел
			// значения.
			key, value := node.Content[i], node.Content[i+1]
			if key.Tag == "!!merge" {
				if value.Kind == yaml.SequenceNode {
					// part содержит очередное отображение из последовательности
					// YAML-слияния.
					for _, part := range value.Content {
						// err содержит отказ проверки очередной части
						// YAML-слияния по тому же Go-типу.
						if err := checkNode(part, typ, path, active, depth+1); err != nil {
							return err
						}
					}
				} else /* err содержит отказ проверки одиночного значения YAML-слияния. */ if err := checkNode(value, typ, path, active, depth+1); err != nil {
					return err
				}
				continue
			}
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
				return fail("ключ должен быть строкой")
			}
			// name содержит строковое имя текущего YAML-параметра для проверки повтора и
			// допустимого поля.
			name := key.Value
			if seen[name] {
				return fail("дубликат параметра " + name)
			}
			seen[name] = true
			// field содержит Go-тип найденного поля; ok отмечает наличие имени YAML в
			// целевой структуре.
			field, ok := fields[name]
			if !ok {
				return fail("неизвестный параметр " + name)
			}
			// child строит полный путь вложенного поля для точной диагностики его значения.
			child := name
			if path != "" {
				child = path + "." + name
			}
			// err содержит отказ проверки значения вложенного поля по найденному Go-типу.
			if err := checkNode(value, field, child, active, depth+1); err != nil {
				return err
			}
		}
	case reflect.String:
		if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
			return fail("нужна строка")
		}
	case reflect.Bool:
		if node.Kind != yaml.ScalarNode || node.Tag != "!!bool" {
			return fail("нужен bool")
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64, reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		if node.Kind != yaml.ScalarNode || node.Tag != "!!int" {
			return fail("нужно целое число")
		}
	default:
		return fail("неподдерживаемый тип")
	}
	return nil
}
