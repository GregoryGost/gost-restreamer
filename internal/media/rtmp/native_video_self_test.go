package rtmp

import (
	"bytes"
	"context"
	"errors"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/codec"
	"github.com/bluenviron/gortmplib/pkg/amf0"
	"github.com/bluenviron/gortmplib/pkg/message"
)

// nativeVideoFormat извлекает фактический формат выбранной синтетической фикстуры без подстановки желаемого.
func nativeVideoFormat(t *testing.T, kind string, headers, first []byte) core.VideoFormat {
	t.Helper()
	// actual содержит измеренный формат; err сообщает об отказе разбора конфигурации кодека.
	var actual core.VideoFormat
	var err error
	switch kind {
	case "hevc":
		actual, err = codec.HEVCFormat(codec.VideoDescription{Headers: headers})
	case "h264":
		actual, err = codec.H264Format(codec.VideoDescription{Headers: headers, Width: 128, Height: 128})
	case "vp9":
		actual, err = codec.VP9Format(headers, first, core.FrameRate{Numerator: 30, Denominator: 1})
	}
	if err != nil {
		t.Fatal(err)
	}
	return actual
}

// TestNativeVideoRecordsAndSignedCTS проверяет реальные байты HEVC/VP9 и Enhanced HDR AVC.
// Сериализатор сохраняет конфигурационные записи, AU и знаковый HEVC CTS; нулевой CTS использует FramesX.
func TestNativeVideoRecordsAndSignedCTS(t *testing.T) {
	// kind выбирает реальный кодек; каждый подтест использует отдельный сериализатор и буфер.
	for _, kind := range []string{"hevc", "h264", "vp9"} {
		// Подтест извлекает фактический формат из заголовков и AU без повторной сериализации SDK.
		t.Run(kind, func(t *testing.T) {
			// headers, first и second сохраняют полные исходные сжатые буферы фикстуры.
			headers, first, second := nativeVideoFixture(t, kind)
			// actual содержит только измеренные геометрию, CICP, профиль, глубину и частоту кадров.
			actual := nativeVideoFormat(t, kind, headers, first)
			// state извлекает статические HDR-сведения первого независимого AU, metadata хранит их.
			// err сообщает об отказе разбора и последующих проверок текущего потока.
			var state codec.HDRState
			metadata, err := state.Observe(actual.Codec, headers, first, true)
			if err != nil {
				t.Fatal(err)
			}
			// video задаёт исходный фактический снимок и точную миллисекундную шкалу для CTS.
			video, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: 1, Revision: 1, TimeBase: media.TimeBase{Numerator: 1, Denominator: 1000}, Video: actual, HDR: metadata}, headers)
			if err != nil {
				t.Fatal(err)
			}
			// audio предоставляет неизменную AAC-LC дорожку того же поколения источника.
			_, audio := transportSelfConfigs(t, video.Info().TimeBase)
			// memory и conn выполняют настоящую сериализацию без сокета и внешнего получателя.
			memory := &transportSelfMemory{}
			conn := transportSelfConn(t, memory)
			conn.target.profile = core.MediaFormat{Video: actual, Audio: audio.Info().Audio}
			if err = validateOutboundProfile(conn.target.profile); err != nil {
				t.Fatal(err)
			}
			if err = conn.WriteHeaders(context.Background(), video, audio); err != nil {
				t.Fatal(err)
			}
			// firstInfo содержит отрицательное начало и отрицательный HEVC/H264 CTS; VP9 требует PTS=DTS.
			firstInfo := media.PacketInfo{PTS: -20, DTS: -10}
			if kind == "vp9" {
				firstInfo.PTS = firstInfo.DTS
			}
			// packet сохраняет исходную AU; независимость сериализатор подтверждает по данным.
			packet, err := media.NewPacket(firstInfo, video, first)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = conn.WritePacket(context.Background(), packet); err != nil {
				t.Fatal(err)
			}
			// reader перебирает реальные сообщения до SequenceStart, затем до CodedFrames.
			reader := bytes.NewReader(memory.output.Bytes())
			// frame сохраняет очередное полное сообщение штатного исходящего RTMP.
			var frame transportSelfFrame
			for {
				frame, err = transportSelfReadFrame(reader, 128)
				if err != nil {
					t.Fatal(err)
				}
				if frame.typ == 9 && frame.body[0]&15 == 0 {
					break
				}
			}
			// fourCC задаёт буквальные идентификаторы Enhanced RTMP независимо от рабочего преобразователя кодеков.
			fourCC := map[string]string{"hevc": "hvc1", "h264": "avc1", "vp9": "vp09"}[kind]
			if len(frame.body) < 5 || frame.body[0] != 0x90 || string(frame.body[1:5]) != fourCC || !bytes.Equal(frame.body[5:], headers) {
				t.Fatalf("Неверный record или добавлен FullBox: %x", frame.body)
			}
			// audioFrame сохраняет отдельный прежний ASC SequenceStart.
			audioFrame, err := transportSelfReadFrame(reader, 128)
			if err != nil || audioFrame.typ != 8 {
				t.Fatalf("Отсутствует AAC SequenceStart: %+v %v", audioFrame, err)
			}
			frame, err = transportSelfReadFrame(reader, 128)
			if err != nil {
				t.Fatal(err)
			}
			// offset задаёт начало исходных сжатых данных после нормативного префикса кодека.
			offset := 8
			if kind == "vp9" {
				offset = 5
			} else if !bytes.Equal(frame.body[5:8], []byte{0xff, 0xff, 0xf6}) {
				t.Fatalf("Потерян отрицательный CTS: %x", frame.body[:8])
			}
			if frame.body[0] != 0x91 || string(frame.body[1:5]) != fourCC || !bytes.Equal(frame.body[offset:], first) {
				t.Fatal("Изменён CodedFrames payload")
			}
			if kind != "vp9" && frame.stamp != 10 || kind == "vp9" && frame.stamp != 0 {
				t.Fatalf("Потеряна знаковая исходная шкала: %d", frame.stamp)
			}
			memory.output.Reset()
			packet, err = media.NewPacket(media.PacketInfo{PTS: 100, DTS: 100}, video, second)
			if err != nil {
				t.Fatal(err)
			}
			if _, err = conn.WritePacket(context.Background(), packet); err != nil {
				t.Fatal(err)
			}
			frame, err = transportSelfReadFrame(bytes.NewReader(memory.output.Bytes()), 128)
			if err != nil {
				t.Fatal(err)
			}
			if string(frame.body[1:5]) != fourCC || kind == "hevc" && (frame.body[0] != 0xa3 || !bytes.Equal(frame.body[5:], second)) || kind == "vp9" && (frame.body[0] != 0xa1 || !bytes.Equal(frame.body[5:], second)) || kind == "h264" && (frame.body[0] != 0xa1 || !bytes.Equal(frame.body[5:8], []byte{0, 0, 0}) || !bytes.Equal(frame.body[8:], second)) {
				t.Fatalf("Неверная нулевая CTS форма: %x", frame.body)
			}
		})
	}
}

// TestNewVideoIngestPreservesAUAndTiming проверяет приём независимых и зависимых AU новых кодеков с исходными байтами.
func TestNewVideoIngestPreservesAUAndTiming(t *testing.T) {
	// kind выбирает HEVC либо VP9, принятые входящим Enhanced RTMP портом.
	for _, kind := range []string{"hevc", "vp9"} {
		// Подтест сохраняет отдельную публикацию, record и актуальную подписку.
		t.Run(kind, func(t *testing.T) {
			// err сохраняет отказы последовательных шагов настройки и передачи публикации.
			var err error
			// headers, first и second содержат исходные сжатые данные без транспортных заголовков.
			headers, first, second := nativeVideoFixture(t, kind)
			// p и sub создают новую публикацию с действующим поколением источника.
			p, sub := newContractPublication(t)
			p.fps = core.FrameRate{Numerator: 30, Denominator: 1}
			if kind == "hevc" {
				p.fps = core.FrameRate{Numerator: 12, Denominator: 1}
			}
			// fourCC выбирает нормативный транспортный идентификатор Enhanced RTMP.
			fourCC := message.FourCCHEVC
			if kind == "vp9" {
				fourCC = message.FourCCVP9
			}
			p.wire = &wireConn{lastConfig: append([]byte{0x90, byte(fourCC >> 24), byte(fourCC >> 16), byte(fourCC >> 8), byte(fourCC)}, headers...)}
			if err = p.videoConfig(fourCC); err != nil {
				t.Fatal(err)
			}
			p.wire.lastConfig = []byte{0xaf, 0, 0x11, 0x90}
			if err = p.audioConfig(2); err != nil {
				t.Fatal(err)
			}
			if err = p.videoPacketCTS(10*time.Millisecond, -5*time.Millisecond, first); kind == "hevc" && err != nil || kind == "vp9" && !errors.Is(err, codec.ErrTiming) {
				t.Fatal(err)
			}
			if kind == "vp9" {
				// Ненулевой CTS VP9 уже отвергнут; следующая нормативная AU сохраняет одну метку.
				if err = p.videoPacket(10*time.Millisecond, first); err != nil {
					t.Fatal(err)
				}
			}
			if err = p.audioPacket(10*time.Millisecond, []byte{0xbe, 0xef}); err != nil {
				t.Fatal(err)
			}
			// d содержит первый независимый AU и точную неизменяемую конфигурационную запись публикации.
			d := delivery(t, sub)
			_ = delivery(t, sub)
			if !bytes.Equal(d.Packet.Payload(), first) || !bytes.Equal(d.VideoConfig.Headers(), headers) {
				t.Fatal("Ingest изменил record либо AU")
			}
			if kind == "hevc" && d.Packet.Info().PTS >= d.Packet.Info().DTS {
				t.Fatal("Отрицательный HEVC CTS потерян")
			}
			if err = p.videoPacket(100*time.Millisecond, second); err != nil {
				t.Fatal(err)
			}
			d = delivery(t, sub)
			if !bytes.Equal(d.Packet.Payload(), second) || d.Packet.Info().Keyframe {
				t.Fatal("Inter AU потерял bytes либо стал независимым")
			}
		})
	}
}

// TestColorOverrideRejectsCodedContradiction подтверждает, что внешние теги не заменяют известный цвет.
func TestColorOverrideRejectsCodedContradiction(t *testing.T) {
	// override содержит разобранный полный объект HDR, err сообщает об отказе его проверки.
	// video задаёт известные SDR-характеристики сжатого потока.
	override, err := parseColorOverride(selfColorInfo(1000))
	if err != nil {
		t.Fatal(err)
	}
	video := core.VideoFormat{BitDepth: 8, Color: core.ColorInfo{Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"}}
	if _, _, err = override.apply(video, media.HDRMetadata{}); err == nil {
		t.Fatal("HDR override ретегировал SDR поток")
	}
	if _, err = parseColorOverride(amf0.Object{}); err != nil {
		t.Fatal(err)
	}
}

// TestIncompleteHEVCIngestKeepsOriginalRecord подтверждает восстановление внутреннего состояния разбора
// из первого AU без изменения неполного ERTMP hvcC, публикуемых заголовков или исходных байтов NAL.
func TestIncompleteHEVCIngestKeepsOriginalRecord(t *testing.T) {
	// complete, first и second содержат полную конфигурационную запись и независимый и зависимый AU синтетического PQ x265.
	complete, first, second := nativeVideoFixture(t, "hevc")
	// raw сохраняет исходную сводку hvcC без массивов NAL, разрешённую отдельной привязкой ERTMP.
	raw := bytes.Clone(complete[:23])
	raw[22] = 0
	// p и sub создают отдельную публикацию с актуальным источником и подпиской.
	p, sub := newContractPublication(t)
	p.wire = &wireConn{lastConfig: append([]byte{0x90, 'h', 'v', 'c', '1'}, raw...)}
	// err хранит последовательные результаты настройки, приёма и штатной сериализации.
	var err error
	if err = p.videoConfig(message.FourCCHEVC); err != nil {
		t.Fatal(err)
	}
	p.wire.lastConfig = []byte{0xaf, 0, 0x11, 0x90}
	if err = p.audioConfig(2); err != nil {
		t.Fatal(err)
	}
	if err = p.videoPacket(0, first); err != nil {
		t.Fatal(err)
	}
	if err = p.audioPacket(0, []byte{0xbe, 0xef}); err != nil {
		t.Fatal(err)
	}
	// sample содержит фактические геометрию и FPS с побайтно сохранённой исходной неполной записью.
	sample := delivery(t, sub)
	_ = delivery(t, sub)
	if !bytes.Equal(sample.VideoConfig.Headers(), raw) || !bytes.Equal(sample.Packet.Payload(), first) || !sample.Packet.Info().Keyframe {
		t.Fatal("Derived context подменил raw hvcC либо AU")
	}
	if err = p.videoPacket(100*time.Millisecond, second); err != nil {
		t.Fatal(err)
	}
	// nextVideo подтверждает, что сохранённое состояние разбора обслуживает последующий зависимый кадр.
	nextVideo := delivery(t, sub).Packet
	if !bytes.Equal(nextVideo.Payload(), second) || !bytes.Equal(nextVideo.Config().Headers(), raw) {
		t.Fatal("Interframe потерял исходный record либо payload")
	}
	// memory и conn проверяют штатный SequenceStart с той же исходной неполной конфигурационной записью.
	memory := &transportSelfMemory{}
	conn := transportSelfConn(t, memory)
	conn.target.profile = core.MediaFormat{Video: sample.VideoConfig.Info().Video, Audio: sample.AudioConfig.Info().Audio}
	if err = conn.WriteHeaders(context.Background(), sample.VideoConfig, sample.AudioConfig); err != nil {
		t.Fatal(err)
	}
	if _, err = conn.WritePacket(context.Background(), sample.Packet); err != nil {
		t.Fatal(err)
	}
	// reader ищет настоящий SequenceStart среди Metadata/onMetaData, не перемаршаливая hvcC.
	reader := bytes.NewReader(memory.output.Bytes())
	for {
		// frame содержит очередное полное сетевое сообщение; readErr сообщает об отказе его чтения.
		frame, readErr := transportSelfReadFrame(reader, 128)
		if readErr != nil {
			t.Fatal(readErr)
		}
		if frame.typ == 9 && frame.body[0]&15 == 0 {
			if !bytes.Equal(frame.body[5:], raw) {
				t.Fatal("Native RTMP подменил incomplete hvcC")
			}
			break
		}
	}
}
