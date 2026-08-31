package ratelimit

import (
	"context"
	"errors"
	"testing"
	"time"

	"claracore-server/internal/platform"
	"claracore-server/internal/ports"
)

func TestMemoryCountsAndResetsFixedWindow(t *testing.T) {
	clock := &platform.FixedClock{T: time.Date(2026, 8, 29, 8, 0, 10, 0, time.UTC)}
	limiter := NewMemory(clock, 10)
	for i := 0; i < 2; i++ {
		result, err := limiter.Allow(context.Background(), "login-key", 2, time.Minute)
		if err != nil || !result.Allowed {
			t.Fatalf("attempt %d should pass: result=%+v err=%v", i+1, result, err)
		}
	}
	blocked, err := limiter.Allow(context.Background(), "login-key", 2, time.Minute)
	if err != nil || blocked.Allowed || blocked.RetryAfter != 50*time.Second {
		t.Fatalf("third attempt should wait for the fixed boundary: result=%+v err=%v", blocked, err)
	}

	clock.T = clock.T.Add(50*time.Second + time.Millisecond)
	reset, err := limiter.Allow(context.Background(), "login-key", 2, time.Minute)
	if err != nil || !reset.Allowed {
		t.Fatalf("new window should reset the count: result=%+v err=%v", reset, err)
	}
}

func TestMemoryBoundsUniqueKeysAndRecoversAfterExpiry(t *testing.T) {
	clock := &platform.FixedClock{T: time.Date(2026, 8, 29, 8, 0, 10, 0, time.UTC)}
	limiter := NewMemory(clock, 1)
	if result, err := limiter.Allow(context.Background(), "first", 10, time.Minute); err != nil || !result.Allowed {
		t.Fatalf("first key should pass: result=%+v err=%v", result, err)
	}
	if result, err := limiter.Allow(context.Background(), "second", 10, time.Minute); err != nil || result.Allowed {
		t.Fatalf("unseen key must fail closed at capacity: result=%+v err=%v", result, err)
	}
	clock.T = clock.T.Add(50*time.Second + time.Millisecond)
	if result, err := limiter.Allow(context.Background(), "second", 10, time.Minute); err != nil || !result.Allowed {
		t.Fatalf("expired key must free capacity: result=%+v err=%v", result, err)
	}
}

type stubLimiter struct {
	result ports.RateLimitResult
	err    error
	calls  int
}

func (s *stubLimiter) Allow(context.Context, string, int, time.Duration) (ports.RateLimitResult, error) {
	s.calls++
	return s.result, s.err
}

func TestFailoverUsesLocalLimiterOnlyWhenPrimaryErrors(t *testing.T) {
	primary := &stubLimiter{err: errors.New("redis unavailable")}
	fallback := &stubLimiter{result: ports.RateLimitResult{Allowed: true}}
	limiter := NewFailover(primary, fallback)
	result, err := limiter.Allow(context.Background(), "key", 1, time.Minute)
	if err != nil || !result.Allowed || primary.calls != 1 || fallback.calls != 1 {
		t.Fatalf("primary error should use fallback: result=%+v err=%v calls=%d/%d", result, err, primary.calls, fallback.calls)
	}

	primary.err = nil
	primary.result = ports.RateLimitResult{Allowed: false, RetryAfter: time.Minute}
	result, err = limiter.Allow(context.Background(), "key", 1, time.Minute)
	if err != nil || result.Allowed || fallback.calls != 1 {
		t.Fatalf("a healthy primary denial must not bypass through fallback: result=%+v err=%v fallbackCalls=%d", result, err, fallback.calls)
	}
}

func TestMemoryRejectsInvalidInputAndCancelledContext(t *testing.T) {
	limiter := NewMemory(&platform.FixedClock{T: time.Now()}, 10)
	if _, err := limiter.Allow(context.Background(), "", 1, time.Minute); err == nil {
		t.Fatal("empty key should fail")
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	if _, err := limiter.Allow(ctx, "key", 1, time.Minute); !errors.Is(err, context.Canceled) {
		t.Fatalf("cancelled context should be preserved: %v", err)
	}
}
