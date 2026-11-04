package rtmp

import (
	"bytes"
	"context"
	"encoding/binary"
	"errors"
	"fmt"
	"io"
	"math"
	"testing"
	"testing/synctest"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/bluenviron/gortmplib/pkg/amf0"
	"github.com/bluenviron/gortmplib/pkg/bytecounter"
	"github.com/bluenviron/gortmplib/pkg/message"
)

func newContractPublication(t *testing.T) (*publication, *media.Subscription) {
	t.Helper()
	router, err := media.NewRouter(media.RouterLimits{MaxConsumers: 2, MaxRetainedBytes: 4 << 20, MaxConfigBytes: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = router.Close() })
	source, err := router.BeginSource()
	if err != nil {
		t.Fatal(err)
	}
	return &publication{router: router, source: source}, newSubscription(t, router)
}

// contractMessage задаёт независимую абсолютную метку RTMP, включая расширенную метку.
// Все сообщения в этих сценариях помещаются в один исходный блок размером 128 байт.
func contractMessage(id, typ byte, at uint32, body []byte) []byte {
	frame := chunk0(id, typ, body)
	if at < 0xffffff {
		frame[1], frame[2], frame[3] = byte(at>>16), byte(at>>8), byte(at)
		return frame
	}
	frame[1], frame[2], frame[3] = 0xff, 0xff, 0xff
	var extended [4]byte
	binary.BigEndian.PutUint32(extended[:], at)
	return bytes.Join([][]byte{frame[:12], extended[:], frame[12:]}, nil)
}

func contractMetadata(t *testing.T, enhanced bool, data amf0.Data) []byte {
	t.Helper()
	body, err := data.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	typ := byte(18)
	if enhanced {
		typ = 9
		body = append([]byte{0x84, 'a', 'v', '0', '1'}, body...)
	}
	return contractMessage(5, typ, 0, body)
}

// contractConsume проходит тот же ограничитель и декодер сообщений, что и приём публикации.
// Допуск core здесь уже выдан; сетевой жизненный цикл проверяется отдельным сценарием ниже.
func contractConsume(t *testing.T, p *publication, raw []byte) error {
	t.Helper()
	w := guarded(t, raw, testLimits())
	p.wire = w
	counter := bytecounter.NewReader(w)
	reader := message.NewReader(counter, counter, func(uint32) error { return nil })
	for {
		msg, err := reader.Read()
		if errors.Is(err, io.EOF) {
			return nil
		}
		if err != nil {
			return err
		}
		if err = p.message(msg); err != nil {
			return err
		}
	}
}

func contractVideoConfig(config []byte) []byte {
	return contractMessage(6, 9, 0, append([]byte{0x90, 'a', 'v', '0', '1'}, config...))
}

func contractVideo(at uint32, tu []byte) []byte {
	return contractMessage(6, 9, at, append([]byte{0x91, 'a', 'v', '0', '1'}, tu...))
}

func contractAudio(at uint32, au []byte) []byte {
	return contractMessage(4, 8, at, append([]byte{0x91, 'm', 'p', '4', 'a'}, au...))
}

func contractAudioConfig() []byte {
	return contractMessage(4, 8, 0, []byte{0x90, 'm', 'p', '4', 'a', 0x11, 0x90})
}

func contractReady(config []byte, at uint32) []byte {
	return bytes.Join([][]byte{contractVideoConfig(config), contractAudioConfig(), contractVideo(at, contractAV1TU(config)), contractAudio(at, []byte{0xbe, 0xef})}, nil)
}

func requireContractNoDelivery(t *testing.T, sub *media.Subscription) {
	t.Helper()
	if got, err := contractDeliveryIfReady(sub); !errors.Is(err, context.Canceled) {
		t.Fatalf("неожиданная доставка: packet=%+v err=%v", got.Packet.Info(), err)
	}
}

// contractDeliveryIfReady вызывается внутри synctest вместе с созданием подписки.
// Отмена наступает только после завершения Next либо его устойчивой блокировки.
func contractDeliveryIfReady(sub *media.Subscription) (media.Delivery, error) {
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	var got media.Delivery
	var err error
	done := make(chan struct{})
	go func() {
		got, err = sub.Next(ctx)
		close(done)
	}()
	synctest.Wait()
	select {
	case <-done:
	default:
		cancel()
		<-done
	}
	return got, err
}

func TestContractPublicationDeliveryProbeDistinguishesQueuedPacket(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, sub := newContractPublication(t)
		requireContractNoDelivery(t, sub)
		config := contractAV1Config(contractSDRSequence())
		if err := contractConsume(t, p, contractReady(config, 100)); err != nil {
			t.Fatal(err)
		}
		want := map[media.Track][]byte{media.TrackVideo: contractAV1TU(config), media.TrackAudio: {0xbe, 0xef}}
		for range 2 {
			got, err := contractDeliveryIfReady(sub)
			track := got.Packet.Config().Info().Track
			payload, exists := want[track]
			if err != nil || !exists || !bytes.Equal(got.Packet.Payload(), payload) || !acceptanceWireTime(got.Packet, 100) {
				t.Fatalf("доступный пакет не обнаружен: track=%s payload=%x err=%v", track, got.Packet.Payload(), err)
			}
			delete(want, track)
		}
		requireContractNoDelivery(t, sub)
	})
}

func TestContractPublicationHiddenFrameBeforeKeyDoesNotStartFeed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, sub := newContractPublication(t)
		config := contractAV1Config(contractSDRSequence())
		// Первый кадр — скрытый INTER_FRAME; следующий — показанный KEY_FRAME.
		// Префиксы заголовков синтетические и не заявляются декодируемыми.
		tu := append(bytes.Clone(config[4:]), 0x1a, 1, 0x28, 0x1a, 1, 0x10)
		raw := bytes.Join([][]byte{contractVideoConfig(config), contractAudioConfig(), contractVideo(0, tu), contractAudio(0, []byte{0xcc})}, nil)
		if err := contractConsume(t, p, raw); err != nil {
			t.Fatalf("поддержанный TU отвергнут: %v", err)
		}
		requireContractNoDelivery(t, sub)
		if err := contractConsume(t, p, bytes.Join([][]byte{contractVideo(100, contractAV1TU(config)), contractAudio(100, []byte{0xbe, 0xef})}, nil)); err != nil {
			t.Fatal(err)
		}
		v, a := delivery(t, sub), delivery(t, sub)
		if !v.Packet.Info().Keyframe || !acceptanceWireTime(v.Packet, 100) || !acceptanceWireTime(a.Packet, 100) {
			t.Fatal("независимый старт после зависимого TU потерян")
		}
		if err := contractConsume(t, p, contractVideo(133, tu)); err != nil {
			t.Fatal(err)
		}
		got := delivery(t, sub)
		if got.Packet.Info().Keyframe || !bytes.Equal(got.Packet.Payload(), tu) || !acceptanceWireTime(got.Packet, 133) {
			t.Fatal("зависимый TU после старта потерян или помечен независимым")
		}
		requireContractNoDelivery(t, sub)
	})
}

func TestContractPublicationExtendedAACPreservesPayloadAndOrigin(t *testing.T) {
	p, sub := newContractPublication(t)
	config := contractAV1Config(contractSDRSequence())
	if err := contractConsume(t, p, contractReady(config, 123)); err != nil {
		t.Fatal(err)
	}
	v, a := delivery(t, sub), delivery(t, sub)
	if v.Packet.Config().Info().Track != media.TrackVideo || a.Packet.Config().Info().Track != media.TrackAudio || !bytes.Equal(a.Packet.Payload(), []byte{0xbe, 0xef}) || !bytes.Equal(a.AudioConfig.Headers(), []byte{0x11, 0x90}) || !acceptanceWireTime(v.Packet, 123) || !acceptanceWireTime(a.Packet, 123) {
		t.Fatalf("расширенный AAC потерял полезные данные, заголовки или начало шкалы: video=%+v audio=%+v", v.Packet.Info(), a.Packet.Info())
	}
	if got := v.VideoConfig.Info().Video; !got.FrameRate.Equal(core.FrameRate{Numerator: 30000, Denominator: 1001}) || got.Width != 640 || got.Height != 360 {
		t.Fatalf("timing_info не дошёл до Router: %+v", got)
	}
	if got := a.AudioConfig.Info().Audio; got.SampleRateHz != 48000 || got.Channels != 2 || got.ChannelLayout != "stereo" || got.CodecProfile != "LC" {
		t.Fatalf("ASC не определил формат: %+v", got)
	}
	// Ожидания заданы в секундах: длительность видео 1001/30000, AAC — 1024/48000.
	if v.Packet.Info().DurationTicks != 8008 || a.Packet.Info().DurationTicks != 5120 || v.VideoConfig.Info().TimeBase != (media.TimeBase{Numerator: 1, Denominator: 240000}) || a.AudioConfig.Info().TimeBase != v.VideoConfig.Info().TimeBase {
		t.Fatal("потеряна точная общая шкала рационального видео и AAC")
	}
}

func TestContractPublicationWaitsForCompleteIndependentAVStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, sub := newContractPublication(t)
		config := contractAV1Config(contractSDRSequence())
		beforeReady := bytes.Join([][]byte{
			contractAudio(0, []byte{0xde, 0xad}),
			contractVideo(0, contractAV1TU(config)),
			contractVideoConfig(config),
			contractVideo(10, contractAV1TU(config)),
			contractAudioConfig(),
			contractVideo(20, []byte{0x1a, 1, 0x30}),
			contractAudio(20, []byte{0xca, 0xfe}),
		}, nil)
		if err := contractConsume(t, p, beforeReady); err != nil {
			t.Fatal(err)
		}
		requireContractNoDelivery(t, sub)
		if err := contractConsume(t, p, bytes.Join([][]byte{contractVideo(100, contractAV1TU(config)), contractAudio(99, []byte{0xfa}), contractAudio(100, []byte{0xfb})}, nil)); err != nil {
			t.Fatal(err)
		}
		v, a := delivery(t, sub), delivery(t, sub)
		if !v.Packet.Info().Keyframe || !acceptanceWireTime(v.Packet, 100) || !acceptanceWireTime(a.Packet, 100) || !bytes.Equal(a.Packet.Payload(), []byte{0xfb}) {
			t.Fatal("до независимого A/V-старта выпущены прежние пакеты")
		}
		requireContractNoDelivery(t, sub)
	})
}

func TestContractPublicationMetadataRepresentationsAgree(t *testing.T) {
	for _, enhanced := range []bool{false, true} {
		for _, wrapped := range []bool{false, true} {
			for _, ecma := range []bool{false, true} {
				t.Run(fmt.Sprintf("enhanced=%t/wrapped=%t/ecma=%t", enhanced, wrapped, ecma), func(t *testing.T) {
					p, sub := newContractPublication(t)
					var object any = amf0.Object{{Key: "framerate", Value: float64(30000) / 1001}}
					if ecma {
						object = amf0.ECMAArray(object.(amf0.Object))
					}
					data := amf0.Data{"onMetaData", object}
					if wrapped {
						data = append(amf0.Data{"@setDataFrame"}, data...)
					}
					config := testAV1Config()
					raw := bytes.Join([][]byte{contractMetadata(t, enhanced, data), contractVideoConfig(config), contractAudioConfig(), contractVideo(100, testAV1TU()), contractAudio(100, []byte{1})}, nil)
					if err := contractConsume(t, p, raw); err != nil {
						t.Fatal(err)
					}
					v, a := delivery(t, sub), delivery(t, sub)
					if !v.VideoConfig.Info().Video.FrameRate.Equal(core.FrameRate{Numerator: 30000, Denominator: 1001}) || !acceptanceWireTime(v.Packet, 100) || !acceptanceWireTime(a.Packet, 100) {
						t.Fatal("форма AMF изменила наблюдаемую частоту или временную шкалу")
					}
				})
			}
		}
	}
}

func TestContractPublicationInvalidMetadataDoesNotPublishOrReplaceRate(t *testing.T) {
	for _, tc := range []struct {
		name  string
		value any
	}{
		{"zero", float64(0)}, {"negative", float64(-1)}, {"NaN", math.NaN()},
		{"positive-infinity", math.Inf(1)}, {"negative-infinity", math.Inf(-1)},
		{"string", "30"}, {"boolean", true}, {"null", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			synctest.Test(t, func(t *testing.T) {
				p, sub := newContractPublication(t)
				valid := contractMetadata(t, false, amf0.Data{"onMetaData", amf0.Object{{Key: "framerate", Value: float64(30)}}})
				if err := contractConsume(t, p, valid); err != nil {
					t.Fatal(err)
				}
				invalid := contractMetadata(t, true, amf0.Data{"onMetaData", amf0.Object{{Key: "framerate", Value: tc.value}}})
				if err := contractConsume(t, p, invalid); !errors.Is(err, ErrCodec) {
					t.Fatalf("некорректная частота принята: %v", err)
				}
				if !p.fps.Equal(core.FrameRate{Numerator: 30, Denominator: 1}) {
					t.Fatalf("отказ заменил установленную частоту: %+v", p.fps)
				}
				requireContractNoDelivery(t, sub)
			})
		})
	}
	for _, data := range []amf0.Data{{"onMetaData", "invalid-object"}, {"onMetaData", amf0.Object{{Key: "framerate", Value: float64(24)}}}} {
		synctest.Test(t, func(t *testing.T) {
			p, sub := newContractPublication(t)
			p.fps = core.FrameRate{Numerator: 30, Denominator: 1}
			if err := contractConsume(t, p, contractMetadata(t, false, data)); !errors.Is(err, ErrCodec) {
				t.Fatalf("некорректное изменение или тип метаданных приняты: %v", err)
			}
			requireContractNoDelivery(t, sub)
		})
	}
}

func TestContractPublicationTimestampWrapAndResetAreExplicitErrors(t *testing.T) {
	for _, track := range []string{"video", "audio"} {
		for _, wrap := range []bool{false, true} {
			t.Run(track+map[bool]string{false: "-reset", true: "-wrap"}[wrap], func(t *testing.T) {
				synctest.Test(t, func(t *testing.T) {
					p, sub := newContractPublication(t)
					config := contractAV1Config(contractSDRSequence())
					at := uint32(100)
					if wrap {
						at = math.MaxUint32
					}
					bad := contractAudio(0, []byte{0xcc})
					if track == "video" {
						bad = contractVideo(0, contractAV1TU(config))
					}
					if wrap {
						// Блок типа 2 наследует длину и тип, прибавляя 1 мс к 0xffffffff.
						id := byte(4)
						if track == "video" {
							id = 6
						}
						body := contractAudio(at, []byte{0xcc, 0xdd})[16:]
						if track == "video" {
							body = contractVideo(at, contractAV1TU(config))[16:]
						}
						bad = append([]byte{0x80 | id, 0, 0, 1}, body...)
					}
					if err := contractConsume(t, p, append(contractReady(config, at), bad...)); !errors.Is(err, media.ErrDTSRegression) {
						t.Fatalf("сброс или переполнение метки скрыты: %v", err)
					}
					v, a := delivery(t, sub), delivery(t, sub)
					if !acceptanceWireTime(v.Packet, int64(at)) || !acceptanceWireTime(a.Packet, int64(at)) {
						t.Fatal("крайняя абсолютная метка изменилась")
					}
					requireContractNoDelivery(t, sub)
				})
			})
		}
	}
}

func TestContractPublicationContradictoryMetadataStopsBeforeNextVideo(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		p, sub := newContractPublication(t)
		config := contractAV1Config(contractSDRSequence())
		if err := contractConsume(t, p, contractReady(config, 100)); err != nil {
			t.Fatal(err)
		}
		_ = delivery(t, sub)
		_ = delivery(t, sub)
		metadata := contractMetadata(t, false, amf0.Data{"onMetaData", amf0.Object{{Key: "framerate", Value: float64(30)}}})
		if err := contractConsume(t, p, append(metadata, contractVideo(133, contractAV1TU(config))...)); !errors.Is(err, ErrCodec) {
			t.Fatalf("метаданные 30 FPS затмили timing_info 30000/1001: %v", err)
		}
		requireContractNoDelivery(t, sub)
	})
}

func TestContractPublicationTerminalMessagesCloseActiveFeed(t *testing.T) {
	for _, tc := range []struct {
		name string
		raw  []byte
		want error
	}{
		{"audio-end", chunk0(4, 8, []byte{0x92, 'm', 'p', '4', 'a'}), ErrUnready},
		{"video-end", chunk0(6, 9, []byte{0x82, 'a', 'v', '0', '1'}), ErrUnready},
		{"malformed-TU", contractVideo(133, []byte{0x1a, 0}), ErrCodec},
		{"AV1-FramesX", contractMessage(6, 9, 133, append([]byte{0x93, 'a', 'v', '0', '1'}, testAV1TU()...)), ErrCodec},
	} {
		t.Run(tc.name, func(t *testing.T) {
			f := newIngestFixture(t)
			s, address := runServer(t, f, nil)
			sub := newSubscription(t, f.router)
			client, err := connectClient(t, address, f.stream, f.stream.PublicationKey)
			if err != nil {
				t.Fatal(err)
			}
			sendReady(t, client, 100)
			current := delivery(t, sub)
			_ = delivery(t, sub)
			writeWire(t, client, tc.raw)
			if err := serverResult(t, s); !errors.Is(err, tc.want) {
				t.Fatalf("завершающее сообщение не завершило приём требуемой ошибкой: %v", err)
			}
			if current.IsCurrent() {
				t.Fatal("завершённая последовательность осталась текущим источником")
			}
			if _, err := client.Read(); err == nil {
				t.Fatal("соединение завершённой последовательности открыто")
			}
		})
	}
}
