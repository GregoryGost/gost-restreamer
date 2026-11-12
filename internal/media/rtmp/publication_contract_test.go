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

// Создаёт публикацию с текущим Source и подпиской на отдельном Router; закрытие маршрутизатора
// регистрирует в очистке теста.
func newContractPublication(t *testing.T) (*publication, *media.Subscription) {
	t.Helper()
	// router содержит отдельный маршрутизатор тестовой публикации. err сообщает об отказе создания.
	router, err := media.NewRouter(media.RouterLimits{MaxConsumers: 2, MaxRetainedBytes: 4 << 20, MaxConfigBytes: 1 << 16})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает маршрутизатор тестовой публикации при завершении теста.
		_ = router.Close()
	})
	// source содержит текущий допуск нового сегмента тестового маршрутизатора.
	source, err := router.BeginSource()
	if err != nil {
		t.Fatal(err)
	}
	return &publication{router: router, source: source}, newSubscription(t, router)
}

// contractMessage задаёт независимую абсолютную метку RTMP, включая расширенную метку.
// Все сообщения в этих сценариях помещаются в один исходный блок размером 128 байт.
func contractMessage(id, typ byte, at uint32, body []byte) []byte {
	// frame содержит fmt0-сообщение с независимо заданной абсолютной меткой.
	frame := chunk0(id, typ, body)
	if at < 0xffffff {
		frame[1], frame[2], frame[3] = byte(at>>16), byte(at>>8), byte(at)
		return frame
	}
	frame[1], frame[2], frame[3] = 0xff, 0xff, 0xff
	// extended содержит четырёхбайтовую расширенную метку для значений от 0xffffff.
	var extended [4]byte
	binary.BigEndian.PutUint32(extended[:], at)
	return bytes.Join([][]byte{frame[:12], extended[:], frame[12:]}, nil)
}

// Кодирует AMF0-метаданные обычным DataAMF0 либо Enhanced AV1 Metadata для прохождения одного и того
// же приёмного пути.
func contractMetadata(t *testing.T, enhanced bool, data amf0.Data) []byte {
	t.Helper()
	// body содержит сериализованные AMF-метаданные. err сообщает об отказе кодирования.
	body, err := data.Marshal()
	if err != nil {
		t.Fatal(err)
	}
	// typ выбирает DataAMF0 либо Enhanced видеометаданные.
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
	// w содержит ограничитель сырых байтов тестовой публикации.
	w := guarded(t, raw, testLimits())
	p.wire = w
	// counter считает байты для библиотечного читателя сообщений.
	counter := bytecounter.NewReader(w)
	// reader декодирует типизированные сообщения после ограничителя.
	reader := message.NewReader(counter, counter, func(uint32) error {
		// Принимает уведомление о пороге подтверждения без дополнительных действий в локальном разборе
		// публикации.
		return nil
	})
	for {
		// msg содержит очередное декодированное сообщение. err сообщает об окончании потока или
		// отказе чтения.
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

// Оборачивает av1C в независимое Enhanced RTMP SequenceStart видеосообщение.
func contractVideoConfig(config []byte) []byte {
	return contractMessage(6, 9, 0, append([]byte{0x90, 'a', 'v', '0', '1'}, config...))
}

// Оборачивает TU в Enhanced RTMP AV1 CodedFrames с заданной абсолютной меткой.
func contractVideo(at uint32, tu []byte) []byte {
	return contractMessage(6, 9, at, append([]byte{0x91, 'a', 'v', '0', '1'}, tu...))
}

// Оборачивает AAC AU в Enhanced RTMP mp4a CodedFrames с заданной абсолютной меткой.
func contractAudio(at uint32, au []byte) []byte {
	return contractMessage(4, 8, at, append([]byte{0x91, 'm', 'p', '4', 'a'}, au...))
}

// Создаёт Enhanced RTMP mp4a SequenceStart с ASC AAC-LC с двумя каналами и частотой 48 кГц.
func contractAudioConfig() []byte {
	return contractMessage(4, 8, 0, []byte{0x90, 'm', 'p', '4', 'a', 0x11, 0x90})
}

// Собирает обе конфигурации, независимый TU и аудио на одной метке для запуска тестовой публикации.
func contractReady(config []byte, at uint32) []byte {
	return bytes.Join([][]byte{contractVideoConfig(config), contractAudioConfig(), contractVideo(at, contractAV1TU(config)), contractAudio(at, []byte{0xbe, 0xef})}, nil)
}

// Проверяет в synctest отсутствие доступной доставки: устойчиво заблокированный Next должен
// завершиться только по отмене.
func requireContractNoDelivery(t *testing.T, sub *media.Subscription) {
	t.Helper()
	// got содержит возможную неожиданную доставку. err содержит результат пробы готовности подписки.
	if got, err := contractDeliveryIfReady(sub); !errors.Is(err, context.Canceled) {
		t.Fatalf("неожиданная доставка: packet=%+v err=%v", got.Packet.Info(), err)
	}
}

// contractDeliveryIfReady вызывается внутри synctest вместе с созданием подписки.
// Отмена наступает только после завершения Next либо его устойчивой блокировки.
func contractDeliveryIfReady(sub *media.Subscription) (media.Delivery, error) {
	// ctx позволяет отменить устойчиво заблокированный Next. cancel освобождает его ожидание после
	// synctest.Wait.
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	// got принимает возможную доступную доставку из подписки.
	var got media.Delivery
	// err принимает результат Next, в том числе отмену при отсутствии готового пакета.
	var err error
	// done закрывается после завершения одного Next и публикации его результата.
	done := make(chan struct{})
	go func() {
		// Выполняет один Next, сохраняет его результат во внешних переменных и закрывает done; отменой и
		// ожиданием владеет вызывающая функция.

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

// Проверяет, что проба доставки различает пустую подписку и реально готовые A/V-пакеты с точными
// данными и временем.
func TestContractPublicationDeliveryProbeDistinguishesQueuedPacket(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// В управляемом окружении проверяет различие между отсутствующим пакетом и доступными видео и аудио
		// после готовности публикации.

		// p содержит тестовый адаптер публикации. sub содержит подписку на его пакеты.
		p, sub := newContractPublication(t)
		requireContractNoDelivery(t, sub)
		// config содержит независимый av1C с собственным timing_info.
		config := contractAV1Config(contractSDRSequence())
		// err сообщает об отказе обработки полной стартовой A/V-последовательности.
		if err := contractConsume(t, p, contractReady(config, 100)); err != nil {
			t.Fatal(err)
		}
		// want связывает каждую ожидаемую дорожку с исходной нагрузкой для проверки ровно одной
		// выдачи.
		want := map[media.Track][]byte{media.TrackVideo: contractAV1TU(config), media.TrackAudio: {0xbe, 0xef}}
		for range 2 {
			// got содержит результат пробы готовой доставки. err сообщает об отказе её
			// получения.
			got, err := contractDeliveryIfReady(sub)
			// track обозначает дорожку фактически выданного пакета.
			track := got.Packet.Config().Info().Track
			// payload содержит ожидаемые байты выбранной дорожки. exists показывает,
			// ожидалась ли ещё такая дорожка.
			payload, exists := want[track]
			if err != nil || !exists || !bytes.Equal(got.Packet.Payload(), payload) || !acceptanceWireTime(got.Packet, 100) {
				t.Fatalf("доступный пакет не обнаружен: track=%s payload=%x err=%v", track, got.Packet.Payload(), err)
			}
			delete(want, track)
		}
		requireContractNoDelivery(t, sub)
	})
}

// Проверяет ожидание независимого старта после TU со скрытым первым кадром и передачу такого
// зависимого TU после готовности.
func TestContractPublicationHiddenFrameBeforeKeyDoesNotStartFeed(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Проверяет, что скрытый кадр перед показанным ключевым кадром не запускает публикацию, но сохраняется
		// как зависимый TU после независимого старта.

		// p содержит тестовый адаптер публикации. sub содержит подписку на его пакеты.
		p, sub := newContractPublication(t)
		// config содержит независимую последовательность с подтверждаемой частотой.
		config := contractAV1Config(contractSDRSequence())
		// Первый кадр — скрытый INTER_FRAME; следующий — показанный KEY_FRAME.
		// Префиксы заголовков синтетические и не заявляются декодируемыми.
		// tu содержит скрытый INTER_FRAME перед показанным KEY_FRAME и потому не даёт
		// независимого старта.
		tu := append(bytes.Clone(config[4:]), 0x1a, 1, 0x28, 0x1a, 1, 0x10)
		// raw содержит конфигурации и зависимый стартовый TU с аудио.
		raw := bytes.Join([][]byte{contractVideoConfig(config), contractAudioConfig(), contractVideo(0, tu), contractAudio(0, []byte{0xcc})}, nil)
		// err сообщает об отказе обработки допустимого зависимого TU до готовности.
		if err := contractConsume(t, p, raw); err != nil {
			t.Fatalf("поддержанный TU отвергнут: %v", err)
		}
		requireContractNoDelivery(t, sub)
		// err сообщает об отказе последующего независимого A/V-старта.
		if err := contractConsume(t, p, bytes.Join([][]byte{contractVideo(100, contractAV1TU(config)), contractAudio(100, []byte{0xbe, 0xef})}, nil)); err != nil {
			t.Fatal(err)
		}
		// v содержит независимый видеостарт. a содержит согласованное аудио после зависимого TU.
		v, a := delivery(t, sub), delivery(t, sub)
		if !v.Packet.Info().Keyframe || !acceptanceWireTime(v.Packet, 100) || !acceptanceWireTime(a.Packet, 100) {
			t.Fatal("независимый старт после зависимого TU потерян")
		}
		// err сообщает об отказе передачи зависимого TU после готовности.
		if err := contractConsume(t, p, contractVideo(133, tu)); err != nil {
			t.Fatal(err)
		}
		// got содержит доставленный зависимый TU для проверки исходных байтов и метки.
		got := delivery(t, sub)
		if got.Packet.Info().Keyframe || !bytes.Equal(got.Packet.Payload(), tu) || !acceptanceWireTime(got.Packet, 133) {
			t.Fatal("зависимый TU после старта потерян или помечен независимым")
		}
		requireContractNoDelivery(t, sub)
	})
}

// Проверяет исходные байты Enhanced AAC и ASC, общее начало A/V и точные длительности дробного AV1 и
// AAC в шкале 1/240000.
func TestContractPublicationExtendedAACPreservesPayloadAndOrigin(t *testing.T) {
	// p содержит тестовый адаптер публикации. sub содержит подписку на его пакеты.
	p, sub := newContractPublication(t)
	// config содержит av1C с дробной частотой из timing_info.
	config := contractAV1Config(contractSDRSequence())
	// err сообщает об отказе стартовой последовательности с Enhanced AAC.
	if err := contractConsume(t, p, contractReady(config, 123)); err != nil {
		t.Fatal(err)
	}
	// v содержит видеопакет с исходным временем. a содержит AAC-пакет с исходными ASC и нагрузкой.
	v, a := delivery(t, sub), delivery(t, sub)
	if v.Packet.Config().Info().Track != media.TrackVideo || a.Packet.Config().Info().Track != media.TrackAudio || !bytes.Equal(a.Packet.Payload(), []byte{0xbe, 0xef}) || !bytes.Equal(a.AudioConfig.Headers(), []byte{0x11, 0x90}) || !acceptanceWireTime(v.Packet, 123) || !acceptanceWireTime(a.Packet, 123) {
		t.Fatalf("расширенный AAC потерял полезные данные, заголовки или начало шкалы: video=%+v audio=%+v", v.Packet.Info(), a.Packet.Info())
	}
	// got содержит наблюдаемый видеоформат из доставленной конфигурации.
	if got := v.VideoConfig.Info().Video; !got.FrameRate.Equal(core.FrameRate{Numerator: 30000, Denominator: 1001}) || got.Width != 640 || got.Height != 360 {
		t.Fatalf("timing_info не дошёл до Router: %+v", got)
	}
	// got содержит наблюдаемый аудиоформат из доставленного ASC.
	if got := a.AudioConfig.Info().Audio; got.SampleRateHz != 48000 || got.Channels != 2 || got.ChannelLayout != "stereo" || got.CodecProfile != "LC" {
		t.Fatalf("ASC не определил формат: %+v", got)
	}
	// Ожидания заданы в секундах: длительность видео 1001/30000, AAC — 1024/48000.
	if v.Packet.Info().DurationTicks != 8008 || a.Packet.Info().DurationTicks != 5120 || v.VideoConfig.Info().TimeBase != (media.TimeBase{Numerator: 1, Denominator: 240000}) || a.AudioConfig.Info().TimeBase != v.VideoConfig.Info().TimeBase {
		t.Fatal("потеряна точная общая шкала рационального видео и AAC")
	}
}

// Проверяет отсутствие выдачи до обеих конфигураций и независимого видео и отсев аудио раньше PTS
// этого видео.
func TestContractPublicationWaitsForCompleteIndependentAVStart(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Проверяет отсутствие выдачи до обоих заголовков и независимого A/V-старта, включая отбрасывание
		// аудио перед точкой показа видео.

		// p содержит тестовый адаптер публикации. sub содержит подписку на его пакеты.
		p, sub := newContractPublication(t)
		// config содержит AV1-конфигурацию для проверки ожидания полной пары.
		config := contractAV1Config(contractSDRSequence())
		// beforeReady содержит сообщения без полного независимого A/V-старта.
		beforeReady := bytes.Join([][]byte{
			contractAudio(0, []byte{0xde, 0xad}),
			contractVideo(0, contractAV1TU(config)),
			contractVideoConfig(config),
			contractVideo(10, contractAV1TU(config)),
			contractAudioConfig(),
			contractVideo(20, []byte{0x1a, 1, 0x30}),
			contractAudio(20, []byte{0xca, 0xfe}),
		}, nil)
		// err сообщает об отказе обработки допустимых сообщений до готовности.
		if err := contractConsume(t, p, beforeReady); err != nil {
			t.Fatal(err)
		}
		requireContractNoDelivery(t, sub)
		// err сообщает об отказе независимого видео и аудио до и на точке среза.
		if err := contractConsume(t, p, bytes.Join([][]byte{contractVideo(100, contractAV1TU(config)), contractAudio(99, []byte{0xfa}), contractAudio(100, []byte{0xfb})}, nil)); err != nil {
			t.Fatal(err)
		}
		// v содержит первый независимый видеокадр. a содержит аудио точно на его PTS после
		// отсева раннего пакета.
		v, a := delivery(t, sub), delivery(t, sub)
		if !v.Packet.Info().Keyframe || !acceptanceWireTime(v.Packet, 100) || !acceptanceWireTime(a.Packet, 100) || !bytes.Equal(a.Packet.Payload(), []byte{0xfb}) {
			t.Fatal("до независимого A/V-старта выпущены прежние пакеты")
		}
		requireContractNoDelivery(t, sub)
	})
}

// Проверяет одинаковый FPS и время A/V для обычных/Enhanced метаданных, Object/ECMAArray и наличия
// обёртки @setDataFrame.
func TestContractPublicationMetadataRepresentationsAgree(t *testing.T) {
	// enhanced выбирает обычные либо Enhanced видеометаданные.
	for _, enhanced := range []bool{false, true} {
		// wrapped выбирает наличие обёртки @setDataFrame.
		for _, wrapped := range []bool{false, true} {
			// ecma выбирает AMF ECMAArray либо Object.
			for _, ecma := range []bool{false, true} {
				t.Run(fmt.Sprintf("enhanced=%t/wrapped=%t/ecma=%t", enhanced, wrapped, ecma), func(t *testing.T) {
					// Для выбранных обёртки, вида AMF-объекта и формы RTMP проверяет одинаковые частоту кадров и метки
					// доставленных A/V-пакетов.

					// p содержит тестовый адаптер публикации. sub содержит подписку
					// на его пакеты.
					p, sub := newContractPublication(t)
					// object содержит выбранное представление метаданных с дробным
					// framerate.
					var object any = amf0.Object{{Key: "framerate", Value: float64(30000) / 1001}}
					if ecma {
						object = amf0.ECMAArray(object.(amf0.Object))
					}
					// data содержит onMetaData и необязательную обёртку.
					data := amf0.Data{"onMetaData", object}
					if wrapped {
						data = append(amf0.Data{"@setDataFrame"}, data...)
					}
					// config содержит av1C без собственного FPS, требующий
					// метаданных.
					config := testAV1Config()
					// raw содержит выбранные метаданные, конфигурации и
					// согласованный A/V-старт.
					raw := bytes.Join([][]byte{contractMetadata(t, enhanced, data), contractVideoConfig(config), contractAudioConfig(), contractVideo(100, testAV1TU()), contractAudio(100, []byte{1})}, nil)
					// err сообщает об отказе обработки выбранного представления
					// метаданных.
					if err := contractConsume(t, p, raw); err != nil {
						t.Fatal(err)
					}
					// v содержит видеодоставку. a содержит согласованную
					// аудиодоставку для сравнения разных форм AMF.
					v, a := delivery(t, sub), delivery(t, sub)
					if !v.VideoConfig.Info().Video.FrameRate.Equal(core.FrameRate{Numerator: 30000, Denominator: 1001}) || !acceptanceWireTime(v.Packet, 100) || !acceptanceWireTime(a.Packet, 100) {
						t.Fatal("форма AMF изменила наблюдаемую частоту или временную шкалу")
					}
				})
			}
		}
	}
}

// Проверяет отказ неверных типов и значений framerate без изменения ранее установленной частоты и без
// доставки пакетов.
func TestContractPublicationInvalidMetadataDoesNotPublishOrReplaceRate(t *testing.T) {
	// tc содержит недопустимое значение framerate.
	for _, tc := range []struct {
		// name обозначает нарушение частоты в имени подтеста.
		name string
		// value содержит недопустимое число либо значение неверного типа.
		value any
	}{
		{"zero", float64(0)}, {"negative", float64(-1)}, {"NaN", math.NaN()},
		{"positive-infinity", math.Inf(1)}, {"negative-infinity", math.Inf(-1)},
		{"string", "30"}, {"boolean", true}, {"null", nil},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Запускает изолированную проверку выбранного недопустимого значения частоты кадров в метаданных.

			synctest.Test(t, func(t *testing.T) {
				// Проверяет ErrCodec, сохранение ранее установленной частоты кадров и отсутствие выдачи после
				// недопустимых метаданных.

				// p содержит тестовый адаптер публикации. sub содержит подписку на его
				// пакеты.
				p, sub := newContractPublication(t)
				// valid содержит исходные допустимые метаданные 30 FPS.
				valid := contractMetadata(t, false, amf0.Data{"onMetaData", amf0.Object{{Key: "framerate", Value: float64(30)}}})
				// err сообщает об отказе установки исходной допустимой частоты.
				if err := contractConsume(t, p, valid); err != nil {
					t.Fatal(err)
				}
				// invalid содержит Enhanced метаданные с выбранным недопустимым
				// framerate.
				invalid := contractMetadata(t, true, amf0.Data{"onMetaData", amf0.Object{{Key: "framerate", Value: tc.value}}})
				// err содержит результат обработки недопустимой частоты.
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
	// data содержит неверный тип объекта либо попытку изменить установленный FPS.
	for _, data := range []amf0.Data{{"onMetaData", "invalid-object"}, {"onMetaData", amf0.Object{{Key: "framerate", Value: float64(24)}}}} {
		synctest.Test(t, func(t *testing.T) {
			// Проверяет отказ при неверном типе объекта метаданных либо попытке заменить установленную частоту
			// кадров.

			// p содержит тестовый адаптер публикации. sub содержит подписку на его пакеты.
			p, sub := newContractPublication(t)
			p.fps = core.FrameRate{Numerator: 30, Denominator: 1}
			// err содержит результат обработки неверного объекта или новой несовместимой
			// частоты.
			if err := contractConsume(t, p, contractMetadata(t, false, data)); !errors.Is(err, ErrCodec) {
				t.Fatalf("некорректное изменение или тип метаданных приняты: %v", err)
			}
			requireContractNoDelivery(t, sub)
		})
	}
}

// Проверяет сохранение крайней абсолютной метки и явный ErrDTSRegression при сбросе или переполнении
// метки каждой дорожки.
func TestContractPublicationTimestampWrapAndResetAreExplicitErrors(t *testing.T) {
	// track выбирает проверяемую видео- или аудиодорожку.
	for _, track := range []string{"video", "audio"} {
		// wrap выбирает сворачивание uint32 либо явный сброс метки.
		for _, wrap := range []bool{false, true} {
			t.Run(track+map[bool]string{false: "-reset", true: "-wrap"}[wrap], func(t *testing.T) {
				// Запускает проверку выбранной дорожки при сбросе либо сворачивании временной метки.

				synctest.Test(t, func(t *testing.T) {
					// Проверяет явный ErrDTSRegression после выбранного сброса либо сворачивания и сохранение исходных
					// меток уже принятых пакетов.

					// p содержит тестовый адаптер публикации. sub содержит подписку
					// на его пакеты.
					p, sub := newContractPublication(t)
					// config содержит исходную AV1-конфигурацию с timing_info.
					config := contractAV1Config(contractSDRSequence())
					// at задаёт начальную абсолютную метку: 100 мс либо MaxUint32.
					at := uint32(100)
					if wrap {
						at = math.MaxUint32
					}
					// bad содержит пакет выбранной дорожки с уменьшившейся либо
					// свернувшейся меткой.
					bad := contractAudio(0, []byte{0xcc})
					if track == "video" {
						bad = contractVideo(0, contractAV1TU(config))
					}
					if wrap {
						// Блок типа 2 наследует длину и тип, прибавляя 1 мс к 0xffffffff.
						// id выбирает поток блоков аудио или видео для
						// наследуемого продолжения.
						id := byte(4)
						if track == "video" {
							id = 6
						}
						// body содержит нагрузку пакета выбранной дорожки после
						// расширенного fmt0-заголовка.
						body := contractAudio(at, []byte{0xcc, 0xdd})[16:]
						if track == "video" {
							body = contractVideo(at, contractAV1TU(config))[16:]
						}
						bad = append([]byte{0x80 | id, 0, 0, 1}, body...)
					}
					// err содержит результат обработки стартовой пары и последующей
					// регрессии метки.
					if err := contractConsume(t, p, append(contractReady(config, at), bad...)); !errors.Is(err, media.ErrDTSRegression) {
						t.Fatalf("сброс или переполнение метки скрыты: %v", err)
					}
					// v содержит принятую видеодоставку с исходной крайней меткой. a
					// содержит соответствующую аудиодоставку.
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

// Проверяет остановку до выдачи следующего TU, если метаданные 30 FPS противоречат timing_info
// 30000/1001.
func TestContractPublicationContradictoryMetadataStopsBeforeNextVideo(t *testing.T) {
	synctest.Test(t, func(t *testing.T) {
		// Проверяет, что метаданные, противоречащие timing_info AV1, прерывают обработку до следующего
		// видеопакета.

		// p содержит тестовый адаптер публикации. sub содержит подписку на его пакеты.
		p, sub := newContractPublication(t)
		// config содержит AV1-конфигурацию с timing_info 30000/1001.
		config := contractAV1Config(contractSDRSequence())
		// err сообщает об отказе исходного независимого старта.
		if err := contractConsume(t, p, contractReady(config, 100)); err != nil {
			t.Fatal(err)
		}
		_ = delivery(t, sub)
		_ = delivery(t, sub)
		// metadata содержит противоречащий timing_info framerate=30.
		metadata := contractMetadata(t, false, amf0.Data{"onMetaData", amf0.Object{{Key: "framerate", Value: float64(30)}}})
		// err содержит результат обработки противоречащих метаданных до следующего видео.
		if err := contractConsume(t, p, append(metadata, contractVideo(133, contractAV1TU(config))...)); !errors.Is(err, ErrCodec) {
			t.Fatalf("метаданные 30 FPS затмили timing_info 30000/1001: %v", err)
		}
		requireContractNoDelivery(t, sub)
	})
}

// Проверяет сетевое завершение и отзыв текущих доставок после SequenceEnd, повреждённого TU или
// запрещённого AV1 FramesX.
func TestContractPublicationTerminalMessagesCloseActiveFeed(t *testing.T) {
	// tc содержит завершающее или повреждённое сообщение и ожидаемую причину остановки.
	for _, tc := range []struct {
		// name обозначает вид терминального сообщения в имени подтеста.
		name string
		// raw содержит сырые RTMP-байты выбранного сообщения.
		raw []byte
		// want задаёт ожидаемую категорию завершения приёма.
		want error
	}{
		{"audio-end", chunk0(4, 8, []byte{0x92, 'm', 'p', '4', 'a'}), ErrUnready},
		{"video-end", chunk0(6, 9, []byte{0x82, 'a', 'v', '0', '1'}), ErrUnready},
		{"malformed-TU", contractVideo(133, []byte{0x1a, 0}), ErrCodec},
		{"AV1-FramesX", contractMessage(6, 9, 133, append([]byte{0x93, 'a', 'v', '0', '1'}, testAV1TU()...)), ErrCodec},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Передаёт выбранное завершающее либо повреждённое сообщение через реальное локальное соединение и
			// проверяет ошибку, отзыв источника и закрытие соединения.

			// f содержит изолированные доменные данные, активную Session и маршрутизатор
			// входящего потока.
			f := newIngestFixture(t)
			// s содержит локальный входящий сервер. address содержит его TCP-адрес для
			// тестового клиента.
			s, address := runServer(t, f, nil)
			// sub содержит подписку на пакеты тестового маршрутизатора.
			sub := newSubscription(t, f.router)
			// client содержит авторизованного клиента. err сообщает об отказе подключения.
			client, err := connectClient(t, address, f.stream, f.stream.PublicationKey)
			if err != nil {
				t.Fatal(err)
			}
			sendReady(t, client, 100)
			// current содержит доставку действующего допуска до терминального сообщения.
			current := delivery(t, sub)
			_ = delivery(t, sub)
			writeWire(t, client, tc.raw)
			// err содержит причину завершения входящего соединения после сообщения.
			if err := serverResult(t, s); !errors.Is(err, tc.want) {
				t.Fatalf("завершающее сообщение не завершило приём требуемой ошибкой: %v", err)
			}
			if current.IsCurrent() {
				t.Fatal("завершённая последовательность осталась текущим источником")
			}
			// err содержит результат чтения клиента после завершения приёма.
			if _, err := client.Read(); err == nil {
				t.Fatal("соединение завершённой последовательности открыто")
			}
		})
	}
}
