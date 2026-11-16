package codec

import "strconv"

// primaryName переводит код основных цветов v в каноническое имя CICP.
// prefix сохраняет неизвестный числовой код в принятой транспортом форме.
func primaryName(v uint8, prefix string) string {
	switch v {
	case 1:
		return "bt709"
	case 2:
		return "unspecified"
	case 4:
		return "bt470m"
	case 5:
		return "bt470bg"
	case 6:
		return "smpte170m"
	case 7:
		return "smpte240m"
	case 8:
		return "film"
	case 9:
		return "bt2020"
	}
	return prefix + strconv.Itoa(int(v))
}

// cicpTransferName переводит код передаточной характеристики v независимо от таблиц основных
// цветов и матриц. Неизвестный код остаётся числовым с префиксом prefix.
func cicpTransferName(v uint8, prefix string) string {
	switch v {
	case 1:
		return "bt709"
	case 2:
		return "unspecified"
	case 4:
		return "gamma22"
	case 5:
		return "gamma28"
	case 6:
		return "smpte170m"
	case 7:
		return "smpte240m"
	case 8:
		return "linear"
	case 13:
		return "iec61966-2-1"
	case 14:
		return "bt2020-10"
	case 15:
		return "bt2020-12"
	case 16:
		return "smpte2084"
	case 18:
		return "arib-std-b67"
	}
	return prefix + strconv.Itoa(int(v))
}

// cicpMatrixName переводит код матрицы v; код 4 означает fcc, а не основные цвета bt470m.
// Неизвестный код сохраняется с префиксом prefix без подстановки известного цвета.
func cicpMatrixName(v uint8, prefix string) string {
	switch v {
	case 0:
		return "gbr"
	case 1:
		return "bt709"
	case 2:
		return "unspecified"
	case 4:
		return "fcc"
	case 5:
		return "bt470bg"
	case 6:
		return "smpte170m"
	case 7:
		return "smpte240m"
	case 9:
		return "bt2020nc"
	case 10:
		return "bt2020c"
	}
	return prefix + strconv.Itoa(int(v))
}
