package ports

import (
	"context"
	"time"
)

// RateLimitResult is the outcome of one counted attempt in a fixed window.
// RetryAfter is meaningful only when Allowed is false.
type RateLimitResult struct {
	Allowed    bool
	RetryAfter time.Duration
}

// RateLimiter counts an attempt and decides whether it may proceed.
//
// Keys must already be privacy-safe: implementations may persist them in Redis.
// The interface deliberately does not accept usernames, IP addresses, or domain
// entities, so infrastructure cannot accidentally invent identity semantics.
type RateLimiter interface {
	Allow(ctx context.Context, key string, limit int, window time.Duration) (RateLimitResult, error)
}
