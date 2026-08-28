package main

import (
	"bytes"
	"context"
	"errors"
	"log/slog"
	"strings"
	"sync"
	"testing"
	"time"

	"claracore-server/internal/domain/activity"
	"claracore-server/internal/platform"
)

func TestDeleteExpiredTraceBatchesDrainsFullBatches(t *testing.T) {
	now := time.Date(2026, time.August, 11, 9, 30, 0, 0, time.FixedZone("CST", 8*60*60))
	store := &traceCleanupStore{
		results: []traceDeleteResult{{deleted: 1000}, {deleted: 1000}, {deleted: 17}},
	}
	svc := activity.NewService(store, platform.FixedClock{T: now}, platform.NewIDGenerator(platform.FixedClock{T: now}))

	deleted, err := deleteExpiredTraceBatches(context.Background(), svc, platform.FixedClock{T: now})
	if err != nil {
		t.Fatalf("deleteExpiredTraceBatches: %v", err)
	}
	if deleted != 2017 {
		t.Fatalf("deleted = %d, want 2017", deleted)
	}

	calls := store.snapshotCalls()
	if len(calls) != 3 {
		t.Fatalf("calls = %d, want 3", len(calls))
	}
	wantBefore := now.UTC().Add(-30 * 24 * time.Hour)
	for i, call := range calls {
		if call.limit != 1000 {
			t.Errorf("call %d limit = %d, want 1000", i, call.limit)
		}
		if !call.before.Equal(wantBefore) {
			t.Errorf("call %d before = %s, want %s", i, call.before, wantBefore)
		}
	}
}

func TestRunTraceCleanupLoopRetriesAfterErrorAndStopsWithContext(t *testing.T) {
	now := time.Date(2026, time.August, 11, 1, 30, 0, 0, time.UTC)
	store := &traceCleanupStore{
		results: []traceDeleteResult{{err: errors.New("temporary database error")}, {deleted: 7}},
		called:  make(chan struct{}, 2),
	}
	svc := activity.NewService(store, platform.FixedClock{T: now}, platform.NewIDGenerator(platform.FixedClock{T: now}))
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time, 1)
	done := make(chan struct{})

	go runTraceCleanupLoop(ctx, svc, platform.FixedClock{T: now}, logger, ticks, done)
	waitForCleanupCall(t, store.called)

	// The startup attempt failed, but the worker remains alive and retries when
	// the next daily tick is delivered by the test-controlled channel.
	ticks <- now.Add(traceCleanupInterval)
	waitForCleanupCall(t, store.called)
	cancel()
	waitForWorkerExit(t, done)

	if got := len(store.snapshotCalls()); got != 2 {
		t.Fatalf("calls = %d, want 2", got)
	}
	if output := logs.String(); !strings.Contains(output, "清理失败，将在下一轮重试") {
		t.Fatalf("warning log missing; logs:\n%s", output)
	}
}

func waitForCleanupCall(t *testing.T, called <-chan struct{}) {
	t.Helper()
	select {
	case <-called:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for cleanup call")
	}
}

func waitForWorkerExit(t *testing.T, done <-chan struct{}) {
	t.Helper()
	select {
	case <-done:
	case <-time.After(time.Second):
		t.Fatal("timed out waiting for cleanup worker to exit")
	}
}

type traceDeleteResult struct {
	deleted int
	err     error
}

type traceDeleteCall struct {
	before time.Time
	limit  int
}

type traceCleanupStore struct {
	mu      sync.Mutex
	results []traceDeleteResult
	calls   []traceDeleteCall
	called  chan struct{}
}

func (s *traceCleanupStore) Record(context.Context, activity.Event) error {
	panic("unexpected Record call")
}

func (s *traceCleanupStore) ListVisible(context.Context, activity.VisibleQuery) ([]activity.Event, error) {
	panic("unexpected ListVisible call")
}

func (s *traceCleanupStore) ListAdmin(context.Context, activity.AdminQuery) ([]activity.Event, error) {
	panic("unexpected ListAdmin call")
}

func (s *traceCleanupStore) CountContextFeedback(context.Context, activity.ContextFeedbackQuery) (activity.ContextFeedbackQuality, error) {
	panic("unexpected CountContextFeedback call")
}

func (s *traceCleanupStore) DeleteTracesBefore(_ context.Context, before time.Time, limit int) (int, error) {
	s.mu.Lock()
	s.calls = append(s.calls, traceDeleteCall{before: before, limit: limit})
	var result traceDeleteResult
	if len(s.results) > 0 {
		result = s.results[0]
		s.results = s.results[1:]
	}
	called := s.called
	s.mu.Unlock()

	if called != nil {
		called <- struct{}{}
	}
	return result.deleted, result.err
}

func (s *traceCleanupStore) snapshotCalls() []traceDeleteCall {
	s.mu.Lock()
	defer s.mu.Unlock()
	return append([]traceDeleteCall(nil), s.calls...)
}
