package ratelimit

import (
	"context"
	"time"

	"claracore-server/internal/ports"
)

// Failover uses the shared limiter when it is healthy and the local limiter
// when it is not. A Redis outage may multiply the effective limit by the number
// of instances, but it must not turn password hashing into an unbounded endpoint.
type Failover struct {
	primary  ports.RateLimiter
	fallback ports.RateLimiter
}

func NewFailover(primary, fallback ports.RateLimiter) *Failover {
	return &Failover{primary: primary, fallback: fallback}
}

func (f *Failover) Allow(ctx context.Context, key string, limit int, window time.Duration) (ports.RateLimitResult, error) {
	if f.primary != nil {
		result, err := f.primary.Allow(ctx, key, limit, window)
		if err == nil {
			return result, nil
		}
	}
	return f.fallback.Allow(ctx, key, limit, window)
}

var _ ports.RateLimiter = (*Failover)(nil)
