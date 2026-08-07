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
