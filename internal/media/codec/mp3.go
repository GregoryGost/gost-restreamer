package codec

import (
	"bytes"
	"fmt"
)

// mp3Header проверяет raw как MPEG1 LayerIII, 128 кбит/с, 48 кГц, стерео либо joint-stereo без CRC.
// Возвращаемый размер полного кадра включает необязательный байт дополнения.
func mp3Header(raw []byte) (int, error) {
	if len(raw) < 4 || raw[0] != 0xff || raw[1]&0xfe != 0xfa {
		return 0, fmt.Errorf("%w: повреждённый либо неподдержанный MP3 header", ErrCodec)
	}
	if raw[1]&1 == 0 || raw[2]>>4 != 9 || raw[2]>>2&3 != 1 || raw[3]>>6 > 1 || raw[3]&3 == 2 {
		return 0, fmt.Errorf("%w: неподдержанный MP3 binding или CRC protection", ErrCodec)
	}
	return 384 + int(raw[2]>>1&1), nil
}

// MP3Headers получает независимый четырёхбайтовый дескриптор из полной единицы доступа raw.
// Кадр и 32-байтовая служебная часть проверяются; main_data_begin должен быть 0.
func MP3Headers(raw []byte) ([]byte, error) {
	// size содержит длину кадра из фактического заголовка; err сообщает о неподдержанных параметрах.
	size, err := mp3Header(raw)
	if err != nil {
		return nil, err
	}
	if len(raw) != size || len(raw) < 36 || raw[4] != 0 || raw[5]&0x80 != 0 {
		return nil, fmt.Errorf("%w: размер MP3 frame либо зависимый reservoir", ErrCodec)
	}
	// r читает служебную часть после девятибитного main_data_begin.
	r := bitReader{b: raw[4:36], p: 9}
	r.read(3) // private_bits стереофонического MPEG1.
	r.read(8) // scfsi двух каналов.
	// mainBits суммирует заявленные part2_3_length всех гранул и каналов.
	mainBits := uint32(0)
	for range 4 {
		mainBits += r.read(12)
		if r.read(9) > 288 { // big_values: не более 576 спектральных линий на гранулу.
			return nil, fmt.Errorf("%w: MP3 big_values", ErrCodec)
		}
		r.read(8) // global_gain.
		r.read(4) // scalefac_compress.
		if r.read(1) != 0 {
			if r.read(2) == 0 {
				return nil, fmt.Errorf("%w: MP3 block_type0 при window_switching", ErrCodec)
			}
			r.read(1)  // mixed_block_flag.
			r.read(10) // table_select двух областей.
			r.read(9)  // subblock_gain.
		} else {
			r.read(15) // table_select трёх областей.
			r.read(4)  // region0_count.
			r.read(3)  // region1_count.
		}
		r.read(3) // preflag, scalefac_scale, count1table_select.
	}
	if r.err != nil || r.p != 256 || mainBits > uint32((len(raw)-36)*8) {
		return nil, fmt.Errorf("%w: MP3 side info выходит за frame", ErrCodec)
	}
	return bytes.Clone(raw[:4]), nil
}
