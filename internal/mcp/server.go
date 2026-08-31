package mcp

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"io"
	"log/slog"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/modelcontextprotocol/go-sdk/auth"
	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"claracore-server/internal/domain/activity"
	"claracore-server/internal/domain/external"
	"claracore-server/internal/ports"
)

// serverName 是 MCP initialize 响应里的服务端标识。
const serverName = "claracore-server"

// Host 是 MCP 入口。
//
// 它持有领域服务与日志，不持有数据库连接——存储对这一层不可见
// （test/arch_test.go 会拦住 internal/mcp → internal/adapters 的 import）。
type Host struct {
	gov                governanceService
	know               knowledgeService
	continuity         continuityService
	activity           activityService
	external           externalSearchService
	externalConfigured bool
	logger             *slog.Logger
	version            string

	// semanticSearch 只用于 server_status 的自述，不影响任何行为。
	// 检索该不该用向量由 knowledge.Service 自己知道（它持有 Embedder），
	// Host 在这里放一个副本纯粹是为了能对外说清楚这个部署是什么形态。
	semanticSearch      bool
	embeddingCacheStats func() (hits, misses uint64, enabled bool)
	maxRequestBodyBytes int64
	requestLimiter      ports.RateLimiter
	requestRateLimit    int
	tenantRateLimit     int
	requestRateWindow   time.Duration

	srv *sdk.Server
}

type HostOptions struct {
	Version string
	// MaxRequestBodyBytes is enforced by the SDK for Content-Length, chunked,
	// and HTTP/2 requests. Zero keeps the SDK's safe 4 MiB default.
	MaxRequestBodyBytes int64
	// SemanticSearch 表示这个部署接了 embedding 服务。
	SemanticSearch bool
	// EmbeddingCacheStats returns this process's counters for the effective
	// local + optional shared query/content cache. Nil means no decorator.
	EmbeddingCacheStats func() (hits, misses uint64, enabled bool)
	// RequestLimiter applies authenticated per-agent and per-tenant budgets.
	// Limiter keys are one-way digests and never contain token, user, tenant, or
	// agent text. TenantRequestRateLimit must be no smaller than the agent limit.
	RequestLimiter         ports.RateLimiter
	RequestRateLimit       int
	TenantRequestRateLimit int
	RequestRateWindow      time.Duration
	// ExternalSearch may remain available from the persisted mirror while the
	// upstream connector is disabled; ExternalConfigured distinguishes that
	// mirror-only state from an actively refreshable source.
	ExternalSearch     externalSearchService
	ExternalConfigured bool
}

func NewHost(gov governanceService, know knowledgeService, continuity continuityService, activities activityService, logger *slog.Logger, opts HostOptions) *Host {
	if continuity == nil {
		panic("mcp: continuityService 不能为空")
	}
	if activities == nil {
		panic("mcp: activityService 不能为空")
	}
	if opts.RequestLimiter != nil {
		if opts.RequestRateLimit <= 0 {
			opts.RequestRateLimit = 300
		}
		if opts.TenantRequestRateLimit < opts.RequestRateLimit {
			opts.TenantRequestRateLimit = opts.RequestRateLimit * 10
		}
		if opts.RequestRateWindow <= 0 {
			opts.RequestRateWindow = time.Minute
		}
	}
	h := &Host{
		gov: gov, know: know, continuity: continuity, activity: activities, external: opts.ExternalSearch, logger: logger,
		version:             opts.Version,
		semanticSearch:      opts.SemanticSearch,
		embeddingCacheStats: opts.EmbeddingCacheStats,
		maxRequestBodyBytes: opts.MaxRequestBodyBytes,
		requestLimiter:      opts.RequestLimiter,
		requestRateLimit:    opts.RequestRateLimit,
		tenantRateLimit:     opts.TenantRequestRateLimit,
		requestRateWindow:   opts.RequestRateWindow,
	}
	h.externalConfigured = opts.ExternalConfigured
	h.srv = h.newServer()
	return h
}

type externalSearchService interface {
	Search(context.Context, external.SearchInput) (external.SearchResult, error)
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

	// 工具数量刻意控制在 24 以内（docs/03-mcp-contract.md §1）：
	// 每个工具的 schema 都占 agent 的上下文预算，工具越多选错的概率越高。
	// 维护类操作（导入导出、合并建议、休眠归档、标签别名）**一律不进这里**，
	// 它们是人的运维动作，归 Web 治理面。

	// --- 系统 ---
	addAuditedTool(h, s, &sdk.Tool{
		Name: "whoami",
		Description: "返回当前 token 背后的身份，以及可读写的空间列表。" +
			"写入类工具不传 space 时的默认目标即 defaultSpaceId。",
	}, activity.KindTrace, activity.AccessRead, auditWhoAmI, h.handleWhoAmI)
	addAuditedTool(h, s, &sdk.Tool{
		Name:        "server_status",
		Description: "服务版本，以及这个部署是否接了语义检索（没接时 card_recall 退化为中文全文匹配）。",
	}, activity.KindTrace, activity.AccessRead, auditServerStatus, h.handleServerStatus)
	addAuditedTool(h, s, &sdk.Tool{
		Name:        "gateway_auto_context",
		Description: "对当前情境做保守的单证据自动召回：显式 lineId 精确返回 Shared Line；否则 Memory 与 Card 各自过门禁并竞争唯一位置。低相关、域内或跨域歧义、模型不可用时 abstain；deliver_one 会返回供反馈关联的匿名 contextId。",
	}, activity.KindTrace, activity.AccessRead, auditGatewayAutoContext, h.handleGatewayAutoContext)
	addAuditedTool(h, s, &sdk.Tool{
		Name:        "gateway_context_feedback",
		Description: "记录 Memory、Card 或 Shared Line 自动上下文是否被交付、使用、忽略或判错。回传 deliver_one 的 contextId 可计入选择覆盖；只写治理活动，不修改实体正文，旧 cardId 调用继续兼容。",
	}, activity.KindGovernance, activity.AccessWrite, auditGatewayContextFeedback, h.handleGatewayContextFeedback)
	addAuditedTool(h, s, &sdk.Tool{
		Name:        "context_quality_status",
		Description: "汇总当前租户近 30 天的选择、固定分类放弃原因、已关联反馈选择和已提交 outcome。只返回计数，不暴露内容或 actor；原因、反馈覆盖与 outcome 都不等同于准确率。",
	}, activity.KindTrace, activity.AccessRead, auditContextQualityStatus, h.handleContextQualityStatus)

	// --- Memory ---
	addAuditedTool(h, s, &sdk.Tool{
		Name:        "memoria_create",
		Description: "记一条记忆。不传 space 即写入个人空间——随手记不该有心智负担，要共享再用 promote。",
	}, activity.KindTrace, activity.AccessWrite, auditMemoriaCreate, h.handleMemoriaCreate)
	addAuditedTool(h, s, &sdk.Tool{
		Name: "memoria_search",
		Description: "统一检索团队知识。兼容字段 hits 仍只含 Memory；matches 将 Memory、Card 与只读外部镜像的语义命中统一排序，" +
			"字面降级结果按 memory_text/card_text 分组。每条标注空间、来源和 semantic；外部项只给来源链接与同步新鲜度，不返回本地正文。",
	}, activity.KindTrace, activity.AccessRead, auditMemoriaSearch, h.handleMemoriaSearch)
	addAuditedTool(h, s, &sdk.Tool{
		Name:        "memoria_get",
		Description: "按 ID 取一条记忆，含标签与关联。",
	}, activity.KindTrace, activity.AccessRead, auditMemoriaGet, h.handleMemoriaGet)
	addAuditedTool(h, s, &sdk.Tool{
		Name: "memoria_update",
		Description: "更新记忆的内容、标签或状态。归档、恢复、打标签都用这一个工具（改 status 或 labels）。" +
			"传上次读到的 version 可获得并发保护：期间被人改过会返回 VERSION_CONFLICT 而不是静默覆盖。",
	}, activity.KindTrace, activity.AccessWrite, auditMemoriaUpdate, h.handleMemoriaUpdate)
	addAuditedTool(h, s, &sdk.Tool{
		Name:        "memoria_delete",
		Description: "软删除一条记忆：不再进默认检索，但仍可按 ID 取回。",
	}, activity.KindTrace, activity.AccessWrite, auditMemoriaDelete, h.handleMemoriaDelete)
	addAuditedTool(h, s, &sdk.Tool{
		Name:        "memoria_link",
		Description: "在两条内容之间建立或删除关联（related / causes / evolved-from / contradicts / part-of）。两端都要在你的可读范围内。",
	}, activity.KindTrace, activity.AccessWrite, auditMemoriaLink, h.handleMemoriaLink)

	// --- Card ---
	addAuditedTool(h, s, &sdk.Tool{
		Name: "card_create",
		Description: "创建知识卡片：一次踩坑或一条工程约定的完整记录。" +
			"recallTriggers 是这张卡的核心——写清楚未来什么情境下该想起它，那是 card_recall 的匹配依据。" +
			"「理解」栏为空的卡只是摘要。",
	}, activity.KindTrace, activity.AccessWrite, auditCardCreate, h.handleCardCreate)
	addAuditedTool(h, s, &sdk.Tool{
		Name: "card_recall",
		Description: "按你当前的工作情境召回卡片——「我正在做 X，有什么该注意的」。" +
			"匹配的是作者显式写下的召回条件而不是卡片全文，所以 situation 要用完整的一句话描述正在做什么，不要传关键词。" +
			"要按主题找卡片请用 card_search。",
	}, activity.KindTrace, activity.AccessRead, auditCardRecall, h.handleCardRecall)
	addAuditedTool(h, s, &sdk.Tool{
		Name:        "card_search",
		Description: "按内容检索知识卡片——「关于 X 的卡片有哪些」。与 card_recall 是不同的意图：这里匹配卡片正文。",
	}, activity.KindTrace, activity.AccessRead, auditCardSearch, h.handleCardSearch)
	addAuditedTool(h, s, &sdk.Tool{
		Name:        "card_get",
		Description: "按 ID 取完整卡片，含全部 section、召回条件与关联。",
	}, activity.KindTrace, activity.AccessRead, auditCardGet, h.handleCardGet)
	addAuditedTool(h, s, &sdk.Tool{
		Name:        "card_update",
		Description: "更新卡片内容、召回条件或状态。把摄取来的 draft 改成 active 就是「审过了」。",
	}, activity.KindTrace, activity.AccessWrite, auditCardUpdate, h.handleCardUpdate)

	// --- 空间 ---
	addAuditedTool(h, s, &sdk.Tool{
		Name:        "space_list",
		Description: "列出你能访问的空间及读写权限。",
	}, activity.KindTrace, activity.AccessRead, auditSpaceList, h.handleSpaceList)
	addAuditedTool(h, s, &sdk.Tool{
		Name: "promote",
		Description: "把一条记忆或一张卡片复制到另一个空间（个人经验 → 团队共识）。" +
			"复制不是移动，原件保留；两份此后各自演进，不会自动同步。",
	}, activity.KindGovernance, activity.AccessWrite, auditPromote, h.handlePromote)
	addAuditedTool(h, s, &sdk.Tool{
		Name:        "space_stats",
		Description: "空间内容统计。unqualifiedCards 是「理解」栏为空、只算摘要的卡片数；draftCards 是等人过目的摄取产物。",
	}, activity.KindTrace, activity.AccessRead, auditSpaceStats, h.handleSpaceStats)

	// --- Shared Line / Continuity ---
	addAuditedTool(h, s, &sdk.Tool{
		Name: "shared_line_list",
		Description: "列出 Shared Line 摘要目录。默认只列当前 agent 拥有的 active line；allAgents=true 可查看同租户团队目录。" +
			"列表只用于选择，不是恢复上下文；恢复请再调用 shared_line_get。",
	}, activity.KindTrace, activity.AccessRead, auditSharedLineList, h.handleSharedLineList)
	addAuditedTool(h, s, &sdk.Tool{
		Name:        "shared_line_get",
		Description: "读取一条 Shared Line 的完整恢复包（当前位置 + 最近交接）。不传 lineId 时仅在当前 agent 恰有一条 active line 时自动选择；多条时会要求明确 ID。",
	}, activity.KindTrace, activity.AccessRead, auditSharedLineGet, h.handleSharedLineGet)
	addAuditedTool(h, s, &sdk.Tool{
		Name:        "shared_line_update",
		Description: "更新 Shared Line 的当前工作位置。不传 lineId 时：没有则创建、一条则更新、多条则拒绝并返回候选；显式更新同租户他人 line 只记录本次 writer，不转移 owner。",
	}, activity.KindTrace, activity.AccessWrite, auditSharedLineUpdate, h.handleSharedLineUpdate)
	addAuditedTool(h, s, &sdk.Tool{
		Name:        "shared_line_handoff",
		Description: "为一条 Shared Line 写入阶段性交接，并返回该 exact line 的完整恢复包。多条 active line 时应传 lineId。",
	}, activity.KindTrace, activity.AccessWrite, auditSharedLineHandoff, h.handleSharedLineHandoff)
	addAuditedTool(h, s, &sdk.Tool{
		Name:        "shared_line_status",
		Description: "归档或重新激活一条 Shared Line；历史与当前位置保留，不做删除。",
	}, activity.KindTrace, activity.AccessWrite, auditSharedLineStatus, h.handleSharedLineStatus)

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
			Stateless:                    true,
			Logger:                       h.logger,
			MaxRequestBodyBytes:          h.maxRequestBodyBytes,
			PropagateRequestCancellation: true,
		},
	)

	authenticated := auth.RequireBearerToken(h.verifyToken, &auth.RequireBearerTokenOptions{
		// token 允许不过期（一个 agent 多 token 并存是平滑轮换的前提，
		// 而长期驻留的机器 token 未必需要 TTL）。过期判断在领域层，
		// 见 verifyToken 的注释。
		AllowMissingExpiration: true,
	})(h.injectIdentity(h.limitRequests(mcpHandler)))
	return mcpResponseHeaders(h.limitSource(authenticated))
}

// limitSource bounds unauthenticated database lookups as well as authenticated
// calls. It trusts X-Real-IP only from the supported loopback reverse proxy;
// direct peers cannot choose their bucket. The source budget is deliberately
// wider than the per-agent budget so offices behind one NAT do not contend
// before their individual identities are known.
func (h *Host) limitSource(next http.Handler) http.Handler {
	if h.requestLimiter == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		result, err := h.requestLimiter.Allow(
			r.Context(), mcpRateKey("source", mcpClientIP(r)),
			h.requestRateLimit*6, h.requestRateWindow,
		)
		if !h.handleRateResult(w, result, err) {
			return
		}
		next.ServeHTTP(w, r)
	})
}

// limitRequests protects the expensive authenticated path without trusting
// X-Forwarded-For or any other caller-controlled identity. Authentication runs
// first; all tokens for the same agent therefore share one budget, so rotating
// or minting another token cannot bypass the limit. A second tenant-wide budget
// prevents creating more agents from bypassing the aggregate capacity boundary.
// Agent is checked first so a single abusive agent stops consuming the tenant
// budget once its narrower allowance is exhausted. Invalid tokens never create
// persistent limiter keys.
func (h *Host) limitRequests(next http.Handler) http.Handler {
	if h.requestLimiter == nil {
		return next
	}
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		id, ok := identityFrom(r.Context())
		if !ok {
			h.logger.Error("MCP 限流中间件缺少已认证身份")
			http.Error(w, "service unavailable", http.StatusServiceUnavailable)
			return
		}
		result, err := h.requestLimiter.Allow(r.Context(), mcpRateKey("agent", string(id.AgentID)), h.requestRateLimit, h.requestRateWindow)
		if !h.handleRateResult(w, result, err) {
			return
		}
		result, err = h.requestLimiter.Allow(r.Context(), mcpRateKey("tenant", string(id.TenantID)), h.tenantRateLimit, h.requestRateWindow)
		if !h.handleRateResult(w, result, err) {
			return
		}
		next.ServeHTTP(w, r)
	})
}

func (h *Host) handleRateResult(w http.ResponseWriter, result ports.RateLimitResult, err error) bool {
	if err != nil {
		h.logger.Warn("MCP 限流器不可用")
		http.Error(w, "service unavailable", http.StatusServiceUnavailable)
		return false
	}
	if !result.Allowed {
		w.Header().Set("Retry-After", strconv.FormatInt(retryAfterSeconds(result.RetryAfter), 10))
		http.Error(w, "too many requests", http.StatusTooManyRequests)
		return false
	}
	return true
}

func mcpClientIP(r *http.Request) string {
	peer := parseRequestIP(r.RemoteAddr)
	if peer != nil && peer.IsLoopback() {
		if forwarded := net.ParseIP(strings.TrimSpace(r.Header.Get("X-Real-IP"))); forwarded != nil {
			return forwarded.String()
		}
	}
	if peer != nil {
		return peer.String()
	}
	return "unknown"
}

func parseRequestIP(remoteAddr string) net.IP {
	host, _, err := net.SplitHostPort(strings.TrimSpace(remoteAddr))
	if err != nil {
		host = strings.Trim(strings.TrimSpace(remoteAddr), "[]")
	}
	return net.ParseIP(host)
}

func mcpRateKey(scope, value string) string {
	sum := sha256.Sum256([]byte(scope + "\x00" + value))
	return "rate:mcp:" + scope + ":" + hex.EncodeToString(sum[:])
}

func retryAfterSeconds(value time.Duration) int64 {
	seconds := int64((value + time.Second - 1) / time.Second)
	if seconds < 1 {
		return 1
	}
	return seconds
}

// mcpResponseHeaders prevents agents, forward proxies, and browser tooling
// from retaining responses that may contain tenant knowledge. It wraps auth so
// the policy also covers 401/403 and malformed-protocol responses.
func mcpResponseHeaders(next http.Handler) http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		secured := &mcpResponseWriter{ResponseWriter: w}
		secured.applyHeaders()
		next.ServeHTTP(secured, r)
	})
}

// mcpResponseWriter reapplies the policy at commit time because the MCP SDK
// intentionally sets its own no-cache/no-transform directive after middleware
// entry. no-cache alone still permits storage; no-store is the required
// boundary for tenant knowledge.
type mcpResponseWriter struct {
	http.ResponseWriter
	wroteHeader bool
}

func (w *mcpResponseWriter) Unwrap() http.ResponseWriter { return w.ResponseWriter }

func (w *mcpResponseWriter) applyHeaders() {
	hdr := w.Header()
	hdr.Set("Cache-Control", "no-store, no-transform")
	hdr.Set("Pragma", "no-cache")
	hdr.Set("X-Content-Type-Options", "nosniff")
	hdr.Set("X-Robots-Tag", "noindex, nofollow, noarchive")
}

func (w *mcpResponseWriter) WriteHeader(status int) {
	if w.wroteHeader {
		return
	}
	w.wroteHeader = true
	w.applyHeaders()
	w.ResponseWriter.WriteHeader(status)
}

func (w *mcpResponseWriter) Write(body []byte) (int, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return w.ResponseWriter.Write(body)
}

func (w *mcpResponseWriter) ReadFrom(src io.Reader) (int64, error) {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	return io.Copy(w.ResponseWriter, src)
}

func (w *mcpResponseWriter) Flush() {
	if !w.wroteHeader {
		w.WriteHeader(http.StatusOK)
	}
	_ = http.NewResponseController(w.ResponseWriter).Flush()
}
