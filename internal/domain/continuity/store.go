package continuity

import (
	"context"
	"time"

	"claracore-server/internal/platform"
)

type ListQuery struct {
	TenantID       platform.ID
	OwnerAgent     platform.ID
	AllAgents      bool
	Status         string
	Limit          int
	AfterUpdatedAt time.Time
	AfterID        platform.ID
}

type Store interface {
	ListLines(ctx context.Context, q ListQuery) ([]Line, error)
	FindLine(ctx context.Context, tenantID, lineID platform.ID, activeOnly bool) (*Line, error)
	CreateLine(ctx context.Context, line Line) error
	UpdateLineStatus(ctx context.Context, tenantID, lineID platform.ID, status string, at time.Time) error

	FindPosition(ctx context.Context, tenantID, lineID platform.ID) (*Position, error)
	CreatePosition(ctx context.Context, p Position) error
	UpdatePosition(ctx context.Context, p Position, expectedVersion int) error
	AppendPositionHistory(ctx context.Context, historyID platform.ID, p Position, at time.Time) error
	ListPositionHistory(ctx context.Context, tenantID, lineID platform.ID, limit int) ([]Position, error)

	LatestHandoff(ctx context.Context, tenantID, lineID platform.ID) (*Handoff, error)
	CreateHandoff(ctx context.Context, h Handoff) error
	ListHandoffs(ctx context.Context, tenantID, lineID platform.ID, limit int) ([]Handoff, error)

	Atomically(ctx context.Context, fn func(Store) error) error
}
