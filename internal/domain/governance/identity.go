package governance

import (
	"time"

	"claracore-server/internal/platform"
)

// TenantStatus / UserStatus 的取值。用 const 而非 var——
// 领域层禁止包级可变状态（test/arch_test.go）。
const (
	StatusActive    = "active"
	StatusSuspended = "suspended"
	StatusDisabled  = "disabled"
)

// 用户角色。权限只有这两级：admin 管治理面，member 用知识面。
const (
	RoleAdmin  = "admin"
	RoleMember = "member"
)

type Tenant struct {
	ID        platform.ID
	Name      string
	Status    string
	CreatedAt time.Time
}

func (t Tenant) IsActive() bool { return t.Status == StatusActive }

type User struct {
	ID           platform.ID
	TenantID     platform.ID
	Username     string
	PasswordHash string
	DisplayName  string
	Role         string
	MustChangePw bool
	Status       string
	CreatedAt    time.Time
}

func (u User) IsActive() bool { return u.Status == StatusActive }
func (u User) IsAdmin() bool  { return u.Role == RoleAdmin }

// Agent 是审计维度，不是权限维度（docs/02-data-model.md §2.1）。
//
// 这个类型上没有任何权限方法，是有意为之：agent 继承其所有者的全部权限，
// 一旦这里出现 CanXxx，就意味着有人开始在 agent 上配权限了。
type Agent struct {
	ID          platform.ID
	TenantID    platform.ID
	UserID      platform.ID
	Slug        string
	DisplayName string
	CreatedAt   time.Time
}

type Token struct {
	ID         platform.ID
	TenantID   platform.ID
	UserID     platform.ID
	AgentID    platform.ID
	Name       string
	Prefix     string
	Hash       string
	ExpiresAt  *time.Time
	LastUsedAt *time.Time
	RevokedAt  *time.Time
	CreatedAt  time.Time
}

// Validate 检查 token 自身的可用性，不涉及用户与租户状态。
func (t Token) Validate(now time.Time) error {
	if t.RevokedAt != nil {
		return ErrTokenRevoked
	}
	if t.ExpiresAt != nil && !now.Before(*t.ExpiresAt) {
		return ErrTokenExpired
	}
	return nil
}

// Identity 是一次调用背后的完整身份，全部由 token 反查得来。
//
// 任何 MCP 工具都不接受 agentId / userId / tenantId 参数
// （docs/03-mcp-contract.md §3.3）——参数化的身份等同于可伪造的身份。
type Identity struct {
	TenantID    platform.ID
	UserID      platform.ID
	AgentID     platform.ID
	TokenID     platform.ID
	Username    string
	DisplayName string
	Role        string
	AgentSlug   string
	AgentName   string
}

func (i Identity) IsAdmin() bool { return i.Role == RoleAdmin }
