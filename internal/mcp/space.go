package mcp

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"claracore-server/internal/domain/knowledge"
	"claracore-server/internal/platform"
)

// 本文件是空间相关的 3 个工具与 1 个系统工具
// （docs/03-mcp-contract.md §4.3、§4.5）。

// ---------------------------------------------------------------------------
// space_list
// ---------------------------------------------------------------------------

type spaceListInput struct{}

type spaceListOut struct {
	Spaces []spaceInfo `json:"spaces"`
	// DefaultSpaceID 是写入类工具不传 space 时的目标。
	DefaultSpaceID string `json:"defaultSpaceId,omitempty"`
}

// handleSpaceList 与 whoami 的空间部分重复，**这是有意的**。
//
// whoami 回答「我是谁」，顺带给出空间；space_list 回答「我能往哪写」。
// 合并成一个的话，agent 每次想确认目标空间都要连带拉一遍身份信息——
// 而它已经知道自己是谁了。两个工具共用同一个 SpaceScope，不存在
// 两份权限判断。
func (h *Host) handleSpaceList(ctx context.Context, _ *sdk.CallToolRequest, _ spaceListInput) (*sdk.CallToolResult, spaceListOut, error) {
	r, err := h.begin(ctx)
	if err != nil {
		return nil, spaceListOut{}, toolError(h.logger, "space_list", err)
	}
	accesses := r.scope.All()
	spaces := make([]spaceInfo, 0, len(accesses))
	for _, a := range accesses {
		spaces = append(spaces, spaceInfo{
			ID:       string(a.Space.ID),
			Type:     a.Space.Type,
			Name:     a.Space.Name,
			CanRead:  a.CanRead,
			CanWrite: a.CanWrite,
		})
	}
	out := spaceListOut{Spaces: spaces}
	if personal, err := r.scope.PersonalSpace(); err == nil {
		out.DefaultSpaceID = string(personal.ID)
	}
	return nil, out, nil
}

// ---------------------------------------------------------------------------
// promote
// ---------------------------------------------------------------------------

type promoteInput struct {
	EntityType string `json:"entityType,omitempty" jsonschema:"memory（默认）或 card"`
	EntityID   string `json:"entityId" jsonschema:"要提升的内容 ID"`
	ToSpace    string `json:"toSpace" jsonschema:"目标空间 ID，你必须对它有写权限"`
}

type promoteOut struct {
	// Type/ID 指向**新副本**，不是原件。
	Type  string   `json:"type"`
	ID    string   `json:"id"`
	Space spaceRef `json:"space"`
	Note  string   `json:"note"`
}

// handlePromote 把一条内容复制到另一个空间。
//
// 复制而非移动，原件保留；两份此后各自演进，**不做自动同步**——
// 自动同步会让团队空间里一条已达成共识的约定，被原作者的一次
// 随手修改悄悄改写（docs/02-data-model.md §4）。
func (h *Host) handlePromote(ctx context.Context, _ *sdk.CallToolRequest, in promoteInput) (*sdk.CallToolResult, promoteOut, error) {
	r, err := h.begin(ctx)
	if err != nil {
		return nil, promoteOut{}, toolError(h.logger, "promote", err)
	}
	// 两端都校：源可读 + 目标可写（2026-08-06 拍板）。
	// 只校目标的话，拿到一个自己无权读的 ID 就能把它复制进
	// 一个自己能读的空间——一次完整的越权读取，还留下合法的审计痕迹。
	readable, err := r.readableIDs(nil)
	if err != nil {
		return nil, promoteOut{}, toolError(h.logger, "promote", err)
	}
	target, err := r.writeTarget(in.ToSpace)
	if err != nil {
		return nil, promoteOut{}, toolError(h.logger, "promote", err)
	}

	ref, err := h.know.Promote(ctx, knowledge.PromoteInput{
		TenantID:       r.identity.TenantID,
		Ref:            knowledge.Ref{Type: defaultEntityType(in.EntityType), ID: platform.ID(in.EntityID)},
		ReadableSpaces: readable,
		TargetSpaceID:  target.ID,
		ActorUserID:    r.identity.UserID,
		ActorAgentID:   r.agentID(),
	})
	if err != nil {
		return nil, promoteOut{}, toolError(h.logger, "promote", err)
	}
	return nil, promoteOut{
		Type:  ref.Type,
		ID:    string(ref.ID),
		Space: r.spaceRefOf(target.ID),
		Note:  "已复制到目标空间，原件保留。两份此后各自演进，不会自动同步",
	}, nil
}

// ---------------------------------------------------------------------------
// space_stats
// ---------------------------------------------------------------------------

type spaceStatsInput struct {
	Spaces []string `json:"spaces,omitempty" jsonschema:"限定空间 ID。不传即统计全部可读空间"`
}

type spaceStatOut struct {
	Space    spaceRef `json:"space"`
	Memories int      `json:"memories"`
	Cards    int      `json:"cards"`
	// DraftCards 是摄取产生、还没人过目的卡：它们不进默认检索，
	// 所以这个数字直接等于「积压了多少待审内容」。
	DraftCards int `json:"draftCards"`
	// UnqualifiedCards 是「理解」栏为空的卡——只是摘要，不是合格卡片。
	// 这个数字的存在意义就是能被催。
	UnqualifiedCards int `json:"unqualifiedCards"`
	RecallTriggers   int `json:"recallTriggers"`
}

type spaceStatsOut struct {
	Spaces []spaceStatOut `json:"spaces"`
}

func (h *Host) handleSpaceStats(ctx context.Context, _ *sdk.CallToolRequest, in spaceStatsInput) (*sdk.CallToolResult, spaceStatsOut, error) {
	r, err := h.begin(ctx)
	if err != nil {
		return nil, spaceStatsOut{}, toolError(h.logger, "space_stats", err)
	}
	spaces, err := r.readableIDs(in.Spaces)
	if err != nil {
		return nil, spaceStatsOut{}, toolError(h.logger, "space_stats", err)
	}
	stats, err := h.know.SpaceStats(ctx, r.identity.TenantID, spaces)
	if err != nil {
		return nil, spaceStatsOut{}, toolError(h.logger, "space_stats", err)
	}

	out := make([]spaceStatOut, 0, len(stats))
	for _, st := range stats {
		out = append(out, spaceStatOut{
			Space:            r.spaceRefOf(st.SpaceID),
			Memories:         st.Memories,
			Cards:            st.Cards,
			DraftCards:       st.DraftCards,
			UnqualifiedCards: st.UnqualifiedCards,
			RecallTriggers:   st.RecallTriggers,
		})
	}
	return nil, spaceStatsOut{Spaces: out}, nil
}

// ---------------------------------------------------------------------------
// server_status
// ---------------------------------------------------------------------------

type serverStatusInput struct{}

type serverStatusOut struct {
	Name    string `json:"name"`
	Version string `json:"version"`
	// SemanticSearch 表示这个部署有没有接 embedding 服务。
	//
	// agent 需要它才能理解 card_recall 的行为：没接的时候召回退化成
	// 中文全文匹配，精度会明显下降但不是失灵。没有这个字段的话，
	// 「召回质量差」和「服务配错了」在 agent 眼里是同一件事。
	SemanticSearch bool `json:"semanticSearch"`
}

// handleServerStatus 不需要身份：它回答的是「这个服务是什么」，
// 不涉及任何租户数据。认证仍然发生在中间件里——只是这个 handler
// 不再去解析身份。
func (h *Host) handleServerStatus(_ context.Context, _ *sdk.CallToolRequest, _ serverStatusInput) (*sdk.CallToolResult, serverStatusOut, error) {
	return nil, serverStatusOut{
		Name:           serverName,
		Version:        h.version,
		SemanticSearch: h.semanticSearch,
	}, nil
}
