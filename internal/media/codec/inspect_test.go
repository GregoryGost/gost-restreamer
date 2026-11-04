package codec

import (
	"encoding/hex"
	"errors"
	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"testing"
)

func h264Fixture(t *testing.T) VideoDescription {
	t.Helper()
	headers, err := hex.DecodeString("0164000bffe100186764000bacd942046c0440000003004000000f03c50a658001000668ebe3cb22c0fdf8f800")
	if err != nil {
		t.Fatal(err)
	}
	return VideoDescription{Headers: headers, Width: 128, Height: 128}
}
func TestH264GateRequiresSlicePPSAndActualIDR(t *testing.T) {
	desc := h264Fixture(t)
	format, err := H264Format(desc)
	if err != nil {
		t.Fatal(err)
	}
	c, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: 1, Revision: 1, TimeBase: media.TimeBase{Numerator: 1, Denominator: 30}, Video: format}, desc.Headers)
	if err != nil {
		t.Fatal(err)
	}
	// first_mb_in_slice=0, slice_type=I, PPS=0, frame_num=0, idr_pic_id=0.
	for _, tc := range []struct {
		payload    []byte
		key, valid bool
	}{{[]byte{0, 0, 0, 3, 0x65, 0xb8, 0x40}, true, true}, {[]byte{0, 0, 0, 3, 0x41, 0xb8, 0x40}, false, true}, {[]byte{0, 0, 0, 1, 0x65}, false, false}, {[]byte{0, 0, 0, 3, 0x65, 0xb4, 0x40}, false, false}, {[]byte{0, 0, 0, 1, 0xe5}, false, false}, {[]byte{0, 0, 0, 1, 0x09}, false, false}, {[]byte{0, 0, 0, 9, 0x65}, false, false}} {
		key, err := InspectH264AU(desc, tc.payload)
		if (err == nil) != tc.valid || key != tc.key {
			t.Fatalf("AU=%x key=%v err=%v", tc.payload, key, err)
		}
		p, err := media.NewPacket(media.PacketInfo{DTS: 0, PTS: 0, DurationTicks: 1, Keyframe: !tc.key}, c, tc.payload)
		if err != nil {
			t.Fatal(err)
		}
		var inspector Inspector
		key, err = inspector.Inspect(p)
		if (err == nil) != tc.valid || key != tc.key {
			t.Fatalf("флаг sample заменил разбор AU: %v %v", key, err)
		}
	}
	bad := h264Fixture(t)
	bad.Headers[36] = 0x67
	if _, err := InspectH264AU(bad, []byte{0, 0, 0, 3, 0x65, 0xb8, 0x40}); err == nil {
		t.Fatal("PPS другого типа принят")
	}
}
func TestAACBoundariesAndActualConfig(t *testing.T) {
	headers := []byte{0x11, 0x90}
	format, err := AudioFormatASC(headers)
	if err != nil {
		t.Fatal(err)
	}
	c, _ := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackAudio, SourceEpoch: 1, Revision: 1, TimeBase: media.TimeBase{Numerator: 1, Denominator: 48000}, Audio: format}, headers)
	var inspector Inspector
	p, _ := media.NewPacket(media.PacketInfo{DurationTicks: 1024}, c, []byte{0x21, 0x10, 0x04, 0x60, 0x8c, 0x1c})
	if _, err = inspector.Inspect(p); err != nil {
		t.Fatal(err)
	}
	for _, raw := range [][]byte{nil, {0xff, 0xf1}, []byte("ADIFbad")} {
		if err = ValidateAACAU(headers, raw); !errors.Is(err, ErrCodec) {
			t.Fatal(err)
		}
	}
	format.SampleRateHz = 44100
	bad, _ := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackAudio, SourceEpoch: 1, Revision: 1, TimeBase: media.TimeBase{Numerator: 1, Denominator: 48000}, Audio: format}, headers)
	p, _ = media.NewPacket(media.PacketInfo{}, bad, []byte{1})
	if _, err = inspector.Inspect(p); !errors.Is(err, ErrCodec) {
		t.Fatal(err)
	}
	if err = Compatible(core.MediaFormat{}, core.MediaFormat{}); !errors.Is(err, ErrCodec) {
		t.Fatal(err)
	}
}

type ppsBits struct {
	raw   []byte
	count int
}

func (b *ppsBits) put(v uint32, n int) {
	for i := n - 1; i >= 0; i-- {
		if b.count%8 == 0 {
			b.raw = append(b.raw, 0)
		}
		b.raw[b.count/8] |= byte(v>>uint(i)&1) << uint(7-b.count%8)
		b.count++
	}
}
func (b *ppsBits) ue(v uint32) {
	v++
	width := 0
	for n := v; n > 0; n >>= 1 {
		width++
	}
	b.put(0, width-1)
	b.put(v, width)
}
func (b *ppsBits) se(v int32) {
	if v <= 0 {
		b.ue(uint32(-2 * v))
	} else {
		b.ue(uint32(2*v - 1))
	}
}
func (b *ppsBits) tail() {
	b.put(1, 1)
	for b.count%8 != 0 {
		b.put(0, 1)
	}
}
func ppsFixture(l0, bipred, groups uint32, qp, chroma int32, extension, scaling bool) []byte {
	b := ppsBits{}
	b.ue(0)
	b.ue(0)
	b.put(0, 2)
	b.ue(groups)
	b.ue(l0)
	b.ue(0)
	b.put(0, 1)
	b.put(bipred, 2)
	b.se(qp)
	b.se(0)
	b.se(chroma)
	b.put(4, 3)
	if extension {
		b.put(1, 1)
		if scaling {
			b.put(1, 1)
		} else {
			b.put(0, 1)
		}
		b.se(0)
	}
	b.tail()
	return append([]byte{0x68}, b.raw...)
}
func TestH264ValidatesCompletePPSAndSameInlineParameterSets(t *testing.T) {
	for _, tc := range []struct {
		name  string
		pps   []byte
		valid bool
	}{
		{"ordinary", ppsFixture(0, 0, 0, 0, 0, false, false), true},
		{"signed-qp", ppsFixture(0, 0, 0, -26, -12, false, false), true},
		{"transform-extension", ppsFixture(0, 0, 0, 0, 0, true, false), true},
		{"too-many-references", ppsFixture(32, 0, 0, 0, 0, false, false), false},
		{"reserved-bipred", ppsFixture(0, 3, 0, 0, 0, false, false), false},
		{"unsupported-slice-groups", ppsFixture(0, 0, 1, 0, 0, false, false), false},
		{"invalid-qp", ppsFixture(0, 0, 0, -27, 0, false, false), false},
		{"invalid-chroma", ppsFixture(0, 0, 0, 0, 13, false, false), false},
		{"unsupported-scaling", ppsFixture(0, 0, 0, 0, 0, true, true), false},
		{"truncated-pps", []byte{0x68, 0xc0}, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			desc := h264Fixture(t)
			n := int(be.Uint16(desc.Headers[6:8]))
			sps := append([]byte(nil), desc.Headers[8:8+n]...)
			desc.Headers = append(desc.Headers[:8+n], 1, byte(len(tc.pps)>>8), byte(len(tc.pps)))
			desc.Headers = append(desc.Headers, tc.pps...)
			au := []byte{0, 0, 0, 3, 0x65, 0xb8, 0x40}
			if tc.valid {
				au = append(append([]byte{byte(len(sps) >> 24), byte(len(sps) >> 16), byte(len(sps) >> 8), byte(len(sps))}, sps...), au...)
			}
			key, err := InspectH264AU(desc, au)
			if (err == nil) != tc.valid || key != tc.valid {
				t.Fatalf("PPS=%x key=%v err=%v", tc.pps, key, err)
			}
		})
	}
}
