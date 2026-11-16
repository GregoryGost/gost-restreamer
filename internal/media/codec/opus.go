package codec

import (
	"bytes"
	"encoding/binary"
	"fmt"
)

// OpusHead содержит переменные поля заголовка версии 1: стерео, карта каналов 0, усиление 0.
type OpusHead struct {
	// PreSkip задаёт начальное исключение 0 либо 120 отсчётов шкалы декодера 48 кГц.
	PreSkip uint16
	// InputSampleRate сохраняет исходную частоту как справочное поле, а не частоту декодера.
	InputSampleRate uint32
}

// ParseOpusHead разбирает полный 19-байтовый заголовок raw по RFC7845 с младшим байтом первым.
// raw не изменяется; остальные карты каналов, усиление, версии и начальное исключение отвергаются.
func ParseOpusHead(raw []byte) (OpusHead, error) {
	if len(raw) != 19 || string(raw[:8]) != "OpusHead" || raw[8] != 1 || raw[9] != 2 || raw[16] != 0 || raw[17] != 0 || raw[18] != 0 {
		return OpusHead{}, fmt.Errorf("%w: неподдержанный OpusHead", ErrCodec)
	}
	// head хранит фактические поля с младшим байтом первым без переопределения справочной частоты.
	head := OpusHead{PreSkip: binary.LittleEndian.Uint16(raw[10:12]), InputSampleRate: binary.LittleEndian.Uint32(raw[12:16])}
	if head.PreSkip != 0 && head.PreSkip != 120 {
		return OpusHead{}, fmt.Errorf("%w: неподдержанный Opus pre-skip", ErrCodec)
	}
	return head, nil
}

// NormalizeOpusHead возвращает отдельный заголовок без начального исключения после проверки raw.
// Вызывающий предварительно потребляет единственную целую начальную AU при исходном PreSkip 120.
func NormalizeOpusHead(raw []byte) ([]byte, error) {
	// err сообщает о неподдержанной исходной конфигурации.
	if _, err := ParseOpusHead(raw); err != nil {
		return nil, err
	}
	// out сохраняет все байты raw, кроме уже обработанного начального исключения.
	out := bytes.Clone(raw)
	out[10], out[11] = 0, 0
	return out, nil
}

// OpusHeadToDOps переводит проверенный заголовок raw в 11-байтовое тело dOps версии 0.
// Поля dOps имеют старший байт первым; raw остаётся неизменным.
func OpusHeadToDOps(raw []byte) ([]byte, error) {
	// head содержит исходные значения; err сообщает об отказе проверки заголовка.
	head, err := ParseOpusHead(raw)
	if err != nil {
		return nil, err
	}
	// out содержит поля dOps без заголовка блока MP4.
	out := make([]byte, 11)
	out[1] = 2
	binary.BigEndian.PutUint16(out[2:4], head.PreSkip)
	binary.BigEndian.PutUint32(out[4:8], head.InputSampleRate)
	return out, nil
}

// OpusHeadFromDOps переводит полное тело dOps raw в независимый нормативный OpusHead.
// Обязательны версия 0, стерео, карта 0 и усиление 0; ограничение PreSkip проверяется после перевода.
func OpusHeadFromDOps(raw []byte) ([]byte, error) {
	if len(raw) != 11 || raw[0] != 0 || raw[1] != 2 || raw[8] != 0 || raw[9] != 0 || raw[10] != 0 {
		return nil, fmt.Errorf("%w: неподдержанный dOps", ErrCodec)
	}
	// out содержит OpusHead с младшим байтом первым вместо сетевого порядка dOps.
	out := append([]byte("OpusHead"), 1, 2, 0, 0, 0, 0, 0, 0, 0, 0, 0)
	binary.LittleEndian.PutUint16(out[10:12], binary.BigEndian.Uint16(raw[2:4]))
	binary.LittleEndian.PutUint32(out[12:16], binary.BigEndian.Uint32(raw[4:8]))
	// err сообщает о начальном исключении за пределами поддержанных значений.
	if _, err := ParseOpusHead(out); err != nil {
		return nil, err
	}
	return out, nil
}

// opusSamples проверяет raw как один кадр CELT на 120 отсчётов с упаковкой и дополнением RFC6716.
// Код 3 допускается только при M=1; коды 1/2 всегда означают два кадра и отвергаются.
// Проверка ограничивает пакет 1 МиБ и кадр 1275 байтами, но не декодирует сжатые данные CELT.
func opusSamples(raw []byte) (uint32, error) {
	if len(raw) == 0 || len(raw) > 1<<20 {
		return 0, fmt.Errorf("%w: размер Opus packet", ErrCodec)
	}
	// config определяет CELT и длительность, code — число кадров и способ упаковки.
	config, code := raw[0]>>3, raw[0]&3
	if config < 16 || config&3 != 0 || raw[0]&4 == 0 || code == 1 || code == 2 {
		return 0, fmt.Errorf("%w: требуется stereo CELT frame120", ErrCodec)
	}
	// start указывает на сжатый кадр; padding считает хвостовые байты заполнения.
	start, padding := 1, 0
	if code == 3 {
		if len(raw) < 2 || raw[1]&63 != 1 {
			return 0, fmt.Errorf("%w: требуется одна Opus frame", ErrCodec)
		}
		start = 2
		if raw[1]&64 != 0 {
			for {
				if start >= len(raw)-padding {
					return 0, fmt.Errorf("%w: усечённое Opus padding", ErrCodec)
				}
				// size задаёт очередную часть длины padding; 255 продолжает описание и добавляет254.
				size := int(raw[start])
				start++
				padding += size
				if size != 255 {
					break
				}
				padding--
			}
		}
	}
	if len(raw)-start-padding < 0 || len(raw)-start-padding > 1275 {
		return 0, fmt.Errorf("%w: некорректная длина Opus frame", ErrCodec)
	}
	return 120, nil
}
