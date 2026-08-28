package mcp

import (
	"context"
	"strings"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"claracore-server/internal/domain/continuity"
	"claracore-server/internal/platform"
)

// continuityService 是 Host 对 Shared Line 域的完整需求。Shared Line 以租户为
// 共享边界，不复用 knowledge 的 space 权限：它记录的是可续接工作位置，不是事实库。
type continuityService interface {
	List(context.Context, continuity.ListInput) ([]continuity.LineSummary, error)
	Get(context.Context, continuity.GetInput) (continuity.ResumePacket, error)
	Update(context.Context, continuity.UpdateInput) (continuity.ResumePacket, error)
	Handoff(context.Context, continuity.HandoffInput) (continuity.ResumePacket, error)
	SetStatus(context.Context, continuity.SetStatusInput) (continuity.ResumePacket, error)
}

type sharedLineStatusInput struct {
	LineID string `json:"lineId"`
	Status string `json:"status"`
}

func (h *Host) handleSharedLineStatus(ctx context.Context, _ *sdk.CallToolRequest, in sharedLineStatusInput) (*sdk.CallToolResult, resumePacketOut, error) {
	id, ok := identityFrom(ctx)
	if !ok {
		return nil, resumePacketOut{}, toolError(h.logger, "shared_line_status", errNoIdentity)
	}
	p, err := h.continuity.SetStatus(ctx, continuity.SetStatusInput{TenantID: id.TenantID, AgentID: id.AgentID, LineID: platform.ID(in.LineID), Status: in.Status})
	if err != nil {
		return nil, resumePacketOut{}, toolError(h.logger, "shared_line_status", err)
	}
	return nil, encodeResumePacket(p), nil
}

type sharedLineListInput struct {
	Status    string `json:"status,omitempty" jsonschema:"active | archived | all。不传即 active"`
	AllAgents bool   `json:"allAgents,omitempty" jsonschema:"是否列出同租户所有 agent 的 line；默认只列当前 agent 拥有的 line"`
	Limit     int    `json:"limit,omitempty" jsonschema:"最多返回多少条，默认 20，最大 100"`
}

type sharedLineListOut struct {
	Lines []sharedLineSummaryOut `json:"lines"`
}

type sharedLineSummaryOut struct {
	ID                   string `json:"id"`
	OwnerAgentID         string `json:"ownerAgentId"`
	Title                string `json:"title"`
	Status               string `json:"status"`
	Summary              string `json:"summary,omitempty"`
	InterpretationStatus string `json:"interpretationStatus,omitempty"`
	NextStep             string `json:"nextStep,omitempty"`
	UpdatedAt            string `json:"updatedAt"`
}

func (h *Host) handleSharedLineList(ctx context.Context, _ *sdk.CallToolRequest, in sharedLineListInput) (*sdk.CallToolResult, sharedLineListOut, error) {
	id, ok := identityFrom(ctx)
	if !ok {
		return nil, sharedLineListOut{}, toolError(h.logger, "shared_line_list", errNoIdentity)
	}
	lines, err := h.continuity.List(ctx, continuity.ListInput{
		TenantID: id.TenantID, AgentID: id.AgentID, AllAgents: in.AllAgents, Status: in.Status, Limit: in.Limit,
	})
	if err != nil {
		return nil, sharedLineListOut{}, toolError(h.logger, "shared_line_list", err)
	}
	out := sharedLineListOut{Lines: make([]sharedLineSummaryOut, 0, len(lines))}
	for _, line := range lines {
		out.Lines = append(out.Lines, sharedLineSummaryOut{
			ID: string(line.ID), OwnerAgentID: string(line.OwnerAgentID), Title: line.Title, Status: line.Status,
			Summary: line.Summary, InterpretationStatus: line.InterpretationStatus, NextStep: line.NextStep,
			UpdatedAt: stamp(line.UpdatedAt),
		})
	}
	return nil, out, nil
}

type sharedLineGetInput struct {
	LineID string `json:"lineId,omitempty" jsonschema:"Shared Line ID；有多条 active line 时必须明确传入"`
}

func (h *Host) handleSharedLineGet(ctx context.Context, _ *sdk.CallToolRequest, in sharedLineGetInput) (*sdk.CallToolResult, resumePacketOut, error) {
	id, ok := identityFrom(ctx)
	if !ok {
		return nil, resumePacketOut{}, toolError(h.logger, "shared_line_get", errNoIdentity)
	}
	packet, err := h.continuity.Get(ctx, continuity.GetInput{TenantID: id.TenantID, AgentID: id.AgentID, LineID: platform.ID(in.LineID)})
	if err != nil {
		return nil, resumePacketOut{}, toolError(h.logger, "shared_line_get", err)
	}
	return nil, encodeResumePacket(packet), nil
}

type sharedLineUpdateInput struct {
	LineID               string   `json:"lineId,omitempty" jsonschema:"Shared Line ID；不传时按当前 agent 的 active line 数量自动创建或选择"`
	Title                string   `json:"title,omitempty" jsonschema:"新建 line 的标题；不传则使用当前 agent 名称生成"`
	Summary              string   `json:"summary" jsonschema:"当前可续接工作摘要"`
	InterpretationStatus string   `json:"interpretationStatus,omitempty" jsonschema:"draft | confirmed | active | needs_review | stale | closed；不传即 draft"`
	FactsUsed            []string `json:"factsUsed,omitempty" jsonschema:"支撑当前判断的 Memoria/Card 等事实 ID；这里只存 ID，不复制事实正文"`
	NextStep             string   `json:"nextStep,omitempty" jsonschema:"下一步具体动作"`
	Version              *int     `json:"version,omitempty" jsonschema:"上次读到的位置版本号，用于并发保护；新建时可传 0"`
}

func (h *Host) handleSharedLineUpdate(ctx context.Context, _ *sdk.CallToolRequest, in sharedLineUpdateInput) (*sdk.CallToolResult, resumePacketOut, error) {
	id, ok := identityFrom(ctx)
	if !ok {
		return nil, resumePacketOut{}, toolError(h.logger, "shared_line_update", errNoIdentity)
	}
	title := strings.TrimSpace(in.Title)
	if title == "" {
		name := strings.TrimSpace(id.AgentName)
		if name == "" {
			name = strings.TrimSpace(id.AgentSlug)
		}
		if name == "" {
			name = "Agent"
		}
		title = name + " Shared Line"
	}
	packet, err := h.continuity.Update(ctx, continuity.UpdateInput{
		TenantID: id.TenantID, UserID: id.UserID, AgentID: id.AgentID, LineID: platform.ID(in.LineID), Title: title,
		Summary: in.Summary, InterpretationStatus: in.InterpretationStatus, FactsUsed: toIDs(in.FactsUsed), NextStep: in.NextStep,
		ExpectedVersion: in.Version,
	})
	if err != nil {
		return nil, resumePacketOut{}, toolError(h.logger, "shared_line_update", err)
	}
	return nil, encodeResumePacket(packet), nil
}

type sharedLineHandoffInput struct {
	LineID    string   `json:"lineId,omitempty" jsonschema:"Shared Line ID；有多条 active line 时必须明确传入"`
	Objective string   `json:"objective,omitempty" jsonschema:"本阶段目标"`
	Completed []string `json:"completed,omitempty" jsonschema:"本阶段已完成事项"`
	OpenItems []string `json:"openItems,omitempty" jsonschema:"仍未完成或待确认事项"`
	NextStep  string   `json:"nextStep,omitempty" jsonschema:"接手者的第一步"`
}

func (h *Host) handleSharedLineHandoff(ctx context.Context, _ *sdk.CallToolRequest, in sharedLineHandoffInput) (*sdk.CallToolResult, resumePacketOut, error) {
	id, ok := identityFrom(ctx)
	if !ok {
		return nil, resumePacketOut{}, toolError(h.logger, "shared_line_handoff", errNoIdentity)
	}
	packet, err := h.continuity.Handoff(ctx, continuity.HandoffInput{
		TenantID: id.TenantID, UserID: id.UserID, AgentID: id.AgentID, LineID: platform.ID(in.LineID),
		Objective: in.Objective, Completed: in.Completed, OpenItems: in.OpenItems, NextStep: in.NextStep,
	})
	if err != nil {
		return nil, resumePacketOut{}, toolError(h.logger, "shared_line_handoff", err)
	}
	return nil, encodeResumePacket(packet), nil
}

type resumePacketOut struct {
	Line            sharedLineOut       `json:"line"`
	RecentHandoff   *sharedHandoffOut   `json:"recentHandoff,omitempty"`
	PositionHistory []sharedPositionOut `json:"positionHistory"`
	HandoffHistory  []sharedHandoffOut  `json:"handoffHistory"`
}

type sharedLineOut struct {
	ID           string             `json:"id"`
	OwnerUserID  string             `json:"ownerUserId"`
	OwnerAgentID string             `json:"ownerAgentId"`
	Title        string             `json:"title"`
	Status       string             `json:"status"`
	CreatedAt    string             `json:"createdAt"`
	UpdatedAt    string             `json:"updatedAt"`
	Position     *sharedPositionOut `json:"position,omitempty"`
}

type sharedPositionOut struct {
	ID                   string   `json:"id"`
	Summary              string   `json:"summary"`
	InterpretationStatus string   `json:"interpretationStatus"`
	FactsUsed            []string `json:"factsUsed"`
	NextStep             string   `json:"nextStep,omitempty"`
	WriterUserID         string   `json:"writerUserId"`
	WriterAgentID        string   `json:"writerAgentId"`
	Version              int      `json:"version"`
	UpdatedAt            string   `json:"updatedAt"`
}

type sharedHandoffOut struct {
	ID            string   `json:"id"`
	Objective     string   `json:"objective,omitempty"`
	Completed     []string `json:"completed"`
	OpenItems     []string `json:"openItems"`
	NextStep      string   `json:"nextStep,omitempty"`
	WriterUserID  string   `json:"writerUserId"`
	WriterAgentID string   `json:"writerAgentId"`
	CreatedAt     string   `json:"createdAt"`
}

func encodeResumePacket(packet continuity.ResumePacket) resumePacketOut {
	line := sharedLineOut{
		ID: string(packet.Line.ID), OwnerUserID: string(packet.Line.OwnerUserID), OwnerAgentID: string(packet.Line.OwnerAgentID),
		Title: packet.Line.Title, Status: packet.Line.Status, CreatedAt: stamp(packet.Line.CreatedAt), UpdatedAt: stamp(packet.Line.UpdatedAt),
	}
	if p := packet.Line.Position; p != nil {
		facts := make([]string, 0, len(p.FactsUsed))
		for _, id := range p.FactsUsed {
			facts = append(facts, string(id))
		}
		line.Position = &sharedPositionOut{
			ID: string(p.ID), Summary: p.Summary, InterpretationStatus: p.InterpretationStatus, FactsUsed: facts,
			NextStep: p.NextStep, WriterUserID: string(p.WriterUserID), WriterAgentID: string(p.WriterAgentID), Version: p.Version,
			UpdatedAt: stamp(p.UpdatedAt),
		}
	}
	out := resumePacketOut{Line: line}
	out.PositionHistory = make([]sharedPositionOut, 0, len(packet.PositionHistory))
	for _, p := range packet.PositionHistory {
		facts := make([]string, 0, len(p.FactsUsed))
		for _, id := range p.FactsUsed {
			facts = append(facts, string(id))
		}
		out.PositionHistory = append(out.PositionHistory, sharedPositionOut{ID: string(p.ID), Summary: p.Summary, InterpretationStatus: p.InterpretationStatus, FactsUsed: facts, NextStep: p.NextStep, WriterUserID: string(p.WriterUserID), WriterAgentID: string(p.WriterAgentID), Version: p.Version, UpdatedAt: stamp(p.UpdatedAt)})
	}
	out.HandoffHistory = make([]sharedHandoffOut, 0, len(packet.HandoffHistory))
	for _, h := range packet.HandoffHistory {
		out.HandoffHistory = append(out.HandoffHistory, sharedHandoffOut{ID: string(h.ID), Objective: h.Objective, Completed: nonNil(h.Completed), OpenItems: nonNil(h.OpenItems), NextStep: h.NextStep, WriterUserID: string(h.WriterUserID), WriterAgentID: string(h.WriterAgentID), CreatedAt: stamp(h.CreatedAt)})
	}
	if h := packet.RecentHandoff; h != nil {
		out.RecentHandoff = &sharedHandoffOut{
			ID: string(h.ID), Objective: h.Objective, Completed: nonNil(h.Completed), OpenItems: nonNil(h.OpenItems), NextStep: h.NextStep,
			WriterUserID: string(h.WriterUserID), WriterAgentID: string(h.WriterAgentID), CreatedAt: stamp(h.CreatedAt),
		}
	}
	return out
}
