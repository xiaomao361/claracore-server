package mcpserver

import (
	"bytes"
	"encoding/json"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// shipwright 契约 v1（/Volumes/E61/zhouwei/projects/shipwright/docs/CONTRACT.md）在 Go 侧的最小实现：
// 结果是一个紧凑 JSON 文本块；省略了内容就同时给 agent（_more）和网关（_meta）打标记；
// 失败一律 isError + 错误码。bosun 读 _meta 统计服务内省略与错误分类。
const contractVersion = 1

// more 是 _more 数组里的一项；字段名与取值约束见契约 §4.1。
type more struct {
	Kind       string   `json:"kind"`
	Param      string   `json:"param"`
	Hint       string   `json:"hint"`
	Count      int      `json:"count"`
	Field      string   `json:"field,omitempty"`
	Shown      *int     `json:"shown,omitempty"`
	Total      *int     `json:"total,omitempty"`
	NextOffset *int     `json:"next_offset,omitempty"`
	Longest    *int     `json:"longest,omitempty"`
	Omitted    []string `json:"omitted,omitempty"`
}

func compactJSON(v any) string {
	var buf bytes.Buffer
	enc := json.NewEncoder(&buf)
	enc.SetEscapeHTML(false)
	_ = enc.Encode(v)
	return string(bytes.TrimRight(buf.Bytes(), "\n"))
}

// okResult 输出成功结果。payload 必须是 JSON 对象（map 或 struct）；有省略时把 _more 并进去。
func okResult(payload any, mores []more) *sdk.CallToolResult {
	meta := sdk.Meta{"shipwright/contract": contractVersion}
	text := compactJSON(payload)
	if len(mores) > 0 {
		meta["shipwright/more"] = mores
		var obj map[string]json.RawMessage
		_ = json.Unmarshal([]byte(text), &obj)
		raw, _ := json.Marshal(mores)
		obj["_more"] = raw
		text = compactJSON(obj)
	}
	return &sdk.CallToolResult{Meta: meta, Content: []sdk.Content{&sdk.TextContent{Text: text}}}
}

func errResult(code, msg string) *sdk.CallToolResult {
	return &sdk.CallToolResult{
		Meta:    sdk.Meta{"shipwright/contract": contractVersion, "shipwright/error": map[string]string{"code": code}},
		Content: []sdk.Content{&sdk.TextContent{Text: code + ": " + msg}},
		IsError: true,
	}
}

func ptr[T any](v T) *T { return &v }
