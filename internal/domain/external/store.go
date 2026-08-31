package external

import (
	"context"
	"time"

	"claracore-server/internal/platform"
)

// Store persists body-free mirror metadata, derived vectors, and sync receipts.
type Store interface {
	EnsureSource(ctx context.Context, source MirrorSource) (MirrorSource, error)
	QueueSyncRequest(ctx context.Context, request SyncRequest) (bool, error)
	ClaimSyncRequest(ctx context.Context, tenantID platform.ID, sourceName string, claimToken platform.ID, leaseUntil, at time.Time) (SyncRequestClaim, bool, error)
	ResolveSyncRequest(ctx context.Context, claim SyncRequestClaim, retry bool, at time.Time) error
	BeginSync(ctx context.Context, run SyncRun) error
	FindIndexState(ctx context.Context, sourceID platform.ID, externalID, model string) (IndexState, bool, error)
	TouchDocument(ctx context.Context, document MirrorDocument) error
	SaveIndexedDocument(ctx context.Context, document MirrorDocument, vector DocumentVector) error
	CompleteSync(ctx context.Context, sourceID, runID platform.ID, report SyncReport, at time.Time) (int, error)
	FailSync(ctx context.Context, sourceID, runID platform.ID, reason string, at time.Time) error
	ListSourceHealth(ctx context.Context, tenantID platform.ID) ([]SourceHealth, error)
	ListSyncRuns(ctx context.Context, tenantID platform.ID, limit int) ([]SyncRunReceipt, error)
	SearchCandidates(ctx context.Context, query SearchQuery) ([]SearchCandidate, error)
}
