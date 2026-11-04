package fmp4_test

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

// Контейнеры построены по структуре ISOBMFF независимо от рабочего mux.
// Семантика elst/tfdt/trun сверена с FFmpeg n7.1.5, libavformat/mov.c.
func contractWords(values ...uint32) []byte {
	b := make([]byte, 4*len(values))
	for i, v := range values {
		binary.BigEndian.PutUint32(b[4*i:], v)
	}
	return b
}

func contractBox(kind string, parts ...[]byte) []byte {
	b := make([]byte, 8)
	copy(b[4:], kind)
	for _, part := range parts {
		b = append(b, part...)
	}
	binary.BigEndian.PutUint32(b, uint32(len(b)))
	return b
}

func contractFull(kind string, version byte, flags uint32, parts ...[]byte) []byte {
	return contractBox(kind, append([]byte{version}, contractWords(flags)[1:]...), bytes.Join(parts, nil))
}

func contractHeaders(t *testing.T) []byte {
	t.Helper()
	b, err := hex.DecodeString("0164000bffe100186764000bacd942046c0440000003004000000f03c50a658001000668ebe3cb22c0fdf8f800")
	if err != nil {
		t.Fatal(err)
	}
	return b
}

func contractTrack(t *testing.T, audio bool, version byte, edits []byte) []byte {
	t.Helper()
	id, scale, handler := uint32(9), uint32(30000), "vide"
	entry := make([]byte, 78)
	binary.BigEndian.PutUint16(entry[6:], 1)
	binary.BigEndian.PutUint16(entry[24:], 128)
	binary.BigEndian.PutUint16(entry[26:], 128)
	entryBox := contractBox("avc1", entry, contractBox("avcC", contractHeaders(t)))
	mediaHeader := contractFull("vmhd", 0, 1, make([]byte, 8))
	if audio {
		id, scale, handler = 2, 48000, "soun"
		entry = make([]byte, 28)
		binary.BigEndian.PutUint16(entry[6:], 1)
		binary.BigEndian.PutUint16(entry[16:], 2)
		binary.BigEndian.PutUint16(entry[18:], 16)
		binary.BigEndian.PutUint32(entry[24:], 48000<<16)
		// ES_Descriptor содержит DecoderConfigDescriptor с DecoderSpecificInfo для AAC-LC, 48 кГц и двух каналов.
		es := []byte{3, 25, 0, 2, 0, 4, 17, 0x40, 0x15, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 0, 5, 2, 0x11, 0x90, 6, 1, 2}
		entryBox = contractBox("mp4a", entry, contractFull("esds", 0, 0, es))
		mediaHeader = contractFull("smhd", 0, 0, make([]byte, 4))
	}
	tkhd, mdhd := make([]byte, 80), make([]byte, 20)
	idPos, scalePos := 8, 8
	if version == 1 {
		tkhd, mdhd = make([]byte, 92), make([]byte, 32)
		idPos, scalePos = 16, 16
	}
	binary.BigEndian.PutUint32(tkhd[idPos:], id)
	binary.BigEndian.PutUint32(mdhd[scalePos:], scale)
	matrixPos := 36
	if version == 1 {
		matrixPos = 48
	}
	binary.BigEndian.PutUint32(tkhd[matrixPos:], 0x10000)
	binary.BigEndian.PutUint32(tkhd[matrixPos+16:], 0x10000)
	binary.BigEndian.PutUint32(tkhd[matrixPos+32:], 0x40000000)
	if audio {
		binary.BigEndian.PutUint16(tkhd[matrixPos-4:], 0x100)
	} else {
		binary.BigEndian.PutUint32(tkhd[matrixPos+36:], 128<<16)
		binary.BigEndian.PutUint32(tkhd[matrixPos+40:], 128<<16)
	}
	binary.BigEndian.PutUint16(mdhd[len(mdhd)-4:], 0x55c4)
	dinf := contractBox("dinf", contractFull("dref", 0, 0, contractWords(1), contractFull("url ", 0, 1)))
	stbl := contractBox("stbl", contractFull("stsd", 0, 0, contractWords(1), entryBox), contractFull("stts", 0, 0, contractWords(0)), contractFull("stsc", 0, 0, contractWords(0)), contractFull("stsz", 0, 0, contractWords(0, 0)), contractFull("stco", 0, 0, contractWords(0)))
	mdia := contractBox("mdia", contractFull("mdhd", version, 0, mdhd), contractFull("hdlr", 0, 0, contractWords(0), []byte(handler), make([]byte, 13)), contractBox("minf", mediaHeader, dinf, stbl))
	return contractBox("trak", contractFull("tkhd", version, 7, tkhd), edits, mdia)
}

func contractInit(t *testing.T, version byte, edits []byte) []byte {
	t.Helper()
	mvhd := make([]byte, 96)
	pos := 8
	if version == 1 {
		mvhd, pos = make([]byte, 108), 16
	}
	binary.BigEndian.PutUint32(mvhd[pos:], 1000)
	ratePos := pos + 8
	if version == 1 {
		ratePos += 4
	}
	binary.BigEndian.PutUint32(mvhd[ratePos:], 0x10000)
	binary.BigEndian.PutUint16(mvhd[ratePos+4:], 0x100)
	binary.BigEndian.PutUint32(mvhd[ratePos+16:], 0x10000)
	binary.BigEndian.PutUint32(mvhd[ratePos+32:], 0x10000)
	binary.BigEndian.PutUint32(mvhd[ratePos+48:], 0x40000000)
	binary.BigEndian.PutUint32(mvhd[len(mvhd)-4:], 10)
	return append(contractBox("ftyp", []byte("iso6"), contractWords(0), []byte("iso6isom")), contractBox("moov", contractFull("mvhd", version, 0, mvhd), contractTrack(t, false, version, edits), contractTrack(t, true, version, nil), contractBox("mvex", contractFull("trex", 0, 0, contractWords(9, 1, 1000, 2, 0x10000)), contractFull("trex", 0, 0, contractWords(2, 1, 1024, 2, 0))))...)
}

func contractOptions() fmp4.DemuxOptions {
	return fmp4.DemuxOptions{Limits: acceptanceLimits(), SourceEpoch: 47, VideoRevision: 3, AudioRevision: 5}
}

// Перестройка меняет только первый блок по указанному пути, пересчитывая размеры предков.
func contractRewrite(t *testing.T, data []byte, path []string, change func([]byte) []byte) []byte {
	t.Helper()
	for pos := 0; pos+8 <= len(data); {
		n := int(binary.BigEndian.Uint32(data[pos:]))
		if n < 8 || n > len(data)-pos {
			t.Fatal("повреждена структура тестового контейнера")
		}
		if string(data[pos+4:pos+8]) == path[0] {
			body := bytes.Clone(data[pos+8 : pos+n])
			if len(path) == 1 {
				body = change(body)
			} else {
				body = contractRewrite(t, body, path[1:], change)
			}
			result := append([]byte(nil), data[:pos]...)
			result = append(result, contractBox(path[0], body)...)
			return append(result, data[pos+n:]...)
		}
		pos += n
	}
	t.Fatalf("нет блока %v в тестовом контейнере", path)
	return nil
}

func contractEdit(version byte, entries ...[3]int64) []byte {
	b := contractWords(uint32(len(entries)))
	for _, entry := range entries {
		if version == 1 {
			part := make([]byte, 16)
			binary.BigEndian.PutUint64(part, uint64(entry[0]))
			binary.BigEndian.PutUint64(part[8:], uint64(entry[1]))
			b = append(b, part...)
		} else {
			b = append(b, contractWords(uint32(entry[0]), uint32(entry[1]))...)
		}
		b = append(b, contractWords(uint32(entry[2]))...)
	}
	return contractBox("edts", contractFull("elst", version, 0, b))
}

func contractFragment(decode, dataOffset uint32, tfhdFlags uint32, tfhdFields []byte, truns ...[]byte) []byte {
	parts := [][]byte{contractFull("tfhd", 0, tfhdFlags, contractWords(9), tfhdFields), contractFull("tfdt", 0, 0, contractWords(decode))}
	parts = append(parts, truns...)
	if len(truns) == 0 {
		parts = append(parts, contractFull("trun", 0, 1, contractWords(1, dataOffset)))
	}
	return contractBox("moof", contractFull("mfhd", 0, 0, contractWords(1)), contractBox("traf", parts...))
}

func contractOneFragment(decode uint32) []byte {
	m := contractFragment(decode, 0, 0x20000, nil)
	m = contractFragment(decode, uint32(len(m)+8), 0x20000, nil)
	return append(m, contractBox("mdat", []byte{0x61, 0x62})...)
}

func TestContractIndependentInitDescriptionsAreSortedAndIsolated(t *testing.T) {
	for _, version := range []byte{0, 1} {
		t.Run(string(rune('0'+version)), func(t *testing.T) {
			wire := append(contractInit(t, version, nil), contractOneFragment(3000)...)
			d, err := fmp4.NewDemuxer(bytes.NewReader(wire), contractOptions())
			if err != nil {
				t.Fatal(err)
			}
			desc := d.Descriptions()
			if len(desc) != 2 || desc[0].ID != 2 || desc[1].ID != 9 || desc[0].Track != media.TrackAudio || desc[1].Width != 128 || desc[1].Height != 128 || desc[1].TimeBase != (media.TimeBase{Numerator: 1, Denominator: 30000}) || !bytes.Equal(desc[1].Headers, contractHeaders(t)) {
				t.Fatalf("неверные сведения: %+v", desc)
			}
			desc[0].Headers[0], desc[1].Headers[0], desc[1].ID = 0, 0, 0
			again := d.Descriptions()
			if again[1].ID != 9 || again[0].Headers[0] != 0x11 || again[1].Headers[0] != 1 {
				t.Fatal("Descriptions передаёт внутренние данные во владение вызывающего")
			}
			p, err := d.Next()
			if err != nil || p.Info() != (media.PacketInfo{DTS: 3000, PTS: 3000, DurationTicks: 1000}) || !bytes.Equal(p.Payload(), []byte("ab")) || p.Config().Info().SourceEpoch != 47 || p.Config().Headers()[0] != 1 {
				t.Fatalf("изменились пакет или конфигурация: %+v, %v", p.Info(), err)
			}
		})
	}
}

func TestContractInitRejectsAmbiguousAndMalformedTracks(t *testing.T) {
	set32 := func(offset int, value uint32) func([]byte) []byte {
		return func(b []byte) []byte { binary.BigEndian.PutUint32(b[offset:], value); return b }
	}
	cases := []struct {
		name   string
		path   []string
		change func([]byte) []byte
		want   error
	}{
		{"movie-timescale-zero", []string{"moov", "mvhd"}, set32(12, 0), fmp4.ErrInvalid},
		{"movie-version", []string{"moov", "mvhd"}, func(b []byte) []byte { b[0] = 2; return b }, fmp4.ErrInvalid},
		{"track-zero-id", []string{"moov", "trak", "tkhd"}, set32(12, 0), fmp4.ErrInvalid},
		{"track-duplicate-id", []string{"moov", "trak", "tkhd"}, set32(12, 2), fmp4.ErrInvalid},
		{"track-version", []string{"moov", "trak", "tkhd"}, func(b []byte) []byte { b[0] = 2; return b }, fmp4.ErrInvalid},
		{"media-timescale-zero", []string{"moov", "trak", "mdia", "mdhd"}, set32(12, 0), fmp4.ErrInvalid},
		{"media-version", []string{"moov", "trak", "mdia", "mdhd"}, func(b []byte) []byte { b[0] = 2; return b }, fmp4.ErrInvalid},
		{"handler-contradiction", []string{"moov", "trak", "mdia", "hdlr"}, func(b []byte) []byte { copy(b[8:12], "soun"); return b }, fmp4.ErrInvalid},
		{"missing-movie-header", []string{"moov"}, func(b []byte) []byte { copy(b[4:8], "free"); return b }, fmp4.ErrInvalid},
		{"duplicate-movie-header", []string{"moov"}, func(b []byte) []byte { n := binary.BigEndian.Uint32(b); return append(b, b[:n]...) }, fmp4.ErrInvalid},
		{"missing-track-header", []string{"moov", "trak"}, func(b []byte) []byte { copy(b[4:8], "free"); return b }, fmp4.ErrInvalid},
		{"duplicate-track-header", []string{"moov", "trak"}, func(b []byte) []byte { n := binary.BigEndian.Uint32(b); return append(b, b[:n]...) }, fmp4.ErrInvalid},
		{"missing-audio", []string{"moov"}, func(b []byte) []byte {
			return b[:int(binary.BigEndian.Uint32(b))+int(binary.BigEndian.Uint32(b[binary.BigEndian.Uint32(b):]))]
		}, fmp4.ErrInvalid},
		{"third-track", []string{"moov"}, func(b []byte) []byte { return append(b, contractTrack(t, true, 0, nil)...) }, fmp4.ErrLimit},
		{"unknown-default-track", []string{"moov", "mvex", "trex"}, set32(4, 99), fmp4.ErrInvalid},
		{"unknown-description-index", []string{"moov", "mvex", "trex"}, set32(8, 2), fmp4.ErrInvalid},
		{"truncated-defaults", []string{"moov", "mvex", "trex"}, func(b []byte) []byte { return b[:20] }, fmp4.ErrInvalid},
		{"trailing-partial-child", []string{"moov"}, func(b []byte) []byte { return append(b, 0) }, fmp4.ErrInvalid},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			wire := contractRewrite(t, contractInit(t, 0, nil), tc.path, tc.change)
			if _, err := fmp4.NewDemuxer(bytes.NewReader(wire), contractOptions()); !errors.Is(err, tc.want) {
				t.Fatalf("ожидалась %v, получена %v", tc.want, err)
			}
		})
	}
}

type contractReadCounter struct {
	r            io.Reader
	bytes, calls int
}

func (r *contractReadCounter) Read(p []byte) (int, error) {
	r.calls++
	n, err := r.r.Read(p)
	r.bytes += n
	return n, err
}

func TestContractFramingBudgetsRejectBeforeBodyAndRemainSticky(t *testing.T) {
	for _, kind := range []string{"moof", "free", "mdat"} {
		t.Run(kind, func(t *testing.T) {
			o := contractOptions()
			wire := contractInit(t, 0, nil)
			if kind == "mdat" {
				m := contractFragment(0, 0, 0x20000, nil)
				m = contractFragment(0, uint32(len(m)+8), 0x20000, nil)
				wire = append(wire, m...)
			}
			limit := o.Limits.MaxMoofBytes
			if kind == "free" {
				limit = o.Limits.MaxBoxBytes
			}
			if kind == "mdat" {
				limit = o.Limits.MaxMdatBytes
			}
			wire = append(wire, contractWords(uint32(limit+1))...)
			wire = append(wire, kind...)
			r := &contractReadCounter{r: bytes.NewReader(wire)}
			d, err := fmp4.NewDemuxer(r, o)
			if err != nil {
				t.Fatal(err)
			}
			_, first := d.Next()
			if !errors.Is(first, fmp4.ErrLimit) || r.bytes != len(wire) {
				t.Fatalf("бюджет не проверен на заголовке: %v, прочитано %d/%d", first, r.bytes, len(wire))
			}
			calls := r.calls
			if _, err = d.Next(); err != first || r.calls != calls {
				t.Fatalf("повторное чтение после ошибки: %v, calls=%d/%d", err, r.calls, calls)
			}
		})
	}
	for _, extended := range []bool{false, true} {
		o := contractOptions()
		prefix := contractBox("ftyp", []byte("iso6"), contractWords(0))
		o.Limits.MaxInitBytes = 4096
		header := append(contractWords(4096), []byte("moov")...)
		if extended {
			header = append(contractWords(1), []byte("moov")...)
			header = append(header, []byte{0, 0, 0, 0, 0, 0, 16, 0}...)
		}
		wire := append(prefix, header...)
		r := &contractReadCounter{r: bytes.NewReader(wire)}
		if _, err := fmp4.NewDemuxer(r, o); !errors.Is(err, fmp4.ErrLimit) || r.bytes != len(wire) {
			t.Fatalf("общий init-бюджет не проверен до тела: %v", err)
		}
	}
}

func TestContractEditOriginsUseMovieAndTrackTimeScalesExactly(t *testing.T) {
	for _, version := range []byte{0, 1} {
		for _, tc := range []struct {
			name    string
			entries [][3]int64
			want    int64
		}{
			{"media-origin", [][3]int64{{0, 9000, 65536}}, -6000},
			// Пауза 250/1000 s и media_time=1500/30000 s дают начало 0.3 s.
			{"positive-gap", [][3]int64{{250, -1, 65536}, {0, 1500, 65536}}, 9000},
		} {
			t.Run(tc.name+string(rune('0'+version)), func(t *testing.T) {
				wire := append(contractInit(t, version, contractEdit(version, tc.entries...)), contractOneFragment(3000)...)
				d, err := fmp4.NewDemuxer(bytes.NewReader(wire), contractOptions())
				if err != nil {
					t.Fatal(err)
				}
				p, err := d.Next()
				if err != nil || p.Info().PTS != tc.want || p.Info().DTS != tc.want || p.Info().DurationTicks != 1000 {
					t.Fatalf("ожидалось DTS=PTS=%d, получено %+v, %v", tc.want, p.Info(), err)
				}
			})
		}
	}
	for _, tc := range []struct {
		name    string
		version byte
		entries [][3]int64
		want    error
	}{
		{"unsupported-version", 2, [][3]int64{{0, 0, 65536}}, fmp4.ErrInvalid},
		{"zero-edits", 0, nil, fmp4.ErrUnsupportedTiming},
		{"non-unit-rate", 0, [][3]int64{{0, 0, 32768}}, fmp4.ErrUnsupportedTiming},
		{"gap-after-media", 0, [][3]int64{{0, 0, 65536}, {1, -1, 65536}}, fmp4.ErrUnsupportedTiming},
		{"negative-media-time", 0, [][3]int64{{0, -2, 65536}}, fmp4.ErrUnsupportedTiming},
		{"two-media-origins", 0, [][3]int64{{0, 0, 65536}, {0, 1, 65536}}, fmp4.ErrUnsupportedTiming},
		{"gap-only", 0, [][3]int64{{10, -1, 65536}}, fmp4.ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := fmp4.NewDemuxer(bytes.NewReader(contractInit(t, 0, contractEdit(tc.version, tc.entries...))), contractOptions())
			if !errors.Is(err, tc.want) {
				t.Fatalf("ожидалась %v, получена %v", tc.want, err)
			}
		})
	}
	wire := contractInit(t, 0, contractEdit(0, [3]int64{1, -1, 65536}, [3]int64{0, 0, 65536}))
	wire = contractRewrite(t, wire, []string{"moov", "mvhd"}, func(b []byte) []byte { binary.BigEndian.PutUint32(b[12:], 1001); return b })
	if _, err := fmp4.NewDemuxer(bytes.NewReader(wire), contractOptions()); !errors.Is(err, fmp4.ErrUnsupportedTiming) {
		t.Fatalf("нецелая пауза была округлена: %v", err)
	}
}

func TestContractFragmentDefaultsRunsAndPayloadGaps(t *testing.T) {
	// tfhd переопределяет trex: 40 тактов, 2 байта; первый пакет допускает независимый старт, второй зависит от него.
	build := func(offset uint32) []byte {
		return contractFragment(3000, 0, 0x2003a, contractWords(1, 40, 2, 0x10000), contractFull("trun", 0, 5, contractWords(1, offset, 0)), contractFull("trun", 0, 0, contractWords(1)))
	}
	m := build(0)
	m = build(uint32(len(m) + 8 + 3))
	wire := contractInit(t, 0, nil)
	wire = append(wire, contractBox("styp", []byte("iso6"), contractWords(0), []byte("iso6"))...)
	wire = append(wire, contractBox("free", []byte{0, 0, 0, 0})...)
	wire = append(wire, contractFull("sidx", 0, 0, contractWords(9, 30000, 0, 0, 0))...)
	wire = append(wire, m...)
	wire = append(wire, contractBox("mdat", []byte{9, 9, 9, 'a', 'b', 'c', 'd', 8, 8})...)
	wire = append(wire, contractOneFragment(4000)...)
	wire = append(wire, contractBox("mfra", contractFull("mfro", 0, 0, contractWords(24)))...)
	d, err := fmp4.NewDemuxer(bytes.NewReader(wire), contractOptions())
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []struct {
		info    media.PacketInfo
		payload string
	}{{media.PacketInfo{DTS: 3000, PTS: 3000, DurationTicks: 40, Keyframe: true}, "ab"}, {media.PacketInfo{DTS: 3040, PTS: 3040, DurationTicks: 40}, "cd"}, {media.PacketInfo{DTS: 4000, PTS: 4000, DurationTicks: 1000}, "ab"}} {
		p, err := d.Next()
		if err != nil || p.Info() != want.info || string(p.Payload()) != want.payload {
			t.Fatalf("пакет %d: %+v, %q, %v", i, p.Info(), p.Payload(), err)
		}
	}
	if _, err := d.Next(); !errors.Is(err, io.EOF) {
		t.Fatal(err)
	}
}

func TestContractMalformedFragmentFailsWithoutReadingPayload(t *testing.T) {
	for _, tc := range []struct {
		name   string
		path   []string
		change func([]byte) []byte
	}{
		{"missing-base-offset", []string{"moof", "traf", "tfhd"}, func(b []byte) []byte { b[1], b[2], b[3] = 0, 0, 0; return b }},
		{"unknown-tfhd-flags", []string{"moof", "traf", "tfhd"}, func(b []byte) []byte { b[3] |= 0x40; return b }},
		{"unknown-track", []string{"moof", "traf", "tfhd"}, func(b []byte) []byte { binary.BigEndian.PutUint32(b[4:], 99); return b }},
		{"tfdt-version", []string{"moof", "traf", "tfdt"}, func(b []byte) []byte { b[0] = 2; return b }},
		{"tfdt-length", []string{"moof", "traf", "tfdt"}, func(b []byte) []byte { return append(b, 0) }},
		{"trun-version", []string{"moof", "traf", "trun"}, func(b []byte) []byte { b[0] = 2; return b }},
		{"unknown-trun-flags", []string{"moof", "traf", "trun"}, func(b []byte) []byte { b[3] |= 0x10; return b }},
		{"missing-run-offset", []string{"moof", "traf", "trun"}, func(b []byte) []byte { b[3] = 0; return b[:8] }},
		{"trailing-run-fields", []string{"moof", "traf", "trun"}, func(b []byte) []byte { return append(b, 0, 0, 0, 0) }},
	} {
		t.Run(tc.name, func(t *testing.T) {
			fragment := contractRewrite(t, contractOneFragment(0), tc.path, tc.change)
			r := &contractReadCounter{r: bytes.NewReader(append(contractInit(t, 0, nil), fragment...))}
			d, err := fmp4.NewDemuxer(r, contractOptions())
			if err != nil {
				t.Fatal(err)
			}
			_, first := d.Next()
			if !errors.Is(first, fmp4.ErrInvalid) {
				t.Fatalf("ожидалась ErrInvalid, получена %v", first)
			}
			if r.bytes > len(contractInit(t, 0, nil))+len(fragment)-2 {
				t.Fatal("payload прочитан до проверки структуры фрагмента")
			}
			calls := r.calls
			if _, err := d.Next(); err != first || r.calls != calls {
				t.Fatalf("ошибка demux не сохранена: %v", err)
			}
		})
	}
}

func TestContractInterleavedTrackPayloadUsesOffsetsInsteadOfDeclarationOrder(t *testing.T) {
	build := func(offset uint32) []byte {
		video := contractBox("traf", contractFull("tfhd", 0, 0x20000, contractWords(9)), contractFull("tfdt", 0, 0, contractWords(3000)), contractFull("trun", 0, 1, contractWords(1, offset+2)))
		audio := contractBox("traf", contractFull("tfhd", 0, 0x20000, contractWords(2)), contractFull("tfdt", 0, 0, contractWords(4800)), contractFull("trun", 0, 1, contractWords(1, offset)))
		return contractBox("moof", contractFull("mfhd", 0, 0, contractWords(1)), video, audio)
	}
	m := build(0)
	m = build(uint32(len(m) + 8))
	wire := append(contractInit(t, 0, nil), m...)
	wire = append(wire, contractBox("mdat", []byte("CDab"))...)
	d, err := fmp4.NewDemuxer(bytes.NewReader(wire), contractOptions())
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range []struct {
		track   media.Track
		info    media.PacketInfo
		payload string
	}{
		{media.TrackAudio, media.PacketInfo{DTS: 4800, PTS: 4800, DurationTicks: 1024}, "CD"},
		{media.TrackVideo, media.PacketInfo{DTS: 3000, PTS: 3000, DurationTicks: 1000}, "ab"},
	} {
		p, err := d.Next()
		if err != nil || p.Config().Info().Track != want.track || p.Info() != want.info || string(p.Payload()) != want.payload {
			t.Fatalf("пакет %d прочитан с чужого смещения: %+v, %q, %v", i, p.Info(), p.Payload(), err)
		}
	}
}

func TestContractAbsoluteBaseAndSignedRunOffset(t *testing.T) {
	init := contractInit(t, 0, nil)
	build := func(base uint32) []byte {
		return contractFragment(3000, 0, 0x3b, contractWords(0, base, 1, 40, 2, 0), contractFull("trun", 0, 1, contractWords(1, 0xfffffff6)))
	}
	m := build(0)
	// data_offset=-10 возвращает абсолютную базу к началу payload.
	m = build(uint32(len(init) + len(m) + 8 + 10))
	wire := append(init, m...)
	wire = append(wire, contractBox("mdat", []byte("ab"))...)
	d, err := fmp4.NewDemuxer(bytes.NewReader(wire), contractOptions())
	if err != nil {
		t.Fatal(err)
	}
	p, err := d.Next()
	if err != nil || p.Info() != (media.PacketInfo{DTS: 3000, PTS: 3000, DurationTicks: 40, Keyframe: true}) || string(p.Payload()) != "ab" {
		t.Fatalf("абсолютная база или signed offset искажены: %+v, %q, %v", p.Info(), p.Payload(), err)
	}
}

func TestContractTopLevelFramingRejectsInvalidHeaders(t *testing.T) {
	for _, tc := range []struct {
		name string
		wire []byte
		want error
	}{
		{"zero-size", append(contractWords(0), []byte("ftyp")...), fmp4.ErrInvalid},
		{"size-below-header", append(contractWords(7), []byte("ftyp")...), fmp4.ErrInvalid},
		{"short-extended-size", append(contractWords(1), []byte("ftyp\x00")...), io.ErrUnexpectedEOF},
		{"extended-below-header", append(append(contractWords(1), []byte("ftyp")...), contractWords(0, 15)...), fmp4.ErrInvalid},
		{"movie-before-file-type", contractInit(t, 0, nil)[24:], fmp4.ErrInvalid},
		{"duplicate-file-type", append(contractBox("ftyp", []byte("iso6"), contractWords(0)), contractInit(t, 0, nil)...), fmp4.ErrInvalid},
		{"unexpected-init-box", contractBox("mdat"), fmp4.ErrInvalid},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := fmp4.NewDemuxer(bytes.NewReader(tc.wire), contractOptions()); !errors.Is(err, tc.want) {
				t.Fatalf("ожидалась %v, получена %v", tc.want, err)
			}
		})
	}
	options := contractOptions()
	options.Limits.MaxExtradataBytes = 4
	if _, err := fmp4.NewDemuxer(bytes.NewReader(contractInit(t, 0, nil)), options); !errors.Is(err, fmp4.ErrLimit) {
		t.Fatalf("ограничение extradata проигнорировано: %v", err)
	}
}

func TestContractDemuxDisconnectDoesNotResumeReads(t *testing.T) {
	wire := append(contractInit(t, 0, nil), contractOneFragment(3000)...)
	r := &contractReadCounter{r: bytes.NewReader(wire[:len(wire)-1])}
	d, err := fmp4.NewDemuxer(r, contractOptions())
	if err != nil {
		t.Fatal(err)
	}
	p, first := d.Next()
	if !errors.Is(first, io.ErrUnexpectedEOF) || p.Validate() == nil {
		t.Fatalf("частичный payload опубликован: %+v, %v", p.Info(), first)
	}
	calls := r.calls
	if _, err := d.Next(); err != first || r.calls != calls {
		t.Fatalf("чтение возобновилось после разрыва: %v, calls=%d/%d", err, r.calls, calls)
	}
}
