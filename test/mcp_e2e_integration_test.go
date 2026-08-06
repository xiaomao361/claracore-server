//go:build integration

// 端到端：真实 MySQL + 真实领域服务 + 真实 MCP Host。
//
// 与 internal/mcp 下的单测分工：那边用 fake 治理服务测协议层行为（快、无依赖），
// 这边测**装配是否正确**——同样一套代码接上真库、真 token 之后还成不成立。
// 两者都需要：fake 测不出「Store 的 SQL 写错了」，真库测不出「中间件顺序反了」。
//
// 放在 test/ 而不是 internal/mcp/：Host 不得 import adapters（架构约束），
// 只有完整装配的集成测试才允许把两端接起来。
package test

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

	"claracore-server/internal/adapters/mysql"
	"claracore-server/internal/domain/governance"
	mcphost "claracore-server/internal/mcp"
	"claracore-server/internal/platform"
)

func TestMCPWhoAmIEndToEnd(t *testing.T) {
	db := openDB(t)
	ctx := context.Background()

	clock := platform.SystemClock{}
	ids := platform.NewIDGenerator(clock)
	store := mysql.NewGovernanceStore(db)
	svc := governance.NewService(store, clock, ids)

	// 开一个租户：租户 + 团队空间 + 管理员 + 管理员的个人空间，四件事原子完成
	provisioned, err := svc.ProvisionTenant(ctx, governance.ProvisionTenantInput{
		TenantName:    "端到端测试租户",
		TeamSpaceName: "端到端团队",
		AdminUsername: "e2e-admin",
		AdminDisplay:  "端到端管理员",
		PasswordHash:  "$argon2id$e2e$fake", // 领域层不做哈希，Web 登录实现时才有真实值
	})
	if err != nil {
		t.Fatalf("开租户失败: %v", err)
	}
	tenantID := provisioned.Tenant.ID
	t.Cleanup(func() {
		for _, stmt := range []string{
			`DELETE FROM tokens WHERE tenant_id = ?`,
			`DELETE FROM agents WHERE tenant_id = ?`,
			`DELETE FROM space_permissions WHERE space_id IN (SELECT id FROM spaces WHERE tenant_id = ?)`,
			`DELETE FROM spaces WHERE tenant_id = ?`,
			`DELETE FROM users WHERE tenant_id = ?`,
			`DELETE FROM tenants WHERE id = ?`,
		} {
			if _, err := db.ExecContext(context.Background(), stmt, string(tenantID)); err != nil {
				t.Errorf("清理失败（需手工处理 tenant %s）: %v", tenantID, err)
			}
		}
	})

	agent, err := svc.CreateAgent(ctx, tenantID, provisioned.Admin.ID, "clara", "Clara")
	if err != nil {
		t.Fatalf("建 agent 失败: %v", err)
	}

	plaintext, token, err := svc.IssueToken(ctx, governance.IssueTokenInput{
		TenantID: tenantID, UserID: provisioned.Admin.ID, AgentID: agent.ID,
		Name: "端到端", TTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("签发 token 失败: %v", err)
	}
	if plaintext == "" {
		t.Fatal("明文 token 为空")
	}

	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(mcphost.NewHost(svc, logger, "e2e").Handler())
	t.Cleanup(srv.Close)

	t.Run("真实 token 换到真实身份与空间", func(t *testing.T) {
		out := callWhoAmI(t, srv, plaintext)

		if out.TenantID != string(tenantID) || out.UserID != string(provisioned.Admin.ID) {
			t.Errorf("身份不对: %+v", out)
		}
		if out.Username != "e2e-admin" || out.Role != governance.RoleAdmin {
			t.Errorf("用户信息不对: %+v", out)
		}
		if out.AgentSlug != "clara" || out.AgentID != string(agent.ID) {
			t.Errorf("agent 信息不对: %+v", out)
		}

		// 管理员能看到团队空间和自己的个人空间
		if len(out.Spaces) != 2 {
			t.Fatalf("期望 2 个空间（团队 + 个人），实际 %d: %+v", len(out.Spaces), out.Spaces)
		}
		if out.Spaces[0].Type != governance.SpaceTeam {
			t.Errorf("第一个应是团队空间（排序 team > project > personal）: %+v", out.Spaces[0])
		}
		// 团队空间隐含可读，写需显式授权——刚开的租户还没授权
		if !out.Spaces[0].CanRead || out.Spaces[0].CanWrite {
			t.Errorf("团队空间应可读不可写: %+v", out.Spaces[0])
		}
		if out.DefaultSpaceID != string(provisioned.Personal.ID) {
			t.Errorf("defaultSpaceId 应为管理员的个人空间 %s，实际 %q",
				provisioned.Personal.ID, out.DefaultSpaceID)
		}
	})

	t.Run("last_used_at 被真实写入", func(t *testing.T) {
		// 上一个子测试已经用过这个 token，认证路径应当已经 touch 过
		tokens, err := store.ListTokens(ctx, tenantID, provisioned.Admin.ID)
		if err != nil {
			t.Fatal(err)
		}
		var found *governance.Token
		for i := range tokens {
			if tokens[i].ID == token.ID {
				found = &tokens[i]
			}
		}
		if found == nil {
			t.Fatal("签发的 token 不在列表里")
		}
		if found.LastUsedAt == nil {
			t.Error("调用过之后 last_used_at 仍为空——认证路径没有 touch")
		}
	})

	t.Run("吊销后立刻失效", func(t *testing.T) {
		if err := svc.RevokeToken(ctx, tenantID, token.ID); err != nil {
			t.Fatalf("吊销失败: %v", err)
		}
		status, body := rawCall(t, srv, plaintext,
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"whoami","arguments":{}}}`)
		if status != http.StatusUnauthorized {
			t.Fatalf("吊销后应返回 401，实际 %d：%s", status, body)
		}
		// 响应体不得透露「这个 token 曾经有效」
		for _, forbidden := range []string{"revoked", "吊销", "governance"} {
			if strings.Contains(body, forbidden) {
				t.Errorf("401 响应泄露了吊销状态（含 %q）：%q", forbidden, body)
			}
		}
	})

	t.Run("伪造 token 同样是 401 且响应一致", func(t *testing.T) {
		revokedStatus, revokedBody := rawCall(t, srv, plaintext,
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"whoami","arguments":{}}}`)
		bogusStatus, bogusBody := rawCall(t, srv, "ccs_completely_made_up",
			`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"whoami","arguments":{}}}`)

		if revokedStatus != bogusStatus || revokedBody != bogusBody {
			t.Errorf("已吊销与不存在的 token 响应不一致，泄露了 token 状态：\n  %d %q\n  %d %q",
				revokedStatus, revokedBody, bogusStatus, bogusBody)
		}
	})
}

// --- 辅助 ---

type e2eWhoAmI struct {
	TenantID       string `json:"tenantId"`
	UserID         string `json:"userId"`
	Username       string `json:"username"`
	Role           string `json:"role"`
	AgentID        string `json:"agentId"`
	AgentSlug      string `json:"agentSlug"`
	DefaultSpaceID string `json:"defaultSpaceId"`
	Spaces         []struct {
		ID       string `json:"id"`
		Type     string `json:"type"`
		Name     string `json:"name"`
		CanRead  bool   `json:"canRead"`
		CanWrite bool   `json:"canWrite"`
	} `json:"spaces"`
}

func callWhoAmI(t *testing.T, srv *httptest.Server, token string) e2eWhoAmI {
	t.Helper()
	status, body := rawCall(t, srv, token,
		`{"jsonrpc":"2.0","id":1,"method":"tools/call","params":{"name":"whoami","arguments":{}}}`)
	if status != http.StatusOK {
		t.Fatalf("whoami 期望 200，实际 %d：%s", status, body)
	}

	var envelope struct {
		Result struct {
			StructuredContent e2eWhoAmI `json:"structuredContent"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(extractJSON(body)), &envelope); err != nil {
		t.Fatalf("解析响应失败: %v\n%s", err, body)
	}
	return envelope.Result.StructuredContent
}

func rawCall(t *testing.T, srv *httptest.Server, token, payload string) (int, string) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, srv.URL, bytes.NewReader([]byte(payload)))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	req.Header.Set("Authorization", "Bearer "+token)

	resp, err := srv.Client().Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return resp.StatusCode, string(raw)
}

// extractJSON 取出 SSE 响应里的 data 负载（stateless 下 POST 响应仍是 event-stream）。
func extractJSON(body string) string {
	if strings.HasPrefix(strings.TrimSpace(body), "{") {
		return body
	}
	sc := bufio.NewScanner(strings.NewReader(body))
	sc.Buffer(make([]byte, 0, 64*1024), 1<<20)
	for sc.Scan() {
		if data, ok := strings.CutPrefix(sc.Text(), "data:"); ok {
			return strings.TrimSpace(data)
		}
	}
	return body
}
