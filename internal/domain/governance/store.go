package governance

import (
	"context"
	"time"

	"claracore-server/internal/platform"
)

// AuthContext 是一次认证所需的全部数据行。
//
// 合成一个方法而非四个 Find，是因为认证在**每一次 MCP 调用**上都会发生：
// 四次往返 × 每次工具调用，是这套系统里最容易被忽视的延迟来源。
// adapters 用一条 JOIN 取回即可。
//
// 注意这里只取行，不做判断——token 是否过期、用户是否停用、租户是否 active，
// 全部由领域层的 Service.Authenticate 决定。SQL 里不写 `WHERE revoked_at IS NULL`：
// 那会让「为什么我的 token 不能用」退化成一句无法区分的「token 不存在」。
type AuthContext struct {
	Token  Token
	User   User
	Agent  Agent
	Tenant Tenant
}

// Store 是治理域的持久化出口。
//
// 接口定义在本包而非 ports/：方法签名涉及领域类型，放 ports 会造成
// domain → ports → domain 的循环 import（docs/05-code-structure.md §3.1）。
//
// 未找到时返回本包的哨兵错误（ErrTokenNotFound / ErrUserNotFound / ErrSpaceNotFound），
// 而不是 (nil, nil) —— 后者会让每个调用点都得记得判空。
//
// Create* 系列在违反唯一性约束时返回 ErrAlreadyExists（账号重名、agent slug
// 重复、第二个个人空间/团队空间）。这些约束由存储层强制——应用层的
// SELECT-then-INSERT 预检在并发下必然失效。
type Store interface {
	// --- 认证路径（热路径，每次调用都走） ---

	FindAuthContext(ctx context.Context, tokenHash string) (*AuthContext, error)

	// TouchToken 更新 last_used_at。用于识别哪些 token 可以安全吊销。
	TouchToken(ctx context.Context, tokenID platform.ID, at time.Time) error

	// ListSpaceMemberships 返回与该用户有关的空间及其显式授权行（可能为 nil）。
	//
	// 「有关」= 他自己的个人空间 + 本租户的团队空间 + 他拥有或被授权的项目空间。
	// 实现只负责捞行，不做权限判断——规则在 ResolveScope 里，只此一份。
	ListSpaceMemberships(ctx context.Context, tenantID, userID platform.ID) ([]SpaceMembership, error)

	// --- 治理面（Web 管理用，低频） ---

	CreateTenant(ctx context.Context, t Tenant) error
	CreateUser(ctx context.Context, u User) error
	CreateAgent(ctx context.Context, a Agent) error
	CreateSpace(ctx context.Context, s Space) error
	CreateToken(ctx context.Context, t Token) error

	FindUserByUsername(ctx context.Context, tenantID platform.ID, username string) (*User, error)
	FindSpace(ctx context.Context, tenantID, spaceID platform.ID) (*Space, error)

	// ListTokens 返回该用户的全部 token（含已吊销的，用于审计展示）。
	// 明文无从取回，返回值中只有 prefix。
	ListTokens(ctx context.Context, tenantID, userID platform.ID) ([]Token, error)

	// RevokeToken 幂等：已吊销的再次吊销不报错，不覆盖原时间。
	RevokeToken(ctx context.Context, tenantID, tokenID platform.ID, at time.Time) error

	// GrantPermission 插入或覆盖一行授权。
	GrantPermission(ctx context.Context, p Permission) error
	RevokePermission(ctx context.Context, spaceID, userID platform.ID) error

	// Atomically 在一个原子单元内执行 fn，fn 收到的 Store 与外层同属该单元。
	//
	// 领域层有几个操作跨多个实体：建用户必须同时建个人空间、建租户必须同时建
	// 团队空间。半途失败留下的「有用户没有个人空间」会让此后每次写入都撞上
	// ErrNoPersonalSpace，而且没有任何自愈路径。
	//
	// 接口不叫 BeginTx / Commit：领域层要表达的是「原子地做这几件事」，
	// 至于底下是 SQL 事务还是别的什么，不是它该知道的。
	Atomically(ctx context.Context, fn func(Store) error) error
}
