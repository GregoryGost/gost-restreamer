package rtmp

import (
	"bytes"
	"errors"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
)

type contractSequence struct {
	tick, scale uint32
	equal       bool
	level       byte
	highDepth   bool
	superres    bool
	primaries   byte
	transfer    byte
	matrix      byte
	fullRange   bool
}

func contractSDRSequence() contractSequence {
	return contractSequence{tick: 1001, scale: 60000, equal: true, level: 8, primaries: 1, transfer: 1, matrix: 1}
}

// contractAV1Config записывает поля AV1 §5.5 и av1C §2.3 независимо от рабочего парсера.
// Размеры равны 640×360; при equal_picture_interval кадр занимает два такта отображения.
// TU ниже содержит лишь проверяемую часть заголовка кадра и не заявляется декодируемым.
func contractAV1Config(s contractSequence) []byte {
	var payload []byte
	position := 0
	put := func(value uint32, width int) {
		for bit := width - 1; bit >= 0; bit-- {
			if position%8 == 0 {
				payload = append(payload, 0)
			}
			payload[position/8] |= byte(value>>uint(bit)&1) << uint(7-position%8)
			position++
		}
	}
	flag := func(value bool) uint32 {
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
	depth := byte(0)
	if s.highDepth {
		depth = 0x40
	}
	return append([]byte{0x81, s.level, 0x0c | depth, 0, 0x0a, byte(len(payload))}, payload...)
}

func contractAV1TU(config []byte) []byte {
	return append(bytes.Clone(config[4:]), 0x1a, 1, 0x10)
}

func TestContractAV1TimingAndObservedColor(t *testing.T) {
	for _, tc := range []struct {
		name     string
		seq      contractSequence
		metadata core.FrameRate
		want     core.FrameRate
		color    core.ColorInfo
	}{
		{"timing-without-metadata", contractSDRSequence(), core.FrameRate{}, core.FrameRate{Numerator: 30000, Denominator: 1001}, core.ColorInfo{Range: "limited", Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"}},
		{"equivalent-metadata", contractSDRSequence(), core.FrameRate{Numerator: 60000, Denominator: 2002}, core.FrameRate{Numerator: 30000, Denominator: 1001}, core.ColorInfo{Range: "limited", Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"}},
		{"variable-interval-needs-metadata", contractSequence{tick: 1, scale: 60, equal: false, level: 31, primaries: 9, transfer: 14, matrix: 9, fullRange: true}, core.FrameRate{Numerator: 24, Denominator: 1}, core.FrameRate{Numerator: 24, Denominator: 1}, core.ColorInfo{Range: "full", Primaries: "cicp:9", Transfer: "cicp:14", Matrix: "cicp:9"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			config := contractAV1Config(tc.seq)
			got, key, err := InspectAV1TU(config, contractAV1TU(config), tc.metadata)
			if err != nil || !key || got.Width != 640 || got.Height != 360 || !got.FrameRate.Equal(tc.want) || got.Color != tc.color || got.CodecProfile != "main" || got.BitDepth != 8 || got.PixelFormat != "yuv420p" {
				t.Fatalf("наблюдаемый формат: %+v key=%v err=%v", got, key, err)
			}
			if tc.seq.level == 31 && got.Level != "unspecified" {
				t.Fatalf("неуказанный level заменён догадкой: %q", got.Level)
			}
		})
	}
	config := contractAV1Config(contractSDRSequence())
	if _, key, err := InspectAV1TU(config, contractAV1TU(config), core.FrameRate{Numerator: 30, Denominator: 1}); !errors.Is(err, ErrCodec) || key {
		t.Fatalf("противоречащий timing_info FPS принят: key=%v err=%v", key, err)
	}
}

func TestContractAV1RejectsMalformedConfiguration(t *testing.T) {
	base := contractAV1Config(contractSDRSequence())
	mutate := func(offset int, mask byte) []byte { b := bytes.Clone(base); b[offset] ^= mask; return b }
	for _, tc := range []struct {
		name string
		raw  []byte
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
			if _, _, err := InspectAV1TU(tc.raw, contractAV1TU(base), core.FrameRate{}); !errors.Is(err, ErrCodec) {
				t.Fatalf("некорректный av1C принят: %v", err)
			}
		})
	}
}

func TestContractAV1RejectsUnsupportedSequences(t *testing.T) {
	for _, tc := range []struct {
		name   string
		change func(*contractSequence)
	}{
		{"10-bit", func(s *contractSequence) { s.highDepth = true }},
		{"superres", func(s *contractSequence) { s.superres = true }},
		{"PQ", func(s *contractSequence) { s.transfer = 16 }},
		{"HLG", func(s *contractSequence) { s.transfer = 18 }},
		{"reserved-level", func(s *contractSequence) { s.level = 24 }},
		{"zero-timescale", func(s *contractSequence) { s.scale = 0 }},
		{"zero-display-tick", func(s *contractSequence) { s.tick = 0 }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			s := contractSDRSequence()
			tc.change(&s)
			config := contractAV1Config(s)
			if _, _, err := InspectAV1TU(config, contractAV1TU(config), core.FrameRate{}); !errors.Is(err, ErrCodec) {
				t.Fatalf("неподдержанный sequence принят: %v", err)
			}
		})
	}
}

func TestContractAV1ConfigurationReservedBitsAndSequenceOrder(t *testing.T) {
	// AV1 ISOBMFF v1.2 §2.3.3 требует нули в младших четырёх зарезервированных битах;
	// §2.3.4 требует, чтобы Sequence Header OBU был первым в configOBUs.
	for _, tc := range []struct {
		name string
		raw  []byte
	}{
		{"reserved-low", []byte{0x81, 8, 0x0c, 1, 0x0a, 11, 0, 0, 0, 66, 167, 191, 228, 96, 13, 0, 64}},
		{"sequence-after-padding", []byte{0x81, 8, 0x0c, 0, 0x7a, 1, 0x80, 0x0a, 11, 0, 0, 0, 66, 167, 191, 228, 96, 13, 0, 64}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, _, err := InspectAV1TU(tc.raw, testAV1TU(), core.FrameRate{Numerator: 30, Denominator: 1}); !errors.Is(err, ErrCodec) {
				t.Fatalf("некорректный av1C принят: raw=%x err=%v", tc.raw, err)
			}
		})
	}
}

func TestContractAV1ConfigurationAllowsMetadataAndPaddingAfterSequence(t *testing.T) {
	base := contractAV1Config(contractSDRSequence())
	// metadata_type=5 (TIMECODE), counting_type=0, full_timestamp_flag=0,
	// discontinuity_flag=0, cnt_dropped_flag=0, n_frames=0, seconds_flag=0,
	// time_offset_length=0; завершение OBU содержит trailing_one_bit.
	metadata := []byte{0x2a, 4, 5, 0, 0, 1}
	for _, extra := range [][]byte{{0x7a, 1, 0x80}, metadata, append(bytes.Clone(metadata), 0x7a, 1, 0x80)} {
		config := append(bytes.Clone(base), extra...)
		got, key, err := InspectAV1TU(config, contractAV1TU(base), core.FrameRate{})
		if err != nil || !key || got.Width != 640 || got.Height != 360 || !got.FrameRate.Equal(core.FrameRate{Numerator: 30000, Denominator: 1001}) {
			t.Fatalf("допустимые OBU после sequence отвергнуты: extra=%x format=%+v key=%v err=%v", extra, got, key, err)
		}
	}
}

func TestContractAV1TemporalUnitStructure(t *testing.T) {
	config := contractAV1Config(contractSDRSequence())
	sequence := bytes.Clone(config[4:])
	key := []byte{0x1a, 1, 0x10}
	hidden := []byte{0x1a, 1, 0x28}
	// AV1 ISOBMFF v1.2 §2.4 требует показанный KEY_FRAME первым кадром sync sample.
	// Скрытый INTER_FRAME перед ним допустим внутри TU, но не даёт независимого старта.
	for _, tc := range []struct {
		name        string
		tu          []byte
		wantErr     error
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
			got, independent, err := InspectAV1TU(config, tc.tu, core.FrameRate{})
			if !errors.Is(err, tc.wantErr) || independent != tc.independent || (err == nil && (got.Width != 640 || got.Height != 360)) {
				t.Fatalf("TU: format=%+v independent=%v err=%v", got, independent, err)
			}
		})
	}
	// 4096 пустых OBU_TEMPORAL_DELIMITER допустимы по количеству, но не дают кадр;
	// ещё один OBU должен завершаться ошибкой бюджета, а не разбором всех элементов.
	if _, err := splitOBUs(bytes.Repeat([]byte{0x12, 0}, 4097)); !errors.Is(err, ErrLimit) {
		t.Fatalf("не ограничено число OBU: %v", err)
	}
}
