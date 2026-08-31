package mcp

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"claracore-server/internal/domain/governance"
	"claracore-server/internal/platform"
	"claracore-server/internal/ports"
)

// fakeGov 是治理域的替身。
//
// Host 层的 bug（中间件顺序、身份传递、错误映射、编码）与存储层无关，
// 用真库测等于把两件事绑死：数据库连不上时，协议层的回归就测不了了。
type fakeGov struct {
	byToken  map[string]governance.Identity
	byUser   map[platform.ID][]governance.SpaceMembership
	scopeErr error

	authCalls int
}

func (f *fakeGov) Authenticate(_ context.Context, plaintext string) (governance.Identity, error) {
	f.authCalls++
	id, ok := f.byToken[plaintext]
	if !ok {
		return governance.Identity{}, governance.ErrTokenNotFound
	}
	return id, nil
}

func (f *fakeGov) Scope(_ context.Context, id governance.Identity) (governance.SpaceScope, error) {
	if f.scopeErr != nil {
		return governance.SpaceScope{}, f.scopeErr
	}
	// 用真实的 ResolveScope 构造 —— 权限规则不该在测试里被复刻一份
	return governance.ResolveScope(id.UserID, f.byUser[id.UserID]), nil
}

// --- 测试夹具 ---

const (
	tenantA  = "01TENANTAAAAAAAAAAAAAAAAAA"
	userClar = "01USERCLARAAAAAAAAAAAAAAAA"
	userMate = "01USERMATEAAAAAAAAAAAAAAAA"
	spTeam   = "01SPACETEAMAAAAAAAAAAAAAAA"
	spPers   = "01SPACEPERSONALAAAAAAAAAAA"
	spProj   = "01SPACEPROJECTAAAAAAAAAAAA"

	agentClara = "01AGENTCLARAAAAAAAAAAAAAAA"
)

func newTestHost(t *testing.T, options ...HostOptions) (*httptest.Server, *fakeGov) {
	t.Helper()

	owner := platform.ID(userClar)
	gov := &fakeGov{
		byToken: map[string]governance.Identity{
			"ccs_valid_clara": {
				TenantID: tenantA, UserID: userClar, AgentID: agentClara,
				TokenID:  "01TOKENAAAAAAAAAAAAAAAAAAA",
				Username: "zhouwei", DisplayName: "毛仔", Role: governance.RoleAdmin,
				AgentSlug: "clara", AgentName: "Clara",
			},
			// 同一个人的另一个 agent：验证 token 即身份，agent 不同则身份不同
			"ccs_valid_codex": {
				TenantID: tenantA, UserID: userClar, AgentID: "01AGENTCODEXAAAAAAAAAAAAAA",
				TokenID:  "01TOKENBBBBBBBBBBBBBBBBBBB",
				Username: "zhouwei", DisplayName: "毛仔", Role: governance.RoleAdmin,
				AgentSlug: "codex", AgentName: "Codex",
			},
			// 没有个人空间的用户：数据损坏场景
			"ccs_no_personal": {
				TenantID: tenantA, UserID: userMate, AgentID: "01AGENTOTHERAAAAAAAAAAAAAA",
				Username: "tongshi", DisplayName: "同事", Role: governance.RoleMember,
				AgentSlug: "clara", AgentName: "Clara",
			},
		},
		byUser: map[platform.ID][]governance.SpaceMembership{
			userClar: {
				{Space: governance.Space{ID: spTeam, TenantID: tenantA, Type: governance.SpaceTeam, Name: "团队"}},
				{Space: governance.Space{ID: spPers, TenantID: tenantA, Type: governance.SpacePersonal, Name: "毛仔", OwnerUserID: &owner}},
				{Space: governance.Space{ID: spProj, TenantID: tenantA, Type: governance.SpaceProject, Name: "项目", OwnerUserID: &owner}},
			},
			// userMate 有可读空间但没有个人空间
			userMate: {
				{Space: governance.Space{ID: spTeam, TenantID: tenantA, Type: governance.SpaceTeam, Name: "团队"}},
			},
		},
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	opts := HostOptions{Version: "test"}
	if len(options) > 0 {
		opts = options[0]
	}
	srv := httptest.NewServer(NewHost(gov, newFakeKnow(), newFakeContinuity(), newFakeActivity(), logger, opts).Handler())
	t.Cleanup(srv.Close)
	return srv, gov
}

func TestRequestBodyLimitRejectsOversizedJSON(t *testing.T) {
	srv, gov := newTestHost(t, HostOptions{Version: "test", MaxRequestBodyBytes: 128})
	payload := `{"jsonrpc":"2.0","id":1,"method":"server_status","padding":"` + strings.Repeat("x", 256) + `"}`
	req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(payload))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Authorization", "Bearer ccs_valid_clara")
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusRequestEntityTooLarge {
		t.Fatalf("status = %d, want %d", resp.StatusCode, http.StatusRequestEntityTooLarge)
	}
	if gov.authCalls != 1 {
		t.Fatalf("auth calls = %d, want 1", gov.authCalls)
	}
}

// newToolHost 与 newTestHost 同一套夹具，但把核心域的替身也交出来，
// 让工具层的测试能断言「Host 到底往领域层传了什么」——那才是这一层
// 唯一该被测的东西。
func newToolHost(t *testing.T) (*httptest.Server, *fakeKnow) {
	return newToolHostWithActivity(t, newFakeActivity())
}

func newToolHostWithActivity(t *testing.T, activities activityService) (*httptest.Server, *fakeKnow) {
	srv, know, _ := newToolHostWithActivityAndContinuity(t, activities)
	return srv, know
}

func newToolHostWithActivityAndContinuity(t *testing.T, activities activityService, options ...HostOptions) (*httptest.Server, *fakeKnow, *fakeContinuity) {
	t.Helper()
	owner := platform.ID(userClar)
	gov := &fakeGov{
		byToken: map[string]governance.Identity{
			"ccs_valid_clara": {
				TenantID: tenantA, UserID: userClar, AgentID: agentClara,
				TokenID:  "01TOKENAAAAAAAAAAAAAAAAAAA",
				Username: "zhouwei", DisplayName: "毛仔", Role: governance.RoleAdmin,
				AgentSlug: "clara", AgentName: "Clara",
			},
			// 只对团队空间有读权限、没有写权限，也没有个人空间
			"ccs_readonly": {
				TenantID: tenantA, UserID: userMate, AgentID: "01AGENTOTHERAAAAAAAAAAAAAA",
				Username: "tongshi", DisplayName: "同事", Role: governance.RoleMember,
				AgentSlug: "clara", AgentName: "Clara",
			},
		},
		byUser: map[platform.ID][]governance.SpaceMembership{
			userClar: {
				{Space: governance.Space{ID: spTeam, TenantID: tenantA, Type: governance.SpaceTeam, Name: "团队"}},
				{Space: governance.Space{ID: spPers, TenantID: tenantA, Type: governance.SpacePersonal, Name: "毛仔", OwnerUserID: &owner}},
				{Space: governance.Space{ID: spProj, TenantID: tenantA, Type: governance.SpaceProject, Name: "项目", OwnerUserID: &owner}},
			},
			userMate: {
				{Space: governance.Space{ID: spTeam, TenantID: tenantA, Type: governance.SpaceTeam, Name: "团队"}},
			},
		},
	}
	know := newFakeKnow()
	cont := newFakeContinuity()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	opts := HostOptions{Version: "test", SemanticSearch: true}
	if len(options) > 0 {
		opts = options[0]
	}
	srv := httptest.NewServer(NewHost(gov, know, cont, activities, logger, opts).Handler())
	t.Cleanup(srv.Close)
	return srv, know, cont
}

// --- JSON-RPC 客户端 ---

type rpcResponse struct {
	status int
	body   map[string]any
	header http.Header
	// raw 是未经解析的响应体。认证失败走 SDK 的 http.Error，返回的是纯文本
	// 而非 JSON——只比较解析后的 body 会让「响应体是否一致」的断言恒真。
	raw string
}

// call 发一次 MCP 请求。stateless 下没有 initialize 握手，直接调即可。
func call(t *testing.T, srv *httptest.Server, token, method string, params any, headers ...string) rpcResponse {
	t.Helper()

	payload := map[string]any{"jsonrpc": "2.0", "id": 1, "method": method}
	if params != nil {
		payload["params"] = params
	}
	buf, err := json.Marshal(payload)
	if err != nil {
		t.Fatal(err)
	}

	req, err := http.NewRequest(http.MethodPost, srv.URL, bytes.NewReader(buf))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	if token != "" {
		req.Header.Set("Authorization", "Bearer "+token)
	}
	for i := 0; i+1 < len(headers); i += 2 {
		req.Header.Set(headers[i], headers[i+1])
	}

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()

	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return rpcResponse{status: resp.StatusCode, body: parseBody(t, raw), raw: string(raw), header: resp.Header.Clone()}
}

// parseBody 兼容两种响应体：纯 JSON 与 SSE（stateless 下 POST 的响应
// 仍是 text/event-stream，这也是 nginx 仍需 proxy_buffering off 的原因）。
func parseBody(t *testing.T, raw []byte) map[string]any {
	t.Helper()
	if len(bytes.TrimSpace(raw)) == 0 {
		return nil
	}

	var out map[string]any
	if json.Unmarshal(raw, &out) == nil {
		return out
	}

	sc := bufio.NewScanner(bytes.NewReader(raw))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		line := sc.Text()
		data, ok := strings.CutPrefix(line, "data:")
		if !ok {
			continue
		}
		if err := json.Unmarshal([]byte(strings.TrimSpace(data)), &out); err == nil {
			return out
		}
	}
	return nil
}

// whoami 调用工具并返回解析后的结构化输出。
func whoami(t *testing.T, srv *httptest.Server, token string, headers ...string) (whoamiOutput, rpcResponse) {
	t.Helper()
	resp := call(t, srv, token, "tools/call",
		map[string]any{"name": "whoami", "arguments": map[string]any{}}, headers...)

	var out whoamiOutput
	result, ok := resp.body["result"].(map[string]any)
	if !ok {
		return out, resp
	}
	structured, ok := result["structuredContent"]
	if !ok {
		return out, resp
	}
	buf, err := json.Marshal(structured)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(buf, &out); err != nil {
		t.Fatal(err)
	}
	return out, resp
}

// --- 测试 ---

func TestWhoAmIReturnsIdentityAndSpaces(t *testing.T) {
	srv, _ := newTestHost(t)

	out, resp := whoami(t, srv, "ccs_valid_clara")
	if resp.status != http.StatusOK {
		t.Fatalf("期望 200，实际 %d：%v", resp.status, resp.body)
	}
	if out.TenantID != tenantA || out.UserID != userClar {
		t.Errorf("身份不对: %+v", out)
	}
	if out.Username != "zhouwei" || out.DisplayName != "毛仔" || out.Role != governance.RoleAdmin {
		t.Errorf("用户信息不对: %+v", out)
	}
	if out.AgentSlug != "clara" || out.AgentName != "Clara" {
		t.Errorf("agent 信息不对: %+v", out)
	}

	// 空间顺序稳定：team > project > personal（governance.sortAccesses）。
	// 顺序抖动会让 agent 的选择跟着抖。
	if len(out.Spaces) != 3 {
		t.Fatalf("期望 3 个空间，实际 %d: %+v", len(out.Spaces), out.Spaces)
	}
	wantOrder := []string{governance.SpaceTeam, governance.SpaceProject, governance.SpacePersonal}
	for i, want := range wantOrder {
		if out.Spaces[i].Type != want {
			t.Errorf("第 %d 个空间应为 %s，实际 %s", i, want, out.Spaces[i].Type)
		}
	}

	// 权限位来自领域规则，Host 不得自行判断
	byID := map[string]spaceInfo{}
	for _, s := range out.Spaces {
		byID[s.ID] = s
	}
	if s := byID[spTeam]; !s.CanRead || s.CanWrite {
		t.Errorf("团队空间应可读不可写（无显式授权）: %+v", s)
	}
	if s := byID[spPers]; !s.CanRead || !s.CanWrite {
		t.Errorf("个人空间应可读写: %+v", s)
	}
	if s := byID[spProj]; !s.CanRead || !s.CanWrite {
		t.Errorf("自建项目空间应可读写: %+v", s)
	}

	// 默认写入目标必须是个人空间——没有它，「不传 space 写哪」
	// 就是一条 agent 无从验证的口头约定
	if out.DefaultSpaceID != spPers {
		t.Errorf("defaultSpaceId 应为个人空间 %s，实际 %q", spPers, out.DefaultSpaceID)
	}
}

func TestMCPResponsesAreNeverCacheable(t *testing.T) {
	srv, _ := newTestHost(t)
	for _, token := range []string{"ccs_valid_clara", "ccs_invalid"} {
		resp := call(t, srv, token, "tools/list", nil)
		for name, want := range map[string]string{
			"Cache-Control":          "no-store, no-transform",
			"Pragma":                 "no-cache",
			"X-Content-Type-Options": "nosniff",
			"X-Robots-Tag":           "noindex, nofollow, noarchive",
		} {
			if got := resp.header.Get(name); got != want {
				t.Errorf("token=%q %s = %q, want %q", token, name, got, want)
			}
		}
	}
}

type recordingMCPLimiter struct {
	results []ports.RateLimitResult
	keys    []string
	limits  []int
	windows []time.Duration
}

func (l *recordingMCPLimiter) Allow(_ context.Context, key string, limit int, window time.Duration) (ports.RateLimitResult, error) {
	l.keys = append(l.keys, key)
	l.limits = append(l.limits, limit)
	l.windows = append(l.windows, window)
	if len(l.results) == 0 {
		return ports.RateLimitResult{Allowed: true}, nil
	}
	result := l.results[0]
	l.results = l.results[1:]
	return result, nil
}

func TestMCPRateLimitUsesAuthenticatedAgentWithoutLeakingIdentity(t *testing.T) {
	limiter := &recordingMCPLimiter{results: []ports.RateLimitResult{
		{Allowed: true},
		{Allowed: true},
		{Allowed: true},
		{Allowed: true},
		{Allowed: true},
		{Allowed: false, RetryAfter: 1500 * time.Millisecond},
		{Allowed: true},
	}}
	srv, gov := newTestHost(t, HostOptions{
		Version: "test", RequestLimiter: limiter, RequestRateLimit: 42, TenantRequestRateLimit: 420, RequestRateWindow: 2 * time.Minute,
	})
	// A second token for the same agent must share the first token's budget.
	gov.byToken["ccs_same_agent"] = gov.byToken["ccs_valid_clara"]

	if first := call(t, srv, "ccs_valid_clara", "tools/list", nil); first.status != http.StatusOK {
		t.Fatalf("first request status = %d, want 200", first.status)
	}
	blocked := call(t, srv, "ccs_same_agent", "tools/list", nil)
	if blocked.status != http.StatusTooManyRequests || blocked.header.Get("Retry-After") != "2" {
		t.Fatalf("blocked response = %d retry=%q, want 429 retry=2", blocked.status, blocked.header.Get("Retry-After"))
	}
	if got := blocked.header.Get("Cache-Control"); got != "no-store, no-transform" {
		t.Fatalf("rate-limit response cache policy = %q", got)
	}
	// Authentication failures count only against the bounded source bucket;
	// random token values never create persistent keys.
	if invalid := call(t, srv, "ccs_invalid", "tools/list", nil); invalid.status != http.StatusUnauthorized {
		t.Fatalf("invalid token status = %d, want 401", invalid.status)
	}
	if len(limiter.keys) != 7 {
		t.Fatalf("limiter calls = %d, want source+agent+tenant, source+agent+tenant, source: %v", len(limiter.keys), limiter.keys)
	}
	if limiter.keys[0] != limiter.keys[3] || limiter.keys[0] != limiter.keys[6] {
		t.Fatalf("same network source must share one key: %v", limiter.keys)
	}
	if limiter.keys[1] != limiter.keys[4] {
		t.Fatalf("same agent must share one key: %v", limiter.keys)
	}
	if limiter.keys[2] != limiter.keys[5] {
		t.Fatalf("same tenant must share one aggregate key: %v", limiter.keys)
	}
	if limiter.limits[0] != 42*6 || limiter.limits[1] != 42 || limiter.limits[2] != 420 || limiter.windows[0] != 2*time.Minute {
		t.Fatalf("limiter configuration not preserved: limits=%v windows=%v", limiter.limits, limiter.windows)
	}
	for _, secret := range []string{"ccs_valid_clara", "ccs_same_agent", agentClara, userClar, tenantA} {
		if strings.Contains(strings.Join(limiter.keys, "\n"), secret) {
			t.Fatalf("persistent limiter key leaks identity %q: %v", secret, limiter.keys)
		}
	}
	if !strings.HasPrefix(limiter.keys[0], "rate:mcp:source:") || !strings.HasPrefix(limiter.keys[1], "rate:mcp:agent:") || !strings.HasPrefix(limiter.keys[2], "rate:mcp:tenant:") {
		t.Fatalf("limiter keys lack explicit namespaces: %v", limiter.keys)
	}
}

func TestMCPTenantRateLimitCannotBeBypassedWithAnotherAgent(t *testing.T) {
	limiter := &recordingMCPLimiter{results: []ports.RateLimitResult{
		{Allowed: true}, {Allowed: true}, {Allowed: true},
		{Allowed: true}, {Allowed: true}, {Allowed: false, RetryAfter: time.Second},
	}}
	srv, _ := newTestHost(t, HostOptions{
		Version: "test", RequestLimiter: limiter, RequestRateLimit: 42, TenantRequestRateLimit: 420, RequestRateWindow: time.Minute,
	})
	if first := call(t, srv, "ccs_valid_clara", "tools/list", nil); first.status != http.StatusOK {
		t.Fatalf("first agent status = %d, want 200", first.status)
	}
	blocked := call(t, srv, "ccs_valid_codex", "tools/list", nil)
	if blocked.status != http.StatusTooManyRequests || blocked.header.Get("Retry-After") != "1" {
		t.Fatalf("second agent must share tenant budget: status=%d retry=%q", blocked.status, blocked.header.Get("Retry-After"))
	}
	if limiter.keys[1] == limiter.keys[4] || limiter.keys[2] != limiter.keys[5] {
		t.Fatalf("different agents must have distinct agent keys and one tenant key: %v", limiter.keys)
	}
}

func TestMCPAgentDenialStopsBeforeConsumingTenantBudget(t *testing.T) {
	limiter := &recordingMCPLimiter{results: []ports.RateLimitResult{
		{Allowed: true},
		{Allowed: false, RetryAfter: time.Second},
	}}
	srv, _ := newTestHost(t, HostOptions{
		Version: "test", RequestLimiter: limiter, RequestRateLimit: 42, TenantRequestRateLimit: 420, RequestRateWindow: time.Minute,
	})
	blocked := call(t, srv, "ccs_valid_clara", "tools/list", nil)
	if blocked.status != http.StatusTooManyRequests {
		t.Fatalf("agent denial status = %d, want 429", blocked.status)
	}
	if len(limiter.keys) != 2 || !strings.HasPrefix(limiter.keys[1], "rate:mcp:agent:") {
		t.Fatalf("tenant budget must not be consumed after agent denial: %v", limiter.keys)
	}
}

func TestMCPSourceLimitDoesNotTrustForwardedIPFromDirectPeer(t *testing.T) {
	trusted := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	trusted.RemoteAddr = "127.0.0.1:12345"
	trusted.Header.Set("X-Real-IP", "203.0.113.10")
	if got := mcpClientIP(trusted); got != "203.0.113.10" {
		t.Fatalf("loopback proxy source = %q", got)
	}

	direct := httptest.NewRequest(http.MethodPost, "/mcp", nil)
	direct.RemoteAddr = "198.51.100.8:54321"
	direct.Header.Set("X-Real-IP", "203.0.113.10")
	if got := mcpClientIP(direct); got != "198.51.100.8" {
		t.Fatalf("direct peer must ignore forwarded header, got %q", got)
	}
}

var _ ports.RateLimiter = (*recordingMCPLimiter)(nil)

// TestIdentityComesOnlyFromToken 是铁律 4 的可执行形式。
func TestIdentityComesOnlyFromToken(t *testing.T) {
	srv, _ := newTestHost(t)

	// 伪造 Desktop 那套 header，外加一个直接冒名的 userId
	out, resp := whoami(t, srv, "ccs_valid_clara",
		"X-ClaraCore-Agent-ID", "codex",
		"X-ClaraCore-User-ID", userMate,
		"X-Forwarded-User", "someone-else")
	if resp.status != http.StatusOK {
		t.Fatalf("期望 200，实际 %d", resp.status)
	}
	if out.AgentSlug != "clara" {
		t.Errorf("客户端声明的 header 改变了 agent 身份：%q", out.AgentSlug)
	}
	if out.UserID != userClar {
		t.Errorf("客户端声明的 header 改变了用户身份：%q", out.UserID)
	}

	// 同一用户的两个 agent：身份必须跟着 token 走
	other, _ := whoami(t, srv, "ccs_valid_codex")
	if other.AgentSlug != "codex" || other.AgentID == out.AgentID {
		t.Errorf("换 token 后 agent 身份没变: %+v", other)
	}
	if other.UserID != out.UserID {
		t.Errorf("同一用户的两个 agent 应属于同一 user: %q vs %q", other.UserID, out.UserID)
	}
}

// TestAuthFailuresAreIndistinguishable 保证三种 token 失败对外完全一致。
//
// 区分「不存在」「已吊销」「已过期」等于告诉攻击者哪些 token 曾经有效。
func TestAuthFailuresAreIndistinguishable(t *testing.T) {
	reasons := []error{
		governance.ErrTokenNotFound,
		governance.ErrTokenRevoked,
		governance.ErrTokenExpired,
		governance.ErrTokenMalformed,
		governance.ErrUserDisabled,
		governance.ErrTenantInactive,
	}

	var seen []string
	for _, reason := range reasons {
		failing := &failingGov{err: reason}
		logger := slog.New(slog.NewTextHandler(io.Discard, nil))
		s := httptest.NewServer(NewHost(failing, newFakeKnow(), newFakeContinuity(), newFakeActivity(), logger, HostOptions{Version: "test"}).Handler())

		resp := call(t, s, "ccs_whatever", "tools/call",
			map[string]any{"name": "whoami", "arguments": map[string]any{}})
		s.Close()

		if resp.status != http.StatusUnauthorized {
			t.Fatalf("%v 应返回 401，实际 %d", reason, resp.status)
		}
		// 比原始响应体：SDK 把 verifier 的错误原文写进 401 body
		seen = append(seen, resp.raw)
	}

	for i := 1; i < len(seen); i++ {
		if seen[i] != seen[0] {
			t.Errorf("不同失败原因的响应体不一致，泄露了 token 状态：\n  %q\n  %q", seen[0], seen[i])
		}
	}
	// 响应体里不得出现领域错误的任何痕迹
	for _, forbidden := range []string{"governance", "吊销", "过期", "停用", "租户"} {
		if strings.Contains(seen[0], forbidden) {
			t.Errorf("401 响应体泄露了失败原因（含 %q）：%q", forbidden, seen[0])
		}
	}
}

func TestMissingTokenIsRejected(t *testing.T) {
	srv, gov := newTestHost(t)

	resp := call(t, srv, "", "tools/call",
		map[string]any{"name": "whoami", "arguments": map[string]any{}})
	if resp.status != http.StatusUnauthorized {
		t.Fatalf("无 token 应返回 401，实际 %d", resp.status)
	}
	// 没有 token 时不该白跑一次认证
	if gov.authCalls != 0 {
		t.Errorf("无 token 却调用了 %d 次 Authenticate", gov.authCalls)
	}

	// 401 不带 WWW-Authenticate，这是**已知且有意接受**的偏差，不是漏了。
	// SDK 只在配置了 ResourceMetadataURL 或 Scopes 时才发这个头（auth/auth.go
	// RequireBearerToken），而我们不走 OAuth 发现流程（RFC 9728）——token 自管。
	// RFC 6750 建议 401 带上它，但眼下没有任何消费者会读，为它包一层
	// ResponseWriter 属于为未知变化留接口。将来真接 OAuth，配上
	// ResourceMetadataURL 这个头会自动出现。
	//
	// 这条断言锁住现状：哪天它变了，说明有人动了认证配置，应当是有意为之。
	if got := respHeaderProbe(t, srv); got != "" {
		t.Errorf("WWW-Authenticate 头出现了（%q）——若是有意接入 OAuth，请一并更新本注释", got)
	}
}

// TestWhoAmISurvivesMissingPersonalSpace：个人空间缺失是数据损坏，
// 但此时最该做的是让人看见它，而不是让 whoami 整个失败。
func TestWhoAmISurvivesMissingPersonalSpace(t *testing.T) {
	srv, _ := newTestHost(t)

	out, resp := whoami(t, srv, "ccs_no_personal")
	if resp.status != http.StatusOK {
		t.Fatalf("期望 200，实际 %d：%v", resp.status, resp.body)
	}
	if out.UserID != userMate {
		t.Fatalf("身份不对: %+v", out)
	}
	if out.DefaultSpaceID != "" {
		t.Errorf("没有个人空间时 defaultSpaceId 应为空，实际 %q", out.DefaultSpaceID)
	}
	if len(out.Spaces) != 1 || out.Spaces[0].Type != governance.SpaceTeam {
		t.Errorf("仍应返回其余可读空间: %+v", out.Spaces)
	}
}

// TestToolErrorsDoNotLeakInternals：Scope 失败时不能把内部错误原文吐给调用方。
func TestToolErrorsDoNotLeakInternals(t *testing.T) {
	srv, gov := newTestHost(t)
	gov.scopeErr = errInternalProbe

	_, resp := whoami(t, srv, "ccs_valid_clara")
	body := string(mustJSON(t, resp.body))

	if strings.Contains(body, "ccs_internal_secret") {
		t.Errorf("内部错误原文泄露到响应里：%s", body)
	}
	if !strings.Contains(body, codeInternal) {
		t.Errorf("期望响应带 %s 错误码，实际：%s", codeInternal, body)
	}
}

// TestStatelessRejectsGetAndDelete 锁住 docs/06-poc-findings.md §4.1 的结论：
// stateless 下没有 SSE 长连接，nginx 因此不需要 proxy_read_timeout 3600s。
// 哪天这个行为变了，nginx 配置也得跟着改——所以它值得一条测试。
func TestStatelessRejectsGetAndDelete(t *testing.T) {
	srv, _ := newTestHost(t)

	for _, method := range []string{http.MethodGet, http.MethodDelete} {
		req, err := http.NewRequest(method, srv.URL, nil)
		if err != nil {
			t.Fatal(err)
		}
		req.Header.Set("Authorization", "Bearer ccs_valid_clara")
		req.Header.Set("Accept", "text/event-stream")
		resp, err := srv.Client().Do(req)
		if err != nil {
			t.Fatal(err)
		}
		resp.Body.Close()
		if resp.StatusCode != http.StatusMethodNotAllowed {
			t.Errorf("%s 期望 405（stateless 无长连接），实际 %d", method, resp.StatusCode)
		}
	}
}

func TestWhoAmIIsListedWithoutIdentityParams(t *testing.T) {
	srv, _ := newTestHost(t)

	resp := call(t, srv, "ccs_valid_clara", "tools/list", map[string]any{})

	result, _ := resp.body["result"].(map[string]any)
	tools, _ := result["tools"].([]any)
	if len(tools) == 0 {
		t.Fatalf("工具列表为空：%s", mustJSON(t, resp.body))
	}

	found := false
	for _, raw := range tools {
		tool, _ := raw.(map[string]any)
		if tool["name"] != "whoami" {
			continue
		}
		found = true

		// 关键区分：身份可以**出现在返回值里**，绝不能出现在**入参里**
		// （docs/03-mcp-contract.md §3.3，参数化的身份等同于可伪造的身份）。
		in := string(mustJSON(t, tool["inputSchema"]))
		for _, forbidden := range []string{"tenantId", "userId", "agentId", "space"} {
			if strings.Contains(in, forbidden) {
				t.Errorf("whoami 的入参 schema 里出现了 %q：%s", forbidden, in)
			}
		}

		// 反面确认：身份确实在输出里。否则上面那条断言可能只是因为
		// schema 生成失败而空过。
		out := string(mustJSON(t, tool["outputSchema"]))
		for _, want := range []string{"tenantId", "userId", "agentId", "defaultSpaceId"} {
			if !strings.Contains(out, want) {
				t.Errorf("whoami 的输出 schema 缺少 %q：%s", want, out)
			}
		}
	}
	if !found {
		t.Fatal("工具列表里没有 whoami")
	}
}

// --- 辅助 ---

type failingGov struct{ err error }

func (f *failingGov) Authenticate(context.Context, string) (governance.Identity, error) {
	return governance.Identity{}, f.err
}

func (f *failingGov) Scope(context.Context, governance.Identity) (governance.SpaceScope, error) {
	return governance.SpaceScope{}, f.err
}

var errInternalProbe = &probeError{}

type probeError struct{}

func (*probeError) Error() string {
	return "dial tcp 192.168.3.200:3316: ccs_internal_secret"
}

func mustJSON(t *testing.T, v any) []byte {
	t.Helper()
	buf, err := json.Marshal(v)
	if err != nil {
		t.Fatal(err)
	}
	return buf
}

// respHeaderProbe 单独发一次无 token 请求，取回 WWW-Authenticate 头。
func respHeaderProbe(t *testing.T, srv *httptest.Server) string {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL, strings.NewReader(`{"jsonrpc":"2.0","id":1,"method":"tools/list"}`))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	return resp.Header.Get("WWW-Authenticate")
}
