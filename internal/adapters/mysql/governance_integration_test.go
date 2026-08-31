//go:build integration

// 适配器测试：对着真实 MySQL 验证 GovernanceStore 的行为
// （docs/05-code-structure.md §6.4）。
//
// 基准是领域层的 fakeStore：同一个接口，两个实现，行为必须一致——
// 否则领域单测全绿而线上行为不同，是最难查的一类故障。
//
// 隔离方式：绝大多数用例跑在一个最终 rollback 的事务里，不留痕迹；
// 需要验证真实提交的用例自己清理。
package mysql_test

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"claracore-server/internal/adapters/mysql"
	"claracore-server/internal/config"
	"claracore-server/internal/domain/governance"
	"claracore-server/internal/platform"
)

func openTestDB(t *testing.T) *sql.DB {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	// config.Load 读的是相对路径 .env，而测试的工作目录是 internal/adapters/mysql/
	t.Chdir(filepath.Join(wd, "..", "..", ".."))

	cfg, err := config.Load()
	if err != nil {
		t.Skipf("配置不可用，跳过集成测试: %v", err)
	}
	db, err := mysql.Open(context.Background(), cfg.MySQL.DSN(), mysql.PoolConfig{
		MaxOpenConns: 4, MaxIdleConns: 2, ConnMaxLifetime: time.Minute,
	})
	if err != nil {
		t.Skipf("数据库不可达，跳过集成测试: %v", err)
	}
	t.Cleanup(func() { db.Close() })
	return db
}

func hashOf(s string) string {
	sum := sha256.Sum256([]byte(s))
	return hex.EncodeToString(sum[:])
}

// fixture 是一套完整的治理数据：一个租户、两个用户、一个 agent、一个 token，
// 以及覆盖全部三种类型、三种可见性的六个空间。
type fixture struct {
	tenant   governance.Tenant
	user     governance.User // 被测视角
	other    governance.User // 同租户的另一个人，用于验证隔离
	agent    governance.Agent
	token    governance.Token
	personal governance.Space // user 的个人空间
	otherPer governance.Space // other 的个人空间：不该出现在 user 的成员关系里
	team     governance.Space
	owned    governance.Space // user 自己建的项目空间
	granted  governance.Space // other 建的，显式授权给 user
	hidden   governance.Space // other 建的，未授权：不该出现

	// 另一个租户的数据。少一个 WHERE tenant_id 就会把这两个捞出来，
	// 而 team 空间在领域规则里是「全租户隐含可读」——那就是一次跨租户泄露。
	foreignTenant governance.Tenant
	foreignUser   governance.User  // 与 user 同名，只有 tenant_id 不同
	foreignTeam   governance.Space // 别的租户的团队空间
	foreignOwned  governance.Space // 别的租户下、owner 却是本 user 的空间（数据损坏场景）
}

func seed(ctx context.Context, t *testing.T, st governance.Store, ids *platform.IDGenerator, now time.Time) fixture {
	t.Helper()

	f := fixture{}
	f.tenant = governance.Tenant{ID: ids.New(), Name: "契约测试租户", Status: governance.StatusActive, CreatedAt: now}
	f.user = governance.User{
		ID: ids.New(), TenantID: f.tenant.ID, Username: "contract-user",
		PasswordHash: "argon2id$fake", DisplayName: "契约用户",
		Role: governance.RoleMember, MustChangePw: true, Status: governance.StatusActive, CreatedAt: now,
	}
	f.other = governance.User{
		ID: ids.New(), TenantID: f.tenant.ID, Username: "contract-other",
		PasswordHash: "argon2id$fake", DisplayName: "另一个人",
		Role: governance.RoleAdmin, MustChangePw: false, Status: governance.StatusActive, CreatedAt: now,
	}
	f.agent = governance.Agent{
		ID: ids.New(), TenantID: f.tenant.ID, UserID: f.user.ID,
		Slug: "clara", DisplayName: "Clara", CreatedAt: now,
	}
	expires := now.Add(24 * time.Hour)
	f.token = governance.Token{
		ID: ids.New(), TenantID: f.tenant.ID, UserID: f.user.ID, AgentID: f.agent.ID,
		Name: "MacBook", Prefix: "ccs_abc", Hash: hashOf("contract-primary"),
		ExpiresAt: &expires, CreatedAt: now,
	}

	space := func(typ, name string, owner *platform.ID) governance.Space {
		return governance.Space{ID: ids.New(), TenantID: f.tenant.ID, Type: typ, Name: name, OwnerUserID: owner, CreatedAt: now}
	}
	f.personal = space(governance.SpacePersonal, "契约用户", &f.user.ID)
	f.otherPer = space(governance.SpacePersonal, "另一个人", &f.other.ID)
	f.team = space(governance.SpaceTeam, "团队", nil)
	f.owned = space(governance.SpaceProject, "我建的项目", &f.user.ID)
	f.granted = space(governance.SpaceProject, "被授权的项目", &f.other.ID)
	f.hidden = space(governance.SpaceProject, "看不见的项目", &f.other.ID)

	f.foreignTenant = governance.Tenant{ID: ids.New(), Name: "别人家的租户", Status: governance.StatusActive, CreatedAt: now}
	f.foreignUser = governance.User{
		ID: ids.New(), TenantID: f.foreignTenant.ID, Username: f.user.Username,
		PasswordHash: "argon2id$other", DisplayName: "别人家的同名用户",
		Role: governance.RoleMember, MustChangePw: false, Status: governance.StatusActive, CreatedAt: now,
	}
	f.foreignTeam = governance.Space{
		ID: ids.New(), TenantID: f.foreignTenant.ID, Type: governance.SpaceTeam,
		Name: "别人家的团队空间", CreatedAt: now,
	}
	f.foreignOwned = governance.Space{
		ID: ids.New(), TenantID: f.foreignTenant.ID, Type: governance.SpaceProject,
		Name: "跨租户错挂的项目", OwnerUserID: &f.user.ID, CreatedAt: now,
	}

	must := func(err error) {
		if err != nil {
			t.Fatalf("准备测试数据失败: %v", err)
		}
	}
	must(st.CreateTenant(ctx, f.tenant))
	must(st.CreateTenant(ctx, f.foreignTenant))
	must(st.CreateUser(ctx, f.user))
	must(st.CreateUser(ctx, f.other))
	must(st.CreateUser(ctx, f.foreignUser))
	must(st.CreateAgent(ctx, f.agent))
	must(st.CreateToken(ctx, f.token))
	for _, sp := range []governance.Space{
		f.personal, f.otherPer, f.team, f.owned, f.granted, f.hidden,
		f.foreignTeam, f.foreignOwned,
	} {
		must(st.CreateSpace(ctx, sp))
	}
	must(st.GrantPermission(ctx, governance.Permission{
		SpaceID: f.granted.ID, UserID: f.user.ID,
		CanRead: true, CanWrite: false, GrantedBy: f.other.ID, CreatedAt: now,
	}))
	return f
}

func TestGovernanceStoreContract(t *testing.T) {
	db := openTestDB(t)
	store := mysql.NewGovernanceStore(db)
	ctx := context.Background()
	ids := platform.NewIDGenerator(platform.SystemClock{})
	// DATETIME(3) 只有毫秒精度，先截断，否则回读比较必然不等
	now := time.Now().UTC().Truncate(time.Millisecond)

	errRollback := errors.New("契约测试结束，回滚")
	var f fixture

	err := store.Atomically(ctx, func(st governance.Store) error {
		f = seed(ctx, t, st, ids, now)

		t.Run("FindAuthContext 一次取回 token+user+agent+tenant", func(t *testing.T) {
			auth, err := st.FindAuthContext(ctx, f.token.Hash)
			if err != nil {
				t.Fatalf("认证上下文应当存在: %v", err)
			}
			if auth.Token.ID != f.token.ID || auth.User.ID != f.user.ID ||
				auth.Agent.ID != f.agent.ID || auth.Tenant.ID != f.tenant.ID {
				t.Fatalf("取回的四个实体对不上: %+v", auth)
			}
			if auth.Token.ExpiresAt == nil || !auth.Token.ExpiresAt.Equal(*f.token.ExpiresAt) {
				t.Errorf("expires_at 回读不一致: %v，期望 %v", auth.Token.ExpiresAt, f.token.ExpiresAt)
			}
			if auth.Token.LastUsedAt != nil || auth.Token.RevokedAt != nil {
				t.Errorf("未使用未吊销的 token 不该有时间戳: %+v", auth.Token)
			}
			if auth.User.Username != f.user.Username || auth.User.DisplayName != f.user.DisplayName || auth.User.Role != governance.RoleMember || auth.User.Status != governance.StatusActive {
				t.Errorf("用户标量字段回读错误: %+v", auth.User)
			}
		})

		t.Run("未知 hash 返回 ErrTokenNotFound", func(t *testing.T) {
			_, err := st.FindAuthContext(ctx, hashOf("从未签发过"))
			if !errors.Is(err, governance.ErrTokenNotFound) {
				t.Fatalf("期望 ErrTokenNotFound，实际: %v", err)
			}
		})

		// 库里没有外键，悬空引用是可能出现的。这两条确认它被识别为数据损坏，
		// 而不是被 INNER JOIN 悄悄伪装成「token 不存在」。
		t.Run("悬空的 user/agent 引用被识别出来", func(t *testing.T) {
			danglingUser := governance.Token{
				ID: ids.New(), TenantID: f.tenant.ID, UserID: ids.New(), AgentID: f.agent.ID,
				Name: "悬空用户", Prefix: "ccs_x", Hash: hashOf("dangling-user"), CreatedAt: now,
			}
			if err := st.CreateToken(ctx, danglingUser); err != nil {
				t.Fatal(err)
			}
			if _, err := st.FindAuthContext(ctx, danglingUser.Hash); !errors.Is(err, governance.ErrUserNotFound) {
				t.Errorf("期望 ErrUserNotFound，实际: %v", err)
			}

			danglingAgent := governance.Token{
				ID: ids.New(), TenantID: f.tenant.ID, UserID: f.user.ID, AgentID: ids.New(),
				Name: "悬空 agent", Prefix: "ccs_y", Hash: hashOf("dangling-agent"), CreatedAt: now,
			}
			if err := st.CreateToken(ctx, danglingAgent); err != nil {
				t.Fatal(err)
			}
			if _, err := st.FindAuthContext(ctx, danglingAgent.Hash); !errors.Is(err, governance.ErrAgentNotFound) {
				t.Errorf("期望 ErrAgentNotFound，实际: %v", err)
			}
		})

		t.Run("TouchToken 写入 last_used_at 且可重复调用", func(t *testing.T) {
			at := now.Add(time.Minute)
			if err := st.TouchToken(ctx, f.token.ID, at); err != nil {
				t.Fatalf("TouchToken 失败: %v", err)
			}
			auth, err := st.FindAuthContext(ctx, f.token.Hash)
			if err != nil {
				t.Fatal(err)
			}
			if auth.Token.LastUsedAt == nil || !auth.Token.LastUsedAt.Equal(at) {
				t.Fatalf("last_used_at 未写入: %v", auth.Token.LastUsedAt)
			}
			// 写同值时 MySQL 报 0 行受影响，不能因此判成「token 不存在」
			if err := st.TouchToken(ctx, f.token.ID, at); err != nil {
				t.Errorf("重复写同一时间不该报错: %v", err)
			}
			if err := st.TouchToken(ctx, ids.New(), at); !errors.Is(err, governance.ErrTokenNotFound) {
				t.Errorf("不存在的 token 期望 ErrTokenNotFound，实际: %v", err)
			}
		})

		t.Run("ListSpaceMemberships 只捞相关空间", func(t *testing.T) {
			ms, err := st.ListSpaceMemberships(ctx, f.tenant.ID, f.user.ID)
			if err != nil {
				t.Fatalf("查询成员关系失败: %v", err)
			}
			got := map[platform.ID]governance.SpaceMembership{}
			for _, m := range ms {
				got[m.Space.ID] = m
			}
			for _, want := range []governance.Space{f.personal, f.team, f.owned, f.granted} {
				if _, ok := got[want.ID]; !ok {
					t.Errorf("缺少应当相关的空间: %s(%s)", want.Name, want.Type)
				}
			}
			for _, unwanted := range []governance.Space{f.otherPer, f.hidden} {
				if _, ok := got[unwanted.ID]; ok {
					t.Errorf("捞出了无关的空间: %s(%s)", unwanted.Name, unwanted.Type)
				}
			}
			// 跨租户是最严重的一类：team 在领域规则里全租户隐含可读，
			// 少一个 WHERE tenant_id 就是把别人家的团队知识直接读出来。
			for _, foreign := range []governance.Space{f.foreignTeam, f.foreignOwned} {
				if _, ok := got[foreign.ID]; ok {
					t.Errorf("跨租户越权：捞出了别的租户的空间 %s(%s)", foreign.Name, foreign.Type)
				}
			}
			if len(ms) != 4 {
				t.Fatalf("期望 4 个相关空间，实际 %d 个", len(ms))
			}

			// 授权行必须原样带回，且 Store 不得替领域层做判断
			gm := got[f.granted.ID]
			if gm.Permission == nil {
				t.Fatal("被授权的项目空间没带回授权行")
			}
			if !gm.Permission.CanRead || gm.Permission.CanWrite {
				t.Errorf("授权行读写位回读错误: %+v", gm.Permission)
			}
			if gm.Permission.GrantedBy != f.other.ID || gm.Permission.SpaceID != f.granted.ID || gm.Permission.UserID != f.user.ID {
				t.Errorf("授权行的主体信息不对: %+v", gm.Permission)
			}
			if got[f.team.ID].Permission != nil || got[f.owned.ID].Permission != nil {
				t.Error("没有授权行的空间不该凭空造出 Permission")
			}
			// 空间的可空字段
			if got[f.team.ID].Space.OwnerUserID != nil {
				t.Error("team 空间的 owner 应为 NULL")
			}
			if !got[f.personal.ID].Space.IsOwnedBy(f.user.ID) {
				t.Error("personal 空间的 owner 回读错误")
			}

			// 端到端：SQL 捞回的原始事实喂给领域规则，结论必须是预期的那份
			scope := governance.ResolveScope(f.user.ID, ms)
			checks := []struct {
				space       governance.Space
				read, write bool
				label       string
			}{
				{f.personal, true, true, "个人空间读写"},
				{f.team, true, false, "团队空间可读、无显式授权即不可写"},
				{f.owned, true, true, "自建项目空间读写"},
				{f.granted, true, false, "被授权项目空间只读"},
			}
			for _, c := range checks {
				if scope.CanRead(c.space.ID) != c.read || scope.CanWrite(c.space.ID) != c.write {
					t.Errorf("%s：实际 read=%v write=%v", c.label,
						scope.CanRead(c.space.ID), scope.CanWrite(c.space.ID))
				}
			}
			if _, err := scope.PersonalSpace(); err != nil {
				t.Errorf("个人空间应当可解析: %v", err)
			}
			if scope.CanRead(f.hidden.ID) {
				t.Error("未授权的项目空间竟然可读")
			}
		})

		t.Run("空间治理列表按租户隔离", func(t *testing.T) {
			spaces, err := st.ListSpaces(ctx, f.tenant.ID)
			if err != nil {
				t.Fatal(err)
			}
			gotSpaces := make(map[platform.ID]bool, len(spaces))
			for _, space := range spaces {
				gotSpaces[space.ID] = true
				if space.TenantID != f.tenant.ID {
					t.Fatalf("空间列表泄露其他租户: %+v", space)
				}
			}
			for _, want := range []governance.Space{f.personal, f.otherPer, f.team, f.owned, f.granted, f.hidden} {
				if !gotSpaces[want.ID] {
					t.Errorf("租户空间列表缺少 %s", want.Name)
				}
			}
			if gotSpaces[f.foreignTeam.ID] || gotSpaces[f.foreignOwned.ID] {
				t.Fatal("空间治理列表跨租户泄露")
			}

			permissions, err := st.ListPermissions(ctx, f.tenant.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(permissions) != 1 || permissions[0].SpaceID != f.granted.ID || permissions[0].UserID != f.user.ID {
				t.Fatalf("空间授权列表不对: %+v", permissions)
			}
		})

		t.Run("FindUserByUsername 与 FindSpace 的租户隔离", func(t *testing.T) {
			u, err := st.FindUserByUsername(ctx, f.tenant.ID, f.user.Username)
			if err != nil {
				t.Fatalf("按账号查询失败: %v", err)
			}
			// 另一个租户下有个同名账号，查回来的必须是本租户这个
			if u.ID != f.user.ID || u.PasswordHash != f.user.PasswordHash {
				t.Errorf("查回的用户不对（可能命中了别的租户的同名账号）: %+v", u)
			}
			other, err := st.FindUserByUsername(ctx, f.foreignTenant.ID, f.user.Username)
			if err != nil {
				t.Fatalf("同名账号在另一个租户下也该查得到: %v", err)
			}
			if other.ID != f.foreignUser.ID {
				t.Errorf("跨租户同名账号解析错了: %+v", other)
			}
			if _, err := st.FindUserByUsername(ctx, ids.New(), f.user.Username); !errors.Is(err, governance.ErrUserNotFound) {
				t.Errorf("不存在的租户下期望 ErrUserNotFound，实际: %v", err)
			}
			if _, err := st.FindUserByUsername(ctx, f.tenant.ID, "查无此人"); !errors.Is(err, governance.ErrUserNotFound) {
				t.Errorf("期望 ErrUserNotFound，实际: %v", err)
			}

			sp, err := st.FindSpace(ctx, f.tenant.ID, f.owned.ID)
			if err != nil {
				t.Fatalf("查询空间失败: %v", err)
			}
			if sp.Name != f.owned.Name || !sp.IsOwnedBy(f.user.ID) {
				t.Errorf("查回的空间不对: %+v", sp)
			}
			// 空间确实存在，但属于别的租户——对本租户必须等同于不存在，
			// 返回 ErrSpaceForbidden 就等于告诉对方「这个 ID 在别处是有效的」
			if _, err := st.FindSpace(ctx, f.tenant.ID, f.foreignTeam.ID); !errors.Is(err, governance.ErrSpaceNotFound) {
				t.Errorf("跨租户查空间期望 ErrSpaceNotFound，实际: %v", err)
			}
		})

		t.Run("ListTokens 含已吊销，RevokeToken 幂等", func(t *testing.T) {
			second := governance.Token{
				ID: ids.New(), TenantID: f.tenant.ID, UserID: f.user.ID, AgentID: f.agent.ID,
				Name: "第二台设备", Prefix: "ccs_two", Hash: hashOf("contract-second"), CreatedAt: now,
			}
			if err := st.CreateToken(ctx, second); err != nil {
				t.Fatal(err)
			}

			revokedAt := now.Add(2 * time.Hour)
			if err := st.RevokeToken(ctx, f.tenant.ID, second.ID, revokedAt); err != nil {
				t.Fatalf("吊销失败: %v", err)
			}
			// 幂等：不报错，且不覆盖第一次的时间戳（那是审计证据）
			if err := st.RevokeToken(ctx, f.tenant.ID, second.ID, revokedAt.Add(time.Hour)); err != nil {
				t.Errorf("重复吊销不该报错: %v", err)
			}
			if err := st.RevokeToken(ctx, f.tenant.ID, ids.New(), revokedAt); !errors.Is(err, governance.ErrTokenNotFound) {
				t.Errorf("吊销不存在的 token 期望 ErrTokenNotFound，实际: %v", err)
			}
			if err := st.RevokeToken(ctx, ids.New(), second.ID, revokedAt); !errors.Is(err, governance.ErrTokenNotFound) {
				t.Errorf("跨租户吊销期望 ErrTokenNotFound，实际: %v", err)
			}

			list, err := st.ListTokens(ctx, f.tenant.ID, f.user.ID)
			if err != nil {
				t.Fatalf("列出 token 失败: %v", err)
			}
			var found *governance.Token
			for i := range list {
				if list[i].ID == second.ID {
					found = &list[i]
				}
			}
			if found == nil {
				t.Fatal("已吊销的 token 必须仍出现在审计列表里")
			}
			if found.RevokedAt == nil || !found.RevokedAt.Equal(revokedAt) {
				t.Errorf("revoked_at 被覆盖或未写入: %v，期望 %v", found.RevokedAt, revokedAt)
			}
			// 明文无从取回，列表里只该有 prefix
			if found.Prefix != "ccs_two" {
				t.Errorf("prefix 回读错误: %q", found.Prefix)
			}
		})

		t.Run("GrantPermission 覆盖已有授权，RevokePermission 幂等", func(t *testing.T) {
			// 同一 (space, user) 再授一次：应当覆盖而不是主键冲突
			later := now.Add(3 * time.Hour)
			if err := st.GrantPermission(ctx, governance.Permission{
				SpaceID: f.granted.ID, UserID: f.user.ID,
				CanRead: true, CanWrite: true, GrantedBy: f.other.ID, CreatedAt: later,
			}); err != nil {
				t.Fatalf("覆盖授权失败: %v", err)
			}
			ms, err := st.ListSpaceMemberships(ctx, f.tenant.ID, f.user.ID)
			if err != nil {
				t.Fatal(err)
			}
			scope := governance.ResolveScope(f.user.ID, ms)
			if !scope.CanWrite(f.granted.ID) {
				t.Error("提权后应当可写")
			}

			if err := st.RevokePermission(ctx, f.granted.ID, f.user.ID); err != nil {
				t.Fatalf("撤销授权失败: %v", err)
			}
			if err := st.RevokePermission(ctx, f.granted.ID, f.user.ID); err != nil {
				t.Errorf("撤销本就不存在的授权不该报错: %v", err)
			}
			ms, err = st.ListSpaceMemberships(ctx, f.tenant.ID, f.user.ID)
			if err != nil {
				t.Fatal(err)
			}
			for _, m := range ms {
				if m.Space.ID == f.granted.ID {
					t.Error("撤销授权后该空间不该再出现在成员关系里")
				}
			}
		})

		t.Run("事务内再次 Atomically 复用当前事务", func(t *testing.T) {
			nested := governance.Agent{
				ID: ids.New(), TenantID: f.tenant.ID, UserID: f.user.ID,
				Slug: "codex", DisplayName: "Codex", CreatedAt: now,
			}
			if err := st.Atomically(ctx, func(inner governance.Store) error {
				return inner.CreateAgent(ctx, nested)
			}); err != nil {
				t.Fatalf("嵌套 Atomically 失败: %v", err)
			}
			tok := governance.Token{
				ID: ids.New(), TenantID: f.tenant.ID, UserID: f.user.ID, AgentID: nested.ID,
				Name: "嵌套写入的", Prefix: "ccs_n", Hash: hashOf("contract-nested"), CreatedAt: now,
			}
			if err := st.CreateToken(ctx, tok); err != nil {
				t.Fatal(err)
			}
			auth, err := st.FindAuthContext(ctx, tok.Hash)
			if err != nil {
				t.Fatalf("嵌套事务里写的 agent 应当可见: %v", err)
			}
			if auth.Agent.Slug != "codex" {
				t.Errorf("回读的 agent 不对: %+v", auth.Agent)
			}
		})

		t.Run("有界治理读模型隔离租户且不读取凭据", func(t *testing.T) {
			users, err := st.ListUserSummaries(ctx, governance.GovernancePageQuery{TenantID: f.tenant.ID, Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			if len(users) != 2 || users[0].TenantID != f.tenant.ID || users[1].TenantID != f.tenant.ID {
				t.Fatalf("member summaries must contain exactly this tenant's users: %+v", users)
			}
			if users[0].ID <= users[1].ID {
				t.Fatalf("member summaries must use descending keyset order: %+v", users)
			}

			spaces, err := st.ListGovernedSpaces(ctx, governance.GovernancePageQuery{TenantID: f.tenant.ID, Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			if len(spaces) != 4 {
				t.Fatalf("governance spaces must exclude two personal and all foreign spaces: %+v", spaces)
			}
			for _, space := range spaces {
				if space.TenantID != f.tenant.ID || space.Type == governance.SpacePersonal {
					t.Fatalf("space page leaked an out-of-scope row: %+v", space)
				}
			}

			if err := st.GrantPermission(ctx, governance.Permission{
				SpaceID: f.team.ID, UserID: f.other.ID, CanRead: true, CanWrite: true,
				GrantedBy: f.user.ID, CreatedAt: now,
			}); err != nil {
				t.Fatal(err)
			}
			grants, err := st.ListSpaceGrantSummaries(ctx, governance.GovernancePageQuery{TenantID: f.tenant.ID, SpaceID: f.team.ID, Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			if len(grants) != 2 {
				t.Fatalf("grant page must contain this tenant's two members: %+v", grants)
			}
			for _, grant := range grants {
				if grant.User.ID == f.other.ID && (grant.Permission == nil || !grant.Permission.CanWrite) {
					t.Fatalf("selected space permission missing from projection: %+v", grant)
				}
				if grant.User.ID == f.foreignUser.ID {
					t.Fatalf("foreign member leaked into grant page: %+v", grant)
				}
			}

			tokens, err := st.ListTokenSummaries(ctx, governance.GovernancePageQuery{TenantID: f.tenant.ID, UserID: f.user.ID, Limit: 10})
			if err != nil {
				t.Fatal(err)
			}
			if len(tokens) < 2 {
				t.Fatalf("token projection should contain all own tokens created by the contract: %+v", tokens)
			}
			for _, token := range tokens {
				if token.TenantID != f.tenant.ID || token.UserID != f.user.ID {
					t.Fatalf("token projection leaked another owner or tenant: %+v", token)
				}
			}
			if _, err := st.FindTokenSummary(ctx, f.tenant.ID, f.other.ID, f.token.ID); !errors.Is(err, governance.ErrTokenNotFound) {
				t.Fatalf("token ownership lookup must fail closed, got %v", err)
			}
			agents, err := st.ListAgentAccessSummaries(ctx, f.tenant.ID, f.user.ID, 1)
			if err != nil || len(agents) != 1 {
				t.Fatalf("agent aggregate must honor its hard limit: %+v err=%v", agents, err)
			}
		})

		// 唯一性约束由数据库强制（应用层预检在并发下必然失效），
		// 但错误必须以领域语言冒出来——Host 层要判「账号已被占用」时
		// 不能 import 数据库驱动去看错误码。
		t.Run("唯一键冲突映射为 ErrAlreadyExists", func(t *testing.T) {
			cases := []struct {
				name string
				do   func() error
			}{
				{"同一用户的第二个个人空间", func() error {
					return st.CreateSpace(ctx, governance.Space{
						ID: ids.New(), TenantID: f.tenant.ID, Type: governance.SpacePersonal,
						Name: "又一个个人空间", OwnerUserID: &f.user.ID, CreatedAt: now,
					})
				}},
				{"同一租户的第二个团队空间", func() error {
					return st.CreateSpace(ctx, governance.Space{
						ID: ids.New(), TenantID: f.tenant.ID, Type: governance.SpaceTeam,
						Name: "又一个团队空间", CreatedAt: now,
					})
				}},
				{"同租户内账号重名", func() error {
					u := f.user
					u.ID = ids.New()
					return st.CreateUser(ctx, u)
				}},
				{"同一用户下 agent slug 重复", func() error {
					a := f.agent
					a.ID = ids.New()
					return st.CreateAgent(ctx, a)
				}},
				{"token hash 重复", func() error {
					tk := f.token
					tk.ID = ids.New()
					return st.CreateToken(ctx, tk)
				}},
			}
			for _, c := range cases {
				err := c.do()
				if !errors.Is(err, governance.ErrAlreadyExists) {
					t.Errorf("%s：期望 ErrAlreadyExists，实际: %v", c.name, err)
				}
			}
		})

		t.Run("非冲突的写入错误不被误判为 ErrAlreadyExists", func(t *testing.T) {
			// 边界：writeErr 只该翻译 1062。这里用超长 name 触发一个别的错误，
			// 如果它也被翻译成「已存在」，Host 层就会对着一个数据错误
			// 显示「这个名字已经有人用了」。
			tooLong := governance.Tenant{
				ID: ids.New(), Name: strings.Repeat("超", 200),
				Status: governance.StatusActive, CreatedAt: now,
			}
			err := st.CreateTenant(ctx, tooLong)
			if err == nil {
				t.Skip("这个库没拒绝超长租户名（可能是非严格模式），跳过")
			}
			if errors.Is(err, governance.ErrAlreadyExists) {
				t.Errorf("非唯一键错误被误判成 ErrAlreadyExists: %v", err)
			}
		})

		return errRollback
	})

	if !errors.Is(err, errRollback) {
		t.Fatalf("期望以回滚结束，实际: %v", err)
	}

	// 上面全部写入都必须已经消失——顺带证明 Atomically 的回滚是真的
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM tenants WHERE id IN (?, ?)`,
		string(f.tenant.ID), string(f.foreignTenant.ID)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("回滚后测试数据仍在库里（tenant %s / %s），需要手工清理", f.tenant.ID, f.foreignTenant.ID)
	}
}

// TestGovernanceStoreAtomicallyCommits 验证真实提交路径。
//
// 上面的契约测试全程在一个最终回滚的事务里，证明不了「成功时数据真的落库」。
// 这里走一次完整的 begin → commit，用后即删。
func TestGovernanceStoreAtomicallyCommits(t *testing.T) {
	db := openTestDB(t)
	store := mysql.NewGovernanceStore(db)
	ctx := context.Background()
	ids := platform.NewIDGenerator(platform.SystemClock{})
	now := time.Now().UTC().Truncate(time.Millisecond)

	tenant := governance.Tenant{ID: ids.New(), Name: "提交测试租户", Status: governance.StatusActive, CreatedAt: now}
	user := governance.User{
		ID: ids.New(), TenantID: tenant.ID, Username: "commit-user",
		PasswordHash: "argon2id$fake", DisplayName: "提交用户",
		Role: governance.RoleAdmin, MustChangePw: true, Status: governance.StatusActive, CreatedAt: now,
	}
	owner := user.ID
	personal := governance.Space{
		ID: ids.New(), TenantID: tenant.ID, Type: governance.SpacePersonal,
		Name: "提交用户", OwnerUserID: &owner, CreatedAt: now,
	}
	t.Cleanup(func() {
		// 提交过的数据必须自己收拾干净，否则下一次跑测试会撞唯一键
		for _, stmt := range []struct {
			sql string
			arg string
		}{
			{`DELETE FROM spaces WHERE tenant_id = ?`, string(tenant.ID)},
			{`DELETE FROM users WHERE tenant_id = ?`, string(tenant.ID)},
			{`DELETE FROM tenants WHERE id = ?`, string(tenant.ID)},
		} {
			if _, err := db.ExecContext(context.Background(), stmt.sql, stmt.arg); err != nil {
				t.Errorf("清理测试数据失败（需手工处理 tenant %s）: %v", tenant.ID, err)
			}
		}
	})

	// 失败路径：半途出错不得留下任何一行
	boom := errors.New("模拟第二步失败")
	err := store.Atomically(ctx, func(st governance.Store) error {
		if err := st.CreateTenant(ctx, tenant); err != nil {
			return err
		}
		return boom
	})
	if !errors.Is(err, boom) {
		t.Fatalf("错误应当原样返回（领域层要靠 errors.Is 识别哨兵错误），实际: %v", err)
	}
	var n int
	if err := db.QueryRowContext(ctx, `SELECT COUNT(*) FROM tenants WHERE id = ?`, string(tenant.ID)).Scan(&n); err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("fn 返回错误后租户仍被写入了——没有回滚")
	}

	// 成功路径：三张表一起落库
	if err := store.Atomically(ctx, func(st governance.Store) error {
		if err := st.CreateTenant(ctx, tenant); err != nil {
			return err
		}
		if err := st.CreateUser(ctx, user); err != nil {
			return err
		}
		return st.CreateSpace(ctx, personal)
	}); err != nil {
		t.Fatalf("提交失败: %v", err)
	}

	got, err := store.FindUserByUsername(ctx, tenant.ID, user.Username)
	if err != nil {
		t.Fatalf("提交后应当查得到用户: %v", err)
	}
	if got.ID != user.ID || !got.CreatedAt.Equal(now) {
		t.Errorf("回读的用户不一致: %+v", got)
	}
	ms, err := store.ListSpaceMemberships(ctx, tenant.ID, user.ID)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := governance.ResolveScope(user.ID, ms).PersonalSpace(); err != nil {
		t.Errorf("提交后个人空间应当可解析: %v", err)
	}
}
