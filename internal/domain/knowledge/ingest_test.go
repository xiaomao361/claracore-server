package knowledge

import (
	"context"
	"errors"
	"testing"

	"claracore-server/internal/platform"
)

func doc(key, hash, title, body string) Document {
	return Document{
		SourceKey: key, SourcePath: key + ".md", ContentHash: hash,
		Title: title, Body: body,
	}
}

func ingestInto(t *testing.T, svc *Service, docs ...Document) IngestReport {
	t.Helper()
	report, err := svc.Ingest(context.Background(), IngestInput{
		TenantID: tenantA, SpaceID: spaceTeam, AuthorUserID: userA,
		Documents: docs,
	})
	if err != nil {
		t.Fatal(err)
	}
	return report
}

// ---------------------------------------------------------------------------
// 幂等：这一层存在的全部理由
// ---------------------------------------------------------------------------

func TestIngestIsIdempotent(t *testing.T) {
	svc, store := newTestService(t)
	d := doc("k1", "h1", "连接池调优", "把最大连接数降到四十。")

	first := ingestInto(t, svc, d)
	if first.Created != 1 || first.Updated != 0 || first.Unchanged != 0 {
		t.Fatalf("首次应新建，实际 %+v", first)
	}

	// 同一份文档再摄一次：**一步都不该走**
	second := ingestInto(t, svc, d)
	if second.Unchanged != 1 || second.Created != 0 || second.Updated != 0 {
		t.Fatalf("内容没变时应完全跳过，实际 %+v", second)
	}
	if len(store.cards) != 1 {
		t.Fatalf("反复摄取必须得到同一张卡，实际库里有 %d 张", len(store.cards))
	}
}

// TestUnchangedIngestDoesNotTouchVersionOrVector 是幂等真正的代价所在。
//
// 没有这条，重扫一个没改过的目录会把每张卡的 version 推高一格、
// 把每个向量作废重算——那是真金白银的外部调用，而且「谁在什么时候
// 改了这张卡」的版本历史会被一堆无意义的自增淹掉。
func TestUnchangedIngestDoesNotTouchVersionOrVector(t *testing.T) {
	svc, store, emb := newSearchService(t, "连接池")
	d := doc("k1", "h1", "连接池调优", "把最大连接数降到四十。")

	if _, err := svc.Ingest(context.Background(), IngestInput{
		TenantID: tenantA, SpaceID: spaceTeam, AuthorUserID: userA, Documents: []Document{d},
	}); err != nil {
		t.Fatal(err)
	}
	indexAll(t, svc)

	var cardID string
	for id := range store.cards {
		cardID = string(id)
	}
	beforeVersion := store.cards[platform.ID(cardID)].Version
	beforeEmb, _ := store.embeddingOf(Ref{EntityCard, platform.ID(cardID)}, emb.Model())

	if _, err := svc.Ingest(context.Background(), IngestInput{
		TenantID: tenantA, SpaceID: spaceTeam, AuthorUserID: userA, Documents: []Document{d},
	}); err != nil {
		t.Fatal(err)
	}

	after := store.cards[platform.ID(cardID)]
	if after.Version != beforeVersion {
		t.Fatalf("内容没变不该推高 version：%d → %d", beforeVersion, after.Version)
	}
	afterEmb, _ := store.embeddingOf(Ref{EntityCard, platform.ID(cardID)}, emb.Model())
	if afterEmb.Status != EmbedReady || len(afterEmb.Vector) == 0 {
		t.Fatal("内容没变不该作废已算好的向量——那是真金白银的外部调用")
	}
	if afterEmb.UpdatedAt != beforeEmb.UpdatedAt {
		t.Fatal("内容没变时连队列行都不该被碰")
	}
}

func TestChangedContentUpdatesInPlace(t *testing.T) {
	svc, store := newTestService(t)
	ingestInto(t, svc, doc("k1", "h1", "旧标题", "旧正文。"))

	report := ingestInto(t, svc, doc("k1", "h2", "新标题", "新正文。"))
	if report.Updated != 1 || report.Created != 0 {
		t.Fatalf("内容变了应更新而不是新建，实际 %+v", report)
	}
	if len(store.cards) != 1 {
		t.Fatalf("仍应只有一张卡，实际 %d 张", len(store.cards))
	}
	for _, c := range store.cards {
		if c.Title != "新标题" || c.Original != "新正文。" {
			t.Fatalf("标题与原文应被更新，实际 %+v", c)
		}
		if c.Version != 2 {
			t.Fatalf("更新应推高 version，实际 %d", c.Version)
		}
	}
}

func TestSourceNameBackfillDoesNotInvalidateVector(t *testing.T) {
	svc, store, emb := newSearchService(t, "连接池")
	d := doc("k1", "h1", "连接池调优", "把最大连接数降到四十。")
	ingestInto(t, svc, d)
	indexAll(t, svc)

	var cardID platform.ID
	for id := range store.cards {
		cardID = id
	}
	before, _ := store.embeddingOf(Ref{EntityCard, cardID}, emb.Model())
	report, err := svc.Ingest(context.Background(), IngestInput{
		TenantID: tenantA, SpaceID: spaceTeam, AuthorUserID: userA,
		SourceName: "team-handbook", Documents: []Document{d},
	})
	if err != nil {
		t.Fatal(err)
	}
	if report.Updated != 1 || store.cards[cardID].SourceName != "team-handbook" {
		t.Fatalf("稳定来源名应补写到原卡，实际 report=%+v card=%+v", report, store.cards[cardID])
	}
	after, _ := store.embeddingOf(Ref{EntityCard, cardID}, emb.Model())
	if after.Status != EmbedReady || after.UpdatedAt != before.UpdatedAt {
		t.Fatal("只补来源元数据时不该让已有向量失效")
	}
}

// TestUpdateKeepsHumanWork 是这一刀最要紧的一条取舍。
//
// 源文档改了一个错别字就把提炼、理解、召回条件清空，
// 等于每次上游更新都惩罚一次做过功的人。
func TestUpdateKeepsHumanWork(t *testing.T) {
	svc, store := newTestService(t)
	ingestInto(t, svc, doc("k1", "h1", "标题", "原文第一版。"))

	// agent 与人给这张卡加上了价值
	var cardID string
	for id := range store.cards {
		cardID = string(id)
	}
	if _, err := svc.UpdateCard(context.Background(), UpdateCardInput{
		TenantID: tenantA, ID: platform.ID(cardID), WritableSpaces: mySpaces(),
		Distilled:      ptr("提炼过的一句"),
		Understanding:  ptr("我的理解"),
		RecallTriggers: &[]string{"遇到这类问题时"},
	}); err != nil {
		t.Fatal(err)
	}

	// 上游文档更新了
	ingestInto(t, svc, doc("k1", "h2", "标题", "原文第二版。"))

	got := store.cards[platform.ID(cardID)]
	if got.Original != "原文第二版。" {
		t.Fatalf("原文应被更新，实际 %q", got.Original)
	}
	if got.Understanding != "我的理解" || got.Distilled != "提炼过的一句" {
		t.Fatal("提炼与理解是人后来加的，源更新时绝不能清空——那会惩罚做过功的人")
	}
	if len(store.triggers[platform.ID(cardID)]) != 1 {
		t.Fatal("召回条件同样要保住")
	}
}

// ---------------------------------------------------------------------------
// 产物形态
// ---------------------------------------------------------------------------

func TestIngestedCardsAreDraftWithoutTriggers(t *testing.T) {
	svc, store := newTestService(t)
	ingestInto(t, svc, doc("k1", "h1", "摄进来的文档", "一段原文。"))

	for id, c := range store.cards {
		// 摄取产物一律 draft：一张没有召回条件的卡进了检索只会稀释结果
		if c.Status != CardDraft {
			t.Fatalf("摄取产物必须是 draft，实际 %s", c.Status)
		}
		// 原文进 Original，提炼留空——那是 agent 的活
		if c.Original == "" || c.Understanding != "" || c.Distilled != "" {
			t.Fatalf("摄取只落原文，不提炼，实际 %+v", c)
		}
		if c.IsQualified() {
			t.Fatal("没有「理解」的卡不该算合格")
		}
		if len(store.triggers[id]) != 0 {
			t.Fatal("摄取不产出召回条件——card_recall 命不中它，直到有人写下触发条件")
		}
		if c.SourceKey == "" || c.SourceHash == "" {
			t.Fatal("摄取产物必须带上源标识与内容指纹，否则下次重扫认不出它")
		}
	}
}

func TestIngestSkipsDeprecatedButStillRecognizesIt(t *testing.T) {
	svc, store := newTestService(t)
	ingestInto(t, svc, doc("k1", "h1", "标题", "原文。"))

	var cardID string
	for id := range store.cards {
		cardID = string(id)
	}
	// 人看过之后判定这份文档没用
	if _, err := svc.UpdateCard(context.Background(), UpdateCardInput{
		TenantID: tenantA, ID: platform.ID(cardID), WritableSpaces: mySpaces(),
		Status: ptr(CardDeprecated),
	}); err != nil {
		t.Fatal(err)
	}

	report := ingestInto(t, svc, doc("k1", "h2", "标题", "原文改了。"))
	if report.Created != 0 {
		t.Fatal("重扫必须认出这是同一份源，不能新建一张——" +
			"否则「已经判过这份文档没用」的决定就被悄悄推翻了")
	}
	if store.cards[platform.ID(cardID)].Status != CardDeprecated {
		t.Fatal("摄取不该把人设定的状态改回去")
	}
}

// ---------------------------------------------------------------------------
// 失败清单
// ---------------------------------------------------------------------------

func TestBadDocumentDoesNotAbortTheBatch(t *testing.T) {
	svc, store := newTestService(t)

	report := ingestInto(t, svc,
		doc("k1", "h1", "好文档一", "正文。"),
		doc("k2", "h2", "", "没有标题"),
		doc("k3", "h3", "好文档二", "正文。"),
		doc("k4", "h4", "没有正文", ""),
	)
	// 保留成功项 + 列失败清单：一份畸形文档不该让整批回滚
	if report.Created != 2 {
		t.Fatalf("好的那两份应当写进去，实际 %+v", report)
	}
	if len(report.Failed) != 2 {
		t.Fatalf("坏的那两份应进失败清单，实际 %+v", report.Failed)
	}
	if len(store.cards) != 2 {
		t.Fatalf("库里应只有 2 张卡，实际 %d", len(store.cards))
	}
	// 只说「失败 2 条」等于没说——要能照着去修那一份
	for _, f := range report.Failed {
		if f.SourcePath == "" || f.Reason == "" {
			t.Fatalf("失败项必须带源路径与原因，实际 %+v", f)
		}
	}
	if report.Total() != 4 {
		t.Fatalf("总数应对得上，实际 %d", report.Total())
	}
}

// TestDuplicateSourceKeyInSameBatchIsRejected 防的是一种极难排查的现象。
//
// 同一批里两份文档算出同一个 SourceKey 时，它们会在唯一键上互相覆盖，
// 最终留下哪一份取决于遍历顺序——表现是「每次摄取结果都不一样」。
func TestDuplicateSourceKeyInSameBatchIsRejected(t *testing.T) {
	svc, store := newTestService(t)

	report := ingestInto(t, svc,
		doc("same", "h1", "第一份", "正文一。"),
		doc("same", "h2", "第二份", "正文二。"),
	)
	if report.Created != 1 || len(report.Failed) != 1 {
		t.Fatalf("同批重复的源标识应被拒绝而不是互相覆盖，实际 %+v", report)
	}
	if len(store.cards) != 1 {
		t.Fatalf("只该留下一张，实际 %d", len(store.cards))
	}
}

func TestIngestReportsMissingSourceCardsWithoutMutatingThem(t *testing.T) {
	svc, store := newTestService(t)
	first, err := svc.Ingest(context.Background(), IngestInput{
		TenantID: tenantA, SpaceID: spaceTeam, AuthorUserID: userA,
		SourceName: "team-handbook", DetectMissing: true,
		Documents: []Document{
			doc("kept", "h1", "保留", "正文。"),
			doc("gone", "h2", "已删除", "旧正文。"),
		},
	})
	if err != nil || first.Created != 2 {
		t.Fatalf("首次全量扫描失败: report=%+v err=%v", first, err)
	}

	var goneBefore Card
	for _, card := range store.cards {
		if card.SourceKey == "gone" {
			goneBefore = card
		}
	}
	report, err := svc.Ingest(context.Background(), IngestInput{
		TenantID: tenantA, SpaceID: spaceTeam, AuthorUserID: userA,
		SourceName: "team-handbook", DetectMissing: true,
		Documents: []Document{doc("kept", "h1", "保留", "正文。")},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Missing) != 1 || report.Missing[0].CardID != goneBefore.ID || report.Missing[0].SourcePath != "gone.md" {
		t.Fatalf("应只报告本轮消失的源卡，实际 %+v", report.Missing)
	}
	if got := store.cards[goneBefore.ID]; got.Status != goneBefore.Status || got.Version != goneBefore.Version {
		t.Fatalf("缺失检测不得自动废弃或修改卡片，before=%+v after=%+v", goneBefore, got)
	}
}

func TestInvalidButPresentDocumentIsNotReportedMissing(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.Ingest(context.Background(), IngestInput{
		TenantID: tenantA, SpaceID: spaceTeam, AuthorUserID: userA,
		SourceName: "team-handbook", Documents: []Document{doc("present", "h1", "标题", "正文。")},
	})
	if err != nil {
		t.Fatal(err)
	}

	bad := doc("present", "h2", "", "内容暂时写坏了")
	report, err := svc.Ingest(context.Background(), IngestInput{
		TenantID: tenantA, SpaceID: spaceTeam, AuthorUserID: userA,
		SourceName: "team-handbook", DetectMissing: true, Documents: []Document{bad},
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(report.Failed) != 1 || len(report.Missing) != 0 {
		t.Fatalf("存在但内容不合法的文件应进失败清单而非缺失清单，实际 %+v", report)
	}
}

func TestMissingDetectionRequiresStableSourceName(t *testing.T) {
	svc, _ := newTestService(t)
	_, err := svc.Ingest(context.Background(), IngestInput{
		TenantID: tenantA, SpaceID: spaceTeam, AuthorUserID: userA, DetectMissing: true,
	})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("缺失检测没有稳定来源名时必须拒绝，实际 %v", err)
	}
}

func TestIngestRequiresScope(t *testing.T) {
	svc, _ := newTestService(t)

	_, err := svc.Ingest(context.Background(), IngestInput{
		TenantID: tenantA, AuthorUserID: userA,
		Documents: []Document{doc("k", "h", "t", "b")},
	})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("缺少目标空间应报错——作用域字段无默认值，实际: %v", err)
	}
}
