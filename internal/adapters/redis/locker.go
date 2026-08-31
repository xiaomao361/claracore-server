package redis

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"sync/atomic"
	"time"

	redisclient "github.com/redis/go-redis/v9"

	"claracore-server/internal/ports"
)

const releaseLeaseScript = `
if redis.call("GET", KEYS[1]) == ARGV[1] then
  return redis.call("DEL", KEYS[1])
end
return 0`

var _ ports.Locker = (*Cache)(nil)

type lease struct {
	client   *redisclient.Client
	key      string
	token    string
	released atomic.Bool
}

func (c *Cache) TryLock(ctx context.Context, key string, ttl time.Duration) (ports.Lease, bool, error) {
	if key == "" {
		return nil, false, fmt.Errorf("redis: lock key 不能为空")
	}
	if ttl <= 0 {
		return nil, false, fmt.Errorf("redis: lock ttl 必须为正数")
	}
	raw := make([]byte, 16)
	if _, err := rand.Read(raw); err != nil {
		return nil, false, fmt.Errorf("redis: 生成 lock token: %w", err)
	}
	token := hex.EncodeToString(raw)
	redisKey := c.prefix + key
	acquired, err := c.client.SetNX(ctx, redisKey, token, ttl).Result()
	if err != nil {
		return nil, false, err
	}
	if !acquired {
		return nil, false, nil
	}
	return &lease{client: c.client, key: redisKey, token: token}, true, nil
}

func (l *lease) Release(ctx context.Context) error {
	if l.released.Swap(true) {
		return nil
	}
	return l.client.Eval(ctx, releaseLeaseScript, []string{l.key}, l.token).Err()
}
