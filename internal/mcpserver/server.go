// Package mcpserver 把 memo 暴露成 MCP Streamable HTTP 服务，部署在 bosun 网关之后。
//
// 身份只认网关附带的 X-Bosun-User；它不是凭据，只有在 /mcp 要求网关令牌时才可信（契约 §9）。
package mcpserver

import (
	"bytes"
	"context"
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"log/slog"
	"net/http"
	"net/url"
	"regexp"
	"strings"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"claracore-server/internal/memo"
)

const ServiceName = "claracore"

const description = "团队 agent 的记忆与知识库：按调用人归属保存事实、决定、踩坑和长期笔记，可选共享给团队，按情境检索。"

const instructions = "两类条目：memory 是可观察的事实、决定、偏好、踩坑结论（短）；knowledge 是 agent 自己沉淀的长期笔记或文档（可长）。" +
	"开始一项任务前可用 memory_search(query=任务描述) 看有没有相关的旧记忆；用户说「记一下」或做出值得留存的决定时用 memory_save。" +
	"只写可观察的事实，不写推断。内容过时或有误时用 memory_supersede 取代，旧的保留可查，不删除。" +
	"条目默认只有写入人自己的 agent 能看到；shared=true 时团队所有人可见，但只有作者能取代。" +
	"检索结果正文默认截到 300 字，要全文用 memory_get(id)。结果里的 semantic 字段说明这次是否用上了语义检索。"

// Config 是 HTTP 外壳的设置。
type Config struct {
	Version string
	// Token 是网关的上游凭据（Authorization: Bearer）。空 = 不鉴权，只允许回环监听，身份头不可信。
	Token string
	// DevUser 仅在请求不带 X-Bosun-User 时使用，供本机直连调试。生产留空。
	DevUser string
}

type Server struct {
	svc *memo.Service
	cfg Config
	log *slog.Logger
	srv *sdk.Server
}

func New(svc *memo.Service, cfg Config, log *slog.Logger) *Server {
	s := &Server{svc: svc, cfg: cfg, log: log}
	s.srv = sdk.NewServer(&sdk.Implementation{Name: ServiceName, Version: cfg.Version},
		&sdk.ServerOptions{Instructions: instructions, Logger: quiet(log)})
	s.addTools()
	return s
}

// quiet 给 SDK 用：无状态模式下它每个请求都记两行连接日志，只留 Warn 以上。
func quiet(log *slog.Logger) *slog.Logger {
	return slog.New(levelFloor{log.Handler(), slog.LevelWarn})
}

type levelFloor struct {
	slog.Handler
	min slog.Level
}

func (h levelFloor) Enabled(ctx context.Context, l slog.Level) bool {
	return l >= h.min && h.Handler.Enabled(ctx, l)
}

func (h levelFloor) WithAttrs(a []slog.Attr) slog.Handler {
	return levelFloor{h.Handler.WithAttrs(a), h.min}
}

func (h levelFloor) WithGroup(name string) slog.Handler {
	return levelFloor{h.Handler.WithGroup(name), h.min}
}

// Handler 挂 /mcp 与 /health。/health 不需要令牌。
func (s *Server) Handler() http.Handler {
	mcp := sdk.NewStreamableHTTPHandler(func(*http.Request) *sdk.Server { return s.srv }, &sdk.StreamableHTTPOptions{
		Stateless:    true,
		JSONResponse: true,
		Logger:       quiet(s.log),
		// 设了令牌就由令牌把关（网关以内网地址访问，Host 不是回环）；没设令牌时保留 SDK 的 DNS rebinding 防护。
		DisableLocalhostProtection: s.cfg.Token != "",
	})
	mux := http.NewServeMux()
	mux.Handle("/mcp", s.requireToken(mcp))
	mux.HandleFunc("GET /health", s.health)
	return mux
}

func (s *Server) requireToken(next http.Handler) http.Handler {
	if s.cfg.Token == "" {
		return next
	}
	want := []byte("Bearer " + s.cfg.Token)
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if subtle.ConstantTimeCompare([]byte(r.Header.Get("Authorization")), want) != 1 {
			http.Error(w, `{"error":"unauthorized"}`, http.StatusUnauthorized)
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (s *Server) health(w http.ResponseWriter, r *http.Request) {
	out := map[string]any{"service": ServiceName, "version": s.cfg.Version, "contract": contractVersion}
	semantic := "off"
	if s.svc.SemanticConfigured() {
		semantic = "on"
	}
	out["semantic"] = semantic
	st, err := s.svc.Stats(r.Context())
	w.Header().Set("Content-Type", "application/json")
	if err != nil {
		s.log.Error("health: 数据库不可用", "err", err)
		out["status"], out["reason"] = "error", "database"
		w.WriteHeader(http.StatusServiceUnavailable)
	} else {
		out["status"], out["entries"], out["unindexed"] = "ok", st.Entries, st.Unindexed
	}
	_, _ = w.Write([]byte(compactJSON(out)))
}

// ---- 工具 ----

type handler func(ctx context.Context, caller string, args json.RawMessage) (*sdk.CallToolResult, error)

var requestIDPattern = regexp.MustCompile(`^[A-Za-z0-9._-]{1,64}$`)

func (s *Server) add(name, desc string, schema map[string]any, writes bool, h handler) {
	ann := &sdk.ToolAnnotations{ReadOnlyHint: !writes, IdempotentHint: !writes, DestructiveHint: ptr(false), OpenWorldHint: ptr(false)}
	s.srv.AddTool(&sdk.Tool{Name: name, Description: desc, InputSchema: schema, Annotations: ann},
		func(ctx context.Context, req *sdk.CallToolRequest) (*sdk.CallToolResult, error) {
			start := time.Now()
			var header http.Header
			if req.Extra != nil {
				header = req.Extra.Header
			}
			res := s.call(ctx, name, s.caller(header), req.Params.Arguments, h)
			outcome := "ok"
			if res.IsError {
				outcome = res.Meta["shipwright/error"].(map[string]string)["code"]
			}
			rid := header.Get("X-Request-Id")
			if !requestIDPattern.MatchString(rid) {
				rid = "-"
			}
			s.log.Info(fmt.Sprintf("tool %s %s %dms request_id=%s", name, outcome, time.Since(start).Milliseconds(), rid))
			return res, nil
		})
}

func (s *Server) call(ctx context.Context, name, caller string, args json.RawMessage, h handler) (res *sdk.CallToolResult) {
	defer func() {
		if p := recover(); p != nil {
			s.log.Error("工具 panic", "tool", name, "panic", p)
			res = errResult("INTERNAL", "panic")
		}
	}()
	res, err := h(ctx, caller, args)
	if err == nil {
		return res
	}
	var me *memo.Error
	if errors.As(err, &me) {
		return errResult(string(me.Code), me.Msg)
	}
	// 未预期的错误只给类型，原文进日志（契约 §5：异常信息里可能带数据）。
	s.log.Error("工具内部错误", "tool", name, "err", err)
	return errResult("INTERNAL", fmt.Sprintf("%T", err))
}

// caller 取网关附带的身份（UTF-8 百分号编码）；没有时退回 DevUser。
func (s *Server) caller(h http.Header) string {
	if raw := h.Get("X-Bosun-User"); raw != "" {
		if v, err := url.PathUnescape(raw); err == nil {
			return strings.TrimSpace(v)
		}
		return ""
	}
	return s.cfg.DevUser
}

// decode 严格解析参数：未知字段与类型错误都是 INVALID_ARGUMENT，不静默忽略。
func decode(args json.RawMessage, v any) error {
	if len(bytes.TrimSpace(args)) == 0 {
		args = []byte("{}")
	}
	dec := json.NewDecoder(bytes.NewReader(args))
	dec.DisallowUnknownFields()
	if err := dec.Decode(v); err != nil {
		return &memo.Error{Code: memo.CodeInvalid, Msg: "参数不合法：" + err.Error()}
	}
	return nil
}

func prop(typ, desc string) map[string]any { return map[string]any{"type": typ, "description": desc} }

func object(required []string, props map[string]any) map[string]any {
	o := map[string]any{"type": "object", "properties": props, "additionalProperties": false}
	if len(required) > 0 {
		o["required"] = required
	}
	return o
}

var (
	pKind   = map[string]any{"type": "string", "enum": []string{"memory", "knowledge"}, "description": "memory：事实、决定、踩坑结论（短）；knowledge：长期笔记或文档"}
	pTags   = map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "标签，最多 8 个，不含空白，如项目名或模块名"}
	pSource = prop("string", "来源，单行，如仓库、工单号或会话说明")
	pShared = prop("boolean", "true 时团队所有人可见；默认只有你自己")
)

const (
	searchDefaultChars = 300
	getDefaultChars    = 8000
	maxChars           = 50000
)

func (s *Server) addTools() {
	s.add("memory_save", "保存一条记忆或知识，归属于当前调用人。只写可观察的事实，不写推断。返回新条目及是否已建好语义索引。",
		object([]string{"title", "body"}, map[string]any{
			"title": prop("string", "一句话标题，最多 200 字"), "body": prop("string", "正文，最多 50000 字"),
			"kind": pKind, "tags": pTags, "source": pSource, "shared": pShared,
		}), true, s.save)

	s.add("memory_search", "按情境检索记忆与知识（文本 + 语义），不给 query 时按时间倒序列出最近的条目。正文按 max_chars 截断。",
		object(nil, map[string]any{
			"query":     prop("string", "要找什么，用自然语言描述情境效果最好；留空则列出最近条目"),
			"kind":      map[string]any{"type": "string", "enum": []string{"memory", "knowledge"}, "description": "只查一类；留空两类都查"},
			"tags":      map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "要求同时具有这些标签"},
			"scope":     map[string]any{"type": "string", "enum": []string{"all", "mine", "shared"}, "description": "all（默认，自己的 + 共享的）| mine | shared"},
			"limit":     map[string]any{"type": "integer", "minimum": 1, "maximum": 50, "description": "每页条数，默认 10，最多 50"},
			"offset":    map[string]any{"type": "integer", "minimum": 0, "description": "分页起点，默认 0；取下一页用上次返回的 next_offset"},
			"max_chars": map[string]any{"type": "integer", "minimum": 0, "maximum": maxChars, "description": "每条正文最多返回多少字，默认 300；0 表示不返回正文"},
		}), false, s.search)

	s.add("memory_get", "按 id 读取一条记忆或知识的全文，包括已被取代的条目（会给出 superseded_by）。",
		object([]string{"id"}, map[string]any{
			"id":        prop("string", "条目 id（memory_search / memory_save 返回）"),
			"max_chars": map[string]any{"type": "integer", "minimum": 1, "maximum": maxChars, "description": "正文最多返回多少字，默认 8000"},
		}), false, s.get)

	s.add("memory_supersede", "用新内容取代自己写的一条记忆，或只把它作废（不给 title/body 时 reason 必填）。旧条目保留可查，不删除。",
		object([]string{"id"}, map[string]any{
			"id":     prop("string", "要取代的条目 id，必须是你写的、仍有效的条目"),
			"reason": prop("string", "为什么取代或作废，最多 500 字"),
			"title":  prop("string", "新标题；留空沿用旧的"),
			"body":   prop("string", "新正文；留空沿用旧的。title 与 body 都留空表示只作废"),
			"tags":   map[string]any{"type": "array", "items": map[string]any{"type": "string"}, "description": "新标签；不传沿用旧的"},
			"source": prop("string", "新来源；不传沿用旧的"),
			"shared": prop("boolean", "新的共享设置；不传沿用旧的"),
		}), true, s.supersede)
}

func (s *Server) save(ctx context.Context, caller string, args json.RawMessage) (*sdk.CallToolResult, error) {
	var in struct {
		Title, Body, Kind, Source string
		Tags                      []string
		Shared                    bool
	}
	if err := decode(args, &in); err != nil {
		return nil, err
	}
	res, err := s.svc.Save(ctx, caller, memo.SaveInput{Kind: in.Kind, Title: in.Title, Body: in.Body, Tags: in.Tags, Source: in.Source, Shared: in.Shared})
	if err != nil {
		return nil, err
	}
	return okResult(res, nil), nil
}

func (s *Server) search(ctx context.Context, caller string, args json.RawMessage) (*sdk.CallToolResult, error) {
	in := struct {
		Query, Kind, Scope string
		Tags               []string
		Limit, Offset      int
		MaxChars           *int `json:"max_chars"`
	}{}
	if err := decode(args, &in); err != nil {
		return nil, err
	}
	chars := searchDefaultChars
	if in.MaxChars != nil {
		chars = *in.MaxChars
	}
	if chars < 0 || chars > maxChars {
		return nil, &memo.Error{Code: memo.CodeInvalid, Msg: fmt.Sprintf("max_chars 取值 0~%d", maxChars)}
	}
	res, err := s.svc.Search(ctx, caller, memo.SearchInput{Query: in.Query, Kind: in.Kind, Tags: in.Tags, Scope: in.Scope, Limit: in.Limit, Offset: in.Offset})
	if err != nil {
		return nil, err
	}

	var mores []more
	clipped, longest := 0, 0
	for i := range res.Items {
		if chars == 0 {
			res.Items[i].Body = ""
			continue
		}
		body, cut, n := memo.Clip(res.Items[i].Body, chars)
		if cut {
			res.Items[i].Body = body
			clipped++
			longest = max(longest, n)
		}
	}
	if res.NextOffset != nil {
		mores = append(mores, more{Kind: "items", Param: "offset", Count: 1,
			Hint:  fmt.Sprintf("还有更多结果，用 offset=%d 取下一页", *res.NextOffset),
			Shown: ptr(len(res.Items)), Total: ptr(res.Total), NextOffset: res.NextOffset})
	}
	if chars == 0 && len(res.Items) > 0 {
		mores = append(mores, more{Kind: "detail", Param: "max_chars", Count: 1, Omitted: []string{"body"},
			Hint: "未返回正文，调大 max_chars 或用 memory_get(id) 读取"})
	}
	if clipped > 0 {
		mores = append(mores, more{Kind: "text", Param: "max_chars", Field: "body", Count: clipped, Longest: ptr(longest),
			Hint: "正文已截断，用 memory_get(id) 读全文，或调大 max_chars"})
	}
	return okResult(res, mores), nil
}

func (s *Server) get(ctx context.Context, caller string, args json.RawMessage) (*sdk.CallToolResult, error) {
	in := struct {
		ID       string
		MaxChars *int `json:"max_chars"`
	}{}
	if err := decode(args, &in); err != nil {
		return nil, err
	}
	chars := getDefaultChars
	if in.MaxChars != nil {
		chars = *in.MaxChars
	}
	if chars < 1 || chars > maxChars {
		return nil, &memo.Error{Code: memo.CodeInvalid, Msg: fmt.Sprintf("max_chars 取值 1~%d", maxChars)}
	}
	e, err := s.svc.Get(ctx, caller, in.ID)
	if err != nil {
		return nil, err
	}
	var mores []more
	if body, cut, n := memo.Clip(e.Body, chars); cut {
		e.Body = body
		mores = append(mores, more{Kind: "text", Param: "max_chars", Field: "body", Count: 1, Longest: ptr(n),
			Hint: fmt.Sprintf("正文共 %d 字，调大 max_chars 读取更多", n)})
	}
	return okResult(e, mores), nil
}

func (s *Server) supersede(ctx context.Context, caller string, args json.RawMessage) (*sdk.CallToolResult, error) {
	in := struct {
		ID, Reason, Title, Body string
		Tags                    []string
		Source                  *string
		Shared                  *bool
	}{}
	if err := decode(args, &in); err != nil {
		return nil, err
	}
	res, err := s.svc.Supersede(ctx, caller, memo.SupersedeInput{ID: in.ID, Reason: in.Reason, Title: in.Title, Body: in.Body,
		Tags: in.Tags, Source: in.Source, Shared: in.Shared})
	if err != nil {
		return nil, err
	}
	return okResult(res, nil), nil
}
