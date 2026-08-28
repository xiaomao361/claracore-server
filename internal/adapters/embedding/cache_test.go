package embedding

import (
	"context"
	"sync"
	"testing"
	"time"
)

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
