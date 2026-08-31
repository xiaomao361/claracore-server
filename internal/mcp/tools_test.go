package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"claracore-server/internal/domain/activity"
	"claracore-server/internal/domain/continuity"
	"claracore-server/internal/domain/external"
	"claracore-server/internal/domain/knowledge"
	"claracore-server/internal/platform"
)

type fakeExternalSearch struct {
	last   external.SearchInput
	result external.SearchResult
	err    error
}

func (f *fakeExternalSearch) Search(_ context.Context, in external.SearchInput) (external.SearchResult, error) {
	f.last = in
	return f.result, f.err
}

// fakeKnow 是核心域的替身，**只记录 Host 传了什么**。
//
// 它刻意不复刻任何领域逻辑：这一层唯一该被测的是「Host 有没有把权限
// 正确地解析成空间集合再交下去」。用真的 knowledge.Service 测的话，
// 一个漏掉权限解析的 Host 仍然会因为领域层兜住而全绿——
// 而线上真正防住越权的正是 Host 传下去的那个集合。
type fakeKnow struct {
	lastCreateMemory knowledge.CreateMemoryInput
	lastUpdateMemory knowledge.UpdateMemoryInput
	lastSearch       knowledge.SearchMemoriesInput
	lastRecall       knowledge.RecallCardsInput
	lastCreateCard   knowledge.CreateCardInput
	lastLink         knowledge.LinkInput
	lastPromote      knowledge.PromoteInput
	lastGetSpaces    []platform.ID
	lastStatsSpaces  []platform.ID

	calls int
	err   error

	memory knowledge.Memory
	card   knowledge.Card
	search knowledge.MemorySearchResult
	recall knowledge.CardSearchResult
	stats  []knowledge.SpaceStats
}

type fakeContinuity struct {
	lastList    continuity.ListInput
	lastGet     continuity.GetInput
	lastUpdate  continuity.UpdateInput
	lastHandoff continuity.HandoffInput

	err    error
	lines  []continuity.LineSummary
	packet continuity.ResumePacket
}

func (f *fakeContinuity) SetStatus(_ context.Context, in continuity.SetStatusInput) (continuity.ResumePacket, error) {
	f.packet.Line.Status = in.Status
	return f.packet, f.err
}

func newFakeContinuity() *fakeContinuity {
	now := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	line := continuity.Line{
		ID: "01LINEAAAAAAAAAAAAAAAAAAAA", TenantID: tenantA, OwnerUserID: userClar, OwnerAgentID: agentClara,
		Title: "Clara Shared Line", Status: continuity.LineActive, CreatedAt: now, UpdatedAt: now,
		Position: &continuity.Position{
			ID: "01POSITIONAAAAAAAAAAAAAAAA", TenantID: tenantA, LineID: "01LINEAAAAAAAAAAAAAAAAAAAA",
			Summary: "当前进展", InterpretationStatus: continuity.PositionActive, NextStep: "继续验证",
			WriterUserID: userClar, WriterAgentID: agentClara, Version: 1, UpdatedAt: now,
		},
	}
	return &fakeContinuity{
		lines: []continuity.LineSummary{{
			ID: line.ID, OwnerAgentID: line.OwnerAgentID, Title: line.Title, Status: line.Status,
			Summary: line.Position.Summary, InterpretationStatus: line.Position.InterpretationStatus,
			NextStep: line.Position.NextStep, UpdatedAt: now,
		}},
		packet: continuity.ResumePacket{Line: line},
	}
}

func (f *fakeContinuity) List(_ context.Context, in continuity.ListInput) ([]continuity.LineSummary, error) {
	f.lastList = in
	return f.lines, f.err
}

func (f *fakeContinuity) Get(_ context.Context, in continuity.GetInput) (continuity.ResumePacket, error) {
	f.lastGet = in
	return f.packet, f.err
}

func (f *fakeContinuity) Update(_ context.Context, in continuity.UpdateInput) (continuity.ResumePacket, error) {
	f.lastUpdate = in
	return f.packet, f.err
}

func (f *fakeContinuity) Handoff(_ context.Context, in continuity.HandoffInput) (continuity.ResumePacket, error) {
	f.lastHandoff = in
	return f.packet, f.err
}

func newFakeKnow() *fakeKnow {
	now := time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC)
	agent := platform.ID(agentClara)
	return &fakeKnow{
		memory: knowledge.Memory{
			ID: "01MEMORYAAAAAAAAAAAAAAAAAA", TenantID: tenantA, SpaceID: spPers,
			AuthorUserID: userClar, AuthorAgentID: &agent,
			Title: "MySQL 连接池配置", Body: "MaxOpenConns 设成核数的 4 倍。",
			Status: knowledge.MemoryActive, Version: 1, CreatedAt: now, UpdatedAt: now,
		},
		card: knowledge.Card{
			ID: "01CARDAAAAAAAAAAAAAAAAAAAA", TenantID: tenantA, SpaceID: spTeam,
			AuthorUserID: userClar,
			Title:        "备份成功不等于数据可恢复", CardType: "协作安全",
			Status: knowledge.CardActive, Distilled: "只给路径和校验和不等于有备份。",
			Understanding: "校验和证明包没坏，不证明包里装对了东西。",
			Version:       1, CreatedAt: now, UpdatedAt: now,
			RecallTriggers: []knowledge.RecallTrigger{{Text: "准备删除数据时"}},
		},
	}
}

func (f *fakeKnow) bump() error { f.calls++; return f.err }

func (f *fakeKnow) CreateMemory(_ context.Context, in knowledge.CreateMemoryInput) (knowledge.Memory, error) {
	f.lastCreateMemory = in
	if err := f.bump(); err != nil {
		return knowledge.Memory{}, err
	}
	m := f.memory
	m.SpaceID = in.SpaceID
	m.AuthorAgentID = in.AuthorAgentID
	m.Labels = in.Labels
	return m, nil
}

func (f *fakeKnow) GetMemory(_ context.Context, _, _ platform.ID, spaces []platform.ID) (knowledge.Memory, []knowledge.Link, error) {
	f.lastGetSpaces = spaces
	if err := f.bump(); err != nil {
		return knowledge.Memory{}, nil, err
	}
	return f.memory, []knowledge.Link{{
		From: knowledge.Ref{Type: knowledge.EntityMemory, ID: f.memory.ID},
		To:   knowledge.Ref{Type: knowledge.EntityCard, ID: "01CARDHIDDENAAAAAAAAAAAAAA"},
		Kind: "related", Note: "同一次踩坑",
	}}, nil
}

func (f *fakeKnow) UpdateMemory(_ context.Context, in knowledge.UpdateMemoryInput) (knowledge.Memory, error) {
	f.lastUpdateMemory = in
	if err := f.bump(); err != nil {
		return knowledge.Memory{}, err
	}
	return f.memory, nil
}

func (f *fakeKnow) DeleteMemory(_ context.Context, _, _ platform.ID, writable []platform.ID) (knowledge.Memory, error) {
	f.lastUpdateMemory = knowledge.UpdateMemoryInput{WritableSpaces: writable}
	if err := f.bump(); err != nil {
		return knowledge.Memory{}, err
	}
	return f.memory, nil
}

func (f *fakeKnow) SearchMemories(_ context.Context, in knowledge.SearchMemoriesInput) (knowledge.MemorySearchResult, error) {
	f.lastSearch = in
	if err := f.bump(); err != nil {
		return knowledge.MemorySearchResult{}, err
	}
	return f.search, nil
}

func (f *fakeKnow) RecallMemoryForContext(_ context.Context, in knowledge.SearchMemoriesInput) (knowledge.AutomaticMemoryRecallResult, error) {
	f.lastSearch = in
	if err := f.bump(); err != nil {
		return knowledge.AutomaticMemoryRecallResult{}, err
	}
	if len(f.search.Hits) == 0 {
		return knowledge.AutomaticMemoryRecallResult{Reason: "no_candidates", SemanticAvailable: f.search.SemanticAvailable, PolicyAvailable: f.search.SemanticAvailable}, nil
	}
	hit := f.search.Hits[0]
	return knowledge.AutomaticMemoryRecallResult{Hit: &hit, Reason: "high_confidence_top1", SemanticAvailable: f.search.SemanticAvailable, PolicyAvailable: f.search.SemanticAvailable}, nil
}

func (f *fakeKnow) CreateCard(_ context.Context, in knowledge.CreateCardInput) (knowledge.Card, error) {
	f.lastCreateCard = in
	if err := f.bump(); err != nil {
		return knowledge.Card{}, err
	}
	c := f.card
	c.SpaceID = in.SpaceID
	c.Status = in.Status
	return c, nil
}

func (f *fakeKnow) GetCard(_ context.Context, _, _ platform.ID, spaces []platform.ID) (knowledge.Card, []knowledge.Link, error) {
	f.lastGetSpaces = spaces
	if err := f.bump(); err != nil {
		return knowledge.Card{}, nil, err
	}
	return f.card, nil, nil
}

func (f *fakeKnow) UpdateCard(_ context.Context, in knowledge.UpdateCardInput) (knowledge.Card, error) {
	if err := f.bump(); err != nil {
		return knowledge.Card{}, err
	}
	_ = in
	return f.card, nil
}

func (f *fakeKnow) SearchCards(_ context.Context, _ knowledge.SearchCardsInput) (knowledge.CardSearchResult, error) {
	if err := f.bump(); err != nil {
		return knowledge.CardSearchResult{}, err
	}
	return f.recall, nil
}

func (f *fakeKnow) RecallCards(_ context.Context, in knowledge.RecallCardsInput) (knowledge.CardSearchResult, error) {
	f.lastRecall = in
	if err := f.bump(); err != nil {
		return knowledge.CardSearchResult{}, err
	}
	return f.recall, nil
}

func (f *fakeKnow) RecallCardForContext(_ context.Context, in knowledge.RecallCardsInput) (knowledge.AutomaticRecallResult, error) {
	f.lastRecall = in
	if err := f.bump(); err != nil {
		return knowledge.AutomaticRecallResult{}, err
	}
	if len(f.recall.Hits) == 0 {
		return knowledge.AutomaticRecallResult{Reason: "no_candidates", SemanticAvailable: f.recall.SemanticAvailable, PolicyAvailable: f.recall.SemanticAvailable}, nil
	}
	hit := f.recall.Hits[0]
	return knowledge.AutomaticRecallResult{Hit: &hit, Reason: "high_confidence_top1", SemanticAvailable: f.recall.SemanticAvailable, PolicyAvailable: true}, nil
}

func (f *fakeKnow) CreateLink(_ context.Context, in knowledge.LinkInput) (knowledge.Link, error) {
	f.lastLink = in
	if err := f.bump(); err != nil {
		return knowledge.Link{}, err
	}
	return knowledge.Link{From: in.From, To: in.To, Kind: in.Kind}, nil
}

func (f *fakeKnow) DeleteLink(_ context.Context, in knowledge.LinkInput) error {
	f.lastLink = in
	return f.bump()
}

func (f *fakeKnow) Promote(_ context.Context, in knowledge.PromoteInput) (knowledge.Ref, error) {
	f.lastPromote = in
	if err := f.bump(); err != nil {
		return knowledge.Ref{}, err
	}
	return knowledge.Ref{Type: in.Ref.Type, ID: "01COPYAAAAAAAAAAAAAAAAAAAA"}, nil
}

func (f *fakeKnow) SpaceStats(_ context.Context, _ platform.ID, spaces []platform.ID) ([]knowledge.SpaceStats, error) {
	f.lastStatsSpaces = spaces
	if err := f.bump(); err != nil {
		return nil, err
	}
	return f.stats, nil
}

// --- 调用辅助 ---

// callTool 调一个工具，返回结构化输出与错误文案。
func callTool(t *testing.T, srv *httptest.Server, token, name string, args map[string]any) (map[string]any, string) {
	t.Helper()
	if args == nil {
		args = map[string]any{}
	}
	resp := call(t, srv, token, "tools/call", map[string]any{"name": name, "arguments": args})

	if e, ok := resp.body["error"].(map[string]any); ok {
		msg, _ := e["message"].(string)
		return nil, msg
	}
	result, ok := resp.body["result"].(map[string]any)
	if !ok {
		t.Fatalf("响应里没有 result: %s", resp.raw)
	}
	// 工具执行失败走 isError + content，不是 JSON-RPC 的 error
	if isErr, _ := result["isError"].(bool); isErr {
		return nil, contentText(result)
	}
	structured, _ := result["structuredContent"].(map[string]any)
	return structured, ""
}

func contentText(result map[string]any) string {
	items, _ := result["content"].([]any)
	for _, it := range items {
		if m, ok := it.(map[string]any); ok {
			if s, ok := m["text"].(string); ok {
				return s
			}
		}
	}
	return ""
}

func TestSharedLineUsesTokenIdentityAndReturnsExactPacket(t *testing.T) {
	srv, _, cont := newToolHostWithActivityAndContinuity(t, newFakeActivity())

	out, errMsg := callTool(t, srv, "ccs_valid_clara", "shared_line_update", map[string]any{
		"summary":              "已完成 MCP 接入",
		"interpretationStatus": "active",
		"factsUsed":            []string{"01MEMORYAAAAAAAAAAAAAAAAAA"},
		"nextStep":             "运行集成测试",
		"version":              1,
	})
	if errMsg != "" {
		t.Fatal(errMsg)
	}
	if cont.lastUpdate.TenantID != tenantA || cont.lastUpdate.UserID != userClar || cont.lastUpdate.AgentID != agentClara {
		t.Fatalf("Shared Line 身份必须来自 token，实际 tenant=%s user=%s agent=%s",
			cont.lastUpdate.TenantID, cont.lastUpdate.UserID, cont.lastUpdate.AgentID)
	}
	if cont.lastUpdate.Title != "Clara Shared Line" {
		t.Fatalf("新建时应生成稳定默认标题，实际 %q", cont.lastUpdate.Title)
	}
	var packet resumePacketOut
	decodeInto(t, out, &packet)
	if packet.Line.ID != "01LINEAAAAAAAAAAAAAAAAAAAA" || packet.Line.Position == nil || packet.Line.Position.Version != 1 {
		t.Fatalf("更新确认必须返回 exact line 的完整恢复包，实际 %+v", packet)
	}
}

func TestSharedLineErrorsHaveStableCodes(t *testing.T) {
	srv, _, cont := newToolHostWithActivityAndContinuity(t, newFakeActivity())
	cont.err = &continuity.LineIDRequiredError{Candidates: []continuity.LineSummary{
		{ID: "01LINEAAAAAAAAAAAAAAAAAAAA", Title: "发布收口"},
		{ID: "01LINEBBBBBBBBBBBBBBBBBBBB", Title: "记忆库"},
	}}
	_, errMsg := callTool(t, srv, "ccs_valid_clara", "shared_line_get", nil)
	if !strings.Contains(errMsg, codeLineIDRequired) || !strings.Contains(errMsg, "01LINEAAAAAAAAAAAAAAAAAAAA") {
		t.Fatalf("多线歧义应返回稳定码与候选，实际 %q", errMsg)
	}

	cont.err = &continuity.VersionConflictError{Current: 7}
	_, errMsg = callTool(t, srv, "ccs_valid_clara", "shared_line_update", map[string]any{
		"lineId": "01LINEAAAAAAAAAAAAAAAAAAAA", "summary": "冲突更新", "version": 6,
	})
	if !strings.Contains(errMsg, codeConflict) || !strings.Contains(errMsg, "7") {
		t.Fatalf("并发冲突应返回 VERSION_CONFLICT 与当前版本，实际 %q", errMsg)
	}
}

func decodeInto(t *testing.T, raw map[string]any, dst any) {
	t.Helper()
	buf, err := json.Marshal(raw)
	if err != nil {
		t.Fatal(err)
	}
	if err := json.Unmarshal(buf, dst); err != nil {
		t.Fatal(err)
	}
}

// ---------------------------------------------------------------------------
// 权限解析：Host 层唯一真正的职责
// ---------------------------------------------------------------------------

func TestWriteDefaultsToPersonalSpace(t *testing.T) {
	srv, know := newToolHost(t)

	out, errMsg := callTool(t, srv, "ccs_valid_clara", "memoria_create", map[string]any{
		"title": "标题", "body": "正文",
	})
	if errMsg != "" {
		t.Fatal(errMsg)
	}
	// 不传 space 即个人空间：随手记不该有心智负担（03 §3.1）
	if know.lastCreateMemory.SpaceID != spPers {
		t.Fatalf("默认应写入个人空间 %s，实际 %s", spPers, know.lastCreateMemory.SpaceID)
	}
	var m memoryOut
	decodeInto(t, out, &m)
	if m.Space.Type != "personal" || m.Space.Name != "毛仔" {
		t.Fatalf("返回值必须标注来源空间的类型与名字，实际 %+v", m.Space)
	}
}

// TestWriteToUnwritableSpaceNeverReachesDomain 是这一层最要紧的一条。
//
// 权限必须在 Host 就拦住。让它穿到领域层再被兜住的话，这个测试仍然会绿，
// 但线上真正防住越权的那一步已经没了——领域层收到的是「已经解析好的
// 确定空间」，它压根不认识 user 和 permission。
func TestWriteToUnwritableSpaceNeverReachesDomain(t *testing.T) {
	srv, know := newToolHost(t)

	// ccs_readonly 只对团队空间有读权限
	_, errMsg := callTool(t, srv, "ccs_readonly", "memoria_create", map[string]any{
		"title": "标题", "body": "正文", "space": spTeam,
	})
	if errMsg == "" {
		t.Fatal("对只读空间的写入必须被拒绝")
	}
	if !strings.Contains(errMsg, codeForbidden) {
		t.Fatalf("应返回 %s，实际: %s", codeForbidden, errMsg)
	}
	if know.calls != 0 {
		t.Fatal("权限没通过时领域层根本不该被调到——那一步才是线上真正拦住越权的地方")
	}
}

func TestUpdateLocatesByWritableSpaces(t *testing.T) {
	srv, know := newToolHost(t)

	title := "新标题"
	if _, errMsg := callTool(t, srv, "ccs_valid_clara", "memoria_update", map[string]any{
		"id": "01MEMORYAAAAAAAAAAAAAAAAAA", "title": title,
	}); errMsg != "" {
		t.Fatal(errMsg)
	}
	// 用可写集合定位，不是「可读定位 + 再判可写」：对一条自己改不了的内容，
	// 「不存在」与「无权修改」给调用方的下一步是同一件事。
	//
	// 注意团队空间**不在**里面：team 是全租户隐含可读、写需显式授权，
	// 而 fixture 里没给 clara 写权限。这正是可写集合与可读集合必须
	// 分开的原因——它们真的不一样。
	got := know.lastUpdateMemory.WritableSpaces
	if len(got) != 2 {
		t.Fatalf("clara 只对个人与项目空间可写，实际传下去 %d 个: %v", len(got), got)
	}
	for _, id := range got {
		if id == spTeam {
			t.Fatal("团队空间只可读不可写，不该出现在定位集合里——出现了就意味着一次越权修改")
		}
	}
}

func TestUpdateOfReadOnlyUserGetsEmptyWritableSet(t *testing.T) {
	srv, know := newToolHost(t)

	// 只读用户没有任何可写空间。领域层会因此报 ErrInvalidArgument，
	// 而不是拿一个空集合去查库（`IN ()` 是语法错误）。
	if _, errMsg := callTool(t, srv, "ccs_readonly", "memoria_update", map[string]any{
		"id": "01MEMORYAAAAAAAAAAAAAAAAAA", "title": "x",
	}); errMsg != "" {
		t.Log("领域层拒绝了空可写集合，符合预期:", errMsg)
	}
	if len(know.lastUpdateMemory.WritableSpaces) != 0 {
		t.Fatalf("只读用户的可写集合必须是空的，实际 %v", know.lastUpdateMemory.WritableSpaces)
	}
}

func TestSearchDefaultsToAllReadableSpacesWithRank(t *testing.T) {
	srv, know := newToolHost(t)

	if _, errMsg := callTool(t, srv, "ccs_valid_clara", "memoria_search", map[string]any{
		"query": "连接池",
	}); errMsg != "" {
		t.Fatal(errMsg)
	}
	got := know.lastSearch.Spaces
	if len(got) != 3 {
		t.Fatalf("不传 spaces 即覆盖全部可读空间，实际 %d 个", len(got))
	}
	// Rank 来自 governance.Space.SearchRank()：team > project > personal。
	// 在 Host 里另写一份映射，就是把「空间层级意味着什么」复制到了第二个地方。
	want := map[platform.ID]int{spTeam: 2, spProj: 1, spPers: 0}
	for _, sp := range got {
		if want[sp.ID] != sp.Rank {
			t.Fatalf("空间 %s 的排序权重应为 %d，实际 %d", sp.ID, want[sp.ID], sp.Rank)
		}
	}
}

func TestSearchRejectsUnreadableSpace(t *testing.T) {
	srv, know := newToolHost(t)

	_, errMsg := callTool(t, srv, "ccs_valid_clara", "memoria_search", map[string]any{
		"query": "x", "spaces": []string{"01SPACEHIDDENAAAAAAAAAAAAA"},
	})
	// 报错而不是静默跳过：静默过滤会让 agent 以为搜过了那个空间
	// 并得到「没有结果」——检索为空和无权检索是完全不同的两件事
	if errMsg == "" {
		t.Fatal("指定了不可读空间必须报错，不能静默跳过")
	}
	if know.calls != 0 {
		t.Fatal("权限没通过时不该调领域层")
	}
}

// ---------------------------------------------------------------------------
// 身份只来自 token
// ---------------------------------------------------------------------------

func TestAuthorComesFromTokenNotArguments(t *testing.T) {
	srv, know := newToolHost(t)

	// 硬塞身份字段会被 SDK 的 schema 校验直接拒掉（additionalProperties）。
	// 这比"解码时被忽略"更强：伪造身份的尝试连 handler 都进不去。
	_, errMsg := callTool(t, srv, "ccs_valid_clara", "memoria_create", map[string]any{
		"title": "标题", "body": "正文",
		"authorUserId": "01ATTACKERAAAAAAAAAAAAAAAA",
		"tenantId":     "01OTHERTENANTAAAAAAAAAAAAA",
	})
	if errMsg == "" {
		t.Fatal("塞进来的身份字段应当被 schema 拒绝")
	}
	if know.calls != 0 {
		t.Fatal("schema 校验没通过时不该进 handler")
	}

	// 正常调用：身份全部来自 token
	if _, errMsg := callTool(t, srv, "ccs_valid_clara", "memoria_create", map[string]any{
		"title": "标题", "body": "正文",
	}); errMsg != "" {
		t.Fatal(errMsg)
	}
	in := know.lastCreateMemory
	if in.AuthorUserID != userClar || in.TenantID != tenantA {
		t.Fatalf("身份必须来自 token（铁律 4），实际 user=%s tenant=%s", in.AuthorUserID, in.TenantID)
	}
	// agent 是审计维度：记下「哪个 agent 写的」，但它不参与任何判断
	if in.AuthorAgentID == nil || *in.AuthorAgentID != agentClara {
		t.Fatalf("应记下写入的 agent，实际 %v", in.AuthorAgentID)
	}
}

// TestToolSchemasHaveNoIdentityParams 直接检查对外暴露的 schema。
//
// 参数化的身份等同于可伪造的身份（03 §3.3）。这条盯的是**未来新增的工具**：
// 谁哪天顺手加一个 userId 参数，这里会红。
func TestToolSchemasHaveNoIdentityParams(t *testing.T) {
	srv, _ := newToolHost(t)
	resp := call(t, srv, "ccs_valid_clara", "tools/list", map[string]any{})

	result, ok := resp.body["result"].(map[string]any)
	if !ok {
		t.Fatalf("tools/list 失败: %s", resp.raw)
	}
	tools, _ := result["tools"].([]any)
	if len(tools) != 24 {
		t.Fatalf("契约应恰好注册 24 个工具，实际 %d 个", len(tools))
	}

	banned := []string{"userid", "tenantid", "agentid", "authoruserid"}
	names := map[string]bool{}
	for _, item := range tools {
		tool, _ := item.(map[string]any)
		name, _ := tool["name"].(string)
		names[name] = true

		schema, err := json.Marshal(tool["inputSchema"])
		if err != nil {
			t.Fatal(err)
		}
		lowered := strings.ToLower(string(schema))
		for _, b := range banned {
			// "spaceId" 这类是允许的；这里只找身份字段
			if strings.Contains(lowered, `"`+b+`"`) {
				t.Errorf("工具 %s 的 schema 里出现了身份参数 %q——身份只能来自 token", name, b)
			}
		}
	}

	for _, want := range []string{
		"whoami", "server_status", "gateway_auto_context", "gateway_context_feedback", "context_quality_status",
		"memoria_create", "memoria_search", "memoria_get", "memoria_update", "memoria_delete", "memoria_link",
		"card_create", "card_recall", "card_search", "card_get", "card_update",
		"space_list", "promote", "space_stats",
		"shared_line_list", "shared_line_get", "shared_line_update", "shared_line_handoff",
		"shared_line_status",
	} {
		if !names[want] {
			t.Errorf("契约里的工具 %s 没有注册", want)
		}
	}
	// 自动上下文是核心运行面能力，仍给少量增长留出硬上限。
	if len(tools) > 24 {
		t.Errorf("工具数量 %d 超过 24，维护类操作应该归 Web 治理面而不是塞进 MCP", len(tools))
	}
}

// ---------------------------------------------------------------------------
// 结果编码
// ---------------------------------------------------------------------------

func TestSearchResultCarriesSemanticSignals(t *testing.T) {
	srv, know := newToolHost(t)
	know.search = knowledge.MemorySearchResult{
		Hits: []knowledge.MemoryHit{
			{Memory: know.memory, Score: 0.81, Semantic: true},
			{Memory: know.memory, Score: 0.30, Semantic: false},
		},
		SemanticAvailable: true,
		CardMatches:       3,
	}

	out, errMsg := callTool(t, srv, "ccs_valid_clara", "memoria_search", map[string]any{"query": "x"})
	if errMsg != "" {
		t.Fatal(errMsg)
	}
	var res memoriaSearchOut
	decodeInto(t, out, &res)

	if !res.SemanticAvailable {
		t.Fatal("semanticAvailable 必须透传——「没搜到」和「语义那一半没跑」是两件事")
	}
	if len(res.Hits) != 2 || !res.Hits[0].Semantic || res.Hits[1].Semantic {
		t.Fatalf("每条命中都要标注 semantic，实际 %+v", res.Hits)
	}
	// agent 搜一次 memory 没找到就以为没有，而卡片才是核心载体
	if res.CardMatches != 3 {
		t.Fatalf("卡片命中计数应透传，实际 %d", res.CardMatches)
	}
	if res.Hits[0].Space.Type != "personal" {
		t.Fatalf("每条命中都要标注来源空间，实际 %+v", res.Hits[0].Space)
	}
}

func TestMemoriaSearchReturnsUnifiedPermissionScopedMatches(t *testing.T) {
	lastSuccess := time.Date(2026, 8, 29, 8, 0, 0, 0, time.UTC)
	ext := &fakeExternalSearch{result: external.SearchResult{
		SemanticAvailable: true,
		Hits: []external.SearchHit{{
			Document:   external.MirrorDocument{ID: "01EXTERNALAAAAAAAAAAAAAAAA", TenantID: tenantA, SpaceID: spTeam, Title: "Atlas Runbook", Kind: "yuque_document", SourceRef: "https://atlas.example/doc", Revision: "rev-2", UpstreamUpdatedAt: "2026-08-29T07:00:00Z", UpdatedAt: lastSuccess},
			SourceName: "atlas-docs", SourceSystem: "shared-docs", LastSyncStatus: external.SyncFailed, LastSucceededAt: &lastSuccess, Score: 0.91,
		}},
	}}
	srv, know, _ := newToolHostWithActivityAndContinuity(t, newFakeActivity(), HostOptions{Version: "test", SemanticSearch: true, ExternalSearch: ext, ExternalConfigured: true})
	know.search = knowledge.MemorySearchResult{Hits: []knowledge.MemoryHit{{Memory: know.memory, Score: 0.80, Semantic: true}}, SemanticAvailable: true, CardMatches: 1}
	know.recall = knowledge.CardSearchResult{Hits: []knowledge.CardHit{{Card: know.card, Score: 0.85, Semantic: true}}, SemanticAvailable: true}
	out, errMsg := callTool(t, srv, "ccs_valid_clara", "memoria_search", map[string]any{"query": "部署", "limit": 3})
	if errMsg != "" {
		t.Fatal(errMsg)
	}
	var result memoriaSearchOut
	decodeInto(t, out, &result)
	if len(result.Matches) != 3 || result.Matches[0].Type != "external" || result.Matches[1].Type != "card" || result.Matches[2].Type != "memory" {
		t.Fatalf("semantic matches must share one ranking: %+v", result.Matches)
	}
	if result.Matches[0].External == nil || result.Matches[0].External.Freshness != "last_success_preserved_after_failure" || result.Matches[0].External.SourceRef == "" {
		t.Fatalf("external source and freshness must be explicit: %+v", result.Matches[0])
	}
	if ext.last.TenantID != tenantA || len(ext.last.SpaceIDs) != 3 || ext.last.Query != "部署" {
		t.Fatalf("resolved read scope must reach external search: %+v", ext.last)
	}
	if result.ExternalStatus != "available" || !result.ExternalSemanticAvailable || !result.CardSemanticAvailable {
		t.Fatalf("external retrieval state must be explicit: %+v", result)
	}
	if result.Matches[0].External.Space.Type != "team" {
		t.Fatalf("external hit must carry authoritative space metadata: %+v", result.Matches[0].External.Space)
	}
}

func TestUnifiedSearchDoesNotBypassLabelOrHistoricalFilters(t *testing.T) {
	ext := &fakeExternalSearch{}
	for _, tc := range []struct {
		args   map[string]any
		status string
	}{
		{map[string]any{"query": "x", "labels": []string{"private"}}, "excluded_by_label_filter"},
		{map[string]any{"query": "x", "timeView": "historical"}, "excluded_by_historical_view"},
	} {
		srv, _, _ := newToolHostWithActivityAndContinuity(t, newFakeActivity(), HostOptions{Version: "test", ExternalSearch: ext})
		out, errMsg := callTool(t, srv, "ccs_valid_clara", "memoria_search", tc.args)
		if errMsg != "" {
			t.Fatal(errMsg)
		}
		var result memoriaSearchOut
		decodeInto(t, out, &result)
		if result.ExternalStatus != tc.status {
			t.Fatalf("status=%s want=%s", result.ExternalStatus, tc.status)
		}
	}
	if ext.last.Query != "" {
		t.Fatalf("external search must not run through unsupported filters: %+v", ext.last)
	}
}

func TestRecallCarriesMatchedTrigger(t *testing.T) {
	srv, know := newToolHost(t)
	know.recall = knowledge.CardSearchResult{
		Hits: []knowledge.CardHit{{
			Card: know.card, Score: 0.62, Semantic: true, Trigger: "准备删除数据时",
		}},
		SemanticAvailable: true,
	}

	out, errMsg := callTool(t, srv, "ccs_valid_clara", "card_recall", map[string]any{
		"situation": "用户让我清理目录",
	})
	if errMsg != "" {
		t.Fatal(errMsg)
	}
	var res cardSearchOut
	decodeInto(t, out, &res)
	if len(res.Hits) != 1 {
		t.Fatalf("期望 1 张卡，实际 %d", len(res.Hits))
	}
	// 没有召回理由的话，一次召回对 agent 来说就是凭空出现的建议
	if res.Hits[0].Trigger != "准备删除数据时" {
		t.Fatalf("必须带上命中的召回条件，实际 %q", res.Hits[0].Trigger)
	}
	if len(res.Hits[0].RecallTriggers) == 0 {
		t.Fatal("返回的卡片要带上它的全部召回条件")
	}
	if !res.Hits[0].Qualified {
		t.Fatal("「理解」栏非空的卡应标为 qualified——那条质量分界线只该有一处定义")
	}
	if know.lastRecall.Situation != "用户让我清理目录" {
		t.Fatalf("situation 应原样传下去，实际 %q", know.lastRecall.Situation)
	}
}

func TestEmptyLabelsEncodeAsArray(t *testing.T) {
	srv, _ := newToolHost(t)

	out, errMsg := callTool(t, srv, "ccs_valid_clara", "memoria_create", map[string]any{
		"title": "标题", "body": "正文",
	})
	if errMsg != "" {
		t.Fatal(errMsg)
	}
	// null 会让 agent 侧的 for 循环炸，而「没有标签」是完全正常的状态
	labels, ok := out["labels"]
	if !ok {
		t.Fatal("labels 字段应当存在")
	}
	if labels == nil {
		t.Fatal("空标签应编码成 [] 而不是 null")
	}
}

func TestGetReturnsLinksWithoutResolvingTitles(t *testing.T) {
	srv, _ := newToolHost(t)

	out, errMsg := callTool(t, srv, "ccs_valid_clara", "memoria_get", map[string]any{
		"id": "01MEMORYAAAAAAAAAAAAAAAAAA",
	})
	if errMsg != "" {
		t.Fatal(errMsg)
	}
	var m memoryOut
	decodeInto(t, out, &m)
	if len(m.Links) != 1 || m.Links[0].Kind != "related" {
		t.Fatalf("应带出关联，实际 %+v", m.Links)
	}
	// 对端可能落在无权访问的空间里，所以只给 ID 和 kind，不解析标题。
	// 想看内容就拿 ID 去 get，那里会撞上正常的权限检查。
	raw, _ := json.Marshal(out["links"])
	if strings.Contains(string(raw), "title") {
		t.Fatal("关联不该解析对端标题——对端可能在调用方无权访问的空间里")
	}
}

// ---------------------------------------------------------------------------
// 错误映射
// ---------------------------------------------------------------------------

func TestVersionConflictReportsCurrentVersion(t *testing.T) {
	srv, know := newToolHost(t)
	know.err = &knowledge.VersionConflictError{Expected: 3, Current: 7}

	_, errMsg := callTool(t, srv, "ccs_valid_clara", "memoria_update", map[string]any{
		"id": "01MEMORYAAAAAAAAAAAAAAAAAA", "title": "x", "version": 3,
	})
	if !strings.Contains(errMsg, codeConflict) {
		t.Fatalf("应返回 %s，实际: %s", codeConflict, errMsg)
	}
	// 只说「撞车了」的话，agent 还得再发一次 get 才能重试，
	// 而那次 get 之后可能再撞一次——重试就不收敛
	if !strings.Contains(errMsg, "7") {
		t.Fatalf("冲突响应必须带上当前版本号，实际: %s", errMsg)
	}
}

func TestNotFoundDoesNotDistinguishForbidden(t *testing.T) {
	srv, know := newToolHost(t)
	know.err = knowledge.ErrNotFound

	_, errMsg := callTool(t, srv, "ccs_valid_clara", "memoria_get", map[string]any{"id": "01XAAAAAAAAAAAAAAAAAAAAAAA"})
	if !strings.Contains(errMsg, codeNotFound) {
		t.Fatalf("应返回 %s，实际: %s", codeNotFound, errMsg)
	}
	// 区分「不存在」与「无权访问」等于让任何人都能用一个 ID
	// 探测出「这条内容存在于某个我看不见的空间」
	if strings.Contains(errMsg, "权限") || strings.Contains(errMsg, "forbidden") {
		t.Fatalf("不得暗示这是权限问题: %s", errMsg)
	}
}

func TestUnmappedErrorLeaksNothing(t *testing.T) {
	srv, know := newToolHost(t)
	know.err = errors.New("Error 1146: Table 'claracore_server.memories' doesn't exist")

	_, errMsg := callTool(t, srv, "ccs_valid_clara", "memoria_get", map[string]any{"id": "01XAAAAAAAAAAAAAAAAAAAAAAA"})
	if !strings.Contains(errMsg, codeInternal) {
		t.Fatalf("未识别的错误应收敛成 %s，实际: %s", codeInternal, errMsg)
	}
	// SQL 错误含表名列名，超时错误含内网地址——都不该进 agent 的上下文
	if strings.Contains(errMsg, "memories") || strings.Contains(errMsg, "1146") {
		t.Fatalf("错误原文泄露了内部结构: %s", errMsg)
	}
}

// ---------------------------------------------------------------------------
// promote / link / stats
// ---------------------------------------------------------------------------

func TestPromoteChecksBothEnds(t *testing.T) {
	srv, know := newToolHost(t)

	// 提升到项目空间（clara 是 owner，隐含可写）。团队空间在这套夹具里
	// 只可读不可写，正好用来验证下面那条拒绝用例。
	if _, errMsg := callTool(t, srv, "ccs_valid_clara", "promote", map[string]any{
		"entityId": "01MEMORYAAAAAAAAAAAAAAAAAA", "toSpace": spProj,
	}); errMsg != "" {
		t.Fatal(errMsg)
	}
	in := know.lastPromote
	// 只校目标写权限的话，拿到一个自己无权读的 ID 就能把它复制进
	// 一个自己能读的空间——一次完整的越权读取
	if len(in.ReadableSpaces) != 3 {
		t.Fatalf("源必须用可读集合定位，实际 %v", in.ReadableSpaces)
	}
	if in.TargetSpaceID != spProj {
		t.Fatalf("目标空间应是 %s，实际 %s", spProj, in.TargetSpaceID)
	}
	// 副本的作者是执行者，不是原作者：「谁决定把它摆到团队面前」是一份新责任
	if in.ActorUserID != userClar {
		t.Fatalf("执行者应来自 token，实际 %s", in.ActorUserID)
	}
}

func TestPromoteToUnwritableSpaceIsRejected(t *testing.T) {
	srv, know := newToolHost(t)

	// team 对所有人隐含可读，但写需要显式授权——这里两个用户都没有。
	_, errMsg := callTool(t, srv, "ccs_valid_clara", "promote", map[string]any{
		"entityId": "01MEMORYAAAAAAAAAAAAAAAAAA", "toSpace": spTeam,
	})
	if errMsg == "" {
		t.Fatal("对只读空间的提升必须被拒绝")
	}
	if know.calls != 0 {
		t.Fatal("权限没通过时不该调领域层")
	}
}

func TestLinkDefaultsToMemoryAndSupportsRemove(t *testing.T) {
	srv, know := newToolHost(t)

	if _, errMsg := callTool(t, srv, "ccs_valid_clara", "memoria_link", map[string]any{
		"fromId": "01AAAAAAAAAAAAAAAAAAAAAAAA", "toId": "01BBBBBBBBBBBBBBBBBBBBBBBB",
		"kind": "related",
	}); errMsg != "" {
		t.Fatal(errMsg)
	}
	if know.lastLink.From.Type != knowledge.EntityMemory || know.lastLink.To.Type != knowledge.EntityMemory {
		t.Fatalf("省略类型时默认 memory，实际 %+v → %+v", know.lastLink.From, know.lastLink.To)
	}
	// 建关联要求两端**可读**而非可写：关联不修改任何一端的内容
	if len(know.lastLink.ReadableSpaces) != 3 {
		t.Fatalf("应传可读集合，实际 %v", know.lastLink.ReadableSpaces)
	}

	out, errMsg := callTool(t, srv, "ccs_valid_clara", "memoria_link", map[string]any{
		"fromId": "01AAAAAAAAAAAAAAAAAAAAAAAA", "toId": "01BBBBBBBBBBBBBBBBBBBBBBBB",
		"kind": "related", "remove": true, "toType": "card",
	})
	if errMsg != "" {
		t.Fatal(errMsg)
	}
	if removed, _ := out["removed"].(bool); !removed {
		t.Fatal("remove=true 应走删除路径")
	}
	if know.lastLink.To.Type != knowledge.EntityCard {
		t.Fatalf("显式类型应生效，实际 %s", know.lastLink.To.Type)
	}
}

func TestSpaceStatsCoversReadableSpaces(t *testing.T) {
	srv, know := newToolHost(t)
	know.stats = []knowledge.SpaceStats{
		{SpaceID: spTeam, Cards: 10, DraftCards: 4, UnqualifiedCards: 6, RecallTriggers: 21},
	}

	out, errMsg := callTool(t, srv, "ccs_valid_clara", "space_stats", nil)
	if errMsg != "" {
		t.Fatal(errMsg)
	}
	if len(know.lastStatsSpaces) != 3 {
		t.Fatalf("不传 spaces 即统计全部可读空间，实际 %v", know.lastStatsSpaces)
	}
	var res spaceStatsOut
	decodeInto(t, out, &res)
	if len(res.Spaces) != 1 || res.Spaces[0].UnqualifiedCards != 6 {
		t.Fatalf("「还有多少张卡只是摘要」应透传，实际 %+v", res.Spaces)
	}
	if res.Spaces[0].Space.Name != "团队" {
		t.Fatalf("统计也要标注是哪个空间，实际 %+v", res.Spaces[0].Space)
	}
}

func TestServerStatusReportsSemanticSearch(t *testing.T) {
	srv, _ := newToolHost(t)

	out, errMsg := callTool(t, srv, "ccs_valid_clara", "server_status", nil)
	if errMsg != "" {
		t.Fatal(errMsg)
	}
	// 没有这个字段的话，「召回质量差」和「服务配错了」在 agent 眼里
	// 是同一件事
	if enabled, _ := out["semanticSearch"].(bool); !enabled {
		t.Fatalf("应如实报告是否接了语义检索，实际 %+v", out)
	}
}

func TestServerStatusReportsEmbeddingCacheCounters(t *testing.T) {
	srv, _, _ := newToolHostWithActivityAndContinuity(t, newFakeActivity(), HostOptions{
		Version: "test", SemanticSearch: true,
		EmbeddingCacheStats: func() (uint64, uint64, bool) { return 7, 3, true },
	})
	out, errMsg := callTool(t, srv, "ccs_valid_clara", "server_status", nil)
	if errMsg != "" {
		t.Fatal(errMsg)
	}
	cache, _ := out["embeddingCache"].(map[string]any)
	if cache["enabled"] != true || cache["hits"] != float64(7) || cache["misses"] != float64(3) {
		t.Fatalf("缓存状态不完整: %+v", out)
	}
}

func TestContextQualityStatusReportsSubmittedFeedbackWithoutClaimingAccuracy(t *testing.T) {
	since := time.Date(2026, 7, 29, 1, 2, 3, 0, time.UTC)
	activities := newFakeActivity()
	activities.quality = activity.ContextFeedbackQuality{
		Since: since, Selected: 11, Abstained: 19, FeedbackCoveredSelections: 6,
		AbstainReasons: activity.ContextAbstainReasons{
			NoRelevantEvidence: 9, Ambiguous: 4, SemanticUnavailable: 2,
			RetrievalTimeout: 1, SharedLineNotReady: 2, Other: 1,
		},
		Delivered: 2, Used: 3, Ignored: 1, Wrong: 1, Corrected: 1,
	}
	srv, _ := newToolHostWithActivity(t, activities)

	out, errMsg := callTool(t, srv, "ccs_valid_clara", "context_quality_status", nil)
	if errMsg != "" {
		t.Fatal(errMsg)
	}
	if out["submitted"] != float64(8) || out["selected"] != float64(11) || out["abstained"] != float64(19) || out["feedbackCoveredSelections"] != float64(6) || out["used"] != float64(3) || out["wrong"] != float64(1) {
		t.Fatalf("反馈计数不完整: %+v", out)
	}
	reasons, _ := out["abstainReasons"].(map[string]any)
	if reasons["noRelevantEvidence"] != float64(9) || reasons["ambiguous"] != float64(4) || reasons["semanticUnavailable"] != float64(2) || reasons["retrievalTimeout"] != float64(1) || reasons["sharedLineNotReady"] != float64(2) || reasons["other"] != float64(1) {
		t.Fatalf("放弃原因计数不完整: %+v", out)
	}
	if caveat, _ := out["caveat"].(string); !strings.Contains(caveat, "不是自动上下文准确率") {
		t.Fatalf("必须明确反馈计数不等于准确率: %+v", out)
	}
	activities.mu.Lock()
	tenant, window := activities.qualityTenant, activities.qualityWindow
	activities.mu.Unlock()
	if tenant != tenantA || window != 30*24*time.Hour {
		t.Fatalf("查询必须来自 token 租户并限制到 30 天: tenant=%s window=%s", tenant, window)
	}
}

func TestGatewayAutoContextSelectsExactlyOneDomain(t *testing.T) {
	t.Run("memory", func(t *testing.T) {
		srv, know := newToolHost(t)
		know.memory.Body = strings.Repeat("长", contextPrimaryTextRunes+500)
		know.search = knowledge.MemorySearchResult{
			Hits: []knowledge.MemoryHit{{Memory: know.memory, Score: 0.82, Semantic: true}}, SemanticAvailable: true,
		}
		know.recall = knowledge.CardSearchResult{SemanticAvailable: true}
		out, errMsg := callTool(t, srv, "ccs_valid_clara", "gateway_auto_context", map[string]any{"prompt": "连接池如何配置"})
		if errMsg != "" {
			t.Fatal(errMsg)
		}
		block, _ := out["block"].(map[string]any)
		if out["decision"] != "deliver_one" || block["domain"] != "memory" || block["id"] != string(know.memory.ID) {
			t.Fatalf("应交付唯一 Memory 块: %+v", out)
		}
		contextID, _ := out["contextId"].(string)
		if len(contextID) != 32 {
			t.Fatalf("每次交付必须带匿名关联 ID: %+v", out)
		}
		body, _ := block["body"].(string)
		if !strings.Contains(body, "[truncated]") || !strings.Contains(body, "terminal Agent") || len([]rune(body)) > contextPrimaryTextRunes+500 {
			t.Fatalf("Memory 证据必须有界且保留最终判断说明，长度=%d", len([]rune(body)))
		}
		if know.lastSearch.TimeView != "" || know.lastSearch.Query != "连接池如何配置" {
			t.Fatalf("Host 只传真实 prompt，current 约束由领域路径固定: %+v", know.lastSearch)
		}
	})

	t.Run("cross domain ambiguity", func(t *testing.T) {
		srv, know := newToolHost(t)
		know.search = knowledge.MemorySearchResult{
			Hits: []knowledge.MemoryHit{{Memory: know.memory, Score: 0.82, Semantic: true}}, SemanticAvailable: true,
		}
		know.recall = knowledge.CardSearchResult{
			Hits: []knowledge.CardHit{{Card: know.card, Score: 0.83, Semantic: true, Trigger: "连接池变更时"}}, SemanticAvailable: true,
		}
		out, errMsg := callTool(t, srv, "ccs_valid_clara", "gateway_auto_context", map[string]any{"prompt": "连接池如何配置"})
		if errMsg != "" {
			t.Fatal(errMsg)
		}
		if out["decision"] != "abstain" || out["reason"] != "ambiguous_domains" || out["block"] != nil || out["contextId"] != nil {
			t.Fatalf("两域都高置信时必须 fail closed: %+v", out)
		}
	})

	t.Run("explicit shared line", func(t *testing.T) {
		srv, know, cont := newToolHostWithActivityAndContinuity(t, newFakeActivity())
		out, errMsg := callTool(t, srv, "ccs_valid_clara", "gateway_auto_context", map[string]any{"lineId": "01LINEAAAAAAAAAAAAAAAAAAAA"})
		if errMsg != "" {
			t.Fatal(errMsg)
		}
		block, _ := out["block"].(map[string]any)
		if out["decision"] != "deliver_one" || block["domain"] != "shared_line" || cont.lastGet.LineID != "01LINEAAAAAAAAAAAAAAAAAAAA" {
			t.Fatalf("明确 lineId 应精确交付当前位置: %+v lastGet=%+v", out, cont.lastGet)
		}
		if contextID, _ := out["contextId"].(string); len(contextID) != 32 {
			t.Fatalf("显式 Shared Line 交付必须带匿名关联 ID: %+v", out)
		}
		if know.calls != 0 {
			t.Fatalf("显式 Shared Line 不应再竞争 Memory/Card，实际调用 %d 次", know.calls)
		}
	})
}

func TestGatewayContextFeedbackSupportsAllDeliveredDomains(t *testing.T) {
	srv, _, cont := newToolHostWithActivityAndContinuity(t, newFakeActivity())
	contextID := "ABCDEF0123456789ABCDEF0123456789"
	for _, tc := range []struct {
		domain string
		id     string
	}{
		{"memory", "01MEMORYAAAAAAAAAAAAAAAAAA"},
		{"shared_line", "01LINEAAAAAAAAAAAAAAAAAAAA"},
	} {
		out, errMsg := callTool(t, srv, "ccs_valid_clara", "gateway_context_feedback", map[string]any{"domain": tc.domain, "id": tc.id, "contextId": contextID, "outcome": "used"})
		if errMsg != "" {
			t.Fatalf("%s: %s", tc.domain, errMsg)
		}
		if out["domain"] != tc.domain || out["id"] != tc.id || out["contextId"] != strings.ToLower(contextID) || out["outcome"] != "used" {
			t.Fatalf("%s 反馈未按实体回显: %+v", tc.domain, out)
		}
	}
	if cont.lastGet.LineID != "01LINEAAAAAAAAAAAAAAAAAAAA" {
		t.Fatalf("Shared Line 反馈必须先验证租户内实体: %+v", cont.lastGet)
	}
	if _, errMsg := callTool(t, srv, "ccs_valid_clara", "gateway_context_feedback", map[string]any{"domain": "memory", "id": "01MEMORYAAAAAAAAAAAAAAAAAA", "contextId": "not-an-id", "outcome": "used"}); !strings.Contains(errMsg, codeInvalid) {
		t.Fatalf("畸形 contextId 必须被拒绝，实际 %q", errMsg)
	}
}

func TestCardCreateDoesNotDefaultStatusInHost(t *testing.T) {
	srv, know := newToolHost(t)

	if _, errMsg := callTool(t, srv, "ccs_valid_clara", "card_create", map[string]any{
		"title": "标题", "cardType": "协作安全", "distilled": "一句话",
	}); errMsg != "" {
		t.Fatal(errMsg)
	}
	// 「手写默认 active、摄取显式 draft」是业务规则，它在 CreateCard 里。
	// Host 在这儿补一个默认值就是把那条规则复制到了第二个地方。
	if know.lastCreateCard.Status != "" {
		t.Fatalf("Host 不该替领域层决定默认状态，实际传下去 %q", know.lastCreateCard.Status)
	}
}
