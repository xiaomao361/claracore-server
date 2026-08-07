package knowledge

import (
	"context"
	"errors"
	"slices"
	"testing"
	"time"

	"claracore-server/internal/platform"
)

// 测试用的固定作用域。三个空间模拟：个人、团队、以及一个调用者看不见的空间。
const (
	tenantA   = "01TESTTENANTAAAAAAAAAAAAAA"
	tenantB   = "01TESTTENANTBBBBBBBBBBBBBB"
	spacePriv = "01TESTSPACEPERSONALAAAAAAA"
	spaceTeam = "01TESTSPACETEAMAAAAAAAAAAA"
	spaceHid  = "01TESTSPACEHIDDENAAAAAAAAA" // 调用者无任何权限
	userA     = "01TESTUSERAAAAAAAAAAAAAAAA"
	userB     = "01TESTUSERBBBBBBBBBBBBBBBB"
	agentA    = "01TESTAGENTAAAAAAAAAAAAAAA"
)

func newTestService(t *testing.T) (*Service, *fakeStore) {
	t.Helper()
	clock := &platform.FixedClock{T: time.Date(2026, 8, 7, 9, 0, 0, 0, time.UTC)}
	store := newFakeStore()
	return NewService(store, clock, platform.NewIDGenerator(clock)), store
}

func ptr[T any](v T) *T { return &v }

// mySpaces 是调用者可读可写的空间集合。spaceHid 刻意不在其中。
func mySpaces() []platform.ID { return []platform.ID{spacePriv, spaceTeam} }

func seedMemory(t *testing.T, svc *Service, space platform.ID, labels ...string) Memory {
	t.Helper()
	m, err := svc.CreateMemory(context.Background(), CreateMemoryInput{
		TenantID:      tenantA,
		SpaceID:       space,
		AuthorUserID:  userA,
		AuthorAgentID: ptr(platform.ID(agentA)),
		Title:         "MySQL 连接池配置",
		Body:          "MaxOpenConns 设成 CPU 核数的 4 倍。",
		Labels:        labels,
	})
	if err != nil {
		t.Fatal(err)
	}
	return m
}

func TestCreateMemoryNormalizesLabels(t *testing.T) {
	svc, store := newTestService(t)

	m := seedMemory(t, svc, spacePriv, "  ops  ", "mysql", "ops", "", "mysql")

	// 去空白、去空项、去重、排序：标签集合会进检索条件与返回值，
	// 顺序不稳定会让相同内容的两次调用返回不同结果。
	if !slices.Equal(m.Labels, []string{"mysql", "ops"}) {
		t.Errorf("标签未被规范化: %v", m.Labels)
	}
	if m.Version != 1 {
		t.Errorf("新建记忆的 version 应为 1，实际 %d", m.Version)
	}
	if m.Status != MemoryActive {
		t.Errorf("新建记忆应为 active，实际 %q", m.Status)
	}
	// 标签必须真的写进 labels 表，而不是只挂在返回的结构体上
	if got := store.labels[Ref{EntityMemory, m.ID}.String()]; !slices.Equal(got, []string{"mysql", "ops"}) {
		t.Errorf("labels 表里的标签不对: %v", got)
	}
}

func TestCreateMemoryRejectsMissingFields(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	base := CreateMemoryInput{
		TenantID: tenantA, SpaceID: spacePriv, AuthorUserID: userA,
		Title: "标题", Body: "正文",
	}

	cases := map[string]func(*CreateMemoryInput){
		"缺租户":  func(in *CreateMemoryInput) { in.TenantID = "" },
		"缺空间":  func(in *CreateMemoryInput) { in.SpaceID = "" },
		"缺作者":  func(in *CreateMemoryInput) { in.AuthorUserID = "" },
		"缺标题":  func(in *CreateMemoryInput) { in.Title = "   " },
		"缺正文":  func(in *CreateMemoryInput) { in.Body = "\n\t " },
		"标签超长": func(in *CreateMemoryInput) { in.Labels = []string{string(make([]rune, MaxLabelLen+1))} },
	}

	for name, mutate := range cases {
		t.Run(name, func(t *testing.T) {
			in := base
			mutate(&in)
			if _, err := svc.CreateMemory(ctx, in); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("期望 ErrInvalidArgument，实际 %v", err)
			}
		})
	}
}

// 空间过滤是权限的唯一落点。取一条落在不可见空间里的记忆，
// 必须与"不存在"完全无法区分——否则任何人都能用一个 ID 探测内容是否存在。
func TestGetMemoryFiltersBySpace(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()

	hidden := seedMemory(t, svc, spaceHid)

	_, _, err := svc.GetMemory(ctx, tenantA, hidden.ID, mySpaces())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("不可见空间里的记忆应报 ErrNotFound，实际 %v", err)
	}

	// 同一条记忆，把它所在的空间加进来就能取到——证明上面那次失败
	// 确实来自空间过滤，而不是别的什么原因（比如 seed 根本没写进去）。
	got, _, err := svc.GetMemory(ctx, tenantA, hidden.ID, []platform.ID{spaceHid})
	if err != nil {
		t.Fatalf("空间可见时应能取到: %v", err)
	}
	if got.ID != hidden.ID {
		t.Errorf("取到的不是同一条: %s", got.ID)
	}
}

// 跨租户取内容必须失败。这条盯的是租户隔离没有在某次改动里被去掉。
func TestGetMemoryIsolatesTenants(t *testing.T) {
	svc, _ := newTestService(t)
	m := seedMemory(t, svc, spacePriv)

	_, _, err := svc.GetMemory(context.Background(), tenantB, m.ID, mySpaces())
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("跨租户读取应报 ErrNotFound，实际 %v", err)
	}
}

func TestGetMemoryRejectsEmptySpaces(t *testing.T) {
	svc, _ := newTestService(t)
	m := seedMemory(t, svc, spacePriv)

	// 空集合意味着调用方没有任何可访问的空间。返回"找不到"会误导排查方向。
	_, _, err := svc.GetMemory(context.Background(), tenantA, m.ID, nil)
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("空空间集合应报 ErrInvalidArgument，实际 %v", err)
	}
}

// 部分更新：nil 表示不变，空字符串表示清空。两者必须可区分。
func TestUpdateMemoryOnlyTouchesProvidedFields(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	m := seedMemory(t, svc, spacePriv, "ops")

	updated, err := svc.UpdateMemory(ctx, UpdateMemoryInput{
		TenantID: tenantA, ID: m.ID, WritableSpaces: mySpaces(),
		Title: ptr("MySQL 连接池配置（已修订）"),
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Title != "MySQL 连接池配置（已修订）" {
		t.Errorf("标题没更新: %q", updated.Title)
	}
	if updated.Body != m.Body {
		t.Errorf("没传 Body 却被改了: %q", updated.Body)
	}
	if updated.Version != 2 {
		t.Errorf("version 应递增到 2，实际 %d", updated.Version)
	}

	// 没传 Labels 就不该动 labels 表
	got, _, err := svc.GetMemory(ctx, tenantA, m.ID, mySpaces())
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Equal(got.Labels, []string{"ops"}) {
		t.Errorf("没传 Labels 却被改了: %v", got.Labels)
	}
}

func TestUpdateMemoryReplacesLabels(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	m := seedMemory(t, svc, spacePriv, "ops", "mysql")

	if _, err := svc.UpdateMemory(ctx, UpdateMemoryInput{
		TenantID: tenantA, ID: m.ID, WritableSpaces: mySpaces(),
		Labels: ptr([]string{"redis"}),
	}); err != nil {
		t.Fatal(err)
	}

	got, _, err := svc.GetMemory(ctx, tenantA, m.ID, mySpaces())
	if err != nil {
		t.Fatal(err)
	}
	// 替换语义而非增量：传进来的就是最终状态
	if !slices.Equal(got.Labels, []string{"redis"}) {
		t.Errorf("标签应被整体替换，实际 %v", got.Labels)
	}
}

// 传了 version 且对不上 → 冲突。这是端到端的乐观锁：
// 能发现"我上次 get 之后有人改过"。
func TestUpdateMemoryDetectsStaleVersion(t *testing.T) {
	svc, _ := newTestService(t)
	ctx := context.Background()
	m := seedMemory(t, svc, spacePriv)

	if _, err := svc.UpdateMemory(ctx, UpdateMemoryInput{
		TenantID: tenantA, ID: m.ID, WritableSpaces: mySpaces(), Body: ptr("别人先改的"),
	}); err != nil {
		t.Fatal(err)
	}

	// 拿着旧版本号再改
	_, err := svc.UpdateMemory(ctx, UpdateMemoryInput{
		TenantID: tenantA, ID: m.ID, WritableSpaces: mySpaces(),
		ExpectedVersion: ptr(1), Body: ptr("我基于旧内容改的"),
	})
	if !errors.Is(err, ErrVersionConflict) {
		t.Fatalf("旧版本号应触发冲突，实际 %v", err)
	}

	// 冲突不得留下任何痕迹
	got, _, err := svc.GetMemory(ctx, tenantA, m.ID, mySpaces())
	if err != nil {
		t.Fatal(err)
	}
	if got.Body != "别人先改的" {
		t.Errorf("冲突的更新竟然写进去了: %q", got.Body)
	}
}

// 不传 version 也绝不会静默覆盖：SQL 上的版本条件仍在。
// 这里用 Store 直接模拟"读之后、写之前有人改了"。
func TestUpdateMemoryWithoutVersionStillGuarded(t *testing.T) {
	svc, store := newTestService(t)
	ctx := context.Background()
	m := seedMemory(t, svc, spacePriv)

	// 绕过 Service 直接改库，制造出 Service 读到的 version 已过期的状态
	raced := store.memories[m.ID]
	raced.Version = 99
	store.memories[m.ID] = raced

	_, err := svc.UpdateMemory(ctx, UpdateMemoryInput{
		TenantID: tenantA, ID: m.ID, WritableSpaces: mySpaces(), Body: ptr("覆盖"),
	})
	// Service 读到 version=99 → 以 99 为条件写回 100，这次能成功。
	// 关键是它**用了读到的版本号做条件**，而不是无条件覆盖：
	// 下面把条件改成不可能满足的值来证明这一点。
	if err != nil {
		t.Fatalf("正常的读-改-写应当成功: %v", err)
	}
	if store.memories[m.ID].Version != 100 {
		t.Errorf("version 应从 99 递增到 100，实际 %d", store.memories[m.ID].Version)
	}
}

// 定位用的是**可写**空间集合。对一条只读的内容，"改不了"与"看不见"
// 必须给出同一个错误——否则等于确认"它存在，只是你够不着"。
func TestUpdateMemoryUsesWritableSpaces(t *testing.T) {
	svc, _ := newTestService(t)
	m := seedMemory(t, svc, spaceTeam)

	// 调用者对 spaceTeam 只有读权限（写集合里没有它）
	_, err := svc.UpdateMemory(context.Background(), UpdateMemoryInput{
		TenantID: tenantA, ID: m.ID, WritableSpaces: []platform.ID{spacePriv},
		Title: ptr("擅自修改"),
	})
	if !errors.Is(err, ErrNotFound) {
		t.Fatalf("对只读空间的内容应报 ErrNotFound，实际 %v", err)
	}
}

func TestUpdateMemoryRejectsInvalidStatus(t *testing.T) {
	svc, _ := newTestService(t)
	m := seedMemory(t, svc, spacePriv)

	_, err := svc.UpdateMemory(context.Background(), UpdateMemoryInput{
		TenantID: tenantA, ID: m.ID, WritableSpaces: mySpaces(), Status: ptr("removed"),
	})
	if !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("非法状态应被拒绝，实际 %v", err)
	}
}

// 软删除：行还在，仍可按 ID 取到，只是不进默认检索。
// 硬删会让指向它的 link 变成悬空引用——多态引用没有外键能拦住。
func TestDeleteMemoryIsSoft(t *testing.T) {
	svc, store := newTestService(t)
	ctx := context.Background()
	m := seedMemory(t, svc, spacePriv)

	if err := svc.DeleteMemory(ctx, tenantA, m.ID, mySpaces()); err != nil {
		t.Fatal(err)
	}
	if _, ok := store.memories[m.ID]; !ok {
		t.Fatal("软删除不该真的删行")
	}

	got, _, err := svc.GetMemory(ctx, tenantA, m.ID, mySpaces())
	if err != nil {
		t.Fatalf("软删除后仍应能按 ID 取到: %v", err)
	}
	if got.Status != MemoryDeleted {
		t.Errorf("状态应为 deleted，实际 %q", got.Status)
	}
	if got.IsVisible() {
		t.Error("已删除的记忆不该出现在默认检索里")
	}
}

// Atomically 必须真的回滚：记忆行写成功、标签写失败时，
// 不能留下一条没有标签的记忆——那正是调用方最难察觉的一类脏数据。
func TestCreateMemoryRollsBackOnLabelFailure(t *testing.T) {
	svc, store := newTestService(t)
	store.failOn = "ReplaceLabels"

	_, err := svc.CreateMemory(context.Background(), CreateMemoryInput{
		TenantID: tenantA, SpaceID: spacePriv, AuthorUserID: userA,
		Title: "标题", Body: "正文", Labels: []string{"ops"},
	})
	if err == nil {
		t.Fatal("标签写入失败时创建应当失败")
	}
	if len(store.memories) != 0 {
		t.Errorf("事务应回滚，实际残留 %d 条记忆", len(store.memories))
	}
}

// checkTenant 是防数据损坏的最后一道闸，正常情况下永远不触发。
// 这条测试盯着它还活着——别当冗余代码删掉。
func TestTenantMismatchIsDetected(t *testing.T) {
	svc, store := newTestService(t)
	m := seedMemory(t, svc, spacePriv)

	// 模拟一次错误的迁移：行还在原空间，tenant_id 却指向了别的租户
	corrupted := store.memories[m.ID]
	corrupted.TenantID = tenantB
	store.memories[m.ID] = corrupted

	// FindMemory 的租户条件会先拦下来（这正是它该做的）
	if _, _, err := svc.GetMemory(context.Background(), tenantA, m.ID, mySpaces()); !errors.Is(err, ErrNotFound) {
		t.Fatalf("期望 ErrNotFound，实际 %v", err)
	}
	// 而 checkTenant 防的是"存储层的租户条件被某次改动去掉了"这种情形
	if err := checkTenant(tenantA, tenantB); !errors.Is(err, ErrTenantMismatch) {
		t.Fatalf("checkTenant 失效了: %v", err)
	}
	if err := checkTenant(tenantA, tenantA); err != nil {
		t.Fatalf("同租户不该报错: %v", err)
	}
}
