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

// Переносит время, число видеокадров, полезные байты и насыщение в исходные счётчики расчёта скорости.
func rawCounters(s media.CounterSnapshot) monitoring.RawCounters {
	return monitoring.RawCounters{SampledAt: s.SampledAt, VideoFrames: s.VideoFrames, PayloadBytes: s.PayloadBytes, Saturated: s.Saturated}
}

// Создаёт журнал с точными идентификаторами Session, группы, работника и роли либо возвращает nil при
// выключенном журналировании.
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
	// done закрывается после полного дочитывания диагностики Supervisor.
	done := make(chan struct{})
	go func() {
		// Дочитывает диагностику Supervisor до первой ошибки и при наличии logger записывает полные фрагменты
		// и аргументы; закрывает done после завершения.

		defer close(done)
		for {
			// d содержит очередное полное событие диагностики; err сообщает о завершении
			// либо отказе чтения очереди.
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

// attemptBinding связывает локальный номер Supervisor с доменной попыткой, наблюдателем и журналом.
type attemptBinding struct {
	// localID содержит номер физической попытки внутри конкретного Supervisor.
	localID uint64
	// coreID содержит локальный для источника группы номер доменной попытки.
	coreID uint64
	// observer публикует состояние и метрики точной доменной FFmpeg-попытки.
	observer *core.FFmpegAttemptObserver
	// logger связывает записи с обоими номерами попытки и идентичностью работника.
	logger *slog.Logger
}

// Связывает локальный номер Supervisor с номером доменной попытки и наблюдателем, добавляя оба номера
// в журнал.
func bindFFmpegAttempt(localID, coreID uint64, observer *core.FFmpegAttemptObserver, logger *slog.Logger) *attemptBinding {
	if logger != nil {
		logger = logger.With("attempt", localID, "core_attempt", coreID)
		logger.Info("ffmpeg_attempt", "operation", "begin_attempt")
	}
	return &attemptBinding{localID: localID, coreID: coreID, observer: observer, logger: logger}
}

// Публикует завершение FFmpeg и журналирует исходную ошибку; остановка исключает обычную ошибку
// выполнения из оперативного отказа.
func (g *Group) finishFFmpegObservation(binding *attemptBinding, failure error, stopped bool) {
	// observationFailure содержит причину оперативного отказа, подавляемую при обычной остановке
	// работника.
	observationFailure := failure
	if stopped {
		observationFailure = nil
	}
	g.observeFFmpegCompletion(binding, observationFailure, stopped)
	if binding != nil && failure != nil && binding.logger != nil {
		binding.logger.Error("ffmpeg_worker", "operation", "wait", "error", failure)
	}
}

// Публикует FAILED либо STOPPED точного наблюдателя попытки в отдельный конечный срок остановки.
func (g *Group) observeFFmpegCompletion(binding *attemptBinding, failure error, stopped bool) {
	if binding == nil {
		return
	}
	// state выбирает FAILED либо STOPPED; message содержит текст передаваемой причины завершения.
	state, message := core.ObservationFailed, ""
	if failure != nil {
		message = failure.Error()
	}
	if stopped {
		state = core.ObservationStopped
	}
	// ctx ограничивает публикацию завершения отдельным сроком; cancel освобождает таймер.
	ctx, cancel := context.WithTimeout(context.Background(), g.config.ShutdownTimeout)
	defer cancel()
	_ = binding.observer.Observe(ctx, core.FFmpegObservation{State: state, Error: message})
}

// Возвращает обработчик итога только текущей локальной попытки; поздний результат другого номера
// пропускается, а ошибка очистки сохраняется и при остановке.
func (g *Group) observeAttemptResult(binding *atomic.Pointer[attemptBinding]) func(ffmpeg.AttemptResult) {
	return func(result ffmpeg.AttemptResult) {
		// Принимает результат только совпадающей физической попытки, публикует её завершение и ошибки; при
		// остановке ошибкой наблюдения остаётся только ошибка очистки.

		// owner содержит текущую привязку для проверки совпадения результата с физической
		// попыткой.
		owner := binding.Load()
		if owner == nil || owner.localID != result.ID {
			return
		}
		// failure объединяет ошибки выполнения и очистки; при остановке сохраняет только ошибку
		// очистки.
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

// Регистрирует чтение метрик Supervisor для текущей привязки попытки, различая прогрев, устаревание и
// доступный отчёт и переводя bitrate в биты в секунду.
func (g *Group) registerFFmpegMetrics(supervisor *ffmpeg.Supervisor, observer *core.FFmpegObserver, binding *atomic.Pointer[attemptBinding]) error {
	if g.config.Sampler == nil {
		return nil
	}
	return g.config.Sampler.Register(observer.Token(), func(ctx context.Context, _ time.Time, publish bool) error {
		// Публикует метрики только текущей привязки и совпадающей попытки Supervisor, отмечая отсутствие
		// отчёта как прогрев, а просроченный отчёт как устаревший.

		if !publish {
			return nil
		}
		// owner содержит текущую привязку наблюдателя, с которой сверяется progress-снимок
		// Supervisor.
		owner := binding.Load()
		if owner == nil {
			return nil
		}
		// s содержит снимок текущей физической попытки Supervisor и накопленных потерь.
		s := supervisor.Snapshot()
		if s.Attempt != owner.localID {
			return nil
		}
		// r содержит последний полный progress-отчёт проверенной попытки.
		r := s.Report
		// status задаёт доступность измерения; reason различает обычный отчёт, прогрев и
		// устаревание.
		status, reason := core.MetricAvailable, core.ReasonNone
		if !s.HasReport {
			status, reason = core.MetricWarmingUp, core.ReasonWarmingUp
		} else if time.Since(r.SampledAt) > g.config.MetricStaleAfter {
			status, reason = core.MetricStale, core.ReasonStale
		}
		// p содержит типизированные progress-поля с признаками наличия и битрейтом в битах в
		// секунду.
		p := core.FFmpegProgress{Frame: core.OptionalUint64{Value: r.Frame, Present: r.HasFrame}, FPS: core.OptionalFloat64{Value: r.FPS, Present: r.HasFPS},
			TotalSizeBytes: core.OptionalUint64{Value: r.TotalSizeBytes, Present: r.HasTotalSize}, BitrateBitsPerSecond: core.OptionalFloat64{Value: r.BitrateKbitsPerSecond * 1000, Present: r.HasBitrate},
			OutTimeUS: core.OptionalInt64{Value: r.OutTimeUS, Present: r.HasOutTime}, DupFrames: core.OptionalUint64{Value: r.DupFrames, Present: r.HasDupFrames},
			DropFrames: core.OptionalUint64{Value: r.DropFrames, Present: r.HasDropFrames}, Speed: core.OptionalFloat64{Value: r.Speed, Present: r.HasSpeed}, End: r.End}
		return owner.observer.ObserveMetrics(ctx, core.FFmpegMetrics{SampledAt: s.SampledAt, Status: status, Reason: reason, Progress: p, HasProgress: s.HasReport, LastProgressAt: r.SampledAt, ProgressLosses: s.DroppedReports, DiagnosticLosses: s.DroppedDiagnostics})
	})
}

// Запрашивает ограниченную остановку Supervisor, сохраняет ошибку очистки и ждёт полного дочитывания
// его диагностики до EOF.
func (g *Group) joinDiagnostics(s *ffmpeg.Supervisor, done <-chan struct{}) {
	// ctx ограничивает запрос остановки Supervisor; cancel освобождает таймер перед полным
	// дочитыванием диагностики.
	ctx, cancel := context.WithTimeout(context.Background(), g.config.ShutdownTimeout)
	defer cancel()
	g.addCleanup(s.Stop(ctx))
	<-done
}

// Регистрирует отдельные расчёты скорости LIVE, заглушки и выхода по идентичности группы; отсутствие
// пакетов представляется отсутствием измерения, а публикация выполняется по запросу сборщика.
func (g *Group) registerMetrics() error {
	if g.config.Sampler == nil {
		return nil
	}
	// calculators содержит независимые расчёты скорости входа LIVE, входа заглушки и выхода группы.
	calculators := [3]*monitoring.RateCalculator{}
	// i обозначает направление, для которого создаётся расчёт скорости.
	for i := range calculators {
		// c содержит новый расчёт скорости выбранного направления; err сообщает об отказе его
		// создания.
		c, err := monitoring.NewRateCalculator(g.config.MetricStaleAfter)
		if err != nil {
			return err
		}
		calculators[i] = c
	}
	// token содержит точную идентичность наблюдателя группы для регистрации сборщика и различения
	// источников.
	token := g.observer.Token()
	return g.config.Sampler.Register(token, func(ctx context.Context, _ time.Time, publish bool) error {
		// Снимает счётчики трёх направлений и обновляет независимые расчёты скоростей; при publish передаёт
		// метрики наблюдателю группы, пустые направления отмечает отсутствующими.

		// live содержит накопления входа LIVE; fallback содержит накопления заглушки; output
		// содержит накопления выхода переключателя.
		live, fallback, output := g.MediaSnapshots()
		// snapshots упорядочивает три снимка для независимого расчёта скоростей.
		snapshots := [3]media.CounterSnapshot{live, fallback, output}
		// values собирает метрики LIVE, заглушки и выхода в том же порядке.
		values := [3]core.MediaMetrics{}
		// kinds задаёт различающиеся типы источника для идентичности каждого расчёта скорости.
		kinds := [3]monitoring.SourceKind{monitoring.SourceGroupLive, monitoring.SourceGroupFallback, monitoring.SourceGroupOutput}
		// i обозначает направление измерения; s содержит его текущие накопления пакетов.
		for i, s := range snapshots {
			// rates содержит рассчитанные скорости выбранного направления либо признак
			// отсутствия при пустых накоплениях.
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
