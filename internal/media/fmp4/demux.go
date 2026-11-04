package fmp4

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"math"
	"math/big"
	"sort"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
)

type trackState struct {
	description           TrackDescription
	config                media.CodecConfig
	offset                int64
	duration, size, flags uint32
	lastDTS               int64
	hasDTS                bool
}
type sample struct {
	track  *trackState
	offset uint64
	size   uint32
	info   media.PacketInfo
}

// Demuxer читает только текущий ограниченный init/moof и одну единицу доступа.
type Demuxer struct {
	r        io.Reader
	options  DemuxOptions
	init     Init
	tracks   map[uint32]*trackState
	position uint64
	samples  []sample
	mdatEnd  uint64
	err      error
}

func NewDemuxer(r io.Reader, options DemuxOptions) (*Demuxer, error) {
	if r == nil {
		return nil, ErrInvalid
	}
	if err := options.Limits.Validate(); err != nil {
		return nil, err
	}
	d := &Demuxer{r: r, options: options, tracks: make(map[uint32]*trackState)}
	var total uint64
	seenFtyp := false
	for {
		b, err := d.header()
		if err != nil {
			return nil, err
		}
		if b.size > options.Limits.MaxInitBytes-total {
			return nil, ErrLimit
		}
		total += b.size
		if b.kind != "ftyp" && b.kind != "moov" && b.kind != "free" && b.kind != "skip" {
			return nil, fmt.Errorf("%w: неожиданный init box %s", ErrInvalid, b.kind)
		}
		b, err = d.body(b, options.Limits.MaxBoxBytes)
		if err != nil {
			return nil, err
		}
		if b.kind == "ftyp" {
			if seenFtyp {
				return nil, ErrInvalid
			}
			seenFtyp = true
		}
		if b.kind == "moov" {
			if !seenFtyp {
				return nil, ErrInvalid
			}
			if err = d.parseInit(b.data); err != nil {
				return nil, err
			}
			return d, nil
		}
	}
}
func (d *Demuxer) Init() Init { return d.init }

// Descriptions возвращает отдельные копии сведений, извлечённых из init.
func (d *Demuxer) Descriptions() []TrackDescription {
	result := make([]TrackDescription, 0, len(d.tracks))
	for _, t := range d.tracks {
		desc := t.description
		desc.Headers = bytes.Clone(desc.Headers)
		result = append(result, desc)
	}
	sort.Slice(result, func(i, j int) bool { return result[i].ID < result[j].ID })
	return result
}
func (d *Demuxer) header() (rawBox, error) {
	b, err := readHeader(d.r)
	if err != nil {
		return b, err
	}
	if d.position > math.MaxUint64-b.header {
		return b, ErrLimit
	}
	d.position += b.header
	return b, nil
}
func (d *Demuxer) body(b rawBox, limit uint64) (rawBox, error) {
	if d.position > math.MaxUint64-(b.size-b.header) {
		return rawBox{}, ErrLimit
	}
	b, err := readBody(d.r, b, limit)
	if err == nil {
		d.position += b.size - b.header
	}
	return b, err
}
func (d *Demuxer) skip(n uint64) error {
	if n > math.MaxInt64 {
		return ErrLimit
	}
	if d.position > math.MaxUint64-n {
		return ErrLimit
	}
	_, err := io.CopyN(io.Discard, d.r, int64(n))
	if err == nil {
		d.position += n
	}
	return err
}
func (d *Demuxer) Next() (media.Packet, error) {
	if d.err != nil {
		return media.Packet{}, d.err
	}
	p, err := d.next()
	if err != nil {
		d.err = err
	}
	return p, err
}
func (d *Demuxer) next() (media.Packet, error) {
	for len(d.samples) == 0 {
		if d.mdatEnd > d.position {
			if err := d.skip(d.mdatEnd - d.position); err != nil {
				return media.Packet{}, err
			}
		}
		d.mdatEnd = 0
		start := d.position
		b, err := d.header()
		if err != nil {
			return media.Packet{}, err
		}
		if b.kind == "mfra" || b.kind == "sidx" || b.kind == "styp" || b.kind == "free" {
			if b.size > d.options.Limits.MaxBoxBytes {
				return media.Packet{}, ErrLimit
			}
			if err = d.skip(b.size - b.header); err != nil {
				return media.Packet{}, err
			}
			continue
		}
		if b.kind != "moof" {
			return media.Packet{}, fmt.Errorf("%w: ожидался moof, получен %s", ErrInvalid, b.kind)
		}
		b, err = d.body(b, d.options.Limits.MaxMoofBytes)
		if err != nil {
			return media.Packet{}, err
		}
		if err = d.parseMoof(b.data, start); err != nil {
			return media.Packet{}, err
		}
		mdat, err := d.header()
		if err != nil {
			return media.Packet{}, err
		}
		if mdat.kind != "mdat" {
			return media.Packet{}, ErrInvalid
		}
		if mdat.size > d.options.Limits.MaxMdatBytes || mdat.size > d.options.Limits.MaxBoxBytes {
			return media.Packet{}, ErrLimit
		}
		if d.position > math.MaxUint64-(mdat.size-mdat.header) {
			return media.Packet{}, ErrLimit
		}
		d.mdatEnd = d.position + mdat.size - mdat.header
		sort.Slice(d.samples, func(i, j int) bool { return d.samples[i].offset < d.samples[j].offset })
		end := d.position
		for _, s := range d.samples {
			if s.offset < end || s.offset > d.mdatEnd || uint64(s.size) > d.mdatEnd-s.offset {
				return media.Packet{}, fmt.Errorf("%w: sample за пределами mdat", ErrInvalid)
			}
			end = s.offset + uint64(s.size)
		}
	}
	s := d.samples[0]
	d.samples = d.samples[1:]
	if s.offset > d.position {
		if err := d.skip(s.offset - d.position); err != nil {
			return media.Packet{}, err
		}
	}
	payload := make([]byte, int(s.size))
	if _, err := io.ReadFull(d.r, payload); err != nil {
		return media.Packet{}, err
	}
	d.position += uint64(s.size)
	return media.NewPacket(s.info, s.track.config, payload)
}
func (d *Demuxer) parseInit(data []byte) error {
	boxes, err := children(data)
	if err != nil {
		return err
	}
	mvhd, err := find(data, "mvhd")
	if err != nil {
		return err
	}
	var movieScale uint32
	if len(mvhd.data) < 20 {
		return ErrInvalid
	}
	switch mvhd.data[0] {
	case 0:
		movieScale = be.Uint32(mvhd.data[12:16])
	case 1:
		if len(mvhd.data) < 32 {
			return ErrInvalid
		}
		movieScale = be.Uint32(mvhd.data[20:24])
	default:
		return ErrInvalid
	}
	if movieScale == 0 {
		return ErrInvalid
	}
	var metadata *configMetadata
	for _, b := range boxes {
		switch b.kind {
		case "trak":
			if len(d.tracks) >= 2 {
				return ErrLimit
			}
			t, err := d.parseTrack(b.data, movieScale)
			if err != nil {
				return err
			}
			if _, ok := d.tracks[t.description.ID]; ok {
				return ErrInvalid
			}
			d.tracks[t.description.ID] = t
		case "uuid":
			if len(b.data) >= 16 && bytes.Equal(b.data[:16], metadataID[:]) {
				if metadata != nil {
					return ErrInvalid
				}
				metadata = &configMetadata{}
				decoder := json.NewDecoder(bytes.NewReader(b.data[16:]))
				decoder.DisallowUnknownFields()
				if err := decoder.Decode(metadata); err != nil {
					return fmt.Errorf("%w: metadata: %v", ErrInvalid, err)
				}
				if metadata.Version != 1 {
					return ErrInvalid
				}
				if err := decoder.Decode(new(any)); err != io.EOF {
					return ErrInvalid
				}
			}
		}
	}
	if len(d.tracks) != 2 {
		return fmt.Errorf("%w: нужны обе дорожки", ErrInvalid)
	}
	if mvex, err := find(data, "mvex"); err == nil {
		defaults, err := children(mvex.data)
		if err != nil {
			return err
		}
		for _, b := range defaults {
			if b.kind == "trex" {
				if len(b.data) != 24 {
					return ErrInvalid
				}
				t := d.tracks[be.Uint32(b.data[4:8])]
				if t == nil {
					return ErrInvalid
				}
				if be.Uint32(b.data[8:12]) != 1 {
					return ErrInvalid
				}
				t.duration = be.Uint32(b.data[12:16])
				t.size = be.Uint32(b.data[16:20])
				t.flags = be.Uint32(b.data[20:24])
			}
		}
	}
	for _, t := range d.tracks {
		var c media.CodecConfig
		if metadata != nil {
			info := metadata.info(t.description.Track)
			applyIdentity(&info, d.options)
			c, err = media.NewCodecConfig(info, t.description.Headers)
		} else if d.options.ResolveConfig != nil {
			desc := t.description
			desc.Headers = bytes.Clone(desc.Headers)
			c, err = d.options.ResolveConfig(desc)
			if err == nil {
				if err = validateWireConfig(c, t.description); err != nil {
					return err
				}
				info := c.Info()
				applyIdentity(&info, d.options)
				c, err = media.NewCodecConfig(info, t.description.Headers)
			}
		} else {
			revision := d.options.VideoRevision
			if t.description.Track == media.TrackAudio {
				revision = d.options.AudioRevision
			}
			c, err = ResolveCodecConfig(t.description, d.options.SourceEpoch, revision)
		}
		if err != nil {
			return err
		}
		if err = validateWireConfig(c, t.description); err != nil {
			return err
		}
		t.config = c
		if t.description.Track == media.TrackVideo {
			if d.init.Video.Validate() == nil {
				return ErrInvalid
			}
			d.init.Video = c
		} else {
			if d.init.Audio.Validate() == nil {
				return ErrInvalid
			}
			d.init.Audio = c
		}
	}
	if d.init.Video.Info().SourceEpoch != d.init.Audio.Info().SourceEpoch {
		return ErrInvalid
	}
	return nil
}
func applyIdentity(info *media.CodecConfigInfo, o DemuxOptions) {
	if o.SourceEpoch != 0 {
		info.SourceEpoch = o.SourceEpoch
	}
	if info.Track == media.TrackVideo && o.VideoRevision != 0 {
		info.Revision = o.VideoRevision
	}
	if info.Track == media.TrackAudio && o.AudioRevision != 0 {
		info.Revision = o.AudioRevision
	}
}
func (d *Demuxer) parseTrack(data []byte, movieScale uint32) (*trackState, error) {
	tkhd, err := find(data, "tkhd")
	if err != nil {
		return nil, err
	}
	if len(tkhd.data) < 24 {
		return nil, ErrInvalid
	}
	idPos := 12
	if tkhd.data[0] == 1 {
		idPos = 20
	} else if tkhd.data[0] != 0 {
		return nil, ErrInvalid
	}
	if len(tkhd.data) < idPos+4 {
		return nil, ErrInvalid
	}
	id := be.Uint32(tkhd.data[idPos : idPos+4])
	if id == 0 {
		return nil, ErrInvalid
	}
	mdia, err := find(data, "mdia")
	if err != nil {
		return nil, err
	}
	mdhd, err := find(mdia.data, "mdhd")
	if err != nil {
		return nil, err
	}
	if len(mdhd.data) < 20 {
		return nil, ErrInvalid
	}
	scalePos := 12
	if mdhd.data[0] == 1 {
		scalePos = 20
	} else if mdhd.data[0] != 0 {
		return nil, ErrInvalid
	}
	if len(mdhd.data) < scalePos+4 {
		return nil, ErrInvalid
	}
	scale := be.Uint32(mdhd.data[scalePos : scalePos+4])
	if scale == 0 {
		return nil, ErrInvalid
	}
	minf, err := find(mdia.data, "minf")
	if err != nil {
		return nil, err
	}
	stbl, err := find(minf.data, "stbl")
	if err != nil {
		return nil, err
	}
	stsd, err := find(stbl.data, "stsd")
	if err != nil {
		return nil, err
	}
	if len(stsd.data) < 8 || be.Uint32(stsd.data[4:8]) != 1 {
		return nil, ErrInvalid
	}
	entries, err := children(stsd.data[8:])
	if err != nil || len(entries) != 1 {
		return nil, ErrInvalid
	}
	entry := entries[0]
	t := &trackState{description: TrackDescription{ID: id, TimeBase: media.TimeBase{Numerator: 1, Denominator: scale}}}
	desc := &t.description
	var extra rawBox
	switch entry.kind {
	case "avc1", "av01":
		if len(entry.data) < 78 {
			return nil, ErrInvalid
		}
		desc.Track = media.TrackVideo
		desc.Codec = core.CodecH264
		kind := "avcC"
		if entry.kind == "av01" {
			desc.Codec = core.CodecAV1
			kind = "av1C"
		}
		desc.Width = uint32(be.Uint16(entry.data[24:26]))
		desc.Height = uint32(be.Uint16(entry.data[26:28]))
		extra, err = find(entry.data[78:], kind)
		if err != nil {
			return nil, err
		}
		if uint64(len(extra.data)) > d.options.Limits.MaxExtradataBytes {
			return nil, ErrLimit
		}
		desc.Headers = bytes.Clone(extra.data)
	case "mp4a":
		if len(entry.data) < 28 || be.Uint16(entry.data[8:10]) != 0 {
			return nil, ErrInvalid
		}
		desc.Track = media.TrackAudio
		desc.Codec = core.CodecAAC
		desc.Channels = be.Uint16(entry.data[16:18])
		desc.SampleRateHz = be.Uint32(entry.data[24:28]) >> 16
		extra, err = find(entry.data[28:], "esds")
		if err != nil {
			return nil, err
		}
		if len(extra.data) < 4 {
			return nil, ErrInvalid
		}
		desc.Headers, err = parseASC(extra.data[4:], d.options.Limits.MaxExtradataBytes)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("%w: codec %s", ErrInvalid, entry.kind)
	}
	if len(desc.Headers) == 0 {
		return nil, ErrInvalid
	}
	hdlr, err := find(mdia.data, "hdlr")
	if err != nil {
		return nil, err
	}
	if len(hdlr.data) < 12 {
		return nil, ErrInvalid
	}
	expectedHandler := "vide"
	if desc.Track == media.TrackAudio {
		expectedHandler = "soun"
	}
	if string(hdlr.data[8:12]) != expectedHandler {
		return nil, fmt.Errorf("%w: hdlr противоречит sample entry", ErrInvalid)
	}
	edts, err := find(data, "edts")
	if err == nil {
		elst, err := find(edts.data, "elst")
		if err != nil {
			return nil, err
		}
		t.offset, err = parseEdit(elst.data, movieScale, scale)
		if err != nil {
			return nil, err
		}
	}
	return t, nil
}
func parseASC(data []byte, limit uint64) ([]byte, error) {
	var walk func([]byte, int) ([]byte, error)
	walk = func(b []byte, depth int) ([]byte, error) {
		if depth > 3 {
			return nil, ErrInvalid
		}
		for len(b) > 0 {
			tag := b[0]
			b = b[1:]
			n := uint32(0)
			ended := false
			for i := 0; i < 4; i++ {
				if len(b) == 0 {
					return nil, ErrInvalid
				}
				v := b[0]
				b = b[1:]
				n = n<<7 | uint32(v&0x7f)
				if v&0x80 == 0 {
					ended = true
					break
				}
			}
			if !ended || uint64(n) > uint64(len(b)) {
				return nil, ErrInvalid
			}
			body := b[:n]
			b = b[n:]
			if tag == 5 {
				if uint64(n) > limit {
					return nil, ErrLimit
				}
				return bytes.Clone(body), nil
			}
			skip := 0
			switch tag {
			case 3:
				if len(body) < 3 || body[2] != 0 {
					return nil, ErrInvalid
				}
				skip = 3
			case 4:
				if len(body) < 13 || body[0] != 0x40 {
					return nil, ErrInvalid
				}
				skip = 13
			default:
				continue
			}
			result, err := walk(body[skip:], depth+1)
			if err == nil {
				return result, nil
			}
			if err != ErrMetadataUnavailable {
				return nil, err
			}
		}
		return nil, ErrMetadataUnavailable
	}
	return walk(data, 0)
}
func parseEdit(data []byte, movieScale, scale uint32) (int64, error) {
	if len(data) < 8 {
		return 0, ErrInvalid
	}
	version := data[0]
	if version > 1 {
		return 0, ErrInvalid
	}
	count := be.Uint32(data[4:8])
	if count == 0 || count > 2 {
		return 0, ErrUnsupportedTiming
	}
	c := cursor{b: data[8:]}
	offset := int64(0)
	seenMedia := false
	for i := uint32(0); i < count; i++ {
		var duration uint64
		var mediaTime int64
		if version == 1 {
			duration = c.u64()
			mediaTime = int64(c.u64())
		} else {
			duration = uint64(c.u32())
			mediaTime = int64(int32(c.u32()))
		}
		rate := c.u32()
		if c.err != nil || rate != 0x10000 {
			return 0, ErrUnsupportedTiming
		}
		if mediaTime == -1 {
			if seenMedia || duration > math.MaxInt64 {
				return 0, ErrUnsupportedTiming
			}
			gap, err := rescale(int64(duration), media.TimeBase{Numerator: 1, Denominator: movieScale}, media.TimeBase{Numerator: 1, Denominator: scale})
			if err != nil {
				return 0, err
			}
			offset, err = add(offset, gap)
			if err != nil {
				return 0, err
			}
		} else {
			if seenMedia || mediaTime < 0 {
				return 0, ErrUnsupportedTiming
			}
			seenMedia = true
			var err error
			offset, err = add(offset, -mediaTime)
			if err != nil {
				return 0, err
			}
		}
	}
	if !seenMedia || len(c.b) != 0 {
		return 0, ErrInvalid
	}
	return offset, nil
}

func (d *Demuxer) parseMoof(data []byte, start uint64) error {
	boxes, err := children(data)
	if err != nil {
		return err
	}
	d.samples = nil
	for _, b := range boxes {
		if b.kind != "traf" {
			continue
		}
		if err = d.parseTraf(b.data, start); err != nil {
			return err
		}
	}
	if len(d.samples) == 0 {
		return ErrInvalid
	}
	return nil
}
func (d *Demuxer) parseTraf(data []byte, start uint64) error {
	tfhd, err := find(data, "tfhd")
	if err != nil {
		return err
	}
	f, err := flags(tfhd.data)
	if err != nil {
		return err
	}
	if f&^uint32(0x03003b) != 0 {
		return ErrInvalid
	}
	c := cursor{b: tfhd.data[4:]}
	id := c.u32()
	t := d.tracks[id]
	if t == nil {
		return ErrInvalid
	}
	base := start
	if f&1 != 0 {
		base = c.u64()
	} else if f&0x020000 == 0 {
		return fmt.Errorf("%w: неявный base offset", ErrInvalid)
	}
	if f&2 != 0 && c.u32() != 1 {
		return ErrInvalid
	}
	duration, size, sampleFlags := t.duration, t.size, t.flags
	if f&8 != 0 {
		duration = c.u32()
	}
	if f&16 != 0 {
		size = c.u32()
	}
	if f&32 != 0 {
		sampleFlags = c.u32()
	}
	if c.err != nil || len(c.b) != 0 {
		return ErrInvalid
	}
	tfdt, err := find(data, "tfdt")
	if err != nil {
		return err
	}
	if len(tfdt.data) < 8 {
		return ErrInvalid
	}
	var decode uint64
	switch tfdt.data[0] {
	case 0:
		if len(tfdt.data) != 8 {
			return ErrInvalid
		}
		decode = uint64(be.Uint32(tfdt.data[4:]))
	case 1:
		if len(tfdt.data) != 12 {
			return ErrInvalid
		}
		decode = be.Uint64(tfdt.data[4:])
	default:
		return ErrInvalid
	}
	boxes, err := children(data)
	if err != nil {
		return err
	}
	var nextOffset uint64
	haveOffset := false
	for _, b := range boxes {
		if b.kind != "trun" {
			continue
		}
		f, err = flags(b.data)
		if err != nil {
			return err
		}
		if f&^uint32(0xf05) != 0 || b.data[0] > 1 || f&4 != 0 && f&0x400 != 0 {
			return ErrInvalid
		}
		if t.description.Codec == core.CodecAV1 && f&0x800 != 0 {
			return fmt.Errorf("%w: composition offset в AV1SampleEntry", ErrUnsupportedTiming)
		}
		c = cursor{b: b.data[4:]}
		count := c.u32()
		if count == 0 || uint64(count)+uint64(len(d.samples)) > uint64(d.options.Limits.MaxSamples) {
			return ErrLimit
		}
		if f&1 != 0 {
			delta := int64(int32(c.u32()))
			if delta < 0 {
				magnitude := uint64(-delta)
				if magnitude > base {
					return ErrInvalid
				}
				nextOffset = base - magnitude
			} else {
				if base > math.MaxUint64-uint64(delta) {
					return ErrInvalid
				}
				nextOffset = base + uint64(delta)
			}
			haveOffset = true
		}
		if !haveOffset {
			return ErrInvalid
		}
		firstFlags := sampleFlags
		if f&4 != 0 {
			firstFlags = c.u32()
		}
		entryWords := 0
		for _, bit := range []uint32{0x100, 0x200, 0x400, 0x800} {
			if f&bit != 0 {
				entryWords++
			}
		}
		if c.err != nil || uint64(count)*uint64(entryWords)*4 != uint64(len(c.b)) {
			return ErrInvalid
		}
		for index := uint32(0); index < count; index++ {
			dur, n, sf := duration, size, sampleFlags
			if index == 0 {
				sf = firstFlags
			}
			if f&0x100 != 0 {
				dur = c.u32()
			}
			if f&0x200 != 0 {
				n = c.u32()
			}
			if f&0x400 != 0 {
				sf = c.u32()
			}
			cts := int64(0)
			if f&0x800 != 0 {
				value := c.u32()
				cts = int64(value)
				if b.data[0] == 1 {
					cts = int64(int32(value))
				}
			}
			if n == 0 || uint64(n) > d.options.Limits.MaxSampleBytes {
				return ErrLimit
			}
			if nextOffset > math.MaxUint64-uint64(n) || decode > math.MaxUint64-uint64(dur) {
				return ErrUnsupportedTiming
			}
			dts := new(big.Int).SetUint64(decode)
			dts.Add(dts, big.NewInt(t.offset))
			pts := new(big.Int).Add(new(big.Int).Set(dts), big.NewInt(cts))
			if !dts.IsInt64() || !pts.IsInt64() {
				return ErrUnsupportedTiming
			}
			timeBase := t.description.TimeBase
			target := t.config.Info().TimeBase
			dt, err := rescale(dts.Int64(), timeBase, target)
			if err != nil {
				return err
			}
			pt, err := rescale(pts.Int64(), timeBase, target)
			if err != nil {
				return err
			}
			du, err := rescale(int64(dur), timeBase, target)
			if err != nil {
				return err
			}
			if t.hasDTS && dt < t.lastDTS {
				return fmt.Errorf("%w: DTS дорожки сброшен", ErrUnsupportedTiming)
			}
			t.lastDTS, t.hasDTS = dt, true
			d.samples = append(d.samples, sample{track: t, offset: nextOffset, size: n, info: media.PacketInfo{PTS: pt, DTS: dt, DurationTicks: du, Keyframe: t.description.Track == media.TrackVideo && sf&0x10000 == 0}})
			nextOffset += uint64(n)
			decode += uint64(dur)
		}
		if c.err != nil {
			return c.err
		}
	}
	return nil
}
