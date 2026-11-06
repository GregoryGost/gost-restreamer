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

func (c ProgressConfig) Validate() error {
	if c == (ProgressConfig{}) {
		return nil
	}
	if c.MaxLineBytes <= 0 || c.MaxReportBytes <= 0 || c.MaxFields <= 0 || c.MaxLineBytes > c.MaxReportBytes {
		return fmt.Errorf("%w: некорректные ограничения progress", ErrInvalidSupervisor)
	}
	return nil
}

// ProgressReport содержит значения CLI FFmpeg; FPS и bitrate являются его накопительными оценками.
// BitrateKbitsPerSecond имеет единицу 1000 bit/s, OutTimeUS — микросекунды.
// Has-поля отличают N/A или отсутствие поля от измеренного нуля.
type ProgressReport struct {
	SampledAt             time.Time
	Frame                 uint64
	HasFrame              bool
	FPS                   float64
	HasFPS                bool
	TotalSizeBytes        uint64
	HasTotalSize          bool
	BitrateKbitsPerSecond float64
	HasBitrate            bool
	OutTimeUS             int64
	HasOutTime            bool
	DupFrames             uint64
	HasDupFrames          bool
	DropFrames            uint64
	HasDropFrames         bool
	Speed                 float64
	HasSpeed              bool
	End                   bool
}

// ProgressSnapshot копируется по значению и не удерживает изменяемых ссылок.
// SampledAt обозначает время чтения источника, Report.SampledAt — последний успешный отчёт.
// Счётчики потерь накоплены за весь Supervisor, Attempt относится только к текущей попытке.
type ProgressSnapshot struct {
	SampledAt          time.Time
	Attempt            uint64
	Report             ProgressReport
	HasReport          bool
	DroppedReports     uint64
	DroppedDiagnostics uint64
}

type progressStore struct {
	mu      sync.Mutex
	attempt uint64
	report  ProgressReport
	has     bool
	dropped uint64
}

func (s *progressStore) begin(attempt uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	s.attempt, s.report, s.has = attempt, ProgressReport{}, false
}

func (s *progressStore) publish(attempt uint64, report ProgressReport) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.attempt == attempt {
		s.report, s.has = report, true
	}
}

func (s *progressStore) drop(attempt uint64) {
	s.mu.Lock()
	defer s.mu.Unlock()
	if s.attempt == attempt && s.dropped < math.MaxUint64 {
		s.dropped++
	}
}

func (s *progressStore) snapshot() ProgressSnapshot {
	s.mu.Lock()
	defer s.mu.Unlock()
	return ProgressSnapshot{SampledAt: time.Now(), Attempt: s.attempt, Report: s.report, HasReport: s.has, DroppedReports: s.dropped}
}

// progressParser принадлежит одному stdout. Write и finish сериализованы собственным mutex.
// Буфер имеет фиксированную capacity; неизвестные поля нигде не сохраняются.
type progressParser struct {
	mu        sync.Mutex
	config    ProgressConfig
	store     *progressStore
	attempt   uint64
	line      []byte
	longLine  bool
	used      int
	fields    int
	bad       bool
	pending   bool
	closed    bool
	report    ProgressReport
	seen      uint16
	outTimeMS int64
	hasTimeMS bool
}

func newProgressParser(config ProgressConfig, store *progressStore, attempt uint64) *progressParser {
	if config == (ProgressConfig{}) {
		return nil
	}
	return &progressParser{config: config, store: store, attempt: attempt, line: make([]byte, 0, config.MaxLineBytes)}
}

func (p *progressParser) reject() {
	if !p.bad {
		p.bad = true
		p.store.drop(p.attempt)
	}
}

func (p *progressParser) write(data []byte) {
	if p == nil {
		return
	}
	p.mu.Lock()
	defer p.mu.Unlock()
	if p.closed {
		return
	}
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

func (p *progressParser) consumeLine() {
	line := bytes.TrimSuffix(p.line, []byte{'\r'})
	key, value, ok := bytes.Cut(line, []byte{'='})
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

func (p *progressParser) reset() {
	p.used, p.fields, p.seen = 0, 0, 0
	p.bad, p.pending, p.hasTimeMS = false, false, false
	p.report, p.outTimeMS = ProgressReport{}, 0
}

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

func (p *progressParser) field(key, value string) bool {
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
		v, err := strconv.ParseInt(value, 10, 64)
		r.OutTimeUS, r.HasOutTime = v, err == nil
		return err == nil
	case "out_time_ms":
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

func progressUint(value string) (uint64, bool) {
	v, err := strconv.ParseUint(value, 10, 64)
	return v, err == nil
}

func progressFloat(value string) (float64, bool) {
	v, err := strconv.ParseFloat(value, 64)
	return v, err == nil && !math.IsNaN(v) && !math.IsInf(v, 0) && v >= 0
}
