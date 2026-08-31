package ports

import (
	"context"
	"time"

	"claracore-server/internal/platform"
)

// TenantCapacitySnapshot is an exact, tenant-scoped logical-capacity snapshot.
// Logical bytes count application payloads stored in MySQL; they deliberately
// exclude indexes, row overhead, backups, binary logs, and InnoDB free space.
// The snapshot contains no content, entity identifiers, or actor identifiers.
type TenantCapacitySnapshot struct {
	MeasuredAt time.Time

	Members int64
	Agents  int64
	Spaces  int64

	Memories              int64
	Cards                 int64
	RecallTriggers        int64
	Labels                int64
	Links                 int64
	KnowledgeLogicalBytes int64

	ContinuityLines        int64
	ContinuityPositions    int64
	ContinuityHistory      int64
	ContinuityHandoffs     int64
	ContinuityLogicalBytes int64

	Embeddings            int64
	EmbeddingLogicalBytes int64

	ExternalSources      int64
	ExternalSyncRuns     int64
	ExternalDocuments    int64
	ExternalVectors      int64
	ExternalLogicalBytes int64

	AuditEvents       int64
	AuditLogicalBytes int64
}

func (s TenantCapacitySnapshot) KnowledgeRows() int64 {
	return s.Memories + s.Cards + s.RecallTriggers + s.Labels + s.Links
}

func (s TenantCapacitySnapshot) ContinuityRows() int64 {
	return s.ContinuityLines + s.ContinuityPositions + s.ContinuityHistory + s.ContinuityHandoffs
}

func (s TenantCapacitySnapshot) ExternalRows() int64 {
	return s.ExternalSources + s.ExternalSyncRuns + s.ExternalDocuments + s.ExternalVectors
}

func (s TenantCapacitySnapshot) TotalLogicalBytes() int64 {
	return s.KnowledgeLogicalBytes + s.ContinuityLogicalBytes + s.EmbeddingLogicalBytes + s.ExternalLogicalBytes + s.AuditLogicalBytes
}

// TenantCapacityReader performs an explicit administrative diagnostic. It is
// intentionally separate from the high-frequency metrics scrape path.
type TenantCapacityReader interface {
	ReadTenantCapacity(context.Context, platform.ID) (TenantCapacitySnapshot, error)
}
