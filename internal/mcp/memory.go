package mcp

import (
	"context"
	"slices"
	"time"

	sdk "github.com/modelcontextprotocol/go-sdk/mcp"

	"claracore-server/internal/domain/external"
	"claracore-server/internal/domain/knowledge"
	"claracore-server/internal/platform"
)

// 本文件是 Memory 的 6 个工具（docs/03-mcp-contract.md §4.1）。
//
// 每个 handler 的形状都一样，而且**必须**一样：
//
//	begin(ctx)                    → 身份 + 权限（唯一入口）
//	writeTarget / readTargets     → 权限解析成确定的空间
//	know.Xxx(...)                 → 领域层做事
//	encodeXxx(...)                → 编码返回
//
// 中间不出现任何 if 业务判断。这一层多写一个 `if` 就是把一条规则
// 复制到了第二个地方（docs/05-code-structure.md §4）。

// ---------------------------------------------------------------------------
// memoria_create
// ---------------------------------------------------------------------------

type memoriaCreateInput struct {
	Title      string   `json:"title" jsonschema:"记忆标题，一句话说清这是什么"`
	Body       string   `json:"body" jsonschema:"记忆正文"`
	Labels     []string `json:"labels,omitempty" jsonschema:"标签，用于按主题收窄检索"`
	Space      string   `json:"space,omitempty" jsonschema:"目标空间 ID。不传即写入个人空间（whoami 的 defaultSpaceId）"`
	Supersedes string   `json:"supersedes,omitempty" jsonschema:"被这条新事实替代的旧 Memory ID。创建新记忆、旧记忆转 historical、建立 supersedes 链会原子提交"`
}

func (h *Host) handleMemoriaCreate(ctx context.Context, _ *sdk.CallToolRequest, in memoriaCreateInput) (*sdk.CallToolResult, memoryOut, error) {
	r, err := h.begin(ctx)
	if err != nil {
		return nil, memoryOut{}, toolError(h.logger, "memoria_create", err)
	}
	space, err := r.writeTarget(in.Space)
	if err != nil {
		return nil, memoryOut{}, toolError(h.logger, "memoria_create", err)
	}

	m, err := h.know.CreateMemory(ctx, knowledge.CreateMemoryInput{
		TenantID:       r.identity.TenantID,
		SpaceID:        space.ID,
		AuthorUserID:   r.identity.UserID,
		AuthorAgentID:  r.agentID(),
		Title:          in.Title,
		Body:           in.Body,
		Labels:         in.Labels,
		SupersedesID:   platform.ID(in.Supersedes),
		WritableSpaces: r.writableSpaces(),
	})
	if err != nil {
		return nil, memoryOut{}, toolError(h.logger, "memoria_create", err)
	}
	return nil, r.encodeMemory(m, nil), nil
}

// ---------------------------------------------------------------------------
// memoria_get
// ---------------------------------------------------------------------------

type memoriaGetInput struct {
	ID string `json:"id" jsonschema:"记忆 ID"`
}

func (h *Host) handleMemoriaGet(ctx context.Context, _ *sdk.CallToolRequest, in memoriaGetInput) (*sdk.CallToolResult, memoryOut, error) {
	r, err := h.begin(ctx)
	if err != nil {
		return nil, memoryOut{}, toolError(h.logger, "memoria_get", err)
	}
	// 取单条用**可读**集合：读一条自己改不了的内容是完全正常的事。
	spaces, err := r.readableIDs(nil)
	if err != nil {
		return nil, memoryOut{}, toolError(h.logger, "memoria_get", err)
	}
	m, links, err := h.know.GetMemory(ctx, r.identity.TenantID, platform.ID(in.ID), spaces)
	if err != nil {
		return nil, memoryOut{}, toolError(h.logger, "memoria_get", err)
	}
	return nil, r.encodeMemory(m, links), nil
}

// ---------------------------------------------------------------------------
// memoria_update
// ---------------------------------------------------------------------------

// memoriaUpdateInput 的字段全是指针：**nil = 不改，"" = 清空**。
//
// 用值类型的话没法表达「把正文清空」这个合法意图——它会和「这次不改正文」
// 变成同一个请求。Desktop 的 archive / restore / tag 都并进了这个工具
// （通过 status 与 labels 表达），所以它必须能精确表达"只改这一栏"。
type memoriaUpdateInput struct {
	ID      string `json:"id" jsonschema:"记忆 ID"`
	Version *int   `json:"version,omitempty" jsonschema:"上次读到的版本号。传了就是端到端的并发保护：期间被人改过会返回 VERSION_CONFLICT 而不是静默覆盖"`

	Title  *string   `json:"title,omitempty" jsonschema:"新标题。不传表示不改"`
	Body   *string   `json:"body,omitempty" jsonschema:"新正文。不传表示不改"`
	Status *string   `json:"status,omitempty" jsonschema:"active | archived | superseded | deleted。archived/superseded/deleted 都不进默认检索，但仍可按 ID 取到"`
	Labels *[]string `json:"labels,omitempty" jsonschema:"整体替换标签。不传表示不改，传空数组表示清空"`
}

func (h *Host) handleMemoriaUpdate(ctx context.Context, _ *sdk.CallToolRequest, in memoriaUpdateInput) (*sdk.CallToolResult, memoryOut, error) {
	r, err := h.begin(ctx)
	if err != nil {
		return nil, memoryOut{}, toolError(h.logger, "memoria_update", err)
	}
	m, err := h.know.UpdateMemory(ctx, knowledge.UpdateMemoryInput{
		TenantID:        r.identity.TenantID,
		ID:              platform.ID(in.ID),
		WritableSpaces:  r.writableSpaces(),
		ExpectedVersion: in.Version,
		Title:           in.Title,
		Body:            in.Body,
		Status:          in.Status,
		Labels:          in.Labels,
	})
	if err != nil {
		return nil, memoryOut{}, toolError(h.logger, "memoria_update", err)
	}
	return nil, r.encodeMemory(m, nil), nil
}

// ---------------------------------------------------------------------------
// memoria_delete
// ---------------------------------------------------------------------------

type memoriaDeleteInput struct {
	ID string `json:"id" jsonschema:"记忆 ID"`
}

type deleteOut struct {
	ID      string `json:"id"`
	Deleted bool   `json:"deleted"`
	// Note 说清楚这是软删除：agent 需要知道这条还能按 ID 取回来，
	// 否则它会以为自己刚做了一件不可逆的事而不敢用这个工具。
	Note string `json:"note"`
	// spaceID 只给 Host 边界写活动记录，不进 MCP 协议输出。
	spaceID platform.ID
}

func (h *Host) handleMemoriaDelete(ctx context.Context, _ *sdk.CallToolRequest, in memoriaDeleteInput) (*sdk.CallToolResult, deleteOut, error) {
	r, err := h.begin(ctx)
	if err != nil {
		return nil, deleteOut{}, toolError(h.logger, "memoria_delete", err)
	}
	deleted, err := h.know.DeleteMemory(ctx, r.identity.TenantID, platform.ID(in.ID), r.writableSpaces())
	if err != nil {
		return nil, deleteOut{}, toolError(h.logger, "memoria_delete", err)
	}
	return nil, deleteOut{
		ID:      in.ID,
		Deleted: true,
		Note:    "软删除：不再进默认检索，但仍可按 ID 取回，也可用 memoria_update 把 status 改回 active",
		spaceID: deleted.SpaceID,
	}, nil
}

// ---------------------------------------------------------------------------
// memoria_search
// ---------------------------------------------------------------------------

type memoriaSearchInput struct {
	Query    string   `json:"query" jsonschema:"检索词或一句自然语言描述"`
	Labels   []string `json:"labels,omitempty" jsonschema:"标签过滤，多个标签取交集（必须同时具备）"`
	Spaces   []string `json:"spaces,omitempty" jsonschema:"限定空间 ID。不传即覆盖全部可读空间；传了其中一个不可读会报错而不是静默跳过"`
	Limit    int      `json:"limit,omitempty" jsonschema:"返回条数，默认 10，上限 50"`
	TimeView string   `json:"timeView,omitempty" jsonschema:"current | historical | all。默认 current；historical 只看被替代事实"`
}

type memoryHitOut struct {
	memoryOut
	Score float64 `json:"score"`
	// Semantic 区分「向量算出来的相关」与「字面撞上了」。
	// 两种分数不是一个量纲，所以结果分两组：语义命中在前，全文命中在后。
	Semantic bool `json:"semantic"`
}

type memoriaSearchOut struct {
	Hits []memoryHitOut `json:"hits"`
	// Matches is the forward-compatible unified view. Semantic matches from
	// Memory, Card, and body-free external mirrors share one score space. Text
	// fallbacks remain explicitly grouped because MySQL scores from separate
	// indexes are not comparable.
	Matches []knowledgeMatchOut `json:"matches"`

	// SemanticAvailable = false 表示这次没用上向量（没配 embedding 服务，
	// 或者算查询向量时外部服务失败）。**必须看这个字段**：
	// 「没搜到」和「语义那一半根本没跑」是完全不同的两件事。
	SemanticAvailable     bool `json:"semanticAvailable"`
	CardSemanticAvailable bool `json:"cardSemanticAvailable"`

	// CardMatches 是「另有 N 条卡片命中，用 card_search 查看」。
	// -1 表示这次没统计（memory 命中已经够多了）。
	// 它解决的是真实风险：agent 搜一次 memory 没找到就以为没有，
	// 而知识卡片才是这个产品的核心载体。
	CardMatches               int    `json:"cardMatches"`
	ExternalStatus            string `json:"externalStatus"`
	ExternalSemanticAvailable bool   `json:"externalSemanticAvailable"`
}

type externalHitOut struct {
	ID                string   `json:"id"`
	Space             spaceRef `json:"space"`
	Title             string   `json:"title"`
	Kind              string   `json:"kind"`
	SourceName        string   `json:"sourceName"`
	SourceSystem      string   `json:"sourceSystem"`
	SourceRef         string   `json:"sourceRef"`
	Revision          string   `json:"revision"`
	UpstreamUpdatedAt string   `json:"upstreamUpdatedAt,omitempty"`
	IndexedAt         string   `json:"indexedAt"`
	LastSucceededAt   string   `json:"lastSucceededAt,omitempty"`
	LastSyncStatus    string   `json:"lastSyncStatus"`
	Freshness         string   `json:"freshness"`
}

type knowledgeMatchOut struct {
	Type     string          `json:"type"`
	Group    string          `json:"group"`
	Score    float64         `json:"score"`
	Semantic bool            `json:"semantic"`
	Memory   *memoryOut      `json:"memory,omitempty"`
	Card     *cardOut        `json:"card,omitempty"`
	External *externalHitOut `json:"external,omitempty"`
	spaceID  platform.ID
	recency  time.Time
	id       platform.ID
}

func (h *Host) handleMemoriaSearch(ctx context.Context, _ *sdk.CallToolRequest, in memoriaSearchInput) (*sdk.CallToolResult, memoriaSearchOut, error) {
	r, err := h.begin(ctx)
	if err != nil {
		return nil, memoriaSearchOut{}, toolError(h.logger, "memoria_search", err)
	}
	spaces, err := r.readTargets(in.Spaces)
	if err != nil {
		return nil, memoriaSearchOut{}, toolError(h.logger, "memoria_search", err)
	}

	res, err := h.know.SearchMemories(ctx, knowledge.SearchMemoriesInput{
		TenantID: r.identity.TenantID,
		Spaces:   spaces,
		Query:    in.Query,
		Labels:   in.Labels,
		Limit:    in.Limit,
		TimeView: in.TimeView,
	})
	if err != nil {
		return nil, memoriaSearchOut{}, toolError(h.logger, "memoria_search", err)
	}
	cardRes := knowledge.CardSearchResult{}
	if in.TimeView != "historical" {
		cardRes, err = h.know.SearchCards(ctx, knowledge.SearchCardsInput{
			TenantID: r.identity.TenantID, Spaces: spaces, Query: in.Query, Labels: in.Labels, Limit: in.Limit,
		})
		if err != nil {
			return nil, memoriaSearchOut{}, toolError(h.logger, "memoria_search", err)
		}
	}
	externalStatus := "unconfigured"
	externalRes := external.SearchResult{}
	if h.external != nil {
		externalStatus = "mirror_only"
		if h.externalConfigured {
			externalStatus = "available"
		}
		if in.TimeView == "historical" {
			externalStatus = "excluded_by_historical_view"
		} else if len(in.Labels) > 0 {
			externalStatus = "excluded_by_label_filter"
		} else {
			ids := make([]platform.ID, 0, len(spaces))
			for _, sp := range spaces {
				ids = append(ids, sp.ID)
			}
			externalRes, err = h.external.Search(ctx, external.SearchInput{
				TenantID: r.identity.TenantID, SpaceIDs: ids, Query: in.Query, Limit: in.Limit,
			})
			if err != nil {
				h.logger.Error("外部知识检索降级", "error", err)
				externalStatus = "degraded"
				externalRes = external.SearchResult{}
			}
		}
	}

	hits := make([]memoryHitOut, 0, len(res.Hits))
	for _, hit := range res.Hits {
		hits = append(hits, memoryHitOut{
			memoryOut: r.encodeMemory(hit.Memory, nil),
			Score:     hit.Score,
			Semantic:  hit.Semantic,
		})
	}
	matches := r.encodeKnowledgeMatches(res.Hits, cardRes.Hits, externalRes.Hits, spaces, in.Limit)
	return nil, memoriaSearchOut{
		Hits:                      hits,
		Matches:                   matches,
		SemanticAvailable:         res.SemanticAvailable,
		CardSemanticAvailable:     cardRes.SemanticAvailable,
		CardMatches:               res.CardMatches,
		ExternalStatus:            externalStatus,
		ExternalSemanticAvailable: externalRes.SemanticAvailable,
	}, nil
}

func (r request) encodeKnowledgeMatches(memories []knowledge.MemoryHit, cards []knowledge.CardHit, externals []external.SearchHit, spaces []knowledge.SearchSpace, requestedLimit int) []knowledgeMatchOut {
	limit := requestedLimit
	if limit <= 0 {
		limit = 10
	}
	if limit > 50 {
		limit = 50
	}
	ranks := make(map[platform.ID]int, len(spaces))
	var semantic, memoryText, cardText []knowledgeMatchOut
	for _, sp := range spaces {
		ranks[sp.ID] = sp.Rank
	}
	for _, hit := range memories {
		encoded := r.encodeMemory(hit.Memory, nil)
		item := knowledgeMatchOut{Type: "memory", Score: hit.Score, Semantic: hit.Semantic, Memory: &encoded, spaceID: hit.Memory.SpaceID, recency: hit.Memory.UpdatedAt, id: hit.Memory.ID}
		if hit.Semantic {
			item.Group = "semantic"
			semantic = append(semantic, item)
		} else {
			item.Group = "memory_text"
			memoryText = append(memoryText, item)
		}
	}
	for _, hit := range cards {
		encoded := r.encodeCard(hit.Card, nil)
		item := knowledgeMatchOut{Type: "card", Score: hit.Score, Semantic: hit.Semantic, Card: &encoded, spaceID: hit.Card.SpaceID, recency: hit.Card.UpdatedAt, id: hit.Card.ID}
		if hit.Semantic {
			item.Group = "semantic"
			semantic = append(semantic, item)
		} else {
			item.Group = "card_text"
			cardText = append(cardText, item)
		}
	}
	for _, hit := range externals {
		encoded := r.encodeExternalHit(hit)
		semantic = append(semantic, knowledgeMatchOut{Type: "external", Group: "semantic", Score: hit.Score, Semantic: true, External: &encoded, spaceID: hit.Document.SpaceID, recency: hit.Document.UpdatedAt, id: hit.Document.ID})
	}
	slices.SortFunc(semantic, func(a, b knowledgeMatchOut) int {
		if c := knowledge.CompareSemanticScore(a.Score, b.Score); c != 0 {
			return c
		}
		if ranks[a.spaceID] != ranks[b.spaceID] {
			if ranks[a.spaceID] > ranks[b.spaceID] {
				return -1
			}
			return 1
		}
		if !a.recency.Equal(b.recency) {
			if a.recency.After(b.recency) {
				return -1
			}
			return 1
		}
		if a.Type != b.Type {
			return stringCompare(a.Type, b.Type)
		}
		return stringCompare(string(a.id), string(b.id))
	})
	out := append(semantic, memoryText...)
	out = append(out, cardText...)
	if len(out) > limit {
		out = out[:limit]
	}
	return out
}

func stringCompare(a, b string) int {
	if a < b {
		return -1
	}
	if a > b {
		return 1
	}
	return 0
}

func (r request) encodeExternalHit(hit external.SearchHit) externalHitOut {
	freshness := "last_sync_succeeded"
	if hit.LastSyncStatus == external.SyncRunning {
		freshness = "refresh_running_from_last_success"
	}
	if hit.LastSyncStatus == external.SyncFailed {
		freshness = "last_success_preserved_after_failure"
	}
	out := externalHitOut{
		ID: string(hit.Document.ID), Space: r.spaceRefOf(hit.Document.SpaceID), Title: hit.Document.Title,
		Kind: hit.Document.Kind, SourceName: hit.SourceName, SourceSystem: hit.SourceSystem,
		SourceRef: hit.Document.SourceRef, Revision: hit.Document.Revision,
		UpstreamUpdatedAt: hit.Document.UpstreamUpdatedAt, IndexedAt: stamp(hit.Document.UpdatedAt),
		LastSyncStatus: hit.LastSyncStatus, Freshness: freshness,
	}
	if hit.LastSucceededAt != nil {
		out.LastSucceededAt = stamp(*hit.LastSucceededAt)
	}
	return out
}

// ---------------------------------------------------------------------------
// memoria_link
// ---------------------------------------------------------------------------

type memoriaLinkInput struct {
	FromType string `json:"fromType,omitempty" jsonschema:"起点类型：memory（默认）或 card"`
	FromID   string `json:"fromId" jsonschema:"起点 ID"`
	ToType   string `json:"toType,omitempty" jsonschema:"终点类型：memory（默认）或 card"`
	ToID     string `json:"toId" jsonschema:"终点 ID"`
	Kind     string `json:"kind" jsonschema:"related | causes | evolved-from | contradicts | part-of"`
	Note     string `json:"note,omitempty" jsonschema:"为什么建立这条关联"`
	Remove   bool   `json:"remove,omitempty" jsonschema:"true 表示删除这条关联。删除是幂等的：本来就不存在也返回成功"`
}

type linkOut struct {
	Link    linkRef `json:"link"`
	Removed bool    `json:"removed,omitempty"`
}

func (h *Host) handleMemoriaLink(ctx context.Context, _ *sdk.CallToolRequest, in memoriaLinkInput) (*sdk.CallToolResult, linkOut, error) {
	r, err := h.begin(ctx)
	if err != nil {
		return nil, linkOut{}, toolError(h.logger, "memoria_link", err)
	}
	// 关联要求两端**可读**而非可写：关联是一句关于世界的陈述
	// （"这条导致了那条"），不修改任何一端的内容。
	spaces, err := r.readableIDs(nil)
	if err != nil {
		return nil, linkOut{}, toolError(h.logger, "memoria_link", err)
	}

	input := knowledge.LinkInput{
		TenantID:       r.identity.TenantID,
		From:           knowledge.Ref{Type: defaultEntityType(in.FromType), ID: platform.ID(in.FromID)},
		To:             knowledge.Ref{Type: defaultEntityType(in.ToType), ID: platform.ID(in.ToID)},
		Kind:           in.Kind,
		Note:           in.Note,
		ReadableSpaces: spaces,
	}
	ref := linkRef{
		FromType: input.From.Type, FromID: in.FromID,
		ToType: input.To.Type, ToID: in.ToID,
		Kind: in.Kind, Note: in.Note,
	}

	if in.Remove {
		if err := h.know.DeleteLink(ctx, input); err != nil {
			return nil, linkOut{}, toolError(h.logger, "memoria_link", err)
		}
		return nil, linkOut{Link: ref, Removed: true}, nil
	}
	if _, err := h.know.CreateLink(ctx, input); err != nil {
		return nil, linkOut{}, toolError(h.logger, "memoria_link", err)
	}
	return nil, linkOut{Link: ref}, nil
}

// defaultEntityType 让 memory 成为省略时的默认。
//
// 这是一个**取默认值**而不是业务判断：绝大多数关联发生在记忆之间，
// 而工具名是 memoria_link。非法取值不在这里拦——Ref.Validate() 会拦，
// 在这里也拦一次就是把同一条规则写了两遍。
func defaultEntityType(t string) string {
	if t == "" {
		return knowledge.EntityMemory
	}
	return t
}
