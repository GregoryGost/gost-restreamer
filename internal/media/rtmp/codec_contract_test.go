package rtmp

import (
	"bytes"
	"errors"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

// contractSequence задаёт независимые поля синтетической последовательности AV1 для проверки рабочего
// парсера.
type contractSequence struct {
	// tick задаёт NumUnitsInDisplayTick. scale задаёт TimeScale временных параметров AV1.
	tick, scale uint32
	// equal включает постоянный интервал картинки с двумя тактами отображения.
	equal bool
	// level задаёт индекс уровня AV1 в последовательности и префиксе av1C.
	level byte
	// highDepth включает признак высокой разрядности для проверки отказа.
	highDepth bool
	// superres включает неподдержанное переопределение разрешения.
	superres bool
	// primaries задаёт числовой CICP-код основных цветов.
	primaries byte
	// transfer задаёт числовой CICP-код передаточной характеристики.
	transfer byte
	// matrix задаёт числовой CICP-код цветовой матрицы.
	matrix byte
	// fullRange выбирает полный диапазон цвета.
	fullRange bool
}

// Возвращает независимые параметры AV1 SDR 640×360 с частотой 30000/1001 из timing_info.
func contractSDRSequence() contractSequence {
	return contractSequence{tick: 1001, scale: 60000, equal: true, level: 8, primaries: 1, transfer: 1, matrix: 1}
}

// contractAV1Config записывает поля AV1 §5.5 и av1C §2.3 независимо от рабочего парсера.
// Размеры равны 640×360; при equal_picture_interval кадр занимает два такта отображения.
// TU ниже содержит лишь проверяемую часть заголовка кадра и не заявляется декодируемым.
func contractAV1Config(s contractSequence) []byte {
	// payload накапливает байты независимого тестового заголовка последовательности.
	var payload []byte
	// position содержит число записанных битов и определяет следующую позицию.
	position := 0
	// put записывает заданное число битов в общий заголовок последовательности.
	put := func(value uint32, width int) {
		// Дописывает заданное число битов от старшего к младшему в общий буфер payload, продвигая position.

		// bit задаёт позицию очередного записываемого бита значения, от старшего к младшему.
		for bit := width - 1; bit >= 0; bit-- {
			if position%8 == 0 {
				payload = append(payload, 0)
			}
			payload[position/8] |= byte(value>>uint(bit)&1) << uint(7-position%8)
			position++
		}
	}
	// flag преобразует логический параметр последовательности в битовое значение.
	flag := func(value bool) uint32 {
		// Преобразует логическое поле тестового заголовка AV1 в битовое значение 1 либо 0.

		if value {
			return 1
		}
		return 0
	}
	put(0, 3) // seq_profile: Main.
	put(0, 1) // still_picture.
	put(0, 1) // reduced_still_picture_header.
	put(1, 1) // timing_info_present_flag.
	put(s.tick, 32)
	put(s.scale, 32)
	put(flag(s.equal), 1)
	if s.equal {
		put(2, 3) // uvlc(1): num_ticks_per_picture_minus_1.
	}
	put(0, 1)  // decoder_model_info_present_flag.
	put(0, 1)  // initial_display_delay_present_flag.
	put(0, 5)  // operating_points_cnt_minus_1.
	put(0, 12) // operating_point_idc.
	put(uint32(s.level), 5)
	if s.level > 7 {
		put(0, 1) // seq_tier.
	}
	put(9, 4) // frame_width_bits_minus_1.
	put(8, 4) // frame_height_bits_minus_1.
	put(639, 10)
	put(359, 9)
	put(0, 1) // frame_id_numbers_present_flag.
	put(0, 3) // use_128x128_superblock, enable_filter_intra, enable_intra_edge_filter.
	put(0, 5) // Межкадровые инструменты и enable_order_hint выключены.
	put(0, 1) // seq_choose_screen_content_tools.
	put(0, 1) // seq_force_screen_content_tools.
	put(flag(s.superres), 1)
	put(0, 2) // enable_cdef, enable_restoration.
	put(flag(s.highDepth), 1)
	put(0, 1) // mono_chrome.
	put(1, 1) // color_description_present_flag.
	put(uint32(s.primaries), 8)
	put(uint32(s.transfer), 8)
	put(uint32(s.matrix), 8)
	put(flag(s.fullRange), 1)
	put(0, 2) // chroma_sample_position.
	put(0, 1) // separate_uv_delta_q.
	put(0, 1) // film_grain_params_present.
	put(1, 1) // trailing_one_bit; оставшаяся часть байта заполнена нулями.
	// depth содержит бит высокой разрядности для префикса av1C.
	depth := byte(0)
	if s.highDepth {
		depth = 0x40
	}
	return append([]byte{0x81, s.level, 0x0c | depth, 0, 0x0a, byte(len(payload))}, payload...)
}

// Создаёт синтетический TU из заданной последовательности и проверяемого префикса показанного
// KEY_FRAME.
func contractAV1TU(config []byte) []byte {
	return append(bytes.Clone(config[4:]), 0x1a, 1, 0x10)
}

// Проверяет FPS из timing_info без метаданных, математически равные метаданные, явный FPS для
// переменных интервалов и отказ противоречащей частоты.
func TestContractAV1TimingAndObservedColor(t *testing.T) {
	// tc содержит последовательность, метаданные и независимо ожидаемые FPS и цвет.
	for _, tc := range []struct {
		// name обозначает источник частоты кадров в имени подтеста.
		name string
		// seq содержит поля синтетической последовательности AV1.
		seq contractSequence
		// metadata задаёт дополнительно переданный FPS либо нулевое отсутствие сведений.
		metadata core.FrameRate
		// want задаёт ожидаемую итоговую частоту кадров.
		want core.FrameRate
		// color задаёт ожидаемые диапазон и характеристики цвета.
		color core.ColorInfo
	}{
		{"timing-without-metadata", contractSDRSequence(), core.FrameRate{}, core.FrameRate{Numerator: 30000, Denominator: 1001}, core.ColorInfo{Range: "limited", Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"}},
		{"equivalent-metadata", contractSDRSequence(), core.FrameRate{Numerator: 60000, Denominator: 2002}, core.FrameRate{Numerator: 30000, Denominator: 1001}, core.ColorInfo{Range: "limited", Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"}},
		{"variable-interval-needs-metadata", contractSequence{tick: 1, scale: 60, equal: false, level: 31, primaries: 9, transfer: 14, matrix: 9, fullRange: true}, core.FrameRate{Numerator: 24, Denominator: 1}, core.FrameRate{Numerator: 24, Denominator: 1}, core.ColorInfo{Range: "full", Primaries: "cicp:9", Transfer: "cicp:14", Matrix: "cicp:9"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет размеры, цвет, независимый старт и точную частоту кадров для выбранных timing_info и
			// метаданных AV1.

			// config содержит независимый av1C выбранного сценария.
			config := contractAV1Config(tc.seq)
			// got содержит наблюдаемый формат. key показывает независимость TU. err сообщает
			// об отказе проверки.
			got, key, err := InspectAV1TU(config, contractAV1TU(config), tc.metadata)
			if err != nil || !key || got.Width != 640 || got.Height != 360 || !got.FrameRate.Equal(tc.want) || got.Color != tc.color || got.CodecProfile != "main" || got.BitDepth != 8 || got.PixelFormat != "yuv420p" {
				t.Fatalf("наблюдаемый формат: %+v key=%v err=%v", got, key, err)
			}
			if tc.seq.level == 31 && got.Level != "unspecified" {
				t.Fatalf("неуказанный level заменён догадкой: %q", got.Level)
			}
		})
	}
	// config содержит av1C с постоянным timing_info для проверки противоречащих метаданных.
	config := contractAV1Config(contractSDRSequence())
	// key содержит результат определения независимости при противоречивом FPS. err сообщает об
	// отказе согласования частоты.
	if _, key, err := InspectAV1TU(config, contractAV1TU(config), core.FrameRate{Numerator: 30, Denominator: 1}); !errors.Is(err, ErrCodec) || key {
		t.Fatalf("противоречащий timing_info FPS принят: key=%v err=%v", key, err)
	}
}

// Проверяет отказ повреждённых av1C/OBU и несогласованных полей префикса с последовательностью.
func TestContractAV1RejectsMalformedConfiguration(t *testing.T) {
	// base содержит корректную независимую запись av1C для контролируемых повреждений.
	base := contractAV1Config(contractSDRSequence())
	// mutate возвращает отдельную копию av1C с инвертированными битами выбранного байта.
	mutate := func(offset int, mask byte) []byte {
		// Возвращает копию базового av1C с инвертированными по маске битами указанного байта.
		// b содержит копию базового av1C, изменяемую выбранной маской.
		b := bytes.Clone(base)
		b[offset] ^= mask
		return b
	}
	// tc содержит выбранный повреждённый av1C.
	for _, tc := range []struct {
		// name обозначает повреждение конфигурации в имени подтеста.
		name string
		// raw содержит повреждённую запись av1C или OBU.
		raw []byte
	}{
		{"truncated-record", base[:3]},
		{"marker", mutate(0, 0x80)},
		{"version", mutate(0, 3)},
		{"reserved-high", mutate(3, 0x20)},
		{"unsupported-delay", mutate(3, 0x10)},
		{"missing-sequence", base[:4]},
		{"duplicate-sequence", append(bytes.Clone(base), base[4:]...)},
		{"frame-in-config", append(bytes.Clone(base), 0x1a, 1, 0x10)},
		{"forbidden-bit", mutate(4, 0x80)},
		{"reserved-obu-bit", mutate(4, 1)},
		{"missing-size", mutate(4, 2)},
		{"truncated-size", append(bytes.Clone(base[:4]), 0x0a, 0x80)},
		{"unterminated-size", append(bytes.Clone(base[:4]), 0x0a, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0x80, 0)},
		{"oversize-payload", append(bytes.Clone(base[:4]), 0x0a, 0x7f, 0)},
		{"truncated-sequence", append(bytes.Clone(base[:4]), 0x0a, 1, 0)},
		{"sequence-trailing-bit", mutate(len(base)-1, 0x08)},
		{"sequence-padding", mutate(len(base)-1, 1)},
		{"profile-mismatch", mutate(1, 0x20)},
		{"level-mismatch", mutate(1, 1)},
		{"tier-mismatch", mutate(2, 0x80)},
		{"depth-mismatch", mutate(2, 0x40)},
		{"twelve-bit-mismatch", mutate(2, 0x20)},
		{"monochrome-mismatch", mutate(2, 0x10)},
		{"subsampling-x-mismatch", mutate(2, 8)},
		{"subsampling-y-mismatch", mutate(2, 4)},
		{"chroma-position-mismatch", mutate(2, 1)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет ErrCodec для выбранного повреждения av1C либо его OBU.

			// err содержит результат проверки выбранной повреждённой конфигурации.
			if _, _, err := InspectAV1TU(tc.raw, contractAV1TU(base), core.FrameRate{}); !errors.Is(err, ErrCodec) {
				t.Fatalf("некорректный av1C принят: %v", err)
			}
		})
	}
}

// Проверяет отказ AV1 с высокой разрядностью, superres, PQ/HLG, зарезервированным уровнем и нулевыми
// параметрами timing_info.
func TestContractAV1RejectsUnsupportedSequences(t *testing.T) {
	// tc выбирает неподдержанный параметр последовательности.
	for _, tc := range []struct {
		// name обозначает неподдержанную возможность в имени подтеста.
		name string
		// change изменяет соответствующее поле исходной SDR-последовательности.
		change func(*contractSequence)
	}{
		{"10-bit", func(s *contractSequence) {
			// Включает неподдержанную глубину цвета 10 бит в тестовой последовательности AV1.
			s.highDepth = true
		}},
		{"superres", func(s *contractSequence) {
			// Включает неподдержанное сверхразрешение в тестовой последовательности AV1.
			s.superres = true
		}},
		{"PQ", func(s *contractSequence) {
			// Задаёт характеристику переноса PQ в тестовой последовательности AV1.
			s.transfer = 16
		}},
		{"HLG", func(s *contractSequence) {
			// Задаёт характеристику переноса HLG в тестовой последовательности AV1.
			s.transfer = 18
		}},
		{"reserved-level", func(s *contractSequence) {
			// Задаёт зарезервированный уровень 24 в тестовой последовательности AV1.
			s.level = 24
		}},
		{"zero-timescale", func(s *contractSequence) {
			// Обнуляет временную шкалу тестовой последовательности AV1.
			s.scale = 0
		}},
		{"zero-display-tick", func(s *contractSequence) {
			// Обнуляет длительность такта отображения тестовой последовательности AV1.
			s.tick = 0
		}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Применяет выбранный неподдержанный параметр последовательности и проверяет ErrCodec.

			// s содержит копию допустимых полей последовательности для выбранного изменения.
			s := contractSDRSequence()
			tc.change(&s)
			// config содержит av1C после внесения неподдержанного параметра.
			config := contractAV1Config(s)
			// err содержит результат проверки неподдержанной последовательности.
			if _, _, err := InspectAV1TU(config, contractAV1TU(config), core.FrameRate{}); !errors.Is(err, ErrCodec) {
				t.Fatalf("неподдержанный sequence принят: %v", err)
			}
		})
	}
}

// Проверяет нулевые зарезервированные биты av1C и обязательность Sequence Header первым OBU
// конфигурации.
func TestContractAV1ConfigurationReservedBitsAndSequenceOrder(t *testing.T) {
	// AV1 ISOBMFF v1.2 §2.3.3 требует нули в младших четырёх зарезервированных битах;
	// §2.3.4 требует, чтобы Sequence Header OBU был первым в configOBUs.
	// tc содержит конфигурацию с нарушением зарезервированных битов или порядка OBU.
	for _, tc := range []struct {
		// name обозначает вид нарушения в имени подтеста.
		name string
		// raw содержит av1C с выбранным нарушением.
		raw []byte
	}{
		{"reserved-low", []byte{0x81, 8, 0x0c, 1, 0x0a, 11, 0, 0, 0, 66, 167, 191, 228, 96, 13, 0, 64}},
		{"sequence-after-padding", []byte{0x81, 8, 0x0c, 0, 0x7a, 1, 0x80, 0x0a, 11, 0, 0, 0, 66, 167, 191, 228, 96, 13, 0, 64}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет отказ для выбранного нарушения зарезервированных битов av1C либо порядка Sequence Header
			// OBU.

			// err содержит результат проверки зарезервированных битов и порядка
			// последовательности.
			if _, _, err := InspectAV1TU(tc.raw, testAV1TU(), core.FrameRate{Numerator: 30, Denominator: 1}); !errors.Is(err, ErrCodec) {
				t.Fatalf("некорректный av1C принят: raw=%x err=%v", tc.raw, err)
			}
		})
	}
}

// Проверяет приём метаданных и OBU заполнения после последовательности с сохранением независимого
// старта и фактического формата.
func TestContractAV1ConfigurationAllowsMetadataAndPaddingAfterSequence(t *testing.T) {
	// base содержит корректный av1C перед добавлением допустимых служебных OBU.
	base := contractAV1Config(contractSDRSequence())
	// metadata_type=5 (TIMECODE), counting_type=0, full_timestamp_flag=0,
	// discontinuity_flag=0, cnt_dropped_flag=0, n_frames=0, seconds_flag=0,
	// time_offset_length=0; завершение OBU содержит trailing_one_bit.
	// metadata содержит синтетический OBU TIMECODE с завершающим битом.
	metadata := []byte{0x2a, 4, 5, 0, 0, 1}
	// extra содержит OBU заполнения, метаданные либо их последовательность после заголовка.
	for _, extra := range [][]byte{{0x7a, 1, 0x80}, metadata, append(bytes.Clone(metadata), 0x7a, 1, 0x80)} {
		// config содержит av1C с выбранными допустимыми OBU после последовательности.
		config := append(bytes.Clone(base), extra...)
		// got содержит подтверждённый формат. key показывает независимость TU. err сообщает об
		// отказе проверки дополненной конфигурации.
		got, key, err := InspectAV1TU(config, contractAV1TU(base), core.FrameRate{})
		if err != nil || !key || got.Width != 640 || got.Height != 360 || !got.FrameRate.Equal(core.FrameRate{Numerator: 30000, Denominator: 1001}) {
			t.Fatalf("допустимые OBU после sequence отвергнуты: extra=%x format=%+v key=%v err=%v", extra, got, key, err)
		}
	}
}

// Проверяет структуру TU, независимость только при показанном KEY_FRAME первым кадром TU и наличии
// последовательности, отказ неверного порядка и ограничение числа OBU.
func TestContractAV1TemporalUnitStructure(t *testing.T) {
	// config содержит av1C для проверки порядка кадров и структуры TU.
	config := contractAV1Config(contractSDRSequence())
	// sequence содержит отдельную копию исходного OBU последовательности.
	sequence := bytes.Clone(config[4:])
	// key содержит синтетический заголовок показанного KEY_FRAME.
	key := []byte{0x1a, 1, 0x10}
	// hidden содержит синтетический заголовок скрытого INTER_FRAME.
	hidden := []byte{0x1a, 1, 0x28}
	// AV1 ISOBMFF v1.2 §2.4 требует показанный KEY_FRAME первым кадром sync sample.
	// Скрытый INTER_FRAME перед ним допустим внутри TU, но не даёт независимого старта.
	// tc содержит выбранную структуру TU и ожидаемые ошибку и независимость.
	for _, tc := range []struct {
		// name обозначает сочетание OBU и кадров в имени подтеста.
		name string
		// tu содержит проверяемую временную единицу AV1.
		tu []byte
		// wantErr задаёт ожидаемую категорию отказа либо nil.
		wantErr error
		// independent задаёт ожидаемую возможность независимого старта.
		independent bool
	}{
		{"hidden-then-shown", bytes.Join([][]byte{sequence, hidden, key}, nil), nil, false},
		{"padding-after-frame", append(contractAV1TU(config), 0x7a, 1, 0), nil, true},
		{"key-without-inline-sequence", key, nil, false},
		{"no-frame", sequence, ErrCodec, false},
		{"hidden-only", append(bytes.Clone(sequence), hidden...), ErrCodec, false},
		{"two-shown", append(contractAV1TU(config), key...), ErrCodec, false},
		{"sequence-after-frame", append(bytes.Clone(key), sequence...), ErrCodec, false},
		{"nonempty-delimiter", append([]byte{0x12, 1, 0}, contractAV1TU(config)...), ErrCodec, false},
		{"late-delimiter", append(contractAV1TU(config), 0x12, 0), ErrCodec, false},
		{"unknown-obu", append(contractAV1TU(config), 0x4a, 0), ErrCodec, false},
		{"truncated-frame", append(bytes.Clone(sequence), 0x1a, 0), ErrCodec, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет выбранную структуру TU, ожидаемую ошибку и признак независимого старта по фактическому
			// порядку кадров.

			// got содержит наблюдаемый формат TU. independent содержит вычисленную
			// независимость. err содержит результат проверки структуры.
			got, independent, err := InspectAV1TU(config, tc.tu, core.FrameRate{})
			if !errors.Is(err, tc.wantErr) || independent != tc.independent || (err == nil && (got.Width != 640 || got.Height != 360)) {
				t.Fatalf("TU: format=%+v independent=%v err=%v", got, independent, err)
			}
		})
	}
	// 4096 пустых OBU_TEMPORAL_DELIMITER допустимы по количеству, но не дают кадр;
	// ещё один OBU должен завершаться ошибкой бюджета, а не разбором всех элементов.
	// err содержит результат разбиения 4097 OBU сверх установленного предела.
	if _, err := splitOBUs(bytes.Repeat([]byte{0x12, 0}, 4097)); !errors.Is(err, ErrLimit) {
		t.Fatalf("не ограничено число OBU: %v", err)
	}
}
