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
	"log/slog"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"claracore-server/internal/adapters/embedding"
	"claracore-server/internal/adapters/mysql"
	"claracore-server/internal/config"
	"claracore-server/internal/domain/governance"
	"claracore-server/internal/domain/knowledge"
	mcphost "claracore-server/internal/mcp"
	"claracore-server/internal/platform"
	"claracore-server/internal/ports"
	"claracore-server/migrations"
)

// version 是服务端在 MCP initialize 里自报的版本。
// 发布时用 -ldflags "-X main.version=..." 覆盖。
var version = "0.1.0-dev"

func main() {
	migrateOnly := flag.Bool("migrate", false, "应用数据库迁移后退出，不启动服务")
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
	govStore := mysql.NewGovernanceStore(db)
	govService := governance.NewService(govStore, clock, ids)

	// embedder 为 nil 是**降级形态而非异常形态**：不入索引队列，
	// 检索退化成中文全文匹配，系统功能保持完整（01-architecture.md §6.1）。
	embedder := newEmbedder(cfg.Embedding)
	knowStore := mysql.NewKnowledgeStore(db)
	knowService := knowledge.NewService(knowStore, clock, ids, embedder)

	mcpHost := mcphost.NewHost(govService, knowService, logger, mcphost.HostOptions{
		Version:        version,
		SemanticSearch: cfg.Embedding.Enabled(),
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
