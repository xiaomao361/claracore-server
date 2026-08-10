package mcp

import (
	"context"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"claracore-server/internal/domain/knowledge"
	"claracore-server/internal/platform"
)

// 本文件是 Card 的 5 个工具（docs/03-mcp-contract.md §4.2）。
//
// Card 与 Memory 是**并列的一等公民**，不是它的子类型：结构、召回机制、
// 生命周期都不同（01-architecture.md §1.3）。所以它有自己的一套工具，
// 而不是给 memoria_* 加一个 type 参数。

// ---------------------------------------------------------------------------
// card_create
// ---------------------------------------------------------------------------

type cardCreateInput struct {
	Title    string `json:"title" jsonschema:"卡片标题，一句话说清这条经验是什么"`
	CardType string `json:"cardType" jsonschema:"卡片类型，如「Codex 协作安全」「工程约定」"`
	Source   string `json:"source,omitempty" jsonschema:"来源，如某次复盘、某个文档"`

	Original      string `json:"original,omitempty" jsonschema:"作者原意：未经整理的现场记录"`
	Distilled     string `json:"distilled,omitempty" jsonschema:"提炼：这件事到底是什么"`
	Understanding string `json:"understanding,omitempty" jsonschema:"理解：你自己想明白的那一层。这一栏为空的卡只是摘要，不是合格卡片"`
	WhyKeep       string `json:"whyKeep,omitempty" jsonschema:"为什么值得保留"`
	Unresolved    string `json:"unresolved,omitempty" jsonschema:"还没解决的问题"`

	RecallTriggers []string `json:"recallTriggers,omitempty" jsonschema:"未来什么情境下该想起这张卡。每条单独做向量匹配，是 card_recall 精度的来源——写具体的场景，不要写关键词"`
	Labels         []string `json:"labels,omitempty" jsonschema:"标签"`
	Status         string   `json:"status,omitempty" jsonschema:"draft | active | deprecated。不传即 active（手写默认，作者自管）；摄取产生的内容应显式传 draft，它不进默认检索"`
	Space          string   `json:"space,omitempty" jsonschema:"目标空间 ID。不传即写入个人空间"`
}

func (h *Host) handleCardCreate(ctx context.Context, _ *sdk.CallToolRequest, in cardCreateInput) (*sdk.CallToolResult, cardOut, error) {
	r, err := h.begin(ctx)
	if err != nil {
		return nil, cardOut{}, toolError(h.logger, "card_create", err)
	}
	space, err := r.writeTarget(in.Space)
	if err != nil {
		return nil, cardOut{}, toolError(h.logger, "card_create", err)
	}

	c, err := h.know.CreateCard(ctx, knowledge.CreateCardInput{
		TenantID:      r.identity.TenantID,
		SpaceID:       space.ID,
		AuthorUserID:  r.identity.UserID,
		AuthorAgentID: r.agentID(),
		Title:         in.Title,
		CardType:      in.CardType,
		Source:        in.Source,
		Original:      in.Original,
		Distilled:     in.Distilled,
		Understanding: in.Understanding,
		WhyKeep:       in.WhyKeep,
		Unresolved:    in.Unresolved,
		// Status 留空由领域层填 active。**不在这里补默认值**：
		// 「手写默认 active、摄取显式 draft」是一条业务规则，
		// 它已经在 CreateCard 里了，这儿再写一次就是第二份。
		Status:         in.Status,
		RecallTriggers: in.RecallTriggers,
		Labels:         in.Labels,
	})
	if err != nil {
		return nil, cardOut{}, toolError(h.logger, "card_create", err)
	}
	return nil, r.encodeCard(c, nil), nil
}

// ---------------------------------------------------------------------------
// card_get
// ---------------------------------------------------------------------------

type cardGetInput struct {
	ID string `json:"id" jsonschema:"卡片 ID"`
}

func (h *Host) handleCardGet(ctx context.Context, _ *sdk.CallToolRequest, in cardGetInput) (*sdk.CallToolResult, cardOut, error) {
	r, err := h.begin(ctx)
	if err != nil {
		return nil, cardOut{}, toolError(h.logger, "card_get", err)
	}
	spaces, err := r.readableIDs(nil)
	if err != nil {
		return nil, cardOut{}, toolError(h.logger, "card_get", err)
	}
	c, links, err := h.know.GetCard(ctx, r.identity.TenantID, platform.ID(in.ID), spaces)
	if err != nil {
		return nil, cardOut{}, toolError(h.logger, "card_get", err)
	}
	return nil, r.encodeCard(c, links), nil
}

// ---------------------------------------------------------------------------
// card_update
// ---------------------------------------------------------------------------

type cardUpdateInput struct {
	ID      string `json:"id" jsonschema:"卡片 ID"`
	Version *int   `json:"version,omitempty" jsonschema:"上次读到的版本号，用于并发保护"`

	Title         *string `json:"title,omitempty"`
	CardType      *string `json:"cardType,omitempty"`
	Status        *string `json:"status,omitempty" jsonschema:"draft | active | deprecated。把摄取来的 draft 改成 active 就是「审过了」"`
	Source        *string `json:"source,omitempty"`
	Original      *string `json:"original,omitempty"`
	Distilled     *string `json:"distilled,omitempty"`
	Understanding *string `json:"understanding,omitempty" jsonschema:"补上这一栏，这张卡才从摘要变成合格卡片"`
	WhyKeep       *string `json:"whyKeep,omitempty"`
	Unresolved    *string `json:"unresolved,omitempty"`

	Labels *[]string `json:"labels,omitempty" jsonschema:"整体替换标签。不传表示不改"`
	// RecallTriggers 整体替换。传空数组会清空全部召回条件——
	// 那张卡从此再也不会被 card_recall 命中，是个有意义但代价很大的动作。
	RecallTriggers *[]string `json:"recallTriggers,omitempty" jsonschema:"整体替换召回条件。不传表示不改；传空数组会让这张卡再也不会被 card_recall 命中"`
}

func (h *Host) handleCardUpdate(ctx context.Context, _ *sdk.CallToolRequest, in cardUpdateInput) (*sdk.CallToolResult, cardOut, error) {
	r, err := h.begin(ctx)
	if err != nil {
		return nil, cardOut{}, toolError(h.logger, "card_update", err)
	}
	c, err := h.know.UpdateCard(ctx, knowledge.UpdateCardInput{
		TenantID:        r.identity.TenantID,
		ID:              platform.ID(in.ID),
		WritableSpaces:  r.writableSpaces(),
		ExpectedVersion: in.Version,
		Title:           in.Title,
		CardType:        in.CardType,
		Status:          in.Status,
		Source:          in.Source,
		Original:        in.Original,
		Distilled:       in.Distilled,
		Understanding:   in.Understanding,
		WhyKeep:         in.WhyKeep,
		Unresolved:      in.Unresolved,
		Labels:          in.Labels,
		RecallTriggers:  in.RecallTriggers,
	})
	if err != nil {
		return nil, cardOut{}, toolError(h.logger, "card_update", err)
	}
	return nil, r.encodeCard(c, nil), nil
}

// ---------------------------------------------------------------------------
// card_search —— 「关于 X 的卡片有哪些」
// ---------------------------------------------------------------------------

type cardSearchInput struct {
	Query  string   `json:"query" jsonschema:"检索词。匹配的是卡片正文（标题、提炼、理解）"`
	Labels []string `json:"labels,omitempty" jsonschema:"标签过滤，多个标签取交集"`
	Spaces []string `json:"spaces,omitempty" jsonschema:"限定空间 ID。不传即覆盖全部可读空间"`
	Limit  int      `json:"limit,omitempty" jsonschema:"返回条数，默认 10，上限 50"`
}

type cardHitOut struct {
	cardOut
	Score    float64 `json:"score"`
	Semantic bool    `json:"semantic"`
	// Trigger 只有 card_recall 会填：它是「为什么现在给你看这张卡」。
	Trigger string `json:"matchedTrigger,omitempty"`
}

type cardSearchOut struct {
	Hits              []cardHitOut `json:"hits"`
	SemanticAvailable bool         `json:"semanticAvailable"`
}

func (h *Host) handleCardSearch(ctx context.Context, _ *sdk.CallToolRequest, in cardSearchInput) (*sdk.CallToolResult, cardSearchOut, error) {
	r, err := h.begin(ctx)
	if err != nil {
		return nil, cardSearchOut{}, toolError(h.logger, "card_search", err)
	}
	spaces, err := r.readTargets(in.Spaces)
	if err != nil {
		return nil, cardSearchOut{}, toolError(h.logger, "card_search", err)
	}
	res, err := h.know.SearchCards(ctx, knowledge.SearchCardsInput{
		TenantID: r.identity.TenantID,
		Spaces:   spaces,
		Query:    in.Query,
		Labels:   in.Labels,
		Limit:    in.Limit,
	})
	if err != nil {
		return nil, cardSearchOut{}, toolError(h.logger, "card_search", err)
	}
	return nil, cardSearchOut{Hits: r.encodeCardHits(res.Hits), SemanticAvailable: res.SemanticAvailable}, nil
}

// ---------------------------------------------------------------------------
// card_recall —— 本产品的核心能力
// ---------------------------------------------------------------------------

type cardRecallInput struct {
	// Situation 传的是**当前正在做的事**，不是关键词。
	//
	// 它匹配的是作者显式写下的召回条件（每条单独 embedding），
	// 而不是卡片全文——一张五十行复盘的整体向量会把
	// 「即将执行删除操作」这类关键信号稀释掉。
	Situation string   `json:"situation" jsonschema:"你当前的工作情境，用完整的一句话描述正在做什么，例如「用户让我清理 ~/.cache 释放磁盘，我准备执行 rm -rf」。不要传关键词"`
	Spaces    []string `json:"spaces,omitempty" jsonschema:"限定空间 ID。不传即覆盖全部可读空间"`
	Limit     int      `json:"limit,omitempty" jsonschema:"返回卡片数，默认 10，上限 50"`
}

func (h *Host) handleCardRecall(ctx context.Context, _ *sdk.CallToolRequest, in cardRecallInput) (*sdk.CallToolResult, cardSearchOut, error) {
	r, err := h.begin(ctx)
	if err != nil {
		return nil, cardSearchOut{}, toolError(h.logger, "card_recall", err)
	}
	spaces, err := r.readTargets(in.Spaces)
	if err != nil {
		return nil, cardSearchOut{}, toolError(h.logger, "card_recall", err)
	}
	res, err := h.know.RecallCards(ctx, knowledge.RecallCardsInput{
		TenantID:  r.identity.TenantID,
		Spaces:    spaces,
		Situation: in.Situation,
		Limit:     in.Limit,
	})
	if err != nil {
		return nil, cardSearchOut{}, toolError(h.logger, "card_recall", err)
	}
	return nil, cardSearchOut{Hits: r.encodeCardHits(res.Hits), SemanticAvailable: res.SemanticAvailable}, nil
}

func (r request) encodeCardHits(hits []knowledge.CardHit) []cardHitOut {
	out := make([]cardHitOut, 0, len(hits))
	for _, hit := range hits {
		out = append(out, cardHitOut{
			cardOut:  r.encodeCard(hit.Card, nil),
			Score:    hit.Score,
			Semantic: hit.Semantic,
			Trigger:  hit.Trigger,
		})
	}
	return out
}
