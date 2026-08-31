package governance

import (
	"context"
	"errors"
	"testing"
	"time"

	"claracore-server/internal/platform"
	"claracore-server/internal/ports"
)

func seedWeb(t *testing.T) (*Service, *fakeStore, *platform.FixedClock, *fakeSessions, User) {
	t.Helper()
	store := newFakeStore()
	svc, clock, _, sessions := newWebService(t, store)

	res, err := svc.ProvisionTenant(context.Background(), ProvisionTenantInput{
		TenantName:    "测试团队",
		AdminUsername: "maozai",
		AdminPassword: testPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	return svc, store, clock, sessions, res.Admin
}

// ---------------------------------------------------------------------------
// 登录
// ---------------------------------------------------------------------------

func TestLoginIssuesSession(t *testing.T) {
	svc, _, _, sessions, admin := seedWeb(t)

	token, p, err := svc.Login(context.Background(), "maozai", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	if token == "" {
		t.Fatal("应当签发会话令牌")
	}
	if p.UserID != admin.ID || !p.IsAdmin() {
		t.Fatalf("身份不对: %+v", p)
	}
	// 管理员分配的初始密码在改掉之前不该能做别的事
	if !p.MustChangePassword {
		t.Fatal("首次登录必须强制改密")
	}

	// 库里存的是哈希，不是令牌本身
	for hash := range sessions.byHash {
		if hash == string(token) {
			t.Fatal("会话表里存的必须是令牌的哈希——数据库被读走时不该拿到能直接登录的东西")
		}
	}
}

// TestLoginFailuresAreIndistinguishable 是一条安全属性。
//
// 区分「账号不存在」「密码不对」「账号已停用」等于提供一个账号枚举接口：
// 用不同的用户名试一遍就能得到一份有效账号清单。
func TestLoginFailuresAreIndistinguishable(t *testing.T) {
	svc, _, _, _, admin := seedWeb(t)
	ctx := context.Background()

	cases := map[string]func() error{
		"账号不存在": func() error {
			_, _, err := svc.Login(ctx, "nobody", testPassword)
			return err
		},
		"密码不对": func() error {
			_, _, err := svc.Login(ctx, "maozai", "wrong-password-here")
			return err
		},
		"空密码": func() error {
			_, _, err := svc.Login(ctx, "maozai", "")
			return err
		},
	}
	for name, run := range cases {
		t.Run(name, func(t *testing.T) {
			if err := run(); !errors.Is(err, ErrInvalidCredentials) {
				t.Fatalf("应收敛成同一个 ErrInvalidCredentials，实际: %v", err)
			}
		})
	}

	// 停用之后同样只说「凭据不对」——说「已停用」等于确认这个人存在
	if err := svc.SetUserStatus(ctx, admin.TenantID, admin.ID, StatusDisabled); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Login(ctx, "maozai", testPassword); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("停用账号登录也该是 ErrInvalidCredentials，实际: %v", err)
	}
}

// TestLoginRefusesAmbiguousTenant 盯住「一个部署对应一个租户」。
//
// 库里有两个活跃租户时**报错而不是猜**：猜错的后果是某个人
// 登进了别人的团队。
func TestLoginRefusesAmbiguousTenant(t *testing.T) {
	svc, store, _, _, _ := seedWeb(t)

	store.tenants["01SECONDTENANTAAAAAAAAAAAA"] = Tenant{
		ID: "01SECONDTENANTAAAAAAAAAAAA", Name: "第二个租户", Status: StatusActive,
	}
	_, _, err := svc.Login(context.Background(), "maozai", testPassword)
	if !errors.Is(err, ErrTenantInactive) {
		t.Fatalf("多于一个活跃租户时应报错而不是猜一个，实际: %v", err)
	}
}

func TestLoginRequiresWebDeps(t *testing.T) {
	store := newFakeStore()
	// 纯 MCP 部署：没有配 hasher 与 sessions
	svc := NewService(store, platform.FixedClock{T: time.Now()}, platform.NewIDGenerator(platform.SystemClock{}), nil, nil)

	if _, _, err := svc.Login(context.Background(), "x", "y"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("没配 Web 依赖时应明确报错，而不是 panic 或静默失败，实际: %v", err)
	}
}

func TestLoginUpgradesWeakPasswordHashWithoutRevokingSessions(t *testing.T) {
	svc, store, _, sessions, admin := seedWeb(t)
	ctx := context.Background()
	u := store.users[admin.ID]
	u.PasswordHash = "weak" + u.PasswordHash
	store.users[admin.ID] = u

	const existingToken = "existing-browser-session"
	if err := sessions.Create(ctx, ports.Session{
		TokenHash: hashSessionToken(existingToken), TenantID: string(admin.TenantID), UserID: string(admin.ID), AuthVersion: u.AuthVersion,
		ExpiresAt: time.Now().Add(time.Hour), CreatedAt: time.Now(), LastSeenAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}

	if _, _, err := svc.Login(ctx, "maozai", testPassword); err != nil {
		t.Fatal(err)
	}
	got := store.users[admin.ID]
	if got.PasswordHash != fakeHashPrefix+testPassword {
		t.Fatalf("weak hash was not upgraded: %q", got.PasswordHash)
	}
	if got.AuthVersion != u.AuthVersion || got.MustChangePw != u.MustChangePw {
		t.Fatalf("derived-hash upgrade changed credential semantics: before=%+v after=%+v", u, got)
	}
	if _, err := svc.ResumeSession(ctx, existingToken); err != nil {
		t.Fatalf("rehash must not revoke an existing valid session: %v", err)
	}
}

func TestLoginRehashCASDoesNotOverwriteConcurrentPasswordReset(t *testing.T) {
	svc, store, _, sessions, admin := seedWeb(t)
	u := store.users[admin.ID]
	u.PasswordHash = "weak" + u.PasswordHash
	store.users[admin.ID] = u
	store.beforePasswordHashUpgrade = func(f *fakeStore, userID platform.ID) {
		current := f.users[userID]
		current.PasswordHash = fakeHashPrefix + "concurrent-new-password"
		current.AuthVersion++
		f.users[userID] = current
	}

	if _, _, err := svc.Login(context.Background(), "maozai", testPassword); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("stale verified password must lose to concurrent reset: %v", err)
	}
	got := store.users[admin.ID]
	if got.PasswordHash != fakeHashPrefix+"concurrent-new-password" || got.AuthVersion != u.AuthVersion+1 {
		t.Fatalf("concurrent reset was overwritten: %+v", got)
	}
	if len(sessions.byHash) != 0 {
		t.Fatal("stale login must not issue a session")
	}
}

func TestLoginRehashCASAcceptsAnotherInstanceUpgrade(t *testing.T) {
	svc, store, _, _, admin := seedWeb(t)
	u := store.users[admin.ID]
	u.PasswordHash = "weak" + u.PasswordHash
	store.users[admin.ID] = u
	store.beforePasswordHashUpgrade = func(f *fakeStore, userID platform.ID) {
		current := f.users[userID]
		current.PasswordHash = fakeHashPrefix + testPassword
		f.users[userID] = current
	}

	if _, principal, err := svc.Login(context.Background(), "maozai", testPassword); err != nil || principal.UserID != admin.ID {
		t.Fatalf("benign concurrent rehash should re-read and succeed: principal=%+v err=%v", principal, err)
	}
}

// ---------------------------------------------------------------------------
// 会话
// ---------------------------------------------------------------------------

func TestResumeSessionRereadsUser(t *testing.T) {
	svc, _, _, _, admin := seedWeb(t)
	ctx := context.Background()

	token, _, err := svc.Login(ctx, "maozai", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ResumeSession(ctx, string(token)); err != nil {
		t.Fatal(err)
	}

	// 停用之后，**下一次请求**就必须失效。
	// 把角色和状态冻在会话里是个常见优化，代价是「停用」变成
	// 「等他下次重新登录才生效」——治理面最不能接受的那种延迟。
	if err := svc.SetUserStatus(ctx, admin.TenantID, admin.ID, StatusDisabled); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.ResumeSession(ctx, string(token)); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("账号停用后会话必须立即失效，实际: %v", err)
	}
}

// TestResumeSessionRereadsUserEvenIfSessionSurvives 补住上一条测试的一个洞。
//
// 上面那条 disable 走的是 SetUserStatus，而它顺手删掉了会话——于是
// ResumeSession 是在「找不到会话」那一步失败的，`!user.IsActive()`
// 那道检查根本没被走到。**探针发现了这件事**：把那道检查去掉，
// 上面那条测试照样绿（2026-08-10）。
//
// 这里直接改库里的行、绕过 SetUserStatus，模拟「会话还在但人已经停用」——
// 那是运维直接改库、或者将来某条新代码路径忘了删会话时的真实状态。
func TestResumeSessionRereadsUserEvenIfSessionSurvives(t *testing.T) {
	svc, store, _, sessions, admin := seedWeb(t)
	ctx := context.Background()

	token, _, err := svc.Login(ctx, "maozai", testPassword)
	if err != nil {
		t.Fatal(err)
	}

	// 绕过 SetUserStatus：会话行仍然完好
	u := store.users[admin.ID]
	u.Status = StatusDisabled
	store.users[admin.ID] = u
	if len(sessions.byHash) != 1 {
		t.Fatal("测试前提不成立：会话应当还在")
	}

	if _, err := svc.ResumeSession(ctx, string(token)); !errors.Is(err, ErrSessionInvalid) {
		t.Fatal("会话还在但人已停用时，必须靠回查用户行拦下来")
	}
	// 顺手清掉那条已经没有意义的会话
	if len(sessions.byHash) != 0 {
		t.Fatal("发现账号已停用时应当把这条会话也删掉，别让它继续占着")
	}
}

// TestResumeSessionPicksUpRoleChange 是同一条设计的另一面。
//
// 角色不缓存进会话：一个刚被降级的账号必须在下一次请求就失去管理页。
func TestResumeSessionPicksUpRoleChange(t *testing.T) {
	svc, store, _, _, admin := seedWeb(t)
	ctx := context.Background()

	token, p, err := svc.Login(ctx, "maozai", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	if !p.IsAdmin() {
		t.Fatal("测试前提不成立：初始应当是管理员")
	}

	u := store.users[admin.ID]
	u.Role = RoleMember
	store.users[admin.ID] = u

	got, err := svc.ResumeSession(ctx, string(token))
	if err != nil {
		t.Fatal(err)
	}
	if got.IsAdmin() {
		t.Fatal("角色变更必须立即生效——冻在会话里等于「降级要等他下次登录」")
	}
}

func TestResumeSessionRejectsExpired(t *testing.T) {
	store := newFakeStore()
	clock := &platform.FixedClock{T: time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)}
	hasher, sessions := &fakeHasher{}, newFakeSessions()
	svc := NewService(store, clock, platform.NewIDGenerator(clock), hasher, sessions)
	ctx := context.Background()

	if _, err := svc.ProvisionTenant(ctx, ProvisionTenantInput{
		TenantName: "测试团队", AdminUsername: "maozai", AdminPassword: testPassword,
	}); err != nil {
		t.Fatal(err)
	}
	token, _, err := svc.Login(ctx, "maozai", testPassword)
	if err != nil {
		t.Fatal(err)
	}

	// 过期判断用可注入的 Clock，不在 SQL 里——那样就有两个时钟源
	clock.T = clock.T.Add(sessionTTL + time.Minute)
	if _, err := svc.ResumeSession(ctx, string(token)); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("过期会话必须失效，实际: %v", err)
	}
}

func TestResumeSessionRejectsUnknownToken(t *testing.T) {
	svc, _, _, _, _ := seedWeb(t)

	if _, err := svc.ResumeSession(context.Background(), "伪造的令牌"); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("未知令牌应无效，实际: %v", err)
	}
	if _, err := svc.ResumeSession(context.Background(), ""); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("空令牌应无效，实际: %v", err)
	}
}

func TestLogoutIsIdempotent(t *testing.T) {
	svc, _, _, sessions, _ := seedWeb(t)
	ctx := context.Background()

	token, _, err := svc.Login(ctx, "maozai", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	if err := svc.Logout(ctx, string(token)); err != nil {
		t.Fatal(err)
	}
	if len(sessions.byHash) != 0 {
		t.Fatal("登出应删掉会话行")
	}
	// 再登出一次仍然成功：调用方的意图是「让这个会话不存在」
	if err := svc.Logout(ctx, string(token)); err != nil {
		t.Fatalf("重复登出应当幂等，实际: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 改密与重置
// ---------------------------------------------------------------------------

func TestChangePasswordKillsAllSessions(t *testing.T) {
	svc, _, _, sessions, admin := seedWeb(t)
	ctx := context.Background()

	// 两个浏览器各登一次
	first, _, err := svc.Login(ctx, "maozai", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Login(ctx, "maozai", testPassword); err != nil {
		t.Fatal(err)
	}
	if len(sessions.byHash) != 2 {
		t.Fatalf("应有两条会话，实际 %d", len(sessions.byHash))
	}

	newPassword := "another-good-password"
	if err := svc.ChangePassword(ctx, admin.TenantID, admin.ID, testPassword, newPassword); err != nil {
		t.Fatal(err)
	}
	// 改密最常见的动机就是「我怀疑密码泄露了」——那时别处已经登录的
	// 浏览器必须一起掉线，否则改密什么也没解决
	if len(sessions.byHash) != 0 {
		t.Fatalf("改密必须废掉全部会话，实际还剩 %d 条", len(sessions.byHash))
	}
	if _, err := svc.ResumeSession(ctx, string(first)); !errors.Is(err, ErrSessionInvalid) {
		t.Fatal("旧会话必须失效")
	}

	// 新密码可用，旧密码不可用，且不再强制改密
	_, p, err := svc.Login(ctx, "maozai", newPassword)
	if err != nil {
		t.Fatal(err)
	}
	if p.MustChangePassword {
		t.Fatal("本人改过密之后不该再被拦在改密页")
	}
	if _, _, err := svc.Login(ctx, "maozai", testPassword); !errors.Is(err, ErrInvalidCredentials) {
		t.Fatal("旧密码必须失效")
	}
}

func TestChangePasswordRequiresOldPassword(t *testing.T) {
	svc, _, _, _, admin := seedWeb(t)

	err := svc.ChangePassword(context.Background(), admin.TenantID, admin.ID, "wrong-old-password", "brand-new-password")
	if !errors.Is(err, ErrInvalidCredentials) {
		t.Fatalf("旧密码不对必须拒绝——否则拿到一张会话票就能改密码，等于账号劫持，实际: %v", err)
	}
}

func TestChangePasswordRejectsSameAndWeak(t *testing.T) {
	svc, _, _, _, admin := seedWeb(t)
	ctx := context.Background()

	if err := svc.ChangePassword(ctx, admin.TenantID, admin.ID, testPassword, testPassword); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("新旧密码相同应拒绝，实际: %v", err)
	}
	if err := svc.ChangePassword(ctx, admin.TenantID, admin.ID, testPassword, "short"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("过短的密码应拒绝，实际: %v", err)
	}
}

func TestResetPasswordForcesChangeAndKillsSessions(t *testing.T) {
	svc, _, _, sessions, admin := seedWeb(t)
	ctx := context.Background()

	if _, _, err := svc.Login(ctx, "maozai", testPassword); err != nil {
		t.Fatal(err)
	}
	// 先自己改一次密，把 must_change_pw 清掉
	settled := "settled-password-x"
	if err := svc.ChangePassword(ctx, admin.TenantID, admin.ID, testPassword, settled); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Login(ctx, "maozai", settled); err != nil {
		t.Fatal(err)
	}

	// 管理员重置：不验旧密码（管理员本来就不知道），但要把 must_change_pw 置回真
	reset := "admin-issued-temp-pw"
	if err := svc.ResetPassword(ctx, admin.TenantID, admin.ID, reset); err != nil {
		t.Fatal(err)
	}
	if len(sessions.byHash) != 0 {
		t.Fatalf("重置密码必须废掉全部会话，实际还剩 %d 条", len(sessions.byHash))
	}
	_, p, err := svc.Login(ctx, "maozai", reset)
	if err != nil {
		t.Fatal(err)
	}
	if !p.MustChangePassword {
		t.Fatal("管理员知道的那个临时密码不该长期有效——必须强制改密")
	}
}

// TestPasswordResetRejectsSessionInsertedAfterDelete models the narrow race
// that a plain DELETE cannot close:
//
//  1. login verifies the old password and prepares a session;
//  2. an administrator resets the password and deletes existing sessions;
//  3. the already-verified login inserts its session after that DELETE.
//
// The late row must still be unusable because it carries the old auth version.
func TestPasswordResetRejectsSessionInsertedAfterDelete(t *testing.T) {
	svc, store, _, sessions, admin := seedWeb(t)
	ctx := context.Background()

	token, _, err := svc.Login(ctx, "maozai", testPassword)
	if err != nil {
		t.Fatal(err)
	}
	hash := hashSessionToken(string(token))
	lateSession := sessions.byHash[hash]
	oldVersion := store.users[admin.ID].AuthVersion

	if err := svc.ResetPassword(ctx, admin.TenantID, admin.ID, "admin-issued-new-password"); err != nil {
		t.Fatal(err)
	}
	if got := store.users[admin.ID].AuthVersion; got != oldVersion+1 {
		t.Fatalf("password reset auth version = %d, want %d", got, oldVersion+1)
	}
	if len(sessions.byHash) != 0 {
		t.Fatal("reset must first delete all sessions")
	}

	// Simulate the old-password login committing immediately after the DELETE.
	sessions.byHash[hash] = lateSession
	if _, err := svc.ResumeSession(ctx, string(token)); !errors.Is(err, ErrSessionInvalid) {
		t.Fatalf("late old-version session must be rejected, got %v", err)
	}
	if len(sessions.byHash) != 0 {
		t.Fatal("version-mismatched session should be removed after rejection")
	}
}

func TestSetUserStatusValidatesValue(t *testing.T) {
	svc, _, _, _, admin := seedWeb(t)

	if err := svc.SetUserStatus(context.Background(), admin.TenantID, admin.ID, "sleeping"); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("非法状态应拒绝，实际: %v", err)
	}
}

func TestCleanupSessionsRemovesExpiredOnly(t *testing.T) {
	store := newFakeStore()
	clock := &platform.FixedClock{T: time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)}
	svc := NewService(store, clock, platform.NewIDGenerator(clock), &fakeHasher{}, newFakeSessions())
	ctx := context.Background()

	if _, err := svc.ProvisionTenant(ctx, ProvisionTenantInput{
		TenantName: "测试团队", AdminUsername: "maozai", AdminPassword: testPassword,
	}); err != nil {
		t.Fatal(err)
	}
	if _, _, err := svc.Login(ctx, "maozai", testPassword); err != nil {
		t.Fatal(err)
	}

	n, err := svc.CleanupSessions(ctx)
	if err != nil {
		t.Fatal(err)
	}
	if n != 0 {
		t.Fatalf("还没过期的会话不该被清掉，实际清了 %d 条", n)
	}

	clock.T = clock.T.Add(sessionTTL + time.Hour)
	if n, err = svc.CleanupSessions(ctx); err != nil || n != 1 {
		t.Fatalf("过期会话应被清掉，实际清了 %d 条 err=%v", n, err)
	}
}
