package media_test

import (
	"bytes"
	"encoding/hex"
	"encoding/json"
	"errors"
	"math"
	"os"
	"sync"
	"testing"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
)

func videoInfo() media.CodecConfigInfo {
	return media.CodecConfigInfo{
		Track: media.TrackVideo, SourceEpoch: 7, Revision: 3,
		TimeBase: media.TimeBase{Numerator: 1, Denominator: 90000},
		Video: core.VideoFormat{
			Codec: core.CodecAV1, Width: 2560, Height: 1440,
			FrameRate:    core.FrameRate{Numerator: 60, Denominator: 1},
			CodecProfile: "main", PixelFormat: "yuv420p", BitDepth: 8,
			Color: core.ColorInfo{Range: "limited", Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"},
		},
	}
}

func audioInfo() media.CodecConfigInfo {
	return media.CodecConfigInfo{
		Track: media.TrackAudio, SourceEpoch: 7, Revision: 9,
		TimeBase: media.TimeBase{Numerator: 1, Denominator: 48000},
		Audio:    core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", SampleRateHz: 48000, Channels: 2, ChannelLayout: "stereo"},
	}
}

func configFor(t *testing.T, info media.CodecConfigInfo) media.CodecConfig {
	t.Helper()
	c, err := media.NewCodecConfig(info, []byte{1, 2, 3})
	if err != nil {
		t.Fatalf("NewCodecConfig: %v", err)
	}
	return c
}

func requireCategory(t *testing.T, err, category error) {
	t.Helper()
	if !errors.Is(err, category) {
		t.Fatalf("ошибка = %v, нужна категория %v", err, category)
	}
	if err.Error() == category.Error() {
		t.Errorf("ошибка %v не содержит контекста", err)
	}
}

func TestTrackContractValues(t *testing.T) {
	if media.TrackVideo != "video" || media.TrackAudio != "audio" {
		t.Fatal("изменились строковые значения дорожек")
	}
}

func TestTimeBaseValidationAndEquality(t *testing.T) {
	cases := []struct {
		name string
		a, b media.TimeBase
		want bool
	}{
		{"equivalent", media.TimeBase{Numerator: 1, Denominator: 90000}, media.TimeBase{Numerator: 2, Denominator: 180000}, true},
		{"different", media.TimeBase{Numerator: 1, Denominator: 90000}, media.TimeBase{Numerator: 1, Denominator: 48000}, false},
		{"large_equal", media.TimeBase{Numerator: math.MaxUint32, Denominator: math.MaxUint32}, media.TimeBase{Numerator: 1, Denominator: 1}, true},
		{"overflow_collision", media.TimeBase{Numerator: math.MaxUint32, Denominator: 1}, media.TimeBase{Numerator: 1, Denominator: math.MaxUint32}, false},
		{"zero", media.TimeBase{}, media.TimeBase{}, false},
		{"zero_numerator", media.TimeBase{Numerator: 0, Denominator: 1}, media.TimeBase{Numerator: 1, Denominator: 1}, false},
		{"zero_denominator", media.TimeBase{Numerator: 1, Denominator: 0}, media.TimeBase{Numerator: 1, Denominator: 1}, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if got := tc.a.Equal(tc.b); got != tc.want || tc.b.Equal(tc.a) != tc.want {
				t.Fatalf("Equal(%v, %v) = %v, нужно %v в обоих направлениях", tc.a, tc.b, got, tc.want)
			}
		})
	}
	for _, tb := range []media.TimeBase{{}, {0, 1}, {1, 0}} {
		requireCategory(t, tb.Validate(), media.ErrInvalidTimeBase)
	}
	for _, tb := range []media.TimeBase{{1, 1}, {math.MaxUint32, math.MaxUint32}} {
		if err := tb.Validate(); err != nil {
			t.Fatalf("положительная TimeBase %v отклонена: %v", tb, err)
		}
	}
}

func TestCodecConfigValidation(t *testing.T) {
	cases := []struct {
		name   string
		info   media.CodecConfigInfo
		mutate func(*media.CodecConfigInfo)
	}{
		{"track_empty", videoInfo(), func(i *media.CodecConfigInfo) { i.Track = "" }},
		{"track_unknown", videoInfo(), func(i *media.CodecConfigInfo) { i.Track = "subtitle" }},
		{"epoch_zero", videoInfo(), func(i *media.CodecConfigInfo) { i.SourceEpoch = 0 }},
		{"revision_zero", videoInfo(), func(i *media.CodecConfigInfo) { i.Revision = 0 }},
		{"timebase_numerator", videoInfo(), func(i *media.CodecConfigInfo) { i.TimeBase.Numerator = 0 }},
		{"timebase_denominator", videoInfo(), func(i *media.CodecConfigInfo) { i.TimeBase.Denominator = 0 }},
		{"video_invalid", videoInfo(), func(i *media.CodecConfigInfo) { i.Video.Width = 0 }},
		{"audio_invalid", audioInfo(), func(i *media.CodecConfigInfo) { i.Audio.SampleRateHz = 0 }},
		{"video_with_audio", videoInfo(), func(i *media.CodecConfigInfo) { i.Audio = audioInfo().Audio }},
		{"audio_with_video", audioInfo(), func(i *media.CodecConfigInfo) { i.Video = videoInfo().Video }},
		{"video_with_partial_audio", videoInfo(), func(i *media.CodecConfigInfo) { i.Audio.Channels = 1 }},
		{"audio_with_partial_video", audioInfo(), func(i *media.CodecConfigInfo) { i.Video.Level = "4" }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			i := tc.info
			tc.mutate(&i)
			_, err := media.NewCodecConfig(i, []byte{1})
			requireCategory(t, err, media.ErrInvalidConfig)
		})
	}
	for _, headers := range [][]byte{nil, {}} {
		_, err := media.NewCodecConfig(videoInfo(), headers)
		requireCategory(t, err, media.ErrInvalidConfig)
	}
	for _, info := range []media.CodecConfigInfo{videoInfo(), audioInfo()} {
		info.SourceEpoch = math.MaxUint64
		info.Revision = math.MaxUint64
		c := configFor(t, info)
		if err := c.Validate(); err != nil {
			t.Fatalf("Корректная конфигурация отклонена: %v", err)
		}
		if c.Info() != info || c.StoredBytes() != 3 || !bytes.Equal(c.Headers(), []byte{1, 2, 3}) {
			t.Fatal("конструктор не сохранил описание и headers")
		}
	}
}

func TestPacketValidationAndSignedTimestamps(t *testing.T) {
	v := configFor(t, videoInfo())
	a := configFor(t, audioInfo())
	cases := []media.PacketInfo{
		{}, {PTS: -3000, DTS: -1500, DurationTicks: 1500},
		{PTS: math.MinInt64, DTS: math.MaxInt64, DurationTicks: math.MaxInt64, Keyframe: true},
		{PTS: math.MaxInt64, DTS: math.MinInt64, DurationTicks: 1},
	}
	for _, info := range cases {
		p, err := media.NewPacket(info, v, []byte{0x10})
		if err != nil {
			t.Fatalf("допустимые PTS/DTS %v отклонены: %v", info, err)
		}
		if err := p.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if p.Info() != info || p.Config().Info() != v.Info() || !bytes.Equal(p.Payload(), []byte{0x10}) || p.StoredBytes() != 4 {
			t.Fatalf("пакет изменил значения: %v", info)
		}
	}
	for _, tc := range []struct {
		name    string
		info    media.PacketInfo
		config  media.CodecConfig
		payload []byte
	}{
		{"duration_negative", media.PacketInfo{DurationTicks: -1}, v, []byte{1}},
		{"audio_keyframe", media.PacketInfo{Keyframe: true}, a, []byte{1}},
		{"config_zero", media.PacketInfo{}, media.CodecConfig{}, []byte{1}},
		{"nil_payload", media.PacketInfo{}, v, nil},
		{"empty_payload", media.PacketInfo{}, v, []byte{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := media.NewPacket(tc.info, tc.config, tc.payload)
			requireCategory(t, err, media.ErrInvalidPacket)
		})
	}
	p, err := media.NewPacket(media.PacketInfo{PTS: -1024, DTS: -1024, DurationTicks: 1024}, a, []byte{1})
	if err != nil || p.Info().Keyframe || p.Config().Info().Track != media.TrackAudio {
		t.Fatalf("корректный аудиопакет: %v, %v", p.Info(), err)
	}
}

func TestZeroValuesHaveSafeGetters(t *testing.T) {
	var c media.CodecConfig
	if c.Info() != (media.CodecConfigInfo{}) || len(c.Headers()) != 0 || c.StoredBytes() != 0 {
		t.Fatal("Методы чтения нулевой конфигурации вернули ненулевые данные")
	}
	requireCategory(t, c.Validate(), media.ErrInvalidConfig)
	var p media.Packet
	if p.Info() != (media.PacketInfo{}) || len(p.Payload()) != 0 || p.StoredBytes() != 0 || p.Config().Info() != (media.CodecConfigInfo{}) || len(p.Config().Headers()) != 0 {
		t.Fatal("Методы чтения нулевого пакета вернули ненулевые данные")
	}
	requireCategory(t, p.Validate(), media.ErrInvalidPacket)
}

func TestPacketAndConfigOwnership(t *testing.T) {
	headers := []byte{1, 2, 3}
	info := videoInfo()
	c, err := media.NewCodecConfig(info, headers)
	if err != nil {
		t.Fatal(err)
	}
	copyOfConfig := c
	payload := []byte{4, 5, 6, 7}
	packetInfo := media.PacketInfo{PTS: 2, DTS: -1, DurationTicks: 1, Keyframe: true}
	p, err := media.NewPacket(packetInfo, c, payload)
	if err != nil {
		t.Fatal(err)
	}
	copyOfPacket := p
	other, err := media.NewPacket(media.PacketInfo{}, c, []byte{8})
	if err != nil {
		t.Fatal(err)
	}
	headers[0], payload[0] = 99, 99
	info.Video.Width = 1
	packetInfo.PTS = 99
	c.Headers()[1] = 99
	p.Payload()[1] = 99
	p.Config().Headers()[2] = 99
	returnedConfigInfo := c.Info()
	returnedConfigInfo.Revision = 99
	returnedPacketInfo := p.Info()
	returnedPacketInfo.PTS = 99
	for _, got := range []media.CodecConfig{c, copyOfConfig, p.Config(), copyOfPacket.Config(), other.Config()} {
		if got.Info() != videoInfo() || !bytes.Equal(got.Headers(), []byte{1, 2, 3}) || got.StoredBytes() != 3 {
			t.Fatal("Конфигурация изменилась через общий буфер или описание")
		}
	}
	for _, got := range []media.Packet{p, copyOfPacket} {
		if got.Info() != (media.PacketInfo{PTS: 2, DTS: -1, DurationTicks: 1, Keyframe: true}) || !bytes.Equal(got.Payload(), []byte{4, 5, 6, 7}) || got.StoredBytes() != 7 {
			t.Fatal("пакет изменился через общий буфер или описание")
		}
	}
	if other.StoredBytes() != 4 || !bytes.Equal(other.Payload(), []byte{8}) {
		t.Fatal("Общая конфигурация учтена неверно во втором пакете")
	}
}

func TestImmutableValuesSupportConcurrentReaders(t *testing.T) {
	c := configFor(t, videoInfo())
	p, err := media.NewPacket(media.PacketInfo{PTS: -1}, c, []byte{4, 5})
	if err != nil {
		t.Fatal(err)
	}
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			for range 32 {
				headers, payload := p.Config().Headers(), p.Payload()
				if !bytes.Equal(headers, []byte{1, 2, 3}) || !bytes.Equal(payload, []byte{4, 5}) || p.StoredBytes() != 5 || p.Info().PTS != -1 {
					t.Error("совместное чтение изменило значение")
					return
				}
				headers[0], payload[0] = 99, 99
			}
		})
	}
	wg.Wait()
	if !bytes.Equal(p.Payload(), []byte{4, 5}) || !bytes.Equal(c.Headers(), []byte{1, 2, 3}) {
		t.Fatal("изменения копий повлияли на оригинал")
	}
}

func TestSyntheticPacketSequence(t *testing.T) {
	data, err := os.ReadFile("testdata/packet-sequence.json")
	if err != nil {
		t.Fatal(err)
	}
	var fixture struct {
		Configs []struct {
			Name        string
			Info        media.CodecConfigInfo
			HeadersHex  string `json:"headers_hex"`
			StoredBytes uint64 `json:"stored_bytes"`
		}
		Packets []struct {
			ConfigName  string `json:"config_name"`
			Info        media.PacketInfo
			PayloadHex  string `json:"payload_hex"`
			StoredBytes uint64 `json:"stored_bytes"`
		}
	}
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Configs) != 2 || len(fixture.Packets) != 4 {
		t.Fatal("изменился состав синтетического набора")
	}
	configs := make(map[string]media.CodecConfig)
	for _, entry := range fixture.Configs {
		headers, err := hex.DecodeString(entry.HeadersHex)
		if err != nil {
			t.Fatal(err)
		}
		c, err := media.NewCodecConfig(entry.Info, headers)
		if err != nil {
			t.Fatalf("config %s: %v", entry.Name, err)
		}
		if c.Info() != entry.Info || !bytes.Equal(c.Headers(), headers) || c.StoredBytes() != entry.StoredBytes {
			t.Fatalf("config %s не сохранила ожидаемые свойства", entry.Name)
		}
		configs[entry.Name] = c
	}
	if configs["video"].Info() != videoInfo() || configs["audio"].Info() != audioInfo() || configs["video"].StoredBytes() != 3 || configs["audio"].StoredBytes() != 2 {
		t.Fatal("Описания фикстуры не совпадают с независимо заданными форматами")
	}
	wantInfos := []media.PacketInfo{{PTS: 0, DTS: -3000, DurationTicks: 1500, Keyframe: true}, {PTS: -3000, DTS: -1500, DurationTicks: 1500}, {PTS: 1500, DTS: 0}, {PTS: -1024, DTS: -1024, DurationTicks: 1024}}
	wantWeights := []uint64{7, 5, 6, 7}
	wantTracks := []media.Track{media.TrackVideo, media.TrackVideo, media.TrackVideo, media.TrackAudio}
	for n, entry := range fixture.Packets {
		payload, err := hex.DecodeString(entry.PayloadHex)
		if err != nil {
			t.Fatal(err)
		}
		p, err := media.NewPacket(entry.Info, configs[entry.ConfigName], payload)
		if err != nil {
			t.Fatalf("packet %d: %v", n, err)
		}
		if p.Info() != wantInfos[n] || p.Info() != entry.Info || p.StoredBytes() != wantWeights[n] || p.StoredBytes() != entry.StoredBytes || !bytes.Equal(p.Payload(), payload) || p.Config().Info() != configs[entry.ConfigName].Info() || p.Config().Info().Track != wantTracks[n] {
			t.Fatalf("packet %d не сохранил ожидаемые свойства", n)
		}
	}
}
