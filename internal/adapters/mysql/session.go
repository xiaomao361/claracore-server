package mysql

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"claracore-server/internal/ports"
)

// SessionStore 是 ports.SessionStore 的 MySQL 实现。
//
// 它是**默认实现**而不是降级实现：多实例部署时，内存会话会让人在
// 实例之间反复掉线（docs/01-architecture.md §6.2 里 SessionStore 那一行）。
// 配了 Redis 之后可以换成 Redis 实现，但数据库这条路必须一直能走——
// 想轻量试用的人不该被要求先搭一套 Redis。
type SessionStore struct {
	db *sql.DB
}

var _ ports.SessionStore = (*SessionStore)(nil)

func NewSessionStore(db *sql.DB) *SessionStore { return &SessionStore{db: db} }

func (s *SessionStore) Create(ctx context.Context, sess ports.Session) error {
	_, err := s.db.ExecContext(ctx,
		`INSERT INTO sessions (id, tenant_id, user_id, auth_version, expires_at, created_at, last_seen_at)
		 VALUES (?, ?, ?, ?, ?, ?, ?)`,
		sess.TokenHash, sess.TenantID, sess.UserID, sess.AuthVersion,
		sess.ExpiresAt.UTC(), sess.CreatedAt.UTC(), sess.LastSeenAt.UTC())
	if err != nil {
		return fmt.Errorf("mysql: 创建会话: %w", err)
	}
	return nil
}

// Find 按令牌哈希取会话。
//
// **不在 SQL 里过滤过期**：过期判断在领域层（用可注入的 Clock），
// 与 token 的过期判断同一条纪律。放进 SQL 就有了两个判断者、两个时钟源，
// 测试注入假时钟时立刻打架。
func (s *SessionStore) Find(ctx context.Context, tokenHash string) (ports.Session, bool, error) {
	var (
		sess                       ports.Session
		expires, created, lastSeen time.Time
	)
	err := s.db.QueryRowContext(ctx,
		`SELECT id, tenant_id, user_id, auth_version, expires_at, created_at, last_seen_at
		 FROM sessions WHERE id = ?`, tokenHash,
	).Scan(&sess.TokenHash, &sess.TenantID, &sess.UserID, &sess.AuthVersion, &expires, &created, &lastSeen)
	switch {
	case errors.Is(err, sql.ErrNoRows):
		// 不存在不是错误：过期清理、主动登出、账号停用都会让它消失
		return ports.Session{}, false, nil
	case err != nil:
		return ports.Session{}, false, fmt.Errorf("mysql: 查询会话: %w", err)
	}
	sess.ExpiresAt, sess.CreatedAt, sess.LastSeenAt = expires.UTC(), created.UTC(), lastSeen.UTC()
	return sess, true, nil
}

func (s *SessionStore) Touch(ctx context.Context, tokenHash string, seenAt, expiresAt time.Time) error {
	_, err := s.db.ExecContext(ctx,
		`UPDATE sessions SET last_seen_at = ?, expires_at = ? WHERE id = ?`,
		seenAt.UTC(), expiresAt.UTC(), tokenHash)
	if err != nil {
		return fmt.Errorf("mysql: 更新会话活跃时间: %w", err)
	}
	// 不检查影响行数：会话在这一刻被并发删掉（管理员停用了这个人）
	// 是完全正常的，而调用方对 Touch 失败的处理本来就是「忽略」——
	// 它是运维辅助信号，不是授权依据。
	return nil
}

func (s *SessionStore) Delete(ctx context.Context, tokenHash string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE id = ?`, tokenHash)
	if err != nil {
		return fmt.Errorf("mysql: 删除会话: %w", err)
	}
	return nil // 幂等：本来就不存在也算删掉了
}

// DeleteByUser 废掉某人的全部会话。
//
// 这是整张表存库（而不是签名 cookie）的意义所在：停用账号、重置密码、
// 改密之后，别处已经登录的浏览器必须当场失效。走 idx_sessions_user。
func (s *SessionStore) DeleteByUser(ctx context.Context, userID string) error {
	_, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE user_id = ?`, userID)
	if err != nil {
		return fmt.Errorf("mysql: 删除用户会话: %w", err)
	}
	return nil
}

func (s *SessionStore) DeleteExpired(ctx context.Context, before time.Time) (int, error) {
	res, err := s.db.ExecContext(ctx, `DELETE FROM sessions WHERE expires_at < ?`, before.UTC())
	if err != nil {
		return 0, fmt.Errorf("mysql: 清理过期会话: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return 0, nil // 删掉了但数不清，不值得让清理任务失败
	}
	return int(n), nil
}
