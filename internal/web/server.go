// Package web 是 Host B：给人用的治理面（docs/04-web-scope.md）。
//
// 与 MCP Host 的关系是**并列**，不是上下层：两者都只调用 domain/，
// 都不碰 adapters/，互相之间也不 import（由 test/arch_test.go 强制）。
// 任何「Web 专用的写入路径」都是设计事故——业务逻辑只有一份，
// 在领域层。
//
// Web 的第一职责不是「管理」，而是**消除黑盒**（04 §1.1）：团队版里
// 成员看不见 agent 在读什么写什么，而黑盒的直接后果是没人敢把团队知识
// 放进来。当前已经打通登录/接入/成员治理、内容浏览与卡片审核，并通过
// /activity 和受二次确认 + meta-audit 约束的 /admin/audit 补齐活动可见性。
package web

import (
	"context"
	"embed"
	"errors"
	"html/template"
	"log/slog"
	"mime"
	"net/http"
	"strings"
	"time"

	"claracore-server/internal/domain/activity"
	"claracore-server/internal/domain/continuity"
	"claracore-server/internal/domain/external"
	"claracore-server/internal/domain/governance"
	"claracore-server/internal/domain/knowledge"
	"claracore-server/internal/platform"
	"claracore-server/internal/ports"
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
	ListUserSummariesPage(ctx context.Context, in governance.GovernancePageInput) (governance.UserSummaryPage, error)
	FindUserSummary(ctx context.Context, tenantID, userID platform.ID) (governance.UserSummary, error)
	CreateUser(ctx context.Context, in governance.CreateUserInput) (governance.User, governance.Space, error)
	ListSpacePage(ctx context.Context, in governance.GovernancePageInput) (governance.SpacePage, error)
	ListSpaceGrantPage(ctx context.Context, in governance.GovernancePageInput) (governance.SpaceGrantPage, error)
	CreateProjectSpace(ctx context.Context, tenantID, ownerUserID platform.ID, name string) (governance.Space, error)
	GrantSpaceAccess(ctx context.Context, tenantID, spaceID, userID, grantedBy platform.ID, canRead, canWrite bool) error
	RevokeSpaceAccess(ctx context.Context, tenantID, spaceID, userID platform.ID) error

	Scope(ctx context.Context, id governance.Identity) (governance.SpaceScope, error)
	CreateAgent(ctx context.Context, tenantID, userID platform.ID, slug, displayName string) (governance.Agent, error)
	IssueToken(ctx context.Context, in governance.IssueTokenInput) (string, governance.Token, error)
	ListTokenSummariesPage(ctx context.Context, in governance.GovernancePageInput) (governance.TokenSummaryPage, error)
	FindOwnTokenSummary(ctx context.Context, tenantID, userID, tokenID platform.ID) (governance.TokenSummary, error)
	ListAgentAccessSummaries(ctx context.Context, tenantID, userID platform.ID, limit int) ([]governance.AgentAccessSummary, error)
	RevokeToken(ctx context.Context, tenantID, tokenID platform.ID) error
}

// knowledgeService 只用于概览页的统计。
//
// 概览页要回答「本空间还有多少条待审」——04 §6.1 明确点名的那个空白。
// 复用领域层的 SpaceStats，**不另写一份统计 SQL**：两份统计
// 会立刻给出两个对不上的数字，而那种不一致查起来极费时间。
type knowledgeService interface {
	SpaceStats(ctx context.Context, tenantID platform.ID, spaces []platform.ID) ([]knowledge.SpaceStats, error)

	// ListContents 是浏览页与审核队列共用的列表查询。
	// **不是检索**——治理面必须看得到 draft 与 deprecated（见 knowledge/list.go）。
	ListContents(ctx context.Context, in knowledge.ListContentsInput) (knowledge.ListContentsResult, error)

	GetCard(ctx context.Context, tenantID, id platform.ID, spaces []platform.ID) (knowledge.Card, []knowledge.Link, error)
	GetMemory(ctx context.Context, tenantID, id platform.ID, spaces []platform.ID) (knowledge.Memory, []knowledge.Link, error)
	FindCardDuplicateCandidates(ctx context.Context, tenantID, cardID platform.ID, spaces []platform.ID) ([]knowledge.CardDuplicateCandidate, error)

	// UpdateCard 是 Web 上**唯一**的内容写入口（审核页）。
	// 复用 MCP 那侧同一个领域用例——「Web 专用的写入路径」是设计事故。
	UpdateCard(ctx context.Context, in knowledge.UpdateCardInput) (knowledge.Card, error)
	DeprecateDraftCards(ctx context.Context, in knowledge.DeprecateDraftCardsInput) ([]knowledge.Card, error)
	Ingest(ctx context.Context, in knowledge.IngestInput) (knowledge.IngestReport, error)
	EmbeddingHealth(ctx context.Context, tenantID platform.ID, failureLimit int) (knowledge.EmbeddingHealth, error)
	BackfillEmbeddings(ctx context.Context, tenantID platform.ID, limit int) (knowledge.BackfillReport, error)
	RetryEmbeddingFailures(ctx context.Context, tenantID platform.ID) (int, error)
}

type markdownScanner func(ctx context.Context, sourceName, root string) ([]knowledge.Document, error)

// DatabasePoolStats 是系统页需要的 database/sql 池摘要。
// Web 不接收 *sql.DB，避免 Host 绕过领域服务直接查询业务数据。
type DatabasePoolStats struct {
	MaxOpenConnections int
	OpenConnections    int
	InUse              int
	Idle               int
	WaitCount          int64
	WaitDuration       time.Duration
	MaxIdleClosed      int64
	MaxIdleTimeClosed  int64
	MaxLifetimeClosed  int64
}

// HTTPRequestStats is a bounded process-local health summary. It deliberately
// carries no route, identity, request ID, or content dimensions.
type HTTPRequestStats struct {
	Available      bool
	Window         time.Duration
	Requests       uint64
	ClientErrors   uint64
	ServerErrors   uint64
	Panics         uint64
	AverageLatency time.Duration
	P95Latency     time.Duration
	P95Overflow    bool
	InFlight       int64
}

// activityService is the Web host's narrow view of the activity domain.
// Ordinary visibility is already reduced to resolved shared-space IDs before
// it crosses this interface; the MySQL adapter applies the actual predicate.
type activityService interface {
	Record(ctx context.Context, in activity.RecordInput) (activity.Event, error)
	ListVisible(ctx context.Context, in activity.ListVisibleInput) (activity.Page, error)
	ListAdmin(ctx context.Context, in activity.ListAdminInput) (activity.Page, error)
	ContextFeedbackQuality(ctx context.Context, tenantID platform.ID, window time.Duration) (activity.ContextFeedbackQuality, error)
	SharedActivityPulse(ctx context.Context, tenantID platform.ID, readableSharedSpaceIDs []platform.ID, days int) (activity.SharedActivityPulse, error)
}

// continuityService 是治理面读取共同线所需的最小能力。共同线与记忆、
// 知识卡是并列领域，因此在 Web 中使用独立页面，不混进内容类型筛选。
type continuityService interface {
	List(ctx context.Context, in continuity.ListInput) ([]continuity.LineSummary, error)
	ListPage(ctx context.Context, in continuity.ListInput) (continuity.ListPage, error)
	Stats(ctx context.Context, tenantID platform.ID) (continuity.LineStats, error)
	Get(ctx context.Context, in continuity.GetInput) (continuity.ResumePacket, error)
	SetStatus(ctx context.Context, in continuity.SetStatusInput) (continuity.ResumePacket, error)
}

type Options struct {
	Version             string
	SourceDigest        string
	BinaryDigest        string
	Env                 string
	DatabaseHealth      func(context.Context) error
	DatabasePoolStats   func() DatabasePoolStats
	SchemaVersion       string
	SchemaHealth        func(context.Context) (string, error)
	StartedAt           time.Time
	Now                 func() time.Time
	ShutdownDrain       time.Duration
	RedisConfigured     bool
	RedisHealth         func(context.Context) error
	EmbeddingProvider   string
	MySQLTransport      string
	RedisTransport      string
	EmbeddingTransport  string
	ExternalTransport   string
	EmbeddingCacheStats func() (hits, misses uint64, enabled bool)
	LoginRateLimiter    ports.RateLimiter
	LoginRateLimit      int
	LoginRateWindow     time.Duration
	MCPRateLimit        int
	MCPTenantRateLimit  int
	MCPRateWindow       time.Duration
	MetricsAddr         string
	BackupStatus        ports.BackupStatusReader
	RestoreStatus       ports.RestoreStatusReader
	IntegrityStatus     ports.IntegrityStatusReader
	TenantCapacity      ports.TenantCapacityReader
	MaxFormBytes        int64
	ExportMaxBytes      int64
	TenantExporter      ports.TenantExporter
	HTTPRequestStats    func() HTTPRequestStats
	ExternalConfigured  bool
	ExternalSources     func(context.Context, platform.ID) ([]external.SourceHealth, error)
	ExternalSyncRuns    func(context.Context, platform.ID, int) ([]external.SyncRunReceipt, error)
	QueueExternalSync   func(context.Context, platform.ID) (bool, error)
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
	// IngestRoot 是管理员页面唯一允许扫描的文件树；空值关闭该能力。
	IngestRoot string
	// ScanMarkdown 在装配层注入文件系统 adapter，保持 Web 只依赖领域类型。
	ScanMarkdown markdownScanner
}

type Host struct {
	gov        governanceService
	know       knowledgeService
	continuity continuityService
	activity   activityService
	logger     *slog.Logger
	opts       Options
	exportGate chan struct{}
	// tmpl 是**每个页面一套模板**，不是一个大集合。
	//
	// 全部文件 ParseFS 进同一个 Template 时，各页面的 {{define "content"}}
	// 会互相覆盖——最后解析的那个赢，于是每一页都渲染出同一份内容。
	// 这个坑不报错：模板解析成功，页面也返回 200，只是内容全错
	// （2026-08-10 落地时踩到，六个页面全渲染成了成员管理）。
	tmpl map[string]*template.Template
}

func NewHost(gov governanceService, know knowledgeService, continuity continuityService, activity activityService, logger *slog.Logger, opts Options) *Host {
	if opts.Now == nil {
		opts.Now = time.Now
	}
	if opts.LoginRateLimit <= 0 {
		opts.LoginRateLimit = 10
	}
	if opts.LoginRateWindow <= 0 {
		opts.LoginRateWindow = 15 * time.Minute
	}
	if opts.MaxFormBytes <= 0 {
		opts.MaxFormBytes = 1 << 20
	}
	if opts.ExportMaxBytes <= 0 {
		opts.ExportMaxBytes = 1 << 30
	}
	return &Host{
		gov:        gov,
		know:       know,
		continuity: continuity,
		activity:   activity,
		logger:     logger,
		opts:       opts,
		exportGate: make(chan struct{}, 1),
		// 模板在启动时一次性解析：解析失败是装配错误，该在启动时炸，
		// 而不是等某个人点开某个页面时才 500。
		tmpl: parseTemplates(),
	}
}

// pages 是全部页面模板。新增页面要加进来——漏加会在启动时 panic，
// 而不是等有人点开它才 500。
var pages = []string{
	"login.html", "password.html", "overview.html", "tokens.html", "members.html",
	"spaces.html",
	"ingest.html",
	"indexing.html",
	"review.html", "review_detail.html", "browse.html", "browse_detail.html",
	"continuity.html", "continuity_detail.html",
	"activity.html", "audit_confirm.html", "audit.html", "system.html",
	"sources.html",
	"export.html",
}

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
	mux.Handle("POST /logout", h.requireUser(http.HandlerFunc(h.handleLogout)))

	// 改密页只要求「已登录」，不要求「已改密」——否则强制改密的人
	// 会被自己的中间件挡在改密页外面，形成一个谁也进不去的死锁。
	mux.Handle("GET /password", h.requireUser(http.HandlerFunc(h.handlePasswordPage)))
	mux.Handle("POST /password", h.requireUser(http.HandlerFunc(h.handlePasswordChange)))

	mux.Handle("GET /{$}", h.requirePasswordSet(http.HandlerFunc(h.handleOverview)))
	mux.Handle("GET /tokens", h.requirePasswordSet(http.HandlerFunc(h.handleTokensPage)))
	mux.Handle("POST /tokens", h.requirePasswordSet(http.HandlerFunc(h.handleTokenCreate)))
	mux.Handle("POST /tokens/revoke", h.requirePasswordSet(http.HandlerFunc(h.handleTokenRevoke)))

	// 审核是 Web 上唯一的内容写入场景（01 §10 的唯一例外）。
	// 它挂 requirePasswordSet 而不是 requireAdmin——能写就能审，
	// 不引入新的权限概念（04 §6）。
	mux.Handle("GET /review", h.requirePasswordSet(http.HandlerFunc(h.handleReviewList)))
	mux.Handle("GET /review/{id}", h.requirePasswordSet(http.HandlerFunc(h.handleReviewDetail)))
	mux.Handle("POST /review/{id}", h.requirePasswordSet(http.HandlerFunc(h.handleReviewSubmit)))
	mux.Handle("POST /review/batch-deprecate", h.requirePasswordSet(http.HandlerFunc(h.handleReviewBatchDeprecate)))

	mux.Handle("GET /browse", h.requirePasswordSet(http.HandlerFunc(h.handleBrowse)))
	mux.Handle("GET /browse/{id}", h.requirePasswordSet(http.HandlerFunc(h.handleBrowseDetail)))
	mux.Handle("GET /continuity", h.requirePasswordSet(http.HandlerFunc(h.handleContinuity)))
	mux.Handle("GET /continuity/{id}", h.requirePasswordSet(http.HandlerFunc(h.handleContinuityDetail)))
	mux.Handle("POST /continuity/{id}/status", h.requirePasswordSet(http.HandlerFunc(h.handleContinuityStatus)))
	mux.Handle("GET /activity", h.requirePasswordSet(http.HandlerFunc(h.handleActivity)))

	mux.Handle("GET /admin/members", h.requireAdmin(http.HandlerFunc(h.handleMembersPage)))
	mux.Handle("POST /admin/members", h.requireAdmin(http.HandlerFunc(h.handleMemberCreate)))
	mux.Handle("POST /admin/members/status", h.requireAdmin(http.HandlerFunc(h.handleMemberStatus)))
	mux.Handle("POST /admin/members/reset", h.requireAdmin(http.HandlerFunc(h.handleMemberReset)))
	mux.Handle("GET /admin/spaces", h.requireAdmin(http.HandlerFunc(h.handleSpacesPage)))
	mux.Handle("POST /admin/spaces", h.requireAdmin(http.HandlerFunc(h.handleSpaceCreate)))
	mux.Handle("POST /admin/spaces/grant", h.requireAdmin(http.HandlerFunc(h.handleSpaceGrant)))
	mux.Handle("POST /admin/spaces/revoke", h.requireAdmin(http.HandlerFunc(h.handleSpaceRevoke)))
	mux.Handle("GET /admin/ingest", h.requireAdmin(http.HandlerFunc(h.handleIngestPage)))
	mux.Handle("POST /admin/ingest", h.requireAdmin(http.HandlerFunc(h.handleIngestRun)))
	mux.Handle("GET /admin/indexing", h.requireAdmin(http.HandlerFunc(h.handleIndexingPage)))
	mux.Handle("POST /admin/indexing/backfill", h.requireAdmin(http.HandlerFunc(h.handleIndexingBackfill)))
	mux.Handle("POST /admin/indexing/retry", h.requireAdmin(http.HandlerFunc(h.handleIndexingRetry)))
	mux.Handle("GET /admin/audit", h.requireAdmin(http.HandlerFunc(h.handleAuditConfirm)))
	mux.Handle("POST /admin/audit", h.requireAdmin(http.HandlerFunc(h.handleAudit)))
	mux.Handle("GET /admin/system", h.requireAdmin(http.HandlerFunc(h.handleSystem)))
	mux.Handle("GET /admin/sources", h.requireAdmin(http.HandlerFunc(h.handleSources)))
	mux.Handle("POST /admin/sources/sync", h.requireAdmin(http.HandlerFunc(h.handleSourceSync)))
	mux.Handle("GET /admin/export", h.requireAdmin(http.HandlerFunc(h.handleExportPage)))
	mux.Handle("POST /admin/export", h.requireAdmin(http.HandlerFunc(h.handleExportDownload)))

	return securityHeaders(limitWebForms(mux, h.opts.MaxFormBytes))
}

// limitWebForms makes the Web write boundary explicit. Every browser write in
// this service is an application/x-www-form-urlencoded form; parsing it once
// here prevents FormValue from silently ignoring an oversized-body error and
// letting a partially parsed destructive action continue.
func limitWebForms(next http.Handler, maxBytes int64) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !isWrite(r) {
			next.ServeHTTP(w, r)
			return
		}
		mediaType, _, err := mime.ParseMediaType(r.Header.Get("Content-Type"))
		if err != nil || mediaType != "application/x-www-form-urlencoded" {
			http.Error(w, "只接受 application/x-www-form-urlencoded 表单", http.StatusUnsupportedMediaType)
			return
		}
		r.Body = http.MaxBytesReader(w, r.Body, maxBytes)
		if err := r.ParseForm(); err != nil {
			var tooLarge *http.MaxBytesError
			if errors.As(err, &tooLarge) {
				http.Error(w, "表单超过允许的大小", http.StatusRequestEntityTooLarge)
				return
			}
			http.Error(w, "表单格式无效", http.StatusBadRequest)
			return
		}
		next.ServeHTTP(w, r)
	})
}

// securityHeaders 是一层薄防护。
//
// CSP 里没有 unsafe-inline：模板是纯 SSR，没有内联脚本，也不打算有。
// 一旦将来有人想加一行 <script>，CSP 会先把他挡住，那正是想要的效果——
// 前后端不分离是明确决定（04 §4），别让它从一行内联脚本开始滑坡。
func securityHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		hdr := w.Header()
		hdr.Set("Content-Security-Policy", "default-src 'none'; script-src 'self'; style-src 'self'; img-src 'self' data:; font-src 'self'; connect-src 'self'; form-action 'self'; frame-ancestors 'none'; base-uri 'none'; object-src 'none'; manifest-src 'none'; worker-src 'none'")
		hdr.Set("X-Content-Type-Options", "nosniff")
		hdr.Set("X-Frame-Options", "DENY")
		hdr.Set("Referrer-Policy", "no-referrer")
		hdr.Set("Permissions-Policy", "camera=(), geolocation=(), microphone=(), payment=(), usb=()")
		hdr.Set("Cross-Origin-Opener-Policy", "same-origin")
		hdr.Set("Cross-Origin-Resource-Policy", "same-origin")
		hdr.Set("X-Permitted-Cross-Domain-Policies", "none")
		if strings.HasPrefix(r.URL.Path, "/static/") {
			// Assets are embedded but their URLs are not content-hashed. A short
			// cache avoids needless transfers without serving an old UI across a
			// release for months.
			hdr.Set("Cache-Control", "public, max-age=300")
		} else {
			// Every SSR page can contain tenant knowledge, CSRF values, token
			// plaintext, generated passwords, or operational state.
			hdr.Set("Cache-Control", "no-store, private")
			hdr.Set("Pragma", "no-cache")
			hdr.Set("X-Robots-Tag", "noindex, nofollow, noarchive")
		}
		next.ServeHTTP(w, r)
	})
}

func templateFuncs() template.FuncMap {
	return template.FuncMap{
		"mul100": func(v float64) float64 { return v * 100 },
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
