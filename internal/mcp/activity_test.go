package mcp

import (
	"context"
	"encoding/json"
	"errors"
	"go/ast"
	"go/parser"
	"go/token"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"sync"
	"testing"

	"claracore-server/internal/domain/activity"
	"claracore-server/internal/domain/knowledge"
	"claracore-server/internal/platform"
)

type fakeActivity struct {
	mu      sync.Mutex
	records []activity.RecordInput
	err     error
}

func newFakeActivity() *fakeActivity { return &fakeActivity{} }

func (f *fakeActivity) Record(_ context.Context, in activity.RecordInput) (activity.Event, error) {
	f.mu.Lock()
	defer f.mu.Unlock()
	in.AgentID = cloneActivityID(in.AgentID)
	in.EntityID = cloneActivityID(in.EntityID)
	in.SpaceID = cloneActivityID(in.SpaceID)
	in.Metadata = append(json.RawMessage(nil), in.Metadata...)
	f.records = append(f.records, in)
	return activity.Event{}, f.err
}

func (f *fakeActivity) snapshot() []activity.RecordInput {
	f.mu.Lock()
	defer f.mu.Unlock()
	out := make([]activity.RecordInput, len(f.records))
	copy(out, f.records)
	return out
}

func cloneActivityID(in *platform.ID) *platform.ID {
	if in == nil {
		return nil
	}
	v := *in
	return &v
}

func recordForAction(t *testing.T, records []activity.RecordInput, action string) activity.RecordInput {
	t.Helper()
	for _, record := range records {
		if record.Action == action {
			return record
		}
	}
	t.Fatalf("没有记录工具 %s 的活动", action)
	return activity.RecordInput{}
}

func decodeActivityMetadata(t *testing.T, record activity.RecordInput, dst any) {
	t.Helper()
	if len(record.Metadata) == 0 {
		t.Fatalf("工具 %s 没有 metadata", record.Action)
	}
	if err := json.Unmarshal(record.Metadata, dst); err != nil {
		t.Fatalf("工具 %s 的 metadata 不是合法 JSON: %v", record.Action, err)
	}
}

func TestEveryRegisteredToolIsAuditedWithClassification(t *testing.T) {
	activities := newFakeActivity()
	srv, _ := newToolHostWithActivity(t, activities)

	calls := []struct {
		name string
		args map[string]any
	}{
		{"whoami", nil},
		{"server_status", nil},
		{"memoria_create", map[string]any{"title": "标题", "body": "正文"}},
		{"memoria_search", map[string]any{"query": "连接池"}},
		{"memoria_get", map[string]any{"id": "01MEMORYAAAAAAAAAAAAAAAAAA"}},
		{"memoria_update", map[string]any{"id": "01MEMORYAAAAAAAAAAAAAAAAAA", "title": "新标题"}},
		{"memoria_delete", map[string]any{"id": "01MEMORYAAAAAAAAAAAAAAAAAA"}},
		{"memoria_link", map[string]any{
			"fromId": "01MEMORYAAAAAAAAAAAAAAAAAA",
			"toId":   "01CARDAAAAAAAAAAAAAAAAAAAA",
			"toType": "card",
			"kind":   "related",
		}},
		{"card_create", map[string]any{"title": "标题", "cardType": "工程约定"}},
		{"card_recall", map[string]any{"situation": "准备改动连接池"}},
		{"card_search", map[string]any{"query": "连接池"}},
		{"card_get", map[string]any{"id": "01CARDAAAAAAAAAAAAAAAAAAAA"}},
		{"card_update", map[string]any{"id": "01CARDAAAAAAAAAAAAAAAAAAAA", "title": "新标题"}},
		{"space_list", nil},
		{"promote", map[string]any{
			"entityType": "memory",
			"entityId":   "01MEMORYAAAAAAAAAAAAAAAAAA",
			"toSpace":    spProj,
		}},
		{"space_stats", nil},
	}

	for _, call := range calls {
		if _, errMsg := callTool(t, srv, "ccs_valid_clara", call.name, call.args); errMsg != "" {
			t.Fatalf("工具 %s 调用失败: %s", call.name, errMsg)
		}
	}

	records := activities.snapshot()
	if len(records) != len(calls) {
		t.Fatalf("16 个工具应各记一次活动，实际 calls=%d records=%d", len(calls), len(records))
	}

	writes := map[string]bool{
		"memoria_create": true,
		"memoria_update": true,
		"memoria_delete": true,
		"memoria_link":   true,
		"card_create":    true,
		"card_update":    true,
		"promote":        true,
	}
	seen := make(map[string]bool, len(records))
	for _, record := range records {
		if seen[record.Action] {
			t.Errorf("工具 %s 被重复记录", record.Action)
		}
		seen[record.Action] = true
		wantKind := activity.KindTrace
		if record.Action == "promote" {
			wantKind = activity.KindGovernance
		}
		if record.Source != activity.SourceMCP || record.Kind != wantKind {
			t.Errorf("工具 %s 的 source/kind 错误: %s/%s", record.Action, record.Source, record.Kind)
		}
		wantAccess := activity.AccessRead
		if writes[record.Action] {
			wantAccess = activity.AccessWrite
		}
		if record.Access != wantAccess {
			t.Errorf("工具 %s 的读写分类应为 %s，实际 %s", record.Action, wantAccess, record.Access)
		}
		if record.Outcome != activity.OutcomeSuccess {
			t.Errorf("工具 %s 成功调用应记 success，实际 %s", record.Action, record.Outcome)
		}
		if record.TenantID != tenantA || record.UserID != userClar {
			t.Errorf("工具 %s 的 actor 不来自认证身份: tenant=%s user=%s", record.Action, record.TenantID, record.UserID)
		}
		if record.AgentID == nil || *record.AgentID != agentClara {
			t.Errorf("工具 %s 没有记录认证 token 对应的 agent", record.Action)
		}
	}
	for _, call := range calls {
		if !seen[call.name] {
			t.Errorf("工具 %s 没有活动记录", call.name)
		}
	}
}

func TestAuditFailurePreservesToolSuccessAndToolError(t *testing.T) {
	activities := newFakeActivity()
	activities.err = errors.New("audit store unavailable")
	srv, know := newToolHostWithActivity(t, activities)

	out, errMsg := callTool(t, srv, "ccs_valid_clara", "memoria_create", map[string]any{
		"title": "标题",
		"body":  "正文",
	})
	if errMsg != "" {
		t.Fatalf("业务写已成功时，审计失败不能改成工具失败: %s", errMsg)
	}
	var memory memoryOut
	decodeInto(t, out, &memory)
	if memory.ID != "01MEMORYAAAAAAAAAAAAAAAAAA" {
		t.Fatalf("审计失败改变了原工具结果: %+v", memory)
	}

	know.err = errors.New("database address must stay server-side")
	_, errMsg = callTool(t, srv, "ccs_valid_clara", "memoria_get", map[string]any{
		"id": "01MEMORYAAAAAAAAAAAAAAAAAA",
	})
	if !strings.Contains(errMsg, codeInternal) {
		t.Fatalf("审计失败不应覆盖 handler 原本的协议错误，实际 %q", errMsg)
	}
}

func TestAuditMetadataNeverContainsContentQueryTokenOrError(t *testing.T) {
	activities := newFakeActivity()
	srv, know := newToolHostWithActivity(t, activities)

	secrets := []string{
		"SECRET_TITLE_7f0e",
		"SECRET_BODY_8e1f",
		"SECRET_QUERY_9a2b",
		"SECRET_SITUATION_a3c4",
		"SECRET_NOTE_b5d6",
		"SECRET_TRIGGER_c7e8",
		"SECRET_LABEL_d9f0",
		"SECRET_ERROR_e1a2",
		"ccs_valid_clara",
	}

	if _, errMsg := callTool(t, srv, "ccs_valid_clara", "memoria_create", map[string]any{
		"title":  secrets[0],
		"body":   secrets[1],
		"labels": []string{secrets[6]},
	}); errMsg != "" {
		t.Fatal(errMsg)
	}
	if _, errMsg := callTool(t, srv, "ccs_valid_clara", "memoria_search", map[string]any{
		"query": secrets[2], "labels": []string{secrets[6]},
	}); errMsg != "" {
		t.Fatal(errMsg)
	}
	if _, errMsg := callTool(t, srv, "ccs_valid_clara", "card_create", map[string]any{
		"title":          secrets[0],
		"cardType":       "工程约定",
		"source":         secrets[1],
		"original":       secrets[1],
		"distilled":      secrets[1],
		"understanding":  secrets[1],
		"whyKeep":        secrets[1],
		"unresolved":     secrets[1],
		"recallTriggers": []string{secrets[5]},
		"labels":         []string{secrets[6]},
	}); errMsg != "" {
		t.Fatal(errMsg)
	}
	if _, errMsg := callTool(t, srv, "ccs_valid_clara", "card_recall", map[string]any{
		"situation": secrets[3],
	}); errMsg != "" {
		t.Fatal(errMsg)
	}
	if _, errMsg := callTool(t, srv, "ccs_valid_clara", "memoria_link", map[string]any{
		"fromId": "01MEMORYAAAAAAAAAAAAAAAAAA",
		"toId":   "01CARDAAAAAAAAAAAAAAAAAAAA",
		"toType": "card",
		"kind":   "related",
		"note":   secrets[4],
	}); errMsg != "" {
		t.Fatal(errMsg)
	}

	know.err = errors.New(secrets[7])
	if _, errMsg := callTool(t, srv, "ccs_valid_clara", "memoria_get", map[string]any{
		"id": "01MEMORYAAAAAAAAAAAAAAAAAA",
	}); errMsg == "" {
		t.Fatal("领域错误应映射成工具错误")
	}

	raw, err := json.Marshal(activities.snapshot())
	if err != nil {
		t.Fatal(err)
	}
	for _, secret := range secrets {
		if strings.Contains(string(raw), secret) {
			t.Errorf("活动记录泄露了私密输入/凭据/错误文本 %q: %s", secret, raw)
		}
	}
	failed := recordForAction(t, activities.snapshot(), "memoria_get")
	if failed.Outcome != activity.OutcomeError {
		t.Fatalf("失败的工具调用应记 error outcome，实际 %s", failed.Outcome)
	}
}

func TestLinkAuditDropsNonIDEndpointText(t *testing.T) {
	activities := newFakeActivity()
	srv, _ := newToolHostWithActivity(t, activities)
	secret := strings.Repeat("PRIVATE_FREE_TEXT_", 300)
	if _, errMsg := callTool(t, srv, "ccs_valid_clara", "memoria_link", map[string]any{
		"fromId": secret,
		"toId":   "also-not-an-id",
		"kind":   "related",
	}); errMsg != "" {
		t.Fatalf("夹具允许调用完成，不影响本测试的审计边界: %s", errMsg)
	}
	record := recordForAction(t, activities.snapshot(), "memoria_link")
	if strings.Contains(string(record.Metadata), "PRIVATE_FREE_TEXT") || len(record.Metadata) != 0 {
		t.Fatalf("非 ID 自由文本不得进入 link metadata: %s", record.Metadata)
	}
}

func TestSearchAndRecallAuditOnlyResultIDsCountsAndSemanticFlags(t *testing.T) {
	activities := newFakeActivity()
	srv, know := newToolHostWithActivity(t, activities)

	know.search = knowledge.MemorySearchResult{
		Hits:              []knowledge.MemoryHit{{Memory: know.memory, Score: 0.81, Semantic: true}},
		SemanticAvailable: true,
		CardMatches:       2,
	}
	know.recall = knowledge.CardSearchResult{
		Hits:              []knowledge.CardHit{{Card: know.card, Score: 0.73, Semantic: true, Trigger: "不应进入活动流"}},
		SemanticAvailable: true,
	}

	for _, call := range []struct {
		name string
		args map[string]any
	}{
		{"memoria_search", map[string]any{"query": "私密查询"}},
		{"card_search", map[string]any{"query": "私密查询"}},
		{"card_recall", map[string]any{"situation": "私密工作情境"}},
	} {
		if _, errMsg := callTool(t, srv, "ccs_valid_clara", call.name, call.args); errMsg != "" {
			t.Fatalf("%s: %s", call.name, errMsg)
		}
	}

	type hitMetadata struct {
		HitIDs            []string `json:"hitIds"`
		HitCount          int      `json:"hitCount"`
		SemanticHitCount  int      `json:"semanticHitCount"`
		SemanticAvailable bool     `json:"semanticAvailable"`
		CardMatches       int      `json:"cardMatches"`
	}
	records := activities.snapshot()
	for _, action := range []string{"memoria_search", "card_search", "card_recall"} {
		record := recordForAction(t, records, action)
		var meta hitMetadata
		decodeActivityMetadata(t, record, &meta)
		wantID := string(know.card.ID)
		if action == "memoria_search" {
			wantID = string(know.memory.ID)
			if meta.CardMatches != 2 {
				t.Errorf("memoria_search 应记录 cardMatches=2，实际 %d", meta.CardMatches)
			}
		}
		if len(meta.HitIDs) != 1 || meta.HitIDs[0] != wantID || meta.HitCount != 1 {
			t.Errorf("%s 的命中 ID/计数错误: %+v", action, meta)
		}
		if meta.SemanticHitCount != 1 || !meta.SemanticAvailable {
			t.Errorf("%s 的语义标记错误: %+v", action, meta)
		}
		if bytes := string(record.Metadata); strings.Contains(bytes, "私密") || strings.Contains(bytes, "不应进入活动流") {
			t.Errorf("%s 的 metadata 带入了 query/situation/trigger: %s", action, bytes)
		}
	}
}

func TestSharedWritesCaptureOutputEntityAndSpace(t *testing.T) {
	activities := newFakeActivity()
	srv, know := newToolHostWithActivity(t, activities)
	know.memory.SpaceID = spProj
	know.card.SpaceID = spProj

	calls := []struct {
		name string
		args map[string]any
	}{
		{"memoria_create", map[string]any{"title": "标题", "body": "正文", "space": spProj}},
		{"memoria_update", map[string]any{"id": string(know.memory.ID), "title": "新标题"}},
		{"card_create", map[string]any{"title": "标题", "cardType": "工程约定", "space": spProj}},
		{"card_update", map[string]any{"id": string(know.card.ID), "title": "新标题"}},
		{"promote", map[string]any{"entityType": "memory", "entityId": string(know.memory.ID), "toSpace": spProj}},
	}
	for _, call := range calls {
		if _, errMsg := callTool(t, srv, "ccs_valid_clara", call.name, call.args); errMsg != "" {
			t.Fatalf("%s: %s", call.name, errMsg)
		}
	}

	records := activities.snapshot()
	for _, call := range calls {
		record := recordForAction(t, records, call.name)
		if record.SpaceID == nil || *record.SpaceID != spProj {
			t.Errorf("共享写 %s 应从输出记录项目空间 %s，实际 %v", call.name, spProj, record.SpaceID)
		}
		if record.EntityType == "" || record.EntityID == nil || *record.EntityID == "" {
			t.Errorf("共享写 %s 应从输出记录目标实体，实际 type=%q id=%v", call.name, record.EntityType, record.EntityID)
		}
	}
}

func TestMemoryDeleteAuditCarriesResolvedSpace(t *testing.T) {
	activities := newFakeActivity()
	srv, know := newToolHostWithActivity(t, activities)
	know.memory.SpaceID = spProj
	const id = "01MEMORYAAAAAAAAAAAAAAAAAA"
	if _, errMsg := callTool(t, srv, "ccs_valid_clara", "memoria_delete", map[string]any{"id": id}); errMsg != "" {
		t.Fatal(errMsg)
	}
	record := recordForAction(t, activities.snapshot(), "memoria_delete")
	if record.EntityType != knowledge.EntityMemory || record.EntityID == nil || *record.EntityID != id {
		t.Fatalf("delete 应安全记录 memory ID，实际 type=%q id=%v", record.EntityType, record.EntityID)
	}
	if record.SpaceID == nil || *record.SpaceID != spProj {
		t.Fatalf("共享记忆的软删除应带领域层解析的空间 %s，实际 %v", spProj, record.SpaceID)
	}
}

func TestToolRegistrationCannotBypassAuditWrapper(t *testing.T) {
	_, thisFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("无法定位测试文件")
	}
	dir := filepath.Dir(thisFile)
	entries, err := os.ReadDir(dir)
	if err != nil {
		t.Fatal(err)
	}

	fset := token.NewFileSet()
	registrations := 0
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".go") || strings.HasSuffix(entry.Name(), "_test.go") {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		file, err := parser.ParseFile(fset, path, nil, 0)
		if err != nil {
			t.Fatalf("解析 %s: %v", path, err)
		}
		for _, imp := range file.Imports {
			importPath, err := strconv.Unquote(imp.Path.Value)
			if err != nil {
				t.Fatal(err)
			}
			if importPath == "github.com/modelcontextprotocol/go-sdk/mcp" && imp.Name != nil && imp.Name.Name == "." {
				t.Errorf("%s 不得 dot-import MCP SDK，否则可绕过 AddTool 审计检查", path)
			}
		}
		for _, decl := range file.Decls {
			fn, ok := decl.(*ast.FuncDecl)
			if !ok || fn.Body == nil {
				continue
			}
			ast.Inspect(fn.Body, func(node ast.Node) bool {
				sel, ok := node.(*ast.SelectorExpr)
				if !ok || sel.Sel.Name != "AddTool" {
					return true
				}
				registrations++
				if fn.Name.Name != "addAuditedTool" {
					pos := fset.Position(sel.Pos())
					t.Errorf("%s:%d 直接注册工具会绕过活动审计；必须走 addAuditedTool", pos.Filename, pos.Line)
				}
				return true
			})
		}
	}
	if registrations != 1 {
		t.Fatalf("生产代码应只有 addAuditedTool 内部一个 SDK AddTool 落点，实际 %d 个", registrations)
	}
}
