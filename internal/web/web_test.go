package web

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"html"
	"io"
	"log/slog"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"claracore-server/internal/domain/activity"
	"claracore-server/internal/domain/continuity"
	"claracore-server/internal/domain/external"
	"claracore-server/internal/domain/governance"
	"claracore-server/internal/domain/knowledge"
	"claracore-server/internal/platform"
	"claracore-server/internal/ports"
)

type fakeBackupStatusReader struct {
	status ports.BackupStatus
	err    error
}

func (f fakeBackupStatusReader) Read() (ports.BackupStatus, error) { return f.status, f.err }

type fakeRestoreStatusReader struct {
	status ports.RestoreStatus
	err    error
}

func (f fakeRestoreStatusReader) Read() (ports.RestoreStatus, error) { return f.status, f.err }

type fakeIntegrityStatusReader struct {
	status ports.IntegrityStatus
	err    error
}

func (f fakeIntegrityStatusReader) Read() (ports.IntegrityStatus, error) { return f.status, f.err }

type fakeTenantCapacityReader struct {
	snapshot ports.TenantCapacitySnapshot
	err      error
}

func (f fakeTenantCapacityReader) ReadTenantCapacity(context.Context, platform.ID) (ports.TenantCapacitySnapshot, error) {
	return f.snapshot, f.err
}

// fakeGov 是治理域的替身。
//
// Web 层的 bug（中间件顺序、CSRF、cookie 属性、越权）与存储层无关。
// 用真库测等于把两件事绑死：数据库连不上时，这一层的回归就测不了了。
type fakeGov struct {
	principal governance.Principal
	loginErr  error
	sessions  map[string]governance.Principal

	tokens      []governance.Token
	users       []governance.User
	spaces      []governance.Space
	permissions []governance.Permission
	userNext    string
	tokenNext   string
	spaceNext   string
	grantNext   string
	lastReset   platform.ID
	lastStatus  struct {
		user   platform.ID
		status string
	}
	revoked      platform.ID
	created      governance.CreateUserInput
	createdSpace string
	lastGrant    governance.Permission
	lastRevoke   struct{ space, user platform.ID }
	calls        int
}

const (
	tenantA   = platform.ID("01TENANTAAAAAAAAAAAAAAAAAA")
	userMe    = platform.ID("01USERMEAAAAAAAAAAAAAAAAAA")
	userOther = platform.ID("01USEROTHERAAAAAAAAAAAAAAA")
	spPers    = platform.ID("01SPACEPERSONALAAAAAAAAAAA")
	spTeam    = platform.ID("01SPACETEAMAAAAAAAAAAAAAAA")
	spProject = platform.ID("01SPACEPROJECTAAAAAAAAAAAA")
	goodToken = "session-token-for-tests"
)

func newFakeGov(role string, mustChange bool) *fakeGov {
	p := governance.Principal{
		TenantID: tenantA, UserID: userMe, Username: "maozai", DisplayName: "毛仔",
		Role: role, MustChangePassword: mustChange,
		SessionExpiresAt: time.Now().Add(time.Hour),
	}
	owner := userMe
	return &fakeGov{
		principal: p,
		sessions:  map[string]governance.Principal{goodToken: p},
		users: []governance.User{
			{ID: userMe, TenantID: tenantA, Username: "maozai", DisplayName: "毛仔", Role: role, Status: governance.StatusActive},
			{ID: userOther, TenantID: tenantA, Username: "tongshi", DisplayName: "同事", Role: governance.RoleMember, Status: governance.StatusActive},
		},
		spaces: []governance.Space{
			{ID: spTeam, TenantID: tenantA, Type: governance.SpaceTeam, Name: "团队"},
			{ID: spProject, TenantID: tenantA, Type: governance.SpaceProject, Name: "项目", OwnerUserID: &owner},
		},
	}
}

func (f *fakeGov) Login(_ context.Context, username, password string) (governance.SessionToken, governance.Principal, error) {
	f.calls++
	if f.loginErr != nil {
		return "", governance.Principal{}, f.loginErr
	}
	if username != "maozai" || password == "" {
		return "", governance.Principal{}, governance.ErrInvalidCredentials
	}
	f.sessions[goodToken] = f.principal
	return governance.SessionToken(goodToken), f.principal, nil
}

func (f *fakeGov) ResumeSession(_ context.Context, token string) (governance.Principal, error) {
	p, ok := f.sessions[token]
	if !ok {
		return governance.Principal{}, governance.ErrSessionInvalid
	}
	return p, nil
}

func (f *fakeGov) Logout(_ context.Context, token string) error {
	delete(f.sessions, token)
	return nil
}

func (f *fakeGov) ChangePassword(_ context.Context, _, _ platform.ID, old, _ string) error {
	f.calls++
	if old != "current-password" {
		return governance.ErrInvalidCredentials
	}
	p := f.principal
	p.MustChangePassword = false
	f.principal = p
	return nil
}

func (f *fakeGov) ResetPassword(_ context.Context, _, userID platform.ID, _ string) error {
	f.calls++
	f.lastReset = userID
	return nil
}

func (f *fakeGov) SetUserStatus(_ context.Context, _, userID platform.ID, status string) error {
	f.calls++
	f.lastStatus.user, f.lastStatus.status = userID, status
	return nil
}

func (f *fakeGov) ListUsers(_ context.Context, _ platform.ID) ([]governance.User, error) {
	return f.users, nil
}

func (f *fakeGov) ListUserSummariesPage(_ context.Context, in governance.GovernancePageInput) (governance.UserSummaryPage, error) {
	items := make([]governance.UserSummary, 0, len(f.users))
	for _, user := range f.users {
		if user.TenantID != in.TenantID {
			continue
		}
		items = append(items, userSummary(user))
	}
	return governance.UserSummaryPage{Items: items, NextCursor: f.userNext}, nil
}

func (f *fakeGov) FindUserSummary(_ context.Context, tenantID, userID platform.ID) (governance.UserSummary, error) {
	for _, user := range f.users {
		if user.TenantID == tenantID && user.ID == userID {
			return userSummary(user), nil
		}
	}
	return governance.UserSummary{}, governance.ErrUserNotFound
}

func (f *fakeGov) CreateUser(_ context.Context, in governance.CreateUserInput) (governance.User, governance.Space, error) {
	f.calls++
	f.created = in
	return governance.User{ID: "01NEWUSERAAAAAAAAAAAAAAAAA", Username: in.Username}, governance.Space{}, nil
}

func (f *fakeGov) ListSpaces(_ context.Context, _ platform.ID) ([]governance.Space, error) {
	return f.spaces, nil
}

func (f *fakeGov) ListSpacePage(_ context.Context, in governance.GovernancePageInput) (governance.SpacePage, error) {
	items := make([]governance.Space, 0, len(f.spaces))
	for _, space := range f.spaces {
		if space.TenantID == in.TenantID && space.Type != governance.SpacePersonal {
			items = append(items, space)
		}
	}
	return governance.SpacePage{Items: items, NextCursor: f.spaceNext}, nil
}

func (f *fakeGov) ListSpaceGrantPage(_ context.Context, in governance.GovernancePageInput) (governance.SpaceGrantPage, error) {
	var selected governance.Space
	found := false
	for _, space := range f.spaces {
		if space.TenantID == in.TenantID && space.ID == in.SpaceID {
			selected, found = space, true
			break
		}
	}
	if !found {
		return governance.SpaceGrantPage{}, governance.ErrSpaceNotFound
	}
	if selected.Type == governance.SpacePersonal {
		return governance.SpaceGrantPage{}, governance.ErrSpaceForbidden
	}
	items := make([]governance.SpaceGrantSummary, 0, len(f.users))
	for _, user := range f.users {
		if user.TenantID != in.TenantID || selected.OwnerUserID != nil && user.ID == *selected.OwnerUserID {
			continue
		}
		var permission *governance.Permission
		for i := range f.permissions {
			if f.permissions[i].SpaceID == selected.ID && f.permissions[i].UserID == user.ID {
				copy := f.permissions[i]
				permission = &copy
				break
			}
		}
		items = append(items, governance.SpaceGrantSummary{User: userSummary(user), Permission: permission})
	}
	return governance.SpaceGrantPage{Space: selected, Items: items, NextCursor: f.grantNext}, nil
}

func (f *fakeGov) ListSpacePermissions(_ context.Context, _ platform.ID) ([]governance.Permission, error) {
	return f.permissions, nil
}

func (f *fakeGov) CreateProjectSpace(_ context.Context, tenantID, ownerUserID platform.ID, name string) (governance.Space, error) {
	f.calls++
	f.createdSpace = name
	space := governance.Space{ID: "01NEWSPACEAAAAAAAAAAAAAAAA", TenantID: tenantID, Type: governance.SpaceProject, Name: name, OwnerUserID: &ownerUserID}
	f.spaces = append(f.spaces, space)
	return space, nil
}

func (f *fakeGov) GrantSpaceAccess(_ context.Context, _, spaceID, userID, grantedBy platform.ID, canRead, canWrite bool) error {
	f.calls++
	f.lastGrant = governance.Permission{SpaceID: spaceID, UserID: userID, GrantedBy: grantedBy, CanRead: canRead, CanWrite: canWrite}
	return nil
}

func (f *fakeGov) RevokeSpaceAccess(_ context.Context, _, spaceID, userID platform.ID) error {
	f.calls++
	f.lastRevoke.space, f.lastRevoke.user = spaceID, userID
	return nil
}

// Scope 给两个空间：个人（可读可写）与团队（隐含可读、写需授权而未授权）。
//
// 这个不对称是有意的——「可读集合」与「可写集合」在 Web 层是两条
// 不同的路径（浏览用前者，审核用后者），夹具里若只有一个可读可写的
// 空间，那两条路径就永远测不出区别。
func (f *fakeGov) Scope(_ context.Context, _ governance.Identity) (governance.SpaceScope, error) {
	owner := userMe
	return governance.ResolveScope(userMe, []governance.SpaceMembership{
		{Space: governance.Space{ID: spPers, TenantID: tenantA, Type: governance.SpacePersonal, Name: "毛仔", OwnerUserID: &owner}},
		{Space: governance.Space{ID: spTeam, TenantID: tenantA, Type: governance.SpaceTeam, Name: "团队"}},
	}), nil
}

func (f *fakeGov) CreateAgent(_ context.Context, _, _ platform.ID, slug, _ string) (governance.Agent, error) {
	f.calls++
	if slug == "taken" {
		return governance.Agent{}, governance.ErrAlreadyExists
	}
	return governance.Agent{ID: "01AGENTAAAAAAAAAAAAAAAAAAA", Slug: slug}, nil
}

func (f *fakeGov) IssueToken(_ context.Context, _ governance.IssueTokenInput) (string, governance.Token, error) {
	f.calls++
	return "ccs_plaintext_token_value", governance.Token{ID: "01TOKENAAAAAAAAAAAAAAAAAAA"}, nil
}

func (f *fakeGov) ListTokens(_ context.Context, _, _ platform.ID) ([]governance.Token, error) {
	return f.tokens, nil
}

func (f *fakeGov) ListTokenSummariesPage(_ context.Context, in governance.GovernancePageInput) (governance.TokenSummaryPage, error) {
	items := make([]governance.TokenSummary, 0, len(f.tokens))
	for _, token := range f.tokens {
		if token.TenantID != "" && token.TenantID != in.TenantID || token.UserID != "" && token.UserID != in.UserID {
			continue
		}
		items = append(items, tokenSummary(token))
	}
	return governance.TokenSummaryPage{Items: items, NextCursor: f.tokenNext}, nil
}

func (f *fakeGov) FindOwnTokenSummary(_ context.Context, tenantID, userID, tokenID platform.ID) (governance.TokenSummary, error) {
	for _, token := range f.tokens {
		if token.ID != tokenID || token.TenantID != "" && token.TenantID != tenantID || token.UserID != "" && token.UserID != userID {
			continue
		}
		return tokenSummary(token), nil
	}
	return governance.TokenSummary{}, governance.ErrTokenNotFound
}

func (f *fakeGov) ListAgentAccessSummaries(_ context.Context, tenantID, userID platform.ID, limit int) ([]governance.AgentAccessSummary, error) {
	byAgent := make(map[platform.ID]*governance.AgentAccessSummary)
	for _, token := range f.tokens {
		if token.TenantID != "" && token.TenantID != tenantID || token.UserID != "" && token.UserID != userID {
			continue
		}
		summary := byAgent[token.AgentID]
		if summary == nil {
			summary = &governance.AgentAccessSummary{AgentID: token.AgentID, LastIssuedAt: token.CreatedAt}
			byAgent[token.AgentID] = summary
		}
		summary.TokenCount++
		if token.RevokedAt == nil {
			summary.ActiveTokens++
		}
		if token.LastUsedAt != nil && (summary.LastUsedAt == nil || token.LastUsedAt.After(*summary.LastUsedAt)) {
			lastUsed := *token.LastUsedAt
			summary.LastUsedAt = &lastUsed
		}
		if token.CreatedAt.After(summary.LastIssuedAt) {
			summary.LastIssuedAt = token.CreatedAt
		}
	}
	items := make([]governance.AgentAccessSummary, 0, len(byAgent))
	for _, summary := range byAgent {
		items = append(items, *summary)
		if len(items) == limit {
			break
		}
	}
	return items, nil
}

func userSummary(user governance.User) governance.UserSummary {
	return governance.UserSummary{
		ID: user.ID, TenantID: user.TenantID, Username: user.Username,
		DisplayName: user.DisplayName, Role: user.Role, MustChangePw: user.MustChangePw,
		Status: user.Status, CreatedAt: user.CreatedAt,
	}
}

func tokenSummary(token governance.Token) governance.TokenSummary {
	return governance.TokenSummary{
		ID: token.ID, TenantID: token.TenantID, UserID: token.UserID, AgentID: token.AgentID,
		Name: token.Name, Prefix: token.Prefix, ExpiresAt: token.ExpiresAt,
		LastUsedAt: token.LastUsedAt, RevokedAt: token.RevokedAt, CreatedAt: token.CreatedAt,
	}
}

func (f *fakeGov) RevokeToken(_ context.Context, _, tokenID platform.ID) error {
	f.calls++
	f.revoked = tokenID
	return nil
}

type fakeKnow struct {
	items        []knowledge.ContentItem
	total        int
	nextCursor   string
	card         knowledge.Card
	memory       knowledge.Memory
	lastList     knowledge.ListContentsInput
	lastUpdate   knowledge.UpdateCardInput
	lastBatch    knowledge.DeprecateDraftCardsInput
	lastIngest   knowledge.IngestInput
	ingestReport knowledge.IngestReport
	ingestErr    error
	embedHealth  knowledge.EmbeddingHealth
	backfill     knowledge.BackfillReport
	retried      int
	updateErr    error
	batchErr     error
	getErr       error
	duplicates   []knowledge.CardDuplicateCandidate
	spaceStats   []knowledge.SpaceStats
	calls        int
}

type fakeActivity struct {
	records        []activity.RecordInput
	visibleInput   activity.ListVisibleInput
	adminInput     activity.ListAdminInput
	visiblePage    activity.Page
	adminPage      activity.Page
	recordErr      error
	visibleErr     error
	adminErr       error
	quality        activity.ContextFeedbackQuality
	qualityErr     error
	qualityTenant  platform.ID
	qualityWindow  time.Duration
	sharedPulse    activity.SharedActivityPulse
	sharedErr      error
	sharedTenant   platform.ID
	sharedSpaces   []platform.ID
	sharedDays     int
	listAdminCalls int
	order          []string
}

type fakeContinuity struct {
	lines       []continuity.LineSummary
	lastInput   continuity.ListInput
	lastGet     continuity.GetInput
	lastStatus  continuity.SetStatusInput
	packet      continuity.ResumePacket
	stats       continuity.LineStats
	statsTenant platform.ID
	err         error
}

func newFakeContinuity() *fakeContinuity {
	now := time.Date(2026, 8, 27, 11, 30, 0, 0, time.UTC)
	line := continuity.Line{
		ID: "01LINEAAAAAAAAAAAAAAAAAAAA", TenantID: tenantA, OwnerUserID: userMe, OwnerAgentID: "codex",
		Title: "ClaraCore 联调收口", Status: continuity.LineActive, CreatedAt: now.Add(-time.Hour), UpdatedAt: now,
		Position: &continuity.Position{
			ID: "01POSITIONAAAAAAAAAAAAAAAA", TenantID: tenantA, LineID: "01LINEAAAAAAAAAAAAAAAAAAAA",
			Summary: "共同线 Web 入口与筛选交互正在验证。", InterpretationStatus: continuity.PositionConfirmed,
			NextStep: "部署后人工刷新页面确认。", WriterUserID: userMe, WriterAgentID: "codex", Version: 2, UpdatedAt: now,
		},
	}
	handoff := continuity.Handoff{
		ID: "01HANDOFFAAAAAAAAAAAAAAAAAA", TenantID: tenantA, LineID: line.ID,
		WriterUserID: userMe, WriterAgentID: "codex", Objective: "上线共同线治理页",
		Completed: []string{"列表完成"}, OpenItems: []string{"人工验收"}, NextStep: "检查历史页", CreatedAt: now,
	}
	return &fakeContinuity{lines: []continuity.LineSummary{{
		ID: "01LINEAAAAAAAAAAAAAAAAAAAA", OwnerAgentID: "codex", Title: "ClaraCore 联调收口",
		Status: continuity.LineActive, Summary: "共同线 Web 入口与筛选交互正在验证。",
		InterpretationStatus: continuity.PositionConfirmed, NextStep: "部署后人工刷新页面确认。",
		UpdatedAt: now,
	}}, packet: continuity.ResumePacket{Line: line, RecentHandoff: &handoff, PositionHistory: []continuity.Position{*line.Position}, HandoffHistory: []continuity.Handoff{handoff}}, stats: continuity.LineStats{Active: 1}}
}

func (f *fakeContinuity) List(_ context.Context, in continuity.ListInput) ([]continuity.LineSummary, error) {
	f.lastInput = in
	lines := f.lines
	if in.Limit > 0 && len(lines) > in.Limit {
		lines = lines[:in.Limit]
	}
	return lines, f.err
}

func (f *fakeContinuity) ListPage(_ context.Context, in continuity.ListInput) (continuity.ListPage, error) {
	f.lastInput = in
	start := 0
	if in.Cursor != "" {
		start = 30
	}
	if start >= len(f.lines) {
		return continuity.ListPage{}, f.err
	}
	end := start + in.Limit
	if end > len(f.lines) {
		end = len(f.lines)
	}
	page := continuity.ListPage{Lines: f.lines[start:end]}
	if end < len(f.lines) {
		page.NextCursor = "next-cursor"
	}
	return page, f.err
}

func (f *fakeContinuity) Stats(_ context.Context, tenantID platform.ID) (continuity.LineStats, error) {
	f.statsTenant = tenantID
	return f.stats, f.err
}

func (f *fakeContinuity) Get(_ context.Context, in continuity.GetInput) (continuity.ResumePacket, error) {
	f.lastGet = in
	return f.packet, f.err
}

func (f *fakeContinuity) SetStatus(_ context.Context, in continuity.SetStatusInput) (continuity.ResumePacket, error) {
	f.lastStatus = in
	f.packet.Line.Status = in.Status
	return f.packet, f.err
}

func (f *fakeActivity) Record(_ context.Context, in activity.RecordInput) (activity.Event, error) {
	f.order = append(f.order, "record")
	f.records = append(f.records, in)
	if f.recordErr != nil {
		return activity.Event{}, f.recordErr
	}
	return activity.Event{Action: in.Action}, nil
}

func (f *fakeActivity) ListVisible(_ context.Context, in activity.ListVisibleInput) (activity.Page, error) {
	f.order = append(f.order, "visible")
	f.visibleInput = in
	return f.visiblePage, f.visibleErr
}

func (f *fakeActivity) ListAdmin(_ context.Context, in activity.ListAdminInput) (activity.Page, error) {
	f.order = append(f.order, "admin")
	f.listAdminCalls++
	f.adminInput = in
	return f.adminPage, f.adminErr
}

func (f *fakeActivity) ContextFeedbackQuality(_ context.Context, tenantID platform.ID, window time.Duration) (activity.ContextFeedbackQuality, error) {
	f.qualityTenant, f.qualityWindow = tenantID, window
	return f.quality, f.qualityErr
}

func (f *fakeActivity) SharedActivityPulse(_ context.Context, tenantID platform.ID, spaces []platform.ID, days int) (activity.SharedActivityPulse, error) {
	f.sharedTenant, f.sharedSpaces, f.sharedDays = tenantID, append([]platform.ID(nil), spaces...), days
	return f.sharedPulse, f.sharedErr
}

func (f *fakeKnow) SpaceStats(_ context.Context, _ platform.ID, spaces []platform.ID) ([]knowledge.SpaceStats, error) {
	if f.spaceStats != nil {
		return f.spaceStats, nil
	}
	out := make([]knowledge.SpaceStats, 0, len(spaces))
	for _, id := range spaces {
		out = append(out, knowledge.SpaceStats{
			SpaceID: id, Memories: 3, Cards: 2, DraftCards: 1, UnqualifiedCards: 1, RecallTriggers: 4,
			ActiveCards: 1, ActiveUnqualifiedCards: 0, RecallReadyCards: 1,
		})
	}
	return out, nil
}

func newTestWeb(t *testing.T, role string, mustChange bool) (*httptest.Server, *fakeGov) {
	t.Helper()
	gov := newFakeGov(role, mustChange)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	activity := &fakeActivity{}
	know := &fakeKnow{
		card: knowledge.Card{
			ID: "01CARDAAAAAAAAAAAAAAAAAAAA", TenantID: tenantA, SpaceID: spPers,
			Title: "摄取来的文档", CardType: "摄取原料", Status: knowledge.CardDraft,
			Source: "docs/x.md", Original: "一段原文。", Version: 1,
		},
		memory: knowledge.Memory{
			ID: "01MEMORYAAAAAAAAAAAAAAAAAA", TenantID: tenantA, SpaceID: spPers,
			Title: "一条记忆", Body: "正文。", Status: knowledge.MemoryActive, Version: 1,
		},
	}
	srv := httptest.NewServer(NewHost(gov, know, newFakeContinuity(), activity, logger, Options{
		Version: "test", PublicMCPURL: "https://ccs.example.com/mcp",
	}).Handler())
	t.Cleanup(srv.Close)
	return srv, gov
}

// newContentWeb 额外交出核心域替身，供审核/浏览页的用例断言。
func newContentWeb(t *testing.T, role string) (*httptest.Server, *fakeGov, *fakeKnow) {
	t.Helper()
	gov := newFakeGov(role, false)
	know := &fakeKnow{
		card: knowledge.Card{
			ID: "01CARDAAAAAAAAAAAAAAAAAAAA", TenantID: tenantA, SpaceID: spPers,
			Title: "摄取来的文档", CardType: "摄取原料", Status: knowledge.CardDraft,
			Source: "docs/x.md", Original: "一段原文。", Version: 1,
		},
		memory: knowledge.Memory{
			ID: "01MEMORYAAAAAAAAAAAAAAAAAA", TenantID: tenantA, SpaceID: spPers,
			Title: "一条记忆", Body: "正文。", Status: knowledge.MemoryActive, Version: 1,
		},
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(NewHost(gov, know, newFakeContinuity(), &fakeActivity{}, logger, Options{Version: "test"}).Handler())
	t.Cleanup(srv.Close)
	return srv, gov, know
}

func newActivityWeb(t *testing.T, role string) (*httptest.Server, *fakeGov, *fakeKnow, *fakeActivity) {
	t.Helper()
	gov := newFakeGov(role, false)
	know := &fakeKnow{
		card: knowledge.Card{
			ID: "01CARDAAAAAAAAAAAAAAAAAAAA", TenantID: tenantA, SpaceID: spPers,
			Title: "摄取来的文档", CardType: "摄取原料", Status: knowledge.CardDraft,
			Source: "docs/x.md", Original: "一段原文。", Version: 1,
		},
	}
	act := &fakeActivity{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(NewHost(gov, know, newFakeContinuity(), act, logger, Options{Version: "test"}).Handler())
	t.Cleanup(srv.Close)
	return srv, gov, know, act
}

// client 返回一个不自动跳转的客户端：重定向本身是被测行为。
func client(srv *httptest.Server) *http.Client {
	return &http.Client{
		CheckRedirect: func(*http.Request, []*http.Request) error { return http.ErrUseLastResponse },
	}
}

func get(t *testing.T, srv *httptest.Server, path string, withSession bool) *http.Response {
	t.Helper()
	req, err := http.NewRequest(http.MethodGet, srv.URL+path, nil)
	if err != nil {
		t.Fatal(err)
	}
	if withSession {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: goodToken})
	}
	resp, err := client(srv).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func post(t *testing.T, srv *httptest.Server, path string, form url.Values, withSession, withCSRF bool) *http.Response {
	t.Helper()
	if withCSRF {
		form.Set("csrf", csrfToken(goodToken))
	}
	req, err := http.NewRequest(http.MethodPost, srv.URL+path, strings.NewReader(form.Encode()))
	if err != nil {
		t.Fatal(err)
	}
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	if withSession {
		req.AddCookie(&http.Cookie{Name: sessionCookie, Value: goodToken})
	}
	resp, err := client(srv).Do(req)
	if err != nil {
		t.Fatal(err)
	}
	return resp
}

func TestWebWriteBoundaryRejectsOversizedAndUnsupportedBodies(t *testing.T) {
	gov := newFakeGov(governance.RoleAdmin, false)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(NewHost(gov, &fakeKnow{}, newFakeContinuity(), &fakeActivity{}, logger, Options{
		Version: "test", MaxFormBytes: 128,
	}).Handler())
	t.Cleanup(srv.Close)

	for _, tc := range []struct {
		name        string
		contentType string
		body        string
		wantStatus  int
	}{
		{name: "oversized urlencoded form", contentType: "application/x-www-form-urlencoded", body: "username=" + strings.Repeat("x", 256), wantStatus: http.StatusRequestEntityTooLarge},
		{name: "unsupported json", contentType: "application/json", body: `{}`, wantStatus: http.StatusUnsupportedMediaType},
	} {
		t.Run(tc.name, func(t *testing.T) {
			req, err := http.NewRequest(http.MethodPost, srv.URL+"/login", strings.NewReader(tc.body))
			if err != nil {
				t.Fatal(err)
			}
			req.Header.Set("Content-Type", tc.contentType)
			resp, err := client(srv).Do(req)
			if err != nil {
				t.Fatal(err)
			}
			defer resp.Body.Close()
			if resp.StatusCode != tc.wantStatus {
				t.Fatalf("status = %d, want %d", resp.StatusCode, tc.wantStatus)
			}
		})
	}
	if gov.calls != 0 {
		t.Fatalf("oversized or unsupported writes reached governance: calls = %d", gov.calls)
	}
}

func body(t *testing.T, resp *http.Response) string {
	t.Helper()
	defer resp.Body.Close()
	b, err := io.ReadAll(resp.Body)
	if err != nil {
		t.Fatal(err)
	}
	return string(b)
}

// ---------------------------------------------------------------------------
// 认证边界
// ---------------------------------------------------------------------------

// TestUnauthenticatedIsRedirectedEverywhere 扫全部受保护路径。
//
// 逐个 handler 判断的话，少判一次不会有编译错误，只会有一个
// 不需要登录就能打开的页面。这条测试盯的正是「新加的路由忘了挂中间件」。
func TestUnauthenticatedIsRedirectedEverywhere(t *testing.T) {
	srv, _ := newTestWeb(t, governance.RoleAdmin, false)

	for _, path := range []string{"/", "/governance", "/context-quality", "/tokens", "/password", "/activity", "/admin/members", "/admin/spaces", "/admin/ingest", "/admin/indexing", "/admin/audit", "/admin/system", "/admin/sources", "/admin/export"} {
		t.Run(path, func(t *testing.T) {
			resp := get(t, srv, path, false)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusSeeOther {
				t.Fatalf("未登录访问 %s 应重定向，实际 %d", path, resp.StatusCode)
			}
			if loc := resp.Header.Get("Location"); loc != "/login" {
				t.Fatalf("应跳到登录页，实际 %q", loc)
			}
		})
	}
}

func TestMemberCannotSeeAdminPages(t *testing.T) {
	srv, gov := newTestWeb(t, governance.RoleMember, false)

	for _, path := range []string{"/admin/members", "/admin/spaces", "/admin/ingest", "/admin/indexing", "/admin/audit", "/admin/system", "/admin/sources", "/admin/export"} {
		resp := get(t, srv, path, true)
		defer resp.Body.Close()
		// 404 而不是 403：不确认「这个管理页存在」
		if resp.StatusCode != http.StatusNotFound {
			t.Fatalf("普通成员访问 %s 应得到 404，实际 %d", path, resp.StatusCode)
		}
	}
	if gov.calls != 0 {
		t.Fatal("权限没通过时不该调领域层")
	}
}

func TestMemberCannotPostToAdminEndpoints(t *testing.T) {
	srv, gov := newTestWeb(t, governance.RoleMember, false)

	for _, path := range []string{"/admin/members", "/admin/members/status", "/admin/members/reset", "/admin/spaces", "/admin/spaces/grant", "/admin/spaces/revoke", "/admin/ingest", "/admin/indexing/backfill", "/admin/indexing/retry", "/admin/audit", "/admin/sources/sync", "/admin/export"} {
		t.Run(path, func(t *testing.T) {
			resp := post(t, srv, path, url.Values{"id": {string(userOther)}, "status": {"disabled"}}, true, true)
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusNotFound {
				t.Fatalf("普通成员 POST %s 应被拒绝，实际 %d", path, resp.StatusCode)
			}
		})
	}
	if gov.calls != 0 {
		t.Fatal("越权的写请求一次都不该到达领域层")
	}
}

// TestMustChangePasswordBlocksEverythingElse 盯住强制改密。
//
// 管理员分配的初始密码至少经过一次口头或聊天工具的传递，
// 在改掉之前不该能做别的事。
func TestMustChangePasswordBlocksEverythingElse(t *testing.T) {
	srv, _ := newTestWeb(t, governance.RoleAdmin, true)

	for _, path := range []string{"/", "/governance", "/context-quality", "/tokens", "/activity", "/admin/members", "/admin/spaces", "/admin/ingest", "/admin/indexing", "/admin/audit", "/admin/system", "/admin/sources", "/admin/export"} {
		resp := get(t, srv, path, true)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusSeeOther || resp.Header.Get("Location") != "/password" {
			t.Fatalf("%s 应被赶去改密页，实际 %d → %q", path, resp.StatusCode, resp.Header.Get("Location"))
		}
	}

	// 改密页自己必须进得去，否则就是一个谁也解不开的死锁
	resp := get(t, srv, "/password", true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("改密页必须可访问，实际 %d", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// CSRF
// ---------------------------------------------------------------------------

// TestWritesRequireCSRF 扫全部写路径。
//
// 与认证那条同一个理由：这条盯的是「新加的 POST 路由忘了校 CSRF」。
func TestWritesRequireCSRF(t *testing.T) {
	srv, gov := newTestWeb(t, governance.RoleAdmin, false)

	writes := map[string]url.Values{
		"/logout":                            {},
		"/password":                          {"current": {"x"}, "new": {"yyyyyyyyyy"}, "confirm": {"yyyyyyyyyy"}},
		"/tokens":                            {"agent": {"clara"}, "name": {"MacBook"}},
		"/tokens/revoke":                     {"id": {"01TOKENAAAAAAAAAAAAAAAAAAA"}},
		"/review/01CARDAAAAAAAAAAAAAAAAAAAA": {"action": {"reject"}},
		"/admin/members":                     {"username": {"newbie"}},
		"/admin/members/status":              {"id": {string(userOther)}, "status": {"disabled"}},
		"/admin/members/reset":               {"id": {string(userOther)}},
		"/admin/spaces":                      {"name": {"新项目"}},
		"/admin/spaces/grant":                {"space": {string(spProject)}, "user": {string(userOther)}, "access": {"write"}},
		"/admin/spaces/revoke":               {"space": {string(spProject)}, "user": {string(userOther)}},
		"/admin/ingest":                      {"space": {string(spPers)}, "source": {"team-handbook"}},
		"/admin/indexing/backfill":           {},
		"/admin/indexing/retry":              {},
		"/admin/audit":                       {},
		"/admin/sources/sync":                {},
		"/admin/export":                      {"confirm": {tenantExportConfirmation}},
	}
	for path, form := range writes {
		t.Run(path, func(t *testing.T) {
			resp := post(t, srv, path, form, true, false) // 不带 csrf
			defer resp.Body.Close()
			if resp.StatusCode != http.StatusForbidden {
				t.Fatalf("没有 CSRF token 的写请求应被拒绝，实际 %d", resp.StatusCode)
			}
		})
	}
	if gov.calls != 0 {
		t.Fatal("CSRF 没过的请求一次都不该到达领域层")
	}
}

func TestAdminCanManageProjectSpacesAndPermissions(t *testing.T) {
	srv, gov := newTestWeb(t, governance.RoleAdmin, false)

	page := body(t, get(t, srv, "/admin/spaces", true))
	for _, want := range []string{"空间与权限", "团队", "项目", "同事", "默认只读"} {
		if !strings.Contains(page, want) {
			t.Fatalf("空间治理页缺少 %q: %.300s", want, page)
		}
	}

	resp := post(t, srv, "/admin/spaces", url.Values{"name": {"新项目"}}, true, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || gov.createdSpace != "新项目" {
		t.Fatalf("创建项目空间失败: status=%d name=%q", resp.StatusCode, gov.createdSpace)
	}

	resp = post(t, srv, "/admin/spaces/grant", url.Values{
		"space": {string(spProject)}, "user": {string(userOther)}, "access": {"write"},
	}, true, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || !gov.lastGrant.CanRead || !gov.lastGrant.CanWrite {
		t.Fatalf("读写授权没有正确下沉到领域层: status=%d grant=%+v", resp.StatusCode, gov.lastGrant)
	}

	resp = post(t, srv, "/admin/spaces/revoke", url.Values{
		"space": {string(spProject)}, "user": {string(userOther)},
	}, true, true)
	resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther || gov.lastRevoke.space != spProject || gov.lastRevoke.user != userOther {
		t.Fatalf("撤销授权没有正确下沉到领域层: status=%d revoke=%+v", resp.StatusCode, gov.lastRevoke)
	}
}

func TestGovernancePagesExposeBoundedKeysetNavigation(t *testing.T) {
	srv, gov := newTestWeb(t, governance.RoleAdmin, false)
	gov.userNext = "next-member"
	gov.tokenNext = "next-token"
	gov.spaceNext = "next-space"
	gov.grantNext = "next-grant"
	gov.tokens = []governance.Token{{ID: "01TOKENAAAAAAAAAAAAAAAAAAA", Name: "Laptop", Prefix: "ccs_safe"}}

	members := body(t, get(t, srv, "/admin/members?cursor=current-member", true))
	for _, want := range []string{"返回第一页", "/admin/members?cursor=next-member", "下一页"} {
		if !strings.Contains(members, want) {
			t.Errorf("member page missing bounded navigation %q", want)
		}
	}
	tokens := body(t, get(t, srv, "/tokens?cursor=current-token", true))
	for _, want := range []string{"返回第一页", "/tokens?cursor=next-token", "下一页"} {
		if !strings.Contains(tokens, want) {
			t.Errorf("token page missing bounded navigation %q", want)
		}
	}
	spaces := body(t, get(t, srv, "/admin/spaces?space="+string(spProject)+"&spaces_cursor=current-space&grants_cursor=current-grant", true))
	for _, want := range []string{
		"每页最多 20 个", "返回第一页", "spaces_cursor=next-space", "返回成员第一页",
		"grants_cursor=next-grant", "space=" + string(spProject), "spaces_cursor=current-space",
	} {
		if !strings.Contains(spaces, want) {
			t.Errorf("space page missing bounded navigation %q", want)
		}
	}
	if strings.Contains(spaces, "创建者：毛仔") && strings.Contains(spaces, `aria-label="团队的成员权限"`) {
		t.Fatal("space governance must expand only the selected space's member matrix")
	}
}

// TestCSRFTokenIsSessionBound 确认 CSRF token 不能跨会话复用。
func TestCSRFTokenIsSessionBound(t *testing.T) {
	if csrfToken("session-a") == csrfToken("session-b") {
		t.Fatal("不同会话必须派生出不同的 CSRF token")
	}
	// 派生用 HMAC 而不是明文拼接：即使派生值泄露也推不回会话令牌
	if strings.Contains(csrfToken(goodToken), goodToken) {
		t.Fatal("CSRF token 里不该出现会话令牌本身")
	}
}

// ---------------------------------------------------------------------------
// Markdown 导入
// ---------------------------------------------------------------------------

func TestResolveIngestDirRejectsTraversalAndSymlinkEscape(t *testing.T) {
	root := t.TempDir()
	outside := t.TempDir()
	inside := filepath.Join(root, "docs")
	if err := os.Mkdir(inside, 0o755); err != nil {
		t.Fatal(err)
	}
	realInside, err := filepath.EvalSymlinks(inside)
	if err != nil {
		t.Fatal(err)
	}
	if got, err := resolveIngestDir(root, "docs"); err != nil || got != realInside {
		t.Fatalf("根目录内的普通目录应允许: got=%q err=%v", got, err)
	}
	if _, err := resolveIngestDir(root, "../escape"); err == nil {
		t.Fatal("相对路径穿越必须被拒绝")
	}
	link := filepath.Join(root, "outside-link")
	if err := os.Symlink(outside, link); err != nil {
		t.Fatal(err)
	}
	if _, err := resolveIngestDir(root, "outside-link"); err == nil {
		t.Fatal("指向根目录外的符号链接必须被拒绝")
	}
}

func TestAdminIngestUsesWritableSpaceAndSafeMissingDetection(t *testing.T) {
	root := t.TempDir()
	docsDir := filepath.Join(root, "docs")
	if err := os.Mkdir(docsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	realDocsDir, err := filepath.EvalSymlinks(docsDir)
	if err != nil {
		t.Fatal(err)
	}
	gov := newFakeGov(governance.RoleAdmin, false)
	know := &fakeKnow{ingestReport: knowledge.IngestReport{
		Created: 1,
		Missing: []knowledge.IngestMissing{{CardID: "01MISSINGCARDAAAAAAAAAAAAA", SourcePath: "old.md"}},
	}}
	act := &fakeActivity{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(NewHost(gov, know, newFakeContinuity(), act, logger, Options{
		Version: "test", IngestRoot: root,
		ScanMarkdown: func(_ context.Context, sourceName, scanRoot string) ([]knowledge.Document, error) {
			if sourceName != "team-handbook" || scanRoot != realDocsDir {
				t.Fatalf("扫描器收到错误边界: source=%q root=%q", sourceName, scanRoot)
			}
			return []knowledge.Document{docForWeb("k1", "h1")}, nil
		},
	}).Handler())
	t.Cleanup(srv.Close)

	resp := post(t, srv, "/admin/ingest", url.Values{
		"space": {string(spPers)}, "source": {"team-handbook"}, "dir": {"docs"},
	}, true, true)
	page := body(t, resp)
	if know.lastIngest.SpaceID != spPers || know.lastIngest.SourceName != "team-handbook" || !know.lastIngest.DetectMissing {
		t.Fatalf("完整扫描应按可写空间执行并开启缺失检测，实际 %+v", know.lastIngest)
	}
	if !strings.Contains(page, "仅报告，未自动修改卡片") || !strings.Contains(page, "old.md") {
		t.Fatalf("页面应明确展示非破坏性的缺失报告，实际 %.400s", page)
	}
	if len(act.records) != 1 || act.records[0].Action != "ingest.run" || act.records[0].SpaceID == nil || *act.records[0].SpaceID != spPers {
		t.Fatalf("导入应留下只含稳定标识的治理记录，实际 %+v", act.records)
	}
}

func TestPartialIngestScanDisablesMissingDetection(t *testing.T) {
	root := t.TempDir()
	gov := newFakeGov(governance.RoleAdmin, false)
	know := &fakeKnow{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(NewHost(gov, know, newFakeContinuity(), &fakeActivity{}, logger, Options{
		Version: "test", IngestRoot: root,
		ScanMarkdown: func(context.Context, string, string) ([]knowledge.Document, error) {
			return []knowledge.Document{docForWeb("k1", "h1")}, errors.New("one file unreadable")
		},
	}).Handler())
	t.Cleanup(srv.Close)

	page := body(t, post(t, srv, "/admin/ingest", url.Values{
		"space": {string(spPers)}, "source": {"team-handbook"},
	}, true, true))
	if know.lastIngest.DetectMissing {
		t.Fatal("部分扫描不能证明未出现的文件已删除")
	}
	if !strings.Contains(page, "本轮未执行缺失检测") {
		t.Fatalf("页面应把降级讲清楚，实际 %.300s", page)
	}
}

func docForWeb(key, hash string) knowledge.Document {
	return knowledge.Document{SourceKey: key, SourcePath: key + ".md", ContentHash: hash, Title: "标题", Body: "正文。"}
}

func TestAdminCanObserveBackfillAndRetryEmbeddingFailures(t *testing.T) {
	srv, _, know := newContentWeb(t, governance.RoleAdmin)
	know.embedHealth = knowledge.EmbeddingHealth{
		Enabled: true, Model: "bge-m3",
		Stats: knowledge.EmbeddingStats{Pending: 3, Ready: 9, Failed: 1, Exhausted: 1},
		Failures: []knowledge.Embedding{{
			Ref:      knowledge.Ref{Type: knowledge.EntityMemory, ID: "01FAILEDMEAaaaaaaaaaaaaaaa"},
			Attempts: 5, LastError: "model unavailable", UpdatedAt: time.Now(),
		}},
	}
	page := body(t, get(t, srv, "/admin/indexing", true))
	for _, want := range []string{"bge-m3", "model unavailable", "已停止自动重试"} {
		if !strings.Contains(page, want) {
			t.Fatalf("索引页缺少 %q，实际 %.500s", want, page)
		}
	}

	know.backfill = knowledge.BackfillReport{Enabled: true, Model: "bge-m3", Memories: 2, Cards: 1, Triggers: 3}
	page = body(t, post(t, srv, "/admin/indexing/backfill", url.Values{}, true, true))
	if !strings.Contains(page, "Memory 2") || !strings.Contains(page, "召回条件 3") {
		t.Fatalf("回填结果应可见，实际 %.400s", page)
	}

	know.retried = 1
	page = body(t, post(t, srv, "/admin/indexing/retry", url.Values{}, true, true))
	if !strings.Contains(page, "重新入队 1 条") {
		t.Fatalf("人工重试数量应可见，实际 %.400s", page)
	}
}

// ---------------------------------------------------------------------------
// 登录与 cookie
// ---------------------------------------------------------------------------

func TestLoginSetsHardenedCookie(t *testing.T) {
	srv, _ := newTestWeb(t, governance.RoleAdmin, false)

	resp := post(t, srv, "/login", url.Values{"username": {"maozai"}, "password": {"pw"}}, false, false)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("登录成功应重定向，实际 %d", resp.StatusCode)
	}

	var found *http.Cookie
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie {
			found = c
		}
	}
	if found == nil {
		t.Fatal("应当下发会话 cookie")
	}
	// HttpOnly：即使页面上出现 XSS，会话令牌也不会被脚本偷走
	if !found.HttpOnly {
		t.Fatal("会话 cookie 必须是 HttpOnly")
	}
	// SameSite=Lax：跨站发起的 POST 不带 cookie，这是 CSRF 的第一道门
	if found.SameSite != http.SameSiteLaxMode {
		t.Fatalf("会话 cookie 应为 SameSite=Lax，实际 %v", found.SameSite)
	}
}

func TestLoginFailureSaysNothingSpecific(t *testing.T) {
	srv, gov := newTestWeb(t, governance.RoleAdmin, false)
	gov.loginErr = governance.ErrInvalidCredentials

	resp := post(t, srv, "/login", url.Values{"username": {"nobody"}, "password": {"x"}}, false, false)
	page := body(t, resp)
	if resp.StatusCode != http.StatusUnauthorized {
		t.Fatalf("登录失败应返回 401，实际 %d", resp.StatusCode)
	}
	// 区分「账号不存在」与「密码不对」等于提供账号枚举接口
	for _, leak := range []string{"不存在", "已停用", "disabled"} {
		if strings.Contains(page, leak) {
			t.Fatalf("登录失败页不得泄露具体原因（出现了 %q）", leak)
		}
	}
}

func TestLoginRedirectsToPasswordWhenMustChange(t *testing.T) {
	srv, _ := newTestWeb(t, governance.RoleAdmin, true)

	resp := post(t, srv, "/login", url.Values{"username": {"maozai"}, "password": {"pw"}}, false, false)
	defer resp.Body.Close()
	if loc := resp.Header.Get("Location"); loc != "/password" {
		t.Fatalf("首次登录应直接去改密页，实际 %q", loc)
	}
}

func TestLogoutClearsCookieAndSession(t *testing.T) {
	srv, gov := newTestWeb(t, governance.RoleAdmin, false)

	resp := post(t, srv, "/logout", url.Values{}, true, true)
	defer resp.Body.Close()
	if _, still := gov.sessions[goodToken]; still {
		t.Fatal("登出应删掉会话")
	}
	for _, c := range resp.Cookies() {
		if c.Name == sessionCookie && c.MaxAge >= 0 {
			t.Fatal("登出应清掉 cookie")
		}
	}
}

// ---------------------------------------------------------------------------
// token 页
// ---------------------------------------------------------------------------

func TestTokenCreateShowsPlaintextOnceWithSnippet(t *testing.T) {
	srv, _ := newTestWeb(t, governance.RoleMember, false)

	resp := post(t, srv, "/tokens", url.Values{"agent": {"clara"}, "name": {"MacBook"}}, true, true)
	page := body(t, resp)

	if !strings.Contains(page, "ccs_plaintext_token_value") {
		t.Fatal("创建后应展示一次明文")
	}
	// 不要让人自己拼 JSON——接入体验的差距全在这里
	if !strings.Contains(page, "https://ccs.example.com/mcp") {
		t.Fatal("应给出可直接粘贴的 MCP 配置，且地址是对外地址而不是监听地址")
	}
	if !strings.Contains(page, "Bearer ccs_plaintext_token_value") {
		t.Fatal("配置片段里应已经嵌好 token")
	}

	// 再打开一次页面，明文不该再出现
	fresh := body(t, get(t, srv, "/tokens", true))
	if strings.Contains(fresh, "ccs_plaintext_token_value") {
		t.Fatal("明文只该出现那一次——库里只有哈希，刷新后就取不回来了")
	}
}

// TestTokenRevokeRejectsSomeoneElsesToken 是一条越权防线。
//
// RevokeToken 只按 tenant 过滤，所以「这个 token 是不是我的」
// 必须由 Host 检查——否则拿到一个别人的 token ID 就能把同事的 agent 停掉。
func TestTokenRevokeRejectsSomeoneElsesToken(t *testing.T) {
	srv, gov := newTestWeb(t, governance.RoleMember, false)
	gov.tokens = []governance.Token{{ID: "01MYTOKENAAAAAAAAAAAAAAAAA", Name: "我的"}}

	resp := post(t, srv, "/tokens/revoke", url.Values{"id": {"01SOMEONEELSESAAAAAAAAAAAA"}}, true, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("吊销别人的 token 应得到 404，实际 %d", resp.StatusCode)
	}
	if gov.revoked != "" {
		t.Fatalf("不该真的吊销任何 token，实际吊销了 %s", gov.revoked)
	}

	// 自己的可以吊销
	resp2 := post(t, srv, "/tokens/revoke", url.Values{"id": {"01MYTOKENAAAAAAAAAAAAAAAAA"}}, true, true)
	defer resp2.Body.Close()
	if gov.revoked != "01MYTOKENAAAAAAAAAAAAAAAAA" {
		t.Fatal("自己的 token 应当能吊销")
	}
}

func TestDuplicateAgentSlugGivesUsefulMessage(t *testing.T) {
	srv, _ := newTestWeb(t, governance.RoleMember, false)

	resp := post(t, srv, "/tokens", url.Values{"agent": {"taken"}, "name": {"MacBook"}}, true, true)
	page := body(t, resp)
	if !strings.Contains(page, "taken") {
		t.Fatalf("重名应给出能照着改的提示，实际页面: %.200s", page)
	}
}

// ---------------------------------------------------------------------------
// 成员管理
// ---------------------------------------------------------------------------

func TestCreateMemberGeneratesInitialPassword(t *testing.T) {
	srv, gov := newTestWeb(t, governance.RoleAdmin, false)

	resp := post(t, srv, "/admin/members", url.Values{
		"username": {"newbie"}, "display": {"新人"}, "role": {"member"},
	}, true, true)
	page := body(t, resp)

	// 初始密码由服务端生成：管理员自己想的那个大概率是 Team2026!，
	// 而它会被同一批人共用
	if gov.created.Password == "" {
		t.Fatal("应由服务端生成初始密码")
	}
	if len(gov.created.Password) < governance.MinPasswordLen {
		t.Fatalf("生成的密码太短: %d 位", len(gov.created.Password))
	}
	if !strings.Contains(page, gov.created.Password) {
		t.Fatal("应把初始密码展示给管理员，让他发给本人")
	}
}

func TestAdminCannotDisableSelf(t *testing.T) {
	srv, gov := newTestWeb(t, governance.RoleAdmin, false)

	resp := post(t, srv, "/admin/members/status", url.Values{
		"id": {string(userMe)}, "status": {"disabled"},
	}, true, true)
	page := body(t, resp)

	// 把自己关在门外之后没有恢复路径：无自助注册，没有第二个入口
	if gov.lastStatus.user != "" {
		t.Fatal("不该真的停用自己")
	}
	if !strings.Contains(page, "不能停用自己") {
		t.Fatalf("应给出明确提示，实际页面: %.200s", page)
	}
}

func TestAdminCanDisableOthers(t *testing.T) {
	srv, gov := newTestWeb(t, governance.RoleAdmin, false)

	resp := post(t, srv, "/admin/members/status", url.Values{
		"id": {string(userOther)}, "status": {"disabled"},
	}, true, true)
	defer resp.Body.Close()
	if gov.lastStatus.user != userOther || gov.lastStatus.status != "disabled" {
		t.Fatalf("应停用目标账号，实际 %+v", gov.lastStatus)
	}
}

// ---------------------------------------------------------------------------
// 渲染
// ---------------------------------------------------------------------------

func TestOverviewShowsDraftBacklog(t *testing.T) {
	srv, _ := newTestWeb(t, governance.RoleMember, false)

	page := body(t, get(t, srv, "/", true))
	// 04 §6.1 点名的那个空白：draft 需要一个可见入口，
	// 否则摄取灌进来几千条，没人知道它们在等谁
	if !strings.Contains(page, "待审草稿") {
		t.Fatal("概览页必须给出待审草稿的入口")
	}
	if !strings.Contains(page, "毛仔") {
		t.Fatal("应列出可访问的空间")
	}
}

func TestOverviewShowsAgentsAndRecentSharedCards(t *testing.T) {
	gov := newFakeGov(governance.RoleMember, false)
	lastUsed := time.Date(2026, 8, 28, 8, 0, 0, 0, time.UTC)
	gov.tokens = []governance.Token{{ID: "token", AgentID: "01AGENTAAAAAAAAAAAAAAAAAAA", Name: "MacBook", Prefix: "ccs", LastUsedAt: &lastUsed}}
	know := &fakeKnow{items: []knowledge.ContentItem{{
		Ref: knowledge.Ref{Type: knowledge.EntityCard, ID: "01CARDAAAAAAAAAAAAAAAAAAAA"}, SpaceID: spTeam,
		Title: "团队发布卡片", Status: knowledge.CardActive, AuthorUserID: userOther, UpdatedAt: lastUsed,
	}}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(NewHost(gov, know, newFakeContinuity(), &fakeActivity{}, logger, Options{Version: "test"}).Handler())
	t.Cleanup(srv.Close)

	page := body(t, get(t, srv, "/", true))
	for _, want := range []string{"我的 Agent", "01AGENTAAAAAAAAAAAAAAAAAAA", "共签发 1 个 token", "团队近期卡片", "团队发布卡片", "团队"} {
		if !strings.Contains(page, want) {
			t.Errorf("概览缺少 %q", want)
		}
	}
	if know.lastList.Kind != knowledge.EntityCard || len(know.lastList.Statuses) != 1 || know.lastList.Statuses[0] != knowledge.CardActive || len(know.lastList.Spaces) != 1 || know.lastList.Spaces[0] != spTeam {
		t.Fatalf("概览近期动态必须只查共享空间 active 卡片，实际 %+v", know.lastList)
	}
}

func TestOverviewShowsActionableRuntimeAndQualitySignals(t *testing.T) {
	gov := newFakeGov(governance.RoleMember, false)
	know := &fakeKnow{
		spaceStats: []knowledge.SpaceStats{{
			SpaceID: spTeam, Cards: 12, ActiveCards: 10, ActiveUnqualifiedCards: 2,
			RecallReadyCards: 7, RecallTriggers: 15,
		}},
		embedHealth: knowledge.EmbeddingHealth{
			Enabled: true, Model: "bge-m3",
			Stats: knowledge.EmbeddingStats{Pending: 5, Ready: 20, Failed: 2, Exhausted: 1},
		},
	}
	lines := newFakeContinuity()
	lines.stats = continuity.LineStats{Active: 4, Archived: 3, NeedsReview: 1, Stale: 2}
	activities := &fakeActivity{quality: activity.ContextFeedbackQuality{
		Selected: 18, Abstained: 7, FeedbackCoveredSelections: 9,
		AbstainReasons: activity.ContextAbstainReasons{
			NoRelevantEvidence: 2, Ambiguous: 2, SemanticUnavailable: 1,
			RetrievalTimeout: 1, SharedLineNotReady: 1,
		},
		Delivered: 3, Used: 6, Wrong: 2, Corrected: 1,
	}, sharedPulse: activity.SharedActivityPulse{
		Writes: 11, ActiveUsers: 3, ActiveAgents: 2,
		DailyWrites: []activity.DailySharedWrites{
			{Date: time.Date(2026, 8, 22, 0, 0, 0, 0, time.UTC), Writes: 0},
			{Date: time.Date(2026, 8, 23, 0, 0, 0, 0, time.UTC), Writes: 1},
			{Date: time.Date(2026, 8, 24, 0, 0, 0, 0, time.UTC), Writes: 2},
			{Date: time.Date(2026, 8, 25, 0, 0, 0, 0, time.UTC), Writes: 0},
			{Date: time.Date(2026, 8, 26, 0, 0, 0, 0, time.UTC), Writes: 3},
			{Date: time.Date(2026, 8, 27, 0, 0, 0, 0, time.UTC), Writes: 1},
			{Date: time.Date(2026, 8, 28, 0, 0, 0, 0, time.UTC), Writes: 4},
		},
	}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(NewHost(gov, know, lines, activities, logger, Options{Version: "test"}).Handler())
	t.Cleanup(srv.Close)

	page := body(t, get(t, srv, "/", true))
	for _, want := range []string{
		`aria-labelledby="operations-heading"`, "运行与质量", "进行中的共同线", "待复核 1 · 已陈旧 2",
		"有召回条件的卡片", "7 / 10", "2 张已发布卡片缺少", "语义索引失败", "等待 5 · 已耗尽重试 1",
		"近 30 天已关联反馈的选择", "9 / 18", "自动放弃 7：无相关证据 2 · 歧义 2 · 语义不可用 1 · 超时 1", "Shared Line 未就绪 1 · 其他/旧记录 0 · 错误反馈 2 · 已纠正 1", "匹配本窗口真实选择", "不等于自动上下文准确率",
		"近 7 天共享协作", "共享写入", ">11<", "活跃成员", ">3<", "活跃 Agent", ">2<", "08-28", "4</strong><small>次写入", "不含个人空间",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("概览看板缺少 %q", want)
		}
	}
	if lines.statsTenant != tenantA || activities.qualityTenant != tenantA || activities.qualityWindow != 30*24*time.Hour ||
		activities.sharedTenant != tenantA || activities.sharedDays != 7 || len(activities.sharedSpaces) != 1 || activities.sharedSpaces[0] != spTeam {
		t.Fatalf("看板统计必须按当前租户、可读共享空间与固定窗口查询: lineTenant=%s feedbackTenant=%s feedbackWindow=%s sharedTenant=%s sharedSpaces=%v sharedDays=%d", lines.statsTenant, activities.qualityTenant, activities.qualityWindow, activities.sharedTenant, activities.sharedSpaces, activities.sharedDays)
	}
}

func TestOverviewKeepsRenderingWhenOptionalSignalsFail(t *testing.T) {
	gov := newFakeGov(governance.RoleMember, false)
	know := &fakeKnow{ingestErr: errors.New("embedding stats unavailable")}
	lines := newFakeContinuity()
	lines.err = errors.New("line stats unavailable")
	activities := &fakeActivity{qualityErr: errors.New("feedback stats unavailable"), sharedErr: errors.New("shared activity unavailable")}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(NewHost(gov, know, lines, activities, logger, Options{Version: "test"}).Handler())
	t.Cleanup(srv.Close)

	page := body(t, get(t, srv, "/", true))
	for _, want := range []string{"运行与质量", "共同线统计暂时不可用", "语义索引统计暂时不可用", "自动上下文反馈统计暂时不可用", "共享协作统计暂时不可用", "空间明细"} {
		if !strings.Contains(page, want) {
			t.Errorf("局部统计失败时概览缺少 %q", want)
		}
	}
}

func TestGovernanceTurnsExistingSignalsIntoScopedWork(t *testing.T) {
	gov := newFakeGov(governance.RoleAdmin, false)
	know := &fakeKnow{spaceStats: []knowledge.SpaceStats{
		{SpaceID: spPers, DraftCards: 3, ActiveCards: 4, ActiveUnqualifiedCards: 1},
		{SpaceID: spTeam, DraftCards: 7, ActiveCards: 8, ActiveUnqualifiedCards: 2},
	}, embedHealth: knowledge.EmbeddingHealth{Enabled: true, Stats: knowledge.EmbeddingStats{Pending: 2, Failed: 4, Exhausted: 1}}}
	lines := newFakeContinuity()
	lines.stats = continuity.LineStats{Active: 5, NeedsReview: 1, Stale: 2}
	activities := &fakeActivity{quality: activity.ContextFeedbackQuality{
		Wrong: 2, Corrected: 1, AbstainReasons: activity.ContextAbstainReasons{Ambiguous: 4},
	}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(NewHost(gov, know, lines, activities, logger, Options{
		Version: "test", ExternalConfigured: true,
		ExternalSources: func(context.Context, platform.ID) ([]external.SourceHealth, error) {
			return []external.SourceHealth{
				{Name: "healthy", LastSyncStatus: external.SyncSucceeded, LastSucceededAt: ptrTime(time.Now())},
				{Name: "failed", LastSyncStatus: external.SyncFailed},
			}, nil
		},
	}).Handler())
	t.Cleanup(srv.Close)

	page := body(t, get(t, srv, "/governance", true))
	for _, want := range []string{
		"治理待办", "审核待发布卡片", ">3</strong><span>项", "补齐已发布卡片", ">1</strong><span>项",
		"待复核 1 条，已陈旧 2 条", "错误 2、已纠正 1、因歧义放弃 4",
		"处理语义索引失败", ">4</strong><span>项", "恢复外部来源", ">1</strong><span>项",
	} {
		if !strings.Contains(page, want) {
			t.Errorf("治理待办缺少 %q", want)
		}
	}
	if strings.Contains(page, ">10</strong><span>项") {
		t.Fatal("审核待办不得包含只读团队空间的草稿")
	}
}

func TestGovernanceHidesAdminOperationsFromMembers(t *testing.T) {
	srv, _ := newTestWeb(t, governance.RoleMember, false)
	page := body(t, get(t, srv, "/governance", true))
	for _, hidden := range []string{"处理语义索引失败", "恢复外部来源"} {
		if strings.Contains(page, hidden) {
			t.Errorf("成员治理待办不应显示管理员运行项 %q", hidden)
		}
	}
}

func TestContextQualitySupportsBoundedWindowsAndHonestCopy(t *testing.T) {
	gov := newFakeGov(governance.RoleMember, false)
	activities := &fakeActivity{quality: activity.ContextFeedbackQuality{
		Selected: 20, Abstained: 6, FeedbackCoveredSelections: 5,
		AbstainReasons: activity.ContextAbstainReasons{NoRelevantEvidence: 2, Ambiguous: 1, SemanticUnavailable: 1, RetrievalTimeout: 1, SharedLineNotReady: 1},
		Delivered:      2, Used: 3, Ignored: 1, Wrong: 2, Corrected: 1,
	}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(NewHost(gov, &fakeKnow{}, newFakeContinuity(), activities, logger, Options{Version: "test"}).Handler())
	t.Cleanup(srv.Close)

	page := body(t, get(t, srv, "/context-quality?range=7d", true))
	for _, want := range []string{
		"自动上下文质量", "近 7 天选择与覆盖", "覆盖 25%", `progress max="20" value="5"`,
		"无相关证据", "候选歧义", "反馈结果", "没有反馈表示未知", "不能解释成自动上下文准确率",
		`href="/activity?action=gateway_context_feedback&amp;range=7d"`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("上下文质量页缺少 %q", want)
		}
	}
	if activities.qualityTenant != tenantA || activities.qualityWindow != 7*24*time.Hour {
		t.Fatalf("上下文质量窗口不正确: tenant=%s window=%s", activities.qualityTenant, activities.qualityWindow)
	}
}

func TestTokensGroupsAccessByAgentAndLinksToFilteredActivity(t *testing.T) {
	gov := newFakeGov(governance.RoleMember, false)
	lastUsed := time.Date(2026, 8, 30, 8, 0, 0, 0, time.UTC)
	gov.tokens = []governance.Token{{
		ID: "token", TenantID: tenantA, UserID: userMe, AgentID: "codex-local", Name: "MacBook", Prefix: "ccs", CreatedAt: lastUsed.Add(-time.Hour), LastUsedAt: &lastUsed,
	}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(NewHost(gov, &fakeKnow{}, newFakeContinuity(), &fakeActivity{}, logger, Options{Version: "test"}).Handler())
	t.Cleanup(srv.Close)

	page := body(t, get(t, srv, "/tokens", true))
	for _, want := range []string{"Agent 接入状态", "codex-local", "有效 token", "全部 token", "最近使用", `href="/activity?agent=codex-local"`, "Token 明细"} {
		if !strings.Contains(page, want) {
			t.Errorf("接入页缺少 %q", want)
		}
	}
}

func ptrTime(value time.Time) *time.Time { return &value }

func TestWorkspaceShellKeepsNavigationAndAccessibilityContracts(t *testing.T) {
	srv, _ := newTestWeb(t, governance.RoleAdmin, false)
	page := body(t, get(t, srv, "/", true))
	for _, want := range []string{
		`class="skip-link"`, `href="#main-content"`, `<aside class="sidebar">`,
		`<nav aria-label="主导航">`, `href="/governance"`, `href="/context-quality"`, `aria-current="page"`, `id="main-content"`,
		`role="status"`,
	} {
		if want == `role="status"` {
			// Overview 没有 notice 时不会渲染 live region；该属性在 layout 的
			// 条件分支里，由静态模板检查覆盖。
			continue
		}
		if !strings.Contains(page, want) {
			t.Errorf("工作区 shell 缺少 %s", want)
		}
	}
	if strings.Count(page, "<h1>") != 1 {
		t.Fatalf("页面应只有一个主标题，实际 %d", strings.Count(page, "<h1>"))
	}

	css := body(t, get(t, srv, "/static/app.css", false))
	for _, want := range []string{"@media (max-width: 680px)", "prefers-reduced-motion", ":focus-visible", "min-width: 320px"} {
		if !strings.Contains(css, want) {
			t.Errorf("响应式/可访问样式缺少 %q", want)
		}
	}
	if strings.Contains(css, "transition: all") || strings.Contains(css, "transition-property: all") {
		t.Fatal("界面样式不得使用 transition: all")
	}
}

func TestAdminSourcesPageQueuesOneAuditedSyncAndShowsReceipts(t *testing.T) {
	gov := newFakeGov(governance.RoleAdmin, false)
	act := &fakeActivity{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	finished := now.Add(-time.Minute)
	queued := 0
	srv := httptest.NewServer(NewHost(gov, &fakeKnow{}, newFakeContinuity(), act, logger, Options{
		Version: "test", Now: func() time.Time { return now }, ExternalConfigured: true,
		ExternalSources: func(context.Context, platform.ID) ([]external.SourceHealth, error) {
			return []external.SourceHealth{{Name: "atlas-docs", System: "shared-docs", LastSyncStatus: external.SyncSucceeded, LastSucceededAt: &finished, DocumentCount: 37}}, nil
		},
		ExternalSyncRuns: func(_ context.Context, tenantID platform.ID, limit int) ([]external.SyncRunReceipt, error) {
			if tenantID != tenantA || limit != externalReceiptLimit {
				t.Fatalf("unexpected receipt scope: tenant=%s limit=%d", tenantID, limit)
			}
			return []external.SyncRunReceipt{{ID: "run-1", SourceName: "atlas-docs", SourceSystem: "shared-docs", Status: external.SyncSucceeded, Seen: 37, Indexed: 4, Unchanged: 33, StartedAt: now.Add(-2 * time.Minute), FinishedAt: &finished}}, nil
		},
		QueueExternalSync: func(_ context.Context, tenantID platform.ID) (bool, error) {
			if tenantID != tenantA {
				t.Fatalf("unexpected queue tenant: %s", tenantID)
			}
			queued++
			return queued == 1, nil
		},
	}).Handler())
	t.Cleanup(srv.Close)

	page := body(t, get(t, srv, "/admin/sources", true))
	for _, want := range []string{"外部来源", "Atlas 同步", "开始同步", "atlas-docs", "37 份有效镜像", "最近运行", "新索引 4", "外部正文"} {
		if !strings.Contains(page, want) {
			t.Errorf("外部来源页缺少 %q", want)
		}
	}
	resp := post(t, srv, "/admin/sources/sync", url.Values{}, true, true)
	first := body(t, resp)
	if resp.StatusCode != http.StatusOK || !strings.Contains(first, "同步请求已记录") || queued != 1 {
		t.Fatalf("first queue request failed: status=%d queued=%d body=%.300s", resp.StatusCode, queued, first)
	}
	resp = post(t, srv, "/admin/sources/sync", url.Values{}, true, true)
	second := body(t, resp)
	if !strings.Contains(second, "已有一轮手动同步在等待") || queued != 2 {
		t.Fatalf("duplicate queue feedback missing: queued=%d body=%.300s", queued, second)
	}
	if len(act.records) != 2 || act.records[0].Action != "external.sync.queue" || act.records[0].Outcome != activity.OutcomeSuccess {
		t.Fatalf("manual sync queue must be audited without form data: records=%+v", act.records)
	}
}

func TestExternalSourceRequestStatusIsExplicit(t *testing.T) {
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	pending := now.Add(-time.Minute)
	active := now.Add(time.Minute)
	expired := now.Add(-time.Second)
	for _, tc := range []struct {
		name   string
		health external.SourceHealth
		want   string
	}{
		{name: "pending", health: external.SourceHealth{SyncRequestedAt: &pending}, want: "等待处理"},
		{name: "active claim", health: external.SourceHealth{SyncClaimed: true, SyncClaimUntil: &active}, want: "正在处理"},
		{name: "expired claim", health: external.SourceHealth{SyncClaimed: true, SyncClaimUntil: &expired}, want: "等待其他实例接管"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			if got := makeSourceHealthView(tc.health, true, now).Request; got != tc.want {
				t.Fatalf("request status = %q, want %q", got, tc.want)
			}
		})
	}
}

func TestAdminSystemPageReportsActualRuntimeShape(t *testing.T) {
	gov := newFakeGov(governance.RoleAdmin, false)
	know := &fakeKnow{embedHealth: knowledge.EmbeddingHealth{Enabled: true, Model: "bge-m3", Stats: knowledge.EmbeddingStats{Ready: 12, Failed: 1}}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	startedAt := time.Date(2026, 8, 29, 6, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(NewHost(gov, know, newFakeContinuity(), &fakeActivity{}, logger, Options{
		Version: "v-test", SourceDigest: strings.Repeat("a", 64), BinaryDigest: strings.Repeat("b", 64), Env: "development", RedisConfigured: true, EmbeddingProvider: "ollama",
		MySQLTransport: "TLS 1.2+，验证服务端身份", RedisTransport: "TLS 1.2+，验证服务端身份",
		EmbeddingTransport: "HTTPS，验证服务端身份", ExternalTransport: "HTTPS，验证服务端身份",
		DatabaseHealth: func(context.Context) error { return nil },
		SchemaHealth:   func(context.Context) (string, error) { return "0008", nil },
		DatabasePoolStats: func() DatabasePoolStats {
			return DatabasePoolStats{
				MaxOpenConnections: 20, OpenConnections: 19, InUse: 18, Idle: 1,
				WaitCount: 7, WaitDuration: 1500 * time.Millisecond,
				MaxIdleClosed: 2, MaxIdleTimeClosed: 3, MaxLifetimeClosed: 4,
			}
		},
		SchemaVersion:       "0008",
		StartedAt:           startedAt,
		Now:                 func() time.Time { return startedAt.Add(2*time.Hour + 3*time.Minute + 4*time.Second) },
		ShutdownDrain:       5 * time.Second,
		RedisHealth:         func(context.Context) error { return nil },
		EmbeddingCacheStats: func() (uint64, uint64, bool) { return 7, 3, true },
		LoginRateLimiter:    &scriptedRateLimiter{},
		LoginRateLimit:      10,
		LoginRateWindow:     15 * time.Minute,
		MCPRateLimit:        300,
		MCPTenantRateLimit:  3000,
		MCPRateWindow:       time.Minute,
		MetricsAddr:         "127.0.0.1:51892",
		HTTPRequestStats: func() HTTPRequestStats {
			return HTTPRequestStats{
				Available: true, Window: 5 * time.Minute, Requests: 42, ClientErrors: 3,
				ServerErrors: 1, Panics: 0, AverageLatency: 37 * time.Millisecond,
				P95Latency: 250 * time.Millisecond, InFlight: 2,
			}
		},
		BackupStatus: fakeBackupStatusReader{status: ports.BackupStatus{
			Database: "claracore_server", Archive: "claracore-server_claracore_server_20260829T070000Z.sql.gz",
			CreatedAt: startedAt.Add(time.Hour), VerifiedAt: startedAt.Add(time.Hour + time.Minute),
			MaxAge: 26 * time.Hour, IntegrityVerified: true, Recoverability: "not_proven",
		}},
		RestoreStatus: fakeRestoreStatusReader{status: ports.RestoreStatus{
			Database: "claracore_server", Archive: "claracore-server_claracore_server_20260822T090000Z.sql.gz",
			Report:     "claracore-server_claracore_server_20260822T090000Z.sql.gz.restore-report.tsv",
			RestoredAt: startedAt.Add(-6 * 24 * time.Hour), MaxAge: 30 * 24 * time.Hour,
			TableCount: 25, MigrationCount: 10, LatestMigration: "0010",
			TechnicalRestoreVerified: true, SemanticInspection: "not_proven",
		}},
		IntegrityStatus: fakeIntegrityStatusReader{status: ports.IntegrityStatus{
			Verified: true, SchemaVersion: "0008", CheckedAt: startedAt.Add(90 * time.Minute), MaxAge: 25 * time.Hour, RulesChecked: 29,
		}},
		TenantCapacity: fakeTenantCapacityReader{snapshot: ports.TenantCapacitySnapshot{
			MeasuredAt: startedAt.Add(2 * time.Hour), Members: 8, Agents: 11, Spaces: 5,
			Memories: 34, Cards: 21, RecallTriggers: 40, Labels: 18, Links: 7, KnowledgeLogicalBytes: 2048,
			ContinuityLines: 6, ContinuityPositions: 5, ContinuityHistory: 19, ContinuityHandoffs: 4, ContinuityLogicalBytes: 1024,
			Embeddings: 95, EmbeddingLogicalBytes: 512,
			ExternalSources: 1, ExternalSyncRuns: 9, ExternalDocuments: 37, ExternalVectors: 37, ExternalLogicalBytes: 256,
			AuditEvents: 123, AuditLogicalBytes: 128,
		}},
		ExternalConfigured: true,
		ExternalSources: func(context.Context, platform.ID) ([]external.SourceHealth, error) {
			succeeded := startedAt.Add(90 * time.Minute)
			return []external.SourceHealth{{
				Name: "atlas-docs", System: "shared-docs", LastSyncStatus: external.SyncSucceeded,
				LastSucceededAt: &succeeded, DocumentCount: 37,
			}}, nil
		},
	}).Handler())
	t.Cleanup(srv.Close)

	page := strings.ReplaceAll(body(t, get(t, srv, "/admin/system", true)), "&#43;", "+")
	for _, want := range []string{"系统状态", "v-test", strings.Repeat("a", 64), "aaaaaaaaaaaa", strings.Repeat("b", 64), "bbbbbbbbbbbb", "development", "迁移 0008", "与当前二进制一致", "连接正常", "TLS 1.2+，验证服务端身份", "HTTPS，验证服务端身份", "使用 18 / 20", "接近上限", "累计等待 7 次 / 1.5s", "累计回收 9", "2 小时 3 分钟", "等待 5s 摘流", "跨实例登录与 MCP 限流已启用，embedding 缓存与索引租约已启用", "IP 与账号组合 10 次/15 分钟", "每 agent 300 次/1 分钟", "每租户合计 3000 次/1 分钟", "token 与新增 agent 都不能绕过对应预算", "本机 127.0.0.1:51892 提供 Prometheus 文本指标", "多实例基础协调已装配", "HTTP 最近窗口", "存在服务端错误", "本实例5 分钟内完成 42", "4xx 3", "5xx 1", "panic 0", "P95上界 250ms", "平均 37ms", "当前在途 2", "bge-m3", "命中 7", "数据关系检查通过", "29 条固定规则", "最近备份按期完成", "claracore-server_claracore_server_20260829T070000Z.sql.gz", "年龄 1 小时 3 分钟 / 上限 26 小时", "完整性已验证；可恢复性尚未证明", "技术恢复演练有效", "claracore-server_claracore_server_20260822T090000Z.sql.gz", "年龄 6 天 2 小时 / 上限 30 天", "25 张表", "10 条迁移（最新 0010）", "技术恢复已验证", "代表性 Memory、Card 与 Shared Line 仍需人工抽查", "外部知识源", "atlas-docs", "shared-docs", "37 份有效镜像", "本地不保存外部正文", "同步正常", "存储与成员配额", "已有精确逻辑容量基线", "不等同于内容存储、成员数或账单配额", "租户容量快照", "3.9 KiB", "8 名成员", "11 个 Agent", "5 个空间", "Memory 34", "Card 21", "共同线 6", "语义索引", "95 行", "活动与审计", "123 行", "不等于 InnoDB 磁盘占用", "不会进入高频 Prometheus 抓取路径"} {
		if !strings.Contains(page, want) {
			t.Errorf("系统页缺少 %q", want)
		}
	}
}

func TestAdminSystemPageReportsIntegrityViolationStaleAndInvalid(t *testing.T) {
	gov := newFakeGov(governance.RoleAdmin, false)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name   string
		reader ports.IntegrityStatusReader
		want   []string
	}{
		{name: "violations", reader: fakeIntegrityStatusReader{status: ports.IntegrityStatus{SchemaVersion: "0010", CheckedAt: now.Add(-time.Hour), MaxAge: 25 * time.Hour, RulesChecked: 29, Violations: 8, Issues: []ports.IntegrityIssue{{Domain: "knowledge", Rule: "memory_tenant", Violations: 2}, {Domain: "knowledge", Rule: "memory_space", Violations: 2}, {Domain: "knowledge", Rule: "memory_author_user", Violations: 2}, {Domain: "knowledge", Rule: "memory_author_agent", Violations: 2}}}}, want: []string{"发现 8 次关系违规", "4 类", "knowledge.memory_tenant", "同一行可能命中多条规则"}},
		{name: "stale", reader: fakeIntegrityStatusReader{status: ports.IntegrityStatus{Verified: true, SchemaVersion: "0010", CheckedAt: now.Add(-26 * time.Hour), MaxAge: 25 * time.Hour, RulesChecked: 29}}, want: []string{"完整性检查已过期", "年龄 1 天 2 小时 / 上限 25 小时"}},
		{name: "invalid", reader: fakeIntegrityStatusReader{err: errors.New("bad receipt")}, want: []string{"无有效回执", "完整性 timer、oneshot"}},
		{name: "schema mismatch", reader: fakeIntegrityStatusReader{status: ports.IntegrityStatus{Verified: true, SchemaVersion: "0009", CheckedAt: now, MaxAge: 25 * time.Hour, RulesChecked: 27}}, want: []string{"回执版本不匹配", "回执 schema 0009", "当前二进制 schema 0010"}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			srv := httptest.NewServer(NewHost(gov, &fakeKnow{}, newFakeContinuity(), &fakeActivity{}, logger, Options{Now: func() time.Time { return now }, SchemaVersion: "0010", IntegrityStatus: tc.reader}).Handler())
			t.Cleanup(srv.Close)
			page := body(t, get(t, srv, "/admin/system", true))
			for _, want := range tc.want {
				if !strings.Contains(page, want) {
					t.Errorf("page missing %q", want)
				}
			}
		})
	}
}

func TestAdminSystemPageReportsStaleOrInvalidBackupReceipt(t *testing.T) {
	gov := newFakeGov(governance.RoleAdmin, false)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

	t.Run("stale", func(t *testing.T) {
		srv := httptest.NewServer(NewHost(gov, &fakeKnow{}, newFakeContinuity(), &fakeActivity{}, logger, Options{
			Now: func() time.Time { return now },
			BackupStatus: fakeBackupStatusReader{status: ports.BackupStatus{
				Database: "claracore_server", Archive: "claracore-server_claracore_server_20260827T060000Z.sql.gz",
				CreatedAt: now.Add(-54 * time.Hour), VerifiedAt: now.Add(-54*time.Hour + time.Minute),
				MaxAge: 26 * time.Hour, IntegrityVerified: true, Recoverability: "not_proven",
			}},
		}).Handler())
		t.Cleanup(srv.Close)
		page := body(t, get(t, srv, "/admin/system", true))
		for _, want := range []string{"最新备份已过期", "年龄 2 天 6 小时 / 上限 26 小时", "可恢复性尚未证明"} {
			if !strings.Contains(page, want) {
				t.Errorf("过期备份状态缺少 %q", want)
			}
		}
	})

	t.Run("invalid", func(t *testing.T) {
		srv := httptest.NewServer(NewHost(gov, &fakeKnow{}, newFakeContinuity(), &fakeActivity{}, logger, Options{
			Now:          func() time.Time { return now },
			BackupStatus: fakeBackupStatusReader{err: errors.New("malformed receipt")},
		}).Handler())
		t.Cleanup(srv.Close)
		page := body(t, get(t, srv, "/admin/system", true))
		for _, want := range []string{"无有效回执", "回执缺失、不可读或格式无效", "无法确认最新归档"} {
			if !strings.Contains(page, want) {
				t.Errorf("无效备份回执状态缺少 %q", want)
			}
		}
	})
}

func TestAdminSystemPageReportsStaleOrInvalidRestoreReceipt(t *testing.T) {
	gov := newFakeGov(governance.RoleAdmin, false)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	now := time.Date(2026, 8, 29, 12, 0, 0, 0, time.UTC)

	t.Run("stale", func(t *testing.T) {
		srv := httptest.NewServer(NewHost(gov, &fakeKnow{}, newFakeContinuity(), &fakeActivity{}, logger, Options{
			Now: func() time.Time { return now },
			RestoreStatus: fakeRestoreStatusReader{status: ports.RestoreStatus{
				RestoredAt: now.Add(-31 * 24 * time.Hour), MaxAge: 30 * 24 * time.Hour,
				TableCount: 25, MigrationCount: 10, LatestMigration: "0010",
				TechnicalRestoreVerified: true, SemanticInspection: "not_proven",
			}},
		}).Handler())
		t.Cleanup(srv.Close)
		page := body(t, get(t, srv, "/admin/system", true))
		for _, want := range []string{"恢复演练已过期", "年龄 31 天 / 上限 30 天", "技术恢复已验证", "仍需人工抽查"} {
			if !strings.Contains(page, want) {
				t.Errorf("过期恢复状态缺少 %q", want)
			}
		}
	})

	t.Run("invalid", func(t *testing.T) {
		srv := httptest.NewServer(NewHost(gov, &fakeKnow{}, newFakeContinuity(), &fakeActivity{}, logger, Options{
			Now:           func() time.Time { return now },
			RestoreStatus: fakeRestoreStatusReader{err: errors.New("malformed receipt")},
		}).Handler())
		t.Cleanup(srv.Close)
		page := body(t, get(t, srv, "/admin/system", true))
		for _, want := range []string{"无有效回执", "隔离恢复任务、回传路径", "无法确认归档曾完整导入"} {
			if !strings.Contains(page, want) {
				t.Errorf("无效恢复回执状态缺少 %q", want)
			}
		}
	})
}

func TestAdminSystemPageShowsPersistedMirrorWhenConnectorIsDisabled(t *testing.T) {
	gov := newFakeGov(governance.RoleAdmin, false)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	lastSuccess := time.Date(2026, 8, 29, 8, 0, 0, 0, time.UTC)
	srv := httptest.NewServer(NewHost(gov, &fakeKnow{}, newFakeContinuity(), &fakeActivity{}, logger, Options{
		ExternalConfigured: false,
		ExternalSources: func(context.Context, platform.ID) ([]external.SourceHealth, error) {
			return []external.SourceHealth{{Name: "atlas-docs", System: "shared-docs", LastSyncStatus: external.SyncSucceeded, LastSucceededAt: &lastSuccess, DocumentCount: 12}}, nil
		},
	}).Handler())
	t.Cleanup(srv.Close)
	page := body(t, get(t, srv, "/admin/system", true))
	for _, want := range []string{"仅镜像", "仅镜像（不刷新）", "12 份有效镜像", "连接关闭，检索继续但不会刷新", "atlas-docs"} {
		if !strings.Contains(page, want) {
			t.Errorf("镜像降级状态缺少 %q", want)
		}
	}
}

func TestAdminSystemPageReportsSchemaDriftAsUnsafe(t *testing.T) {
	gov := newFakeGov(governance.RoleAdmin, false)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(NewHost(gov, &fakeKnow{}, newFakeContinuity(), &fakeActivity{}, logger, Options{
		SchemaVersion:  "0008",
		DatabaseHealth: func(context.Context) error { return nil },
		SchemaHealth:   func(context.Context) (string, error) { return "", errors.New("checksum mismatch") },
	}).Handler())
	t.Cleanup(srv.Close)

	page := body(t, get(t, srv, "/admin/system", true))
	for _, want := range []string{"数据库迁移状态异常", "与当前二进制不一致", "不能安全接收流量"} {
		if !strings.Contains(page, want) {
			t.Errorf("schema 漂移页缺少 %q", want)
		}
	}
}

func TestAdminSystemPageReportsRedisFallback(t *testing.T) {
	gov := newFakeGov(governance.RoleAdmin, false)
	know := &fakeKnow{embedHealth: knowledge.EmbeddingHealth{Enabled: true, Model: "bge-m3"}}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(NewHost(gov, know, newFakeContinuity(), &fakeActivity{}, logger, Options{
		RedisConfigured: true, EmbeddingProvider: "ollama",
		DatabaseHealth:   func(context.Context) error { return nil },
		RedisHealth:      func(context.Context) error { return errors.New("unreachable") },
		LoginRateLimiter: &scriptedRateLimiter{},
	}).Handler())
	t.Cleanup(srv.Close)

	page := body(t, get(t, srv, "/admin/system", true))
	for _, want := range []string{"Redis 当前不可用", "已配置但当前不可达", "登录与 MCP 限流退回进程内保护", "多实例协调当前降级", "核心读写不受影响"} {
		if !strings.Contains(page, want) {
			t.Errorf("Redis 降级状态缺少 %q", want)
		}
	}
}

func TestAdminSystemPageDoesNotOverclaimMultiInstanceWithoutRedis(t *testing.T) {
	gov := newFakeGov(governance.RoleAdmin, false)
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(NewHost(gov, &fakeKnow{}, newFakeContinuity(), &fakeActivity{}, logger, Options{
		EmbeddingProvider: "disabled",
		DatabaseHealth:    func(context.Context) error { return nil },
		LoginRateLimiter:  &scriptedRateLimiter{},
		LoginRateLimit:    10,
		LoginRateWindow:   15 * time.Minute,
	}).Handler())
	t.Cleanup(srv.Close)

	page := body(t, get(t, srv, "/admin/system", true))
	for _, want := range []string{"登录与 MCP 限流使用进程内计数", "IP 与账号组合 10 次/15 分钟", "单实例完整；多实例需配置 Redis"} {
		if !strings.Contains(page, want) {
			t.Errorf("无 Redis 的运行形态缺少 %q", want)
		}
	}
	if strings.Contains(page, "多实例基础协调已装配") {
		t.Fatal("未配置 Redis 时不得声称多实例协调已装配")
	}
}

// TestTemplatesEscapeUserInput 确认没人把 html/template 换成 text/template。
func TestTemplatesEscapeUserInput(t *testing.T) {
	srv, gov := newTestWeb(t, governance.RoleAdmin, false)
	gov.users = []governance.User{{
		ID: userOther, TenantID: tenantA, Username: "tongshi",
		DisplayName: `<script>alert(1)</script>`, Role: governance.RoleMember, Status: governance.StatusActive,
	}}

	page := body(t, get(t, srv, "/admin/members", true))
	if strings.Contains(page, "<script>alert(1)</script>") {
		t.Fatal("用户输入必须被转义——这条红了说明有人把 html/template 换成了 text/template")
	}
	if !strings.Contains(page, "&lt;script&gt;") {
		t.Fatal("应当出现转义后的内容")
	}
}

func TestSecurityHeadersArePresent(t *testing.T) {
	srv, _ := newTestWeb(t, governance.RoleAdmin, false)

	resp := get(t, srv, "/login", false)
	defer resp.Body.Close()
	csp := resp.Header.Get("Content-Security-Policy")
	for _, want := range []string{
		"default-src 'none'", "script-src 'self'", "style-src 'self'",
		"form-action 'self'", "frame-ancestors 'none'", "base-uri 'none'", "object-src 'none'",
	} {
		if !strings.Contains(csp, want) {
			t.Fatalf("CSP 缺少 %q，实际 %q", want, csp)
		}
	}
	if strings.Contains(csp, "default-src 'self'") {
		t.Fatalf("CSP 不应回退到宽泛同源默认，实际 %q", csp)
	}
	// 模板是纯 SSR，没有内联脚本也不打算有——CSP 里出现 unsafe-inline
	// 说明有人开始往页面里塞 <script> 了
	if strings.Contains(csp, "unsafe-inline") {
		t.Fatal("CSP 不该有 unsafe-inline：前后端不分离是明确决定，别从一行内联脚本开始滑坡")
	}
	if resp.Header.Get("X-Content-Type-Options") != "nosniff" {
		t.Fatal("缺少 nosniff")
	}
	for name, want := range map[string]string{
		"X-Frame-Options":                   "DENY",
		"Referrer-Policy":                   "no-referrer",
		"Cross-Origin-Opener-Policy":        "same-origin",
		"Cross-Origin-Resource-Policy":      "same-origin",
		"X-Permitted-Cross-Domain-Policies": "none",
		"Cache-Control":                     "no-store, private",
		"Pragma":                            "no-cache",
		"X-Robots-Tag":                      "noindex, nofollow, noarchive",
	} {
		if got := resp.Header.Get(name); got != want {
			t.Errorf("%s = %q, want %q", name, got, want)
		}
	}
	permissions := resp.Header.Get("Permissions-Policy")
	for _, disabled := range []string{"camera=()", "geolocation=()", "microphone=()", "payment=()", "usb=()"} {
		if !strings.Contains(permissions, disabled) {
			t.Errorf("Permissions-Policy missing %q: %q", disabled, permissions)
		}
	}
}

func TestStaticAssetsUseOnlyShortPublicCache(t *testing.T) {
	srv, _ := newTestWeb(t, governance.RoleAdmin, false)
	resp := get(t, srv, "/static/app.js", false)
	defer resp.Body.Close()
	if got := resp.Header.Get("Cache-Control"); got != "public, max-age=300" {
		t.Fatalf("static cache = %q", got)
	}
	if got := resp.Header.Get("X-Robots-Tag"); got != "" {
		t.Fatalf("static asset unexpectedly has page indexing policy: %q", got)
	}
}

func TestConsequentialActionsHaveExplicitConfirmation(t *testing.T) {
	srv, gov := newTestWeb(t, governance.RoleAdmin, false)
	gov.tokens = []governance.Token{{ID: "01TOKENAAAAAAAAAAAAAAAAAAA", Name: "MacBook", Prefix: "ccs_test"}}
	gov.permissions = []governance.Permission{{SpaceID: spTeam, UserID: userOther, CanRead: true}}
	for _, path := range []string{"/tokens", "/admin/members", "/admin/spaces", "/continuity/01LINEAAAAAAAAAAAAAAAAAAAA", "/review/01CARDAAAAAAAAAAAAAAAAAAAA"} {
		page, errMsg := getBody(t, srv, path)
		if errMsg != "" {
			t.Fatalf("%s: %s", path, errMsg)
		}
		if !strings.Contains(page, "data-confirm-message=") {
			t.Fatalf("%s 的高影响操作缺少二次确认", path)
		}
	}
}

func (f *fakeKnow) ListContents(_ context.Context, in knowledge.ListContentsInput) (knowledge.ListContentsResult, error) {
	f.lastList = in
	f.calls++
	return knowledge.ListContentsResult{Items: f.items, Total: f.total, NextCursor: f.nextCursor}, nil
}

func (f *fakeKnow) GetCard(_ context.Context, _, _ platform.ID, spaces []platform.ID) (knowledge.Card, []knowledge.Link, error) {
	f.calls++
	if f.getErr != nil {
		return knowledge.Card{}, nil, f.getErr
	}
	f.lastList.Spaces = spaces
	return f.card, nil, nil
}

func (f *fakeKnow) GetMemory(_ context.Context, _, _ platform.ID, spaces []platform.ID) (knowledge.Memory, []knowledge.Link, error) {
	f.calls++
	if f.getErr != nil {
		return knowledge.Memory{}, nil, f.getErr
	}
	f.lastList.Spaces = spaces
	return f.memory, nil, nil
}

func (f *fakeKnow) FindCardDuplicateCandidates(_ context.Context, _, _ platform.ID, _ []platform.ID) ([]knowledge.CardDuplicateCandidate, error) {
	return f.duplicates, nil
}

func (f *fakeKnow) UpdateCard(_ context.Context, in knowledge.UpdateCardInput) (knowledge.Card, error) {
	f.lastUpdate = in
	f.calls++
	if f.updateErr != nil {
		return knowledge.Card{}, f.updateErr
	}
	return f.card, nil
}

func (f *fakeKnow) DeprecateDraftCards(_ context.Context, in knowledge.DeprecateDraftCardsInput) ([]knowledge.Card, error) {
	f.lastBatch = in
	f.calls++
	if f.batchErr != nil {
		return nil, f.batchErr
	}
	out := make([]knowledge.Card, 0, len(in.Cards))
	for _, item := range in.Cards {
		card := f.card
		card.ID, card.Status, card.Version = item.ID, knowledge.CardDeprecated, item.Version+1
		out = append(out, card)
	}
	return out, nil
}

func (f *fakeKnow) Ingest(_ context.Context, in knowledge.IngestInput) (knowledge.IngestReport, error) {
	f.lastIngest = in
	f.calls++
	return f.ingestReport, f.ingestErr
}

func (f *fakeKnow) EmbeddingHealth(_ context.Context, _ platform.ID, _ int) (knowledge.EmbeddingHealth, error) {
	f.calls++
	return f.embedHealth, f.ingestErr
}

func (f *fakeKnow) BackfillEmbeddings(_ context.Context, _ platform.ID, _ int) (knowledge.BackfillReport, error) {
	f.calls++
	return f.backfill, f.ingestErr
}

func (f *fakeKnow) RetryEmbeddingFailures(_ context.Context, _ platform.ID) (int, error) {
	f.calls++
	return f.retried, f.ingestErr
}

// ---------------------------------------------------------------------------
// 卡片审核：Web 上唯一的内容写入场景
// ---------------------------------------------------------------------------

// TestReviewQueueUsesWritableSpaces 盯住「能写就能审」这条。
//
// 用可读集合的话，人会看到一堆自己按不动的按钮；更糟的是，
// 那意味着权限判断在提交时才发生，而那时他已经填了一屏字。
func TestReviewQueueUsesWritableSpaces(t *testing.T) {
	srv, _, know := newContentWeb(t, governance.RoleMember)

	if _, errMsg := getBody(t, srv, "/review"); errMsg != "" {
		t.Fatal(errMsg)
	}
	in := know.lastList
	if in.Kind != knowledge.EntityCard {
		t.Fatalf("审核队列只看卡片，实际 %q", in.Kind)
	}
	// 只查 draft：队列的全部意义就是「还没人过目的那些」
	if len(in.Statuses) != 1 || in.Statuses[0] != knowledge.CardDraft {
		t.Fatalf("审核队列应只查 draft，实际 %v", in.Statuses)
	}
	// fakeGov 里这个人只对个人空间可写（team 是隐含可读、写需授权）
	for _, id := range in.Spaces {
		if id == spTeam {
			t.Fatal("团队空间只可读不可写，不该出现在审核队列的空间集合里")
		}
	}
}

func TestReviewQueueOffersOnlyExplicitBatchDeprecation(t *testing.T) {
	srv, _, know := newContentWeb(t, governance.RoleMember)
	know.items = []knowledge.ContentItem{{
		Ref:     knowledge.Ref{Type: knowledge.EntityCard, ID: "01CARDAAAAAAAAAAAAAAAAAAAA"},
		SpaceID: spPers, Title: "部署回滚检查", Status: knowledge.CardDraft, Version: 7,
	}}
	page, errMsg := getBody(t, srv, "/review")
	if errMsg != "" {
		t.Fatal(errMsg)
	}
	for _, want := range []string{
		`action="/review/batch-deprecate"`, `name="selection"`, `value="01CARDAAAAAAAAAAAAAAAAAAAA:7"`,
		`选择“部署回滚检查”标记为不需要`, `data-confirm-message=`, `发布仍需逐张`,
	} {
		if !strings.Contains(page, want) {
			t.Errorf("批量审核页缺少 %q", want)
		}
	}
	if strings.Contains(page, "批量发布") {
		t.Fatal("批量操作不得绕过逐张理解与召回条件审核")
	}
}

func TestReviewBatchDeprecateUsesVersionsWritableScopeAndPreservesFilter(t *testing.T) {
	srv, _, know := newContentWeb(t, governance.RoleMember)
	resp := post(t, srv, "/review/batch-deprecate", url.Values{
		"selection": {"01CARDAAAAAAAAAAAAAAAAAAAA:3", "01CARDBBBBBBBBBBBBBBBBBBB:9"},
		"space":     {string(spPers)},
	}, true, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("成功应回到队列，实际 %d", resp.StatusCode)
	}
	if got := resp.Header.Get("Location"); got != "/review?batch=2&space="+url.QueryEscape(string(spPers)) {
		t.Fatalf("应保留筛选并显示结果数量，实际 %q", got)
	}
	if know.lastBatch.TenantID != tenantA || len(know.lastBatch.Cards) != 2 {
		t.Fatalf("批量输入没有完整下推: %+v", know.lastBatch)
	}
	if know.lastBatch.Cards[0].Version != 3 || know.lastBatch.Cards[1].Version != 9 {
		t.Fatalf("每张卡的乐观锁版本必须下推: %+v", know.lastBatch.Cards)
	}
	for _, id := range know.lastBatch.WritableSpaces {
		if id == spTeam {
			t.Fatal("只读团队空间不得进入批量写范围")
		}
	}
}

func TestReviewBatchDeprecateFailsClosed(t *testing.T) {
	t.Run("empty selection", func(t *testing.T) {
		srv, _, know := newContentWeb(t, governance.RoleMember)
		resp := post(t, srv, "/review/batch-deprecate", url.Values{}, true, true)
		defer resp.Body.Close()
		if resp.StatusCode != http.StatusBadRequest || len(know.lastBatch.Cards) != 0 {
			t.Fatalf("空选择必须在领域调用前拒绝: status=%d batch=%+v", resp.StatusCode, know.lastBatch)
		}
	})

	t.Run("stale version", func(t *testing.T) {
		srv, _, know := newContentWeb(t, governance.RoleMember)
		know.batchErr = &knowledge.VersionConflictError{Expected: 1, Current: 2}
		resp := post(t, srv, "/review/batch-deprecate", url.Values{
			"selection": {"01CARDAAAAAAAAAAAAAAAAAAAA:1"},
		}, true, true)
		page := body(t, resp)
		if resp.StatusCode != http.StatusConflict || !strings.Contains(page, "整批均未处理") {
			t.Fatalf("并发冲突必须明确整批回滚: status=%d body=%q", resp.StatusCode, page)
		}
	})
}

func TestReviewPublishGoesThroughUpdateCard(t *testing.T) {
	srv, _, know := newContentWeb(t, governance.RoleMember)

	resp := post(t, srv, "/review/01CARDAAAAAAAAAAAAAAAAAAAA", url.Values{
		"action": {"publish"}, "title": {"审过之后的标题"}, "cardType": {"协作安全"},
		"understanding": {"我想明白的那一层"}, "triggers": {"第一条\n\n第二条\n"}, "version": {"1"},
	}, true, true)
	defer resp.Body.Close()

	in := know.lastUpdate
	if in.Status == nil || *in.Status != knowledge.CardActive {
		t.Fatalf("确认发布应把状态改成 active，实际 %v", in.Status)
	}
	// 审核就是一次带写权限校验的更新，没有独立的审核实体
	if len(in.WritableSpaces) == 0 {
		t.Fatal("必须用可写集合定位")
	}
	// 摄取给的标题是文件名、类型一律「摄取原料」，不让改的话
	// 审完的卡类型全是一个值，card_search 的过滤就废了
	if in.Title == nil || *in.Title != "审过之后的标题" {
		t.Fatal("审核时应能改标题")
	}
	if in.CardType == nil || *in.CardType != "协作安全" {
		t.Fatal("审核时应能改卡片类型")
	}
	if in.RecallTriggers == nil || len(*in.RecallTriggers) != 2 {
		t.Fatalf("召回条件应按行拆分并去掉空行，实际 %v", in.RecallTriggers)
	}
}

// TestReviewRejectUsesDeprecated 锁住退回语义。
//
// 留在 draft 的话，几千条永远挂着，「还有 N 条待审」那个数字
// 就失去意义了——而那个数字正是审核队列存在的理由。
func TestReviewRejectUsesDeprecated(t *testing.T) {
	srv, _, know := newContentWeb(t, governance.RoleMember)

	resp := post(t, srv, "/review/01CARDAAAAAAAAAAAAAAAAAAAA", url.Values{
		"action": {"reject"}, "title": {"不需要的文档"}, "cardType": {"摄取原料"}, "version": {"1"},
	}, true, true)
	defer resp.Body.Close()

	if know.lastUpdate.Status == nil || *know.lastUpdate.Status != knowledge.CardDeprecated {
		t.Fatalf("标记不需要应落成 deprecated，实际 %v", know.lastUpdate.Status)
	}
}

func TestReviewRejectsUnknownAction(t *testing.T) {
	srv, _, know := newContentWeb(t, governance.RoleMember)

	resp := post(t, srv, "/review/01CARDAAAAAAAAAAAAAAAAAAAA", url.Values{
		"action": {"delete-everything"}, "version": {"1"},
	}, true, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusBadRequest {
		t.Fatalf("未知动作应被拒，实际 %d", resp.StatusCode)
	}
	if know.lastUpdate.Status != nil {
		t.Fatal("未知动作不该走到领域层")
	}
}

// TestReviewKeepsInputOnValidationError 是一条体验属性，但它有真实后果。
//
// 把人填了一屏的字丢掉，他下次就不敢再点那个按钮了——而审核
// 本来就是最容易被拖延的事。
func TestReviewKeepsInputOnValidationError(t *testing.T) {
	srv, _, know := newContentWeb(t, governance.RoleMember)
	know.updateErr = errors.New("knowledge: 参数非法: 卡片至少要有一个 section 有内容")

	resp := post(t, srv, "/review/01CARDAAAAAAAAAAAAAAAAAAAA", url.Values{
		"action": {"publish"}, "title": {"我刚打的标题"},
		"understanding": {"我刚打的理解"}, "triggers": {"我刚写的召回条件"}, "version": {"1"},
	}, true, true)
	page := body(t, resp)

	for _, typed := range []string{"我刚打的标题", "我刚打的理解", "我刚写的召回条件"} {
		if !strings.Contains(page, typed) {
			t.Fatalf("校验失败后必须把人填的内容放回表单，丢了 %q", typed)
		}
	}
}

func TestReviewRefusesNonDraftCard(t *testing.T) {
	srv, _, know := newContentWeb(t, governance.RoleMember)
	know.card.Status = knowledge.CardActive

	page, errMsg := getBody(t, srv, "/review/01CARDAAAAAAAAAAAAAAAAAAAA")
	if errMsg != "" {
		t.Fatal(errMsg)
	}
	if strings.Contains(page, "name=\"action\"") || !strings.Contains(page, "不可在 Web 中编辑") {
		t.Fatal("非草稿只能只读查看，不应渲染审核提交按钮")
	}

	resp := post(t, srv, "/review/01CARDAAAAAAAAAAAAAAAAAAAA", url.Values{
		"action": {"publish"}, "version": {"1"},
	}, true, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("非草稿提交应返回 409，实际 %d", resp.StatusCode)
	}
	if know.lastUpdate.ID != "" {
		t.Fatal("非草稿提交不应进入领域更新")
	}
}

func TestReviewRejectsStaleVersion(t *testing.T) {
	srv, _, know := newContentWeb(t, governance.RoleMember)
	resp := post(t, srv, "/review/01CARDAAAAAAAAAAAAAAAAAAAA", url.Values{
		"action": {"publish"}, "version": {"0"},
	}, true, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusConflict {
		t.Fatalf("过期版本提交应返回 409，实际 %d", resp.StatusCode)
	}
	if know.lastUpdate.ID != "" {
		t.Fatal("过期版本不应进入领域更新")
	}
}

func TestReviewDetailNotFoundIsNot500(t *testing.T) {
	srv, _, know := newContentWeb(t, governance.RoleMember)
	know.getErr = knowledge.ErrNotFound

	resp := get(t, srv, "/review/01NOSUCHCARDAAAAAAAAAAAAAA", true)
	defer resp.Body.Close()
	// 不区分「不存在」与「无权访问」：区分等于让任何人用一个 ID
	// 就能探测出「这条内容存在于某个我看不见的空间」
	if resp.StatusCode != http.StatusNotFound {
		t.Fatalf("取不到应是 404，实际 %d", resp.StatusCode)
	}
}

// ---------------------------------------------------------------------------
// 内容浏览
// ---------------------------------------------------------------------------

func TestBrowseUsesReadableSpacesAndShowsAllStatuses(t *testing.T) {
	srv, _, know := newContentWeb(t, governance.RoleMember)

	if _, errMsg := getBody(t, srv, "/browse"); errMsg != "" {
		t.Fatal(errMsg)
	}
	in := know.lastList
	// 浏览用可读集合——看得见但改不了是完全正常的状态
	if len(in.Spaces) < 2 {
		t.Fatalf("应覆盖全部可读空间（含只读的团队空间），实际 %v", in.Spaces)
	}
	// 不传状态 = 全部状态。治理面必须看得到 draft 与 deprecated，
	// 那正是它存在的理由
	if len(in.Statuses) != 0 {
		t.Fatalf("默认不该过滤状态，实际 %v", in.Statuses)
	}
	if in.Kind != knowledge.EntityCard {
		t.Fatalf("默认看卡片（核心载体），实际 %q", in.Kind)
	}
}

func TestBrowseHonoursFilters(t *testing.T) {
	srv, _, know := newContentWeb(t, governance.RoleMember)

	if _, errMsg := getBody(t, srv, "/browse?kind=memory&status=archived&space="+string(spPers)); errMsg != "" {
		t.Fatal(errMsg)
	}
	in := know.lastList
	if in.Kind != knowledge.EntityMemory {
		t.Fatalf("类型筛选应生效，实际 %q", in.Kind)
	}
	if len(in.Statuses) != 1 || in.Statuses[0] != knowledge.MemoryArchived {
		t.Fatalf("状态筛选应生效，实际 %v", in.Statuses)
	}
	// 空间筛选传给领域层，由它确认这个空间在可读集合里——
	// 页面不能自己拿参数去查
	if in.SpaceID != spPers {
		t.Fatalf("空间筛选应传下去，实际 %q", in.SpaceID)
	}
}

func TestBrowseFullTextQueryIsBoundedAndPreservedInPagination(t *testing.T) {
	srv, _, know := newContentWeb(t, governance.RoleMember)
	know.items = []knowledge.ContentItem{{Ref: knowledge.Ref{Type: knowledge.EntityCard, ID: "01CARDAAAAAAAAAAAAAAAAAAAA"}, Title: "部署回滚检查"}}
	know.total = 1
	know.nextCursor = "cursor+/="

	page := body(t, get(t, srv, "/browse?q="+url.QueryEscape("部署 回滚"), true))
	if know.lastList.Query != "部署 回滚" {
		t.Fatalf("query should reach domain service: %q", know.lastList.Query)
	}
	if !strings.Contains(page, `type="search"`) || !strings.Contains(page, `maxlength="200"`) || !strings.Contains(page, `role="status"`) {
		t.Fatalf("search form should have a visible label, bounded native control, and announced count: %s", page)
	}
	if !strings.Contains(page, `value="部署 回滚"`) {
		t.Fatal("query should be preserved in the rendered form")
	}
	unescapedPage := html.UnescapeString(page)
	if !strings.Contains(unescapedPage, `q=%E9%83%A8%E7%BD%B2+%E5%9B%9E%E6%BB%9A`) || !strings.Contains(unescapedPage, `cursor=cursor%2B%2F%3D`) {
		t.Fatalf("pagination link should preserve and URL-encode query and cursor: %s", page)
	}
}

func TestBrowseShowsSpaceAuthorAndPreservesListContext(t *testing.T) {
	srv, _, know := newContentWeb(t, governance.RoleMember)
	agentID := platform.ID("01AGENTAAAAAAAAAAAAAAAAAAA")
	know.items = []knowledge.ContentItem{{
		Ref: knowledge.Ref{Type: knowledge.EntityCard, ID: "01CARDAAAAAAAAAAAAAAAAAAAA"}, SpaceID: spPers,
		Title: "带来源的卡片", Status: knowledge.CardActive, AuthorUserID: userMe, AuthorAgentID: &agentID,
	}}
	page := body(t, get(t, srv, "/browse?kind=card&q="+url.QueryEscape("部署 回滚")+"&space="+string(spPers), true))
	for _, want := range []string{"毛仔", string(agentID), "back="} {
		if !strings.Contains(page, want) {
			t.Errorf("内容列表缺少 %q", want)
		}
	}
	back := "/browse?kind=card&q=" + url.QueryEscape("部署 回滚") + "&space=" + string(spPers)
	detail := body(t, get(t, srv, "/browse/01CARDAAAAAAAAAAAAAAAAAAAA?kind=card&back="+url.QueryEscape(back), true))
	if !strings.Contains(html.UnescapeString(detail), `href="`+back+`"`) {
		t.Fatalf("详情返回链接应保留原筛选，实际 %s", detail)
	}
}

func TestBrowseFilterFormAutoSubmitsWithoutInlineScript(t *testing.T) {
	srv, _, _ := newContentWeb(t, governance.RoleMember)

	page := body(t, get(t, srv, "/browse", true))
	if strings.Contains(page, "onchange=") {
		t.Fatal("筛选不能依赖被 CSP 禁止的内联脚本")
	}
	if !strings.Contains(page, `type="submit">应用筛选</button>`) {
		t.Fatal("禁用 JavaScript 时仍必须提供可见的提交按钮")
	}
	if strings.Count(page, "data-auto-submit") < 3 || !strings.Contains(page, `<script src="/static/app.js" defer></script>`) {
		t.Fatal("类型、空间、状态选择应由同源外部脚本立即提交")
	}
	script, err := os.ReadFile("static/app.js")
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(script), `control.name === "kind"`) {
		t.Fatal("switching content kind should clear the previous kind's incompatible status before auto-submit")
	}
}

func TestReviewFilterFormWorksWithoutInlineScript(t *testing.T) {
	srv, _, _ := newContentWeb(t, governance.RoleMember)

	page := body(t, get(t, srv, "/review", true))
	if strings.Contains(page, "onchange=") {
		t.Fatal("审核队列筛选不能依赖被 CSP 禁止的内联脚本")
	}
	if !strings.Contains(page, `type="submit">应用筛选</button>`) {
		t.Fatal("审核队列必须提供可见的提交按钮")
	}
	if !strings.Contains(page, "data-auto-submit") {
		t.Fatal("审核队列选择空间后应立即提交")
	}
}

func TestFilterEnhancementScriptIsServed(t *testing.T) {
	srv, _ := newTestWeb(t, governance.RoleMember, false)
	resp := get(t, srv, "/static/app.js", false)
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("筛选脚本状态码 = %d", resp.StatusCode)
	}
	page := body(t, resp)
	if !strings.Contains(page, "requestSubmit") || strings.Contains(page, "innerHTML") {
		t.Fatalf("筛选脚本应只做表单提交，实际 %q", page)
	}
}

func TestContinuityPageShowsSharedLinesAndFilters(t *testing.T) {
	gov := newFakeGov(governance.RoleMember, false)
	lines := newFakeContinuity()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(NewHost(gov, &fakeKnow{}, lines, &fakeActivity{}, logger, Options{Version: "test"}).Handler())
	t.Cleanup(srv.Close)

	page := body(t, get(t, srv, "/continuity?status=all", true))
	for _, want := range []string{"共同线", "ClaraCore 联调收口", "共同线 Web 入口与筛选交互正在验证。", "部署后人工刷新页面确认。", "data-auto-submit", "/continuity/01LINEAAAAAAAAAAAAAAAAAAAA"} {
		if !strings.Contains(page, want) {
			t.Errorf("共同线页面缺少 %q", want)
		}
	}
	if lines.lastInput.TenantID != tenantA || !lines.lastInput.AllAgents || lines.lastInput.Status != "all" {
		t.Fatalf("共同线查询边界错误: %+v", lines.lastInput)
	}
}

func TestContinuityPagePaginatesWithoutSilentTruncation(t *testing.T) {
	gov := newFakeGov(governance.RoleMember, false)
	lines := newFakeContinuity()
	base := lines.lines[0]
	lines.lines = make([]continuity.LineSummary, 62)
	for i := range lines.lines {
		lines.lines[i] = base
		lines.lines[i].ID = platform.ID(fmt.Sprintf("line-%02d", i))
		lines.lines[i].Title = fmt.Sprintf("共同线 %02d", i)
	}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(NewHost(gov, &fakeKnow{}, lines, &fakeActivity{}, logger, Options{Version: "test"}).Handler())
	t.Cleanup(srv.Close)

	page := body(t, get(t, srv, "/continuity?status=all&cursor=page-two", true))
	if lines.lastInput.Cursor != "page-two" || lines.lastInput.Limit != 30 {
		t.Fatalf("第二页应使用不透明游标，实际 %+v", lines.lastInput)
	}
	if !strings.Contains(page, "回到第一页") || !strings.Contains(page, "下一页") {
		t.Fatal("共同线分页应显示第一页与下一页导航")
	}
}

func TestContinuityDetailShowsPositionAndHandoffHistory(t *testing.T) {
	gov := newFakeGov(governance.RoleMember, false)
	lines := newFakeContinuity()
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(NewHost(gov, &fakeKnow{}, lines, &fakeActivity{}, logger, Options{Version: "test"}).Handler())
	t.Cleanup(srv.Close)

	page := body(t, get(t, srv, "/continuity/01LINEAAAAAAAAAAAAAAAAAAAA", true))
	for _, want := range []string{"当前位置", "位置历史", "交接历史", "上线共同线治理页", "列表完成", "人工验收", "归档共同线"} {
		if !strings.Contains(page, want) {
			t.Errorf("共同线详情缺少 %q", want)
		}
	}
	if lines.lastGet.TenantID != tenantA || lines.lastGet.LineID != "01LINEAAAAAAAAAAAAAAAAAAAA" || lines.lastGet.AgentID != webContinuityAgentID {
		t.Fatalf("共同线详情查询边界错误: %+v", lines.lastGet)
	}
}

func TestContinuityStatusWriteUsesCSRFAndGovernanceAudit(t *testing.T) {
	gov := newFakeGov(governance.RoleMember, false)
	lines := newFakeContinuity()
	act := &fakeActivity{}
	logger := slog.New(slog.NewTextHandler(io.Discard, nil))
	srv := httptest.NewServer(NewHost(gov, &fakeKnow{}, lines, act, logger, Options{Version: "test"}).Handler())
	t.Cleanup(srv.Close)

	resp := post(t, srv, "/continuity/01LINEAAAAAAAAAAAAAAAAAAAA/status", url.Values{"status": {continuity.LineArchived}}, true, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusSeeOther {
		t.Fatalf("status=%d", resp.StatusCode)
	}
	if lines.lastStatus.TenantID != tenantA || lines.lastStatus.LineID != "01LINEAAAAAAAAAAAAAAAAAAAA" || lines.lastStatus.Status != continuity.LineArchived {
		t.Fatalf("status input boundary error: %+v", lines.lastStatus)
	}
	if len(act.records) != 1 || act.records[0].Action != "shared_line.status" || act.records[0].EntityID == nil || *act.records[0].EntityID != "01LINEAAAAAAAAAAAAAAAAAAAA" {
		t.Fatalf("status write should leave a scoped governance audit: %+v", act.records)
	}
}

func TestBrowseDetailWarnsAboutMissingTriggers(t *testing.T) {
	srv, _, _ := newContentWeb(t, governance.RoleMember)

	page := body(t, get(t, srv, "/browse/01CARDAAAAAAAAAAAAAAAAAAAA?kind=card", true))
	// 一张没有召回条件的卡永远不会被 card_recall 命中——
	// 那是这个产品的核心能力，页面必须说出来
	if !strings.Contains(page, "card_recall 永远命不中") {
		t.Fatal("没有召回条件的卡应当被显著标出")
	}
	if !strings.Contains(page, "docs/x.md") {
		t.Fatal("应标注来源——原创/提升自/摄取进来的，三者可信度不同")
	}
}

func getBody(t *testing.T, srv *httptest.Server, path string) (string, string) {
	t.Helper()
	resp := get(t, srv, path, true)
	if resp.StatusCode >= 400 {
		defer resp.Body.Close()
		return "", "HTTP " + resp.Status
	}
	return body(t, resp), ""
}

// ---------------------------------------------------------------------------
// 活动流与完整审计
// ---------------------------------------------------------------------------

func TestActivityPassesOnlyReadableSharedSpaces(t *testing.T) {
	srv, _, _, act := newActivityWeb(t, governance.RoleMember)
	agentID := platform.ID("01AGENTAAAAAAAAAAAAAAAAAAA")
	act.visiblePage = activity.Page{Events: []activity.Event{{
		ID: "01AUDITAAAAAAAAAAAAAAAAAAA", TenantID: tenantA, UserID: userMe,
		AgentID: &agentID, Source: activity.SourceMCP, Kind: activity.KindTrace,
		Access: activity.AccessRead, Outcome: activity.OutcomeSuccess,
		Action: "card_recall", CreatedAt: time.Now().UTC(),
	}}}

	page := body(t, get(t, srv, "/activity", true))
	if !strings.Contains(page, "card_recall") {
		t.Fatal("活动页应渲染领域层返回的事件")
	}
	if act.visibleInput.TenantID != tenantA || act.visibleInput.UserID != userMe {
		t.Fatalf("可见性查询身份不对: %+v", act.visibleInput)
	}
	if len(act.visibleInput.ReadableSharedSpaceIDs) != 1 || act.visibleInput.ReadableSharedSpaceIDs[0] != spTeam {
		t.Fatalf("只应把可读的 team/project 交给活动域，实际 %v", act.visibleInput.ReadableSharedSpaceIDs)
	}
}

func TestActivityFiltersReachDomainAndSurvivePagination(t *testing.T) {
	srv, _, _, act := newActivityWeb(t, governance.RoleMember)
	act.visiblePage = activity.Page{NextCursor: "cursor+/="}
	path := "/activity?action=card.update&outcome=error&agent=" + url.QueryEscape("agent-a") + "&space=" + url.QueryEscape(string(spTeam)) + "&range=7d"
	page := html.UnescapeString(body(t, get(t, srv, path, true)))
	if act.visibleInput.Action != "card.update" || act.visibleInput.Outcome != activity.OutcomeError || act.visibleInput.AgentID != "agent-a" || act.visibleInput.SpaceID != spTeam || act.visibleInput.Since.IsZero() {
		t.Fatalf("活动筛选没有下推到领域层: %+v", act.visibleInput)
	}
	for _, want := range []string{"action=card.update", "outcome=error", "agent=agent-a", "space=" + string(spTeam), "range=7d", "cursor=cursor%2B%2F%3D"} {
		if !strings.Contains(page, want) {
			t.Errorf("活动分页没有保留 %q: %s", want, page)
		}
	}
}

func TestAdminAuditRequiresConfirmationAndRecordsBeforeQuery(t *testing.T) {
	srv, _, _, act := newActivityWeb(t, governance.RoleAdmin)

	confirm := body(t, get(t, srv, "/admin/audit", true))
	if !strings.Contains(confirm, "查询动作本身会先写入") {
		t.Fatal("GET 只能展示二次确认说明")
	}
	if act.listAdminCalls != 0 || len(act.records) != 0 {
		t.Fatal("仅打开确认页不得读取完整日志或伪造 meta-audit")
	}

	resp := post(t, srv, "/admin/audit", url.Values{}, true, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("确认后的审计查询应成功，实际 %d", resp.StatusCode)
	}
	if len(act.order) < 2 || act.order[0] != "record" || act.order[1] != "admin" {
		t.Fatalf("必须先写 meta-audit 再查完整日志，实际顺序 %v", act.order)
	}
	if len(act.records) != 1 {
		t.Fatalf("期望一条 meta-audit，实际 %d", len(act.records))
	}
	meta := act.records[0]
	if meta.Kind != activity.KindMetaAudit || meta.Access != activity.AccessRead || meta.Action != "admin.audit.query" {
		t.Fatalf("meta-audit 形态不对: %+v", meta)
	}
}

func TestAdminAuditFiltersReachDomainAndPaginationForm(t *testing.T) {
	srv, _, _, act := newActivityWeb(t, governance.RoleAdmin)
	act.adminPage = activity.Page{NextCursor: "next-page"}
	resp := post(t, srv, "/admin/audit", url.Values{
		"action": {"token"}, "outcome": {activity.OutcomeSuccess}, "agent": {"agent-a"}, "space": {string(spTeam)}, "range": {"24h"},
	}, true, true)
	page := body(t, resp)
	if act.adminInput.Action != "token" || act.adminInput.Outcome != activity.OutcomeSuccess || act.adminInput.AgentID != "agent-a" || act.adminInput.SpaceID != spTeam || act.adminInput.Since.IsZero() {
		t.Fatalf("审计筛选没有下推到领域层: %+v", act.adminInput)
	}
	for _, want := range []string{`name="action" value="token"`, `name="outcome" value="success"`, `name="agent" value="agent-a"`, `name="space" value="` + string(spTeam) + `"`, `name="range" value="24h"`} {
		if !strings.Contains(page, want) {
			t.Errorf("审计下一页表单没有保留 %q", want)
		}
	}
}

func TestAdminAuditFailsClosedWhenMetaAuditCannotPersist(t *testing.T) {
	srv, _, _, act := newActivityWeb(t, governance.RoleAdmin)
	act.recordErr = errors.New("audit unavailable")

	resp := post(t, srv, "/admin/audit", url.Values{}, true, true)
	page := body(t, resp)
	if resp.StatusCode != http.StatusInternalServerError {
		t.Fatalf("meta-audit 写不下时应拒绝查询，实际 %d", resp.StatusCode)
	}
	if act.listAdminCalls != 0 {
		t.Fatal("meta-audit 写不下时不得先把完整日志读进内存")
	}
	if !strings.Contains(page, "已拒绝展示完整日志") {
		t.Fatalf("页面应解释 fail-closed，实际 %.200s", page)
	}
}

func TestAuthenticatedWebWritesAreAuditedWithoutSensitivePayloads(t *testing.T) {
	tests := []struct {
		name, path, action string
		form               url.Values
	}{
		{"logout", "/logout", "session.logout", url.Values{}},
		{"password", "/password", "password.change", url.Values{"current": {"current-password"}, "new": {"secret-password-123"}, "confirm": {"secret-password-123"}}},
		{"token issue", "/tokens", "token.issue", url.Values{"agent": {"secret-agent-slug"}, "name": {"Secret Device"}}},
		{"token revoke", "/tokens/revoke", "token.revoke", url.Values{"id": {"01TOKENAAAAAAAAAAAAAAAAAAA"}}},
		{"review", "/review/01CARDAAAAAAAAAAAAAAAAAAAA", "card.review", url.Values{"action": {"reject"}, "title": {"secret-card-title"}}},
		{"batch review", "/review/batch-deprecate", "card.review_batch_deprecate", url.Values{"selection": {"01CARDAAAAAAAAAAAAAAAAAAAA:1"}}},
		{"shared line status", "/continuity/01LINEAAAAAAAAAAAAAAAAAAAA/status", "shared_line.status", url.Values{"status": {continuity.LineArchived}}},
		{"member create", "/admin/members", "member.create", url.Values{"username": {"secret-user"}, "display": {"Secret User"}, "role": {"member"}}},
		{"member status", "/admin/members/status", "member.status", url.Values{"id": {string(userOther)}, "status": {"disabled"}}},
		{"member reset", "/admin/members/reset", "member.password_reset", url.Values{"id": {string(userOther)}}},
		{"space create", "/admin/spaces", "space.create", url.Values{"name": {"Secret Project"}}},
		{"space grant", "/admin/spaces/grant", "space.permission.grant", url.Values{"space": {string(spProject)}, "user": {string(userOther)}, "access": {"write"}}},
		{"space revoke", "/admin/spaces/revoke", "space.permission.revoke", url.Values{"space": {string(spProject)}, "user": {string(userOther)}}},
	}

	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, gov, _, act := newActivityWeb(t, governance.RoleAdmin)
			if tt.action == "token.revoke" {
				gov.tokens = []governance.Token{{ID: "01TOKENAAAAAAAAAAAAAAAAAAA"}}
			}
			resp := post(t, srv, tt.path, tt.form, true, true)
			// The audit runs after the handler returns. Drain the response so
			// net/http completes the middleware chain before inspecting the fake.
			_ = body(t, resp)
			if len(act.records) != 1 {
				t.Fatalf("每个已认证写请求必须留一条记录，实际 %d", len(act.records))
			}
			if got := act.records[0].Action; got != tt.action {
				t.Fatalf("动作不对: want %q, got %q", tt.action, got)
			}
			encoded, err := json.Marshal(act.records[0])
			if err != nil {
				t.Fatal(err)
			}
			for _, secret := range []string{"secret-password-123", "secret-agent-slug", "Secret Device", "secret-card-title", "secret-user", "Secret User", "ccs_plaintext_token_value"} {
				if strings.Contains(string(encoded), secret) {
					t.Fatalf("审计记录泄露了表单或凭据 %q: %s", secret, encoded)
				}
			}
		})
	}
}

func TestWebWriteAuditCapturesRenderedFailureAndCreatedTargets(t *testing.T) {
	t.Run("rendered validation failure", func(t *testing.T) {
		srv, _, _, act := newActivityWeb(t, governance.RoleMember)
		resp := post(t, srv, "/password", url.Values{
			"current": {"current-password"},
			"new":     {"new-password-one"},
			"confirm": {"different-password"},
		}, true, true)
		_ = body(t, resp)

		// Form validation deliberately re-renders with HTTP 200. The audit
		// outcome must still describe the failed action, not the render status.
		if len(act.records) != 1 || act.records[0].Outcome != activity.OutcomeError {
			t.Fatalf("rendered validation error must be audited as error: %+v", act.records)
		}
	})

	tests := []struct {
		name, path, entityType string
		form                   url.Values
		entityID               platform.ID
	}{
		{
			name: "issued token", path: "/tokens", entityType: "token",
			form:     url.Values{"agent": {"clara"}, "name": {"MacBook"}},
			entityID: "01TOKENAAAAAAAAAAAAAAAAAAA",
		},
		{
			name: "created member", path: "/admin/members", entityType: "user",
			form:     url.Values{"username": {"new-user"}, "display": {"New User"}, "role": {"member"}},
			entityID: "01NEWUSERAAAAAAAAAAAAAAAAA",
		},
		{
			name: "created project space", path: "/admin/spaces", entityType: "space",
			form:     url.Values{"name": {"New Project"}},
			entityID: "01NEWSPACEAAAAAAAAAAAAAAAA",
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			srv, _, _, act := newActivityWeb(t, governance.RoleAdmin)
			resp := post(t, srv, tt.path, tt.form, true, true)
			_ = body(t, resp)

			if len(act.records) != 1 {
				t.Fatalf("expected one audit record, got %d", len(act.records))
			}
			record := act.records[0]
			if record.EntityType != tt.entityType || record.EntityID == nil || *record.EntityID != tt.entityID {
				t.Fatalf("created target was not resolved from domain output: %+v", record)
			}
			if record.Outcome != activity.OutcomeSuccess {
				t.Fatalf("successful write outcome = %s", record.Outcome)
			}
		})
	}
}

func TestAuthenticatedCSRFRejectionIsAuditedAsFailure(t *testing.T) {
	srv, _, _, act := newActivityWeb(t, governance.RoleMember)
	resp := post(t, srv, "/tokens", url.Values{
		"agent": {"clara"}, "name": {"MacBook"},
	}, true, false)
	_ = body(t, resp)

	if resp.StatusCode != http.StatusForbidden {
		t.Fatalf("missing CSRF status = %d, want 403", resp.StatusCode)
	}
	if len(act.records) != 1 || act.records[0].Action != "token.issue" || act.records[0].Outcome != activity.OutcomeError {
		t.Fatalf("authenticated rejected write must leave a failed audit record: %+v", act.records)
	}
}

func TestOrdinaryAuditFailureDoesNotChangeCompletedWriteResult(t *testing.T) {
	srv, _, _, act := newActivityWeb(t, governance.RoleMember)
	act.recordErr = errors.New("audit unavailable")

	resp := post(t, srv, "/tokens", url.Values{"agent": {"clara"}, "name": {"MacBook"}}, true, true)
	defer resp.Body.Close()
	if resp.StatusCode != http.StatusOK {
		t.Fatalf("业务写已完成后，审计失败不应把响应改成失败，实际 %d", resp.StatusCode)
	}
}

func TestReviewAuditCarriesResolvedSpaceForSharedVisibility(t *testing.T) {
	srv, _, know, act := newActivityWeb(t, governance.RoleMember)
	// The handler must use the domain result rather than guessing from the URL
	// or form. A real domain call only returns this card after write permission.
	know.card.SpaceID = spTeam

	resp := post(t, srv, "/review/01CARDAAAAAAAAAAAAAAAAAAAA", url.Values{
		"action": {"reject"}, "title": {"不需要的卡"}, "version": {"1"},
	}, true, true)
	_ = body(t, resp)
	if len(act.records) != 1 || act.records[0].SpaceID == nil || *act.records[0].SpaceID != spTeam {
		t.Fatalf("共享写入必须带领域层解析后的空间，实际 %+v", act.records)
	}
}
