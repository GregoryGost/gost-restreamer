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

const MaxFileBytes = 1 << 20
const StandardYAMLPath = "/etc/gost-restreamer/config.yaml"
const StandardYMLPath = "/etc/gost-restreamer/config.yml"

type Warning struct{ YAMLPath, YMLPath, Selected string }
type Selection struct {
	Path    string
	Warning *Warning
}

func Select(explicit string) (Selection, error) {
	return SelectPaths(explicit, StandardYAMLPath, StandardYMLPath)
}

// SelectPaths использует Lstat: висячая ссылка остаётся выбранным, но нечитаемым файлом.
func SelectPaths(explicit, yamlPath, ymlPath string) (Selection, error) {
	if explicit != "" {
		return Selection{Path: explicit}, nil
	}
	exists := func(path string) (bool, error) {
		_, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			return false, nil
		}
		return err == nil, err
	}
	first, e := exists(yamlPath)
	if e != nil {
		return Selection{}, fmt.Errorf("%s: %w", yamlPath, e)
	}
	second, e := exists(ymlPath)
	if e != nil {
		return Selection{}, fmt.Errorf("%s: %w", ymlPath, e)
	}
	if first {
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
func LoadSelected(explicit string) (Config, Selection, error) {
	s, err := Select(explicit)
	if err != nil {
		return Config{}, s, err
	}
	c, err := Load(s.Path)
	return c, s, err
}

// Load накладывает единственный строгий документ на собственную копию значений по умолчанию.
func Load(path string) (Config, error) {
	c, err := load(path)
	if err != nil {
		return Config{}, fmt.Errorf("%s: %w", path, err)
	}
	return c, nil
}
func load(path string) (Config, error) {
	f, err := os.OpenFile(path, os.O_RDONLY|syscall.O_NONBLOCK, 0)
	if err != nil {
		return Config{}, err
	}
	info, statErr := f.Stat()
	if statErr != nil {
		return Config{}, errors.Join(statErr, f.Close())
	}
	if !info.Mode().IsRegular() {
		return Config{}, errors.Join(errors.New("конфигурация должна быть обычным файлом"), f.Close())
	}
	data, readErr := io.ReadAll(io.LimitReader(f, MaxFileBytes+1))
	if err = errors.Join(readErr, f.Close()); err != nil {
		return Config{}, err
	}
	if len(data) > MaxFileBytes {
		return Config{}, fmt.Errorf("файл превышает %d байт", MaxFileBytes)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	var document yaml.Node
	if err = decoder.Decode(&document); err != nil {
		return Config{}, fmt.Errorf("YAML: %w", err)
	}
	if document.Kind != yaml.DocumentNode || len(document.Content) != 1 || document.Content[0].Kind != yaml.MappingNode {
		return Config{}, errors.New("нужен один YAML mapping-документ")
	}
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
	c := Default()
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

var durationType = reflect.TypeFor[time.Duration]()

// checkNode исключает неявное преобразование чисел и bool в строки библиотекой YAML.
type nodeType struct {
	node *yaml.Node
	typ  reflect.Type
}

func checkNode(node *yaml.Node, typ reflect.Type, path string, active map[nodeType]bool, depth int) (retErr error) {
	fail := func(reason string) error {
		return fmt.Errorf("поле %s (строка %d): %s", path, node.Line, reason)
	}
	key := nodeType{node, typ}
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
		var level slog.Level
		if err := level.UnmarshalText([]byte(node.Value)); err != nil {
			return fail(err.Error())
		}
		return nil
	}
	if typ == durationType {
		if node.Kind != yaml.ScalarNode || node.Tag != "!!str" {
			return fail("нужна строка длительности с единицей")
		}
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
		fields := map[string]reflect.Type{}
		for f := range typ.Fields() {
			fields[f.Tag.Get("yaml")] = f.Type
		}
		seen := map[string]bool{}
		for i := 0; i < len(node.Content); i += 2 {
			key, value := node.Content[i], node.Content[i+1]
			if key.Tag == "!!merge" {
				if value.Kind == yaml.SequenceNode {
					for _, part := range value.Content {
						if err := checkNode(part, typ, path, active, depth+1); err != nil {
							return err
						}
					}
				} else if err := checkNode(value, typ, path, active, depth+1); err != nil {
					return err
				}
				continue
			}
			if key.Kind != yaml.ScalarNode || key.Tag != "!!str" {
				return fail("ключ должен быть строкой")
			}
			name := key.Value
			if seen[name] {
				return fail("дубликат параметра " + name)
			}
			seen[name] = true
			field, ok := fields[name]
			if !ok {
				return fail("неизвестный параметр " + name)
			}
			child := name
			if path != "" {
				child = path + "." + name
			}
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
