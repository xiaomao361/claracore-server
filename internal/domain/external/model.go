package external

import (
	"errors"
	"fmt"
	"strings"
	"time"

	"claracore-server/internal/platform"
)

var ErrSyncBusy = errors.New("external: source sync is already running")

const (
	SourceActive      = "active"
	DocumentActive    = "active"
	DocumentMissing   = "missing"
	SyncRunning       = "running"
	SyncSucceeded     = "succeeded"
	SyncFailed        = "failed"
	SyncNever         = "never"
	MaxIndexTextRunes = 2000
	MaxSyncDocuments  = 100_000
	MaxSyncErrorLen   = 512
)

type MirrorSource struct {
	ID        platform.ID
	TenantID  platform.ID
	SpaceID   platform.ID
	Kind      string
	Name      string
	System    string
	Status    string
	CreatedAt time.Time
	UpdatedAt time.Time
}

func (s MirrorSource) validate() error {
	if s.ID == "" || s.TenantID == "" || s.SpaceID == "" || strings.TrimSpace(s.Kind) == "" || strings.TrimSpace(s.Name) == "" || strings.TrimSpace(s.System) == "" {
		return fmt.Errorf("external: source identity is incomplete")
	}
	if len([]rune(s.Name)) > 120 || len([]rune(s.System)) > 120 || len(s.Kind) > 32 {
		return fmt.Errorf("external: source fields exceed storage bounds")
	}
	if s.Status != SourceActive {
		return fmt.Errorf("external: unsupported source status %q", s.Status)
	}
	return nil
}

type MirrorDocument struct {
	ID                platform.ID
	TenantID          platform.ID
	SpaceID           platform.ID
	ExternalSourceID  platform.ID
	UpstreamSourceID  string
	ExternalID        string
	Kind              string
	Title             string
	SourceRef         string
	Revision          string
	UpstreamUpdatedAt string
	State             string
	LastSeenRunID     platform.ID
	CreatedAt         time.Time
	UpdatedAt         time.Time
}

func (d MirrorDocument) validate() error {
	if d.ID == "" || d.TenantID == "" || d.SpaceID == "" || d.ExternalSourceID == "" || d.LastSeenRunID == "" ||
		strings.TrimSpace(d.UpstreamSourceID) == "" || strings.TrimSpace(d.ExternalID) == "" || strings.TrimSpace(d.Kind) == "" ||
		strings.TrimSpace(d.Title) == "" || strings.TrimSpace(d.SourceRef) == "" || strings.TrimSpace(d.Revision) == "" {
		return fmt.Errorf("external: document metadata is incomplete")
	}
	if d.State != DocumentActive {
		return fmt.Errorf("external: synchronized document must be active")
	}
	if len(d.UpstreamSourceID) > 255 || len(d.ExternalID) > 255 || len(d.Kind) > 64 || len([]rune(d.Title)) > 500 || len(d.SourceRef) > 2048 || len(d.Revision) > 128 || len(d.UpstreamUpdatedAt) > 64 {
		return fmt.Errorf("external: document fields exceed storage bounds")
	}
	return nil
}

type DocumentVector struct {
	ID                 platform.ID
	TenantID           platform.ID
	ExternalDocumentID platform.ID
	Model              string
	Revision           string
	InputHash          string
	Vector             []float32
	CreatedAt          time.Time
	UpdatedAt          time.Time
}

func (v DocumentVector) validate() error {
	if v.ID == "" || v.TenantID == "" || v.ExternalDocumentID == "" || strings.TrimSpace(v.Model) == "" || strings.TrimSpace(v.Revision) == "" || len(v.InputHash) != 64 || len(v.Vector) == 0 {
		return fmt.Errorf("external: derived vector is incomplete")
	}
	if len(v.Model) > 64 || len(v.Revision) > 128 {
		return fmt.Errorf("external: vector fields exceed storage bounds")
	}
	return nil
}

type IndexState struct {
	DocumentID platform.ID
	Revision   string
	Ready      bool
}

type SyncRun struct {
	ID               platform.ID
	TenantID         platform.ID
	ExternalSourceID platform.ID
	Status           string
	StartedAt        time.Time
	LeaseUntil       time.Time
	CreatedAt        time.Time
}

type SyncRequest struct {
	ID               platform.ID
	TenantID         platform.ID
	ExternalSourceID platform.ID
	RequestedAt      time.Time
}

type SyncRequestClaim struct {
	RequestID        platform.ID
	TenantID         platform.ID
	ExternalSourceID platform.ID
	ClaimToken       platform.ID
	LeaseUntil       time.Time
}

type SyncReport struct {
	SourceID  platform.ID
	RunID     platform.ID
	Seen      int
	Indexed   int
	Unchanged int
	Missing   int
}

// SyncRunReceipt is the body-free operational receipt shown to administrators.
// It contains counts and bounded failure text, never upstream document bodies.
type SyncRunReceipt struct {
	ID               platform.ID
	ExternalSourceID platform.ID
	SourceName       string
	SourceSystem     string
	Status           string
	Seen             int
	Indexed          int
	Unchanged        int
	Missing          int
	ErrorMessage     string
	StartedAt        time.Time
	FinishedAt       *time.Time
}

type SourceHealth struct {
	ID              platform.ID
	SpaceID         platform.ID
	Kind            string
	Name            string
	System          string
	LastSyncStatus  string
	LastStartedAt   *time.Time
	LastSucceededAt *time.Time
	LastError       string
	DocumentCount   int
	SyncRunID       platform.ID
	SyncLeaseUntil  *time.Time
	SyncRequestedAt *time.Time
	SyncClaimed     bool
	SyncClaimUntil  *time.Time
}

// SearchQuery is already permission-resolved by the application layer. Store
// implementations must push TenantID and SpaceIDs into SQL before rows are
// returned; filtering an all-tenant result in memory is a data leak.
type SearchQuery struct {
	TenantID platform.ID
	SpaceIDs []platform.ID
	Query    string
	Model    string
	Limit    int
}

type SearchCandidate struct {
	Document        MirrorDocument
	SourceName      string
	SourceSystem    string
	LastSyncStatus  string
	LastSucceededAt *time.Time
	TitleScore      float64
	Vector          []float32
}

type SearchHit struct {
	Document        MirrorDocument
	SourceName      string
	SourceSystem    string
	LastSyncStatus  string
	LastSucceededAt *time.Time
	Score           float64
}

type SearchInput struct {
	TenantID platform.ID
	SpaceIDs []platform.ID
	Query    string
	Limit    int
}

type SearchResult struct {
	Hits              []SearchHit
	SemanticAvailable bool
}
