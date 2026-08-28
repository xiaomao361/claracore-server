// Command server 是 ClaraCore Server 的唯一入口。
//
// 本文件只做装配（wiring）：加载配置、构造适配器、注入领域服务、启动 Host。
// 任何业务判断都不应出现在这里（docs/05-code-structure.md §4）。
package main

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"syscall"
	"time"

	"claracore-server/internal/adapters/crypto"
	"claracore-server/internal/adapters/embedding"
	"claracore-server/internal/adapters/mysql"
	"claracore-server/internal/config"
	"claracore-server/internal/domain/activity"
	"claracore-server/internal/domain/continuity"
	"claracore-server/internal/domain/governance"
	"claracore-server/internal/domain/knowledge"
	"claracore-server/internal/ingest"
	mcphost "claracore-server/internal/mcp"
	"claracore-server/internal/platform"
	"claracore-server/internal/ports"
	"claracore-server/internal/web"
	"claracore-server/migrations"
)

// version 是服务端在 MCP initialize 里自报的版本。
// 发布时用 -ldflags "-X main.version=..." 覆盖。
var version = "0.1.0-dev"

func main() {
	migrateOnly := flag.Bool("migrate", false, "应用数据库迁移后退出，不启动服务")
	bootstrap := flag.Bool("bootstrap", false, "在空数据库中初始化首个租户与管理员后退出")
	bootstrapTenant := flag.String("bootstrap-tenant", "ClaraCore 团队", "首次初始化的租户名")
	bootstrapTeamSpace := flag.String("bootstrap-team-space", "团队记忆", "首次初始化的团队空间名")
	bootstrapAdmin := flag.String("bootstrap-admin", "admin", "首次初始化的管理员账号")
	bootstrapAdminDisplay := flag.String("bootstrap-admin-display", "管理员", "首次初始化的管理员显示名")
	bootstrapPasswordFile := flag.String("bootstrap-password-file", "", "保存初始管理员密码的绝对路径（必须为 0600）")
	ingestDir := flag.String("ingest", "", "摄取一个 Markdown 目录后退出（需同时给 -ingest-space 与 -ingest-source）")
	ingestSpace := flag.String("ingest-space", "", "摄取的目标空间 ID")
	ingestSource := flag.String("ingest-source", "", "源名，跨机器稳定（如 clara-docs）。它参与 source_key，改了等于换了一个源")
	ingestUser := flag.String("ingest-user", "", "摄取产物记在哪个用户名下（用户 ID）")
	flag.Parse()

	logger := slog.New(slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: slog.LevelInfo}))

	cfg, err := config.Load()
	if err != nil {
		// 配置错误一律启动时 fatal，不允许运行期才发现（05 §6.3）
		logger.Error("配置加载失败", "error", err)
		os.Exit(1)
	}

	clock := platform.SystemClock{}
	ids := platform.NewIDGenerator(clock)

	logger.Info("配置就绪",
		"env", cfg.Env,
		"addr", cfg.Addr,
		"mysql_host", cfg.MySQL.Host,
		"mysql_port", cfg.MySQL.Port,
		"mysql_db", cfg.MySQL.Database,
		"redis_enabled", cfg.Redis.Enabled(),
		"embedding_provider", cfg.Embedding.Provider,
		"public_mcp_url", cfg.Web.PublicMCPURL,
		"secure_cookie", cfg.Web.SecureCookie,
	)

	db, err := mysql.Open(context.Background(), cfg.MySQL.DSN(), mysql.PoolConfig{
		MaxOpenConns:    cfg.MySQL.MaxOpenConns,
		MaxIdleConns:    cfg.MySQL.MaxIdleConns,
		ConnMaxLifetime: cfg.MySQL.ConnMaxLifetime,
	})
	if err != nil {
		logger.Error("数据库不可用", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	// 迁移是显式动作，不在服务启动时隐式发生：
	// 多实例滚动发布时，谁先起来谁改表结构是排查起来最痛苦的一类故障。
	if *migrateOnly {
		if err := mysql.NewMigrator(db, migrations.FS, logger).Run(context.Background()); err != nil {
			logger.Error("迁移失败", "error", err)
			os.Exit(1)
		}
		return
	}

	// 装配：adapters 实现 domain 定义的接口，Host 只认识 domain。
	// 依赖方向在这里收口，其他任何地方都不该出现这种跨层组装。
	// 密码哈希与会话存储是 Web 治理面的两件基础设施，都在 ports/ 后面。
	// 领域层不读配置，所以 argon2id 的参数在这里注入（config.Password）。
	hasher := crypto.NewArgon2id(crypto.Argon2idParams{
		Memory:      uint32(cfg.Password.MemoryKiB),
		Iterations:  uint32(cfg.Password.Iterations),
		Parallelism: uint8(cfg.Password.Parallelism),
		SaltLength:  16,
		KeyLength:   32,
	})
	sessions := mysql.NewSessionStore(db)

	govStore := mysql.NewGovernanceStore(db)
	govService := governance.NewService(govStore, clock, ids, hasher, sessions)
	if *bootstrap {
		err := runBootstrap(context.Background(), govService, bootstrapArgs{
			Tenant:       *bootstrapTenant,
			TeamSpace:    *bootstrapTeamSpace,
			Admin:        *bootstrapAdmin,
			AdminDisplay: *bootstrapAdminDisplay,
			PasswordFile: *bootstrapPasswordFile,
		})
		if err != nil {
			logger.Error("初始化失败", "error", err)
			os.Exit(1)
		}
		logger.Info("初始化完成；管理员首次登录后必须修改密码",
			"tenant", *bootstrapTenant, "admin", *bootstrapAdmin)
		return
	}

	// embedder 为 nil 是**降级形态而非异常形态**：不入索引队列，
	// 检索退化成中文全文匹配，系统功能保持完整（01-architecture.md §6.1）。
	embedder := newEmbedder(cfg.Embedding)
	var embeddingCache *embedding.Cached
	if embedder != nil && cfg.Embedding.CacheEntries > 0 {
		embeddingCache = embedding.NewCached(embedder, cfg.Embedding.CacheEntries, cfg.Embedding.CacheTTL)
		embedder = embeddingCache
	}
	knowStore := mysql.NewKnowledgeStore(db)
	knowService := knowledge.NewService(knowStore, clock, ids, embedder).WithAutomaticRecallPolicy(knowledge.AutomaticRecallPolicy{
		MinScore: cfg.AutoContext.MinScore, MinMargin: cfg.AutoContext.MinMargin,
	})
	activityStore := mysql.NewActivityStore(db)
	activityService := activity.NewService(activityStore, clock, ids)
	continuityService := continuity.NewService(mysql.NewContinuityStore(db), clock, ids)

	// 摄取是显式动作，与 -migrate 同一形态：跑完就退，不启动服务。
	// 它天生幂等，可以反复跑——「源文档更新了要重扫」就是常态。
	if *ingestDir != "" {
		if err := runIngest(context.Background(), govService, knowService, logger, ingestArgs{
			Dir: *ingestDir, Space: *ingestSpace, Source: *ingestSource, User: *ingestUser,
		}); err != nil {
			logger.Error("摄取失败", "error", err)
			os.Exit(1)
		}
		return
	}

	mcpHost := mcphost.NewHost(govService, knowService, continuityService, activityService, logger, mcphost.HostOptions{
		Version:        version,
		SemanticSearch: cfg.Embedding.Enabled(),
		EmbeddingCacheStats: func() (uint64, uint64, bool) {
			if embeddingCache == nil {
				return 0, 0, false
			}
			hits, misses := embeddingCache.Stats()
			return hits, misses, true
		},
	})

	// Host B：治理面。与 MCP Host 并列，共用同一套领域服务——
	// 「Web 专用的写入路径」是设计事故（docs/04-web-scope.md §1.4）。
	webHost := web.NewHost(govService, knowService, continuityService, activityService, logger, web.Options{
		Version:           version,
		Env:               cfg.Env,
		DatabaseHealth:    db.PingContext,
		RedisConfigured:   cfg.Redis.Enabled(),
		EmbeddingProvider: cfg.Embedding.Provider,
		EmbeddingCacheStats: func() (uint64, uint64, bool) {
			if embeddingCache == nil {
				return 0, 0, false
			}
			hits, misses := embeddingCache.Stats()
			return hits, misses, true
		},
		PublicMCPURL: cfg.Web.PublicMCPURL,
		SecureCookie: cfg.Web.SecureCookie,
		IngestRoot:   cfg.Web.IngestRoot,
		ScanMarkdown: func(ctx context.Context, sourceName, root string) ([]knowledge.Document, error) {
			return ingest.NewMarkdownDir(sourceName, root).Scan(ctx)
		},
	})

	mux := http.NewServeMux()

	// MCP 入口。认证在 Host 内部（bearer token），这里不加任何前置判断——
	// 多一处身份判断就多一处可能与 governance.Authenticate 不一致的地方。
	mux.Handle("/mcp", mcpHost.Handler())
	mux.Handle("/mcp/", mcpHost.Handler())

	mux.HandleFunc("GET /healthz", func(w http.ResponseWriter, r *http.Request) {
		status := map[string]any{
			"ok":      true,
			"env":     cfg.Env,
			"time":    clock.Now().Format(time.RFC3339),
			"traceId": ids.New(),
		}
		w.Header().Set("Content-Type", "application/json")
		if err := db.PingContext(r.Context()); err != nil {
			status["ok"] = false
			status["mysql"] = "unreachable"
			w.WriteHeader(http.StatusServiceUnavailable) // 必须在写 body 前，且晚于 Header().Set
		}
		json.NewEncoder(w).Encode(status)
	})

	// Web 挂在根路径，放在最后：ServeMux 的最长前缀匹配保证
	// /mcp 与 /healthz 仍然各归各的。
	mux.Handle("/", webHost.Handler())

	srv := &http.Server{
		Addr:              cfg.Addr,
		Handler:           mux,
		ReadHeaderTimeout: 10 * time.Second,
	}

	// 优雅关闭：收到信号后停止接收新请求，等待在途请求完成
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 索引 worker 跟着同一个 ctx 停。它不属于请求路径：
	// 写入只负责把内容排进队列（同事务落一行 pending），向量在这里补上。
	indexerDone := make(chan struct{})
	go runIndexer(ctx, knowService, cfg.Embedding, logger, indexerDone)

	// 调用 trace 只保留 30 天；治理和元审计不走这条删除路径。
	// worker 启动即清一次，之后每日清理，并跟随服务上下文退出。
	traceCleanupDone := make(chan struct{})
	go runTraceCleanup(ctx, activityService, clock, logger, traceCleanupDone)

	// 数据库会话负责即时吊销与滑动过期；独立 worker 回收已经失效的行，
	// 避免长期运行后 sessions 表只增不减。
	sessionCleanupDone := make(chan struct{})
	go runSessionCleanup(ctx, govService.CleanupSessions, logger, sessionCleanupDone)

	go func() {
		logger.Info("服务启动", "addr", cfg.Addr)
		if err := srv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
			logger.Error("服务异常退出", "error", err)
			os.Exit(1)
		}
	}()

	<-ctx.Done()
	logger.Info("收到关闭信号，正在退出")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("关闭超时", "error", err)
	}
	<-indexerDone
	<-traceCleanupDone
	<-sessionCleanupDone
	logger.Info("已退出")
}

// newEmbedder 按配置构造 Embedder，未启用时返回 nil。
//
// 返回 nil 而不是一个「什么都不做的 Embedder」：空实现的 Model() 得编一个
// 模型名出来，而那个名字会被写进 embeddings.model 列，将来真的接上模型时
// 没人分得清哪些行是它留下的。领域层只在一处判空，代价比那个小得多。
func newEmbedder(cfg config.Embedding) ports.Embedder {
	switch cfg.Provider {
	case config.ProviderOllama:
		return embedding.NewOllama(cfg.BaseURL, cfg.Model, cfg.APIKey, cfg.Timeout)
	case config.ProviderOpenAICompat:
		return embedding.NewOpenAICompat(cfg.BaseURL, cfg.Model, cfg.APIKey, cfg.Timeout)
	default:
		return nil
	}
}

type ingestArgs struct {
	Dir, Space, Source, User string
}

type bootstrapArgs struct {
	Tenant, TeamSpace, Admin, AdminDisplay, PasswordFile string
}

func runBootstrap(ctx context.Context, gov *governance.Service, args bootstrapArgs) error {
	password, err := readBootstrapPassword(args.PasswordFile)
	if err != nil {
		return err
	}
	_, err = gov.BootstrapTenant(ctx, governance.ProvisionTenantInput{
		TenantName:    args.Tenant,
		TeamSpaceName: args.TeamSpace,
		AdminUsername: args.Admin,
		AdminDisplay:  args.AdminDisplay,
		AdminPassword: password,
	})
	return err
}

func readBootstrapPassword(path string) (string, error) {
	if path == "" {
		return "", fmt.Errorf("缺少 -bootstrap-password-file")
	}
	if !filepath.IsAbs(path) {
		return "", fmt.Errorf("-bootstrap-password-file 必须是绝对路径")
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", fmt.Errorf("读取初始密码文件: %w", err)
	}
	if !info.Mode().IsRegular() {
		return "", fmt.Errorf("初始密码文件必须是普通文件")
	}
	if info.Mode().Perm()&0o077 != 0 {
		return "", fmt.Errorf("初始密码文件权限必须为 0600（当前 %04o）", info.Mode().Perm())
	}
	if info.Size() > 4096 {
		return "", fmt.Errorf("初始密码文件过大")
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return "", fmt.Errorf("读取初始密码文件: %w", err)
	}
	password := strings.TrimSuffix(strings.TrimSuffix(string(raw), "\n"), "\r")
	if strings.ContainsAny(password, "\r\n") {
		return "", fmt.Errorf("初始密码文件只能包含一行")
	}
	return password, nil
}

// runIngest 把一个目录摄进指定空间。
//
// **不做权限判断**：这是本机命令行，跑它的人已经有服务器权限了，
// 再套一层空间授权检查只是仪式。将来 Web 上的摄取管理页要走
// governance.SpaceScope.ResolveWriteTarget——那时调用方是浏览器里的人，
// 权限才真的是一道门。
func runIngest(ctx context.Context, gov *governance.Service, know *knowledge.Service, logger *slog.Logger, args ingestArgs) error {
	var missing []string
	for name, v := range map[string]string{
		"-ingest-space": args.Space, "-ingest-source": args.Source, "-ingest-user": args.User,
	} {
		if v == "" {
			missing = append(missing, name)
		}
	}
	if len(missing) > 0 {
		sort.Strings(missing) // 错误信息要稳定，map 遍历顺序是随机的
		return fmt.Errorf("缺少参数: %s", strings.Join(missing, ", "))
	}

	source := ingest.NewMarkdownDir(args.Source, args.Dir)
	logger.Info("开始扫描", "source", source.Name(), "dir", args.Dir)

	docs, err := source.Scan(ctx)
	scanComplete := err == nil
	if err != nil {
		// 扫描出问题也可能带回了一批好文档——先把问题说出来，再摄那一批。
		// 直接退出的话，一个坏文件会挡住其余几千份。
		logger.Warn("扫描时有文件读取失败", "error", err)
	}
	if len(docs) == 0 {
		return fmt.Errorf("没有扫到任何 .md 文档")
	}

	// 租户由部署解析，不再多加一个 flag：「一个团队一套部署单元」
	// （02 §2.1），而那条规则只住在 governance.Tenant 里。
	// 库里有多于一个活跃租户时它会报错而不是猜——那正是想要的。
	tenant, err := gov.Tenant(ctx)
	if err != nil {
		return err
	}

	report, err := know.Ingest(ctx, knowledge.IngestInput{
		TenantID:      tenant.ID,
		SpaceID:       platform.ID(args.Space),
		AuthorUserID:  platform.ID(args.User),
		SourceName:    args.Source,
		DetectMissing: scanComplete,
		Documents:     docs,
	})
	if err != nil {
		return err
	}

	logger.Info("摄取完成",
		"scanned", len(docs),
		"created", report.Created, "updated", report.Updated,
		"unchanged", report.Unchanged, "failed", len(report.Failed))
	for _, missing := range report.Missing {
		logger.Warn("源文件已缺失（未自动修改卡片）", "card_id", missing.CardID, "source", missing.SourcePath)
	}
	for _, f := range report.Failed {
		// 失败清单要逐条打出来：只说「失败 3 条」等于没说，
		// 人得能照着去修那一份
		logger.Warn("摄取失败项", "source", f.SourcePath, "reason", f.Reason)
	}
	if report.Created+report.Updated > 0 {
		logger.Info("提醒：摄取产物是 draft 原料，不进默认检索。" +
			"要让它们能被 card_recall 命中，需要 agent 补上提炼与召回条件，再由人确认")
	}
	return nil
}

// runIndexer 是后台索引 worker：把队列里的 pending 向量补上。
//
// 三条纪律：
//
//   - **失败不退出**。最常见的失败是 embedding 服务临时不可用，
//     那一批已经被标记并累加了 attempts，下一轮会重试。worker 退出
//     只会让积压变成永久积压。
//   - **一轮里持续抽干**。领到满批说明还有积压，立刻再来一轮而不是
//     等下一个 tick——否则一次批量摄取要花 (条数/批量) × 间隔 才追得完。
//   - **随 ctx 停**。正在跑的那一批做完就退，不打断它：中途放弃只会
//     留下一批已经算好却没写回的向量，那次外部调用就白花了。
func runIndexer(ctx context.Context, svc *knowledge.Service, cfg config.Embedding, logger *slog.Logger, done chan<- struct{}) {
	defer close(done)
	if !cfg.Enabled() {
		logger.Info("未配置 embedding，索引 worker 不启动；检索降级为全文匹配")
		return
	}
	logger.Info("索引 worker 启动", "interval", cfg.IndexInterval, "batch", cfg.IndexBatch, "model", cfg.Model)

	ticker := time.NewTicker(cfg.IndexInterval)
	defer ticker.Stop()
	for {
		for {
			report, err := svc.IndexPending(ctx, cfg.IndexBatch)
			if err != nil {
				logger.Warn("索引失败，将在下一轮重试",
					"picked", report.Picked, "failed", report.Failed, "error", err)
				break
			}
			if report.Indexed > 0 {
				logger.Info("索引完成", "indexed", report.Indexed, "failed", report.Failed)
			}
			if report.Picked < cfg.IndexBatch {
				break // 没领满，说明队列空了
			}
		}
		select {
		case <-ctx.Done():
			logger.Info("索引 worker 退出")
			return
		case <-ticker.C:
		}
	}
}
