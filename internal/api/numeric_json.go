package api

import (
	"bytes"
	"encoding/json"
	"io"
	"strings"
)

// exactIntegerToken возвращает точную целую запись JSON-числа в объединении диапазонов
// int64 и uint64. Десятичная степень ограничивается длиной входа: даже огромная степень
// не приводит к выделению памяти по её значению. Знак нуля сохраняется, чтобы разбор uint64 отклонил -0.
func exactIntegerToken(token string) (string, bool) {
	if token == "" || (token[0] != '-' && (token[0] < '0' || token[0] > '9')) || !json.Valid([]byte(token)) {
		return "", false
	}
	// negative сохраняет исходный знак; coefficient содержит мантиссу без десятичной степени.
	negative := token[0] == '-'
	coefficient := token
	if negative {
		coefficient = coefficient[1:]
	}
	// exponent содержит ограниченную десятичную степень; exponentText — её исходную запись.
	exponent := 0
	// coefficientPart выделяет мантиссу; exponentText хранит степень, found отмечает разделитель.
	if coefficientPart, exponentText, found := strings.Cut(coefficient, "e"); found {
		coefficient = coefficientPart
		exponent = boundedExponent(exponentText, len(token)+21)
	} else if coefficientPart, exponentText, found := strings.Cut(coefficient, "E"); found {
		// coefficientPart, exponentText и found имеют те же роли для заглавного разделителя E.
		coefficient = coefficientPart
		exponent = boundedExponent(exponentText, len(token)+21)
	}
	// fractionDigits учитывает положение точки; digits хранит только цифры мантиссы.
	fractionDigits := 0
	digits := coefficient
	// point задаёт положение десятичной точки, если мантисса содержит дробную часть.
	if point := strings.IndexByte(coefficient, '.'); point >= 0 {
		fractionDigits = len(coefficient) - point - 1
		digits = coefficient[:point] + coefficient[point+1:]
	}
	digits = strings.TrimLeft(digits, "0")
	if digits == "" {
		if negative {
			return "-0", true
		}
		return "0", true
	}
	// scale задаёт число добавляемых нулей; отрицательная степень требует нулевого хвоста.
	scale := exponent - fractionDigits
	if scale < 0 {
		if -scale >= len(digits) || strings.Trim(digits[len(digits)+scale:], "0") != "" {
			return "", false
		}
		digits = digits[:len(digits)+scale]
		scale = 0
	}
	if len(digits) > 20 || scale > 20-len(digits) {
		return "", false
	}
	digits += strings.Repeat("0", scale)
	// limit содержит максимальную допустимую абсолютную величину с учётом знака.
	limit := "18446744073709551615"
	if negative {
		limit = "9223372036854775808"
	}
	if len(digits) > len(limit) || len(digits) == len(limit) && digits > limit {
		return "", false
	}
	if negative {
		return "-" + digits, true
	}
	return digits, true
}

// boundedExponent разбирает уже проверенную JSON-грамматикой степень, насыщая её
// абсолютную величину на limit. Этого достаточно для сравнения с длиной мантиссы.
func boundedExponent(text string, limit int) int {
	// negative отмечает знак степени; value накапливает её ограниченную величину.
	negative := text[0] == '-'
	value := 0
	if text[0] == '-' || text[0] == '+' {
		text = text[1:]
	}
	// digit содержит очередную десятичную цифру степени.
	for _, digit := range text {
		if value > (limit-int(digit-'0'))/10 {
			value = limit
			break
		}
		value = value*10 + int(digit-'0')
	}
	if negative {
		return -value
	}
	return value
}

// normalizeIntegerTokens копирует корректный JSON, заменяя только математически целые
// числовые лексемы в машинном диапазоне. Строки, экранированные последовательности, ключи,
// повторные свойства и все прочие байты остаются исходными; дроби и переполнения не исправляются.
func normalizeIntegerTokens(data []byte) []byte {
	if !json.Valid(data) {
		return data
	}
	// result собирает копию документа; index указывает ещё не обработанный байт.
	result := make([]byte, 0, len(data))
	for index := 0; index < len(data); {
		// start обозначает начало строки, числа либо одного неизменяемого байта.
		start := index
		switch {
		case data[index] == '"':
			index++
			for index < len(data) {
				if data[index] == '\\' {
					index += 2
					continue
				}
				if data[index] == '"' {
					index++
					break
				}
				index++
			}
			result = append(result, data[start:index]...)
		case data[index] == '-' || data[index] >= '0' && data[index] <= '9':
			for index < len(data) && strings.ContainsRune("0123456789.eE+-", rune(data[index])) {
				index++
			}
			// integer содержит нормализованное целое; whole подтверждает диапазон и целочисленность.
			if integer, whole := exactIntegerToken(string(data[start:index])); whole {
				result = append(result, integer...)
			} else {
				result = append(result, data[start:index]...)
			}
		default:
			result = append(result, data[index])
			index++
		}
	}
	return result
}

// unmarshalJSON разделяет два штатных вызова Huma: предварительная проверка получает
// точные строковые проекции чисел, а типизированный Body — исходные байты с заменой
// только целых числовых лексем. Лишние JSON-значения отклоняются в обоих случаях.
func unmarshalJSON(data []byte, target any) error {
	// generic указывает результат предварительного разбора Huma; ok отличает его от типизированного Body.
	if generic, ok := target.(*any); ok {
		// decoder сохраняет числовые лексемы через UseNumber вместо float64.
		decoder := json.NewDecoder(bytes.NewReader(data))
		decoder.UseNumber()
		if err := decoder.Decode(generic); err != nil { // err содержит отказ JSON-разбора.
			return err
		}
		if err := decoder.Decode(new(any)); err != io.EOF { // err отличает конец тела от второго значения.
			if err == nil {
				return io.ErrUnexpectedEOF
			}
			return err
		}
		*generic = projectNumericTokens(*generic)
		return nil
	}
	return json.Unmarshal(normalizeIntegerTokens(data), target)
}

// projectNumericTokens заменяет только json.Number в предварительном дереве Huma строками.
// Целые лексемы приводятся к той же записи, что используется числовым enum схемы.
func projectNumericTokens(value any) any {
	// typed содержит текущее скалярное значение, объект либо массив предварительного JSON-разбора.
	switch typed := value.(type) {
	case json.Number:
		// integer содержит точную целую запись; whole отличает её от дроби либо переполнения.
		if integer, whole := exactIntegerToken(string(typed)); whole {
			return integer
		}
		return string(typed)
	case map[string]any:
		// key адресует свойство; child содержит его ещё не спроецированное значение.
		for key, child := range typed {
			typed[key] = projectNumericTokens(child)
		}
	case []any:
		// index адресует элемент; child содержит его ещё не спроецированное значение.
		for index, child := range typed {
			typed[index] = projectNumericTokens(child)
		}
	}
	return value
}
