package web

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"time"

	"claracore-server/internal/domain/activity"
	"claracore-server/internal/domain/governance"
	"claracore-server/internal/platform"
)

const (
	activityPageLimit     = 40
	activityRecordTimeout = 2 * time.Second
)

// statusWriter records the final HTTP status without buffering or changing the
// response. It lets the write middleware audit the real handler outcome.
type statusWriter struct {
	http.ResponseWriter
	status int
}

type webAuditStateKey struct{}

type webAuditState struct {
	EntityType string
	EntityID   *platform.ID
	SpaceID    *platform.ID
	Failed     bool
}

func setWebAuditTarget(r *http.Request, entityType string, entityID, spaceID platform.ID) {
	state, _ := r.Context().Value(webAuditStateKey{}).(*webAuditState)
	if state == nil {
		return
	}
	state.EntityType = entityType
	if entityID != "" {
		id := entityID
		state.EntityID = &id
	}
	if spaceID != "" {
		id := spaceID
		state.SpaceID = &id
	}
}

func markWebAuditFailed(r *http.Request) {
	state, _ := r.Context().Value(webAuditStateKey{}).(*webAuditState)
	if state != nil {
		state.Failed = true
	}
}

func (w *statusWriter) WriteHeader(code int) {
	if w.status != 0 {
		return
	}
	w.status = code
	w.ResponseWriter.WriteHeader(code)
}

func (w *statusWriter) Write(p []byte) (int, error) {
	if w.status == 0 {
		w.status = http.StatusOK
	}
	return w.ResponseWriter.Write(p)
}

func (w *statusWriter) Status() int {
	if w.status == 0 {
		return http.StatusOK
	}
	return w.status
}

// recordWebWrite is the single best-effort audit boundary for authenticated
// browser writes. It stores only stable action and entity identifiers. Form
// bodies are deliberately never serialized: they contain passwords, token
// plaintext, titles, and card content.
func (h *Host) recordWebWrite(r *http.Request, p governance.Principal, status int) {
	if h.activity == nil {
		return
	}
	action, entityType, entityID := webWriteDescriptor(r, p)
	var spaceID *platform.ID
	state, _ := r.Context().Value(webAuditStateKey{}).(*webAuditState)
	if state != nil {
		if state.EntityType != "" && state.EntityID != nil {
			entityType, entityID = state.EntityType, state.EntityID
		}
		spaceID = state.SpaceID
	}
	outcome := activity.OutcomeSuccess
	if status >= http.StatusBadRequest || (state != nil && state.Failed) {
		outcome = activity.OutcomeError
	}
	// 业务写已经结束时，客户端断开不应该顺带取消治理记录。
	// 去掉请求 cancel，但用短超时保证审计故障不会卡住响应。
	recordCtx, cancel := context.WithTimeout(context.WithoutCancel(r.Context()), activityRecordTimeout)
	defer cancel()
	_, err := h.activity.Record(recordCtx, activity.RecordInput{
		TenantID:   p.TenantID,
		UserID:     p.UserID,
		Source:     activity.SourceWeb,
		Kind:       activity.KindGovernance,
		Access:     activity.AccessWrite,
		Outcome:    outcome,
		Action:     action,
		EntityType: entityType,
		EntityID:   entityID,
		SpaceID:    spaceID,
	})
	if err != nil {
		// The business write has already happened. Turning this into an HTTP
		// failure would invite a retry and possibly duplicate the action.
		h.logger.Error("记录 Web 活动失败", "action", action, "error", err)
	}
}

func webWriteDescriptor(r *http.Request, p governance.Principal) (string, string, *platform.ID) {
	var action, entityType string
	var id platform.ID

	switch r.Pattern {
	case "POST /logout":
		action = "session.logout"
	case "POST /password":
		action, entityType, id = "password.change", "user", p.UserID
	case "POST /tokens":
		action = "token.issue"
	case "POST /tokens/revoke":
		action, entityType, id = "token.revoke", "token", platform.ID(r.FormValue("id"))
	case "POST /review/{id}":
		action, entityType, id = "card.review", "card", platform.ID(r.PathValue("id"))
	case "POST /continuity/{id}/status":
		action, entityType, id = "shared_line.status", "shared_line", platform.ID(r.PathValue("id"))
	case "POST /admin/members":
		action = "member.create"
	case "POST /admin/members/status":
		action, entityType, id = "member.status", "user", platform.ID(r.FormValue("id"))
	case "POST /admin/members/reset":
		action, entityType, id = "member.password_reset", "user", platform.ID(r.FormValue("id"))
	case "POST /admin/spaces":
		action = "space.create"
	case "POST /admin/spaces/grant":
		action, entityType, id = "space.permission.grant", "space", platform.ID(r.FormValue("space"))
	case "POST /admin/spaces/revoke":
		action, entityType, id = "space.permission.revoke", "space", platform.ID(r.FormValue("space"))
	case "POST /admin/ingest":
		action, entityType, id = "ingest.run", "space", platform.ID(r.FormValue("space"))
	case "POST /admin/indexing/backfill":
		action = "embedding.backfill"
	case "POST /admin/indexing/retry":
		action = "embedding.retry"
	case "POST /admin/audit":
		action = "admin.audit.query"
	default:
		// A new authenticated write cannot silently disappear from audit just
		// because its richer descriptor was not added yet.
		action = "web.write"
	}
	if entityType == "" || id == "" {
		return action, "", nil
	}
	return action, entityType, &id
}

type activityEventView struct {
	Event    activity.Event
	Own      bool
	Metadata string
}

func eventViews(events []activity.Event, userID platform.ID) []activityEventView {
	views := make([]activityEventView, 0, len(events))
	for _, event := range events {
		views = append(views, activityEventView{
			Event:    event,
			Own:      event.UserID == userID,
			Metadata: string(event.Metadata),
		})
	}
	return views
}

type activityPageData struct {
	pageData
	Events     []activityEventView
	NextCursor string
	NextURL    string
	Action     string
	Outcome    string
	AgentID    string
	SpaceID    string
	Range      string
}

func (h *Host) handleActivity(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	data := activityPageData{pageData: h.page(r, "活动流", "activity")}
	data.Action, data.Outcome = strings.TrimSpace(r.URL.Query().Get("action")), r.URL.Query().Get("outcome")
	data.AgentID, data.SpaceID = strings.TrimSpace(r.URL.Query().Get("agent")), strings.TrimSpace(r.URL.Query().Get("space"))
	data.Range = normalizedActivityRange(r.URL.Query().Get("range"))

	if h.activity == nil {
		data.Error = "活动流暂时不可用"
		h.render(w, "activity.html", data)
		return
	}

	scope, err := h.gov.Scope(r.Context(), governance.Identity{TenantID: p.TenantID, UserID: p.UserID})
	if err != nil {
		data.Error = "读取空间信息失败"
		h.logger.Error("活动流解析空间权限失败", "error", err)
		h.render(w, "activity.html", data)
		return
	}
	var shared []platform.ID
	for _, access := range scope.All() {
		if !access.CanRead {
			continue
		}
		switch access.Space.Type {
		case governance.SpaceTeam, governance.SpaceProject:
			shared = append(shared, access.Space.ID)
		}
	}

	page, err := h.activity.ListVisible(r.Context(), activity.ListVisibleInput{
		TenantID:               p.TenantID,
		UserID:                 p.UserID,
		ReadableSharedSpaceIDs: shared,
		Cursor:                 r.URL.Query().Get("cursor"),
		Limit:                  activityPageLimit,
		Action:                 data.Action,
		Outcome:                data.Outcome,
		AgentID:                platform.ID(data.AgentID),
		SpaceID:                platform.ID(data.SpaceID),
		Since:                  activitySince(data.Range),
	})
	if err != nil {
		data.Error = "读取活动流失败"
		h.logger.Error("读取活动流失败", "error", err)
		h.render(w, "activity.html", data)
		return
	}
	data.Events = eventViews(page.Events, p.UserID)
	data.NextCursor = page.NextCursor
	if data.NextCursor != "" {
		data.NextURL = activityURL("/activity", data.NextCursor, data.Action, data.Outcome, data.AgentID, data.SpaceID, data.Range)
	}
	h.render(w, "activity.html", data)
}

type auditConfirmData struct{ pageData }

func (h *Host) handleAuditConfirm(w http.ResponseWriter, r *http.Request) {
	data := auditConfirmData{pageData: h.page(r, "完整审计", "audit")}
	h.render(w, "audit_confirm.html", data)
}

type auditPageData struct {
	pageData
	Events     []activityEventView
	NextCursor string
	Action     string
	Outcome    string
	AgentID    string
	SpaceID    string
	Range      string
}

func (h *Host) handleAudit(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	if h.activity == nil {
		h.renderAuditFailure(w, r, "审计服务暂时不可用")
		return
	}

	// Fail closed: the complete audit query must never run unless its own
	// meta-audit record has already been persisted.
	metadata, _ := json.Marshal(map[string]string{"scope": "tenant", "confirmation": "explicit"})
	if _, err := h.activity.Record(r.Context(), activity.RecordInput{
		TenantID: p.TenantID,
		UserID:   p.UserID,
		Source:   activity.SourceWeb,
		Kind:     activity.KindMetaAudit,
		Access:   activity.AccessRead,
		Outcome:  activity.OutcomeSuccess,
		Action:   "admin.audit.query",
		Metadata: metadata,
	}); err != nil {
		h.logger.Error("记录管理员审计查询失败", "error", err)
		h.renderAuditFailure(w, r, "无法记录本次审计查询，已拒绝展示完整日志")
		return
	}

	action, outcome := strings.TrimSpace(r.FormValue("action")), r.FormValue("outcome")
	agentID, spaceID := strings.TrimSpace(r.FormValue("agent")), strings.TrimSpace(r.FormValue("space"))
	rangeValue := normalizedActivityRange(r.FormValue("range"))
	page, err := h.activity.ListAdmin(r.Context(), activity.ListAdminInput{
		TenantID: p.TenantID,
		Cursor:   r.FormValue("cursor"),
		Limit:    activityPageLimit,
		Action:   action,
		Outcome:  outcome,
		AgentID:  platform.ID(agentID),
		SpaceID:  platform.ID(spaceID),
		Since:    activitySince(rangeValue),
	})
	if err != nil {
		h.logger.Error("读取完整审计日志失败", "error", err)
		h.renderAuditFailure(w, r, "读取完整审计日志失败")
		return
	}
	data := auditPageData{
		pageData:   h.page(r, "完整审计", "audit"),
		Events:     eventViews(page.Events, p.UserID),
		NextCursor: page.NextCursor,
		Action:     action,
		Outcome:    outcome,
		AgentID:    agentID,
		SpaceID:    spaceID,
		Range:      rangeValue,
	}
	h.render(w, "audit.html", data)
}

func activityURL(path, cursor, action, outcome, agentID, spaceID, rangeValue string) string {
	q := url.Values{}
	for key, value := range map[string]string{"cursor": cursor, "action": action, "outcome": outcome, "agent": agentID, "space": spaceID, "range": rangeValue} {
		if value != "" {
			q.Set(key, value)
		}
	}
	if len(q) == 0 {
		return path
	}
	return path + "?" + q.Encode()
}

func normalizedActivityRange(raw string) string {
	switch raw {
	case "24h", "7d", "30d":
		return raw
	default:
		return ""
	}
}

func activitySince(rangeValue string) time.Time {
	var duration time.Duration
	switch rangeValue {
	case "24h":
		duration = 24 * time.Hour
	case "7d":
		duration = 7 * 24 * time.Hour
	case "30d":
		duration = 30 * 24 * time.Hour
	default:
		return time.Time{}
	}
	return time.Now().UTC().Add(-duration)
}

func (h *Host) renderAuditFailure(w http.ResponseWriter, r *http.Request, message string) {
	data := auditConfirmData{pageData: h.page(r, "完整审计", "audit")}
	data.Error = message
	w.WriteHeader(http.StatusInternalServerError)
	h.render(w, "audit_confirm.html", data)
}
