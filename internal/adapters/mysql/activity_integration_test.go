//go:build integration

package mysql

import (
	"context"
	"database/sql"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"

	"claracore-server/internal/config"
	"claracore-server/internal/domain/activity"
	"claracore-server/internal/platform"
)

// Activity integration tests use a transaction-backed store so even retention
// DELETEs are rolled back. A global DeleteTracesBefore against the shared dev DB
// would otherwise be capable of deleting legitimate old traces.
func openActivityTestStore(t *testing.T) (*ActivityStore, *sql.Tx) {
	t.Helper()
	wd, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	t.Chdir(filepath.Join(wd, "..", "..", ".."))

	cfg, err := config.Load()
	if err != nil {
		t.Skipf("配置不可用，跳过集成测试: %v", err)
	}
	db, err := Open(context.Background(), cfg.MySQL.DSN(), PoolConfig{
		MaxOpenConns: 2, MaxIdleConns: 1, ConnMaxLifetime: time.Minute,
	})
	if err != nil {
		t.Skipf("数据库不可达，跳过集成测试: %v", err)
	}
	t.Cleanup(func() { db.Close() })

	tx, err := db.BeginTx(context.Background(), nil)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = tx.Rollback() })
	return &ActivityStore{ex: tx}, tx
}

type activityFixture struct {
	tenant, foreign platform.ID
	me, other       platform.ID
	agent           platform.ID
	shared, hidden  platform.ID
	ids             *platform.IDGenerator
}

func newActivityFixture() activityFixture {
	ids := platform.NewIDGenerator(platform.SystemClock{})
	return activityFixture{
		tenant: ids.New(), foreign: ids.New(),
		me: ids.New(), other: ids.New(), agent: ids.New(),
		shared: ids.New(), hidden: ids.New(), ids: ids,
	}
}

func (f activityFixture) event(user platform.ID, at time.Time) activity.Event {
	return activity.Event{
		ID: f.ids.New(), TenantID: f.tenant, UserID: user,
		Source: activity.SourceMCP, Kind: activity.KindTrace,
		Access: activity.AccessRead, Outcome: activity.OutcomeSuccess,
		Action: "card_recall", CreatedAt: at,
	}
}

func recordActivity(t *testing.T, store *ActivityStore, events ...activity.Event) {
	t.Helper()
	for _, event := range events {
		if err := store.Record(context.Background(), event); err != nil {
			t.Fatal(err)
		}
	}
}

func TestActivityVisibleQueryEnforcesPrivacyInSQL(t *testing.T) {
	store, _ := openActivityTestStore(t)
	f := newActivityFixture()
	base := time.Now().UTC().Truncate(time.Millisecond)

	ownRead := f.event(f.me, base.Add(7*time.Minute))
	agent := f.agent
	ownRead.AgentID = &agent
	ownRead.Metadata = json.RawMessage(`{"hits":2}`)

	ownError := f.event(f.me, base.Add(6*time.Minute))
	ownError.Outcome = activity.OutcomeError

	otherSharedWrite := f.event(f.other, base.Add(5*time.Minute))
	otherSharedWrite.Access = activity.AccessWrite
	shared := f.shared
	otherSharedWrite.SpaceID = &shared

	otherSharedRead := f.event(f.other, base.Add(4*time.Minute))
	otherSharedRead.SpaceID = &shared

	otherHiddenWrite := f.event(f.other, base.Add(3*time.Minute))
	otherHiddenWrite.Access = activity.AccessWrite
	hidden := f.hidden
	otherHiddenWrite.SpaceID = &hidden

	otherNoSpaceWrite := f.event(f.other, base.Add(2*time.Minute))
	otherNoSpaceWrite.Access = activity.AccessWrite

	foreignWrite := f.event(f.other, base.Add(time.Minute))
	foreignWrite.TenantID = f.foreign
	foreignWrite.Access = activity.AccessWrite
	foreignWrite.SpaceID = &shared

	recordActivity(t, store, ownRead, ownError, otherSharedWrite, otherSharedRead,
		otherHiddenWrite, otherNoSpaceWrite, foreignWrite)

	got, err := store.ListVisible(context.Background(), activity.VisibleQuery{
		TenantID: f.tenant, UserID: f.me,
		ReadableSharedSpaceIDs: []platform.ID{f.shared}, Limit: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := map[platform.ID]bool{ownRead.ID: true, ownError.ID: true, otherSharedWrite.ID: true}
	if len(got) != len(want) {
		t.Fatalf("应只看到自己的全部记录 + 共享空间里的他人写入，实际 %+v", got)
	}
	for _, event := range got {
		if !want[event.ID] {
			t.Fatalf("返回了不该可见的事件 %s", event.ID)
		}
		if event.CreatedAt.Location() != time.UTC {
			t.Fatalf("回读时间必须是 UTC，实际 %v", event.CreatedAt.Location())
		}
	}
	if got[0].ID != ownRead.ID || got[0].AgentID == nil || *got[0].AgentID != f.agent || string(got[0].Metadata) != `{"hits": 2}` && string(got[0].Metadata) != `{"hits":2}` {
		t.Fatalf("可选字段或 metadata 回读不完整: %+v", got[0])
	}

	// 没有共享空间时仍必须能看自己的全部活动，但看不到任何他人记录。
	ownOnly, err := store.ListVisible(context.Background(), activity.VisibleQuery{
		TenantID: f.tenant, UserID: f.me, Limit: 50,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(ownOnly) != 2 {
		t.Fatalf("空共享空间集合应只返回自己的 2 条，实际 %d", len(ownOnly))
	}
}

func TestActivityAdminAndKeysetPaginationStayTenantScoped(t *testing.T) {
	store, _ := openActivityTestStore(t)
	f := newActivityFixture()
	base := time.Now().UTC().Truncate(time.Millisecond)

	const total = 7
	for i := 0; i < total; i++ {
		event := f.event(f.other, base)
		if i >= 3 {
			event.CreatedAt = base.Add(time.Duration(i) * time.Minute)
		}
		recordActivity(t, store, event)
	}
	foreign := f.event(f.other, base.Add(20*time.Minute))
	foreign.TenantID = f.foreign
	recordActivity(t, store, foreign)

	seen := map[platform.ID]int{}
	query := activity.AdminQuery{TenantID: f.tenant, Limit: 3}
	for page := 0; page < 10; page++ {
		events, err := store.ListAdmin(context.Background(), query)
		if err != nil {
			t.Fatal(err)
		}
		if len(events) == 0 {
			break
		}
		for _, event := range events {
			seen[event.ID]++
			if event.TenantID != f.tenant {
				t.Fatalf("管理员列表泄露了别的租户: %s", event.TenantID)
			}
		}
		last := events[len(events)-1]
		query.AfterCreatedAt, query.AfterID = last.CreatedAt, last.ID
	}
	if len(seen) != total {
		t.Fatalf("应不漏地翻完 %d 条，实际 %d 条", total, len(seen))
	}
	for id, count := range seen {
		if count != 1 {
			t.Fatalf("事件 %s 在 keyset 分页中出现 %d 次", id, count)
		}
	}
}

func TestDeleteTracesBeforeKeepsPermanentAuditKinds(t *testing.T) {
	store, _ := openActivityTestStore(t)
	f := newActivityFixture()
	old := time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
	cutoff := time.Date(2021, 1, 1, 0, 0, 0, 0, time.UTC)
	fresh := time.Date(2022, 1, 1, 0, 0, 0, 0, time.UTC)

	for i := 0; i < 3; i++ {
		recordActivity(t, store, f.event(f.me, old.Add(time.Duration(i)*time.Millisecond)))
	}
	governance := f.event(f.me, old)
	governance.Kind = activity.KindGovernance
	meta := f.event(f.me, old)
	meta.Kind = activity.KindMetaAudit
	freshTrace := f.event(f.me, fresh)
	recordActivity(t, store, governance, meta, freshTrace)

	n, err := store.DeleteTracesBefore(context.Background(), cutoff, 2)
	if err != nil || n != 2 {
		t.Fatalf("第一批应只删 2 条旧 trace，n=%d err=%v", n, err)
	}
	n, err = store.DeleteTracesBefore(context.Background(), cutoff, 2)
	if err != nil || n != 1 {
		t.Fatalf("第二批应删剩余 1 条旧 trace，n=%d err=%v", n, err)
	}

	remaining, err := store.ListAdmin(context.Background(), activity.AdminQuery{TenantID: f.tenant, Limit: 20})
	if err != nil {
		t.Fatal(err)
	}
	want := map[platform.ID]bool{governance.ID: true, meta.ID: true, freshTrace.ID: true}
	if len(remaining) != len(want) {
		t.Fatalf("治理审计、meta-audit 与新 trace 必须保留，实际 %+v", remaining)
	}
	for _, event := range remaining {
		if !want[event.ID] {
			t.Fatalf("保留集合出现意外事件 %s", event.ID)
		}
	}
}
