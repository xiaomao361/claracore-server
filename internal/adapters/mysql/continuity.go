package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"claracore-server/internal/domain/continuity"
	"claracore-server/internal/platform"
)

type ContinuityStore struct {
	db *sql.DB
	ex sqlExecutor
}

var _ continuity.Store = (*ContinuityStore)(nil)

func NewContinuityStore(db *sql.DB) *ContinuityStore { return &ContinuityStore{db: db, ex: db} }

const lineProjection = `l.id, l.tenant_id, l.owner_user_id, l.owner_agent_id,
	l.title, l.status, l.created_at, l.updated_at,
	p.id, p.summary, p.interpretation_status, p.next_step, p.version, p.updated_at`

func (s *ContinuityStore) ListLines(ctx context.Context, q continuity.ListQuery) ([]continuity.Line, error) {
	where := []string{"l.tenant_id = ?"}
	args := []any{string(q.TenantID)}
	if !q.AllAgents {
		where = append(where, "l.owner_agent_id = ?")
		args = append(args, string(q.OwnerAgent))
	}
	if q.Status != "all" {
		where = append(where, "l.status = ?")
		args = append(args, q.Status)
	}
	args = append(args, q.Limit)
	rows, err := s.ex.QueryContext(ctx,
		`SELECT `+lineProjection+`
		 FROM continuity_lines l
		 LEFT JOIN continuity_positions p ON p.tenant_id = l.tenant_id AND p.line_id = l.id
		 WHERE `+strings.Join(where, " AND ")+`
		 ORDER BY COALESCE(p.updated_at, l.updated_at) DESC, l.id DESC LIMIT ?`, args...)
	if err != nil {
		return nil, fmt.Errorf("mysql: 查询 Shared Line: %w", err)
	}
	defer rows.Close()
	var out []continuity.Line
	for rows.Next() {
		line, err := scanLine(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *line)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mysql: 遍历 Shared Line: %w", err)
	}
	return out, nil
}

func (s *ContinuityStore) FindLine(ctx context.Context, tenantID, lineID platform.ID, activeOnly bool) (*continuity.Line, error) {
	status := ""
	args := []any{string(tenantID), string(lineID)}
	if activeOnly {
		status = " AND l.status = ?"
		args = append(args, continuity.LineActive)
	}
	return scanLine(s.ex.QueryRowContext(ctx,
		`SELECT `+lineProjection+`
		 FROM continuity_lines l
		 LEFT JOIN continuity_positions p ON p.tenant_id = l.tenant_id AND p.line_id = l.id
		 WHERE l.tenant_id = ? AND l.id = ?`+status, args...))
}

type rowScanner interface{ Scan(...any) error }

func scanLine(row rowScanner) (*continuity.Line, error) {
	var line continuity.Line
	var id, tenantID, ownerUserID, ownerAgentID string
	var created, updated time.Time
	var positionID, summary, positionStatus, nextStep sql.NullString
	var positionVersion sql.NullInt64
	var positionUpdated sql.NullTime
	err := row.Scan(&id, &tenantID, &ownerUserID, &ownerAgentID,
		&line.Title, &line.Status, &created, &updated,
		&positionID, &summary, &positionStatus, &nextStep, &positionVersion, &positionUpdated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, continuity.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("mysql: 读取 Shared Line: %w", err)
	}
	line.ID, line.TenantID = platform.ID(id), platform.ID(tenantID)
	line.OwnerUserID, line.OwnerAgentID = platform.ID(ownerUserID), platform.ID(ownerAgentID)
	line.CreatedAt, line.UpdatedAt = created.UTC(), updated.UTC()
	if positionID.Valid {
		line.Position = &continuity.Position{
			ID: platform.ID(positionID.String), TenantID: line.TenantID, LineID: line.ID,
			Summary: summary.String, InterpretationStatus: positionStatus.String,
			NextStep: nextStep.String, Version: int(positionVersion.Int64), UpdatedAt: positionUpdated.Time.UTC(),
		}
	}
	return &line, nil
}

func (s *ContinuityStore) CreateLine(ctx context.Context, line continuity.Line) error {
	_, err := s.ex.ExecContext(ctx,
		`INSERT INTO continuity_lines
		 (id, tenant_id, owner_user_id, owner_agent_id, title, status, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?)`,
		string(line.ID), string(line.TenantID), string(line.OwnerUserID), string(line.OwnerAgentID),
		line.Title, line.Status, line.CreatedAt.UTC(), line.UpdatedAt.UTC())
	if err != nil {
		return fmt.Errorf("mysql: 创建 Shared Line: %w", err)
	}
	return nil
}

func (s *ContinuityStore) UpdateLineStatus(ctx context.Context, tenantID, lineID platform.ID, status string, at time.Time) error {
	res, err := s.ex.ExecContext(ctx, `UPDATE continuity_lines SET status = ?, updated_at = ? WHERE tenant_id = ? AND id = ?`, status, at.UTC(), string(tenantID), string(lineID))
	if err != nil {
		return fmt.Errorf("mysql: 更新 Shared Line 状态: %w", err)
	}
	if n, _ := res.RowsAffected(); n == 0 {
		return continuity.ErrNotFound
	}
	return nil
}

func (s *ContinuityStore) FindPosition(ctx context.Context, tenantID, lineID platform.ID) (*continuity.Position, error) {
	return scanPosition(s.ex.QueryRowContext(ctx,
		`SELECT id, tenant_id, line_id, summary, interpretation_status, facts_used, next_step,
		        writer_user_id, writer_agent_id, version, updated_at
		 FROM continuity_positions WHERE tenant_id = ? AND line_id = ?`, string(tenantID), string(lineID)))
}

func scanPosition(row rowScanner) (*continuity.Position, error) {
	var p continuity.Position
	var id, tenantID, lineID, writerUserID, writerAgentID string
	var facts []byte
	var updated time.Time
	err := row.Scan(&id, &tenantID, &lineID, &p.Summary, &p.InterpretationStatus, &facts, &p.NextStep,
		&writerUserID, &writerAgentID, &p.Version, &updated)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, continuity.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("mysql: 读取 Shared Line 位置: %w", err)
	}
	p.ID, p.TenantID, p.LineID = platform.ID(id), platform.ID(tenantID), platform.ID(lineID)
	p.WriterUserID, p.WriterAgentID, p.UpdatedAt = platform.ID(writerUserID), platform.ID(writerAgentID), updated.UTC()
	if err := json.Unmarshal(facts, &p.FactsUsed); err != nil {
		return nil, fmt.Errorf("mysql: 解析 Shared Line facts_used: %w", err)
	}
	return &p, nil
}

func (s *ContinuityStore) CreatePosition(ctx context.Context, p continuity.Position) error {
	facts, err := json.Marshal(p.FactsUsed)
	if err != nil {
		return fmt.Errorf("mysql: 编码 Shared Line facts_used: %w", err)
	}
	_, err = s.ex.ExecContext(ctx,
		`INSERT INTO continuity_positions
		 (id, tenant_id, line_id, summary, interpretation_status, facts_used, next_step,
		  writer_user_id, writer_agent_id, version, created_at, updated_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(p.ID), string(p.TenantID), string(p.LineID), p.Summary, p.InterpretationStatus, facts, p.NextStep,
		string(p.WriterUserID), string(p.WriterAgentID), p.Version, p.UpdatedAt.UTC(), p.UpdatedAt.UTC())
	if err != nil {
		return fmt.Errorf("mysql: 创建 Shared Line 位置: %w", err)
	}
	return nil
}

func (s *ContinuityStore) UpdatePosition(ctx context.Context, p continuity.Position, expectedVersion int) error {
	facts, err := json.Marshal(p.FactsUsed)
	if err != nil {
		return fmt.Errorf("mysql: 编码 Shared Line facts_used: %w", err)
	}
	res, err := s.ex.ExecContext(ctx,
		`UPDATE continuity_positions
		 SET summary = ?, interpretation_status = ?, facts_used = ?, next_step = ?,
		     writer_user_id = ?, writer_agent_id = ?, version = ?, updated_at = ?
		 WHERE tenant_id = ? AND line_id = ? AND version = ?`,
		p.Summary, p.InterpretationStatus, facts, p.NextStep, string(p.WriterUserID), string(p.WriterAgentID),
		p.Version, p.UpdatedAt.UTC(), string(p.TenantID), string(p.LineID), expectedVersion)
	if err != nil {
		return fmt.Errorf("mysql: 更新 Shared Line 位置: %w", err)
	}
	if n, err := res.RowsAffected(); err == nil && n > 0 {
		return nil
	}
	var current int
	err = s.ex.QueryRowContext(ctx,
		`SELECT version FROM continuity_positions WHERE tenant_id = ? AND line_id = ?`,
		string(p.TenantID), string(p.LineID)).Scan(&current)
	if errors.Is(err, sql.ErrNoRows) {
		return continuity.ErrNotFound
	}
	if err != nil {
		return fmt.Errorf("mysql: 确认 Shared Line 位置版本: %w", err)
	}
	return &continuity.VersionConflictError{Current: current}
}

func (s *ContinuityStore) AppendPositionHistory(ctx context.Context, historyID platform.ID, p continuity.Position, at time.Time) error {
	facts, err := json.Marshal(p.FactsUsed)
	if err != nil {
		return fmt.Errorf("mysql: 编码 Shared Line 历史 facts_used: %w", err)
	}
	_, err = s.ex.ExecContext(ctx,
		`INSERT INTO continuity_position_history
		 (id, tenant_id, line_id, position_id, position_version, summary, interpretation_status,
		  facts_used, next_step, writer_user_id, writer_agent_id, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(historyID), string(p.TenantID), string(p.LineID), string(p.ID), p.Version,
		p.Summary, p.InterpretationStatus, facts, p.NextStep,
		string(p.WriterUserID), string(p.WriterAgentID), at.UTC())
	if err != nil {
		return fmt.Errorf("mysql: 追加 Shared Line 位置历史: %w", err)
	}
	return nil
}

func (s *ContinuityStore) ListPositionHistory(ctx context.Context, tenantID, lineID platform.ID, limit int) ([]continuity.Position, error) {
	rows, err := s.ex.QueryContext(ctx, `SELECT position_id, tenant_id, line_id, summary, interpretation_status, facts_used, next_step, writer_user_id, writer_agent_id, position_version, created_at FROM continuity_position_history WHERE tenant_id=? AND line_id=? ORDER BY created_at DESC, id DESC LIMIT ?`, string(tenantID), string(lineID), limit)
	if err != nil {
		return nil, fmt.Errorf("mysql: 查询 Shared Line 位置历史: %w", err)
	}
	defer rows.Close()
	var out []continuity.Position
	for rows.Next() {
		p, err := scanPosition(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *p)
	}
	return out, rows.Err()
}

func (s *ContinuityStore) LatestHandoff(ctx context.Context, tenantID, lineID platform.ID) (*continuity.Handoff, error) {
	return scanHandoff(s.ex.QueryRowContext(ctx,
		`SELECT id, tenant_id, line_id, writer_user_id, writer_agent_id,
		        objective, completed, open_items, next_step, created_at
		 FROM continuity_handoffs
		 WHERE tenant_id = ? AND line_id = ? ORDER BY created_at DESC, id DESC LIMIT 1`,
		string(tenantID), string(lineID)))
}

func scanHandoff(row rowScanner) (*continuity.Handoff, error) {
	var h continuity.Handoff
	var id, tenant, line, writerUser, writerAgent string
	var completed, openItems []byte
	var created time.Time
	err := row.Scan(
		&id, &tenant, &line, &writerUser, &writerAgent,
		&h.Objective, &completed, &openItems, &h.NextStep, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return nil, continuity.ErrNotFound
	}
	if err != nil {
		return nil, fmt.Errorf("mysql: 查询 Shared Line 交接: %w", err)
	}
	h.ID, h.TenantID, h.LineID = platform.ID(id), platform.ID(tenant), platform.ID(line)
	h.WriterUserID, h.WriterAgentID, h.CreatedAt = platform.ID(writerUser), platform.ID(writerAgent), created.UTC()
	if err := json.Unmarshal(completed, &h.Completed); err != nil {
		return nil, fmt.Errorf("mysql: 解析 Shared Line completed: %w", err)
	}
	if err := json.Unmarshal(openItems, &h.OpenItems); err != nil {
		return nil, fmt.Errorf("mysql: 解析 Shared Line open_items: %w", err)
	}
	return &h, nil
}

func (s *ContinuityStore) CreateHandoff(ctx context.Context, h continuity.Handoff) error {
	completed, err := json.Marshal(h.Completed)
	if err != nil {
		return fmt.Errorf("mysql: 编码 Shared Line completed: %w", err)
	}
	openItems, err := json.Marshal(h.OpenItems)
	if err != nil {
		return fmt.Errorf("mysql: 编码 Shared Line open_items: %w", err)
	}
	_, err = s.ex.ExecContext(ctx,
		`INSERT INTO continuity_handoffs
		 (id, tenant_id, line_id, writer_user_id, writer_agent_id, objective, completed, open_items, next_step, created_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		string(h.ID), string(h.TenantID), string(h.LineID), string(h.WriterUserID), string(h.WriterAgentID),
		h.Objective, completed, openItems, h.NextStep, h.CreatedAt.UTC())
	if err != nil {
		return fmt.Errorf("mysql: 创建 Shared Line 交接: %w", err)
	}
	return nil
}

func (s *ContinuityStore) ListHandoffs(ctx context.Context, tenantID, lineID platform.ID, limit int) ([]continuity.Handoff, error) {
	rows, err := s.ex.QueryContext(ctx, `SELECT id, tenant_id, line_id, writer_user_id, writer_agent_id, objective, completed, open_items, next_step, created_at FROM continuity_handoffs WHERE tenant_id=? AND line_id=? ORDER BY created_at DESC, id DESC LIMIT ?`, string(tenantID), string(lineID), limit)
	if err != nil {
		return nil, fmt.Errorf("mysql: 查询 Shared Line 交接历史: %w", err)
	}
	defer rows.Close()
	var out []continuity.Handoff
	for rows.Next() {
		h, err := scanHandoff(rows)
		if err != nil {
			return nil, err
		}
		out = append(out, *h)
	}
	return out, rows.Err()
}

func (s *ContinuityStore) Atomically(ctx context.Context, fn func(continuity.Store) error) error {
	if s.db == nil {
		return fn(s)
	}
	tx, err := s.db.BeginTx(ctx, nil)
	if err != nil {
		return fmt.Errorf("mysql: 开启事务: %w", err)
	}
	committed := false
	defer func() {
		if !committed {
			_ = tx.Rollback()
		}
	}()
	if err := fn(&ContinuityStore{ex: tx}); err != nil {
		return err
	}
	if err := tx.Commit(); err != nil {
		return fmt.Errorf("mysql: 提交事务: %w", err)
	}
	committed = true
	return nil
}
