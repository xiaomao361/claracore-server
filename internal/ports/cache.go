package ports

import (
	"context"
	"time"
)

// Cache is the optional shared byte cache used by infrastructure decorators.
// Implementations must treat a missing key as a normal result, not an error.
// Domain packages deliberately do not depend on it.
type Cache interface {
	GetMany(ctx context.Context, keys []string) (map[string][]byte, error)
	SetMany(ctx context.Context, values map[string][]byte, ttl time.Duration) error
}
