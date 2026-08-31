package redis

import (
	"context"
	"fmt"
	"strconv"
	"time"

	"claracore-server/internal/ports"
)

const incrementWindowScript = `
local count = redis.call("INCR", KEYS[1])
if count == 1 then
  redis.call("PEXPIRE", KEYS[1], ARGV[1])
end
return count`

// Allow applies an atomic fixed-window counter in Redis. The bucket is part of
// the key, so one delayed expiry cannot extend the active decision window.
func (c *Cache) Allow(ctx context.Context, key string, limit int, window time.Duration) (ports.RateLimitResult, error) {
	if key == "" || len(key) > 256 || limit <= 0 || window <= 0 || window > 24*time.Hour {
		return ports.RateLimitResult{}, fmt.Errorf("redis: rate limit 参数非法")
	}
	now := time.Now().UTC()
	nanos := window.Nanoseconds()
	bucket := now.UnixNano() / nanos
	next := time.Unix(0, (bucket+1)*nanos).UTC()
	retryAfter := next.Sub(now)
	if retryAfter <= 0 {
		retryAfter = time.Millisecond
	}
	storageKey := c.prefix + key + ":" + strconv.FormatInt(bucket, 10)
	count, err := c.client.Eval(ctx, incrementWindowScript, []string{storageKey}, (2 * window).Milliseconds()).Int64()
	if err != nil {
		return ports.RateLimitResult{}, err
	}
	return ports.RateLimitResult{Allowed: count <= int64(limit), RetryAfter: retryAfter}, nil
}

var _ ports.RateLimiter = (*Cache)(nil)
