package ports

import (
	"context"
	"time"
)

// Session 是一条 Web 登录会话。
//
// 字段全是标准库类型，因为 ports 不认识领域概念——ID 在这里就是字符串。
// 这不是形式主义：SessionStore 的 Redis 实现（多实例部署时）
// 不应该、也不需要知道 platform.ID 是什么。
type Session struct {
	// TokenHash 是会话令牌的 SHA-256，**不是令牌本身**。
	//
	// 与 tokens 表同一条纪律：库里存哈希，明文只在签发时进一次 cookie。
	// 数据库被读走时，拿到的是一堆无法用于登录的哈希。
	TokenHash string

	TenantID string
	UserID   string
	// AuthVersion binds the session to the credential generation that issued
	// it. Resume must compare it with the current user row; deleting existing
	// sessions alone cannot revoke a concurrent late insert.
	AuthVersion uint64

	ExpiresAt  time.Time
	CreatedAt  time.Time
	LastSeenAt time.Time
}

func (s Session) Expired(now time.Time) bool { return !now.Before(s.ExpiresAt) }

// SessionStore 是 Web 会话的存储。
//
// # 为什么是存储而不是签名 cookie
//
// 会话必须能被**立即吊销**：停用一个员工账号时，他浏览器里那张已签发的票
// 必须当场失效。签名 cookie（JWT 那一路）做不到这件事——除非再引入一张
// 吊销名单表，那时它已经是一个更复杂的会话存储了。
//
// 默认实现落在数据库而不是内存，也是同一个理由的延伸：多实例部署时
// 内存实现会让人在实例之间反复掉线。
type SessionStore interface {
	Create(ctx context.Context, s Session) error

	// Find 按令牌哈希取会话。第二个返回值为 false 表示不存在，
	// 那不是错误——过期清理、主动登出、账号停用都会让它消失。
	Find(ctx context.Context, tokenHash string) (Session, bool, error)

	// Touch 更新 last_seen_at 并顺延 expiresAt（滑动过期）。
	//
	// 调用方负责节流：每个请求都写一次库，等于给每次页面加载
	// 加上一次写事务。与 tokens.last_used_at 同一套做法。
	Touch(ctx context.Context, tokenHash string, seenAt, expiresAt time.Time) error

	Delete(ctx context.Context, tokenHash string) error

	// DeleteByUser 废掉某人的全部会话。
	//
	// 这是整个接口存在的意义：停用账号、重置密码、改密之后，
	// 别处已经登录的浏览器必须当场失效。
	DeleteByUser(ctx context.Context, userID string) error

	// DeleteExpired 清理过期会话，返回删除行数。由运维或定时任务调用。
	DeleteExpired(ctx context.Context, before time.Time) (int, error)
}
