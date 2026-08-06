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

	"claracore-server/internal/adapters/mysql"
	"claracore-server/internal/config"
	"claracore-server/internal/platform"
	"claracore-server/migrations"
)

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

	mux := http.NewServeMux()
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
	logger.Info("已退出")
}
