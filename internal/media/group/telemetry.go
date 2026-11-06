package group

import (
	"context"
	"errors"
	"log/slog"
	"sync/atomic"
	"time"

	"github.com/GregoryGost/gost-restreamer/internal/core"
	"github.com/GregoryGost/gost-restreamer/internal/media"
	"github.com/GregoryGost/gost-restreamer/internal/media/ffmpeg"
	"github.com/GregoryGost/gost-restreamer/internal/monitoring"
	"github.com/GregoryGost/gost-restreamer/internal/monitoring/logging"
)

func rawCounters(s media.CounterSnapshot) monitoring.RawCounters {
	return monitoring.RawCounters{SampledAt: s.SampledAt, VideoFrames: s.VideoFrames, PayloadBytes: s.PayloadBytes, Saturated: s.Saturated}
}

func (g *Group) ffmpegLogger(observer *core.FFmpegObserver, role core.FFmpegRole) *slog.Logger {
	if g.config.Logger == nil {
		return nil
	}
	return g.config.Logger.With("component", "ffmpeg", "session_id", g.parent.SessionID, "group_id", g.id,
		"group_instance_id", g.observer.Token(), "worker_instance_id", observer.Token(), "worker_role", role)
}

// drainDiagnostics читает до EOF независимо от отмены медиапотока.
// Logger принадлежит одному Supervisor; очередь сохраняет локальные номера попыток.
func (g *Group) drainDiagnostics(s *ffmpeg.Supervisor, logger *slog.Logger) <-chan struct{} {
	done := make(chan struct{})
	go func() {
		defer close(done)
		for {
			d, err := s.NextDiagnostic(context.Background())
			if err != nil {
				return
			}
			if logger == nil {
				continue
			}
			logger.LogAttrs(context.Background(), slog.LevelInfo, "ffmpeg_diagnostic", slog.String("operation", d.Kind),
				slog.Time("observed_at", d.Time), slog.Uint64("attempt", d.Attempt), slog.Uint64("sequence", d.Sequence),
				logging.Binary("data", d.Data), slog.Any("argv", d.Args), slog.String("message", d.Message))
		}
	}()
	return done
}

type attemptBinding struct {
	localID  uint64
	coreID   uint64
	observer *core.FFmpegAttemptObserver
	logger   *slog.Logger
}

func bindFFmpegAttempt(localID, coreID uint64, observer *core.FFmpegAttemptObserver, logger *slog.Logger) *attemptBinding {
	if logger != nil {
		logger = logger.With("attempt", localID, "core_attempt", coreID)
		logger.Info("ffmpeg_attempt", "operation", "begin_attempt")
	}
	return &attemptBinding{localID: localID, coreID: coreID, observer: observer, logger: logger}
}

func (g *Group) finishFFmpegObservation(binding *attemptBinding, failure error, stopped bool) {
	observationFailure := failure
	if stopped {
		observationFailure = nil
	}
	g.observeFFmpegCompletion(binding, observationFailure, stopped)
	if binding != nil && failure != nil && binding.logger != nil {
		binding.logger.Error("ffmpeg_worker", "operation", "wait", "error", failure)
	}
}

func (g *Group) observeFFmpegCompletion(binding *attemptBinding, failure error, stopped bool) {
	if binding == nil {
		return
	}
	state, message := core.ObservationFailed, ""
	if failure != nil {
		message = failure.Error()
	}
	if stopped {
		state = core.ObservationStopped
	}
	ctx, cancel := context.WithTimeout(context.Background(), g.config.ShutdownTimeout)
	defer cancel()
	_ = binding.observer.Observe(ctx, core.FFmpegObservation{State: state, Error: message})
}

func (g *Group) observeAttemptResult(binding *atomic.Pointer[attemptBinding]) func(ffmpeg.AttemptResult) {
	return func(result ffmpeg.AttemptResult) {
		owner := binding.Load()
		if owner == nil || owner.localID != result.ID {
			return
		}
		failure := errors.Join(result.Err, result.CleanupErr)
		if result.Stopped {
			failure = result.CleanupErr
		}
		g.observeFFmpegCompletion(owner, failure, result.Stopped)
		if owner.logger != nil && (result.Err != nil || result.CleanupErr != nil) {
			owner.logger.Error("ffmpeg_attempt", "operation", "attempt_result", "error", result.Err, "cleanup_error", result.CleanupErr, "stopped", result.Stopped)
		}
	}
}

func (g *Group) registerFFmpegMetrics(supervisor *ffmpeg.Supervisor, observer *core.FFmpegObserver, binding *atomic.Pointer[attemptBinding]) error {
	if g.config.Sampler == nil {
		return nil
	}
	return g.config.Sampler.Register(observer.Token(), func(ctx context.Context, _ time.Time, publish bool) error {
		if !publish {
			return nil
		}
		owner := binding.Load()
		if owner == nil {
			return nil
		}
		s := supervisor.Snapshot()
		if s.Attempt != owner.localID {
			return nil
		}
		r := s.Report
		status, reason := core.MetricAvailable, core.ReasonNone
		if !s.HasReport {
			status, reason = core.MetricWarmingUp, core.ReasonWarmingUp
		} else if time.Since(r.SampledAt) > g.config.MetricStaleAfter {
			status, reason = core.MetricStale, core.ReasonStale
		}
		p := core.FFmpegProgress{Frame: core.OptionalUint64{Value: r.Frame, Present: r.HasFrame}, FPS: core.OptionalFloat64{Value: r.FPS, Present: r.HasFPS},
			TotalSizeBytes: core.OptionalUint64{Value: r.TotalSizeBytes, Present: r.HasTotalSize}, BitrateBitsPerSecond: core.OptionalFloat64{Value: r.BitrateKbitsPerSecond * 1000, Present: r.HasBitrate},
			OutTimeUS: core.OptionalInt64{Value: r.OutTimeUS, Present: r.HasOutTime}, DupFrames: core.OptionalUint64{Value: r.DupFrames, Present: r.HasDupFrames},
			DropFrames: core.OptionalUint64{Value: r.DropFrames, Present: r.HasDropFrames}, Speed: core.OptionalFloat64{Value: r.Speed, Present: r.HasSpeed}, End: r.End}
		return owner.observer.ObserveMetrics(ctx, core.FFmpegMetrics{SampledAt: s.SampledAt, Status: status, Reason: reason, Progress: p, HasProgress: s.HasReport, LastProgressAt: r.SampledAt, ProgressLosses: s.DroppedReports, DiagnosticLosses: s.DroppedDiagnostics})
	})
}

func (g *Group) joinDiagnostics(s *ffmpeg.Supervisor, done <-chan struct{}) {
	ctx, cancel := context.WithTimeout(context.Background(), g.config.ShutdownTimeout)
	defer cancel()
	g.addCleanup(s.Stop(ctx))
	<-done
}

func (g *Group) registerMetrics() error {
	if g.config.Sampler == nil {
		return nil
	}
	calculators := [3]*monitoring.RateCalculator{}
	for i := range calculators {
		c, err := monitoring.NewRateCalculator(g.config.MetricStaleAfter)
		if err != nil {
			return err
		}
		calculators[i] = c
	}
	token := g.observer.Token()
	return g.config.Sampler.Register(token, func(ctx context.Context, _ time.Time, publish bool) error {
		live, fallback, output := g.MediaSnapshots()
		snapshots := [3]media.CounterSnapshot{live, fallback, output}
		values := [3]core.MediaMetrics{}
		kinds := [3]monitoring.SourceKind{monitoring.SourceGroupLive, monitoring.SourceGroupFallback, monitoring.SourceGroupOutput}
		for i, s := range snapshots {
			rates := calculators[i].Sample(monitoring.SourceIdentity{Token: token, Kind: kinds[i]}, rawCounters(s), time.Now())
			if s.VideoFrames == 0 && s.AudioUnits == 0 {
				rates = calculators[i].Absent()
			}
			values[i] = s.Metrics(rates)
		}
		if !publish {
			return nil
		}
		return g.observer.ObserveMetrics(ctx, core.GroupMetrics{LiveInput: values[0], FallbackInput: values[1], Output: values[2]})
	})
}
