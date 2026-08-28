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
)

func TestRunSessionCleanupLoopRunsOnStartupRetriesAndStops(t *testing.T) {
	var mu sync.Mutex
	calls := 0
	called := make(chan struct{}, 2)
	cleanup := func(context.Context) (int, error) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		called <- struct{}{}
		if calls == 1 {
			return 0, errors.New("temporary")
		}
		return 3, nil
	}
	var logs bytes.Buffer
	logger := slog.New(slog.NewTextHandler(&logs, nil))
	ctx, cancel := context.WithCancel(context.Background())
	ticks := make(chan time.Time, 1)
	done := make(chan struct{})
	go runSessionCleanupLoop(ctx, cleanup, logger, ticks, done)
	waitForCleanupCall(t, called)
	ticks <- time.Now()
	waitForCleanupCall(t, called)
	cancel()
	waitForWorkerExit(t, done)

	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Fatalf("calls=%d", calls)
	}
	output := logs.String()
	if !strings.Contains(output, "清理失败，将在下一轮重试") || !strings.Contains(output, "deleted=3") {
		t.Fatalf("日志不完整:\n%s", output)
	}
}
