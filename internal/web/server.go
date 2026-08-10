// Package web 是 Host B：给人用的治理面（docs/04-web-scope.md）。
//
// 与 MCP Host 的关系是**并列**，不是上下层：两者都只调用 domain/，
// 都不碰 adapters/，互相之间也不 import（由 test/arch_test.go 强制）。
// 任何「Web 专用的写入路径」都是设计事故——业务逻辑只有一份，
// 在领域层。
//
// Web 的第一职责不是「管理」，而是**消除黑盒**（04 §1.1）：团队版里
// 成员看不见 agent 在读什么写什么，而黑盒的直接后果是没人敢把团队知识
// 放进来。这一版先把「人能进得来、能发 token、能管账号」这条闭环做通，
// 可见性页面（内容浏览、活动流、卡片审核）是下一刀。
package web

import (
	"context"
	"embed"
	"html/template"
	"log/slog"
	"net/http"
	"time"

	"claracore-server/internal/domain/governance"
	"claracore-server/internal/domain/knowledge"
	"claracore-server/internal/platform"
)

//go:embed templates/*.html static/*
var assets embed.FS

// governanceService 是 Web 需要的治理域能力，按使用方需要定义。
//
// 与 mcp.governanceService 是两个接口而不是共用一个：两个 Host 需要的
// 能力集不同（Web 要登录与成员管理，MCP 要 token 认证），
// 合成一个大接口只会让两边都被迫依赖对方需要的东西。
type governanceService interface {
	Login(ctx context.Context, username, password string) (governance.SessionToken, governance.Principal, error)
	ResumeSession(ctx context.Context, token string) (governance.Principal, error)
	Logout(ctx context.Context, token string) error

	ChangePassword(ctx context.Context, tenantID, userID platform.ID, oldPassword, newPassword string) error
	ResetPassword(ctx context.Context, tenantID, userID platform.ID, newPassword string) error
	SetUserStatus(ctx context.Context, tenantID, userID platform.ID, status string) error
	ListUsers(ctx context.Context, tenantID platform.ID) ([]governance.User, error)
	CreateUser(ctx context.Context, in governance.CreateUserInput) (governance.User, governance.Space, error)

	Scope(ctx context.Context, id governance.Identity) (governance.SpaceScope, error)
	CreateAgent(ctx context.Context, tenantID, userID platform.ID, slug, displayName string) (governance.Agent, error)
	IssueToken(ctx context.Context, in governance.IssueTokenInput) (string, governance.Token, error)
	ListTokens(ctx context.Context, tenantID, userID platform.ID) ([]governance.Token, error)
	RevokeToken(ctx context.Context, tenantID, tokenID platform.ID) error
}

// knowledgeService 只用于概览页的统计。
//
// 概览页要回答「本空间还有多少条待审」——04 §6.1 明确点名的那个空白。
// 复用领域层的 SpaceStats，**不另写一份统计 SQL**：两份统计
// 会立刻给出两个对不上的数字，而那种不一致查起来极费时间。
type knowledgeService interface {
	SpaceStats(ctx context.Context, tenantID platform.ID, spaces []platform.ID) ([]knowledge.SpaceStats, error)
}

type Options struct {
	Version string
	// PublicMCPURL 是给用户复制的 MCP 连接地址。
	//
	// 必须可配：服务监听的是 127.0.0.1:51890，而用户要填进 agent 配置的
	// 是 nginx 后面那个对外地址。拿监听地址拼一个出来，等于发给每个人
	// 一份连不上的配置。
	PublicMCPURL string
	// SecureCookie 决定会话 cookie 带不带 Secure 标记。
	//
	// 生产必须为真（HTTPS）。本地开发走 http 时为真会让 cookie 根本存不下来，
	// 表现是「登录成功但立刻又跳回登录页」——所以它是配置而不是常量。
	SecureCookie bool
}

type Host struct {
	gov    governanceService
	know   knowledgeService
	logger *slog.Logger
	opts   Options
	// tmpl 是**每个页面一套模板**，不是一个大集合。
	//
	// 全部文件 ParseFS 进同一个 Template 时，各页面的 {{define "content"}}
	// 会互相覆盖——最后解析的那个赢，于是每一页都渲染出同一份内容。
	// 这个坑不报错：模板解析成功，页面也返回 200，只是内容全错
	// （2026-08-10 落地时踩到，六个页面全渲染成了成员管理）。
	tmpl map[string]*template.Template
}

func NewHost(gov governanceService, know knowledgeService, logger *slog.Logger, opts Options) *Host {
	return &Host{
		gov:    gov,
		know:   know,
		logger: logger,
		opts:   opts,
		// 模板在启动时一次性解析：解析失败是装配错误，该在启动时炸，
		// 而不是等某个人点开某个页面时才 500。
		tmpl: parseTemplates(),
	}
}

// pages 是全部页面模板。新增页面要加进来——漏加会在启动时 panic，
// 而不是等有人点开它才 500。
var pages = []string{"login.html", "password.html", "overview.html", "tokens.html", "members.html"}

func parseTemplates() map[string]*template.Template {
	out := make(map[string]*template.Template, len(pages))
	for _, name := range pages {
		out[name] = template.Must(
			template.New(name).Funcs(templateFuncs()).
				ParseFS(assets, "templates/layout.html", "templates/"+name))
	}
	return out
}

// Handler 返回挂载在 / 上的处理器。
//
// 路由分三层，**每一层都是一道必须走的门**：
//
//	public    —— 登录页与静态资源
//	requireUser        —— 需要有效会话
//	requirePasswordSet —— 需要会话，且初始密码已经改过
//	requireAdmin       —— 需要会话 + 已改密 + admin 角色
//
// 分层写成中间件而不是在每个 handler 里各判一次：少判一次不会有
// 编译错误，只会有一个不需要登录就能打开的管理页。
func (h *Host) Handler() http.Handler {
	mux := http.NewServeMux()

	// 静态资源直接从 embed 提供：CSS 编进二进制，保住单文件部署
	mux.Handle("GET /static/", http.FileServerFS(assets))

	mux.HandleFunc("GET /login", h.handleLoginPage)
	mux.HandleFunc("POST /login", h.handleLogin)
	mux.HandleFunc("POST /logout", h.handleLogout)

	// 改密页只要求「已登录」，不要求「已改密」——否则强制改密的人
	// 会被自己的中间件挡在改密页外面，形成一个谁也进不去的死锁。
	mux.Handle("GET /password", h.requireUser(http.HandlerFunc(h.handlePasswordPage)))
	mux.Handle("POST /password", h.requireUser(http.HandlerFunc(h.handlePasswordChange)))

	mux.Handle("GET /{$}", h.requirePasswordSet(http.HandlerFunc(h.handleOverview)))
	mux.Handle("GET /tokens", h.requirePasswordSet(http.HandlerFunc(h.handleTokensPage)))
	mux.Handle("POST /tokens", h.requirePasswordSet(http.HandlerFunc(h.handleTokenCreate)))
	mux.Handle("POST /tokens/revoke", h.requirePasswordSet(http.HandlerFunc(h.handleTokenRevoke)))

	mux.Handle("GET /admin/members", h.requireAdmin(http.HandlerFunc(h.handleMembersPage)))
	mux.Handle("POST /admin/members", h.requireAdmin(http.HandlerFunc(h.handleMemberCreate)))
	mux.Handle("POST /admin/members/status", h.requireAdmin(http.HandlerFunc(h.handleMemberStatus)))
	mux.Handle("POST /admin/members/reset", h.requireAdmin(http.HandlerFunc(h.handleMemberReset)))

	return securityHeaders(mux)
}

// securityHeaders 是一层薄防护。
//
// CSP 里没有 unsafe-inline：模板是纯 SSR，没有内联脚本，也不打算有。
// 一旦将来有人想加一行 <script>，CSP 会先把他挡住，那正是想要的效果——
// 前后端不分离是明确决定（04 §4），别让它从一行内联脚本开始滑坡。
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := w.Header()
		hdr.Set("Content-Security-Policy", "default-src 'self'; frame-ancestors 'none'; base-uri 'none'")
		hdr.Set("X-Content-Type-Options", "nosniff")
		hdr.Set("Referrer-Policy", "same-origin")
		next.ServeHTTP(w, r)
	})
}

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"stamp": func(t time.Time) string {
			if t.IsZero() {
				return "—"
			}
			return t.UTC().Format("2006-01-02 15:04")
		},
		// stampPtr 用于 last_used_at 这类可空时间：从没用过的 token
		// 显示「从未」而不是一个 1970 年的日期
		"stampPtr": func(t *time.Time) string {
			if t == nil || t.IsZero() {
				return "从未"
			}
			return t.UTC().Format("2006-01-02 15:04")
		},
	}
}
