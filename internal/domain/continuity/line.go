// Package continuity owns the team Server's Shared Line domain.
//
// A Shared Line is a resumable current position, not a durable fact store.
// Facts belong in Memoria; the line may only keep their opaque IDs in FactsUsed.
package continuity

import (
	"fmt"
	"strings"
	"time"

	"claracore-server/internal/platform"
)

const (
	LineActive   = "active"
	LineArchived = "archived"

	PositionDraft       = "draft"
	PositionConfirmed   = "confirmed"
	PositionActive      = "active"
	PositionNeedsReview = "needs_review"
	PositionStale       = "stale"
	PositionClosed      = "closed"

	MaxTitleLen    = 200
	MaxSummaryLen  = 12000
	MaxNextStepLen = 4000
	MaxFactsUsed   = 50
	MaxListLimit   = 100
)

type Line struct {
	ID           platform.ID
	TenantID     platform.ID
	OwnerUserID  platform.ID
	OwnerAgentID platform.ID
	Title        string
	Status       string
	CreatedAt    time.Time
	UpdatedAt    time.Time
	Position     *Position
}

type Position struct {
	ID                   platform.ID
	TenantID             platform.ID
	LineID               platform.ID
	Summary              string
	InterpretationStatus string
	FactsUsed            []platform.ID
	NextStep             string
	WriterUserID         platform.ID
	WriterAgentID        platform.ID
	Version              int
	UpdatedAt            time.Time
}

type Handoff struct {
	ID            platform.ID
	TenantID      platform.ID
	LineID        platform.ID
	WriterUserID  platform.ID
	WriterAgentID platform.ID
	Objective     string
	Completed     []string
	OpenItems     []string
	NextStep      string
	CreatedAt     time.Time
}

type LineSummary struct {
	ID                   platform.ID
	OwnerAgentID         platform.ID
	Title                string
	Status               string
	Summary              string
	InterpretationStatus string
	NextStep             string
	UpdatedAt            time.Time
}

// LineStats is the tenant-wide dashboard view of Shared Line lifecycle state.
// NeedsReview and Stale are active-line subsets, not additional lines.
type LineStats struct {
	Active      int
	Archived    int
	NeedsReview int
	Stale       int
}

type ResumePacket struct {
	Line            Line
	RecentHandoff   *Handoff
	PositionHistory []Position
	HandoffHistory  []Handoff
}

func validPositionStatus(s string) bool {
	switch s {
	case PositionDraft, PositionConfirmed, PositionActive, PositionNeedsReview, PositionStale, PositionClosed:
		return true
	}
	return false
}

func normalizeText(raw string, max int, field string, required bool) (string, error) {
	raw = strings.TrimSpace(raw)
	if required && raw == "" {
		return "", fmt.Errorf("%w: 缺少%s", ErrInvalidArgument, field)
	}
	if len([]rune(raw)) > max {
		return "", fmt.Errorf("%w: %s超过 %d 字", ErrInvalidArgument, field, max)
	}
	return raw, nil
}

func normalizeFacts(raw []platform.ID) ([]platform.ID, error) {
	if len(raw) > MaxFactsUsed {
		return nil, fmt.Errorf("%w: factsUsed 超过 %d 条", ErrInvalidArgument, MaxFactsUsed)
	}
	seen := make(map[platform.ID]bool, len(raw))
	out := make([]platform.ID, 0, len(raw))
	for _, id := range raw {
		if id == "" {
			return nil, fmt.Errorf("%w: factsUsed 含空 ID", ErrInvalidArgument)
		}
		if !seen[id] {
			seen[id] = true
			out = append(out, id)
		}
	}
	return out, nil
}

func normalizeStringList(raw []string, maxItems, maxItemLen int, field string) ([]string, error) {
	if len(raw) > maxItems {
		return nil, fmt.Errorf("%w: %s 超过 %d 条", ErrInvalidArgument, field, maxItems)
	}
	out := make([]string, 0, len(raw))
	for _, value := range raw {
		value = strings.TrimSpace(value)
		if value == "" {
			continue
		}
		if len([]rune(value)) > maxItemLen {
			return nil, fmt.Errorf("%w: %s 单项超过 %d 字", ErrInvalidArgument, field, maxItemLen)
		}
		out = append(out, value)
	}
	return out, nil
}
