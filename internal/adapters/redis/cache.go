package redis

import (
	"context"
	"crypto/tls"
	"time"

	redisclient "github.com/redis/go-redis/v9"

	"claracore-server/internal/ports"
)

// Cache is a namespaced Redis implementation of ports.Cache. The prefix is
// applied to every key so one Redis deployment can safely host several apps.
type Cache struct {
	client *redisclient.Client
	prefix string
}

var _ ports.Cache = (*Cache)(nil)

func NewCache(addr, password, prefix string, timeout time.Duration, tlsConfigs ...*tls.Config) *Cache {
	if timeout <= 0 {
		timeout = 500 * time.Millisecond
	}
	var tlsConfig *tls.Config
	if len(tlsConfigs) > 0 && tlsConfigs[0] != nil {
		tlsConfig = tlsConfigs[0].Clone()
	}
	return &Cache{
		client: redisclient.NewClient(&redisclient.Options{
			Addr: addr, Password: password,
			DialTimeout: timeout, ReadTimeout: timeout, WriteTimeout: timeout, TLSConfig: tlsConfig,
		}),
		prefix: prefix,
	}
}

func (c *Cache) GetMany(ctx context.Context, keys []string) (map[string][]byte, error) {
	out := make(map[string][]byte, len(keys))
	if len(keys) == 0 {
		return out, nil
	}
	redisKeys := make([]string, len(keys))
	for i, key := range keys {
		redisKeys[i] = c.prefix + key
	}
	values, err := c.client.MGet(ctx, redisKeys...).Result()
	if err != nil {
		return nil, err
	}
	for i, value := range values {
		if value == nil {
			continue
		}
		switch typed := value.(type) {
		case string:
			out[keys[i]] = []byte(typed)
		case []byte:
			out[keys[i]] = append([]byte(nil), typed...)
		}
	}
	return out, nil
}

func (c *Cache) SetMany(ctx context.Context, values map[string][]byte, ttl time.Duration) error {
	if len(values) == 0 {
		return nil
	}
	pipe := c.client.Pipeline()
	for key, value := range values {
		pipe.Set(ctx, c.prefix+key, value, ttl)
	}
	_, err := pipe.Exec(ctx)
	return err
}

func (c *Cache) Ping(ctx context.Context) error { return c.client.Ping(ctx).Err() }

func (c *Cache) Close() error { return c.client.Close() }
