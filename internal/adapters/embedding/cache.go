package embedding

import (
	"container/list"
	"context"
	"crypto/sha256"
	"encoding/binary"
	"encoding/hex"
	"errors"
	"math"
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

// Cached is a bounded process-local LRU for text+model embeddings with an
// optional shared byte cache behind it. The shared layer is best effort, so
// the single-binary default remains independent of Redis.
type Cached struct {
	next       ports.Embedder
	maxEntries int
	ttl        time.Duration
	shared     ports.Cache

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
	return newCached(next, maxEntries, ttl, nil)
}

// NewTieredCached adds an optional shared cache behind the process-local LRU.
// A shared-cache failure is deliberately a cache miss: Redis must never turn
// the otherwise working embedding path into an outage.
func NewTieredCached(next ports.Embedder, maxEntries int, ttl time.Duration, shared ports.Cache) *Cached {
	if maxEntries < 0 {
		maxEntries = 0
	}
	return newCached(next, maxEntries, ttl, shared)
}

func newCached(next ports.Embedder, maxEntries int, ttl time.Duration, shared ports.Cache) *Cached {
	if ttl <= 0 {
		ttl = 24 * time.Hour
	}
	return &Cached{next: next, maxEntries: maxEntries, ttl: ttl, shared: shared, lru: list.New(), entries: make(map[cacheKey]*list.Element, maxEntries)}
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
		missing = append(missing, i)
	}
	missing = c.getShared(ctx, texts, out, missing)
	c.misses.Add(uint64(len(missing)))
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
			c.putShared(ctx, map[cacheKey][]float32{key: vectors[0]})
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
	sharedValues := make(map[cacheKey][]float32, len(missing))
	for i, original := range missing {
		key := c.key(texts[original])
		c.put(key, vectors[i])
		sharedValues[key] = vectors[i]
		out[original] = cloneVector(vectors[i])
	}
	c.putShared(ctx, sharedValues)
	return out, nil
}

func (c *Cached) getShared(ctx context.Context, texts []string, out [][]float32, missing []int) []int {
	if c.shared == nil || len(missing) == 0 {
		return missing
	}
	keys := make([]string, len(missing))
	keyByIndex := make([]cacheKey, len(missing))
	for i, original := range missing {
		keyByIndex[i] = c.key(texts[original])
		keys[i] = sharedKey(keyByIndex[i])
	}
	values, err := c.shared.GetMany(ctx, keys)
	if err != nil {
		return missing
	}
	remaining := make([]int, 0, len(missing))
	for i, original := range missing {
		vector, err := decodeVector(values[keys[i]])
		if err != nil {
			remaining = append(remaining, original)
			continue
		}
		c.hits.Add(1)
		c.put(keyByIndex[i], vector)
		out[original] = vector
	}
	return remaining
}

func (c *Cached) putShared(ctx context.Context, vectors map[cacheKey][]float32) {
	if c.shared == nil || len(vectors) == 0 {
		return
	}
	values := make(map[string][]byte, len(vectors))
	for key, vector := range vectors {
		values[sharedKey(key)] = encodeVector(vector)
	}
	// Best effort by design: the local cache and provider result remain valid.
	_ = c.shared.SetMany(ctx, values, c.ttl)
}

func (c *Cached) key(text string) cacheKey {
	return sha256.Sum256([]byte(c.next.Model() + "\x00" + text))
}

func (c *Cached) get(key cacheKey) ([]float32, bool) {
	if c.maxEntries == 0 {
		return nil, false
	}
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
	if c.maxEntries == 0 {
		return
	}
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

func sharedKey(key cacheKey) string { return "embedding:v1:" + hex.EncodeToString(key[:]) }

func encodeVector(vector []float32) []byte {
	encoded := make([]byte, 4+len(vector)*4)
	binary.BigEndian.PutUint32(encoded[:4], uint32(len(vector)))
	for i, value := range vector {
		binary.BigEndian.PutUint32(encoded[4+i*4:], math.Float32bits(value))
	}
	return encoded
}

func decodeVector(encoded []byte) ([]float32, error) {
	if len(encoded) < 4 {
		return nil, errors.New("embedding cache: invalid vector")
	}
	dimension := int(binary.BigEndian.Uint32(encoded[:4]))
	if dimension == 0 || len(encoded) != 4+dimension*4 {
		return nil, errors.New("embedding cache: invalid vector")
	}
	vector := make([]float32, dimension)
	for i := range vector {
		vector[i] = math.Float32frombits(binary.BigEndian.Uint32(encoded[4+i*4:]))
	}
	return vector, nil
}

func cloneVector(vector []float32) []float32 { return append([]float32(nil), vector...) }
