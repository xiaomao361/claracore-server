// claracore-server：团队 agent 的记忆与知识库 MCP 服务，部署在 bosun 网关之后。
package main

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"log/slog"
	"net"
	"net/http"
	"os"
	"os/signal"
	"syscall"
	"time"

	"claracore-server/internal/embedding"
	"claracore-server/internal/mcpserver"
	"claracore-server/internal/memo"
	"claracore-server/internal/platform"
)

var version = "dev"

func main() {
	if err := run(); err != nil {
		fmt.Fprintln(os.Stderr, "claracore-server:", err)
		os.Exit(1)
	}
}

func run() error {
	addr := flag.String("addr", env("CLARACORE_ADDR", "127.0.0.1:9830"), "监听地址")
	dbPath := flag.String("db", env("CLARACORE_DB", "claracore.db"), "SQLite 数据库文件")
	allowOpen := flag.Bool("allow-unauthenticated-network", false, "未设 CLARACORE_TOKEN 时仍允许监听非回环地址（仅限防火墙已限制来源）")
	flag.Parse()

	log := slog.New(slog.NewTextHandler(os.Stderr, nil))
	token := os.Getenv("CLARACORE_TOKEN")
	if token == "" && !isLoopback(*addr) && !*allowOpen {
		return fmt.Errorf("监听 %s 需要设置 CLARACORE_TOKEN（网关的上游凭据）", *addr)
	}
	if token == "" {
		log.Warn("未设置 CLARACORE_TOKEN：X-Bosun-User 可被本机任意进程伪造，仅用于本地调试")
	}

	embedder, err := newEmbedder()
	if err != nil {
		return err
	}
	db, err := memo.Open(*dbPath)
	if err != nil {
		return fmt.Errorf("打开数据库 %s: %w", *dbPath, err)
	}
	defer db.Close()

	svc := memo.NewService(db, embedder, platform.SystemClock{}, log)
	srv := mcpserver.New(svc, mcpserver.Config{Version: version, Token: token, DevUser: os.Getenv("CLARACORE_DEV_USER")}, log)

	ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
	defer stop()
	go svc.RunIndexer(ctx, time.Minute)

	httpSrv := &http.Server{Addr: *addr, Handler: srv.Handler(), ReadHeaderTimeout: 10 * time.Second}
	errc := make(chan error, 1)
	go func() { errc <- httpSrv.ListenAndServe() }()
	log.Info("listening", "addr", *addr, "db", *dbPath, "semantic", embedder != nil, "version", version)

	select {
	case err := <-errc:
		return err
	case <-ctx.Done():
	}
	shutdown, cancel := context.WithTimeout(context.Background(), 10*time.Second)
	defer cancel()
	if err := httpSrv.Shutdown(shutdown); err != nil && !errors.Is(err, http.ErrServerClosed) {
		return err
	}
	return nil
}

// newEmbedder 按环境变量选向量服务；不配置时返回 nil，检索只走文本。
func newEmbedder() (embedding.Embedder, error) {
	provider := os.Getenv("CLARACORE_EMBEDDING_PROVIDER")
	url, model, key := os.Getenv("CLARACORE_EMBEDDING_URL"), os.Getenv("CLARACORE_EMBEDDING_MODEL"), os.Getenv("CLARACORE_EMBEDDING_API_KEY")
	if provider == "" {
		return nil, nil
	}
	if url == "" || model == "" {
		return nil, fmt.Errorf("CLARACORE_EMBEDDING_PROVIDER=%s 需要同时设置 CLARACORE_EMBEDDING_URL 与 CLARACORE_EMBEDDING_MODEL", provider)
	}
	switch provider {
	case "ollama":
		return embedding.NewOllama(url, model, key, 0), nil
	case "openai":
		return embedding.NewOpenAICompat(url, model, key, 0), nil
	}
	return nil, fmt.Errorf("CLARACORE_EMBEDDING_PROVIDER 只能是 ollama 或 openai，收到 %q", provider)
}

func isLoopback(addr string) bool {
	host, _, err := net.SplitHostPort(addr)
	if err != nil {
		return false
	}
	if host == "localhost" {
		return true
	}
	ip := net.ParseIP(host)
	return ip != nil && ip.IsLoopback()
}

func env(key, fallback string) string {
	if v := os.Getenv(key); v != "" {
		return v
	}
	return fallback
}
