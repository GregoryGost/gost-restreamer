package fmp4

import (
	"bytes"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
)

func testLimits() Limits {
	return Limits{MaxBoxBytes: 1 << 20, MaxInitBytes: 1 << 20, MaxMoofBytes: 1 << 16, MaxMdatBytes: 1 << 20, MaxSampleBytes: 1 << 18, MaxExtradataBytes: 1 << 12, MaxSamples: 100}
}
func testInit(t testing.TB) Init {
	t.Helper()
	// Заголовки libx264 описывают High/yuv420p, 128×128 при 30 FPS.
	headers, err := hex.DecodeString("0164000bffe100186764000bacd942046c0440000003004000000f03c50a658001000668ebe3cb22c0fdf8f800")
	if err != nil {
		t.Fatal(err)
	}
	v, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: 11, Revision: 2, TimeBase: media.TimeBase{Numerator: 1, Denominator: 30}, Video: core.VideoFormat{Codec: core.CodecH264, Width: 128, Height: 128, FrameRate: core.FrameRate{Numerator: 30, Denominator: 1}, CodecProfile: "high", PixelFormat: "yuv420p", BitDepth: 8, Color: core.ColorInfo{Range: "limited", Primaries: "unspecified", Transfer: "unspecified", Matrix: "unspecified"}}}, headers)
	if err != nil {
		t.Fatal(err)
	}
	a, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackAudio, SourceEpoch: 11, Revision: 7, TimeBase: media.TimeBase{Numerator: 1, Denominator: 48000}, Audio: core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", SampleRateHz: 48000, Channels: 2, ChannelLayout: "stereo"}}, []byte{0x11, 0x90})
	if err != nil {
		t.Fatal(err)
	}
	return Init{Video: v, Audio: a, Origin: OriginMapping{OffsetTicks: 3, TimeBase: media.TimeBase{Numerator: 1, Denominator: 1}}}
}
func packet(t testing.TB, c media.CodecConfig, info media.PacketInfo, payload []byte) media.Packet {
	t.Helper()
	p, err := media.NewPacket(info, c, payload)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

type shortWriter struct {
	w io.Writer
	n int
}

func (w shortWriter) Write(b []byte) (int, error) {
	if len(b) > w.n {
		b = b[:w.n]
	}
	return w.w.Write(b)
}

type shortReader struct {
	r io.Reader
	n int
}

func (r shortReader) Read(b []byte) (int, error) {
	if len(b) > r.n {
		b = b[:r.n]
	}
	return r.r.Read(b)
}

func TestStreamingRoundTripPreservesSignedTimestampsAndConfig(t *testing.T) {
	init := testInit(t)
	var buffer bytes.Buffer
	m, err := NewMuxer(shortWriter{&buffer, 3}, init, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	packets := []media.Packet{packet(t, init.Video, media.PacketInfo{DTS: -60, PTS: -57, DurationTicks: 1, Keyframe: true}, []byte{1, 2, 3}), packet(t, init.Audio, media.PacketInfo{DTS: -95000, PTS: -95001, DurationTicks: 1024}, []byte{4, 5}), packet(t, init.Video, media.PacketInfo{DTS: -59, PTS: -61, DurationTicks: 1}, []byte{6, 7})}
	for _, p := range packets {
		if err = m.WritePacket(p); err != nil {
			t.Fatal(err)
		}
	}
	d, err := NewDemuxer(shortReader{bytes.NewReader(buffer.Bytes()), 2}, DemuxOptions{Limits: testLimits()})
	if err != nil {
		t.Fatal(err)
	}
	for index, want := range packets {
		got, err := d.Next()
		if err != nil {
			t.Fatal(err)
		}
		if got.Info() != want.Info() || !sameConfig(got.Config(), want.Config()) || !bytes.Equal(got.Payload(), want.Payload()) {
			t.Fatalf("пакет %d изменился: %+v != %+v", index, got.Info(), want.Info())
		}
	}
	if _, err = d.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("ожидался EOF: %v", err)
	}
}
func TestPacketRejectionPrecedesIO(t *testing.T) {
	init := testInit(t)
	var output bytes.Buffer
	m, err := NewMuxer(&output, init, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	before := output.Len()
	for _, info := range []media.PacketInfo{{DTS: math.MinInt64, PTS: 0}, {DTS: 0, PTS: math.MaxInt64}, {DTS: 0, PTS: 0, DurationTicks: math.MaxInt64}} {
		if err = m.WritePacket(packet(t, init.Video, info, []byte{1})); !errors.Is(err, ErrUnsupportedTiming) {
			t.Fatalf("не отвергнуто время: %v", err)
		}
		if output.Len() != before {
			t.Fatal("ошибочный пакет попал в транспорт")
		}
	}
	validator := NewFFmpegTimingValidator()
	p := packet(t, init.Video, media.PacketInfo{PTS: 0, DTS: 1}, []byte{1})
	if err = validator.Validate(p); !errors.Is(err, ErrUnsupportedTiming) {
		t.Fatal(err)
	}
	p = packet(t, init.Video, media.PacketInfo{PTS: 2, DTS: 1}, []byte{1})
	if err = validator.Validate(p); err != nil {
		t.Fatal(err)
	}
	if err = validator.Validate(p); !errors.Is(err, ErrUnsupportedTiming) {
		t.Fatal(err)
	}
}
func TestDeclaredLimitsPrecedeBodyReads(t *testing.T) {
	l := testLimits()
	data := join(u32(uint32(l.MaxBoxBytes+1)), []byte("ftyp"))
	_, err := NewDemuxer(bytes.NewReader(data), DemuxOptions{Limits: l})
	if !errors.Is(err, ErrLimit) {
		t.Fatalf("размер должен быть отклонён до EOF: %v", err)
	}
	init := testInit(t)
	var output bytes.Buffer
	m, err := NewMuxer(&output, init, l)
	if err != nil {
		t.Fatal(err)
	}
	if err = m.WritePacket(packet(t, init.Video, media.PacketInfo{PTS: 0, DTS: 0}, []byte{1, 2})); err != nil {
		t.Fatal(err)
	}
	data = bytes.Clone(output.Bytes())
	top, err := children(data)
	if err != nil {
		t.Fatal(err)
	}
	position := 0
	for _, b := range top {
		if b.kind == "mdat" {
			be.PutUint32(data[position:position+4], uint32(l.MaxMdatBytes+1))
			data = data[:position+8]
			break
		}
		position += int(b.size)
	}
	d, err := NewDemuxer(bytes.NewReader(data), DemuxOptions{Limits: l})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Next(); !errors.Is(err, ErrLimit) {
		t.Fatalf("mdat должен быть отклонён до чтения: %v", err)
	}
}
func TestDisconnectAndStickyWriteFailure(t *testing.T) {
	init := testInit(t)
	var output bytes.Buffer
	m, err := NewMuxer(&output, init, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	if err = m.WritePacket(packet(t, init.Video, media.PacketInfo{}, []byte{1, 2, 3})); err != nil {
		t.Fatal(err)
	}
	data := output.Bytes()
	d, err := NewDemuxer(bytes.NewReader(data[:len(data)-1]), DemuxOptions{Limits: testLimits()})
	if err != nil {
		t.Fatal(err)
	}
	if _, err = d.Next(); !errors.Is(err, io.ErrUnexpectedEOF) {
		t.Fatal(err)
	}
	m.w = failingWriter{}
	if err = m.WritePacket(packet(t, init.Audio, media.PacketInfo{}, []byte{1})); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal(err)
	}
	m.w = &output
	if err = m.WritePacket(packet(t, init.Audio, media.PacketInfo{}, []byte{1})); !errors.Is(err, io.ErrClosedPipe) {
		t.Fatal("повторная запись после частичной ошибки разрешена")
	}
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) { return 0, io.ErrClosedPipe }
func TestExactOriginAndTimescale(t *testing.T) {
	if _, err := MovieTimeScale(media.TimeBase{Numerator: 1, Denominator: math.MaxUint32}, media.TimeBase{Numerator: 1, Denominator: math.MaxUint32 - 1}); !errors.Is(err, ErrUnsupportedTiming) {
		t.Fatal(err)
	}
	init := testInit(t)
	init.Origin = OriginMapping{OffsetTicks: 1, TimeBase: media.TimeBase{Numerator: 1, Denominator: 7}}
	var b bytes.Buffer
	if _, err := NewMuxer(&b, init, testLimits()); !errors.Is(err, ErrUnsupportedTiming) || b.Len() != 0 {
		t.Fatalf("непредставимый общий origin: %v", err)
	}
}

func TestFFmpegMappedRangeRejectsOverflowBeforeAdvancing(t *testing.T) {
	init := testInit(t)
	v := NewFFmpegTimingValidatorForOrigin(OriginMapping{OffsetTicks: 1, TimeBase: init.Video.Info().TimeBase})
	bad := packet(t, init.Video, media.PacketInfo{DTS: math.MaxInt64 - 1, PTS: math.MaxInt64}, []byte{1})
	if err := v.Validate(bad); !errors.Is(err, ErrUnsupportedTiming) {
		t.Fatal(err)
	}
	if err := v.Validate(packet(t, init.Video, media.PacketInfo{DTS: 0, PTS: 1}, []byte{1})); err != nil {
		t.Fatal(err)
	}
}
func TestAV1OmitsCompositionOffsets(t *testing.T) {
	init := testInit(t)
	info := init.Video.Info()
	info.Video.Codec = core.CodecAV1
	var err error
	init.Video, err = media.NewCodecConfig(info, []byte{0x81, 0, 0, 0})
	if err != nil {
		t.Fatal(err)
	}
	var out bytes.Buffer
	m, err := NewMuxer(&out, init, testLimits())
	if err != nil {
		t.Fatal(err)
	}
	before := out.Len()
	if err = m.WritePacket(packet(t, init.Video, media.PacketInfo{PTS: 1, DTS: 0}, []byte{1})); !errors.Is(err, ErrUnsupportedTiming) || out.Len() != before {
		t.Fatal(err)
	}
	if err = m.WritePacket(packet(t, init.Video, media.PacketInfo{Keyframe: true}, []byte{1})); err != nil {
		t.Fatal(err)
	}
	moof, err := find(out.Bytes(), "moof")
	if err != nil {
		t.Fatal(err)
	}
	traf, err := find(moof.data, "traf")
	if err != nil {
		t.Fatal(err)
	}
	trun, err := find(traf.data, "trun")
	if err != nil {
		t.Fatal(err)
	}
	f, _ := flags(trun.data)
	if f&0x800 != 0 {
		t.Fatal("AV1 trun содержит composition offset")
	}
}

func TestSampleDeclarationsAndOffsetsFailBeforePayloadRead(t *testing.T) {
	for _, kind := range []string{"sample-count", "sample-size", "data-offset"} {
		t.Run(kind, func(t *testing.T) {
			init := testInit(t)
			var wire bytes.Buffer
			m, err := NewMuxer(&wire, init, testLimits())
			if err != nil {
				t.Fatal(err)
			}
			if err = m.WritePacket(packet(t, init.Video, media.PacketInfo{}, []byte{1, 2, 3})); err != nil {
				t.Fatal(err)
			}
			data := bytes.Clone(wire.Bytes())
			moof, err := find(data, "moof")
			if err != nil {
				t.Fatal(err)
			}
			traf, err := find(moof.data, "traf")
			if err != nil {
				t.Fatal(err)
			}
			trun, err := find(traf.data, "trun")
			if err != nil {
				t.Fatal(err)
			}
			expected := ErrLimit
			switch kind {
			case "sample-count":
				be.PutUint32(trun.data[4:8], math.MaxUint32)
			case "sample-size":
				be.PutUint32(trun.data[16:20], math.MaxUint32)
			case "data-offset":
				be.PutUint32(trun.data[8:12], math.MaxUint32)
				expected = ErrInvalid
			}
			// Заголовок mdat присутствует, но само тело намеренно не передаётся.
			data = data[:len(data)-3]
			d, err := NewDemuxer(bytes.NewReader(data), DemuxOptions{Limits: testLimits()})
			if err != nil {
				t.Fatal(err)
			}
			if _, err = d.Next(); !errors.Is(err, expected) {
				t.Fatalf("нарушение %s дошло до payload: %v", kind, err)
			}
		})
	}
}

func TestConfigMetadataPreservesContractualStringBytes(t *testing.T) {
	init := testInit(t)
	info := init.Video.Info()
	info.Video.CodecProfile = "high\xff"
	var err error
	init.Video, err = media.NewCodecConfig(info, init.Video.Headers())
	if err != nil {
		t.Fatal(err)
	}
	var wire bytes.Buffer
	if _, err = NewMuxer(&wire, init, testLimits()); err != nil {
		t.Fatal(err)
	}
	d, err := NewDemuxer(&wire, DemuxOptions{Limits: testLimits()})
	if err != nil {
		t.Fatal(err)
	}
	if d.Init().Video.Info() != init.Video.Info() {
		t.Fatal("JSON изменил байты контрактных строк")
	}
}

func TestH264ContradictoryDimensionsFailBeforeInitWrite(t *testing.T) {
	for _, field := range []string{"width", "height"} {
		t.Run(field, func(t *testing.T) {
			init := testInit(t)
			info := init.Video.Info()
			if field == "width" {
				info.Video.Width = 144
			} else {
				info.Video.Height = 144
			}
			var err error
			init.Video, err = media.NewCodecConfig(info, init.Video.Headers())
			if err != nil {
				t.Fatal(err)
			}
			var wire bytes.Buffer
			if _, err := NewMuxer(&wire, init, testLimits()); !errors.Is(err, ErrInvalid) || wire.Len() != 0 {
				t.Fatalf("описание вопреки SPS попало в транспорт: %v, bytes=%d", err, wire.Len())
			}
		})
	}
}

func TestDemuxH264SPSDimensionsConstrainEveryMetadataSource(t *testing.T) {
	for _, source := range []string{"uuid", "resolver", "default"} {
		t.Run(source, func(t *testing.T) {
			init := testInit(t)
			var wire bytes.Buffer
			if _, err := NewMuxer(&wire, init, testLimits()); err != nil {
				t.Fatal(err)
			}
			data := bytes.Clone(wire.Bytes())
			entry := bytes.Index(data, []byte("avc1"))
			if entry < 4 || be.Uint16(data[entry+28:entry+30]) != 128 {
				t.Fatal("нет ожидаемой video sample entry")
			}
			be.PutUint16(data[entry+28:entry+30], 144)
			options := DemuxOptions{Limits: testLimits(), SourceEpoch: 1, VideoRevision: 1, AudioRevision: 1}
			if source == "uuid" {
				if bytes.Count(data, []byte(`"Width":128`)) != 1 {
					t.Fatal("нет ожидаемого размера в UUID")
				}
				data = bytes.Replace(data, []byte(`"Width":128`), []byte(`"Width":144`), 1)
			} else {
				uuid := bytes.Index(data, []byte("uuid"))
				if uuid < 4 {
					t.Fatal("нет ожидаемого UUID")
				}
				copy(data[uuid:uuid+4], "free")
				if source == "resolver" {
					options.ResolveConfig = func(desc TrackDescription) (media.CodecConfig, error) {
						if desc.Track == media.TrackAudio {
							return ResolveCodecConfig(desc, 1, 1)
						}
						info := init.Video.Info()
						info.Video.Width = desc.Width
						return media.NewCodecConfig(info, desc.Headers)
					}
				}
			}
			if _, err := NewDemuxer(bytes.NewReader(data), options); !errors.Is(err, ErrInvalid) {
				t.Fatalf("%s обошёл проверку SPS: %v", source, err)
			}
		})
	}
}

func FuzzBoundedDemuxer(f *testing.F) {
	init := testInit(f)
	var wire bytes.Buffer
	m, err := NewMuxer(&wire, init, testLimits())
	if err != nil {
		f.Fatal(err)
	}
	if err = m.WritePacket(packet(f, init.Video, media.PacketInfo{Keyframe: true}, []byte{1, 2, 3})); err != nil {
		f.Fatal(err)
	}
	f.Add(wire.Bytes())
	f.Add([]byte{})
	f.Fuzz(func(t *testing.T, data []byte) {
		if len(data) > 8192 {
			t.Skip()
		}
		d, err := NewDemuxer(bytes.NewReader(data), DemuxOptions{Limits: testLimits(), SourceEpoch: 1, VideoRevision: 1, AudioRevision: 1})
		if err != nil {
			return
		}
		for i := 0; i < 100; i++ {
			if _, err = d.Next(); err != nil {
				return
			}
		}
		t.Fatal("ограниченный ввод не завершился")
	})
}
