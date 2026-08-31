//go:build integration

package main

import (
	"context"
	"database/sql"
	"io"
	"log/slog"
	"os"
	"path/filepath"
	"sync/atomic"
	"testing"
	"time"

	miniredis "github.com/alicebob/miniredis/v2"

	"claracore-server/internal/adapters/mysql"
	redisadapter "claracore-server/internal/adapters/redis"
	"claracore-server/internal/config"
	"claracore-server/internal/domain/knowledge"
	"claracore-server/internal/platform"
	"claracore-server/internal/ports"
)

// 这组测试走真实 MySQL Store 和两个独立 Redis client，验证的不是
// locker 单元行为，而是两个完整 indexer worker 对同一数据库队列的装配结果。
// Redis 服务用 miniredis 隔离启动，避免测试依赖开发机常驻 Redis。

type blockingEmbedder struct {
	model   string
	calls   atomic.Int64
	started chan struct{}
	release chan struct{}
}

func newBlockingEmbedder(model string, expectedCalls int) *blockingEmbedder {
	return &blockingEmbedder{
		model: model, started: make(chan struct{}, expectedCalls), release: make(chan struct{}),
	}
}

func (e *blockingEmbedder) Model() string { return e.model }

func (e *blockingEmbedder) Embed(ctx context.Context, texts []string) ([][]float32, error) {
	e.calls.Add(1)
	e.started <- struct{}{}
	select {
	case <-e.release:
	case <-ctx.Done():
		return nil, ctx.Err()
	}
	vectors := make([][]float32, len(texts))
	for i := range vectors {
		vectors[i] = []float32{0.25, 0.5, 0.75}
	}
	return vectors, nil
}

type observingLocker struct {
	inner     ports.Locker
	attempted chan<- struct{}
}

func (l observingLocker) TryLock(ctx context.Context, key string, ttl time.Duration) (ports.Lease, bool, error) {
	l.attempted <- struct{}{}
	return l.inner.TryLock(ctx, key, ttl)
}

func TestTwoIndexersShareRedisLeaseAcrossRealMySQLQueue(t *testing.T) {
	db := openIndexerTestDB(t)
	embedder := newBlockingEmbedder("integration-shared-lease", 2)
	tenant, svcA, svcB := seedIndexerMemory(t, db, embedder)

	server := miniredis.RunT(t)
	cacheA := redisadapter.NewCache(server.Addr(), "", "ccs:indexer-it:", 100*time.Millisecond)
	cacheB := redisadapter.NewCache(server.Addr(), "", "ccs:indexer-it:", 100*time.Millisecond)
	t.Cleanup(func() { _ = cacheA.Close() })
	t.Cleanup(func() { _ = cacheB.Close() })

	attempted := make(chan struct{}, 2)
	cfg := indexerTestConfig(embedder.Model())
	ctx, cancel := context.WithCancel(context.Background())
	doneA, doneB := make(chan struct{}), make(chan struct{})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	go runIndexer(ctx, svcA, cfg, observingLocker{inner: cacheA, attempted: attempted}, logger, doneA)
	go runIndexer(ctx, svcB, cfg, observingLocker{inner: cacheB, attempted: attempted}, logger, doneB)

	waitSignal(t, embedder.started, "任一 worker 进入 embedding")
	waitSignal(t, attempted, "第一个 worker 尝试租约")
	waitSignal(t, attempted, "第二个 worker 尝试租约")
	close(embedder.release)
	waitEmbeddingReady(t, svcA, tenant)
	cancel()
	waitDone(t, doneA, "worker A")
	waitDone(t, doneB, "worker B")

	if got := embedder.calls.Load(); got != 1 {
		t.Fatalf("共享租约应让同一批只调用一次 embedding，实际 %d 次", got)
	}
}

func TestTwoIndexersFailOpenWhenRedisIsDownAndWriteBackIdempotently(t *testing.T) {
	db := openIndexerTestDB(t)
	embedder := newBlockingEmbedder("integration-redis-outage", 2)
	tenant, svcA, svcB := seedIndexerMemory(t, db, embedder)

	server := miniredis.RunT(t)
	server.SetError("LOADING Redis is unavailable for integration fault injection")
	cacheA := redisadapter.NewCache(server.Addr(), "", "ccs:indexer-it:", 50*time.Millisecond)
	cacheB := redisadapter.NewCache(server.Addr(), "", "ccs:indexer-it:", 50*time.Millisecond)
	t.Cleanup(func() { _ = cacheA.Close() })
	t.Cleanup(func() { _ = cacheB.Close() })

	cfg := indexerTestConfig(embedder.Model())
	ctx, cancel := context.WithCancel(context.Background())
	doneA, doneB := make(chan struct{}), make(chan struct{})
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	go runIndexer(ctx, svcA, cfg, cacheA, logger, doneA)
	go runIndexer(ctx, svcB, cfg, cacheB, logger, doneB)

	// 两个 worker 都必须在 Redis 已停时继续领到同一条 pending；先把它们
	// 一起卡在 embedding，再放行，才能真实覆盖并发幂等写回而非顺序空跑。
	waitSignal(t, embedder.started, "Redis 故障后的 worker A embedding")
	waitSignal(t, embedder.started, "Redis 故障后的 worker B embedding")
	close(embedder.release)
	waitEmbeddingReady(t, svcA, tenant)
	cancel()
	waitDone(t, doneA, "worker A")
	waitDone(t, doneB, "worker B")

	if got := embedder.calls.Load(); got != 2 {
		t.Fatalf("Redis 故障时两个实例应 fail-open 并允许重复幂等计算，实际 %d 次", got)
	}
	health, err := svcA.EmbeddingHealth(context.Background(), tenant, 10)
	if err != nil {
		t.Fatal(err)
	}
	if health.Stats.Ready != 1 || health.Stats.Pending != 0 || health.Stats.Failed != 0 {
		t.Fatalf("并发写回后队列应收敛为一条 ready: %+v", health.Stats)
	}
}

func openIndexerTestDB(t *testing.T) *sql.DB {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Join(wd, "..", ".."))
	cfg, err := config.Load()
	if err != nil {
		t.Skipf("配置不可用，跳过集成测试: %v", err)
	}
	db, err := mysql.Open(context.Background(), cfg.MySQL.DSN(), mysql.PoolConfig{
		MaxOpenConns: 4, MaxIdleConns: 2, ConnMaxLifetime: time.Minute,
	})
	if err != nil {
		t.Skipf("数据库不可达，跳过集成测试: %v", err)
	}
	t.Cleanup(func() { _ = db.Close() })
	return db
}

func seedIndexerMemory(t *testing.T, db *sql.DB, embedder ports.Embedder) (platform.ID, *knowledge.Service, *knowledge.Service) {
	t.Helper()
	ctx := context.Background()
	clock := platform.SystemClock{}
	ids := platform.NewIDGenerator(clock)
	tenant, space, user := ids.New(), ids.New(), ids.New()
	storeA, storeB := mysql.NewKnowledgeStore(db), mysql.NewKnowledgeStore(db)
	svcA := knowledge.NewService(storeA, clock, ids, embedder)
	svcB := knowledge.NewService(storeB, clock, ids, embedder)
	if _, err := svcA.CreateMemory(ctx, knowledge.CreateMemoryInput{
		TenantID: tenant, SpaceID: space, AuthorUserID: user,
		Title: "双实例索引租约", Body: "同一批待索引内容只能由一个健康实例调用 embedding。",
	}); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		for _, table := range []string{"embeddings", "memories"} {
			if _, err := db.ExecContext(context.Background(), `DELETE FROM `+table+` WHERE tenant_id = ?`, string(tenant)); err != nil {
				t.Errorf("清理 %s 失败，测试租户 %s 留在数据库: %v", table, tenant, err)
			}
		}
	})
	return tenant, svcA, svcB
}

func indexerTestConfig(model string) config.Embedding {
	return config.Embedding{
		Provider: config.ProviderOllama, Model: model,
		Timeout: 500 * time.Millisecond, IndexInterval: time.Hour, IndexBatch: 8,
	}
}

func waitEmbeddingReady(t *testing.T, svc *knowledge.Service, tenant platform.ID) {
	t.Helper()
	deadline := time.NewTimer(3 * time.Second)
	defer deadline.Stop()
	ticker := time.NewTicker(10 * time.Millisecond)
	defer ticker.Stop()
	for {
		health, err := svc.EmbeddingHealth(context.Background(), tenant, 10)
		if err != nil {
			t.Fatal(err)
		}
		if health.Stats.Ready == 1 {
			return
		}
		select {
		case <-deadline.C:
			t.Fatalf("等待 embedding ready 超时，最后状态 %+v", health.Stats)
		case <-ticker.C:
		}
	}
}

func waitSignal(t *testing.T, ch <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("等待%s超时", label)
	}
}

func waitDone(t *testing.T, ch <-chan struct{}, label string) {
	t.Helper()
	select {
	case <-ch:
	case <-time.After(3 * time.Second):
		t.Fatalf("等待%s退出超时", label)
	}
}

var _ ports.Embedder = (*blockingEmbedder)(nil)
var _ ports.Locker = observingLocker{}
