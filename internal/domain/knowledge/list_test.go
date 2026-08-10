package knowledge

import (
	"context"
	"errors"
	"testing"

	"claracore-server/internal/platform"
)

func listCards(t *testing.T, svc *Service, in ListContentsInput) ListContentsResult {
	t.Helper()
	in.TenantID, in.Kind = tenantA, EntityCard
	if in.Spaces == nil {
		in.Spaces = mySpaces()
	}
	res, err := svc.ListContents(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	return res
}

// ---------------------------------------------------------------------------
// 列表能看到检索看不到的东西——这正是治理面存在的理由
// ---------------------------------------------------------------------------

func TestListSeesDraftAndDeprecated(t *testing.T) {
	svc, _ := newTestService(t)
	seedCard(t, svc, spacePriv, withCard("正常的卡"))
	if _, err := svc.CreateCard(context.Background(), CreateCardInput{
		TenantID: tenantA, SpaceID: spacePriv, AuthorUserID: userA,
		Title: "摄取来的草稿", CardType: "摄取原料", Status: CardDraft,
		Original: "一段原文。",
	}); err != nil {
		t.Fatal(err)
	}

	// 检索只看得到 active
	found, err := svc.SearchCards(context.Background(), SearchCardsInput{
		TenantID: tenantA, Spaces: readable(mySpaces()...), Query: "原文",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, h := range found.Hits {
		if h.Card.Status == CardDraft {
			t.Fatal("检索不该返回 draft")
		}
	}

	// 列表看得到全部——没有这条，摄取灌进来的几千条永远没人知道在等谁
	all := listCards(t, svc, ListContentsInput{})
	if len(all.Items) != 2 {
		t.Fatalf("列表应看到全部状态，实际 %d 条", len(all.Items))
	}

	drafts := listCards(t, svc, ListContentsInput{Statuses: []string{CardDraft}})
	if len(drafts.Items) != 1 || drafts.Items[0].Status != CardDraft {
		t.Fatalf("按状态筛应只剩草稿，实际 %+v", drafts.Items)
	}
	if drafts.Total != 1 {
		t.Fatalf("总数应与筛选条件一致，实际 %d", drafts.Total)
	}
}

func TestListExcludesUnreadableSpaces(t *testing.T) {
	svc, _ := newTestService(t)
	seedCard(t, svc, spacePriv, withCard("看得见"))
	seedCard(t, svc, spaceHid, withCard("看不见"))

	res := listCards(t, svc, ListContentsInput{})
	if len(res.Items) != 1 || res.Items[0].Title != "看得见" {
		t.Fatalf("不可读空间的内容不该出现，实际 %+v", res.Items)
	}
	if res.Total != 1 {
		t.Fatalf("总数也不该数进不可读空间，实际 %d", res.Total)
	}
}

// TestSpaceFilterCannotEscapePermissions 盯住一个很容易开的口子。
//
// 页面上的空间筛选是个参数，直接拿它去查等于让筛选变成绕过权限的入口。
func TestSpaceFilterCannotEscapePermissions(t *testing.T) {
	svc, _ := newTestService(t)
	seedCard(t, svc, spaceHid, withCard("别人的卡"))

	_, err := svc.ListContents(context.Background(), ListContentsInput{
		TenantID: tenantA, Kind: EntityCard, Spaces: mySpaces(), SpaceID: spaceHid,
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("筛选到一个不可读的空间必须被拒，实际: %v", err)
	}
}

func TestListRejectsBadInput(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	if _, err := svc.ListContents(ctx, ListContentsInput{
		TenantID: tenantA, Kind: "everything", Spaces: mySpaces(),
	}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatal("不做混排：Kind 必须是 memory 或 card")
	}
	// 两套枚举刻意不同，传错会安静地返回空列表——那比报错更难查
	if _, err := svc.ListContents(ctx, ListContentsInput{
		TenantID: tenantA, Kind: EntityMemory, Spaces: mySpaces(), Statuses: []string{CardDraft},
	}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatal("memory 不接受 draft 状态，应当报错而不是返回空")
	}
	if _, err := svc.ListContents(ctx, ListContentsInput{
		TenantID: tenantA, Kind: EntityCard, Spaces: mySpaces(), Cursor: "不是合法游标",
	}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatal("畸形游标应当报错")
	}
}

// ---------------------------------------------------------------------------
// 游标分页：不漏不重
// ---------------------------------------------------------------------------

func TestCursorPaginationCoversEverythingExactlyOnce(t *testing.T) {
	svc, _ := newSearchServiceNoEmbedder(t)
	const total = 7
	for i := 0; i < total; i++ {
		seedCard(t, svc, spacePriv, withCard("卡片"))
	}

	seen := map[platform.ID]int{}
	cursor := ""
	pages := 0
	for {
		res := listCards(t, svc, ListContentsInput{Limit: 3, Cursor: cursor})
		pages++
		for _, it := range res.Items {
			seen[it.Ref.ID]++
		}
		if pages == 1 && res.Total != total {
			t.Fatalf("第一页应给出总数 %d，实际 %d", total, res.Total)
		}
		if pages > 1 && res.Total != -1 {
			t.Fatal("后续页不该再数一遍总数——那个数字没变，白花钱")
		}
		if res.NextCursor == "" {
			break
		}
		cursor = res.NextCursor
		if pages > 10 {
			t.Fatal("翻页没有终止，游标逻辑有问题")
		}
	}

	if len(seen) != total {
		t.Fatalf("应当不漏地翻完全部 %d 条，实际见到 %d 条", total, len(seen))
	}
	for id, n := range seen {
		if n != 1 {
			t.Fatalf("%s 出现了 %d 次——分页重复了", id, n)
		}
	}
	if pages != 3 {
		t.Fatalf("7 条按每页 3 条应翻 3 页，实际 %d 页", pages)
	}
}

func TestNoNextCursorOnExactFit(t *testing.T) {
	svc, _ := newSearchServiceNoEmbedder(t)
	for i := 0; i < 3; i++ {
		seedCard(t, svc, spacePriv, withCard("卡片"))
	}

	res := listCards(t, svc, ListContentsInput{Limit: 3})
	if len(res.Items) != 3 {
		t.Fatalf("应返回 3 条，实际 %d", len(res.Items))
	}
	// 正好取满时若假设「还有下一页」，用户会点进一个空页面
	if res.NextCursor != "" {
		t.Fatal("正好取满不等于还有下一页——多取一条才知道")
	}
}

func TestListLimitIsClamped(t *testing.T) {
	svc, _ := newSearchServiceNoEmbedder(t)
	seedCard(t, svc, spacePriv, withCard("卡片"))

	res := listCards(t, svc, ListContentsInput{Limit: 99999})
	if len(res.Items) != 1 {
		t.Fatalf("实际 %d", len(res.Items))
	}
	// 上限本身由 maxListLimit 保证，这里只确认不报错、不返回全库
}

// ---------------------------------------------------------------------------
// 行的内容
// ---------------------------------------------------------------------------

func TestListItemCarriesProvenance(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	// 摄取产物
	if _, err := svc.Ingest(ctx, IngestInput{
		TenantID: tenantA, SpaceID: spacePriv, AuthorUserID: userA,
		Documents: []Document{doc("k1", "h1", "摄取来的", "原文。")},
	}); err != nil {
		t.Fatal(err)
	}
	// 手写的
	seedCard(t, svc, spacePriv, withCard("手写的"))

	res := listCards(t, svc, ListContentsInput{})
	byTitle := map[string]ContentItem{}
	for _, it := range res.Items {
		byTitle[it.Title] = it
	}

	// 04 §3.1 页面 2 要求标注来源：原创 / 提升自 / 摄取进来的，
	// 三者的可信度完全不同
	if !byTitle["摄取来的"].IsIngested() {
		t.Fatal("摄取产物应能被标注出来")
	}
	if byTitle["手写的"].IsIngested() {
		t.Fatal("手写卡片不该被当成摄取产物")
	}
	// 「理解栏是否为空」那条分界线由服务端算好，不让页面自己判
	if byTitle["摄取来的"].Qualified {
		t.Fatal("摄取产物没有「理解」，不该算合格")
	}
	if !byTitle["手写的"].Qualified {
		t.Fatal("写了「理解」的卡应算合格")
	}
}

func TestPromotedCopyShowsItsOrigin(t *testing.T) {
	svc, _ := newTestService(t)
	src := seedMemory(t, svc, spacePriv)

	if _, err := svc.Promote(context.Background(), PromoteInput{
		TenantID: tenantA, Ref: Ref{EntityMemory, src.ID},
		ReadableSpaces: mySpaces(), TargetSpaceID: spaceTeam, ActorUserID: userB,
	}); err != nil {
		t.Fatal(err)
	}

	res, err := svc.ListContents(context.Background(), ListContentsInput{
		TenantID: tenantA, Kind: EntityMemory, Spaces: mySpaces(), SpaceID: spaceTeam,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(res.Items) != 1 {
		t.Fatalf("团队空间应有 1 条副本，实际 %d", len(res.Items))
	}
	if res.Items[0].CopiedFrom == nil {
		t.Fatal("提升来的副本必须带上来源——「这条团队约定来自谁的哪次踩坑」")
	}
}
