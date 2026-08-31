package main

import (
	"context"
	"log/slog"
	"sync"
	"time"

	externaldomain "claracore-server/internal/domain/external"
	"claracore-server/internal/platform"
)

const externalHealthSampleInterval = 30 * time.Second

// externalHealthMetrics is a constant-cardinality, body-free aggregation. It
// deliberately contains no tenant, source name, URL, error text, or run ID.
type externalHealthMetrics struct {
	Configured      bool
	Available       bool
	SampledAt       time.Time
	FreshnessBudget time.Duration
	Sources         int
	Documents       int
	Running         int
	Failed          int
	NeverSucceeded  int
	Stale           int
	PendingRequests int
	ActiveClaims    int
	ExpiredClaims   int
}

type externalHealthSnapshot struct {
	mu   sync.RWMutex
	data externalHealthMetrics
}

func newExternalHealthSnapshot(configured bool) *externalHealthSnapshot {
	return &externalHealthSnapshot{data: externalHealthMetrics{Configured: configured}}
}

func (s *externalHealthSnapshot) Store(data externalHealthMetrics) {
	s.mu.Lock()
	s.data = data
	s.mu.Unlock()
}

func (s *externalHealthSnapshot) Load() externalHealthMetrics {
	s.mu.RLock()
	data := s.data
	s.mu.RUnlock()
	return data
}

type externalHealthReader interface {
	Health(context.Context, platform.ID) ([]externaldomain.SourceHealth, error)
}

func runExternalHealthSampler(ctx context.Context, enabled bool, reader externalHealthReader, tenantID platform.ID, syncInterval, syncTimeout time.Duration, now func() time.Time, snapshot *externalHealthSnapshot, logger *slog.Logger, done chan<- struct{}) {
	defer close(done)
	if !enabled || reader == nil || tenantID == "" || snapshot == nil {
		return
	}
	if now == nil {
		now = time.Now
	}
	if logger == nil {
		logger = slog.Default()
	}
	sample := func() {
		at := now().UTC()
		sampleCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
		sources, err := reader.Health(sampleCtx, tenantID)
		cancel()
		if err != nil {
			snapshot.Store(externalHealthMetrics{Configured: true, SampledAt: at, FreshnessBudget: externalFreshnessBudget(syncInterval, syncTimeout)})
			if ctx.Err() == nil {
				logger.Warn("采样 Atlas 外部源指标失败", "error", err)
			}
			return
		}
		snapshot.Store(summarizeExternalHealth(sources, at, syncInterval, syncTimeout))
	}
	sample()
	ticker := time.NewTicker(externalHealthSampleInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			sample()
		}
	}
}

func summarizeExternalHealth(sources []externaldomain.SourceHealth, now time.Time, syncInterval, syncTimeout time.Duration) externalHealthMetrics {
	budget := externalFreshnessBudget(syncInterval, syncTimeout)
	out := externalHealthMetrics{
		Configured: true, Available: true, SampledAt: now.UTC(), FreshnessBudget: budget, Sources: len(sources),
	}
	for _, source := range sources {
		if source.DocumentCount > 0 {
			out.Documents += source.DocumentCount
		}
		switch source.LastSyncStatus {
		case externaldomain.SyncRunning:
			out.Running++
		case externaldomain.SyncFailed:
			out.Failed++
		}
		if source.LastSucceededAt == nil {
			out.NeverSucceeded++
			out.Stale++
		} else {
			age := now.Sub(*source.LastSucceededAt)
			if age < -5*time.Minute || age > budget {
				out.Stale++
			}
		}
		if source.SyncRequestedAt != nil {
			out.PendingRequests++
		}
		if source.SyncClaimed {
			if source.SyncClaimUntil != nil && source.SyncClaimUntil.After(now) {
				out.ActiveClaims++
			} else {
				out.ExpiredClaims++
			}
		}
	}
	return out
}

func externalFreshnessBudget(syncInterval, syncTimeout time.Duration) time.Duration {
	budget := 2*syncInterval + syncTimeout
	if budget < 15*time.Minute {
		return 15 * time.Minute
	}
	return budget
}
