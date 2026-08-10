package governance

import (
	"context"
	"fmt"
	"sort"
	"strings"
	"time"

	"claracore-server/internal/platform"
	"claracore-server/internal/ports"
)

// tokenTouchInterval 是 last_used_at 的写入节流窗口。
//
// 每次调用都写一次 last_used_at，等于给认证这条最热的路径挂上一次写事务。
// 而这个字段的用途是「判断哪些 token 可以安全吊销」——分钟级精度足够。
const tokenTouchInterval = 5 * time.Minute

// Service 编排治理域的用例。
//
// 不持有任何配置或连接：时间来自 Clock（否则时间相关逻辑无法测试），
// ID 来自 IDGenerator（生成逻辑不散落），存储来自 Store 接口。
type Service struct {
	store Store
	clock platform.Clock
	ids   *platform.IDGenerator

	// hasher 与 sessions 是 Web 登录需要的两件基础设施，都在 ports/。
	//
	// 放在治理域而不是 Web Host：「谁是谁」的规则只该有一份。
	// 密码校验、会话签发、停用即失效——这些都是身份问题，
	// 让 Host 自己拼一遍的话，MCP 那侧的 token 认证与 Web 这侧的
	// 会话认证会长出两套「用户还有效吗」的判断。
	//
	// 二者可以为 nil：纯 MCP 部署用不到它们。用到时才报错，
	// 而不是在装配处强制所有部署都配齐（见 requireWebDeps）。
	hasher   ports.PasswordHasher
	sessions ports.SessionStore
}

func NewService(store Store, clock platform.Clock, ids *platform.IDGenerator, hasher ports.PasswordHasher, sessions ports.SessionStore) *Service {
	return &Service{store: store, clock: clock, ids: ids, hasher: hasher, sessions: sessions}
}

// Authenticate 用 bearer token 明文换取调用者身份。
//
// 这是整套系统唯一的身份来源。Desktop 通过 X-ClaraCore-Agent-ID header
// 声明身份的做法在 Server 版被彻底移除——参数化的身份等同于可伪造的身份
// （docs/03-mcp-contract.md §3.3）。
func (s *Service) Authenticate(ctx context.Context, plaintext string) (Identity, error) {
	hash, err := ParseToken(plaintext)
	if err != nil {
		return Identity{}, err
	}

	auth, err := s.store.FindAuthContext(ctx, hash)
	if err != nil {
		return Identity{}, err
	}

	now := s.clock.Now()
	if err := auth.Token.Validate(now); err != nil {
		return Identity{}, err
	}
	if !auth.Tenant.IsActive() {
		return Identity{}, ErrTenantInactive
	}
	if !auth.User.IsActive() {
		return Identity{}, ErrUserDisabled
	}
	if err := checkAuthConsistency(auth); err != nil {
		return Identity{}, err
	}

	s.touchToken(ctx, auth.Token, now)

	return Identity{
		TenantID:    auth.Token.TenantID,
		UserID:      auth.User.ID,
		AgentID:     auth.Agent.ID,
		TokenID:     auth.Token.ID,
		Username:    auth.User.Username,
		DisplayName: auth.User.DisplayName,
		Role:        auth.User.Role,
		AgentSlug:   auth.Agent.Slug,
		AgentName:   auth.Agent.DisplayName,
	}, nil
}

// checkAuthConsistency 防御数据损坏导致的越权。
//
// 正常情况下这些永远成立。但一旦某次错误的迁移或补数据让 token 指向了
// 别的租户的用户，没有这道检查就是一次静默的跨租户越权——多租户系统里
// 最不能接受的一类故障。宁可拒绝一次合法请求，不可放行一次越权。
func checkAuthConsistency(auth *AuthContext) error {
	switch {
	case auth.Token.UserID != auth.User.ID:
		return fmt.Errorf("%w: token 与用户不匹配", ErrTokenNotFound)
	case auth.Token.AgentID != auth.Agent.ID:
		return fmt.Errorf("%w: token 与 agent 不匹配", ErrTokenNotFound)
	case auth.Agent.UserID != auth.User.ID:
		return fmt.Errorf("%w: agent 不属于该用户", ErrTokenNotFound)
	case auth.Token.TenantID != auth.Tenant.ID,
		auth.User.TenantID != auth.Tenant.ID,
		auth.Agent.TenantID != auth.Tenant.ID:
		return fmt.Errorf("%w: 租户不一致", ErrTokenNotFound)
	}
	return nil
}

// touchToken 按节流窗口更新 last_used_at，失败不影响认证结果。
//
// last_used_at 是运维辅助信号，不是授权依据。让它的写失败拒绝一次合法调用，
// 是把次要信号放到了主路径上。
func (s *Service) touchToken(ctx context.Context, token Token, now time.Time) {
	if token.LastUsedAt != nil && now.Sub(*token.LastUsedAt) < tokenTouchInterval {
		return
	}
	_ = s.store.TouchToken(ctx, token.ID, now)
}

// Scope 解析调用者的空间权限，是 governance 交给 knowledge 域的全部内容。
func (s *Service) Scope(ctx context.Context, id Identity) (SpaceScope, error) {
	memberships, err := s.store.ListSpaceMemberships(ctx, id.TenantID, id.UserID)
	if err != nil {
		return SpaceScope{}, err
	}
	return ResolveScope(id.UserID, memberships), nil
}

// --- 治理面用例 ---

type ProvisionTenantInput struct {
	TenantName    string
	TeamSpaceName string
	AdminUsername string
	AdminDisplay  string
	// AdminPassword 是**明文**初始密码，由 Service 通过 ports.PasswordHasher 哈希。
	//
	// 2026-08-10 从 PasswordHash 改成明文：原来的设计让调用方自己哈希，
	// 结果是集成测试里塞了一堆 "$argon2id$e2e$fake" 这种永远验不过的假值，
	// 而没有任何地方能真正创建一个能登录的账号。哈希本身仍然不在领域层——
	// 它在 ports.PasswordHasher 的实现里，参数属于配置这条没有变。
	AdminPassword string
}

type ProvisionTenantResult struct {
	Tenant    Tenant
	Admin     User
	TeamSpace Space
	Personal  Space
}

// ProvisionTenant 开一个租户：租户 + 团队空间 + 管理员 + 管理员的个人空间。
//
// 四件事必须原子完成。任一半途失败留下的残缺状态都没有自愈路径：
// 没有团队空间的租户无处共享，没有个人空间的用户每次写入都会失败。
func (s *Service) ProvisionTenant(ctx context.Context, in ProvisionTenantInput) (ProvisionTenantResult, error) {
	if err := requireAll(map[string]string{
		"租户名":   in.TenantName,
		"管理员账号": in.AdminUsername,
		"初始密码":  in.AdminPassword,
	}); err != nil {
		return ProvisionTenantResult{}, err
	}
	hash, err := s.hashPassword(in.AdminPassword)
	if err != nil {
		return ProvisionTenantResult{}, err
	}

	now := s.clock.Now()
	tenant := Tenant{
		ID:        s.ids.New(),
		Name:      in.TenantName,
		Status:    StatusActive,
		CreatedAt: now,
	}
	admin := User{
		ID:           s.ids.New(),
		TenantID:     tenant.ID,
		Username:     in.AdminUsername,
		PasswordHash: hash,
		DisplayName:  defaultIfEmpty(in.AdminDisplay, in.AdminUsername),
		Role:         RoleAdmin,
		MustChangePw: true, // 首次登录强制改密
		Status:       StatusActive,
		CreatedAt:    now,
	}
	teamSpace := Space{
		ID:        s.ids.New(),
		TenantID:  tenant.ID,
		Type:      SpaceTeam,
		Name:      defaultIfEmpty(in.TeamSpaceName, in.TenantName),
		CreatedAt: now,
	}
	personal := personalSpaceFor(s.ids.New(), admin, now)

	err = s.store.Atomically(ctx, func(st Store) error {
		if err := st.CreateTenant(ctx, tenant); err != nil {
			return err
		}
		if err := st.CreateUser(ctx, admin); err != nil {
			return err
		}
		if err := st.CreateSpace(ctx, teamSpace); err != nil {
			return err
		}
		return st.CreateSpace(ctx, personal)
	})
	if err != nil {
		return ProvisionTenantResult{}, err
	}

	return ProvisionTenantResult{
		Tenant: tenant, Admin: admin, TeamSpace: teamSpace, Personal: personal,
	}, nil
}

type CreateUserInput struct {
	TenantID    platform.ID
	Username    string
	DisplayName string
	// Password 是明文初始密码，由 Service 哈希（见 ProvisionTenantInput.AdminPassword）。
	Password string
	Role     string
}

// CreateUser 建用户并同时建其个人空间。
//
// 个人空间不是「用户第一次写入时惰性创建」——惰性创建在并发下会撞上
// uk_spaces_personal 唯一约束，而那时的报错发生在一次普通的写记忆调用里，
// 用户完全无从理解。
func (s *Service) CreateUser(ctx context.Context, in CreateUserInput) (User, Space, error) {
	if err := requireAll(map[string]string{
		"账号":   in.Username,
		"初始密码": in.Password,
	}); err != nil {
		return User{}, Space{}, err
	}
	hash, err := s.hashPassword(in.Password)
	if err != nil {
		return User{}, Space{}, err
	}
	if in.TenantID == "" {
		return User{}, Space{}, fmt.Errorf("%w: 缺少租户", ErrInvalidArgument)
	}
	role := defaultIfEmpty(in.Role, RoleMember)
	if role != RoleAdmin && role != RoleMember {
		return User{}, Space{}, fmt.Errorf("%w: 角色取值非法 %q", ErrInvalidArgument, role)
	}

	now := s.clock.Now()
	user := User{
		ID:           s.ids.New(),
		TenantID:     in.TenantID,
		Username:     in.Username,
		PasswordHash: hash,
		DisplayName:  defaultIfEmpty(in.DisplayName, in.Username),
		Role:         role,
		MustChangePw: true,
		Status:       StatusActive,
		CreatedAt:    now,
	}
	personal := personalSpaceFor(s.ids.New(), user, now)

	err = s.store.Atomically(ctx, func(st Store) error {
		if err := st.CreateUser(ctx, user); err != nil {
			return err
		}
		return st.CreateSpace(ctx, personal)
	})
	if err != nil {
		return User{}, Space{}, err
	}
	return user, personal, nil
}

// CreateAgent 注册一个 agent。
//
// 注意这里**不涉及任何权限操作**：agent 继承其所有者的全部权限，
// 新增 agent 不需要配任何东西（docs/02-data-model.md §2.1）。
func (s *Service) CreateAgent(ctx context.Context, tenantID, userID platform.ID, slug, displayName string) (Agent, error) {
	if err := requireAll(map[string]string{"agent 标识": slug}); err != nil {
		return Agent{}, err
	}
	agent := Agent{
		ID:          s.ids.New(),
		TenantID:    tenantID,
		UserID:      userID,
		Slug:        slug,
		DisplayName: defaultIfEmpty(displayName, slug),
		CreatedAt:   s.clock.Now(),
	}
	if err := s.store.CreateAgent(ctx, agent); err != nil {
		return Agent{}, err
	}
	return agent, nil
}

// CreateProjectSpace 建项目空间。创建者即 owner，不额外写授权行——
// owner 的读写权限由 ResolveScope 规则保证，不依赖一行可被误删的数据。
// 其他成员必须显式加入（2026-08-06 拍板）。
func (s *Service) CreateProjectSpace(ctx context.Context, tenantID, ownerUserID platform.ID, name string) (Space, error) {
	if err := requireAll(map[string]string{"空间名": name}); err != nil {
		return Space{}, err
	}
	owner := ownerUserID
	space := Space{
		ID:          s.ids.New(),
		TenantID:    tenantID,
		Type:        SpaceProject,
		Name:        name,
		OwnerUserID: &owner,
		CreatedAt:   s.clock.Now(),
	}
	if err := s.store.CreateSpace(ctx, space); err != nil {
		return Space{}, err
	}
	return space, nil
}

type IssueTokenInput struct {
	TenantID platform.ID
	UserID   platform.ID
	AgentID  platform.ID
	Name     string
	TTL      time.Duration // 0 表示不过期
}

// IssueToken 签发 token，返回的明文**只在此刻存在一次**。
//
// 不吊销该 agent 的其他 token：一个 agent 多 token 并存是平滑轮换的前提
// （Desktop 单 token 轮换即全断的教训）。轮换流程是签发新的 → 各设备逐个更新
// → 观察旧 token 的 last_used_at 不再变化 → 吊销。
func (s *Service) IssueToken(ctx context.Context, in IssueTokenInput) (plaintext string, token Token, err error) {
	if in.TenantID == "" || in.UserID == "" || in.AgentID == "" {
		return "", Token{}, fmt.Errorf("%w: 签发 token 需要租户、用户与 agent", ErrInvalidArgument)
	}

	plaintext, prefix, hash, err := NewTokenSecret()
	if err != nil {
		return "", Token{}, err
	}

	now := s.clock.Now()
	token = Token{
		ID:        s.ids.New(),
		TenantID:  in.TenantID,
		UserID:    in.UserID,
		AgentID:   in.AgentID,
		Name:      defaultIfEmpty(in.Name, "未命名"),
		Prefix:    prefix,
		Hash:      hash,
		CreatedAt: now,
	}
	if in.TTL > 0 {
		expires := now.Add(in.TTL)
		token.ExpiresAt = &expires
	}

	if err := s.store.CreateToken(ctx, token); err != nil {
		return "", Token{}, err
	}
	return plaintext, token, nil
}

func (s *Service) RevokeToken(ctx context.Context, tenantID, tokenID platform.ID) error {
	return s.store.RevokeToken(ctx, tenantID, tokenID, s.clock.Now())
}

func (s *Service) ListTokens(ctx context.Context, tenantID, userID platform.ID) ([]Token, error) {
	return s.store.ListTokens(ctx, tenantID, userID)
}

// GrantSpaceAccess 授予某人对某空间的读写权限。
//
// 个人空间不可授权他人：它的封闭性是设计约束而非默认值，是用户敢于随手
// 记录的前提（docs/02-data-model.md §2.2）。要共享就走 promote 复制一份。
func (s *Service) GrantSpaceAccess(ctx context.Context, tenantID, spaceID, userID, grantedBy platform.ID, canRead, canWrite bool) error {
	space, err := s.store.FindSpace(ctx, tenantID, spaceID)
	if err != nil {
		return err
	}
	if space.Type == SpacePersonal {
		return fmt.Errorf("%w: 个人空间不可授权他人，请改用 promote", ErrSpaceForbidden)
	}
	if !canRead && !canWrite {
		return fmt.Errorf("%w: 授权至少要包含读或写，撤销请用 RevokeSpaceAccess", ErrInvalidArgument)
	}
	// 可写必然可读：允许「只写不读」会造出一个能写进去却看不见的空间，
	// 那是权限配置错误，不是一种有意义的权限。
	if canWrite {
		canRead = true
	}

	return s.store.GrantPermission(ctx, Permission{
		SpaceID:   spaceID,
		UserID:    userID,
		CanRead:   canRead,
		CanWrite:  canWrite,
		GrantedBy: grantedBy,
		CreatedAt: s.clock.Now(),
	})
}

func (s *Service) RevokeSpaceAccess(ctx context.Context, spaceID, userID platform.ID) error {
	return s.store.RevokePermission(ctx, spaceID, userID)
}

// --- 内部工具 ---

func personalSpaceFor(id platform.ID, u User, now time.Time) Space {
	owner := u.ID
	return Space{
		ID:          id,
		TenantID:    u.TenantID,
		Type:        SpacePersonal,
		Name:        u.DisplayName,
		OwnerUserID: &owner,
		CreatedAt:   now,
	}
}

func requireAll(fields map[string]string) error {
	var missing []string
	for name, value := range fields {
		if strings.TrimSpace(value) == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) == 0 {
		return nil
	}
	// map 遍历顺序随机，排序后错误信息才稳定可测
	sort.Strings(missing)
	return fmt.Errorf("%w: 缺少 %s", ErrInvalidArgument, strings.Join(missing, "、"))
}

func defaultIfEmpty(v, fallback string) string {
	if strings.TrimSpace(v) == "" {
		return fallback
	}
	return v
}
