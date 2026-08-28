// Package activity records and lists the observable activity of authenticated
// ClaraCore users and agents.
//
// The package owns classification, validation, pagination, and the visibility
// query contract. It does not know how space permissions are resolved: callers
// pass the already-resolved readable shared-space IDs to ListVisible.
package activity

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"claracore-server/internal/platform"
)

const (
	SourceMCP = "mcp"
	SourceWeb = "web"

	KindTrace      = "trace"
	KindGovernance = "governance"
	KindMetaAudit  = "meta_audit"

	AccessRead  = "read"
	AccessWrite = "write"

	OutcomeSuccess = "success"
	OutcomeError   = "error"
)

const (
	MaxActionLen     = 64
	MaxEntityTypeLen = 24
	// Metadata is supplemental and must never be able to crowd out the primary
	// action/outcome record. Hosts use a much smaller allowlist in practice.
	MaxMetadataBytes = 8 * 1024
)

var ErrInvalidArgument = errors.New("activity: 参数非法")

// Event is one immutable audit/activity record.
//
// UserID is required because the first version only records authenticated
// actions. AgentID is absent for browser governance actions. Entity and space
// are optional dimensions: list/search operations often have no single target.
type Event struct {
	ID       platform.ID
	TenantID platform.ID
	UserID   platform.ID
	AgentID  *platform.ID

	Source  string
	Kind    string
	Access  string
	Outcome string
	Action  string

	EntityType string
	EntityID   *platform.ID
	SpaceID    *platform.ID
	Metadata   json.RawMessage

	CreatedAt time.Time
}

// RecordInput is the host-facing shape for recording an event.
type RecordInput struct {
	TenantID platform.ID
	UserID   platform.ID
	AgentID  *platform.ID

	Source  string
	Kind    string
	Access  string
	Outcome string
	Action  string

	EntityType string
	EntityID   *platform.ID
	SpaceID    *platform.ID
	Metadata   json.RawMessage
}

// ListVisibleInput lists the activity visible to one ordinary user.
//
// ReadableSharedSpaceIDs must already have been resolved by governance. The
// activity domain intentionally does not import governance or duplicate its
// permission rules.
type ListVisibleInput struct {
	TenantID               platform.ID
	UserID                 platform.ID
	ReadableSharedSpaceIDs []platform.ID
	Cursor                 string
	Limit                  int
}

// ListAdminInput lists the full tenant-wide audit stream.
// Authorization and the required meta-audit record belong to the host layer.
type ListAdminInput struct {
	TenantID platform.ID
	Cursor   string
	Limit    int
}

type Page struct {
	Events     []Event
	NextCursor string
}

// VisibleQuery is the persistence contract for ordinary-user visibility.
// Store implementations must apply the visibility predicate in the query,
// before any row enters application memory.
type VisibleQuery struct {
	TenantID               platform.ID
	UserID                 platform.ID
	ReadableSharedSpaceIDs []platform.ID
	AfterCreatedAt         time.Time
	AfterID                platform.ID
	Limit                  int
}

// AdminQuery is the persistence contract for the tenant-wide audit stream.
type AdminQuery struct {
	TenantID       platform.ID
	AfterCreatedAt time.Time
	AfterID        platform.ID
	Limit          int
}

// ContextFeedbackQuery selects durable automatic-context feedback. Results are
// aggregate-only: callers never receive card IDs, actor IDs, or content.
type ContextFeedbackQuery struct {
	TenantID platform.ID
	Since    time.Time
}

// ContextFeedbackQuality is an outcome histogram, not an accuracy score.
// Missing feedback means "unknown", never "correct".
type ContextFeedbackQuality struct {
	Since     time.Time
	Delivered int
	Used      int
	Ignored   int
	Wrong     int
	Corrected int
}

func (q ContextFeedbackQuality) Total() int {
	return q.Delivered + q.Used + q.Ignored + q.Wrong + q.Corrected
}

// Store is the activity domain's persistence outlet.
type Store interface {
	Record(ctx context.Context, event Event) error
	ListVisible(ctx context.Context, query VisibleQuery) ([]Event, error)
	ListAdmin(ctx context.Context, query AdminQuery) ([]Event, error)
	CountContextFeedback(ctx context.Context, query ContextFeedbackQuery) (ContextFeedbackQuality, error)
	DeleteTracesBefore(ctx context.Context, before time.Time, limit int) (int, error)
}
