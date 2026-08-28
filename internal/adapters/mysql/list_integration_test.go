//go:build integration

// 列表查询在真库上的行为。重点是**游标分页不漏不重**——
// 领域层的 fake 能验排序逻辑，验不了 SQL 的复合比较写对没有。
package mysql_test

import (
	"context"
	"testing"
	"time"

	"claracore-server/internal/adapters/mysql"
	"claracore-server/internal/domain/knowledge"
	"claracore-server/internal/platform"
)

func TestListContentsPushesFiltersIntoSQL(t *testing.T) {
	db := openTestDB(t)
	store := mysql.NewKnowledgeStore(db)
	ctx := context.Background()
	ids := platform.NewIDGenerator(platform.SystemClock{})
	now := time.Now().UTC().Truncate(time.Millisecond)
	f := newKFixture(ids)
	cleanupTenant(t, db, f.tenant, f.foreign)

	mine := f.card(ids, f.spaceMine, now)
	hidden := f.card(ids, f.spaceHid, now)
	foreign := f.card(ids, f.spaceMine, now)
	foreign.TenantID = f.foreign
	draft := f.card(ids, f.spaceMine, now)
	draft.Status = knowledge.CardDraft
	for _, c := range []knowledge.Card{mine, hidden, foreign, draft} {
		if err := store.CreateCard(ctx, c); err != nil {
			t.Fatal(err)
		}
	}

	got, err := store.ListContents(ctx, knowledge.ListQuery{
		TenantID: f.tenant, SpaceIDs: f.visible(), Kind: knowledge.EntityCard, Limit: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 治理面看得到 draft（那正是它存在的理由），但看不到别的空间与别的租户
	if len(got) != 2 {
		t.Fatalf("应看到本租户可见空间下的 2 张（含 draft），实际 %d", len(got))
	}
	for _, it := range got {
		if it.Ref.ID == hidden.ID {
			t.Fatal("返回了不可读空间的内容")
		}
		if it.Ref.ID == foreign.ID {
			t.Fatal("返回了别的租户的内容——跨租户泄露")
		}
	}

	n, err := store.CountContents(ctx, knowledge.ListQuery{
		TenantID: f.tenant, SpaceIDs: f.visible(), Kind: knowledge.EntityCard,
	})
	if err != nil {
		t.Fatal(err)
	}
	// 计数与列表必须用同一份 WHERE，否则「共 N 条」和翻出来的对不上
	if n != 2 {
		t.Fatalf("计数应与列表一致，实际 %d", n)
	}
}

func TestListContentsFullTextFilterKeepsDraftsAndTenantScope(t *testing.T) {
	db := openTestDB(t)
	store := mysql.NewKnowledgeStore(db)
	ctx := context.Background()
	ids := platform.NewIDGenerator(platform.SystemClock{})
	now := time.Now().UTC().Truncate(time.Millisecond)
	f := newKFixture(ids)
	cleanupTenant(t, db, f.tenant, f.foreign)

	active := f.card(ids, f.spaceMine, now)
	active.Title = "部署回滚检查"
	draft := f.card(ids, f.spaceMine, now.Add(time.Millisecond))
	draft.Title, draft.Status = "部署回滚原料", knowledge.CardDraft
	hidden := f.card(ids, f.spaceHid, now)
	hidden.Title = "部署回滚隐藏"
	foreign := f.card(ids, f.spaceMine, now)
	foreign.Title, foreign.TenantID = "部署回滚外租户", f.foreign
	unmatched := f.card(ids, f.spaceMine, now)
	unmatched.Title = "连接池调优"
	for _, card := range []knowledge.Card{active, draft, hidden, foreign, unmatched} {
		if err := store.CreateCard(ctx, card); err != nil {
			t.Fatal(err)
		}
	}

	query := knowledge.ListQuery{TenantID: f.tenant, SpaceIDs: f.visible(), Kind: knowledge.EntityCard, Query: "部署回滚", Limit: 50}
	items, err := store.ListContents(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 2 {
		t.Fatalf("应命中可读范围内 active + draft 两张，实际 %+v", items)
	}
	count, err := store.CountContents(ctx, query)
	if err != nil {
		t.Fatal(err)
	}
	if count != len(items) {
		t.Fatalf("filtered count=%d items=%d", count, len(items))
	}
}

// TestCursorPaginationOnRealDB 验的是 SQL 里那个复合比较。
//
// `updated_at < ? OR (updated_at = ? AND id < ?)` 写错一个分支，
// 表现是翻页时漏行或重复——而那是静默的。
func TestCursorPaginationOnRealDB(t *testing.T) {
	db := openTestDB(t)
	store := mysql.NewKnowledgeStore(db)
	ctx := context.Background()
	ids := platform.NewIDGenerator(platform.SystemClock{})
	base := time.Now().UTC().Truncate(time.Millisecond)
	f := newKFixture(ids)
	cleanupTenant(t, db, f.tenant, f.foreign)

	const total = 7
	// 前三条**共用同一个 updated_at**：那才逼得出「时间相同才比 ID」那条分支
	for i := 0; i < total; i++ {
		c := f.card(ids, f.spaceMine, base)
		if i >= 3 {
			c.UpdatedAt = base.Add(time.Duration(i) * time.Minute)
		}
		if err := store.CreateCard(ctx, c); err != nil {
			t.Fatal(err)
		}
	}

	seen := map[platform.ID]int{}
	q := knowledge.ListQuery{
		TenantID: f.tenant, SpaceIDs: f.visible(), Kind: knowledge.EntityCard, Limit: 3,
	}
	for page := 0; page < 10; page++ {
		items, err := store.ListContents(ctx, q)
		if err != nil {
			t.Fatal(err)
		}
		if len(items) == 0 {
			break
		}
		for _, it := range items {
			seen[it.Ref.ID]++
		}
		last := items[len(items)-1]
		q.AfterUpdatedAt, q.AfterID = last.UpdatedAt, last.Ref.ID
	}

	if len(seen) != total {
		t.Fatalf("应不漏地翻完 %d 条，实际 %d 条", total, len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("%s 出现 %d 次——分页重复", id, n)
		}
	}
}

func TestListCardsCarryQualifiedAndSource(t *testing.T) {
	db := openTestDB(t)
	store := mysql.NewKnowledgeStore(db)
	ctx := context.Background()
	ids := platform.NewIDGenerator(platform.SystemClock{})
	now := time.Now().UTC().Truncate(time.Millisecond)
	f := newKFixture(ids)
	cleanupTenant(t, db, f.tenant, f.foreign)

	qualified := f.card(ids, f.spaceMine, now) // fixture 自带 Understanding
	summary := ingestedCard(ids, f, f.spaceMine, "k1", "h1", now)
	for _, c := range []knowledge.Card{qualified, summary} {
		if err := store.CreateCard(ctx, c); err != nil {
			t.Fatal(err)
		}
	}

	got, err := store.ListContents(ctx, knowledge.ListQuery{
		TenantID: f.tenant, SpaceIDs: f.visible(), Kind: knowledge.EntityCard, Limit: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	byID := map[platform.ID]knowledge.ContentItem{}
	for _, it := range got {
		byID[it.Ref.ID] = it
	}
	// SQL 里的 `body_understanding IS NOT NULL` 必须与 Card.IsQualified() 等价，
	// 也必须与 space_stats 的口径一致——三处说的是同一件事
	if !byID[qualified.ID].Qualified {
		t.Fatal("写了「理解」的卡应算合格")
	}
	if byID[summary.ID].Qualified {
		t.Fatal("摄取产物没有「理解」，不该算合格")
	}
	if !byID[summary.ID].IsIngested() {
		t.Fatal("摄取产物应能被标注出来")
	}
}
