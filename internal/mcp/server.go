package mcp

import (
	"log/slog"
	"net/http"

	"github.com/modelcontextprotocol/go-sdk/auth"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"
)

// serverName 是 MCP initialize 响应里的服务端标识。
const serverName = "claracore-server"

// Host 是 MCP 入口。
//
// 它持有领域服务与日志，不持有数据库连接——存储对这一层不可见
// （test/arch_test.go 会拦住 internal/mcp → internal/adapters 的 import）。
type Host struct {
	gov     governanceService
	logger  *slog.Logger
	version string

	srv *sdk.Server
}

func NewHost(gov governanceService, logger *slog.Logger, version string) *Host {
	h := &Host{gov: gov, logger: logger, version: version}
	h.srv = h.newServer()
	return h
}

// newServer 注册工具集。
//
// 所有请求共用一个 *sdk.Server：工具集与身份无关（agent 继承其所有者的
// 全部权限，不存在「这个 agent 少几个工具」的情况），按请求重建纯属浪费。
// 身份走 context 传递，PoC 已验证 HTTP 中间件的 context 能贯通到 tool handler。
func (h *Host) newServer() *sdk.Server {
	s := sdk.NewServer(&sdk.Implementation{
		Name:    serverName,
		Version: h.version,
	}, nil)

	sdk.AddTool(s, &sdk.Tool{
		Name: "whoami",
		Description: "返回当前 token 背后的身份，以及可读写的空间列表。" +
			"写入类工具不传 space 时的默认目标即 defaultSpaceId。",
	}, h.handleWhoAmI)

	return s
}

// Handler 返回挂载在 /mcp 上的处理器。
//
// 中间件顺序是**语义要求**不是风格问题：RequireBearerToken 必须在最外层，
// injectIdentity 依赖它放进 context 的 TokenInfo。顺序写反的表现是所有请求
// 401，injectIdentity 里有一条日志专门解释这种情况。
//
// Stateless: true —— 无会话状态，任意实例可处理任意请求，这是多实例部署的前提。
// 代价见 docs/06-poc-findings.md：GET/DELETE 返回 405（没有 SSE 长连接），
// tool handler 里读到的协议版本是 SDK 默认值（业务逻辑不得依赖它）。
func (h *Host) Handler() http.Handler {
	mcpHandler := sdk.NewStreamableHTTPHandler(
		func(*http.Request) *sdk.Server { return h.srv },
		&sdk.StreamableHTTPOptions{
			Stateless: true,
			Logger:    h.logger,
		},
	)

	return auth.RequireBearerToken(h.verifyToken, &auth.RequireBearerTokenOptions{
		// token 允许不过期（一个 agent 多 token 并存是平滑轮换的前提，
		// 而长期驻留的机器 token 未必需要 TTL）。过期判断在领域层，
		// 见 verifyToken 的注释。
		AllowMissingExpiration: true,
	})(h.injectIdentity(mcpHandler))
}
