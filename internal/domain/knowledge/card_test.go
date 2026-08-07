package knowledge

import (
	"context"
	"errors"
	"slices"
	"testing"

	"claracore-server/internal/platform"
)

func seedCard(t *testing.T, svc *Service, space platform.ID, in ...func(*CreateCardInput)) Card {
	t.Helper()
	input := CreateCardInput{
		TenantID:      tenantA,
		SpaceID:       space,
		AuthorUserID:  userA,
		AuthorAgentID: ptr(platform.ID(agentA)),
		Title:         "删除操作前必须先列清单",
		CardType:      "Codex 协作安全",
		Distilled:     "备份报告只给归档路径不给文件清单，等于没有备份。",
		Understanding: "校验和证明的是这个包没坏，不是这个包里有你要的东西。",
		RecallTriggers: []string{
			"准备删除聊天记录、Agent 工作区、Memory 或本地 Git 仓库时",
			"为释放磁盘空间而清理隐藏目录、应用容器或历史工作区时",
		},
		Labels: []string{"codex", "安全"},
	}
	for _, mutate := range in {
		mutate(&input)
	}
	c, err := svc.CreateCard(context.Background(), input)
	if err != nil {
		t.Fatal(err)
	}
	return c
}

// status 按来源分（2026-08-06 拍板）：手写默认 active，摄取显式传 draft。
// 默认值给 active 是因为手写是主路径——最常见的调用不该关心这个字段。
func TestCreateCardStatusDefaultsByOrigin(t *testing.T) {
	svc, _ := newTestService(t)

	handwritten := seedCard(t, svc, spacePriv)
	if handwritten.Status != CardActive {
		t.Errorf("手写卡片应默认 active，实际 %q", handwritten.Status)
	}
	if !handwritten.IsVisible() {
		t.Error("active 卡片应进默认检索")
	}

	ingested := seedCard(t, svc, spacePriv, func(in *CreateCardInput) { in.Status = CardDraft })
	if ingested.Status != CardDraft {
		t.Errorf("摄取卡片应为 draft，实际 %q", ingested.Status)
	}
	if ingested.IsVisible() {
		t.Error("draft 卡片不该进默认检索——摄取产物需人过一遍")
	}
}

// deprecated 不删除：「这条约定曾经成立过」本身是信息。
// 检索时降权而非消失，否则团队会重复踩同一个已被否决的方案。
func TestDeprecatedCardStaysRetrievable(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	c := seedCard(t, svc, spacePriv)

	if _, err := svc.UpdateCard(ctx, UpdateCardInput{
		TenantID: tenantA, ID: c.ID, WritableSpaces: mySpaces(), Status: ptr(CardDeprecated),
	}); err != nil {
		t.Fatal(err)
	}

	got, _, err := svc.GetCard(ctx, tenantA, c.ID, mySpaces())
	if err != nil {
		t.Fatalf("deprecated 卡片仍应能按 ID 取到: %v", err)
	}
	if got.IsVisible() {
		t.Error("deprecated 不该进默认检索")
	}
}

func TestCreateCardRejectsUnknownStatus(t *testing.T) {
	svc, _ := newTestService(t)

	// verifying / confirmed 是被砍掉的旧枚举值。它们必须被拒绝而不是静默写入，
	// 否则库里会出现两个谁也不会去查的状态。
	for _, bad := range []string{"verifying", "confirmed", "ACTIVE", "已确认"} {
		t.Run(bad, func(t *testing.T) {
			_, err := svc.CreateCard(context.Background(), CreateCardInput{
				TenantID: tenantA, SpaceID: spacePriv, AuthorUserID: userA,
				Title: "标题", CardType: "类型", Distilled: "内容", Status: bad,
			})
			if !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("非法状态 %q 应被拒绝，实际 %v", bad, err)
			}
		})
	}
}

// Understanding 是质量分界线，但**不是**写入门槛：
// 摄取进来的存量文档天然没有这一栏，而摄取是第一版的核心能力。
func TestCardQualityIsMeasuredNotEnforced(t *testing.T) {
	svc, _ := newTestService(t)

	summary := seedCard(t, svc, spacePriv, func(in *CreateCardInput) {
		in.Understanding = ""
		in.Status = CardDraft
	})
	if summary.IsQualified() {
		t.Error("没有「理解」一栏的卡片不算合格")
	}

	full := seedCard(t, svc, spacePriv)
	if !full.IsQualified() {
		t.Error("写了「理解」的卡片应当算合格")
	}
}

// 五个 section 全空的卡片只是一个标题，检索时会返回一条什么都没有的结果。
func TestCreateCardRejectsEmptyBody(t *testing.T) {
	svc, _ := newTestService(t)

	_, err := svc.CreateCard(context.Background(), CreateCardInput{
		TenantID: tenantA, SpaceID: spacePriv, AuthorUserID: userA,
		Title: "只有标题", CardType: "类型",
	})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("全空 section 应被拒绝，实际 %v", err)
	}
}

func TestCreateCardRequiresCardType(t *testing.T) {
	svc, _ := newTestService(t)

	_, err := svc.CreateCard(context.Background(), CreateCardInput{
		TenantID: tenantA, SpaceID: spacePriv, AuthorUserID: userA,
		Title: "标题", CardType: "  ", Distilled: "内容",
	})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("缺卡片类型应被拒绝，实际 %v", err)
	}
}

// 触发条件：去重但**保序**。作者写下的先后本身是信息，
// 「最典型的场景」通常排在第一条。
func TestRecallTriggersDedupeButKeepOrder(t *testing.T) {
	svc, store := newTestService(t)

	c := seedCard(t, svc, spacePriv, func(in *CreateCardInput) {
		in.RecallTriggers = []string{"  乙  ", "甲", "乙", "", "丙"}
	})

	got := make([]string, 0, len(c.RecallTriggers))
	for _, tr := range c.RecallTriggers {
		got = append(got, tr.Text)
	}
	if !slices.Equal(got, []string{"乙", "甲", "丙"}) {
		t.Errorf("触发条件应去重保序，实际 %v", got)
	}

	for i, tr := range c.RecallTriggers {
		if tr.SortOrder != i {
			t.Errorf("第 %d 条的 sort_order 是 %d", i, tr.SortOrder)
		}
		// space_id 有意冗余，使 card_recall 粗筛无需 JOIN cards。
		// 忘了填它，粗筛就会漏掉这张卡的全部触发条件。
		if tr.SpaceID != c.SpaceID {
			t.Errorf("触发条件的 space_id 未冗余: %q", tr.SpaceID)
		}
		if tr.CardID != c.ID {
			t.Errorf("触发条件没挂到卡片上: %q", tr.CardID)
		}
	}

	if len(store.triggers[c.ID]) != 3 {
		t.Errorf("triggers 表里应有 3 条，实际 %d", len(store.triggers[c.ID]))
	}
}

func TestRecallTriggerRejectsOverlongText(t *testing.T) {
	svc, _ := newTestService(t)

	_, err := svc.CreateCard(context.Background(), CreateCardInput{
		TenantID: tenantA, SpaceID: spacePriv, AuthorUserID: userA,
		Title: "标题", CardType: "类型", Distilled: "内容",
		RecallTriggers: []string{string(make([]rune, MaxTriggerTextLen+1))},
	})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("超长触发条件应被拒绝，实际 %v", err)
	}
}

// 触发条件是整体替换：作者重排顺序与删掉中间一条，是同一件事。
func TestUpdateCardReplacesTriggers(t *testing.T) {
	svc, store := newTestService(t)
	ctx := context.Background()
	c := seedCard(t, svc, spacePriv)

	if _, err := svc.UpdateCard(ctx, UpdateCardInput{
		TenantID: tenantA, ID: c.ID, WritableSpaces: mySpaces(),
		RecallTriggers: ptr([]string{"只剩这一条"}),
	}); err != nil {
		t.Fatal(err)
	}

	got, _, err := svc.GetCard(ctx, tenantA, c.ID, mySpaces())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.RecallTriggers) != 1 || got.RecallTriggers[0].Text != "只剩这一条" {
		t.Errorf("触发条件应被整体替换，实际 %+v", got.RecallTriggers)
	}

	// 传空切片是有意义但代价很大的动作：这张卡再也不会被 card_recall 命中
	if _, err := svc.UpdateCard(ctx, UpdateCardInput{
		TenantID: tenantA, ID: c.ID, WritableSpaces: mySpaces(),
		RecallTriggers: ptr([]string{}),
	}); err != nil {
		t.Fatal(err)
	}
	if len(store.triggers[c.ID]) != 0 {
		t.Errorf("传空切片应清空触发条件，实际还剩 %d 条", len(store.triggers[c.ID]))
	}
}

// 没传 RecallTriggers 就不该动它们——这是最容易写错的一处：
// 把 nil 当成"空列表"，一次普通的改标题就会把整张卡的召回能力清掉。
func TestUpdateCardKeepsTriggersWhenNotProvided(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	c := seedCard(t, svc, spacePriv)

	if _, err := svc.UpdateCard(ctx, UpdateCardInput{
		TenantID: tenantA, ID: c.ID, WritableSpaces: mySpaces(), Title: ptr("改个标题"),
	}); err != nil {
		t.Fatal(err)
	}

	got, _, err := svc.GetCard(ctx, tenantA, c.ID, mySpaces())
	if err != nil {
		t.Fatal(err)
	}
	if len(got.RecallTriggers) != 2 {
		t.Errorf("没传触发条件却被改了，现在剩 %d 条", len(got.RecallTriggers))
	}
}

func TestUpdateCardDetectsStaleVersion(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	c := seedCard(t, svc, spacePriv)

	if _, err := svc.UpdateCard(ctx, UpdateCardInput{
		TenantID: tenantA, ID: c.ID, WritableSpaces: mySpaces(), Distilled: ptr("别人先改的"),
	}); err != nil {
		t.Fatal(err)
	}
	_, err := svc.UpdateCard(ctx, UpdateCardInput{
		TenantID: tenantA, ID: c.ID, WritableSpaces: mySpaces(),
		ExpectedVersion: ptr(1), Distilled: ptr("我基于旧内容改的"),
	})
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("旧版本号应触发冲突，实际 %v", err)
	}
}

func TestGetCardFiltersBySpace(t *testing.T) {
	svc, _ := newTestService(t)
	hidden := seedCard(t, svc, spaceHid)

	_, _, err := svc.GetCard(context.Background(), tenantA, hidden.ID, mySpaces())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("不可见空间里的卡片应报 ErrNotFound，实际 %v", err)
	}
}

func TestGetCardRejectsEmptySpaces(t *testing.T) {
	svc, _ := newTestService(t)
	c := seedCard(t, svc, spacePriv)

	if _, _, err := svc.GetCard(context.Background(), tenantA, c.ID, nil); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("空空间集合应报 ErrInvalidArgument，实际 %v", err)
	}
	if _, err := svc.UpdateCard(context.Background(), UpdateCardInput{
		TenantID: tenantA, ID: c.ID,
	}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("更新时空空间集合应报 ErrInvalidArgument，实际 %v", err)
	}
}

// 一张没有召回条件的卡片是这个产品最不该产出的东西——
// 它退化成一条普通记忆，却占着"知识卡片"的位置。
func TestCreateCardRollsBackOnTriggerFailure(t *testing.T) {
	svc, store := newTestService(t)
	store.failOn = "ReplaceRecallTriggers"

	_, err := svc.CreateCard(context.Background(), CreateCardInput{
		TenantID: tenantA, SpaceID: spacePriv, AuthorUserID: userA,
		Title: "标题", CardType: "类型", Distilled: "内容",
		RecallTriggers: []string{"某个时机"},
	})
	if err == nil {
		t.Fatal("触发条件写入失败时创建应当失败")
	}
	if len(store.cards) != 0 {
		t.Errorf("事务应回滚，实际残留 %d 张卡片", len(store.cards))
	}
	if len(store.labels) != 0 {
		t.Errorf("事务应回滚，标签表里残留 %d 项", len(store.labels))
	}
}
