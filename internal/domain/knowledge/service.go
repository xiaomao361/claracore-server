package knowledge

import (
	"context"
	"fmt"
	"time"

	"claracore-server/internal/platform"
	"claracore-server/internal/ports"
)

// Service 编排核心域的用例。
//
// 与 governance.Service 同构：不持有配置或连接，时间来自 Clock，
// ID 来自 IDGenerator，存储来自 Store 接口。
type Service struct {
	store Store
	clock platform.Clock
	ids   *platform.IDGenerator

	// embedder 可以为 nil，那是**降级形态而非异常形态**
	// （docs/01-architecture.md §6.1）：不入索引队列，检索退化成
	// 中文全文匹配，系统功能保持完整。
	//
	// 用 nil 而不是塞一个「什么都不做的 Embedder」：空实现的 Model()
	// 得编一个模型名出来，而那个名字会被写进 embeddings.model 列，
	// 将来真的接上模型时没人分得清哪些行是它留下的。
	embedder         ports.Embedder
	autoRecallPolicy AutomaticRecallPolicy
}

func NewService(store Store, clock platform.Clock, ids *platform.IDGenerator, embedder ports.Embedder) *Service {
	return &Service{store: store, clock: clock, ids: ids, embedder: embedder, autoRecallPolicy: DefaultAutomaticRecallPolicy()}
}

// WithAutomaticRecallPolicy returns a shallow copy with deployment-calibrated
// deterministic gates. The terminal Agent remains the final relevance judge.
func (s *Service) WithAutomaticRecallPolicy(policy AutomaticRecallPolicy) *Service {
	copy := *s
	copy.autoRecallPolicy = policy
	return &copy
}

// ---------------------------------------------------------------------------
// Memory
// ---------------------------------------------------------------------------

type CreateMemoryInput struct {
	TenantID platform.ID
	// SpaceID 已由应用层经 governance.SpaceScope.ResolveWriteTarget 解析，
	// 到这里必然是一个确定值。本包不做、也无从做权限判断。
	SpaceID       platform.ID
	AuthorUserID  platform.ID
	AuthorAgentID *platform.ID
	Title         string
	Body          string
	Labels        []string
	// SupersedesID 非空时，新记忆与旧记忆在同一事务中完成替代：
	// current(new) -> historical(old)，旧记录保留但退出默认检索。
	SupersedesID   platform.ID
	WritableSpaces []platform.ID
}

func (s *Service) CreateMemory(ctx context.Context, in CreateMemoryInput) (Memory, error) {
	now := s.clock.Now()
	m := Memory{
		ID:            s.ids.New(),
		TenantID:      in.TenantID,
		SpaceID:       in.SpaceID,
		AuthorUserID:  in.AuthorUserID,
		AuthorAgentID: in.AuthorAgentID,
		Title:         in.Title,
		Body:          in.Body,
		Status:        MemoryActive,
		Version:       1,
		CreatedAt:     now,
		UpdatedAt:     now,
		Labels:        in.Labels,
	}
	if err := m.validate(); err != nil {
		return Memory{}, err
	}

	err := s.store.Atomically(ctx, func(st Store) error {
		var historical *Memory
		if in.SupersedesID != "" {
			if in.SupersedesID == m.ID || len(in.WritableSpaces) == 0 {
				return fmt.Errorf("%w: supersede 缺少可写范围或目标非法", ErrInvalidArgument)
			}
			var findErr error
			historical, findErr = st.FindMemory(ctx, in.TenantID, in.SupersedesID, in.WritableSpaces)
			if findErr != nil {
				return findErr
			}
			if historical.Status != MemoryActive {
				return fmt.Errorf("%w: 只能替代 active 记忆", ErrInvalidArgument)
			}
		}
		if err := st.CreateMemory(ctx, m); err != nil {
			return err
		}
		if err := st.ReplaceLabels(ctx, m.TenantID, Ref{EntityMemory, m.ID}, m.Labels, now); err != nil {
			return err
		}
		// 入队与内容同事务提交：分两步的话，中间崩一次这条内容就永远
		// 搜不到，而且没有任何地方会报错（见 enqueueEmbedding 的注释）。
		if err := s.enqueueEmbedding(ctx, st, m.TenantID, Ref{EntityMemory, m.ID}, memoryEmbedText(m), now); err != nil {
			return err
		}
		if historical == nil {
			return nil
		}
		old := *historical
		old.Status, old.Version, old.UpdatedAt = MemorySuperseded, old.Version+1, now
		if err := st.UpdateMemory(ctx, old, historical.Version); err != nil {
			return err
		}
		link := Link{ID: s.ids.New(), TenantID: m.TenantID, From: Ref{EntityMemory, m.ID}, To: Ref{EntityMemory, old.ID}, Kind: LinkSupersedes, CreatedAt: now}
		if err := link.validate(); err != nil {
			return err
		}
		return st.CreateLink(ctx, link)
	})
	if err != nil {
		return Memory{}, err
	}
	return m, nil
}

// GetMemory 按 ID 取一条，含标签与关联。
//
// spaces 是调用者的**可读**空间集合。
func (s *Service) GetMemory(ctx context.Context, tenantID, id platform.ID, spaces []platform.ID) (Memory, []Link, error) {
	if err := requireSpaces(spaces); err != nil {
		return Memory{}, nil, err
	}
	m, err := s.store.FindMemory(ctx, tenantID, id, spaces)
	if err != nil {
		return Memory{}, nil, err
	}
	if err := checkTenant(tenantID, m.TenantID); err != nil {
		return Memory{}, nil, err
	}
	links, err := s.store.ListLinks(ctx, tenantID, Ref{EntityMemory, m.ID})
	if err != nil {
		return Memory{}, nil, err
	}
	return *m, links, nil
}

type UpdateMemoryInput struct {
	TenantID platform.ID
	ID       platform.ID

	// WritableSpaces 是调用者有**写**权限的空间集合，用于定位这条记忆。
	//
	// 刻意不用可读集合再单独判一次写权限：对一条自己改不了的内容，
	// 「不存在」与「无权修改」给调用方的下一步是同一件事，
	// 而分开返回等于确认「它存在，只是你够不着」。
	WritableSpaces []platform.ID

	// ExpectedVersion 为 nil 时退化为「读-改-写」窗口内的乐观锁：
	// 用刚读到的版本号作条件。它仍然不会静默覆盖——
	// 只是把冲突检测的窗口从「调用方上次读取」缩短到「本次读取」。
	//
	// 需要真正的端到端并发保护时，调用方应把上次 get 拿到的 version 传进来。
	ExpectedVersion *int

	// nil 表示该字段不变。空字符串是合法的"清空"意图，与 nil 不是一回事——
	// 用 *string 而非 string 正是为了让这两者可区分。
	Title  *string
	Body   *string
	Status *string
	Labels *[]string
}

func (s *Service) UpdateMemory(ctx context.Context, in UpdateMemoryInput) (Memory, error) {
	if err := requireSpaces(in.WritableSpaces); err != nil {
		return Memory{}, err
	}
	cur, err := s.store.FindMemory(ctx, in.TenantID, in.ID, in.WritableSpaces)
	if err != nil {
		return Memory{}, err
	}
	if err := checkTenant(in.TenantID, cur.TenantID); err != nil {
		return Memory{}, err
	}

	expected, err := resolveExpectedVersion(in.ExpectedVersion, cur.Version)
	if err != nil {
		return Memory{}, err
	}

	next := *cur
	applyString(&next.Title, in.Title)
	applyString(&next.Body, in.Body)
	applyString(&next.Status, in.Status)
	labelsChanged := in.Labels != nil
	if labelsChanged {
		next.Labels = *in.Labels
	}
	next.Version = expected + 1
	next.UpdatedAt = s.clock.Now()

	if err := next.validate(); err != nil {
		return Memory{}, err
	}

	err = s.store.Atomically(ctx, func(st Store) error {
		if err := st.UpdateMemory(ctx, next, expected); err != nil {
			return err
		}
		if labelsChanged {
			if err := st.ReplaceLabels(ctx, next.TenantID, Ref{EntityMemory, next.ID}, next.Labels, next.UpdatedAt); err != nil {
				return err
			}
		}
		// 无条件入队，不在这里判断「正文有没有变」：判断的依据是
		// content_hash，而它由 EnqueueEmbedding 的 upsert 比对——
		// 在领域层再算一次「变了没有」，就有了两处会给出不同答案的判断。
		return s.enqueueEmbedding(ctx, st, next.TenantID, Ref{EntityMemory, next.ID}, memoryEmbedText(next), next.UpdatedAt)
	})
	if err != nil {
		return Memory{}, err
	}
	return next, nil
}

// DeleteMemory 是软删除：置 status = deleted，行保留。
//
// 硬删会让指向它的 link 变成悬空引用——多态引用没有外键能拦住这件事。
// 软删之后仍可按 ID 取到，只是不进默认检索。
func (s *Service) DeleteMemory(ctx context.Context, tenantID, id platform.ID, writableSpaces []platform.ID) (Memory, error) {
	deleted := MemoryDeleted
	return s.UpdateMemory(ctx, UpdateMemoryInput{
		TenantID:       tenantID,
		ID:             id,
		WritableSpaces: writableSpaces,
		Status:         &deleted,
	})
}

// ---------------------------------------------------------------------------
// Card
// ---------------------------------------------------------------------------

type CreateCardInput struct {
	TenantID      platform.ID
	SpaceID       platform.ID
	AuthorUserID  platform.ID
	AuthorAgentID *platform.ID

	Title    string
	CardType string
	Source   string

	Original      string
	Distilled     string
	Understanding string
	WhyKeep       string
	Unresolved    string

	// Status 留空即 CardActive（手写默认，作者自管）。
	// **摄取路径必须显式传 CardDraft** ——摄取产物不进默认检索，需人过一遍
	// （2026-08-06 拍板）。默认值给 active 而不是 draft，是因为手写是主路径：
	// 让最常见的调用不必关心这个字段。
	Status string

	RecallTriggers []string
	Labels         []string
	EvolvesFromID  platform.ID
	WritableSpaces []platform.ID
}

func (s *Service) CreateCard(ctx context.Context, in CreateCardInput) (Card, error) {
	triggers, err := normalizeTriggers(in.RecallTriggers)
	if err != nil {
		return Card{}, err
	}

	now := s.clock.Now()
	c := Card{
		ID:            s.ids.New(),
		TenantID:      in.TenantID,
		SpaceID:       in.SpaceID,
		AuthorUserID:  in.AuthorUserID,
		AuthorAgentID: in.AuthorAgentID,
		Title:         in.Title,
		CardType:      in.CardType,
		Status:        defaultIfEmpty(in.Status, CardActive),
		Source:        in.Source,
		Original:      in.Original,
		Distilled:     in.Distilled,
		Understanding: in.Understanding,
		WhyKeep:       in.WhyKeep,
		Unresolved:    in.Unresolved,
		Version:       1,
		CreatedAt:     now,
		UpdatedAt:     now,
		Labels:        in.Labels,
	}
	if err := c.validate(); err != nil {
		return Card{}, err
	}
	c.RecallTriggers = s.buildTriggers(c, triggers, now)

	if in.EvolvesFromID == "" {
		if err := s.persistNewCard(ctx, c, now); err != nil {
			return Card{}, err
		}
		return c, nil
	}
	if len(in.WritableSpaces) == 0 {
		return Card{}, fmt.Errorf("%w: evolvesFrom 缺少可写范围", ErrInvalidArgument)
	}
	err = s.store.Atomically(ctx, func(st Store) error {
		old, err := st.FindCard(ctx, in.TenantID, in.EvolvesFromID, in.WritableSpaces)
		if err != nil {
			return err
		}
		if old.Status != CardActive {
			return fmt.Errorf("%w: 只能演进 active 卡片", ErrInvalidArgument)
		}
		if err := s.persistNewCardWithStore(ctx, st, c, now); err != nil {
			return err
		}
		previous := *old
		previous.Status, previous.Version, previous.UpdatedAt = CardDeprecated, previous.Version+1, now
		if err := st.UpdateCard(ctx, previous, old.Version); err != nil {
			return err
		}
		link := Link{ID: s.ids.New(), TenantID: c.TenantID, From: Ref{EntityCard, c.ID}, To: Ref{EntityCard, old.ID}, Kind: LinkEvolvedFrom, CreatedAt: now}
		if err := link.validate(); err != nil {
			return err
		}
		return st.CreateLink(ctx, link)
	})
	if err != nil {
		return Card{}, err
	}
	return c, nil
}

// persistNewCard 把一张新卡的三部分写进同一个原子单元。
//
// 一张没有召回条件的卡片是这个产品最不该产出的东西——
// 它退化成一条普通记忆，却占着"知识卡片"的位置。
func (s *Service) persistNewCard(ctx context.Context, c Card, at time.Time) error {
	return s.store.Atomically(ctx, func(st Store) error {
		return s.persistNewCardWithStore(ctx, st, c, at)
	})
}

func (s *Service) persistNewCardWithStore(ctx context.Context, st Store, c Card, at time.Time) error {
	if err := st.CreateCard(ctx, c); err != nil {
		return err
	}
	if err := st.ReplaceLabels(ctx, c.TenantID, Ref{EntityCard, c.ID}, c.Labels, at); err != nil {
		return err
	}
	if err := st.ReplaceRecallTriggers(ctx, c.TenantID, c.ID, c.RecallTriggers); err != nil {
		return err
	}
	// 卡片正文与每条召回条件各自入队。两者匹配的是不同的东西：
	// 正文回答「这张卡在讲什么」（card_search），召回条件回答
	// 「什么情境下该想起它」（card_recall）——后者才是这个产品的核心优势。
	if err := s.enqueueEmbedding(ctx, st, c.TenantID, Ref{EntityCard, c.ID}, cardEmbedText(c), at); err != nil {
		return err
	}
	return s.enqueueCardTriggers(ctx, st, c, at)
}

// buildTriggers 把清洗后的文本变成带 ID 的触发条件行。
// sort_order 按传入顺序重编：作者写下的先后本身是信息。
func (s *Service) buildTriggers(c Card, texts []string, at time.Time) []RecallTrigger {
	out := make([]RecallTrigger, 0, len(texts))
	for i, text := range texts {
		out = append(out, RecallTrigger{
			ID:        s.ids.New(),
			TenantID:  c.TenantID,
			CardID:    c.ID,
			SpaceID:   c.SpaceID,
			Text:      text,
			SortOrder: i,
			CreatedAt: at,
		})
	}
	return out
}

func (s *Service) GetCard(ctx context.Context, tenantID, id platform.ID, spaces []platform.ID) (Card, []Link, error) {
	if err := requireSpaces(spaces); err != nil {
		return Card{}, nil, err
	}
	c, err := s.store.FindCard(ctx, tenantID, id, spaces)
	if err != nil {
		return Card{}, nil, err
	}
	if err := checkTenant(tenantID, c.TenantID); err != nil {
		return Card{}, nil, err
	}
	links, err := s.store.ListLinks(ctx, tenantID, Ref{EntityCard, c.ID})
	if err != nil {
		return Card{}, nil, err
	}
	return *c, links, nil
}

type UpdateCardInput struct {
	TenantID        platform.ID
	ID              platform.ID
	WritableSpaces  []platform.ID
	ExpectedVersion *int

	Title         *string
	CardType      *string
	Status        *string
	Source        *string
	Original      *string
	Distilled     *string
	Understanding *string
	WhyKeep       *string
	Unresolved    *string

	Labels *[]string
	// RecallTriggers 非 nil 时整体替换。传空切片即清空全部召回条件——
	// 那会让这张卡再也不会被 card_recall 命中，是个有意义但代价很大的动作。
	RecallTriggers *[]string
}

func (s *Service) UpdateCard(ctx context.Context, in UpdateCardInput) (Card, error) {
	if err := requireSpaces(in.WritableSpaces); err != nil {
		return Card{}, err
	}
	cur, err := s.store.FindCard(ctx, in.TenantID, in.ID, in.WritableSpaces)
	if err != nil {
		return Card{}, err
	}
	if err := checkTenant(in.TenantID, cur.TenantID); err != nil {
		return Card{}, err
	}

	expected, err := resolveExpectedVersion(in.ExpectedVersion, cur.Version)
	if err != nil {
		return Card{}, err
	}

	next := *cur
	applyString(&next.Title, in.Title)
	applyString(&next.CardType, in.CardType)
	applyString(&next.Status, in.Status)
	applyString(&next.Source, in.Source)
	applyString(&next.Original, in.Original)
	applyString(&next.Distilled, in.Distilled)
	applyString(&next.Understanding, in.Understanding)
	applyString(&next.WhyKeep, in.WhyKeep)
	applyString(&next.Unresolved, in.Unresolved)

	labelsChanged := in.Labels != nil
	if labelsChanged {
		next.Labels = *in.Labels
	}
	next.Version = expected + 1
	next.UpdatedAt = s.clock.Now()

	if err := next.validate(); err != nil {
		return Card{}, err
	}

	triggersChanged := in.RecallTriggers != nil
	if triggersChanged {
		texts, err := normalizeTriggers(*in.RecallTriggers)
		if err != nil {
			return Card{}, err
		}
		next.RecallTriggers = s.buildTriggers(next, texts, next.UpdatedAt)
	}

	err = s.store.Atomically(ctx, func(st Store) error {
		if err := st.UpdateCard(ctx, next, expected); err != nil {
			return err
		}
		if labelsChanged {
			if err := st.ReplaceLabels(ctx, next.TenantID, Ref{EntityCard, next.ID}, next.Labels, next.UpdatedAt); err != nil {
				return err
			}
		}
		if err := s.enqueueEmbedding(ctx, st, next.TenantID, Ref{EntityCard, next.ID}, cardEmbedText(next), next.UpdatedAt); err != nil {
			return err
		}
		if !triggersChanged {
			return nil
		}
		// 顺序不能换：ReplaceRecallTriggers 是先删后插，旧触发条件的行
		// 一旦没了，它们留下的向量就再也定位不到，会永远躺在库里。
		if err := st.DeleteCardTriggerEmbeddings(ctx, next.TenantID, next.ID); err != nil {
			return err
		}
		if err := st.ReplaceRecallTriggers(ctx, next.TenantID, next.ID, next.RecallTriggers); err != nil {
			return err
		}
		return s.enqueueCardTriggers(ctx, st, next, next.UpdatedAt)
	})
	if err != nil {
		return Card{}, err
	}
	return next, nil
}

// ---------------------------------------------------------------------------
// Link
// ---------------------------------------------------------------------------

type LinkInput struct {
	TenantID platform.ID
	From     Ref
	To       Ref
	Kind     string
	Note     string

	// ReadableSpaces 用于确认两端都在调用者视野内。
	//
	// 建关联要求两端**可读**而非可写：关联是一句关于世界的陈述
	// （"这条导致了那条"），不修改任何一端的内容。
	// 但两端都必须看得见——否则可以用一个猜来的 ID 把自己的记忆
	// 挂到一条无权访问的内容上，再通过 ListLinks 确认它的存在。
	ReadableSpaces []platform.ID
}

func (s *Service) CreateLink(ctx context.Context, in LinkInput) (Link, error) {
	if err := requireSpaces(in.ReadableSpaces); err != nil {
		return Link{}, err
	}
	if !validUserLinkKind(in.Kind) {
		return Link{}, fmt.Errorf("%w: 关联类型取值非法 %q", ErrInvalidArgument, in.Kind)
	}
	l := Link{
		ID:        s.ids.New(),
		TenantID:  in.TenantID,
		From:      in.From,
		To:        in.To,
		Kind:      in.Kind,
		Note:      in.Note,
		CreatedAt: s.clock.Now(),
	}
	if err := l.validate(); err != nil {
		return Link{}, err
	}
	if err := s.requireVisible(ctx, in.TenantID, l.From, in.ReadableSpaces); err != nil {
		return Link{}, err
	}
	if err := s.requireVisible(ctx, in.TenantID, l.To, in.ReadableSpaces); err != nil {
		return Link{}, err
	}
	if err := s.store.CreateLink(ctx, l); err != nil {
		return Link{}, err
	}
	return l, nil
}

// DeleteLink 幂等：关联本就不存在时不报错。
//
// 与创建一样要求两端可读——否则可以拿一个猜来的 ID 试删，
// 用"报错还是成功"反推那条内容是否存在。
func (s *Service) DeleteLink(ctx context.Context, in LinkInput) error {
	if err := requireSpaces(in.ReadableSpaces); err != nil {
		return err
	}
	if err := in.From.Validate(); err != nil {
		return err
	}
	if err := in.To.Validate(); err != nil {
		return err
	}
	if !validUserLinkKind(in.Kind) {
		return fmt.Errorf("%w: 关联类型取值非法 %q", ErrInvalidArgument, in.Kind)
	}
	if err := s.requireVisible(ctx, in.TenantID, in.From, in.ReadableSpaces); err != nil {
		return err
	}
	if err := s.requireVisible(ctx, in.TenantID, in.To, in.ReadableSpaces); err != nil {
		return err
	}
	return s.store.DeleteLink(ctx, in.TenantID, in.From, in.To, in.Kind)
}

// requireVisible 确认某个引用指向的实体在给定空间集合内确实存在。
//
// 这是应用层保证多态引用完整性的地方——数据库没有外键能替我们做这件事
// （docs/02-data-model.md §3.1 的明确权衡）。
func (s *Service) requireVisible(ctx context.Context, tenantID platform.ID, ref Ref, spaces []platform.ID) error {
	switch ref.Type {
	case EntityMemory:
		m, err := s.store.FindMemory(ctx, tenantID, ref.ID, spaces)
		if err != nil {
			return err
		}
		return checkTenant(tenantID, m.TenantID)
	case EntityCard:
		c, err := s.store.FindCard(ctx, tenantID, ref.ID, spaces)
		if err != nil {
			return err
		}
		return checkTenant(tenantID, c.TenantID)
	default:
		return fmt.Errorf("%w: 未知的实体类型 %q", ErrInvalidArgument, ref.Type)
	}
}

// ---------------------------------------------------------------------------
// Promote
// ---------------------------------------------------------------------------

type PromoteInput struct {
	TenantID platform.ID
	Ref      Ref

	// ReadableSpaces 用于定位源。**两端都校**是 2026-08-06 的拍板：
	// 只校目标空间可写，就意味着拿到一个 ID 就能把自己无权读的内容
	// 搬进一个自己能读的空间——一次完整的越权读取，还留下了合法的审计痕迹。
	ReadableSpaces []platform.ID

	// TargetSpaceID 已由应用层经 governance.SpaceScope.ResolveWriteTarget 校过可写。
	TargetSpaceID platform.ID

	// ActorUserID 是执行提升的人，也是副本的作者。
	//
	// 不沿用原作者：「这条团队约定来自谁的哪次踩坑」由 CopiedFrom 回答，
	// 而「谁决定把它摆到团队面前」是一份新的责任，该记在做决定的人头上。
	ActorUserID  platform.ID
	ActorAgentID *platform.ID
}

// Promote 把一条内容复制到另一个空间（docs/02-data-model.md §4）。
//
// 复制而非移动，原件保留；复制发生时的内容即为副本的初始内容，
// 包括标签与召回条件。两份此后各自演进，**不做双向同步**——
// 自动同步会让团队空间里一条已达成共识的约定，被原作者的一次随手修改悄悄改写。
func (s *Service) Promote(ctx context.Context, in PromoteInput) (Ref, error) {
	if err := requireSpaces(in.ReadableSpaces); err != nil {
		return Ref{}, err
	}
	if err := in.Ref.Validate(); err != nil {
		return Ref{}, err
	}
	if in.TargetSpaceID == "" {
		return Ref{}, fmt.Errorf("%w: 缺少目标空间", ErrInvalidArgument)
	}
	if in.ActorUserID == "" {
		return Ref{}, fmt.Errorf("%w: 缺少提升执行者", ErrInvalidArgument)
	}

	switch in.Ref.Type {
	case EntityMemory:
		return s.promoteMemory(ctx, in)
	case EntityCard:
		return s.promoteCard(ctx, in)
	default:
		return Ref{}, fmt.Errorf("%w: 未知的实体类型 %q", ErrInvalidArgument, in.Ref.Type)
	}
}

func (s *Service) promoteMemory(ctx context.Context, in PromoteInput) (Ref, error) {
	src, err := s.store.FindMemory(ctx, in.TenantID, in.Ref.ID, in.ReadableSpaces)
	if err != nil {
		return Ref{}, err
	}
	if err := checkTenant(in.TenantID, src.TenantID); err != nil {
		return Ref{}, err
	}
	if err := checkPromoteTarget(src.SpaceID, in.TargetSpaceID); err != nil {
		return Ref{}, err
	}

	now := s.clock.Now()
	copied := Memory{
		ID:            s.ids.New(),
		TenantID:      src.TenantID,
		SpaceID:       in.TargetSpaceID,
		AuthorUserID:  in.ActorUserID,
		AuthorAgentID: in.ActorAgentID,
		Title:         src.Title,
		Body:          src.Body,
		// 副本一律 active，不继承源的生命周期状态：提升是一次主动的
		// 「我认为这值得团队看到」，把一条 archived 的记忆搬过去再让它
		// 继续隐身，等于这次动作什么也没发生。
		Status:     MemoryActive,
		CopiedFrom: &Origin{Type: EntityMemory, ID: src.ID},
		Version:    1,
		CreatedAt:  now,
		UpdatedAt:  now,
		Labels:     src.Labels,
	}
	if err := copied.validate(); err != nil {
		return Ref{}, err
	}

	err = s.store.Atomically(ctx, func(st Store) error {
		if err := st.CreateMemory(ctx, copied); err != nil {
			return err
		}
		if err := st.ReplaceLabels(ctx, copied.TenantID, Ref{EntityMemory, copied.ID}, copied.Labels, now); err != nil {
			return err
		}
		// 副本要自己的向量行：它是另一个空间里的另一条内容，
		// 检索时按各自空间的权限被取到，不能共用原件那一行。
		return s.enqueueEmbedding(ctx, st, copied.TenantID, Ref{EntityMemory, copied.ID}, memoryEmbedText(copied), now)
	})
	if err != nil {
		return Ref{}, err
	}
	return Ref{EntityMemory, copied.ID}, nil
}

func (s *Service) promoteCard(ctx context.Context, in PromoteInput) (Ref, error) {
	src, err := s.store.FindCard(ctx, in.TenantID, in.Ref.ID, in.ReadableSpaces)
	if err != nil {
		return Ref{}, err
	}
	if err := checkTenant(in.TenantID, src.TenantID); err != nil {
		return Ref{}, err
	}
	if err := checkPromoteTarget(src.SpaceID, in.TargetSpaceID); err != nil {
		return Ref{}, err
	}

	now := s.clock.Now()
	copied := *src
	copied.ID = s.ids.New()
	copied.SpaceID = in.TargetSpaceID
	copied.AuthorUserID = in.ActorUserID
	copied.AuthorAgentID = in.ActorAgentID
	// 同 memory：主动提升一张 draft 卡，说明提升者已经过目并认可它。
	copied.Status = CardActive
	copied.CopiedFrom = &Origin{Type: EntityCard, ID: src.ID}
	copied.Version = 1
	copied.CreatedAt = now
	copied.UpdatedAt = now

	if err := copied.validate(); err != nil {
		return Ref{}, err
	}

	// 召回条件随卡快照。它们是这张卡的一部分，不是可选的附属物——
	// 一张丢了召回条件的卡片进了团队空间，就再也不会在该出现的时候出现。
	texts := make([]string, 0, len(src.RecallTriggers))
	for _, t := range src.RecallTriggers {
		texts = append(texts, t.Text)
	}
	copied.RecallTriggers = s.buildTriggers(copied, texts, now)

	if err := s.persistNewCard(ctx, copied, now); err != nil {
		return Ref{}, err
	}
	return Ref{EntityCard, copied.ID}, nil
}

// checkPromoteTarget 拦住「提升到自己所在的空间」。
//
// 那会得到同一空间里两条内容一模一样、只差一个 copied_from 的记录。
// 检索时它们会并排出现，而调用方无从判断该信哪一条。
func checkPromoteTarget(srcSpace, target platform.ID) error {
	if srcSpace == target {
		return fmt.Errorf("%w: 内容已在目标空间中", ErrInvalidArgument)
	}
	return nil
}

// ---------------------------------------------------------------------------
// 内部工具
// ---------------------------------------------------------------------------

// resolveExpectedVersion 决定乐观锁的比较基准。
//
// 调用方传了就用它——那是真正的端到端保护，能发现「我上次 get 之后有人改过」。
// 没传就用刚读到的版本号，把窗口缩到本次读-改-写之间。
// 无论哪种，都不存在"跳过版本检查"的路径：静默覆盖别人的修改
// 在多人共享的知识库里是不可接受的（docs/02-data-model.md §6）。
func resolveExpectedVersion(requested *int, current int) (int, error) {
	if requested == nil {
		return current, nil
	}
	if *requested != current {
		// 提前返回，省掉一次注定失败的写。SQL 上的版本条件仍然保留——
		// 它才是权威，这里只是让常见冲突少走一次往返。
		return 0, &VersionConflictError{Expected: *requested, Current: current}
	}
	return *requested, nil
}

// checkTenant 防御数据损坏导致的跨租户越权。
//
// 与 governance.checkAuthConsistency 同一性质：正常情况下永远不触发。
// 存储层的每条查询都带了 tenant_id 条件，所以这里为真意味着
// 那个条件被某次改动去掉了——多租户系统里最不能接受的一类回归。
func checkTenant(want, got platform.ID) error {
	if want != got {
		return fmt.Errorf("%w: 期望 %s，实际 %s", ErrTenantMismatch, want, got)
	}
	return nil
}

// applyString 把非 nil 的新值写入目标。nil 表示该字段不变。
func applyString(dst *string, src *string) {
	if src != nil {
		*dst = *src
	}
}

func defaultIfEmpty(v, fallback string) string {
	if v == "" {
		return fallback
	}
	return v
}
