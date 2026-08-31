package ratelimit

import (
	"context"
	"fmt"
	"strconv"
	"sync"
	"time"

	"claracore-server/internal/platform"
	"claracore-server/internal/ports"
)

const defaultMaxKeys = 20_000

type counter struct {
	count     int
	expiresAt time.Time
}

// Memory is the always-available per-process limiter. A configured Redis
// limiter may sit in front of it, but Redis failure must never remove all login
// protection. Capacity is bounded so unique-key floods cannot grow memory
// without limit; when full, unseen keys fail closed until an entry expires.
type Memory struct {
	mu      sync.Mutex
	clock   platform.Clock
	maxKeys int
	entries map[string]counter
}

func NewMemory(clock platform.Clock, maxKeys int) *Memory {
	if maxKeys <= 0 {
		maxKeys = defaultMaxKeys
	}
	return &Memory{clock: clock, maxKeys: maxKeys, entries: make(map[string]counter)}
}

func (m *Memory) Allow(ctx context.Context, key string, limit int, window time.Duration) (ports.RateLimitResult, error) {
	if err := ctx.Err(); err != nil {
		return ports.RateLimitResult{}, err
	}
	if err := validate(key, limit, window); err != nil {
		return ports.RateLimitResult{}, err
	}
	now := m.clock.Now().UTC()
	bucket, retryAfter := fixedWindow(now, window)
	storageKey := key + ":" + strconv.FormatInt(bucket, 10)

	m.mu.Lock()
	defer m.mu.Unlock()
	if _, exists := m.entries[storageKey]; !exists && len(m.entries) >= m.maxKeys {
		for k, value := range m.entries {
			if !value.expiresAt.After(now) {
				delete(m.entries, k)
			}
		}
		if len(m.entries) >= m.maxKeys {
			return ports.RateLimitResult{Allowed: false, RetryAfter: retryAfter}, nil
		}
	}
	value := m.entries[storageKey]
	value.count++
	// Local buckets can disappear exactly at the next boundary. Redis retains
	// old buckets longer for distributed clock skew, but doing that here would
	// let stale entries crowd out every client at a window transition.
	value.expiresAt = now.Add(retryAfter)
	m.entries[storageKey] = value
	return ports.RateLimitResult{Allowed: value.count <= limit, RetryAfter: retryAfter}, nil
}

func validate(key string, limit int, window time.Duration) error {
	if key == "" || len(key) > 256 {
		return fmt.Errorf("ratelimit: key 长度非法")
	}
	if limit <= 0 {
		return fmt.Errorf("ratelimit: limit 必须为正数")
	}
	if window <= 0 || window > 24*time.Hour {
		return fmt.Errorf("ratelimit: window 必须在 0 到 24 小时之间")
	}
	return nil
}

func fixedWindow(now time.Time, window time.Duration) (bucket int64, retryAfter time.Duration) {
	nanos := window.Nanoseconds()
	bucket = now.UnixNano() / nanos
	next := time.Unix(0, (bucket+1)*nanos).UTC()
	retryAfter = next.Sub(now)
	if retryAfter <= 0 {
		retryAfter = time.Millisecond
	}
	return bucket, retryAfter
}

var _ ports.RateLimiter = (*Memory)(nil)
