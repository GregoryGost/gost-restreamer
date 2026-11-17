package codec

import (
	"bytes"
	"fmt"
)

// ac3FrameHeader сохраняет фактические поля BSI и начало первого аудиоблока.
type ac3FrameHeader struct {
	// mode задаёт bsmod, присутствующий как в BSI, так и в dac3.
	mode uint32
	// audioBit указывает на первый blksw после полного BSI.
	audioBit int
}

// ac3Config проверяет raw как полное тело dac3 поддержанного обычного AC3.
func ac3Config(raw []byte) error {
	if len(raw) != 3 {
		return fmt.Errorf("%w: длина dac3", ErrCodec)
	}
	// r читает fscod, bsid, bsmod, acmod, lfeon, bitrate_code и reserved.
	r := bitReader{b: raw}
	if r.read(2) != 0 || r.read(5) != 8 {
		return fmt.Errorf("%w: неподдержанный AC3 dac3", ErrCodec)
	}
	r.read(3) // bsmod сохраняется в дескрипторе, но не меняет поддержанные параметры PCM.
	if r.read(3) != 2 || r.read(1) != 0 || r.read(5) != 10 || r.read(5) != 0 || r.err != nil {
		return fmt.Errorf("%w: неподдержанный AC3 dac3", ErrCodec)
	}
	return nil
}

// ac3ParseFrame проверяет raw как один полный кадр AC3 из 768 байтов: 1536 отсчётов,
// 48 кГц, стерео, 192 кбит/с. Полный BSI читается по фактическим флагам;
// обе CRC проверяются до возврата audioBit.
func ac3ParseFrame(raw []byte) (ac3FrameHeader, error) {
	if len(raw) != 768 || raw[0] != 0x0b || raw[1] != 0x77 {
		return ac3FrameHeader{}, fmt.Errorf("%w: размер либо sync AC3 frame", ErrCodec)
	}
	// r читает syncinfo и BSI после синхрослова и CRC1.
	r := bitReader{b: raw, p: 32}
	if r.read(2) != 0 || r.read(6)>>1 != 10 || r.read(5) != 8 {
		return ac3FrameHeader{}, fmt.Errorf("%w: неподдержанный AC3 syncinfo", ErrCodec)
	}
	// frame сохраняет фактический bsmod для последующей сверки с dac3.
	frame := ac3FrameHeader{mode: r.read(3)}
	if r.read(3) != 2 {
		return frame, fmt.Errorf("%w: неподдержанный AC3 acmod", ErrCodec)
	}
	r.read(2) // dsurmod существует при acmod2 независимо от его значения.
	if r.read(1) != 0 {
		return frame, fmt.Errorf("%w: AC3 LFE вне stereo binding", ErrCodec)
	}
	r.read(5) // dialnorm.
	if r.read(1) != 0 {
		r.read(8) // compr.
	}
	if r.read(1) != 0 {
		r.read(8) // langcod.
	}
	if r.read(1) != 0 {
		r.read(7) // mixlevel и roomtyp.
	}
	r.read(2) // copyrightb и origbs.
	if r.read(1) != 0 {
		r.read(14) // timecod1.
	}
	if r.read(1) != 0 {
		r.read(14) // timecod2.
	}
	if r.read(1) != 0 {
		// count задаёт фактическое число дополнительных байтов BSI по addbsil+1.
		count := int(r.read(6)) + 1
		audioSkipBits(&r, count*8)
	}
	if r.err != nil || r.p >= len(raw)*8-18 {
		return frame, fmt.Errorf("%w: усечённый AC3 BSI", ErrCodec)
	}
	// split задаёт границу 5/8 кадра в байтах согласно A/52, включая синхрослово.
	split := ((len(raw) >> 2) + (len(raw) >> 4)) << 1
	if audioCRC16(raw[2:split], 0x8005) != 0 || audioCRC16(raw[split:], 0x8005) != 0 {
		return frame, fmt.Errorf("%w: AC3 CRC1 либо CRC2", ErrCodec)
	}
	frame.audioBit = r.p
	return frame, nil
}

// AC3Headers выводит независимый трёхбайтовый dac3 из полной проверенной AU payload.
// Результат сохраняет фактический bsmod; желаемые параметры не используются.
func AC3Headers(payload []byte) ([]byte, error) {
	// frame содержит проверенный BSI; err сообщает о неподдержанных параметрах либо неверной CRC.
	frame, err := ac3ParseFrame(payload)
	if err != nil {
		return nil, err
	}
	// fields собирает 24-битный dac3: fscod=0, bsid=8, acmod=2, lfeon=0, bitrate_code=10, reserved=0.
	fields := uint32(8)<<17 | frame.mode<<14 | 2<<11 | 10<<5
	return []byte{byte(fields >> 16), byte(fields >> 8), byte(fields)}, nil
}

// ac3Samples сверяет фактические заголовки dac3 headers и AU payload, возвращая 1536 отсчётов.
func ac3Samples(headers, payload []byte) (uint32, error) {
	// err сообщает о недопустимом dac3 до чтения кадра.
	if err := ac3Config(headers); err != nil {
		return 0, err
	}
	// actual содержит dac3 из BSI; err сообщает о непригодном полном кадре.
	actual, err := AC3Headers(payload)
	if err != nil {
		return 0, err
	}
	if !bytes.Equal(headers, actual) {
		return 0, fmt.Errorf("%w: AC3 BSI противоречит dac3", ErrCodec)
	}
	return 1536, nil
}
