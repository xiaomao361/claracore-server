//go:build integration

// 摄取的幂等锚点在真库上的行为。
//
// 幂等这件事有一半是数据库的责任（uk_cards_source），领域层的 fake
// 只能模拟它。这里验的是那一半：唯一键真的拦得住、NULL 真的自由、
// 按 source_key 真的查得回来。
package mysql_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"claracore-server/internal/adapters/mysql"
	"claracore-server/internal/domain/knowledge"
	"claracore-server/internal/platform"
)

func ingestedCard(ids *platform.IDGenerator, f kfixture, space platform.ID, sourceKey, sourceHash string, now time.Time) knowledge.Card {
	c := f.card(ids, space, now)
	c.Status = knowledge.CardDraft
	c.CardType = "摄取原料"
	c.Source = sourceKey + ".md"
	c.SourceKey = sourceKey
	c.SourceHash = sourceHash
	// 摄取产物只有原文
	c.Distilled, c.Understanding = "", ""
	c.Original = "一段从文档里扒下来的原文。"
	return c
}

func TestSourceColumnsRoundTrip(t *testing.T) {
	db := openTestDB(t)
	store := mysql.NewKnowledgeStore(db)
	ctx := context.Background()
	ids := platform.NewIDGenerator(platform.SystemClock{})
	now := time.Now().UTC().Truncate(time.Millisecond)
	f := newKFixture(ids)
	cleanupTenant(t, db, f.tenant, f.foreign)

	c := ingestedCard(ids, f, f.spaceMine, "key-1", "hash-1", now)
	if err := store.CreateCard(ctx, c); err != nil {
		t.Fatal(err)
	}

	got, err := store.FindCardBySourceKey(ctx, f.tenant, f.spaceMine, "key-1")
	if err != nil {
		t.Fatalf("按源标识应当查得回来: %v", err)
	}
	if got.ID != c.ID || got.SourceKey != "key-1" || got.SourceHash != "hash-1" {
		t.Fatalf("源标识与内容指纹应原样回读，实际 %+v", got)
	}
	// 认出这是同一份源，靠的就是这两列——它们必须真的落库
	if got.Status != knowledge.CardDraft {
		t.Fatalf("摄取产物应是 draft，实际 %s", got.Status)
	}
}

// TestUniqueKeyBlocksDuplicateSource 验的是幂等真正的那道锁。
//
// 靠应用层 SELECT-then-INSERT 在并发下必然失效，所以这条约束
// 必须由数据库执行——0002 建 spaces 生成列唯一索引时已经写过同一条教训。
func TestUniqueKeyBlocksDuplicateSource(t *testing.T) {
	db := openTestDB(t)
	store := mysql.NewKnowledgeStore(db)
	ctx := context.Background()
	ids := platform.NewIDGenerator(platform.SystemClock{})
	now := time.Now().UTC().Truncate(time.Millisecond)
	f := newKFixture(ids)
	cleanupTenant(t, db, f.tenant, f.foreign)

	first := ingestedCard(ids, f, f.spaceMine, "same-key", "hash-1", now)
	if err := store.CreateCard(ctx, first); err != nil {
		t.Fatal(err)
	}

	second := ingestedCard(ids, f, f.spaceMine, "same-key", "hash-2", now)
	err := store.CreateCard(ctx, second)
	if !errors.Is(err, knowledge.ErrAlreadyExists) {
		t.Fatalf("同空间同源标识必须被数据库拦下，实际: %v", err)
	}

	// 同一个 source_key 在**另一个空间**里是合法的：
	// 唯一键是 (space_id, source_key)，同一份文档可以摄进两个空间
	other := ingestedCard(ids, f, f.spaceTeam, "same-key", "hash-1", now)
	if err := store.CreateCard(ctx, other); err != nil {
		t.Fatalf("另一个空间下的同名源不该被误杀: %v", err)
	}
}

// TestHandwrittenCardsAreFreeOfTheConstraint 是 0005 选 NULL 而非空串的理由。
//
// MySQL 的唯一索引允许多行 NULL，于是同一个约束同时做到两件事：
// 摄取产物被约束，手写卡片完全不受影响。
func TestHandwrittenCardsAreFreeOfTheConstraint(t *testing.T) {
	db := openTestDB(t)
	store := mysql.NewKnowledgeStore(db)
	ctx := context.Background()
	ids := platform.NewIDGenerator(platform.SystemClock{})
	now := time.Now().UTC().Truncate(time.Millisecond)
	f := newKFixture(ids)
	cleanupTenant(t, db, f.tenant, f.foreign)

	// 三张手写卡片（source_key 为空 → 库里是 NULL），同一个空间
	for i := 0; i < 3; i++ {
		c := f.card(ids, f.spaceMine, now)
		if err := store.CreateCard(ctx, c); err != nil {
			t.Fatalf("手写卡片不该受 uk_cards_source 约束（第 %d 张）: %v", i+1, err)
		}
	}

	// 而按空 source_key 去查，必须是 ErrNotFound 而不是随便返回一张
	if _, err := store.FindCardBySourceKey(ctx, f.tenant, f.spaceMine, ""); !errors.Is(err, knowledge.ErrNotFound) {
		t.Fatalf("空源标识应报 ErrNotFound，实际: %v", err)
	}
}

func TestFindBySourceKeyIsScopedToTenantAndSpace(t *testing.T) {
	db := openTestDB(t)
	store := mysql.NewKnowledgeStore(db)
	ctx := context.Background()
	ids := platform.NewIDGenerator(platform.SystemClock{})
	now := time.Now().UTC().Truncate(time.Millisecond)
	f := newKFixture(ids)
	cleanupTenant(t, db, f.tenant, f.foreign)

	c := ingestedCard(ids, f, f.spaceMine, "key-1", "hash-1", now)
	if err := store.CreateCard(ctx, c); err != nil {
		t.Fatal(err)
	}

	// 换个空间查不到
	if _, err := store.FindCardBySourceKey(ctx, f.tenant, f.spaceTeam, "key-1"); !errors.Is(err, knowledge.ErrNotFound) {
		t.Fatalf("跨空间不该查到，实际: %v", err)
	}
	// 换个租户查不到——漏了这个条件就是跨租户泄露
	if _, err := store.FindCardBySourceKey(ctx, f.foreign, f.spaceMine, "key-1"); !errors.Is(err, knowledge.ErrNotFound) {
		t.Fatal("跨租户查到了内容——这是跨租户泄露")
	}
}

// TestDeprecatedCardIsStillRecognized 锁住「不做状态过滤」这条决定。
//
// 一张被人判定为过时的摄取卡，下次重扫仍要被认出来——否则会新建一张，
// 而那个「已经判过这份文档没用」的决定就被悄悄推翻了。
func TestDeprecatedCardIsStillRecognized(t *testing.T) {
	db := openTestDB(t)
	store := mysql.NewKnowledgeStore(db)
	ctx := context.Background()
	ids := platform.NewIDGenerator(platform.SystemClock{})
	now := time.Now().UTC().Truncate(time.Millisecond)
	f := newKFixture(ids)
	cleanupTenant(t, db, f.tenant, f.foreign)

	c := ingestedCard(ids, f, f.spaceMine, "key-1", "hash-1", now)
	c.Status = knowledge.CardDeprecated
	if err := store.CreateCard(ctx, c); err != nil {
		t.Fatal(err)
	}

	got, err := store.FindCardBySourceKey(ctx, f.tenant, f.spaceMine, "key-1")
	if err != nil {
		t.Fatalf("deprecated 的卡仍要能按源标识认出来: %v", err)
	}
	if got.Status != knowledge.CardDeprecated {
		t.Fatalf("状态应原样返回，实际 %s", got.Status)
	}
}
