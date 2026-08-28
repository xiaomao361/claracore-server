package continuity

import (
	"context"
	"encoding/base64"
	"errors"
	"fmt"
	"strconv"
	"strings"
	"time"

	"claracore-server/internal/platform"
)

type Service struct {
	store Store
	clock platform.Clock
	ids   *platform.IDGenerator
}

func NewService(store Store, clock platform.Clock, ids *platform.IDGenerator) *Service {
	return &Service{store: store, clock: clock, ids: ids}
}

type ListInput struct {
	TenantID  platform.ID
	AgentID   platform.ID
	AllAgents bool
	Status    string
	Limit     int
	Cursor    string
}

type ListPage struct {
	Lines      []LineSummary
	NextCursor string
}

func (s *Service) List(ctx context.Context, in ListInput) ([]LineSummary, error) {
	if in.TenantID == "" || in.AgentID == "" {
		return nil, fmt.Errorf("%w: 缺少身份", ErrInvalidArgument)
	}
	if in.Status == "" {
		in.Status = LineActive
	}
	if in.Status != LineActive && in.Status != LineArchived && in.Status != "all" {
		return nil, fmt.Errorf("%w: status 只能是 active、archived 或 all", ErrInvalidArgument)
	}
	if in.Limit <= 0 {
		in.Limit = 20
	}
	if in.Limit > MaxListLimit {
		in.Limit = MaxListLimit
	}
	query := ListQuery{
		TenantID: in.TenantID, OwnerAgent: in.AgentID, AllAgents: in.AllAgents,
		Status: in.Status, Limit: in.Limit,
	}
	if in.Cursor != "" {
		var err error
		query.AfterUpdatedAt, query.AfterID, err = decodeListCursor(in.Cursor)
		if err != nil {
			return nil, err
		}
	}
	lines, err := s.store.ListLines(ctx, query)
	if err != nil {
		return nil, err
	}
	out := make([]LineSummary, 0, len(lines))
	for _, line := range lines {
		if line.TenantID != in.TenantID {
			return nil, ErrTenantMismatch
		}
		summary := LineSummary{ID: line.ID, OwnerAgentID: line.OwnerAgentID, Title: line.Title, Status: line.Status, UpdatedAt: line.UpdatedAt}
		if line.Position != nil {
			summary.Summary = line.Position.Summary
			summary.InterpretationStatus = line.Position.InterpretationStatus
			summary.NextStep = line.Position.NextStep
			if line.Position.UpdatedAt.After(summary.UpdatedAt) {
				summary.UpdatedAt = line.Position.UpdatedAt
			}
		}
		out = append(out, summary)
	}
	return out, nil
}

func (s *Service) ListPage(ctx context.Context, in ListInput) (ListPage, error) {
	limit := in.Limit
	if limit <= 0 {
		limit = 20
	}
	if limit >= MaxListLimit {
		limit = MaxListLimit - 1
	}
	in.Limit = limit + 1
	lines, err := s.List(ctx, in)
	if err != nil {
		return ListPage{}, err
	}
	page := ListPage{Lines: lines}
	if len(lines) > limit {
		last := lines[limit-1]
		page.Lines = lines[:limit]
		page.NextCursor = encodeListCursor(last.UpdatedAt, last.ID)
	}
	return page, nil
}

func encodeListCursor(updatedAt time.Time, id platform.ID) string {
	raw := strconv.FormatInt(updatedAt.UTC().UnixMilli(), 10) + ":" + string(id)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeListCursor(cursor string) (time.Time, platform.ID, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%w: 游标格式非法", ErrInvalidArgument)
	}
	millis, rawID, ok := strings.Cut(string(raw), ":")
	if !ok || rawID == "" {
		return time.Time{}, "", fmt.Errorf("%w: 游标格式非法", ErrInvalidArgument)
	}
	n, err := strconv.ParseInt(millis, 10, 64)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%w: 游标格式非法", ErrInvalidArgument)
	}
	return time.UnixMilli(n).UTC(), platform.ID(rawID), nil
}

type GetInput struct {
	TenantID platform.ID
	AgentID  platform.ID
	LineID   platform.ID
}

type SetStatusInput struct {
	TenantID platform.ID
	AgentID  platform.ID
	LineID   platform.ID
	Status   string
}

func (s *Service) SetStatus(ctx context.Context, in SetStatusInput) (ResumePacket, error) {
	if in.TenantID == "" || in.AgentID == "" || in.LineID == "" {
		return ResumePacket{}, fmt.Errorf("%w: 缺少身份或 lineId", ErrInvalidArgument)
	}
	if in.Status != LineActive && in.Status != LineArchived {
		return ResumePacket{}, fmt.Errorf("%w: status 只能是 active 或 archived", ErrInvalidArgument)
	}
	line, err := s.store.FindLine(ctx, in.TenantID, in.LineID, false)
	if err != nil {
		return ResumePacket{}, err
	}
	now := s.clock.Now()
	if line.Status != in.Status {
		if err := s.store.UpdateLineStatus(ctx, in.TenantID, in.LineID, in.Status, now); err != nil {
			return ResumePacket{}, err
		}
		line.Status, line.UpdatedAt = in.Status, now
	}
	return s.packet(ctx, s.store, *line)
}

func (s *Service) Get(ctx context.Context, in GetInput) (ResumePacket, error) {
	if in.LineID != "" {
		if in.TenantID == "" || in.AgentID == "" {
			return ResumePacket{}, fmt.Errorf("%w: 缺少身份", ErrInvalidArgument)
		}
		// An explicit ID is an intentional lifecycle/history lookup, so archived
		// lines remain readable. Implicit selection below still considers only
		// active lines and keeps the ambiguity fail-closed behavior.
		line, err := s.store.FindLine(ctx, in.TenantID, in.LineID, false)
		if err != nil {
			return ResumePacket{}, err
		}
		if line.TenantID != in.TenantID {
			return ResumePacket{}, ErrTenantMismatch
		}
		return s.packet(ctx, s.store, *line)
	}
	line, err := s.resolveActiveLine(ctx, s.store, in.TenantID, in.AgentID, in.LineID)
	if err != nil {
		return ResumePacket{}, err
	}
	return s.packet(ctx, s.store, *line)
}

type UpdateInput struct {
	TenantID platform.ID
	UserID   platform.ID
	AgentID  platform.ID
	LineID   platform.ID
	Title    string

	Summary              string
	InterpretationStatus string
	FactsUsed            []platform.ID
	NextStep             string
	ExpectedVersion      *int
}

func (s *Service) Update(ctx context.Context, in UpdateInput) (ResumePacket, error) {
	if in.TenantID == "" || in.UserID == "" || in.AgentID == "" {
		return ResumePacket{}, fmt.Errorf("%w: 缺少身份", ErrInvalidArgument)
	}
	summary, err := normalizeText(in.Summary, MaxSummaryLen, "摘要", true)
	if err != nil {
		return ResumePacket{}, err
	}
	nextStep, err := normalizeText(in.NextStep, MaxNextStepLen, "下一步", false)
	if err != nil {
		return ResumePacket{}, err
	}
	status := strings.TrimSpace(in.InterpretationStatus)
	if status == "" {
		status = PositionDraft
	}
	if !validPositionStatus(status) {
		return ResumePacket{}, fmt.Errorf("%w: interpretationStatus 取值非法", ErrInvalidArgument)
	}
	facts, err := normalizeFacts(in.FactsUsed)
	if err != nil {
		return ResumePacket{}, err
	}

	var written Line
	err = s.store.Atomically(ctx, func(st Store) error {
		line, err := s.resolveActiveLine(ctx, st, in.TenantID, in.AgentID, in.LineID)
		if errors.Is(err, ErrNotFound) && in.LineID == "" {
			title, titleErr := normalizeText(in.Title, MaxTitleLen, "标题", true)
			if titleErr != nil {
				return titleErr
			}
			now := s.clock.Now()
			line = &Line{
				ID: s.ids.New(), TenantID: in.TenantID, OwnerUserID: in.UserID, OwnerAgentID: in.AgentID,
				Title: title, Status: LineActive, CreatedAt: now, UpdatedAt: now,
			}
			if err := st.CreateLine(ctx, *line); err != nil {
				return err
			}
		} else if err != nil {
			return err
		}
		position, err := st.FindPosition(ctx, in.TenantID, line.ID)
		now := s.clock.Now()
		if errors.Is(err, ErrNotFound) {
			if in.ExpectedVersion != nil && *in.ExpectedVersion != 0 {
				return &VersionConflictError{Current: 0}
			}
			position = &Position{ID: s.ids.New(), TenantID: in.TenantID, LineID: line.ID, Version: 1}
			position.Summary, position.InterpretationStatus = summary, status
			position.FactsUsed, position.NextStep = facts, nextStep
			position.WriterUserID, position.WriterAgentID, position.UpdatedAt = in.UserID, in.AgentID, now
			if err := st.CreatePosition(ctx, *position); err != nil {
				return err
			}
		} else if err != nil {
			return err
		} else {
			expected := position.Version
			if in.ExpectedVersion != nil {
				expected = *in.ExpectedVersion
				if expected != position.Version {
					return &VersionConflictError{Current: position.Version}
				}
			}
			position.Summary, position.InterpretationStatus = summary, status
			position.FactsUsed, position.NextStep = facts, nextStep
			position.WriterUserID, position.WriterAgentID = in.UserID, in.AgentID
			position.Version, position.UpdatedAt = expected+1, now
			if err := st.UpdatePosition(ctx, *position, expected); err != nil {
				return err
			}
		}
		if err := st.AppendPositionHistory(ctx, s.ids.New(), *position, now); err != nil {
			return err
		}
		line.Position, line.UpdatedAt = position, now
		written = *line
		return nil
	})
	if err != nil {
		return ResumePacket{}, err
	}
	return s.packet(ctx, s.store, written)
}

type HandoffInput struct {
	TenantID  platform.ID
	UserID    platform.ID
	AgentID   platform.ID
	LineID    platform.ID
	Objective string
	Completed []string
	OpenItems []string
	NextStep  string
}

func (s *Service) Handoff(ctx context.Context, in HandoffInput) (ResumePacket, error) {
	if in.TenantID == "" || in.UserID == "" || in.AgentID == "" {
		return ResumePacket{}, fmt.Errorf("%w: 缺少身份", ErrInvalidArgument)
	}
	objective, err := normalizeText(in.Objective, MaxNextStepLen, "目标", false)
	if err != nil {
		return ResumePacket{}, err
	}
	nextStep, err := normalizeText(in.NextStep, MaxNextStepLen, "下一步", false)
	if err != nil {
		return ResumePacket{}, err
	}
	completed, err := normalizeStringList(in.Completed, 100, 1000, "已完成")
	if err != nil {
		return ResumePacket{}, err
	}
	openItems, err := normalizeStringList(in.OpenItems, 100, 1000, "待办")
	if err != nil {
		return ResumePacket{}, err
	}
	if objective == "" && nextStep == "" && len(completed) == 0 && len(openItems) == 0 {
		return ResumePacket{}, fmt.Errorf("%w: 交接内容不能为空", ErrInvalidArgument)
	}
	line, err := s.resolveActiveLine(ctx, s.store, in.TenantID, in.AgentID, in.LineID)
	if err != nil {
		return ResumePacket{}, err
	}
	handoff := Handoff{
		ID: s.ids.New(), TenantID: in.TenantID, LineID: line.ID,
		WriterUserID: in.UserID, WriterAgentID: in.AgentID,
		Objective: objective, Completed: completed, OpenItems: openItems, NextStep: nextStep,
		CreatedAt: s.clock.Now(),
	}
	if err := s.store.CreateHandoff(ctx, handoff); err != nil {
		return ResumePacket{}, err
	}
	packet, err := s.packet(ctx, s.store, *line)
	if err != nil {
		return ResumePacket{}, err
	}
	packet.RecentHandoff = &handoff
	return packet, nil
}

func (s *Service) resolveActiveLine(ctx context.Context, st Store, tenantID, agentID, lineID platform.ID) (*Line, error) {
	if tenantID == "" || agentID == "" {
		return nil, fmt.Errorf("%w: 缺少身份", ErrInvalidArgument)
	}
	if lineID != "" {
		line, err := st.FindLine(ctx, tenantID, lineID, true)
		if err != nil {
			return nil, err
		}
		if line.TenantID != tenantID {
			return nil, ErrTenantMismatch
		}
		return line, nil
	}
	lines, err := st.ListLines(ctx, ListQuery{TenantID: tenantID, OwnerAgent: agentID, Status: LineActive, Limit: 3})
	if err != nil {
		return nil, err
	}
	switch len(lines) {
	case 0:
		return nil, ErrNotFound
	case 1:
		return &lines[0], nil
	default:
		candidates := make([]LineSummary, 0, len(lines))
		for _, line := range lines {
			candidates = append(candidates, LineSummary{ID: line.ID, OwnerAgentID: line.OwnerAgentID, Title: line.Title, Status: line.Status, UpdatedAt: line.UpdatedAt})
		}
		return nil, &LineIDRequiredError{Candidates: candidates}
	}
}

func (s *Service) packet(ctx context.Context, st Store, line Line) (ResumePacket, error) {
	position, err := st.FindPosition(ctx, line.TenantID, line.ID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return ResumePacket{}, err
	}
	if err == nil {
		line.Position = position
	}
	handoff, err := st.LatestHandoff(ctx, line.TenantID, line.ID)
	if err != nil && !errors.Is(err, ErrNotFound) {
		return ResumePacket{}, err
	}
	if errors.Is(err, ErrNotFound) {
		handoff = nil
	}
	positions, err := st.ListPositionHistory(ctx, line.TenantID, line.ID, 10)
	if err != nil {
		return ResumePacket{}, err
	}
	handoffs, err := st.ListHandoffs(ctx, line.TenantID, line.ID, 10)
	if err != nil {
		return ResumePacket{}, err
	}
	return ResumePacket{Line: line, RecentHandoff: handoff, PositionHistory: positions, HandoffHistory: handoffs}, nil
}
