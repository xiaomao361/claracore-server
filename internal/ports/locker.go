package ports

import (
	"context"
	"time"
)

// Locker is an optional cross-process lease used for short infrastructure
// jobs. A false acquisition is normal contention, not an error.
type Locker interface {
	TryLock(ctx context.Context, key string, ttl time.Duration) (Lease, bool, error)
}

// Lease must only release the lock token it acquired. Release is idempotent.
type Lease interface {
	Release(ctx context.Context) error
}
