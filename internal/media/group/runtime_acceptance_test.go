package group

import (
	"context"
	"fmt"
	"io"
	"net"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
	"github.com/GregoryGost/gost-restreamer/internal/media/fmp4"
	"github.com/GregoryGost/gost-restreamer/internal/storage"
)

// acceptanceBits последовательно упаковывает поля синтетических видеозаголовков в биты.
type acceptanceBits struct {
	// data хранит уже выделенные байты упаковываемого заголовка.
	data []byte
	// count считает число записанных битов и определяет позицию следующего бита в байте.
	count uint
}

// Записывает значение заданной разрядности от старшего бита к младшему, дополняя новый байт нулями.
func (b *acceptanceBits) put(v uint64, n uint) {
	// i задаёт оставшуюся разрядность поля при записи от старшего бита к младшему.
	for i := n; i > 0; i-- {
		if b.count%8 == 0 {
			b.data = append(b.data, 0)
		}
		b.data[len(b.data)-1] |= byte(v>>(i-1)&1) << (7 - b.count%8)
		b.count++
	}
}

// Кодирует беззнаковое значение Exp-Golomb для синтетических заголовков H.264.
func (b *acceptanceBits) ue(v uint32) {
	// n содержит увеличенное на один значение для беззнакового Exp-Golomb.
	n := uint64(v) + 1
	// w считает разрядность увеличенного значения и длину последующей кодовой части.
	w := uint(0)
	// x содержит сдвигаемый остаток числа при подсчёте его разрядности.
	for x := n; x != 0; x >>= 1 {
		w++
	}
	b.put(0, w-1)
	b.put(n, w)
}

// Заголовки записаны по синтаксису AV1 §5.5 и H.264 7.3.2.1.1/VUI.
// Срезы проверяют протокол и независимость; декодируемость устанавливает отдельная FFmpeg-проверка.
func acceptanceVideoHeaders(codec core.Codec, width, height, fps uint32) []byte {
	return acceptanceVideoHeadersAtRate(codec, width, height, core.FrameRate{Numerator: fps, Denominator: 1})
}

// Создаёт синтетический av1C либо avcC с заданными размерами и рациональной частотой; заголовки
// предназначены для проверки протокола и независимости.
func acceptanceVideoHeadersAtRate(codec core.Codec, width, height uint32, rate core.FrameRate) []byte {
	// b накапливает битовые поля синтетического AV1 sequence header либо H.264 SPS.
	b := &acceptanceBits{}
	if codec == core.CodecAV1 {
		b.put(0, 5)
		b.put(1, 1)
		b.put(uint64(rate.Denominator), 32)
		b.put(uint64(rate.Numerator), 32)
		b.put(1, 1)
		b.put(1, 1)
		b.put(0, 1)
		b.put(0, 1)
		b.put(0, 5)
		b.put(0, 12)
		b.put(8, 5)
		b.put(0, 1)
		b.put(11, 4)
		b.put(11, 4)
		b.put(uint64(width-1), 12)
		b.put(uint64(height-1), 12)
		b.put(0, 1)
		b.put(0, 3)
		b.put(0, 5)
		b.put(0, 1)
		b.put(0, 1)
		b.put(0, 3)
		b.put(0, 1)
		b.put(0, 1)
		b.put(1, 1)
		b.put(1, 8)
		b.put(1, 8)
		b.put(1, 8)
		b.put(0, 1)
		b.put(0, 2)
		b.put(0, 1)
		b.put(0, 1)
		b.put(1, 1)
		return append([]byte{0x81, 8, 0x0c, 0, 0x0a, byte(len(b.data))}, b.data...)
	}
	b.put(100, 8)
	b.put(0, 8)
	b.put(51, 8)
	b.ue(0)
	b.ue(1)
	b.ue(0)
	b.ue(0)
	b.put(0, 1)
	b.put(0, 1)
	b.ue(0)
	b.ue(0)
	b.ue(0)
	b.ue(1)
	b.put(0, 1)
	b.ue((width+15)/16 - 1)
	b.ue((height+15)/16 - 1)
	b.put(1, 1)
	b.put(1, 1)
	if height%16 != 0 {
		b.put(1, 1)
		b.ue(0)
		b.ue(0)
		b.ue(0)
		b.ue((16 - height%16) / 2)
	} else {
		b.put(0, 1)
	}
	b.put(1, 1)
	b.put(0, 1)
	b.put(0, 1)
	b.put(1, 1)
	b.put(5, 3)
	b.put(0, 1)
	b.put(1, 1)
	b.put(1, 8)
	b.put(1, 8)
	b.put(1, 8)
	b.put(0, 1)
	b.put(1, 1)
	b.put(uint64(rate.Denominator), 32)
	b.put(uint64(2*rate.Numerator), 32)
	b.put(1, 1)
	b.put(0, 4)
	b.put(1, 1)
	for b.count%8 != 0 {
		b.put(0, 1)
	}
	// sps собирает H.264 SPS с NAL-заголовком и вставками предотвращения эмуляции стартовых кодов.
	sps := []byte{0x67}
	// zeros считает последовательные нулевые байты для вставки 0x03 перед опасной
	// последовательностью.
	zeros := 0
	// v содержит очередной байт упакованного H.264 SPS.
	for _, v := range b.data {
		if zeros == 2 && v <= 3 {
			sps = append(sps, 3)
			zeros = 0
		}
		sps = append(sps, v)
		if v == 0 {
			zeros++
		} else {
			zeros = 0
		}
	}
	// pps задаёт контрольный H.264 PPS, добавляемый после синтетического SPS.
	pps := []byte{0x68, 0xce, 0x3c, 0x80}
	// result собирает avcC с длинами и содержимым SPS и PPS.
	result := []byte{1, 100, 0, 51, 0xff, 0xe1, byte(len(sps) >> 8), byte(len(sps))}
	result = append(result, sps...)
	result = append(result, 1, 0, byte(len(pps)))
	return append(result, pps...)
}

// Создаёт пару видео/AAC заданного профиля и эпохи для тестов с целочисленной частотой и AAC 48 kHz.
func acceptanceRuntimeConfigs(t testing.TB, p core.GroupProfile, epoch uint64) (media.CodecConfig, media.CodecConfig) {
	t.Helper()
	// v содержит видеоконфигурацию заданного профиля и эпохи; err сохраняет результаты создания
	// обеих дорожек.
	v, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackVideo, SourceEpoch: epoch, Revision: 1, TimeBase: media.TimeBase{Numerator: 1, Denominator: p.Format.Video.FrameRate.Numerator}, Video: p.Format.Video}, acceptanceVideoHeaders(p.Format.Video.Codec, p.Format.Video.Width, p.Format.Video.Height, p.Format.Video.FrameRate.Numerator))
	if err != nil {
		t.Fatal(err)
	}
	// a содержит AAC-конфигурацию той же эпохи с контрольным ASC 48000 Гц.
	a, err := media.NewCodecConfig(media.CodecConfigInfo{Track: media.TrackAudio, SourceEpoch: epoch, Revision: 1, TimeBase: media.TimeBase{Numerator: 1, Denominator: 48000}, Audio: p.Format.Audio}, []byte{0x11, 0x90})
	if err != nil {
		t.Fatal(err)
	}
	return v, a
}

// Создаёт профиль 60 FPS заданного размера: AV1 copy либо H.264 transcode с цветом bt709.
func acceptanceRuntimeProfile(codec core.Codec, width, height uint32) core.GroupProfile {
	// p содержит изменяемый контрольный профиль AV1 copy либо H.264 transcode заданных размеров.
	p := switchAcceptanceConfig().Profile
	p.Mode = core.ModeTranscode
	p.Format.Video.Codec = codec
	p.Format.Video.CodecProfile = "high"
	if codec == core.CodecAV1 {
		p.Mode = core.ModeCopy
		p.Format.Video.CodecProfile = "main"
	}
	p.Format.Video.Width = width
	p.Format.Video.Height = height
	p.Format.Video.FrameRate = core.FrameRate{Numerator: 60, Denominator: 1}
	p.Format.Video.Color = core.ColorInfo{Range: "limited", Primaries: "bt709", Transfer: "bt709", Matrix: "bt709"}
	p.VideoEncoding.GOPFrames = 60
	return p
}

// Возвращает конечные пределы блоков и сэмплов fMP4 для управляемых медиапроцессов.
func acceptanceRuntimeLimits() fmp4.Limits {
	return fmp4.Limits{MaxBoxBytes: 1 << 20, MaxInitBytes: 1 << 20, MaxMoofBytes: 1 << 16, MaxMdatBytes: 1 << 20, MaxSampleBytes: 1 << 18, MaxExtradataBytes: 1 << 12, MaxSamples: 100}
}

// Создаёт команду повторного запуска тестового бинарника как управляемого медиапроцесса с путями UDS и
// параметрами профиля.
func acceptanceHelperCommand(p core.GroupProfile, a ffmpeg.Attempt) (ffmpeg.Command, error) {
	// binary задаёт текущий тестовый бинарник для контролируемого медиапроцесса; err сообщает об
	// отказе определения пути.
	binary, err := os.Executable()
	if err != nil {
		return ffmpeg.Command{}, err
	}
	return ffmpeg.Command{Binary: binary, Args: []string{"-test.run=^TestGroupControlledMediaProcess$", "--", a.OutputPath, string(p.Format.Video.Codec), fmt.Sprint(p.Format.Video.Width), fmt.Sprint(p.Format.Video.Height), a.InputPath}}, nil
}

// При специальных аргументах работает дочерним медиапроцессом: подключает UDS, потребляет вход при
// наличии и выдаёт контрольный fMP4 60 FPS до разрыва соединения.
func TestGroupControlledMediaProcess(t *testing.T) {
	// position хранит положение разделителя дочерних аргументов; -1 означает обычный тестовый
	// запуск.
	position := -1
	// i обозначает позицию аргумента; a содержит проверяемую строку при поиске разделителя --.
	for i, a := range os.Args {
		if a == "--" {
			position = i
			break
		}
	}
	if position < 0 {
		return
	}
	// args содержит пять параметров контролируемого процесса после разделителя --.
	args := os.Args[position+1:]
	if len(args) != 5 {
		os.Exit(21)
	}
	// w содержит разобранную ширину видео; e сохраняет ошибки разбора параметров и открытия
	// выходного транспорта.
	w, e := strconv.ParseUint(args[2], 10, 32)
	if e != nil {
		os.Exit(22)
	}
	// h содержит разобранную высоту видео контрольного профиля.
	h, e := strconv.ParseUint(args[3], 10, 32)
	if e != nil {
		os.Exit(23)
	}
	// output содержит выходное UDS-соединение для выдачи контрольного fMP4.
	output, e := net.Dial("unix", args[0])
	if e != nil {
		os.Exit(24)
	}
	defer func() {
		// Закрывает выходное UDS-соединение тестового дочернего процесса при возврате, игнорируя ошибку.
		_ = output.Close()
	}()
	if args[4] != "" {
		// input содержит необязательное входное UDS-соединение; e сообщает об отказе его
		// открытия.
		input, e := net.Dial("unix", args[4])
		if e != nil {
			os.Exit(25)
		}
		defer func() {
			// Закрывает входное UDS-соединение тестового дочернего процесса при возврате, игнорируя ошибку.
			_ = input.Close()
		}()
		go func() {
			// Дочитывает вход тестового дочернего процесса в отдельной горутине, игнорируя результат io.Copy.
			_, _ = io.Copy(io.Discard, input)
		}()
	}
	// p содержит профиль, восстановленный из кодека и размеров аргументов процесса.
	p := acceptanceRuntimeProfile(core.Codec(args[1]), uint32(w), uint32(h))
	// v содержит контрольную видеоконфигурацию; a содержит AAC-конфигурацию выдаваемого fMP4.
	v, a := acceptanceRuntimeConfigs(t, p, 1)
	// mux записывает контрольные видео- и аудиопакеты в выходное UDS-соединение.
	mux, e := fmp4.NewMuxer(output, fmp4.Init{Video: v, Audio: a}, acceptanceRuntimeLimits())
	if e != nil {
		os.Exit(26)
	}
	// ticker задаёт темп выдачи видеокадров 60 кадров в секунду.
	ticker := time.NewTicker(time.Second / 60)
	defer ticker.Stop()
	// audio хранит следующую AAC-метку с шагом 1024 в шкале 1/48000.
	var audio int64
	// video задаёт очередной номер видеокадра в шкале 1/60.
	for video := int64(0); ; video++ {
		// payload содержит контрольный независимый H.264-срез либо AV1 TU выбранного профиля.
		payload := []byte{0, 0, 0, 3, 0x65, 0xb8, 0x40}
		if p.Format.Video.Codec == core.CodecAV1 {
			payload = append(v.Headers()[4:], 0x1a, 1, 0x10)
		}
		// packet содержит очередной пакет видео либо аудио; e сообщает об отказе создания перед
		// записью в muxer.
		packet, e := media.NewPacket(media.PacketInfo{PTS: video, DTS: video, DurationTicks: 1, Keyframe: true}, v, payload)
		if e != nil || mux.WritePacket(packet) != nil {
			return
		}
		for audio*60 < (video+1)*48000 {
			packet, e = media.NewPacket(media.PacketInfo{PTS: audio, DTS: audio, DurationTicks: 1024}, a, switchAcceptanceAACPayload())
			if e != nil || mux.WritePacket(packet) != nil {
				return
			}
			audio += 1024
		}
		<-ticker.C
	}
}

// acceptanceRuntimeFixture объединяет настоящий доменный запуск Session, закреплённые выходы и
// управляемые медиапорты теста.
type acceptanceRuntimeFixture struct {
	// service предоставляет доменные операции временной SQLite и общий реестр допусков.
	service *core.Service
	// parent содержит точное выполнение успешно запущенной контрольной Session.
	parent core.ExecutionFence
	// observer содержит действующего наблюдателя этой Session.
	observer *core.SessionObserver
	// outputs хранит четыре закреплённых выхода трёх профилей для проверки разделения групп.
	outputs []core.SessionOutput
	// config задаёт конечные бюджеты и управляемые порты выполнения групп.
	config RuntimeConfig
}

// Создаёт временную SQLite, Session с тремя профилями и четырьмя выходами, удержанные артефакты и
// наблюдателя; выделяет короткий каталог UDS с очисткой после теста.
func newRuntimeAcceptanceFixture(t *testing.T) acceptanceRuntimeFixture {
	t.Helper()
	// ctx задаёт общий контекст создания доменных данных контрольной Session.
	ctx := context.Background()
	// options задаёт конечные пределы временной SQLite, подготовок, наблюдений и резерваций выходов.
	options := storage.Options{PreparationLimits: core.PreparationLimits{MaxQueued: 3, MaxProfiles: 8}, OutputExecutionLimits: core.OutputExecutionLimits{MaxReservations: 128, MaxIdentifierBytes: 1024, MaxBytes: 4 << 20}, ObservationLimits: core.ObservationLimits{MaxSessions: 8, MaxGroups: 32, MaxOutputs: 64, MaxFFmpegWorkers: 32, MaxIdentifierBytes: 1024, MaxErrorBytes: 65536, MaxBytes: 4 << 20}, MaxOpenConns: 4, MaxIdleConns: 4, ConnMaxLifetime: time.Minute, ConnMaxIdleTime: time.Minute, BusyTimeout: 10 * time.Millisecond, RetryAttempts: 3, RetryTimeout: time.Second, RetryDelay: time.Millisecond}
	// store содержит временное SQLite-хранилище; err сохраняет результаты создания и подготовки
	// доменных сущностей.
	store, err := storage.Open(ctx, filepath.Join(t.TempDir(), "runtime.sqlite"), options)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает временное SQLite-хранилище сценария и сообщает ошибку через t.Error.

		// err сообщает об отказе закрытия временного хранилища после сценария.
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	// svc предоставляет доменные операции над временным хранилищем.
	svc, err := core.NewService(store, store.Incarnation())
	if err != nil {
		t.Fatal(err)
	}
	// asset содержит исходный Asset контрольной заглушки.
	asset, err := svc.CreateAsset(ctx, "Заглушка")
	if err != nil {
		t.Fatal(err)
	}
	// policy связывает этот Asset с контрольными сроками watchdog Stream.
	policy := core.FallbackPolicy{AssetID: asset.ID, Watchdog: switchAcceptanceConfig().Watchdog}
	// stream содержит контрольный Stream с постоянным ключом и выбранной политикой заглушки.
	stream, err := svc.CreateStream(ctx, core.Stream{Name: "Поток", PublicationKey: "runtime-key", Fallback: policy})
	if err != nil {
		t.Fatal(err)
	}
	// groups собирает три сохранённые группы разных полных профилей.
	var groups []core.TranscodeGroup
	// p содержит очередной контрольный профиль AV1 copy либо H.264 transcode.
	for _, p := range []core.GroupProfile{acceptanceRuntimeProfile(core.CodecAV1, 2560, 1440), acceptanceRuntimeProfile(core.CodecH264, 2560, 1440), acceptanceRuntimeProfile(core.CodecH264, 1920, 1080)} {
		// profile содержит сохранённый профиль текущей группы; err сообщает об отказе его
		// создания.
		profile, err := svc.CreateProfile(ctx, core.TranscodeProfile{Name: "Профиль", Profile: p})
		if err != nil {
			t.Fatal(err)
		}
		// g содержит сохранённую группу текущего профиля; err сообщает об отказе её создания.
		g, err := svc.CreateGroup(ctx, core.TranscodeGroup{Name: "Группа", StreamID: stream.ID, ProfileID: profile.ID})
		if err != nil {
			t.Fatal(err)
		}
		groups = append(groups, g)
		// count задаёт число назначений группы: два для H.264 1440p и одно для остальных.
		count := 1
		if p.Format.Video.Codec == core.CodecH264 && p.Format.Video.Width == 2560 {
			count = 2
		}
		// i обозначает номер создаваемого назначения текущей группы.
		for i := 0; i < count; i++ {
			if _, err = svc.CreateDestination(ctx, core.Destination{Name: "Назначение", StreamID: stream.ID, GroupID: g.ID, Platform: "custom", Endpoint: "rtmp://example.invalid/live", Enabled: true}); err != nil {
				t.Fatal(err)
			}
		}
	}
	// g содержит поколение подготовки всех требуемых профилей контрольного Stream.
	g, err := svc.ReservePreparation(ctx, core.PreparationRequest{AssetID: asset.ID, ExpectedRevision: asset.Revision, Kind: core.AssetImage, GeneratorVersion: "runtime-generator"})
	if err != nil {
		t.Fatal(err)
	}
	// pf содержит точный допуск исполнения зарезервированной подготовки.
	pf, err := svc.ClaimPreparation(ctx, g.ID)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.MarkPreparationIssued(ctx, pf); err != nil {
		t.Fatal(err)
	}
	// publication собирает готовые варианты всех требуемых профилей поколения.
	publication := core.GenerationPublication{SourceHash: "controlled-source", SizeBytes: 1, SourceFormat: core.SourceFormat{ImageFormat: "png"}}
	// r содержит очередной требуемый профиль с точной идентичностью варианта и путём.
	for _, r := range g.RequiredProfiles {
		publication.Variants = append(publication.Variants, core.FallbackVariant{ID: r.VariantID, GenerationID: g.ID, AssetID: g.AssetID, SourceHash: publication.SourceHash, Profile: r.Profile, VerifiedFormat: r.Profile.Format, GeneratorVersion: g.GeneratorVersion, ManagedPath: r.ManagedPath, SizeBytes: 1, ContentHash: "controlled-result"})
	}
	if _, err = svc.PublishPreparation(ctx, pf, publication); err != nil {
		t.Fatal(err)
	}
	if err = svc.AcknowledgePreparationCleanup(ctx, pf); err != nil {
		t.Fatal(err)
	}
	// request собирает запуск Session с отдельной подготовленной привязкой каждой группы.
	request := core.StartSessionRequest{RequestID: "runtime-parent", StreamID: stream.ID}
	// group содержит очередную сохранённую группу для выбора соответствующего готового варианта.
	for _, group := range groups {
		// profile содержит прочитанный профиль этой группы; err сообщает об отказе чтения.
		profile, err := svc.GetProfile(ctx, group.ProfileID)
		if err != nil {
			t.Fatal(err)
		}
		// r содержит кандидат готового варианта при сопоставлении полного профиля группы.
		for _, r := range g.RequiredProfiles {
			if r.Profile.Equal(profile.Profile) {
				request.Bindings = append(request.Bindings, core.PreparedBinding{GroupID: group.ID, GenerationID: g.ID, VariantID: r.VariantID, GeneratorVersion: g.GeneratorVersion})
			}
		}
	}
	// receipt содержит принятую квитанцию запуска Session с подготовленными привязками.
	receipt, err := svc.StartSession(ctx, request)
	if err != nil {
		t.Fatal(err)
	}
	// parent содержит точную идентичность исполнения команды запуска Session.
	parent, err := svc.Claim(ctx, receipt.RequestID)
	if err != nil {
		t.Fatal(err)
	}
	if err = svc.MarkIssued(ctx, parent); err != nil {
		t.Fatal(err)
	}
	if err = svc.Complete(ctx, parent, core.Completion{Success: true}); err != nil {
		t.Fatal(err)
	}
	// observer содержит наблюдателя успешно запущенной контрольной Session.
	observer, err := svc.OpenSessionObserver(ctx, parent)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает наблюдателя Session после сценария, игнорируя ошибку.
		_ = observer.Close(context.Background())
	})
	// outputs содержит страницу четырёх выходов с закреплёнными снимками.
	outputs, err := svc.ListOutputs(ctx, core.PageRequest{Query: core.ListQuery{Limit: 20, SessionID: parent.SessionID}})
	if err != nil {
		t.Fatal(err)
	}
	// parentDirectory задаёт короткий временный корень UDS с учётом ограничений Darwin.
	parentDirectory := os.TempDir()
	if runtime.GOOS == "darwin" {
		parentDirectory = "/tmp"
	}
	// base содержит выделенный каталог UDS контролируемых медиапроцессов.
	base, err := os.MkdirTemp(parentDirectory, "group-acceptance-")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Удаляет короткий временный корень UDS и сообщает ошибку через t.Error.

		// err сообщает об отказе удаления выделенного корня UDS после теста.
		if err := os.RemoveAll(base); err != nil {
			t.Error(err)
		}
	})
	// c содержит настройки групп с конечными сроками и файловым портом, который заменяет конкретный
	// сценарий.
	c := RuntimeConfig{Switcher: switchAcceptanceConfig(), Binary: "controlled", Container: acceptanceRuntimeLimits(), ResubscribeBackoff: time.Millisecond, ControlInterval: 5 * time.Millisecond, ShutdownTimeout: 2 * time.Second, FFmpeg: ffmpeg.Config{BaseDir: base, StartupTimeout: time.Second, VideoTimeout: time.Second, AudioTimeout: time.Second, WatchdogInterval: 10 * time.Millisecond, RestartBackoff: time.Millisecond, GracePeriod: 50 * time.Millisecond, KillTimeout: time.Second, ShutdownTimeout: 2 * time.Second, DiagnosticEvents: 32, DiagnosticBytes: 1 << 16, DiagnosticChunkBytes: 1024, MaxCommandBytes: 8192, MaxCommandArgs: 64}}
	c.OpenFile = func(context.Context, core.ArtifactManifest) (*os.File, error) {
		// Возвращает os.ErrNotExist вместо открытия файла до явной подстановки проигрывателя конкретным
		// сценарием.
		return nil, os.ErrNotExist
	}

	return acceptanceRuntimeFixture{svc, parent, observer, outputs.Items, c}
}

// Ожидает выполнения условия с миллисекундной проверкой и конечным сроком в три секунды.
func waitRuntimeAcceptance(t *testing.T, predicate func() bool) {
	t.Helper()
	// ctx ограничивает ожидание условия тремя секундами; cancel освобождает таймер.
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	// ticker задаёт миллисекундный период повторной проверки условия.
	ticker := time.NewTicker(time.Millisecond)
	defer ticker.Stop()
	for !predicate() {
		select {
		case <-ctx.Done():
			t.Fatal("условие runtime не выполнено до срока")
		case <-ticker.C:
		}
	}
}

// Проверяет общий результат двух выходов одной группы, сохранение её после первой ссылки и остановку
// последней ссылкой без отзыва Session.
func TestRegistrySharesGroupResultAndLastReferenceJoins(t *testing.T) {
	// f предоставляет Session с двумя назначениями общего H.264-профиля.
	f := newRuntimeAcceptanceFixture(t)
	// ctx задаёт контекст операций получения и освобождения общих ссылок.
	ctx := context.Background()
	// live содержит общий входной Router; err сохраняет результаты создания реестра и выдачи ссылок.
	live, err := media.NewRouter(switchAcceptanceConfig().Router)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// Закрывает LIVE Router после проверки общих ссылок, игнорируя ошибку.
		_ = live.Close()
	}()
	// attempts считает открытия файла, позволяя обнаружить лишнее выполнение одной общей группы.
	var attempts atomic.Int32
	// c задаёт файловый порт с подсчётом попыток и контролируемым os.ErrNotExist.
	c := f.config
	c.OpenFile = func(context.Context, core.ArtifactManifest) (*os.File, error) {
		// Считает попытки открытия файла и возвращает os.ErrNotExist, позволяя проверить единственное
		// выполнение общей группы.

		attempts.Add(1)
		return nil, os.ErrNotExist
	}
	// r содержит реестр разделяемых групп точного выполнения Session.
	r, err := NewRegistry(ctx, RegistryConfig{Core: f.service, Parent: f.parent, Observer: f.observer, Runtime: c, MaxGroups: 8, MaxReferences: 8})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает реестр общих групп с фоновым контекстом, игнорируя ошибку.
		_ = r.Close(context.Background())
	})
	// same собирает два выхода H.264 1440p с одинаковым полным профилем группы.
	var same []core.SessionOutput
	// o содержит очередной сохранённый выход для поиска общей группы.
	for _, o := range f.outputs {
		if o.Snapshot.Profile.Profile.Format.Video.Codec == core.CodecH264 && o.Snapshot.Profile.Profile.Format.Video.Width == 2560 {
			same = append(same, o)
		}
	}
	if len(same) != 2 {
		t.Fatal("нет двух назначений общей группы")
	}
	// a удерживает первую ссылку на общий H.264-результат.
	a, err := r.Acquire(ctx, same[0].Snapshot, live)
	if err != nil {
		t.Fatal(err)
	}
	// b удерживает вторую ссылку на тот же результат и сохраняет группу после закрытия первой.
	b, err := r.Acquire(ctx, same[1].Snapshot, live)
	if err != nil {
		t.Fatal(err)
	}
	if a.Group() != b.Group() || a.Router() != b.Router() {
		t.Fatal("два назначения получили разные общие результаты")
	}
	waitRuntimeAcceptance(t, func() bool {
		// Ожидает первую фактическую попытку открытия файла общей группы.
		return attempts.Load() > 0
	})
	if err = a.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-b.Group().Done():
		t.Fatal("первый ref остановил используемую группу")
	default:
	}
	if err = b.Close(); err != nil {
		t.Fatal(err)
	}
	select {
	case <-b.Group().Done():
	case <-time.After(3 * time.Second):
		t.Fatal("последний ref не остановил и не join группу")
	}
	if !f.observer.Valid() {
		t.Fatal("last ref отозвал parent Session")
	}
}

// Проверяет три независимых профиля, общий H.264-результат двух выходов, сохранение Router при замене
// LIVE и паузе и изоляцию остановки одной группы.
func TestGroupParallelProfilesKeepFallbackAndRoutersAcrossOverride(t *testing.T) {
	// f предоставляет три профиля и четыре выхода для параллельного медиасценария.
	f := newRuntimeAcceptanceFixture(t)
	// ctx ограничивает параллельный сценарий двенадцатью секундами; cancel освобождает таймер.
	ctx, cancel := context.WithTimeout(context.Background(), 12*time.Second)
	defer cancel()
	// live содержит общий Router входной AV1/AAC-публикации; err сохраняет результаты создания
	// реестра, подписок и команд управления.
	live, err := media.NewRouter(media.RouterLimits{MaxConsumers: 8, MaxRetainedBytes: 12 << 20, MaxConfigBytes: 1 << 12})
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// Закрывает LIVE Router после сценария параллельных профилей, игнорируя ошибку.
		_ = live.Close()
	}()
	// c содержит настройки групп с коротким сроком синхронизации и контролируемыми медиапроцессами.
	c := f.config
	c.Switcher.Queue.ResyncTimeout = 40 * time.Millisecond
	// files связывает временный дескриптор заглушки с его полным профилем до вызова построителя.
	var files sync.Map
	// transcodes считает физические построения H.264-перекодировщиков.
	var transcodes atomic.Int32
	// players считает физические построения проигрывателей трёх независимых групп.
	var players atomic.Int32
	// dir задаёт каталог временных файлов, передаваемых проигрывателям через ExtraFiles.
	dir := t.TempDir()
	c.OpenFile = func(_ context.Context, m core.ArtifactManifest) (*os.File, error) {
		// Создаёт независимый временный файл и запоминает его полный профиль для построителя тестового
		// проигрывателя.

		// file содержит новый дескриптор для конкретного манифеста; err сообщает об отказе его
		// создания.
		file, err := os.CreateTemp(dir, "held")
		if err == nil {
			files.Store(file, m.Profile)
		}
		return file, err
	}
	c.Player = func(_ context.Context, _ string, file *os.File, a ffmpeg.Attempt) (ffmpeg.Command, error) {
		// Извлекает профиль открытого файла, считает проигрыватели и строит тестовую команду, передавая файл
		// через ExtraFiles.

		// p содержит профиль, сохранённый для дескриптора; ok показывает наличие ожидаемой
		// привязки файла.
		p, ok := files.LoadAndDelete(file)
		if !ok {
			return ffmpeg.Command{}, core.ErrCorrupt
		}
		players.Add(1)
		// command содержит команду контролируемого проигрывателя; err сообщает об отказе её
		// построения.
		command, err := acceptanceHelperCommand(p.(core.GroupProfile), a)
		command.ExtraFiles = []*os.File{file}
		return command, err
	}
	c.Transcode = func(_ context.Context, n ffmpeg.NVIDIAConfig, a ffmpeg.Attempt) (ffmpeg.Command, error) {
		// Допускает только H.264, считает отдельных работников transcode и строит команду тестового процесса
		// по выходному профилю.

		if n.Profile.Format.Video.Codec != core.CodecH264 {
			return ffmpeg.Command{}, core.ErrInvalid
		}
		transcodes.Add(1)
		return acceptanceHelperCommand(n.Profile, a)
	}
	// r содержит общий реестр трёх независимых профилей Session.
	r, err := NewRegistry(ctx, RegistryConfig{Core: f.service, Parent: f.parent, Observer: f.observer, Runtime: c, MaxGroups: 8, MaxReferences: 8})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		// Закрывает реестр параллельных групп с фоновым контекстом, игнорируя ошибку.
		_ = r.Close(context.Background())
	})
	// refs собирает отдельные ссылки четырёх сохранённых выходов.
	var refs []*Reference
	// o содержит очередной выход, для которого запрашивается ссылка на группу.
	for _, o := range f.outputs {
		// ref содержит выданную ссылку; err сообщает об отказе её получения.
		ref, err := r.Acquire(ctx, o.Snapshot, live)
		if err != nil {
			t.Fatal(err)
		}
		refs = append(refs, ref)
	}
	// groups содержит множество фактически созданных экземпляров групп для проверки трёх полных
	// профилей.
	groups := map[*Group]bool{}
	// ref содержит очередную ссылку при сборе множества общих групп.
	for _, ref := range refs {
		groups[ref.Group()] = true
	}
	if len(groups) != 3 {
		t.Fatalf("три полных профиля создали %d групп", len(groups))
	}
	t.Cleanup(func() {
		// При провале теста снимает под блокировкой состояние источников, попыток и очередей каждой группы для
		// диагностики.

		if t.Failed() {
			// g содержит группу, чьё внутреннее состояние записывается при провале сценария.
			for g := range groups {
				g.switcher.mu.Lock()
				// liveFeed сохраняет состояние LIVE под блокировкой переключателя для
				// диагностического сообщения.
				liveFeed := g.switcher.feeds[0]
				// fallbackFeed сохраняет состояние заглушки под той же блокировкой для
				// диагностики.
				fallbackFeed := g.switcher.feeds[1]
				t.Logf("group=%s codec=%s width=%d status=%+v attempts=%d/%d live attempt=%d seen=%v dts=%v stable=%v rejected=%v failure=%v queue=%d fallback seen=%v failure=%v transcodes=%d players=%d", g.id, g.config.Switcher.Profile.Format.Video.Codec, g.config.Switcher.Profile.Format.Video.Width, g.switcher.status, g.attempts[0].Load(), g.attempts[1].Load(), liveFeed.attempt, liveFeed.seen, liveFeed.dts, liveFeed.stable, liveFeed.rejected, liveFeed.failure, len(liveFeed.queue), fallbackFeed.seen, fallbackFeed.failure, transcodes.Load(), players.Load())
				g.switcher.mu.Unlock()
			}
		}
	})
	waitRuntimeAcceptance(t, func() bool {
		// Ожидает доступный fallback всех групп и как минимум вторую LIVE-подписку после короткого тайм-аута
		// без OBS.

		// g содержит очередную группу при ожидании готовой заглушки и повторной LIVE-подписки до
		// OBS.
		for g := range groups {
			if !g.Status().Available || g.Status().Source != Fallback || g.attempts[0].Load() < 2 {
				return false
			}
		}
		return true
	})
	if transcodes.Load() != 0 || players.Load() != 3 {
		t.Fatalf("до OBS: transcode=%d players=%d", transcodes.Load(), players.Load())
	}
	// producerCancel отменяет текущего производителя LIVE при замене либо очистке.
	var producerCancel context.CancelFunc
	// producerDone подтверждает фактическое завершение текущего производителя LIVE.
	var producerDone <-chan struct{}
	// startProducer создаёт новую эпоху AV1/AAC-публикации и сохраняет её отмену и сигнал
	// завершения.
	startProducer := func() {
		// Создаёт новую LIVE-публикацию AV1/AAC и её отменяемую горутину, сохраняя текущие функции отмены и
		// подтверждения завершения.

		// source содержит новую эпоху LIVE; err сохраняет результаты её создания и установки
		// конфигураций.
		source, err := live.BeginSource()
		if err != nil {
			t.Fatal(err)
		}
		// p задаёт исходный AV1 copy-профиль 1440p60 производителя.
		p := acceptanceRuntimeProfile(core.CodecAV1, 2560, 1440)
		// v содержит видеоконфигурацию новой эпохи LIVE; a содержит её согласованную
		// AAC-конфигурацию.
		v, a := acceptanceRuntimeConfigs(t, p, source.Epoch())
		if err = source.UpdateConfigs(v, a); err != nil {
			t.Fatal(err)
		}
		// work задаёт отдельный срок текущего производителя; stop отменяет его без завершения
		// всего сценария.
		work, stop := context.WithCancel(ctx)
		producerCancel = stop
		// done закрывается после возврата текущего производителя LIVE.
		done := make(chan struct{})
		producerDone = done
		go func() {
			// Публикует условные AV1-кадры 60 FPS и полные AAC AU до конца каждого кадра, прекращаясь при отмене
			// либо ошибке и закрывая done.

			defer close(done)
			// ticker задаёт темп входной LIVE-публикации 60 кадров в секунду.
			ticker := time.NewTicker(time.Second / 60)
			defer ticker.Stop()
			// audio хранит следующую аудиометку в шкале 1/48000 с шагом 1024.
			var audio int64
			// video задаёт следующую видеометку в шкале 1/60.
			for video := int64(0); ; video++ {
				// packet содержит очередной входной AV1 либо AAC-пакет; e сообщает об
				// отказе его создания.
				packet, e := media.NewPacket(media.PacketInfo{PTS: video, DTS: video, DurationTicks: 1, Keyframe: true}, v, append(v.Headers()[4:], 0x1a, 1, 0x10))
				if e != nil || source.Publish(packet) != nil {
					return
				}
				for audio*60 < (video+1)*48000 {
					packet, e = media.NewPacket(media.PacketInfo{PTS: audio, DTS: audio, DurationTicks: 1024}, a, switchAcceptanceAACPayload())
					if e != nil || source.Publish(packet) != nil {
						return
					}
					audio += 1024
				}
				select {
				case <-work.Done():
					return
				case <-ticker.C:
				}
			}
		}()
	}
	startProducer()
	defer func() {
		// Отменяет текущего производителя LIVE и ждёт завершения его горутины.
		producerCancel()
		<-producerDone
	}()
	waitRuntimeAcceptance(t, func() bool {
		// Ожидает доступный LIVE во всех трёх группах после первой публикации.

		// g содержит очередную группу при ожидании восстановления LIVE первой публикации.
		for g := range groups {
			if !g.Status().Available || g.Status().Source != Live {
				return false
			}
		}
		return true
	})
	if transcodes.Load() != 2 {
		t.Fatalf("ожидались два независимых H.264 encoder и ни одного copy encoder: %d", transcodes.Load())
	}
	// shared собирает две ссылки общей H.264-группы 1440p.
	var shared []*Reference
	// independent сохраняет ссылку отдельной H.264-группы 1080p для проверки изоляции остановки.
	var independent *Reference
	// ref содержит очередную ссылку при выборе общей и независимой H.264-групп.
	for _, ref := range refs {
		if ref.Group().config.Switcher.Profile.Format.Video.Codec == core.CodecH264 {
			if ref.Group().config.Switcher.Profile.Format.Video.Width == 2560 {
				shared = append(shared, ref)
			} else {
				independent = ref
			}
		}
	}
	if len(shared) != 2 || shared[0].Router() != shared[1].Router() {
		t.Fatal("общая H.264 группа не разделяет результат")
	}
	// q задаёт пределы сравниваемых выходных подписок с увеличенным сроком синхронизации.
	q := c.Switcher.Queue
	q.ResyncTimeout = time.Second
	// a содержит первую подписку общего H.264-результата.
	a, err := shared[0].Router().Subscribe(q)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// Закрывает первую подписку общего выхода H.264 после проверки, игнорируя ошибку.
		_ = a.Close()
	}()
	// b содержит вторую независимую подписку того же общего результата.
	b, err := shared[1].Router().Subscribe(q)
	if err != nil {
		t.Fatal(err)
	}
	defer func() {
		// Закрывает вторую подписку общего выхода H.264 после проверки, игнорируя ошибку.
		_ = b.Close()
	}()
	// seen индексирует пакеты первой подписки по дорожке и DTS для сравнения с соседним
	// потребителем.
	seen := map[string]media.Packet{}
	for range 8 {
		// d содержит очередную выдачу первой подписки при сохранении контрольных пакетов.
		d := switchAcceptanceNext(t, a)
		seen[fmt.Sprintf("%s/%d", d.Packet.Config().Info().Track, d.Packet.Info().DTS)] = d.Packet
	}
	// matches считает совпавшие позиции двух независимых подписок общего результата.
	matches := 0
	for range 8 {
		// d содержит очередную выдачу второй подписки для поиска общего пакета.
		d := switchAcceptanceNext(t, b)
		// p содержит соответствующий пакет первой подписки; ok показывает наличие совпадающих
		// дорожки и DTS.
		if p, ok := seen[fmt.Sprintf("%s/%d", d.Packet.Config().Info().Track, d.Packet.Info().DTS)]; ok {
			if p.Info() != d.Packet.Info() || string(p.Payload()) != string(d.Packet.Payload()) {
				t.Fatal("consumers общей группы получили разные результаты")
			}
			matches++
		}
	}
	if matches == 0 {
		t.Fatal("не найден общий результат двух consumers")
	}
	// stable сохраняет постоянный Router каждой группы до замены LIVE и паузы.
	stable := map[*Group]*media.Router{}
	// g содержит очередную группу при сохранении её исходного Router.
	for g := range groups {
		stable[g] = g.Router()
	}
	producerCancel()
	<-producerDone
	startProducer()
	waitRuntimeAcceptance(t, func() bool {
		// Ожидает LIVE после замены производителя, сохраняя Router каждой группы и требуя новых попыток двух
		// transcode-работников.

		// g содержит очередную группу при ожидании LIVE после замены производителя без замены
		// Router.
		for g := range groups {
			if !g.Status().Available || g.Status().Source != Live || g.Router() != stable[g] {
				return false
			}
		}
		return transcodes.Load() >= 4
	})
	if _, err = f.service.PauseSession(ctx, core.SessionCommand{RequestID: "runtime-manual-pause", SessionID: f.parent.SessionID}); err != nil {
		t.Fatal(err)
	}
	waitRuntimeAcceptance(t, func() bool {
		// Ожидает применение ручной паузы: все группы доступны и выбрали fallback.

		// g содержит очередную группу при ожидании общей ручной паузы на заглушке.
		for g := range groups {
			if !g.Status().Paused || g.Status().Source != Fallback || !g.Status().Available {
				return false
			}
		}
		return true
	})
	if _, err = f.service.ResumeSession(ctx, core.SessionCommand{RequestID: "runtime-manual-resume", SessionID: f.parent.SessionID}); err != nil {
		t.Fatal(err)
	}
	waitRuntimeAcceptance(t, func() bool {
		// Ожидает применение возобновления: все группы доступны, выбрали LIVE и сняли признак Paused.

		// g содержит очередную группу при ожидании возобновления LIVE после ручной паузы.
		for g := range groups {
			if g.Status().Source != Live || g.Status().Paused || !g.Status().Available {
				return false
			}
		}
		return true
	})
	if independent == nil {
		t.Fatal("нет отдельной 1080p группы")
	}
	if err = independent.Group().Stop(ctx); err != nil {
		t.Fatal(err)
	}
	// g содержит очередную группу при проверке сохранности здоровых соседей остановленной
	// 1080p-группы.
	for g := range groups {
		if g != independent.Group() {
			if g.Router() != stable[g] || !g.Status().Available {
				t.Fatal("отказ одной группы изменил здорового соседа")
			}
			select {
			case <-g.Done():
				t.Fatal("отказ одной группы остановил другую")
			default:
			}
		}
	}
	_ = switchAcceptanceNext(t, a)
}
