package fmp4

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
)

var metadataID = [16]byte{0xf5, 0x8b, 0x13, 0xca, 0x41, 0x4c, 0x4f, 0x16, 0x99, 0x87, 0x77, 0x03, 0x4b, 0x2b, 0x5c, 0x91}

type configMetadata struct {
	Version      uint32
	Video, Audio trackMetadata
	Format       core.MediaFormat
}
type trackMetadata struct {
	SourceEpoch, Revision uint64
	TimeBase              media.TimeBase
}

func (m configMetadata) info(track media.Track) media.CodecConfigInfo {
	identity := m.Video
	if track == media.TrackAudio {
		identity = m.Audio
	}
	info := media.CodecConfigInfo{Track: track, SourceEpoch: identity.SourceEpoch, Revision: identity.Revision, TimeBase: identity.TimeBase}
	if track == media.TrackVideo {
		info.Video = m.Format.Video
	} else {
		info.Audio = m.Format.Audio
	}
	return info
}

// Muxer пишет init сразу, затем отдельный moof/mdat для каждого пакета.
// При ошибке I/O транспорт становится непригодным для дальнейших записей.
type Muxer struct {
	w        io.Writer
	init     Init
	limits   Limits
	sequence uint32
	last     map[media.Track]int64
	err      error
}

func NewMuxer(w io.Writer, init Init, limits Limits) (*Muxer, error) {
	if w == nil {
		return nil, ErrInvalid
	}
	if err := limits.Validate(); err != nil {
		return nil, err
	}
	if err := init.Validate(); err != nil {
		return nil, err
	}
	data, err := marshalInit(init, limits)
	if err != nil {
		return nil, err
	}
	if err = writeAll(w, data); err != nil {
		return nil, err
	}
	return &Muxer{w: w, init: init, limits: limits, last: make(map[media.Track]int64)}, nil
}
func sameConfig(a, b media.CodecConfig) bool {
	return a.Info() == b.Info() && bytes.Equal(a.Headers(), b.Headers())
}
func sameHeaders(c media.CodecConfig, headers []byte) bool {
	return c.StoredBytes() == uint64(len(headers)) && bytes.Equal(c.Headers(), headers)
}
func (m *Muxer) WritePacket(p media.Packet) error {
	if m.err != nil {
		return m.err
	}
	if err := p.Validate(); err != nil {
		return err
	}
	c := p.Config()
	info := c.Info()
	if c.StoredBytes() > m.limits.MaxExtradataBytes {
		return ErrLimit
	}
	expected, id := m.init.Video, uint32(1)
	if info.Track == media.TrackAudio {
		expected, id = m.init.Audio, 2
	}
	if !sameConfig(c, expected) {
		return ErrConfigChanged
	}
	size := p.StoredBytes() - c.StoredBytes()
	if size > m.limits.MaxSampleBytes || size+8 > m.limits.MaxMdatBytes || size+8 > m.limits.MaxBoxBytes {
		return ErrLimit
	}
	if m.sequence == math.MaxUint32 {
		return fmt.Errorf("%w: sequence number", ErrLimit)
	}
	scale, err := trackScale(info.TimeBase)
	if err != nil {
		return err
	}
	base := media.TimeBase{Numerator: 1, Denominator: scale}
	pi := p.Info()
	dts, err := rescale(pi.DTS, info.TimeBase, base)
	if err != nil {
		return err
	}
	pts, err := rescale(pi.PTS, info.TimeBase, base)
	if err != nil {
		return err
	}
	duration, err := rescale(pi.DurationTicks, info.TimeBase, base)
	if err != nil {
		return err
	}
	shift, err := m.init.Origin.ticks(scale)
	if err != nil {
		return err
	}
	storedDTS, err := add(dts, shift)
	if err != nil || storedDTS < 0 {
		return ErrUnsupportedTiming
	}
	cts := newDifference(pts, dts)
	if !cts.IsInt64() || cts.Int64() < math.MinInt32 || cts.Int64() > math.MaxInt32 || duration < 0 || duration > math.MaxUint32 {
		return ErrUnsupportedTiming
	}
	if info.Track == media.TrackVideo && info.Video.Codec == core.CodecAV1 && pts != dts {
		return fmt.Errorf("%w: AV1SampleEntry требует PTS = DTS", ErrUnsupportedTiming)
	}
	if previous, ok := m.last[info.Track]; ok && dts < previous {
		return ErrUnsupportedTiming
	}
	var sampleFlags uint32 = 0x02000000
	if info.Track == media.TrackVideo && !pi.Keyframe {
		sampleFlags = 0x01010000
	}
	trunFlags := uint32(0x701)
	fields := [][]byte{u32(1), u32(0), u32(uint32(duration)), u32(uint32(size)), u32(sampleFlags)}
	if info.Track != media.TrackVideo || info.Video.Codec != core.CodecAV1 {
		trunFlags |= 0x800
		fields = append(fields, u32(uint32(int32(cts.Int64()))))
	}
	trun := full("trun", 1, trunFlags, fields...)
	moof := box("moof", full("mfhd", 0, 0, u32(m.sequence+1)), box("traf", full("tfhd", 0, 0x020000, u32(id)), full("tfdt", 1, 0, u64(uint64(storedDTS))), trun))
	if uint64(len(moof)) > m.limits.MaxMoofBytes {
		return ErrLimit
	}
	// Смещение trun всегда относится к началу единственного moof.
	offsetPos := len(moof) - len(trun) + 16
	be.PutUint32(moof[offsetPos:offsetPos+4], uint32(len(moof)+8))
	for _, b := range [][]byte{moof, join(u32(uint32(size+8)), []byte("mdat")), p.Payload()} {
		if err := writeAll(m.w, b); err != nil {
			m.err = err
			return err
		}
	}
	m.sequence++
	m.last[info.Track] = dts
	return nil
}

func marshalInit(init Init, l Limits) ([]byte, error) {
	v, a := init.Video.Info(), init.Audio.Info()
	for _, c := range []media.CodecConfig{init.Video, init.Audio} {
		if c.StoredBytes() > l.MaxExtradataBytes {
			return nil, ErrLimit
		}
	}
	// Ограничение строк предшествует JSON-копии полного описания.
	textBytes := len(v.Video.CodecProfile) + len(v.Video.Level) + len(v.Video.Tier) + len(v.Video.PixelFormat) + len(v.Video.Color.Range) + len(v.Video.Color.Primaries) + len(v.Video.Color.Transfer) + len(v.Video.Color.Matrix) + len(a.Audio.CodecProfile) + len(a.Audio.ChannelLayout)
	initLimit := min(l.MaxInitBytes, l.MaxBoxBytes)
	if uint64(textBytes)*6+4096+2*(init.Video.StoredBytes()+init.Audio.StoredBytes()) > initLimit {
		return nil, ErrLimit
	}
	meta, err := json.Marshal(configMetadata{Version: 1, Video: trackMetadata{v.SourceEpoch, v.Revision, v.TimeBase}, Audio: trackMetadata{a.SourceEpoch, a.Revision, a.TimeBase}, Format: core.MediaFormat{Video: v.Video, Audio: a.Audio}})
	if err != nil {
		return nil, err
	}
	scale, err := MovieTimeScale(v.TimeBase, a.TimeBase)
	if err != nil {
		return nil, err
	}
	mvhd := make([]byte, 96)
	be.PutUint32(mvhd[8:12], scale)
	be.PutUint32(mvhd[16:20], 0x10000)
	be.PutUint16(mvhd[20:22], 0x100)
	putMatrix(mvhd[32:68])
	be.PutUint32(mvhd[92:96], 3)
	video, err := marshalTrack(1, init.Video, init.Origin, l)
	if err != nil {
		return nil, err
	}
	audio, err := marshalTrack(2, init.Audio, init.Origin, l)
	if err != nil {
		return nil, err
	}
	data := join(box("ftyp", []byte("isom"), u32(0x200), []byte("isomiso6mp41")), box("moov", full("mvhd", 0, 0, mvhd), video, audio, box("mvex", full("trex", 0, 0, u32(1), u32(1), make([]byte, 12)), full("trex", 0, 0, u32(2), u32(1), make([]byte, 12))), box("uuid", metadataID[:], meta)))
	if uint64(len(data)) > l.MaxInitBytes {
		return nil, ErrLimit
	}
	top, err := children(data)
	if err != nil {
		return nil, err
	}
	for _, b := range top {
		if b.size > l.MaxBoxBytes {
			return nil, ErrLimit
		}
	}
	return data, nil
}
func putMatrix(b []byte) {
	be.PutUint32(b[0:4], 0x10000)
	be.PutUint32(b[16:20], 0x10000)
	be.PutUint32(b[32:36], 0x40000000)
}
func marshalTrack(id uint32, c media.CodecConfig, origin OriginMapping, l Limits) ([]byte, error) {
	info := c.Info()
	scale, err := trackScale(info.TimeBase)
	if err != nil {
		return nil, err
	}
	shift, err := origin.ticks(scale)
	if err != nil {
		return nil, err
	}
	tkhd := make([]byte, 80)
	be.PutUint32(tkhd[8:12], id)
	putMatrix(tkhd[36:72])
	if info.Track == media.TrackVideo {
		if info.Video.Width > 65535 || info.Video.Height > 65535 {
			return nil, ErrInvalid
		}
		be.PutUint32(tkhd[72:76], info.Video.Width<<16)
		be.PutUint32(tkhd[76:80], info.Video.Height<<16)
	} else {
		be.PutUint16(tkhd[32:34], 0x100)
	}
	mdhd := make([]byte, 20)
	be.PutUint32(mdhd[8:12], scale)
	be.PutUint16(mdhd[16:18], 0x55c4)
	handler := "vide"
	mediaHeader := full("vmhd", 0, 1, make([]byte, 8))
	if info.Track == media.TrackAudio {
		handler = "soun"
		mediaHeader = full("smhd", 0, 0, make([]byte, 4))
	}
	entry, err := marshalEntry(c, l)
	if err != nil {
		return nil, err
	}
	stbl := box("stbl", full("stsd", 0, 0, u32(1), entry), full("stts", 0, 0, u32(0)), full("stsc", 0, 0, u32(0)), full("stsz", 0, 0, u32(0), u32(0)), full("stco", 0, 0, u32(0)))
	dinf := box("dinf", full("dref", 0, 0, u32(1), full("url ", 0, 1)))
	mdia := box("mdia", full("mdhd", 0, 0, mdhd), full("hdlr", 0, 0, u32(0), []byte(handler), make([]byte, 12), []byte{0}), box("minf", mediaHeader, dinf, stbl))
	edits := []byte(nil)
	if shift != 0 {
		edits = box("edts", full("elst", 1, 0, u32(1), u64(0), u64(uint64(shift)), u32(0x10000)))
	}
	return box("trak", full("tkhd", 0, 7, tkhd), edits, mdia), nil
}
func descriptor(tag byte, data []byte) []byte {
	n := len(data)
	var length [4]byte
	pos := 3
	length[pos] = byte(n & 0x7f)
	for n >>= 7; n > 0; n >>= 7 {
		pos--
		length[pos] = byte(n&0x7f) | 0x80
	}
	return join([]byte{tag}, length[pos:], data)
}
func marshalEntry(c media.CodecConfig, l Limits) ([]byte, error) {
	info := c.Info()
	headers := c.Headers()
	if uint64(len(headers)) > l.MaxExtradataBytes {
		return nil, ErrLimit
	}
	if info.Track == media.TrackVideo {
		if info.Video.Codec == core.CodecH264 {
			if _, err := spsFromAVCC(TrackDescription{Width: info.Video.Width, Height: info.Video.Height, Headers: headers}); err != nil {
				return nil, err
			}
		}
		entry := make([]byte, 78)
		be.PutUint16(entry[6:8], 1)
		be.PutUint16(entry[24:26], uint16(info.Video.Width))
		be.PutUint16(entry[26:28], uint16(info.Video.Height))
		be.PutUint32(entry[28:32], 0x480000)
		be.PutUint32(entry[32:36], 0x480000)
		be.PutUint16(entry[40:42], 1)
		be.PutUint16(entry[74:76], 0x18)
		be.PutUint16(entry[76:78], 0xffff)
		kind, config := "avc1", "avcC"
		if info.Video.Codec == core.CodecAV1 {
			kind, config = "av01", "av1C"
		}
		if len(headers) < 4 {
			return nil, ErrInvalid
		}
		return box(kind, entry, box(config, headers)), nil
	}
	if info.Audio.SampleRateHz > 65535 {
		return nil, ErrInvalid
	}
	actual, err := audioFromASC(headers)
	if err != nil {
		return nil, err
	}
	if actual.SampleRateHz != info.Audio.SampleRateHz || actual.Channels != info.Audio.Channels {
		return nil, fmt.Errorf("%w: описание audio противоречит ASC", ErrInvalid)
	}
	if len(headers) > 0x0fffffff-32 {
		return nil, ErrLimit
	}
	entry := make([]byte, 28)
	be.PutUint16(entry[6:8], 1)
	be.PutUint16(entry[16:18], info.Audio.Channels)
	be.PutUint16(entry[18:20], 16)
	be.PutUint32(entry[24:28], info.Audio.SampleRateHz<<16)
	es := descriptor(3, join([]byte{0, 2, 0}, descriptor(4, join([]byte{0x40, 0x15, 0, 0, 0}, make([]byte, 8), descriptor(5, headers))), descriptor(6, []byte{2})))
	return box("mp4a", entry, full("esds", 0, 0, es)), nil
}
