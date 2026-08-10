//go:build integration

// 端到端语义召回：真库 + 真 embedding 服务 + 完整装配。
//
// 这是唯一一个能回答「这套检索到底有没有用」的测试。前面所有的领域单测
// 用的都是 fakeEmbedder——那个假向量能验证排序规则、分组、降级路径，
// 唯独验证不了最核心的那件事：**一段情境和一条作者写下的召回条件，
// 在真实模型的向量空间里是不是真的挨得近**。
//
// 构造是刻意的对抗性：正确的那张卡与情境**没有字面重叠**，
// 而干扰卡**有**字面重叠。于是纯全文匹配会把干扰卡排在前面，
// 只有语义那一段能把顺序纠正过来。这样它证明的就不是"跑通了"，
// 而是"这一段确实在起作用"。
package test

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	"claracore-server/internal/adapters/embedding"
	"claracore-server/internal/adapters/mysql"
	"claracore-server/internal/config"
	"claracore-server/internal/domain/knowledge"
	"claracore-server/internal/platform"
	"claracore-server/internal/ports"
)

func newRecallEnv(t *testing.T) (*knowledge.Service, *knowledge.Service, func()) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Join(wd, ".."))

	cfg, err := config.Load()
	if err != nil {
		t.Skipf("配置不可用，跳过: %v", err)
	}
	if !cfg.Embedding.Enabled() {
		t.Skip("未配置 embedding 服务，跳过端到端语义召回")
	}

	db, err := mysql.Open(context.Background(), cfg.MySQL.DSN(), mysql.PoolConfig{
		MaxOpenConns: 4, MaxIdleConns: 2, ConnMaxLifetime: time.Minute,
	})
	if err != nil {
		t.Skipf("数据库不可达，跳过: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	var embedder ports.Embedder
	switch cfg.Embedding.Provider {
	case config.ProviderOllama:
		embedder = embedding.NewOllama(cfg.Embedding.BaseURL, cfg.Embedding.Model, cfg.Embedding.APIKey, cfg.Embedding.Timeout)
	default:
		embedder = embedding.NewOpenAICompat(cfg.Embedding.BaseURL, cfg.Embedding.Model, cfg.Embedding.APIKey, cfg.Embedding.Timeout)
	}
	// 先探一次：服务没起来时给出明确的跳过理由，而不是让第一个断言莫名其妙地红
	if _, err := embedder.Embed(context.Background(), []string{"探活"}); err != nil {
		t.Skipf("embedding 服务不可用，跳过: %v", err)
	}

	clock := platform.SystemClock{}
	ids := platform.NewIDGenerator(clock)
	store := mysql.NewKnowledgeStore(db)

	semantic := knowledge.NewService(store, clock, ids, embedder)
	// 同一批数据上再装一个没有 Embedder 的服务：它跑的就是降级路径，
	// 用来做对照——没有对照就说不清语义那一段到底贡献了什么。
	fullTextOnly := knowledge.NewService(store, clock, ids, nil)

	cleanup := func() {}
	return semantic, fullTextOnly, cleanup
}

func TestSemanticRecallBeatsFullTextEndToEnd(t *testing.T) {
	semantic, fullTextOnly, _ := newRecallEnv(t)
	ctx := context.Background()

	ids := platform.NewIDGenerator(platform.SystemClock{})
	tenant, space, user := ids.New(), ids.New(), ids.New()

	// 正确的那张卡：召回条件与下面的情境**没有字面重叠**
	right, err := semantic.CreateCard(ctx, knowledge.CreateCardInput{
		TenantID: tenant, SpaceID: space, AuthorUserID: user,
		Title: "备份成功不等于数据可恢复", CardType: "Codex 协作安全",
		Distilled:     "备份报告只给归档路径与校验和，不给目标文件清单，等于没有备份。",
		Understanding: "校验和证明的是这个包没坏，不是这个包里装对了东西。",
		RecallTriggers: []string{
			"准备执行不可逆的数据清除动作之前",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	// 干扰卡：召回条件与情境**有**明显字面重叠（"目录"），但语义无关
	decoy, err := semantic.CreateCard(ctx, knowledge.CreateCardInput{
		TenantID: tenant, SpaceID: space, AuthorUserID: user,
		Title: "目录命名规范", CardType: "工程约定",
		Distilled:     "目录用小写连字符，不要驼峰。",
		Understanding: "大小写敏感性在 macOS 与 Linux 上不一致，踩过一次。",
		RecallTriggers: []string{
			"整理目录结构与目录命名时",
		},
	})
	if err != nil {
		t.Fatal(err)
	}

	t.Cleanup(func() {
		// 这些用例真提交（全文索引在提交时才更新），自己清理
		env := mysqlCleanup(t)
		env(tenant)
	})

	// 后台 worker 平时干的活，这里同步跑一遍
	for {
		report, err := semantic.IndexPending(ctx, 32)
		if err != nil {
			t.Fatalf("索引失败: %v", err)
		}
		if report.Picked == 0 {
			break
		}
		t.Logf("索引一批: picked=%d indexed=%d failed=%d", report.Picked, report.Indexed, report.Failed)
	}

	situation := "用户说这个目录没用了让我整个删掉，但里面可能有不可再生的历史"
	spaces := []knowledge.SearchSpace{{ID: space, Rank: 0}}

	// --- 对照组：只有全文 ---
	plain, err := fullTextOnly.RecallCards(ctx, knowledge.RecallCardsInput{
		TenantID: tenant, Spaces: spaces, Situation: situation, Limit: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if plain.SemanticAvailable {
		t.Fatal("对照组不该有语义")
	}
	t.Logf("全文路径首条: %s", firstTitle(plain))

	// --- 实验组：两段式 ---
	got, err := semantic.RecallCards(ctx, knowledge.RecallCardsInput{
		TenantID: tenant, Spaces: spaces, Situation: situation, Limit: 5,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !got.SemanticAvailable {
		t.Fatal("实验组应当用上了向量")
	}
	if len(got.Hits) == 0 {
		t.Fatal("一张卡都没召回")
	}
	for _, h := range got.Hits {
		t.Logf("语义路径: %-20s score=%.4f semantic=%v trigger=%q", h.Card.Title, h.Score, h.Semantic, h.Trigger)
	}

	top := got.Hits[0]
	if top.Card.ID != right.ID {
		t.Fatalf("语义召回没能把正确的卡排到第一位\n  期望: %s（%s）\n  实际: %s\n"+
			"  这条卡的召回条件与情境没有字面重叠，只有向量能把它找出来——"+
			"排不上来说明第二段精算没有真的起作用", right.Title, right.ID, top.Card.Title)
	}
	if !top.Semantic {
		t.Fatal("首条应标记为语义命中")
	}
	if top.Trigger == "" {
		t.Fatal("召回必须带上命中的那条召回条件——没有它，这次召回对 agent 来说就是凭空出现的建议")
	}
	// 干扰卡有字面重叠，可以出现在结果里，但不该压过正确的那张
	for i, h := range got.Hits {
		if h.Card.ID == decoy.ID && i == 0 {
			t.Fatal("字面重叠的干扰卡排到了第一位")
		}
	}
}

func firstTitle(r knowledge.CardSearchResult) string {
	if len(r.Hits) == 0 {
		return "（无命中）"
	}
	return r.Hits[0].Card.Title
}

// mysqlCleanup 返回一个按租户清库的函数。
func mysqlCleanup(t *testing.T) func(platform.ID) {
	t.Helper()
	cfg, err := config.Load()
	if err != nil {
		t.Fatal(err)
	}
	db, err := mysql.Open(context.Background(), cfg.MySQL.DSN(), mysql.PoolConfig{
		MaxOpenConns: 2, MaxIdleConns: 1, ConnMaxLifetime: time.Minute,
	})
	if err != nil {
		t.Fatal(err)
	}
	return func(tenant platform.ID) {
		defer db.Close()
		ctx := context.Background()
		var errs []error
		for _, table := range []string{"embeddings", "card_recall_triggers", "labels", "links", "cards", "memories"} {
			if _, err := db.ExecContext(ctx, `DELETE FROM `+table+` WHERE tenant_id = ?`, string(tenant)); err != nil {
				errs = append(errs, err)
			}
		}
		if err := errors.Join(errs...); err != nil {
			t.Errorf("清理失败，库里留下了测试数据: %v", err)
		}
	}
}
