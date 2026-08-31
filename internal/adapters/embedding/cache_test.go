package embedding

import (
	"context"
	"crypto/sha256"
	"errors"
	"sync"
	"testing"
	"time"
)

type fakeSharedCache struct {
	mu      sync.Mutex
	values  map[string][]byte
	getErr  error
	setErr  error
	getCall int
	setCall int
}

func newFakeSharedCache() *fakeSharedCache {
	return &fakeSharedCache{values: make(map[string][]byte)}
}

func (c *fakeSharedCache) GetMany(_ context.Context, keys []string) (map[string][]byte, error) {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.getCall++
	if c.getErr != nil {
		return nil, c.getErr
	}
	out := make(map[string][]byte, len(keys))
	for _, key := range keys {
		if value, ok := c.values[key]; ok {
			out[key] = append([]byte(nil), value...)
		}
	}
	return out, nil
}

func (c *fakeSharedCache) SetMany(_ context.Context, values map[string][]byte, _ time.Duration) error {
	c.mu.Lock()
	defer c.mu.Unlock()
	c.setCall++
	if c.setErr != nil {
		return c.setErr
	}
	for key, value := range values {
		c.values[key] = append([]byte(nil), value...)
	}
	return nil
}

type countingEmbedder struct {
	mu    sync.Mutex
	calls int
}

func (e *countingEmbedder) Model() string { return "test-model" }
func (e *countingEmbedder) Embed(_ context.Context, texts []string) ([][]float32, error) {
	e.mu.Lock()
	e.calls++
	e.mu.Unlock()
	out := make([][]float32, len(texts))
	for i, text := range texts {
		out[i] = []float32{float32(len(text)), float32(i + 1)}
	}
	return out, nil
}

func TestCachedEmbedderHitsAndCopiesVectors(t *testing.T) {
	next := &countingEmbedder{}
	cache := NewCached(next, 2, time.Hour)
	first, err := cache.Embed(context.Background(), []string{"hello"})
	if err != nil {
		t.Fatal(err)
	}
	first[0][0] = 999
	second, err := cache.Embed(context.Background(), []string{"hello"})
	if err != nil {
		t.Fatal(err)
	}
	if next.calls != 1 || second[0][0] != 5 {
		t.Fatalf("calls=%d vector=%v", next.calls, second)
	}
	if hits, misses := cache.Stats(); hits != 1 || misses != 1 {
		t.Fatalf("hits=%d misses=%d", hits, misses)
	}
}

func TestCachedEmbedderEvictsAndExpires(t *testing.T) {
	next := &countingEmbedder{}
	cache := NewCached(next, 1, time.Millisecond)
	if _, err := cache.Embed(context.Background(), []string{"a", "b"}); err != nil {
		t.Fatal(err)
	}
	if _, err := cache.Embed(context.Background(), []string{"a"}); err != nil {
		t.Fatal(err)
	}
	time.Sleep(2 * time.Millisecond)
	if _, err := cache.Embed(context.Background(), []string{"a"}); err != nil {
		t.Fatal(err)
	}
	if next.calls != 3 {
		t.Fatalf("淘汰和过期后应重新计算，calls=%d", next.calls)
	}
}

func TestCachedEmbedderCoalescesConcurrentQueries(t *testing.T) {
	next := &countingEmbedder{}
	cache := NewCached(next, 8, time.Hour)
	start := make(chan struct{})
	var wg sync.WaitGroup
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			<-start
			if _, err := cache.Embed(context.Background(), []string{"same"}); err != nil {
				t.Error(err)
			}
		}()
	}
	close(start)
	wg.Wait()
	if next.calls != 1 {
		t.Fatalf("并发相同 query 应合并为一次，calls=%d", next.calls)
	}
}

func TestTieredCachedSharesAcrossInstances(t *testing.T) {
	shared := newFakeSharedCache()
	firstProvider := &countingEmbedder{}
	first := NewTieredCached(firstProvider, 0, time.Hour, shared)
	if _, err := first.Embed(context.Background(), []string{"shared"}); err != nil {
		t.Fatal(err)
	}

	secondProvider := &countingEmbedder{}
	second := NewTieredCached(secondProvider, 0, time.Hour, shared)
	got, err := second.Embed(context.Background(), []string{"shared"})
	if err != nil {
		t.Fatal(err)
	}
	if secondProvider.calls != 0 || got[0][0] != 6 {
		t.Fatalf("第二实例应命中共享缓存，calls=%d vector=%v", secondProvider.calls, got)
	}
	if hits, misses := second.Stats(); hits != 1 || misses != 0 {
		t.Fatalf("hits=%d misses=%d", hits, misses)
	}
}

func TestTieredCachedFallsBackWhenSharedCacheFails(t *testing.T) {
	shared := newFakeSharedCache()
	shared.getErr = errors.New("redis unavailable")
	shared.setErr = errors.New("redis unavailable")
	next := &countingEmbedder{}
	cache := NewTieredCached(next, 2, time.Hour, shared)

	first, err := cache.Embed(context.Background(), []string{"fallback"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := cache.Embed(context.Background(), []string{"fallback"})
	if err != nil {
		t.Fatal(err)
	}
	if next.calls != 1 || first[0][0] != second[0][0] {
		t.Fatalf("共享缓存故障不应影响本地缓存与 provider，calls=%d", next.calls)
	}
}

func TestTieredCachedTreatsCorruptSharedVectorAsMiss(t *testing.T) {
	shared := newFakeSharedCache()
	next := &countingEmbedder{}
	key := sharedKey(sha256Key(next.Model(), "broken"))
	shared.values[key] = []byte("not-a-vector")
	cache := NewTieredCached(next, 0, time.Hour, shared)

	got, err := cache.Embed(context.Background(), []string{"broken"})
	if err != nil {
		t.Fatal(err)
	}
	if next.calls != 1 || got[0][0] != 6 {
		t.Fatalf("损坏缓存应按 miss 重算，calls=%d vector=%v", next.calls, got)
	}
}

func sha256Key(model, text string) cacheKey {
	return cacheKey(sha256.Sum256([]byte(model + "\x00" + text)))
}
