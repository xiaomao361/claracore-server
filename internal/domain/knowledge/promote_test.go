package knowledge

import (
	"context"
	"errors"
	"slices"
	"testing"

	"claracore-server/internal/platform"
)

// 提升是**复制而非移动**：原件保留在个人空间，两份此后各自演进。
func TestPromoteMemoryCopiesNotMoves(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	src := seedMemory(t, svc, spacePriv, "ops", "mysql")

	ref, err := svc.Promote(ctx, PromoteInput{
		TenantID: tenantA, Ref: Ref{EntityMemory, src.ID},
		ReadableSpaces: mySpaces(), TargetSpaceID: spaceTeam,
		ActorUserID: userB,
	})
	if err != nil {
		t.Fatal(err)
	}
	if ref.ID == src.ID {
		t.Fatal("副本必须是新实体，不能复用原 ID")
	}

	// 原件还在
	orig, _, err := svc.GetMemory(ctx, tenantA, src.ID, mySpaces())
	if err != nil {
		t.Fatalf("原件应保留在源空间: %v", err)
	}
	if orig.SpaceID != spacePriv {
		t.Errorf("原件被移动了: %q", orig.SpaceID)
	}

	copied, _, err := svc.GetMemory(ctx, tenantA, ref.ID, mySpaces())
	if err != nil {
		t.Fatal(err)
	}
	if copied.SpaceID != spaceTeam {
		t.Errorf("副本应落在目标空间，实际 %q", copied.SpaceID)
	}
	if copied.Version != 1 {
		t.Errorf("副本是新实体，version 应为 1，实际 %d", copied.Version)
	}
	if copied.Title != src.Title || copied.Body != src.Body {
		t.Error("副本内容与原件不一致")
	}

	// 来源可追溯：「这条团队约定来自谁的哪次踩坑」
	if copied.CopiedFrom == nil {
		t.Fatal("副本没有记录来源")
	}
	if copied.CopiedFrom.Type != EntityMemory || copied.CopiedFrom.ID != src.ID {
		t.Errorf("来源记录不对: %+v", copied.CopiedFrom)
	}

	// 标签随内容快照
	if !slices.Equal(copied.Labels, []string{"mysql", "ops"}) {
		t.Errorf("标签应随内容快照，实际 %v", copied.Labels)
	}
}

// 副本的作者是**执行提升的人**，不沿用原作者。
// 「谁决定把它摆到团队面前」是一份新的责任，该记在做决定的人头上；
// 「它原本是谁的」由 CopiedFrom 回答。
func TestPromoteRecordsActorAsAuthor(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	src := seedMemory(t, svc, spacePriv) // 作者是 userA

	ref, err := svc.Promote(ctx, PromoteInput{
		TenantID: tenantA, Ref: Ref{EntityMemory, src.ID},
		ReadableSpaces: mySpaces(), TargetSpaceID: spaceTeam,
		ActorUserID: userB, // 提升的是 userB
	})
	if err != nil {
		t.Fatal(err)
	}
	copied, _, err := svc.GetMemory(ctx, tenantA, ref.ID, mySpaces())
	if err != nil {
		t.Fatal(err)
	}
	if copied.AuthorUserID != userB {
		t.Errorf("副本作者应为提升执行者 %s，实际 %s", userB, copied.AuthorUserID)
	}
}

// 副本一律 active，不继承源的生命周期状态：
// 把一条 archived 的记忆搬过去再让它继续隐身，等于这次动作什么也没发生。
func TestPromoteResetsStatusToActive(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	src := seedMemory(t, svc, spacePriv)

	if _, err := svc.UpdateMemory(ctx, UpdateMemoryInput{
		TenantID: tenantA, ID: src.ID, WritableSpaces: mySpaces(),
		Status: ptr(MemoryArchived),
	}); err != nil {
		t.Fatal(err)
	}

	ref, err := svc.Promote(ctx, PromoteInput{
		TenantID: tenantA, Ref: Ref{EntityMemory, src.ID},
		ReadableSpaces: mySpaces(), TargetSpaceID: spaceTeam, ActorUserID: userB,
	})
	if err != nil {
		t.Fatal(err)
	}
	copied, _, err := svc.GetMemory(ctx, tenantA, ref.ID, mySpaces())
	if err != nil {
		t.Fatal(err)
	}
	if copied.Status != MemoryActive {
		t.Errorf("副本应为 active，实际 %q", copied.Status)
	}
}

// 召回条件必须随卡快照。一张丢了召回条件的卡片进了团队空间，
// 就再也不会在该出现的时候出现——这是最沉默的一种失效。
func TestPromoteCardSnapshotsTriggers(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	src := seedCard(t, svc, spacePriv)

	ref, err := svc.Promote(ctx, PromoteInput{
		TenantID: tenantA, Ref: Ref{EntityCard, src.ID},
		ReadableSpaces: mySpaces(), TargetSpaceID: spaceTeam, ActorUserID: userB,
	})
	if err != nil {
		t.Fatal(err)
	}

	copied, _, err := svc.GetCard(ctx, tenantA, ref.ID, mySpaces())
	if err != nil {
		t.Fatal(err)
	}
	if len(copied.RecallTriggers) != len(src.RecallTriggers) {
		t.Fatalf("召回条件应随卡快照：源 %d 条，副本 %d 条",
			len(src.RecallTriggers), len(copied.RecallTriggers))
	}
	for i, tr := range copied.RecallTriggers {
		if tr.Text != src.RecallTriggers[i].Text {
			t.Errorf("第 %d 条召回条件内容不一致", i)
		}
		if tr.CardID != copied.ID {
			t.Errorf("第 %d 条召回条件还挂在原卡上: %q", i, tr.CardID)
		}
		// space_id 必须跟着副本走，否则 card_recall 在目标空间里粗筛不到它
		if tr.SpaceID != spaceTeam {
			t.Errorf("第 %d 条召回条件的 space_id 没跟着副本走: %q", i, tr.SpaceID)
		}
		if tr.ID == src.RecallTriggers[i].ID {
			t.Errorf("第 %d 条召回条件复用了原 ID", i)
		}
	}
	if copied.CopiedFrom == nil || copied.CopiedFrom.ID != src.ID {
		t.Errorf("卡片副本没有记录来源: %+v", copied.CopiedFrom)
	}
	if !slices.Equal(copied.Labels, []string{"codex", "安全"}) {
		t.Errorf("标签应随卡快照，实际 %v", copied.Labels)
	}
}

func TestPromoteDraftCardBecomesActive(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	src := seedCard(t, svc, spacePriv, func(in *CreateCardInput) { in.Status = CardDraft })

	ref, err := svc.Promote(ctx, PromoteInput{
		TenantID: tenantA, Ref: Ref{EntityCard, src.ID},
		ReadableSpaces: mySpaces(), TargetSpaceID: spaceTeam, ActorUserID: userB,
	})
	if err != nil {
		t.Fatal(err)
	}
	copied, _, err := svc.GetCard(ctx, tenantA, ref.ID, mySpaces())
	if err != nil {
		t.Fatal(err)
	}
	// 主动提升一张 draft 卡，说明提升者已经过目并认可它
	if copied.Status != CardActive {
		t.Errorf("副本应为 active，实际 %q", copied.Status)
	}
}

// **两端都校**（2026-08-06 拍板）。只校目标空间可写，就意味着拿到一个 ID
// 就能把自己无权读的内容搬进一个自己能读的空间——一次完整的越权读取，
// 还留下了合法的审计痕迹。
func TestPromoteChecksSourceReadability(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	hiddenMem := seedMemory(t, svc, spaceHid)
	hiddenCard := seedCard(t, svc, spaceHid)

	for name, ref := range map[string]Ref{
		"记忆": {EntityMemory, hiddenMem.ID},
		"卡片": {EntityCard, hiddenCard.ID},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.Promote(ctx, PromoteInput{
				TenantID: tenantA, Ref: ref,
				ReadableSpaces: mySpaces(), TargetSpaceID: spaceTeam, ActorUserID: userA,
			})
			if !errors.Is(err, ErrNotFound) {
				t.Fatalf("源不可读时应报 ErrNotFound，实际 %v", err)
			}
		})
	}
}

// 提升到自己所在的空间会得到两条内容一模一样、只差一个 copied_from 的记录。
// 检索时它们会并排出现，而调用方无从判断该信哪一条。
func TestPromoteRejectsSameSpace(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	m := seedMemory(t, svc, spacePriv)
	c := seedCard(t, svc, spacePriv)

	for name, ref := range map[string]Ref{
		"记忆": {EntityMemory, m.ID},
		"卡片": {EntityCard, c.ID},
	} {
		t.Run(name, func(t *testing.T) {
			_, err := svc.Promote(ctx, PromoteInput{
				TenantID: tenantA, Ref: ref,
				ReadableSpaces: mySpaces(), TargetSpaceID: spacePriv, ActorUserID: userA,
			})
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("同空间提升应被拒绝，实际 %v", err)
			}
		})
	}
}

func TestPromoteRejectsBadInput(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	m := seedMemory(t, svc, spacePriv)
	ok := PromoteInput{
		TenantID: tenantA, Ref: Ref{EntityMemory, m.ID},
		ReadableSpaces: mySpaces(), TargetSpaceID: spaceTeam, ActorUserID: userA,
	}

	cases := map[string]func(*PromoteInput){
		"空 spaces": func(in *PromoteInput) { in.ReadableSpaces = nil },
		"未知实体类型":   func(in *PromoteInput) { in.Ref.Type = "note" },
		"缺目标空间":    func(in *PromoteInput) { in.TargetSpaceID = "" },
		"缺执行者":     func(in *PromoteInput) { in.ActorUserID = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := ok
			mutate(&in)
			if _, err := svc.Promote(ctx, in); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("期望 ErrInvalidArgument，实际 %v", err)
			}
		})
	}
}

// 跨租户提升必须失败：源在别的租户里，就该当作不存在。
func TestPromoteIsolatesTenants(t *testing.T) {
	svc, _ := newTestService(t)
	m := seedMemory(t, svc, spacePriv)

	_, err := svc.Promote(context.Background(), PromoteInput{
		TenantID: tenantB, Ref: Ref{EntityMemory, m.ID},
		ReadableSpaces: mySpaces(), TargetSpaceID: spaceTeam, ActorUserID: userA,
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("跨租户提升应报 ErrNotFound，实际 %v", err)
	}
}

// 提升卡片时若召回条件写入失败，不能留下一张没有召回条件的卡。
func TestPromoteCardRollsBackOnFailure(t *testing.T) {
	svc, store := newTestService(t)
	ctx := context.Background()
	src := seedCard(t, svc, spacePriv)
	before := len(store.cards)

	store.failOn = "ReplaceRecallTriggers"
	_, err := svc.Promote(ctx, PromoteInput{
		TenantID: tenantA, Ref: Ref{EntityCard, src.ID},
		ReadableSpaces: mySpaces(), TargetSpaceID: spaceTeam, ActorUserID: userB,
	})
	if err == nil {
		t.Fatal("召回条件写入失败时提升应当失败")
	}
	if len(store.cards) != before {
		t.Errorf("事务应回滚，卡片数从 %d 变成了 %d", before, len(store.cards))
	}
}

func TestPromoteMemoryRollsBackOnFailure(t *testing.T) {
	svc, store := newTestService(t)
	ctx := context.Background()
	src := seedMemory(t, svc, spacePriv, "ops")
	before := len(store.memories)

	store.failOn = "ReplaceLabels"
	if _, err := svc.Promote(ctx, PromoteInput{
		TenantID: tenantA, Ref: Ref{EntityMemory, src.ID},
		ReadableSpaces: mySpaces(), TargetSpaceID: spaceTeam, ActorUserID: userB,
	}); err == nil {
		t.Fatal("标签写入失败时提升应当失败")
	}
	if len(store.memories) != before {
		t.Errorf("事务应回滚，记忆数从 %d 变成了 %d", before, len(store.memories))
	}
}

// 提升出来的副本可以再被提升（个人 → 项目 → 团队的逐级共享）。
// 此时 copied_from 指向的是**直接来源**而非最初的原件。
func TestPromoteChainRecordsImmediateSource(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	origin := seedMemory(t, svc, spacePriv)

	mid, err := svc.Promote(ctx, PromoteInput{
		TenantID: tenantA, Ref: Ref{EntityMemory, origin.ID},
		ReadableSpaces: []platform.ID{spacePriv, spaceHid}, TargetSpaceID: spaceHid,
		ActorUserID: userA,
	})
	if err != nil {
		t.Fatal(err)
	}
	final, err := svc.Promote(ctx, PromoteInput{
		TenantID: tenantA, Ref: mid,
		ReadableSpaces: []platform.ID{spaceHid, spaceTeam}, TargetSpaceID: spaceTeam,
		ActorUserID: userB,
	})
	if err != nil {
		t.Fatal(err)
	}

	got, _, err := svc.GetMemory(ctx, tenantA, final.ID, mySpaces())
	if err != nil {
		t.Fatal(err)
	}
	if got.CopiedFrom.ID != mid.ID {
		t.Errorf("copied_from 应指向直接来源 %s，实际 %s", mid.ID, got.CopiedFrom.ID)
	}
}
