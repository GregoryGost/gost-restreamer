package ffmpeg

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"math"
	"net"
	"os"
	"slices"
	"strings"
	"testing"
	"time"
)

func parserConfig() ProgressConfig {
	return ProgressConfig{MaxLineBytes: 128, MaxReportBytes: 2048, MaxFields: 32}
}

func TestProgressFragmentsUnitsAvailabilityAndWholeReports(t *testing.T) {
	store := &progressStore{}
	store.begin(7)
	p := newProgressParser(parserConfig(), store, 7)
	input := "frame= 42\nfps=29.97\ntotal_size=1001\nbitrate= 800.8kbits/s\nout_time_ms=-500000\nout_time_us=-400000\ndup_frames=2\ndrop_frames=3\nspeed=1.25x\nstream_0_0_q=-1.0\nprogress=continue\n"
	before := time.Now()
	for _, b := range []byte(input) {
		p.write([]byte{b})
	}
	s := store.snapshot()
	r := s.Report
	if !s.HasReport || s.Attempt != 7 || s.DroppedReports != 0 || r.SampledAt.Before(before) || r.SampledAt.After(s.SampledAt) ||
		!r.HasFrame || r.Frame != 42 || !r.HasFPS || r.FPS != 29.97 || !r.HasTotalSize || r.TotalSizeBytes != 1001 ||
		!r.HasBitrate || r.BitrateKbitsPerSecond != 800.8 || !r.HasOutTime || r.OutTimeUS != -400000 ||
		!r.HasDupFrames || r.DupFrames != 2 || !r.HasDropFrames || r.DropFrames != 3 || !r.HasSpeed || r.Speed != 1.25 || r.End {
		t.Fatalf("неверные единицы или поля: %+v", s)
	}
	p.write([]byte("frame=N/A\nfps=N/A\ntotal_size=N/A\nbitrate=N/A\nout_time_us=N/A\nout_time_ms=-9000\ndup_frames=N/A\ndrop_frames=N/A\nspeed=N/A\nprogress=continue\nframe=0\nfps=0\nbitrate=0kbits/s\nspeed=0x\nprogress=end\n"))
	r = store.snapshot().Report
	if !r.End || !r.HasFrame || r.Frame != 0 || !r.HasFPS || r.FPS != 0 || !r.HasBitrate || r.BitrateKbitsPerSecond != 0 || !r.HasSpeed || r.Speed != 0 || r.HasOutTime || r.HasTotalSize || r.HasDupFrames || r.HasDropFrames {
		t.Fatalf("отчёт унаследовал предыдущие поля: %+v", r)
	}
	p.write([]byte("out_time_ms=-9000\nprogress=continue\n"))
	if r = store.snapshot().Report; !r.HasOutTime || r.OutTimeUS != -9000 {
		t.Fatalf("out_time_ms должен оставаться в микросекундах: %+v", r)
	}
	p.write([]byte("frame=N/A\nfps=N/A\ntotal_size=N/A\nbitrate=N/A\nout_time_us=N/A\ndup_frames=N/A\ndrop_frames=N/A\nspeed=N/A\nprogress=end\n"))
	r = store.snapshot().Report
	if !r.End || r.HasFrame || r.HasFPS || r.HasTotalSize || r.HasBitrate || r.HasOutTime || r.HasDupFrames || r.HasDropFrames || r.HasSpeed {
		t.Fatalf("N/A выдано за число: %+v", r)
	}
	p.finish()
	if store.snapshot().DroppedReports != 0 {
		t.Fatal("полный EOF потерял отчёт")
	}
}

func TestProgressRejectsMalformedWholeReportAndResynchronizes(t *testing.T) {
	for _, malformed := range []string{
		"frame=-1", "frame=18446744073709551616", "frame=no", "fps=NaN", "fps=+Inf", "fps=-1", "fps=1e1000",
		"total_size=-1", "bitrate=1bits/s", "bitrate=-1kbits/s", "bitrate=NaNkbits/s", "speed=Inf x", "speed=-1x", "speed=1",
		"out_time_us=9223372036854775808", "out_time_ms=-9223372036854775809", "dup_frames=-1", "drop_frames=x",
		"frame=1\nframe=2", "progress=other", "broken", "=1", "a b=1", "frame=", "\x00=1",
	} {
		t.Run(malformed, func(t *testing.T) {
			store := &progressStore{}
			store.begin(1)
			p := newProgressParser(parserConfig(), store, 1)
			p.write([]byte("frame=9\nprogress=continue\n"))
			previous := store.snapshot().Report
			p.write([]byte("unknown=allowed\n" + malformed + "\nprogress=continue\n"))
			if s := store.snapshot(); s.Report != previous || s.DroppedReports != 1 {
				t.Fatalf("часть ошибочного отчёта опубликована: %+v", s)
			}
			p.write([]byte("frame=3\nprogress=end\n"))
			p.finish()
			if s := store.snapshot(); s.Report.Frame != 3 || !s.Report.End || s.DroppedReports != 1 {
				t.Fatalf("не восстановлен разбор: %+v", s)
			}
		})
	}
}

func TestProgressBoundsAndPartialEOF(t *testing.T) {
	for _, tc := range []struct {
		name   string
		config ProgressConfig
		data   string
	}{
		{"строка", ProgressConfig{MaxLineBytes: 20, MaxReportBytes: 1024, MaxFields: 10}, "unknown=" + strings.Repeat("x", 10000) + "\n"},
		{"отчёт", ProgressConfig{MaxLineBytes: 20, MaxReportBytes: 30, MaxFields: 20}, strings.Repeat("unknown=x\n", 10)},
		{"поля", ProgressConfig{MaxLineBytes: 20, MaxReportBytes: 1024, MaxFields: 2}, "unknown=1\nunknown=2\nunknown=3\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			store := &progressStore{}
			store.begin(1)
			p := newProgressParser(tc.config, store, 1)
			p.write([]byte(tc.data))
			if len(p.line) > tc.config.MaxLineBytes || cap(p.line) != tc.config.MaxLineBytes || p.used > tc.config.MaxReportBytes || p.fields > tc.config.MaxFields {
				t.Fatal("превышен бюджет накопления")
			}
			p.write([]byte("progress=continue\nframe=4\nprogress=end\n"))
			p.finish()
			if s := store.snapshot(); s.DroppedReports != 1 || !s.HasReport || s.Report.Frame != 4 {
				t.Fatalf("потери или resync: %+v", s)
			}
		})
	}
	for _, partial := range []string{"frame=4", "frame=4\n", "frame=4\nprogress=end"} {
		store := &progressStore{}
		store.begin(1)
		p := newProgressParser(parserConfig(), store, 1)
		p.write([]byte(partial))
		p.finish()
		p.finish()
		p.write([]byte("\nprogress=end\n"))
		if s := store.snapshot(); s.HasReport || s.DroppedReports != 1 {
			t.Fatalf("EOF опубликовал частичный отчёт %q: %+v", partial, s)
		}
	}
}

func TestProgressExactLimitsSignedRangeAttemptAndCopySafety(t *testing.T) {
	store := &progressStore{}
	store.begin(1)
	data := "out_time_us=-9223372036854775808\nframe=18446744073709551615\nprogress=continue\n"
	p := newProgressParser(ProgressConfig{MaxLineBytes: 32, MaxReportBytes: len(data), MaxFields: 3}, store, 1)
	p.write([]byte(data))
	s := store.snapshot()
	if !s.HasReport || s.DroppedReports != 0 || s.Report.OutTimeUS != math.MinInt64 || s.Report.Frame != math.MaxUint64 {
		t.Fatalf("точная граница: %+v", s)
	}
	s.Report.Frame = 0
	if store.snapshot().Report.Frame != math.MaxUint64 {
		t.Fatal("снимок изменил источник")
	}
	store.begin(2)
	if s := store.snapshot(); s.HasReport || s.Attempt != 2 || s.Report != (ProgressReport{}) {
		t.Fatalf("новая попытка унаследовала отчёт: %+v", s)
	}
	p.write([]byte("frame=99\nprogress=end\n"))
	p.write([]byte("broken\n"))
	p.finish()
	if s := store.snapshot(); s.HasReport || s.DroppedReports != 0 {
		t.Fatalf("старая попытка изменила новую: %+v", s)
	}
	p = newProgressParser(parserConfig(), store, 2)
	p.write([]byte("frame=2\nprogress=end\n"))
	if s := store.snapshot(); !s.HasReport || s.Report.Frame != 2 {
		t.Fatal(s)
	}
	store.dropped = math.MaxUint64
	store.drop(2)
	if store.snapshot().DroppedReports != math.MaxUint64 {
		t.Fatal("переполнение счётчика потерь")
	}
}

func TestProgressConfigurationAndDisabledParser(t *testing.T) {
	for _, c := range []ProgressConfig{{MaxLineBytes: -1}, {MaxLineBytes: 1}, {MaxLineBytes: 2, MaxReportBytes: 1, MaxFields: 1}, {MaxLineBytes: 1, MaxReportBytes: 1}} {
		if !errors.Is(c.Validate(), ErrInvalidSupervisor) {
			t.Fatalf("принят неполный бюджет: %+v", c)
		}
		cfg := supervisorConfig(t)
		cfg.Progress = c
		if _, err := New(cfg); !errors.Is(err, ErrInvalidSupervisor) {
			t.Fatal(err)
		}
	}
	if err := (ProgressConfig{}).Validate(); err != nil {
		t.Fatal(err)
	}
	p := newProgressParser(ProgressConfig{}, nil, 1)
	p.write([]byte("frame=1\nprogress=end\n"))
	p.finish()
}

func TestStandardCommandsRequestProgressOnStdout(t *testing.T) {
	nvidia, err := BuildNVIDIACommand(nvidiaCommandConfig(), Attempt{InputPath: "/tmp/in", OutputPath: "/tmp/out"})
	if err != nil {
		t.Fatal(err)
	}
	f, err := os.CreateTemp(t.TempDir(), "media")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = f.Close() })
	player, err := BuildFileCommand("ffmpeg", f, Attempt{OutputPath: "/tmp/out"})
	if err != nil {
		t.Fatal(err)
	}
	for _, command := range []Command{nvidia, player} {
		for _, pair := range [][2]string{{"-progress", "pipe:1"}, {"-stats_period", "2"}} {
			i := slices.Index(command.Args, pair[0])
			if i < 0 || i+1 >= len(command.Args) || command.Args[i+1] != pair[1] {
				t.Fatalf("команда потеряла progress %v: %v", pair, command.Args)
			}
		}
		if !slices.Contains(command.Args, "-nostats") {
			t.Fatal("не отключена строка статистики stderr", command.Args)
		}
	}
}

func TestRejectedStartDiagnosticDrainerFinishesAfterStop(t *testing.T) {
	s := newTestSupervisor(t, supervisorConfig(t))
	drained := make(chan error, 1)
	go func() {
		_, err := s.NextDiagnostic(context.Background())
		drained <- err
	}()
	if err := s.Start(context.Background(), Worker{}); !errors.Is(err, ErrInvalidSupervisor) {
		t.Fatal(err)
	}
	if err := s.Stop(context.Background()); err != nil {
		t.Fatal(err)
	}
	select {
	case err := <-drained:
		if !errors.Is(err, io.EOF) {
			t.Fatal(err)
		}
	case <-time.After(time.Second):
		t.Fatal("drainer не получил EOF после отказа Start и Stop")
	}
}

const nativeProgressBytes = "frame=12\nfps=6.0\ntotal_size=N/A\nbitrate=N/A\nout_time_us=-2500\nspeed=1.0x\nprogress=end\n"

// TestProgressNativeChild проверяет stdout и FD 3 через настоящий processowner/helper.
func TestProgressNativeChild(t *testing.T) {
	for i, arg := range os.Args {
		if arg != "native-progress" {
			continue
		}
		f := os.NewFile(3, "progress-input")
		data, err := io.ReadAll(f)
		if err != nil || string(data) != "progress-fixture" {
			os.Exit(30)
		}
		for _, part := range []string{nativeProgressBytes[:3], nativeProgressBytes[3:13], nativeProgressBytes[13:]} {
			if _, err := io.WriteString(os.Stdout, part); err != nil {
				os.Exit(31)
			}
		}
		if _, err := os.Stderr.Write([]byte{0, 255, '\n'}); err != nil {
			os.Exit(32)
		}
		out, err := net.Dial("unix", os.Args[i+1])
		if err != nil {
			os.Exit(33)
		}
		if _, err = out.Write([]byte("VA")); err != nil {
			os.Exit(34)
		}
		var reply [1]byte
		if _, err = io.ReadFull(out, reply[:]); err != nil || reply[0] != 'Q' {
			os.Exit(35)
		}
		_ = out.Close()
		os.Exit(0)
	}
}

func TestManagedProgressKeepsFullDiagnosticsAndNativeCompletion(t *testing.T) {
	registry := managedRegistry(t)
	ctx, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	scope, err := registry.Create(ctx, "progress-session")
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = scope.Close() })
	f, err := os.CreateTemp(t.TempDir(), "progress")
	if err != nil {
		t.Fatal(err)
	}
	if _, err = f.WriteString("progress-fixture"); err != nil {
		t.Fatal(err)
	}
	config := supervisorConfig(t)
	config.Progress = parserConfig()
	config.DiagnosticEvents = 64
	config.StartupTimeout, config.VideoTimeout, config.AudioTimeout = 5*time.Second, 5*time.Second, 5*time.Second
	supervisor := newTestSupervisor(t, config)
	drained := make(chan []Diagnostic, 1)
	go func() {
		var records []Diagnostic
		for {
			d, err := supervisor.NextDiagnostic(context.Background())
			if err != nil {
				break
			}
			records = append(records, d)
		}
		drained <- records
	}()
	binary, err := os.Executable()
	if err != nil {
		t.Fatal(err)
	}
	ready, release := make(chan struct{}), make(chan struct{})
	t.Cleanup(func() {
		select {
		case <-release:
		default:
			close(release)
		}
	})
	worker := Worker{SessionID: "session", GroupID: "group", Owner: newOwnedPermit(), ProcessScope: scope, Topology: FileInput,
		Command: func(_ context.Context, a Attempt) (Command, error) {
			return Command{Binary: binary, Args: []string{"-test.run=^TestProgressNativeChild$", "--", "native-progress", a.OutputPath}, ExtraFiles: []*os.File{f}}, nil
		},
		Output: func(ctx context.Context, conn net.Conn, progress Progress) error {
			var marker [2]byte
			if _, err := io.ReadFull(conn, marker[:]); err != nil {
				return err
			}
			if string(marker[:]) != "VA" || !progress(Video, 1) || !progress(Audio, 1) {
				return fmt.Errorf("неверные контрольные медиаданные %q", marker)
			}
			close(ready)
			select {
			case <-release:
			case <-ctx.Done():
				return ctx.Err()
			}
			if _, err := conn.Write([]byte{'Q'}); err != nil {
				return err
			}
			_, err := io.Copy(io.Discard, conn)
			return err
		},
	}
	if err = supervisor.Start(ctx, worker); err != nil {
		t.Fatal(err)
	}
	select {
	case <-ready:
	case <-supervisor.Done():
		t.Fatalf("worker завершился до контрольных медиаданных: %+v", waitResult(t, supervisor))
	case <-ctx.Done():
		t.Fatal(ctx.Err())
	}
	for !supervisor.Snapshot().HasReport {
		select {
		case <-ctx.Done():
			t.Fatal("stdout не прочитан", ctx.Err())
		case <-time.After(time.Millisecond):
		}
	}
	if s := supervisor.Snapshot(); s.Attempt != 1 || !s.Report.End || s.Report.Frame != 12 || s.Report.OutTimeUS != -2500 || s.Report.HasBitrate {
		t.Fatalf("снимок native stdout: %+v", s)
	}
	select {
	case <-supervisor.Done():
		t.Fatal("progress=end заменил native Wait")
	default:
	}
	close(release)
	if r := waitResult(t, supervisor); r.Err != nil || !r.Clean {
		t.Fatalf("native completion: %+v", r)
	}
	var stdout, stderr []byte
	for _, d := range <-drained {
		if d.Attempt != 1 || d.SessionID != "session" || d.GroupID != "group" {
			t.Fatalf("диагностика потеряла владельца: %+v", d)
		}
		switch d.Kind {
		case "stdout":
			stdout = append(stdout, d.Data...)
		case "stderr":
			stderr = append(stderr, d.Data...)
		}
	}
	if string(stdout) != nativeProgressBytes || !bytes.Equal(stderr, []byte{0, 255, '\n'}) || supervisor.Snapshot().DroppedDiagnostics != 0 {
		t.Fatalf("диагностика изменена: stdout=%q stderr=%x", stdout, stderr)
	}
	if _, err = f.Stat(); !errors.Is(err, os.ErrClosed) {
		t.Fatal("native FD не закрыт", err)
	}
	if err = scope.Seal(ctx); err != nil {
		t.Fatal(err)
	}
	if err = scope.Close(); err != nil {
		t.Fatal(err)
	}
	if err = registry.AcknowledgeScope(ctx, scope.ID()); err != nil {
		t.Fatal(err)
	}
}
