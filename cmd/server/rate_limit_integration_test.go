//go:build integration

package main

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"os"
	"path/filepath"
	"testing"
	"time"

	redisadapter "claracore-server/internal/adapters/redis"
	"claracore-server/internal/config"
)

// Two independent clients against the configured Redis prove that the
// aggregate tenant counter is shared by application instances, not merely by
// goroutines inside one process. The unique bucket expires automatically.
func TestConfiguredRedisSharesTenantRateBudgetAcrossClients(t *testing.T) {
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Join(wd, "..", ".."))
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	if !cfg.Redis.Enabled() {
		t.Skip("Redis is not configured; cross-instance rate-limit acceptance requires it")
	}
	tlsConfig, err := loadDependencyTLS(cfg.Redis.TLS)
	if err != nil {
		t.Fatal(err)
	}
	random := make([]byte, 8)
	if _, err := rand.Read(random); err != nil {
		t.Fatal(err)
	}
	prefix := cfg.Redis.Prefix + "rate-it:" + hex.EncodeToString(random) + ":"
	cacheA := redisadapter.NewCache(cfg.Redis.Addr, cfg.Redis.Password, prefix, cfg.Redis.Timeout, tlsConfig)
	cacheB := redisadapter.NewCache(cfg.Redis.Addr, cfg.Redis.Password, prefix, cfg.Redis.Timeout, tlsConfig)
	t.Cleanup(func() { _ = cacheA.Close() })
	t.Cleanup(func() { _ = cacheB.Close() })

	ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if err := cacheA.Ping(ctx); err != nil {
		t.Fatalf("configured Redis is unavailable: %v", err)
	}
	window := 10 * time.Second
	key := "rate:mcp:tenant:integration"
	// Fixed windows are aligned to wall-clock boundaries. Do not begin the
	// four-call assertion at the tail of one, otherwise a legitimate rollover
	// can make the fourth call look like unshared state.
	probe, err := cacheA.Allow(ctx, key+":boundary-probe", 1, window)
	if err != nil {
		t.Fatal(err)
	}
	if probe.RetryAfter < 2*time.Second {
		timer := time.NewTimer(probe.RetryAfter + 50*time.Millisecond)
		defer timer.Stop()
		select {
		case <-ctx.Done():
			t.Fatal(ctx.Err())
		case <-timer.C:
		}
	}
	for i, cache := range []*redisadapter.Cache{cacheA, cacheB, cacheA} {
		result, err := cache.Allow(ctx, key, 3, window)
		if err != nil || !result.Allowed {
			t.Fatalf("shared attempt %d should pass: result=%+v err=%v", i+1, result, err)
		}
	}
	blocked, err := cacheB.Allow(ctx, key, 3, window)
	if err != nil || blocked.Allowed || blocked.RetryAfter <= 0 {
		t.Fatalf("second configured client must observe the shared tenant budget: result=%+v err=%v", blocked, err)
	}
}
