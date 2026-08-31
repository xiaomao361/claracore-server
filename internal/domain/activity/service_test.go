package activity

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"
	"time"

	"claracore-server/internal/platform"
)

type fakeStore struct {
	recorded []Event
	visible  []Event
	admin    []Event

	visibleQueries []VisibleQuery
	adminQueries   []AdminQuery

	deleteBefore  time.Time
	deleteLimit   int
	deleteCount   int
	feedbackQuery ContextFeedbackQuery
	feedback      ContextFeedbackQuality
	sharedQuery   SharedActivityQuery
	sharedPulse   SharedActivityPulse

	err error
}

func (f *fakeStore) Record(_ context.Context, event Event) error {
	if f.err != nil {
		return f.err
	}
	f.recorded = append(f.recorded, event)
	return nil
}

func (f *fakeStore) ListVisible(_ context.Context, query VisibleQuery) ([]Event, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.visibleQueries = append(f.visibleQueries, query)
	return append([]Event(nil), f.visible...), nil
}

func (f *fakeStore) ListAdmin(_ context.Context, query AdminQuery) ([]Event, error) {
	if f.err != nil {
		return nil, f.err
	}
	f.adminQueries = append(f.adminQueries, query)
	return append([]Event(nil), f.admin...), nil
}

func (f *fakeStore) DeleteTracesBefore(_ context.Context, before time.Time, limit int) (int, error) {
	if f.err != nil {
		return 0, f.err
	}
	f.deleteBefore, f.deleteLimit = before, limit
	return f.deleteCount, nil
}

func (f *fakeStore) CountContextFeedback(_ context.Context, query ContextFeedbackQuery) (ContextFeedbackQuality, error) {
	if f.err != nil {
		return ContextFeedbackQuality{}, f.err
	}
	f.feedbackQuery = query
	return f.feedback, nil
}

func (f *fakeStore) CountSharedActivity(_ context.Context, query SharedActivityQuery) (SharedActivityPulse, error) {
	if f.err != nil {
		return SharedActivityPulse{}, f.err
	}
	f.sharedQuery = query
	return f.sharedPulse, nil
}

func newTestService(store Store) (*Service, time.Time) {
	location := time.FixedZone("test-east", 8*60*60)
	now := time.Date(2026, 8, 11, 9, 30, 0, 123456789, location)
	clock := platform.FixedClock{T: now}
	return NewService(store, clock, platform.NewIDGenerator(clock)), now.UTC()
}

func validRecordInput() RecordInput {
	agent := platform.ID(strings.Repeat("A", 26))
	entity := platform.ID(strings.Repeat("B", 26))
	space := platform.ID(strings.Repeat("C", 26))
	return RecordInput{
		TenantID: platform.ID(strings.Repeat("D", 26)), UserID: platform.ID(strings.Repeat("E", 26)), AgentID: &agent,
		Source: SourceMCP, Kind: KindTrace, Access: AccessWrite, Outcome: OutcomeSuccess,
		Action: "card_create", EntityType: "card", EntityID: &entity, SpaceID: &space,
		Metadata: json.RawMessage(` { "version": 1, "semantic": true } `),
	}
}

func TestRecordDropsMalformedOptionalIDsButKeepsAudit(t *testing.T) {
	store := &fakeStore{}
	svc, _ := newTestService(store)
	in := validRecordInput()
	malformed := platform.ID(strings.Repeat("X", 27))
	in.AgentID = &malformed
	in.EntityID = &malformed
	in.SpaceID = &malformed

	event, err := svc.Record(context.Background(), in)
	if err != nil {
		t.Fatalf("可选的不可存储 ID 不应让主审计事件丢失: %v", err)
	}
	if len(store.recorded) != 1 {
		t.Fatalf("主审计事件应仍被写入，实际 %d", len(store.recorded))
	}
	if event.AgentID != nil || event.EntityID != nil || event.EntityType != "" || event.SpaceID != nil {
		t.Fatalf("不可存储的可选 ID 应被降级丢弃: %+v", event)
	}
}

func TestRecordDropsOversizedMetadataButKeepsAudit(t *testing.T) {
	store := &fakeStore{}
	svc, _ := newTestService(store)
	in := validRecordInput()
	in.Metadata, _ = json.Marshal(map[string]string{"value": strings.Repeat("x", MaxMetadataBytes+1)})

	event, err := svc.Record(context.Background(), in)
	if err != nil {
		t.Fatalf("过大 metadata 不应让主审计事件丢失: %v", err)
	}
	if len(store.recorded) != 1 || len(event.Metadata) != 0 {
		t.Fatalf("应保留主事件并丢弃过大 metadata: %+v", event)
	}
}

func TestRecordBuildsValidatedUTCEvent(t *testing.T) {
	store := &fakeStore{}
	svc, wantTime := newTestService(store)
	in := validRecordInput()
	originalAgent := *in.AgentID

	event, err := svc.Record(context.Background(), in)
	if err != nil {
		t.Fatal(err)
	}
	if event.ID == "" {
		t.Fatal("记录必须生成 ID")
	}
	if !event.CreatedAt.Equal(wantTime) || event.CreatedAt.Location() != time.UTC {
		t.Fatalf("时间必须转为 UTC，实际 %v", event.CreatedAt)
	}
	if got := string(event.Metadata); got != `{"version":1,"semantic":true}` {
		t.Fatalf("metadata 应校验并压紧，实际 %s", got)
	}
	if len(store.recorded) != 1 || store.recorded[0].ID != event.ID {
		t.Fatalf("Store 应收到同一事件，实际 %+v", store.recorded)
	}

	// 输入指针与字节切片不能在 Record 返回后反向修改审计证据。
	*in.AgentID = "mutated"
	in.Metadata[0] = '['
	if event.AgentID == nil || *event.AgentID != originalAgent {
		t.Fatal("Event 应复制可选 ID，而不是保留调用方指针")
	}
	if string(event.Metadata) != `{"version":1,"semantic":true}` {
		t.Fatal("Event 应复制 metadata，而不是复用调用方切片")
	}
}

func TestRecordRejectsInvalidInputs(t *testing.T) {
	tests := []struct {
		name   string
		mutate func(*RecordInput)
	}{
		{"缺租户", func(in *RecordInput) { in.TenantID = "" }},
		{"缺用户", func(in *RecordInput) { in.UserID = "" }},
		{"空 agent ID", func(in *RecordInput) { empty := platform.ID(""); in.AgentID = &empty }},
		{"未知来源", func(in *RecordInput) { in.Source = "terminal" }},
		{"未知类型", func(in *RecordInput) { in.Kind = "debug" }},
		{"未知访问类型", func(in *RecordInput) { in.Access = "execute" }},
		{"未知结果", func(in *RecordInput) { in.Outcome = "maybe" }},
		{"缺动作", func(in *RecordInput) { in.Action = "  " }},
		{"动作超长", func(in *RecordInput) { in.Action = strings.Repeat("界", MaxActionLen+1) }},
		{"实体只有类型", func(in *RecordInput) { in.EntityID = nil }},
		{"实体只有 ID", func(in *RecordInput) { in.EntityType = "" }},
		{"实体 ID 为空", func(in *RecordInput) { empty := platform.ID(""); in.EntityID = &empty }},
		{"空间 ID 为空", func(in *RecordInput) { empty := platform.ID(""); in.SpaceID = &empty }},
		{"metadata 非法", func(in *RecordInput) { in.Metadata = json.RawMessage(`{"broken"`) }},
	}

	for _, tc := range tests {
		t.Run(tc.name, func(t *testing.T) {
			store := &fakeStore{}
			svc, _ := newTestService(store)
			in := validRecordInput()
			tc.mutate(&in)
			if _, err := svc.Record(context.Background(), in); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("应返回 ErrInvalidArgument，实际 %v", err)
			}
			if len(store.recorded) != 0 {
				t.Fatal("非法事件不得写入 Store")
			}
		})
	}
}

func TestRecordPropagatesStoreError(t *testing.T) {
	want := errors.New("store unavailable")
	svc, _ := newTestService(&fakeStore{err: want})
	if _, err := svc.Record(context.Background(), validRecordInput()); !errors.Is(err, want) {
		t.Fatalf("应原样返回 Store 错误，实际 %v", err)
	}
}

func TestContextFeedbackQualityBuildsTenantScopedUTCQuery(t *testing.T) {
	store := &fakeStore{feedback: ContextFeedbackQuality{
		Used: 3, Wrong: 1,
		Abstained: 4, AbstainReasons: ContextAbstainReasons{NoRelevantEvidence: 2, Ambiguous: 1, Other: 1},
	}}
	svc, _ := newTestService(store)

	quality, err := svc.ContextFeedbackQuality(context.Background(), "tenant-1", 30*24*time.Hour)
	if err != nil {
		t.Fatal(err)
	}
	if quality.Total() != 4 {
		t.Fatalf("total = %d, want 4", quality.Total())
	}
	if quality.AbstainReasons.Total() != quality.Abstained {
		t.Fatalf("abstain reason histogram must reconcile: %+v", quality)
	}
	wantSince := time.Date(2026, 7, 12, 1, 30, 0, 123456789, time.UTC)
	if store.feedbackQuery.TenantID != "tenant-1" || !store.feedbackQuery.Since.Equal(wantSince) || store.feedbackQuery.Since.Location() != time.UTC {
		t.Fatalf("query should be tenant-scoped and UTC: %+v", store.feedbackQuery)
	}
	if _, err := svc.ContextFeedbackQuality(context.Background(), "", time.Hour); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("missing tenant should fail: %v", err)
	}
	if _, err := svc.ContextFeedbackQuality(context.Background(), "tenant-1", 0); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("missing window should fail: %v", err)
	}
}

func TestSharedActivityPulseBuildsPrivacyScopedCompleteUTCDays(t *testing.T) {
	store := &fakeStore{sharedPulse: SharedActivityPulse{
		Writes: 6, ActiveUsers: 2, ActiveAgents: 1,
		DailyWrites: []DailySharedWrites{
			{Date: time.Date(2026, 8, 6, 0, 0, 0, 0, time.FixedZone("west", -7*60*60)), Writes: 2},
			{Date: time.Date(2026, 8, 11, 0, 0, 0, 0, time.UTC), Writes: 4},
		},
	}}
	svc, _ := newTestService(store)

	pulse, err := svc.SharedActivityPulse(context.Background(), "tenant-1", []platform.ID{"team", "project", "team"}, 7)
	if err != nil {
		t.Fatal(err)
	}
	wantSince := time.Date(2026, 8, 5, 0, 0, 0, 0, time.UTC)
	if store.sharedQuery.TenantID != "tenant-1" || !store.sharedQuery.Since.Equal(wantSince) {
		t.Fatalf("query should be tenant-scoped to seven UTC calendar days: %+v", store.sharedQuery)
	}
	if got := store.sharedQuery.ReadableSharedSpaceIDs; len(got) != 2 || got[0] != "team" || got[1] != "project" {
		t.Fatalf("readable shared spaces should be stably deduplicated: %v", got)
	}
	if len(pulse.DailyWrites) != 7 || pulse.DailyWrites[0].Date != wantSince || pulse.DailyWrites[0].Writes != 0 || pulse.DailyWrites[6].Writes != 4 {
		t.Fatalf("pulse should fill every UTC day without inventing writes: %+v", pulse.DailyWrites)
	}
	// The west-zone midnight belongs to 2026-08-06 UTC after normalization.
	if pulse.DailyWrites[1].Writes != 2 {
		t.Fatalf("store dates must be normalized to UTC: %+v", pulse.DailyWrites)
	}
	if pulse.Writes != 6 || pulse.ActiveUsers != 2 || pulse.ActiveAgents != 1 {
		t.Fatalf("aggregate counts should survive day completion: %+v", pulse)
	}
}

func TestSharedActivityPulseWithNoSharedSpacesDoesNotQueryStore(t *testing.T) {
	store := &fakeStore{}
	svc, _ := newTestService(store)
	pulse, err := svc.SharedActivityPulse(context.Background(), "tenant-1", nil, 7)
	if err != nil {
		t.Fatal(err)
	}
	if store.sharedQuery.TenantID != "" || len(pulse.DailyWrites) != 7 || pulse.Writes != 0 {
		t.Fatalf("empty shared scope should return an explicit zero series without querying storage: query=%+v pulse=%+v", store.sharedQuery, pulse)
	}
	for _, daily := range pulse.DailyWrites {
		if daily.Writes != 0 {
			t.Fatalf("empty shared scope invented activity: %+v", pulse.DailyWrites)
		}
	}
}

func TestSharedActivityPulseRejectsInvalidScopeAndWindow(t *testing.T) {
	svc, _ := newTestService(&fakeStore{})
	for _, tc := range []struct {
		name   string
		tenant platform.ID
		spaces []platform.ID
		days   int
	}{
		{"missing tenant", "", []platform.ID{"team"}, 7},
		{"zero days", "tenant", []platform.ID{"team"}, 0},
		{"unbounded days", "tenant", []platform.ID{"team"}, maxPulseDays + 1},
		{"empty space id", "tenant", []platform.ID{""}, 7},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if _, err := svc.SharedActivityPulse(context.Background(), tc.tenant, tc.spaces, tc.days); !errors.Is(err, ErrInvalidArgument) {
				t.Fatalf("expected ErrInvalidArgument, got %v", err)
			}
		})
	}
}

func TestListVisibleBuildsBoundedKeysetQuery(t *testing.T) {
	base := time.Date(2026, 8, 11, 1, 0, 0, 0, time.UTC)
	store := &fakeStore{visible: []Event{
		{ID: "event-4", CreatedAt: base.Add(4 * time.Minute)},
		{ID: "event-3", CreatedAt: base.Add(3 * time.Minute)},
		{ID: "event-2", CreatedAt: base.Add(2 * time.Minute)},
		{ID: "event-1", CreatedAt: base.Add(time.Minute)},
	}}
	svc, _ := newTestService(store)

	page, err := svc.ListVisible(context.Background(), ListVisibleInput{
		TenantID: "tenant-1", UserID: "user-1",
		ReadableSharedSpaceIDs: []platform.ID{"team", "project", "team"},
		Limit:                  3,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != 3 || page.NextCursor == "" {
		t.Fatalf("多取一条后应返回 3 条与下一页游标，实际 %+v", page)
	}
	q := store.visibleQueries[0]
	if q.Limit != 4 {
		t.Fatalf("Store 应多取一条，实际 limit=%d", q.Limit)
	}
	if len(q.ReadableSharedSpaceIDs) != 2 || q.ReadableSharedSpaceIDs[0] != "team" || q.ReadableSharedSpaceIDs[1] != "project" {
		t.Fatalf("共享空间应稳定去重，实际 %v", q.ReadableSharedSpaceIDs)
	}

	store.visible = nil
	if _, err := svc.ListVisible(context.Background(), ListVisibleInput{
		TenantID: "tenant-1", UserID: "user-1", Cursor: page.NextCursor,
	}); err != nil {
		t.Fatal(err)
	}
	q = store.visibleQueries[1]
	if !q.AfterCreatedAt.Equal(page.Events[2].CreatedAt) || q.AfterID != page.Events[2].ID {
		t.Fatalf("游标应还原最后一条的复合键，实际 %v/%s", q.AfterCreatedAt, q.AfterID)
	}
	if q.Limit != defaultListLimit+1 {
		t.Fatalf("默认页长应为 %d 并多取一条，实际 %d", defaultListLimit, q.Limit)
	}
}

func TestListFiltersAreValidatedAndPassedToStore(t *testing.T) {
	store := &fakeStore{}
	svc, _ := newTestService(store)
	_, err := svc.ListVisible(context.Background(), ListVisibleInput{
		TenantID: "tenant", UserID: "user", Action: " card.update ", Outcome: OutcomeError,
		AgentID: "agent", SpaceID: "space", Since: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	q := store.visibleQueries[0]
	if q.Action != "card.update" || q.Outcome != OutcomeError || q.AgentID != "agent" || q.SpaceID != "space" || q.Since.IsZero() {
		t.Fatalf("筛选应原样下推到可见性查询，实际 %+v", q)
	}
	_, err = svc.ListAdmin(context.Background(), ListAdminInput{
		TenantID: "tenant", Action: "token", Outcome: OutcomeSuccess, AgentID: "agent", SpaceID: "space", Since: time.Date(2026, 8, 1, 0, 0, 0, 0, time.UTC),
	})
	if err != nil {
		t.Fatal(err)
	}
	aq := store.adminQueries[0]
	if aq.Action != "token" || aq.Outcome != OutcomeSuccess || aq.AgentID != "agent" || aq.SpaceID != "space" || aq.Since.IsZero() {
		t.Fatalf("筛选应原样下推到管理员查询，实际 %+v", aq)
	}
	if _, err := svc.ListAdmin(context.Background(), ListAdminInput{TenantID: "tenant", Outcome: "maybe"}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("非法结果筛选应拒绝，实际 %v", err)
	}
}

func TestListVisibleRejectsInvalidScopeAndCursor(t *testing.T) {
	svc, _ := newTestService(&fakeStore{})
	tests := []ListVisibleInput{
		{UserID: "user"},
		{TenantID: "tenant"},
		{TenantID: "tenant", UserID: "user", ReadableSharedSpaceIDs: []platform.ID{""}},
		{TenantID: "tenant", UserID: "user", Cursor: "not-a-cursor"},
	}
	for _, in := range tests {
		if _, err := svc.ListVisible(context.Background(), in); !errors.Is(err, ErrInvalidArgument) {
			t.Fatalf("非法列表参数应被拒，输入 %+v，错误 %v", in, err)
		}
	}
}

func TestListAdminClampsLimitAndUsesCursor(t *testing.T) {
	base := time.Date(2026, 8, 11, 1, 0, 0, 0, time.UTC)
	events := make([]Event, maxListLimit+1)
	for i := range events {
		events[i] = Event{ID: platform.ID("event-" + string(rune('a'+i%26))), CreatedAt: base.Add(-time.Duration(i) * time.Millisecond)}
	}
	store := &fakeStore{admin: events}
	svc, _ := newTestService(store)
	page, err := svc.ListAdmin(context.Background(), ListAdminInput{TenantID: "tenant", Limit: 9999})
	if err != nil {
		t.Fatal(err)
	}
	if len(page.Events) != maxListLimit || page.NextCursor == "" {
		t.Fatalf("管理员列表应卡上限并生成游标，实际 %d/%q", len(page.Events), page.NextCursor)
	}
	if store.adminQueries[0].Limit != maxListLimit+1 {
		t.Fatalf("Store limit 应为上限+1，实际 %d", store.adminQueries[0].Limit)
	}
	if _, err := svc.ListAdmin(context.Background(), ListAdminInput{TenantID: "", Cursor: page.NextCursor}); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("缺租户必须被拒，实际 %v", err)
	}
}

func TestExactFitHasNoNextCursor(t *testing.T) {
	store := &fakeStore{admin: []Event{{ID: "a", CreatedAt: time.Now()}, {ID: "b", CreatedAt: time.Now()}}}
	svc, _ := newTestService(store)
	page, err := svc.ListAdmin(context.Background(), ListAdminInput{TenantID: "tenant", Limit: 2})
	if err != nil {
		t.Fatal(err)
	}
	if page.NextCursor != "" {
		t.Fatal("正好取满不代表还有下一页")
	}
}

func TestDeleteTracesBeforeIsUTCAndBounded(t *testing.T) {
	store := &fakeStore{deleteCount: 7}
	svc, _ := newTestService(store)
	local := time.Date(2026, 7, 12, 9, 0, 0, 0, time.FixedZone("east", 8*60*60))

	n, err := svc.DeleteTracesBefore(context.Background(), local, 0)
	if err != nil || n != 7 {
		t.Fatalf("删除结果不符: n=%d err=%v", n, err)
	}
	if store.deleteBefore.Location() != time.UTC || !store.deleteBefore.Equal(local) {
		t.Fatalf("截止时间必须按 UTC 交给 Store，实际 %v", store.deleteBefore)
	}
	if store.deleteLimit != defaultDeleteLimit {
		t.Fatalf("默认批量应为 %d，实际 %d", defaultDeleteLimit, store.deleteLimit)
	}
	if _, err := svc.DeleteTracesBefore(context.Background(), time.Time{}, 1); !errors.Is(err, ErrInvalidArgument) {
		t.Fatalf("零截止时间必须被拒，实际 %v", err)
	}
	if _, err := svc.DeleteTracesBefore(context.Background(), local, maxDeleteLimit+1); err != nil {
		t.Fatal(err)
	}
	if store.deleteLimit != maxDeleteLimit {
		t.Fatalf("清理批量必须卡上限 %d，实际 %d", maxDeleteLimit, store.deleteLimit)
	}
}
