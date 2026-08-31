// Command server 是 ClaraCore Server 的唯一入口。
//
// 本文件只做装配（wiring）：加载配置、构造适配器、注入领域服务、启动 Host。
// 任何业务判断都不应出现在这里（docs/05-code-structure.md §4）。
package main

import (
	"context"
	"crypto/tls"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"path/filepath"
	"runtime"
	"sort"
	"strings"
	"syscall"
	"time"

	"claracore-server/internal/adapters/backupstatus"
	"claracore-server/internal/adapters/crypto"
	"claracore-server/internal/adapters/embedding"
	atlasadapter "claracore-server/internal/adapters/external/atlas"
	"claracore-server/internal/adapters/integritystatus"
	"claracore-server/internal/adapters/mysql"
	"claracore-server/internal/adapters/ratelimit"
	redisadapter "claracore-server/internal/adapters/redis"
	"claracore-server/internal/adapters/restorestatus"
	"claracore-server/internal/adapters/tlsclient"
	"claracore-server/internal/buildprovenance"
	"claracore-server/internal/config"
	"claracore-server/internal/domain/activity"
	"claracore-server/internal/domain/continuity"
	externaldomain "claracore-server/internal/domain/external"
	"claracore-server/internal/domain/governance"
	"claracore-server/internal/domain/knowledge"
	"claracore-server/internal/ingest"
	mcphost "claracore-server/internal/mcp"
	"claracore-server/internal/platform"
	"claracore-server/internal/portableexport"
	"claracore-server/internal/ports"
	"claracore-server/internal/teamimport"
	"claracore-server/internal/web"
	"claracore-server/migrations"
)

// version 是服务端在 MCP initialize 里自报的版本。
// 发布时用 -ldflags "-X main.version=..." 覆盖。
var (
	version      = "0.1.0-dev"
	sourceDigest = "development"
)

func main() {
	versionOnly := flag.Bool("version", false, "输出版本、Go 工具链与目标平台 JSON 后退出")
	verifyExport := flag.String("verify-export", "", "离线校验团队导出 ZIP 后输出 JSON 并退出")
	verifyProvenance := flag.String("verify-provenance", "", "离线校验本发布目录的构建 provenance 后输出 JSON 并退出")
	planImport := flag.String("plan-import", "", "离线预检团队导入 ZIP 的关系与接管边界后输出 JSON 并退出")
	importExport := flag.String("import-export", "", "把已预检的团队导出原子导入空部署后输出 JSON 并退出")
	checkImport := flag.Bool("check-import", false, "只读重建尚未启动服务的团队导入回执后输出 JSON 并退出")
	checkIntegrity := flag.Bool("check-integrity", false, "只读检查全库关键跨表引用完整性后输出 JSON 并退出")
	importAdmin := flag.String("import-admin", "", "导入后接管团队的归档管理员账号；计划阶段可留空以列出候选")
	importPasswordFile := flag.String("import-password-file", "", "团队导入接管管理员的初始密码绝对路径（必须为 0600）")
	confirmImportEmpty := flag.Bool("confirm-import-empty", false, "确认只向空部署导入，并接受除接管管理员外所有成员保持禁用")
	migrateOnly := flag.Bool("migrate", false, "应用数据库迁移后退出，不启动服务")
	checkOnly := flag.Bool("check", false, "只读验证配置、MySQL 与数据库迁移状态后退出")
	checkUpgradeOnly := flag.Bool("check-upgrade", false, "只读检查升级所需迁移、停机与回滚边界后退出")
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
	actions := 0
	for _, active := range []bool{*versionOnly, *verifyExport != "", *verifyProvenance != "", *planImport != "", *importExport != "", *checkImport, *checkIntegrity, *migrateOnly, *checkOnly, *checkUpgradeOnly, *bootstrap, *ingestDir != ""} {
		if active {
			actions++
		}
	}
	if actions > 1 {
		logger.Error("-version、-verify-export、-verify-provenance、-plan-import、-import-export、-check-import、-check-integrity、-check、-check-upgrade、-migrate、-bootstrap 与 -ingest 只能选择一个")
		os.Exit(1)
	}
	if *verifyExport != "" {
		verification, err := portableexport.VerifyFile(*verifyExport, portableexport.DefaultLimits())
		if err != nil {
			logger.Error("团队导出校验失败", "error", err)
			os.Exit(1)
		}
		if err := writeExportVerification(os.Stdout, verification); err != nil {
			logger.Error("输出团队导出校验结果失败", "error", err)
			os.Exit(1)
		}
		return
	}
	if *planImport != "" {
		archive, err := portableexport.OpenFile(*planImport, portableexport.DefaultLimits())
		if err != nil {
			logger.Error("团队导入归档校验失败", "error", err)
			os.Exit(1)
		}
		defer archive.Close()
		plan, err := teamimport.Analyze(archive, *importAdmin)
		if err != nil {
			logger.Error("团队导入语义预检失败", "error", err)
			os.Exit(1)
		}
		if err := writeImportPlan(os.Stdout, plan); err != nil {
			logger.Error("输出团队导入计划失败", "error", err)
			os.Exit(1)
		}
		return
	}
	var (
		importArchive  *portableexport.Archive
		importPassword string
	)
	if *importExport != "" {
		if strings.TrimSpace(*importAdmin) == "" || !*confirmImportEmpty {
			logger.Error("团队导入必须指定 -import-admin 并显式给出 -confirm-import-empty")
			os.Exit(1)
		}
		archive, err := portableexport.OpenFile(*importExport, portableexport.DefaultLimits())
		if err != nil {
			logger.Error("团队导入归档校验失败", "error", err)
			os.Exit(1)
		}
		importArchive = archive
		defer importArchive.Close()
		if _, err := teamimport.Analyze(importArchive, *importAdmin); err != nil {
			logger.Error("团队导入语义预检失败", "error", err)
			os.Exit(1)
		}
		password, err := readBootstrapPassword(*importPasswordFile)
		if err != nil {
			logger.Error("读取团队导入管理员密码失败", "error", err)
			os.Exit(1)
		}
		importPassword = password
	}
	if *checkImport && strings.TrimSpace(*importAdmin) == "" {
		logger.Error("团队导入回执检查必须指定 -import-admin")
		os.Exit(1)
	}
	binaryDigest, err := executableSHA256()
	if err != nil {
		logger.Error("计算当前二进制摘要失败", "error", err)
		os.Exit(1)
	}
	if *versionOnly {
		if err := writeVersionInfo(os.Stdout, version, sourceDigest, binaryDigest); err != nil {
			logger.Error("输出版本信息失败", "error", err)
			os.Exit(1)
		}
		return
	}
	if *verifyProvenance != "" {
		if !filepath.IsAbs(*verifyProvenance) {
			logger.Error("构建 provenance 必须使用绝对路径")
			os.Exit(1)
		}
		provenanceInfo, err := os.Lstat(*verifyProvenance)
		if err != nil || provenanceInfo.Mode()&os.ModeSymlink != 0 || !provenanceInfo.Mode().IsRegular() {
			logger.Error("构建 provenance 必须是普通文件且不能是符号链接", "error", err)
			os.Exit(1)
		}
		resolvedProvenance, err := filepath.EvalSymlinks(*verifyProvenance)
		if err != nil {
			logger.Error("解析构建 provenance 路径失败", "error", err)
			os.Exit(1)
		}
		executable, err := os.Executable()
		if err != nil {
			logger.Error("定位当前二进制失败", "error", err)
			os.Exit(1)
		}
		executable, err = filepath.EvalSymlinks(executable)
		if err != nil {
			logger.Error("解析当前二进制路径失败", "error", err)
			os.Exit(1)
		}
		result, err := buildprovenance.Verify(buildprovenance.VerifyInput{
			ReleaseDir: filepath.Dir(resolvedProvenance), BinaryPath: executable,
			ProvenancePath: resolvedProvenance, Version: version, SourceDigest: sourceDigest,
			TargetOS: runtime.GOOS, TargetArch: runtime.GOARCH, GoVersion: runtime.Version(),
		})
		if err != nil {
			logger.Error("构建 provenance 校验失败", "error", err)
			os.Exit(1)
		}
		if err := json.NewEncoder(os.Stdout).Encode(result); err != nil {
			logger.Error("输出构建 provenance 校验结果失败", "error", err)
			os.Exit(1)
		}
		return
	}

	cfg, err := config.Load()
	if err != nil {
		// 配置错误一律启动时 fatal，不允许运行期才发现（05 §6.3）
		logger.Error("配置加载失败", "error", err)
		os.Exit(1)
	}

	hasher := crypto.NewArgon2id(crypto.Argon2idParams{
		Memory:      uint32(cfg.Password.MemoryKiB),
		Iterations:  uint32(cfg.Password.Iterations),
		Parallelism: uint8(cfg.Password.Parallelism),
		SaltLength:  16,
		KeyLength:   32,
	})
	clock := platform.SystemClock{}
	ids := platform.NewIDGenerator(clock)
	startedAt := clock.Now()
	mysqlTLS, err := loadDependencyTLS(cfg.MySQL.TLS)
	if err != nil {
		logger.Error("加载 MySQL TLS 配置失败", "error", err)
		os.Exit(1)
	}
	redisTLS, err := loadDependencyTLS(cfg.Redis.TLS)
	if err != nil {
		logger.Error("加载 Redis TLS 配置失败", "error", err)
		os.Exit(1)
	}

	logger.Info("配置就绪",
		"env", cfg.Env,
		"addr", cfg.Addr,
		"mysql_host", cfg.MySQL.Host,
		"mysql_port", cfg.MySQL.Port,
		"mysql_db", cfg.MySQL.Database,
		"mysql_tls", cfg.MySQL.TLS.Mode,
		"redis_enabled", cfg.Redis.Enabled(),
		"redis_tls", cfg.Redis.TLS.Mode,
		"embedding_provider", cfg.Embedding.Provider,
		"public_mcp_url", cfg.Web.PublicMCPURL,
		"secure_cookie", cfg.Web.SecureCookie,
		"metrics_enabled", cfg.Metrics.Enabled(),
	)

	mysqlOptions := make([]mysql.OpenOption, 0, 1)
	if mysqlTLS != nil {
		mysqlOptions = append(mysqlOptions, mysql.WithTLS(mysqlTLS))
	}
	db, err := mysql.Open(context.Background(), cfg.MySQL.DSN(), mysql.PoolConfig{
		MaxOpenConns:    cfg.MySQL.MaxOpenConns,
		MaxIdleConns:    cfg.MySQL.MaxIdleConns,
		ConnMaxLifetime: cfg.MySQL.ConnMaxLifetime,
	}, mysqlOptions...)
	if err != nil {
		logger.Error("数据库不可用", "error", err)
		os.Exit(1)
	}
	defer db.Close()

	// 迁移是显式动作，不在服务启动时隐式发生：
	// 多实例滚动发布时，谁先起来谁改表结构是排查起来最痛苦的一类故障。
	migrator := mysql.NewMigrator(db, migrations.FS, logger)
	if *migrateOnly {
		if err := migrator.Run(context.Background()); err != nil {
			logger.Error("迁移失败", "error", err)
			os.Exit(1)
		}
		return
	}
	if *checkUpgradeOnly {
		plan, err := migrator.Plan(context.Background())
		if err != nil {
			logger.Error("升级计划检查失败", "error", err)
			os.Exit(1)
		}
		if err := writeUpgradePlanSuccess(os.Stdout, version, sourceDigest, binaryDigest, cfg.Env, plan); err != nil {
			logger.Error("输出升级计划失败", "error", err)
			os.Exit(1)
		}
		return
	}
	schemaStatus, err := migrator.Check(context.Background())
	if err != nil {
		logger.Error("数据库迁移状态与当前二进制不兼容；请先显式执行 -migrate", "error", err)
		os.Exit(1)
	}
	logger.Info("数据库迁移状态已验证",
		"latest", schemaStatus.LatestVersion,
		"applied", schemaStatus.Applied,
		"expected", schemaStatus.Expected,
	)
	if *checkOnly {
		if err := writePreflightSuccess(os.Stdout, version, sourceDigest, binaryDigest, cfg.Env, schemaStatus); err != nil {
			logger.Error("输出预检结果失败", "error", err)
			os.Exit(1)
		}
		return
	}
	if *checkImport {
		importEmbeddingModel := ""
		if cfg.Embedding.Enabled() {
			importEmbeddingModel = cfg.Embedding.Model
		}
		result, err := mysql.NewTenantImportChecker(db, schemaStatus.LatestVersion).Check(context.Background(), mysql.TenantImportCheckInput{
			AdminUsername: *importAdmin, EmbeddingModel: importEmbeddingModel,
		})
		if err != nil {
			logger.Error("团队导入回执无法证明", "error", err)
			os.Exit(1)
		}
		if err := writeImportCheckResult(os.Stdout, result); err != nil {
			logger.Error("输出团队导入回执失败", "error", err)
			os.Exit(1)
		}
		return
	}
	if *checkIntegrity {
		result, err := mysql.NewDataIntegrityChecker(db, schemaStatus.LatestVersion).Check(context.Background())
		if err != nil {
			logger.Error("数据完整性检查失败", "error", err)
			os.Exit(1)
		}
		if err := writeDataIntegrityResult(os.Stdout, result); err != nil {
			logger.Error("输出数据完整性检查结果失败", "error", err)
			os.Exit(1)
		}
		if cfg.Web.IntegrityStatusFile != "" {
			status := ports.IntegrityStatus{
				Verified: result.OK, SchemaVersion: result.SchemaVersion, CheckedAt: result.CheckedAt,
				MaxAge: cfg.Web.IntegrityMaxAge, RulesChecked: result.RulesChecked, Violations: result.Violations,
				Issues: make([]ports.IntegrityIssue, 0, len(result.Issues)),
			}
			for _, issue := range result.Issues {
				status.Issues = append(status.Issues, ports.IntegrityIssue{Domain: issue.Domain, Rule: issue.Rule, Violations: issue.Violations})
			}
			if err := integritystatus.Publish(cfg.Web.IntegrityStatusFile, status); err != nil {
				logger.Error("发布数据完整性状态回执失败", "error", err)
				os.Exit(1)
			}
		}
		if !result.OK {
			logger.Error("数据完整性检查发现违规", "violations", result.Violations, "issues", len(result.Issues))
			os.Exit(1)
		}
		return
	}
	if importArchive != nil {
		importEmbeddingModel := ""
		if cfg.Embedding.Enabled() {
			importEmbeddingModel = cfg.Embedding.Model
		}
		result, err := mysql.NewTenantImporter(db, schemaStatus.LatestVersion, hasher, clock).Import(context.Background(), importArchive, mysql.TenantImportInput{
			AdminUsername: *importAdmin, AdminPassword: importPassword, EmbeddingModel: importEmbeddingModel,
		})
		if err != nil {
			logger.Error("团队导入失败", "error", err)
			os.Exit(1)
		}
		if err := writeImportResult(os.Stdout, result); err != nil {
			logger.Error("输出团队导入结果失败", "error", err)
			os.Exit(1)
		}
		return
	}

	// 装配：adapters 实现 domain 定义的接口，Host 只认识 domain。
	// 依赖方向在这里收口，其他任何地方都不该出现这种跨层组装。
	// 密码哈希与会话存储是 Web 治理面的两件基础设施，都在 ports/ 后面。
	// 领域层不读配置，所以 argon2id 的参数在这里注入（config.Password）。
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
	var sharedCache *redisadapter.Cache
	if cfg.Redis.Enabled() {
		sharedCache = redisadapter.NewCache(cfg.Redis.Addr, cfg.Redis.Password, cfg.Redis.Prefix, cfg.Redis.Timeout, redisTLS)
		defer sharedCache.Close()
		pingCtx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		if err := sharedCache.Ping(pingCtx); err != nil {
			logger.Warn("Redis 缓存当前不可用，将自动降级到进程内缓存与 embedding 服务")
		} else {
			logger.Info("Redis embedding 缓存已连接")
		}
		cancel()
	}
	var embeddingCache *embedding.Cached
	if embedder != nil && (cfg.Embedding.CacheEntries > 0 || sharedCache != nil) {
		embeddingCache = embedding.NewTieredCached(embedder, cfg.Embedding.CacheEntries, cfg.Embedding.CacheTTL, sharedCache)
		embedder = embeddingCache
	}
	knowStore := mysql.NewKnowledgeStore(db)
	knowService := knowledge.NewService(knowStore, clock, ids, embedder).WithAutomaticRecallPolicy(knowledge.AutomaticRecallPolicy{
		MinScore: cfg.AutoContext.MinScore, MinMargin: cfg.AutoContext.MinMargin,
	})
	activityStore := mysql.NewActivityStore(db)
	activityService := activity.NewService(activityStore, clock, ids)
	continuityService := continuity.NewService(mysql.NewContinuityStore(db), clock, ids)
	externalService := externaldomain.NewService(mysql.NewExternalStore(db), clock, ids, embedder)
	var (
		atlasSource    *atlasadapter.Source
		externalTenant platform.ID
	)
	if cfg.External.Atlas.Enabled() {
		atlasSource, err = atlasadapter.New(atlasadapter.Config{
			Endpoint: cfg.External.Atlas.Endpoint, Token: cfg.External.Atlas.Token,
			System: cfg.External.Atlas.System, Timeout: cfg.External.Atlas.Timeout,
			MaxWireBytes: cfg.External.Atlas.MaxWireBytes, MaxBodyBytes: cfg.External.Atlas.MaxBodyBytes,
		})
		if err != nil {
			logger.Error("Atlas 外部源配置无效", "error", err)
			os.Exit(1)
		}
		tenant, tenantErr := govService.Tenant(context.Background())
		if tenantErr != nil {
			logger.Error("Atlas 外部源无法解析唯一租户", "error", tenantErr)
			os.Exit(1)
		}
		externalTenant = tenant.ID
	}
	localLoginLimiter := ratelimit.NewMemory(clock, 20_000)
	localMCPLimiter := ratelimit.NewMemory(clock, 20_000)
	httpStats := newHTTPWindowStats()
	var loginLimiter ports.RateLimiter = localLoginLimiter
	var mcpLimiter ports.RateLimiter = localMCPLimiter
	if sharedCache != nil {
		loginLimiter = ratelimit.NewFailover(sharedCache, localLoginLimiter)
		mcpLimiter = ratelimit.NewFailover(sharedCache, localMCPLimiter)
	}

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
		Version:                version,
		MaxRequestBodyBytes:    cfg.HTTP.MCPMaxBodyBytes,
		RequestLimiter:         mcpLimiter,
		RequestRateLimit:       cfg.HTTP.MCPRateLimit,
		TenantRequestRateLimit: cfg.HTTP.MCPTenantRateLimit,
		RequestRateWindow:      cfg.HTTP.MCPRateWindow,
		SemanticSearch:         cfg.Embedding.Enabled(),
		EmbeddingCacheStats: func() (uint64, uint64, bool) {
			if embeddingCache == nil {
				return 0, 0, false
			}
			hits, misses := embeddingCache.Stats()
			return hits, misses, true
		},
		ExternalSearch:     externalService,
		ExternalConfigured: cfg.External.Atlas.Enabled(),
	})
	var redisHealth func(context.Context) error
	if sharedCache != nil {
		redisHealth = sharedCache.Ping
	}
	var backupStatus ports.BackupStatusReader
	if cfg.Web.BackupStatusFile != "" {
		backupStatus = backupstatus.NewFile(cfg.Web.BackupStatusFile)
	}
	var restoreStatus ports.RestoreStatusReader
	if cfg.Web.RestoreStatusFile != "" {
		restoreStatus = restorestatus.NewFile(cfg.Web.RestoreStatusFile)
	}
	var integrityStatus ports.IntegrityStatusReader
	if cfg.Web.IntegrityStatusFile != "" {
		integrityStatus = integritystatus.NewFile(cfg.Web.IntegrityStatusFile)
	}
	var externalHealth func(context.Context, platform.ID) ([]externaldomain.SourceHealth, error)
	var externalRuns func(context.Context, platform.ID, int) ([]externaldomain.SyncRunReceipt, error)
	externalMetrics := newExternalHealthSnapshot(cfg.External.Atlas.Enabled())
	if externalService != nil {
		externalHealth = externalService.Health
		externalRuns = externalService.Runs
	}
	externalSyncRequests := make(chan struct{}, 1)
	var queueExternalSync func(context.Context, platform.ID) (bool, error)
	if cfg.External.Atlas.Enabled() {
		queueExternalSync = func(ctx context.Context, tenantID platform.ID) (bool, error) {
			return queueConfiguredAtlasSync(ctx, externalService, externalSyncRequests, externalTenant, tenantID, cfg.External.Atlas)
		}
	}

	// Host B：治理面。与 MCP Host 并列，共用同一套领域服务——
	// 「Web 专用的写入路径」是设计事故（docs/04-web-scope.md §1.4）。
	webHost := web.NewHost(govService, knowService, continuityService, activityService, logger, web.Options{
		Version:        version,
		SourceDigest:   sourceDigest,
		BinaryDigest:   binaryDigest,
		Env:            cfg.Env,
		DatabaseHealth: db.PingContext,
		DatabasePoolStats: func() web.DatabasePoolStats {
			stats := db.Stats()
			return web.DatabasePoolStats{
				MaxOpenConnections: stats.MaxOpenConnections,
				OpenConnections:    stats.OpenConnections,
				InUse:              stats.InUse,
				Idle:               stats.Idle,
				WaitCount:          stats.WaitCount,
				WaitDuration:       stats.WaitDuration,
				MaxIdleClosed:      stats.MaxIdleClosed,
				MaxIdleTimeClosed:  stats.MaxIdleTimeClosed,
				MaxLifetimeClosed:  stats.MaxLifetimeClosed,
			}
		},
		SchemaVersion: schemaStatus.LatestVersion,
		SchemaHealth: func(ctx context.Context) (string, error) {
			status, err := migrator.Check(ctx)
			return status.LatestVersion, err
		},
		StartedAt:          startedAt,
		Now:                clock.Now,
		ShutdownDrain:      cfg.ShutdownDrain,
		RedisConfigured:    cfg.Redis.Enabled(),
		RedisHealth:        redisHealth,
		EmbeddingProvider:  cfg.Embedding.Provider,
		MySQLTransport:     statefulTransportSummary(cfg.MySQL.TLS, true),
		RedisTransport:     statefulTransportSummary(cfg.Redis.TLS, cfg.Redis.Enabled()),
		EmbeddingTransport: httpTransportSummary(cfg.Embedding.BaseURL, cfg.Embedding.Enabled()),
		ExternalTransport:  httpTransportSummary(cfg.External.Atlas.Endpoint, cfg.External.Atlas.Enabled()),
		LoginRateLimiter:   loginLimiter,
		LoginRateLimit:     cfg.Web.LoginRateLimit,
		LoginRateWindow:    cfg.Web.LoginRateWindow,
		MCPRateLimit:       cfg.HTTP.MCPRateLimit,
		MCPTenantRateLimit: cfg.HTTP.MCPTenantRateLimit,
		MCPRateWindow:      cfg.HTTP.MCPRateWindow,
		MetricsAddr:        cfg.Metrics.Addr,
		BackupStatus:       backupStatus,
		RestoreStatus:      restoreStatus,
		IntegrityStatus:    integrityStatus,
		TenantCapacity:     mysql.NewTenantCapacityReader(db),
		MaxFormBytes:       cfg.HTTP.WebMaxFormBytes,
		ExportMaxBytes:     cfg.Web.ExportMaxBytes,
		TenantExporter:     mysql.NewTenantExporter(db, schemaStatus.LatestVersion),
		HTTPRequestStats: func() web.HTTPRequestStats {
			return httpStats.Snapshot(clock.Now())
		},
		ExternalConfigured: cfg.External.Atlas.Enabled(),
		ExternalSources:    externalHealth,
		ExternalSyncRuns:   externalRuns,
		QueueExternalSync:  queueExternalSync,
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

	readiness := newReadinessGate()
	probes := probeOptions{
		Version:      version,
		SourceDigest: sourceDigest,
		BinaryDigest: binaryDigest,
		Env:          cfg.Env,
		InstanceID:   cfg.InstanceID,
		Now:          clock.Now,
		NewTraceID: func() string {
			return string(ids.New())
		},
		Accepting:       readiness.Accepting,
		DrainRetryAfter: cfg.ShutdownDrain,
		DatabaseHealth:  db.PingContext,
		SchemaHealth:    migrator.Check,
	}
	mux.HandleFunc("GET /livez", newLivenessProbe(probes))
	mux.HandleFunc("GET /readyz", newReadinessProbe(probes))
	// /healthz 保留兼容旧监控，但语义收紧为 readiness：MySQL 可达且迁移
	// 与当前二进制精确匹配。新部署应显式区分 /livez 与 /readyz。
	mux.HandleFunc("GET /healthz", newReadinessProbe(probes))

	// Web 挂在根路径，放在最后：ServeMux 的最长前缀匹配保证
	// /mcp 与三个探针仍然各归各的。
	mux.Handle("/", webHost.Handler())

	srv := newHTTPServer(
		cfg.Addr,
		observeHTTP(
			rejectNewRequestsWhileDraining(mux, readiness.Accepting, cfg.ShutdownDrain),
			logger,
			func() string { return string(ids.New()) },
			clock.Now,
			httpStats,
		),
		cfg.HTTP,
		logger,
	)
	var metricsSrv *http.Server
	if cfg.Metrics.Enabled() {
		metricsSrv = newHTTPServer(cfg.Metrics.Addr, newMetricsHandler(metricsOptions{
			Version:            version,
			SourceDigest:       sourceDigest,
			BinaryDigest:       binaryDigest,
			Environment:        cfg.Env,
			SchemaVersion:      schemaStatus.LatestVersion,
			MCPAgentRateLimit:  cfg.HTTP.MCPRateLimit,
			MCPTenantRateLimit: cfg.HTTP.MCPTenantRateLimit,
			MCPRateWindow:      cfg.HTTP.MCPRateWindow,
			StartedAt:          startedAt,
			Now:                clock.Now,
			Accepting:          readiness.Accepting,
			HTTPStats: func() web.HTTPRequestStats {
				return httpStats.Snapshot(clock.Now())
			},
			DatabasePool:    db.Stats,
			BackupStatus:    backupStatus,
			RestoreStatus:   restoreStatus,
			IntegrityStatus: integrityStatus,
			ExternalHealth:  externalMetrics.Load,
		}), cfg.HTTP, logger)
	}

	// 优雅关闭：收到信号后停止接收新请求，等待在途请求完成
	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()

	// 索引 worker 跟着同一个 ctx 停。它不属于请求路径：
	// 写入只负责把内容排进队列（同事务落一行 pending），向量在这里补上。
	indexerDone := make(chan struct{})
	go runIndexer(ctx, knowService, cfg.Embedding, sharedCache, logger, indexerDone)

	// 外部活源与本地内容索引分开：它不使用保存 source_text 的内部队列，
	// 每轮只在内存中取正文并落 body-free 向量镜像。
	externalSyncDone := make(chan struct{})
	go runAtlasSync(ctx, externalService, atlasSource, externalTenant, cfg.External.Atlas, externalSyncRequests, logger, externalSyncDone)
	externalHealthDone := make(chan struct{})
	go runExternalHealthSampler(
		ctx, cfg.Metrics.Enabled() && cfg.External.Atlas.Enabled(), externalService, externalTenant,
		cfg.External.Atlas.SyncInterval, cfg.External.Atlas.SyncTimeout, clock.Now, externalMetrics, logger, externalHealthDone,
	)

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
	if metricsSrv != nil {
		go func() {
			logger.Info("本机指标监听启动", "addr", cfg.Metrics.Addr)
			if err := metricsSrv.ListenAndServe(); err != nil && !errors.Is(err, http.ErrServerClosed) {
				logger.Error("指标监听异常退出", "error", err)
				os.Exit(1)
			}
		}()
	}

	<-ctx.Done()
	readiness.StartDraining()
	logger.Info("收到关闭信号，实例已停止接收新业务请求", "drain", cfg.ShutdownDrain)
	if cfg.ShutdownDrain > 0 {
		timer := time.NewTimer(cfg.ShutdownDrain)
		<-timer.C
	}
	logger.Info("摘流等待完成，正在关闭 HTTP listener")

	shutdownCtx, cancel := context.WithTimeout(context.Background(), 15*time.Second)
	defer cancel()
	if err := srv.Shutdown(shutdownCtx); err != nil {
		logger.Error("关闭超时", "error", err)
	}
	if metricsSrv != nil {
		if err := metricsSrv.Shutdown(shutdownCtx); err != nil {
			logger.Error("关闭指标监听超时", "error", err)
		}
	}
	<-indexerDone
	<-externalSyncDone
	<-externalHealthDone
	<-traceCleanupDone
	<-sessionCleanupDone
	logger.Info("已退出")
}

func notifyExternalSync(queue chan<- struct{}) {
	select {
	case queue <- struct{}{}:
	default:
	}
}

type atlasSyncRequester interface {
	RequestSync(context.Context, externaldomain.RequestSyncInput) (bool, error)
}

type atlasSyncService interface {
	atlasSyncRequester
	Sync(context.Context, externaldomain.SyncInput) (externaldomain.SyncReport, error)
	ClaimSyncRequest(context.Context, platform.ID, string, time.Duration) (externaldomain.SyncRequestClaim, bool, error)
	ResolveSyncRequest(context.Context, externaldomain.SyncRequestClaim, bool) error
}

func queueConfiguredAtlasSync(ctx context.Context, svc atlasSyncRequester, wake chan<- struct{}, configuredTenant, requestedTenant platform.ID, cfg config.AtlasExternal) (bool, error) {
	if err := ctx.Err(); err != nil {
		return false, err
	}
	if svc == nil || configuredTenant == "" || requestedTenant == "" || requestedTenant != configuredTenant {
		return false, errors.New("external sync tenant does not match configured source")
	}
	accepted, err := svc.RequestSync(ctx, externaldomain.RequestSyncInput{
		TenantID: requestedTenant, SpaceID: platform.ID(cfg.SpaceID), Kind: "atlas_mcp",
		Name: cfg.Name, System: cfg.System,
	})
	if err == nil && accepted {
		notifyExternalSync(wake)
	}
	return accepted, err
}

func runAtlasSync(ctx context.Context, svc atlasSyncService, remote externaldomain.Source, tenantID platform.ID, cfg config.AtlasExternal, requests <-chan struct{}, logger *slog.Logger, done chan<- struct{}) {
	defer close(done)
	if !cfg.Enabled() || svc == nil || remote == nil {
		logger.Info("未配置 Atlas 外部源，同步 worker 不启动")
		return
	}
	logger.Info("Atlas 外部源同步 worker 启动", "source", cfg.Name, "system", cfg.System, "interval", cfg.SyncInterval)
	scheduled := time.NewTicker(cfg.SyncInterval)
	defer scheduled.Stop()
	requestPoll := time.NewTicker(5 * time.Second)
	defer requestPoll.Stop()
	if !runClaimedAtlasSync(ctx, svc, remote, tenantID, cfg, logger) {
		runAtlasSyncOnce(ctx, svc, remote, tenantID, cfg, logger)
	}
	for {
		select {
		case <-ctx.Done():
			logger.Info("Atlas 外部源同步 worker 退出")
			return
		case <-scheduled.C:
			if !runClaimedAtlasSync(ctx, svc, remote, tenantID, cfg, logger) {
				runAtlasSyncOnce(ctx, svc, remote, tenantID, cfg, logger)
			}
		case <-requests:
			runClaimedAtlasSync(ctx, svc, remote, tenantID, cfg, logger)
		case <-requestPoll.C:
			runClaimedAtlasSync(ctx, svc, remote, tenantID, cfg, logger)
		}
	}
}

func runAtlasSyncOnce(ctx context.Context, svc atlasSyncService, remote externaldomain.Source, tenantID platform.ID, cfg config.AtlasExternal, logger *slog.Logger) error {
	syncCtx, cancel := context.WithTimeout(ctx, cfg.SyncTimeout)
	report, err := svc.Sync(syncCtx, externaldomain.SyncInput{
		TenantID: tenantID, SpaceID: platform.ID(cfg.SpaceID), Kind: "atlas_mcp",
		Name: cfg.Name, System: cfg.System, PageSize: cfg.PageSize, MaxDuration: cfg.SyncTimeout, Remote: remote,
	})
	cancel()
	switch {
	case errors.Is(err, externaldomain.ErrSyncBusy):
		logger.Info("Atlas 外部源已有其他实例同步，本轮跳过", "source", cfg.Name)
	case err != nil && ctx.Err() == nil:
		logger.Warn("Atlas 外部源同步失败，保留上一版可用镜像", "source", cfg.Name, "error", err)
	case err == nil:
		logger.Info("Atlas 外部源同步完成", "source", cfg.Name, "seen", report.Seen,
			"indexed", report.Indexed, "unchanged", report.Unchanged, "missing", report.Missing)
	}
	return err
}

func runClaimedAtlasSync(ctx context.Context, svc atlasSyncService, remote externaldomain.Source, tenantID platform.ID, cfg config.AtlasExternal, logger *slog.Logger) bool {
	claim, claimed, err := svc.ClaimSyncRequest(ctx, tenantID, cfg.Name, cfg.SyncTimeout)
	if err != nil {
		logger.Warn("领取 Atlas 手动同步请求失败", "source", cfg.Name, "error", err)
		return false
	}
	if !claimed {
		return false
	}
	logger.Info("Atlas 外部源开始处理管理员同步请求", "source", cfg.Name, "request", claim.RequestID)
	err = runAtlasSyncOnce(ctx, svc, remote, tenantID, cfg, logger)
	retry := errors.Is(err, externaldomain.ErrSyncBusy) || ctx.Err() != nil
	resolveCtx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
	defer cancel()
	if resolveErr := svc.ResolveSyncRequest(resolveCtx, claim, retry); resolveErr != nil {
		logger.Warn("更新 Atlas 手动同步请求状态失败；租约过期后可由其他实例接管", "source", cfg.Name, "error", resolveErr)
	} else if retry {
		logger.Info("Atlas 手动同步请求已释放，等待其他实例重试", "source", cfg.Name)
	}
	return true
}

func newHTTPServer(addr string, handler http.Handler, limits config.HTTP, logger *slog.Logger) *http.Server {
	return &http.Server{
		Addr:              addr,
		Handler:           handler,
		ReadHeaderTimeout: limits.ReadHeaderTimeout,
		ReadTimeout:       limits.ReadTimeout,
		WriteTimeout:      limits.WriteTimeout,
		IdleTimeout:       limits.IdleTimeout,
		MaxHeaderBytes:    limits.MaxHeaderBytes,
		ErrorLog:          slog.NewLogLogger(logger.Handler(), slog.LevelError),
	}
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

func loadDependencyTLS(cfg config.ClientTLS) (*tls.Config, error) {
	if !cfg.Enabled() {
		return nil, nil
	}
	return tlsclient.Load(tlsclient.Files{
		CAFile: cfg.CAFile, CertFile: cfg.CertFile, KeyFile: cfg.KeyFile, ServerName: cfg.ServerName,
	})
}

func statefulTransportSummary(tlsConfig config.ClientTLS, configured bool) string {
	if !configured {
		return "未配置"
	}
	if tlsConfig.Enabled() {
		return "TLS 1.2 或更高版本，验证服务端身份；可选私有 CA / mTLS"
	}
	return "明文连接；生产仅允许 loopback"
}

func httpTransportSummary(baseURL string, configured bool) string {
	if !configured {
		return "未配置"
	}
	if strings.HasPrefix(strings.ToLower(baseURL), "https://") {
		return "HTTPS，验证服务端身份"
	}
	return "HTTP；生产仅允许 loopback"
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
func runIndexer(ctx context.Context, svc *knowledge.Service, cfg config.Embedding, locker ports.Locker, logger *slog.Logger, done chan<- struct{}) {
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
			var lease ports.Lease
			if locker != nil {
				var acquired bool
				var err error
				lease, acquired, err = locker.TryLock(ctx, "lock:indexer:"+cfg.Model, indexerLeaseTTL(cfg.Timeout))
				if err != nil {
					// Redis is optional. A lock outage must not turn an otherwise
					// healthy single-instance deployment into a permanent queue stall.
					logger.Warn("索引租约不可用，本轮按单实例语义继续", "error", err)
					lease = nil
				} else if !acquired {
					break
				}
			}
			report, err := svc.IndexPending(ctx, cfg.IndexBatch)
			releaseIndexerLease(lease, logger)
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

func indexerLeaseTTL(embeddingTimeout time.Duration) time.Duration {
	if embeddingTimeout <= 0 {
		embeddingTimeout = 30 * time.Second
	}
	return 2*embeddingTimeout + 5*time.Second
}

func releaseIndexerLease(lease ports.Lease, logger *slog.Logger) {
	if lease == nil {
		return
	}
	ctx, cancel := context.WithTimeout(context.Background(), 2*time.Second)
	defer cancel()
	if err := lease.Release(ctx); err != nil {
		// TTL still bounds the failure. Do not fail the completed indexing batch.
		logger.Warn("索引租约释放失败，将等待 TTL 自动过期", "error", err)
	}
}
