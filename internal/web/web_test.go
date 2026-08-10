package web

import (
	"context"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

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

func (f *fakeGov) Scope(_ context.Context, _ governance.Identity) (governance.SpaceScope, error) {
	owner := userMe
	return governance.ResolveScope(userMe, []governance.SpaceMembership{
		{Space: governance.Space{ID: spPers, TenantID: tenantA, Type: governance.SpacePersonal, Name: "毛仔", OwnerUserID: &owner}},
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

type fakeKnow struct{}

func (fakeKnow) SpaceStats(_ context.Context, _ platform.ID, spaces []platform.ID) ([]knowledge.SpaceStats, error) {
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
	srv := httptest.NewServer(NewHost(gov, fakeKnow{}, logger, Options{
		Version: "test", PublicMCPURL: "https://ccs.example.com/mcp",
	}).Handler())
	t.Cleanup(srv.Close)
	return srv, gov
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

	for _, path := range []string{"/", "/tokens", "/password", "/admin/members"} {
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

	resp := get(t, srv, "/admin/members", true)
	defer resp.Body.Close()
	// 404 而不是 403：不确认「这个管理页存在」
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("普通成员访问管理页应得到 404，实际 %d", resp.StatusCode)
	}
	if gov.calls != 0 {
		t.Fatal("权限没通过时不该调领域层")
	}
}

func TestMemberCannotPostToAdminEndpoints(t *testing.T) {
	srv, gov := newTestWeb(t, governance.RoleMember, false)

	for _, path := range []string{"/admin/members", "/admin/members/status", "/admin/members/reset"} {
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

	for _, path := range []string{"/", "/tokens", "/admin/members"} {
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
		"/logout":               {},
		"/password":             {"current": {"x"}, "new": {"yyyyyyyyyy"}, "confirm": {"yyyyyyyyyy"}},
		"/tokens":               {"agent": {"clara"}, "name": {"MacBook"}},
		"/tokens/revoke":        {"id": {"01TOKENAAAAAAAAAAAAAAAAAAA"}},
		"/admin/members":        {"username": {"newbie"}},
		"/admin/members/status": {"id": {string(userOther)}, "status": {"disabled"}},
		"/admin/members/reset":  {"id": {string(userOther)}},
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
