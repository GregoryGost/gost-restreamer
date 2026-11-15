package api

import (
	"encoding/json"
	"fmt"
	"math/big"
	"reflect"
	"strconv"
	"strings"

	"github.com/danielgtaylor/huma/v2"
)

// privateBodySchema создаёт независимую полностью разыменованную схему проверки тела.
// Числовые узлы становятся строками для точной проекции unmarshalJSON; физические
// диапазоны проверяет типизированный декодер, а доменные ограничения остаются у прежних владельцев.
func privateBodySchema(registry huma.Registry, public *huma.Schema) (*huma.Schema, error) {
	// cloned содержит свежую копию всех экспортируемых полей без приватных кэшей Huma;
	// err сообщает о неразрешимой либо циклической ссылке в конечном графе DTO.
	cloned, err := cloneSchemaValue(registry, reflect.ValueOf(public), map[*huma.Schema]bool{})
	if err != nil {
		return nil, err
	}
	return cloned.Interface().(*huma.Schema), nil
}

// cloneSchemaValue копирует схемы и их метаданные в отдельные карты, срезы и указатели.
// У Schema копируются только экспортируемые поля: PrecomputeMessages получает новые
// msgRequired/msgDependentRequired вместо скрытых карт публичного экземпляра.
func cloneSchemaValue(registry huma.Registry, source reflect.Value, stack map[*huma.Schema]bool) (reflect.Value, error) {
	if !source.IsValid() {
		return source, nil
	}
	// result сохраняет тип текущего значения, в том числе допустимые пустые указатели.
	result := reflect.New(source.Type()).Elem()
	if source.Type() == reflect.TypeFor[*huma.Schema]() && !source.IsNil() {
		// schema содержит текущий узел; cloned — свежий экземпляр без скрытого состояния Huma.
		schema := source.Interface().(*huma.Schema)
		if stack[schema] {
			return reflect.Value{}, fmt.Errorf("циклическая схема тела: %w", huma.ErrSchemaInvalid)
		}
		stack[schema] = true
		defer delete(stack, schema)
		// cloned содержит копию экспортируемых полей, включая рекурсивно скопированные дочерние узлы;
		// err сообщает о невозможности скопировать вложенную схему.
		cloned, err := cloneSchemaValue(registry, source.Elem(), stack)
		if err != nil {
			return reflect.Value{}, err
		}
		// target содержит изменяемую внутреннюю схему, на которую публичное дерево не ссылается.
		target := cloned.Addr().Interface().(*huma.Schema)
		if schema.Ref != "" {
			// referenced содержит публичную цель ссылки; resolved — её внутреннюю копию.
			referenced := registry.SchemaFromRef(schema.Ref)
			if referenced == nil {
				return reflect.Value{}, fmt.Errorf("неразрешимая схема тела %s: %w", schema.Ref, huma.ErrSchemaInvalid)
			}
			// resolved содержит полностью разыменованную цель; resolveErr — отказ её копирования.
			resolved, resolveErr := cloneSchemaValue(registry, reflect.ValueOf(referenced), stack)
			if resolveErr != nil {
				return reflect.Value{}, resolveErr
			}
			target.Ref = ""
			target.AllOf = append(target.AllOf, resolved.Interface().(*huma.Schema))
		}
		if target.Type == huma.TypeInteger || target.Type == huma.TypeNumber {
			target.Type = huma.TypeString
			target.Format = ""
			target.Minimum, target.Maximum, target.ExclusiveMinimum, target.ExclusiveMaximum, target.MultipleOf = nil, nil, nil, nil, nil
			// key обозначает числовое ограничение, которое больше не применяется к строковой проекции.
			for _, key := range []string{"minimum", "maximum", "exclusiveMinimum", "exclusiveMaximum", "multipleOf"} {
				delete(target.Extensions, key)
			}
			// index адресует числовой enum; value содержит его точное объявленное значение.
			for index, value := range target.Enum {
				// projected содержит точную строку enum; projectErr обозначает неподдержанный тип метаданных.
				projected, projectErr := numericSchemaString(value)
				if projectErr != nil {
					return reflect.Value{}, projectErr
				}
				target.Enum[index] = projected
			}
			if target.Const != nil {
				target.Const, err = numericSchemaString(target.Const)
				if err != nil {
					return reflect.Value{}, err
				}
			}
		}
		// Дочерние узлы уже подготовили сообщения, включая AdditionalProperties, который Huma не обходит.
		target.PrecomputeMessages()
		return reflect.ValueOf(target), nil
	}
	// kind выбирает способ независимого копирования изменяемых контейнеров и указателей.
	switch kind := source.Kind(); kind {
	case reflect.Interface:
		if !source.IsNil() {
			// child содержит скопированное конкретное значение interface; err — отказ вложенной схемы.
			child, err := cloneSchemaValue(registry, source.Elem(), stack)
			if err != nil {
				return reflect.Value{}, err
			}
			result.Set(child)
		}
	case reflect.Pointer:
		if !source.IsNil() {
			// child содержит отдельную копию значения по указателю; err — отказ вложенной схемы.
			child, err := cloneSchemaValue(registry, source.Elem(), stack)
			if err != nil {
				return reflect.Value{}, err
			}
			result.Set(child.Addr())
		}
	case reflect.Map:
		if !source.IsNil() {
			result.Set(reflect.MakeMapWithSize(source.Type(), source.Len()))
			// iterator перечисляет исходные пары ключей и значений, не изменяя публичную карту.
			iterator := source.MapRange()
			for iterator.Next() {
				// child содержит независимое значение карты; err — отказ вложенной схемы.
				child, err := cloneSchemaValue(registry, iterator.Value(), stack)
				if err != nil {
					return reflect.Value{}, err
				}
				result.SetMapIndex(iterator.Key(), child)
			}
		}
	case reflect.Slice:
		if !source.IsNil() {
			result.Set(reflect.MakeSlice(source.Type(), source.Len(), source.Len()))
			// index адресует очередной элемент копируемого среза.
			for index := 0; index < source.Len(); index++ {
				// child содержит копию элемента; err — отказ вложенной схемы.
				child, err := cloneSchemaValue(registry, source.Index(index), stack)
				if err != nil {
					return reflect.Value{}, err
				}
				result.Index(index).Set(child)
			}
		}
	case reflect.Struct:
		// index адресует поле, информация field исключает скрытые кэши Huma.
		for index := 0; index < source.NumField(); index++ {
			// field описывает доступность текущего поля для чтения через reflect.
			field := source.Type().Field(index)
			if !field.IsExported() {
				continue
			}
			// child содержит копию экспортируемого поля; err — отказ вложенной схемы.
			child, err := cloneSchemaValue(registry, source.Field(index), stack)
			if err != nil {
				return reflect.Value{}, err
			}
			result.Field(index).Set(child)
		}
	default:
		result.Set(source)
	}
	return result, nil
}

// numericSchemaString проецирует точное значение числового enum/const в строку Huma.
// Вещественные метаданные запрещены: их исходная точность уже неизвестна, поэтому enum
// восстанавливаются из исходных лексем тегов до создания внутренней схемы.
func numericSchemaString(value any) (string, error) {
	// number содержит исходную лексему json.Number; ok подтверждает точный источник метаданных.
	if number, ok := value.(json.Number); ok {
		if integer, whole := exactIntegerToken(string(number)); whole { // integer/whole описывают целую проекцию.
			return integer, nil
		}
		return string(number), nil
	}
	// reflected содержит типизированное целое Go без преобразования через float64.
	reflected := reflect.ValueOf(value)
	switch reflected.Kind() {
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		return strconv.FormatInt(reflected.Int(), 10), nil
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		return strconv.FormatUint(reflected.Uint(), 10), nil
	}
	return "", fmt.Errorf("неточные числовые метаданные: %w", huma.ErrSchemaInvalid)
}

// repairPublicNumericSchemas уточняет все зарегистрированные DTO по реальным Go-типам
// перед публикацией. Это также охватывает схемы, используемые только данными SSE.
func repairPublicNumericSchemas(registry huma.Registry) {
	// name адресует компонент; schema содержит его публичное описание.
	for name, schema := range registry.Map() {
		repairPublicNumericSchema(registry, schema, registry.TypeFromRef("#/components/schemas/"+name), "", map[*huma.Schema]bool{})
	}
}

// repairPublicNumericSchema сопоставляет schema с типом DTO и исходными тегами.
// Общий FormatDTO сохраняет нулевые размеры до READY; условные ограничения прежних
// профилей и версии 2 остаются доменными и не сужают общий физический диапазон.
func repairPublicNumericSchema(registry huma.Registry, schema *huma.Schema, typ reflect.Type, tags reflect.StructTag, seen map[*huma.Schema]bool) {
	if schema == nil || typ == nil || seen[schema] {
		return
	}
	seen[schema] = true
	if schema.Ref != "" {
		repairPublicNumericSchema(registry, registry.SchemaFromRef(schema.Ref), registry.TypeFromRef(schema.Ref), tags, seen)
		return
	}
	for typ.Kind() == reflect.Pointer {
		typ = typ.Elem()
	}
	if typ.Kind() == reflect.Struct && typ.NumField() == 3 {
		// valueField содержит тип целевого значения Field; wrapped подтверждает наличие Value.
		if valueField, wrapped := typ.FieldByName("Value"); wrapped {
			typ = valueField.Type
			for typ.Kind() == reflect.Pointer {
				typ = typ.Elem()
			}
		}
	}
	if schema.Type == huma.TypeInteger {
		applyIntegerSchemaBounds(schema, typ, tags)
		return
	}
	// branches содержит ветви объединения схем, которым соответствует тот же Go-тип.
	for _, branches := range [][]*huma.Schema{schema.AnyOf, schema.OneOf, schema.AllOf} {
		// branch содержит очередную ветвь, включая явный null либо строку/объект ProfileText.
		for _, branch := range branches {
			repairPublicNumericSchema(registry, branch, typ, tags, seen)
		}
	}
	switch typ.Kind() {
	case reflect.Struct:
		// index адресует реальное поле DTO для сопоставления с JSON-свойством.
		for index := 0; index < typ.NumField(); index++ {
			// field содержит тип, имя JSON и исходные ограничения свойства.
			field := typ.Field(index)
			// name содержит JSON-имя без omitempty и других опций.
			name, _, _ := strings.Cut(field.Tag.Get("json"), ",")
			if field.Anonymous && name == "" {
				repairPublicNumericSchema(registry, schema, field.Type, "", map[*huma.Schema]bool{})
				continue
			}
			if name == "" {
				name = field.Name
			}
			repairPublicNumericSchema(registry, schema.Properties[name], field.Type, field.Tag, seen)
			if field.Type.Kind() == reflect.Pointer && !strings.Contains(string(field.Tag.Get("json")), "omitempty") && schema.Properties[name] != nil && schema.Properties[name].Type == huma.TypeInteger {
				schema.Properties[name].Nullable = true
			}
		}
	case reflect.Slice, reflect.Array:
		repairPublicNumericSchema(registry, schema.Items, typ.Elem(), "", seen)
	case reflect.Map:
		// additional содержит схему значений словаря; ok отличает её от логического ограничения.
		if additional, ok := schema.AdditionalProperties.(*huma.Schema); ok {
			repairPublicNumericSchema(registry, additional, typ.Elem(), "", seen)
		}
	}
}

// applyIntegerSchemaBounds публикует точные физические границы типа и более узкие
// явно заданные теги. Extensions заменяют ограничения float64 при сериализации Huma.
func applyIntegerSchemaBounds(schema *huma.Schema, typ reflect.Type, tags reflect.StructTag) {
	// minimum/maximum содержат точные десятичные границы без float64.
	var minimum, maximum string
	switch typ.Kind() {
	case reflect.Uint, reflect.Uint8, reflect.Uint16, reflect.Uint32, reflect.Uint64:
		minimum = "0"
		maximum = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), uint(typ.Bits())), big.NewInt(1)).String()
		if typ.Bits() >= 32 {
			schema.Format = "uint" + strconv.Itoa(typ.Bits())
		}
	case reflect.Int, reflect.Int8, reflect.Int16, reflect.Int32, reflect.Int64:
		minimum = new(big.Int).Neg(new(big.Int).Lsh(big.NewInt(1), uint(typ.Bits()-1))).String()
		maximum = new(big.Int).Sub(new(big.Int).Lsh(big.NewInt(1), uint(typ.Bits()-1)), big.NewInt(1)).String()
	default:
		return
	}
	if schema.Extensions == nil {
		schema.Extensions = map[string]any{}
	}
	// key обозначает включительную границу; physical содержит точную границу Go-типа.
	for key, physical := range map[string]string{"minimum": minimum, "maximum": maximum} {
		// bound сохраняет physical либо более узкое исходное значение тега.
		bound := physical
		if declared := tags.Get(key); declared != "" { // declared содержит исходную точную лексему ограничения.
			// tagged содержит границу тега, valid подтверждает её целочисленность,
			// native содержит физическую границу для точного сравнения.
			tagged, valid := new(big.Int).SetString(declared, 10)
			native, _ := new(big.Int).SetString(physical, 10)
			if valid && (key == "minimum" && tagged.Cmp(native) > 0 || key == "maximum" && tagged.Cmp(native) < 0) {
				bound = declared
			}
		}
		schema.Extensions[key] = json.Number(bound)
	}
	if declared := tags.Get("enum"); declared != "" { // declared сохраняет исходные лексемы enum, включая большие целые.
		schema.Enum = nil
		// value содержит очередную точную лексему перечисления из исходного тега.
		for _, value := range strings.Split(declared, ",") {
			schema.Enum = append(schema.Enum, json.Number(strings.TrimSpace(value)))
		}
	}
	if declared := tags.Get("const"); declared != "" { // declared содержит точное значение const из исходного тега.
		schema.Const = json.Number(declared)
	}
}
