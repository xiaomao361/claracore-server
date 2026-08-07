package knowledge

import (
	"context"
	"errors"
	"testing"

	"claracore-server/internal/platform"
)

func TestCreateLinkAcrossEntityTypes(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	m := seedMemory(t, svc, spacePriv)
	c := seedCard(t, svc, spaceTeam)

	// memory → card 是常见形态：一次踩坑（记忆）演化成一条团队约定（卡片）
	l, err := svc.CreateLink(ctx, LinkInput{
		TenantID: tenantA, ReadableSpaces: mySpaces(),
		From: Ref{EntityMemory, m.ID}, To: Ref{EntityCard, c.ID},
		Kind: LinkEvolvedFrom, Note: "这条约定就是从这次踩坑来的",
	})
	if err != nil {
		t.Fatal(err)
	}

	_, links, err := svc.GetMemory(ctx, tenantA, m.ID, mySpaces())
	if err != nil {
		t.Fatal(err)
	}
	if len(links) != 1 || links[0].ID != l.ID {
		t.Fatalf("取记忆时应带出关联，实际 %+v", links)
	}

	// 同一条关联从对端也能查到——单向存储 + 反向索引，不存两行
	_, cardLinks, err := svc.GetCard(ctx, tenantA, c.ID, mySpaces())
	if err != nil {
		t.Fatal(err)
	}
	if len(cardLinks) != 1 {
		t.Errorf("对端也应查到这条关联，实际 %d 条", len(cardLinks))
	}
}

// 同两个实体之间可以既 related 又 contradicts，那是两条不同的事实。
// 但同 kind 重复创建必须被拦（uk_links 含 kind）。
func TestCreateLinkRejectsDuplicateKind(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	a := seedMemory(t, svc, spacePriv)
	b := seedMemory(t, svc, spacePriv)

	in := LinkInput{
		TenantID: tenantA, ReadableSpaces: mySpaces(),
		From: Ref{EntityMemory, a.ID}, To: Ref{EntityMemory, b.ID}, Kind: LinkRelated,
	}
	if _, err := svc.CreateLink(ctx, in); err != nil {
		t.Fatal(err)
	}
	if _, err := svc.CreateLink(ctx, in); !errors.Is(err, ErrAlreadyExists) {
		t.Fatalf("同 kind 重复创建应报 ErrAlreadyExists，实际 %v", err)
	}

	// 换个 kind 就该成功
	in.Kind = LinkContradicts
	if _, err := svc.CreateLink(ctx, in); err != nil {
		t.Fatalf("不同 kind 应当允许: %v", err)
	}
}

// 自指关联没有任何含义，却会污染每一次「取出关联」的结果，
// 并在图遍历里制造自环。
func TestCreateLinkRejectsSelfReference(t *testing.T) {
	svc, _ := newTestService(t)
	m := seedMemory(t, svc, spacePriv)

	_, err := svc.CreateLink(context.Background(), LinkInput{
		TenantID: tenantA, ReadableSpaces: mySpaces(),
		From: Ref{EntityMemory, m.ID}, To: Ref{EntityMemory, m.ID}, Kind: LinkRelated,
	})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("自指关联应被拒绝，实际 %v", err)
	}
}

func TestCreateLinkRejectsBadInput(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	a := seedMemory(t, svc, spacePriv)
	b := seedMemory(t, svc, spacePriv)
	ok := LinkInput{
		TenantID: tenantA, ReadableSpaces: mySpaces(),
		From: Ref{EntityMemory, a.ID}, To: Ref{EntityMemory, b.ID}, Kind: LinkRelated,
	}

	cases := map[string]func(*LinkInput){
		"未知 kind":     func(in *LinkInput) { in.Kind = "supersedes" },
		"未知实体类型":      func(in *LinkInput) { in.From.Type = "note" },
		"空实体 ID":      func(in *LinkInput) { in.To.ID = "" },
		"缺租户":         func(in *LinkInput) { in.TenantID = "" },
		"空 spaces 集合": func(in *LinkInput) { in.ReadableSpaces = nil },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := ok
			mutate(&in)
			if _, err := svc.CreateLink(ctx, in); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("期望 ErrInvalidArgument，实际 %v", err)
			}
		})
	}
}

// 两端都必须可读——否则可以用一个猜来的 ID 把自己的记忆挂到一条
// 无权访问的内容上，再通过 ListLinks 确认它确实存在。
func TestCreateLinkRequiresBothEndsVisible(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	mine := seedMemory(t, svc, spacePriv)
	hidden := seedMemory(t, svc, spaceHid)

	_, err := svc.CreateLink(ctx, LinkInput{
		TenantID: tenantA, ReadableSpaces: mySpaces(),
		From: Ref{EntityMemory, mine.ID}, To: Ref{EntityMemory, hidden.ID}, Kind: LinkRelated,
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("对端不可见时应报 ErrNotFound，实际 %v", err)
	}

	// 反方向同样要拦：只校 from 不校 to（或反之）是最容易漏的一半
	_, err = svc.CreateLink(ctx, LinkInput{
		TenantID: tenantA, ReadableSpaces: mySpaces(),
		From: Ref{EntityMemory, hidden.ID}, To: Ref{EntityMemory, mine.ID}, Kind: LinkRelated,
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("起点不可见时应报 ErrNotFound，实际 %v", err)
	}
}

// 建关联要求两端**可读**而非可写：关联是一句关于世界的陈述，
// 不修改任何一端的内容。
func TestCreateLinkNeedsOnlyReadAccess(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	mine := seedMemory(t, svc, spacePriv)
	teamCard := seedCard(t, svc, spaceTeam)

	// spaceTeam 只读不可写，仍应能建立关联
	if _, err := svc.CreateLink(ctx, LinkInput{
		TenantID: tenantA, ReadableSpaces: mySpaces(),
		From: Ref{EntityMemory, mine.ID}, To: Ref{EntityCard, teamCard.ID}, Kind: LinkRelated,
	}); err != nil {
		t.Fatalf("只读空间的内容应当可以被关联: %v", err)
	}
}

// 删除幂等：调用方的意图是「让这条关联不存在」，那个状态已经达成了。
func TestDeleteLinkIsIdempotent(t *testing.T) {
	svc, store := newTestService(t)
	ctx := context.Background()
	a := seedMemory(t, svc, spacePriv)
	b := seedMemory(t, svc, spacePriv)

	in := LinkInput{
		TenantID: tenantA, ReadableSpaces: mySpaces(),
		From: Ref{EntityMemory, a.ID}, To: Ref{EntityMemory, b.ID}, Kind: LinkRelated,
	}
	if _, err := svc.CreateLink(ctx, in); err != nil {
		t.Fatal(err)
	}
	if err := svc.DeleteLink(ctx, in); err != nil {
		t.Fatal(err)
	}
	if len(store.links) != 0 {
		t.Fatalf("关联应被删除，实际还剩 %d 条", len(store.links))
	}
	// 再删一次不该报错
	if err := svc.DeleteLink(ctx, in); err != nil {
		t.Fatalf("重复删除应当幂等: %v", err)
	}
}

// 删除同样要求两端可读——否则可以拿一个猜来的 ID 试删，
// 用"报错还是成功"反推那条内容是否存在。
func TestDeleteLinkRequiresVisibility(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	mine := seedMemory(t, svc, spacePriv)
	hidden := seedMemory(t, svc, spaceHid)

	err := svc.DeleteLink(ctx, LinkInput{
		TenantID: tenantA, ReadableSpaces: mySpaces(),
		From: Ref{EntityMemory, mine.ID}, To: Ref{EntityMemory, hidden.ID}, Kind: LinkRelated,
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("对端不可见时应报 ErrNotFound，实际 %v", err)
	}
}

func TestDeleteLinkRejectsBadInput(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	a := seedMemory(t, svc, spacePriv)
	b := seedMemory(t, svc, spacePriv)
	ok := LinkInput{
		TenantID: tenantA, ReadableSpaces: mySpaces(),
		From: Ref{EntityMemory, a.ID}, To: Ref{EntityMemory, b.ID}, Kind: LinkRelated,
	}

	cases := map[string]func(*LinkInput){
		"空 spaces": func(in *LinkInput) { in.ReadableSpaces = nil },
		"未知 kind":  func(in *LinkInput) { in.Kind = "supersedes" },
		"起点类型非法":   func(in *LinkInput) { in.From.Type = "note" },
		"终点 ID 为空": func(in *LinkInput) { in.To.ID = "" },
	}
	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := ok
			mutate(&in)
			if err := svc.DeleteLink(ctx, in); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("期望 ErrInvalidArgument，实际 %v", err)
			}
		})
	}
}

func TestRefValidation(t *testing.T) {
	if err := (Ref{EntityMemory, platform.ID("01X")}).Validate(); err != nil {
		t.Fatalf("合法引用不该报错: %v", err)
	}
	if got := (Ref{EntityCard, "01X"}).String(); got != "card:01X" {
		t.Errorf("Ref.String() = %q", got)
	}
}

// requireVisible 对未知实体类型的兜底。走到这里说明 Ref.Validate 被绕过了，
// 但兜底分支不能变成一次静默放行。
func TestRequireVisibleRejectsUnknownType(t *testing.T) {
	svc, _ := newTestService(t)
	err := svc.requireVisible(context.Background(), tenantA, Ref{"note", "01X"}, mySpaces())
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("期望 ErrInvalidArgument，实际 %v", err)
	}
}
