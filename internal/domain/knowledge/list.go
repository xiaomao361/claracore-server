package knowledge

import (
	"context"
	"encoding/base64"
	"fmt"
	"strconv"
	"strings"
	"time"
	"unicode/utf8"

	"claracore-server/internal/platform"
)

// 列表查询的两个上限。与检索的常量分开：它们回答的不是同一个问题。
const (
	defaultListLimit = 30
	maxListLimit     = 100
	MaxListQueryLen  = 200
)

// ListContentsInput 是「按条件翻一页内容」，不是检索。
//
// # 为什么检索不能当列表用
//
// 三条都不是小毛病：
//
//	① 检索只返回默认可见状态。而浏览页必须能看到 draft（待审）与
//	   deprecated（已过时）——那正是治理面存在的理由。
//	② 检索有 candidateCap 截断和相关性排序。列表要的是确定的时间序
//	   和能一页页翻到底的分页，而"前 500 条候选里相关性最高的 10 条"
//	   翻不到第 51 页。
//	③ 检索会走向量精算。浏览页每翻一页算一次余弦是纯浪费。
//
// 硬把检索接口加个 includeStatuses 参数来复用，会同时毁掉两边：
// 检索多一个能把未审草稿混进 agent 上下文的开关，列表还是翻不到底。
type ListContentsInput struct {
	TenantID platform.ID

	// Spaces 是调用者的**可读**空间集合（审核页传可写集合）。
	Spaces []platform.ID

	// Kind 必填：memory 或 card。
	//
	// **不做混排**。与 memoria_search 不混排卡片是同一条理由：
	// 两者是并列的一等公民，把它们排进一个列表要先回答
	// "一条记忆和一张卡怎么比"，而那个问题没有答案。
	Kind string

	// Statuses 为空表示不过滤（浏览页的默认）。
	// 审核队列传 [draft]。
	Statuses []string

	// SpaceID 非空时只看这一个空间（页面上的空间筛选）。
	// 它仍然必须在 Spaces 里——权限不因为多了一个筛选参数而放宽。
	SpaceID platform.ID

	// Query is an optional literal/full-text filter within the governance list.
	// Unlike agent search, it keeps all explicitly requested lifecycle states
	// and deterministic keyset pagination; it does not run semantic reranking.
	Query string

	// Cursor 来自上一页的 NextCursor，空表示第一页。
	Cursor string
	Limit  int
}

// ContentItem 是列表里的一行。
//
// **不带正文**：列表页不需要它，而 MEDIUMTEXT 乘以一页 30 行
// 是实打实的传输量。要正文就点进详情，那里走 GetMemory / GetCard。
type ContentItem struct {
	Ref     Ref
	SpaceID platform.ID
	Title   string
	Status  string

	// CardType 与 Qualified 只有卡片有。Qualified 由服务端算好——
	// 「理解栏是否为空」那条质量分界线只该有一处定义。
	CardType  string
	Qualified bool

	AuthorUserID  platform.ID
	AuthorAgentID *platform.ID

	// CopiedFrom 与 SourcePath 让列表能标注来源（04 §3.1 页面 2 的要求）：
	// 原创 / 提升自别处 / 摄取进来的。三者的可信度完全不同。
	CopiedFrom *Origin
	SourcePath string

	CreatedAt time.Time
	UpdatedAt time.Time
}

// IsIngested 表示这是摄取产物而非人或 agent 写的。
func (c ContentItem) IsIngested() bool { return c.SourcePath != "" }

type ListContentsResult struct {
	Items []ContentItem
	// NextCursor 为空表示没有下一页。
	NextCursor string
	// Total 是符合条件的总数，用于「共 128 条待审」这类提示。
	//
	// 与 Items 分开一次查询：COUNT 走的是索引，而列表要回表取列，
	// 两件事的代价差一个量级。翻到第 5 页时不必再数一遍——
	// 调用方只在第一页要它（见 CountOnly）。
	Total int
}

// ListQuery 是交给 Store 的列表条件。
//
// 与 CandidateQuery 分开而不是复用：那个是检索粗筛（带 MATCH 打分、
// 有 cap 截断），这个是确定序的分页。合成一个的话，两边的必填字段
// 会互相污染，最后变成一个谁都要判空的大结构体。
type ListQuery struct {
	TenantID platform.ID
	SpaceIDs []platform.ID
	Kind     string
	Statuses []string
	Query    string

	// After 是 keyset 分页的锚点，零值表示第一页。
	//
	// **不用 OFFSET**：翻页期间有内容被更新时，OFFSET 会让某些行
	// 被跳过、某些行出现两次——而审核队列恰恰是一边翻一边改的场景，
	// 那是 OFFSET 最容易出错的地方。
	AfterUpdatedAt time.Time
	AfterID        platform.ID

	Limit int
	// CountOnly 为真时只数总数，不取行。
	CountOnly bool
}

// ListContents 翻一页内容。
func (s *Service) ListContents(ctx context.Context, in ListContentsInput) (ListContentsResult, error) {
	if err := requireSpaces(in.Spaces); err != nil {
		return ListContentsResult{}, err
	}
	if in.Kind != EntityMemory && in.Kind != EntityCard {
		return ListContentsResult{}, fmt.Errorf("%w: 内容类型必须是 %s 或 %s", ErrInvalidArgument, EntityMemory, EntityCard)
	}
	for _, st := range in.Statuses {
		if !validStatusFor(in.Kind, st) {
			return ListContentsResult{}, fmt.Errorf("%w: %s 不接受状态 %q", ErrInvalidArgument, in.Kind, st)
		}
	}
	query := strings.TrimSpace(in.Query)
	if utf8.RuneCountInString(query) > MaxListQueryLen {
		return ListContentsResult{}, fmt.Errorf("%w: 查询超过 %d 字", ErrInvalidArgument, MaxListQueryLen)
	}

	spaces := in.Spaces
	if in.SpaceID != "" {
		// 筛选到单个空间，但**必须先确认它在可读集合里**。
		// 直接拿参数去查等于让筛选参数变成一个绕过权限的入口。
		if !containsID(in.Spaces, in.SpaceID) {
			return ListContentsResult{}, ErrNotFound
		}
		spaces = []platform.ID{in.SpaceID}
	}

	limit := in.Limit
	switch {
	case limit <= 0:
		limit = defaultListLimit
	case limit > maxListLimit:
		limit = maxListLimit
	}

	q := ListQuery{
		TenantID: in.TenantID,
		SpaceIDs: spaces,
		Kind:     in.Kind,
		Statuses: in.Statuses,
		Query:    query,
		// 多取一条用来判断「还有没有下一页」，返回时再切掉。
		// 比另发一次 COUNT 便宜，也比"取满 limit 就假设还有下一页"诚实——
		// 后者会在正好取满时给出一个点进去是空的下一页。
		Limit: limit + 1,
	}
	if in.Cursor != "" {
		at, id, err := decodeCursor(in.Cursor)
		if err != nil {
			return ListContentsResult{}, err
		}
		q.AfterUpdatedAt, q.AfterID = at, id
	}

	items, err := s.store.ListContents(ctx, q)
	if err != nil {
		return ListContentsResult{}, err
	}
	result := ListContentsResult{Total: -1}
	if len(items) > limit {
		last := items[limit-1]
		result.NextCursor = encodeCursor(last.UpdatedAt, last.Ref.ID)
		items = items[:limit]
	}
	result.Items = items

	// 只在第一页数总数：翻到第 5 页时那个数字没变，再数一遍是白花的钱。
	if in.Cursor == "" {
		countQuery := q
		countQuery.CountOnly = true
		total, err := s.store.CountContents(ctx, countQuery)
		if err != nil {
			return ListContentsResult{}, err
		}
		result.Total = total
	}
	return result, nil
}

// validStatusFor 拦住「给 memory 传 draft」这类参数。
//
// 两套枚举刻意不同（02 §3.2）：Memory 记生命周期，Card 记知识成熟度。
// 不校验的话，一个传错的状态会安静地返回空列表，而调用方会以为
// 「这个空间里确实没有」。
func validStatusFor(kind, status string) bool {
	switch kind {
	case EntityMemory:
		return validMemoryStatus(status)
	case EntityCard:
		return validCardStatus(status)
	}
	return false
}

func containsID(list []platform.ID, want platform.ID) bool {
	for _, id := range list {
		if id == want {
			return true
		}
	}
	return false
}

// encodeCursor 把 (updated_at, id) 编成一个不透明字符串。
//
// 不透明是有意的：调用方不该解析它、更不该构造它。做成
// `?after_id=xxx` 那种可读形式，迟早有人手工拼一个，
// 而那时排序键与游标不一致的后果是静默漏行。
func encodeCursor(at time.Time, id platform.ID) string {
	raw := strconv.FormatInt(at.UTC().UnixMilli(), 10) + ":" + string(id)
	return base64.RawURLEncoding.EncodeToString([]byte(raw))
}

func decodeCursor(cursor string) (time.Time, platform.ID, error) {
	raw, err := base64.RawURLEncoding.DecodeString(cursor)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%w: 游标格式非法", ErrInvalidArgument)
	}
	millis, id, ok := strings.Cut(string(raw), ":")
	if !ok {
		return time.Time{}, "", fmt.Errorf("%w: 游标格式非法", ErrInvalidArgument)
	}
	n, err := strconv.ParseInt(millis, 10, 64)
	if err != nil {
		return time.Time{}, "", fmt.Errorf("%w: 游标格式非法", ErrInvalidArgument)
	}
	return time.UnixMilli(n).UTC(), platform.ID(id), nil
}
