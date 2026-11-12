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

// Возвращает тестовое описание AV1 2560×1440/60 с SourceEpoch=7, Revision=3 и шкалой 1/90000.
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

// Возвращает тестовое описание двухканального AAC-LC 48 кГц с SourceEpoch=7, Revision=9 и шкалой
// 1/48000.
func audioInfo() media.CodecConfigInfo {
	return media.CodecConfigInfo{
		Track: media.TrackAudio, SourceEpoch: 7, Revision: 9,
		TimeBase: media.TimeBase{Numerator: 1, Denominator: 48000},
		Audio:    core.AudioFormat{Codec: core.CodecAAC, CodecProfile: "LC", SampleRateHz: 48000, Channels: 2, ChannelLayout: "stereo"},
	}
}

// Создаёт конфигурацию с заданным описанием и трёхбайтовыми тестовыми заголовками; ошибка завершает
// тест.
func configFor(t *testing.T, info media.CodecConfigInfo) media.CodecConfig {
	t.Helper()
	// c содержит конфигурацию с переданным описанием и тестовыми заголовками. err сообщает об отказе
	// её создания.
	c, err := media.NewCodecConfig(info, []byte{1, 2, 3})
	if err != nil {
		t.Fatalf("NewCodecConfig: %v", err)
	}
	return c
}

// Проверяет доступность категории через errors.Is и наличие дополнительного контекста в тексте ошибки.
func requireCategory(t *testing.T, err, category error) {
	t.Helper()
	if !errors.Is(err, category) {
		t.Fatalf("ошибка = %v, нужна категория %v", err, category)
	}
	if err.Error() == category.Error() {
		t.Errorf("ошибка %v не содержит контекста", err)
	}
}

// Проверяет неизменность строковых значений video и audio для типов дорожек.
func TestTrackContractValues(t *testing.T) {
	if media.TrackVideo != "video" || media.TrackAudio != "audio" {
		t.Fatal("изменились строковые значения дорожек")
	}
}

// Проверяет положительность шкал и симметричное математическое равенство дробей, включая крайние
// uint32 и некорректные нулевые значения.
func TestTimeBaseValidationAndEquality(t *testing.T) {
	// cases перечисляет пары временных шкал и ожидаемые результаты их математического сравнения.
	cases := []struct {
		// name обозначает сценарий сравнения шкал в имени подтеста.
		name string
		// a содержит первую сравниваемую шкалу. b содержит вторую сравниваемую шкалу.
		a, b media.TimeBase
		// want задаёт ожидаемое математическое равенство шкал в обоих направлениях.
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
	// tc содержит выбранную пару шкал и ожидаемый результат сравнения.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет симметричность сравнения выбранной пары временных шкал с ожидаемым результатом.

			// got содержит результат сравнения первой шкалы со второй.
			if got := tc.a.Equal(tc.b); got != tc.want || tc.b.Equal(tc.a) != tc.want {
				t.Fatalf("Equal(%v, %v) = %v, нужно %v в обоих направлениях", tc.a, tc.b, got, tc.want)
			}
		})
	}
	// tb содержит недопустимую шкалу с нулевой частью дроби.
	for _, tb := range []media.TimeBase{{}, {0, 1}, {1, 0}} {
		requireCategory(t, tb.Validate(), media.ErrInvalidTimeBase)
	}
	// tb содержит допустимую положительную шкалу, включая предельные uint32.
	for _, tb := range []media.TimeBase{{1, 1}, {math.MaxUint32, math.MaxUint32}} {
		// err сообщает, отклонена ли допустимая положительная шкала.
		if err := tb.Validate(); err != nil {
			t.Fatalf("положительная TimeBase %v отклонена: %v", tb, err)
		}
	}
}

// Проверяет обязательность дорожки, SourceEpoch, Revision, шкалы и заголовков, отсутствие чужого
// формата и сохранение корректных полей с предельными идентичностями.
func TestCodecConfigValidation(t *testing.T) {
	// cases перечисляет отдельные повреждения допустимых описаний дорожек.
	cases := []struct {
		// name обозначает проверяемое повреждение в имени подтеста.
		name string
		// info содержит исходное допустимое описание нужной дорожки.
		info media.CodecConfigInfo
		// mutate вносит конкретное нарушение в копию описания дорожки.
		mutate func(*media.CodecConfigInfo)
	}{
		{"track_empty", videoInfo(), func(i *media.CodecConfigInfo) {
			// Удаляет обозначение дорожки из проверяемой конфигурации.
			i.Track = ""
		}},
		{"track_unknown", videoInfo(), func(i *media.CodecConfigInfo) {
			// Задаёт неподдержанную дорожку subtitle в проверяемой конфигурации.
			i.Track = "subtitle"
		}},
		{"epoch_zero", videoInfo(), func(i *media.CodecConfigInfo) {
			// Обнуляет SourceEpoch в проверяемой конфигурации.
			i.SourceEpoch = 0
		}},
		{"revision_zero", videoInfo(), func(i *media.CodecConfigInfo) {
			// Обнуляет Revision в проверяемой конфигурации.
			i.Revision = 0
		}},
		{"timebase_numerator", videoInfo(), func(i *media.CodecConfigInfo) {
			// Обнуляет числитель временной шкалы проверяемой конфигурации.
			i.TimeBase.Numerator = 0
		}},
		{"timebase_denominator", videoInfo(), func(i *media.CodecConfigInfo) {
			// Обнуляет знаменатель временной шкалы проверяемой конфигурации.
			i.TimeBase.Denominator = 0
		}},
		{"video_invalid", videoInfo(), func(i *media.CodecConfigInfo) {
			// Обнуляет ширину видео в проверяемой конфигурации.
			i.Video.Width = 0
		}},
		{"audio_invalid", audioInfo(), func(i *media.CodecConfigInfo) {
			// Обнуляет частоту аудио в проверяемой конфигурации.
			i.Audio.SampleRateHz = 0
		}},
		{"video_with_audio", videoInfo(), func(i *media.CodecConfigInfo) {
			// Добавляет полное описание аудио в видеоконфигурацию.
			i.Audio = audioInfo().Audio
		}},
		{"audio_with_video", audioInfo(), func(i *media.CodecConfigInfo) {
			// Добавляет полное описание видео в аудиоконфигурацию.
			i.Video = videoInfo().Video
		}},
		{"video_with_partial_audio", videoInfo(), func(i *media.CodecConfigInfo) {
			// Добавляет число аудиоканалов в видеоконфигурацию без остального описания аудио.
			i.Audio.Channels = 1
		}},
		{"audio_with_partial_video", audioInfo(), func(i *media.CodecConfigInfo) {
			// Добавляет уровень видео в аудиоконфигурацию без остального описания видео.
			i.Video.Level = "4"
		}},
	}
	// tc содержит выбранное повреждение конфигурации.
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			// Применяет выбранное повреждение к копии описания и проверяет отказ конструктора с ErrInvalidConfig.

			// i содержит копию исходного описания для применения выбранного повреждения.
			i := tc.info
			tc.mutate(&i)
			// err содержит результат создания конфигурации с повреждённым описанием.
			_, err := media.NewCodecConfig(i, []byte{1})
			requireCategory(t, err, media.ErrInvalidConfig)
		})
	}
	// headers содержит один из двух вариантов пустого буфера заголовков.
	for _, headers := range [][]byte{nil, {}} {
		// err содержит результат создания конфигурации без заголовков.
		_, err := media.NewCodecConfig(videoInfo(), headers)
		requireCategory(t, err, media.ErrInvalidConfig)
	}
	// info содержит допустимое описание видео или аудио для проверки предельных идентичностей.
	for _, info := range []media.CodecConfigInfo{videoInfo(), audioInfo()} {
		info.SourceEpoch = math.MaxUint64
		info.Revision = math.MaxUint64
		// c содержит конфигурацию с максимальными SourceEpoch и Revision.
		c := configFor(t, info)
		// err сообщает, отклонена ли конфигурация с допустимыми предельными идентичностями.
		if err := c.Validate(); err != nil {
			t.Fatalf("Корректная конфигурация отклонена: %v", err)
		}
		if c.Info() != info || c.StoredBytes() != 3 || !bytes.Equal(c.Headers(), []byte{1, 2, 3}) {
			t.Fatal("конструктор не сохранил описание и headers")
		}
	}
}

// Проверяет сохранение знаковых PTS/DTS до границ int64 и отказ для отрицательной длительности,
// ключевого аудио, пустой нагрузки и некорректной конфигурации.
func TestPacketValidationAndSignedTimestamps(t *testing.T) {
	// v содержит допустимую видеоконфигурацию для проверки меток пакетов.
	v := configFor(t, videoInfo())
	// a содержит допустимую аудиоконфигурацию для проверки аудиопакетов.
	a := configFor(t, audioInfo())
	// cases перечисляет допустимые знаковые метки и длительности вплоть до пределов int64.
	cases := []media.PacketInfo{
		{}, {PTS: -3000, DTS: -1500, DurationTicks: 1500},
		{PTS: math.MinInt64, DTS: math.MaxInt64, DurationTicks: math.MaxInt64, Keyframe: true},
		{PTS: math.MaxInt64, DTS: math.MinInt64, DurationTicks: 1},
	}
	// info содержит очередное допустимое сочетание меток, длительности и признака кадра.
	for _, info := range cases {
		// p содержит видеопакет с проверяемыми метками. err сообщает об отказе его создания.
		p, err := media.NewPacket(info, v, []byte{0x10})
		if err != nil {
			t.Fatalf("допустимые PTS/DTS %v отклонены: %v", info, err)
		}
		// err сообщает, отклонён ли уже созданный допустимый пакет.
		if err := p.Validate(); err != nil {
			t.Fatalf("Validate: %v", err)
		}
		if p.Info() != info || p.Config().Info() != v.Info() || !bytes.Equal(p.Payload(), []byte{0x10}) || p.StoredBytes() != 4 {
			t.Fatalf("пакет изменил значения: %v", info)
		}
	}
	// tc содержит выбранное нарушение структуры пакета.
	for _, tc := range []struct {
		// name обозначает нарушение в имени подтеста.
		name string
		// info содержит метки и признаки пакета для отрицательного сценария.
		info media.PacketInfo
		// config содержит конфигурацию дорожки для отрицательного сценария.
		config media.CodecConfig
		// payload содержит проверяемый буфер сжатых данных, в том числе nil или пустой.
		payload []byte
	}{
		{"duration_negative", media.PacketInfo{DurationTicks: -1}, v, []byte{1}},
		{"audio_keyframe", media.PacketInfo{Keyframe: true}, a, []byte{1}},
		{"config_zero", media.PacketInfo{}, media.CodecConfig{}, []byte{1}},
		{"nil_payload", media.PacketInfo{}, v, nil},
		{"empty_payload", media.PacketInfo{}, v, []byte{}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			// Проверяет отказ конструктора с ErrInvalidPacket для выбранных недопустимых свойств пакета,
			// конфигурации либо полезной нагрузки.

			// err содержит результат создания пакета с выбранным нарушением.
			_, err := media.NewPacket(tc.info, tc.config, tc.payload)
			requireCategory(t, err, media.ErrInvalidPacket)
		})
	}
	// p содержит допустимую аудиоединицу с отрицательными метками. err сообщает об отказе её
	// создания.
	p, err := media.NewPacket(media.PacketInfo{PTS: -1024, DTS: -1024, DurationTicks: 1024}, a, []byte{1})
	if err != nil || p.Info().Keyframe || p.Config().Info().Track != media.TrackAudio {
		t.Fatalf("корректный аудиопакет: %v, %v", p.Info(), err)
	}
}

// Проверяет безопасное чтение нулевых Packet и CodecConfig при обязательном отказе их валидации.
func TestZeroValuesHaveSafeGetters(t *testing.T) {
	// c содержит нулевую конфигурацию для проверки безопасных методов чтения.
	var c media.CodecConfig
	if c.Info() != (media.CodecConfigInfo{}) || len(c.Headers()) != 0 || c.StoredBytes() != 0 {
		t.Fatal("Методы чтения нулевой конфигурации вернули ненулевые данные")
	}
	requireCategory(t, c.Validate(), media.ErrInvalidConfig)
	// p содержит нулевой пакет для проверки безопасных методов чтения.
	var p media.Packet
	if p.Info() != (media.PacketInfo{}) || len(p.Payload()) != 0 || p.StoredBytes() != 0 || p.Config().Info() != (media.CodecConfigInfo{}) || len(p.Config().Headers()) != 0 {
		t.Fatal("Методы чтения нулевого пакета вернули ненулевые данные")
	}
	requireCategory(t, p.Validate(), media.ErrInvalidPacket)
}

// Проверяет, что изменение входных буферов, возвращённых копий и описаний не меняет пакет,
// конфигурацию и их копии по значению.
func TestPacketAndConfigOwnership(t *testing.T) {
	// headers содержит исходный изменяемый буфер заголовков для проверки копирования конструктором.
	headers := []byte{1, 2, 3}
	// info содержит исходное описание видеодорожки, изменяемое после создания конфигурации.
	info := videoInfo()
	// c содержит созданную конфигурацию с собственной копией заголовков. err сообщает об отказе её
	// создания.
	c, err := media.NewCodecConfig(info, headers)
	if err != nil {
		t.Fatal(err)
	}
	// copyOfConfig сохраняет копию конфигурации по значению для проверки её неизменности.
	copyOfConfig := c
	// payload содержит исходный изменяемый буфер сжатого пакета.
	payload := []byte{4, 5, 6, 7}
	// packetInfo содержит исходные метки пакета, изменяемые после его создания.
	packetInfo := media.PacketInfo{PTS: 2, DTS: -1, DurationTicks: 1, Keyframe: true}
	// p содержит пакет, который должен сохранить исходные метки и собственную копию нагрузки.
	p, err := media.NewPacket(packetInfo, c, payload)
	if err != nil {
		t.Fatal(err)
	}
	// copyOfPacket сохраняет копию пакета по значению для проверки неизменности.
	copyOfPacket := p
	// other содержит второй пакет с той же конфигурацией и отдельной полезной нагрузкой.
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
	// returnedConfigInfo содержит возвращённую по значению копию описания, изменение которой не
	// должно менять конфигурацию.
	returnedConfigInfo := c.Info()
	returnedConfigInfo.Revision = 99
	// returnedPacketInfo содержит возвращённую по значению копию меток, изменение которой не должно
	// менять пакет.
	returnedPacketInfo := p.Info()
	returnedPacketInfo.PTS = 99
	// got обозначает одну из конфигураций или её копий, проверяемых после изменения внешних буферов.
	for _, got := range []media.CodecConfig{c, copyOfConfig, p.Config(), copyOfPacket.Config(), other.Config()} {
		if got.Info() != videoInfo() || !bytes.Equal(got.Headers(), []byte{1, 2, 3}) || got.StoredBytes() != 3 {
			t.Fatal("Конфигурация изменилась через общий буфер или описание")
		}
	}
	// got обозначает исходный пакет или его копию, проверяемые после изменения внешних буферов.
	for _, got := range []media.Packet{p, copyOfPacket} {
		if got.Info() != (media.PacketInfo{PTS: 2, DTS: -1, DurationTicks: 1, Keyframe: true}) || !bytes.Equal(got.Payload(), []byte{4, 5, 6, 7}) || got.StoredBytes() != 7 {
			t.Fatal("пакет изменился через общий буфер или описание")
		}
	}
	if other.StoredBytes() != 4 || !bytes.Equal(other.Payload(), []byte{8}) {
		t.Fatal("Общая конфигурация учтена неверно во втором пакете")
	}
}

// Проверяет совместное чтение Packet и CodecConfig несколькими горутинами с изменением только
// полученных копий буферов.
func TestImmutableValuesSupportConcurrentReaders(t *testing.T) {
	// c содержит общую неизменяемую конфигурацию для параллельных читателей.
	c := configFor(t, videoInfo())
	// p содержит общий пакет для параллельного чтения. err сообщает об отказе его создания.
	p, err := media.NewPacket(media.PacketInfo{PTS: -1}, c, []byte{4, 5})
	if err != nil {
		t.Fatal(err)
	}
	// wg позволяет дождаться всех горутин, читающих общий пакет.
	var wg sync.WaitGroup
	for range 8 {
		wg.Go(func() {
			// Многократно читает общий неизменяемый пакет и изменяет полученные копии байтов, проверяя сохранность
			// исходного значения.

			for range 32 {
				// headers содержит отдельную копию заголовков для текущего читателя.
				// payload содержит отдельную копию нагрузки, которую читатель может
				// изменять.
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

// Проверяет загрузку синтетических конфигураций и четырёх пакетов из JSON по независимо заданным
// меткам, дорожкам и логическому весу.
func TestSyntheticPacketSequence(t *testing.T) {
	// data содержит байты JSON с синтетической последовательностью пакетов. err сообщает об отказе
	// чтения фикстуры.
	data, err := os.ReadFile("testdata/packet-sequence.json")
	if err != nil {
		t.Fatal(err)
	}
	// fixture принимает описания конфигураций и пакетов из синтетического JSON.
	var fixture struct {
		// Configs содержит именованные описания дорожек с заголовками и ожидаемым логическим
		// весом.
		Configs []struct {
			// Name задаёт ключ конфигурации для последующих ссылок пакетов.
			Name string
			// Info содержит описание дорожки, заданное в фикстуре.
			Info media.CodecConfigInfo
			// HeadersHex содержит заголовки дорожки в шестнадцатеричной записи.
			HeadersHex string `json:"headers_hex"`
			// StoredBytes задаёт ожидаемый логический вес конфигурации в байтах.
			StoredBytes uint64 `json:"stored_bytes"`
		}
		// Packets содержит последовательность пакетов со ссылками на именованные конфигурации.
		Packets []struct {
			// ConfigName выбирает именованную конфигурацию для пакета фикстуры.
			ConfigName string `json:"config_name"`
			// Info содержит ожидаемые метки, длительность и признак кадра пакета.
			Info media.PacketInfo
			// PayloadHex содержит сжатую нагрузку пакета в шестнадцатеричной записи.
			PayloadHex string `json:"payload_hex"`
			// StoredBytes задаёт ожидаемый логический вес пакета вместе с конфигурацией, в
			// байтах.
			StoredBytes uint64 `json:"stored_bytes"`
		}
	}
	// err сообщает о невозможности разобрать JSON синтетической последовательности.
	if err := json.Unmarshal(data, &fixture); err != nil {
		t.Fatal(err)
	}
	if len(fixture.Configs) != 2 || len(fixture.Packets) != 4 {
		t.Fatal("изменился состав синтетического набора")
	}
	// configs связывает имена из фикстуры с созданными неизменяемыми конфигурациями.
	configs := make(map[string]media.CodecConfig)
	// entry содержит очередное описание конфигурации из JSON.
	for _, entry := range fixture.Configs {
		// headers содержит декодированные заголовки дорожки. err сообщает о некорректной
		// шестнадцатеричной записи.
		headers, err := hex.DecodeString(entry.HeadersHex)
		if err != nil {
			t.Fatal(err)
		}
		// c содержит конфигурацию, созданную по очередному описанию фикстуры.
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
	// wantInfos задаёт независимые ожидаемые метки и признаки четырёх пакетов.
	wantInfos := []media.PacketInfo{{PTS: 0, DTS: -3000, DurationTicks: 1500, Keyframe: true}, {PTS: -3000, DTS: -1500, DurationTicks: 1500}, {PTS: 1500, DTS: 0}, {PTS: -1024, DTS: -1024, DurationTicks: 1024}}
	// wantWeights задаёт независимо ожидаемый логический вес каждого пакета в байтах.
	wantWeights := []uint64{7, 5, 6, 7}
	// wantTracks задаёт ожидаемую дорожку каждого пакета последовательности.
	wantTracks := []media.Track{media.TrackVideo, media.TrackVideo, media.TrackVideo, media.TrackAudio}
	// n обозначает позицию пакета в последовательности и массивах ожидаемых свойств. entry содержит
	// описание этого пакета из JSON.
	for n, entry := range fixture.Packets {
		// payload содержит декодированную сжатую нагрузку пакета. err сообщает о некорректной
		// шестнадцатеричной записи.
		payload, err := hex.DecodeString(entry.PayloadHex)
		if err != nil {
			t.Fatal(err)
		}
		// p содержит пакет, созданный с указанной в фикстуре конфигурацией и нагрузкой.
		p, err := media.NewPacket(entry.Info, configs[entry.ConfigName], payload)
		if err != nil {
			t.Fatalf("packet %d: %v", n, err)
		}
		if p.Info() != wantInfos[n] || p.Info() != entry.Info || p.StoredBytes() != wantWeights[n] || p.StoredBytes() != entry.StoredBytes || !bytes.Equal(p.Payload(), payload) || p.Config().Info() != configs[entry.ConfigName].Info() || p.Config().Info().Track != wantTracks[n] {
			t.Fatalf("packet %d не сохранил ожидаемые свойства", n)
		}
	}
}
