package mcpserver

import (
	"encoding/json"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"claracore-server/internal/memo"
	"claracore-server/internal/platform"
)

// 这些测试走真实的 HTTP 外壳与 MCP SDK，检验网关能看到的那一面：鉴权、身份头、契约标记。

func newTestServer(t *testing.T, cfg Config) *httptest.Server {
	t.Helper()
	db, err := memo.Open(filepath.Join(t.TempDir(), "t.db"))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	log := slog.New(slog.NewTextHandler(io.Discard, nil))
	svc := memo.NewService(db, nil, platform.SystemClock{}, log)
	ts := httptest.NewServer(New(svc, cfg, log).Handler())
	t.Cleanup(ts.Close)
	return ts
}

type rpcResult struct {
	Meta    map[string]json.RawMessage `json:"_meta"`
	Content []struct{ Text string }    `json:"content"`
	IsError bool                       `json:"isError"`
	Tools   []struct {
		Name        string
		Annotations map[string]any
	} `json:"tools"`
}

func rpc(t *testing.T, ts *httptest.Server, headers map[string]string, method string, params any) (int, rpcResult) {
	t.Helper()
	body, _ := json.Marshal(map[string]any{"jsonrpc": "2.0", "id": 1, "method": method, "params": params})
	req, _ := http.NewRequest(http.MethodPost, ts.URL+"/mcp", strings.NewReader(string(body)))
	req.Header.Set("Content-Type", "application/json")
	req.Header.Set("Accept", "application/json, text/event-stream")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	resp, err := http.DefaultClient.Do(req)
	if err != nil {
		t.Fatal(err)
	}
	defer resp.Body.Close()
	raw, _ := io.ReadAll(resp.Body)
	if resp.StatusCode != http.StatusOK {
		return resp.StatusCode, rpcResult{}
	}
	var env struct {
		Result rpcResult
		Error  *struct{ Message string }
	}
	if err := json.Unmarshal(raw, &env); err != nil {
		t.Fatalf("响应不是 JSON: %s", raw)
	}
	if env.Error != nil {
		t.Fatalf("JSON-RPC 错误: %s", env.Error.Message)
	}
	return resp.StatusCode, env.Result
}

func call(t *testing.T, ts *httptest.Server, headers map[string]string, tool string, args any) rpcResult {
	t.Helper()
	_, r := rpc(t, ts, headers, "tools/call", map[string]any{"name": tool, "arguments": args})
	if len(r.Content) != 1 {
		t.Fatalf("%s 应当恰好一个文本块: %+v", tool, r)
	}
	if string(r.Meta["shipwright/contract"]) != "1" {
		t.Fatalf("%s 缺契约标记: %v", tool, r.Meta)
	}
	return r
}

func as(user string) map[string]string {
	return map[string]string{"Authorization": "Bearer gw", "X-Bosun-User": user}
}

func TestTokenAndHealth(t *testing.T) {
	ts := newTestServer(t, Config{Version: "test", Token: "gw"})
	if code, _ := rpc(t, ts, map[string]string{"Authorization": "Bearer wrong"}, "tools/list", map[string]any{}); code != http.StatusUnauthorized {
		t.Fatalf("错误令牌应当 401，得到 %d", code)
	}
	resp, err := http.Get(ts.URL + "/health")
	if err != nil {
		t.Fatal(err)
	}
	var h map[string]any
	json.NewDecoder(resp.Body).Decode(&h)
	resp.Body.Close()
	if resp.StatusCode != 200 || h["status"] != "ok" || h["contract"] != float64(1) || h["semantic"] != "off" {
		t.Fatalf("health 不需要令牌且应报 ok: %d %v", resp.StatusCode, h)
	}
}

func TestToolsAndAnnotations(t *testing.T) {
	ts := newTestServer(t, Config{Token: "gw"})
	_, r := rpc(t, ts, as("a"), "tools/list", map[string]any{})
	readOnly := map[string]bool{}
	for _, tool := range r.Tools {
		readOnly[tool.Name] = tool.Annotations["readOnlyHint"] == true
	}
	want := map[string]bool{"memory_save": false, "memory_search": true, "memory_get": true, "memory_supersede": false}
	if len(readOnly) != len(want) {
		t.Fatalf("工具集: %v", readOnly)
	}
	for name, ro := range want {
		if got, ok := readOnly[name]; !ok || got != ro {
			t.Errorf("%s readOnlyHint=%v，期望 %v", name, got, ro)
		}
	}
}

func TestIdentityFromGateway(t *testing.T) {
	ts := newTestServer(t, Config{Token: "gw"})
	// 身份是百分号编码的 UTF-8。
	saved := call(t, ts, as("%E5%BC%A0%E4%B8%89"), "memory_save", map[string]any{"title": "私事", "body": "只给张三"})
	var out struct{ Entry memo.Entry }
	json.Unmarshal([]byte(saved.Content[0].Text), &out)
	if out.Entry.Owner != "张三" {
		t.Fatalf("owner 应为解码后的身份: %q", out.Entry.Owner)
	}

	r := call(t, ts, as("lisi"), "memory_get", map[string]any{"id": out.Entry.ID})
	if !r.IsError || !strings.HasPrefix(r.Content[0].Text, "NOT_FOUND:") || !strings.Contains(string(r.Meta["shipwright/error"]), "NOT_FOUND") {
		t.Fatalf("别人的私有条目应 NOT_FOUND: %+v", r)
	}

	r = call(t, ts, map[string]string{"Authorization": "Bearer gw"}, "memory_search", map[string]any{})
	if !r.IsError || !strings.HasPrefix(r.Content[0].Text, "PERMISSION_DENIED:") {
		t.Fatalf("不经网关（无身份头）应 PERMISSION_DENIED: %+v", r)
	}
}

func TestContractMarkersAndErrors(t *testing.T) {
	ts := newTestServer(t, Config{Token: "gw"})
	long := strings.Repeat("长", 400)
	for i := 0; i < 3; i++ {
		call(t, ts, as("a"), "memory_save", map[string]any{"title": "条目", "body": long})
	}
	r := call(t, ts, as("a"), "memory_search", map[string]any{"limit": 2})
	var page struct {
		Items      []json.RawMessage
		Total      int
		NextOffset *int             `json:"next_offset"`
		More       []map[string]any `json:"_more"`
	}
	json.Unmarshal([]byte(r.Content[0].Text), &page)
	if len(page.Items) != 2 || page.Total != 3 || page.NextOffset == nil || *page.NextOffset != 2 {
		t.Fatalf("分页结构: %s", r.Content[0].Text)
	}
	kinds := map[string]bool{}
	for _, m := range page.More {
		kinds[m["kind"].(string)] = true
	}
	if !kinds["items"] || !kinds["text"] || r.Meta["shipwright/more"] == nil {
		t.Fatalf("应同时标出分页与正文截断（正文和 _meta 各一份）: %s / %v", r.Content[0].Text, r.Meta)
	}

	r = call(t, ts, as("a"), "memory_save", map[string]any{"title": "x", "body": "y", "colour": "red"})
	if !r.IsError || !strings.HasPrefix(r.Content[0].Text, "INVALID_ARGUMENT:") {
		t.Fatalf("未知参数应 INVALID_ARGUMENT: %+v", r)
	}
	r = call(t, ts, as("a"), "memory_search", map[string]any{"limit": "ten"})
	if !r.IsError || !strings.HasPrefix(r.Content[0].Text, "INVALID_ARGUMENT:") {
		t.Fatalf("类型错误应 INVALID_ARGUMENT: %+v", r)
	}
}
