package codec

import (
	"bytes"
	"encoding/hex"
	"errors"
	"testing"
)

// Преобразует шестнадцатеричный эталон AAC в байты и завершает тест при ошибке записи эталона.
func aacHex(t *testing.T, value string) []byte {
	t.Helper()
	// raw содержит байты шестнадцатеричного AAC-эталона. err сообщает о некорректной записи эталона.
	raw, err := hex.DecodeString(value)
	if err != nil {
		t.Fatal(err)
	}
	return raw
}

// Проверяет приём моно- и стереофонических AAC-LC AU с разными окнами и метаданными, неизменность
// входа и допустимость завершающего нулевого байта.
func TestAACLCActualFramesAndChannelHeaders(t *testing.T) {
	// Первые четыре AU получены встроенным кодировщиком AAC FFmpeg 7.1.5 из anullsrc при 48 kHz.
	// Каждый AU отдельно декодирован с -xerror: ровно 1024 нулевых сэмпла на канал.
	// Два дополнительных полных AU с нулевыми спектральными полосами также отдельно декодированы тем же FFmpeg.
	// tc содержит выбранную AAC-единицу и соответствующее ей число каналов.
	for _, tc := range []struct {
		// name обозначает вариант AAC в имени подтеста. raw содержит шестнадцатеричную запись
		// полной единицы доступа.
		name, raw string
		// channels задаёт число каналов для построения соответствующего ASC.
		channels byte
	}{
		{"mono", "01182007", 1},
		{"stereo", "211004608c1c", 2},
		{"mono-leading-fill", "de02004c61766336312e31392e313031000230400e", 1},
		{"stereo-leading-fill", "de02004c61766336312e31392e31303100422008c11838", 2},
		{"short-windows", "214000320641c0", 2},
		{"independent-windows", "2064000190000e", 2},
		// AAC синусоиды 997 Hz отдельно декодирован в 1024 ненулевых сэмпла на канал.
		{"spectral-data", "214a6cfe1f87e1f845d94bb2c97564bab29d3ffef7fe7f1c69e3a9aebfff3ffd7efd6bae2f53fff27fdfefd6bad6b5edfff97fdbebad75ad5889776e6d04848495ea9a993b5f3a48c1819567931440441910e78880e7645101f4e7e3101f4fa45171007d39dbf9658801f4fa73918cf258fb790d842c6d093e99183c703144611199107a2201e1c62039dee3101f47be514400fa7d19f2e3f2007d3e9011cb63c9729e60c2a594da81a506360c0c6d122962c76c1df077c1df1fff7bff3f8e34f1d4d75fff9ffebf7eb41fe3fff27fdfefd683febfff97fdbebad0000000000000000e", 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет для выбранного AAC AU соответствие заголовкам каналов, неизменность исходных байтов и
			// допустимость завершающего нулевого байта.

			// raw содержит декодированные байты проверяемой AAC-единицы.
			raw := aacHex(t, tc.raw)
			// original сохраняет исходные байты для проверки неизменности входа.
			original := bytes.Clone(raw)
			// headers содержит ASC с частотой 48 кГц и числом каналов выбранной единицы.
			headers := []byte{0x11, 0x80 | tc.channels<<3}
			// err сообщает об отказе проверки полной AAC-единицы.
			if err := ValidateAACAU(headers, raw); err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(raw, original) {
				t.Fatal("входной AU изменён")
			}
			// Нулевые байты после AU допускаются FFmpeg; префикс не вводит иной запрет.
			// err сообщает об отказе проверки полной единицы с дополнительным нулевым
			// байтом.
			if err := ValidateAACAU(headers, append(bytes.Clone(raw), 0)); err != nil {
				t.Fatal(err)
			}
		})
	}
}

// Проверяет отказ для усечённых AU, запрещённых полей, несовпадения каналов с ASC, неподдержанных
// элементов и пустых каналов без ID_END.
func TestAACLCPrefixRejectsTruncationAndReservedFields(t *testing.T) {
	// mono содержит полный эталон AAC с одним каналом.
	mono := aacHex(t, "01182007")
	// stereo содержит полный эталон AAC с парой каналов.
	stereo := aacHex(t, "211004608c1c")
	// fill содержит эталон стереофонического AAC с предшествующим FIL.
	fill := aacHex(t, "de02004c61766336312e31392e31303100422008c11838")
	// tc содержит эталон и ASC для проверки каждого байтового усечения.
	for _, tc := range []struct {
		// name обозначает вид усекаемой единицы в имени подтеста.
		name string
		// headers содержит ASC, соответствующий исходной полной единице.
		headers []byte
		// raw содержит полную AAC-единицу, из которой берутся усечённые префиксы.
		raw []byte
	}{
		{"mono", []byte{0x11, 0x88}, mono},
		{"stereo", []byte{0x11, 0x90}, stereo},
		{"fill", []byte{0x11, 0x90}, fill},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет отказ с ErrCodec при каждом байтовом обрыве выбранного короткого AAC AU.

			// В этих коротких AU каждый байтовый обрыв теряет обязательный минимум.
			// n задаёт длину проверяемого усечённого префикса в байтах.
			for n := 0; n < len(tc.raw); n++ {
				// err содержит результат проверки усечённой AAC-единицы.
				if err := ValidateAACAU(tc.headers, tc.raw[:n]); !errors.Is(err, ErrCodec) {
					t.Fatalf("обрезанный заголовок/минимум принят: n=%d err=%v", n, err)
				}
			}
		})
	}
	// tc выбирает исходный AU и запрещённый бит для повреждения.
	for _, tc := range []struct {
		// name обозначает повреждаемое поле в имени подтеста.
		name string
		// raw содержит исходную полную AAC-единицу для копирования.
		raw []byte
		// bit задаёт позицию запрещённого бита от начала AU.
		bit int
		// channels задаёт число каналов для ASC повреждаемой единицы.
		channels byte
	}{
		{"mono-reserved", mono, 15, 1},
		{"mono-prediction", mono, 25, 1},
		{"stereo-reserved", stereo, 8, 2},
		{"stereo-prediction", stereo, 18, 2},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Устанавливает выбранный запрещённый бит в копии AAC AU и проверяет отказ с ErrCodec.

			// raw содержит отдельную копию AU с установленным запрещённым битом.
			raw := bytes.Clone(tc.raw)
			raw[tc.bit/8] |= 1 << uint(7-tc.bit%8)
			// err содержит результат проверки AU с запрещённым полем.
			if err := ValidateAACAU([]byte{0x11, 0x80 | tc.channels<<3}, raw); !errors.Is(err, ErrCodec) {
				t.Fatal("запрещённое поле принято", err)
			}
		})
	}
	// raw содержит очередной неподдержанный, повреждённый или слишком большой AU.
	for _, raw := range [][]byte{
		{0}, {0xe0}, {0xff, 0xf1}, []byte("ADIF"),
		{0x40}, {0x60}, {0xa0}, // CCE, LFE, PCE.
		mono,                                 // SCE при stereo ASC.
		{0x21, 0x10, 0x1c, 0x60, 0x8c, 0x1c}, // ms_mask_present=3.
		{0x21, 0x3f, 0x84, 0x60, 0x8c, 0x1c}, // max_sfb=63.
		make([]byte, 1<<20+1),
	} {
		// err содержит результат проверки недопустимого AU со стереофоническим ASC.
		if err := ValidateAACAU([]byte{0x11, 0x90}, raw); !errors.Is(err, ErrCodec) {
			t.Fatalf("неподдержанный AU принят: %x: %v", raw[:min(len(raw), 8)], err)
		}
	}
	// err содержит результат проверки стереофонического AU с монофоническим ASC.
	if err := ValidateAACAU([]byte{0x11, 0x88}, stereo); !errors.Is(err, ErrCodec) {
		t.Fatal("CPE принят при mono ASC", err)
	}
	// err содержит результат проверки AAC без ASC.
	if err := ValidateAACAU(nil, stereo); !errors.Is(err, ErrCodec) {
		t.Fatal("отсутствующий ASC принят", err)
	}
	// tc содержит канальную конфигурацию и пустые каналы без синтаксического ID_END.
	for _, tc := range []struct {
		// headers содержит соответствующий ASC. raw содержит AAC-единицу с отсутствующим
		// обязательным ID_END.
		headers, raw []byte
	}{
		{[]byte{0x11, 0x88}, []byte{0, 0, 0, 0}},
		{[]byte{0x11, 0x90}, []byte{0x20, 0, 0, 0, 0, 0, 0}},
		{[]byte{0x11, 0x90}, []byte{0x21, 0x10, 0x04, 0x60, 0x8c, 0}},
	} {
		// err содержит результат проверки пустых каналов без ID_END.
		if err := ValidateAACAU(tc.headers, tc.raw); !errors.Is(err, ErrCodec) {
			t.Fatalf("пустые каналы без ID_END приняты: %x", tc.raw)
		}
	}
}

// Добавляет перед AU тестовый DSE с выбираемым выравниванием и обычной либо расширенной длиной.
func aacDataPrefix(raw []byte, aligned, extended bool) []byte {
	// prefix содержит первый байт DSE с выбираемым признаком выравнивания.
	prefix := byte(0x80)
	if aligned {
		prefix++
	}
	// count задаёт число байтов тестовых данных DSE.
	count := 3
	// out накапливает заголовок DSE перед добавлением данных и исходного AU.
	out := []byte{prefix, 3}
	if extended {
		count = 256
		out = []byte{prefix, 255, 1}
	}
	return append(append(out, bytes.Repeat([]byte{0x41}, count)...), raw...)
}

// Проверяет пропуск полных DSE/FIL перед каналом и отказ при каждом усечении метаданных или следующего
// канала.
func TestAACLCPrefixSkipsOnlyBoundedMetadata(t *testing.T) {
	// raw содержит полный стереофонический AU для добавления после метаданных.
	raw := aacHex(t, "211004608c1c")
	// aligned выбирает наличие выравнивания DSE перед телом.
	for _, aligned := range []bool{false, true} {
		// extended выбирает обычную либо расширенную запись длины DSE.
		for _, extended := range []bool{false, true} {
			// payload содержит DSE и следующий за ним полный AAC AU.
			payload := aacDataPrefix(raw, aligned, extended)
			// err сообщает об отказе проверки полной последовательности DSE и AU.
			if err := ValidateAACAU([]byte{0x11, 0x90}, payload); err != nil {
				t.Fatal(err)
			}
			// n задаёт длину очередного усечённого префикса DSE и AU в байтах.
			for n := range payload {
				// err содержит результат проверки усечённых метаданных или канальных
				// данных.
				if err := ValidateAACAU([]byte{0x11, 0x90}, payload[:n]); !errors.Is(err, ErrCodec) {
					t.Fatalf("обрезанный DSE/канал принят: aligned=%v extended=%v n=%d", aligned, extended, n)
				}
			}
		}
	}
	// FIL с count=0 сдвигает DSE; его выравнивание возвращает начало канала на байт.
	// b собирает битовый префикс с пустым FIL и выровненным DSE.
	b := ppsBits{}
	b.put(6, 3)
	b.put(0, 4)
	b.put(4, 3)
	b.put(0, 4)
	b.put(1, 1)
	b.put(1, 8)
	for b.count%8 != 0 {
		b.put(0, 1)
	}
	b.put(0x41, 8)
	// err сообщает об отказе проверки FIL, выровненного DSE и полного AU.
	if err := ValidateAACAU([]byte{0x11, 0x90}, append(b.raw, raw...)); err != nil {
		t.Fatal(err)
	}
	// prefix содержит очередной усечённый вариант FIL или DSE.
	for _, prefix := range [][]byte{{0xc2}, {0xde}, {0xde, 0x04}, {0x81}, {0x81, 255}, {0x81, 255, 1}, {0x80, 3, 0x41}} {
		// err содержит результат проверки усечённых метаданных.
		if err := ValidateAACAU([]byte{0x11, 0x90}, prefix); !errors.Is(err, ErrCodec) {
			t.Fatalf("обрезанный FIL/DSE принят: %x", prefix)
		}
	}
}

// Проверяет, что FIL или DSE после пустых каналов принимается только с последующим синтаксическим
// ID_END.
func TestAACEmptyChannelsRequireSyntaxEndAfterMetadata(t *testing.T) {
	// raw содержит стереофонический AU с пустыми каналами для перестроения завершающих элементов.
	raw := aacHex(t, "211004608c1c")
	// typ выбирает DSE или FIL после пустых каналов.
	for _, typ := range []uint32{4, 6} {
		// b собирает пустые каналы и выбранные метаданные по битам.
		b := ppsBits{}
		// Два пустых канала настоящего stereo AU заканчиваются перед битом 43.
		// bit задаёт позицию одного из первых 43 битов исходного AU, копируемых до его ID_END.
		for bit := range 43 {
			b.put(uint32(raw[bit/8]>>uint(7-bit%8)&1), 1)
		}
		b.put(typ, 3)
		b.put(0, 4)
		if typ == 4 {
			b.put(0, 1)
			b.put(0, 8)
		}
		// withoutEnd сохраняет каналы и метаданные до добавления ID_END.
		withoutEnd := bytes.Clone(b.raw)
		b.put(7, 3)
		for b.count%8 != 0 {
			b.put(0, 1)
		}
		// err сообщает об отказе проверки полной последовательности с ID_END.
		if err := ValidateAACAU([]byte{0x11, 0x90}, b.raw); err != nil {
			t.Fatal(err)
		}
		// err содержит результат проверки той же последовательности без ID_END.
		if err := ValidateAACAU([]byte{0x11, 0x90}, withoutEnd); !errors.Is(err, ErrCodec) {
			t.Fatal("метаданные без ID_END приняты", err)
		}
	}
}
