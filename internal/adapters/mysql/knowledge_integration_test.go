//go:build integration

// 适配器测试：对着真实 MySQL 验证 KnowledgeStore 的行为
// （docs/05-code-structure.md §6.4）。
//
// 基准是领域层的 fakeStore：同一个接口，两个实现，行为必须一致——
// 否则领域单测全绿而线上行为不同，是最难查的一类故障。
//
// 隔离方式：契约用例跑在一个最终 rollback 的事务里。
// 全文检索用例必须真提交（InnoDB 的 FULLTEXT 索引在事务提交时才更新），
// 它自己负责清理。
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

// kfixture 是一套核心域数据：一个租户、三个空间。
//
// 无需建 governance 的表行——核心域的表没有外键（引用完整性由领域层保证），
// 空间 ID 在这里就是三个不透明的值，正好对应 Store 看待它们的方式。
type kfixture struct {
	tenant    platform.ID
	foreign   platform.ID // 另一个租户：少一个 WHERE tenant_id 就会被捞出来
	spaceMine platform.ID
	spaceTeam platform.ID
	spaceHid  platform.ID // 不在可见集合里
	user      platform.ID
	other     platform.ID
	agent     platform.ID
}

func newKFixture(ids *platform.IDGenerator) kfixture {
	return kfixture{
		tenant: ids.New(), foreign: ids.New(),
		spaceMine: ids.New(), spaceTeam: ids.New(), spaceHid: ids.New(),
		user: ids.New(), other: ids.New(), agent: ids.New(),
	}
}

func (f kfixture) visible() []platform.ID { return []platform.ID{f.spaceMine, f.spaceTeam} }

func (f kfixture) memory(ids *platform.IDGenerator, space platform.ID, now time.Time) knowledge.Memory {
	agent := f.agent
	return knowledge.Memory{
		ID: ids.New(), TenantID: f.tenant, SpaceID: space,
		AuthorUserID: f.user, AuthorAgentID: &agent,
		Title: "MySQL 连接池配置", Body: "MaxOpenConns 设成 CPU 核数的 4 倍。",
		Status: knowledge.MemoryActive, Version: 1, CreatedAt: now, UpdatedAt: now,
	}
}

func (f kfixture) card(ids *platform.IDGenerator, space platform.ID, now time.Time) knowledge.Card {
	return knowledge.Card{
		ID: ids.New(), TenantID: f.tenant, SpaceID: space,
		AuthorUserID: f.user,
		Title:        "删除操作前必须先列清单", CardType: "Codex 协作安全",
		Status:        knowledge.CardActive,
		Source:        "0731 复盘",
		Distilled:     "备份报告只给归档路径不给文件清单，等于没有备份。",
		Understanding: "校验和证明的是这个包没坏，不是这个包里有你要的东西。",
		Version:       1, CreatedAt: now, UpdatedAt: now,
	}
}

func TestDeprecateDraftCardsIsAtomicOnMySQL(t *testing.T) {
	db := openTestDB(t)
	store := mysql.NewKnowledgeStore(db)
	ctx := context.Background()
	ids := platform.NewIDGenerator(platform.SystemClock{})
	now := time.Now().UTC().Truncate(time.Millisecond)
	f := newKFixture(ids)
	cleanupTenant(t, db, f.tenant, f.foreign)

	draft := f.card(ids, f.spaceMine, now)
	draft.Title, draft.Status = "待审一", knowledge.CardDraft
	active := f.card(ids, f.spaceMine, now)
	active.Title = "已发布"
	secondDraft := f.card(ids, f.spaceMine, now)
	secondDraft.Title, secondDraft.Status = "待审二", knowledge.CardDraft
	for _, card := range []knowledge.Card{draft, active, secondDraft} {
		if err := store.CreateCard(ctx, card); err != nil {
			t.Fatal(err)
		}
	}
	svc := knowledge.NewService(store, &platform.FixedClock{T: now.Add(time.Minute)}, ids, nil)

	_, err := svc.DeprecateDraftCards(ctx, knowledge.DeprecateDraftCardsInput{
		TenantID: f.tenant, WritableSpaces: f.visible(),
		Cards: []knowledge.DraftCardVersion{{ID: draft.ID, Version: 1}, {ID: active.ID, Version: 1}},
	})
	if !errors.Is(err, knowledge.ErrInvalidArgument) {
		t.Fatalf("混入非草稿应拒绝整批: %v", err)
	}
	rolledBack, err := store.FindCard(ctx, f.tenant, draft.ID, f.visible())
	if err != nil {
		t.Fatal(err)
	}
	if rolledBack.Status != knowledge.CardDraft || rolledBack.Version != 1 {
		t.Fatalf("真实事务未回滚第一张: %+v", rolledBack)
	}

	updated, err := svc.DeprecateDraftCards(ctx, knowledge.DeprecateDraftCardsInput{
		TenantID: f.tenant, WritableSpaces: f.visible(),
		Cards: []knowledge.DraftCardVersion{{ID: draft.ID, Version: 1}, {ID: secondDraft.ID, Version: 1}},
	})
	if err != nil || len(updated) != 2 {
		t.Fatalf("两张草稿应原子更新: updated=%+v err=%v", updated, err)
	}
	for _, id := range []platform.ID{draft.ID, secondDraft.ID} {
		got, err := store.FindCard(ctx, f.tenant, id, f.visible())
		if err != nil {
			t.Fatal(err)
		}
		if got.Status != knowledge.CardDeprecated || got.Version != 2 || !got.UpdatedAt.Equal(now.Add(time.Minute)) {
			t.Fatalf("真实写回不完整: %+v", got)
		}
	}
}

func TestKnowledgeStoreContract(t *testing.T) {
	db := openTestDB(t)
	store := mysql.NewKnowledgeStore(db)
	ctx := context.Background()
	ids := platform.NewIDGenerator(platform.SystemClock{})
	// DATETIME(3) 只有毫秒精度，先截断，否则回读比较必然不等
	now := time.Now().UTC().Truncate(time.Millisecond)

	errRollback := errors.New("契约测试结束，回滚")
	f := newKFixture(ids)

	err := store.Atomically(ctx, func(st knowledge.Store) error {

		t.Run("记忆写入后可原样回读", func(t *testing.T) {
			m := f.memory(ids, f.spaceMine, now)
			m.Labels = []string{"mysql", "ops"}
			if err := st.CreateMemory(ctx, m); err != nil {
				t.Fatal(err)
			}
			if err := st.ReplaceLabels(ctx, f.tenant, knowledge.Ref{Type: knowledge.EntityMemory, ID: m.ID}, m.Labels, now); err != nil {
				t.Fatal(err)
			}

			got, err := st.FindMemory(ctx, f.tenant, m.ID, f.visible())
			if err != nil {
				t.Fatalf("刚写进去的记忆应当能取到: %v", err)
			}
			if got.Title != m.Title || got.Body != m.Body || got.Status != m.Status {
				t.Errorf("标量字段回读不一致: %+v", got)
			}
			if got.AuthorAgentID == nil || *got.AuthorAgentID != f.agent {
				t.Errorf("author_agent_id 回读错误: %v", got.AuthorAgentID)
			}
			if !got.CreatedAt.Equal(now) || got.CreatedAt.Location() != time.UTC {
				t.Errorf("created_at 应当是 UTC 且与写入一致: %v", got.CreatedAt)
			}
			// FindMemory 必须自己把标签填上——memoria_get 必然要带出标签，
			// 留给调用方再查一次只会让每个调用点都写一遍同样的两步。
			if len(got.Labels) != 2 || got.Labels[0] != "mysql" || got.Labels[1] != "ops" {
				t.Errorf("标签未被填充或顺序不对: %v", got.Labels)
			}
			if got.CopiedFrom != nil {
				t.Errorf("非提升产物不该有来源: %+v", got.CopiedFrom)
			}
		})

		// 权限过滤必须下推到 SQL，绝不后置（02-data-model.md §5.1）。
		// 这条盯的就是 `space_id IN (...)` 还在。
		t.Run("空间过滤下推到 SQL", func(t *testing.T) {
			hidden := f.memory(ids, f.spaceHid, now)
			if err := st.CreateMemory(ctx, hidden); err != nil {
				t.Fatal(err)
			}
			if _, err := st.FindMemory(ctx, f.tenant, hidden.ID, f.visible()); !errors.Is(err, knowledge.ErrNotFound) {
				t.Fatalf("不可见空间里的记忆应报 ErrNotFound，实际 %v", err)
			}
			// 把它所在的空间加进来就能取到，证明上面那次失败确实来自空间过滤
			if _, err := st.FindMemory(ctx, f.tenant, hidden.ID, []platform.ID{f.spaceHid}); err != nil {
				t.Fatalf("空间可见时应能取到: %v", err)
			}
		})

		// 漏掉 WHERE tenant_id 就是跨租户泄露。
		t.Run("租户隔离", func(t *testing.T) {
			m := f.memory(ids, f.spaceMine, now)
			if err := st.CreateMemory(ctx, m); err != nil {
				t.Fatal(err)
			}
			if _, err := st.FindMemory(ctx, f.foreign, m.ID, f.visible()); !errors.Is(err, knowledge.ErrNotFound) {
				t.Fatalf("跨租户读取应报 ErrNotFound，实际 %v", err)
			}
		})

		t.Run("空空间集合不构成 SQL 语法错误", func(t *testing.T) {
			m := f.memory(ids, f.spaceMine, now)
			if err := st.CreateMemory(ctx, m); err != nil {
				t.Fatal(err)
			}
			// `IN ()` 是语法错误。必须在拼 SQL 之前拦下来，
			// 而不是让调用方收到一句 MySQL 语法错误。
			if _, err := st.FindMemory(ctx, f.tenant, m.ID, nil); !errors.Is(err, knowledge.ErrNotFound) {
				t.Fatalf("空空间集合应报 ErrNotFound，实际 %v", err)
			}
			if _, err := st.FindCard(ctx, f.tenant, m.ID, nil); !errors.Is(err, knowledge.ErrNotFound) {
				t.Fatalf("空空间集合应报 ErrNotFound，实际 %v", err)
			}
		})

		t.Run("乐观锁：版本不符即冲突", func(t *testing.T) {
			m := f.memory(ids, f.spaceMine, now)
			if err := st.CreateMemory(ctx, m); err != nil {
				t.Fatal(err)
			}

			next := m
			next.Body = "改过一次"
			next.Version = 2
			if err := st.UpdateMemory(ctx, next, 1); err != nil {
				t.Fatalf("版本相符时更新应当成功: %v", err)
			}

			// 拿着已经过期的版本号再改
			stale := m
			stale.Body = "基于旧内容改的"
			stale.Version = 2
			if err := st.UpdateMemory(ctx, stale, 1); !errors.Is(err, knowledge.ErrVersionConflict) {
				t.Fatalf("版本过期应报 ErrVersionConflict，实际 %v", err)
			}

			got, err := st.FindMemory(ctx, f.tenant, m.ID, f.visible())
			if err != nil {
				t.Fatal(err)
			}
			if got.Body != "改过一次" {
				t.Errorf("冲突的更新竟然写进去了: %q", got.Body)
			}
		})

		// 0 行有两种原因，它们给调用方的下一步完全不同：
		// 冲突该重试，不存在该放弃。混成一个错误就把这个判断毁了。
		t.Run("更新不存在的记忆报 ErrNotFound 而非冲突", func(t *testing.T) {
			ghost := f.memory(ids, f.spaceMine, now)
			ghost.Version = 2
			if err := st.UpdateMemory(ctx, ghost, 1); !errors.Is(err, knowledge.ErrNotFound) {
				t.Fatalf("期望 ErrNotFound，实际 %v", err)
			}
		})

		t.Run("卡片写入后可原样回读", func(t *testing.T) {
			c := f.card(ids, f.spaceMine, now)
			c.SourceName = "ops-retrospectives"
			if err := st.CreateCard(ctx, c); err != nil {
				t.Fatal(err)
			}
			triggers := []knowledge.RecallTrigger{
				{ID: ids.New(), TenantID: f.tenant, CardID: c.ID, SpaceID: c.SpaceID,
					Text: "准备删除 Agent 工作区或本地 Git 仓库时", SortOrder: 0, CreatedAt: now},
				{ID: ids.New(), TenantID: f.tenant, CardID: c.ID, SpaceID: c.SpaceID,
					Text: "备份报告只给出归档路径却没有文件清单时", SortOrder: 1, CreatedAt: now},
			}
			if err := st.ReplaceRecallTriggers(ctx, f.tenant, c.ID, triggers); err != nil {
				t.Fatal(err)
			}

			got, err := st.FindCard(ctx, f.tenant, c.ID, f.visible())
			if err != nil {
				t.Fatal(err)
			}
			if got.Title != c.Title || got.CardType != c.CardType || got.Source != c.Source || got.SourceName != c.SourceName {
				t.Errorf("标量字段回读不一致: %+v", got)
			}
			if got.Distilled != c.Distilled || got.Understanding != c.Understanding {
				t.Errorf("section 回读不一致: %+v", got)
			}
			// 空 section 写成 NULL，回读为空串——统一口径，
			// 否则"有多少张卡还只是摘要"会有两个答案
			if got.Original != "" || got.WhyKeep != "" || got.Unresolved != "" {
				t.Errorf("未填写的 section 应回读为空串: %+v", got)
			}
			if !got.IsQualified() {
				t.Error("写了「理解」的卡片应当算合格")
			}
			if len(got.RecallTriggers) != 2 {
				t.Fatalf("召回条件应有 2 条，实际 %d", len(got.RecallTriggers))
			}
			// ORDER BY sort_order：作者写下的先后本身是信息
			if got.RecallTriggers[0].SortOrder != 0 || got.RecallTriggers[1].SortOrder != 1 {
				t.Errorf("召回条件顺序不对: %+v", got.RecallTriggers)
			}
			if got.RecallTriggers[0].SpaceID != c.SpaceID {
				t.Errorf("召回条件的 space_id 冗余列没写对: %q", got.RecallTriggers[0].SpaceID)
			}
		})

		t.Run("来源卡片枚举按租户和空间隔离", func(t *testing.T) {
			const sourceName = "team-handbook-contract"
			wanted := f.card(ids, f.spaceMine, now)
			wanted.SourceName, wanted.SourceKey = sourceName, "wanted-key"
			hidden := f.card(ids, f.spaceHid, now)
			hidden.SourceName, hidden.SourceKey = sourceName, "hidden-key"
			foreign := f.card(ids, f.spaceMine, now)
			foreign.TenantID, foreign.SourceName, foreign.SourceKey = f.foreign, sourceName, "foreign-key"
			for _, card := range []knowledge.Card{wanted, hidden, foreign} {
				if err := st.CreateCard(ctx, card); err != nil {
					t.Fatal(err)
				}
			}
			got, err := st.ListCardsBySource(ctx, f.tenant, f.spaceMine, sourceName)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0].ID != wanted.ID || got[0].SourceKey != wanted.SourceKey {
				t.Fatalf("来源枚举越过了租户或空间边界: %+v", got)
			}
		})

		// 替换语义：传进来的就是最终状态。
		t.Run("标签与召回条件是整体替换", func(t *testing.T) {
			c := f.card(ids, f.spaceMine, now)
			ref := knowledge.Ref{Type: knowledge.EntityCard, ID: c.ID}
			if err := st.CreateCard(ctx, c); err != nil {
				t.Fatal(err)
			}
			if err := st.ReplaceLabels(ctx, f.tenant, ref, []string{"a", "b", "c"}, now); err != nil {
				t.Fatal(err)
			}
			if err := st.ReplaceLabels(ctx, f.tenant, ref, []string{"b"}, now); err != nil {
				t.Fatal(err)
			}
			got, err := st.FindCard(ctx, f.tenant, c.ID, f.visible())
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Labels) != 1 || got.Labels[0] != "b" {
				t.Errorf("标签应被整体替换，实际 %v", got.Labels)
			}

			// 空集合即清空
			if err := st.ReplaceLabels(ctx, f.tenant, ref, nil, now); err != nil {
				t.Fatal(err)
			}
			if err := st.ReplaceRecallTriggers(ctx, f.tenant, c.ID, nil); err != nil {
				t.Fatal(err)
			}
			got, err = st.FindCard(ctx, f.tenant, c.ID, f.visible())
			if err != nil {
				t.Fatal(err)
			}
			if len(got.Labels) != 0 || len(got.RecallTriggers) != 0 {
				t.Errorf("空集合应清空，实际 labels=%v triggers=%d", got.Labels, len(got.RecallTriggers))
			}
		})

		t.Run("提升来源两列一并回读", func(t *testing.T) {
			src := f.memory(ids, f.spaceMine, now)
			if err := st.CreateMemory(ctx, src); err != nil {
				t.Fatal(err)
			}
			copied := f.memory(ids, f.spaceTeam, now)
			copied.AuthorUserID = f.other
			copied.CopiedFrom = &knowledge.Origin{Type: knowledge.EntityMemory, ID: src.ID}
			if err := st.CreateMemory(ctx, copied); err != nil {
				t.Fatal(err)
			}

			got, err := st.FindMemory(ctx, f.tenant, copied.ID, f.visible())
			if err != nil {
				t.Fatal(err)
			}
			if got.CopiedFrom == nil {
				t.Fatal("提升来源丢了")
			}
			if got.CopiedFrom.Type != knowledge.EntityMemory || got.CopiedFrom.ID != src.ID {
				t.Errorf("提升来源回读错误: %+v", got.CopiedFrom)
			}
			if got.AuthorUserID != f.other {
				t.Errorf("副本作者应是提升执行者: %q", got.AuthorUserID)
			}
		})

		t.Run("关联：唯一键含 kind，重复报 ErrAlreadyExists", func(t *testing.T) {
			a := f.memory(ids, f.spaceMine, now)
			b := f.card(ids, f.spaceTeam, now)
			if err := st.CreateMemory(ctx, a); err != nil {
				t.Fatal(err)
			}
			if err := st.CreateCard(ctx, b); err != nil {
				t.Fatal(err)
			}

			from := knowledge.Ref{Type: knowledge.EntityMemory, ID: a.ID}
			to := knowledge.Ref{Type: knowledge.EntityCard, ID: b.ID}
			l := knowledge.Link{
				ID: ids.New(), TenantID: f.tenant, From: from, To: to,
				Kind: knowledge.LinkEvolvedFrom, Note: "这条约定就是从这次踩坑来的", CreatedAt: now,
			}
			if err := st.CreateLink(ctx, l); err != nil {
				t.Fatal(err)
			}

			dup := l
			dup.ID = ids.New()
			if err := st.CreateLink(ctx, dup); !errors.Is(err, knowledge.ErrAlreadyExists) {
				t.Fatalf("同 kind 重复应报 ErrAlreadyExists，实际 %v", err)
			}

			// 同两个实体、不同 kind 是两条不同的事实，不该互相覆盖
			other := l
			other.ID = ids.New()
			other.Kind = knowledge.LinkContradicts
			if err := st.CreateLink(ctx, other); err != nil {
				t.Fatalf("不同 kind 应当允许: %v", err)
			}

			// 单向存储 + 反向索引：从对端也能查到同一条
			fromSide, err := st.ListLinks(ctx, f.tenant, from)
			if err != nil {
				t.Fatal(err)
			}
			toSide, err := st.ListLinks(ctx, f.tenant, to)
			if err != nil {
				t.Fatal(err)
			}
			if len(fromSide) != 2 || len(toSide) != 2 {
				t.Errorf("两端都应查到 2 条，实际 from=%d to=%d", len(fromSide), len(toSide))
			}
			if fromSide[0].Note == "" && fromSide[1].Note == "" {
				t.Error("note 没有回读出来")
			}

			// 删除幂等：调用方的意图是「让这条关联不存在」
			if err := st.DeleteLink(ctx, f.tenant, from, to, knowledge.LinkEvolvedFrom); err != nil {
				t.Fatal(err)
			}
			if err := st.DeleteLink(ctx, f.tenant, from, to, knowledge.LinkEvolvedFrom); err != nil {
				t.Fatalf("重复删除应当幂等: %v", err)
			}
			left, err := st.ListLinks(ctx, f.tenant, from)
			if err != nil {
				t.Fatal(err)
			}
			if len(left) != 1 {
				t.Errorf("只该删掉指定 kind 的那条，实际还剩 %d 条", len(left))
			}
		})

		t.Run("关联的租户隔离", func(t *testing.T) {
			a := f.memory(ids, f.spaceMine, now)
			b := f.memory(ids, f.spaceMine, now)
			if err := st.CreateMemory(ctx, a); err != nil {
				t.Fatal(err)
			}
			if err := st.CreateMemory(ctx, b); err != nil {
				t.Fatal(err)
			}
			from := knowledge.Ref{Type: knowledge.EntityMemory, ID: a.ID}
			to := knowledge.Ref{Type: knowledge.EntityMemory, ID: b.ID}
			if err := st.CreateLink(ctx, knowledge.Link{
				ID: ids.New(), TenantID: f.tenant, From: from, To: to,
				Kind: knowledge.LinkRelated, CreatedAt: now,
			}); err != nil {
				t.Fatal(err)
			}
			got, err := st.ListLinks(ctx, f.foreign, from)
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 0 {
				t.Errorf("跨租户不该查到关联，实际 %d 条", len(got))
			}
		})

		// 嵌套 Atomically 复用当前事务：MySQL 无真嵌套事务，
		// 用 SAVEPOINT 模拟会让「回滚」在内外层含义不同。
		t.Run("嵌套事务复用当前事务", func(t *testing.T) {
			m := f.memory(ids, f.spaceMine, now)
			if err := st.Atomically(ctx, func(inner knowledge.Store) error {
				return inner.CreateMemory(ctx, m)
			}); err != nil {
				t.Fatal(err)
			}
			if _, err := st.FindMemory(ctx, f.tenant, m.ID, f.visible()); err != nil {
				t.Fatalf("嵌套事务写入的记忆应当可见: %v", err)
			}
		})

		t.Run("看板卡片质量统计只按已发布状态计算", func(t *testing.T) {
			statsFixture := newKFixture(ids)
			create := func(status, understanding string, withTrigger bool) {
				card := statsFixture.card(ids, statsFixture.spaceMine, now)
				card.Status, card.Understanding = status, understanding
				if err := st.CreateCard(ctx, card); err != nil {
					t.Fatal(err)
				}
				if withTrigger {
					if err := st.ReplaceRecallTriggers(ctx, statsFixture.tenant, card.ID, []knowledge.RecallTrigger{{
						ID: ids.New(), TenantID: statsFixture.tenant, CardID: card.ID, SpaceID: card.SpaceID,
						Text: "需要这张卡时", CreatedAt: now,
					}}); err != nil {
						t.Fatal(err)
					}
				}
			}
			create(knowledge.CardActive, "完整理解", true)
			create(knowledge.CardActive, "", false)
			create(knowledge.CardDraft, "", true)
			create(knowledge.CardDeprecated, "完整理解", true)

			stats, err := st.CountSpaceContents(ctx, statsFixture.tenant, []platform.ID{statsFixture.spaceMine})
			if err != nil || len(stats) != 1 {
				t.Fatalf("统计失败: stats=%+v err=%v", stats, err)
			}
			got := stats[0]
			if got.Cards != 4 || got.DraftCards != 1 || got.ActiveCards != 2 || got.ActiveUnqualifiedCards != 1 || got.RecallReadyCards != 1 || got.RecallTriggers != 3 {
				t.Fatalf("看板卡片质量口径不符: %+v", got)
			}
		})

		return errRollback
	})

	if !errors.Is(err, errRollback) {
		t.Fatalf("契约测试事务应以约定的错误回滚: %v", err)
	}
}

// TestKnowledgeStoreAtomicallyRollsBack 确认失败的事务真的什么都没留下。
//
// 这条必须在事务外跑：要验证的正是提交与回滚本身。
func TestKnowledgeStoreAtomicallyRollsBack(t *testing.T) {
	db := openTestDB(t)
	store := mysql.NewKnowledgeStore(db)
	ctx := context.Background()
	ids := platform.NewIDGenerator(platform.SystemClock{})
	now := time.Now().UTC().Truncate(time.Millisecond)
	f := newKFixture(ids)

	m := f.memory(ids, f.spaceMine, now)
	boom := errors.New("写到一半炸了")

	err := store.Atomically(ctx, func(st knowledge.Store) error {
		if err := st.CreateMemory(ctx, m); err != nil {
			return err
		}
		return boom
	})
	// fn 的错误必须原样返回，领域层靠 errors.Is 认哨兵错误
	if !errors.Is(err, boom) {
		t.Fatalf("fn 的错误应当原样返回，实际 %v", err)
	}

	if _, err := store.FindMemory(ctx, f.tenant, m.ID, f.visible()); !errors.Is(err, knowledge.ErrNotFound) {
		t.Fatalf("事务失败后不该留下任何行，实际 %v", err)
	}
}

// TestChineseSearchWorksImmediatelyAfterCommit 验证用户真正依赖的保证：
// 中文内容刚提交后立刻可搜。FULLTEXT 的 ngram 结构由 schema 集成测试检查；
// 这里走正式 Store 路径，同时覆盖 InnoDB FTS 刚提交后短暂不可见时的字面兜底。
func TestChineseSearchWorksImmediatelyAfterCommit(t *testing.T) {
	db := openTestDB(t)
	store := mysql.NewKnowledgeStore(db)
	ctx := context.Background()
	ids := platform.NewIDGenerator(platform.SystemClock{})
	now := time.Now().UTC().Truncate(time.Millisecond)
	f := newKFixture(ids)

	m := f.memory(ids, f.spaceMine, now)
	m.Title = "数据库连接池调优笔记"
	m.Body = "把最大连接数从两百降到四十之后，超时反而消失了。"

	if err := store.CreateMemory(ctx, m); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if _, err := db.ExecContext(ctx, `DELETE FROM memories WHERE id = ?`, string(m.ID)); err != nil {
			t.Errorf("清理失败，库里留下了测试数据 %s: %v", m.ID, err)
		}
	})

	// 三个词都在中文句子的中间，没有空格分隔。
	for _, keyword := range []string{"连接池", "调优", "超时"} {
		t.Run(keyword, func(t *testing.T) {
			got, err := store.SearchMemories(ctx, knowledge.CandidateQuery{
				TenantID: f.tenant, SpaceIDs: f.visible(), Query: keyword, Limit: 10,
			})
			if err != nil {
				t.Fatal(err)
			}
			if len(got) != 1 || got[0].Memory.ID != m.ID || got[0].TextScore <= 0 {
				t.Fatalf("中文关键词 %q 刚提交后不可搜: %+v", keyword, got)
			}
		})
	}
}
