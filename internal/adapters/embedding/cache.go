package embedding

import (
	"container/list"
	"context"
	"crypto/sha256"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sync/singleflight"

	"claracore-server/internal/ports"
)

type cacheKey [sha256.Size]byte

type cacheEntry struct {
	key       cacheKey
	vector    []float32
	expiresAt time.Time
}

// Cached is a bounded process-local LRU for text+model embeddings. It keeps
// the single-binary default independent of Redis while removing repeated
// query calls. Multi-instance deployments may replace this decorator later.
type Cached struct {
	next       ports.Embedder
	maxEntries int
	ttl        time.Duration

	mu      sync.Mutex
	lru     *list.List
	entries map[cacheKey]*list.Element
	group   singleflight.Group
	hits    atomic.Uint64
	misses  atomic.Uint64
}

var _ ports.Embedder = (*Cached)(nil)

func NewCached(next ports.Embedder, maxEntries int, ttl time.Duration) *Cached {
	if maxEntries < 1 {
		maxEntries = 1
	}
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &Cached{next: next, maxEntries: maxEntries, ttl: ttl, lru: list.New(), entries: make(map[cacheKey]*list.Element, maxEntries)}
}

func (c *Cached) Model() string { return c.next.Model() }

func (c *Cached) Stats() (hits, misses uint64) { return c.hits.Load(), c.misses.Load() }

func (c *Cached) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	if len(texts) == 0 {
		return [][]float32{}, nil
	}
	out := make([][]float32, len(texts))
	missing := make([]int, 0, len(texts))
	for i, text := range texts {
		if vector, ok := c.get(c.key(text)); ok {
			c.hits.Add(1)
			out[i] = vector
			continue
		}
		c.misses.Add(1)
		missing = append(missing, i)
	}
	if len(missing) == 0 {
		return out, nil
	}

	// Query paths are single-text and most susceptible to concurrent stampedes.
	// Coalesce them without forcing indexing batches into one request at a time.
	if len(texts) == 1 {
		key := c.key(texts[0])
		value, err, _ := c.group.Do(string(key[:]), func() (any, error) {
			if vector, ok := c.get(key); ok {
				return vector, nil
			}
			vectors, err := c.next.Embed(ctx, texts)
			if err != nil {
				return nil, err
			}
			c.put(key, vectors[0])
			return cloneVector(vectors[0]), nil
		})
		if err != nil {
			return nil, err
		}
		out[0] = cloneVector(value.([]float32))
		return out, nil
	}

	missingTexts := make([]string, len(missing))
	for i, original := range missing {
		missingTexts[i] = texts[original]
	}
	vectors, err := c.next.Embed(ctx, missingTexts)
	if err != nil {
		return nil, err
	}
	for i, original := range missing {
		key := c.key(texts[original])
		c.put(key, vectors[i])
		out[original] = cloneVector(vectors[i])
	}
	return out, nil
}

func (c *Cached) key(text string) cacheKey {
	return sha256.Sum256([]byte(c.next.Model() + "\x00" + text))
}

func (c *Cached) get(key cacheKey) ([]float32, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	element, ok := c.entries[key]
	if !ok {
		return nil, false
	}
	entry := element.Value.(cacheEntry)
	if time.Now().After(entry.expiresAt) {
		c.lru.Remove(element)
		delete(c.entries, key)
		return nil, false
	}
	c.lru.MoveToFront(element)
	return cloneVector(entry.vector), true
}

func (c *Cached) put(key cacheKey, vector []float32) {
	c.mu.Lock()
	defer c.mu.Unlock()
	if element, ok := c.entries[key]; ok {
		element.Value = cacheEntry{key: key, vector: cloneVector(vector), expiresAt: time.Now().Add(c.ttl)}
		c.lru.MoveToFront(element)
		return
	}
	element := c.lru.PushFront(cacheEntry{key: key, vector: cloneVector(vector), expiresAt: time.Now().Add(c.ttl)})
	c.entries[key] = element
	for c.lru.Len() > c.maxEntries {
		oldest := c.lru.Back()
		delete(c.entries, oldest.Value.(cacheEntry).key)
		c.lru.Remove(oldest)
	}
}

func cloneVector(vector []float32) []float32 { return append([]float32(nil), vector...) }
