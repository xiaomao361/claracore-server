package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"testing"
	"time"

	externaldomain "claracore-server/internal/domain/external"
	"claracore-server/internal/platform"
)

func TestSummarizeExternalHealthIsBoundedAndActionable(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	recent, old := now.Add(-time.Minute), now.Add(-time.Hour)
	activeClaim, expiredClaim := now.Add(time.Minute), now.Add(-time.Second)
	got := summarizeExternalHealth([]externaldomain.SourceHealth{
		{LastSyncStatus: externaldomain.SyncRunning, LastSucceededAt: &recent, DocumentCount: 12, SyncRequestedAt: &recent, SyncClaimed: true, SyncClaimUntil: &activeClaim},
		{LastSyncStatus: externaldomain.SyncFailed, LastSucceededAt: &old, DocumentCount: 25, SyncClaimed: true, SyncClaimUntil: &expiredClaim},
		{LastSyncStatus: externaldomain.SyncNever, DocumentCount: -1},
	}, now, 15*time.Minute, 10*time.Minute)
	if !got.Configured || !got.Available || got.FreshnessBudget != 40*time.Minute || got.Sources != 3 || got.Documents != 37 ||
		got.Running != 1 || got.Failed != 1 || got.NeverSucceeded != 1 || got.Stale != 2 || got.PendingRequests != 1 ||
		got.ActiveClaims != 1 || got.ExpiredClaims != 1 {
		t.Fatalf("unexpected external health aggregation: %+v", got)
	}
}

func TestExternalFreshnessBudgetHasNoiseFloor(t *testing.T) {
	if got := externalFreshnessBudget(time.Minute, time.Minute); got != 15*time.Minute {
		t.Fatalf("small sync budgets need a 15-minute alert floor, got %s", got)
	}
	if got := externalFreshnessBudget(time.Hour, 10*time.Minute); got != 130*time.Minute {
		t.Fatalf("freshness budget must cover two intervals plus one timeout, got %s", got)
	}
}

type samplerExternalHealthReader struct {
	sources []externaldomain.SourceHealth
	err     error
	called  chan struct{}
}

func (r *samplerExternalHealthReader) Health(context.Context, platform.ID) ([]externaldomain.SourceHealth, error) {
	if r.called != nil {
		select {
		case r.called <- struct{}{}:
		default:
		}
	}
	return r.sources, r.err
}

func TestExternalHealthSamplerPublishesFailureWithoutLeakingLastData(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	reader := &samplerExternalHealthReader{err: errors.New("database unavailable"), called: make(chan struct{}, 1)}
	snapshot := newExternalHealthSnapshot(true)
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})
	go runExternalHealthSampler(ctx, true, reader, "tenant", 15*time.Minute, 10*time.Minute, func() time.Time { return now }, snapshot, slog.New(slog.NewTextHandler(io.Discard, nil)), done)
	select {
	case <-reader.called:
	case <-time.After(time.Second):
		t.Fatal("external health sampler did not run immediately")
	}
	cancel()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("external health sampler did not stop")
	}
	got := snapshot.Load()
	if !got.Configured || got.Available || got.SampledAt != now || got.FreshnessBudget != 40*time.Minute || got.Sources != 0 {
		t.Fatalf("failed sample must be explicit and body-free: %+v", got)
	}
}
