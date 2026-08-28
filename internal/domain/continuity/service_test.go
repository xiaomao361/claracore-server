package continuity

import (
	"context"
	"errors"
	"maps"
	"slices"
	"testing"
	"time"

	"claracore-server/internal/platform"
)

const (
	tenantA = platform.ID("01CONTINUITYTENANTAAAAAAA")
	userA   = platform.ID("01CONTINUITYUSERAAAAAAAAA")
	agentA  = platform.ID("01CONTINUITYAGENTAAAAAAAA")
	userB   = platform.ID("01CONTINUITYUSERBBBBBBBBB")
	agentB  = platform.ID("01CONTINUITYAGENTBBBBBBBB")
)

type fakeStore struct {
	lines     map[platform.ID]Line
	positions map[platform.ID]Position
	history   map[platform.ID]Position
	handoffs  map[platform.ID]Handoff
}

func newFakeStore() *fakeStore {
	return &fakeStore{
		lines: map[platform.ID]Line{}, positions: map[platform.ID]Position{},
		history: map[platform.ID]Position{}, handoffs: map[platform.ID]Handoff{},
	}
}

func (f *fakeStore) ListLines(_ context.Context, q ListQuery) ([]Line, error) {
	var out []Line
	for _, line := range f.lines {
		if line.TenantID != q.TenantID || (!q.AllAgents && line.OwnerAgentID != q.OwnerAgent) || (q.Status != "all" && line.Status != q.Status) {
			continue
		}
		copy := line
		if p, ok := f.positions[line.ID]; ok {
			pCopy := p
			copy.Position = &pCopy
		}
		out = append(out, copy)
	}
	slices.SortFunc(out, func(a, b Line) int {
		at, bt := a.UpdatedAt, b.UpdatedAt
		if a.Position != nil && a.Position.UpdatedAt.After(at) {
			at = a.Position.UpdatedAt
		}
		if b.Position != nil && b.Position.UpdatedAt.After(bt) {
			bt = b.Position.UpdatedAt
		}
		if !at.Equal(bt) {
			if at.After(bt) {
				return -1
			}
			return 1
		}
		if a.ID < b.ID {
			return -1
		}
		if a.ID > b.ID {
			return 1
		}
		return 0
	})
	if q.Limit > 0 && len(out) > q.Limit {
		out = out[:q.Limit]
	}
	return out, nil
}

func (f *fakeStore) FindLine(_ context.Context, tenantID, lineID platform.ID, activeOnly bool) (*Line, error) {
	line, ok := f.lines[lineID]
	if !ok || line.TenantID != tenantID || (activeOnly && line.Status != LineActive) {
		return nil, ErrNotFound
	}
	return &line, nil
}
func (f *fakeStore) CreateLine(_ context.Context, line Line) error {
	f.lines[line.ID] = line
	return nil
}

func (f *fakeStore) UpdateLineStatus(_ context.Context, tenantID, lineID platform.ID, status string, at time.Time) error {
	line, ok := f.lines[lineID]
	if !ok || line.TenantID != tenantID {
		return ErrNotFound
	}
	line.Status, line.UpdatedAt = status, at
	f.lines[lineID] = line
	return nil
}
func (f *fakeStore) FindPosition(_ context.Context, tenantID, lineID platform.ID) (*Position, error) {
	p, ok := f.positions[lineID]
	if !ok || p.TenantID != tenantID {
		return nil, ErrNotFound
	}
	p.FactsUsed = slices.Clone(p.FactsUsed)
	return &p, nil
}
func (f *fakeStore) CreatePosition(_ context.Context, p Position) error {
	if _, exists := f.positions[p.LineID]; exists {
		return errors.New("duplicate position")
	}
	f.positions[p.LineID] = p
	return nil
}
func (f *fakeStore) UpdatePosition(_ context.Context, p Position, expected int) error {
	cur, ok := f.positions[p.LineID]
	if !ok || cur.TenantID != p.TenantID {
		return ErrNotFound
	}
	if cur.Version != expected {
		return &VersionConflictError{Current: cur.Version}
	}
	f.positions[p.LineID] = p
	return nil
}
func (f *fakeStore) AppendPositionHistory(_ context.Context, id platform.ID, p Position, _ time.Time) error {
	f.history[id] = p
	return nil
}
func (f *fakeStore) ListPositionHistory(_ context.Context, tenantID, lineID platform.ID, limit int) ([]Position, error) {
	var out []Position
	for _, p := range f.history {
		if p.TenantID == tenantID && p.LineID == lineID {
			out = append(out, p)
		}
	}
	slices.SortFunc(out, func(a, b Position) int { return b.Version - a.Version })
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
func (f *fakeStore) LatestHandoff(_ context.Context, tenantID, lineID platform.ID) (*Handoff, error) {
	var latest *Handoff
	for _, h := range f.handoffs {
		if h.TenantID != tenantID || h.LineID != lineID {
			continue
		}
		copy := h
		if latest == nil || copy.CreatedAt.After(latest.CreatedAt) {
			latest = &copy
		}
	}
	if latest == nil {
		return nil, ErrNotFound
	}
	return latest, nil
}
func (f *fakeStore) CreateHandoff(_ context.Context, h Handoff) error {
	f.handoffs[h.ID] = h
	return nil
}
func (f *fakeStore) ListHandoffs(_ context.Context, tenantID, lineID platform.ID, limit int) ([]Handoff, error) {
	var out []Handoff
	for _, h := range f.handoffs {
		if h.TenantID == tenantID && h.LineID == lineID {
			out = append(out, h)
		}
	}
	slices.SortFunc(out, func(a, b Handoff) int {
		if a.CreatedAt.After(b.CreatedAt) {
			return -1
		}
		if a.CreatedAt.Before(b.CreatedAt) {
			return 1
		}
		return 0
	})
	if len(out) > limit {
		out = out[:limit]
	}
	return out, nil
}
func (f *fakeStore) Atomically(ctx context.Context, fn func(Store) error) error {
	copy := newFakeStore()
	maps.Copy(copy.lines, f.lines)
	maps.Copy(copy.positions, f.positions)
	maps.Copy(copy.history, f.history)
	maps.Copy(copy.handoffs, f.handoffs)
	if err := fn(copy); err != nil {
		return err
	}
	f.lines, f.positions, f.history, f.handoffs = copy.lines, copy.positions, copy.history, copy.handoffs
	return nil
}

func newService() (*Service, *fakeStore) {
	clock := &platform.OffsetClock{Base: time.Date(2026, 8, 27, 9, 0, 0, 0, time.UTC), Step: time.Minute}
	store := newFakeStore()
	return NewService(store, clock, platform.NewIDGenerator(clock)), store
}

func TestUpdateCreatesOwnedLineAndHistory(t *testing.T) {
	svc, store := newService()
	packet, err := svc.Update(context.Background(), UpdateInput{
		TenantID: tenantA, UserID: userA, AgentID: agentA, Title: "Server 公共记忆库",
		Summary: "空间治理与导入已完成", InterpretationStatus: PositionActive,
		FactsUsed: []platform.ID{"01FACTAAAAAAAAAAAAAAAAAAAA"}, NextStep: "实现 Continuity",
	})
	if err != nil {
		t.Fatal(err)
	}
	if packet.Line.OwnerAgentID != agentA || packet.Line.Position == nil || packet.Line.Position.Version != 1 {
		t.Fatalf("首次 update 应创建 owner 不变的线与当前位置，实际 %+v", packet)
	}
	if len(store.history) != 1 {
		t.Fatal("每次位置写入都应追加历史")
	}
}

func TestGetReturnsExactAndOnlyOwnedActiveLine(t *testing.T) {
	svc, _ := newService()
	created, err := svc.Update(context.Background(), UpdateInput{
		TenantID: tenantA, UserID: userA, AgentID: agentA, Title: "唯一目标", Summary: "可恢复位置",
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, in := range []GetInput{
		{TenantID: tenantA, AgentID: agentA, LineID: created.Line.ID},
		{TenantID: tenantA, AgentID: agentA},
	} {
		packet, err := svc.Get(context.Background(), in)
		if err != nil || packet.Line.ID != created.Line.ID || packet.Line.Position == nil {
			t.Fatalf("get 应返回 exact resume packet，actual=%+v err=%v", packet, err)
		}
	}
}

func TestContinuityValidationBoundaries(t *testing.T) {
	svc, _ := newService()
	if _, err := svc.List(context.Background(), ListInput{}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("list 缺身份应拒绝，实际 %v", err)
	}
	if _, err := svc.List(context.Background(), ListInput{TenantID: tenantA, AgentID: agentA, Status: "deleted"}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("list 非法状态应拒绝，实际 %v", err)
	}

	invalidUpdates := []UpdateInput{
		{TenantID: tenantA, UserID: userA, AgentID: agentA, Title: "目标", Summary: ""},
		{TenantID: tenantA, UserID: userA, AgentID: agentA, Title: "目标", Summary: "x", InterpretationStatus: "unknown"},
		{TenantID: tenantA, UserID: userA, AgentID: agentA, Title: "目标", Summary: "x", FactsUsed: []platform.ID{""}},
	}
	for _, in := range invalidUpdates {
		if _, err := svc.Update(context.Background(), in); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("非法 update 应拒绝，input=%+v err=%v", in, err)
		}
	}
	if _, err := svc.Handoff(context.Background(), HandoffInput{TenantID: tenantA, AgentID: agentA, NextStep: "x"}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("handoff 缺 writer user 应拒绝，实际 %v", err)
	}
	if _, err := svc.Handoff(context.Background(), HandoffInput{TenantID: tenantA, UserID: userA, AgentID: agentA}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("空 handoff 应拒绝，实际 %v", err)
	}

	conflict := &VersionConflictError{Current: 4}
	if !errors.Is(conflict, ErrVersionConflict) || conflict.Error() == "" {
		t.Fatal("VersionConflictError 必须保留哨兵与当前版本")
	}
	required := &LineIDRequiredError{Candidates: []LineSummary{{Title: "一"}, {Title: "二"}}}
	if !errors.Is(required, ErrLineIDRequired) || required.Error() == "" {
		t.Fatal("LineIDRequiredError 必须保留哨兵与候选计数")
	}
}

func TestExplicitCrossAgentUpdateRecordsWriterWithoutTransferringOwner(t *testing.T) {
	svc, _ := newService()
	first, err := svc.Update(context.Background(), UpdateInput{TenantID: tenantA, UserID: userA, AgentID: agentA, Title: "目标", Summary: "第一步"})
	if err != nil {
		t.Fatal(err)
	}
	second, err := svc.Update(context.Background(), UpdateInput{
		TenantID: tenantA, UserID: userB, AgentID: agentB, LineID: first.Line.ID,
		Summary: "第二个 agent 接手", ExpectedVersion: ptr(first.Line.Position.Version),
	})
	if err != nil {
		t.Fatal(err)
	}
	if second.Line.OwnerAgentID != agentA || second.Line.Position.WriterAgentID != agentB {
		t.Fatalf("显式跨 agent 写只能改变 writer，不能转移 owner：%+v", second)
	}
}

func TestAmbiguousOwnedLinesRequireExplicitIDAndDoNotWrite(t *testing.T) {
	svc, store := newService()
	for _, title := range []string{"目标一", "目标二"} {
		line := Line{ID: svc.ids.New(), TenantID: tenantA, OwnerUserID: userA, OwnerAgentID: agentA, Title: title, Status: LineActive, CreatedAt: svc.clock.Now(), UpdatedAt: svc.clock.Now()}
		store.lines[line.ID] = line
	}
	_, err := svc.Update(context.Background(), UpdateInput{TenantID: tenantA, UserID: userA, AgentID: agentA, Title: "不会创建", Summary: "不能猜"})
	var required *LineIDRequiredError
	if !errors.As(err, &required) || len(required.Candidates) != 2 {
		t.Fatalf("应返回有界候选并拒绝猜测，实际 %v", err)
	}
	if len(store.positions) != 0 || len(store.history) != 0 {
		t.Fatal("歧义时不得写任何位置")
	}
}

func TestVersionConflictCarriesCurrentVersion(t *testing.T) {
	svc, _ := newService()
	packet, err := svc.Update(context.Background(), UpdateInput{TenantID: tenantA, UserID: userA, AgentID: agentA, Title: "目标", Summary: "v1"})
	if err != nil {
		t.Fatal(err)
	}
	stale := packet.Line.Position.Version - 1
	_, err = svc.Update(context.Background(), UpdateInput{TenantID: tenantA, UserID: userA, AgentID: agentA, LineID: packet.Line.ID, Summary: "覆盖", ExpectedVersion: &stale})
	var conflict *VersionConflictError
	if !errors.As(err, &conflict) || conflict.Current != 1 {
		t.Fatalf("冲突应带当前版本，实际 %v", err)
	}
}

func TestListDefaultsToOwnerAndCanExplicitlyListTenant(t *testing.T) {
	svc, _ := newService()
	a, _ := svc.Update(context.Background(), UpdateInput{TenantID: tenantA, UserID: userA, AgentID: agentA, Title: "A", Summary: "A"})
	b, _ := svc.Update(context.Background(), UpdateInput{TenantID: tenantA, UserID: userB, AgentID: agentB, Title: "B", Summary: "B"})
	own, err := svc.List(context.Background(), ListInput{TenantID: tenantA, AgentID: agentA})
	if err != nil || len(own) != 1 || own[0].ID != a.Line.ID {
		t.Fatalf("默认只列 owner 的线: %+v err=%v", own, err)
	}
	all, err := svc.List(context.Background(), ListInput{TenantID: tenantA, AgentID: agentA, AllAgents: true})
	if err != nil || len(all) != 2 {
		t.Fatalf("显式 allAgents 应列租户内两条: %+v err=%v", all, err)
	}
	_ = b
}

func TestHandoffReturnsExactLineResumePacket(t *testing.T) {
	svc, _ := newService()
	line, _ := svc.Update(context.Background(), UpdateInput{TenantID: tenantA, UserID: userA, AgentID: agentA, Title: "目标", Summary: "当前位置"})
	packet, err := svc.Handoff(context.Background(), HandoffInput{
		TenantID: tenantA, UserID: userB, AgentID: agentB, LineID: line.Line.ID,
		Objective: "继续目标", Completed: []string{"完成一"}, OpenItems: []string{"待办一"}, NextStep: "下一步",
	})
	if err != nil {
		t.Fatal(err)
	}
	if packet.Line.ID != line.Line.ID || packet.RecentHandoff == nil || packet.RecentHandoff.WriterAgentID != agentB {
		t.Fatalf("交接应返回实际写入线的 resume packet，实际 %+v", packet)
	}
}

func TestLineLifecyclePreservesHistory(t *testing.T) {
	svc, _ := newService()
	created, err := svc.Update(context.Background(), UpdateInput{TenantID: tenantA, UserID: userA, AgentID: agentA, Title: "长期目标", Summary: "第一版"})
	if err != nil {
		t.Fatal(err)
	}
	updated, err := svc.Update(context.Background(), UpdateInput{TenantID: tenantA, UserID: userA, AgentID: agentA, LineID: created.Line.ID, Summary: "第二版"})
	if err != nil {
		t.Fatal(err)
	}
	archived, err := svc.SetStatus(context.Background(), SetStatusInput{TenantID: tenantA, AgentID: agentA, LineID: created.Line.ID, Status: LineArchived})
	if err != nil || archived.Line.Status != LineArchived || len(archived.PositionHistory) != 2 {
		t.Fatalf("archive=%+v err=%v", archived, err)
	}
	active, err := svc.SetStatus(context.Background(), SetStatusInput{TenantID: tenantA, AgentID: agentA, LineID: created.Line.ID, Status: LineActive})
	if err != nil || active.Line.Status != LineActive || active.Line.Position.Version != updated.Line.Position.Version {
		t.Fatalf("activate=%+v err=%v", active, err)
	}
	if _, err := svc.SetStatus(context.Background(), SetStatusInput{TenantID: tenantA, AgentID: agentA, LineID: created.Line.ID, Status: "deleted"}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("非法状态应拒绝: %v", err)
	}
	readArchived, err := svc.Get(context.Background(), GetInput{TenantID: tenantA, AgentID: agentB, LineID: created.Line.ID})
	if err != nil || readArchived.Line.ID != created.Line.ID {
		t.Fatalf("explicit archived line should remain readable with history: packet=%+v err=%v", readArchived, err)
	}
}

func ptr[T any](v T) *T { return &v }
