package codec_test

import (
	"bytes"
	"context"
	"encoding/binary"
	"encoding/hex"
	"os"
	"os/exec"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
)

// audioContractHex возвращает буквальный образец raw; t сообщает о неверной записи тестового оракула.
func audioContractHex(t *testing.T, raw string) []byte {
	t.Helper()
	// decoded содержит байты внешнего формата; err сообщает об ошибке самой фикстуры.
	decoded, err := hex.DecodeString(raw)
	if err != nil {
		t.Fatal(err)
	}
	return decoded
}

// audioContractNative исполняет для t ограниченный binary с синтетическим input и точными args.
// Результат содержит только stdout; никакой внешней площадки или пользовательского файла нет.
func audioContractNative(t *testing.T, binary string, input []byte, args ...string) []byte {
	t.Helper()
	// ctx и cancel ограничивают одну native codec операцию пятнадцатью секундами.
	ctx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	// command запускается без shell, stderr сохраняется для первой причины отказа.
	command := exec.CommandContext(ctx, binary, args...)
	command.Stdin = bytes.NewReader(input)
	// diagnostic сохраняет фактический stderr; output и err описывают завершение native операции.
	var diagnostic bytes.Buffer
	command.Stderr = &diagnostic
	output, err := command.Output()
	if err != nil {
		t.Fatalf("native codec: %v stderr=%s args=%q", err, diagnostic.String(), args)
	}
	return output
}

// TestAC3NativeSemanticFailureAfterValidCRCs проверяет actual six-block silence и независимый malformed input.
// t снимает только явно размеченный запрет EXP_REUSE в первом block; после исправления обеих CRC
// Headers остаётся допустимым, но silence normalizer обязан отвергнуть отсутствующие initial exponents.
func TestAC3NativeSemanticFailureAfterValidCRCs(t *testing.T) {
	// binary включает только явно разрешённую CPU codec проверку.
	binary := os.Getenv("GOST_MEDIA_FFMPEG")
	if binary == "" {
		t.Skip("нужен GOST_MEDIA_FFMPEG")
	}
	// encoded получен собственным кодировщиком с изменённой длиной BSI, чтобы offsets не были константами.
	encoded := audioContractNative(t, binary, nil, "-v", "error", "-f", "lavfi", "-i", "anullsrc=r=48000:cl=stereo", "-frames:a", "1", "-c:a", "ac3", "-b:a", "192000", "-channel_coupling", "0", "-stereo_rematrixing", "0", "-mixing_level", "85", "-room_type", "2", "-f", "ac3", "pipe:1")
	if len(encoded) == 0 || len(encoded)%768 != 0 || len(encoded) > 4*768 {
		t.Fatal("не получен ограниченный набор полных AC3 AU192k", len(encoded))
	}
	// frame выбирает первую полную AU независимо от flush-кадра native encoder.
	frame := bytes.Clone(encoded[:768])
	// headers и err подтверждают actual BSI/dac3 и обе исходные CRC.
	headers, err := codec.AC3Headers(frame)
	if err != nil {
		t.Fatal(err)
	}
	// normalized должен доказать все шесть блоков, а decoder независимо проверяет точный PCM0.
	normalized, err := codec.NormalizeAC3Silence(headers, frame)
	if err != nil {
		t.Fatal(err)
	}
	// pcm декодируется в float32, исключая сокрытие малых значений округлением в s16.
	pcm := audioContractNative(t, binary, normalized, "-v", "error", "-f", "ac3", "-i", "pipe:0", "-c:a", "pcm_f32le", "-f", "f32le", "pipe:1")
	if len(pcm) != 1536*2*4 || !bytes.Equal(pcm, make([]byte, len(pcm))) {
		t.Fatal("six-block normalization не дала1536 stereo PCM0", len(pcm))
	}
	// position отсчитывает фактические BSI bits после sync/CRC1; read получает count старших битов.
	position := 32
	read := func(count int) uint32 {
		// value собирает поле; count задаёт проверенное число битов нормативного синтаксиса.
		value := uint32(0)
		for range count {
			if position >= len(frame)*8 {
				t.Fatal("BSI фикстуры усечён")
			}
			value = value<<1 | uint32(frame[position/8]>>uint(7-position%8)&1)
			position++
		}
		return value
	}
	if read(2) != 0 || read(6)>>1 != 10 || read(5) != 8 {
		t.Fatal("фикстура вне AC3 binding")
	}
	read(3) // bsmod не влияет на расположение условных stereo полей.
	if read(3) != 2 {
		t.Fatal("ожидается stereo acmod2")
	}
	read(2) // dsurmod существует только у выбранного acmod2.
	if read(1) != 0 {
		t.Fatal("ожидается noLFE")
	}
	read(5) // dialnorm предшествует условным полям BSI.
	// count задаёт длины compr/langcod/audprodi при наличии соответствующего флага.
	for _, count := range []int{8, 8, 7} {
		if read(1) != 0 {
			read(count)
		}
	}
	read(2) // copyrightb и origbs.
	for range 2 {
		if read(1) != 0 {
			read(14)
		}
	}
	if read(1) != 0 {
		// extraBytes задаёт фактическое число дополнительных байтов BSI.
		extraBytes := int(read(6)) + 1
		for range extraBytes {
			read(8)
		}
	}
	read(4) // Два blksw и два dithflag первого audio block.
	if read(1) != 0 {
		read(8) // dynrng присутствует только при фактическом флаге.
	}
	if read(1) != 1 || read(1) != 0 || read(1) != 1 || read(4) != 0 {
		t.Fatal("native fixture должна явно задать no coupling/no rematrix")
	}
	// strategyBit вычислен по BSI и audblk syntax, а не по byte offset конкретного encoder результата.
	strategyBit := position
	if read(2) == 0 {
		t.Fatal("первый exponent strategy уже EXP_REUSE")
	}
	// malformed меняет initial exponent strategy в запрещённый reuse без предыдущего блока.
	malformed := bytes.Clone(frame)
	// bit обозначает один из двух bits стратегии первого канала.
	for bit := strategyBit; bit < strategyBit+2; bit++ {
		malformed[bit/8] &^= 1 << uint(7-bit%8)
	}
	audioContractAC3CRC(malformed)
	if _, err = codec.AC3Headers(malformed); err != nil {
		t.Fatal("исправленные CRC/BSI не достигли semantic gate", err)
	}
	if _, err = codec.NormalizeAC3Silence(headers, malformed); err == nil {
		t.Fatal("CRC-корректный first-block EXP_REUSE принят как доказанная тишина")
	}
}

// audioContractAC3CRC исправляет два контрольных слова frame по полиному A/52 x16+x15+x2+1.
// Обратный проход вычисляет prefix CRC1; CRC2 завершает свою область обычным остатком.
func audioContractAC3CRC(frame []byte) {
	// split задаёт нормативную границу5/8 кадра768bytes, prefix восстанавливает состояние перед данными.
	split, prefix := 480, uint16(0)
	// index перебирает байты первой области в обратном порядке.
	for index := split - 1; index >= 4; index-- {
		for range 8 {
			if prefix&1 == 0 {
				prefix >>= 1
			} else {
				prefix = (prefix^0x8005)>>1 | 0x8000
			}
		}
		prefix ^= uint16(frame[index]) << 8
	}
	for range 16 {
		if prefix&1 == 0 {
			prefix >>= 1
		} else {
			prefix = (prefix^0x8005)>>1 | 0x8000
		}
	}
	binary.BigEndian.PutUint16(frame[2:4], prefix)
	// checksum накапливает CRC2 до завершающего контрольного слова.
	checksum := uint16(0)
	// value обозначает очередной байт второй области.
	for _, value := range frame[split : len(frame)-2] {
		checksum ^= uint16(value) << 8
		for range 8 {
			if checksum&0x8000 == 0 {
				checksum <<= 1
			} else {
				checksum = checksum<<1 ^ 0x8005
			}
		}
	}
	binary.BigEndian.PutUint16(frame[len(frame)-2:], checksum)
}

// TestFLACRejectsForbiddenLPCAndEmptyFirstRicePartition проверяет семантику после успешных CRC.
// t использует буквальные AU: RFC9639§9.2.6 запрещает отрицательный shift, §9.2.7 требует
// размер первой partition строго больше predictor order, даже если декодер принимает такую форму.
func TestFLACRejectsForbiddenLPCAndEmptyFirstRicePartition(t *testing.T) {
	// headers описывает точный standalone PCM16/48k/stereo блок64, не влияя на семантику subframes.
	headers := make([]byte, 34)
	binary.BigEndian.PutUint16(headers[:2], 64)
	binary.BigEndian.PutUint16(headers[2:4], 64)
	binary.BigEndian.PutUint64(headers[10:18], uint64(48000)<<44|uint64(1)<<41|uint64(15)<<36|64)
	// scenario содержит две независимо построенные CRC-корректные формы вне нормативного FLAC.
	for _, scenario := range []struct {
		// name определяет запрещённую семантику; raw содержит полный кадр с CRC8 и CRC16.
		name, raw string
	}{
		{"negative-lpc-shift", "fff86a18003f384000000f80f020000007c07800b37c"},
		{"empty-first-rice-partition", "fff86a18003f3818000000000000000013c1e0f0783c1e0f0783c1e0f0783c1e0f07806000000000000000004f0783c1e0f0783c1e0f0783c1e0f0783c1e002bf3"},
	} {
		// Подтест проверяет CRC и отказ выбранной запрещённой формы FLAC; t сообщает о нарушении.
		t.Run(scenario.name, func(t *testing.T) {
			// frame хранит независимую буквальную AU.
			frame := audioContractHex(t, scenario.raw)
			// crc16 проверяет целый кадр, включая footer, по нормативному полиному0x8005.
			crc16 := uint16(0)
			// value обозначает следующий байт, включаемый в независимую CRC.
			for _, value := range frame {
				crc16 ^= uint16(value) << 8
				for range 8 {
					if crc16&0x8000 != 0 {
						crc16 = crc16<<1 ^ 0x8005
					} else {
						crc16 <<= 1
					}
				}
			}
			if crc16 != 0 {
				t.Fatal("ошибка CRC-корректной фикстуры", crc16)
			}
			// parsed и err должны отвергнуть семантику независимо от пройденной CRC.
			parsed, err := codec.FLACFrameInfo(headers, frame)
			if err == nil {
				t.Fatalf("запрещённый FLAC принят: %+v", parsed)
			}
		})
	}
}

// TestOpusLiteralEndianAndPacketBoundaries проверяет независимые записи RFC7845 и dOps.
// t подтверждает advisory rate44100, реальный clock48000 и отказ форм вне одного CELT120.
func TestOpusLiteralEndianAndPacketBoundaries(t *testing.T) {
	// head и dops представляют одни поля в двух нормативных порядках байтов.
	head := audioContractHex(t, "4f707573486561640102780044ac0000000000")
	dops := audioContractHex(t, "000200780000ac44000000")
	// parsed и err проверяют LE независимо от обратного преобразования.
	parsed, err := codec.ParseOpusHead(head)
	if err != nil || parsed.PreSkip != 120 || parsed.InputSampleRate != 44100 {
		t.Fatalf("Head=%+v err=%v", parsed, err)
	}
	// encoded получает dOps из Head; err сохраняет ошибку преобразования.
	encoded, err := codec.OpusHeadToDOps(head)
	if err != nil || !bytes.Equal(encoded, dops) {
		t.Fatalf("dOps=%x err=%v", encoded, err)
	}
	encoded, err = codec.OpusHeadFromDOps(dops)
	if err != nil || !bytes.Equal(encoded, head) {
		t.Fatalf("Head=%x err=%v", encoded, err)
	}
	// actual измеряется по Head; advisory44100 не заменяет clock декодера48000.
	actual, err := codec.AudioFormatHeaders(core.CodecOpus, head)
	if err != nil || actual.SampleRateHz != 48000 || actual.Channels != 2 || actual.CodecProfile != "celt" {
		t.Fatalf("actual=%+v err=%v", actual, err)
	}
	// normalized сохраняет исходный Head и меняет только уже обработанный preSkip.
	normalized, err := codec.NormalizeOpusHead(head)
	if err != nil || normalized[10] != 0 || normalized[11] != 0 || head[10] != 120 || !bytes.Equal(normalized[12:], head[12:]) {
		t.Fatal("нормализация изменила advisory fields или исходный Head", err)
	}
	// scenario задаёт полную CELT120 либо заведомо иной framing/duration.
	for _, scenario := range []struct {
		// name различает проверяемое нарушение границы пакета.
		name string
		// raw содержит буквальный Opus packet, valid обозначает разрешённую форму.
		raw   []byte
		valid bool
	}{
		{"single", []byte{0xe4, 0xff, 0xfe}, true},
		{"one-code3", []byte{0xe7, 1, 0xff, 0xfe}, true},
		{"padding", []byte{0xe7, 0x41, 1, 0xff, 0xfe, 0}, true},
		{"two-cbr", []byte{0xe5, 0xff, 0xfe}, false},
		{"two-vbr", []byte{0xe6, 1, 0xff, 0xfe}, false},
		{"code3-count2", []byte{0xe7, 2, 0xff, 0xfe}, false},
		{"truncated-padding", []byte{0xe7, 0x41, 255}, false},
		{"duration5ms", []byte{0xec, 0xff, 0xfe}, false},
		{"mono", []byte{0xe0, 0xff, 0xfe}, false},
	} {
		// Подтест сверяет допустимость и число сэмплов выбранной формы Opus; t сообщает об отклонении.
		t.Run(scenario.name, func(t *testing.T) {
			// samples и sampleErr отделяют фактический размер AU от желаемого формата.
			samples, sampleErr := codec.AudioSamples(core.CodecOpus, normalized, scenario.raw)
			if (sampleErr == nil) != scenario.valid || scenario.valid && samples != 120 {
				t.Fatalf("samples=%d err=%v", samples, sampleErr)
			}
		})
	}
}

// TestMP3FrameLocalFlagsAndStrictDurations проверяет, что меняющиеся флаги кадра не становятся config.
// t проверяет MPEG1L3/48k/128k/reservoir0 и запрещает подстановку длительности для новых кодеков.
func TestMP3FrameLocalFlagsAndStrictDurations(t *testing.T) {
	// descriptor задаёт первый unprotected stereo header; frame содержит полный padded joint-stereo AU.
	descriptor := []byte{0xff, 0xfb, 0x94, 0}
	frame := make([]byte, 385)
	copy(frame, []byte{0xff, 0xfb, 0x97, 0x7d})
	// samples и err показывают, что padding/private/mode_extension/copyright/original/emphasis локальны AU.
	samples, err := codec.AudioSamples(core.CodecMP3, descriptor, frame)
	if err != nil || samples != 1152 {
		t.Fatalf("допустимые локальные флаги: samples=%d err=%v", samples, err)
	}
	// scenario меняет ровно одну контрактную границу полного кадра.
	for _, scenario := range []struct {
		// name обозначает неподдержанную или повреждённую форму.
		name string
		// offset и mask задают мутацию исходной полной AU.
		offset int
		mask   byte
	}{
		{"protected", 1, 1}, {"mpeg2", 1, 8}, {"free-format", 2, 0x90},
		{"reservoir-low", 5, 0x80}, {"reservoir-high", 4, 1}, {"dual-channel", 3, 0xc0},
	} {
		// Подтест требует отказа после одной мутации MP3; t сообщает о принятии запрещённого кадра.
		t.Run(scenario.name, func(t *testing.T) {
			// changed сохраняет остальные поля, чтобы отказ относился к выбранной форме.
			changed := bytes.Clone(frame)
			changed[scenario.offset] ^= scenario.mask
			// err сообщает об ожидаемом отказе именно изменённой AU, не о пригодности исходной фикстуры.
			if _, err := codec.AudioSamples(core.CodecMP3, descriptor, changed); err == nil {
				t.Fatal("форма вне binding принята")
			}
		})
	}
	// duration проверяет неизвестную, частичную, точную и превышенную длительность полного AU1152.
	for _, duration := range []int64{0, 1, 1151, 1152, 1153} {
		// timingErr сообщает о несогласованности container duration и фактической AU.
		timingErr := codec.ValidateAudioDuration(core.CodecMP3, descriptor, frame, media.PacketInfo{DurationTicks: duration}, media.TimeBase{Numerator: 1, Denominator: 48000})
		if (timingErr == nil) != (duration == 1152) {
			t.Fatalf("duration=%d err=%v", duration, timingErr)
		}
	}
}
