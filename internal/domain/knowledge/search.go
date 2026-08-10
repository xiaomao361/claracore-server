package knowledge

import (
	"context"
	"fmt"
	"math"
	"slices"
	"time"

	"claracore-server/internal/platform"
)

// 检索的几个数值全部写成常量，不做成配置项。
//
// 「调了也不知道好没好」的参数可配，等于把调参责任推给一个没有评估集的用户
// （与 01-architecture.md §11.2 对空间权重的判断同一条理由）。等真的跑出
// 命中率数据、知道该往哪边调了，再说。
const (
	// candidateCap 是第一段粗筛的候选集上限（docs/02-data-model.md §5.1 建议 500）。
	candidateCap = 500

	defaultSearchLimit = 10
	maxSearchLimit     = 50

	// scoreTieEpsilon 是「分数接近」的界限。差距在这以内才轮到空间层级决胜——
	// 空间只做 tie-break，不做排序主键，否则个人空间里的精准命中会排在
	// 团队空间的勉强沾边之后（01-architecture.md §11.2）。
	//
	// **这个值相对于真实分数的尺度并不小**：2026-08-10 在 bge-m3 上实测，
	// 一次对抗性召回里正确卡与干扰卡的余弦是 0.6195 与 0.6051——差 0.0144，
	// 只比这条界限大一点。embedding 模型的余弦普遍挤在 0.5~0.8 这个窄带里，
	// 不是均匀铺满 0~1，所以 0.01 大约相当于「一个身位」而不是「几乎相等」。
	//
	// 暂不调整：往下调会让空间层级几乎永远不生效（那条 tie-break 就白留了），
	// 往上调会让空间层级开始压过真实的相关性差异。两边都是拍脑袋，
	// 而这个项目对「调了也不知道好没好」的参数一贯的态度是先跑出数据。
	// 真要动它，先有一组带标注的召回评估集。
	scoreTieEpsilon = 0.01

	// cardHintThreshold 决定什么时候去数「另有 N 条卡片命中」。
	//
	// memory 命中够多时就不数了：那次 count 虽然走 ngram 索引，也不是白给的。
	// 它要解决的是真实风险——agent 搜一次 memory 没找到就以为没有，
	// 而 Card 才是这个产品的核心载体（03-mcp-contract.md §6）。
	cardHintThreshold = 5
)

// SearchSpace 是一个可读空间及其排序权重。
//
// 只收 ID 和 Rank，**不收空间类型**：knowledge 不知道 team / project / personal
// 是什么，也不该知道。「团队空间比个人空间权威」是治理域的判断，
// 跨过域边界的只是一个整数——Rank 大的在分数接近时胜出，仅此而已。
//
// 这不是洁癖。一旦这里出现 `if space.Type == "team"`，权限与空间语义
// 就在 knowledge 里有了第二个副本，而两份规则永远只会改对其中一份。
type SearchSpace struct {
	ID   platform.ID
	Rank int
}

// CandidateQuery 是交给 Store 的第一段粗筛条件。
//
// 实现必须把 SpaceIDs 下推成 SQL 的 `WHERE space_id IN (...)`——
// **权限过滤绝不后置**（docs/02-data-model.md §5.1）。
type CandidateQuery struct {
	TenantID platform.ID
	SpaceIDs []platform.ID

	// Query 为空时按时间新近度取候选，非空时按 ngram 全文相关性排序后截断。
	Query string

	// Labels 非空时要求候选**同时具有**全部标签（交集，不是并集）。
	// 并集可以靠调用两次得到，交集不行——「缩小范围」是这个参数唯一的用途。
	Labels []string

	// Limit 是候选集上限，由领域层填 candidateCap。
	Limit int
}

// MemoryCandidate 是粗筛出的一条记忆及其全文相关性。
//
// 粗筛不填 Labels：那要么是 N+1 次查询，要么是一次会让候选集膨胀的 JOIN，
// 而标签在检索结果里只是展示信息。要标签就按 ID 取单条。
type MemoryCandidate struct {
	Memory    Memory
	TextScore float64
}

type CardCandidate struct {
	Card      Card
	TextScore float64
}

type TriggerCandidate struct {
	Trigger   RecallTrigger
	TextScore float64
}

// Hit 的 Semantic 字段是这套检索最重要的一个信号。
//
// 语义命中（余弦相似度）与全文命中（ngram 相关性）**不通约成同一个分数**：
// 它们不是一个量纲，硬加权出来的排序是假的。所以结果分成两组——
// 语义命中在前，全文命中在后，各自组内排序——并且每条都标着自己是哪一种。
// 调用方看得出区别，才能判断「这条是真的相关，还是只是字面撞上了」。
type MemoryHit struct {
	Memory   Memory
	Score    float64
	Semantic bool
}

type CardHit struct {
	Card     Card
	Score    float64
	Semantic bool

	// Trigger 是命中的那条召回条件，只有 RecallCards 会填。
	// 它回答的是「为什么现在给我看这张卡」——没有它，一次召回
	// 对 agent 来说就是凭空出现的建议。
	Trigger string
}

// SearchResult 的 SemanticAvailable 表示这次检索是否真的用上了向量。
//
// false 有两种原因：没配 Embedder（降级运行），或者算查询向量时外部服务失败。
// 两种都不该让检索失败，但都必须让调用方知道——否则「没搜到」
// 和「语义那一半根本没跑」长得一模一样，而它们的下一步完全不同。
// 与「检索为空 ≠ 无权检索」是同一条哲学。
type MemorySearchResult struct {
	Hits              []MemoryHit
	SemanticAvailable bool

	// CardMatches 是「另有 N 条卡片命中，用 card_search 查看」的计数
	// （03-mcp-contract.md §6）。只在 memory 命中偏少时才去数，-1 表示没数。
	CardMatches int
}

type CardSearchResult struct {
	Hits              []CardHit
	SemanticAvailable bool
}

// ---------------------------------------------------------------------------
// memoria_search
// ---------------------------------------------------------------------------

type SearchMemoriesInput struct {
	TenantID platform.ID

	// Spaces 是调用者的**可读**空间集合，已由应用层经
	// governance.SpaceScope.ResolveReadTargets 解析。
	Spaces []SearchSpace

	Query  string
	Labels []string
	Limit  int
}

func (s *Service) SearchMemories(ctx context.Context, in SearchMemoriesInput) (MemorySearchResult, error) {
	spaces, limit, err := prepareSearch(in.Spaces, in.Limit)
	if err != nil {
		return MemorySearchResult{}, err
	}
	q := CandidateQuery{
		TenantID: in.TenantID,
		SpaceIDs: spaceIDs(spaces),
		Query:    in.Query,
		Labels:   in.Labels,
		Limit:    candidateCap,
	}

	candidates, err := s.store.SearchMemories(ctx, q)
	if err != nil {
		return MemorySearchResult{}, err
	}

	scored := make([]scored[Memory], 0, len(candidates))
	for _, c := range candidates {
		if err := checkTenant(in.TenantID, c.Memory.TenantID); err != nil {
			return MemorySearchResult{}, err
		}
		scored = append(scored, newScored(c.Memory, c.Memory.SpaceID, c.Memory.UpdatedAt, c.Memory.ID, c.TextScore))
	}

	refs := make([]Ref, 0, len(scored))
	for _, c := range candidates {
		refs = append(refs, Ref{Type: EntityMemory, ID: c.Memory.ID})
	}
	semantic, err := rerank(ctx, s, in.TenantID, in.Query, refs, scored)
	if err != nil {
		return MemorySearchResult{}, err
	}

	ranked := rank(scored, spaces, limit)
	hits := make([]MemoryHit, 0, len(ranked))
	for _, r := range ranked {
		hits = append(hits, MemoryHit{Memory: r.item, Score: r.score, Semantic: r.semantic})
	}

	result := MemorySearchResult{Hits: hits, SemanticAvailable: semantic, CardMatches: -1}
	if len(hits) < cardHintThreshold && in.Query != "" {
		n, err := s.store.CountCardMatches(ctx, q)
		if err != nil {
			return MemorySearchResult{}, err
		}
		result.CardMatches = n
	}
	return result, nil
}

// ---------------------------------------------------------------------------
// card_search
// ---------------------------------------------------------------------------

type SearchCardsInput struct {
	TenantID platform.ID
	Spaces   []SearchSpace
	Query    string
	Labels   []string
	Limit    int
}

func (s *Service) SearchCards(ctx context.Context, in SearchCardsInput) (CardSearchResult, error) {
	spaces, limit, err := prepareSearch(in.Spaces, in.Limit)
	if err != nil {
		return CardSearchResult{}, err
	}

	candidates, err := s.store.SearchCards(ctx, CandidateQuery{
		TenantID: in.TenantID,
		SpaceIDs: spaceIDs(spaces),
		Query:    in.Query,
		Labels:   in.Labels,
		Limit:    candidateCap,
	})
	if err != nil {
		return CardSearchResult{}, err
	}

	scored := make([]scored[Card], 0, len(candidates))
	refs := make([]Ref, 0, len(candidates))
	for _, c := range candidates {
		if err := checkTenant(in.TenantID, c.Card.TenantID); err != nil {
			return CardSearchResult{}, err
		}
		scored = append(scored, newScored(c.Card, c.Card.SpaceID, c.Card.UpdatedAt, c.Card.ID, c.TextScore))
		refs = append(refs, Ref{Type: EntityCard, ID: c.Card.ID})
	}

	semantic, err := rerank(ctx, s, in.TenantID, in.Query, refs, scored)
	if err != nil {
		return CardSearchResult{}, err
	}

	ranked := rank(scored, spaces, limit)
	hits := make([]CardHit, 0, len(ranked))
	for _, r := range ranked {
		hits = append(hits, CardHit{Card: r.item, Score: r.score, Semantic: r.semantic})
	}
	return CardSearchResult{Hits: hits, SemanticAvailable: semantic}, nil
}

// ---------------------------------------------------------------------------
// card_recall —— 本产品的核心能力
// ---------------------------------------------------------------------------

type RecallCardsInput struct {
	TenantID platform.ID
	Spaces   []SearchSpace

	// Situation 是 agent 当前的工作情境，例如
	// 「用户让我清理 ~/.qclaw 目录释放磁盘空间，我准备执行 rm -rf」。
	//
	// 它匹配的是**作者显式写下的召回条件**，不是卡片全文——
	// 这正是 card_recall 与 card_search 不合并的原因（03-mcp-contract.md §4.2）。
	Situation string

	Limit int
}

// RecallCards 按当前情境召回卡片。
//
// 与另外两条检索的形状不同：粗筛打在 card_recall_triggers 上，
// 精算比的是「情境 ↔ 每条触发条件」的相似度，命中任一条即召回**整张卡**。
// 一张卡的多条触发条件都命中时，取分数最高的那条作为召回理由。
func (s *Service) RecallCards(ctx context.Context, in RecallCardsInput) (CardSearchResult, error) {
	spaces, limit, err := prepareSearch(in.Spaces, in.Limit)
	if err != nil {
		return CardSearchResult{}, err
	}
	ids := spaceIDs(spaces)

	candidates, err := s.store.SearchRecallTriggers(ctx, CandidateQuery{
		TenantID: in.TenantID,
		SpaceIDs: ids,
		Query:    in.Situation,
		Limit:    candidateCap,
	})
	if err != nil {
		return CardSearchResult{}, err
	}

	scored := make([]scored[RecallTrigger], 0, len(candidates))
	refs := make([]Ref, 0, len(candidates))
	for _, c := range candidates {
		if err := checkTenant(in.TenantID, c.Trigger.TenantID); err != nil {
			return CardSearchResult{}, err
		}
		scored = append(scored, newScored(c.Trigger, c.Trigger.SpaceID, c.Trigger.CreatedAt, c.Trigger.ID, c.TextScore))
		refs = append(refs, Ref{Type: EntityCardTrigger, ID: c.Trigger.ID})
	}

	semantic, err := rerank(ctx, s, in.TenantID, in.Situation, refs, scored)
	if err != nil {
		return CardSearchResult{}, err
	}

	// 先按触发条件排序，再折叠到卡片：一张卡的多条条件命中时，
	// 排在最前的那条就是它的分数与召回理由。折叠之后才截断到 limit，
	// 否则「同一张卡占掉三个名额」会把别的卡挤出去。
	best := rank(scored, spaces, len(scored))
	type pick struct {
		trigger  RecallTrigger
		score    float64
		semantic bool
	}
	order := make([]platform.ID, 0, len(best))
	picked := make(map[platform.ID]pick, len(best))
	for _, r := range best {
		if _, seen := picked[r.item.CardID]; seen {
			continue
		}
		picked[r.item.CardID] = pick{trigger: r.item, score: r.score, semantic: r.semantic}
		order = append(order, r.item.CardID)
		if len(order) == limit {
			break
		}
	}
	if len(order) == 0 {
		return CardSearchResult{Hits: []CardHit{}, SemanticAvailable: semantic}, nil
	}

	// 回表取整卡。仍然带上可读空间集合——触发条件表里的 space_id 是冗余列，
	// 拿它当权限依据等于信任一份可能与 cards 表不同步的副本。
	cards, err := s.store.FindCards(ctx, in.TenantID, order, ids)
	if err != nil {
		return CardSearchResult{}, err
	}
	byID := make(map[platform.ID]Card, len(cards))
	for _, c := range cards {
		if err := checkTenant(in.TenantID, c.TenantID); err != nil {
			return CardSearchResult{}, err
		}
		byID[c.ID] = c
	}

	hits := make([]CardHit, 0, len(order))
	for _, id := range order {
		c, ok := byID[id]
		if !ok {
			// 卡片没取到：要么它已经不可见，要么触发条件行是悬空引用
			// （多态引用没有外键，见 02 §3.1 的明确权衡）。跳过而不是报错——
			// 一条脏数据不该让整次召回失败。
			continue
		}
		p := picked[id]
		hits = append(hits, CardHit{Card: c, Score: p.score, Semantic: p.semantic, Trigger: p.trigger.Text})
	}
	return CardSearchResult{Hits: hits, SemanticAvailable: semantic}, nil
}

// ---------------------------------------------------------------------------
// 第二段：精算
// ---------------------------------------------------------------------------

// scored 是排序过程中的一条候选。泛型是为了让三条检索共用同一套排序规则——
// 排序规则写三遍就会有两遍是错的。
type scored[T any] struct {
	item     T
	spaceID  platform.ID
	recency  time.Time
	tieID    platform.ID // 一切都相同时按 ID 定序，保证同样的查询返回同样的顺序
	score    float64
	semantic bool
}

func newScored[T any](item T, spaceID platform.ID, recency time.Time, tieID platform.ID, textScore float64) scored[T] {
	return scored[T]{item: item, spaceID: spaceID, recency: recency, tieID: tieID, score: textScore}
}

// rerank 是两段式检索的第二段：给候选集算余弦相似度。
//
// 返回值表示语义精算是否真的发生。**外部服务失败时降级而不是报错**：
// embedding 服务挂掉应该让检索变差，不该让检索停摆——第一段的 ngram
// 全文匹配本来就是一条完整可用的路径。
//
// # 为什么这里没有 goroutine 池
//
// docs/02-data-model.md §5.1 写的是「goroutine 池并行计算余弦」。落地时
// 认为那条不必照做，理由是它误诊了 Desktop 的病：那边真正的故障是
// **JS 单线程事件循环被一次大搜索独占，所有其他 agent 一起阻塞**，
// 而在 Go 里每个请求本来就跑在自己的 goroutine 上，这个病由 runtime
// 免费治好了。一次检索内部的计算量是 500 候选 × 千余维 ≈ 五十万次乘加，
// 不到 1ms；再开一个池只是给它加上调度和内存屏障的开销。
//
// 这一段真正的成本在下面那行 LoadVectors——从 MySQL 拉几百条 BLOB 并解码，
// 而那是 IO，池子帮不上忙。将来要优化，该动的是候选集大小和向量缓存，
// 不是并行度。
//
// 写成包级函数而不是 Service 的方法：Go 不允许方法带类型参数，
// 而三条检索共用一套精算逻辑的价值高于「它看起来像个方法」。
func rerank[T any](ctx context.Context, s *Service, tenantID platform.ID, query string, refs []Ref, cands []scored[T]) (bool, error) {
	if s.embedder == nil || query == "" || len(cands) == 0 {
		return false, nil
	}
	vectors, err := s.embedder.Embed(ctx, []string{query})
	if err != nil || len(vectors) != 1 || len(vectors[0]) == 0 {
		return false, nil
	}
	qv := vectors[0]

	stored, err := s.store.LoadVectors(ctx, tenantID, s.embedder.Model(), refs)
	if err != nil {
		return false, err
	}
	if len(stored) == 0 {
		// 一条向量都没有：这批内容还没被索引（异步入队，worker 还没轮到），
		// 或者刚换过模型。全文那一段的结果照常返回，只是不带语义标记。
		return false, nil
	}

	for i := range cands {
		v, ok := stored[refs[i].String()]
		if !ok || len(v) != len(qv) {
			// 维度不同 = 这条是用另一个模型算的，跨模型比余弦没有意义。
			// 当作"没有向量"处理，它会落到全文那一组里。
			continue
		}
		cands[i].score = cosine(qv, v)
		cands[i].semantic = true
	}
	return true, nil
}

func cosine(a, b []float32) float64 {
	var dot, na, nb float64
	for i := range a {
		x, y := float64(a[i]), float64(b[i])
		dot += x * y
		na += x * x
		nb += y * y
	}
	if na == 0 || nb == 0 {
		return 0
	}
	return dot / (math.Sqrt(na) * math.Sqrt(nb))
}

// rank 排序并截断。
//
// 语义命中整体排在全文命中之前——不是因为语义一定更准，而是因为
// 两种分数不是一个量纲，混在一起排出来的顺序是假的（见 MemoryHit.Semantic）。
func rank[T any](cands []scored[T], spaces []SearchSpace, limit int) []scored[T] {
	rankOf := make(map[platform.ID]int, len(spaces))
	for _, sp := range spaces {
		rankOf[sp.ID] = sp.Rank
	}

	slices.SortFunc(cands, func(a, b scored[T]) int {
		if a.semantic != b.semantic {
			if a.semantic {
				return -1
			}
			return 1
		}
		// 先把分数量化到 epsilon 网格再比，而不是「差值小于 epsilon 就算相等」——
		// 后者不满足传递性（a≈b、b≈c 但 a<c），排序结果会依赖初始顺序。
		if c := compareDesc(bucket(a.score), bucket(b.score)); c != 0 {
			return c
		}
		if c := compareDesc(float64(rankOf[a.spaceID]), float64(rankOf[b.spaceID])); c != 0 {
			return c
		}
		if !a.recency.Equal(b.recency) {
			if a.recency.After(b.recency) {
				return -1
			}
			return 1
		}
		return compareID(a.tieID, b.tieID)
	})

	if len(cands) > limit {
		cands = cands[:limit]
	}
	return cands
}

func bucket(score float64) float64 { return math.Round(score / scoreTieEpsilon) }

func compareDesc(a, b float64) int {
	switch {
	case a > b:
		return -1
	case a < b:
		return 1
	}
	return 0
}

func compareID(a, b platform.ID) int {
	switch {
	case a < b:
		return -1
	case a > b:
		return 1
	}
	return 0
}

// ---------------------------------------------------------------------------
// 入参处理
// ---------------------------------------------------------------------------

// prepareSearch 校验空间集合并归一化 limit。
//
// 空间集合为空报 ErrInvalidArgument 而不是返回空结果：「你没有任何可访问的空间」
// 和「搜到了但是没有」的排查方向完全不同（与 requireSpaces 同一判断）。
func prepareSearch(spaces []SearchSpace, limit int) ([]SearchSpace, int, error) {
	if len(spaces) == 0 {
		return nil, 0, fmt.Errorf("%w: 未提供任何可访问的空间", ErrInvalidArgument)
	}
	for _, sp := range spaces {
		if sp.ID == "" {
			return nil, 0, fmt.Errorf("%w: 空间 ID 为空", ErrInvalidArgument)
		}
	}
	switch {
	case limit <= 0:
		limit = defaultSearchLimit
	case limit > maxSearchLimit:
		limit = maxSearchLimit
	}
	return spaces, limit, nil
}

func spaceIDs(spaces []SearchSpace) []platform.ID {
	out := make([]platform.ID, 0, len(spaces))
	for _, sp := range spaces {
		out = append(out, sp.ID)
	}
	return out
}
