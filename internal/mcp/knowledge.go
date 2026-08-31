package mcp

import (
	"context"
	"time"

	"claracore-server/internal/domain/governance"
	"claracore-server/internal/domain/knowledge"
	"claracore-server/internal/platform"
)

// knowledgeService 是 Host 需要的核心域能力，**按使用方需要定义**
// （docs/05-code-structure.md §3.1）。方法多是因为工具多，不是因为
// 这里塞了一个万能接口——每一条都对应一个 MCP 工具。
type knowledgeService interface {
	CreateMemory(ctx context.Context, in knowledge.CreateMemoryInput) (knowledge.Memory, error)
	GetMemory(ctx context.Context, tenantID, id platform.ID, spaces []platform.ID) (knowledge.Memory, []knowledge.Link, error)
	UpdateMemory(ctx context.Context, in knowledge.UpdateMemoryInput) (knowledge.Memory, error)
	DeleteMemory(ctx context.Context, tenantID, id platform.ID, writableSpaces []platform.ID) (knowledge.Memory, error)
	SearchMemories(ctx context.Context, in knowledge.SearchMemoriesInput) (knowledge.MemorySearchResult, error)
	RecallMemoryForContext(ctx context.Context, in knowledge.SearchMemoriesInput) (knowledge.AutomaticMemoryRecallResult, error)

	CreateCard(ctx context.Context, in knowledge.CreateCardInput) (knowledge.Card, error)
	GetCard(ctx context.Context, tenantID, id platform.ID, spaces []platform.ID) (knowledge.Card, []knowledge.Link, error)
	UpdateCard(ctx context.Context, in knowledge.UpdateCardInput) (knowledge.Card, error)
	SearchCards(ctx context.Context, in knowledge.SearchCardsInput) (knowledge.CardSearchResult, error)
	RecallCards(ctx context.Context, in knowledge.RecallCardsInput) (knowledge.CardSearchResult, error)
	RecallCardForContext(ctx context.Context, in knowledge.RecallCardsInput) (knowledge.AutomaticRecallResult, error)

	CreateLink(ctx context.Context, in knowledge.LinkInput) (knowledge.Link, error)
	DeleteLink(ctx context.Context, in knowledge.LinkInput) error

	Promote(ctx context.Context, in knowledge.PromoteInput) (knowledge.Ref, error)
	SpaceStats(ctx context.Context, tenantID platform.ID, spaces []platform.ID) ([]knowledge.SpaceStats, error)
}

// ---------------------------------------------------------------------------
// 编排：把权限解析成确定的空间，再调领域层
// ---------------------------------------------------------------------------

// request 是一次工具调用已经解析好的上下文。
//
// 每个 handler 的第一行都是 h.begin(ctx)：拿身份、解析权限。
// 抽成一个函数不只是省几行——**它保证每个工具都走了同一条权限解析路径**。
// 少写一次 Scope 调用不会有编译错误，只会有一个不带权限判断的工具。
type request struct {
	identity governance.Identity
	scope    governance.SpaceScope
}

func (h *Host) begin(ctx context.Context) (request, error) {
	id, ok := identityFrom(ctx)
	if !ok {
		return request{}, errNoIdentity
	}
	scope, err := h.gov.Scope(ctx, id)
	if err != nil {
		return request{}, err
	}
	return request{identity: id, scope: scope}, nil
}

// writeTarget 把可选的 space 参数解析成一个确定的空间。
// 空字符串即个人空间（docs/03-mcp-contract.md §3.1）。
func (r request) writeTarget(requested string) (governance.Space, error) {
	return r.scope.ResolveWriteTarget(platform.ID(requested))
}

// writableSpaces 是全部可写空间，用于**定位**要修改的内容。
//
// 不是「先按可读定位，再判断能不能写」：对一条自己改不了的内容，
// 「不存在」与「无权修改」给调用方的下一步是同一件事，
// 分开返回等于确认「它存在，只是你够不着」（05 §2.2）。
func (r request) writableSpaces() []platform.ID {
	var out []platform.ID
	for _, a := range r.scope.All() {
		if a.CanWrite {
			out = append(out, a.Space.ID)
		}
	}
	return out
}

// readTargets 把可选的 spaces 参数解析成带排序权重的可读空间集合。
//
// Rank 来自 governance.Space.SearchRank()——**权重的定义只有那一份**。
// 在这里写 `if space.Type == "team" { rank = 2 }` 就是第二份，
// 而两份规则永远只会改对其中一份。
func (r request) readTargets(requested []string) ([]knowledge.SearchSpace, error) {
	ids, err := r.scope.ResolveReadTargets(toIDs(requested))
	if err != nil {
		return nil, err
	}
	out := make([]knowledge.SearchSpace, 0, len(ids))
	for _, id := range ids {
		space, ok := r.scope.Space(id)
		if !ok {
			// ResolveReadTargets 刚确认过可读，取不到说明 scope 内部不一致。
			// 跳过而不是崩：少一个空间的检索结果，好过整次调用失败。
			continue
		}
		out = append(out, knowledge.SearchSpace{ID: id, Rank: space.SearchRank()})
	}
	return out, nil
}

// readableIDs 是不需要排序权重时的可读空间集合（get 类工具用）。
func (r request) readableIDs(requested []string) ([]platform.ID, error) {
	return r.scope.ResolveReadTargets(toIDs(requested))
}

// agentID 是写入时记的「哪个 agent 写的」。
//
// agent 是审计维度不是权限维度（01 §11.1）：它只回答「这条内容是谁的
// 哪个 agent 写的」，不参与任何判断。
func (r request) agentID() *platform.ID {
	id := r.identity.AgentID
	if id == "" {
		return nil
	}
	return &id
}

func toIDs(ss []string) []platform.ID {
	if len(ss) == 0 {
		return nil
	}
	out := make([]platform.ID, 0, len(ss))
	for _, s := range ss {
		out = append(out, platform.ID(s))
	}
	return out
}

// ---------------------------------------------------------------------------
// 结果编码
// ---------------------------------------------------------------------------

// spaceRef 是检索结果里的来源空间标注。
//
// **每个返回内容的工具都必须带它**（docs/03-mcp-contract.md §3.2）：
// agent 据此判断权威性——team = 已达成共识的约定，personal = 个人经验。
// 缺了这个字段，agent 无法正确加权，一条个人的随手记和一条团队约定
// 在它眼里一样重。
type spaceRef struct {
	ID   string `json:"id"`
	Type string `json:"type"` // team | project | personal
	Name string `json:"name"`
}

// spaceRefOf 从 scope 里查出空间信息。
//
// 查不到时只回 ID：这种情况只会发生在数据不一致时（内容指向一个
// scope 里没有的空间），而那时最该做的是把内容返回给人看见，
// 不是因为标注不全就整个失败。
func (r request) spaceRefOf(id platform.ID) spaceRef {
	if s, ok := r.scope.Space(id); ok {
		return spaceRef{ID: string(s.ID), Type: s.Type, Name: s.Name}
	}
	return spaceRef{ID: string(id)}
}

type originRef struct {
	Type string `json:"type"`
	ID   string `json:"id"`
}

// linkRef 是关联的一端。
//
// **刻意只给 ID 和 kind，不解析对端标题**：关联的对端可能落在调用方
// 无权访问的空间里（关联只要求两端可读**于创建时**，权限之后可能变化）。
// 要展示标题就得再判一次权限，而那属于调用方拿着 ID 自己去 get——
// 它会在那里撞上正常的权限检查。
type linkRef struct {
	FromType string `json:"fromType"`
	FromID   string `json:"fromId"`
	ToType   string `json:"toType"`
	ToID     string `json:"toId"`
	Kind     string `json:"kind"`
	Note     string `json:"note,omitempty"`
}

func encodeLinks(links []knowledge.Link) []linkRef {
	out := make([]linkRef, 0, len(links))
	for _, l := range links {
		out = append(out, linkRef{
			FromType: l.From.Type, FromID: string(l.From.ID),
			ToType: l.To.Type, ToID: string(l.To.ID),
			Kind: l.Kind, Note: l.Note,
		})
	}
	return out
}

type memoryOut struct {
	ID     string   `json:"id"`
	Space  spaceRef `json:"space"`
	Title  string   `json:"title"`
	Body   string   `json:"body"`
	Status string   `json:"status"`
	Labels []string `json:"labels"`

	// Version 要回给调用方：更新时把它传回来才能拿到端到端的并发保护
	// （docs/03-mcp-contract.md §3.4）。不给的话乐观锁的窗口
	// 就只剩服务端一次读-改-写那么宽。
	Version int `json:"version"`

	AuthorUserID  string     `json:"authorUserId"`
	AuthorAgentID string     `json:"authorAgentId,omitempty"`
	CopiedFrom    *originRef `json:"copiedFrom,omitempty"`
	CreatedAt     string     `json:"createdAt"`
	UpdatedAt     string     `json:"updatedAt"`

	Links []linkRef `json:"links,omitempty"`
}

func (r request) encodeMemory(m knowledge.Memory, links []knowledge.Link) memoryOut {
	out := memoryOut{
		ID:           string(m.ID),
		Space:        r.spaceRefOf(m.SpaceID),
		Title:        m.Title,
		Body:         m.Body,
		Status:       m.Status,
		Labels:       nonNil(m.Labels),
		Version:      m.Version,
		AuthorUserID: string(m.AuthorUserID),
		CreatedAt:    stamp(m.CreatedAt),
		UpdatedAt:    stamp(m.UpdatedAt),
	}
	if m.AuthorAgentID != nil {
		out.AuthorAgentID = string(*m.AuthorAgentID)
	}
	if m.CopiedFrom != nil {
		out.CopiedFrom = &originRef{Type: m.CopiedFrom.Type, ID: string(m.CopiedFrom.ID)}
	}
	if len(links) > 0 {
		out.Links = encodeLinks(links)
	}
	return out
}

type cardOut struct {
	ID       string   `json:"id"`
	Space    spaceRef `json:"space"`
	Title    string   `json:"title"`
	CardType string   `json:"cardType"`
	Status   string   `json:"status"`
	Source   string   `json:"source,omitempty"`

	Original      string `json:"original,omitempty"`
	Distilled     string `json:"distilled,omitempty"`
	Understanding string `json:"understanding,omitempty"`
	WhyKeep       string `json:"whyKeep,omitempty"`
	Unresolved    string `json:"unresolved,omitempty"`

	// RecallTriggers 是这张卡最要紧的部分：作者显式写下的
	// 「未来什么时候该想起它」。返回一张不带触发条件的卡，
	// 等于把这个产品的核心优势藏了起来。
	RecallTriggers []string `json:"recallTriggers"`

	// Qualified 就是「理解」栏非空。把它算好回传而不是让调用方
	// 自己判断 understanding 是否为空——那条质量分界线只该有一处定义。
	Qualified bool `json:"qualified"`

	Labels        []string   `json:"labels"`
	Version       int        `json:"version"`
	AuthorUserID  string     `json:"authorUserId"`
	AuthorAgentID string     `json:"authorAgentId,omitempty"`
	CopiedFrom    *originRef `json:"copiedFrom,omitempty"`
	CreatedAt     string     `json:"createdAt"`
	UpdatedAt     string     `json:"updatedAt"`

	Links []linkRef `json:"links,omitempty"`
}

func (r request) encodeCard(c knowledge.Card, links []knowledge.Link) cardOut {
	triggers := make([]string, 0, len(c.RecallTriggers))
	for _, t := range c.RecallTriggers {
		triggers = append(triggers, t.Text)
	}
	out := cardOut{
		ID:             string(c.ID),
		Space:          r.spaceRefOf(c.SpaceID),
		Title:          c.Title,
		CardType:       c.CardType,
		Status:         c.Status,
		Source:         c.Source,
		Original:       c.Original,
		Distilled:      c.Distilled,
		Understanding:  c.Understanding,
		WhyKeep:        c.WhyKeep,
		Unresolved:     c.Unresolved,
		RecallTriggers: triggers,
		Qualified:      c.IsQualified(),
		Labels:         nonNil(c.Labels),
		Version:        c.Version,
		AuthorUserID:   string(c.AuthorUserID),
		CreatedAt:      stamp(c.CreatedAt),
		UpdatedAt:      stamp(c.UpdatedAt),
	}
	if c.AuthorAgentID != nil {
		out.AuthorAgentID = string(*c.AuthorAgentID)
	}
	if c.CopiedFrom != nil {
		out.CopiedFrom = &originRef{Type: c.CopiedFrom.Type, ID: string(c.CopiedFrom.ID)}
	}
	if len(links) > 0 {
		out.Links = encodeLinks(links)
	}
	return out
}

// stamp 统一成 RFC3339 UTC。
//
// 存储层已经全 UTC，这里再 .UTC() 一次是防御：某天有人在中间层
// 塞进一个本地时区的时间，那会变成一条只在跨时区时才出错的 bug
// （Desktop 的 localDateForTimezone 教训）。
func stamp(t time.Time) string { return t.UTC().Format(time.RFC3339) }

// nonNil 让空列表编码成 [] 而不是 null。
//
// 不是审美：agent 侧 `for x in labels` 撞上 null 会炸，而
// 「没有标签」是完全正常的状态，不该让调用方每次都判空。
func nonNil(ss []string) []string {
	if ss == nil {
		return []string{}
	}
	return ss
}

// pointer 把「传了空字符串」和「没传」区分开。
//
// 领域层用 *string 表达这个区别：nil = 不变，"" = 清空。
// MCP 的 JSON 输入天然有这个能力（字段缺席 vs 显式 null/""），
// 所以工具的输入结构直接用指针，一路传到领域层不做转换。
func pointer[T any](v T) *T { return &v }
