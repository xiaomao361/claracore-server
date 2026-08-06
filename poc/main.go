// ClaraCore Server PoC
//
// 验证目标（仅此四项，不做任何多余的事）：
//  1. MCP Streamable HTTP + Stateless = true 能否正常服务
//  2. bearer token 认证链路是否可用（auth.RequireBearerToken）
//  3. token → (tenant, user, agent) 的身份能否传递到 tool handler
//     —— 即「token 即身份」能否落地，不依赖客户端声明的 header
//  4. Claude Code 实际连接时协议版本能否协商成功
//
// 这是丢弃型代码，不是 05-code-structure.md 的正式结构。
package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"log"
	"log/slog"
	"net/http"
	"os"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	"github.com/modelcontextprotocol/go-sdk/mcp"
)

// ---------------------------------------------------------------------------
// 身份模型（真实实现中来自数据库 tokens 表）
// ---------------------------------------------------------------------------

type identity struct {
	TenantID string
	UserID   string
	AgentID  string
	Spaces   []string
}

// 模拟 tokens 表：一个 token = 一个 agent，同一 user 可有多个 agent
var tokenTable = map[string]identity{
	"ccs_poc_clara_001": {
		TenantID: "t_demo", UserID: "u_zhouwei", AgentID: "clara",
		Spaces: []string{"personal:zhouwei", "team:demo"},
	},
	"ccs_poc_codex_002": {
		TenantID: "t_demo", UserID: "u_zhouwei", AgentID: "codex",
		Spaces: []string{"personal:zhouwei", "team:demo"},
	},
	"ccs_poc_other_003": {
		TenantID: "t_other", UserID: "u_someone", AgentID: "clara",
		Spaces: []string{"personal:someone"},
	},
}

// contextKey 用于把身份注入请求 context
type contextKey struct{}

var identityKey = contextKey{}

// ---------------------------------------------------------------------------
// 认证：TokenVerifier —— 「token 即身份」的落点
// ---------------------------------------------------------------------------

func verifyToken(ctx context.Context, token string, req *http.Request) (*auth.TokenInfo, error) {
	id, ok := tokenTable[token]
	if !ok {
		return nil, auth.ErrInvalidToken
	}
	// 中间件默认要求 Expiration 非零，否则拒绝
	return &auth.TokenInfo{
		UserID:     id.UserID,
		Expiration: time.Now().Add(time.Hour),
		Extra: map[string]any{
			"tenant_id": id.TenantID,
			"agent_id":  id.AgentID,
			"spaces":    id.Spaces,
		},
	}, nil
}

// injectIdentity 把已验证的身份放进请求 context，供 getServer 与 tool handler 使用。
// 注意：身份只来自 token，绝不读取客户端声明的 header（如 Desktop 的 X-ClaraCore-Agent-ID）。
func injectIdentity(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		info := auth.TokenInfoFromContext(r.Context())
		if info == nil {
			http.Error(w, "no token info", http.StatusUnauthorized)
			return
		}
		id := identity{
			UserID:   info.UserID,
			TenantID: asString(info.Extra["tenant_id"]),
			AgentID:  asString(info.Extra["agent_id"]),
			Spaces:   asStrings(info.Extra["spaces"]),
		}
		ctx := context.WithValue(r.Context(), identityKey, id)
		next.ServeHTTP(w, r.WithContext(ctx))
	})
}

func identityFrom(ctx context.Context) (identity, bool) {
	id, ok := ctx.Value(identityKey).(identity)
	return id, ok
}

// logRequest 记录每个 MCP 请求的方法与协议版本，用于观察真实客户端行为。
func logRequest(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, err := io.ReadAll(io.LimitReader(r.Body, 1<<20))
		if err == nil {
			r.Body = io.NopCloser(bytes.NewReader(body))
			var probe struct {
				Method string `json:"method"`
				Params struct {
					ProtocolVersion string `json:"protocolVersion"`
					ClientInfo      struct {
						Name    string `json:"name"`
						Version string `json:"version"`
					} `json:"clientInfo"`
				} `json:"params"`
				Meta map[string]any `json:"_meta"`
			}
			if json.Unmarshal(body, &probe) == nil && probe.Method != "" {
				log.Printf("[REQ] method=%s protocol=%q client=%s/%s meta=%v ua=%q",
					probe.Method,
					probe.Params.ProtocolVersion,
					probe.Params.ClientInfo.Name, probe.Params.ClientInfo.Version,
					probe.Meta != nil,
					r.Header.Get("User-Agent"))
			}
		}
		next.ServeHTTP(w, r)
	})
}

// ---------------------------------------------------------------------------
// 工具
// ---------------------------------------------------------------------------

type whoamiInput struct{}

type whoamiOutput struct {
	TenantID        string   `json:"tenantId"`
	UserID          string   `json:"userId"`
	AgentID         string   `json:"agentId"`
	ReadableSpaces  []string `json:"readableSpaces"`
	ProtocolVersion string   `json:"protocolVersion"`
	Stateless       bool     `json:"stateless"`
}

func handleWhoAmI(ctx context.Context, req *mcp.CallToolRequest, in whoamiInput) (*mcp.CallToolResult, whoamiOutput, error) {
	id, ok := identityFrom(ctx)
	if !ok {
		// 若走到这里，说明 HTTP 中间件的 context 没能传递到 tool handler
		return nil, whoamiOutput{}, fmt.Errorf("identity not found in context — 身份传递链路断了")
	}

	protocol := "(unknown)"
	if req != nil && req.Session != nil {
		if v := req.Session.InitializeParams(); v != nil {
			protocol = v.ProtocolVersion
		}
	}

	return nil, whoamiOutput{
		TenantID:        id.TenantID,
		UserID:          id.UserID,
		AgentID:         id.AgentID,
		ReadableSpaces:  id.Spaces,
		ProtocolVersion: protocol,
		Stateless:       true,
	}, nil
}

// echo 用于确认基本调用链路
type echoInput struct {
	Text string `json:"text" jsonschema:"要回显的文本"`
}

type echoOutput struct {
	Echo    string `json:"echo"`
	CalledBy string `json:"calledBy"`
}

func handleEcho(ctx context.Context, req *mcp.CallToolRequest, in echoInput) (*mcp.CallToolResult, echoOutput, error) {
	id, _ := identityFrom(ctx)
	return nil, echoOutput{
		Echo:     in.Text,
		CalledBy: fmt.Sprintf("%s/%s", id.UserID, id.AgentID),
	}, nil
}

// ---------------------------------------------------------------------------
// 装配
// ---------------------------------------------------------------------------

func newServer() *mcp.Server {
	s := mcp.NewServer(&mcp.Implementation{
		Name:    "claracore-server-poc",
		Version: "0.0.1",
	}, nil)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "whoami",
		Description: "返回当前 token 解析出的身份与可读空间。验证「token 即身份」链路。",
	}, handleWhoAmI)

	mcp.AddTool(s, &mcp.Tool{
		Name:        "echo",
		Description: "回显文本，并标注调用者身份。",
	}, handleEcho)

	return s
}

func main() {
	addr := os.Getenv("POC_ADDR")
	if addr == "" {
		addr = "127.0.0.1:51888"
	}

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelDebug}))

	// 每个请求都可以拿到 *http.Request，因此可按身份返回不同 server 实例。
	// PoC 中所有请求共用一个 server（工具集相同），身份走 context 传递。
	handler := mcp.NewStreamableHTTPHandler(
		func(r *http.Request) *mcp.Server { return newServer() },
		&mcp.StreamableHTTPOptions{
			Stateless: true, // ← 本次验证的核心
			Logger:    logger,
		},
	)

	// 中间件链：RequireBearerToken → injectIdentity → MCP handler
	authed := logRequest(
		auth.RequireBearerToken(verifyToken, &auth.RequireBearerTokenOptions{})(
			injectIdentity(handler),
		),
	)

	mux := http.NewServeMux()
	mux.Handle("/mcp", authed)
	mux.Handle("/mcp/", authed)

	// 健康检查（不需要认证）
	mux.HandleFunc("/healthz", func(w http.ResponseWriter, r *http.Request) {
		json.NewEncoder(w).Encode(map[string]any{"ok": true, "stateless": true})
	})

	log.Printf("PoC MCP server listening on http://%s/mcp (stateless=true)", addr)
	log.Printf("tokens: ccs_poc_clara_001 / ccs_poc_codex_002 / ccs_poc_other_003")
	if err := http.ListenAndServe(addr, mux); err != nil {
		log.Fatal(err)
	}
}

// ---------------------------------------------------------------------------

func asString(v any) string {
	s, _ := v.(string)
	return s
}

func asStrings(v any) []string {
	switch t := v.(type) {
	case []string:
		return t
	case []any:
		out := make([]string, 0, len(t))
		for _, e := range t {
			out = append(out, asString(e))
		}
		return out
	}
	return nil
}
