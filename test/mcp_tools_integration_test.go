//go:build integration

// 全栈端到端：真 token → 真 MCP 协议 → 真领域服务 → 真 MySQL → 真 embedding。
//
// 与另外两处的分工：
//   - internal/mcp 的单测：协议层行为，用 fake 领域服务，快且无依赖
//   - recall_e2e_integration_test.go：检索质量，绕过 MCP 直接调领域层
//   - **本文件**：把整条链路串起来，验证「一个 agent 通过 MCP 真的能
//     记下一条经验，并在对的时机被召回」——那是这个产品的一句话价值。
//
// 这条链路上任何一环装错（中间件顺序、权限解析、空间标注、索引 worker
// 没接上）都会在这里现形，而它们各自的单测都可能全绿。
package test

import (
	"context"
	"encoding/json"
	"io"
	"log/slog"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"claracore-server/internal/adapters/crypto"
	"claracore-server/internal/adapters/embedding"
	"claracore-server/internal/adapters/mysql"
	"claracore-server/internal/config"
	"claracore-server/internal/domain/activity"
	"claracore-server/internal/domain/governance"
	"claracore-server/internal/domain/knowledge"
	mcphost "claracore-server/internal/mcp"
	"claracore-server/internal/platform"
	"claracore-server/internal/ports"
)

type toolEnv struct {
	srv   *httptest.Server
	token string
	know  *knowledge.Service

	tenantID    platform.ID
	personalID  platform.ID
	teamID      platform.ID
	hasEmbedder bool
}

func newToolEnv(t *testing.T) toolEnv {
	t.Helper()
	db := openDB(t)
	ctx := context.Background()

	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Join(wd, ".."))
	cfg, err := config.Load()
	if err != nil {
		t.Skipf("配置不可用: %v", err)
	}

	clock := platform.SystemClock{}
	ids := platform.NewIDGenerator(clock)
	govStore := mysql.NewGovernanceStore(db)
	gov := governance.NewService(govStore, clock, ids, crypto.NewArgon2id(crypto.Argon2idParams{}), mysql.NewSessionStore(db))

	provisioned, err := gov.ProvisionTenant(ctx, governance.ProvisionTenantInput{
		TenantName:    "工具端到端租户",
		TeamSpaceName: "工具端到端团队",
		AdminUsername: "tools-e2e-admin",
		AdminDisplay:  "工具端到端管理员",
		AdminPassword: "e2e-admin-password",
	})
	if err != nil {
		t.Fatalf("开租户失败: %v", err)
	}
	tenantID := provisioned.Tenant.ID

	t.Cleanup(func() {
		bg := context.Background()
		for _, stmt := range []string{
			`DELETE FROM audit_logs WHERE tenant_id = ?`,
			`DELETE FROM embeddings WHERE tenant_id = ?`,
			`DELETE FROM card_recall_triggers WHERE tenant_id = ?`,
			`DELETE FROM labels WHERE tenant_id = ?`,
			`DELETE FROM links WHERE tenant_id = ?`,
			`DELETE FROM cards WHERE tenant_id = ?`,
			`DELETE FROM memories WHERE tenant_id = ?`,
			`DELETE FROM tokens WHERE tenant_id = ?`,
			`DELETE FROM agents WHERE tenant_id = ?`,
			`DELETE FROM space_permissions WHERE space_id IN (SELECT id FROM spaces WHERE tenant_id = ?)`,
			`DELETE FROM spaces WHERE tenant_id = ?`,
			`DELETE FROM users WHERE tenant_id = ?`,
			`DELETE FROM tenants WHERE id = ?`,
		} {
			if _, err := db.ExecContext(bg, stmt, string(tenantID)); err != nil {
				t.Errorf("清理失败（需手工处理 tenant %s）: %v", tenantID, err)
			}
		}
	})

	agent, err := gov.CreateAgent(ctx, tenantID, provisioned.Admin.ID, "clara", "Clara")
	if err != nil {
		t.Fatalf("建 agent 失败: %v", err)
	}
	plaintext, _, err := gov.IssueToken(ctx, governance.IssueTokenInput{
		TenantID: tenantID, UserID: provisioned.Admin.ID, AgentID: agent.ID,
		Name: "工具端到端", TTL: time.Hour,
	})
	if err != nil {
		t.Fatalf("签发 token 失败: %v", err)
	}

	// embedding 不可用时整条链路照跑，只是检索退化成全文匹配——
	// 那正是「降级是默认路径不是异常路径」该有的样子，测试也照此分支。
	var embedder ports.Embedder
	if cfg.Embedding.Enabled() {
		e := embedding.NewOllama(cfg.Embedding.BaseURL, cfg.Embedding.Model, cfg.Embedding.APIKey, cfg.Embedding.Timeout)
		if _, err := e.Embed(ctx, []string{"探活"}); err == nil {
			embedder = e
		} else {
			t.Logf("embedding 服务不可用，本轮跑降级路径: %v", err)
		}
	}

	know := knowledge.NewService(mysql.NewKnowledgeStore(db), clock, ids, embedder)
	activityService := activity.NewService(mysql.NewActivityStore(db), clock, ids)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(mcphost.NewHost(gov, know, activityService, logger, mcphost.HostOptions{
		Version: "tools-e2e", SemanticSearch: embedder != nil,
	}).Handler())
	t.Cleanup(srv.Close)

	return toolEnv{
		srv: srv, token: plaintext, know: know,
		tenantID:    tenantID,
		personalID:  provisioned.Personal.ID,
		teamID:      provisioned.TeamSpace.ID,
		hasEmbedder: embedder != nil,
	}
}

// tool 调一个 MCP 工具，把结构化输出解到 dst；返回错误文案（成功时为空）。
func (e toolEnv) tool(t *testing.T, name string, args map[string]any, dst any) string {
	t.Helper()
	payload, err := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "tools/call",
		"params": map[string]any{"name": name, "arguments": args},
	})
	if err != nil {
		t.Fatal(err)
	}
	status, body := rawCall(t, e.srv, e.token, string(payload))
	if status != 200 {
		t.Fatalf("%s 返回 HTTP %d: %s", name, status, body)
	}

	var envelope struct {
		Result struct {
			IsError           bool            `json:"isError"`
			StructuredContent json.RawMessage `json:"structuredContent"`
			Content           []struct {
				Text string `json:"text"`
			} `json:"content"`
		} `json:"result"`
		Error *struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(extractJSON(body)), &envelope); err != nil {
		t.Fatalf("%s 响应解析失败: %v\n%s", name, err, body)
	}
	if envelope.Error != nil {
		return envelope.Error.Message
	}
	if envelope.Result.IsError {
		if len(envelope.Result.Content) > 0 {
			return envelope.Result.Content[0].Text
		}
		return "(工具报错但没有内容)"
	}
	if dst != nil && len(envelope.Result.StructuredContent) > 0 {
		if err := json.Unmarshal(envelope.Result.StructuredContent, dst); err != nil {
			t.Fatalf("%s 输出解码失败: %v", name, err)
		}
	}
	return ""
}

// 只取测试关心的字段：这里不 import internal/mcp 的私有输出类型，
// 那样等于用被测代码自己的定义去验它自己。
type e2eSpace struct {
	ID   string `json:"id"`
	Type string `json:"type"`
	Name string `json:"name"`
}

type e2eContent struct {
	ID             string   `json:"id"`
	Space          e2eSpace `json:"space"`
	Title          string   `json:"title"`
	Version        int      `json:"version"`
	Labels         []string `json:"labels"`
	Status         string   `json:"status"`
	Qualified      bool     `json:"qualified"`
	RecallTriggers []string `json:"recallTriggers"`
	MatchedTrigger string   `json:"matchedTrigger"`
	Semantic       bool     `json:"semantic"`
}

type e2eSearch struct {
	Hits              []e2eContent `json:"hits"`
	SemanticAvailable bool         `json:"semanticAvailable"`
	CardMatches       int          `json:"cardMatches"`
}

// TestMCPToolsEndToEnd 走一遍真实的工作流。
func TestMCPToolsEndToEnd(t *testing.T) {
	env := newToolEnv(t)

	var memory e2eContent
	t.Run("memoria_create 默认写个人空间", func(t *testing.T) {
		if msg := env.tool(t, "memoria_create", map[string]any{
			"title":  "MySQL 连接池调优",
			"body":   "把最大连接数从两百降到四十之后，超时反而消失了。",
			"labels": []string{"mysql", "ops"},
		}, &memory); msg != "" {
			t.Fatal(msg)
		}
		if memory.Space.ID != string(env.personalID) {
			t.Fatalf("不传 space 应写入个人空间 %s，实际 %s", env.personalID, memory.Space.ID)
		}
		// 检索结果必须能让 agent 判断权威性——缺了 type 就判断不了
		if memory.Space.Type != governance.SpacePersonal || memory.Space.Name == "" {
			t.Fatalf("返回值必须标注来源空间的类型与名字，实际 %+v", memory.Space)
		}
		if len(memory.Labels) != 2 {
			t.Fatalf("标签应写进去，实际 %v", memory.Labels)
		}
	})

	t.Run("写入不可写空间被拒", func(t *testing.T) {
		// 刚开的租户里团队空间是隐含可读、写需显式授权
		msg := env.tool(t, "memoria_create", map[string]any{
			"title": "越权尝试", "body": "x", "space": string(env.teamID),
		}, nil)
		if msg == "" {
			t.Fatal("对无写权限的空间写入必须被拒绝")
		}
		if !containsAny(msg, "FORBIDDEN") {
			t.Fatalf("应返回 FORBIDDEN，实际: %s", msg)
		}
	})

	var card e2eContent
	t.Run("card_create 带召回条件", func(t *testing.T) {
		if msg := env.tool(t, "card_create", map[string]any{
			"title":         "备份成功不等于数据可恢复",
			"cardType":      "Codex 协作安全",
			"distilled":     "备份报告只给归档路径与校验和，不给目标文件清单，等于没有备份。",
			"understanding": "校验和证明的是这个包没坏，不是这个包里装对了东西。",
			"recallTriggers": []string{
				"准备执行不可逆的数据清除动作之前",
			},
		}, &card); msg != "" {
			t.Fatal(msg)
		}
		if card.Status != knowledge.CardActive {
			t.Fatalf("手写卡片默认 active，实际 %s", card.Status)
		}
		// 「理解」栏是质量分界线，判定只该有一处定义，由服务端算好回传
		if !card.Qualified {
			t.Fatal("写了「理解」栏的卡应标为 qualified")
		}
		if len(card.RecallTriggers) != 1 {
			t.Fatalf("召回条件应回传，实际 %v", card.RecallTriggers)
		}
	})

	t.Run("索引 worker 把向量补上", func(t *testing.T) {
		if !env.hasEmbedder {
			t.Skip("降级路径，没有索引队列")
		}
		total := 0
		for {
			report, err := env.know.IndexPending(context.Background(), 32)
			if err != nil {
				t.Fatalf("索引失败: %v", err)
			}
			total += report.Indexed
			if report.Picked == 0 {
				break
			}
		}
		// 一条记忆 + 一张卡片正文 + 一条召回条件 = 3
		if total != 3 {
			t.Fatalf("期望索引 3 条（记忆 / 卡片正文 / 召回条件），实际 %d", total)
		}
	})

	t.Run("card_recall 按情境召回并给出理由", func(t *testing.T) {
		var res e2eSearch
		if msg := env.tool(t, "card_recall", map[string]any{
			"situation": "用户说这个目录没用了让我整个删掉，但里面可能有不可再生的历史",
		}, &res); msg != "" {
			t.Fatal(msg)
		}
		if len(res.Hits) == 0 {
			t.Fatal("应召回那张卡")
		}
		if res.Hits[0].ID != card.ID {
			t.Fatalf("召回了错误的卡片: %s", res.Hits[0].Title)
		}
		// 没有召回理由，一次召回对 agent 来说就是凭空出现的建议
		if res.Hits[0].MatchedTrigger == "" {
			t.Fatal("必须带上命中的那条召回条件")
		}
		if res.SemanticAvailable != env.hasEmbedder {
			t.Fatalf("semanticAvailable 应如实反映这个部署的形态，期望 %v 实际 %v",
				env.hasEmbedder, res.SemanticAvailable)
		}
		if env.hasEmbedder && !res.Hits[0].Semantic {
			t.Fatal("接了 embedding 时命中应标为语义命中")
		}
		if res.Hits[0].Space.Type == "" {
			t.Fatal("召回结果同样要标注来源空间")
		}
	})

	t.Run("memoria_search 提示另有卡片命中", func(t *testing.T) {
		var res e2eSearch
		if msg := env.tool(t, "memoria_search", map[string]any{"query": "归档路径"}, &res); msg != "" {
			t.Fatal(msg)
		}
		// agent 搜一次 memory 没找到就以为没有，而卡片才是核心载体
		if res.CardMatches != 1 {
			t.Fatalf("memory 命中偏少时应给出卡片命中计数，期望 1，实际 %d", res.CardMatches)
		}
	})

	t.Run("memoria_update 的乐观锁", func(t *testing.T) {
		newTitle := "MySQL 连接池调优（已复核）"
		var updated e2eContent
		if msg := env.tool(t, "memoria_update", map[string]any{
			"id": memory.ID, "version": memory.Version, "title": newTitle,
		}, &updated); msg != "" {
			t.Fatal(msg)
		}
		if updated.Version != memory.Version+1 {
			t.Fatalf("版本号应递增，实际 %d", updated.Version)
		}

		// 再用旧版本号改一次：必须冲突，且告诉调用方现在是第几版
		msg := env.tool(t, "memoria_update", map[string]any{
			"id": memory.ID, "version": memory.Version, "title": "再改一次",
		}, nil)
		if !containsAny(msg, "VERSION_CONFLICT") {
			t.Fatalf("应返回 VERSION_CONFLICT，实际: %s", msg)
		}
		if !containsAny(msg, "2") {
			t.Fatalf("冲突响应要带上当前版本号，实际: %s", msg)
		}
	})

	t.Run("memoria_link 建立与删除", func(t *testing.T) {
		args := map[string]any{
			"fromId": memory.ID, "toType": "card", "toId": card.ID, "kind": "related",
			"note": "同一次踩坑的两面",
		}
		if msg := env.tool(t, "memoria_link", args, nil); msg != "" {
			t.Fatal(msg)
		}
		var got e2eContent
		if msg := env.tool(t, "memoria_get", map[string]any{"id": memory.ID}, &got); msg != "" {
			t.Fatal(msg)
		}

		args["remove"] = true
		if msg := env.tool(t, "memoria_link", args, nil); msg != "" {
			t.Fatal(msg)
		}
		// 幂等：再删一次仍然成功
		if msg := env.tool(t, "memoria_link", args, nil); msg != "" {
			t.Fatalf("删除关联应当幂等，实际: %s", msg)
		}
	})

	t.Run("space_stats 数出还有多少张卡只是摘要", func(t *testing.T) {
		var res struct {
			Spaces []struct {
				Space            e2eSpace `json:"space"`
				Memories         int      `json:"memories"`
				Cards            int      `json:"cards"`
				UnqualifiedCards int      `json:"unqualifiedCards"`
				RecallTriggers   int      `json:"recallTriggers"`
			} `json:"spaces"`
		}
		if msg := env.tool(t, "space_stats", map[string]any{}, &res); msg != "" {
			t.Fatal(msg)
		}
		var personal *struct {
			Space            e2eSpace `json:"space"`
			Memories         int      `json:"memories"`
			Cards            int      `json:"cards"`
			UnqualifiedCards int      `json:"unqualifiedCards"`
			RecallTriggers   int      `json:"recallTriggers"`
		}
		for i := range res.Spaces {
			if res.Spaces[i].Space.ID == string(env.personalID) {
				personal = &res.Spaces[i]
			}
		}
		if personal == nil {
			t.Fatalf("个人空间应出现在统计里: %+v", res.Spaces)
		}
		if personal.Memories != 1 || personal.Cards != 1 || personal.RecallTriggers != 1 {
			t.Fatalf("统计不对: %+v", personal)
		}
		// 这张卡写了「理解」，所以不该被算成"只是摘要"
		if personal.UnqualifiedCards != 0 {
			t.Fatalf("合格卡片不该被数进 unqualifiedCards，实际 %d", personal.UnqualifiedCards)
		}
	})

	t.Run("memoria_delete 是软删除", func(t *testing.T) {
		if msg := env.tool(t, "memoria_delete", map[string]any{"id": memory.ID}, nil); msg != "" {
			t.Fatal(msg)
		}
		// 软删除之后仍能按 ID 取到——这正是软删除的意义
		var got e2eContent
		if msg := env.tool(t, "memoria_get", map[string]any{"id": memory.ID}, &got); msg != "" {
			t.Fatalf("软删除后仍应能按 ID 取到，实际: %s", msg)
		}
		if got.Status != knowledge.MemoryDeleted {
			t.Fatalf("状态应为 deleted，实际 %s", got.Status)
		}
		// 但不再进默认检索
		var res e2eSearch
		if msg := env.tool(t, "memoria_search", map[string]any{"query": "连接池"}, &res); msg != "" {
			t.Fatal(msg)
		}
		for _, h := range res.Hits {
			if h.ID == memory.ID {
				t.Fatal("软删除的记忆不该出现在默认检索里")
			}
		}
	})
}

func containsAny(s string, subs ...string) bool {
	for _, sub := range subs {
		if len(sub) > 0 && len(s) >= len(sub) {
			for i := 0; i+len(sub) <= len(s); i++ {
				if s[i:i+len(sub)] == sub {
					return true
				}
			}
		}
	}
	return false
}
