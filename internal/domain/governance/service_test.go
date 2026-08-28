package governance

import (
	"context"
	"errors"
	"testing"
	"time"

	"claracore-server/internal/platform"
)

var errFake = errors.New("fake store: 注入的故障")

func newTestService(t *testing.T, store Store) (*Service, *platform.FixedClock) {
	t.Helper()
	svc, clock, _, _ := newWebService(t, store)
	return svc, clock
}

// newWebService 额外交出密码哈希与会话存储，供登录相关的用例断言。
func newWebService(t *testing.T, store Store) (*Service, *platform.FixedClock, *fakeHasher, *fakeSessions) {
	t.Helper()
	clock := &platform.FixedClock{T: time.Date(2026, 8, 6, 12, 0, 0, 0, time.UTC)}
	hasher, sessions := &fakeHasher{}, newFakeSessions()
	return NewService(store, clock, platform.NewIDGenerator(clock), hasher, sessions), clock, hasher, sessions
}

// testPassword 满足 MinPasswordLen。夹具里的密码要能真的登录，
// 否则登录用例就得另建一套账号。
const testPassword = "correct-horse-battery"

// seed 建一个可用的租户：租户 + 用户 + agent + 个人空间 + 团队空间 + 一个有效 token。
func seed(t *testing.T, svc *Service) (Identity, string) {
	t.Helper()
	ctx := context.Background()

	res, err := svc.ProvisionTenant(ctx, ProvisionTenantInput{
		TenantName:    "测试团队",
		AdminUsername: "maozai",
		AdminPassword: testPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	agent, err := svc.CreateAgent(ctx, res.Tenant.ID, res.Admin.ID, "clara", "Clara")
	if err != nil {
		t.Fatal(err)
	}
	plaintext, _, err := svc.IssueToken(ctx, IssueTokenInput{
		TenantID: res.Tenant.ID, UserID: res.Admin.ID, AgentID: agent.ID, Name: "MacBook",
	})
	if err != nil {
		t.Fatal(err)
	}

	id, err := svc.Authenticate(ctx, plaintext)
	if err != nil {
		t.Fatal(err)
	}
	return id, plaintext
}

// 签发出来的 token 立刻就能认证——这条串起 IssueToken → 存储 → ParseToken → Authenticate
// 的完整链路，任何一环的 hash 口径不一致都会在这里断掉。
func TestIssuedTokenAuthenticates(t *testing.T) {
	store := newFakeStore()
	svc, _ := newTestService(t, store)
	id, _ := seed(t, svc)

	if id.Username != "maozai" || id.AgentSlug != "clara" {
		t.Errorf("身份解析错误: %+v", id)
	}
	if id.Role != RoleAdmin {
		t.Error("首个用户应当是管理员")
	}
}

func TestAuthenticateRejects(t *testing.T) {
	ctx := context.Background()

	t.Run("畸形 token 不查库", func(t *testing.T) {
		store := newFakeStore()
		svc, _ := newTestService(t, store)
		if _, err := svc.Authenticate(ctx, "随便什么东西"); !errors.Is(err, ErrTokenMalformed) {
			t.Errorf("得到 %v", err)
		}
	})

	t.Run("不存在的 token", func(t *testing.T) {
		store := newFakeStore()
		svc, _ := newTestService(t, store)
		plaintext, _, _, _ := NewTokenSecret()
		if _, err := svc.Authenticate(ctx, plaintext); !errors.Is(err, ErrTokenNotFound) {
			t.Errorf("得到 %v", err)
		}
	})

	t.Run("已吊销的 token", func(t *testing.T) {
		store := newFakeStore()
		svc, _ := newTestService(t, store)
		id, plaintext := seed(t, svc)
		if err := svc.RevokeToken(ctx, id.TenantID, id.TokenID); err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Authenticate(ctx, plaintext); !errors.Is(err, ErrTokenRevoked) {
			t.Errorf("得到 %v", err)
		}
	})

	t.Run("已过期的 token", func(t *testing.T) {
		store := newFakeStore()
		svc, clock := newTestService(t, store)
		id, _ := seed(t, svc)

		expiring, _, err := svc.IssueToken(ctx, IssueTokenInput{
			TenantID: id.TenantID, UserID: id.UserID, AgentID: id.AgentID, TTL: time.Hour,
		})
		if err != nil {
			t.Fatal(err)
		}
		if _, err := svc.Authenticate(ctx, expiring); err != nil {
			t.Fatalf("未到期时应当可用: %v", err)
		}

		clock.T = clock.T.Add(2 * time.Hour)
		if _, err := svc.Authenticate(ctx, expiring); !errors.Is(err, ErrTokenExpired) {
			t.Errorf("过期后应当拒绝，得到 %v", err)
		}
	})

	t.Run("租户被停用", func(t *testing.T) {
		store := newFakeStore()
		svc, _ := newTestService(t, store)
		id, plaintext := seed(t, svc)

		tenant := store.tenants[id.TenantID]
		tenant.Status = StatusSuspended
		store.tenants[id.TenantID] = tenant

		if _, err := svc.Authenticate(ctx, plaintext); !errors.Is(err, ErrTenantInactive) {
			t.Errorf("得到 %v", err)
		}
	})

	t.Run("用户被停用", func(t *testing.T) {
		store := newFakeStore()
		svc, _ := newTestService(t, store)
		id, plaintext := seed(t, svc)

		user := store.users[id.UserID]
		user.Status = StatusDisabled
		store.users[id.UserID] = user

		if _, err := svc.Authenticate(ctx, plaintext); !errors.Is(err, ErrUserDisabled) {
			t.Errorf("得到 %v", err)
		}
	})
}

// 数据损坏导致的跨租户越权，是多租户系统里最不能接受的一类故障。
// 这条检查在正常情况下永远不触发，所以必须有测试盯着它还活着。
func TestAuthenticateRejectsInconsistentData(t *testing.T) {
	ctx := context.Background()

	t.Run("token 指向别的租户", func(t *testing.T) {
		store := newFakeStore()
		svc, _ := newTestService(t, store)
		id, plaintext := seed(t, svc)

		token := store.tokens[id.TokenID]
		token.TenantID = "01OTHERTENANTAAAAAAAAAAAAA"
		store.tokens[id.TokenID] = token
		store.tenants[token.TenantID] = Tenant{ID: token.TenantID, Status: StatusActive}

		if _, err := svc.Authenticate(ctx, plaintext); !errors.Is(err, ErrTokenNotFound) {
			t.Errorf("跨租户的 token 必须被拒绝，得到 %v", err)
		}
	})

	t.Run("agent 不属于该用户", func(t *testing.T) {
		store := newFakeStore()
		svc, _ := newTestService(t, store)
		id, plaintext := seed(t, svc)

		agent := store.agents[id.AgentID]
		agent.UserID = "01OTHERUSERAAAAAAAAAAAAAAA"
		store.agents[id.AgentID] = agent

		if _, err := svc.Authenticate(ctx, plaintext); !errors.Is(err, ErrTokenNotFound) {
			t.Errorf("得到 %v", err)
		}
	})
}

func TestTouchTokenThrottling(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	svc, clock := newTestService(t, store)
	_, plaintext := seed(t, svc)

	before := store.touchCalls // seed 里的首次认证已经 touch 过一次

	if _, err := svc.Authenticate(ctx, plaintext); err != nil {
		t.Fatal(err)
	}
	if store.touchCalls != before {
		t.Error("节流窗口内不应重复写 last_used_at——认证是最热的路径")
	}

	clock.T = clock.T.Add(tokenTouchInterval + time.Minute)
	if _, err := svc.Authenticate(ctx, plaintext); err != nil {
		t.Fatal(err)
	}
	if store.touchCalls != before+1 {
		t.Error("超过节流窗口后应当更新 last_used_at")
	}
}

// last_used_at 是运维辅助信号，不是授权依据：它写不进去不该拒绝一次合法调用。
func TestTouchFailureDoesNotBlockAuth(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	svc, clock := newTestService(t, store)
	_, plaintext := seed(t, svc)

	store.failOnTouch = true
	clock.T = clock.T.Add(tokenTouchInterval + time.Minute)

	if _, err := svc.Authenticate(ctx, plaintext); err != nil {
		t.Errorf("last_used_at 写失败不应影响认证结果，得到 %v", err)
	}
}

func TestProvisionTenantCreatesEverything(t *testing.T) {
	store := newFakeStore()
	svc, _ := newTestService(t, store)

	res, err := svc.ProvisionTenant(context.Background(), ProvisionTenantInput{
		TenantName:    "毛仔的团队",
		AdminUsername: "maozai",
		AdminPassword: testPassword,
	})
	if err != nil {
		t.Fatal(err)
	}

	if res.TeamSpace.Type != SpaceTeam || res.TeamSpace.OwnerUserID != nil {
		t.Error("团队空间应当无 owner")
	}
	if res.Personal.Type != SpacePersonal || !res.Personal.IsOwnedBy(res.Admin.ID) {
		t.Error("管理员的个人空间应当归其所有")
	}
	if !res.Admin.MustChangePw {
		t.Error("首次登录应强制改密")
	}
	if res.TeamSpace.Name != "毛仔的团队" {
		t.Errorf("未指定团队空间名时应沿用租户名，实际 %q", res.TeamSpace.Name)
	}
}

func TestBootstrapTenantOnlyInitializesEmptyDeployment(t *testing.T) {
	store := newFakeStore()
	svc, _ := newTestService(t, store)
	in := ProvisionTenantInput{
		TenantName:    "ClaraCore 团队",
		TeamSpaceName: "团队记忆",
		AdminUsername: "admin",
		AdminPassword: testPassword,
	}

	first, err := svc.BootstrapTenant(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if first.Admin.Username != "admin" || !first.Admin.MustChangePw {
		t.Fatalf("初始化管理员不正确: %+v", first.Admin)
	}

	if _, err := svc.BootstrapTenant(context.Background(), in); !errors.Is(err, ErrAlreadyProvisioned) {
		t.Fatalf("重复初始化应拒绝，得到 %v", err)
	}
	if len(store.tenants) != 1 || len(store.users) != 1 || len(store.spaces) != 2 {
		t.Fatalf("重复初始化不应写入数据: tenants=%d users=%d spaces=%d",
			len(store.tenants), len(store.users), len(store.spaces))
	}
}

// 半途失败留下的「有租户没有团队空间」没有自愈路径，必须整体回滚。
func TestProvisionTenantRollsBackOnFailure(t *testing.T) {
	store := newFakeStore()
	store.failOnCreateSpace = true
	svc, _ := newTestService(t, store)

	_, err := svc.ProvisionTenant(context.Background(), ProvisionTenantInput{
		TenantName:    "会失败的团队",
		AdminUsername: "someone",
		AdminPassword: testPassword,
	})
	if !errors.Is(err, errFake) {
		t.Fatalf("应当把 store 的错误透出，得到 %v", err)
	}
	if len(store.tenants) != 0 || len(store.users) != 0 {
		t.Errorf("失败后不应留下残缺状态：租户 %d 个，用户 %d 个", len(store.tenants), len(store.users))
	}
}

func TestProvisionTenantValidatesInput(t *testing.T) {
	store := newFakeStore()
	svc, _ := newTestService(t, store)

	_, err := svc.ProvisionTenant(context.Background(), ProvisionTenantInput{TenantName: "只有名字"})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("缺少必填项应当报 ErrInvalidArgument，得到 %v", err)
	}
	// 错误信息必须稳定：map 遍历顺序随机，未排序时这条会随机失败
	if got := err.Error(); got != "governance: 参数非法: 缺少 初始密码、管理员账号" {
		t.Errorf("错误信息不稳定或措辞变了: %q", got)
	}
}

func TestCreateUserAlsoCreatesPersonalSpace(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	svc, _ := newTestService(t, store)
	admin, _ := seed(t, svc)

	user, personal, err := svc.CreateUser(ctx, CreateUserInput{
		TenantID: admin.TenantID, Username: "tongshi", Password: testPassword,
	})
	if err != nil {
		t.Fatal(err)
	}
	if user.Role != RoleMember {
		t.Error("未指定角色时应为 member")
	}
	if !personal.IsOwnedBy(user.ID) {
		t.Error("个人空间应当归新用户所有")
	}

	// 新用户能看到团队空间和自己的个人空间，看不到管理员的个人空间
	scope, err := svc.Scope(ctx, Identity{TenantID: user.TenantID, UserID: user.ID})
	if err != nil {
		t.Fatal(err)
	}
	if len(scope.Readable()) != 2 {
		t.Errorf("新用户应可读 2 个空间（团队 + 自己的个人），实际 %d 个", len(scope.Readable()))
	}
	if scope.CanRead(adminPersonalSpace(t, store, admin.UserID)) {
		t.Error("不该看到管理员的个人空间")
	}
}

// TestCreateUserRejectsDuplicateUsername 盯着两件事，别当成重复用例删掉。
//
// 一是唯一性冲突以 ErrAlreadyExists 的形态到达领域层——Host 层要显示
// 「这个账号已经有人用了」，靠的就是它；二是失败时**不覆盖原用户**。
// 后者是 fake 曾经的真实行为：map 直接赋值，重名即静默顶掉原记录，
// 而真实数据库会拒绝。那种 fake 会让领域测试跑在数据库不可能达到的状态上。
func TestCreateUserRejectsDuplicateUsername(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	svc, _ := newTestService(t, store)
	admin, _ := seed(t, svc)

	first, _, err := svc.CreateUser(ctx, CreateUserInput{
		TenantID: admin.TenantID, Username: "tongshi", Password: testPassword,
	})
	if err != nil {
		t.Fatal(err)
	}

	_, _, err = svc.CreateUser(ctx, CreateUserInput{
		TenantID: admin.TenantID, Username: "tongshi", Password: testPassword + "2",
	})
	if !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("同租户重名应返回 ErrAlreadyExists，得到 %v", err)
	}

	got, err := store.FindUserByUsername(ctx, admin.TenantID, "tongshi")
	if err != nil {
		t.Fatal(err)
	}
	if got.ID != first.ID || got.PasswordHash != fakeHashPrefix+testPassword {
		t.Error("失败的创建把原用户覆盖了——密码被换掉等于一次账号劫持")
	}

	// 同名但不同租户必须放行：唯一键是 (tenant_id, username)，不是 username
	if _, _, err := svc.CreateUser(ctx, CreateUserInput{
		TenantID: "01OTHERTENANTAAAAAAAAAAAAA", Username: "tongshi", Password: testPassword,
	}); err != nil {
		t.Errorf("另一个租户下的同名账号被误杀: %v", err)
	}
}

func TestCreateUserRejectsBadRole(t *testing.T) {
	store := newFakeStore()
	svc, _ := newTestService(t, store)
	_, _, err := svc.CreateUser(context.Background(), CreateUserInput{
		TenantID: "01TENANTAAAAAAAAAAAAAAAAAA", Username: "x", Password: testPassword, Role: "superadmin",
	})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Errorf("非法角色应被拒绝，得到 %v", err)
	}
}

func TestGrantSpaceAccess(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	svc, _ := newTestService(t, store)
	admin, _ := seed(t, svc)
	other, _, err := svc.CreateUser(ctx, CreateUserInput{
		TenantID: admin.TenantID, Username: "tongshi", DisplayName: "同事",
		Password: testPassword, Role: RoleMember,
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Run("个人空间不可授权他人", func(t *testing.T) {
		personal := adminPersonalSpace(t, store, admin.UserID)
		err := svc.GrantSpaceAccess(ctx, admin.TenantID, personal, other.ID, admin.UserID, true, false)
		if !errors.Is(err, ErrSpaceForbidden) {
			t.Errorf("个人空间的封闭性是设计约束，得到 %v", err)
		}
	})

	t.Run("可写蕴含可读", func(t *testing.T) {
		project, err := svc.CreateProjectSpace(ctx, admin.TenantID, admin.UserID, "新项目")
		if err != nil {
			t.Fatal(err)
		}
		if err := svc.GrantSpaceAccess(ctx, admin.TenantID, project.ID, other.ID, admin.UserID, false, true); err != nil {
			t.Fatal(err)
		}
		p := store.perms[permKey{project.ID, other.ID}]
		if !p.CanRead {
			t.Error("允许「只写不读」会造出一个能写进去却看不见的空间")
		}
	})

	t.Run("既不读也不写应当报错", func(t *testing.T) {
		project, _ := svc.CreateProjectSpace(ctx, admin.TenantID, admin.UserID, "另一个项目")
		err := svc.GrantSpaceAccess(ctx, admin.TenantID, project.ID, other.ID, admin.UserID, false, false)
		if !errors.Is(err, ErrInvalidArgument) {
			t.Errorf("空授权应当报错而非静默写入一行无意义数据，得到 %v", err)
		}
	})

	t.Run("不能把其他租户用户写进授权表", func(t *testing.T) {
		project, _ := svc.CreateProjectSpace(ctx, admin.TenantID, admin.UserID, "租户边界")
		foreignID := platform.ID("01FOREIGNUSERAAAAAAAAAAAAAA")
		store.users[foreignID] = User{ID: foreignID, TenantID: "01FOREIGNTENANTAAAAAAAAAAA", Username: "foreign"}
		err := svc.GrantSpaceAccess(ctx, admin.TenantID, project.ID, foreignID, admin.UserID, true, false)
		if !errors.Is(err, ErrUserNotFound) {
			t.Fatalf("跨租户授权必须被拒绝，得到 %v", err)
		}
	})

	t.Run("撤销也必须经过租户边界", func(t *testing.T) {
		project, _ := svc.CreateProjectSpace(ctx, admin.TenantID, admin.UserID, "撤销边界")
		if err := svc.GrantSpaceAccess(ctx, admin.TenantID, project.ID, other.ID, admin.UserID, true, false); err != nil {
			t.Fatal(err)
		}
		if err := svc.RevokeSpaceAccess(ctx, "01FOREIGNTENANTAAAAAAAAAAA", project.ID, other.ID); !errors.Is(err, ErrSpaceNotFound) {
			t.Fatalf("跨租户撤销必须被拒绝，得到 %v", err)
		}
		if err := svc.RevokeSpaceAccess(ctx, admin.TenantID, project.ID, other.ID); err != nil {
			t.Fatal(err)
		}
		if _, ok := store.perms[permKey{project.ID, other.ID}]; ok {
			t.Fatal("授权行没有被撤销")
		}
	})
}

// 项目空间的创建者不写授权行，权限来自 ResolveScope 规则——这条验证端到端确实生效。
func TestProjectOwnerHasAccessWithoutPermissionRow(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	svc, _ := newTestService(t, store)
	admin, _ := seed(t, svc)

	project, err := svc.CreateProjectSpace(ctx, admin.TenantID, admin.UserID, "我的项目")
	if err != nil {
		t.Fatal(err)
	}
	if len(store.perms) != 0 {
		t.Error("创建项目空间不该写授权行")
	}

	scope, err := svc.Scope(ctx, admin)
	if err != nil {
		t.Fatal(err)
	}
	if !scope.CanWrite(project.ID) {
		t.Error("创建者不该被自己建的空间锁在门外")
	}
}

func TestRevokeTokenIsIdempotent(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	svc, clock := newTestService(t, store)
	admin, _ := seed(t, svc)

	if err := svc.RevokeToken(ctx, admin.TenantID, admin.TokenID); err != nil {
		t.Fatal(err)
	}
	first := *store.tokens[admin.TokenID].RevokedAt

	clock.T = clock.T.Add(time.Hour)
	if err := svc.RevokeToken(ctx, admin.TenantID, admin.TokenID); err != nil {
		t.Fatalf("重复吊销不应报错: %v", err)
	}
	if !store.tokens[admin.TokenID].RevokedAt.Equal(first) {
		t.Error("重复吊销不应覆盖原时间——那会让审计记录失真")
	}
}

// 一个 agent 多 token 并存是平滑轮换的前提（Desktop 单 token 轮换即全断的教训）。
func TestMultipleTokensPerAgentCoexist(t *testing.T) {
	ctx := context.Background()
	store := newFakeStore()
	svc, _ := newTestService(t, store)
	admin, oldToken := seed(t, svc)

	newToken, _, err := svc.IssueToken(ctx, IssueTokenInput{
		TenantID: admin.TenantID, UserID: admin.UserID, AgentID: admin.AgentID, Name: "iPhone",
	})
	if err != nil {
		t.Fatal(err)
	}

	for name, plaintext := range map[string]string{"旧 token": oldToken, "新 token": newToken} {
		if _, err := svc.Authenticate(ctx, plaintext); err != nil {
			t.Errorf("%s 应当仍然可用: %v", name, err)
		}
	}
}

func adminPersonalSpace(t *testing.T, store *fakeStore, userID platform.ID) platform.ID {
	t.Helper()
	for _, s := range store.spaces {
		if s.Type == SpacePersonal && s.IsOwnedBy(userID) {
			return s.ID
		}
	}
	t.Fatal("没找到个人空间")
	return ""
}
