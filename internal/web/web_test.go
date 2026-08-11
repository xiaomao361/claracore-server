package web

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"claracore-server/internal/domain/activity"
	"claracore-server/internal/domain/governance"
	"claracore-server/internal/domain/knowledge"
	"claracore-server/internal/platform"
)

// fakeGov 是治理域的替身。
//
// Web 层的 bug（中间件顺序、CSRF、cookie 属性、越权）与存储层无关。
// 用真库测等于把两件事绑死：数据库连不上时，这一层的回归就测不了了。
type fakeGov struct {
	principal governance.Principal
	loginErr  error
	sessions  map[string]governance.Principal

	tokens     []governance.Token
	users      []governance.User
	lastReset  platform.ID
	lastStatus struct {
		user   platform.ID
		status string
	}
	revoked platform.ID
	created governance.CreateUserInput
	calls   int
}

const (
	tenantA   = platform.ID("01TENANTAAAAAAAAAAAAAAAAAA")
	userMe    = platform.ID("01USERMEAAAAAAAAAAAAAAAAAA")
	userOther = platform.ID("01USEROTHERAAAAAAAAAAAAAAA")
	spPers    = platform.ID("01SPACEPERSONALAAAAAAAAAAA")
	spTeam    = platform.ID("01SPACETEAMAAAAAAAAAAAAAAA")
	goodToken = "session-token-for-tests"
)

func newFakeGov(role string, mustChange bool) *fakeGov {
	p := governance.Principal{
		TenantID: tenantA, UserID: userMe, Username: "maozai", DisplayName: "毛仔",
		Role: role, MustChangePassword: mustChange,
		SessionExpiresAt: time.Now().Add(time.Hour),
	}
	return &fakeGov{
		principal: p,
		sessions:  map[string]governance.Principal{goodToken: p},
		users: []governance.User{
			{ID: userMe, TenantID: tenantA, Username: "maozai", DisplayName: "毛仔", Role: role, Status: governance.StatusActive},
			{ID: userOther, TenantID: tenantA, Username: "tongshi", DisplayName: "同事", Role: governance.RoleMember, Status: governance.StatusActive},
		},
	}
}

func (f *fakeGov) Login(_ context.Context, username, password string) (governance.SessionToken, governance.Principal, error) {
	f.calls++
	if f.loginErr != nil {
		return "", governance.Principal{}, f.loginErr
	}
	if username != "maozai" || password == "" {
		return "", governance.Principal{}, governance.ErrInvalidCredentials
	}
	f.sessions[goodToken] = f.principal
	return governance.SessionToken(goodToken), f.principal, nil
}

func (f *fakeGov) ResumeSession(_ context.Context, token string) (governance.Principal, error) {
	p, ok := f.sessions[token]
	if !ok {
		return governance.Principal{}, governance.ErrSessionInvalid
	}
	return p, nil
}

func (f *fakeGov) Logout(_ context.Context, token string) error {
	delete(f.sessions, token)
	return nil
}

func (f *fakeGov) ChangePassword(_ context.Context, _, _ platform.ID, old, _ string) error {
	f.calls++
	if old != "current-password" {
		return governance.ErrInvalidCredentials
	}
	p := f.principal
	p.MustChangePassword = false
	f.principal = p
	return nil
}

func (f *fakeGov) ResetPassword(_ context.Context, _, userID platform.ID, _ string) error {
	f.calls++
	f.lastReset = userID
	return nil
}

func (f *fakeGov) SetUserStatus(_ context.Context, _, userID platform.ID, status string) error {
	f.calls++
	f.lastStatus.user, f.lastStatus.status = userID, status
	return nil
}

func (f *fakeGov) ListUsers(_ context.Context, _ platform.ID) ([]governance.User, error) {
	return f.users, nil
}

func (f *fakeGov) CreateUser(_ context.Context, in governance.CreateUserInput) (governance.User, governance.Space, error) {
	f.calls++
	f.created = in
	return governance.User{ID: "01NEWUSERAAAAAAAAAAAAAAAAA", Username: in.Username}, governance.Space{}, nil
}

// Scope 给两个空间：个人（可读可写）与团队（隐含可读、写需授权而未授权）。
//
// 这个不对称是有意的——「可读集合」与「可写集合」在 Web 层是两条
// 不同的路径（浏览用前者，审核用后者），夹具里若只有一个可读可写的
// 空间，那两条路径就永远测不出区别。
func (f *fakeGov) Scope(_ context.Context, _ governance.Identity) (governance.SpaceScope, error) {
	owner := userMe
	return governance.ResolveScope(userMe, []governance.SpaceMembership{
		{Space: governance.Space{ID: spPers, TenantID: tenantA, Type: governance.SpacePersonal, Name: "毛仔", OwnerUserID: &owner}},
		{Space: governance.Space{ID: spTeam, TenantID: tenantA, Type: governance.SpaceTeam, Name: "团队"}},
	}), nil
}

func (f *fakeGov) CreateAgent(_ context.Context, _, _ platform.ID, slug, _ string) (governance.Agent, error) {
	f.calls++
	if slug == "taken" {
		return governance.Agent{}, governance.ErrAlreadyExists
	}
	return governance.Agent{ID: "01AGENTAAAAAAAAAAAAAAAAAAA", Slug: slug}, nil
}

func (f *fakeGov) IssueToken(_ context.Context, _ governance.IssueTokenInput) (string, governance.Token, error) {
	f.calls++
	return "ccs_plaintext_token_value", governance.Token{ID: "01TOKENAAAAAAAAAAAAAAAAAAA"}, nil
}

func (f *fakeGov) ListTokens(_ context.Context, _, _ platform.ID) ([]governance.Token, error) {
	return f.tokens, nil
}

func (f *fakeGov) RevokeToken(_ context.Context, _, tokenID platform.ID) error {
	f.calls++
	f.revoked = tokenID
	return nil
}

type fakeKnow struct {
	items      []knowledge.ContentItem
	total      int
	card       knowledge.Card
	memory     knowledge.Memory
	lastList   knowledge.ListContentsInput
	lastUpdate knowledge.UpdateCardInput
	updateErr  error
	getErr     error
	calls      int
}

type fakeActivity struct {
	records        []activity.RecordInput
	visibleInput   activity.ListVisibleInput
	adminInput     activity.ListAdminInput
	visiblePage    activity.Page
	adminPage      activity.Page
	recordErr      error
	visibleErr     error
	adminErr       error
	listAdminCalls int
	order          []string
}

func (f *fakeActivity) Record(_ context.Context, in activity.RecordInput) (activity.Event, error) {
	f.order = append(f.order, "record")
	f.records = append(f.records, in)
	if f.recordErr != nil {
		return activity.Event{}, f.recordErr
	}
	return activity.Event{Action: in.Action}, nil
}

func (f *fakeActivity) ListVisible(_ context.Context, in activity.ListVisibleInput) (activity.Page, error) {
	f.order = append(f.order, "visible")
	f.visibleInput = in
	return f.visiblePage, f.visibleErr
}

func (f *fakeActivity) ListAdmin(_ context.Context, in activity.ListAdminInput) (activity.Page, error) {
	f.order = append(f.order, "admin")
	f.listAdminCalls++
	f.adminInput = in
	return f.adminPage, f.adminErr
}

func (*fakeKnow) SpaceStats(_ context.Context, _ platform.ID, spaces []platform.ID) ([]knowledge.SpaceStats, error) {
	out := make([]knowledge.SpaceStats, 0, len(spaces))
	for _, id := range spaces {
		out = append(out, knowledge.SpaceStats{SpaceID: id, Memories: 3, Cards: 2, DraftCards: 1, UnqualifiedCards: 1, RecallTriggers: 4})
	}
	return out, nil
}

func newTestWeb(t *testing.T, role string, mustChange bool) (*httptest.Server, *fakeGov) {
	t.Helper()
	gov := newFakeGov(role, mustChange)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	activity := &fakeActivity{}
	know := &fakeKnow{
		card: knowledge.Card{
			ID: "01CARDAAAAAAAAAAAAAAAAAAAA", TenantID: tenantA, SpaceID: spPers,
			Title: "摄取来的文档", CardType: "摄取原料", Status: knowledge.CardDraft,
			Source: "docs/x.md", Original: "一段原文。", Version: 1,
		},
		memory: knowledge.Memory{
			ID: "01MEMORYAAAAAAAAAAAAAAAAAA", TenantID: tenantA, SpaceID: spPers,
			Title: "一条记忆", Body: "正文。", Status: knowledge.MemoryActive, Version: 1,
		},
	}
	srv := httptest.NewServer(NewHost(gov, know, activity, logger, Options{
		Version: "test", PublicMCPURL: "https://ccs.example.com/mcp",
	}).Handler())
	t.Cleanup(srv.Close)
	return srv, gov
}

// newContentWeb 额外交出核心域替身，供审核/浏览页的用例断言。
func newContentWeb(t *testing.T, role string) (*httptest.Server, *fakeGov, *fakeKnow) {
	t.Helper()
	gov := newFakeGov(role, false)
	know := &fakeKnow{
		card: knowledge.Card{
			ID: "01CARDAAAAAAAAAAAAAAAAAAAA", TenantID: tenantA, SpaceID: spPers,
			Title: "摄取来的文档", CardType: "摄取原料", Status: knowledge.CardDraft,
			Source: "docs/x.md", Original: "一段原文。", Version: 1,
		},
		memory: knowledge.Memory{
			ID: "01MEMORYAAAAAAAAAAAAAAAAAA", TenantID: tenantA, SpaceID: spPers,
			Title: "一条记忆", Body: "正文。", Status: knowledge.MemoryActive, Version: 1,
		},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(NewHost(gov, know, &fakeActivity{}, logger, Options{Version: "test"}).Handler())
	t.Cleanup(srv.Close)
	return srv, gov, know
}

func newActivityWeb(t *testing.T, role string) (*httptest.Server, *fakeGov, *fakeKnow, *fakeActivity) {
	t.Helper()
	gov := newFakeGov(role, false)
	know := &fakeKnow{
		card: knowledge.Card{
			ID: "01CARDAAAAAAAAAAAAAAAAAAAA", TenantID: tenantA, SpaceID: spPers,
			Title: "摄取来的文档", CardType: "摄取原料", Status: knowledge.CardDraft,
			Source: "docs/x.md", Original: "一段原文。", Version: 1,
		},
	}
	act := &fakeActivity{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(NewHost(gov, know, act, logger, Options{Version: "test"}).Handler())
	t.Cleanup(srv.Close)
	return srv, gov, know, act
}

// client 返回一个不自动跳转的客户端：重定向本身是被测行为。
func client(srv *httptest.Server) *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func get(t *testing.T, srv *httptest.Server, path string, withSession bool) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if withSession {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: goodToken})
	}
	resp, err := client(srv).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func post(t *testing.T, srv *httptest.Server, path string, form url.Values, withSession, withCSRF bool) *http.Response {
	t.Helper()
	if withCSRF {
		form.Set("csrf", csrfToken(goodToken))
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if withSession {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: goodToken})
	}
	resp, err := client(srv).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// 认证边界
// ---------------------------------------------------------------------------

// TestUnauthenticatedIsRedirectedEverywhere 扫全部受保护路径。
//
// 逐个 handler 判断的话，少判一次不会有编译错误，只会有一个
// 不需要登录就能打开的页面。这条测试盯的正是「新加的路由忘了挂中间件」。
func TestUnauthenticatedIsRedirectedEverywhere(t *testing.T) {
	srv, _ := newTestWeb(t, governance.RoleAdmin, false)

	for _, path := range []string{"/", "/tokens", "/password", "/activity", "/admin/members", "/admin/audit"} {
		t.Run(path, func(t *testing.T) {
			resp := get(t, srv, path, false)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusSeeOther {
				t.Fatalf("未登录访问 %s 应重定向，实际 %d", path, resp.StatusCode)
			}
			if loc := resp.Header.Get("Location"); loc != "/login" {
				t.Fatalf("应跳到登录页，实际 %q", loc)
			}
		})
	}
}

func TestMemberCannotSeeAdminPages(t *testing.T) {
	srv, gov := newTestWeb(t, governance.RoleMember, false)

	for _, path := range []string{"/admin/members", "/admin/audit"} {
		resp := get(t, srv, path, true)
		defer resp.Body.Close()
		// 404 而不是 403：不确认「这个管理页存在」
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("普通成员访问 %s 应得到 404，实际 %d", path, resp.StatusCode)
		}
	}
	if gov.calls != 0 {
		t.Fatal("权限没通过时不该调领域层")
	}
}

func TestMemberCannotPostToAdminEndpoints(t *testing.T) {
	srv, gov := newTestWeb(t, governance.RoleMember, false)

	for _, path := range []string{"/admin/members", "/admin/members/status", "/admin/members/reset", "/admin/audit"} {
		t.Run(path, func(t *testing.T) {
			resp := post(t, srv, path, url.Values{"id": {string(userOther)}, "status": {"disabled"}}, true, true)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("普通成员 POST %s 应被拒绝，实际 %d", path, resp.StatusCode)
			}
		})
	}
	if gov.calls != 0 {
		t.Fatal("越权的写请求一次都不该到达领域层")
	}
}

// TestMustChangePasswordBlocksEverythingElse 盯住强制改密。
//
// 管理员分配的初始密码至少经过一次口头或聊天工具的传递，
// 在改掉之前不该能做别的事。
func TestMustChangePasswordBlocksEverythingElse(t *testing.T) {
	srv, _ := newTestWeb(t, governance.RoleAdmin, true)

	for _, path := range []string{"/", "/tokens", "/activity", "/admin/members", "/admin/audit"} {
		resp := get(t, srv, path, true)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/password" {
			t.Fatalf("%s 应被赶去改密页，实际 %d → %q", path, resp.StatusCode, resp.Header.Get("Location"))
		}
	}

	// 改密页自己必须进得去，否则就是一个谁也解不开的死锁
	resp := get(t, srv, "/password", true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("改密页必须可访问，实际 %d", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// CSRF
// ---------------------------------------------------------------------------

// TestWritesRequireCSRF 扫全部写路径。
//
// 与认证那条同一个理由：这条盯的是「新加的 POST 路由忘了校 CSRF」。
func TestWritesRequireCSRF(t *testing.T) {
	srv, gov := newTestWeb(t, governance.RoleAdmin, false)

	writes := map[string]url.Values{
		"/logout":                            {},
		"/password":                          {"current": {"x"}, "new": {"yyyyyyyyyy"}, "confirm": {"yyyyyyyyyy"}},
		"/tokens":                            {"agent": {"clara"}, "name": {"MacBook"}},
		"/tokens/revoke":                     {"id": {"01TOKENAAAAAAAAAAAAAAAAAAA"}},
		"/review/01CARDAAAAAAAAAAAAAAAAAAAA": {"action": {"reject"}},
		"/admin/members":                     {"username": {"newbie"}},
		"/admin/members/status":              {"id": {string(userOther)}, "status": {"disabled"}},
		"/admin/members/reset":               {"id": {string(userOther)}},
		"/admin/audit":                       {},
	}
	for path, form := range writes {
		t.Run(path, func(t *testing.T) {
			resp := post(t, srv, path, form, true, false) // 不带 csrf
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("没有 CSRF token 的写请求应被拒绝，实际 %d", resp.StatusCode)
			}
		})
	}
	if gov.calls != 0 {
		t.Fatal("CSRF 没过的请求一次都不该到达领域层")
	}
}

// TestCSRFTokenIsSessionBound 确认 CSRF token 不能跨会话复用。
func TestCSRFTokenIsSessionBound(t *testing.T) {
	if csrfToken("session-a") == csrfToken("session-b") {
		t.Fatal("不同会话必须派生出不同的 CSRF token")
	}
	// 派生用 HMAC 而不是明文拼接：即使派生值泄露也推不回会话令牌
	if strings.Contains(csrfToken(goodToken), goodToken) {
		t.Fatal("CSRF token 里不该出现会话令牌本身")
	}
}

// ---------------------------------------------------------------------------
// 登录与 cookie
// ---------------------------------------------------------------------------

func TestLoginSetsHardenedCookie(t *testing.T) {
	srv, _ := newTestWeb(t, governance.RoleAdmin, false)

	resp := post(t, srv, "/login", url.Values{"username": {"maozai"}, "password": {"pw"}}, false, false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("登录成功应重定向，实际 %d", resp.StatusCode)
	}

	var found *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie {
			found = c
		}
	}
	if found == nil {
		t.Fatal("应当下发会话 cookie")
	}
	// HttpOnly：即使页面上出现 XSS，会话令牌也不会被脚本偷走
	if !found.HttpOnly {
		t.Fatal("会话 cookie 必须是 HttpOnly")
	}
	// SameSite=Lax：跨站发起的 POST 不带 cookie，这是 CSRF 的第一道门
	if found.SameSite != http.SameSiteLaxMode {
		t.Fatalf("会话 cookie 应为 SameSite=Lax，实际 %v", found.SameSite)
	}
}

func TestLoginFailureSaysNothingSpecific(t *testing.T) {
	srv, gov := newTestWeb(t, governance.RoleAdmin, false)
	gov.loginErr = governance.ErrInvalidCredentials

	resp := post(t, srv, "/login", url.Values{"username": {"nobody"}, "password": {"x"}}, false, false)
	page := body(t, resp)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("登录失败应返回 401，实际 %d", resp.StatusCode)
	}
	// 区分「账号不存在」与「密码不对」等于提供账号枚举接口
	for _, leak := range []string{"不存在", "已停用", "disabled"} {
		if strings.Contains(page, leak) {
			t.Fatalf("登录失败页不得泄露具体原因（出现了 %q）", leak)
		}
	}
}

func TestLoginRedirectsToPasswordWhenMustChange(t *testing.T) {
	srv, _ := newTestWeb(t, governance.RoleAdmin, true)

	resp := post(t, srv, "/login", url.Values{"username": {"maozai"}, "password": {"pw"}}, false, false)
	defer resp.Body.Close()
	if loc := resp.Header.Get("Location"); loc != "/password" {
		t.Fatalf("首次登录应直接去改密页，实际 %q", loc)
	}
}

func TestLogoutClearsCookieAndSession(t *testing.T) {
	srv, gov := newTestWeb(t, governance.RoleAdmin, false)

	resp := post(t, srv, "/logout", url.Values{}, true, true)
	defer resp.Body.Close()
	if _, still := gov.sessions[goodToken]; still {
		t.Fatal("登出应删掉会话")
	}
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie && c.MaxAge >= 0 {
			t.Fatal("登出应清掉 cookie")
		}
	}
}

// ---------------------------------------------------------------------------
// token 页
// ---------------------------------------------------------------------------

func TestTokenCreateShowsPlaintextOnceWithSnippet(t *testing.T) {
	srv, _ := newTestWeb(t, governance.RoleMember, false)

	resp := post(t, srv, "/tokens", url.Values{"agent": {"clara"}, "name": {"MacBook"}}, true, true)
	page := body(t, resp)

	if !strings.Contains(page, "ccs_plaintext_token_value") {
		t.Fatal("创建后应展示一次明文")
	}
	// 不要让人自己拼 JSON——接入体验的差距全在这里
	if !strings.Contains(page, "https://ccs.example.com/mcp") {
		t.Fatal("应给出可直接粘贴的 MCP 配置，且地址是对外地址而不是监听地址")
	}
	if !strings.Contains(page, "Bearer ccs_plaintext_token_value") {
		t.Fatal("配置片段里应已经嵌好 token")
	}

	// 再打开一次页面，明文不该再出现
	fresh := body(t, get(t, srv, "/tokens", true))
	if strings.Contains(fresh, "ccs_plaintext_token_value") {
		t.Fatal("明文只该出现那一次——库里只有哈希，刷新后就取不回来了")
	}
}

// TestTokenRevokeRejectsSomeoneElsesToken 是一条越权防线。
//
// RevokeToken 只按 tenant 过滤，所以「这个 token 是不是我的」
// 必须由 Host 检查——否则拿到一个别人的 token ID 就能把同事的 agent 停掉。
func TestTokenRevokeRejectsSomeoneElsesToken(t *testing.T) {
	srv, gov := newTestWeb(t, governance.RoleMember, false)
	gov.tokens = []governance.Token{{ID: "01MYTOKENAAAAAAAAAAAAAAAAA", Name: "我的"}}

	resp := post(t, srv, "/tokens/revoke", url.Values{"id": {"01SOMEONEELSESAAAAAAAAAAAA"}}, true, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("吊销别人的 token 应得到 404，实际 %d", resp.StatusCode)
	}
	if gov.revoked != "" {
		t.Fatalf("不该真的吊销任何 token，实际吊销了 %s", gov.revoked)
	}

	// 自己的可以吊销
	resp2 := post(t, srv, "/tokens/revoke", url.Values{"id": {"01MYTOKENAAAAAAAAAAAAAAAAA"}}, true, true)
	defer resp2.Body.Close()
	if gov.revoked != "01MYTOKENAAAAAAAAAAAAAAAAA" {
		t.Fatal("自己的 token 应当能吊销")
	}
}

func TestDuplicateAgentSlugGivesUsefulMessage(t *testing.T) {
	srv, _ := newTestWeb(t, governance.RoleMember, false)

	resp := post(t, srv, "/tokens", url.Values{"agent": {"taken"}, "name": {"MacBook"}}, true, true)
	page := body(t, resp)
	if !strings.Contains(page, "taken") {
		t.Fatalf("重名应给出能照着改的提示，实际页面: %.200s", page)
	}
}

// ---------------------------------------------------------------------------
// 成员管理
// ---------------------------------------------------------------------------

func TestCreateMemberGeneratesInitialPassword(t *testing.T) {
	srv, gov := newTestWeb(t, governance.RoleAdmin, false)

	resp := post(t, srv, "/admin/members", url.Values{
		"username": {"newbie"}, "display": {"新人"}, "role": {"member"},
	}, true, true)
	page := body(t, resp)

	// 初始密码由服务端生成：管理员自己想的那个大概率是 Team2026!，
	// 而它会被同一批人共用
	if gov.created.Password == "" {
		t.Fatal("应由服务端生成初始密码")
	}
	if len(gov.created.Password) < governance.MinPasswordLen {
		t.Fatalf("生成的密码太短: %d 位", len(gov.created.Password))
	}
	if !strings.Contains(page, gov.created.Password) {
		t.Fatal("应把初始密码展示给管理员，让他发给本人")
	}
}

func TestAdminCannotDisableSelf(t *testing.T) {
	srv, gov := newTestWeb(t, governance.RoleAdmin, false)

	resp := post(t, srv, "/admin/members/status", url.Values{
		"id": {string(userMe)}, "status": {"disabled"},
	}, true, true)
	page := body(t, resp)

	// 把自己关在门外之后没有恢复路径：无自助注册，没有第二个入口
	if gov.lastStatus.user != "" {
		t.Fatal("不该真的停用自己")
	}
	if !strings.Contains(page, "不能停用自己") {
		t.Fatalf("应给出明确提示，实际页面: %.200s", page)
	}
}

func TestAdminCanDisableOthers(t *testing.T) {
	srv, gov := newTestWeb(t, governance.RoleAdmin, false)

	resp := post(t, srv, "/admin/members/status", url.Values{
		"id": {string(userOther)}, "status": {"disabled"},
	}, true, true)
	defer resp.Body.Close()
	if gov.lastStatus.user != userOther || gov.lastStatus.status != "disabled" {
		t.Fatalf("应停用目标账号，实际 %+v", gov.lastStatus)
	}
}

// ---------------------------------------------------------------------------
// 渲染
// ---------------------------------------------------------------------------

func TestOverviewShowsDraftBacklog(t *testing.T) {
	srv, _ := newTestWeb(t, governance.RoleMember, false)

	page := body(t, get(t, srv, "/", true))
	// 04 §6.1 点名的那个空白：draft 需要一个可见入口，
	// 否则摄取灌进来几千条，没人知道它们在等谁
	if !strings.Contains(page, "待审草稿") {
		t.Fatal("概览页必须给出待审草稿的入口")
	}
	if !strings.Contains(page, "毛仔") {
		t.Fatal("应列出可访问的空间")
	}
}

// TestTemplatesEscapeUserInput 确认没人把 html/template 换成 text/template。
func TestTemplatesEscapeUserInput(t *testing.T) {
	srv, gov := newTestWeb(t, governance.RoleAdmin, false)
	gov.users = []governance.User{{
		ID: userOther, TenantID: tenantA, Username: "tongshi",
		DisplayName: `<script>alert(1)</script>`, Role: governance.RoleMember, Status: governance.StatusActive,
	}}

	page := body(t, get(t, srv, "/admin/members", true))
	if strings.Contains(page, "<script>alert(1)</script>") {
		t.Fatal("用户输入必须被转义——这条红了说明有人把 html/template 换成了 text/template")
	}
	if !strings.Contains(page, "&lt;script&gt;") {
		t.Fatal("应当出现转义后的内容")
	}
}

func TestSecurityHeadersArePresent(t *testing.T) {
	srv, _ := newTestWeb(t, governance.RoleAdmin, false)

	resp := get(t, srv, "/login", false)
	defer resp.Body.Close()
	csp := resp.Header.Get("Content-Security-Policy")
	if !strings.Contains(csp, "default-src 'self'") {
		t.Fatalf("缺少 CSP，实际 %q", csp)
	}
	// 模板是纯 SSR，没有内联脚本也不打算有——CSP 里出现 unsafe-inline
	// 说明有人开始往页面里塞 <script> 了
	if strings.Contains(csp, "unsafe-inline") {
		t.Fatal("CSP 不该有 unsafe-inline：前后端不分离是明确决定，别从一行内联脚本开始滑坡")
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("缺少 nosniff")
	}
}

func (f *fakeKnow) ListContents(_ context.Context, in knowledge.ListContentsInput) (knowledge.ListContentsResult, error) {
	f.lastList = in
	f.calls++
	return knowledge.ListContentsResult{Items: f.items, Total: f.total}, nil
}

func (f *fakeKnow) GetCard(_ context.Context, _, _ platform.ID, spaces []platform.ID) (knowledge.Card, []knowledge.Link, error) {
	f.calls++
	if f.getErr != nil {
		return knowledge.Card{}, nil, f.getErr
	}
	f.lastList.Spaces = spaces
	return f.card, nil, nil
}

func (f *fakeKnow) GetMemory(_ context.Context, _, _ platform.ID, spaces []platform.ID) (knowledge.Memory, []knowledge.Link, error) {
	f.calls++
	if f.getErr != nil {
		return knowledge.Memory{}, nil, f.getErr
	}
	f.lastList.Spaces = spaces
	return f.memory, nil, nil
}

func (f *fakeKnow) UpdateCard(_ context.Context, in knowledge.UpdateCardInput) (knowledge.Card, error) {
	f.lastUpdate = in
	f.calls++
	if f.updateErr != nil {
		return knowledge.Card{}, f.updateErr
	}
	return f.card, nil
}

// ---------------------------------------------------------------------------
// 卡片审核：Web 上唯一的内容写入场景
// ---------------------------------------------------------------------------

// TestReviewQueueUsesWritableSpaces 盯住「能写就能审」这条。
//
// 用可读集合的话，人会看到一堆自己按不动的按钮；更糟的是，
// 那意味着权限判断在提交时才发生，而那时他已经填了一屏字。
func TestReviewQueueUsesWritableSpaces(t *testing.T) {
	srv, _, know := newContentWeb(t, governance.RoleMember)

	if _, errMsg := getBody(t, srv, "/review"); errMsg != "" {
		t.Fatal(errMsg)
	}
	in := know.lastList
	if in.Kind != knowledge.EntityCard {
		t.Fatalf("审核队列只看卡片，实际 %q", in.Kind)
	}
	// 只查 draft：队列的全部意义就是「还没人过目的那些」
	if len(in.Statuses) != 1 || in.Statuses[0] != knowledge.CardDraft {
		t.Fatalf("审核队列应只查 draft，实际 %v", in.Statuses)
	}
	// fakeGov 里这个人只对个人空间可写（team 是隐含可读、写需授权）
	for _, id := range in.Spaces {
		if id == spTeam {
			t.Fatal("团队空间只可读不可写，不该出现在审核队列的空间集合里")
		}
	}
}

func TestReviewPublishGoesThroughUpdateCard(t *testing.T) {
	srv, _, know := newContentWeb(t, governance.RoleMember)

	resp := post(t, srv, "/review/01CARDAAAAAAAAAAAAAAAAAAAA", url.Values{
		"action": {"publish"}, "title": {"审过之后的标题"}, "cardType": {"协作安全"},
		"understanding": {"我想明白的那一层"}, "triggers": {"第一条\n\n第二条\n"},
	}, true, true)
	defer resp.Body.Close()

	in := know.lastUpdate
	if in.Status == nil || *in.Status != knowledge.CardActive {
		t.Fatalf("确认发布应把状态改成 active，实际 %v", in.Status)
	}
	// 审核就是一次带写权限校验的更新，没有独立的审核实体
	if len(in.WritableSpaces) == 0 {
		t.Fatal("必须用可写集合定位")
	}
	// 摄取给的标题是文件名、类型一律「摄取原料」，不让改的话
	// 审完的卡类型全是一个值，card_search 的过滤就废了
	if in.Title == nil || *in.Title != "审过之后的标题" {
		t.Fatal("审核时应能改标题")
	}
	if in.CardType == nil || *in.CardType != "协作安全" {
		t.Fatal("审核时应能改卡片类型")
	}
	if in.RecallTriggers == nil || len(*in.RecallTriggers) != 2 {
		t.Fatalf("召回条件应按行拆分并去掉空行，实际 %v", in.RecallTriggers)
	}
}

// TestReviewRejectUsesDeprecated 锁住退回语义。
//
// 留在 draft 的话，几千条永远挂着，「还有 N 条待审」那个数字
// 就失去意义了——而那个数字正是审核队列存在的理由。
func TestReviewRejectUsesDeprecated(t *testing.T) {
	srv, _, know := newContentWeb(t, governance.RoleMember)

	resp := post(t, srv, "/review/01CARDAAAAAAAAAAAAAAAAAAAA", url.Values{
		"action": {"reject"}, "title": {"不需要的文档"}, "cardType": {"摄取原料"},
	}, true, true)
	defer resp.Body.Close()

	if know.lastUpdate.Status == nil || *know.lastUpdate.Status != knowledge.CardDeprecated {
		t.Fatalf("标记不需要应落成 deprecated，实际 %v", know.lastUpdate.Status)
	}
}

func TestReviewRejectsUnknownAction(t *testing.T) {
	srv, _, know := newContentWeb(t, governance.RoleMember)

	resp := post(t, srv, "/review/01CARDAAAAAAAAAAAAAAAAAAAA", url.Values{
		"action": {"delete-everything"},
	}, true, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("未知动作应被拒，实际 %d", resp.StatusCode)
	}
	if know.lastUpdate.Status != nil {
		t.Fatal("未知动作不该走到领域层")
	}
}

// TestReviewKeepsInputOnValidationError 是一条体验属性，但它有真实后果。
//
// 把人填了一屏的字丢掉，他下次就不敢再点那个按钮了——而审核
// 本来就是最容易被拖延的事。
func TestReviewKeepsInputOnValidationError(t *testing.T) {
	srv, _, know := newContentWeb(t, governance.RoleMember)
	know.updateErr = errors.New("knowledge: 参数非法: 卡片至少要有一个 section 有内容")

	resp := post(t, srv, "/review/01CARDAAAAAAAAAAAAAAAAAAAA", url.Values{
		"action": {"publish"}, "title": {"我刚打的标题"},
		"understanding": {"我刚打的理解"}, "triggers": {"我刚写的召回条件"},
	}, true, true)
	page := body(t, resp)

	for _, typed := range []string{"我刚打的标题", "我刚打的理解", "我刚写的召回条件"} {
		if !strings.Contains(page, typed) {
			t.Fatalf("校验失败后必须把人填的内容放回表单，丢了 %q", typed)
		}
	}
}

func TestReviewDetailNotFoundIsNot500(t *testing.T) {
	srv, _, know := newContentWeb(t, governance.RoleMember)
	know.getErr = knowledge.ErrNotFound

	resp := get(t, srv, "/review/01NOSUCHCARDAAAAAAAAAAAAAA", true)
	defer resp.Body.Close()
	// 不区分「不存在」与「无权访问」：区分等于让任何人用一个 ID
	// 就能探测出「这条内容存在于某个我看不见的空间」
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("取不到应是 404，实际 %d", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// 内容浏览
// ---------------------------------------------------------------------------

func TestBrowseUsesReadableSpacesAndShowsAllStatuses(t *testing.T) {
	srv, _, know := newContentWeb(t, governance.RoleMember)

	if _, errMsg := getBody(t, srv, "/browse"); errMsg != "" {
		t.Fatal(errMsg)
	}
	in := know.lastList
	// 浏览用可读集合——看得见但改不了是完全正常的状态
	if len(in.Spaces) < 2 {
		t.Fatalf("应覆盖全部可读空间（含只读的团队空间），实际 %v", in.Spaces)
	}
	// 不传状态 = 全部状态。治理面必须看得到 draft 与 deprecated，
	// 那正是它存在的理由
	if len(in.Statuses) != 0 {
		t.Fatalf("默认不该过滤状态，实际 %v", in.Statuses)
	}
	if in.Kind != knowledge.EntityCard {
		t.Fatalf("默认看卡片（核心载体），实际 %q", in.Kind)
	}
}

func TestBrowseHonoursFilters(t *testing.T) {
	srv, _, know := newContentWeb(t, governance.RoleMember)

	if _, errMsg := getBody(t, srv, "/browse?kind=memory&status=archived&space="+string(spPers)); errMsg != "" {
		t.Fatal(errMsg)
	}
	in := know.lastList
	if in.Kind != knowledge.EntityMemory {
		t.Fatalf("类型筛选应生效，实际 %q", in.Kind)
	}
	if len(in.Statuses) != 1 || in.Statuses[0] != knowledge.MemoryArchived {
		t.Fatalf("状态筛选应生效，实际 %v", in.Statuses)
	}
	// 空间筛选传给领域层，由它确认这个空间在可读集合里——
	// 页面不能自己拿参数去查
	if in.SpaceID != spPers {
		t.Fatalf("空间筛选应传下去，实际 %q", in.SpaceID)
	}
}

func TestBrowseDetailWarnsAboutMissingTriggers(t *testing.T) {
	srv, _, _ := newContentWeb(t, governance.RoleMember)

	page := body(t, get(t, srv, "/browse/01CARDAAAAAAAAAAAAAAAAAAAA?kind=card", true))
	// 一张没有召回条件的卡永远不会被 card_recall 命中——
	// 那是这个产品的核心能力，页面必须说出来
	if !strings.Contains(page, "card_recall 永远命不中") {
		t.Fatal("没有召回条件的卡应当被显著标出")
	}
	if !strings.Contains(page, "docs/x.md") {
		t.Fatal("应标注来源——原创/提升自/摄取进来的，三者可信度不同")
	}
}

func getBody(t *testing.T, srv *httptest.Server, path string) (string, string) {
	t.Helper()
	resp := get(t, srv, path, true)
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		return "", "HTTP " + resp.Status
	}
	return body(t, resp), ""
}

// ---------------------------------------------------------------------------
// 活动流与完整审计
// ---------------------------------------------------------------------------

func TestActivityPassesOnlyReadableSharedSpaces(t *testing.T) {
	srv, _, _, act := newActivityWeb(t, governance.RoleMember)
	agentID := platform.ID("01AGENTAAAAAAAAAAAAAAAAAAA")
	act.visiblePage = activity.Page{Events: []activity.Event{{
		ID: "01AUDITAAAAAAAAAAAAAAAAAAA", TenantID: tenantA, UserID: userMe,
		AgentID: &agentID, Source: activity.SourceMCP, Kind: activity.KindTrace,
		Access: activity.AccessRead, Outcome: activity.OutcomeSuccess,
		Action: "card_recall", CreatedAt: time.Now().UTC(),
	}}}

	page := body(t, get(t, srv, "/activity", true))
	if !strings.Contains(page, "card_recall") {
		t.Fatal("活动页应渲染领域层返回的事件")
	}
	if act.visibleInput.TenantID != tenantA || act.visibleInput.UserID != userMe {
		t.Fatalf("可见性查询身份不对: %+v", act.visibleInput)
	}
	if len(act.visibleInput.ReadableSharedSpaceIDs) != 1 || act.visibleInput.ReadableSharedSpaceIDs[0] != spTeam {
		t.Fatalf("只应把可读的 team/project 交给活动域，实际 %v", act.visibleInput.ReadableSharedSpaceIDs)
	}
}

func TestAdminAuditRequiresConfirmationAndRecordsBeforeQuery(t *testing.T) {
	srv, _, _, act := newActivityWeb(t, governance.RoleAdmin)

	confirm := body(t, get(t, srv, "/admin/audit", true))
	if !strings.Contains(confirm, "查询动作本身会先写入") {
		t.Fatal("GET 只能展示二次确认说明")
	}
	if act.listAdminCalls != 0 || len(act.records) != 0 {
		t.Fatal("仅打开确认页不得读取完整日志或伪造 meta-audit")
	}

	resp := post(t, srv, "/admin/audit", url.Values{}, true, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("确认后的审计查询应成功，实际 %d", resp.StatusCode)
	}
	if len(act.order) < 2 || act.order[0] != "record" || act.order[1] != "admin" {
		t.Fatalf("必须先写 meta-audit 再查完整日志，实际顺序 %v", act.order)
	}
	if len(act.records) != 1 {
		t.Fatalf("期望一条 meta-audit，实际 %d", len(act.records))
	}
	meta := act.records[0]
	if meta.Kind != activity.KindMetaAudit || meta.Access != activity.AccessRead || meta.Action != "admin.audit.query" {
		t.Fatalf("meta-audit 形态不对: %+v", meta)
	}
}

func TestAdminAuditFailsClosedWhenMetaAuditCannotPersist(t *testing.T) {
	srv, _, _, act := newActivityWeb(t, governance.RoleAdmin)
	act.recordErr = errors.New("audit unavailable")

	resp := post(t, srv, "/admin/audit", url.Values{}, true, true)
	page := body(t, resp)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("meta-audit 写不下时应拒绝查询，实际 %d", resp.StatusCode)
	}
	if act.listAdminCalls != 0 {
		t.Fatal("meta-audit 写不下时不得先把完整日志读进内存")
	}
	if !strings.Contains(page, "已拒绝展示完整日志") {
		t.Fatalf("页面应解释 fail-closed，实际 %.200s", page)
	}
}

func TestAuthenticatedWebWritesAreAuditedWithoutSensitivePayloads(t *testing.T) {
	tests := []struct {
		name, path, action string
		form               url.Values
	}{
		{"logout", "/logout", "session.logout", url.Values{}},
		{"password", "/password", "password.change", url.Values{"current": {"current-password"}, "new": {"secret-password-123"}, "confirm": {"secret-password-123"}}},
		{"token issue", "/tokens", "token.issue", url.Values{"agent": {"secret-agent-slug"}, "name": {"Secret Device"}}},
		{"token revoke", "/tokens/revoke", "token.revoke", url.Values{"id": {"01TOKENAAAAAAAAAAAAAAAAAAA"}}},
		{"review", "/review/01CARDAAAAAAAAAAAAAAAAAAAA", "card.review", url.Values{"action": {"reject"}, "title": {"secret-card-title"}}},
		{"member create", "/admin/members", "member.create", url.Values{"username": {"secret-user"}, "display": {"Secret User"}, "role": {"member"}}},
		{"member status", "/admin/members/status", "member.status", url.Values{"id": {string(userOther)}, "status": {"disabled"}}},
		{"member reset", "/admin/members/reset", "member.password_reset", url.Values{"id": {string(userOther)}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, gov, _, act := newActivityWeb(t, governance.RoleAdmin)
			if tt.action == "token.revoke" {
				gov.tokens = []governance.Token{{ID: "01TOKENAAAAAAAAAAAAAAAAAAA"}}
			}
			resp := post(t, srv, tt.path, tt.form, true, true)
			defer resp.Body.Close()
			if len(act.records) != 1 {
				t.Fatalf("每个已认证写请求必须留一条记录，实际 %d", len(act.records))
			}
			if got := act.records[0].Action; got != tt.action {
				t.Fatalf("动作不对: want %q, got %q", tt.action, got)
			}
			encoded, err := json.Marshal(act.records[0])
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"secret-password-123", "secret-agent-slug", "Secret Device", "secret-card-title", "secret-user", "Secret User", "ccs_plaintext_token_value"} {
				if strings.Contains(string(encoded), secret) {
					t.Fatalf("审计记录泄露了表单或凭据 %q: %s", secret, encoded)
				}
			}
		})
	}
}

func TestWebWriteAuditCapturesRenderedFailureAndCreatedTargets(t *testing.T) {
	t.Run("rendered validation failure", func(t *testing.T) {
		srv, _, _, act := newActivityWeb(t, governance.RoleMember)
		resp := post(t, srv, "/password", url.Values{
			"current": {"current-password"},
			"new":     {"new-password-one"},
			"confirm": {"different-password"},
		}, true, true)
		defer resp.Body.Close()

		// Form validation deliberately re-renders with HTTP 200. The audit
		// outcome must still describe the failed action, not the render status.
		if len(act.records) != 1 || act.records[0].Outcome != activity.OutcomeError {
			t.Fatalf("rendered validation error must be audited as error: %+v", act.records)
		}
	})

	tests := []struct {
		name, path, entityType string
		form                   url.Values
		entityID               platform.ID
	}{
		{
			name: "issued token", path: "/tokens", entityType: "token",
			form:     url.Values{"agent": {"clara"}, "name": {"MacBook"}},
			entityID: "01TOKENAAAAAAAAAAAAAAAAAAA",
		},
		{
			name: "created member", path: "/admin/members", entityType: "user",
			form:     url.Values{"username": {"new-user"}, "display": {"New User"}, "role": {"member"}},
			entityID: "01NEWUSERAAAAAAAAAAAAAAAAA",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _, _, act := newActivityWeb(t, governance.RoleAdmin)
			resp := post(t, srv, tt.path, tt.form, true, true)
			defer resp.Body.Close()

			if len(act.records) != 1 {
				t.Fatalf("expected one audit record, got %d", len(act.records))
			}
			record := act.records[0]
			if record.EntityType != tt.entityType || record.EntityID == nil || *record.EntityID != tt.entityID {
				t.Fatalf("created target was not resolved from domain output: %+v", record)
			}
			if record.Outcome != activity.OutcomeSuccess {
				t.Fatalf("successful write outcome = %s", record.Outcome)
			}
		})
	}
}

func TestAuthenticatedCSRFRejectionIsAuditedAsFailure(t *testing.T) {
	srv, _, _, act := newActivityWeb(t, governance.RoleMember)
	resp := post(t, srv, "/tokens", url.Values{
		"agent": {"clara"}, "name": {"MacBook"},
	}, true, false)
	defer resp.Body.Close()

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("missing CSRF status = %d, want 403", resp.StatusCode)
	}
	if len(act.records) != 1 || act.records[0].Action != "token.issue" || act.records[0].Outcome != activity.OutcomeError {
		t.Fatalf("authenticated rejected write must leave a failed audit record: %+v", act.records)
	}
}

func TestOrdinaryAuditFailureDoesNotChangeCompletedWriteResult(t *testing.T) {
	srv, _, _, act := newActivityWeb(t, governance.RoleMember)
	act.recordErr = errors.New("audit unavailable")

	resp := post(t, srv, "/tokens", url.Values{"agent": {"clara"}, "name": {"MacBook"}}, true, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("业务写已完成后，审计失败不应把响应改成失败，实际 %d", resp.StatusCode)
	}
}

func TestReviewAuditCarriesResolvedSpaceForSharedVisibility(t *testing.T) {
	srv, _, know, act := newActivityWeb(t, governance.RoleMember)
	// The handler must use the domain result rather than guessing from the URL
	// or form. A real domain call only returns this card after write permission.
	know.card.SpaceID = spTeam

	resp := post(t, srv, "/review/01CARDAAAAAAAAAAAAAAAAAAAA", url.Values{
		"action": {"reject"}, "title": {"不需要的卡"},
	}, true, true)
	defer resp.Body.Close()
	if len(act.records) != 1 || act.records[0].SpaceID == nil || *act.records[0].SpaceID != spTeam {
		t.Fatalf("共享写入必须带领域层解析后的空间，实际 %+v", act.records)
	}
}
