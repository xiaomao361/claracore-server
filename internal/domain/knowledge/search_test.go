package knowledge

import (
	"context"
	"errors"
	"testing"
	"time"

	"claracore-server/internal/platform"
)

// newSearchService 构造一个带 Embedder 的服务。
//
// 关键词表决定了向量空间：fakeEmbedder 给每个词一个维度，向量就是词频，
// 于是"共享关键词多的两段文本余弦更高"成立——那正是排序唯一依赖的性质。
func newSearchService(t *testing.T, words ...string) (*Service, *fakeStore, *fakeEmbedder) {
	t.Helper()
	clock := &platform.OffsetClock{Base: time.Date(2026, 8, 10, 9, 0, 0, 0, time.UTC), Step: time.Minute}
	store := newFakeStore()
	emb := newFakeEmbedder(words...)
	return NewService(store, clock, platform.NewIDGenerator(clock), emb), store, emb
}

// readable 把裸空间 ID 变成带权重的可读空间集合。
// Rank 由治理域决定（team > project > personal），knowledge 只看这个整数。
func readable(spaces ...platform.ID) []SearchSpace {
	out := make([]SearchSpace, 0, len(spaces))
	for _, s := range spaces {
		rank := 0
		if s == spaceTeam {
			rank = 2
		}
		out = append(out, SearchSpace{ID: s, Rank: rank})
	}
	return out
}

// indexAll 把队列里所有待算的向量算完，模拟后台 worker 追上进度。
func indexAll(t *testing.T, svc *Service) {
	t.Helper()
	for {
		r, err := svc.IndexPending(context.Background(), 0)
		if err != nil {
			t.Fatal(err)
		}
		if r.Picked == 0 {
			return
		}
	}
}

// ---------------------------------------------------------------------------
// 权限过滤：这一组是安全属性，坏了不会有人察觉
// ---------------------------------------------------------------------------

func TestSearchMemoriesExcludesUnreadableSpaces(t *testing.T) {
	svc, _ := newTestService(t)
	visible := seedMemory(t, svc, spacePriv)
	hidden := seedMemory(t, svc, spaceHid) // 调用者对这个空间没有任何权限

	res, err := svc.SearchMemories(context.Background(), SearchMemoriesInput{
		TenantID: tenantA,
		Spaces:   readable(mySpaces()...),
		Query:    "连接池",
	})
	if err != nil {
		t.Fatal(err)
	}
	var got []platform.ID
	for _, h := range res.Hits {
		got = append(got, h.Memory.ID)
	}
	if len(got) != 1 || got[0] != visible.ID {
		t.Fatalf("只应返回可读空间里的那条\n  期望: [%s]\n  实际: %v", visible.ID, got)
	}
	for _, id := range got {
		if id == hidden.ID {
			t.Fatal("返回了调用者无权访问的空间里的内容——权限过滤没有下推到粗筛")
		}
	}
}

func TestSearchCrossTenantIsolated(t *testing.T) {
	svc, store := newTestService(t)
	mine := seedMemory(t, svc, spacePriv)

	// 另一个租户在同一个空间 ID 下的内容。正常情况下不会有，
	// 但空间 ID 是调用方传进来的，泄露一次就是跨租户越权。
	foreign := mine
	foreign.ID = platform.ID("01FOREIGNMEMORYAAAAAAAAAAA")
	foreign.TenantID = tenantB
	if err := store.CreateMemory(context.Background(), foreign); err != nil {
		t.Fatal(err)
	}

	res, err := svc.SearchMemories(context.Background(), SearchMemoriesInput{
		TenantID: tenantA,
		Spaces:   readable(mySpaces()...),
		Query:    "连接池",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range res.Hits {
		if h.Memory.TenantID != tenantA {
			t.Fatalf("返回了别的租户的内容: %s", h.Memory.ID)
		}
	}
	if len(res.Hits) != 1 {
		t.Fatalf("期望 1 条，实际 %d 条", len(res.Hits))
	}
}

func TestSearchRequiresSpaces(t *testing.T) {
	svc, _ := newTestService(t)

	_, err := svc.SearchMemories(context.Background(), SearchMemoriesInput{TenantID: tenantA, Query: "x"})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("空空间集合应报 ErrInvalidArgument，实际: %v", err)
	}
	// 「你没有任何可访问空间」和「搜到了但是没有」的排查方向完全不同，
	// 静默返回空结果会把前者伪装成后者。
	_, err = svc.SearchCards(context.Background(), SearchCardsInput{TenantID: tenantA, Query: "x"})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("card_search 同样应报错，实际: %v", err)
	}
	_, err = svc.RecallCards(context.Background(), RecallCardsInput{TenantID: tenantA, Situation: "x"})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("card_recall 同样应报错，实际: %v", err)
	}
}

func TestSearchExcludesNonVisibleStatus(t *testing.T) {
	svc, _ := newTestService(t)
	m := seedMemory(t, svc, spacePriv)
	if err := svc.DeleteMemory(context.Background(), tenantA, m.ID, mySpaces()); err != nil {
		t.Fatal(err)
	}

	res, err := svc.SearchMemories(context.Background(), SearchMemoriesInput{
		TenantID: tenantA,
		Spaces:   readable(mySpaces()...),
		Query:    "连接池",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 0 {
		t.Fatalf("软删除的记忆不该进默认检索，实际返回 %d 条", len(res.Hits))
	}
}

// ---------------------------------------------------------------------------
// 降级：没有 Embedder 时检索必须仍然可用
// ---------------------------------------------------------------------------

func TestSearchWithoutEmbedderFallsBackToFullText(t *testing.T) {
	svc, _ := newTestService(t) // 无 Embedder
	seedMemory(t, svc, spacePriv)

	res, err := svc.SearchMemories(context.Background(), SearchMemoriesInput{
		TenantID: tenantA,
		Spaces:   readable(mySpaces()...),
		Query:    "连接池",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 {
		t.Fatalf("降级路径必须仍能返回全文命中，实际 %d 条", len(res.Hits))
	}
	if res.SemanticAvailable {
		t.Fatal("没有 Embedder 时 SemanticAvailable 必须是 false")
	}
	if res.Hits[0].Semantic {
		t.Fatal("全文命中不得标成语义命中")
	}
}

func TestSearchDegradesWhenEmbedderFails(t *testing.T) {
	svc, _, emb := newSearchService(t, "连接池", "删除")
	seedMemory(t, svc, spacePriv)
	indexAll(t, svc)

	// 索引完之后 embedding 服务挂掉：检索该变差，不该停摆。
	emb.fail = errors.New("connection refused")

	res, err := svc.SearchMemories(context.Background(), SearchMemoriesInput{
		TenantID: tenantA,
		Spaces:   readable(mySpaces()...),
		Query:    "连接池",
	})
	if err != nil {
		t.Fatalf("embedding 服务失败不该让检索报错: %v", err)
	}
	if len(res.Hits) != 1 {
		t.Fatalf("全文那一段仍应返回结果，实际 %d 条", len(res.Hits))
	}
	if res.SemanticAvailable {
		t.Fatal("算不出查询向量时 SemanticAvailable 必须是 false——否则调用方会以为语义那一半跑过了")
	}
}

// ---------------------------------------------------------------------------
// 两段式：语义命中与全文命中分组，不通约
// ---------------------------------------------------------------------------

func TestSemanticHitsRankAboveTextOnlyHits(t *testing.T) {
	svc, _, _ := newSearchService(t, "连接池", "备份")

	indexed := seedMemory(t, svc, spacePriv) // 标题正文都含「连接池」
	indexAll(t, svc)

	// 这条在索引之后写入，还没轮到 worker——它只有全文命中，没有向量。
	// 这正是异步索引的常态，检索必须能同时容纳两种命中。
	unindexed, err := svc.CreateMemory(context.Background(), CreateMemoryInput{
		TenantID: tenantA, SpaceID: spacePriv, AuthorUserID: userA,
		Title: "连接池连接池连接池", Body: "字面命中更强，但还没有向量。",
	})
	if err != nil {
		t.Fatal(err)
	}

	res, err := svc.SearchMemories(context.Background(), SearchMemoriesInput{
		TenantID: tenantA,
		Spaces:   readable(mySpaces()...),
		Query:    "连接池",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 2 {
		t.Fatalf("期望 2 条命中，实际 %d 条", len(res.Hits))
	}
	if !res.SemanticAvailable {
		t.Fatal("有 Embedder 且有向量时 SemanticAvailable 应为 true")
	}
	if res.Hits[0].Memory.ID != indexed.ID || !res.Hits[0].Semantic {
		t.Fatalf("语义命中应排在第一位并标记 Semantic\n  实际首条: %s (semantic=%v)",
			res.Hits[0].Memory.ID, res.Hits[0].Semantic)
	}
	if res.Hits[1].Memory.ID != unindexed.ID || res.Hits[1].Semantic {
		t.Fatalf("未索引的全文命中应排在后面且不标 Semantic\n  实际次条: %s (semantic=%v)",
			res.Hits[1].Memory.ID, res.Hits[1].Semantic)
	}
}

// TestSpaceRankOnlyBreaksTies 锁住「空间只做 tie-break」这条决定。
//
// 两条内容在向量空间里完全一样（同样的关键词构成），此时才轮到空间层级决胜；
// 如果哪天有人把空间权重加进主排序，这个测试仍然会绿——所以下面还有一条
// 反向的用例，验证分数明显不同时空间层级不起作用。
func TestSpaceRankOnlyBreaksTies(t *testing.T) {
	svc, _, _ := newSearchService(t, "连接池")

	personal := seedMemory(t, svc, spacePriv)
	team := seedMemory(t, svc, spaceTeam)
	indexAll(t, svc)

	res, err := svc.SearchMemories(context.Background(), SearchMemoriesInput{
		TenantID: tenantA,
		Spaces:   readable(spacePriv, spaceTeam), // spaceTeam 的 Rank 更高
		Query:    "连接池",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 2 {
		t.Fatalf("期望 2 条，实际 %d 条", len(res.Hits))
	}
	if res.Hits[0].Memory.ID != team.ID {
		t.Fatalf("分数相同时应由空间层级决胜，团队空间在前\n  实际首条: %s（团队那条是 %s，个人那条是 %s）",
			res.Hits[0].Memory.ID, team.ID, personal.ID)
	}
}

func TestSpaceRankLosesToRelevance(t *testing.T) {
	svc, _, _ := newSearchService(t, "连接池", "备份")

	// 个人空间里是精准命中，团队空间里只是勉强沾边。
	precise, err := svc.CreateMemory(context.Background(), CreateMemoryInput{
		TenantID: tenantA, SpaceID: spacePriv, AuthorUserID: userA,
		Title: "连接池", Body: "连接池连接池",
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateMemory(context.Background(), CreateMemoryInput{
		TenantID: tenantA, SpaceID: spaceTeam, AuthorUserID: userA,
		Title: "备份策略", Body: "备份备份备份，顺带提一句连接池。",
	}); err != nil {
		t.Fatal(err)
	}
	indexAll(t, svc)

	res, err := svc.SearchMemories(context.Background(), SearchMemoriesInput{
		TenantID: tenantA,
		Spaces:   readable(spacePriv, spaceTeam),
		Query:    "连接池",
	})
	if err != nil {
		t.Fatal(err)
	}
	if res.Hits[0].Memory.ID != precise.ID {
		t.Fatal("相关性优先于空间层级：个人空间里的精准命中不该被团队空间的勉强沾边压下去")
	}
}

func TestSearchLimitIsClamped(t *testing.T) {
	svc, _ := newTestService(t)
	for i := 0; i < 3; i++ {
		seedMemory(t, svc, spacePriv)
	}

	res, err := svc.SearchMemories(context.Background(), SearchMemoriesInput{
		TenantID: tenantA, Spaces: readable(mySpaces()...), Query: "连接池", Limit: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 2 {
		t.Fatalf("limit=2 时应返回 2 条，实际 %d 条", len(res.Hits))
	}

	res, err = svc.SearchMemories(context.Background(), SearchMemoriesInput{
		TenantID: tenantA, Spaces: readable(mySpaces()...), Query: "连接池", Limit: 9999,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) > maxSearchLimit {
		t.Fatalf("limit 应被截到 %d，实际返回 %d 条", maxSearchLimit, len(res.Hits))
	}
}

func TestSearchLabelsAreIntersection(t *testing.T) {
	svc, _ := newTestService(t)
	both := seedMemory(t, svc, spacePriv, "mysql", "ops")
	seedMemory(t, svc, spacePriv, "mysql")

	res, err := svc.SearchMemories(context.Background(), SearchMemoriesInput{
		TenantID: tenantA, Spaces: readable(mySpaces()...), Query: "连接池",
		Labels: []string{"mysql", "ops"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 || res.Hits[0].Memory.ID != both.ID {
		t.Fatalf("多个标签是交集不是并集：只该返回同时有 mysql 与 ops 的那条，实际 %d 条", len(res.Hits))
	}
}

// ---------------------------------------------------------------------------
// 「另有 N 条卡片命中」
// ---------------------------------------------------------------------------

func TestSearchMemoriesHintsCardMatches(t *testing.T) {
	svc, _ := newTestService(t)
	seedCard(t, svc, spacePriv, withCard("备份成功不等于数据可恢复"))

	res, err := svc.SearchMemories(context.Background(), SearchMemoriesInput{
		TenantID: tenantA, Spaces: readable(mySpaces()...), Query: "备份",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 0 {
		t.Fatalf("这次没有 memory 命中，实际 %d 条", len(res.Hits))
	}
	// agent 搜一次 memory 没找到就以为没有，而 Card 才是这个产品的核心载体。
	if res.CardMatches != 1 {
		t.Fatalf("memory 命中为空时必须给出卡片命中计数，期望 1，实际 %d", res.CardMatches)
	}
}

func TestCardMatchCountSkippedWhenMemoryHitsAreEnough(t *testing.T) {
	svc, store := newTestService(t)
	for i := 0; i < cardHintThreshold; i++ {
		seedMemory(t, svc, spacePriv)
	}
	// 让 CountCardMatches 一被调用就报错：命中够多时它根本不该被调用。
	store.failOn = "CountCardMatches"

	res, err := svc.SearchMemories(context.Background(), SearchMemoriesInput{
		TenantID: tenantA, Spaces: readable(mySpaces()...), Query: "连接池",
	})
	if err != nil {
		t.Fatalf("命中够多时不该再去数卡片: %v", err)
	}
	if res.CardMatches != -1 {
		t.Fatalf("没数就该是 -1（而不是 0，那会被读成\"确实没有卡片\"），实际 %d", res.CardMatches)
	}
}

// ---------------------------------------------------------------------------
// card_recall —— 核心能力
// ---------------------------------------------------------------------------

// withCard 复用 card_test.go 里的 seedCard，只覆盖标题与召回条件。
func withCard(title string, triggers ...string) func(*CreateCardInput) {
	return func(in *CreateCardInput) {
		in.Title = title
		if len(triggers) > 0 {
			in.RecallTriggers = triggers
		}
	}
}

func TestRecallReturnsWholeCardWithReason(t *testing.T) {
	svc, _, _ := newSearchService(t, "删除", "备份", "连接池")
	card := seedCard(t, svc, spacePriv, withCard("备份成功不等于数据可恢复",
		"准备删除聊天记录、Agent 工作区或本地 Git 仓库时",
		"为释放磁盘空间清理隐藏目录时"))
	indexAll(t, svc)

	res, err := svc.RecallCards(context.Background(), RecallCardsInput{
		TenantID:  tenantA,
		Spaces:    readable(mySpaces()...),
		Situation: "用户让我清理 ~/.qclaw 目录释放磁盘空间，我准备执行删除",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 {
		t.Fatalf("期望召回 1 张卡，实际 %d 张", len(res.Hits))
	}
	if res.Hits[0].Card.ID != card.ID {
		t.Fatalf("召回了错误的卡片: %s", res.Hits[0].Card.ID)
	}
	// 没有召回理由的话，一次召回对 agent 来说就是凭空出现的建议。
	if res.Hits[0].Trigger == "" {
		t.Fatal("召回必须带上命中的那条触发条件")
	}
	// 一张卡的多条触发条件都命中时只应出现一次，否则它会占掉别的卡的名额。
	if len(res.Hits) > 1 {
		t.Fatal("同一张卡不得因为多条触发条件命中而重复出现")
	}
}

func TestRecallExcludesDraftCards(t *testing.T) {
	svc, _, _ := newSearchService(t, "删除")
	// 摄取产生的草稿：不进默认检索，需人过一遍（02 §3.2）。
	if _, err := svc.CreateCard(context.Background(), CreateCardInput{
		TenantID: tenantA, SpaceID: spacePriv, AuthorUserID: userA,
		Title: "摄取来的长文", CardType: "文档", Status: CardDraft,
		Distilled:      "一段没人审过的摘要。",
		RecallTriggers: []string{"准备删除数据时"},
	}); err != nil {
		t.Fatal(err)
	}
	indexAll(t, svc)

	res, err := svc.RecallCards(context.Background(), RecallCardsInput{
		TenantID: tenantA, Spaces: readable(mySpaces()...), Situation: "我准备删除一批数据",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 0 {
		t.Fatalf("draft 卡片不该被召回——未审草稿混进 agent 上下文正是 status 三态要防的事，实际召回 %d 张", len(res.Hits))
	}
}

func TestRecallExcludesUnreadableSpaces(t *testing.T) {
	svc, _, _ := newSearchService(t, "删除")
	seedCard(t, svc, spaceHid, withCard("看不见的卡", "准备删除数据时"))
	indexAll(t, svc)

	res, err := svc.RecallCards(context.Background(), RecallCardsInput{
		TenantID: tenantA, Spaces: readable(mySpaces()...), Situation: "我准备删除一批数据",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 0 {
		t.Fatal("召回了无权访问空间里的卡片——触发条件表上的 space_id 是冗余列，权限仍要以 cards 表为准")
	}
}

func TestRecallLimitCountsCardsNotTriggers(t *testing.T) {
	svc, _, _ := newSearchService(t, "删除")
	// 第一张卡有三条都会命中的触发条件；若截断发生在折叠之前，
	// 它会占满 limit，把第二张卡整个挤出去。
	seedCard(t, svc, spacePriv, withCard("卡片一", "准备删除数据时", "删除前确认清单时", "执行删除操作时"))
	seedCard(t, svc, spaceTeam, withCard("卡片二", "删除历史仓库时"))
	indexAll(t, svc)

	res, err := svc.RecallCards(context.Background(), RecallCardsInput{
		TenantID: tenantA, Spaces: readable(mySpaces()...), Situation: "我准备删除数据", Limit: 2,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 2 {
		t.Fatalf("limit 数的是卡片不是触发条件，期望 2 张卡，实际 %d 张", len(res.Hits))
	}
}

func TestRecallWorksWithoutEmbedder(t *testing.T) {
	svc, _ := newTestService(t) // 降级形态
	seedCard(t, svc, spacePriv, withCard("备份成功不等于数据可恢复", "准备删除聊天记录时"))

	res, err := svc.RecallCards(context.Background(), RecallCardsInput{
		TenantID: tenantA, Spaces: readable(mySpaces()...), Situation: "我准备删除聊天记录",
	})
	if err != nil {
		t.Fatal(err)
	}
	// 精度下降，但不是失灵——这正是给 card_recall_triggers 补全文索引的理由。
	if len(res.Hits) != 1 {
		t.Fatalf("没有 Embedder 时召回应退化成全文匹配，实际 %d 张", len(res.Hits))
	}
	if res.SemanticAvailable {
		t.Fatal("降级时 SemanticAvailable 必须是 false")
	}
}

func TestSearchCardsFindsByContent(t *testing.T) {
	svc, _ := newTestService(t)
	card := seedCard(t, svc, spacePriv, withCard("备份成功不等于数据可恢复"))

	res, err := svc.SearchCards(context.Background(), SearchCardsInput{
		TenantID: tenantA, Spaces: readable(mySpaces()...), Query: "校验和",
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Hits) != 1 || res.Hits[0].Card.ID != card.ID {
		t.Fatalf("card_search 匹配的是卡片正文，期望命中 1 张，实际 %d 张", len(res.Hits))
	}
}

// ---------------------------------------------------------------------------
// space_stats
// ---------------------------------------------------------------------------

func TestSpaceStatsCountsEmptySpacesToo(t *testing.T) {
	svc, _ := newTestService(t)
	seedMemory(t, svc, spacePriv)

	stats, err := svc.SpaceStats(context.Background(), tenantA, mySpaces())
	if err != nil {
		t.Fatal(err)
	}
	if len(stats) != 2 {
		t.Fatalf("每个可读空间都该出现，实际 %d 个", len(stats))
	}
	// 空空间缺席的话，「这个空间是空的」和「这个空间我看不见」
	// 在返回值上长得一模一样
	if stats[0].SpaceID != spacePriv || stats[0].Memories != 1 {
		t.Fatalf("个人空间应有 1 条记忆，实际 %+v", stats[0])
	}
	if stats[1].SpaceID != spaceTeam || stats[1].Memories != 0 {
		t.Fatalf("空的团队空间应以 0 出现，实际 %+v", stats[1])
	}
}

// TestSpaceStatsUnqualifiedMatchesIsQualified 把统计口径与领域判定钉在一起。
//
// SQL 那边数的是 `body_understanding IS NULL`，这里数的是 !IsQualified()。
// 两者等价靠的是 0002「空 section 写 NULL 而非空串」加上写入侧的 nullIfEmpty；
// 哪天有人给 Card.IsQualified() 加一条判据（比如"还要求有召回条件"）
// 而忘了改 SQL，这个测试不会红——但它至少把两份定义写在了一起，
// 让改的人有机会看见另一份。
func TestSpaceStatsUnqualifiedMatchesIsQualified(t *testing.T) {
	svc, _ := newTestService(t)

	qualified := seedCard(t, svc, spacePriv, withCard("合格的卡"))
	summary, err := svc.CreateCard(context.Background(), CreateCardInput{
		TenantID: tenantA, SpaceID: spacePriv, AuthorUserID: userA,
		Title: "摄取来的长文", CardType: "文档", Status: CardDraft,
		Distilled:      "只有摘要，没有理解。",
		RecallTriggers: []string{"看到这类文档时"},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !qualified.IsQualified() || summary.IsQualified() {
		t.Fatal("测试前提不成立：一张该合格、一张该不合格")
	}

	stats, err := svc.SpaceStats(context.Background(), tenantA, mySpaces())
	if err != nil {
		t.Fatal(err)
	}
	got := stats[0]
	if got.Cards != 2 {
		t.Fatalf("卡片总数应为 2，实际 %d", got.Cards)
	}
	if got.UnqualifiedCards != 1 {
		t.Fatalf("「还有多少张卡只是摘要」应为 1，实际 %d——这个数字的用途是能被催", got.UnqualifiedCards)
	}
	if got.DraftCards != 1 {
		t.Fatalf("待审草稿应为 1，实际 %d", got.DraftCards)
	}
	// draft 的卡也有召回条件，但它不进默认检索——统计要如实反映库里有什么
	if got.RecallTriggers != 3 {
		t.Fatalf("召回条件应为 2+1=3 条，实际 %d", got.RecallTriggers)
	}
}

func TestSpaceStatsRequiresSpaces(t *testing.T) {
	svc, _ := newTestService(t)
	if _, err := svc.SpaceStats(context.Background(), tenantA, nil); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("空空间集合应报 ErrInvalidArgument，实际: %v", err)
	}
}

// ---------------------------------------------------------------------------
// 版本冲突要带上当前版本
// ---------------------------------------------------------------------------

func TestVersionConflictCarriesCurrentVersion(t *testing.T) {
	svc, _ := newTestService(t)
	m := seedMemory(t, svc, spacePriv)

	title := "改一次"
	if _, err := svc.UpdateMemory(context.Background(), UpdateMemoryInput{
		TenantID: tenantA, ID: m.ID, WritableSpaces: mySpaces(), Title: &title,
	}); err != nil {
		t.Fatal(err)
	}

	// 拿着过期的版本号再改一次
	stale := 1
	_, err := svc.UpdateMemory(context.Background(), UpdateMemoryInput{
		TenantID: tenantA, ID: m.ID, WritableSpaces: mySpaces(),
		ExpectedVersion: &stale, Title: &title,
	})
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("应当是版本冲突，实际: %v", err)
	}
	var conflict *VersionConflictError
	if !errors.As(err, &conflict) {
		t.Fatal("必须能取到当前版本号——只知道「撞车了」的话，agent 还得再发一次 get 才能重试，而那次 get 之后可能再撞一次")
	}
	if conflict.Expected != 1 || conflict.Current != 2 {
		t.Fatalf("期望 expected=1 current=2，实际 %+v", conflict)
	}
}
