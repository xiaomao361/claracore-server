package web

import (
	"errors"
	"net/http"

	"claracore-server/internal/domain/continuity"
	"claracore-server/internal/platform"
)

const webContinuityAgentID = platform.ID("web-governance")

type continuityData struct {
	pageData
	Lines  []continuity.LineSummary
	Status string
}

func (h *Host) handleContinuity(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	data := continuityData{pageData: h.page(r, "共同线", "continuity")}
	data.Status = r.URL.Query().Get("status")
	if data.Status != continuity.LineArchived && data.Status != "all" {
		data.Status = continuity.LineActive
	}

	lines, err := h.continuity.List(r.Context(), continuity.ListInput{
		TenantID: p.TenantID,
		// Web 是用户治理面，没有 agent 身份；AllAgents=true 时这里只作为
		// 领域接口要求的非空调用方标识，实际结果仍由 tenant 严格隔离。
		AgentID:   webContinuityAgentID,
		AllAgents: true,
		Status:    data.Status,
		Limit:     100,
	})
	if err != nil {
		h.logger.Error("读取共同线失败", "error", err)
		data.Error = "读取共同线失败"
		h.render(w, "continuity.html", data)
		return
	}
	data.Lines = lines
	h.render(w, "continuity.html", data)
}

type continuityDetailData struct {
	pageData
	Packet continuity.ResumePacket
}

func (h *Host) handleContinuityDetail(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	packet, err := h.continuity.Get(r.Context(), continuity.GetInput{
		TenantID: p.TenantID,
		AgentID:  webContinuityAgentID,
		LineID:   platform.ID(r.PathValue("id")),
	})
	if errors.Is(err, continuity.ErrNotFound) {
		http.NotFound(w, r)
		return
	}
	if err != nil {
		h.logger.Error("读取共同线详情失败", "error", err)
		http.Error(w, "读取共同线详情失败", http.StatusInternalServerError)
		return
	}
	h.render(w, "continuity_detail.html", continuityDetailData{
		pageData: h.page(r, packet.Line.Title, "continuity"),
		Packet:   packet,
	})
}

func (h *Host) handleContinuityStatus(w http.ResponseWriter, r *http.Request) {
	p, _ := principalFrom(r.Context())
	lineID := platform.ID(r.PathValue("id"))
	status := r.FormValue("status")
	if status != continuity.LineActive && status != continuity.LineArchived {
		markWebAuditFailed(r)
		http.Error(w, "状态只能是 active 或 archived", http.StatusBadRequest)
		return
	}
	setWebAuditTarget(r, "shared_line", lineID, "")
	_, err := h.continuity.SetStatus(r.Context(), continuity.SetStatusInput{
		TenantID: p.TenantID,
		AgentID:  webContinuityAgentID,
		LineID:   lineID,
		Status:   status,
	})
	if errors.Is(err, continuity.ErrNotFound) {
		markWebAuditFailed(r)
		http.NotFound(w, r)
		return
	}
	if err != nil {
		markWebAuditFailed(r)
		h.logger.Error("更新共同线状态失败", "error", err)
		http.Error(w, "更新共同线状态失败", http.StatusInternalServerError)
		return
	}
	http.Redirect(w, r, "/continuity/"+string(lineID), http.StatusSeeOther)
}
