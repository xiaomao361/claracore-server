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
	whereString, args := appendActivityFilters(where.String(), args, query.Action, query.Outcome, query.AgentID, query.SpaceID, query.Since)

	return s.list(ctx, whereString, args, query.AfterCreatedAt, query.AfterID, query.Limit)
}

func (s *ActivityStore) ListAdmin(ctx context.Context, query activity.AdminQuery) ([]activity.Event, error) {
	where, args := appendActivityFilters(" WHERE tenant_id = ?", []any{string(query.TenantID)}, query.Action, query.Outcome, query.AgentID, query.SpaceID, query.Since)
	return s.list(ctx, where, args,
		query.AfterCreatedAt, query.AfterID, query.Limit)
}

func appendActivityFilters(where string, args []any, action, outcome string, agentID, spaceID platform.ID, since time.Time) (string, []any) {
	if action != "" {
		where += " AND action LIKE ?"
		args = append(args, "%"+action+"%")
	}
	if outcome != "" {
		where += " AND outcome = ?"
		args = append(args, outcome)
	}
	if agentID != "" {
		where += " AND agent_id = ?"
		args = append(args, string(agentID))
	}
	if spaceID != "" {
		where += " AND space_id = ?"
		args = append(args, string(spaceID))
	}
	if !since.IsZero() {
		where += " AND created_at >= ?"
		args = append(args, since.UTC())
	}
	return where, args
}

func (s *ActivityStore) CountContextFeedback(ctx context.Context, query activity.ContextFeedbackQuery) (activity.ContextFeedbackQuality, error) {
	quality := activity.ContextFeedbackQuality{Since: query.Since.UTC()}
	err := s.ex.QueryRowContext(ctx,
		`WITH relevant AS (
			SELECT kind, action,
				JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.decision')) AS decision,
				JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.reason')) AS reason,
				JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.contextId')) AS context_id,
				JSON_UNQUOTE(JSON_EXTRACT(metadata, '$.feedbackOutcome')) AS feedback_outcome
			FROM audit_logs
			WHERE tenant_id = ? AND outcome = ? AND created_at >= ?
				AND action IN ('gateway_auto_context', 'gateway_context_feedback')
		), selections AS (
			SELECT context_id
			FROM relevant
			WHERE kind = 'trace' AND action = 'gateway_auto_context'
				AND decision = 'deliver_one' AND context_id IS NOT NULL AND context_id <> ''
		), feedback AS (
			SELECT context_id, feedback_outcome
			FROM relevant
			WHERE kind = 'governance' AND action = 'gateway_context_feedback'
		)
		SELECT
			(SELECT COUNT(*) FROM relevant WHERE kind = 'trace' AND action = 'gateway_auto_context' AND decision = 'deliver_one'),
			(SELECT COUNT(*) FROM relevant WHERE kind = 'trace' AND action = 'gateway_auto_context' AND decision = 'abstain'),
			(SELECT COALESCE(SUM(reason IN ('no_candidates', 'low_relevance', 'no_high_confidence_evidence')), 0) FROM relevant WHERE kind = 'trace' AND action = 'gateway_auto_context' AND decision = 'abstain'),
			(SELECT COALESCE(SUM(reason IN ('ambiguous_top_results', 'ambiguous_domains')), 0) FROM relevant WHERE kind = 'trace' AND action = 'gateway_auto_context' AND decision = 'abstain'),
			(SELECT COALESCE(SUM(reason = 'semantic_unavailable'), 0) FROM relevant WHERE kind = 'trace' AND action = 'gateway_auto_context' AND decision = 'abstain'),
			(SELECT COALESCE(SUM(reason = 'retrieval_timeout'), 0) FROM relevant WHERE kind = 'trace' AND action = 'gateway_auto_context' AND decision = 'abstain'),
			(SELECT COALESCE(SUM(reason = 'shared_line_not_ready'), 0) FROM relevant WHERE kind = 'trace' AND action = 'gateway_auto_context' AND decision = 'abstain'),
			(SELECT COALESCE(SUM(reason IS NULL OR reason NOT IN ('no_candidates', 'low_relevance', 'no_high_confidence_evidence', 'ambiguous_top_results', 'ambiguous_domains', 'semantic_unavailable', 'retrieval_timeout', 'shared_line_not_ready')), 0) FROM relevant WHERE kind = 'trace' AND action = 'gateway_auto_context' AND decision = 'abstain'),
			(SELECT COUNT(DISTINCT feedback.context_id) FROM feedback INNER JOIN selections ON selections.context_id = feedback.context_id),
			(SELECT COALESCE(SUM(feedback_outcome = 'delivered'), 0) FROM feedback),
			(SELECT COALESCE(SUM(feedback_outcome = 'used'), 0) FROM feedback),
			(SELECT COALESCE(SUM(feedback_outcome = 'ignored'), 0) FROM feedback),
			(SELECT COALESCE(SUM(feedback_outcome = 'wrong'), 0) FROM feedback),
			(SELECT COALESCE(SUM(feedback_outcome = 'corrected'), 0) FROM feedback)`,
		string(query.TenantID), activity.OutcomeSuccess, query.Since.UTC(),
	).Scan(
		&quality.Selected, &quality.Abstained,
		&quality.AbstainReasons.NoRelevantEvidence, &quality.AbstainReasons.Ambiguous,
		&quality.AbstainReasons.SemanticUnavailable, &quality.AbstainReasons.RetrievalTimeout,
		&quality.AbstainReasons.SharedLineNotReady, &quality.AbstainReasons.Other,
		&quality.FeedbackCoveredSelections,
		&quality.Delivered, &quality.Used, &quality.Ignored, &quality.Wrong, &quality.Corrected,
	)
	if err != nil {
		return activity.ContextFeedbackQuality{}, fmt.Errorf("mysql: 汇总自动上下文质量: %w", err)
	}
	return quality, nil
}

// CountSharedActivity aggregates only successful writes in caller-resolved
// readable shared spaces. The space predicate is applied in SQL so inaccessible
// project and personal activity never enters application memory, even as rows.
func (s *ActivityStore) CountSharedActivity(ctx context.Context, query activity.SharedActivityQuery) (activity.SharedActivityPulse, error) {
	pulse := activity.SharedActivityPulse{Since: query.Since.UTC()}
	if len(query.ReadableSharedSpaceIDs) == 0 {
		return pulse, nil
	}
	where := ` WHERE tenant_id = ? AND outcome = ? AND access = ? AND created_at >= ? AND space_id IN (` + placeholders(len(query.ReadableSharedSpaceIDs)) + `)`
	args := []any{string(query.TenantID), activity.OutcomeSuccess, activity.AccessWrite, query.Since.UTC()}
	args = append(args, idArgs(query.ReadableSharedSpaceIDs)...)

	var lastWrite sql.NullTime
	if err := s.ex.QueryRowContext(ctx,
		`SELECT COUNT(*), COUNT(DISTINCT user_id), COUNT(DISTINCT agent_id), MAX(created_at)
		 FROM audit_logs`+where, args...).Scan(
		&pulse.Writes, &pulse.ActiveUsers, &pulse.ActiveAgents, &lastWrite,
	); err != nil {
		return activity.SharedActivityPulse{}, fmt.Errorf("mysql: 汇总共享空间活动: %w", err)
	}
	if lastWrite.Valid {
		at := lastWrite.Time.UTC()
		pulse.LastWriteAt = &at
	}

	rows, err := s.ex.QueryContext(ctx,
		`SELECT DATE(created_at), COUNT(*)
		 FROM audit_logs`+where+`
		 GROUP BY DATE(created_at)
		 ORDER BY DATE(created_at) ASC`, args...)
	if err != nil {
		return activity.SharedActivityPulse{}, fmt.Errorf("mysql: 按日汇总共享空间活动: %w", err)
	}
	defer rows.Close()
	for rows.Next() {
		var daily activity.DailySharedWrites
		if err := rows.Scan(&daily.Date, &daily.Writes); err != nil {
			return activity.SharedActivityPulse{}, fmt.Errorf("mysql: 读取共享空间活动日桶: %w", err)
		}
		daily.Date = daily.Date.UTC()
		pulse.DailyWrites = append(pulse.DailyWrites, daily)
	}
	if err := rows.Err(); err != nil {
		return activity.SharedActivityPulse{}, fmt.Errorf("mysql: 遍历共享空间活动日桶: %w", err)
	}
	return pulse, nil
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
