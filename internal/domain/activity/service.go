package activity

import (
	"bytes"
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"claracore-server/internal/platform"
)

const (
	defaultListLimit   = 30
	maxListLimit       = 100
	defaultDeleteLimit = 1000
	maxDeleteLimit     = 10000
)

type Service struct {
	store Store
	clock platform.Clock
	ids   *platform.IDGenerator
}

func NewService(store Store, clock platform.Clock, ids *platform.IDGenerator) *Service {
	return &Service{store: store, clock: clock, ids: ids}
}

func (s *Service) Record(ctx context.Context, in RecordInput) (Event, error) {
	metadata, err := normalizeMetadata(in.Metadata)
	if err != nil {
		return Event{}, err
	}
	agentID, _ := normalizeStoredID(in.AgentID)
	entityID, entityDropped := normalizeStoredID(in.EntityID)
	spaceID, _ := normalizeStoredID(in.SpaceID)
	entityType := strings.TrimSpace(in.EntityType)
	if entityDropped {
		// Entity IDs can originate in failed MCP/Web inputs. A malformed target
		// must not make the audit INSERT fail; retain the action/outcome and drop
		// only the untrusted optional target.
		entityType = ""
	}

	event := Event{
		ID:         s.ids.New(),
		TenantID:   in.TenantID,
		UserID:     in.UserID,
		AgentID:    agentID,
		Source:     strings.TrimSpace(in.Source),
		Kind:       strings.TrimSpace(in.Kind),
		Access:     strings.TrimSpace(in.Access),
		Outcome:    strings.TrimSpace(in.Outcome),
		Action:     strings.TrimSpace(in.Action),
		EntityType: entityType,
		EntityID:   entityID,
		SpaceID:    spaceID,
		Metadata:   metadata,
		CreatedAt:  s.clock.Now().UTC(),
	}
	if err := event.validate(); err != nil {
		return Event{}, err
	}
	if err := s.store.Record(ctx, event); err != nil {
		return Event{}, err
	}
	return event, nil
}

func (s *Service) ListVisible(ctx context.Context, in ListVisibleInput) (Page, error) {
	if in.TenantID == "" || in.UserID == "" {
		return Page{}, fmt.Errorf("%w: 缺少租户或用户", ErrInvalidArgument)
	}
	spaces, err := normalizeSpaceIDs(in.ReadableSharedSpaceIDs)
	if err != nil {
		return Page{}, err
	}
	if err := validateListFilters(in.Action, in.Outcome); err != nil {
		return Page{}, err
	}

	limit := normalizedLimit(in.Limit)
	query := VisibleQuery{
		TenantID:               in.TenantID,
		UserID:                 in.UserID,
		ReadableSharedSpaceIDs: spaces,
		Limit:                  limit + 1,
		Action:                 strings.TrimSpace(in.Action),
		Outcome:                strings.TrimSpace(in.Outcome),
		AgentID:                in.AgentID,
		SpaceID:                in.SpaceID,
		Since:                  in.Since.UTC(),
	}
	if in.Cursor != "" {
		query.AfterCreatedAt, query.AfterID, err = decodeCursor(in.Cursor)
		if err != nil {
			return Page{}, err
		}
	}

	events, err := s.store.ListVisible(ctx, query)
	if err != nil {
		return Page{}, err
	}
	return makePage(events, limit), nil
}

func (s *Service) ListAdmin(ctx context.Context, in ListAdminInput) (Page, error) {
	if in.TenantID == "" {
		return Page{}, fmt.Errorf("%w: 缺少租户", ErrInvalidArgument)
	}

	if err := validateListFilters(in.Action, in.Outcome); err != nil {
		return Page{}, err
	}
	limit := normalizedLimit(in.Limit)
	query := AdminQuery{
		TenantID: in.TenantID, Limit: limit + 1,
		Action: strings.TrimSpace(in.Action), Outcome: strings.TrimSpace(in.Outcome),
		AgentID: in.AgentID, SpaceID: in.SpaceID,
		Since: in.Since.UTC(),
	}
	var err error
	if in.Cursor != "" {
		query.AfterCreatedAt, query.AfterID, err = decodeCursor(in.Cursor)
		if err != nil {
			return Page{}, err
		}
	}

	events, err := s.store.ListAdmin(ctx, query)
	if err != nil {
		return Page{}, err
	}
	return makePage(events, limit), nil
}

func validateListFilters(action, outcome string) error {
	action = strings.TrimSpace(action)
	if utf8.RuneCountInString(action) > 120 {
		return fmt.Errorf("%w: 动作筛选超过 120 字", ErrInvalidArgument)
	}
	outcome = strings.TrimSpace(outcome)
	if outcome != "" && outcome != OutcomeSuccess && outcome != OutcomeError {
		return fmt.Errorf("%w: 结果筛选只能是 success 或 error", ErrInvalidArgument)
	}
	return nil
}

// ContextFeedbackQuality returns aggregate submitted feedback since the given
// instant. Governance rows are retained independently from short-lived traces.
func (s *Service) ContextFeedbackQuality(ctx context.Context, tenantID platform.ID, window time.Duration) (ContextFeedbackQuality, error) {
	if tenantID == "" || window <= 0 {
		return ContextFeedbackQuality{}, fmt.Errorf("%w: 缺少租户或统计窗口", ErrInvalidArgument)
	}
	since := s.clock.Now().UTC().Add(-window)
	return s.store.CountContextFeedback(ctx, ContextFeedbackQuery{
		TenantID: tenantID,
		Since:    since,
	})
}

// DeleteTracesBefore removes one bounded batch of expired trace rows.
// Governance and meta-audit rows are never eligible for this path.
func (s *Service) DeleteTracesBefore(ctx context.Context, before time.Time, limit int) (int, error) {
	if before.IsZero() {
		return 0, fmt.Errorf("%w: 缺少清理截止时间", ErrInvalidArgument)
	}
	switch {
	case limit <= 0:
		limit = defaultDeleteLimit
	case limit > maxDeleteLimit:
		limit = maxDeleteLimit
	}
	return s.store.DeleteTracesBefore(ctx, before.UTC(), limit)
}

func (e *Event) validate() error {
	if e.ID == "" || e.TenantID == "" || e.UserID == "" {
		return fmt.Errorf("%w: 缺少 ID、租户或用户", ErrInvalidArgument)
	}
	if e.CreatedAt.IsZero() {
		return fmt.Errorf("%w: 缺少创建时间", ErrInvalidArgument)
	}
	if e.AgentID != nil && *e.AgentID == "" {
		return fmt.Errorf("%w: agent ID 为空", ErrInvalidArgument)
	}
	if e.SpaceID != nil && *e.SpaceID == "" {
		return fmt.Errorf("%w: 空间 ID 为空", ErrInvalidArgument)
	}

	switch e.Source {
	case SourceMCP, SourceWeb:
	default:
		return fmt.Errorf("%w: 来源取值非法 %q", ErrInvalidArgument, e.Source)
	}
	switch e.Kind {
	case KindTrace, KindGovernance, KindMetaAudit:
	default:
		return fmt.Errorf("%w: 类型取值非法 %q", ErrInvalidArgument, e.Kind)
	}
	switch e.Access {
	case AccessRead, AccessWrite:
	default:
		return fmt.Errorf("%w: 访问类型取值非法 %q", ErrInvalidArgument, e.Access)
	}
	switch e.Outcome {
	case OutcomeSuccess, OutcomeError:
	default:
		return fmt.Errorf("%w: 结果取值非法 %q", ErrInvalidArgument, e.Outcome)
	}

	if e.Action == "" {
		return fmt.Errorf("%w: 缺少动作", ErrInvalidArgument)
	}
	if utf8.RuneCountInString(e.Action) > MaxActionLen {
		return fmt.Errorf("%w: 动作超过 %d 字", ErrInvalidArgument, MaxActionLen)
	}
	if utf8.RuneCountInString(e.EntityType) > MaxEntityTypeLen {
		return fmt.Errorf("%w: 实体类型超过 %d 字", ErrInvalidArgument, MaxEntityTypeLen)
	}
	if (e.EntityType == "") != (e.EntityID == nil) {
		return fmt.Errorf("%w: 实体类型与实体 ID 必须同时提供", ErrInvalidArgument)
	}
	if e.EntityID != nil && *e.EntityID == "" {
		return fmt.Errorf("%w: 实体 ID 为空", ErrInvalidArgument)
	}
	return nil
}

func normalizeMetadata(raw json.RawMessage) (json.RawMessage, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return nil, nil
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return nil, fmt.Errorf("%w: metadata 不是合法 JSON", ErrInvalidArgument)
	}
	if compact.Len() > MaxMetadataBytes {
		// Metadata is optional. Dropping an oversized supplement is safer than
		// losing the immutable action/outcome event to a storage error.
		return nil, nil
	}
	return json.RawMessage(bytes.Clone(compact.Bytes())), nil
}

func normalizeSpaceIDs(raw []platform.ID) ([]platform.ID, error) {
	if len(raw) == 0 {
		return nil, nil
	}
	seen := make(map[platform.ID]struct{}, len(raw))
	out := make([]platform.ID, 0, len(raw))
	for _, id := range raw {
		if id == "" {
			return nil, fmt.Errorf("%w: 可读共享空间 ID 为空", ErrInvalidArgument)
		}
		if _, ok := seen[id]; ok {
			continue
		}
		seen[id] = struct{}{}
		out = append(out, id)
	}
	return out, nil
}

func normalizedLimit(limit int) int {
	switch {
	case limit <= 0:
		return defaultListLimit
	case limit > maxListLimit:
		return maxListLimit
	default:
		return limit
	}
}

func makePage(events []Event, limit int) Page {
	page := Page{Events: events}
	if len(events) <= limit {
		return page
	}
	last := events[limit-1]
	page.NextCursor = encodeCursor(last.CreatedAt, last.ID)
	page.Events = events[:limit]
	return page
}

func encodeCursor(createdAt time.Time, id platform.ID) string {
	raw := strconv.FormatInt(createdAt.UTC().UnixMilli(), 10) + ":" + string(id)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(cursor string) (time.Time, platform.ID, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%w: 游标格式非法", ErrInvalidArgument)
	}
	millis, rawID, ok := strings.Cut(string(raw), ":")
	if !ok || rawID == "" {
		return time.Time{}, "", fmt.Errorf("%w: 游标格式非法", ErrInvalidArgument)
	}
	n, err := strconv.ParseInt(millis, 10, 64)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%w: 游标格式非法", ErrInvalidArgument)
	}
	return time.UnixMilli(n).UTC(), platform.ID(rawID), nil
}

func cloneID(id *platform.ID) *platform.ID {
	if id == nil {
		return nil
	}
	copy := *id
	return &copy
}

// normalizeStoredID protects the fixed-width ASCII audit columns from optional
// IDs copied out of failed protocol/form input. Empty pointers remain intact so
// normal validation still catches programming mistakes; other malformed values
// are dropped while the primary action/outcome is still recorded.
func normalizeStoredID(id *platform.ID) (*platform.ID, bool) {
	if id == nil {
		return nil, false
	}
	if *id == "" {
		return cloneID(id), false
	}
	if len(*id) != 26 {
		return nil, true
	}
	for i := 0; i < len(*id); i++ {
		c := (*id)[i]
		if (c < '0' || c > '9') && (c < 'A' || c > 'Z') {
			return nil, true
		}
	}
	return cloneID(id), false
}
