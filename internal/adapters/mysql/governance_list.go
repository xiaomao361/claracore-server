package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"claracore-server/internal/domain/governance"
	"claracore-server/internal/platform"
)

const userSummaryColumns = `id, tenant_id, username, display_name, role, must_change_pw, status, created_at`

func scanUserSummary(row scanRow) (governance.UserSummary, error) {
	var out governance.UserSummary
	var id, tenantID string
	var createdAt time.Time
	if err := row.Scan(&id, &tenantID, &out.Username, &out.DisplayName, &out.Role, &out.MustChangePw, &out.Status, &createdAt); err != nil {
		return governance.UserSummary{}, err
	}
	out.ID, out.TenantID, out.CreatedAt = platform.ID(id), platform.ID(tenantID), createdAt.UTC()
	return out, nil
}

func (s *GovernanceStore) ListUserSummaries(ctx context.Context, query governance.GovernancePageQuery) ([]governance.UserSummary, error) {
	statement := `SELECT ` + userSummaryColumns + ` FROM users WHERE tenant_id = ? ORDER BY id DESC LIMIT ?`
	args := []any{query.TenantID, query.Limit}
	if query.BeforeID != "" {
		statement = `SELECT ` + userSummaryColumns + ` FROM users WHERE tenant_id = ? AND id < ? ORDER BY id DESC LIMIT ?`
		args = []any{query.TenantID, query.BeforeID, query.Limit}
	}
	rows, err := s.ex.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("mysql: 查询有界成员列表: %w", err)
	}
	defer rows.Close()
	var out []governance.UserSummary
	for rows.Next() {
		user, err := scanUserSummary(rows)
		if err != nil {
			return nil, fmt.Errorf("mysql: 读取成员摘要: %w", err)
		}
		out = append(out, user)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mysql: 遍历成员摘要: %w", err)
	}
	return out, nil
}

func (s *GovernanceStore) FindUserSummary(ctx context.Context, tenantID, userID platform.ID) (governance.UserSummary, error) {
	out, err := scanUserSummary(s.ex.QueryRowContext(ctx, `SELECT `+userSummaryColumns+` FROM users WHERE tenant_id = ? AND id = ?`, tenantID, userID))
	if errors.Is(err, sql.ErrNoRows) {
		return governance.UserSummary{}, governance.ErrUserNotFound
	}
	if err != nil {
		return governance.UserSummary{}, fmt.Errorf("mysql: 查询成员摘要: %w", err)
	}
	return out, nil
}

func (s *GovernanceStore) ListTokenSummaries(ctx context.Context, query governance.GovernancePageQuery) ([]governance.TokenSummary, error) {
	const columns = `id, tenant_id, user_id, agent_id, name, token_prefix, expires_at, last_used_at, revoked_at, created_at`
	statement := `SELECT ` + columns + ` FROM tokens WHERE tenant_id = ? AND user_id = ? ORDER BY id DESC LIMIT ?`
	args := []any{query.TenantID, query.UserID, query.Limit}
	if query.BeforeID != "" {
		statement = `SELECT ` + columns + ` FROM tokens WHERE tenant_id = ? AND user_id = ? AND id < ? ORDER BY id DESC LIMIT ?`
		args = []any{query.TenantID, query.UserID, query.BeforeID, query.Limit}
	}
	rows, err := s.ex.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("mysql: 查询有界 token 列表: %w", err)
	}
	defer rows.Close()
	var out []governance.TokenSummary
	for rows.Next() {
		token, err := scanTokenSummary(rows)
		if err != nil {
			return nil, fmt.Errorf("mysql: 读取 token 摘要: %w", err)
		}
		out = append(out, token)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mysql: 遍历 token 摘要: %w", err)
	}
	return out, nil
}

func scanTokenSummary(row scanRow) (governance.TokenSummary, error) {
	var out governance.TokenSummary
	var id, tenantID, userID, agentID string
	var expiresAt, lastUsedAt, revokedAt sql.NullTime
	var createdAt time.Time
	if err := row.Scan(&id, &tenantID, &userID, &agentID, &out.Name, &out.Prefix, &expiresAt, &lastUsedAt, &revokedAt, &createdAt); err != nil {
		return governance.TokenSummary{}, err
	}
	out.ID, out.TenantID, out.UserID, out.AgentID = platform.ID(id), platform.ID(tenantID), platform.ID(userID), platform.ID(agentID)
	out.ExpiresAt, out.LastUsedAt, out.RevokedAt = timePtr(expiresAt), timePtr(lastUsedAt), timePtr(revokedAt)
	out.CreatedAt = createdAt.UTC()
	return out, nil
}

func (s *GovernanceStore) FindTokenSummary(ctx context.Context, tenantID, userID, tokenID platform.ID) (governance.TokenSummary, error) {
	row := s.ex.QueryRowContext(ctx, `SELECT id, tenant_id, user_id, agent_id, name, token_prefix, expires_at, last_used_at, revoked_at, created_at
		FROM tokens WHERE tenant_id = ? AND user_id = ? AND id = ?`, tenantID, userID, tokenID)
	out, err := scanTokenSummary(row)
	if errors.Is(err, sql.ErrNoRows) {
		return governance.TokenSummary{}, governance.ErrTokenNotFound
	}
	if err != nil {
		return governance.TokenSummary{}, fmt.Errorf("mysql: 查询 token 摘要: %w", err)
	}
	return out, nil
}

func (s *GovernanceStore) ListGovernedSpaces(ctx context.Context, query governance.GovernancePageQuery) ([]governance.Space, error) {
	const columns = `id, tenant_id, type, name, owner_user_id, created_at`
	statement := `SELECT ` + columns + ` FROM spaces WHERE tenant_id = ? AND type <> ? ORDER BY id DESC LIMIT ?`
	args := []any{query.TenantID, governance.SpacePersonal, query.Limit}
	if query.BeforeID != "" {
		statement = `SELECT ` + columns + ` FROM spaces WHERE tenant_id = ? AND type <> ? AND id < ? ORDER BY id DESC LIMIT ?`
		args = []any{query.TenantID, governance.SpacePersonal, query.BeforeID, query.Limit}
	}
	rows, err := s.ex.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("mysql: 查询有界空间列表: %w", err)
	}
	defer rows.Close()
	var out []governance.Space
	for rows.Next() {
		var space governance.Space
		var id, tenantID string
		var owner sql.NullString
		var createdAt time.Time
		if err := rows.Scan(&id, &tenantID, &space.Type, &space.Name, &owner, &createdAt); err != nil {
			return nil, fmt.Errorf("mysql: 读取空间摘要: %w", err)
		}
		space.ID, space.TenantID, space.CreatedAt = platform.ID(id), platform.ID(tenantID), createdAt.UTC()
		if owner.Valid {
			value := platform.ID(owner.String)
			space.OwnerUserID = &value
		}
		out = append(out, space)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mysql: 遍历空间摘要: %w", err)
	}
	return out, nil
}

func (s *GovernanceStore) ListSpaceGrantSummaries(ctx context.Context, query governance.GovernancePageQuery) ([]governance.SpaceGrantSummary, error) {
	statement := `SELECT u.id, u.tenant_id, u.username, u.display_name, u.role, u.must_change_pw, u.status, u.created_at,
		p.space_id, p.user_id, p.can_read, p.can_write, p.granted_by, p.created_at
		FROM users u LEFT JOIN space_permissions p ON p.space_id = ? AND p.user_id = u.id
		WHERE u.tenant_id = ? ORDER BY u.id DESC LIMIT ?`
	args := []any{query.SpaceID, query.TenantID, query.Limit}
	filters := ""
	if query.OwnerUserID != "" {
		filters += " AND u.id <> ?"
		args = []any{query.SpaceID, query.TenantID, query.OwnerUserID}
	}
	if query.BeforeID != "" {
		filters += " AND u.id < ?"
		args = append(args, query.BeforeID)
	}
	if filters != "" {
		statement = `SELECT u.id, u.tenant_id, u.username, u.display_name, u.role, u.must_change_pw, u.status, u.created_at,
			p.space_id, p.user_id, p.can_read, p.can_write, p.granted_by, p.created_at
			FROM users u LEFT JOIN space_permissions p ON p.space_id = ? AND p.user_id = u.id
			WHERE u.tenant_id = ?` + filters + ` ORDER BY u.id DESC LIMIT ?`
		args = append(args, query.Limit)
	}
	rows, err := s.ex.QueryContext(ctx, statement, args...)
	if err != nil {
		return nil, fmt.Errorf("mysql: 查询有界空间授权列表: %w", err)
	}
	defer rows.Close()
	var out []governance.SpaceGrantSummary
	for rows.Next() {
		var user governance.UserSummary
		var id, tenantID string
		var createdAt time.Time
		var spaceID, userID, grantedBy sql.NullString
		var canRead, canWrite sql.NullBool
		var permissionCreatedAt sql.NullTime
		if err := rows.Scan(&id, &tenantID, &user.Username, &user.DisplayName, &user.Role, &user.MustChangePw, &user.Status, &createdAt,
			&spaceID, &userID, &canRead, &canWrite, &grantedBy, &permissionCreatedAt); err != nil {
			return nil, fmt.Errorf("mysql: 读取空间授权摘要: %w", err)
		}
		user.ID, user.TenantID, user.CreatedAt = platform.ID(id), platform.ID(tenantID), createdAt.UTC()
		item := governance.SpaceGrantSummary{User: user}
		if spaceID.Valid {
			item.Permission = &governance.Permission{
				SpaceID: platform.ID(spaceID.String), UserID: platform.ID(userID.String),
				CanRead: canRead.Bool, CanWrite: canWrite.Bool, GrantedBy: platform.ID(grantedBy.String),
				CreatedAt: permissionCreatedAt.Time.UTC(),
			}
		}
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mysql: 遍历空间授权摘要: %w", err)
	}
	return out, nil
}

func (s *GovernanceStore) ListAgentAccessSummaries(ctx context.Context, tenantID, userID platform.ID, limit int) ([]governance.AgentAccessSummary, error) {
	rows, err := s.ex.QueryContext(ctx, `SELECT agent_id, COUNT(*), SUM(revoked_at IS NULL), MAX(last_used_at), MAX(created_at)
		FROM tokens WHERE tenant_id = ? AND user_id = ?
		GROUP BY agent_id ORDER BY COALESCE(MAX(last_used_at), MAX(created_at)) DESC, agent_id DESC LIMIT ?`, tenantID, userID, limit)
	if err != nil {
		return nil, fmt.Errorf("mysql: 查询 Agent 接入摘要: %w", err)
	}
	defer rows.Close()
	var out []governance.AgentAccessSummary
	for rows.Next() {
		var item governance.AgentAccessSummary
		var agentID string
		var lastUsed sql.NullTime
		if err := rows.Scan(&agentID, &item.TokenCount, &item.ActiveTokens, &lastUsed, &item.LastIssuedAt); err != nil {
			return nil, fmt.Errorf("mysql: 读取 Agent 接入摘要: %w", err)
		}
		item.AgentID = platform.ID(agentID)
		item.LastUsedAt = timePtr(lastUsed)
		item.LastIssuedAt = item.LastIssuedAt.UTC()
		out = append(out, item)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("mysql: 遍历 Agent 接入摘要: %w", err)
	}
	return out, nil
}
