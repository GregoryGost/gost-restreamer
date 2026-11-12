package ffmpeg

import (
	"bytes"
	"fmt"
	"math"
	"strconv"
	"strings"
	"sync"
	"time"
)

// ProgressConfig ограничивает память парсера до накопления строки и отчёта.
// Нулевое значение отключает разбор для низкоуровневых команд компонента.
type ProgressConfig struct {
	// MaxLineBytes исключает LF, MaxReportBytes включает разделители строк.
	MaxLineBytes   int
	MaxReportBytes int
	// MaxFields включает завершающее поле progress.
	MaxFields int
}

// Validate допускает полностью нулевую конфигурацию отключённого разбора либо требует положительные
// согласованные пределы строки, отчёта и числа полей.
func (c ProgressConfig) Validate() error {
	if c == (ProgressConfig{}) {
		return nil
	}
	if c.MaxLineBytes <= 0 || c.MaxReportBytes <= 0 || c.MaxFields <= 0 || c.MaxLineBytes > c.MaxReportBytes {
		return fmt.Errorf("%w: некорректные ограничения progress", ErrInvalidSupervisor)
	}
	return nil
}

// ProgressReport хранит принятые значения отчёта CLI FFmpeg: BitrateKbitsPerSecond измеряется в 1000
// бит/с, OutTimeUS — в микросекундах; Has-поля отличают доступное значение, включая ноль, от
// отсутствующего или N/A.
type ProgressReport struct {
	// SampledAt содержит время получения полного пригодного отчёта FFmpeg.
	SampledAt time.Time
	// Frame содержит сообщённое FFmpeg накопленное число кадров.
	Frame uint64
	// HasFrame отличает пригодное поле frame от отсутствующего или N/A.
	HasFrame bool
	// FPS хранит сообщённую FFmpeg частоту обработки видео в кадрах в секунду.
	FPS float64
	// HasFPS отличает пригодное поле fps от отсутствующего или N/A.
	HasFPS bool
	// TotalSizeBytes содержит сообщённый FFmpeg размер выхода в байтах.
	TotalSizeBytes uint64
	// HasTotalSize отличает пригодное поле total_size от отсутствующего или N/A.
	HasTotalSize bool
	// BitrateKbitsPerSecond содержит оценку битрейта FFmpeg в тысячах бит в секунду.
	BitrateKbitsPerSecond float64
	// HasBitrate отличает пригодное поле bitrate от отсутствующего или N/A.
	HasBitrate bool
	// OutTimeUS содержит время выхода из out_time_us либо запасного out_time_ms в микросекундах.
	OutTimeUS int64
	// HasOutTime показывает наличие пригодного времени выхода в отчёте.
	HasOutTime bool
	// DupFrames содержит сообщённое FFmpeg число продублированных кадров.
	DupFrames uint64
	// HasDupFrames отличает пригодное поле dup_frames от отсутствующего или N/A.
	HasDupFrames bool
	// DropFrames содержит сообщённое FFmpeg число пропущенных кадров.
	DropFrames uint64
	// HasDropFrames отличает пригодное поле drop_frames от отсутствующего или N/A.
	HasDropFrames bool
	// Speed содержит сообщённое FFmpeg отношение скорости обработки к реальному времени.
	Speed float64
	// HasSpeed отличает пригодное поле speed от отсутствующего или N/A.
	HasSpeed bool
	// End показывает, завершён ли отчёт разделителем progress=end.
	End bool
}

// ProgressSnapshot копируется по значению и не удерживает изменяемых ссылок.
// SampledAt обозначает время чтения источника, Report.SampledAt — последний успешный отчёт.
// Счётчики потерь накоплены за весь Supervisor, Attempt относится только к текущей попытке.
type ProgressSnapshot struct {
	SampledAt time.Time
	Attempt   uint64
	// Report содержит последний полный пригодный отчёт текущей попытки.
	Report ProgressReport
	// HasReport показывает, получен ли пригодный отчёт после начала текущей попытки.
	HasReport bool
	// DroppedReports считает отвергнутые или незавершённые отчёты за срок работы Supervisor.
	DroppedReports uint64
	// DroppedDiagnostics считает события диагностики, отвергнутые очередью за срок работы
	// Supervisor.
	DroppedDiagnostics uint64
}

// progressStore хранит последний отчёт текущей попытки и общий счётчик потерь под блокировкой.
type progressStore struct {
	// mu защищает номер попытки, последний отчёт и счётчик потерь.
	mu sync.Mutex
	// attempt задаёт текущую попытку и исключает публикацию запоздавших отчётов прежнего процесса.
	attempt uint64
	// report хранит последний полный пригодный отчёт текущей попытки.
	report ProgressReport
	// has отмечает наличие отчёта после последнего begin.
	has bool
	// dropped накапливает число потерянных отчётов с насыщением на MaxUint64.
	dropped uint64
}

// Начинает новую попытку с пустым последним отчётом, сохраняя накопленное число потерь Supervisor.
func (s *progressStore) begin(attempt uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempt, s.report, s.has = attempt, ProgressReport{}, false
}

// Сохраняет полный отчёт только при совпадении номера текущей попытки, исключая поздние отчёты
// прежнего процесса.
func (s *progressStore) publish(attempt uint64, report ProgressReport) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.attempt == attempt {
		s.report, s.has = report, true
	}
}

// Учитывает потерю отчёта только текущей попытки с насыщением счётчика на MaxUint64.
func (s *progressStore) drop(attempt uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.attempt == attempt && s.dropped < math.MaxUint64 {
		s.dropped++
	}
}

// Возвращает независимый снимок текущей попытки, последнего отчёта и накопленных потерь с временем
// чтения.
func (s *progressStore) snapshot() ProgressSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ProgressSnapshot{SampledAt: time.Now(), Attempt: s.attempt, Report: s.report, HasReport: s.has, DroppedReports: s.dropped}
}

// progressParser принадлежит одному stdout. write и finish выполняются под собственным мьютексом.
// Буфер имеет фиксированную ёмкость; неизвестные поля нигде не сохраняются.
type progressParser struct {
	// mu исключает одновременное изменение состояния разбора при write и finish.
	mu sync.Mutex
	// config задаёт пределы строки, отчёта и числа полей.
	config ProgressConfig
	// store принимает пригодные отчёты и учитывает их потери.
	store *progressStore
	// attempt связывает результаты разбора с конкретным запуском процесса.
	attempt uint64
	// line накапливает байты текущей строки без завершающего LF в пределах MaxLineBytes.
	line []byte
	// longLine отмечает превышение длины строки и запрещает разбор её остатка.
	longLine bool
	// used считает байты текущего отчёта вместе с разделителями строк до его отклонения.
	used int
	// fields считает поля текущего отчёта, включая завершающий progress.
	fields int
	// bad отмечает уже отвергнутый отчёт и предотвращает повторный учёт его потери.
	bad bool
	// pending отмечает наличие незавершённого отчёта для учёта потери при finish.
	pending bool
	// closed запрещает разбор данных после finish.
	closed bool
	// report накапливает известные поля до пригодного разделителя progress.
	report ProgressReport
	// seen содержит биты уже встреченных известных полей для отклонения повторов.
	seen uint16
	// outTimeMS хранит запасное поле out_time_ms, которое разборщик трактует как микросекунды.
	outTimeMS int64
	// hasTimeMS отмечает пригодное запасное время, используемое при отсутствии out_time_us.
	hasTimeMS bool
}

// Создаёт разборщик с фиксированной ёмкостью строки для одной попытки; нулевые ограничения отключают
// разбор и возвращают nil.
func newProgressParser(config ProgressConfig, store *progressStore, attempt uint64) *progressParser {
	if config == (ProgressConfig{}) {
		return nil
	}
	return &progressParser{config: config, store: store, attempt: attempt, line: make([]byte, 0, config.MaxLineBytes)}
}

// Единожды помечает текущий отчёт непригодным и учитывает его потерю до следующего корректного
// разделителя.
func (p *progressParser) reject() {
	if !p.bad {
		p.bad = true
		p.store.drop(p.attempt)
	}
}

// Разбирает произвольные фрагменты stdout под блокировкой, ограничивая строку и весь отчёт до
// накопления лишних данных; после finish данные пропускаются.
func (p *progressParser) write(data []byte) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
	// b содержит очередной байт фрагмента stdout для накопления строки и учёта размера отчёта.
	for _, b := range data {
		p.pending = true
		if !p.bad {
			if p.used == p.config.MaxReportBytes {
				p.reject()
			} else {
				p.used++
			}
		}
		if b == '\n' {
			if !p.longLine {
				p.consumeLine()
			}
			p.line, p.longLine = p.line[:0], false
			continue
		}
		if len(p.line) == p.config.MaxLineBytes {
			p.longLine = true
			p.reject()
		} else if !p.longLine {
			p.line = append(p.line, b)
		}
	}
}

// Проверяет строку key=value, число полей и разделитель progress; только полный пригодный отчёт
// публикуется, после разделителя начинается новый.
func (p *progressParser) consumeLine() {
	// line содержит накопленную строку после удаления необязательного завершающего CR.
	line := bytes.TrimSuffix(p.line, []byte{'\r'})
	// key содержит имя поля, value содержит его текст после первого знака равенства, ok показывает
	// наличие этого разделителя.
	key, value, ok := bytes.Cut(line, []byte{'='})
	// delimiter отмечает корректное поле progress=continue или progress=end, завершающее отчёт.
	delimiter := ok && bytes.Equal(key, []byte("progress")) && (bytes.Equal(value, []byte("continue")) || bytes.Equal(value, []byte("end")))
	if !p.bad {
		if p.fields == p.config.MaxFields {
			p.reject()
		} else {
			p.fields++
		}
	}
	if delimiter {
		if !p.bad {
			if !p.report.HasOutTime {
				p.report.OutTimeUS, p.report.HasOutTime = p.outTimeMS, p.hasTimeMS
			}
			p.report.End = bytes.Equal(value, []byte("end"))
			p.report.SampledAt = time.Now()
			p.store.publish(p.attempt, p.report)
		}
		p.reset()
		return
	}
	if p.bad {
		return
	}
	if !ok || len(key) == 0 || len(value) == 0 {
		p.reject()
		return
	}
	// b содержит очередной байт имени поля для проверки допустимого ASCII-набора.
	for _, b := range key {
		if (b < 'a' || b > 'z') && (b < 'A' || b > 'Z') && (b < '0' || b > '9') && b != '_' {
			p.reject()
			return
		}
	}
	if !p.field(string(key), strings.TrimSpace(string(value))) {
		p.reject()
	}
}

// Сбрасывает состояние текущего отчёта и признаков полей, сохраняя буфер строки и привязку попытки.
func (p *progressParser) reset() {
	p.used, p.fields, p.seen = 0, 0, 0
	p.bad, p.pending, p.hasTimeMS = false, false, false
	p.report, p.outTimeMS = ProgressReport{}, 0
}

// Завершает разбор единожды, учитывая неполный последний отчёт как потерю и освобождая буфер строки.
func (p *progressParser) finish() {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if !p.closed && p.pending {
		p.reject()
	}
	p.closed = true
	p.line = nil
}

// Разбирает известное поле с проверкой повторов и единиц; N/A оставляет значение отсутствующим,
// неизвестные поля не сохраняются.
func (p *progressParser) field(key, value string) bool {
	// bit задаёт маску известного поля в наборе уже встреченных полей.
	var bit uint16
	switch key {
	case "frame":
		bit = 1 << 0
	case "fps":
		bit = 1 << 1
	case "total_size":
		bit = 1 << 2
	case "bitrate":
		bit = 1 << 3
	case "out_time_us":
		bit = 1 << 4
	case "out_time_ms":
		bit = 1 << 5
	case "dup_frames":
		bit = 1 << 6
	case "drop_frames":
		bit = 1 << 7
	case "speed":
		bit = 1 << 8
	case "progress":
		return false
	default:
		return true
	}
	if p.seen&bit != 0 {
		return false
	}
	p.seen |= bit
	if value == "N/A" {
		return true
	}
	// r указывает на накапливаемый отчёт, в который записывается разобранное поле.
	r := &p.report
	switch key {
	case "frame":
		r.Frame, r.HasFrame = progressUint(value)
		return r.HasFrame
	case "fps":
		r.FPS, r.HasFPS = progressFloat(value)
		return r.HasFPS
	case "total_size":
		r.TotalSizeBytes, r.HasTotalSize = progressUint(value)
		return r.HasTotalSize
	case "bitrate":
		if !strings.HasSuffix(value, "kbits/s") {
			return false
		}
		r.BitrateKbitsPerSecond, r.HasBitrate = progressFloat(strings.TrimSpace(strings.TrimSuffix(value, "kbits/s")))
		return r.HasBitrate
	case "out_time_us":
		// v содержит разобранное out_time_us в микросекундах; err сообщает о неверном формате
		// или выходе за int64.
		v, err := strconv.ParseInt(value, 10, 64)
		r.OutTimeUS, r.HasOutTime = v, err == nil
		return err == nil
	case "out_time_ms":
		// v содержит запасное out_time_ms в микросекундах; err сообщает о неверном формате или
		// выходе за int64.
		v, err := strconv.ParseInt(value, 10, 64)
		p.outTimeMS, p.hasTimeMS = v, err == nil
		return err == nil
	case "dup_frames":
		r.DupFrames, r.HasDupFrames = progressUint(value)
		return r.HasDupFrames
	case "drop_frames":
		r.DropFrames, r.HasDropFrames = progressUint(value)
		return r.HasDropFrames
	case "speed":
		if !strings.HasSuffix(value, "x") {
			return false
		}
		r.Speed, r.HasSpeed = progressFloat(strings.TrimSpace(strings.TrimSuffix(value, "x")))
		return r.HasSpeed
	}
	return false
}

// Разбирает десятичный uint64 для поля progress и отдельно сообщает пригодность значения.
func progressUint(value string) (uint64, bool) {
	// v содержит разобранное неотрицательное целое поле progress; err сообщает о неверном формате
	// или выходе за uint64.
	v, err := strconv.ParseUint(value, 10, 64)
	return v, err == nil
}

// Разбирает конечное неотрицательное число progress, отвергая NaN, бесконечность и отрицательные
// значения.
func progressFloat(value string) (float64, bool) {
	// v содержит числовое поле progress в binary64; err сообщает об отказе числового разбора до
	// проверки конечности и знака.
	v, err := strconv.ParseFloat(value, 64)
	return v, err == nil && !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0
}
