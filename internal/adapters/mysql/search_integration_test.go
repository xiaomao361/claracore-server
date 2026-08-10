//go:build integration

// 检索与索引队列的适配器测试（docs/05-code-structure.md §6.4）。
//
// 与其他契约测试的隔离方式**不同**：这里的用例大多必须真提交，
// 因为 InnoDB 的 FULLTEXT 索引在事务提交时才更新——包在一个最终 rollback
// 的事务里跑，MATCH 会一条都查不到，于是"粗筛返回空"这个最该被发现的故障
// 会伪装成测试通过。所以每个用例自己按租户清理。
package mysql_test

import (
	"context"
	"database/sql"
	"testing"
	"time"

	"claracore-server/internal/adapters/mysql"
	"claracore-server/internal/domain/knowledge"
	"claracore-server/internal/platform"
)

// cleanupTenant 按租户清掉本次测试写进去的所有行。
//
// 按租户而不是按 ID 清：漏掉一张表就会在共享的开发库里留下垃圾，
// 而下一次跑测试时那些垃圾会以"多了一条命中"的形式让人白查半天。
func cleanupTenant(t *testing.T, db *sql.DB, tenants ...platform.ID) {
	t.Helper()
	t.Cleanup(func() {
		ctx := context.Background()
		for _, tenant := range tenants {
			for _, table := range []string{
				"embeddings", "card_recall_triggers", "labels", "links", "cards", "memories",
			} {
				if _, err := db.ExecContext(ctx, `DELETE FROM `+table+` WHERE tenant_id = ?`, string(tenant)); err != nil {
					t.Errorf("清理 %s 失败，库里留下了测试数据: %v", table, err)
				}
			}
		}
	})
}

func seedSearchFixture(t *testing.T, store *mysql.KnowledgeStore, ids *platform.IDGenerator, f kfixture, now time.Time) (visible, hidden, foreign knowledge.Memory) {
	t.Helper()
	ctx := context.Background()

	visible = f.memory(ids, f.spaceMine, now)
	visible.Title = "数据库连接池调优笔记"
	visible.Body = "把最大连接数从两百降到四十之后，超时反而消失了。"

	hidden = f.memory(ids, f.spaceHid, now) // 空间不在可见集合里
	hidden.Title = "连接池的另一篇笔记"

	foreign = f.memory(ids, f.spaceMine, now) // 同一个空间 ID，另一个租户
	foreign.TenantID = f.foreign
	foreign.Title = "连接池：别的租户的笔记"

	for _, m := range []knowledge.Memory{visible, hidden, foreign} {
		if err := store.CreateMemory(ctx, m); err != nil {
			t.Fatal(err)
		}
	}
	return visible, hidden, foreign
}

// TestSearchMemoriesPushesFiltersIntoSQL 是这一层最要紧的一条：
// 权限与租户过滤必须发生在 SQL 里，不是发生在应用层。
func TestSearchMemoriesPushesFiltersIntoSQL(t *testing.T) {
	db := openTestDB(t)
	store := mysql.NewKnowledgeStore(db)
	ctx := context.Background()
	ids := platform.NewIDGenerator(platform.SystemClock{})
	now := time.Now().UTC().Truncate(time.Millisecond)
	f := newKFixture(ids)
	cleanupTenant(t, db, f.tenant, f.foreign)

	visible, hidden, foreign := seedSearchFixture(t, store, ids, f, now)

	got, err := store.SearchMemories(ctx, knowledge.CandidateQuery{
		TenantID: f.tenant, SpaceIDs: f.visible(), Query: "连接池", Limit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("期望只命中可见空间里的那一条，实际 %d 条", len(got))
	}
	switch got[0].Memory.ID {
	case hidden.ID:
		t.Fatal("返回了无权访问空间里的内容——space_id IN (...) 没有下推到 SQL")
	case foreign.ID:
		t.Fatal("返回了别的租户的内容——漏了 WHERE tenant_id = ?，这是跨租户泄露")
	case visible.ID:
	default:
		t.Fatalf("返回了意料之外的行: %s", got[0].Memory.ID)
	}
	if got[0].TextScore <= 0 {
		t.Fatalf("中文全文相关性应大于 0（ngram 分词生效），实际 %v", got[0].TextScore)
	}
}

// TestSearchKeepsTextMisses 锁住「MATCH 只排序、不做 WHERE 过滤」这条设计。
//
// 把 MATCH 放进 WHERE 会让候选集只剩字面命中的行，于是第二段的向量精算
// 再准也没用——语义相关但字面不沾边的内容根本进不了候选集。
func TestSearchKeepsTextMisses(t *testing.T) {
	db := openTestDB(t)
	store := mysql.NewKnowledgeStore(db)
	ctx := context.Background()
	ids := platform.NewIDGenerator(platform.SystemClock{})
	now := time.Now().UTC().Truncate(time.Millisecond)
	f := newKFixture(ids)
	cleanupTenant(t, db, f.tenant, f.foreign)

	hit := f.memory(ids, f.spaceMine, now)
	hit.Title = "数据库连接池调优笔记"

	miss := f.memory(ids, f.spaceMine, now.Add(-time.Hour))
	miss.Title = "周五团建去哪吃"
	miss.Body = "投票结果是火锅。"

	for _, m := range []knowledge.Memory{hit, miss} {
		if err := store.CreateMemory(ctx, m); err != nil {
			t.Fatal(err)
		}
	}

	got, err := store.SearchMemories(ctx, knowledge.CandidateQuery{
		TenantID: f.tenant, SpaceIDs: f.visible(), Query: "连接池", Limit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 {
		t.Fatalf("字面没命中的行也该进候选集（等着第二段的向量给它翻案），实际 %d 条", len(got))
	}
	if got[0].Memory.ID != hit.ID {
		t.Fatal("全文命中的应排在前面——候选集截断要按非向量信号排序，不能随机丢弃")
	}
	if got[1].TextScore != 0 {
		t.Fatalf("没有字面命中的行相关性应为 0，实际 %v", got[1].TextScore)
	}
}

func TestSearchExcludesInvisibleStatus(t *testing.T) {
	db := openTestDB(t)
	store := mysql.NewKnowledgeStore(db)
	ctx := context.Background()
	ids := platform.NewIDGenerator(platform.SystemClock{})
	now := time.Now().UTC().Truncate(time.Millisecond)
	f := newKFixture(ids)
	cleanupTenant(t, db, f.tenant, f.foreign)

	deleted := f.memory(ids, f.spaceMine, now)
	deleted.Title = "数据库连接池调优笔记"
	deleted.Status = knowledge.MemoryDeleted
	if err := store.CreateMemory(ctx, deleted); err != nil {
		t.Fatal(err)
	}

	got, err := store.SearchMemories(ctx, knowledge.CandidateQuery{
		TenantID: f.tenant, SpaceIDs: f.visible(), Query: "连接池", Limit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("软删除的记忆不进默认检索，实际返回 %d 条", len(got))
	}
}

func TestSearchLabelFilterIsIntersection(t *testing.T) {
	db := openTestDB(t)
	store := mysql.NewKnowledgeStore(db)
	ctx := context.Background()
	ids := platform.NewIDGenerator(platform.SystemClock{})
	now := time.Now().UTC().Truncate(time.Millisecond)
	f := newKFixture(ids)
	cleanupTenant(t, db, f.tenant, f.foreign)

	both := f.memory(ids, f.spaceMine, now)
	both.Title = "数据库连接池调优笔记"
	one := f.memory(ids, f.spaceMine, now)
	one.Title = "连接池的另一种配法"

	for _, pair := range []struct {
		m      knowledge.Memory
		labels []string
	}{{both, []string{"mysql", "ops"}}, {one, []string{"mysql"}}} {
		if err := store.CreateMemory(ctx, pair.m); err != nil {
			t.Fatal(err)
		}
		ref := knowledge.Ref{Type: knowledge.EntityMemory, ID: pair.m.ID}
		if err := store.ReplaceLabels(ctx, f.tenant, ref, pair.labels, now); err != nil {
			t.Fatal(err)
		}
	}

	got, err := store.SearchMemories(ctx, knowledge.CandidateQuery{
		TenantID: f.tenant, SpaceIDs: f.visible(), Query: "连接池",
		Labels: []string{"mysql", "ops"}, Limit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].Memory.ID != both.ID {
		t.Fatalf("多个标签是交集不是并集（HAVING COUNT(*) = n），实际返回 %d 条", len(got))
	}
}

// TestRecallTriggersTrustCardsNotRedundantSpace 是本轮新加的一条安全属性。
//
// card_recall_triggers.space_id 是冗余列。这里刻意让它指向一个可见空间，
// 而它所属的卡片在不可见空间里——如果粗筛拿冗余列当权限依据，
// 这条触发条件就会被召回，连带整张无权访问的卡片。
func TestRecallTriggersTrustCardsNotRedundantSpace(t *testing.T) {
	db := openTestDB(t)
	store := mysql.NewKnowledgeStore(db)
	ctx := context.Background()
	ids := platform.NewIDGenerator(platform.SystemClock{})
	now := time.Now().UTC().Truncate(time.Millisecond)
	f := newKFixture(ids)
	cleanupTenant(t, db, f.tenant, f.foreign)

	card := f.card(ids, f.spaceHid, now) // 卡片在**不可见**空间
	if err := store.CreateCard(ctx, card); err != nil {
		t.Fatal(err)
	}
	// 触发条件上的冗余 space_id 指向**可见**空间（模拟一次没同步的补数据）
	if err := store.ReplaceRecallTriggers(ctx, f.tenant, card.ID, []knowledge.RecallTrigger{{
		ID: ids.New(), TenantID: f.tenant, CardID: card.ID, SpaceID: f.spaceMine,
		Text: "准备删除聊天记录时", SortOrder: 0, CreatedAt: now,
	}}); err != nil {
		t.Fatal(err)
	}

	got, err := store.SearchRecallTriggers(ctx, knowledge.CandidateQuery{
		TenantID: f.tenant, SpaceIDs: f.visible(), Query: "删除聊天记录", Limit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatal("权限必须以 cards 表为准：拿触发条件上的冗余 space_id 当依据，" +
			"任何一次没同步它就是一次静默越权")
	}
}

func TestRecallTriggersExcludeDraftCards(t *testing.T) {
	db := openTestDB(t)
	store := mysql.NewKnowledgeStore(db)
	ctx := context.Background()
	ids := platform.NewIDGenerator(platform.SystemClock{})
	now := time.Now().UTC().Truncate(time.Millisecond)
	f := newKFixture(ids)
	cleanupTenant(t, db, f.tenant, f.foreign)

	draft := f.card(ids, f.spaceMine, now)
	draft.Status = knowledge.CardDraft // 摄取产物，需人过一遍
	if err := store.CreateCard(ctx, draft); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceRecallTriggers(ctx, f.tenant, draft.ID, []knowledge.RecallTrigger{{
		ID: ids.New(), TenantID: f.tenant, CardID: draft.ID, SpaceID: f.spaceMine,
		Text: "准备删除聊天记录时", SortOrder: 0, CreatedAt: now,
	}}); err != nil {
		t.Fatal(err)
	}

	got, err := store.SearchRecallTriggers(ctx, knowledge.CandidateQuery{
		TenantID: f.tenant, SpaceIDs: f.visible(), Query: "删除聊天记录", Limit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatal("draft 卡片的触发条件不该被召回——这正是躲不掉 JOIN cards 的原因")
	}
}

func TestCountCardMatchesCountsOnlyRealHits(t *testing.T) {
	db := openTestDB(t)
	store := mysql.NewKnowledgeStore(db)
	ctx := context.Background()
	ids := platform.NewIDGenerator(platform.SystemClock{})
	now := time.Now().UTC().Truncate(time.Millisecond)
	f := newKFixture(ids)
	cleanupTenant(t, db, f.tenant, f.foreign)

	hit := f.card(ids, f.spaceMine, now)
	miss := f.card(ids, f.spaceMine, now)
	miss.Title = "周五团建"
	miss.Distilled = "投票结果是火锅。"
	miss.Understanding = "人多的时候先定时间再定地点。"
	for _, c := range []knowledge.Card{hit, miss} {
		if err := store.CreateCard(ctx, c); err != nil {
			t.Fatal(err)
		}
	}

	n, err := store.CountCardMatches(ctx, knowledge.CandidateQuery{
		TenantID: f.tenant, SpaceIDs: f.visible(), Query: "归档路径", Limit: 100,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 与 SearchCards 相反，这里 MATCH 要进 WHERE：把没命中的也数进去就是在骗人。
	if n != 1 {
		t.Fatalf("「另有 N 条卡片命中」只该数真的命中的，期望 1，实际 %d", n)
	}
}

func TestFindCardsFillsLabelsAndTriggers(t *testing.T) {
	db := openTestDB(t)
	store := mysql.NewKnowledgeStore(db)
	ctx := context.Background()
	ids := platform.NewIDGenerator(platform.SystemClock{})
	now := time.Now().UTC().Truncate(time.Millisecond)
	f := newKFixture(ids)
	cleanupTenant(t, db, f.tenant, f.foreign)

	card := f.card(ids, f.spaceMine, now)
	hiddenCard := f.card(ids, f.spaceHid, now)
	for _, c := range []knowledge.Card{card, hiddenCard} {
		if err := store.CreateCard(ctx, c); err != nil {
			t.Fatal(err)
		}
	}
	ref := knowledge.Ref{Type: knowledge.EntityCard, ID: card.ID}
	if err := store.ReplaceLabels(ctx, f.tenant, ref, []string{"codex", "安全"}, now); err != nil {
		t.Fatal(err)
	}
	if err := store.ReplaceRecallTriggers(ctx, f.tenant, card.ID, []knowledge.RecallTrigger{
		{ID: ids.New(), TenantID: f.tenant, CardID: card.ID, SpaceID: f.spaceMine, Text: "第一条", SortOrder: 0, CreatedAt: now},
		{ID: ids.New(), TenantID: f.tenant, CardID: card.ID, SpaceID: f.spaceMine, Text: "第二条", SortOrder: 1, CreatedAt: now},
	}); err != nil {
		t.Fatal(err)
	}

	got, err := store.FindCards(ctx, f.tenant, []platform.ID{card.ID, hiddenCard.ID}, f.visible())
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 {
		t.Fatalf("不可见空间的卡片应被过滤掉且不报错，实际返回 %d 张", len(got))
	}
	if len(got[0].Labels) != 2 {
		t.Fatalf("标签应被批量填充，实际 %v", got[0].Labels)
	}
	// 返回一张没有触发条件的卡，等于让调用方看不出它为什么会出现
	if len(got[0].RecallTriggers) != 2 || got[0].RecallTriggers[0].Text != "第一条" {
		t.Fatalf("召回条件应按 sort_order 批量填充，实际 %+v", got[0].RecallTriggers)
	}
}

// ---------------------------------------------------------------------------
// 索引队列
// ---------------------------------------------------------------------------

func pendingEmbedding(ids *platform.IDGenerator, f kfixture, ref knowledge.Ref, text, hash string, now time.Time) knowledge.Embedding {
	return knowledge.Embedding{
		ID: ids.New(), TenantID: f.tenant, Ref: ref, Model: "test-model",
		ContentHash: hash, SourceText: text, Status: knowledge.EmbedPending,
		CreatedAt: now, UpdatedAt: now,
	}
}

// TestEnqueueKeepsVectorWhenContentUnchanged 验证 upsert 里那段
// ON DUPLICATE KEY UPDATE 的赋值顺序真的对。
//
// content_hash 一旦被提前覆盖，前面几行用来判断「内容变了没有」的比较
// 就全部退化成恒等式，每次入队都会作废向量——那不会报错，只会让
// 每改一次标签都触发一次真金白银的外部调用。
func TestEnqueueKeepsVectorWhenContentUnchanged(t *testing.T) {
	db := openTestDB(t)
	store := mysql.NewKnowledgeStore(db)
	ctx := context.Background()
	ids := platform.NewIDGenerator(platform.SystemClock{})
	now := time.Now().UTC().Truncate(time.Millisecond)
	f := newKFixture(ids)
	cleanupTenant(t, db, f.tenant, f.foreign)

	ref := knowledge.Ref{Type: knowledge.EntityMemory, ID: ids.New()}
	e := pendingEmbedding(ids, f, ref, "原文", "hash-a", now)
	if err := store.EnqueueEmbedding(ctx, e); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveEmbeddingVector(ctx, e.ID, []float32{1, 2, 3}, now); err != nil {
		t.Fatal(err)
	}

	// 同样的 hash 再入队一次（对应"只改了标签"）
	again := pendingEmbedding(ids, f, ref, "原文", "hash-a", now.Add(time.Minute))
	if err := store.EnqueueEmbedding(ctx, again); err != nil {
		t.Fatal(err)
	}
	vectors, err := store.LoadVectors(ctx, f.tenant, "test-model", []knowledge.Ref{ref})
	if err != nil {
		t.Fatal(err)
	}
	got := vectors[ref.String()]
	if len(got) != 3 || got[0] != 1 || got[2] != 3 {
		t.Fatalf("内容未变时已算好的向量必须原样保留，实际 %v", got)
	}

	// 换 hash（对应"正文改了"）：向量必须作废，重新排队
	changed := pendingEmbedding(ids, f, ref, "改过的原文", "hash-b", now.Add(2*time.Minute))
	if err := store.EnqueueEmbedding(ctx, changed); err != nil {
		t.Fatal(err)
	}
	vectors, err = store.LoadVectors(ctx, f.tenant, "test-model", []knowledge.Ref{ref})
	if err != nil {
		t.Fatal(err)
	}
	if _, ok := vectors[ref.String()]; ok {
		t.Fatal("正文变了还留着旧向量，检索就会拿旧内容的向量去匹配新内容")
	}
	pending, err := store.ListPendingEmbeddings(ctx, "test-model", 5, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(pending) != 1 || pending[0].SourceText != "改过的原文" {
		t.Fatalf("内容变了应重新入队且带上新文本，实际 %+v", pending)
	}
}

func TestVectorRoundTripsThroughBlob(t *testing.T) {
	db := openTestDB(t)
	store := mysql.NewKnowledgeStore(db)
	ctx := context.Background()
	ids := platform.NewIDGenerator(platform.SystemClock{})
	now := time.Now().UTC().Truncate(time.Millisecond)
	f := newKFixture(ids)
	cleanupTenant(t, db, f.tenant, f.foreign)

	ref := knowledge.Ref{Type: knowledge.EntityCardTrigger, ID: ids.New()}
	e := pendingEmbedding(ids, f, ref, "触发条件", "hash", now)
	if err := store.EnqueueEmbedding(ctx, e); err != nil {
		t.Fatal(err)
	}
	// 含负数与小数：float32 小端编码错了的话，正数往往还能凑巧看着对
	want := []float32{-0.5, 0, 1.25, 3.4028235e+38}
	if err := store.SaveEmbeddingVector(ctx, e.ID, want, now); err != nil {
		t.Fatal(err)
	}

	vectors, err := store.LoadVectors(ctx, f.tenant, "test-model", []knowledge.Ref{ref})
	if err != nil {
		t.Fatal(err)
	}
	got := vectors[ref.String()]
	if len(got) != len(want) {
		t.Fatalf("维度对不上：期望 %d，实际 %d", len(want), len(got))
	}
	for i := range want {
		if got[i] != want[i] {
			t.Fatalf("第 %d 维往返不一致：期望 %v，实际 %v", i, want[i], got[i])
		}
	}
}

func TestLoadVectorsSkipsPendingAndOtherTenants(t *testing.T) {
	db := openTestDB(t)
	store := mysql.NewKnowledgeStore(db)
	ctx := context.Background()
	ids := platform.NewIDGenerator(platform.SystemClock{})
	now := time.Now().UTC().Truncate(time.Millisecond)
	f := newKFixture(ids)
	cleanupTenant(t, db, f.tenant, f.foreign)

	stillPending := knowledge.Ref{Type: knowledge.EntityMemory, ID: ids.New()}
	if err := store.EnqueueEmbedding(ctx, pendingEmbedding(ids, f, stillPending, "还没算", "h1", now)); err != nil {
		t.Fatal(err)
	}

	otherTenant := knowledge.Ref{Type: knowledge.EntityMemory, ID: ids.New()}
	e := pendingEmbedding(ids, f, otherTenant, "别人的", "h2", now)
	e.TenantID = f.foreign
	if err := store.EnqueueEmbedding(ctx, e); err != nil {
		t.Fatal(err)
	}
	if err := store.SaveEmbeddingVector(ctx, e.ID, []float32{9, 9}, now); err != nil {
		t.Fatal(err)
	}

	vectors, err := store.LoadVectors(ctx, f.tenant, "test-model", []knowledge.Ref{stillPending, otherTenant})
	if err != nil {
		t.Fatal(err)
	}
	if len(vectors) != 0 {
		t.Fatalf("pending 的没有向量、别的租户的不该看见，实际返回 %d 条", len(vectors))
	}
}

func TestPendingQueueOrderAndAttemptCap(t *testing.T) {
	db := openTestDB(t)
	store := mysql.NewKnowledgeStore(db)
	ctx := context.Background()
	ids := platform.NewIDGenerator(platform.SystemClock{})
	now := time.Now().UTC().Truncate(time.Millisecond)
	f := newKFixture(ids)
	cleanupTenant(t, db, f.tenant, f.foreign)

	older := pendingEmbedding(ids, f, knowledge.Ref{Type: knowledge.EntityMemory, ID: ids.New()}, "旧的", "h1", now.Add(-time.Hour))
	newer := pendingEmbedding(ids, f, knowledge.Ref{Type: knowledge.EntityMemory, ID: ids.New()}, "新的", "h2", now)
	for _, e := range []knowledge.Embedding{older, newer} {
		if err := store.EnqueueEmbedding(ctx, e); err != nil {
			t.Fatal(err)
		}
	}

	got, err := store.ListPendingEmbeddings(ctx, "test-model", 5, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 2 || got[0].ID != older.ID {
		t.Fatal("最久没动的应排在前面：失败的行每失败一次就更新时间戳，于是自然排到队尾")
	}

	// 失败 5 次之后不再被领到——一条注定失败的内容不该霸着每一批的名额
	for i := 0; i < 5; i++ {
		if err := store.MarkEmbeddingsFailed(ctx, []platform.ID{older.ID}, "模型拒绝了这段文本", now); err != nil {
			t.Fatal(err)
		}
	}
	got, err = store.ListPendingEmbeddings(ctx, "test-model", 5, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 1 || got[0].ID != newer.ID {
		t.Fatalf("超过重试上限的行不该再被领到，实际 %d 条", len(got))
	}
}

func TestDeleteCardTriggerEmbeddings(t *testing.T) {
	db := openTestDB(t)
	store := mysql.NewKnowledgeStore(db)
	ctx := context.Background()
	ids := platform.NewIDGenerator(platform.SystemClock{})
	now := time.Now().UTC().Truncate(time.Millisecond)
	f := newKFixture(ids)
	cleanupTenant(t, db, f.tenant, f.foreign)

	card := f.card(ids, f.spaceMine, now)
	if err := store.CreateCard(ctx, card); err != nil {
		t.Fatal(err)
	}
	trigger := knowledge.RecallTrigger{
		ID: ids.New(), TenantID: f.tenant, CardID: card.ID, SpaceID: f.spaceMine,
		Text: "准备删除数据时", SortOrder: 0, CreatedAt: now,
	}
	if err := store.ReplaceRecallTriggers(ctx, f.tenant, card.ID, []knowledge.RecallTrigger{trigger}); err != nil {
		t.Fatal(err)
	}
	ref := knowledge.Ref{Type: knowledge.EntityCardTrigger, ID: trigger.ID}
	if err := store.EnqueueEmbedding(ctx, pendingEmbedding(ids, f, ref, trigger.Text, "h", now)); err != nil {
		t.Fatal(err)
	}

	// 顺序要求：先删向量，再替换触发条件。反过来的话这个 JOIN
	// 就再也定位不到旧行留下的向量。
	if err := store.DeleteCardTriggerEmbeddings(ctx, f.tenant, card.ID); err != nil {
		t.Fatal(err)
	}
	got, err := store.ListPendingEmbeddings(ctx, "test-model", 5, 10)
	if err != nil {
		t.Fatal(err)
	}
	if len(got) != 0 {
		t.Fatalf("旧召回条件的向量行必须一起删掉，否则永远躺在库里既查不到也清不掉，实际残留 %d 条", len(got))
	}
}
