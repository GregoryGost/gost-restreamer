package storage

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"slices"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// migrationVideoMetadata сохраняет параметры видеодорожки, представляя строковые параметры точными байтами.
type migrationVideoMetadata struct {
	// Codec указывает кодек сжатой дорожки.
	Codec core.Codec
	// Width содержит ширину видеокадра в пикселях.
	Width uint32
	// Height содержит высоту видеокадра в пикселях.
	Height uint32
	// FrameRate задаёт частоту видеокадров точной положительной дробью.
	FrameRate core.FrameRate
	// CodecProfile сохраняет точные байты параметра CodecProfile без нормализации текста.
	CodecProfile []byte
	// Level сохраняет точные байты параметра Level без нормализации текста.
	Level []byte
	// Tier сохраняет точные байты параметра Tier без нормализации текста.
	Tier []byte
	// PixelFormat сохраняет точные байты параметра PixelFormat без нормализации текста.
	PixelFormat []byte
	// BitDepth содержит глубину компонента пикселя в битах.
	BitDepth uint8
	// Range сохраняет точные байты параметра Range без нормализации текста.
	Range []byte
	// Primaries сохраняет точные байты параметра Primaries без нормализации текста.
	Primaries []byte
	// Transfer сохраняет точные байты параметра Transfer без нормализации текста.
	Transfer []byte
	// Matrix сохраняет точные байты параметра Matrix без нормализации текста.
	Matrix []byte
}

// migrationAudioMetadata сохраняет параметры аудиодорожки с точными байтами строковых параметров.
type migrationAudioMetadata struct {
	// Codec указывает кодек сжатой дорожки.
	Codec core.Codec
	// CodecProfile сохраняет точные байты параметра CodecProfile без нормализации текста.
	CodecProfile []byte
	// SampleRateHz содержит частоту дискретизации аудио в герцах.
	SampleRateHz uint32
	// Channels содержит число аудиоканалов.
	Channels uint16
	// ChannelLayout сохраняет точные байты параметра ChannelLayout без нормализации текста.
	ChannelLayout []byte
}

// migrationFormatMetadata объединяет версию формата и сохраняемые параметры обеих дорожек.
type migrationFormatMetadata struct {
	// Version содержит версию сохраняемого представления.
	Version int
	// Video содержит описание обязательной видеодорожки.
	Video migrationVideoMetadata
	// Audio содержит описание обязательной аудиодорожки.
	Audio migrationAudioMetadata
}

// migrationProfileMetadataV1 сохраняет точный порядок и типы полей прежнего формата хранения.
type migrationProfileMetadataV1 struct {
	// Version содержит версию сохраняемого представления.
	Version int
	// Mode сохраняет режим обработки LIVE-видео группы.
	Mode core.GroupMode
	// Format содержит версионированное описание обеих дорожек.
	Format migrationFormatMetadata
	// VideoEncoding сохраняет параметры видеокодирования.
	VideoEncoding migrationVideoEncodingV1
	// AudioEncoding сохраняет параметры аудиокодирования.
	AudioEncoding migrationAudioEncodingV1
}

// migrationVideoEncodingV1 сохраняет прежний порядок и типы полей видеокодирования профиля v1.
type migrationVideoEncodingV1 struct {
	// TargetBitrateBPS содержит целевой видеобитрейт в битах в секунду.
	TargetBitrateBPS uint64
	// MaxBitrateBPS содержит верхний предел видеобитрейта в битах в секунду.
	MaxBitrateBPS uint64
	// BufferBits содержит размер буфера управления битрейтом в битах.
	BufferBits uint64
	// GOPFrames содержит длину GOP в кадрах.
	GOPFrames uint32
	// MaxBFrames содержит максимальное число B-кадров.
	MaxBFrames uint32
	// ClosedGOP указывает требование закрытого GOP.
	ClosedGOP bool
}

// migrationAudioEncodingV1 сохраняет прежнее представление аудиобитрейта профиля v1.
type migrationAudioEncodingV1 struct { /* BitrateBPS содержит аудиобитрейт в битах в секунду. */
	BitrateBPS uint64
}

// migrationProfileMetadataV3 сохраняет профиль v3 с явным режимом LIVE-аудио и допускающим null блоком
// кодировщиков.
type migrationProfileMetadataV3 struct {
	// Version содержит версию сохраняемого представления.
	Version int
	// Mode сохраняет режим обработки LIVE-видео группы.
	Mode core.GroupMode
	// LiveAudioMode сохраняет явно выбранный режим обработки LIVE-аудио.
	LiveAudioMode core.LiveAudioMode
	// Format содержит версионированное описание обеих дорожек.
	Format migrationFormatMetadata
	// VideoEncoding сохраняет параметры видеокодирования.
	VideoEncoding core.VideoEncoding
	// AudioEncoding сохраняет параметры аудиокодирования.
	AudioEncoding core.AudioEncoding
	// Encoding сохраняет настройки кодировщиков либо явный null для прежнего профиля.
	Encoding *core.EncodingSettings
}

// decodeMigrationJSON читает в value единственный JSON из data и отклоняет неизвестные поля.
func decodeMigrationJSON(data []byte, value any) error {
	// decoder проверяет полную форму сохраняемого объекта.
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	// err содержит ошибку основного объекта.
	if err := decoder.Decode(value); err != nil {
		return err
	}
	// err отличает конец данных от второго значения.
	if err := decoder.Decode(new(any)); err != io.EOF {
		return core.ErrCorrupt
	}
	return nil
}

// value восстанавливает структурный формат d без применения строгих правил желаемых настроек.
func (d migrationFormatMetadata) value() core.MediaFormat {
	// v и a содержат сохранённые дорожки, включая точные байты текста.
	v, a := d.Video, d.Audio
	return core.MediaFormat{Video: core.VideoFormat{Codec: v.Codec, Width: v.Width, Height: v.Height, FrameRate: v.FrameRate, CodecProfile: string(v.CodecProfile), Level: string(v.Level), Tier: string(v.Tier), PixelFormat: string(v.PixelFormat), BitDepth: v.BitDepth, Color: core.ColorInfo{Range: string(v.Range), Primaries: string(v.Primaries), Transfer: string(v.Transfer), Matrix: string(v.Matrix)}}, Audio: core.AudioFormat{Codec: a.Codec, CodecProfile: string(a.CodecProfile), SampleRateHz: a.SampleRateHz, Channels: a.Channels, ChannelLayout: string(a.ChannelLayout)}}
}

// validateMigrationProfileV3Presence сохраняет требования прежнего декодера к наличию полей до того,
// как преобразование может добавить отсутствующие пустые значения. При Encoding:null проверяет всю
// структуру профиля, допускавшую этот блок кодировщиков. data содержит исходное представление v3;
// ошибка сообщает о нарушении прежней обязательности полей без изменения данных.
func validateMigrationProfileV3Presence(data []byte) error {
	// root содержит поля исходного профиля до преобразования в типизированную структуру,
	// а err сообщает об ошибке их разбора или обязательности.
	root, err := migrationRequiredObject(data, true, "Version", "Mode", "LiveAudioMode", "Format", "VideoEncoding", "AudioEncoding")
	if err != nil {
		return err
	}
	// encoding сохраняет явное значение блока, в том числе разрешённый null;
	// ok показывает присутствие этого обязательного поля.
	encoding, ok := root["Encoding"]
	if !ok {
		return fmt.Errorf("%w: отсутствует материализованное поле Encoding", core.ErrInvalidMetadata)
	}
	if !bytes.Equal(bytes.TrimSpace(encoding), []byte("null")) {
		return nil
	}
	// err сообщает об отсутствующих либо null-полях прежних блоков видеокодирования.
	if _, err := migrationRequiredObject(root["VideoEncoding"], true, "TargetBitrateBPS", "MaxBitrateBPS", "BufferBits", "GOPFrames", "MaxBFrames", "ClosedGOP"); err != nil {
		return err
	}
	// err сообщает об отсутствии прежнего аудиобитрейта.
	if _, err := migrationRequiredObject(root["AudioEncoding"], true, "BitrateBPS"); err != nil {
		return err
	}
	// format содержит обязательные секции исходного медиаформата.
	format, err := migrationRequiredObject(root["Format"], true, "Version", "Video", "Audio")
	if err != nil {
		return err
	}
	// video содержит все прежние поля описания видеодорожки, включая явно пустые строки.
	video, err := migrationRequiredObject(format["Video"], false, "Codec", "Width", "Height", "FrameRate", "CodecProfile", "Level", "Tier", "PixelFormat", "BitDepth", "Range", "Primaries", "Transfer", "Matrix")
	if err != nil {
		return err
	}
	// err сообщает об отсутствии числителя или знаменателя прежней частоты кадров.
	if _, err := migrationRequiredObject(video["FrameRate"], true, "Numerator", "Denominator"); err != nil {
		return err
	}
	// err сообщает об отсутствии обязательного поля аудиодорожки.
	_, err = migrationRequiredObject(format["Audio"], false, "Codec", "CodecProfile", "SampleRateHz", "Channels", "ChannelLayout")
	return err
}

// migrationRequiredObject разбирает JSON-объект data и проверяет наличие полей fields;
// nonNull дополнительно требует, чтобы их значения не были null. Возвращает исходные значения
// полей либо ошибку структуры без назначения значений по умолчанию.
func migrationRequiredObject(data []byte, nonNull bool, fields ...string) (map[string]json.RawMessage, error) {
	// values сопоставляет поля объекта с их исходными JSON-значениями.
	var values map[string]json.RawMessage
	// err содержит причину отказа разбора объекта.
	if err := json.Unmarshal(data, &values); err != nil {
		return nil, err
	}
	// field содержит очередное имя обязательного поля.
	for _, field := range fields {
		// value содержит исходное значение поля, а ok показывает, присутствует ли оно.
		value, ok := values[field]
		if !ok || nonNull && bytes.Equal(bytes.TrimSpace(value), []byte("null")) {
			return nil, fmt.Errorf("%w: отсутствует материализованное поле %s", core.ErrInvalidMetadata, field)
		}
	}
	return values, nil
}

// migrationProfile читает прежний профиль data и проверяет старый контракт. Результат oldCanonical
// нужен для сверки существующих ключей до замены. Ненулевой Encoding уже имел закрытую матрицу,
// поэтому его ошибки исправлению не подлежат. replacement сохраняет прежнюю версию и числа; changed
// сообщает о замене открытых строк. retErr содержит отказ старого или нового контракта.
func migrationProfile(data []byte) (replacement []byte, oldCanonical []byte, changed bool, retErr error) {
	// header выбирает известную сохраняемую версию профиля.
	var header struct { /* Version содержит версию оболочки профиля. */
		Version int
	}
	if retErr = json.Unmarshal(data, &header); retErr != nil {
		return
	}
	// legacy содержит общий прежний профиль для структурной проверки и ремонта.
	var legacy migrationProfileMetadataV1
	// explicit сохраняет прежнюю оболочку с явным режимом LIVE-аудио.
	var explicit migrationProfileMetadataV3
	switch header.Version {
	case 1:
		retErr = decodeMigrationJSON(data, &legacy)
	case 2:
		// profile проверяет весь прежний v2: его правила уже исключали ремонтируемые строки.
		var profile core.GroupProfile
		profile, retErr = core.DecodeProfile(data)
		if retErr == nil {
			_, oldCanonical, retErr = core.ProfileIdentity(profile)
		}
		replacement = data
		return
	case 3:
		if retErr = validateMigrationProfileV3Presence(data); retErr != nil {
			return
		}
		retErr = decodeMigrationJSON(data, &explicit)
		if retErr != nil {
			return
		}
		if explicit.LiveAudioMode != core.LiveAudioCopy && explicit.LiveAudioMode != core.LiveAudioTranscode {
			retErr = core.ErrCorrupt
			return
		}
		if explicit.Encoding != nil {
			// profile сохраняет закрытый профиль без миграционных допущений.
			var profile core.GroupProfile
			profile, retErr = core.DecodeProfile(data)
			if retErr == nil {
				_, oldCanonical, retErr = core.ProfileIdentity(profile)
			}
			replacement = data
			return
		}
		legacy = migrationProfileMetadataV1{Version: 1, Mode: explicit.Mode, Format: explicit.Format, VideoEncoding: migrationVideoEncodingV1(explicit.VideoEncoding), AudioEncoding: migrationAudioEncodingV1(explicit.AudioEncoding)}
	default:
		retErr = core.ErrCorrupt
		return
	}
	if retErr != nil {
		return
	}
	if legacy.Version != 1 || legacy.Format.Version != 1 || legacy.Mode != core.ModeCopy && legacy.Mode != core.ModeTranscode || explicit.LiveAudioMode == core.LiveAudioTranscode && legacy.Mode != core.ModeTranscode {
		retErr = core.ErrCorrupt
		return
	}
	// format содержит исходные дорожки для проверки независимых чисел и ранее обязательного текста.
	format := legacy.Format.value()
	if retErr = format.Validate(); retErr != nil {
		return
	}
	if retErr = core.VideoEncoding(legacy.VideoEncoding).Validate(); retErr != nil {
		return
	}
	if retErr = core.AudioEncoding(legacy.AudioEncoding).Validate(); retErr != nil {
		return
	}
	// canonical сохраняет исходную оболочку с сокращённой частотой кадров.
	canonical := legacy
	// field выбирает каждое байтовое поле для прежней нормализации null в пустую строку.
	for _, field := range []*[]byte{&canonical.Format.Video.CodecProfile, &canonical.Format.Video.Level, &canonical.Format.Video.Tier, &canonical.Format.Video.PixelFormat, &canonical.Format.Video.Range, &canonical.Format.Video.Primaries, &canonical.Format.Video.Transfer, &canonical.Format.Video.Matrix, &canonical.Format.Audio.CodecProfile, &canonical.Format.Audio.ChannelLayout} {
		if *field == nil {
			*field = []byte{}
		}
	}
	// numerator и denominator вычисляют НОД без округления FPS.
	numerator, denominator := canonical.Format.Video.FrameRate.Numerator, canonical.Format.Video.FrameRate.Denominator
	for denominator != 0 {
		numerator, denominator = denominator, numerator%denominator
	}
	canonical.Format.Video.FrameRate.Numerator /= numerator
	canonical.Format.Video.FrameRate.Denominator /= numerator
	if header.Version == 3 {
		explicit.Format = canonical.Format
		oldCanonical, retErr = json.Marshal(explicit)
	} else {
		oldCanonical, retErr = json.Marshal(canonical)
	}
	if retErr != nil {
		return
	}
	// original содержит исходные поля профиля для точечного обновления без изменения чисел и версии.
	var original map[string]json.RawMessage
	if retErr = json.Unmarshal(data, &original); retErr != nil {
		return
	}
	// v и a ссылаются на ремонтируемые дорожки локальной копии.
	v, a := &legacy.Format.Video, &legacy.Format.Audio
	if legacy.Mode == core.ModeCopy {
		v.Codec = core.CodecAV1
	} else {
		v.Codec = core.CodecH264
	}
	if v.Codec == core.CodecAV1 {
		if string(v.CodecProfile) != "main" {
			v.CodecProfile = []byte("main")
		}
		if string(v.Tier) != "" && string(v.Tier) != "main" {
			v.Tier = []byte{}
		}
		// level содержит прежнее обозначение AV1 для проверки конечного набора.
		level := string(v.Level)
		if level != "" && level != "auto" && (len(level) != 3 || level[0] < '2' || level[0] > '7' || level[1] != '.' || level[2] < '0' || level[2] > '3') {
			v.Level = []byte{}
		}
	} else {
		if !slices.Contains([]string{"baseline", "main", "high"}, string(v.CodecProfile)) || string(v.CodecProfile) == "baseline" && legacy.VideoEncoding.MaxBFrames > 0 {
			v.CodecProfile = []byte("high")
		}
		if len(v.Tier) > 0 {
			v.Tier = []byte{}
		}
		if !slices.Contains([]string{"", "auto", "1", "1.0", "1b", "1.0b", "1.1", "1.2", "1.3", "2", "2.0", "2.1", "2.2", "3", "3.0", "3.1", "3.2", "4", "4.0", "4.1", "4.2", "5", "5.0", "5.1", "5.2"}, string(v.Level)) {
			v.Level = []byte{}
		}
	}
	if string(v.PixelFormat) != "yuv420p" || v.BitDepth != 8 {
		v.PixelFormat = []byte("yuv420p")
		v.BitDepth = 8
	}
	if !slices.Contains([]string{"limited", "full", "tv", "pc"}, string(v.Range)) {
		v.Range = []byte("limited")
	}
	if string(v.Primaries) != "bt709" {
		v.Primaries = []byte("bt709")
	}
	if string(v.Transfer) != "bt709" {
		v.Transfer = []byte("bt709")
	}
	if string(v.Matrix) != "bt709" {
		v.Matrix = []byte("bt709")
	}
	if string(a.CodecProfile) != "LC" {
		a.CodecProfile = []byte("LC")
	}
	if a.Channels != 1 && a.Channels != 2 {
		a.Channels = 2
	}
	if a.Channels == 1 {
		a.ChannelLayout = []byte("mono")
	} else {
		a.ChannelLayout = []byte("stereo")
	}
	// repairedFormat содержит изменённую часть исходной оболочки, err сообщает отказ кодирования.
	repairedFormat, err := json.Marshal(legacy.Format)
	if err != nil {
		retErr = err
		return
	}
	// unchanged сравнивает смысл полей без учёта оформления исходного JSON.
	unchanged := format == legacy.Format.value()
	if unchanged {
		replacement = data
		return
	}
	original["Format"] = repairedFormat
	replacement, retErr = json.Marshal(original)
	if retErr != nil {
		return
	}
	// Строгий декодер проверяет результат единственной разрешённой замены.
	_, retErr = core.DecodeProfile(replacement)
	changed = retErr == nil
	return
}

// migrationIdentity проверяет key и canonical контейнера object по исходным байтам oldCanonical.
// Эти поля обязательны для требований поколения, вариантов и манифестов.
func migrationIdentity(object map[string]json.RawMessage, oldCanonical []byte) error {
	// key и canonical принимают сохраняемую идентичность до преобразования.
	var key string
	var canonical []byte
	// err содержит нарушение типа сохраняемого ключа.
	if err := json.Unmarshal(object["ProfileKey"], &key); err != nil {
		return err
	}
	// err содержит нарушение типа сохраняемых канонических байтов.
	if err := json.Unmarshal(object["CanonicalProfile"], &canonical); err != nil {
		return err
	}
	// hash представляет вычисленную исходную идентичность.
	hash := sha256.Sum256(oldCanonical)
	if key != hex.EncodeToString(hash[:]) || !bytes.Equal(canonical, oldCanonical) {
		return fmt.Errorf("%w: идентичность прежнего профиля", core.ErrCorrupt)
	}
	return nil
}
