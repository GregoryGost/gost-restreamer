package fallback

import (
	"bytes"
	"context"
	"encoding/binary"
	"io"
	"math"
	"os"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
)

// sourceVideoEntry хранит фактическую запись кодека и nclx одной выбранной видеодорожки MOV/MP4.
// Значения нужны лишь во время проверки файла; SourceFormat и история не расширяются.
type sourceVideoEntry struct {
	// headers сохраняет исходную запись кодека без контейнерного FullBox VP9.
	headers []byte
	// color содержит CICP и диапазон реально прочитанного nclx; при его отсутствии значение нулевое.
	color core.ColorInfo
	// provenance подтверждает наличие трёх внешних полей без копии значений в HDR времени выполнения.
	provenance media.ColorConfigProvenance
}

// sourceVideoDescription читает один ограниченный moov и фиксированный путь до выбранной видеозаписи.
// ctx отменяет обход верхних блоков; file читается через ReadAt без сканирования сжатого mdat.
// kind задаёт уже наблюдаемый кодек, а существующие бюджеты ограничивают moov и запись кодека.
func (p Prober) sourceVideoDescription(ctx context.Context, file *os.File, kind core.Codec) (sourceVideoEntry, error) {
	// status содержит размер исходника для проверки смещений верхнего уровня, err — отказ Stat.
	status, err := file.Stat()
	if err != nil {
		return sourceVideoEntry{}, err
	}
	// offset продвигается по размерам блоков верхнего уровня; содержимое больших mdat не читается.
	for offset := int64(0); offset < status.Size(); {
		if err = ctx.Err(); err != nil {
			return sourceVideoEntry{}, err
		}
		// header содержит минимальный заголовок блока и место для расширенного 64-битного размера.
		var header [16]byte
		if _, err = file.ReadAt(header[:8], offset); err != nil {
			return sourceVideoEntry{}, err
		}
		// size и headerSize задают полный размер блока и длину его заголовка в байтах.
		size, headerSize := uint64(binary.BigEndian.Uint32(header[:4])), uint64(8)
		switch size {
		case 1:
			if _, err = file.ReadAt(header[8:], offset+8); err != nil {
				return sourceVideoEntry{}, err
			}
			size, headerSize = binary.BigEndian.Uint64(header[8:]), 16
		case 0:
			size = uint64(status.Size() - offset)
		}
		if size < headerSize || size > uint64(status.Size()-offset) || size > math.MaxInt64 {
			return sourceVideoEntry{}, ErrInvalid
		}
		if string(header[4:8]) == "moov" {
			if size > p.Limits.Container.MaxInitBytes || size > p.Limits.Container.MaxBoxBytes {
				return sourceVideoEntry{}, ErrLimit
			}
			// moov содержит только ограниченные метаданные, без сжатой нагрузки mdat.
			moov := make([]byte, size-headerSize)
			if _, err = file.ReadAt(moov, offset+int64(headerSize)); err != nil {
				return sourceVideoEntry{}, err
			}
			return sourceVideoRecord(moov, kind, p.Limits.Container.MaxExtradataBytes)
		}
		offset += int64(size)
	}
	return sourceVideoEntry{}, io.ErrUnexpectedEOF
}

// sourceVP9Record сохраняет прежний узкий вход для разбора исходного vpcC без FullBox.
// moov и maximum задают ограниченное тело контейнера и предельный размер записи кодека.
func sourceVP9Record(moov []byte, maximum uint64) ([]byte, error) {
	// description содержит одну проверенную VP9 запись; err сохраняет исходный отказ её разбора.
	description, err := sourceVideoRecord(moov, core.CodecVP9, maximum)
	return description.headers, err
}

// sourceVideoRecord проходит фиксированный moov/trak/mdia/minf/stbl/stsd путь до записи kind.
// maximum ограничивает запись кодека; несколько подходящих записей, неверный nclx и противоречивое
// обрамление явно отклоняются. Метод не импортирует временные таблицы или сжатые пакеты.
func sourceVideoRecord(moov []byte, kind core.Codec, maximum uint64) (sourceVideoEntry, error) {
	// parents содержит содержимое блоков текущего уровня фиксированного пути.
	parents := [][]byte{moov}
	// level задаёт следующий обязательный уровень до stsd.
	for _, level := range []string{"trak", "mdia", "minf", "stbl", "stsd"} {
		// next собирает дочерние блоки в пределах исходного размера только следующего уровня.
		var next [][]byte
		// parent обозначает очередной контейнер текущего уровня.
		for _, parent := range parents {
			// bodies содержит искомые дочерние блоки, err — отказ повреждённой структуры.
			bodies, err := sourceBoxBodies(parent, level)
			if err != nil {
				return sourceVideoEntry{}, err
			}
			next = append(next, bodies...)
		}
		parents = next
	}
	// entryKinds задаёт допустимые имена видеозаписей, configKind — имя вложенной конфигурации наблюдаемого кодека.
	var entryKinds []string
	var configKind string
	switch kind {
	case core.CodecAV1:
		entryKinds, configKind = []string{"av01"}, "av1C"
	case core.CodecH264:
		entryKinds, configKind = []string{"avc1"}, "avcC"
	case core.CodecHEVC:
		entryKinds, configKind = []string{"hvc1", "hev1"}, "hvcC"
	case core.CodecVP9:
		entryKinds, configKind = []string{"vp09"}, "vpcC"
	default:
		return sourceVideoEntry{}, ErrInvalid
	}
	// result сохраняет одну видеозапись без смешивания дескрипторов разных дорожек.
	var result sourceVideoEntry
	// stsd содержит version/flags, entry_count и последовательность описаний дорожек.
	for _, stsd := range parents {
		if len(stsd) < 8 || binary.BigEndian.Uint32(stsd[:4]) != 0 {
			return result, ErrInvalid
		}
		// allEntries сверяет число фактических записей с entry_count; err сообщает о нарушенном размере.
		allEntries, err := sourceBoxBodies(stsd[8:], "")
		if err != nil || uint64(len(allEntries)) != uint64(binary.BigEndian.Uint32(stsd[4:8])) {
			return result, ErrInvalid
		}
		// entryKind выбирает применимую запись конкретного кодека.
		for _, entryKind := range entryKinds {
			// entries содержит только применимые записи, err сохраняет нарушение обрамления.
			entries, err := sourceBoxBodies(stsd[8:], entryKind)
			if err != nil {
				return result, err
			}
			// entry содержит фиксированные 78 байтов описания видео и дочерние блоки.
			for _, entry := range entries {
				if len(entry) < 78 || result.headers != nil {
					return result, ErrInvalid
				}
				// configs содержит единственное фактическое описание выбранного кодека.
				// err сообщает о повреждённом обрамлении дочерних блоков этой видеозаписи.
				configs, err := sourceBoxBodies(entry[78:], configKind)
				if err != nil || len(configs) != 1 {
					return result, ErrInvalid
				}
				result.headers = configs[0]
				if kind == core.CodecVP9 {
					if len(result.headers) < 4 || binary.BigEndian.Uint32(result.headers) != 1<<24 {
						return result, ErrInvalid
					}
					result.headers = result.headers[4:]
				}
				if len(result.headers) == 0 || uint64(len(result.headers)) > maximum {
					return result, ErrLimit
				}
				result.headers = bytes.Clone(result.headers)
				// colors сохраняет только colr этой же записи, без доверия меткам ffprobe.
				colors, err := sourceBoxBodies(entry[78:], "colr")
				if err != nil || len(colors) > 1 {
					return result, ErrInvalid
				}
				if len(colors) == 1 {
					// color содержит исходный nclx с тремя независимыми uint16 и битом диапазона.
					color := colors[0]
					if len(color) != 11 || string(color[:4]) != "nclx" || color[10]&127 != 0 || binary.BigEndian.Uint16(color[4:]) > 255 || binary.BigEndian.Uint16(color[6:]) > 255 || binary.BigEndian.Uint16(color[8:]) > 255 {
						return result, ErrInvalid
					}
					result.color = codec.CICPColor(color[5], color[7], color[9], color[10]&128 != 0)
					result.provenance = media.ColorConfigProvenance{Source: media.HDRSourceContainer, Primaries: true, Transfer: true, Matrix: true}
				}
			}
		}
	}
	if result.headers == nil {
		return result, ErrInvalid
	}
	if kind == core.CodecVP9 {
		// err проверяет реальную структуру записи кодека до разбора первых AU.
		if _, err := codec.ParseVP9Config(result.headers); err != nil {
			return result, err
		}
	}
	return result, nil
}

// sourceVideoFormat сверяет данные ffprobe с записью кодека и nclx именно этого MOV/MP4 файла.
// ctx и существующие бюджеты ограничивают чтение. Только реальные внешние байты дают provenance;
// отсутствие nclx сохраняет строгие проверки, а метки ffprobe и ранее переданный source не создают признак происхождения.
func (p Prober) sourceVideoFormat(ctx context.Context, file *os.File, stream probeStream) (core.VideoFormat, sourceVideoEntry, error) {
	// description содержит реальные записи файла; err сообщает об отказе ограниченного чтения moov.
	description, err := p.sourceVideoDescription(ctx, file, core.Codec(stream.Codec))
	if err != nil {
		return core.VideoFormat{}, description, err
	}
	if stream.Codec != string(core.CodecVP9) {
		// probed сверяет запись кодека с дампом выбранной видеодорожки, не допуская подмены другой записью.
		probed, parseErr := probeExtraData(stream.ExtraData, stream.ExtraDataSize)
		if parseErr != nil || !bytes.Equal(probed, description.headers) {
			return core.VideoFormat{}, description, ErrInvalid
		}
	}
	// Отсутствующие метки ffprobe можно уточнить только реально прочитанным nclx.
	// Зарезервированные обозначения и диапазон не заменяются; известные метки проверяются ниже.
	if description.provenance != (media.ColorConfigProvenance{}) {
		if stream.Primaries == "" || stream.Primaries == "unspecified" {
			stream.Primaries = description.color.Primaries
		}
		if stream.Transfer == "" || stream.Transfer == "unspecified" {
			stream.Transfer = description.color.Transfer
		}
		if stream.Matrix == "" || stream.Matrix == "unspecified" {
			stream.Matrix = description.color.Matrix
		}
	}
	// measured содержит полный наблюдаемый формат с ограниченным разрешением внешних CICP.
	measured, err := stream.videoWithExternalColor(description.provenance)
	if err != nil {
		return measured, description, err
	}
	if description.provenance != (media.ColorConfigProvenance{}) {
		// external меняет только цвет на реально прочитанный nclx для строгой сверки меток ffprobe.
		external := measured
		external.Color = description.color
		if err = codec.CompatibleVideoActual(measured, external); err != nil {
			return measured, description, err
		}
	}
	if err = codec.ValidateVideoHeaders(description.headers, measured, description.provenance); err != nil {
		return measured, description, err
	}
	return measured, description, nil
}

// sourceBoxBodies разбирает конечную последовательность блоков одного уровня и выбирает содержимое
// блоков с именем kind; пустое имя выбирает все блоки. Размеры 0 и 64-битные размеры
// допустимы только в пределах data.
func sourceBoxBodies(data []byte, kind string) ([][]byte, error) {
	// bodies содержит срезы одного ограниченного по размеру moov на время проверки источника.
	var bodies [][]byte
	for len(data) != 0 {
		if len(data) < 8 {
			return nil, ErrInvalid
		}
		// size и header задают длину текущего блока и его полного заголовка.
		size, header := uint64(binary.BigEndian.Uint32(data[:4])), uint64(8)
		switch size {
		case 1:
			if len(data) < 16 {
				return nil, ErrInvalid
			}
			size, header = binary.BigEndian.Uint64(data[8:16]), 16
		case 0:
			size = uint64(len(data))
		}
		if size < header || size > uint64(len(data)) {
			return nil, ErrInvalid
		}
		if kind == "" || string(data[4:8]) == kind {
			bodies = append(bodies, data[header:size])
		}
		data = data[size:]
	}
	return bodies, nil
}
