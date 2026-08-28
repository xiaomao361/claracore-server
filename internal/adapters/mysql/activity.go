package mysql

import (
	"context"
	"database/sql"
	"fmt"
	"strings"
	"time"

	"claracore-server/internal/domain/activity"
	"claracore-server/internal/platform"
)

// ActivityStore is the MySQL implementation of activity.Store.
//
// Its most important rule is that ordinary-user visibility is applied in SQL.
// Reading tenant-wide rows and filtering them in Go would already expose private
// read/recall activity to application memory before the filter runs.
type ActivityStore struct {
	ex sqlExecutor
}

var _ activity.Store = (*ActivityStore)(nil)

func NewActivityStore(db *sql.DB) *ActivityStore {
	return &ActivityStore{ex: db}
}

const activityColumns = `id, tenant_id, user_id, agent_id,
	source, kind, access, outcome, action,
	entity_type, entity_id, space_id, metadata, created_at`

func (s *ActivityStore) Record(ctx context.Context, event activity.Event) error {
	var metadata any
	if len(event.Metadata) > 0 {
		metadata = string(event.Metadata)
	}
	_, err := s.ex.ExecContext(ctx,
		`INSERT INTO audit_logs (`+activityColumns+`)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(event.ID), string(event.TenantID), string(event.UserID), idPtrArg(event.AgentID),
		event.Source, event.Kind, event.Access, event.Outcome, event.Action,
		nullIfEmpty(event.EntityType), idPtrArg(event.EntityID), idPtrArg(event.SpaceID), metadata,
		event.CreatedAt.UTC())
	if err != nil {
		return fmt.Errorf("mysql: 记录活动: %w", err)
	}
	return nil
}

// ListVisible returns either the caller's own complete activity or other users'
// writes in one of the caller-supplied readable shared spaces.
func (s *ActivityStore) ListVisible(ctx context.Context, query activity.VisibleQuery) ([]activity.Event, error) {
	var where strings.Builder
	where.WriteString(" WHERE tenant_id = ? AND (user_id = ?")
	args := []any{string(query.TenantID), string(query.UserID)}

	if len(query.ReadableSharedSpaceIDs) > 0 {
		where.WriteString(" OR (user_id <> ? AND access = ? AND space_id IN (")
		where.WriteString(placeholders(len(query.ReadableSharedSpaceIDs)))
		where.WriteString("))")
		args = append(args, string(query.UserID), activity.AccessWrite)
		args = append(args, idArgs(query.ReadableSharedSpaceIDs)...)
	}
	where.WriteString(")")

	return s.list(ctx, where.String(), args, query.AfterCreatedAt, query.AfterID, query.Limit)
}

func (s *ActivityStore) ListAdmin(ctx context.Context, query activity.AdminQuery) ([]activity.Event, error) {
	return s.list(ctx, " WHERE tenant_id = ?", []any{string(query.TenantID)},
		query.AfterCreatedAt, query.AfterID, query.Limit)
}

func (s *ActivityStore) CountContextFeedback(ctx context.Context, query activity.ContextFeedbackQuery) (activity.ContextFeedbackQuality, error) {
	quality := activity.ContextFeedbackQuality{Since: query.Since.UTC()}
	err := s.ex.QueryRowContext(ctx,
		`SELECT
			COALESCE(SUM(CASE WHEN JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.feedbackOutcome')) = 'delivered' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.feedbackOutcome')) = 'used' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.feedbackOutcome')) = 'ignored' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.feedbackOutcome')) = 'wrong' THEN 1 ELSE 0 END), 0),
			COALESCE(SUM(CASE WHEN JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.feedbackOutcome')) = 'corrected' THEN 1 ELSE 0 END), 0)
		 FROM audit_logs
		 WHERE tenant_id = ? AND kind = ? AND action = ? AND outcome = ? AND created_at >= ?`,
		string(query.TenantID), activity.KindGovernance, "gateway_context_feedback", activity.OutcomeSuccess, query.Since.UTC(),
	).Scan(&quality.Delivered, &quality.Used, &quality.Ignored, &quality.Wrong, &quality.Corrected)
	if err != nil {
		return activity.ContextFeedbackQuality{}, fmt.Errorf("mysql: 汇总自动上下文反馈: %w", err)
	}
	return quality, nil
}

func (s *ActivityStore) list(
	ctx context.Context,
	where string,
	args []any,
	afterCreatedAt time.Time,
	afterID platform.ID,
	limit int,
) ([]activity.Event, error) {
	if !afterCreatedAt.IsZero() {
		where += " AND (created_at < ? OR (created_at = ? AND id < ?))"
		args = append(args, afterCreatedAt.UTC(), afterCreatedAt.UTC(), string(afterID))
	}
	args = append(args, limit)

	rows, err := s.ex.QueryContext(ctx,
		`SELECT `+activityColumns+` FROM audit_logs`+where+`
		 ORDER BY created_at DESC, id DESC
		 LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("mysql: 查询活动: %w", err)
	}
	defer rows.Close()

	var events []activity.Event
	for rows.Next() {
		event, err := scanActivityEvent(rows)
		if err != nil {
			return nil, err
		}
		events = append(events, event)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mysql: 遍历活动: %w", err)
	}
	return events, nil
}

func scanActivityEvent(rows *sql.Rows) (activity.Event, error) {
	var (
		event                                  activity.Event
		id, tenantID, userID                   string
		agentID, entityType, entityID, spaceID sql.NullString
		metadata                               sql.NullString
		createdAt                              time.Time
	)
	if err := rows.Scan(
		&id, &tenantID, &userID, &agentID,
		&event.Source, &event.Kind, &event.Access, &event.Outcome, &event.Action,
		&entityType, &entityID, &spaceID, &metadata, &createdAt,
	); err != nil {
		return activity.Event{}, fmt.Errorf("mysql: 读取活动行: %w", err)
	}
	if entityType.Valid != entityID.Valid {
		return activity.Event{}, fmt.Errorf("mysql: 活动 %s 的 entity_type/entity_id 不完整", id)
	}

	event.ID = platform.ID(id)
	event.TenantID = platform.ID(tenantID)
	event.UserID = platform.ID(userID)
	event.AgentID = idFromNull(agentID)
	if entityType.Valid {
		event.EntityType = entityType.String
		event.EntityID = idFromNull(entityID)
	}
	event.SpaceID = idFromNull(spaceID)
	if metadata.Valid {
		event.Metadata = []byte(metadata.String)
	}
	event.CreatedAt = createdAt.UTC()
	return event, nil
}

// DeleteTracesBefore removes only trace rows, in bounded oldest-first batches.
// Governance and meta-audit evidence is intentionally outside this path.
func (s *ActivityStore) DeleteTracesBefore(ctx context.Context, before time.Time, limit int) (int, error) {
	result, err := s.ex.ExecContext(ctx,
		`DELETE FROM audit_logs
		 WHERE kind = ? AND created_at < ?
		 ORDER BY created_at ASC, id ASC
		 LIMIT ?`,
		activity.KindTrace, before.UTC(), limit)
	if err != nil {
		return 0, fmt.Errorf("mysql: 清理过期 trace: %w", err)
	}
	n, err := result.RowsAffected()
	if err != nil {
		return 0, fmt.Errorf("mysql: 读取 trace 清理数量: %w", err)
	}
	return int(n), nil
}
