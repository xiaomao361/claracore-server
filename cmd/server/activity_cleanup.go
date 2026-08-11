package main

import (
	"context"
	"log/slog"
	"time"

	"claracore-server/internal/domain/activity"
	"claracore-server/internal/platform"
)

const (
	traceRetention       = 30 * 24 * time.Hour
	traceCleanupInterval = 24 * time.Hour
	traceCleanupBatch    = 1000
)

// runTraceCleanup removes expired invocation traces on startup and once per
// day. Governance and meta-audit rows are protected by
// activity.Service.DeleteTracesBefore, the only deletion path used here.
func runTraceCleanup(
	ctx context.Context,
	svc *activity.Service,
	clock platform.Clock,
	logger *slog.Logger,
	done chan<- struct{},
) {
	ticker := time.NewTicker(traceCleanupInterval)
	defer ticker.Stop()
	runTraceCleanupLoop(ctx, svc, clock, logger, ticker.C, done)
}

// runTraceCleanupLoop accepts the tick channel so tests can drive daily runs
// without waiting on wall-clock time.
func runTraceCleanupLoop(
	ctx context.Context,
	svc *activity.Service,
	clock platform.Clock,
	logger *slog.Logger,
	ticks <-chan time.Time,
	done chan<- struct{},
) {
	defer close(done)
	logger.Info("Activity trace 清理 worker 启动",
		"retention", traceRetention,
		"interval", traceCleanupInterval,
		"batch", traceCleanupBatch)

	for {
		deleted, err := deleteExpiredTraceBatches(ctx, svc, clock)
		if err != nil {
			logger.Warn("Activity trace 清理失败，将在下一轮重试", "deleted", deleted, "error", err)
		} else if deleted > 0 {
			logger.Info("Activity trace 清理完成", "deleted", deleted)
		}

		select {
		case <-ctx.Done():
			logger.Info("Activity trace 清理 worker 退出")
			return
		case <-ticks:
		}
	}
}

// deleteExpiredTraceBatches drains every expired trace present at the start of
// this run. A full batch means more rows may remain, so it immediately asks for
// another bounded batch instead of waiting a day.
func deleteExpiredTraceBatches(ctx context.Context, svc *activity.Service, clock platform.Clock) (int, error) {
	before := clock.Now().UTC().Add(-traceRetention)
	total := 0
	for {
		deleted, err := svc.DeleteTracesBefore(ctx, before, traceCleanupBatch)
		total += deleted
		if err != nil {
			return total, err
		}
		if deleted < traceCleanupBatch {
			return total, nil
		}
	}
}
