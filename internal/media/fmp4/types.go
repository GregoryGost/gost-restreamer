// Package fmp4 реализует ограниченный последовательный транспорт сжатых A/V-пакетов.
// Владелец Reader/Writer отвечает за отмену, дедлайны и закрытие соединения.
// Обращения к одному Muxer или Demuxer выполняются последовательно.
package fmp4

import (
	"errors"
	"fmt"
	"math"
	"math/big"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
)

var (
	// ErrUnsupportedCodec обозначает допустимую форму кодека вне реализованного последовательного порта.
	ErrUnsupportedCodec = errors.New("форма кодека не поддержана fMP4 портом")
	// ErrInvalid обозначает нарушение структуры или согласованности входных данных fMP4.
	ErrInvalid = errors.New("некорректный fMP4")
	// ErrLimit обозначает недопустимый бюджет либо превышение разрешённого размера или числа
	// элементов.
	ErrLimit = errors.New("превышен бюджет fMP4")
	// ErrUnsupportedTiming обозначает невозможность точно представить время в выбранной шкале или на
	// порте FFmpeg.
	ErrUnsupportedTiming = errors.New("время не представимо на выбранном порте")
	// ErrConfigChanged сообщает, что новая конфигурация дорожки требует отдельного транспорта вместо
	// продолжения текущего.
	ErrConfigChanged = errors.New("конфигурация fMP4 требует нового транспорта")
	// ErrMetadataUnavailable сообщает, что заголовков контейнера недостаточно для восстановления
	// полной конфигурации дорожки.
	ErrMetadataUnavailable = errors.New("метаданных fMP4 недостаточно для полной конфигурации")
)

// Limits ограничивает размеры блоков и число описаний единиц доступа.
// MaxBoxBytes, MaxInitBytes, MaxMoofBytes и MaxMdatBytes включают заголовки;
// MaxSampleBytes и MaxExtradataBytes учитывают только данные.
type Limits struct {
	// MaxBoxBytes ограничивает полный размер отдельного блока MP4; MaxInitBytes задаёт бюджет
	// инициализации, MaxMoofBytes — описания фрагмента, MaxMdatBytes — блока сжатых данных.
	MaxBoxBytes, MaxInitBytes, MaxMoofBytes, MaxMdatBytes uint64
	// MaxSampleBytes ограничивает полезную нагрузку одной единицы доступа; MaxExtradataBytes задаёт
	// предел объёма заголовков кодека без обрамления блока.
	MaxSampleBytes, MaxExtradataBytes uint64
	// MaxSamples ограничивает число описаний единиц доступа, принимаемых в одном фрагменте.
	MaxSamples uint32
}

// Validate проверяет положительные бюджеты не выше MaxInt32 и согласованность ограничений данных,
// инициализации и moof.
func (l Limits) Validate() error {
	// n обозначает очередной бюджет, который проверяется на ненулевое значение и предел MaxInt32.
	for _, n := range []uint64{l.MaxBoxBytes, l.MaxInitBytes, l.MaxMoofBytes, l.MaxMdatBytes, l.MaxSampleBytes, l.MaxExtradataBytes, uint64(l.MaxSamples)} {
		if n == 0 || n > uint64(math.MaxInt32) {
			return fmt.Errorf("%w: бюджеты должны быть положительными и не превышать MaxInt32", ErrLimit)
		}
	}
	if l.MaxSampleBytes > l.MaxMdatBytes || l.MaxExtradataBytes > l.MaxInitBytes || l.MaxMoofBytes > l.MaxBoxBytes {
		return fmt.Errorf("%w: несогласованные бюджеты", ErrLimit)
	}
	return nil
}

// OriginMapping добавляет один и тот же рациональный сдвиг обеим дорожкам.
// Список редактирования elst снимает его при чтении. Нулевой OffsetTicks означает тождество.
type OriginMapping struct {
	// OffsetTicks задаёт общий неотрицательный сдвиг дорожек в единицах TimeBase; ноль отключает
	// сдвиг.
	OffsetTicks int64
	// TimeBase задаёт длительность одного отсчёта OffsetTicks; при нулевом сдвиге её проверка не
	// требуется.
	TimeBase media.TimeBase
}

// Вычисляет точный неотрицательный сдвиг начала в заданной шкале; нулевой OffsetTicks не требует
// проверки TimeBase.
func (o OriginMapping) ticks(scale uint32) (int64, error) {
	if o.OffsetTicks == 0 {
		return 0, nil
	}
	if o.OffsetTicks < 0 {
		return 0, fmt.Errorf("%w: отрицательный сдвиг origin", ErrUnsupportedTiming)
	}
	return rescale(o.OffsetTicks, o.TimeBase, media.TimeBase{Numerator: 1, Denominator: scale})
}

// Init фиксирует пару конфигураций на весь срок одного транспорта.
// Headers содержат заголовки кодека без обрамления MP4: для нового аудио это LE OpusHead,
// STREAMINFO34, внутренний четырёхбайтовый дескриптор MP3 либо dac3.
type Init struct {
	// Video хранит конфигурацию видеодорожки, Audio — аудиодорожки; обе относятся к одной исходной
	// эпохе.
	Video, Audio media.CodecConfig
	// Origin задаёт общий сдвиг сохраняемых временных меток, который снимается списком
	// редактирования при чтении.
	Origin OriginMapping
}

// Validate проверяет обе конфигурации, общий SourceEpoch и представимость общей шкалы A/V.
func (i Init) Validate() error {
	// err сообщает о некорректной конфигурации видеодорожки инициализации.
	if err := i.Video.Validate(); err != nil {
		return err
	}
	// err сообщает о некорректной конфигурации аудиодорожки инициализации.
	if err := i.Audio.Validate(); err != nil {
		return err
	}
	// v содержит описание видеодорожки, a — аудиодорожки для сверки ролей, эпохи и совместимости
	// шкал.
	v, a := i.Video.Info(), i.Audio.Info()
	if v.Track != media.TrackVideo || a.Track != media.TrackAudio || v.SourceEpoch != a.SourceEpoch {
		return fmt.Errorf("%w: несогласованная пара A/V", ErrInvalid)
	}
	// err сообщает, что общая точная шкала двух дорожек не помещается в uint32.
	_, err := MovieTimeScale(v.TimeBase, a.TimeBase)
	return err
}

// MovieTimeScale возвращает точную общую шкалу без округления.
func MovieTimeScale(bases ...media.TimeBase) (uint32, error) {
	// result накапливает наименьшее общее кратное сокращённых знаменателей шкал дорожек.
	result := uint64(1)
	// b обозначает очередную рациональную шкалу, включаемую в общую шкалу фильма.
	for _, b := range bases {
		// err сообщает о недопустимости очередной рациональной шкалы.
		if err := b.Validate(); err != nil {
			return 0, err
		}
		// d содержит знаменатель шкалы после сокращения дроби на наибольший общий делитель.
		d := uint64(b.Denominator) / gcd(uint64(b.Numerator), uint64(b.Denominator))
		result = result / gcd(result, d) * d
		if result > math.MaxUint32 {
			return 0, fmt.Errorf("%w: общий timescale", ErrUnsupportedTiming)
		}
	}
	return uint32(result), nil
}

// Вычисляет наибольший общий делитель для сокращения шкал и вычисления их общего знаменателя.
func gcd(a, b uint64) uint64 {
	for b != 0 {
		a, b = b, a%b
	}
	return a
}

// Возвращает минимальную целочисленную шкалу, точно представляющую заданный TimeBase.
func trackScale(b media.TimeBase) (uint32, error) { return MovieTimeScale(b) }

// Переводит знаковое число отсчётов между положительными рациональными шкалами; округление и выход за
// int64 возвращают ErrUnsupportedTiming.
func rescale(v int64, from, to media.TimeBase) (int64, error) {
	if from.Validate() != nil || to.Validate() != nil {
		return 0, ErrUnsupportedTiming
	}
	// n хранит числитель преобразования временной метки в произвольной точности, включая её знак.
	n := big.NewInt(v)
	n.Mul(n, new(big.Int).SetUint64(uint64(from.Numerator)*uint64(to.Denominator)))
	// d хранит положительный знаменатель преобразования между исходной и целевой шкалами.
	d := new(big.Int).SetUint64(uint64(from.Denominator) * uint64(to.Numerator))
	// q получает целое частное преобразования, r — остаток, который должен быть нулевым для точного
	// отображения.
	q, r := new(big.Int), new(big.Int)
	q.QuoRem(n, d, r)
	if r.Sign() != 0 || !q.IsInt64() {
		return 0, fmt.Errorf("%w: нет точного рационального отображения", ErrUnsupportedTiming)
	}
	return q.Int64(), nil
}

// Складывает знаковые временные значения с проверкой границ int64 до сложения.
func add(a, b int64) (int64, error) {
	if b > 0 && a > math.MaxInt64-b || b < 0 && a < math.MinInt64-b {
		return 0, ErrUnsupportedTiming
	}
	return a + b, nil
}

// Вычисляет разность временных значений в big.Int, сохраняя результат за пределами int64.
func newDifference(a, b int64) *big.Int { return new(big.Int).Sub(big.NewInt(a), big.NewInt(b)) }

// TrackDescription содержит только сведения, полученные из заголовков MP4.
type TrackDescription struct {
	// SampleEntry сохраняет различие hvc1/hev1 и не переносит ERTMP FourCC правила на MP4.
	SampleEntry string
	// HDR содержит стандартные дескрипторы записи контейнера во время выполнения, без расширения UUID.
	HDR media.HDRMetadata
	// ActualVideo появляется только после измерения неполного нового формата восстановителем HEVC/VP9.
	ActualVideo core.VideoFormat
	// FirstSample и FirstSampleInfo дают восстановителю отдельную копию удержанного независимого AU и его времени.
	FirstSample     []byte
	FirstSampleInfo media.PacketInfo

	// ID содержит идентификатор дорожки из заголовков MP4.
	ID uint32
	// Track задаёт роль дорожки: сжатое видео либо аудио.
	Track media.Track
	// Codec задаёт кодек, определённый по записи описания сжатых данных.
	Codec core.Codec
	// TimeBase задаёт длительность одного отсчёта временной шкалы дорожки MP4.
	TimeBase media.TimeBase
	// Width и Height содержат объявленные в записи MP4 ширину и высоту видео в пикселях.
	Width, Height uint32
	// SampleRateHz содержит объявленную частоту дискретизации аудио в герцах.
	SampleRateHz uint32
	// Channels содержит объявленное число аудиоканалов записи MP4.
	Channels uint16
	// Headers содержит заголовки кодека без контейнерного обрамления; MP3 получает внутренний
	// дескриптор после проверки первой AU, Opus — производный Head0 после начального исключения.
	Headers []byte
	// Color содержит исходное тело необязательного colr/nclx; nil означает отсутствие дескриптора.
	Color []byte
}

// DemuxOptions задаёт новую идентичность и восстановление полного описания.
// Нулевой ResolveConfig использует ResolveCodecConfig; новые аудиозаписи дополнительно проверяют первую AU.
// Пользовательский ResolveConfig получает фактические Headers.
type DemuxOptions struct {
	// FreshSilentMP3 разрешён только нормализатору свежей собственной MP3-тишины.
	// Полная AU1152 остаётся обязательной, но её исходная контейнерная длительность от 1 до 1152
	// выдаётся без исправления. Вызывающий доказывает форму N+1, единственный неполный хвост,
	// полный период видео и PCM-ноль, затем удаляет первую AU и строит сетку N×1152 до публикации.
	// Обычное чтение, проигрывание, история и повторное открытие артефакта оставляют false.
	FreshSilentMP3 bool
	// FreshSilentOpus разрешён только нормализатору свежего результата собственного генератора тишины.
	// Полная CELT120 AU остаётся обязательной, но исходная duration от 0 до 120 samples выдаётся
	// без исправления. Вызывающий удерживает весь результат и доказывает полный период и PCM-ноль
	// перед созданием пакетов с положительной duration120 и публикацией. Обычные файлы, история,
	// проигрыватель и повторное открытие готового артефакта оставляют false.
	FreshSilentOpus bool
	// FreshHDRSource разрешает направленную коррекцию дескрипторов только у собственного свежего
	// Encoding5 с проверенным общим снимком источника. Обычные приём и копирование оставляет nil.
	// Снимок копируется при создании Demuxer и никогда не принимается за фактические сведения кодека.
	FreshHDRSource *media.HDRMetadata

	// Limits задаёт пределы размеров блоков, заголовков, единиц доступа и их числа при чтении.
	Limits Limits
	// SourceEpoch, VideoRevision и AudioRevision ненулевыми значениями заменяют эпоху и ревизии
	// восстановленных конфигураций; нули сохраняют сведения собственных метаданных или
	// ResolveConfig, если они имеются.
	SourceEpoch, VideoRevision, AudioRevision uint64
	// NormalizeHeaders при явном указании нормализует копию заголовков до восстановления формата
	// и строгой проверки контейнера. Вызывающий подтверждает происхождение свежего результата
	// собственного кодировщика; обычное чтение, приём и копирование оставляют nil. Поля записи контейнера,
	// сжатые данные и временные метки не меняются; результат ограничен MaxExtradataBytes.
	NormalizeHeaders func(TrackDescription) ([]byte, error)
	// ResolveConfig восстанавливает полный формат по копии фактического описания при отсутствии
	// собственных метаданных; nil выбирает ResolveCodecConfig. У новых аудиозаписей callback
	// получает завершённые фактические заголовки и копию первой удержанной AU.
	ResolveConfig func(TrackDescription) (media.CodecConfig, error)
}

// FFmpegTimingValidator проверяет ограничения CLI до сериализации пакета.
type FFmpegTimingValidator struct {
	// last хранит последний принятый DTS каждой дорожки в исходных отсчётах её конфигурации.
	last map[media.Track]int64
	// configs хранит принятые конфигурации дорожек для запрета их изменения внутри одного
	// транспорта.
	configs map[media.Track]media.CodecConfig
	// origin хранит сдвиг, с которым будет работать Muxer и который учитывается при дополнительной
	// проверке диапазона.
	origin OriginMapping
	// mapped включает проверку представимости DTS и PTS после применения origin, в том числе при
	// нулевом сдвиге.
	mapped bool
}

// NewFFmpegTimingValidator создаёт отдельное состояние конфигураций и последних DTS обеих дорожек для
// проверок CLI FFmpeg.
func NewFFmpegTimingValidator() *FFmpegTimingValidator {
	return &FFmpegTimingValidator{last: make(map[media.Track]int64), configs: make(map[media.Track]media.CodecConfig)}
}

// NewFFmpegTimingValidatorForOrigin дополнительно проверяет диапазон после сдвига Muxer.
func NewFFmpegTimingValidatorForOrigin(origin OriginMapping) *FFmpegTimingValidator {
	// v содержит новый независимый проверяющий объект, которому назначаются origin и проверка
	// сдвинутого диапазона.
	v := NewFFmpegTimingValidator()
	v.origin = origin
	v.mapped = true
	return v
}

// Validate проверяет представимость пакета на порте FFmpeg: PTS не раньше DTS, допустимые длительность
// и смещение, неизменность конфигурации и строгое продвижение DTS дорожки. При настроенном Origin
// дополнительно проверяет сдвинутый диапазон; состояние обновляется только после успешных проверок.
func (v *FFmpegTimingValidator) Validate(p media.Packet) error {
	// err сообщает о некорректной структуре входного сжатого пакета.
	if err := p.Validate(); err != nil {
		return err
	}
	// info содержит исходные временные метки и признак кадра проверяемой единицы доступа.
	info := p.Info()
	// c хранит конфигурацию дорожки, задающую кодек и исходную шкалу временных меток.
	c := p.Config()
	// track выбирает отдельное состояние последнего DTS и конфигурации для аудио либо видео.
	track := c.Info().Track
	if info.PTS < info.DTS {
		return fmt.Errorf("%w: PTS < DTS", ErrUnsupportedTiming)
	}
	// scale содержит целую шкалу, точно представляющую отсчёты конфигурации; err сообщает о
	// невозможности её построить.
	scale, err := trackScale(c.Info().TimeBase)
	if err != nil {
		return err
	}
	// base задаёт единицу временных полей контейнера как один отсчёт выбранной целой шкалы.
	base := media.TimeBase{Numerator: 1, Denominator: scale}
	// pts содержит время показа после точного перевода в целую шкалу контейнера.
	pts, err := rescale(info.PTS, c.Info().TimeBase, base)
	if err != nil {
		return err
	}
	// dts содержит время декодирования после точного перевода в целую шкалу контейнера.
	dts, err := rescale(info.DTS, c.Info().TimeBase, base)
	if err != nil {
		return err
	}
	// duration содержит длительность единицы доступа после точного перевода в целую шкалу
	// контейнера.
	duration, err := rescale(info.DurationTicks, c.Info().TimeBase, base)
	if err != nil {
		return err
	}
	// cts хранит разность PTS и DTS произвольной точности для проверки допустимого композиционного
	// смещения.
	cts := newDifference(pts, dts)
	if !cts.IsInt64() || cts.Int64() > math.MaxInt32 || duration > math.MaxUint32 {
		return ErrUnsupportedTiming
	}
	if track == media.TrackVideo && c.Info().Video.Codec == core.CodecAV1 && pts != dts {
		return ErrUnsupportedTiming
	}
	if v.mapped {
		// shift содержит точный общий сдвиг в шкале дорожки; err сообщает о непредставимости
		// этого сдвига.
		shift, err := v.origin.ticks(scale)
		if err != nil {
			return err
		}
		// storedDTS содержит время декодирования после сдвига, которое должно быть
		// неотрицательным и помещаться в int64.
		storedDTS, err := add(dts, shift)
		if err != nil || storedDTS < 0 {
			return ErrUnsupportedTiming
		}
		if _, err = add(pts, shift); err != nil {
			return err
		}
	}
	if v.configs == nil {
		v.configs = make(map[media.Track]media.CodecConfig)
		v.last = make(map[media.Track]int64)
	}
	// previous хранит ранее принятую конфигурацию этой дорожки; ok отмечает наличие значения для
	// сравнения.
	if previous, ok := v.configs[track]; ok && !sameConfig(previous, c) {
		return ErrConfigChanged
	}
	// previous содержит последний принятый DTS этой дорожки; ok показывает, требуется ли проверка
	// строгого продвижения.
	if previous, ok := v.last[track]; ok && info.DTS <= previous {
		return fmt.Errorf("%w: DTS не продвигается", ErrUnsupportedTiming)
	}
	v.last[track] = info.DTS
	v.configs[track] = c
	return nil
}
