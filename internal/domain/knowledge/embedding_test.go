package knowledge

import (
	"context"
	"errors"
	"strings"
	"testing"
	"time"

	"claracore-server/internal/platform"
)

// pendingCount 数队列里还有多少条没算完。
func (f *fakeStore) pendingCount(model string) int {
	n := 0
	for _, e := range f.embeddings {
		if e.Model == model && e.Status != EmbedReady {
			n++
		}
	}
	return n
}

func (f *fakeStore) embeddingOf(ref Ref, model string) (Embedding, bool) {
	e, ok := f.embeddings[embKey(ref, model)]
	return e, ok
}

// ---------------------------------------------------------------------------
// 入队：写入路径不依赖外部服务
// ---------------------------------------------------------------------------

func TestCreateEnqueuesPendingEmbedding(t *testing.T) {
	svc, store, emb := newSearchService(t, "连接池")
	m := seedMemory(t, svc, spacePriv)

	e, ok := store.embeddingOf(Ref{EntityMemory, m.ID}, emb.Model())
	if !ok {
		t.Fatal("创建记忆时应在同一个事务里入队")
	}
	if e.Status != EmbedPending || len(e.Vector) != 0 {
		t.Fatalf("入队时应是 pending 且没有向量，实际 status=%s vector=%d", e.Status, len(e.Vector))
	}
	// 写入路径一次外部调用都不该发生：Ollama 挂了照样要能记东西。
	if emb.calls != 0 {
		t.Fatalf("写入路径不得调用 Embedder，实际调了 %d 次", emb.calls)
	}
	if e.SourceText == "" || !strings.Contains(e.SourceText, m.Title) {
		t.Fatalf("入队必须存下真正要算的那段文本，实际: %q", e.SourceText)
	}
}

func TestNoEmbedderMeansNoQueue(t *testing.T) {
	svc, store := newTestService(t) // 降级形态
	seedMemory(t, svc, spacePriv)

	if len(store.embeddings) != 0 {
		t.Fatal("没有 Embedder 就没有模型名，攒一堆不知道用哪个模型算的 pending 行只是垃圾")
	}
}

func TestEnqueueRollsBackWithContent(t *testing.T) {
	svc, store, emb := newSearchService(t, "连接池")
	store.failOn = "ReplaceLabels" // 写内容的中途失败

	_, err := svc.CreateMemory(context.Background(), CreateMemoryInput{
		TenantID: tenantA, SpaceID: spacePriv, AuthorUserID: userA,
		Title: "标题", Body: "正文",
	})
	if err == nil {
		t.Fatal("注入的故障应让创建失败")
	}
	if len(store.embeddings) != 0 {
		t.Fatal("入队必须与内容同事务回滚——否则会留下一条指向不存在内容的队列行")
	}
	if store.pendingCount(emb.Model()) != 0 {
		t.Fatal("回滚后队列里不该有残留")
	}
}

func TestCardEnqueuesBodyAndEveryTrigger(t *testing.T) {
	svc, store, emb := newSearchService(t, "删除")
	c := seedCard(t, svc, spacePriv, withCard("删除前必须先列清单", "准备删除数据时", "清理隐藏目录时"))

	if _, ok := store.embeddingOf(Ref{EntityCard, c.ID}, emb.Model()); !ok {
		t.Fatal("卡片正文要有自己的向量行（card_search 用）")
	}
	for _, tr := range c.RecallTriggers {
		if _, ok := store.embeddingOf(Ref{EntityCardTrigger, tr.ID}, emb.Model()); !ok {
			t.Fatalf("每条召回条件都要单独 embedding，缺了: %q", tr.Text)
		}
	}
	if len(store.embeddings) != 3 {
		t.Fatalf("期望 1 条正文 + 2 条召回条件 = 3 行，实际 %d 行", len(store.embeddings))
	}
}

// TestCardEmbedTextExcludesOriginal 锁住「原文不进向量」这条取舍，
// 与 0002 迁移里 ft_cards 不含 body_original 是同一个理由。
func TestCardEmbedTextExcludesOriginal(t *testing.T) {
	c := Card{
		Title:         "标题",
		Original:      "现场原话噪音很大很大很大",
		Distilled:     "提炼过的一句",
		Understanding: "我的理解",
		WhyKeep:       "值得留着",
		Unresolved:    "还没想清楚的",
	}
	text := cardEmbedText(c)
	if strings.Contains(text, "现场原话") {
		t.Fatal("原文是未经整理的现场记录，纳进向量只会稀释「提炼」与「理解」的信号")
	}
	if !strings.Contains(text, "提炼过的一句") || !strings.Contains(text, "我的理解") {
		t.Fatalf("提炼与理解必须进向量，实际: %q", text)
	}
	if strings.Contains(text, "值得留着") || strings.Contains(text, "还没想清楚的") {
		t.Fatal("WhyKeep / Unresolved 回答的不是「这张卡在讲什么」，不进向量")
	}
}

func TestEmbedTextIsTruncated(t *testing.T) {
	svc, store, emb := newSearchService(t, "长")
	long := strings.Repeat("长", MaxEmbedTextLen*2)

	m, err := svc.CreateMemory(context.Background(), CreateMemoryInput{
		TenantID: tenantA, SpaceID: spacePriv, AuthorUserID: userA,
		Title: "超长正文", Body: long,
	})
	if err != nil {
		t.Fatal(err)
	}
	e, _ := store.embeddingOf(Ref{EntityMemory, m.ID}, emb.Model())
	if n := len([]rune(e.SourceText)); n != MaxEmbedTextLen {
		t.Fatalf("超长文本应截断到 %d 字，实际 %d 字", MaxEmbedTextLen, n)
	}
	// 按字节截会把一个汉字切成两半，留下无效 UTF-8 一路传到 HTTP 请求体才炸。
	if !utf8Valid(e.SourceText) {
		t.Fatal("截断后必须仍是合法 UTF-8——说明是按 rune 截的，不是按 byte")
	}
}

func TestBulkImportEmbeddingConstructorsReuseCanonicalTextAndQueueShape(t *testing.T) {
	at := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	ids := platform.NewIDGenerator(platform.FixedClock{T: at})
	tests := []struct {
		name string
		ref  Ref
		want string
		make func() (Embedding, bool)
	}{
		{name: "memory", ref: Ref{Type: EntityMemory, ID: "01MEMORYIMPORT00000000000"}, want: "标题\n\n正文", make: func() (Embedding, bool) {
			return NewPendingMemoryEmbedding(ids, tenantA, "01MEMORYIMPORT00000000000", "bge-m3", " 标题 ", " 正文 ", at)
		}},
		{name: "card", ref: Ref{Type: EntityCard, ID: "01CARDIMPORT0000000000000"}, want: "卡片\n\n提炼\n\n理解", make: func() (Embedding, bool) {
			return NewPendingCardEmbedding(ids, tenantA, "01CARDIMPORT0000000000000", "bge-m3", "卡片", "提炼", "理解", at)
		}},
		{name: "trigger", ref: Ref{Type: EntityCardTrigger, ID: "01TRIGGERIMPORT0000000000"}, want: "准备删除数据时", make: func() (Embedding, bool) {
			return NewPendingTriggerEmbedding(ids, tenantA, "01TRIGGERIMPORT0000000000", "bge-m3", " 准备删除数据时 ", at)
		}},
	}
	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			embedding, ok := tc.make()
			if !ok || embedding.ID == "" || embedding.TenantID != tenantA || embedding.Ref != tc.ref || embedding.Model != "bge-m3" || embedding.SourceText != tc.want || embedding.ContentHash != contentHash(tc.want) || embedding.Status != EmbedPending || embedding.CreatedAt != at || embedding.UpdatedAt != at {
				t.Fatalf("unexpected canonical pending embedding: ok=%v row=%+v", ok, embedding)
			}
		})
	}
	if _, ok := NewPendingMemoryEmbedding(ids, tenantA, "01EMPTYIMPORT000000000000", "", "title", "body", at); ok {
		t.Fatal("an empty model must never create an unowned persistent queue row")
	}
}

func utf8Valid(s string) bool {
	for _, r := range s {
		if r == '�' {
			return false
		}
	}
	return true
}

// ---------------------------------------------------------------------------
// 不做无谓的重算
// ---------------------------------------------------------------------------

func TestLabelOnlyUpdateKeepsVector(t *testing.T) {
	svc, store, emb := newSearchService(t, "连接池")
	m := seedMemory(t, svc, spacePriv)
	indexAll(t, svc)

	before, _ := store.embeddingOf(Ref{EntityMemory, m.ID}, emb.Model())
	if before.Status != EmbedReady {
		t.Fatalf("索引后应是 ready，实际 %s", before.Status)
	}

	if _, err := svc.UpdateMemory(context.Background(), UpdateMemoryInput{
		TenantID: tenantA, ID: m.ID, WritableSpaces: mySpaces(),
		Labels: &[]string{"新标签"},
	}); err != nil {
		t.Fatal(err)
	}

	after, _ := store.embeddingOf(Ref{EntityMemory, m.ID}, emb.Model())
	if after.Status != EmbedReady || len(after.Vector) == 0 {
		t.Fatal("只改标签不该让向量作废——每改一次标签重算一次向量是真金白银的外部调用")
	}
}

func TestBodyChangeInvalidatesVector(t *testing.T) {
	svc, store, emb := newSearchService(t, "连接池", "备份")
	m := seedMemory(t, svc, spacePriv)
	indexAll(t, svc)

	newBody := "改成完全不同的内容：备份策略。"
	if _, err := svc.UpdateMemory(context.Background(), UpdateMemoryInput{
		TenantID: tenantA, ID: m.ID, WritableSpaces: mySpaces(), Body: &newBody,
	}); err != nil {
		t.Fatal(err)
	}

	after, _ := store.embeddingOf(Ref{EntityMemory, m.ID}, emb.Model())
	if after.Status != EmbedPending || len(after.Vector) != 0 {
		t.Fatalf("正文变了必须重新入队并作废旧向量，否则检索会拿旧内容的向量匹配新内容\n  实际 status=%s vector=%d",
			after.Status, len(after.Vector))
	}
	if !strings.Contains(after.SourceText, "备份策略") {
		t.Fatalf("入队的文本应是新内容，实际: %q", after.SourceText)
	}
}

func TestReplacingTriggersDropsTheirEmbeddings(t *testing.T) {
	svc, store, emb := newSearchService(t, "删除")
	c := seedCard(t, svc, spacePriv, withCard("卡", "旧条件一", "旧条件二"))
	indexAll(t, svc)
	oldTriggers := c.RecallTriggers

	if _, err := svc.UpdateCard(context.Background(), UpdateCardInput{
		TenantID: tenantA, ID: c.ID, WritableSpaces: mySpaces(),
		RecallTriggers: &[]string{"新条件"},
	}); err != nil {
		t.Fatal(err)
	}

	for _, tr := range oldTriggers {
		if _, ok := store.embeddingOf(Ref{EntityCardTrigger, tr.ID}, emb.Model()); ok {
			t.Fatalf("旧召回条件的向量行必须一起删掉，否则它会永远躺在库里既查不到也清不掉（%q）", tr.Text)
		}
	}
	// 新条件要有自己的队列行
	if store.pendingCount(emb.Model()) == 0 {
		t.Fatal("替换后的新召回条件应重新入队")
	}
}

func TestPromotedCopyGetsItsOwnEmbedding(t *testing.T) {
	svc, store, emb := newSearchService(t, "连接池")
	src := seedMemory(t, svc, spacePriv)

	ref, err := svc.Promote(context.Background(), PromoteInput{
		TenantID: tenantA, Ref: Ref{EntityMemory, src.ID},
		ReadableSpaces: mySpaces(), TargetSpaceID: spaceTeam, ActorUserID: userB,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 副本是另一个空间里的另一条内容，检索按各自空间的权限取到，不能共用原件那一行。
	if _, ok := store.embeddingOf(ref, emb.Model()); !ok {
		t.Fatal("提升产生的副本要有自己的向量行")
	}
}

// ---------------------------------------------------------------------------
// IndexPending：失败与重试
// ---------------------------------------------------------------------------

func TestIndexPendingWritesVectors(t *testing.T) {
	svc, store, emb := newSearchService(t, "连接池")
	m := seedMemory(t, svc, spacePriv)

	report, err := svc.IndexPending(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if report.Picked != 1 || report.Indexed != 1 || report.Failed != 0 {
		t.Fatalf("期望领 1 条算 1 条，实际 %+v", report)
	}
	e, _ := store.embeddingOf(Ref{EntityMemory, m.ID}, emb.Model())
	if e.Status != EmbedReady || len(e.Vector) == 0 {
		t.Fatalf("算完应置 ready 并写回向量，实际 status=%s vector=%d", e.Status, len(e.Vector))
	}
}

func TestIndexPendingIsNoOpWithoutEmbedder(t *testing.T) {
	svc, _ := newTestService(t)
	report, err := svc.IndexPending(context.Background(), 0)
	if err != nil {
		t.Fatalf("降级形态下 worker 空转不该报错: %v", err)
	}
	if report.Picked != 0 {
		t.Fatalf("没有 Embedder 就没有队列，实际领了 %d 条", report.Picked)
	}
}

func TestIndexPendingMarksFailureAndRetries(t *testing.T) {
	svc, store, emb := newSearchService(t, "连接池")
	m := seedMemory(t, svc, spacePriv)
	emb.fail = errors.New("dial tcp: connection refused")

	report, err := svc.IndexPending(context.Background(), 0)
	if err == nil {
		t.Fatal("外部服务失败应把错误带回给调用方，让 worker 能记日志")
	}
	if report.Failed != 1 {
		t.Fatalf("期望 1 条失败，实际 %+v", report)
	}
	e, _ := store.embeddingOf(Ref{EntityMemory, m.ID}, emb.Model())
	if e.Status != EmbedFailed || e.Attempts != 1 || e.LastError == "" {
		t.Fatalf("失败要留下痕迹给运维看，实际 status=%s attempts=%d err=%q", e.Status, e.Attempts, e.LastError)
	}

	// 行还在队列里，服务恢复后会被重新领到。
	emb.fail = nil
	report, err = svc.IndexPending(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if report.Indexed != 1 {
		t.Fatalf("服务恢复后失败的行应被重试，实际 %+v", report)
	}
}

func TestIndexPendingStopsRetryingAfterMaxAttempts(t *testing.T) {
	svc, store, emb := newSearchService(t, "连接池")
	seedMemory(t, svc, spacePriv)
	emb.fail = errors.New("模型拒绝了这段文本")

	for i := 0; i < maxEmbedAttempts; i++ {
		if _, err := svc.IndexPending(context.Background(), 0); err == nil {
			t.Fatalf("第 %d 轮应当失败", i+1)
		}
	}
	// 一条注定失败的内容不该永远占着每一批的名额，把整个队列拖死。
	report, err := svc.IndexPending(context.Background(), 0)
	if err != nil {
		t.Fatal(err)
	}
	if report.Picked != 0 {
		t.Fatalf("超过 %d 次后不该再领这条，实际领了 %d 条", maxEmbedAttempts, report.Picked)
	}
	// 但行还在——「这条内容算不出向量」本身是要给运维看的信息。
	if store.pendingCount(emb.Model()) != 1 {
		t.Fatal("放弃重试不等于删除队列行")
	}
}

// TestIndexPendingRejectsMisalignedVectors 是这一层最要紧的一条防线。
//
// 向量数量与文本对不上时按下标硬配，会把向量安到别的内容头上——
// 那是一种不报错、只让检索悄悄变差的故障，事后几乎无法定位。
func TestIndexPendingRejectsMisalignedVectors(t *testing.T) {
	svc, store, emb := newSearchService(t, "连接池")
	seedMemory(t, svc, spacePriv)
	seedMemory(t, svc, spaceTeam)

	emb.short = 1 // 只返回第一条的向量

	report, err := svc.IndexPending(context.Background(), 0)
	if err == nil {
		t.Fatal("数量对不上必须整批判失败")
	}
	if report.Indexed != 0 {
		t.Fatalf("一条都不该写回，实际写回 %d 条", report.Indexed)
	}
	for _, e := range store.embeddings {
		if e.Status == EmbedReady {
			t.Fatal("错位的向量绝不能落库")
		}
	}
}

func TestIndexPendingBatchIsClamped(t *testing.T) {
	if clampBatch(0) != defaultIndexBatch {
		t.Fatalf("0 应取默认批量 %d", defaultIndexBatch)
	}
	if clampBatch(-3) != defaultIndexBatch {
		t.Fatalf("负数应取默认批量 %d", defaultIndexBatch)
	}
	if clampBatch(99999) != maxIndexBatch {
		t.Fatalf("超大批量应截到 %d", maxIndexBatch)
	}
	if clampBatch(7) != 7 {
		t.Fatal("正常值应原样通过")
	}
}

func TestBackfillQueuesVisibleContentForNewModel(t *testing.T) {
	disabled, store := newTestService(t)
	seedMemory(t, disabled, spacePriv)
	seedCard(t, disabled, spacePriv, withCard("备份", "准备删数据时", "准备恢复数据时"))
	if len(store.embeddings) != 0 {
		t.Fatal("夹具必须模拟先无模型运行")
	}

	clock := &platform.OffsetClock{Base: time.Date(2026, 8, 12, 9, 0, 0, 0, time.UTC), Step: time.Minute}
	emb := newFakeEmbedder("备份", "数据")
	enabled := NewService(store, clock, platform.NewIDGenerator(clock), emb)
	report, err := enabled.BackfillEmbeddings(context.Background(), tenantA, 20)
	if err != nil {
		t.Fatal(err)
	}
	if !report.Enabled || report.Memories != 1 || report.Cards != 1 || report.Triggers != 2 || report.Total() != 4 {
		t.Fatalf("新模型应补齐可检索内容及每条召回条件，实际 %+v", report)
	}
	if store.pendingCount(emb.Model()) != 4 {
		t.Fatalf("应有 4 条待算，实际 %d", store.pendingCount(emb.Model()))
	}

	again, err := enabled.BackfillEmbeddings(context.Background(), tenantA, 20)
	if err != nil {
		t.Fatal(err)
	}
	if again.Total() != 0 {
		t.Fatalf("回填必须幂等，第二次不应重置已有队列，实际 %+v", again)
	}
}

func TestEmbeddingHealthAndManualRetryAreTenantScoped(t *testing.T) {
	svc, store, emb := newSearchService(t, "连接池")
	m := seedMemory(t, svc, spacePriv)
	e, _ := store.embeddingOf(Ref{EntityMemory, m.ID}, emb.Model())
	for i := 0; i < maxEmbedAttempts; i++ {
		if err := store.MarkEmbeddingsFailed(context.Background(), []platform.ID{e.ID}, "模型不可用", time.Now()); err != nil {
			t.Fatal(err)
		}
	}
	store.embeddings["foreign"] = Embedding{
		ID: "01FOREIGNEMBEDDINGAAAAAAAA", TenantID: tenantB,
		Ref: Ref{EntityMemory, "01FOREIGNMEMORYAAAAAAAAAA"}, Model: emb.Model(),
		Status: EmbedFailed, Attempts: maxEmbedAttempts, LastError: "别的租户",
	}

	health, err := svc.EmbeddingHealth(context.Background(), tenantA, 20)
	if err != nil {
		t.Fatal(err)
	}
	if !health.Enabled || health.Stats.Failed != 1 || health.Stats.Exhausted != 1 || len(health.Failures) != 1 {
		t.Fatalf("应显示本租户失败与耗尽数量，实际 %+v", health)
	}
	n, err := svc.RetryEmbeddingFailures(context.Background(), tenantA)
	if err != nil {
		t.Fatal(err)
	}
	if n != 1 {
		t.Fatalf("只应重试本租户的一条，实际 %d", n)
	}
	got, _ := store.embeddingOf(Ref{EntityMemory, m.ID}, emb.Model())
	if got.Status != EmbedPending || got.Attempts != 0 || got.LastError != "" {
		t.Fatalf("人工重试应重置队列状态，实际 %+v", got)
	}
	if store.embeddings["foreign"].Status != EmbedFailed {
		t.Fatal("人工重试不得跨租户")
	}
}
