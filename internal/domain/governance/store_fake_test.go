package governance

import (
	"claracore-server/internal/ports"
	"context"
	"fmt"
	"maps"
	"slices"
	"strings"
	"time"

	"claracore-server/internal/platform"
)

// fakeStore 是领域层测试用的内存 Store。
//
// 领域单测必须快且无外部依赖（docs/05-code-structure.md §6.4）——
// 用真实 MySQL 测权限规则，测的就不再是规则本身了。
type fakeStore struct {
	tenants map[platform.ID]Tenant
	users   map[platform.ID]User
	agents  map[platform.ID]Agent
	tokens  map[platform.ID]Token
	spaces  map[platform.ID]Space
	perms   map[permKey]Permission

	// 故障注入
	failOnCreateSpace         bool
	failOnTouch               bool
	beforePasswordHashUpgrade func(*fakeStore, platform.ID)

	touchCalls int
}

type permKey struct {
	space platform.ID
	user  platform.ID
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		tenants: map[platform.ID]Tenant{},
		users:   map[platform.ID]User{},
		agents:  map[platform.ID]Agent{},
		tokens:  map[platform.ID]Token{},
		spaces:  map[platform.ID]Space{},
		perms:   map[permKey]Permission{},
	}
}

func (f *fakeStore) clone() *fakeStore {
	c := newFakeStore()
	maps.Copy(c.tenants, f.tenants)
	maps.Copy(c.users, f.users)
	maps.Copy(c.agents, f.agents)
	maps.Copy(c.tokens, f.tokens)
	maps.Copy(c.spaces, f.spaces)
	maps.Copy(c.perms, f.perms)
	c.failOnCreateSpace = f.failOnCreateSpace
	c.failOnTouch = f.failOnTouch
	c.beforePasswordHashUpgrade = f.beforePasswordHashUpgrade
	return c
}

func (f *fakeStore) restore(from *fakeStore) {
	f.tenants, f.users, f.agents = from.tenants, from.users, from.agents
	f.tokens, f.spaces, f.perms = from.tokens, from.spaces, from.perms
}

func (f *fakeStore) FindAuthContext(_ context.Context, tokenHash string) (*AuthContext, error) {
	for _, t := range f.tokens {
		if t.Hash != tokenHash {
			continue
		}
		user, ok := f.users[t.UserID]
		if !ok {
			return nil, ErrUserNotFound
		}
		agent, ok := f.agents[t.AgentID]
		if !ok {
			return nil, ErrAgentNotFound
		}
		tenant, ok := f.tenants[t.TenantID]
		if !ok {
			return nil, ErrTenantInactive
		}
		return &AuthContext{
			Token: AuthToken{
				ID: t.ID, TenantID: t.TenantID, UserID: t.UserID, AgentID: t.AgentID,
				ExpiresAt: t.ExpiresAt, LastUsedAt: t.LastUsedAt, RevokedAt: t.RevokedAt,
			},
			User: AuthUser{
				ID: user.ID, TenantID: user.TenantID, Username: user.Username,
				DisplayName: user.DisplayName, Role: user.Role, Status: user.Status,
			},
			Agent: AuthAgent{
				ID: agent.ID, TenantID: agent.TenantID, UserID: agent.UserID,
				Slug: agent.Slug, DisplayName: agent.DisplayName,
			},
			Tenant: AuthTenant{ID: tenant.ID, Status: tenant.Status},
		}, nil
	}
	return nil, ErrTokenNotFound
}

func (f *fakeStore) TouchToken(_ context.Context, tokenID platform.ID, at time.Time) error {
	f.touchCalls++
	if f.failOnTouch {
		return errFake
	}
	t, ok := f.tokens[tokenID]
	if !ok {
		return ErrTokenNotFound
	}
	t.LastUsedAt = &at
	f.tokens[tokenID] = t
	return nil
}

func (f *fakeStore) ListSpaceMemberships(_ context.Context, tenantID, userID platform.ID) ([]SpaceMembership, error) {
	var out []SpaceMembership
	for _, s := range f.spaces {
		if s.TenantID != tenantID {
			continue
		}
		relevant := s.Type == SpaceTeam || s.IsOwnedBy(userID)
		var p *Permission
		if perm, ok := f.perms[permKey{s.ID, userID}]; ok {
			relevant = true
			cp := perm
			p = &cp
		}
		if relevant {
			out = append(out, SpaceMembership{Space: s, Permission: p})
		}
	}
	return out, nil
}

func (f *fakeStore) CreateTenant(_ context.Context, t Tenant) error {
	f.tenants[t.ID] = t
	return nil
}

// 下面几个 Create 都带唯一性检查。这不是为了模拟得像，是因为**不检查的
// fake 会让领域测试跑在一个数据库不可能达到的状态上**：真实的 CreateUser
// 撞上重名会返回 ErrAlreadyExists，而一个用 map 直接赋值的 fake 会静默把
// 原用户覆盖掉——测试全绿，线上行为完全不同。
//
// 约束与 migrations/0001_governance.sql 里的唯一键一一对应。

func (f *fakeStore) CreateUser(_ context.Context, u User) error {
	for _, existing := range f.users {
		if existing.TenantID == u.TenantID && existing.Username == u.Username {
			return ErrAlreadyExists // uk_users_tenant_username
		}
	}
	f.users[u.ID] = u
	return nil
}

func (f *fakeStore) CreateAgent(_ context.Context, a Agent) error {
	for _, existing := range f.agents {
		if existing.UserID == a.UserID && existing.Slug == a.Slug {
			return ErrAlreadyExists // uk_agents_user_slug
		}
	}
	f.agents[a.ID] = a
	return nil
}

func (f *fakeStore) CreateSpace(_ context.Context, s Space) error {
	if f.failOnCreateSpace {
		return errFake
	}
	for _, existing := range f.spaces {
		if existing.TenantID != s.TenantID {
			continue
		}
		switch {
		case s.Type == SpacePersonal && existing.Type == SpacePersonal &&
			s.OwnerUserID != nil && existing.IsOwnedBy(*s.OwnerUserID):
			return ErrAlreadyExists // uk_spaces_personal
		case s.Type == SpaceTeam && existing.Type == SpaceTeam:
			return ErrAlreadyExists // uk_spaces_team
		}
	}
	f.spaces[s.ID] = s
	return nil
}

func (f *fakeStore) CreateToken(_ context.Context, t Token) error {
	for _, existing := range f.tokens {
		if existing.Hash == t.Hash {
			return ErrAlreadyExists // uk_tokens_hash
		}
	}
	f.tokens[t.ID] = t
	return nil
}

func (f *fakeStore) FindUserByUsername(_ context.Context, tenantID platform.ID, username string) (*User, error) {
	for _, u := range f.users {
		if u.TenantID == tenantID && u.Username == username {
			cp := u
			return &cp, nil
		}
	}
	return nil, ErrUserNotFound
}

func (f *fakeStore) FindSpace(_ context.Context, tenantID, spaceID platform.ID) (*Space, error) {
	s, ok := f.spaces[spaceID]
	if !ok || s.TenantID != tenantID {
		return nil, ErrSpaceNotFound
	}
	cp := s
	return &cp, nil
}

func (f *fakeStore) ListTokens(_ context.Context, tenantID, userID platform.ID) ([]Token, error) {
	var out []Token
	for _, t := range f.tokens {
		if t.TenantID == tenantID && t.UserID == userID {
			out = append(out, t)
		}
	}
	return out, nil
}

func (f *fakeStore) ListUserSummaries(_ context.Context, query GovernancePageQuery) ([]UserSummary, error) {
	var out []UserSummary
	for _, user := range f.users {
		if user.TenantID != query.TenantID || query.BeforeID != "" && user.ID >= query.BeforeID {
			continue
		}
		out = append(out, UserSummary{
			ID: user.ID, TenantID: user.TenantID, Username: user.Username, DisplayName: user.DisplayName,
			Role: user.Role, MustChangePw: user.MustChangePw, Status: user.Status, CreatedAt: user.CreatedAt,
		})
	}
	slices.SortFunc(out, func(a, b UserSummary) int { return strings.Compare(string(b.ID), string(a.ID)) })
	if len(out) > query.Limit {
		out = out[:query.Limit]
	}
	return out, nil
}

func (f *fakeStore) FindUserSummary(_ context.Context, tenantID, userID platform.ID) (UserSummary, error) {
	user, ok := f.users[userID]
	if !ok || user.TenantID != tenantID {
		return UserSummary{}, ErrUserNotFound
	}
	return UserSummary{
		ID: user.ID, TenantID: user.TenantID, Username: user.Username, DisplayName: user.DisplayName,
		Role: user.Role, MustChangePw: user.MustChangePw, Status: user.Status, CreatedAt: user.CreatedAt,
	}, nil
}

func (f *fakeStore) ListTokenSummaries(_ context.Context, query GovernancePageQuery) ([]TokenSummary, error) {
	var out []TokenSummary
	for _, token := range f.tokens {
		if token.TenantID != query.TenantID || token.UserID != query.UserID || query.BeforeID != "" && token.ID >= query.BeforeID {
			continue
		}
		out = append(out, tokenSummaryFromToken(token))
	}
	slices.SortFunc(out, func(a, b TokenSummary) int { return strings.Compare(string(b.ID), string(a.ID)) })
	if len(out) > query.Limit {
		out = out[:query.Limit]
	}
	return out, nil
}

func (f *fakeStore) FindTokenSummary(_ context.Context, tenantID, userID, tokenID platform.ID) (TokenSummary, error) {
	token, ok := f.tokens[tokenID]
	if !ok || token.TenantID != tenantID || token.UserID != userID {
		return TokenSummary{}, ErrTokenNotFound
	}
	return tokenSummaryFromToken(token), nil
}

func tokenSummaryFromToken(token Token) TokenSummary {
	return TokenSummary{
		ID: token.ID, TenantID: token.TenantID, UserID: token.UserID, AgentID: token.AgentID,
		Name: token.Name, Prefix: token.Prefix, ExpiresAt: token.ExpiresAt, LastUsedAt: token.LastUsedAt,
		RevokedAt: token.RevokedAt, CreatedAt: token.CreatedAt,
	}
}

func (f *fakeStore) ListGovernedSpaces(_ context.Context, query GovernancePageQuery) ([]Space, error) {
	var out []Space
	for _, space := range f.spaces {
		if space.TenantID != query.TenantID || space.Type == SpacePersonal || query.BeforeID != "" && space.ID >= query.BeforeID {
			continue
		}
		out = append(out, space)
	}
	slices.SortFunc(out, func(a, b Space) int { return strings.Compare(string(b.ID), string(a.ID)) })
	if len(out) > query.Limit {
		out = out[:query.Limit]
	}
	return out, nil
}

func (f *fakeStore) ListSpaceGrantSummaries(_ context.Context, query GovernancePageQuery) ([]SpaceGrantSummary, error) {
	users, _ := f.ListUserSummaries(context.Background(), GovernancePageQuery{TenantID: query.TenantID, BeforeID: query.BeforeID, Limit: len(f.users) + 1})
	out := make([]SpaceGrantSummary, 0, len(users))
	for _, user := range users {
		if user.ID == query.OwnerUserID {
			continue
		}
		item := SpaceGrantSummary{User: user}
		if permission, ok := f.perms[permKey{query.SpaceID, user.ID}]; ok {
			copy := permission
			item.Permission = &copy
		}
		out = append(out, item)
		if len(out) == query.Limit {
			break
		}
	}
	return out, nil
}

func (f *fakeStore) ListAgentAccessSummaries(_ context.Context, tenantID, userID platform.ID, limit int) ([]AgentAccessSummary, error) {
	byAgent := map[platform.ID]*AgentAccessSummary{}
	for _, token := range f.tokens {
		if token.TenantID != tenantID || token.UserID != userID {
			continue
		}
		item := byAgent[token.AgentID]
		if item == nil {
			item = &AgentAccessSummary{AgentID: token.AgentID}
			byAgent[token.AgentID] = item
		}
		item.TokenCount++
		if token.RevokedAt == nil {
			item.ActiveTokens++
		}
		if token.LastUsedAt != nil && (item.LastUsedAt == nil || token.LastUsedAt.After(*item.LastUsedAt)) {
			value := *token.LastUsedAt
			item.LastUsedAt = &value
		}
		if token.CreatedAt.After(item.LastIssuedAt) {
			item.LastIssuedAt = token.CreatedAt
		}
	}
	out := make([]AgentAccessSummary, 0, len(byAgent))
	for _, item := range byAgent {
		out = append(out, *item)
	}
	slices.SortFunc(out, func(a, b AgentAccessSummary) int {
		aSort, bSort := a.LastIssuedAt, b.LastIssuedAt
		if a.LastUsedAt != nil {
			aSort = *a.LastUsedAt
		}
		if b.LastUsedAt != nil {
			bSort = *b.LastUsedAt
		}
		if aSort.Equal(bSort) {
			return strings.Compare(string(b.AgentID), string(a.AgentID))
		}
		if aSort.After(bSort) {
			return -1
		}
		return 1
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}

func (f *fakeStore) RevokeToken(_ context.Context, tenantID, tokenID platform.ID, at time.Time) error {
	t, ok := f.tokens[tokenID]
	if !ok || t.TenantID != tenantID {
		return ErrTokenNotFound
	}
	if t.RevokedAt == nil { // 幂等：不覆盖原时间
		t.RevokedAt = &at
		f.tokens[tokenID] = t
	}
	return nil
}

func (f *fakeStore) GrantPermission(_ context.Context, p Permission) error {
	f.perms[permKey{p.SpaceID, p.UserID}] = p
	return nil
}

func (f *fakeStore) RevokePermission(_ context.Context, spaceID, userID platform.ID) error {
	delete(f.perms, permKey{spaceID, userID})
	return nil
}

// Atomically 用快照实现真回滚——不真回滚的 fake 会让
// 「失败后不留残缺状态」这条测试变成永远通过的空检查。
func (f *fakeStore) Atomically(ctx context.Context, fn func(Store) error) error {
	snapshot := f.clone()
	if err := fn(f); err != nil {
		f.restore(snapshot)
		return err
	}
	return nil
}

// --- 治理面：账号管理（Web 用） ---

func (f *fakeStore) FindUserByID(_ context.Context, tenantID, userID platform.ID) (*User, error) {
	u, ok := f.users[userID]
	if !ok || u.TenantID != tenantID {
		return nil, ErrUserNotFound
	}
	return &u, nil
}

// ListTenants 返回全部租户，**不过滤状态**——与 MySQL 实现一致。
// 领域层要能看见「库里有两个活跃租户」这件事并报错。
func (f *fakeStore) ListTenants(_ context.Context) ([]Tenant, error) {
	out := make([]Tenant, 0, len(f.tenants))
	for _, t := range f.tenants {
		out = append(out, t)
	}
	slices.SortFunc(out, func(a, b Tenant) int { return strings.Compare(string(a.ID), string(b.ID)) })
	return out, nil
}

func (f *fakeStore) ListUsers(_ context.Context, tenantID platform.ID) ([]User, error) {
	var out []User
	for _, u := range f.users {
		if u.TenantID == tenantID {
			out = append(out, u)
		}
	}
	slices.SortFunc(out, func(a, b User) int { return strings.Compare(string(a.ID), string(b.ID)) })
	return out, nil
}

func (f *fakeStore) ListSpaces(_ context.Context, tenantID platform.ID) ([]Space, error) {
	var out []Space
	for _, s := range f.spaces {
		if s.TenantID == tenantID {
			out = append(out, s)
		}
	}
	slices.SortFunc(out, func(a, b Space) int { return strings.Compare(string(a.ID), string(b.ID)) })
	return out, nil
}

func (f *fakeStore) ListPermissions(_ context.Context, tenantID platform.ID) ([]Permission, error) {
	var out []Permission
	for _, p := range f.perms {
		space, spaceOK := f.spaces[p.SpaceID]
		user, userOK := f.users[p.UserID]
		if spaceOK && userOK && space.TenantID == tenantID && user.TenantID == tenantID {
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b Permission) int {
		if cmp := strings.Compare(string(a.SpaceID), string(b.SpaceID)); cmp != 0 {
			return cmp
		}
		return strings.Compare(string(a.UserID), string(b.UserID))
	})
	return out, nil
}

func (f *fakeStore) UpdateUserPassword(_ context.Context, tenantID, userID platform.ID, hash string, mustChange bool, _ time.Time) error {
	u, ok := f.users[userID]
	if !ok || u.TenantID != tenantID {
		return ErrUserNotFound
	}
	u.PasswordHash, u.MustChangePw = hash, mustChange
	u.AuthVersion++
	f.users[userID] = u
	return nil
}

func (f *fakeStore) UpgradeUserPasswordHash(_ context.Context, tenantID, userID platform.ID, expectedHash, newHash string, expectedAuthVersion uint64) (bool, error) {
	if f.beforePasswordHashUpgrade != nil {
		hook := f.beforePasswordHashUpgrade
		f.beforePasswordHashUpgrade = nil
		hook(f, userID)
	}
	u, ok := f.users[userID]
	if !ok || u.TenantID != tenantID || u.PasswordHash != expectedHash || u.AuthVersion != expectedAuthVersion {
		return false, nil
	}
	u.PasswordHash = newHash
	f.users[userID] = u
	return true, nil
}

func (f *fakeStore) UpdateUserStatus(_ context.Context, tenantID, userID platform.ID, status string, _ time.Time) error {
	u, ok := f.users[userID]
	if !ok || u.TenantID != tenantID {
		return ErrUserNotFound
	}
	if u.Status != status {
		u.AuthVersion++
	}
	u.Status = status
	f.users[userID] = u
	return nil
}

// --- 测试用的密码哈希与会话存储 ---

// fakeHasher 用一个可逆的前缀冒充哈希。
//
// 不用真的 argon2：领域测试关心的是「验过了没有」「改密之后旧会话还在不在」，
// 而真实哈希每次跑要花几十毫秒 × 每个用例。真实实现有自己的测试
// （internal/adapters/crypto），那里才是验哈希正确性的地方。
type fakeHasher struct {
	failVerify  error
	needsRehash bool
}

const fakeHashPrefix = "fakehash:"

func (h *fakeHasher) Hash(plaintext string) (string, error) {
	return fakeHashPrefix + plaintext, nil
}

func (h *fakeHasher) Verify(plaintext, hash string) (bool, error) {
	if h.failVerify != nil {
		return false, h.failVerify
	}
	if !strings.HasPrefix(hash, fakeHashPrefix) && !strings.HasPrefix(hash, "weak"+fakeHashPrefix) {
		// 与真实实现同一个行为：畸形 hash 是数据问题，不是「密码错了」
		return false, fmt.Errorf("fake: 无法识别的哈希格式")
	}
	return strings.TrimPrefix(strings.TrimPrefix(hash, "weak"), fakeHashPrefix) == plaintext, nil
}

func (h *fakeHasher) NeedsRehash(hash string) (bool, error) {
	if !strings.HasPrefix(hash, fakeHashPrefix) && !strings.HasPrefix(hash, "weak"+fakeHashPrefix) {
		return false, fmt.Errorf("fake: 无法识别的哈希格式")
	}
	return h.needsRehash || strings.HasPrefix(hash, "weak"+fakeHashPrefix), nil
}

// fakeSessions 是内存会话存储。
//
// 它必须模拟真库的一件事：**DeleteByUser 真的会删掉那个人的全部会话**。
// 写成空操作的话，「改密之后别处的浏览器要掉线」这条会永远绿。
type fakeSessions struct {
	byHash map[string]ports.Session
}

func newFakeSessions() *fakeSessions {
	return &fakeSessions{byHash: map[string]ports.Session{}}
}

func (s *fakeSessions) Create(_ context.Context, sess ports.Session) error {
	s.byHash[sess.TokenHash] = sess
	return nil
}

func (s *fakeSessions) Find(_ context.Context, hash string) (ports.Session, bool, error) {
	sess, ok := s.byHash[hash]
	return sess, ok, nil
}

func (s *fakeSessions) Touch(_ context.Context, hash string, seenAt, expiresAt time.Time) error {
	sess, ok := s.byHash[hash]
	if !ok {
		return nil
	}
	sess.LastSeenAt, sess.ExpiresAt = seenAt, expiresAt
	s.byHash[hash] = sess
	return nil
}

func (s *fakeSessions) Delete(_ context.Context, hash string) error {
	delete(s.byHash, hash)
	return nil
}

func (s *fakeSessions) DeleteByUser(_ context.Context, userID string) error {
	for hash, sess := range s.byHash {
		if sess.UserID == userID {
			delete(s.byHash, hash)
		}
	}
	return nil
}

func (s *fakeSessions) DeleteExpired(_ context.Context, before time.Time) (int, error) {
	n := 0
	for hash, sess := range s.byHash {
		if sess.ExpiresAt.Before(before) {
			delete(s.byHash, hash)
			n++
		}
	}
	return n, nil
}
