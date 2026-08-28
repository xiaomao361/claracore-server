package main

import (
	"context"
	"log/slog"
	"time"
)

const sessionCleanupInterval = 24 * time.Hour

// runSessionCleanup removes expired Web sessions on startup and daily. The
// domain service owns the expiration clock and deletion semantics; this worker
// only owns scheduling and retry-after-next-tick behavior.
func runSessionCleanup(ctx context.Context, cleanup func(context.Context) (int, error), logger *slog.Logger, done chan<- struct{}) {
	ticker := time.NewTicker(sessionCleanupInterval)
	defer ticker.Stop()
	runSessionCleanupLoop(ctx, cleanup, logger, ticker.C, done)
}

func runSessionCleanupLoop(ctx context.Context, cleanup func(context.Context) (int, error), logger *slog.Logger, ticks <-chan time.Time, done chan<- struct{}) {
	defer close(done)
	logger.Info("Web session 清理 worker 启动", "interval", sessionCleanupInterval)
	for {
		deleted, err := cleanup(ctx)
		if err != nil {
			if ctx.Err() == nil {
				logger.Warn("Web session 清理失败，将在下一轮重试", "error", err)
			}
		} else if deleted > 0 {
			logger.Info("Web session 清理完成", "deleted", deleted)
		}

		select {
		case <-ctx.Done():
			logger.Info("Web session 清理 worker 退出")
			return
		case <-ticks:
		}
	}
}
