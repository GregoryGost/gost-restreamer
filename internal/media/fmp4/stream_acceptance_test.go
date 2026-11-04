package fmp4_test

import (
	"bytes"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"io"
	"math"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
)

func acceptanceLimits() fmp4.Limits {
	return fmp4.Limits{MaxBoxBytes: 1 << 20, MaxInitBytes: 1 << 20, MaxMoofBytes: 4096, MaxMdatBytes: 65536, MaxSampleBytes: 32768, MaxExtradataBytes: 4096, MaxSamples: 32}
}

func acceptanceInit(t *testing.T) fmp4.Init {
	t.Helper()
	// Синтетический источник получен через libx264; ffprobe подтвердил 128×128, 30 FPS, High и yuv420p.
	headers, err := hex.DecodeString("0164000bffe100186764000bacd942046c0440000003004000000f03c50a658001000668ebe3cb22c0fdf8f800")
	if err != nil {
		t.Fatal(err)
	}
	v, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: 9, Revision: 3, TimeBase: media.TimeBase{Numerator: 1, Denominator: 30000}, Video: core.VideoFormat{Codec: core.CodecH264, Width: 128, Height: 128, FrameRate: core.FrameRate{Numerator: 30, Denominator: 1}, CodecProfile: "high", PixelFormat: "yuv420p", BitDepth: 8, Color: core.ColorInfo{Range: "limited", Primaries: "unspecified", Transfer: "unspecified", Matrix: "unspecified"}}}, headers)
	if err != nil {
		t.Fatal(err)
	}
	a, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackAudio, SourceEpoch: 9, Revision: 8, TimeBase: media.TimeBase{Numerator: 1, Denominator: 48000}, Audio: core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", SampleRateHz: 48000, Channels: 2, ChannelLayout: "stereo"}}, []byte{0x11, 0x90})
	if err != nil {
		t.Fatal(err)
	}
	return fmp4.Init{Video: v, Audio: a, Origin: fmp4.OriginMapping{OffsetTicks: 4, TimeBase: media.TimeBase{Numerator: 1, Denominator: 1}}}
}

func acceptancePacket(t *testing.T, c media.CodecConfig, info media.PacketInfo, payload []byte) media.Packet {
	t.Helper()
	p, err := media.NewPacket(info, c, payload)
	if err != nil {
		t.Fatal(err)
	}
	return p
}

type acceptanceFragments struct {
	r io.Reader
	w io.Writer
}

func (f acceptanceFragments) Read(p []byte) (int, error) {
	if len(p) > 1 {
		p = p[:1]
	}
	return f.r.Read(p)
}

func (f acceptanceFragments) Write(p []byte) (int, error) {
	if len(p) > 3 {
		p = p[:3]
	}
	return f.w.Write(p)
}

func TestAcceptanceFractionalTimeBaseAndReorderedPresentation(t *testing.T) {
	init := acceptanceInit(t)
	info := init.Video.Info()
	info.TimeBase.Numerator = 1001
	var err error
	init.Video, err = media.NewCodecConfig(info, init.Video.Headers())
	if err != nil {
		t.Fatal(err)
	}
	var wire bytes.Buffer
	m, err := fmp4.NewMuxer(acceptanceFragments{w: &wire}, init, acceptanceLimits())
	if err != nil {
		t.Fatal(err)
	}
	// Метки заданы независимо: порядок декодирования возрастает, порядок показа меняется.
	packets := []media.Packet{
		acceptancePacket(t, init.Video, media.PacketInfo{DTS: -30, PTS: -28, DurationTicks: 1, Keyframe: true}, []byte{1, 2}),
		acceptancePacket(t, init.Audio, media.PacketInfo{DTS: -24000, PTS: -24000, DurationTicks: 1024}, []byte{3}),
		acceptancePacket(t, init.Video, media.PacketInfo{DTS: -29, PTS: -30, DurationTicks: 1}, []byte{4, 5}),
		acceptancePacket(t, init.Video, media.PacketInfo{DTS: -28, PTS: -29, DurationTicks: 1}, []byte{6}),
		acceptancePacket(t, init.Audio, media.PacketInfo{DTS: -22976, PTS: -22976, DurationTicks: 1024}, []byte{7}),
	}
	for _, p := range packets {
		if err := m.WritePacket(p); err != nil {
			t.Fatal(err)
		}
	}
	d, err := fmp4.NewDemuxer(acceptanceFragments{r: bytes.NewReader(wire.Bytes())}, fmp4.DemuxOptions{Limits: acceptanceLimits(), SourceEpoch: 31, VideoRevision: 17, AudioRevision: 18})
	if err != nil {
		t.Fatal(err)
	}
	for i, want := range packets {
		got, err := d.Next()
		if err != nil {
			t.Fatalf("пакет %d: %v", i, err)
		}
		info := got.Config().Info()
		revision := uint64(17)
		if info.Track == media.TrackAudio {
			revision = 18
		}
		if got.Info() != want.Info() || !bytes.Equal(got.Payload(), want.Payload()) || !bytes.Equal(got.Config().Headers(), want.Config().Headers()) || info.SourceEpoch != 31 || info.Revision != revision || info.TimeBase != want.Config().Info().TimeBase {
			t.Fatalf("изменились время, данные или идентичность пакета %d: %+v, %+v", i, got.Info(), info)
		}
	}
	if _, err := d.Next(); !errors.Is(err, io.EOF) {
		t.Fatalf("после всех пакетов ожидался EOF: %v", err)
	}
}

// acceptanceBox читает структуру синтетических данных независимо от рабочего декодера.
func acceptanceBox(t *testing.T, data []byte, path ...string) []byte {
	t.Helper()
	for len(data) >= 8 {
		n := int(binary.BigEndian.Uint32(data[:4]))
		if n < 8 || n > len(data) {
			t.Fatal("некорректная структура fixture")
		}
		if string(data[4:8]) == path[0] {
			if len(path) == 1 {
				return data[8:n]
			}
			return acceptanceBox(t, data[8:n], path[1:]...)
		}
		data = data[n:]
	}
	t.Fatalf("в fixture нет box %v", path)
	return nil
}

func TestAcceptanceInitRejectsContradictoryAudioDescription(t *testing.T) {
	for _, field := range []string{"channels", "sample-rate"} {
		t.Run(field, func(t *testing.T) {
			var wire bytes.Buffer
			if _, err := fmp4.NewMuxer(&wire, acceptanceInit(t), acceptanceLimits()); err != nil {
				t.Fatal(err)
			}
			data := bytes.Clone(wire.Bytes())
			// mp4a имеет уникальную сигнатуру в синтетических данных; длина блока проверяется отдельно.
			pos := bytes.Index(data, []byte("mp4a"))
			if pos < 4 || int(binary.BigEndian.Uint32(data[pos-4:pos])) < 36 {
				t.Fatal("в fixture нет корректного mp4a")
			}
			entry := data[pos+4:]
			if field == "channels" {
				binary.BigEndian.PutUint16(entry[16:18], 1)
			} else {
				binary.BigEndian.PutUint32(entry[24:28], 44100<<16)
			}
			if _, err := fmp4.NewDemuxer(bytes.NewReader(data), fmp4.DemuxOptions{Limits: acceptanceLimits()}); err == nil {
				t.Fatal("init принят: sample entry противоречит AAC ASC и полному описанию")
			}
		})
	}
}

func TestAcceptanceAudioResolverRejectsASCContradiction(t *testing.T) {
	for _, desc := range []fmp4.TrackDescription{
		{Track: media.TrackAudio, Codec: core.CodecAAC, TimeBase: media.TimeBase{Numerator: 1, Denominator: 48000}, SampleRateHz: 44100, Channels: 2, Headers: []byte{0x11, 0x90}},
		{Track: media.TrackAudio, Codec: core.CodecAAC, TimeBase: media.TimeBase{Numerator: 1, Denominator: 48000}, SampleRateHz: 48000, Channels: 1, Headers: []byte{0x11, 0x90}},
	} {
		if _, err := fmp4.ResolveCodecConfig(desc, 1, 1); err == nil {
			t.Errorf("sample entry противоречит ASC: sample_rate=%d channels=%d", desc.SampleRateHz, desc.Channels)
		}
	}
}

func TestAcceptanceMuxWireRetainsSignedCTSAndAV1OmitsCTS(t *testing.T) {
	for _, codec := range []core.Codec{core.CodecH264, core.CodecAV1} {
		t.Run(string(codec), func(t *testing.T) {
			init := acceptanceInit(t)
			info := init.Video.Info()
			info.Video.Codec = codec
			info.TimeBase.Numerator = 1001
			headers := init.Video.Headers()
			if codec == core.CodecAV1 {
				info.Video.CodecProfile = "Main"
				headers = []byte{0x81, 0, 0, 0}
			}
			var err error
			init.Video, err = media.NewCodecConfig(info, headers)
			if err != nil {
				t.Fatal(err)
			}
			var wire bytes.Buffer
			m, err := fmp4.NewMuxer(&wire, init, acceptanceLimits())
			if err != nil {
				t.Fatal(err)
			}
			pts := int64(-2)
			if codec == core.CodecAV1 {
				pts = 0
			}
			if err := m.WritePacket(acceptancePacket(t, init.Video, media.PacketInfo{DTS: 0, PTS: pts, DurationTicks: 1, Keyframe: true}, []byte{9})); err != nil {
				t.Fatal(err)
			}
			trun := acceptanceBox(t, wire.Bytes(), "moof", "traf", "trun")
			flags := binary.BigEndian.Uint32(trun[:4]) & 0xffffff
			if codec == core.CodecAV1 {
				if flags&0x800 != 0 {
					t.Fatal("AV1 содержит запрещённое поле composition offset")
				}
			} else if trun[0] != 1 || flags&0x800 == 0 || int32(binary.BigEndian.Uint32(trun[len(trun)-4:])) != -2002 {
				t.Fatal("H.264 потерял отрицательное смещение -2002 в шкале 1/30000")
			}
		})
	}
}

func TestAcceptanceRejectedTimingDoesNotPoisonFollowingPacket(t *testing.T) {
	init := acceptanceInit(t)
	var wire bytes.Buffer
	m, err := fmp4.NewMuxer(&wire, init, acceptanceLimits())
	if err != nil {
		t.Fatal(err)
	}
	before := wire.Len()
	bad := acceptancePacket(t, init.Video, media.PacketInfo{DTS: math.MaxInt64, PTS: math.MaxInt64}, []byte{1})
	if err := m.WritePacket(bad); !errors.Is(err, fmp4.ErrUnsupportedTiming) || wire.Len() != before {
		t.Fatalf("непредставимый пакет изменил транспорт: %v", err)
	}
	good := acceptancePacket(t, init.Video, media.PacketInfo{DTS: 0, PTS: 1}, []byte{2})
	if err := m.WritePacket(good); err != nil {
		t.Fatal("ошибка до записи испортила исправный транспорт:", err)
	}
}

type acceptanceCountingReader struct {
	r     io.Reader
	bytes int
}

func (r *acceptanceCountingReader) Read(p []byte) (int, error) {
	n, err := r.r.Read(p)
	r.bytes += n
	return n, err
}

func TestAcceptanceDemuxRejectsSampleCountAndOffsetBeforePayload(t *testing.T) {
	for _, violation := range []string{"sample-count", "sample-offset"} {
		t.Run(violation, func(t *testing.T) {
			var wire bytes.Buffer
			init := acceptanceInit(t)
			m, err := fmp4.NewMuxer(&wire, init, acceptanceLimits())
			if err != nil {
				t.Fatal(err)
			}
			if err := m.WritePacket(acceptancePacket(t, init.Video, media.PacketInfo{DTS: 0, PTS: 0, DurationTicks: 1, Keyframe: true}, []byte{1, 2, 3})); err != nil {
				t.Fatal(err)
			}
			data := bytes.Clone(wire.Bytes())
			trun := acceptanceBox(t, data, "moof", "traf", "trun")
			wantErr := fmp4.ErrLimit
			if violation == "sample-count" {
				binary.BigEndian.PutUint32(trun[4:8], acceptanceLimits().MaxSamples+1)
			} else {
				binary.BigEndian.PutUint32(trun[8:12], math.MaxInt32)
				wantErr = fmp4.ErrInvalid
			}
			reader := &acceptanceCountingReader{r: bytes.NewReader(data)}
			d, err := fmp4.NewDemuxer(reader, fmp4.DemuxOptions{Limits: acceptanceLimits()})
			if err != nil {
				t.Fatal(err)
			}
			if _, err := d.Next(); !errors.Is(err, wantErr) {
				t.Fatalf("недопустимый sample принят: %v", err)
			}
			if reader.bytes > len(data)-3 {
				t.Fatal("payload прочитан до проверки count/offset")
			}
		})
	}
}

func acceptanceH264Init(t *testing.T) fmp4.Init {
	t.Helper()
	init := acceptanceInit(t)
	info := init.Video.Info()
	info.Video.Width, info.Video.Height = 128, 128
	info.Video.FrameRate = core.FrameRate{Numerator: 30, Denominator: 1}
	// Синтетический источник получен через libx264; ffprobe подтвердил 128×128, 30 FPS, High и yuv420p.
	headers, err := hex.DecodeString("0164000bffe100186764000bacd942046c0440000003004000000f03c50a658001000668ebe3cb22c0fdf8f800")
	if err != nil {
		t.Fatal(err)
	}
	init.Video, err = media.NewCodecConfig(info, headers)
	if err != nil {
		t.Fatal(err)
	}
	return init
}

func TestAcceptanceVideoDescriptionCannotContradictSampleEntry(t *testing.T) {
	for _, violation := range []string{"width", "codec", "resolver-width", "resolver-codec"} {
		t.Run(violation, func(t *testing.T) {
			init := acceptanceH264Init(t)
			var wire bytes.Buffer
			if _, err := fmp4.NewMuxer(&wire, init, acceptanceLimits()); err != nil {
				t.Fatal(err)
			}
			data := bytes.Clone(wire.Bytes())
			options := fmp4.DemuxOptions{Limits: acceptanceLimits()}
			switch violation {
			case "width":
				pos := bytes.Index(data, []byte("avc1"))
				if pos < 4 {
					t.Fatal("нет avc1")
				}
				binary.BigEndian.PutUint16(data[pos+4+24:pos+4+26], 144)
			case "codec":
				pos := bytes.Index(data, []byte("avc1"))
				if pos < 4 {
					t.Fatal("нет avc1")
				}
				copy(data[pos:pos+4], "av01")
				pos = bytes.Index(data, []byte("avcC"))
				if pos < 4 {
					t.Fatal("нет avcC")
				}
				copy(data[pos:pos+4], "av1C")
			default:
				pos := bytes.Index(data, []byte("uuid"))
				if pos < 4 {
					t.Fatal("нет metadata uuid")
				}
				copy(data[pos:pos+4], "free")
				options.ResolveConfig = func(desc fmp4.TrackDescription) (media.CodecConfig, error) {
					if desc.Track == media.TrackAudio {
						return fmp4.ResolveCodecConfig(desc, 9, 1)
					}
					info := init.Video.Info()
					if violation == "resolver-width" {
						info.Video.Width = 144
					} else {
						info.Video.Codec = core.CodecAV1
					}
					return media.NewCodecConfig(info, desc.Headers)
				}
			}
			if _, err := fmp4.NewDemuxer(bytes.NewReader(data), options); err == nil {
				t.Fatal("противоречие video metadata и sample entry принято")
			}
		})
	}
}

func TestAcceptanceRetainedMetadataCannotOverrideH264SPSDimensions(t *testing.T) {
	init := acceptanceH264Init(t)
	info := init.Video.Info()
	info.Video.Width = 144
	var err error
	init.Video, err = media.NewCodecConfig(info, init.Video.Headers())
	if err != nil {
		t.Fatal(err)
	}
	var wire bytes.Buffer
	if _, err := fmp4.NewMuxer(&wire, init, acceptanceLimits()); err != nil {
		return
	}
	if _, err := fmp4.NewDemuxer(bytes.NewReader(wire.Bytes()), fmp4.DemuxOptions{Limits: acceptanceLimits()}); err == nil {
		t.Fatal("UUID и sample entry объявили 144×128 вопреки реальному SPS 128×128")
	}
}

func TestAcceptanceDefaultVideoResolverChecksActualSPSDimensions(t *testing.T) {
	init := acceptanceH264Init(t)
	desc := fmp4.TrackDescription{Track: media.TrackVideo, Codec: core.CodecH264, TimeBase: init.Video.Info().TimeBase, Width: 128, Height: 128, Headers: init.Video.Headers()}
	if _, err := fmp4.ResolveCodecConfig(desc, 1, 1); err != nil {
		t.Fatal("валидный фактический SPS отвергнут:", err)
	}
	desc.Width = 144
	if _, err := fmp4.ResolveCodecConfig(desc, 1, 1); err == nil {
		t.Fatal("SPS не ограничил заявленные размеры sample entry")
	}
}
