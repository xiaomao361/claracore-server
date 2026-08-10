package knowledge

import (
	"context"
	"time"

	"claracore-server/internal/platform"
)

// Store 是核心域的持久化出口。
//
// 接口定义在本包而非 ports/：方法签名涉及领域类型，放 ports 会造成
// domain → ports → domain 的循环 import（docs/05-code-structure.md §3.1）。
//
// # 关于 spaces 参数
//
// 每个读取方法都收一个 spaces 集合，实现必须把它下推成 SQL 的
// `WHERE space_id IN (...)`——**权限过滤绝不后置**（docs/02-data-model.md §5.1）：
// 先查全量再在应用层筛，既是性能问题，更是「筛之前那些行已经进过内存」的泄露风险。
//
// 实现不需要知道这批空间是怎么来的，也**不得**对它们做任何加工：
// 权限规则只有一份，在 governance.ResolveScope。这里出现任何形如
// `OR type = 'team'` 的判断，规则就有了第二个副本。
//
// # 关于错误
//
// 找不到时返回 ErrNotFound 而不是 (nil, nil)。
// 乐观锁条件不满足时返回 ErrVersionConflict——注意「影响行数为 0」在
// MySQL 驱动默认配置下也可能是「值没变」，实现必须区分（0001 那一轮的教训）。
type Store interface {
	// --- Memory ---

	CreateMemory(ctx context.Context, m Memory) error

	// FindMemory 在 spaces 范围内按 ID 取一条，并填充 Labels 字段。
	//
	// 填 Labels 而非留给调用方再查一次：memoria_get 必然要带出标签，
	// 拆成两次调用只会让每个调用点都写一遍同样的两步。
	FindMemory(ctx context.Context, tenantID, id platform.ID, spaces []platform.ID) (*Memory, error)

	// UpdateMemory 以 expectedVersion 为乐观锁条件写回。
	// m.Version 已是新版本号，实现直接写入即可，不要自行 +1。
	UpdateMemory(ctx context.Context, m Memory, expectedVersion int) error

	// --- Card ---

	CreateCard(ctx context.Context, c Card) error

	// FindCard 在 spaces 范围内按 ID 取一张，并填充 Labels 与 RecallTriggers。
	FindCard(ctx context.Context, tenantID, id platform.ID, spaces []platform.ID) (*Card, error)

	// FindCardBySourceKey 按摄取源的稳定标识取卡，是幂等摄取的读侧。
	//
	// 只在**一个确定空间**里找，不收 spaces 集合：摄取是往一个明确的
	// 目标空间里灌，而 uk_cards_source 的唯一性也只在空间内成立。
	// 找不到返回 ErrNotFound（调用方据此走「新建」分支）。
	//
	// 注意它**不做状态过滤**：一张被人改成 deprecated 的摄取卡，
	// 下次重扫仍然要认出它是同一份源——否则会新建一张，
	// 而那个「已经判过这份文档没用」的决定就被悄悄推翻了。
	FindCardBySourceKey(ctx context.Context, tenantID, spaceID platform.ID, sourceKey string) (*Card, error)

	UpdateCard(ctx context.Context, c Card, expectedVersion int) error

	// --- 多态附属物 ---

	// ReplaceLabels 用给定集合整体替换某实体的标签。
	//
	// 是「替换」而不是「增删」：调用方传来的就是它认为的最终状态，
	// 增量语义会逼每个调用点先查现状再算差集，那段代码写五遍就会错一遍。
	ReplaceLabels(ctx context.Context, tenantID platform.ID, ref Ref, labels []string, at time.Time) error

	// ReplaceRecallTriggers 整体替换一张卡的召回条件，同样是替换语义。
	//
	// 触发条件是有序列表，增量更新还要额外维护 sort_order 的连续性——
	// 整体替换让「作者重排了顺序」和「作者删了中间一条」变成同一件事。
	ReplaceRecallTriggers(ctx context.Context, tenantID, cardID platform.ID, triggers []RecallTrigger) error

	// --- 检索：第一段粗筛 ---
	//
	// 三个方法的实现纪律相同，也是这一层最要紧的一条：
	// **把 q.SpaceIDs 下推成 SQL 的 `WHERE space_id IN (...)`，绝不后置**
	// （docs/02-data-model.md §5.1）。查全量再在应用层筛，既是性能问题，
	// 更是「筛之前那些行已经进过内存」的泄露风险。
	//
	// 只返回默认可见状态的内容（memory 的 active、card 的 active）。
	// 审核用的 draft 列表是 Web 治理面的列表查询，不是检索——
	// 让检索接口长出一个 includeStatuses 参数，等于让每个调用点
	// 都有机会把摄取产生的未审草稿混进 agent 的上下文里。

	// SearchMemories 粗筛记忆。q.Query 非空时按 ngram 全文相关性排序，
	// 为空时按时间新近度；两种都截断到 q.Limit。
	//
	// 返回的 TextScore 只在同一次查询内部可比——它是 MySQL 的 MATCH 相关性，
	// 没有跨查询的绝对含义。领域层只拿它排序，不拿它当阈值。
	SearchMemories(ctx context.Context, q CandidateQuery) ([]MemoryCandidate, error)

	SearchCards(ctx context.Context, q CandidateQuery) ([]CardCandidate, error)

	// SearchRecallTriggers 粗筛召回条件（card_recall 的第一段）。
	//
	// 必须只返回**所属卡片当前可见**的触发条件。card_recall_triggers 表上
	// 没有 status 列，所以这里躲不掉一次 JOIN cards——0002 迁移里
	// 「冗余 space_id 使粗筛无需 JOIN」那句话只对权限过滤成立，
	// 当时没想到状态过滤（实现检索时发现，2026-08-10）。
	SearchRecallTriggers(ctx context.Context, q CandidateQuery) ([]TriggerCandidate, error)

	// CountCardMatches 数一数同样的条件下有多少张卡命中。
	//
	// 只用于 memoria_search 返回的「另有 N 条卡片命中」提示，不返回内容。
	// 与 SearchCards 用同一份 WHERE 条件，否则那个数字会和用户
	// 真去 card_search 时看到的对不上，比不给这个数字更糟。
	CountCardMatches(ctx context.Context, q CandidateQuery) (int, error)

	// FindCards 在 spaces 范围内批量取卡片，用于 card_recall 的回表。
	//
	// 找不到的 ID 直接不出现在结果里，不报 ErrNotFound：召回本来就是
	// 「有什么给什么」，一条悬空的触发条件不该让整次召回失败。
	// 同样只返回默认可见状态的卡片。
	FindCards(ctx context.Context, tenantID platform.ID, ids, spaces []platform.ID) ([]Card, error)

	// CountSpaceContents 按空间统计内容量，用于 space_stats。
	//
	// 「有多少张卡只是摘要」这个数字必须由 SQL 直接给出：把卡片全捞出来
	// 在应用层调 Card.IsQualified() 数一遍，在几千张卡的规模上就已经不能接受。
	// 代价是 Card.IsQualified() 的判定在 SQL 里有了第二份表达
	// （`body_understanding IS NULL`），两者必须始终等价——0002 迁移
	// 「空 section 写 NULL 而非空串」正是为这个查询留的口径，
	// 而 nullIfEmpty 保证了写入侧不会产生空串。有测试盯着这对等价关系。
	CountSpaceContents(ctx context.Context, tenantID platform.ID, spaces []platform.ID) ([]SpaceStats, error)

	// --- 检索：第二段精算所需的向量 ---

	// LoadVectors 批量取指定实体在指定模型下**已算好**的向量，
	// 返回值以 Ref.String() 为键。没有向量的实体不出现在结果里。
	//
	// 不带空间过滤：refs 全部来自第一段粗筛，已经过过权限了。
	// 在这里再过滤一次意味着权限规则有了第二个可以走的入口。
	LoadVectors(ctx context.Context, tenantID platform.ID, model string, refs []Ref) (map[string][]float32, error)

	// --- 索引队列 ---

	// EnqueueEmbedding 把一条内容排进索引队列，**内容未变时不得重置已有向量**。
	//
	// 实现要按 content_hash 判断：hash 相同说明这次写入没碰到被 embedding 的
	// 那几栏（比如只改了标签），已算好的向量继续有效。粗暴地每次都置回 pending，
	// 等于每改一次标签就重算一次向量——那是真金白银的外部调用。
	EnqueueEmbedding(ctx context.Context, e Embedding) error

	// DeleteCardTriggerEmbeddings 删掉一张卡全部召回条件的向量行。
	//
	// 必须在 ReplaceRecallTriggers **之前**调用：那个方法先删后插，
	// 旧触发条件的行一旦没了，就再也没有办法定位它们留下的向量，
	// 那些行会永远躺在库里，既不会被查到也不会被清掉。
	DeleteCardTriggerEmbeddings(ctx context.Context, tenantID, cardID platform.ID) error

	// ListPendingEmbeddings 领一批待算的向量（pending 与重试次数未超上限的 failed）。
	//
	// 按最久没动的排在前面：失败的行每失败一次就更新时间戳，于是自然排到队尾，
	// 不会霸着队头把整批活卡死。
	ListPendingEmbeddings(ctx context.Context, model string, maxAttempts, limit int) ([]Embedding, error)

	// SaveEmbeddingVector 写回算好的向量，置 ready 并清空失败计数。
	SaveEmbeddingVector(ctx context.Context, id platform.ID, vector []float32, at time.Time) error

	// MarkEmbeddingsFailed 批量标记失败：累加 attempts、记下原因。
	// 行不删——「这条内容算不出向量」本身是要给运维看的信息。
	MarkEmbeddingsFailed(ctx context.Context, ids []platform.ID, reason string, at time.Time) error

	// --- Link ---

	// CreateLink 在同 (from, to, kind) 已存在时返回 ErrAlreadyExists。
	CreateLink(ctx context.Context, l Link) error

	// DeleteLink 幂等：关联本就不存在时返回 nil 而非 ErrNotFound。
	// 调用方的意图是「让这条关联不存在」，那个状态已经达成了。
	DeleteLink(ctx context.Context, tenantID platform.ID, from, to Ref, kind string) error

	// ListLinks 返回该实体作为起点或终点的全部关联。
	//
	// 不带 spaces 过滤：关联的**对端**可能落在调用方无权访问的空间里，
	// 而这里返回的只是一个 ID 和 kind，不含任何内容。
	// 是否把对端的标题展示出来，由应用层用可读空间集合再判一次。
	ListLinks(ctx context.Context, tenantID platform.ID, ref Ref) ([]Link, error)

	// Atomically 在一个原子单元内执行 fn，fn 收到的 Store 与外层同属该单元。
	//
	// 核心域里有三处跨表写：建记忆（行 + 标签）、建卡片（行 + 标签 + 触发条件）、
	// 提升（读源 + 写目标的全套副本）。任一半途失败留下的都是脏数据——
	// 一张没有召回条件的卡片，正是这个产品最不该产出的东西。
	Atomically(ctx context.Context, fn func(Store) error) error
}
