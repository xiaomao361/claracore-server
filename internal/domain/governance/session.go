package governance

import (
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"fmt"
	"strings"
	"time"

	"claracore-server/internal/platform"
	"claracore-server/internal/ports"
)

// 会话相关的两个节流/长度常量。
const (
	// sessionTTL 是一次登录能活多久（滑动）。
	//
	// 七天：治理面是低频操作，天天要求重新登录只会让人把密码写在便签上。
	// 「立即吊销」的能力由 sessions 存库提供，不靠短 TTL 兜底。
	sessionTTL = 7 * 24 * time.Hour

	// sessionTouchInterval 是 last_seen_at 的写入节流窗口。
	// 每个请求都写一次，等于给每次页面加载挂上一次写事务。
	sessionTouchInterval = 5 * time.Minute

	// sessionTokenBytes 是会话令牌的随机长度。32 字节 = 256 bit。
	sessionTokenBytes = 32
)

// Principal 是 Web 会话背后的**人**。
//
// 与 Identity 的区别是没有 agent——Web 是人在用（docs/04-web-scope.md §1.3
// 的工作面 / 治理面切分）。硬把 Identity 复用过来的话，agent 字段就得填
// 一个假值，而那个假值会一路流进审计日志，让「这条是人做的还是 agent 做的」
// 再也说不清。
type Principal struct {
	TenantID    platform.ID
	UserID      platform.ID
	Username    string
	DisplayName string
	Role        string

	// MustChangePassword 为真时，Host 必须把人挡在改密页——
	// 管理员分配的初始密码在改掉之前不该能做任何别的事。
	MustChangePassword bool

	SessionExpiresAt time.Time
}

func (p Principal) IsAdmin() bool { return p.Role == RoleAdmin }

// SessionToken 是签发给浏览器的明文令牌，**只在登录那一刻存在**。
type SessionToken string

// hashSessionToken 与 tokens 表同一条纪律：库里只有哈希。
func hashSessionToken(plaintext string) string {
	sum := sha256.Sum256([]byte(plaintext))
	return hex.EncodeToString(sum[:])
}

// requireWebDeps 检查这个部署配齐了 Web 需要的两件基础设施。
//
// 返回错误而不是在装配处强制：纯 MCP 部署压根不需要它们，
// 让所有部署都配一份用不上的东西是无谓的摩擦。
func (s *Service) requireWebDeps() error {
	if s.hasher == nil || s.sessions == nil {
		return fmt.Errorf("%w: 这个部署没有配置密码哈希或会话存储", ErrInvalidArgument)
	}
	return nil
}

func (s *Service) hashPassword(plaintext string) (string, error) {
	if s.hasher == nil {
		return "", fmt.Errorf("%w: 这个部署没有配置密码哈希", ErrInvalidArgument)
	}
	if err := checkPasswordStrength(plaintext); err != nil {
		return "", err
	}
	return s.hasher.Hash(plaintext)
}

// MinPasswordLen 是密码长度下限。
//
// 只卡长度，不卡「必须含大写字母和符号」：那类规则的实际效果是
// 让人把 Password1! 写在便签上。封闭部署、无自助注册、账号由管理员分配，
// 长度是这里唯一值得强制的东西。
const MinPasswordLen = 10

func checkPasswordStrength(plaintext string) error {
	if len([]rune(plaintext)) < MinPasswordLen {
		return fmt.Errorf("%w: 密码至少 %d 个字符", ErrInvalidArgument, MinPasswordLen)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 登录
// ---------------------------------------------------------------------------

// Login 用账号密码换一张会话票。
//
// # 为什么不需要传租户
//
// 「一个团队一套部署单元」是 docs/02-data-model.md §2.1 的设计前提：
// 一个部署对应一个租户。多租户的列存在是为了隔离与将来的可能性，
// 不是因为一个部署要托管多个团队。所以登录页只问账号密码，
// 租户由部署自己解析——库里有多于一个活跃租户时**报错而不是猜**
// （见 resolveTenant）。
//
// # 失败为什么只有一种错误
//
// 账号不存在、密码不对、账号已停用，对外都是 ErrInvalidCredentials。
// 区分它们等于提供一个账号枚举接口：攻击者用不同的用户名试一遍，
// 就能得到一份有效账号清单。停用的账号尤其不能说——那等于告诉对方
// 「这个人存在，只是被关了」。
func (s *Service) Login(ctx context.Context, username, password string) (SessionToken, Principal, error) {
	if err := s.requireWebDeps(); err != nil {
		return "", Principal{}, err
	}
	username = strings.TrimSpace(username)
	if username == "" || password == "" {
		return "", Principal{}, ErrInvalidCredentials
	}

	tenant, err := s.resolveTenant(ctx)
	if err != nil {
		return "", Principal{}, err
	}

	user, err := s.store.FindUserByUsername(ctx, tenant.ID, username)
	if err != nil {
		// 账号不存在也走同一个出口，见方法注释
		return "", Principal{}, ErrInvalidCredentials
	}

	ok, err := s.hasher.Verify(password, user.PasswordHash)
	if err != nil {
		// 哈希串本身有问题（迁移事故、测试假值）——这是数据问题不是用户问题，
		// 但对外仍然只说凭据不对。调用方负责把 err 记进日志。
		return "", Principal{}, fmt.Errorf("%w: %v", ErrInvalidCredentials, err)
	}
	if !ok || !user.IsActive() || !tenant.IsActive() {
		return "", Principal{}, ErrInvalidCredentials
	}

	return s.issueSession(ctx, *user)
}

// resolveTenant 解析这个部署对应的租户。
//
// 多于一个活跃租户时报错：那是一次运维事故（同一个库被两套部署共用），
// 而在这种状态下猜一个出来，等于让某个人登进了别人的团队。
func (s *Service) resolveTenant(ctx context.Context) (Tenant, error) {
	tenants, err := s.store.ListTenants(ctx)
	if err != nil {
		return Tenant{}, err
	}
	var active []Tenant
	for _, t := range tenants {
		if t.IsActive() {
			active = append(active, t)
		}
	}
	switch len(active) {
	case 1:
		return active[0], nil
	case 0:
		return Tenant{}, fmt.Errorf("%w: 这个部署还没有开过租户", ErrTenantInactive)
	default:
		return Tenant{}, fmt.Errorf("%w: 库里有 %d 个活跃租户，登录无法确定属于哪一个（一个部署对应一个租户）",
			ErrTenantInactive, len(active))
	}
}

func (s *Service) issueSession(ctx context.Context, user User) (SessionToken, Principal, error) {
	raw := make([]byte, sessionTokenBytes)
	if _, err := rand.Read(raw); err != nil {
		return "", Principal{}, fmt.Errorf("governance: 生成会话令牌失败: %w", err)
	}
	plaintext := base64.RawURLEncoding.EncodeToString(raw)

	now := s.clock.Now()
	expires := now.Add(sessionTTL)
	if err := s.sessions.Create(ctx, ports.Session{
		TokenHash:  hashSessionToken(plaintext),
		TenantID:   string(user.TenantID),
		UserID:     string(user.ID),
		ExpiresAt:  expires,
		CreatedAt:  now,
		LastSeenAt: now,
	}); err != nil {
		return "", Principal{}, err
	}
	return SessionToken(plaintext), principalOf(user, expires), nil
}

func principalOf(u User, expires time.Time) Principal {
	return Principal{
		TenantID:           u.TenantID,
		UserID:             u.ID,
		Username:           u.Username,
		DisplayName:        u.DisplayName,
		Role:               u.Role,
		MustChangePassword: u.MustChangePw,
		SessionExpiresAt:   expires,
	}
}

// ResumeSession 用 cookie 里的令牌换回当前的人。
//
// **每次都回查用户行**，不把角色和状态缓存进会话：一个刚被停用、
// 或者刚被降级的账号，必须在下一次请求就失效。把这些字段冻在会话里
// 是个常见的优化，代价是「停用」变成「等他下次重新登录才生效」——
// 而那正是治理面最不能接受的一种延迟。
func (s *Service) ResumeSession(ctx context.Context, token string) (Principal, error) {
	if err := s.requireWebDeps(); err != nil {
		return Principal{}, err
	}
	if token == "" {
		return Principal{}, ErrSessionInvalid
	}
	hash := hashSessionToken(token)

	sess, ok, err := s.sessions.Find(ctx, hash)
	if err != nil {
		return Principal{}, err
	}
	now := s.clock.Now()
	if !ok || sess.Expired(now) {
		return Principal{}, ErrSessionInvalid
	}

	user, err := s.store.FindUserByID(ctx, platform.ID(sess.TenantID), platform.ID(sess.UserID))
	if err != nil {
		return Principal{}, ErrSessionInvalid
	}
	if !user.IsActive() {
		// 会话还在但人已经停用：顺手清掉，别让它继续占着
		_ = s.sessions.Delete(ctx, hash)
		return Principal{}, ErrSessionInvalid
	}

	expires := sess.ExpiresAt
	// 滑动过期，带节流：还在用的会话不该在开着的标签页里突然掉线，
	// 但也不必每个请求都写一次库（与 tokens.last_used_at 同一套做法）。
	if now.Sub(sess.LastSeenAt) >= sessionTouchInterval {
		expires = now.Add(sessionTTL)
		if err := s.sessions.Touch(ctx, hash, now, expires); err != nil {
			// 写失败不影响这次认证：它是运维辅助信号，不是授权依据
			expires = sess.ExpiresAt
		}
	}
	return principalOf(*user, expires), nil
}

func (s *Service) Logout(ctx context.Context, token string) error {
	if err := s.requireWebDeps(); err != nil {
		return err
	}
	if token == "" {
		return nil // 幂等：没登录也算登出成功
	}
	return s.sessions.Delete(ctx, hashSessionToken(token))
}

// ---------------------------------------------------------------------------
// 密码与账号状态
// ---------------------------------------------------------------------------

// ChangePassword 是本人改密：必须验旧密码。
//
// 改完**废掉这个人的全部会话**（包括当前这条）。这是有意的：
// 改密最常见的动机就是「我怀疑密码泄露了」，而那时别处已经登录的
// 浏览器必须一起掉线，否则改密什么也没解决。Host 负责改完之后
// 重新签发一张会话，让本人不必再登一次。
func (s *Service) ChangePassword(ctx context.Context, tenantID, userID platform.ID, oldPassword, newPassword string) error {
	if err := s.requireWebDeps(); err != nil {
		return err
	}
	user, err := s.store.FindUserByID(ctx, tenantID, userID)
	if err != nil {
		return err
	}
	ok, err := s.hasher.Verify(oldPassword, user.PasswordHash)
	if err != nil || !ok {
		return ErrInvalidCredentials
	}
	if oldPassword == newPassword {
		return fmt.Errorf("%w: 新密码不能与旧密码相同", ErrInvalidArgument)
	}
	hash, err := s.hashPassword(newPassword)
	if err != nil {
		return err
	}
	if err := s.store.UpdateUserPassword(ctx, tenantID, userID, hash, false, s.clock.Now()); err != nil {
		return err
	}
	return s.sessions.DeleteByUser(ctx, string(userID))
}

// ResetPassword 是管理员重置他人密码。
//
// 与 ChangePassword 的两处不同都是有意的：不验旧密码（管理员本来就不知道），
// 且把 must_change_pw 置回真——管理员知道的那个密码不该长期有效。
func (s *Service) ResetPassword(ctx context.Context, tenantID, userID platform.ID, newPassword string) error {
	if err := s.requireWebDeps(); err != nil {
		return err
	}
	if _, err := s.store.FindUserByID(ctx, tenantID, userID); err != nil {
		return err
	}
	hash, err := s.hashPassword(newPassword)
	if err != nil {
		return err
	}
	if err := s.store.UpdateUserPassword(ctx, tenantID, userID, hash, true, s.clock.Now()); err != nil {
		return err
	}
	// 别处登录的会话必须当场失效——这正是 sessions 存库而非签名 cookie 的意义
	return s.sessions.DeleteByUser(ctx, string(userID))
}

// SetUserStatus 启用或停用账号。
//
// 停用时废掉全部会话，但**不吊销 token**：那是两件事。token 会在下一次
// 认证时被 checkAuthConsistency 之外的用户状态检查拦下
// （Authenticate 会查 user.IsActive），所以停用是立即生效的；
// 而主动吊销 token 是另一个动作，管理员可能只想临时停一个人的 Web 访问。
func (s *Service) SetUserStatus(ctx context.Context, tenantID, userID platform.ID, status string) error {
	if err := s.requireWebDeps(); err != nil {
		return err
	}
	if status != StatusActive && status != StatusDisabled {
		return fmt.Errorf("%w: 账号状态取值非法 %q", ErrInvalidArgument, status)
	}
	if err := s.store.UpdateUserStatus(ctx, tenantID, userID, status, s.clock.Now()); err != nil {
		return err
	}
	if status == StatusActive {
		return nil
	}
	return s.sessions.DeleteByUser(ctx, string(userID))
}

// ListUsers 列出租户下的全部账号（成员管理页）。
func (s *Service) ListUsers(ctx context.Context, tenantID platform.ID) ([]User, error) {
	return s.store.ListUsers(ctx, tenantID)
}

// FindUser 按 ID 取一个账号，用于管理页的详情与校验。
func (s *Service) FindUser(ctx context.Context, tenantID, userID platform.ID) (User, error) {
	u, err := s.store.FindUserByID(ctx, tenantID, userID)
	if err != nil {
		return User{}, err
	}
	return *u, nil
}

// CleanupSessions 删掉已过期的会话行，返回删除数量。由运维或定时任务调用。
func (s *Service) CleanupSessions(ctx context.Context) (int, error) {
	if err := s.requireWebDeps(); err != nil {
		return 0, err
	}
	return s.sessions.DeleteExpired(ctx, s.clock.Now())
}
