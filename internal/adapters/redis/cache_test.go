package redis

import (
	"context"
	"crypto/tls"
	"crypto/x509"
	"net/http/httptest"
	"testing"
	"time"

	miniredis "github.com/alicebob/miniredis/v2"
)

func TestCacheNegotiatesVerifiedTLS(t *testing.T) {
	fixture := httptest.NewTLSServer(nil)
	serverTLS := fixture.TLS.Clone()
	certificate := fixture.Certificate()
	fixture.Close()

	server, err := miniredis.RunTLS(serverTLS)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(server.Close)
	roots := x509.NewCertPool()
	roots.AddCert(certificate)
	clientTLS := &tls.Config{MinVersion: tls.VersionTLS12, RootCAs: roots, ServerName: "example.com"}
	cache := NewCache(server.Addr(), "", "ccs:tls:", 500*time.Millisecond, clientTLS)
	t.Cleanup(func() { _ = cache.Close() })

	if err := cache.Ping(context.Background()); err != nil {
		t.Fatalf("verified TLS Redis ping failed: %v", err)
	}
	if err := cache.SetMany(context.Background(), map[string][]byte{"key": []byte("value")}, time.Minute); err != nil {
		t.Fatal(err)
	}
	got, err := cache.GetMany(context.Background(), []string{"key"})
	if err != nil || string(got["key"]) != "value" {
		t.Fatalf("TLS Redis round trip failed: got=%q err=%v", got["key"], err)
	}
}

func TestCacheNamespacesBatchesAndExpires(t *testing.T) {
	server := miniredis.RunT(t)
	cache := NewCache(server.Addr(), "", "ccs:test:", 100*time.Millisecond)
	t.Cleanup(func() { _ = cache.Close() })

	ctx := context.Background()
	if err := cache.SetMany(ctx, map[string][]byte{"a": {1, 2}, "b": {3, 4}}, time.Minute); err != nil {
		t.Fatal(err)
	}
	if !server.Exists("ccs:test:a") || server.Exists("a") {
		t.Fatal("Redis key 必须带部署前缀")
	}
	got, err := cache.GetMany(ctx, []string{"a", "missing", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if string(got["a"]) != string([]byte{1, 2}) || string(got["b"]) != string([]byte{3, 4}) {
		t.Fatalf("批量读回不一致: %#v", got)
	}

	server.FastForward(time.Minute + time.Second)
	got, err = cache.GetMany(ctx, []string{"a", "b"})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("TTL 后应为空: %#v", got)
	}
}

func TestLockerUsesOwnedExpiringLease(t *testing.T) {
	server := miniredis.RunT(t)
	cache := NewCache(server.Addr(), "", "ccs:test:", 100*time.Millisecond)
	t.Cleanup(func() { _ = cache.Close() })
	ctx := context.Background()

	first, acquired, err := cache.TryLock(ctx, "lock:indexer:model", time.Minute)
	if err != nil || !acquired {
		t.Fatalf("首次租约应成功: acquired=%v err=%v", acquired, err)
	}
	if _, acquired, err := cache.TryLock(ctx, "lock:indexer:model", time.Minute); err != nil || acquired {
		t.Fatalf("租约存续时其他实例不得取得: acquired=%v err=%v", acquired, err)
	}
	if err := first.Release(ctx); err != nil {
		t.Fatal(err)
	}
	second, acquired, err := cache.TryLock(ctx, "lock:indexer:model", time.Minute)
	if err != nil || !acquired {
		t.Fatalf("释放后应可重新取得: acquired=%v err=%v", acquired, err)
	}
	if err := first.Release(ctx); err != nil {
		t.Fatal(err)
	}
	if _, acquired, err := cache.TryLock(ctx, "lock:indexer:model", time.Minute); err != nil || acquired {
		t.Fatalf("旧租约重复释放不得删除新租约: acquired=%v err=%v", acquired, err)
	}
	if err := second.Release(ctx); err != nil {
		t.Fatal(err)
	}
}

func TestRateLimiterIsAtomicNamespacedAndExpiring(t *testing.T) {
	server := miniredis.RunT(t)
	cache := NewCache(server.Addr(), "", "ccs:test:", 100*time.Millisecond)
	t.Cleanup(func() { _ = cache.Close() })
	ctx := context.Background()

	for i := 0; i < 2; i++ {
		result, err := cache.Allow(ctx, "rate:login:hashed", 2, time.Minute)
		if err != nil || !result.Allowed {
			t.Fatalf("attempt %d should pass: result=%+v err=%v", i+1, result, err)
		}
	}
	blocked, err := cache.Allow(ctx, "rate:login:hashed", 2, time.Minute)
	if err != nil || blocked.Allowed || blocked.RetryAfter <= 0 || blocked.RetryAfter > time.Minute {
		t.Fatalf("third attempt should be blocked: result=%+v err=%v", blocked, err)
	}
	keys := server.Keys()
	prefix := "ccs:test:rate:login:hashed:"
	if len(keys) != 1 || len(keys[0]) < len(prefix) || keys[0][:len(prefix)] != prefix {
		t.Fatalf("rate key should be namespaced and bucketed: %v", keys)
	}
	if ttl := server.TTL(keys[0]); ttl <= 0 || ttl > 2*time.Minute {
		t.Fatalf("rate key must expire: ttl=%s", ttl)
	}
}

func TestTwoRedisClientsShareTenantRateBudget(t *testing.T) {
	server := miniredis.RunT(t)
	cacheA := NewCache(server.Addr(), "", "ccs:tenant-rate:", 100*time.Millisecond)
	cacheB := NewCache(server.Addr(), "", "ccs:tenant-rate:", 100*time.Millisecond)
	t.Cleanup(func() { _ = cacheA.Close() })
	t.Cleanup(func() { _ = cacheB.Close() })
	ctx := context.Background()
	key := "rate:mcp:tenant:hashed-tenant"

	for i, cache := range []*Cache{cacheA, cacheB, cacheA} {
		result, err := cache.Allow(ctx, key, 3, time.Minute)
		if err != nil || !result.Allowed {
			t.Fatalf("shared attempt %d should pass: result=%+v err=%v", i+1, result, err)
		}
	}
	blocked, err := cacheB.Allow(ctx, key, 3, time.Minute)
	if err != nil || blocked.Allowed || blocked.RetryAfter <= 0 {
		t.Fatalf("second client must observe the shared tenant budget: result=%+v err=%v", blocked, err)
	}
}
