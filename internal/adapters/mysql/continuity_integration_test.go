//go:build integration

package mysql_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"claracore-server/internal/adapters/mysql"
	"claracore-server/internal/domain/continuity"
	"claracore-server/internal/platform"
)

func TestContinuityStoreAndServiceIntegration(t *testing.T) {
	db := openTestDB(t)
	ctx := context.Background()
	clock := &platform.OffsetClock{Base: time.Date(2026, 8, 27, 10, 0, 0, 0, time.UTC), Step: time.Millisecond}
	ids := platform.NewIDGenerator(clock)
	tenantID, foreignTenant := ids.New(), ids.New()
	ownerUser, ownerAgent, writerUser, writerAgent := ids.New(), ids.New(), ids.New(), ids.New()

	t.Cleanup(func() {
		for _, cleanupTenant := range []platform.ID{tenantID, foreignTenant} {
			for _, stmt := range []string{
				`DELETE FROM continuity_handoffs WHERE tenant_id = ?`,
				`DELETE FROM continuity_position_history WHERE tenant_id = ?`,
				`DELETE FROM continuity_positions WHERE tenant_id = ?`,
				`DELETE FROM continuity_lines WHERE tenant_id = ?`,
			} {
				if _, err := db.ExecContext(context.Background(), stmt, string(cleanupTenant)); err != nil {
					t.Errorf("清理 Continuity 租户 %s 失败: %v", cleanupTenant, err)
				}
			}
		}
	})

	store := mysql.NewContinuityStore(db)
	svc := continuity.NewService(store, clock, ids)
	created, err := svc.Update(ctx, continuity.UpdateInput{
		TenantID: tenantID, UserID: ownerUser, AgentID: ownerAgent, Title: "公共记忆库落地",
		Summary: "前四项已完成", InterpretationStatus: continuity.PositionActive,
		FactsUsed: []platform.ID{ids.New()}, NextStep: "接入 Shared Line MCP",
	})
	if err != nil {
		t.Fatal(err)
	}
	if created.Line.Position == nil || created.Line.Position.Version != 1 {
		t.Fatalf("首次写入应返回 version=1 的恢复包，实际 %+v", created)
	}

	updated, err := svc.Update(ctx, continuity.UpdateInput{
		TenantID: tenantID, UserID: writerUser, AgentID: writerAgent, LineID: created.Line.ID,
		Summary: "另一个 agent 完成接入", InterpretationStatus: continuity.PositionConfirmed,
		NextStep: "交给本地服务器验证", ExpectedVersion: intPtr(created.Line.Position.Version),
	})
	if err != nil {
		t.Fatal(err)
	}
	if updated.Line.OwnerAgentID != ownerAgent || updated.Line.Position.WriterAgentID != writerAgent {
		t.Fatalf("显式跨 agent 写必须保留 owner 并记录 writer，实际 %+v", updated)
	}

	owned, err := svc.List(ctx, continuity.ListInput{TenantID: tenantID, AgentID: writerAgent})
	if err != nil {
		t.Fatal(err)
	}
	if len(owned) != 0 {
		t.Fatalf("默认列表只看 owner，writer 不应因此拥有 line，实际 %+v", owned)
	}
	all, err := svc.List(ctx, continuity.ListInput{TenantID: tenantID, AgentID: writerAgent, AllAgents: true})
	if err != nil || len(all) != 1 || all[0].ID != created.Line.ID {
		t.Fatalf("allAgents 应看到同租户目录，actual=%+v err=%v", all, err)
	}
	for i, title := range []string{"第二条线", "第三条线"} {
		pageAgent := ids.New()
		positionStatus := []string{continuity.PositionNeedsReview, continuity.PositionStale}[i]
		if _, err := svc.Update(ctx, continuity.UpdateInput{
			TenantID: tenantID, UserID: ownerUser, AgentID: pageAgent, Title: title, Summary: "分页探针", InterpretationStatus: positionStatus,
		}); err != nil {
			t.Fatal(err)
		}
	}
	if _, err := svc.Update(ctx, continuity.UpdateInput{
		TenantID: foreignTenant, UserID: ownerUser, AgentID: ids.New(), Title: "外租户线",
		Summary: "不能进入当前租户统计", InterpretationStatus: continuity.PositionNeedsReview,
	}); err != nil {
		t.Fatal(err)
	}
	firstPage, err := svc.ListPage(ctx, continuity.ListInput{TenantID: tenantID, AgentID: writerAgent, AllAgents: true, Limit: 1})
	if err != nil || len(firstPage.Lines) != 1 || firstPage.NextCursor == "" {
		t.Fatalf("真实 SQL 第一页应有游标，actual=%+v err=%v", firstPage, err)
	}
	secondPage, err := svc.ListPage(ctx, continuity.ListInput{TenantID: tenantID, AgentID: writerAgent, AllAgents: true, Limit: 1, Cursor: firstPage.NextCursor})
	if err != nil || len(secondPage.Lines) != 1 || secondPage.Lines[0].ID == firstPage.Lines[0].ID {
		t.Fatalf("真实 SQL keyset 第二页应推进且不重复，first=%+v second=%+v err=%v", firstPage, secondPage, err)
	}
	lineStats, err := svc.Stats(ctx, tenantID)
	if err != nil || lineStats.Active != 3 || lineStats.Archived != 0 || lineStats.NeedsReview != 1 || lineStats.Stale != 1 {
		t.Fatalf("真实 SQL 共同线统计不符: stats=%+v err=%v", lineStats, err)
	}

	_, err = svc.Update(ctx, continuity.UpdateInput{
		TenantID: tenantID, UserID: ownerUser, AgentID: ownerAgent, LineID: created.Line.ID,
		Summary: "过期写入", ExpectedVersion: intPtr(1),
	})
	var conflict *continuity.VersionConflictError
	if !errors.As(err, &conflict) || conflict.Current != 2 {
		t.Fatalf("真实 SQL 乐观锁应返回 current=2，实际 %v", err)
	}

	packet, err := svc.Handoff(ctx, continuity.HandoffInput{
		TenantID: tenantID, UserID: writerUser, AgentID: writerAgent, LineID: created.Line.ID,
		Objective: "可部署验证", Completed: []string{"MCP 接入", "MySQL 持久化"},
		OpenItems: []string{"人工界面验收"}, NextStep: "运行部署探针",
	})
	if err != nil || packet.RecentHandoff == nil || packet.RecentHandoff.LineID != created.Line.ID {
		t.Fatalf("handoff 应返回 exact line 的恢复包，actual=%+v err=%v", packet, err)
	}

	if _, err := store.FindLine(ctx, foreignTenant, created.Line.ID, true); !errors.Is(err, continuity.ErrNotFound) {
		t.Fatalf("跨租户按 ID 读取必须表现为不存在，实际 %v", err)
	}
	archived, err := svc.SetStatus(ctx, continuity.SetStatusInput{
		TenantID: tenantID, AgentID: ownerAgent, LineID: created.Line.ID, Status: continuity.LineArchived,
	})
	if err != nil || archived.Line.Status != continuity.LineArchived {
		t.Fatalf("archive failed: packet=%+v err=%v", archived, err)
	}
	lineStats, err = svc.Stats(ctx, tenantID)
	if err != nil || lineStats.Active != 2 || lineStats.Archived != 1 {
		t.Fatalf("归档后的真实 SQL 共同线统计不符: stats=%+v err=%v", lineStats, err)
	}
	readArchived, err := svc.Get(ctx, continuity.GetInput{TenantID: tenantID, AgentID: writerAgent, LineID: created.Line.ID})
	if err != nil || len(readArchived.PositionHistory) != 2 || readArchived.RecentHandoff == nil {
		t.Fatalf("explicit archived line should keep complete resume history: packet=%+v err=%v", readArchived, err)
	}

	var historyCount int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM continuity_position_history WHERE tenant_id = ? AND line_id = ?`,
		string(tenantID), string(created.Line.ID)).Scan(&historyCount); err != nil {
		t.Fatal(err)
	}
	if historyCount != 2 {
		t.Fatalf("两次成功位置写入应留下两条追加历史，实际 %d", historyCount)
	}
}

func intPtr(v int) *int { return &v }
