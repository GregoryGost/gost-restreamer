package core

import (
	"slices"
	"strconv"
	"strings"
)

// NormalizeCustom проверяет закрытый каталог параметров и возвращает каноническую строку.
func NormalizeCustom(encoder, value string) (string, error) {
	// field указывает поле профиля, к которому относится ошибка пользовательских параметров.
	field := "profile.encoding.video_custom"
	if encoder == AudioEncoderAAC {
		field = "profile.encoding.audio_custom"
	}
	// fail формирует отказ нормализации с кодом, описанием и выбранным полем профиля.
	fail := func(code, detail string) (string, error) {
		// Возвращает пустой результат и ProfileError с выбранным путём поля и доверенными кодом и объяснением
		// отказа.

		return "", &ProfileError{code: code, field: field, detail: detail}
	}
	if len(value) > 4096 {
		return fail("invalid_custom", "Строка параметров не должна превышать 4096 байт.")
	}
	if encoder != VideoEncoderNVENC && encoder != VideoEncoderSVTAV1 && encoder != AudioEncoderAAC {
		return fail("invalid_custom", "Для этого кодировщика каталог пользовательских параметров не определён.")
	}
	// i указывает проверяемый байт исходной строки параметров.
	for i := 0; i < len(value); i++ {
		if value[i] < 32 || value[i] >= 127 || value[i] == 34 || value[i] == 39 || value[i] == 92 {
			return fail("invalid_custom", "Допустимы только скалярные name=value, разделённые ASCII-пробелами; кавычки, экранирование и управляющие символы запрещены.")
		}
	}
	// pairs накапливает проверенные пары для сортировки и канонической записи.
	pairs := make([]string, 0, 32)
	// seen отмечает уже встреченные имена параметров для запрета повторов.
	seen := make(map[string]bool)
	// pair содержит очередную разделённую пробелом пару пользовательского параметра.
	for pair := range strings.SplitSeq(value, " ") {
		if pair == "" {
			continue
		}
		if len(pairs) == 32 {
			return fail("invalid_custom", "Допустимы не более 32 параметров.")
		}
		// name содержит имя параметра, raw — его строковое значение, а ok показывает наличие
		// разделителя равенства.
		name, raw, ok := strings.Cut(pair, "=")
		if !ok || len(name) == 0 || len(name) > 64 || len(raw) == 0 || len(raw) > 128 || strings.Contains(raw, "=") {
			return fail("invalid_custom", "Каждая пара требует имя до 64 байт и одно значение до 128 байт, разделённые одним знаком равенства.")
		}
		// i указывает проверяемый байт имени параметра.
		for i := 0; i < len(name); i++ {
			// c содержит байт имени, проверяемый по разрешённому алфавиту.
			c := name[i]
			// validCharacter показывает, разрешён ли текущий символ имени параметра.
			validCharacter := c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || strings.ContainsRune("_-:.", rune(c))
			if !validCharacter {
				return fail("invalid_custom", "Имя параметра содержит неподдерживаемые символы.")
			}
		}
		if seen[name] {
			return fail("duplicate_custom", "Повтор одного пользовательского параметра запрещён.")
		}
		seen[name] = true
		if ownedCustomOption(name) {
			return fail("custom_conflict", "Параметр принадлежит типизированным настройкам профиля или внутреннему медиарецепту; задайте его обычным полем профиля.")
		}
		switch {
		case encoder == VideoEncoderNVENC && (name == "nonref_p" || name == "strict_gop"):
			// n содержит числовое значение nonref_p или strict_gop, а err — причину отказа
			// его разбора.
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || n < 0 || n > 1 {
				return fail("invalid_custom", "Параметры nonref_p и strict_gop допускают только 0 или 1.")
			}
			raw = strconv.FormatInt(n, 10)
		case encoder == VideoEncoderSVTAV1 && name == "lp":
			// n содержит число потоков lp, а err — причину отказа его целочисленного
			// разбора.
			n, err := strconv.ParseInt(raw, 10, 64)
			if err != nil || n < 0 || n > 6 {
				return fail("invalid_custom", "Параметр lp допускает целое число 0..6.")
			}
			raw = strconv.FormatInt(n, 10)
		case encoder == AudioEncoderAAC && name == "aac_coder":
			if raw != "twoloop" && raw != "fast" {
				return fail("invalid_custom", "Параметр aac_coder допускает twoloop или fast.")
			}
		default:
			return fail("unknown_custom", "Параметр отсутствует в поддерживаемом каталоге выбранного кодировщика.")
		}
		pairs = append(pairs, name+"="+raw)
	}
	slices.Sort(pairs)
	return strings.Join(pairs, " "), nil
}

// ownedCustomOption распознаёт имена и алиасы параметров, принадлежащих типизированному профилю или
// внутреннему медиарецепту; регистр и разделители при сравнении игнорируются.
func ownedCustomOption(name string) bool {
	// key содержит имя параметра в нижнем регистре без разделителей для проверки конфликтующих
	// алиасов.
	key := strings.ToLower(name)
	key = strings.NewReplacer("-", "", "_", "", ":", "", ".", "").Replace(key)
	// Включены имена OBS, FFmpeg и алиасы кодировщиков, чтобы конфликт не превращался в неизвестный параметр.
	return slices.Contains([]string{
		"version", "mode", "encoding", "videoencoder", "audioencoder", "encoder", "codec", "c", "cv", "ca", "vcodec", "acodec",
		"rc", "ratecontrol", "rcmode", "ratemode", "bitrate", "averagebitrate", "targetbitrate", "targetbitratebps", "b", "bv", "ba", "vb", "ab", "tbr", "mbr", "maxbitrate", "maxbitratebps", "maxrate", "maxratev", "maxratea", "minrate", "minratev", "bufsize", "bufsizev", "buffer", "bufferbits", "vbvbufsize", "vbvbuffersize", "vbvmaxrate", "vbvinit", "bufsz", "bufinitialsz", "bufoptimalsz", "crf", "cq", "qp", "q", "qscale", "qv", "qa",
		"g", "gv", "gop", "gopsize", "gopframes", "keyint", "keyintmin", "keyintmax", "intra", "intraperiod", "intraperiodlength", "keyframeinterval", "keyintsec", "bf", "bfv", "bframes", "maxbframes", "closedgop", "opengop", "cgop", "flags", "flagsv", "flags2", "flags2v",
		"profile", "profilev", "profilea", "codecprofile", "level", "levelv", "tier", "tierv", "pixfmt", "pixfmtv", "pixelformat", "bitdepth", "inputdepth", "width", "height", "s", "sv", "size", "r", "rv", "fps", "framerate", "fpsnum", "fpsdenom", "fpnum", "fpden", "timebase", "enctimebase", "enctimebasev", "fpsmode", "fpsmodev", "rescaleoutput",
		"colorrange", "colorrangev", "colorprimaries", "colorprimariesv", "colortrc", "colortrcv", "colorspace", "colorspacev", "primaries", "transfer", "matrix", "range", "colorformat", "fullrange",
		"ar", "ara", "samplerate", "sampleratehz", "ac", "aca", "channels", "channellayout", "channellayouta", "audiobitrate", "samplefmt", "cutoff",
		"preset", "presetv", "encmode", "tune", "tuning", "multipass", "lookahead", "enablelookahead", "lookaheadframes", "rclookahead", "lad", "laginframes", "aq", "enableaq", "aqmode", "aqstrength", "spatialaq", "temporalaq", "enablespatialaq", "enabletemporalaq", "adaptivequantization", "adaptquant",
		"predstruct", "predictionstructure", "hierarchicallevels", "hierarchy", "overlays", "enableoverlays", "dg", "enabledg", "tf", "enabletf", "scd", "scdetection", "scenecut", "noscenecut", "forcedidr", "forcekeyframes", "surfaces", "delay", "zerolatency", "brefmode", "bref", "badapt", "iadapt", "rcinitoccupancy",
		"i", "input", "output", "f", "format", "map", "vf", "af", "filter", "filterv", "filtera", "filtercomplex", "filtercomplexscript", "filterthreads", "filtercomplexthreads", "filterbufferedframes", "path", "filename", "url", "protocolwhitelist", "protocolblacklist", "safe", "y", "n", "stdin", "nostdin", "hidebanner", "loglevel", "progress", "stats", "statperiod", "statsperiod", "pass", "passlogfile", "vstats", "vstatsfile", "statsfile", "threads", "threadtype", "cpuflags", "gpu", "hwaccel", "hwacceloutputformat", "hwacceldevice", "inithwdevice", "filterhwdevice", "svtav1params", "svtav1opts", "x264params", "x264opts", "x265params", "params", "copyts", "avoidnegativets", "movflags", "videotracktimescale", "movietimescale", "useeditlist", "t", "to", "ss", "itsoffset", "streamloop", "loop", "re", "readrate", "shortest", "an", "vn", "sn", "dn", "fflags", "flags2a",
	}, key)
}
